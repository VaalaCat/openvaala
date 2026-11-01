package appserver

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// fakeThreadClient 是只实现打开和恢复 Thread 用到的调用的 runtimeclient.Client。
type fakeThreadClient struct {
	runtimeclient.Client
	events   chan runtimeclient.ClientEvent
	record   defs.Conversation
	details  conversation.SessionDetails
	titleErr error
}

// Events 返回测试 Client 的事件通道。
func (f *fakeThreadClient) Events() <-chan runtimeclient.ClientEvent {
	// 1. 返回已设置的通道。
	return f.events
}

// OpenConversation 返回预设对话记录。
func (f *fakeThreadClient) OpenConversation(context.Context, *runtimeclient.OpenConversationRequest) (defs.Conversation, error) {
	// 1. 返回预设的对话记录。
	return f.record, nil
}

// GetConversation 返回预设对话记录。
func (f *fakeThreadClient) GetConversation(context.Context, defs.ConversationID) (defs.Conversation, error) {
	// 1. 读取测试记录。
	return f.record, nil
}

// GetSessionDetails 返回预设 Session 详情。
func (f *fakeThreadClient) GetSessionDetails(context.Context, defs.ConversationID, defs.SessionID) (conversation.SessionDetails, error) {
	// 1. 读取模型和 Executor 等测试详情。
	return f.details, nil
}

// SetConversationTitle 保存测试标题，并按需交付标题事件。
func (f *fakeThreadClient) SetConversationTitle(_ context.Context, _ defs.ConversationID, title string) error {
	// 1. 预设错误时返回错误，否则更新标题并向非空事件通道写出标题变化。
	if f.titleErr != nil {
		return f.titleErr
	}
	f.record.Title = title
	if f.events != nil {
		f.events <- &runtimeclient.ConversationTitleChanged{
			ConversationID: f.record.ID,
			Title:          title,
		}
	}
	return nil
}

// TestConnectionThreadTitleNotifications 检查标题事件和重新同步的通知。
func TestConnectionThreadTitleNotifications(t *testing.T) {
	// 1. 列出改名、清空、子 Thread 和重新同步场景。
	tests := []struct {
		name       string
		threadID   string
		title      string
		isResync   bool
		wantNotify bool
	}{
		{
			name:       "automatic_title",
			threadID:   "conversation",
			title:      "修复登录",
			wantNotify: true,
		},
		{
			name:       "explicit_rename",
			threadID:   "conversation",
			title:      "用户改名",
			wantNotify: true,
		},
		{
			name:       "clear_title",
			threadID:   "conversation",
			wantNotify: true,
		},
		{
			name:     "sub_agent_keeps_own_name",
			threadID: "sub-session",
			title:    "根对话标题",
		},
		{
			name:       "resync_changed_title",
			threadID:   "conversation",
			title:      "重连后的标题",
			isResync:   true,
			wantNotify: true,
		},
		{
			name:     "resync_unchanged_title",
			threadID: "conversation",
			title:    "旧标题",
			isResync: true,
		},
	}
	// 2. 处理事件，核对是否恰好发送预期标题通知。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outgoing := make(chan codexMessage, 2)
			thread := &connectionThread{
				threadID:       tt.threadID,
				conversationID: "conversation",
				sessionID:      "session",
				conversationRecord: defs.Conversation{
					ID:    "conversation",
					Title: "旧标题",
				},
				isOpenDelivered: true,
				isSynced:        true,
				outgoing:        outgoing,
				client:          &fakeThreadClient{events: make(chan runtimeclient.ClientEvent)},
			}
			var event runtimeclient.ClientEvent = &runtimeclient.ConversationTitleChanged{
				ConversationID: "conversation",
				Title:          tt.title,
			}
			if tt.isResync {
				event = &runtimeclient.ConversationSynced{Conversation: defs.Conversation{
					ID:    "conversation",
					Title: tt.title,
				}}
			}
			if err := thread.handleClientEvent(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			if tt.wantNotify {
				select {
				case notification := <-outgoing:
					var params codexThreadNameUpdatedNotification
					if err := json.Unmarshal(notification.Params, &params); err != nil {
						t.Fatal(err)
					}
					if notification.Method != "thread/name/updated" || params.ThreadID != "conversation" || params.ThreadName != tt.title {
						t.Fatalf("title notification = %+v, params %+v", notification, params)
					}
				default:
					t.Fatal("missing title notification")
				}
			}
			select {
			case notification := <-outgoing:
				t.Fatalf("unexpected notification = %+v", notification)
			default:
			}
		})
	}
}

