package dao

import (
	"context"
	"errors"
	"fmt"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// ConversationNamespace 是没归档对话的索引记录所在的记录分组；索引记录由 metadata 日志生成。
const ConversationNamespace = "conversations"

// ArchivedConversationNamespace 是已归档对话的索引记录所在的记录分组。
const ArchivedConversationNamespace = "archived_conversations"

// ConversationQuery 读取对话记录。
type ConversationQuery interface {
	// GetConversation 读取指定对话，返回记录及是否找到。
	GetConversation(context.Context, defs.ConversationID) (defs.Conversation, bool, error)
	// ListConversations 返回筛选后的一页对话及各自的根 Session；根 Session 记录缺失或损坏时返回错误。
	ListConversations(context.Context, ListConversationsRequest) (storage.Page[ConversationListEntry, RecordCursor[defs.ConversationID]], error)
}

// ConversationMutation 保存对话记录。
type ConversationMutation interface {
	// SetConversation 追加对话的 metadata 日志，写入归档状态对应分组的索引，并删掉另一个分组里的旧索引。
	SetConversation(context.Context, defs.Conversation) error
}

// conversationDAOImpl 读写对话的 metadata 日志和索引记录。
type conversationDAOImpl struct {
	records storage.RecordStorage
	logs    storage.LogStorage
}

// ConversationListEntry 是对话列表里的一条：对话记录和它的根 Session。
type ConversationListEntry struct {
	Conversation defs.Conversation `json:"conversation"`
	RootSession  defs.Session      `json:"root_session"`
}

// ListConversationsRequest 是分页列出对话的筛选条件和翻页位置。
type ListConversationsRequest struct {
	// Directories 精确匹配 local 工作目录；为空时不限目录，多个目录命中任意一个。
	Directories []string
	// SearchTerm 按区分大小写的普通子串匹配标题；为空时不筛选标题。
	SearchTerm string
	Order      storage.Order
	// Cursor 是上一页最后一条的位置；零值从头开始。
	Cursor RecordCursor[defs.ConversationID]
	Limit  int
	// Archived 是否只列归档的对话？false 时只列没归档的。
	Archived bool
}

// SetConversation 检查 ID 后追加 metadata 日志并写入索引，再删掉另一个归档分组里的旧索引。
func (d *conversationDAOImpl) SetConversation(ctx context.Context, value defs.Conversation) error {
	// 1. 检查对话和根 Session 的 ID。
	if value.ID == "" || value.RootSessionID == "" {
		return errors.New("conversation requires own and root session IDs")
	}
	// 2. 清空载荷里的更新时间，追加 metadata 日志，再用日志行的追加时间写入索引。
	value.UpdatedAt = 0
	if err := saveMetadata(ctx, d.logs, d.records, metadataEntry{
		Kind:         metadataConversationSaved,
		Conversation: &value,
	}); err != nil {
		return fmt.Errorf("save Conversation %q: %w", value.ID, err)
	}
	// 3. 删除另一个归档分组的旧索引；失败时返回错误，对话会同时出现在两个列表里，下次保存或重建索引后恢复。
	if err := d.records.Delete(ctx, getConversationNamespace(value.ArchivedAt == 0), storage.RecordKey(value.ID)); err != nil {
		return fmt.Errorf("save Conversation %q: metadata saved; previous index not removed: %w", value.ID, err)
	}
	return nil
}

// GetConversation 读取对话记录。
func (d *conversationDAOImpl) GetConversation(ctx context.Context, id defs.ConversationID) (defs.Conversation, bool, error) {
	// 1. 先查没归档的分组，再查归档分组；找到时解码返回，两边都没有时返回未找到。
	for _, namespace := range []string{ConversationNamespace, ArchivedConversationNamespace} {
		record, found, err := d.records.Get(ctx, namespace, storage.RecordKey(id))
		if err != nil {
			return defs.Conversation{}, false, fmt.Errorf("get Conversation %q: %w", id, err)
		}
		if !found {
			continue
		}
		value, err := decodeConversation(record)
		if err != nil {
			return defs.Conversation{}, false, err
		}
		return value, true, nil
	}
	return defs.Conversation{}, false, nil
}

// ListConversations 分页读取匹配的对话及其根 Session。
func (d *conversationDAOImpl) ListConversations(ctx context.Context, request ListConversationsRequest) (storage.Page[ConversationListEntry, RecordCursor[defs.ConversationID]], error) {
	// 1. 组合目录和标题筛选条件。
	var result storage.Page[ConversationListEntry, RecordCursor[defs.ConversationID]]
	filter := storage.RecordFilter{Logic: storage.RecordFilterAnd}
	if len(request.Directories) > 0 {
		filter.Conditions = append(filter.Conditions, storage.RecordFilterCondition{
			Field:    "directory",
			Operator: storage.RecordFilterIn,
			Values:   request.Directories,
		})
	}
	if request.SearchTerm != "" {
		filter.Conditions = append(filter.Conditions, storage.RecordFilterCondition{
			Field:    "title",
			Operator: storage.RecordFilterContains,
			Values:   []string{request.SearchTerm},
		})
	}
	// 2. 按归档状态、排序和游标读取一页对话。
	page, err := d.records.List(ctx, storage.ListRecordsRequest{
		Namespace: getConversationNamespace(request.Archived),
		Order:     request.Order,
		Cursor:    getStorageCursor(request.Cursor, storage.RecordKey(request.Cursor.ID)),
		Limit:     request.Limit,
		Filter:    filter,
	})
	if err != nil {
		return result, fmt.Errorf("list Conversations: %w", err)
	}
	// 3. 解码对话并收集根 Session 的记录 Key。
	result.Items = make([]ConversationListEntry, len(page.Items))
	rootSessionKeys := make([]storage.RecordKey, len(page.Items))
	for index, record := range page.Items {
		conversation, err := decodeConversation(record)
		if err != nil {
			return storage.Page[ConversationListEntry, RecordCursor[defs.ConversationID]]{}, err
		}
		result.Items[index].Conversation = conversation
		rootSessionKeys[index] = sessionKey(conversation.ID, conversation.RootSessionID)
	}
	if len(rootSessionKeys) == 0 {
		return result, nil
	}
	// 4. 批量读取根 Session；缺失或损坏时返回错误。
	rootSessionRecords, err := d.records.MGet(ctx, SessionNamespace, rootSessionKeys)
	if err != nil {
		return storage.Page[ConversationListEntry, RecordCursor[defs.ConversationID]]{}, fmt.Errorf("list Conversations root sessions: %w", err)
	}
	for index, key := range rootSessionKeys {
		conversation := result.Items[index].Conversation
		record, found := rootSessionRecords[key]
		if !found {
			return storage.Page[ConversationListEntry, RecordCursor[defs.ConversationID]]{}, fmt.Errorf("conversation %q root session %q not found", conversation.ID, conversation.RootSessionID)
		}
		if result.Items[index].RootSession, err = decodeSession(record); err != nil {
			return storage.Page[ConversationListEntry, RecordCursor[defs.ConversationID]]{}, err
		}
	}
	// 5. 还有下一页时，用本页最后一条对话的 ID 和更新时间作为下一页位置。
	if page.Next != (storage.RecordCursor{}) {
		last := result.Items[len(result.Items)-1].Conversation
		result.Next = RecordCursor[defs.ConversationID]{
			ID:        last.ID,
			UpdatedAt: last.UpdatedAt,
		}
	}
	return result, nil
}

// DeleteConversation 先追加 conversation_deleted，再依次删除 Turn 索引、Session 索引、对话索引，最后删除对话日志。
func (d *daoImpl) DeleteConversation(ctx context.Context, conversationID defs.ConversationID) error {
	// 1. 读取对话，不存在时直接返回。
	_, found, err := d.GetConversation(ctx, conversationID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	// 2. 先追加 conversation_deleted，失败时停止删除。
	records, logs := d.conversationDAOImpl.records, d.conversationDAOImpl.logs
	entry := metadataEntry{
		Kind:                  metadataConversationDeleted,
		DeletedConversationID: conversationID,
	}
	if _, err := appendMetadata(ctx, logs, conversationID, entry); err != nil {
		return fmt.Errorf("delete Conversation %q: %w", conversationID, err)
	}
	// 3. 逐个删除 Session 下的 Turn 索引；失败时返回错误，可以重新调用。
	for session, err := range d.ScanSessions(ctx, conversationID, storage.Order{Direction: storage.DirectionAsc}) {
		if err != nil {
			return fmt.Errorf("delete Conversation %q: %w", conversationID, err)
		}
		start, end := turnRange(session.ID)
		if err := records.DeleteRange(ctx, turnNamespace, start, end); err != nil {
			return fmt.Errorf("delete Session %q Turns: %w", session.ID, err)
		}
	}
	// 4. 删除全部 Session 索引和两个分组中的对话索引。
	start, end := sessionRange(conversationID)
	if err := records.DeleteRange(ctx, SessionNamespace, start, end); err != nil {
		return fmt.Errorf("delete Conversation %q Sessions: %w", conversationID, err)
	}
	for _, namespace := range []string{ConversationNamespace, ArchivedConversationNamespace} {
		if err := records.Delete(ctx, namespace, storage.RecordKey(conversationID)); err != nil {
			return fmt.Errorf("delete Conversation %q: %w", conversationID, err)
		}
	}
	// 5. 最后删除对话日志；失败时返回错误，重建索引时会按 conversation_deleted 继续删除。
	if err := logs.DeletePath(ctx, storage.StreamPath(conversationID)); err != nil {
		return fmt.Errorf("delete Conversation %q logs: %w", conversationID, err)
	}
	return nil
}

// decodeConversation 解码并检查对话记录。
func decodeConversation(record storage.Record) (defs.Conversation, error) {
	// 1. 解码载荷，检查记录 Key、对话 ID 和根 Session ID。
	value, err := decodePayload[defs.Conversation](record.Payload)
	if err != nil {
		return defs.Conversation{}, fmt.Errorf("decode Conversation %q: %w", record.Key, err)
	}
	if record.Key != storage.RecordKey(value.ID) || value.RootSessionID == "" {
		return defs.Conversation{}, fmt.Errorf("stored Conversation %q identity is invalid", record.Key)
	}
	// 2. 更新时间取记录的 UpdatedAt。
	value.UpdatedAt = record.UpdatedAt
	return value, nil
}
