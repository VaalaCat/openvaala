package dao_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

// TestSessionQueryAndMutationSetGet 检查 Session 创建、覆盖和缺失记录。
func TestSessionQueryAndMutationSetGet(t *testing.T) {
	// 1. 保存并回读 Session，检查存储分配的时间。
	query, mutation, _ := openDAO(t)
	session := defs.Session{
		ID:             defs.NewSessionID(),
		ConversationID: "test-conversation",
		AgentName:      "test",
		TaskName:       "first",
	}
	if err := mutation.SetSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	got, found, err := query.GetSession(t.Context(), "test-conversation", session.ID)
	session.UpdatedAt = got.UpdatedAt
	if err != nil || !found || got.UpdatedAt == 0 || got != session {
		t.Fatalf("GetSession() = %+v, %v, %v, want %+v", got, found, err, session)
	}
	// 2. 覆盖任务名并回读，再检查缺失记录和空 ID 错误。
	session.TaskName = "replaced"
	if err := mutation.SetSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	got, found, err = query.GetSession(t.Context(), "test-conversation", session.ID)
	session.UpdatedAt = got.UpdatedAt
	if err != nil || !found || got.UpdatedAt == 0 || got != session {
		t.Fatalf("GetSession() after replacement = %+v, %v, %v, want %+v", got, found, err, session)
	}
	if got, found, err := query.GetSession(t.Context(), "test-conversation", "missing"); err != nil || found || got != (defs.Session{}) {
		t.Fatalf("GetSession(missing) = %+v, %v, %v", got, found, err)
	}
	if err := mutation.SetSession(t.Context(), defs.Session{TaskName: "missing ID"}); err == nil {
		t.Fatal("SetSession() accepted an empty ID")
	}
}

// TestSessionQueryGetSessionByID 检查仅凭 Session ID 查找记录。
func TestSessionQueryGetSessionByID(t *testing.T) {
	// 1. 准备两个已写 Item 的对话和一个尚未写 Item 的根 Session。
	query, mutation, _ := openDAO(t)
	// 两个对话各有根 Session 和一个子 Session，都写过 Item；第三个对话只建了根 Session 记录，还没收到第一条输入，没有 Item。
	var children []defs.Session
	for _, conversationID := range []defs.ConversationID{defs.NewConversationID(), defs.NewConversationID()} {
		root := defs.Session{
			ID:             defs.NewSessionID(),
			ConversationID: conversationID,
			AgentName:      "root",
		}
		child := defs.Session{
			ID:              defs.NewSessionID(),
			ConversationID:  conversationID,
			AgentName:       "child",
			TaskName:        "review",
			ParentSessionID: root.ID,
		}
		for _, session := range []defs.Session{root, child} {
			if err := mutation.SetSession(t.Context(), session); err != nil {
				t.Fatal(err)
			}
			if _, err := mutation.AppendItems(t.Context(), conversationID, session.ID, &defs.Item{ID: defs.NewItemID()}); err != nil {
				t.Fatal(err)
			}
		}
		children = append(children, child)
	}
	rootBeforeInput := defs.Session{
		ID:             defs.NewSessionID(),
		ConversationID: defs.NewConversationID(),
		AgentName:      "root",
	}
	if err := mutation.SetSession(t.Context(), rootBeforeInput); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		sessionID defs.SessionID
		want      defs.Session
		wantFound bool
	}{
		{
			name:      "child_in_first_conversation",
			sessionID: children[0].ID,
			want:      children[0],
			wantFound: true,
		},
		{
			name:      "child_in_second_conversation",
			sessionID: children[1].ID,
			want:      children[1],
			wantFound: true,
		},
		{
			name:      "missing",
			sessionID: "missing",
		},
		{
			name:      "root_session_before_first_input",
			sessionID: rootBeforeInput.ID,
		},
	}
	// 2. 检查找到两个子 Session，缺失和未写 Item 的 Session 返回未找到。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := query.GetSessionByID(t.Context(), tt.sessionID)
			if tt.wantFound {
				tt.want.UpdatedAt = got.UpdatedAt
			}
			if err != nil || found != tt.wantFound || got != tt.want || (found && got.UpdatedAt == 0) {
				t.Fatalf("GetSessionByID(%s) = %+v, %v, %v, want %+v, %v", tt.sessionID, got, found, err, tt.want, tt.wantFound)
			}
		})
	}
}

