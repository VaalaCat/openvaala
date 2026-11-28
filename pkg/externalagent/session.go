package externalagent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/docs"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// ExternalSession 在原生输入、状态和轮次事件之外提供外部历史与驱动配置。
type ExternalSession interface {
	session.Session
	// GetTerminalManager 返回外部会话持有的后台命令集合，供协作工具使用。
	GetTerminalManager() terminal.TerminalManager
	// GetHistoryDAO 返回完整回放后的内存 DAO，首次恢复期间返回 restoring。原生历史使用 Conversation DAO。
	GetHistoryDAO() (query dao.Query, restoring bool)
	// SetOption 通过驱动选择模型、模式等选项；原生 SelectModel 只选择模型。
	SetOption(ctx context.Context, selection defs.ExternalOptionSelection) error
	// Reload 用最新定义替换外部进程并等到就绪；原生无对应的单会话进程。
	Reload(ctx context.Context, launch ExternalSessionLaunch) error
}

// externalSessionImpl 的业务状态由 Run 串行读写。
type externalSessionImpl struct {
	record          defs.Session
	agentType       defs.AgentType
	terminalManager terminal.TerminalManager
	forkSummary     string
	started         atomic.Bool
	stopped         chan struct{}
	stopReason      error
	requests        chan sessionRequest
	events          chan<- session.SessionEvent
	tasks           sync.WaitGroup

	launch         ExternalSessionLaunch
	driver         ExternalAgentSession
	driverCtx      context.Context
	cancelDriver   context.CancelFunc
	driverEvents   <-chan ExternalSessionEvent
	driverExit     <-chan error
	driverReady    bool
	restoreErr     error
	reloadReply    chan error
	interruptTimer *time.Timer

	externalState session.SessionExternalStateUpdated

	historyDB    sqlite.Database
	historyQuery dao.Query
	historyMut   dao.Mutation
	// historyReady 在首次完整 Ready 后发布 historyQuery，重载期间继续借出同一份历史。
	historyReady        atomic.Bool
	lastSavedOrdinal    int64
	inbox               *session.SessionInbox
	pendingContextItems []*defs.Item
	steerWaiters        []chan sessionReply[[]*defs.Item]
	firstPromptUsed     bool
	lastTurnID          defs.TurnID
	lastTurnStatus      defs.TurnStatus

	turn         *defs.Turn
	turnInputIDs []defs.ItemID
	// delivery 在驱动确认开轮或补充前持有整批借出记录。
	delivery *inputDelivery
	// pendingFinish 暂存早于输入 RPC 返回的轮次终态。
	pendingFinish   *ExternalTurnFinishedEvent
	modelCallID     defs.ModelCallID
	draftItems      []draftItem
	savedDraftCount int
	replayUser      *draftItem
}

// ExternalSessionOpt 接收 Conversation 已解析的可启动记录及其 Workspace。装配方持有并关闭 Workspace。
type ExternalSessionOpt struct {
	// AgentType 用于按实际驱动类型报告不支持的原生操作。
	AgentType defs.AgentType
	Session   defs.Session
	Launch    ExternalSessionLaunch
	// ForkSummary 是新会话首次发送时附上的摘要，只驻留在运行实例中。
	ForkSummary string
}

// NewExternalSession 只校验身份、执行位置和驱动依赖。
func NewExternalSession(opt *ExternalSessionOpt) (ExternalSession, error) {
	if opt == nil || opt.Session.ID == "" || opt.Session.ConversationID == "" || opt.Launch.Provider == nil || opt.Launch.Workspace == nil {
		return nil, errors.New("external session requires session identity, provider and workspace")
	}
	if opt.AgentType == "" {
		return nil, errors.New("external session requires an agent type")
	}
	terminals, err := terminal.NewTerminalManager(terminal.NewTerminalManagerOpt())
	if err != nil {
		return nil, err
	}
	return &externalSessionImpl{
		record:          opt.Session,
		agentType:       opt.AgentType,
		terminalManager: terminals,
		forkSummary:     opt.ForkSummary,
		launch:          opt.Launch,
		requests:        make(chan sessionRequest), stopped: make(chan struct{}),
	}, nil
}

// Run 与原生 Run 一样串行收件并在返回前等待任务结束；历史从驱动回放到独立内存库。
func (s *externalSessionImpl) Run(ctx context.Context, events chan<- session.SessionEvent) (runErr error) {
	if events == nil {
		return errors.New("session events channel is required")
	}
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("session can only run once")
	}
	defer close(events)
	s.events = events
	ctx, cancel := context.WithCancel(defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{
		ConversationID: s.record.ConversationID, SessionID: s.GetID(),
		ParentSessionID: s.record.ParentSessionID, AgentName: s.record.AgentName,
	}))
	defer close(s.stopped)
	s.tasks.Go(func() { _ = s.terminalManager.Run(ctx) })
	if runErr = ctx.Err(); runErr == nil {
		runErr = s.openHistory(ctx)
	}
	if runErr == nil {
		s.startDriver(ctx)
		runErr = s.runEventLoop(ctx)
	}

	cancel()
	if s.interruptTimer != nil {
		s.interruptTimer.Stop()
	}
	// 进程可能在退出前同步交出最后一条通知，继续接收直至 Run 返回。
	for s.driverExit != nil {
		select {
		case <-s.driverExit:
			s.driverExit = nil
		case <-s.driverEvents:
		}
	}
	s.tasks.Wait()
	s.launch.closeSkillWorkspace(ctx)
	if !s.historyReady.Load() {
		s.lastTurnID, s.lastTurnStatus = "", ""
		runErr = fmt.Errorf("external session cannot be restored: %w", runErr)
		events <- &session.SessionHistoryRestored{Err: runErr}
	} else if s.activeTurnID() != "" && errors.Is(runErr, ctx.Err()) {
		// 主动关闭会话时也把已收到的输出和中断终态交给 Conversation。
		err := s.finishTurn(context.WithoutCancel(ctx), &ExternalTurnFinishedEvent{TurnID: s.turn.ID, Status: defs.TurnStatusInterrupted}, false)
		runErr = errors.Join(runErr, err)
	}
	s.historyReady.Store(false)
	if s.historyDB != nil {
		runErr = errors.Join(runErr, s.historyDB.Close())
	}
	s.stopReason = runErr
	return runErr
}

