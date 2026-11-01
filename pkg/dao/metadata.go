package dao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// MetadataStreamID 是每个对话目录里保存 Conversation、Session、Turn 记录的 Stream，文件名为 metadata.jsonl。
const MetadataStreamID storage.StreamID = "metadata"

// metadataEntryKind 是 metadata.jsonl 中一行记录的种类。
type metadataEntryKind string

const (
	// metadataConversationSaved 由 SetConversation 在对话记录变化时写入，例如创建、分叉、改标题、改 Executor 设置、归档或取消归档、更新消息数。
	metadataConversationSaved metadataEntryKind = "conversation_saved"
	// metadataSessionSaved 由 SetSession 在创建 Session 或修改它的模型选择时写入。
	metadataSessionSaved metadataEntryKind = "session_saved"
	// metadataTurnSaved 由 SetTurn 在 Turn 开始、写入首批 Item、结束、被修复为中断或随分叉复制时写入。
	metadataTurnSaved metadataEntryKind = "turn_saved"
	// metadataSessionDeleted 由 DeleteSession 在删除 Session 的索引和日志之前写入。
	metadataSessionDeleted metadataEntryKind = "session_deleted"
	// metadataConversationDeleted 由 DeleteConversation 在删除对话的索引和目录之前写入。
	metadataConversationDeleted metadataEntryKind = "conversation_deleted"
)

// metadataEntry 是 metadata.jsonl 一行的 payload，Kind 决定哪一个字段有值。
type metadataEntry struct {
	Kind metadataEntryKind `json:"kind"`
	// Conversation、Session、Turn 是保存时实体的完整最新状态，UpdatedAt 置零不写入。
	Conversation *defs.Conversation `json:"conversation,omitempty"`
	Session      *defs.Session      `json:"session,omitempty"`
	Turn         *defs.Turn         `json:"turn,omitempty"`
	// DeletedSessionID 是 session_deleted 删除的 Session。
	DeletedSessionID defs.SessionID `json:"deleted_session_id,omitempty"`
	// DeletedConversationID 是 conversation_deleted 删除的对话，必须是这个 metadata.jsonl 所属的对话。
	DeletedConversationID defs.ConversationID `json:"deleted_conversation_id,omitempty"`
}

// indexRecord 是一行 metadata 在索引里对应的记录分组和记录。
type indexRecord struct {
	namespace string
	record    storage.Record
}

// saveMetadata 先追加实体的 metadata 日志，再用日志的追加时间写入索引。
func saveMetadata(ctx context.Context, logs storage.LogStorage, records storage.RecordStorage, entry metadataEntry) error {
	// 1. 检查日志字段并组装索引记录。
	conversationID, index, err := entry.getIndexRecord()
	if err != nil {
		return err
	}
	// 2. 先追加 metadata，取得日志时间。
	updatedAt, err := appendMetadata(ctx, logs, conversationID, entry)
	if err != nil {
		return err
	}
	// 3. 按日志时间写索引；失败时返回错误，已落盘的日志保留，由重建索引补齐。
	index.record.UpdatedAt = updatedAt
	if err := records.Set(ctx, index.namespace, index.record); err != nil {
		return fmt.Errorf("metadata saved; index not updated: %w", err)
	}
	return nil
}

// appendMetadata 向对话 metadata 日志追加一条记录，返回追加时间。
func appendMetadata(ctx context.Context, logs storage.LogStorage, conversationID defs.ConversationID, entry metadataEntry) (int64, error) {
	// 1. 编码日志载荷。
	payload, err := json.Marshal(entry)
	if err != nil {
		return 0, fmt.Errorf("encode metadata %s: %w", entry.Kind, err)
	}
	// 2. 追加到对话的 metadata Stream，返回存储分配的追加时间。
	infos, err := logs.Append(ctx, storage.StreamPath(conversationID), MetadataStreamID, payload)
	if err != nil {
		return 0, fmt.Errorf("append metadata %s: %w", entry.Kind, err)
	}
	return infos[0].UpdatedAt, nil
}

