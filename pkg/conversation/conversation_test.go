package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// conversationBuildDirectory 是本包测试共用的编译目录，TestMain 创建，全部测试结束后删除。
var conversationBuildDirectory string

// TestMain 创建并清理本包测试共用的编译目录。
func TestMain(m *testing.M) {
	// 1. 创建共用编译目录，执行测试，删除目录并返回测试退出码。
	directory, err := os.MkdirTemp("", "openvaala-conversation-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	conversationBuildDirectory = directory
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// buildFakeMCPServerOnce 在第一次用到时把 pkg/mcpclient 的假 MCP server 编译到 conversationBuildDirectory；
// 测试本身带 -race 时同样带 -race 编译。
var buildFakeMCPServerOnce = sync.OnceValues(func() (string, error) {
	binary := filepath.Join(conversationBuildDirectory, "mcpserver")
	arguments := []string{"build", "-o", binary}
	if info, ok := debug.ReadBuildInfo(); ok && slices.ContainsFunc(info.Settings, func(setting debug.BuildSetting) bool {
		return setting.Key == "-race" && setting.Value == "true"
	}) {
		arguments = append(arguments, "-race")
	}
	command := exec.Command("go", append(arguments, "../mcpclient/testdata/mcpserver")...)
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build fake MCP server: %w\n%s", err, output)
	}
	return binary, nil
})

// buildFakeMCPServer 返回共用的测试 MCP server 程序。
func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	// 1. 执行共用的单次编译，失败时结束测试。
	binary, err := buildFakeMCPServerOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// TestConversationCreateAndGet 检查新对话的保存和重新打开，以及根 Session、对话记录写入失败或根记录缺失时的错误。
func TestConversationCreateAndGet(t *testing.T) {
	// 1. 分别执行正常创建、根 Session 保存失败、对话记录保存失败和根记录被删的场景。
	for _, failure := range []string{"", "root", "conversation", "broken_root"} {
		t.Run(failure, func(t *testing.T) {
			store := openConversationDAO(t)
			mutation := &conversationMutation{
				Mutation: store.mutation,
				fail:     failure,
			}
			opt := &ConversationOpt{
				Query:    store.query,
				Mutation: mutation,
				Definitions: &ConversationDefinitions{
					Agents: []*agent.AgentDefinition{{
						Name: "assistant",
						Model: defs.ModelRef{
							ProviderID: "test",
							ModelID:    "test",
						},
					}},
					Models: []*defs.ModelDefinition{newTestModel(&llmkit.Target{Model: "test"}, 0)},
				},
			}
			// 2. 创建对话，保存失败时检查没返回实例、根失败时也没保存对话记录。
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if failure == "root" || failure == "conversation" {
				if err == nil || c != nil {
					t.Fatal("partial creation succeeded")
				}
				if failure == "root" && mutation.conversationSaved {
					t.Fatal("conversation saved after root failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root, found, err := store.query.GetSession(t.Context(), c.GetID(), c.GetRootSessionID())
			if err != nil || !found || root.ConversationID != c.GetID() {
				t.Fatalf("root=%+v %v", root, err)
			}
			if failure == "broken_root" {
				if err := store.mutation.DeleteSession(t.Context(), c.GetID(), root.ID); err != nil {
					t.Fatal(err)
				}
			}
			// 3. 重新打开，核对根 Session；根记录被删时应返回错误。
			loaded, err := reopenTestConversation(t, c, opt)
			if failure == "broken_root" {
				if err == nil {
					t.Fatal("missing root accepted")
				}
				return
			}
			if err != nil || loaded.GetRootSessionID() != root.ID {
				t.Fatalf("loaded=%v %v", loaded, err)
			}
		})
	}
}

// TestConversationRejectsInvalidConfiguration 检查配置无效时 CreateConversation 返回错误，且没保存根 Session。
func TestConversationRejectsInvalidConfiguration(t *testing.T) {
	// 1. 准备缺失依赖、重复定义和未知根 Agent 的配置。
	tests := []struct {
		kind string
		// wantErr 非空时，错误信息必须包含它。
		wantErr string
	}{
		{kind: "nil_opt"},
		{
			kind:    "executor_without_id",
			wantErr: "execution environment requires an ID",
		},
		{kind: "duplicate_executor"},
		{kind: "no_agents"},
		{
			kind:    "no_models",
			wantErr: "at least one model",
		},
		{
			kind:    "nil_agent",
			wantErr: "agent definition requires a name",
		},
		{
			kind:    "unnamed_agent",
			wantErr: "agent definition requires a name",
		},
		{kind: "duplicate_agent"},
		{kind: "agent_without_model"},
		{kind: "unknown_root"},
	}
	for _, tt := range tests {
		kind := tt.kind
		t.Run(kind, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			mutation := &conversationMutation{Mutation: store.mutation}
			opt := conversationOpt(store, model)
			opt.Mutation = mutation
			rootAgentName := "assistant"
			switch kind {
			case "nil_opt":
				opt = nil
			case "executor_without_id":
				opt.Definitions.Executors = newTestExecutors("")
			case "duplicate_executor":
				opt.Definitions.Executors = newTestExecutors("local", "local")
			case "no_agents":
				opt.Definitions.Agents = nil
			case "no_models":
				opt.Definitions.Models = nil
			case "nil_agent":
				opt.Definitions.Agents = []*agent.AgentDefinition{nil}
			case "unnamed_agent":
				opt.Definitions.Agents = append(opt.Definitions.Agents, &agent.AgentDefinition{
					Name:  " ",
					Model: opt.Definitions.Agents[0].Model,
				})
			case "duplicate_agent":
				opt.Definitions.Agents = append(opt.Definitions.Agents, opt.Definitions.Agents[0])
			case "agent_without_model":
				opt.Definitions.Agents = []*agent.AgentDefinition{{Name: "assistant"}}
			case "unknown_root":
				rootAgentName = "missing"
			}
			// 2. 创建对话，检查错误内容以及根 Session 尚未保存。
			c, err := CreateConversation(t.Context(), rootAgentName, defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err == nil || c != nil || mutation.sessionSaved {
				t.Fatal("invalid configuration started creating persistent records")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("CreateConversation() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestConversationRun 检查根 Session 输入、子任务结果通知、子任务失败和对话停止的完整流程。
func TestConversationRun(t *testing.T) {
	// 1. 准备根输入、无效输入、InputID 去重、子任务通知、子任务失败和整体停止场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *conversationHarness)
	}{
		{
			name: "root_input_events",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "first")
				receiveModelRequest(t, h.model).reply <- modelResponse{text: "answer"}
				finished, completed := h.waitTurnFinished(t, id)
				if finished.Status != defs.TurnStatusCompleted || finished.Err != nil || !slices.Contains(completed, "answer") {
					t.Fatalf("finished=%+v completed=%v", finished, completed)
				}
			},
		},
		{
			name: "invalid_input_rejected",
			run: func(t *testing.T, h *conversationHarness) {
				for _, input := range []*ConversationInput{
					nil,
					{Mode: session.SessionInputModeNormal},
					{
						Mode: "invalid",
						From: &defs.Address{Kind: defs.IdentityUser},
					},
				} {
					if id, err := h.c.HandleInput(t.Context(), input); err == nil || id != "" {
						t.Fatalf("invalid input %+v accepted as %s", input, id)
					}
				}
			},
		},
		{
			name: "delegate_and_summary_visible",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				childID := decodeSpawnAgentResult(t, root, "spawn")
				root.reply <- modelResponse{text: "delegated"}
				if finished, _ := h.waitTurnFinished(t, id); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("delegating Turn=%+v", finished)
				}
				child.reply <- modelResponse{text: "child summary result"}
				notified := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				if !strings.Contains(requestJSON(notified), "openvaala-subagent-notification") ||
					!strings.Contains(requestJSON(notified), "child summary result") ||
					!strings.Contains(requestJSON(notified), string(childID)) {
					t.Fatalf("root did not read notification: %s", requestJSON(notified))
				}
				notified.reply <- modelResponse{text: "summary for user"}
				finished, completed := h.nextTurnFinished(t)
				if finished.Status != defs.TurnStatusCompleted || slices.Contains(finished.InputItemIDs, id) || !slices.Contains(completed, "summary for user") {
					t.Fatalf("summary Turn=%+v completed=%v", finished, completed)
				}
			},
		},
		{
			name: "parent_interrupted_keeps_notification",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				root.reply <- modelResponse{
					reasoning: "root thinking",
					hold:      true,
				}
				h.waitAgentEvent(t, agent.AgentEventItemStarted, defs.ItemReasoning)
				child.reply <- modelResponse{text: "child result"}
				rootSession, found := h.impl().runningSessions.Load(h.c.GetRootSessionID())
				if !found {
					t.Fatal("root Session is not running")
				}
				steer, err := rootSession.WaitSteer(t.Context())
				if err != nil || len(steer) != 1 || steer[0].Message.SubagentNotification == nil {
					t.Fatalf("notification not attached: %+v %v", steer, err)
				}
				if turnID, err := rootSession.InterruptTurn(t.Context()); err != nil || turnID == "" {
					t.Fatalf("interrupt=%s %v", turnID, err)
				}
				interrupted, _ := h.waitTurnFinished(t, id)
				if interrupted.Status != defs.TurnStatusInterrupted || slices.Contains(interrupted.InputItemIDs, steer[0].ID) {
					t.Fatalf("interrupted Turn=%+v", interrupted)
				}
				// 用户中断主 Agent 后通知只排队：下一条用户输入先开一轮，通知在它之后开一轮。
				userID := h.input(t, session.SessionInputModeNormal, "resume after pause")
				userTurn := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				if !strings.Contains(requestJSON(userTurn), "resume after pause") || strings.Contains(requestJSON(userTurn), "child result") {
					t.Fatalf("user input did not run before the paused notification: %s", requestJSON(userTurn))
				}
				userTurn.reply <- modelResponse{text: "user first"}
				if finished, _ := h.waitTurnFinished(t, userID); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("user Turn=%+v", finished)
				}
				next := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				if !strings.Contains(requestJSON(next), "openvaala-subagent-notification") || !strings.Contains(requestJSON(next), "child result") {
					t.Fatalf("notification lost after interrupt: %s", requestJSON(next))
				}
				next.reply <- modelResponse{text: "after interrupt"}
				if finished, _ := h.waitTurnFinished(t, steer[0].ID); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("notification Turn=%+v", finished)
				}
				saved := listSessionItems(t, h.store, h.c.GetID(), h.c.GetRootSessionID())
				if !slices.ContainsFunc(saved, func(item *defs.Item) bool {
					return item.TurnID == interrupted.TurnID && item.Reasoning != nil && item.Reasoning.Status == defs.ReasoningInterrupted
				}) {
					t.Fatal("interrupted reasoning was not persisted")
				}
			},
		},
		{
			name: "duplicate_input_id_runs_once",
			run: func(t *testing.T, h *conversationHarness) {
				input := conversationInput(session.SessionInputModeNormal, "only once")
				input.InputID = "retry-1"
				first, err := h.c.HandleInput(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				second, err := h.c.HandleInput(t.Context(), input)
				if err != nil || second != first {
					t.Fatalf("retry = %s %v, want %s", second, err, first)
				}
				receiveModelRequest(t, h.model).reply <- modelResponse{text: "done"}
				h.waitTurnFinished(t, first)
				requireNoModelRequest(t, h.model)
			},
		},
		{
			name: "child_failure_isolated",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				root.reply <- modelResponse{text: "delegated"}
				h.waitTurnFinished(t, id)
				child.reply <- modelResponse{status: http.StatusBadRequest}
				notified := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				if !strings.Contains(requestText(t, notified), `<status>failed</status>`) {
					t.Fatalf("failed notification missing: %s", requestJSON(notified))
				}
				notified.reply <- modelResponse{text: "noted"}
				h.nextTurnFinished(t)
				next := h.input(t, session.SessionInputModeNormal, "still running")
				receiveSessionRequest(t, h.model, h.c.GetRootSessionID()).reply <- modelResponse{text: "yes"}
				if finished, _ := h.waitTurnFinished(t, next); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("root after child failure=%+v", finished)
				}
			},
		},
		{
			name: "session_not_blocked_by_busy_parent",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "first task")
				root, child := receiveRootAndTask(t, h.model, "review")
				childID := decodeSpawnAgentResult(t, root, "spawn")
				root.reply <- modelResponse{
					text: "root busy",
					hold: true,
				}
				if err := h.impl().sendMessage(t.Context(), h.record(t, h.c.GetRootSessionID()), &SendMessageParam{
					SessionID: childID,
					Mode:      session.SessionInputModeNormal,
					Message:   "second task",
				}); err != nil {
					t.Fatal(err)
				}
				child.reply <- modelResponse{text: "first done"}
				receiveSessionRequest(t, h.model, childID).reply <- modelResponse{text: "second done"}
				waitCompletedTurns(t, h.store, childID, 2)
				h.stop(t)
				if finished, _ := h.waitTurnFinished(t, id); finished.Status != defs.TurnStatusInterrupted {
					t.Fatalf("busy root Turn=%+v", finished)
				}
			},
		},
		{
			name: "stop_waits_all_tasks",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				childID := decodeSpawnAgentResult(t, root, "spawn")
				root.reply <- modelResponse{
					text: "root partial",
					hold: true,
				}
				child.reply <- modelResponse{
					text: "child partial",
					hold: true,
				}
				if err := h.stop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run=%v", err)
				}
				if finished, _ := h.waitTurnFinished(t, id); finished.Status != defs.TurnStatusInterrupted {
					t.Fatalf("root Turn=%+v", finished)
				}
				for event := range h.events {
					if _, ok := event.(*session.SessionTurnFinished); ok {
						t.Fatalf("unexpected event after root Turn: %+v", event)
					}
				}
				for _, sessionID := range []defs.SessionID{h.c.GetRootSessionID(), childID} {
					if latest := latestTurn(t, h.store, sessionID); latest.Status != defs.TurnStatusInterrupted {
						t.Fatalf("Session %s latest Turn=%+v", sessionID, latest)
					}
				}
				if _, found := h.impl().runningSessions.Load(childID); found {
					t.Fatal("stopped child is still running")
				}
				if _, err := h.c.HandleInput(t.Context(), conversationInput(session.SessionInputModeNormal, "late")); err == nil {
					t.Fatal("stopped Conversation accepted input")
				}
			},
		},
	}
	// 2. 启动独立对话，执行输入与模型回复，检查事件和保存结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			tt.run(t, startTestConversation(t, store, model, createTestConversation(t, store, model)))
		})
	}
}