// runEventLoop 按到达顺序处理公开请求、任务结果、驱动通知和中断期限。
func (s *externalSessionImpl) runEventLoop(ctx context.Context) error {
	for {
		var deadline <-chan time.Time
		if s.interruptTimer != nil {
			deadline = s.interruptTimer.C
		}
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-s.requests:
			err = s.handleRequest(ctx, req)
		case event := <-s.driverEvents:
			err = s.handleDriverEvent(ctx, event)
		case cause := <-s.driverExit:
			err = s.handleDriverExit(ctx, cause)
		case <-deadline:
			s.cancelDriver()
			err = s.finishAfterDelivery(ctx, &ExternalTurnFinishedEvent{TurnID: s.turn.ID, Status: defs.TurnStatusInterrupted})
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.scheduleTurnInput(ctx); err != nil {
			return err
		}
	}
}

// activeTurnID 只将实时轮交给请求方，回放中的轮尚属于历史恢复。
func (s *externalSessionImpl) activeTurnID() defs.TurnID {
	if s.turn == nil || !s.historyReady.Load() {
		return ""
	}
	return s.turn.ID
}

// handleRequest 串行处理收件箱操作、驱动调用和任务结果。
func (s *externalSessionImpl) handleRequest(ctx context.Context, request sessionRequest) error {
	switch req := request.(type) {
	case inputRequested:
		s.handleInput(ctx, req)
	case contextItemsRequested:
		if s.restoreErr == nil {
			s.pendingContextItems = append(s.pendingContextItems, req.items...)
		}
		req.reply <- s.restoreErr
	case statusRequested:
		status := session.SessionStatus{
			State: session.SessionStateIdle, ActiveTurnID: s.activeTurnID(),
			LastTurnID: s.lastTurnID, LastTurnStatus: s.lastTurnStatus,
			PendingInputCount: s.inbox.GetPendingInputCount(), PendingContextItemCount: len(s.pendingContextItems),
		}
		if status.ActiveTurnID != "" {
			status.State = session.SessionStateRunning
		}
		req.reply <- sessionReply[session.SessionStatus]{value: status}
	case withdrawRequested:
		items, err := s.inbox.Withdraw(ctx, req.itemID)
		req.reply <- sessionReply[[]*defs.Item]{items, err}
		if err == nil {
			s.publishPendingInputs()
		}
	case unreadRequested:
		items := s.inbox.GetUnreadSteer()
		if req.wait && len(items) == 0 && s.activeTurnID() != "" {
			s.steerWaiters = append(s.steerWaiters, req.reply)
		} else {
			req.reply <- sessionReply[[]*defs.Item]{value: items}
		}
	case pendingInputRequested:
		s.handlePendingInput(ctx, req)
	case interruptRequested:
		id := s.activeTurnID()
		if id == "" {
			req.reply <- sessionReply[defs.TurnID]{}
			break
		}
		if err := s.inbox.SaveStopRequest(ctx, id, s.record.ParentSessionID == ""); err != nil {
			req.reply <- sessionReply[defs.TurnID]{id, err}
			break
		}
		s.interruptTurn(ctx, interruptReturned{turnID: id, reply: req.reply})
	case optionRequested:
		if !s.driverReady || s.reloadReply != nil {
			req.reply <- errors.New("external session is not ready")
			break
		}
		driver, driverCtx := s.driver, s.driverCtx
		s.tasks.Go(func() {
			callCtx, cancel := context.WithCancel(req.ctx)
			stop := context.AfterFunc(driverCtx, cancel)
			err := driver.SetOption(callCtx, req.selection)
			stop()
			cancel()
			s.sendResult(ctx, optionSet{req.reply, err})
		})
	case reloadRequested:
		s.handleReload(ctx, req)
	case inputDelivered:
		return s.handleDelivery(ctx, req)
	case optionSet:
		req.reply <- req.err
	case interruptReturned:
		if req.reply != nil {
			req.reply <- sessionReply[defs.TurnID]{req.turnID, req.err}
		}
		if req.inputReply != nil {
			req.inputReply <- req.err
		}
	}
	return nil
}

// handleDriverEvent 将后端事实按到达顺序写入内存历史或交给 Conversation。
func (s *externalSessionImpl) handleDriverEvent(ctx context.Context, event ExternalSessionEvent) error {
	switch e := event.(type) {
	case *ExternalSessionReadyEvent:
		if s.driverReady {
			return nil
		}
		firstReady := !s.historyReady.Load()
		var restored session.SessionHistoryRestored
		if firstReady {
			if err := s.flushReplayUser(ctx); err != nil {
				return err
			}
			if s.turn != nil {
				if err := s.finishTurn(ctx, &ExternalTurnFinishedEvent{TurnID: s.turn.ID, Status: defs.TurnStatusCompleted}, true); err != nil {
					return err
				}
			}
			usage, err := dao.GetSessionUsage(ctx, s.historyQuery, s.GetID())
			if err != nil {
				return err
			}
			restored = session.SessionHistoryRestored{
				LastSavedOrdinal: s.lastSavedOrdinal,
				LastTurnID:       s.lastTurnID,
				LastTurnStatus:   s.lastTurnStatus,
				Usage:            usage,
			}
			s.historyReady.Store(true)
		}
		s.driverReady = true
		s.record.External = &e.Binding
		s.externalState = session.SessionExternalStateUpdated{Capabilities: e.Capabilities, Options: e.Options, Commands: e.Commands, MCPServers: e.MCPServers}
		s.events <- &session.SessionExternalBindingUpdated{Binding: e.Binding}
		s.publishExternalState()
		if firstReady {
			s.events <- &restored
		}
		if s.reloadReply != nil {
			s.events <- &session.SessionExternalReloaded{}
			s.reloadReply <- nil
			s.reloadReply = nil
		}
	case *ExternalBindingUpdatedEvent:
		s.record.External = &e.Binding
		s.events <- &session.SessionExternalBindingUpdated{Binding: e.Binding}
	case *ExternalOptionsUpdatedEvent:
		s.externalState.Options = e.Options
		if s.driverReady {
			s.publishExternalState()
		}
	case *ExternalCommandsUpdatedEvent:
		s.externalState.Commands = e.Commands
		if s.driverReady {
			s.publishExternalState()
		}
	case *ExternalAgentStreamEvent:
		if e.Replaying {
			if s.historyReady.Load() {
				return nil
			}
			return s.handleReplay(ctx, e.Event)
		}
		if id := s.activeTurnID(); id != "" && e.TurnID == id {
			return s.handleOutput(ctx, e.Event, false)
		}
	case *ExternalTurnFinishedEvent:
		switch e.Status {
		case defs.TurnStatusCompleted, defs.TurnStatusFailed, defs.TurnStatusInterrupted:
		default:
			return nil
		}
		if id := s.activeTurnID(); id == "" || e.TurnID != id {
			return nil
		}
		return s.finishAfterDelivery(ctx, e)
	}
	return nil
}

