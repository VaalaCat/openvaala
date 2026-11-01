package dao_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
)

// historyEntry 是测试日志里的一条 Item，由 item 按字段生成。
type historyEntry struct {
	id   defs.ItemID
	turn defs.TurnID
	// covers 不为空时这条是摘要，值是摘要覆盖到的最后一条 Item。
	covers defs.ItemID
	// fork 这条是不是 Session 分叉标记？
	fork bool
	// change 这条是不是模型切换？
	change bool
	// reasoning 这条是不是推理？
	reasoning bool
}

// item 将历史测试记录转换为 Item。
func (e historyEntry) item() *defs.Item {
	// 1. 按标记生成分叉、摘要、模型切换、推理或普通 Item。
	switch {
	case e.fork:
		return &defs.Item{
			ID:          e.id,
			Kind:        defs.ItemSessionFork,
			SessionFork: &defs.SessionFork{SourceSessionID: "source"},
		}
	case e.covers != "":
		return &defs.Item{
			ID:          e.id,
			TurnID:      e.turn,
			ModelCallID: "call",
			Kind:        defs.ItemContextSummary,
			ContextSummary: &defs.ContextSummary{
				Text:                 "summary",
				CoveredThroughItemID: e.covers,
			},
		}
	case e.change:
		return &defs.Item{
			ID:     e.id,
			TurnID: e.turn,
			Kind:   defs.ItemModelChange,
			ModelChange: &defs.ModelChange{
				PreviousModel: defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				},
				Model: defs.ModelRef{
					ProviderID: "p",
					ModelID:    "b",
				},
			},
		}
	case e.reasoning:
		return &defs.Item{
			ID:          e.id,
			TurnID:      e.turn,
			ModelCallID: "call",
			Kind:        defs.ItemReasoning,
			Reasoning: &defs.Reasoning{
				Status:  defs.ReasoningCompleted,
				Summary: []string{"r"},
			},
		}
	default:
		return &defs.Item{
			ID:     e.id,
			TurnID: e.turn,
		}
	}
}

