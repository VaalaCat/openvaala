package conversation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestConversationSpawnAgentFork 检查子 Session 按 fork_turns 继承父 Session 已结束的历史，遇到摘要时带上摘要并停止。
func TestConversationSpawnAgentFork(t *testing.T) {
	// 1. 列出不同 fork_turns 选项应继承的 Item。
	for _, tt := range []struct {
		fork string
		want []defs.ItemID
	}{
		{
			"",
			nil,
		},
		{
			"none",
			nil,
		},
		{
			"1",
			[]defs.ItemID{"T10"},
		},
		{
			"2",
			[]defs.ItemID{"T9", "T10"},
		},
		{
			"3",
			[]defs.ItemID{"S", "T9", "T10"},
		},
		{
			"all",
			[]defs.ItemID{"S", "T9", "T10"},
		},
	} {
		name := tt.fork
		if name == "" {
			name = "default_none"
		}
		t.Run(name, func(t *testing.T) {
			// 2. 保存摘要和历史轮次，创建并启动子 Session。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			c := createTestConversation(t, store, model)
			saveCompletedHistory(t, store, c.GetID(), c.GetRootSessionID(), historyMessage("omitted", "T7", "covered"))
			saveCompletedHistory(t, store, c.GetID(), c.GetRootSessionID(), &defs.Item{
				ID:          "S",
				TurnID:      "T8",
				ModelCallID: "compact",
				Kind:        defs.ItemContextSummary,
				ContextSummary: &defs.ContextSummary{
					Text:                 "summary",
					CoveredThroughItemID: "omitted",
				},
			})
			for _, id := range []defs.ItemID{"T9", "T10", "T11"} {
				saveCompletedHistory(t, store, c.GetID(), c.GetRootSessionID(), historyMessage(id, defs.TurnID(id), string(id)))
			}
			h := startTestConversation(t, store, model, c)
			call := spawnCall("T11", "spawn-1")
			id, err := h.impl().spawnAgent(t.Context(), h.record(t, c.GetRootSessionID()), call, &SpawnAgentParam{
				TaskName:  "review",
				Message:   "check",
				ForkTurns: tt.fork,
			})
			if err != nil {
				t.Fatal(err)
			}
			receiveSessionRequest(t, model, id)
			// 3. 检查子 Session 的父级信息、fork 记录和继承的 Item 顺序。
			child, found, err := store.query.GetSession(t.Context(), c.GetID(), id)
			if err != nil || !found || child.ParentTurnID != call.TurnID || child.ParentCallID != call.ToolCall.CallID || child.ParentSessionID != c.GetRootSessionID() {
				t.Fatalf("child=%+v %v", child, err)
			}
			var got []defs.ItemID
			for _, item := range listSessionItems(t, store, c.GetID(), id) {
				if item.Kind == defs.ItemSessionFork {
					if item.SessionFork.SourceSessionID != c.GetRootSessionID() || item.TurnID != "" || item.ModelCallID != "" {
						t.Fatal("bad fork")
					}
					break
				}
				got = append(got, item.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("fork=%v want=%v", got, tt.want)
			}
		})
	}
}

// TestConversationMultilevelFork 检查孙 Session 继承历史时不含父 Session 正在执行的那一轮。
func TestConversationMultilevelFork(t *testing.T) {
	// 1. 分别按最近一轮和全部历史创建两级子 Session。
	for _, fork := range []string{"1", "all"} {
		t.Run(fork, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			c := createTestConversation(t, store, model)
			original := historyMessage("original", "ended", "shared background")
			saveCompletedHistory(t, store, c.GetID(), c.GetRootSessionID(), original)
			h := startTestConversation(t, store, model, c)
			parent, err := h.impl().spawnAgent(t.Context(), h.record(t, c.GetRootSessionID()), spawnCall("active_root", "parent"), &SpawnAgentParam{
				TaskName:  "parent",
				Message:   "parent work",
				ForkTurns: "all",
			})
			if err != nil {
				t.Fatal(err)
			}
			receiveSessionRequest(t, model, parent)
			// 用父 Session 正在执行的 Turn 创建孙 Session，继承时不含这一轮。
			child, err := h.impl().spawnAgent(t.Context(), h.record(t, parent), spawnCall(latestTurn(t, store, parent).ID, "child"), &SpawnAgentParam{
				TaskName:  "child",
				Message:   "child work",
				ForkTurns: fork,
			})
			if err != nil {
				t.Fatal(err)
			}
			// 2. 检查孙 Session 只继承已结束的共享背景，不继承父 Session 正在执行的任务。
			request := receiveSessionRequest(t, model, child)
			if !strings.Contains(requestJSON(request), "shared background") || strings.Contains(requestJSON(request), "parent work") {
				t.Fatal("multilevel fork did not retain only ended history")
			}
			items := listSessionItems(t, store, c.GetID(), child)
			if len(items) != 3 || items[0].ID != original.ID || items[1].SessionFork == nil || items[1].SessionFork.SourceSessionID != parent {
				t.Fatalf("fork=%+v", items)
			}
		})
	}
}

