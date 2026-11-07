package appserver

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// connectionThread 是连接跟随的一个 Thread：一个只跟随这个 Thread 对应 Session 的 Client，以及把 Client 事件翻译成 Codex 通知
// 时已经发出内容的记录。Run 是唯一读写下面运行状态的 goroutine；连接经 requests 交来请求，经 opened 等它首次同步完成，
// 经 unsubscribed 让它停止跟随。根 Thread 还从对话状态里找出需要自动跟随的子 Session，经 subAgentDiscoveries 交给连接。
type connectionThread struct {
	// threadID 是 Codex 的 Thread id：根 Thread 是 ConversationID，子 Thread 是子 SessionID。
	threadID       string
	conversationID defs.ConversationID
	// sessionID 是这个 Thread 跟随的 Session：根 Thread 跟随根 Session，子 Thread 跟随子 Session。
	sessionID     defs.SessionID
	runtimeServer RuntimeServerInfo
	telemetry     *telemetry.Telemetry
	// version 写进 Thread.cliVersion。
	version string
	// pathMappings 把对话归属的目录换回客户端路径，写进 Thread 的 cwd。
	pathMappings config.PathMappings
	// isWindowsClient 开始跟随时连接认为客户端是不是 Windows 上的？换回客户端路径时只用和它平台一致的映射。
	isWindowsClient bool
	// outgoing 是连接的写出队列。
	outgoing chan<- codexMessage
	// connectionDone 在连接结束时关闭。
	connectionDone <-chan struct{}
	// subAgentDiscoveries 是连接接收根 Thread 发现的子 Session 的通道，由连接的主循环读取；只有根 Thread 写入。
	subAgentDiscoveries chan<- *subAgentSessionDiscovered
	// isHistoryPaged 客户端是不是自己翻页读历史（thread/resume、thread/fork 带 excludeTurns）？是时首次同步时在跑的那一轮里已经保存、
	// 还在执行的工具调用由翻页给出（inProgress），接入时只登记、不再发 item/started，之后的输出和结果照常发出。
	isHistoryPaged bool
	// isEphemeral 这是不是本连接用 thread/start 或 thread/fork 建的临时 Thread？是时回复打开请求的 Thread 对象带 ephemeral。
	isEphemeral bool
	// requests 是连接交来的请求。
	requests chan connectionThreadRequest
	// opened 交出首次同步的结果，只写一次。
	opened chan connectionThreadOpened
	// unsubscribed 由连接在退订这个 Thread 时关闭。
	unsubscribed chan struct{}
	// done 在 Run 返回前关闭。
	done chan struct{}

	// 以下字段只由 Run 读写。

	// client 是本 Thread 自己的 Client，Run 开始时创建，在 tasks 里运行，Run 返回前取消并等待。
	client runtimeclient.Client
	// queuedClientEvents 是等待 Client 调用期间收到、还没处理的事件，按到达顺序处理。
	queuedClientEvents []runtimeclient.ClientEvent
	// conversationRecord 是最近一次同步时的对话记录。
	conversationRecord defs.Conversation
	// conversationSessions 是对话里各 Session 最近的运行信息（来自 ConversationSynced 和 SessionStatusChanged）：agent 路径、
	// 子 Thread 对象的父 Thread 和层数取自其中的 Session 记录，根 Thread 还据此找出要跟随的子 Session。
	conversationSessions map[defs.SessionID]runtime.SessionRuntimeInfo
	// executors 是跟随的 Session 最近读到的 Executor，按配置顺序，第一个是默认 Executor：翻译工具调用用它；对话不装配 Executor 时为空。
	executors []conversation.ExecutorDetails
	// agentPaths 是对话里各 Session 的 agent 路径，对话里出现新的 Session 时重算。
	agentPaths map[defs.SessionID]string
	// translator 按 executors 和 agentPaths 翻译 Item，其中之一变化时重建。
	translator *codexItemTranslator
	// pendingSubAgentDiscoveries 是根 Thread 已经发现、还没交给连接的子 Session，按发现顺序交出。
	pendingSubAgentDiscoveries []*subAgentSessionDiscovered
	// isSynced 首次同步是否已经处理？
	isSynced bool
	// followStartOrdinal 是首次同步交付到的位置（跟随起点）：之前的历史由客户端翻页读到，之后的由实时推送发出。
	// 它和 followStartActiveTurnID 都在首次同步时写入，之后只读，经 opened 交给连接。
	followStartOrdinal storage.Ordinal
	// followStartActiveTurnID 是首次同步时在跑的 Turn，只有它里面已经保存、还在执行的工具调用由接入时的实时推送补发。
	followStartActiveTurnID defs.TurnID
	// syncedSessionInfo 是最近一次同步（ConversationSynced）时跟随的 Session 的运行信息：紧随其后的 SessionSynced 据此补发
	// 同步之前开始和结束的 Turn。
	syncedSessionInfo runtime.SessionRuntimeInfo
	// isConversationDropped Client 是否已经放弃这个对话（重新同步被 serve 拒绝）？之后的 turn/start 先重新打开。
	isConversationDropped bool
	// selectedModel 是跟随的 Session 当前所选的模型。
	selectedModel defs.ModelSelection
	// activeTurnID 是客户端看到的进行中的 Turn：已经发了它的 turn/started、还没发 turn/completed，没有时为空。
	activeTurnID defs.TurnID
	// lastCompletedTurnID 是最近一次发了 turn/completed 的 Turn：TurnID 是 UUIDv7，按字符串比较就是创建先后，
	// 不大于它的 Turn 不再发 turn/completed（同步时补发过的不会随实时事件再发一次）。
	lastCompletedTurnID defs.TurnID
	// threadStatusType 是最近一次告诉客户端的 Thread 状态。
	threadStatusType string
	// reportedTokenUsage 是最近一次发出的用量，还没发过时为 nil。
	reportedTokenUsage *codexThreadTokenUsage

	// 以下四项是本轮的显示记录，每轮开始时清空。

	// streamingItems 是本轮输出中各位置（ItemIndex）的显示记录。
	streamingItems map[int]*threadItemDisplay
	// displayedItems 是本轮已经处理过的 Item 和它的显示记录，重复交付时据此只补发缺少的部分。
	displayedItems map[defs.ItemID]*threadItemDisplay
	// toolCallsByCallID 是本轮的 ToolCall，ToolResult 和工具进度据此找到调用。
	toolCallsByCallID map[string]*defs.Item
	// openReplies 是本轮已经 item/started、还没 item/completed 的回复和推理，按开始先后。
	openReplies []openReply

	// lastDisplayedOrdinal 是已经显示过的已保存 Item 的最大流水号，重新同步时只显示这之后的。
	lastDisplayedOrdinal storage.Ordinal
	// historyEndOrdinal 是本连接用打开响应里的起点游标翻历史时读到的最后位置：开始跟随时是跟随起点；之后每次回 thread/resume 响应时，
	// 更新为已经实时推给客户端的位置（lastDisplayedOrdinal）。之后存盘的 Item 由实时推送接上；普通分页照常读存储。
	historyEndOrdinal storage.Ordinal
	// clientUserMessageIDs 是本连接提交的输入 ItemID 对应的 clientUserMessageId，显示这条 userMessage 时填进 clientId 后删掉。
	clientUserMessageIDs map[defs.ItemID]string
	// pendingTurnStart 是在等 TurnID 的 turn/start，没有时为 nil。
	pendingTurnStart *pendingTurnStart
	// isOpenDelivered 连接是否已经回复了打开这个 Thread 的请求？
	isOpenDelivered bool
	// heldNotifications 是暂存的通知：还没回复打开请求，或 pendingTurnStart 不为空时，本 Thread 的通知按顺序存在这里。
	heldNotifications []codexMessage
	// tasks 是 Client 的 Run 和正在执行的 Client 调用；Run 返回前等待。
	tasks sync.WaitGroup
}

