package conversation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// sendMessage 向目标 Session 提交操作者消息或授权范围内的 Agent 消息。
func (c *conversationImpl) sendMessage(ctx context.Context, source defs.Session, param *SendMessageParam) (defs.ItemID, error) {
	// 1. 校验消息内容和目标身份。
	if source.ID != "" && !c.isAllowedTarget(source, param.SessionID) {
		return "", fmt.Errorf("Session %s is outside the collaboration scope of %s: %w", param.SessionID, source.ID, source.WithDetachOriginHint(ErrSendPermissionDenied))
	}
	if strings.TrimSpace(param.Message) == "" {
		return "", errors.New("send requires a message")
	}
	// 2. 先确认目标 Session 能运行，再替换后代的模型选择。
	target, found := c.sessionRecords.Load(param.SessionID)
	if !found {
		return "", fmt.Errorf("Session %s is not in Conversation %s", param.SessionID, c.id)
	}
	toParent := source.ID != "" && source.ParentSessionID == target.ID
	if toParent && param.Mode == session.SessionInputModeInterrupt {
		return "", fmt.Errorf("send can only interrupt a descendant agent: %w", ErrSendPermissionDenied)
	}
	changeModel := param.Model != "" || param.ReasoningEffort != ""
	if changeModel && toParent {
		return "", fmt.Errorf("send_message can only change the model of a descendant agent: %w", ErrSendPermissionDenied)
	}
	instance, err := c.getOrOpenSession(ctx, target, nil)
	if err != nil {
		return "", err
	}
	if changeModel {
		if _, err = c.changeDescendantModel(ctx, target, param); err != nil {
			return "", err
		}
	}
	// 3. 按来源和目标的关系，以指定模式提交消息，目标接收后返回。
	mode := param.Mode
	if mode == session.SessionInputModeInterrupt {
		if _, err := instance.InterruptTurn(ctx); err != nil {
			return "", err
		}
		mode = session.SessionInputModeNormal
	}
	var item *defs.Item
	if source.ID == "" {
		item = &defs.Item{
			ID: defs.NewItemID(), Kind: defs.ItemMessage,
			Message: &defs.Message{
				From:    &defs.Address{Kind: defs.IdentityUser},
				To:      &defs.Address{Kind: defs.IdentityAgent, ID: string(target.ID)},
				Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: param.Message}}},
			},
		}
	} else {
		relation := defs.AgentRelationAncestor
		if toParent {
			relation = defs.AgentRelationChild
		}
		item = newAgentMessage(source, target.ID, relation, param.Message)
	}
	return instance.HandleInput(ctx, &session.SessionInput{Mode: cmp.Or(mode, session.SessionInputModeNormal), Item: item})
}

