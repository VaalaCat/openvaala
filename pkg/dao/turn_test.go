package dao_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestTurnQueryAndMutationSetGetAndList 检查 Turn 保存、读取及 Session 内分页。
func TestTurnQueryAndMutationSetGetAndList(t *testing.T) {
	// 1. 准备 Key 与写入时间升降序样例。
	tests := []struct {
		name    string
		order   storage.Order
		wantIDs []defs.TurnID
	}{
		{
			name:    "ascending",
			order:   storage.Order{Direction: storage.DirectionAsc},
			wantIDs: []defs.TurnID{"a", "z"},
		},
		{
			name:    "descending",
			order:   storage.Order{Direction: storage.DirectionDesc},
			wantIDs: []defs.TurnID{"z", "a"},
		},
		{
			name: "updated_at_ascending",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionAsc,
			},
			wantIDs: []defs.TurnID{"z", "a"},
		},
		{
			name: "updated_at_descending",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionDesc,
			},
			wantIDs: []defs.TurnID{"a", "z"},
		},
	}
	// 2. 写入两个 Session 下同名 Turn，检查读取隔开及逐页顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			sessionIDs := []defs.SessionID{defs.NewSessionID(), defs.NewSessionID()}
			turns := [][]defs.Turn{
				{
					{
						ID:             "z",
						ConversationID: conversationID,
						SessionID:      sessionIDs[0],
						Status:         defs.TurnStatusRunning,
					},
					{
						ID:             "a",
						ConversationID: conversationID,
						SessionID:      sessionIDs[0],
						Status:         defs.TurnStatusCompleted,
					},
				},
				{
					{
						ID:             "z",
						ConversationID: conversationID,
						SessionID:      sessionIDs[1],
						Status:         defs.TurnStatusInterrupted,
					},
					{
						ID:             "a",
						ConversationID: conversationID,
						SessionID:      sessionIDs[1],
						Status:         defs.TurnStatusFailed,
					},
				},
			}
			for _, sessionTurns := range turns {
				for _, record := range sessionTurns {
					waitNextMillisecond() // 写入顺序即最后写入时间顺序：z 早于 a
					if err := mutation.SetTurn(t.Context(), record); err != nil {
						t.Fatal(err)
					}
				}
			}

			for index, sessionID := range sessionIDs {
				got, exists, err := query.GetTurn(t.Context(), sessionID, "z")
				want := turns[index][0]
				want.UpdatedAt = got.UpdatedAt
				if err != nil || !exists || got.UpdatedAt == 0 || !reflect.DeepEqual(got, want) {
					t.Fatalf("GetTurn(%q, z) = %+v, %v, %v, want %+v", sessionID, got, exists, err, want)
				}
			}

			request := dao.ListTurnsRequest{
				SessionID: sessionIDs[0],
				Order:     tt.order,
				Limit:     1,
			}
			var gotIDs []defs.TurnID
			for {
				page, err := query.ListTurns(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				for _, value := range page.Items {
					gotIDs = append(gotIDs, value.ID)
				}
				if page.Next == (dao.RecordCursor[defs.TurnID]{}) {
					break
				}
				last := page.Items[len(page.Items)-1]
				if page.Next != (dao.RecordCursor[defs.TurnID]{
					ID:        last.ID,
					UpdatedAt: last.UpdatedAt,
				}) {
					t.Fatalf("ListTurns() Next = %+v, want last Turn position", page.Next)
				}
				request.Cursor = page.Next
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Fatalf("ListTurns() IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// TestGetSessionUsage 检查 Session 用量汇总。
func TestGetSessionUsage(t *testing.T) {
	// 1. 准备空用量、多页 Turn 及另一 Session 的用量。
	usageOf := func(input, output int) defs.TokenUsage {
		return defs.TokenUsage{
			Input:      input,
			Output:     output,
			CacheRead:  input / 2,
			CacheWrite: input / 4,
			Reasoning:  output / 2,
		}
	}
	manyTurns := make([]defs.TokenUsage, 300) // 多于一页，覆盖翻页读取
	var manyWant defs.TokenUsage
	for index := range manyTurns {
		manyTurns[index] = usageOf(index+1, 2)
		manyWant = manyWant.Add(manyTurns[index])
	}
	tests := []struct {
		name   string
		target []defs.TokenUsage // 目标 Session 各 Turn 的用量
		other  []defs.TokenUsage // 另一个 Session 各 Turn 的用量，不应计入
		want   defs.TokenUsage
	}{
		{name: "no_turns"},
		{
			name:   "turn_without_usage",
			target: []defs.TokenUsage{usageOf(100, 20), {}, usageOf(40, 8)},
			want:   usageOf(100, 20).Add(usageOf(40, 8)),
		},
		{
			name:   "multiple_pages",
			target: manyTurns,
			want:   manyWant,
		},
		{
			name:   "ignores_other_session",
			target: []defs.TokenUsage{usageOf(10, 2)},
			other:  []defs.TokenUsage{usageOf(1000, 200)},
			want:   usageOf(10, 2),
		},
	}
	// 2. 写入各 Turn，检查只累加目标 Session。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID := defs.NewConversationID()
			targetID, otherID := defs.NewSessionID(), defs.NewSessionID()
			for sessionID, usages := range map[defs.SessionID][]defs.TokenUsage{targetID: tt.target, otherID: tt.other} {
				for _, usage := range usages {
					turn := defs.Turn{
						ID:             defs.NewTurnID(),
						ConversationID: conversationID,
						SessionID:      sessionID,
						Status:         defs.TurnStatusCompleted,
						Usage:          usage,
						ContextUsage: &defs.TurnContextUsage{
							ModelCallID:        defs.NewModelCallID(),
							InputThroughItemID: defs.NewItemID(),
							Usage:              defs.TokenUsage{Input: 9000, Output: 1000},
						},
					}
					if err := mutation.SetTurn(t.Context(), turn); err != nil {
						t.Fatal(err)
					}
				}
			}

			got, err := dao.GetSessionUsage(t.Context(), query, targetID)
			if err != nil || got != tt.want {
				t.Fatalf("GetSessionUsage() = %+v, %v, want %+v", got, err, tt.want)
			}
		})
	}
}

