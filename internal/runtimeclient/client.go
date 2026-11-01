// Package runtimeclient 是 OpenVaala 协议的 Go 客户端：保持一条到 Runtime Server 的 WebSocket 连接，
// 断线后退避重连，并为打开的对话补齐断线期间的内容。
package runtimeclient

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/google/uuid"
)

// Client 是 OpenVaala 协议的客户端：保持一条 WebSocket 连接，断线后退避重连，并恢复已打开对话的订阅与续传。
type Client interface {
	// Run 连接并保持连接直到 ctx 结束；返回前关闭 Events。只能调用一次。
	// 认证失败（ErrUnauthorized）与 initialize 被拒绝时返回错误，不再重连；其他断线按退避重连。
	Run(ctx context.Context) error
	// Events 是按发生顺序交付的事件；调用方需持续读取到关闭。
	Events() <-chan ClientEvent
	// Reconnect 跳过普通断线的退避等待；正在连接或已经连接时不额外拨号。
	Reconnect(ctx context.Context) (err error)
	// SubscribeRunningConversations 启用全局运行集合订阅及断线恢复；初始集合和错误经 Events 交付，重复调用合并。
	SubscribeRunningConversations(ctx context.Context) (err error)
	// GetRPCFrames 返回有界调试记录的独立副本，并允许下一次日志变化通知。
	GetRPCFrames(ctx context.Context) (frames []RPCFrame, err error)
	// ClearRPCFrames 清空记录和拥塞丢帧计数，保留暂停状态和递增序号。
	ClearRPCFrames(ctx context.Context) (err error)
	// SetRPCLogPaused 暂停或恢复采集；不停止业务连接和事件。
	SetRPCLogPaused(ctx context.Context, paused bool) (err error)

	// CreateConversation 新建对话；Model 为零值时使用 Agent 的默认模型。
	CreateConversation(ctx context.Context, request *CreateConversationRequest) (defs.Conversation, error)
	// ListModels 返回模型目录（不含连接参数）与 agentName 的默认模型；agentName 为空时不返回默认模型。
	ListModels(ctx context.Context, agentName string) (runtime.ModelList, error)
	// GetSkills 返回已有对话或草稿 Agent 的 Skill 补全目录；草稿按请求的 Executor 设置查。
	GetSkills(ctx context.Context, request *GetSkillsRequest) ([]skill.SkillInfo, error)
	// GetHooks 调 hook/list，返回 serve 当前配置的全部 hook，按配置顺序；没有 hook 时返回空。
	GetHooks(ctx context.Context) ([]*hook.HookInfo, error)
	// ResolveWorkspaceDirectory 调 executor/resolveDirectory，在 Executor 所在的机器上把用户写的目录解析成绝对路径并确认它是目录。
	ResolveWorkspaceDirectory(ctx context.Context, request *ResolveWorkspaceDirectoryRequest) (string, error)
	// GetWorkspaceDirectories 调 executor/listDirectories，返回解析后的目录及其普通直接子目录。
	GetWorkspaceDirectories(ctx context.Context, request *GetWorkspaceDirectoriesRequest) (directories *conversation.WorkspaceDirectories, err error)
	// ListAgents 调 agent/list，返回配置里的全部 Agent 名、说明和默认模型，按配置顺序。
	ListAgents(ctx context.Context) ([]runtime.AgentInfo, error)
	// ListExecutors 调 executor/list，返回全部 Executor 和它们的默认根目录。
	ListExecutors(ctx context.Context) ([]conversation.ExecutorDetails, error)
	// ListMCPServers 调 mcpServer/list，返回已打开的对话里各 Executor 上的 MCP 连接情况，读取不等待连接；
	// 没有连接时返回空；对话在 serve 里没打开时返回错误。
	ListMCPServers(ctx context.Context, conversationID defs.ConversationID) ([]conversation.MCPServerInfo, error)
	// SetClientMCPServers 调 conversation/setClientMCPServers，换掉本连接为已打开的对话登记的全部客户端 MCP server；serverIDs 为空表示全部移除。
	// server 在本机运行，serve 登记后经本连接请本机连接它们：Client 为每条转发流发出 ClientMCPConnectRequested、ClientMCPMessageReceived
	// 与 ClientMCPDisconnectRequested，调用方用 SendClientMCPMessage、SendClientMCPClosed 回复。登记只属于当前这条连接：
	// 连接断开后 serve 移除它，重连后需要重新登记。对话在 serve 里没打开时返回错误。
	SetClientMCPServers(ctx context.Context, conversationID defs.ConversationID, serverIDs []string) error
	// SendClientMCPMessage 发出 clientMCP/message 通知，把本机 MCP server 在 streamID 这条流上发来的一条完整 JSON-RPC 消息交给 serve；
	// 写出后返回，不等待 serve 处理。当前没有已连接的连接时立即返回 ErrConnectionLost，不等重连：转发流属于发出它的那条连接，
	// 连接断开后 serve 已结束它，重连后的新连接不认识旧的 streamID。
	// ctx 在写出过程中取消会关闭整条连接，所有转发流和请求随之中断重连；调用方应传会话级 ctx。
	SendClientMCPMessage(ctx context.Context, streamID string, message json.RawMessage) error
	// SendClientMCPClosed 发出 clientMCP/closed 通知，告知 serve streamID 这条流已结束：连不上本机 MCP server 或它退出了；
	// err 是失败原因，nil 表示正常结束。送达规则与 ctx 的要求同 SendClientMCPMessage。
	SendClientMCPClosed(ctx context.Context, streamID string, err error) error
	// OpenTerminal 调 terminal/open，在 Executor 上为本连接新开一个终端并返回它的 ID、实际 Executor 和起始目录。
	// 输出和结束分别经 Events 交付 TerminalOutput 与 TerminalExited；终端只属于当前这条连接，连接断开后 serve 结束它，重连后需要重新打开。
	// virtual Executor 不支持终端，返回错误。
	OpenTerminal(ctx context.Context, request *TerminalOpenRequest) (result *runtimeprotocol.TerminalOpenResult, err error)
	// CloseTerminal 调 terminal/close，结束终端进程；终端已经结束时成功。结束后 Events 交付 TerminalExited。
	CloseTerminal(ctx context.Context, terminalID string) (err error)
	// WriteTerminal 发出 terminal/input 通知，把键盘或粘贴的字节原样写进终端；写出后返回，不等待 serve 处理。
	// 当前没有已连接的连接时立即返回 ErrConnectionLost，不等重连。
	WriteTerminal(terminalID string, data []byte) (err error)
	// ResizeTerminal 发出 terminal/resize 通知，修改终端的行列数；送达规则同 WriteTerminal。
	ResizeTerminal(terminalID string, rows, cols uint16) (err error)
	// ListProviders 调 provider/list，返回 serve 配置里的全部 Provider（带 API key 原文）和模型配置的默认模型：models.default_model，
	// 没写时是排序后的第一个模型，没有模型时是零值；defaults 是同次加载的模型默认配置。Agent 自己写了 model 时，它新建对话用的不是这个默认模型。
	ListProviders(ctx context.Context) (providers []defs.ProviderConfig, defaultModel defs.ModelRef, defaults defs.ModelDefaults, err error)
	// SetProvider 调 provider/set，写入整份 Provider 并让 serve 重新加载配置；defaultModel 不是零值时同一次写入还把默认模型改成它。
	// 返回 Runtime 替换目录的结果，加载失败时 serve 的文件和 Runtime 都不变，错误是加载错误原文。
	SetProvider(ctx context.Context, provider defs.ProviderConfig, defaultModel defs.ModelRef) (runtime.SetDefinitionsResult, error)
	// DeleteProvider 调 provider/delete，删除 Provider 并让 serve 重新加载配置；返回值同 SetProvider。
	DeleteProvider(ctx context.Context, providerID string) (runtime.SetDefinitionsResult, error)
	// SetDefaultModel 调 config/setDefaultModel，改 serve 配置里的默认模型并重新加载配置；返回值同 SetProvider。
	SetDefaultModel(ctx context.Context, ref defs.ModelRef) (runtime.SetDefinitionsResult, error)
	// ReloadConfig 调 config/reload，让 serve 不改文件、重新加载整份配置；返回值同 SetProvider。
	ReloadConfig(ctx context.Context) (runtime.SetDefinitionsResult, error)
	// ListCatalogProviders 调 catalog/providers/list，返回模型目录里可选的模型服务；目录拿不到时返回错误。
	ListCatalogProviders(ctx context.Context) ([]defs.CatalogProvider, error)
	// ListCatalogModels 调 catalog/models/list，返回模型目录里一个服务的全部模型，key 是写进配置文件的模型 id。
	ListCatalogModels(ctx context.Context, catalogProviderID string) (map[string]*defs.ProviderModelConfig, error)
	// FetchProviderModels 调 provider/fetchModels，让 serve 从 Provider 的上游 /models 拉模型并补字段；上游请求失败时返回错误。
	FetchProviderModels(ctx context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error)
	// GetContextUsage 返回对话内一个 Session 已有的上下文占用详情，不调用模型。
	GetContextUsage(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (runtime.SessionContextUsage, error)
	// GetSessionDetails 返回对话内一个 Session 的静态配置：所选模型与摘要模型、Executor、skills、工具名。
	GetSessionDetails(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (conversation.SessionDetails, error)
	// SetSessionModel 保存 Session 所选的模型与推理强度，从下一个 Turn 开始生效，返回保存后的记录；成功后服务端推送 session/statusUpdated。
	SetSessionModel(ctx context.Context, request *SetSessionModelRequest) (defs.Session, error)
	// ListConversations 返回一页对话记录：Archived 为 true 时只列归档的，否则只列没归档的；NextCursor 非零时可作为下一页的 Cursor。
	ListConversations(ctx context.Context, request *ListConversationsRequest) (ListConversationsResult, error)
	// ForkConversation 调 conversation/fork，把对话复制到截取点为止并在服务端打开新对话，返回新对话的记录；原对话不变。
	// 截取点之前一轮都没有时服务端新建一个空对话。两个截取点都给、轮次不在根 Session 里、LastTurnID 还没结束时，分别返回可用
	// errors.Is 识别的 conversation.ErrForkBoundaryConflict、ErrForkTurnNotFound、ErrForkTurnRunning。
	ForkConversation(ctx context.Context, request *ForkConversationRequest) (defs.Conversation, error)
	// SetConversationArchived 调 conversation/setArchived，归档或取消归档，返回保存后的记录；不打开也不停止对话，
	// 已经是这个状态时什么都不改。
	SetConversationArchived(ctx context.Context, request *SetConversationArchivedRequest) (defs.Conversation, error)
	// DeleteConversation 调 conversation/delete，删除对话、全部 Session 和历史；有 Session 在执行 Turn 时返回可用 errors.Is 识别的
	// session.ErrSessionBusy，什么都不删。打开着这个对话的 Client 随后重新同步失败，交付 ConversationSyncFailed。
	DeleteConversation(ctx context.Context, request *DeleteConversationRequest) error
	// GetConversation 调 conversation/read，读对话记录，不打开对话。
	GetConversation(ctx context.Context, conversationID defs.ConversationID) (defs.Conversation, error)
	// GetSessionByID 调 session/get，只凭 SessionID 读 Session 记录（含所属对话），不打开对话；找不到时返回可用 errors.Is 识别的
	// runtime.ErrSessionNotFound。
	GetSessionByID(ctx context.Context, sessionID defs.SessionID) (defs.Session, error)
	// ListSessions 调 session/list，分页列出对话内的 Session 记录。
	ListSessions(ctx context.Context, request *ListSessionsRequest) (ListSessionsResult, error)
	// ListItems 调 session/items/list，分页读取一个 Session 的日志。
	ListItems(ctx context.Context, request *ListItemsRequest) (ListItemsResult, error)
	// ListTurns 调 session/turns/list，按 TurnID 分页列出一个 Session 的 Turn。
	ListTurns(ctx context.Context, request *ListTurnsRequest) (ListTurnsResult, error)
	// GetTurns 调 session/turns/get，按 TurnIDs 的顺序返回一个 Session 里找到的 Turn 记录，跳过不存在的。
	GetTurns(ctx context.Context, request *GetTurnsRequest) ([]defs.Turn, error)
	// SetConversationTitle 保存对话标题，空串表示清空。
	SetConversationTitle(ctx context.Context, conversationID defs.ConversationID, title string) error
	// SetConversationExecutorSettings 调 conversation/setExecutorSettings，让对话换上完整的新设置，返回保存后的记录；
	// 有 Session 在执行 Turn 时返回可用 errors.Is 识别的 session.ErrSessionBusy。打开着这个对话的 Client 随后自动重新同步，
	// ConversationSynced 带上新记录。
	SetConversationExecutorSettings(ctx context.Context, conversationID defs.ConversationID, settings defs.ExecutorSettings) (defs.Conversation, error)
	// SubmitInput 以用户身份向对话根 Session 提交内容：为本次提交生成 InputID，请求发出后断线时在重连后用同一 InputID 重发，
	// 直到得到结果或 ctx 结束；服务端按 InputID 判重。DisableSubmitInputRetry 为 true 时直接返回断线错误。
	SubmitInput(ctx context.Context, request *SubmitInputRequest) (defs.ItemID, error)
	// InterruptTurn 中断根 Session 的 Turn，返回 Turn ID。
	InterruptTurn(ctx context.Context, conversationID defs.ConversationID) (defs.TurnID, error)
	// SteerInput 将已排队用户输入移入指定轮，断线不重发。
	SteerInput(ctx context.Context, conversationID defs.ConversationID, itemID defs.ItemID, turnID defs.TurnID) error
	// InterruptTurnWithInput 置顶已排队用户输入并取消指定轮，断线不重发。
	InterruptTurnWithInput(ctx context.Context, conversationID defs.ConversationID, turnID defs.TurnID, itemID defs.ItemID) error
	// WithdrawInput 原子取走用户输入；returnContent 为 true 时返回完整用户消息内容，断线不重发。
	WithdrawInput(ctx context.Context, conversationID defs.ConversationID, itemID defs.ItemID, returnContent bool) ([]*defs.Content, error)
	// CompactContext 调 conversation/compact，让根 Session 开一个只压缩的 Turn，返回它的 TurnID；
	// 根 Session 忙时返回可用 errors.Is 识别的 session.ErrSessionBusy。
	CompactContext(ctx context.Context, conversationID defs.ConversationID) (defs.TurnID, error)
	// OpenConversation 打开一个对话，首次同步完成后返回：之前交付 ConversationSynced 与 SessionSynced（跟随的 Session 的全部历史与
	// 生成中 Item 列表；SkipHistory 时不含历史），之后交付它的实时事件，重连后自动补齐，每次同步都带上请求的环境变量
	// （服务端重启后按记录里的 Executor 设置冷打开）。跟随的 Session 由请求的 SessionID 指定，为空时是根 Session，
	// 对话换配置后的重新同步仍跟随同一个 Session。
	// 重新同步时服务端拒绝打开对话，交付 ConversationSyncFailed 并放弃这个对话。
	// 同一时刻只打开一个对话，打开新对话后旧对话的事件不再交付。
	OpenConversation(ctx context.Context, request *OpenConversationRequest) (defs.Conversation, error)
}

// clientImpl 的连接和打开的对话只由 Run 的事件循环读写，调用方取连接、打开对话的请求和后台任务的结果都经 loopInputs 交给它。
type clientImpl struct {
	opt ClientOpt
	// rpcSamples 只接收尽力采集的调试帧，队列拥塞只增加丢帧计数；网络任务不等待消费。
	rpcSamples  chan RPCFrame
	rpcDropped  atomic.Uint64
	rpcDropWake chan struct{}
	// 调试记录由 Run 循环改写；暂停值同时供网络采样任务读取。
	rpcFrames          []RPCFrame
	rpcNextIndex       int
	rpcSequence        uint64
	rpcLogPaused       atomic.Bool
	rpcLogDirty        bool
	rpcLogNotified     bool
	rpcReportedDropped uint64
	// reconnectTimer 保存下一次拨号的等待，isConnecting 表示唯一拨号任务正在执行。
	reconnectTimer   *time.Timer
	isConnecting     bool
	connectionNumber uint64
	// wantsRunningConversations 保存调用方的订阅意图；其余字段属于当前连接的全局订阅。
	wantsRunningConversations         bool
	isSubscribingRunningConversations bool
	isRunningConversationsSubscribed  bool
	runningSubscriptionGeneration     uint64
	stagedRunningConversations        *runtime.RunningConversations
	runningSubscriptionRetryTimer     *time.Timer
	failedRunningSubscriptions        int
	cancelRunningSubscribe            context.CancelFunc

	// events 是交给调用方的事件，Run 返回前关闭。
	events chan ClientEvent
	// loopInputs 是交给事件循环的调用方请求和后台任务结果。
	loopInputs chan clientLoopInput
	// started 是 Run 只能调用一次的令牌。
	started chan struct{}
	// stopped 在 Run 返回前关闭，之后的调用返回 ErrClientStopped。
	stopped chan struct{}
	// tasks 是拨号、连接读循环与同步任务，Run 返回前等待。
	tasks sync.WaitGroup

	// 以下字段只由 Run 的事件循环读写。

	// connection 是当前连上的连接，断开后为 nil。
	connection *clientConnection
	// connectionWaiters 是在等下一次连上的调用。
	connectionWaiters []chan *clientConnection
	// failedConnectAttempts 是连续连接失败的次数，决定下次重连等多久；连上后同步成功（没有打开的对话时直接）清零。
	failedConnectAttempts int
	// syncGeneration 是最近一次启动的同步任务的编号，用来认出过期的同步结果。
	syncGeneration int
	// cancelSync 取消最近一次启动的同步任务，新同步开始前调用。
	cancelSync context.CancelFunc
	// openedConversation 是当前打开的对话，没有时为 nil。
	openedConversation *openedConversation
}

// openedConversation 是 Client 当前打开的对话及其续传状态；只由事件循环读写。
type openedConversation struct {
	id defs.ConversationID
	// environment 是打开请求带的环境变量，每次同步（包括重连后）都原样交给服务端。
	environment []string
	// sessionID 是跟随的 Session：打开请求指定的 SessionID，请求为空时首次同步完成后是对话记录里的根 Session。
	sessionID defs.SessionID
	// skipHistory 首次同步是否跳过已保存的历史？首次同步完成后置为 false。
	skipHistory bool
	// lastSavedOrdinal 是跟随的 Session 已交付的最后流水号，重连后从它之后补齐。
	lastSavedOrdinal storage.Ordinal
	// isSyncing 同步任务是否还没完成？没完成时收到的通知暂存在 pendingNotifications。
	isSyncing bool
	// syncGeneration 是为这个对话启动的最近一次同步任务的编号。
	syncGeneration int
	// pendingNotifications 是同步期间收到、按到达顺序暂存的通知，同步完成后交付。
	pendingNotifications []*receivedNotification
	// openReply 是等待首次同步结果的 OpenConversation 调用；首次同步完成后置空。
	openReply chan conversationOpenResult
}

// ClientOpt 保存连接地址与 token，构造后只读。
type ClientOpt struct {
	// URL 是 OpenVaala 协议的 WebSocket 地址，例如 ws://127.0.0.1:8790/api/v1/openvaala。
	URL string
	// Token 非空时以 Authorization: Bearer 携带；URL 自带 ?token= 时可为空。
	Token string
	// Telemetry 是遥测管道，用于重连与通知丢弃的日志；nil 时使用全局兜底。
	Telemetry *telemetry.Telemetry
	// FailRequestsWhenDisconnected 为 true 时业务请求不等待连接；零值沿用等待重连。
	FailRequestsWhenDisconnected bool
	// DisableSubmitInputRetry 为 true 时提交后断线直接返回，不重发同一 InputID。
	DisableSubmitInputRetry bool
	// RPCFrameCapacity 是调试记录的帧数上限；0 关闭采集。
	RPCFrameCapacity int
}

const (
	// eventBufferSize 是 events 通道的容量。
	eventBufferSize = 256
	// historyPageSize 是同步时补齐日志每页读的条数。
	historyPageSize = 5
)

// NewClientOpt 设置服务端地址和 token。
func NewClientOpt(url, token string) *ClientOpt {
	// 1. 保存地址和认证 token。
	return &ClientOpt{
		URL:   url,
		Token: token,
	}
}

// NewClient 校验连接地址并创建客户端。
func NewClient(opt *ClientOpt) (Client, error) {
	// 1. 检查服务端地址和采集容量。
	if opt == nil || opt.URL == "" {
		return nil, errors.New("runtime client requires a URL")
	}
	if opt.RPCFrameCapacity < 0 {
		return nil, errors.New("RPC frame capacity must not be negative")
	}
	// 2. 保存配置，创建事件通道、采样通道和生命周期通知。
	var samples chan RPCFrame
	if opt.RPCFrameCapacity > 0 {
		samples = make(chan RPCFrame, min(opt.RPCFrameCapacity, 256))
	}
	return &clientImpl{
		rpcSamples:  samples,
		rpcDropWake: make(chan struct{}, 1),
		opt:         *opt,
		events:      make(chan ClientEvent, eventBufferSize),
		loopInputs:  make(chan clientLoopInput),
		started:     make(chan struct{}, 1),
		stopped:     make(chan struct{}),
	}, nil
}

// Events 返回客户端按发生顺序交付事件的通道。
func (c *clientImpl) Events() <-chan ClientEvent {
	// 1. 返回事件通道。
	return c.events
}

// Run 运行连接和对话同步事件循环。
func (c *clientImpl) Run(ctx context.Context) error {
	// 1. 登记本次运行，重复调用时返回错误。
	select {
	case c.started <- struct{}{}:
	default:
		return errors.New("client Run can only be called once")
	}
	// 2. 设置运行上下文，登记取消、等待任务和关闭通知的收尾操作。
	ctx = telemetry.ContextWithTelemetry(ctx, c.opt.Telemetry)
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		if c.reconnectTimer != nil {
			c.reconnectTimer.Stop()
		}
		if c.runningSubscriptionRetryTimer != nil {
			c.runningSubscriptionRetryTimer.Stop()
		}
		c.tasks.Wait()
		close(c.stopped)
		close(c.events)
	}()
	// 3. 发出连接中事件，开始连接并处理循环输入。
	c.emit(ctx, &ConnectionChanged{State: ConnectionConnecting})
	c.startConnecting(ctx, 0)
	for {
		var retry, runningRetry <-chan time.Time
		if c.reconnectTimer != nil {
			retry = c.reconnectTimer.C
		}
		if c.runningSubscriptionRetryTimer != nil {
			runningRetry = c.runningSubscriptionRetryTimer.C
		}
		var logEvents chan ClientEvent
		if c.rpcLogDirty && !c.rpcLogNotified {
			logEvents = c.events
		}
		logChanged := &RPCLogChanged{DroppedFrames: c.rpcDropped.Load(), Paused: c.rpcLogPaused.Load()}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry:
			c.reconnectTimer = nil
			c.startConnecting(ctx, 0)
		case <-runningRetry:
			c.runningSubscriptionRetryTimer = nil
			c.startRunningSubscription(ctx)
		case frame := <-c.rpcSamples:
			c.appendRPCFrame(frame)
		case <-c.rpcDropWake:
			c.rpcLogDirty = true
		case logEvents <- logChanged:
			c.rpcLogNotified = true
			c.rpcReportedDropped = logChanged.DroppedFrames
		case input := <-c.loopInputs:
			if err := c.handleLoopInput(ctx, input); err != nil {
				return err
			}
		}
	}
}

