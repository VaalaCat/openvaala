package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/modelcatalog"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/httpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/www"
	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	// runtimeServerHealthzTimeout 是单次 healthz 请求的最长等待。
	runtimeServerHealthzTimeout = time.Second
	// runtimeServerShutdownRequestTimeout 是 serve stop 发出 shutdown 请求后等待回复的最长时间。
	runtimeServerShutdownRequestTimeout = time.Second
	// runtimeServerStopTimeout 是 serve stop 等待 serve 停止的最长时间。
	runtimeServerStopTimeout = 10 * time.Second
	// runtimeServerStopPollInterval 是 serve stop 等待期间检查 serve 是否已停止的间隔。
	runtimeServerStopPollInterval = time.Second
	// noRuntimeServerText 是 serve stop、serve reload 发现没有 serve 在运行时输出的文本。
	noRuntimeServerText = "no runtime server is running\n"
)

// newServeCommand 创建 serve 命令和它的 stop、reload 子命令。
func newServeCommand() *cobra.Command {
	// 1. 注册 serve 命令和隐藏参数 --idle-timeout：客户端自动拉起 serve 时传入，手动运行的 serve 默认常驻。
	command := &cobra.Command{
		Use:   "serve",
		Short: "Serve the Runtime over WebSocket for Web UIs on the local network",
		Args:  cobra.NoArgs,
		RunE:  BuildCLIHandler(runServeCommand),
	}
	command.Flags().String("listen", "", "Runtime Server listen address (host:port)")
	command.Flags().Duration("idle-timeout", 0, "exit after this long without connections or running turns (0 keeps running)")
	_ = command.Flags().MarkHidden("idle-timeout")
	// 2. 注册 stop 和 reload 子命令。
	command.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the running runtime server",
		Args:  cobra.NoArgs,
		RunE:  BuildCLIHandler(runServeStopCommand),
	}, &cobra.Command{
		Use:   "reload",
		Short: "Reload agent and model configuration in the running runtime server",
		Args:  cobra.NoArgs,
		RunE: BuildCLIHandler(func(ctx context.Context, args *AppArgs) error {
			return runServeReloadCommand(ctx, args, noRuntimeServerText)
		}),
	})
	return command
}

