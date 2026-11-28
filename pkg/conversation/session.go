package conversation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// spawnAgent 创建直接子 Session 并提交首个任务。
func (c *conversationImpl) spawnAgent(ctx context.Context, source defs.Session, turnID defs.TurnID, callID string, param *SpawnAgentParam) (SpawnAgentResult, error) {
	// 1. 校验任务内容和 Agent，按父 Session 内存记录里最近一次选择的模型选出子 Session 的模型，解析继承轮数。
	if strings.TrimSpace(param.TaskName) == "" || strings.TrimSpace(param.Message) == "" {
		return SpawnAgentResult{}, errors.New("spawn requires task_name and message")
	}
	agentName := param.AgentName
	if agentName == "" {
		agentName = source.AgentName
	}
	config := c.config.Load()
	definition, found := config.agentDefinitions[agentName]
	if !found {
		return SpawnAgentResult{}, newSpawnAgentNotConfiguredError(agentName, config.agentDefinitions)
	}
	if definition.Type.IsExternal() && param.Model != "" {
		ref, err := defs.ParseModelRef(param.Model)
		if err != nil {
			return SpawnAgentResult{}, err
		}
		if ref.ProviderID != agentName {
			return SpawnAgentResult{}, fmt.Errorf("model %s belongs to a different agent than %s", ref, agentName)
		}
		spawn := *param
		spawn.InitialOptions = slices.DeleteFunc(slices.Clone(param.InitialOptions), func(option defs.ExternalOptionSelection) bool {
			return option.Kind == "model"
		})
		if ref.ModelID != "default" {
			spawn.InitialOptions = append(spawn.InitialOptions, defs.ExternalOptionSelection{Kind: "model", Value: ref.ModelID})
		}
		param = &spawn
	}
	parent, _ := c.sessionRecords.Load(source.ID)
	selection, err := getSpawnModelSelection(config.models, parent, definition, param)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	limit, err := forkTurnLimit(param.ForkTurns)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	// 2. 创建子 Session 记录，写入父 Session、Turn 和工具调用信息。
	newSessionRecord := defs.Session{
		ID:                   defs.NewSessionID(),
		ConversationID:       c.id,
		AgentName:            agentName,
		TaskName:             param.TaskName,
		Labels:               param.Labels,
		Background:           param.Background,
		SuppressFinishReport: param.NotifyOnFinish != nil && !*param.NotifyOnFinish,
		Model:                selection,
		ParentSessionID:      source.ID,
		ParentTurnID:         turnID,
		ParentCallID:         callID,
	}

	// 3. 按 fork_turns 读取父 Session 已结束 Turn 的历史，连同 fork 记录一次写入，再保存子 Session 记录。
	var batch []*defs.Item
	message := newAgentMessage(source, newSessionRecord.ID, defs.AgentRelationAncestor, param.Message)
	if limit >= 0 {
		query, _, err := c.GetSessionHistoryDAO(ctx, source.ID, true)
		if err != nil {
			return SpawnAgentResult{}, err
		}
		history, err := dao.LoadContextHistory(ctx, query, c.id, source.ID, turnID)
		if err != nil {
			return SpawnAgentResult{}, fmt.Errorf("load context history of Session %s: %w", source.ID, err)
		}
		batch = selectForkHistory(history, limit)
		if source.External != nil || definition.Type.IsExternal() {
			if summary := renderChatHistorySummary(batch); summary != "" {
				message.Message.Content = append([]*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: summary, IsContext: true}}}, message.Message.Content...)
			}
			batch = nil
		}
	}
	if definition.Type.IsExternal() {
		executorID, cwd, _, err := resolveExternalSessionWorkspace(config, parent.External, param)
		if err != nil {
			return SpawnAgentResult{}, err
		}
		newSessionRecord.External = &defs.ExternalSession{AgentType: definition.Type, ExecutorID: executorID, Cwd: cwd}
	} else {
		batch = append(batch, &defs.Item{
			ID:          defs.NewItemID(),
			Kind:        defs.ItemSessionFork,
			SessionFork: &defs.SessionFork{SourceSessionID: source.ID},
		})
		if _, err := c.opt.Mutation.AppendItems(ctx, c.id, newSessionRecord.ID, batch...); err != nil {
			return SpawnAgentResult{}, fmt.Errorf("write inherited history and fork for Session %s: %w", newSessionRecord.ID, err)
		}
	}
	if err := c.opt.Mutation.SetSession(ctx, newSessionRecord); err != nil {
		return SpawnAgentResult{}, fmt.Errorf("save Session %s: %w", newSessionRecord.ID, err)
	}
	newSessionRecord, _, err = c.opt.Query.GetSession(ctx, c.id, newSessionRecord.ID)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	c.sessionRecords.Store(newSessionRecord.ID, newSessionRecord)

	// 4. 启动子 Session，以 normal 提交首个任务；记录已保存，启动或提交失败时仍返回新 SessionID 和错误。
	result := SpawnAgentResult{SessionID: newSessionRecord.ID, TaskName: param.TaskName}
	instance, err := c.getOrOpenSession(ctx, newSessionRecord, param)
	if external, ok := instance.(*conversationExternalSession); ok && err == nil {
		err = external.waitForHistory(ctx)
	}
	if err == nil {
		result.InputItemID, err = instance.HandleInput(ctx, &session.SessionInput{
			Mode: session.SessionInputModeNormal,
			Item: message,
		})
	}
	if err != nil {
		return result, fmt.Errorf("Session %s for task %q created; first input not accepted: %w", newSessionRecord.ID, param.TaskName, err)
	}
	result.Status = "accepted"
	return result, nil
}

