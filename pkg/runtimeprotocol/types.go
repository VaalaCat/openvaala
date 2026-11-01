package runtimeprotocol

import (
	"encoding/json"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// ── JSON-RPC 帧 ──

// JSONRPCRequest 是客户端发来的一帧请求（省略 jsonrpc 字段）；Params 原样交给方法处理函数解析。
type JSONRPCRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// JSONRPCMessage 是服务端写出的一帧：响应带 ID 与 Result 或 Error，通知只带 Method 与 Params。
// Result 与 Params 在服务端写出这一帧时才编码。
type JSONRPCMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params any             `json:"params,omitempty"`
	Result any             `json:"result,omitempty"`
	Error  *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError 是响应中的错误；方法处理函数返回它时保留错误码，返回其他错误时由服务端按错误种类换成对应错误码。
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error 返回协议错误文本。
func (e *JSONRPCError) Error() string {
	// 1. 返回响应中的错误消息。
	return e.Message
}

// ── JSON-RPC 错误码 ──

// JSON-RPC 错误码：协议错误由连接产生，-32602 由参数解析、Executor 设置校验和分叉截取点校验产生；Runtime 返回的错误中，Session 忙为 -32001，
// 对话不存在为 -32002，按 SessionID 找不到 Session 为 -32003，其余统一为 -32000。
const (
	ErrorCodeParse                = -32700
	ErrorCodeInvalidRequest       = -32600
	ErrorCodeMethodNotFound       = -32601
	ErrorCodeInvalidParams        = -32602
	ErrorCodeRuntime              = -32000
	ErrorCodeSessionBusy          = -32001
	ErrorCodeConversationNotFound = -32002
	ErrorCodeSessionNotFound      = -32003
)

// ── OpenVaala 请求参数 ──

// InitializeParams 是 initialize 的参数：客户端信息和它使用的协议版本。
type InitializeParams struct {
	ClientInfo      ClientInfo `json:"client_info"`
	ProtocolVersion string     `json:"protocol_version"`
}

// ClientInfo 是客户端的名称和版本。
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// CreateConversationParams 是 conversation/create 的参数。
type CreateConversationParams struct {
	AgentName string              `json:"agent_name"`
	Model     defs.ModelSelection `json:"model,omitzero"`
	// ExecutorSettings 里没有 local 目录时使用 local 的默认目录（server.workspace_directory，没配时是 serve 的工作目录）。
	ExecutorSettings defs.ExecutorSettings `json:"executor_settings"`
	// Environment 为空时本机命令继承 serve 进程的环境。
	Environment []string `json:"environment,omitempty"`
	// Ephemeral 是否建临时对话？临时对话不装配，没有 Executor 和 Skill，executor_settings 原样记下但不生效，environment 忽略。
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// OpenConversationParams 是 conversation/open 的参数；Executor 设置取记录里的值，对话已在运行时忽略 environment，沿用首次打开时的。
type OpenConversationParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	Environment    []string            `json:"environment,omitempty"`
}

// SetExecutorSettingsParams 是 conversation/setExecutorSettings 的参数；executor_settings 是完整的新设置，不是增量。
type SetExecutorSettingsParams struct {
	ConversationID   defs.ConversationID   `json:"conversation_id"`
	ExecutorSettings defs.ExecutorSettings `json:"executor_settings"`
}

// SetClientMCPServersParams 是 conversation/setClientMCPServers 的参数：server_ids 是本连接为这个对话登记的全部客户端 MCP server，
// 换掉本连接之前为这个对话登记的；空列表表示全部移除。server 在客户端机器上运行，serve 只经过本连接转发它的 MCP 消息。
type SetClientMCPServersParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	ServerIDs      []string            `json:"server_ids"`
}

// ListSkillsParams 是 skill/list 的参数；没有 conversation_id 时按 agent_name 和 executor_settings 查草稿 Skills。
type ListSkillsParams struct {
	ConversationID   defs.ConversationID   `json:"conversation_id,omitempty"`
	AgentName        string                `json:"agent_name,omitempty"`
	ExecutorSettings defs.ExecutorSettings `json:"executor_settings,omitzero"`
}

// ListModelsParams 是 model/list 的参数；agent_name 为空时不返回 Agent 默认模型。
type ListModelsParams struct {
	AgentName string `json:"agent_name"`
}

// EmptyParams 用于无参数的方法，包括全局运行订阅和退订。
type EmptyParams struct{}

