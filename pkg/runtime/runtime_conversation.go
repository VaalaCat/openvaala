package runtime

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// runtimeConversation 是 Runtime 运行表里的一个已打开对话：保存对话记录、订阅、Session 运行信息和未保存对话的丢弃计时，
// 由它自己的事件循环串行修改。
type runtimeConversation struct {
	conversation conversation.Conversation
	// cancel 取消这个对话实例的 ctx：删除对话时由 Runtime 事件循环调用以停止实例，
	// 没保存的对话到时丢弃时由本对话的事件循环调用，实例退出后由运行任务调用以释放 ctx。
	cancel   context.CancelFunc
	query    dao.Query
	mutation dao.Mutation
	// unsavedStore 是这个对话登记的内存库，从共用库打开的对话为 nil。
	unsavedStore *unsavedConversationStore
	// unsavedIdleTimeout 是对话没保存时的丢弃等待时长。
	unsavedIdleTimeout time.Duration
	// requests 是交给本对话事件循环的请求。
	requests chan runtimeConversationRequest
	// runningChanged 是 Runtime 持有的运行集合唤醒通道。
	runningChanged chan<- struct{}
	// stopped 在 Conversation.Run 与事件循环都结束后关闭，随后关闭全部订阅；stopReason 是 Conversation.Run 的返回值，
	// 在 stopped 关闭前写入。
	stopped    chan struct{}
	stopReason error

	// 以下只由事件循环读写（事件循环开始前由 Run 初始化，结束后由 Run 收尾）。
	// models 是模型目录，agentDefaultModels 是 Agent 名到该 Agent 默认模型的索引，用来按"下一次请求用的模型"计算窗口；
	// 取自启动实例时的 Runtime definitions，收到 ConversationReconfigured 时换成对话新配置的。
	models             map[defs.ModelRef]*defs.ModelDefinition
	agentDefaultModels map[string]defs.ModelRef
	// record 是对话记录的最新一份：Runtime 事件循环登记这个对话时读出，之后标题、归档、消息数和 Executor 设置都由本事件循环
	// 改它的副本，经 saveRecord 整条写库成功后才替换（Executor 设置写库失败时也替换，和对话正在用的设置保持一致）；
	// 其中的 UpdatedAt 是保存成功后取的当前时间，与存储分配的值可能差几毫秒。
	record defs.Conversation
	// streamingTurns 是各 Session 活动 Turn 中已开始、尚未保存的 Item 副本，由转发的事件累积，只用来给新订阅者生成初始的生成中 Item；
	// Item 的草稿本身在 Agent 的 draftItems 里。
	streamingTurns map[defs.SessionID]*streamingTurn
	// lastSavedOrdinals 是各 Session 已转发的最后一条"已保存"流水号：Session 启动事件给出初值，
	// 本次运行中未启动的 Session 被订阅时从日志读取。
	lastSavedOrdinals map[defs.SessionID]storage.Ordinal
	// runtimeSessions 是各 Session 的运行信息：对话启动前用已保存的记录初始化，之后按事件更新并推给对话订阅者。
	runtimeSessions map[defs.SessionID]*runtimeSession
	// contentSubscribers 是各 Session 的内容订阅；conversationSubscribers 是对话标题与运行信息订阅。
	contentSubscribers      map[*subscriptionStream[conversation.ConversationEvent]]*runtimeSubscriber
	conversationSubscribers map[*subscriptionStream[ConversationEvent]]*runtimeSubscriber
	// rootTurnMessageCount 是根 Session 当前这一轮已保存、计入对话消息数的条数；这一轮结束时累加到对话记录并清零。
	rootTurnMessageCount int
	// discardTimer 在没保存的对话没有订阅者、也没有 Session 在跑时开始计时，到时调用 cancel 结束对话；
	// 有人订阅、开始跑 Turn 或对话已保存时停掉。从共用库打开的对话一直为 nil。
	discardTimer *time.Timer
}

// newRuntimeConversation 创建已打开对话的运行记录。
func newRuntimeConversation(
	instance conversation.Conversation,
	cancel context.CancelFunc,
	record defs.Conversation,
	query dao.Query,
	mutation dao.Mutation,
	definitions *runtimeDefinitions,
	unsavedStore *unsavedConversationStore,
	unsavedIdleTimeout time.Duration,
	runningChanged chan<- struct{},
) *runtimeConversation {
	// 1. 保存依赖并创建请求通道、退出通知和各类运行信息表。
	return &runtimeConversation{
		conversation:            instance,
		cancel:                  cancel,
		record:                  record,
		query:                   query,
		mutation:                mutation,
		models:                  definitions.modelsByRef,
		agentDefaultModels:      definitions.agentDefaultModels,
		unsavedStore:            unsavedStore,
		unsavedIdleTimeout:      unsavedIdleTimeout,
		requests:                make(chan runtimeConversationRequest),
		runningChanged:          runningChanged,
		stopped:                 make(chan struct{}),
		streamingTurns:          make(map[defs.SessionID]*streamingTurn),
		lastSavedOrdinals:       make(map[defs.SessionID]storage.Ordinal),
		runtimeSessions:         make(map[defs.SessionID]*runtimeSession),
		contentSubscribers:      make(map[*subscriptionStream[conversation.ConversationEvent]]*runtimeSubscriber),
		conversationSubscribers: make(map[*subscriptionStream[ConversationEvent]]*runtimeSubscriber),
	}
}

