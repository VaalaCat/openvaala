package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/terminal"
)

// ── Runtime 错误 ──

// ErrRuntimeStopped 是 Runtime.Run 返回后，需要经事件循环处理的调用返回的错误。
var ErrRuntimeStopped = errors.New("runtime has stopped")

// ErrConversationNotOpen 是对话没有运行实例时，需要已打开对话的方法返回的错误；调用方先 OpenConversation。
var ErrConversationNotOpen = errors.New("conversation is not open")

// ErrConversationNotFound 是找不到对话记录时返回的错误。
var ErrConversationNotFound = errors.New("conversation not found")

// ErrSessionNotFound 是按 SessionID 找不到 Session 时返回的错误。
var ErrSessionNotFound = errors.New("session not found")

// ErrSubscriberTooSlow 表示订阅者的缓冲已满，订阅被关闭；调用方用最后看到的位置重新订阅。
var ErrSubscriberTooSlow = errors.New("subscriber is too slow")

// ErrNoModelConfigured 是没有任何模型时新建对话返回的错误。
var ErrNoModelConfigured = errors.New("no model is configured; run openvaala to set up a model provider")

// errConversationAlreadyOpen 是登记新装配的实例时，同一对话已有运行实例的错误；回复里同时带上运行中的那个实例。
var errConversationAlreadyOpen = errors.New("conversation is already open")

// ErrConversationReconfigured 是对话原地换上新配置（改 Executor 设置或 reload）后，关闭它全部订阅时的原因；
// 客户端收到后重新订阅、增量同步。
var ErrConversationReconfigured = errors.New("conversation reconfigured")

// ── Runtime 请求参数 ──

// CreateConversationRequest 是新建对话的参数：用哪个 Agent、什么模型、哪些 Executor 和环境变量；对话建好后立即启动。
type CreateConversationRequest struct {
	// ConversationID 是调用方指定的新对话 ID；为空时由 Runtime 生成。不检查是否重复。
	ConversationID defs.ConversationID
	// AgentName 是根 Session 使用的 Agent 名。
	AgentName string
	// Model 是根 Session 的模型选择；零值时使用根 Agent 的默认模型。
	Model defs.ModelSelection
	// ExecutorSettings 补上缺的 local 目录后存进记录；新加入的未知 Executor 返回 conversation.ErrInvalidExecutorSettings。
	ExecutorSettings defs.ExecutorSettings
	// Environment 是客户端的环境变量（格式同 os.Environ），只给 local 种类的 Executor 执行命令用；nil 时继承 serve 进程的环境。
	Environment []string
	// Ephemeral 为 true 时建临时对话：存储在一个只属于它的 sqlite 内存库里，永远不进对话列表；
	// 没有订阅者、也没有 Session 在跑，满 UnsavedConversationIdleTimeout 后结束并丢弃，之后按 ID 读回「对话不存在」。
	Ephemeral bool
	// WithoutExecutors 为 true 时不装配 Executor 和 Skill，ExecutorSettings 原样存进记录、不生效，Environment 忽略。
	WithoutExecutors bool
}

// OpenConversationRequest 是打开已保存对话的参数：对话没在运行时按记录里的 Executor 设置和 Environment 装配；已打开时忽略 Environment。
type OpenConversationRequest struct {
	ConversationID defs.ConversationID
	// Environment 和 CreateConversationRequest.Environment 相同。
	Environment []string
}

// SetConversationExecutorSettingsRequest 是对话新的完整 Executor 设置。
type SetConversationExecutorSettingsRequest struct {
	ConversationID   defs.ConversationID
	ExecutorSettings defs.ExecutorSettings
}

// SetClientMCPServersRequest 是一条客户端连接为一个对话登记的全部 MCP server，换掉它之前为这个对话登记的；Servers 为空表示全部移除。
type SetClientMCPServersRequest struct {
	ConversationID defs.ConversationID
	// ClientID 是 serve 上这条客户端连接的 ID。
	ClientID string
	Servers  []conversation.ClientMCPServer
}

