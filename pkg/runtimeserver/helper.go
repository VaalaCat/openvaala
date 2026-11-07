package runtimeserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strconv"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/coder/websocket"
)

// defaultPageLimit 是分页请求省略 limit 时的每页条数。
const defaultPageLimit = 50

// nullID 是无法取得请求 ID 时错误响应使用的 id。
var nullID = json.RawMessage("null")

// newJSONRPCError 创建带错误码和文本的协议错误。
func newJSONRPCError(code int, message string) *runtimeprotocol.JSONRPCError {
	// 1. 填入错误码和文本。
	return &runtimeprotocol.JSONRPCError{
		Code:    code,
		Message: message,
	}
}

// getJSONRPCError 将业务错误转换为带原错误文本的 JSON-RPC 错误。
func getJSONRPCError(err error) *runtimeprotocol.JSONRPCError {
	// 1. 已有协议错误原样返回。
	var protocolErr *runtimeprotocol.JSONRPCError
	if errors.As(err, &protocolErr) {
		return protocolErr
	}
	// 2. Executor 设置无效和分叉截取点错误用 -32602，Session 忙用 -32001，对话、Session 或终端不存在分别用 -32002、-32003、-32004。
	switch {
	case errors.Is(err, conversation.ErrInvalidExecutorSettings),
		errors.Is(err, conversation.ErrForkBoundaryConflict),
		errors.Is(err, conversation.ErrForkTurnNotFound):
		return newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, err.Error())
	case errors.Is(err, session.ErrSessionBusy):
		return newJSONRPCError(runtimeprotocol.ErrorCodeSessionBusy, err.Error())
	case errors.Is(err, runtime.ErrConversationNotFound):
		return newJSONRPCError(runtimeprotocol.ErrorCodeConversationNotFound, err.Error())
	case errors.Is(err, runtime.ErrSessionNotFound):
		return newJSONRPCError(runtimeprotocol.ErrorCodeSessionNotFound, err.Error())
	case errors.Is(err, terminal.ErrTerminalNotFound):
		return newJSONRPCError(runtimeprotocol.ErrorCodeTerminalNotFound, "terminal not found")
	}
	// 3. 其余错误用 -32000。
	return newJSONRPCError(runtimeprotocol.ErrorCodeRuntime, err.Error())
}

// newErrorResponse 把错误编码为失败响应。
func newErrorResponse(id json.RawMessage, err error) *runtimeprotocol.JSONRPCMessage {
	// 1. 保留请求 ID，并填入转换后的协议错误。
	return &runtimeprotocol.JSONRPCMessage{
		ID:    id,
		Error: getJSONRPCError(err),
	}
}

// getPeerAddress 提取 TCP 对端地址的主机与端口。
func getPeerAddress(remoteAddr string) (string, int) {
	// 1. 拆出主机与端口；拆分失败时返回原地址和端口 0。
	host, port, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr, 0
	}
	number, _ := strconv.Atoi(port)
	return host, number
}

// getRPCRequestID 将请求 ID 转换为遥测文本。
func getRPCRequestID(id json.RawMessage) string {
	// 1. null 返回空字符串、不采集 ID；字符串去掉 JSON 引号，数字等其余值保留原文。
	if string(id) == "null" {
		return ""
	}
	if len(id) > 0 && id[0] == '"' {
		var text string
		if err := json.Unmarshal(id, &text); err == nil {
			return text
		}
	}
	return string(id)
}

// isNormalConnectionClose 连接是否因正常关闭、对端离开或上下文取消而结束？
func isNormalConnectionClose(err error) bool {
	// 1. 检查关闭帧状态和上下文取消错误；其余错误返回 false。
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return errors.Is(err, context.Canceled)
}

// getRecordOrder 补齐并检查记录列表排序。
func getRecordOrder(order storage.Order) (storage.Order, error) {
	// 1. 补齐排序默认值，不支持的排序返回无效参数错误。
	order, err := order.WithDefaults()
	if err != nil {
		return storage.Order{}, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, err.Error())
	}
	return order, nil
}