func (s *externalSessionImpl) openHistory(ctx context.Context) error {
	db, err := sqlite.OpenDatabase(sqlite.NewMemoryDatabaseOpt(dao.RecordNamespaces()...))
	if err != nil {
		return err
	}
	s.historyDB = db
	s.historyQuery, s.historyMut, err = dao.NewDAO(dao.NewDAOOpt(db.Record(), db.Log()))
	if err != nil {
		return err
	}
	record := s.record
	s.inbox, err = session.NewSessionInbox(&session.SessionInboxOpt{ConversationID: record.ConversationID, SessionID: record.ID, Query: s.historyQuery, Mutation: s.historyMut})
	if err != nil {
		return err
	}
	if record.ParentSessionID != "" {
		saved, err := s.historyMut.AppendItems(ctx, record.ConversationID, record.ID, &defs.Item{
			ID: defs.NewItemID(), Kind: defs.ItemSessionFork, SessionFork: &defs.SessionFork{SourceSessionID: record.ParentSessionID},
		})
		if err != nil {
			return err
		}
		s.lastSavedOrdinal = int64(saved[0].Ordinal)
	}
	return nil
}

// startDriver 在目标机同步薄 skill 后启动进程；每个驱动有独立的事件通道和退出通知。
func (s *externalSessionImpl) startDriver(ctx context.Context) {
	launch := s.launch
	req := ExternalSessionOpenRequest{
		Ephemeral: launch.Ephemeral, Workspace: launch.Workspace, LaunchEnv: launch.LaunchEnv, ExecutorKind: launch.ExecutorKind,
		LocalMCPURL: launch.LocalMCPURL, MCPServers: launch.MCPServers, AttachmentDirectory: launch.AttachmentDirectory,
	}
	if s.record.External == nil || s.record.External.SessionID == "" {
		req.ExecutorID, req.Cwd, req.InitialOptions = launch.ExecutorID, launch.Cwd, launch.InitialOptions
	} else {
		req.Binding = s.record.External
		req.ExecutorID, req.Cwd = s.record.External.ExecutorID, s.record.External.Cwd
	}
	driver := launch.Provider.OpenSession(req)
	skillWorkspace := launch.SkillWorkspace
	driverCtx, cancel := context.WithCancel(ctx)
	updates := make(chan ExternalSessionEvent)
	exit := make(chan error, 1)
	s.driver, s.driverCtx, s.cancelDriver = driver, driverCtx, cancel
	s.driverEvents, s.driverExit = updates, exit
	s.tasks.Go(func() {
		syncExternalSkill(driverCtx, skillWorkspace)
		exit <- driver.Run(driverCtx, updates)
	})
}

// closeSkillWorkspace 释放这次启动独占的主目录连接。
func (launch ExternalSessionLaunch) closeSkillWorkspace(ctx context.Context) {
	if launch.SkillWorkspace != nil && launch.SkillWorkspace != launch.Workspace {
		if err := launch.SkillWorkspace.Close(); err != nil {
			telemetry.GetLogger(ctx).Warn("close external skill workspace failed", "error", err)
		}
	}
}

// syncExternalSkill 在目标机用户主目录下同步两份薄 skill，单个目录不可写时仍尝试另一个目录。
func syncExternalSkill(ctx context.Context, ws workspace.Workspace) {
	if ws == nil {
		telemetry.GetLogger(ctx).Warn("external skill home workspace unavailable")
		return
	}
	data := []byte(docs.ExternalAgentSkill)
	for _, prefix := range []string{".claude", ".agents"} {
		dir := filepath.Join(ws.GetRootDirectory(), prefix, "skills", "openvaala")
		path := filepath.Join(dir, "SKILL.md")
		existing, err := ws.ReadFile(ctx, path, 0, len(data)+1)
		if err == nil && existing.EOF && string(existing.Content) == docs.ExternalAgentSkill {
			continue
		}
		if err = ws.CreateDirectory(ctx, dir); err == nil {
			err = ws.WriteFile(ctx, path, data, 0644, false)
		}
		if err != nil {
			telemetry.GetLogger(ctx).Warn("external skill sync failed", "path", path, "error", err)
		}
	}
}