// Definitions 是从配置加载出的整份定义，reload 时整份换掉；构造后只读。
type Definitions struct {
	Agents []*agent.AgentDefinition
	Models []*defs.ModelDefinition
	// MCPServers 是全部 MCP server 定义，含插件带来的。
	MCPServers []*mcpclient.ServerDefinition
	// Hooks 是全部 hook 定义（含插件带来的），已按配置顺序排列；为空时对话也有一个不含任何 hook 的 hook.Hooks。
	Hooks []*hook.HookDefinition
	// Executors 决定每个对话能用哪些 Executor，reload 时和其他定义一起替换；nil 时没有 Executor。
	Executors executor.ExecutorProvider
	// Skills 是所有对话共用的 skill：skills.directory、builtin、各插件，按这个顺序去重。
	Skills []*skill.Skill
}

// ListModelsRequest 查询模型目录；AgentName 为空时不返回 Agent 默认模型。
type ListModelsRequest struct {
	AgentName string
}

// GetSkillsRequest 指定查哪里的 Skill：有 ConversationID 时查这个已打开对话；还没有对话时按 AgentName 和 ExecutorSettings 查草稿。
type GetSkillsRequest struct {
	ConversationID defs.ConversationID
	AgentName      string
	// ExecutorSettings 只在没有 ConversationID 时用：草稿对话按它找默认 Executor 扫项目目录和主目录。
	ExecutorSettings defs.ExecutorSettings
}

// ResolveWorkspaceDirectoryRequest 是 /cd 的输入。
type ResolveWorkspaceDirectoryRequest struct {
	// ExecutorID 是要在哪个 Executor 上解析目录。
	ExecutorID string
	// Directory 是用户写的目录。
	Directory string
	// BaseDirectory 是当前目录；为空时用这个 Executor 的默认根目录。
	BaseDirectory string
}

// OpenTerminalRequest 指定终端的 Executor、起始目录、客户端环境变量、初始尺寸和所属对话。
type OpenTerminalRequest struct {
	// ExecutorID 为空时用 local Executor。
	ExecutorID string
	// Directory 为空时用该 Executor 的默认工作目录；否则按 ResolveWorkspaceDirectory 的规则解析并检查它是目录。
	Directory string
	// Environment 的含义同 CreateConversationRequest；只给 local 种类的 Executor 用。
	Environment []string
	// Rows、Cols 是 PTY 初始行列数，为零时按 24×80。
	Rows, Cols uint16
	// ConversationID 为空时只有原生客户端的 Dock 显示这个终端。
	ConversationID defs.ConversationID
}

// GetWorkspaceDirectoriesRequest 指定 Executor、待列出的目录和相对路径基准。
type GetWorkspaceDirectoriesRequest struct {
	ExecutorID    string
	Directory     string
	BaseDirectory string
}

// SetSessionModelRequest 是给一个 Session 换模型的参数；推理强度为空时沿用原来的强度（新模型不支持就用模型默认值）。
type SetSessionModelRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	Model          defs.ModelSelection
}

// GetConversationRequest 指定一个对话：读对话记录、查对话是否打开、查 MCP 连接都用它。
type GetConversationRequest struct {
	ConversationID defs.ConversationID
}

// GetSessionByIDRequest 是只凭 SessionID 读 Session 记录的请求。
type GetSessionByIDRequest struct {
	SessionID defs.SessionID
}

// SetConversationTitleRequest 是新的对话标题；只保存前 256 个 Unicode 字符，对话没打开时直接写库。
type SetConversationTitleRequest struct {
	ConversationID defs.ConversationID
	Title          string
}

// ForkConversationRequest 把已保存的对话根 Session 复制到某一轮为止，成为一个新对话并打开；还在运行的轮次只复制已保存的部分。
type ForkConversationRequest struct {
	ConversationID defs.ConversationID
	// ForkedConversationID 是调用方指定的分叉出的新对话 ID；为空时由 Runtime 生成。不检查是否重复。
	ForkedConversationID defs.ConversationID
	// BeforeTurnID 复制到这一轮之前（不含），回退重来用；LastTurnID 复制到这一轮（含）。
	// 两个最多填一个；都不填时复制到最后一条已保存的 Item，包括还在运行的轮次已保存的部分。
	BeforeTurnID defs.TurnID
	LastTurnID   defs.TurnID
	// Model 是新根 Session 的模型；零值时照抄来源根 Session 的模型。
	Model defs.ModelSelection
	// ExecutorSettings、Environment 和 CreateConversationRequest 一样；调用方一般传来源对话的设置。来源记录里就有、
	// 配置中已不存在的 Executor 只记 Warn。
	ExecutorSettings defs.ExecutorSettings
	Environment      []string
	// Ephemeral 为 true 时，分叉存进只属于它的 sqlite 内存库，不进对话列表；
	// 没有订阅者、也没有 Session 在跑，满 UnsavedConversationIdleTimeout 后结束并丢弃。
	Ephemeral bool
}

