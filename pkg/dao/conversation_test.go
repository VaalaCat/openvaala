package dao_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestConversationStorage 检查对话保存、读取和分页。
func TestConversationStorage(t *testing.T) {
	// 1. 准备按 Key 升降序和写入时间倒序的样例。
	tests := []struct {
		name  string
		order storage.Order
	}{
		{
			name:  "key_asc",
			order: storage.Order{Direction: storage.DirectionAsc},
		},
		{
			name:  "key_desc",
			order: storage.Order{Direction: storage.DirectionDesc},
		},
		{
			name: "updated_at_desc",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionDesc,
			},
		},
	}
	// 2. 保存对话及根 Session，检查存储时间、字段和逐页结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, m, _ := openDAO(t)
			records := []defs.Conversation{
				{
					ID:            defs.NewConversationID(),
					RootSessionID: defs.NewSessionID(),
					Title:         "first",
				},
				{
					ID:            defs.NewConversationID(),
					RootSessionID: defs.NewSessionID(),
					Title:         "second",
					MessageCount:  3,
				},
			}
			for _, record := range records {
				record.UpdatedAt = 42 // 业务设置的时间不会被保存
				if err := m.SetSession(t.Context(), defs.Session{
					ID:             record.RootSessionID,
					ConversationID: record.ID,
					AgentName:      "test",
				}); err != nil {
					t.Fatal(err)
				}
				before := time.Now().UnixMilli()
				if err := m.SetConversation(t.Context(), record); err != nil {
					t.Fatal(err)
				}
				got, found, err := q.GetConversation(t.Context(), record.ID)
				if err != nil || !found || got.UpdatedAt < before || got.Title != record.Title || got.MessageCount != record.MessageCount {
					t.Fatalf("get=%+v %v", got, err)
				}
			}
			page, err := q.ListConversations(t.Context(), dao.ListConversationsRequest{
				Order: tt.order,
				Limit: 1,
			})
			if err != nil || page.Next.ID != page.Items[0].Conversation.ID || page.Next.UpdatedAt != page.Items[0].Conversation.UpdatedAt {
				t.Fatalf("page=%+v %v", page, err)
			}
			next, err := q.ListConversations(t.Context(), dao.ListConversationsRequest{
				Order:  tt.order,
				Cursor: page.Next,
				Limit:  1,
			})
			if err != nil || next.Next != (dao.RecordCursor[defs.ConversationID]{}) || next.Items[0].Conversation.ID == page.Items[0].Conversation.ID {
				t.Fatalf("next=%+v %v", next, err)
			}
		})
	}
}

// TestConversationListCursorWithoutIDStartsFromBeginning 检查未带 ID 的对话游标从起点读取。
func TestConversationListCursorWithoutIDStartsFromBeginning(t *testing.T) {
	// 1. 准备不同排序条件。
	tests := []struct {
		name  string
		order storage.Order
	}{
		{
			name:  "key_asc",
			order: storage.Order{Direction: storage.DirectionAsc},
		},
		{
			name: "updated_at_asc",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionAsc,
			},
		},
		{
			name: "updated_at_desc",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionDesc,
			},
		},
	}
	// 2. 保存一条对话，仅传游标时间并检查仍能读到这条对话。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, m, _ := openDAO(t)
			record := defs.Conversation{
				ID:            defs.NewConversationID(),
				RootSessionID: defs.NewSessionID(),
			}
			if err := m.SetSession(t.Context(), defs.Session{
				ID:             record.RootSessionID,
				ConversationID: record.ID,
				AgentName:      "test",
			}); err != nil {
				t.Fatal(err)
			}
			if err := m.SetConversation(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			page, err := q.ListConversations(t.Context(), dao.ListConversationsRequest{
				Order:  tt.order,
				Cursor: dao.RecordCursor[defs.ConversationID]{UpdatedAt: 123},
				Limit:  10,
			})
			if err != nil || len(page.Items) != 1 || page.Items[0].Conversation.ID != record.ID {
				t.Fatalf("ListConversations(cursor without ID) = %+v, %v, want the only Conversation", page, err)
			}
		})
	}
}