func (s *externalSessionImpl) handleDriverExit(ctx context.Context, cause error) error {
	wasReady := s.driverReady
	s.cancelDriver()
	s.driver, s.driverExit, s.driverEvents = nil, nil, nil
	s.driverReady = false
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cause == nil {
		cause = errors.New("process exited")
	}
	if !s.historyReady.Load() {
		return cause
	}
	if s.turn != nil {
		return s.finishAfterDelivery(ctx, &ExternalTurnFinishedEvent{TurnID: s.turn.ID, Status: defs.TurnStatusFailed, Err: fmt.Errorf("external agent process exited: %w", cause)})
	}
	if !wasReady {
		// 曾经完整恢复，但这次重开失败。完整历史仍可读，已收下的新输入得到明确的失败轮。
		s.restoreErr = fmt.Errorf("external session cannot be restored: %w", cause)
		if s.reloadReply != nil {
			s.reloadReply <- s.restoreErr
			s.reloadReply = nil
		}
		return s.startQueuedTurn(ctx)
	}
	return nil
}

func (s *externalSessionImpl) handleReload(ctx context.Context, req reloadRequested) {
	defer func() {
		if req.launch.SkillWorkspace != s.launch.SkillWorkspace {
			req.launch.closeSkillWorkspace(ctx)
		}
	}()
	if s.reloadReply != nil || !s.historyReady.Load() || s.driver != nil && !s.driverReady {
		req.reply <- session.ErrSessionBusy
		return
	}
	if s.record.External == nil || s.record.External.SessionID == "" {
		req.reply <- errors.New("external session cannot be restored: missing binding")
		return
	}
	if req.launch.Provider == nil || req.launch.Workspace == nil {
		req.reply <- errors.New("external session reload requires provider and workspace")
		return
	}
	s.restoreErr = nil
	if s.launch.SkillWorkspace != req.launch.SkillWorkspace {
		s.launch.closeSkillWorkspace(ctx)
	}
	s.launch, s.reloadReply = req.launch, req.reply
	if s.driver == nil {
		return
	}
	if s.activeTurnID() != "" {
		s.interruptTurn(ctx, interruptReturned{turnID: s.turn.ID})
	} else {
		s.cancelDriver()
	}
}

func (s *externalSessionImpl) handleInput(ctx context.Context, req inputRequested) {
	if s.restoreErr != nil {
		req.reply <- sessionReply[defs.ItemID]{err: s.restoreErr}
		return
	}
	input := req.input
	if id, ok := s.inbox.GetAcceptedInputID(input.InputID); ok {
		req.reply <- sessionReply[defs.ItemID]{value: id}
		return
	}
	id := s.activeTurnID()
	if id != "" && input.Mode == session.SessionInputModeIdleOnly {
		req.reply <- sessionReply[defs.ItemID]{err: session.ErrSessionBusy}
		return
	}
	if input.Mode == session.SessionInputModeSteer && id != "" {
		if !s.externalState.Capabilities.SupportsSteer {
			input.Mode = session.SessionInputModeNormal
			if isUserMessage(input.Item) {
				input.Mode = session.SessionInputModeInterrupt
			}
		} else if s.interruptTimer != nil || s.pendingFinish != nil {
			input.Mode = session.SessionInputModeNormal
		}
	}
	itemID, err := s.inbox.Add(ctx, &input, id)
	req.reply <- sessionReply[defs.ItemID]{itemID, err}
	if err != nil {
		return
	}
	s.publishPendingInputs()
	if len(s.steerWaiters) > 0 {
		if items := s.inbox.GetUnreadSteer(); len(items) > 0 {
			s.wakeSteerWaiters(items)
		}
	}
	if id != "" && input.Mode == session.SessionInputModeInterrupt && isUserMessage(input.Item) {
		s.interruptTurn(ctx, interruptReturned{turnID: id})
	}
}

func (s *externalSessionImpl) handlePendingInput(ctx context.Context, req pendingInputRequested) {
	if req.turnID == "" || req.turnID != s.activeTurnID() || s.pendingFinish != nil {
		req.reply <- errors.New("turn is no longer running")
		return
	}
	if req.interrupt || !s.externalState.Capabilities.SupportsSteer {
		if err := s.inbox.InterruptTurnWithInput(ctx, req.turnID, req.itemID); err != nil {
			req.reply <- err
			return
		}
		s.publishPendingInputs()
		s.interruptTurn(ctx, interruptReturned{turnID: req.turnID, inputReply: req.reply})
		return
	}
	if s.delivery != nil || s.interruptTimer != nil {
		req.reply <- session.ErrSessionBusy
		return
	}
	if err := s.inbox.SteerInput(ctx, req.itemID, req.turnID); err != nil {
		req.reply <- err
		return
	}
	s.wakeSteerWaiters(s.inbox.GetUnreadSteer())
	s.deliverSteer(ctx, req.reply)
}

// scheduleTurnInput 按待办或重载需要重开已退出的驱动，再向就绪驱动交付轮次输入。
func (s *externalSessionImpl) scheduleTurnInput(ctx context.Context) error {
	if s.driver == nil && s.turn == nil && s.restoreErr == nil && (s.reloadReply != nil || s.inbox.GetPendingInputCount() > 0) {
		s.startDriver(ctx)
	}
	if !s.driverReady || s.driverCtx.Err() != nil || s.delivery != nil || s.reloadReply != nil || s.interruptTimer != nil {
		return nil
	}
	if s.turn != nil {
		if s.externalState.Capabilities.SupportsSteer {
			s.deliverSteer(ctx, nil)
		}
		return nil
	}
	return s.startQueuedTurn(ctx)
}

