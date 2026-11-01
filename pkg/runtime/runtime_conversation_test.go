package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestRuntimeSessionSubscription 检查 Session 内容订阅的初始信息、断线续传、慢订阅和关闭原因。
func TestRuntimeSessionSubscription(t *testing.T) {
	// 1. 列出初始信息、续传、慢订阅与关闭的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "snapshot_mid_generation",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "天气怎么样", "")
				release := make(chan struct{})
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  []string{"今天", "晴"},
					release: release,
				}
				collectUntil(t, watcher.Events, isMessageDelta)

				late := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				if late.LastSavedOrdinal != 1 || len(late.StreamingItems) != 1 || messageText(late.StreamingItems[0].Item) != "今天" {
					t.Fatalf("snapshot = %d %+v", late.LastSavedOrdinal, late.StreamingItems)
				}
				close(release)
				text := messageText(late.StreamingItems[0].Item)
				var savedText string
				for _, event := range collectUntil(t, late.Events, isTurnFinished) {
					switch sessionEvent := getSessionEvent(event).(type) {
					case *session.SessionAgentEvent:
						if sessionEvent.Event.Type == agent.AgentEventItemDelta {
							text += sessionEvent.Event.Delta.Text
						}
					case *session.SessionItemsSaved:
						for _, logged := range sessionEvent.Items {
							if logged.Ordinal != 2 {
								t.Fatalf("saved ordinal = %d, want 2", logged.Ordinal)
							}
							savedText = messageText(logged.Item)
						}
					}
				}
				if text != "今天晴" || savedText != "今天晴" {
					t.Fatalf("stream text = %q, saved = %q", text, savedText)
				}
			},
		},
		{
			name: "snapshot_item_index_after_saved_reasoning",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "天气怎么样", "")
				release := make(chan struct{})
				h.model.receive(t).reply <- runtimeModelResponse{
					reasoning: "思考中",
					chunks:    []string{"今天", "晴"},
					release:   release,
				}
				watched := collectUntil(t, watcher.Events, isMessageDelta)
				messageIndex := getSessionEvent(watched[len(watched)-1]).(*session.SessionAgentEvent).Event.ItemIndex

				// 推理在消息开始时完成并保存，已移出生成中 Item 列表：消息在列表中的位置与 ItemIndex 不同，增量只能靠 ItemIndex 定位。
				late := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				if late.LastSavedOrdinal != 2 || len(late.StreamingItems) != 1 || messageIndex == 0 {
					t.Fatalf("snapshot = %d %+v, message ItemIndex = %d", late.LastSavedOrdinal, late.StreamingItems, messageIndex)
				}
				message := late.StreamingItems[0]
				if message.ItemIndex != messageIndex || message.Item.Kind != defs.ItemMessage || messageText(message.Item) != "今天" {
					t.Fatalf("snapshot item = %d %+v, want message at ItemIndex %d", message.ItemIndex, message.Item, messageIndex)
				}
				close(release)
				items := map[int]*defs.Item{message.ItemIndex: message.Item}
				for _, event := range collectUntil(t, late.Events, isTurnFinished) {
					agentEvent, ok := getSessionEvent(event).(*session.SessionAgentEvent)
					if !ok || agentEvent.Event.Type != agent.AgentEventItemDelta {
						continue
					}
					item, found := items[agentEvent.Event.ItemIndex]
					if !found {
						t.Fatalf("delta ItemIndex %d is not in snapshot", agentEvent.Event.ItemIndex)
					}
					agent.ApplyItemDelta(item, agentEvent.Event.Delta)
				}
				if text := messageText(message.Item); text != "今天晴" {
					t.Fatalf("message text = %q, want 今天晴", text)
				}
			},
		},
		{
			name: "resume_without_gap",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "第一轮", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"一"}}
				collectUntil(t, watcher.Events, isTurnFinished)

				// 第一轮的全部事件已转发，断线重连的客户端从这里续传。
				resumed := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				history := h.waitLog(t, conv.ID, conv.RootSessionID, 2)
				if resumed.LastSavedOrdinal != 2 || len(history) != 2 || len(resumed.StreamingItems) != 0 {
					t.Fatalf("resume point = %d, history = %d, streaming = %d", resumed.LastSavedOrdinal, len(history), len(resumed.StreamingItems))
				}
				h.submit(t, conv.ID, "第二轮", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"二"}}
				var ordinals []storage.Ordinal
				for _, event := range collectUntil(t, resumed.Events, isTurnFinished) {
					if saved, ok := getSessionEvent(event).(*session.SessionItemsSaved); ok {
						for _, logged := range saved.Items {
							ordinals = append(ordinals, logged.Ordinal)
						}
					}
				}
				if len(ordinals) != 2 || ordinals[0] != 3 || ordinals[1] != 4 {
					t.Fatalf("live ordinals = %v, want [3 4]", ordinals)
				}
			},
		},
		{
			name: "child_session_subscription",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.submit(t, conv.ID, "委派", "")
				h.model.receive(t).reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
					ID:        "spawn",
					Name:      "spawn_agent",
					Arguments: `{"task_name":"review","message":"check"}`,
				}}}
				root, child := h.model.receiveRootAndTask(t, "review")
				childID := h.getChildSessionID(t, conv)
				subscription := h.subscribeSession(t, conv.ID, childID)
				if subscription.LastSavedOrdinal < 1 {
					t.Fatalf("child resume point = %d, want at least the fork boundary", subscription.LastSavedOrdinal)
				}
				child.reply <- runtimeModelResponse{chunks: []string{"子任务完成"}}
				root.reply <- runtimeModelResponse{chunks: []string{"已委派"}}
				// 续传位置之后的保存事件必须从 LastSavedOrdinal+1 连续增长，并与最终日志尾部对齐：不缺不重。
				next := subscription.LastSavedOrdinal + 1
				for _, event := range collectUntil(t, subscription.Events, isTurnFinished) {
					sessionEvent, ok := event.(*conversation.ConversationSessionEvent)
					if ok && sessionEvent.SessionID != childID {
						t.Fatalf("child subscription received event of %s", sessionEvent.SessionID)
					}
					if saved, ok := getSessionEvent(event).(*session.SessionItemsSaved); ok {
						for _, logged := range saved.Items {
							if logged.Ordinal != next {
								t.Fatalf("live ordinal = %d, want %d", logged.Ordinal, next)
							}
							next++
						}
					}
				}
				childLog := h.waitLog(t, conv.ID, childID, int(next-1))
				if childLog[len(childLog)-1].Ordinal != next-1 {
					t.Fatalf("child log tail = %d, want %d", childLog[len(childLog)-1].Ordinal, next-1)
				}
			},
		},
		{
			name: "subscribe_unstarted_child_after_restart",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				rootWatcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "委派", "")
				h.model.receive(t).reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
					ID:        "spawn",
					Name:      "spawn_agent",
					Arguments: `{"task_name":"review","message":"check"}`,
				}}}
				root, child := h.model.receiveRootAndTask(t, "review")
				childID := h.getChildSessionID(t, conv)
				childWatcher := h.subscribeSession(t, conv.ID, childID)
				root.reply <- runtimeModelResponse{chunks: []string{"已委派"}}
				collectUntil(t, rootWatcher.Events, isTurnFinished)
				child.reply <- runtimeModelResponse{chunks: []string{"子任务完成"}}
				collectUntil(t, childWatcher.Events, isTurnFinished)
				// 子任务结果作为通知触发根 Session 的下一轮。
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"已汇总"}}
				collectUntil(t, rootWatcher.Events, isTurnFinished)
				h.stop(t)

				// 重启后订阅子 Session 会加载对话但只启动根 Session：续传位置来自日志最新流水号。
				restarted := startTestRuntime(t, h.store, h.model)
				if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				subscription := restarted.subscribeSession(t, conv.ID, childID)
				childLog := restarted.waitLog(t, conv.ID, childID, 1)
				if tail := childLog[len(childLog)-1].Ordinal; subscription.LastSavedOrdinal != tail || len(subscription.StreamingItems) != 0 {
					t.Fatalf("resume point = %d, streaming = %d; want log tail %d", subscription.LastSavedOrdinal, len(subscription.StreamingItems), tail)
				}
			},
		},
		{
			name: "foreign_session_rejected",
			run: func(t *testing.T, h *runtimeHarness) {
				a, b := h.create(t), h.create(t)
				request := &runtime.SubscribeSessionRequest{
					ConversationID: a.ID,
					SessionID:      b.RootSessionID,
				}
				if _, err := h.rt.SubscribeSession(t.Context(), request); err == nil {
					t.Fatal("SubscribeSession() of a foreign Session error = nil")
				}
			},
		},
		{
			name: "slow_subscriber_closed",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				slow := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				fast := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "长回答", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: strings.Split(strings.Repeat("字", 300), "")}
				collectUntil(t, fast.Events, isTurnFinished)
				waitEventsClosed(t, slow.Events)
				if !errors.Is(slow.GetCloseReason(), runtime.ErrSubscriberTooSlow) {
					t.Fatalf("slow close reason = %v", slow.GetCloseReason())
				}
			},
		},
		{
			name: "unsubscribe_on_ctx_done",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				ctx, cancel := context.WithCancel(t.Context())
				subscription, err := h.rt.SubscribeSession(ctx, &runtime.SubscribeSessionRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				waitEventsClosed(t, subscription.Events)
				if !errors.Is(subscription.GetCloseReason(), context.Canceled) {
					t.Fatalf("close reason = %v", subscription.GetCloseReason())
				}
			},
		},
		{
			name: "reload_after_root_exit",
			run: func(t *testing.T, h *runtimeHarness) {
				store := openRuntimeStore(t)
				store.opt.Logs = &unsettledTurnLogs{LogStorage: store.opt.Logs}
				failing := startTestRuntime(t, store, h.model)
				conv := failing.create(t)
				subscription := failing.subscribeSession(t, conv.ID, conv.RootSessionID)
				failing.submit(t, conv.ID, "第一轮", "")
				failing.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"一"}}
				waitEventsClosed(t, subscription.Events)
				if subscription.GetCloseReason() == nil || !strings.Contains(subscription.GetCloseReason().Error(), "has stopped") {
					t.Fatalf("close reason = %v", subscription.GetCloseReason())
				}
				if _, err := failing.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				failing.submit(t, conv.ID, "重新加载", "")
				failing.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"二"}}
				failing.waitLog(t, conv.ID, conv.RootSessionID, 4)
			},
		},
		{
			name: "run_stops_with_unread_subscriber",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				unread := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "长回答", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  strings.Split(strings.Repeat("字", 300), ""),
					release: make(chan struct{}),
				}
				if err := h.stop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() = %v", err)
				}
				waitEventsClosed(t, unread.Events)
			},
		},
	}
	// 2. 运行对话，检查订阅初始日志位置、生成中内容、事件次序、慢订阅及结束原因。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t)))
		})
	}
}

