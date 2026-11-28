package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/spf13/cobra"
)

// newSessionCommand 注册会话操作及连接参数。
func newSessionCommand() *cobra.Command {
	command := &cobra.Command{Use: "session", Short: "Manage sessions"}
	flags := command.PersistentFlags()
	addRuntimeCommandFlags(flags)
	command.AddCommand(
		newSessionSpawnCommand(), newSessionSendCommand(), newSessionListCommand(), newSessionWaitCommand(),
		newSessionImportCommand(), newSessionLogsCommand(), newSessionUpdateCommand(), newSessionOptionCommand(),
		sessionTargetCommand("attach", attachSession),
		sessionTargetCommand("inspect", inspectSession),
		sessionTargetCommand("open", openSessionPage),
		sessionTargetCommand("stop", stopSession),
		sessionTargetCommand("reload", reloadSession),
		sessionTargetCommand("detach", detachSession),
	)
	return command
}

// newSessionSpawnCommand 提交独立主会话或活动会话的子任务。
func newSessionSpawnCommand() *cobra.Command {
	var params conversation.SpawnAgentParam
	var initialOptions []string
	var notifyOnFinish, noWait bool
	command := &cobra.Command{Use: "spawn <prompt>", Args: cobra.ExactArgs(1)}
	flags := command.Flags()
	flags.StringVar(&params.ExecutorID, "executor", "", "executor ID")
	flags.StringVar(&params.Cwd, "cwd", "", "working directory")
	flags.StringVar(&params.TaskName, "task", "", "task name")
	flags.StringToStringVar(&params.Labels, "label", nil, "session labels (key=value)")
	flags.BoolVar(&params.Background, "background", false, "hide the session from the default list")
	flags.StringVar(&params.Model, "model", "", "model (provider_id/model_id)")
	flags.StringVar(&params.ReasoningEffort, "reasoning-effort", "", "model reasoning effort")
	flags.StringVar(&params.ForkTurns, "fork-turns", "", "inherit completed turns (none, all, or a count)")
	flags.BoolVar(&notifyOnFinish, "notify-on-finish", true, "notify the parent when finished")
	flags.StringArrayVar(&initialOptions, "initial-option", nil, "initial external option (kind:id=value, repeatable)")
	flags.BoolVar(&noWait, "no-wait", false, "return once the input is accepted")
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
		spawn := params
		spawn.AgentName, _ = cmd.Flags().GetString("agent")
		spawn.Message = args[0]
		if cmd.Flags().Changed("notify-on-finish") {
			spawn.NotifyOnFinish = &notifyOnFinish
		}
		for _, value := range initialOptions {
			option, err := parseSessionOption(value)
			if err != nil {
				return fmt.Errorf("--initial-option: %w", err)
			}
			spawn.InitialOptions = append(spawn.InitialOptions, option)
		}
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
			if conn.SessionID != "" {
				spawn.TaskName = cmp.Or(spawn.TaskName, "task")
				result, err := client.SpawnSession(ctx, &runtimeprotocol.SpawnSessionParams{
					CollaborationSource: collaborationSource(conn),
					SpawnAgentParam:     spawn,
				})
				if err != nil {
					return err
				}
				return finishSubmittedInput(ctx, client, app.Stdout, runtimeprotocol.SessionParams{
					ConversationID: conn.ConversationID, SessionID: result.SessionID,
				}, result.InputItemID, noWait)
			}
			if spawn.ForkTurns != "" && spawn.ForkTurns != "none" || spawn.NotifyOnFinish != nil && !*spawn.NotifyOnFinish {
				return errors.New("--fork-turns and --notify-on-finish require a source session")
			}
			model := defs.ModelSelection{ReasoningEffort: spawn.ReasoningEffort}
			if spawn.Model != "" {
				var err error
				model.Ref, err = defs.ParseModelRef(spawn.Model)
				if err != nil {
					return err
				}
			}
			settings := defs.ExecutorSettings{DefaultExecutorID: spawn.ExecutorID}
			if spawn.Cwd != "" {
				executorID := cmp.Or(spawn.ExecutorID, defs.LocalExecutorID)
				directory, err := client.ResolveWorkspaceDirectory(ctx, &runtimeclient.ResolveWorkspaceDirectoryRequest{
					ExecutorID: executorID, Directory: spawn.Cwd,
				})
				if err != nil {
					return err
				}
				settings.WorkspaceDirectories = map[string]string{executorID: directory}
			}
			record, err := client.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{
				AgentName: cmp.Or(spawn.AgentName, app.Config.Run.AgentName), ExecutorSettings: settings,
				Model: model, InitialOptions: spawn.InitialOptions,
			})
			if err != nil {
				return err
			}
			target := runtimeprotocol.SessionParams{ConversationID: record.ID, SessionID: record.RootSessionID}
			if spawn.TaskName != "" || len(spawn.Labels) > 0 || spawn.Background {
				if _, err := client.UpdateSession(ctx, &runtimeprotocol.UpdateSessionParams{
					SessionParams: target,
					SessionUpdate: conversation.SessionUpdate{TaskName: &spawn.TaskName, Labels: spawn.Labels, Background: &spawn.Background},
				}); err != nil {
					return err
				}
			}
			inputID, err := client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
				ConversationID: record.ID,
				Content:        []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: spawn.Message}}},
			})
			if err != nil {
				return err
			}
			return finishSubmittedInput(ctx, client, app.Stdout, target, inputID, noWait)
		})
	})
	return command
}