// startQueuedTurn 整批分配输入，先记录轮次；驱动恢复失败时也把已经接收的这一批交代给订阅方。
func (s *externalSessionImpl) startQueuedTurn(ctx context.Context) error {
	if s.inbox.GetPendingInputCount() == 0 {
		return nil
	}
	id := defs.NewTurnID()
	batch, err := s.inbox.TakeWholeBatch(ctx, id)
	if err != nil || len(batch) == 0 {
		return err
	}
	delivery := &inputDelivery{opening: true, items: s.pendingContextItems}
	s.pendingContextItems = nil
	for _, group := range batch {
		delivery.groupIDs = append(delivery.groupIDs, group.Items[0].ID)
		delivery.items = append(delivery.items, group.Items...)
	}
	s.turnInputIDs = delivery.groupIDs
	if err := s.startTurn(ctx, id, delivery.items, false); err != nil {
		return err
	}
	s.delivery = delivery
	s.publishPendingInputs()
	if s.restoreErr != nil {
		return s.finishTurn(ctx, &ExternalTurnFinishedEvent{TurnID: id, Status: defs.TurnStatusFailed, Err: s.restoreErr}, false)
	}
	var sessionInfo *defs.Session
	if !s.firstPromptUsed {
		sessionInfo = &s.record
	}
	items, err := PrepareExternalPrompt(delivery.items, s.forkSummary, sessionInfo)
	if err != nil {
		return s.handleDelivery(ctx, inputDelivered{id, err})
	}
	driver, driverCtx := s.driver, s.driverCtx
	s.tasks.Go(func() {
		err := driver.StartTurn(driverCtx, id, items)
		s.sendResult(ctx, inputDelivered{id, err})
	})
	return nil
}

func (s *externalSessionImpl) deliverSteer(ctx context.Context, reply chan error) {
	id := s.turn.ID
	groups := s.inbox.TakeSteer(id)
	if len(groups) == 0 {
		return
	}
	delivery := &inputDelivery{reply: reply}
	for _, group := range groups {
		delivery.groupIDs = append(delivery.groupIDs, group[0].ID)
		delivery.items = append(delivery.items, group...)
	}
	s.delivery = delivery
	s.publishPendingInputs()
	driver, driverCtx := s.driver, s.driverCtx
	s.tasks.Go(func() {
		err := driver.SteerInput(driverCtx, id, delivery.items)
		s.sendResult(ctx, inputDelivered{id, err})
	})
}

func (s *externalSessionImpl) handleDelivery(ctx context.Context, result inputDelivered) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.turn == nil || s.turn.ID != result.turnID || s.delivery == nil {
		return nil
	}
	delivery := s.delivery
	if result.err != nil && delivery.opening {
		if s.pendingFinish != nil && s.driverCtx.Err() != nil {
			return s.finishTurn(ctx, s.pendingFinish, false)
		}
		return s.finishTurn(ctx, &ExternalTurnFinishedEvent{TurnID: result.turnID, Status: defs.TurnStatusFailed, Err: result.err}, false)
	}
	s.delivery = nil
	if result.err == nil {
		if delivery.opening {
			s.firstPromptUsed = true
			s.forkSummary = ""
		} else {
			if err := s.appendInputs(ctx, delivery.items, false); err != nil {
				return err
			}
			s.turnInputIDs = append(s.turnInputIDs, delivery.groupIDs...)
		}
		if err := s.inbox.ConfirmSteer(ctx, result.turnID, delivery.groupIDs); err != nil {
			return err
		}
	}
	if delivery.reply != nil {
		delivery.reply <- result.err
	}
	if s.pendingFinish != nil {
		return s.finishTurn(ctx, s.pendingFinish, false)
	}
	return nil
}

// interruptTurn 在发送取消请求时就开始计时，不依赖 RPC 何时返回。
func (s *externalSessionImpl) interruptTurn(ctx context.Context, result interruptReturned) {
	if s.interruptTimer != nil || s.pendingFinish != nil {
		if result.reply != nil {
			result.reply <- sessionReply[defs.TurnID]{value: result.turnID}
		}
		if result.inputReply != nil {
			result.inputReply <- nil
		}
		return
	}
	s.interruptTimer = time.NewTimer(5 * time.Second)
	driver, driverCtx := s.driver, s.driverCtx
	s.tasks.Go(func() {
		result.err = driver.InterruptTurn(driverCtx)
		s.sendResult(ctx, result)
	})
}

func (s *externalSessionImpl) sendResult(ctx context.Context, result sessionRequest) {
	select {
	case s.requests <- result:
	case <-ctx.Done():
	}
}

func (s *externalSessionImpl) publishExternalState() {
	state := s.externalState
	s.events <- &state
}

func (s *externalSessionImpl) publishPendingInputs() {
	pending := session.SessionPendingInputs{Inputs: s.inbox.GetPendingUsers()}
	if s.interruptTimer == nil && s.pendingFinish == nil {
		pending.SteerTurnID = s.activeTurnID()
	}
	s.events <- &session.SessionPendingInputsUpdated{PendingInputs: pending}
}

func (s *externalSessionImpl) wakeSteerWaiters(items []*defs.Item) {
	for _, reply := range s.steerWaiters {
		reply <- sessionReply[[]*defs.Item]{value: defs.CloneItems(items)}
	}
	s.steerWaiters = nil
}

// handleReplay 在用户消息边界划分历史轮；消息分片先合并，最后一个用户块可紧邻 Ready。
func (s *externalSessionImpl) handleReplay(ctx context.Context, e *agent.AgentEvent) error {
	if e == nil {
		return nil
	}
	switch e.Type {
	case agent.AgentEventItemStarted, agent.AgentEventItemDelta, agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
	default:
		return nil
	}
	if pending := s.replayUser; pending != nil {
		if e.ItemIndex == pending.index && (e.Item == nil || isUserMessage(e.Item) && e.Type != agent.AgentEventItemStarted) {
			switch e.Type {
			case agent.AgentEventItemDelta:
				return applyDraftDelta(pending.item, e.Delta)
			case agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
				if e.Item != nil {
					pending.item = e.Item.Clone()
				}
				return s.flushReplayUser(ctx)
			}
		}
		if err := s.flushReplayUser(ctx); err != nil {
			return err
		}
	}
	if isUserMessage(e.Item) {
		if e.Item.ID != "" && len(s.turnInputIDs) > 0 && s.turnInputIDs[0] == e.Item.ID {
			return nil
		}
		s.replayUser = &draftItem{index: e.ItemIndex, item: e.Item.Clone()}
		if e.Type == agent.AgentEventItemCompleted {
			return s.flushReplayUser(ctx)
		}
		return nil
	}
	if s.turn == nil {
		return nil
	}
	return s.handleOutput(ctx, e, true)
}

