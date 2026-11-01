package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/session"
	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// assertACPPromptResult 检查 prompt 成功结束，停止原因是 reason。
func assertACPPromptResult(t *testing.T, message acpWireMessage, reason acp.StopReason) {
	t.Helper()
	// 1. 响应是错误时结束测试，否则解析并比较停止原因。
	if message.Error != nil {
		t.Fatalf("prompt = %+v", message.Error)
	}
	var result acp.PromptResponse
	if err := json.Unmarshal(message.Result, &result); err != nil || result.StopReason != reason {
		t.Fatalf("prompt = %s (%v), want %s", message.Result, err, reason)
	}
}

// TestACPSessionTitleNotifications 检查自动生成标题、改名、清除标题和加载已保存标题时，编辑器都收到 session_info_update。
func TestACPSessionTitleNotifications(t *testing.T) {
	// 1. 列出四种标题变化和期望的标题。
	tests := []struct {
		name  string
		title string
	}{
		{
			name:  "automatic_title",
			title: "修复登录",
		},
		{
			name:  "explicit_rename",
			title: "新的标题",
		},
		{
			name:  "clear_title",
			title: "",
		},
		{
			name:  "load_saved_title",
			title: "已保存的标题",
		},
	}
	// 2. 完成一轮对话，按场景改标题或重启后加载，再等对应的标题通知。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			config, _ := newACPConfig(t, model.server.URL)
			wire := startACPHost(t, config)
			wire.initialize(t)
			directory := t.TempDir()
			id := wire.newSession(t, directory)
			promptID := wire.prompt(t, id, "修复登录")
			model.next(t).reply <- acpModelReply{text: "好"}
			assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
			before := 0
			if tt.name != "automatic_title" {
				before = len(wire.updates)
				if err := wire.runtime.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: defs.ConversationID(id),
					Title:          tt.title,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.name == "load_saved_title" {
				wire.stop()
				wire = startACPHost(t, config)
				wire.initialize(t)
				wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       directory,
				}, nil)
				before = 0
			}
			for {
				for _, notification := range wire.updates[before:] {
					update := notification.Update.SessionInfoUpdate
					if notification.SessionId == id && update != nil && update.Title != nil && *update.Title == tt.title {
						if update.SessionUpdate != "session_info_update" {
							t.Fatalf("title update = %+v", update)
						}
						return
					}
				}
				wire.next(t)
			}
		})
	}
}

// TestACPTitleChangedEvent 检查 ConversationTitleChanged 事件转成编辑器的 session_info_update 通知，空标题也照发。
func TestACPTitleChangedEvent(t *testing.T) {
	// 1. 逐个标题交给 handleClientEvent，检查通知的方法、会话 ID 和标题。
	for _, title := range []string{"自动标题", "用户改名", ""} {
		t.Run(title, func(t *testing.T) {
			outgoing := make(chan acpMessage, 1)
			s := &acpSession{
				record: defs.Conversation{
					ID:    "conversation",
					Title: "旧标题",
				},
				outgoing:        outgoing,
				isOpenDelivered: true,
			}
			if err := s.handleClientEvent(t.Context(), &runtimeclient.ConversationTitleChanged{
				ConversationID: s.record.ID,
				Title:          title,
			}); err != nil {
				t.Fatal(err)
			}
			message := <-outgoing
			var notification acp.SessionNotification
			if err := json.Unmarshal(message.Params, &notification); err != nil {
				t.Fatal(err)
			}
			update := notification.Update.SessionInfoUpdate
			if message.Method != "session/update" || notification.SessionId != "conversation" || update == nil || update.Title == nil || *update.Title != title {
				t.Fatalf("title notification = %s", message.Params)
			}
		})
	}
}

// TestACPToolResult 检查工具结果显示给编辑器的状态、文本、图片和 rawOutput。
func TestACPToolResult(t *testing.T) {
	// 1. 列出普通结果、带图片、出错和几种命令结果。
	const content = `{"path":"a.png","mime_type":"image/png","size":3}`
	tests := []struct {
		name       string
		result     *defs.ToolResult
		wantStatus acp.ToolCallStatus
		wantText   string
		wantImages []string
	}{
		{
			name: "text_only",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(content),
			},
			wantStatus: acp.ToolCallStatusCompleted,
			wantText:   content,
		},
		{
			name: "one_image",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(content),
				Images: []*defs.ImageContent{{
					DataURL: "data:image/png;base64,AAAA",
					Alt:     "a.png",
				}},
			},
			wantStatus: acp.ToolCallStatusCompleted,
			wantText:   content,
			wantImages: []string{"AAAA"},
		},
		{
			name: "two_images",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(content),
				Images:  []*defs.ImageContent{{DataURL: "data:image/png;base64,AAAA"}, {DataURL: "data:image/png;base64,BBBB"}},
			},
			wantStatus: acp.ToolCallStatusCompleted,
			wantText:   content,
			wantImages: []string{"AAAA", "BBBB"},
		},
		{
			name: "failed_with_image",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(content),
				Error: &defs.ToolError{
					Code:    "read_failed",
					Message: "boom",
				},
				Images: []*defs.ImageContent{{DataURL: "data:image/png;base64,AAAA"}},
			},
			wantStatus: acp.ToolCallStatusFailed,
			wantText:   content + "\nboom",
			wantImages: []string{"AAAA"},
		},
		{
			name: "command_exit_zero",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(`{"command_id":"cmd_1","status":"exited","exit_code":0,"output":"ok\n","next_cursor":3,"has_more":false}`),
			},
			wantStatus: acp.ToolCallStatusCompleted,
			wantText:   "ok\n",
		},
		{
			name: "command_running",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(`{"command_id":"cmd_1","status":"running","exit_code":null,"output":"building","next_cursor":8,"has_more":false}`),
			},
			wantStatus: acp.ToolCallStatusCompleted,
			wantText:   "building",
		},
		{
			name: "command_exit_nonzero",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(`{"command_id":"cmd_1","status":"exited","exit_code":2,"output":"oops\n","next_cursor":5,"has_more":false}`),
			},
			wantStatus: acp.ToolCallStatusFailed,
			wantText:   "oops\n",
		},
		{
			name: "command_failed",
			result: &defs.ToolResult{
				CallID:  "call_1",
				Content: json.RawMessage(`{"command_id":"cmd_1","status":"failed","exit_code":null,"error":"connection lost","output":"partial","next_cursor":7,"has_more":false}`),
			},
			wantStatus: acp.ToolCallStatusFailed,
			wantText:   "partial\nconnection lost",
		},
	}
	// 2. 显示工具结果，检查状态、文本块和图片块；rawOutput 不带图片数据，传入的结果图片不被改动。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outgoing := make(chan acpMessage, 1)
			s := &acpSession{
				record:   defs.Conversation{ID: "conversation"},
				outgoing: outgoing,
			}
			s.displayItem(&defs.Item{
				Kind:       defs.ItemToolResult,
				ToolResult: tt.result,
			}, &acpStreamingItem{})
			message := <-outgoing
			var notification acp.SessionNotification
			if err := json.Unmarshal(message.Params, &notification); err != nil {
				t.Fatal(err)
			}
			update := notification.Update.ToolCallUpdate
			if update == nil || update.Status == nil || *update.Status != tt.wantStatus {
				t.Fatalf("tool call update = %s, want status %q", message.Params, tt.wantStatus)
			}
			if len(update.Content) != 1+len(tt.wantImages) || update.Content[0].Content.Content.Text == nil {
				t.Fatalf("content = %s, want a text block followed by %d image blocks", message.Params, len(tt.wantImages))
			}
			if text := update.Content[0].Content.Content.Text.Text; text != tt.wantText {
				t.Fatalf("text = %q, want %q", text, tt.wantText)
			}
			for index, wantData := range tt.wantImages {
				image := update.Content[1+index].Content.Content.Image
				if image == nil || image.Data != wantData || image.MimeType != "image/png" {
					t.Fatalf("content[%d] = %+v, want image/png %q", 1+index, update.Content[1+index].Content.Content, wantData)
				}
			}
			rawOutput, err := json.Marshal(update.RawOutput)
			if err != nil {
				t.Fatal(err)
			}
			for _, leaked := range []string{`"images"`, "data:image"} {
				if strings.Contains(string(rawOutput), leaked) {
					t.Fatalf("rawOutput = %s, must not contain %s", rawOutput, leaked)
				}
			}
			if !strings.Contains(string(rawOutput), `"call_id":"call_1"`) {
				t.Fatalf("rawOutput = %s, want the tool result without images", rawOutput)
			}
			if len(tt.result.Images) != len(tt.wantImages) {
				t.Fatalf("displayItem changed the result images: %+v", tt.result.Images)
			}
		})
	}
}