// TestRuntimeRunningChangesIgnoreContent 检查文字和用量变化不会重新推送全局运行集合。
func TestRuntimeRunningChangesIgnoreContent(t *testing.T) {
	// 1. 分别检查普通文字增量和带用量的流，等待运行集合已经包含对话。
	for _, withUsage := range []bool{false, true} {
		t.Run(fmt.Sprintf("usage_%v", withUsage), func(t *testing.T) {
			h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
			conv := h.create(t)
			subscription, err := h.rt.SubscribeRunningConversations(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			watcher := h.subscribeSession(t, conv.ID, conv.RootSessionID)
			h.submit(t, conv.ID, "流式回复", "")
			request := h.model.receive(t)
			waitRunningConversations(t, subscription.Events, []defs.ConversationID{conv.ID})
			// 2. 模型开始输出但尚未结束 Turn，只观察内容事件，不应收到新的运行集合。
			release := make(chan struct{})
			response := runtimeModelResponse{chunks: []string{"一", "二"}, release: release}
			if withUsage {
				response.usage = &runtimeModelUsage{input: 100, output: 2}
			}
			request.reply <- response
			collectUntil(t, watcher.Events, isMessageDelta)
			select {
			case event := <-subscription.Events:
				t.Fatalf("content caused running update: %+v", event)
			case <-time.After(50 * time.Millisecond):
			}
			// 3. 结束 Turn 后只推送一次空集合。
			close(release)
			collectUntil(t, watcher.Events, isTurnFinished)
			waitRunningConversations(t, subscription.Events, nil)
			select {
			case event := <-subscription.Events:
				t.Fatalf("extra running update after usage: %+v", event)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// TestRuntimeConversationSubscription 检查对话订阅初始信息与关闭原因。
func TestRuntimeConversationSubscription(t *testing.T) {
	// 1. 列出对话订阅初始信息与关闭原因的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness, conv defs.Conversation, subscription *runtime.ConversationSubscription)
	}{
		{
			// 晚到订阅的初始信息包含活动 Turn 状态和已测量占用，与最新推送一致。
			name: "late_subscriber_starts_from_projection",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, subscription *runtime.ConversationSubscription) {
				h.submit(t, conv.ID, "长任务", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks:  []string{"处理中", "完成"},
					release: make(chan struct{}),
				}
				infos := collectUntil(t, subscription.Events, func(info runtime.SessionRuntimeInfo) bool {
					return info.State == session.SessionStateRunning && info.Context.Source == runtime.ContextUsageSourceEstimated
				})
				latest := infos[len(infos)-1]
				late := h.subscribeConversation(t, conv.ID)
				if len(late.Sessions) != 1 || late.Sessions[0] != latest || latest.ActiveTurnID == "" {
					t.Fatalf("late Sessions = %+v, want [%+v] with an active Turn", late.Sessions, latest)
				}
			},
		},
		{
			name: "closed_when_runtime_stops",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, subscription *runtime.ConversationSubscription) {
				h.stop(t)
				waitEventsClosed(t, subscription.Events)
				if subscription.GetCloseReason() == nil {
					t.Fatal("close reason = nil")
				}
			},
		},
		{
			name: "unsubscribe_on_ctx_done",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, _ *runtime.ConversationSubscription) {
				ctx, cancel := context.WithCancel(t.Context())
				subscription, err := h.rt.SubscribeConversation(ctx, &runtime.SubscribeConversationRequest{ConversationID: conv.ID})
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				waitEventsClosed(t, subscription.Events)
				if !errors.Is(subscription.GetCloseReason(), context.Canceled) {
					t.Fatalf("close reason = %v", subscription.GetCloseReason())
				}
			},
		},
	}
	// 2. 启动并订阅对话，检查晚到订阅的 Session 信息和订阅关闭原因。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
			conv := h.create(t)
			subscription := h.subscribeConversation(t, conv.ID)
			if len(subscription.Sessions) != 1 || subscription.Sessions[0].Session.ID != conv.RootSessionID ||
				subscription.Sessions[0].Context.Source != runtime.ContextUsageSourceUnknown {
				t.Fatalf("Sessions = %+v, want the root Session with unknown context usage", subscription.Sessions)
			}
			tt.run(t, h, conv, subscription)
		})
	}
}

