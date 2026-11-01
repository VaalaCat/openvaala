package file_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	"github.com/google/uuid"
)

// testEvent 是测试写进日志的载荷。
type testEvent struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// TestLogStorageAppendBatch 检查文件日志的批量追加和重新打开。
func TestLogStorageAppendBatch(t *testing.T) {
	// 1. 准备单批和多批载荷及期望序号。
	tests := []struct {
		name         string
		batches      [][]testEvent
		wantOrdinals [][]storage.Ordinal
	}{
		{
			name: "single_batch",
			batches: [][]testEvent{{{
				Kind: "message",
				Text: "one",
			}, {
				Kind: "message",
				Text: "two",
			}}},
			wantOrdinals: [][]storage.Ordinal{{1, 2}},
		},
		{
			name: "batches_continue_ordinals",
			batches: [][]testEvent{
				{{
					Kind: "message",
					Text: "one",
				}},
				{{
					Kind: "message",
					Text: "two",
				}, {
					Kind: "message",
					Text: "three",
				}, {
					Kind: "message",
					Text: "four",
				}},
			},
			wantOrdinals: [][]storage.Ordinal{{1}, {2, 3, 4}},
		},
	}
	// 2. 追加各批后重新打开，检查序号和完整日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs, root := openTestLogStorage(t)
			path, streamID := newStreamPath(), newStreamID()
			var want []testEvent
			for index, batch := range tt.batches {
				infos, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, batch...)...)
				if err != nil || !slices.Equal(toOrdinals(infos), tt.wantOrdinals[index]) {
					t.Fatalf("Append() = %v, %v, want %v", infos, err, tt.wantOrdinals[index])
				}
				want = append(want, batch...)
			}
			reopened, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			page, err := reopened.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.DirectionAsc,
				Limit:    10,
			})
			if err != nil || page.Next != 0 || !reflect.DeepEqual(decodeEvents(t, page.Items), want) {
				t.Fatalf("reopened List() = %+v, %v, want %+v", page, err, want)
			}
		})
	}
}

// TestLogStorageList 检查文件日志双向分页。
func TestLogStorageList(t *testing.T) {
	// 1. 准备三条日志及各方向、游标和页大小。
	logs, root := openTestLogStorage(t)
	path, streamID := newStreamPath(), newStreamID()
	want := []testEvent{
		{
			Kind: "message",
			Text: "one",
		},
		{
			Kind: "message",
			Text: "two",
		},
		{
			Kind: "message",
			Text: "three",
		},
	}
	if _, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, want...)...); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		order        storage.Direction
		cursor       storage.Ordinal
		limit        int
		wantNext     storage.Ordinal
		wantOrdinals []storage.Ordinal
		want         []testEvent
	}{
		{
			name:         "ascending_first_page",
			order:        storage.DirectionAsc,
			cursor:       0,
			limit:        2,
			wantNext:     2,
			wantOrdinals: []storage.Ordinal{1, 2},
			want:         want[:2],
		},
		{
			name:         "ascending_last_page",
			order:        storage.DirectionAsc,
			cursor:       2,
			limit:        2,
			wantNext:     0,
			wantOrdinals: []storage.Ordinal{3},
			want:         want[2:],
		},
		{
			name:         "ascending_exact_limit",
			order:        storage.DirectionAsc,
			cursor:       0,
			limit:        3,
			wantNext:     0,
			wantOrdinals: []storage.Ordinal{1, 2, 3},
			want:         want,
		},
		{
			name:         "ascending_empty_tail",
			order:        storage.DirectionAsc,
			cursor:       3,
			limit:        2,
			wantNext:     0,
			wantOrdinals: []storage.Ordinal{},
			want:         []testEvent{},
		},
		{
			name:         "descending_first_page",
			order:        storage.DirectionDesc,
			cursor:       0,
			limit:        2,
			wantNext:     2,
			wantOrdinals: []storage.Ordinal{3, 2},
			want:         []testEvent{want[2], want[1]},
		},
		{
			name:         "descending_last_page",
			order:        storage.DirectionDesc,
			cursor:       2,
			limit:        2,
			wantNext:     0,
			wantOrdinals: []storage.Ordinal{1},
			want:         want[:1],
		},
		{
			name:         "descending_empty_head",
			order:        storage.DirectionDesc,
			cursor:       1,
			limit:        2,
			wantNext:     0,
			wantOrdinals: []storage.Ordinal{},
			want:         []testEvent{},
		},
	}
	// 2. 检查本页内容、序号、下一页位置及空页切片。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    tt.order,
				Cursor:   tt.cursor,
				Limit:    tt.limit,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := decodeEvents(t, page.Items)
			if page.Next != tt.wantNext || !slices.Equal(entryOrdinals(page.Items), tt.wantOrdinals) || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("List() = %+v, %+v, want Next %d, %+v", page, got, tt.wantNext, tt.want)
			}
			if page.Items == nil {
				t.Fatal("List() returned a nil Slice")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, string(path), string(streamID)+".jsonl")); err != nil {
		t.Fatal(err)
	}
}

