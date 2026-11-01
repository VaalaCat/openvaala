package agent_context

import (
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// ── Context 加载参数 ──

// ContextLoadParam 是 Load 整体替换用的指令和历史；字段为空就清空对应内容。
type ContextLoadParam struct {
	// System 是系统指令。
	System string
	// Items 是有序历史：最新摘要加上没被它覆盖的原文。
	Items []*defs.Item
	// ContextUsage 是最近完整普通调用的报告；为 nil 时仅本地计数。
	ContextUsage *defs.TurnContextUsage
}

// ── Context 测量 ──

// ContextMeasurement 保存指令、工具和历史的估算用量，供压缩判断和用量展示读取。
type ContextMeasurement struct {
	// Model 是本次测量使用的模型，不依赖最近调用详情是否已加载。
	Model defs.ModelRef `json:"model"`
	// Window 是模型上下文窗口；0 表示窗口未知，此时不触发自动压缩。
	Window int `json:"window"`
	// ThresholdTokens 是预留最大输出后的自动压缩阈值；窗口未知时为 0。
	ThresholdTokens int `json:"threshold_tokens"`
	// RequestTokens 是利用最近实际用量和内容变化计算的预计占用；无适用报告时等于 EstimatedTokens。
	RequestTokens int `json:"request_tokens"`
	// EstimatedTokens 是未经校正的本地估算，等于 SystemTokens、ToolTokens 和 Items 各项 Tokens 之和。
	EstimatedTokens int `json:"estimated_tokens"`
	// IsUsageBased 本次预计占用是否使用了仍适用的模型上报用量？
	IsUsageBased bool `json:"is_usage_based"`
	// SystemTokens 是系统指令的估算用量。
	SystemTokens int `json:"system_tokens"`
	// ToolTokens 是工具定义的估算用量。
	ToolTokens int `json:"tool_tokens"`
	// Items 只含会发给模型的 Item，按上下文顺序排列。
	Items []ItemTokens `json:"items"`
}

// ItemTokens 保存一条模型可见 Item 的身份、类别和估算用量。
type ItemTokens struct {
	ItemID   defs.ItemID     `json:"item_id"`
	TurnID   defs.TurnID     `json:"turn_id"`
	Category ContextCategory `json:"category"`
	Tokens   int             `json:"tokens"`
}

// ContextCategory 区分模型可见 Item 的业务类别。
type ContextCategory string

const (
	ContextCategoryInstruction     ContextCategory = "instruction"    // Skill、目录规则和 hook 输出
	ContextCategorySummary         ContextCategory = "summary"        // 上下文摘要
	ContextCategoryUserInput       ContextCategory = "user_input"     // 用户输入
	ContextCategoryAgentMessage    ContextCategory = "agent_message"  // 其他 Session 发来的消息，包括子 Agent 的结果通知
	ContextCategoryAssistantText   ContextCategory = "assistant_text" // 本 Session 模型输出的文本
	ContextCategoryReasoning       ContextCategory = "reasoning"
	ContextCategoryToolCall        ContextCategory = "tool_call"
	ContextCategoryToolResult      ContextCategory = "tool_result"
	ContextCategoryModelError      ContextCategory = "model_error"
	ContextCategoryModelChange     ContextCategory = "model_change"     // 模型切换提示
	ContextCategoryExecutorsChange ContextCategory = "executors_change" // Executor 列表变化提示
	ContextCategoryTurnInterrupted ContextCategory = "turn_interrupted" // 上一轮被中断提示
)

// ── Context 压缩结果 ──

// CompactionCall 保存一次发起过的摘要模型调用的 ID 和用量，成功、失败时都会返回；未上报用量时 Usage 为 nil。
type CompactionCall struct {
	ModelCallID defs.ModelCallID
	Usage       *defs.TokenUsage
}
