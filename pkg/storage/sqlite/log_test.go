package sqlite

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// toTestPayloads 将数字编码为测试日志载荷。
func toTestPayloads(values ...int) []json.RawMessage {
	// 1. 依次生成包含 n 字段的 JSON。
	payloads := make([]json.RawMessage, len(values))
	for index, value := range values {
		payloads[index] = json.RawMessage(`{"n":` + strconv.Itoa(value) + `}`)
	}
	return payloads
}

// getTestOrdinals 提取日志项的顺序号。
func getTestOrdinals(entries []storage.LogEntry) []storage.Ordinal {
	// 1. 按输入顺序收集 Ordinal。
	ordinals := make([]storage.Ordinal, len(entries))
	for index, entry := range entries {
		ordinals[index] = entry.Ordinal
	}
	return ordinals
}

// TestLogStorageAppend 检查 SQLite 日志追加的序号和时间。
func TestLogStorageAppend(t *testing.T) {
	// 1. 准备批内连续、跨批连续及不同 Stream 的追加样例。
	type appendCall struct {
		path     storage.StreamPath
		streamID storage.StreamID
		payloads []json.RawMessage
		want     []storage.Ordinal
	}
	tests := []struct {
		name  string
		calls []appendCall
	}{
		{
			name: "batch_of_three_gets_consecutive_ordinals",
			calls: []appendCall{
				{
					path:     "conversation-a",
					streamID: "session-a",
					payloads: toTestPayloads(1, 2, 3),
					want:     []storage.Ordinal{1, 2, 3},
				},
			},
		},
		{
			name: "next_batch_continues_after_previous_batch",
			calls: []appendCall{
				{
					path:     "conversation-a",
					streamID: "session-a",
					payloads: toTestPayloads(1, 2, 3),
					want:     []storage.Ordinal{1, 2, 3},
				},
				{
					path:     "conversation-a",
					streamID: "session-a",
					payloads: toTestPayloads(4),
					want:     []storage.Ordinal{4},
				},
			},
		},
		{
			name: "each_stream_counts_from_one",
			calls: []appendCall{
				{
					path:     "conversation-a",
					streamID: "session-a",
					payloads: toTestPayloads(1, 2),
					want:     []storage.Ordinal{1, 2},
				},
				{
					path:     "conversation-a",
					streamID: "session-b",
					payloads: toTestPayloads(1),
					want:     []storage.Ordinal{1},
				},
				{
					path:     "conversation-b",
					streamID: "session-a",
					payloads: toTestPayloads(1),
					want:     []storage.Ordinal{1},
				},
				{
					path:     "conversation-a",
					streamID: "session-a",
					payloads: toTestPayloads(3),
					want:     []storage.Ordinal{3},
				},
			},
		},
	}
	// 2. 执行各批追加并回读，检查连续序号、批内时间和载荷。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := openTestMemoryDatabase(t).Log()
			for _, call := range tt.calls {
				infos, err := logs.Append(t.Context(), call.path, call.streamID, call.payloads...)
				if err != nil {
					t.Fatal(err)
				}
				if len(infos) != len(call.want) {
					t.Fatalf("Append() = %+v, want ordinals %v", infos, call.want)
				}
				for index, info := range infos {
					if info.Ordinal != call.want[index] {
						t.Fatalf("Append() = %+v, want ordinals %v", infos, call.want)
					}
					if info.UpdatedAt <= 0 || info.UpdatedAt != infos[0].UpdatedAt {
						t.Fatalf("Append() = %+v, want one positive updated_at for the whole batch", infos)
					}
				}

				page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
					Path:     call.path,
					StreamID: call.streamID,
					Cursor:   infos[0].Ordinal - 1,
					Limit:    len(infos),
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Items) != len(infos) {
					t.Fatalf("List() = %+v, want the %d appended entries", page.Items, len(infos))
				}
				for index, entry := range page.Items {
					if entry.Ordinal != infos[index].Ordinal || entry.UpdatedAt != infos[index].UpdatedAt || string(entry.Payload) != string(call.payloads[index]) {
						t.Fatalf("List() item %d = %+v, want %+v with payload %s", index, entry, infos[index], call.payloads[index])
					}
				}
			}
		})
	}
}

