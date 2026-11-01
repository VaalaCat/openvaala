package storage_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

// TestRecordAndLogPersistAcrossReopen 检查 SQLite 记录和文件日志重新打开后的读写。
func TestRecordAndLogPersistAcrossReopen(t *testing.T) {
	// 1. 创建两种存储并写入一条记录和两条日志。
	root := t.TempDir()
	databasePath := filepath.Join(root, "records.db")
	logRoot := filepath.Join(root, "logs")
	path := storage.StreamPath("conversation-a")
	streamID := storage.StreamID("session-a")
	want := json.RawMessage(`{"name":"alpha"}`)

	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(databasePath, storage.RecordNamespace{Name: "agents"}))
	if err != nil {
		t.Fatal(err)
	}
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(logRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Record().Set(t.Context(), "agents", storage.Record{
		Key:       "agent-a",
		UpdatedAt: 1000,
		Payload:   want,
	}); err != nil {
		t.Fatal(err)
	}
	if infos, err := logs.Append(t.Context(), path, streamID, want, want); err != nil || !slices.Equal(toOrdinals(infos), []storage.Ordinal{1, 2}) {
		t.Fatalf("Append() = %v, %v, want [1 2]", infos, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. 关闭后重新打开，检查记录时间、载荷和日志分页。
	database, err = storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(databasePath, storage.RecordNamespace{Name: "agents"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logs, err = storagefile.NewLogStorage(storagefile.NewLogStorageOpt(logRoot))
	if err != nil {
		t.Fatal(err)
	}

	record, found, err := database.Record().Get(t.Context(), "agents", "agent-a")
	if err != nil || !found || string(record.Payload) != string(want) || record.UpdatedAt != 1000 {
		t.Fatalf("Get() = %+v, %v, %v, want %s", record, found, err, want)
	}
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    1,
	})
	if err != nil || page.Next != 1 || len(page.Items) != 1 || string(page.Items[0].Payload) != string(want) {
		t.Fatalf("List() = %+v, %v", page, err)
	}
	// 3. 删除 Stream，检查空日志及重新追加从序号 1 开始。
	if err := logs.Delete(t.Context(), path, streamID); err != nil {
		t.Fatal(err)
	}
	page, err = logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    10,
	})
	if err != nil || page.Next != 0 || len(page.Items) != 0 {
		t.Fatalf("List() after Delete = %+v, %v", page, err)
	}
	if infos, err := logs.Append(t.Context(), path, streamID, want); err != nil || !slices.Equal(toOrdinals(infos), []storage.Ordinal{1}) {
		t.Fatalf("Append() after Delete = %v, %v, want [1]", infos, err)
	}
}

// toOrdinals 提取追加结果的序号。
func toOrdinals(infos []storage.LogEntryInfo) []storage.Ordinal {
	// 1. 按输入顺序收集 Ordinal。
	ordinals := make([]storage.Ordinal, len(infos))
	for index, info := range infos {
		ordinals[index] = info.Ordinal
	}
	return ordinals
}