// SetConversationArchivedRequest 归档（Archived 为 true）或取消归档一个已保存的对话。
type SetConversationArchivedRequest struct {
	ConversationID defs.ConversationID
	Archived       bool
}

// DeleteConversationRequest 删除一个已保存的对话、它的全部 Session 和历史。
type DeleteConversationRequest struct {
	ConversationID defs.ConversationID
}

// SubmitInputRequest 是交给对话根 Session 的一次输入。
type SubmitInputRequest struct {
	ConversationID defs.ConversationID
	// InputID 可选：根 Session 本次运行期内重复的 InputID 只接收一次，返回首次的 ItemID。
	InputID defs.InputID
	Mode    session.SessionInputMode
	// From 必须是用户或 Agent 身份。
	From    *defs.Address
	Content []*defs.Content
}

// InterruptTurnRequest 请求取消对话根 Session 的活动 Turn。
type InterruptTurnRequest struct {
	ConversationID defs.ConversationID
}

// SteerInputRequest 指定要移入根 Session 当前轮的排队用户输入。
type SteerInputRequest struct {
	ConversationID defs.ConversationID
	ItemID         defs.ItemID
	TurnID         defs.TurnID
}

// InterruptTurnWithInputRequest 指定优先用户输入和要取消的根 Session 轮。
type InterruptTurnWithInputRequest struct {
	ConversationID defs.ConversationID
	ItemID         defs.ItemID
	TurnID         defs.TurnID
}

// WithdrawInputRequest 指定要原子取走的根 Session 用户输入。
type WithdrawInputRequest struct {
	ConversationID defs.ConversationID
	ItemID         defs.ItemID
}

// AppendContextItemsRequest 指定要注入根 Session 下一轮上下文的 Item。
type AppendContextItemsRequest struct {
	ConversationID defs.ConversationID
	// Items 可以是任何不需要模型调用、也不由运行时自己写入的种类，载荷必须有效；ID 由调用方生成，还没分配 TurnID 和 ModelCallID。
	Items []*defs.Item
}

// CompactContextRequest 选择要手动压缩根 Session 上下文的对话。
type CompactContextRequest struct {
	ConversationID defs.ConversationID
}

// ListItemsRequest 分页读取对话内一个 Session 的日志；分页字段与 dao.ListItemsRequest 相同。
type ListItemsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	Order          storage.Direction
	Cursor         storage.Ordinal
	Limit          int
}

// ListTurnsRequest 分页读取一个 Session 的 Turn 记录；ConversationID 用来选这个对话的存储，分页字段与 dao.ListTurnsRequest 相同。
type ListTurnsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	Order          storage.Order
	Cursor         dao.RecordCursor[defs.TurnID]
	Limit          int
}

// GetTurnsRequest 按 ID 取一个 Session 的多条 Turn 记录；ConversationID 用来选这个对话的存储。
type GetTurnsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	TurnIDs        []defs.TurnID
}

// SubscribeConversationRequest 订阅对话标题和全部 Session 的运行信息。
type SubscribeConversationRequest struct {
	ConversationID defs.ConversationID
}

// SubscribeSessionRequest 订阅对话内一个 Session 的内容。
type SubscribeSessionRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
}

// GetSessionContextUsageRequest 读取对话内一个 Session 的上下文占用详情。
type GetSessionContextUsageRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
}

// GetSessionDetailsRequest 读取对话内一个 Session 的静态配置。
type GetSessionDetailsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
}

// ── Runtime 事件循环请求 ──

// runtimeRequest 是交给 Runtime.Run 事件循环处理的请求集合。
type runtimeRequest interface{ isRuntimeRequest() }

// runningConversationRequested 由需要已打开对话的公开方法触发：事件循环查运行表，回复运行中的实例，没打开时回复 ErrConversationNotOpen。
type runningConversationRequested struct {
	conversationID defs.ConversationID
	reply          chan conversationReply
}

func (*runningConversationRequested) isRuntimeRequest() {}

// conversationStartRequested 由新建、打开、未打开时改设置或分叉在装配好实例后发出，请求 Runtime 登记并启动。
type conversationStartRequested struct {
	instance conversation.Conversation
	// unsavedStore 是没保存的对话的内存库；从共用库打开时为 nil。
	unsavedStore *unsavedConversationStore
	// executorSettings 非 nil 时，Runtime 登记前先把它写进对话记录。
	executorSettings *defs.ExecutorSettings
	reply            chan conversationReply
}

