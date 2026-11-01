package dao

import (
	"encoding/json"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// decodePayload 将 JSON 载荷解码为指定实体。
func decodePayload[T any](payload json.RawMessage) (T, error) {
	// 1. 解码载荷，返回实体和解码错误。
	var value T
	err := json.Unmarshal(payload, &value)
	return value, err
}

// getConversationNamespace 返回对话归档状态对应的记录分组。
func getConversationNamespace(isArchived bool) string {
	// 1. 归档对话使用归档分组，其余使用普通对话分组。
	if isArchived {
		return ArchivedConversationNamespace
	}
	return ConversationNamespace
}

// turnKeyPrefix 返回 Session 下 Turn 记录的 Key 前缀。
func turnKeyPrefix(sessionID defs.SessionID) storage.RecordKey {
	// 1. 在 Session ID 后添加斜杠。
	return storage.RecordKey(string(sessionID) + "/")
}

// turnKey 拼接 Session 下的 Turn 记录 Key。
func turnKey(sessionID defs.SessionID, turnID defs.TurnID) storage.RecordKey {
	// 1. 将 Turn ID 接到 Session 的 Key 前缀后。
	return turnKeyPrefix(sessionID) + storage.RecordKey(turnID)
}

// turnRange 返回 Session 下 Turn 记录的 [start, end) 范围。
func turnRange(sessionID defs.SessionID) (storage.RecordKey, storage.RecordKey) {
	// 1. 按 Session 的 Key 前缀计算范围。
	return storage.PrefixRange(turnKeyPrefix(sessionID))
}

// sessionKeyPrefix 返回对话下 Session 记录的 Key 前缀。
func sessionKeyPrefix(conversationID defs.ConversationID) storage.RecordKey {
	// 1. 在对话 ID 后添加斜杠。
	return storage.RecordKey(string(conversationID) + "/")
}

// sessionKey 拼接对话下的 Session 记录 Key。
func sessionKey(conversationID defs.ConversationID, id defs.SessionID) storage.RecordKey {
	// 1. 将 Session ID 接到对话的 Key 前缀后。
	return sessionKeyPrefix(conversationID) + storage.RecordKey(id)
}

// sessionRange 返回对话下 Session 记录的 [start, end) 范围。
func sessionRange(conversationID defs.ConversationID) (storage.RecordKey, storage.RecordKey) {
	// 1. 按对话的 Key 前缀计算范围。
	return storage.PrefixRange(sessionKeyPrefix(conversationID))
}

// getStorageCursor 将实体分页位置转换为存储游标。
func getStorageCursor[ID ~string](cursor RecordCursor[ID], key storage.RecordKey) storage.RecordCursor {
	// 1. ID 为空时返回起始游标，否则使用记录 Key 和更新时间。
	if cursor.ID == "" {
		return storage.RecordCursor{}
	}
	return storage.RecordCursor{
		Key:       key,
		UpdatedAt: cursor.UpdatedAt,
	}
}