// getIndexRecord 检查 metadata 载荷，返回所属对话和索引记录。
func (entry metadataEntry) getIndexRecord() (defs.ConversationID, indexRecord, error) {
	// 1. 要求恰好填写一个实体字段。
	if entry.getFieldCount() != 1 {
		return "", indexRecord{}, fmt.Errorf("metadata %s must carry exactly one record", entry.Kind)
	}
	// 2. 按种类和载荷选择记录分组及 Key；删除记录或种类不符时返回错误。
	var conversationID defs.ConversationID
	var index indexRecord
	var value any
	switch {
	case entry.Kind == metadataConversationSaved && entry.Conversation != nil:
		conversationID, value = entry.Conversation.ID, entry.Conversation
		index = indexRecord{
			namespace: getConversationNamespace(entry.Conversation.ArchivedAt != 0),
			record:    storage.Record{Key: storage.RecordKey(entry.Conversation.ID)},
		}
	case entry.Kind == metadataSessionSaved && entry.Session != nil:
		conversationID, value = entry.Session.ConversationID, entry.Session
		index = indexRecord{
			namespace: SessionNamespace,
			record:    storage.Record{Key: sessionKey(entry.Session.ConversationID, entry.Session.ID)},
		}
	case entry.Kind == metadataTurnSaved && entry.Turn != nil:
		conversationID, value = entry.Turn.ConversationID, entry.Turn
		index = indexRecord{
			namespace: turnNamespace,
			record:    storage.Record{Key: turnKey(entry.Turn.SessionID, entry.Turn.ID)},
		}
	default:
		return "", indexRecord{}, fmt.Errorf("metadata %s has no matching record", entry.Kind)
	}
	// 3. 编码实体载荷写入索引记录，UpdatedAt 留空。
	payload, err := json.Marshal(value)
	if err != nil {
		return "", indexRecord{}, fmt.Errorf("encode metadata %s record: %w", entry.Kind, err)
	}
	index.record.Payload = payload
	return conversationID, index, nil
}

// getFieldCount 返回 metadata 已填写的实体字段数。
func (entry metadataEntry) getFieldCount() int {
	// 1. 逐项统计非空实体和删除 ID。
	count := 0
	for _, filled := range []bool{
		entry.Conversation != nil, entry.Session != nil, entry.Turn != nil, entry.DeletedSessionID != "", entry.DeletedConversationID != "",
	} {
		if filled {
			count++
		}
	}
	return count
}

// RebuildMetadataIndexResult 是一次重建索引的结果。
type RebuildMetadataIndexResult struct {
	// RebuiltConversations 是写入索引的对话数。
	RebuiltConversations int
	// SkippedConversations 是因 metadata 不完整或损坏而没有写入索引的对话及原因；没有时为空切片。
	SkippedConversations []SkippedConversation
	// DeletedConversations 是 metadata 日志里记了删除、这次重建顺带删掉目录的对话；没有时为空切片。
	DeletedConversations []defs.ConversationID
}

// SkippedConversation 是一个因 metadata 不完整或损坏而没有写入索引的对话。
type SkippedConversation struct {
	// ConversationID 是对话 ID，也是它的日志目录名。
	ConversationID defs.ConversationID
	// Reason 是跳过的原因。
	Reason error
}