// changeDescendantModel 替换后代 Session 的模型选择，返回保存后的记录。
func (c *conversationImpl) changeDescendantModel(ctx context.Context, target defs.Session, param *SendMessageParam) (defs.Session, error) {
	// 1. 读取目标的 Agent 定义。
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
func (c *conversationImpl) waitAgent(ctx context.Context, source defs.Session, param *WaitAgentParam) (result WaitAgentResult, err error) {
	// 1. 取得来源 Session 的运行实例，设置等待时限；timeout_seconds 为 0 时等 30 秒。
	instance, found := c.runningSessions.Load(source.ID)
	if !found {
		return WaitAgentResult{}, errors.New("calling Session is not running")
	}
	if param.WaitFor != "" && param.WaitFor != waitReasonSubagentResult {
		return WaitAgentResult{}, fmt.Errorf("invalid wait_for: %q", param.WaitFor)
	}
	timeout := time.Duration(param.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			result, err = WaitAgentResult{Reason: waitReasonTimeout}, nil
		}
	}()
	// 2. 先读调用方的未读补充，再查正在执行的后代；返回空闲前再从调用方核对一次。
	if param.WaitFor == waitReasonSubagentResult {
		items, err := instance.GetUnreadSteer(waitCtx)
		if err != nil {
			return WaitAgentResult{}, err
		}
		if len(items) > 0 {
			return newWaitAgentResult(items), nil
		}
		hasRunningDescendant := false
		for _, target := range c.runningSessions.Values() {
			if !c.isDescendant(source.ID, target.GetID()) {
				continue
			}
			if external, ok := target.(*conversationExternalSession); ok && external.detaching.Load() {
				// 转出完成后才有父回报，旧驱动此时可能已经退出。
				hasRunningDescendant = true
				break
			}
			status, err := target.GetStatus(waitCtx)
			if err != nil {
				return WaitAgentResult{}, err
			}
			if status.ActiveTurnID != "" {
				hasRunningDescendant = true
				break
			}
		}
		if !hasRunningDescendant {
			items, err = instance.GetUnreadSteer(waitCtx)
			if err != nil {
				return WaitAgentResult{}, err
			}
			if len(items) > 0 {
				return newWaitAgentResult(items), nil
			}
			return WaitAgentResult{Reason: waitReasonNoRunningSubagents, Hint: "使用 list_agents 查看后代的最近状态和结果。"}, nil
		}
	}
	// 3. 有运行后代或省略 wait_for 时等待原来的补充输入。
	items, err := instance.WaitSteer(waitCtx)
	if err != nil {
		return WaitAgentResult{}, err
	}
	if len(items) == 0 {
		return WaitAgentResult{Reason: waitReasonNoActiveTurn}, nil
	}
	return newWaitAgentResult(items), nil
}

// interruptAgent 请求取消操作者选中的 Session 或来源的后代 Session 的活动轮。
func (c *conversationImpl) interruptAgent(ctx context.Context, source defs.Session, target defs.SessionID) (defs.TurnID, error) {
	// 1. 确认目标是后代，向运行中的目标提交取消请求；目标没在运行或空闲时返回空 TurnID。
	if source.ID != "" && !c.isDescendant(source.ID, target) {
		return "", fmt.Errorf("Session %s is not a descendant of %s", target, source.ID)
	}
	if _, err := c.getCollaborationSource(ctx, target); err != nil {
		return "", err
	}
	instance, found := c.runningSessions.Load(target)
	if !found {
		return "", nil
	}
	return instance.InterruptTurn(ctx)
}

// ListAgents 按父链、工作位置和活动时间筛选，再读取状态并按条数截断。
// sourceID 为空时是操作者主动筛选整个对话，其目录默认取根会话的位置。
func (c *conversationImpl) ListAgents(ctx context.Context, sourceID defs.SessionID, param *ListAgentsParam) (ListAgentsResult, error) {
	filter := *param
	source, err := c.getCollaborationSource(ctx, cmp.Or(sourceID, c.rootSessionID))
	if err != nil {
		return ListAgentsResult{}, err
	}
	if filter.Cwd == "" {
		filter.Cwd = c.sessionDirectory(source)
	}
	hours := 24
	if filter.SinceHours != nil {
		hours = *filter.SinceHours
	}
	if hours < 0 || filter.Limit < 0 {
		return ListAgentsResult{}, errors.New("invalid session list filter")
	}
	filter.SinceHours = &hours
	filter.Limit = min(cmp.Or(filter.Limit, 20), 100)
	cutoff := time.Now().Unix() - int64(hours)*3600
	var records []defs.Session
	for _, record := range c.sessionRecords.Values() {
		if sourceID != "" && !c.isDescendant(sourceID, record.ID) {
			continue
		}
		if filter.SessionID != "" && record.ID != filter.SessionID || !filter.All && record.Background {
			continue
		}
		if hours != 0 && record.UpdatedAt/1000 < cutoff || filter.Cwd != "*" && c.sessionDirectory(record) != filter.Cwd {
			continue
		}
		if hasSessionLabels(record.Labels, filter.Labels) {
			records = append(records, record)
		}
	}
	infos, err := c.getSessionInfos(ctx, records)
	if err != nil {
		return ListAgentsResult{}, err
	}
	if len(filter.Statuses) > 0 {
		infos = slices.DeleteFunc(infos, func(s SessionInfo) bool { return !slices.Contains(filter.Statuses, s.Status.State) })
	}
	return ListAgentsResult{
		Agents:        infos[:min(filter.Limit, len(infos))],
		TotalMatched:  len(infos),
		Truncated:     len(infos) > filter.Limit,
		AppliedFilter: filter,
		Hint:          "设置 cwd=*、since_hours=0、all=true 可扩大范围；limit 最多为 100。",
	}, nil
}

