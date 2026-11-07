package conversation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// spawnAgent 创建直接子 Session 并提交首个任务。
func (c *conversationImpl) spawnAgent(ctx context.Context, source defs.Session, callItem *defs.Item, param *SpawnAgentParam) (defs.SessionID, error) {
	// 1. 校验任务内容和 Agent，按父 Session 内存记录里最近一次选择的模型选出子 Session 的模型，解析继承轮数。
	if strings.TrimSpace(param.TaskName) == "" || strings.TrimSpace(param.Message) == "" {
		return "", errors.New("spawn requires task_name and message")
	}
	agentName := param.AgentName
	if agentName == "" {
		agentName = source.AgentName
	}
	if _, found := c.config.Load().agentDefinitions[agentName]; !found {
		return "", fmt.Errorf("agent %q is not configured", agentName)
	}
	parent, _ := c.sessionRecords.Load(source.ID)
	selection, err := c.getSpawnModelSelection(parent, agentName, param)
	if err != nil {
		return "", err
	}
	limit, err := forkTurnLimit(param.ForkTurns)
	if err != nil {
		return "", err
	}
	// 2. 创建子 Session 记录，写入父 Session、Turn 和工具调用信息。
	newSessionRecord := defs.Session{
		ID:              defs.NewSessionID(),
		ConversationID:  c.id,
		AgentName:       agentName,
		TaskName:        param.TaskName,
		Model:           selection,
		ParentSessionID: source.ID,
		ParentTurnID:    callItem.TurnID,
		ParentCallID:    callItem.ToolCall.CallID,
	}

	// 3. 按 fork_turns 读取父 Session 已结束 Turn 的历史，连同 fork 记录一次写入，再保存子 Session 记录。
	var batch []*defs.Item
	if limit >= 0 {
		history, err := dao.LoadContextHistory(ctx, c.opt.Query, c.id, source.ID, callItem.TurnID)
		if err != nil {
			return "", fmt.Errorf("load context history of Session %s: %w", source.ID, err)
		}
		batch = selectForkHistory(history, limit)
	}
	batch = append(batch, &defs.Item{
		ID:          defs.NewItemID(),
		Kind:        defs.ItemSessionFork,
		SessionFork: &defs.SessionFork{SourceSessionID: source.ID},
	})
	if _, err := c.opt.Mutation.AppendItems(ctx, c.id, newSessionRecord.ID, batch...); err != nil {
		return "", fmt.Errorf("write inherited history and fork for Session %s: %w", newSessionRecord.ID, err)
	}
	if err := c.opt.Mutation.SetSession(ctx, newSessionRecord); err != nil {
		return "", fmt.Errorf("save Session %s: %w", newSessionRecord.ID, err)
	}
	c.sessionRecords.Store(newSessionRecord.ID, newSessionRecord)

	// 4. 启动子 Session，以 normal 提交首个任务；记录已保存，启动或提交失败时仍返回新 SessionID 和错误。
	instance, err := c.startSession(newSessionRecord, true)
	if err == nil {
		_, err = instance.HandleInput(ctx, &session.SessionInput{
			Mode: session.SessionInputModeNormal,
			Item: newAgentMessage(source, newSessionRecord.ID, defs.AgentRelationAncestor, param.Message),
		})
	}
	if err != nil {
		return newSessionRecord.ID, fmt.Errorf("Session %s for task %q created; first input not accepted: %w", newSessionRecord.ID, param.TaskName, err)
	}
	return newSessionRecord.ID, nil
}

// getSpawnModelSelection 选择子 Session 的模型和推理强度。
func (c *conversationImpl) getSpawnModelSelection(parent defs.Session, agentName string, param *SpawnAgentParam) (defs.ModelSelection, error) {
	// 1. 读取 Agent 默认模型；参数指定了模型时解析引用，校验它在目录里、推理强度受支持。
	config := c.config.Load()
	agentModel := config.agentDefinitions[agentName].Model
	switch {
	case param.Model != "":
		ref, err := defs.ParseModelRef(param.Model)
		if err != nil {
			return defs.ModelSelection{}, err
		}
		model, found := config.models[ref]
		if !found {
			return defs.ModelSelection{}, fmt.Errorf("model %s is not configured", ref)
		}
		return newModelSelection(model, param.ReasoningEffort, "")
	// 2. 和父 Session 是同一 Agent 时沿用父 Session 的模型和强度，父 Session 没选过模型时用 Agent 默认模型。
	case agentName == parent.AgentName:
		model, reasoningEffort, found := defs.GetSelectedModel(config.models, parent.Model, agentModel)
		if !found {
			return defs.ModelSelection{}, fmt.Errorf("model %s is not configured", cmp.Or(parent.Model.Ref, agentModel))
		}
		return newModelSelection(model, param.ReasoningEffort, reasoningEffort)
	}
	// 3. 不同 Agent 用它的默认模型，校验参数指定的推理强度。
	return newModelSelection(config.models[agentModel], param.ReasoningEffort, "")
}

// notifyParent 向父 Session 提交补充消息。
func (c *conversationImpl) notifyParent(config *conversationConfig, child defs.Session, message *defs.Item) {
	// 1. 取得或启动父 Session。
	parent, _ := c.sessionRecords.Load(child.ParentSessionID)
	instance, err := c.startSession(parent, false)
	// 2. 用子 Session 启动时那套配置的 sessionsCtx，以 steer 提交消息。
	if err == nil {
		_, err = instance.HandleInput(config.sessionsCtx, &session.SessionInput{
			Mode: session.SessionInputModeSteer,
			Item: message,
		})
	}
	// 3. 通知失败且这套配置的 Session 还没停止时记警告；已经停止时直接丢弃消息。
	if err != nil && config.sessionsCtx.Err() == nil {
		telemetry.GetLogger(defs.ContextWithSessionCtxMeta(config.sessionsCtx,
			defs.SessionCtxMeta{SessionID: child.ID, ParentSessionID: child.ParentSessionID})).
			Warn("notify parent session failed", "error", err)
	}
}

// isDescendant target 是否为 source 的后代 Session，且不是 source 自身？
func (c *conversationImpl) isDescendant(source, target defs.SessionID) bool {
	// 1. 沿 target 的父链向上找 source，最多走 Session 总数步，没找到时返回 false。
	record, found := c.sessionRecords.Load(target)
	for steps := c.sessionRecords.Len(); found && record.ParentSessionID != "" && steps > 0; steps-- {
		if record.ParentSessionID == source {
			return true
		}
		record, found = c.sessionRecords.Load(record.ParentSessionID)
	}
	return false
}

// isAllowedTarget target 是否为 source 可以发送消息的后代或直接父 Session？
func (c *conversationImpl) isAllowedTarget(source defs.Session, target defs.SessionID) bool {
	// 1. 检查 target 是否为后代或直接父 Session。
	return c.isDescendant(source.ID, target) || (source.ParentSessionID != "" && source.ParentSessionID == target)
}