func (*conversationStartRequested) isRuntimeRequest() {}

// conversationReply 是事件循环对 runningConversationRequested、conversationStartRequested 和 conversationArchiveChangeRequested 的回复。
type conversationReply struct {
	// runningConversation 是运行中的实例：查找成功、登记成功，或登记时同一对话已有实例（err 为 errConversationAlreadyOpen）时有值。
	runningConversation *runtimeConversation
	// record 是登记后的对话记录（标题已截断），或归档后保存的记录。
	record defs.Conversation
	err    error
}

// conversationStopped 由对话的运行任务在对话退出后触发，请求移除运行实例。
type conversationStopped struct {
	runningConversation *runtimeConversation
}

func (*conversationStopped) isRuntimeRequest() {}

// conversationTitleChangeRequested 由 SetConversationTitle 在显式改名时发出；已打开对话交给对话保存，其余由 Runtime 直接保存。
type conversationTitleChangeRequested struct {
	ctx     context.Context
	request *SetConversationTitleRequest
	reply   chan error
}

func (*conversationTitleChangeRequested) isRuntimeRequest() {}

// conversationArchiveChangeRequested 由 SetConversationArchived 在归档或取消归档时发出；已打开对话交给对话保存，其余由 Runtime 直接保存。
type conversationArchiveChangeRequested struct {
	ctx     context.Context
	request *SetConversationArchivedRequest
	reply   chan conversationReply
}

func (*conversationArchiveChangeRequested) isRuntimeRequest() {}

// conversationDeleteRequested 由 DeleteConversation 触发：事件循环确认对话已保存，有 Session 在执行 Turn 时回复 session.ErrSessionBusy；
// 否则停掉运行实例，再删除它的全部记录和日志，回复删除结果。
type conversationDeleteRequested struct {
	request *DeleteConversationRequest
	reply   chan error
}

func (*conversationDeleteRequested) isRuntimeRequest() {}

// runningConversationsRequested 由 IsIdle 触发：事件循环把运行中或正在退出的对话复制一份回复给调用方。
type runningConversationsRequested struct {
	reply chan []*runtimeConversation
}

func (*runningConversationsRequested) isRuntimeRequest() {}

// definitionsChangeRequested 由 SetDefinitions 触发：事件循环换上新定义，回复运行表里全部对话（含正在退出的）的列表，
// 由 SetDefinitions 逐个更新对话。
type definitionsChangeRequested struct {
	definitions *runtimeDefinitions
	reply       chan []*runtimeConversation
}

func (*definitionsChangeRequested) isRuntimeRequest() {}

// runningSubscribeRequested 由全局运行订阅触发，Runtime 登记订阅并读取初始集合。
type runningSubscribeRequested struct {
	ctx   context.Context
	reply chan runningSubscribeReply
}

func (*runningSubscribeRequested) isRuntimeRequest() {}

// runningSubscribeReply 是 Runtime 返回的全局运行订阅或初始读取错误。
type runningSubscribeReply struct {
	subscription *RunningConversationsSubscription
	err          error
}

// runningUnsubscribeRequested 在订阅上下文结束时触发，Runtime 移除并关闭对应流。
type runningUnsubscribeRequested struct {
	stream *subscriptionStream[RunningConversations]
	reason error
}

func (*runningUnsubscribeRequested) isRuntimeRequest() {}

// terminalOpenRequested 由 OpenTerminal 在工作区建好后发出：事件循环在 r.tasks 里启动 shell，登记所属对话并唤醒订阅，再回复启动结果。
type terminalOpenRequested struct {
	// ctx 是 OpenTerminal 调用方的 ctx，启动 shell 时去掉取消。
	ctx context.Context
	// request 带着已建好的独占工作区，StartTerminal 之后工作区归 pkg/terminal 关闭。
	request        *terminal.TerminalRequest
	conversationID defs.ConversationID
	reply          chan terminalOpenReply
}

func (*terminalOpenRequested) isRuntimeRequest() {}

// terminalOpenReply 是事件循环对 terminalOpenRequested 的回复：已启动登记的终端，或启动错误。
type terminalOpenReply struct {
	opened *OpenedTerminal
	err    error
}