// ProviderParams 是 provider/fetchModels 的参数：完整的 Provider。
type ProviderParams struct {
	Provider defs.ProviderConfig `json:"provider"`
}

// SetProviderParams 是 provider/set 的参数：完整的 Provider，以及可选的、同时写进配置的默认模型（provider/model）。
type SetProviderParams struct {
	Provider     defs.ProviderConfig `json:"provider"`
	DefaultModel string              `json:"default_model,omitempty"`
}

// ProviderIDParams 是 provider/delete 的参数（配置里的 Provider id），也是 catalog/models/list 的参数（模型目录里的服务 id）。
type ProviderIDParams struct {
	ProviderID string `json:"provider_id"`
}

// SetDefaultModelParams 是 config/setDefaultModel 的参数：model 写成 provider/model。
type SetDefaultModelParams struct {
	Model string `json:"model"`
}

// ResolveDirectoryParams 是 executor/resolveDirectory 的参数；base_directory 为空时相对路径拼在这个 Executor 的默认根目录后面。
type ResolveDirectoryParams struct {
	ExecutorID    string `json:"executor_id"`
	Directory     string `json:"directory"`
	BaseDirectory string `json:"base_directory,omitempty"`
}

// ListDirectoriesParams 是 executor/listDirectories 的参数；directory 不包含候选筛选词。
type ListDirectoriesParams struct {
	ExecutorID    string `json:"executor_id"`
	Directory     string `json:"directory"`
	BaseDirectory string `json:"base_directory,omitempty"`
}

// SetSessionModelParams 是 session/setModel 的参数。
type SetSessionModelParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	SessionID      defs.SessionID      `json:"session_id"`
	Model          defs.ModelSelection `json:"model"`
}

// ConversationParams 用于只需要对话身份的方法：read、interruptTurn、compact、delete、subscribe、unsubscribe、mcpServer/list。
type ConversationParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
}

// ListConversationsParams 是 conversation/list 的参数；archived 为 true 时只列归档的对话，省略或 false 时只列没归档的。
type ListConversationsParams struct {
	Directories []string                              `json:"directories,omitempty"`
	SearchTerm  string                                `json:"search_term,omitempty"`
	Order       storage.Order                         `json:"order,omitzero"`
	Cursor      dao.RecordCursor[defs.ConversationID] `json:"cursor,omitzero"`
	Limit       int                                   `json:"limit,omitempty"`
	Archived    bool                                  `json:"archived,omitempty"`
}

// ForkConversationParams 是 conversation/fork 的参数。
type ForkConversationParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	// BeforeTurnID 复制到这一轮之前（不含），LastTurnID 复制到这一轮（含）；最多给一个，都不给时复制全部已结束的轮次。
	BeforeTurnID defs.TurnID `json:"before_turn_id,omitempty"`
	LastTurnID   defs.TurnID `json:"last_turn_id,omitempty"`
	// Model 省略时照抄来源根 Session 的模型。
	Model defs.ModelSelection `json:"model,omitzero"`
	// ExecutorSettings 是新对话的完整设置，和 Environment 一起按 conversation/create 的规则装配。
	ExecutorSettings defs.ExecutorSettings `json:"executor_settings"`
	Environment      []string              `json:"environment,omitempty"`
}

// SetConversationArchivedParams 是 conversation/setArchived 的参数：archived 为 true 时归档，false 时取消归档。
type SetConversationArchivedParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	Archived       bool                `json:"archived"`
}

// SetConversationTitleParams 是 conversation/setTitle 的参数。
type SetConversationTitleParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	Title          string              `json:"title"`
}

// SubmitInputParams 是 conversation/submitInput 的参数。
type SubmitInputParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	InputID        defs.InputID        `json:"input_id,omitempty"`
	// Mode 省略时使用普通模式。
	Mode session.SessionInputMode `json:"mode,omitempty"`
	// Content 原样交给 Runtime；Session 拒绝空内容、空条目、未知类型和缺少载荷的内容，图片的 data URL 格式不检查。
	Content []*defs.Content `json:"content"`
}

// SteerInputParams 是 conversation/steerInput 的参数。
type SteerInputParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	ItemID         defs.ItemID         `json:"item_id"`
	TurnID         defs.TurnID         `json:"turn_id"`
}

