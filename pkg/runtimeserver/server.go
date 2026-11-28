// Package runtimeserver 是 Runtime 外层的网络协议宿主：把 WebSocket 连接上的 JSON-RPC 请求转换为 Runtime 公开调用，
// 并把订阅事件推送为通知。
package runtimeserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RuntimeServer 把 Runtime 公开方法暴露为 HTTP 和 WebSocket 路由，管理已升级连接和空闲退出。
type RuntimeServer interface {
	// RegisterRoutes 把健康检查、协议、关闭和页面路由注册到调用方的路由器。
	RegisterRoutes(router gin.IRouter)
	// Run 在 ctx 结束、收到关闭请求或空闲超时时取消并等待所有连接，然后返回 ctx.Err()。只能调用一次。
	Run(ctx context.Context) error
}

// runtimeServerImpl 保存 WebSocket 连接任务和空闲退出状态。
type runtimeServerImpl struct {
	opt RuntimeServerOpt

	// started 是否已经调用过 Run？
	started atomic.Bool
	// connectionsReady 在 Run 发布连接上下文后关闭，等待中的握手可以继续。
	connectionsReady chan struct{}
	// connectionsMu 让开始握手时的登记和停止时的封口互斥。
	connectionsMu sync.Mutex
	runCtx        context.Context
	connections   sync.WaitGroup
	// openConnectionCount 是当前正在服务的 WebSocket 连接数，握手中的也算；由 serveConnection 更新，空闲检查读取。
	openConnectionCount atomic.Int64
	// lastConnectionClosedAt 是最后一个连接结束的时间，还没有连接结束过时是 Run 开始的时间；由 serveConnection 更新，空闲检查读取。
	lastConnectionClosedAt atomic.Pointer[time.Time]
	// shutdownRequested 由 handleShutdown 通知 Run 结束。
	shutdownRequested chan struct{}
}

// Configuration 读写 serve 本机的模型配置。写方法写完重新加载整份配置，加载失败时恢复写之前的文件并返回加载错误；
// 成功时返回新的整份定义，由 handler 交给 Runtime.SetDefinitions。由 config.ConfigFiles 实现。
type Configuration interface {
	// ListProviders 按 id 顺序返回配置里的全部 Provider，带 API key 原文；同时返回模型配置的默认模型：models.default_model，
	// 没写时是按 Provider id、模型 id 排序的第一个模型，模型目录为空时是零值；defaults 是同次加载的模型默认配置。
	ListProviders(ctx context.Context) (providers []defs.ProviderConfig, defaultModel defs.ModelRef, defaults defs.ModelDefaults, err error)
	// SetProvider 把整份 Provider 写成 models/<id>.yaml（已有时整份替换）；defaultModel 不是零值时同时把 models.default_model 改成它，
	// 两个文件写完只重新加载一次，失败时都恢复。id 不合法时返回 invalid provider id，不碰文件。
	SetProvider(ctx context.Context, provider defs.ProviderConfig, defaultModel defs.ModelRef) (*runtime.Definitions, error)
	// DeleteProvider 删除 models/<id>.yaml，再重新加载配置；没有这个 Provider 时返回 provider <id> is not configured。
	DeleteProvider(ctx context.Context, providerID string) (*runtime.Definitions, error)
	// SetDefaultModel 只改配置文件里的 models.default_model，其余内容和注释保留，再重新加载配置；配置文件不存在时新建。
	SetDefaultModel(ctx context.Context, ref defs.ModelRef) (*runtime.Definitions, error)
	// Reload 不改任何文件，用启动时的配置路径和 flag 重新加载整份配置。
	Reload(ctx context.Context) (*runtime.Definitions, error)
	// ListExecutorConfigs 按 ID 顺序返回 executors/*.yaml 里的原样配置。
	ListExecutorConfigs(ctx context.Context) (executors []defs.ExecutorConfig, err error)
	// SetExecutor 原子写入 executors/<id>.yaml 并重新加载配置；失败时恢复原文件。
	SetExecutor(ctx context.Context, executor defs.ExecutorConfig) (definitions *runtime.Definitions, err error)
	// DeleteExecutor 删除 executors/<id>.yaml 并重新加载配置；文件不存在时返回错误。
	DeleteExecutor(ctx context.Context, executorID string) (definitions *runtime.Definitions, err error)
	// TestSSHExecutor 用临时 SSH 参数拨号并解析工作目录，返回规范后的绝对目录，不写入任何文件。
	TestSSHExecutor(ctx context.Context, sshCfg defs.SSHExecutorConfig, workspaceDirectory string) (resolvedDirectory string, err error)
	// ListSSHConfigHosts 读取 serve 本机 ~/.ssh/config 里的非通配符 Host 项；文件缺失或不可读时返回空列表。
	ListSSHConfigHosts(ctx context.Context) (hosts []defs.SSHConfigHost, err error)
}

