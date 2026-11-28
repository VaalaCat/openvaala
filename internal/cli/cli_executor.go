package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/spf13/cobra"
)

func newExecutorCommand() *cobra.Command {
	command := &cobra.Command{Use: "executor", Short: "List execution environments"}
	addRuntimeCommandFlags(command.PersistentFlags())
	var refreshTools bool
	list := &cobra.Command{
		Use: "ls", Args: cobra.NoArgs,
		RunE: BuildCLIHandler(func(ctx context.Context, app *AppArgs) error {
			return runRuntimeCommand(ctx, app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
				result, err := client.ListConversationExecutors(ctx, &runtimeprotocol.ListExecutorsParams{
					ConversationID: conn.ConversationID, RefreshTools: refreshTools,
				})
				if err != nil {
					return err
				}
				for _, info := range result.Executors {
					label := info.ID
					if info.ID == result.DefaultExecutorID {
						label += " (default)"
					}
					fmt.Fprintln(app.Stdout, label, info.Kind, info.WorkspaceDirectory, info.Description)
				}
				return nil
			})
		}),
	}
	list.Flags().String("conversation", "", "conversation ID")
	list.Flags().BoolVar(&refreshTools, "refresh-tools", false, "accepted for parity with list_executors (no active model turn)")
	command.AddCommand(list, newExecutorToolCommand())
	return command
}

func newExecutorToolCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "tool <name> [arguments-json]", Args: cobra.RangeArgs(1, 2),
		RunE: buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
			params := runtimeprotocol.CallExecutorToolParams{Name: args[0]}
			if len(args) == 2 {
				if !json.Valid([]byte(args[1])) {
					return errors.New("tool arguments must be JSON")
				}
				params.Arguments = json.RawMessage(args[1])
			}
			return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
				params.CollaborationSource = collaborationSource(conn)
				result, err := client.CallExecutorTool(ctx, &params)
				if err != nil {
					return err
				}
				if result.Error != nil {
					return errors.New(result.Error.Message)
				}
				if len(result.Images) > 0 {
					return json.NewEncoder(app.Stdout).Encode(result)
				}
				_, err = fmt.Fprintln(app.Stdout, string(result.Content))
				return err
			})
		}),
	}
	command.Flags().String("conversation", "", "conversation ID")
	return command
}

func newExecCommand() *cobra.Command {
	var executorID, cwd string
	command := &cobra.Command{
		Use: "exec --executor <id> -- <command> [args...]", Short: "Run a command on an executor",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 || len(args) == 0 {
				return errors.New("exec requires a command after --")
			}
			return nil
		},
		RunE: buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
			return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
				result, err := client.Exec(ctx, &runtime.ExecutorExecRequest{
					ConversationID: conn.ConversationID, ExecutorID: executorID, Cwd: cwd,
					Command: args[0], Args: args[1:],
				})
				if err != nil {
					return err
				}
				_, stdoutErr := io.WriteString(app.Stdout, result.Stdout)
				_, stderrErr := io.WriteString(app.Stderr, result.Stderr)
				if result.ExitCode != 0 {
					return fmt.Errorf("command exited with status %d", result.ExitCode)
				}
				return errors.Join(stdoutErr, stderrErr)
			})
		}),
	}
	flags := command.Flags()
	addRuntimeCommandFlags(flags)
	flags.StringVar(&executorID, "executor", "", "executor ID")
	flags.StringVar(&cwd, "cwd", "", "working directory")
	flags.String("conversation", "", "conversation ID")
	_ = command.MarkFlagRequired("executor")
	return command
}
