package turn

import (
	"errors"
	"fmt"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
)

// isUserInput 这组输入是否来自用户？
func isUserInput(group []*defs.Item) bool {
	// 1. 检查首条输入的消息发送方是否为用户；Agent 消息、子任务首条输入和结果通知都不算。
	message := group[0].Message
	return message != nil && message.From != nil && message.From.Kind == defs.IdentityUser
}

// getPromptText 提取用户输入 hook 使用的正文。
func getPromptText(group []*defs.Item) string {
	// 1. 按顺序收集消息中的非上下文文本块，再用换行连接；跳过图片、附带材料和 skill 说明。
	var texts []string
	for _, item := range group {
		if item.Kind != defs.ItemMessage || item.Message == nil {
			continue
		}
		for _, content := range item.Message.Content {
			if content.Kind == defs.ContentText && content.Text != nil && !content.Text.IsContext {
				texts = append(texts, content.Text.Text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// newHookRunItems 将 Turn 触发的 hook 记录转换为 Item。
func newHookRunItems(runs []*defs.HookRun, turnID defs.TurnID) []*defs.Item {
	// 1. 按运行记录顺序创建 Item，填写所属 Turn 和运行记录，不填写模型调用 ID。
	items := make([]*defs.Item, 0, len(runs))
	for _, run := range runs {
		items = append(items, &defs.Item{
			ID:      defs.NewItemID(),
			TurnID:  turnID,
			Kind:    defs.ItemHookRun,
			HookRun: run,
		})
	}
	return items
}

// markInputHandoff 在输入组末项或拦截 hook 记录上写入交接事实。
func markInputHandoff(group, hookItems []*defs.Item, outcome *hook.HookOutcome, inputItemID defs.ItemID) {
	// 1. 被拦截时同时写组末项的交接事实和 hook 的拦截事实，否则只写组末项的交接事实。
	if outcome != nil && outcome.IsBlocked {
		for index := len(hookItems) - 1; index >= 0; index-- {
			if hookItems[index] == nil || hookItems[index].HookRun == nil {
				continue
			}
			hookItems[index].HandedOffInputItemID = inputItemID
			hookItems[index].HookRun.BlockedInputItemID = inputItemID
			return
		}
	}
	if len(hookItems) > 0 {
		hookItems[len(hookItems)-1].HandedOffInputItemID = inputItemID
		return
	}
	if len(group) > 0 {
		group[len(group)-1].HandedOffInputItemID = inputItemID
	}
}

// InputHandoffStatus 表示一组输入在 Item 日志中的交接记录状态。
type InputHandoffStatus string

const (
	// InputHandoffNone 表示这组输入在指定 Turn 中还没有任何记录。
	InputHandoffNone InputHandoffStatus = "none"
	// InputHandoffComplete 表示正文、Skill 或拦截事实及末项交接标记均已写成。
	InputHandoffComplete InputHandoffStatus = "complete"
	// InputHandoffPartial 表示只写成了部分输入，或缺少末项交接标记。
	InputHandoffPartial InputHandoffStatus = "partial"
)

// ErrInputHandoffIncomplete 表示输入组记录不完整，调用方不得补写尾项或再次执行 hook。
var ErrInputHandoffIncomplete = errors.New("input record incomplete")

// CheckInputHandoff 按 Turn、组首 ID、组内顺序和实际末项判断输入交接状态。
func CheckInputHandoff(turnID defs.TurnID, group defs.SessionInputGroup, loggedItems []*defs.Item) (InputHandoffStatus, error) {
	// 1. 收集同一 Turn 中这组成员和末项交接标记的位置；同一条 Item 同时可以是组成员和末项。
	if turnID == "" || len(group.Items) == 0 || group.Items[0] == nil || group.Items[0].ID == "" {
		return InputHandoffPartial, fmt.Errorf("%w: invalid Turn or input group", ErrInputHandoffIncomplete)
	}
	memberIDs := make(map[defs.ItemID]int, len(group.Items))
	for index, item := range group.Items {
		if item == nil || item.ID == "" {
			return InputHandoffPartial, fmt.Errorf("%w: invalid input group", ErrInputHandoffIncomplete)
		}
		memberIDs[item.ID] = index
	}
	memberPositions := make([]int, len(group.Items))
	for index := range memberPositions {
		memberPositions[index] = -1
	}
	memberSeen := make([]int, len(group.Items))
	markerCount := 0
	markerPosition := -1
	blockedMarker := false
	bodyCount := 0
	for position, item := range loggedItems {
		if item == nil || item.TurnID != turnID {
			continue
		}
		if index, isMember := memberIDs[item.ID]; isMember {
			bodyCount++
			memberSeen[index]++
			if memberPositions[index] == -1 {
				memberPositions[index] = position
			}
		}
		if item.HandedOffInputItemID == group.Items[0].ID {
			markerCount++
			markerPosition = position
			blockedMarker = item.HookRun != nil && item.HookRun.BlockedInputItemID == group.Items[0].ID
		}
	}
	// 2. 没有正文成员和末项事实时是全无；其余缺失、乱序、重复或提前/重复标记均为 partial。
	if bodyCount == 0 && markerCount == 0 {
		return InputHandoffNone, nil
	}
	if markerCount == 0 || markerCount > 1 {
		return inputHandoffIncomplete(turnID, group.Items[0].ID)
	}
	if blockedMarker {
		if bodyCount != 0 {
			return inputHandoffIncomplete(turnID, group.Items[0].ID)
		}
		return InputHandoffComplete, nil
	}
	for index, count := range memberSeen {
		if count != 1 {
			return inputHandoffIncomplete(turnID, group.Items[0].ID)
		}
		if index > 0 && memberPositions[index-1] >= memberPositions[index] {
			return inputHandoffIncomplete(turnID, group.Items[0].ID)
		}
	}
	lastMemberPosition := memberPositions[len(memberPositions)-1]
	if markerPosition != lastMemberPosition && markerPosition <= lastMemberPosition {
		return inputHandoffIncomplete(turnID, group.Items[0].ID)
	}
	return InputHandoffComplete, nil
}

// inputHandoffIncomplete 创建带 TurnID 和组首 ID 的不完整交接错误。
func inputHandoffIncomplete(turnID defs.TurnID, inputItemID defs.ItemID) (InputHandoffStatus, error) {
	// 1. 返回 partial 状态及可定位的错误。
	return InputHandoffPartial, fmt.Errorf("%w: Turn %q input %q", ErrInputHandoffIncomplete, turnID, inputItemID)
}
