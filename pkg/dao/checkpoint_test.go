package dao_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

// TestSessionCheckpointLog 检查交错记录、翻页、清空待办及删除 checkpoint 流。
func TestSessionCheckpointLog(t *testing.T) {
	// 1. 在文件日志和临时对话使用的 SQLite 内存日志上运行同一组操作。
	for _, tt := range []struct {
		name string
		open func(*testing.T) (storage.RecordStorage, storage.LogStorage)
	}{
		{
			name: "file",
			open: openStorage,
		},
		{
			name: "sqlite_memory",
			open: func(t *testing.T) (storage.RecordStorage, storage.LogStorage) {
				db, err := storagesqlite.OpenDatabase(storagesqlite.NewMemoryDatabaseOpt(dao.RecordNamespaces()...))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				})
				return db.Record(), db.Log()
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			records, logs := tt.open(t)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			const conversationID defs.ConversationID = "conversation"
			const sessionID defs.SessionID = "session"
			const inputItemID defs.ItemID = "review-message"
			pending := &defs.SessionCheckpoint{PendingUserInputs: []defs.SessionInputBatch{{{
				InputID: "review-request",
				Items: []*defs.Item{
					{
						ID:   inputItemID,
						Kind: defs.ItemMessage,
						Message: &defs.Message{Content: []*defs.Content{{
							Kind: defs.ContentText,
							Text: &defs.TextContent{Text: "Review this PR"},
						}}},
					},
					{
						ID:   "review-skill",
						Kind: defs.ItemSkillInstruction,
						SkillInstruction: &defs.SkillInstruction{
							InputItemID: inputItemID,
							Text:        "Check tests first",
						},
					},
				},
			}}}}
			first := &defs.TurnCheckpoint{
				TurnID: "first-turn",
				Step:   1,
				Phase:  defs.TurnCheckpointPhaseRequest,
				Request: &defs.TurnRequestCheckpoint{
					Purpose:                defs.ModelCallPurposeConversation,
					ModelCallID:            "first-call",
					ModelCallThroughItemID: "review-skill",
				},
			}
			second := &defs.TurnCheckpoint{
				TurnID: "second-turn",
				Step:   2,
				Phase:  defs.TurnCheckpointPhaseTools,
				Tools:  &defs.TurnToolsCheckpoint{ModelCallID: "second-call"},
			}
			// 2. 交错写入待办、首轮进度、空待办和次轮进度，回读待办及两轮。
			if err := mutation.AppendSessionCheckpoint(ctx, conversationID, sessionID, pending); err != nil {
				t.Fatal(err)
			}
			if err := mutation.AppendTurnCheckpoint(ctx, conversationID, sessionID, first); err != nil {
				t.Fatal(err)
			}
			if got, err := query.GetSessionCheckpoint(ctx, conversationID, sessionID); err != nil || !reflect.DeepEqual(got, pending) {
				t.Fatalf("GetSessionCheckpoint() = %+v, %v; want %+v", got, err, pending)
			}
			if err := mutation.AppendSessionCheckpoint(ctx, conversationID, sessionID, &defs.SessionCheckpoint{}); err != nil {
				t.Fatal(err)
			}
			for step := 1; step <= 270; step++ {
				second.Step = step
				if err := mutation.AppendTurnCheckpoint(ctx, conversationID, sessionID, second); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := query.GetSessionCheckpoint(ctx, conversationID, sessionID); err != nil || !reflect.DeepEqual(got, &defs.SessionCheckpoint{}) {
				t.Fatalf("GetSessionCheckpoint() = %+v, %v; want non-nil empty checkpoint", got, err)
			}
			for _, want := range []*defs.TurnCheckpoint{first, second} {
				if got, err := query.GetTurnCheckpoint(ctx, conversationID, sessionID, want.TurnID); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("GetTurnCheckpoint(%s) = %+v, %v; want %+v", want.TurnID, got, err, want)
				}
			}
			// 3. 清理整条流，两种记录都查不到。
			if err := mutation.DeleteSessionCheckpoints(ctx, conversationID, sessionID); err != nil {
				t.Fatal(err)
			}
			if got, err := query.GetSessionCheckpoint(ctx, conversationID, sessionID); err != nil || got != nil {
				t.Fatalf("GetSessionCheckpoint() after delete = %+v, %v", got, err)
			}
			for _, turnID := range []defs.TurnID{first.TurnID, second.TurnID} {
				if got, err := query.GetTurnCheckpoint(ctx, conversationID, sessionID, turnID); err != nil || got != nil {
					t.Fatalf("GetTurnCheckpoint(%s) after delete = %+v, %v", turnID, got, err)
				}
			}
		})
	}
}