// TestConversationRunWithoutSubscriber 检查 Run 的 events 为 nil 时排队输入照常执行。
func TestConversationRunWithoutSubscriber(t *testing.T) {
	// 1. 分别准备单条和多条排队输入。
	for _, tt := range []struct {
		name   string
		inputs int
	}{
		{
			name:   "single_input",
			inputs: 1,
		},
		{
			name:   "queued_inputs",
			inputs: 3,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 用 nil 事件通道运行对话，提交全部输入并回复模型，等待完成后取消。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			c := createTestConversation(t, store, model)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- c.Run(ctx, nil) }()
			for i := range tt.inputs {
				if _, err := c.HandleInput(t.Context(), conversationInput(session.SessionInputModeNormal, fmt.Sprint("input ", i))); err != nil {
					t.Fatal(err)
				}
			}
			for range tt.inputs {
				receiveModelRequest(t, model).reply <- modelResponse{text: "answer"}
			}
			waitCompletedTurns(t, store, c.GetRootSessionID(), tt.inputs)
			cancel()
			if err := waitRunDone(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run=%v", err)
			}
		})
	}
}

// TestConversationRunLifecycle 检查 Run 只能调用一次、Run 之前的输入会等待、根 Session 启动失败时 Run 返回错误。
func TestConversationRunLifecycle(t *testing.T) {
	// 1. 列出重复启动、启动前等待和根 Session 修复失败场景。
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, store *conversationStore, model *conversationModel, c Conversation)
	}{
		{
			name: "run_only_once",
			run: func(t *testing.T, store *conversationStore, model *conversationModel, c Conversation) {
				h := startTestConversation(t, store, model, c)
				if err := c.Run(t.Context(), nil); err == nil {
					t.Fatal("second Run accepted")
				}
				h.stop(t)
			},
		},
		{
			name: "input_waits_for_run",
			run: func(t *testing.T, store *conversationStore, model *conversationModel, c Conversation) {
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				if _, err := c.HandleInput(ctx, conversationInput(session.SessionInputModeNormal, "early")); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("input before Run=%v", err)
				}
			},
		},
		{
			name: "root_failure_stops_run",
			run: func(t *testing.T, store *conversationStore, model *conversationModel, c Conversation) {
				opt := conversationOpt(store, model)
				opt.Query = &failingTurnQuery{
					Query:     store.query,
					sessionID: c.GetRootSessionID(),
				}
				loaded, err := reopenTestConversation(t, c, opt)
				if err != nil {
					t.Fatal(err)
				}
				// 同步调用 Run 时没有并发读取方；缓冲容纳根 Session 的启动与停止事件。
				events := make(chan ConversationEvent, 2)
				if err := loaded.Run(t.Context(), events); err == nil || !strings.Contains(err.Error(), "injected list turns") {
					t.Fatalf("Run=%v", err)
				}
				var received []ConversationEvent
				for event := range events {
					received = append(received, event)
				}
				if len(received) != 2 {
					t.Fatalf("events=%+v", received)
				}
				if stopped, ok := received[1].(*ConversationSessionStopped); !ok || stopped.Err == nil {
					t.Fatalf("last event=%T %+v", received[1], received[1])
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 创建独立对话，执行生命周期操作并检查错误和事件。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			tt.run(t, store, model, createTestConversation(t, store, model))
		})
	}
}

// TestConversationEnvironments 检查执行环境装配失败时不保存记录、父子 Session 共用一个 MCP 进程、Run 返回后连接断开。
func TestConversationEnvironments(t *testing.T) {
	// 1. 列出装配失败、父子共用执行环境和 Run 返回后停止连接的场景。
	tests := []struct {
		name string
		run  func(t *testing.T, store *conversationStore, model *conversationModel, opt *ConversationOpt, pidPath string)
	}{
		{
			name: "create_conversation_environment_error_saves_nothing",
			run: func(t *testing.T, store *conversationStore, _ *conversationModel, opt *ConversationOpt, pidPath string) {
				mutation := &conversationMutation{Mutation: store.mutation}
				opt.Mutation = mutation
				// 默认 Executor 的根目录不存在，装配失败。
				opt.Definitions.Executors[0].WorkspaceDirectory = filepath.Join(t.TempDir(), "missing")
				c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
				if err == nil || c != nil {
					t.Fatalf("CreateConversation() = %v, %v; want an environment error", c, err)
				}
				if mutation.sessionSaved || mutation.conversationSaved {
					t.Fatalf("records saved: session %v, conversation %v; want none", mutation.sessionSaved, mutation.conversationSaved)
				}
				page, err := store.query.ListConversations(t.Context(), dao.ListConversationsRequest{Limit: 100})
				if err != nil || len(page.Items) != 0 {
					t.Fatalf("ListConversations() = %+v, %v; want no conversation", page.Items, err)
				}
				if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fake server was started: %v", err)
				}
			},
		},
		{
			name: "root_and_child_share_environments",
			run: func(t *testing.T, store *conversationStore, model *conversationModel, opt *ConversationOpt, pidPath string) {
				c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
				if err != nil {
					t.Fatal(err)
				}
				h := startTestConversation(t, store, model, c)
				details, err := c.GetSessionDetails(t.Context(), c.GetRootSessionID())
				if err != nil || len(details.Executors) != len(opt.Definitions.Executors) {
					t.Fatalf("executors = %+v, %v; want %d", details.Executors, err, len(opt.Definitions.Executors))
				}
				h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, model, "review")
				root.reply <- modelResponse{calls: []llmkit.ToolCall{{
					ID:        "root-pid",
					Name:      "mcp__fake__pid",
					Arguments: `{}`,
				}}}
				child.reply <- modelResponse{calls: []llmkit.ToolCall{{
					ID:        "child-pid",
					Name:      "mcp__fake__pid",
					Arguments: `{}`,
				}}}
				// 两边下一次请求带回各自调用的结果，先到哪个不定。
				serverPIDs := make(map[string]string)
				for range 2 {
					request := receiveModelRequest(t, model)
					callID := "child-pid"
					if strings.Contains(requestJSON(request), `"tool_call_id":"root-pid"`) {
						callID = "root-pid"
					}
					var serverPID string
					decodeRequestToolResult(t, request, callID, &serverPID)
					serverPIDs[callID] = serverPID
					request.reply <- modelResponse{text: "done"}
				}
				want := strconv.Itoa(readFakeMCPServerPID(t, pidPath))
				if serverPIDs["root-pid"] != want || serverPIDs["child-pid"] != want {
					t.Fatalf("server PIDs = %v, want both sessions served by the one process %s", serverPIDs, want)
				}
			},
		},
		{
			name: "environments_stop_after_sessions",
			run: func(t *testing.T, store *conversationStore, model *conversationModel, opt *ConversationOpt, pidPath string) {
				c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
				if err != nil {
					t.Fatal(err)
				}
				h := startTestConversation(t, store, model, c)
				waitMCPServerConnected(t, c, "local", "fake")
				serverPID := readFakeMCPServerPID(t, pidPath)
				h.stop(t)
				for _, info := range c.GetMCPServers() {
					if info.State == mcpclient.ConnectionStateConnected {
						t.Fatalf("MCP server %s is still connected after Run returned", info.ServerID)
					}
				}
				if isProcessAlive(serverPID) {
					t.Fatalf("fake server process %d is still running after Run returned", serverPID)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 配置 local 和测试 MCP server，执行用例并核对记录、进程和连接状态。
			store, model := openConversationDAO(t), newConversationModel(t)
			opt := conversationOpt(store, model)
			pidPath := filepath.Join(t.TempDir(), "fake.pid")
			server := newFakeMCPServerDefinition(t, "fake", "local")
			server.Args = []string{"-pidfile", pidPath}
			opt.Definitions.Executors = []*defs.ExecutorDefinition{newLocalTestExecutor(t, "local")}
			opt.Definitions.MCPServers = []*mcpclient.ServerDefinition{server}
			tt.run(t, store, model, opt, pidPath)
		})
	}
}

// TestConversationReconfigure 检查运行中替换 Executor 设置或定义：根 Session 重启、客户端 MCP 保留、并发替换都生效、Run 不退出。
func TestConversationReconfigure(t *testing.T) {
	// 1. 准备配置变更场景；getRootExecutorIDs 返回根 Session 详情里的 Executor ID。
	getRootExecutorIDs := func(t *testing.T, h *conversationHarness) []string {
		t.Helper()
		details, err := h.c.GetSessionDetails(t.Context(), h.c.GetRootSessionID())
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, executor := range details.Executors {
			ids = append(ids, executor.ID)
		}
		return ids
	}
	tests := []struct {
		name string
		// run 的 newDefinitions 每次返回一份新的测试定义，带着这个用例的两个 Executor。
		run func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions)
	}{
		{
			name: "set_executor_settings_in_place",
			run: func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions) {
				localDirectory := h.impl().config.Load().definitions.Executors[0].WorkspaceDirectory
				settings, err := h.c.SetExecutorSettings(t.Context(), defs.ExecutorSettings{ExecutorIDs: []string{"other"}})
				if err != nil {
					t.Fatalf("SetExecutorSettings() error = %v", err)
				}
				if want := (defs.ExecutorSettings{
					ExecutorIDs:          []string{"other"},
					WorkspaceDirectories: map[string]string{"local": localDirectory},
				}); !reflect.DeepEqual(settings, want) {
					t.Fatalf("SetExecutorSettings() = %+v, want %+v with the local directory filled", settings, want)
				}
				event, seen := h.waitEvent(t, func(event ConversationEvent) bool {
					_, ok := event.(*ConversationReconfigured)
					return ok
				})
				if got := event.(*ConversationReconfigured).ExecutorSettings; !reflect.DeepEqual(got, settings) {
					t.Fatalf("ConversationReconfigured.ExecutorSettings = %+v, want %+v", got, settings)
				}
				// 第一个是 Run 启动根 Session 时的 started，之后是换配置时的 stopped、started。
				if got, want := getRootLifecycle(h.c.GetRootSessionID(), seen), []string{"started", "stopped", "started"}; !slices.Equal(got, want) {
					t.Fatalf("root lifecycle before ConversationReconfigured = %v, want %v", got, want)
				}
				if got := getRootExecutorIDs(t, h); !slices.Equal(got, []string{"other"}) {
					t.Fatalf("root executors = %v, want [other]", got)
				}
				select {
				case err := <-h.done:
					t.Fatalf("Run returned after reconfiguring: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
			},
		},
		{
			name: "set_definitions_rescans_skills",
			run: func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions) {
				definitions := newDefinitions()
				definitions.Skills = []*skill.Skill{{
					Name:        "added",
					Description: "新加的技能",
					Path:        "/skills/added/SKILL.md",
				}}
				if err := h.c.SetDefinitions(t.Context(), definitions); err != nil {
					t.Fatalf("SetDefinitions() error = %v", err)
				}
				if !slices.ContainsFunc(h.c.GetSkills(), func(info skill.SkillInfo) bool { return info.Name == "added" }) {
					t.Fatalf("GetSkills() = %+v, want added", h.c.GetSkills())
				}
			},
		},
		{
			name: "set_definitions_rejects_missing_agent",
			run: func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions) {
				before, err := h.c.GetSessionDetails(t.Context(), h.c.GetRootSessionID())
				if err != nil {
					t.Fatal(err)
				}
				definitions := newDefinitions()
				definitions.Agents = definitions.Agents[1:]
				err = h.c.SetDefinitions(t.Context(), definitions)
				if want := `uses agent "assistant", which is not configured`; err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("SetDefinitions() error = %v, want error containing %q", err, want)
				}
				after, err := h.c.GetSessionDetails(t.Context(), h.c.GetRootSessionID())
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("root details after rejection = %+v, %v; want unchanged %+v", after, err, before)
				}
				requireNoEvent(t, h, func(event ConversationEvent) bool {
					_, ok := event.(*ConversationReconfigured)
					return ok
				})
			},
		},
		{
			name: "client_mcp_survives_reconfigure",
			run: func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions) {
				if err := h.c.SetClientMCPServers(t.Context(), "client-1", []ClientMCPServer{{
					ID:        "echo",
					Transport: pingMCPTransport{},
				}}); err != nil {
					t.Fatalf("SetClientMCPServers() error = %v", err)
				}
				if err := h.c.SetDefinitions(t.Context(), newDefinitions()); err != nil {
					t.Fatalf("SetDefinitions() error = %v", err)
				}
				if !slices.ContainsFunc(h.c.GetMCPServers(), func(info MCPServerInfo) bool {
					return info.ExecutorID == defs.ClientExecutorID && info.ServerID == "echo"
				}) {
					t.Fatalf("MCP servers after reconfigure = %+v, want the client server echo", h.c.GetMCPServers())
				}
				id := h.input(t, session.SessionInputModeNormal, "ping")
				request := receiveModelRequest(t, h.model)
				if names := requestToolNames(request); !slices.Contains(names, "mcp__echo__ping") {
					t.Fatalf("model tools = %v, want mcp__echo__ping", names)
				}
				request.reply <- modelResponse{text: "pong"}
				h.waitTurnFinished(t, id)
			},
		},
		{
			name: "concurrent_changes_both_apply",
			run: func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions) {
				definitions := newDefinitions()
				definitions.Models = append(definitions.Models, newTestModel(&llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  h.model.server.URL,
					Model:    "extra",
				}, 0))
				errs := make(chan error, 2)
				go func() {
					_, err := h.c.SetExecutorSettings(t.Context(), defs.ExecutorSettings{ExecutorIDs: []string{"other"}})
					errs <- err
				}()
				go func() { errs <- h.c.SetDefinitions(t.Context(), definitions) }()
				for range 2 {
					select {
					case err := <-errs:
						if err != nil {
							t.Fatalf("concurrent change error = %v", err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("concurrent changes did not return")
					}
				}
				if got := getRootExecutorIDs(t, h); !slices.Equal(got, []string{"other"}) {
					t.Fatalf("root executors = %v, want [other]", got)
				}
				extra := defs.ModelSelection{Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "extra",
				}}
				if _, err := h.c.SetSessionModel(t.Context(), h.c.GetRootSessionID(), extra); err != nil {
					t.Fatalf("SetSessionModel(test/extra) error = %v", err)
				}
			},
		},
		{
			name: "input_after_reconfigure_reaches_new_root",
			run: func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions) {
				definitions := newDefinitions()
				definitions.Agents[0].Instructions = "NEW_INSTRUCTIONS"
				if err := h.c.SetDefinitions(t.Context(), definitions); err != nil {
					t.Fatalf("SetDefinitions() error = %v", err)
				}
				id := h.input(t, session.SessionInputModeNormal, "after reconfigure")
				request := receiveModelRequest(t, h.model)
				if !strings.Contains(requestText(t, request), "NEW_INSTRUCTIONS") {
					t.Fatalf("system instructions do not carry the new definition: %s", requestText(t, request))
				}
				request.reply <- modelResponse{text: "done"}
				if finished, _ := h.waitTurnFinished(t, id); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("Turn after reconfigure = %+v, want completed", finished)
				}
			},
		},
		{
			// 换定义校验之后才建出的子 Session 可能用着新定义里没有的 Agent：启动它、查它的详情都返回普通错误，对话照常运行。
			name: "unconfigured_child_agent_returns_error",
			run: func(t *testing.T, h *conversationHarness, _ func() *ConversationDefinitions) {
				rootID := h.c.GetRootSessionID()
				childID := saveUsageAgent(t, h.store, h.c.GetID(), rootID, "retired", nil)
				child, found, err := h.store.query.GetSession(t.Context(), h.c.GetID(), childID)
				if err != nil || !found {
					t.Fatalf("GetSession(child) = %v, %v", found, err)
				}
				h.impl().sessionRecords.Store(childID, child)
				want := `uses agent "retired", which is not configured`
				for _, param := range []*SendMessageParam{
					{
						SessionID: childID,
						Mode:      session.SessionInputModeNormal,
						Message:   "start",
					},
					{
						SessionID: childID,
						Mode:      session.SessionInputModeNormal,
						Message:   "switch model",
						Model:     "test/other",
					},
				} {
					if err := h.impl().sendMessage(t.Context(), h.record(t, rootID), param); err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("sendMessage(%+v) error = %v, want error containing %q", param, err, want)
					}
				}
				if _, err := h.c.GetSessionDetails(t.Context(), childID); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("GetSessionDetails(child) error = %v, want error containing %q", err, want)
				}
				if infos, err := h.c.ListSessionInfos(t.Context()); err != nil || len(infos) != 2 {
					t.Fatalf("ListSessionInfos() = %+v, %v; want root and child", infos, err)
				}
				id := h.input(t, session.SessionInputModeNormal, "still running")
				receiveSessionRequest(t, h.model, rootID).reply <- modelResponse{text: "yes"}
				if finished, _ := h.waitTurnFinished(t, id); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("root Turn = %+v, want completed", finished)
				}
			},
		},
		{
			name: "run_returns_after_reconfigure",
			run: func(t *testing.T, h *conversationHarness, newDefinitions func() *ConversationDefinitions) {
				if err := h.c.SetDefinitions(t.Context(), newDefinitions()); err != nil {
					t.Fatalf("SetDefinitions() error = %v", err)
				}
				if err := h.stop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() = %v, want context.Canceled", err)
				}
				for range h.allEvents {
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 用 local 和 virtual 两个执行环境启动对话，执行设置、定义或并发变更。
			store, model := openConversationDAO(t), newConversationModel(t)
			executors := append([]*defs.ExecutorDefinition{newLocalTestExecutor(t, "local")}, newTestExecutors("other")...)
			newDefinitions := func() *ConversationDefinitions {
				definitions := conversationDefinitions(model)
				definitions.Executors = executors
				return definitions
			}
			opt := conversationOpt(store, model)
			opt.Definitions = newDefinitions()
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			tt.run(t, startTestConversation(t, store, model, c), newDefinitions)
		})
	}
}

