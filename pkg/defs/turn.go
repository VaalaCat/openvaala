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
	// IsCompactOnly 是否只运行手动压缩？普通 Turn 触发自动压缩时仍为 false。
	IsCompactOnly bool `json:"is_compact_only,omitempty"`
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

// ModelCallPurpose 是一次模型调用的用途。
type ModelCallPurpose string

const (
	// ModelCallPurposeConversation 是产生对话输出的普通模型调用。
	ModelCallPurposeConversation ModelCallPurpose = "conversation"
	// ModelCallPurposeCompaction 是生成上下文摘要的摘要模型调用。
	ModelCallPurposeCompaction ModelCallPurpose = "compaction"
)

// TurnCheckpointPhase 是进度所记录的模型请求或已放行工具批。
type TurnCheckpointPhase string

const (
	TurnCheckpointPhaseRequest TurnCheckpointPhase = "request"
	TurnCheckpointPhaseTools   TurnCheckpointPhase = "tools"
)

// TurnCheckpoint 保存原 Turn 当时的模型请求或工具批进度。
type TurnCheckpoint struct {
	// TurnID 是本条进度所属的原轮次。
	TurnID TurnID `json:"turn_id"`
	// Step 是本轮普通生成的步号，从 1 开始；重试和步内摘要不消耗新步。
	Step int `json:"step"`
	// Phase 只取 request（即将发普通或摘要请求）或 tools（这批工具已放行）。
	Phase TurnCheckpointPhase `json:"phase"`
	// StopHookContinuations 是连续被停止 hook 拦截并续跑的次数，进入 tools 时清零。
	StopHookContinuations int `json:"stop_hook_continuations"`
	// IsContinuedByStopHook 本轮是否曾因停止 hook 续跑？进入工具阶段也不清除。
	IsContinuedByStopHook bool `json:"is_continued_by_stop_hook"`
	// Request 只在 request 阶段填写，tools 时为 nil。
	Request *TurnRequestCheckpoint `json:"request,omitempty"`
	// Tools 只在 tools 阶段填写，request 时为 nil。
	Tools *TurnToolsCheckpoint `json:"tools,omitempty"`
}

// TurnRequestCheckpoint 保存发起模型请求前选定的范围和本次尝试 ID。
type TurnRequestCheckpoint struct {
	// Purpose 区分普通回复 conversation 和摘要 compaction。
	Purpose ModelCallPurpose `json:"purpose"`
	// ModelCallID 是这次模型尝试的身份，重试用新 ID。
	ModelCallID ModelCallID `json:"model_call_id"`
	// ModelCallThroughItemID 是普通请求的最后一个输入 Item（含本条，不含本次输出）；摘要请求时为空。
	ModelCallThroughItemID ItemID `json:"model_call_through_item_id,omitempty"`
	// Compaction 只在摘要请求时填写，普通请求为 nil。
	Compaction *TurnCompactionCheckpoint `json:"compaction,omitempty"`
}

// TurnCompactionCheckpoint 保存原摘要选择和范围。
type TurnCompactionCheckpoint struct {
	// SummaryItemID 是要生成的摘要 Item 的身份，重启仍用它查找是否已经写入。
	SummaryItemID ItemID `json:"summary_item_id"`
	// CoveredThroughItemID 是这份摘要覆盖到的最后一个原始 Item，包含它本身。
	CoveredThroughItemID ItemID `json:"covered_through_item_id"`
	// CompactModelRef 是此次实际选用的摘要模型（含提供方）。
	CompactModelRef ModelRef `json:"compact_model_ref"`
}

// TurnToolsCheckpoint 标记普通模型生成的一批工具调用已放行。
type TurnToolsCheckpoint struct {
	// ModelCallID 指生成这批 ToolCall 的普通模型调用；逐个 CallID 从已存的本批 Item 读取。
	ModelCallID ModelCallID `json:"model_call_id"`
}
