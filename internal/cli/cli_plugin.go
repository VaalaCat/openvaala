package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/spf13/cobra"
)

// pluginChangesPendingText 是 plugin 命令改完插件、发现本机没有 serve 在运行时输出的提示。
const pluginChangesPendingText = "no runtime server is running; changes apply when it starts\n"

// newPluginCommand 创建 plugin 命令和它的 install、update、uninstall、enable、disable、list 子命令。
func newPluginCommand() *cobra.Command {
	// 1. 创建 plugin 命令，注册各子命令和 install 的 --path、--ref 参数。
	command := &cobra.Command{
		Use:   "plugin",
		Short: "Install and manage plugins that bring skills, hooks and MCP servers",
		Args:  cobra.NoArgs,
	}
	install := &cobra.Command{
		Use:   "install <source>",
		Short: "Install a plugin from a git URL or a local directory",
		Args:  cobra.ExactArgs(1),
		RunE:  runPluginInstall,
	}
	install.Flags().String("path", "", "plugin directory inside the repository (default: the repository root)")
	install.Flags().String("ref", "", "git branch or tag to install (default: the default branch)")
	command.AddCommand(
		install,
		&cobra.Command{
			Use:   "update [name...]",
			Short: "Update plugins installed by plugin install (default: all of them)",
			RunE:  runPluginUpdate,
		},
		&cobra.Command{
			Use:   "uninstall <name>",
			Short: "Remove a plugin",
			Args:  cobra.ExactArgs(1),
			RunE:  runPluginUninstall,
		},
		&cobra.Command{
			Use:   "enable <name>",
			Short: "Enable a disabled plugin",
			Args:  cobra.ExactArgs(1),
			RunE:  runPluginEnable,
		},
		&cobra.Command{
			Use:   "disable <name>",
			Short: "Disable a plugin without removing it",
			Args:  cobra.ExactArgs(1),
			RunE:  runPluginDisable,
		},
		&cobra.Command{
			Use:   "list",
			Short: "List the plugins in the plugins directory",
			Args:  cobra.NoArgs,
			RunE:  runPluginList,
		},
	)
	return command
}

// runPluginInstall 安装插件，再让本机 serve 重新加载配置。
func runPluginInstall(command *cobra.Command, arguments []string) error {
	// 1. 读取配置路径和安装参数。
	args, err := newPluginAppArgs(command)
	if err != nil {
		return err
	}
	request := &config.InstallPluginRequest{Source: arguments[0]}
	if request.Path, err = command.Flags().GetString("path"); err != nil {
		return err
	}
	if request.Ref, err = command.Flags().GetString("ref"); err != nil {
		return err
	}
	// 2. 安装插件并输出名称；插件已禁用时输出启用提示。
	name, isDisabled, err := config.InstallPlugin(command.Context(), args.ConfigOpt, request)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("installed plugin %s\n", name)
	if isDisabled {
		text += fmt.Sprintf("plugin %s is disabled; run openvaala plugin enable %s\n", name, name)
	}
	if err := writeText(args.Stdout, text); err != nil {
		return err
	}
	// 3. 让本机 serve 重新加载配置；没有 serve 在运行时提示启动后生效。
	return runServeReloadCommand(command.Context(), args, pluginChangesPendingText)
}

// runPluginUpdate 更新插件，再让本机 serve 重新加载配置。
func runPluginUpdate(command *cobra.Command, arguments []string) error {
	// 1. 读取配置选项，更新指定的插件，没指定时更新全部由 plugin install 安装的插件。
	args, err := newPluginAppArgs(command)
	if err != nil {
		return err
	}
	updatedNames, updateErr := config.UpdatePlugins(command.Context(), args.ConfigOpt, arguments)
	// 2. 一个都没更新成功时返回更新错误，没有错误就输出没有可更新的插件。
	if len(updatedNames) == 0 {
		if updateErr != nil {
			return updateErr
		}
		return writeText(args.Stdout, "no plugins to update\n")
	}
	// 3. 输出更新成功的插件，让本机 serve 重新加载一次配置，合并返回更新和重新加载的错误。
	var text strings.Builder
	for _, name := range updatedNames {
		fmt.Fprintf(&text, "updated plugin %s\n", name)
	}
	if err := writeText(args.Stdout, text.String()); err != nil {
		return errors.Join(updateErr, err)
	}
	return errors.Join(updateErr, runServeReloadCommand(command.Context(), args, pluginChangesPendingText))
}

