package runtimeclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// ── Client 错误 ──

// ErrConnectionLost 表示请求发出后连接断开，服务端是否执行未知。
var ErrConnectionLost = errors.New("connection to runtime server lost")

// ErrUnauthorized 表示服务端拒绝了 token；Client.Run 遇到它时返回，不再重连。
var ErrUnauthorized = errors.New("runtime server rejected the token")

// ErrClientStopped 表示 Client.Run 已返回，不再处理调用。
var ErrClientStopped = errors.New("runtime client has stopped")

// ErrInitializeRejected 表示服务端拒绝 initialize；Client.Run 返回此错误，不再重连。
// 错误文本提示用户停止旧 serve 后重试。
var ErrInitializeRejected = errors.New("runtime server rejected the protocol version; run openvaala serve stop and retry")

// serverRuntimeError 是服务端返回的、带固定错误码的 Runtime 错误：Error() 是服务端原文，errors.Is 识别对应的哨兵错误。
type serverRuntimeError struct {
	message  string
	sentinel error
}

// Error 返回服务端错误文本。
func (e *serverRuntimeError) Error() string {
	// 1. 读取错误文本。
	return e.message
}

// Unwrap 返回可由 errors.Is 识别的 Runtime 错误。
func (e *serverRuntimeError) Unwrap() error {
	// 1. 返回对应的哨兵错误。
	return e.sentinel
}

