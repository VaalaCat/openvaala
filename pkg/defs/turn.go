package defs

import "github.com/VaalaCat/ai-gateway/pkg/llmkit"

// TurnStatus 是一轮执行正在运行还是以什么方式结束。
type TurnStatus string

const (
	TurnStatusRunning     TurnStatus = "running"
	TurnStatusCompleted   TurnStatus = "completed"
	TurnStatusFailed      TurnStatus = "failed"
	TurnStatusInterrupted TurnStatus = "interrupted"
)

// TokenUsage 是一次或多次模型调用的 token 数，各家的统计方式已经统一：Input 包含缓存命中和缓存写入，
// CacheRead 和 CacheWrite 是 Input 的一部分，Reasoning 是 Output 的一部分。
type TokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	Reasoning  int `json:"reasoning"`
}

// Add 返回两份 token 用量相加的结果。
func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	// 1. 分别累加输入、输出、缓存读取、缓存写入和推理用量。
	return TokenUsage{
		Input:      u.Input + other.Input,
		Output:     u.Output + other.Output,
		CacheRead:  u.CacheRead + other.CacheRead,
		CacheWrite: u.CacheWrite + other.CacheWrite,
		Reasoning:  u.Reasoning + other.Reasoning,
	}
}

// TurnExecutor 是一个 Executor 和它在对话里的根目录。
type TurnExecutor struct {
	ID                 string `json:"id"`
	WorkspaceDirectory string `json:"workspace_directory"`
}

// TurnItemRange 是一轮已保存 Item 的对外序号范围，两端都包含；起点为零表示还没有 Item，运行中终点为零。
type TurnItemRange struct {
	FirstOrdinal uint64 `json:"first_ordinal"`
	LastOrdinal  uint64 `json:"last_ordinal"`
}

// TurnContextUsage 保存最近一次完整普通调用的用量和原输入位置；本轮没有完整普通调用时沿用上一轮的报告。
type TurnContextUsage struct {
	ModelCallID ModelCallID `json:"model_call_id"`
	// InputThroughItemID 是请求发出前上下文最后一条 Item，不含该次生成输出。
	InputThroughItemID ItemID `json:"input_through_item_id"`
	// Usage 是服务端上报并已归一化的用量；没有上报时为零值。
	Usage            TokenUsage              `json:"usage,omitzero"`
	ReasoningContext llmkit.ReasoningContext `json:"reasoning_context,omitempty"`
}

// Turn 是一轮 Agent 执行的记录：状态、错误、Item 范围、用量，以及用到的模型和 Executor。
type Turn struct {
	ID TurnID `json:"id"`
	// UpdatedAt 是最后一次写入的 Unix 毫秒，由存储在写入时分配；写入时设置的值不会被保存。
	UpdatedAt int64 `json:"updated_at,omitempty"`
	// ConversationID 是所属对话；DAO 据此找到对话目录里的 metadata.jsonl。
	ConversationID ConversationID `json:"conversation_id"`
	SessionID      SessionID      `json:"session_id"`
	Status         TurnStatus     `json:"status"`
	// FinalModelCallID 是本轮最后一次成功的普通模型调用；只在 completed 时填写。
	FinalModelCallID ModelCallID `json:"final_model_call_id,omitempty"`
	// Error 是 Turn 失败或被中断时的错误说明。
	Error      string        `json:"error,omitempty"`
	ItemsRange TurnItemRange `json:"items_range,omitzero"`
	// Usage 是本 Turn 全部模型调用（包括压缩）的用量合计，写入终态时填写；running 时为零。
	Usage TokenUsage `json:"usage,omitzero"`
	// ContextUsage 是最近一次完整普通调用的报告；本轮没有完整普通调用时沿用上一轮的报告，模型切换后为空。
	ContextUsage *TurnContextUsage `json:"context_usage,omitempty"`
	// Model 是本 Turn 实际使用的模型和推理强度，创建 running 记录时写入。
	Model ModelSelection `json:"model,omitzero"`
	// Executors 是本 Turn 开始时对话可用的 Executor，按顺序，第一个是默认；创建 running 记录时写入。
	Executors []TurnExecutor `json:"executors,omitempty"`
}
