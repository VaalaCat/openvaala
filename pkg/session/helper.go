package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/turn"
)

// cloneInputItems 复制一组原消息和 skill Item。
func cloneInputItems(items []*defs.Item) []*defs.Item {
	// 1. 按原顺序复制各条 Item。
	cloned := make([]*defs.Item, len(items))
	for index, item := range items {
		cloned[index] = item.Clone()
	}
	return cloned
}

// cloneInputGroups 按组顺序复制补充输入并展开成一个列表。
func cloneInputGroups(groups defs.SessionInputBatch) []*defs.Item {
	// 1. 按组顺序逐条复制 Item。
	var items []*defs.Item
	for _, group := range groups {
		items = append(items, cloneInputItems(group.Items)...)
	}
	return items
}

// newSessionEvent 把 Turn 事件转换为带 TurnID 的 Session 事件。
func newSessionEvent(turnID defs.TurnID, event turn.TurnEvent) SessionEvent {
	// 1. 按 Turn 事件类型换成对应的 Session 事件并填入 TurnID；遇到不认识的类型时 panic。
	switch event := event.(type) {
	case *turn.TurnAgentEvent:
		return &SessionAgentEvent{
			TurnID: turnID,
			Event:  event.Event,
		}
	case *turn.TurnItemsSaved:
		return &SessionItemsSaved{
			TurnID: turnID,
			Items:  event.Items,
		}
	case *turn.TurnHookRunEvent:
		return &SessionHookRunEvent{
			TurnID: turnID,
			Run:    event.Run,
		}
	default:
		panic(fmt.Sprintf("unsupported Turn event %T", event))
	}
}

// countInboxGroups 统计排队的各批输入一共有多少条。
func countInboxGroups(inputs []defs.SessionInputBatch) int {
	// 1. 累加每批里的组数，一组算一条输入。
	count := 0
	for _, input := range inputs {
		count += len(input)
	}
	return count
}

// logSessionStopped 按停止原因记录 Session 日志。
func logSessionStopped(logger *slog.Logger, err error) {
	// 1. 没有错误或因 ctx 取消停止时记 Info，其余错误记 Error。
	if err == nil || errors.Is(err, context.Canceled) {
		logger.Info("session stopped", "reason", err)
		return
	}
	logger.Error("session stopped with error", "error", err)
}

// isUserInputGroup 这一组是否是指定 ID 的用户消息？
func isUserInputGroup(group defs.SessionInputGroup) bool {
	// 1. 检查组首消息的发送方。
	from := group.Items[0].Message.From
	return from != nil && from.Kind == defs.IdentityUser
}

// newPendingInput 从用户消息生成文字预览和图片计数。
func newPendingInput(item *defs.Item, isSteer bool) PendingInput {
	// 1. 按内容顺序连接普通文字并计数图片。
	pendingInput := PendingInput{
		ItemID:  item.ID,
		IsSteer: isSteer,
	}
	var text strings.Builder
	for _, content := range item.Message.Content {
		switch content.Kind {
		case defs.ContentText:
			if !content.Text.IsContext {
				text.WriteString(content.Text.Text)
			}
		case defs.ContentImage:
			pendingInput.ImageCount++
		}
	}
	// 2. 合并预览空白并按 Unicode 码点截取。
	preview := []rune(strings.Join(strings.Fields(text.String()), " "))
	if len(preview) > 200 {
		preview = append(preview[:199], '…')
	}
	pendingInput.Preview = string(preview)
	return pendingInput
}

// cloneSessionCheckpoint 复制待办列表和轮次归属，保留各组不变的原 Item。
func cloneSessionCheckpoint(checkpoint *defs.SessionCheckpoint) *defs.SessionCheckpoint {
	// 1. 复制会在变更待办时修改的切片和轮次记录。
	cloned := *checkpoint
	cloned.PendingUserInputs = slices.Clone(checkpoint.PendingUserInputs)
	cloned.PendingAgentMessages = slices.Clone(checkpoint.PendingAgentMessages)
	if inputs := checkpoint.TurnInputs; inputs != nil {
		clonedInputs := *inputs
		clonedInputs.SteerInputs = slices.Clone(inputs.SteerInputs)
		cloned.TurnInputs = &clonedInputs
	}
	return &cloned
}