// TestACPToolCallTitle 检查 MCP 工具和内置工具在编辑器里的标题和种类。
func TestACPToolCallTitle(t *testing.T) {
	// 1. 列出工具名和期望的标题、种类。
	tests := []struct {
		name      string
		tool      string
		arguments string
		wantTitle string
		wantKind  acp.ToolKind
	}{
		{
			name:      "mcp_tool",
			tool:      "mcp__fake__echo",
			arguments: `{"text":"hi"}`,
			wantTitle: "fake: echo",
			wantKind:  acp.ToolKindOther,
		},
		{
			name:      "mcp_name_without_tool_part",
			tool:      "mcp__fake",
			arguments: `{}`,
			wantTitle: "mcp__fake",
			wantKind:  acp.ToolKindOther,
		},
		{
			name:      "builtin_tool",
			tool:      "bash",
			arguments: `{"command":"true"}`,
			wantTitle: "bash",
			wantKind:  acp.ToolKindExecute,
		},
		{
			name:      "command_follow_up_tool",
			tool:      "list_commands",
			arguments: `{}`,
			wantTitle: "list_commands",
			wantKind:  acp.ToolKindExecute,
		},
	}
	// 2. 执行一次工具调用，检查全部 tool_call 和 tool_call_update 的标题以及种类。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, _ := newACPConfig(t, model.server.URL)
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			id := wire.newSession(t, t.TempDir())
			responseID := wire.prompt(t, id, "hello")
			model.next(t).reply <- acpModelReply{
				tool:      tt.tool,
				arguments: tt.arguments,
			}
			model.next(t).reply <- acpModelReply{text: "done"}
			assertACPPromptResult(t, wire.response(t, responseID), acp.StopReasonEndTurn)
			var titles []string
			for _, update := range wire.updates {
				if call := update.Update.ToolCall; call != nil {
					titles = append(titles, call.Title)
					if call.Kind != tt.wantKind {
						t.Fatalf("tool call kind = %q, want %q", call.Kind, tt.wantKind)
					}
				}
				if call := update.Update.ToolCallUpdate; call != nil && call.Title != nil {
					titles = append(titles, *call.Title)
				}
			}
			if len(titles) == 0 {
				t.Fatal("no tool call title reached the editor")
			}
			for _, title := range titles {
				if title != tt.wantTitle {
					t.Fatalf("tool call titles = %q, want all %q", titles, tt.wantTitle)
				}
			}
		})
	}
}

// TestACPTurnStreaming 检查一轮里的流式文本、推理和用量，工具通知，模型失败，取消，插队中断别人的 Turn，忙碌拒绝和多会话互不影响。
func TestACPTurnStreaming(t *testing.T) {
	// 1. 逐场景运行。
	for _, scenario := range []string{"text_reasoning_usage", "read_file", "tool_failure", "bash_progress", "model_failure", "cancel", "immediate_cancel", "foreign_turn", "busy", "two_sessions", "live_load"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 准备独立 HOME、受控模型、acpAgent 和工作区文件。
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, _ := newACPConfig(t, model.server.URL)
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("WORKSPACE_CONTENT"), 0600); err != nil {
				t.Fatal(err)
			}
			// 2. 创建会话；foreign_turn 场景直接经 Runtime 提交一轮并让模型挂起，检查新 prompt 中断它后单独执行。
			id := wire.newSession(t, directory)
			if scenario == "foreign_turn" {
				record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
				if err != nil {
					t.Fatal(err)
				}
				progress, err := wire.runtime.SubscribeSession(t.Context(), &runtime.SubscribeSessionRequest{
					ConversationID: record.ID,
					SessionID:      record.RootSessionID,
				})
				if err != nil {
					t.Fatal(err)
				}
				foreignInputID, err := wire.runtime.SubmitInput(t.Context(), &runtime.SubmitInputRequest{
					ConversationID: record.ID,
					Mode:           session.SessionInputModeNormal,
					From: &defs.Address{
						Kind: defs.IdentityUser,
						ID:   "user",
					},
					Content: []*defs.Content{{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "foreign input"},
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				model.next(t).reply <- acpModelReply{hold: true}
				promptID := wire.prompt(t, id, "改做 B")
				isForeignTurnFinished := false
				for !isForeignTurnFinished {
					select {
					case event := <-progress.Events:
						finished := getACPTestTurnFinished(event)
						if finished == nil || !slices.Contains(finished.InputItemIDs, foreignInputID) {
							continue
						}
						if finished.Status != defs.TurnStatusInterrupted {
							t.Fatalf("foreign turn status = %q, want %q", finished.Status, defs.TurnStatusInterrupted)
						}
						isForeignTurnFinished = true
					case <-time.After(10 * time.Second):
						t.Fatal("foreign turn was not interrupted by the prompt")
					}
				}
				second := model.next(t)
				assertACPModelRequestTexts(t, second.body, "上一轮已中断。", "改做 B")
				second.reply <- acpModelReply{text: "好"}
				assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
				return
			}
			// 3. 提交输入；immediate_cancel 场景立即取消，检查 prompt 以 cancelled 结束。
			responseID := wire.prompt(t, id, "hello")
			if scenario == "immediate_cancel" {
				wire.notify(t, "session/cancel", acp.CancelNotification{SessionId: id})
				assertACPPromptResult(t, wire.response(t, responseID), acp.StopReasonCancelled)
				// 取消可能发生在模型请求发出之前或之后，检查完 prompt 结果就结束这个场景。
				return
			}
			// 4. 按场景让模型回复，检查错误、工具通知、取消后继续、忙碌拒绝、加载不重复或两个会话独立执行。
			request := model.next(t)
			switch scenario {
			case "model_failure":
				request.reply <- acpModelReply{status: 400}
				if result := wire.response(t, responseID); result.Error == nil || result.Error.Code != -32603 {
					t.Fatalf("model failure = %+v", result)
				}
				return
			case "read_file", "tool_failure", "bash_progress":
				reply := acpModelReply{
					tool:      "read_file",
					arguments: `{"path":"notes.txt"}`,
				}
				if scenario == "tool_failure" {
					reply.arguments = `{"path":"missing.txt"}`
				}
				if scenario == "bash_progress" {
					reply.tool, reply.arguments = "bash", `{"command":"printf FIRST; sleep 0.05; printf SECOND"}`
				}
				request.reply <- reply
				second := model.next(t)
				if scenario == "read_file" && !strings.Contains(fmt.Sprint(second.body), "WORKSPACE_CONTENT") {
					t.Fatalf("tool result missing: %v", second.body)
				}
				second.reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, wire.response(t, responseID), acp.StopReasonEndTurn)
				toolStarted, toolFinished, progress, location := false, false, false, false
				for _, update := range wire.updates {
					if update.Update.ToolCall != nil {
						toolStarted = true
					}
					if tool := update.Update.ToolCallUpdate; tool != nil {
						if tool.Status != nil {
							if *tool.Status == acp.ToolCallStatusInProgress {
								progress = true
							}
							if *tool.Status == acp.ToolCallStatusCompleted || *tool.Status == acp.ToolCallStatusFailed {
								toolFinished = true
								if scenario == "bash_progress" && (len(tool.Content) != 1 || tool.Content[0].Content.Content.Text.Text != "FIRSTSECOND") {
									t.Fatalf("duplicated tool output: %+v", tool.Content)
								}
								if (*tool.Status == acp.ToolCallStatusFailed) != (scenario == "tool_failure") {
									t.Fatalf("tool status = %s", *tool.Status)
								}
							}
						}
						for _, loc := range tool.Locations {
							if loc.Path == filepath.Join(directory, "notes.txt") {
								location = true
							}
						}
					}
				}
				if !toolStarted || !toolFinished || (scenario == "bash_progress" && !progress) || (scenario == "read_file" && !location) {
					t.Fatalf("tool started=%v finished=%v progress=%v location=%v", toolStarted, toolFinished, progress, location)
				}
				return
			case "cancel", "busy", "two_sessions", "live_load":
				request.reply <- acpModelReply{
					text: "partial",
					hold: true,
				}
				for {
					message := wire.next(t)
					if message.Method == "session/update" && len(wire.updates) > 0 && wire.updates[len(wire.updates)-1].Update.AgentMessageChunk != nil {
						break
					}
				}
				if scenario == "busy" {
					busy := wire.response(t, wire.prompt(t, id, "/skill:missing"))
					if busy.Error == nil || busy.Error.Code != -32001 || busy.Error.Message != "session is busy" {
						t.Fatalf("busy = %+v", busy)
					}
				}
				if scenario == "live_load" {
					before := len(wire.updates)
					wire.call(t, "session/load", acp.LoadSessionRequest{
						SessionId: id,
						Cwd:       directory,
					}, nil)
					for _, update := range wire.updates[before:] {
						if update.Update.AgentMessageChunk != nil {
							t.Fatal("live load duplicated text")
						}
					}
				}
				if scenario == "two_sessions" {
					second := wire.newSession(t, t.TempDir())
					nextID := wire.prompt(t, second, "independent")
					model.next(t).reply <- acpModelReply{text: "independent reply"}
					assertACPPromptResult(t, wire.response(t, nextID), acp.StopReasonEndTurn)
				}
				wire.notify(t, "session/cancel", acp.CancelNotification{SessionId: id})
				assertACPPromptResult(t, wire.response(t, responseID), acp.StopReasonCancelled)
				// 提交下一条 prompt，检查模型依次收到半截回复、中断提示和新输入。
				nextID := wire.prompt(t, id, "改做 B")
				next := model.next(t)
				assertACPModelRequestTexts(t, next.body, "partial", "上一轮已中断。", "改做 B")
				next.reply <- acpModelReply{text: "continued"}
				assertACPPromptResult(t, wire.response(t, nextID), acp.StopReasonEndTurn)
				return
			default:
				request.reply <- acpModelReply{
					text:      "hello| world",
					reasoning: "thinking",
					usage:     true,
				}
			}
			// 5. 等 prompt 结束，拼接文本和推理通知，检查没有重复或缺失。
			assertACPPromptResult(t, wire.response(t, responseID), acp.StopReasonEndTurn)
			text, thought := "", ""
			for _, notification := range wire.updates {
				update := notification.Update
				if update.AgentMessageChunk != nil {
					text += update.AgentMessageChunk.Content.Text.Text
				}
				if update.AgentThoughtChunk != nil {
					thought += update.AgentThoughtChunk.Content.Text.Text
				}
			}
			if text != "hello world" || thought != "thinking" {
				t.Fatalf("duplicate or missing text: %q, thought %q", text, thought)
			}
			// 6. 等状态订阅发来的用量；usage_update 可能晚于 prompt 响应。
			found := false
			for !found {
				for _, update := range wire.updates {
					if usage := update.Update.UsageUpdate; usage != nil && usage.Size == 32000 && usage.Used > 0 {
						found = true
					}
				}
				if !found {
					wire.next(t)
				}
			}
		})
	}
}