// TestConversationSpawnAgentModel 检查 spawn_agent 为子 Session 选出的 Agent、模型和推理强度，以及无效参数的错误。
func TestConversationSpawnAgentModel(t *testing.T) {
	// 1. 列出继承父 Session 模型、指定模型、父记录没有选择和模型不在目录里的情况。
	for _, tt := range []struct {
		name          string
		agentName     string
		model         string
		effort        string
		parentModel   defs.ModelSelection // 非零时先把父 Session 切换到这个选择；spawn 仍传切换前的旧记录
		legacyParent  bool                // 是否把内存里父记录的 Model 清零，模拟旧记录？
		removedParent bool                // 是否把内存里父记录的 Model 改成目录里没有的？
		wantAgent     string
		wantSelection defs.ModelSelection
		wantUpstream  string
		wantErr       string
	}{
		{
			name:          "inherits_source_agent",
			wantAgent:     "assistant",
			wantSelection: defs.ModelSelection{Ref: testModelRef},
			wantUpstream:  "test",
		},
		{
			name:          "selected_agent",
			agentName:     "worker",
			wantAgent:     "worker",
			wantSelection: defs.ModelSelection{Ref: workerModelRef},
			wantUpstream:  "worker-model",
		},
		{
			name:      "explicit_model",
			model:     "test/other",
			effort:    "high",
			wantAgent: "assistant",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			wantUpstream: "other-upstream",
		},
		{
			name:      "explicit_model_default_effort",
			model:     "test/other",
			wantAgent: "assistant",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "low",
			},
			wantUpstream: "other-upstream",
		},
		{
			name: "inherit_latest_parent_selection",
			parentModel: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			wantAgent: "assistant",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			wantUpstream: "other-upstream",
		},
		{
			name: "inherit_with_effort_override",
			parentModel: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			effort:    "low",
			wantAgent: "assistant",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "low",
			},
			wantUpstream: "other-upstream",
		},
		{
			name:          "inherit_zero_parent_falls_back",
			legacyParent:  true,
			wantAgent:     "assistant",
			wantSelection: defs.ModelSelection{Ref: testModelRef},
			wantUpstream:  "test",
		},
		{
			name:          "inherit_removed_parent_model_fails",
			removedParent: true,
			wantErr:       "model test/gone is not configured",
		},
		{
			name:          "explicit_model_ignores_removed_parent_model",
			removedParent: true,
			model:         "test/other",
			wantAgent:     "assistant",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "low",
			},
			wantUpstream: "other-upstream",
		},
		{
			name:          "different_agent_ignores_removed_parent_model",
			removedParent: true,
			agentName:     "worker",
			wantAgent:     "worker",
			wantSelection: defs.ModelSelection{Ref: workerModelRef},
			wantUpstream:  "worker-model",
		},
		{
			name:      "different_agent_ignores_parent",
			agentName: "worker",
			parentModel: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			wantAgent:     "worker",
			wantSelection: defs.ModelSelection{Ref: workerModelRef},
			wantUpstream:  "worker-model",
		},
		{
			name:    "malformed_model",
			model:   "nope",
			wantErr: "provider_id/model_id",
		},
		{
			name:    "unknown_model",
			model:   "test/gone",
			wantErr: "test/gone",
		},
		{
			name:    "unsupported_effort",
			model:   "test/other",
			effort:  "max",
			wantErr: `does not support reasoning effort "max"`,
		},
		{
			name:      "unknown_agent",
			agentName: "missing",
			wantErr:   `agent "missing" is not configured`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 准备父 Session 最新的模型选择，用工具绑定时的旧记录创建子 Session。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			c := createTestConversation(t, store, model)
			h := startTestConversation(t, store, model, c)
			source := h.record(t, c.GetRootSessionID()) // 工具创建时绑定的旧记录
			if tt.parentModel != (defs.ModelSelection{}) {
				if _, err := c.SetSessionModel(t.Context(), c.GetRootSessionID(), tt.parentModel); err != nil {
					t.Fatal(err)
				}
			}
			if tt.legacyParent {
				legacy := h.record(t, c.GetRootSessionID())
				legacy.Model = defs.ModelSelection{}
				h.impl().sessionRecords.Store(legacy.ID, legacy)
			}
			if tt.removedParent {
				removed := h.record(t, c.GetRootSessionID())
				removed.Model = defs.ModelSelection{Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "gone",
				}}
				h.impl().sessionRecords.Store(removed.ID, removed)
			}
			id, err := h.impl().spawnAgent(t.Context(), source, spawnCall("spawn-turn", "spawn"), &SpawnAgentParam{
				TaskName:        "survey",
				Message:         "count CPU cores",
				AgentName:       tt.agentName,
				Model:           tt.model,
				ReasoningEffort: tt.effort,
			})
			// 3. 失败时检查没保存子 Session，成功时核对模型请求、身份块和记录。
			if tt.wantErr != "" {
				if err == nil || id != "" || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("spawn=%s %v", id, err)
				}
				sessions, queryErr := store.query.ListSessions(t.Context(), dao.ListSessionsRequest{
					ConversationID: c.GetID(),
					Order:          storage.Order{Direction: storage.DirectionAsc},
					Limit:          10,
				})
				if queryErr != nil || len(sessions.Items) != 1 {
					t.Fatalf("sessions=%+v %v, want only root", sessions.Items, queryErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			request := receiveSessionRequest(t, model, id)
			if request.body["model"] != tt.wantUpstream {
				t.Fatalf("child model=%v, want %s", request.body["model"], tt.wantUpstream)
			}
			// 每个请求只有它自己的一个 Session 身份块。
			if got := strings.Count(requestJSON(request), `\nSessionID: `); got != 1 {
				t.Fatalf("identity blocks=%d, want 1", got)
			}
			if child := h.record(t, id); child.AgentName != tt.wantAgent || child.Model != tt.wantSelection {
				t.Fatalf("child agent=%q model=%+v, want %q %+v", child.AgentName, child.Model, tt.wantAgent, tt.wantSelection)
			}
		})
	}
}

