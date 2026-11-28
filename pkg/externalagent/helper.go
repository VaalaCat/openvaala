package externalagent

import (
	"errors"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
)

// PrepareExternalPrompt 保留交付的输入 ID，按摘要、各条正文、整批附件、会话提示排列内容。
// 摘要和会话提示由宿主在首次发送时提供，驱动按返回的内容顺序编码。
func PrepareExternalPrompt(items []*defs.Item, summary string, sessionInfo *defs.Session) ([]*defs.Item, error) {
	if len(items) == 0 {
		return nil, errors.New("empty external prompt")
	}
	result := defs.CloneItems(items)
	var attachments []*defs.Content
	for i, item := range result {
		if item == nil {
			return nil, errors.New("external prompt requires a message")
		}
		if item.Message == nil {
			var text string
			switch {
			case item.ClientInstruction != nil:
				text = prompt.FormatClientInstruction(*item.ClientInstruction)
			case item.AgentsInstruction != nil:
				text = prompt.FormatAgentsInstruction(*item.AgentsInstruction)
			case item.SkillInstruction != nil:
				text = prompt.FormatSkillInstruction(*item.SkillInstruction)
			case item.MemoryIndex != nil:
				text = prompt.FormatMemoryIndex(*item.MemoryIndex)
			case item.HookRun != nil:
				text = prompt.FormatHookRun(*item.HookRun)
			default:
				return nil, errors.New("external prompt requires a message or context instruction")
			}
			message := &defs.Message{From: &defs.Address{Kind: defs.IdentityUser}}
			if text != "" {
				message.Content = []*defs.Content{textBlock(text)}
			}
			item = &defs.Item{ID: item.ID, Kind: defs.ItemMessage, Message: message}
			result[i] = item
		}
		m := item.Message
		var text []string
		for _, c := range m.Content {
			if err := c.Validate(); err != nil {
				return nil, err
			}
			if c.Kind == defs.ContentText {
				text = append(text, c.Text.Text)
			} else {
				attachments = append(attachments, c)
			}
		}
		if m.SubagentNotification != nil || m.AgentSender != nil {
			if m.From == nil {
				return nil, errors.New("missing agent sender")
			}
			sender := defs.SessionID(m.From.ID)
			if m.SubagentNotification != nil {
				text = []string{prompt.FormatSubagentNotification(sender, *m.SubagentNotification, text)}
			} else {
				text = []string{prompt.FormatAgentMessage(m)}
			}
		}
		m.AgentSender, m.SubagentNotification, m.Content = nil, nil, nil
		if i == 0 && summary != "" {
			m.Content = append(m.Content, textBlock(summary))
		}
		for _, body := range text {
			m.Content = append(m.Content, textBlock(body))
		}
	}
	last := result[len(result)-1].Message
	last.Content = append(last.Content, attachments...)
	if sessionInfo != nil {
		last.Content = append(last.Content, textBlock(prompt.FormatSessionInfo(*sessionInfo)))
	}
	return result, nil
}

func textBlock(text string) *defs.Content {
	return &defs.Content{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}
}

// isUserMessage 是否是后端回放或人提交的用户消息？
func isUserMessage(item *defs.Item) bool {
	return item != nil && item.Message != nil && item.Message.From != nil && item.Message.From.Kind == defs.IdentityUser
}

// hasAssistantText 该输出是否有能回报给父会话的助手正文？
func hasAssistantText(item *defs.Item) bool {
	if item.Kind != defs.ItemMessage || item.Message == nil || isUserMessage(item) {
		return false
	}
	for _, content := range item.Message.Content {
		if content != nil && content.Text != nil && strings.TrimSpace(content.Text.Text) != "" {
			return true
		}
	}
	return false
}

// groupHasTool 该输出组是否已出现工具调用？
func groupHasTool(drafts []draftItem, id defs.ModelCallID) bool {
	for i := len(drafts) - 1; i >= 0; i-- {
		item := drafts[i].item
		if item == nil {
			continue
		}
		if item.ModelCallID == id && item.ToolCall != nil {
			return true
		}
	}
	return false
}

func draftIndex(drafts []draftItem, e *agent.AgentEvent) int {
	if item := e.Item; item != nil && item.ToolCall != nil && item.ToolCall.CallID != "" {
		return slices.IndexFunc(drafts, func(d draftItem) bool {
			return d.item != nil && d.item.ToolCall != nil && d.item.ToolCall.CallID == item.ToolCall.CallID
		})
	}
	return slices.IndexFunc(drafts, func(d draftItem) bool { return d.index == e.ItemIndex })
}

// applyDraftDelta 复用原生增量拼装；先检查协议草稿可接收此增量。
func applyDraftDelta(item *defs.Item, delta *agent.AgentItemDelta) error {
	if delta == nil {
		return errors.New("external stream delta is missing")
	}
	switch item.Kind {
	case defs.ItemMessage:
		if item.Message == nil || len(item.Message.Content) == 0 || item.Message.Content[0] == nil || item.Message.Content[0].Text == nil {
			return errors.New("external text delta requires a text block")
		}
	case defs.ItemReasoning:
		if item.Reasoning == nil {
			return errors.New("external reasoning delta requires a reasoning item")
		}
	case defs.ItemToolCall:
		if item.ToolCall == nil {
			return errors.New("external tool delta requires a tool call")
		}
	}
	agent.ApplyItemDelta(item, delta)
	return nil
}