// assertACPModelRequestTexts 检查模型请求的 messages 按顺序包含 texts。
func assertACPModelRequestTexts(t *testing.T, body map[string]any, texts ...string) {
	t.Helper()
	// 1. 编码 messages，逐个查找文本，每找到一个就从它后面接着找。
	messages, err := json.Marshal(body["messages"])
	if err != nil {
		t.Fatal(err)
	}
	rest := string(messages)
	for _, text := range texts {
		index := strings.Index(rest, text)
		if index < 0 {
			t.Fatalf("model request messages missing %q in order %q: %s", text, texts, messages)
		}
		rest = rest[index+len(text):]
	}
}

// TestACPHistoryReplay 检查加载时回放历史、订阅断开重新同步后只补发缺的部分，以及工具执行中加载能收到之后的输出。
func TestACPHistoryReplay(t *testing.T) {
	// 1. 逐场景运行。
	for _, scenario := range []string{"load_replays_history", "resync_sends_missing_suffix", "load_while_tool_runs"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 准备独立 HOME、受控模型和配置。
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, _ := newACPConfig(t, model.server.URL)
			if scenario == "load_while_tool_runs" {
				// 2. load_while_tool_runs：starter 执行等 FIFO 的 bash，loader 中途加载，只看到一个工具调用，之后收到加载后的输出。
				server := startACPTestServer(t, cfg, nil)
				starter, loader := startACPAgent(t, cfg, server), startACPAgent(t, cfg, server)
				starter.initialize(t)
				loader.initialize(t)
				directory := t.TempDir()
				fifo := filepath.Join(directory, "release.fifo")
				if err := syscall.Mkfifo(fifo, 0600); err != nil {
					t.Fatal(err)
				}
				id := starter.newSession(t, directory)
				promptID := starter.prompt(t, id, "wait for it")
				model.next(t).reply <- acpModelReply{
					tool:      "bash",
					arguments: `{"command":"echo before; cat release.fifo; echo after"}`,
				}
				// 写端要等 cat 打开读端才能打开，打开时 bash 一定在执行。
				var release *os.File
				opened := make(chan error, 1)
				go func() {
					var err error
					release, err = os.OpenFile(fifo, os.O_WRONLY, 0)
					opened <- err
				}()
				select {
				case err := <-opened:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("bash did not open the FIFO")
				}
				loader.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       directory,
				}, nil)
				calls := 0
				for _, notification := range loader.updates {
					if notification.Update.ToolCall != nil {
						calls++
					}
					if notification.Update.ToolCallUpdate != nil {
						t.Fatalf("replay has a tool update %+v before the command finished", notification.Update.ToolCallUpdate)
					}
				}
				if calls != 1 {
					t.Fatalf("replay shows %d tool calls, want the running one", calls)
				}
				if _, err := release.WriteString("released\n"); err != nil {
					t.Fatal(err)
				}
				if err := release.Close(); err != nil {
					t.Fatal(err)
				}
				model.next(t).reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, starter.response(t, promptID), acp.StopReasonEndTurn)
				// 读到 loader 收到工具结束前的最后一次进度，检查里面有加载之后的输出；加载前的 before 不要求补发。
				output := ""
				for {
					if loader.next(t).Method != "session/update" {
						continue
					}
					update := loader.updates[len(loader.updates)-1].Update.ToolCallUpdate
					if update == nil || update.Status == nil {
						continue
					}
					if *update.Status != acp.ToolCallStatusInProgress {
						break
					}
					output = update.Content[0].Content.Content.Text.Text
				}
				if !strings.Contains(output, "released\nafter\n") {
					t.Fatalf("progress shown to the editor that opened mid-turn = %q, want the output produced after it opened", output)
				}
				return
			}
			// 3. 其他场景启动 acpAgent 并创建会话。
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			directory := t.TempDir()
			id := wire.newSession(t, directory)
			if scenario == "resync_sends_missing_suffix" {
				// 4. resync_sends_missing_suffix：先订阅 Runtime，每段增量确认 Runtime 收到后再发下一段。
				record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
				if err != nil {
					t.Fatal(err)
				}
				progress, err := wire.runtime.SubscribeSession(t.Context(), &runtime.SubscribeSessionRequest{
					ConversationID: record.ID,
					SessionID:      record.RootSessionID,
				})
				if err != nil {
					t.Fatal(err)
				}
				promptID := wire.prompt(t, id, "long answer")
				chunks := make(chan string)
				model.next(t).reply <- acpModelReply{stream: chunks}
				var reply strings.Builder
				sendChunk := func() {
					t.Helper()
					part := fmt.Sprintf("[%06d]%s", reply.Len(), strings.Repeat("x", 1000))
					chunks <- part
					reply.WriteString(part)
					for {
						select {
						case event := <-progress.Events:
							if delta := getACPTestDelta(event); delta != nil && delta.Text == part {
								return
							}
						case <-time.After(10 * time.Second):
							t.Fatal("runtime did not stream the chunk")
						}
					}
				}
				sendChunk()
				waitACPAgentChunk(t, wire)
				// 编辑器暂停读取，模型继续输出，直到日志出现 subscriber removed（Runtime 以 ErrSubscriberTooSlow 关闭了订阅）。
				wire.pauseReading()
				for !strings.Contains(wire.pipeline.GetLogText(), `"msg":"subscriber removed"`) {
					if reply.Len() > 8<<20 {
						t.Fatal("subscription was not closed while the editor stopped reading")
					}
					sendChunk()
				}
				// 订阅关闭后再发一些增量，然后恢复读取，检查重新同步补发。
				for range 16 {
					sendChunk()
				}
				wire.resumeReading()
				var text strings.Builder
				displayed := 0
				readText := func() {
					for ; displayed < len(wire.updates); displayed++ {
						if chunk := wire.updates[displayed].Update.AgentMessageChunk; chunk != nil {
							text.WriteString(chunk.Content.Text.Text)
						}
					}
				}
				// 等补发的文本全部到达，再发一些增量后结束 Turn，检查编辑器拿到的文本不缺不重。
				for text.Len() < reply.Len() {
					wire.next(t)
					readText()
				}
				for range 16 {
					sendChunk()
				}
				close(chunks)
				assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
				readText()
				if text.String() != reply.String() {
					t.Fatalf("editor text has %d bytes, want the %d-byte reply without gaps or repeats", text.Len(), reply.Len())
				}
				return
			}
			// 5. load_replays_history：先完成含推理和工具的一轮，再重启 acpAgent 并加载。
			promptID := wire.prompt(t, id, "original question")
			model.next(t).reply <- acpModelReply{
				tool:      "read_file",
				arguments: `{"path":"absent"}`,
				reasoning: "reasoning once",
			}
			model.next(t).reply <- acpModelReply{text: "saved| answer"}
			assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
			wire.stop()
			wire = startACPHost(t, cfg)
			wire.initialize(t)
			wire.call(t, "session/load", acp.LoadSessionRequest{
				SessionId: id,
				Cwd:       directory,
			}, nil)
			// 6. 检查各类历史内容只显示一次，加载时没有请求模型。
			user, text, thought := "", "", ""
			calls, results := 0, 0
			for _, notification := range wire.updates {
				update := notification.Update
				if update.UserMessageChunk != nil {
					user += update.UserMessageChunk.Content.Text.Text
				}
				if update.AgentMessageChunk != nil {
					text += update.AgentMessageChunk.Content.Text.Text
				}
				if update.AgentThoughtChunk != nil {
					thought += update.AgentThoughtChunk.Content.Text.Text
				}
				if update.ToolCall != nil {
					calls++
				}
				if update.ToolCallUpdate != nil && update.ToolCallUpdate.Status != nil {
					results++
				}
			}
			if user != "original question" || text != "saved answer" || thought != "reasoning once" || calls != 1 || results != 1 {
				t.Fatalf("replay user=%q text=%q thought=%q calls=%d results=%d", user, text, thought, calls, results)
			}
			select {
			case req := <-model.requests:
				t.Fatalf("load called model: %v", req.body)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// getACPTestTurnFinished 从对话事件里取出 SessionTurnFinished，不是时返回 nil。
func getACPTestTurnFinished(event conversation.ConversationEvent) *session.SessionTurnFinished {
	// 1. 先解开 Session 事件包装，再取 SessionTurnFinished。
	wrapped, ok := event.(*conversation.ConversationSessionEvent)
	if !ok {
		return nil
	}
	finished, _ := wrapped.Event.(*session.SessionTurnFinished)
	return finished
}

// getACPTestDelta 从对话事件里取出 Agent 的 Item 增量，不是时返回 nil。
func getACPTestDelta(event conversation.ConversationEvent) *agent.AgentItemDelta {
	// 1. 依次解开 Session 和 Agent 事件，类型是 ItemDelta 时返回增量。
	wrapped, ok := event.(*conversation.ConversationSessionEvent)
	if !ok {
		return nil
	}
	agentEvent, ok := wrapped.Event.(*session.SessionAgentEvent)
	if !ok || agentEvent.Event.Type != agent.AgentEventItemDelta {
		return nil
	}
	return agentEvent.Event.Delta
}

// TestACPContentAndSkills 检查 Skill 作为可用命令通知编辑器、各种输入内容交给模型，以及无效内容和读取失败被拒绝。
func TestACPContentAndSkills(t *testing.T) {
	// 1. 逐场景准备 Skill 和输入，检查命令列表、错误响应和模型收到的内容。
	for _, scenario := range []string{"mixed_content", "invalid_image", "unsupported_blob", "skill_body_failure", "agents_read_failure", "separate_skills"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, _ := newACPConfig(t, model.server.URL)
			directory := t.TempDir()
			skillPath := filepath.Join(directory, ".agents", "skills", "review", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skillPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(skillPath, []byte("---\nname: review\ndescription: Review project\n---\nFIRST_SKILL_BODY"), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "agents_read_failure" {
				if err := os.Mkdir(filepath.Join(directory, "AGENTS.md"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			id := wire.newSession(t, directory)
			// 读到新会话的可用命令通知，检查里面有 review 和它的描述。
			for {
				wire.next(t)
				if update := wire.updates[len(wire.updates)-1].Update.AvailableCommandsUpdate; update != nil {
					found := false
					for _, command := range update.AvailableCommands {
						if command.Name == "review" && command.Description == "Review project" {
							found = true
						}
					}
					if !found {
						t.Fatal("review command not advertised")
					}
					break
				}
			}
			switch scenario {
			case "skill_body_failure":
				if err := os.Remove(skillPath); err != nil {
					t.Fatal(err)
				}
			case "separate_skills":
				otherDirectory := t.TempDir()
				otherPath := filepath.Join(otherDirectory, ".agents", "skills", "review", "SKILL.md")
				_ = os.MkdirAll(filepath.Dir(otherPath), 0700)
				_ = os.WriteFile(otherPath, []byte("---\nname: review\ndescription: Second project\n---\nSECOND_SKILL_BODY"), 0600)
				otherID := wire.newSession(t, otherDirectory)
				promptID := wire.prompt(t, otherID, "/review")
				req := model.next(t)
				body := fmt.Sprint(req.body)
				if !strings.Contains(body, "SECOND_SKILL_BODY") || strings.Contains(body, "FIRST_SKILL_BODY") {
					t.Fatalf("wrong skills: %s", body)
				}
				req.reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
			}
			prompt := []acp.ContentBlock{acp.TextBlock("/review")}
			if scenario == "mixed_content" {
				prompt = append(prompt,
					acp.ImageBlock("aGVsbG8=", "image/png"), acp.ResourceLinkBlock("reference", "https://example.invalid/info"),
					acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{
						Uri:  "file:///selection",
						Text: "/skill:missing /review EMBEDDED_TEXT",
					}}),
					acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
						Uri:      "file:///blob",
						MimeType: acp.Ptr("text/plain"),
						Blob:     base64.StdEncoding.EncodeToString([]byte("BLOB_TEXT")),
					}}))
			}
			if scenario == "invalid_image" {
				prompt = []acp.ContentBlock{acp.ImageBlock("not base64", "image/png")}
			}
			if scenario == "unsupported_blob" {
				prompt = []acp.ContentBlock{acp.ResourceBlock(acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
					Uri:      "file:///blob",
					MimeType: acp.Ptr("application/pdf"),
					Blob:     "aGVsbG8=",
				}})}
			}
			promptID := wire.send(t, "session/prompt", acp.PromptRequest{
				SessionId: id,
				Prompt:    prompt,
			})
			if scenario == "invalid_image" || scenario == "unsupported_blob" || scenario == "skill_body_failure" || scenario == "agents_read_failure" {
				result := wire.response(t, promptID)
				if result.Error == nil {
					t.Fatal("invalid content, missing skill or unreadable default executor rules accepted")
				}
				if scenario == "agents_read_failure" && !strings.Contains(result.Error.Message, `load AGENTS.md from executor "local"`) {
					t.Fatalf("error = %+v, want the default executor AGENTS.md failure", result.Error)
				}
				select {
				case <-model.requests:
					t.Fatal("rejected input reached model")
				default:
				}
				return
			}
			req := model.next(t)
			body := fmt.Sprint(req.body)
			if !strings.Contains(body, "FIRST_SKILL_BODY") {
				t.Fatalf("skill missing: %s", body)
			}
			if scenario == "mixed_content" {
				for _, expected := range []string{"aGVsbG8=", "example.invalid/info", "/skill:missing /review EMBEDDED_TEXT", "BLOB_TEXT"} {
					if !strings.Contains(body, expected) {
						t.Fatalf("missing %q in %s", expected, body)
					}
				}
			}
			req.reply <- acpModelReply{text: "done"}
			assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
		})
	}
}