// ResponseError 是服务端以 JSON-RPC 错误拒绝请求时的错误码与原文；Session 忙、对话不存在、找不到 Session 与分叉截取点不合法
// 另外还原为哨兵错误，不是 ResponseError。
// 调用方用 errors.As 取出，按错误码转换成自己协议的错误。
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error 返回带错误码的服务端错误文本。
func (e *ResponseError) Error() string {
	// 1. 拼接错误消息和错误码。
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

// ── Client 请求参数 ──

// ListConversationsRequest 是列出对话的参数。
type ListConversationsRequest struct {
	// Directories 按 local 目录筛选，为空时不限。
	Directories []string
	// SearchTerm 按区分大小写的标题子串筛选，为空时不限。
	SearchTerm string
	// Order 为零值时按 key（即创建顺序）升序。
	Order storage.Order
	// Cursor 为零值时从起点开始。
	Cursor dao.RecordCursor[defs.ConversationID]
	Limit  int
	// Archived 是否只列归档的对话？为 false 时只列没归档的。
	Archived bool
}

// ListConversationsResult 是筛选后的一页对话及各自的根 Session。
type ListConversationsResult struct {
	Conversations []dao.ConversationListEntry
	// NextCursor 为零值表示没有下一页。
	NextCursor dao.RecordCursor[defs.ConversationID]
}

// ForkConversationRequest 把对话复制到截取点为止；BeforeTurnID 和 LastTurnID 最多填一个，都不填时复制全部已结束的轮次。
type ForkConversationRequest struct {
	ConversationID defs.ConversationID
	// BeforeTurnID 复制到这一轮之前（不含），回退重来用。
	BeforeTurnID defs.TurnID
	// LastTurnID 复制到这一轮（含）。
	LastTurnID defs.TurnID
	// Model 为零值时照抄来源根 Session 的模型。
	Model defs.ModelSelection
	// ExecutorSettings 是新对话的完整设置，一般传来源对话的；含义同 CreateConversationRequest。
	ExecutorSettings defs.ExecutorSettings
	// Environment 的含义同 CreateConversationRequest。
	Environment []string
}

// SetConversationArchivedRequest 归档（Archived 为 true）或取消归档一个已保存的对话。
type SetConversationArchivedRequest struct {
	ConversationID defs.ConversationID
	Archived       bool
}

// DeleteConversationRequest 删除一个已保存的对话。
type DeleteConversationRequest struct {
	ConversationID defs.ConversationID
}

// SubmitInputRequest 以用户身份向对话根 Session 提交内容。
type SubmitInputRequest struct {
	ConversationID defs.ConversationID
	// Mode 省略时为 normal。
	Mode session.SessionInputMode
	// Content 原样交给服务端。
	Content []*defs.Content
}

// CreateConversationRequest 以 AgentName 对应的 Agent 新建对话。
type CreateConversationRequest struct {
	AgentName string
	// Model 为零值时使用 Agent 的默认模型。
	Model defs.ModelSelection
	// ExecutorSettings 里没有 local 目录时由服务端使用默认目录（配了 server.workspace_directory 时是它，否则是 serve 的启动目录）。
	ExecutorSettings defs.ExecutorSettings
	// Environment 为 nil 时本机命令继承 serve 进程的环境。
	Environment []string
	// Ephemeral 是否建临时对话？临时对话不存盘、不进对话列表，没有 Executor 和 Skill，ExecutorSettings 原样存进记录、不装配，
	// Environment 不使用；没有订阅者、也没有在跑的 Turn 一段时间后由服务端丢弃。
	Ephemeral bool
}

// OpenConversationRequest 打开一个已有对话，服务端按对话记录里的 Executor 设置打开。
type OpenConversationRequest struct {
	ConversationID defs.ConversationID
	// SessionID 是要跟随的 Session；为空时跟随根 Session。内容事件、补齐和 SessionSynced 都只针对它。
	SessionID defs.SessionID
	// Environment 为 nil 时本机命令继承 serve 进程的环境；对话已在服务端运行时沿用它第一次打开时的。
	Environment []string
	// SkipHistory 首次同步是否跳过已保存的历史？跳过时 SessionSynced.Items 为空，只交付订阅位置之后的内容；
	// 之后的重新同步照常从上次交付的位置补齐。
	SkipHistory bool
}

// TerminalOpenRequest 指定新终端用哪个 Executor、起始目录和初始尺寸。
type TerminalOpenRequest struct {
	// ExecutorID 为空时用 local Executor。
	ExecutorID string
	// Directory 为空时用该 Executor 的默认工作目录。
	Directory string
	// Rows、Cols 是初始行列数，为零时按 24×80。
	Rows, Cols uint16
	// Environment 为 nil 时 local Executor 的 shell 继承 serve 进程的环境。
	Environment []string
}

// GetSkillsRequest 选择已有对话的 Skills；尚未建立对话时用 AgentName 和 ExecutorSettings 查草稿 Skills。
type GetSkillsRequest struct {
	ConversationID   defs.ConversationID
	AgentName        string
	ExecutorSettings defs.ExecutorSettings
}

// ResolveWorkspaceDirectoryRequest 是 /cd 的输入。
type ResolveWorkspaceDirectoryRequest struct {
	// ExecutorID 是在哪个 Executor 上解析。
	ExecutorID string
	// Directory 是用户写的目录：~、相对或绝对路径。
	Directory string
	// BaseDirectory 是当前目录；为空时相对路径拼在这个 Executor 的默认根目录后面。
	BaseDirectory string
}

// GetWorkspaceDirectoriesRequest 指定 Executor、待列出的目录和相对路径基准。
type GetWorkspaceDirectoriesRequest struct {
	ExecutorID    string
	Directory     string
	BaseDirectory string
}

// ListItemsRequest 是分页读取一个 Session 日志的参数。
type ListItemsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	// Order 为零值时按流水号升序。
	Order storage.Direction
	// Cursor 为零值时从起点开始，翻页时取上一页的 NextCursor。
	Cursor storage.Ordinal
	// Limit 为零时由服务端决定每页条数。
	Limit int
}

// ListItemsResult 是一页日志 Item；服务端可能因页内容过大而少于 Limit 条。
type ListItemsResult struct {
	Items []runtimeprotocol.LoggedItem
	// NextCursor 为零表示没有下一页。
	NextCursor storage.Ordinal
}

// ListTurnsRequest 是按 TurnID 分页列出一个 Session 的 Turn 的参数；Order 与 Cursor 的零值含义同 ListConversationsRequest。
type ListTurnsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	Order          storage.Order
	Cursor         dao.RecordCursor[defs.TurnID]
	Limit          int
}

// GetTurnsRequest 是按 ID 取一个 Session 的多条 Turn 记录的参数。
type GetTurnsRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	TurnIDs        []defs.TurnID
}

// ListTurnsResult 是一页 Turn 记录；NextCursor 为零值表示没有下一页。
type ListTurnsResult struct {
	Turns      []defs.Turn
	NextCursor dao.RecordCursor[defs.TurnID]
}

