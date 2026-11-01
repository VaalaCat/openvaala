package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

// TestReindex 检查索引重建、损坏对话跳过和数据目录锁处理。
func TestReindex(t *testing.T) {
	// 1. 列出空目录、已有对话、损坏记录和锁占用场景。
	tests := []struct {
		name string
		// setup 准备数据目录，返回 reindex 之后应该能查到的对话，以及应该被跳过并写进 stderr 的对话。
		setup      func(t *testing.T, dataDirectory string) (indexed, skipped []defs.ConversationID)
		wantStdout string
		// wantErr 非空时 reindex 必须返回含它的错误，并且不改动 metadata.db。
		wantErr string
	}{
		{
			name:       "empty_data_directory",
			setup:      func(*testing.T, string) (indexed, skipped []defs.ConversationID) { return nil, nil },
			wantStdout: "rebuilt 0 conversations\n",
		},
		{
			name: "rebuilds_copied_conversation",
			setup: func(t *testing.T, dataDirectory string) (indexed, skipped []defs.ConversationID) {
				id := writeReindexConversation(t, dataDirectory)
				removeMetadataIndex(t, dataDirectory)
				return []defs.ConversationID{id}, nil
			},
			wantStdout: "rebuilt 1 conversations\n",
		},
		{
			name: "skips_damaged_conversation",
			setup: func(t *testing.T, dataDirectory string) (indexed, skipped []defs.ConversationID) {
				id := writeReindexConversation(t, dataDirectory)
				damagedID := defs.NewConversationID()
				damaged := filepath.Join(dataDirectory, "conversations", string(damagedID))
				if err := os.MkdirAll(damaged, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(damaged, "metadata.jsonl"), []byte("not json\n"), 0600); err != nil {
					t.Fatal(err)
				}
				return []defs.ConversationID{id}, []defs.ConversationID{damagedID}
			},
			wantStdout: "rebuilt 1 conversations\n",
		},
		{
			name: "locked_by_serve",
			setup: func(t *testing.T, dataDirectory string) (indexed, skipped []defs.ConversationID) {
				id := writeReindexConversation(t, dataDirectory)
				unlock, err := lockDataDirectory(dataDirectory)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := unlock(); err != nil {
						t.Error(err)
					}
				})
				return []defs.ConversationID{id}, nil
			},
			wantErr: "another openvaala process is using",
		},
	}
	// 2. 准备数据并执行 reindex，检查输出、错误和可查询对话。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dataDirectory := filepath.Join(home, ".local", "share", "openvaala")
			indexed, skipped := tt.setup(t, dataDirectory)
			databaseBefore, _ := os.ReadFile(filepath.Join(dataDirectory, "metadata.db"))

			var stdout, stderr bytes.Buffer
			command := NewCLI(&CLIOpt{
				Stdout: &stdout,
				Stderr: &stderr,
			})
			command.SetArgs([]string{"reindex"})
			err := command.ExecuteContext(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("reindex error = %v, want containing %q", err, tt.wantErr)
				}
				if databaseAfter, _ := os.ReadFile(filepath.Join(dataDirectory, "metadata.db")); !bytes.Equal(databaseAfter, databaseBefore) {
					t.Fatal("metadata.db changed although reindex was rejected")
				}
			} else if err != nil {
				t.Fatalf("reindex error = %v, stderr = %s", err, stderr.String())
			}
			if stdout.String() != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if len(skipped) == 0 && stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			for _, id := range skipped {
				if want := "skipped conversation " + string(id) + ": "; !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
			query := openReindexQuery(t, dataDirectory)
			for _, id := range indexed {
				if _, found, err := query.GetConversation(t.Context(), id); err != nil || !found {
					t.Fatalf("GetConversation(%s) found=%v err=%v", id, found, err)
				}
			}
		})
	}
}

// openReindexOpt 按 CLI 布局打开测试存储，返回数据库关闭函数。
func openReindexOpt(t *testing.T, dataDirectory string) (*dao.DAOOpt, func()) {
	t.Helper()
	// 1. 创建数据目录，打开 metadata.db 和 conversations 日志目录。
	if err := os.MkdirAll(dataDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(dataDirectory, "metadata.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	closeDatabase := func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(dataDirectory, "conversations")))
	if err != nil {
		closeDatabase()
		t.Fatal(err)
	}
	return dao.NewDAOOpt(database.Record(), logs), closeDatabase
}

// openReindexQuery 创建测试数据目录的查询对象。
func openReindexQuery(t *testing.T, dataDirectory string) dao.Query {
	t.Helper()
	// 1. 打开存储并登记关闭操作，创建 DAO 查询对象。
	opt, closeDatabase := openReindexOpt(t, dataDirectory)
	t.Cleanup(closeDatabase)
	query, _, err := dao.NewDAO(opt)
	if err != nil {
		t.Fatal(err)
	}
	return query
}

// writeReindexConversation 写入用于重建索引的测试对话。
func writeReindexConversation(t *testing.T, dataDirectory string) defs.ConversationID {
	t.Helper()
	// 1. 打开 DAO 并登记关闭数据库，写入只有根 Session 的对话，再返回对话 ID。
	opt, closeDatabase := openReindexOpt(t, dataDirectory)
	defer closeDatabase()
	_, mutation, err := dao.NewDAO(opt)
	if err != nil {
		t.Fatal(err)
	}
	conversation := defs.Conversation{
		ID:            defs.NewConversationID(),
		RootSessionID: defs.NewSessionID(),
	}
	if err := mutation.SetSession(t.Context(), defs.Session{
		ID:             conversation.RootSessionID,
		ConversationID: conversation.ID,
		AgentName:      "assistant",
	}); err != nil {
		t.Fatal(err)
	}
	if err := mutation.SetConversation(t.Context(), conversation); err != nil {
		t.Fatal(err)
	}
	return conversation.ID
}

// removeMetadataIndex 删除测试索引文件。
func removeMetadataIndex(t *testing.T, dataDirectory string) {
	t.Helper()
	// 1. 删除 metadata.db 及 WAL、共享内存文件，忽略文件不存在，保留对话目录。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(dataDirectory, "metadata.db"+suffix)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}
