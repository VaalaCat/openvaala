package conversation

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestConversationWaitAgent 检查 wait_agent 被子任务结果、用户 steer、Agent 消息唤醒或等到超时时返回的原因和发送方，以及下一次模型请求读到的消息。
func TestConversationWaitAgent(t *testing.T) {
	// 1. 准备子任务通知、用户 steer、Agent 消息和超时场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *conversationHarness)
	}{
		{
			name: "wait_agent_woken_by_subagent_result",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				childID := decodeSpawnAgentResult(t, root, "spawn")
				root.reply <- waitResponse("wait", `{"timeout_seconds":60}`)
				child.reply <- modelResponse{text: "child result"}
				next := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				var waited waitAgentResult
				decodeRequestToolResult(t, next, "wait", &waited)
				if waited.Reason != waitReasonSubagentResult || !slices.Equal(waited.From, []defs.SessionID{childID}) {
					t.Fatalf("wait=%+v", waited)
				}
				if !strings.Contains(requestJSON(next), "openvaala-subagent-notification") || !strings.Contains(requestJSON(next), "child result") {
					t.Fatalf("notification not in next request: %s", requestJSON(next))
				}
				next.reply <- modelResponse{text: "done"}
				if finished, _ := h.waitTurnFinished(t, id); finished.Status != defs.TurnStatusCompleted || len(finished.InputItemIDs) != 2 {
					t.Fatalf("finished=%+v", finished)
				}
			},
		},
		{
			name: "wait_agent_woken_by_user_steer",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "wait for me")
				receiveModelRequest(t, h.model).reply <- waitResponse("wait", `{"timeout_seconds":60}`)
				steer := h.input(t, session.SessionInputModeSteer, "extra requirement")
				next := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				var waited waitAgentResult
				decodeRequestToolResult(t, next, "wait", &waited)
				if waited.Reason != waitReasonUserSteer || len(waited.From) != 0 || !strings.Contains(requestJSON(next), "extra requirement") {
					t.Fatalf("wait=%+v request=%s", waited, requestJSON(next))
				}
				next.reply <- modelResponse{text: "ok"}
				if finished, _ := h.waitTurnFinished(t, id); !slices.Contains(finished.InputItemIDs, steer) {
					t.Fatalf("steer not read by waiting Turn: %+v", finished)
				}
			},
		},
		{
			name: "wait_agent_woken_by_agent_message",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "delegate")
				receiveModelRequest(t, h.model).reply <- spawnResponse("spawn", "review", "check A")
				root, child := receiveRootAndTask(t, h.model, "review")
				childID := decodeSpawnAgentResult(t, root, "spawn")
				if text := requestText(t, child); !strings.Contains(text, `<hint>这条消息来自上级 Agent，不是用户。`) || !strings.Contains(text, `</hint>\ncheck A`) {
					t.Fatalf("task message lacks ancestor hint: %s", text)
				}
				root.reply <- waitResponse("wait", `{"timeout_seconds":60}`)
				child.reply <- modelResponse{calls: []llmkit.ToolCall{{
					ID:        "report",
					Name:      "send_message",
					Arguments: fmt.Sprintf(`{"session_id":%q,"message":"progress report","mode":"steer"}`, h.c.GetRootSessionID()),
				}}}
				next := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				var waited waitAgentResult
				decodeRequestToolResult(t, next, "wait", &waited)
				if waited.Reason != waitReasonAgentMessage || !slices.Equal(waited.From, []defs.SessionID{childID}) {
					t.Fatalf("wait=%+v", waited)
				}
				wantSender := `<session_id>` + string(childID) + `</session_id>\n<task_name>review</task_name>\n<hint>这条消息来自你的子 Agent，不是用户。`
				if text := requestText(t, next); !strings.Contains(text, wantSender) || !strings.Contains(text, `</hint>\nprogress report`) {
					t.Fatalf("agent message lacks child sender: %s", text)
				}
				next.reply <- modelResponse{text: "done"}
				// 子任务随后结束并通知根 Session，通知可能并入同一 Turn，期间的模型请求一律回复 ack。
				h.ackUntilTurnFinished(t, id)
			},
		},
		{
			name: "wait_agent_timeout",
			run: func(t *testing.T, h *conversationHarness) {
				id := h.input(t, session.SessionInputModeNormal, "wait briefly")
				receiveModelRequest(t, h.model).reply <- waitResponse("wait", `{"timeout_seconds":1}`)
				next := receiveSessionRequest(t, h.model, h.c.GetRootSessionID())
				var waited waitAgentResult
				decodeRequestToolResult(t, next, "wait", &waited)
				if waited.Reason != waitReasonTimeout || len(waited.From) != 0 {
					t.Fatalf("wait=%+v", waited)
				}
				next.reply <- modelResponse{text: "done"}
				h.waitTurnFinished(t, id)
			},
		},
	}
	// 2. 每个场景启动独立对话执行检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			tt.run(t, startTestConversation(t, store, model, createTestConversation(t, store, model)))
		})
	}
}