// handleLoopInput 按输入类型处理连接、对话打开、通知和同步结果。
func (c *clientImpl) handleLoopInput(ctx context.Context, input clientLoopInput) error {
	// 1. 分派输入，不可恢复的连接错误交给 Run 返回。
	switch input := input.(type) {
	case *reconnectRequested:
		if c.connection == nil && !c.isConnecting {
			if c.reconnectTimer != nil {
				c.reconnectTimer.Stop()
				c.reconnectTimer = nil
			}
			c.emit(ctx, &ConnectionChanged{State: ConnectionReconnecting, Attempt: c.failedConnectAttempts})
			c.startConnecting(ctx, 0)
		}
		input.reply <- struct{}{}
	case *runningSubscribeRequested:
		if !c.wantsRunningConversations {
			c.wantsRunningConversations = true
			c.emit(ctx, &RunningConversationsChanged{})
			c.startRunningSubscription(ctx)
		}
		input.reply <- struct{}{}
	case *runningSubscriptionPrepared:
		isCurrent := input.connection == c.connection && input.generation == c.runningSubscriptionGeneration
		if isCurrent {
			c.stagedRunningConversations = nil
		}
		input.reply <- isCurrent
	case *runningConversationsSubscribed:
		c.handleRunningSubscribed(ctx, input)
	case *rpcLogRequested:
		c.handleRPCLogRequested(input)
	case *connectionRequested:
		c.handleConnectionRequested(input)
	case *conversationOpenRequested:
		c.handleConversationOpenRequested(ctx, input)
	case *connectionReady:
		c.handleConnectionReady(ctx, input)
	case *connectionFailed:
		return c.handleConnectionFailed(ctx, input)
	case *connectionLost:
		c.handleConnectionLost(ctx, input)
	case *notificationReceived:
		c.handleNotificationReceived(ctx, input)
	case *syncFinished:
		c.handleSyncFinished(ctx, input)
	default:
		panic(fmt.Sprintf("unsupported Client loop input %T", input))
	}
	return nil
}

