package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"

	"github.com/VaalaCat/openvaala/internal/appserver"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/spf13/cobra"
)

// codexResumeHint 是 codex 正常退出后写到 stderr 的提示：本进程退出后 socket 被删掉，要用 openvaala codex -- resume 恢复对话。
const codexResumeHint = "The codex --remote commands above stop working once openvaala codex exits. To reconnect, run:\n  openvaala codex -- resume <session id>"

// newCodexCommand 创建 codex 命令（启动 Codex TUI）和它的 app 子命令（启动 Codex 桌面应用）。
func newCodexCommand() *cobra.Command {
	// 1. 注册 codex 命令和 app 子命令。
	command := &cobra.Command{
		Use:   "codex [-- CODEX_ARGS...]",
		Short: "Open the Codex TUI or desktop app connected to OpenVaala",
		Long: `Open the Codex TUI connected to OpenVaala, or use "codex app" for the macOS desktop app.

For the TUI, arguments after -- go to codex. The codex options -c/--config key=value, --enable, --disable and --strict-config are passed on to codex too,
wherever they appear before --.`,
		Args: cobra.ArbitraryArgs,
		RunE: BuildCLIHandler(runCodexCommand),
	}
	addURLFlag(command.Flags())
	app := &cobra.Command{
		Use:   "app [directory]",
		Short: "Open the installed Codex desktop app with OpenVaala as its backend (macOS only)",
		Long: `Open the installed Codex desktop app with OpenVaala as its backend (macOS only).

The directory defaults to the current directory. Quit any running Codex desktop app first.
The app starts OpenVaala's app-server itself; this command returns after launching it.
No Codex configuration is changed and the desktop app is not installed automatically.`,
		Args: cobra.MaximumNArgs(1),
		RunE: BuildCLIHandler(runCodexAppCommand),
	}
	addURLFlag(app.Flags())
	command.AddCommand(app)
	return command
}

