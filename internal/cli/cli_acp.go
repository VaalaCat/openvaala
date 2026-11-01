package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/spf13/cobra"
)

// newACPCommand 创建 acp 命令：通过 stdio 用 ACP 协议服务编辑器。
func newACPCommand() *cobra.Command {
	// 1. 注册 acp 命令和 --url 参数。
	command := &cobra.Command{
		Use:   "acp",
		Short: "Serve editors over ACP stdio",
		Args:  cobra.NoArgs,
		RunE:  BuildCLIHandler(runACPCommand),
	}
	addURLFlag(command.Flags())
	return command
}

// runACPCommand 选择 serve，运行 acpAgent 处理编辑器请求。
func runACPCommand(ctx context.Context, args *AppArgs) (err error) {
	// 1. 标准输入和输出都必须能关闭。
	input, ok := args.Stdin.(io.ReadCloser)
	if !ok {
		return errors.New("ACP stdin must be closable")
	}
	output, ok := args.Stdout.(io.WriteCloser)
	if !ok {
		return errors.New("ACP stdout must be closable")
	}
	// 2. 启动遥测（文本日志写 stderr）并放进 ctx，选择 serve，提示写 stderr；stdout 只写协议消息。
	tel, shutdown, err := newTelemetry(ctx, args.Config, args.Version, args.Stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, shutdown(context.WithoutCancel(ctx))) }()
	ctx = telemetry.ContextWithTelemetry(ctx, tel)
	server, err := getRuntimeConnection(ctx, args, runtimeServerIdleTimeout)
	if err != nil {
		return err
	}
	writeRuntimeServerNotice(ctx, args.Stderr, server)
	// 3. 把管道或 socket 输入改成可中断的，创建 acpAgent。
	if input, err = getInterruptibleInput(input); err != nil {
		return err
	}
	host := &acpAgent{
		config:    args.Config,
		server:    server,
		telemetry: tel,
		input:     input,
		output:    output,
		version:   args.Version,
	}
	// 4. 运行 acpAgent；输入结束（EOF）或被取消时正常返回。
	err = host.Run(ctx)
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// getInterruptibleInput 把管道和 socket 输入改成非阻塞文件，阻塞中的 Read 能被 Close 中断。
func getInterruptibleInput(input io.ReadCloser) (io.ReadCloser, error) {
	// 1. 不是文件，或不是管道和 socket 时原样返回。
	file, ok := input.(*os.File)
	if !ok {
		return input, nil
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect stdin: %w", err)
	}
	if info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return input, nil
	}
	// 2. 把描述符设为非阻塞，用同一个描述符新建文件对象返回。
	if err := syscall.SetNonblock(int(file.Fd()), true); err != nil {
		return nil, fmt.Errorf("prepare stdin: %w", err)
	}
	return os.NewFile(file.Fd(), file.Name()), nil
}