// TestConversationSendMessage 检查 send_message 用 normal 时另开一轮，用 steer 时并进目标的活动 Turn。
func TestConversationSendMessage(t *testing.T) {
	// 1. 分别使用 normal 和 steer 向执行中的子 Session 发消息。
	for _, tt := range []struct {
		name      string
		mode      session.SessionInputMode
		wantTurns int
	}{
		{
			name:      "normal_queues_own_turn",
			mode:      session.SessionInputModeNormal,
			wantTurns: 2,
		},
		{
			name:      "steer_joins_active_turn",
			mode:      session.SessionInputModeSteer,
			wantTurns: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			h := startTestConversation(t, store, model, createTestConversation(t, store, model))
			root := h.record(t, h.c.GetRootSessionID())
			child, err := h.impl().spawnAgent(t.Context(), root, spawnCall("root-turn", "a"), &SpawnAgentParam{
				TaskName: "review",
				Message:  "first",
			})
			if err != nil {
				t.Fatal(err)
			}
			first := receiveSessionRequest(t, model, child)
			if err := h.impl().sendMessage(t.Context(), root, &SendMessageParam{
				SessionID: child,
				Mode:      tt.mode,
				Message:   "second",
			}); err != nil {
				t.Fatal(err)
			}
			// 2. 结束首个请求，检查第二条消息被哪个 Turn 读入、一共几轮。
			first.reply <- modelResponse{text: "first done"}
			next := receiveSessionRequest(t, model, child)
			if !strings.Contains(requestJSON(next), "second") {
				t.Fatal("target missed message")
			}
			next.reply <- modelResponse{text: "second done"}
			turns := waitCompletedTurns(t, store, child, tt.wantTurns)
			for _, item := range listSessionItems(t, store, h.c.GetID(), child) {
				if itemText(item) == "second" && item.TurnID != turns[len(turns)-1].ID {
					t.Fatalf("message read by Turn %s, want %s", item.TurnID, turns[len(turns)-1].ID)
				}
			}
		})
	}
}