// connectionThreadOpt 由连接在开始跟随时填写：要连的 serve、写出队列、要跟随的 Session 和读历史的方式。
type connectionThreadOpt struct {
	RuntimeServer RuntimeServerInfo
	Telemetry     *telemetry.Telemetry
	Version       string
	PathMappings  config.PathMappings
	// IsWindowsClient 连接此时认为客户端是不是 Windows 上的？
	IsWindowsClient bool
	// Outgoing 是连接的写出队列。
	Outgoing chan<- codexMessage
	// ConnectionDone 在连接结束时关闭，之后不再写出。
	ConnectionDone <-chan struct{}
	// SubAgentDiscoveries 是连接接收根 Thread 发现的子 Session 的通道。
	SubAgentDiscoveries chan<- *subAgentSessionDiscovered
	ThreadID            string
	ConversationID      defs.ConversationID
	SessionID           defs.SessionID
	// StartedStatusType 是连接已经在 thread/started 里告诉客户端的状态（自动跟随的子 Thread），没有发过时为空：
	// 首次同步状态不同时发送 thread/status/changed。
	StartedStatusType string
	// IsHistoryPaged 客户端是不是自己翻页读历史（thread/resume、thread/fork 带 excludeTurns）？
	IsHistoryPaged bool
	// IsEphemeral 这是不是本连接用 thread/start 或 thread/fork 建的临时 Thread？
	IsEphemeral bool
}

// newConnectionThread 创建跟随指定 Session 的 Thread。
func newConnectionThread(opt *connectionThreadOpt) *connectionThread {
	// 1. 保存连接和展示设置，创建请求、打开结果、退订及结束通知通道。
	return &connectionThread{
		threadID:            opt.ThreadID,
		conversationID:      opt.ConversationID,
		sessionID:           opt.SessionID,
		runtimeServer:       opt.RuntimeServer,
		telemetry:           opt.Telemetry,
		version:             opt.Version,
		pathMappings:        opt.PathMappings,
		isWindowsClient:     opt.IsWindowsClient,
		outgoing:            opt.Outgoing,
		connectionDone:      opt.ConnectionDone,
		subAgentDiscoveries: opt.SubAgentDiscoveries,
		isHistoryPaged:      opt.IsHistoryPaged,
		isEphemeral:         opt.IsEphemeral,
		threadStatusType:    opt.StartedStatusType,
		requests:            make(chan connectionThreadRequest),
		opened:              make(chan connectionThreadOpened, 1),
		unsubscribed:        make(chan struct{}),
		done:                make(chan struct{}),
	}
}