// getRootLifecycle 按事件顺序列出根 Session 的启动和停止。
func getRootLifecycle(rootID defs.SessionID, events []ConversationEvent) []string {
	// 1. 按事件顺序提取根 Session 的 started 和 stopped。
	var lifecycle []string
	for _, event := range events {
		switch event := event.(type) {
		case *ConversationSessionStarted:
			if event.Session.ID == rootID {
				lifecycle = append(lifecycle, "started")
			}
		case *ConversationSessionStopped:
			if event.SessionID == rootID {
				lifecycle = append(lifecycle, "stopped")
			}
		}
	}
	return lifecycle
}

// requireNoEvent 检查 50 毫秒内没有匹配的对话事件。
func requireNoEvent(t *testing.T, h *conversationHarness, match func(ConversationEvent) bool) {
	t.Helper()
	// 1. 50 毫秒内持续读取事件，读到匹配的事件时结束测试。
	timeout := time.After(50 * time.Millisecond)
	for {
		select {
		case event := <-h.allEvents:
			if match(event) {
				t.Fatalf("unexpected event %T %+v", event, event)
			}
		case <-timeout:
			return
		}
	}
}

// pingMCPTransport 每次 Connect 起一个进程内的 MCP server，只有一个回复 pong 的 ping 工具。
type pingMCPTransport struct{}

// Connect 创建带 ping 工具的进程内 MCP 连接。
func (pingMCPTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	// 1. 创建只返回 pong 的 ping 工具，通过进程内传输连接 server 和 client。
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "ping",
		Version: "1.0.0",
	}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "ping",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
		})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		return nil, err
	}
	return clientTransport.Connect(ctx)
}

// TestConversationReopenUsesSavedAgent 检查重开对话后子 Session 仍用保存的 Agent，这个 Agent 被删出定义时重开失败。
func TestConversationReopenUsesSavedAgent(t *testing.T) {
	// 1. 分别保留和删除 worker 定义；先创建 worker 子 Session 并停止对话。
	for _, tt := range []struct {
		name         string
		removeWorker bool
	}{
		{name: "child_keeps_worker_model"},
		{
			name:         "missing_saved_agent",
			removeWorker: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			c := createTestConversation(t, store, model)
			h := startTestConversation(t, store, model, c)
			childID, err := h.impl().spawnAgent(t.Context(), h.record(t, c.GetRootSessionID()), spawnCall("spawn-turn", "child"), &SpawnAgentParam{
				TaskName:  "survey",
				Message:   "first task",
				AgentName: "worker",
			})
			if err != nil {
				t.Fatal(err)
			}
			receiveSessionRequest(t, model, childID)
			h.stop(t)

			// 2. 保留或删除 worker 定义后重开，检查报错或子 Session 继续用 worker 的模型。
			opt := conversationOpt(store, model)
			if tt.removeWorker {
				opt.Definitions.Agents = opt.Definitions.Agents[:1]
			}
			loaded, err := reopenTestConversation(t, c, opt)
			if tt.removeWorker {
				if err == nil || !strings.Contains(err.Error(), `agent "worker"`) {
					t.Fatalf("GetConversation = %v, want missing worker error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			loadedHarness := startTestConversation(t, store, model, loaded)
			if err := loadedHarness.impl().sendMessage(t.Context(), loadedHarness.record(t, loaded.GetRootSessionID()), &SendMessageParam{
				SessionID: childID,
				Mode:      session.SessionInputModeNormal,
				Message:   "second task",
			}); err != nil {
				t.Fatal(err)
			}
			if request := receiveSessionRequest(t, model, childID); request.body["model"] != "worker-model" {
				t.Fatalf("reopened child model=%v, want worker-model", request.body["model"])
			}
		})
	}
}

// collaborationToolNames 是每个对话的 Session 固定带有的协作工具名，多个测试按它推算期望的工具集合。
var collaborationToolNames = []string{"spawn_agent", "send_message", "interrupt_agent", "list_agents", "read_session_history", "wait_agent"}

// allLocalToolNames 是测试环境 local 不按名单过滤时的全部内置工具名：执行环境带的 read_file、apply_patch，加上 Session 为有工作区的
// 执行环境建的五个命令工具。
var allLocalToolNames = []string{"read_file", "apply_patch", "bash", "read_command_output", "write_stdin", "stop_command", "list_commands"}

// TestConversationAgentTools 检查根 Agent、子 Agent 和重新打开后的子 Agent 按 ToolNames 拿到的工具。
func TestConversationAgentTools(t *testing.T) {
	// 1. 准备根 Agent、worker 和重新打开 worker 的工具预期。
	for _, tt := range []struct {
		name      string
		agentName string
		reopen    bool
		wantTools []string
	}{
		{
			name:      "root_without_tool_names",
			wantTools: allLocalToolNames,
		},
		{
			name:      "worker_selected_tools",
			agentName: "worker",
			wantTools: []string{"read_file"},
		},
		{
			name:      "reopened_worker_keeps_tools",
			agentName: "worker",
			reopen:    true,
			wantTools: []string{"read_file"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 配置工具名单并取得目标 Session 的模型请求。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			// local 带 read_file、apply_patch 和命令工具；assistant 不写 ToolNames，worker 只选 read_file。
			opt := conversationOpt(store, model)
			opt.Definitions.Executors = newTestExecutors("local")
			opt.Definitions.Agents[1].ToolNames = []string{"read_file"}
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			id := c.GetRootSessionID()
			var request modelRequest
			if tt.agentName == "" {
				h.input(t, session.SessionInputModeNormal, "list your tools")
				request = receiveSessionRequest(t, model, id)
			} else {
				id, err = h.impl().spawnAgent(t.Context(), h.record(t, c.GetRootSessionID()), spawnCall("spawn-turn", "spawn"), &SpawnAgentParam{
					TaskName:  "survey",
					Message:   "read the notes",
					AgentName: tt.agentName,
				})
				if err != nil {
					t.Fatal(err)
				}
				request = receiveSessionRequest(t, model, id)
			}
			if tt.reopen {
				h.stop(t)
				loaded, err := reopenTestConversation(t, c, opt)
				if err != nil {
					t.Fatal(err)
				}
				h = startTestConversation(t, store, model, loaded)
				if err := h.impl().sendMessage(t.Context(), h.record(t, loaded.GetRootSessionID()), &SendMessageParam{
					SessionID: id,
					Mode:      session.SessionInputModeNormal,
					Message:   "read again",
				}); err != nil {
					t.Fatal(err)
				}
				request = receiveSessionRequest(t, model, id)
			}
			// 3. 检查模型收到的工具是选中的内置工具加上全部协作工具。
			want := slices.Sorted(slices.Values(append(slices.Clone(tt.wantTools), collaborationToolNames...)))
			if got := slices.Sorted(slices.Values(requestToolNames(request))); !slices.Equal(got, want) {
				t.Fatalf("request tools=%v, want %v", got, want)
			}
		})
	}
}

// TestConversationAgentToolAndSkillNames 检查 Agent 用了未知工具或 Skill 时建对话失败，没有执行环境时不校验这两项。
func TestConversationAgentToolAndSkillNames(t *testing.T) {
	// 1. 准备未知工具、未知 Skill 和没有执行环境的情况。
	for _, tt := range []struct {
		name         string
		hasExecutors bool
		toolNames    []string
		skillNames   []string
		wantErr      string
	}{
		// 用户配置里的 Agent 大多写了工具和 Skill；没有 Executor 的对话照样能建，根 Session 只带协作工具。
		{
			name:       "without_executors_ignores_names",
			toolNames:  []string{"bash"},
			skillNames: []string{"x"},
		},
		{
			name:         "executor_rejects_unknown_tool",
			hasExecutors: true,
			toolNames:    []string{"search"},
			wantErr:      `agent "assistant" uses tool "search", which is not configured`,
		},
		{
			name:         "executor_rejects_unknown_skill",
			hasExecutors: true,
			skillNames:   []string{"x"},
			wantErr:      `agent "assistant" uses skill "x", which is not configured`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			mutation := &conversationMutation{Mutation: store.mutation}
			opt := conversationOpt(store, model)
			opt.Mutation = mutation
			opt.Definitions.Agents[0].ToolNames = tt.toolNames
			opt.Definitions.Agents[0].SkillNames = tt.skillNames
			if tt.hasExecutors {
				opt.Definitions.Executors = newTestExecutors("local")
			}
			// 2. 检查无效名称在保存前报错，无环境时模型只收到协作工具。
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr || mutation.sessionSaved {
					t.Fatalf("CreateConversation() error = %v, sessionSaved = %v; want %q before saving", err, mutation.sessionSaved, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			h.input(t, session.SessionInputModeNormal, "list your tools")
			request := receiveSessionRequest(t, model, c.GetRootSessionID())
			want := slices.Sorted(slices.Values(collaborationToolNames))
			if got := slices.Sorted(slices.Values(requestToolNames(request))); !slices.Equal(got, want) {
				t.Fatalf("request tools=%v, want %v", got, want)
			}
		})
	}
}

// TestConversationAgentSkills 检查不同 Agent 按 SkillNames 拿到的 Skill，以及系统提示里 Skill 说明块的位置和内容。
func TestConversationAgentSkills(t *testing.T) {
	// 1. 准备根 Agent、worker、重开后的 worker 和没有 Skill 的场景。
	for _, tt := range []struct {
		name       string
		agentName  string
		reopen     bool
		noCatalog  bool
		wantSkills []string
	}{
		{
			name:       "root_sees_all_skills",
			wantSkills: []string{"alpha", "beta"},
		},
		{
			name:       "worker_selected_skills",
			agentName:  "worker",
			wantSkills: []string{"beta"},
		},
		{
			name:       "reopened_worker_keeps_skills",
			agentName:  "worker",
			reopen:     true,
			wantSkills: []string{"beta"},
		},
		{
			name:      "empty_catalog_has_no_block",
			noCatalog: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 配置 Skill 目录和 Agent 名单，取得目标 Session 的请求。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			// skill 目录有 alpha、beta 两项；assistant 不写 SkillNames，worker 只选 beta。
			opt := conversationOpt(store, model)
			if !tt.noCatalog {
				opt.Definitions.Skills = []*skill.Skill{
					{
						Name:        "alpha",
						Description: "Alpha 技能",
						Path:        "/skills/alpha/SKILL.md",
					},
					{
						Name:        "beta",
						Description: "Beta 技能",
						Path:        "/skills/beta/SKILL.md",
					},
				}
				opt.Definitions.Agents[1].SkillNames = []string{"beta"}
			}
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			id := c.GetRootSessionID()
			var request modelRequest
			if tt.agentName == "" {
				h.input(t, session.SessionInputModeNormal, "use a skill")
				request = receiveSessionRequest(t, model, id)
			} else {
				id, err = h.impl().spawnAgent(t.Context(), h.record(t, c.GetRootSessionID()), spawnCall("spawn-turn", "spawn"), &SpawnAgentParam{
					TaskName:  "survey",
					Message:   "use a skill",
					AgentName: tt.agentName,
				})
				if err != nil {
					t.Fatal(err)
				}
				request = receiveSessionRequest(t, model, id)
			}
			if tt.reopen {
				h.stop(t)
				loaded, err := reopenTestConversation(t, c, opt)
				if err != nil {
					t.Fatal(err)
				}
				h = startTestConversation(t, store, model, loaded)
				if err := h.impl().sendMessage(t.Context(), h.record(t, loaded.GetRootSessionID()), &SendMessageParam{
					SessionID: id,
					Mode:      session.SessionInputModeNormal,
					Message:   "use it again",
				}); err != nil {
					t.Fatal(err)
				}
				request = receiveSessionRequest(t, model, id)
			}
			// 3. 核对 Skill 说明块的位置、名字和路径，没有 Skill 时不插入说明块。
			text := requestText(t, request)
			blockStart := strings.Index(text, "<openvaala-skills>")
			if len(tt.wantSkills) == 0 {
				if blockStart >= 0 {
					t.Fatal("empty skill catalog still inserted a skill block")
				}
				return
			}
			instructionsAt := strings.Index(text, "Test ")
			sessionAt := strings.Index(text, "<openvaala-session>")
			if blockStart < 0 || instructionsAt < 0 || !(instructionsAt < blockStart && blockStart < sessionAt) {
				t.Fatalf("skill block position: instructions=%d skills=%d session=%d", instructionsAt, blockStart, sessionAt)
			}
			block := text[blockStart:sessionAt]
			for _, name := range []string{"alpha", "beta"} {
				listed := strings.Contains(block, "- "+name+": ") && strings.Contains(block, "/skills/"+name+"/SKILL.md")
				if listed != slices.Contains(tt.wantSkills, name) {
					t.Fatalf("skill %s listed=%v, want %v in block %q", name, listed, slices.Contains(tt.wantSkills, name), block)
				}
			}
		})
	}
}

// TestConversationSystemInstructions 检查 Agent 指令、Skill、Executor 和 Session 说明拼成的完整系统消息。
func TestConversationSystemInstructions(t *testing.T) {
	// 1. 分别准备单个执行环境，以及带 Skill 和两个执行环境的系统消息。
	for _, tt := range []struct {
		name     string
		catalogs bool
	}{{
		"single_executor",
		false,
	}, {
		"skills_and_executors",
		true,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openConversationDAO(t), newConversationModel(t)
			opt := conversationOpt(store, model)
			opt.Definitions.Agents[0].Instructions = "Keep 100% <literal>\nnext"
			opt.Definitions.Executors = newTestExecutors("local")
			want := "Keep 100% <literal>\nnext"
			if tt.catalogs {
				opt.Definitions.Skills = []*skill.Skill{{
					Name:        "alpha",
					Description: "Alpha 技能",
					Path:        "/skills/alpha/SKILL.md",
				}}
				opt.Definitions.Executors = newTestExecutors("local", "sandbox")
				want += "\n<openvaala-skills>\n以下 skill 为特定任务提供说明。任务符合某项描述时，先用 read_file 完整读取它的 SKILL.md 再行动（条目带 executor 时，调用 read_file 传入同一 executor）；SKILL.md 中的相对路径以其所在目录为基准。\n- alpha: Alpha 技能\n  path: /skills/alpha/SKILL.md\n</openvaala-skills>"
			}
			want += "\n<openvaala-executors>\nExecutor 是工具实际运行的环境（一台机器或一个虚拟环境），各自有独立的文件系统和命令行：读写文件和运行命令只作用于所选 Executor，不同 Executor 之间的文件互不可见。可在多个 Executor 上运行的工具带有 executor 参数，用它选择执行位置；省略时使用参数说明中的默认 Executor。\n修改文件前，先用 read_file 读取目标文件，收到结果后再修改。\n- local（默认）: local 测试环境\n  workspace: /workspace/local\n"
			if tt.catalogs {
				want += "- sandbox: sandbox 测试环境\n  workspace: /workspace/sandbox\n"
			}
			want += "</openvaala-executors>"
			// 2. 启动对话并提交输入，核对模型收到的完整系统消息。
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			h.input(t, session.SessionInputModeNormal, "hello")
			request := receiveSessionRequest(t, model, c.GetRootSessionID())
			want += "\n<openvaala-session>\nSessionID: " + string(c.GetRootSessionID()) + "\nTaskName: \nParentSessionID: \n</openvaala-session>"
			message := request.body["messages"].([]any)[0].(map[string]any)
			if message["role"] != "system" || message["content"] != want {
				t.Fatalf("system = %#v, want %q", message, want)
			}
		})
	}
}

// TestConversationExecutorInstructions 检查系统提示里 Executor 说明块的位置，以及多个 Executor 时工具带 executor 参数。
func TestConversationExecutorInstructions(t *testing.T) {
	// 1. 分别配置一个或两个 Executor。
	for _, tt := range []struct {
		name        string
		executorIDs []string
	}{
		{
			name:        "two_executors_listed",
			executorIDs: []string{"local", "sandbox"},
		},
		{
			name:        "single_executor_listed",
			executorIDs: []string{"local"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 读取模型请求，核对说明块顺序、默认 Executor 和 executor 参数枚举。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			opt := conversationOpt(store, model)
			opt.Definitions.Skills = []*skill.Skill{{
				Name:        "alpha",
				Description: "Alpha 技能",
				Path:        "/skills/alpha/SKILL.md",
			}}
			opt.Definitions.Executors = newTestExecutors(tt.executorIDs...)
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			h.input(t, session.SessionInputModeNormal, "where am I")
			text := requestText(t, receiveSessionRequest(t, model, c.GetRootSessionID()))
			skillsEnd := strings.Index(text, "</openvaala-skills>")
			executorsAt := strings.Index(text, "<openvaala-executors>")
			sessionAt := strings.Index(text, "<openvaala-session>")
			if skillsEnd < 0 || !(skillsEnd < executorsAt && executorsAt < sessionAt) {
				t.Fatalf("executor block position: skills end=%d executors=%d session=%d", skillsEnd, executorsAt, sessionAt)
			}
			if !strings.Contains(text, "- local（默认）: local 测试环境") || !strings.Contains(text, "先用 read_file 读取目标文件，收到结果后再修改") {
				t.Fatalf("executor instructions missing: %s", text)
			}
			if strings.Contains(text, `"executor":`) != (len(tt.executorIDs) > 1) {
				t.Fatalf("executor parameter does not match configured environments: %s", text)
			}
			if len(tt.executorIDs) > 1 && !strings.Contains(text, `"enum":["local","sandbox"]`) {
				t.Fatalf("executor instructions or parameter missing: %s", text)
			}
		})
	}
}

// TestConversationReopenDoesNotReplay 检查重新打开对话时不重放历史子任务和排队输入，list_agents 和历史查询返回保存的状态。
func TestConversationReopenDoesNotReplay(t *testing.T) {
	// 1. 准备已完成或还有未完成任务的子 Session，停止对话、关闭并重新打开存储。
	for _, tt := range []struct {
		name      string
		completed bool
	}{
		{name: "reopen_with_unfinished_child"},
		{
			name:      "list_agents_after_reopen",
			completed: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			c := createTestConversation(t, store, model)
			h := startTestConversation(t, store, model, c)
			rootRecord := h.record(t, c.GetRootSessionID())
			childID, err := h.impl().spawnAgent(t.Context(), rootRecord, spawnCall("spawn-turn", "child"), &SpawnAgentParam{
				TaskName: "review",
				Message:  "saved task",
			})
			if err != nil {
				t.Fatal(err)
			}
			request := receiveSessionRequest(t, model, childID)
			if tt.completed {
				request.reply <- modelResponse{text: "saved final"}
				receiveSessionRequest(t, model, c.GetRootSessionID()).reply <- modelResponse{text: "saved root reply"}
				h.nextTurnFinished(t)
			} else if err := h.impl().sendMessage(t.Context(), rootRecord, &SendMessageParam{
				SessionID: childID,
				Mode:      session.SessionInputModeNormal,
				Message:   "queued task",
			}); err != nil {
				t.Fatal(err)
			}
			h.stop(t)
			if err := store.database.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openConversationDAOAt(t, store.root)
			loaded, err := reopenTestConversation(t, c, conversationOpt(reopened, model))
			if err != nil {
				t.Fatal(err)
			}
			// 2. 检查重开和历史查询不产生模型请求，list_agents 返回保存的子 Session 状态。
			requireNoModelRequest(t, model)
			loadedHarness := startTestConversation(t, reopened, model, loaded)
			agents, err := loadedHarness.impl().listAgents(t.Context(), loadedHarness.record(t, loaded.GetRootSessionID()))
			wantStatus := defs.TurnStatusInterrupted
			if tt.completed {
				wantStatus = defs.TurnStatusCompleted
			}
			if err != nil || len(agents) != 1 || agents[0].Session.ID != childID || agents[0].Status.State != session.SessionStateStopped ||
				agents[0].Status.LastTurnID == "" || agents[0].Status.LastTurnStatus != wantStatus {
				t.Fatalf("restored=%+v %v", agents, err)
			}
			history, err := loadedHarness.impl().readSessionHistory(t.Context(), loadedHarness.record(t, loaded.GetRootSessionID()), &SessionHistoryParam{
				SessionID: childID,
				Limit:     20,
			})
			if err != nil || len(history.Items) == 0 {
				t.Fatalf("saved history=%+v %v", history, err)
			}
			requireNoModelRequest(t, model)
			// 3. 提交新的根输入，检查正常完成且没有启动历史子 Session。
			id := loadedHarness.input(t, session.SessionInputModeNormal, "new explicit input")
			receiveSessionRequest(t, model, loaded.GetRootSessionID()).reply <- modelResponse{text: "new reply"}
			if finished, _ := loadedHarness.waitTurnFinished(t, id); finished.Status != defs.TurnStatusCompleted {
				t.Fatalf("finished=%+v", finished)
			}
			if _, found := loadedHarness.impl().runningSessions.Load(childID); found {
				t.Fatal("new root input restarted historical child")
			}
		})
	}
}