// getLogOrder 补齐并检查日志读取方向。
func getLogOrder(direction storage.Direction) (storage.Direction, error) {
	// 1. 补齐方向默认值，不支持的方向返回无效参数错误。
	direction, err := direction.WithDefault()
	if err != nil {
		return "", newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, err.Error())
	}
	return direction, nil
}

// getPageLimit 读取请求的每页条数。
func getPageLimit(limit int) int {
	// 1. 未指定条数时返回 defaultPageLimit，否则返回指定条数。
	if limit == 0 {
		return defaultPageLimit
	}
	return limit
}

// getNonNilSlice 把 nil 切片换成空切片，JSON 编码为 [] 而不是 null。
func getNonNilSlice[T any](items []T) []T {
	// 1. 将 nil 替换为空切片，其余切片原样返回。
	if items == nil {
		return []T{}
	}
	return items
}

// toLoggedItems 将 DAO 日志条目转换为响应条目。
func toLoggedItems(items []dao.LoggedItem) []runtimeprotocol.LoggedItem {
	// 1. 按原顺序复制日志序号和 Item。
	result := make([]runtimeprotocol.LoggedItem, len(items))
	for index, logged := range items {
		result[index] = runtimeprotocol.LoggedItem{
			Ordinal: logged.Ordinal,
			Item:    logged.Item,
		}
	}
	return result
}

// toStreamingItems 将订阅时生成中的 Item 转换为响应条目。
func toStreamingItems(items []runtime.StreamingItem) []runtimeprotocol.StreamingItem {
	// 1. 按原顺序复制 Item 索引和内容。
	result := make([]runtimeprotocol.StreamingItem, len(items))
	for index, entry := range items {
		result[index] = runtimeprotocol.StreamingItem{
			ItemIndex: entry.ItemIndex,
			Item:      entry.Item,
		}
	}
	return result
}

// getTerminalItems 将运行中的终端转换为响应条目，没有终端时是空切片。
func getTerminalItems(runningTerminals []runtime.RunningTerminal) []runtimeprotocol.TerminalItem {
	// 1. 按原顺序复制终端编号、Executor、目录、启动时间和所属对话。
	items := make([]runtimeprotocol.TerminalItem, len(runningTerminals))
	for index, running := range runningTerminals {
		items[index] = runtimeprotocol.TerminalItem{
			TerminalID:     running.Info.ID,
			ExecutorID:     running.Info.EnvironmentID,
			Directory:      running.Info.Directory,
			StartedAt:      running.Info.StartedAt,
			ConversationID: running.ConversationID,
		}
	}
	return items
}

// getErrorText 读取错误文本。
func getErrorText(err error) string {
	// 1. nil 返回空文本，其余返回错误文本。
	if err == nil {
		return ""
	}
	return err.Error()
}

// newMethodHandler 创建解析参数后调用指定方法的请求处理函数，并记录参数和结果类型。
func newMethodHandler[P any, R any](
	handle func(ctx context.Context, connection *serverConnection, params *P) (R, error),
) methodHandler {
	// 1. 记录 P、R 的类型，返回接收原始 JSON 参数的处理函数。
	return methodHandler{
		params: reflect.TypeFor[P](),
		result: reflect.TypeFor[R](),
		handle: func(ctx context.Context, connection *serverConnection, raw json.RawMessage) (any, error) {
			// 1. 解析参数，解析失败时返回 -32602。
			params := new(P)
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, params); err != nil {
					return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidParams, err.Error())
				}
			}
			// 2. 调用业务方法并返回结果或错误。
			result, err := handle(ctx, connection, params)
			if err != nil {
				return nil, err
			}
			return result, nil
		},
	}
}

// newNotificationTarget 生成通知的对话、Session 和订阅定位信息。
func newNotificationTarget(key subscriptionKey, sessionID defs.SessionID) runtimeprotocol.NotificationTarget {
	// 1. key 不带 Session 时是对话订阅，否则是 Session 订阅。
	subscription := runtimeprotocol.SubscriptionKindSession
	if key.sessionID == "" {
		subscription = runtimeprotocol.SubscriptionKindConversation
	}
	// 2. 填入对话、事件所属 Session 和订阅种类。
	return runtimeprotocol.NotificationTarget{
		ConversationID: key.conversationID,
		SessionID:      sessionID,
		Subscription:   subscription,
	}
}
