package agent

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// newItemEvent 创建带输出位置的 Item 事件。
func (a *agentImpl) newItemEvent(eventType AgentEventType, item *defs.Item, delta *AgentItemDelta) *AgentEvent {
	// 1. 填写草稿位置和增量，增量以外的事件另附 Item 副本。
	event := &AgentEvent{
		Type:      eventType,
		ItemIndex: a.draftItemIndexes[item],
		Delta:     delta,
	}
	if eventType != AgentEventItemDelta {
		event.Item = item.Clone()
	}
	return event
}

// hasMessageText 消息 Item 是否已经有非空白文字？
func hasMessageText(item *defs.Item) bool {
	// 1. 检查消息的文本块是否包含非空白文字。
	for _, content := range item.Message.Content {
		if content.Text != nil && strings.TrimSpace(content.Text.Text) != "" {
			return true
		}
	}
	return false
}

// collectCompletedDraftItems 按草稿顺序收集正常完成的 Item，并单独返回可执行的工具调用。
func (a *agentImpl) collectCompletedDraftItems(items []*defs.Item) (completedItems, toolCalls []*defs.Item) {
	// 1. 按草稿顺序收集正常完成的 Item，并单独收集其中的工具调用。
	for _, item := range items {
		if a.draftItemFinishReason[item] != AgentEventItemCompleted {
			continue
		}
		completedItems = append(completedItems, item)
		if item.Kind == defs.ItemToolCall {
			toolCalls = append(toolCalls, item)
		}
	}
	return completedItems, toolCalls
}

// getLastMessageText 拼出最后一条消息的全部文本。
func getLastMessageText(items []*defs.Item) string {
	// 1. 从末尾查找消息，拼接它的全部文本块；没有消息时返回空串。
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].Kind != defs.ItemMessage {
			continue
		}
		var text strings.Builder
		for _, content := range items[index].Message.Content {
			if content.Text != nil {
				text.WriteString(content.Text.Text)
			}
		}
		return text.String()
	}
	return ""
}

// newHookRunItems 把 hook 运行记录依次转换为指定 Turn 和模型调用的 hook_run Item。
func newHookRunItems(runs []*defs.HookRun, turnID defs.TurnID, modelCallID defs.ModelCallID) []*defs.Item {
	// 1. 按原顺序为每条运行记录创建 Item，填写所属 Turn 和模型调用 ID。
	items := make([]*defs.Item, 0, len(runs))
	for _, run := range runs {
		items = append(items, &defs.Item{
			ID:          defs.NewItemID(),
			TurnID:      turnID,
			ModelCallID: modelCallID,
			Kind:        defs.ItemHookRun,
			HookRun:     run,
		})
	}
	return items
}

// Clone 复制 Agent 定义、列表和 ACP 启动命令。
func (d *AgentDefinition) Clone() *AgentDefinition {
	// 1. 复制角色字段及工具名、skill 名和 MCP server id 列表，保留 nil 和空列表的区别。
	cloned := *d
	cloned.ToolNames = slices.Clone(d.ToolNames)
	cloned.SkillNames = slices.Clone(d.SkillNames)
	cloned.MCPServerIDs = slices.Clone(d.MCPServerIDs)
	cloned.ACP = d.ACP.Clone()
	return &cloned
}

// ValidateExecutionType 校验 Go 定义中已设置的执行配置字段。
func (d *AgentDefinition) ValidateExecutionType() error {
	// 1. 收集已设置字段，按执行类型校验。
	var fields []string
	if d.Model != (defs.ModelRef{}) {
		fields = append(fields, "model")
	}
	if d.CompactModel != (defs.ModelRef{}) {
		fields = append(fields, "compact_model")
	}
	if d.Instructions != "" {
		fields = append(fields, "instructions")
	}
	if d.MaxSteps != 0 {
		fields = append(fields, "max_steps")
	}
	return ValidateAgentExecutionType(d.Type, d.ACP, fields...)
}

// ValidateAgentExecutionType 校验执行类型、ACP 命令和已设置的配置字段。
func ValidateAgentExecutionType(agentType defs.AgentType, acp *defs.ACPCommand, fields ...string) error {
	// 1. 校验执行类型对应的字段组合。
	switch agentType {
	case "", defs.AgentTypeNative:
		if acp != nil || slices.Contains(fields, "acp") {
			return errors.New("native agent must not configure acp")
		}
	case defs.AgentTypeACP:
		if acp == nil || strings.TrimSpace(acp.Command) == "" {
			return errors.New("acp.command must not be empty")
		}
		for field, params := range map[string]map[string]map[string]any{
			"param_overwrite":           acp.ParamOverwrite,
			"ephemeral_param_overwrite": acp.EphemeralParamOverwrite,
		} {
			for method := range params {
				switch method {
				case "initialize", "session/new", "session/load", "session/list", "session/prompt", "session/cancel", "session/set_mode", "session/set_config_option":
				default:
					return fmt.Errorf("unknown ACP method %s in %s", method, field)
				}
			}
		}
		for _, field := range []string{"model", "compact_model", "instructions", "max_steps"} {
			if slices.Contains(fields, field) {
				return fmt.Errorf("acp agent must not configure %s", field)
			}
		}
	default:
		return fmt.Errorf("unsupported agent type %q", agentType)
	}
	return nil
}