// TestLogStorageWritesOrdinalEnvelopeJSONL 检查日志文件的 JSONL 行格式。
func TestLogStorageWritesOrdinalEnvelopeJSONL(t *testing.T) {
	// 1. 追加包含换行文本和缩进格式的 JSON 载荷。
	logs, root := openTestLogStorage(t)
	path, streamID := newStreamPath(), newStreamID()
	want := testEvent{
		Kind: "message",
		Text: "first line\nsecond line",
	}
	indented, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logs.Append(t.Context(), path, streamID, indented); err != nil {
		t.Fatal(err)
	}
	// 2. 读取原始文件，检查单行 JSON、结尾换行及三个外层字段。
	content, err := os.ReadFile(filepath.Join(root, string(path), string(streamID)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) != 2 || lines[1] != "" || !json.Valid([]byte(lines[0])) {
		t.Fatalf("JSONL content = %q", content)
	}
	var envelope struct {
		Ordinal storage.Ordinal `json:"ordinal"`
		Payload testEvent       `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Ordinal != 1 || envelope.Payload != want {
		t.Fatalf("decoded envelope = %+v, want ordinal 1 and payload %+v", envelope, want)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["ordinal"] == nil || fields["updated_at"] == nil || fields["payload"] == nil {
		t.Fatalf("JSONL fields = %v, want ordinal, updated_at and payload", fields)
	}
}

// streamLocation 是测试中一条 Stream 的 Path 与 StreamID。
type streamLocation struct {
	path     storage.StreamPath
	streamID storage.StreamID
}

// TestLogStorageSeparatesStreams 检查不同路径或 Stream 的文件和序号。
func TestLogStorageSeparatesStreams(t *testing.T) {
	// 1. 准备同路径不同 Stream、同 Stream 不同路径的样例。
	tests := []struct {
		name    string
		streams []streamLocation
	}{
		{
			name: "same_path_different_ids",
			streams: []streamLocation{{
				"conv-a",
				"s1",
			}, {
				"conv-a",
				"s2",
			}},
		},
		{
			name: "same_id_different_paths",
			streams: []streamLocation{{
				"conv-a",
				"metadata",
			}, {
				"conv-b",
				"metadata",
			}},
		},
	}
	// 2. 分别追加并检查各自从序号 1 开始且文件存在。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs, root := openTestLogStorage(t)
			for index, stream := range tt.streams {
				infos, err := logs.Append(t.Context(), stream.path, stream.streamID, encodeEvents(t, testEvent{
					Kind: "stream",
					Text: string(rune('A' + index)),
				})...)
				if err != nil || !slices.Equal(toOrdinals(infos), []storage.Ordinal{1}) {
					t.Fatalf("Append(%s/%s) = %v, %v", stream.path, stream.streamID, infos, err)
				}
				if _, err := os.Stat(filepath.Join(root, string(stream.path), string(stream.streamID)+".jsonl")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestLogStorageTruncatesIncompleteTail 检查重新打开时截去未写完的尾行。
func TestLogStorageTruncatesIncompleteTail(t *testing.T) {
	// 1. 写入两条日志，再直接追加半行 JSON。
	logs, root := openTestLogStorage(t)
	path, streamID := newStreamPath(), newStreamID()
	if _, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
		Kind: "message",
		Text: "one",
	}, testEvent{
		Kind: "message",
		Text: "two",
	})...); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(root, string(path), string(streamID)+".jsonl")
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"ordinal":3`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// 2. 重新打开并追加第三条，检查序号连续且文件只有完整行。
	reopened, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(root))
	if err != nil {
		t.Fatal(err)
	}
	infos, err := reopened.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
		Kind: "message",
		Text: "three",
	})...)
	if err != nil || !slices.Equal(toOrdinals(infos), []storage.Ordinal{3}) {
		t.Fatalf("Append after partial tail = %v, %v", infos, err)
	}
	page, err := reopened.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeEvents(t, page.Items); !reflect.DeepEqual(got, []testEvent{{
		Kind: "message",
		Text: "one",
	}, {
		Kind: "message",
		Text: "two",
	}, {
		Kind: "message",
		Text: "three",
	}}) {
		t.Fatalf("repaired stream = %+v", got)
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) != 4 || lines[3] != "" {
		t.Fatalf("repaired JSONL has incomplete lines: %q", content)
	}
	for _, line := range lines[:3] {
		if !json.Valid([]byte(line)) {
			t.Fatalf("repaired JSONL contains invalid line %q", line)
		}
	}
}

