package conversation

import (
	"context"
	"encoding/json"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/prompt"
)

// collaborationTool 是一项协作工具：参数解析交给 BaseTool，执行时调用绑定了来源 Session 的 collaborate。
type collaborationTool[P any] struct {
	*executor.BaseTool[P]

	// collaborate 以绑定的来源 Session 调用 Conversation 的协作方法，返回值会编码成工具结果。
	collaborate func(ctx context.Context, callItem *defs.Item, param *P) (any, error)
}

// newCollaborationTool 按工具定义创建参数解析，连同协作方法组成一项工具。
func newCollaborationTool[P any](
	opt *executor.BaseToolOpt,
	collaborate func(ctx context.Context, callItem *defs.Item, param *P) (any, error),
) (executor.Tool, error) {
	// 1. 创建解析参数的 BaseTool，保存协作方法。
	base, err := executor.NewBaseTool[P](opt)
	if err != nil {
		return nil, err
	}
	return &collaborationTool[P]{
		BaseTool:    base,
		collaborate: collaborate,
	}, nil
}

// Execute 解析参数、执行协作操作并返回工具结果。
func (t *collaborationTool[P]) Execute(ctx context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析调用参数，失败时返回 invalid_arguments。
	param, err := t.ParseArguments(call.ToolCall.Arguments)
	if err != nil {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  call.ToolCall.CallID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    "invalid_arguments",
				Message: err.Error(),
			},
		}}
	}
	// 2. 执行协作操作，把返回值和错误编码成工具结果。
	payload, err := t.collaborate(ctx, call, param)
	return encodeToolResult(call.ToolCall.CallID, payload, err)
}

// newCollaborationTools 创建绑定来源 Session 的六项协作工具。
func newCollaborationTools(c *conversationImpl, source defs.Session, definitions *ConversationDefinitions) ([]executor.Tool, error) {
	// 1. 创建 spawn_agent，工具说明里列出可选的 Agent 和模型。
	spawnAgent, err := newCollaborationTool(
		&executor.BaseToolOpt{
			Name:        "spawn_agent",
			Description: prompt.FormatSpawnAgentDescription(newAgentPromptEntries(definitions.Agents), definitions.Models),
		},
		func(ctx context.Context, callItem *defs.Item, param *SpawnAgentParam) (any, error) {
			id, err := c.spawnAgent(ctx, source, callItem, param)
			return spawnAgentResult{
				SessionID: id,
				TaskName:  param.TaskName,
				Status:    "accepted",
			}, err
		})
	if err != nil {
		return nil, err
	}
	// 2. 创建 send_message，返回目标 Session 和接收状态。
	sendMessage, err := newCollaborationTool(
		&executor.BaseToolOpt{
			Name:        "send_message",
			Description: prompt.SendMessageDescription,
		},
		func(ctx context.Context, _ *defs.Item, param *SendMessageParam) (any, error) {
			err := c.sendMessage(ctx, source, param)
			return sendMessageResult{
				SessionID: param.SessionID,
				Mode:      param.Mode,
				Status:    "accepted",
			}, err
		})
	if err != nil {
		return nil, err
	}
	// 3. 创建 interrupt_agent，返回取消请求对应的 Turn。
	interruptAgent, err := newCollaborationTool(
		&executor.BaseToolOpt{
			Name:        "interrupt_agent",
			Description: prompt.InterruptAgentDescription,
		},
		func(ctx context.Context, _ *defs.Item, param *interruptAgentParam) (any, error) {
			id, err := c.interruptAgent(ctx, source, param.SessionID)
			status := "no_active_turn"
			if id != "" {
				status = "cancel_requested"
			}
			return interruptAgentResult{
				SessionID: param.SessionID,
				TurnID:    id,
				Status:    status,
			}, err
		})
	if err != nil {
		return nil, err
	}
	// 4. 创建 list_agents，返回来源 Session 的后代信息。
	listAgents, err := newCollaborationTool(
		&executor.BaseToolOpt{
			Name:        "list_agents",
			Description: prompt.ListAgentsDescription,
		},
		func(ctx context.Context, _ *defs.Item, _ *listAgentsParam) (any, error) {
			infos, err := c.listAgents(ctx, source)
			return listAgentsResult{Sessions: infos}, err
		})
	if err != nil {
		return nil, err
	}
	// 5. 创建 read_session_history，返回历史页和下一页游标。
	readSessionHistory, err := newCollaborationTool(
		&executor.BaseToolOpt{
			Name:        "read_session_history",
			Description: prompt.ReadSessionHistoryDescription,
		},
		func(ctx context.Context, _ *defs.Item, param *SessionHistoryParam) (any, error) {
			page, err := c.readSessionHistory(ctx, source, param)
			return sessionHistoryResult{
				SessionID:  param.SessionID,
				Items:      transformHistoryItems(page.Items),
				NextCursor: page.Next,
			}, err
		})
	if err != nil {
		return nil, err
	}
	// 6. 创建 wait_agent，按固定顺序返回六项工具。
	waitAgent, err := newCollaborationTool(
		&executor.BaseToolOpt{
			Name:        "wait_agent",
			Description: prompt.WaitAgentDescription,
		},
		func(ctx context.Context, _ *defs.Item, param *WaitAgentParam) (any, error) {
			return c.waitAgent(ctx, source, param)
		})
	if err != nil {
		return nil, err
	}
	return []executor.Tool{spawnAgent, sendMessage, interruptAgent, listAgents, readSessionHistory, waitAgent}, nil
}
