package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/VaalaCat/openvaala/internal/cli"
)

var version = "dev"

// main 接收退出信号，执行命令并输出错误。
func main() {
	// 1. 准备接收中断信号的命令。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	command := cli.NewCLI(&cli.CLIOpt{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
	})
	// 2. 执行命令，停止信号监听并处理执行错误。
	err := command.ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
