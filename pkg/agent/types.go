package agent

import (
	"encoding/json"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"

	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
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

// AgentModelCall 保存一次已结束模型调用的结果；失败和取消也报告已产生的消耗，未上报用量时 Usage 为 nil。
type AgentModelCall struct {
	ModelCallID defs.ModelCallID      `json:"model_call_id"`
	Purpose     defs.ModelCallPurpose `json:"purpose"`
	Usage       *defs.TokenUsage      `json:"usage,omitempty"`
	// ContextUsage 只在普通调用正常结束时填写，记录原请求的位置和用量。
	ContextUsage *defs.TurnContextUsage `json:"context_usage,omitempty"`
	// Result 是本次普通模型调用完整结束时的调用级事实；非末项、失败和未结束时为空。
	Result defs.ModelCallResult `json:"result,omitempty"`
}

// ── Agent 恢复输入 ──

// AgentResumeInput 是 Turn 根据已保存事实构造的一次性恢复执行输入。
type AgentResumeInput struct {
	// Step 是原请求的普通生成步号。
	Step int
	// StopHookContinuations 是原请求连续被停止 hook 拦截的次数。
	StopHookContinuations int
	// IsContinuedByStopHook 表示此前是否已因停止 hook 续跑。
	IsContinuedByStopHook bool
	// ExistingItems 是本轮已经保存的输出，作为结果前缀，不再发送事件。
	ExistingItems []*defs.Item
	// Usage 是本轮从原调用报告恢复的累计用量。
	Usage defs.TokenUsage
	// Retry 是未完整结束的普通请求；不是重试分支时为空。
	Retry *AgentResumeRetry
	// Finish 是已完整结束的无工具回复；不是回复收尾分支时为空。
	Finish *AgentResumeFinish
	// ToolBatch 是已放行并补齐结果的工具批；不恢复工具批时为空。
	ToolBatch *AgentResumeToolBatch
	// Compaction 是本步已确认的摘要；没有恢复摘要 checkpoint 时为空。
	Compaction *AgentResumeCompaction
}

// AgentResumeCompaction 保存确认过的摘要身份和重试请求的原始输入。
type AgentResumeCompaction struct {
	// SummaryItemID 是原请求分配的摘要 Item ID。
	SummaryItemID defs.ItemID
	// Source 是原请求使用的历史前缀，末项是原覆盖末项；已保存摘要正文时为空。
	Source []*defs.Item
	// Model 是原请求使用的摘要模型；已保存摘要正文时为空。
	Model *defs.ModelDefinition
}

// AgentResumeRetry 保存重试同一普通生成步需要的输入位置。
type AgentResumeRetry struct {
	// InputThroughItemID 是原请求已经确认的逻辑末项。
	InputThroughItemID defs.ItemID
	// NeedsModelError 表示旧尝试有残留输出但还没有失败记录。
	NeedsModelError bool
}

// AgentResumeFinish 保存完整回复后的收尾输入。
type AgentResumeFinish struct {
	// ModelCallID 是已经完整结束的普通模型调用。
	ModelCallID defs.ModelCallID
	// LastAssistantMessage 是原调用最后一条回复的正文；空回复时为空。
	LastAssistantMessage string
	// HasInput 表示回复之后已经交接了有效补充输入。
	HasInput bool
}

// AgentResumeToolBatch 保存已持久化的工具批结算输入。
type AgentResumeToolBatch struct {
	// ModelCallID 是产生这批工具调用的模型请求。
	ModelCallID defs.ModelCallID
	// Results 是按原 CallID 顺序配对的工具结果，包含补写的未知结果。
	Results []*defs.ToolResult
	// IsTurnStopped 表示批内已有工具 hook 要求结束本轮。
	IsTurnStopped bool
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

// ── Agent 发给 Turn 的请求 ──

// AgentRequest 限定 Agent 可以发给 Turn 的请求类型。
type AgentRequest interface{ isAgentRequest() }

// AgentInputRequest 由 Agent 在每步调用模型前、或准备结束本轮时发给 Turn，要求读取补充输入。
type AgentInputRequest struct {
	// FinishIfEmpty 没有补充输入时是否让 Session 停止向本轮投递？Agent 准备结束本轮时为 true。
	FinishIfEmpty bool
	// Reply 接收 Turn 的回复，Turn 回复后关闭它。
	Reply chan<- AgentInputResult
}

// AgentSaveCheckpointRequest 由 Agent 在发起模型调用或准备工具前发给 Turn。
type AgentSaveCheckpointRequest struct {
	// Checkpoint 是本次模型尝试或要放行的工具批进度。
	Checkpoint defs.TurnCheckpoint
	// AfterItemID 是写进度前要确认已保存的最后一项；没有前置 Item 时为空。
	AfterItemID defs.ItemID
	// Reply 由 Turn 回送保存结果后关闭；nil 表示 Item 和进度均已写成。
	Reply chan<- error
}

func (AgentInputRequest) isAgentRequest()          {}
func (AgentSaveCheckpointRequest) isAgentRequest() {}

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
	// MaxSteps 是本轮普通生成的步数上限，不含摘要或重试；0 表示不限。最后一步禁用工具并要求模型总结进度。
	MaxSteps int
}

// ── Agent 任务标识与状态 ──

// agentTaskID 是单次 Agent.Run 内递增分配的唯一任务标识。
type agentTaskID uint64

// agentTaskStatus 是内存任务的生命周期状态，由任务在 Run 或 Cancel 内更新，供 Agent 在 Run 外读取。
type agentTaskStatus string

const (
	// agentTaskPending 表示任务已创建，尚未开始运行。
	agentTaskPending agentTaskStatus = "pending"
	// agentTaskRunning 表示任务正在执行本次 Run。
	agentTaskRunning agentTaskStatus = "running"
	// agentTaskWaiting 表示任务已暂停并返回依赖列表，等待依赖任务全部完成后再次调用 Run。
	agentTaskWaiting agentTaskStatus = "waiting"
	// agentTaskCompleted 表示任务已成功完成全部阶段。
	agentTaskCompleted agentTaskStatus = "completed"
	// agentTaskFailed 表示任务因非取消错误而失败结束。
	agentTaskFailed agentTaskStatus = "failed"
	// agentTaskCanceled 表示任务因上下文取消或调用 Cancel 而取消。
	agentTaskCanceled agentTaskStatus = "canceled"
)

// generationPhase 是生成任务内部依次推进的执行阶段，用于等待返回后继续对应分支。
type generationPhase string

const (
	// generationPrepare 表示准备本步输入和模型请求，或等待摘要完成的阶段。
	generationPrepare generationPhase = "prepare"
	// generationModel 表示调用普通模型、转发流事件并收集输出与工具调用的阶段。
	generationModel generationPhase = "model"
	// generationTools 表示准备并等待本批工具任务完成，再汇总进入下一步的阶段。
	generationTools generationPhase = "tools"
	// generationFinish 表示无工具调用时运行停止 hook 并检查补充输入的结束阶段。
	generationFinish generationPhase = "finish"
)

// ── Agent 任务输入 ──

// agentTaskInput 限定 Agent 可以交给任务 Run 的输入类型。
type agentTaskInput interface{ isAgentTaskInput() }

// generationTaskStart 保存新生成任务第一次运行时使用的步号和停止 hook 状态。
type generationTaskStart struct {
	// Step 是本轮普通生成的步号，从 1 开始，摘要和重试不另计步。
	Step int
	// StopHookContinuations 记录连续因停止 hook 拦截而续跑的次数；进入工具阶段时清零。
	StopHookContinuations int
	// IsContinuedByStopHook 本轮是否曾因停止 hook 拦截而续跑？后续调用停止 hook 时仍带上这个标记。
	IsContinuedByStopHook bool
	// SkipInputRead 是恢复未完成请求时是否跳过读取后来到达的补充输入。
	SkipInputRead bool
	// ResumeFinish 是已有完整回复的收尾状态；非 nil 时从停止 hook 阶段继续。
	ResumeFinish *AgentResumeFinish
	// ResumeToolBatch 是已补齐结果的工具批；非 nil 时从批次结算继续。
	ResumeToolBatch *AgentResumeToolBatch
	// ResumeCompaction 是本普通步已确认的摘要；没有恢复摘要 checkpoint 时为空。
	ResumeCompaction *AgentResumeCompaction
}

// generationTaskInput 由 Agent 传给生成任务的 Run，每次只填一项。
type generationTaskInput struct {
	// Start 只在新生成任务第一次运行时传入。
	Start *generationTaskStart
	// Completions 在摘要任务完成或这一批工具调用都有结果后，由 Agent 把结果再次传给同一个生成任务的 Run。
	Completions []agentTaskCompletion
}

// toolTaskInput 由 Agent 在启动单次工具任务时传入。
type toolTaskInput struct {
	// Call 是执行器准备好的调用；准备时已有结果就直接结算，不再执行。
	Call *executor.PreparedToolCall
	// ExecutorEvents 由 Agent 创建和关闭，执行器用它把进度、hook 和结果直接发给 Agent。
	ExecutorEvents chan<- executor.ExecutorEvent
}

// compactionTaskInput 由 Agent 在选定要压缩的历史后交给摘要任务。
type compactionTaskInput struct {
	// Request 是 Context 用待压缩历史组装的摘要请求，禁用工具调用。
	Request *llmkit.Request
	// Model 是 Context 选用的摘要模型，由 Context 随请求返回。
	Model *defs.ModelDefinition
	// Client 是 Context 配置的摘要客户端，由 Context 随请求返回。
	Client llmkit.Client
	// Summary 是 Agent 摘要草稿的模板副本。任务复制模板并填入摘要结果，由 Agent 更新原草稿并写入上下文。
	Summary *defs.Item
	// Generation 保存摘要所属普通生成的步号和停止 hook 续跑状态；手动压缩用第 1 步，续跑状态为零值。
	Generation generationTaskStart
	// IsManual 本次摘要是否由手动压缩触发？Run 不读取此字段。
	IsManual bool
	// IsCompacted 请求发出前的上下文是否已有摘要？它不表示本次压缩是否成功。
	IsCompacted bool
}

func (*generationTaskInput) isAgentTaskInput() {}
func (*toolTaskInput) isAgentTaskInput()       {}
func (*compactionTaskInput) isAgentTaskInput() {}

// ── Agent 任务结果 ──

// agentTaskOutput 限定各类内存任务通过 agentTaskCompletion 交回的结果类型。
type agentTaskOutput interface{ isAgentTaskOutput() }

// agentTaskRunResult 是任务单次 Run 返回给 Agent 的结果。
type agentTaskRunResult struct {
	// WaitingOn 是当前任务等待的依赖任务 ID 列表；为空只表示不再等待，不代表执行成功。
	WaitingOn []agentTaskID
}

// agentTaskCompletion 保存任务到达终态后的结果，由 Agent 通过 GetCompletion 取出并交给等待它的生成任务。
type agentTaskCompletion struct {
	// TaskID 是已到达终态的任务标识，Agent 据此在任务表中移除已交付的依赖。
	TaskID agentTaskID
	// Output 是任务成功完成时产出的类型化结果，供 Agent 或等待它的生成任务读取；失败时可为 nil。
	Output agentTaskOutput
	// Err 是任务失败或取消时记录的错误，生成任务收到非 nil 错误时直接终止本轮。
	Err error
}

// generationTaskOutput 是生成任务完成后通过 GetCompletion 交给 Agent 的最终输出。
type generationTaskOutput struct {
	// FinalModelCallID 是本步最近一次成功普通模型调用的 ID，Agent 在不再启动下一步时记为本轮最终调用 ID。
	FinalModelCallID defs.ModelCallID
	// NextGeneration 是下一步生成任务的启动参数，由 Agent 在 handleTaskRunReturned 中据此创建新生成任务；为 nil 表示本轮结束。
	NextGeneration *generationTaskStart
}

// toolTaskOutput 是工具任务完成后通过 GetCompletion 返回的工具执行结果。
type toolTaskOutput struct {
	// Finished 是本次工具调用已结算的最终执行事件，由工具任务写入并供生成任务检查 hook 停止状态。
	Finished *executor.ExecutionFinishedEvent
}

// compactionTaskOutput 保存一次摘要模型调用的用量和已填充内容的摘要草稿，由压缩任务发给 Agent 结算。
type compactionTaskOutput struct {
	// Summary 是克隆模板后填入模型调用 ID 和摘要文本的 ContextSummary Item；调用失败时为 nil，成功时由 Agent 据此更新原草稿。
	Summary *defs.Item
	// Call 是本次摘要模型调用的标识和 token 用量，无论成功或失败都由 Agent 计入本轮统计并发出完成事件。
	Call *agent_context.CompactionCall
}

func (*generationTaskOutput) isAgentTaskOutput() {}
func (*toolTaskOutput) isAgentTaskOutput()       {}
func (*compactionTaskOutput) isAgentTaskOutput() {}

// ── Agent 任务事件 ──

// agentTaskEvent 限定内存任务通过 taskEvents 通道向 Agent 发送的事件和同步请求类型。
type agentTaskEvent interface{ isAgentTaskEvent() }

// agentTaskModelEvent 由生成任务收到模型流事件后发出。
type agentTaskModelEvent struct {
	// Event 是普通模型流产生的一条 llmkit 事件，由生成任务转发给 Agent 更新草稿和统计。
	Event llmkit.Event
	// ModelCallResult 是流在 EventDone 时确认的调用级结果；未完整结束时为空。
	ModelCallResult defs.ModelCallResult
}

// agentTaskExecutorEvent 由生成任务在 PrepareCalls 阶段收到执行器事件后转发给 Agent。
type agentTaskExecutorEvent struct {
	// TaskID 是转发该执行器事件的生成任务标识。
	TaskID agentTaskID
	// Event 是 PrepareCalls 阶段收到的执行器事件，由 Agent 交给 handleExecutorEvent 合并并发送。
	Event executor.ExecutorEvent
}

// agentTaskRunReturnedEvent 由 Agent 启动的任务调用在 Run 返回后发出，等待依赖也会发出。
type agentTaskRunReturnedEvent struct {
	// TaskID 是本次 Run 返回的任务标识，Agent 据此将其移出 runningTaskIDs 并读取任务状态。
	TaskID agentTaskID
	// Result 是 task.Run 返回的调度结果，Agent 据此判断任务是否正在等待依赖。
	Result agentTaskRunResult
	// Err 是本次 task.Run 返回的错误。
	Err error
}

// agentTaskHookRunEvent 由生成任务收到 Stop 或 SubagentStop hook 的运行更新后发出。
type agentTaskHookRunEvent struct {
	// TaskID 是转发该 hook 运行更新的生成任务标识。
	TaskID agentTaskID
	// Run 是 Stop 或 SubagentStop hook 的运行记录快照，由 Agent 直接转为 AgentEventHookRun 发出。
	Run *defs.HookRun
}

func (*agentTaskModelEvent) isAgentTaskEvent()       {}
func (*agentTaskExecutorEvent) isAgentTaskEvent()    {}
func (*agentTaskRunReturnedEvent) isAgentTaskEvent() {}
func (*agentTaskHookRunEvent) isAgentTaskEvent()     {}

// ── Agent 任务请求 ──

// agentTaskGenerationInputRequested 由生成任务开始准备或等待摘要结束后发出。
type agentTaskGenerationInputRequested struct {
	// TaskID 是发起本次输入准备请求的生成任务标识。
	TaskID agentTaskID
	// Step 是当前生成步号，Agent 据此为准备阶段的日志和遥测上下文附加步数属性。
	Step int
	// IsAfterCompaction 本次准备是否发生在摘要任务完成之后？为 true 时跳过补充输入读取与再次压缩检查。
	IsAfterCompaction bool
	// SkipInputRead 是恢复旧请求时是否跳过后来到达的补充输入。
	SkipInputRead bool
	// SkipEnvironmentPreparation 是恢复旧请求时是否跳过 AGENTS、memory 和压缩准备。
	SkipEnvironmentPreparation bool
	// ResumeCompaction 是这次准备要接续的摘要；不从摘要 checkpoint 恢复时为空。
	ResumeCompaction *AgentResumeCompaction
	// Reply 是生成任务等待的回复通道，Agent 写入输入准备结果后关闭。
	Reply chan agentTaskGenerationInputResult
}

// agentTaskModelCallStartedRequest 由生成任务在每次调用模型之前发出。
type agentTaskModelCallStartedRequest struct {
	// Checkpoint 记录本次普通调用的身份、步号和输入范围。
	Checkpoint defs.TurnCheckpoint
	// Reply 由 Agent 在 Turn 确认写入且登记模型流后回复并关闭；失败或取消时回错。
	Reply chan error
}

// agentTaskSaveCheckpointRequested 由摘要任务发模型请求前、或生成任务准备整批工具前发出。
type agentTaskSaveCheckpointRequested struct {
	// Checkpoint 记录本次摘要尝试或本批待放行的工具。
	Checkpoint defs.TurnCheckpoint
	// AfterItemID 是工具批最后一条 ToolCall；摘要留空，由 Agent 选取本次请求的前置 Item。
	AfterItemID defs.ItemID
	// Reply 接收 Agent 返回的写入结果或取消错误，回复后关闭。
	Reply chan error
}

// agentTaskModelCallFinishedRequest 由生成任务转发完一次模型调用的事件后发出。
type agentTaskModelCallFinishedRequest struct {
	// Step 是本次模型调用所属的步号，Agent 据此为结算阶段的日志和遥测附加步数属性。
	Step int
	// CallErr 是 Client.Call 或模型流转发返回的错误，Agent 据此结束遥测、收尾未完成草稿并决定是否写入上下文。
	CallErr error
	// Recorder 是生成任务在调用前启动的聊天遥测记录器，Agent 仅在开启对应配置时记录模型输出，但都会结束遥测。
	Recorder *telemetry.ChatRecorder
	// Reply 是生成任务等待的结算回复通道，Agent 写入可用调用结果或错误后关闭。
	Reply chan agentTaskModelCallResult
}

// agentTaskToolsRequested 由生成任务在执行器准备完整批次后发出。
type agentTaskToolsRequested struct {
	// Step 是发起工具请求的生成步号，Agent 据此附加遥测步数属性。
	Step int
	// TaskID 是请求创建工具任务的生成任务标识。
	TaskID agentTaskID
	// Calls 是执行器 PrepareCalls 返回的已准备工具调用列表，Agent 为每项创建并启动一个工具任务。
	Calls []*executor.PreparedToolCall
	// Reply 是生成任务等待的创建结果通道，Agent 写入已启动的工具任务 ID 列表或错误后关闭。
	Reply chan agentTaskCreationResult
}

// agentTaskToolResultRequested 由工具任务执行完调用后发出，取得 Agent 已合并的最终结果。
type agentTaskToolResultRequested struct {
	// TaskID 是请求取回工具执行结果的 toolTask 标识。
	TaskID agentTaskID
	// CallID 是工具调用的 CallID，Agent 据此从 toolCallsByCallID 取出并移除已合并的最终执行结果。
	CallID string
	// Reply 是 toolTask 等待的结果通道，Agent 写入已合并的执行事件或缺失错误后关闭。
	Reply chan agentTaskToolResult
}

// agentTaskToolCallsRejectedRequest 由生成任务在最后一步收到工具调用时发出。
type agentTaskToolCallsRejectedRequest struct {
	// TaskID 是最后一步收到工具调用并请求拒绝的生成任务标识。
	TaskID agentTaskID
	// Calls 是最后一步禁用工具后模型仍返回的工具调用列表，Agent 为每个调用写入拒绝结果后通过 Reply 返回处理结果。
	Calls []*defs.Item
	// Reply 是生成任务等待拒绝处理结果的通道，Agent 通过该通道返回处理结果后关闭。
	Reply chan error
}

// agentTaskCompactionRequested 由生成任务在准备阶段收到摘要请求后发出。
type agentTaskCompactionRequested struct {
	// Step 是触发摘要请求时的生成步号，Agent 据此为启动压缩任务的上下文附加步数属性。
	Step int
	// TaskID 是请求创建摘要任务的生成任务标识。
	TaskID agentTaskID
	// Input 是生成任务从输入准备阶段得到的摘要任务输入，由 Agent 在创建并启动压缩任务时传入。
	Input compactionTaskInput
	// Reply 是生成任务等待的创建结果通道，Agent 写入已启动的摘要任务 ID 或取消错误后关闭。
	Reply chan agentTaskCreationResult
}

// agentTaskCompactionFinishedRequest 由压缩任务在每次摘要模型调用结束后发出。
type agentTaskCompactionFinishedRequest struct {
	// TaskID 是完成一次摘要模型尝试的压缩任务标识。
	TaskID agentTaskID
	// Output 是本次摘要尝试的调用用量和摘要内容，Agent 据此累计统计、发事件并在成功时更新原草稿。
	Output compactionTaskOutput
	// Err 是本次摘要模型调用返回的错误；非 nil 时 Agent 只记入调用统计与事件，不将摘要写入上下文。
	Err error
	// Reply 只返回 Agent 处理摘要结果时的错误；摘要模型的错误由 Err 传入。
	Reply chan error
}

// agentTaskStopHooksFinishedRequest 由生成任务在 Stop 或 SubagentStop hook 结束后发出。
type agentTaskStopHooksFinishedRequest struct {
	// TaskID 是执行完停止 hook 的生成任务标识。
	TaskID agentTaskID
	// ModelCallID 是本步普通模型调用的 ID，Agent 据此创建并关联要保存的 Stop 或 SubagentStop hook 记录。
	ModelCallID defs.ModelCallID
	// Outcome 是停止 hook 的执行结果，Agent 将其中的 SavedRuns 转为 Item 写入上下文、草稿并发出事件。
	Outcome *hook.HookOutcome
	// Reply 是生成任务等待的通道，Agent 写入 hook 记录保存结果后关闭。
	Reply chan error
}

// agentTaskToolBatchFinishedRequest 由生成任务在本批工具结果齐备后发出。
type agentTaskToolBatchFinishedRequest struct {
	// Step 是当前工具批次所属的生成步号，Agent 据此附加日志与遥测步数属性。
	Step int
	// TaskID 是报告工具批次完成的生成任务标识。
	TaskID agentTaskID
	// ModelCallID 是发起本批工具调用的模型调用 ID，Agent 据此关联批收尾时追加的目录规则 Item。
	ModelCallID defs.ModelCallID
	// Results 是按调用顺序配对的结果，供 Agent 收集成功执行环境。
	Results []*defs.ToolResult
	// Reply 是生成任务等待的通道，Agent 处理完工作区和工具带回的目录规则后写入结果并关闭。
	Reply chan error
}

// agentTaskFinalInputRequested 由生成任务在准备正常结束且允许接收补充输入时发出。
type agentTaskFinalInputRequested struct {
	// Step 是请求最终补充输入时的生成步号，Agent 据此附加日志步数属性。
	Step int
	// TaskID 是准备结束本轮并请求最终补充输入的生成任务标识。
	TaskID agentTaskID
	// Reply 是生成任务等待的最终输入通道，Agent 调用 readInputs 并写入结果后关闭。
	Reply chan agentTaskFinalInputResult
}

func (*agentTaskGenerationInputRequested) isAgentTaskEvent()  {}
func (*agentTaskModelCallStartedRequest) isAgentTaskEvent()   {}
func (*agentTaskModelCallFinishedRequest) isAgentTaskEvent()  {}
func (*agentTaskSaveCheckpointRequested) isAgentTaskEvent()   {}
func (*agentTaskToolResultRequested) isAgentTaskEvent()       {}
func (*agentTaskToolsRequested) isAgentTaskEvent()            {}
func (*agentTaskToolCallsRejectedRequest) isAgentTaskEvent()  {}
func (*agentTaskCompactionRequested) isAgentTaskEvent()       {}
func (*agentTaskCompactionFinishedRequest) isAgentTaskEvent() {}
func (*agentTaskStopHooksFinishedRequest) isAgentTaskEvent()  {}
func (*agentTaskToolBatchFinishedRequest) isAgentTaskEvent()  {}
func (*agentTaskFinalInputRequested) isAgentTaskEvent()       {}

// ── Agent 任务回复 ──

// agentTaskGenerationInputResult 是 Agent 给生成任务的回复：直接调用普通模型，或先生成摘要。
type agentTaskGenerationInputResult struct {
	// Request 是 Context 为普通模型调用组装的请求；需要先摘要或准备出错时为 nil。
	Request *llmkit.Request
	// InputThroughItemID 是构建这次请求时上下文中最后一条 Item 的 ID；历史为空时没有 ID。
	InputThroughItemID defs.ItemID
	// IsCompacted 构建本次普通请求时上下文是否以摘要开头？随普通请求记入遥测，不是本次摘要结果。
	IsCompacted bool
	// Compaction 是需要先压缩历史时返回的摘要任务输入，此时 Request 为空。摘要完成后，生成任务再向 Agent 要普通请求。
	Compaction *compactionTaskInput
	// Err 是准备阶段发生的错误（不含模型调用失败）。
	Err error
}

// agentTaskModelCallResult 是 Agent 结算一次普通模型调用后返回给生成任务的结果。
type agentTaskModelCallResult struct {
	// ToolCalls 是本次成功模型调用产出的完整工具调用副本，供生成任务在工具阶段准备调用或在最后一步请求拒绝。
	ToolCalls []*defs.Item
	// LastAssistantMessage 是无工具调用时本次产出中的最后一条助手文本，由生成任务传给停止 hook。
	LastAssistantMessage string
	// ModelCallID 是本次普通模型调用的 ID，生成任务据此更新 lastModelCallID。
	ModelCallID defs.ModelCallID
	// Err 是取消、写入上下文、测量上下文或校验工具调用时发生的错误；非 nil 时生成任务不再重试并直接返回。
	Err error
}

// agentTaskCreationResult 是 Agent 响应创建工具任务或摘要任务请求时返回的创建结果。
type agentTaskCreationResult struct {
	// TaskIDs 是 Agent 分配的任务 ID 列表，生成任务将其作为 WaitingOn 返回。
	TaskIDs []agentTaskID
	// Err 是上下文已取消导致无法创建任务时返回的错误。
	Err error
}

// agentTaskFinalInputResult 是当前生成任务准备结束前向 Agent 读取补充输入后返回的结果，供当前生成任务决定是否请 Agent 创建下一步。
type agentTaskFinalInputResult struct {
	// HasInput 本次是否拿到补充输入？为 true 时当前生成任务请 Agent 创建下一步。
	HasInput bool
	// Err 是调用 readInputs 读取最终补充输入时返回的错误。
	Err error
}

// ── Agent 模型流合并 ──

// agentModelStream 保存还在接收事件的普通模型调用及其草稿起点。
type agentModelStream struct {
	// call 累计本次普通模型调用的标识、token 用量和上下文用量报告，在调用结束时通过 AgentEventModelCallFinished 发出。
	call AgentModelCall
	// inputThroughItemID 是构建这次请求时上下文中最后一条 Item 的 ID，收到 EventDone 时写入 TurnContextUsage 用于用量统计；历史为空时没有 ID。
	inputThroughItemID defs.ItemID
	// outputStart 是本次模型调用开始时 draftItems 的起始下标，结算时据此截取本轮尝试产出的草稿切片。
	outputStart int
}

// ── Agent 工具结果交接 ──

// agentToolCall 保存本批调用的草稿及等待 ToolTask 取回的最终执行结果。
type agentToolCall struct {
	// item 是本次模型调用产出的 ToolCall 草稿，用于关联进度事件的 ItemIndex 并为工具结果与 hook 记录提供 TurnID 和 ModelCallID。
	item *defs.Item
	// finished 是执行器发来的工具最终执行事件（写入上下文的是结果 Item 而非该事件），等待对应工具任务调用 takeToolResult 取回。
	finished *executor.ExecutionFinishedEvent
}

// agentTaskToolResult 是 Agent 响应工具任务取结果请求时返回的回复。
type agentTaskToolResult struct {
	// Finished 是 Agent 从 toolCallsByCallID 取出并交还 toolTask 的工具最终执行事件。
	Finished *executor.ExecutionFinishedEvent
	// Err 是 takeToolResult 取结果时仍无已合并结果返回的错误。
	Err error
}