// runPluginUninstall 删除插件，再让本机 serve 重新加载配置。
func runPluginUninstall(command *cobra.Command, arguments []string) error {
	// 1. 读取配置选项并删除指定插件。
	args, err := newPluginAppArgs(command)
	if err != nil {
		return err
	}
	if err := config.UninstallPlugin(args.ConfigOpt, arguments[0]); err != nil {
		return err
	}
	// 2. 输出删除结果，让本机 serve 重新加载配置。
	if err := writeText(args.Stdout, fmt.Sprintf("uninstalled plugin %s\n", arguments[0])); err != nil {
		return err
	}
	return runServeReloadCommand(command.Context(), args, pluginChangesPendingText)
}

// runPluginEnable 启用指定插件。
func runPluginEnable(command *cobra.Command, arguments []string) error {
	// 1. 将指定插件设为启用。
	return setPluginDisabled(command, arguments[0], false)
}

// runPluginDisable 禁用指定插件。
func runPluginDisable(command *cobra.Command, arguments []string) error {
	// 1. 将指定插件设为禁用，保留插件文件。
	return setPluginDisabled(command, arguments[0], true)
}

// setPluginDisabled 启用或禁用插件，状态变了时让本机 serve 重新加载配置。
func setPluginDisabled(command *cobra.Command, name string, isDisabled bool) error {
	// 1. 读取配置选项并修改插件状态。
	args, err := newPluginAppArgs(command)
	if err != nil {
		return err
	}
	state := "enabled"
	if isDisabled {
		state = "disabled"
	}
	isChanged, err := config.SetPluginDisabled(args.ConfigOpt, name, isDisabled)
	if err != nil {
		return err
	}
	// 2. 状态没变时输出已经是这个状态；变了时输出结果，让本机 serve 重新加载配置。
	if !isChanged {
		return writeText(args.Stdout, fmt.Sprintf("plugin %s is already %s\n", name, state))
	}
	if err := writeText(args.Stdout, fmt.Sprintf("%s plugin %s\n", state, name)); err != nil {
		return err
	}
	return runServeReloadCommand(command.Context(), args, pluginChangesPendingText)
}

// runPluginList 输出本机插件列表。
func runPluginList(command *cobra.Command, _ []string) error {
	// 1. 读取插件列表。
	args, err := newPluginAppArgs(command)
	if err != nil {
		return err
	}
	entries, err := config.ListPlugins(args.ConfigOpt)
	if err != nil {
		return err
	}
	// 2. 每个插件一行：名字、版本（没有时为 -）、启用状态、来源（不是 plugin install 安装的写 (manual)）和读取错误。
	table := tabwriter.NewWriter(args.Stdout, 0, 0, 2, ' ', 0)
	for _, entry := range entries {
		fields := []string{entry.Name, "-", "enabled", "(manual)"}
		if entry.Version != "" {
			fields[1] = entry.Version
		}
		if entry.IsDisabled {
			fields[2] = "disabled"
		}
		if entry.Record != nil {
			fields[3] = getPluginSourceText(entry.Record)
		}
		if entry.Error != "" {
			fields = append(fields, entry.Error)
		}
		if _, err := fmt.Fprintln(table, strings.Join(fields, "\t")); err != nil {
			return fmt.Errorf("cannot write output: %w", err)
		}
	}
	// 3. 刷新表格输出。
	if err := table.Flush(); err != nil {
		return fmt.Errorf("cannot write output: %w", err)
	}
	return nil
}

// newPluginAppArgs 为 plugin 命令准备 AppArgs：只有配置选项和输出流，不加载配置。
func newPluginAppArgs(command *cobra.Command) (*AppArgs, error) {
	// 1. 读取配置选项，带上标准输出和标准错误。
	opt, err := getConfigOpt(command)
	if err != nil {
		return nil, err
	}
	return &AppArgs{
		ConfigOpt: opt,
		Stdout:    command.OutOrStdout(),
		Stderr:    command.ErrOrStderr(),
	}, nil
}
