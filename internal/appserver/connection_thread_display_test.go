package appserver

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// newDisplayTestThread 创建已同步且已响应打开请求的根 Thread。
func newDisplayTestThread(outgoing chan codexMessage) *connectionThread {
	// 1. 设置 Client、翻译器、显示记录和消息输出队列。
	return &connectionThread{
		threadID:             "thread-1",
		sessionID:            testRootSessionID,
		isOpenDelivered:      true,
		isSynced:             true,
		outgoing:             outgoing,
		client:               &fakeThreadClient{},
		translator:           newTestTranslator(testRootSessionID),
		conversationSessions: map[defs.SessionID]runtime.SessionRuntimeInfo{},
		streamingItems:       map[int]*threadItemDisplay{},
		displayedItems:       map[defs.ItemID]*threadItemDisplay{},
		toolCallsByCallID:    map[string]*defs.Item{},
		clientUserMessageIDs: map[defs.ItemID]string{},
	}
}

// summarizeDisplayMessages 收集通知摘要、回复增量和最终文本。
func summarizeDisplayMessages(t *testing.T, outgoing chan codexMessage) (summaries []string, deltaText, completedText string) {
	t.Helper()
	// 1. 逐条解码队列通知，收集方法、Item 类型和 phase，拼接回复文本并记录最后完成内容。
	for len(outgoing) > 0 {
		message := <-outgoing
		var params struct {
			Item  map[string]any `json:"item"`
			Delta string         `json:"delta"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			t.Fatal(err)
		}
		summary := message.Method
		if message.Method == "item/agentMessage/delta" {
			deltaText += params.Delta
		}
		if params.Item != nil {
			summary = fmt.Sprintf("%s %v", summary, params.Item["type"])
			if phase, found := params.Item["phase"]; found {
				summary = fmt.Sprintf("%s %v", summary, phase)
			}
			if message.Method == "item/completed" && params.Item["type"] == codexItemAgentMessage {
				completedText = params.Item["text"].(string)
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries, deltaText, completedText
}

// TestConnectionThreadReplyPhase 检查回复完成时的 phase 和重新同步后补齐的回复文本。
func TestConnectionThreadReplyPhase(t *testing.T) {
	// 1. 准备回复、工具、正常结束、中断和重新同步事件。
	const turnID defs.TurnID = "turn-1"
	newReply := func(text string, modelCallID defs.ModelCallID) *defs.Item {
		reply := newTestMessage("reply-"+string(modelCallID), testRootAgent, newTestText(text))
		reply.TurnID, reply.ModelCallID = turnID, modelCallID
		return reply
	}
	newCall := func(status defs.ToolCallStatus) *defs.Item {
		call := newTestCall("call-item", "call-1", "list_agents", "{}")
		call.ModelCallID, call.ToolCall.Status = "model-1", status
		return call
	}
	reply := newReply("let me check", "model-1")
	// startedReply 是 reply 开始时交来的 Item：还没有内容。
	startedReply := reply.Clone()
	startedReply.Message.Content = []*defs.Content{newTestText("")}
	// streamReply 依次生成回复开始、增量、完成和保存事件。
	streamReply := func(reply *defs.Item, itemIndex int, ordinal int) []runtimeclient.ClientEvent {
		started := reply.Clone()
		started.Message.Content = []*defs.Content{newTestText("")}
		return []runtimeclient.ClientEvent{
			&runtimeclient.ItemChanged{
				Type:      agent.AgentEventItemStarted,
				TurnID:    turnID,
				ItemIndex: itemIndex,
				Item:      started,
			},
			&runtimeclient.ItemChanged{
				Type:      agent.AgentEventItemDelta,
				TurnID:    turnID,
				ItemIndex: itemIndex,
				Delta:     &agent.AgentItemDelta{Text: getMessageText(reply.Message)},
			},
			&runtimeclient.ItemChanged{
				Type:      agent.AgentEventItemCompleted,
				TurnID:    turnID,
				ItemIndex: itemIndex,
				Item:      reply,
			},
			&runtimeclient.ItemsSaved{
				TurnID: turnID,
				Items: []runtimeprotocol.LoggedItem{{
					Ordinal: storage.Ordinal(ordinal),
					Item:    reply,
				}},
			},
		}
	}
	turnFinished := &runtimeclient.TurnFinished{
		TurnID: turnID,
		Status: defs.TurnStatusCompleted,
	}
	// interruptedReplyEvents 是回复生成到一半被取消的事件：Agent 以到现在为止的内容中断它，Turn 保存它，再在本轮最后写 turn_interrupted。
	interruptedReplyEvents := func(reply *defs.Item) []runtimeclient.ClientEvent {
		started := reply.Clone()
		started.Message.Content = []*defs.Content{newTestText("")}
		interrupted := &defs.Item{
			ID:     "interrupted-item",
			TurnID: turnID,
			Kind:   defs.ItemTurnInterrupted,
		}
		return []runtimeclient.ClientEvent{
			&runtimeclient.ItemChanged{
				Type:      agent.AgentEventItemStarted,
				TurnID:    turnID,
				ItemIndex: 0,
				Item:      started,
			},
			&runtimeclient.ItemChanged{
				Type:      agent.AgentEventItemDelta,
				TurnID:    turnID,
				ItemIndex: 0,
				Delta:     &agent.AgentItemDelta{Text: getMessageText(reply.Message)},
			},
			&runtimeclient.ItemChanged{
				Type:      agent.AgentEventItemInterrupted,
				TurnID:    turnID,
				ItemIndex: 0,
				Item:      reply,
			},
			&runtimeclient.ItemsSaved{
				TurnID: turnID,
				Items: []runtimeprotocol.LoggedItem{{
					Ordinal: 1,
					Item:    reply,
				}},
			},
			&runtimeclient.ItemsSaved{
				TurnID: turnID,
				Items: []runtimeprotocol.LoggedItem{{
					Ordinal: 2,
					Item:    interrupted,
				}},
			},
			&runtimeclient.TurnFinished{
				TurnID: turnID,
				Status: defs.TurnStatusInterrupted,
			},
		}
	}
	tests := []struct {
		name   string
		events []runtimeclient.ClientEvent
		want   []string
		// wantReplyText 不为空时，回复的增量拼起来和 item/completed 的 text 都应是它。
		wantReplyText string
	}{
		{
			// 回复保存后仍有工具调用，包括不显示的 list_agents，phase 为 commentary。
			name: "reply_saved_before_its_tool_call_is_commentary",
			events: append(streamReply(reply, 0, 1),
				&runtimeclient.ItemChanged{
					Type:      agent.AgentEventItemStarted,
					TurnID:    turnID,
					ItemIndex: 1,
					Item:      newCall(""),
				},
				&runtimeclient.ItemChanged{
					Type:      agent.AgentEventItemCompleted,
					TurnID:    turnID,
					ItemIndex: 1,
					Item:      newCall(defs.ToolCallReady),
				},
			),
			want: []string{"turn/started", "item/started agentMessage", "item/agentMessage/delta", "item/completed agentMessage commentary"},
		},
		{
			name:   "reply_the_turn_ends_on_is_final_answer",
			events: append(streamReply(reply, 0, 1), turnFinished),
			want:   []string{"turn/started", "item/started agentMessage", "item/agentMessage/delta", "item/completed agentMessage final_answer", "turn/completed"},
		},
		{
			// 后续模型输出开始时，前一条回复使用 commentary。
			name:   "reply_followed_by_the_next_model_output_is_commentary",
			events: append(append(streamReply(reply, 0, 1), streamReply(newReply("more", "model-2"), 1, 2)...), turnFinished),
			want: []string{
				"turn/started", "item/started agentMessage", "item/agentMessage/delta",
				"item/completed agentMessage commentary", "item/started agentMessage", "item/agentMessage/delta",
				"item/completed agentMessage final_answer", "turn/completed",
			},
		},
		{
			// 被拦下的补充输入只保存无 ModelCallID 的 hook_run，前一条回复仍为 final_answer。
			name: "reply_followed_by_hook_run_is_final_answer",
			events: append(streamReply(reply, 0, 1),
				&runtimeclient.ItemsSaved{
					TurnID: turnID,
					Items: []runtimeprotocol.LoggedItem{{
						Ordinal: 2,
						Item: &defs.Item{
							ID:     "hook-item",
							TurnID: turnID,
							Kind:   defs.ItemHookRun,
							HookRun: &defs.HookRun{
								ID:          "run-1",
								Event:       defs.HookEventUserPromptSubmit,
								Status:      defs.HookRunBlocked,
								BlockReason: "no",
							},
						},
					}},
				},
				turnFinished,
			),
			want: []string{"turn/started", "item/started agentMessage", "item/agentMessage/delta", "item/completed agentMessage final_answer", "turn/completed"},
		},
		{
			// turn_interrupted 只标记本轮结束，被中断后保存的回复仍为 final_answer。
			name:   "interrupted_reply_followed_by_turn_interrupted_is_final_answer",
			events: interruptedReplyEvents(reply),
			want:   []string{"turn/started", "item/started agentMessage", "item/agentMessage/delta", "item/completed agentMessage final_answer", "turn/completed"},
		},
		{
			// 订阅关闭后缺少后续增量和完成事件；本轮结束后同步完整回复，补发缺少文本并完成。
			name: "reply_resynced_after_its_turn_ended_completes_with_saved_text",
			events: []runtimeclient.ClientEvent{
				&runtimeclient.ItemChanged{
					Type:      agent.AgentEventItemStarted,
					TurnID:    turnID,
					ItemIndex: 0,
					Item:      startedReply,
				},
				&runtimeclient.ItemChanged{
					Type:      agent.AgentEventItemDelta,
					TurnID:    turnID,
					ItemIndex: 0,
					Delta:     &agent.AgentItemDelta{Text: "let me"},
				},
				&runtimeclient.ConversationSynced{SessionInfos: []runtime.SessionRuntimeInfo{{
					Session:        defs.Session{ID: testRootSessionID},
					LastTurnID:     turnID,
					LastTurnStatus: defs.TurnStatusCompleted,
					Context:        runtime.ContextUsage{Source: runtime.ContextUsageSourceUnknown},
				}}},
				&runtimeclient.SessionSynced{
					Items: []runtimeprotocol.LoggedItem{{
						Ordinal: 1,
						Item:    reply,
					}},
					LastSavedOrdinal: 1,
				},
			},
			want: []string{
				"turn/started", "item/started agentMessage", "item/agentMessage/delta", "item/agentMessage/delta",
				"item/completed agentMessage final_answer", "turn/completed",
			},
			wantReplyText: "let me check",
		},
	}
	// 2. 依次交付各场景事件，核对通知顺序、回复 phase（本轮之后还有输出的是 commentary，本轮最后一条是 final_answer）、增量和完成文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outgoing := make(chan codexMessage, 64)
			thread := newDisplayTestThread(outgoing)
			for _, event := range tt.events {
				if err := thread.handleClientEvent(t.Context(), event); err != nil {
					t.Fatal(err)
				}
			}
			got, deltaText, completedText := summarizeDisplayMessages(t, outgoing)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("notifications = %q, want %q", got, tt.want)
			}
			if tt.wantReplyText != "" && (deltaText != tt.wantReplyText || completedText != tt.wantReplyText) {
				t.Fatalf("reply deltas = %q, completed text = %q, want both %q", deltaText, completedText, tt.wantReplyText)
			}
		})
	}
}