// ModelCatalog 给 /provider 提供可选的模型服务和模型，都是弱依赖：失败时返回错误，由 TUI 改成手动填写。
// 由 modelcatalog.Catalog 实现。
type ModelCatalog interface {
	// ListCatalogProviders 返回模型目录里能对应上协议和 base_url 的服务；目录数据拿不到时返回错误。
	ListCatalogProviders(ctx context.Context) ([]defs.CatalogProvider, error)
	// ListCatalogModels 返回模型目录里一个服务的全部模型，key 是写进配置文件的模型 id；服务不在目录里时返回错误。
	ListCatalogModels(ctx context.Context, catalogProviderID string) (map[string]*defs.ProviderModelConfig, error)
	// FetchProviderModels 请求 Provider 上游的 /models 取模型 id，再按 id 从模型目录补字段；上游请求失败时返回错误。
	FetchProviderModels(ctx context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error)
}

// RuntimeServerOpt 保存 Runtime、模型配置、模型目录与 token，构造后只读。
type RuntimeServerOpt struct {
	Runtime runtime.Runtime
	// Configuration 读写 serve 本机的模型配置，必须非 nil。
	Configuration Configuration
	// ModelCatalog 提供引导用的模型服务和模型，必须非 nil。
	ModelCatalog ModelCatalog
	// Token 是全部协议路由共用的访问令牌。
	Token string
	// ServerID 标识这台机器上的 serve，重启后保持不变，initialize 原样返回；客户端据此判断连上的是不是本机的 serve。
	ServerID string
	// Telemetry 是遥测管道，由 Run 挂到 ctx 供全部连接使用；nil 时使用全局兜底。
	Telemetry *telemetry.Telemetry
	// WebUI 是页面静态文件，根目录下有 index.html；nil 时不提供页面。
	WebUI fs.FS
	// IdleTimeout 大于 0 时，没有连接且 Runtime 空闲持续这么久后 Run 返回 nil；为 0 时常驻。
	IdleTimeout time.Duration
}

// NewRuntimeServerOpt 保存 Runtime 和访问令牌。
func NewRuntimeServerOpt(rt runtime.Runtime, token string) *RuntimeServerOpt {
	// 1. 填入 Runtime 和 token。
	return &RuntimeServerOpt{
		Runtime: rt,
		Token:   token,
	}
}

// NewRuntimeServer 校验服务配置并创建运行所需的通知通道。
func NewRuntimeServer(opt *RuntimeServerOpt) (RuntimeServer, error) {
	// 1. 检查 Runtime、配置服务、模型目录和 token。
	if opt == nil || opt.Runtime == nil || opt.Configuration == nil || opt.ModelCatalog == nil || opt.Token == "" {
		return nil, errors.New("runtime server requires a runtime, a configuration, a model catalog, and a token")
	}
	// 2. 创建连接就绪和关闭请求通道，最多暂存一个关闭请求。
	return &runtimeServerImpl{
		opt:               *opt,
		connectionsReady:  make(chan struct{}),
		shutdownRequested: make(chan struct{}, 1),
	}, nil
}

func (s *runtimeServerImpl) RegisterRoutes(router gin.IRouter) {
	// 1. 注册健康检查，并给协议与关闭路由添加 token 校验。
	api := router.Group("/api/v1")
	api.GET("/healthz", handleHealthz)
	protocols := api.Group("", NewTokenMiddleware(s.opt.Token, s.handleTokenRejection))
	protocols.GET("/openvaala", s.handleOpenVaalaUpgrade)
	protocols.POST("/shutdown", s.handleShutdown)
	RegisterWebRoutes(router, s.opt.WebUI, "/web/v1/")
}

