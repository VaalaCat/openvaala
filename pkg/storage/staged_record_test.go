package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/storage"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
)

// TestStagedRecordStorage 检查记录的暂存、搬运和失败重试。
func TestStagedRecordStorage(t *testing.T) {
	// 1. 准备暂存与直接写入的数据及对应场景。
	written := map[string]storage.Record{
		"sessions": {
			Key:       "conversation-a/session-a",
			UpdatedAt: 100,
			Payload:   json.RawMessage(`{"name":"session"}`),
		},
		"conversations": {
			Key:       "conversation-a",
			UpdatedAt: 200,
			Payload:   json.RawMessage(`{"name":"conversation"}`),
		},
		"turns": {
			Key:       "session-a/turn-a",
			UpdatedAt: 300,
			Payload:   json.RawMessage(`{"name":"turn"}`),
		},
	}
	tests := []struct {
		name string
		run  func(t *testing.T, staged *storage.StagedRecordStorage, staging storagesqlite.Database, target *faultyRecordStorage)
	}{
		{
			name: "staged_namespaces_stay_in_staging",
			run: func(t *testing.T, staged *storage.StagedRecordStorage, staging storagesqlite.Database, target *faultyRecordStorage) {
				requireRecords(t, staging.Record(), written, "sessions", "conversations")
				requireNoRecords(t, staging.Record(), "turns")
				requireRecords(t, target, written, "turns")
				requireNoRecords(t, target, "sessions", "conversations")
				requireRecords(t, staged, written, "sessions", "conversations", "turns")
			},
		},
		{
			name: "persist_moves_in_given_order",
			run: func(t *testing.T, staged *storage.StagedRecordStorage, staging storagesqlite.Database, target *faultyRecordStorage) {
				if err := staged.Persist(t.Context()); err != nil {
					t.Fatal(err)
				}
				if !staged.IsPersisted() || !slices.Equal(target.persistedNamespaces, []string{"sessions", "conversations"}) {
					t.Fatalf("IsPersisted() = %v, MSet order = %v; want persisted sessions then conversations", staged.IsPersisted(), target.persistedNamespaces)
				}
				requireRecords(t, target, written, "sessions", "conversations", "turns")
				// 关掉 staging 后仍能读写暂存的 namespace：Persist 之后不再碰 staging。
				if err := staging.Close(); err != nil {
					t.Fatal(err)
				}
				renamed := storage.Record{
					Key:       "conversation-a",
					UpdatedAt: 400,
					Payload:   json.RawMessage(`{"name":"renamed"}`),
				}
				if err := staged.Set(t.Context(), "conversations", renamed); err != nil {
					t.Fatal(err)
				}
				requireRecords(t, staged, map[string]storage.Record{"conversations": renamed}, "conversations")
				if err := staged.Persist(t.Context()); err != nil {
					t.Fatalf("second Persist() = %v, want nil", err)
				}
			},
		},
		{
			name: "filter_before_and_after_persist",
			run: func(t *testing.T, staged *storage.StagedRecordStorage, staging storagesqlite.Database, target *faultyRecordStorage) {
				request := storage.ListRecordsRequest{
					Namespace: "conversations",
					Limit:     1,
					Filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
						Field:    "name",
						Operator: storage.RecordFilterContains,
						Values:   []string{"conversation"},
					}}},
				}
				for attempt := range 2 {
					page, err := staged.List(t.Context(), request)
					if err != nil || len(page.Items) != 1 || page.Items[0].Key != "conversation-a" || page.Next != (storage.RecordCursor{}) {
						t.Fatalf("attempt %d filtered list = %+v, %v", attempt, page, err)
					}
					if err := staged.Persist(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "failed_persist_keeps_staging",
			run: func(t *testing.T, staged *storage.StagedRecordStorage, staging storagesqlite.Database, target *faultyRecordStorage) {
				target.failingNamespace = "conversations"
				if err := staged.Persist(t.Context()); err == nil || !strings.Contains(err.Error(), `persist staged namespace "conversations"`) {
					t.Fatalf("Persist() = %v, want the conversations namespace error", err)
				}
				if staged.IsPersisted() {
					t.Fatal("IsPersisted() = true after a failed Persist")
				}
				requireRecords(t, target, written, "sessions")
				requireNoRecords(t, target, "conversations")
				requireRecords(t, staging.Record(), written, "sessions", "conversations")
				target.failingNamespace = ""
				if err := staged.Persist(t.Context()); err != nil {
					t.Fatal(err)
				}
				requireRecords(t, target, written, "sessions", "conversations", "turns")
			},
		},
	}
	// 2. 创建暂存和目标存储，写入数据后检查搬运顺序、读写切换和重试。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staging := openStagingDatabase(t)
			target := &faultyRecordStorage{RecordStorage: openStagingDatabase(t).Record()}
			staged := storage.NewStagedRecordStorage(staging.Record(), target, "sessions", "conversations")
			for namespace, record := range written {
				if err := staged.Set(t.Context(), namespace, record); err != nil {
					t.Fatal(err)
				}
			}
			tt.run(t, staged, staging, target)
		})
	}
}