// TestACPModelConfiguration 检查编辑器切换模型和推理强度后，上游请求用的模型和推理强度。
func TestACPModelConfiguration(t *testing.T) {
	// 1. 启动 acpAgent 并创建会话。
	t.Setenv("HOME", t.TempDir())
	model := newACPModel(t)
	cfg, _ := newACPConfig(t, model.server.URL)
	wire := startACPHost(t, cfg)
	wire.initialize(t)
	id := wire.newSession(t, t.TempDir())
	// 2. 依次切到 second、改推理强度、切回 default，每次执行一轮，检查上游模型和推理强度。
	for _, step := range []struct{ option, value, wantModel, wantEffort string }{
		{
			"model",
			"local/second",
			"second",
			"low",
		}, {
			"reasoning_effort",
			"high",
			"second",
			"high",
		}, {
			"model",
			"local/default",
			"default",
			"",
		},
	} {
		t.Run(step.option+step.value, func(t *testing.T) {
			var response acp.SetSessionConfigOptionResponse
			wire.call(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": step.option, "value": step.value}, &response)
			promptID := wire.prompt(t, id, "test model")
			req := model.next(t)
			if req.body["model"] != step.wantModel || (step.wantEffort != "" && req.body["reasoning_effort"] != step.wantEffort) {
				t.Fatalf("wrong selection: %v", req.body)
			}
			req.reply <- acpModelReply{text: "done"}
			assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
		})
	}
}

// TestACPModelListAfterCatalogChange 检查模型目录变化后，编辑器的模型选项跟着刷新，新模型能选，删掉的模型不能选。
func TestACPModelListAfterCatalogChange(t *testing.T) {
	// 1. 列出新增和删除模型两种场景。
	tests := []struct {
		name string
		// models 是改写后 models/local.yaml 里 models 一段的内容。
		models string
		// selectModel 是重新同步后要选的模型；wantSelectErr 非空时选择失败，否则下一轮请求上游的 wantUpstreamModel。
		selectModel       string
		wantModels        []string
		wantSelectErr     string
		wantUpstreamModel string
	}{
		{
			name:              "added_model_is_listed_and_selectable",
			models:            "  default: {}\n  second: {}\n  third: {}\n",
			selectModel:       "local/third",
			wantModels:        []string{"local/default", "local/second", "local/third"},
			wantUpstreamModel: "third",
		},
		{
			name:          "removed_model_is_no_longer_listed",
			models:        "  default: {}\n",
			selectModel:   "local/second",
			wantModels:    []string{"local/default"},
			wantSelectErr: "unknown model",
		},
	}
	// 2. 改写模型配置并更新 Runtime 定义，等同步后的选项列表，再检查选择结果和下一轮的上游模型。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, configPath := newACPConfig(t, model.server.URL)
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			id := wire.newSession(t, t.TempDir())
			saveACPSession(t, wire, model, id)
			writeTestFiles(t, map[string]string{
				filepath.Join(filepath.Dir(configPath), "models", "local.yaml"): fmt.Sprintf("id: local\nprotocol: openai_chat\nbase_url: %s\nmodels:\n%s", model.server.URL, tt.models),
			})
			reloaded, err := config.LoadConfig(&config.LoadConfigOpt{Path: configPath})
			if err != nil {
				t.Fatal(err)
			}
			before := len(wire.updates)
			definitions := &runtime.Definitions{
				Agents:     reloaded.GetAgentDefinitions(),
				Models:     reloaded.GetModels(),
				MCPServers: reloaded.GetMCPServers(),
				Hooks:      reloaded.GetHooks(),
			}
			if _, err := wire.runtime.SetDefinitions(t.Context(), definitions); err != nil {
				t.Fatal(err)
			}
			// 重新同步后的 config_option_update 列出新的模型目录。
			for isListed := false; !isListed; {
				wire.next(t)
				for _, notification := range wire.updates[before:] {
					if update := notification.Update.ConfigOptionUpdate; update != nil {
						_, values := getACPSelectOption(update.ConfigOptions, "model")
						isListed = slices.Equal(values, tt.wantModels)
					}
				}
			}
			result := wire.response(t, wire.send(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": "model", "value": tt.selectModel}))
			if tt.wantSelectErr != "" {
				if result.Error == nil || result.Error.Message != tt.wantSelectErr {
					t.Fatalf("set_config_option = %+v, want error %q", result, tt.wantSelectErr)
				}
				return
			}
			var response acp.SetSessionConfigOptionResponse
			if result.Error != nil || json.Unmarshal(result.Result, &response) != nil {
				t.Fatalf("set_config_option = %+v", result)
			}
			if current, _ := getACPSelectOption(response.ConfigOptions, "model"); current != tt.selectModel {
				t.Fatalf("selected model = %q, want %q", current, tt.selectModel)
			}
			promptID := wire.prompt(t, id, "use the new model")
			request := model.next(t)
			if request.body["model"] != tt.wantUpstreamModel {
				t.Fatalf("upstream model = %v, want %q", request.body["model"], tt.wantUpstreamModel)
			}
			request.reply <- acpModelReply{text: "done"}
			assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
		})
	}
}