// terminalsSubscribeRequested 由 SubscribeTerminals 触发，Runtime 登记订阅并回复同一时刻的运行中终端。
type terminalsSubscribeRequested struct {
	ctx   context.Context
	reply chan *TerminalsSubscription
}

func (*terminalsSubscribeRequested) isRuntimeRequest() {}

// terminalsUnsubscribeRequested 在终端订阅上下文结束时触发，Runtime 移除并关闭对应流。
type terminalsUnsubscribeRequested struct {
	stream *subscriptionStream[[]RunningTerminal]
	reason error
}

func (*terminalsUnsubscribeRequested) isRuntimeRequest() {}

// ── Runtime 运行状态 ──

// runtimeDefinitions 是 Runtime 当前的定义和两个索引，构造后只读，reload 时整份替换。
type runtimeDefinitions struct {
	// conversationDefinitions 是交给对话的整份定义（Agent、模型目录、MCP server、hook、Executor、共用 skill），建一次、各对话共用；
	// 对话只给其中至少一个 Agent 会用到的 MCP server 建连接；没有 hook 定义时 Hooks 也不为 nil。
	conversationDefinitions *conversation.ConversationDefinitions
	// modelsByRef 是按引用索引的模型目录；runtimeConversation 用它算 Session 下一次请求的上下文窗口。
	modelsByRef map[defs.ModelRef]*defs.ModelDefinition
	// agentDefaultModels 是 Agent 名到该 Agent 默认模型的索引；ListModels 用它返回默认模型，Session 没选模型时也用它找模型。
	agentDefaultModels map[string]defs.ModelRef
}

// unsavedConversationStore 是一个没保存的对话的内存库和用它构造的 DAO。
type unsavedConversationStore struct {
	database storagesqlite.Database
	query    dao.Query
	mutation dao.Mutation
	// stagedLogs、stagedRecords 为 nil 时是临时对话：整个对话都在内存库里，永远不保存。
	// 不为 nil 时是普通新对话：metadata Stream 和 sessions、conversations 索引暂存在内存库，其余直接写共用库；
	// 第一条输入或手动压缩时由 saveConversation 搬进共用库。
	stagedLogs    *storage.StagedLogStorage
	stagedRecords *storage.StagedRecordStorage
}

// ── Runtime 订阅对象 ──

// RunningConversationsSubscription 返回登记时的运行集合及之后的完整集合更新。
type RunningConversationsSubscription struct {
	Initial RunningConversations
	Events  <-chan RunningConversations
	stream  *subscriptionStream[RunningConversations]
}

// GetCloseReason 返回全局运行订阅关闭的原因。
func (s *RunningConversationsSubscription) GetCloseReason() error {
	// 1. 返回事件循环关闭通道前写入的原因。
	return s.stream.closeReason
}

// TerminalsSubscription 返回登记时的运行中终端及之后的完整列表更新。
type TerminalsSubscription struct {
	// Initial 和 Events 里的列表按启动先后排序，由全部订阅者共享，只读。
	Initial []RunningTerminal
	Events  <-chan []RunningTerminal
	stream  *subscriptionStream[[]RunningTerminal]
}

// GetCloseReason 返回终端订阅关闭的原因：订阅方 ctx 结束、ErrSubscriberTooSlow 或 ErrRuntimeStopped。
func (s *TerminalsSubscription) GetCloseReason() error {
	// 1. 返回事件循环关闭通道前写入的原因。
	return s.stream.closeReason
}

// subscriptionBufferSize 是每个订阅的事件缓冲；写满时订阅被关闭，原因为 ErrSubscriberTooSlow。
const subscriptionBufferSize = 256

// SessionSubscription 是一个 Session 的内容订阅。
type SessionSubscription struct {
	// LastSavedOrdinal 是订阅时已转发的最后一条已保存日志的流水号；它及之前的日志用 ListItems 补齐。
	LastSavedOrdinal storage.Ordinal
	// StreamingItems 是订阅时该 Session 活动 Turn 中已开始、尚未保存的 Item，按首次出现顺序，带 ItemIndex：
	// 活动 Turn 中可能同时有多个 Item 在生成（例如推理与消息），后续增量事件只用 ItemIndex 定位所属 Item。
	StreamingItems []StreamingItem
	// PendingInputs 是订阅时待处理用户输入及可接收补充的轮。
	PendingInputs session.SessionPendingInputs
	// Events 紧接着订阅时的初始信息开始推送；事件对象由全部订阅者共享，只读。
	Events <-chan conversation.ConversationEvent
	stream *subscriptionStream[conversation.ConversationEvent]
}

