package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/spf13/cobra"
)

func newBridgeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "bridge",
		Short: "Connect an external World host over stdio JSON-RPC",
		Args:  cobra.NoArgs,
		RunE:  runBridgeCommand,
	}
}

func runBridgeCommand(command *cobra.Command, _ []string) (err error) {
	ctx := command.Context()
	cfg, _, err := LoadConfig(command)
	if err != nil {
		return err
	}

	input, output, closePipes, err := openBridgeStdio(command)
	if err != nil {
		return err
	}
	defer closePipes()

	tel, shutdown, err := newTelemetry(ctx, cfg, command.ErrOrStderr())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, shutdown(context.WithoutCancel(ctx))) }()

	directory := filepath.Join(cfg.GetDataDirectory(), "world")
	identity, err := world.LoadOrCreateIdentity(filepath.Join(directory, "identity.json"))
	if err != nil {
		return err
	}
	database, err := sqlite.OpenDatabase(sqlite.NewDatabaseOpt(filepath.Join(directory, "world.db"), world.WorldNamespaces()...))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, database.Close()) }()

	nodeCfg := cfg.WorldNodeWithDefaults()
	nodeOpt := world.NewWorldNodeOpt(database.Record(), identity, nodeCfg.Name, nodeCfg.Domain, nodeCfg.ServerURL)
	nodeOpt.DefaultTarget, nodeOpt.AdvertiseTargets = nodeCfg.DefaultTarget, nodeCfg.AdvertiseTargets
	nodeOpt.Relays = cfg.World.Relays
	nodeOpt.Telemetry = tel
	err = world.RunBridge(ctx, nodeOpt, input, output)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// openBridgeStdio 为桥接命令准备可关闭的标准流，返回独立句柄的清理函数。
func openBridgeStdio(command *cobra.Command) (io.ReadCloser, io.WriteCloser, func(), error) {
	input, ok := command.InOrStdin().(io.ReadCloser)
	if !ok {
		return nil, nil, nil, errors.New("bridge stdin must be closable")
	}
	output, ok := command.OutOrStdout().(io.WriteCloser)
	if !ok {
		return nil, nil, nil, errors.New("bridge stdout must be closable")
	}
	stdin, err := openBridgePipe(input)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("prepare bridge stdin: %w", err)
	}
	stdout, err := openBridgePipe(output)
	if err != nil {
		if stdin != nil {
			stdin.Close()
		}
		return nil, nil, nil, fmt.Errorf("prepare bridge stdout: %w", err)
	}
	if stdin != nil {
		input = stdin
	}
	if stdout != nil {
		output = stdout
	}
	return input, output, func() {
		if stdin != nil {
			stdin.Close()
		}
		if stdout != nil {
			stdout.Close()
		}
	}, nil
}

// openBridgePipe 给继承的管道和 socket 创建可由 Close 中断的独立文件句柄。
func openBridgePipe(stream io.Closer) (*os.File, error) {
	file, ok := stream.(*os.File)
	if !ok {
		return nil, nil
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode()&(os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice) == 0 {
		return nil, nil
	}
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}