// newSessionSendCommand 读取正文并向目标会话提交消息。
func newSessionSendCommand() *cobra.Command {
	var promptFile, mode, model, reasoningEffort string
	var noWait, interrupt bool
	command := &cobra.Command{Use: "send <session-id> [prompt]", Args: cobra.RangeArgs(1, 2)}
	flags := command.Flags()
	flags.StringVar(&mode, "mode", "", "input mode (normal or steer)")
	flags.StringVar(&model, "model", "", "model (provider_id/model_id)")
	flags.StringVar(&reasoningEffort, "reasoning-effort", "", "model reasoning effort")
	flags.StringVar(&promptFile, "prompt-file", "", "read the prompt from a file")
	flags.BoolVar(&noWait, "no-wait", false, "return once the input is accepted")
	flags.BoolVar(&interrupt, "interrupt", false, "interrupt the current turn before sending")
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
		if (len(args) == 2) == (promptFile != "") {
			return errors.New("session send requires a prompt or --prompt-file")
		}
		var prompt string
		if promptFile != "" {
			body, err := os.ReadFile(promptFile)
			if err != nil {
				return err
			}
			prompt = string(body)
		} else {
			prompt = args[1]
		}
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
			target, err := resolveSessionTarget(ctx, client, conn, args[0])
			if err != nil {
				return err
			}
			source := collaborationSource(conn)
			source.ConversationID = target.ConversationID
			result, err := client.SendSession(ctx, &runtimeprotocol.SendSessionParams{
				CollaborationSource: source,
				SendMessageParam: conversation.SendMessageParam{
					SessionID: target.SessionID, Message: prompt, Mode: session.SessionInputMode(mode), Model: model, ReasoningEffort: reasoningEffort,
				},
				Interrupt: interrupt,
			})
			if err != nil {
				return err
			}
			err = finishSubmittedInput(ctx, client, app.Stdout, target, result.InputItemID, noWait)
			var responseErr *runtimeclient.ResponseError
			if errors.As(err, &responseErr) && responseErr.Message == conversation.ErrSendPermissionDenied.Error() {
				return printInputAccepted(app.Stdout, target.SessionID, result.InputItemID)
			}
			return err
		})
	})
	return command
}