// ListSessionsRequest 是分页列出对话内 Session 记录的参数；Order 与 Cursor 的零值含义同 ListConversationsRequest。
type ListSessionsRequest struct {
	ConversationID defs.ConversationID
	Order          storage.Order
	Cursor         dao.RecordCursor[defs.SessionID]
	Limit          int
}

// ListSessionsResult 是一页 Session 记录；NextCursor 为零值表示没有下一页。
type ListSessionsResult struct {
	Sessions   []defs.Session
	NextCursor dao.RecordCursor[defs.SessionID]
}

// SetSessionModelRequest 保存一个 Session 所选的模型与推理强度，从下一个 Turn 开始生效；推理强度为空时由服务端按规则解析。
type SetSessionModelRequest struct {
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	Model          defs.ModelSelection
}

// ── Client 事件 ──

// ClientEvent 是 Client 按发生顺序交付给调用方的事件。
type ClientEvent interface{ isClientEvent() }

// ConnectionState 是 Client 与服务端连接的阶段。
type ConnectionState string

const (
	// ConnectionConnecting 是 Run 开始后第一次连接中。
	ConnectionConnecting ConnectionState = "connecting"
	// ConnectionConnected 是已经连上并完成 initialize。
	ConnectionConnected ConnectionState = "connected"
	// ConnectionReconnecting 是连接失败或断开后等着重连。
	ConnectionReconnecting ConnectionState = "reconnecting"
)

// ConnectionChanged 由 Client 在首次连接、连接成功，以及连接失败或断开后开始重连时发出。
type ConnectionChanged struct {
	State ConnectionState
	// Err 是导致重连的原因，手动重连或其他阶段为空。
	Err error
	// Attempt 是连续失败后的第几次重连，从 1 开始。
	Attempt int
	// RetryIn 是距离这次重连的等待时长。
	RetryIn time.Duration
	// ServerID 在 State 为 connected 时是 initialize 返回的 server_id，其他状态为空。
	ServerID string
}

// RunningConversationsChanged 由 Client 在订阅完成、集合变化、连接丢失或订阅失败时交付；未知时不把空切片当成空集合。
type RunningConversationsChanged struct {
	ConversationIDs []defs.ConversationID
	IsKnown         bool
	Err             error
}

// RPCLogChanged 由 Client 在调试记录或暂停状态变化时合并交付；调用 GetRPCFrames 后可再次收到通知。
// DroppedFrames 是上次清空后因采样队列拥塞丢弃的帧数，不包含容量淘汰和主动暂停。
type RPCLogChanged struct {
	DroppedFrames uint64
	Paused        bool
}

// RPCDirection 是调试帧的收发方向。
type RPCDirection string

const (
	RPCSent     RPCDirection = "sent"
	RPCReceived RPCDirection = "received"
)

// RPCFrame 是一次收帧或写出尝试；Payload 是独立的原始 JSON，不包含握手 URL 和认证头。
// 写出尝试在调用 socket.Write 前记录，写出失败时不代表服务端已接收。
type RPCFrame struct {
	ConnectionID uint64
	Sequence     uint64
	Time         time.Time
	Direction    RPCDirection
	Payload      json.RawMessage
}

func (*RunningConversationsChanged) isClientEvent() {}
func (*RPCLogChanged) isClientEvent()               {}

// SessionSynced 由 Client 在打开对话或重连后完成同步时发出，紧跟在 ConversationSynced 之后；之后的 ItemsSaved 紧接 Items。
type SessionSynced struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	// Items 是上次交付位置之后补齐的已保存 Item；打开对话时为全部历史，SkipHistory 时为空。
	Items []runtimeprotocol.LoggedItem
	// StreamingItems 是跟随的 Session 正在生成的 Item 列表。
	StreamingItems []runtimeprotocol.StreamingItem
	// PendingInputs 是本次订阅读到的完整待处理用户列表。
	PendingInputs session.SessionPendingInputs
	// LastSavedOrdinal 是本次同步交付到的位置。
	LastSavedOrdinal storage.Ordinal
}

// ItemsSaved 由 Client 在跟随的 Session 写入一批 Item 后发出，流水号与之前交付的连续。
type ItemsSaved struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	TurnID         defs.TurnID
	Items          []runtimeprotocol.LoggedItem
}