// TestSessionConversationScope 检查相同 Session ID 在不同对话中分别保存。
func TestSessionConversationScope(t *testing.T) {
	// 1. 在两个对话中保存相同 Session ID 和不同 Agent 名称。
	q, m, _ := openDAO(t)
	first, second := defs.NewConversationID(), defs.NewConversationID()
	sid := defs.NewSessionID()
	for _, id := range []defs.ConversationID{first, second} {
		if err := m.SetSession(t.Context(), defs.Session{
			ID:             sid,
			ConversationID: id,
			AgentName:      string(id),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 2. 分别列出 Session，检查只返回对应对话的记录。
	for _, id := range []defs.ConversationID{first, second} {
		page, err := q.ListSessions(t.Context(), dao.ListSessionsRequest{
			ConversationID: id,
			Order:          storage.Order{Direction: storage.DirectionAsc},
			Limit:          10,
		})
		if err != nil || len(page.Items) != 1 || page.Items[0].AgentName != string(id) {
			t.Fatalf("scope=%+v %v", page, err)
		}
	}
}

// TestArchiveConversation 检查对话归档和取消归档。
func TestArchiveConversation(t *testing.T) {
	// 1. 准备归档、取消及再次归档的时间序列。
	archivedAt := time.Now().UnixMilli()
	tests := []struct {
		name string
		// archivedAts 是依次保存的 ArchivedAt，0 表示没归档。
		archivedAts  []int64
		wantArchived bool
	}{
		{
			name:         "archive",
			archivedAts:  []int64{0, archivedAt},
			wantArchived: true,
		},
		{
			name:        "unarchive",
			archivedAts: []int64{archivedAt, 0},
		},
		{
			name:         "archive_again",
			archivedAts:  []int64{archivedAt, archivedAt + 1},
			wantArchived: true,
		},
	}
	// 2. 依次保存并检查读取结果、两个列表和对应记录分组。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, m, records := openDAO(t)
			conversation := defs.Conversation{
				ID:            defs.NewConversationID(),
				RootSessionID: defs.NewSessionID(),
				Title:         "归档",
			}
			if err := m.SetSession(t.Context(), defs.Session{
				ID:             conversation.RootSessionID,
				ConversationID: conversation.ID,
				AgentName:      "test",
			}); err != nil {
				t.Fatal(err)
			}
			for _, value := range tt.archivedAts {
				conversation.ArchivedAt = value
				if err := m.SetConversation(t.Context(), conversation); err != nil {
					t.Fatal(err)
				}
			}
			got, found, err := q.GetConversation(t.Context(), conversation.ID)
			if err != nil || !found || got.ArchivedAt != conversation.ArchivedAt || got.Title != conversation.Title {
				t.Fatalf("GetConversation() = %+v, %v, %v, want ArchivedAt %d", got, found, err, conversation.ArchivedAt)
			}
			for _, isArchivedList := range []bool{false, true} {
				page, err := q.ListConversations(t.Context(), dao.ListConversationsRequest{
					Limit:    10,
					Archived: isArchivedList,
				})
				if err != nil {
					t.Fatal(err)
				}
				isListed := slices.ContainsFunc(page.Items, func(value dao.ConversationListEntry) bool { return value.Conversation.ID == conversation.ID })
				if isListed != (isArchivedList == tt.wantArchived) {
					t.Fatalf("ListConversations(Archived: %v) listed=%v, want %v", isArchivedList, isListed, isArchivedList == tt.wantArchived)
				}
			}
			wantNamespace := dao.ConversationNamespace
			if tt.wantArchived {
				wantNamespace = dao.ArchivedConversationNamespace
			}
			for _, namespace := range []string{dao.ConversationNamespace, dao.ArchivedConversationNamespace} {
				_, isStored, err := records.Get(t.Context(), namespace, storage.RecordKey(conversation.ID))
				if err != nil || isStored != (namespace == wantNamespace) {
					t.Fatalf("records.Get(%s) stored=%v err=%v, want stored only in %s", namespace, isStored, err, wantNamespace)
				}
			}
		})
	}
}

// TestDeleteConversation 检查对话及所属数据删除。
func TestDeleteConversation(t *testing.T) {
	// 1. 准备普通、已归档和缺失对话场景。
	tests := []struct {
		name       string
		archivedAt int64
		// isMissing 是否删除一个不存在的对话？是时两个已有对话都不受影响。
		isMissing bool
	}{
		{name: "conversation"},
		{
			name:       "archived_conversation",
			archivedAt: time.Now().UnixMilli(),
		},
		{
			name:      "missing_conversation",
			isMissing: true,
		},
	}
	// 2. 写入两个对话，删除目标后检查其索引日志和另一对话。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			target, targetSessions := writeConversationTree(t, mutation, tt.archivedAt)
			kept, keptSessions := writeConversationTree(t, mutation, 0)
			deletedID := target.ID
			if tt.isMissing {
				deletedID = defs.NewConversationID()
			}
			if err := mutation.DeleteConversation(t.Context(), deletedID); err != nil {
				t.Fatalf("DeleteConversation() = %v", err)
			}
			requireConversationTree(t, query, logs, target.ID, targetSessions, tt.isMissing)
			requireConversationTree(t, query, logs, kept.ID, keptSessions, true)
			// 删除后不留下被删对话的 metadata 目录，删除不存在的对话也不会建出目录。
			paths, err := logs.GetStreamPaths(t.Context(), dao.MetadataStreamID)
			if err != nil || slices.Contains(paths, storage.StreamPath(deletedID)) {
				t.Fatalf("metadata paths = %v, %v, want no %s", paths, err, deletedID)
			}
		})
	}
}