// TestLogStorageRejectsMiddleCorruption 检查完整中间行损坏时不修改文件。
func TestLogStorageRejectsMiddleCorruption(t *testing.T) {
	// 1. 写入中间夹有损坏行的原始文件。
	logs, root := openTestLogStorage(t)
	path, streamID := newStreamPath(), newStreamID()
	if err := os.MkdirAll(filepath.Join(root, string(path)), 0700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(root, string(path), string(streamID)+".jsonl")
	wantBytes := []byte("{\"ordinal\":1,\"payload\":{\"kind\":\"valid\",\"text\":\"one\"}}\ninvalid\n{\"ordinal\":2,\"payload\":{\"kind\":\"valid\",\"text\":\"two\"}}\n")
	if err := os.WriteFile(filePath, wantBytes, 0600); err != nil {
		t.Fatal(err)
	}
	// 2. 检查读取和追加均报错，文件内容保持不变。
	if _, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    10,
	}); err == nil {
		t.Fatal("List() accepted middle corruption")
	}
	if _, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
		Kind: "message",
		Text: "three",
	})...); err == nil {
		t.Fatal("Append() accepted middle corruption")
	}
	gotBytes, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gotBytes, wantBytes) {
		t.Fatalf("corrupt file changed: %q", gotBytes)
	}
}

// TestLogStorageRejectsInvalidEnvelope 检查文件日志拒绝损坏的完整行。
func TestLogStorageRejectsInvalidEnvelope(t *testing.T) {
	// 1. 准备仅载荷、重复或跳号序号和缺少载荷的样例。
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "payload_only",
			content: "{\"kind\":\"message\",\"text\":\"one\"}\n",
		},
		{
			name:    "duplicate_ordinal",
			content: "{\"ordinal\":1,\"payload\":{\"text\":\"one\"}}\n{\"ordinal\":1,\"payload\":{\"text\":\"two\"}}\n",
		},
		{
			name:    "skipped_ordinal",
			content: "{\"ordinal\":1,\"payload\":{\"text\":\"one\"}}\n{\"ordinal\":3,\"payload\":{\"text\":\"three\"}}\n",
		},
		{
			name:    "missing_payload",
			content: "{\"ordinal\":1}\n",
		},
	}
	// 2. 写入原始文件并检查 List 返回错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs, root := openTestLogStorage(t)
			path, streamID := newStreamPath(), newStreamID()
			if err := os.MkdirAll(filepath.Join(root, string(path)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, string(path), string(streamID)+".jsonl"), []byte(tt.content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.DirectionAsc,
				Limit:    10,
			}); err == nil {
				t.Fatal("List() accepted invalid persisted envelope")
			}
		})
	}
}