// InterruptTurnWithInputParams 是 conversation/interruptTurnWithInput 的参数。
type InterruptTurnWithInputParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	ItemID         defs.ItemID         `json:"item_id"`
	TurnID         defs.TurnID         `json:"turn_id"`
}

// WithdrawInputParams 是 conversation/withdrawInput 的参数。
type WithdrawInputParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	ItemID         defs.ItemID         `json:"item_id"`
	ReturnContent  bool                `json:"return_content"`
}

// SessionParams 用于定位对话内一个 Session 的方法：session/subscribe、session/unsubscribe、session/contextUsage、session/details。
type SessionParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	SessionID      defs.SessionID      `json:"session_id"`
}

// GetSessionParams 是 session/get 的参数：只凭 session_id 找 Session，不需要知道它属于哪个对话。
type GetSessionParams struct {
	SessionID defs.SessionID `json:"session_id"`
}

// ListSessionsParams 是 session/list 的参数。
type ListSessionsParams struct {
	ConversationID defs.ConversationID              `json:"conversation_id"`
	Order          storage.Order                    `json:"order,omitzero"`
	Cursor         dao.RecordCursor[defs.SessionID] `json:"cursor,omitzero"`
	Limit          int                              `json:"limit,omitempty"`
}

// ListTurnsParams 是 session/turns/list 的参数；conversation_id 选这个对话的存储，Turn 记录的读取不要求对话已打开。
type ListTurnsParams struct {
	ConversationID defs.ConversationID           `json:"conversation_id"`
	SessionID      defs.SessionID                `json:"session_id"`
	Order          storage.Order                 `json:"order,omitzero"`
	Cursor         dao.RecordCursor[defs.TurnID] `json:"cursor,omitzero"`
	Limit          int                           `json:"limit,omitempty"`
}

// GetTurnsParams 是 session/turns/get 的参数；Turn 记录的读取不要求对话已打开。
type GetTurnsParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	SessionID      defs.SessionID      `json:"session_id"`
	TurnIDs        []defs.TurnID       `json:"turn_ids"`
}

// ListItemsParams 是 session/items/list 的参数；一页 Item 的 JSON 累计超过服务端的页大小上限时提前截断。
type ListItemsParams struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	SessionID      defs.SessionID      `json:"session_id"`
	Order          storage.Direction   `json:"order,omitempty"`
	Cursor         storage.Ordinal     `json:"cursor,omitempty"`
	Limit          int                 `json:"limit,omitempty"`
}

// TerminalOpenParams 是 terminal/open 的参数：在 executor_id 对应的 Executor 上为这条连接新开一个终端，shell 的起始目录是 directory。
type TerminalOpenParams struct {
	// ExecutorID 为空时用 local Executor；virtual Executor 不支持终端。
	ExecutorID string `json:"executor_id"`
	// Directory 为空时用该 Executor 的默认工作目录；相对路径接在默认工作目录后面，~ 按 Executor 所在机器的主目录展开。
	Directory string `json:"directory"`
	// Rows、Cols 是终端初始行列数，为零时按 24×80。
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
	// Environment 是客户端的环境变量（格式同 os.Environ），只给 local Executor 的 shell 用；为空时继承 serve 进程的环境。
	Environment []string `json:"environment,omitempty"`
}

// TerminalParams 是 terminal/close 的参数：结束这条连接打开的一个终端。
type TerminalParams struct {
	TerminalID string `json:"terminal_id"`
}

// ── OpenVaala 请求结果 ──

// InitializeResult 是 initialize 的结果。
type InitializeResult struct {
	ServerInfo      ServerInfo `json:"server_info"`
	ProtocolVersion string     `json:"protocol_version"`
	// ServerID 标识这台机器上的 serve；客户端读到的本机 server_id 与它一致，就说明两边用的是同一个文件系统。
	ServerID string `json:"server_id"`
}

// ServerInfo 是服务端名称。
type ServerInfo struct {
	Name string `json:"name"`
}

// EmptyResult 是没有返回内容的方法的结果，编码为 {}。
type EmptyResult struct{}

// ConversationResult 是返回一条对话记录的结果。
type ConversationResult struct {
	Conversation defs.Conversation `json:"conversation"`
}

// ListConversationsResult 是 conversation/list 的结果：一页对话和下一页游标。
type ListConversationsResult struct {
	Conversations []dao.ConversationListEntry           `json:"conversations"`
	NextCursor    dao.RecordCursor[defs.ConversationID] `json:"next_cursor,omitzero"`
}

