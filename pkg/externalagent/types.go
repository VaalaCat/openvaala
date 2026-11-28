package externalagent

import (
	"context"
	"errors"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// ── 外部驱动 请求 ──

// ExternalAgentOptionsRequest 指定探测位置、联动选项及刷新要求。
type ExternalAgentOptionsRequest struct {
	Workspace    workspace.Workspace
	ExecutorID   string
	Cwd          string
	Selections   []defs.ExternalOptionSelection
	ForceRefresh bool
}

// ExternalSessionOpenRequest 指定执行位置、恢复绑定和启动配置，Workspace 由装配方持有。
type ExternalSessionOpenRequest struct {
	Ephemeral      bool
	Workspace      workspace.Workspace
	ExecutorID     string
	ExecutorKind   defs.ExecutorKind
	Cwd            string
	Binding        *defs.ExternalSession
	InitialOptions []defs.ExternalOptionSelection
	LaunchEnv      map[string]string
	LocalMCPURL    string
	MCPServers     []*mcpclient.ServerDefinition
	// AttachmentDirectory 是装配方解析好的目标机对话附件绝对目录，含自定义本机数据目录。
	AttachmentDirectory string
}

// ── 外部驱动 查询结果与能力 ──

// ExternalSessionInfo 是可导入会话的列表条目，更新时间为 Unix 毫秒。
type ExternalSessionInfo struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updated_at"`
}

// ExternalSessionPreview 保存预览中首末两条用户消息的正文。
type ExternalSessionPreview struct {
	FirstUserMessage string `json:"first_user_message"`
	LastUserMessage  string `json:"last_user_message"`
}

// ── 外部驱动 会话能力 ──

// ExternalAgentCapabilities 由驱动握手后上报。
type ExternalAgentCapabilities = defs.ExternalCapabilities

// ── 外部驱动 事件 ──

// ExternalSessionEvent 由驱动发给包装层；事件及其载荷发送后只读。原生 Session 直接消费 Turn 的事件。
type ExternalSessionEvent interface{ isExternalSessionEvent() }

// ExternalSessionReadyEvent 由驱动在创建或完整回放后发出；原生 Run 自行从 DAO 恢复。
type ExternalSessionReadyEvent struct {
	Binding      defs.ExternalSession
	Capabilities ExternalAgentCapabilities
	Options      defs.ExternalOptions
	Commands     []defs.ExternalCommand
	MCPServers   []string
}

func (*ExternalSessionReadyEvent) isExternalSessionEvent() {}

// ExternalAgentStreamEvent 由驱动在后端流式更新时发出，沿用 SessionAgentEvent 载荷；回放额外包含用户消息。
// ItemIndex 在一轮输出内标识同一草稿；用户消息的多个分片沿用同一位置，由完成事件或下一项划定结尾。
type ExternalAgentStreamEvent struct {
	TurnID    defs.TurnID
	Replaying bool
	Event     *agent.AgentEvent
}

func (*ExternalAgentStreamEvent) isExternalSessionEvent() {}

// ExternalTurnFinishedEvent 由驱动在后端报告本轮结束时发出，包装层写成与原生一致的 SessionTurnFinished。
type ExternalTurnFinishedEvent struct {
	TurnID       defs.TurnID
	Status       defs.TurnStatus
	Usage        *defs.TokenUsage
	ContextUsage *defs.TurnContextUsage
	Err          error
}

func (*ExternalTurnFinishedEvent) isExternalSessionEvent() {}

// ExternalBindingUpdatedEvent 由驱动在收到新的恢复绑定时发出；原生无对应，历史由本机 Session ID 定位。
type ExternalBindingUpdatedEvent struct{ Binding defs.ExternalSession }

func (*ExternalBindingUpdatedEvent) isExternalSessionEvent() {}

// ExternalOptionsUpdatedEvent 由驱动在后端选择变化时发出；原生通过 SelectModel 直接接收选择。
type ExternalOptionsUpdatedEvent struct{ Options defs.ExternalOptions }

func (*ExternalOptionsUpdatedEvent) isExternalSessionEvent() {}

// ExternalCommandsUpdatedEvent 由驱动在后端更新命令表时发出；原生无对应，命令由宿主提供。
type ExternalCommandsUpdatedEvent struct{ Commands []defs.ExternalCommand }

func (*ExternalCommandsUpdatedEvent) isExternalSessionEvent() {}

// ── 外部 Session 错误 ──