// TestTurnContextUsage 检查普通调用报告与累计消耗分别保存，读取结果各持独立副本。
func TestTurnContextUsage(t *testing.T) {
	// 1. 保存有用量、无用量、未知模式和没有报告的 Turn。
	for _, tt := range []struct {
		name   string
		report *defs.TurnContextUsage
	}{
		{name: "no_report"},
		{name: "current_turn_with_usage", report: &defs.TurnContextUsage{ModelCallID: "call", InputThroughItemID: "input", Usage: defs.TokenUsage{Input: 9000, Output: 5000, Reasoning: 4000, CacheRead: 2000}, ReasoningContext: "current_turn"}},
		{name: "known_without_usage", report: &defs.TurnContextUsage{ModelCallID: "call", InputThroughItemID: "input", ReasoningContext: "all_turns"}},
		{name: "unknown_with_usage", report: &defs.TurnContextUsage{ModelCallID: "call", InputThroughItemID: "input", Usage: defs.TokenUsage{Input: 100, Output: 20}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			record := defs.Turn{ID: defs.NewTurnID(), ConversationID: defs.NewConversationID(), SessionID: defs.NewSessionID(), Status: defs.TurnStatusCompleted, Usage: defs.TokenUsage{Input: 12000, Output: 6000}, ContextUsage: tt.report}
			if err := mutation.SetTurn(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			got, found, err := query.GetTurn(t.Context(), record.SessionID, record.ID)
			if err != nil || !found || !reflect.DeepEqual(got.ContextUsage, tt.report) || got.Usage != record.Usage {
				t.Fatalf("GetTurn = %+v, %v", got, err)
			}
			// 2. 修改一份查询结果后重新读取，持久化的报告保持原值。
			if got.ContextUsage != nil {
				got.ContextUsage.InputThroughItemID = "changed"
				got.ContextUsage.Usage.Input = -1
			}
			page, err := query.ListTurns(t.Context(), dao.ListTurnsRequest{SessionID: record.SessionID, Limit: 1})
			if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].ContextUsage, tt.report) {
				t.Fatalf("ListTurns = %+v, %v", page, err)
			}
		})
	}
}

// TestTurnQueryGetMissing 检查缺失 Turn 返回零值和未找到。
func TestTurnQueryGetMissing(t *testing.T) {
	// 1. 查询不存在的 Turn，检查无错误、未找到且记录为零值。
	query, _, _ := openDAO(t)
	got, exists, err := query.GetTurn(t.Context(), defs.NewSessionID(), "missing")
	if err != nil || exists || !reflect.DeepEqual(got, defs.Turn{}) {
		t.Fatalf("GetTurn(missing) = %+v, %v, %v", got, exists, err)
	}
}

