package session

import (
	"errors"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// ── Session 错误 ──

// errSessionStopped 是 Session.Run 已返回、事件循环不再处理请求时 sendRequest 返回的错误。
var errSessionStopped = errors.New("session has stopped")

// ErrSessionBusy 是 Session 忙时拒绝请求的错误：idle_only 输入遇到正在执行的 Turn，或手动压缩遇到正在执行的 Turn、排队的用户输入。
var ErrSessionBusy = errors.New("session is busy")

// ── Session 输入参数 ──

// SessionInputMode 是一条输入的提交方式。Agent 发来的消息不插队，没挂上活动 Turn 的都排进 Agent 消息队列。
type SessionInputMode string

const (
	// SessionInputModeNormal 排队等新 Turn，空闲时立刻开始。
	SessionInputModeNormal SessionInputMode = "normal"
	// SessionInputModeIdleOnly 只在没有活动 Turn 时接收并立刻开始，忙时返回 ErrSessionBusy，不排队。
	SessionInputModeIdleOnly SessionInputMode = "idle_only"
	// SessionInputModeSteer 挂到还在接收补充输入的活动 Turn 上；没有这样的 Turn 时按 normal 排队。
	SessionInputModeSteer SessionInputMode = "steer"
	// SessionInputModeInterrupt 插队：取消活动 Turn，这条排到用户队列最前；空闲时和 normal 一样立刻开始。
	SessionInputModeInterrupt SessionInputMode = "interrupt"
)

// SessionInput 是交给 Session 的一次输入。
type SessionInput struct {
	Mode SessionInputMode
	// Item 是输入消息，ID 由调用方生成，还没分配 TurnID 和 ModelCallID。
	Item *defs.Item
	// InputID 可选：本次运行期内重复的 InputID 只接收一次，返回首次接收的 ItemID。
	InputID defs.InputID
}

// ── Session 查询状态 ──

// SessionState 是 Session 在查询时所处的阶段。
type SessionState string

const (
	// SessionStateIdle 没有活动 Turn。
	SessionStateIdle SessionState = "idle"
	// SessionStateRunning 有活动 Turn。
	SessionStateRunning SessionState = "running"
	// SessionStateStopping 已开始停止，在等活动 Turn 结束。
	SessionStateStopping SessionState = "stopping"
	// SessionStateStopped Session.Run 已返回，或 Session 没在运行。
	SessionStateStopped SessionState = "stopped"
)

// SessionStatus 是查询时 Session 的执行状态和最近 Turn。
type SessionStatus struct {
	State SessionState `json:"state"`
	// ActiveTurnID 是活动 Turn，没有时为空。
	ActiveTurnID defs.TurnID `json:"active_turn_id,omitempty"`
	// PendingInputCount 是排队等新 Turn 的输入条数，用户输入和 Agent 消息都算。
	PendingInputCount int `json:"pending_input_count"`
	// LastTurnID 是最近一个 Turn。
	LastTurnID defs.TurnID `json:"last_turn_id,omitempty"`
	// LastTurnStatus 是最近一个 Turn 保存的终态，终态没保存时为空。
	LastTurnStatus defs.TurnStatus `json:"last_turn_status,omitempty"`
}

// PendingInput 是尚未交给 Turn 的一条用户输入的轻量展示内容。
type PendingInput struct {
	ItemID     defs.ItemID `json:"item_id"`
	Preview    string      `json:"preview"`
	ImageCount int         `json:"image_count"`
	IsSteer    bool        `json:"is_steer"`
}

// SessionPendingInputs 是待处理用户输入及仍接收补充输入的轮。
type SessionPendingInputs struct {
	SteerTurnID defs.TurnID    `json:"steer_turn_id,omitempty"`
	Inputs      []PendingInput `json:"inputs"`
}

// ── Session 对外事件 ──

// SessionEvent 是 Session.Run 的 events 里的事件，由各 Turn 执行任务发出。
type SessionEvent interface{ isSessionEvent() }

// SessionPendingInputsUpdated 由 Session 在输入进入、移动、取走或补充接收状态变化后发出。
type SessionPendingInputsUpdated struct {
	PendingInputs SessionPendingInputs
}

func (*SessionPendingInputsUpdated) isSessionEvent() {}

// SessionAgentEvent 由 Turn 执行任务在收到 TurnAgentEvent 后转发，携带对应的 Agent 事件。
type SessionAgentEvent struct {
	TurnID defs.TurnID
	Event  *agent.AgentEvent
}

func (*SessionAgentEvent) isSessionEvent() {}

// SessionTurnStarted 由 Turn 执行任务在调用 Turn.Run 之前发出；同一 Turn 的其他事件都在它之后。
type SessionTurnStarted struct {
	TurnID defs.TurnID
	// Model 是本 Turn 使用的模型和推理强度。
	Model defs.ModelSelection
}

func (*SessionTurnStarted) isSessionEvent() {}

// SessionItemsSaved 由 Turn 执行任务在 Turn 写入一批 Item 后转发；Items 按序号递增，
// Agent 输出 Item 的保存事件总在它的终态 SessionAgentEvent 之后。
type SessionItemsSaved struct {
	TurnID defs.TurnID
	Items  []dao.LoggedItem
}

func (*SessionItemsSaved) isSessionEvent() {}

// SessionHookRunEvent 由 Turn 执行任务转发 Turn 自己触发的 hook（SessionStart、SubagentStart、UserPromptSubmit）的运行记录：
// 每次运行开始（running）和结束（最终状态）各发一次，同一次运行的 ID 不变。Agent 触发的 hook 运行经 SessionAgentEvent 到达。
type SessionHookRunEvent struct {
	TurnID defs.TurnID
	Run    *defs.HookRun
}

func (*SessionHookRunEvent) isSessionEvent() {}

// SessionTurnFinished 由 Turn 执行任务在 Turn.Run 返回（终态已尽力写库）后发出；正常结束、失败、被中断都会发。
type SessionTurnFinished struct {
	TurnID defs.TurnID
	// InputItemIDs 是本轮读入的全部输入，包括补充输入。
	InputItemIDs []defs.ItemID
	// Status 是本轮的终态，只在终态已保存时填写。
	Status defs.TurnStatus
	// Outputs 是本轮的输出 Item。
	Outputs []*defs.Item
	// FinalModelCallID 是最后一次模型调用的 ID，只在 completed 时填写。
	FinalModelCallID defs.ModelCallID
	// Err 是 Turn.Run 返回的错误。
	Err error
}

func (*SessionTurnFinished) isSessionEvent() {}

// ── Session 收件箱 ──

// inboxInput 保存等待新 Turn 的输入组和提交时的追踪信息。
type inboxInput struct {
	// groups 每组是一条消息及它选中的 skill 说明：第一组作为 Turn 的输入，其余各组开 Turn 时挂成补充输入，
	// 在第一次请求模型前一起读入。例如中断后没读到的两条用户 steer 是一批，一起开一轮。
	groups [][]*defs.Item
	// parentSpan 是这一批第一条输入提交时 ctx 里的 span，开 Turn 时作为 invoke_agent span 的父 span。
	// 放回队列的补充输入没有记下 span，为零值；Agent 消息合批时取队首一批的。
	parentSpan telemetry.SpanRef
}

// ── Session 事件循环请求 ──

// sessionRequest 是 Session 各方法交给事件循环处理的请求。
type sessionRequest interface{ isSessionRequest() }

// inputRequested 由 HandleInput 在输入通过校验后交给事件循环。
type inputRequested struct {
	item    *defs.Item
	mode    SessionInputMode
	inputID defs.InputID
	// parentSpan 是 HandleInput 调用方 ctx 里的 span。
	parentSpan telemetry.SpanRef
	reply      chan inputAccepted
}

func (inputRequested) isSessionRequest() {}

// inputAccepted 是事件循环对一次输入的回复：itemID 是实际接收的输入，重复 InputID 时是首次接收的 ItemID。
type inputAccepted struct {
	itemID defs.ItemID
	err    error
}

// steerInputRequested 由 SteerInput 请求将排队用户输入移入指定轮。
type steerInputRequested struct {
	itemID defs.ItemID
	turnID defs.TurnID
	reply  chan error
}

func (steerInputRequested) isSessionRequest() {}

// interruptTurnWithInputRequested 由 InterruptTurnWithInput 请求置顶排队用户输入并取消指定轮。
type interruptTurnWithInputRequested struct {
	itemID defs.ItemID
	turnID defs.TurnID
	reply  chan error
}

func (interruptTurnWithInputRequested) isSessionRequest() {}

// withdrawInputRequested 由 WithdrawInput 请求取走待处理用户输入组。
type withdrawInputRequested struct {
	itemID defs.ItemID
	reply  chan inputWithdrawn
}

func (withdrawInputRequested) isSessionRequest() {}

// inputWithdrawn 是一次原子取组的结果。
type inputWithdrawn struct {
	items []*defs.Item
	err   error
}

// steerTakeRequested 由活动 Turn 通过 TakeSteer 取补充输入时交给事件循环。
type steerTakeRequested struct {
	turnID defs.TurnID
	// finish 是否在没有补充输入时停止接收？停止后 steer 输入改为排队。
	finish bool
	reply  chan [][]*defs.Item
}

func (steerTakeRequested) isSessionRequest() {}

// steerWaitRequested 由 WaitSteer 在等活动 Turn 挂上补充输入时交给事件循环。
type steerWaitRequested struct{ reply chan []*defs.Item }

func (steerWaitRequested) isSessionRequest() {}

// statusRequested 由 GetStatus 在查询执行状态时交给事件循环。
type statusRequested struct{ reply chan SessionStatus }

func (statusRequested) isSessionRequest() {}

// interruptRequested 由 InterruptTurn 在请求取消活动 Turn 时交给事件循环。
type interruptRequested struct{ reply chan defs.TurnID }

func (interruptRequested) isSessionRequest() {}

// compactRequested 由 CompactContext 在请求手动压缩上下文时交给事件循环。
type compactRequested struct{ reply chan compactAccepted }

func (compactRequested) isSessionRequest() {}

// compactAccepted 是事件循环对一次手动压缩请求的回复：turnID 是已启动的只压缩 Turn；Session 忙、已开始停止或模型不在目录里时 err 非 nil。
type compactAccepted struct {
	turnID defs.TurnID
	err    error
}

// modelSelectionRequested 由 SelectModel 在替换下一个 Turn 的模型时交给事件循环。
type modelSelectionRequested struct{ model defs.ModelSelection }

func (modelSelectionRequested) isSessionRequest() {}

// ── Session Turn 执行结果 ──

// turnFinished 由 Turn 执行任务在发出 SessionTurnFinished 后交回事件循环。
type turnFinished struct {
	turnID defs.TurnID
	status defs.TurnStatus
	// contextUsage 是最近一次完整普通调用的报告；本轮没有完整普通调用时沿用上一轮的报告，模型切换后为空。
	contextUsage *defs.TurnContextUsage
	err          error
}