// TestSessionQueryList 检查 Session 列表的排序和分页。
func TestSessionQueryList(t *testing.T) {
	// 1. 准备 Key 与写入时间升降序样例。
	tests := []struct {
		name    string
		order   storage.Order
		wantIDs []int
	}{
		{
			name:    "ascending",
			order:   storage.Order{Direction: storage.DirectionAsc},
			wantIDs: []int{0, 1, 2},
		},
		{
			name:    "descending",
			order:   storage.Order{Direction: storage.DirectionDesc},
			wantIDs: []int{2, 1, 0},
		},
		{
			name: "updated_at_ascending",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionAsc,
			},
			wantIDs: []int{1, 2, 0},
		},
		{
			name: "updated_at_descending",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionDesc,
			},
			wantIDs: []int{0, 2, 1},
		},
	}
	// 2. 写入并更新本对话 Session，再写入其他对话，检查两页结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, _ := openDAO(t)
			sessions := []defs.Session{
				{
					ID:             defs.NewSessionID(),
					ConversationID: "test-conversation",
					AgentName:      "test",
					TaskName:       "one",
				},
				{
					ID:             defs.NewSessionID(),
					ConversationID: "test-conversation",
					AgentName:      "test",
					TaskName:       "two",
				},
				{
					ID:             defs.NewSessionID(),
					ConversationID: "test-conversation",
					AgentName:      "test",
					TaskName:       "three",
				},
			}
			for _, session := range sessions {
				if err := mutation.SetSession(t.Context(), session); err != nil {
					t.Fatal(err)
				}
			}
			// 重写 one 使其最近活跃；随后写入的其他对话 Session 更新，但不应出现在本对话任何一页。
			waitNextMillisecond()
			sessions[0].TaskName = "one again"
			for _, session := range []defs.Session{sessions[0], {
				ID:             defs.NewSessionID(),
				ConversationID: "other-conversation",
				AgentName:      "other",
			}} {
				if err := mutation.SetSession(t.Context(), session); err != nil {
					t.Fatal(err)
				}
			}

			page, err := query.ListSessions(t.Context(), dao.ListSessionsRequest{
				ConversationID: "test-conversation",
				Order:          tt.order,
				Limit:          2,
			})
			wantNext := dao.RecordCursor[defs.SessionID]{ID: sessions[tt.wantIDs[1]].ID}
			if len(page.Items) == 2 {
				wantNext.UpdatedAt = page.Items[1].UpdatedAt
			}
			if err != nil || page.Next != wantNext || !slices.Equal(getSessionIDs(page.Items), getSessionIDsAt(sessions, tt.wantIDs[:2])) {
				t.Fatalf("ListSessions() = %+v, %v, want sessions %v", page, err, tt.wantIDs[:2])
			}
			last, err := query.ListSessions(t.Context(), dao.ListSessionsRequest{
				ConversationID: "test-conversation",
				Order:          tt.order,
				Cursor:         page.Next,
				Limit:          2,
			})
			if err != nil || last.Next != (dao.RecordCursor[defs.SessionID]{}) || !slices.Equal(getSessionIDs(last.Items), getSessionIDsAt(sessions, tt.wantIDs[2:])) {
				t.Fatalf("ListSessions() final page = %+v, %v", last, err)
			}
		})
	}

	query, _, _ := openDAO(t)
	empty, err := query.ListSessions(t.Context(), dao.ListSessionsRequest{
		ConversationID: "test-conversation",
		Order:          storage.Order{Direction: storage.DirectionAsc},
		Limit:          2,
	})
	if err != nil || empty.Next != (dao.RecordCursor[defs.SessionID]{}) || len(empty.Items) != 0 {
		t.Fatalf("ListSessions() empty = %+v, %v", empty, err)
	}
	if _, err := query.ListSessions(t.Context(), dao.ListSessionsRequest{
		ConversationID: "test-conversation",
		Order:          storage.Order{Direction: "unsupported"},
		Limit:          2,
	}); err == nil {
		t.Fatal("ListSessions() accepted an unsupported order")
	}
}