// TestRuntimeConversationTitleEvents 检查标题保存成功时推送标题变化，保存失败和标题没变时不推送。
func TestRuntimeConversationTitleEvents(t *testing.T) {
	// 1. 逐个运行场景：自动命名成功后手动改名、自动命名保存失败、手动改名保存失败。
	for _, scenario := range []string{"automatic_and_explicit", "automatic_save_failure", "explicit_save_failure"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 创建带标题保存故障开关的 Runtime 和对话订阅；手动改名失败场景先提交一条只有附带材料的输入，
			// 把对话存进共用库且不自动命名。
			store := openRuntimeStore(t)
			logs := &titleFailingLogs{LogStorage: store.opt.Logs}
			store.opt.Logs = logs
			h := startTestRuntime(t, store, newRuntimeModel(t))
			conv := h.create(t)
			watcher := h.subscribeConversation(t, conv.ID)
			if scenario == "explicit_save_failure" {
				request := newSubmitInputRequest(conv.ID, "附带材料", "")
				request.Content[0].Text.IsContext = true
				if _, err := h.rt.SubmitInput(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				collectUntil(t, watcher.Events, isTurnCompletedInfo)
			}
			// 2. 打开故障开关，按场景触发自动或显式标题保存，核对返回错误与首次推送。
			logs.fail.Store(scenario != "automatic_and_explicit")
			if scenario == "explicit_save_failure" {
				if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: conv.ID,
					Title:          "无法保存",
				}); err == nil {
					t.Fatal("title save should fail")
				}
			} else {
				h.submit(t, conv.ID, " 自动\t标题\n说明", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好"}}
				if scenario == "automatic_and_explicit" {
					events := collectUntil(t, watcher.Events, func(event runtime.ConversationTitleChanged) bool { return true })
					if len(events) != 1 || events[0].Title != "自动 标题" {
						t.Fatalf("title events = %+v", events)
					}
				}
				collectUntil(t, watcher.Events, isTurnCompletedInfo)
			}
			// 3. 读取新订阅的初始标题，成功时为自动标题，保存失败时仍为空。
			want := ""
			if scenario == "automatic_and_explicit" {
				want = "自动 标题"
			}
			if snapshot := h.subscribeConversation(t, conv.ID); snapshot.Title != want {
				t.Fatalf("snapshot title = %q, want %q", snapshot.Title, want)
			}
			logs.fail.Store(false)
			// 4. 关掉故障开关；同值更新和之前的失败都不推送，只收到之后成功手动改名的事件。
			if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
				ConversationID: conv.ID,
				Title:          want,
			}); err != nil {
				t.Fatal(err)
			}
			if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
				ConversationID: conv.ID,
				Title:          "手动改名",
			}); err != nil {
				t.Fatal(err)
			}
			events := collectUntil(t, watcher.Events, func(event runtime.ConversationTitleChanged) bool { return event.Title == "手动改名" })
			if len(events) != 1 {
				t.Fatalf("title events = %+v, want one successful rename", events)
			}
			record, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: conv.ID})
			if err != nil || record.Title != "手动改名" {
				t.Fatalf("saved title = %q, %v", record.Title, err)
			}
		})
	}
}

// titleFailingLogs 在测试要求时拒绝保存非空标题，其他 metadata 和输入、模型输出照常保存。
type titleFailingLogs struct {
	storage.LogStorage
	fail atomic.Bool
}

// Append 开启故障时拒绝写入带非空标题的 metadata，其余照常追加。
func (l *titleFailingLogs) Append(ctx context.Context, path storage.StreamPath, streamID storage.StreamID, payloads ...json.RawMessage) ([]storage.LogEntryInfo, error) {
	// 1. 开启故障且写入 metadata 时，检查记录中的标题，非空则返回注入错误。
	if l.fail.Load() && streamID == dao.MetadataStreamID {
		for _, payload := range payloads {
			var entry struct {
				Conversation *defs.Conversation `json:"conversation"`
			}
			if json.Unmarshal(payload, &entry) == nil && entry.Conversation != nil && entry.Conversation.Title != "" {
				return nil, errors.New("injected title save failure")
			}
		}
	}
	// 2. 其余数据交给原日志存储追加。
	return l.LogStorage.Append(ctx, path, streamID, payloads...)
}

// testContextWindow 是运行信息测试中不会触发压缩的 Agent 默认模型上下文窗口。
const testContextWindow = 100_000