// TestLoadContextHistory 检查上下文历史中的摘要、分叉和模型切换。
func TestLoadContextHistory(t *testing.T) {
	// 1. 准备摘要覆盖、排除轮次、继承摘要、模型切换前的推理和跨页历史场景。
	// 摘要之后追加 300 条，倒读必须跨页才能到达摘要。
	pagedEntries := []historyEntry{{id: "a"}, {id: "b"}, {
		id:     "S",
		turn:   "T",
		covers: "a",
	}}
	pagedWant := []defs.ItemID{"S", "b"}
	for i := range 300 {
		id := defs.ItemID(fmt.Sprintf("tail%03d", i))
		pagedEntries = append(pagedEntries, historyEntry{id: id})
		pagedWant = append(pagedWant, id)
	}

	tests := []struct {
		name    string
		entries []historyEntry
		exclude defs.TurnID
		want    []defs.ItemID
		wantErr bool
	}{
		{
			name:    "no_summary",
			entries: []historyEntry{{id: "1"}, {id: "2"}, {id: "3"}},
			want:    []defs.ItemID{"1", "2", "3"},
		},
		{
			name: "summary_keeps_uncovered_tail",
			entries: []historyEntry{{id: "1"}, {id: "2"}, {id: "3"}, {id: "4"}, {
				id:     "S5",
				turn:   "T",
				covers: "2",
			}, {id: "6"}},
			want: []defs.ItemID{"S5", "3", "4", "6"},
		},
		{
			name: "excluded_turn_summary_ignored",
			entries: []historyEntry{
				{
					id:   "1",
					turn: "T1",
				}, {
					id:   "2",
					turn: "T1",
				}, {
					id:   "3",
					turn: "T2",
				},
				{
					id:   "4",
					turn: "T3",
				}, {
					id:     "S5",
					turn:   "T3",
					covers: "2",
				}, {
					id:   "6",
					turn: "T3",
				},
			},
			exclude: "T3",
			want:    []defs.ItemID{"1", "2", "3"},
		},
		{
			name: "earlier_summary_before_excluded_turn",
			entries: []historyEntry{
				{
					id:   "1",
					turn: "T1",
				}, {
					id:     "S2",
					turn:   "T1",
					covers: "1",
				}, {
					id:   "3",
					turn: "T2",
				},
				{
					id:   "4",
					turn: "T3",
				}, {
					id:     "S5",
					turn:   "T3",
					covers: "3",
				}, {
					id:   "6",
					turn: "T3",
				},
			},
			exclude: "T3",
			want:    []defs.ItemID{"S2", "3"},
		},
		{
			name: "inherited_summary_without_boundary",
			entries: []historyEntry{
				{
					id:     "S0",
					turn:   "T0",
					covers: "missing",
				}, {id: "A1"}, {id: "A2"}, {
					id:   "Fork",
					fork: true,
				}, {id: "B1"},
			},
			want: []defs.ItemID{"S0", "A1", "A2", "B1"},
		},
		{
			name: "local_summary_covers_inherited",
			entries: []historyEntry{
				{
					id:     "S0",
					turn:   "T0",
					covers: "missing",
				}, {id: "A1"}, {id: "A2"}, {
					id:   "Fork",
					fork: true,
				}, {id: "B1"},
				{
					id:     "S1",
					turn:   "T1",
					covers: "A1",
				}, {id: "B2"},
			},
			want: []defs.ItemID{"S1", "A2", "B1", "B2"},
		},
		{
			name: "local_summary_missing_boundary",
			entries: []historyEntry{
				{id: "A1"}, {
					id:   "Fork",
					fork: true,
				}, {id: "B1"}, {
					id:     "S1",
					turn:   "T1",
					covers: "missing",
				},
			},
			wantErr: true,
		},
		{
			name: "reasoning_before_model_change_dropped",
			entries: []historyEntry{
				{
					id:   "u1",
					turn: "T1",
				}, {
					id:        "r1",
					turn:      "T1",
					reasoning: true,
				}, {
					id:   "a1",
					turn: "T1",
				},
				{
					id:     "c2",
					turn:   "T2",
					change: true,
				}, {
					id:   "u2",
					turn: "T2",
				}, {
					id:        "r2",
					turn:      "T2",
					reasoning: true,
				},
			},
			want: []defs.ItemID{"u1", "a1", "c2", "u2", "r2"},
		},
		{
			name: "two_changes_keep_reasoning_after_latest",
			entries: []historyEntry{
				{
					id:        "r1",
					turn:      "T1",
					reasoning: true,
				}, {
					id:     "c2",
					turn:   "T2",
					change: true,
				}, {
					id:        "r2",
					turn:      "T2",
					reasoning: true,
				},
				{
					id:     "c3",
					turn:   "T3",
					change: true,
				}, {
					id:        "r3",
					turn:      "T3",
					reasoning: true,
				},
			},
			want: []defs.ItemID{"c2", "c3", "r3"},
		},
		{
			// 分界早于摘要窗口：窗口里的推理都来自当前模型，全部保留。
			name: "change_covered_by_summary_keeps_reasoning",
			entries: []historyEntry{
				{
					id:   "1",
					turn: "T1",
				}, {
					id:     "c2",
					turn:   "T2",
					change: true,
				}, {
					id:   "3",
					turn: "T2",
				},
				{
					id:     "S4",
					turn:   "T3",
					covers: "3",
				}, {
					id:        "r5",
					turn:      "T3",
					reasoning: true,
				}, {
					id:   "6",
					turn: "T3",
				},
			},
			want: []defs.ItemID{"S4", "r5", "6"},
		},
		{
			name: "no_change_keeps_reasoning",
			entries: []historyEntry{{
				id:        "r1",
				turn:      "T1",
				reasoning: true,
			}, {
				id:   "2",
				turn: "T1",
			}},
			want: []defs.ItemID{"r1", "2"},
		},
		{
			name:    "across_pages",
			entries: pagedEntries,
			want:    pagedWant,
		},
	}
	// 2. 写入历史后加载上下文，检查 Item 顺序及错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			for _, entry := range tt.entries {
				if _, err := mutation.AppendItems(t.Context(), conversationID, sessionID, entry.item()); err != nil {
					t.Fatal(err)
				}
			}

			got, err := dao.LoadContextHistory(t.Context(), query, conversationID, sessionID, tt.exclude)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadContextHistory() error = %v, wantErr %v", err, tt.wantErr)
			}
			gotIDs := make([]defs.ItemID, len(got))
			for i, item := range got {
				gotIDs[i] = item.ID
			}
			if !slices.Equal(gotIDs, tt.want) && !tt.wantErr {
				t.Fatalf("LoadContextHistory() = %v, want %v", gotIDs, tt.want)
			}
		})
	}
}