// Run 启动对话实例，在当前 goroutine 跑事件循环；对话退出后关闭 stopped 和全部订阅。
func (c *runtimeConversation) Run(ctx context.Context) {
	// 1. 添加对话遥测，读取已有 Session 信息。
	ctx = telemetry.ContextWithAttrs(ctx, telemetry.ConversationID(string(c.conversation.GetID())))
	c.seedRuntimeSessions(ctx)
	signalRunningConversationsChanged(c.runningChanged)
	// 2. 启动对话任务，在本任务中处理事件与请求。
	events := make(chan conversation.ConversationEvent)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		c.stopReason = c.conversation.Run(ctx, events)
	})
	c.runEventLoop(ctx, events)
	// 3. 等待对话退出，先关闭 stopped，再以退出原因关闭全部订阅。
	tasks.Wait()
	close(c.stopped)
	c.closeSubscribers(newConversationStoppedError(c.conversation.GetID(), c.stopReason))
}

// seedRuntimeSessions 读取已有 Session 记录，初始化运行信息。
func (c *runtimeConversation) seedRuntimeSessions(ctx context.Context) {
	// 1. 读取全部 Session 信息，失败时记录 Warn 并从空表启动。
	infos, err := c.conversation.ListSessionInfos(ctx)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("session runtime info not seeded", "error", err)
		return
	}
	// 2. 逐个初始化运行信息和下一次请求的上下文窗口。
	for _, info := range infos {
		projection := newRuntimeSession(info)
		projection.refreshContext(c.models, c.agentDefaultModels)
		c.runtimeSessions[info.Session.ID] = projection
	}
}

// runEventLoop 串行更新订阅、Session 信息和未保存对话的丢弃计时。
func (c *runtimeConversation) runEventLoop(ctx context.Context, events <-chan conversation.ConversationEvent) {
	// 1. 按对话状态安排丢弃计时，未计时时使用 nil 通道。
	for {
		c.scheduleDiscard()
		var discardTimerFired <-chan time.Time
		if c.discardTimer != nil {
			discardTimerFired = c.discardTimer.C
		}
		// 2. 处理计时、对话事件和订阅等请求；对话事件通道关闭后返回。
		select {
		case <-discardTimerFired:
			// 到期且仍可丢弃时取消对话；到期的计时器留着，不再重新计时。
			if c.isDiscardable() {
				c.cancel()
			}
		case event, open := <-events:
			if !open {
				return
			}
			if reconfigured, ok := event.(*conversation.ConversationReconfigured); ok {
				c.handleConversationReconfigured(ctx, reconfigured)
				continue
			}
			c.handleConversationEvent(ctx, event)
		case request := <-c.requests:
			switch request := request.(type) {
			case *subscribeRequested:
				c.handleSubscribeRequested(request)
			case *contextUsageRequested:
				c.handleContextUsageRequested(request)
			case *sessionRecordReplaceRequested:
				c.handleSessionRecordReplaceRequested(ctx, request)
			case *titleChangeRequested:
				c.handleTitleChangeRequested(request)
			case *archiveChangeRequested:
				c.handleArchiveChangeRequested(request)
			case *recordRequested:
				c.handleRecordRequested(request)
			case *sessionsRunningRequested:
				c.handleSessionsRunningRequested(request)
			case *unsubscribeRequested:
				if request.contentStream != nil {
					removeSubscriber(c.contentSubscribers, request.contentStream, request.reason)
				} else {
					removeSubscriber(c.conversationSubscribers, request.statusStream, request.reason)
				}
			default:
				panic(fmt.Sprintf("unsupported runtime Conversation request %T", request))
			}
		}
	}
}

// scheduleDiscard 按未保存对话的使用状态启动或停止丢弃计时。
func (c *runtimeConversation) scheduleDiscard() {
	// 1. 不可丢弃时停止并清空计时器。
	if !c.isDiscardable() {
		if c.discardTimer != nil {
			c.discardTimer.Stop()
			c.discardTimer = nil
		}
		return
	}
	// 2. 可丢弃且未计时时启动计时器，已到期计时器不重启。
	if c.discardTimer == nil {
		c.discardTimer = time.NewTimer(c.unsavedIdleTimeout)
	}
}

