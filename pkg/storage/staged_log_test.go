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

// TestStagedLogStorage 检查日志的暂存、搬运和失败重试。
func TestStagedLogStorage(t *testing.T) {
	// 1. 准备暂存与直接写入的数据及对应场景。
	const path storage.StreamPath = "conversation-a"
	metadata := []json.RawMessage{json.RawMessage(`{"n":1}`), json.RawMessage(`{"n":2}`), json.RawMessage(`{"n":3}`)}
	items := []json.RawMessage{json.RawMessage(`{"item":1}`)}
	tests := []struct {
		name string
		run  func(t *testing.T, staged *storage.StagedLogStorage, staging storagesqlite.Database, target *faultyLogStorage)
	}{
		{
			name: "staged_streams_stay_in_staging",
			run: func(t *testing.T, staged *storage.StagedLogStorage, staging storagesqlite.Database, target *faultyLogStorage) {
				requirePayloads(t, staging.Log(), path, "metadata", metadata)
				requirePayloads(t, staging.Log(), path, "session-a", nil)
				requirePayloads(t, target, path, "metadata", nil)
				requirePayloads(t, target, path, "session-a", items)
				requirePayloads(t, staged, path, "metadata", metadata)
				requirePayloads(t, staged, path, "session-a", items)
			},
		},
		{
			name: "persist_appends_in_order",
			run: func(t *testing.T, staged *storage.StagedLogStorage, staging storagesqlite.Database, target *faultyLogStorage) {
				if err := staged.Persist(t.Context()); err != nil {
					t.Fatal(err)
				}
				if !staged.IsPersisted() {
					t.Fatal("IsPersisted() = false after Persist")
				}
				requirePayloads(t, target, path, "metadata", metadata)
				// 关掉 staging 后仍能读写暂存的 Stream：Persist 之后不再碰 staging。
				if err := staging.Close(); err != nil {
					t.Fatal(err)
				}
				next := json.RawMessage(`{"n":4}`)
				if infos, err := staged.Append(t.Context(), path, "metadata", next); err != nil || infos[0].Ordinal != 4 {
					t.Fatalf("Append() after Persist = %+v, %v, want ordinal 4", infos, err)
				}
				requirePayloads(t, staged, path, "metadata", append(slices.Clone(metadata), next))
				if err := staged.Persist(t.Context()); err != nil {
					t.Fatalf("second Persist() = %v, want nil", err)
				}
			},
		},
		{
			name: "delete_path_clears_staging_and_target",
			run: func(t *testing.T, staged *storage.StagedLogStorage, staging storagesqlite.Database, target *faultyLogStorage) {
				if err := staged.DeletePath(t.Context(), path); err != nil {
					t.Fatal(err)
				}
				requirePayloads(t, staging.Log(), path, "metadata", nil)
				requirePayloads(t, target, path, "session-a", nil)
				requirePayloads(t, staged, path, "metadata", nil)
				requirePayloads(t, staged, path, "session-a", nil)
			},
		},
		{
			name: "failed_persist_keeps_staging",
			run: func(t *testing.T, staged *storage.StagedLogStorage, staging storagesqlite.Database, target *faultyLogStorage) {
				target.failingStreamID = "metadata"
				if err := staged.Persist(t.Context()); err == nil || !strings.Contains(err.Error(), `persist staged stream "metadata"`) {
					t.Fatalf("Persist() = %v, want the metadata stream error", err)
				}
				if staged.IsPersisted() {
					t.Fatal("IsPersisted() = true after a failed Persist")
				}
				requirePayloads(t, target, path, "metadata", nil)
				requirePayloads(t, staging.Log(), path, "metadata", metadata)
				target.failingStreamID = ""
				if err := staged.Persist(t.Context()); err != nil {
					t.Fatal(err)
				}
				requirePayloads(t, target, path, "metadata", metadata)
			},
		},
	}
	// 2. 创建暂存和目标存储，写入数据后检查搬运顺序、读写切换和重试。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staging := openStagingDatabase(t)
			target := &faultyLogStorage{LogStorage: openStagingDatabase(t).Log()}
			staged := storage.NewStagedLogStorage(staging.Log(), target, "metadata")
			// 分两批追加，Persist 要按原顺序搬过去。
			for _, batch := range [][]json.RawMessage{metadata[:2], metadata[2:]} {
				if _, err := staged.Append(t.Context(), path, "metadata", batch...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := staged.Append(t.Context(), path, "session-a", items...); err != nil {
				t.Fatal(err)
			}
			tt.run(t, staged, staging, target)
		})
	}
}

// faultyLogStorage 在 failingStreamID 非空时，Append 这个 Stream 返回错误。
type faultyLogStorage struct {
	storage.LogStorage
	failingStreamID storage.StreamID
}

// Append 按指定 Stream 注入追加错误。
func (s *faultyLogStorage) Append(ctx context.Context, path storage.StreamPath, streamID storage.StreamID, payloads ...json.RawMessage) ([]storage.LogEntryInfo, error) {
	// 1. 命中 failingStreamID 时返回错误，其余调用底层追加。
	if streamID == s.failingStreamID {
		return nil, errors.New("target is unavailable")
	}
	return s.LogStorage.Append(ctx, path, streamID, payloads...)
}

// requirePayloads 检查 Stream 的全部载荷与期望一致。
func requirePayloads(t *testing.T, logs storage.LogStorage, path storage.StreamPath, streamID storage.StreamID, want []json.RawMessage) {
	t.Helper()
	// 1. 升序读取日志，将各载荷转成文字后比较。
	page, err := logs.List(t.Context(), storage.ListLogEntriesRequest{
		Path:     path,
		StreamID: streamID,
		Order:    storage.DirectionAsc,
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(page.Items))
	for index, entry := range page.Items {
		got[index] = string(entry.Payload)
	}
	wantText := make([]string, len(want))
	for index, payload := range want {
		wantText[index] = string(payload)
	}
	if !slices.Equal(got, wantText) {
		t.Fatalf("List(%s/%s) = %v, want %v", path, streamID, got, wantText)
	}
}
