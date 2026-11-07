package runtimeserver

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// ── ServerConnection 运行记录 ──

// receivedFrame 由连接读任务在读到帧或读失败时交给请求循环；读失败为最后一项，payload 为空。
type receivedFrame struct {
	payload []byte
	err     error
}

// methodHandler 是一个方法的参数、结果类型和处理函数；类型与 runtimeprotocol.Methods 里同名方法登记的一致。
type methodHandler struct {
	// params 是方法参数结构体的类型，处理函数收到它的指针。
	params reflect.Type
	// result 是处理函数的返回类型。
	result reflect.Type
	// handle 处理一个已通过初始化检查的请求，返回非 nil 的结果对象或错误；ctx 是连接的生命周期。
	handle func(ctx context.Context, connection *serverConnection, params json.RawMessage) (any, error)
}

// subscriptionKind 标识连接内的目标订阅、全局运行集合订阅或运行中终端列表订阅。
type subscriptionKind string

const (
	subscriptionKindTarget               subscriptionKind = "target"
	subscriptionKindRunningConversations subscriptionKind = "running_conversations"
	subscriptionKindTerminals            subscriptionKind = "terminals"
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
