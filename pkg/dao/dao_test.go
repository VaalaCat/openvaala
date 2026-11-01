package dao_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

// TestNewDAO 检查 DAO 创建时的存储配置。
func TestNewDAO(t *testing.T) {
	// 1. 准备有效存储及缺少存储的配置。
	records, logs := openStorage(t)
	tests := []struct {
		name    string
		opt     *dao.DAOOpt
		wantErr bool
	}{
		{
			name:    "nil_options",
			wantErr: true,
		},
		{
			name:    "missing_records",
			opt:     dao.NewDAOOpt(nil, logs),
			wantErr: true,
		},
		{
			name:    "missing_logs",
			opt:     dao.NewDAOOpt(records, nil),
			wantErr: true,
		},
		{
			name: "valid_pair",
			opt:  dao.NewDAOOpt(records, logs),
		},
	}
	// 2. 检查创建错误以及有效配置返回读写接口。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, mutation, err := dao.NewDAO(tt.opt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewDAO() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (query == nil || mutation == nil) {
				t.Fatal("NewDAO() returned nil Query or Mutation")
			}
		})
	}
}

// TestStoredPayloadOmitsUpdatedAt 检查实体载荷不保存传入的写入时间。
func TestStoredPayloadOmitsUpdatedAt(t *testing.T) {
	// 1. 准备对话、Session、Turn 和 Item 的保存入口。
	tests := []struct {
		name  string
		store func(t *testing.T, mutation dao.Mutation, records storage.RecordStorage, logs storage.LogStorage) json.RawMessage
	}{
		{
			name: "conversation",
			store: func(t *testing.T, mutation dao.Mutation, records storage.RecordStorage, _ storage.LogStorage) json.RawMessage {
				value := defs.Conversation{
					ID:            defs.NewConversationID(),
					RootSessionID: defs.NewSessionID(),
					UpdatedAt:     42,
				}
				if err := mutation.SetConversation(t.Context(), value); err != nil {
					t.Fatal(err)
				}
				return getStoredPayload(t, records, "conversations", storage.RecordKey(value.ID))
			},
		},
		{
			name: "session",
			store: func(t *testing.T, mutation dao.Mutation, records storage.RecordStorage, _ storage.LogStorage) json.RawMessage {
				value := defs.Session{
					ID:             defs.NewSessionID(),
					ConversationID: "test-conversation",
					AgentName:      "test",
					UpdatedAt:      42,
				}
				if err := mutation.SetSession(t.Context(), value); err != nil {
					t.Fatal(err)
				}
				return getStoredPayload(t, records, "sessions", storage.RecordKey("test-conversation/"+string(value.ID)))
			},
		},
		{
			name: "turn",
			store: func(t *testing.T, mutation dao.Mutation, records storage.RecordStorage, _ storage.LogStorage) json.RawMessage {
				value := defs.Turn{
					ID:             defs.NewTurnID(),
					ConversationID: defs.NewConversationID(),
					SessionID:      defs.NewSessionID(),
					Status:         defs.TurnStatusRunning,
					UpdatedAt:      42,
				}
				if err := mutation.SetTurn(t.Context(), value); err != nil {
					t.Fatal(err)
				}
				return getStoredPayload(t, records, "turns", storage.RecordKey(string(value.SessionID)+"/"+string(value.ID)))
			},
		},
		{
			name: "item",
			store: func(t *testing.T, mutation dao.Mutation, _ storage.RecordStorage, logs storage.LogStorage) json.RawMessage {
				conversationID := defs.NewConversationID()
				sessionID := defs.NewSessionID()
				if _, err := mutation.AppendItems(t.Context(), conversationID, sessionID, &defs.Item{
					ID:        "1",
					UpdatedAt: 42,
				}); err != nil {
					t.Fatal(err)
				}
				page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
					Path:     storage.StreamPath(conversationID),
					StreamID: storage.StreamID(sessionID),
					Order:    storage.DirectionAsc,
					Limit:    1,
				})
				if err != nil || len(page.Items) != 1 {
					t.Fatalf("logs.List() = %+v, %v", page, err)
				}
				return page.Items[0].Payload
			},
		},
	}
	// 2. 保存带时间的实体，读取原始载荷并检查没有 updated_at 字段。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, logs := openStorage(t)
			_, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
			if err != nil {
				t.Fatal(err)
			}
			if payload := tt.store(t, mutation, records, logs); strings.Contains(string(payload), "updated_at") {
				t.Fatalf("stored payload = %s, want no updated_at", payload)
			}
		})
	}
}

// getStoredPayload 从底层记录存储读取原始 JSON 载荷。
func getStoredPayload(t *testing.T, records storage.RecordStorage, namespace string, key storage.RecordKey) json.RawMessage {
	t.Helper()
	// 1. 按分组和 Key 读取，未找到或读取失败时终止测试。
	stored, found, err := records.Get(t.Context(), namespace, key)
	if err != nil || !found {
		t.Fatalf("records.Get(%s, %s) = %v, %v", namespace, key, found, err)
	}
	return stored.Payload
}

// openStorage 在临时目录打开测试索引和日志存储。
func openStorage(t *testing.T) (storage.RecordStorage, storage.LogStorage) {
	t.Helper()
	// 1. 创建临时目录，再打开 metadata.db 和 conversations 日志目录。
	root := t.TempDir()
	return openStorageAt(t, filepath.Join(root, "metadata.db"), filepath.Join(root, "conversations"))
}

// openStorageAt 打开指定路径的测试索引和对话日志。
func openStorageAt(t *testing.T, databasePath, logRoot string) (storage.RecordStorage, storage.LogStorage) {
	t.Helper()
	// 1. 声明全部 DAO 记录分组，打开 SQLite 并登记关闭。
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(databasePath, dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	// 2. 打开指定日志根目录，返回记录和日志存储。
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(logRoot))
	if err != nil {
		t.Fatal(err)
	}
	return database.Record(), logs
}

// openDAO 打开测试 DAO 及其底层记录存储。
func openDAO(t *testing.T) (dao.Query, dao.Mutation, storage.RecordStorage) {
	t.Helper()
	// 1. 打开两个底层存储，再创建读写接口。
	records, logs := openStorage(t)
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(records, logs))
	if err != nil {
		t.Fatal(err)
	}
	return query, mutation, records
}