// TestSessionPersistsAcrossReopen 检查 Session 重新打开数据库后仍可读取。
func TestSessionPersistsAcrossReopen(t *testing.T) {
	// 1. 创建落盘存储及可重复打开的 DAO。
	root := t.TempDir()
	databasePath := filepath.Join(root, "records.db")
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
	if err != nil {
		t.Fatal(err)
	}
	openSessionDAO := func() (storagesqlite.Database, dao.Query, dao.Mutation) {
		database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(databasePath, dao.RecordNamespaces()...))
		if err != nil {
			t.Fatal(err)
		}
		query, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
		if err != nil {
			t.Fatal(err)
		}
		return database, query, mutation
	}
	// 2. 写入 Session 并关闭数据库。
	database, _, mutation := openSessionDAO()
	want := defs.Session{
		ID:             defs.NewSessionID(),
		ConversationID: "test-conversation",
		AgentName:      "test",
		TaskName:       "persistent",
	}
	if err := mutation.SetSession(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. 重新打开，检查字段和非零写入时间。
	database, query, _ := openSessionDAO()
	t.Cleanup(func() { _ = database.Close() })
	got, found, err := query.GetSession(t.Context(), "test-conversation", want.ID)
	want.UpdatedAt = got.UpdatedAt
	if err != nil || !found || got.UpdatedAt == 0 || got != want {
		t.Fatalf("GetSession() after reopen = %+v, %v, %v, want %+v", got, found, err, want)
	}
}

// TestSessionQueryScanSessions 检查 Session 的跨页迭代及提前停止。
func TestSessionQueryScanSessions(t *testing.T) {
	// 1. 准备 300 条 Session 和升降序、提前停止场景。
	const sessionCount = 300
	tests := []struct {
		name      string
		order     storage.Order
		stopAfter int
		wantCount int
	}{
		{
			name:      "ascending_all_pages",
			order:     storage.Order{Direction: storage.DirectionAsc},
			wantCount: sessionCount,
		},
		{
			name:      "descending_all_pages",
			order:     storage.Order{Direction: storage.DirectionDesc},
			wantCount: sessionCount,
		},
		{
			name:      "break_early",
			order:     storage.Order{Direction: storage.DirectionAsc},
			stopAfter: 3,
			wantCount: 3,
		},
	}
	query, mutation, _ := openDAO(t)
	conversationID := defs.NewConversationID()
	wantIDs := make([]defs.SessionID, sessionCount)
	for index := range wantIDs {
		wantIDs[index] = defs.NewSessionID()
		if err := mutation.SetSession(t.Context(), defs.Session{
			ID:             wantIDs[index],
			ConversationID: conversationID,
			AgentName:      "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mutation.SetSession(t.Context(), defs.Session{
		ID:             defs.NewSessionID(),
		ConversationID: defs.NewConversationID(),
		AgentName:      "other",
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(wantIDs)
	// 2. 检查迭代顺序及数量。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotIDs []defs.SessionID
			for value, err := range query.ScanSessions(t.Context(), conversationID, tt.order) {
				if err != nil {
					t.Fatal(err)
				}
				gotIDs = append(gotIDs, value.ID)
				if len(gotIDs) == tt.stopAfter {
					break
				}
			}
			want := slices.Clone(wantIDs)
			if tt.order.Direction == storage.DirectionDesc {
				slices.Reverse(want)
			}
			if !slices.Equal(gotIDs, want[:tt.wantCount]) {
				t.Fatalf("ScanSessions() returned %d IDs, want first %d in %s order", len(gotIDs), tt.wantCount, tt.order.Direction)
			}
		})
	}

	t.Run("damaged_record", func(t *testing.T) {
		query, _, records := openDAO(t)
		if err := records.Set(t.Context(), "sessions", storage.Record{
			Key:       "damaged-conversation/s",
			UpdatedAt: 1,
			Payload:   json.RawMessage(`{"id":"other","conversation_id":"damaged-conversation","agent_name":"test"}`),
		}); err != nil {
			t.Fatal(err)
		}
		for _, err := range query.ScanSessions(t.Context(), "damaged-conversation", storage.Order{Direction: storage.DirectionAsc}) {
			if err == nil {
				t.Fatal("ScanSessions() accepted damaged Session")
			}
		}
	})
}

