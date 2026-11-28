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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/modelcatalog"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/httpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/sessiontoken"
	"github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
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
	command.Flags().String("relay-mode", "", "override world.relay_mode for this run (off, domain, public). Defaults to domain with the control plane, off otherwise")
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
func runServeCommand(ctx context.Context, args *AppArgs) error {
	listen := args.Config.Server.Listen
	restart, err := runServeOnce(ctx, args)
	if err != nil || restart == nil {
		return err
	}
	argv := os.Args
	if args.Config.Server.Listen != listen {
		argv = append(argv, "--listen", args.Config.Server.Listen)
	}
	err = restartServe(args.Executable, argv, os.Environ())
	if err != nil {
		slog.Error("restart serve", "error", err)
	}
	return err
}

func restartServe(executable string, argv, env []string) error {
	return syscall.Exec(executable, argv, env)
}

// archiveWorldServer 在关闭数据库后、释放数据目录锁前备份旧域。
func archiveWorldServer(dataDirectory, domain string) error {
	timestamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	return os.Rename(filepath.Join(dataDirectory, "worldserver"), filepath.Join(dataDirectory, "worldserver.bak-"+domain+"-"+timestamp))
}

// runServeOnce 运行并清理本次服务，返回已提交的重启请求。
func runServeOnce(ctx context.Context, args *AppArgs) (restart *runtimeserver.ServeRestartRequest, err error) {
	nodeCfg := args.Config.WorldNodeWithDefaults()
	if nodeCfg.Enabled {
		if err := args.Config.ValidateWorldRuntimeTargets(nodeCfg); err != nil {
			return nil, err
		}
	}
	// 1. 启动遥测，返回时最后关闭。
	tel, shutdownTelemetry, err := newTelemetry(ctx, args.Config, args.Stderr)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, shutdownTelemetry(context.WithoutCancel(ctx)))
	}()
	// 2. 绑定监听地址并取得数据目录锁。
	listener, err := net.Listen("tcp", args.Config.Server.Listen)
	if err != nil {
		return nil, newServeListenError(ctx, args.Config.Server.Listen, getRuntimeServerFilePath(args.Config), err)
	}
	// 返回时关闭监听器，忽略重复关闭的错误。
	defer func() { _ = listener.Close() }()
	dataDirectory := args.Config.GetDataDirectory()
	if err := ensureDataDirectory(dataDirectory); err != nil {
		return nil, err
	}
	unlock, err := lockDataDirectory(dataDirectory)
	if err != nil {
		return nil, err
	}
	// Runtime 和存储关闭后释放数据目录锁。
	defer func() {
		if err == nil && restart != nil && restart.ArchiveWorldServer {
			err = archiveWorldServer(dataDirectory, args.Config.World.Server.Domain)
		}
		err = errors.Join(err, unlock())
	}()
	// 3. 写入连接凭据，并在同一个路由器上装配已启用的控制面和节点。
	connection, err := writeRuntimeServerFile(args.Config, listener)
	token, serverID := connection.Token, connection.ServerID
	if err != nil {
		return nil, err
	}
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	files := config.NewConfigFiles(args.ConfigOpt)
	files.DataDirectory = dataDirectory
	var worldServer worldserver.WorldServer
	var publicRelay *worldserver.Relay
	if args.Config.World.Server.Enabled {
		worldServer, err = newConfiguredWorldServer(args.Config.World, listener.Addr().(*net.TCPAddr), dataDirectory, files, tel)
		if err != nil {
			return nil, err
		}
		worldServer.RegisterRoutes(engine)
	} else if args.Config.World.GetRelayMode() == "public" {
		publicRelay = worldserver.NewRelay(args.Config.World.RelayInstances)
		publicRelay.RegisterRoutes(engine)
	}
	remote := args.Config.World.RemoteAccess
	publicRelays := args.Config.World.Relays
	if nodeCfg.Enabled && worldServer != nil && args.Config.World.Server.Domain == nodeCfg.Domain && args.Config.World.GetRelayMode() != "public" {
		home, _ := world.GetWorldSocketURL(nodeCfg.ServerURL, "")
		publicRelays = slices.DeleteFunc(slices.Clone(publicRelays), func(raw string) bool {
			base, _ := world.GetWorldSocketURL(raw, "")
			return base == home
		})
	}
	if remote.Name == "" && nodeCfg.Enabled {
		remote.Name, _ = os.Hostname()
	}
	var node world.WorldNode
	if nodeCfg.Enabled {
		directory := filepath.Join(dataDirectory, "world")
		identity, identityErr := world.LoadOrCreateIdentity(filepath.Join(directory, "identity.json"))
		if identityErr != nil {
			return nil, identityErr
		}
		database, openErr := sqlite.OpenDatabase(sqlite.NewDatabaseOpt(filepath.Join(directory, "world.db"), world.WorldNamespaces()...))
		if openErr != nil {
			return nil, openErr
		}
		defer func() { err = errors.Join(err, database.Close()) }()
		nodeOpt := world.NewWorldNodeOpt(database.Record(), identity, nodeCfg.Name, nodeCfg.Domain, nodeCfg.ServerURL)
		nodeOpt.DirectURLs, nodeOpt.DefaultTarget, nodeOpt.AdvertiseTargets = nodeCfg.DirectURLs, nodeCfg.DefaultTarget, nodeCfg.AdvertiseTargets
		nodeOpt.RemoteAccessName = remote.Name
		nodeOpt.WebURL = args.Config.World.WebURL
		nodeOpt.Relays = publicRelays
		nodeOpt.Telemetry = tel
		node, err = world.NewWorldNode(nodeOpt)
		if err != nil {
			return nil, err
		}
		files.ExtraTools = node
	}
	// 4. 使用同一组附加工具创建 Runtime 和配置重载入口。
	tokenSecret := sessiontoken.NewSecret(token)
	rt, closeRuntimeResources, err := newRuntime(ctx, args.Config, tel, connection, tokenSecret, files.ExtraTools)
	if err != nil {
		return nil, err
	}
	world.RegisterDirectRoutes(engine, node, worldServer)
	// Runtime 停止后关闭存储。
	defer func() {
		err = errors.Join(err, closeRuntimeResources())
	}()
	// 5. 打开 models.dev 模型目录缓存，创建 Runtime Server。
	cacheDirectory, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locate cache directory: %w", err)
	}
	catalogHTTPClient := httpclient.NewHTTPClient(httpclient.NewHTTPClientOpt().WithHeaders(args.Config.GetHTTPHeaders()))
	modelCatalog, err := modelcatalog.NewCatalog(modelcatalog.NewCatalogOpt(filepath.Join(cacheDirectory, "openvaala", "models-dev.json")).WithHTTPClient(catalogHTTPClient))
	if err != nil {
		return nil, err
	}
	serverOpt := &runtimeserver.RuntimeServerOpt{Runtime: rt, TokenSecret: tokenSecret}
	restartRequests := make(chan runtimeserver.ServeRestartRequest, 1)
	serverOpt.ConnectionFile = getRuntimeServerFilePath(args.Config)
	serverOpt.ConfiguredToken = args.Config.Server.Token
	serverOpt.RemoteAccessName = remote.Name
	if nodeCfg.ServerURL != "" {
		serverOpt.RegisteredNode = world.Address{Node: nodeCfg.Name, Domain: nodeCfg.Domain}
	}
	serverOpt.RemoteAccessRelays = publicRelays
	serverOpt.RemoteAccessWebURL = world.SelectRemoteAccessPage(args.Config.World.WebURL, publicRelays, nodeCfg.DirectURLs)
	serverOpt.WorldRunning = runtimeprotocol.WorldRunning{RemoteAccessEnabled: remote.Enabled, NodeEnabled: node != nil, ServerEnabled: worldServer != nil}
	serverOpt.WorldNode, serverOpt.WorldServer = node, worldServer
	serverOpt.WorldIdentityPath = filepath.Join(dataDirectory, "world", "identity.json")
	serverOpt.RestartRequests = restartRequests
	serverOpt.ServerID = serverID
	serverOpt.Telemetry = tel
	serverOpt.Configuration = files
	serverOpt.ModelCatalog = modelCatalog
	serverOpt.WebUI = www.GetAssets()
	if !remote.Enabled && node == nil && worldServer == nil && publicRelay == nil {
		serverOpt.IdleTimeout = args.IdleTimeout
	}
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		return nil, err
	}

	// 6. 启动 Runtime；返回时等 Server 关闭全部连接后再停止并等待 Runtime。
	stopRuntime := startDetachedRun(ctx, rt.Run)
	defer func() {
		err = errors.Join(err, stopRuntime())
	}()

	// 7. 挂载 Runtime 路由并运行共享的 HTTP 服务。
	server.RegisterRoutes(engine)
	address := listener.Addr().(*net.TCPAddr)
	if host, port, _ := net.SplitHostPort(args.Config.Server.Listen); port == "0" {
		args.Config.Server.Listen = net.JoinHostPort(host, fmt.Sprint(address.Port))
	}
	serverCtx := telemetry.ContextWithTelemetry(ctx, tel)
	serverCtx = telemetry.ContextWithAttrs(serverCtx, telemetry.ServerAddress(address.IP.String()), telemetry.ServerPort(address.Port))
	telemetry.GetLogger(serverCtx).Info("runtime server listening")
	fmt.Fprintf(args.Stdout, "serving OpenVaala on %s, WebSocket path /api/v1/openvaala\n", args.Config.Server.Listen)
	return runServeHTTP(serverCtx, listener, engine, server, rt, node, worldServer, publicRelay, restartRequests, remote.Enabled && len(publicRelays) > 0)
}