// finishSubmittedInput 查已完成的输入，否则开始订阅并等待对应轮次。
func finishSubmittedInput(ctx context.Context, client runtimeclient.Client, out io.Writer, target runtimeprotocol.SessionParams, inputID defs.ItemID, noWait bool) error {
	if noWait {
		return printInputAccepted(out, target.SessionID, inputID)
	}
	turn, err := getSubmittedTurn(ctx, client, target, inputID)
	if err != nil {
		return err
	}
	if turn == nil {
		snapshot, err := subscribeSession(ctx, client, target)
		if err != nil {
			return err
		}
		if snapshot.LastSavedOrdinal != 0 {
			turn, err = getSubmittedTurn(ctx, client, target, inputID)
			if err != nil {
				return err
			}
		}
	}
	for turn == nil {
		event, err := nextSessionEvent(ctx, client, target.SessionID)
		if err != nil {
			return err
		}
		finished, ok := event.(*runtimeclient.TurnFinished)
		if !ok || finished.SessionID != target.SessionID || !slices.Contains(finished.InputItemIDs, inputID) {
			continue
		}
		if err := getTurnFailure(finished.Status, finished.Error); err != nil {
			return err
		}
		turn, err = getSessionTurn(ctx, client, target, finished.TurnID)
		if err != nil {
			return err
		}
		if turn == nil {
			return fmt.Errorf("finished turn %s is not in history", finished.TurnID)
		}
	}
	if err := getTurnFailure(turn.Status, turn.Error); err != nil {
		return err
	}
	return printFinalTurn(ctx, client, out, target, *turn)
}

// getSubmittedTurn 按已保存输入的 ItemID 查找所属的已结束轮。
func getSubmittedTurn(ctx context.Context, client runtimeclient.Client, target runtimeprotocol.SessionParams, inputID defs.ItemID) (*defs.Turn, error) {
	request := runtimeclient.ListItemsRequest{
		ConversationID: target.ConversationID, SessionID: target.SessionID, Order: storage.DirectionDesc,
	}
	for {
		page, err := client.ListItems(ctx, &request)
		if err != nil {
			return nil, err
		}
		for _, row := range page.Items {
			if row.Item.ID == inputID {
				turn, err := getSessionTurn(ctx, client, target, row.Item.TurnID)
				if turn != nil && turn.Status == defs.TurnStatusRunning {
					return nil, err
				}
				return turn, err
			}
		}
		if page.NextCursor == 0 {
			return nil, nil
		}
		request.Cursor = page.NextCursor
	}
}

// getSessionTurn 读取指定轮的持久记录。
func getSessionTurn(ctx context.Context, client runtimeclient.Client, target runtimeprotocol.SessionParams, id defs.TurnID) (*defs.Turn, error) {
	turns, err := client.GetTurns(ctx, &runtimeclient.GetTurnsRequest{
		ConversationID: target.ConversationID, SessionID: target.SessionID, TurnIDs: []defs.TurnID{id},
	})
	if err != nil || len(turns) == 0 {
		return nil, err
	}
	return &turns[0], nil
}

// printFinalTurn 输出最后一次模型调用的 Agent 正文。
func printFinalTurn(ctx context.Context, client runtimeclient.Client, out io.Writer, target runtimeprotocol.SessionParams, turn defs.Turn) error {
	if turn.FinalModelCallID == "" || turn.ItemsRange.FirstOrdinal == 0 {
		return nil
	}
	request := runtimeclient.ListItemsRequest{
		ConversationID: target.ConversationID, SessionID: target.SessionID,
		Cursor: storage.Ordinal(turn.ItemsRange.FirstOrdinal - 1),
	}
	var text strings.Builder
	for {
		page, err := client.ListItems(ctx, &request)
		if err != nil {
			return err
		}
		for _, row := range page.Items {
			if row.Item.TurnID == turn.ID && row.Item.ModelCallID == turn.FinalModelCallID &&
				row.Item.Message != nil && row.Item.Message.From != nil && row.Item.Message.From.Kind == defs.IdentityAgent {
				text.WriteString(getSessionItemText(row.Item))
			}
		}
		if page.NextCursor == 0 || page.NextCursor >= storage.Ordinal(turn.ItemsRange.LastOrdinal) {
			break
		}
		request.Cursor = page.NextCursor
	}
	if text.Len() > 0 {
		_, err := fmt.Fprintln(out, text.String())
		return err
	}
	return nil
}

