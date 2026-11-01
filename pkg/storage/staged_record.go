package storage

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// StagedRecordStorage 把指定分组的记录先写在 staging，其余分组直接读写 target；
// Persist 把暂存记录搬到 target 后，全部读写 target。
// staging 必须是只由本实例写入的空库；搬运前暂存的分组只读 staging，不合并 target 中的记录。
type StagedRecordStorage struct {
	staging          RecordStorage
	target           RecordStorage
	stagedNamespaces []string
	// lock 串行执行暂存分组的读写和搬运；未暂存或已搬完的分组不加锁。
	lock sync.Mutex
	// isPersisted 暂存记录是否已全部搬完？Persist 持锁设为 true 后不再改变。
	isPersisted atomic.Bool
}

var _ RecordStorage = (*StagedRecordStorage)(nil)

// stagedPersistPageSize 是 Persist 从 staging 每次列出的条数。
const stagedPersistPageSize = 100

// NewStagedRecordStorage 创建指定记录分组的暂存存储。
func NewStagedRecordStorage(staging, target RecordStorage, stagedNamespaces ...string) *StagedRecordStorage {
	// 1. 保存底层存储并复制暂存范围，两个底层存储由调用方打开和关闭。
	return &StagedRecordStorage{
		staging:          staging,
		target:           target,
		stagedNamespaces: slices.Clone(stagedNamespaces),
	}
}

// Persist 把暂存的记录原样写入 target，全部成功后读写都改走 target。
func (s *StagedRecordStorage) Persist(ctx context.Context) error {
	// 1. 锁住暂存读写，已完成搬运时直接返回。
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.isPersisted.Load() {
		return nil
	}
	// 2. 按声明顺序搬运；失败时保留暂存状态，下次从头重试。已写入的记录会被覆盖。
	for _, namespace := range s.stagedNamespaces {
		if err := s.persistNamespace(ctx, namespace); err != nil {
			return fmt.Errorf("persist staged namespace %q: %w", namespace, err)
		}
	}
	// 3. 全部写入成功后切换到 target。
	s.isPersisted.Store(true)
	return nil
}

// persistNamespace 将一个 namespace 的暂存记录分页写入 target。
func (s *StagedRecordStorage) persistNamespace(ctx context.Context, namespace string) error {
	// 1. 从 Key 起点开始分页读取暂存记录。
	request := ListRecordsRequest{
		Namespace: namespace,
		Limit:     stagedPersistPageSize,
	}
	// 2. 原样写入 Key、UpdatedAt 和 Payload，按游标读取下一页。
	for {
		page, err := s.staging.List(ctx, request)
		if err != nil {
			return err
		}
		if err := s.target.MSet(ctx, namespace, page.Items); err != nil {
			return err
		}
		if page.Next == (RecordCursor{}) {
			return nil
		}
		request.Cursor = page.Next
	}
}

// IsPersisted 暂存数据已经全部写入 target 了吗？
func (s *StagedRecordStorage) IsPersisted() bool {
	// 1. 已完成时直接返回，否则持锁读取结果；正在搬运时等它结束。
	if s.isPersisted.Load() {
		return true
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.isPersisted.Load()
}

// Set 向当前存储写入一条记录。
func (s *StagedRecordStorage) Set(ctx context.Context, namespace string, record Record) error {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	records, unlock := s.lockRecords(namespace)
	defer unlock()
	return records.Set(ctx, namespace, record)
}

// MSet 向当前存储批量写入记录。
func (s *StagedRecordStorage) MSet(ctx context.Context, namespace string, records []Record) error {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	selected, unlock := s.lockRecords(namespace)
	defer unlock()
	return selected.MSet(ctx, namespace, records)
}

// Get 从当前存储读取一条记录。
func (s *StagedRecordStorage) Get(ctx context.Context, namespace string, key RecordKey) (Record, bool, error) {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	records, unlock := s.lockRecords(namespace)
	defer unlock()
	return records.Get(ctx, namespace, key)
}

// MGet 从当前存储批量读取记录。
func (s *StagedRecordStorage) MGet(ctx context.Context, namespace string, keys []RecordKey) (map[RecordKey]Record, error) {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	records, unlock := s.lockRecords(namespace)
	defer unlock()
	return records.MGet(ctx, namespace, keys)
}

// Delete 从当前存储删除一条记录。
func (s *StagedRecordStorage) Delete(ctx context.Context, namespace string, key RecordKey) error {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	records, unlock := s.lockRecords(namespace)
	defer unlock()
	return records.Delete(ctx, namespace, key)
}

// DeleteRange 从当前存储删除指定 Key 范围内的记录。
func (s *StagedRecordStorage) DeleteRange(ctx context.Context, namespace string, start, end RecordKey) error {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	records, unlock := s.lockRecords(namespace)
	defer unlock()
	return records.DeleteRange(ctx, namespace, start, end)
}

// List 从当前存储分页读取记录。
func (s *StagedRecordStorage) List(ctx context.Context, request ListRecordsRequest) (Page[Record, RecordCursor], error) {
	// 1. 选择存储并完成本次读写，读写结束时释放暂存锁。
	records, unlock := s.lockRecords(request.Namespace)
	defer unlock()
	return records.List(ctx, request)
}

// lockRecords 选出本次读写用的记录存储；用 staging 时持有暂存锁，由返回的函数解锁。
func (s *StagedRecordStorage) lockRecords(namespace string) (RecordStorage, func()) {
	// 1. 未声明暂存或已搬完的数据使用 target，返回空解锁函数。
	if !slices.Contains(s.stagedNamespaces, namespace) || s.isPersisted.Load() {
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
