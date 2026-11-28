package cli

import (
	"context"
	"fmt"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/spf13/cobra"
)

func newAgentCommand() *cobra.Command {
	command := &cobra.Command{Use: "agent", Short: "List configured agents and their options"}
	addRuntimeCommandFlags(command.PersistentFlags())
	command.AddCommand(&cobra.Command{
		Use: "ls", Args: cobra.NoArgs,
		RunE: BuildCLIHandler(func(ctx context.Context, app *AppArgs) error {
			return runRuntimeCommand(ctx, app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
				agents, err := client.ListAgents(ctx)
				if err != nil {
					return err
				}
				for _, agent := range agents {
					fmt.Fprintln(app.Stdout, agent.Name, agent.Type, agent.DefaultModel, agent.Description)
				}
				return nil
			})
		}),
	}, &cobra.Command{
		Use: "options <agent-name>", Args: cobra.ExactArgs(1),
		RunE: buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
			return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
				options, err := client.GetAgentOptions(ctx, &runtime.AgentOptionsRequest{
					ExternalAgentLocation: runtime.ExternalAgentLocation{AgentName: args[0]},
				})
				if err != nil {
					return err
				}
				printSessionOptions(app.Stdout, options)
				return nil
			})
		}),
	})
	return command
}
