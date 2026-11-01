package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// logStorageImpl 把 Stream 保存在同一个库的 logs 表里；表由 OpenDatabase 创建。
type logStorageImpl struct {
	db *sql.DB
}

var _ storage.LogStorage = (*logStorageImpl)(nil)

// streamNamePattern 是 Path 和 StreamID 允许的字符：字母、数字、`_` 和 `-`。
var streamNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Append 在事务中追加一批日志，返回顺序号及追加时间。
func (logs *logStorageImpl) Append(
	ctx context.Context,
	path storage.StreamPath,
	streamID storage.StreamID,
	payloads ...json.RawMessage,
) (infos []storage.LogEntryInfo, err error) {
	// 1. 检查路径、Stream 名称和非空 JSON 载荷。
	if err := validateStream(path, streamID); err != nil {
		return nil, err
	}
	if len(payloads) == 0 {
		return nil, errors.New("log append requires at least one payload")
	}
	for index, payload := range payloads {
		if !json.Valid(payload) {
			return nil, fmt.Errorf("log payload %d is not valid JSON", index)
		}
	}

	// 2. 开启事务并安排失败回滚。
	tx, err := logs.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin log append: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback log append: %w", rollbackErr))
		}
	}()
	// 3. 读取末尾顺序号，为本批分配连续序号和同一时间。
	var lastOrdinal storage.Ordinal
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ordinal), 0) FROM logs WHERE path = ? AND stream_id = ?`,
		string(path), string(streamID),
	).Scan(&lastOrdinal); err != nil {
		return nil, fmt.Errorf("get log tail: %w", err)
	}
	updatedAt := time.Now().UnixMilli()
	infos = make([]storage.LogEntryInfo, len(payloads))
	for index, payload := range payloads {
		ordinal := lastOrdinal + storage.Ordinal(index) + 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO logs (path, stream_id, ordinal, updated_at, payload) VALUES (?, ?, ?, ?, ?)`,
			string(path), string(streamID), ordinal, updatedAt, []byte(payload),
		); err != nil {
			return nil, fmt.Errorf("append log ordinal %d: %w", ordinal, err)
		}
		infos[index] = storage.LogEntryInfo{
			Ordinal:   ordinal,
			UpdatedAt: updatedAt,
		}
	}
	// 4. 提交事务并返回本批追加信息。
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit log append: %w", err)
	}
	return infos, nil
}

// Delete 删除指定路径下的一条日志 Stream。
func (logs *logStorageImpl) Delete(ctx context.Context, path storage.StreamPath, streamID storage.StreamID) error {
	// 1. 检查路径和 Stream 名称后删除匹配行。
	if err := validateStream(path, streamID); err != nil {
		return err
	}
	if _, err := logs.db.ExecContext(ctx,
		`DELETE FROM logs WHERE path = ? AND stream_id = ?`, string(path), string(streamID),
	); err != nil {
		return fmt.Errorf("delete log stream: %w", err)
	}
	return nil
}

// DeletePath 删除指定路径下的全部日志。
func (logs *logStorageImpl) DeletePath(ctx context.Context, path storage.StreamPath) error {
	// 1. 检查路径后删除匹配行，不存在时直接成功。
	if !streamNamePattern.MatchString(string(path)) {
		return fmt.Errorf("invalid log path %q", path)
	}
	if _, err := logs.db.ExecContext(ctx, `DELETE FROM logs WHERE path = ?`, string(path)); err != nil {
		return fmt.Errorf("delete log path: %w", err)
	}
	return nil
}