// newSessionListCommand 提交筛选条件并显示服务端应用的条件。
func newSessionListCommand() *cobra.Command {
	var filter conversation.ListAgentsParam
	var sinceHours int
	var sessionID string
	var statuses []string
	command := &cobra.Command{Use: "ls", Args: cobra.NoArgs}
	command.Flags().StringVar(&sessionID, "session-id", "", "match a session ID")
	command.Flags().StringSliceVar(&statuses, "status", nil, "match session states (comma-separated)")
	command.Flags().StringVar(&filter.Cwd, "cwd", "", "match a working directory (* for all)")
	command.Flags().IntVar(&sinceHours, "since-hours", 24, "match sessions updated within this many hours (0 for all)")
	command.Flags().IntVar(&filter.Limit, "limit", 0, "maximum sessions to list")
	command.Flags().StringToStringVar(&filter.Labels, "label", nil, "match session labels (key=value)")
	command.Flags().BoolVar(&filter.All, "all", false, "include background sessions")
	command.Flags().String("conversation", "", "conversation ID")
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, _ []string) error {
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
			if conn.ConversationID == "" || conn.SessionID == "" && app.ConversationID == "" {
				return errors.New("session ls requires a source session or --conversation")
			}
			query := filter
			query.SessionID = defs.SessionID(sessionID)
			for _, status := range statuses {
				query.Statuses = append(query.Statuses, session.SessionState(status))
			}
			if cmd.Flags().Changed("since-hours") {
				query.SinceHours = &sinceHours
			}
			result, err := client.ListSessions(ctx, &runtimeclient.ListSessionsRequest{ConversationID: conn.ConversationID, SourceSessionID: conn.SessionID, Filter: &query})
			if err != nil {
				return err
			}
			if filter := result.AppliedFilter; filter != nil {
				hours := 0
				if filter.SinceHours != nil {
					hours = *filter.SinceHours
				}
				fmt.Fprintf(app.Stdout, "cwd=%s since_hours=%d all=%t limit=%d labels=%s\n", filter.Cwd, hours, filter.All, filter.Limit, formatSessionLabels(filter.Labels))
			}
			for _, record := range result.Sessions {
				printSessionRecord(app.Stdout, record)
			}
			if result.Truncated && result.TotalMatched != nil {
				fmt.Fprintf(app.Stdout, "matched=%d %s\n", *result.TotalMatched, result.Hint)
			}
			return nil
		})
	})
	return command
}

// newSessionWaitCommand 等待会话收到协作消息。
func newSessionWaitCommand() *cobra.Command {
	var timeout int
	var waitFor string
	command := &cobra.Command{Use: "wait [session-id]", Args: cobra.MaximumNArgs(1)}
	command.Flags().IntVar(&timeout, "timeout", 0, "wait timeout in seconds")
	command.Flags().StringVar(&waitFor, "wait-for", "", "wait for a subagent_result")
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
			source := collaborationSource(conn)
			if len(args) > 0 {
				target, err := resolveSessionTarget(ctx, client, conn, args[0])
				if err != nil {
					return err
				}
				source = runtimeprotocol.CollaborationSource{ConversationID: target.ConversationID, SourceSessionID: target.SessionID}
			} else if source.SourceSessionID == "" {
				return errors.New("session wait requires a source session or session id")
			}
			result, err := client.WaitSession(ctx, &runtimeprotocol.WaitSessionParams{
				CollaborationSource: source,
				WaitAgentParam:      conversation.WaitAgentParam{TimeoutSeconds: timeout, WaitFor: waitFor},
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(app.Stdout, result.Reason)
			if result.Text != "" {
				fmt.Fprintln(app.Stdout, result.Text)
			}
			if result.DetachedConversationID != "" {
				fmt.Fprintln(app.Stdout, result.DetachedConversationID, result.DetachedSessionID)
			}
			if result.Hint != "" {
				fmt.Fprintln(app.Stdout, result.Hint)
			}
			return nil
		})
	})
	return command
}

