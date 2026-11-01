package dao_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// metadataLine 是测试读到的一行 metadata：外层追加时间与 payload 的种类。
type metadataLine struct {
	UpdatedAt int64
	Kind      string
	Payload   json.RawMessage
}

// readMetadata 读取对话测试日志中的 metadata 行。
func readMetadata(t *testing.T, logs storage.LogStorage, conversationID defs.ConversationID) []metadataLine {
	t.Helper()
	// 1. 升序读取至多 1000 行 metadata。
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     storage.StreamPath(conversationID),
		StreamID: "metadata",
		Order:    storage.DirectionAsc,
		Limit:    1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 2. 解码种类并保留每行日志时间和原始载荷。
	lines := make([]metadataLine, len(page.Items))
	for index, entry := range page.Items {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(entry.Payload, &head); err != nil {
			t.Fatal(err)
		}
		lines[index] = metadataLine{
			UpdatedAt: entry.UpdatedAt,
			Kind:      head.Kind,
			Payload:   entry.Payload,
		}
	}
	return lines
}

// getMetadataKinds 提取 metadata 行的种类。
func getMetadataKinds(lines []metadataLine) []string {
	// 1. 按输入顺序收集 Kind。
	kinds := make([]string, len(lines))
	for index, line := range lines {
		kinds[index] = line.Kind
	}
	return kinds
}

// TestSaveRecordAppendsMetadataFirst 检查实体保存先写 metadata，并沿用日志时间。
func TestSaveRecordAppendsMetadataFirst(t *testing.T) {
	// 1. 准备 Session、对话和 Turn 的保存与查询入口。
	conversationID := defs.NewConversationID()
	rootID := defs.NewSessionID()
	turnID := defs.NewTurnID()
	tests := []struct {
		name      string
		save      func(t *testing.T, mutation dao.Mutation)
		wantKinds []string
		// getIndexedAt 返回索引里这条记录的 UpdatedAt。
		getIndexedAt func(t *testing.T, query dao.Query) int64
	}{
		{
			name: "session",
			save: func(t *testing.T, mutation dao.Mutation) {
				if err := mutation.SetSession(t.Context(), defs.Session{
					ID:             rootID,
					ConversationID: conversationID,
					AgentName:      "test",
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantKinds: []string{"session_saved"},
			getIndexedAt: func(t *testing.T, query dao.Query) int64 {
				value, found, err := query.GetSession(t.Context(), conversationID, rootID)
				if err != nil || !found {
					t.Fatalf("GetSession() = %v, %v", found, err)
				}
				return value.UpdatedAt
			},
		},
		{
			name: "conversation",
			save: func(t *testing.T, mutation dao.Mutation) {
				if err := mutation.SetConversation(t.Context(), defs.Conversation{
					ID:            conversationID,
					RootSessionID: rootID,
					Title:         "标题",
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantKinds: []string{"conversation_saved"},
			getIndexedAt: func(t *testing.T, query dao.Query) int64 {
				value, found, err := query.GetConversation(t.Context(), conversationID)
				if err != nil || !found {
					t.Fatalf("GetConversation() = %v, %v", found, err)
				}
				return value.UpdatedAt
			},
		},
		{
			name: "turn",
			save: func(t *testing.T, mutation dao.Mutation) {
				turn := defs.Turn{
					ID:             turnID,
					ConversationID: conversationID,
					SessionID:      rootID,
					Status:         defs.TurnStatusRunning,
				}
				if err := mutation.SetTurn(t.Context(), turn); err != nil {
					t.Fatal(err)
				}
			},
			wantKinds: []string{"turn_saved"},
			getIndexedAt: func(t *testing.T, query dao.Query) int64 {
				value, found, err := query.GetTurn(t.Context(), rootID, turnID)
				if err != nil || !found {
					t.Fatalf("GetTurn() = %v, %v", found, err)
				}
				return value.UpdatedAt
			},
		},
	}
	// 2. 保存实体，检查日志种类、不含载荷时间且索引时间与日志一致。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			tt.save(t, mutation)
			lines := readMetadata(t, logs, conversationID)
			if !slices.Equal(getMetadataKinds(lines), tt.wantKinds) {
				t.Fatalf("metadata kinds = %v, want %v", getMetadataKinds(lines), tt.wantKinds)
			}
			if strings.Contains(string(lines[0].Payload), "updated_at") {
				t.Fatalf("metadata payload = %s, want no updated_at", lines[0].Payload)
			}
			if got := tt.getIndexedAt(t, query); got != lines[0].UpdatedAt {
				t.Fatalf("indexed UpdatedAt = %d, want metadata time %d", got, lines[0].UpdatedAt)
			}
		})
	}
}

// TestSaveRecordFailures 检查保存失败时日志与索引的剩余数据。
func TestSaveRecordFailures(t *testing.T) {
	// 1. 准备日志追加失败和索引写入失败场景。
	conversationID := defs.NewConversationID()
	session := defs.Session{
		ID:             defs.NewSessionID(),
		ConversationID: conversationID,
		AgentName:      "test",
	}
	tests := []struct {
		name         string
		failAppend   bool
		failIndex    bool
		wantMetadata int
		wantErr      string
	}{
		{
			name:         "append_fails_index_untouched",
			failAppend:   true,
			wantMetadata: 0,
			wantErr:      "append metadata",
		},
		{
			name:         "index_fails_metadata_kept",
			failIndex:    true,
			wantMetadata: 1,
			wantErr:      "index not updated",
		},
	}
	// 2. 保存 Session 后检查错误、metadata 行数和索引未写入。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			var daoRecords storage.RecordStorage = records
			var daoLogs storage.LogStorage = logs
			if tt.failAppend {
				daoLogs = &failingAppendLogStorage{LogStorage: logs}
			}
			if tt.failIndex {
				daoRecords = &failingSetRecordStorage{RecordStorage: records}
			}
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(daoRecords, daoLogs))
			if err != nil {
				t.Fatal(err)
			}
			if err := mutation.SetSession(t.Context(), session); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("SetSession() error = %v, want %q", err, tt.wantErr)
			}
			if got := readMetadata(t, logs, conversationID); len(got) != tt.wantMetadata {
				t.Fatalf("metadata lines = %d, want %d", len(got), tt.wantMetadata)
			}
			if _, found, err := query.GetSession(t.Context(), conversationID, session.ID); err != nil || found {
				t.Fatalf("GetSession() found=%v err=%v, want not indexed", found, err)
			}
		})
	}
}