// TestConversationSpawnFailures 检查创建子 Session 各阶段失败后保存了哪些记录，以及父 Session 照常运行。
func TestConversationSpawnFailures(t *testing.T) {
	// 1. 分别在历史、fork、Session 保存和首次输入阶段注入错误。
	for _, stage := range []string{"inherited", "fork", "session", "first_input"} {
		t.Run(stage, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			created := createTestConversation(t, store, model)
			saveCompletedHistory(t, store, created.GetID(), created.GetRootSessionID(), historyMessage("original", "ended", "background"))
			mutation := &spawnFailureMutation{
				Mutation: store.mutation,
				root:     created.GetRootSessionID(),
				stage:    stage,
			}
			opt := conversationOpt(store, model)
			opt.Mutation = mutation
			if stage == "first_input" {
				opt.Query = &spawnStartupQuery{
					Query: store.query,
					root:  created.GetRootSessionID(),
				}
			}
			c, err := reopenTestConversation(t, created, opt)
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			// 2. 创建子 Session，核对返回的 ID、持久化的记录和内存记录。
			id, err := h.impl().spawnAgent(t.Context(), h.record(t, c.GetRootSessionID()), spawnCall("active", "spawn"), &SpawnAgentParam{
				TaskName:  "failed task",
				Message:   "work",
				ForkTurns: "all",
			})
			saved := stage == "first_input"
			if err == nil || (id != "") != saved {
				t.Fatalf("spawn=%s %v", id, err)
			}
			sessions, queryErr := store.query.ListSessions(t.Context(), dao.ListSessionsRequest{
				ConversationID: c.GetID(),
				Order:          storage.Order{Direction: storage.DirectionAsc},
				Limit:          10,
			})
			if queryErr != nil || (len(sessions.Items) == 2) != saved {
				t.Fatalf("partial persistent sessions=%+v %v", sessions.Items, queryErr)
			}
			if _, known := h.impl().sessionRecords.Load(id); saved != known {
				t.Fatalf("participant known=%v, want %v", known, saved)
			}
			if saved {
				if !strings.Contains(err.Error(), `"failed task" created; first input not accepted`) {
					t.Fatalf("first input error lost task identity: %v", err)
				}
				// 子 Session 异常退出时以普通 Agent 消息通知父方，父方继续运行。
				exited := receiveSessionRequest(t, model, c.GetRootSessionID())
				if !strings.Contains(requestJSON(exited), "异常退出") || !strings.Contains(requestJSON(exited), "injected startup") {
					t.Fatalf("parent missed abnormal exit: %s", requestJSON(exited))
				}
				exited.reply <- modelResponse{text: "noted"}
				h.nextTurnFinished(t)
			}
			// 3. 向根 Session 再提交输入，检查它照常完成。
			next := h.input(t, session.SessionInputModeNormal, "root still runs")
			receiveSessionRequest(t, model, c.GetRootSessionID()).reply <- modelResponse{text: "root result"}
			if finished, _ := h.waitTurnFinished(t, next); finished.Status != defs.TurnStatusCompleted {
				t.Fatalf("root=%+v", finished)
			}
		})
	}
}