// newSessionImportCommand 浏览后端目录或按 ID 导入会话。
func newSessionImportCommand() *cobra.Command {
	var location runtime.ExternalAgentLocation
	var query, cursor string
	command := &cobra.Command{Use: "import [external-session-id]", Args: cobra.MaximumNArgs(1)}
	flags := command.Flags()
	flags.StringVar(&location.ExecutorID, "executor", "", "executor ID")
	flags.StringVar(&location.Cwd, "cwd", "", "working directory")
	flags.StringVar(&query, "query", "", "search session titles and directories")
	flags.StringVar(&cursor, "cursor", "", "cursor for the next page")
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
		location.AgentName, _ = cmd.Flags().GetString("agent")
		if location.AgentName == "" {
			return errors.New("session import requires --agent")
		}
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
			if len(args) > 0 {
				result, err := client.ImportSession(ctx, &runtime.ImportSessionRequest{ExternalSessionRequest: runtime.ExternalSessionRequest{
					ExternalAgentLocation: location, ExternalSessionID: args[0],
				}})
				if err != nil {
					return err
				}
				printConversationIDs(app.Stdout, result.Conversation)
				return nil
			}
			page, err := client.ListExternalSessions(ctx, &runtime.ExternalSessionsRequest{
				ExternalAgentLocation: location, Query: query, Cursor: cursor,
			})
			if err != nil {
				return err
			}
			for _, record := range page.Sessions {
				fmt.Fprintln(app.Stdout, record.SessionID, record.Title, record.Cwd)
			}
			if page.NextCursor != "" {
				fmt.Fprintf(app.Stdout, "next_cursor=%s\n", page.NextCursor)
			}
			return nil
		})
	})
	return command
}

// newSessionLogsCommand 打印最近的 Session 日志。
func newSessionLogsCommand() *cobra.Command {
	var limit int
	var cursor uint64
	command := sessionTargetCommand("logs", func(ctx context.Context, client runtimeclient.Client, out io.Writer, conn runtimeConnection, _ string, target runtimeprotocol.SessionParams) error {
		var items []runtimeprotocol.LoggedItem
		var next storage.Ordinal
		if conn.SessionID != "" {
			source := collaborationSource(conn)
			source.ConversationID = target.ConversationID
			page, err := client.ReadSessionHistory(ctx, &runtimeprotocol.ReadSessionHistoryParams{
				CollaborationSource: source,
				SessionHistoryParam: conversation.SessionHistoryParam{SessionID: target.SessionID, Cursor: storage.Ordinal(cursor), Limit: limit},
			})
			if err != nil {
				return err
			}
			items, next = page.Items, page.NextCursor
		} else {
			page, err := client.ListItems(ctx, &runtimeclient.ListItemsRequest{
				ConversationID: target.ConversationID, SessionID: target.SessionID, Order: storage.DirectionDesc, Cursor: storage.Ordinal(cursor), Limit: limit,
			})
			if err != nil {
				return err
			}
			items, next = page.Items, page.NextCursor
			slices.Reverse(items)
		}
		for _, row := range items {
			printSessionItem(out, row)
		}
		if next != 0 {
			fmt.Fprintf(out, "next_cursor=%d\n", next)
		}
		return nil
	})
	command.Flags().IntVar(&limit, "limit", 0, "maximum items to display")
	command.Flags().Uint64Var(&cursor, "cursor", 0, "next cursor from the previous page")
	return command
}

// newSessionUpdateCommand 将显式指定的字段交给服务端保存。
func newSessionUpdateCommand() *cobra.Command {
	command := &cobra.Command{Use: "update <session-id>", Args: cobra.ExactArgs(1)}
	flags := command.Flags()
	flags.String("task-name", "", "task name")
	flags.StringToString("label", nil, "replace session labels (key=value)")
	flags.Bool("background", false, "hide the session from the default list")
	command.RunE = buildCLIHandlerWithArguments(func(cmd *cobra.Command, app *AppArgs, args []string) error {
		var update conversation.SessionUpdate
		if flags.Changed("task-name") {
			value, _ := flags.GetString("task-name")
			update.TaskName = &value
		}
		if flags.Changed("background") {
			value, _ := flags.GetBool("background")
			update.Background = &value
		}
		if flags.Changed("label") {
			update.Labels, _ = flags.GetStringToString("label")
		}
		return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, _ string) error {
			target, err := resolveSessionTarget(ctx, client, conn, args[0])
			if err != nil {
				return err
			}
			result, err := client.UpdateSession(ctx, &runtimeprotocol.UpdateSessionParams{SessionParams: target, SessionUpdate: update})
			if err != nil {
				return err
			}
			printSessionRecord(app.Stdout, result.Session)
			return nil
		})
	})
	return command
}

