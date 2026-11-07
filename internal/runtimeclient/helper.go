package runtimeclient

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
)

// forkBoundarySentinels 是服务端以 -32602 返回、消息里带着原文的分叉截取点错误。
var forkBoundarySentinels = []error{conversation.ErrForkBoundaryConflict, conversation.ErrForkTurnNotFound}

// getResponseError 将服务端错误转为可识别的 Runtime 错误，或附带方法名的协议错误。
func getResponseError(method string, responseErr *ResponseError) error {
	// 1. 按错误码和分叉截取点错误原文恢复哨兵错误，保留服务端文本。
	switch responseErr.Code {
	case runtimeprotocol.ErrorCodeInvalidParams:
		for _, sentinel := range forkBoundarySentinels {
			if strings.Contains(responseErr.Message, sentinel.Error()) {
				return &serverRuntimeError{
					message:  responseErr.Message,
					sentinel: sentinel,
				}
			}
		}
	case runtimeprotocol.ErrorCodeSessionBusy:
		return &serverRuntimeError{
			message:  responseErr.Message,
			sentinel: session.ErrSessionBusy,
		}
	case runtimeprotocol.ErrorCodeConversationNotFound:
		return &serverRuntimeError{
			message:  responseErr.Message,
			sentinel: runtime.ErrConversationNotFound,
		}
	case runtimeprotocol.ErrorCodeSessionNotFound:
		return &serverRuntimeError{
			message:  responseErr.Message,
			sentinel: runtime.ErrSessionNotFound,
		}
	}
	// 2. 其余协议错误附上请求方法名。
	return fmt.Errorf("%s: %w", method, responseErr)
}

// getReconnectDelay 返回重连前的退避等待时长。
func getReconnectDelay(attempt int) time.Duration {
	// 1. attempt 从 1 开始，前四次按 1、2、4、8 秒返回，第五次起等待 10 秒。
	if attempt >= 5 {
		return 10 * time.Second
	}
	return time.Duration(1<<(attempt-1)) * time.Second
}

// redactDialError 隐藏拨号错误中的 URL 认证信息。
func redactDialError(err error) error {
	// 1. 非 URL 错误原样返回。
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	// 2. 复制 URL 错误并替换其中的地址，保留错误链。
	redacted := *urlErr
	redacted.URL = RedactURL(urlErr.URL)
	return &redacted
}

// RedactURL 返回隐去认证信息（用户信息、?token= 等查询参数）后的地址。
func RedactURL(rawURL string) string {
	// 1. 解析地址，解析失败时返回隐藏占位文本。
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "<unparsable url>"
	}
	// 2. 清除认证信息、查询参数和片段，返回地址。
	parsed.User, parsed.RawQuery, parsed.Fragment, parsed.ForceQuery = nil, "", "", false
	return parsed.String()
}

// itemEventTypes 是内容订阅的 item 通知名对应的 Agent 事件类型；工具进度由 deliverContentNotification 单独交付为 ToolProgress。
var itemEventTypes = map[string]agent.AgentEventType{
	"item/started":     agent.AgentEventItemStarted,
	"item/delta":       agent.AgentEventItemDelta,
	"item/completed":   agent.AgentEventItemCompleted,
	"item/interrupted": agent.AgentEventItemInterrupted,
}

// newRunningConversationsChanged 把运行中对话集合的订阅结果变成事件；result 为 nil 表示集合未知。
func newRunningConversationsChanged(result *runtime.RunningConversations, err error) ClientEvent {
	if result == nil {
		return &RunningConversationsChanged{Err: err}
	}
	return &RunningConversationsChanged{IsKnown: true, ConversationIDs: result.ConversationIDs}
}

// newTerminalsChanged 把运行中终端列表的订阅结果变成事件；result 为 nil 表示列表未知。
func newTerminalsChanged(result *runtimeprotocol.TerminalListResult, err error) ClientEvent {
	if result == nil {
		return &TerminalsChanged{Err: err}
	}
	return &TerminalsChanged{IsKnown: true, Terminals: result.Terminals}
}