// Run 运行 Thread 的 Client，完成首次同步后处理请求和实时事件，直到退订、打开失败、Client 断线或 ctx 结束。
func (t *connectionThread) Run(ctx context.Context) (err error) {
	// 1. 初始化 Session、Item 和用户输入的显示记录，创建 Client；创建失败时经 opened 交出错误并返回 nil。
	defer close(t.done)
	t.conversationSessions = make(map[defs.SessionID]runtime.SessionRuntimeInfo)
	t.streamingItems = make(map[int]*threadItemDisplay)
	t.displayedItems = make(map[defs.ItemID]*threadItemDisplay)
	t.toolCallsByCallID = make(map[string]*defs.Item)
	t.clientUserMessageIDs = make(map[defs.ItemID]string)
	clientOpt := runtimeclient.NewClientOpt(t.runtimeServer.URL, t.runtimeServer.Token)
	clientOpt.Telemetry = t.telemetry
	if t.client, err = runtimeclient.NewClient(clientOpt); err != nil {
		t.opened <- connectionThreadOpened{err: err}
		return nil
	}
	// 2. 运行 Client，登记取消、等待任务和回复未完成 turn/start 的收尾操作。
	clientCtx, stopClient := context.WithCancel(ctx)
	clientDone := make(chan error, 1)
	t.tasks.Go(func() { clientDone <- t.client.Run(clientCtx) })
	defer func() {
		stopClient()
		t.tasks.Wait()
		t.failPendingTurnStart(ctx)
	}()

	// 3. 打开对话，交付首次同步 Thread、历史起点和子 Session；打开失败交给连接响应后正常返回。
	thread, err := t.open(ctx)
	if err != nil {
		t.opened <- connectionThreadOpened{err: err}
		return nil
	}
	delivered := make(chan struct{})
	t.opened <- connectionThreadOpened{
		thread:       thread,
		hasExecutors: len(t.executors) > 0,
		followStart: historyReadEnd{
			ordinal:      t.followStartOrdinal,
			activeTurnID: t.followStartActiveTurnID,
		},
		subAgentSessionIDs: t.getSubAgentSessionIDs(),
		delivered:          delivered,
	}
	// 4. 等待连接回复打开请求，之后释放暂存通知。
	select {
	case <-delivered:
	case <-t.unsubscribed:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	t.isOpenDelivered = true
	t.flushHeldNotifications(ctx)
	// 5. 每轮先检查退订，再处理暂存的事件；退订时返回 nil，还在等 Turn ID 的 turn/start 由收尾回复 thread not loaded。
	for {
		if t.isUnsubscribed() {
			return nil
		}
		if err := t.handleQueuedClientEvents(ctx); err != nil {
			return err
		}
		// 6. 同时等待退订、请求、Client 事件和交出下一个子 Session；没有待交出的子 Session 时 subAgentDiscoveries 为 nil，这一路不会就绪。
		// Client 断线进入重连时返回 ErrConnectionLost，ctx 结束时返回结束原因。
		var subAgentDiscoveries chan<- *subAgentSessionDiscovered
		var nextDiscovery *subAgentSessionDiscovered
		if len(t.pendingSubAgentDiscoveries) > 0 {
			subAgentDiscoveries, nextDiscovery = t.subAgentDiscoveries, t.pendingSubAgentDiscoveries[0]
		}
		select {
		case <-t.unsubscribed:
			return nil
		case request := <-t.requests:
			t.handleRequest(ctx, request)
		case event, ok := <-t.client.Events():
			if !ok {
				// 事件流关闭时，ctx 已结束就返回结束原因，否则返回 Client.Run 的错误。
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				return <-clientDone
			}
			if err := t.handleClientEvent(ctx, event); err != nil {
				return err
			}
		case subAgentDiscoveries <- nextDiscovery:
			t.pendingSubAgentDiscoveries = t.pendingSubAgentDiscoveries[1:]
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// isUnsubscribed 连接是否已退订这个 Thread？
func (t *connectionThread) isUnsubscribed() bool {
	// 1. 检查退订通知是否已关闭。
	select {
	case <-t.unsubscribed:
		return true
	default:
		return false
	}
}

// open 打开对话、读取 Session 详情，处理首次同步后返回 Thread。
func (t *connectionThread) open(ctx context.Context) (codexThread, error) {
	// 1. 以 SkipHistory 打开对话并跟随指定 Session，暂存期间到达的事件。
	record, err := callClient(ctx, t, func(ctx context.Context) (defs.Conversation, error) {
		return t.client.OpenConversation(ctx, &runtimeclient.OpenConversationRequest{
			ConversationID: t.conversationID,
			SessionID:      t.sessionID,
			SkipHistory:    true,
		})
	})
	if err != nil {
		return codexThread{}, err
	}
	// 2. 读取 Executor 详情并创建 Item 翻译器。
	details, err := t.readSessionDetails(ctx)
	if err != nil {
		return codexThread{}, err
	}
	t.executors = details.Executors
	t.translator = t.newSessionTranslator()
	// 3. 处理 callClient 暂存的首次同步事件（ConversationSynced、SessionSynced），返回打开请求用的 Thread；
	// 产生的通知暂存到连接回复打开请求之后发出。
	if err := t.handleQueuedClientEvents(ctx); err != nil {
		return codexThread{}, err
	}
	return t.newCodexThread(record, details), nil
}

// isRootThread 这个 Thread 是不是对话的根 Thread？
func (t *connectionThread) isRootThread() bool {
	// 1. 比较 Thread ID 和对话 ID。
	return t.threadID == string(t.conversationID)
}

// readSessionDetails 读取跟随 Session 的模型和 Executor 等详情。
func (t *connectionThread) readSessionDetails(ctx context.Context) (conversation.SessionDetails, error) {
	// 1. 调用 Client 获取详情；对话可无 Executor，模型不在目录时详情里的模型为空。
	return callClient(ctx, t, func(ctx context.Context) (conversation.SessionDetails, error) {
		return t.client.GetSessionDetails(ctx, t.conversationID, t.sessionID)
	})
}

// newSessionTranslator 按 Session 的 Executor 目录和 Agent 路径创建 Item 翻译器。
func (t *connectionThread) newSessionTranslator() *codexItemTranslator {
	// 1. 整理各 Executor 工作目录，按配置第一项选取默认 Executor，创建翻译器。
	workspaceDirectories := make(map[string]string, len(t.executors))
	for _, executorDetails := range t.executors {
		workspaceDirectories[executorDetails.ID] = executorDetails.WorkspaceDirectory
	}
	return newCodexItemTranslator(t.sessionID, t.agentPaths, workspaceDirectories, getDefaultExecutorDetails(t.executors).ID)
}

// newCodexThread 按对话记录、Session 详情和已报告状态创建 Thread。
func (t *connectionThread) newCodexThread(record defs.Conversation, details conversation.SessionDetails) codexThread {
	// 1. 使用 Session 所选模型；目录有对应模型时使用详情中的实际模型。
	model := details.Session.Model
	if details.Model != nil {
		model.Ref = details.Model.Ref
	}
	// 2. 根 Thread 填入归属目录、状态和临时标记。
	status := newCodexThreadStatus(t.threadStatusType)
	if t.isRootThread() {
		thread := newCodexRootThread(record, model, t.pathMappings, t.isWindowsClient, status, t.version)
		thread.Ephemeral = t.isEphemeral
		return thread
	}
	// 3. 子 Thread 从对话 Session 记录中计算父 Thread、层数和来源。
	sessionRecords := slices.DeleteFunc(t.getSessionRecords(), func(sessionRecord defs.Session) bool { return sessionRecord.ID == t.sessionID })
	subAgents := getSubAgentSessions(record, append(sessionRecords, details.Session))
	subAgent := subAgents[slices.IndexFunc(subAgents, func(subAgent subAgentSession) bool { return subAgent.record.ID == t.sessionID })]
	subAgent.record.Model = model
	return newCodexChildThread(record, subAgent, t.pathMappings, t.isWindowsClient, status, t.version)
}

// newSubAgentSessionDiscovered 创建根 Thread 向连接交付的子 Session 发现事件。
func (t *connectionThread) newSubAgentSessionDiscovered(info runtime.SessionRuntimeInfo) *subAgentSessionDiscovered {
	// 1. 读取子 Session 的层级和当前运行状态，创建带客户端目录的子 Thread。
	subAgents := getSubAgentSessions(t.conversationRecord, t.getSessionRecords())
	subAgent := subAgents[slices.IndexFunc(subAgents, func(subAgent subAgentSession) bool { return subAgent.record.ID == info.Session.ID })]
	status := newCodexThreadStatus(getCodexThreadStatusType(info))
	return &subAgentSessionDiscovered{
		conversationID: t.conversationID,
		thread:         newCodexChildThread(t.conversationRecord, subAgent, t.pathMappings, t.isWindowsClient, status, t.version),
	}
}

// getSessionRecords 返回按创建顺序排列的对话 Session 记录。
func (t *connectionThread) getSessionRecords() []defs.Session {
	// 1. 收集运行信息中的记录，按 Session ID 排序。
	records := make([]defs.Session, 0, len(t.conversationSessions))
	for _, info := range t.conversationSessions {
		records = append(records, info.Session)
	}
	slices.SortFunc(records, func(a, b defs.Session) int { return cmp.Compare(a.ID, b.ID) })
	return records
}

// getSubAgentSessionIDs 返回按创建顺序排列的子 Session ID。
func (t *connectionThread) getSubAgentSessionIDs() []defs.SessionID {
	// 1. 遍历已排序记录，只收集有父 Session 的 ID。
	var sessionIDs []defs.SessionID
	for _, record := range t.getSessionRecords() {
		if record.ParentSessionID != "" {
			sessionIDs = append(sessionIDs, record.ID)
		}
	}
	return sessionIDs
}

// callClient 在 Thread 任务中调用 Client，等待期间暂存事件。
func callClient[T any](ctx context.Context, t *connectionThread, call func(context.Context) (T, error)) (T, error) {
	// 1. 启动调用任务，取得事件通道。
	var value T
	callDone := make(chan error, 1)
	t.tasks.Go(func() {
		var err error
		value, err = call(ctx)
		callDone <- err
	})
	events := t.client.Events()
	// 2. 等待调用结果并按序暂存事件；调用结束时读完缓冲事件，取消错误使用 Thread 的结束原因。
	for {
		select {
		case err := <-callDone:
			t.queueBufferedClientEvents(events)
			if err != nil && ctx.Err() != nil {
				err = context.Cause(ctx)
			}
			return value, err
		case event, ok := <-events:
			if !ok {
				// Client 已经停止，事件流关闭后不再读它，继续等调用返回。
				events = nil
				continue
			}
			t.queuedClientEvents = append(t.queuedClientEvents, event)
		}
	}
}

// queueBufferedClientEvents 暂存事件通道中已有的事件。
func (t *connectionThread) queueBufferedClientEvents(events <-chan runtimeclient.ClientEvent) {
	// 1. 持续读取可立即取得的事件，通道为空或已关闭时返回，不等待新事件。
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			t.queuedClientEvents = append(t.queuedClientEvents, event)
		default:
			return
		}
	}
}

// handleQueuedClientEvents 按到达顺序处理暂存事件。
func (t *connectionThread) handleQueuedClientEvents(ctx context.Context) error {
	// 1. 逐条移除并处理事件，Client 断线时返回错误；处理期间新暂存的事件继续排在末尾。
	for len(t.queuedClientEvents) > 0 {
		event := t.queuedClientEvents[0]
		t.queuedClientEvents = t.queuedClientEvents[1:]
		if err := t.handleClientEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

// handleRequest 处理连接交来的请求，发送响应或查询结果。
func (t *connectionThread) handleRequest(ctx context.Context, request connectionThreadRequest) {
	// 1. 按类型分派 Turn、恢复、改名和查询请求。
	switch request := request.(type) {
	case *turnStartRequested:
		t.handleTurnStartRequested(ctx, request)
	case *turnSteerRequested:
		t.handleTurnSteerRequested(ctx, request)
	case *threadResumeRequested:
		t.handleThreadResumeRequested(ctx, request)
	case *threadNameSetRequested:
		_, err := callClient(ctx, t, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, t.client.SetConversationTitle(ctx, t.conversationID, request.title)
		})
		t.respond(ctx, "thread/name/set", request.requestID, codexEmptyObjectResult{}, err)
	case *threadStatusRequested:
		request.reply <- t.getSessionStatus(request.sessionID)
	case *threadHistoryEndRequested:
		request.reply <- t.historyEndOrdinal
	default:
		panic(fmt.Sprintf("unsupported connectionThread request %T", request))
	}
}

// handleTurnStartRequested 提交新 Turn 输入，保存待取得 Turn ID 的响应信息。
func (t *connectionThread) handleTurnStartRequested(ctx context.Context, request *turnStartRequested) {
	// 1. 已有启动请求等待 Turn ID 时回复 Session 忙，被放弃的对话先重新打开。
	if t.pendingTurnStart != nil {
		t.respond(ctx, "turn/start", request.requestID, nil, session.ErrSessionBusy)
		return
	}
	if t.isConversationDropped {
		if err := t.reopenConversation(ctx); err != nil {
			t.respond(ctx, "turn/start", request.requestID, nil, err)
			return
		}
	}
	// 2. 按需保存模型选择，以 idle_only 提交用户输入。
	if err := t.selectModel(ctx, request.model, request.effort); err != nil {
		t.respond(ctx, "turn/start", request.requestID, nil, err)
		return
	}
	inputItemID, err := callClient(ctx, t, func(ctx context.Context) (defs.ItemID, error) {
		return t.client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
			ConversationID: t.conversationID,
			Mode:           session.SessionInputModeIdleOnly,
			Content:        request.content,
		})
	})
	if err != nil {
		t.respond(ctx, "turn/start", request.requestID, nil, err)
		return
	}
	// 3. 登记客户端消息 ID 和待响应请求，输入保存或 Turn 结束时回复。
	if request.clientUserMessageID != "" {
		t.clientUserMessageIDs[inputItemID] = request.clientUserMessageID
	}
	t.pendingTurnStart = &pendingTurnStart{
		requestID:   request.requestID,
		inputItemID: inputItemID,
	}
}

// reopenConversation 重新打开 Client 已放弃的对话；之后到达的同步事件会补发没显示过的 Item 和 Turn。
func (t *connectionThread) reopenConversation(ctx context.Context) error {
	// 1. 重新打开跟随的 Session，成功后清除放弃标记。
	_, err := callClient(ctx, t, func(ctx context.Context) (defs.Conversation, error) {
		return t.client.OpenConversation(ctx, &runtimeclient.OpenConversationRequest{
			ConversationID: t.conversationID,
			SessionID:      t.sessionID,
		})
	})
	if err != nil {
		return err
	}
	t.isConversationDropped = false
	return nil
}

// selectModel 校验并保存请求的模型和推理强度。
func (t *connectionThread) selectModel(ctx context.Context, model, effort *string) error {
	// 1. 未指定模型和强度时返回，否则读取目录。
	if model == nil && effort == nil {
		return nil
	}
	catalog, err := callClient(ctx, t, func(ctx context.Context) (runtime.ModelList, error) {
		return t.client.ListModels(ctx, "")
	})
	if err != nil {
		return err
	}
	// 2. 在现有选择上应用请求模型，换模型时重置强度，模型无效返回 -32602。
	selection := t.selectedModel
	if model != nil {
		ref, err := defs.ParseModelRef(*model)
		if err != nil {
			return &codexError{
				errorCodeInvalidParams,
				err.Error(),
			}
		}
		if ref != selection.Ref {
			selection = defs.ModelSelection{Ref: ref}
		}
	}
	index := slices.IndexFunc(catalog.Models, func(definition *defs.ModelDefinition) bool { return definition.Ref == selection.Ref })
	if index < 0 {
		return &codexError{
			errorCodeInvalidParams,
			fmt.Sprintf("model %s is not in the model catalog", selection.Ref),
		}
	}
	// 3. 校验强度；不支持强度选择的模型将 none 作为默认强度，其他不支持的强度返回 -32602。
	definition := catalog.Models[index]
	if effort != nil {
		selection.ReasoningEffort = *effort
		if *effort == "none" && len(definition.ReasoningEfforts) == 0 {
			selection.ReasoningEffort = ""
		}
		if !definition.IsReasoningEffortSupported(selection.ReasoningEffort) {
			return &codexError{
				errorCodeInvalidParams,
				fmt.Sprintf("reasoning effort %s is not supported by %s", *effort, selection.Ref),
			}
		}
	}
	// 4. 选择未变时返回，否则保存 Session 模型并更新本地选择。
	if selection == t.selectedModel {
		return nil
	}
	record, err := callClient(ctx, t, func(ctx context.Context) (defs.Session, error) {
		return t.client.SetSessionModel(ctx, &runtimeclient.SetSessionModelRequest{
			ConversationID: t.conversationID,
			SessionID:      t.sessionID,
			Model:          selection,
		})
	})
	if err != nil {
		return err
	}
	t.selectedModel = record.Model
	return nil
}

// handleTurnSteerRequested 校验正在执行的 Turn，提交插话并响应。
func (t *connectionThread) handleTurnSteerRequested(ctx context.Context, request *turnSteerRequested) {
	// 1. expectedTurnID 为空或与正在执行的 Turn 不同，回复 -32600。
	if request.expectedTurnID == "" || request.expectedTurnID != t.activeTurnID {
		t.respond(ctx, "turn/steer", request.requestID, nil, &codexError{
			errorCodeInvalidRequest,
			fmt.Sprintf("expected turn %s is not active", request.expectedTurnID),
		})
		return
	}
	// 2. 以 steer 提交内容，错误时响应失败。
	inputItemID, err := callClient(ctx, t, func(ctx context.Context) (defs.ItemID, error) {
		return t.client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
			ConversationID: t.conversationID,
			Mode:           session.SessionInputModeSteer,
			Content:        request.content,
		})
	})
	if err != nil {
		t.respond(ctx, "turn/steer", request.requestID, nil, err)
		return
	}
	// 3. 登记客户端消息 ID，回复插话目标 Turn ID。
	if request.clientUserMessageID != "" {
		t.clientUserMessageIDs[inputItemID] = request.clientUserMessageID
	}
	t.respond(ctx, "turn/steer", request.requestID, codexTurnSteerResult{TurnID: string(request.expectedTurnID)}, nil)
}