// failingAppendLogStorage 包装日志存储，Append 总是失败。
type failingAppendLogStorage struct {
	storage.LogStorage
}

// Append 返回注入的日志追加错误。
func (*failingAppendLogStorage) Append(context.Context, storage.StreamPath, storage.StreamID, ...json.RawMessage) ([]storage.LogEntryInfo, error) {
	// 1. 返回注入的日志追加错误。
	return nil, errors.New("injected Append failure")
}

// failingSetRecordStorage 包装记录存储，Set 总是失败。
type failingSetRecordStorage struct {
	storage.RecordStorage
}

// Set 返回注入的索引写入错误。
func (*failingSetRecordStorage) Set(context.Context, string, storage.Record) error {
	// 1. 返回注入的索引写入错误。
	return errors.New("injected Set failure")
}

// conversationIndex 是通过公开查询读到的一个对话的全部索引内容。
type conversationIndex struct {
	Conversation defs.Conversation
	Sessions     []defs.Session
	Turns        map[defs.SessionID][]defs.Turn
}

// getConversationIndex 读取对话及所属全部 Session 和 Turn。
func getConversationIndex(t *testing.T, query dao.Query, id defs.ConversationID) conversationIndex {
	t.Helper()
	// 1. 读取对话并创建索引结果。
	conversation, found, err := query.GetConversation(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("GetConversation(%s) = %v, %v", id, found, err)
	}
	index := conversationIndex{
		Conversation: conversation,
		Turns:        map[defs.SessionID][]defs.Turn{},
	}
	// 2. 迭代全部 Session，逐页读取并保存其 Turn。
	for session, err := range query.ScanSessions(t.Context(), id, storage.Order{Direction: storage.DirectionAsc}) {
		if err != nil {
			t.Fatal(err)
		}
		index.Sessions = append(index.Sessions, session)
		var turns []defs.Turn
		request := dao.ListTurnsRequest{
			SessionID: session.ID,
			Limit:     100,
		}
		for {
			page, err := query.ListTurns(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			turns = append(turns, page.Items...)
			if page.Next == (dao.RecordCursor[defs.TurnID]{}) {
				break
			}
			request.Cursor = page.Next
		}
		index.Turns[session.ID] = turns
	}
	return index
}

// writeConversation 写入带更新记录、子 Session 及删除记录的测试对话。
func writeConversation(t *testing.T, mutation dao.Mutation) (defs.Conversation, defs.SessionID) {
	t.Helper()
	// 1. 创建对话及根 Session。
	ctx := t.Context()
	conversation := defs.Conversation{
		ID:            defs.NewConversationID(),
		RootSessionID: defs.NewSessionID(),
	}
	root := defs.Session{
		ID:             conversation.RootSessionID,
		ConversationID: conversation.ID,
		AgentName:      "assistant",
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// 2. 保存根 Session 和对话，再修改标题与根 Session 模型。
	must(mutation.SetSession(ctx, root))
	must(mutation.SetConversation(ctx, conversation))
	conversation.Title = "迁移测试"
	must(mutation.SetConversation(ctx, conversation))
	root.Model = defs.ModelSelection{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "b",
		},
		ReasoningEffort: "high",
	}
	must(mutation.SetSession(ctx, root))
	// 3. 写入 Turn 的运行与完成记录，再更新对话消息数。
	turn := defs.Turn{
		ID:             defs.NewTurnID(),
		ConversationID: conversation.ID,
		SessionID:      root.ID,
		Status:         defs.TurnStatusRunning,
		Model:          root.Model,
	}
	must(mutation.SetTurn(ctx, turn))
	turn.Status, turn.Usage = defs.TurnStatusCompleted, defs.TokenUsage{
		Input:  10,
		Output: 5,
	}
	turn.ItemsRange = defs.TurnItemRange{
		FirstOrdinal: 101,
		LastOrdinal:  135,
	}
	turn.ContextUsage = &defs.TurnContextUsage{
		ModelCallID:        defs.NewModelCallID(),
		InputThroughItemID: defs.NewItemID(),
		Usage:              defs.TokenUsage{Input: 9, Output: 4, Reasoning: 3},
		ReasoningContext:   "current_turn",
	}
	must(mutation.SetTurn(ctx, turn))
	conversation.MessageCount = 2
	must(mutation.SetConversation(ctx, conversation))
	// 4. 为两个子 Session 写入分叉 Item 和 Turn，再删除其中一个并返回其 ID。
	for _, task := range []string{"keep", "drop"} {
		child := defs.Session{
			ID:              defs.NewSessionID(),
			ConversationID:  conversation.ID,
			AgentName:       "assistant",
			TaskName:        task,
			ParentSessionID: root.ID,
			ParentTurnID:    turn.ID,
			ParentCallID:    "call-" + task,
		}
		if _, err := mutation.AppendItems(ctx, conversation.ID, child.ID, &defs.Item{
			ID:          defs.NewItemID(),
			Kind:        defs.ItemSessionFork,
			SessionFork: &defs.SessionFork{SourceSessionID: root.ID},
		}); err != nil {
			t.Fatal(err)
		}
		must(mutation.SetSession(ctx, child))
		must(mutation.SetTurn(ctx, defs.Turn{
			ID:             defs.NewTurnID(),
			ConversationID: conversation.ID,
			SessionID:      child.ID,
			Status:         defs.TurnStatusFailed,
			Error:          "boom",
		}))
		if task == "drop" {
			must(mutation.DeleteSession(ctx, conversation.ID, child.ID))
			return conversation, child.ID
		}
	}
	t.Fatal("unreachable")
	return conversation, ""
}

// TestRebuildMetadataIndex 检查从 metadata 重建对话索引。
func TestRebuildMetadataIndex(t *testing.T) {
	// 1. 准备重建到新索引或原索引、清掉过期索引、删除后重新保存 Session、跨页 metadata、归档和中断的删除场景。
	tests := []struct {
		name string
		// damage 在重建前修改日志目录，返回预期仍在索引里的对话，以及预期按 WAL 删除的对话。
		damage func(t *testing.T, logs storage.LogStorage, logRoot string, ids []defs.ConversationID) (kept, deleted []defs.ConversationID)
		// sameIndex 是否重建到原索引文件？否时重建到全新的索引文件。
		sameIndex bool
		// extraWrite 在两个对话写完之后，对第一个对话再写一批记录；nil 表示不写。
		extraWrite func(t *testing.T, mutation dao.Mutation, conversation defs.Conversation, deletedID defs.SessionID)
		// deletedBack 被删的子 Session 是否在 extraWrite 里重新保存过？是时重建后它应该存在。
		deletedBack bool
	}{
		{
			name:   "fresh_index_matches_direct_writes",
			damage: keepAllConversations,
		},
		{
			name:      "rebuild_into_same_index",
			sameIndex: true,
			damage:    keepAllConversations,
		},
		{
			name:      "stale_index_cleared",
			sameIndex: true,
			damage: func(t *testing.T, _ storage.LogStorage, logRoot string, ids []defs.ConversationID) ([]defs.ConversationID, []defs.ConversationID) {
				if err := os.RemoveAll(filepath.Join(logRoot, string(ids[1]))); err != nil {
					t.Fatal(err)
				}
				return ids[:1], nil
			},
		},
		{
			// session_deleted 之后又出现同 ID 的 session_saved：Session 回来，删除前的 Turn 不回来。
			name:        "deleted_session_saved_again",
			damage:      keepAllConversations,
			deletedBack: true,
			extraWrite: func(t *testing.T, mutation dao.Mutation, conversation defs.Conversation, deletedID defs.SessionID) {
				session := defs.Session{
					ID:             deletedID,
					ConversationID: conversation.ID,
					AgentName:      "assistant",
					TaskName:       "again",
				}
				if err := mutation.SetSession(t.Context(), session); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// 比 dao 每页读取的 256 行更多，重建要翻页读完；第一个 Turn 的最后一行在最后一页，最后一行为准。
			name:   "more_than_one_page_of_metadata",
			damage: keepAllConversations,
			extraWrite: func(t *testing.T, mutation dao.Mutation, conversation defs.Conversation, _ defs.SessionID) {
				turn := defs.Turn{
					ID:             defs.NewTurnID(),
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
					Status:         defs.TurnStatusRunning,
				}
				if err := mutation.SetTurn(t.Context(), turn); err != nil {
					t.Fatal(err)
				}
				for range 300 {
					other := defs.Turn{
						ID:             defs.NewTurnID(),
						ConversationID: conversation.ID,
						SessionID:      conversation.RootSessionID,
						Status:         defs.TurnStatusCompleted,
					}
					if err := mutation.SetTurn(t.Context(), other); err != nil {
						t.Fatal(err)
					}
				}
				turn.Status = defs.TurnStatusCompleted
				if err := mutation.SetTurn(t.Context(), turn); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// 归档状态随 conversation_saved 重放，重建后仍只在归档列表里。
			name:   "archived_conversation_stays_archived",
			damage: keepAllConversations,
			extraWrite: func(t *testing.T, mutation dao.Mutation, conversation defs.Conversation, _ defs.SessionID) {
				conversation.ArchivedAt = time.Now().UnixMilli()
				if err := mutation.SetConversation(t.Context(), conversation); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// 删除只写完 conversation_deleted 就中断：索引和目录都还在，重建按 WAL 删掉目录、不建索引。
			name:      "interrupted_delete_finished",
			sameIndex: true,
			damage: func(t *testing.T, logs storage.LogStorage, _ string, ids []defs.ConversationID) ([]defs.ConversationID, []defs.ConversationID) {
				payload := json.RawMessage(`{"kind":"conversation_deleted","deleted_conversation_id":"` + string(ids[0]) + `"}`)
				if _, err := logs.Append(t.Context(), storage.StreamPath(ids[0]), dao.MetadataStreamID, payload); err != nil {
					t.Fatal(err)
				}
				return ids[1:], ids[:1]
			},
		},
	}
	// 2. 写入两个对话并修改日志，重复重建后检查全部实体与删除结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			logRoot := filepath.Join(root, "conversations")
			records, logs := openStorageAt(t, filepath.Join(root, "metadata.db"), logRoot)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			first, deletedID := writeConversation(t, mutation)
			second, _ := writeConversation(t, mutation)
			if tt.extraWrite != nil {
				tt.extraWrite(t, mutation, first, deletedID)
			}
			ids := []defs.ConversationID{first.ID, second.ID}
			want := map[defs.ConversationID]conversationIndex{}
			for _, id := range ids {
				want[id] = getConversationIndex(t, query, id)
			}
			kept, deleted := tt.damage(t, logs, logRoot, ids)

			targetRecords, targetLogs := records, storage.LogStorage(nil)
			if !tt.sameIndex {
				targetRecords, targetLogs = openStorageAt(t, filepath.Join(root, "rebuilt.db"), logRoot)
			} else {
				_, targetLogs = openStorageAt(t, filepath.Join(root, "unused.db"), logRoot)
			}
			opt := dao.NewDAOOpt(targetRecords, targetLogs)
			for run := range 2 {
				// 第一次重建已经删掉目录，第二次不再报告删除。
				wantDeleted := deleted
				if run > 0 {
					wantDeleted = nil
				}
				result, err := dao.RebuildMetadataIndex(t.Context(), opt)
				if err != nil || result.RebuiltConversations != len(kept) || len(result.SkippedConversations) != 0 ||
					!slices.Equal(result.DeletedConversations, wantDeleted) {
					t.Fatalf("RebuildMetadataIndex() run %d = %+v, %v, want %d rebuilt and %v deleted", run, result, err, len(kept), wantDeleted)
				}
			}
			rebuilt, _, err := dao.NewDAO(opt)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range kept {
				if got := getConversationIndex(t, rebuilt, id); !reflect.DeepEqual(got, want[id]) {
					t.Fatalf("rebuilt %s = %+v, want %+v", id, got, want[id])
				}
			}
			if _, found, err := rebuilt.GetSession(t.Context(), first.ID, deletedID); err != nil || found != tt.deletedBack {
				t.Fatalf("deleted Session found=%v err=%v, want found=%v", found, err, tt.deletedBack)
			}
			// 删除前的 Turn 不会因为 Session 被重新保存而回来。
			deletedTurns, err := rebuilt.ListTurns(t.Context(), dao.ListTurnsRequest{
				SessionID: deletedID,
				Limit:     100,
			})
			if err != nil || len(deletedTurns.Items) != 0 {
				t.Fatalf("ListTurns(deleted Session) = %+v, %v, want empty", deletedTurns.Items, err)
			}
			for _, id := range deleted {
				if _, found, err := rebuilt.GetConversation(t.Context(), id); err != nil || found {
					t.Fatalf("deleted Conversation found=%v err=%v, want not indexed", found, err)
				}
				if _, err := os.Stat(filepath.Join(logRoot, string(id))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("deleted Conversation directory stat = %v, want not exist", err)
				}
			}
			for _, isArchivedList := range []bool{false, true} {
				page, err := rebuilt.ListConversations(t.Context(), dao.ListConversationsRequest{
					Order: storage.Order{
						By:        storage.OrderByUpdatedAt,
						Direction: storage.DirectionDesc,
					},
					Limit:    10,
					Archived: isArchivedList,
				})
				wantCount := 0
				for _, id := range kept {
					if (want[id].Conversation.ArchivedAt != 0) == isArchivedList {
						wantCount++
					}
				}
				if err != nil || len(page.Items) != wantCount {
					t.Fatalf("ListConversations(Archived: %v) = %+v, %v, want %d", isArchivedList, page.Items, err, wantCount)
				}
				for index, entry := range page.Items {
					conversation := entry.Conversation
					if (conversation.ArchivedAt != 0) != isArchivedList || !reflect.DeepEqual(conversation, want[conversation.ID].Conversation) ||
						(index > 0 && conversation.UpdatedAt > page.Items[index-1].Conversation.UpdatedAt) {
						t.Fatalf("ListConversations(Archived: %v)[%d] = %+v", isArchivedList, index, conversation)
					}
				}
			}
		})
	}
}

// keepAllConversations 返回全部对话作为应重建列表。
func keepAllConversations(_ *testing.T, _ storage.LogStorage, _ string, ids []defs.ConversationID) ([]defs.ConversationID, []defs.ConversationID) {
	// 1. 原样返回对话 ID，不返回删除列表。
	return ids, nil
}

// TestRebuildMetadataIndexEmptyRoot 检查空日志根目录的索引重建。
func TestRebuildMetadataIndexEmptyRoot(t *testing.T) {
	// 1. 打开空存储后重建，检查数量为零且跳过和删除列表均为非 nil 空列表。
	records, logs := openStorage(t)
	result, err := dao.RebuildMetadataIndex(t.Context(), dao.NewDAOOpt(records, logs))
	if err != nil || result.RebuiltConversations != 0 || result.SkippedConversations == nil || len(result.SkippedConversations) != 0 ||
		result.DeletedConversations == nil || len(result.DeletedConversations) != 0 {
		t.Fatalf("RebuildMetadataIndex() = %+v, %v, want empty result", result, err)
	}
}

// TestRebuildMetadataIndexSkipsDamagedConversation 检查索引重建跳过损坏对话并保留目录。
func TestRebuildMetadataIndexSkipsDamagedConversation(t *testing.T) {
	// 1. 准备损坏行、缺记录、目录不符及错误删除记录场景。
	appendRaw := func(t *testing.T, path string, content string) {
		t.Helper()
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteString(content); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		// damage 在日志目录里制造一个有问题的对话，返回它的目录名。
		damage     func(t *testing.T, mutation dao.Mutation, logs storage.LogStorage, logRoot string) defs.ConversationID
		wantReason string
	}{
		{
			name: "corrupt_complete_line",
			damage: func(t *testing.T, mutation dao.Mutation, _ storage.LogStorage, logRoot string) defs.ConversationID {
				conversation, _ := writeConversation(t, mutation)
				appendRaw(t, filepath.Join(logRoot, string(conversation.ID), "metadata.jsonl"), "not json\n")
				return conversation.ID
			},
			wantReason: "invalid JSONL",
		},
		{
			name: "missing_conversation_record",
			damage: func(t *testing.T, mutation dao.Mutation, _ storage.LogStorage, _ string) defs.ConversationID {
				id := defs.NewConversationID()
				if err := mutation.SetSession(t.Context(), defs.Session{
					ID:             defs.NewSessionID(),
					ConversationID: id,
					AgentName:      "assistant",
				}); err != nil {
					t.Fatal(err)
				}
				return id
			},
			wantReason: "conversation record is missing",
		},
		{
			name: "missing_root_session",
			damage: func(t *testing.T, mutation dao.Mutation, _ storage.LogStorage, _ string) defs.ConversationID {
				conversation := defs.Conversation{
					ID:            defs.NewConversationID(),
					RootSessionID: defs.NewSessionID(),
				}
				if err := mutation.SetConversation(t.Context(), conversation); err != nil {
					t.Fatal(err)
				}
				return conversation.ID
			},
			wantReason: "root Session",
		},
		{
			name: "directory_name_mismatch",
			damage: func(t *testing.T, mutation dao.Mutation, _ storage.LogStorage, logRoot string) defs.ConversationID {
				conversation, _ := writeConversation(t, mutation)
				renamed := defs.NewConversationID()
				if err := os.Rename(filepath.Join(logRoot, string(conversation.ID)), filepath.Join(logRoot, string(renamed))); err != nil {
					t.Fatal(err)
				}
				return renamed
			},
			wantReason: "belongs to Conversation",
		},
		{
			name: "unknown_kind",
			damage: func(t *testing.T, mutation dao.Mutation, logs storage.LogStorage, _ string) defs.ConversationID {
				conversation, _ := writeConversation(t, mutation)
				payload := json.RawMessage(`{"kind":"conversation_renamed","conversation":{"id":"` + string(conversation.ID) + `","root_session_id":"x"}}`)
				if _, err := logs.Append(t.Context(), storage.StreamPath(conversation.ID), "metadata", payload); err != nil {
					t.Fatal(err)
				}
				return conversation.ID
			},
			wantReason: "has no matching record",
		},
		{
			name: "two_records_in_one_line",
			damage: func(t *testing.T, mutation dao.Mutation, logs storage.LogStorage, _ string) defs.ConversationID {
				conversation, _ := writeConversation(t, mutation)
				payload := json.RawMessage(`{"kind":"session_deleted","deleted_session_id":"s","conversation":{"id":"` + string(conversation.ID) + `"}}`)
				if _, err := logs.Append(t.Context(), storage.StreamPath(conversation.ID), "metadata", payload); err != nil {
					t.Fatal(err)
				}
				return conversation.ID
			},
			wantReason: "session_deleted requires only deleted_session_id",
		},
		{
			// 删除记录写的是别的对话：不能据此删除，这个对话被跳过。
			name: "conversation_deleted_for_other_conversation",
			damage: func(t *testing.T, mutation dao.Mutation, logs storage.LogStorage, _ string) defs.ConversationID {
				conversation, _ := writeConversation(t, mutation)
				payload := json.RawMessage(`{"kind":"conversation_deleted","deleted_conversation_id":"` + string(defs.NewConversationID()) + `"}`)
				if _, err := logs.Append(t.Context(), storage.StreamPath(conversation.ID), "metadata", payload); err != nil {
					t.Fatal(err)
				}
				return conversation.ID
			},
			wantReason: "conversation_deleted requires only this conversation's deleted_conversation_id",
		},
	}
	// 2. 重建后检查跳过原因、正常对话完整、损坏对话不入索引且目录保留。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			logRoot := filepath.Join(root, "conversations")
			records, logs := openStorageAt(t, filepath.Join(root, "metadata.db"), logRoot)
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			good, _ := writeConversation(t, mutation)
			wantGood := getConversationIndex(t, query, good.ID)
			damagedID := tt.damage(t, mutation, logs, logRoot)

			targetRecords, targetLogs := openStorageAt(t, filepath.Join(root, "rebuilt.db"), logRoot)
			opt := dao.NewDAOOpt(targetRecords, targetLogs)
			result, err := dao.RebuildMetadataIndex(t.Context(), opt)
			if err != nil || result.RebuiltConversations != 1 || len(result.SkippedConversations) != 1 {
				t.Fatalf("RebuildMetadataIndex() = %+v, %v", result, err)
			}
			skipped := result.SkippedConversations[0]
			if skipped.ConversationID != damagedID || skipped.Reason == nil || !strings.Contains(skipped.Reason.Error(), tt.wantReason) {
				t.Fatalf("skipped = %+v, want %s with %q", skipped, damagedID, tt.wantReason)
			}
			rebuilt, _, err := dao.NewDAO(opt)
			if err != nil {
				t.Fatal(err)
			}
			if got := getConversationIndex(t, rebuilt, good.ID); !reflect.DeepEqual(got, wantGood) {
				t.Fatalf("good conversation = %+v, want %+v", got, wantGood)
			}
			if _, found, err := rebuilt.GetConversation(t.Context(), damagedID); err != nil || found {
				t.Fatalf("damaged conversation found=%v err=%v", found, err)
			}
			// 跳过的目录（例如只复制了一半、没有 conversation_saved 的分叉）原样保留：WAL 没有记下要删它。
			if _, err := os.Stat(filepath.Join(logRoot, string(damagedID))); err != nil {
				t.Fatalf("damaged conversation directory stat = %v, want kept", err)
			}
		})
	}
}