// TestSessionMutationDeleteSession 检查删除 Session 的日志和各级索引。
func TestSessionMutationDeleteSession(t *testing.T) {
	// 1. 分别删除匹配对话、不匹配对话和重复删除，检查 Session、Turn、Item 与最后一条 metadata。
	for _, tt := range []struct {
		name    string
		scope   defs.ConversationID
		repeats int
		remains bool
		// wantLastKind 是删除之后 metadata 最后一行的种类；对话不匹配时没有删除行。
		wantLastKind string
	}{
		{
			"matching_conversation",
			"test-conversation",
			1,
			false,
			"session_deleted",
		},
		{
			"wrong_conversation",
			"other-conversation",
			1,
			true,
			"turn_saved",
		},
		{
			"repeated_delete",
			"test-conversation",
			2,
			false,
			"session_deleted",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			if err := mutation.DeleteSession(t.Context(), "test-conversation", ""); err == nil {
				t.Fatal("DeleteSession() accepted an empty ID")
			}
			session := defs.Session{
				ID:             defs.NewSessionID(),
				ConversationID: "test-conversation",
				AgentName:      "test",
				TaskName:       "delete me",
			}
			turnRecord := defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: session.ConversationID,
				SessionID:      session.ID,
				Status:         defs.TurnStatusCompleted,
			}
			if err := mutation.SetSession(t.Context(), session); err != nil {
				t.Fatal(err)
			}
			if err := mutation.SetTurn(t.Context(), turnRecord); err != nil {
				t.Fatal(err)
			}
			if _, err := mutation.AppendItems(t.Context(), session.ConversationID, session.ID, &defs.Item{ID: defs.NewItemID()}, &defs.Item{
				ID:          "summary",
				TurnID:      "turn",
				ModelCallID: "compact",
				Kind:        defs.ItemContextSummary,
				ContextSummary: &defs.ContextSummary{
					Text:                 "summary",
					CoveredThroughItemID: "source",
				},
			}); err != nil {
				t.Fatal(err)
			}

			for range tt.repeats {
				if err := mutation.DeleteSession(t.Context(), tt.scope, session.ID); err != nil {
					t.Fatal(err)
				}
			}
			_, sessionFound, err := query.GetSession(t.Context(), "test-conversation", session.ID)
			if err != nil {
				t.Fatal(err)
			}
			turnPage, err := query.ListTurns(t.Context(), dao.ListTurnsRequest{
				SessionID: session.ID,
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Limit:     10,
			})
			if err != nil {
				t.Fatal(err)
			}
			itemPage, err := query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: session.ConversationID,
				SessionID:      session.ID,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err != nil {
				t.Fatal(err)
			}
			wantTurns, wantItems := 0, 0
			if tt.remains {
				wantTurns, wantItems = 1, 2
			}
			if sessionFound != tt.remains || len(turnPage.Items) != wantTurns || len(itemPage.Items) != wantItems {
				t.Fatalf("DeleteSession() left Session=%v Turns=%+v Items=%+v", sessionFound, turnPage.Items, itemPage.Items)
			}
			lines := readMetadata(t, logs, session.ConversationID)
			if len(lines) == 0 {
				t.Fatal("metadata has no lines")
			}
			last := lines[len(lines)-1]
			if last.Kind != tt.wantLastKind {
				t.Fatalf("last metadata kind = %q, want %q (all kinds %v)", last.Kind, tt.wantLastKind, getMetadataKinds(lines))
			}
			if !tt.remains && !strings.Contains(string(last.Payload), `"deleted_session_id":"`+string(session.ID)+`"`) {
				t.Fatalf("session_deleted payload = %s, want deleted_session_id %s", last.Payload, session.ID)
			}
		})
	}
}