// ItemChanged 由 Client 在跟随的 Session 的 Item 开始、产生增量、完成或中断时发出。
type ItemChanged struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	// Type 区分开始、增量、完成和中断四种情况。
	Type      agent.AgentEventType
	TurnID    defs.TurnID
	ItemIndex int
	// Item 是开始、完成和中断时的完整 Item；中断且不用保存时为 nil，增量时为 nil。
	Item *defs.Item
	// Delta 只在增量时有值。
	Delta *agent.AgentItemDelta
}

// TurnStarted 由 Client 在跟随的 Session 的 Turn 开始时发出（来自内容订阅）。
type TurnStarted struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	TurnID         defs.TurnID
}

// TurnFinished 由 Client 在跟随的 Session 的 Turn 结束时发出（来自内容订阅）。
type TurnFinished struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	TurnID         defs.TurnID
	// Status 在终态没有保存时为空。
	Status defs.TurnStatus
	// InputItemIDs 是本轮处理的输入 Item，可与 SubmitInput 返回的 ItemID 对应。
	InputItemIDs []defs.ItemID
	Error        string
}

// ConversationSynced 由 Client 在打开对话或重新同步后发出，紧跟着发 SessionSynced。
type ConversationSynced struct {
	ConversationID defs.ConversationID
	// Conversation 是 conversation/open 返回的对话记录，带当前的 Executor 设置；标题取对话订阅登记时的值。
	Conversation defs.Conversation
	// SessionInfos 是对话内全部 Session 的运行信息（订阅响应）。
	SessionInfos []runtime.SessionRuntimeInfo
}

// ConversationSyncFailed 由 Client 在重新同步时 serve 拒绝打开对话后发出（例如对话已停止，冷打开时默认 Executor 不可用）。
// Client 放弃这个对话，不再交付它的事件，连接保持；调用方改好设置后可以重新打开。
type ConversationSyncFailed struct {
	ConversationID defs.ConversationID
	// Err 是 serve 返回的错误。
	Err error
}

// SessionStatusChanged 由 Client 在任一 Session 的运行信息变化时发出（来自对话订阅的 session/statusUpdated）。
type SessionStatusChanged struct {
	ConversationID defs.ConversationID
	// Info 是完整的运行信息。
	Info runtime.SessionRuntimeInfo
}

// SessionPendingInputsChanged 由 Client 在跟随 Session 的待处理列表变化时发出。
type SessionPendingInputsChanged struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	PendingInputs  session.SessionPendingInputs
}

// ConversationTitleChanged 由 Client 在已打开对话的标题保存成功并变化后发出。
type ConversationTitleChanged struct {
	ConversationID defs.ConversationID
	// Title 为空表示清空标题。
	Title string
}

// ToolProgress 由 Client 在跟随的 Session 的工具执行产生输出时发出。
type ToolProgress struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	TurnID         defs.TurnID
	// ItemIndex 指向本轮输出中的 ToolCall。
	ItemIndex int
	// Progress.CallID 是这个 ToolCall 的 CallID（中途接入时 ToolCall 可能已经保存、不在生成中 Item 列表里，按它找到调用），
	// Progress.Content 是本次输出的 JSON。
	Progress agent.AgentToolProgress
}

// HookRunChanged 由 Client 在跟随的 Session 里一次 hook 运行开始或结束时发出（来自 hook/started 与 hook/completed）。
type HookRunChanged struct {
	SessionID      defs.SessionID
	ConversationID defs.ConversationID
	// TurnID 是触发这次运行的 Turn。
	TurnID defs.TurnID
	// Run 是这次运行当时的完整记录：同一次运行（相同的 ID）开始时状态为 running，结束时为最终状态。
	Run *defs.HookRun
}

// ClientMCPConnectRequested 由 Client 在收到 serve 的 clientMCP/connect 通知时立即发出（不经同步暂存，也与打开的对话无关）：
// serve 请客户端按 ServerID 的配置连上本机的 MCP server，这条转发流此后用 StreamID 标识；连不上时调用方用 SendClientMCPClosed 告知 serve。
type ClientMCPConnectRequested struct {
	StreamID string
	ServerID string
}

// ClientMCPMessageReceived 由 Client 在收到 serve 的 clientMCP/message 通知时立即发出（不经同步暂存）：
// Message 是 StreamID 这条流上 serve 写给本机 MCP server 的一条完整 JSON-RPC 消息原文。
type ClientMCPMessageReceived struct {
	StreamID string
	Message  json.RawMessage
}

