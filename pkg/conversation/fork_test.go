package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestForkConversation 检查不同截取点、带摘要的历史、再次分叉和换模型时，分叉出的对话记录、Turn 和历史。
func TestForkConversation(t *testing.T) {
	// 1. 列出截取点、摘要、再次分叉、换模型和各种截取错误的预期结果。
	compacted := []forkSourceStep{forkStepTurn, forkStepTurn, forkStepCompact, forkStepTurn, forkStepTurn}
	tests := []struct {
		name    string
		steps   []forkSourceStep
		request func(source forkSource) *ForkConversationRequest
		wantErr error
		// wantTurns 是复制过去的来源轮次数（从第一轮起）；成功时都检查新对话的记录、Turn 记录和接起来的历史。
		wantTurns int
		check     func(t *testing.T, store *conversationStore, opt *ConversationOpt, source forkSource, forked defs.Conversation)
	}{
		{
			name:  "no_summary",
			steps: []forkSourceStep{forkStepTurn, forkStepTurn, forkStepTurn},
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source:       source.record,
					BeforeTurnID: source.turnIDs[2],
				}
			},
			wantTurns: 2,
			check: func(t *testing.T, store *conversationStore, _ *ConversationOpt, _ forkSource, forked defs.Conversation) {
				if first := readFirstLogItem(t, store, forked.ID, forked.RootSessionID); first.Kind == defs.ItemSessionFork {
					t.Fatalf("first log line = %+v, want a copied Item", first)
				}
				if forked.MessageCount != 4 {
					t.Fatalf("MessageCount = %d, want 4", forked.MessageCount)
				}
			},
		},
		{
			name:  "all_ended_turns",
			steps: []forkSourceStep{forkStepTurn, forkStepTurn, forkStepRunning},
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{Source: source.record}
			},
			wantTurns: 2,
		},
		{
			name:  "with_summary",
			steps: compacted,
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source:     source.record,
					LastTurnID: source.turnIDs[4],
				}
			},
			wantTurns: 5,
			check: func(t *testing.T, store *conversationStore, _ *ConversationOpt, source forkSource, forked defs.Conversation) {
				sourceItems := listSessionItems(t, store, source.record.ID, source.record.RootSessionID)
				summary := sourceItems[slices.IndexFunc(sourceItems, func(item *defs.Item) bool { return item.Kind == defs.ItemContextSummary })]
				// 来源开头没有 SessionFork 记录，对外序号就是行号。
				boundaryOrdinal := int64(slices.IndexFunc(sourceItems, func(item *defs.Item) bool {
					return item.ID == summary.ContextSummary.CoveredThroughItemID
				}) + 1)
				first := readFirstLogItem(t, store, forked.ID, forked.RootSessionID)
				want := &defs.SessionFork{
					SourceSessionID:      source.record.RootSessionID,
					SourceConversationID: source.record.ID,
					SourceEndOrdinal:     boundaryOrdinal,
				}
				if first.Kind != defs.ItemSessionFork || !reflect.DeepEqual(first.SessionFork, want) || first.TurnID != "" {
					t.Fatalf("first log line = %+v %+v, want marker %+v", first, first.SessionFork, want)
				}
				sourceContext := getContextItemIDs(t, store, source.record.ID, source.record.RootSessionID)
				if forkedContext := getContextItemIDs(t, store, forked.ID, forked.RootSessionID); !slices.Equal(forkedContext, sourceContext) {
					t.Fatalf("forked context = %v, want %v", forkedContext, sourceContext)
				}
				if forked.MessageCount != 8 {
					t.Fatalf("MessageCount = %d, want 8", forked.MessageCount)
				}
			},
		},
		{
			name:  "fork_of_fork_inside_prefix",
			steps: compacted,
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source:     source.record,
					LastTurnID: source.turnIDs[4],
				}
			},
			wantTurns: 5,
			check: func(t *testing.T, store *conversationStore, opt *ConversationOpt, source forkSource, forked defs.Conversation) {
				// 再次分叉只复制第一轮：这一轮在首次分叉里由 SessionFork 指向来源，要从来源历史里读出来，原样写进新日志。
				second, err := ForkConversation(t.Context(), &ForkConversationRequest{
					Source:       forked,
					BeforeTurnID: source.turnIDs[1],
				}, opt)
				if err != nil {
					t.Fatal(err)
				}
				if second.ForkedFromID != forked.ID {
					t.Fatalf("ForkedFromID = %s, want %s", second.ForkedFromID, forked.ID)
				}
				want := getItemIDs(getTurnItems(listSessionItems(t, store, source.record.ID, source.record.RootSessionID), source.turnIDs[:1]))
				if got := getItemIDs(listSessionItems(t, store, second.ID, second.RootSessionID)); len(want) == 0 || !slices.Equal(got, want) {
					t.Fatalf("second fork Items = %v, want first source Turn %v", got, want)
				}
				if first := readFirstLogItem(t, store, second.ID, second.RootSessionID); first.Kind == defs.ItemSessionFork {
					t.Fatalf("second fork first log line = %+v, want a copied Item", first)
				}
				getContextItemIDs(t, store, second.ID, second.RootSessionID)
			},
		},
		{
			name:  "model_override",
			steps: []forkSourceStep{forkStepTurn},
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source: source.record,
					Model: defs.ModelSelection{Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "other",
					}},
					ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: "local"},
				}
			},
			wantTurns: 1,
			check: func(t *testing.T, store *conversationStore, opt *ConversationOpt, source forkSource, forked defs.Conversation) {
				root, _, err := store.query.GetSession(t.Context(), forked.ID, forked.RootSessionID)
				if want := (defs.ModelSelection{
					Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "other",
					},
					ReasoningEffort: "low",
				}); err != nil || root.Model != want {
					t.Fatalf("root Model = %+v %v, want %+v", root.Model, err, want)
				}
				_, err = ForkConversation(t.Context(), &ForkConversationRequest{
					Source: source.record,
					Model: defs.ModelSelection{Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "missing",
					}},
				}, opt)
				if err == nil || err.Error() != "model test/missing is not configured" {
					t.Fatalf("fork with unconfigured model err = %v", err)
				}
				requireConversationIDs(t, store, source.record.ID, forked.ID)
			},
		},
		{
			name:  "boundary_conflict",
			steps: []forkSourceStep{forkStepTurn, forkStepTurn},
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source:       source.record,
					BeforeTurnID: source.turnIDs[1],
					LastTurnID:   source.turnIDs[0],
				}
			},
			wantErr: ErrForkBoundaryConflict,
		},
		{
			name:  "turn_not_found",
			steps: []forkSourceStep{forkStepTurn},
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source:     source.record,
					LastTurnID: defs.NewTurnID(),
				}
			},
			wantErr: ErrForkTurnNotFound,
		},
		{
			name:  "last_turn_running",
			steps: []forkSourceStep{forkStepTurn, forkStepRunning},
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source:     source.record,
					LastTurnID: source.turnIDs[1],
				}
			},
			wantErr: ErrForkTurnRunning,
		},
		{
			name:  "nothing_before_first_turn",
			steps: []forkSourceStep{forkStepTurn, forkStepTurn},
			request: func(source forkSource) *ForkConversationRequest {
				return &ForkConversationRequest{
					Source:       source.record,
					BeforeTurnID: source.turnIDs[0],
				}
			},
			wantErr: ErrNothingToFork,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 执行来源对话并分叉；预期报错时检查错误，且没有新增对话。
			model := newConversationModel(t)
			store := openConversationDAO(t)
			source := newForkSource(t, store, model, tt.steps)
			opt := conversationOpt(store, model)
			request := tt.request(source)
			forked, err := ForkConversation(t.Context(), request, opt)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ForkConversation() err = %v, want %v", err, tt.wantErr)
				}
				requireConversationIDs(t, store, source.record.ID)
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			// 3. 检查新对话和根 Session 已保存，且 ID 和来源不同。
			saved, found, err := store.query.GetConversation(t.Context(), forked.ID)
			saved.UpdatedAt = 0
			want := defs.Conversation{
				ID:               forked.ID,
				ExecutorSettings: request.ExecutorSettings,
				Title:            source.record.Title,
				RootSessionID:    forked.RootSessionID,
				MessageCount:     forked.MessageCount,
				ForkedFromID:     source.record.ID,
			}
			if err != nil || !found || !reflect.DeepEqual(saved, want) || !reflect.DeepEqual(forked, want) || forked.RootSessionID == source.record.RootSessionID {
				t.Fatalf("saved = %+v %v %v, returned %+v, want %+v", saved, found, err, forked, want)
			}
			root, found, err := store.query.GetSession(t.Context(), forked.ID, forked.RootSessionID)
			if err != nil || !found || root.AgentName != "assistant" || root.ParentSessionID != "" || root.ConversationID != forked.ID {
				t.Fatalf("root Session = %+v %v %v", root, found, err)
			}

			// 4. 检查复制的 Turn 只换了对话、Session 和保存时间，用量照抄。
			sourceTurns := listForkTestTurns(t, store, source.record.RootSessionID)[:tt.wantTurns]
			forkedTurns := listForkTestTurns(t, store, forked.RootSessionID)
			if len(forkedTurns) != len(sourceTurns) {
				t.Fatalf("forked Turns = %+v, want %d", forkedTurns, len(sourceTurns))
			}
			for index, turn := range forkedTurns {
				if turn.ConversationID != forked.ID || turn.SessionID != forked.RootSessionID {
					t.Fatalf("forked Turn %d = %+v, want Conversation %s Session %s", index, turn, forked.ID, forked.RootSessionID)
				}
				if got, want := withoutTurnOwner(turn), withoutTurnOwner(sourceTurns[index]); !reflect.DeepEqual(got, want) {
					t.Fatalf("forked Turn %d = %+v, want %+v", index, got, want)
				}
			}

			// 5. 检查新对话读出的历史（SessionFork 指向的部分也读出来）和来源保留轮次一致，再执行用例自己的检查。
			wantItems := getTurnItems(listSessionItems(t, store, source.record.ID, source.record.RootSessionID), source.turnIDs[:tt.wantTurns])
			gotItems := listSessionItems(t, store, forked.ID, forked.RootSessionID)
			if len(wantItems) == 0 || !reflect.DeepEqual(withoutUpdatedAt(gotItems), withoutUpdatedAt(wantItems)) {
				t.Fatalf("forked Items = %v, want %v", getItemIDs(gotItems), getItemIDs(wantItems))
			}
			if tt.check != nil {
				tt.check(t, store, opt, source, forked)
			}
		})
	}
}