// faultyRecordStorage 记下 MSet 写过的 namespace；failingNamespace 非空时，MSet 这个 namespace 返回错误。
type faultyRecordStorage struct {
	storage.RecordStorage
	failingNamespace    string
	persistedNamespaces []string
}

// MSet 记录批量写入分组并按分组注入错误。
func (s *faultyRecordStorage) MSet(ctx context.Context, namespace string, records []storage.Record) error {
	// 1. 命中失败分组时返回错误，否则记录分组名称并调用底层批量写入。
	if namespace == s.failingNamespace {
		return errors.New("target is unavailable")
	}
	s.persistedNamespaces = append(s.persistedNamespaces, namespace)
	return s.RecordStorage.MSet(ctx, namespace, records)
}

// openStagingDatabase 打开测试暂存内存库。
func openStagingDatabase(t *testing.T) storagesqlite.Database {
	t.Helper()
	// 1. 声明 sessions、conversations、turns，为 conversations 的 name 字段建立索引，并登记测试结束时关闭数据库。
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewMemoryDatabaseOpt(
		storage.RecordNamespace{Name: "sessions"},
		storage.RecordNamespace{
			Name: "conversations",
			Fields: []storage.RecordField{{
				Name:            "name",
				PayloadJSONPath: []string{"name"},
			}},
			IndexedFields: []string{"name"},
		},
		storage.RecordNamespace{Name: "turns"},
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// requireRecords 检查各分组只有指定记录，且按 Key 能完整读取。
func requireRecords(t *testing.T, records storage.RecordStorage, want map[string]storage.Record, namespaces ...string) {
	t.Helper()
	// 1. 逐组查询指定 Key 并列出记录，比较时间、载荷和记录数量。
	for _, namespace := range namespaces {
		record := want[namespace]
		got, found, err := records.Get(t.Context(), namespace, record.Key)
		if err != nil || !found || got.UpdatedAt != record.UpdatedAt || string(got.Payload) != string(record.Payload) {
			t.Fatalf("Get(%s, %s) = %+v, %v, %v, want %+v", namespace, record.Key, got, found, err, record)
		}
		page, err := records.List(t.Context(), storage.ListRecordsRequest{
			Namespace: namespace,
			Limit:     10,
		})
		if err != nil || len(page.Items) != 1 || page.Items[0].Key != record.Key {
			t.Fatalf("List(%s) = %+v, %v, want only %s", namespace, page.Items, err, record.Key)
		}
	}
}

// requireNoRecords 检查指定分组都没有记录。
func requireNoRecords(t *testing.T, records storage.RecordStorage, namespaces ...string) {
	t.Helper()
	// 1. 逐组列出记录，要求结果为空且没有错误。
	for _, namespace := range namespaces {
		page, err := records.List(t.Context(), storage.ListRecordsRequest{
			Namespace: namespace,
			Limit:     10,
		})
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("List(%s) = %+v, %v, want empty", namespace, page.Items, err)
		}
	}
}