// conversationHarness 是运行中的测试对话，带着收集好的全部事件和根 Session 事件。
type conversationHarness struct {
	store *conversationStore
	model *conversationModel
	c     Conversation
	// events 是根 Session 的事件，由转发任务从 Run 的全部事件里拆出。
	events chan session.SessionEvent
	// allEvents 是 Run 发出的全部事件。
	allEvents chan ConversationEvent
	// cancel 取消 Run 的 ctx。
	cancel context.CancelFunc
	// done 收 Run 的返回值。
	done chan error
	// runErr 是 stop 第一次等到的 Run 返回值。
	runErr error
	// ended 是否已经调用过 stop？
	ended bool
}

// startTestConversation 启动测试对话，收集全部事件和根 Session 事件，等根 Session 首次启动后返回。
func startTestConversation(t *testing.T, store *conversationStore, model *conversationModel, c Conversation) *conversationHarness {
	t.Helper()
	// 1. 创建运行上下文和带缓冲的事件通道。
	ctx, cancel := context.WithCancel(t.Context())
	h := &conversationHarness{
		store:     store,
		model:     model,
		c:         c,
		events:    make(chan session.SessionEvent, 4096),
		allEvents: make(chan ConversationEvent, 4096),
		cancel:    cancel,
		done:      make(chan error, 1),
	}
	// 2. 启动 Run 和事件转发，分别收集全部事件和根 Session 事件。
	conversationEvents := make(chan ConversationEvent)
	go func() { h.done <- c.Run(ctx, conversationEvents) }()
	go func() {
		for event := range conversationEvents {
			h.allEvents <- event
			if sessionEvent, ok := event.(*ConversationSessionEvent); ok && sessionEvent.SessionID == c.GetRootSessionID() {
				h.events <- sessionEvent.Event
			}
		}
		close(h.allEvents)
		close(h.events)
	}()
	// 3. 登记测试清理，等根 Session 首次启动。
	t.Cleanup(func() { h.stop(t) })
	<-h.impl().rootStarted
	return h
}

// waitEvent 读到匹配的对话事件为止，返回它和之前读到的事件。
func (h *conversationHarness) waitEvent(t *testing.T, match func(ConversationEvent) bool) (ConversationEvent, []ConversationEvent) {
	t.Helper()
	// 1. 五秒内逐条读取全部事件，事件关闭或超时时结束测试。
	var seen []ConversationEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-h.allEvents:
			if !open {
				t.Fatalf("events closed before match; seen %d", len(seen))
			}
			if match(event) {
				return event, seen
			}
			seen = append(seen, event)
		case <-timeout:
			t.Fatalf("event did not arrive; seen %d", len(seen))
		}
	}
}

// impl 返回测试对话的实现实例。
func (h *conversationHarness) impl() *conversationImpl {
	// 1. 把 Conversation 转成实现类型。
	return h.c.(*conversationImpl)
}

// stop 取消对话并等待 Run 返回。
func (h *conversationHarness) stop(t *testing.T) error {
	t.Helper()
	// 1. 首次调用取消对话并等待 Run 返回，重复调用返回保存的结果。
	if !h.ended {
		h.cancel()
		h.runErr = waitRunDone(t, h.done)
		h.ended = true
	}
	return h.runErr
}

// record 取得测试对话内存里的 Session 记录。
func (h *conversationHarness) record(t *testing.T, id defs.SessionID) defs.Session {
	t.Helper()
	// 1. 从 sessionRecords 读取指定 Session，找不到时结束测试。
	record, found := h.impl().sessionRecords.Load(id)
	if !found {
		t.Fatalf("Session %s not found", id)
	}
	return record
}

// input 提交用户文本输入，返回接收的 ItemID。
func (h *conversationHarness) input(t *testing.T, mode session.SessionInputMode, text string) defs.ItemID {
	t.Helper()
	// 1. 提交指定模式的用户文本，检查接收成功且 ItemID 非空。
	id, err := h.c.HandleInput(t.Context(), conversationInput(mode, text))
	if err != nil || id == "" {
		t.Fatalf("HandleInput=%s %v", id, err)
	}
	return id
}

// nextTurnFinished 读取根 Session 下一个 Turn 结束事件，并返回期间完成的消息文本。
func (h *conversationHarness) nextTurnFinished(t *testing.T) (*session.SessionTurnFinished, []string) {
	t.Helper()
	// 1. 不指定输入 ID，等下一个根 Turn 结束。
	return h.waitTurnFinished(t, "")
}

// waitTurnFinished 等读入了 itemID 的根 Turn 结束，返回结束事件和期间完成的消息文本。
func (h *conversationHarness) waitTurnFinished(t *testing.T, itemID defs.ItemID) (*session.SessionTurnFinished, []string) {
	t.Helper()
	// 1. 五秒内收集已完成的消息，遇到 InputItemIDs 含 itemID 的 Turn 结束事件时返回；itemID 为空时任一轮都算。
	var completed []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-h.events:
			if !open {
				t.Fatalf("events closed before Turn of %s finished", itemID)
			}
			switch event := event.(type) {
			case *session.SessionAgentEvent:
				if event.Event.Type == agent.AgentEventItemCompleted && event.Event.Item != nil {
					completed = append(completed, itemText(event.Event.Item))
				}
			case *session.SessionTurnFinished:
				if itemID == "" || slices.Contains(event.InputItemIDs, itemID) {
					return event, completed
				}
			}
		case <-timeout:
			t.Fatalf("Turn of %s did not finish", itemID)
		}
	}
}