func (s *externalSessionImpl) flushReplayUser(ctx context.Context) error {
	if s.replayUser == nil {
		return nil
	}
	item := s.replayUser.item
	s.replayUser = nil
	if s.turn != nil {
		if err := s.finishTurn(ctx, &ExternalTurnFinishedEvent{TurnID: s.turn.ID, Status: defs.TurnStatusCompleted}, true); err != nil {
			return err
		}
	}
	if item.ID == "" {
		item.ID = defs.NewItemID()
	}
	s.turnInputIDs = []defs.ItemID{item.ID}
	return s.startTurn(ctx, defs.NewTurnID(), []*defs.Item{item}, true)
}

// startTurn 建立 running 记录，实时订阅先看到开轮，随后看到这批输入。
func (s *externalSessionImpl) startTurn(ctx context.Context, id defs.TurnID, items []*defs.Item, replaying bool) error {
	turn := &defs.Turn{ID: id, SessionID: s.GetID(), ConversationID: s.record.ConversationID, Status: defs.TurnStatusRunning}
	if model := s.externalState.Options.Model; model != nil {
		turn.Model.Ref = defs.ModelRef{ProviderID: s.record.AgentName, ModelID: model.CurrentValue}
	}
	if err := s.historyMut.SetTurn(ctx, *turn); err != nil {
		return err
	}
	s.turn = turn
	s.modelCallID = defs.NewModelCallID()
	if !replaying {
		s.events <- &session.SessionTurnStarted{TurnID: id, Model: turn.Model}
	}
	return s.appendInputs(ctx, items, replaying)
}

func (s *externalSessionImpl) appendInputs(ctx context.Context, inputs []*defs.Item, replaying bool) error {
	// 驱动可以持有输入直到后端完成本轮，历史归属写在独立副本中。
	items := defs.CloneItems(inputs)
	for _, item := range items {
		item.TurnID, item.ModelCallID = s.turn.ID, ""
	}
	return s.saveItems(ctx, items, replaying)
}

// handleOutput 按首次出现顺序维护草稿；完成快照替换增量，工具调用按 CallID 匹配。
func (s *externalSessionImpl) handleOutput(ctx context.Context, e *agent.AgentEvent, replaying bool) error {
	if e == nil {
		return nil
	}
	switch e.Type {
	case agent.AgentEventItemStarted, agent.AgentEventItemDelta, agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
	case agent.AgentEventToolProgress, agent.AgentEventContextMeasured:
		if !replaying {
			s.events <- &session.SessionAgentEvent{TurnID: s.turn.ID, Event: e}
		}
		return nil
	default:
		return nil
	}
	index := draftIndex(s.draftItems, e)
	var draft *draftItem
	if index >= 0 {
		draft = &s.draftItems[index]
		if draft.complete || e.Type == agent.AgentEventItemStarted {
			return nil
		}
	} else {
		if e.Item == nil || e.Type == agent.AgentEventItemDelta {
			return nil
		}
		index = len(s.draftItems)
		item := e.Item.Clone()
		if item.ID == "" {
			item.ID = defs.NewItemID()
		}
		// 工具后的第一条助手消息另开一组，迟到的工具结果仍沿用对应调用的组。
		if item.Kind == defs.ItemMessage && groupHasTool(s.draftItems, s.modelCallID) {
			s.modelCallID = defs.NewModelCallID()
		}
		item.TurnID, item.ModelCallID = s.turn.ID, s.modelCallID
		if item.ToolResult != nil {
			for _, prior := range s.draftItems {
				if call := prior.item; call != nil && call.ToolCall != nil && call.ToolCall.CallID == item.ToolResult.CallID {
					item.ModelCallID = call.ModelCallID
					break
				}
			}
		}
		s.draftItems = append(s.draftItems, draftItem{index: e.ItemIndex, item: item})
		draft = &s.draftItems[index]
	}
	switch e.Type {
	case agent.AgentEventItemDelta:
		if err := applyDraftDelta(draft.item, e.Delta); err != nil {
			return err
		}
	case agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
		draft.complete = true
		if e.Item != nil {
			item := e.Item.Clone()
			item.ID, item.TurnID, item.ModelCallID = draft.item.ID, s.turn.ID, draft.item.ModelCallID
			draft.item = item
		} else if e.Type == agent.AgentEventItemInterrupted {
			draft.item = nil
		}
	}
	if !replaying {
		event := *e
		event.ItemIndex = index
		if e.Type != agent.AgentEventItemDelta {
			event.Item = draft.item.Clone()
		}
		s.events <- &session.SessionAgentEvent{TurnID: s.turn.ID, Event: &event}
	}
	return s.saveCompletedDrafts(ctx, replaying)
}

// saveCompletedDrafts 只追加已结束的连续前缀，保留交错工具调用首次出现的顺序。
func (s *externalSessionImpl) saveCompletedDrafts(ctx context.Context, replaying bool) error {
	var items []*defs.Item
	for s.savedDraftCount < len(s.draftItems) {
		draft := s.draftItems[s.savedDraftCount]
		if !draft.complete {
			break
		}
		if draft.item != nil {
			items = append(items, draft.item)
		}
		s.savedDraftCount++
	}
	if len(items) == 0 {
		return nil
	}
	return s.saveItems(ctx, items, replaying)
}

func (s *externalSessionImpl) saveItems(ctx context.Context, items []*defs.Item, replaying bool) error {
	saved, err := s.historyMut.AppendItems(ctx, s.turn.ConversationID, s.GetID(), items...)
	if err != nil {
		return err
	}
	if s.turn.ItemsRange.FirstOrdinal == 0 {
		s.turn.ItemsRange.FirstOrdinal = uint64(saved[0].Ordinal)
		if err := s.historyMut.SetTurn(ctx, *s.turn); err != nil {
			return err
		}
	}
	s.lastSavedOrdinal = int64(saved[len(saved)-1].Ordinal)
	if !replaying {
		s.events <- &session.SessionItemsSaved{TurnID: s.turn.ID, Items: saved}
	}
	return nil
}