// ApplyItemDelta 将消息、推理或工具参数的增量追加到 Item。
func ApplyItemDelta(item *defs.Item, delta *AgentItemDelta) {
	// 1. 按 Item 类别追加：消息文本接到第一个内容块，推理片段接到摘要和内容末尾，参数接到工具参数末尾。
	switch item.Kind {
	case defs.ItemMessage:
		item.Message.Content[0].Text.Text += delta.Text
	case defs.ItemReasoning:
		item.Reasoning.Summary = append(item.Reasoning.Summary, delta.ReasoningSummary...)
		item.Reasoning.Content = append(item.Reasoning.Content, delta.ReasoningContent...)
	case defs.ItemToolCall:
		item.ToolCall.Arguments += delta.Arguments
	}
}

// getItemIDs 按 Item 顺序返回字符串形式的 ID。
func getItemIDs(items []*defs.Item) []string {
	// 1. 依次提取并转换 Item ID。
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, string(item.ID))
	}
	return ids
}

// truncateMemoryIndex 把读到的 MEMORY.md 截到 maxMemoryIndexBytes 以内：超出时在最后一个换行符处截断，
// 没有换行符时退到最后一个完整的 UTF-8 字符；isEndOfFile 表示 content 是否读到了文件末尾；返回正文和是否截断过。
func truncateMemoryIndex(content []byte, isEndOfFile bool) (string, bool) {
	// 1. 不超过上限时原样返回。
	if len(content) <= maxMemoryIndexBytes {
		return string(content), false
	}
	// 2. 上限处正好是行尾：就在上限处截断，不丢最后一整行；后面只剩这个换行符时，文件没有被截断。
	if content[maxMemoryIndexBytes] == '\n' {
		return string(content[:maxMemoryIndexBytes]), !isEndOfFile
	}
	// 3. 优先在上限以内的最后一个换行符处截断，保证以完整的一行结尾。
	if cut := bytes.LastIndexByte(content[:maxMemoryIndexBytes], '\n'); cut >= 0 {
		return string(content[:cut]), true
	}
	// 4. 没有换行符时，从上限处往回退到一个字符的开头，不把多字节字符截成两半。
	cut := maxMemoryIndexBytes
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return string(content[:cut]), true
}

// GetIconKey 按 Agent 名称匹配品牌图标，有外部启动命令时也匹配可执行文件名。
func (d *AgentDefinition) GetIconKey() string {
	names := []string{strings.ToLower(d.Name)}
	if d.Type.IsExternal() && d.ACP != nil && d.ACP.Command != "" {
		command := path.Base(strings.ReplaceAll(d.ACP.Command, `\`, "/"))
		names = append(names, strings.ToLower(command))
	}
	aliases := []struct{ name, icon string }{
		{"github-copilot", "githubcopilot"}, {"githubcopilot", "githubcopilot"},
		{"claude-code", "claudecode"}, {"claudecode", "claudecode"},
		{"gemini-cli", "geminicli"}, {"geminicli", "geminicli"},
		{"antigravity", "antigravity"}, {"agy", "antigravity"},
		{"opencode", "opencode"}, {"windsurf", "windsurf"},
		{"cursor", "cursor"}, {"codex", "codex"}, {"claude", "claude"},
		{"gemini", "gemini"}, {"openai", "openai"}, {"goose", "goose"},
		{"cline", "cline"}, {"kiro", "kiro"}, {"trae", "trae"},
		{"qwen", "qwen"}, {"kimi", "kimi"}, {"amp", "amp"},
	}
	for _, value := range names {
		for _, alias := range aliases {
			if value == alias.name {
				return alias.icon
			}
		}
	}
	for _, value := range names {
		words := " " + strings.Join(strings.FieldsFunc(value, isAgentIconSeparator), " ") + " "
		for _, alias := range aliases {
			if strings.Contains(words, " "+strings.ReplaceAll(alias.name, "-", " ")+" ") {
				return alias.icon
			}
		}
	}
	return ""
}

func isAgentIconSeparator(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}