// ackUntilTurnFinished 一路用 ack 回复模型请求，直到读入了 itemID 的根 Turn 结束。
func (h *conversationHarness) ackUntilTurnFinished(t *testing.T, itemID defs.ItemID) *session.SessionTurnFinished {
	t.Helper()
	// 1. 五秒内用 ack 回复收到的模型请求，等到读入了 itemID 的根 Turn 结束事件时返回，超时结束测试。
	timeout := time.After(5 * time.Second)
	for {
		select {
		case request := <-h.model.requests:
			request.reply <- modelResponse{text: "ack"}
		case event := <-h.events:
			if finished, ok := event.(*session.SessionTurnFinished); ok && slices.Contains(finished.InputItemIDs, itemID) {
				return finished
			}
		case <-timeout:
			t.Fatalf("Turn of %s did not finish", itemID)
		}
	}
}

// waitAgentEvent 等根 Session 发出指定类型、指定 Item 种类的 Agent 事件。
func (h *conversationHarness) waitAgentEvent(t *testing.T, eventType agent.AgentEventType, kind defs.ItemKind) {
	t.Helper()
	// 1. 五秒内读取根 Session 事件，直到 Agent 事件类型和 Item 种类都匹配。
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-h.events:
			if agentEvent, ok := event.(*session.SessionAgentEvent); ok && agentEvent.Event.Type == eventType &&
				agentEvent.Event.Item != nil && agentEvent.Event.Item.Kind == kind {
				return
			}
		case <-timeout:
			t.Fatalf("Agent event %s of %s did not arrive", eventType, kind)
		}
	}
}

// waitRunDone 等待对话 Run 返回。
func waitRunDone(t *testing.T, done <-chan error) error {
	t.Helper()
	// 1. 五秒内等 Run 的返回值，超时结束测试。
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Conversation did not stop")
		return nil
	}
}

// conversationStore 是测试用的 SQLite 记录、文件日志和在它们上面建的 DAO。
type conversationStore struct {
	root     string
	query    dao.Query
	mutation dao.Mutation
	database storagesqlite.Database
	// logs 是 DAO 底下的日志存储，测试直接读日志原始行时用。
	logs storage.LogStorage
}

// openConversationDAO 在临时目录创建对话测试存储。
func openConversationDAO(t *testing.T) *conversationStore {
	t.Helper()
	// 1. 分配临时目录并打开测试存储。
	return openConversationDAOAt(t, t.TempDir())
}

// openConversationDAOAt 打开指定目录下的测试记录和日志存储，登记测试结束时关闭数据库。
func openConversationDAOAt(t *testing.T, root string) *conversationStore {
	t.Helper()
	// 1. 打开 SQLite 记录和文件日志，创建 DAO。
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
	if err != nil {
		t.Fatal(err)
	}
	q, m, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
	if err != nil {
		t.Fatal(err)
	}
	return &conversationStore{
		root:     root,
		query:    q,
		mutation: m,
		database: database,
		logs:     logs,
	}
}

// conversationOpt 组装测试对话的依赖：DAO、测试定义和图片大小上限。
func conversationOpt(store *conversationStore, model *conversationModel) *ConversationOpt {
	// 1. 填入 DAO、测试定义和 500 KiB 图片上限。
	return &ConversationOpt{
		Query:         store.query,
		Mutation:      store.mutation,
		Definitions:   conversationDefinitions(model),
		MaxImageBytes: 500 << 10,
	}
}

// reopenTestConversation 按保存的对话记录重新打开测试对话，返回还没启动的实例。
func reopenTestConversation(t *testing.T, c Conversation, opt *ConversationOpt) (Conversation, error) {
	t.Helper()
	// 1. 用 opt.Query 读取对话记录，按记录里的 Executor 设置重新打开。
	record, found, err := opt.Query.GetConversation(t.Context(), c.GetID())
	if err != nil || !found {
		t.Fatalf("GetConversation(%s) = %v, %v", c.GetID(), found, err)
	}
	return GetConversation(t.Context(), record, record.ExecutorSettings, opt)
}

// conversationDefinitions 返回测试用的 Agent 和模型定义，模型都指向受控模型服务。
func conversationDefinitions(model *conversationModel) *ConversationDefinitions {
	// 1. 创建 assistant、worker 两个 Agent 和四个模型定义，填写测试连接、窗口和强度。
	return &ConversationDefinitions{
		Agents: []*agent.AgentDefinition{
			{
				Name:         "assistant",
				Description:  "主 Agent",
				Instructions: "Test assistant",
				Model: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				},
			},
			{
				Name:         "worker",
				Description:  "只读调查",
				Instructions: "Test worker",
				Model: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "worker-model",
				},
			},
		},
		Models: []*defs.ModelDefinition{
			newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  model.server.URL,
				Model:    "test",
			}, 0),
			newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  model.server.URL,
				Model:    "worker-model",
			}, 0),
			&defs.ModelDefinition{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "other",
				},
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "other-upstream",
				},
				ContextWindow:          64000,
				ReasoningEfforts:       []string{"low", "high"},
				DefaultReasoningEffort: "low",
			},
			&defs.ModelDefinition{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "third",
				},
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "third-upstream",
				},
				ReasoningEfforts:       []string{"high", "medium"},
				DefaultReasoningEffort: "medium",
			},
		},
	}
}

// newTestModel 创建带连接参数和上下文窗口的测试模型。
func newTestModel(target *llmkit.Target, window int) *defs.ModelDefinition {
	// 1. 用 test/<上游模型名> 作为引用，填入连接和窗口；窗口为 0 时不压缩。
	return &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    target.Model,
		},
		Target:        target,
		ContextWindow: window,
	}
}

// createTestConversation 创建根 Agent 为 assistant 的测试对话。
func createTestConversation(t *testing.T, store *conversationStore, model *conversationModel) Conversation {
	t.Helper()
	// 1. 用默认模型和空的 Executor 设置创建对话，失败时结束测试。
	c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, conversationOpt(store, model))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// conversationInput 创建用户文本输入。
func conversationInput(mode session.SessionInputMode, text string) *ConversationInput {
	// 1. 填写输入模式、用户地址和文本内容。
	return &ConversationInput{
		Mode: mode,
		From: &defs.Address{
			Kind: defs.IdentityUser,
			ID:   "user",
		},
		Content: []*defs.Content{
			{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: text},
			},
		},
	}
}

// modelRequest 是受控模型服务收到的一次请求，测试往 reply 里写回复。
type modelRequest struct {
	body  map[string]any
	reply chan modelResponse
}

// modelResponse 是测试给受控模型服务的一次回复。
type modelResponse struct {
	// text、reasoning、calls 依次写成文本、推理和工具调用增量。
	text      string
	reasoning string
	calls     []llmkit.ToolCall
	// status 非 0 时直接回这个 HTTP 错误码。
	status int
	// hold 是否写完增量后挂住流，直到请求取消？
	hold bool
}

// conversationModel 是按测试指令回复的模型 HTTP 服务，收到的请求按顺序放进 requests。
type conversationModel struct {
	server   *httptest.Server
	requests chan modelRequest
}

