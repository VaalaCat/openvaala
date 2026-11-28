package world

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
)

// WorldHost 打开来信会话、执行来信并接收异步回信。
type WorldHost interface {
	OpenSession(ctx context.Context, req OpenSessionRequest) (OpenedSession, error)
	RunInboundTurn(ctx context.Context, req InboundTurnRequest, onSubmitted func()) (string, error)
	NotifyOutbound(ctx context.Context, msg WorldMessage) error
}

type runtimeHost struct {
	rt runtime.Runtime
}

// NewRuntimeHost 让 World 使用本机 Runtime 处理消息。
func NewRuntimeHost(rt runtime.Runtime) WorldHost {
	return &runtimeHost{rt: rt}
}

func (h *runtimeHost) OpenSession(ctx context.Context, req OpenSessionRequest) (OpenedSession, error) {
	// 1. 优先打开已有对话；只有确认该对话不存在时才创建。
	var conv defs.Conversation
	var err error
	if req.ExistingSessionID != "" {
		conv, err = h.rt.OpenConversation(ctx, &runtime.OpenConversationRequest{ConversationID: defs.ConversationID(req.ExistingSessionID)})
	}
	if req.ExistingSessionID == "" || errors.Is(err, runtime.ErrConversationNotFound) {
		conv, err = h.rt.CreateConversation(ctx, &runtime.CreateConversationRequest{AgentName: req.Target})
		if err == nil {
			err = h.rt.SetConversationTitle(ctx, &runtime.SetConversationTitleRequest{ConversationID: conv.ID, Title: req.Title})
		}
	}
	return OpenedSession{SessionID: string(conv.ID), RootSessionID: conv.RootSessionID}, err
}

func (h *runtimeHost) RunInboundTurn(ctx context.Context, req InboundTurnRequest, onSubmitted func()) (string, error) {
	id := defs.ConversationID(req.OpenedSession.SessionID)
	// 1. 先订阅再投递，让快速完成的 Turn 同样可以观察到。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	request := &runtime.SubscribeSessionRequest{ConversationID: id, SessionID: req.OpenedSession.RootSessionID}
	sub, err := h.rt.SubscribeSession(ctx, request)
	for err == nil && sub.Restoring {
		// 恢复订阅只用于等待。Runtime 写入完整历史后关闭它，再登记接收本轮事件的订阅。
		for range sub.Events {
		}
		if reason := sub.GetCloseReason(); !errors.Is(reason, runtime.ErrSessionHistoryRestored) {
			return "", reason
		}
		sub, err = h.rt.SubscribeSession(ctx, request)
	}
	if err != nil {
		return "", err
	}
	itemID, err := h.rt.SubmitInput(ctx, worldInput(id, req.Message.MessageID, req.Message.Body))
	if err != nil {
		return "", err
	}
	// 2. 提交输入后对话已写入共享库，通知客户端刷新。
	onSubmitted()
	// 3. 按输入 Item 关联对应轮次，只提取最终模型调用的文本输出。
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case event, ok := <-sub.Events:
			if !ok {
				return "", errors.New("world conversation subscription closed")
			}
			sessionEvent, ok := event.(*conversation.ConversationSessionEvent)
			if !ok {
				continue
			}
			finished, ok := sessionEvent.Event.(*session.SessionTurnFinished)
			if !ok || !slices.Contains(finished.InputItemIDs, itemID) {
				continue
			}
			if finished.Err != nil {
				return "", finished.Err
			}
			if finished.Status != defs.TurnStatusCompleted {
				return "", fmt.Errorf("world turn finished with status %q", finished.Status)
			}
			var reply strings.Builder
			for _, item := range finished.Outputs {
				if item.Kind == defs.ItemMessage && item.ModelCallID == finished.FinalModelCallID {
					reply.WriteString(messageText(item))
				}
			}
			return reply.String(), nil
		}
	}
}

func (h *runtimeHost) NotifyOutbound(ctx context.Context, msg WorldMessage) error {
	// 1. 以消息 ID 去重，将回信交给来源对话。
	id := defs.ConversationID(msg.ConversationID)
	record, err := h.rt.OpenConversation(ctx, &runtime.OpenConversationRequest{ConversationID: id})
	if err != nil {
		return err
	}
	input := worldInput(id, msg.MessageID, "[回信] "+msg.PeerAddr+"\n"+cmp.Or(msg.ErrorText, msg.ReplyBody))
	root, err := h.rt.GetSession(ctx, id, record.RootSessionID)
	if err != nil {
		return err
	}
	input.Mode = session.SessionInputModeSteer
	if root.External != nil {
		details, err := h.rt.GetSessionDetails(ctx, &runtime.GetSessionDetailsRequest{ConversationID: id, SessionID: root.ID})
		if err != nil {
			return err
		}
		if details.Capabilities == nil || !details.Capabilities.SupportsSteer {
			input.Mode = session.SessionInputModeNormal
		}
	}
	_, err = h.rt.SubmitInput(ctx, input)
	return err
}