// isUnsaved 对话是否仍未写入共享库？
func (c *runtimeConversation) isUnsaved() bool {
	// 1. 从共享库打开的对话返回 false。
	if c.unsavedStore == nil {
		return false
	}
	// 2. 临时对话算未保存；普通新对话看 metadata 有没有搬完（正在搬时等它结束）：搬完就算已保存，之后索引搬失败也一样；metadata 搬失败仍算未保存。
	return c.unsavedStore.stagedLogs == nil || !c.unsavedStore.stagedLogs.IsPersisted()
}

// isDiscardable 对话是否尚未保存，且没有订阅者和运行中的 Turn？
func (c *runtimeConversation) isDiscardable() bool {
	// 1. 先检查订阅和 Session 运行信息，再查询存储是否已保存。
	return len(c.contentSubscribers) == 0 && len(c.conversationSubscribers) == 0 && !c.isAnyRuntimeSessionRunning() && c.isUnsaved()
}

// subscribe 经对话事件循环登记订阅并取回初始信息。
func (c *runtimeConversation) subscribe(ctx context.Context, sessionID defs.SessionID) (subscribeReply, error) {
	// 1. 提交订阅请求，SessionID 为空时订阅对话标题和运行信息。
	reply := make(chan subscribeReply, 1)
	if err := c.sendRequest(ctx, &subscribeRequested{
		ctx:       ctx,
		sessionID: sessionID,
		reply:     reply,
	}); err != nil {
		return subscribeReply{}, err
	}
	// 2. 等待登记结果和同一时刻的初始信息。
	result := <-reply
	return result, result.err
}