// newConversationModel 启动按测试指令回复的模型 HTTP 服务。
func newConversationModel(t *testing.T) *conversationModel {
	t.Helper()
	// 1. 创建请求队列，启动 HTTP 服务并登记测试结束时关闭。
	model := &conversationModel{requests: make(chan modelRequest, 128)}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 解码模型请求交给测试，等测试的回复或请求取消。
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		request := modelRequest{
			body:  body,
			reply: make(chan modelResponse, 1),
		}
		model.requests <- request
		var response modelResponse
		select {
		case response = <-request.reply:
		case <-r.Context().Done():
			return
		}
		// 2. 按回复发送错误，或依次写出文本、推理和工具调用增量；hold 时挂住到请求取消，否则写结束标记。
		if response.status != 0 {
			http.Error(w, "injected model error", response.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if response.text != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", response.text)
		}
		if response.reasoning != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", response.reasoning)
		}
		for i, call := range response.calls {
			fmt.Fprintf(w,
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n",
				i, call.ID, call.Name, call.Arguments,
			)
		}
		if response.hold {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(model.server.Close)
	return model
}

// receiveModelRequest 等待并返回下一次模型请求。
func receiveModelRequest(t *testing.T, model *conversationModel) modelRequest {
	t.Helper()
	// 1. 五秒内取得下一次请求，超时结束测试。
	select {
	case request := <-model.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("model request did not arrive")
		return modelRequest{}
	}
}

// receiveSessionRequest 读取指定 Session 的下一次模型请求。
func receiveSessionRequest(t *testing.T, model *conversationModel, id defs.SessionID) modelRequest {
	t.Helper()
	// 1. 按系统提示里的 Session 身份块筛选请求，其他 Session 的请求一律回复 ack。
	for {
		request := receiveModelRequest(t, model)
		if strings.Contains(requestJSON(request), `\nSessionID: `+string(id)+`\n`) {
			return request
		}
		request.reply <- modelResponse{text: "ack"}
	}
}

// receiveRootAndTask 读取 spawn 之后同时到达的根 Session 请求和子任务首个请求。
func receiveRootAndTask(t *testing.T, model *conversationModel, task string) (root, child modelRequest) {
	t.Helper()
	// 1. 读两个请求，按任务名区分根和子任务，检查两者都到了。
	for range 2 {
		request := receiveModelRequest(t, model)
		if strings.Contains(requestJSON(request), "TaskName: "+task+`\n`) {
			child = request
		} else {
			root = request
		}
	}
	if root.reply == nil || child.reply == nil {
		t.Fatal("missing root or child request")
	}
	return root, child
}

// requireNoModelRequest 检查短时间内没有新模型请求。
func requireNoModelRequest(t *testing.T, model *conversationModel) {
	t.Helper()
	// 1. 观察 50 毫秒，出现模型请求时结束测试。
	select {
	case request := <-model.requests:
		t.Fatalf("unexpected model request: %s", requestJSON(request))
	case <-time.After(50 * time.Millisecond):
	}
}

// spawnResponse 创建模型调用 spawn_agent 的回复。
func spawnResponse(callID, task, message string) modelResponse {
	// 1. 编码任务名和内容，构造 spawn_agent 调用。
	arguments, _ := json.Marshal(SpawnAgentParam{
		TaskName: task,
		Message:  message,
	})
	return modelResponse{calls: []llmkit.ToolCall{{
		ID:        callID,
		Name:      "spawn_agent",
		Arguments: string(arguments),
	}}}
}

// decodeSpawnAgentResult 从 spawn_agent 的结果里取出子 Session ID。
func decodeSpawnAgentResult(t *testing.T, request modelRequest, callID string) defs.SessionID {
	t.Helper()
	// 1. 解码调用结果，检查 Status 为 accepted 且 SessionID 非空。
	var result spawnAgentResult
	decodeRequestToolResult(t, request, callID, &result)
	if result.SessionID == "" || result.Status != "accepted" {
		t.Fatalf("spawn_agent result=%+v", result)
	}
	return result.SessionID
}

// decodeRequestToolResult 解码模型请求中指定调用的工具结果。
func decodeRequestToolResult(t *testing.T, request modelRequest, callID string, target any) {
	t.Helper()
	// 1. 取得工具结果，检查没有工具错误后解码到 target。
	result := requestToolResult(t, request, callID)
	if result.Error != nil {
		t.Fatalf("tool %s failed: %+v", callID, result.Error)
	}
	if err := json.Unmarshal(result.Content, target); err != nil {
		t.Fatal(err)
	}
}

// requestToolResult 查找模型请求中指定 CallID 的工具结果。
func requestToolResult(t *testing.T, request modelRequest, callID string) defs.ToolResult {
	t.Helper()
	// 1. 遍历请求消息，找到 CallID 后解码工具结果，没找到时结束测试。
	for _, value := range request.body["messages"].([]any) {
		message := value.(map[string]any)
		if message["tool_call_id"] != callID {
			continue
		}
		var result defs.ToolResult
		if err := json.Unmarshal([]byte(message["content"].(string)), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	t.Fatalf("tool result %s missing", callID)
	return defs.ToolResult{}
}

// requestJSON 把模型请求正文编码成 JSON 文本。
func requestJSON(request modelRequest) string {
	// 1. 编码请求正文并转成字符串。
	data, _ := json.Marshal(request.body)
	return string(data)
}

// requestToolNames 返回一次模型请求携带的全部工具名。
func requestToolNames(request modelRequest) []string {
	// 1. 按请求里的工具顺序取出函数名。
	var names []string
	for _, value := range request.body["tools"].([]any) {
		names = append(names, value.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

// newLocalTestExecutor 创建使用临时根目录和 HOME 的 local Executor 定义。
func newLocalTestExecutor(t *testing.T, id string) *defs.ExecutorDefinition {
	t.Helper()
	// 1. 把 HOME 设为临时目录，工作区根目录用另一个临时目录。
	t.Setenv("HOME", t.TempDir())
	return &defs.ExecutorDefinition{
		ID:                 id,
		Kind:               defs.ExecutorKindLocal,
		Description:        id + " 测试环境",
		WorkspaceDirectory: t.TempDir(),
	}
}

// newFakeMCPServerDefinition 创建分配给指定 Executor 的测试 MCP server 定义。
func newFakeMCPServerDefinition(t *testing.T, serverID string, executorIDs ...string) *mcpclient.ServerDefinition {
	t.Helper()
	// 1. 配置 stdio 程序、Executor 名单和超时；serverID 为 hang 时加上不完成握手的参数。
	server := &mcpclient.ServerDefinition{
		ID:             serverID,
		Type:           mcpclient.ServerTransportStdio,
		Command:        buildFakeMCPServer(t),
		ExecutorIDs:    executorIDs,
		StartupTimeout: 20 * time.Second,
		ToolTimeout:    30 * time.Second,
	}
	if serverID == "hang" {
		server.Args = []string{"-hang"}
	}
	return server
}

// commandTransport 保存测试 MCP server 的启动命令和参数，每次 Connect 启动一个新进程。
type commandTransport struct {
	command   string
	arguments []string
}

// Connect 启动一个新的测试 MCP server 进程并连上它。
func (t *commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	// 1. 用保存的命令和参数创建进程，再建立连接。
	return (&mcp.CommandTransport{Command: exec.Command(t.command, t.arguments...)}).Connect(ctx)
}

// readFakeMCPServerPID 读取测试 MCP server 的进程号。
func readFakeMCPServerPID(t *testing.T, pidPath string) int {
	t.Helper()
	// 1. 读取 PID 文件并解析进程号，失败时结束测试。
	content, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(content))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// isProcessAlive 进程号 pid 的进程是否还在？
func isProcessAlive(pid int) bool {
	// 1. 发信号 0，没报错就说明进程还在。
	return syscall.Kill(pid, 0) == nil
}

// registerClientMCPTestServer 为运行中的对话登记客户端测试 MCP server。
func registerClientMCPTestServer(t *testing.T, c Conversation, serverID string) {
	t.Helper()
	// 1. 为客户端 c1 创建测试传输并登记 serverID；serverID 为 hang 时 server 不完成握手。
	transport := &commandTransport{command: buildFakeMCPServer(t)}
	if serverID == "hang" {
		transport.arguments = []string{"-hang"}
	}
	if err := c.SetClientMCPServers(t.Context(), "c1", []ClientMCPServer{{
		ID:        serverID,
		Transport: transport,
	}}); err != nil {
		t.Fatal(err)
	}
	// 2. 不是 hang 时等连接完成。
	if serverID != "hang" {
		waitMCPServerConnected(t, c, defs.ClientExecutorID, serverID)
	}
}

// waitMCPServerConnected 等待指定执行环境的 MCP server 连接完成。
func waitMCPServerConnected(t *testing.T, c Conversation, executorID, serverID string) {
	t.Helper()
	// 1. 五秒内轮询指定 Executor 上这个 server 的连接状态，超时结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		servers := c.GetMCPServers()
		if slices.ContainsFunc(servers, func(info MCPServerInfo) bool {
			return info.ExecutorID == executorID && info.ServerID == serverID && info.State == mcpclient.ConnectionStateConnected
		}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("MCP server %s on %s did not connect: %+v", serverID, executorID, servers)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newTestExecutors 按顺序创建指定 ID 的 virtual Executor 定义。
func newTestExecutors(ids ...string) []*defs.ExecutorDefinition {
	// 1. 按传入 ID 创建 virtual 定义，根目录为 /workspace/<id>。
	executors := make([]*defs.ExecutorDefinition, len(ids))
	for index, id := range ids {
		executors[index] = &defs.ExecutorDefinition{
			ID:                 id,
			Kind:               defs.ExecutorKindVirtual,
			Description:        id + " 测试环境",
			WorkspaceDirectory: "/workspace/" + id,
		}
	}
	return executors
}

// requestText 把模型请求编码成不转义 HTML 字符的 JSON 文本。
func requestText(t *testing.T, request modelRequest) string {
	t.Helper()
	// 1. 关闭 HTML 转义并编码请求，失败时结束测试。
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request.body); err != nil {
		t.Fatal(err)
	}
	return encoded.String()
}

// itemText 提取消息中的全部文本内容。
func itemText(item *defs.Item) string {
	// 1. 不是消息时返回空，否则拼接全部文本内容。
	if item.Message == nil {
		return ""
	}
	var text strings.Builder
	for _, content := range item.Message.Content {
		if content != nil && content.Text != nil {
			text.WriteString(content.Text.Text)
		}
	}
	return text.String()
}

// listSessionItems 按时间正序读取 Session 的全部 Item。
func listSessionItems(t *testing.T, store *conversationStore, conversationID defs.ConversationID, id defs.SessionID) []*defs.Item {
	t.Helper()
	// 1. 按时间正序扫描 Item，读取失败时结束测试。
	var items []*defs.Item
	for item, err := range store.query.ScanItems(t.Context(), conversationID, id, storage.DirectionAsc) {
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

// latestTurn 返回 Session 最新一条 Turn 记录。
func latestTurn(t *testing.T, store *conversationStore, id defs.SessionID) defs.Turn {
	t.Helper()
	// 1. 倒序查一条 Turn，没有时结束测试。
	page, err := store.query.ListTurns(t.Context(), dao.ListTurnsRequest{
		SessionID: id,
		Order:     storage.Order{Direction: storage.DirectionDesc},
		Limit:     1,
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("latest Turn of %s=%+v %v", id, page, err)
	}
	return page.Items[0]
}

// waitCompletedTurns 等 Session 正好有 count 个 Turn 且都已 completed，返回这些 Turn。
func waitCompletedTurns(t *testing.T, store *conversationStore, id defs.SessionID, count int) []defs.Turn {
	t.Helper()
	// 1. 五秒内轮询全部 Turn，记录数和 completed 数都等于 count 时返回。
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err := store.query.ListTurns(t.Context(), dao.ListTurnsRequest{
			SessionID: id,
			Order:     storage.Order{Direction: storage.DirectionAsc},
			Limit:     100,
		})
		if err != nil {
			t.Fatal(err)
		}
		completed := 0
		for _, turn := range page.Items {
			if turn.Status == defs.TurnStatusCompleted {
				completed++
			}
		}
		if completed == count && len(page.Items) == count {
			return page.Items
		}
		if time.Now().After(deadline) {
			t.Fatalf("Session %s Turns=%+v, want %d completed", id, page.Items, count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// conversationMutation 包住真实 DAO 写入，按 fail 注入根 Session 或对话记录的保存错误，并记下写过哪些记录。
type conversationMutation struct {
	dao.Mutation
	// fail 为 root 时保存 Session 失败，为 conversation 时保存对话记录失败。
	fail string
	// conversationSaved 是否保存过对话记录？
	conversationSaved bool
	// sessionSaved 是否保存过 Session 记录？
	sessionSaved bool
}

// SetSession 在 fail 为 root 时返回注入错误，否则记下已保存并交给 DAO。
func (m *conversationMutation) SetSession(ctx context.Context, s defs.Session) error {
	// 1. fail 为 root 时返回错误，否则标记已保存并交给 DAO。
	if m.fail == "root" {
		return errors.New("injected root error")
	}
	m.sessionSaved = true
	return m.Mutation.SetSession(ctx, s)
}

// SetConversation 在 fail 为 conversation 时返回注入错误，否则记下已保存并交给 DAO。
func (m *conversationMutation) SetConversation(ctx context.Context, c defs.Conversation) error {
	// 1. fail 为 conversation 时返回错误，否则标记已保存并交给 DAO。
	if m.fail == "conversation" {
		return errors.New("injected conversation error")
	}
	m.conversationSaved = true
	return m.Mutation.SetConversation(ctx, c)
}

// failingTurnQuery 查询指定 Session 的 Turn 时返回注入错误，这个 Session 启动时修复最近 Turn 就会失败。
type failingTurnQuery struct {
	dao.Query
	sessionID defs.SessionID
}

// ListTurns 查询指定 Session 时返回注入错误，其余照常查询。
func (q *failingTurnQuery) ListTurns(ctx context.Context, request dao.ListTurnsRequest) (storage.Page[defs.Turn, dao.RecordCursor[defs.TurnID]], error) {
	// 1. 是指定 Session 时返回注入错误，否则交给 DAO。
	if request.SessionID == q.sessionID {
		return storage.Page[defs.Turn, dao.RecordCursor[defs.TurnID]]{}, errors.New("injected list turns")
	}
	return q.Query.ListTurns(ctx, request)
}

// TestValidateSessionTree 检查 validateSessionTree 接受正常父子关系，拒绝父 Session 缺失和根 Session 带父级。
func TestValidateSessionTree(t *testing.T) {
	// 1. 准备正常父子关系、缺失父 Session 和根 Session 带父级的记录。
	conversation := defs.Conversation{
		ID:            "C",
		RootSessionID: "root",
	}
	child := func(parent defs.SessionID) defs.Session {
		return defs.Session{
			ID:              "child",
			ConversationID:  "C",
			ParentSessionID: parent,
			ParentTurnID:    "T",
			ParentCallID:    "call",
		}
	}
	tests := []struct {
		name    string
		records []defs.Session
		wantErr bool
	}{
		{
			name: "valid_tree",
			records: []defs.Session{{
				ID:             "root",
				ConversationID: "C",
			}, child("root")},
		},
		{
			name: "missing_parent",
			records: []defs.Session{{
				ID:             "root",
				ConversationID: "C",
			}, child("lost")},
			wantErr: true,
		},
		{
			name: "root_with_parent",
			records: []defs.Session{{
				ID:              "root",
				ConversationID:  "C",
				ParentSessionID: "child",
			}, child("root")},
			wantErr: true,
		},
	}
	// 2. 校验记录树并核对预期错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateSessionTree(conversation, tt.records); (err != nil) != tt.wantErr {
				t.Fatalf("validateSessionTree() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestConversationEvents 检查 Session 启动事件先于它的其他事件、子 Session 启动时带 fork 后的日志序号、根 Session 停止事件在最后。
func TestConversationEvents(t *testing.T) {
	// 1. 列出根 Session 启动、子 Session 带日志序号启动和根 Session 停止场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *conversationHarness)
	}{
		{
			name: "root_started_before_its_events",
			run: func(t *testing.T, h *conversationHarness) {
				rootID := h.c.GetRootSessionID()
				first, _ := h.waitEvent(t, func(ConversationEvent) bool { return true })
				started, ok := first.(*ConversationSessionStarted)
				if !ok || started.Session.ID != rootID || started.LastSavedOrdinal != 0 {
					t.Fatalf("first event = %T %+v", first, first)
				}
				h.input(t, session.SessionInputModeNormal, "hello")
				receiveModelRequest(t, h.model).reply <- modelResponse{text: "hi"}
				h.waitEvent(t, func(event ConversationEvent) bool { return isTurnFinishedOf(event, rootID) })
			},
		},
		{
			name: "child_started_with_fork_ordinal",
			run: func(t *testing.T, h *conversationHarness) {
				h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				childID := decodeSpawnAgentResult(t, root, "spawn")
				root.reply <- modelResponse{text: "delegated"}
				child.reply <- modelResponse{text: "child done"}
				_, seen := h.waitEvent(t, func(event ConversationEvent) bool { return isTurnFinishedOf(event, childID) })
				startedIndex, firstEventIndex := -1, -1
				for index, event := range seen {
					switch event := event.(type) {
					case *ConversationSessionStarted:
						if event.Session.ID == childID {
							startedIndex = index
							if event.LastSavedOrdinal != 1 || event.Session.ParentSessionID != h.c.GetRootSessionID() {
								t.Fatalf("child started = %+v", event)
							}
						}
					case *ConversationSessionEvent:
						if event.SessionID == childID && firstEventIndex == -1 {
							firstEventIndex = index
						}
					}
				}
				if startedIndex == -1 || firstEventIndex == -1 || startedIndex > firstEventIndex {
					t.Fatalf("child started at %d, first child event at %d", startedIndex, firstEventIndex)
				}
			},
		},
		{
			name: "root_only_stopped_is_last",
			run: func(t *testing.T, h *conversationHarness) {
				h.stop(t)
				var last ConversationEvent
				for event := range h.allEvents {
					last = event
				}
				stopped, ok := last.(*ConversationSessionStopped)
				if !ok || stopped.SessionID != h.c.GetRootSessionID() {
					t.Fatalf("last event = %T %+v", last, last)
				}
			},
		},
	}
	// 2. 每个场景启动独立对话执行检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			h := startTestConversation(t, store, model, createTestConversation(t, store, model))
			tt.run(t, h)
		})
	}
}

// isTurnFinishedOf 事件是否为指定 Session 的 Turn 结束通知？
func isTurnFinishedOf(event ConversationEvent, sessionID defs.SessionID) bool {
	// 1. 先看事件属不属于这个 Session，再看内层事件是不是 Turn 结束。
	sessionEvent, ok := event.(*ConversationSessionEvent)
	if !ok || sessionEvent.SessionID != sessionID {
		return false
	}
	_, finished := sessionEvent.Event.(*session.SessionTurnFinished)
	return finished
}

// TestConversationInterruptTurn 检查 InterruptTurn 取消根 Session 的活动 Turn 并返回它的 ID，空闲时返回空。
func TestConversationInterruptTurn(t *testing.T) {
	// 1. 分别准备执行中和空闲的根 Session。
	tests := []struct {
		name       string
		activeTurn bool
	}{
		{
			name:       "active_turn_interrupted",
			activeTurn: true,
		},
		{name: "idle_returns_empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			h := startTestConversation(t, store, model, createTestConversation(t, store, model))
			var id defs.ItemID
			if tt.activeTurn {
				id = h.input(t, session.SessionInputModeNormal, "long task")
				receiveModelRequest(t, h.model).reply <- modelResponse{
					text: "working",
					hold: true,
				}
				h.waitAgentEvent(t, agent.AgentEventItemStarted, defs.ItemMessage)
			}
			// 2. 请求取消，核对返回的 ID 和 interrupted 结果，空闲时应返回空 ID。
			turnID, err := h.c.InterruptTurn(t.Context())
			if err != nil || (turnID != "") != tt.activeTurn {
				t.Fatalf("InterruptTurn() = %q, %v", turnID, err)
			}
			if tt.activeTurn {
				finished, _ := h.waitTurnFinished(t, id)
				if finished.TurnID != turnID || finished.Status != defs.TurnStatusInterrupted {
					t.Fatalf("finished = %+v", finished)
				}
			}
		})
	}
}

// TestConversationListSessionInfosReportsUsageAndWindow 检查 ListSessionInfos 返回各 Session 已保存 Turn 的用量之和与所选模型的上下文窗口。
func TestConversationListSessionInfosReportsUsageAndWindow(t *testing.T) {
	// 1. 准备不同的 Turn 数、用量和 Agent 模型窗口样本。
	first := defs.TokenUsage{
		Input:      100,
		Output:     20,
		CacheRead:  60,
		CacheWrite: 10,
		Reasoning:  5,
	}
	second := defs.TokenUsage{
		Input:     40,
		Output:    8,
		CacheRead: 30,
		Reasoning: 2,
	}
	tests := []struct {
		name       string
		agentName  string
		turns      []defs.TokenUsage
		wantUsage  defs.TokenUsage
		wantWindow int
	}{
		{
			name:       "sums_saved_turn_usage",
			agentName:  "assistant",
			turns:      []defs.TokenUsage{first, second},
			wantUsage:  first.Add(second),
			wantWindow: 8000,
		},
		{
			name:       "turn_without_usage",
			agentName:  "assistant",
			turns:      []defs.TokenUsage{first, {}},
			wantUsage:  first,
			wantWindow: 8000,
		},
		{
			name:       "no_turns",
			agentName:  "assistant",
			wantWindow: 8000,
		},
		{
			name:      "agent_without_context_window",
			agentName: "worker",
			turns:     []defs.TokenUsage{second},
			wantUsage: second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 保存子 Session 的历史 Turn 并重开对话，查询后核对根和子 Session 的用量、窗口。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			opt := conversationOpt(store, model)
			opt.Definitions.Models[0].ContextWindow = 8000 // worker-model 的窗口为 0。
			created, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			id := saveUsageAgent(t, store, created.GetID(), created.GetRootSessionID(), tt.agentName, tt.turns)
			c, err := reopenTestConversation(t, created, opt)
			if err != nil {
				t.Fatal(err)
			}

			infos, err := c.ListSessionInfos(t.Context())
			if err != nil || len(infos) != 2 {
				t.Fatalf("ListSessionInfos() = %+v, %v", infos, err)
			}
			if root := infos[slices.IndexFunc(infos, func(info SessionInfo) bool { return info.Session.ID == c.GetRootSessionID() })]; root.Usage != (defs.TokenUsage{}) || root.ContextWindow != 8000 {
				t.Fatalf("root info = %+v", root)
			}
			got := infos[slices.IndexFunc(infos, func(info SessionInfo) bool { return info.Session.ID == id })]
			if got.Usage != tt.wantUsage || got.ContextWindow != tt.wantWindow {
				t.Fatalf("info = usage %+v window %d, want usage %+v window %d", got.Usage, got.ContextWindow, tt.wantUsage, tt.wantWindow)
			}
		})
	}
}

// TestConversationListSessionInfos 检查 ListSessionInfos 按 SessionID 列出根和子 Session，子 Session 带父 Session ID。
func TestConversationListSessionInfos(t *testing.T) {
	// 1. 准备只有根 Session 和带子 Session 的对话。
	tests := []struct {
		name      string
		spawn     bool
		wantCount int
	}{
		{
			name:      "root_only",
			wantCount: 1,
		},
		{
			name:      "root_and_child",
			spawn:     true,
			wantCount: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			h := startTestConversation(t, store, model, createTestConversation(t, store, model))
			if tt.spawn {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				root.reply <- modelResponse{text: "delegated"}
				h.waitTurnFinished(t, id)
				child.reply <- modelResponse{
					text: "child done",
					hold: true,
				}
			}
			// 2. 查询全部 Session，检查数量、根 ID 和子 Session 的父 ID。
			infos, err := h.c.ListSessionInfos(t.Context())
			if err != nil || len(infos) != tt.wantCount || infos[0].Session.ID != h.c.GetRootSessionID() {
				t.Fatalf("ListSessionInfos() = %+v, %v", infos, err)
			}
			if tt.spawn && infos[1].Session.ParentSessionID != h.c.GetRootSessionID() {
				t.Fatalf("child info = %+v", infos[1])
			}
		})
	}
}

// TestConversationGetSessionDetails 检查 GetSessionDetails 返回的模型、Executor、工具和 Skill，以及它不等 MCP 连接、不请求模型。
func TestConversationGetSessionDetails(t *testing.T) {
	// 1. 准备模型、工具、Skill、MCP 连接和不在对话里的 Session 的详情预期。
	tests := []struct {
		name          string
		toolNames     []string                // assistant 的 ToolNames
		skillNames    []string                // assistant 的 SkillNames
		mcpServer     string                  // 非空时 local 带一个这个 ID 的假 MCP server；hang 的握手永远不完成，其余是正常的 server
		mcpServerIDs  defs.OptionalStringList // assistant 的 MCPServerIDs
		wantMCPTools  bool                    // 详情里是否应有 fake server 的全部 MCP 工具？
		clientServer  string                  // 非空时客户端 Executor 登记一个这个 ID 的假 MCP server；hang 的握手永远不完成，其余是正常的 server
		wantClientMCP bool                    // 详情里是否应有 client-fake server 的全部 MCP 工具？
		childAgent    string                  // 非空时查询用这个 Agent 保存的子 Session，否则查询根 Session
		selectModel   defs.ModelSelection     // 非零时先把根 Session 切换到这个选择并重新加载对话
		removedModel  bool                    // 是否把根 Session 记录里的模型改成目录里没有的再重新加载对话？
		unknown       bool
		wantModel     string // 模型引用；空表示详情里没有 Model 和 CompactModel
		wantCompact   string // 摘要模型引用
		wantWindow    int
		wantTools     []string
		wantSkills    []string
		wantErrSubstr string
	}{
		{
			name:        "root_selected_tools_and_skills",
			toolNames:   []string{"read_file"},
			skillNames:  []string{"alpha"},
			wantModel:   "test/test",
			wantCompact: "test/compact-model",
			wantWindow:  8000,
			wantTools:   []string{"read_file"},
			wantSkills:  []string{"alpha"},
		},
		{
			name:        "tool_names_select_command_tools_one_by_one",
			toolNames:   []string{"read_file", "bash"},
			wantModel:   "test/test",
			wantCompact: "test/compact-model",
			wantWindow:  8000,
			wantTools:   []string{"read_file", "bash"},
			wantSkills:  []string{"alpha", "beta"},
		},
		{
			name:        "root_without_tool_names_lists_all_executor_tools",
			wantModel:   "test/test",
			wantCompact: "test/compact-model",
			wantWindow:  8000,
			wantTools:   allLocalToolNames,
			wantSkills:  []string{"alpha", "beta"},
		},
		{
			name:        "child_uses_its_own_definition",
			childAgent:  "worker",
			wantModel:   "test/worker-model",
			wantCompact: "test/worker-model",
			wantWindow:  0,
			wantTools:   allLocalToolNames,
			wantSkills:  []string{"alpha", "beta"},
		},
		{
			// 摘要模型窗口（8000）小于所选模型，按 defs.GetCompactModel 退回所选模型。
			name:        "selected_model_replaces_agent_default",
			selectModel: defs.ModelSelection{Ref: otherModelRef},
			wantModel:   "test/other",
			wantCompact: "test/other",
			wantWindow:  64000,
			wantTools:   allLocalToolNames,
			wantSkills:  []string{"alpha", "beta"},
		},
		{
			name:         "removed_model_has_no_model_details",
			removedModel: true,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:          "session_outside_conversation",
			unknown:       true,
			wantErrSubstr: "is not in Conversation",
		},
		{
			name:        "hanging_mcp_server_is_not_waited_for",
			mcpServer:   "hang",
			wantModel:   "test/test",
			wantCompact: "test/compact-model",
			wantWindow:  8000,
			wantTools:   allLocalToolNames,
			wantSkills:  []string{"alpha", "beta"},
		},
		{
			name:         "connected_mcp_server_tools_are_listed_without_mcp_servers",
			mcpServer:    "fake",
			wantMCPTools: true,
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:         "empty_mcp_servers_lists_no_mcp_tools",
			mcpServer:    "fake",
			mcpServerIDs: defs.OptionalStringList{},
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:         "mcp_servers_lists_only_selected_servers",
			mcpServer:    "fake",
			mcpServerIDs: defs.OptionalStringList{"other"},
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:         "mcp_servers_naming_the_server_lists_its_tools",
			mcpServer:    "fake",
			mcpServerIDs: defs.OptionalStringList{"fake"},
			wantMCPTools: true,
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:         "tool_names_do_not_filter_mcp_tools",
			mcpServer:    "fake",
			toolNames:    []string{"read_file"},
			wantMCPTools: true,
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    []string{"read_file"},
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:          "connected_client_mcp_tools_are_listed_without_mcp_servers",
			clientServer:  "client-fake",
			wantClientMCP: true,
			wantModel:     "test/test",
			wantCompact:   "test/compact-model",
			wantWindow:    8000,
			wantTools:     allLocalToolNames,
			wantSkills:    []string{"alpha", "beta"},
		},
		{
			name:         "mcp_servers_list_excludes_client_mcp_tools",
			clientServer: "client-fake",
			mcpServerIDs: defs.OptionalStringList{"client-fake"},
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:         "empty_mcp_servers_excludes_client_mcp_tools",
			clientServer: "client-fake",
			mcpServerIDs: defs.OptionalStringList{},
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
		{
			name:         "hanging_client_mcp_server_is_not_waited_for",
			clientServer: "hang",
			wantModel:    "test/test",
			wantCompact:  "test/compact-model",
			wantWindow:   8000,
			wantTools:    allLocalToolNames,
			wantSkills:   []string{"alpha", "beta"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 配置执行环境、Skill、压缩模型和 Agent 名单，创建对话。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			opt := conversationOpt(store, model)
			localExecutor := newTestExecutors("local")[0]
			if tt.mcpServer != "" {
				localExecutor = newLocalTestExecutor(t, "local")
				opt.Definitions.MCPServers = []*mcpclient.ServerDefinition{newFakeMCPServerDefinition(t, tt.mcpServer, "local")}
			}
			opt.Definitions.Executors = append([]*defs.ExecutorDefinition{localExecutor}, newTestExecutors("sandbox")...)
			opt.Definitions.Skills = []*skill.Skill{
				{
					Name:        "alpha",
					Description: "Alpha 技能",
					Path:        "/skills/alpha/SKILL.md",
				},
				{
					Name:        "beta",
					Description: "Beta 技能",
					Path:        "/skills/beta/SKILL.md",
				},
			}
			opt.Definitions.Models[0].ContextWindow = 8000 // worker-model 保持 0
			// 配置窗口为 8000 的压缩模型，和默认主模型的窗口一样大。
			compactModel := newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  model.server.URL,
				Model:    "compact-model",
			}, 8000)
			opt.Definitions.Models = append(opt.Definitions.Models, compactModel)
			opt.Definitions.Agents[0].CompactModel = compactModel.Ref
			opt.Definitions.Agents[0].ToolNames = tt.toolNames
			opt.Definitions.Agents[0].SkillNames = tt.skillNames
			opt.Definitions.Agents[0].MCPServerIDs = tt.mcpServerIDs
			// 握手永远不完成的 server 启动超时 20 秒：打开对话若等它，会远超 10 秒。
			openStart := time.Now()
			created, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(openStart); elapsed > 10*time.Second {
				t.Fatalf("CreateConversation() took %v, want it to open without waiting for MCP connections", elapsed)
			}
			// 对话记录带标题，GetSessionDetails 也不填 Title，由 Runtime 以已保存的记录为准填入。
			record, found, err := store.query.GetConversation(t.Context(), created.GetID())
			if err != nil || !found {
				t.Fatalf("GetConversation() = %v, %v", found, err)
			}
			record.Title = "design review"
			if err := store.mutation.SetConversation(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			// 3. 选择查询目标，保存模型选择或缺失模型，再重新打开对话。
			sessionID := created.GetRootSessionID()
			if tt.childAgent != "" {
				sessionID = saveUsageAgent(t, store, created.GetID(), created.GetRootSessionID(), tt.childAgent, nil)
			}
			if tt.unknown {
				sessionID = defs.NewSessionID()
			}
			// 保存模型选择；之后重开对话，读到的是存储填了 UpdatedAt 的 Session 记录。
			if tt.selectModel != (defs.ModelSelection{}) {
				if _, err := created.SetSessionModel(t.Context(), sessionID, tt.selectModel); err != nil {
					t.Fatal(err)
				}
			}
			if tt.removedModel {
				record, found, err := store.query.GetSession(t.Context(), created.GetID(), sessionID)
				if err != nil || !found {
					t.Fatalf("GetSession() = %v, %v", found, err)
				}
				record.Model = defs.ModelSelection{Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "gone",
				}}
				if err := store.mutation.SetSession(t.Context(), record); err != nil {
					t.Fatal(err)
				}
			}
			c, err := reopenTestConversation(t, created, opt)
			if err != nil {
				t.Fatal(err)
			}
			// 执行环境和客户端登记的 MCP 连接随对话运行：先运行对话，再等 server 连上；握手永远不完成的 server 不等。
			if tt.mcpServer != "" || tt.clientServer != "" {
				startTestConversation(t, store, model, c)
			}
			if tt.mcpServer != "" && tt.mcpServer != "hang" {
				waitMCPServerConnected(t, c, "local", tt.mcpServer)
			}
			if tt.clientServer != "" {
				registerClientMCPTestServer(t, c, tt.clientServer)
			}

			// 4. 查询详情，检查耗时、身份、公开模型、Executor、Skill 和工具，且没有模型请求。
			detailsStart := time.Now()
			got, err := c.GetSessionDetails(t.Context(), sessionID)
			if elapsed := time.Since(detailsStart); elapsed > time.Second {
				t.Fatalf("GetSessionDetails() took %v, want it to return without waiting for MCP connections", elapsed)
			}
			if tt.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("GetSessionDetails() = %+v, %v, want error containing %q", got, err, tt.wantErrSubstr)
				}
				requireNoModelRequest(t, model)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantSession, found, err := store.query.GetSession(t.Context(), created.GetID(), sessionID)
			if err != nil || !found {
				t.Fatalf("GetSession() = %v, %v", found, err)
			}
			if got.ConversationID != created.GetID() || got.Title != "" || got.Session != wantSession {
				t.Fatalf("identity = %q %q %+v, want %q %q %+v", got.ConversationID, got.Title, got.Session, created.GetID(), "", wantSession)
			}
			if tt.wantModel == "" {
				if got.Model != nil || got.CompactModel != nil {
					t.Fatalf("model = %+v compact %+v, want none", got.Model, got.CompactModel)
				}
			} else if got.Model.Target != nil || got.CompactModel.Target != nil {
				t.Fatal("session details expose model connection parameters")
			} else if got.Model.Ref.String() != tt.wantModel || got.CompactModel.Ref.String() != tt.wantCompact || got.Model.ContextWindow != tt.wantWindow {
				t.Fatalf("model = %s compact %s window %d, want %s %s %d", got.Model.Ref, got.CompactModel.Ref, got.Model.ContextWindow, tt.wantModel, tt.wantCompact, tt.wantWindow)
			}
			wantExecutors := []ExecutorDetails{
				{
					ID:                 "local",
					Description:        "local 测试环境",
					WorkspaceDirectory: localExecutor.WorkspaceDirectory,
				},
				{
					ID:                 "sandbox",
					Description:        "sandbox 测试环境",
					WorkspaceDirectory: "/workspace/sandbox",
				},
			}
			if !slices.Equal(got.Executors, wantExecutors) {
				t.Fatalf("executors = %+v, want %+v", got.Executors, wantExecutors)
			}
			if !slices.Equal(got.Skills, tt.wantSkills) {
				t.Fatalf("skills = %v, want %v", got.Skills, tt.wantSkills)
			}
			wantTools := append(slices.Clone(tt.wantTools), collaborationToolNames...)
			if tt.wantMCPTools {
				for _, name := range []string{"echo", "fail", "slow", "image", "big", "structured", "argv", "pid", "change"} {
					wantTools = append(wantTools, "mcp__fake__"+name)
				}
			}
			if tt.wantClientMCP {
				for _, name := range []string{"echo", "fail", "slow", "image", "big", "structured", "argv", "pid", "change"} {
					wantTools = append(wantTools, "mcp__client-fake__"+name)
				}
			}
			wantTools = slices.Sorted(slices.Values(wantTools))
			if gotTools := slices.Sorted(slices.Values(got.Tools)); !slices.Equal(gotTools, wantTools) {
				t.Fatalf("tools = %v, want %v", gotTools, wantTools)
			}
			requireNoModelRequest(t, model)
		})
	}
}

// testModelRef、otherModelRef、thirdModelRef、workerModelRef 是 conversationDefinitions 里四个测试模型的引用。
var (
	testModelRef = defs.ModelRef{
		ProviderID: "test",
		ModelID:    "test",
	}
	otherModelRef = defs.ModelRef{
		ProviderID: "test",
		ModelID:    "other",
	}
	thirdModelRef = defs.ModelRef{
		ProviderID: "test",
		ModelID:    "third",
	}
	workerModelRef = defs.ModelRef{
		ProviderID: "test",
		ModelID:    "worker-model",
	}
)

// failingSessionMutation 让保存 Session 记录失败，其余写入交给 DAO。
type failingSessionMutation struct{ dao.Mutation }

// SetSession 返回注入的 Session 保存错误。
func (failingSessionMutation) SetSession(context.Context, defs.Session) error {
	// 1. 返回 disk full。
	return errors.New("disk full")
}

// TestConversationSetSessionModel 检查 SetSessionModel 的强度选择、失败时内存和持久记录都不变、运行中的 Session 下一轮用新模型。
func TestConversationSetSessionModel(t *testing.T) {
	// 1. 列出强度选择、模型不在目录里和保存失败的情况。
	tests := []struct {
		name     string
		current  defs.ModelSelection // 先切换到这个选择；零值表示保持建对话时的默认
		request  defs.ModelSelection
		failSave bool
		want     defs.ModelSelection
		wantErr  string
	}{
		{
			name: "explicit_effort",
			request: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			want: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
		},
		{
			name:    "default_effort",
			request: defs.ModelSelection{Ref: otherModelRef},
			want: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "low",
			},
		},
		{
			name: "keep_supported_effort",
			current: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			request: defs.ModelSelection{Ref: thirdModelRef},
			want: defs.ModelSelection{
				Ref:             thirdModelRef,
				ReasoningEffort: "high",
			},
		},
		{
			name: "reset_unsupported_effort",
			current: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "low",
			},
			request: defs.ModelSelection{Ref: thirdModelRef},
			want: defs.ModelSelection{
				Ref:             thirdModelRef,
				ReasoningEffort: "medium",
			},
		},
		{
			name: "model_without_efforts",
			current: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			request: defs.ModelSelection{Ref: testModelRef},
			want:    defs.ModelSelection{Ref: testModelRef},
		},
		{
			name: "unsupported_effort",
			request: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "max",
			},
			wantErr: `does not support reasoning effort "max"`,
		},
		{
			name: "unknown_model",
			request: defs.ModelSelection{Ref: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "gone",
			}},
			wantErr: "test/gone",
		},
		{
			name:    "zero_model",
			request: defs.ModelSelection{},
			wantErr: "is not configured",
		},
		{
			name:     "save_failure_keeps_memory",
			request:  defs.ModelSelection{Ref: otherModelRef},
			failSave: true,
			wantErr:  "disk full",
		},
	}
	// 2. 逐个用例设置原模型并提交新选择，核对返回值、内存和持久记录，失败时检查选择没变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			c := createTestConversation(t, store, newConversationModel(t))
			impl, rootID := c.(*conversationImpl), c.GetRootSessionID()
			if tt.current != (defs.ModelSelection{}) {
				if _, err := c.SetSessionModel(t.Context(), rootID, tt.current); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := impl.sessionRecords.Load(rootID)
			if tt.failSave {
				impl.opt.Mutation = failingSessionMutation{impl.opt.Mutation}
			}
			got, err := c.SetSessionModel(t.Context(), rootID, tt.request)
			after, _ := impl.sessionRecords.Load(rootID)
			stored, found, queryErr := store.query.GetSession(t.Context(), c.GetID(), rootID)
			if queryErr != nil || !found {
				t.Fatalf("stored root = %v, %v", found, queryErr)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || after.Model != before.Model || stored.Model != before.Model {
					t.Fatalf("SetSessionModel() = %+v, %v; memory %+v stored %+v, want unchanged %+v", got.Model, err, after.Model, stored.Model, before.Model)
				}
				return
			}
			if err != nil || got.Model != tt.want || after.Model != tt.want || stored.Model != tt.want {
				t.Fatalf("SetSessionModel() = %+v, %v; memory %+v stored %+v, want %+v", got.Model, err, after.Model, stored.Model, tt.want)
			}
		})
	}
	// 3. 检查不在对话里的 Session 返回错误。
	t.Run("unknown_session", func(t *testing.T) {
		c := createTestConversation(t, openConversationDAO(t), newConversationModel(t))
		if _, err := c.SetSessionModel(t.Context(), defs.NewSessionID(), defs.ModelSelection{Ref: otherModelRef}); err == nil || !strings.Contains(err.Error(), "is not in Conversation") {
			t.Fatalf("SetSessionModel(unknown) = %v", err)
		}
	})
	// 4. 给运行中的 Session 换模型，检查下一轮请求用新模型和强度。
	t.Run("running_session_uses_new_model_next_turn", func(t *testing.T) {
		store, model := openConversationDAO(t), newConversationModel(t)
		c := createTestConversation(t, store, model)
		h := startTestConversation(t, store, model, c)
		if _, err := c.SetSessionModel(t.Context(), c.GetRootSessionID(), defs.ModelSelection{
			Ref:             otherModelRef,
			ReasoningEffort: "high",
		}); err != nil {
			t.Fatal(err)
		}
		h.input(t, session.SessionInputModeNormal, "你好")
		request := receiveSessionRequest(t, model, c.GetRootSessionID())
		if request.body["model"] != "other-upstream" || request.body["reasoning_effort"] != "high" {
			t.Fatalf("request model = %v effort = %v", request.body["model"], request.body["reasoning_effort"])
		}
		request.reply <- modelResponse{text: "好"}
	})
}

