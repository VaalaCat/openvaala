package storage

import (
	"context"
	"encoding/json"
)

// StreamPath 是 Stream 所在的一级目录，例如对话 ID；只允许字母、数字、`_` 和 `-`。
type StreamPath string

// StreamID 标识 Path 内的一条 Stream；只允许字母、数字、`_` 和 `-`。
type StreamID string

// Ordinal 是 Stream 内从 1 开始、由 LogStorage 分配的严格顺序；0 只用于初始游标。
type Ordinal uint64

// LogEntry 是 Stream 中一条已追加的 JSON Payload。
type LogEntry struct {
	Ordinal Ordinal
	// UpdatedAt 是追加时的 Unix 毫秒，由 LogStorage 分配；日志追加后不再修改，这个时间就是保存时间。
	UpdatedAt int64
	Payload   json.RawMessage
}

// LogEntryInfo 是刚追加的一条日志分到的顺序号和追加时间。
type LogEntryInfo struct {
	Ordinal   Ordinal
	UpdatedAt int64
}

// ListLogEntriesRequest 是一次日志分页读取的条件；日志只按 Ordinal 排序。
type ListLogEntriesRequest struct {
	Path     StreamPath
	StreamID StreamID
	Order    Direction
	// Cursor 为 0 时从这个方向的起点读；非零时从它之后读，不含它本身。
	Cursor Ordinal
	Limit  int
}

// LogStorage 保存按 Stream 追加的 JSON Payload，并按请求方向读取。
type LogStorage interface {
	// Append 把一批 Payload（至少一条）一次写入并 fsync，按顺序返回分到的顺序号和追加时间，同一批的追加时间相同；
	// 崩溃时可能只写入这批的一部分。
	Append(ctx context.Context, path StreamPath, streamID StreamID, payloads ...json.RawMessage) ([]LogEntryInfo, error)
	// Delete 删除指定路径下的一条 Stream；Stream 不存在时什么都不做。
	Delete(ctx context.Context, path StreamPath, streamID StreamID) error
	// DeletePath 删除 path 下全部 Stream；path 不存在时什么都不做。
	DeletePath(ctx context.Context, path StreamPath) error
	// List 按方向分页；这个方向还有更多时 Next 为本页最后一个 Ordinal，否则为 0。
	List(ctx context.Context, request ListLogEntriesRequest) (Page[LogEntry, Ordinal], error)
	// GetStreamPaths 返回含有这个 StreamID 的全部 Path，按字节序升序；一个都没有时返回空切片。
	GetStreamPaths(ctx context.Context, streamID StreamID) ([]StreamPath, error)
}