// TestConversationFinalModelCallID 检查实时结束通知与重读历史的最终调用一致。
func TestConversationFinalModelCallID(t *testing.T) {
	// 1. 准备有正文、仅推理、无输出和模型失败的回复。
	for _, tt := range []struct {
		name     string
		response modelResponse
		status   defs.TurnStatus
	}{
		{
			name:     "message",
			response: modelResponse{text: "final answer"},
			status:   defs.TurnStatusCompleted,
		},
		{
			name:     "reasoning_only",
			response: modelResponse{reasoning: "thinking"},
			status:   defs.TurnStatusCompleted,
		},
		{
			name:   "empty_output",
			status: defs.TurnStatusCompleted,
		},
		{
			name:     "model_failure",
			response: modelResponse{status: 400},
			status:   defs.TurnStatusFailed,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 从公开输入入口运行整条主链，接收 Conversation 转发的结束通知。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			c := createTestConversation(t, store, model)
			h := startTestConversation(t, store, model, c)
			inputID := h.input(t, session.SessionInputModeNormal, "final call")
			receiveSessionRequest(t, model, c.GetRootSessionID()).reply <- tt.response
			finished, _ := h.waitTurnFinished(t, inputID)
			if finished.Status != tt.status || (finished.FinalModelCallID != "") != (tt.status == defs.TurnStatusCompleted) {
				t.Fatalf("finished = %+v, want %s", finished, tt.status)
			}
			if err := h.stop(t); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			// 3. 重新打开存储，通过批量读取和历史列表比较最终调用及终态。
			reloaded := openConversationDAOAt(t, store.root)
			records, err := reloaded.query.GetTurns(t.Context(), c.GetRootSessionID(), []defs.TurnID{finished.TurnID})
			if err != nil || len(records) != 1 {
				t.Fatalf("GetTurns() = %+v, %v", records, err)
			}
			listed := latestTurn(t, reloaded, c.GetRootSessionID())
			for _, record := range []defs.Turn{records[0], listed} {
				if record.ID != finished.TurnID || record.Status != finished.Status || record.FinalModelCallID != finished.FinalModelCallID {
					t.Fatalf("history = %+v, notification = %+v", record, finished)
				}
			}
		})
	}
}

