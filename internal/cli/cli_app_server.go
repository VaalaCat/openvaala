package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/internal/appserver"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// appServerSocketProbeTimeout 是启动监听前试连已有 socket 文件的最长等待：连得上说明已有 app-server 在监听，连不上就当残留文件删掉。
const appServerSocketProbeTimeout = time.Second

// codexRootFlagNames 是 codex 根命令的参数名，注册在 OpenVaala 的根命令上，写在子命令前面或后面都能收下。
var codexRootFlagNames = []string{"config", "enable", "disable", "strict-config"}

// ignoredAppServerFlagNames 是 codex app-server 有、OpenVaala 收下不用的参数名，注册在 app-server 上。
var ignoredAppServerFlagNames = []string{"analytics-default-enabled", "code-mode-host", "remote-control", "startup-client-info", "startup-sign-out"}

// ignoredCodexFlagNames 是 app-server 收下不用的全部 codex 参数，按这个顺序记入 AppArgs.IgnoredCodexFlags。
var ignoredCodexFlagNames = slices.Concat(codexRootFlagNames, ignoredAppServerFlagNames)

// webSocketAuthFlagNames 是 app-server 收到就报错的 Codex WebSocket 认证参数名。
var webSocketAuthFlagNames = []string{
	"ws-auth", "ws-token-file", "ws-token-sha256", "ws-shared-secret-file", "ws-issuer", "ws-audience", "ws-max-clock-skew-seconds",
}

// addCodexRootFlags 在 flags 上注册 codex 根命令参数并从帮助中隐藏。
func addCodexRootFlags(flags *pflag.FlagSet) {
	// 1. 注册 -c/--config、--enable、--disable 和 --strict-config 并隐藏；openvaala codex 把它们交给 codex，app-server 收下不用。
	flags.StringArrayP("config", "c", nil, "Codex configuration override key=value (ignored by app-server; passed to codex by openvaala codex)")
	flags.StringArray("enable", nil, "Codex feature to enable (ignored by app-server; passed to codex by openvaala codex)")
	flags.StringArray("disable", nil, "Codex feature to disable (ignored by app-server; passed to codex by openvaala codex)")
	flags.Bool("strict-config", false, "Codex strict config mode (ignored by app-server; passed to codex by openvaala codex)")
	for _, name := range codexRootFlagNames {
		_ = flags.MarkHidden(name)
	}
}

// newAppServerCommand 创建 app-server 命令，注册监听、启动检查参数和 codex app-server 的其余参数。
func newAppServerCommand() *cobra.Command {
	// 1. 创建 app-server 命令，命令行写法和 codex app-server 一致。
	command := &cobra.Command{
		Use:   "app-server",
		Short: "Serve Codex clients over the Codex app-server protocol (stdio, a Unix socket or a loopback WebSocket)",
		Long: `Serve Codex clients over the Codex app-server protocol.

The command line follows "codex app-server", so a tool that runs "codex app-server ..." can run "openvaala app-server ..." instead.
--listen takes stdio:// (default), unix://, unix://PATH and ws://IP:PORT; ws:// only accepts loopback addresses and needs no authentication.
The other arguments of "codex app-server" that OpenVaala does not need are accepted and ignored (they are hidden from this help).
The WebSocket authentication arguments (--ws-*) are rejected, because app-server only listens on loopback and the runtime server
checks the token.`,
		Args: cobra.NoArgs,
		RunE: BuildCLIHandler(runAppServerCommand),
	}
	// 2. 注册监听方式、隐藏的启动查询参数和收下不用的 codex 参数。
	flags := command.Flags()
	flags.String("listen", "stdio://", "listen on stdio://, unix:// (default socket path), unix://PATH or ws://IP:PORT (loopback only)")
	flags.Bool("stdio", false, "use stdio as the transport (same as --listen stdio://)")
	flags.Bool("analytics-default-enabled", false, "Codex analytics default (accepted and ignored)")
	flags.String("code-mode-host", "", "Codex code-mode host URL (accepted and ignored)")
	flags.Bool("remote-control", false, "Codex remote control (accepted and ignored)")
	flags.Bool("print-startup-config", false, "print startup configuration as JSON and exit")
	flags.String("startup-client-info", "", "Codex startup client information (accepted and ignored)")
	flags.Bool("startup-sign-out", false, "Codex startup sign-out (accepted and ignored)")
	_ = flags.MarkHidden("print-startup-config")
	// 3. 注册运行时会拒绝的 WebSocket 认证参数，把它们和收下不用的参数从帮助中隐藏，再注册 --url。
	for _, name := range webSocketAuthFlagNames {
		flags.String(name, "", "Codex WebSocket authentication (not supported)")
	}
	for _, name := range slices.Concat(ignoredAppServerFlagNames, webSocketAuthFlagNames) {
		_ = flags.MarkHidden(name)
	}
	addURLFlag(flags)
	return command
}