// List 按顺序号分页读取日志。
func (logs *logStorageImpl) List(
	ctx context.Context,
	request storage.ListLogEntriesRequest,
) (storage.Page[storage.LogEntry, storage.Ordinal], error) {
	// 1. 检查路径、Stream、页大小和方向。
	var page storage.Page[storage.LogEntry, storage.Ordinal]
	if err := validateStream(request.Path, request.StreamID); err != nil {
		return page, err
	}
	if request.Limit <= 0 {
		return page, errors.New("log entry limit must be positive")
	}
	order, err := request.Order.WithDefault()
	if err != nil {
		return page, err
	}

	// 2. 非零游标不得超过日志末尾。
	if request.Cursor != 0 {
		var lastOrdinal storage.Ordinal
		if err := logs.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(ordinal), 0) FROM logs WHERE path = ? AND stream_id = ?`,
			string(request.Path), string(request.StreamID),
		).Scan(&lastOrdinal); err != nil {
			return page, fmt.Errorf("get log tail: %w", err)
		}
		if request.Cursor > lastOrdinal {
			return page, fmt.Errorf("ordinal %d is beyond log tail %d", request.Cursor, lastOrdinal)
		}
	}

	// 3. 按方向添加游标条件，多取一条判断下一页。
	query := `SELECT ordinal, updated_at, payload FROM logs WHERE path = ? AND stream_id = ?`
	arguments := []any{string(request.Path), string(request.StreamID)}
	direction := "ASC"
	if order == storage.DirectionDesc {
		direction = "DESC"
		if request.Cursor != 0 {
			query += ` AND ordinal < ?`
			arguments = append(arguments, request.Cursor)
		}
	} else {
		query += ` AND ordinal > ?`
		arguments = append(arguments, request.Cursor)
	}
	query += ` ORDER BY ordinal ` + direction + ` LIMIT ?`
	arguments = append(arguments, request.Limit+1)

	// 4. 读取日志行，还有更多时去掉多取的一条，用本页末项顺序号作为 Next。
	rows, err := logs.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return page, fmt.Errorf("list log entries: %w", err)
	}
	defer rows.Close()
	page.Items = make([]storage.LogEntry, 0, min(request.Limit+1, 64))
	for rows.Next() {
		var entry storage.LogEntry
		var payload []byte
		if err := rows.Scan(&entry.Ordinal, &entry.UpdatedAt, &payload); err != nil {
			return storage.Page[storage.LogEntry, storage.Ordinal]{}, fmt.Errorf("scan log entry: %w", err)
		}
		entry.Payload = payload
		page.Items = append(page.Items, entry)
	}
	if err := rows.Err(); err != nil {
		return storage.Page[storage.LogEntry, storage.Ordinal]{}, fmt.Errorf("read log entries: %w", err)
	}
	if len(page.Items) > request.Limit {
		page.Items = page.Items[:request.Limit]
		page.Next = page.Items[request.Limit-1].Ordinal
	}
	return page, nil
}

// GetStreamPaths 返回含有指定 Stream 的全部路径。
func (logs *logStorageImpl) GetStreamPaths(ctx context.Context, streamID storage.StreamID) ([]storage.StreamPath, error) {
	// 1. 检查 Stream 名称，按升序查询去重后的路径。
	if !streamNamePattern.MatchString(string(streamID)) {
		return nil, fmt.Errorf("invalid log stream ID %q", streamID)
	}
	rows, err := logs.db.QueryContext(ctx,
		`SELECT DISTINCT path FROM logs WHERE stream_id = ? ORDER BY path`, string(streamID),
	)
	if err != nil {
		return nil, fmt.Errorf("get log stream paths: %w", err)
	}
	defer rows.Close()
	// 2. 依次读取路径，读取出错时返回错误。
	paths := []storage.StreamPath{}
	for rows.Next() {
		var path storage.StreamPath
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan log stream path: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read log stream paths: %w", err)
	}
	return paths, nil
}

// validateStream 检查日志路径和 Stream 名称。
func validateStream(path storage.StreamPath, streamID storage.StreamID) error {
	// 1. 要求两者均为非空的字母、数字、下划线或短横线。
	if !streamNamePattern.MatchString(string(path)) || !streamNamePattern.MatchString(string(streamID)) {
		return fmt.Errorf("invalid log stream %q/%q", path, streamID)
	}
	return nil
}