// TestConnectionThreadReportsSelectedModel 检查打开和恢复 Thread 报告的模型选择。
func TestConnectionThreadReportsSelectedModel(t *testing.T) {
	// 1. 准备目录内模型、默认模型和已移除模型。
	effort := func(value string) *string { return &value }
	tests := []struct {
		name         string
		session      defs.ModelSelection   // Session 记录里的选择
		model        *defs.ModelDefinition // Session 详情里的 Model；nil 表示所选模型不在目录里
		wantModel    string                // Thread 报告的 model
		wantEffort   *string               // Thread 报告的 reasoningEffort
		wantProvider string                // Thread 报告的 modelProvider
	}{
		{
			name: "selected_model_in_catalog",
			session: defs.ModelSelection{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "a",
				},
				ReasoningEffort: "high",
			},
			model: &defs.ModelDefinition{Ref: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "a",
			}},
			wantModel:    "test/a",
			wantEffort:   effort("high"),
			wantProvider: "test",
		},
		{
			name:    "zero_selection_reports_agent_default_model",
			session: defs.ModelSelection{},
			model: &defs.ModelDefinition{Ref: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "default",
			}},
			wantModel:    "test/default",
			wantProvider: "test",
		},
		{
			name: "selected_model_removed_reports_selected_ref",
			session: defs.ModelSelection{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "gone",
				},
				ReasoningEffort: "high",
			},
			wantModel:    "test/gone",
			wantEffort:   effort("high"),
			wantProvider: "test",
		},
	}
	// 2. 分别打开和恢复 Thread，核对模型、Provider 和推理强度。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const conversationID defs.ConversationID = "conversation-1"
			const sessionID defs.SessionID = "session-1"
			client := &fakeThreadClient{
				events: make(chan runtimeclient.ClientEvent),
				record: defs.Conversation{
					ID:            conversationID,
					RootSessionID: sessionID,
				},
				details: conversation.SessionDetails{
					ConversationID: conversationID,
					Session: defs.Session{
						ID:             sessionID,
						ConversationID: conversationID,
						AgentName:      "assistant",
						Model:          tt.session,
					},
					Model: tt.model,
				},
			}
			outgoing := make(chan codexMessage, 8)
			newThread := func() *connectionThread {
				return &connectionThread{
					threadID:        string(conversationID),
					conversationID:  conversationID,
					sessionID:       sessionID,
					client:          client,
					outgoing:        outgoing,
					isOpenDelivered: true,
				}
			}
			requireThread := func(t *testing.T, operation string, thread codexThread) {
				t.Helper()
				if thread.Model != tt.wantModel || thread.ModelProvider != tt.wantProvider ||
					(thread.ReasoningEffort == nil) != (tt.wantEffort == nil) || (tt.wantEffort != nil && *thread.ReasoningEffort != *tt.wantEffort) {
					t.Fatalf("%s thread model = %q provider %q effort %v, want %q %q %v",
						operation, thread.Model, thread.ModelProvider, thread.ReasoningEffort, tt.wantModel, tt.wantProvider, tt.wantEffort)
				}
			}

			opened, err := newThread().open(t.Context())
			if err != nil {
				t.Fatalf("open() error = %v", err)
			}
			requireThread(t, "open", opened)

			newThread().handleThreadResumeRequested(t.Context(), &threadResumeRequested{requestID: json.RawMessage(`1`)})
			response := <-outgoing
			result, ok := response.Result.(codexThreadOpenResult)
			if response.Error != nil || !ok {
				t.Fatalf("thread/resume response = %+v, want a thread", response)
			}
			requireThread(t, "resume", result.Thread)
		})
	}
}