// TestLogStorageConcurrentAppend 检查并发批量追加的序号。
func TestLogStorageConcurrentAppend(t *testing.T) {
	// 1. 并发提交 32 批，每批两条日志。
	logs, _ := openTestLogStorage(t)
	path, streamID := newStreamPath(), newStreamID()
	batches := make(chan []storage.Ordinal, 32)
	errors := make(chan error, 32)
	var tasks sync.WaitGroup
	for index := range 32 {
		tasks.Go(func() {
			text := string(rune('a' + index%26))
			infos, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
				Kind: "first",
				Text: text,
			}, testEvent{
				Kind: "second",
				Text: text,
			})...)
			batches <- toOrdinals(infos)
			errors <- err
		})
	}
	// 2. 等全部追加结束，检查批内和整体序号连续。
	tasks.Wait()
	close(batches)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	gotOrdinals := make([]int, 0, 64)
	for ordinals := range batches {
		if len(ordinals) != 2 || ordinals[1] != ordinals[0]+1 {
			t.Fatalf("batch ordinals = %v, want two consecutive", ordinals)
		}
		for _, ordinal := range ordinals {
			gotOrdinals = append(gotOrdinals, int(ordinal))
		}
	}
	slices.Sort(gotOrdinals)
	for index, ordinal := range gotOrdinals {
		if ordinal != index+1 {
			t.Fatalf("ordinals = %v", gotOrdinals)
		}
	}
	// 3. 读取全部日志，检查共有 64 条且已到末页。
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    64,
	})
	if err != nil || page.Next != 0 || len(page.Items) != 64 {
		t.Fatalf("List() = Next %d, %d entries, %v", page.Next, len(page.Items), err)
	}
}

// TestLogStorageDelete 检查文件 Stream 删除后重建。
func TestLogStorageDelete(t *testing.T) {
	// 1. 准备缺失和已存在 Stream 及重复删除场景。
	tests := []struct {
		name        string
		appendFirst bool
		deleteTwice bool
	}{
		{
			name:        "missing_stream",
			deleteTwice: true,
		},
		{
			name:        "existing_stream",
			appendFirst: true,
			deleteTwice: true,
		},
	}
	// 2. 删除后检查文件消失，再追加检查只剩新内容且从序号 1 开始。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs, root := openTestLogStorage(t)
			path, streamID := newStreamPath(), newStreamID()
			if tt.appendFirst {
				if _, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
					Kind: "old",
					Text: "old",
				})...); err != nil {
					t.Fatal(err)
				}
				if _, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
					Path:     path,
					StreamID: streamID,
					Order:    storage.DirectionAsc,
					Limit:    10,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := logs.Delete(t.Context(), path, streamID); err != nil {
				t.Fatal(err)
			}
			if tt.deleteTwice {
				if err := logs.Delete(t.Context(), path, streamID); err != nil {
					t.Fatal(err)
				}
			}
			filePath := filepath.Join(root, string(path), string(streamID)+".jsonl")
			if _, err := os.Stat(filePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("deleted stream still exists: %v", err)
			}

			want := testEvent{
				Kind: "new",
				Text: "new",
			}
			if infos, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, want)...); err != nil || !slices.Equal(toOrdinals(infos), []storage.Ordinal{1}) {
				t.Fatalf("Append() after Delete = %v, %v, want ordinal 1", infos, err)
			}
			page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.DirectionAsc,
				Limit:    10,
			})
			if err != nil || !slices.Equal(entryOrdinals(page.Items), []storage.Ordinal{1}) || !reflect.DeepEqual(decodeEvents(t, page.Items), []testEvent{want}) {
				t.Fatalf("List() after Delete = %+v, %v", page, err)
			}
		})
	}
}