// TestRuntimeSessionRuntimeInfo 检查 Session 运行状态、模型、用量和上下文基准。
func TestRuntimeSessionRuntimeInfo(t *testing.T) {
	// 1. 列出 Session 运行状态、模型、用量和上下文基准的测试场景。
	tests := []struct {
		name   string
		window int
		run    func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent)
	}{
		{
			name:   "reported_usage",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				content := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"你好！"},
					usage: &runtimeModelUsage{
						input:  1000,
						output: 20,
					},
				}
				collectUntil(t, events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				final := getLastInfo(collectUntil(t, events, isTurnCompletedInfo))
				// 运行信息直接使用 AgentContext 最后一次测量，回复不再由 Runtime 重复累加。
				measurement := getLastContextMeasurement(t, collectUntil(t, content.Events, isTurnFinished))
				wantTokens := measurement.RequestTokens
				if final.Usage != (defs.TokenUsage{
					Input:  1000,
					Output: 20,
				}) || final.Context.Source != runtime.ContextUsageSourceActual ||
					final.Context.Tokens != wantTokens || final.Context.Window != testContextWindow || final.ActiveTurnID != "" {
					t.Fatalf("final info = %+v, want Tokens %d", final, wantTokens)
				}
			},
		},
		{
			name:   "usage_not_reported",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"你好！"}}
				final := getLastInfo(collectUntil(t, events, isTurnCompletedInfo))
				if final.Usage != (defs.TokenUsage{}) || final.Context.Source != runtime.ContextUsageSourceEstimated || final.Context.Tokens <= 0 {
					t.Fatalf("final info = %+v", final)
				}
			},
		},
		{
			// 窗口为 0 时不压缩：每轮模型只收到一次请求，来源和数值照常计算。
			name:   "zero_window",
			window: 0,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				content := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "第一轮", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"一"},
					usage: &runtimeModelUsage{
						input:  100,
						output: 10,
					},
				}
				first := getLastInfo(collectUntil(t, events, isTurnCompletedInfo))
				h.submit(t, conv.ID, "第二轮", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"二"},
					usage: &runtimeModelUsage{
						input:  200,
						output: 20,
					},
				}
				final := getLastInfo(collectUntil(t, events, isNextTurnCompletedInfo(first)))
				h.model.requireNoRequest(t)
				// 第二轮测量只使用这一轮的实测校正，零窗口不改变测量来源。
				collectUntil(t, content.Events, isTurnFinished)
				measurement := getLastContextMeasurement(t, collectUntil(t, content.Events, isTurnFinished))
				wantTokens := measurement.RequestTokens
				if final.Context.Window != 0 || final.Context.Source != runtime.ContextUsageSourceActual || final.Context.Tokens != wantTokens ||
					final.Usage != (defs.TokenUsage{
						Input:  300,
						Output: 30,
					}) {
					t.Fatalf("final info = %+v, want Tokens %d", final, wantTokens)
				}
			},
		},
		{
			// 第二轮触发压缩：压缩让实际基准作废，来源退回估算，直到普通调用上报用量后恢复为实际。
			name:   "compaction_invalidates_baseline",
			window: 10,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				h.submit(t, conv.ID, "第一轮", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"一"},
					usage: &runtimeModelUsage{
						input:  100,
						output: 10,
					},
				}
				first := getLastInfo(collectUntil(t, events, isTurnCompletedInfo))
				h.submit(t, conv.ID, "第二轮", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"摘要"},
					usage: &runtimeModelUsage{
						input:  200,
						output: 20,
					},
				}
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"二"},
					usage: &runtimeModelUsage{
						input:  300,
						output: 30,
					},
				}
				infos := collectUntil(t, events, isNextTurnCompletedInfo(first))
				estimated := slices.ContainsFunc(infos, func(info runtime.SessionRuntimeInfo) bool {
					return info.Context.Source == runtime.ContextUsageSourceEstimated
				})
				final := getLastInfo(infos)
				if !estimated || final.Context.Source != runtime.ContextUsageSourceActual || final.Usage != (defs.TokenUsage{
					Input:  600,
					Output: 60,
				}) {
					t.Fatalf("second Turn infos = %+v", infos)
				}
			},
		},
		{
			name:   "child_session_enters_projection",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				h.submit(t, conv.ID, "委派", "")
				h.model.receive(t).reply <- runtimeModelResponse{calls: []llmkit.ToolCall{{
					ID:        "spawn",
					Name:      "spawn_agent",
					Arguments: `{"task_name":"review","message":"check"}`,
				}}}
				collectUntil(t, events, func(info runtime.SessionRuntimeInfo) bool {
					return info.Session.ParentSessionID == conv.RootSessionID && info.Session.TaskName == "review"
				})
			},
		},
		{
			// 重启后用量从 Turn 记录恢复；第一次测量之前上下文占用未知，窗口来自 Agent 定义。
			name:   "initial_info_after_restart",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"你好！"},
					usage: &runtimeModelUsage{
						input:  1000,
						output: 20,
					},
				}
				collectUntil(t, events, isTurnCompletedInfo)
				h.stop(t)

				restarted := startTestRuntimeWithWindow(t, h.store, h.model, testContextWindow)
				if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				subscription := restarted.subscribeConversation(t, conv.ID)
				if len(subscription.Sessions) != 1 {
					t.Fatalf("Sessions = %+v", subscription.Sessions)
				}
				root := subscription.Sessions[0]
				if root.Session.ID != conv.RootSessionID || root.Usage != (defs.TokenUsage{
					Input:  1000,
					Output: 20,
				}) ||
					root.Context != (runtime.ContextUsage{
						Window: testContextWindow,
						Source: runtime.ContextUsageSourceUnknown,
					}) ||
					root.LastTurnStatus != defs.TurnStatusCompleted {
					t.Fatalf("root info after restart = %+v", root)
				}
			},
		},
		{
			// 增量等不改变运行信息的事件不推送：连续两条推送一定不同。
			name:   "pushed_only_on_change",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				h.submit(t, conv.ID, "长回答", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: strings.Split(strings.Repeat("字", 50), ""),
					usage: &runtimeModelUsage{
						input:  100,
						output: 50,
					},
				}
				infos := collectUntil(t, events, isTurnCompletedInfo)
				for index := 1; index < len(infos); index++ {
					if infos[index] == infos[index-1] {
						t.Fatalf("info #%d repeats the previous push: %+v", index, infos[index])
					}
				}
			},
		},
		{
			// 运行中：窗口和本轮模型跟着活动 Turn；结束后清空本轮模型。
			name:   "active_turn_model",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				// 根 Session 在对话加载后异步启动，用的是启动时读到的记录；InterruptTurn 等它启动完成才返回
				// （没有活动 Turn 时什么也不做），之后再切换模型。
				if _, err := h.rt.InterruptTurn(t.Context(), &runtime.InterruptTurnRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				if _, err := h.rt.SetSessionModel(t.Context(), &runtime.SetSessionModelRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
					Model: defs.ModelSelection{
						Ref: defs.ModelRef{
							ProviderID: "test",
							ModelID:    "b",
						},
						ReasoningEffort: "high",
					},
				}); err != nil {
					t.Fatal(err)
				}
				h.submit(t, conv.ID, "你好", "")
				running := collectUntil(t, events, func(info runtime.SessionRuntimeInfo) bool { return info.State == session.SessionStateRunning })
				active := getLastInfo(running)
				wantB := defs.ModelSelection{
					Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "b",
					},
					ReasoningEffort: "high",
				}
				if active.ActiveTurnModel != wantB || active.Context.Window != 64000 {
					t.Fatalf("running info = %+v", active)
				}
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"好"},
					usage: &runtimeModelUsage{
						input:  500,
						output: 5,
					},
				}
				final := getLastInfo(collectUntil(t, events, isTurnCompletedInfo))
				if final.ActiveTurnModel != (defs.ModelSelection{}) || final.Session.Model != wantB || final.Context.Window != 64000 ||
					final.Context.Source != runtime.ContextUsageSourceActual {
					t.Fatalf("final info = %+v", final)
				}
			},
		},
		{
			// 空闲时切到别的模型：窗口跟所选模型，基准不再适用改为估算；切回原模型恢复实际值。
			name:   "switch_model_toggles_baseline",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				content := h.subscribeSession(t, conv.ID, conv.RootSessionID)
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"好"},
					usage: &runtimeModelUsage{
						input:  500,
						output: 5,
					},
				}
				getLastInfo(collectUntil(t, events, isTurnCompletedInfo))
				measurement := getLastContextMeasurement(t, collectUntil(t, content.Events, isTurnFinished))
				setModel := func(ref defs.ModelRef) runtime.SessionRuntimeInfo {
					if _, err := h.rt.SetSessionModel(t.Context(), &runtime.SetSessionModelRequest{
						ConversationID: conv.ID,
						SessionID:      conv.RootSessionID,
						Model:          defs.ModelSelection{Ref: ref},
					}); err != nil {
						t.Fatal(err)
					}
					return getLastInfo(collectUntil(t, events, func(info runtime.SessionRuntimeInfo) bool { return info.Session.Model.Ref == ref }))
				}
				switched := setModel(defs.ModelRef{
					ProviderID: "test",
					ModelID:    "b",
				})
				if switched.Context.Window != 64000 || switched.Context.Source != runtime.ContextUsageSourceEstimated ||
					switched.Context.Tokens != measurement.EstimatedTokens {
					t.Fatalf("after switch = %+v", switched.Context)
				}
				switchedUsage, err := h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil || switchedUsage.Window != 64000 || switchedUsage.ThresholdTokens != agent_context.GetThresholdTokens(64000, 0) {
					t.Fatalf("usage after switch = %+v, %v", switchedUsage, err)
				}
				back := setModel(defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				})
				if back.Context.Window != testContextWindow || back.Context.Source != runtime.ContextUsageSourceActual ||
					back.Context.Tokens != measurement.RequestTokens {
					t.Fatalf("after switching back = %+v", back.Context)
				}
				usage, err := h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
					ConversationID: conv.ID,
					SessionID:      conv.RootSessionID,
				})
				if err != nil || usage.LastRequest == nil || usage.LastRequest.Model != (defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				}) ||
					usage.ThresholdTokens != agent_context.GetThresholdTokens(testContextWindow, 0) {
					t.Fatalf("usage = %+v, %v", usage, err)
				}
			},
		},
		{
			// 旧记录（Model 为零值）：窗口按 Agent 默认模型取，不是 0。
			name:   "legacy_record_window_uses_agent_default",
			window: testContextWindow,
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation, events <-chan runtime.ConversationEvent) {
				// 先提交首条输入保存对话，再停止 Runtime，改写记录后重启。
				h.completeRound(t, conv, "保存", 2)
				h.stop(t)
				record, found, err := h.store.query.GetSession(t.Context(), conv.ID, conv.RootSessionID)
				if err != nil || !found {
					t.Fatalf("GetSession() = %v, %v", found, err)
				}
				record.Model = defs.ModelSelection{}
				if err := h.store.mutation.SetSession(t.Context(), record); err != nil {
					t.Fatal(err)
				}
				restarted := startTestRuntimeWithWindow(t, h.store, h.model, testContextWindow)
				if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				subscription := restarted.subscribeConversation(t, conv.ID)
				root := subscription.Sessions[slices.IndexFunc(subscription.Sessions, func(info runtime.SessionRuntimeInfo) bool { return info.Session.ID == conv.RootSessionID })]
				if root.Context.Window != testContextWindow {
					t.Fatalf("legacy root window = %d, want %d", root.Context.Window, testContextWindow)
				}
			},
		},
	}
	// 2. 按场景完成主子任务和模型调用，检查 Session 状态、当前模型、用量与下次请求占用。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntimeWithWindow(t, openRuntimeStore(t), newRuntimeModel(t), tt.window)
			conv := h.create(t)
			tt.run(t, h, conv, h.subscribeConversation(t, conv.ID).Events)
		})
	}
}

