package conversation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// sendMessage 向后代或直接父 Session 提交消息。
func (c *conversationImpl) sendMessage(ctx context.Context, source defs.Session, param *SendMessageParam) error {
	// 1. 校验消息内容和目标身份。
	if strings.TrimSpace(param.Message) == "" {
		return errors.New("send requires a message")
	}
	if !c.isAllowedTarget(source, param.SessionID) {
		return fmt.Errorf("Session %s is outside the collaboration scope of %s", param.SessionID, source.ID)
	}
	// 2. 参数带模型或强度时先替换后代的模型选择，新选择从下一个 Turn 起使用。
	target, _ := c.sessionRecords.Load(param.SessionID)
	if param.Model != "" || param.ReasoningEffort != "" {
		var err error
		if target, err = c.changeDescendantModel(ctx, source, target, param); err != nil {
			return err
		}
	}
	// 3. 取得或启动目标 Session，按来源和目标的关系，以指定模式提交消息，目标接收后返回。
	instance, err := c.startSession(target, false)
	if err != nil {
		return err
	}
	relation := defs.AgentRelationAncestor
	if source.ParentSessionID == target.ID {
		relation = defs.AgentRelationChild
	}
	_, err = instance.HandleInput(ctx, &session.SessionInput{
		Mode: param.Mode,
		Item: newAgentMessage(source, target.ID, relation, param.Message),
	})
	return err
}

// changeDescendantModel 替换后代 Session 的模型选择，返回保存后的记录。
func (c *conversationImpl) changeDescendantModel(ctx context.Context, source, target defs.Session, param *SendMessageParam) (defs.Session, error) {
	// 1. 确认目标是后代，读取它的 Agent 定义。
	if !c.isDescendant(source.ID, target.ID) {
		return defs.Session{}, errors.New("send_message can only change the model of a descendant agent")
	}
	definition, found := c.config.Load().agentDefinitions[target.AgentName]
	if !found {
		return defs.Session{}, newAgentNotConfiguredError(target)
	}
	// 2. 没给模型时沿用目标当前的模型或 Agent 默认模型，再校验并保存模型和强度。
	ref := cmp.Or(target.Model.Ref, definition.Model)
	if param.Model != "" {
		var err error
		if ref, err = defs.ParseModelRef(param.Model); err != nil {
			return defs.Session{}, err
		}
	}
	return c.SetSessionModel(ctx, target.ID, defs.ModelSelection{
		Ref:             ref,
		ReasoningEffort: param.ReasoningEffort,
	})
}

// waitAgent 等来源 Session 的活动 Turn 收到补充输入，返回唤醒原因和 Agent 发送方。
func (c *conversationImpl) waitAgent(ctx context.Context, source defs.Session, param *WaitAgentParam) (waitAgentResult, error) {
	// 1. 取得来源 Session 的运行实例，设置等待时限；timeout_seconds 为 0 时等 30 秒。
	instance, _ := c.runningSessions.Load(source.ID)
	timeout := time.Duration(param.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 2. 等未读的补充输入；等到时限返回 timeout，调用方取消等其他错误原样返回。
	items, err := instance.WaitSteer(waitCtx)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return waitAgentResult{Reason: waitReasonTimeout}, nil
		}
		return waitAgentResult{}, err
	}
	// 3. 按输入来源生成唤醒结果；Turn 已结束且没有输入时返回空的 agent_message。
	return newWaitAgentResult(items), nil
}

// interruptAgent 请求取消后代的活动 Turn。
func (c *conversationImpl) interruptAgent(ctx context.Context, source defs.Session, target defs.SessionID) (defs.TurnID, error) {
	// 1. 确认目标是后代，向运行中的目标提交取消请求；目标没在运行或空闲时返回空 TurnID。
	if !c.isDescendant(source.ID, target) {
		return "", fmt.Errorf("Session %s is not a descendant of %s", target, source.ID)
	}
	instance, found := c.runningSessions.Load(target)
	if !found {
		return "", nil
	}
	return instance.InterruptTurn(ctx)
}

// listAgents 查询来源 Session 全部后代的信息。
func (c *conversationImpl) listAgents(ctx context.Context, source defs.Session) ([]SessionInfo, error) {
	// 1. 筛出来源 Session 的全部后代，按 SessionID 排序后查询信息。
	descendants := slices.DeleteFunc(c.sessionRecords.Values(), func(record defs.Session) bool {
		return !c.isDescendant(source.ID, record.ID)
	})
	return c.getSessionInfos(ctx, descendants)
}

