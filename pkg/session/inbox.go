package session

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// SessionInbox 保存待处理输入、轮次归属和借出记录，由所属 Session 的事件循环串行调用。
type SessionInbox struct {
	opt SessionInboxOpt
	// checkpoint 是最近一次成功写入的待办。
	checkpoint defs.SessionCheckpoint
	// acceptedInputs 保存本次运行期已接收的 InputID 和对应的 ItemID。
	acceptedInputs map[defs.InputID]defs.ItemID
	// borrowedSteerIDs 标记已借出但尚未确认的补充组。
	borrowedSteerIDs map[defs.ItemID]struct{}
	// queuedBatchSpans 按各批次首项保存排队输入的追踪信息。
	queuedBatchSpans map[defs.ItemID]telemetry.SpanRef
}

// NewSessionInbox 保存会话身份和 DAO。
func NewSessionInbox(opt *SessionInboxOpt) (*SessionInbox, error) {
	// 1. 校验依赖并初始化内存索引。
	if opt == nil || opt.SessionID == "" || opt.Query == nil || opt.Mutation == nil {
		return nil, errors.New("session inbox requires a Session ID and DAO")
	}
	return &SessionInbox{
		opt:              *opt,
		acceptedInputs:   make(map[defs.InputID]defs.ItemID),
		borrowedSteerIDs: make(map[defs.ItemID]struct{}),
		queuedBatchSpans: make(map[defs.ItemID]telemetry.SpanRef),
	}, nil
}

// Load 读取 Session 待办并恢复运行期队列和 InputID 去重。
func (b *SessionInbox) Load(ctx context.Context) error {
	// 1. 读取最新待办；没有记录时从空 checkpoint 开始。
	checkpoint, err := b.opt.Query.GetSessionCheckpoint(ctx, b.opt.ConversationID, b.opt.SessionID)
	if err != nil {
		return fmt.Errorf("load Session checkpoint: %w", err)
	}
	if checkpoint == nil {
		return nil
	}
	b.checkpoint = *checkpoint
	for _, batches := range [][]defs.SessionInputBatch{checkpoint.PendingUserInputs, checkpoint.PendingAgentMessages} {
		for _, batch := range batches {
			for _, group := range batch {
				if group.InputID != "" && len(group.Items) > 0 {
					b.acceptedInputs[group.InputID] = group.Items[0].ID
				}
			}
		}
	}
	if inputs := checkpoint.TurnInputs; inputs != nil {
		if inputs.OpeningInput != nil && inputs.OpeningInput.InputID != "" {
			b.acceptedInputs[inputs.OpeningInput.InputID] = inputs.OpeningInput.Items[0].ID
		}
		for _, group := range inputs.SteerInputs {
			if group.InputID != "" {
				b.acceptedInputs[group.InputID] = group.Items[0].ID
			}
		}
	}
	return nil
}

// Add 校验并按发送方和模式接收输入，活动轮可以直接收下补充。
func (b *SessionInbox) Add(ctx context.Context, input *SessionInput, turnID defs.TurnID) (defs.ItemID, error) {
	// 1. 校验输入，重复提交返回原 ItemID。
	if err := validateSessionInput(input); err != nil {
		return "", err
	}
	if itemID, found := b.acceptedInputs[input.InputID]; found {
		return itemID, nil
	}
	// 2. 保存完整输入组。
	group := defs.SessionInputGroup{InputID: input.InputID, Items: []*defs.Item{input.Item.Clone()}}
	if err := b.addInput(ctx, group, input.Mode, turnID, telemetry.GetSpanRef(ctx)); err != nil {
		return "", err
	}
	return input.Item.ID, nil
}

// Withdraw 从普通队列或未借出的补充中撤回完整用户输入组。
func (b *SessionInbox) Withdraw(ctx context.Context, itemID defs.ItemID) ([]*defs.Item, error) {
	// 1. 移除目标输入组，保存后更新批次追踪。
	next := cloneSessionCheckpoint(&b.checkpoint)
	group, oldBatchID, newBatchID := takePendingUserGroup(next, itemID)
	if inputs := next.TurnInputs; group.Items == nil && inputs != nil {
		index := getInputGroupIndex(inputs.SteerInputs, itemID)
		if _, borrowed := b.borrowedSteerIDs[itemID]; !borrowed && index >= 0 && isUserInputGroup(inputs.SteerInputs[index]) {
			group = inputs.SteerInputs[index]
			inputs.SteerInputs = slices.Delete(inputs.SteerInputs, index, index+1)
			if inputs.OpeningInput == nil && len(inputs.SteerInputs) == 0 {
				next.TurnInputs = nil
			}
		}
	}
	if group.Items == nil {
		return nil, errors.New("input is no longer pending")
	}
	// 2. 持久移除后更新批次追踪并交还整组 Item。
	if err := b.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		return nil, fmt.Errorf("save withdrawn input: %w", err)
	}
	b.moveQueuedBatchSpan(oldBatchID, newBatchID)
	return group.Items, nil
}

