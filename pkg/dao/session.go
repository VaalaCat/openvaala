package dao

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// SessionNamespace 是 Session 索引记录所在的记录分组；索引记录由 metadata 日志生成。
const SessionNamespace = "sessions"

// sessionDAOImpl 读写 Session 的 metadata 日志和索引记录，删除 Session 时一并删除它的 Item 日志。
type sessionDAOImpl struct {
	records storage.RecordStorage
	logs    storage.LogStorage
}

// ListSessionsRequest 是分页列出一个对话内 Session 的条件。
type ListSessionsRequest struct {
	ConversationID defs.ConversationID
	Order          storage.Order
	// Cursor 是上一页最后一条的位置；零值从头开始。
	Cursor RecordCursor[defs.SessionID]
	Limit  int
}

// SetSession 检查必填字段后追加 Session 的 metadata 日志并写入索引。
func (d *sessionDAOImpl) SetSession(ctx context.Context, value defs.Session) error {
	// 1. 检查 Session ID、对话 ID 和 Agent 名称。
	if err := validateSession(value); err != nil {
		return err
	}
	// 2. 清空载荷里的更新时间，追加 metadata 日志，再用日志行的追加时间写入索引。
	value.UpdatedAt = 0
	if err := saveMetadata(ctx, d.logs, d.records, metadataEntry{
		Kind:    metadataSessionSaved,
		Session: &value,
	}); err != nil {
		return fmt.Errorf("save Session %q: %w", value.ID, err)
	}
	return nil
}

// DeleteSession 先追加 session_deleted，再删除 Turn 索引、Item 日志，最后删除 Session 索引。
func (d *sessionDAOImpl) DeleteSession(ctx context.Context, conversationID defs.ConversationID, id defs.SessionID) error {
	// 1. 检查 ID 并读取 Session，不存在时直接返回。
	if id == "" || conversationID == "" {
		return errors.New("delete Session requires an ID and conversation ID")
	}
	_, found, err := d.GetSession(ctx, conversationID, id)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	// 2. 追加 session_deleted，失败时停止删除。
	entry := metadataEntry{
		Kind:             metadataSessionDeleted,
		DeletedSessionID: id,
	}
	if _, err := appendMetadata(ctx, d.logs, conversationID, entry); err != nil {
		return fmt.Errorf("delete Session %q: %w", id, err)
	}
	// 3. 依次删除 Turn 索引、Item 日志和 Session 索引，任一步失败时返回错误。
	start, end := turnRange(id)
	if err := d.records.DeleteRange(ctx, turnNamespace, start, end); err != nil {
		return fmt.Errorf("delete Session %q Turns: %w", id, err)
	}
	if err := d.logs.Delete(ctx, storage.StreamPath(conversationID), storage.StreamID(id)); err != nil {
		return fmt.Errorf("delete Session %q Items: %w", id, err)
	}
	if err := d.records.Delete(ctx, SessionNamespace, sessionKey(conversationID, id)); err != nil {
		return fmt.Errorf("delete Session %q: %w", id, err)
	}
	return nil
}

// GetSession 读取指定 Session，返回记录及是否找到。
func (d *sessionDAOImpl) GetSession(ctx context.Context, conversationID defs.ConversationID, id defs.SessionID) (defs.Session, bool, error) {
	// 1. 按对话 ID 和 Session ID 查询记录，没找到时返回未找到。
	record, exists, err := d.records.Get(ctx, SessionNamespace, sessionKey(conversationID, id))
	if err != nil {
		return defs.Session{}, false, fmt.Errorf("get Session %q: %w", id, err)
	}
	if !exists {
		return defs.Session{}, false, nil
	}
	// 2. 解码并检查记录。
	value, err := decodeSession(record)
	if err != nil {
		return defs.Session{}, false, err
	}
	return value, true, nil
}