// runCodexAppCommand 检查工作目录和已安装的 Codex 桌面应用，用 OpenVaala 做后端启动它。
func runCodexAppCommand(ctx context.Context, args *AppArgs) error {
	// 1. 检查是 macOS、可执行文件路径已知，工作目录（默认当前目录）存在且是目录。
	if runtime.GOOS != "darwin" {
		return errors.New("openvaala codex app is only supported on macOS")
	}
	if args.Executable == "" {
		return errors.New("cannot launch Codex desktop: the openvaala executable is unknown")
	}
	directory := "."
	if len(args.CodexArguments) > 0 {
		directory = args.CodexArguments[0]
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve workspace directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("inspect workspace directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace is not a directory: %s", directory)
	}

	// 2. 用 osascript 按 bundle ID 查询应用的安装位置和是否在运行。
	query := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", `
ObjC.import('AppKit');
function run() {
    const bundleID = 'com.openai.codex';
    const applicationURL = $.NSWorkspace.sharedWorkspace.URLForApplicationWithBundleIdentifier(bundleID);
    return JSON.stringify({
        path: applicationURL.isNil() ? null : ObjC.unwrap(applicationURL.path),
        running: $.NSRunningApplication.runningApplicationsWithBundleIdentifier(bundleID).count > 0
    });
}`)
	// 系统诊断写 stderr，stdout 接收 JSON 查询结果。
	query.Stderr = args.Stderr
	output, err := query.Output()
	if err != nil {
		return fmt.Errorf("locate Codex desktop: %w", err)
	}
	// 3. 应用已在运行、没有安装或安装位置不是目录时返回错误。
	var application struct {
		Path    string `json:"path"`
		Running bool   `json:"running"`
	}
	if err := json.Unmarshal(output, &application); err != nil {
		return fmt.Errorf("read Codex desktop location: %w", err)
	}
	if application.Running {
		return errors.New("Codex desktop is already running; quit it first, then run openvaala codex app again")
	}
	if application.Path == "" {
		return errors.New("Codex desktop is not installed; install it first, then run openvaala codex app again")
	}
	info, err = os.Stat(application.Path)
	if err != nil {
		return fmt.Errorf("inspect installed Codex desktop: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Codex desktop application is not a directory: %s", application.Path)
	}
	// 4. 启动桌面应用，open 命令返回后结束。
	command := newCodexAppLaunchCommand(ctx, args, application.Path, directory)
	command.Stdout, command.Stderr = args.Stdout, args.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("launch Codex desktop: %w", err)
	}
	return nil
}

// runCodexCommand 在本进程的 Unix socket 上运行 AppServer，再运行连接它的 Codex TUI。
func runCodexCommand(ctx context.Context, args *AppArgs) (err error) {
	// 1. 查找 codex 命令并读取当前目录。
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return fmt.Errorf("cannot run codex (install the Codex CLI first): %w", err)
	}
	directory, err := os.Getwd()
	if err != nil {
		return err
	}
	// 2. 收到 SIGHUP 时取消 ctx；启动只写日志文件和 OTLP 的遥测，放进 ctx 给后续日志使用。
	ctx, stopHangup := signal.NotifyContext(ctx, syscall.SIGHUP)
	defer stopHangup()
	tel, shutdown, err := newTelemetry(ctx, args.Config, args.Version, nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, shutdown(context.WithoutCancel(ctx))) }()
	ctx = telemetry.ContextWithTelemetry(ctx, tel)
	// 3. 选择 serve（没有显式地址时复用或拉起本机 serve，拉起失败返回错误），输出可执行文件变更提示。
	connection, err := getRuntimeConnection(ctx, args, runtimeServerIdleTimeout)
	if err != nil {
		return err
	}
	writeRuntimeServerNotice(ctx, args.Stderr, connection)
	// 4. 创建本进程专用的 codex-<pid>.sock 监听器。
	serverFilePath, err := getRuntimeServerFilePath()
	if err != nil {
		return err
	}
	socketPath := filepath.Join(filepath.Dir(serverFilePath), fmt.Sprintf("codex-%d.sock", os.Getpid()))
	listener, err := listenAppServerSocket(socketPath)
	if err != nil {
		return err
	}
	// 返回时关闭监听器并删除 socket，创建 AppServer 失败时也一样。
	defer func() { _ = listener.Close() }()
	// 5. 创建并运行 AppServer；Codex 退出后停止并等待它。
	opt := appserver.NewAppServerOpt(&runtimeServerLocatorImpl{args: args}, args.Config.Run.AgentName)
	opt.Version = args.Version
	opt.ConfigDirectory = filepath.Dir(args.ConfigPath)
	opt.Telemetry = tel
	opt.PathMappings = args.Config.Client.PathMappings
	opt.WithListener(listener)
	appServer, err := appserver.NewAppServer(opt)
	if err != nil {
		return err
	}
	stopAppServer := startDetachedRun(ctx, appServer.Run)
	defer func() { err = errors.Join(err, stopAppServer()) }()

	// 6. 用 --remote 指向 socket、-C 传入当前目录，接上根参数和 -- 之后的参数，把终端交给 codex。
	codexArguments := slices.Concat([]string{"--remote", "unix://" + socketPath, "-C", directory}, args.CodexRootArguments, args.CodexArguments)
	command := exec.CommandContext(ctx, codexPath, codexArguments...)
	command.Stdin, command.Stdout, command.Stderr = args.Stdin, args.Stdout, args.Stderr
	// ctx 取消时给 codex 发 SIGTERM，codex 和它的 node 包装进程随之退出，这时终端不做恢复。
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	codexErr := command.Run()
	// 7. 因退出信号结束时正常返回；codex 执行失败时返回错误，正常退出时输出重连提示。
	if ctx.Err() != nil {
		return nil
	}
	if codexErr != nil {
		return fmt.Errorf("codex: %w", codexErr)
	}
	if _, err := fmt.Fprintln(args.Stderr, codexResumeHint); err != nil {
		tel.GetLogger(ctx).Warn("cannot show the codex resume hint", "error", err)
	}
	return nil
}