// TestLogStorageList 检查 SQLite 日志的双向分页。
func TestLogStorageList(t *testing.T) {
	// 1. 准备不同方向、游标、页大小和空日志样例。
	tests := []struct {
		name        string
		appended    int
		order       storage.Direction
		cursor      storage.Ordinal
		limit       int
		wantOrdinal []storage.Ordinal
		wantNext    storage.Ordinal
		wantErr     string
	}{
		{
			name:        "asc_first_page",
			appended:    5,
			order:       storage.DirectionAsc,
			limit:       2,
			wantOrdinal: []storage.Ordinal{1, 2},
			wantNext:    2,
		},
		{
			name:        "asc_cursor_is_excluded",
			appended:    5,
			order:       storage.DirectionAsc,
			cursor:      2,
			limit:       2,
			wantOrdinal: []storage.Ordinal{3, 4},
			wantNext:    4,
		},
		{
			name:        "asc_last_page_has_no_next",
			appended:    5,
			order:       storage.DirectionAsc,
			cursor:      4,
			limit:       2,
			wantOrdinal: []storage.Ordinal{5},
		},
		{
			name:        "asc_exact_fit_has_no_next",
			appended:    4,
			order:       storage.DirectionAsc,
			cursor:      2,
			limit:       2,
			wantOrdinal: []storage.Ordinal{3, 4},
		},
		{
			name:        "default_order_is_asc",
			appended:    3,
			limit:       2,
			wantOrdinal: []storage.Ordinal{1, 2},
			wantNext:    2,
		},
		{
			name:        "desc_first_page",
			appended:    5,
			order:       storage.DirectionDesc,
			limit:       2,
			wantOrdinal: []storage.Ordinal{5, 4},
			wantNext:    4,
		},
		{
			name:        "desc_cursor_is_excluded",
			appended:    5,
			order:       storage.DirectionDesc,
			cursor:      4,
			limit:       2,
			wantOrdinal: []storage.Ordinal{3, 2},
			wantNext:    2,
		},
		{
			name:        "desc_last_page_has_no_next",
			appended:    5,
			order:       storage.DirectionDesc,
			cursor:      2,
			limit:       2,
			wantOrdinal: []storage.Ordinal{1},
		},
		{
			name:        "desc_exact_fit_has_no_next",
			appended:    4,
			order:       storage.DirectionDesc,
			cursor:      3,
			limit:       2,
			wantOrdinal: []storage.Ordinal{2, 1},
		},
		{
			name:        "limit_larger_than_stream",
			appended:    3,
			order:       storage.DirectionAsc,
			limit:       10,
			wantOrdinal: []storage.Ordinal{1, 2, 3},
		},
		{
			name:        "empty_stream",
			order:       storage.DirectionAsc,
			limit:       2,
			wantOrdinal: []storage.Ordinal{},
		},
		{
			name:        "asc_cursor_at_tail_is_empty",
			appended:    3,
			order:       storage.DirectionAsc,
			cursor:      3,
			limit:       2,
			wantOrdinal: []storage.Ordinal{},
		},
		{
			name:        "desc_cursor_at_tail_continues_before_it",
			appended:    3,
			order:       storage.DirectionDesc,
			cursor:      3,
			limit:       2,
			wantOrdinal: []storage.Ordinal{2, 1},
		},
		{
			name:     "asc_cursor_beyond_tail",
			appended: 3,
			order:    storage.DirectionAsc,
			cursor:   4,
			limit:    2,
			wantErr:  "ordinal 4 is beyond log tail 3",
		},
		{
			name:     "desc_cursor_beyond_tail",
			appended: 3,
			order:    storage.DirectionDesc,
			cursor:   4,
			limit:    2,
			wantErr:  "ordinal 4 is beyond log tail 3",
		},
		{
			name:     "default_order_cursor_beyond_tail",
			appended: 3,
			cursor:   9,
			limit:    2,
			wantErr:  "ordinal 9 is beyond log tail 3",
		},
		{
			name:    "asc_cursor_beyond_empty_stream",
			order:   storage.DirectionAsc,
			cursor:  1,
			limit:   2,
			wantErr: "ordinal 1 is beyond log tail 0",
		},
		{
			name:    "desc_cursor_beyond_empty_stream",
			order:   storage.DirectionDesc,
			cursor:  1,
			limit:   2,
			wantErr: "ordinal 1 is beyond log tail 0",
		},
	}
	// 2. 写入目标与其他 Stream，检查本页序号、载荷、下一页位置及错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := openTestMemoryDatabase(t).Log()
			for index := range tt.appended {
				if _, err := logs.Append(t.Context(), "conversation-a", "session-a", toTestPayloads(index+1)...); err != nil {
					t.Fatal(err)
				}
			}
			// 另一条 Stream 的条目不能混进这条 Stream 的分页。
			if _, err := logs.Append(t.Context(), "conversation-a", "session-b", toTestPayloads(100, 101, 102, 103, 104, 105)...); err != nil {
				t.Fatal(err)
			}

			page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     "conversation-a",
				StreamID: "session-a",
				Order:    tt.order,
				Cursor:   tt.cursor,
				Limit:    tt.limit,
			})
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("List() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := getTestOrdinals(page.Items); !slices.Equal(got, tt.wantOrdinal) || page.Next != tt.wantNext {
				t.Fatalf("List() ordinals=%v next=%d, want ordinals=%v next=%d", got, page.Next, tt.wantOrdinal, tt.wantNext)
			}
			for _, entry := range page.Items {
				if want := `{"n":` + strconv.Itoa(int(entry.Ordinal)) + `}`; string(entry.Payload) != want {
					t.Fatalf("List() ordinal %d payload = %s, want %s", entry.Ordinal, entry.Payload, want)
				}
			}
		})
	}
}