// GetCloseReason 返回 Events 关闭的原因：订阅方 ctx 结束、ErrSubscriberTooSlow、ErrConversationReconfigured，或对话退出（含 Runtime 停止）。
func (s *SessionSubscription) GetCloseReason() error {
	// 1. 返回事件循环关闭通道前写入的原因。
	return s.stream.closeReason
}

// ConversationSubscription 是对话标题与运行信息订阅。
type ConversationSubscription struct {
	// Title 是订阅时已保存的标题。
	Title string
	// Sessions 是订阅时全部 Session 的运行信息，按 SessionID 排序。
	Sessions []SessionRuntimeInfo
	// Events 按顺序推送之后的运行信息变化和已保存标题的变化。
	Events <-chan ConversationEvent
	stream *subscriptionStream[ConversationEvent]
}

// GetCloseReason 返回 Events 关闭的原因，和 SessionSubscription.GetCloseReason 相同。
func (s *ConversationSubscription) GetCloseReason() error {
	// 1. 返回事件循环关闭通道前写入的原因。
	return s.stream.closeReason
}

// StreamingItem 是一个生成中的 Item 及它在本轮 Agent 输出中的位置，与 AgentEvent.ItemIndex 对应。
type StreamingItem struct {
	ItemIndex int
	Item      *defs.Item
}

// subscriptionStream 是一个订阅的事件通道与关闭原因；只由所属事件循环写入和关闭，原因在关闭通道前写入。
type subscriptionStream[T any] struct {
	events      chan T
	closeReason error
}

// ── Runtime 对话事件 ──

// ConversationEvent 是对话订阅推送的事件：ConversationTitleChanged 或 SessionRuntimeInfo。
type ConversationEvent interface{ isRuntimeConversationEvent() }

// ConversationTitleChanged 由对话事件循环在新标题保存成功后发给对话订阅者；显式改名和首条用户输入自动命名都会发，标题没变时不发。
type ConversationTitleChanged struct{ Title string }

func (ConversationTitleChanged) isRuntimeConversationEvent() {}
func (SessionRuntimeInfo) isRuntimeConversationEvent()       {}

// ── Runtime 运行信息 ──

// RunningConversations 列出至少有一个 Session 正在执行 Turn 的对话。
type RunningConversations struct {
	ConversationIDs []defs.ConversationID `json:"conversation_ids"`
}

// OpenedTerminal 是 OpenTerminal 的结果。
type OpenedTerminal struct {
	Terminal terminal.Terminal
	// ExecutorID 是实际使用的 Executor。
	ExecutorID string
	// Directory 是解析后的绝对起始目录。
	Directory string
}

// RunningTerminal 是一个运行中的终端和打开它的对话。
type RunningTerminal struct {
	Info terminal.TerminalInfo
	// ConversationID 为空表示打开终端时没有指定对话。
	ConversationID defs.ConversationID
}

// SessionRuntimeInfo 是一个 Session 的运行信息，可直接用 == 比较；runtimeConversation 在它有变化时发给对话订阅者，
// 例如 Session 启停、Turn 开始或结束、用量和上下文占用变化、换模型。
type SessionRuntimeInfo struct {
	Session        defs.Session         `json:"session"`
	State          session.SessionState `json:"state"`
	ActiveTurnID   defs.TurnID          `json:"active_turn_id,omitempty"`
	LastTurnID     defs.TurnID          `json:"last_turn_id,omitempty"`
	LastTurnStatus defs.TurnStatus      `json:"last_turn_status,omitempty"`
	// ActiveTurnModel 是活动 Turn 使用的模型和推理强度；没有活动 Turn 时为零值。所选模型见 Session.Model。
	ActiveTurnModel defs.ModelSelection `json:"active_turn_model,omitzero"`
	// Context 是下一次请求的预计占用。
	Context ContextUsage `json:"context"`
	// Usage 是累计用量：对话加载时已保存 Turn 的用量之和，加上之后各次模型调用上报的用量。
	Usage defs.TokenUsage `json:"usage"`
}

// ContextUsage 是下一次请求的预计占用。
type ContextUsage struct {
	// Window 是模型上下文窗口；0 表示窗口未知。
	Window int `json:"window"`
	// Tokens 是预计的输入 token 数。
	Tokens int `json:"tokens"`
	// Source 是 Tokens 的来源：实际基准加估算、整份估算，或还没有测量。
	Source ContextUsageSource `json:"source"`
}