// TestConnectionThreadHookNotifications 检查 hook 通知的顺序和完整运行内容。
func TestConnectionThreadHookNotifications(t *testing.T) {
	// 1. 准备各事件和运行状态，设置时间、范围及输出条目的预期 JSON。
	const turnID defs.TurnID = "turn-1"
	newRun := func(event defs.HookEventName, status defs.HookRunStatus, edit func(run *defs.HookRun)) *defs.HookRun {
		run := &defs.HookRun{
			ID:           "hook-run-1",
			Event:        event,
			ExecutorID:   "local",
			SourcePath:   "/hooks/go.yaml",
			Handler:      "check.sh",
			DisplayOrder: 3,
			Status:       status,
			StartedAt:    1_700_000_000_123,
		}
		if status != defs.HookRunRunning {
			run.CompletedAt = 1_700_000_002_456
		}
		if edit != nil {
			edit(run)
		}
		return run
	}
	// wantRun 拼出 run 的预期 JSON；statusMessage、source、executionMode、handlerType 在 OpenVaala 里总是固定值。
	wantRun := func(event, scope, status, times, entries string) string {
		return `{"id":"hook-run-1","eventName":"` + event + `","handlerType":"command","executionMode":"sync","scope":"` + scope +
			`","sourcePath":"/hooks/go.yaml","source":"user","displayOrder":3,"status":"` + status + `","statusMessage":null,` + times + `,"entries":` + entries + `}`
	}
	const runningTimes = `"startedAt":1700000000,"completedAt":null,"durationMs":null`
	const finishedTimes = `"startedAt":1700000000,"completedAt":1700000002,"durationMs":2333`
	tests := []struct {
		name        string
		runs        []*defs.HookRun
		wantMethods []string
		wantRuns    []string
	}{
		{
			name:        "session_start_running_is_thread_scoped_started",
			runs:        []*defs.HookRun{newRun(defs.HookEventSessionStart, defs.HookRunRunning, nil)},
			wantMethods: []string{"hook/started"},
			wantRuns:    []string{wantRun("sessionStart", "thread", "running", runningTimes, `[]`)},
		},
		{
			name:        "subagent_start_is_thread_scoped",
			runs:        []*defs.HookRun{newRun(defs.HookEventSubagentStart, defs.HookRunCompleted, nil)},
			wantMethods: []string{"hook/completed"},
			wantRuns:    []string{wantRun("subagentStart", "thread", "completed", finishedTimes, `[]`)},
		},
		{
			name: "completed_maps_every_output_to_an_entry",
			runs: []*defs.HookRun{newRun(defs.HookEventPostToolUse, defs.HookRunCompleted, func(run *defs.HookRun) {
				run.AdditionalContext, run.BlockReason, run.StopReason, run.SystemMessage, run.ErrorMessage = "补充", "原因", "停止", "提示", "出错"
			})},
			wantMethods: []string{"hook/completed"},
			wantRuns: []string{wantRun("postToolUse", "turn", "completed", finishedTimes,
				`[{"kind":"context","text":"补充"},{"kind":"feedback","text":"原因"},{"kind":"stop","text":"停止"},{"kind":"warning","text":"提示"},{"kind":"error","text":"出错"}]`)},
		},
		{
			name: "blocked_carries_the_reason_as_feedback",
			runs: []*defs.HookRun{newRun(defs.HookEventPreToolUse, defs.HookRunBlocked, func(run *defs.HookRun) {
				run.BlockReason = "不要用 rm -rf"
			})},
			wantMethods: []string{"hook/completed"},
			wantRuns:    []string{wantRun("preToolUse", "turn", "blocked", finishedTimes, `[{"kind":"feedback","text":"不要用 rm -rf"}]`)},
		},
		{
			name: "stopped_carries_the_stop_reason",
			runs: []*defs.HookRun{newRun(defs.HookEventStop, defs.HookRunStopped, func(run *defs.HookRun) {
				run.StopReason = "测试没过"
			})},
			wantMethods: []string{"hook/completed"},
			wantRuns:    []string{wantRun("stop", "turn", "stopped", finishedTimes, `[{"kind":"stop","text":"测试没过"}]`)},
		},
		{
			name: "failed_carries_the_error",
			runs: []*defs.HookRun{newRun(defs.HookEventUserPromptSubmit, defs.HookRunFailed, func(run *defs.HookRun) {
				run.ErrorMessage = "timed out after 10s"
			})},
			wantMethods: []string{"hook/completed"},
			wantRuns:    []string{wantRun("userPromptSubmit", "turn", "failed", finishedTimes, `[{"kind":"error","text":"timed out after 10s"}]`)},
		},
		{
			name:        "subagent_stop_is_turn_scoped",
			runs:        []*defs.HookRun{newRun(defs.HookEventSubagentStop, defs.HookRunCompleted, nil)},
			wantMethods: []string{"hook/completed"},
			wantRuns:    []string{wantRun("subagentStop", "turn", "completed", finishedTimes, `[]`)},
		},
		{
			name: "started_then_completed_of_one_run_share_the_turn_started",
			runs: []*defs.HookRun{
				newRun(defs.HookEventPreToolUse, defs.HookRunRunning, nil),
				newRun(defs.HookEventPreToolUse, defs.HookRunCompleted, nil),
			},
			wantMethods: []string{"hook/started", "hook/completed"},
			wantRuns: []string{
				wantRun("preToolUse", "turn", "running", runningTimes, `[]`),
				wantRun("preToolUse", "turn", "completed", finishedTimes, `[]`),
			},
		},
	}
	// 2. 交付运行记录，核对 Turn 开始先于 hook 通知，以及事件名、时间、范围和输出条目。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outgoing := make(chan codexMessage, 8)
			thread := &connectionThread{
				threadID:        "conversation",
				conversationID:  "conversation",
				sessionID:       "session",
				isOpenDelivered: true,
				isSynced:        true,
				outgoing:        outgoing,
				client:          &fakeThreadClient{events: make(chan runtimeclient.ClientEvent)},
			}
			for _, run := range tt.runs {
				if err := thread.handleClientEvent(t.Context(), &runtimeclient.HookRunChanged{
					ConversationID: "conversation",
					TurnID:         turnID,
					Run:            run,
				}); err != nil {
					t.Fatal(err)
				}
			}
			started := <-outgoing
			if started.Method != "turn/started" {
				t.Fatalf("first notification = %s, want turn/started before the hook notifications", started.Method)
			}
			for index, wantMethod := range tt.wantMethods {
				notification := <-outgoing
				if notification.Method != wantMethod {
					t.Fatalf("notification %d = %s, want %s", index, notification.Method, wantMethod)
				}
				wantParams := `{"threadId":"conversation","turnId":"turn-1","run":` + tt.wantRuns[index] + `}`
				var got, want any
				if err := json.Unmarshal(notification.Params, &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(wantParams), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s params = %s, want %s", notification.Method, notification.Params, wantParams)
				}
			}
			select {
			case extra := <-outgoing:
				t.Fatalf("unexpected extra notification = %+v", extra)
			default:
			}
		})
	}
}