// TestLogStorageDeletePath 检查文件路径删除及其他路径保留。
func TestLogStorageDeletePath(t *testing.T) {
	// 1. 准备多 Stream、重复删除、缺失路径和缺失根目录。
	tests := []struct {
		name          string
		streamIDs     []storage.StreamID
		withOtherPath bool
		deleteTwice   bool
	}{
		{
			name:          "two_streams",
			streamIDs:     []storage.StreamID{"session-a", "session-b"},
			withOtherPath: true,
		},
		{
			name:          "delete_twice",
			streamIDs:     []storage.StreamID{"session-a"},
			withOtherPath: true,
			deleteTwice:   true,
		},
		{
			name:          "missing_path",
			withOtherPath: true,
		},
		{name: "missing_root"},
	}
	// 2. 删除后检查路径和缓存，再追加从序号 1 开始且其他路径仍在。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs, root := openTestLogStorage(t)
			path, otherPath := newStreamPath(), newStreamPath()
			for _, streamID := range tt.streamIDs {
				if _, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
					Kind: "old",
					Text: "1",
				}, testEvent{
					Kind: "old",
					Text: "2",
				}, testEvent{
					Kind: "old",
					Text: "3",
				})...); err != nil {
					t.Fatal(err)
				}
				if _, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
					Path:     path,
					StreamID: streamID,
					Order:    storage.DirectionAsc,
					Limit:    10,
				}); err != nil {
					t.Fatal(err)
				}
			}
			wantPaths := []storage.StreamPath{}
			if tt.withOtherPath {
				wantPaths = append(wantPaths, otherPath)
				if _, err := logs.Append(t.Context(), otherPath, "session-a", encodeEvents(t, testEvent{
					Kind: "other",
					Text: "other",
				})...); err != nil {
					t.Fatal(err)
				}
			}

			if err := logs.DeletePath(t.Context(), path); err != nil {
				t.Fatal(err)
			}
			if tt.deleteTwice {
				if err := logs.DeletePath(t.Context(), path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, string(path))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("deleted path still exists: %v", err)
			}
			paths, err := logs.GetStreamPaths(t.Context(), "session-a")
			if err != nil || !slices.Equal(paths, wantPaths) {
				t.Fatalf("GetStreamPaths() = %v, %v, want %v", paths, err, wantPaths)
			}

			infos, err := logs.Append(t.Context(), path, "session-a", encodeEvents(t, testEvent{
				Kind: "new",
				Text: "new",
			})...)
			if err != nil || !slices.Equal(toOrdinals(infos), []storage.Ordinal{1}) {
				t.Fatalf("Append() after DeletePath = %v, %v, want ordinal 1", infos, err)
			}
			if tt.withOtherPath {
				other, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
					Path:     otherPath,
					StreamID: "session-a",
					Order:    storage.DirectionAsc,
					Limit:    10,
				})
				if err != nil || len(other.Items) != 1 {
					t.Fatalf("other path List() = %+v, %v, want its entry kept", other, err)
				}
			}
		})
	}
}

// TestLogStorageDeleteSerializesWithAccess 检查同一 Stream 的并发删除、追加和读取。
func TestLogStorageDeleteSerializesWithAccess(t *testing.T) {
	// 1. 写入初始日志。
	logs, _ := openTestLogStorage(t)
	path, streamID := newStreamPath(), newStreamID()
	if _, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
		Kind: "seed",
		Text: "seed",
	})...); err != nil {
		t.Fatal(err)
	}

	// 2. 同时启动四次追加、四次读取和一次删除，等待全部返回。
	start := make(chan struct{})
	errors := make(chan error, 9)
	var tasks sync.WaitGroup
	for index := range 4 {
		tasks.Go(func() {
			<-start
			_, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
				Kind: "append",
				Text: string(rune('a' + index)),
			})...)
			errors <- err
		})
		tasks.Go(func() {
			<-start
			_, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.DirectionAsc,
				Limit:    10,
			})
			errors <- err
		})
	}
	tasks.Go(func() {
		<-start
		errors <- logs.Delete(t.Context(), path, streamID)
	})
	close(start)
	tasks.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	// 3. 回读保留的日志，检查序号从 1 连续排列。
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, ordinal := range entryOrdinals(page.Items) {
		if ordinal != storage.Ordinal(index+1) {
			t.Fatalf("List() ordinals = %v", entryOrdinals(page.Items))
		}
	}
}