// ClientMCPDisconnectRequested 由 Client 在收到 serve 的 clientMCP/disconnect 通知时立即发出（不经同步暂存）：
// serve 一侧已结束 StreamID 这条流，调用方应关闭与本机 MCP server 的连接。
type ClientMCPDisconnectRequested struct {
	StreamID string
}

// TerminalOutput 由 Client 在收到 serve 的 terminal/output 通知时立即发出（不经同步暂存）：Data 是 TerminalID 这个终端新产生的一批输出，按产生顺序交付。
type TerminalOutput struct {
	TerminalID string
	Data       []byte
}

// TerminalExited 由 Client 在收到 serve 的 terminal/exited 通知时立即发出（不经同步暂存）：TerminalID 这个终端的进程已结束，此后不再有它的事件。
// Error 是没能取得退出码的原因，正常结束或被关闭时为空。
type TerminalExited struct {
	TerminalID string
	ExitCode   int
	Error      string
}

func (*ConnectionChanged) isClientEvent()           {}
func (*SessionSynced) isClientEvent()               {}
func (*ItemsSaved) isClientEvent()                  {}
func (*ItemChanged) isClientEvent()                 {}
func (*TurnStarted) isClientEvent()                 {}
func (*TurnFinished) isClientEvent()                {}
func (*ConversationSynced) isClientEvent()          {}
func (*ConversationSyncFailed) isClientEvent()      {}
func (*SessionPendingInputsChanged) isClientEvent() {}
func (*SessionStatusChanged) isClientEvent()        {}
func (*ConversationTitleChanged) isClientEvent()    {}
func (*ToolProgress) isClientEvent()                {}
func (*HookRunChanged) isClientEvent()              {}

func (*ClientMCPConnectRequested) isClientEvent()    {}
func (*ClientMCPMessageReceived) isClientEvent()     {}
func (*ClientMCPDisconnectRequested) isClientEvent() {}

func (*TerminalOutput) isClientEvent() {}
func (*TerminalExited) isClientEvent() {}

// ── Client 线上消息 ──

// wireRequest 是客户端发给服务端的一帧请求，服务端的响应带同一个 id。
type wireRequest struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// wireNotification 是客户端发给服务端的一帧通知：没有 id，服务端不回应。
type wireNotification struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// wireMessage 是服务端发来的一帧：响应带 ID 与 Result 或 Error，通知只带 Method 与 Params。
type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *ResponseError  `json:"error"`
}

// notificationParams 汇集各通知可能带的字段，按通知名读取需要的部分。
type notificationParams struct {
	ConversationID defs.ConversationID          `json:"conversation_id"`
	SessionID      defs.SessionID               `json:"session_id"`
	Subscription   string                       `json:"subscription"`
	TurnID         defs.TurnID                  `json:"turn_id"`
	Status         defs.TurnStatus              `json:"status"`
	InputItemIDs   []defs.ItemID                `json:"input_item_ids"`
	Error          string                       `json:"error"`
	ItemIndex      int                          `json:"item_index"`
	Item           *defs.Item                   `json:"item"`
	Delta          *agent.AgentItemDelta        `json:"delta"`
	Items          []runtimeprotocol.LoggedItem `json:"items"`
	Reason         string                       `json:"reason"`
	Title          string                       `json:"title"`
	// Info 是 session/statusUpdated 带的 Session 运行信息。
	PendingInputs session.SessionPendingInputs `json:"pending_inputs"`
	Info          *runtime.SessionRuntimeInfo  `json:"info,omitempty"`
	// ToolProgress 是 item/toolProgress 带的工具输出。
	ToolProgress *agent.AgentToolProgress `json:"tool_progress,omitempty"`
	// HookRun 是 hook/started 与 hook/completed 带的 hook 运行记录。
	HookRun *defs.HookRun `json:"hook_run,omitempty"`
	// StreamID、ServerID 与 Message 是 clientMCP/connect、clientMCP/message、clientMCP/disconnect 带的转发流信息。
	StreamID string          `json:"stream_id"`
	ServerID string          `json:"server_id"`
	Message  json.RawMessage `json:"message"`
	// TerminalID、Data 与 ExitCode 是 terminal/output 和 terminal/exited 带的终端信息；terminal/exited 的失败原因在 Error。
	TerminalID string `json:"terminal_id"`
	Data       []byte `json:"data"`
	ExitCode   int    `json:"exit_code"`
}

