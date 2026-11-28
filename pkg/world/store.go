package world

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// getThreadPeerNode 返回会话已绑定的对端节点。
func (n *worldNodeImpl) getThreadPeerNode(ctx context.Context, threadID string) (string, error) {
	binding, found, err := getRecord[ThreadBinding](ctx, n.opt.Records, worldThreads, threadID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	peer := binding.PeerAddr
	node, _, _ := strings.Cut(peer, "/")
	return node, nil
}

// getRecentPeers 从最近二十个外域线程中按活动顺序列出对端。
func (n *worldNodeImpl) getRecentPeers(ctx context.Context) ([]RecentContact, error) {
	threads, err := listRecordPage[ThreadBinding](ctx, n.opt.Records, worldThreads, storage.Order{By: storage.OrderByUpdatedAt, Direction: storage.DirectionDesc}, 20, storage.RecordCursor{}, storage.RecordFilterCondition{Field: "peer_domain", Operator: storage.RecordFilterNotIn, Values: []string{n.address.Domain}})
	if err != nil {
		return nil, err
	}
	recent := []RecentContact{}
	seen := map[string]bool{}
	for _, thread := range threads.Items {
		node, _, _ := strings.Cut(thread.PeerAddr, "/")
		if !seen[node] {
			recent = append(recent, RecentContact{Address: thread.PeerAddr, ThreadID: thread.ThreadID})
			seen[node] = true
		}
	}
	return recent, nil
}

func (n *worldNodeImpl) listMessages(ctx context.Context, query ListMessagesParams, limit int, cursor storage.RecordCursor) (listPage[WorldMessage], error) {
	var filters []storage.RecordFilterCondition
	for _, field := range []struct{ name, value string }{
		{"direction", query.Direction},
		{"kind", query.Kind},
		{"status", query.Status},
		{"conversation_id", query.ConversationID},
		{"thread_id", query.ThreadID},
	} {
		if field.value != "" {
			filters = append(filters, matchField(field.name, field.value))
		}
	}
	addresses := slices.DeleteFunc(slices.Clone(query.PeerAddresses), func(address string) bool { return address == "" })
	if len(addresses) > 0 {
		filters = append(filters, matchField("peer_addr", addresses...))
	}
	order := storage.Order{By: storage.OrderByKey, Direction: storage.DirectionDesc}
	return listRecordPage[WorldMessage](ctx, n.opt.Records, worldMessages, order, limit, cursor, filters...)
}

func (n *worldNodeImpl) getMessage(ctx context.Context, messageID string, sourceConversationID *string) (WorldMessage, error) {
	message, found, err := getRecord[WorldMessage](ctx, n.opt.Records, worldMessages, messageID)
	if err != nil {
		return WorldMessage{}, err
	}
	if !found || (sourceConversationID != nil &&
		(message.Direction != WorldMessageDirectionOutbound || message.ConversationID != *sourceConversationID)) {
		return WorldMessage{}, &worldError{"world_message_not_found", "world message not found"}
	}
	return message, nil
}

const (
	worldMessages = "world_messages"
	worldThreads  = "world_threads"
)

// WorldNamespaces 返回节点独立数据库的记录分组及查询索引。
func WorldNamespaces() []storage.RecordNamespace {
	// 1. 为每个可筛选的字符串字段声明同名 JSON 路径。
	namespaces := []storage.RecordNamespace{
		{Name: worldMessages, IndexedFields: []string{"direction", "peer_addr", "kind", "status", "conversation_id", "thread_id", "in_reply_to"}},
		{Name: worldThreads, IndexedFields: []string{"conversation_id", "peer_domain"}},
	}
	for i, namespace := range namespaces {
		for _, field := range namespace.IndexedFields {
			namespaces[i].Fields = append(namespaces[i].Fields, storage.RecordField{
				Name:            field,
				PayloadJSONPath: []string{field},
			})
		}
	}
	return namespaces
}

func listRecordPage[T any](ctx context.Context, records storage.RecordStorage, namespace string, order storage.Order, limit int, cursor storage.RecordCursor, conditions ...storage.RecordFilterCondition) (listPage[T], error) {
	// 1. 按指定游标读取一页记录并解码。
	page, err := records.List(ctx, storage.ListRecordsRequest{
		Namespace: namespace,
		Order:     order,
		Limit:     limit,
		Cursor:    cursor,
		Filter:    storage.RecordFilter{Conditions: conditions},
	})
	if err != nil {
		return listPage[T]{}, err
	}
	result := listPage[T]{Items: make([]T, len(page.Items))}
	for i, record := range page.Items {
		if err := json.Unmarshal(record.Payload, &result.Items[i]); err != nil {
			return listPage[T]{}, err
		}
	}
	// 2. 返回存储游标中参与排序的字段。
	if page.Next != (storage.RecordCursor{}) {
		result.NextCursor.ID = string(page.Next.Key)
		if order.By == storage.OrderByUpdatedAt {
			result.NextCursor.UpdatedAt = page.Next.UpdatedAt
		}
	}
	return result, nil
}

func putRecord(ctx context.Context, records storage.RecordStorage, namespace, key string, updatedAt int64, entry any) error {
	// 1. 用业务主键和更新时间写入完整记录。
	payload, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return records.Set(ctx, namespace, storage.Record{
		Key:       storage.RecordKey(key),
		UpdatedAt: updatedAt,
		Payload:   payload,
	})
}

func getRecord[T any](ctx context.Context, records storage.RecordStorage, namespace, key string) (entry T, found bool, err error) {
	// 1. 读取并解码命中的单条记录。
	record, found, err := records.Get(ctx, namespace, storage.RecordKey(key))
	if err != nil || !found {
		return entry, found, err
	}
	err = json.Unmarshal(record.Payload, &entry)
	return entry, true, err
}

func listRecords[T any](ctx context.Context, records storage.RecordStorage, namespace string, conditions ...storage.RecordFilterCondition) ([]T, error) {
	// 1. 用相同的筛选条件按主键翻页。
	request := storage.ListRecordsRequest{
		Namespace: namespace,
		Limit:     128,
		Filter:    storage.RecordFilter{Conditions: conditions},
	}
	entries := make([]T, 0)
	for {
		page, err := records.List(ctx, request)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Items {
			var entry T
			if err = json.Unmarshal(record.Payload, &entry); err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
		if page.Next == (storage.RecordCursor{}) {
			return entries, nil
		}
		request.Cursor = page.Next
	}
}

func matchField(field string, values ...string) storage.RecordFilterCondition {
	// 1. 按索引字段匹配所列字符串值。
	return storage.RecordFilterCondition{
		Field:    field,
		Operator: storage.RecordFilterIn,
		Values:   values,
	}
}