// TestRuntimeSessionContextUsage 检查 Session 上下文占用详情和未知对象错误。
func TestRuntimeSessionContextUsage(t *testing.T) {
	// 1. 准备读取占用的辅助函数，列出占用详情和未知对象错误的测试场景。
	getUsage := func(h *runtimeHarness, conversationID defs.ConversationID, sessionID defs.SessionID) (runtime.SessionContextUsage, error) {
		return h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
			ConversationID: conversationID,
			SessionID:      sessionID,
		})
	}
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness, conv defs.Conversation)
	}{
		{
			name: "loaded_without_turn",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation) {
				usage, err := getUsage(h, conv.ID, conv.RootSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if usage.NextRequest != (runtime.ContextUsage{
					Window: testContextWindow,
					Source: runtime.ContextUsageSourceUnknown,
				}) ||
					usage.Window != testContextWindow || len(usage.Items) != 0 || usage.LastRequest != nil {
					t.Fatalf("usage = %+v", usage)
				}
			},
		},
		{
			name: "after_reported_turn",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation) {
				events := h.subscribeConversation(t, conv.ID).Events
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"你好！"},
					usage: &runtimeModelUsage{
						input:  1000,
						output: 20,
					},
				}
				collectUntil(t, events, isTurnCompletedInfo)

				usage, err := getUsage(h, conv.ID, conv.RootSessionID)
				if err != nil {
					t.Fatal(err)
				}
				// 新订阅与占用查询读取同一份 Session 运行信息。
				info := h.subscribeConversation(t, conv.ID).Sessions[0]
				if usage.LastRequest == nil || usage.LastRequest.Usage.Input != 1000 || usage.LastRequest.EstimatedTokens <= 0 {
					t.Fatalf("LastRequest = %+v", usage.LastRequest)
				}
				if usage.Window != testContextWindow || usage.ThresholdTokens != testContextWindow*8/10 || usage.SystemTokens <= 0 || usage.ToolTokens <= 0 {
					t.Fatalf("usage = %+v", usage)
				}
				if len(usage.Items) < 2 || usage.Items[0].Category != agent_context.ContextCategoryUserInput ||
					!slices.ContainsFunc(usage.Items, func(item agent_context.ItemTokens) bool {
						return item.Category == agent_context.ContextCategoryAssistantText
					}) {
					t.Fatalf("Items = %+v", usage.Items)
				}
				if usage.NextRequest != info.Context || usage.NextRequest.Source != runtime.ContextUsageSourceActual {
					t.Fatalf("NextRequest = %+v, runtime info context = %+v", usage.NextRequest, info.Context)
				}
			},
		},
		{
			// 重启后使用已保存记录初始化运行信息，首次测量前没有明细，占用未知。
			name: "after_restart_before_measurement",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation) {
				events := h.subscribeConversation(t, conv.ID).Events
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"你好！"},
					usage: &runtimeModelUsage{
						input:  1000,
						output: 20,
					},
				}
				collectUntil(t, events, isTurnCompletedInfo)
				h.stop(t)

				restarted := startTestRuntimeWithWindow(t, h.store, h.model, testContextWindow)
				if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				usage, err := getUsage(restarted, conv.ID, conv.RootSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if usage.NextRequest.Source != runtime.ContextUsageSourceUnknown || usage.Window != testContextWindow ||
					len(usage.Items) != 0 || usage.LastRequest != nil || usage.SystemTokens != 0 {
					t.Fatalf("usage after restart = %+v", usage)
				}
			},
		},
		{
			name: "after_restart_first_measurement_before_response",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation) {
				// 1. 保存有实际用量的普通回复，再启动新的 Runtime。
				events := h.subscribeConversation(t, conv.ID).Events
				h.submit(t, conv.ID, "第一轮", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					chunks: []string{"第一轮完成"},
					usage:  &runtimeModelUsage{input: 1000, output: 20},
				}
				collectUntil(t, events, isTurnCompletedInfo)
				h.stop(t)
				restarted := startTestRuntimeWithWindow(t, h.store, h.model, testContextWindow)
				if _, err := restarted.rt.OpenConversation(t.Context(), &runtime.OpenConversationRequest{ConversationID: conv.ID}); err != nil {
					t.Fatal(err)
				}
				content := restarted.subscribeSession(t, conv.ID, conv.RootSessionID)
				status := restarted.subscribeConversation(t, conv.ID)

				// 2. 新请求停在模型入口，首次测量和上下文查询使用恢复报告。
				restarted.submit(t, conv.ID, "继续", "")
				request := h.model.receive(t)
				measurement := getLastContextMeasurement(t, collectUntil(t, content.Events, isContextMeasured))
				usage, err := getUsage(restarted, conv.ID, conv.RootSessionID)
				if err != nil || !measurement.IsUsageBased || measurement.RequestTokens == measurement.EstimatedTokens ||
					usage.NextRequest.Source != runtime.ContextUsageSourceActual || usage.NextRequest.Tokens != measurement.RequestTokens || usage.LastRequest != nil {
					t.Fatalf("restored usage = %+v, measurement = %+v, error = %v", usage, measurement, err)
				}
				infos := collectUntil(t, status.Events, func(info runtime.SessionRuntimeInfo) bool {
					return info.Context.Source == runtime.ContextUsageSourceActual
				})
				if info := getLastInfo(infos); info.Context != usage.NextRequest || info.Usage != (defs.TokenUsage{Input: 1000, Output: 20}) {
					t.Fatalf("restored info = %+v, usage = %+v", info, usage)
				}

				// 3. 新回复没有 Usage 时只更新本地测量，不重复累计恢复报告。
				request.reply <- runtimeModelResponse{chunks: []string{"继续完成"}}
				collectUntil(t, content.Events, isTurnFinished)
				final := getLastInfo(collectUntil(t, status.Events, isTurnCompletedInfo))
				if final.Usage != (defs.TokenUsage{Input: 1000, Output: 20}) || final.Context.Source != runtime.ContextUsageSourceEstimated {
					t.Fatalf("final info = %+v", final)
				}
			},
		},
		{
			name: "unknown_session",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation) {
				if _, err := getUsage(h, conv.ID, defs.NewSessionID()); err == nil {
					t.Fatal("GetSessionContextUsage() of an unknown Session error = nil")
				}
			},
		},
		{
			name: "session_of_another_conversation",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation) {
				other := h.create(t)
				if _, err := getUsage(h, conv.ID, other.RootSessionID); err == nil {
					t.Fatal("GetSessionContextUsage() of a foreign Session error = nil")
				}
			},
		},
		{
			name: "unknown_conversation",
			run: func(t *testing.T, h *runtimeHarness, conv defs.Conversation) {
				if _, err := getUsage(h, defs.NewConversationID(), conv.RootSessionID); err == nil {
					t.Fatal("GetSessionContextUsage() of an unknown Conversation error = nil")
				}
			},
		},
	}
	// 2. 按场景测量、追加内容或重启，检查占用来源、Item 分类、模型基准及无效 Session 错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntimeWithWindow(t, openRuntimeStore(t), newRuntimeModel(t), testContextWindow)
			tt.run(t, h, h.create(t))
			// 检查占用查询后没有新增模型请求。
			h.model.requireNoRequest(t)
		})
	}
}