// receivedNotification 是一条已解码的通知。
type receivedNotification struct {
	method string
	params notificationParams
}

// ── Client 事件循环输入 ──

// clientLoopInput 是交给 Client.Run 事件循环处理的输入。
type clientLoopInput interface{ isClientLoopInput() }

// connectionRequested 由公开方法在发请求前触发：已连接时立即回复当前连接；未连接时，isWaitingForReconnect 为 true 就在下次连接成功时回复，
// 否则立即回复 nil。
type connectionRequested struct {
	reply                 chan *clientConnection
	isWaitingForReconnect bool
}

// conversationOpenRequested 由 OpenConversation 触发：切换要打开的对话，首次同步完成后回复。
type conversationOpenRequested struct {
	request *OpenConversationRequest
	reply   chan conversationOpenResult
}

// conversationOpenResult 是首次同步的结果。
type conversationOpenResult struct {
	conversation defs.Conversation
	err          error
}

// connectionReady 由连接任务在拨号并完成 initialize 后触发；serverID 是 initialize 返回的 server_id。
type connectionReady struct {
	connection *clientConnection
	serverID   string
}

// connectionFailed 由连接任务在拨号或 initialize 失败后触发。
type connectionFailed struct {
	err error
}

// connectionLost 由连接读循环在连接结束后触发。
type connectionLost struct {
	connection *clientConnection
	err        error
}

// notificationReceived 由连接读循环在收到通知时触发。
type notificationReceived struct {
	connection *clientConnection
	message    *wireMessage
}

// syncFinished 由同步任务在订阅并补齐后触发；generation 与当前同步不符时结果已过时。
type syncFinished struct {
	generation int
	result     *syncResult
	err        error
}

// syncResult 是一次同步得到的对话、跟随的 SessionID、Session 状态、补齐的 Item 与生成中 Item 列表。
type syncResult struct {
	conversation     defs.Conversation
	sessionID        defs.SessionID
	sessionInfos     []runtime.SessionRuntimeInfo
	items            []runtimeprotocol.LoggedItem
	streamingItems   []runtimeprotocol.StreamingItem
	pendingInputs    session.SessionPendingInputs
	lastSavedOrdinal storage.Ordinal
}

func (*connectionRequested) isClientLoopInput()       {}
func (*conversationOpenRequested) isClientLoopInput() {}
func (*connectionReady) isClientLoopInput()           {}
func (*connectionFailed) isClientLoopInput()          {}
func (*connectionLost) isClientLoopInput()            {}
func (*notificationReceived) isClientLoopInput()      {}
func (*syncFinished) isClientLoopInput()              {}

// reconnectRequested 由调用方点击重连时触发，循环确认是否需要跳过退避。
type reconnectRequested struct{ reply chan struct{} }

// runningSubscribeRequested 由调用方启用全局运行集合时触发，循环登记恢复意图后回复。
type runningSubscribeRequested struct{ reply chan struct{} }

// runningConversationsSubscribed 由全局订阅请求完成后触发，携带连接和本次订阅的结果。
type runningConversationsSubscribed struct {
	connection *clientConnection
	generation uint64
	result     runtime.RunningConversations
	err        error
}

// rpcLogRequested 由调用方读取、清空或暂停调试记录时触发。
type rpcLogRequested struct {
	operation rpcLogOperation
	paused    bool
	reply     chan []RPCFrame
}
type rpcLogOperation string

const (
	rpcLogRead  rpcLogOperation = "read"
	rpcLogClear rpcLogOperation = "clear"
	rpcLogPause rpcLogOperation = "pause"
)

func (*reconnectRequested) isClientLoopInput()             {}
func (*runningSubscribeRequested) isClientLoopInput()      {}
func (*runningConversationsSubscribed) isClientLoopInput() {}
func (*rpcLogRequested) isClientLoopInput()                {}

// runningSubscriptionPrepared 由重试任务退订完成时触发；循环清除旧订阅通知后允许重新订阅。
type runningSubscriptionPrepared struct {
	connection *clientConnection
	generation uint64
	reply      chan bool
}

func (*runningSubscriptionPrepared) isClientLoopInput() {}
