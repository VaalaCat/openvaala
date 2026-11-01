package cli

import (
	"context"
	"fmt"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newConfigCommand 创建 config 命令和它的 init、show、check 子命令。
func newConfigCommand() *cobra.Command {
	// 1. 注册 config 命令和三个子命令。
	command := &cobra.Command{
		Use:   "config",
		Short: "Initialize, inspect and validate configuration",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(
		&cobra.Command{
			Use:   "init",
			Short: "Create private configuration without overwriting existing files",
			Args:  cobra.NoArgs,
			RunE:  initializeConfig,
		},
		&cobra.Command{
			Use:   "show",
			Short: "Show effective application settings",
			Args:  cobra.NoArgs,
			RunE:  BuildCLIHandler(showConfig),
		},
		&cobra.Command{
			Use:   "check",
			Short: "Validate configuration and the selected Agent",
			Args:  cobra.NoArgs,
			RunE:  BuildCLIHandler(checkConfig),
		},
	)
	return command
}

// initializeConfig 创建配置文件（已有的不覆盖），输出它的路径。
func initializeConfig(command *cobra.Command, _ []string) error {
	// 1. 读取配置选项，创建配置文件。
	opt, err := getConfigOpt(command)
	if err != nil {
		return err
	}
	path, err := config.InitConfig(opt)
	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}
	// 2. 输出配置文件路径。
	return writeText(command.OutOrStdout(), path+"\n")
}

// showConfig 以 YAML 输出已加载的配置。
func showConfig(_ context.Context, args *AppArgs) error {
	// 1. 编码配置并写入命令输出流。
	payload, err := yaml.Marshal(args.Config)
	if err != nil {
		return fmt.Errorf("cannot encode configuration: %w", err)
	}
	return writeText(args.Stdout, string(payload))
}

// checkConfig 检查选中的 Agent 并输出模型信息。
func checkConfig(_ context.Context, args *AppArgs) error {
	// 1. 查询选中的 Agent。
	definition, err := args.Config.GetAgent(args.Config.Run.AgentName)
	if err != nil {
		return fmt.Errorf("failed to get agent: %w", err)
	}
	// 2. 输出 Agent 和模型；未指定模型时输出 none。
	if definition.Model == nil {
		return writeText(args.Stdout, fmt.Sprintf("configuration valid: agent=%s model=(none)\n", definition.Name))
	}
	return writeText(args.Stdout, fmt.Sprintf(
		"configuration valid: agent=%s provider=%s model=%s\n",
		definition.Name, definition.Model.Provider, definition.Model.Model,
	))
}