// ListAgentsResult 是 agent/list 的结果：配置里的全部 Agent，按配置顺序。
type ListAgentsResult struct {
	Agents []runtime.AgentInfo `json:"agents"`
}

// ListExecutorsResult 是 executor/list 的结果：全部 Executor 和它们的默认根目录。
type ListExecutorsResult struct {
	Executors []conversation.ExecutorDetails `json:"executors"`
}

// ListProvidersResult 是 provider/list 的结果：配置里的全部 Provider，带 API key 原文；DefaultModel 是模型配置的默认模型
// （provider/model），没有时为空；Defaults 是同次加载的模型默认配置。
type ListProvidersResult struct {
	Providers    []defs.ProviderConfig `json:"providers"`
	DefaultModel string                `json:"default_model"`
	Defaults     defs.ModelDefaults    `json:"defaults"`
}

// ListCatalogProvidersResult 是 catalog/providers/list 的结果：模型目录里能对应上协议和 base_url 的服务。
type ListCatalogProvidersResult struct {
	Providers []defs.CatalogProvider `json:"providers"`
}

// ProviderModelsResult 是 catalog/models/list、provider/fetchModels 的结果：模型 id 到模型配置。
type ProviderModelsResult struct {
	Models map[string]*defs.ProviderModelConfig `json:"models"`
}

// ListMCPServersResult 是 mcpServer/list 的结果：对话里每个 Executor 上的每个 MCP 连接，按 Executor 在对话里的顺序排列。
type ListMCPServersResult struct {
	Servers []conversation.MCPServerInfo `json:"servers"`
}

// ListHooksResult 是 hook/list 的结果：配置里的全部 hook，按配置顺序排列。
type ListHooksResult struct {
	Hooks []*hook.HookInfo `json:"hooks"`
}

// ResolveDirectoryResult 是 executor/resolveDirectory 的结果：解析后的绝对目录。
type ResolveDirectoryResult struct {
	WorkspaceDirectory string `json:"workspace_directory"`
}

// TerminalOpenResult 是 terminal/open 的结果：终端 ID，以及实际使用的 Executor 和已解析的绝对起始目录。
type TerminalOpenResult struct {
	TerminalID string `json:"terminal_id"`
	ExecutorID string `json:"executor_id"`
	Directory  string `json:"directory"`
}

// SubmitInputResult 是 conversation/submitInput 的结果：这次输入的 ItemID。
type SubmitInputResult struct {
	ItemID defs.ItemID `json:"item_id"`
}

// WithdrawInputResult 是取回的用户消息内容；撤回时不返回内容。
type WithdrawInputResult struct {
	Content []*defs.Content `json:"content,omitempty"`
}

// InterruptTurnResult 是 conversation/interruptTurn 的结果：被中断的 TurnID，没有活动 Turn 时省略。
type InterruptTurnResult struct {
	TurnID defs.TurnID `json:"turn_id,omitempty"`
}

// CompactContextResult 是 conversation/compact 的结果：刚开始的压缩 Turn；压缩尚未完成。
type CompactContextResult struct {
	TurnID defs.TurnID `json:"turn_id"`
}

// SessionResult 是返回一条 Session 记录的结果。
type SessionResult struct {
	Session defs.Session `json:"session"`
}

// ListSessionsResult 是 session/list 的结果：一页 Session 和下一页游标。
type ListSessionsResult struct {
	Sessions   []defs.Session                   `json:"sessions"`
	NextCursor dao.RecordCursor[defs.SessionID] `json:"next_cursor,omitzero"`
}

// ListTurnsResult 是 session/turns/list 的结果：一页 Turn 和下一页游标。
type ListTurnsResult struct {
	Turns      []defs.Turn                   `json:"turns"`
	NextCursor dao.RecordCursor[defs.TurnID] `json:"next_cursor,omitzero"`
}

// GetTurnsResult 是 session/turns/get 的结果：按请求顺序找到的 Turn。
type GetTurnsResult struct {
	Turns []defs.Turn `json:"turns"`
}

// ListItemsResult 是 session/items/list 的结果：一页日志和下一页游标。
type ListItemsResult struct {
	Items      []LoggedItem    `json:"items"`
	NextCursor storage.Ordinal `json:"next_cursor,omitempty"`
}