// forkSourceStep 是分叉测试里来源对话的一步。
type forkSourceStep string

const (
	// forkStepTurn 跑一轮普通对话：一条用户消息加一条主 Agent 回复。
	forkStepTurn forkSourceStep = "turn"
	// forkStepCompact 手动压缩一次，单独占一轮。
	forkStepCompact forkSourceStep = "compact"
	// forkStepRunning 停止对话后直接写一条 running Turn 记录，当作还没结束的一轮；只能是最后一步。
	forkStepRunning forkSourceStep = "running"
)

// forkSource 是已停止的分叉来源对话。
type forkSource struct {
	// record 是重新读出的已保存记录，带标题。
	record defs.Conversation
	// turnIDs 是各步的 TurnID。
	turnIDs []defs.TurnID
}

// newForkSource 按步骤执行来源对话，停止后写入各轮用量和标题。
func newForkSource(t *testing.T, store *conversationStore, model *conversationModel, steps []forkSourceStep) forkSource {
	t.Helper()
	// 1. 创建并启动来源对话，按步骤跑普通轮、手动压缩或直接写 running Turn。
	c := createTestConversation(t, store, model)
	h := startTestConversation(t, store, model, c)
	var turnIDs []defs.TurnID
	for index, step := range steps {
		switch step {
		case forkStepTurn:
			finished := h.ackUntilTurnFinished(t, h.input(t, session.SessionInputModeNormal, fmt.Sprintf("第 %d 步", index+1)))
			turnIDs = append(turnIDs, finished.TurnID)
		case forkStepCompact:
			turnID, err := c.CompactContext(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			receiveModelRequest(t, model).reply <- modelResponse{text: "摘要"}
			if finished, _ := h.nextTurnFinished(t); finished.TurnID != turnID || finished.Status != defs.TurnStatusCompleted {
				t.Fatalf("compaction finished = %+v, want completed Turn %s", finished, turnID)
			}
			turnIDs = append(turnIDs, turnID)
		case forkStepRunning:
			h.stop(t)
			running := defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: c.GetID(),
				SessionID:      c.GetRootSessionID(),
				Status:         defs.TurnStatusRunning,
			}
			if err := store.mutation.SetTurn(t.Context(), running); err != nil {
				t.Fatal(err)
			}
			turnIDs = append(turnIDs, running.ID)
		}
	}
	h.stop(t)
	// 2. 停止后给每个已结束的 Turn 写入不同的用量。
	for index, turn := range listForkTestTurns(t, store, c.GetRootSessionID()) {
		if turn.Status == defs.TurnStatusRunning {
			continue
		}
		turn.Usage = defs.TokenUsage{
			Input:  100 * (index + 1),
			Output: index + 1,
		}
		if err := store.mutation.SetTurn(t.Context(), turn); err != nil {
			t.Fatal(err)
		}
	}
	// 3. 保存对话标题，重新读取记录并返回各步 TurnID。
	record, _, err := store.query.GetConversation(t.Context(), c.GetID())
	if err != nil {
		t.Fatal(err)
	}
	record.Title = "来源对话"
	if err := store.mutation.SetConversation(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if record, _, err = store.query.GetConversation(t.Context(), c.GetID()); err != nil {
		t.Fatal(err)
	}
	return forkSource{
		record:  record,
		turnIDs: turnIDs,
	}
}

// listForkTestTurns 按 TurnID 正序读出 Session 的全部 Turn 记录。
func listForkTestTurns(t *testing.T, store *conversationStore, sessionID defs.SessionID) []defs.Turn {
	t.Helper()
	// 1. 按 TurnID 正序查询并返回 Turn 记录。
	page, err := store.query.ListTurns(t.Context(), dao.ListTurnsRequest{
		SessionID: sessionID,
		Order:     storage.Order{Direction: storage.DirectionAsc},
		Limit:     100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// readFirstLogItem 读取 Session 原始日志的首条 Item。
func readFirstLogItem(t *testing.T, store *conversationStore, conversationID defs.ConversationID, sessionID defs.SessionID) *defs.Item {
	t.Helper()
	// 1. 从原始日志读第一行并解码 Item，不读 SessionFork 指向的来源历史。
	page, err := store.logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     storage.StreamPath(conversationID),
		StreamID: storage.StreamID(sessionID),
		Order:    storage.DirectionAsc,
		Limit:    1,
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("first log line = %+v %v", page, err)
	}
	var item defs.Item
	if err := json.Unmarshal(page.Items[0].Payload, &item); err != nil {
		t.Fatal(err)
	}
	return &item
}

// getContextItemIDs 读取 Session 交给模型的上下文历史的 Item ID。
func getContextItemIDs(t *testing.T, store *conversationStore, conversationID defs.ConversationID, sessionID defs.SessionID) []defs.ItemID {
	t.Helper()
	// 1. 读取上下文历史并返回 Item ID，读取失败时结束测试。
	history, err := dao.LoadContextHistory(t.Context(), store.query, conversationID, sessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	return getItemIDs(history)
}

// requireConversationIDs 检查没归档的对话正好是 ids。
func requireConversationIDs(t *testing.T, store *conversationStore, ids ...defs.ConversationID) {
	t.Helper()
	// 1. 查询没归档的对话，排序后检查 ID 和预期完全一致。
	page, err := store.query.ListConversations(t.Context(), dao.ListConversationsRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var got []defs.ConversationID
	for _, record := range page.Items {
		got = append(got, record.Conversation.ID)
	}
	slices.Sort(got)
	slices.Sort(ids)
	if !slices.Equal(got, ids) {
		t.Fatalf("conversations = %v, want %v", got, ids)
	}
}

// getTurnItems 返回属于 turnIDs 的 Item，保持原顺序。
func getTurnItems(items []*defs.Item, turnIDs []defs.TurnID) []*defs.Item {
	// 1. 按原顺序保留 TurnID 在名单内的 Item。
	var kept []*defs.Item
	for _, item := range items {
		if slices.Contains(turnIDs, item.TurnID) {
			kept = append(kept, item)
		}
	}
	return kept
}

// getItemIDs 按顺序提取 Item ID。
func getItemIDs(items []*defs.Item) []defs.ItemID {
	// 1. 按输入顺序提取 ID。
	ids := make([]defs.ItemID, len(items))
	for index, item := range items {
		ids[index] = item.ID
	}
	return ids
}

// withoutUpdatedAt 返回 UpdatedAt 清零的 Item 副本。
func withoutUpdatedAt(items []*defs.Item) []*defs.Item {
	// 1. 逐条复制 Item，把 UpdatedAt 清零。
	copied := make([]*defs.Item, len(items))
	for index, item := range items {
		copied[index] = item.Clone()
		copied[index].UpdatedAt = 0
	}
	return copied
}

// withoutTurnOwner 返回清空 ConversationID、SessionID 和 UpdatedAt 的 Turn 副本。
func withoutTurnOwner(turn defs.Turn) defs.Turn {
	// 1. 清空对话、Session 和保存时间。
	turn.ConversationID, turn.SessionID, turn.UpdatedAt = "", "", 0
	return turn
}

// TestForkRejectsMissingSourceHistory 检查来源历史缺失时分叉返回错误，不新增对话。
func TestForkRejectsMissingSourceHistory(t *testing.T) {
	// 1. 分别准备带摘要和不带摘要的来源并完成首次分叉。
	for _, steps := range [][]forkSourceStep{{forkStepTurn, forkStepTurn}, {forkStepTurn, forkStepTurn, forkStepCompact, forkStepTurn}} {
		t.Run(fmt.Sprintf("steps_%d", len(steps)), func(t *testing.T) {
			model, store := newConversationModel(t), openConversationDAO(t)
			source := newForkSource(t, store, model, steps)
			opt := conversationOpt(store, model)
			forked, err := ForkConversation(t.Context(), &ForkConversationRequest{Source: source.record}, opt)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 删掉首次分叉的日志，再从它分叉应返回错误，且不新增对话记录。
			if err := store.logs.Delete(t.Context(), storage.StreamPath(forked.ID), storage.StreamID(forked.RootSessionID)); err != nil {
				t.Fatal(err)
			}
			if _, err := ForkConversation(t.Context(), &ForkConversationRequest{Source: forked}, opt); err == nil {
				t.Fatal("published fork without source Items")
			}
			requireConversationIDs(t, store, source.record.ID, forked.ID)
		})
	}
}