// TakeTurnInput 分配下一轮输入，返回原生 Turn 的开场组及该批次的追踪信息。
func (b *SessionInbox) TakeTurnInput(ctx context.Context, turnID defs.TurnID) (defs.SessionInputBatch, telemetry.SpanRef, error) {
	// 1. 保存原生 Turn 的开场和补充归属，返回首组。
	batch, parentSpan, err := b.takeTurnBatch(ctx, turnID, false)
	if len(batch) > 0 {
		batch = cloneInputBatch(batch[:1])
	}
	return batch, parentSpan, err
}

// TakeWholeBatch 把所有可开轮的输入借给指定 Turn，返回输入副本。
func (b *SessionInbox) TakeWholeBatch(ctx context.Context, turnID defs.TurnID) (defs.SessionInputBatch, error) {
	// 1. 保存该轮的批次归属并登记借出，交给外部 Turn 一次发送。
	batch, _, err := b.takeTurnBatch(ctx, turnID, true)
	return cloneInputBatch(batch), err
}

// TakeSteer 借出该轮尚未借出的补充输入。
func (b *SessionInbox) TakeSteer(turnID defs.TurnID) [][]*defs.Item {
	// 1. 交出该轮尚未借出的输入组。
	if inputs := b.checkpoint.TurnInputs; inputs == nil || inputs.TurnID != turnID {
		return nil
	}
	var items [][]*defs.Item
	for _, group := range b.getUnreadSteerInputs() {
		items = append(items, cloneInputItems(group.Items))
		b.borrowedSteerIDs[group.Items[0].ID] = struct{}{}
	}
	return items
}

// GetUnreadSteer 返回已接收且未借出的补充和排队 Agent 消息副本。
func (b *SessionInbox) GetUnreadSteer() []*defs.Item {
	// 1. 先读活动轮的补充，再读等待下一轮的 Agent 消息。
	items := cloneInputGroups(b.getUnreadSteerInputs())
	for _, batch := range b.checkpoint.PendingAgentMessages {
		items = append(items, cloneInputGroups(batch)...)
	}
	return items
}

// ConfirmSteer 清除已交接的开场和已借出的补充组。
func (b *SessionInbox) ConfirmSteer(ctx context.Context, turnID defs.TurnID, itemIDs []defs.ItemID) error {
	// 1. 在原轮待办中匹配已交接的开场组及已借出的补充组。
	if turnID == "" || b.checkpoint.TurnInputs == nil || b.checkpoint.TurnInputs.TurnID != turnID || len(itemIDs) == 0 {
		return errors.New("turn input handoff is no longer pending")
	}
	next := cloneSessionCheckpoint(&b.checkpoint)
	inputs := next.TurnInputs
	for _, itemID := range itemIDs {
		if inputs.OpeningInput != nil && inputs.OpeningInput.Items[0].ID == itemID {
			inputs.OpeningInput = nil
			continue
		}
		index := getInputGroupIndex(inputs.SteerInputs, itemID)
		if _, borrowed := b.borrowedSteerIDs[itemID]; !borrowed || index < 0 {
			return fmt.Errorf("input %q is not borrowed by Turn %q", itemID, turnID)
		}
		inputs.SteerInputs = slices.Delete(inputs.SteerInputs, index, index+1)
	}
	// 2. 写成待办后清除已确认的借出标记。
	if inputs.OpeningInput == nil && len(inputs.SteerInputs) == 0 {
		next.TurnInputs = nil
	}
	if err := b.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		return fmt.Errorf("confirm input handoff: %w", err)
	}
	for _, itemID := range itemIDs {
		delete(b.borrowedSteerIDs, itemID)
	}
	return nil
}

// ReturnUnconfirmed 在轮终态写成后将未交接的补充放回队首。
func (b *SessionInbox) ReturnUnconfirmed(ctx context.Context, turnID defs.TurnID) error {
	if inputs := b.checkpoint.TurnInputs; inputs != nil && inputs.TurnID == turnID || b.checkpoint.StopRequestedTurnID == turnID {
		return b.requeueTurnInputs(ctx, cloneSessionCheckpoint(&b.checkpoint), turnID)
	}
	return nil
}