// TestACPStorageFailure 检查输入保存失败时 prompt 报错且不请求模型，别的会话照常工作，排除故障后原会话能重试成功。
func TestACPStorageFailure(t *testing.T) {
	// 1. 创建新会话，在它还没创建的日志目录位置写一个普通文件，让第一次保存输入失败。
	t.Setenv("HOME", t.TempDir())
	model := newACPModel(t)
	cfg, _ := newACPConfig(t, model.server.URL)
	wire := startACPHost(t, cfg)
	wire.initialize(t)
	id := wire.newSession(t, t.TempDir())
	homeDirectory, _ := os.UserHomeDir()
	logDirectory := filepath.Join(homeDirectory, ".local", "share", "openvaala", "conversations", string(id))
	if err := os.MkdirAll(filepath.Dir(logDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logDirectory, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	// 2. 提交输入，检查返回 -32603 且没有请求模型。
	result := wire.response(t, wire.prompt(t, id, "cannot persist"))
	if result.Error == nil || result.Error.Code != -32603 {
		t.Fatalf("storage error = %+v", result)
	}
	select {
	case <-model.requests:
		t.Fatal("model called after failed input persistence")
	default:
	}
	// 3. 在另一个新会话完成一轮。
	other := wire.newSession(t, t.TempDir())
	promptID := wire.prompt(t, other, "still works")
	model.next(t).reply <- acpModelReply{text: "yes"}
	assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
	// 4. 删掉挡路的文件，再向原会话提交输入，检查成功。
	if err := os.Remove(logDirectory); err != nil {
		t.Fatal(err)
	}
	promptID = wire.prompt(t, id, "retry")
	model.next(t).reply <- acpModelReply{text: "saved"}
	assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
}

// TestACPUnknownUsageAndExecutorLocation 检查上下文用量未知时不发 usage_update，工具位置按所选 Executor 的目录拼出。
func TestACPUnknownUsageAndExecutorLocation(t *testing.T) {
	// 1. 逐场景准备会话，检查没有用量通知，或指定 Executor 时工具读到它的文件、位置是它目录下的绝对路径。
	for _, scenario := range []string{"unknown_context", "different_executor"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			_, configPath := newACPConfig(t, model.server.URL)
			executorDirectory := t.TempDir()
			if scenario == "different_executor" {
				_ = os.MkdirAll(filepath.Join(filepath.Dir(configPath), "executors"), 0700)
				_ = os.WriteFile(filepath.Join(filepath.Dir(configPath), "executors", "separate.yaml"), []byte("id: separate\ndescription: Separate directory\nkind: local\nworkspace_directory: "+executorDirectory+"\n"), 0600)
				_ = os.WriteFile(filepath.Join(executorDirectory, "notes.txt"), []byte("OTHER_ENV"), 0600)
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: configPath})
			if err != nil {
				t.Fatal(err)
			}
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			id := wire.newSession(t, t.TempDir())
			if scenario == "unknown_context" {
				wire.call(t, "session/list", acp.ListSessionsRequest{}, nil)
				for _, update := range wire.updates {
					if update.Update.UsageUpdate != nil {
						t.Fatal("unmeasured session emitted usage")
					}
				}
				return
			}

			promptID := wire.prompt(t, id, "test environment")
			reply := acpModelReply{
				text:  "done",
				usage: true,
			}
			if scenario == "different_executor" {
				reply = acpModelReply{
					tool:      "read_file",
					arguments: `{"path":"notes.txt","executor":"separate"}`,
				}
			}
			model.next(t).reply <- reply
			if scenario == "different_executor" {
				request := model.next(t)
				if !strings.Contains(fmt.Sprint(request.body), "OTHER_ENV") {
					t.Fatal("wrong tool environment")
				}
				request.reply <- acpModelReply{text: "done"}
			}
			assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
			wire.call(t, "session/list", acp.ListSessionsRequest{}, nil)
			for _, update := range wire.updates {
				if scenario == "unknown_context" && update.Update.UsageUpdate != nil {
					t.Fatal("unknown context emitted usage")
				}
			}
			if scenario != "different_executor" {
				return
			}
			// 位置按所选 Executor 在 Session 详情里的 WorkspaceDirectory 拼出，也就是配置的 workspace_directory。
			record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
			if err != nil {
				t.Fatal(err)
			}
			details, err := wire.runtime.GetSessionDetails(t.Context(), &runtime.GetSessionDetailsRequest{
				ConversationID: record.ID,
				SessionID:      record.RootSessionID,
			})
			if err != nil {
				t.Fatal(err)
			}
			index := slices.IndexFunc(details.Executors, func(executor conversation.ExecutorDetails) bool { return executor.ID == "separate" })
			if index < 0 || details.Executors[index].WorkspaceDirectory != executorDirectory {
				t.Fatalf("executors = %+v, want separate in %s", details.Executors, executorDirectory)
			}
			if !hasACPToolLocation(wire, filepath.Join(details.Executors[index].WorkspaceDirectory, "notes.txt")) {
				t.Fatal("tool location did not use the selected executor directory")
			}
		})
	}
}