// RegisterWebRoutes 将内嵌前端及其入口挂载到调用方的路由器。
func RegisterWebRoutes(router gin.IRouter, assets fs.FS, landing string) {
	router.GET("/web/v1/*filepath", func(c *gin.Context) { handleWebUI(c, assets) })
	router.GET("/", func(c *gin.Context) { c.Redirect(http.StatusFound, landing) })
}

func (s *runtimeServerImpl) Run(ctx context.Context) error {
	// 1. 拒绝重复运行，发布连接上下文和空闲计时起点。
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("RuntimeServer Run can only be called once")
	}
	ctx = telemetry.ContextWithTelemetry(ctx, s.opt.Telemetry)
	runCtx, stopConnections := context.WithCancel(ctx)
	s.runCtx = runCtx
	startedAt := time.Now()
	s.lastConnectionClosedAt.Store(&startedAt)
	close(s.connectionsReady)

	// 2. 等待停止条件，停止登记新连接，再取消并等待已登记的连接。
	s.waitUntilStopped(ctx)
	s.connectionsMu.Lock()
	s.runCtx = nil
	s.connectionsMu.Unlock()
	stopConnections()
	s.connections.Wait()
	return ctx.Err()
}

// waitUntilStopped 等到 ctx 结束、收到关闭请求或空闲超时。
func (s *runtimeServerImpl) waitUntilStopped(ctx context.Context) {
	// 1. 空闲超时大于零时启动周期检查。
	var idleChecks <-chan time.Time
	if s.opt.IdleTimeout > 0 {
		ticker := time.NewTicker(s.opt.IdleTimeout)
		defer ticker.Stop()
		idleChecks = ticker.C
	}
	// 2. 等待上下文结束、关闭请求或空闲超时。
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdownRequested:
			return
		case <-idleChecks:
			if s.isRuntimeIdle(ctx) {
				telemetry.GetLogger(ctx).Info("runtime server is idle, shutting down", "idle_timeout", s.opt.IdleTimeout.String())
				return
			}
		}
	}
}

// handleHealthz 回复健康检查成功。
func handleHealthz(c *gin.Context) {
	// 1. 返回 HTTP 200。
	c.Status(http.StatusOK)
}

// handleShutdown 接受关闭请求并通知 Run 停止。
func (s *runtimeServerImpl) handleShutdown(c *gin.Context) {
	// 1. 回复 HTTP 202，将关闭信号写入通知通道，已有信号时跳过。
	c.Status(http.StatusAccepted)
	select {
	case s.shutdownRequested <- struct{}{}:
	default:
	}
}

// webUINotBuiltText 是二进制没有打包页面时 /web/v1 返回的提示。
const webUINotBuiltText = "web UI is not built into this binary; build with script/build.sh"

// handleWebUI 返回页面静态文件；找不到且路径不带 "." 时返回 index.html，交给前端路由。
func handleWebUI(c *gin.Context, assets fs.FS) {
	// 1. 没有打包页面时返回 404 和构建提示。
	if assets == nil {
		c.String(http.StatusNotFound, webUINotBuiltText)
		return
	}
	// 2. 按请求路径读取文件，/web/v1/ 对应 index.html。
	name := strings.TrimPrefix(path.Clean(c.Param("filepath")), "/")
	if name == "" {
		name = "index.html"
	}
	content, err := fs.ReadFile(assets, name)
	// 3. 找不到且路径不带 "." 时读取 index.html。
	if err != nil && !strings.Contains(name, ".") {
		name = "index.html"
		content, err = fs.ReadFile(assets, name)
	}
	// 4. 仍然读不到返回 404。
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	// 5. assets/ 下的文件名带内容 hash，长期缓存；其余文件每次向 serve 确认。
	cacheControl := "no-cache"
	if strings.HasPrefix(name, "assets/") {
		cacheControl = "public, max-age=31536000, immutable"
	}
	c.Header("Cache-Control", cacheControl)
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, name, time.Time{}, bytes.NewReader(content))
}