// runServeCommand 运行 serve：绑定端口、锁定数据目录、写连接文件，运行 Runtime 和 Runtime Server，返回前按相反顺序关闭。
func runServeCommand(ctx context.Context, args *AppArgs) (err error) {
	// 1. 启动遥测，返回时最后关闭。
	tel, shutdownTelemetry, err := newTelemetry(ctx, args.Config, args.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, shutdownTelemetry(context.WithoutCancel(ctx)))
	}()
	// 2. 绑定监听地址并取得数据目录锁。
	listener, err := net.Listen("tcp", args.Config.Server.Listen)
	if err != nil {
		return newServeListenError(ctx, args.Config.Server.Listen, getRuntimeServerFilePath(args.Config), err)
	}
	// 返回时关闭监听器，忽略重复关闭的错误。
	defer func() { _ = listener.Close() }()
	dataDirectory := args.Config.GetDataDirectory()
	if err := ensureDataDirectory(dataDirectory); err != nil {
		return err
	}
	unlock, err := lockDataDirectory(dataDirectory)
	if err != nil {
		return err
	}
	// Runtime 和存储关闭后释放数据目录锁。
	defer func() {
		err = errors.Join(err, unlock())
	}()
	// 3. 写入连接文件，打开存储并创建 Runtime。
	token, serverID, err := writeRuntimeServerFile(args.Config, listener)
	if err != nil {
		return err
	}
	rt, closeRuntimeResources, err := newRuntime(ctx, args.Config, tel)
	if err != nil {
		return err
	}
	// Runtime 停止后关闭存储。
	defer func() {
		err = errors.Join(err, closeRuntimeResources())
	}()
	// 4. 打开 models.dev 模型目录缓存，创建 Runtime Server。
	cacheDirectory, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("locate cache directory: %w", err)
	}
	catalogHTTPClient := httpclient.NewHTTPClient(httpclient.NewHTTPClientOpt().WithHeaders(args.Config.GetHTTPHeaders()))
	modelCatalog, err := modelcatalog.NewCatalog(modelcatalog.NewCatalogOpt(filepath.Join(cacheDirectory, "openvaala", "models-dev.json")).WithHTTPClient(catalogHTTPClient))
	if err != nil {
		return err
	}
	serverOpt := runtimeserver.NewRuntimeServerOpt(rt, token)
	serverOpt.ServerID = serverID
	serverOpt.Telemetry = tel
	files := config.NewConfigFiles(args.ConfigOpt)
	files.DataDirectory = dataDirectory
	serverOpt.Configuration = files
	serverOpt.ModelCatalog = modelCatalog
	serverOpt.WebUI = www.GetAssets()
	serverOpt.IdleTimeout = args.IdleTimeout
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		return err
	}

	// 5. 启动 Runtime；返回时等 Server 关闭全部连接后再停止并等待 Runtime。
	stopRuntime := startDetachedRun(ctx, rt.Run)
	defer func() {
		err = errors.Join(err, stopRuntime())
	}()

	// 6. 将 Runtime 路由挂到 HTTP 服务，连接遥测使用实际监听地址。
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	server.RegisterRoutes(engine)
	address := listener.Addr().(*net.TCPAddr)
	serverCtx := telemetry.ContextWithTelemetry(ctx, tel)
	serverCtx = telemetry.ContextWithAttrs(serverCtx, telemetry.ServerAddress(address.IP.String()), telemetry.ServerPort(address.Port))
	runCtx, stopServer := context.WithCancel(context.WithoutCancel(serverCtx))
	logger := telemetry.GetLogger(serverCtx)
	httpServer := &http.Server{
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		BaseContext:       func(net.Listener) context.Context { return runCtx },
	}

	// 7. 启动 Runtime Server 和 HTTP 服务，等待取消或任一服务结束。
	logger.Info("runtime server listening")
	fmt.Fprintf(args.Stdout, "serving OpenVaala on %s, WebSocket path /api/v1/openvaala\n", args.Config.Server.Listen)
	serverDone, httpDone := make(chan struct{}), make(chan struct{})
	var serverErr, httpErr error
	go func() {
		defer close(serverDone)
		serverErr = server.Run(runCtx)
	}()
	go func() {
		defer close(httpDone)
		httpErr = httpServer.Serve(listener)
	}()
	select {
	case <-ctx.Done():
	case <-serverDone:
	case <-httpDone:
	}

	// 8. 取消请求上下文唤醒长连接与 Runtime Server，再限期排空 HTTP 服务，最后由 defer 停止 Runtime。
	stopServer()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelShutdown()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, httpServer.Close())
	}
	<-httpDone
	<-serverDone
	if errors.Is(httpErr, http.ErrServerClosed) {
		httpErr = nil
	}
	if errors.Is(serverErr, context.Canceled) {
		serverErr = nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		serverErr = errors.Join(serverErr, ctx.Err())
	}
	return errors.Join(shutdownErr, httpErr, serverErr)
}

// newServeListenError 生成监听失败的错误；本机已有 serve 在运行时提示先 serve stop。
func newServeListenError(ctx context.Context, listen, runtimeServerFilePath string, listenErr error) error {
	// 1. 取出监听错误里的原因，前面加上监听地址。
	if opErr, ok := errors.AsType[*net.OpError](listenErr); ok {
		listenErr = opErr.Err
	}
	err := fmt.Errorf("listen %s: %w", listen, listenErr)
	// 2. 本机连接文件指向的 serve 能访问时补上 serve stop 提示。
	if _, isReachable, readErr := getReachableRuntimeServerFile(ctx, runtimeServerFilePath); readErr != nil || !isReachable {
		return err
	}
	return fmt.Errorf("%w; a runtime server is already running, run openvaala serve stop first", err)
}