// LoggedItem 是 Session 日志中的一条 Item 及其流水号。
type LoggedItem struct {
	Ordinal storage.Ordinal `json:"ordinal"`
	Item    *defs.Item      `json:"item"`
}

// SubscribeConversationResult 是对话订阅的初始信息：登记时已保存的标题与全部 Session 的运行信息，按 SessionID 排序。
type SubscribeConversationResult struct {
	Title    string                       `json:"title"`
	Sessions []runtime.SessionRuntimeInfo `json:"sessions"`
}

// SubscribeSessionResult 是 Session 内容订阅的初始信息。
type SubscribeSessionResult struct {
	// LastSavedOrdinal 及之前的日志用 session/items/list 补齐。
	LastSavedOrdinal storage.Ordinal `json:"last_saved_ordinal"`
	// StreamingItems 是正在生成的 Item，后续 item/delta 按 item_index 接上。
	StreamingItems []StreamingItem              `json:"streaming_items"`
	PendingInputs  session.SessionPendingInputs `json:"pending_inputs"`
}

// StreamingItem 是一个生成中的 Item 及它在本轮 Agent 输出中的位置，与通知的 item_index 对应。
type StreamingItem struct {
	ItemIndex int        `json:"item_index"`
	Item      *defs.Item `json:"item"`
}

// ── OpenVaala 通知参数 ──

// RunningConversationsClosedParams 由连接在全局运行订阅非主动关闭后发出，携带关闭原因。
type RunningConversationsClosedParams struct {
	Reason string `json:"reason"`
}

// NotificationTarget 是单对话通知的定位字段：对话、事件所属的 Session，以及通知来自哪种订阅；
// 对话订阅的 subscription/closed 中 Session 为空。
type NotificationTarget struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	SessionID      defs.SessionID      `json:"session_id"`
	// Subscription 是 SubscriptionKindSession 或 SubscriptionKindConversation；客户端据此把通知分别交给内容与状态显示。
	Subscription SubscriptionKind `json:"subscription"`
}

// SubscriptionKind 是通知来源的订阅种类，JSON 中为 "session" 或 "conversation"。
type SubscriptionKind string

// 通知来源：Session 内容订阅与对话标题、运行信息订阅。
const (
	SubscriptionKindSession      SubscriptionKind = "session"
	SubscriptionKindConversation SubscriptionKind = "conversation"
)

// SessionStatusUpdatedParams 由连接在收到 Session 运行信息变化时发出，携带变化后的完整信息。
type SessionStatusUpdatedParams struct {
	NotificationTarget
	Info runtime.SessionRuntimeInfo `json:"info"`
}

// SessionPendingInputsUpdatedParams 由连接在收到待处理列表变化时发出。
type SessionPendingInputsUpdatedParams struct {
	NotificationTarget
	PendingInputs session.SessionPendingInputs `json:"pending_inputs"`
}

// ConversationTitleUpdatedParams 由连接在对话标题保存成功且变化后发出，空字符串表示清空标题。
type ConversationTitleUpdatedParams struct {
	NotificationTarget
	Title string `json:"title"`
}

// SessionStartedParams 由连接在收到 Session 启动事件时发出，携带 Session 记录和启动前读到的最新日志序号。
type SessionStartedParams struct {
	NotificationTarget
	Session          defs.Session    `json:"session"`
	LastSavedOrdinal storage.Ordinal `json:"last_saved_ordinal"`
}

// SessionStoppedParams 由连接在 Session.Run 返回后发出，error 是返回的错误文本。
type SessionStoppedParams struct {
	NotificationTarget
	Error string `json:"error,omitempty"`
}

// TurnStartedParams 由连接在 Session 开始执行 Turn 时发出，携带 TurnID。
type TurnStartedParams struct {
	NotificationTarget
	TurnID defs.TurnID `json:"turn_id"`
}

// TurnFinishedParams 由连接在 Turn 正常结束、失败或中断后发出；status 只在终态已保存时填写，输出另由 Item 通知发送。
type TurnFinishedParams struct {
	NotificationTarget
	TurnID           defs.TurnID      `json:"turn_id"`
	Status           defs.TurnStatus  `json:"status,omitempty"`
	InputItemIDs     []defs.ItemID    `json:"input_item_ids"`
	FinalModelCallID defs.ModelCallID `json:"final_model_call_id,omitempty"`
	Error            string           `json:"error,omitempty"`
}