// isRuntimeIdle 是否已经无连接满空闲时长，且 Runtime 没有执行 Turn 或后台命令？
func (s *runtimeServerImpl) isRuntimeIdle(ctx context.Context) bool {
	// 1. 有连接或离最后断开时间不足 IdleTimeout 时返回 false。
	if s.openConnectionCount.Load() > 0 || time.Since(*s.lastConnectionClosedAt.Load()) < s.opt.IdleTimeout {
		return false
	}
	// 2. 查询 Runtime 空闲状态，失败记 Warn 并返回 false，下一周期再查。
	isIdle, err := s.opt.Runtime.IsIdle(ctx)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("check runtime idle failed", "error", err)
		return false
	}
	return isIdle
}

// NewTokenMiddleware 校验访问令牌，拒绝时调用可选的 onReject 并返回 HTTP 401。
func NewTokenMiddleware(token string, onReject func(c *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 优先读取 Bearer token，未提供时读取查询参数 token。
		requestToken, found := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !found {
			requestToken = c.Query("token")
		}
		// 2. 令牌为空或不匹配时通知调用方并中止，匹配时继续。
		if token == "" || subtle.ConstantTimeCompare([]byte(requestToken), []byte(token)) != 1 {
			if onReject != nil {
				onReject(c)
			}
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// handleTokenRejection 记录拒绝访问的原因和 TCP 对端地址。
func (s *runtimeServerImpl) handleTokenRejection(c *gin.Context) {
	// 1. 使用请求上下文记录日志，握手尚未开始时也能记录。
	ctx := telemetry.ContextWithTelemetry(c.Request.Context(), s.opt.Telemetry)
	peerAddress, peerPort := getPeerAddress(c.Request.RemoteAddr)
	ctx = telemetry.ContextWithAttrs(ctx, telemetry.NetworkPeerAddress(peerAddress), telemetry.NetworkPeerPort(peerPort))
	telemetry.GetLogger(ctx).Warn("connection rejected", "reason", "invalid token")
}

// handleOpenVaalaUpgrade 使用 OpenVaala 方法表接管 WebSocket 连接。
func (s *runtimeServerImpl) handleOpenVaalaUpgrade(c *gin.Context) {
	// 1. 升级请求并服务连接直到断开。
	s.serveConnection(c, openvaalaMethods)
}

// serveConnection 登记连接任务，升级 WebSocket 并处理到连接结束。
func (s *runtimeServerImpl) serveConnection(c *gin.Context, methods map[string]methodHandler) {
	// 1. 等待连接上下文发布；请求先结束时直接返回。
	select {
	case <-s.connectionsReady:
	case <-c.Request.Context().Done():
		return
	}
	// 2. 升级前登记任务和连接数，服务停止时拒绝新连接。
	s.connectionsMu.Lock()
	runCtx := s.runCtx
	if runCtx == nil || runCtx.Err() != nil {
		s.connectionsMu.Unlock()
		c.Status(http.StatusServiceUnavailable)
		return
	}
	s.connections.Add(1)
	s.openConnectionCount.Add(1)
	s.connectionsMu.Unlock()
	defer s.connections.Done()
	defer func() {
		closedAt := time.Now()
		s.lastConnectionClosedAt.Store(&closedAt)
		s.openConnectionCount.Add(-1)
	}()
	// 3. 创建带连接 ID 和对端地址的上下文。
	peerAddress, peerPort := getPeerAddress(c.Request.RemoteAddr)
	ctx := telemetry.ContextWithAttrs(runCtx, telemetry.ConnectionID(uuid.NewString()),
		telemetry.NetworkPeerAddress(peerAddress), telemetry.NetworkPeerPort(peerPort))
	logger := telemetry.GetLogger(ctx)
	// 4. 接受 WebSocket 升级，不校验 Origin，其他地址的 Web UI 也能连。
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logger.Warn("websocket upgrade failed", "error", err)
		return
	}
	// 5. 运行连接，结束后更新遥测并按正常或异常原因记录日志。
	logger.Info("connection opened")
	telemetry.AddActiveConnections(ctx, 1)
	closeReason := newServerConnection(conn, &s.opt, methods).Run(ctx)
	telemetry.AddActiveConnections(ctx, -1)
	level := slog.LevelWarn
	if isNormalConnectionClose(closeReason) {
		level = slog.LevelInfo
	}
	logger.Log(ctx, level, "connection closed", "reason", closeReason)
}