// TestRuntimeContextOutputBudget 检查上下文详情与业务测量使用同一输出预算，切换模型后使用新预算。
func TestRuntimeContextOutputBudget(t *testing.T) {
	// 1. 按窗口和输出上限创建独立模型配置。
	for _, tt := range []struct {
		name      string
		window    int
		maxOutput int
		threshold int
	}{
		{name: "reserve_output", window: 600_000, maxOutput: 128_000, threshold: 472_000},
		{name: "retain_eighty_percent", window: 600_000, maxOutput: 32_000, threshold: 480_000},
		{name: "unknown_output", window: 600_000, threshold: 480_000},
		{name: "unknown_window", maxOutput: 128_000},
		{name: "output_exceeds_window", window: 600_000, maxOutput: 700_000, threshold: 480_000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openRuntimeStore(t), newRuntimeModel(t)
			opt := newTestRuntimeOpt(store, model)
			opt.Definitions.Models[0].ContextWindow = tt.window
			opt.Definitions.Models[0].MaxOutputTokens = tt.maxOutput
			opt.Definitions.Models[1].MaxOutputTokens = 32_000
			h := startTestRuntimeWithOpt(t, store, model, opt)
			conv := h.create(t)
			content := h.subscribeSession(t, conv.ID, conv.RootSessionID)
			h.submit(t, conv.ID, "检查输出预算", "")
			model.receive(t).reply <- runtimeModelResponse{chunks: []string{"完成"}, usage: &runtimeModelUsage{input: 1000, output: 20}}
			measurement := getLastContextMeasurement(t, collectUntil(t, content.Events, isTurnFinished))

			// 2. 上下文详情与 Agent 测量的压缩阈值相同。
			usage, err := h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
				ConversationID: conv.ID, SessionID: conv.RootSessionID,
			})
			if err != nil || usage.ThresholdTokens != tt.threshold || measurement.ThresholdTokens != tt.threshold {
				t.Fatalf("usage = %+v, measurement = %+v, error = %v; want threshold %d", usage, measurement, err, tt.threshold)
			}

			// 3. 空闲时切换到64k窗口、32k最大输出的模型，详情立即改用32k阈值。
			if _, err := h.rt.SetSessionModel(t.Context(), &runtime.SetSessionModelRequest{
				ConversationID: conv.ID, SessionID: conv.RootSessionID,
				Model: defs.ModelSelection{Ref: defs.ModelRef{ProviderID: "test", ModelID: "b"}},
			}); err != nil {
				t.Fatal(err)
			}
			usage, err = h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
				ConversationID: conv.ID, SessionID: conv.RootSessionID,
			})
			if err != nil || usage.Window != 64_000 || usage.ThresholdTokens != 32_000 || usage.NextRequest.Source != runtime.ContextUsageSourceEstimated {
				t.Fatalf("usage after model switch = %+v, error = %v", usage, err)
			}
		})
	}
}

// TestRuntimeContextMeasurementLifecycle 检查同轮校正、跨轮恢复和统一测量展示。
func TestRuntimeContextMeasurementLifecycle(t *testing.T) {
	// 1. 同轮工具往返和新 Turn 第一次请求前都沿用适用的实际用量。
	for _, tt := range []struct {
		name       string
		newTurn    bool
		reportLast bool
	}{
		{name: "same_turn_reported", reportLast: true},
		{name: "same_turn_unreported"},
		{name: "new_turn_reported", newTurn: true, reportLast: true},
		{name: "new_turn_unreported", newTurn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestRuntimeWithWindow(t, openRuntimeStore(t), newRuntimeModel(t), testContextWindow)
			conv := h.create(t)
			content := h.subscribeSession(t, conv.ID, conv.RootSessionID)
			status := h.subscribeConversation(t, conv.ID)
			h.submit(t, conv.ID, "检查上下文", "")
			firstRequest := h.model.receive(t)
			firstMeasurement := getLastContextMeasurement(t, collectUntil(t, content.Events, isContextMeasured))
			if firstMeasurement.IsUsageBased || firstMeasurement.RequestTokens != firstMeasurement.EstimatedTokens {
				t.Fatalf("first measurement = %+v", firstMeasurement)
			}
			response := runtimeModelResponse{usage: &runtimeModelUsage{input: 1000, output: 10}}
			if tt.newTurn {
				response.chunks = []string{"第一轮完成"}
			} else {
				response.calls = []llmkit.ToolCall{{ID: "list", Name: "list_agents", Arguments: `{}`}}
			}
			firstRequest.reply <- response
			if tt.newTurn {
				collectUntil(t, content.Events, isTurnFinished)
				collectUntil(t, status.Events, isTurnCompletedInfo)
				h.submit(t, conv.ID, "继续检查", "")
			}

			// 2. 第二次请求阻塞在模型中，查询与订阅均读取请求前的统一测量。
			secondRequest := h.model.receive(t)
			beforeEvents := collectUntil(t, content.Events, func(event conversation.ConversationEvent) bool {
				if !isContextMeasured(event) {
					return false
				}
				measurement := getSessionEvent(event).(*session.SessionAgentEvent).Event.Measurement
				return tt.newTurn || slices.ContainsFunc(measurement.Items, func(item agent_context.ItemTokens) bool {
					return item.Category == agent_context.ContextCategoryToolResult
				})
			})
			before := getLastContextMeasurement(t, beforeEvents)
			if !before.IsUsageBased {
				t.Fatalf("second request usage based = %v, new Turn = %v", before.IsUsageBased, tt.newTurn)
			}
			usage, err := h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
				ConversationID: conv.ID, SessionID: conv.RootSessionID,
			})
			if err != nil || usage.LastRequest == nil || usage.LastRequest.EstimatedTokens != firstMeasurement.EstimatedTokens ||
				usage.NextRequest.Tokens != before.RequestTokens || !slices.Equal(usage.Items, before.Items) {
				t.Fatalf("before second request: usage = %+v, measurement = %+v, error = %v", usage, before, err)
			}
			if tt.newTurn {
				infos := collectUntil(t, status.Events, func(info runtime.SessionRuntimeInfo) bool {
					return info.State == session.SessionStateRunning && info.Context.Source == runtime.ContextUsageSourceActual
				})
				if !slices.ContainsFunc(infos, func(info runtime.SessionRuntimeInfo) bool {
					return info.State == session.SessionStateRunning && info.Context.Source == runtime.ContextUsageSourceUnknown && info.Context.Tokens == 0
				}) {
					t.Fatalf("new Turn did not clear the previous measurement before restoring usage: %+v", infos)
				}
			}

			// 3. 最终回复只出现在统一测量中一次，实际用量和本地分类分别保留。
			lastResponse := runtimeModelResponse{chunks: []string{"检查完成"}}
			if tt.reportLast {
				lastResponse.usage = &runtimeModelUsage{input: 2000, output: 20}
			}
			secondRequest.reply <- lastResponse
			finalMeasurement := getLastContextMeasurement(t, collectUntil(t, content.Events, isTurnFinished))
			final := getLastInfo(collectUntil(t, status.Events, isTurnCompletedInfo))
			usage, err = h.rt.GetSessionContextUsage(t.Context(), &runtime.GetSessionContextUsageRequest{
				ConversationID: conv.ID, SessionID: conv.RootSessionID,
			})
			wantSource := runtime.ContextUsageSourceActual
			if !tt.reportLast {
				wantSource = runtime.ContextUsageSourceEstimated
			}
			if err != nil || final.Context.Tokens != finalMeasurement.RequestTokens || final.Context.Source != wantSource ||
				usage.NextRequest != final.Context || !slices.Equal(usage.Items, finalMeasurement.Items) ||
				usage.SystemTokens != finalMeasurement.SystemTokens || usage.ToolTokens != finalMeasurement.ToolTokens {
				t.Fatalf("final = %+v, usage = %+v, measurement = %+v, error = %v", final, usage, finalMeasurement, err)
			}
			wantUsage := defs.TokenUsage{Input: 1000, Output: 10}
			wantEstimate := firstMeasurement.EstimatedTokens
			if tt.reportLast {
				wantUsage = defs.TokenUsage{Input: 3000, Output: 30}
				wantEstimate = before.EstimatedTokens
			}
			if final.Usage != wantUsage || usage.LastRequest.EstimatedTokens != wantEstimate {
				t.Fatalf("cumulative usage = %+v, last request = %+v", final.Usage, usage.LastRequest)
			}
		})
	}
}

