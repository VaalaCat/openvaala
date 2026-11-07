package defs

import (
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
)

// ItemKind 区分消息、模型输出和历史记录的种类。
type ItemKind string

const (
	ItemMessage           ItemKind = "message"
	ItemReasoning         ItemKind = "reasoning"
	ItemToolCall          ItemKind = "tool_call"
	ItemToolResult        ItemKind = "tool_result"
	ItemModelError        ItemKind = "model_error"
	ItemContextSummary    ItemKind = "context_summary"
	ItemSessionFork       ItemKind = "session_fork"
	ItemModelChange       ItemKind = "model_change"
	ItemExecutorsChange   ItemKind = "executors_change"
	ItemSkillInstruction  ItemKind = "skill_instruction"
	ItemAgentsInstruction ItemKind = "agents_instruction"
	ItemMemoryIndex       ItemKind = "memory_index"
	ItemClientInstruction ItemKind = "client_instruction"
	ItemHookRun           ItemKind = "hook_run"
	// ItemTurnInterrupted 由 Turn 在以 interrupted 结束时写在本轮最后；没有载荷，TurnID 说明是哪一轮。
	ItemTurnInterrupted ItemKind = "turn_interrupted"
)

// Item 保存一条消息、模型输出或历史记录，Kind 指定使用的载荷。
type Item struct {
	ID ItemID `json:"id"`
	// UpdatedAt 是保存时的 Unix 毫秒，由存储分配；Item 保存后不再修改。
	UpdatedAt         int64              `json:"updated_at,omitempty"`
	TurnID            TurnID             `json:"turn_id"`
	ModelCallID       ModelCallID        `json:"model_call_id,omitempty"`
	Kind              ItemKind           `json:"kind"`
	Message           *Message           `json:"message,omitempty"`
	Reasoning         *Reasoning         `json:"reasoning,omitempty"`
	ToolCall          *ToolCall          `json:"tool_call,omitempty"`
	ToolResult        *ToolResult        `json:"tool_result,omitempty"`
	ModelError        *ModelError        `json:"model_error,omitempty"`
	ContextSummary    *ContextSummary    `json:"context_summary,omitempty"`
	SessionFork       *SessionFork       `json:"session_fork,omitempty"`
	ModelChange       *ModelChange       `json:"model_change,omitempty"`
	ExecutorsChange   *ExecutorsChange   `json:"executors_change,omitempty"`
	SkillInstruction  *SkillInstruction  `json:"skill_instruction,omitempty"`
	AgentsInstruction *AgentsInstruction `json:"agents_instruction,omitempty"`
	MemoryIndex       *MemoryIndex       `json:"memory_index,omitempty"`
	ClientInstruction *ClientInstruction `json:"client_instruction,omitempty"`
	HookRun           *HookRun           `json:"hook_run,omitempty"`
}

