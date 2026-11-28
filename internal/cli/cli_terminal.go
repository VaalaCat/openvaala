package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/spf13/cobra"
)

func newTerminalCommand() *cobra.Command {
	command := &cobra.Command{Use: "terminal", Short: "Manage interactive terminals"}
	addRuntimeCommandFlags(command.PersistentFlags())
	command.AddCommand(newTerminalListCommand(), newTerminalOpenCommand(), newTerminalCloseCommand(), newTerminalSendCommand(), newTerminalCaptureCommand())
	return command
}

func newTerminalListCommand() *cobra.Command {
	return &cobra.Command{Use: "ls", Args: cobra.NoArgs, RunE: BuildCLIHandler(func(ctx context.Context, app *AppArgs) error {
		return runRuntimeCommand(ctx, app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
			if err := client.SubscribeTerminals(ctx); err != nil {
				return err
			}
			for {
				event, err := nextRuntimeEvent(ctx, client)
				if err != nil {
					return err
				}
				list, ok := event.(*runtimeclient.TerminalsChanged)
				if !ok {
					continue
				}
				if list.Err != nil {
					return list.Err
				}
				if list.IsKnown {
					for _, info := range list.Terminals {
						fmt.Fprintln(app.Stdout, info.TerminalID, info.ExecutorID, info.Directory, info.ConversationID)
					}
					return nil
				}
			}
		})
	})}
}

func newTerminalOpenCommand() *cobra.Command {
	var request runtimeclient.TerminalOpenRequest
	command := &cobra.Command{Use: "open", Args: cobra.NoArgs, RunE: BuildCLIHandler(func(ctx context.Context, app *AppArgs) error {
		return runRuntimeCommand(ctx, app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
			request.ConversationID = conn.ConversationID
			opened, err := client.OpenTerminal(ctx, &request)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(app.Stdout, opened.TerminalID)
			return err
		})
	})}
	flags := command.Flags()
	flags.StringVar(&request.ExecutorID, "executor", "", "executor ID")
	flags.StringVar(&request.Directory, "cwd", "", "working directory")
	flags.Uint16Var(&request.Rows, "rows", 0, "initial rows")
	flags.Uint16Var(&request.Cols, "cols", 0, "initial columns")
	return command
}

func newTerminalCloseCommand() *cobra.Command {
	return &cobra.Command{
		Use: "close <terminal-id>", Args: cobra.ExactArgs(1),
		RunE: buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
			return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
				return client.CloseTerminal(ctx, args[0])
			})
		}),
	}
}

func newTerminalSendCommand() *cobra.Command {
	return &cobra.Command{
		Use: "send <terminal-id> <keys>", Args: cobra.ExactArgs(2),
		RunE: buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
			return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
				return client.SendTerminalInput(ctx, args[0], []byte(args[1]))
			})
		}),
	}
}

func newTerminalCaptureCommand() *cobra.Command {
	var lines int
	command := &cobra.Command{Use: "capture <terminal-id>", Args: cobra.ExactArgs(1)}
	command.Flags().IntVar(&lines, "lines", 0, "number of recent lines")
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
			captured, err := client.CaptureTerminal(ctx, &runtimeprotocol.TerminalCaptureParams{
				TerminalParams: runtimeprotocol.TerminalParams{TerminalID: args[0]}, Lines: lines,
			})
			if err != nil {
				return err
			}
			_, err = io.WriteString(app.Stdout, captured.Text)
			return err
		})
	})
	return command
}