// GetSessionByID 找出写过这个 Session Item 日志的对话，再读取其中的 Session 记录。
func (d *sessionDAOImpl) GetSessionByID(ctx context.Context, sessionID defs.SessionID) (defs.Session, bool, error) {
	// 1. 以 Session ID 为 Stream 查找 Item 日志所在路径，路径名称即对话 ID。
	paths, err := d.logs.GetStreamPaths(ctx, storage.StreamID(sessionID))
	if err != nil {
		return defs.Session{}, false, fmt.Errorf("find Session %q: %w", sessionID, err)
	}
	// 2. 依次读取各对话下的 Session 记录，读到或出错时直接返回；没写过 Item 或都没有匹配记录时返回未找到。
	for _, path := range paths {
		value, found, err := d.GetSession(ctx, defs.ConversationID(path), sessionID)
		if err != nil || found {
			return value, found, err
		}
	}
	return defs.Session{}, false, nil
}

// ListSessions 分页读取一个对话内的 Session。
func (d *sessionDAOImpl) ListSessions(ctx context.Context, request ListSessionsRequest) (storage.Page[defs.Session, RecordCursor[defs.SessionID]], error) {
	// 1. 检查对话 ID，按范围、排序和游标读取一页记录。
	var result storage.Page[defs.Session, RecordCursor[defs.SessionID]]
	if request.ConversationID == "" {
		return result, errors.New("session list requires conversation ID")
	}
	start, end := sessionRange(request.ConversationID)
	page, err := d.records.List(ctx, storage.ListRecordsRequest{
		Namespace: SessionNamespace,
		Start:     start,
		End:       end,
		Order:     request.Order,
		Cursor:    getStorageCursor(request.Cursor, sessionKey(request.ConversationID, request.Cursor.ID)),
		Limit:     request.Limit,
	})
	if err != nil {
		return result, fmt.Errorf("list Sessions: %w", err)
	}
	// 2. 解码本页记录并填写下一页位置。
	result.Items = make([]defs.Session, len(page.Items))
	for index, record := range page.Items {
		if result.Items[index], err = decodeSession(record); err != nil {
			return storage.Page[defs.Session, RecordCursor[defs.SessionID]]{}, err
		}
	}
	if page.Next != (storage.RecordCursor{}) {
		last := result.Items[len(result.Items)-1]
		result.Next = RecordCursor[defs.SessionID]{
			ID:        last.ID,
			UpdatedAt: last.UpdatedAt,
		}
	}
	return result, nil
}

// ScanSessions 分页读取对话内的 Session，按指定排序逐条交给调用方。
func (d *sessionDAOImpl) ScanSessions(ctx context.Context, conversationID defs.ConversationID, order storage.Order) iter.Seq2[defs.Session, error] {
	// 1. 分页读取并逐条交给调用方，末页、读取错误或调用方停止时结束。
	return func(yield func(defs.Session, error) bool) {
		var cursor RecordCursor[defs.SessionID]
		for {
			page, err := d.ListSessions(ctx, ListSessionsRequest{
				ConversationID: conversationID,
				Order:          order,
				Cursor:         cursor,
				Limit:          scanPageSize,
			})
			if err != nil {
				yield(defs.Session{}, err)
				return
			}
			for _, value := range page.Items {
				if !yield(value, nil) {
					return
				}
			}
			if page.Next == (RecordCursor[defs.SessionID]{}) {
				return
			}
			cursor = page.Next
		}
	}
}

// decodeSession 解码并检查 Session 记录。
func decodeSession(record storage.Record) (defs.Session, error) {
	// 1. 解码载荷，检查 Key 与实体 ID 及实体字段。
	value, err := decodePayload[defs.Session](record.Payload)
	if err != nil {
		return defs.Session{}, fmt.Errorf("decode Session %q: %w", record.Key, err)
	}
	if record.Key != sessionKey(value.ConversationID, value.ID) {
		return defs.Session{}, fmt.Errorf("stored Session %q key differs from identity", record.Key)
	}
	if err := validateSession(value); err != nil {
		return defs.Session{}, fmt.Errorf("decode Session %q: %w", record.Key, err)
	}
	// 2. 更新时间取记录的 UpdatedAt。
	value.UpdatedAt = record.UpdatedAt
	return value, nil
}

// validateSession 检查 Session 的必填字段。
func validateSession(value defs.Session) error {
	// 1. 检查实体 ID、对话 ID 和 Agent 名称。
	if value.ID == "" || value.ConversationID == "" || value.AgentName == "" {
		return errors.New("session requires an ID, conversation ID and agent name")
	}
	return nil
}
