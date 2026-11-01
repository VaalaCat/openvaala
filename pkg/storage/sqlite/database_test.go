package sqlite

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// openTestDatabase 打开测试用落盘库。
func openTestDatabase(t *testing.T) Database {
	t.Helper()
	// 1. 在临时目录打开声明了 agents、turns 两个分组的落盘库，登记测试结束时关闭。
	path := filepath.Join(t.TempDir(), "records.db")
	database, err := OpenDatabase(NewDatabaseOpt(path, storage.RecordNamespace{Name: "agents"}, storage.RecordNamespace{Name: "turns"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	return database
}

// openTestMemoryDatabase 打开带指定记录分组的测试内存库。
func openTestMemoryDatabase(t *testing.T, namespaces ...string) Database {
	t.Helper()
	// 1. 将名称转换为分组声明，打开数据库并登记关闭操作。
	declarations := make([]storage.RecordNamespace, len(namespaces))
	for index, name := range namespaces {
		declarations[index] = storage.RecordNamespace{Name: name}
	}
	database, err := OpenDatabase(NewMemoryDatabaseOpt(declarations...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	return database
}

// TestOpenDatabaseRejectsInvalidOptions 检查数据库拒绝无效配置。
func TestOpenDatabaseRejectsInvalidOptions(t *testing.T) {
	// 1. 准备路径、等待时间和分组名称无效的配置。
	path := filepath.Join(t.TempDir(), "records.db")
	tests := []struct {
		name string
		opt  *DatabaseOpt
	}{
		{name: "nil_options"},
		{
			name: "empty_path",
			opt:  NewDatabaseOpt(""),
		},
		{
			name: "negative_busy_timeout",
			opt: &DatabaseOpt{
				Path:        path,
				BusyTimeout: -time.Second,
			},
		},
		{
			name: "empty_namespace",
			opt:  NewDatabaseOpt(path, storage.RecordNamespace{Name: ""}),
		},
		{
			name: "uppercase_namespace",
			opt:  NewDatabaseOpt(path, storage.RecordNamespace{Name: "Agents"}),
		},
		{
			name: "dash_namespace",
			opt:  NewDatabaseOpt(path, storage.RecordNamespace{Name: "agents-x"}),
		},
		{
			name: "injection_namespace",
			opt:  NewDatabaseOpt(path, storage.RecordNamespace{Name: "agents;drop"}),
		},
		{
			name: "memory_negative_busy_timeout",
			opt: &DatabaseOpt{
				IsInMemory:  true,
				BusyTimeout: -time.Second,
			},
		},
		{
			name: "duplicate_namespace",
			opt:  NewMemoryDatabaseOpt(storage.RecordNamespace{Name: "agents"}, storage.RecordNamespace{Name: "agents"}),
		},
		{
			name: "memory_uppercase_namespace",
			opt:  NewMemoryDatabaseOpt(storage.RecordNamespace{Name: "Agents"}),
		},
	}
	// 2. 逐项打开数据库并检查返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, err := OpenDatabase(tt.opt)
			if err == nil {
				_ = database.Close()
				t.Fatal("OpenDatabase() accepted invalid options")
			}
		})
	}
}

// TestOpenDatabaseCreatesDeclaredNamespaces 检查打开和重新打开数据库后的表。
func TestOpenDatabaseCreatesDeclaredNamespaces(t *testing.T) {
	// 1. 准备未声明和已声明记录分组的配置。
	tests := []struct {
		name       string
		namespaces []storage.RecordNamespace
		wantTables []string
	}{
		{
			name:       "no_namespaces",
			wantTables: []string{"logs"},
		},
		{
			name:       "declared_namespaces",
			namespaces: []storage.RecordNamespace{{Name: "turns"}, {Name: "agents"}},
			wantTables: []string{"agents", "logs", "turns"},
		},
	}
	// 2. 两次打开数据库并检查记录表和日志表。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "records.db")
			for range 2 {
				database, err := OpenDatabase(NewDatabaseOpt(path, tt.namespaces...))
				if err != nil {
					t.Fatal(err)
				}
				rows, err := database.(*databaseImpl).db.Query(`
					SELECT name FROM sqlite_schema
					WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
					ORDER BY name
				`)
				if err != nil {
					t.Fatal(err)
				}
				tables := []string{}
				for rows.Next() {
					var name string
					if err := rows.Scan(&name); err != nil {
						t.Fatal(err)
					}
					tables = append(tables, name)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(tables, tt.wantTables) {
					t.Fatalf("tables = %v, want %v", tables, tt.wantTables)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestMemoryDatabase 检查内存库读写、实例隔离及关闭后的数据。
func TestMemoryDatabase(t *testing.T) {
	// 1. 准备记录、日志和三个内存库场景。
	record := storage.Record{
		Key:       "agent-a",
		UpdatedAt: 1000,
		Payload:   json.RawMessage(`{"name":"alpha"}`),
	}
	const path, streamID = storage.StreamPath("conversation-a"), storage.StreamID("session-a")
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "written_record_and_log_can_be_read",
			run: func(t *testing.T) {
				database := openTestMemoryDatabase(t, "agents")
				if err := database.Record().Set(t.Context(), "agents", record); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Log().Append(t.Context(), path, streamID, record.Payload); err != nil {
					t.Fatal(err)
				}
				got, found, err := database.Record().Get(t.Context(), "agents", record.Key)
				if err != nil || !found || string(got.Payload) != string(record.Payload) || got.UpdatedAt != record.UpdatedAt {
					t.Fatalf("Get() = %+v, %v, %v, want %+v", got, found, err, record)
				}
				page, err := database.Log().List(t.Context(), storage.ListLogEntriesRequest{
					Path:     path,
					StreamID: streamID,
					Limit:    10,
				})
				if err != nil || len(page.Items) != 1 || string(page.Items[0].Payload) != string(record.Payload) {
					t.Fatalf("List() = %+v, %v", page, err)
				}
			},
		},
		{
			name: "databases_open_at_the_same_time_do_not_see_each_other",
			run: func(t *testing.T) {
				first := openTestMemoryDatabase(t, "agents")
				second := openTestMemoryDatabase(t, "agents")
				if err := first.Record().Set(t.Context(), "agents", record); err != nil {
					t.Fatal(err)
				}
				if _, err := first.Log().Append(t.Context(), path, streamID, record.Payload); err != nil {
					t.Fatal(err)
				}
				if _, found, err := second.Record().Get(t.Context(), "agents", record.Key); err != nil || found {
					t.Fatalf("second Get() found=%v err=%v, want not found", found, err)
				}
				page, err := second.Log().List(t.Context(), storage.ListLogEntriesRequest{
					Path:     path,
					StreamID: streamID,
					Limit:    10,
				})
				if err != nil || len(page.Items) != 0 {
					t.Fatalf("second List() = %+v, %v, want empty", page, err)
				}
				if paths, err := second.Log().GetStreamPaths(t.Context(), streamID); err != nil || len(paths) != 0 {
					t.Fatalf("second GetStreamPaths() = %v, %v, want empty", paths, err)
				}
				if _, found, err := first.Record().Get(t.Context(), "agents", record.Key); err != nil || !found {
					t.Fatalf("first Get() found=%v err=%v, want found", found, err)
				}
			},
		},
		{
			name: "reopened_database_is_empty",
			run: func(t *testing.T) {
				first, err := OpenDatabase(NewMemoryDatabaseOpt(storage.RecordNamespace{Name: "agents"}))
				if err != nil {
					t.Fatal(err)
				}
				if err := first.Record().Set(t.Context(), "agents", record); err != nil {
					t.Fatal(err)
				}
				if _, err := first.Log().Append(t.Context(), path, streamID, record.Payload); err != nil {
					t.Fatal(err)
				}
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}

				second := openTestMemoryDatabase(t, "agents")
				if _, found, err := second.Record().Get(t.Context(), "agents", record.Key); err != nil || found {
					t.Fatalf("Get() found=%v err=%v, want not found", found, err)
				}
				page, err := second.Log().List(t.Context(), storage.ListLogEntriesRequest{
					Path:     path,
					StreamID: streamID,
					Limit:    10,
				})
				if err != nil || len(page.Items) != 0 {
					t.Fatalf("List() = %+v, %v, want empty", page, err)
				}
			},
		},
	}
	// 2. 分别检查记录日志读写、两个库互不可见和重新打开为空。
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestOpenDatabaseRecordFields 检查数据库拒绝无效字段声明。
func TestOpenDatabaseRecordFields(t *testing.T) {
	// 1. 准备空字段名、空路径、重复字段和未声明索引。
	tests := []struct {
		name      string
		namespace storage.RecordNamespace
		wantError string
	}{
		{
			name: "empty_field_name",
			namespace: storage.RecordNamespace{
				Name:   "agents",
				Fields: []storage.RecordField{{PayloadJSONPath: []string{"name"}}},
			},
			wantError: "requires a name and payload JSON path",
		},
		{
			name: "empty_path",
			namespace: storage.RecordNamespace{
				Name:   "agents",
				Fields: []storage.RecordField{{Name: "name"}},
			},
			wantError: "requires a name and payload JSON path",
		},
		{
			name: "duplicate_field",
			namespace: storage.RecordNamespace{
				Name: "agents",
				Fields: []storage.RecordField{{
					Name:            "name",
					PayloadJSONPath: []string{"a"},
				}, {
					Name:            "name",
					PayloadJSONPath: []string{"b"},
				}},
			},
			wantError: "duplicate record field",
		},
		{
			name: "undeclared_index",
			namespace: storage.RecordNamespace{
				Name:          "agents",
				IndexedFields: []string{"missing"},
			},
			wantError: "unknown indexed field",
		},
	}
	// 2. 检查打开失败及错误内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, err := OpenDatabase(NewMemoryDatabaseOpt(tt.namespace))
			if err == nil {
				_ = database.Close()
				t.Fatal("OpenDatabase accepted invalid fields")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("OpenDatabase = %v, want %q", err, tt.wantError)
			}
		})
	}
}

// TestRecordFieldLiteralPathAcrossReopen 检查字段路径按 JSON 对象键读取。
func TestRecordFieldLiteralPathAcrossReopen(t *testing.T) {
	// 1. 逐个检查带特殊字符的对象键，写入两种 HTML 编码后关闭再打开，检查两条记录均匹配。
	for _, key := range []string{"directory", "a.b", `quote"key`, "apostrophe'key", "[0]", "*", "目录", "<tag>&", "", `slash\key`, `a.b"';DROP TABLE agents;--`} {
		t.Run(key, func(t *testing.T) {
			field := storage.RecordField{
				Name:            `field"name`,
				PayloadJSONPath: []string{key, "value"},
			}
			namespace := storage.RecordNamespace{
				Name:          "agents",
				Fields:        []storage.RecordField{field},
				IndexedFields: []string{field.Name},
			}
			path := filepath.Join(t.TempDir(), "records.db")
			for attempt := range 2 {
				database, err := OpenDatabase(NewDatabaseOpt(path, namespace))
				if err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					payload, err := json.Marshal(map[string]any{key: map[string]string{"value": "match"}})
					if err != nil {
						t.Fatal(err)
					}
					if err := database.Record().Set(t.Context(), "agents", storage.Record{
						Key:       "a",
						UpdatedAt: 1,
						Payload:   payload,
					}); err != nil {
						t.Fatal(err)
					}
					// 同一个键也可以不转义 HTML 字符；字段查询不能依赖 JSON 的具体编码写法。
					rawHTML := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(payload))
					if err := database.Record().Set(t.Context(), "agents", storage.Record{
						Key:       "b",
						UpdatedAt: 2,
						Payload:   json.RawMessage(rawHTML),
					}); err != nil {
						t.Fatal(err)
					}
				}
				page, err := database.Record().List(t.Context(), storage.ListRecordsRequest{
					Namespace: "agents",
					Limit:     10,
					Filter: storage.RecordFilter{Conditions: []storage.RecordFilterCondition{{
						Field:    field.Name,
						Operator: storage.RecordFilterIn,
						Values:   []string{"match"},
					}}},
				})
				if err != nil || len(page.Items) != 2 || page.Items[0].Key != "a" || page.Items[1].Key != "b" {
					t.Errorf("attempt %d List = %+v, %v, want keys a, b", attempt, page, err)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