// writeConversationTree 写入带根 Session 和子 Session 的测试对话。
func writeConversationTree(t *testing.T, mutation dao.Mutation, archivedAt int64) (defs.Conversation, []defs.SessionID) {
	t.Helper()
	// 1. 创建对话、根 Session 和子 Session。
	ctx := t.Context()
	conversation := defs.Conversation{
		ID:            defs.NewConversationID(),
		RootSessionID: defs.NewSessionID(),
		ArchivedAt:    archivedAt,
	}
	root := defs.Session{
		ID:             conversation.RootSessionID,
		ConversationID: conversation.ID,
		AgentName:      "assistant",
	}
	child := defs.Session{
		ID:              defs.NewSessionID(),
		ConversationID:  conversation.ID,
		AgentName:       "assistant",
		TaskName:        "child",
		ParentSessionID: root.ID,
		ParentTurnID:    defs.NewTurnID(),
		ParentCallID:    "call-child",
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// 2. 保存对话及两个 Session。
	must(mutation.SetSession(ctx, root))
	must(mutation.SetConversation(ctx, conversation))
	must(mutation.SetSession(ctx, child))
	// 3. 每个 Session 写入一条 Item 和两条 Turn，返回对话及 Session ID。
	for _, session := range []defs.Session{root, child} {
		if _, err := mutation.AppendItems(ctx, conversation.ID, session.ID, &defs.Item{ID: defs.NewItemID()}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			must(mutation.SetTurn(ctx, defs.Turn{
				ID:             defs.NewTurnID(),
				ConversationID: conversation.ID,
				SessionID:      session.ID,
				Status:         defs.TurnStatusCompleted,
			}))
		}
	}
	return conversation, []defs.SessionID{root.ID, child.ID}
}

// requireConversationTree 检查对话树的索引和日志是否完整保留或删除。
func requireConversationTree(t *testing.T, query dao.Query, logs storage.LogStorage, id defs.ConversationID, sessionIDs []defs.SessionID, isKept bool) {
	t.Helper()
	// 1. 按预期保留状态检查对话和 Session 数量。
	ctx := t.Context()
	wantSessions, wantTurns, wantItems := 0, 0, 0
	if isKept {
		wantSessions, wantTurns, wantItems = len(sessionIDs), 2, 1
	}
	if _, found, err := query.GetConversation(ctx, id); err != nil || found != isKept {
		t.Fatalf("GetConversation(%s) found=%v err=%v, want %v", id, found, err, isKept)
	}
	sessions, err := query.ListSessions(ctx, dao.ListSessionsRequest{
		ConversationID: id,
		Limit:          10,
	})
	if err != nil || len(sessions.Items) != wantSessions {
		t.Fatalf("ListSessions(%s) = %+v, %v, want %d", id, sessions.Items, err, wantSessions)
	}
	// 2. 逐个检查 Session 的 Turn 和 Item 数量。
	for _, sessionID := range sessionIDs {
		turns, err := query.ListTurns(ctx, dao.ListTurnsRequest{
			SessionID: sessionID,
			Limit:     10,
		})
		if err != nil || len(turns.Items) != wantTurns {
			t.Fatalf("ListTurns(%s) = %+v, %v, want %d", sessionID, turns.Items, err, wantTurns)
		}
		items, err := query.ListItems(ctx, dao.ListItemsRequest{
			ConversationID: id,
			SessionID:      sessionID,
			Limit:          10,
		})
		if err != nil || len(items.Items) != wantItems {
			t.Fatalf("ListItems(%s) = %+v, %v, want %d", sessionID, items.Items, err, wantItems)
		}
	}
	// 3. 检查对话的 metadata 日志目录。
	paths, err := logs.GetStreamPaths(ctx, dao.MetadataStreamID)
	if err != nil || slices.Contains(paths, storage.StreamPath(id)) != isKept {
		t.Fatalf("metadata paths = %v, %v, want %s present=%v", paths, err, id, isKept)
	}
}

// TestConversationListFilters 检查对话筛选、分页及根 Session 批量读取。
func TestConversationListFilters(t *testing.T) {
	// 1. 准备目录、标题、归档和目录修改场景。
	tests := []struct {
		name        string
		directories []string
		searchTerm  string
		archived    bool
		moveFirst   bool
		wantIDs     []defs.ConversationID
	}{
		{
			name:    "unfiltered",
			wantIDs: []defs.ConversationID{"a", "b", "c", "d", "e"},
		},
		{
			name:        "directory_exact",
			directories: []string{"/project"},
			wantIDs:     []defs.ConversationID{"a", "d", "e"},
		},
		{
			name:        "multiple_directories",
			directories: []string{"/project", "/other"},
			wantIDs:     []defs.ConversationID{"a", "b", "d", "e"},
		},
		{
			name:       "title_case_sensitive",
			searchTerm: "bug",
			wantIDs:    []defs.ConversationID{"a", "b", "c", "e"},
		},
		{
			name:        "directory_and_title",
			directories: []string{"/project"},
			searchTerm:  "bug",
			wantIDs:     []defs.ConversationID{"a", "e"},
		},
		{
			name:       "literal_search",
			searchTerm: "%_",
			wantIDs:    []defs.ConversationID{"e"},
		},
		{
			name:        "archived",
			directories: []string{"/project"},
			searchTerm:  "bug",
			archived:    true,
			wantIDs:     []defs.ConversationID{"f"},
		},
		{
			name:        "no_matches",
			directories: []string{"/missing"},
		},
		{
			name:        "changed_directory",
			directories: []string{"/project"},
			moveFirst:   true,
			wantIDs:     []defs.ConversationID{"d", "e"},
		},
	}
	// 2. 写入对话及其根 Session、子 Session，逐页检查结果、列表读取次数和根 Session 批量读取的 Key。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			counted := &conversationListRecords{RecordStorage: records}
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(counted, logs))
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range []struct {
				id        defs.ConversationID
				directory string
				title     string
				archived  int64
			}{
				{
					"a",
					"/project",
					"fix bug",
					0,
				},
				{
					"b",
					"/other",
					"fix bug",
					0,
				},
				{
					"c",
					"/project/sub",
					"fix bug",
					0,
				},
				{
					"d",
					"/project",
					"fix BUG",
					0,
				},
				{
					"e",
					"/project",
					"bug %_",
					0,
				},
				{
					"f",
					"/project",
					"archived bug",
					1,
				},
			} {
				conversation := defs.Conversation{
					ID:               row.id,
					RootSessionID:    "root",
					Title:            row.title,
					ArchivedAt:       row.archived,
					ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: row.directory}},
				}
				root := defs.Session{
					ID:             "root",
					ConversationID: row.id,
					AgentName:      "assistant",
					Model: defs.ModelSelection{
						Ref: defs.ModelRef{
							ProviderID: "test",
							ModelID:    string(row.id),
						},
						ReasoningEffort: "high",
					},
				}
				child := defs.Session{
					ID:              "child",
					ConversationID:  row.id,
					AgentName:       "assistant",
					TaskName:        "child",
					ParentSessionID: root.ID,
					ParentTurnID:    "turn",
					ParentCallID:    "spawn",
					Model: defs.ModelSelection{Ref: defs.ModelRef{
						ProviderID: "test",
						ModelID:    "child",
					}},
				}
				for _, session := range []defs.Session{root, child} {
					if err := mutation.SetSession(t.Context(), session); err != nil {
						t.Fatal(err)
					}
				}
				if err := mutation.SetConversation(t.Context(), conversation); err != nil {
					t.Fatal(err)
				}
				if row.id == "a" && tt.moveFirst {
					conversation.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID] = "/moved"
					if err := mutation.SetConversation(t.Context(), conversation); err != nil {
						t.Fatal(err)
					}
				}
			}
			request := dao.ListConversationsRequest{
				Directories: tt.directories,
				SearchTerm:  tt.searchTerm,
				Archived:    tt.archived,
				Limit:       2,
			}
			var gotIDs []defs.ConversationID
			for {
				counted.listCalls, counted.rootReads = 0, nil
				page, err := query.ListConversations(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				wantRootCalls := 0
				if len(page.Items) > 0 {
					wantRootCalls = 1
				}
				if counted.listCalls != 1 || len(counted.rootReads) != wantRootCalls {
					t.Fatalf("list calls=%d root reads=%v, want one list and %d batch reads", counted.listCalls, counted.rootReads, wantRootCalls)
				}
				var wantKeys []storage.RecordKey
				for _, entry := range page.Items {
					conversation, root := entry.Conversation, entry.RootSession
					gotIDs = append(gotIDs, conversation.ID)
					wantKeys = append(wantKeys, storage.RecordKey(string(conversation.ID)+"/root"))
					if root.ID != conversation.RootSessionID || root.ConversationID != conversation.ID || root.Model.Ref.ModelID != string(conversation.ID) || root.Model.ReasoningEffort != "high" {
						t.Fatalf("entry=%+v, want corresponding root model, not the child's", entry)
					}
				}
				if wantRootCalls == 1 && !slices.Equal(counted.rootReads[0], wantKeys) {
					t.Fatalf("root read keys=%v, want %v", counted.rootReads, wantKeys)
				}
				if page.Next.ID == "" {
					break
				}
				if page.Next.ID != gotIDs[len(gotIDs)-1] || page.Next == request.Cursor {
					t.Fatalf("next cursor=%+v after %+v", page.Next, request.Cursor)
				}
				request.Cursor = page.Next
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Fatalf("listed IDs=%v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// conversationListRecords 包装真实记录存储，统计列表读取次数、记下根 Session 批量读取的 Key，并按需注入读取错误。
type conversationListRecords struct {
	storage.RecordStorage
	listCalls int
	rootReads [][]storage.RecordKey
	listErr   error
	rootErr   error
}

// List 记录列表调用次数并注入指定读取错误。
func (r *conversationListRecords) List(ctx context.Context, request storage.ListRecordsRequest) (storage.Page[storage.Record, storage.RecordCursor], error) {
	// 1. 递增计数，已设置 listErr 时返回错误，否则调用底层列表读取。
	r.listCalls++
	if r.listErr != nil {
		return storage.Page[storage.Record, storage.RecordCursor]{}, r.listErr
	}
	return r.RecordStorage.List(ctx, request)
}

// MGet 记录根 Session 读取的 Key 并注入指定错误。
func (r *conversationListRecords) MGet(ctx context.Context, namespace string, keys []storage.RecordKey) (map[storage.RecordKey]storage.Record, error) {
	// 1. 读取 Session 分组时复制 Key 列表并检查 rootErr，其余调用底层批量读取。
	if namespace == dao.SessionNamespace {
		r.rootReads = append(r.rootReads, slices.Clone(keys))
		if r.rootErr != nil {
			return nil, r.rootErr
		}
	}
	return r.RecordStorage.MGet(ctx, namespace, keys)
}

// TestConversationListReadErrors 检查对话列表读取失败时返回完整错误。
func TestConversationListReadErrors(t *testing.T) {
	// 1. 分别制造根 Session 缺失、损坏、读取失败和列表失败，检查没有部分结果且保留底层错误。
	readErr := errors.New("read unavailable")
	for _, tt := range []struct {
		name string
		want string
	}{
		{
			name: "missing_root",
			want: `conversation "conversation" root session "root" not found`,
		},
		{
			name: "invalid_root",
			want: "stored Session",
		},
		{
			name: "root_read_failed",
			want: "read unavailable",
		},
		{
			name: "list_failed",
			want: "read unavailable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			counted := &conversationListRecords{RecordStorage: records}
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(counted, logs))
			if err != nil {
				t.Fatal(err)
			}
			if err := mutation.SetConversation(t.Context(), defs.Conversation{
				ID:            "conversation",
				RootSessionID: "root",
			}); err != nil {
				t.Fatal(err)
			}
			switch tt.name {
			case "invalid_root":
				if err := records.Set(t.Context(), dao.SessionNamespace, storage.Record{
					Key:       "conversation/root",
					UpdatedAt: 1,
					Payload:   json.RawMessage(`{}`),
				}); err != nil {
					t.Fatal(err)
				}
			case "root_read_failed":
				counted.rootErr = readErr
			case "list_failed":
				counted.listErr = readErr
			}
			page, err := query.ListConversations(t.Context(), dao.ListConversationsRequest{Limit: 10})
			if err == nil || !strings.Contains(err.Error(), tt.want) || len(page.Items) != 0 {
				t.Fatalf("ListConversations=%+v, %v, want no partial result and %q", page, err, tt.want)
			}
			if (tt.name == "root_read_failed" || tt.name == "list_failed") && !errors.Is(err, readErr) {
				t.Fatalf("error=%v, want wrapped read error", err)
			}
		})
	}
}