// RebuildMetadataIndex 清空对话、Session、Turn 索引后，逐个重放对话的 metadata 日志重建索引；调用期间不能有其他读写。
func RebuildMetadataIndex(ctx context.Context, opt *DAOOpt) (RebuildMetadataIndexResult, error) {
	// 1. 检查两个存储，列出有 metadata 日志的对话目录；列举失败时直接返回，索引保持原样。
	result := RebuildMetadataIndexResult{
		SkippedConversations: []SkippedConversation{},
		DeletedConversations: []defs.ConversationID{},
	}
	if opt == nil || opt.Records == nil || opt.Logs == nil {
		return result, errors.New("rebuild metadata index requires record and log storage")
	}
	paths, err := opt.Logs.GetStreamPaths(ctx, MetadataStreamID)
	if err != nil {
		return result, fmt.Errorf("list conversation metadata: %w", err)
	}
	// 2. 清空对话、Session 和 Turn 的全部索引。
	for _, namespace := range RecordNamespaces() {
		if err := opt.Records.DeleteRange(ctx, namespace.Name, "", ""); err != nil {
			return result, fmt.Errorf("clear %s index: %w", namespace.Name, err)
		}
	}
	// 3. 逐个重放对话日志，取消时返回；损坏的对话记入跳过列表，记了删除的对话删掉剩余目录。
	for _, path := range paths {
		conversationID := defs.ConversationID(path)
		indexed, isDeleted, err := getConversationIndexRecords(ctx, opt.Logs, conversationID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		if err != nil {
			result.SkippedConversations = append(result.SkippedConversations, SkippedConversation{
				ConversationID: conversationID,
				Reason:         err,
			})
			continue
		}
		if isDeleted {
			if err := opt.Logs.DeletePath(ctx, path); err != nil {
				return result, fmt.Errorf("delete Conversation %q logs: %w", conversationID, err)
			}
			result.DeletedConversations = append(result.DeletedConversations, conversationID)
			continue
		}
		// 4. 写入各实体最后一条记录对应的索引；写入失败时返回错误，可以重新运行。
		for _, namespace := range RecordNamespaces() {
			if err := opt.Records.MSet(ctx, namespace.Name, indexed[namespace.Name]); err != nil {
				return result, fmt.Errorf("index Conversation %q: %w", conversationID, err)
			}
		}
		result.RebuiltConversations++
	}
	return result, nil
}

// conversationMetadata 是按顺序重放一个对话的 metadata.jsonl 后，各实体最后一行对应的索引记录。
type conversationMetadata struct {
	// conversationID 是正在重放的对话，各行实体必须属于它。
	conversationID defs.ConversationID
	// conversation 是最后一行 conversation_saved 的索引记录；还没读到时为 nil。
	conversation *storage.Record
	// conversationNamespace 是最后一行 conversation_saved 按归档状态选的记录分组。
	conversationNamespace string
	// rootSessionID 是最后一行 conversation_saved 里的根 Session。
	rootSessionID defs.SessionID
	// isDeleted 读到过 conversation_deleted 吗？读到之后整个对话都不建索引。
	isDeleted bool
	// sessions 按 Session ID 保存每个 Session 最后一行的索引记录；session_deleted 会移除对应项。
	sessions map[defs.SessionID]storage.Record
	// turns 按 Session ID 和 Turn ID 保存每个 Turn 最后一行的索引记录。
	turns map[defs.SessionID]map[defs.TurnID]storage.Record
}

// getConversationIndexRecords 重放一个对话的 metadata 日志，返回按记录分组归好的索引记录，以及对话是否已删除。
func getConversationIndexRecords(
	ctx context.Context, logs storage.LogStorage, conversationID defs.ConversationID,
) (indexed map[string][]storage.Record, isDeleted bool, err error) {
	// 1. 创建对话重放状态，按顺序号升序读取日志。
	metadata := &conversationMetadata{
		conversationID: conversationID,
		sessions:       map[defs.SessionID]storage.Record{},
		turns:          map[defs.SessionID]map[defs.TurnID]storage.Record{},
	}
	request := storage.ListLogEntriesRequest{
		Path:     storage.StreamPath(conversationID),
		StreamID: MetadataStreamID,
		Order:    storage.DirectionAsc,
		Limit:    scanPageSize,
	}
	// 2. 逐页重放各行，末页返回索引；读取或载荷错误时返回错误。
	for {
		page, err := logs.List(ctx, request)
		if err != nil {
			return nil, false, err
		}
		for _, line := range page.Items {
			if err := metadata.replayLine(line); err != nil {
				return nil, false, fmt.Errorf("metadata ordinal %d: %w", line.Ordinal, err)
			}
		}
		if page.Next == 0 {
			return metadata.getIndexRecords()
		}
		request.Cursor = page.Next
	}
}

// replayLine 重放一行 metadata，更新实体的最新记录或删除状态。
func (m *conversationMetadata) replayLine(line storage.LogEntry) error {
	// 1. 解码日志载荷。
	entry, err := decodePayload[metadataEntry](line.Payload)
	if err != nil {
		return err
	}
	// 2. session_deleted 移除这个 Session 和它的 Turn，conversation_deleted 标记整个对话已删除；删除行带了别的字段时返回错误。
	switch entry.Kind {
	case metadataSessionDeleted:
		if entry.getFieldCount() != 1 || entry.DeletedSessionID == "" {
			return errors.New("session_deleted requires only deleted_session_id")
		}
		delete(m.sessions, entry.DeletedSessionID)
		delete(m.turns, entry.DeletedSessionID)
		return nil
	case metadataConversationDeleted:
		if entry.getFieldCount() != 1 || entry.DeletedConversationID != m.conversationID {
			return errors.New("conversation_deleted requires only this conversation's deleted_conversation_id")
		}
		m.isDeleted = true
		return nil
	}
	// 3. 检查实体属于本对话，索引时间用这一行的追加时间。
	conversationID, index, err := entry.getIndexRecord()
	if err != nil {
		return err
	}
	if conversationID != m.conversationID {
		return fmt.Errorf("%s belongs to Conversation %q", entry.Kind, conversationID)
	}
	index.record.UpdatedAt = line.UpdatedAt
	// 4. 检查实体字段并替换这个实体的上一条记录。
	switch entry.Kind {
	case metadataConversationSaved:
		if entry.Conversation.RootSessionID == "" {
			return errors.New("conversation requires a root session ID")
		}
		m.conversation, m.conversationNamespace, m.rootSessionID = &index.record, index.namespace, entry.Conversation.RootSessionID
	case metadataSessionSaved:
		if err := validateSession(*entry.Session); err != nil {
			return err
		}
		m.sessions[entry.Session.ID] = index.record
	case metadataTurnSaved:
		if err := validateTurn(*entry.Turn); err != nil {
			return err
		}
		if m.turns[entry.Turn.SessionID] == nil {
			m.turns[entry.Turn.SessionID] = map[defs.TurnID]storage.Record{}
		}
		m.turns[entry.Turn.SessionID][entry.Turn.ID] = index.record
	}
	return nil
}

// getIndexRecords 返回重放后按记录分组归好的索引记录，以及对话是否已删除。
func (m *conversationMetadata) getIndexRecords() (indexed map[string][]storage.Record, isDeleted bool, err error) {
	// 1. 已删除时只返回删除标记；否则要求读到过对话和根 Session，缺失时返回错误。
	if m.isDeleted {
		return nil, true, nil
	}
	if m.conversation == nil {
		return nil, false, errors.New("conversation record is missing")
	}
	if _, found := m.sessions[m.rootSessionID]; !found {
		return nil, false, fmt.Errorf("root Session %q is missing", m.rootSessionID)
	}
	// 2. 按分组收集对话、Session 和其下 Turn，跳过所属 Session 不存在的 Turn。
	indexed = map[string][]storage.Record{m.conversationNamespace: {*m.conversation}}
	for sessionID, session := range m.sessions {
		indexed[SessionNamespace] = append(indexed[SessionNamespace], session)
		for _, turn := range m.turns[sessionID] {
			indexed[turnNamespace] = append(indexed[turnNamespace], turn)
		}
	}
	return indexed, false, nil
}
