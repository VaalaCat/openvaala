package world

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/gin-gonic/gin/binding"
)

func (n *worldNodeImpl) GetStatus(ctx context.Context, token string) (NodeStatus, error) {
	result, err := n.request(ctx, clientRequested{operation: "status", token: token})
	return result.status, err
}

func (n *worldNodeImpl) ListPeers(ctx context.Context) (ListPeersResult, error) {
	result, err := n.request(ctx, clientRequested{operation: "peers"})
	if err != nil {
		return ListPeersResult{}, err
	}
	recent, err := n.getRecentPeers(ctx)
	return ListPeersResult{Peers: result.peers, Recent: recent}, err
}

func (n *worldNodeImpl) SendMessage(ctx context.Context, req SendMessageRequest) (WorldMessage, error) {
	return n.sendMessage(ctx, req, false)
}

func (n *worldNodeImpl) EnqueueMessage(ctx context.Context, req SendMessageRequest) (WorldMessage, <-chan WorldMessage, error) {
	return n.enqueueMessage(ctx, req, false)
}

func (n *worldNodeImpl) WaitMessage(ctx context.Context, message WorldMessage, finished <-chan WorldMessage) (WorldMessage, error) {
	return n.waitMessage(ctx, message, finished, false)
}

func (n *worldNodeImpl) ListMessages(ctx context.Context, params ListMessagesParams) (MessagePage, error) {
	limit, cursor, err := clientPage(params.Limit, params.CursorID)
	if err != nil {
		return MessagePage{}, err
	}
	page, err := n.listMessages(ctx, params, limit, cursor)
	return MessagePage(page), err
}

func (n *worldNodeImpl) CancelMessage(ctx context.Context, messageID string) (WorldMessage, error) {
	if messageID == "" {
		return WorldMessage{}, &worldError{"invalid_arguments", "message_id is required"}
	}
	result, err := n.request(ctx, clientRequested{operation: "cancel", messageID: messageID})
	return result.worldMessage, err
}

func (n *worldNodeImpl) ListThreads(ctx context.Context, params ListThreadsParams) (ThreadPage, error) {
	limit, cursor, err := clientPage(params.Limit, params.CursorID)
	if err != nil {
		return ThreadPage{}, err
	}
	if (params.CursorID == nil) != (params.CursorUpdatedAt == nil) {
		return ThreadPage{}, &worldError{"invalid_arguments", "cursor_id and cursor_updated_at must be provided together"}
	}
	if params.CursorUpdatedAt != nil {
		cursor.UpdatedAt = *params.CursorUpdatedAt
		if cursor.UpdatedAt <= 0 {
			return ThreadPage{}, &worldError{"invalid_arguments", "cursor_updated_at must be a positive integer"}
		}
	}
	var filters []storage.RecordFilterCondition
	if params.ConversationID != "" {
		filters = append(filters, matchField("conversation_id", params.ConversationID))
	}
	order := storage.Order{By: storage.OrderByUpdatedAt, Direction: storage.DirectionDesc}
	page, err := listRecordPage[ThreadBinding](ctx, n.opt.Records, worldThreads, order, limit, cursor, filters...)
	return ThreadPage(page), err
}

// Subscribe 登记更新订阅，请求取消时由节点移除并关闭事件通道。
func (n *worldNodeImpl) Subscribe(ctx context.Context) (<-chan NodeNotification, error) {
	sub := make(chan NodeNotification, 32)
	if _, err := n.request(ctx, clientRequested{operation: "subscribe", subscription: sub}); err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() {
		_, _ = n.request(context.WithoutCancel(ctx), clientRequested{operation: "unsubscribe", subscription: sub})
	})
	return sub, nil
}

func (n *worldNodeImpl) Forward(ctx context.Context, rt runtime.Runtime, req ForwardRequest) (WorldMessage, error) {
	if err := binding.Validator.ValidateStruct(req); err != nil {
		return WorldMessage{}, &worldError{"invalid_arguments", strings.ReplaceAll(err.Error(), "ForwardRequest.", "forwardRequest.")}
	}
	conv, err := rt.GetConversation(ctx, &runtime.GetConversationRequest{ConversationID: req.ConversationID})
	if err != nil {
		return WorldMessage{}, err
	}
	// 2. 倒序找最近的助手回复，或收集所有选中项后还原会话顺序。
	wanted := make(map[defs.ItemID]bool, len(req.ItemIDs))
	for _, id := range req.ItemIDs {
		wanted[id] = true
	}
	query := runtime.ListItemsRequest{
		ConversationID: conv.ID,
		SessionID:      conv.RootSessionID,
		Order:          storage.DirectionDesc,
		Limit:          100,
	}
	var parts []string
	for {
		page, err := rt.ListItems(ctx, &query)
		if err != nil {
			return WorldMessage{}, err
		}
		for _, logged := range page.Items {
			item := logged.Item
			if item == nil || item.Kind != defs.ItemMessage {
				continue
			}
			if len(req.ItemIDs) == 0 {
				if item.ModelCallID != "" && messageText(item) != "" {
					parts = append(parts, messageText(item))
					break
				}
			} else if wanted[item.ID] {
				parts = append(parts, messageText(item))
				delete(wanted, item.ID)
			}
		}
		if (len(wanted) == 0 && len(parts) > 0) || page.Next == 0 {
			break
		}
		query.Cursor = page.Next
	}
	if len(parts) == 0 || len(wanted) > 0 {
		return WorldMessage{}, &worldError{"invalid_arguments", "selected conversation messages not found"}
	}
	slices.Reverse(parts)
	if req.Note != "" {
		parts = append(parts, req.Note)
	}
	return n.SendMessage(ctx, SendMessageRequest{
		To:                   req.To,
		SourceConversationID: string(conv.ID),
		Message:              strings.Join(parts, "\n\n"),
	})
}

// publish 在事件循环内广播更新；慢客户端断开后可重新查询节点记录。
func (n *worldNodeImpl) publish(name string, value any) {
	// 1. 完整编码当前事件再向各订阅分发。
	if len(n.subscribers) == 0 {
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	event := NodeNotification{Event: name, Data: data}
	for sub := range n.subscribers {
		select {
		case sub <- event:
		default:
			delete(n.subscribers, sub)
			close(sub)
		}
	}
}