// runServeHTTP 运行已装配的服务，取消长连接并等待各服务退出。
func runServeHTTP(ctx context.Context, listener net.Listener, engine *gin.Engine, server runtimeserver.RuntimeServer, rt runtime.Runtime, node world.WorldNode, worldServer worldserver.WorldServer, publicRelay *worldserver.Relay, restarts <-chan runtimeserver.ServeRestartRequest, remoteAccess bool) (*runtimeserver.ServeRestartRequest, error) {
	// 1. HTTP 请求、控制面、节点与 Runtime Server 共用取消信号。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpServer := &http.Server{
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(telemetry.GetLogger(ctx).Handler(), slog.LevelError),
		BaseContext:       func(net.Listener) context.Context { return runCtx },
	}
	// 2. 仅启动已装配的服务，任一服务返回即唤醒其余服务。
	var worldServerDone, worldNodeDone, serverDone, relayDone chan error
	if publicRelay != nil {
		relayDone = make(chan error, 1)
		go func() { err := publicRelay.Run(runCtx); cancel(); relayDone <- err }()
	}
	if worldServer != nil {
		worldServerDone = make(chan error, 1)
		go func() {
			runErr := worldServer.Run(runCtx)
			cancel()
			worldServerDone <- runErr
		}()
	}
	if node != nil {
		worldNodeDone = make(chan error, 1)
		go func() {
			var rpc world.WorldRPCServer
			if remoteAccess {
				rpc = server
			}
			runErr := node.Run(runCtx, world.NewRuntimeHost(rt), rpc)
			cancel()
			worldNodeDone <- runErr
		}()
	}
	if server != nil {
		serverDone = make(chan error, 1)
		go func() {
			runErr := server.Run(runCtx)
			cancel()
			serverDone <- runErr
		}()
	}
	httpDone := make(chan error, 1)
	go func() {
		runErr := httpServer.Serve(listener)
		cancel()
		httpDone <- runErr
	}()
	var restart *runtimeserver.ServeRestartRequest
	select {
	case <-runCtx.Done():
	case req := <-restarts:
		restart = &req
		cancel()
	}
	// 3. 限期排空 HTTP，再依次等后台服务退出，之后调用方才释放 Runtime 和存储。
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelShutdown()
	err := httpServer.Shutdown(shutdownCtx)
	if err != nil {
		err = errors.Join(err, httpServer.Close())
	}
	if httpErr := <-httpDone; !errors.Is(httpErr, http.ErrServerClosed) {
		err = errors.Join(err, httpErr)
	}
	for _, done := range []chan error{serverDone, worldNodeDone, worldServerDone, relayDone} {
		if done != nil {
			if runErr := <-done; !errors.Is(runErr, context.Canceled) {
				err = errors.Join(err, runErr)
			}
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = errors.Join(err, ctx.Err())
	}
	return restart, err
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
func writeRuntimeServerFile(cfg *config.Config, listener net.Listener) (RuntimeServerConnection, error) {
	// 1. 定位连接文件并读取已有内容；文件缺失或损坏时使用空记录。
	path := getRuntimeServerFilePath(cfg)
	existing, _, _ := getRuntimeServerFile(path)
	// 2. 令牌依次用配置的 server.token 和文件里已有的，server_id 沿用文件里的；都没有时生成随机值。
	var err error
	token := cmp.Or(cfg.Server.Token, existing.Token)
	resetFor := ""
	if existing.ResetFor != "" && existing.ResetFor == config.ConfiguredTokenDigest(cfg.Server.Token) {
		token = existing.Token
		resetFor = existing.ResetFor
	}
	if token == "" {
		if token, err = newRandomHex(32); err != nil {
			return RuntimeServerConnection{}, err
		}
	}
	serverID := existing.ServerID
	if serverID == "" {
		if serverID, err = newRandomHex(16); err != nil {
			return RuntimeServerConnection{}, err
		}
	}
	// 3. 按实际绑定的地址生成连接 URL，监听全部网卡时改用 127.0.0.1。
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return RuntimeServerConnection{}, err
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	file := RuntimeServerConnection{
		ResetFor: resetFor,
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
		return RuntimeServerConnection{}, fmt.Errorf("encode runtime server file: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return RuntimeServerConnection{}, fmt.Errorf("create runtime server file directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".runtime-server-*.yaml")
	if err != nil {
		return RuntimeServerConnection{}, fmt.Errorf("write runtime server file: %w", err)
	}
	_, writeErr := temporary.Write(payload)
	if err := errors.Join(writeErr, temporary.Close()); err != nil {
		_ = os.Remove(temporary.Name())
		return RuntimeServerConnection{}, fmt.Errorf("write runtime server file: %w", err)
	}
	// 6. 用临时文件替换连接文件，失败时删除临时文件。
	if err := os.Rename(temporary.Name(), path); err != nil {
		_ = os.Remove(temporary.Name())
		return RuntimeServerConnection{}, fmt.Errorf("write runtime server file: %w", err)
	}
	return file, nil
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