// CreateConversation 请求服务端创建对话，返回对话记录。
func (c *clientImpl) CreateConversation(ctx context.Context, request *CreateConversationRequest) (defs.Conversation, error) {
	// 1. 提交 Agent、模型、Executor、环境变量和临时对话设置，返回新对话。
	var result runtimeprotocol.ConversationResult
	err := c.call(ctx, "conversation/create", &runtimeprotocol.CreateConversationParams{
		AgentName:        request.AgentName,
		Model:            request.Model,
		ExecutorSettings: request.ExecutorSettings,
		Environment:      request.Environment,
		Ephemeral:        request.Ephemeral,
	}, &result)
	return result.Conversation, err
}

// ListModels 读取模型目录和 Agent 的默认模型。
func (c *clientImpl) ListModels(ctx context.Context, agentName string) (runtime.ModelList, error) {
	// 1. 请求模型列表并返回解码结果。
	var result runtime.ModelList
	err := c.call(ctx, "model/list", map[string]any{"agent_name": agentName}, &result)
	return result, err
}

// GetHooks 读取服务端配置的 hook 列表。
func (c *clientImpl) GetHooks(ctx context.Context) ([]*hook.HookInfo, error) {
	// 1. 请求 hook 列表并返回记录。
	var result runtimeprotocol.ListHooksResult
	err := c.call(ctx, "hook/list", nil, &result)
	return result.Hooks, err
}

// ResolveWorkspaceDirectory 请求 Executor 解析工作目录，返回绝对路径。
func (c *clientImpl) ResolveWorkspaceDirectory(ctx context.Context, request *ResolveWorkspaceDirectoryRequest) (string, error) {
	// 1. 提交 Executor、目录和相对路径起点，返回解析后的目录。
	var result runtimeprotocol.ResolveDirectoryResult
	err := c.call(ctx, "executor/resolveDirectory", &runtimeprotocol.ResolveDirectoryParams{
		ExecutorID:    request.ExecutorID,
		Directory:     request.Directory,
		BaseDirectory: request.BaseDirectory,
	}, &result)
	return result.WorkspaceDirectory, err
}