// ContextUsageSource 是预计占用的来源。
type ContextUsageSource string

const (
	// ContextUsageSourceActual 是基于上次实际用量、结合内容变化估算的统一测量。
	ContextUsageSourceActual ContextUsageSource = "actual"
	// ContextUsageSourceEstimated 整份都是估算：还没有实际基准，或基准已作废。
	ContextUsageSourceEstimated ContextUsageSource = "estimated"
	// ContextUsageSourceUnknown 还没有测量。
	ContextUsageSourceUnknown ContextUsageSource = "unknown"
)

// LastRequestUsage 是最近一次上报了用量的普通模型调用。
type LastRequestUsage struct {
	ModelCallID defs.ModelCallID `json:"model_call_id"`
	// Model 是这次普通调用使用的模型。
	Model defs.ModelRef `json:"model"`
	// Usage 是服务商上报的用量。
	Usage defs.TokenUsage `json:"usage"`
	// EstimatedTokens 是调用前未经实际用量校正的本地估算。
	EstimatedTokens int `json:"estimated_tokens"`
}

// SessionContextUsage 是一个 Session 的上下文占用详情，由已打开对话的 Session 记录、测量和模型调用信息整理。
type SessionContextUsage struct {
	// Window 是下一次请求所用模型的上下文窗口；0 表示窗口未知。
	Window int `json:"window"`
	// ThresholdTokens 是自动压缩阈值，按下一模型的窗口和最大输出计算；还没有测量时为 0。
	ThresholdTokens int `json:"threshold_tokens"`
	// NextRequest 与同一时刻的 SessionRuntimeInfo.Context 相同。
	NextRequest ContextUsage `json:"next_request"`
	// LastRequest 在还没有上报过用量的普通调用时为 nil。
	LastRequest *LastRequestUsage `json:"last_request,omitempty"`
	// SystemTokens、ToolTokens 是最近一次测量中系统提示词和工具定义的占用；还没有测量时为 0。
	SystemTokens int `json:"system_tokens"`
	ToolTokens   int `json:"tool_tokens"`
	// Items 是本轮最近一次统一测量的 Item 分类估算；还没有测量时为空。
	Items []agent_context.ItemTokens `json:"items"`
}

// ── Runtime 终端常量 ──

const (
	// terminalShellCommand 是终端里运行的命令（经 bash -c，本机和 SSH 相同）：先保证 HOME 有值（没有时取当前用户的主目录，再退到 /tmp），
	// 再按 $SHELL、fish、zsh、bash、sh、mksh 的顺序选第一个能找到的 shell，以登录 shell 的方式替换当前进程；都找不到时退出码为 127。
	terminalShellCommand = `if [ -z "$HOME" ]; then HOME=$(cd ~ 2>/dev/null && pwd); fi; [ -n "$HOME" ] || HOME=/tmp; export HOME
for shell in "$SHELL" fish zsh bash sh mksh; do
  shell_path=$(command -v "$shell" 2>/dev/null) && [ -x "$shell_path" ] && exec "$shell_path" -l
done
echo "no usable shell found" >&2
exit 127`
	// terminalStopTimeout 是删除对话时结束一个终端的等待上限。
	terminalStopTimeout = 5 * time.Second
)

// ── Runtime 模型目录 ──

// ModelList 是 ListModels 返回的模型目录；每一项只含公开字段，不含连接参数和凭证。
type ModelList struct {
	Models []*defs.ModelDefinition `json:"models"`
	// AgentDefaultModel 是请求中 Agent 的默认模型；没有指定 Agent 时为零值。
	AgentDefaultModel defs.ModelRef `json:"agent_default_model,omitzero"`
}

// AgentInfo 是 ListAgents 返回的一个 Agent 的公开说明，不含系统指令。
type AgentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// DefaultModel 是 Agent 的默认模型；Agent 没有指定默认模型时为零值。
	DefaultModel defs.ModelRef `json:"default_model,omitzero"`
}

// SetDefinitionsResult 是 SetDefinitions 返回的结果，列出这次没换上新定义的对话。
type SetDefinitionsResult struct {
	// BusyConversationIDs 是正在执行 Turn 或有后台命令没结束、这次没有换上新定义的对话；它们空闲后再 reload 才用上新定义。
	BusyConversationIDs []defs.ConversationID `json:"busy_conversation_ids"`
}