// getSessionStatus 返回指定 Session 的 Thread 状态。
func (t *connectionThread) getSessionStatus(sessionID defs.SessionID) codexThreadStatus {
	// 1. 跟随的 Session 使用已报告状态，其他已知 Session 使用运行信息，未知 Session 返回 notLoaded。
	if sessionID == t.sessionID {
		return newCodexThreadStatus(t.threadStatusType)
	}
	info, isKnown := t.conversationSessions[sessionID]
	if !isKnown {
		return newCodexThreadStatus(codexThreadStatusNotLoaded)
	}
	return newCodexThreadStatus(getCodexThreadStatusType(info))
}

// handleThreadResumeRequested 用现有记录响应恢复请求，重发未保存内容和用量。
func (t *connectionThread) handleThreadResumeRequested(ctx context.Context, request *threadResumeRequested) {
	// 1. 读取对话和 Session 详情，任一步失败时响应错误。
	record, err := callClient(ctx, t, func(ctx context.Context) (defs.Conversation, error) {
		return t.client.GetConversation(ctx, t.conversationID)
	})
	if err != nil {
		t.respond(ctx, "thread/resume", request.requestID, nil, err)
		return
	}
	details, err := t.readSessionDetails(ctx)
	if err != nil {
		t.respond(ctx, "thread/resume", request.requestID, nil, err)
		return
	}
	// 2. 创建带调用方历史的 Thread，响应后记录已实时推送的位置为分页终点。
	thread := t.newCodexThread(record, details)
	thread.Turns = request.turns
	t.respond(ctx, "thread/resume", request.requestID, newCodexThreadOpenResult(thread, len(details.Executors) > 0), nil)
	t.historyEndOrdinal = t.lastDisplayedOrdinal
	// 3. 已保存的回复和推理由历史分页给出，标为完成后从 openReplies 移除；按位置重发未保存的回复和推理的累计内容。
	t.openReplies = slices.DeleteFunc(t.openReplies, func(reply openReply) bool {
		reply.display.isCompleted = reply.display.isCompleted || reply.display.isSaved
		return reply.display.isCompleted
	})
	for _, index := range slices.Sorted(maps.Keys(t.streamingItems)) {
		display := t.streamingItems[index]
		if display.isStarted && !display.isSaved && isStreamingReply(display.streamingItem) {
			t.displayItem(ctx, display.streamingItem, &threadItemDisplay{messagePhase: display.messagePhase}, display.isCompleted)
		}
	}
	// 4. 重新发送已报告的当前用量。
	if info := t.conversationSessions[t.sessionID]; t.reportedTokenUsage != nil {
		turnID := cmp.Or(info.ActiveTurnID, info.LastTurnID)
		t.notify(ctx, "thread/tokenUsage/updated", codexThreadTokenUsageUpdatedNotification{
			ThreadID:   t.threadID,
			TurnID:     string(turnID),
			TokenUsage: *t.reportedTokenUsage,
		})
	}
}