// TestACPConfigOptions 检查 executor 下拉框的选项顺序、当前值和描述，拿不到 Executor 列表时不显示。
func TestACPConfigOptions(t *testing.T) {
	// 1. 准备一个模型，列出几种 Executor 列表。
	models := []*defs.ModelDefinition{{Ref: defs.ModelRef{
		ProviderID: "local",
		ModelID:    "default",
	}}}
	tests := []struct {
		name      string
		executors []conversation.ExecutorDetails
		// wantValues 是 executor 下拉框的选项，nil 表示不显示这个下拉框。
		wantValues  []string
		wantCurrent string
	}{
		{
			name: "default_first",
			executors: []conversation.ExecutorDetails{{
				ID:                 "extra",
				Description:        "Extra directory",
				WorkspaceDirectory: "/b",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			wantValues:  []string{"extra", "local"},
			wantCurrent: "extra",
		},
		{
			name: "single_executor",
			executors: []conversation.ExecutorDetails{{
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
			wantValues:  []string{"local"},
			wantCurrent: "local",
		},
		{
			name: "details_unavailable",
		},
	}
	// 2. 生成设置选项，检查 executor 下拉框的当前值、选项和描述。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := getACPConfigOptions(models, defs.ModelSelection{Ref: models[0].Ref}, tt.executors)
			index := slices.IndexFunc(options, func(option acp.SessionConfigOption) bool { return option.Select.Id == "executor" })
			if tt.wantValues == nil {
				if index >= 0 {
					t.Fatalf("executor option = %+v, want none", options[index].Select)
				}
				return
			}
			if index < 0 {
				t.Fatalf("options = %+v, want an executor option", options)
			}
			selectOption := options[index].Select
			var values []string
			for _, option := range *selectOption.Options.Ungrouped {
				if option.Name != string(option.Value) {
					t.Fatalf("option name = %q, want the executor ID %q", option.Name, option.Value)
				}
				values = append(values, string(option.Value))
			}
			if !slices.Equal(values, tt.wantValues) || string(selectOption.CurrentValue) != tt.wantCurrent {
				t.Fatalf("executor option values = %v, current = %q; want %v, %q", values, selectOption.CurrentValue, tt.wantValues, tt.wantCurrent)
			}
			for position, option := range *selectOption.Options.Ungrouped {
				description := tt.executors[position].Description
				if (description == "") != (option.Description == nil) || (option.Description != nil && *option.Description != description) {
					t.Fatalf("option %s description = %v, want %q", option.Value, option.Description, description)
				}
			}
		})
	}
}

// TestACPSetExecutorOption 检查修改默认 Executor 在重新同步后才回复、Turn 进行中被拒绝，以及连续两个设置请求都得到回复。
func TestACPSetExecutorOption(t *testing.T) {
	// 1. 列出正常修改、忙碌和连续请求三种场景。
	tests := []struct {
		name   string
		isBusy bool
		// isRepeated 是否在第一个请求等重新同步时紧接着再发一个，把默认 Executor 改回 local？
		isRepeated bool
	}{
		{name: "switch_after_resync"},
		{
			name:   "busy",
			isBusy: true,
		},
		{
			name:       "second_request_before_resync",
			isRepeated: true,
		},
	}
	// 2. 发送设置请求，检查回复和通知的先后、命令列表和保存后的设置。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			extraDirectory := t.TempDir()
			writeTestFiles(t, map[string]string{filepath.Join(extraDirectory, ".agents", "skills", "extra-skill", "SKILL.md"): "---\nname: extra-skill\ndescription: Skill of the extra executor\n---\nEXTRA_SKILL_BODY"})
			cfg := newACPConfigWithExecutor(t, model.server.URL, "extra", extraDirectory)
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			directory := t.TempDir()
			var created acp.NewSessionResponse
			wire.call(t, "session/new", acp.NewSessionRequest{
				Cwd:        directory,
				McpServers: []acp.McpServer{},
			}, &created)
			if current, values := getACPSelectOption(created.ConfigOptions, "executor"); current != defs.LocalExecutorID || !slices.Contains(values, "extra") {
				t.Fatalf("new session executor option = %q of %v, want local of a list with extra", current, values)
			}
			id := created.SessionId
			promptID := 0
			if tt.isBusy {
				promptID = wire.prompt(t, id, "hold")
				model.next(t).reply <- acpModelReply{
					text: "partial",
					hold: true,
				}
				waitACPAgentChunk(t, wire)
			}
			if tt.isRepeated {
				// 连续发两个设置请求，检查两个都有回复，最后的默认 Executor 是 local。
				first := wire.send(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": "executor", "value": "extra"})
				second := wire.send(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": "executor", "value": defs.LocalExecutorID})
				if result := wire.response(t, first); result.Error != nil {
					t.Fatalf("first set_config_option = %+v", result.Error)
				}
				result := wire.response(t, second)
				var response acp.SetSessionConfigOptionResponse
				if result.Error != nil || json.Unmarshal(result.Result, &response) != nil {
					t.Fatalf("second set_config_option = %+v", result)
				}
				record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
				if err != nil {
					t.Fatal(err)
				}
				if current, _ := getACPSelectOption(response.ConfigOptions, "executor"); current != defs.LocalExecutorID || record.ExecutorSettings.DefaultExecutorID != defs.LocalExecutorID {
					t.Fatalf("second response executor = %q, saved default = %q; want local", current, record.ExecutorSettings.DefaultExecutorID)
				}
				return
			}
			before := len(wire.updates)
			result := wire.response(t, wire.send(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": "executor", "value": "extra"}))
			record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
			if err != nil {
				t.Fatal(err)
			}
			if tt.isBusy {
				if result.Error == nil || result.Error.Code != -32001 || result.Error.Message != "session is busy" {
					t.Fatalf("busy set_config_option = %+v, want -32001 session is busy", result)
				}
				if record.ExecutorSettings.DefaultExecutorID != "" {
					t.Fatalf("busy change saved settings %+v", record.ExecutorSettings)
				}
				wire.notify(t, "session/cancel", acp.CancelNotification{SessionId: id})
				assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonCancelled)
				return
			}
			if result.Error != nil {
				t.Fatalf("set_config_option = %+v", result.Error)
			}
			var response acp.SetSessionConfigOptionResponse
			if err := json.Unmarshal(result.Result, &response); err != nil {
				t.Fatal(err)
			}
			if current, _ := getACPSelectOption(response.ConfigOptions, "executor"); current != "extra" {
				t.Fatalf("response executor = %q, want extra", current)
			}
			// 回复在重新同步之后：同步后的 config_option_update 和按新默认 Executor 列出的命令先到。
			isUpdated, commands := false, []string(nil)
			for _, notification := range wire.updates[before:] {
				if update := notification.Update.ConfigOptionUpdate; update != nil {
					current, _ := getACPSelectOption(update.ConfigOptions, "executor")
					isUpdated = current == "extra"
				}
				if update := notification.Update.AvailableCommandsUpdate; update != nil {
					commands = nil
					for _, command := range update.AvailableCommands {
						commands = append(commands, command.Name)
					}
				}
			}
			if !isUpdated || !slices.Contains(commands, "extra-skill") {
				t.Fatalf("before the response: executor updated = %v, last commands = %v; want extra and extra-skill", isUpdated, commands)
			}
			settings := record.ExecutorSettings
			if settings.DefaultExecutorID != "extra" || len(settings.ExecutorIDs) != 0 || !maps.Equal(settings.WorkspaceDirectories, map[string]string{defs.LocalExecutorID: directory}) {
				t.Fatalf("saved settings = %+v, want only the default executor changed to extra", settings)
			}
		})
	}
}

// TestACPConversationSyncFailed 检查 Client 放弃对话后等待中的请求报错，之后的请求先重新打开对话，补发放弃期间的历史且不重复，编辑器的 MCP 重新登记。
func TestACPConversationSyncFailed(t *testing.T) {
	// 1. 列出由 prompt 和 executor 设置请求触发重新打开的两种场景。
	tests := []struct {
		name string
		// reopen 在 Client 放弃对话后发一个需要先重新打开对话的请求，返回请求 ID。
		reopen func(t *testing.T, wire *acpWire, id acp.SessionId) int
		// finish 在 serve 恢复后检查 reopen 的请求成功。
		finish func(t *testing.T, wire *acpWire, model *acpModel, requestID int)
	}{
		{
			name: "prompt_reopens",
			reopen: func(t *testing.T, wire *acpWire, id acp.SessionId) int {
				return wire.prompt(t, id, "after drop")
			},
			finish: func(t *testing.T, wire *acpWire, model *acpModel, requestID int) {
				model.next(t).reply <- acpModelReply{text: "AFTER_REOPEN"}
				assertACPPromptResult(t, wire.response(t, requestID), acp.StopReasonEndTurn)
			},
		},
		{
			name: "executor_option_reopens",
			reopen: func(t *testing.T, wire *acpWire, id acp.SessionId) int {
				return wire.send(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": "executor", "value": "extra"})
			},
			finish: func(t *testing.T, wire *acpWire, model *acpModel, requestID int) {
				var response acp.SetSessionConfigOptionResponse
				result := wire.response(t, requestID)
				if result.Error != nil || json.Unmarshal(result.Result, &response) != nil {
					t.Fatalf("set_config_option = %+v", result)
				}
				if current, _ := getACPSelectOption(response.ConfigOptions, "executor"); current != "extra" {
					t.Fatalf("response executor = %q, want extra", current)
				}
			},
		},
	}
	// 2. 关闭订阅并让 serve 拒绝打开，检查等待中和新来的请求都报错；恢复后检查请求成功、历史只显示一次、MCP 重新登记。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var requests, headed atomic.Int32
			editorServer := newACPEditorMCPServer(t, "http", &requests, &headed)
			model := newACPModel(t)
			cfg := newACPConfigWithExecutor(t, model.server.URL, "extra", t.TempDir())
			var refusing *acpSyncRefusingRuntime
			wire := startACPHostWithServer(t, cfg, func(opt *runtimeserver.RuntimeServerOpt) {
				refusing = &acpSyncRefusingRuntime{Runtime: opt.Runtime}
				opt.Runtime = refusing
			})
			wire.initialize(t)
			// 会话带一个编辑器的 MCP server，重新打开后要再次登记。
			var created acp.NewSessionResponse
			wire.call(t, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{map[string]any{"type": "http", "name": "editor", "url": editorServer.URL, "headers": []any{}}}}, &created)
			id := created.SessionId
			promptID := wire.prompt(t, id, "FIRST_QUESTION")
			model.next(t).reply <- acpModelReply{text: "FIRST_ANSWER"}
			assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
			promptID = wire.prompt(t, id, "LOST_QUESTION")
			request := model.next(t)
			refusing.isRefusing.Store(true)
			refusing.closeSessionSubscription()
			result := wire.response(t, promptID)
			if result.Error == nil || result.Error.Code != -32603 || !strings.Contains(result.Error.Message, errACPOpenRefused.Error()) {
				t.Fatalf("pending prompt = %+v, want -32603 with %q", result, errACPOpenRefused)
			}
			// Client 放弃对话后让 serve 完成这一轮，重新打开时应读到这轮的回答。
			request.reply <- acpModelReply{text: "LATE_ANSWER"}
			waitACPRuntimeIdle(t, wire)
			result = wire.response(t, tt.reopen(t, wire, id))
			if result.Error == nil || !strings.Contains(result.Error.Message, errACPOpenRefused.Error()) {
				t.Fatalf("request while serve refuses = %+v, want the open error %q", result, errACPOpenRefused)
			}
			refusing.isRefusing.Store(false)
			tt.finish(t, wire, model, tt.reopen(t, wire, id))
			// 重新打开后补发放弃期间保存的回答，已经显示过的历史不重复。
			for !strings.Contains(getACPAgentText(wire), "LATE_ANSWER") {
				wire.next(t)
			}
			user := ""
			for _, notification := range wire.updates {
				if chunk := notification.Update.UserMessageChunk; chunk != nil {
					user += chunk.Content.Text.Text
				}
			}
			text := getACPAgentText(wire)
			if strings.Count(user, "FIRST_QUESTION") != 1 || strings.Count(user, "LOST_QUESTION") != 1 ||
				strings.Count(text, "FIRST_ANSWER") != 1 || strings.Count(text, "LATE_ANSWER") != 1 {
				t.Fatalf("editor user text = %q, agent text = %q; want each message once", user, text)
			}
			if registrations := refusing.clientMCPRegistrations.Load(); registrations != 2 {
				t.Fatalf("client MCP registrations = %d, want 2: when the session opened and when it reopened the dropped conversation", registrations)
			}
		})
	}
}