// ── runtimeConversation 事件循环请求 ──

// runtimeConversationRequest 是交给对话事件循环处理的请求集合。
type runtimeConversationRequest interface{ isRuntimeConversationRequest() }

// subscribeRequested 由 SubscribeSession / SubscribeConversation 触发，请求事件循环登记订阅并回复初始信息。
type subscribeRequested struct {
	// ctx 是订阅方的 ctx；它结束时自动退订。
	ctx context.Context
	// sessionID 为空时订阅对话标题与运行信息，否则订阅这个 Session 的内容。
	sessionID defs.SessionID
	reply     chan subscribeReply
}

func (*subscribeRequested) isRuntimeConversationRequest() {}

// subscribeReply 是登记结果和登记时一并取出的初始信息。
type subscribeReply struct {
	// contentStream、lastSavedOrdinal、streamingItems 只在内容订阅时有值：事件通道、续传位置和生成中 Item。
	contentStream *subscriptionStream[conversation.ConversationEvent]
	// statusStream、title、sessions 只在对话订阅时有值：事件通道、已保存的标题和全部 Session 的运行信息。
	statusStream     *subscriptionStream[ConversationEvent]
	title            string
	sessions         []SessionRuntimeInfo
	lastSavedOrdinal storage.Ordinal
	streamingItems   []StreamingItem
	pendingInputs    session.SessionPendingInputs
	err              error
}

// contextUsageRequested 由 GetSessionContextUsage 触发，请求事件循环回复一个 Session 的上下文占用详情。
type contextUsageRequested struct {
	sessionID defs.SessionID
	reply     chan contextUsageReply
}

func (*contextUsageRequested) isRuntimeConversationRequest() {}

// contextUsageReply 是事件循环对 contextUsageRequested 的回复。
type contextUsageReply struct {
	usage SessionContextUsage
	err   error
}

// unsubscribeRequested 由 newRuntimeSubscriber 登记的回调在订阅方 ctx 结束后发出，请求事件循环移除并关闭这个订阅。
type unsubscribeRequested struct {
	// contentStream 与 statusStream 恰好一个非 nil，分别对应内容订阅和对话订阅。
	contentStream *subscriptionStream[conversation.ConversationEvent]
	statusStream  *subscriptionStream[ConversationEvent]
	// reason 是订阅方 ctx 的 context.Cause，作为订阅的关闭原因。
	reason error
}

func (*unsubscribeRequested) isRuntimeConversationRequest() {}

// sessionRecordReplaceRequested 由 SetSessionModel 在保存 Session 记录后发出，请求事件循环换上新记录，并推送变化后的运行信息。
type sessionRecordReplaceRequested struct {
	record defs.Session
}

func (*sessionRecordReplaceRequested) isRuntimeConversationRequest() {}

// sessionsRunningRequested 由 isAnySessionRunning 发出（改 Executor 设置、删除对话和忙闲检查时），请求事件循环回复是否有 Session 正在执行 Turn。
type sessionsRunningRequested struct {
	reply chan bool
}

func (*sessionsRunningRequested) isRuntimeConversationRequest() {}

// titleChangeRequested 由 setTitle 发出：SubmitInput 接收用户输入后自动命名，或已打开的对话显式改名；事件循环保存标题后回复。
type titleChangeRequested struct {
	ctx   context.Context
	title string
	// onlyIfEmpty 是否只在标题为空时才改？自动命名时为 true。
	onlyIfEmpty bool
	reply       chan error
}

func (*titleChangeRequested) isRuntimeConversationRequest() {}

// archiveChangeRequested 由 setArchived 在归档或取消归档已打开的对话时发出；事件循环保存后回复最新记录。
type archiveChangeRequested struct {
	ctx      context.Context
	archived bool
	reply    chan archiveChangeReply
}

func (*archiveChangeRequested) isRuntimeConversationRequest() {}

// archiveChangeReply 是事件循环对 archiveChangeRequested 的回复：保存后的对话记录。
type archiveChangeReply struct {
	record defs.Conversation
	err    error
}

// recordRequested 由 getRecord 发出（打开已在运行的对话、改 Executor 设置之后、GetSessionDetails 取标题时），请求事件循环回复内存里最新的对话记录。
type recordRequested struct {
	reply chan defs.Conversation
}

func (*recordRequested) isRuntimeConversationRequest() {}