// TestGetTurns 检查按 ID 批量读取 Turn。
func TestGetTurns(t *testing.T) {
	// 1. 准备请求顺序、缺失、空批次和错误 Session 样例。
	tests := []struct {
		name      string
		sessionID func(stored, other defs.SessionID) defs.SessionID
		ids       []defs.TurnID
		wantIDs   []defs.TurnID
		wantError bool
	}{
		{
			name:    "request_order_skipping_missing",
			ids:     []defs.TurnID{"t3", "missing", "t1"},
			wantIDs: []defs.TurnID{"t3", "t1"},
		},
		{
			name:    "no_ids",
			wantIDs: []defs.TurnID{},
		},
		{
			name:    "only_missing",
			ids:     []defs.TurnID{"missing"},
			wantIDs: []defs.TurnID{},
		},
		{
			name:      "other_session_has_none_of_them",
			sessionID: func(_, other defs.SessionID) defs.SessionID { return other },
			ids:       []defs.TurnID{"t1", "t2"},
			wantIDs:   []defs.TurnID{},
		},
		{
			name:      "empty_session_id",
			sessionID: func(_, _ defs.SessionID) defs.SessionID { return "" },
			ids:       []defs.TurnID{"t1"},
			wantError: true,
		},
	}
	// 2. 保存三条 Turn，检查按输入顺序跳过缺失记录及空 Session 错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			conversationID, storedID, otherID := defs.NewConversationID(), defs.NewSessionID(), defs.NewSessionID()
			stored := make(map[defs.TurnID]defs.Turn)
			for _, id := range []defs.TurnID{"t1", "t2", "t3"} {
				record := defs.Turn{
					ID:             id,
					ConversationID: conversationID,
					SessionID:      storedID,
					Status:         defs.TurnStatusCompleted,
				}
				err := mutation.SetTurn(t.Context(), record)
				if err != nil {
					t.Fatal(err)
				}
				var found bool
				if stored[id], found, err = query.GetTurn(t.Context(), storedID, id); err != nil || !found {
					t.Fatalf("GetTurn(%s) = %v, %v", id, found, err)
				}
			}
			sessionID := storedID
			if tt.sessionID != nil {
				sessionID = tt.sessionID(storedID, otherID)
			}

			got, err := query.GetTurns(t.Context(), sessionID, tt.ids)
			if tt.wantError {
				if err == nil {
					t.Fatalf("GetTurns(%q) error = nil, want an error", sessionID)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := make([]defs.Turn, 0, len(tt.wantIDs))
			for _, id := range tt.wantIDs {
				want = append(want, stored[id])
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("GetTurns() = %+v, want %+v", got, want)
			}
		})
	}
}

// TestTurnMutationRejectsInvalidTurn 检查拒绝无效 Turn。
func TestTurnMutationRejectsInvalidTurn(t *testing.T) {
	// 1. 准备缺少身份或未知状态的记录。
	tests := []struct {
		name  string
		value defs.Turn
	}{
		{
			name: "missing_id",
			value: defs.Turn{
				ConversationID: defs.NewConversationID(),
				SessionID:      defs.NewSessionID(),
				Status:         defs.TurnStatusRunning,
			},
		},
		{
			name: "missing_session_id",
			value: defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: defs.NewConversationID(),
				Status:         defs.TurnStatusRunning,
			},
		},
		{
			name: "missing_conversation",
			value: defs.Turn{
				ID:        defs.NewTurnID(),
				SessionID: defs.NewSessionID(),
				Status:    defs.TurnStatusRunning,
			},
		},
		{
			name: "unsupported_status",
			value: defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: defs.NewConversationID(),
				SessionID:      defs.NewSessionID(),
				Status:         "unknown",
			},
		},
	}
	// 2. 检查保存返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, mutation, _ := openDAO(t)
			if err := mutation.SetTurn(t.Context(), tt.value); err == nil {
				t.Fatal("SetTurn() error = nil")
			}
		})
	}
}

// TestTurnMutationStatuses 检查各 Turn 状态可保存和读取。
func TestTurnMutationStatuses(t *testing.T) {
	// 1. 准备运行、完成、失败和中断四种状态。
	tests := []defs.TurnStatus{
		defs.TurnStatusRunning,
		defs.TurnStatusCompleted,
		defs.TurnStatusFailed,
		defs.TurnStatusInterrupted,
	}
	// 2. 保存带 Executor 列表的 Turn，回读检查全部字段。
	for _, status := range tests {
		t.Run(string(status), func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			want := defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: defs.NewConversationID(),
				SessionID:      defs.NewSessionID(),
				Status:         status,
				Executors: []defs.TurnExecutor{{
					ID:                 "local",
					WorkspaceDirectory: "/a",
				}, {
					ID:                 "gpu",
					WorkspaceDirectory: "/data",
				}},
			}
			if err := mutation.SetTurn(t.Context(), want); err != nil {
				t.Fatal(err)
			}
			got, found, err := query.GetTurn(t.Context(), want.SessionID, want.ID)
			want.UpdatedAt = got.UpdatedAt
			if err != nil || !found || got.UpdatedAt == 0 || !reflect.DeepEqual(got, want) {
				t.Fatalf("GetTurn() = %+v, %v, %v, want %+v", got, found, err, want)
			}
		})
	}
}