// TestRebuildMetadataIndexRecovers 检查索引重建修复尾行和漏写索引。
func TestRebuildMetadataIndexRecovers(t *testing.T) {
	// 1. 准备未完成尾行和 Turn 索引写入失败场景。
	tests := []struct {
		name  string
		write func(t *testing.T, root string) (defs.ConversationID, conversationIndex)
	}{
		{
			name: "torn_tail",
			write: func(t *testing.T, root string) (defs.ConversationID, conversationIndex) {
				records, logs := openStorageAt(t, filepath.Join(root, "metadata.db"), filepath.Join(root, "conversations"))
				query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
				if err != nil {
					t.Fatal(err)
				}
				conversation, _ := writeConversation(t, mutation)
				want := getConversationIndex(t, query, conversation.ID)
				file, err := os.OpenFile(filepath.Join(root, "conversations", string(conversation.ID), "metadata.jsonl"), os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if _, err := file.WriteString(`{"ordinal":999,"updated_at":1,"payload":{"kind"`); err != nil {
					t.Fatal(err)
				}
				return conversation.ID, want
			},
		},
		{
			name: "turn_index_failed",
			write: func(t *testing.T, root string) (defs.ConversationID, conversationIndex) {
				records, logs := openStorageAt(t, filepath.Join(root, "metadata.db"), filepath.Join(root, "conversations"))
				query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
				if err != nil {
					t.Fatal(err)
				}
				conversation, _ := writeConversation(t, mutation)
				_, failing, err := dao.NewDAO(dao.NewDAOOpt(&failingTurnIndexStorage{RecordStorage: records}, logs))
				if err != nil {
					t.Fatal(err)
				}
				turn := defs.Turn{
					ID:             defs.NewTurnID(),
					ConversationID: conversation.ID,
					SessionID:      conversation.RootSessionID,
					Status:         defs.TurnStatusInterrupted,
				}
				if err := failing.SetTurn(t.Context(), turn); err == nil || !strings.Contains(err.Error(), "index not updated") {
					t.Fatalf("SetTurn() error = %v, want index failure", err)
				}
				want := getConversationIndex(t, query, conversation.ID)
				saved := readMetadata(t, logs, conversation.ID)
				turn.UpdatedAt = saved[len(saved)-1].UpdatedAt
				want.Turns[conversation.RootSessionID] = append(want.Turns[conversation.RootSessionID], turn)
				slices.SortFunc(want.Turns[conversation.RootSessionID], func(a, b defs.Turn) int { return strings.Compare(string(a.ID), string(b.ID)) })
				return conversation.ID, want
			},
		},
	}
	// 2. 重建到新数据库并比较全部对话索引。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			id, want := tt.write(t, root)
			records, logs := openStorageAt(t, filepath.Join(root, "rebuilt.db"), filepath.Join(root, "conversations"))
			opt := dao.NewDAOOpt(records, logs)
			if result, err := dao.RebuildMetadataIndex(t.Context(), opt); err != nil || result.RebuiltConversations != 1 {
				t.Fatalf("RebuildMetadataIndex() = %+v, %v", result, err)
			}
			rebuilt, _, err := dao.NewDAO(opt)
			if err != nil {
				t.Fatal(err)
			}
			if got := getConversationIndex(t, rebuilt, id); !reflect.DeepEqual(got, want) {
				t.Fatalf("rebuilt = %+v, want %+v", got, want)
			}
		})
	}
}

// failingTurnIndexStorage 包装记录存储，只有写 Turn 索引时失败。
type failingTurnIndexStorage struct {
	storage.RecordStorage
}

// Set 只对 Turn 索引写入注入错误。
func (s *failingTurnIndexStorage) Set(ctx context.Context, namespace string, record storage.Record) error {
	// 1. turns 分组返回指定错误，其余分组写入底层存储。
	if namespace == "turns" {
		return errors.New("injected turn index failure")
	}
	return s.RecordStorage.Set(ctx, namespace, record)
}
