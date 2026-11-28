package world

import (
	"context"
	"encoding/json"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
)

type sendToolArgs struct {
	To       string `json:"to"`
	Message  string `json:"message"`
	ThreadID string `json:"thread_id,omitempty"`
	Subject  string `json:"subject,omitempty"`
	// 是否在本轮等待远端回复？
	Wait bool `json:"wait,omitempty" jsonschema:"default=true"`
}

type getToolArgs struct {
	MessageID string `json:"message_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
}

type listPeersTool struct {
	*executor.BaseTool[struct{}]
	node *worldNodeImpl
}
type sendMessageTool struct {
	*executor.BaseTool[sendToolArgs]
	node           *worldNodeImpl
	conversationID string
}
type getMessageTool struct {
	*executor.BaseTool[getToolArgs]
	node           *worldNodeImpl
	conversationID string
}

// GetTools 为调用方对话绑定联系人、发信与查询工具。
func (n *worldNodeImpl) GetTools(_ context.Context, conversationID defs.ConversationID) ([]executor.Tool, error) {
	// 1. 由参数结构生成工具 schema，来源对话只从绑定信息取得。
	list, err := executor.NewBaseTool[struct{}](&executor.BaseToolOpt{
		Name:        "list_world_peers",
		Description: "列出当前 World 网络中可见的远程联系人（其他 OpenVaala 节点或桥接的外部 Agent）、在线状态和开放目标说明。",
	})
	if err != nil {
		return nil, err
	}
	send, err := executor.NewBaseTool[sendToolArgs](&executor.BaseToolOpt{
		Name:        "send_world_message",
		Description: "向远程联系人（节点或节点下的开放目标）发送消息、沿 thread_id 继续追问或转发任务。默认等待回复，对方离线时立即返回 pending_offline。",
	})
	if err != nil {
		return nil, err
	}
	get, err := executor.NewBaseTool[getToolArgs](&executor.BaseToolOpt{
		Name:        "get_world_message",
		Description: "按 message_id 或 thread_id 查询本对话发往远程联系人（节点或开放目标）的消息状态和回复。",
	})
	if err != nil {
		return nil, err
	}
	return []executor.Tool{&listPeersTool{list, n}, &sendMessageTool{send, n, string(conversationID)}, &getMessageTool{get, n, string(conversationID)}}, nil
}

func (t *listPeersTool) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 从节点循环读取当前可见的联系人。
	if _, err := t.ParseArguments(call.ToolCall.Arguments); err != nil {
		return toolResult(call, nil, &worldError{"invalid_arguments", err.Error()})
	}
	result, err := t.node.ListPeers(ctx)
	return toolResult(call, result, err)
}

func (t *sendMessageTool) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 在绑定的来源对话下发送，移交等待时保留异步唤醒责任。
	args, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return toolResult(call, nil, &worldError{"invalid_arguments", err.Error()})
	}
	outboundMessage, err := t.node.sendMessage(ctx, SendMessageRequest{
		To:                   args.To,
		Message:              args.Message,
		Subject:              args.Subject,
		ThreadID:             args.ThreadID,
		Wait:                 args.Wait,
		SourceConversationID: t.conversationID,
	}, true)
	return toolResult(call, toolMessage(outboundMessage), err)
}

func (t *getMessageTool) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 只返回绑定对话发出的消息。
	args, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return toolResult(call, nil, &worldError{"invalid_arguments", err.Error()})
	}
	if args.MessageID != "" {
		outboundMessage, err := t.node.getMessage(ctx, args.MessageID, &t.conversationID)
		return toolResult(call, toolMessage(outboundMessage), err)
	}
	if args.ThreadID == "" {
		return toolResult(call, nil, &worldError{"invalid_arguments", "message_id or thread_id is required"})
	}
	outboundMessages, err := listRecords[WorldMessage](ctx, t.node.opt.Records, worldMessages, matchField("thread_id", args.ThreadID), matchField("conversation_id", t.conversationID), matchField("direction", string(WorldMessageDirectionOutbound)))
	return toolResult(call, outboundMessages, err)
}

func toolMessage(outboundMessage WorldMessage) map[string]string {
	// 1. 返回可继续追问的标识、状态与回复。
	result := map[string]string{"message_id": outboundMessage.MessageID, "thread_id": outboundMessage.ThreadID, "status": string(outboundMessage.Status)}
	if outboundMessage.Status == WorldMessageStatusReplied {
		result["reply"] = outboundMessage.ReplyBody
	}
	if outboundMessage.ErrorText != "" {
		result["error"] = outboundMessage.ErrorText
		result["error_code"] = outboundMessage.ErrorCode
	}
	return result
}

func toolResult(call *defs.Item, value any, err error) *executor.ToolExecutionResult {
	// 1. 保留工具调用标识并编码响应。
	result := &defs.ToolResult{CallID: call.ToolCall.CallID}
	if err == nil {
		result.Content, err = json.Marshal(value)
	}
	if err != nil {
		result.Error = &defs.ToolError{Code: ErrorCode(err), Message: err.Error()}
	}
	return &executor.ToolExecutionResult{Result: result}
}