// getSpawnModelSelection 选择子 Session 的模型和推理强度。
func getSpawnModelSelection(models map[defs.ModelRef]*defs.ModelDefinition, parent defs.Session, definition *agent.AgentDefinition, param *SpawnAgentParam) (defs.ModelSelection, error) {
	// 1. 外部 Agent 没有本机模型选择。
	if definition.Type.IsExternal() {
		return defs.ModelSelection{}, nil
	}
	// 2. 取指定模型、同一 Agent 的父会话选择或子 Agent 的默认模型。
	ref := definition.Model
	var carriedEffort string
	switch {
	case param.Model != "":
		requestedRef, err := defs.ParseModelRef(param.Model)
		if err != nil {
			return defs.ModelSelection{}, err
		}
		ref = requestedRef
	case definition.Name == parent.AgentName:
		ref = cmp.Or(parent.Model.Ref, ref)
		carriedEffort = parent.Model.ReasoningEffort
	}
	// 3. 校验原生模型和推理强度。
	model, err := getAgentModel(models, definition, ref)
	if err != nil {
		return defs.ModelSelection{}, err
	}
	return newModelSelection(model, param.ReasoningEffort, carriedEffort)
}

// notifyParent 向父 Session 提交补充消息。
func (c *conversationImpl) notifyParent(config *conversationConfig, child defs.Session, message *defs.Item) {
	if child.SuppressFinishReport {
		return
	}
	// 1. 取得或启动父 Session。
	parent, _ := c.sessionRecords.Load(child.ParentSessionID)
	instance, err := c.startSession(parent, nil)
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
	// 1. 用当前对话持有的记录检查父链。
	record, found := c.sessionRecords.Load(target)
	if !found {
		return false
	}
	result, _ := IsSessionDescendant(source, record, func(id defs.SessionID) (defs.Session, bool, error) {
		record, found := c.sessionRecords.Load(id)
		return record, found, nil
	})
	return result
}

// IsSessionDescendant 目标的父链是否包含来源 Session？
func IsSessionDescendant(source defs.SessionID, target defs.Session, lookup func(defs.SessionID) (defs.Session, bool, error)) (bool, error) {
	// 1. 沿父链查找来源；缺失记录、到根或遇到重复时结束。
	if source == "" || source == target.ID {
		return false, nil
	}
	visited := map[defs.SessionID]struct{}{target.ID: {}}
	for id := target.ParentSessionID; id != ""; {
		if id == source {
			return true, nil
		}
		if _, found := visited[id]; found {
			return false, nil
		}
		visited[id] = struct{}{}
		record, found, err := lookup(id)
		if err != nil || !found {
			return false, err
		}
		id = record.ParentSessionID
	}
	return false, nil
}

// isAllowedTarget target 是否为 source 可以发送消息的后代或直接父 Session？
func (c *conversationImpl) isAllowedTarget(source defs.Session, target defs.SessionID) bool {
	// 1. 检查 target 是否为后代或直接父 Session。
	return c.isDescendant(source.ID, target) || (source.ParentSessionID != "" && source.ParentSessionID == target)
}
