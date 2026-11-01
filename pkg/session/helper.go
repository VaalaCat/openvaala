package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/turn"
)

// cloneItems 按组顺序复制补充输入，摊平成一个列表。
func cloneItems(groups [][]*defs.Item) []*defs.Item {
	// 1. 按组顺序逐条复制 Item。
	var result []*defs.Item
	for _, group := range groups {
		for _, item := range group {
			result = append(result, item.Clone())
		}
	}
	return result
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
func countInboxGroups(inputs []inboxInput) int {
	// 1. 累加每批里的组数，一组算一条输入。
	count := 0
	for _, input := range inputs {
		count += len(input.groups)
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
func isUserInputGroup(group []*defs.Item, itemID defs.ItemID) bool {
	// 1. 检查组首消息 ID 与发送方。
	item := group[0]
	return item.ID == itemID && item.Message.From != nil && item.Message.From.Kind == defs.IdentityUser
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