// TestLogStorageDelete 检查删除 Stream 后的内容和序号。
func TestLogStorageDelete(t *testing.T) {
	// 1. 准备已存在、不存在和空 Stream 样例。
	tests := []struct {
		name        string
		appended    int
		deleteID    storage.StreamID
		wantDeleted bool
	}{
		{
			name:        "deleted_stream_restarts_from_one",
			appended:    3,
			deleteID:    "session-a",
			wantDeleted: true,
		},
		{
			name:     "missing_stream_is_not_an_error",
			appended: 3,
			deleteID: "session-missing",
		},
		{
			name:        "empty_stream_is_not_an_error",
			deleteID:    "session-a",
			wantDeleted: true,
		},
	}
	// 2. 删除后检查其他 Stream 保留，再追加检查序号。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := openTestMemoryDatabase(t).Log()
			request := storage.ListLogEntriesRequest{
				Path:     "conversation-a",
				StreamID: "session-a",
				Limit:    10,
			}
			if tt.appended > 0 {
				if _, err := logs.Append(t.Context(), "conversation-a", "session-a", toTestPayloads(make([]int, tt.appended)...)...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := logs.Append(t.Context(), "conversation-a", "session-other", toTestPayloads(1)...); err != nil {
				t.Fatal(err)
			}

			if err := logs.Delete(t.Context(), "conversation-a", tt.deleteID); err != nil {
				t.Fatal(err)
			}
			page, err := logs.List(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			wantRemaining := tt.appended
			if tt.wantDeleted {
				wantRemaining = 0
			}
			if len(page.Items) != wantRemaining {
				t.Fatalf("List() after Delete = %d entries, want %d", len(page.Items), wantRemaining)
			}
			other, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     "conversation-a",
				StreamID: "session-other",
				Limit:    10,
			})
			if err != nil || len(other.Items) != 1 {
				t.Fatalf("other stream List() = %+v, %v, want its entry kept", other, err)
			}

			infos, err := logs.Append(t.Context(), "conversation-a", "session-a", toTestPayloads(1)...)
			if err != nil {
				t.Fatal(err)
			}
			if want := storage.Ordinal(wantRemaining + 1); infos[0].Ordinal != want {
				t.Fatalf("Append() after Delete = ordinal %d, want %d", infos[0].Ordinal, want)
			}
		})
	}
}

// TestLogStorageDeletePath 检查删除路径下全部 Stream。
func TestLogStorageDeletePath(t *testing.T) {
	// 1. 准备多 Stream、重复删除和缺失路径样例。
	tests := []struct {
		name        string
		streamIDs   []storage.StreamID
		deleteTwice bool
	}{
		{
			name:      "two_streams",
			streamIDs: []storage.StreamID{"session-a", "session-b"},
		},
		{
			name:        "delete_twice",
			streamIDs:   []storage.StreamID{"session-a"},
			deleteTwice: true,
		},
		{name: "missing_path"},
	}
	// 2. 删除后检查其他路径仍在，重新追加从序号 1 开始。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := openTestMemoryDatabase(t).Log()
			for _, streamID := range tt.streamIDs {
				if _, err := logs.Append(t.Context(), "conversation-a", streamID, toTestPayloads(1, 2, 3)...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := logs.Append(t.Context(), "conversation-other", "session-a", toTestPayloads(1)...); err != nil {
				t.Fatal(err)
			}

			if err := logs.DeletePath(t.Context(), "conversation-a"); err != nil {
				t.Fatal(err)
			}
			if tt.deleteTwice {
				if err := logs.DeletePath(t.Context(), "conversation-a"); err != nil {
					t.Fatal(err)
				}
			}
			paths, err := logs.GetStreamPaths(t.Context(), "session-a")
			if err != nil || !slices.Equal(paths, []storage.StreamPath{"conversation-other"}) {
				t.Fatalf("GetStreamPaths() = %v, %v, want only the other path", paths, err)
			}

			infos, err := logs.Append(t.Context(), "conversation-a", "session-a", toTestPayloads(1)...)
			if err != nil || infos[0].Ordinal != 1 {
				t.Fatalf("Append() after DeletePath = %v, %v, want ordinal 1", infos, err)
			}
			other, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     "conversation-other",
				StreamID: "session-a",
				Limit:    10,
			})
			if err != nil || len(other.Items) != 1 {
				t.Fatalf("other path List() = %+v, %v, want its entry kept", other, err)
			}
		})
	}
}

