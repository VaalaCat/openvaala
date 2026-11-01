package dao

import (
	"context"
	"errors"
	"fmt"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// turnNamespace 是 Turn 索引记录所在的记录分组；索引记录由 metadata 日志生成。
const turnNamespace = "turns"

// turnDAOImpl 读写 Turn 的 metadata 日志和索引记录。
type turnDAOImpl struct {
	records storage.RecordStorage
	logs    storage.LogStorage
}

// ListTurnsRequest 是分页列出一个 Session 内 Turn 的条件。
type ListTurnsRequest struct {
	SessionID defs.SessionID
	Order     storage.Order
	// Cursor 是上一页最后一条的位置；零值从头开始。
	Cursor RecordCursor[defs.TurnID]
	Limit  int
}

// SetTurn 检查字段后追加 Turn 的 metadata 日志并写入索引。
func (d *turnDAOImpl) SetTurn(ctx context.Context, value defs.Turn) error {
	// 1. 检查实体的必填字段和状态。
	if err := validateTurn(value); err != nil {
		return err
	}
	// 2. 清空载荷里的更新时间，追加 metadata 日志，再用日志行的追加时间写入索引。
	value.UpdatedAt = 0
	if err := saveMetadata(ctx, d.logs, d.records, metadataEntry{
		Kind: metadataTurnSaved,
		Turn: &value,
	}); err != nil {
		return fmt.Errorf("save Turn %q: %w", value.ID, err)
	}
	return nil
}

// GetTurn 读取指定 Turn，返回记录及是否找到。
func (d *turnDAOImpl) GetTurn(ctx context.Context, sessionID defs.SessionID, id defs.TurnID) (defs.Turn, bool, error) {
	// 1. 检查两个 ID，按 Session ID 和 Turn ID 查询记录；没找到时返回未找到。
	if sessionID == "" || id == "" {
		return defs.Turn{}, false, errors.New("get Turn requires a session ID and Turn ID")
	}
	record, exists, err := d.records.Get(ctx, turnNamespace, turnKey(sessionID, id))
	if err != nil {
		return defs.Turn{}, false, fmt.Errorf("get Turn %q: %w", id, err)
	}
	if !exists {
		return defs.Turn{}, false, nil
	}
	// 2. 解码并检查记录。
	value, err := decodeTurn(record)
	if err != nil {
		return defs.Turn{}, false, err
	}
	return value, true, nil
}

// GetTurns 批量读取 Session 内的 Turn，按输入 ID 顺序返回找到的记录。
func (d *turnDAOImpl) GetTurns(ctx context.Context, sessionID defs.SessionID, ids []defs.TurnID) ([]defs.Turn, error) {
	// 1. 检查 Session ID 并组装 Turn 记录 Key。
	if sessionID == "" {
		return nil, errors.New("get Turns requires a session ID")
	}
	keys := make([]storage.RecordKey, len(ids))
	for index, id := range ids {
		keys[index] = turnKey(sessionID, id)
	}
	// 2. 批量读取，按输入顺序解码找到的记录，跳过不存在的记录。
	records, err := d.records.MGet(ctx, turnNamespace, keys)
	if err != nil {
		return nil, fmt.Errorf("get Turns: %w", err)
	}
	turns := make([]defs.Turn, 0, len(records))
	for _, key := range keys {
		record, exists := records[key]
		if !exists {
			continue
		}
		value, err := decodeTurn(record)
		if err != nil {
			return nil, err
		}
		turns = append(turns, value)
	}
	return turns, nil
}

// ListTurns 分页读取一个 Session 内的 Turn。
func (d *turnDAOImpl) ListTurns(ctx context.Context, request ListTurnsRequest) (storage.Page[defs.Turn, RecordCursor[defs.TurnID]], error) {
	// 1. 检查 Session ID，按范围、排序和游标读取一页记录。
	var result storage.Page[defs.Turn, RecordCursor[defs.TurnID]]
	if request.SessionID == "" {
		return result, errors.New("list Turns requires a session ID")
	}
	start, end := turnRange(request.SessionID)
	page, err := d.records.List(ctx, storage.ListRecordsRequest{
		Namespace: turnNamespace,
		Start:     start,
		End:       end,
		Order:     request.Order,
		Cursor:    getStorageCursor(request.Cursor, turnKey(request.SessionID, request.Cursor.ID)),
		Limit:     request.Limit,
	})
	if err != nil {
		return result, fmt.Errorf("list Turns: %w", err)
	}
	// 2. 解码本页记录并填写下一页位置。
	result.Items = make([]defs.Turn, len(page.Items))
	for index, record := range page.Items {
		if result.Items[index], err = decodeTurn(record); err != nil {
			return storage.Page[defs.Turn, RecordCursor[defs.TurnID]]{}, err
		}
	}
	if page.Next != (storage.RecordCursor{}) {
		last := result.Items[len(result.Items)-1]
		result.Next = RecordCursor[defs.TurnID]{
			ID:        last.ID,
			UpdatedAt: last.UpdatedAt,
		}
	}
	return result, nil
}

// GetSessionUsage 返回 Session 全部 Turn 的用量合计。
func GetSessionUsage(ctx context.Context, query TurnQuery, sessionID defs.SessionID) (defs.TokenUsage, error) {
	// 1. 分页读取 Turn，累加用量直到末页；读取失败时返回已累加的用量和错误。
	var usage defs.TokenUsage
	request := ListTurnsRequest{
		SessionID: sessionID,
		Limit:     scanPageSize,
	}
	for {
		page, err := query.ListTurns(ctx, request)
		if err != nil {
			return usage, err
		}
		for _, turn := range page.Items {
			usage = usage.Add(turn.Usage)
		}
		if page.Next == (RecordCursor[defs.TurnID]{}) {
			return usage, nil
		}
		request.Cursor = page.Next
	}
}

// decodeTurn 解码并检查 Turn 记录。
func decodeTurn(record storage.Record) (defs.Turn, error) {
	// 1. 解码载荷，检查 Key 与实体 ID 及实体字段。
	value, err := decodePayload[defs.Turn](record.Payload)
	if err != nil {
		return defs.Turn{}, fmt.Errorf("decode Turn %q: %w", record.Key, err)
	}
	if record.Key != turnKey(value.SessionID, value.ID) {
		return defs.Turn{}, fmt.Errorf("stored Turn %q key differs from identity", record.Key)
	}
	if err := validateTurn(value); err != nil {
		return defs.Turn{}, fmt.Errorf("decode Turn %q: %w", record.Key, err)
	}
	// 2. 更新时间取记录的 UpdatedAt。
	value.UpdatedAt = record.UpdatedAt
	return value, nil
}

// validateTurn 检查 Turn 的必填字段、状态和 Item 序号范围。
func validateTurn(value defs.Turn) error {
	// 1. 检查 Turn ID、所属对话和 Session。
	if value.ID == "" || value.ConversationID == "" || value.SessionID == "" {
		return errors.New("turn requires an ID, conversation ID and session ID")
	}
	// 2. 检查状态是已知的四种之一。
	switch value.Status {
	case defs.TurnStatusRunning, defs.TurnStatusCompleted, defs.TurnStatusFailed, defs.TurnStatusInterrupted:
	default:
		return fmt.Errorf("unsupported turn status %q", value.Status)
	}
	// 3. 检查首尾 Item 序号：没有首项时不能有末项，运行中的 Turn 还没有末项，结束的 Turn 有首项时末项不小于首项。
	first, last := value.ItemsRange.FirstOrdinal, value.ItemsRange.LastOrdinal
	if (first == 0 && last != 0) ||
		(value.Status == defs.TurnStatusRunning && last != 0) ||
		(value.Status != defs.TurnStatusRunning && first != 0 && last < first) {
		return fmt.Errorf("turn %q has invalid item range [%d, %d]", value.ID, first, last)
	}
	return nil
}