// TestConversationSendMessageModel 检查 send_message 带模型或强度时替换后代的选择并用于下一次请求，失败时记录不变。
func TestConversationSendMessageModel(t *testing.T) {
	// 1. 列出换模型、换强度、模型不在目录里、模型格式错误和子 Agent 改父方模型的情况。
	goneModelRef := defs.ModelRef{
		ProviderID: "test",
		ModelID:    "gone",
	}
	const notDescendantErr = "send_message can only change the model of a descendant agent"
	tests := []struct {
		name           string
		historical     bool   // 目标是否为没在运行的历史后代？否则是刚派出、首个请求还在进行的子 Agent
		spawnModel     string // 派子 Agent 时的 model
		spawnEffort    string // 派子 Agent 时的 reasoning_effort
		currentRemoved bool   // 是否把目标当前的模型改成目录里没有的？
		fromChild      bool   // 是否由子 Agent 给直接父方发送？
		model          string
		effort         string
		wantErr        string
		wantSelection  defs.ModelSelection // 成功后目标 Session 记录里的选择，也是它下一次请求实际使用的选择
		wantUpstream   string
	}{
		{
			name:  "descendant_model_replaced",
			model: "test/other",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "low",
			},
			wantUpstream: "other-upstream",
		},
		{
			name:   "descendant_model_and_effort_replaced",
			model:  "test/other",
			effort: "high",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			wantUpstream: "other-upstream",
		},
		{
			name:        "descendant_effort_only_keeps_model",
			spawnModel:  "test/other",
			spawnEffort: "low",
			effort:      "high",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			wantUpstream: "other-upstream",
		},
		{
			name:       "idle_descendant_starts_on_new_model",
			historical: true,
			model:      "test/other",
			effort:     "high",
			wantSelection: defs.ModelSelection{
				Ref:             otherModelRef,
				ReasoningEffort: "high",
			},
			wantUpstream: "other-upstream",
		},
		{
			name:    "effort_unsupported_by_current_model",
			effort:  "high",
			wantErr: `does not support reasoning effort "high"`,
		},
		{
			name:    "model_not_configured",
			model:   "test/gone",
			wantErr: "model test/gone is not configured",
		},
		{
			name:    "model_malformed",
			model:   "nope",
			wantErr: "provider_id/model_id",
		},
		{
			name:           "effort_only_when_current_model_removed",
			currentRemoved: true,
			effort:         "high",
			wantErr:        "model test/gone is not configured",
		},
		{
			name:      "child_cannot_change_parent_model",
			fromChild: true,
			model:     "test/other",
			wantErr:   notDescendantErr,
		},
		{
			name:      "child_cannot_change_parent_effort",
			fromChild: true,
			effort:    "high",
			wantErr:   notDescendantErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 准备历史或运行中的后代，按参数向目标发送消息。
			store, model := openConversationDAO(t), newConversationModel(t)
			created := createTestConversation(t, store, model)
			var historical defs.SessionID
			if tt.historical {
				historical = saveHistoricalAgent(t, store, created.GetID(), created.GetRootSessionID(), "historical", nil)
			}
			c, err := reopenTestConversation(t, created, conversationOpt(store, model))
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			root := h.record(t, c.GetRootSessionID())
			child := historical
			var first modelRequest
			if !tt.historical {
				if child, err = h.impl().spawnAgent(t.Context(), root, spawnCall("root-turn", "a"), &SpawnAgentParam{
					TaskName:        "review",
					Message:         "first",
					Model:           tt.spawnModel,
					ReasoningEffort: tt.spawnEffort,
				}); err != nil {
					t.Fatal(err)
				}
				first = receiveSessionRequest(t, model, child)
			}
			if tt.currentRemoved {
				removed := h.record(t, child)
				removed.Model = defs.ModelSelection{Ref: goneModelRef}
				h.impl().sessionRecords.Store(child, removed)
			}
			before := h.record(t, child)
			source, target := root, child
			if tt.fromChild {
				source, target = before, root.ID
			}
			err = h.impl().sendMessage(t.Context(), source, &SendMessageParam{
				SessionID:       target,
				Mode:            session.SessionInputModeNormal,
				Message:         "second",
				Model:           tt.model,
				ReasoningEffort: tt.effort,
			})
			// 3. 失败时检查记录不变，成功时检查内存和持久化的模型选择。
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("sendMessage() = %v, want error containing %q", err, tt.wantErr)
				}
				if h.record(t, child) != before || h.record(t, root.ID) != root {
					t.Fatalf("failed send changed Session records: child %+v, before %+v", h.record(t, child), before)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := h.record(t, child).Model; got != tt.wantSelection {
				t.Fatalf("child model = %+v, want %+v", got, tt.wantSelection)
			}
			stored, found, err := store.query.GetSession(t.Context(), c.GetID(), child)
			if err != nil || !found || stored.Model != tt.wantSelection {
				t.Fatalf("stored child = %+v, %v, %v; want model %+v", stored, found, err, tt.wantSelection)
			}
			// 4. 结束旧模型的请求，检查目标下一次请求用新模型并带着消息。
			if first.reply != nil {
				first.reply <- modelResponse{text: "first done"}
			}
			next := receiveSessionRequest(t, model, child)
			if effort, _ := next.body["reasoning_effort"].(string); next.body["model"] != tt.wantUpstream || effort != tt.wantSelection.ReasoningEffort || !strings.Contains(requestJSON(next), "second") {
				t.Fatalf("next request = %s, want model %s effort %q", requestJSON(next), tt.wantUpstream, tt.wantSelection.ReasoningEffort)
			}
		})
	}
}

