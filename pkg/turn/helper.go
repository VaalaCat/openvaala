package turn

import (
	"strings"

	"github.com/VaalaCat/openvaala/pkg/defs"
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
