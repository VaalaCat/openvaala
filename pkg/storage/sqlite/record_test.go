package sqlite

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestRecordStorageSetAndGet 检查记录覆盖时保留传入时间和载荷。
func TestRecordStorageSetAndGet(t *testing.T) {
	// 1. 准备相同 Key 的前后两条记录。
	tests := []struct {
		name   string
		first  storage.Record
		second storage.Record
	}{
		{
			name: "later_time_replaces_record",
			first: storage.Record{
				Key:       "agent-a",
				UpdatedAt: 100,
				Payload:   json.RawMessage(`{"name":"alpha"}`),
			},
			second: storage.Record{
				Key:       "agent-a",
				UpdatedAt: 200,
				Payload:   json.RawMessage(`{"name":"beta","score":2}`),
			},
		},
		{
			name: "earlier_time_is_kept_as_given",
			first: storage.Record{
				Key:       "agent-a",
				UpdatedAt: 200,
				Payload:   json.RawMessage(`{"name":"alpha"}`),
			},
			second: storage.Record{
				Key:       "agent-a",
				UpdatedAt: 100,
				Payload:   json.RawMessage(`{"name":"alpha"}`),
			},
		},
	}
	// 2. 先后写入并回读，检查以后一条为准。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records := openTestDatabase(t).Record()
			if err := records.Set(t.Context(), "agents", tt.first); err != nil {
				t.Fatal(err)
			}
			if err := records.Set(t.Context(), "agents", tt.second); err != nil {
				t.Fatal(err)
			}
			got, found, err := records.Get(t.Context(), "agents", "agent-a")
			if err != nil || !found || string(got.Payload) != string(tt.second.Payload) || got.UpdatedAt != tt.second.UpdatedAt {
				t.Fatalf("Get() = %+v, %v, %v, want %+v", got, found, err, tt.second)
			}
		})
	}
}