// readCodexFlags 把命令上注册了的 codex 参数和 app-server 监听参数读进 args。
func readCodexFlags(flags *pflag.FlagSet, args *AppArgs) (err error) {
	// 1. 收集要交给 codex、收下不用和要拒绝的 codex 参数。
	if args.CodexRootArguments, err = getCodexRootArguments(flags); err != nil {
		return err
	}
	for _, name := range ignoredCodexFlagNames {
		if flag := flags.Lookup(name); flag != nil && flag.Changed {
			args.IgnoredCodexFlags = append(args.IgnoredCodexFlags, "--"+name)
		}
	}
	for _, name := range webSocketAuthFlagNames {
		if flag := flags.Lookup(name); flag != nil && flag.Changed {
			args.UnsupportedWebSocketFlags = append(args.UnsupportedWebSocketFlags, "--"+name)
		}
	}
	// 2. 读取 --print-startup-config、--listen 和 --stdio。
	if flags.Lookup("print-startup-config") != nil {
		if args.PrintAppServerStartupConfig, err = flags.GetBool("print-startup-config"); err != nil {
			return err
		}
	}
	if flag := flags.Lookup("listen"); flag != nil {
		args.AppServerListen = flag.Value.String()
		args.IsAppServerStdio, err = flags.GetBool("stdio")
	}
	return err
}

// getCodexRootArguments 把收到的 codex 根命令参数按 codex 的写法排好。
func getCodexRootArguments(flags *pflag.FlagSet) ([]string, error) {
	// 1. 每个 -c、--enable 和 --disable 的值各写成一对参数。
	var arguments []string
	for _, option := range []struct{ name, codexFlag string }{{
		"config",
		"-c",
	}, {
		"enable",
		"--enable",
	}, {
		"disable",
		"--disable",
	}} {
		values, err := flags.GetStringArray(option.name)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			arguments = append(arguments, option.codexFlag, value)
		}
	}
	// 2. 传了 --strict-config 时追加它。
	isStrict, err := flags.GetBool("strict-config")
	if err != nil {
		return nil, err
	}
	if isStrict {
		arguments = append(arguments, "--strict-config")
	}
	return arguments, nil
}