// handleClientEvent 处理 Client 事件，更新记录并发送 Codex 通知。
func (t *connectionThread) handleClientEvent(ctx context.Context, event runtimeclient.ClientEvent) error {
	// 1. 按事件类型分派，Client 开始重连时返回 ErrConnectionLost。
	switch event := event.(type) {
	case *runtimeclient.ConnectionChanged:
		if event.State == runtimeclient.ConnectionReconnecting {
			return runtimeclient.ErrConnectionLost
		}
	case *runtimeclient.ConversationSynced:
		t.handleConversationSynced(ctx, event)
	case *runtimeclient.ConversationTitleChanged:
		t.conversationRecord.Title = event.Title
		if t.isRootThread() {
			t.notify(ctx, "thread/name/updated", codexThreadNameUpdatedNotification{
				ThreadID:   t.threadID,
				ThreadName: event.Title,
			})
		}
	case *runtimeclient.ConversationSyncFailed:
		t.handleConversationSyncFailed(ctx, event)
	case *runtimeclient.SessionSynced:
		t.handleSessionSynced(ctx, event)
	case *runtimeclient.SessionStatusChanged:
		t.handleSessionInfo(ctx, event.Info)
	case *runtimeclient.TurnStarted:
		t.switchActiveTurn(ctx, event.TurnID)
	case *runtimeclient.ItemChanged:
		t.handleItemChanged(ctx, event)
	case *runtimeclient.ToolProgress:
		t.handleToolProgress(ctx, event)
	case *runtimeclient.HookRunChanged:
		t.handleHookRunChanged(ctx, event)
	case *runtimeclient.ItemsSaved:
		t.handleItemsSaved(ctx, event)
	case *runtimeclient.TurnFinished:
		t.handleTurnFinished(ctx, event)
	}
	return nil
}

