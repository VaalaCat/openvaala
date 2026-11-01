package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// StagedLogStorage 把指定 Stream 的日志先写在 staging，其余 Stream 直接读写 target；
// Persist 把暂存日志搬到 target 后，全部读写 target。
// staging 必须是只由本实例写入的空库；搬运前暂存的 Stream 只读 staging，不合并 target 中的日志。
type StagedLogStorage struct {
	staging         LogStorage
	target          LogStorage
	stagedStreamIDs []StreamID
	// lock 串行执行暂存 Stream 的读写和搬运；未暂存或已搬完的 Stream 不加锁。
	lock sync.Mutex
	// isPersisted 暂存日志是否已全部搬完？Persist 持锁设为 true 后不再改变。
	isPersisted atomic.Bool
}

var _ LogStorage = (*StagedLogStorage)(nil)

// NewStagedLogStorage 创建指定 Stream 的暂存存储。
func NewStagedLogStorage(staging, target LogStorage, stagedStreamIDs ...StreamID) *StagedLogStorage {
	// 1. 保存底层存储并复制暂存范围，两个底层存储由调用方打开和关闭。
	return &StagedLogStorage{
		staging:         staging,
		target:          target,
		stagedStreamIDs: slices.Clone(stagedStreamIDs),
	}
}

// Persist 把暂存的日志按顺序追加到 target，全部成功后读写都改走 target。
func (s *StagedLogStorage) Persist(ctx context.Context) error {
	// 1. 锁住暂存读写，已完成搬运时直接返回。
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.isPersisted.Load() {
		return nil
	}
	// 2. 按声明顺序搬运；失败时保留暂存状态，下次从头重试。已追加的日志会重复写入。
	for _, streamID := range s.stagedStreamIDs {
		if err := s.persistStream(ctx, streamID); err != nil {
			return fmt.Errorf("persist staged stream %q: %w", streamID, err)
		}
	}
	// 3. 全部写入成功后切换到 target。
	s.isPersisted.Store(true)
	return nil
}

// persistStream 将一个 Stream 在各路径下的暂存日志分页追加到 target。
func (s *StagedLogStorage) persistStream(ctx context.Context, streamID StreamID) error {
	// 1. 列出 staging 里含有这个 Stream 的路径。
	paths, err := s.staging.GetStreamPaths(ctx, streamID)
	if err != nil {
		return err
	}
	// 2. 各路径按顺序号升序读取，交由 target 重新分配顺序号和时间。
	for _, path := range paths {
		request := ListLogEntriesRequest{
			Path:     path,
			StreamID: streamID,
			Order:    DirectionAsc,
			Limit:    stagedPersistPageSize,
		}
		for {
			page, err := s.staging.List(ctx, request)
			if err != nil {
				return err
			}
			payloads := make([]json.RawMessage, len(page.Items))
			for index, entry := range page.Items {
				payloads[index] = entry.Payload
			}
			if _, err := s.target.Append(ctx, path, streamID, payloads...); err != nil {
				return err
			}
			if page.Next == 0 {
				break
			}
			request.Cursor = page.Next
		}
	}
	return nil
}

// IsPersisted 暂存数据已经全部写入 target 了吗？
func (s *StagedLogStorage) IsPersisted() bool {
	// 1. 已完成时直接返回，否则持锁读取结果；正在搬运时等它结束。
	if s.isPersisted.Load() {
		return true
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.isPersisted.Load()
}

// Append 向当前存储追加日志。
func (s *StagedLogStorage) Append(ctx context.Context, path StreamPath, streamID StreamID, payloads ...json.RawMessage) ([]LogEntryInfo, error) {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	logs, unlock := s.lockLogs(streamID)
	defer unlock()
	return logs.Append(ctx, path, streamID, payloads...)
}

// Delete 从当前存储删除指定 Stream。
func (s *StagedLogStorage) Delete(ctx context.Context, path StreamPath, streamID StreamID) error {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	logs, unlock := s.lockLogs(streamID)
	defer unlock()
	return logs.Delete(ctx, path, streamID)
}

// DeletePath 删除 staging 和 target 中指定路径下的全部 Stream。
func (s *StagedLogStorage) DeletePath(ctx context.Context, path StreamPath) error {
	// 1. 分别删除两边的路径，合并返回的错误。
	return errors.Join(s.staging.DeletePath(ctx, path), s.target.DeletePath(ctx, path))
}

// List 从当前存储分页读取日志。
func (s *StagedLogStorage) List(ctx context.Context, request ListLogEntriesRequest) (Page[LogEntry, Ordinal], error) {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	logs, unlock := s.lockLogs(request.StreamID)
	defer unlock()
	return logs.List(ctx, request)
}

// GetStreamPaths 从当前存储列出包含指定 Stream 的路径。
func (s *StagedLogStorage) GetStreamPaths(ctx context.Context, streamID StreamID) ([]StreamPath, error) {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	logs, unlock := s.lockLogs(streamID)
	defer unlock()
	return logs.GetStreamPaths(ctx, streamID)
}

// lockLogs 选出本次读写用的日志存储；用 staging 时持有暂存锁，由返回的函数解锁。
func (s *StagedLogStorage) lockLogs(streamID StreamID) (LogStorage, func()) {
	// 1. 未声明暂存或已搬完的数据使用 target，返回空解锁函数。
	if !slices.Contains(s.stagedStreamIDs, streamID) || s.isPersisted.Load() {
		return s.target, func() {}
	}
	// 2. 持锁重查搬运状态，尚未搬完时返回 staging 和读写结束后调用的解锁函数；已搬完时释放锁并返回 target。
	s.lock.Lock()
	if s.isPersisted.Load() {
		s.lock.Unlock()
		return s.target, func() {}
	}
	return s.staging, s.lock.Unlock
}