// TestTurnCheckpointRejectsCorruption 检查 DAO 拒绝不匹配的载荷，倒查时不跳过坏行。
func TestTurnCheckpointRejectsCorruption(t *testing.T) {
	for _, tt := range []struct {
		name       string
		checkpoint string
	}{
		{
			name:       "request_and_tools",
			checkpoint: `{"turn_id":"other-turn","step":1,"phase":"request","request":{"purpose":"conversation","model_call_id":"call","model_call_through_item_id":"input"},"tools":{"model_call_id":"call"}}`,
		},
		{
			name:       "unknown_purpose",
			checkpoint: `{"turn_id":"other-turn","step":1,"phase":"request","request":{"purpose":"unknown","model_call_id":"call","model_call_through_item_id":"input"}}`,
		},
		{
			name:       "compaction_with_conversation_range",
			checkpoint: `{"turn_id":"other-turn","step":1,"phase":"request","request":{"purpose":"compaction","model_call_id":"call","model_call_through_item_id":"input","compaction":{"summary_item_id":"summary","covered_through_item_id":"covered","compact_model_ref":{"provider_id":"provider","model_id":"model"}}}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			const conversationID defs.ConversationID = "conversation"
			const sessionID defs.SessionID = "session"
			records, logs := openStorage(t)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			old := &defs.TurnCheckpoint{
				TurnID: "old-turn",
				Step:   1,
				Phase:  defs.TurnCheckpointPhaseTools,
				Tools:  &defs.TurnToolsCheckpoint{ModelCallID: "old-call"},
			}
			if err := mutation.AppendTurnCheckpoint(ctx, conversationID, sessionID, old); err != nil {
				t.Fatal(err)
			}
			var invalid defs.TurnCheckpoint
			if err := json.Unmarshal([]byte(tt.checkpoint), &invalid); err != nil {
				t.Fatal(err)
			}
			if err := mutation.AppendTurnCheckpoint(ctx, conversationID, sessionID, &invalid); err == nil {
				t.Fatal("AppendTurnCheckpoint accepted invalid checkpoint")
			}
			if _, err := logs.Append(ctx, storage.StreamPath(conversationID), "checkpoint-session", json.RawMessage(`{"turn":`+tt.checkpoint+`}`)); err != nil {
				t.Fatal(err)
			}
			if got, err := query.GetTurnCheckpoint(ctx, conversationID, sessionID, old.TurnID); err == nil || got != nil {
				t.Fatalf("GetTurnCheckpoint() = %+v, %v; want corruption error", got, err)
			}
			if got, err := query.GetSessionCheckpoint(ctx, conversationID, sessionID); err == nil || got != nil {
				t.Fatalf("GetSessionCheckpoint() = %+v, %v; want corruption error", got, err)
			}
		})
	}
}

// TestRecoveryReadsAfterIncompleteDelete 检查删除记录已写但清理失败时三个恢复查询都拒绝旧记录。
func TestRecoveryReadsAfterIncompleteDelete(t *testing.T) {
	for _, tt := range []struct {
		name    string
		wantErr error
	}{
		{
			name:    "session_item_log_cleanup_failed",
			wantErr: dao.ErrSessionDeleted,
		},
		{
			name:    "conversation_index_cleanup_failed",
			wantErr: dao.ErrConversationDeleted,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 保存一轮及待办，再在写成删除记录后注入清理错误。
			ctx := t.Context()
			records, logs := openStorage(t)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			conversation, _ := writeConversation(t, mutation)
			conversationID, sessionID := conversation.ID, conversation.RootSessionID
			turn, err := query.GetLatestLoggedTurn(ctx, conversationID, sessionID)
			if err != nil || turn == nil {
				t.Fatalf("GetLatestLoggedTurn() before deletion = %+v, %v", turn, err)
			}
			if err := mutation.AppendSessionCheckpoint(ctx, conversationID, sessionID, &defs.SessionCheckpoint{StopRequestedTurnID: turn.ID}); err != nil {
				t.Fatal(err)
			}
			if err := mutation.AppendTurnCheckpoint(ctx, conversationID, sessionID, &defs.TurnCheckpoint{
				TurnID: turn.ID,
				Step:   1,
				Phase:  defs.TurnCheckpointPhaseTools,
				Tools:  &defs.TurnToolsCheckpoint{ModelCallID: "call"},
			}); err != nil {
				t.Fatal(err)
			}
			var failedDelete error
			if tt.wantErr == dao.ErrSessionDeleted {
				_, failing, err := dao.NewDAO(dao.NewDAOOpt(records, &failingDeleteLogStorage{LogStorage: logs}))
				if err != nil {
					t.Fatal(err)
				}
				failedDelete = failing.DeleteSession(ctx, conversationID, sessionID)
			} else {
				_, failing, err := dao.NewDAO(dao.NewDAOOpt(&failingDeleteRangeStorage{RecordStorage: records}, logs))
				if err != nil {
					t.Fatal(err)
				}
				failedDelete = failing.DeleteConversation(ctx, conversationID)
			}
			if failedDelete == nil {
				t.Fatal("deletion unexpectedly succeeded")
			}
			// 2. 确认旧待办和进度仍在，恢复入口都只返回删除错误。
			page, err := logs.List(ctx, storage.ListLogEntriesRequest{
				Path:     storage.StreamPath(conversationID),
				StreamID: storage.StreamID("checkpoint-" + sessionID),
				Limit:    10,
			})
			if err != nil || len(page.Items) != 2 {
				t.Fatalf("checkpoint stream after failed delete = %+v, %v", page, err)
			}
			if got, err := query.GetSessionCheckpoint(ctx, conversationID, sessionID); got != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("GetSessionCheckpoint() = %+v, %v; want %v", got, err, tt.wantErr)
			}
			if got, err := query.GetTurnCheckpoint(ctx, conversationID, sessionID, turn.ID); got != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("GetTurnCheckpoint() = %+v, %v; want %v", got, err, tt.wantErr)
			}
			if got, err := query.GetLatestLoggedTurn(ctx, conversationID, sessionID); got != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("GetLatestLoggedTurn() = %+v, %v; want %v", got, err, tt.wantErr)
			}
		})
	}
}

// TestCheckpointRejectsDuplicateJSONKeys 检查重复的字段不能覆盖已记录的停止请求或其他进度。
func TestCheckpointRejectsDuplicateJSONKeys(t *testing.T) {
	for _, tt := range []struct {
		name    string
		payload json.RawMessage
	}{
		{
			name:    "stop_request_lost",
			payload: json.RawMessage(`{"session":{"stop_requested_turn_id":"turn","stop_requested_turn_id":""}}`),
		},
		{
			name:    "session_payload_replaced",
			payload: json.RawMessage(`{"session":{"stop_requested_turn_id":"turn"},"session":{}}`),
		},
		{
			name:    "input_group_inside_array",
			payload: json.RawMessage(`{"session":{"pending_user_inputs":[[{"input_id":"old","input_id":"new","items":[{"id":"input","kind":"message","message":{"content":[{"kind":"text","text":{"text":"hello"}}]}}]}]]}}`),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 注入字段重复的有效 JSON，读取时必须拒绝。
			records, logs := openStorage(t)
			query, _, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := logs.Append(t.Context(), "conversation", "checkpoint-session", tt.payload); err != nil {
				t.Fatal(err)
			}
			if got, err := query.GetSessionCheckpoint(t.Context(), "conversation", "session"); got != nil || err == nil {
				t.Fatalf("GetSessionCheckpoint() = %+v, %v; want corruption error", got, err)
			}
		})
	}
}

// TestTurnCheckpointStopHookContinuations 检查连续次数、曾续跑标记和 tools 阶段的清零规则。
func TestTurnCheckpointStopHookContinuations(t *testing.T) {
	for _, tt := range []struct {
		name      string
		count     int
		continued bool
		tools     bool
		wantErr   bool
	}{
		{name: "negative", count: -1, wantErr: true},
		{name: "over_limit", count: 9, continued: true, wantErr: true},
		{name: "missing_continued_flag", count: 1, wantErr: true},
		{name: "at_limit", count: 8, continued: true},
		{name: "tools_require_reset", count: 1, continued: true, tools: true, wantErr: true},
		{name: "tools_keep_flag", continued: true, tools: true},
		{name: "request_after_tools_keeps_flag", continued: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 保存并回读合法状态；非法状态分别从写入和读取路径验证。
			ctx := t.Context()
			records, logs := openStorage(t)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := defs.TurnCheckpoint{
				TurnID:                "turn",
				Step:                  2,
				StopHookContinuations: tt.count,
				IsContinuedByStopHook: tt.continued,
			}
			if tt.tools {
				checkpoint.Phase = defs.TurnCheckpointPhaseTools
				checkpoint.Tools = &defs.TurnToolsCheckpoint{ModelCallID: "call"}
			} else {
				checkpoint.Phase = defs.TurnCheckpointPhaseRequest
				checkpoint.Request = &defs.TurnRequestCheckpoint{
					Purpose:                defs.ModelCallPurposeConversation,
					ModelCallID:            "call",
					ModelCallThroughItemID: "input",
				}
			}
			if err := mutation.AppendTurnCheckpoint(ctx, "conversation", "session", &checkpoint); (err != nil) != tt.wantErr {
				t.Fatalf("AppendTurnCheckpoint() error = %v; wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				payload, err := json.Marshal(map[string]defs.TurnCheckpoint{"turn": checkpoint})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := logs.Append(ctx, "conversation", "checkpoint-session", payload); err != nil {
					t.Fatal(err)
				}
				if got, err := query.GetTurnCheckpoint(ctx, "conversation", "session", "turn"); got != nil || err == nil {
					t.Fatalf("GetTurnCheckpoint() = %+v, %v; want corruption error", got, err)
				}
			} else if got, err := query.GetTurnCheckpoint(ctx, "conversation", "session", "turn"); err != nil || !reflect.DeepEqual(got, &checkpoint) {
				t.Fatalf("GetTurnCheckpoint() = %+v, %v; want %+v", got, err, checkpoint)
			}
		})
	}
}