// ── 订阅测试夹具 ──

// isContextMeasured 事件是否带统一上下文测量？
func isContextMeasured(event conversation.ConversationEvent) bool {
	// 1. 提取 Agent 事件并检查测量类型。
	agentEvent, ok := getSessionEvent(event).(*session.SessionAgentEvent)
	return ok && agentEvent.Event.Type == agent.AgentEventContextMeasured
}

// getLastContextMeasurement 返回订阅事件中最后一次统一测量。
func getLastContextMeasurement(t *testing.T, events []conversation.ConversationEvent) *agent_context.ContextMeasurement {
	t.Helper()
	// 1. 从后向前找到测量事件，未找到时结束测试。
	for index := len(events) - 1; index >= 0; index-- {
		if isContextMeasured(events[index]) {
			return getSessionEvent(events[index]).(*session.SessionAgentEvent).Event.Measurement
		}
	}
	t.Fatal("context measurement not received")
	return nil
}

// startTestRuntimeWithWindow 启动指定模型上下文窗口的 Runtime。
func startTestRuntimeWithWindow(t *testing.T, store *runtimeStore, model *runtimeModel, window int) *runtimeHarness {
	t.Helper()
	// 1. 创建测试配置，修改默认模型窗口后启动。
	opt := newTestRuntimeOpt(store, model)
	opt.Definitions.Models[0].ContextWindow = window
	return startTestRuntimeWithOpt(t, store, model, opt)
}

// subscribeConversation 登记对话标题和运行信息订阅。
func (h *runtimeHarness) subscribeConversation(t *testing.T, conversationID defs.ConversationID) *runtime.ConversationSubscription {
	t.Helper()
	// 1. 请求订阅，失败则结束测试，成功返回订阅对象。
	subscription, err := h.rt.SubscribeConversation(t.Context(), &runtime.SubscribeConversationRequest{ConversationID: conversationID})
	if err != nil {
		t.Fatal(err)
	}
	return subscription
}

// subscribeSession 登记对话内一个 Session 的内容订阅。
func (h *runtimeHarness) subscribeSession(t *testing.T, conversationID defs.ConversationID, sessionID defs.SessionID) *runtime.SessionSubscription {
	t.Helper()
	// 1. 提交对话和 Session，失败则结束测试，成功返回订阅对象。
	subscription, err := h.rt.SubscribeSession(t.Context(), &runtime.SubscribeSessionRequest{
		ConversationID: conversationID,
		SessionID:      sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return subscription
}

// getChildSessionID 返回测试对话中唯一的子 Session ID。
func (h *runtimeHarness) getChildSessionID(t *testing.T, conv defs.Conversation) defs.SessionID {
	t.Helper()
	// 1. 读取对话内的 Session，返回非根 Session 的 ID，找不到则结束测试。
	page, err := h.rt.ListSessions(t.Context(), &dao.ListSessionsRequest{
		ConversationID: conv.ID,
		Order:          storage.Order{Direction: storage.DirectionAsc},
		Limit:          10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range page.Items {
		if record.ID != conv.RootSessionID {
			return record.ID
		}
	}
	t.Fatalf("child Session not found in %+v", page.Items)
	return ""
}

// receiveRootAndTask 接收委派后的根请求和子任务首个请求。
func (m *runtimeModel) receiveRootAndTask(t *testing.T, task string) (root, child runtimeModelRequest) {
	t.Helper()
	// 1. 接收两条模型请求，按 TaskName 文本区分根任务和子任务。
	for range 2 {
		request := m.receive(t)
		if strings.Contains(fmt.Sprint(request.body), "TaskName: "+task) {
			child = request
		} else {
			root = request
		}
	}
	// 2. 检查两类请求齐全后返回。
	if root.reply == nil || child.reply == nil {
		t.Fatal("missing root or child request")
	}
	return root, child
}

// collectUntil 收集指定类型事件，直到匹配条件成立。
func collectUntil[T any, E any](t *testing.T, events <-chan E, match func(T) bool) []T {
	t.Helper()
	// 1. 读取事件，跳过其他类型，收集到匹配值后返回；通道提前关闭或 5 秒超时则结束测试。
	var collected []T
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-events:
			if !open {
				t.Fatalf("events closed after %d events", len(collected))
			}
			value, ok := any(event).(T)
			if !ok {
				continue
			}
			collected = append(collected, value)
			if match(value) {
				return collected
			}
		case <-timeout:
			t.Fatalf("event did not arrive after %d events", len(collected))
		}
	}
}

// waitEventsClosed 读完事件直到通道关闭。
func waitEventsClosed[T any](t *testing.T, events <-chan T) {
	t.Helper()
	// 1. 最多读取 5 秒，通道关闭后返回，超时则结束测试。
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-events:
			if !open {
				return
			}
		case <-timeout:
			t.Fatal("events were not closed")
		}
	}
}

// getSessionEvent 从对话内容事件提取 Session 事件。
func getSessionEvent(event conversation.ConversationEvent) session.SessionEvent {
	// 1. 是 Session 事件则返回内部事件，其余返回 nil。
	if sessionEvent, ok := event.(*conversation.ConversationSessionEvent); ok {
		return sessionEvent.Event
	}
	return nil
}

// isTurnFinished 事件是否为 Session 的 Turn 结束事件？
func isTurnFinished(event conversation.ConversationEvent) bool {
	// 1. 提取 Session 事件并检查类型。
	_, ok := getSessionEvent(event).(*session.SessionTurnFinished)
	return ok
}

// isMessageDelta 事件是否带消息文本增量？
func isMessageDelta(event conversation.ConversationEvent) bool {
	// 1. 检查 Agent 增量事件且 Text 非空，推理增量不计。
	agentEvent, ok := getSessionEvent(event).(*session.SessionAgentEvent)
	return ok && agentEvent.Event.Type == agent.AgentEventItemDelta && agentEvent.Event.Delta.Text != ""
}

// messageText 返回测试消息的首段文本。
func messageText(item *defs.Item) string {
	// 1. 读取第一段 Text 内容。
	return item.Message.Content[0].Text.Text
}

// isTurnCompletedInfo Session 是否空闲且最近 Turn 已完成？
func isTurnCompletedInfo(info runtime.SessionRuntimeInfo) bool {
	// 1. 比较运行状态和最近 Turn 状态。
	return info.State == session.SessionStateIdle && info.LastTurnStatus == defs.TurnStatusCompleted
}

// isNextTurnCompletedInfo 创建检查下一轮 Turn 完成的条件。
func isNextTurnCompletedInfo(previous runtime.SessionRuntimeInfo) func(runtime.SessionRuntimeInfo) bool {
	// 1. 返回同时检查已完成状态和 TurnID 已变化的匹配函数。
	return func(info runtime.SessionRuntimeInfo) bool {
		return isTurnCompletedInfo(info) && info.LastTurnID != previous.LastTurnID
	}
}

// getLastInfo 返回最后一条 Session 运行信息。
func getLastInfo(infos []runtime.SessionRuntimeInfo) runtime.SessionRuntimeInfo {
	// 1. 取切片的最后一项。
	return infos[len(infos)-1]
}

// unsettledTurnLogs 让第一次追加「Turn 已完成」的 metadata 行失败，使根 Session 因终态未保存而退出。
type unsettledTurnLogs struct {
	storage.LogStorage
	failed atomic.Bool
}