// SkillInstruction 是用户在一条输入里点名的一个 Skill：名字、所在 Executor、文件路径和正文。
type SkillInstruction struct {
	// InputItemID 是点名这个 Skill 的用户输入 Item。
	InputItemID ItemID `json:"input_item_id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Executor    string `json:"executor"`
	Text        string `json:"text"`
}

// AgentsInstruction 是某个 Executor 上一份 AGENTS.md 的路径、正文和适用范围。
type AgentsInstruction struct {
	Executor string `json:"executor"`
	Path     string `json:"path"`
	Text     string `json:"text"`
	// IsGlobal 是否为适用于本 Executor 全部工作区的用户级规则？
	IsGlobal bool `json:"is_global,omitempty"`
}

// MemoryIndex 是 Turn 开始时从 memory Executor 读进上下文的一份 MEMORY.md。
type MemoryIndex struct {
	// Path 是 memory Executor 里的路径：user/MEMORY.md 或 project/MEMORY.md。
	Path string `json:"path"`
	// Project 是项目记忆所属的项目：项目记忆目录相对记忆根目录的路径，例如 projects/local/-tmp-x；用户级记忆为空。
	Project string `json:"project,omitempty"`
	// Text 最多 8 KiB，超出时在最后一个完整行处截断。
	Text string `json:"text"`
	// IsTruncated 是否截断过？
	IsTruncated bool `json:"is_truncated,omitempty"`
}

// ClientInstruction 是客户端写进上下文的一段指令，例如旁支的「之前的历史只作参考，别接着做」。
type ClientInstruction struct {
	Text string `json:"text"`
}

// ContextSummary 是压缩上下文时生成的累积历史摘要。
type ContextSummary struct {
	Text string `json:"text"`
	// CoveredThroughItemID 是摘要覆盖到的最后一条原始 Item，包含它本身。
	CoveredThroughItemID ItemID `json:"covered_through_item_id"`
}

// ValidateKindPayload 检查 Skill、AGENTS.md、记忆目录和客户端指令的种类与载荷对得上，以及客户端指令有正文；不检查 TurnID。
func (item *Item) ValidateKindPayload() error {
	// 1. 这四种指令的种类和载荷必须同时存在。
	if (item.Kind == ItemSkillInstruction) != (item.SkillInstruction != nil) ||
		(item.Kind == ItemAgentsInstruction) != (item.AgentsInstruction != nil) ||
		(item.Kind == ItemMemoryIndex) != (item.MemoryIndex != nil) ||
		(item.Kind == ItemClientInstruction) != (item.ClientInstruction != nil) {
		return fmt.Errorf("instruction kind and payload do not match")
	}
	// 2. 客户端指令的正文不能为空。
	if item.ClientInstruction != nil && item.ClientInstruction.Text == "" {
		return fmt.Errorf("client instruction requires text")
	}
	return nil
}

// ValidateContextItem 检查客户端注入的 Item：没有 TurnID 和 ModelCallID 时也能写入日志、转成模型消息。
func (item *Item) ValidateContextItem() error {
	// 1. 先检查指令类 Item 的种类与载荷。
	if err := item.ValidateKindPayload(); err != nil {
		return err
	}
	// 2. 按种类检查载荷；需要模型调用或只由运行时写入的种类、未知种类一律拒绝。
	switch item.Kind {
	case ItemMessage:
		if err := item.ValidateInputMessage(); err != nil {
			return err
		}
		from := item.Message.From
		if from == nil || (from.Kind != IdentityUser && from.Kind != IdentityAgent) {
			return fmt.Errorf("context message requires a user or agent sender")
		}
	case ItemHookRun:
		if item.HookRun == nil {
			return fmt.Errorf("context hook run requires a payload")
		}
	case ItemSkillInstruction, ItemAgentsInstruction, ItemMemoryIndex, ItemClientInstruction:
		// 载荷已由 ValidateKindPayload 检查。
	case ItemReasoning, ItemToolCall, ItemToolResult, ItemModelError:
		return fmt.Errorf("context item kind %q requires a model call", item.Kind)
	case ItemContextSummary, ItemSessionFork, ItemModelChange, ItemExecutorsChange, ItemTurnInterrupted:
		return fmt.Errorf("context item kind %q is written by the runtime", item.Kind)
	default:
		return fmt.Errorf("unknown context item kind %q", item.Kind)
	}
	return nil
}

// ValidateContextSummary 检查摘要 Item 的类型、ID 和载荷。
func (item *Item) ValidateContextSummary() error {
	// 1. 非摘要 Item 不得带摘要载荷。
	if item.Kind != ItemContextSummary {
		if item.ContextSummary != nil {
			return fmt.Errorf("item kind %q cannot carry a context summary", item.Kind)
		}
		return nil
	}
	// 2. 摘要要有 Item、Turn、模型调用 ID，正文不能为空白，还要有覆盖到的最后一条 Item 的 ID。
	if item.ID == "" || item.TurnID == "" || item.ModelCallID == "" {
		return fmt.Errorf("context summary requires item, turn and model call IDs")
	}
	if item.ContextSummary == nil || strings.TrimSpace(item.ContextSummary.Text) == "" || item.ContextSummary.CoveredThroughItemID == "" {
		return fmt.Errorf("context summary requires text and a covered item ID")
	}
	return nil
}

// ValidateInputMessage 检查提交给 Session 的输入消息。
func (item *Item) ValidateInputMessage() error {
	// 1. 要求消息类型和载荷存在；只有子 Agent 通知允许正文为空。
	if item.Kind != ItemMessage || item.Message == nil {
		return fmt.Errorf("input item must be a message")
	}
	message := item.Message
	if len(message.Content) == 0 && message.SubagentNotification == nil {
		return fmt.Errorf("input message requires content")
	}
	// 2. 逐项检查内容类型和对应载荷。
	for index, content := range message.Content {
		if err := content.Validate(); err != nil {
			return fmt.Errorf("input content %d: %w", index, err)
		}
	}
	return nil
}

// ModelError 保存模型返回的错误码和消息。
type ModelError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ToLLMKit 按指定角色将 Item 转换为单条模型消息。
func (item *Item) ToLLMKit(role llmkit.Role) (*llmkit.Message, error) {
	// 1. 检查 Item 存在。
	if item == nil {
		return nil, fmt.Errorf("item is missing")
	}
	// 2. 按消息、推理、工具调用或工具结果转换，并检查对应角色及载荷；其他种类返回错误。
	switch item.Kind {
	case ItemMessage:
		if item.Message == nil {
			return nil, fmt.Errorf("message item has no message")
		}
		return item.Message.ToLLMKit(role)
	case ItemReasoning:
		if item.Reasoning == nil || role != llmkit.RoleAssistant {
			return nil, fmt.Errorf("reasoning item requires content and assistant role")
		}
		reasoning := item.Reasoning.ToLLMKit()
		return &llmkit.Message{
			Role: role,
			Content: []llmkit.ContentBlock{
				{
					Type:      llmkit.ContentTypeThinking,
					Reasoning: reasoning,
				},
			},
		}, nil
	case ItemToolCall:
		if item.ToolCall == nil || role != llmkit.RoleAssistant || item.ToolCall.CallID == "" || item.ToolCall.Name == "" {
			return nil, fmt.Errorf("tool call item requires a call ID, name and assistant role")
		}
		return &llmkit.Message{
			Role:      role,
			ToolCalls: []llmkit.ToolCall{*item.ToolCall.ToLLMKit()},
		}, nil
	case ItemToolResult:
		if item.ToolResult == nil || role != llmkit.RoleTool {
			return nil, fmt.Errorf("tool result item requires content and tool role")
		}
		return item.ToolResult.ToLLMKit()
	default:
		return nil, fmt.Errorf("item kind %q cannot be represented as a model message", item.Kind)
	}
}

// FromLLMKit 将单条模型消息转换为 Item。
func (item *Item) FromLLMKit(modelMessage *llmkit.Message, from, to *Address) error {
	// 1. 检查消息存在，拒绝原始 JSON。
	if modelMessage == nil {
		return fmt.Errorf("model message is missing")
	}
	if len(modelMessage.RawJSON) != 0 {
		return fmt.Errorf("raw messages cannot be decoded as an item")
	}
	// 2. 保留 Item、Turn 和模型调用 ID，分别读取工具结果、完整调用、已完成推理或普通消息。
	decoded := &Item{
		ID:          item.ID,
		TurnID:      item.TurnID,
		ModelCallID: item.ModelCallID,
	}
	switch {
	case modelMessage.Role == llmkit.RoleTool || modelMessage.ToolCallID != "":
		result := &ToolResult{}
		if err := result.FromLLMKit(modelMessage); err != nil {
			return err
		}
		decoded.Kind, decoded.ToolResult = ItemToolResult, result
	case len(modelMessage.ToolCalls) != 0:
		if modelMessage.Role != llmkit.RoleAssistant || len(modelMessage.ToolCalls) != 1 || len(modelMessage.Content) != 0 {
			return fmt.Errorf("tool call item requires one assistant tool call without other content")
		}
		call, err := toolCallFromLLMKit(&modelMessage.ToolCalls[0])
		if err != nil {
			return err
		}
		decoded.Kind, decoded.ToolCall = ItemToolCall, call
	case len(modelMessage.Content) == 1 && modelMessage.Content[0].Type == llmkit.ContentTypeThinking:
		block := &modelMessage.Content[0]
		if modelMessage.Role != llmkit.RoleAssistant || block.Reasoning == nil || len(block.RawJSON) != 0 {
			return fmt.Errorf("thinking block requires structured reasoning and assistant role")
		}
		reasoning := &Reasoning{}
		reasoning.FromLLMKit(block.Reasoning, llmkit.ReasoningCompleted)
		decoded.Kind, decoded.Reasoning = ItemReasoning, reasoning
	default:
		message := &Message{}
		if err := message.FromLLMKit(modelMessage, from, to); err != nil {
			return err
		}
		decoded.Kind, decoded.Message = ItemMessage, message
	}
	// 3. 全部转换成功后替换 Item，失败时保留原值。
	*item = *decoded
	return nil
}

// ToLLMKit 把模型错误转换为 llmkit 的错误载荷。
func (modelError *ModelError) ToLLMKit() *llmkit.ErrorPayload {
	// 1. 复制错误码和错误消息。
	return &llmkit.ErrorPayload{
		Code:    modelError.Code,
		Message: modelError.Message,
	}
}

// FromLLMKit 使用模型流的错误替换错误记录。
func (modelError *ModelError) FromLLMKit(payload *llmkit.ErrorPayload) {
	// 1. 复制错误码和错误消息。
	*modelError = ModelError{
		Code:    payload.Code,
		Message: payload.Message,
	}
}

// SessionFork 是日志里的一条历史分界，按 SourceConversationID 分两种：
// 为空时是 spawn_agent 写在复制来的历史末尾的分界，后面的 Item 是子 Agent 自己的；
// 不为空时是用户分叉写在新根 Session 日志第 1 行的接续标记，读历史时前面接上来源对话根 Session 的内容。
type SessionFork struct {
	// SourceSessionID 是历史来源的 Session。
	SourceSessionID SessionID `json:"source_session_id"`
	// SourceConversationID 是用户分叉的来源对话；spawn_agent 写的分界为空。
	SourceConversationID ConversationID `json:"source_conversation_id,omitempty"`
	// SourceEndOrdinal 是来源日志里第一条复制过来的 Item 的序号（来源对外的序号）；接上的部分是 [1, SourceEndOrdinal)。
	SourceEndOrdinal int64 `json:"source_end_ordinal,omitempty"`
}

// ValidateSessionFork 检查分叉标记的 ID 和载荷。
func (item *Item) ValidateSessionFork() error {
	// 1. 非分叉 Item 不得带分叉载荷。
	if item.Kind != ItemSessionFork {
		if item.SessionFork != nil {
			return fmt.Errorf("item kind %q cannot carry a session fork", item.Kind)
		}
		return nil
	}
	// 2. 分叉标记要求 Item ID 和来源 Session，不得带 Turn 或模型调用 ID。
	if item.ID == "" || item.TurnID != "" || item.ModelCallID != "" || item.SessionFork == nil || item.SessionFork.SourceSessionID == "" {
		return fmt.Errorf("session fork requires a source and no execution identity")
	}
	// 3. 用户分叉的来源对话和结束序号要同时填写，结束序号大于 1。
	fork := item.SessionFork
	if (fork.SourceConversationID != "" || fork.SourceEndOrdinal != 0) && (fork.SourceConversationID == "" || fork.SourceEndOrdinal <= 1) {
		return fmt.Errorf("session fork continuation requires a source conversation and an end ordinal above 1")
	}
	return nil
}

// ModelChange 由 Turn 在本轮模型和上一轮不同时写在本轮输入之前：作为提示发给模型，它之前的推理不再发送。
type ModelChange struct {
	PreviousModel ModelRef `json:"previous_model"`
	Model         ModelRef `json:"model"`
}

// ValidateModelChange 检查模型切换 Item 的 ID 和前后模型。
func (item *Item) ValidateModelChange() error {
	// 1. 非模型切换 Item 不得带模型切换载荷。
	if item.Kind != ItemModelChange {
		if item.ModelChange != nil {
			return fmt.Errorf("item kind %q cannot carry a model change", item.Kind)
		}
		return nil
	}
	// 2. 要求有 Item 和 Turn ID、没有模型调用 ID，前后模型都不为零且不同。
	change := item.ModelChange
	if item.ID == "" || item.TurnID == "" || item.ModelCallID != "" || change == nil ||
		change.PreviousModel == (ModelRef{}) || change.Model == (ModelRef{}) || change.PreviousModel == change.Model {
		return fmt.Errorf("model change requires item and turn IDs, no model call ID and two different models")
	}
	return nil
}

// ExecutorsChange 由 Turn 在本轮 Executor 列表和上一轮不同时写在本轮输入之前：默认换了、目录换了、增加或减少都在这一条里，
// 作为提示发给模型。
type ExecutorsChange struct {
	PreviousExecutors []TurnExecutor `json:"previous_executors"`
	Executors         []TurnExecutor `json:"executors"`
}

// ExecutorsChangeDetails 按变化种类拆开一条 ExecutorsChange，发给模型的文字和 TUI 显示共用。
type ExecutorsChangeDetails struct {
	// NewDefault 是换上的默认 Executor；默认没换或新列表为空时为 nil。
	NewDefault *TurnExecutor
	// PreviousDefaultID 是原来的默认 Executor ID；NewDefault 为 nil 时为空。
	PreviousDefaultID string
	// Removed 是不再可用的 Executor，目录取自上一轮。
	Removed []TurnExecutor
	// Added 是新增可用的 Executor；已经作为新默认写入 NewDefault 的不再重复列出。
	Added []TurnExecutor
	// DirectoryChanges 是仍然可用但根目录换了的 Executor；新默认的目录已写在 NewDefault 里，不再列出。
	DirectoryChanges []ExecutorDirectoryChange
}

// ExecutorDirectoryChange 是一个仍然可用的 Executor 在对话里的根目录变化。
type ExecutorDirectoryChange struct {
	ID                string
	PreviousDirectory string
	Directory         string
}

// ValidateExecutorsChange 检查 Executor 切换 Item 的 ID 和前后列表。
func (item *Item) ValidateExecutorsChange() error {
	// 1. 非 Executor 切换 Item 不得带切换载荷。
	if item.Kind != ItemExecutorsChange {
		if item.ExecutorsChange != nil {
			return fmt.Errorf("item kind %q cannot carry an executors change", item.Kind)
		}
		return nil
	}
	// 2. 要求有 Item 和 Turn ID、没有模型调用 ID，旧列表不为空且和新列表不同；新列表可以为空。
	change := item.ExecutorsChange
	if item.ID == "" || item.TurnID == "" || item.ModelCallID != "" || change == nil ||
		len(change.PreviousExecutors) == 0 || slices.Equal(change.PreviousExecutors, change.Executors) {
		return fmt.Errorf("executors change requires item and turn IDs, no model call ID and different executors with a previous list")
	}
	return nil
}