// acpEditorHeaderName、acpEditorHeaderValue 是编辑器给 http、sse 类 MCP server 配置的请求头，本机 server 收到的每个请求都应该带着它。
const (
	acpEditorHeaderName  = "X-Editor-Token"
	acpEditorHeaderValue = "editor-secret"
)

// newACPEditorMCPServer 启动编辑器一侧带 echo 工具的 MCP server，统计请求数和带了配置请求头的请求数。
func newACPEditorMCPServer(t *testing.T, transport string, requests, headed *atomic.Int32) *httptest.Server {
	t.Helper()
	// 1. 创建 MCP server 并注册 echo；不声明 tools/list_changed，客户端就不发 subscriptions/listen 长请求。
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "editor",
		Version: "1.0.0",
	}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}})
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echo",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
	},
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var arguments struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: arguments.Text}}}, nil
		})
	// 2. 按 transport 创建 HTTP 或 SSE 处理器，统计全部请求和带配置请求头的请求。
	getServer := func(*http.Request) *mcp.Server { return server }
	handler := http.Handler(mcp.NewStreamableHTTPHandler(getServer, nil))
	if transport == "sse" {
		handler = mcp.NewSSEHandler(getServer, nil)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get(acpEditorHeaderName) == acpEditorHeaderValue {
			headed.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	return httpServer
}

// getACPModelToolNames 取出模型请求里声明的工具名。
func getACPModelToolNames(body map[string]any) []string {
	// 1. 遍历 tools 列表并收集 function.name。
	tools, _ := body["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		function, _ := tool.(map[string]any)["function"].(map[string]any)
		name, _ := function["name"].(string)
		names = append(names, name)
	}
	return names
}

// getACPModelToolResult 解析模型请求里最后一条工具消息的 ToolResult。
func getACPModelToolResult(t *testing.T, body map[string]any) defs.ToolResult {
	t.Helper()
	// 1. 从后往前找工具消息，拼接正文并解析；没有工具消息时结束测试。
	messages, _ := body["messages"].([]any)
	for _, message := range slices.Backward(messages) {
		fields, _ := message.(map[string]any)
		if fields["role"] != "tool" {
			continue
		}
		var text strings.Builder
		switch content := fields["content"].(type) {
		case string:
			text.WriteString(content)
		case []any:
			for _, part := range content {
				partFields, _ := part.(map[string]any)
				partText, _ := partFields["text"].(string)
				text.WriteString(partText)
			}
		}
		var result defs.ToolResult
		if err := json.Unmarshal([]byte(text.String()), &result); err != nil {
			t.Fatalf("tool message %q is not a tool result: %v", text.String(), err)
		}
		return result
	}
	t.Fatal("model request has no tool message")
	return defs.ToolResult{}
}

// TestACPClientMCPServers 检查编辑器带来的 MCP server 经转发出现在模型的工具里、能调用且带着请求头，acp 类型被跳过，重复加载时整体替换。
func TestACPClientMCPServers(t *testing.T) {
	// 1. 列出 http、sse、acp 类型被跳过和重复加载替换四种场景。
	getEditorServer := func(transport, name, url string) map[string]any {
		return map[string]any{"type": transport, "name": name, "url": url, "headers": []any{map[string]any{"name": acpEditorHeaderName, "value": acpEditorHeaderValue}}}
	}
	tests := []struct {
		name string
		// transport 是起的 MCP server 的类型（http 或 sse）；getServers 用它的地址生成 session/new 带的 mcpServers，getLoadedServers 非 nil 时
		// 再生成同一个会话 session/load 带的 mcpServers。
		transport        string
		getServers       func(url string) []any
		getLoadedServers func(url string) []any
		wantTool         string
		wantLoadedTool   string
	}{
		{
			name:      "http",
			transport: "http",
			wantTool:  "mcp__My_Server__echo",
			getServers: func(url string) []any {
				return []any{getEditorServer("http", "My Server", url)}
			},
		},
		{
			name:      "sse",
			transport: "sse",
			wantTool:  "mcp__fs__echo",
			getServers: func(url string) []any {
				return []any{getEditorServer("sse", "fs", url)}
			},
		},
		{
			name:      "acp_type_skipped",
			transport: "http",
			wantTool:  "",
			getServers: func(url string) []any {
				return []any{map[string]any{"type": "acp", "id": "editor-1", "name": "editor"}}
			},
		},
		{
			name:             "load_replaces_servers",
			transport:        "http",
			wantTool:         "mcp__first__echo",
			wantLoadedTool:   "mcp__second__echo",
			getServers:       func(url string) []any { return []any{getEditorServer("http", "first", url)} },
			getLoadedServers: func(url string) []any { return []any{getEditorServer("http", "second", url)} },
		},
	}
	// 2. 新建会话并调用编辑器的工具，检查结果和请求头；重复加载后检查只剩新工具。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var requests, headed atomic.Int32
			editorServer := newACPEditorMCPServer(t, tt.transport, &requests, &headed)
			model := newACPModel(t)
			cfg, _ := newACPConfig(t, model.server.URL)
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			directory := t.TempDir()
			var created acp.NewSessionResponse
			wire.call(t, "session/new", map[string]any{"cwd": directory, "mcpServers": tt.getServers(editorServer.URL)}, &created)

			// runTurn 检查本轮工具列表，调用指定工具并检查结果；未指定工具时检查列表不含 MCP 工具。
			runTurn := func(text, wantTool, unwantedTool string) {
				t.Helper()
				promptID := wire.prompt(t, created.SessionId, text)
				request := model.next(t)
				names := getACPModelToolNames(request.body)
				if wantTool != "" && !slices.Contains(names, wantTool) {
					t.Fatalf("model tools = %v, want %s", names, wantTool)
				}
				if wantTool == "" && slices.ContainsFunc(names, func(name string) bool { return strings.HasPrefix(name, "mcp__") }) {
					t.Fatalf("model tools = %v, want no MCP tools", names)
				}
				if unwantedTool != "" && slices.Contains(names, unwantedTool) {
					t.Fatalf("model tools = %v, want no %s", names, unwantedTool)
				}
				if wantTool != "" {
					request.reply <- acpModelReply{
						tool:      wantTool,
						arguments: `{"text":"hi"}`,
					}
					request = model.next(t)
					if result := getACPModelToolResult(t, request.body); result.Error != nil || string(result.Content) != `"hi"` {
						t.Fatalf("tool result = content %s, error %+v, want content \"hi\" and no error", result.Content, result.Error)
					}
				}
				request.reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
			}
			runTurn("first", tt.wantTool, "")
			if tt.wantTool != "" && (requests.Load() == 0 || requests.Load() != headed.Load()) {
				t.Fatalf("editor server got %d requests, %d with the configured header, want all of them", requests.Load(), headed.Load())
			}
			if tt.getLoadedServers != nil {
				wire.call(t, "session/load", map[string]any{"sessionId": created.SessionId, "cwd": directory, "mcpServers": tt.getLoadedServers(editorServer.URL)}, nil)
				runTurn("second", tt.wantLoadedTool, tt.wantTool)
			}
		})
	}
}

