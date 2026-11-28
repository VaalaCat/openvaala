package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/spf13/cobra"
)

func newSkillCommand() *cobra.Command {
	command := &cobra.Command{Use: "skill", Short: "List, search and read skills"}
	addRuntimeCommandFlags(command.PersistentFlags())
	command.AddCommand(newSkillListCommand(false), newSkillListCommand(true), newSkillGetCommand())
	return command
}

func newSkillListCommand(search bool) *cobra.Command {
	command := &cobra.Command{Use: "list", Args: cobra.NoArgs}
	if search {
		command.Use, command.Args = "search <query>", cobra.ExactArgs(1)
	}
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
		agent, _ := cmd.Flags().GetString("agent")
		var query string
		if search {
			query = args[0]
		}
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
			infos, err := client.GetSkills(ctx, &runtimeclient.GetSkillsRequest{AgentName: agent, Query: query})
			if err != nil {
				return err
			}
			for _, info := range infos {
				fmt.Fprintln(app.Stdout, info.Name, info.Description)
			}
			return nil
		})
	})
	return command
}

func newSkillGetCommand() *cobra.Command {
	return &cobra.Command{
		Use: "get <name> [file]", Args: cobra.RangeArgs(1, 2),
		RunE: buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
			agent, _ := cmd.Flags().GetString("agent")
			return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
				params := runtimeprotocol.GetSkillParams{Name: args[0], AgentName: agent}
				if len(args) == 2 {
					params.File = args[1]
				}
				result, err := client.GetSkill(ctx, &params)
				if err != nil {
					return err
				}
				_, err = io.WriteString(app.Stdout, result.Content)
				return err
			})
		}),
	}
}