// newSessionOptionCommand 读取当前选项或提交一项修改。
func newSessionOptionCommand() *cobra.Command {
	var change string
	command := sessionTargetCommand("option", func(ctx context.Context, client runtimeclient.Client, out io.Writer, _ runtimeConnection, _ string, target runtimeprotocol.SessionParams) error {
		if change != "" {
			selection, err := parseSessionOption(change)
			if err != nil {
				return fmt.Errorf("--set: %w", err)
			}
			if err := client.SetSessionOption(ctx, &runtimeprotocol.SetSessionOptionParams{SessionParams: target, ExternalOptionSelection: selection}); err != nil {
				return err
			}
		}
		snapshot, err := subscribeSession(ctx, client, target)
		if err != nil {
			return err
		}
		if snapshot.Options != nil {
			printSessionOptions(out, *snapshot.Options)
			return nil
		}
		return nil
	})
	command.Flags().StringVar(&change, "set", "", "set an option (kind:id=value)")
	return command
}

// sessionTargetCommand 解析目标，交给一条会话操作。
func sessionTargetCommand(name string, run func(context.Context, runtimeclient.Client, io.Writer, runtimeConnection, string, runtimeprotocol.SessionParams) error) *cobra.Command {
	return &cobra.Command{Use: name + " <session-id>", Args: cobra.ExactArgs(1), RunE: buildCLIHandlerWithArguments(
		func(cmd *cobra.Command, app *AppArgs, args []string) error {
			return runRuntimeCommand(cmd.Context(), app, func(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, serverID string) error {
				target, err := resolveSessionTarget(ctx, client, conn, args[0])
				if err != nil {
					return err
				}
				return run(ctx, client, app.Stdout, conn, serverID, target)
			})
		}),
	}
}

// resolveSessionTarget 使用已知对话 ID，缺省时按目标会话查找。
func resolveSessionTarget(ctx context.Context, client runtimeclient.Client, conn runtimeConnection, id string) (runtimeprotocol.SessionParams, error) {
	target := runtimeprotocol.SessionParams{ConversationID: conn.ConversationID, SessionID: defs.SessionID(id)}
	if target.ConversationID == "" {
		record, err := client.GetSessionByID(ctx, target.SessionID)
		if err != nil {
			return target, err
		}
		target.ConversationID = record.ConversationID
	}
	return target, nil
}

// subscribeSession 订阅目标会话，历史恢复完成后读取服务端替换的快照。
func subscribeSession(ctx context.Context, client runtimeclient.Client, target runtimeprotocol.SessionParams) (runtimeprotocol.SubscribeSessionResult, error) {
	snapshot, err := client.SubscribeSession(ctx, &target)
	if err != nil || !snapshot.Restoring {
		return snapshot, err
	}
	for {
		event, err := nextRuntimeEvent(ctx, client)
		if err != nil {
			return snapshot, err
		}
		if closed, ok := event.(*runtimeclient.SessionSubscriptionClosed); ok && closed.SessionID == target.SessionID {
			if closed.Reason == runtime.ErrSessionHistoryRestored.Error() {
				return client.SubscribeSession(ctx, &target)
			}
			return snapshot, fmt.Errorf("session subscription closed: %s", closed.Reason)
		}
	}
}