// getInputGroupIndex 查找指定组首 Item 的位置，找不到时返回 -1。
func getInputGroupIndex(groups defs.SessionInputBatch, itemID defs.ItemID) int {
	// 1. 比较每组首项的 ID。
	return slices.IndexFunc(groups, func(group defs.SessionInputGroup) bool { return group.Items[0].ID == itemID })
}

// takePendingUserGroup 从待提交记录中取出指定的用户输入组和批次组首变化。
func takePendingUserGroup(checkpoint *defs.SessionCheckpoint, itemID defs.ItemID) (group defs.SessionInputGroup, oldBatchID, newBatchID defs.ItemID) {
	// 1. 在用户队列中找到组，复制并更新所在批次；批次变空时移除。
	for index, batch := range checkpoint.PendingUserInputs {
		groupIndex := getInputGroupIndex(batch, itemID)
		if groupIndex < 0 || !isUserInputGroup(batch[groupIndex]) {
			continue
		}
		group = batch[groupIndex]
		oldBatchID = batch[0].Items[0].ID
		remaining := slices.Delete(slices.Clone(batch), groupIndex, groupIndex+1)
		if len(remaining) == 0 {
			checkpoint.PendingUserInputs = slices.Delete(checkpoint.PendingUserInputs, index, index+1)
		} else {
			checkpoint.PendingUserInputs[index] = remaining
			newBatchID = remaining[0].Items[0].ID
		}
		return group, oldBatchID, newBatchID
	}
	return group, "", ""
}

// getUnhandedTurnInputs 对照原轮日志保留未交接的开场和补充；全未交接时返回原归属。
func getUnhandedTurnInputs(inputs *defs.SessionTurnInputs, rows []dao.LoggedItem) (*defs.SessionTurnInputs, error) {
	// 1. 用原始 Item 对账，完整交接的开场不再重放。
	items := make([]*defs.Item, len(rows))
	for index, row := range rows {
		items[index] = row.Item
	}
	remaining := &defs.SessionTurnInputs{TurnID: inputs.TurnID, OpeningInput: inputs.OpeningInput}
	if opening := inputs.OpeningInput; opening != nil {
		status, err := turn.CheckInputHandoff(inputs.TurnID, *opening, items)
		if err != nil {
			return nil, err
		}
		if status == turn.InputHandoffComplete {
			remaining.OpeningInput = nil
		}
	}
	// 2. 按原顺序保留剩余补充，全部交接时清除归属。
	for _, group := range inputs.SteerInputs {
		status, err := turn.CheckInputHandoff(inputs.TurnID, group, items)
		if err != nil {
			return nil, err
		}
		if status == turn.InputHandoffNone {
			remaining.SteerInputs = append(remaining.SteerInputs, group)
		}
	}
	if remaining.OpeningInput == nil && len(remaining.SteerInputs) == 0 {
		return nil, nil
	}
	if remaining.OpeningInput == inputs.OpeningInput && len(remaining.SteerInputs) == len(inputs.SteerInputs) {
		return inputs, nil
	}
	return remaining, nil
}

// isAgentInputGroup 该输入组是否来自 Agent？
func isAgentInputGroup(group defs.SessionInputGroup) bool {
	// 1. 按输入消息发送方判断。
	from := group.Items[0].Message.From
	return from != nil && from.Kind == defs.IdentityAgent
}

// cloneInputBatch 复制一批输入组及其中的 Item。
func cloneInputBatch(batch defs.SessionInputBatch) defs.SessionInputBatch {
	// 1. 逐组复制 Item，保留原输入 ID。
	cloned := slices.Clone(batch)
	for index := range cloned {
		cloned[index].Items = cloneInputItems(cloned[index].Items)
	}
	return cloned
}