// writeRuntimeServerFile 确定令牌和 server_id，写入本机连接文件 runtime-server.yaml，返回这两项。
func writeRuntimeServerFile(cfg *config.Config, listener net.Listener) (token, serverID string, err error) {
	// 1. 定位连接文件并读取已有内容；文件缺失或损坏时使用空记录。
	path := getRuntimeServerFilePath(cfg)
	existing, _, _ := getRuntimeServerFile(path)
	// 2. 令牌依次用配置的 server.token 和文件里已有的，server_id 沿用文件里的；都没有时生成随机值。
	token = cmp.Or(cfg.Server.Token, existing.Token)
	if token == "" {
		if token, err = newRandomHex(32); err != nil {
			return "", "", err
		}
	}
	serverID = existing.ServerID
	if serverID == "" {
		if serverID, err = newRandomHex(16); err != nil {
			return "", "", err
		}
	}
	// 3. 按实际绑定的地址生成连接 URL，监听全部网卡时改用 127.0.0.1。
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return "", "", err
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	file := RuntimeServerConnection{
		URL:      "ws://" + net.JoinHostPort(host, port) + "/api/v1/openvaala",
		Token:    token,
		ServerID: serverID,
	}
	// 4. 记下当前可执行文件的路径和修改时间；取不到时记录警告，两项留空。
	file.Executable, file.ExecutableModifiedAt, err = getCurrentExecutable()
	if err != nil {
		slog.Warn("cannot record the serve executable; clients will not notice an outdated serve", "error", err)
	}
	// 5. 编码连接信息，以 0700 创建目录，写进同目录的 0600 临时文件；写入失败时删除临时文件。
	payload, err := yaml.Marshal(file)
	if err != nil {
		return "", "", fmt.Errorf("encode runtime server file: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", "", fmt.Errorf("create runtime server file directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".runtime-server-*.yaml")
	if err != nil {
		return "", "", fmt.Errorf("write runtime server file: %w", err)
	}
	_, writeErr := temporary.Write(payload)
	if err := errors.Join(writeErr, temporary.Close()); err != nil {
		_ = os.Remove(temporary.Name())
		return "", "", fmt.Errorf("write runtime server file: %w", err)
	}
	// 6. 用临时文件替换连接文件，失败时删除临时文件。
	if err := os.Rename(temporary.Name(), path); err != nil {
		_ = os.Remove(temporary.Name())
		return "", "", fmt.Errorf("write runtime server file: %w", err)
	}
	return token, serverID, nil
}

// newRandomHex 生成 size 字节随机值的十六进制文本。
func newRandomHex(size int) (string, error) {
	// 1. 读取随机字节并编码为十六进制。
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// isRuntimeServerReachable 连接文件指向的 serve 现在是否可访问？
func isRuntimeServerReachable(ctx context.Context, file RuntimeServerConnection) bool {
	// 1. 生成 healthz 地址，准备超时 1 秒的 GET 请求；地址无效时返回 false。
	healthzURL, err := getRuntimeServerHTTPURL(file.URL, "/api/v1/healthz")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, runtimeServerHealthzTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthzURL, nil)
	if err != nil {
		return false
	}
	// 2. 不复用连接发送一次请求，只有 HTTP 200 返回 true。
	request.Close = true
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// runServeStopCommand 请求本机 serve 停止，等它真正停下后输出 stopped。
func runServeStopCommand(ctx context.Context, args *AppArgs) error {
	// 1. 读取连接文件；不存在时输出未运行提示。
	file, found, err := getRuntimeServerFile(getRuntimeServerFilePath(args.Config))
	if err != nil {
		return err
	}
	if !found {
		return writeText(args.Stdout, noRuntimeServerText)
	}
	// 2. 定位数据目录，向 shutdown 地址发送带令牌的 POST 请求；连接被拒绝时输出未运行提示。
	shutdownURL, err := getRuntimeServerHTTPURL(file.URL, "/api/v1/shutdown")
	if err != nil {
		return err
	}
	response, err := requestRuntimeServerShutdown(ctx, shutdownURL, file.Token)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return writeText(args.Stdout, noRuntimeServerText)
	}
	if err != nil {
		return fmt.Errorf("stop runtime server: %w", err)
	}
	// 3. 只接受 202；令牌被拒或其他状态时返回错误。
	switch response.StatusCode {
	case http.StatusAccepted:
	case http.StatusUnauthorized:
		return errors.New("runtime server rejected the token")
	default:
		return fmt.Errorf("stop runtime server: unexpected response %s", response.Status)
	}

	// 4. 每秒检查一次 serve 是否已停止，最多等 10 秒。
	dataDirectory := args.Config.GetDataDirectory()
	deadline := time.Now().Add(runtimeServerStopTimeout)
	for !isRuntimeServerStopped(ctx, file, dataDirectory) {
		if !time.Now().Before(deadline) {
			return errors.New("runtime server did not stop within 10s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(runtimeServerStopPollInterval):
		}
	}
	// 5. ctx 已结束时返回它的错误，否则输出 stopped。
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeText(args.Stdout, "stopped\n")
}

// runServeReloadCommand 请求本机 serve 重新加载配置，输出结果和还在用旧配置的对话。
func runServeReloadCommand(ctx context.Context, args *AppArgs, notRunningText string) error {
	// 1. 读取本机连接文件，serve 不能访问时输出 notRunningText。
	file, isReachable, err := getReachableRuntimeServerFile(ctx, getRuntimeServerFilePath(args.Config))
	if err != nil {
		return err
	}
	if !isReachable {
		return writeText(args.Stdout, notRunningText)
	}
	// 2. 重新加载配置，输出等待空闲后自动应用的对话 ID。
	result, err := reloadRuntimeServerConfig(ctx, file)
	if err != nil {
		return err
	}
	text := "configuration reloaded\n"
	if count := len(result.PendingConversationIDs); count > 0 {
		ids := make([]string, count)
		for index, id := range result.PendingConversationIDs {
			ids[index] = string(id)
		}
		text += fmt.Sprintf("%d conversations will apply the new configuration automatically when idle: %s\n", count, strings.Join(ids, ", "))
	}
	return writeText(args.Stdout, text)
}

// reloadRuntimeServerConfig 用一个临时 Client 连接本机 serve，请求重新加载配置。
func reloadRuntimeServerConfig(ctx context.Context, file RuntimeServerConnection) (runtime.SetDefinitionsResult, error) {
	// 1. 创建 Client 并启动连接。
	client, err := runtimeclient.NewClient(runtimeclient.NewClientOpt(file.URL, file.Token))
	if err != nil {
		return runtime.SetDefinitionsResult{}, err
	}
	clientCtx, stopClient := context.WithCancelCause(ctx)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		stopClient(client.Run(clientCtx))
	}()
	// 2. 返回时取消 Client，读完剩下的事件，等它的 goroutine 结束。
	defer func() {
		stopClient(nil)
		for range client.Events() {
		}
		<-clientDone
	}()
	// 3. 第一次连上时请求重新加载；进入重连时返回连接错误。
	for event := range client.Events() {
		changed, ok := event.(*runtimeclient.ConnectionChanged)
		switch {
		case !ok:
		case changed.State == runtimeclient.ConnectionConnected:
			return client.ReloadConfig(clientCtx)
		case changed.State == runtimeclient.ConnectionReconnecting:
			return runtime.SetDefinitionsResult{}, fmt.Errorf("connect to runtime server: %w", changed.Err)
		}
	}
	// 4. 连上之前事件流就结束了：等 Client 结束，返回 context 里的结束原因。
	<-clientDone
	return runtime.SetDefinitionsResult{}, context.Cause(clientCtx)
}

// requestRuntimeServerShutdown 向 serve 发送带令牌的 shutdown 请求，返回响应。
func requestRuntimeServerShutdown(ctx context.Context, shutdownURL, token string) (*http.Response, error) {
	// 1. 创建最长等待 1 秒的关闭请求。
	ctx, cancel := context.WithTimeout(ctx, runtimeServerShutdownRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, shutdownURL, nil)
	if err != nil {
		return nil, err
	}
	// 2. 加上令牌，不复用连接发送 POST 请求，关闭响应正文后返回响应。
	request.Header.Set("Authorization", "Bearer "+token)
	request.Close = true
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	_ = response.Body.Close()
	return response, nil
}

// isRuntimeServerStopped 本机 serve 是否已经停止？healthz 访问不到且数据目录锁能取得才算。
func isRuntimeServerStopped(ctx context.Context, file RuntimeServerConnection, dataDirectory string) bool {
	// 1. healthz 能访问时返回 false；否则尝试取得并释放数据目录锁，取锁或释放失败时返回 false。
	if isRuntimeServerReachable(ctx, file) {
		return false
	}
	unlock, err := lockDataDirectory(dataDirectory)
	if err != nil {
		return false
	}
	return unlock() == nil
}