// ItemEventParams 由连接在 Item 开始、增量、完成、中断或工具进度事件到达时发出，各通知只填对应字段。
type ItemEventParams struct {
	NotificationTarget
	TurnID       defs.TurnID              `json:"turn_id"`
	ItemIndex    int                      `json:"item_index"`
	Item         *defs.Item               `json:"item,omitempty"`
	Delta        *agent.AgentItemDelta    `json:"delta,omitempty"`
	ToolProgress *agent.AgentToolProgress `json:"tool_progress,omitempty"`
}

// ItemsSavedParams 由连接在一批 Item 写入 Session 日志后发出，条目按日志序号递增。
type ItemsSavedParams struct {
	NotificationTarget
	TurnID defs.TurnID  `json:"turn_id"`
	Items  []LoggedItem `json:"items"`
}

// HookRunParams 由连接在 Hook 开始和结束时各发一次，携带同一 ID 的完整运行记录。
// 工具与 Stop Hook 来自 Agent，SessionStart、SubagentStart、UserPromptSubmit Hook 来自 Turn。
type HookRunParams struct {
	NotificationTarget
	TurnID  defs.TurnID   `json:"turn_id"`
	HookRun *defs.HookRun `json:"hook_run"`
}

// SubscriptionClosedParams 由连接在订阅事件流关闭后发出，携带退订、过慢、对话退出、Runtime 停止或配置更新的原因；
// 原因是配置更新时，客户端重新订阅并同步。
type SubscriptionClosedParams struct {
	NotificationTarget
	Reason string `json:"reason"`
}

// ── Client MCP 通知参数 ──

// ClientMCPConnectParams 是 serve 发给客户端的 clientMCP/connect 的参数：转发传输每次连接尝试时发出一次，
// 请客户端按 server_id 的配置连上本机的 MCP server，此后这条流上的消息都带 stream_id。重连时换新的 stream_id。
type ClientMCPConnectParams struct {
	StreamID string `json:"stream_id"`
	ServerID string `json:"server_id"`
}

// ClientMCPMessageParams 是 clientMCP/message 的参数，两个方向通用：serve 发给客户端的是 MCP SDK 写出的消息，
// 客户端发给 serve 的是本机 MCP server 发来的消息。message 是 stream_id 这条流上的一条完整 MCP JSON-RPC 消息原文。
type ClientMCPMessageParams struct {
	StreamID string          `json:"stream_id"`
	Message  json.RawMessage `json:"message"`
}

// ClientMCPClosedParams 是客户端发给 serve 的 clientMCP/closed 的参数：客户端在 stream_id 这条流连不上、或本机 MCP server 结束时发出；
// error 是失败原因，正常结束时省略。
type ClientMCPClosedParams struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// ClientMCPDisconnectParams 是 serve 发给客户端的 clientMCP/disconnect 的参数：serve 一侧结束 stream_id 这条流时发出，
// 客户端随之关闭与本机 MCP server 的连接。
type ClientMCPDisconnectParams struct {
	StreamID string `json:"stream_id"`
}

// ── Terminal 通知参数 ──

// TerminalInputParams 是客户端发给 serve 的 terminal/input 的参数：data 原样写进终端，JSON 里是 base64。
type TerminalInputParams struct {
	TerminalID string `json:"terminal_id"`
	Data       []byte `json:"data"`
}

// TerminalResizeParams 是客户端发给 serve 的 terminal/resize 的参数：修改终端的行列数。
type TerminalResizeParams struct {
	TerminalID string `json:"terminal_id"`
	Rows       uint16 `json:"rows"`
	Cols       uint16 `json:"cols"`
}

// TerminalOutputParams 是 serve 发给打开该终端的连接的 terminal/output 的参数：终端新产生的一批输出，JSON 里是 base64，按产生顺序送达。
type TerminalOutputParams struct {
	TerminalID string `json:"terminal_id"`
	Data       []byte `json:"data"`
}

// TerminalExitedParams 是 serve 发给打开该终端的连接的 terminal/exited 的参数：终端进程结束后发出一次，此后这个 terminal_id 不再有通知。
type TerminalExitedParams struct {
	TerminalID string `json:"terminal_id"`
	ExitCode   int    `json:"exit_code"`
	// Error 是进程没能取得退出码的原因（如远端连接断开），正常结束或被关闭时省略。
	Error string `json:"error,omitempty"`
}