// handleConversationSynced 保存同步对话和 Session 信息，重新同步时刷新翻译器。
func (t *connectionThread) handleConversationSynced(ctx context.Context, event *runtimeclient.ConversationSynced) {
	// 1. 根 Thread 标题变化时通知客户端，保存对话和各 Session 运行信息。
	if t.isSynced && t.isRootThread() && t.conversationRecord.Title != event.Conversation.Title {
		t.notify(ctx, "thread/name/updated", codexThreadNameUpdatedNotification{
			ThreadID:   t.threadID,
			ThreadName: event.Conversation.Title,
		})
	}
	t.conversationRecord = event.Conversation
	for _, info := range event.SessionInfos {
		if info.Session.ID == t.sessionID {
			t.syncedSessionInfo = info
		}
		t.handleSessionInfo(ctx, info)
	}
	// 2. 首次同步直接返回；重新同步读取详情，失败时记警告并沿用原翻译器，成功时重建。
	if !t.isSynced {
		return
	}
	details, err := t.readSessionDetails(ctx)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("codex thread session details not refreshed", "thread_id", t.threadID, "error", err)
		return
	}
	t.executors = details.Executors
	t.translator = t.newSessionTranslator()
}

// handleConversationSyncFailed 标记对话已放弃，结束等待请求并报告 systemError。
func (t *connectionThread) handleConversationSyncFailed(ctx context.Context, event *runtimeclient.ConversationSyncFailed) {
	// 1. 标记已放弃，将待启动请求回复为 serve 错误并释放通知。
	t.isConversationDropped = true
	if pending := t.pendingTurnStart; pending != nil {
		t.pendingTurnStart = nil
		t.respond(ctx, "turn/start", pending.requestID, nil, event.Err)
		t.flushHeldNotifications(ctx)
	}
	// 2. Thread 状态还不是 systemError 时改为 systemError，并通知客户端。
	if t.threadStatusType != codexThreadStatusSystemError {
		t.threadStatusType = codexThreadStatusSystemError
		t.notify(ctx, "thread/status/changed", codexThreadStatusChangedNotification{
			ThreadID: t.threadID,
			Status:   newCodexThreadStatus(codexThreadStatusSystemError),
		})
	}
}

// handleSessionInfo 保存 Session 运行信息，更新子 Thread、状态和用量通知。
func (t *connectionThread) handleSessionInfo(ctx context.Context, info runtime.SessionRuntimeInfo) {
	// 1. 保存运行信息，新 Session 出现时重算 Agent 路径并重建翻译器。
	previous, isKnown := t.conversationSessions[info.Session.ID]
	t.conversationSessions[info.Session.ID] = info
	if !isKnown {
		t.agentPaths = getAgentPaths(t.getSessionRecords())
		t.translator = t.newSessionTranslator()
	}
	// 2. 其他 Session 由根 Thread 按需排队交给连接自动跟随。
	if info.Session.ID != t.sessionID {
		if t.isRootThread() && t.isSubAgentSessionToFollow(previous, isKnown, info) {
			t.pendingSubAgentDiscoveries = append(t.pendingSubAgentDiscoveries, t.newSubAgentSessionDiscovered(info))
		}
		return
	}
	// 3. 跟随 Session 更新模型，状态变化时通知客户端。
	t.selectedModel = info.Session.Model
	statusType := getCodexThreadStatusType(info)
	if t.threadStatusType != "" && statusType != t.threadStatusType {
		t.notify(ctx, "thread/status/changed", codexThreadStatusChangedNotification{
			ThreadID: t.threadID,
			Status:   newCodexThreadStatus(statusType),
		})
	}
	t.threadStatusType = statusType
	// 4. 存在 Turn 和已知用量时比较最近报告，只发送变化后的用量。
	turnID := cmp.Or(info.ActiveTurnID, info.LastTurnID)
	if turnID == "" || (info.Usage == defs.TokenUsage{} && info.Context.Source == runtime.ContextUsageSourceUnknown) {
		return
	}
	usage := getCodexThreadTokenUsage(info)
	if t.reportedTokenUsage != nil && reflect.DeepEqual(*t.reportedTokenUsage, usage) {
		return
	}
	t.reportedTokenUsage = &usage
	t.notify(ctx, "thread/tokenUsage/updated", codexThreadTokenUsageUpdatedNotification{
		ThreadID:   t.threadID,
		TurnID:     string(turnID),
		TokenUsage: usage,
	})
}

// isSubAgentSessionToFollow 根 Thread 要不要让连接跟随这个子 Session？
func (t *connectionThread) isSubAgentSessionToFollow(previous runtime.SessionRuntimeInfo, isKnown bool, info runtime.SessionRuntimeInfo) bool {
	// 1. 根 Session 不跟随；首次同步跟随正在执行的子 Session，之后跟随新子 Session 或由空闲转为执行的子 Session。
	switch {
	case info.Session.ParentSessionID == "":
		return false
	case !t.isSynced:
		return info.ActiveTurnID != ""
	case !isKnown:
		return true
	}
	return previous.ActiveTurnID == "" && info.ActiveTurnID != ""
}

// handleSessionSynced 记录同步位置，补发已保存内容、Turn 切换和生成中内容。
func (t *connectionThread) handleSessionSynced(ctx context.Context, event *runtimeclient.SessionSynced) {
	// 1. 首次同步记录跟随起点和分页终点。
	isFirstSync := !t.isSynced
	if isFirstSync {
		t.isSynced = true
		t.followStartOrdinal = event.LastSavedOrdinal
		t.lastDisplayedOrdinal = event.LastSavedOrdinal
		t.historyEndOrdinal = event.LastSavedOrdinal
	}
	// 2. 从保存内容中回复待启动请求，逐条补发未显示 Item，先切换所属 Turn。
	t.resolvePendingTurnStart(ctx, event.Items)
	for _, logged := range event.Items {
		if logged.Ordinal <= t.lastDisplayedOrdinal {
			continue
		}
		// session_fork 不属于任何 Turn。
		if logged.Item.TurnID != "" {
			t.switchActiveTurn(ctx, logged.Item.TurnID)
		}
		t.displaySavedItem(ctx, logged)
	}
	// 3. 按运行信息补发 Turn 切换；运行信息早于内容的订阅位置，Item 里已经出现更新的 Turn 时不退回。
	if t.activeTurnID <= max(t.syncedSessionInfo.ActiveTurnID, t.syncedSessionInfo.LastTurnID) {
		t.switchActiveTurn(ctx, t.syncedSessionInfo.ActiveTurnID)
	}
	// 4. 首次同步有活动 Turn 时登记它，并补上起点及之前仍在执行的工具调用。
	if isFirstSync && t.activeTurnID != "" {
		t.followStartActiveTurnID = t.activeTurnID
		t.displaySavedRunningToolCalls(ctx)
	}
	// 5. 按位置登记生成中 Item，补发尚未显示的开始和累计内容。
	for _, streaming := range event.StreamingItems {
		display := t.ensureStreamingDisplay(streaming.ItemIndex)
		display.streamingItem = streaming.Item.Clone()
		t.displayedItems[streaming.Item.ID] = display
		t.displayItem(ctx, streaming.Item, display, false)
	}
}