// newACPConfigWithExecutor 创建多一个 local 类 Executor 的测试配置。
func newACPConfigWithExecutor(t *testing.T, modelURL, id, directory string) *config.Config {
	t.Helper()
	// 1. 写入 id 和 directory 对应的 Executor 配置，加载后返回。
	_, configPath := newACPConfig(t, modelURL)
	writeTestFiles(t, map[string]string{filepath.Join(filepath.Dir(configPath), "executors", id+".yaml"): fmt.Sprintf("id: %s\ndescription: Second local directory\nkind: local\nworkspace_directory: %s\n", id, directory)})
	cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// getACPSelectOption 返回 id 对应下拉框的当前值和全部选项值。
func getACPSelectOption(options []acp.SessionConfigOption, id string) (string, []string) {
	// 1. 按 id 查找下拉框并收集选项值，找不到时返回空。
	for _, option := range options {
		if option.Select == nil || option.Select.Id != acp.SessionConfigId(id) {
			continue
		}
		var values []string
		for _, value := range *option.Select.Options.Ungrouped {
			values = append(values, string(value.Value))
		}
		return string(option.Select.CurrentValue), values
	}
	return "", nil
}

// waitACPRuntimeIdle 等测试 Runtime 里没有 Turn 在执行。
func waitACPRuntimeIdle(t *testing.T, wire *acpWire) {
	t.Helper()
	// 1. 10 秒内每 10 毫秒检查一次，出错或超时时结束测试。
	deadline := time.Now().Add(10 * time.Second)
	for {
		isIdle, err := wire.runtime.IsIdle(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if isIdle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not become idle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// errACPOpenRefused 是 acpSyncRefusingRuntime 拒绝打开对话时返回的错误。
var errACPOpenRefused = errors.New(`executor "extra": connect refused`)

// acpSyncRefusingRuntime 包住测试 Runtime：能按开关拒绝打开对话、取消最近一次 Session 订阅，并统计客户端 MCP 登记次数。
type acpSyncRefusingRuntime struct {
	runtime.Runtime
	// isRefusing 是否拒绝 OpenConversation？
	isRefusing atomic.Bool
	// cancelSessionSubscription 是最近一次 SubscribeSession 的取消函数。
	cancelSessionSubscription atomic.Pointer[context.CancelFunc]
	// clientMCPRegistrations 是 serve 收到的客户端 MCP 登记次数。
	clientMCPRegistrations atomic.Int32
}

// SetClientMCPServers 记一次登记，再交给测试 Runtime。
func (r *acpSyncRefusingRuntime) SetClientMCPServers(ctx context.Context, request *runtime.SetClientMCPServersRequest) error {
	// 1. 登记次数加一，交给测试 Runtime。
	r.clientMCPRegistrations.Add(1)
	return r.Runtime.SetClientMCPServers(ctx, request)
}

// OpenConversation 开关打开时拒绝打开对话，否则交给测试 Runtime。
func (r *acpSyncRefusingRuntime) OpenConversation(ctx context.Context, request *runtime.OpenConversationRequest) (defs.Conversation, error) {
	// 1. isRefusing 为 true 时返回 errACPOpenRefused，否则交给测试 Runtime。
	if r.isRefusing.Load() {
		return defs.Conversation{}, errACPOpenRefused
	}
	return r.Runtime.OpenConversation(ctx, request)
}

// SubscribeSession 订阅 Session，并记下这次订阅的取消函数。
func (r *acpSyncRefusingRuntime) SubscribeSession(ctx context.Context, request *runtime.SubscribeSessionRequest) (*runtime.SessionSubscription, error) {
	// 1. 创建可取消的 context，保存取消函数，再交给测试 Runtime 订阅。
	ctx, cancel := context.WithCancel(ctx)
	r.cancelSessionSubscription.Store(&cancel)
	return r.Runtime.SubscribeSession(ctx, request)
}

// closeSessionSubscription 取消最近一次 Session 订阅。
func (r *acpSyncRefusingRuntime) closeSessionSubscription() {
	// 1. 调用保存的取消函数。
	(*r.cancelSessionSubscription.Load())()
}