// runAppServerCommand 输出启动配置，或按监听方式运行 AppServer。
func runAppServerCommand(ctx context.Context, args *AppArgs) (err error) {
	// 1. 解析监听参数；--print-startup-config 时输出启动配置 JSON 后返回。
	listen, err := getAppServerListen(args)
	if err != nil {
		return err
	}
	if args.PrintAppServerStartupConfig {
		return appserver.WriteStartupConfig(args.Stdout, defs.GetBuildVersion())
	}
	// 2. 启动遥测，收到了不用的 codex 参数时记一条 Debug 日志。
	tel, shutdown, err := newTelemetry(ctx, args.Config, args.Stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, shutdown(context.WithoutCancel(ctx))) }()
	if len(args.IgnoredCodexFlags) > 0 {
		tel.GetLogger(ctx).Debug("ignored codex app-server flags", "flags", args.IgnoredCodexFlags)
	}
	// 3. 准备 AppServer 选项：每条 Codex 连接各自选择 serve，本机 serve 来自别的可执行文件时提示写到 stderr。
	opt := appserver.NewAppServerOpt(&runtimeServerLocatorImpl{
		args:         args,
		noticeOutput: args.Stderr,
	}, args.Config.Run.AgentName)
	opt.Version = defs.GetBuildVersion()
	opt.ConfigDirectory = filepath.Dir(args.ConfigPath)
	opt.Telemetry = tel
	opt.PathMappings = args.Config.Client.PathMappings
	// 4. 按 stdio、Unix socket 或回环 WebSocket 配置传输；socket 和 WebSocket 把 codex --remote 命令写到 stderr。
	switch listen.Kind {
	case appServerListenStdio:
		input, ok := args.Stdin.(io.ReadCloser)
		if !ok {
			return errors.New("app-server stdin must be closable")
		}
		output, ok := args.Stdout.(io.WriteCloser)
		if !ok {
			return errors.New("app-server stdout must be closable")
		}
		if input, err = getInterruptibleInput(input); err != nil {
			return err
		}
		opt.WithStdio(input, output)
	case appServerListenUnix:
		listener, err := listenAppServerSocket(listen.SocketPath)
		if err != nil {
			return err
		}
		// 返回时关闭监听器并删除 socket 文件，创建 AppServer 失败时也一样。
		defer func() { _ = listener.Close() }()
		showCodexRemote(ctx, tel, args.Stderr, "unix://"+listen.SocketPath)
		opt.WithListener(listener)
	case appServerListenWebSocket:
		// 端口为 0 时由系统分配；回环 WebSocket 不做认证。
		listener, err := net.Listen("tcp", listen.WebSocketAddress.String())
		if err != nil {
			return fmt.Errorf("listen on %s: %w", listen.WebSocketAddress, err)
		}
		defer func() { _ = listener.Close() }()
		showCodexRemote(ctx, tel, args.Stderr, "ws://"+listener.Addr().String())
		opt.WithListener(listener)
	}
	// 5. 创建并运行 AppServer；被取消时正常返回。
	appServer, err := appserver.NewAppServer(opt)
	if err != nil {
		return err
	}
	err = appServer.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// showCodexRemote 向 stderr 写出连接 app-server 的 codex --remote 命令。
func showCodexRemote(ctx context.Context, tel *telemetry.Telemetry, stderr io.Writer, remoteURL string) {
	// 1. 写出命令，写入失败时记录警告。
	if _, err := fmt.Fprintf(stderr, "codex --remote %s\n", remoteURL); err != nil {
		tel.GetLogger(ctx).Warn("cannot show the codex command", "error", err)
	}
}

// appServerListen 是 app-server 对 Codex 客户端的监听方式，由 getAppServerListen 从 --listen、--stdio 解析而来。
type appServerListen struct {
	// Kind 是 stdio、Unix socket 或回环 WebSocket。
	Kind appServerListenKind
	// SocketPath 是 Unix socket 的绝对路径，只在 Kind 为 appServerListenUnix 时有值。
	SocketPath string
	// WebSocketAddress 是回环 IP 和端口（可以是 0，由系统选），只在 Kind 为 appServerListenWebSocket 时有值。
	WebSocketAddress netip.AddrPort
}

// appServerListenKind 是 app-server 的监听种类。
type appServerListenKind int

const (
	// appServerListenStdio 从 stdin 读、向 stdout 写，只服务一条连接。
	appServerListenStdio appServerListenKind = iota
	// appServerListenUnix 在 Unix socket 上监听，socket 文件权限为 0600。
	appServerListenUnix
	// appServerListenWebSocket 在回环 IP 的 TCP 端口上监听 WebSocket。
	appServerListenWebSocket
)