// PruneCheckpoints 只在没有待办且未暂停时清理 checkpoint。
func (b *SessionInbox) PruneCheckpoints(ctx context.Context) error {
	cp := &b.checkpoint
	if cp.IsAgentMessagesPaused || cp.TurnInputs != nil || cp.StopRequestedTurnID != "" || len(cp.PendingUserInputs) != 0 || len(cp.PendingAgentMessages) != 0 {
		return nil
	}
	return b.opt.Mutation.DeleteSessionCheckpoints(ctx, b.opt.ConversationID, b.opt.SessionID)
}

// ClearFailedTurnInputs 在失败轮及其输入已写入日志后清除该轮的借出记录。
func (b *SessionInbox) ClearFailedTurnInputs(ctx context.Context, turnID defs.TurnID) error {
	// 1. 开场和已借出的补充已经写入失败轮。在等待驱动确认期间新到的补充仍须回队。
	next := cloneSessionCheckpoint(&b.checkpoint)
	inputs := next.TurnInputs
	if inputs == nil || inputs.TurnID != turnID {
		return nil
	}
	inputs.OpeningInput = nil
	inputs.SteerInputs = slices.DeleteFunc(inputs.SteerInputs, func(g defs.SessionInputGroup) bool {
		_, borrowed := b.borrowedSteerIDs[g.Items[0].ID]
		return borrowed
	})
	return b.requeueTurnInputs(context.WithoutCancel(ctx), next, turnID)
}

// addInput 将已校验的输入组写入活动轮或待开轮队列。
func (b *SessionInbox) addInput(ctx context.Context, group defs.SessionInputGroup, mode SessionInputMode, turnID defs.TurnID, parentSpan telemetry.SpanRef) error {
	// 1. 按发送方和模式写入下一份待办。
	next := cloneSessionCheckpoint(&b.checkpoint)
	item := group.Items[0]
	isAgent := isAgentInputGroup(group)
	isSteer := mode == SessionInputModeSteer && turnID != ""
	if isUserInputGroup(group) {
		next.IsAgentMessagesPaused = false
	}
	switch {
	case isSteer:
		if next.TurnInputs == nil {
			next.TurnInputs = &defs.SessionTurnInputs{TurnID: turnID}
		}
		next.TurnInputs.SteerInputs = append(next.TurnInputs.SteerInputs, group)
	case !isAgent && mode == SessionInputModeInterrupt:
		next.PendingUserInputs = append([]defs.SessionInputBatch{{group}}, next.PendingUserInputs...)
		next.StopRequestedTurnID = turnID
	case !isAgent:
		next.PendingUserInputs = append(next.PendingUserInputs, defs.SessionInputBatch{group})
	default:
		next.PendingAgentMessages = append(next.PendingAgentMessages, defs.SessionInputBatch{group})
	}
	if err := b.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		return fmt.Errorf("save input checkpoint: %w", err)
	}
	// 2. 登记去重和批次追踪。
	if group.InputID != "" {
		b.acceptedInputs[group.InputID] = item.ID
	}
	if !isSteer {
		b.queuedBatchSpans[item.ID] = parentSpan
	}
	return nil
}

// takeTurnBatch 为原生轮取首批用户输入或全部 Agent 消息，为外部轮取所有可开轮的输入。
func (b *SessionInbox) takeTurnBatch(ctx context.Context, turnID defs.TurnID, whole bool) (defs.SessionInputBatch, telemetry.SpanRef, error) {
	// 1. 选出可以分给下一轮的输入批次。
	if b.checkpoint.TurnInputs != nil {
		return nil, telemetry.SpanRef{}, nil
	}
	next := cloneSessionCheckpoint(&b.checkpoint)
	batches := next.PendingUserInputs
	if !whole && len(batches) > 0 {
		batches = batches[:1]
		next.PendingUserInputs = next.PendingUserInputs[1:]
	} else {
		next.PendingUserInputs = nil
	}
	if (whole || len(batches) == 0) && !next.IsAgentMessagesPaused {
		batches = append(batches, next.PendingAgentMessages...)
		next.PendingAgentMessages = nil
	}
	if len(batches) == 0 {
		return nil, telemetry.SpanRef{}, nil
	}
	// 2. 一次保存整轮归属，写成后更新借出和批次追踪。
	groups := slices.Concat(batches...)
	next.TurnInputs = &defs.SessionTurnInputs{TurnID: turnID, OpeningInput: &groups[0], SteerInputs: groups[1:]}
	if err := b.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		return nil, telemetry.SpanRef{}, fmt.Errorf("allocate Turn %q inputs: %w", turnID, err)
	}
	if whole {
		for _, group := range groups[1:] {
			b.borrowedSteerIDs[group.Items[0].ID] = struct{}{}
		}
	}
	parentSpan := b.queuedBatchSpans[groups[0].Items[0].ID]
	for _, batch := range batches {
		delete(b.queuedBatchSpans, batch[0].Items[0].ID)
	}
	return groups, parentSpan, nil
}