// displaySavedRunningToolCalls 登记首次接入时已保存且仍在执行的工具调用。
func (t *connectionThread) displaySavedRunningToolCalls(ctx context.Context) {
	// 1. 读取活动 Turn 在跟随起点及之前的日志，失败时记警告并返回。
	items, err := t.readSavedTurnItems(ctx, t.activeTurnID)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("codex thread running tool calls not shown", "thread_id", t.threadID, "error", err)
		return
	}
	// 2. 按结果配对，只保留 Ready 且没有结果的调用。
	resultsByCall := getToolResultsByCall(items)
	for _, item := range items {
		if item.Kind != defs.ItemToolCall || item.ToolCall.Status != defs.ToolCallReady || resultsByCall[item] != nil {
			continue
		}
		// 3. 分页模式将可显示调用标为已开始；其他模式补发开始通知，待结果工具在结果到达时显示。
		display := &threadItemDisplay{}
		t.displayedItems[item.ID] = display
		if t.isHistoryPaged {
			display.itemID, display.isStarted = item.ID, t.translator.getStartedItem(item) != nil
			t.toolCallsByCallID[item.ToolCall.CallID] = item
			continue
		}
		t.displayItem(ctx, item, display, false)
	}
}

// readSavedTurnItems 从日志末尾往前翻页，取出指定 Turn 在跟随起点及之前的已保存 Item，按日志顺序返回。
func (t *connectionThread) readSavedTurnItems(ctx context.Context, turnID defs.TurnID) ([]*defs.Item, error) {
	// 1. 从日志末尾倒序分页读取，首次请求不带游标。
	var items []*defs.Item
	request := &runtimeclient.ListItemsRequest{
		ConversationID: t.conversationID,
		SessionID:      t.sessionID,
		Order:          storage.DirectionDesc,
	}
	for {
		page, err := callClient(ctx, t, func(ctx context.Context) (runtimeclient.ListItemsResult, error) {
			return t.client.ListItems(ctx, request)
		})
		if err != nil {
			return nil, err
		}
		// 2. 只收集跟随起点及之前的 Item，遇到其他 Turn 或读到末尾时反转为日志正序并返回。
		for _, logged := range page.Items {
			if logged.Ordinal > t.followStartOrdinal {
				continue
			}
			if logged.Item.TurnID != turnID {
				slices.Reverse(items)
				return items, nil
			}
			items = append(items, logged.Item)
		}
		if page.NextCursor == 0 {
			slices.Reverse(items)
			return items, nil
		}
		request.Cursor = page.NextCursor
	}
}

// switchToNewerTurn 按实时事件切换到更新的活动 Turn。
func (t *connectionThread) switchToNewerTurn(ctx context.Context, turnID defs.TurnID) {
	// 1. 比较活动 Turn 和最近完成 Turn 的 ID，只切换更新的轮次。
	if turnID > t.activeTurnID && turnID > t.lastCompletedTurnID {
		t.switchActiveTurn(ctx, turnID)
	}
}

// switchActiveTurn 完成先前 Turn，切换客户端看到的活动 Turn。
func (t *connectionThread) switchActiveTurn(ctx context.Context, turnID defs.TurnID) {
	// 1. 相同 Turn 直接返回，否则先补发旧 Turn 完成；终态取同步记录，不匹配时使用 completed。
	if turnID == t.activeTurnID {
		return
	}
	if t.activeTurnID != "" {
		status := defs.TurnStatusCompleted
		if t.syncedSessionInfo.LastTurnID == t.activeTurnID {
			status = t.syncedSessionInfo.LastTurnStatus
		}
		t.notifyTurnCompleted(ctx, t.activeTurnID, status, "")
	}
	// 2. 更新活动 Turn，空 ID 表示没有正在执行的 Turn。
	t.activeTurnID = turnID
	if turnID == "" {
		return
	}
	// 3. 新 Turn 清空上一轮显示记录，发送 turn/started。
	clear(t.streamingItems)
	clear(t.displayedItems)
	clear(t.toolCallsByCallID)
	t.openReplies = nil
	t.notify(ctx, "turn/started", codexTurnNotification{
		ThreadID: t.threadID,
		Turn:     newCodexTurn(turnID, codexTurnInProgress, nil),
	})
}

// handleItemChanged 更新生成中 Item 的记录，发送缺少的显示内容或增量。
func (t *connectionThread) handleItemChanged(ctx context.Context, event *runtimeclient.ItemChanged) {
	// 1. 需要时先切换到更新 Turn。
	t.switchToNewerTurn(ctx, event.TurnID)
	// 2. 开始、完成和中断保存 Item 的完整内容并补发显示；增量仅追加到已开始且未完成的记录，无 Item 的中断不显示。
	switch event.Type {
	case agent.AgentEventItemStarted, agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
		if event.Item == nil {
			return
		}
		display := t.ensureStreamingDisplay(event.ItemIndex)
		display.streamingItem = event.Item.Clone()
		t.displayedItems[event.Item.ID] = display
		t.displayItem(ctx, event.Item, display, event.Type != agent.AgentEventItemStarted)
	case agent.AgentEventItemDelta:
		// 已经完成的回复（工具调用开始前提前完成的）不再发增量。
		display := t.streamingItems[event.ItemIndex]
		if display == nil || !display.isStarted || display.isCompleted || event.Delta == nil {
			return
		}
		agent.ApplyItemDelta(display.streamingItem, event.Delta)
		t.sendItemDeltas(ctx, event.TurnID, display, event.Delta.Text, strings.Join(event.Delta.ReasoningSummary, ""), strings.Join(event.Delta.ReasoningContent, ""))
	}
}

// handleToolProgress 按 CallID 找到 bash 调用，发送命令输出增量。
func (t *connectionThread) handleToolProgress(ctx context.Context, event *runtimeclient.ToolProgress) {
	// 1. 按需切换 Turn，查找已开始显示的 bash 调用。
	t.switchToNewerTurn(ctx, event.TurnID)
	call := t.toolCallsByCallID[event.Progress.CallID]
	if call == nil || call.ToolCall.Name != "bash" {
		return
	}
	if display := t.displayedItems[call.ID]; display == nil || !display.isStarted {
		return
	}
	// 2. 从执行进度中提取非空文本，发送 outputDelta；接入前的输出不补发。
	if delta := executor.GetExecutionOutputText(event.Progress.Content); delta != "" {
		t.notify(ctx, "item/commandExecution/outputDelta", codexItemDeltaNotification{
			ThreadID: t.threadID,
			TurnID:   string(event.TurnID),
			ItemID:   string(call.ID),
			Delta:    delta,
		})
	}
}