// TestConversationCollaborationScope 检查谁能给谁发消息、中断谁，以及 list_agents 列出的后代。
func TestConversationCollaborationScope(t *testing.T) {
	// 1. 创建两个直接子 Session 和一个孙 Session。
	store := openConversationDAO(t)
	model := newConversationModel(t)
	h := startTestConversation(t, store, model, createTestConversation(t, store, model))
	root := h.record(t, h.c.GetRootSessionID())
	a, err := h.impl().spawnAgent(t.Context(), root, spawnCall("root-turn", "a"), &SpawnAgentParam{
		TaskName: "A",
		Message:  "task",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.impl().spawnAgent(t.Context(), root, spawnCall("root-turn", "b"), &SpawnAgentParam{
		TaskName: "B",
		Message:  "task",
	})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := h.impl().spawnAgent(t.Context(), h.record(t, a), spawnCall("a-turn", "c"), &SpawnAgentParam{
		TaskName: "C",
		Message:  "task",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 2. 检查不同父子关系下能否发消息和中断。
	for _, tt := range []struct {
		name           string
		source, target defs.SessionID
		allowed        bool
	}{
		{
			name:    "parent_to_child",
			source:  h.c.GetRootSessionID(),
			target:  a,
			allowed: true,
		},
		{
			name:    "root_to_grandchild",
			source:  h.c.GetRootSessionID(),
			target:  grandchild,
			allowed: true,
		},
		{
			name:    "child_to_direct_parent",
			source:  a,
			target:  h.c.GetRootSessionID(),
			allowed: true,
		},
		{
			name:   "grandchild_to_root",
			source: grandchild,
			target: h.c.GetRootSessionID(),
		},
		{
			name:   "sibling",
			source: a,
			target: b,
		},
		{
			name:   "self",
			source: a,
			target: a,
		},
		{
			name:   "unknown",
			source: a,
			target: "unknown",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := h.record(t, tt.source)
			err := h.impl().sendMessage(t.Context(), source, &SendMessageParam{
				SessionID: tt.target,
				Mode:      session.SessionInputModeNormal,
				Message:   "hello",
			})
			if (err == nil) != tt.allowed {
				t.Fatalf("send=%v, allowed %v", err, tt.allowed)
			}
			if tt.target == h.c.GetRootSessionID() {
				return
			}
			_, err = h.impl().interruptAgent(t.Context(), source, tt.target)
			if (err == nil) != tt.allowed {
				t.Fatalf("interrupt=%v, allowed %v", err, tt.allowed)
			}
		})
	}
	// 3. 查询根 Session 的全部后代，检查排序和运行状态；没有后代的 Session 查到空列表。
	agents, err := h.impl().listAgents(t.Context(), root)
	if err != nil || len(agents) != 3 || !slices.IsSortedFunc(agents, func(x, y SessionInfo) int { return strings.Compare(string(x.Session.ID), string(y.Session.ID)) }) {
		t.Fatalf("agents=%+v %v", agents, err)
	}
	for _, agent := range agents {
		if agent.Status.State == session.SessionStateStopped {
			t.Fatalf("running agent reported stopped: %+v", agent)
		}
	}
	if agents, err := h.impl().listAgents(t.Context(), h.record(t, b)); err != nil || len(agents) != 0 {
		t.Fatalf("leaf agents=%+v %v", agents, err)
	}
}

// TestConversationInterruptAgent 检查 interrupt_agent 取消运行中后代的活动 Turn，对没在运行的后代返回空且不启动它。
func TestConversationInterruptAgent(t *testing.T) {
	// 1. 准备正在运行的后代和没在运行的历史后代。
	for _, tt := range []struct {
		name    string
		running bool
	}{
		{
			name:    "running_target",
			running: true,
		},
		{name: "historical_target"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openConversationDAO(t)
			model := newConversationModel(t)
			created := createTestConversation(t, store, model)
			historical := saveHistoricalAgent(t, store, created.GetID(), created.GetRootSessionID(), "historical", nil)
			c, err := reopenTestConversation(t, created, conversationOpt(store, model))
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			root := h.record(t, c.GetRootSessionID())
			target := historical
			if tt.running {
				if target, err = h.impl().spawnAgent(t.Context(), root, spawnCall("root-turn", "a"), &SpawnAgentParam{
					TaskName: "A",
					Message:  "task",
				}); err != nil {
					t.Fatal(err)
				}
				receiveSessionRequest(t, model, target)
			}
			// 2. 取消目标的 Turn，检查运行中的目标保存为 interrupted，历史目标没被启动。
			turnID, err := h.impl().interruptAgent(t.Context(), root, target)
			if err != nil || (turnID != "") != tt.running {
				t.Fatalf("interrupt=%s %v", turnID, err)
			}
			if tt.running {
				receiveSessionRequest(t, model, root.ID).reply <- modelResponse{text: "noted"}
				if finished, _ := h.nextTurnFinished(t); finished.Status != defs.TurnStatusCompleted {
					t.Fatalf("root=%+v", finished)
				}
				if latest := latestTurn(t, store, target); latest.ID != turnID || latest.Status != defs.TurnStatusInterrupted {
					t.Fatalf("target Turn=%+v", latest)
				}
			} else if _, found := h.impl().runningSessions.Load(target); found {
				t.Fatal("interrupt started historical target")
			}
		})
	}
}

// TestConversationReadSessionHistory 检查 read_session_history 分页读后代 fork 之后的历史，游标不能越过 fork，只能读后代。
func TestConversationReadSessionHistory(t *testing.T) {
	// 1. 保存带继承历史和五条本地消息的后代 Session。
	store := openConversationDAO(t)
	model := newConversationModel(t)
	created := createTestConversation(t, store, model)
	var local []*defs.Item
	for i := range 5 {
		local = append(local, historyMessage(defs.ItemID(fmt.Sprint("local-", i)), "T", fmt.Sprint("local ", i)))
	}
	inherited := historyMessage("inherited", "parent-turn", "inherited")
	child := saveHistoricalAgent(t, store, created.GetID(), created.GetRootSessionID(), "child", append([]*defs.Item{inherited}, local...))
	c, err := reopenTestConversation(t, created, conversationOpt(store, model))
	if err != nil {
		t.Fatal(err)
	}
	impl := c.(*conversationImpl)
	root, _ := impl.sessionRecords.Load(c.GetRootSessionID())
	childRecord, _ := impl.sessionRecords.Load(child)
	// 2. 列出最新页、中间页、最后一页、游标指到 fork、非后代和自己的情况。
	for _, tt := range []struct {
		name      string
		source    defs.Session
		target    defs.SessionID
		cursor    storage.Ordinal
		limit     int
		want      []defs.ItemID
		wantNext  bool
		wantError bool
	}{
		{
			name:     "newest_page",
			source:   root,
			target:   child,
			limit:    2,
			want:     []defs.ItemID{"local-3", "local-4"},
			wantNext: true,
		},
		{
			name:     "middle_page",
			source:   root,
			target:   child,
			cursor:   6,
			limit:    2,
			want:     []defs.ItemID{"local-1", "local-2"},
			wantNext: true,
		},
		{
			name:   "last_page_stops_at_fork",
			source: root,
			target: child,
			cursor: 4,
			limit:  20,
			want:   []defs.ItemID{"local-0"},
		},
		{
			name:      "cursor_at_fork",
			source:    root,
			target:    child,
			cursor:    2,
			limit:     20,
			wantError: true,
		},
		{
			name:      "not_descendant",
			source:    childRecord,
			target:    c.GetRootSessionID(),
			limit:     20,
			wantError: true,
		},
		{
			name:      "self",
			source:    root,
			target:    root.ID,
			limit:     20,
			wantError: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 3. 读取历史，检查消息按正序、下一页游标和预期错误。
			page, err := impl.readSessionHistory(t.Context(), tt.source, &SessionHistoryParam{
				SessionID: tt.target,
				Cursor:    tt.cursor,
				Limit:     tt.limit,
			})
			if (err != nil) != tt.wantError {
				t.Fatalf("page=%+v %v", page, err)
			}
			if tt.wantError {
				return
			}
			var got []defs.ItemID
			for _, logged := range page.Items {
				got = append(got, logged.Item.ID)
			}
			if !slices.Equal(got, tt.want) || (page.Next != 0) != tt.wantNext {
				t.Fatalf("history=%v next=%d, want %v next=%v", got, page.Next, tt.want, tt.wantNext)
			}
		})
	}
}

// saveHistoricalAgent 保存一个没在运行的子 Session：第一条样本作为继承的历史放在 fork 记录前，其余作为本地历史放在后面。
func saveHistoricalAgent(t *testing.T, store *conversationStore, conversationID defs.ConversationID, parent defs.SessionID, task string, items []*defs.Item) defs.SessionID {
	t.Helper()
	// 1. 创建子 Session 记录，把第一条样本放在 fork 记录前，其余放在后面。
	record := defs.Session{
		ID:              defs.NewSessionID(),
		ConversationID:  conversationID,
		AgentName:       "assistant",
		TaskName:        task,
		ParentSessionID: parent,
		ParentTurnID:    "spawn-turn",
		ParentCallID:    task,
	}
	fork := &defs.Item{
		ID:          defs.NewItemID(),
		Kind:        defs.ItemSessionFork,
		SessionFork: &defs.SessionFork{SourceSessionID: parent},
	}
	batch := []*defs.Item{fork}
	if len(items) > 0 {
		batch = append([]*defs.Item{items[0]}, fork)
		batch = append(batch, items[1:]...)
	}
	// 2. 先写日志，再保存 Session 记录，返回 ID。
	if _, err := store.mutation.AppendItems(t.Context(), conversationID, record.ID, batch...); err != nil {
		t.Fatal(err)
	}
	if err := store.mutation.SetSession(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	return record.ID
}

// saveUsageAgent 保存一个没在运行的子 Session，每份用量各存一个 completed Turn。
func saveUsageAgent(t *testing.T, store *conversationStore, conversationID defs.ConversationID, parent defs.SessionID, agentName string, usages []defs.TokenUsage) defs.SessionID {
	t.Helper()
	// 1. 保存子 Session 记录，再为每份用量保存一个 completed Turn。
	record := defs.Session{
		ID:              defs.NewSessionID(),
		ConversationID:  conversationID,
		AgentName:       agentName,
		TaskName:        agentName,
		ParentSessionID: parent,
		ParentTurnID:    "spawn-turn",
		ParentCallID:    string(defs.NewSessionID()),
	}
	if err := store.mutation.SetSession(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	for _, usage := range usages {
		turn := defs.Turn{
			ID:             defs.NewTurnID(),
			ConversationID: conversationID,
			SessionID:      record.ID,
			Status:         defs.TurnStatusCompleted,
			Usage:          usage,
		}
		if err := store.mutation.SetTurn(t.Context(), turn); err != nil {
			t.Fatal(err)
		}
	}
	return record.ID
}

// waitResponse 创建模型调用 wait_agent 的回复。
func waitResponse(callID, arguments string) modelResponse {
	// 1. 填入 CallID、wait_agent 名称和参数。
	return modelResponse{calls: []llmkit.ToolCall{{
		ID:        callID,
		Name:      "wait_agent",
		Arguments: arguments,
	}}}
}

// TestGetSessionInfosUsesSelectedModelWindow 检查 ContextWindow 取 Session 所选模型的窗口，没选时用 Agent 默认模型，模型不在目录里时为 0。
func TestGetSessionInfosUsesSelectedModelWindow(t *testing.T) {
	// 1. 准备用 Agent 默认模型、选了别的模型、旧记录没有选择和模型已删掉的情况。
	tests := []struct {
		name       string
		selection  defs.ModelSelection
		legacy     bool
		removed    bool // 是否把记录里的模型改成目录里没有的？
		wantWindow int
	}{
		{
			name:       "agent_default",
			wantWindow: 8000,
		},
		{
			name:       "selected_model",
			selection:  defs.ModelSelection{Ref: otherModelRef},
			wantWindow: 64000,
		},
		{
			name:       "legacy_zero_record_uses_agent_default",
			legacy:     true,
			wantWindow: 8000,
		},
		{
			name:    "removed_model_reports_zero_window",
			removed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 创建对话并设置 Session 模型，查询信息并核对上下文窗口。
			store, model := openConversationDAO(t), newConversationModel(t)
			opt := conversationOpt(store, model)
			opt.Definitions.Models[0].ContextWindow = 8000 // test/test 的窗口为 8000。
			c, err := CreateConversation(t.Context(), defs.NewConversationID(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			impl, rootID := c.(*conversationImpl), c.GetRootSessionID()
			if tt.selection != (defs.ModelSelection{}) {
				if _, err := c.SetSessionModel(t.Context(), rootID, tt.selection); err != nil {
					t.Fatal(err)
				}
			}
			record, _ := impl.sessionRecords.Load(rootID)
			if tt.legacy {
				record.Model = defs.ModelSelection{}
			}
			if tt.removed {
				record.Model = defs.ModelSelection{Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "gone",
				}}
			}
			infos, err := impl.getSessionInfos(t.Context(), []defs.Session{record})
			if err != nil || len(infos) != 1 || infos[0].ContextWindow != tt.wantWindow {
				t.Fatalf("infos = %+v, %v; want window %d", infos, err, tt.wantWindow)
			}
		})
	}
}