// SaveStopRequest 保存活动轮的停止请求及根会话的 Agent 消息暂停状态。
func (b *SessionInbox) SaveStopRequest(ctx context.Context, turnID defs.TurnID, pauseAgentMessages bool) error {
	// 1. 将停止请求和暂停状态一并写入待办。
	next := cloneSessionCheckpoint(&b.checkpoint)
	next.StopRequestedTurnID = turnID
	if pauseAgentMessages {
		next.IsAgentMessagesPaused = true
	}
	return b.appendSessionCheckpoint(context.WithoutCancel(ctx), next)
}

// appendSessionCheckpoint 保存下一份待办并在成功后替换内存记录。
func (b *SessionInbox) appendSessionCheckpoint(ctx context.Context, checkpoint *defs.SessionCheckpoint) error {
	// 1. 先追加完整待办，写成功后更新 Session 的 checkpoint。
	if err := b.opt.Mutation.AppendSessionCheckpoint(ctx, b.opt.ConversationID, b.opt.SessionID, checkpoint); err != nil {
		return err
	}
	b.checkpoint = *checkpoint
	return nil
}

// getUnreadSteerInputs 取尚未借出的活动轮补充组。
func (b *SessionInbox) getUnreadSteerInputs() defs.SessionInputBatch {
	// 1. 从已分配待办中过滤借出的组。
	inputs := b.checkpoint.TurnInputs
	if inputs == nil {
		return nil
	}
	var groups defs.SessionInputBatch
	for _, group := range inputs.SteerInputs {
		if _, borrowed := b.borrowedSteerIDs[group.Items[0].ID]; !borrowed {
			groups = append(groups, group)
		}
	}
	return groups
}

// requeueTurnInputs 将已终结轮的剩余补充按发送方各合一批回队，清理原轮的归属和停止请求。
func (b *SessionInbox) requeueTurnInputs(ctx context.Context, next *defs.SessionCheckpoint, turnID defs.TurnID) error {
	// 1. 只移动该轮尚未交接的补充，不移动开场。
	if inputs := next.TurnInputs; inputs != nil && inputs.TurnID == turnID {
		prependPendingGroups(next, inputs.SteerInputs)
		next.TurnInputs = nil
	}
	// 2. 一行写成新的归属及停止请求，保留根 Session 暂停状态。
	if next.StopRequestedTurnID == turnID {
		next.StopRequestedTurnID = ""
	}
	if err := b.appendSessionCheckpoint(ctx, next); err != nil {
		return fmt.Errorf("clear completed Turn inputs: %w", err)
	}
	clear(b.borrowedSteerIDs)
	return nil
}

// prependPendingGroups 把未交付的用户输入和 Agent 回报分别按原序放回各自队首。
func prependPendingGroups(next *defs.SessionCheckpoint, groups defs.SessionInputBatch) {
	var users, agents defs.SessionInputBatch
	for _, group := range groups {
		if isAgentInputGroup(group) {
			agents = append(agents, group)
		} else {
			users = append(users, group)
		}
	}
	if len(users) > 0 {
		next.PendingUserInputs = append([]defs.SessionInputBatch{users}, next.PendingUserInputs...)
	}
	if len(agents) > 0 {
		next.PendingAgentMessages = append([]defs.SessionInputBatch{agents}, next.PendingAgentMessages...)
	}
}

// SteerInput 将排队用户输入移到指定轮的补充尾部。
func (b *SessionInbox) SteerInput(ctx context.Context, itemID defs.ItemID, turnID defs.TurnID) error {
	// 1. 从普通队列移动目标组，保存后更新批次追踪。
	next := cloneSessionCheckpoint(&b.checkpoint)
	group, oldBatchID, newBatchID := takePendingUserGroup(next, itemID)
	if group.Items == nil {
		return errors.New("input is no longer pending")
	}
	if next.TurnInputs == nil {
		next.TurnInputs = &defs.SessionTurnInputs{TurnID: turnID}
	}
	next.TurnInputs.SteerInputs = append(next.TurnInputs.SteerInputs, group)
	// 2. 记录写成后更新批次追踪信息。
	if err := b.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		return fmt.Errorf("save steered input: %w", err)
	}
	b.moveQueuedBatchSpan(oldBatchID, newBatchID)
	return nil
}

