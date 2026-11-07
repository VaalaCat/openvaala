package agent

import (
	"encoding/json"

	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
)

// ── Agent 输出事件 ──

// AgentEventType 区分 Item、工具进度、上下文测量、模型调用和 hook 运行事件。
type AgentEventType string

const (
	// AgentEventItemStarted 由 Agent 在创建输出草稿时发出。
	AgentEventItemStarted AgentEventType = "item_started"
	// AgentEventItemDelta 由 Agent 在消息、推理或工具参数出现增量时发出。
	AgentEventItemDelta AgentEventType = "item_delta"
	// AgentEventItemCompleted 由 Agent 在输出 Item 正常结束时发出。
	AgentEventItemCompleted AgentEventType = "item_completed"
	// AgentEventItemInterrupted 由 Agent 在输出草稿中断时发出。
	AgentEventItemInterrupted AgentEventType = "item_interrupted"
	// AgentEventToolProgress 由 Agent 在收到工具执行进度时发出。
	AgentEventToolProgress AgentEventType = "tool_progress"
	// AgentEventContextMeasured 由 Agent 在普通请求前、完整响应更新计数报告后、完整输出加入上下文后和模型循环退出前发出；
	// 只压缩的 Turn 在压缩结束后发送最终测量。
	AgentEventContextMeasured AgentEventType = "context_measured"
	// AgentEventModelCallFinished 由 Agent 在每次普通模型或摘要模型调用结束时发出，成功、失败和中断都会发。
	AgentEventModelCallFinished AgentEventType = "model_call_finished"
	// AgentEventHookRun 由 Agent 在工具 hook、Stop 或 SubagentStop 开始和结束时发出，同一次运行的 ID 不变；
	// 需要保存的运行记录另外写成 hook_run Item。
	AgentEventHookRun AgentEventType = "hook_run"
)

// AgentEvent 由 Agent 在输出 Item 变化、工具产生进度、测量完成、模型调用结束或 hook 运行变化时发出。
// 取消后仍继续发送，每个 Item 最后都会有一条完成或中断事件；整个 Run 的结果以返回值为准。
type AgentEvent struct {
	Type AgentEventType `json:"type"`
	// ItemIndex 是 Item 在本次 Run 输出中的位置；ContextMeasured、ModelCallFinished 和 HookRun 不对应 Item，不用它。
	ItemIndex int `json:"item_index"`
	// Item 是开始、完成和中断事件携带的完整 Item 副本，增量事件不带。
	// 中断事件的 Item 是要保存的内容，为 nil 时这个位置不用保存（例如没有文字的中断消息草稿）。
	// ToolCall 的完成事件只说明参数已完整，工具还没执行。
	Item *defs.Item `json:"item,omitempty"`
	// Delta 是增量事件携带的追加内容。
	Delta *AgentItemDelta `json:"delta,omitempty"`
	// ToolProgress 是 ToolProgress 事件携带的工具输出。
	ToolProgress *AgentToolProgress `json:"tool_progress,omitempty"`
	// Measurement 是 ContextMeasured 事件携带的上下文用量。
	Measurement *agent_context.ContextMeasurement `json:"measurement,omitempty"`
	// ModelCall 是 ModelCallFinished 事件携带的调用结果。
	ModelCall *AgentModelCall `json:"model_call,omitempty"`
	// HookRun 是 HookRun 事件携带的运行记录。
	HookRun *defs.HookRun `json:"hook_run,omitempty"`
}

// AgentItemDelta 是 Agent 生成的一段内容增量，按 Item.Kind 填对应字段。
// 增量里的切片是单独的副本，接收方可以直接留用；推理的分段和签名以完成事件里的完整 Item 为准。
type AgentItemDelta struct {
	// Text 是追加到消息的文本。
	Text string `json:"text,omitempty"`
	// ReasoningSummary 是追加的推理摘要片段。
	ReasoningSummary []string `json:"summary,omitempty"`
	// ReasoningContent 是追加的推理内容片段。
	ReasoningContent []string `json:"content,omitempty"`
	// Arguments 是追加的工具参数。
	Arguments string `json:"arguments,omitempty"`
}

// ── Agent 工具进度 ──