// TestConversationCreateWithModel 检查创建对话时根 Session 保存的模型和推理强度，以及无效强度或模型的错误。
func TestConversationCreateWithModel(t *testing.T) {
	// 1. 准备默认模型、指定模型、无效强度和不在目录里的模型。
	tests := []struct {
		name    string
		request defs.ModelSelection
		want    defs.ModelSelection
		wantErr string
	}{
		{
			name: "agent_default",
			want: defs.ModelSelection{Ref: testModelRef},
		},
		{
			name: "explicit_model_and_effort",
			request: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			want: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
		},
		{
			name:    "explicit_model_default_effort",
			request: defs.ModelSelection{Ref: otherModelRef},
			want: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "low",
			},
		},
		{
			name:    "default_model_with_unsupported_effort",
			request: defs.ModelSelection{ReasoningEffort: "high"},
			wantErr: "does not support reasoning effort",
		},
		{
			name: "unknown_model",
			request: defs.ModelSelection{Ref: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "gone",
			}},
			wantErr: "test/gone",
		},
	}
	// 2. 创建对话，核对错误或根 Session 保存的选择。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openConversationDAO(t), newConversationModel(t)
			c, err := CreateConversation(t.Context(), "assistant", tt.request, defs.ExecutorSettings{}, defs.ExecutorSettings{}, conversationOpt(store, model))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("CreateConversation() = %v, want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root, found, err := store.query.GetSession(t.Context(), c.GetID(), c.GetRootSessionID())
			if err != nil || !found || root.Model != tt.want {
				t.Fatalf("root = %+v, %v, %v; want model %+v", root, found, err, tt.want)
			}
		})
	}
}