// InterruptTurnWithInput 把选中输入与未借出补充同批置顶，并保存停止请求。
func (b *SessionInbox) InterruptTurnWithInput(ctx context.Context, turnID defs.TurnID, itemID defs.ItemID) error {
	// 1. 将目标输入和未借出的补充移到队首。
	next := cloneSessionCheckpoint(&b.checkpoint)
	group, oldBatchID, newBatchID := takePendingUserGroup(next, itemID)
	if group.Items == nil {
		return errors.New("input is no longer pending")
	}
	batch := defs.SessionInputBatch{group}
	if inputs := next.TurnInputs; inputs != nil {
		var remaining defs.SessionInputBatch
		for _, steer := range inputs.SteerInputs {
			if _, found := b.borrowedSteerIDs[steer.Items[0].ID]; found {
				remaining = append(remaining, steer)
			} else {
				batch = append(batch, steer)
			}
		}
		inputs.SteerInputs = remaining
		if inputs.OpeningInput == nil && len(remaining) == 0 {
			next.TurnInputs = nil
		}
	}
	next.PendingUserInputs = append([]defs.SessionInputBatch{batch}, next.PendingUserInputs...)
	next.StopRequestedTurnID = turnID
	// 2. 保存迁移和停止请求，更新批次追踪。
	if err := b.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		return fmt.Errorf("save interrupted input: %w", err)
	}
	parentSpan := b.queuedBatchSpans[oldBatchID]
	b.moveQueuedBatchSpan(oldBatchID, newBatchID)
	b.queuedBatchSpans[itemID] = parentSpan
	return nil
}

// moveQueuedBatchSpan 将排队批次的追踪信息转到新组首，批次已空时清除。
func (b *SessionInbox) moveQueuedBatchSpan(oldBatchID, newBatchID defs.ItemID) {
	// 1. 只更新组首有变化的批次。
	if oldBatchID == newBatchID {
		return
	}
	if span, found := b.queuedBatchSpans[oldBatchID]; found {
		delete(b.queuedBatchSpans, oldBatchID)
		if newBatchID != "" {
			b.queuedBatchSpans[newBatchID] = span
		}
	}
}

// reconcileTurnInputs 对照原轮日志，保留尚未交接的输入并恢复已结束轮的待办。
func (b *SessionInbox) reconcileTurnInputs(ctx context.Context, latest defs.Turn, rows []dao.LoggedItem) error {
	// 1. 去掉已完整写入原轮的输入组。
	next := cloneSessionCheckpoint(&b.checkpoint)
	inputs := next.TurnInputs
	if inputs != nil && inputs.TurnID == latest.ID {
		remaining, err := getUnhandedTurnInputs(inputs, rows)
		if err != nil {
			return err
		}
		next.TurnInputs = remaining
	}
	// 2. 已结束轮的剩余输入回队，仍在运行的轮保留剩余归属。
	if latest.Status != defs.TurnStatusRunning {
		if inputs != nil && inputs.TurnID == latest.ID || next.StopRequestedTurnID == latest.ID {
			return b.requeueTurnInputs(ctx, next, latest.ID)
		}
		return nil
	}
	if next.TurnInputs != inputs {
		return b.appendSessionCheckpoint(ctx, next)
	}
	return nil
}

// GetPendingUsers 返回前端可操作的未读补充和排队用户输入。
func (b *SessionInbox) GetPendingUsers() []PendingInput {
	// 1. 先列未借出的用户补充，再列按出队顺序排列的用户输入。
	inputs := make([]PendingInput, 0)
	for _, group := range b.getUnreadSteerInputs() {
		if isUserInputGroup(group) {
			inputs = append(inputs, newPendingInput(group.Items[0], true))
		}
	}
	for _, batch := range b.checkpoint.PendingUserInputs {
		for _, group := range batch {
			if isUserInputGroup(group) {
				inputs = append(inputs, newPendingInput(group.Items[0], false))
			}
		}
	}
	return inputs
}

// GetPendingInputCount 返回等候开轮的用户和 Agent 输入数。
func (b *SessionInbox) GetPendingInputCount() int {
	return countInboxGroups(b.checkpoint.PendingUserInputs) + countInboxGroups(b.checkpoint.PendingAgentMessages)
}

// GetAcceptedInputID 返回本次运行中已接收的输入，供 Session 避免重复触发中断。
func (b *SessionInbox) GetAcceptedInputID(inputID defs.InputID) (defs.ItemID, bool) {
	id, found := b.acceptedInputs[inputID]
	return id, found
}
