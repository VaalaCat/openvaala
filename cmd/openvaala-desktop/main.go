package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/VaalaCat/openvaala/internal/desktop"
	"github.com/spf13/cobra"
)

// desktopCommandFlags 保存桌面入口选择的界面模式。
type desktopCommandFlags struct {
	Native bool
}

// main 接收退出信号，执行完整命令树并输出退出错误。
func main() {
	// 1. 准备命令执行上下文，不查询图形后端。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	command := newDesktopCommand()
	// 2. 在主 goroutine 执行所选命令，结束后释放信号监听。
	err := command.ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newDesktopCommand 组装完整 CLI 命令树，注册默认 WebView 和原生界面选项。
func newDesktopCommand() *cobra.Command {
	// 1. 保留原命令树，将无参数入口设为桌面模式选择。
	flags := &desktopCommandFlags{}
	command := cli.NewCLI(&cli.CLIOpt{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	command.Short = "OpenVaala desktop, terminal UI and runtime server"
	command.RunE = flags.runAppCommand
	command.Flags().BoolVar(&flags.Native, "native", false, "use the native interface instead of the WebView")
	// 2. 注册 app 命令及其界面选项，其他命令继续使用原处理函数。
	appCommand := &cobra.Command{
		Use:   "app",
		Short: "Open the Web UI in a desktop window (or use --native)",
		Args:  cobra.NoArgs,
		RunE:  flags.runAppCommand,
	}
	appCommand.Flags().BoolVar(&flags.Native, "native", false, "use the native interface instead of the WebView")
	cli.AddURLFlag(appCommand.Flags())
	command.AddCommand(appCommand)
	return command
}

// runAppCommand 收集服务启动参数与可选远端目标，在主 goroutine 运行所选桌面界面。
func (f *desktopCommandFlags) runAppCommand(command *cobra.Command, _ []string) error {
	// 1. 读取当前程序、原配置参数以及 --url / client.url 指定的远端目标。
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate desktop executable: %w", err)
	}
	configFlags, err := cli.GetConfigFlags(command.Flags())
	if err != nil {
		return err
	}
	startTarget, cfg := cli.ResolveDesktopStartTarget(command)
	// 2. 两种界面均支持远端目标，未指定时复用或启动常驻的本机 serve。
	runtimeServerStart := cli.RuntimeServerStartOpt{
		Config:      cfg,
		Executable:  executable,
		ConfigFlags: configFlags,
		IdleTimeout: 0,
	}
	if f.Native {
		var headers map[string]string
		if cfg != nil {
			headers = cfg.GetHTTPHeaders()
		}
		app, err := desktop.NewDesktop(&desktop.DesktopOpt{
			RuntimeServerStart: runtimeServerStart,
			StartTarget:        startTarget,
			HTTPHeaders:        headers,
		})
		if err != nil {
			return err
		}
		return app.Run(command.Context())
	}
	app, err := desktop.NewWebViewDesktop(&desktop.WebViewDesktopOpt{
		RuntimeServerStart: runtimeServerStart,
		StartTarget:        startTarget,
	})
	if err != nil {
		return err
	}
	return app.Run(command.Context())
}