// TestConversationSubagentNotification 检查子任务结束时发给父方的消息的正文、发送方和任务通知。
func TestConversationSubagentNotification(t *testing.T) {
	// 1. 准备成功、失败、中断和终态未保存的 Turn 结果。
	child := defs.Session{
		ID:              "child",
		ParentSessionID: "parent",
	}
	progress := historyMessage("progress", "T", "earlier progress")
	progress.ModelCallID = "earlier"
	final := func(text string) *defs.Item {
		item := historyMessage(defs.NewItemID(), "T", text)
		item.ModelCallID = "final"
		return item
	}
	reasoning := &defs.Item{
		Kind:        defs.ItemReasoning,
		ModelCallID: "final",
		Reasoning:   &defs.Reasoning{},
	}
	for _, tt := range []struct {
		name         string
		finished     session.SessionTurnFinished
		want         string
		notification bool
	}{
		{
			name: "multiple_final_messages",
			finished: session.SessionTurnFinished{
				Status:           defs.TurnStatusCompleted,
				FinalModelCallID: "final",
				Outputs:          []*defs.Item{progress, final("first"), reasoning, final("second")},
			},
			want:         "firstsecond",
			notification: true,
		},
		{
			name: "empty_final_never_falls_back",
			finished: session.SessionTurnFinished{
				Status:           defs.TurnStatusCompleted,
				FinalModelCallID: "final",
				Outputs:          []*defs.Item{progress, reasoning},
			},
			notification: true,
		},
		{
			name: "missing_final_id_never_falls_back",
			finished: session.SessionTurnFinished{
				Status:  defs.TurnStatusCompleted,
				Outputs: []*defs.Item{progress, final("answer")},
			},
			notification: true,
		},
		{
			name: "failed_explains_error",
			finished: session.SessionTurnFinished{
				Status:  defs.TurnStatusFailed,
				Outputs: []*defs.Item{progress},
				Err:     errors.New("model failed"),
			},
			want:         "model failed",
			notification: true,
		},
		{
			name: "interrupted_explains_status",
			finished: session.SessionTurnFinished{
				Status: defs.TurnStatusInterrupted,
				Err:    context.Canceled,
			},
			want:         context.Canceled.Error(),
			notification: true,
		},
		{
			name:     "unsaved_terminal_is_plain_message",
			finished: session.SessionTurnFinished{Err: errors.New("save failed")},
			want:     "save failed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 生成发给父方的消息，检查身份、正文和任务通知。
			tt.finished.TurnID = "T"
			item := newSubagentResultMessage(child, &tt.finished)
			if item.ID == "" || item.TurnID != "" || item.Message.From.Kind != defs.IdentityAgent || item.Message.From.ID != "child" || item.Message.To.ID != "parent" {
				t.Fatalf("message identity=%+v", item.Message)
			}
			if got := itemText(item); (tt.finished.Status == defs.TurnStatusCompleted && got != tt.want) || !strings.Contains(got, tt.want) {
				t.Fatalf("body=%q, want %q", got, tt.want)
			}
			notification := item.Message.SubagentNotification
			if (notification != nil) != tt.notification || (notification != nil && (notification.SourceTurnID != "T" || notification.Status != tt.finished.Status)) {
				t.Fatalf("notification=%+v", notification)
			}
		})
	}
}

// TestConversationIsDescendantCycle 检查 isDescendant 的判断，父链成环时也能结束。
func TestConversationIsDescendantCycle(t *testing.T) {
	// 1. 列出正常父链、祖先目标和无关的成环父链。
	for _, tt := range []struct {
		name    string
		records []defs.Session
		source  defs.SessionID
		target  defs.SessionID
		want    bool
	}{
		{
			name: "parent_cycle_unrelated_source_terminates",
			records: []defs.Session{
				{
					ID:              "A",
					ParentSessionID: "B",
				},
				{
					ID:              "B",
					ParentSessionID: "A",
				},
			},
			source: "X",
			target: "A",
			want:   false,
		},
		{
			name: "normal_chain_grandchild_under_root",
			records: []defs.Session{
				{ID: "root"},
				{
					ID:              "child",
					ParentSessionID: "root",
				},
				{
					ID:              "grandchild",
					ParentSessionID: "child",
				},
			},
			source: "root",
			target: "grandchild",
			want:   true,
		},
		{
			name: "ancestor_is_not_descendant",
			records: []defs.Session{
				{ID: "root"},
				{
					ID:              "child",
					ParentSessionID: "root",
				},
			},
			source: "child",
			target: "root",
			want:   false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 写入 Session 记录，检查后代判断结果。
			c := &conversationImpl{}
			for _, record := range tt.records {
				c.sessionRecords.Store(record.ID, record)
			}
			if got := c.isDescendant(tt.source, tt.target); got != tt.want {
				t.Fatalf("isDescendant(%q, %q) = %v, want %v", tt.source, tt.target, got, tt.want)
			}
		})
	}
}