// finishAfterDelivery 等待已发出的输入 RPC 交回结果，再用最先收到的轮次终态结算。
func (s *externalSessionImpl) finishAfterDelivery(ctx context.Context, end *ExternalTurnFinishedEvent) error {
	if s.delivery == nil {
		return s.finishTurn(ctx, end, false)
	}
	if s.pendingFinish == nil {
		s.pendingFinish = end
	}
	return nil
}

// finishTurn 先保存草稿与终态，再结算收件箱，最后用原生结束事件交付回报。
func (s *externalSessionImpl) finishTurn(ctx context.Context, end *ExternalTurnFinishedEvent, replaying bool) error {
	turn := s.turn
	var outputs []*defs.Item
	var finalID defs.ModelCallID
	for i := range s.draftItems {
		draft := &s.draftItems[i]
		if !draft.complete {
			draft.complete = true
			kind := agent.AgentEventItemCompleted
			if end.Status != defs.TurnStatusCompleted {
				kind = agent.AgentEventItemInterrupted
				switch draft.item.Kind {
				case defs.ItemReasoning:
					draft.item.Reasoning.Status = defs.ReasoningInterrupted
				case defs.ItemToolCall:
					draft.item.ToolCall.Status = defs.ToolCallInterrupted
				case defs.ItemMessage:
					if !hasAssistantText(draft.item) {
						draft.item = nil
					}
				}
			}
			if !replaying {
				s.events <- &session.SessionAgentEvent{TurnID: turn.ID, Event: &agent.AgentEvent{Type: kind, ItemIndex: i, Item: draft.item.Clone()}}
			}
		}
		if item := draft.item; item != nil {
			outputs = append(outputs, item)
			if item.Kind == defs.ItemToolCall {
				finalID = ""
			} else if hasAssistantText(item) {
				finalID = item.ModelCallID
			}
		}
	}
	if err := s.saveCompletedDrafts(ctx, replaying); err != nil {
		return err
	}
	if end.Status == defs.TurnStatusInterrupted {
		item := &defs.Item{ID: defs.NewItemID(), TurnID: turn.ID, Kind: defs.ItemTurnInterrupted}
		if err := s.saveItems(ctx, []*defs.Item{item}, replaying); err != nil {
			return err
		}
		outputs = append(outputs, item)
	}
	turn.Status = end.Status
	turn.ItemsRange.LastOrdinal = uint64(s.lastSavedOrdinal)
	if end.Err != nil {
		turn.Error = end.Err.Error()
	}
	if end.Usage != nil {
		turn.Usage = *end.Usage
	}
	turn.ContextUsage = end.ContextUsage
	if end.Status == defs.TurnStatusCompleted {
		turn.FinalModelCallID = finalID
	}
	if err := s.historyMut.SetTurn(ctx, *turn); err != nil {
		return err
	}
	s.lastTurnID, s.lastTurnStatus = turn.ID, turn.Status
	if !replaying {
		if delivery := s.delivery; delivery != nil {
			if delivery.opening {
				if err := s.inbox.ClearFailedTurnInputs(ctx, turn.ID); err != nil {
					return err
				}
			}
			if delivery.reply != nil {
				delivery.reply <- errors.New("turn ended during input delivery")
			}
		}
		if err := s.inbox.ReturnUnconfirmed(ctx, turn.ID); err != nil {
			return err
		}
		if err := s.inbox.PruneCheckpoints(ctx); err != nil {
			return err
		}
		if s.interruptTimer != nil {
			s.interruptTimer.Stop()
			s.interruptTimer = nil
		}
		s.wakeSteerWaiters(nil)
		s.turn = nil
		s.events <- &session.SessionTurnFinished{
			TurnID: turn.ID, InputItemIDs: s.turnInputIDs, Outputs: outputs, Usage: turn.Usage,
			Status: turn.Status, FinalModelCallID: turn.FinalModelCallID, Err: end.Err,
		}
		s.publishPendingInputs()
		if s.reloadReply != nil && s.driver != nil {
			s.cancelDriver()
		}
	}
	s.turn, s.delivery, s.pendingFinish = nil, nil, nil
	s.draftItems, s.turnInputIDs = nil, nil
	s.savedDraftCount = 0
	s.modelCallID = ""
	return nil
}

// GetID 与原生 GetID 一样返回宿主 Session ID，后端续聊 ID 保存在绑定中。
func (s *externalSessionImpl) GetID() defs.SessionID { return s.record.ID }

// GetHistoryDAO 返回首次完整 Ready 发布的内存库；原生历史可直接从 Conversation DAO 读取。
func (s *externalSessionImpl) GetHistoryDAO() (dao.Query, bool) {
	if s.historyReady.Load() {
		return s.historyQuery, false
	}
	select {
	case <-s.stopped:
		return nil, false
	default:
		return nil, true
	}
}

// HandleInput 与原生一样在入队时确认，驱动发送失败通过该轮的 SessionTurnFinished 报告。
func (s *externalSessionImpl) HandleInput(ctx context.Context, input *session.SessionInput) (defs.ItemID, error) {
	if input == nil {
		return "", errors.New("session input is required")
	}
	copy := *input
	copy.Item = input.Item.Clone()
	reply := make(chan sessionReply[defs.ItemID], 1)
	result, err := sessionRequestReply(ctx, s, inputRequested{copy, reply}, reply)
	if err != nil {
		return "", err
	}
	return result.value, result.err
}

// GetStatus 与原生 GetStatus 一样查询收件箱和活动轮，停止后仍可查询最近的终态。
func (s *externalSessionImpl) GetStatus(ctx context.Context) (session.SessionStatus, error) {
	reply := make(chan sessionReply[session.SessionStatus], 1)
	result, err := sessionRequestReply(ctx, s, statusRequested{reply}, reply)
	if err != nil {
		select {
		case <-s.stopped:
			return session.SessionStatus{State: session.SessionStateStopped, LastTurnID: s.lastTurnID, LastTurnStatus: s.lastTurnStatus}, nil
		default:
			return session.SessionStatus{}, err
		}
	}
	return result.value, result.err
}