// getAppServerListen 从 --listen、--stdio 解析监听方式。
func getAppServerListen(args *AppArgs) (appServerListen, error) {
	// 1. 拒绝 WebSocket 认证参数和互相冲突的监听参数。
	if len(args.UnsupportedWebSocketFlags) > 0 {
		return appServerListen{}, fmt.Errorf("%s is not supported: app-server only listens on loopback, and the runtime server checks the token", args.UnsupportedWebSocketFlags[0])
	}
	listen := args.AppServerListen
	if args.IsAppServerStdio && listen != "" && listen != "stdio://" {
		return appServerListen{}, errors.New("--stdio conflicts with --listen")
	}
	// 2. 解析 stdio、Unix socket 路径或回环 IP 和端口；--listen off 和其他写法返回错误。
	switch {
	case listen == "" || listen == "stdio://":
		return appServerListen{Kind: appServerListenStdio}, nil
	case listen == "off":
		return appServerListen{}, errors.New("--listen off is not supported")
	case strings.HasPrefix(listen, "unix://"):
		// 路径为空时用连接文件同目录的 app-server.sock，相对路径按当前目录展开。
		path := strings.TrimPrefix(listen, "unix://")
		if path == "" {
			serverFilePath, err := getRuntimeServerFilePath()
			if err != nil {
				return appServerListen{}, err
			}
			return appServerListen{
				Kind:       appServerListenUnix,
				SocketPath: filepath.Join(filepath.Dir(serverFilePath), "app-server.sock"),
			}, nil
		}
		path, err := filepath.Abs(path)
		return appServerListen{
			Kind:       appServerListenUnix,
			SocketPath: path,
		}, err
	case strings.HasPrefix(listen, "ws://"):
		// 必须是 IP:端口，IP 必须是回环地址；端口为 0 时由系统分配。
		address, err := netip.ParseAddrPort(strings.TrimPrefix(listen, "ws://"))
		if err != nil {
			return appServerListen{}, errors.New("--listen ws:// needs IP:PORT, for example ws://127.0.0.1:4500")
		}
		if !address.Addr().IsLoopback() {
			return appServerListen{}, errors.New("app-server only listens on loopback; use client.url to reach a remote runtime server")
		}
		return appServerListen{
			Kind:             appServerListenWebSocket,
			WebSocketAddress: address,
		}, nil
	default:
		return appServerListen{}, fmt.Errorf("--listen %q is not supported; use stdio://, unix://, unix://PATH or ws://IP:PORT", listen)
	}
}

// runtimeServerLocatorImpl 为 AppServer 的每条 Codex 连接选择要连的 serve。
type runtimeServerLocatorImpl struct {
	// args 是选择 serve 用的命令参数和配置。
	args *AppArgs
	// noticeOutput 接收本机 serve 来自另一个可执行文件的提示；为 nil 时不输出。
	noticeOutput io.Writer
}

// GetRuntimeServer 选择 serve，返回地址、令牌、来源和本机 server_id。
func (l *runtimeServerLocatorImpl) GetRuntimeServer(ctx context.Context) (appserver.RuntimeServerInfo, error) {
	// 1. 选择本次连接使用的 serve。
	connection, err := getRuntimeConnection(ctx, l.args, runtimeServerIdleTimeout)
	if err != nil {
		return appserver.RuntimeServerInfo{}, err
	}
	// 2. noticeOutput 不为 nil 时写出可执行文件变更提示，再返回连接信息。
	if l.noticeOutput != nil {
		writeRuntimeServerNotice(ctx, l.noticeOutput, connection)
	}
	return appserver.RuntimeServerInfo{
		URL:           connection.URL,
		Token:         connection.Token,
		Source:        string(connection.Source),
		LocalServerID: connection.LocalServerID,
	}, nil
}

// listenAppServerSocket 清理残留的 socket 文件，创建只有当前用户能连的 Unix 监听器。
func listenAppServerSocket(path string) (*net.UnixListener, error) {
	// 1. 以 0700 创建 socket 所在目录。
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create app-server socket directory: %w", err)
	}
	// 2. 路径已存在时试连一次：连得上返回已被占用的错误，连不上就删掉残留文件。
	if _, err := os.Lstat(path); err == nil {
		if connection, err := net.DialTimeout("unix", path, appServerSocketProbeTimeout); err == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("app-server is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale app-server socket: %w", err)
		}
	}
	// 3. 开始监听，设为关闭时删除 socket 文件，权限改成 0600；改权限失败时关闭监听器。
	listener, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: path,
		Net:  "unix",
	})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	listener.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("restrict app-server socket permissions: %w", err)
	}
	return listener, nil
}
