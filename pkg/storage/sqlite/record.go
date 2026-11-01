package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// recordStorageImpl 把每个记录分组存成 SQLite 里的一张表。
type recordStorageImpl struct {
	db *sql.DB
	// tables 是打开时已建表的 namespace 到加引号表名的只读映射。
	tables map[string]string
	// fieldPaths 是 namespace 下已声明字段到 SQL 字符串形式 JSON 路径的只读映射。
	fieldPaths map[string]map[string]string
}

var _ storage.RecordStorage = (*recordStorageImpl)(nil)

// namespacePattern 是记录分组名允许的格式：小写字母开头，后面是小写字母、数字或下划线。
var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Set 写入一条记录。
func (records *recordStorageImpl) Set(ctx context.Context, namespace string, record storage.Record) error {
	// 1. 以单条批次调用 MSet。
	return records.MSet(ctx, namespace, []storage.Record{record})
}

// MSet 在事务中写入一批记录。
func (records *recordStorageImpl) MSet(ctx context.Context, namespace string, values []storage.Record) (err error) {
	// 1. 检查记录分组，以及各记录的 Key、正数时间和 JSON 载荷；空批次直接返回。
	table, err := records.getTable(namespace)
	if err != nil {
		return err
	}
	for _, record := range values {
		if record.Key == "" {
			return errors.New("record key is required")
		}
		if record.UpdatedAt <= 0 {
			return fmt.Errorf("record %q updated_at must be positive", record.Key)
		}
		if !json.Valid(record.Payload) {
			return fmt.Errorf("record %q payload is not valid JSON", record.Key)
		}
	}
	if len(values) == 0 {
		return nil
	}

	// 2. 开启事务并安排失败回滚。
	tx, err := records.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record write: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback record write: %w", rollbackErr))
		}
	}()
	// 3. 按输入顺序插入或覆盖记录，重复 Key 使用最后一条。
	statement := `INSERT INTO ` + table + ` (key, updated_at, payload)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			updated_at = excluded.updated_at,
			payload = excluded.payload`
	for _, record := range values {
		if _, err := tx.ExecContext(ctx, statement, string(record.Key), record.UpdatedAt, []byte(record.Payload)); err != nil {
			return fmt.Errorf("write record %q: %w", record.Key, err)
		}
	}
	// 4. 提交本批记录。
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record write: %w", err)
	}
	return nil
}

// Get 读取一条记录，同时返回是否找到。
func (records *recordStorageImpl) Get(ctx context.Context, namespace string, key storage.RecordKey) (storage.Record, bool, error) {
	// 1. 检查记录分组和非空 Key。
	table, err := records.getTable(namespace)
	if err != nil {
		return storage.Record{}, false, err
	}
	if key == "" {
		return storage.Record{}, false, errors.New("record key is required")
	}
	// 2. 按 Key 查询，没找到时返回未找到，其余错误附带 Key 返回。
	var updatedAt int64
	var payload []byte
	err = records.db.QueryRowContext(ctx,
		`SELECT updated_at, payload FROM `+table+` WHERE key = ?`, string(key),
	).Scan(&updatedAt, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Record{}, false, nil
	}
	if err != nil {
		return storage.Record{}, false, fmt.Errorf("get record %q: %w", key, err)
	}
	return newRecord(key, updatedAt, payload), true, nil
}

// MGet 批量读取记录，返回找到的 Key 和记录。
func (records *recordStorageImpl) MGet(ctx context.Context, namespace string, keys []storage.RecordKey) (map[storage.RecordKey]storage.Record, error) {
	// 1. 检查分组及各 Key，空批次返回空 map。
	table, err := records.getTable(namespace)
	if err != nil {
		return nil, err
	}
	arguments := make([]any, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			return nil, errors.New("record key is required")
		}
		arguments = append(arguments, string(key))
	}
	found := make(map[storage.RecordKey]storage.Record, len(keys))
	if len(arguments) == 0 {
		return found, nil
	}
	// 2. 组装 IN 参数并查询匹配记录。
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(arguments)), ",")
	rows, err := records.db.QueryContext(ctx,
		`SELECT key, updated_at, payload FROM `+table+` WHERE key IN (`+placeholders+`)`, arguments...,
	)
	if err != nil {
		return nil, fmt.Errorf("get records: %w", err)
	}
	defer rows.Close()
	// 3. 读取全部结果，读取出错时返回错误。
	for rows.Next() {
		var key storage.RecordKey
		var updatedAt int64
		var payload []byte
		if err := rows.Scan(&key, &updatedAt, &payload); err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}
		found[key] = newRecord(key, updatedAt, payload)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read records: %w", err)
	}
	return found, nil
}