// IsTerminalBusy 本会话是否还有未结束的后台命令？与原生一样查询本会话的命令集合。
func (s *externalSessionImpl) IsTerminalBusy() bool {
	return s.terminalManager.IsBusy()
}

// GetTerminalManager 与原生一样将本会话持有的后台命令集合交给协作工具。
func (s *externalSessionImpl) GetTerminalManager() terminal.TerminalManager {
	return s.terminalManager
}

// InterruptTurn 与原生一样返回请求中断的轮 ID；后端五秒内未结束时关闭进程。
func (s *externalSessionImpl) InterruptTurn(ctx context.Context) (defs.TurnID, error) {
	reply := make(chan sessionReply[defs.TurnID], 1)
	result, err := sessionRequestReply(ctx, s, interruptRequested{reply}, reply)
	if err != nil {
		return "", err
	}
	return result.value, result.err
}

// SteerInput 与原生一样按队中 ItemID 选择输入；驱动不支持补充时中断再发。
func (s *externalSessionImpl) SteerInput(ctx context.Context, itemID defs.ItemID, turnID defs.TurnID) error {
	reply := make(chan error, 1)
	result, err := sessionRequestReply(ctx, s, pendingInputRequested{turnID, itemID, false, reply}, reply)
	return errors.Join(result, err)
}

// InterruptTurnWithInput 与原生一样置顶整组输入，等旧轮终结再向驱动开新轮。
func (s *externalSessionImpl) InterruptTurnWithInput(ctx context.Context, turnID defs.TurnID, itemID defs.ItemID) error {
	reply := make(chan error, 1)
	result, err := sessionRequestReply(ctx, s, pendingInputRequested{turnID, itemID, true, reply}, reply)
	return errors.Join(result, err)
}

// WithdrawInput 与原生一样从收件箱撤回完整的待处理用户输入。
func (s *externalSessionImpl) WithdrawInput(ctx context.Context, id defs.ItemID) ([]*defs.Item, error) {
	reply := make(chan sessionReply[[]*defs.Item], 1)
	result, err := sessionRequestReply(ctx, s, withdrawRequested{id, reply}, reply)
	if err != nil {
		return nil, err
	}
	return result.value, result.err
}

// GetUnreadSteer 与原生一样立即读取收件箱副本；外部轮也能看到排在下一轮的 Agent 消息。
func (s *externalSessionImpl) GetUnreadSteer(ctx context.Context) ([]*defs.Item, error) {
	return s.readSteer(ctx, false)
}

// WaitSteer 与原生一样等待活动轮的未读输入或结束；排队的 Agent 回报也会唤醒等待者。
func (s *externalSessionImpl) WaitSteer(ctx context.Context) ([]*defs.Item, error) {
	return s.readSteer(ctx, true)
}

func (s *externalSessionImpl) readSteer(ctx context.Context, wait bool) ([]*defs.Item, error) {
	reply := make(chan sessionReply[[]*defs.Item], 1)
	result, err := sessionRequestReply(ctx, s, unreadRequested{wait, reply}, reply)
	if err != nil {
		select {
		case <-s.stopped:
			return nil, nil
		default:
			return nil, err
		}
	}
	return result.value, result.err
}

// CompactContext 对应原生的手动压缩，外部上下文由后端管理。
func (s *externalSessionImpl) CompactContext(context.Context) (defs.TurnID, error) {
	return "", s.unsupported()
}

// SelectModel 对应原生的模型选择；外部选项统一通过 SetOption 更新。
func (s *externalSessionImpl) SelectModel(context.Context, defs.ModelSelection) error {
	return s.unsupported()
}

// AppendContextItems 与原生一样暂存上下文，下一次开轮时整批交给该轮。
func (s *externalSessionImpl) AppendContextItems(ctx context.Context, items []*defs.Item) error {
	if err := session.ValidateContextItems(items); err != nil {
		return err
	}
	reply := make(chan error, 1)
	result, err := sessionRequestReply(ctx, s, contextItemsRequested{items: defs.CloneItems(items), reply: reply}, reply)
	return errors.Join(result, err)
}

// SetOption 与原生 SelectModel 一样修改后续轮的选择，等待驱动应答期间照常处理选项通知。
func (s *externalSessionImpl) SetOption(ctx context.Context, selection defs.ExternalOptionSelection) error {
	reply := make(chan error, 1)
	result, err := sessionRequestReply(ctx, s, optionRequested{ctx, selection, reply}, reply)
	return errors.Join(result, err)
}

// Reload 在本实例上等旧驱动退出后按最新定义恢复到 Ready；原生无单会话进程可替换。
func (s *externalSessionImpl) Reload(ctx context.Context, launch ExternalSessionLaunch) error {
	reply := make(chan error, 1)
	select {
	case s.requests <- reloadRequested{launch, reply}:
	case <-ctx.Done():
		launch.closeSkillWorkspace(ctx)
		return ctx.Err()
	case <-s.stopped:
		launch.closeSkillWorkspace(ctx)
		return s.stopReason
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopped:
		return s.stopReason
	}
}

func (s *externalSessionImpl) unsupported() error {
	return fmt.Errorf("%w for %s sessions", ErrExternalUnsupported, s.agentType)
}

// sessionRequestReply 将请求交给循环，在调用方取消或 Session 退出时解除等待。
func sessionRequestReply[T any](ctx context.Context, s *externalSessionImpl, req sessionRequest, reply <-chan T) (T, error) {
	var zero T
	select {
	case s.requests <- req:
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-s.stopped:
		return zero, s.stopReason
	}
	select {
	case result := <-reply:
		return result, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-s.stopped:
		return zero, s.stopReason
	}
}