// attachSession 直接订阅目标会话并持续输出已保存的日志。
func attachSession(ctx context.Context, client runtimeclient.Client, out io.Writer, _ runtimeConnection, _ string, target runtimeprotocol.SessionParams) error {
	snapshot, err := subscribeSession(ctx, client, target)
	if err != nil {
		return err
	}
	var cursor storage.Ordinal
	for cursor < snapshot.LastSavedOrdinal {
		page, err := client.ListItems(ctx, &runtimeclient.ListItemsRequest{
			ConversationID: target.ConversationID, SessionID: target.SessionID, Cursor: cursor,
		})
		if err != nil {
			return err
		}
		for _, row := range page.Items {
			if row.Ordinal <= snapshot.LastSavedOrdinal {
				printSessionItem(out, row)
			}
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	for {
		event, err := nextSessionEvent(ctx, client, target.SessionID)
		if err != nil {
			return err
		}
		if saved, ok := event.(*runtimeclient.ItemsSaved); ok && saved.SessionID == target.SessionID {
			for _, row := range saved.Items {
				if row.Ordinal > snapshot.LastSavedOrdinal {
					printSessionItem(out, row)
				}
			}
		}
	}
}

// nextSessionEvent 等待事件，并返回目标订阅的关闭原因。
func nextSessionEvent(ctx context.Context, client runtimeclient.Client, id defs.SessionID) (runtimeclient.ClientEvent, error) {
	event, err := nextRuntimeEvent(ctx, client)
	if closed, ok := event.(*runtimeclient.SessionSubscriptionClosed); ok && closed.SessionID == id {
		return nil, fmt.Errorf("session subscription closed: %s", closed.Reason)
	}
	return event, err
}

func inspectSession(ctx context.Context, client runtimeclient.Client, out io.Writer, _ runtimeConnection, _ string, target runtimeprotocol.SessionParams) error {
	details, err := client.GetSessionDetails(ctx, target.ConversationID, target.SessionID)
	if err != nil {
		return err
	}
	printSessionRecord(out, details.Session)
	fmt.Fprintf(out, "conversation_id=%s\n", details.ConversationID)
	if details.Session.External != nil {
		fmt.Fprintf(out, "external_session_id=%s executor=%s cwd=%s\n", details.Session.External.SessionID, details.Session.External.ExecutorID, details.Session.External.Cwd)
	}
	return nil
}

func openSessionPage(_ context.Context, _ runtimeclient.Client, _ io.Writer, conn runtimeConnection, serverID string, target runtimeprotocol.SessionParams) error {
	address, err := getSessionPageURL(conn, serverID, target)
	if err != nil {
		return err
	}
	openBrowser(address)
	return nil
}

func getSessionPageURL(conn runtimeConnection, serverID string, target runtimeprotocol.SessionParams) (string, error) {
	path := fmt.Sprintf("s/%s/c/%s/sessions/%s", serverID, target.ConversationID, target.SessionID)
	if strings.Contains(conn.URL, "#world=") {
		page, err := GetWebUIURL(conn.URL, conn.Token)
		if err != nil {
			return "", err
		}
		return url.JoinPath(page, path)
	}
	return getRuntimeServerHTTPURL(conn.URL, webUIPath+path)
}

func stopSession(ctx context.Context, client runtimeclient.Client, out io.Writer, conn runtimeConnection, _ string, target runtimeprotocol.SessionParams) error {
	source := collaborationSource(conn)
	source.ConversationID = target.ConversationID
	result, err := client.StopSession(ctx, &runtimeprotocol.StopSessionParams{CollaborationSource: source, SessionID: target.SessionID})
	if err == nil {
		if result.TurnID != "" {
			fmt.Fprintln(out, result.TurnID)
		} else {
			fmt.Fprintln(out, "no_active_turn")
		}
	}
	return err
}

func reloadSession(ctx context.Context, client runtimeclient.Client, _ io.Writer, _ runtimeConnection, _ string, target runtimeprotocol.SessionParams) error {
	return client.ReloadSession(ctx, &target)
}

func detachSession(ctx context.Context, client runtimeclient.Client, out io.Writer, _ runtimeConnection, _ string, target runtimeprotocol.SessionParams) error {
	result, err := client.DetachSession(ctx, &target)
	if err == nil {
		printConversationIDs(out, result.Conversation)
	}
	return err
}