// TestTurnQueryRejectsDamagedRecord 检查读取损坏 Turn 时返回错误。
func TestTurnQueryRejectsDamagedRecord(t *testing.T) {
	// 1. 准备身份缺失、Key 不符、未知状态和错误 JSON 形状。
	sessionID := defs.NewSessionID()
	otherSessionID := defs.NewSessionID()
	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing_id",
			payload: `{"session_id":"` + string(sessionID) + `","status":"running"}`,
		},
		{
			name:    "id_differs_from_key",
			payload: `{"id":"other","session_id":"` + string(sessionID) + `","status":"running"}`,
		},
		{
			name:    "session_differs_from_key",
			payload: `{"id":"damaged","session_id":"` + string(otherSessionID) + `","status":"running"}`,
		},
		{
			name:    "missing_session",
			payload: `{"id":"damaged","status":"running"}`,
		},
		{
			name:    "unsupported_status",
			payload: `{"id":"damaged","session_id":"` + string(sessionID) + `","status":"unknown"}`,
		},
		{
			name:    "invalid_json_shape",
			payload: `[]`,
		},
	}
	// 2. 绕过 DAO 写入载荷，检查单条读取和分页读取均报错。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, _, records := openDAO(t)
			record := storage.Record{
				Key:       storage.RecordKey(string(sessionID) + "/damaged"),
				UpdatedAt: 1,
				Payload:   json.RawMessage(tt.payload),
			}
			if err := records.Set(t.Context(), "turns", record); err != nil {
				t.Fatal(err)
			}
			if _, _, err := query.GetTurn(t.Context(), sessionID, "damaged"); err == nil {
				t.Fatal("GetTurn(damaged) error = nil")
			}
			if _, err := query.ListTurns(t.Context(), dao.ListTurnsRequest{
				SessionID: sessionID,
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Limit:     10,
			}); err == nil {
				t.Fatal("ListTurns(damaged) error = nil")
			}
		})
	}
}

// TestTurnItemsRange 检查 Turn 状态与 Item 序号范围。
func TestTurnItemsRange(t *testing.T) {
	// 1. 逐项保存空范围、运行中范围和各终态范围，检查无效范围报错、有效范围回读不变。
	for _, tt := range []struct {
		name        string
		status      defs.TurnStatus
		first, last uint64
		invalid     bool
	}{
		{
			"empty_running",
			defs.TurnStatusRunning,
			0,
			0,
			false,
		},
		{
			"empty_completed",
			defs.TurnStatusCompleted,
			0,
			0,
			false,
		},
		{
			"open",
			defs.TurnStatusRunning,
			101,
			0,
			false,
		},
		{
			"completed",
			defs.TurnStatusCompleted,
			101,
			135,
			false,
		},
		{
			"failed",
			defs.TurnStatusFailed,
			101,
			135,
			false,
		},
		{
			"interrupted",
			defs.TurnStatusInterrupted,
			101,
			101,
			false,
		},
		{
			"missing_first",
			defs.TurnStatusCompleted,
			0,
			135,
			true,
		},
		{
			"closed_running",
			defs.TurnStatusRunning,
			101,
			135,
			true,
		},
		{
			"missing_last",
			defs.TurnStatusFailed,
			101,
			0,
			true,
		},
		{
			"reversed",
			defs.TurnStatusInterrupted,
			101,
			100,
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			record := defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: defs.NewConversationID(),
				SessionID:      defs.NewSessionID(),
				Status:         tt.status,
				ItemsRange: defs.TurnItemRange{
					FirstOrdinal: tt.first,
					LastOrdinal:  tt.last,
				},
			}
			err := mutation.SetTurn(t.Context(), record)
			if (err != nil) != tt.invalid {
				t.Fatalf("SetTurn = %v", err)
			}
			if tt.invalid {
				return
			}
			got, found, err := query.GetTurn(t.Context(), record.SessionID, record.ID)
			if err != nil || !found || got.ItemsRange != record.ItemsRange {
				t.Fatalf("GetTurn = %+v %v", got, err)
			}
			page, err := query.ListTurns(t.Context(), dao.ListTurnsRequest{
				SessionID: record.SessionID,
				Limit:     1,
			})
			if err != nil || len(page.Items) != 1 || page.Items[0].ItemsRange != record.ItemsRange {
				t.Fatalf("ListTurns = %+v %v", page, err)
			}
		})
	}
}