// ErrExternalUnsupported 标识只能在原生会话执行的操作。
var ErrExternalUnsupported = errors.New("operation is not supported")

// ── 外部 Session 启动配置 ──

// ExternalSessionLaunch 由 Conversation 按同一份定义和目标 Executor 装配。
// Workspace 由装配方持有。ExecutorID、Cwd、InitialOptions 只用于首次新建。
type ExternalSessionLaunch struct {
	Ephemeral           bool
	Provider            ExternalAgentProvider
	Workspace           workspace.Workspace
	ExecutorID          string
	ExecutorKind        defs.ExecutorKind
	Cwd                 string
	InitialOptions      []defs.ExternalOptionSelection
	LaunchEnv           map[string]string
	LocalMCPURL         string
	MCPServers          []*mcpclient.ServerDefinition
	AttachmentDirectory string
	// SkillWorkspace 以目标机用户的 home 为根。专用工作区由运行实例在替换或退出时关闭；借用主 Workspace 时随对话关闭。
	SkillWorkspace workspace.Workspace
}

// ── 外部 Session 请求 ──

type sessionRequest interface{ isSessionRequest() }

// inputRequested 由调用方提交新输入时发出。
type inputRequested struct {
	input session.SessionInput
	reply chan sessionReply[defs.ItemID]
}

func (inputRequested) isSessionRequest() {}

// contextItemsRequested 由调用方追加上下文时发出，留给下一轮，不触发开轮。
type contextItemsRequested struct {
	items []*defs.Item
	reply chan error
}

func (contextItemsRequested) isSessionRequest() {}

// statusRequested 由调用方读取 Session 状态时发出。
type statusRequested struct {
	reply chan sessionReply[session.SessionStatus]
}

func (statusRequested) isSessionRequest() {}

// interruptRequested 由调用方请求取消活动轮时发出。
type interruptRequested struct {
	reply chan sessionReply[defs.TurnID]
}

func (interruptRequested) isSessionRequest() {}

// pendingInputRequested 由调用方选择队中用户输入进行插话或中断再发时发出。
type pendingInputRequested struct {
	turnID    defs.TurnID
	itemID    defs.ItemID
	interrupt bool
	reply     chan error
}

func (pendingInputRequested) isSessionRequest() {}

// withdrawRequested 由调用方请求撤回排队输入时发出。
type withdrawRequested struct {
	itemID defs.ItemID
	reply  chan sessionReply[[]*defs.Item]
}

func (withdrawRequested) isSessionRequest() {}

// unreadRequested 由调用方查询或等待未读补充时发出。
type unreadRequested struct {
	wait  bool
	reply chan sessionReply[[]*defs.Item]
}

func (unreadRequested) isSessionRequest() {}

// optionRequested 由调用方要求切换后端选项时发出。
type optionRequested struct {
	ctx       context.Context
	selection defs.ExternalOptionSelection
	reply     chan error
}

func (optionRequested) isSessionRequest() {}

// reloadRequested 由 Conversation 用最新定义重载会话时发出。
type reloadRequested struct {
	launch ExternalSessionLaunch
	reply  chan error
}

func (reloadRequested) isSessionRequest() {}

// ── 外部 Session 请求结果 ──

type sessionReply[T any] struct {
	value T
	err   error
}

// inputDelivered 由 StartTurn 或 SteerInput 的短任务在驱动确认或拒绝输入后发出。
type inputDelivered struct {
	turnID defs.TurnID
	err    error
}

func (inputDelivered) isSessionRequest() {}

// optionSet 由 SetOption 短任务在后端返回结果后发出。
type optionSet struct {
	reply chan error
	err   error
}

func (optionSet) isSessionRequest() {}

// interruptReturned 由 InterruptTurn 短任务在取消请求返回后发出，轮次终态仍由驱动报告。
type interruptReturned struct {
	turnID     defs.TurnID
	reply      chan sessionReply[defs.TurnID]
	inputReply chan error
	err        error
}

func (interruptReturned) isSessionRequest() {}

// ── 外部 Session 输入与输出 ──

// inputDelivery 保存已经借给一次驱动调用的输入，收到结果后结算归属。
type inputDelivery struct {
	opening  bool
	items    []*defs.Item
	groupIDs []defs.ItemID
	reply    chan error
}

// draftItem 对应驱动的一条输出；index 用于增量，工具调用另按 CallID 关联。
type draftItem struct {
	index    int
	item     *defs.Item
	complete bool
}