func (c *conversationImpl) sessionDirectory(record defs.Session) string {
	if record.External != nil {
		return record.External.Cwd
	}
	envs := c.config.Load().environments
	if len(envs) > 0 && envs[0].GetWorkspace() != nil {
		return envs[0].GetWorkspace().GetRootDirectory()
	}
	return ""
}

func (c *conversationImpl) getCollaborationSource(ctx context.Context, id defs.SessionID) (defs.Session, error) {
	record, found := c.sessionRecords.Load(id)
	if !found {
		return defs.Session{}, fmt.Errorf("Session %s is not in Conversation %s", id, c.id)
	}
	return record, c.detachedSessionError(ctx, record)
}

func (c *conversationImpl) SpawnAgent(ctx context.Context, id defs.SessionID, param *SpawnAgentParam) (SpawnAgentResult, error) {
	source, err := c.getCollaborationSource(ctx, id)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	instance, found := c.runningSessions.Load(id)
	if !found {
		return SpawnAgentResult{}, errors.New("calling Session is not running")
	}
	status, err := instance.GetStatus(ctx)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	if status.ActiveTurnID == "" {
		return SpawnAgentResult{}, errors.New("calling Session has no active turn")
	}
	return c.spawnAgent(ctx, source, status.ActiveTurnID, "", param)
}

func (c *conversationImpl) SendMessage(ctx context.Context, id defs.SessionID, param *SendMessageParam) (defs.ItemID, error) {
	if id == "" {
		return c.sendMessage(ctx, defs.Session{}, param)
	}
	source, err := c.getCollaborationSource(ctx, id)
	if err != nil {
		return "", err
	}
	return c.sendMessage(ctx, source, param)
}

func (c *conversationImpl) WaitAgent(ctx context.Context, id defs.SessionID, param *WaitAgentParam) (WaitAgentResult, error) {
	source, err := c.getCollaborationSource(ctx, id)
	if err != nil {
		return WaitAgentResult{}, err
	}
	return c.waitAgent(ctx, source, param)
}

func (c *conversationImpl) StopAgent(ctx context.Context, id, target defs.SessionID) (defs.TurnID, error) {
	if id == "" {
		return c.interruptAgent(ctx, defs.Session{}, target)
	}
	source, err := c.getCollaborationSource(ctx, id)
	if err != nil {
		return "", err
	}
	return c.interruptAgent(ctx, source, target)
}

func (c *conversationImpl) UpdateSession(ctx context.Context, id defs.SessionID, update SessionUpdate) (defs.Session, error) {
	return c.saveSessionUpdate(ctx, id, update, nil, nil)
}