// sendRequest 向对话事件循环发送请求。
func (c *runtimeConversation) sendRequest(ctx context.Context, request runtimeConversationRequest) error {
	// 1. 提交请求，或返回对话退出原因、请求上下文错误。
	select {
	case c.requests <- request:
		return nil
	case <-c.stopped:
		return newConversationStoppedError(c.conversation.GetID(), c.stopReason)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleConversationEvent 更新 Session 和生成中 Item，推送运行信息及内容事件。
func (c *runtimeConversation) handleConversationEvent(ctx context.Context, event conversation.ConversationEvent) {
	// 1. 定位 Session 并保存变更前运行信息。
	sessionID := getEventSessionID(event)
	projection := c.loadOrCreateRuntimeSession(sessionID)
	before := projection.info
	wasRunning := c.isAnyRuntimeSessionRunning()
	// 2. 按启动、运行事件或退出更新日志位置、生成中 Item 和 Session 信息。
	switch event := event.(type) {
	case *conversation.ConversationSessionStarted:
		c.lastSavedOrdinals[event.Session.ID] = event.LastSavedOrdinal
		projection.handleSessionStarted(event.Session)
	case *conversation.ConversationSessionEvent:
		c.applySessionEvent(ctx, sessionID, event.Event)
		projection.handleSessionEvent(event.Event)
	case *conversation.ConversationSessionStopped:
		delete(c.streamingTurns, sessionID)
		projection.handleSessionStopped()
	}
	if wasRunning != c.isAnyRuntimeSessionRunning() {
		signalRunningConversationsChanged(c.runningChanged)
	}
	// 3. 刷新上下文占用，运行信息变化时推送给对话订阅者，再转发内容事件。
	projection.refreshContext(c.models, c.agentDefaultModels)
	if projection.info != before {
		c.publishConversationEvent(ctx, projection.info)
	}
	c.forwardContentEvent(ctx, sessionID, event)
}

// handleConversationReconfigured 在对话原地换上新配置后，更新模型索引和 Executor 设置记录，再关闭全部订阅。
func (c *runtimeConversation) handleConversationReconfigured(ctx context.Context, event *conversation.ConversationReconfigured) {
	// 1. 替换模型索引并重新计算各 Session 的上下文窗口。
	c.models, c.agentDefaultModels = event.Models, event.AgentDefaultModels
	for _, projection := range c.runtimeSessions {
		projection.refreshContext(c.models, c.agentDefaultModels)
	}
	// 2. Executor 设置变化时写库，对话正在关闭也照样写；失败时记 Warn 并仍更新内存记录。
	if !isSameExecutorSettings(c.record.ExecutorSettings, event.ExecutorSettings) {
		record := c.record
		record.ExecutorSettings = event.ExecutorSettings
		if err := c.saveRecord(context.WithoutCancel(ctx), record); err != nil {
			telemetry.GetLogger(ctx).Warn("save conversation executor settings failed", "error", err)
			c.record = record
		}
	}
	// 3. 以 ErrConversationReconfigured 关闭全部订阅。
	c.closeSubscribers(ErrConversationReconfigured)
}

// closeSubscribers 以指定原因关闭全部内容订阅和对话订阅。
func (c *runtimeConversation) closeSubscribers(reason error) {
	// 1. 关闭并移除各 Session 的内容订阅。
	for stream := range c.contentSubscribers {
		removeSubscriber(c.contentSubscribers, stream, reason)
	}
	// 2. 关闭并移除对话标题和运行信息订阅。
	for stream := range c.conversationSubscribers {
		removeSubscriber(c.conversationSubscribers, stream, reason)
	}
}

// isBusy 对话是否有 Session 在执行 Turn 或后台命令尚未结束？
func (c *runtimeConversation) isBusy(ctx context.Context) bool {
	// 1. 查询全部 Session 和后台命令；对话退出或请求取消时 Session 查询按 false 计。
	return c.isAnySessionRunning(ctx) || c.conversation.IsTerminalBusy()
}

// isAnySessionRunning 对话中是否有 Session 正在执行 Turn？
func (c *runtimeConversation) isAnySessionRunning(ctx context.Context) bool {
	// 1. 请求事件循环查询全部 Session，提交失败时返回 false。
	request := &sessionsRunningRequested{reply: make(chan bool, 1)}
	if err := c.sendRequest(ctx, request); err != nil {
		return false
	}
	// 2. 返回事件循环的查询结果。
	return <-request.reply
}

// handleSessionsRunningRequested 回复是否有 Session 正在执行 Turn。
func (c *runtimeConversation) handleSessionsRunningRequested(request *sessionsRunningRequested) {
	// 1. 读取 Session 运行信息并回复。
	request.reply <- c.isAnyRuntimeSessionRunning()
}

// isAnyRuntimeSessionRunning 运行信息里是否有 Session 处于 running 状态（正在执行 Turn）？
func (c *runtimeConversation) isAnyRuntimeSessionRunning() bool {
	// 1. 检查全部 Session，找到 running 即返回 true。
	for _, projection := range c.runtimeSessions {
		if projection.info.State == session.SessionStateRunning {
			return true
		}
	}
	return false
}

// handleSessionRecordReplaceRequested 替换已保存的 Session 记录，推送变化后的运行信息。
func (c *runtimeConversation) handleSessionRecordReplaceRequested(ctx context.Context, request *sessionRecordReplaceRequested) {
	// 1. 替换 Session 记录并重新计算上下文占用。
	projection := c.loadOrCreateRuntimeSession(request.record.ID)
	before := projection.info
	projection.info.Session = request.record
	projection.refreshContext(c.models, c.agentDefaultModels)
	// 2. 运行信息变化时通知对话订阅者。
	if projection.info != before {
		c.publishConversationEvent(ctx, projection.info)
	}
}

// loadOrCreateRuntimeSession 取得或初始化 Session 运行信息。
func (c *runtimeConversation) loadOrCreateRuntimeSession(sessionID defs.SessionID) *runtimeSession {
	// 1. 查询 Session；未找到时登记已停止、上下文占用未知的记录，Session 详情随后由启动事件填入。
	projection, found := c.runtimeSessions[sessionID]
	if !found {
		projection = &runtimeSession{info: SessionRuntimeInfo{
			State:   session.SessionStateStopped,
			Context: ContextUsage{Source: ContextUsageSourceUnknown},
		}}
		c.runtimeSessions[sessionID] = projection
	}
	return projection
}

// publishConversationEvent 向对话订阅者发送运行信息或标题变化。
func (c *runtimeConversation) publishConversationEvent(ctx context.Context, event ConversationEvent) {
	// 1. 按顺序入队，缓冲已满时以 ErrSubscriberTooSlow 关闭订阅并记日志。
	for stream := range c.conversationSubscribers {
		select {
		case stream.events <- event:
		default:
			telemetry.GetLogger(ctx).Warn("subscriber removed", "reason", ErrSubscriberTooSlow.Error())
			removeSubscriber(c.conversationSubscribers, stream, ErrSubscriberTooSlow)
		}
	}
}

// forwardContentEvent 向指定 Session 的内容订阅者转发事件。
func (c *runtimeConversation) forwardContentEvent(ctx context.Context, sessionID defs.SessionID, event conversation.ConversationEvent) {
	// 1. 筛选该 Session 的订阅，缓冲已满时记日志并关闭订阅。
	for stream, subscriber := range c.contentSubscribers {
		if subscriber.sessionID != sessionID {
			continue
		}
		select {
		case stream.events <- event:
		default:
			logCtx := telemetry.ContextWithAttrs(ctx, telemetry.SessionID(string(sessionID)))
			telemetry.GetLogger(logCtx).Warn("subscriber removed", "reason", ErrSubscriberTooSlow.Error())
			removeSubscriber(c.contentSubscribers, stream, ErrSubscriberTooSlow)
		}
	}
}

// applySessionEvent 按 Session 事件更新生成中 Item、已保存位置和根 Session 的消息计数。
func (c *runtimeConversation) applySessionEvent(ctx context.Context, sessionID defs.SessionID, event session.SessionEvent) {
	// 1. Turn 开始时创建生成记录，Agent 事件更新副本，保存事件移除已保存 Item 并累加消息数，Turn 结束时保存根消息数。
	switch event := event.(type) {
	case *session.SessionTurnStarted:
		c.streamingTurns[sessionID] = &streamingTurn{}
	case *session.SessionAgentEvent:
		c.streamingTurns[sessionID].applyAgentEvent(event.Event)
	case *session.SessionItemsSaved:
		for _, logged := range event.Items {
			c.lastSavedOrdinals[sessionID] = logged.Ordinal
			c.streamingTurns[sessionID].removeItem(logged.Item.ID)
		}
		if sessionID == c.conversation.GetRootSessionID() {
			c.rootTurnMessageCount += countConversationMessages(event.Items)
		}
	case *session.SessionTurnFinished:
		delete(c.streamingTurns, sessionID)
		if sessionID == c.conversation.GetRootSessionID() {
			c.saveRootMessageCount(ctx)
		}
	}
}

// saveRootMessageCount 把根 Session 这一轮的消息数加到对话记录并保存。
func (c *runtimeConversation) saveRootMessageCount(ctx context.Context) {
	// 1. 不随对话取消，取出本轮消息数并清零；没有新增消息时返回。
	ctx = context.WithoutCancel(ctx)
	count := c.rootTurnMessageCount
	c.rootTurnMessageCount = 0
	if count == 0 {
		return
	}
	// 2. 将消息数加到记录并保存，失败时只记 Warn，本轮计数不再重试。
	record := c.record
	record.MessageCount += count
	if err := c.saveRecord(ctx, record); err != nil {
		telemetry.GetLogger(ctx).Warn("save conversation message count failed", "error", err)
	}
}

// saveRecord 保存对话记录并同步内存记录。
func (c *runtimeConversation) saveRecord(ctx context.Context, record defs.Conversation) error {
	// 1. 写入存储，失败时保留原内存记录。
	if err := c.mutation.SetConversation(ctx, record); err != nil {
		return err
	}
	// 2. 用写入成功后的当前毫秒更新内存记录；该时间可能与存储分配值相差几毫秒。
	record.UpdatedAt = time.Now().UnixMilli()
	c.record = record
	return nil
}

// handleSubscribeRequested 登记订阅并回复初始信息。
func (c *runtimeConversation) handleSubscribeRequested(request *subscribeRequested) {
	// 1. 未指定 Session 时登记对话订阅，回复标题和全部 Session 运行信息。
	if request.sessionID == "" {
		stream := &subscriptionStream[ConversationEvent]{events: make(chan ConversationEvent, subscriptionBufferSize)}
		c.conversationSubscribers[stream] = c.newRuntimeSubscriber(request, unsubscribeRequested{statusStream: stream})
		request.reply <- subscribeReply{
			statusStream: stream,
			sessions:     c.getSessionRuntimeInfos(),
			title:        defs.TruncateConversationTitle(c.record.Title),
		}
		return
	}
	// 2. 内容订阅读取已转发日志位置；本次未启动过的 Session 从日志读取最新位置。
	lastSavedOrdinal, found := c.lastSavedOrdinals[request.sessionID]
	if !found {
		var err error
		if lastSavedOrdinal, err = dao.GetLastOrdinal(request.ctx, c.query, c.conversation.GetID(), request.sessionID); err != nil {
			request.reply <- subscribeReply{err: err}
			return
		}
		c.lastSavedOrdinals[request.sessionID] = lastSavedOrdinal
	}
	// 3. 复制生成中 Item，登记内容订阅后回复初始信息与事件流。
	reply := subscribeReply{lastSavedOrdinal: lastSavedOrdinal, pendingInputs: session.SessionPendingInputs{Inputs: []session.PendingInput{}}}
	if runtimeSession := c.runtimeSessions[request.sessionID]; runtimeSession != nil && runtimeSession.pendingInputs.Inputs != nil {
		reply.pendingInputs = runtimeSession.pendingInputs
	}
	if turn := c.streamingTurns[request.sessionID]; turn != nil {
		reply.streamingItems = turn.cloneStreamingItems()
	}
	stream := &subscriptionStream[conversation.ConversationEvent]{events: make(chan conversation.ConversationEvent, subscriptionBufferSize)}
	c.contentSubscribers[stream] = c.newRuntimeSubscriber(request, unsubscribeRequested{contentStream: stream})
	reply.contentStream = stream
	request.reply <- reply
}

// setTitle 经对话事件循环提交标题更新。
func (c *runtimeConversation) setTitle(ctx context.Context, title string, onlyIfEmpty bool) error {
	// 1. 提交标题和仅空标题可改的条件，等待保存结果。
	request := &titleChangeRequested{
		ctx:         ctx,
		title:       title,
		onlyIfEmpty: onlyIfEmpty,
		reply:       make(chan error, 1),
	}
	if err := c.sendRequest(ctx, request); err != nil {
		return err
	}
	return <-request.reply
}

// handleTitleChangeRequested 保存变更后的标题并通知对话订阅者。
func (c *runtimeConversation) handleTitleChangeRequested(request *titleChangeRequested) {
	// 1. 截取标题；自动命名已有标题或标题未变化时直接回复成功。
	title := defs.TruncateConversationTitle(request.title)
	if (request.onlyIfEmpty && c.record.Title != "") || c.record.Title == title {
		request.reply <- nil
		return
	}
	// 2. 保存新标题，失败时回复错误且不推送。
	record := c.record
	record.Title = title
	if err := c.saveRecord(request.ctx, record); err != nil {
		request.reply <- fmt.Errorf("set Conversation %s title: %w", record.ID, err)
		return
	}
	// 3. 推送已保存标题，再回复成功。
	c.publishConversationEvent(request.ctx, ConversationTitleChanged{Title: title})
	request.reply <- nil
}

// setArchived 经对话事件循环更新归档状态。
func (c *runtimeConversation) setArchived(ctx context.Context, archived bool) (defs.Conversation, error) {
	// 1. 提交归档状态并等待保存后的记录。
	request := &archiveChangeRequested{
		ctx:      ctx,
		archived: archived,
		reply:    make(chan archiveChangeReply, 1),
	}
	if err := c.sendRequest(ctx, request); err != nil {
		return defs.Conversation{}, err
	}
	result := <-request.reply
	return result.record, result.err
}

// handleArchiveChangeRequested 保存归档状态变化，回复最新记录。
func (c *runtimeConversation) handleArchiveChangeRequested(request *archiveChangeRequested) {
	// 1. 计算归档记录，只在状态变化时写库。
	record, isChanged := getArchivedConversation(c.record, request.archived)
	if isChanged {
		if err := c.saveRecord(request.ctx, record); err != nil {
			request.reply <- archiveChangeReply{err: fmt.Errorf("set Conversation %s archived: %w", record.ID, err)}
			return
		}
	}
	// 2. 回复保存后的记录，状态未变化时回复原记录。
	request.reply <- archiveChangeReply{record: c.record}
}

// getRecord 经事件循环读取对话的最新记录。
func (c *runtimeConversation) getRecord(ctx context.Context) (defs.Conversation, error) {
	// 1. 提交读取请求并返回记录，提交失败时返回对话退出或上下文错误。
	request := &recordRequested{reply: make(chan defs.Conversation, 1)}
	if err := c.sendRequest(ctx, request); err != nil {
		return defs.Conversation{}, err
	}
	return <-request.reply, nil
}

// handleRecordRequested 回复对话记录副本。
func (c *runtimeConversation) handleRecordRequested(request *recordRequested) {
	// 1. 复制最新记录，截取标题后回复。
	record := c.record
	record.Title = defs.TruncateConversationTitle(record.Title)
	request.reply <- record
}

// handleContextUsageRequested 回复 Session 的上下文占用详情。
func (c *runtimeConversation) handleContextUsageRequested(request *contextUsageRequested) {
	// 1. 查找 Session，不在这个对话里时回复错误。
	projection, found := c.runtimeSessions[request.sessionID]
	if !found {
		request.reply <- contextUsageReply{err: fmt.Errorf("Session %s is not in Conversation %s", request.sessionID, c.conversation.GetID())}
		return
	}
	// 2. 复制下一次请求占用、最近普通调用和统一测量的分类信息。
	usage := SessionContextUsage{
		Window:      projection.info.Context.Window,
		NextRequest: projection.info.Context,
	}
	if projection.lastRequest != nil {
		lastRequest := *projection.lastRequest
		usage.LastRequest = &lastRequest
	}
	if projection.measurement != nil {
		maxOutputTokens := 0
		if model := c.models[projection.getNextRequestModel(c.models, c.agentDefaultModels)]; model != nil {
			maxOutputTokens = model.MaxOutputTokens
		}
		usage.ThresholdTokens = agent_context.GetThresholdTokens(projection.info.Context.Window, maxOutputTokens)
		usage.SystemTokens = projection.measurement.SystemTokens
		usage.ToolTokens = projection.measurement.ToolTokens
		usage.Items = slices.Clone(projection.measurement.Items)
	}
	// 3. 回复上下文详情。
	request.reply <- contextUsageReply{usage: usage}
}

// newRuntimeSubscriber 创建订阅记录并监听订阅方退出。
func (c *runtimeConversation) newRuntimeSubscriber(request *subscribeRequested, unsubscribe unsubscribeRequested) *runtimeSubscriber {
	// 1. 登记 Session 和上下文监听；结束后携带 context.Cause 发送退订请求，对话已退出时放弃请求。
	return &runtimeSubscriber{
		sessionID: request.sessionID,
		stopWatching: context.AfterFunc(request.ctx, func() {
			unsubscribe.reason = context.Cause(request.ctx)
			_ = c.sendRequest(context.Background(), &unsubscribe)
		}),
	}
}

// getSessionRuntimeInfos 按 SessionID 排序返回全部 Session 运行信息。
func (c *runtimeConversation) getSessionRuntimeInfos() []SessionRuntimeInfo {
	// 1. 收集运行信息副本。
	infos := make([]SessionRuntimeInfo, 0, len(c.runtimeSessions))
	for _, projection := range c.runtimeSessions {
		infos = append(infos, projection.info)
	}
	// 2. 按 SessionID 排序后返回。
	slices.SortFunc(infos, func(a, b SessionRuntimeInfo) int { return cmp.Compare(a.Session.ID, b.Session.ID) })
	return infos
}

// removeSubscriber 移除订阅并以指定原因关闭事件通道。
func removeSubscriber[T any](subscribers map[*subscriptionStream[T]]*runtimeSubscriber, stream *subscriptionStream[T], reason error) {
	// 1. 查找订阅，已移除时返回。
	subscriber, found := subscribers[stream]
	if !found {
		return
	}
	// 2. 移除登记，停止上下文监听，写入关闭原因后关闭通道。
	delete(subscribers, stream)
	subscriber.stopWatching()
	stream.closeReason = reason
	close(stream.events)
}

// isStopped 对话任务与事件循环是否都已退出？
func (c *runtimeConversation) isStopped() bool {
	// 1. 检查 stopped 是否已关闭。
	select {
	case <-c.stopped:
		return true
	default:
		return false
	}
}

// runtimeSubscriber 是事件循环登记的一个订阅。
type runtimeSubscriber struct {
	// sessionID 为空时是对话标题与运行信息订阅，否则是这个 Session 的内容订阅。
	sessionID defs.SessionID
	// stopWatching 停止监听订阅方 ctx。
	stopWatching func() bool
}

// runtimeSession 是 runtimeConversation 为一个 Session 维护的运行信息；只由事件循环读写。
type runtimeSession struct {
	// info 是推给对话订阅者的运行信息。
	info SessionRuntimeInfo
	// pendingInputs 是 Session 最近发布的轻量待处理列表。
	pendingInputs session.SessionPendingInputs
	// measurement 是本轮 AgentContext 最近一次统一测量。
	measurement *agent_context.ContextMeasurement
	// lastRequest 是最近一次上报了用量的普通调用。
	lastRequest *LastRequestUsage
}

// newRuntimeSession 按已有 Session 信息创建运行记录。
func newRuntimeSession(info conversation.SessionInfo) *runtimeSession {
	// 1. 保存记录、状态、Turn 信息和累计用量，上下文占用设为未知。
	return &runtimeSession{info: SessionRuntimeInfo{
		Session:        info.Session,
		State:          info.Status.State,
		ActiveTurnID:   info.Status.ActiveTurnID,
		LastTurnID:     info.Status.LastTurnID,
		LastTurnStatus: info.Status.LastTurnStatus,
		Context:        ContextUsage{Source: ContextUsageSourceUnknown},
		Usage:          info.Usage,
	}}
}

// handleSessionStarted 记录 Session 启动。
func (s *runtimeSession) handleSessionStarted(record defs.Session) {
	// 1. 替换 Session 记录并将状态设为空闲，清除活动 Turn；保留用量和上下文占用。
	s.info.Session = record
	s.info.State = session.SessionStateIdle
	s.info.ActiveTurnID = ""
	s.info.ActiveTurnModel = defs.ModelSelection{}
}

// handleSessionStopped 记录 Session 退出。
func (s *runtimeSession) handleSessionStopped() {
	// 1. 将状态改为已停止，清除活动 Turn，保留最终用量和上下文占用。
	s.info.State = session.SessionStateStopped
	s.info.ActiveTurnID = ""
	s.info.ActiveTurnModel = defs.ModelSelection{}
}

// handleSessionEvent 按 Session 事件更新 Turn 状态和模型用量。
func (s *runtimeSession) handleSessionEvent(event session.SessionEvent) {
	// 1. Turn 开始和结束更新状态及模型，新 Turn 清除旧测量，Agent 事件更新测量和调用信息。
	switch event := event.(type) {
	case *session.SessionPendingInputsUpdated:
		s.pendingInputs = event.PendingInputs
	case *session.SessionTurnStarted:
		s.info.State = session.SessionStateRunning
		s.measurement = nil
		s.info.ActiveTurnID = event.TurnID
		s.info.ActiveTurnModel = event.Model
	case *session.SessionTurnFinished:
		s.info.State = session.SessionStateIdle
		s.info.ActiveTurnID = ""
		s.info.ActiveTurnModel = defs.ModelSelection{}
		s.info.LastTurnID = event.TurnID
		s.info.LastTurnStatus = event.Status
	case *session.SessionAgentEvent:
		s.handleAgentEvent(event.Event)
	}
}

// handleAgentEvent 按统一测量和模型调用结束事件更新上下文与用量。
func (s *runtimeSession) handleAgentEvent(event *agent.AgentEvent) {
	// 1. 测量事件替换统一测量记录。
	switch event.Type {
	case agent.AgentEventContextMeasured:
		s.measurement = event.Measurement
	case agent.AgentEventModelCallFinished:
		call := event.ModelCall
		// 2. 未上报 Usage 时结束本次处理，有用量时累加到 Session。
		if call.Usage == nil {
			break
		}
		s.info.Usage = s.info.Usage.Add(*call.Usage)
		// 3. 普通调用上报 Usage 后，保存实际用量和请求前的本地估算。
		if call.Purpose == agent.ModelCallPurposeConversation {
			s.lastRequest = &LastRequestUsage{
				ModelCallID:     call.ModelCallID,
				Model:           s.info.ActiveTurnModel.Ref,
				Usage:           *call.Usage,
				EstimatedTokens: s.measurement.EstimatedTokens,
			}
		}
	}
}

// refreshContext 按下一次请求的模型重新计算窗口和预计占用。
func (s *runtimeSession) refreshContext(models map[defs.ModelRef]*defs.ModelDefinition, agentDefaultModels map[string]defs.ModelRef) {
	// 1. 解析下一次模型，从目录读取上下文窗口。
	nextModel := s.getNextRequestModel(models, agentDefaultModels)
	s.info.Context.Window = 0
	if model, found := models[nextModel]; found {
		s.info.Context.Window = model.ContextWindow
	}
	// 2. 使用统一测量结果；模型变化时展示本地估算，没有本轮测量时标记未知。
	switch {
	case s.measurement != nil && s.measurement.IsUsageBased && s.measurement.Model == nextModel:
		s.info.Context.Tokens = s.measurement.RequestTokens
		s.info.Context.Source = ContextUsageSourceActual
	case s.measurement != nil:
		s.info.Context.Tokens = s.measurement.EstimatedTokens
		s.info.Context.Source = ContextUsageSourceEstimated
	default:
		s.info.Context.Tokens = 0
		s.info.Context.Source = ContextUsageSourceUnknown
	}
}

// getNextRequestModel 返回下一次请求使用的模型引用。
func (s *runtimeSession) getNextRequestModel(models map[defs.ModelRef]*defs.ModelDefinition, agentDefaultModels map[string]defs.ModelRef) defs.ModelRef {
	// 1. 有活动 Turn 时返回它的模型。
	if s.info.ActiveTurnModel.Ref != (defs.ModelRef{}) {
		return s.info.ActiveTurnModel.Ref
	}
	// 2. 否则解析 Session 所选模型，零值选择使用 Agent 默认模型；模型不在目录或 Session 尚无记录时返回零值。
	if model, _, found := defs.GetSelectedModel(models, s.info.Session.Model, agentDefaultModels[s.info.Session.AgentName]); found {
		return model.Ref
	}
	return defs.ModelRef{}
}

// streamingTurn 是一个 Session 活动 Turn 中已开始、尚未保存的 Item 副本，按首次出现顺序；由转发的 Agent 事件累积。
type streamingTurn struct {
	items []StreamingItem
}

// applyAgentEvent 按 Agent 事件维护生成中 Item 副本。
func (t *streamingTurn) applyAgentEvent(event *agent.AgentEvent) {
	// 1. 按 ItemIndex 查找已有副本。
	index := slices.IndexFunc(t.items, func(entry StreamingItem) bool { return entry.ItemIndex == event.ItemIndex })
	// 2. 开始时克隆加入，增量时合并，完成或中断时替换为完整 Item；无需保存的中断 Item 移除。
	switch event.Type {
	case agent.AgentEventItemStarted:
		t.items = append(t.items, StreamingItem{
			ItemIndex: event.ItemIndex,
			Item:      event.Item.Clone(),
		})
	case agent.AgentEventItemDelta:
		if index >= 0 {
			agent.ApplyItemDelta(t.items[index].Item, event.Delta)
		}
	case agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
		switch {
		case event.Item == nil && index >= 0:
			t.items = slices.Delete(t.items, index, index+1)
		case event.Item != nil && index >= 0:
			t.items[index].Item = event.Item.Clone()
		}
	}
}

// removeItem 移除已经保存的生成中 Item。
func (t *streamingTurn) removeItem(itemID defs.ItemID) {
	// 1. 按 ItemID 删除副本。
	t.items = slices.DeleteFunc(t.items, func(entry StreamingItem) bool { return entry.Item.ID == itemID })
}

// cloneStreamingItems 返回生成中 Item 的深拷贝，保持原顺序。
func (t *streamingTurn) cloneStreamingItems() []StreamingItem {
	// 1. 逐个深拷贝 Item，保留 ItemIndex。
	items := make([]StreamingItem, len(t.items))
	for index, entry := range t.items {
		items[index] = StreamingItem{
			ItemIndex: entry.ItemIndex,
			Item:      entry.Item.Clone(),
		}
	}
	return items
}