// handleHookRunChanged 将 hook 运行变化发送为开始或完成通知。
func (t *connectionThread) handleHookRunChanged(ctx context.Context, event *runtimeclient.HookRunChanged) {
	// 1. 按需补发 Turn 开始，运行中使用 hook/started，其余状态使用 hook/completed。
	t.switchToNewerTurn(ctx, event.TurnID)
	method := "hook/completed"
	if event.Run.Status == defs.HookRunRunning {
		method = "hook/started"
	}
	t.notify(ctx, method, codexHookNotification{
		ThreadID: t.threadID,
		TurnID:   string(event.TurnID),
		Run:      getCodexHookRunSummary(event.Run),
	})
}

// handleItemsSaved 响应已保存的启动输入，补发这批 Item。
func (t *connectionThread) handleItemsSaved(ctx context.Context, event *runtimeclient.ItemsSaved) {
	// 1. 先回复已取得 Turn ID 的启动请求，按需切换 Turn，再显示每条已保存 Item 的缺少部分。
	t.resolvePendingTurnStart(ctx, event.Items)
	t.switchToNewerTurn(ctx, event.TurnID)
	for _, logged := range event.Items {
		t.displaySavedItem(ctx, logged)
	}
}

// handleTurnFinished 响应尚未取得 Turn ID 的启动请求，发送 Turn 完成通知。
func (t *connectionThread) handleTurnFinished(ctx context.Context, event *runtimeclient.TurnFinished) {
	// 1. 本轮包含待启动输入时先回复，即使输入保存事件未交付。
	if pending := t.pendingTurnStart; pending != nil && slices.Contains(event.InputItemIDs, pending.inputItemID) {
		t.respondPendingTurnStart(ctx, event.TurnID)
	}
	// 2. 按需补发 Turn 开始，清除活动 Turn，发送带状态和错误的完成通知。
	t.switchToNewerTurn(ctx, event.TurnID)
	if event.TurnID == t.activeTurnID {
		t.activeTurnID = ""
	}
	t.notifyTurnCompleted(ctx, event.TurnID, event.Status, event.Error)
}

// notifyTurnCompleted 发送指定 Turn 的完成通知。
func (t *connectionThread) notifyTurnCompleted(ctx context.Context, turnID defs.TurnID, status defs.TurnStatus, errorText string) {
	// 1. 跳过不晚于最近完成 Turn 的轮次。
	if turnID <= t.lastCompletedTurnID {
		return
	}
	// 2. 登记完成 ID，先完成本轮回复，再发送 Turn 状态和失败信息。
	t.lastCompletedTurnID = turnID
	t.completeOpenReplies(ctx, turnID)
	codexStatus := getCodexTurnStatus(status)
	turn := newCodexTurn(turnID, codexStatus, getCodexTurnError(codexStatus, errorText))
	t.notify(ctx, "turn/completed", codexTurnNotification{
		ThreadID: t.threadID,
		Turn:     turn,
	})
}

// resolvePendingTurnStart 在已保存 Item 中查找待启动输入，回复所属 Turn。
func (t *connectionThread) resolvePendingTurnStart(ctx context.Context, items []runtimeprotocol.LoggedItem) {
	// 1. 没有待启动请求或未找到输入时返回，找到时使用输入的 Turn ID 响应。
	pending := t.pendingTurnStart
	if pending == nil {
		return
	}
	index := slices.IndexFunc(items, func(logged runtimeprotocol.LoggedItem) bool { return logged.Item.ID == pending.inputItemID })
	if index < 0 {
		return
	}
	t.respondPendingTurnStart(ctx, items[index].Item.TurnID)
}

// respondPendingTurnStart 先回复启动请求的 Turn ID，再释放暂存通知。
func (t *connectionThread) respondPendingTurnStart(ctx context.Context, turnID defs.TurnID) {
	// 1. 清除待启动请求，响应 inProgress Turn，然后按顺序发送暂存通知。
	pending := t.pendingTurnStart
	t.pendingTurnStart = nil
	t.respond(ctx, "turn/start", pending.requestID, codexTurnStartResult{Turn: newCodexTurn(turnID, codexTurnInProgress, nil)}, nil)
	t.flushHeldNotifications(ctx)
}

// notify 编码 Thread 通知，按请求响应进度暂存或发送。
func (t *connectionThread) notify(ctx context.Context, method string, params any) {
	// 1. 编码通知，失败时记录警告并丢弃。
	message, err := newCodexNotification(method, params)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("codex notification dropped", "rpc.method", method, "error", err)
		return
	}
	// 2. 打开请求尚未响应或启动请求仍在等 Turn ID 时暂存，否则发送。
	if !t.isOpenDelivered || t.pendingTurnStart != nil {
		t.heldNotifications = append(t.heldNotifications, message)
		return
	}
	t.send(ctx, message)
}

// flushHeldNotifications 按顺序发送并清空暂存通知。
func (t *connectionThread) flushHeldNotifications(ctx context.Context) {
	// 1. 逐条发送 heldNotifications，随后清空列表。
	for _, message := range t.heldNotifications {
		t.send(ctx, message)
	}
	t.heldNotifications = nil
}

// respond 转换请求错误并发送 Thread 响应。
func (t *connectionThread) respond(ctx context.Context, method string, requestID json.RawMessage, result any, err error) {
	// 1. 错误附上 Thread ID 并记录警告，将结果或错误写入响应队列。
	if err != nil {
		codexErr := getCodexError(err, t.threadID)
		telemetry.GetLogger(ctx).Warn("codex request failed", "method", method, "thread_id", t.threadID, "code", codexErr.Code, "error", codexErr.Message)
		err = codexErr
	}
	t.send(ctx, newCodexResponse(requestID, result, err))
}

// send 将 Thread 消息交给连接写任务。
func (t *connectionThread) send(ctx context.Context, message codexMessage) {
	// 1. 写入发送队列，Thread 上下文结束时丢弃。
	select {
	case t.outgoing <- message:
	case <-ctx.Done():
	}
}

// failPendingTurnStart 在 Thread 停止时，给还在等 Turn ID 的 turn/start 回复 thread not loaded。
func (t *connectionThread) failPendingTurnStart(ctx context.Context) {
	// 1. 没有待启动请求或连接已结束时返回。
	if t.pendingTurnStart == nil {
		return
	}
	select {
	case <-t.connectionDone:
		return
	default:
	}
	// 2. 创建 thread not loaded 错误，记录警告并清除待启动请求。
	codexErr := &codexError{
		errorCodeInvalidRequest,
		"thread not loaded: " + t.threadID,
	}
	telemetry.GetLogger(ctx).Warn("codex request failed", "method", "turn/start", "thread_id", t.threadID, "code", codexErr.Code, "error", codexErr.Message)
	response := newCodexResponse(t.pendingTurnStart.requestID, nil, codexErr)
	t.pendingTurnStart = nil
	// 3. 连接仍在时发送失败响应，连接结束时丢弃。
	select {
	case t.outgoing <- response:
	case <-t.connectionDone:
	}
}