// TestLogStorageGetStreamPaths 检查含指定 Stream 的路径列表。
func TestLogStorageGetStreamPaths(t *testing.T) {
	// 1. 准备路径排序、其他 Stream 和不存在 Stream 的样例。
	type stream struct {
		path     storage.StreamPath
		streamID storage.StreamID
	}
	tests := []struct {
		name     string
		streams  []stream
		streamID storage.StreamID
		want     []storage.StreamPath
	}{
		{
			name: "two_paths_in_byte_order",
			streams: []stream{{
				"conversation-b",
				"session-a",
			}, {
				"conversation-a",
				"session-a",
			}},
			streamID: "session-a",
			want:     []storage.StreamPath{"conversation-a", "conversation-b"},
		},
		{
			name: "uppercase_sorts_before_lowercase",
			streams: []stream{{
				"a-1",
				"session-a",
			}, {
				"a",
				"session-a",
			}, {
				"B",
				"session-a",
			}},
			streamID: "session-a",
			want:     []storage.StreamPath{"B", "a", "a-1"},
		},
		{
			name: "other_stream_ids_are_not_included",
			streams: []stream{{
				"conversation-a",
				"session-a",
			}, {
				"conversation-b",
				"session-b",
			}},
			streamID: "session-a",
			want:     []storage.StreamPath{"conversation-a"},
		},
		{
			name: "no_stream_returns_empty_slice",
			streams: []stream{{
				"conversation-a",
				"session-a",
			}},
			streamID: "session-missing",
			want:     []storage.StreamPath{},
		},
	}
	// 2. 每个 Stream 追加多条，检查路径去重、排序及空列表。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := openTestMemoryDatabase(t).Log()
			for _, stream := range tt.streams {
				// 追加多条，路径只能按 Stream 去重地返回一次。
				if _, err := logs.Append(t.Context(), stream.path, stream.streamID, toTestPayloads(1, 2)...); err != nil {
					t.Fatal(err)
				}
			}
			got, err := logs.GetStreamPaths(t.Context(), tt.streamID)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || !slices.Equal(got, tt.want) {
				t.Fatalf("GetStreamPaths() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestLogStorageRejectsInvalidInput 检查 SQLite 日志拒绝无效输入。
func TestLogStorageRejectsInvalidInput(t *testing.T) {
	// 1. 准备载荷、路径、Stream、页大小和方向无效的调用。
	tests := []struct {
		name string
		call func(ctx context.Context, logs storage.LogStorage) error
	}{
		{
			name: "append_without_payload",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				_, err := logs.Append(ctx, "conversation-a", "session-a")
				return err
			},
		},
		{
			name: "append_invalid_json",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				_, err := logs.Append(ctx, "conversation-a", "session-a", json.RawMessage(`{"n":`))
				return err
			},
		},
		{
			name: "append_path_with_separator",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				_, err := logs.Append(ctx, "conversation/a", "session-a", toTestPayloads(1)...)
				return err
			},
		},
		{
			name: "append_empty_stream_id",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				_, err := logs.Append(ctx, "conversation-a", "", toTestPayloads(1)...)
				return err
			},
		},
		{
			name: "delete_invalid_path",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				return logs.Delete(ctx, "..", "session-a")
			},
		},
		{
			name: "list_zero_limit",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				_, err := logs.List(ctx, storage.ListLogEntriesRequest{
					Path:     "conversation-a",
					StreamID: "session-a",
				})
				return err
			},
		},
		{
			name: "list_unsupported_order",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				_, err := logs.List(ctx, storage.ListLogEntriesRequest{
					Path:     "conversation-a",
					StreamID: "session-a",
					Order:    "sideways",
					Limit:    1,
				})
				return err
			},
		},
		{
			name: "get_stream_paths_invalid_stream_id",
			call: func(ctx context.Context, logs storage.LogStorage) error {
				_, err := logs.GetStreamPaths(ctx, "session/a")
				return err
			},
		},
	}
	// 2. 执行各调用并检查返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := openTestMemoryDatabase(t).Log()
			if err := tt.call(t.Context(), logs); err == nil {
				t.Fatal("call accepted invalid input")
			}
		})
	}
}