// TestLogStorageRejectsInvalidInput 检查文件日志拒绝无效配置和读取请求。
func TestLogStorageRejectsInvalidInput(t *testing.T) {
	// 1. 检查空配置和非绝对根目录。
	if _, err := storagefile.NewLogStorage(nil); err == nil {
		t.Fatal("NewLogStorage() accepted nil options")
	}
	for _, root := range []string{"", "relative/logs"} {
		if _, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(root)); err == nil {
			t.Fatalf("NewLogStorage() accepted root %q", root)
		}
	}
	// 2. 检查无效路径、Stream 名称和路径枚举参数。
	logs, _ := openTestLogStorage(t)
	invalidStreams := []struct {
		name     string
		path     storage.StreamPath
		streamID storage.StreamID
	}{
		{
			name:     "empty_path",
			path:     "",
			streamID: "s1",
		},
		{
			name:     "dot_path",
			path:     ".",
			streamID: "s1",
		},
		{
			name:     "parent_path",
			path:     "..",
			streamID: "s1",
		},
		{
			name:     "nested_path",
			path:     "a/b",
			streamID: "s1",
		},
		{
			name:     "space_in_path",
			path:     "a b",
			streamID: "s1",
		},
		{
			name:     "empty_stream",
			path:     "conv",
			streamID: "",
		},
		{
			name:     "parent_stream",
			path:     "conv",
			streamID: "..",
		},
		{
			name:     "nested_stream",
			path:     "conv",
			streamID: "x/y",
		},
	}
	for _, tt := range invalidStreams {
		if _, err := logs.Append(t.Context(), tt.path, tt.streamID, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("Append(%s) accepted %q/%q", tt.name, tt.path, tt.streamID)
		}
		if err := logs.Delete(t.Context(), tt.path, tt.streamID); err == nil {
			t.Fatalf("Delete(%s) accepted %q/%q", tt.name, tt.path, tt.streamID)
		}
		if _, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
			Path:     tt.path,
			StreamID: tt.streamID,
			Order:    storage.DirectionAsc,
			Limit:    1,
		}); err == nil {
			t.Fatalf("List(%s) accepted %q/%q", tt.name, tt.path, tt.streamID)
		}
	}
	if _, err := logs.GetStreamPaths(t.Context(), "a/b"); err == nil {
		t.Fatal("GetStreamPaths() accepted nested stream ID")
	}
	path, streamID := newStreamPath(), newStreamID()
	// 3. 检查零页大小、未知方向和超过末尾的游标。
	invalidRequests := []struct {
		name    string
		request storage.ListLogEntriesRequest
	}{
		{
			name: "zero_limit",
			request: storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.DirectionAsc,
				Limit:    0,
			},
		},
		{
			name: "unsupported_order",
			request: storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.Direction("unsupported"),
				Limit:    1,
			},
		},
		{
			name: "cursor_beyond_tail",
			request: storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.DirectionAsc,
				Cursor:   1,
				Limit:    1,
			},
		},
	}
	for _, tt := range invalidRequests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := logs.List(t.Context(), tt.request); err == nil {
				t.Fatal("List() error = nil")
			}
		})
	}
}