// Append 让第一次写入 Turn 完成状态的 metadata 失败，其余照常追加。
func (l *unsettledTurnLogs) Append(ctx context.Context, path storage.StreamPath, streamID storage.StreamID, payloads ...json.RawMessage) ([]storage.LogEntryInfo, error) {
	// 1. 这批 metadata 的第一条是首次写入的 Turn 完成状态时返回注入错误。
	var line struct {
		Turn *defs.Turn `json:"turn"`
	}
	if streamID == dao.MetadataStreamID && json.Unmarshal(payloads[0], &line) == nil && line.Turn != nil &&
		line.Turn.Status == defs.TurnStatusCompleted && l.failed.CompareAndSwap(false, true) {
		return nil, errors.New("injected terminal status failure")
	}
	// 2. 其余写入交给原日志存储。
	return l.LogStorage.Append(ctx, path, streamID, payloads...)
}

// TestRuntimeConversationMessageCount 检查消息数累加、标题保留和活跃时间排序。
func TestRuntimeConversationMessageCount(t *testing.T) {
	// 1. 列出消息数累加、标题保留和活跃时间排序的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *runtimeHarness)
	}{
		{
			name: "user_message_and_reply_counted",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{
					reasoning: "先想一想",
					chunks:    []string{"你好！"},
				}
				requireConversation(t, h, conv.ID, func(record defs.Conversation) bool { return record.MessageCount == 2 })
			},
		},
		{
			name: "turns_accumulate",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				for index, text := range []string{"第一轮", "第二轮"} {
					h.submit(t, conv.ID, text, "")
					h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好的"}}
					want := 2 * (index + 1)
					requireConversation(t, h, conv.ID, func(record defs.Conversation) bool { return record.MessageCount == want })
				}
			},
		},
		{
			name: "title_set_after_submit_survives",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				h.submit(t, conv.ID, "写周报", "")
				if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: conv.ID,
					Title:          "写周报",
				}); err != nil {
					t.Fatal(err)
				}
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"好的"}}
				requireConversation(t, h, conv.ID, func(record defs.Conversation) bool {
					return record.MessageCount == 2 && record.Title == "写周报"
				})
			},
		},
		{
			// 标题和消息数都由对话的事件循环整条写库：改名之后的一轮结束，两者都在记录里。
			name: "title_and_message_count_kept_together",
			run: func(t *testing.T, h *runtimeHarness) {
				conv := h.create(t)
				if err := h.rt.SetConversationTitle(t.Context(), &runtime.SetConversationTitleRequest{
					ConversationID: conv.ID,
					Title:          "renamed",
				}); err != nil {
					t.Fatal(err)
				}
				h.submit(t, conv.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"你好！"}}
				requireConversation(t, h, conv.ID, func(record defs.Conversation) bool {
					return record.MessageCount == 2 && record.Title == "renamed"
				})
			},
		},
		{
			name: "recent_activity_listed_first",
			run: func(t *testing.T, h *runtimeHarness) {
				older, newer := h.create(t), h.create(t)
				// 第一条输入之后才进对话列表：两个对话依次先说一轮，newer 后说。
				for _, conv := range []defs.Conversation{older, newer} {
					h.completeRound(t, conv, "第一轮", 2)
					requireConversation(t, h, conv.ID, func(record defs.Conversation) bool { return record.MessageCount == 2 })
				}
				// 等到下一毫秒再写 older，使它的 updated_at 晚于 newer；同毫秒时按 key 倒序。
				newerRecord, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: newer.ID})
				if err != nil {
					t.Fatal(err)
				}
				for time.Now().UnixMilli() <= newerRecord.UpdatedAt {
					time.Sleep(time.Millisecond)
				}
				h.submit(t, older.ID, "你好", "")
				h.model.receive(t).reply <- runtimeModelResponse{chunks: []string{"你好！"}}
				requireConversation(t, h, older.ID, func(record defs.Conversation) bool { return record.MessageCount == 4 })
				page, err := h.rt.ListConversations(t.Context(), &dao.ListConversationsRequest{
					Order: storage.Order{
						By:        storage.OrderByUpdatedAt,
						Direction: storage.DirectionDesc,
					},
					Limit: 10,
				})
				if err != nil || len(page.Items) != 2 || page.Items[0].Conversation.ID != older.ID || page.Items[1].Conversation.ID != newer.ID {
					t.Fatalf("ListConversations() = %+v, %v, want the active conversation first", page.Items, err)
				}
			},
		},
	}
	// 2. 运行输入、回复或标题修改，检查根消息数累计、标题保留和活跃时间排序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t)))
		})
	}
}

// requireConversation 等待对话记录满足指定条件。
func requireConversation(t *testing.T, h *runtimeHarness, id defs.ConversationID, match func(defs.Conversation) bool) {
	t.Helper()
	// 1. 在 5 秒内轮询对话，条件成立返回，读取失败或超时则结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, err := h.rt.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: id})
		if err != nil {
			t.Fatal(err)
		}
		if match(record) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Conversation = %+v, condition not met", record)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRuntimePendingInputSubscription 检查同一次订阅登记返回轻量列表，随后通知不改写旧快照。
func TestRuntimePendingInputSubscription(t *testing.T) {
	// 1. 分别订阅普通排队和未读补充输入。
	for _, isSteer := range []bool{false, true} {
		t.Run(fmt.Sprint(isSteer), func(t *testing.T) {
			h := startTestRuntime(t, openRuntimeStore(t), newRuntimeModel(t))
			record := h.create(t)
			watcher := h.subscribeSession(t, record.ID, record.RootSessionID)
			if watcher.PendingInputs.Inputs == nil {
				t.Fatal("empty inputs is nil")
			}
			h.submit(t, record.ID, "first", "")
			h.model.receive(t)
			started := collectUntil(t, watcher.Events, func(event conversation.ConversationEvent) bool {
				_, ok := getSessionEvent(event).(*session.SessionTurnStarted)
				return ok
			})
			turnID := getSessionEvent(started[len(started)-1]).(*session.SessionTurnStarted).TurnID
			itemID := h.submit(t, record.ID, "queued", "")
			if isSteer {
				if err := h.rt.SteerInput(t.Context(), &runtime.SteerInputRequest{
					ConversationID: record.ID,
					ItemID:         itemID,
					TurnID:         turnID,
				}); err != nil {
					t.Fatal(err)
				}
			}
			collectUntil(t, watcher.Events, func(event conversation.ConversationEvent) bool {
				pending, ok := getSessionEvent(event).(*session.SessionPendingInputsUpdated)
				return ok && len(pending.PendingInputs.Inputs) == 1 && pending.PendingInputs.Inputs[0].IsSteer == isSteer
			})
			// 2. 第二个订阅读到相同列表，之后的取回以新列表通知。
			late := h.subscribeSession(t, record.ID, record.RootSessionID)
			if late.PendingInputs.SteerTurnID != turnID || len(late.PendingInputs.Inputs) != 1 || late.PendingInputs.Inputs[0].ItemID != itemID {
				t.Fatalf("snapshot = %+v", late.PendingInputs)
			}
			if _, err := h.rt.WithdrawInput(t.Context(), &runtime.WithdrawInputRequest{
				ConversationID: record.ID,
				ItemID:         itemID,
			}); err != nil {
				t.Fatal(err)
			}
			deleted := collectUntil(t, late.Events, func(event conversation.ConversationEvent) bool {
				pending, ok := getSessionEvent(event).(*session.SessionPendingInputsUpdated)
				return ok && len(pending.PendingInputs.Inputs) == 0
			})
			pending := getSessionEvent(deleted[len(deleted)-1]).(*session.SessionPendingInputsUpdated).PendingInputs
			if pending.Inputs == nil || pending.SteerTurnID != turnID || len(late.PendingInputs.Inputs) != 1 {
				t.Fatalf("new = %+v, original = %+v", pending, late.PendingInputs)
			}
		})
	}
}
