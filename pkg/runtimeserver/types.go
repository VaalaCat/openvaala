package runtimeserver

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
)

// ── ServerConnection 运行记录 ──

// sessionCaller 保存握手验签后绑定到连接的会话身份。
type sessionCaller struct {
	conversationID defs.ConversationID
	sessionID      defs.SessionID
}

// ── ServerConnection 方法授权 ──

// sessionMethodScope 指定会话 Token 调用方法时允许的目标范围。
type sessionMethodScope string

const (
	sessionMethodGlobal            sessionMethodScope = "global"
	sessionMethodConversation      sessionMethodScope = "conversation"
	sessionMethodBoundConversation sessionMethodScope = "bound_conversation"
	sessionMethodDescendant        sessionMethodScope = "descendant"
)

// ── ServerConnection 接收帧与方法 ──

// receivedFrame 由连接读任务在读到帧或读失败时交给请求循环；读失败为最后一项，payload 为空。
type receivedFrame struct {
	payload []byte
	err     error
}

// deferredResponse 在请求的串行操作完成后等待最终结果。
type deferredResponse func() (any, error)

// methodHandler 是一个方法的参数、结果类型和处理函数；类型与 runtimeprotocol.Methods 里同名方法登记的一致。
type methodHandler struct {
	// params 是方法参数结构体的类型，处理函数收到它的指针。
	params reflect.Type
	// result 是线上响应中的结果类型。
	result reflect.Type
	// handle 处理一个已通过初始化检查的请求，返回结果、延后回复任务或错误。
	handle func(ctx context.Context, connection *serverConnection, params json.RawMessage) (any, error)
}

// subscriptionKind 标识连接内的目标订阅、全局运行集合订阅或运行中终端列表订阅。
type subscriptionKind string

const (
	subscriptionKindTarget               subscriptionKind = "target"
	subscriptionKindRunningConversations subscriptionKind = "running_conversations"
	subscriptionKindTerminals            subscriptionKind = "terminals"
	subscriptionKindWorld                subscriptionKind = "world"
)

// subscriptionKey 标识连接内的订阅种类和目标：对话订阅的 sessionID 为空。
type subscriptionKey struct {
	kind           subscriptionKind
	conversationID defs.ConversationID
	sessionID      defs.SessionID
}

// connectionSubscription 是一个订阅在连接里的登记。
type connectionSubscription struct {
	// cancel 取消订阅 ctx，Runtime 随之退订并关闭事件流。
	cancel context.CancelFunc
	// forwarderDone 在该订阅的转发任务结束时关闭；正常退订时，剩余通知与 subscription/closed 已入队。
	forwarderDone chan struct{}
}

// ── 本地 MCP 工具参数 ──

type terminalOpenToolParams struct {
	ExecutorID string `json:"executor_id,omitempty"`
	Directory  string `json:"directory,omitempty"`
	Rows       uint16 `json:"rows,omitempty"`
	Cols       uint16 `json:"cols,omitempty"`
}

type terminalSendKeysToolParams struct {
	TerminalID string `json:"terminal_id"`
	Keys       string `json:"keys"`
}

type searchSkillsToolParams struct {
	Query string `json:"query"`
}

type getSkillToolParams struct {
	Name string `json:"name"`
	File string `json:"file,omitempty"`
}

// ServeRestartRequest 由已提交 World 设置的连接在写出响应后通知 serve 重启。
type ServeRestartRequest struct{ ArchiveWorldServer bool }

// outgoingFrame 是连接 writer 待写的帧，保存 World 设置的响应需要等待写出。
type outgoingFrame struct {
	message *runtimeprotocol.JSONRPCMessage
	written chan struct{}
}