// saveSessionUpdate 将绑定、选模或用户编辑合入同一份记录，保存成功后发布带存储时间戳的副本。
func (c *conversationImpl) saveSessionUpdate(ctx context.Context, id defs.SessionID, update SessionUpdate, model *defs.ModelSelection, binding *defs.ExternalSession) (defs.Session, error) {
	c.sessionRecordWrites.Lock()
	defer c.sessionRecordWrites.Unlock()
	record, err := c.getCollaborationSource(ctx, id)
	if err != nil {
		return defs.Session{}, err
	}
	if update.detachedConversationID != "" {
		record.DetachedConversationID = update.detachedConversationID
	}
	if update.Labels != nil {
		record.Labels = maps.Clone(update.Labels)
	}
	if update.Background != nil {
		record.Background = *update.Background
	}
	if update.TaskName != nil {
		record.TaskName = *update.TaskName
	}
	if model != nil {
		record.Model = *model
	}
	if binding != nil {
		record.External = binding
	}
	if err = c.opt.Mutation.SetSession(ctx, record); err != nil {
		return defs.Session{}, fmt.Errorf("save Session %s: %w", id, err)
	}
	record, _, err = c.opt.Query.GetSession(ctx, c.id, id)
	if err == nil {
		c.sessionRecords.Store(id, record)
	}
	return record, err
}

// getSessionInfos 汇总各 Session 的记录、执行状态、用量和模型窗口。
func (c *conversationImpl) getSessionInfos(ctx context.Context, records []defs.Session) ([]SessionInfo, error) {
	// 1. 按 SessionID 排序，逐个查询状态：运行中的问实例，没在运行的读最近保存的 Turn。
	slices.SortFunc(records, func(a, b defs.Session) int { return strings.Compare(string(a.ID), string(b.ID)) })
	config := c.config.Load()
	infos := make([]SessionInfo, 0, len(records))
	for _, record := range records {
		query := c.opt.Query
		status := session.SessionStatus{State: session.SessionStateStopped}
		var err error
		if instance, ok := c.runningSessions.Load(record.ID); ok {
			status, err = instance.GetStatus(ctx)
			if external, ok := instance.(*conversationExternalSession); ok {
				query, _ = external.GetHistoryDAO()
			}
		} else if record.External != nil {
			query = nil
		} else {
			status, err = c.getStoppedStatus(ctx, record.ID)
		}
		if err != nil {
			return nil, err
		}
		// 2. 外部历史仅在该实例回放完成后可读。
		var usage defs.TokenUsage
		if query != nil {
			usage, err = dao.GetSessionUsage(ctx, query, record.ID)
		}
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

// ReadSessionHistory 分页读取后代 Session 的本地历史。
func (c *conversationImpl) ReadSessionHistory(
	ctx context.Context,
	sourceID defs.SessionID,
	param *SessionHistoryParam,
) (storage.Page[dao.LoggedItem, storage.Ordinal], error) {
	// 1. 确认目标是后代，游标不能指到 fork 记录或更早。
	var result storage.Page[dao.LoggedItem, storage.Ordinal]
	if !c.isDescendant(sourceID, param.SessionID) {
		return result, fmt.Errorf("Session %s is not a descendant of %s", param.SessionID, sourceID)
	}
	query, _, err := c.GetSessionHistoryDAO(ctx, param.SessionID, true)
	if err != nil {
		return result, err
	}
	forkOrdinal, err := c.getForkOrdinal(ctx, query, param.SessionID)
	if err != nil {
		return result, err
	}
	if param.Cursor != 0 && param.Cursor <= forkOrdinal {
		return result, errors.New("history cursor crosses fork boundary")
	}
	// 2. 从新到旧读取一页日志。
	limit := cmp.Or(param.Limit, 20)
	if limit < 1 || limit > 100 {
		return result, errors.New("history limit must be between 1 and 100")
	}
	page, err := query.ListItems(ctx, dao.ListItemsRequest{
		ConversationID: c.id,
		SessionID:      param.SessionID,
		Order:          storage.DirectionDesc,
		Cursor:         param.Cursor,
		Limit:          limit,
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
		logged.Item = transformHistoryItem(logged.Item)
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
func (c *conversationImpl) getForkOrdinal(ctx context.Context, query dao.Query, id defs.SessionID) (storage.Ordinal, error) {
	// 1. 从新到旧分批扫描日志，找到 fork 记录时返回它的序号，扫完仍没找到时返回错误。
	var cursor storage.Ordinal
	for {
		page, err := query.ListItems(ctx, dao.ListItemsRequest{
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