// TestSessionMutationDeleteFailureKeepsSession 检查删除中途失败后保留 Session。
func TestSessionMutationDeleteFailureKeepsSession(t *testing.T) {
	// 1. 准备 Turn 索引和 Item 日志的删除错误。
	tests := []struct {
		name             string
		failTurnDeletion bool
		failItemDeletion bool
	}{
		{
			name:             "turn_deletion",
			failTurnDeletion: true,
		},
		{
			name:             "item_deletion",
			failItemDeletion: true,
		},
	}
	// 2. 保存 Session、Turn、Item 后删除，检查错误及对应剩余数据。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			if tt.failTurnDeletion {
				records = &failingDeleteRangeStorage{RecordStorage: records}
			}
			if tt.failItemDeletion {
				logs = &failingDeleteLogStorage{LogStorage: logs}
			}
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			session := defs.Session{
				ID:             defs.NewSessionID(),
				ConversationID: "test-conversation",
				AgentName:      "test",
				TaskName:       "keep me",
			}
			if err := mutation.SetSession(t.Context(), session); err != nil {
				t.Fatal(err)
			}
			if err := mutation.SetTurn(t.Context(), defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: session.ConversationID,
				SessionID:      session.ID,
				Status:         defs.TurnStatusCompleted,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := mutation.AppendItems(t.Context(), session.ConversationID, session.ID, &defs.Item{ID: defs.NewItemID()}); err != nil {
				t.Fatal(err)
			}
			if err := mutation.DeleteSession(t.Context(), "test-conversation", session.ID); err == nil {
				t.Fatal("DeleteSession() error = nil")
			}
			got, found, err := query.GetSession(t.Context(), "test-conversation", session.ID)
			session.UpdatedAt = got.UpdatedAt
			if err != nil || !found || got.UpdatedAt == 0 || got != session {
				t.Fatalf("GetSession() after failed delete = %+v, %v, %v", got, found, err)
			}
			turns, err := query.ListTurns(t.Context(), dao.ListTurnsRequest{
				SessionID: session.ID,
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Limit:     10,
			})
			wantTurns := 0
			if tt.failTurnDeletion {
				wantTurns = 1
			}
			if err != nil || len(turns.Items) != wantTurns {
				t.Fatalf("Turns after failed delete = %+v, %v, want %d", turns.Items, err, wantTurns)
			}
			items, err := query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: session.ConversationID,
				SessionID:      session.ID,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err != nil || len(items.Items) != 1 {
				t.Fatalf("Items after failed delete = %+v, %v, want preserved", items.Items, err)
			}
		})
	}
}

// failingDeleteRangeStorage 包装记录存储，DeleteRange 总是失败。
type failingDeleteRangeStorage struct {
	storage.RecordStorage
}

// DeleteRange 返回注入的范围删除错误。
func (*failingDeleteRangeStorage) DeleteRange(context.Context, string, storage.RecordKey, storage.RecordKey) error {
	// 1. 返回注入的范围删除错误。
	return errors.New("injected DeleteRange failure")
}

// failingDeleteLogStorage 包装日志存储，Delete 总是失败。
type failingDeleteLogStorage struct {
	storage.LogStorage
}

// Delete 返回注入的日志删除错误。
func (*failingDeleteLogStorage) Delete(context.Context, storage.StreamPath, storage.StreamID) error {
	// 1. 返回注入的日志删除错误。
	return errors.New("injected Log Delete failure")
}

// getSessionIDs 提取 Session 列表中的 ID。
func getSessionIDs(sessions []defs.Session) []defs.SessionID {
	// 1. 按输入顺序收集 Session ID。
	ids := make([]defs.SessionID, len(sessions))
	for index, session := range sessions {
		ids[index] = session.ID
	}
	return ids
}

// getSessionIDsAt 按给定下标提取 Session ID。
func getSessionIDsAt(sessions []defs.Session, indexes []int) []defs.SessionID {
	// 1. 按下标序列读取对应 Session 的 ID。
	ids := make([]defs.SessionID, len(indexes))
	for position, index := range indexes {
		ids[position] = sessions[index].ID
	}
	return ids
}

// waitNextMillisecond 等到系统时间进入下一毫秒。
func waitNextMillisecond() {
	// 1. 记录起始毫秒，时间未变化时短暂休眠后继续检查。
	start := time.Now().UnixMilli()
	for time.Now().UnixMilli() == start {
		time.Sleep(100 * time.Microsecond)
	}
}