// getSessionInfos 汇总各 Session 的记录、执行状态、用量和模型窗口。
func (c *conversationImpl) getSessionInfos(ctx context.Context, records []defs.Session) ([]SessionInfo, error) {
	// 1. 按 SessionID 排序，逐个查询状态：运行中的问实例，没在运行的读最近保存的 Turn。
	slices.SortFunc(records, func(a, b defs.Session) int { return strings.Compare(string(a.ID), string(b.ID)) })
	config := c.config.Load()
	infos := make([]SessionInfo, 0, len(records))
	for _, record := range records {
		var status session.SessionStatus
		var err error
		if instance, found := c.runningSessions.Load(record.ID); found {
			status, err = instance.GetStatus(ctx)
		} else {
			status, err = c.getStoppedStatus(ctx, record.ID)
		}
		if err != nil {
			return nil, err
		}
		// 2. 累加已保存 Turn 的用量。
		usage, err := dao.GetSessionUsage(ctx, c.opt.Query, record.ID)
		if err != nil {
			return nil, err
		}
		info := SessionInfo{
			Session: record,
			Status:  status,
			Usage:   usage,
		}
		// 3. 读取所选模型的上下文窗口；Agent 不在目录里时只看 Session 的选择，模型不在目录里时为 0。
		var agentModel defs.ModelRef
		if definition, found := config.agentDefinitions[record.AgentName]; found {
			agentModel = definition.Model
		}
		if model, _, found := defs.GetSelectedModel(config.models, record.Model, agentModel); found {
			info.ContextWindow = model.ContextWindow
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// readSessionHistory 分页读取后代 Session 的本地历史。
func (c *conversationImpl) readSessionHistory(
	ctx context.Context,
	source defs.Session,
	param *SessionHistoryParam,
) (storage.Page[dao.LoggedItem, storage.Ordinal], error) {
	// 1. 确认目标是后代，游标不能指到 fork 记录或更早。
	var result storage.Page[dao.LoggedItem, storage.Ordinal]
	if !c.isDescendant(source.ID, param.SessionID) {
		return result, fmt.Errorf("Session %s is not a descendant of %s", param.SessionID, source.ID)
	}
	forkOrdinal, err := c.getForkOrdinal(ctx, param.SessionID)
	if err != nil {
		return result, err
	}
	if param.Cursor != 0 && param.Cursor <= forkOrdinal {
		return result, errors.New("history cursor crosses fork boundary")
	}
	// 2. 从新到旧读取一页日志。
	page, err := c.opt.Query.ListItems(ctx, dao.ListItemsRequest{
		ConversationID: c.id,
		SessionID:      param.SessionID,
		Order:          storage.DirectionDesc,
		Cursor:         param.Cursor,
		Limit:          param.Limit,
	})
	if err != nil {
		return result, err
	}
	// 3. 去掉 fork 记录及更早的记录，读到 fork 记录时把下一页游标清零，再按时间正序返回。
	result.Next = page.Next
	for _, logged := range page.Items {
		if logged.Ordinal <= forkOrdinal {
			result.Next = 0
			break
		}
		result.Items = append(result.Items, logged)
	}
	if len(result.Items) > 0 && result.Items[len(result.Items)-1].Ordinal == forkOrdinal+1 {
		result.Next = 0
	}
	slices.Reverse(result.Items)
	return result, nil
}

// getStoppedStatus 返回没在运行的 Session 的状态，取最近保存的 Turn。
func (c *conversationImpl) getStoppedStatus(ctx context.Context, id defs.SessionID) (session.SessionStatus, error) {
	// 1. 读取最近保存的 Turn；它的记录还是 running 时只填 TurnID，不填状态。
	status := session.SessionStatus{State: session.SessionStateStopped}
	page, err := c.opt.Query.ListTurns(ctx, dao.ListTurnsRequest{
		SessionID: id,
		Order:     storage.Order{Direction: storage.DirectionDesc},
		Limit:     1,
	})
	if err != nil {
		return status, err
	}
	if len(page.Items) > 0 {
		status.LastTurnID = page.Items[0].ID
		if page.Items[0].Status != defs.TurnStatusRunning {
			status.LastTurnStatus = page.Items[0].Status
		}
	}
	return status, nil
}

// getForkOrdinal 从后代日志中查找 fork 记录的序号。
func (c *conversationImpl) getForkOrdinal(ctx context.Context, id defs.SessionID) (storage.Ordinal, error) {
	// 1. 从新到旧分批扫描日志，找到 fork 记录时返回它的序号，扫完仍没找到时返回错误。
	var cursor storage.Ordinal
	for {
		page, err := c.opt.Query.ListItems(ctx, dao.ListItemsRequest{
			ConversationID: c.id,
			SessionID:      id,
			Order:          storage.DirectionDesc,
			Cursor:         cursor,
			Limit:          256,
		})
		if err != nil {
			return 0, err
		}
		for _, logged := range page.Items {
			if logged.Item.Kind == defs.ItemSessionFork {
				return logged.Ordinal, nil
			}
		}
		if page.Next == 0 {
			return 0, errors.New("descendant Session Log has no fork boundary")
		}
		cursor = page.Next
	}
}