// Delete 删除指定 Key 的记录。
func (records *recordStorageImpl) Delete(ctx context.Context, namespace string, key storage.RecordKey) error {
	// 1. 检查分组和非空 Key，再删除匹配记录。
	table, err := records.getTable(namespace)
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("record key is required")
	}
	if _, err := records.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE key = ?`, string(key)); err != nil {
		return fmt.Errorf("delete record %q: %w", key, err)
	}
	return nil
}

// DeleteRange 删除指定 Key 范围内的记录。
func (records *recordStorageImpl) DeleteRange(
	ctx context.Context,
	namespace string,
	start storage.RecordKey,
	end storage.RecordKey,
) error {
	// 1. 检查分组和范围。
	table, err := records.getTable(namespace)
	if err != nil {
		return err
	}
	if err := validateRecordRange(start, end); err != nil {
		return err
	}
	// 2. 按 [start, end) 组装条件并删除，空上下界表示整组。
	conditions, arguments := rangeConditions(start, end)
	query := `DELETE FROM ` + table
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	if _, err := records.db.ExecContext(ctx, query, arguments...); err != nil {
		return fmt.Errorf("delete record range: %w", err)
	}
	return nil
}

// List 按筛选条件、排序和游标读取一页记录。
func (records *recordStorageImpl) List(ctx context.Context, request storage.ListRecordsRequest) (storage.Page[storage.Record, storage.RecordCursor], error) {
	// 1. 检查分组、排序、页大小、Key 范围和游标。
	var page storage.Page[storage.Record, storage.RecordCursor]
	table, err := records.getTable(request.Namespace)
	if err != nil {
		return page, err
	}
	order, err := request.Order.WithDefaults()
	if err != nil {
		return page, err
	}
	if err := validateListRequest(request); err != nil {
		return page, err
	}

	// 2. 按方向组装范围和字段筛选条件。
	direction, comparison := "ASC", ">"
	if order.Direction == storage.DirectionDesc {
		direction, comparison = "DESC", "<"
	}
	conditions, arguments := rangeConditions(request.Start, request.End)
	filterSQL, filterArguments, err := getRecordFilterSQL(records.fieldPaths[request.Namespace], request.Filter)
	if err != nil {
		return page, err
	}
	if filterSQL != "" {
		conditions = append(conditions, filterSQL)
		arguments = append(arguments, filterArguments...)
	}
	// 3. 按 Key 或 (UpdatedAt, Key) 组装排序与游标条件。
	orderClause := "key " + direction
	switch {
	case order.By == storage.OrderByUpdatedAt:
		orderClause = "updated_at " + direction + ", key " + direction
		if request.Cursor != (storage.RecordCursor{}) {
			conditions = append(conditions, "(updated_at, key) "+comparison+" (?, ?)")
			arguments = append(arguments, request.Cursor.UpdatedAt, string(request.Cursor.Key))
		}
	case request.Cursor.Key != "":
		conditions = append(conditions, "key "+comparison+" ?")
		arguments = append(arguments, string(request.Cursor.Key))
	}
	// 4. 多取一条查询，读取全部结果。
	query := `SELECT key, updated_at, payload FROM ` + table
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY ` + orderClause + ` LIMIT ?`
	arguments = append(arguments, request.Limit+1)
	rows, err := records.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return page, fmt.Errorf("list records: %w", err)
	}
	defer rows.Close()
	page.Items = make([]storage.Record, 0, min(request.Limit+1, 64))
	for rows.Next() {
		var key storage.RecordKey
		var updatedAt int64
		var payload []byte
		if err := rows.Scan(&key, &updatedAt, &payload); err != nil {
			return storage.Page[storage.Record, storage.RecordCursor]{}, fmt.Errorf("scan listed record: %w", err)
		}
		page.Items = append(page.Items, newRecord(key, updatedAt, payload))
	}
	if err := rows.Err(); err != nil {
		return storage.Page[storage.Record, storage.RecordCursor]{}, fmt.Errorf("read listed records: %w", err)
	}
	// 5. 有更多记录时移除多取项，设置本页末项为下一页游标。
	if len(page.Items) > request.Limit {
		page.Items = page.Items[:request.Limit]
		last := page.Items[request.Limit-1]
		page.Next = storage.RecordCursor{
			Key:       last.Key,
			UpdatedAt: last.UpdatedAt,
		}
	}
	return page, nil
}

// getTable 查找记录分组的表名。
func (records *recordStorageImpl) getTable(namespace string) (string, error) {
	// 1. 从打开数据库时保存的映射中查找表名，未声明时返回错误。
	table, found := records.tables[namespace]
	if !found {
		return "", fmt.Errorf("unknown record namespace %q", namespace)
	}
	return table, nil
}

// rangeConditions 返回 Key 范围的 SQL 条件和参数。
func rangeConditions(start, end storage.RecordKey) ([]string, []any) {
	// 1. 添加非空的包含下界和排除上界条件。
	conditions := make([]string, 0, 3)
	arguments := make([]any, 0, 4)
	if start != "" {
		conditions = append(conditions, "key >= ?")
		arguments = append(arguments, string(start))
	}
	if end != "" {
		conditions = append(conditions, "key < ?")
		arguments = append(arguments, string(end))
	}
	return conditions, arguments
}

// validateListRequest 检查记录列表的页大小、范围及游标。
func validateListRequest(request storage.ListRecordsRequest) error {
	// 1. 要求页大小为正且小于 MaxInt，检查范围有效且游标在范围内。
	if request.Limit <= 0 || request.Limit == math.MaxInt {
		return errors.New("record list limit must be positive")
	}
	if err := validateRecordRange(request.Start, request.End); err != nil {
		return err
	}
	cursor := request.Cursor.Key
	if cursor != "" && (request.Start != "" && cursor < request.Start || request.End != "" && cursor >= request.End) {
		return errors.New("record cursor is outside the requested range")
	}
	return nil
}

// validateRecordRange 检查记录 Key 范围。
func validateRecordRange(start, end storage.RecordKey) error {
	// 1. 上下界均非空时，要求下界严格小于上界。
	if start != "" && end != "" && start >= end {
		return errors.New("record range start must be before end")
	}
	return nil
}

// newRecord 将查询字段组装为记录。
func newRecord(key storage.RecordKey, updatedAt int64, payload []byte) storage.Record {
	// 1. 保存 Key、写入时间和 JSON 载荷。
	return storage.Record{
		Key:       key,
		UpdatedAt: updatedAt,
		Payload:   payload,
	}
}
