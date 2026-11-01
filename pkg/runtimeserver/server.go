// Package runtimeserver 是 Runtime 外层的网络协议宿主：把 WebSocket 连接上的 JSON-RPC 请求转换为 Runtime 公开调用，
// 并把订阅事件推送为通知。
package runtimeserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strconv"
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

// RuntimeServer 是 Runtime 外层的网络协议宿主：监听配置的地址，校验 token，把 WebSocket 连接上的
// JSON-RPC 请求转换为 Runtime 公开调用。
type RuntimeServer interface {
	// Run 监听并服务直到 ctx 结束、收到 shutdown 请求或空闲超时；返回前停止接受新连接，并关闭、等待全部已升级连接。只能调用一次。
	// 监听或服务失败时返回该错误；ctx 结束时返回 ctx.Err()；收到 shutdown 请求或空闲超时时返回 nil。
	Run(ctx context.Context) error
}

// runtimeServerImpl 保存路由引擎、已升级的 WebSocket 连接任务和连接计数。
type runtimeServerImpl struct {
	opt    RuntimeServerOpt
	engine *gin.Engine

	// started 是 Run 只能调用一次的令牌；runCtx 是 Run 的生命周期，在开始服务前写入，已升级连接从它派生。
	started chan struct{}
	runCtx  context.Context
	// connections 是已升级连接的处理任务：接管连接前登记、连接结束时注销；Run 在 http.Server.Shutdown 之后等待。
	connections sync.WaitGroup
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

// RuntimeServerOpt 保存 Runtime、模型配置、模型目录、监听位置与 token，构造后只读。
type RuntimeServerOpt struct {
	Runtime runtime.Runtime
	// Configuration 读写 serve 本机的模型配置，必须非 nil。
	Configuration Configuration
	// ModelCatalog 提供引导用的模型服务和模型，必须非 nil。
	ModelCatalog ModelCatalog
	// Listen 是 TCP 监听地址，例如 0.0.0.0:8790；Listener 非 nil 时忽略。
	Listen string
	// Listener 是调用方已建立的监听器；非 nil 时优先使用它，Run 返回前关闭。调用方可先写出实际地址再启动服务。
	Listener net.Listener
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

// NewRuntimeServerOpt 保存 Runtime、监听地址和访问令牌。
func NewRuntimeServerOpt(rt runtime.Runtime, listen, token string) *RuntimeServerOpt {
	// 1. 填入 Runtime、TCP 地址和 token。
	return &RuntimeServerOpt{
		Runtime: rt,
		Listen:  listen,
		Token:   token,
	}
}

// NewRuntimeServer 校验服务配置，注册健康检查、协议、关闭和页面路由。
func NewRuntimeServer(opt *RuntimeServerOpt) (RuntimeServer, error) {
	// 1. 检查 Runtime、配置服务、模型目录、监听位置和 token。
	if opt == nil || opt.Runtime == nil || opt.Configuration == nil || opt.ModelCatalog == nil ||
		(opt.Listen == "" && opt.Listener == nil) || opt.Token == "" {
		return nil, errors.New("runtime server requires a runtime, a configuration, a model catalog, a listen address or listener, and a token")
	}
	// 2. 切换 Gin 发布模式，创建路由引擎和运行通知通道。
	gin.SetMode(gin.ReleaseMode)
	s := &runtimeServerImpl{
		opt:     *opt,
		engine:  gin.New(),
		started: make(chan struct{}, 1),
		// 最多暂存一个关闭信号。
		shutdownRequested: make(chan struct{}, 1),
	}
	// 3. 注册健康检查，并给协议与关闭路由添加 token 校验。
	api := s.engine.Group("/api/v1")
	api.GET("/healthz", handleHealthz)
	protocols := api.Group("", s.requireToken)
	protocols.GET("/openvaala", s.handleOpenVaalaUpgrade)
	protocols.POST("/shutdown", s.handleShutdown)
	// 4. 注册页面路由，不校验 token；根路径跳转到页面，浏览器会把地址里的 #token 带过去。
	s.engine.GET("/web/v1/*filepath", s.handleWebUI)
	s.engine.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/web/v1/")
	})
	return s, nil
}

