// Package cli 定义 openvaala 命令行：加载配置，运行 TUI、serve、web、acp、app-server、codex、reindex、plugin 和 config 命令。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/spf13/cobra"
)

// CLIOpt 是命令树使用的标准输出和标准错误。
type CLIOpt struct {
	Stdout io.Writer
	Stderr io.Writer
}

// NewCLI 创建命令树，注册参数和子命令。
func NewCLI(opt *CLIOpt) *cobra.Command {
	// 1. 创建根命令：不收位置参数，运行任何命令前检查 -c/--config 的值，直接运行时进入 TUI；再设置输出流和参数错误提示。
	flags := &CLIFlags{}
	root := &cobra.Command{
		Use:               "openvaala",
		Short:             "OpenVaala terminal UI and runtime server",
		Version:           defs.GetBuildVersion(),
		SilenceErrors:     true,
		SilenceUsage:      true,
		Args:              cobra.NoArgs,
		PersistentPreRunE: checkCodexConfigOverrides,
		RunE:              BuildCLIHandler(runTUICommand),
	}
	root.SetOut(opt.Stdout)
	root.SetErr(opt.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error {
		return fmt.Errorf("invalid flags; use --help for usage")
	})
	// 2. 注册配置、Codex 和连接参数。
	bindings := root.PersistentFlags()
	bindings.StringVar(&flags.ConfigFilePath, "config-file", "", "configuration file (OPENVAALA_CONFIG or ~/.config/openvaala/config.yaml)")
	bindings.StringVar(&flags.AgentsDirectory, "agents-dir", "", "Agent definition directory")
	bindings.StringVar(&flags.ModelsDirectory, "models-dir", "", "Provider definition directory")
	bindings.StringVar(&flags.AgentName, "agent", "", "Agent definition name")
	addCodexRootFlags(bindings)
	addURLFlag(root.Flags())
	_ = root.MarkPersistentFlagFilename("config-file", "yaml", "yml")
	_ = root.MarkPersistentFlagDirname("agents-dir")
	_ = root.MarkPersistentFlagDirname("models-dir")

	// 3. 注册产品子命令。
	root.AddCommand(
		newConfigCommand(),
		newTUICommand(),
		newServeCommand(),
		newWebCommand(),
		newACPCommand(),
		newAppServerCommand(),
		newCodexCommand(),
		newReindexCommand(),
		newPluginCommand(),
	)
	return root
}

// LoadConfig 按命令的 --config-file 等参数加载配置，返回配置和加载用的选项。
func LoadConfig(command *cobra.Command) (*config.Config, *config.LoadConfigOpt, error) {
	// 1. 读取配置参数，加载配置。
	opt, err := getConfigOpt(command)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadConfig(opt)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config: %w", err)
	}
	return cfg, opt, nil
}

// BuildCLIHandler 包装 handler 成 RunE：先加载配置、收集命令参数，再调用 handler。
func BuildCLIHandler(handler func(context.Context, *AppArgs) error) func(*cobra.Command, []string) error {
	// 1. 返回加载配置、收集参数后调用 handler 的 RunE。
	return func(command *cobra.Command, arguments []string) error {
		// 1. 加载配置。
		cfg, opt, err := LoadConfig(command)
		if err != nil {
			return err
		}
		// 2. 收集标准流、配置路径和自动启动 serve 使用的参数。
		args := &AppArgs{
			Config:         cfg,
			ConfigOpt:      opt,
			Stdin:          command.InOrStdin(),
			Stdout:         command.OutOrStdout(),
			Stderr:         command.ErrOrStderr(),
			CodexArguments: arguments,
		}
		if args.ConfigFlags, err = GetConfigFlags(command.Flags()); err != nil {
			return err
		}
		if args.ConfigPath, _, err = config.GetConfigPath("", opt.Path); err != nil {
			return err
		}
		// 取不到可执行文件路径时记录警告并留空，命令照常执行。
		if args.Executable, err = os.Executable(); err != nil {
			slog.Warn("cannot locate the openvaala executable; clients cannot start a runtime server", "error", err)
		}
		// 3. 读取连接、Codex 和空闲退出参数。
		if flag := command.Flags().Lookup("url"); flag != nil {
			args.RuntimeURL = flag.Value.String()
		}
		if err = readCodexFlags(command.Flags(), args); err != nil {
			return err
		}
		if command.Flags().Lookup("idle-timeout") != nil {
			if args.IdleTimeout, err = command.Flags().GetDuration("idle-timeout"); err != nil {
				return err
			}
		}
		// 4. 执行业务处理。
		return handler(command.Context(), args)
	}
}

// checkCodexConfigOverrides 检查每个 -c/--config 都是 key=value 写法。
func checkCodexConfigOverrides(command *cobra.Command, _ []string) error {
	// 1. 读取全部 -c/--config，有一项不含等号就报错，提示 OpenVaala 的配置文件用 --config-file。
	overrides, err := command.Flags().GetStringArray("config")
	if err != nil {
		return err
	}
	for _, override := range overrides {
		if !strings.Contains(override, "=") {
			return errors.New("--config takes key=value (a Codex configuration override); use --config-file for the OpenVaala configuration file")
		}
	}
	return nil
}

// getConfigOpt 读取 --config-file，生成加载配置用的选项。
func getConfigOpt(command *cobra.Command) (*config.LoadConfigOpt, error) {
	// 1. 读取 --config-file，和命令的参数集一起放进配置加载选项。
	path, err := command.Flags().GetString("config-file")
	if err != nil {
		return nil, err
	}
	return &config.LoadConfigOpt{
		Path:  path,
		Flags: command.Flags(),
	}, nil
}