// GetWorkspaceDirectories 请求指定 Executor 的目录候选。
func (c *clientImpl) GetWorkspaceDirectories(ctx context.Context, request *GetWorkspaceDirectoriesRequest) (*conversation.WorkspaceDirectories, error) {
	// 1. 提交 Executor、目录和相对路径基准，读取解析后的目录和候选。
	var result conversation.WorkspaceDirectories
	if err := c.call(ctx, "executor/listDirectories", &runtimeprotocol.ListDirectoriesParams{
		ExecutorID:    request.ExecutorID,
		Directory:     request.Directory,
		BaseDirectory: request.BaseDirectory,
	}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListAgents 读取全部 Agent 的公开说明。
func (c *clientImpl) ListAgents(ctx context.Context) ([]runtime.AgentInfo, error) {
	// 1. 请求 Agent 列表并返回。
	var result runtimeprotocol.ListAgentsResult
	err := c.call(ctx, "agent/list", nil, &result)
	return result.Agents, err
}

// ListExecutors 读取全部 Executor 及其默认根目录。
func (c *clientImpl) ListExecutors(ctx context.Context) ([]conversation.ExecutorDetails, error) {
	// 1. 请求 Executor 列表并返回详情。
	var result runtimeprotocol.ListExecutorsResult
	err := c.call(ctx, "executor/list", nil, &result)
	return result.Executors, err
}

// ListMCPServers 读取对话中各 Executor 的 MCP 连接信息。
func (c *clientImpl) ListMCPServers(ctx context.Context, conversationID defs.ConversationID) ([]conversation.MCPServerInfo, error) {
	// 1. 提交对话 ID，返回 MCP server 列表。
	var result runtimeprotocol.ListMCPServersResult
	err := c.call(ctx, "mcpServer/list", map[string]any{"conversation_id": conversationID}, &result)
	return result.Servers, err
}

// SetClientMCPServers 替换本连接为对话登记的客户端 MCP server。
func (c *clientImpl) SetClientMCPServers(ctx context.Context, conversationID defs.ConversationID, serverIDs []string) error {
	// 1. 将 nil 列表转换为空列表，提交本次完整登记。
	if serverIDs == nil {
		serverIDs = []string{}
	}
	return c.call(ctx, "conversation/setClientMCPServers", &runtimeprotocol.SetClientMCPServersParams{
		ConversationID: conversationID,
		ServerIDs:      serverIDs,
	}, nil)
}

// SendClientMCPMessage 将本机 MCP server 的一条消息转发给 serve。
func (c *clientImpl) SendClientMCPMessage(ctx context.Context, streamID string, message json.RawMessage) error {
	// 1. 按转发流 ID 写出消息通知。
	return c.notify(ctx, "clientMCP/message", &runtimeprotocol.ClientMCPMessageParams{
		StreamID: streamID,
		Message:  message,
	})
}

// SendClientMCPClosed 通知 serve 本机 MCP 转发流已结束。
func (c *clientImpl) SendClientMCPClosed(ctx context.Context, streamID string, err error) error {
	// 1. 填写转发流 ID 和结束错误，写出关闭通知。
	params := &runtimeprotocol.ClientMCPClosedParams{StreamID: streamID}
	if err != nil {
		params.Error = err.Error()
	}
	return c.notify(ctx, "clientMCP/closed", params)
}

// OpenTerminal 请求 serve 为本连接新开一个终端。
func (c *clientImpl) OpenTerminal(ctx context.Context, request *TerminalOpenRequest) (*runtimeprotocol.TerminalOpenResult, error) {
	// 1. 提交 Executor、起始目录、初始行列数和客户端环境变量，返回终端信息。
	var result runtimeprotocol.TerminalOpenResult
	err := c.call(ctx, "terminal/open", &runtimeprotocol.TerminalOpenParams{
		ExecutorID:  request.ExecutorID,
		Directory:   request.Directory,
		Rows:        request.Rows,
		Cols:        request.Cols,
		Environment: request.Environment,
	}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// CloseTerminal 请求 serve 结束一个终端进程。
func (c *clientImpl) CloseTerminal(ctx context.Context, terminalID string) error {
	// 1. 按终端 ID 提交关闭请求。
	return c.call(ctx, "terminal/close", &runtimeprotocol.TerminalParams{TerminalID: terminalID}, nil)
}

// WriteTerminal 把输入字节写给 serve 上的终端。
func (c *clientImpl) WriteTerminal(terminalID string, data []byte) error {
	// 1. 写出输入通知，不等待 serve 处理；没有 ctx 可取消，连接断开时写出返回错误。
	return c.notify(context.Background(), "terminal/input", &runtimeprotocol.TerminalInputParams{
		TerminalID: terminalID,
		Data:       data,
	})
}

// ResizeTerminal 把新的行列数告知 serve 上的终端。
func (c *clientImpl) ResizeTerminal(terminalID string, rows, cols uint16) error {
	// 1. 写出尺寸通知，不等待 serve 处理。
	return c.notify(context.Background(), "terminal/resize", &runtimeprotocol.TerminalResizeParams{
		TerminalID: terminalID,
		Rows:       rows,
		Cols:       cols,
	})
}

// ListProviders 读取 Provider 配置、默认模型和模型默认配置。
func (c *clientImpl) ListProviders(ctx context.Context) (providers []defs.ProviderConfig, defaultModel defs.ModelRef, defaults defs.ModelDefaults, err error) {
	// 1. 读取 Provider 列表、默认模型字符串和模型默认配置。
	var result runtimeprotocol.ListProvidersResult
	if err := c.call(ctx, "provider/list", nil, &result); err != nil {
		return nil, defs.ModelRef{}, defs.ModelDefaults{}, err
	}
	// 2. 默认模型为空时返回零值，否则解析后返回。
	if result.DefaultModel == "" {
		return result.Providers, defs.ModelRef{}, result.Defaults, nil
	}
	defaultModel, err = defs.ParseModelRef(result.DefaultModel)
	if err != nil {
		return nil, defs.ModelRef{}, defs.ModelDefaults{}, fmt.Errorf("provider/list: default_model: %w", err)
	}
	return result.Providers, defaultModel, result.Defaults, nil
}

// SetProvider 保存 Provider 配置和可选的默认模型，返回重新加载结果。
func (c *clientImpl) SetProvider(ctx context.Context, provider defs.ProviderConfig, defaultModel defs.ModelRef) (runtime.SetDefinitionsResult, error) {
	// 1. 提交完整 Provider 和非零值的默认模型。
	params := map[string]any{"provider": provider}
	if defaultModel != (defs.ModelRef{}) {
		params["default_model"] = defaultModel.String()
	}
	var result runtime.SetDefinitionsResult
	err := c.call(ctx, "provider/set", params, &result)
	return result, err
}

// DeleteProvider 删除 Provider，返回重新加载结果。
func (c *clientImpl) DeleteProvider(ctx context.Context, providerID string) (runtime.SetDefinitionsResult, error) {
	// 1. 提交 Provider ID，返回服务端结果。
	var result runtime.SetDefinitionsResult
	err := c.call(ctx, "provider/delete", map[string]any{"provider_id": providerID}, &result)
	return result, err
}

// SetDefaultModel 保存默认模型，返回重新加载结果。
func (c *clientImpl) SetDefaultModel(ctx context.Context, ref defs.ModelRef) (runtime.SetDefinitionsResult, error) {
	// 1. 提交模型引用，返回服务端结果。
	var result runtime.SetDefinitionsResult
	err := c.call(ctx, "config/setDefaultModel", map[string]any{"model": ref.String()}, &result)
	return result, err
}

// ReloadConfig 请求服务端重新加载配置。
func (c *clientImpl) ReloadConfig(ctx context.Context) (runtime.SetDefinitionsResult, error) {
	// 1. 发出重新加载请求并返回结果。
	var result runtime.SetDefinitionsResult
	err := c.call(ctx, "config/reload", nil, &result)
	return result, err
}

// ListCatalogProviders 读取模型目录中的服务列表。
func (c *clientImpl) ListCatalogProviders(ctx context.Context) ([]defs.CatalogProvider, error) {
	// 1. 请求目录服务列表，返回记录。
	var result runtimeprotocol.ListCatalogProvidersResult
	err := c.call(ctx, "catalog/providers/list", nil, &result)
	return result.Providers, err
}

// ListCatalogModels 读取目录中一个服务的模型。
func (c *clientImpl) ListCatalogModels(ctx context.Context, catalogProviderID string) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 提交目录服务 ID，返回按模型 ID 索引的配置。
	var result runtimeprotocol.ProviderModelsResult
	err := c.call(ctx, "catalog/models/list", map[string]any{"provider_id": catalogProviderID}, &result)
	return result.Models, err
}

// FetchProviderModels 请求服务端从 Provider 上游读取模型。
func (c *clientImpl) FetchProviderModels(ctx context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 提交 Provider 配置，返回补全字段后的模型。
	var result runtimeprotocol.ProviderModelsResult
	err := c.call(ctx, "provider/fetchModels", map[string]any{"provider": provider}, &result)
	return result.Models, err
}

// GetContextUsage 读取对话内 Session 的上下文占用。
func (c *clientImpl) GetContextUsage(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (runtime.SessionContextUsage, error) {
	// 1. 提交对话和 Session ID，返回占用详情。
	var result runtime.SessionContextUsage
	err := c.call(ctx, "session/contextUsage", map[string]any{"conversation_id": conversationID, "session_id": sessionID}, &result)
	return result, err
}

// GetSessionDetails 读取对话内 Session 的模型、Executor、Skill 和工具配置。
func (c *clientImpl) GetSessionDetails(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (conversation.SessionDetails, error) {
	// 1. 提交对话和 Session ID，返回配置详情。
	var result conversation.SessionDetails
	err := c.call(ctx, "session/details", map[string]any{"conversation_id": conversationID, "session_id": sessionID}, &result)
	return result, err
}

// SetSessionModel 保存 Session 模型和推理强度，返回更新后的记录。
func (c *clientImpl) SetSessionModel(ctx context.Context, request *SetSessionModelRequest) (defs.Session, error) {
	// 1. 提交 Session 的模型选择，返回保存结果。
	var result runtimeprotocol.SessionResult
	err := c.call(ctx, "session/setModel", map[string]any{
		"conversation_id": request.ConversationID,
		"session_id":      request.SessionID,
		"model":           request.Model,
	}, &result)
	return result.Session, err
}

// ListConversations 按筛选条件分页读取对话。
func (c *clientImpl) ListConversations(ctx context.Context, request *ListConversationsRequest) (ListConversationsResult, error) {
	// 1. 填写排序、页大小、归档状态和非空筛选条件。
	params := map[string]any{"order": request.Order, "limit": request.Limit, "archived": request.Archived}
	if len(request.Directories) > 0 {
		params["directories"] = request.Directories
	}
	if request.SearchTerm != "" {
		params["search_term"] = request.SearchTerm
	}
	if request.Cursor != (dao.RecordCursor[defs.ConversationID]{}) {
		params["cursor"] = request.Cursor
	}
	// 2. 请求对话列表，返回记录和下一页游标。
	var result runtimeprotocol.ListConversationsResult
	err := c.call(ctx, "conversation/list", params, &result)
	return ListConversationsResult{
		Conversations: result.Conversations,
		NextCursor:    result.NextCursor,
	}, err
}

// ForkConversation 按截取点复制对话，返回新对话记录。
func (c *clientImpl) ForkConversation(ctx context.Context, request *ForkConversationRequest) (defs.Conversation, error) {
	// 1. 提交来源、截取点、模型、Executor 和环境变量设置，返回新对话。
	var result runtimeprotocol.ConversationResult
	err := c.call(ctx, "conversation/fork", &runtimeprotocol.ForkConversationParams{
		ConversationID:   request.ConversationID,
		BeforeTurnID:     request.BeforeTurnID,
		LastTurnID:       request.LastTurnID,
		Model:            request.Model,
		ExecutorSettings: request.ExecutorSettings,
		Environment:      request.Environment,
	}, &result)
	return result.Conversation, err
}

// SetConversationArchived 保存对话归档状态，返回对话记录。
func (c *clientImpl) SetConversationArchived(ctx context.Context, request *SetConversationArchivedRequest) (defs.Conversation, error) {
	// 1. 提交对话 ID 和归档状态，返回保存结果。
	var result runtimeprotocol.ConversationResult
	err := c.call(ctx, "conversation/setArchived", map[string]any{"conversation_id": request.ConversationID, "archived": request.Archived}, &result)
	return result.Conversation, err
}

// DeleteConversation 请求删除对话及其 Session 和历史。
func (c *clientImpl) DeleteConversation(ctx context.Context, request *DeleteConversationRequest) error {
	// 1. 提交要删除的对话 ID，返回删除结果。
	return c.call(ctx, "conversation/delete", map[string]any{"conversation_id": request.ConversationID}, nil)
}

// GetConversation 读取已保存的对话记录。
func (c *clientImpl) GetConversation(ctx context.Context, conversationID defs.ConversationID) (defs.Conversation, error) {
	// 1. 提交对话 ID，返回服务端记录。
	var result runtimeprotocol.ConversationResult
	err := c.call(ctx, "conversation/read", map[string]any{"conversation_id": conversationID}, &result)
	return result.Conversation, err
}

// GetSessionByID 按 Session ID 读取记录。
func (c *clientImpl) GetSessionByID(ctx context.Context, sessionID defs.SessionID) (defs.Session, error) {
	// 1. 提交 Session ID，返回记录或查询错误。
	var result runtimeprotocol.SessionResult
	err := c.call(ctx, "session/get", map[string]any{"session_id": sessionID}, &result)
	return result.Session, err
}

// ListSessions 分页读取对话内的 Session。
func (c *clientImpl) ListSessions(ctx context.Context, request *ListSessionsRequest) (ListSessionsResult, error) {
	// 1. 提交分页条件，返回 Session 记录和下一页游标。
	var result runtimeprotocol.ListSessionsResult
	err := c.call(ctx, "session/list", &runtimeprotocol.ListSessionsParams{
		ConversationID: request.ConversationID,
		Order:          request.Order,
		Cursor:         request.Cursor,
		Limit:          request.Limit,
	}, &result)
	return ListSessionsResult{
		Sessions:   result.Sessions,
		NextCursor: result.NextCursor,
	}, err
}

// ListItems 分页读取 Session 日志。
func (c *clientImpl) ListItems(ctx context.Context, request *ListItemsRequest) (ListItemsResult, error) {
	// 1. 提交对话、Session 和分页条件，返回 Item 及下一页流水号。
	var result runtimeprotocol.ListItemsResult
	err := c.call(ctx, "session/items/list", &runtimeprotocol.ListItemsParams{
		ConversationID: request.ConversationID,
		SessionID:      request.SessionID,
		Order:          request.Order,
		Cursor:         request.Cursor,
		Limit:          request.Limit,
	}, &result)
	return ListItemsResult{
		Items:      result.Items,
		NextCursor: result.NextCursor,
	}, err
}

// ListTurns 分页读取 Session 的 Turn。
func (c *clientImpl) ListTurns(ctx context.Context, request *ListTurnsRequest) (ListTurnsResult, error) {
	// 1. 提交对话、Session 和分页条件，返回 Turn 记录和下一页游标。
	var result runtimeprotocol.ListTurnsResult
	err := c.call(ctx, "session/turns/list", &runtimeprotocol.ListTurnsParams{
		ConversationID: request.ConversationID,
		SessionID:      request.SessionID,
		Order:          request.Order,
		Cursor:         request.Cursor,
		Limit:          request.Limit,
	}, &result)
	return ListTurnsResult{
		Turns:      result.Turns,
		NextCursor: result.NextCursor,
	}, err
}

// GetTurns 按 ID 顺序读取 Session 中的 Turn。
func (c *clientImpl) GetTurns(ctx context.Context, request *GetTurnsRequest) ([]defs.Turn, error) {
	// 1. 提交 Turn ID 列表，返回服务端找到的记录。
	var result runtimeprotocol.GetTurnsResult
	err := c.call(ctx, "session/turns/get", &runtimeprotocol.GetTurnsParams{
		ConversationID: request.ConversationID,
		SessionID:      request.SessionID,
		TurnIDs:        request.TurnIDs,
	}, &result)
	return result.Turns, err
}

// SetConversationTitle 保存对话标题。
func (c *clientImpl) SetConversationTitle(ctx context.Context, conversationID defs.ConversationID, title string) error {
	// 1. 提交对话 ID 和标题，返回保存结果。
	return c.call(ctx, "conversation/setTitle", map[string]any{"conversation_id": conversationID, "title": title}, nil)
}

// SetConversationExecutorSettings 替换对话的完整 Executor 设置。
func (c *clientImpl) SetConversationExecutorSettings(ctx context.Context, conversationID defs.ConversationID, settings defs.ExecutorSettings) (defs.Conversation, error) {
	// 1. 提交新设置，返回保存后的对话记录。
	var result runtimeprotocol.ConversationResult
	err := c.call(ctx, "conversation/setExecutorSettings", &runtimeprotocol.SetExecutorSettingsParams{
		ConversationID:   conversationID,
		ExecutorSettings: settings,
	}, &result)
	return result.Conversation, err
}

// SubmitInput 向根 Session 提交用户输入。
func (c *clientImpl) SubmitInput(ctx context.Context, request *SubmitInputRequest) (defs.ItemID, error) {
	// 1. 为本次输入分配 InputID，保存提交参数。
	params := map[string]any{
		"conversation_id": request.ConversationID,
		"input_id":        defs.InputID(uuid.NewString()),
		"mode":            request.Mode,
		"content":         request.Content,
	}
	// 2. 首次发送前按配置取得连接，未发送的拒绝不进入重试。
	connection, err := c.getConnection(ctx, !c.opt.FailRequestsWhenDisconnected)
	if err != nil {
		return "", err
	}
	// 3. 已发送后断线且允许重试时等待恢复，用同一 InputID 再次提交。
	for {
		var result runtimeprotocol.SubmitInputResult
		err = connection.call(ctx, "conversation/submitInput", params, &result)
		if c.opt.DisableSubmitInputRetry || !errors.Is(err, ErrConnectionLost) {
			return result.ItemID, err
		}
		connection, err = c.getConnection(ctx, true)
		if err != nil {
			return "", err
		}
	}
}

// InterruptTurn 请求中断对话根 Session 的 Turn。
func (c *clientImpl) InterruptTurn(ctx context.Context, conversationID defs.ConversationID) (defs.TurnID, error) {
	// 1. 提交对话 ID，返回被中断的 Turn ID。
	var result runtimeprotocol.InterruptTurnResult
	err := c.call(ctx, "conversation/interruptTurn", map[string]any{"conversation_id": conversationID}, &result)
	return result.TurnID, err
}

// CompactContext 请求根 Session 运行一次上下文压缩。
func (c *clientImpl) CompactContext(ctx context.Context, conversationID defs.ConversationID) (defs.TurnID, error) {
	// 1. 提交对话 ID，返回压缩 Turn 的 ID。
	var result runtimeprotocol.CompactContextResult
	err := c.call(ctx, "conversation/compact", map[string]any{"conversation_id": conversationID}, &result)
	return result.TurnID, err
}

// OpenConversation 请求切换并同步对话，返回首次同步结果。
func (c *clientImpl) OpenConversation(ctx context.Context, request *OpenConversationRequest) (defs.Conversation, error) {
	// 1. 将打开请求交给事件循环。
	reply := make(chan conversationOpenResult, 1)
	if err := c.sendFromCaller(ctx, &conversationOpenRequested{
		request: request,
		reply:   reply,
	}); err != nil {
		return defs.Conversation{}, err
	}
	// 2. 等待首次同步结果、调用取消或客户端停止。
	select {
	case result := <-reply:
		return result.conversation, result.err
	case <-ctx.Done():
		return defs.Conversation{}, ctx.Err()
	case <-c.stopped:
		return defs.Conversation{}, ErrClientStopped
	}
}

// call 等待可用连接，发出请求并返回响应结果。
func (c *clientImpl) call(ctx context.Context, method string, params any, result any) error {
	// 1. 向事件循环请求连接，未连接时按调用方策略等待或立即失败。
	connection, err := c.getConnection(ctx, !c.opt.FailRequestsWhenDisconnected)
	if err != nil {
		return err
	}
	// 2. 通过取得的连接发送请求并等待响应。
	return connection.call(ctx, method, params, result)
}

// notify 在已连接的连接上写出通知。
func (c *clientImpl) notify(ctx context.Context, method string, params any) error {
	// 1. 请求可用连接，不等待重连，未连接时返回 ErrConnectionLost。
	connection, err := c.getConnection(ctx, false)
	if err != nil {
		return err
	}
	// 2. 在取得的连接上写出通知。
	return connection.notify(ctx, method, params)
}

// getConnection 向事件循环取得连接，按参数选择是否等待重连。
func (c *clientImpl) getConnection(ctx context.Context, isWaitingForReconnect bool) (*clientConnection, error) {
	// 1. 提交连接请求和等待选项。
	reply := make(chan *clientConnection, 1)
	if err := c.sendFromCaller(ctx, &connectionRequested{
		reply:                 reply,
		isWaitingForReconnect: isWaitingForReconnect,
	}); err != nil {
		return nil, err
	}
	// 2. 返回连接，或返回未连接、调用取消、客户端停止错误。
	select {
	case connection := <-reply:
		if connection == nil {
			return nil, ErrConnectionLost
		}
		return connection, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.stopped:
		return nil, ErrClientStopped
	}
}

// sendFromCaller 将调用方请求交给事件循环。
func (c *clientImpl) sendFromCaller(ctx context.Context, input clientLoopInput) error {
	// 1. 发送请求，调用取消或客户端停止时返回错误。
	select {
	case c.loopInputs <- input:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return ErrClientStopped
	}
}

// sendFromTask 将后台任务结果交给事件循环。
func (c *clientImpl) sendFromTask(ctx context.Context, input clientLoopInput) {
	// 1. 发送任务结果，运行上下文结束时放弃发送并返回。
	select {
	case c.loopInputs <- input:
	case <-ctx.Done():
	}
}

// emit 向调用方交付一个事件。
func (c *clientImpl) emit(ctx context.Context, event ClientEvent) {
	// 1. 将事件写入事件通道，运行上下文结束时放弃发送并返回。
	select {
	case c.events <- event:
	case <-ctx.Done():
	}
}

// startConnecting 启动延迟连接任务，将连接结果交回事件循环。
func (c *clientImpl) startConnecting(ctx context.Context, delay time.Duration) {
	// 1. 延迟由循环计时，已有拨号时不启动第二次尝试。
	if c.isConnecting {
		return
	}
	if delay > 0 {
		c.reconnectTimer = time.NewTimer(delay)
		return
	}
	c.isConnecting = true
	c.connectionNumber++
	connectionNumber := c.connectionNumber
	// 2. 拨号和初始化在任务中执行，结果交回循环。
	c.tasks.Go(func() {
		connection, serverID, err := c.connect(ctx, connectionNumber)
		if err != nil {
			c.sendFromTask(ctx, &connectionFailed{err: err})
			return
		}
		c.sendFromTask(ctx, &connectionReady{connection: connection, serverID: serverID})
	})
}

// connect 建立并初始化连接，返回连接和服务端 ID。
func (c *clientImpl) connect(ctx context.Context, connectionNumber uint64) (*clientConnection, string, error) {
	// 1. 使用地址和 token 拨号。
	conn, err := dialConnection(ctx, c.opt.URL, c.opt.Token)
	if err != nil {
		return nil, "", err
	}
	// 2. 创建连接读循环，结束后交付连接断开事件。
	connection := newClientConnection(conn, c.loopInputs)
	connection.rpcConnectionID = connectionNumber
	connection.rpcSamples = c.rpcSamples
	connection.rpcDropped = &c.rpcDropped
	connection.rpcDropWake = c.rpcDropWake
	connection.rpcLogPaused = &c.rpcLogPaused
	c.tasks.Go(func() {
		err := connection.Run(ctx)
		c.sendFromTask(ctx, &connectionLost{
			connection: connection,
			err:        err,
		})
	})
	// 3. 发送 initialize；服务端拒绝时返回 ErrInitializeRejected，其他错误原样返回。
	var result runtimeprotocol.InitializeResult
	initializeCtx, stopInitialize := context.WithTimeout(ctx, dialTimeout)
	defer stopInitialize()
	err = connection.call(initializeCtx, runtimeprotocol.InitializeMethod, &runtimeprotocol.InitializeParams{
		ClientInfo:      runtimeprotocol.ClientInfo{Name: "openvaala-tui"},
		ProtocolVersion: runtimeprotocol.ProtocolVersion,
	}, &result)
	if err != nil {
		_ = conn.CloseNow()
		var protocolErr *ResponseError
		if errors.As(err, &protocolErr) {
			return nil, "", fmt.Errorf("%w: %w", ErrInitializeRejected, err)
		}
		return nil, "", err
	}
	return connection, result.ServerID, nil
}

// handleConnectionRequested 回复可用连接，或登记等待重连的调用。
func (c *clientImpl) handleConnectionRequested(input *connectionRequested) {
	// 1. 当前连接仍在读取时立即回复。
	if c.connection != nil && !c.connection.isClosed() {
		input.reply <- c.connection
		return
	}
	// 2. 未连接时按参数回复 nil，或登记等待下一次连接。
	if !input.isWaitingForReconnect {
		input.reply <- nil
		return
	}
	c.connectionWaiters = append(c.connectionWaiters, input.reply)
}

// handleConnectionReady 启用新连接，唤醒调用并同步已打开的对话。
func (c *clientImpl) handleConnectionReady(ctx context.Context, input *connectionReady) {
	// 1. 结束本次尝试，新连接已关闭时安排重连。
	c.isConnecting = false
	if input.connection.isClosed() {
		c.scheduleReconnect(ctx, ErrConnectionLost)
		return
	}
	// 2. 保存连接，交付连接成功事件并回复等待的调用。
	c.connection = input.connection
	c.emit(ctx, &ConnectionChanged{
		State:    ConnectionConnected,
		ServerID: input.serverID,
	})
	for _, waiter := range c.connectionWaiters {
		waiter <- c.connection
	}
	c.connectionWaiters = nil
	// 3. 恢复全局订阅；没有打开对话时清零失败次数，否则恢复内容同步。
	c.startRunningSubscription(ctx)
	if c.openedConversation == nil {
		c.failedConnectAttempts = 0
		return
	}
	c.startSync(ctx, nil)
}

// handleConnectionFailed 处理连接失败并决定是否重连。
func (c *clientImpl) handleConnectionFailed(ctx context.Context, input *connectionFailed) error {
	// 1. 结束本次尝试；认证失败和 initialize 被拒绝时返回错误，其他失败安排退避重连。
	c.isConnecting = false
	if errors.Is(input.err, ErrUnauthorized) || errors.Is(input.err, ErrInitializeRejected) {
		return input.err
	}
	c.scheduleReconnect(ctx, input.err)
	return nil
}

// handleConnectionLost 处理当前连接断开，安排重连。
func (c *clientImpl) handleConnectionLost(ctx context.Context, input *connectionLost) {
	// 1. 忽略旧连接的结束通知，清除当前连接并安排重连。
	if input.connection != c.connection {
		return
	}
	c.connection = nil
	c.resetRunningSubscription()
	if c.wantsRunningConversations {
		c.emit(ctx, &RunningConversationsChanged{Err: input.err})
	}
	c.scheduleReconnect(ctx, input.err)
}

// scheduleReconnect 记录连接失败，交付重连事件并安排下次连接。
func (c *clientImpl) scheduleReconnect(ctx context.Context, reason error) {
	// 1. 累计失败次数，计算延迟并记录断开原因。
	c.failedConnectAttempts++
	delay := getReconnectDelay(c.failedConnectAttempts)
	telemetry.GetLogger(ctx).Warn("runtime connection lost, reconnecting",
		"error", reason, "attempt", c.failedConnectAttempts, "retry_in_ms", delay.Milliseconds())
	// 2. 交付重连次数和等待时长，启动延迟连接任务。
	c.emit(ctx, &ConnectionChanged{
		State:   ConnectionReconnecting,
		Err:     reason,
		Attempt: c.failedConnectAttempts,
		RetryIn: delay,
	})
	c.startConnecting(ctx, delay)
}

// handleConversationOpenRequested 切换要跟随的对话，连接可用时开始同步。
func (c *clientImpl) handleConversationOpenRequested(ctx context.Context, input *conversationOpenRequested) {
	// 1. 调用方要求离线立即失败时，不改变原阅读目标。
	if c.opt.FailRequestsWhenDisconnected && (c.connection == nil || c.connection.isClosed()) {
		input.reply <- conversationOpenResult{err: ErrConnectionLost}
		return
	}
	// 2. 结束尚未完成的上一次打开请求。
	previous := c.openedConversation
	if previous != nil && previous.openReply != nil {
		previous.openReply <- conversationOpenResult{err: errors.New("another conversation was opened")}
	}
	// 3. 保存新对话的跟随设置和响应通道。
	c.openedConversation = &openedConversation{
		id:          input.request.ConversationID,
		environment: slices.Clone(input.request.Environment),
		sessionID:   input.request.SessionID,
		skipHistory: input.request.SkipHistory,
		openReply:   input.reply,
	}
	// 4. 已连接时启动同步并退订原对话。
	if c.connection != nil {
		c.startSync(ctx, previous)
	}
}

// startSync 取消旧同步，启动打开对话的订阅和补齐任务。
func (c *clientImpl) startSync(ctx context.Context, previous *openedConversation) {
	// 1. 取消旧任务，创建新的同步上下文和批次编号。
	if c.cancelSync != nil {
		c.cancelSync()
	}
	syncCtx, cancelSync := context.WithCancel(ctx)
	c.cancelSync = cancelSync
	c.syncGeneration++
	// 2. 标记对话正在同步，清空暂存通知并读取本次同步参数。
	open := c.openedConversation
	open.isSyncing = true
	open.syncGeneration = c.syncGeneration
	open.pendingNotifications = nil
	generation, connection, conversationID, fromOrdinal := c.syncGeneration, c.connection, open.id, open.lastSavedOrdinal
	environment, sessionID, skipHistory := open.environment, open.sessionID, open.skipHistory
	// 3. 记录要退订的原对话，启动同步任务并交付结果。
	var previousTarget *syncTarget
	if previous != nil && previous.sessionID != "" {
		previousTarget = &syncTarget{
			conversationID: previous.id,
			sessionID:      previous.sessionID,
		}
	}
	c.tasks.Go(func() {
		result, err := syncConversation(syncCtx, connection, previousTarget, conversationID, sessionID, environment, fromOrdinal, skipHistory)
		c.sendFromTask(ctx, &syncFinished{
			generation: generation,
			result:     result,
			err:        err,
		})
	})
}

// handleSyncFinished 处理同步成败，交付同步记录和暂存通知。
func (c *clientImpl) handleSyncFinished(ctx context.Context, input *syncFinished) {
	// 1. 忽略过期结果，结束本次同步上下文。
	open := c.openedConversation
	if open == nil || input.generation != open.syncGeneration {
		return
	}
	c.cancelSync()
	// 2. 断线错误等重连；首次打开失败时回复调用方，重新同步被 serve 拒绝时交付 ConversationSyncFailed，这两种都放弃这个对话；
	// 其他错误关闭连接，等重连后重新同步。
	if input.err != nil {
		if errors.Is(input.err, ErrConnectionLost) {
			return
		}
		var responseErr *ResponseError
		var runtimeErr *serverRuntimeError
		switch {
		case open.openReply != nil:
			open.openReply <- conversationOpenResult{err: input.err}
		case errors.As(input.err, &responseErr), errors.As(input.err, &runtimeErr):
			c.emit(ctx, &ConversationSyncFailed{
				ConversationID: open.id,
				Err:            input.err,
			})
		default:
			if c.connection != nil {
				_ = c.connection.conn.CloseNow()
			}
			return
		}
		c.openedConversation = nil
		c.failedConnectAttempts = 0
		return
	}
	// 3. 保存同步位置和跟随 Session，清零失败次数，交付对话与 Session 记录。
	result := input.result
	open.sessionID = result.sessionID
	open.skipHistory = false
	open.lastSavedOrdinal = result.lastSavedOrdinal
	open.isSyncing = false
	c.failedConnectAttempts = 0
	c.emit(ctx, &ConversationSynced{
		ConversationID: open.id,
		Conversation:   result.conversation,
		SessionInfos:   result.sessionInfos,
	})
	c.emit(ctx, &SessionSynced{
		SessionID:        result.sessionID,
		ConversationID:   open.id,
		Items:            result.items,
		StreamingItems:   result.streamingItems,
		PendingInputs:    result.pendingInputs,
		LastSavedOrdinal: result.lastSavedOrdinal,
	})
	// 4. 回复首次打开请求，再按到达顺序交付暂存通知。
	if open.openReply != nil {
		open.openReply <- conversationOpenResult{conversation: result.conversation}
		open.openReply = nil
	}
	pending := open.pendingNotifications
	open.pendingNotifications = nil
	for _, notification := range pending {
		c.deliverNotification(ctx, open, notification)
	}
}

// handleNotificationReceived 接收当前连接通知，直接交付 MCP 消息和终端通知，暂存或交付对话通知。
func (c *clientImpl) handleNotificationReceived(ctx context.Context, input *notificationReceived) {
	// 1. 忽略旧连接通知，解码当前连接消息。
	if input.connection != c.connection {
		return
	}
	notification := &receivedNotification{method: input.message.Method}
	if err := json.Unmarshal(input.message.Params, &notification.params); err != nil {
		telemetry.GetLogger(ctx).Warn("notification dropped", "rpc.method", input.message.Method, "error", err)
		return
	}
	// 2. MCP 转发通知和终端通知直接交付给调用方。
	switch params := notification.params; notification.method {
	case "clientMCP/connect":
		c.emit(ctx, &ClientMCPConnectRequested{
			StreamID: params.StreamID,
			ServerID: params.ServerID,
		})
		return
	case "clientMCP/message":
		c.emit(ctx, &ClientMCPMessageReceived{
			StreamID: params.StreamID,
			Message:  params.Message,
		})
		return
	case "clientMCP/disconnect":
		c.emit(ctx, &ClientMCPDisconnectRequested{StreamID: params.StreamID})
		return
	case "terminal/output":
		c.emit(ctx, &TerminalOutput{
			TerminalID: params.TerminalID,
			Data:       params.Data,
		})
		return
	case "terminal/exited":
		c.emit(ctx, &TerminalExited{
			TerminalID: params.TerminalID,
			ExitCode:   params.ExitCode,
			Error:      params.Error,
		})
		return
	}
	// 3. 全局集合不依赖已打开的对话。
	if notification.method == "conversation/runningChanged" || notification.method == "conversation/runningClosed" {
		c.handleRunningNotification(ctx, notification.method, input.message.Params)
		return
	}
	// 4. 忽略其他对话通知；同步期间暂存通知，旧订阅关闭时删除这个订阅暂存的通知。
	open := c.openedConversation
	if open == nil || notification.params.ConversationID != open.id {
		return
	}
	if open.isSyncing {
		if notification.method == "subscription/closed" {
			open.pendingNotifications = slices.DeleteFunc(open.pendingNotifications, func(pending *receivedNotification) bool {
				return pending.params.Subscription == notification.params.Subscription
			})
			return
		}
		open.pendingNotifications = append(open.pendingNotifications, notification)
		return
	}
	// 5. 不在同步过程中时立即交付对话通知。
	c.deliverNotification(ctx, open, notification)
}

// deliverNotification 处理订阅关闭通知，按来源交付内容或状态事件。
func (c *clientImpl) deliverNotification(ctx context.Context, open *openedConversation, notification *receivedNotification) {
	// 1. 订阅被关闭时重新同步。
	if notification.method == "subscription/closed" {
		c.startSync(ctx, nil)
		return
	}
	// 2. 按 Session 或对话订阅分派通知。
	switch notification.params.Subscription {
	case "session":
		c.deliverContentNotification(ctx, open, notification)
	case "conversation":
		c.deliverStatusNotification(ctx, open, notification)
	}
}

// deliverContentNotification 将跟随 Session 的内容通知转换为客户端事件。
func (c *clientImpl) deliverContentNotification(ctx context.Context, open *openedConversation, notification *receivedNotification) {
	// 1. 将 Item 开始、增量、完成和中断通知转为 ItemChanged。
	params := notification.params
	if params.SessionID != open.sessionID {
		return
	}
	if eventType, found := itemEventTypes[notification.method]; found {
		c.emit(ctx, &ItemChanged{SessionID: params.SessionID,
			ConversationID: open.id,
			Type:           eventType,
			TurnID:         params.TurnID,
			ItemIndex:      params.ItemIndex,
			Item:           params.Item,
			Delta:          params.Delta,
		})
		return
	}
	// 2. 交付未发送过的已保存 Item、工具进度、hook 和 Turn 事件。
	switch notification.method {
	case "session/pendingInputsUpdated":
		c.emit(ctx, &SessionPendingInputsChanged{SessionID: params.SessionID, ConversationID: open.id, PendingInputs: params.PendingInputs})
	case "items/saved":
		var items []runtimeprotocol.LoggedItem
		for _, logged := range params.Items {
			if logged.Ordinal > open.lastSavedOrdinal {
				items = append(items, logged)
				open.lastSavedOrdinal = logged.Ordinal
			}
		}
		if len(items) > 0 {
			c.emit(ctx, &ItemsSaved{SessionID: params.SessionID,
				ConversationID: open.id,
				TurnID:         params.TurnID,
				Items:          items,
			})
		}
	case "item/toolProgress":
		if params.ToolProgress != nil {
			c.emit(ctx, &ToolProgress{SessionID: params.SessionID,
				ConversationID: open.id,
				TurnID:         params.TurnID,
				ItemIndex:      params.ItemIndex,
				Progress:       *params.ToolProgress,
			})
		}
	case "hook/started", "hook/completed":
		if params.HookRun != nil {
			c.emit(ctx, &HookRunChanged{SessionID: params.SessionID,
				ConversationID: open.id,
				TurnID:         params.TurnID,
				Run:            params.HookRun,
			})
		}
	case "turn/started":
		c.emit(ctx, &TurnStarted{SessionID: params.SessionID,
			ConversationID: open.id,
			TurnID:         params.TurnID,
		})
	case "turn/finished":
		c.emit(ctx, &TurnFinished{SessionID: params.SessionID,
			ConversationID: open.id,
			TurnID:         params.TurnID,
			Status:         params.Status,
			InputItemIDs:   params.InputItemIDs,
			Error:          params.Error,
		})
	}
}

// deliverStatusNotification 将对话状态通知转换为标题和 Session 状态事件。
func (c *clientImpl) deliverStatusNotification(ctx context.Context, open *openedConversation, notification *receivedNotification) {
	// 1. 按通知方法交付标题或完整 Session 运行信息。
	switch notification.method {
	case "conversation/titleUpdated":
		c.emit(ctx, &ConversationTitleChanged{
			ConversationID: open.id,
			Title:          notification.params.Title,
		})
	case "session/statusUpdated":
		if notification.params.Info != nil {
			c.emit(ctx, &SessionStatusChanged{
				ConversationID: open.id,
				Info:           *notification.params.Info,
			})
		}
	}
}

// syncTarget 是要退订的旧对话订阅。
type syncTarget struct {
	conversationID defs.ConversationID
	sessionID      defs.SessionID
}

// syncConversation 打开对话，重建订阅，补齐日志并返回同步结果。
func syncConversation(
	ctx context.Context,
	connection *clientConnection,
	previous *syncTarget,
	conversationID defs.ConversationID,
	sessionID defs.SessionID,
	environment []string,
	fromOrdinal storage.Ordinal,
	skipHistory bool,
) (*syncResult, error) {
	// 1. 退订原对话的状态和 Session 内容。
	if previous != nil {
		if err := unsubscribeConversation(ctx, connection, previous); err != nil {
			return nil, err
		}
	}
	// 2. 使用请求环境变量打开对话。
	var read runtimeprotocol.ConversationResult
	openParams := &runtimeprotocol.OpenConversationParams{
		ConversationID: conversationID,
		Environment:    environment,
	}
	if err := connection.call(ctx, "conversation/open", openParams, &read); err != nil {
		return nil, err
	}
	// 3. 选择跟随的 Session，退订它的旧订阅并重新订阅对话状态；标题用订阅登记时的值。
	target := &syncTarget{
		conversationID: conversationID,
		sessionID:      cmp.Or(sessionID, read.Conversation.RootSessionID),
	}
	if err := unsubscribeConversation(ctx, connection, target); err != nil {
		return nil, err
	}
	var status runtimeprotocol.SubscribeConversationResult
	if err := connection.call(ctx, "conversation/subscribe", map[string]any{"conversation_id": conversationID}, &status); err != nil {
		return nil, err
	}
	read.Conversation.Title = status.Title
	// 4. 订阅 Session 内容，取得已保存位置和生成中 Item。
	var content runtimeprotocol.SubscribeSessionResult
	sessionParams := map[string]any{"conversation_id": conversationID, "session_id": target.sessionID}
	if err := connection.call(ctx, "session/subscribe", sessionParams, &content); err != nil {
		return nil, err
	}
	// 5. SkipHistory 为 true 时将起点改为订阅末尾，不补历史；否则补齐指定位置之后的日志，再返回对话、Session 状态和内容。
	if skipHistory {
		fromOrdinal = content.LastSavedOrdinal
	}
	items, err := listItemsBetween(ctx, connection, target, fromOrdinal, content.LastSavedOrdinal)
	if err != nil {
		return nil, err
	}
	return &syncResult{
		conversation:     read.Conversation,
		sessionID:        target.sessionID,
		sessionInfos:     status.Sessions,
		items:            items,
		streamingItems:   content.StreamingItems,
		pendingInputs:    content.PendingInputs,
		lastSavedOrdinal: content.LastSavedOrdinal,
	}, nil
}

// unsubscribeConversation 退订对话状态和跟随的 Session 内容。
func unsubscribeConversation(ctx context.Context, connection *clientConnection, target *syncTarget) error {
	// 1. 退订对话状态，失败时返回。
	if err := connection.call(ctx, "conversation/unsubscribe", map[string]any{"conversation_id": target.conversationID}, nil); err != nil {
		return err
	}
	// 2. 退订指定 Session 的内容。
	params := map[string]any{"conversation_id": target.conversationID, "session_id": target.sessionID}
	return connection.call(ctx, "session/unsubscribe", params, nil)
}

// listItemsBetween 按流水号正序读取 from 之后、到 to 为止的 Session 日志。
func listItemsBetween(ctx context.Context, connection *clientConnection, target *syncTarget, from, to storage.Ordinal) ([]runtimeprotocol.LoggedItem, error) {
	// 1. 从起点之后分页读取日志。
	items := []runtimeprotocol.LoggedItem{}
	for cursor := from; cursor < to; {
		var page runtimeprotocol.ListItemsResult
		params := map[string]any{
			"conversation_id": target.conversationID,
			"session_id":      target.sessionID,
			"cursor":          cursor,
			"limit":           historyPageSize,
		}
		if err := connection.call(ctx, "session/items/list", params, &page); err != nil {
			return nil, err
		}
		// 2. 空页时结束，按顺序收集不超过终点的 Item 并推进游标。
		if len(page.Items) == 0 {
			break
		}
		for _, logged := range page.Items {
			if logged.Ordinal > to {
				return items, nil
			}
			items = append(items, logged)
			cursor = logged.Ordinal
		}
	}
	return items, nil
}

// GetSkills 读取已有对话或草稿 Agent 的 Skill 目录。
func (c *clientImpl) GetSkills(ctx context.Context, request *GetSkillsRequest) ([]skill.SkillInfo, error) {
	// 1. 提交对话或 Agent、Executor 设置，返回 Skill 列表。
	var result []skill.SkillInfo
	err := c.call(ctx, "skill/list", &runtimeprotocol.ListSkillsParams{
		ConversationID:   request.ConversationID,
		AgentName:        request.AgentName,
		ExecutorSettings: request.ExecutorSettings,
	}, &result)
	return result, err
}

// SteerInput 提交排队输入转补充操作。
func (c *clientImpl) SteerInput(ctx context.Context, conversationID defs.ConversationID, itemID defs.ItemID, turnID defs.TurnID) error {
	// 1. 发送一次操作请求并返回结果。
	return c.call(ctx, "conversation/steerInput", &runtimeprotocol.SteerInputParams{
		ConversationID: conversationID,
		ItemID:         itemID,
		TurnID:         turnID,
	}, nil)
}

// InterruptTurnWithInput 提交优先输入和指定轮。
func (c *clientImpl) InterruptTurnWithInput(ctx context.Context, conversationID defs.ConversationID, turnID defs.TurnID, itemID defs.ItemID) error {
	// 1. 发送一次操作请求并返回结果。
	return c.call(ctx, "conversation/interruptTurnWithInput", &runtimeprotocol.InterruptTurnWithInputParams{
		ConversationID: conversationID,
		ItemID:         itemID,
		TurnID:         turnID,
	}, nil)
}

// WithdrawInput 提交原子取回或撤回操作。
func (c *clientImpl) WithdrawInput(ctx context.Context, conversationID defs.ConversationID, itemID defs.ItemID, returnContent bool) ([]*defs.Content, error) {
	// 1. 发送一次请求并返回用户消息内容。
	var result runtimeprotocol.WithdrawInputResult
	err := c.call(ctx, "conversation/withdrawInput", &runtimeprotocol.WithdrawInputParams{
		ConversationID: conversationID,
		ItemID:         itemID,
		ReturnContent:  returnContent,
	}, &result)
	return result.Content, err
}

// Reconnect 跳过普通断线的等待，由 Run 合并正在进行的连接尝试。
func (c *clientImpl) Reconnect(ctx context.Context) error {
	// 1. 交给循环决定是否立即拨号。
	reply := make(chan struct{}, 1)
	if err := c.sendFromCaller(ctx, &reconnectRequested{reply: reply}); err != nil {
		return err
	}
	// 2. 等待循环接收或调用结束。
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return ErrClientStopped
	}
}

// SubscribeRunningConversations 登记全局订阅意图，初始集合和后续变化经 Events 交付。
func (c *clientImpl) SubscribeRunningConversations(ctx context.Context) error {
	// 1. 将请求交给唯一事件循环，重复调用合并。
	reply := make(chan struct{}, 1)
	if err := c.sendFromCaller(ctx, &runningSubscribeRequested{reply: reply}); err != nil {
		return err
	}
	// 2. 等待订阅意图登记完成，不等待网络返回。
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return ErrClientStopped
	}
}

// resetRunningSubscription 作废当前全局订阅请求、暂存通知和重试等待，保留调用方订阅意图。
func (c *clientImpl) resetRunningSubscription() {
	// 1. 取消网络请求和重试，清除当前连接的订阅记录。
	c.runningSubscriptionGeneration++
	if c.cancelRunningSubscribe != nil {
		c.cancelRunningSubscribe()
		c.cancelRunningSubscribe = nil
	}
	if c.runningSubscriptionRetryTimer != nil {
		c.runningSubscriptionRetryTimer.Stop()
		c.runningSubscriptionRetryTimer = nil
	}
	c.isSubscribingRunningConversations = false
	c.isRunningConversationsSubscribed = false
	c.stagedRunningConversations = nil
}

// startRunningSubscription 在当前连接建立全局运行订阅，网络响应交回循环。
func (c *clientImpl) startRunningSubscription(ctx context.Context) {
	// 1. 只有已启用且已连接时发起一次订阅。
	if !c.wantsRunningConversations || c.connection == nil || c.isSubscribingRunningConversations || c.isRunningConversationsSubscribed {
		return
	}
	c.isSubscribingRunningConversations = true
	c.runningSubscriptionGeneration++
	generation, connection := c.runningSubscriptionGeneration, c.connection
	requestCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	c.cancelRunningSubscribe = cancel
	// 2. 重试先清理可能残留的旧订阅，再订阅；请求不占用事件循环。
	retry := c.failedRunningSubscriptions > 0
	c.tasks.Go(func() {
		defer cancel()
		var result runtime.RunningConversations
		var err error
		if retry {
			err = connection.call(requestCtx, "conversation/running/unsubscribe", &runtimeprotocol.EmptyParams{}, nil)
			if err == nil {
				prepared := make(chan bool, 1)
				c.sendFromTask(requestCtx, &runningSubscriptionPrepared{connection: connection, generation: generation, reply: prepared})
				select {
				case isCurrent := <-prepared:
					if !isCurrent {
						return
					}
				case <-requestCtx.Done():
					err = requestCtx.Err()
				}
			}
		}
		if err == nil {
			err = connection.call(requestCtx, "conversation/running/subscribe", &runtimeprotocol.EmptyParams{}, &result)
		}
		c.sendFromTask(ctx, &runningConversationsSubscribed{connection: connection, generation: generation, result: result, err: err})
	})
}

// handleRunningSubscribed 应用全局订阅响应，再交付请求期间暂存的最新完整集合。
func (c *clientImpl) handleRunningSubscribed(ctx context.Context, result *runningConversationsSubscribed) {
	// 1. 丢弃旧连接、旧订阅的响应。
	if result.connection != c.connection || result.generation != c.runningSubscriptionGeneration {
		return
	}
	c.isSubscribingRunningConversations = false
	c.cancelRunningSubscribe = nil
	// 2. 失败时标为未知，保持内容订阅并安排全局订阅重试。
	if result.err != nil {
		c.stagedRunningConversations = nil
		c.failedRunningSubscriptions++
		c.emit(ctx, &RunningConversationsChanged{Err: result.err})
		c.runningSubscriptionRetryTimer = time.NewTimer(getReconnectDelay(c.failedRunningSubscriptions))
		return
	}
	// 3. 初始集合先交付，随后交付暂存通知；每条都独立于 Client 内部状态。
	c.isRunningConversationsSubscribed = true
	c.failedRunningSubscriptions = 0
	c.emit(ctx, &RunningConversationsChanged{IsKnown: true, ConversationIDs: result.result.ConversationIDs})
	if c.stagedRunningConversations != nil {
		c.emit(ctx, &RunningConversationsChanged{IsKnown: true, ConversationIDs: c.stagedRunningConversations.ConversationIDs})
		c.stagedRunningConversations = nil
	}
}

// handleRunningNotification 接收全局运行集合或订阅关闭通知。
func (c *clientImpl) handleRunningNotification(ctx context.Context, method string, params json.RawMessage) {
	// 1. 未启用订阅时忽略通知，关闭时作废本次同步并安排重试。
	if !c.wantsRunningConversations {
		return
	}
	if method == "conversation/runningClosed" {
		var closed runtimeprotocol.RunningConversationsClosedParams
		if err := json.Unmarshal(params, &closed); err != nil {
			return
		}
		c.resetRunningSubscription()
		c.failedRunningSubscriptions++
		c.emit(ctx, &RunningConversationsChanged{Err: errors.New(closed.Reason)})
		c.runningSubscriptionRetryTimer = time.NewTimer(getReconnectDelay(c.failedRunningSubscriptions))
		return
	}
	// 2. 订阅响应还没应用时保留最新完整集合，之后直接交付。
	var running runtime.RunningConversations
	if err := json.Unmarshal(params, &running); err != nil {
		return
	}
	if c.isSubscribingRunningConversations {
		c.stagedRunningConversations = &running
		return
	}
	if c.isRunningConversationsSubscribed {
		c.emit(ctx, &RunningConversationsChanged{IsKnown: true, ConversationIDs: running.ConversationIDs})
	}
}