// TestRecordStorageMissingAndDelete 检查缺失记录与记录删除。
func TestRecordStorageMissingAndDelete(t *testing.T) {
	// 1. 查询并删除缺失 Key，再写入、删除并检查记录不存在。
	records := openTestDatabase(t).Record()
	if got, found, err := records.Get(t.Context(), "agents", "missing"); err != nil || found || got.Key != "" {
		t.Fatalf("missing Get() = %+v, %v, %v", got, found, err)
	}
	if err := records.Delete(t.Context(), "agents", "missing"); err != nil {
		t.Fatal(err)
	}
	if err := records.Set(t.Context(), "agents", storage.Record{
		Key:       "agent-a",
		UpdatedAt: 1000,
		Payload:   json.RawMessage(`{"name":"A"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := records.Delete(t.Context(), "agents", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := records.Get(t.Context(), "agents", "agent-a"); err != nil || found {
		t.Fatalf("deleted Get() found=%v err=%v", found, err)
	}
}

// TestRecordStorageMGetAndMSet 检查批量记录读写。
func TestRecordStorageMGetAndMSet(t *testing.T) {
	// 1. 准备两条已保存记录及缺失、重复和空 Key 列表。
	records := openTestDatabase(t).Record()
	written := map[storage.RecordKey]storage.Record{
		"agent-a": {
			Key:       "agent-a",
			UpdatedAt: 300,
			Payload:   json.RawMessage(`{"name":"A"}`),
		},
		"agent-b": {
			Key:       "agent-b",
			UpdatedAt: 100,
			Payload:   json.RawMessage(`{"name":"B"}`),
		},
	}
	if err := records.MSet(t.Context(), "agents", []storage.Record{written["agent-a"], written["agent-b"]}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		keys     []storage.RecordKey
		wantKeys []storage.RecordKey
	}{
		{
			name:     "found_missing_and_duplicate",
			keys:     []storage.RecordKey{"agent-a", "missing", "agent-a", "agent-b"},
			wantKeys: []storage.RecordKey{"agent-a", "agent-b"},
		},
		{
			name:     "only_missing",
			keys:     []storage.RecordKey{"missing"},
			wantKeys: []storage.RecordKey{},
		},
		{
			name:     "no_keys",
			wantKeys: []storage.RecordKey{},
		},
	}
	// 2. 批量读取并检查找到的记录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := records.MGet(t.Context(), "agents", tt.keys)
			if err != nil || got == nil || len(got) != len(tt.wantKeys) {
				t.Fatalf("MGet() = %+v, %v", got, err)
			}
			for _, key := range tt.wantKeys {
				if string(got[key].Payload) != string(written[key].Payload) || got[key].Key != key || got[key].UpdatedAt != written[key].UpdatedAt {
					t.Fatalf("MGet()[%q] = %+v, want %+v", key, got[key], written[key])
				}
			}
		})
	}

	if err := records.MSet(t.Context(), "agents", nil); err != nil {
		t.Fatalf("empty MSet() = %v", err)
	}

	// 同一批里重复的 Key 以后一条为准。
	duplicates := []storage.Record{
		{
			Key:       "agent-c",
			UpdatedAt: 500,
			Payload:   json.RawMessage(`{"name":"first"}`),
		},
		{
			Key:       "agent-c",
			UpdatedAt: 400,
			Payload:   json.RawMessage(`{"name":"last"}`),
		},
	}
	if err := records.MSet(t.Context(), "agents", duplicates); err != nil {
		t.Fatal(err)
	}
	got, found, err := records.Get(t.Context(), "agents", "agent-c")
	if err != nil || !found || string(got.Payload) != string(duplicates[1].Payload) || got.UpdatedAt != duplicates[1].UpdatedAt {
		t.Fatalf("Get(agent-c) = %+v, %v, %v, want %+v", got, found, err, duplicates[1])
	}
}

// TestRecordStorageMSetRollsBack 检查批量写入中途失败时撤销全部记录。
func TestRecordStorageMSetRollsBack(t *testing.T) {
	// 1. 创建拒绝第二条记录的触发器，批量写入后检查报错且第一条未留下。
	database := openTestDatabase(t)
	records := database.Record()
	if _, err := database.(*databaseImpl).db.Exec(`
		CREATE TRIGGER abort_agent_b BEFORE INSERT ON "agents"
		WHEN NEW.key = 'agent-b'
		BEGIN SELECT RAISE(ABORT, 'blocked'); END
	`); err != nil {
		t.Fatal(err)
	}
	err := records.MSet(t.Context(), "agents", []storage.Record{
		{
			Key:       "agent-a",
			UpdatedAt: 1,
			Payload:   json.RawMessage(`{"name":"A"}`),
		},
		{
			Key:       "agent-b",
			UpdatedAt: 1,
			Payload:   json.RawMessage(`{"name":"B"}`),
		},
	})
	if err == nil {
		t.Fatal("MSet() succeeded despite aborting trigger")
	}
	if _, found, err := records.Get(t.Context(), "agents", "agent-a"); err != nil || found {
		t.Fatalf("rolled back record found=%v err=%v", found, err)
	}
}

// TestRecordStorageList 检查记录按 Key 范围分页。
func TestRecordStorageList(t *testing.T) {
	// 1. 准备多个前缀记录和不同方向、游标、页大小。
	records := openTestDatabase(t).Record()
	seeded := []storage.Record{
		{
			Key:       "session-a/001",
			UpdatedAt: 1000,
			Payload:   json.RawMessage(`{"name":"A1"}`),
		},
		{
			Key:       "session-a/003",
			UpdatedAt: 2000,
			Payload:   json.RawMessage(`{"name":"A3"}`),
		},
		{
			Key:       "session-a/009",
			UpdatedAt: 3000,
			Payload:   json.RawMessage(`{"name":"A9"}`),
		},
		{
			Key:       "session-b/002",
			UpdatedAt: 4000,
			Payload:   json.RawMessage(`{"name":"B2"}`),
		},
	}
	if err := records.MSet(t.Context(), "turns", seeded); err != nil {
		t.Fatal(err)
	}
	updatedAt := make(map[storage.RecordKey]int64, len(seeded))
	for _, record := range seeded {
		updatedAt[record.Key] = record.UpdatedAt
	}

	tests := []struct {
		name     string
		request  storage.ListRecordsRequest
		wantKeys []storage.RecordKey
		wantNext storage.RecordCursor
		wantErr  bool
	}{
		{
			name: "ascending_first_page",
			request: storage.ListRecordsRequest{
				Namespace: "turns",
				Start:     "session-a/",
				End:       "session-a0",
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Limit:     2,
			},
			wantKeys: []storage.RecordKey{"session-a/001", "session-a/003"},
			wantNext: storage.RecordCursor{Key: "session-a/003"},
		},
		{
			name: "ascending_after_cursor_is_last_page",
			request: storage.ListRecordsRequest{
				Namespace: "turns",
				Start:     "session-a/",
				End:       "session-a0",
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Cursor:    storage.RecordCursor{Key: "session-a/003"},
				Limit:     2,
			},
			wantKeys: []storage.RecordKey{"session-a/009"},
		},
		{
			name: "descending_before_cursor",
			request: storage.ListRecordsRequest{
				Namespace: "turns",
				Start:     "session-a/",
				End:       "session-a0",
				Order:     storage.Order{Direction: storage.DirectionDesc},
				Cursor:    storage.RecordCursor{Key: "session-a/009"},
				Limit:     2,
			},
			wantKeys: []storage.RecordKey{"session-a/003", "session-a/001"},
		},
		{
			name: "exact_limit_has_no_next",
			request: storage.ListRecordsRequest{
				Namespace: "turns",
				Order:     storage.Order{Direction: storage.DirectionDesc},
				Limit:     4,
			},
			wantKeys: []storage.RecordKey{"session-b/002", "session-a/009", "session-a/003", "session-a/001"},
		},
		{
			name: "empty_end_has_no_upper_bound",
			request: storage.ListRecordsRequest{
				Namespace: "turns",
				Start:     "session-b/",
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Limit:     2,
			},
			wantKeys: []storage.RecordKey{"session-b/002"},
		},
		{
			name: "cursor_at_end_is_outside_range",
			request: storage.ListRecordsRequest{
				Namespace: "turns",
				Start:     "session-a/",
				End:       "session-a0",
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Cursor:    storage.RecordCursor{Key: "session-a0"},
				Limit:     2,
			},
			wantErr: true,
		},
		{
			name: "limit_cannot_overflow_limit_plus_one",
			request: storage.ListRecordsRequest{
				Namespace: "turns",
				Order:     storage.Order{Direction: storage.DirectionAsc},
				Limit:     math.MaxInt,
			},
			wantErr: true,
		},
	}
	// 2. 检查本页 Key、JSON 载荷、下一页位置和无效请求错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := records.List(t.Context(), tt.request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("List() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			gotKeys := make([]storage.RecordKey, len(page.Items))
			for index, record := range page.Items {
				gotKeys[index] = record.Key
				if !json.Valid(record.Payload) {
					t.Fatalf("List() payload %q is invalid", record.Payload)
				}
			}
			if tt.wantNext.Key != "" {
				tt.wantNext.UpdatedAt = updatedAt[tt.wantNext.Key]
			}
			if !slices.Equal(gotKeys, tt.wantKeys) || page.Next != tt.wantNext {
				t.Fatalf("List() = keys %v, Next %+v, want keys %v, Next %+v", gotKeys, page.Next, tt.wantKeys, tt.wantNext)
			}
		})
	}
}

// TestRecordStorageDeleteRange 检查范围删除只移除指定前缀。
func TestRecordStorageDeleteRange(t *testing.T) {
	// 1. 写入两个前缀，重复删除一个前缀并检查另一前缀仍在。
	records := openTestDatabase(t).Record()
	if err := records.MSet(t.Context(), "turns", []storage.Record{
		{
			Key:       "session-a/001",
			UpdatedAt: 1000,
			Payload:   json.RawMessage(`{"name":"A1"}`),
		},
		{
			Key:       "session-a/003",
			UpdatedAt: 1000,
			Payload:   json.RawMessage(`{"name":"A3"}`),
		},
		{
			Key:       "session-a/009",
			UpdatedAt: 1000,
			Payload:   json.RawMessage(`{"name":"A9"}`),
		},
		{
			Key:       "session-b/002",
			UpdatedAt: 1000,
			Payload:   json.RawMessage(`{"name":"B2"}`),
		},
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := records.DeleteRange(t.Context(), "turns", "session-a/", "session-a0"); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []storage.RecordKey{"session-a/001", "session-a/003", "session-a/009"} {
		if _, found, err := records.Get(t.Context(), "turns", key); err != nil || found {
			t.Fatalf("Get(%q) found=%v err=%v", key, found, err)
		}
	}
	if got, found, err := records.Get(t.Context(), "turns", "session-b/002"); err != nil || !found || string(got.Payload) != `{"name":"B2"}` {
		t.Fatalf("Get(session-b/002) = %+v, found=%v, err=%v", got, found, err)
	}
}

// TestRecordStorageRejectsUnknownNamespace 检查记录接口拒绝未声明分组。
func TestRecordStorageRejectsUnknownNamespace(t *testing.T) {
	// 1. 准备各读写接口对未知分组的调用。
	records := openTestDatabase(t).Record()
	record := storage.Record{
		Key:       "a",
		UpdatedAt: 1000,
		Payload:   json.RawMessage(`{}`),
	}
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "set",
			call: func() error { return records.Set(t.Context(), "unknown", record) },
		},
		{
			name: "mset",
			call: func() error { return records.MSet(t.Context(), "unknown", []storage.Record{record}) },
		},
		{
			name: "get",
			call: func() error { _, _, err := records.Get(t.Context(), "unknown", "a"); return err },
		},
		{
			name: "mget",
			call: func() error { _, err := records.MGet(t.Context(), "unknown", []storage.RecordKey{"a"}); return err },
		},
		{
			name: "delete",
			call: func() error { return records.Delete(t.Context(), "unknown", "a") },
		},
		{
			name: "delete_range",
			call: func() error { return records.DeleteRange(t.Context(), "unknown", "", "") },
		},
		{
			name: "list",
			call: func() error {
				_, err := records.List(t.Context(), storage.ListRecordsRequest{
					Namespace: "unknown",
					Order:     storage.Order{},
					Limit:     1,
				})
				return err
			},
		},
	}
	// 2. 检查每个接口都返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil {
				t.Fatal("unknown namespace error = nil")
			}
		})
	}
}

// TestRecordStorageRejectsInvalidInput 检查记录接口拒绝无效字段和请求。
func TestRecordStorageRejectsInvalidInput(t *testing.T) {
	// 1. 检查空 Key、非正时间、损坏 JSON、无效排序及反向范围均返回错误，且无效记录未写入。
	records := openTestDatabase(t).Record()
	if _, err := records.MGet(t.Context(), "agents", []storage.RecordKey{""}); err == nil {
		t.Fatal("MGet() accepted empty key")
	}
	invalidRecords := []struct {
		name   string
		record storage.Record
	}{
		{
			name: "empty_key",
			record: storage.Record{
				UpdatedAt: 1,
				Payload:   json.RawMessage(`{}`),
			},
		},
		{
			name: "zero_updated_at",
			record: storage.Record{
				Key:     "agent-a",
				Payload: json.RawMessage(`{}`),
			},
		},
		{
			name: "negative_updated_at",
			record: storage.Record{
				Key:       "agent-a",
				UpdatedAt: -1,
				Payload:   json.RawMessage(`{}`),
			},
		},
		{
			name: "nil_payload",
			record: storage.Record{
				Key:       "agent-a",
				UpdatedAt: 1,
			},
		},
		{
			name: "broken_payload",
			record: storage.Record{
				Key:       "agent-a",
				UpdatedAt: 1,
				Payload:   json.RawMessage(`{"broken"`),
			},
		},
	}
	for _, tt := range invalidRecords {
		if err := records.Set(t.Context(), "agents", tt.record); err == nil {
			t.Fatalf("Set(%s) accepted %+v", tt.name, tt.record)
		}
		if err := records.MSet(t.Context(), "agents", []storage.Record{tt.record}); err == nil {
			t.Fatalf("MSet(%s) accepted %+v", tt.name, tt.record)
		}
	}
	if _, found, err := records.Get(t.Context(), "agents", "agent-a"); err != nil || found {
		t.Fatalf("invalid record was written: found=%v err=%v", found, err)
	}
	invalidRequests := []storage.ListRecordsRequest{
		{
			Namespace: "agents",
			Limit:     0,
		},
		{
			Namespace: "agents",
			Order:     storage.Order{Direction: "unsupported"},
			Limit:     1,
		},
		{
			Namespace: "agents",
			Order:     storage.Order{By: "unsupported"},
			Limit:     1,
		},
		{
			Namespace: "agents",
			Start:     "z",
			End:       "a",
			Limit:     1,
		},
	}
	for _, request := range invalidRequests {
		if _, err := records.List(t.Context(), request); err == nil {
			t.Fatalf("List() accepted request %+v", request)
		}
	}
}

// TestRecordStoragePersistsAcrossReopen 检查落盘记录重新打开后仍可读取。
func TestRecordStoragePersistsAcrossReopen(t *testing.T) {
	// 1. 写入记录并关闭数据库，重新打开后检查时间和载荷。
	path := filepath.Join(t.TempDir(), "records.db")
	database, err := OpenDatabase(NewDatabaseOpt(path, storage.RecordNamespace{Name: "agents"}))
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(`{"name":"persistent","score":9}`)
	if err := database.Record().Set(t.Context(), "agents", storage.Record{
		Key:       "agent-a",
		UpdatedAt: 1000,
		Payload:   want,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = OpenDatabase(NewDatabaseOpt(path, storage.RecordNamespace{Name: "agents"}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, found, err := database.Record().Get(t.Context(), "agents", "agent-a")
	if err != nil || !found || string(got.Payload) != string(want) || got.UpdatedAt != 1000 {
		t.Fatalf("reopened Get() = %+v, %v, %v", got, found, err)
	}
}

// TestRecordStorageListByUpdatedAt 检查记录按写入时间分页。
func TestRecordStorageListByUpdatedAt(t *testing.T) {
	// 1. 准备升降序、同时间、翻页期间更新和前缀范围场景。
	tests := []struct {
		name string
		run  func(t *testing.T, records storage.RecordStorage)
	}{
		{
			name: "descending_by_last_write",
			run: func(t *testing.T, records storage.RecordStorage) {
				seedActivity(t, records)
				requireListedKeys(t, records, storage.Order{
					By:        storage.OrderByUpdatedAt,
					Direction: storage.DirectionDesc,
				}, []storage.RecordKey{"a", "c", "d", "b"})
			},
		},
		{
			name: "ascending_by_last_write",
			run: func(t *testing.T, records storage.RecordStorage) {
				seedActivity(t, records)
				requireListedKeys(t, records, storage.Order{
					By:        storage.OrderByUpdatedAt,
					Direction: storage.DirectionAsc,
				}, []storage.RecordKey{"b", "d", "c", "a"})
			},
		},
		{
			name: "same_time_ties_break_by_key",
			run: func(t *testing.T, records storage.RecordStorage) {
				if err := records.MSet(t.Context(), "agents", []storage.Record{
					{
						Key:       "y",
						UpdatedAt: 1000,
						Payload:   json.RawMessage(`{}`),
					},
					{
						Key:       "x",
						UpdatedAt: 1000,
						Payload:   json.RawMessage(`{}`),
					},
				}); err != nil {
					t.Fatal(err)
				}
				requireListedKeys(t, records, storage.Order{
					By:        storage.OrderByUpdatedAt,
					Direction: storage.DirectionDesc,
				}, []storage.RecordKey{"y", "x"})
				requireListedKeys(t, records, storage.Order{
					By:        storage.OrderByUpdatedAt,
					Direction: storage.DirectionAsc,
				}, []storage.RecordKey{"x", "y"})
			},
		},
		{
			name: "updated_between_pages_is_skipped_not_repeated",
			run: func(t *testing.T, records storage.RecordStorage) {
				seedActivity(t, records)
				order := storage.Order{
					By:        storage.OrderByUpdatedAt,
					Direction: storage.DirectionDesc,
				}
				first, err := records.List(t.Context(), storage.ListRecordsRequest{
					Namespace: "agents",
					Order:     order,
					Limit:     2,
				})
				if err != nil || len(first.Items) != 2 || first.Next.Key != "c" || first.Next.UpdatedAt != first.Items[1].UpdatedAt {
					t.Fatalf("first page = %+v, %v", first, err)
				}
				if err := records.Set(t.Context(), "agents", storage.Record{
					Key:       "d",
					UpdatedAt: 4000,
					Payload:   json.RawMessage(`{"name":"D2"}`),
				}); err != nil {
					t.Fatal(err)
				}
				second, err := records.List(t.Context(), storage.ListRecordsRequest{
					Namespace: "agents",
					Order:     order,
					Cursor:    first.Next,
					Limit:     2,
				})
				if err != nil || len(second.Items) != 1 || second.Items[0].Key != "b" || second.Next != (storage.RecordCursor{}) {
					t.Fatalf("second page = %+v, %v, want only b", second, err)
				}
			},
		},
		{
			name: "key_range_pages_by_last_write",
			run: func(t *testing.T, records storage.RecordStorage) {
				// 两个前缀交替写入，p/1 最后写入；按最后写入倒序分页时 q/ 前缀不应出现在任何一页。
				for index, key := range []storage.RecordKey{"p/2", "q/1", "p/3", "q/2", "p/1", "q/3"} {
					record := storage.Record{
						Key:       key,
						UpdatedAt: int64(index+1) * 1000,
						Payload:   json.RawMessage(`{}`),
					}
					if err := records.Set(t.Context(), "agents", record); err != nil {
						t.Fatal(err)
					}
				}
				request := storage.ListRecordsRequest{
					Namespace: "agents",
					Start:     "p/",
					End:       "p0",
					Order: storage.Order{
						By:        storage.OrderByUpdatedAt,
						Direction: storage.DirectionDesc,
					},
					Limit: 2,
				}
				first, err := records.List(t.Context(), request)
				if err != nil || len(first.Items) != 2 || first.Items[0].Key != "p/1" || first.Items[1].Key != "p/3" ||
					first.Next != (storage.RecordCursor{
						Key:       "p/3",
						UpdatedAt: first.Items[1].UpdatedAt,
					}) {
					t.Fatalf("first page = %+v, %v", first, err)
				}
				request.Cursor = first.Next
				second, err := records.List(t.Context(), request)
				if err != nil || len(second.Items) != 1 || second.Items[0].Key != "p/2" || second.Next != (storage.RecordCursor{}) {
					t.Fatalf("second page = %+v, %v, want only p/2", second, err)
				}
			},
		},
	}
	// 2. 检查时间排序、同时间按 Key 排序和游标后的记录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, openTestDatabase(t).Record())
		})
	}
}

// seedActivity 写入排序测试的四条记录。
func seedActivity(t *testing.T, records storage.RecordStorage) {
	t.Helper()
	// 1. 写入 b、d 的同一时间，再写入时间更晚的 c 和 a；时间倒序结果为 a、c、d、b。
	if err := records.MSet(t.Context(), "agents", []storage.Record{
		{
			Key:       "b",
			UpdatedAt: 1000,
			Payload:   json.RawMessage(`{}`),
		},
		{
			Key:       "d",
			UpdatedAt: 1000,
			Payload:   json.RawMessage(`{}`),
		},
		{
			Key:       "c",
			UpdatedAt: 2000,
			Payload:   json.RawMessage(`{}`),
		},
		{
			Key:       "a",
			UpdatedAt: 3000,
			Payload:   json.RawMessage(`{}`),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// requireListedKeys 逐页检查 agents 的 Key 顺序。
func requireListedKeys(t *testing.T, records storage.RecordStorage, order storage.Order, want []storage.RecordKey) {
	t.Helper()
	// 1. 每页读取一条，按返回游标收集全部 Key。
	var got []storage.RecordKey
	var cursor storage.RecordCursor
	for {
		page, err := records.List(t.Context(), storage.ListRecordsRequest{
			Namespace: "agents",
			Order:     order,
			Cursor:    cursor,
			Limit:     1,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Items {
			got = append(got, record.Key)
		}
		if page.Next == (storage.RecordCursor{}) {
			break
		}
		cursor = page.Next
	}
	// 2. 将完整 Key 序列与期望比较。
	if !slices.Equal(got, want) {
		t.Fatalf("keys by %+v = %v, want %v", order, got, want)
	}
}

// openFilterDatabase 打开声明目录和标题字段的测试内存库。
func openFilterDatabase(t testing.TB) *databaseImpl {
	t.Helper()
	// 1. 为嵌套目录字段建立联合索引，打开数据库并登记关闭。
	database, err := OpenDatabase(NewMemoryDatabaseOpt(storage.RecordNamespace{
		Name: "conversations",
		Fields: []storage.RecordField{
			{
				Name:            "directory",
				PayloadJSONPath: []string{"workspace", "directory"},
			},
			{
				Name:            "title",
				PayloadJSONPath: []string{"title"},
			},
		},
		IndexedFields: []string{"directory"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database.(*databaseImpl)
}

// TestRecordStorageFilter 检查字符串字段筛选。
func TestRecordStorageFilter(t *testing.T) {
	// 1. 准备多种 JSON 值类型及 AND、OR、in、contains 和无效条件。
	database := openFilterDatabase(t)
	records := database.Record()
	seed := []storage.Record{
		{
			Key:       "a",
			UpdatedAt: 30,
			Payload:   json.RawMessage(`{"workspace":{"directory":"/a"},"title":"fix bug"}`),
		},
		{
			Key:       "b",
			UpdatedAt: 20,
			Payload:   json.RawMessage(`{"workspace":{"directory":"/b"},"title":"fix BUG"}`),
		},
		{
			Key:       "c",
			UpdatedAt: 30,
			Payload:   json.RawMessage(`{"workspace":{"directory":"/a"},"title":"bug %_"}`),
		},
		{
			Key:       "d",
			UpdatedAt: 10,
			Payload:   json.RawMessage(`{"workspace":{"directory":"/a/subdir"},"title":"ordinary"}`),
		},
		{
			Key:       "e",
			UpdatedAt: 40,
			Payload:   json.RawMessage(`{"workspace":{"directory":123},"title":true}`),
		},
		{
			Key:       "f",
			UpdatedAt: 50,
			Payload:   json.RawMessage(`{"workspace":{"directory":null},"title":null}`),
		},
		{
			Key:       "g",
			UpdatedAt: 60,
			Payload:   json.RawMessage(`{}`),
		},
		{
			Key:       "h",
			UpdatedAt: 70,
			Payload:   json.RawMessage(`{"workspace":{"directory":{}},"title":[]}`),
		},
	}
	if err := records.MSet(t.Context(), "conversations", seed); err != nil {
		t.Fatal(err)
	}
	directory := storage.RecordFilterCondition{
		Field:    "directory",
		Operator: storage.RecordFilterIn,
		Values:   []string{"/a"},
	}
	title := storage.RecordFilterCondition{
		Field:    "title",
		Operator: storage.RecordFilterContains,
		Values:   []string{"BUG"},
	}
	tests := []struct {
		name      string
		filter    storage.RecordFilter
		wantKeys  []storage.RecordKey
		wantError string
	}{
		{
			name:     "empty",
			wantKeys: []storage.RecordKey{"a", "b", "c", "d", "e", "f", "g", "h"},
		},
		{
			name:     "in_exact",
			filter:   storage.RecordFilter{Conditions: []storage.RecordFilterCondition{directory}},
			wantKeys: []storage.RecordKey{"a", "c"},
		},
		{
			name: "in_multiple",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "directory",
				Operator: storage.RecordFilterIn,
				Values:   []string{"/a", "/b"},
			}}},
			wantKeys: []storage.RecordKey{"a", "b", "c"},
		},
		{
			name: "in_case_sensitive",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "directory",
				Operator: storage.RecordFilterIn,
				Values:   []string{"/A"},
			}}},
		},
		{
			name:     "contains_case_sensitive",
			filter:   storage.RecordFilter{Conditions: []storage.RecordFilterCondition{title}},
			wantKeys: []storage.RecordKey{"b"},
		},
		{
			name: "contains_literal_wildcards",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "title",
				Operator: storage.RecordFilterContains,
				Values:   []string{"%_"},
			}}},
			wantKeys: []storage.RecordKey{"c"},
		},
		{
			name: "contains_empty_matches_only_strings",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "title",
				Operator: storage.RecordFilterContains,
				Values:   []string{""},
			}}},
			wantKeys: []storage.RecordKey{"a", "b", "c", "d"},
		},
		{
			name: "in_does_not_convert_numbers",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "directory",
				Operator: storage.RecordFilterIn,
				Values:   []string{"123", "null", "{}"},
			}}},
		},
		{
			name:   "default_and",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{directory, title}},
		},
		{
			name: "explicit_and",
			filter: storage.RecordFilter{
				Logic:      storage.RecordFilterAnd,
				Conditions: []storage.RecordFilterCondition{directory, title},
			},
		},
		{
			name: "or",
			filter: storage.RecordFilter{
				Logic:      storage.RecordFilterOr,
				Conditions: []storage.RecordFilterCondition{directory, title},
			},
			wantKeys: []storage.RecordKey{"a", "b", "c"},
		},
		{
			name: "value_is_not_sql",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "directory",
				Operator: storage.RecordFilterIn,
				Values:   []string{"/a') OR 1=1 --"},
			}}},
		},
		{
			name: "unknown_field",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "missing",
				Operator: storage.RecordFilterIn,
				Values:   []string{"a"},
			}}},
			wantError: `unknown filter field "missing"`,
		},
		{
			name: "unknown_logic",
			filter: storage.RecordFilter{
				Logic:      "xor",
				Conditions: []storage.RecordFilterCondition{directory},
			},
			wantError: `unsupported filter logic "xor"`,
		},
		{
			name: "unknown_operator",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "directory",
				Operator: "eq",
				Values:   []string{"a"},
			}}},
			wantError: `unsupported filter operator "eq"`,
		},
		{
			name: "in_no_values",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "directory",
				Operator: storage.RecordFilterIn,
			}}},
			wantError: `filter "directory" requires at least one value`,
		},
		{
			name: "contains_no_values",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "title",
				Operator: storage.RecordFilterContains,
			}}},
			wantError: `filter "title" requires exactly one value`,
		},
		{
			name: "contains_many_values",
			filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
				Field:    "title",
				Operator: storage.RecordFilterContains,
				Values:   []string{"a", "b"},
			}}},
			wantError: `filter "title" requires exactly one value`,
		},
	}
	// 2. 检查匹配记录、空结果及错误内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := records.List(t.Context(), storage.ListRecordsRequest{
				Namespace: "conversations",
				Filter:    tt.filter,
				Limit:     20,
			})
			if tt.wantError != "" {
				if err == nil || err.Error() != tt.wantError {
					t.Fatalf("List error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var keys []storage.RecordKey
			for _, record := range page.Items {
				keys = append(keys, record.Key)
			}
			if !slices.Equal(keys, tt.wantKeys) || page.Next != (storage.RecordCursor{}) {
				t.Fatalf("List = %v, next %+v, want %v, no next page", keys, page.Next, tt.wantKeys)
			}
		})
	}
}

// TestRecordStorageFilteredPagination 检查字段筛选与范围、排序和分页共同生效。
func TestRecordStorageFilteredPagination(t *testing.T) {
	// 1. 准备多个目录、相同写入时间及四种排序。
	records := openFilterDatabase(t).Record()
	for index, directory := range []string{"/a", "/skip", "/a", "/b", "/a", "/b", "/a"} {
		payload, _ := json.Marshal(map[string]any{"workspace": map[string]string{"directory": directory}, "title": "BUG"})
		if err := records.Set(t.Context(), "conversations", storage.Record{
			Key:       storage.RecordKey(fmt.Sprint(index)),
			UpdatedAt: int64(10 - index/2),
			Payload:   payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		order storage.Order
		want  []storage.RecordKey
	}{
		{
			name:  "key_asc",
			order: storage.Order{Direction: storage.DirectionAsc},
			want:  []storage.RecordKey{"2", "3", "4", "5"},
		},
		{
			name:  "key_desc",
			order: storage.Order{Direction: storage.DirectionDesc},
			want:  []storage.RecordKey{"5", "4", "3", "2"},
		},
		{
			name: "updated_asc",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionAsc,
			},
			want: []storage.RecordKey{"4", "5", "2", "3"},
		},
		{
			name: "updated_desc",
			order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionDesc,
			},
			want: []storage.RecordKey{"3", "2", "5", "4"},
		},
	}
	// 2. 逐页收集 Key，检查筛选结果仍受范围和游标限制。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := storage.ListRecordsRequest{
				Namespace: "conversations",
				Start:     "2",
				End:       "6",
				Order:     tt.order,
				Limit:     1,
				Filter: storage.RecordFilter{
					Logic: storage.RecordFilterOr,
					Conditions: []storage.RecordFilterCondition{
						{
							Field:    "directory",
							Operator: storage.RecordFilterIn,
							Values:   []string{"/a"},
						},
						{
							Field:    "title",
							Operator: storage.RecordFilterContains,
							Values:   []string{"BUG"},
						},
					},
				},
			}
			var keys []storage.RecordKey
			for {
				page, err := records.List(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				for _, record := range page.Items {
					keys = append(keys, record.Key)
				}
				if len(keys) > len(tt.want) {
					t.Fatalf("filter bypassed range or cursor: %v", keys)
				}
				if page.Next == (storage.RecordCursor{}) {
					break
				}
				request.Cursor = page.Next
			}
			if !slices.Equal(keys, tt.want) {
				t.Fatalf("List keys = %v, want %v", keys, tt.want)
			}
		})
	}
}

// TestRecordStorageFilterFollowsWrites 检查字段筛选随记录写入和删除更新。
func TestRecordStorageFilterFollowsWrites(t *testing.T) {
	// 1. 依次插入、移出目录、移回和删除记录，每次检查指定目录的匹配数量。
	records := openFilterDatabase(t).Record()
	for _, tt := range []struct {
		name      string
		payload   string
		wantCount int
	}{
		{
			name:      "insert",
			payload:   `{"workspace":{"directory":"/a"}}`,
			wantCount: 1,
		},
		{
			name:    "move",
			payload: `{"workspace":{"directory":"/b"}}`,
		},
		{
			name:      "move_back",
			payload:   `{"workspace":{"directory":"/a"}}`,
			wantCount: 1,
		},
		{name: "delete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.payload == "" {
				err = records.Delete(t.Context(), "conversations", "a")
			} else {
				err = records.Set(t.Context(), "conversations", storage.Record{
					Key:       "a",
					UpdatedAt: 1,
					Payload:   json.RawMessage(tt.payload),
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			page, err := records.List(t.Context(), storage.ListRecordsRequest{
				Namespace: "conversations",
				Limit:     10,
				Filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
					Field:    "directory",
					Operator: storage.RecordFilterIn,
					Values:   []string{"/a"},
				}}},
			})
			if err != nil || len(page.Items) != tt.wantCount {
				t.Fatalf("List = %+v, %v, want count %d", page, err, tt.wantCount)
			}
		})
	}
}

// TestRecordStorageDirectoryQueryUsesIndex 检查目录筛选查询使用有序索引。
func TestRecordStorageDirectoryQueryUsesIndex(t *testing.T) {
	// 1. 分别检查升序和降序查询计划包含字段索引且没有临时排序。
	database := openFilterDatabase(t)
	for _, direction := range []string{"ASC", "DESC"} {
		t.Run(direction, func(t *testing.T) {
			// 与公开 List 的目录条件相同，检查查找使用索引，不锁定完整的查询计划文本。
			rows, err := database.db.Query(`EXPLAIN QUERY PLAN SELECT key, updated_at, payload FROM conversations
				WHERE json_type(CAST(payload AS TEXT), '$."workspace"."directory"') = 'text'
				AND json_extract(CAST(payload AS TEXT), '$."workspace"."directory"') IN (?)
				ORDER BY updated_at `+direction+`, key `+direction+` LIMIT ?`, "/a", 26)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plans []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plans = append(plans, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(plans, "\n")
			if !strings.Contains(plan, "SEARCH conversations USING INDEX conversations.directory.updated_at") || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("directory query did not use ordered index lookup:\n%s", plan)
			}
		})
	}
}

// BenchmarkRecordStorageDirectoryFilter 比较目录索引筛选和分页扫描的查询次数。
func BenchmarkRecordStorageDirectoryFilter(b *testing.B) {
	// 1. 写入一万条记录，分别测量零匹配和稀疏匹配的两种读取方式，统计每次查询调用数。
	database := openFilterDatabase(b)
	records := database.Record()
	seed := make([]storage.Record, 10000)
	for index := range seed {
		payload, _ := json.Marshal(map[string]any{"workspace": map[string]string{"directory": fmt.Sprintf("/project/%d", index%1000)}, "title": "conversation"})
		seed[index] = storage.Record{
			Key:       storage.RecordKey(fmt.Sprintf("%05d", index)),
			UpdatedAt: int64(index + 1),
			Payload:   payload,
		}
	}
	if err := records.MSet(b.Context(), "conversations", seed); err != nil {
		b.Fatal(err)
	}
	for _, tt := range []struct {
		name        string
		directory   string
		wantMatches int
	}{
		{
			name:      "zero_match",
			directory: "/missing",
		},
		{
			name:        "sparse_match",
			directory:   "/project/0",
			wantMatches: 10,
		},
	} {
		for _, indexed := range []bool{true, false} {
			method := "scan_50"
			if indexed {
				method = "indexed"
			}
			b.Run(tt.name+"/"+method, func(b *testing.B) {
				b.ReportAllocs()
				calls := 0
				for b.Loop() {
					request := storage.ListRecordsRequest{
						Namespace: "conversations",
						Limit:     50,
						Order: storage.Order{
							By:        storage.OrderByUpdatedAt,
							Direction: storage.DirectionDesc,
						},
					}
					if indexed {
						request.Limit = 25
						request.Filter = storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
							Field:    "directory",
							Operator: storage.RecordFilterIn,
							Values:   []string{tt.directory},
						}}}
					}
					matches := 0
					for {
						page, err := records.List(b.Context(), request)
						calls++
						if err != nil {
							b.Fatal(err)
						}
						for _, record := range page.Items {
							if indexed {
								matches++
								continue
							}
							var value struct {
								Workspace struct {
									Directory string `json:"directory"`
								} `json:"workspace"`
							}
							if err := json.Unmarshal(record.Payload, &value); err != nil {
								b.Fatal(err)
							}
							if value.Workspace.Directory == tt.directory {
								matches++
							}
						}
						if matches >= 25 || page.Next == (storage.RecordCursor{}) {
							break
						}
						request.Cursor = page.Next
					}
					if matches != tt.wantMatches {
						b.Fatalf("matches = %d, want %d", matches, tt.wantMatches)
					}
				}
				b.ReportMetric(float64(calls)/float64(b.N), "lists/op")
			})
		}
	}
}