// AgentToolProgress 是工具执行中途的一段输出，最终结果另由 ToolResult Item 给出；事件的 ItemIndex 指向对应的 ToolCall。
type AgentToolProgress struct {
	// CallID 是对应工具调用的 CallID，可以关联执行前已保存的 ToolCall。
	CallID string `json:"call_id"`
	// Stream 是这段输出所属的流类别。
	Stream executor.ExecutionStreamType `json:"stream"`
	// Content 是这段输出的 JSON 内容。
	Content json.RawMessage `json:"content"`
}

// ── Agent 模型调用 ──

// ModelCallPurpose 是一次模型调用的用途。
type ModelCallPurpose string

const (
	// ModelCallPurposeConversation 是产生对话输出的普通模型调用。
	ModelCallPurposeConversation ModelCallPurpose = "conversation"
	// ModelCallPurposeCompaction 是生成上下文摘要的摘要模型调用。
	ModelCallPurposeCompaction ModelCallPurpose = "compaction"
)

// AgentModelCall 保存一次已结束模型调用的结果；失败和取消也报告已产生的消耗，未上报用量时 Usage 为 nil。
type AgentModelCall struct {
	ModelCallID defs.ModelCallID `json:"model_call_id"`
	Purpose     ModelCallPurpose `json:"purpose"`
	Usage       *defs.TokenUsage `json:"usage,omitempty"`
	// ContextUsage 只在普通调用正常结束时填写，Context 和 Turn 用同一份。
	ContextUsage *defs.TurnContextUsage `json:"context_usage,omitempty"`
}

// ── Agent 运行结果 ──

// AgentResult 是 Agent 一次 Run 的结果。
type AgentResult struct {
	// Items 是本轮全部草稿，包括失败尝试的输出。
	Items []*defs.Item
	// FinalModelCallID 是最后一次成功的普通模型调用；Run 失败或只压缩时为空。
	FinalModelCallID defs.ModelCallID
	// Stats 是本次运行（一个 Turn）的统计，Turn 据此写遥测小结。
	Stats AgentStats
}

// AgentStats 是 Agent 一次 Run 累计的调用统计。
type AgentStats struct {
	// Steps 是普通模型调用次数，包含失败和重试。
	Steps int
	// InferenceCalls 是普通模型和摘要模型的调用总次数，包含失败和重试。
	InferenceCalls int
	// ToolCalls 是收到的工具结果数，包含没能分发、直接返回错误的调用。
	ToolCalls int
	// FailedToolCalls 是其中带错误的工具结果数。
	FailedToolCalls int
	// Usage 是全部模型调用的 token 合计。
	Usage defs.TokenUsage
	// FinishReason 是最后一次普通模型调用的结束原因，已换成遥测取值；这次调用失败或被取消、又没收到结束原因时为 error。
	FinishReason string
}

// ── Agent 输入请求 ──

// AgentInputRequest 由 Agent 在每步调用模型前、或准备结束本轮时发给 Turn，要求读取补充输入。
type AgentInputRequest struct {
	// FinishIfEmpty 没有补充输入时是否让 Session 停止向本轮投递？Agent 准备结束本轮时为 true。
	FinishIfEmpty bool
	// Reply 接收 Turn 的回复，Turn 回复后关闭它。
	Reply chan<- AgentInputResult
}

// ── Agent 输入回复 ──

// AgentInputResult 由 Turn 在处理完补充输入请求后回复，带回已保存的输入或处理错误。
type AgentInputResult struct {
	Items []*defs.Item
	Err   error
}

// ── Agent 角色定义 ──

// AgentDefinition 是一个可选用的 Agent 角色：指令、默认模型、摘要模型、工具和 skill，构造后只读。
type AgentDefinition struct {
	// Name 是角色名，对应 Session.AgentName。
	Name string
	// Description 说明这个角色适合处理的任务。
	Description string
	// Instructions 是这个角色的系统指令。
	Instructions string
	// Model 是本角色新建 Session 的默认模型；Session 可以改选其他模型。
	Model defs.ModelRef
	// CompactModel 是生成上下文摘要使用的模型；零值表示使用本轮模型。
	CompactModel defs.ModelRef
	// ToolNames 是可用的普通工具名；为空时可用工具目录中的全部工具，协作工具始终可用。
	ToolNames []string
	// SkillNames 是可用的 skill 名；为空时可用 skill 目录中的全部 skill。
	SkillNames []string
	// MCPServerIDs 是可用的 MCP server id；nil 表示全部可用，[] 表示不用 MCP。
	MCPServerIDs defs.OptionalStringList
	// MaxSteps 是一轮最多调用模型的次数；0 表示不限。
	MaxSteps int
}