// saveCompletedHistory 保存一条历史 Item 和它所在的 completed Turn。
func saveCompletedHistory(t *testing.T, store *conversationStore, conversationID defs.ConversationID, id defs.SessionID, item *defs.Item) {
	t.Helper()
	// 1. 先追加 Item，再保存对应的 completed Turn。
	if _, err := store.mutation.AppendItems(t.Context(), conversationID, id, item); err != nil {
		t.Fatal(err)
	}
	if err := store.mutation.SetTurn(t.Context(), defs.Turn{
		ID:             item.TurnID,
		ConversationID: conversationID,
		SessionID:      id,
		Status:         defs.TurnStatusCompleted,
	}); err != nil {
		t.Fatal(err)
	}
}

// historyMessage 创建历史中的用户文本消息。
func historyMessage(id defs.ItemID, turnID defs.TurnID, text string) *defs.Item {
	// 1. 将 ID、TurnID 和文本填入用户消息。
	return &defs.Item{
		ID:     id,
		TurnID: turnID,
		Kind:   defs.ItemMessage,
		Message: &defs.Message{
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
		},
	}
}

// spawnCall 创建关联父 Turn 的 spawn_agent 调用 Item。
func spawnCall(turnID defs.TurnID, callID string) *defs.Item {
	// 1. 分配 Item 和模型调用 ID，填入父 Turn 与工具 CallID。
	return &defs.Item{
		ID:          defs.NewItemID(),
		TurnID:      turnID,
		ModelCallID: defs.NewModelCallID(),
		Kind:        defs.ItemToolCall,
		ToolCall: &defs.ToolCall{
			CallID:    callID,
			Name:      "spawn_agent",
			Arguments: "{}",
			Status:    defs.ToolCallReady,
		},
	}
}

// spawnFailureMutation 包住真实 DAO 写入，在创建子 Session 的 stage 阶段注入写入错误；root 的写入不受影响。
type spawnFailureMutation struct {
	dao.Mutation
	root defs.SessionID
	// stage 是用例阶段：inherited、fork、session 时注入写入错误；first_input 时不注入，由 spawnStartupQuery 让首个输入失败。
	stage string
}

// AppendItems 在 inherited、fork 阶段给子 Session 的继承历史或 fork 记录注入写入错误。
func (m *spawnFailureMutation) AppendItems(ctx context.Context, conversationID defs.ConversationID, id defs.SessionID, items ...*defs.Item) ([]dao.LoggedItem, error) {
	// 1. 子 Session 的写入命中设定阶段时返回错误，其余交给 DAO。
	for _, item := range items {
		if id != m.root && (m.stage == "fork" && item.Kind == defs.ItemSessionFork || m.stage == "inherited" && item.Kind != defs.ItemSessionFork) {
			return nil, errors.New("injected " + m.stage)
		}
	}
	return m.Mutation.AppendItems(ctx, conversationID, id, items...)
}

// SetSession 在 session 阶段给子 Session 记录注入保存错误。
func (m *spawnFailureMutation) SetSession(ctx context.Context, record defs.Session) error {
	// 1. session 阶段保存子 Session 时返回注入错误，其余交给 DAO。
	if record.ID != m.root && m.stage == "session" {
		return errors.New("injected session")
	}
	return m.Mutation.SetSession(ctx, record)
}

// spawnStartupQuery 让非根 Session 启动时修复最近 Turn 失败：子 Session.Run 立刻异常退出，首个输入接收不了。
type spawnStartupQuery struct {
	dao.Query
	root defs.SessionID
}

// ListTurns 查询非根 Session 时返回注入错误，根 Session 照常查询。
func (q *spawnStartupQuery) ListTurns(ctx context.Context, request dao.ListTurnsRequest) (storage.Page[defs.Turn, dao.RecordCursor[defs.TurnID]], error) {
	// 1. 不是根 Session 时返回注入错误，否则交给 DAO。
	if request.SessionID != q.root {
		return storage.Page[defs.Turn, dao.RecordCursor[defs.TurnID]]{}, errors.New("injected startup")
	}
	return q.Query.ListTurns(ctx, request)
}