// Run 监听地址并运行 HTTP 与 WebSocket 服务，等到停止条件后关闭 HTTP 服务，再取消并等待全部已升级连接。
func (s *runtimeServerImpl) Run(ctx context.Context) error {
	// 1. 拒绝重复运行，绑定遥测。
	select {
	case s.started <- struct{}{}:
	default:
		return errors.New("RuntimeServer Run can only be called once")
	}
	ctx = telemetry.ContextWithTelemetry(ctx, s.opt.Telemetry)
	logger := telemetry.GetLogger(ctx)
	// 2. 使用传入的监听器，未传入时监听配置地址。
	listener := s.opt.Listener
	if listener == nil {
		var err error
		if listener, err = net.Listen("tcp", s.opt.Listen); err != nil {
			return fmt.Errorf("listen %s: %w", s.opt.Listen, err)
		}
	}
	// 3. 记录实际监听地址，创建带监听地址遥测的连接上下文和 HTTP 服务。
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	logger.Info("runtime server listening", "server.address", host, "server.port", portNumber)
	runCtx, stopConnections := context.WithCancel(telemetry.ContextWithAttrs(ctx, telemetry.ServerAddress(host), telemetry.ServerPort(portNumber)))
	defer stopConnections()
	s.runCtx = runCtx
	server := &http.Server{
		Handler:           s.engine,
		ReadHeaderTimeout: 10 * time.Second,
		// HTTP 错误写入遥测日志。
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	// 4. 记录空闲计时起点并启动 HTTP 服务。
	startedAt := time.Now()
	s.lastConnectionClosedAt.Store(&startedAt)
	serveErrs := make(chan error, 1)
	go func() {
		serveErrs <- server.Serve(listener)
	}()

	// 5. 等待取消、关闭请求、空闲超时或服务错误；没有服务错误时关闭 HTTP 服务：停止接受新连接，等待还没升级的请求。
	serveErr := s.waitUntilStopped(ctx, serveErrs)
	if serveErr == nil {
		serveErr = server.Shutdown(context.WithoutCancel(ctx))
		if err := <-serveErrs; !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, err)
		}
	}
	// 6. 取消全部升级连接并等待结束，返回服务错误或上下文错误；主动关闭和空闲超时返回 nil。
	stopConnections()
	s.connections.Wait()
	if serveErr != nil {
		return serveErr
	}
	return ctx.Err()
}

// waitUntilStopped 等到 ctx 结束、收到关闭请求、空闲超时或 HTTP 服务出错；只有服务出错时返回错误。
func (s *runtimeServerImpl) waitUntilStopped(ctx context.Context, serveErrs <-chan error) error {
	// 1. 空闲超时大于零时启动周期检查。
	var idleChecks <-chan time.Time
	if s.opt.IdleTimeout > 0 {
		ticker := time.NewTicker(s.opt.IdleTimeout)
		defer ticker.Stop()
		idleChecks = ticker.C
	}
	// 2. 等待上下文结束、关闭请求、空闲超时或服务错误；服务失败返回错误，其余停止条件返回 nil。
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErrs:
			return err
		case <-s.shutdownRequested:
			return nil
		case <-idleChecks:
			if s.isRuntimeIdle(ctx) {
				telemetry.GetLogger(ctx).Info("runtime server is idle, shutting down", "idle_timeout", s.opt.IdleTimeout.String())
				return nil
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
func (s *runtimeServerImpl) handleWebUI(c *gin.Context) {
	// 1. 没有打包页面时返回 404 和构建提示。
	if s.opt.WebUI == nil {
		c.String(http.StatusNotFound, webUINotBuiltText)
		return
	}
	// 2. 按请求路径读取文件，/web/v1/ 对应 index.html。
	name := strings.TrimPrefix(path.Clean(c.Param("filepath")), "/")
	if name == "" {
		name = "index.html"
	}
	content, err := fs.ReadFile(s.opt.WebUI, name)
	// 3. 找不到且路径不带 "." 时读取 index.html。
	if err != nil && !strings.Contains(name, ".") {
		name = "index.html"
		content, err = fs.ReadFile(s.opt.WebUI, name)
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

// requireToken 校验请求的访问令牌。
func (s *runtimeServerImpl) requireToken(c *gin.Context) {
	// 1. 优先读取 Bearer token，未提供时读取查询参数 token。
	token, found := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !found {
		token = c.Query("token")
	}
	// 2. 令牌不匹配时记录拒绝日志，返回 HTTP 401 并中止；匹配时继续。
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.opt.Token)) != 1 {
		peerAddress, peerPort := getPeerAddress(c.Request.RemoteAddr)
		telemetry.GetLogger(telemetry.ContextWithAttrs(s.runCtx, telemetry.NetworkPeerAddress(peerAddress), telemetry.NetworkPeerPort(peerPort))).
			Warn("connection rejected", "reason", "invalid token")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Next()
}

// handleOpenVaalaUpgrade 使用 OpenVaala 方法表接管 WebSocket 连接。
func (s *runtimeServerImpl) handleOpenVaalaUpgrade(c *gin.Context) {
	// 1. 升级请求并服务连接直到断开。
	s.serveConnection(c, openvaalaMethods)
}

// serveConnection 登记连接任务，升级 WebSocket 并处理到连接结束。
func (s *runtimeServerImpl) serveConnection(c *gin.Context, methods map[string]methodHandler) {
	// 1. 升级前登记任务和连接数，握手中的连接也计入；结束时记录断开时间并移除计数。
	s.connections.Add(1)
	defer s.connections.Done()
	s.openConnectionCount.Add(1)
	defer func() {
		closedAt := time.Now()
		s.lastConnectionClosedAt.Store(&closedAt)
		s.openConnectionCount.Add(-1)
	}()
	// 2. 创建带连接 ID 和对端地址的上下文。
	peerAddress, peerPort := getPeerAddress(c.Request.RemoteAddr)
	ctx := telemetry.ContextWithAttrs(s.runCtx, telemetry.ConnectionID(uuid.NewString()),
		telemetry.NetworkPeerAddress(peerAddress), telemetry.NetworkPeerPort(peerPort))
	logger := telemetry.GetLogger(ctx)
	// 3. 接受 WebSocket 升级，不校验 Origin，其他地址的 Web UI 也能连。
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logger.Warn("websocket upgrade failed", "error", err)
		return
	}
	// 4. 运行连接，结束后更新遥测并按正常或异常原因记录日志。
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