// TestLogStorageRejectsInvalidBatchWithoutWriting 检查无效日志批次不写入文件。
func TestLogStorageRejectsInvalidBatchWithoutWriting(t *testing.T) {
	// 1. 准备空批次、损坏 JSON 和 nil 载荷。
	tests := []struct {
		name     string
		payloads []json.RawMessage
	}{
		{name: "empty_batch"},
		{
			name:     "invalid_json",
			payloads: []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"broken"`)},
		},
		{
			name:     "nil_payload",
			payloads: []json.RawMessage{nil},
		},
	}
	// 2. 检查追加返回错误且未创建根目录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "logs")
			logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := logs.Append(t.Context(), newStreamPath(), newStreamID(), tt.payloads...); err == nil {
				t.Fatal("Append() accepted invalid batch")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("rejected batch created root directory: %v", err)
			}
		})
	}
}

// TestLogStorageMissingStreamIsEmpty 检查缺失 Stream 返回空页。
func TestLogStorageMissingStreamIsEmpty(t *testing.T) {
	// 1. 读取尚未创建的日志，检查空结果且没有创建根目录。
	root := filepath.Join(t.TempDir(), "logs")
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(root))
	if err != nil {
		t.Fatal(err)
	}
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     newStreamPath(),
		StreamID: newStreamID(),
		Order:    storage.DirectionAsc,
		Limit:    10,
	})
	if err != nil || page.Next != 0 || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("List() = %+v, %v", page, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("missing read created root directory: %v", err)
	}
}

// TestLogStorageListAcceptsLargeLimit 检查最大页大小仍可读取短日志。
func TestLogStorageListAcceptsLargeLimit(t *testing.T) {
	// 1. 写入一条日志，以 math.MaxInt 为页大小读取并检查末页内容。
	logs, _ := openTestLogStorage(t)
	path, streamID := newStreamPath(), newStreamID()
	want := testEvent{
		Kind: "message",
		Text: "one",
	}
	if _, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, want)...); err != nil {
		t.Fatal(err)
	}
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    math.MaxInt,
	})
	if err != nil || page.Next != 0 || !reflect.DeepEqual(decodeEvents(t, page.Items), []testEvent{want}) {
		t.Fatalf("List() = %+v, %v", page, err)
	}
}

// openTestLogStorage 创建临时目录下的文件日志存储。
func openTestLogStorage(t *testing.T) (storage.LogStorage, string) {
	t.Helper()
	// 1. 打开以临时 logs 目录为根的存储，返回存储和这个目录。
	root := filepath.Join(t.TempDir(), "logs")
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(root))
	if err != nil {
		t.Fatal(err)
	}
	return logs, root
}

// newStreamPath 创建测试日志路径标识。
func newStreamPath() storage.StreamPath {
	// 1. 生成 UUID v7 作为路径名称。
	return storage.StreamPath(uuid.Must(uuid.NewV7()).String())
}

// newStreamID 创建测试 Stream 标识。
func newStreamID() storage.StreamID {
	// 1. 生成 UUID v7 作为 Stream 名称。
	return storage.StreamID(uuid.Must(uuid.NewV7()).String())
}

// encodeEvents 将测试事件编码为日志载荷。
func encodeEvents(t *testing.T, events ...testEvent) []json.RawMessage {
	t.Helper()
	// 1. 按顺序编码各事件，编码失败时终止测试。
	payloads := make([]json.RawMessage, len(events))
	for index, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		payloads[index] = payload
	}
	return payloads
}

// decodeEvents 将日志载荷解码为测试事件。
func decodeEvents(t *testing.T, entries []storage.LogEntry) []testEvent {
	t.Helper()
	// 1. 按顺序解码各条载荷，解码失败时终止测试。
	events := make([]testEvent, len(entries))
	for index, entry := range entries {
		if err := json.Unmarshal(entry.Payload, &events[index]); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

// entryOrdinals 提取日志项的序号。
func entryOrdinals(entries []storage.LogEntry) []storage.Ordinal {
	// 1. 按输入顺序收集 Ordinal。
	ordinals := make([]storage.Ordinal, len(entries))
	for index, entry := range entries {
		ordinals[index] = entry.Ordinal
	}
	return ordinals
}

// TestLogStorageAppendFailureRemovesBatch 检查追加失败后撤销本批日志。
func TestLogStorageAppendFailureRemovesBatch(t *testing.T) {
	// 1. root 用户下跳过；否则先建好日志根目录。
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not restrict root")
	}
	logs, root := openTestLogStorage(t)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path, streamID := newStreamPath(), newStreamID()

	// 2. 取消根目录读取权限，追加时文件写入但目录同步失败，再恢复权限。
	if err := os.Chmod(root, 0300); err != nil {
		t.Fatal(err)
	}
	_, appendErr := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
		Kind: "message",
		Text: "lost",
	})...)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if appendErr == nil {
		t.Fatal("Append() succeeded with unreadable log root")
	}

	// 3. 检查失败批次未留下内容，下一次成功追加从序号 1 开始。
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    10,
	})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("List() after failed Append = %+v, %v, want empty", page, err)
	}
	infos, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, testEvent{
		Kind: "message",
		Text: "kept",
	})...)
	if err != nil || !slices.Equal(toOrdinals(infos), []storage.Ordinal{1}) {
		t.Fatalf("Append() after failure = %v, %v, want [1]", infos, err)
	}
}

// TestLogStorageUpdatedAt 检查文件日志的追加时间。
func TestLogStorageUpdatedAt(t *testing.T) {
	// 1. 准备同批多条和跨批追加样例。
	tests := []struct {
		name    string
		batches [][]testEvent
	}{
		{
			name: "batch_shares_one_time",
			batches: [][]testEvent{{{
				Kind: "a",
				Text: "1",
			}, {
				Kind: "a",
				Text: "2",
			}}},
		},
		{
			name: "later_batch_is_not_earlier",
			batches: [][]testEvent{{{
				Kind: "a",
				Text: "1",
			}}, {{
				Kind: "b",
				Text: "2",
			}}},
		},
	}
	// 2. 检查批内时间相同、回读时间一致且位于调用前后时间内。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs, _ := openTestLogStorage(t)
			path, streamID := newStreamPath(), newStreamID()
			before := time.Now().UnixMilli()
			var appended []storage.LogEntryInfo
			for _, batch := range tt.batches {
				infos, err := logs.Append(t.Context(), path, streamID, encodeEvents(t, batch...)...)
				if err != nil {
					t.Fatal(err)
				}
				for _, info := range infos[1:] {
					if info.UpdatedAt != infos[0].UpdatedAt {
						t.Fatalf("batch times = %+v, want one time", infos)
					}
				}
				appended = append(appended, infos...)
			}
			after := time.Now().UnixMilli()
			page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
				Path:     path,
				StreamID: streamID,
				Order:    storage.DirectionAsc,
				Limit:    10,
			})
			if err != nil || len(page.Items) != len(appended) {
				t.Fatalf("List() = %+v, %v", page, err)
			}
			for index, entry := range page.Items {
				if entry.UpdatedAt != appended[index].UpdatedAt || entry.UpdatedAt < before || entry.UpdatedAt > after {
					t.Fatalf("entry %d UpdatedAt = %d, appended %d, want within [%d, %d]", index, entry.UpdatedAt, appended[index].UpdatedAt, before, after)
				}
				if index > 0 && entry.UpdatedAt < page.Items[index-1].UpdatedAt {
					t.Fatalf("UpdatedAt went backwards: %+v", page.Items)
				}
			}
		})
	}
}

// TestLogStorageGetStreamPaths 检查文件日志路径枚举。
func TestLogStorageGetStreamPaths(t *testing.T) {
	// 1. 准备缺失根目录、多个路径和夹杂文件、目录及符号链接。
	tests := []struct {
		name  string
		setup func(t *testing.T, logs storage.LogStorage, root string)
		want  []storage.StreamPath
	}{
		{
			name:  "missing_root",
			setup: func(t *testing.T, _ storage.LogStorage, root string) {},
			want:  []storage.StreamPath{},
		},
		{
			name: "sorted_paths_with_stream",
			setup: func(t *testing.T, logs storage.LogStorage, root string) {
				for _, path := range []storage.StreamPath{"conv-b", "conv-a"} {
					if _, err := logs.Append(t.Context(), path, "metadata", json.RawMessage(`{}`)); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: []storage.StreamPath{"conv-a", "conv-b"},
		},
		{
			name: "ignores_clutter",
			setup: func(t *testing.T, logs storage.LogStorage, root string) {
				if _, err := logs.Append(t.Context(), "conv-a", "metadata", json.RawMessage(`{}`)); err != nil {
					t.Fatal(err)
				}
				if _, err := logs.Append(t.Context(), "conv-only-session", "s1", json.RawMessage(`{}`)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, "dot.dir"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "dot.dir", "metadata.jsonl"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(root, "conv-dir-stream", "metadata.jsonl"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "conv-a"), filepath.Join(root, "conv-link")); err != nil {
					t.Fatal(err)
				}
			},
			want: []storage.StreamPath{"conv-a"},
		},
	}
	// 2. 检查结果升序排列且只包含普通日志文件所在路径。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs, root := openTestLogStorage(t)
			tt.setup(t, logs, root)
			got, err := logs.GetStreamPaths(t.Context(), "metadata")
			if err != nil || got == nil || !slices.Equal(got, tt.want) {
				t.Fatalf("GetStreamPaths() = %v, %v, want %v", got, err, tt.want)
			}
		})
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