// TestValidateConversationConfigModels 检查 Agent 的默认模型和压缩模型都在目录里、每个模型都带连接参数。
func TestValidateConversationConfigModels(t *testing.T) {
	// 1. 准备默认模型不在目录里、压缩模型不在目录里和模型没有连接参数的情况。
	model := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "a",
		},
		Target:        &llmkit.Target{},
		ContextWindow: 1000,
	}
	tests := []struct {
		name    string
		agents  []*agent.AgentDefinition
		models  []*defs.ModelDefinition
		wantErr string
	}{
		{
			name: "valid",
			agents: []*agent.AgentDefinition{{
				Name:  "assistant",
				Model: model.Ref,
			}},
			models: []*defs.ModelDefinition{model},
		},
		{
			name: "missing_default_model",
			agents: []*agent.AgentDefinition{{
				Name: "assistant",
				Model: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "gone",
				},
			}},
			models:  []*defs.ModelDefinition{model},
			wantErr: "test/gone",
		},
		{
			name: "missing_compact_model",
			agents: []*agent.AgentDefinition{{
				Name:  "assistant",
				Model: model.Ref,
				CompactModel: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "gone",
				},
			}},
			models:  []*defs.ModelDefinition{model},
			wantErr: "test/gone",
		},
		{
			name: "model_without_target",
			agents: []*agent.AgentDefinition{{
				Name:  "assistant",
				Model: model.Ref,
			}},
			models:  []*defs.ModelDefinition{{Ref: model.Ref}},
			wantErr: "connection parameters",
		},
	}
	// 2. 校验配置并核对错误内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := validateConversationConfig(t.Context(), &ConversationDefinitions{
				Agents: tt.agents,
				Models: tt.models,
			}, nil, nil)
			if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("validateConversationConfig() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// conversationHooksStub 是 hook.Hooks 替身：只记下 SessionStart、SubagentStart 事件，不回任何决定。
type conversationHooksStub struct {
	mutex  sync.Mutex
	starts []*hook.HookEvent
}

// RunEvent 记录 SessionStart 和 SubagentStart 事件。
func (stub *conversationHooksStub) RunEvent(_ context.Context, request *hook.HookEventRequest, _ chan<- *defs.HookRun) *hook.HookOutcome {
	// 1. 记下 SessionStart 和 SubagentStart 事件，返回空结果。
	if request.Event.Name == defs.HookEventSessionStart || request.Event.Name == defs.HookEventSubagentStart {
		stub.mutex.Lock()
		stub.starts = append(stub.starts, request.Event)
		stub.mutex.Unlock()
	}
	return &hook.HookOutcome{}
}

// GetHookInfos 返回测试 hook 信息。
func (*conversationHooksStub) GetHookInfos() []*hook.HookInfo {
	// 1. 返回空列表。
	return nil
}

// getStarts 汇总已记录的 Session 启动事件。
func (stub *conversationHooksStub) getStarts(rootSessionID defs.SessionID) []string {
	// 1. 把记下的事件格式化成「角色:事件:来源」，根 Session 写 root，子 Session 写 child。
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	var starts []string
	for _, event := range stub.starts {
		role := "child"
		if event.Session.ID == rootSessionID {
			role = "root"
		}
		starts = append(starts, fmt.Sprintf("%s:%s:%s", role, event.Name, event.Source))
	}
	return starts
}

// TestConversationHookStartSource 检查根 Session 和子 Session 首次启动、重开后各自触发的启动 hook 和来源。
func TestConversationHookStartSource(t *testing.T) {
	// 1. 列出新根、重开根、新子和重开子的启动 hook 预期。
	tests := []struct {
		name string
		// isChild 是否由 spawn_agent 建一个子 Session 来触发 Turn？否则由根 Session 收用户输入触发。
		isChild bool
		// isReopened 是否在第一次触发后停止对话、重开，再触发一次？根 Session 再收一条输入，子 Session 再收一条消息。
		isReopened bool
		wantStarts []string
	}{
		{
			name:       "new_root_session_is_startup",
			wantStarts: []string{"root:SessionStart:startup"},
		},
		{
			name:       "reopened_root_session_with_items_is_resume",
			isReopened: true,
			wantStarts: []string{"root:SessionStart:startup", "root:SessionStart:resume"},
		},
		{
			name:       "spawned_child_session_runs_subagent_start",
			isChild:    true,
			wantStarts: []string{"child:SubagentStart:"},
		},
		{
			name:       "reopened_child_session_does_not_run_again",
			isChild:    true,
			isReopened: true,
			wantStarts: []string{"child:SubagentStart:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 配置 hook，触发根或子 Session 的首轮，按场景重开后再提交一次输入。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			stub := &conversationHooksStub{}
			newOpt := func() *ConversationOpt {
				opt := conversationOpt(store, model)
				opt.Definitions.Executors = newTestExecutors("local")
				opt.Definitions.Hooks = stub
				return opt
			}
			c, err := CreateConversation(t.Context(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, newOpt())
			if err != nil {
				t.Fatal(err)
			}
			rootID := c.GetRootSessionID()
			h := startTestConversation(t, store, model, c)

			var childID defs.SessionID
			if tt.isChild {
				childID, err = h.impl().spawnAgent(t.Context(), h.record(t, rootID), spawnCall("spawn-turn", "child"), &SpawnAgentParam{
					TaskName: "survey",
					Message:  "first task",
				})
				if err != nil {
					t.Fatal(err)
				}
				// 子任务请求先不回复，子 Session 就不会向根 Session 发完成通知。
				receiveSessionRequest(t, model, childID)
			} else {
				itemID := h.input(t, session.SessionInputModeNormal, "第一轮")
				receiveSessionRequest(t, model, rootID).reply <- modelResponse{text: "好"}
				h.waitTurnFinished(t, itemID)
			}
			if tt.isReopened {
				h.stop(t)
				loaded, err := reopenTestConversation(t, c, newOpt())
				if err != nil {
					t.Fatal(err)
				}
				h = startTestConversation(t, store, model, loaded)
				if tt.isChild {
					message := &SendMessageParam{
						SessionID: childID,
						Mode:      session.SessionInputModeNormal,
						Message:   "second task",
					}
					if err := h.impl().sendMessage(t.Context(), h.record(t, rootID), message); err != nil {
						t.Fatal(err)
					}
					receiveSessionRequest(t, model, childID)
				} else {
					h.input(t, session.SessionInputModeNormal, "第二轮")
					receiveSessionRequest(t, model, rootID)
				}
			}
			// 3. 核对启动事件和 startup、resume 来源。
			if got := stub.getStarts(rootID); !slices.Equal(got, tt.wantStarts) {
				t.Fatalf("start hooks = %v, want %v", got, tt.wantStarts)
			}
		})
	}
}
