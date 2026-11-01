package cli

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/internal/tui"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// errTUIRequiresTerminal 是标准输入不是交互终端时 runTUICommand 返回的错误。
var errTUIRequiresTerminal = errors.New("openvaala tui requires an interactive terminal")

// newTUICommand 创建 tui 命令，和不带子命令运行 openvaala 相同。
func newTUICommand() *cobra.Command {
	// 1. 注册 TUI 命令和连接地址参数。
	command := &cobra.Command{
		Use:   "tui",
		Short: "Start the terminal UI (same as running openvaala without arguments)",
		Args:  cobra.NoArgs,
		RunE:  BuildCLIHandler(runTUICommand),
	}
	addURLFlag(command.Flags())
	return command
}

// runTUICommand 检查终端输入，选择 serve 并运行 TUI。
func runTUICommand(ctx context.Context, args *AppArgs) (err error) {
	// 1. 检查标准输入是否为交互终端。
	if !isTerminal(args.Stdin) {
		return errTUIRequiresTerminal
	}
	// 2. 启动只写日志文件和 OTLP 的遥测，放进 ctx 给后续日志使用。
	tel, shutdownTelemetry, err := newTelemetry(ctx, args.Config, args.Version, nil)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, shutdownTelemetry(context.WithoutCancel(ctx)))
	}()
	ctx = telemetry.ContextWithTelemetry(ctx, tel)
	// 3. 读取当前目录，选择要连的 serve。
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	connection, err := getRuntimeConnection(ctx, args, runtimeServerIdleTimeout)
	if err != nil {
		return err
	}
	// 4. 当前目录命中 client.path_mappings 时换成 serve 路径，否则原样交给 TUI（TUI 只在连本机 serve 时使用它）；再运行 TUI。
	serverDirectory, isMapped := args.Config.Client.PathMappings.GetServerPath(directory)
	if !isMapped {
		serverDirectory = directory
	}
	return runTUIClient(ctx, args, tel, connection, serverDirectory, isMapped)
}

// runTUIClient 创建 Runtime Client 和 TUI，运行到界面退出，再停止并等待 Client。
func runTUIClient(ctx context.Context, args *AppArgs, tel *telemetry.Telemetry, connection runtimeConnection, workspaceDirectory string, isWorkspaceDirectoryMapped bool) error {
	// 1. 创建 Runtime Client。
	clientOpt := runtimeclient.NewClientOpt(connection.URL, connection.Token)
	clientOpt.Telemetry = tel
	client, err := runtimeclient.NewClient(clientOpt)
	if err != nil {
		return err
	}
	// 2. 用工作目录、连接信息和启动提示创建 TUI。
	opt := tui.NewTUIOpt(client, args.Config.Run.AgentName, workspaceDirectory, args.Stdin, args.Stdout)
	opt.IsWorkspaceDirectoryMapped = isWorkspaceDirectoryMapped
	opt.LocalServerID = connection.LocalServerID
	if connection.IsFromOtherExecutable {
		opt.StartupNotice = runtimeServerExecutableChangedNotice
	}
	opt.ServerURL = runtimeclient.RedactURL(connection.URL)
	opt.ServerSource = string(connection.Source)
	// 状态面板按 t 时显示带 token 的连接地址。
	opt.ConnectURL = getConnectURL(connection.URL, connection.Token)
	view, err := tui.NewTUI(opt)
	if err != nil {
		return err
	}
	// 3. 后台运行 Client，前台运行界面；Client 因认证或协议错误结束时界面跟着退出。界面退出后取消并等待 Client，被取消不算错误。
	clientCtx, stopClient := context.WithCancel(ctx)
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- client.Run(clientCtx)
	}()
	viewErr := view.Run(ctx)
	stopClient()
	clientErr := <-clientDone
	if errors.Is(clientErr, context.Canceled) {
		clientErr = nil
	}
	return errors.Join(viewErr, clientErr)
}

// getConnectURL 生成状态面板显示的带 token 的连接地址。
func getConnectURL(runtimeURL, token string) string {
	// 1. 解析 serve 地址，解析失败时返回空串。
	parsed, err := url.Parse(runtimeURL)
	if err != nil {
		return ""
	}
	// 2. URL 自带 token 时原样返回；没有时补上 token，token 也为空时返回空串。
	query := parsed.Query()
	if !query.Has("token") {
		if token == "" {
			return ""
		}
		query.Set("token", token)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

// isTerminal input 是不是交互终端？
func isTerminal(input io.Reader) bool {
	// 1. input 是文件且是终端时返回 true。
	file, ok := input.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}