// TestLoadContextHistoryModelFailures 检查失败模型调用从请求历史中排除。
func TestLoadContextHistoryModelFailures(t *testing.T) {
	// 1. 准备失败调用、成功回复、摘要、分叉和用户中断场景。
	message := func(id defs.ItemID, modelCallID defs.ModelCallID, text string) *defs.Item {
		kind := defs.IdentityUser
		if modelCallID != "" {
			kind = defs.IdentityAgent
		}
		return &defs.Item{
			ID:          id,
			TurnID:      "turn",
			ModelCallID: modelCallID,
			Kind:        defs.ItemMessage,
			Message: &defs.Message{
				From: &defs.Address{
					Kind: kind,
					ID:   "sender",
				},
				Content: []*defs.Content{{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: text},
				}},
			},
		}
	}
	user := message("user", "", "user input")
	failed := []*defs.Item{
		message("failed-complete", "failed", "failed complete text"),
		{
			ID:          "failed-reasoning",
			TurnID:      "turn",
			ModelCallID: "failed",
			Kind:        defs.ItemReasoning,
			Reasoning: &defs.Reasoning{
				Status:  defs.ReasoningCompleted,
				Summary: []string{"failed reasoning"},
			},
		},
		{
			ID:          "failed-tool",
			TurnID:      "turn",
			ModelCallID: "failed",
			Kind:        defs.ItemToolCall,
			ToolCall: &defs.ToolCall{
				CallID:    "failed-tool-call",
				Name:      "bash",
				Arguments: `{"command":"echo failed"}`,
				Status:    defs.ToolCallReady,
			},
		},
		message("failed-partial", "failed", "failed partial text"),
		{
			ID:          "failed-error",
			TurnID:      "turn",
			ModelCallID: "failed",
			Kind:        defs.ItemModelError,
			ModelError: &defs.ModelError{
				Code:    "unexpected_eof",
				Message: "failed stream",
			},
		},
	}
	success := message("success", "successful", "successful reply")
	summary := (historyEntry{
		id:     "summary",
		turn:   "summary-turn",
		covers: "failed-complete",
	}).item()
	errorBoundarySummary := (historyEntry{
		id:     "summary",
		turn:   "summary-turn",
		covers: "failed-error",
	}).item()
	inheritedSummary := (historyEntry{
		id:     "inherited-summary",
		turn:   "inherited-turn",
		covers: "not-copied",
	}).item()
	fork := (historyEntry{
		id:   "fork",
		fork: true,
	}).item()
	toolCall := &defs.Item{
		ID:          "tool-call",
		TurnID:      "turn",
		ModelCallID: "earlier",
		Kind:        defs.ItemToolCall,
		ToolCall: &defs.ToolCall{
			CallID:    "earlier-tool-call",
			Name:      "bash",
			Arguments: `{}`,
			Status:    defs.ToolCallReady,
		},
	}
	toolResult := &defs.Item{
		ID:          "tool-result",
		TurnID:      "turn",
		ModelCallID: "earlier",
		Kind:        defs.ItemToolResult,
		ToolResult: &defs.ToolResult{
			CallID:  "earlier-tool-call",
			Content: json.RawMessage(`"earlier tool output"`),
		},
	}
	tests := []struct {
		name        string
		items       []*defs.Item
		wantIDs     []defs.ItemID
		wantText    []string
		wantToolIDs []string
	}{
		{
			name:        "failed_call_removed_but_earlier_tool_result_kept",
			items:       append(append([]*defs.Item{user, toolCall, toolResult}, failed...), success),
			wantIDs:     []defs.ItemID{"user", "tool-call", "tool-result", "success"},
			wantText:    []string{"user input", "earlier tool output", "successful reply"},
			wantToolIDs: []string{"earlier-tool-call"},
		},
		{
			name:     "summary_boundary_is_failed_output",
			items:    append(append([]*defs.Item{user}, failed...), success, summary),
			wantIDs:  []defs.ItemID{"summary", "success"},
			wantText: []string{"summary", "successful reply"},
		},
		{
			name:     "summary_boundary_is_error_item",
			items:    append(append([]*defs.Item{user}, failed...), success, errorBoundarySummary),
			wantIDs:  []defs.ItemID{"summary", "success"},
			wantText: []string{"summary", "successful reply"},
		},
		{
			name:     "inherited_summary_and_fork_keep_failure_excluded",
			items:    append(append([]*defs.Item{inheritedSummary, user}, failed...), success, fork, message("next-user", "", "next input")),
			wantIDs:  []defs.ItemID{"inherited-summary", "user", "success", "next-user"},
			wantText: []string{"summary", "user input", "successful reply", "next input"},
		},
		{
			name: "user_interruption_without_model_error_keeps_partial_text",
			items: []*defs.Item{
				user,
				message("interrupted-partial", "interrupted", "interrupted partial reply"),
				{
					ID:     "interrupted",
					TurnID: "turn",
					Kind:   defs.ItemTurnInterrupted,
				},
			},
			wantIDs:  []defs.ItemID{"user", "interrupted-partial", "interrupted"},
			wantText: []string{"user input", "interrupted partial reply", "openvaala-turn-interrupted-event"},
		},
	}
	// 2. 分别在保存后和重新打开时读取上下文，构建模型请求并检查原始日志未变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, _ := openStorage(t)
			logDirectory := t.TempDir()
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			for _, phase := range []string{"saved", "reopened"} {
				logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(logDirectory))
				if err != nil {
					t.Fatal(err)
				}
				query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
				if err != nil {
					t.Fatal(err)
				}
				if phase == "saved" {
					if _, err := mutation.AppendItems(t.Context(), conversationID, sessionID, tt.items...); err != nil {
						t.Fatal(err)
					}
				}
				history, err := dao.LoadContextHistory(t.Context(), query, conversationID, sessionID, "")
				if err != nil {
					t.Fatalf("%s: LoadContextHistory(): %v", phase, err)
				}
				var gotIDs []defs.ItemID
				for _, item := range history {
					gotIDs = append(gotIDs, item.ID)
				}
				if !slices.Equal(gotIDs, tt.wantIDs) {
					t.Fatalf("%s: context IDs = %v, want %v", phase, gotIDs, tt.wantIDs)
				}
				model := &defs.ModelDefinition{Target: &llmkit.Target{Model: "model"}}
				context, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(
					sessionID, "next-turn", llmkit.NewClient(llmkit.ClientOptions{}), model, model,
				))
				if err != nil {
					t.Fatal(err)
				}
				if err := context.Load(&agent_context.ContextLoadParam{Items: history}); err != nil {
					t.Fatalf("%s: load context: %v", phase, err)
				}
				request, _, err := context.BuildModelRequest(nil)
				if err != nil {
					t.Fatalf("%s: build model request: %v", phase, err)
				}
				var requestText strings.Builder
				var toolIDs []string
				for _, message := range request.Messages {
					for _, content := range message.Content {
						requestText.WriteString(content.Text)
					}
					for _, call := range message.ToolCalls {
						toolIDs = append(toolIDs, call.ID)
					}
				}
				if !slices.Equal(toolIDs, tt.wantToolIDs) {
					t.Fatalf("%s: request tool IDs = %v, want %v", phase, toolIDs, tt.wantToolIDs)
				}
				for _, forbidden := range []string{"failed", "missing_output", "openvaala-model-error"} {
					if strings.Contains(requestText.String(), forbidden) {
						t.Fatalf("%s: request includes %q: %s", phase, forbidden, requestText.String())
					}
				}
				for _, text := range tt.wantText {
					if !strings.Contains(requestText.String(), text) {
						t.Fatalf("%s: request does not include %q: %s", phase, text, requestText.String())
					}
				}
				index := 0
				for item, err := range query.ScanItems(t.Context(), conversationID, sessionID, storage.DirectionAsc) {
					if err != nil {
						t.Fatal(err)
					}
					if index >= len(tt.items) {
						t.Fatalf("%s: raw history contains extra item %q", phase, item.ID)
					}
					want := tt.items[index].Clone()
					want.UpdatedAt = item.UpdatedAt
					if !reflect.DeepEqual(item, want) {
						t.Fatalf("%s: raw history item %d = %+v, want %+v", phase, index, item, want)
					}
					index++
				}
				if index != len(tt.items) {
					t.Fatalf("%s: raw history has %d items, want %d", phase, index, len(tt.items))
				}
			}
		})
	}
}
