package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/VaalaCat/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/turn"
)

// Session 接收输入、串行执行 Turn，管理本 Session 的补充输入和后台命令。
type Session interface {
	// GetID 返回 Session ID。
	GetID() defs.SessionID
	// Run 恢复本 Session 未完成的轮次后处理收件箱；停止服务时保留排队输入，等 Turn 和后台命令结束后返回。
	// events 不能为 nil，调用方要一直读到它关闭；events 关闭时，其他方法已经按 Session 已停止处理。
	Run(ctx context.Context, events chan<- SessionEvent) error
	// HandleInput 接收输入并返回 ItemID，同一 InputID 在本次运行内只接收一次；哪个 Turn 处理了它，看 SessionTurnFinished.InputItemIDs。
	// 输入无效、Session 已开始停止、所选模型不在目录里、选中的 Skill 读不出来或 idle_only 遇到活动 Turn 时返回错误，这条输入不排队；
	// 重新选择模型后可以再提交。
	HandleInput(ctx context.Context, input *SessionInput) (defs.ItemID, error)
	// GetStatus 返回执行状态和最近 Turn，停止后返回最终状态；查询可能短暂落后于已发出的 Turn 结束事件。
	GetStatus(ctx context.Context) (SessionStatus, error)
	// IsTerminalBusy 本 Session 是否还有未结束或正在启动的后台命令？只查命令记录，与是否在执行 Turn 无关。
	IsTerminalBusy() bool
	// InterruptTurn 请求取消活动 Turn 并返回它的 ID，不等取消完成；没有活动 Turn 时返回空。
	// 根 Session 被中断后，Agent 消息继续排队但不启动 Turn，收到下一条用户输入后恢复。
	InterruptTurn(ctx context.Context) (defs.TurnID, error)
	// SteerInput 将普通排队用户输入完整移入指定轮的未读补充。
	SteerInput(ctx context.Context, itemID defs.ItemID, turnID defs.TurnID) (err error)
	// InterruptTurnWithInput 将选中用户输入与旧轮未读补充同批置顶，并取消指定轮。
	InterruptTurnWithInput(ctx context.Context, turnID defs.TurnID, itemID defs.ItemID) (err error)
	// WithdrawInput 原子取走待处理的完整用户输入组。
	WithdrawInput(ctx context.Context, itemID defs.ItemID) (items []*defs.Item, err error)
	// GetUnreadSteer 读取未借出的补充输入副本，没有时立即返回空。
	GetUnreadSteer(ctx context.Context) ([]*defs.Item, error)
	// WaitSteer 等活动 Turn 的未读补充输入，返回副本，不取走；已有输入时立即返回，没有活动 Turn、Turn 结束或 Session 已停止时返回空、不报错。
	// 调用方先取消时返回 ctx.Err()。
	WaitSteer(ctx context.Context) ([]*defs.Item, error)
	// CompactContext 空闲时启动只压缩的 Turn，首条 running 写成后返回它的 ID，不等压缩完成；有活动 Turn 或排队的用户输入时返回 ErrSessionBusy，
	// 排队的 Agent 消息不算忙；已开始停止或所选模型不在目录里时返回错误。
	CompactContext(ctx context.Context) (defs.TurnID, error)
	// SelectModel 接收 Conversation 已校验并保存的模型选择，从下个 Turn 起使用，不影响正在执行的 Turn；Session 已停止时返回 nil。
	SelectModel(ctx context.Context, model defs.ModelSelection) error
	// AppendContextItems 校验并存下 Item，下一个不是只压缩的 Turn 开始时交给它，写在该轮输入之前；Session 停止时还没交出的 Item 丢弃。
	// 任一 Item 无效或 Session 已开始停止时返回错误，整批都不收。
	AppendContextItems(ctx context.Context, items []*defs.Item) (err error)
}

// sessionImpl 下面的运行状态只由 Run 的事件循环读写，每个 Turn 在自己的执行任务里运行。
type sessionImpl struct {
	opt SessionOpt
	// toolExecutor 是本 Session 的工具表，由 NewSession 按 Agent 的工具名单和 MCP 选择建出，交给每个 Turn。
	toolExecutor executor.ToolExecutor
	// terminalManager 管理本 Session 的后台命令，命令工具在其中启动、查找；Run 在事件循环返回后停止并等待全部命令。
	terminalManager terminal.TerminalManager
	// turnOpt 是 Run 开始时组装一次的 Turn 配置，带上工具表和模型客户端，Inbox 是本 Session。
	turnOpt turn.TurnOpt

	// requests 收 HandleInput、TakeSteer、WaitSteer、GetStatus、InterruptTurn、CompactContext、SelectModel、AppendContextItems 交来的请求。
	requests chan sessionRequest
	// turnEvents 收 Turn 执行任务结束后交回的结果。
	turnEvents chan turnFinished
	// runStarted 是 Run 只能调用一次的令牌（容量 1）。
	runStarted chan struct{}
	// stopped 在 Run 返回前关闭，之后的请求都返回 errSessionStopped。
	stopped chan struct{}
	// stopReason 是 Session 开始停止的原因（ctx 结束或终态没保存），nil 表示还没开始停止；非 nil 后事件循环拒收输入、
	// 不再开新 Turn，等活动 Turn 结束后把它作为 Run 的返回值。只由事件循环读写。
	stopReason error
	// tasks 是后台命令管理任务和各 Turn 执行任务，Run 返回前等待。
	tasks sync.WaitGroup

	// queuedBatchSpans 保存有追踪信息的排队批次，以组首 ItemID 标识批次；未追踪时无条目。
	queuedBatchSpans map[defs.ItemID]telemetry.SpanRef
	// activeTurnID 是正在执行的 Turn，没有时为空。
	activeTurnID defs.TurnID
	// cancelActiveTurn 取消活动 Turn 的 ctx。
	cancelActiveTurn context.CancelCauseFunc
	// borrowedSteerIDs 标记已借出但尚未确认的补充组；没有借出的组时为空。
	borrowedSteerIDs map[defs.ItemID]struct{}
	// acceptingSteer 活动 Turn 是否还接收补充输入？不接收后 steer 输入改为排队。
	acceptingSteer bool
	// steerWaiters 是 WaitSteer 登记的回复通道（容量 1），在下次挂上补充输入或活动 Turn 结束时统一回复并清空。
	// 调用方 ctx 已结束的等待者也保留到那时；回复只写入容量 1 的通道，不会阻塞事件循环。
	steerWaiters []chan []*defs.Item
	// acceptedInputs 记录本次运行期已接收的 InputID 和对应的 ItemID，只增不删；只由事件循环读写。
	acceptedInputs map[defs.InputID]defs.ItemID
	// checkpoint 是最近一次成功写入的 Session 待办；持久化写入成功后才替换。
	checkpoint defs.SessionCheckpoint

	// lastTurnID 是最近一个 Turn，openLatestTurn 从 Turn 记录读出，每个 Turn 结束时更新。
	lastTurnID defs.TurnID
	// lastTurnStatus 是最近一个 Turn 保存的终态，终态没保存时为空。
	lastTurnStatus defs.TurnStatus
	// lastTurnContextUsage 是最近一次完整普通调用的报告；最近一个 Turn 没有完整普通调用时沿用更早的报告，模型切换后为空；只由事件循环更新。
	lastTurnContextUsage *defs.TurnContextUsage
	// selectedModel 是下一个 Turn 使用的选择：Run 开始时取 opt.Session.Model，SelectModel 时替换；只由事件循环读写。
	selectedModel defs.ModelSelection
	// lastTurnModel 是上一个普通 Turn 使用的模型：openLatestTurn 从最近的 Turn 记录读出，每次启动普通 Turn 时更新；只压缩的 Turn 不更新。
	lastTurnModel defs.ModelRef
	// lastTurnExecutors 是上一个普通 Turn 开始时的 Executor 列表：openLatestTurn 从最近的 Turn 记录读出，每次启动普通 Turn 时更新；只压缩的 Turn 不更新。
	lastTurnExecutors []defs.TurnExecutor
	// pendingHookStartSource 是还没交给 Turn 的 SessionStart 来源：Run 开始时取 opt.HookStartSource，交给第一个非压缩 Turn 后清空；只由事件循环读写。
	pendingHookStartSource string
	// pendingContextItems 是 AppendContextItems 收下、还没交给 Turn 的 Item，按收到的顺序排；
	// 交给下一个不是只压缩的 Turn 后清空；只由事件循环读写。
	pendingContextItems []*defs.Item
}

// SessionOpt 是创建 Session 用的配置：Session 记录、DAO、模型客户端、Agent 定义，以及 Conversation 交来的执行环境、工具、Skill 和 hook。
type SessionOpt struct {
	// Session 是持久化的 Session 记录。
	Session  defs.Session
	Query    dao.Query
	Mutation dao.Mutation
	Client   llmkit.Client
	// Agent 是本 Session 每个 Turn 使用的 Agent 定义。
	Agent *agent.AgentDefinition
	// Models 是只读的模型目录，由 Conversation 建立索引后共享给全部 Session。
	Models map[defs.ModelRef]*defs.ModelDefinition
	// Environments 是本对话的稳定执行环境，供 Session 创建命令工具。
	Environments []executor.ExecutionEnvironment
	// EnvironmentsManager 提供每轮开场的在线环境和默认环境，由 Conversation 持有。
	EnvironmentsManager executor.ExecutionEnvironmentsManager
	// AgentTools 是 Conversation 为本 Session 建的协作工具，在 Agent 侧执行。
	AgentTools []executor.Tool
	// Skills 是本 Agent 可用的 Skill，接收用户输入时按消息里的斜杠指令读取正文。
	Skills []*skill.Skill
	// Hooks 交给每个 Turn 触发 SessionStart、SubagentStart、UserPromptSubmit 与 Stop 等事件；nil 表示没有 hook。
	Hooks hook.Hooks
	// HookStartSource 是本次启动后第一个非压缩 Turn 触发 SessionStart 的来源：startup 或 resume；为空时不触发
	// （重开的子 Session）。子 Session 的 Turn 按它触发 SubagentStart。
	HookStartSource string
}

// NewSessionOpt 用 Session 记录、DAO、模型客户端和 Agent 定义创建 SessionOpt，其余字段由调用方填写。
func NewSessionOpt(
	session defs.Session,
	query dao.Query,
	mutation dao.Mutation,
	client llmkit.Client,
	agentDefinition *agent.AgentDefinition,
) *SessionOpt {
	// 1. 把 Session 记录和依赖填进配置。
	return &SessionOpt{
		Session:  session,
		Query:    query,
		Mutation: mutation,
		Client:   client,
		Agent:    agentDefinition,
	}
}

// NewSession 校验配置，创建本 Session 的后台命令管理和工具表，返回还没运行的 Session。
func NewSession(opt *SessionOpt) (Session, error) {
	// 1. 校验配置，复制 Agent 定义，创建本 Session 的后台命令管理。
	if opt == nil || opt.Session.ID == "" || opt.Query == nil || opt.Mutation == nil || opt.Client == nil ||
		opt.Agent == nil {
		return nil, errors.New("session requires an ID, DAO query, DAO mutation, model client and agent definition")
	}
	if opt.EnvironmentsManager == nil {
		return nil, errors.New("session requires an execution environments manager")
	}
	sessionOpt := *opt
	sessionOpt.Agent = opt.Agent.Clone()
	terminalManager, err := terminal.NewTerminalManager(terminal.NewTerminalManagerOpt())
	if err != nil {
		return nil, err
	}
	// 2. 为每个有工作区、能跑命令的执行环境创建命令工具。
	sessionTools := make(map[string][]executor.Tool, len(sessionOpt.Environments))
	for _, environment := range sessionOpt.Environments {
		if environment.GetWorkspace() == nil || !environment.IsCommandSupported() {
			continue
		}
		commandTools, err := tools.NewCommandTools(tools.NewCommandToolsOpt(environment.GetID(), environment.GetWorkspace(), terminalManager))
		if err != nil {
			return nil, err
		}
		sessionTools[environment.GetID()] = commandTools
	}
	// 3. 按 Agent 的工具名单、MCP 选择和 hook 创建工具表，初始化请求通道和 InputID 去重记录。
	toolExecutorOpt := executor.NewToolExecutorOpt(sessionOpt.EnvironmentsManager, sessionOpt.AgentTools...)
	toolExecutorOpt.SessionTools = sessionTools
	toolExecutorOpt.ToolNames = sessionOpt.Agent.ToolNames
	toolExecutorOpt.MCPServerIDs = sessionOpt.Agent.MCPServerIDs
	toolExecutorOpt.Hooks = sessionOpt.Hooks
	toolExecutorOpt.Session = sessionOpt.Session
	toolExecutor, err := executor.NewToolExecutor(toolExecutorOpt)
	if err != nil {
		return nil, err
	}
	return &sessionImpl{
		opt:              sessionOpt,
		toolExecutor:     toolExecutor,
		terminalManager:  terminalManager,
		requests:         make(chan sessionRequest),
		turnEvents:       make(chan turnFinished),
		runStarted:       make(chan struct{}, 1),
		stopped:          make(chan struct{}),
		acceptedInputs:   make(map[defs.InputID]defs.ItemID),
		queuedBatchSpans: make(map[defs.ItemID]telemetry.SpanRef),
	}, nil
}

// GetID 返回 Session ID。
func (s *sessionImpl) GetID() defs.SessionID {
	// 1. 读取配置中的 Session ID。
	return s.opt.Session.ID
}

// Run 恢复最近的 Turn，启动后台命令管理，运行事件循环直到停止，再等全部任务结束。
func (s *sessionImpl) Run(ctx context.Context, events chan<- SessionEvent) error {
	// 1. 检查 events 和单次启动，登记返回时先关闭 stopped、再关闭 events；调用时 ctx 已取消则直接返回。
	if events == nil {
		return errors.New("session events channel is required")
	}
	defer close(events)
	select {
	case s.runStarted <- struct{}{}:
	default:
		return errors.New("session can only run once")
	}
	defer close(s.stopped)
	if err := ctx.Err(); err != nil {
		return err
	}
	// 2. 补上 Session 身份，组装 Turn 配置，修复最近的 Turn；修复失败时返回错误。
	ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{
		ConversationID:  s.opt.Session.ConversationID,
		SessionID:       s.opt.Session.ID,
		ParentSessionID: s.opt.Session.ParentSessionID,
		AgentName:       s.opt.Session.AgentName,
	})
	logger := telemetry.GetLogger(ctx)
	logger.Info("session started")

	turnOpt := turn.NewTurnOpt(s.opt.Session.ConversationID, s.opt.Session.ID, s.opt.Agent, s.opt.Query, s.opt.Mutation, s.opt.Client)
	turnOpt.ToolExecutor = s.toolExecutor
	turnOpt.Inbox = s
	turnOpt.Hooks = s.opt.Hooks
	turnOpt.Session = s.opt.Session
	turnOpt.MaxSteps = s.opt.Agent.MaxSteps
	s.turnOpt = *turnOpt
	s.selectedModel = s.opt.Session.Model
	s.pendingHookStartSource = s.opt.HookStartSource
	if err := s.loadSessionCheckpoint(ctx); err != nil {
		logSessionStopped(logger, err)
		return err
	}
	if err := s.openLatestTurn(ctx, events); err != nil {
		logSessionStopped(logger, err)
		return err
	}
	// 3. 用不随 ctx 取消的 ctx 启动后台命令管理，运行事件循环直到停止且活动 Turn 结束。
	terminalCtx, stopTerminals := context.WithCancel(context.WithoutCancel(ctx))
	s.tasks.Go(func() { _ = s.terminalManager.Run(terminalCtx) })
	runErr := s.runEventLoop(ctx, events)
	// 4. 停止并等待后台命令和 Turn 任务，记录停止结果。
	stopTerminals()
	s.tasks.Wait()
	logSessionStopped(logger, runErr)
	return runErr
}

// runEventLoop 串行处理收件箱请求和 Turn 结果。
func (s *sessionImpl) runEventLoop(ctx context.Context, events chan<- SessionEvent) error {
	// 1. 串行处理请求、Turn 结束和 ctx 取消；开始停止后等活动 Turn 结束，再返回停止原因。
	done := ctx.Done()
	for s.stopReason == nil || s.activeTurnID != "" {
		select {
		case request := <-s.requests:
			switch request := request.(type) {
			case inputRequested:
				s.handleInputRequested(ctx, events, request)
			case steerTakeRequested:
				s.handleSteerTakeRequested(events, request)
			case confirmInputHandoffRequested:
				s.handleConfirmInputHandoffRequested(ctx, request)
			case steerInputRequested:
				s.handleSteerInputRequested(ctx, events, request)
			case interruptTurnWithInputRequested:
				s.handleInterruptTurnWithInputRequested(ctx, events, request)
			case withdrawInputRequested:
				s.handleWithdrawInputRequested(ctx, events, request)
			case unreadSteerRequested:
				request.reply <- cloneInputGroups(s.getUnreadSteerInputs())
			case steerWaitRequested:
				s.handleSteerWaitRequested(request)
			case statusRequested:
				request.reply <- s.getStatus()
			case interruptRequested:
				turnID := s.activeTurnID
				if turnID == "" {
					request.reply <- interruptAccepted{}
					break
				}
				next := cloneSessionCheckpoint(&s.checkpoint)
				next.StopRequestedTurnID = turnID
				if s.opt.Session.ParentSessionID == "" {
					next.IsAgentMessagesPaused = true
				}
				if err := s.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
					s.stop(fmt.Errorf("save stop request: %w", err))
					request.reply <- interruptAccepted{turnID: turnID, err: fmt.Errorf("save stop request: %w", err)}
					break
				}
				s.interruptActiveTurn()
				request.reply <- interruptAccepted{turnID: turnID}
			case compactRequested:
				s.handleCompactRequested(ctx, events, request)
			case modelSelectionRequested:
				s.selectedModel = request.model
			case appendContextItemsRequested:
				s.handleAppendContextItemsRequested(request)
			default:
				panic(fmt.Sprintf("unsupported Session request %T", request))
			}
		case finished := <-s.turnEvents:
			s.handleTurnFinished(ctx, events, finished)
		case <-done:
			done = nil
			s.stop(ctx.Err())
			s.publishPendingInputs(events)
		}
	}
	return s.stopReason
}

// HandleInput 校验并提交输入，返回实际接收的 ItemID。
func (s *sessionImpl) HandleInput(ctx context.Context, input *SessionInput) (defs.ItemID, error) {
	// 1. 校验消息、还没分配 TurnID 和 ModelCallID、输入模式。
	if input == nil || input.Item == nil || input.Item.ID == "" || input.Item.TurnID != "" || input.Item.ModelCallID != "" {
		return "", errors.New("session input requires an unassigned item")
	}
	if input.Mode != SessionInputModeNormal && input.Mode != SessionInputModeSteer && input.Mode != SessionInputModeIdleOnly &&
		input.Mode != SessionInputModeInterrupt {
		return "", errors.New("invalid session input mode")
	}
	if err := input.Item.ValidateInputMessage(); err != nil {
		return "", err
	}
	// 2. 复制输入并提交请求，等待事件循环回复接收结果。
	reply := make(chan inputAccepted, 1)
	request := inputRequested{
		item:       input.Item.Clone(),
		mode:       input.Mode,
		inputID:    input.InputID,
		parentSpan: telemetry.GetSpanRef(ctx),
		reply:      reply,
	}
	if err := s.sendRequest(ctx, request); err != nil {
		return "", err
	}
	accepted := <-reply
	return accepted.itemID, accepted.err
}

// GetStatus 查询执行状态和最近 Turn。
func (s *sessionImpl) GetStatus(ctx context.Context) (SessionStatus, error) {
	// 1. 向事件循环查询状态；Run 已返回时返回 stopped 和最后记下的 Turn。
	reply := make(chan SessionStatus, 1)
	err := s.sendRequest(ctx, statusRequested{reply: reply})
	if errors.Is(err, errSessionStopped) {
		return SessionStatus{
			State:          SessionStateStopped,
			LastTurnID:     s.lastTurnID,
			LastTurnStatus: s.lastTurnStatus,
		}, nil
	}
	if err != nil {
		return SessionStatus{}, err
	}
	return <-reply, nil
}

// IsTerminalBusy 本 Session 是否还有未结束或正在启动的后台命令？
func (s *sessionImpl) IsTerminalBusy() bool {
	// 1. 询问本 Session 的后台命令管理。
	return s.terminalManager.IsBusy()
}

// InterruptTurn 请求取消活动 Turn，返回它的 TurnID，不等取消完成。
func (s *sessionImpl) InterruptTurn(ctx context.Context) (defs.TurnID, error) {
	// 1. 向事件循环提交取消请求；Session 已停止时返回空 TurnID。
	reply := make(chan interruptAccepted, 1)
	err := s.sendRequest(ctx, interruptRequested{reply: reply})
	if errors.Is(err, errSessionStopped) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	accepted := <-reply
	return accepted.turnID, accepted.err
}

// SteerInput 提交排队输入转补充请求并等待结果。
func (s *sessionImpl) SteerInput(ctx context.Context, itemID defs.ItemID, turnID defs.TurnID) error {
	// 1. 将目标输入和轮交给事件循环。
	reply := make(chan error, 1)
	if err := s.sendRequest(ctx, steerInputRequested{
		itemID: itemID,
		turnID: turnID,
		reply:  reply,
	}); err != nil {
		return err
	}
	return <-reply
}

// InterruptTurnWithInput 提交优先输入和指定轮并等待结果。
func (s *sessionImpl) InterruptTurnWithInput(ctx context.Context, turnID defs.TurnID, itemID defs.ItemID) error {
	// 1. 将目标输入和轮交给事件循环。
	reply := make(chan error, 1)
	if err := s.sendRequest(ctx, interruptTurnWithInputRequested{
		itemID: itemID,
		turnID: turnID,
		reply:  reply,
	}); err != nil {
		return err
	}
	return <-reply
}

// WithdrawInput 请求原子取出完整输入组。
func (s *sessionImpl) WithdrawInput(ctx context.Context, itemID defs.ItemID) ([]*defs.Item, error) {
	// 1. 将目标输入交给事件循环并返回取组结果。
	reply := make(chan inputWithdrawn, 1)
	if err := s.sendRequest(ctx, withdrawInputRequested{
		itemID: itemID,
		reply:  reply,
	}); err != nil {
		return nil, err
	}
	result := <-reply
	return result.items, result.err
}

// CompactContext 请求压缩一次上下文，返回只压缩的 Turn 的 ID，不等压缩完成。
func (s *sessionImpl) CompactContext(ctx context.Context) (defs.TurnID, error) {
	// 1. 提交压缩请求，等事件循环回复是否接收。
	reply := make(chan compactAccepted, 1)
	if err := s.sendRequest(ctx, compactRequested{ctx: ctx, reply: reply}); err != nil {
		return "", err
	}
	select {
	case accepted := <-reply:
		return accepted.turnID, accepted.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// SelectModel 提交下一个 Turn 使用的模型选择。
func (s *sessionImpl) SelectModel(ctx context.Context, model defs.ModelSelection) error {
	// 1. 把 Conversation 已校验并保存的选择交给事件循环；Session 已停止时返回 nil。
	err := s.sendRequest(ctx, modelSelectionRequested{model: model})
	if errors.Is(err, errSessionStopped) {
		return nil
	}
	return err
}

// AppendContextItems 校验 Item 并提交给事件循环，暂存到下一个非压缩 Turn。
func (s *sessionImpl) AppendContextItems(ctx context.Context, items []*defs.Item) (err error) {
	// 1. 校验每个 Item：ID 不为空，还没分配 TurnID 和 ModelCallID，种类和载荷能写入日志并转成模型消息；任一不过整批拒绝。
	for _, item := range items {
		if item == nil || item.ID == "" || item.TurnID != "" || item.ModelCallID != "" {
			return errors.New("context item requires an unassigned item")
		}
		if err := item.ValidateContextItem(); err != nil {
			return err
		}
	}
	// 2. 复制 Item 并提交请求，等待事件循环回复是否收下。
	reply := make(chan error, 1)
	request := appendContextItemsRequested{
		items: make([]*defs.Item, 0, len(items)),
		reply: reply,
	}
	for _, item := range items {
		request.items = append(request.items, item.Clone())
	}
	if err := s.sendRequest(ctx, request); err != nil {
		return err
	}
	return <-reply
}

// GetUnreadSteer 查询活动轮已收到的未读补充，不登记等待者。
func (s *sessionImpl) GetUnreadSteer(ctx context.Context) ([]*defs.Item, error) {
	// 1. 在 Session 循环里读取未借出的组。
	reply := make(chan []*defs.Item, 1)
	if err := s.sendRequest(ctx, unreadSteerRequested{reply: reply}); err != nil {
		if errors.Is(err, errSessionStopped) {
			return nil, nil
		}
		return nil, err
	}
	return <-reply, nil
}

// WaitSteer 等活动 Turn 的未读补充输入，返回副本。
func (s *sessionImpl) WaitSteer(ctx context.Context) ([]*defs.Item, error) {
	// 1. 提交等待请求；Session 已停止时返回空、不报错。
	reply := make(chan []*defs.Item, 1)
	err := s.sendRequest(ctx, steerWaitRequested{reply: reply})
	if errors.Is(err, errSessionStopped) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 2. 等事件循环回复未读补充输入的副本；没有活动 Turn 或 Turn 已结束时回复空，调用方先取消时返回错误。
	select {
	case items := <-reply:
		return items, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TakeSteer 由活动 Turn 调用，取走挂着的全部补充输入；finish 为 true 且没有输入时，之后的 steer 输入改为排队。
func (s *sessionImpl) TakeSteer(ctx context.Context, turnID defs.TurnID, finish bool) ([][]*defs.Item, error) {
	// 1. 把取输入请求交给事件循环，提交前调用方取消时返回错误。
	reply := make(chan steerTaken, 1)
	if err := s.sendRequest(ctx, steerTakeRequested{turnID: turnID, finish: finish, reply: reply}); err != nil {
		return nil, err
	}
	// 2. 请求已提交时等待事件循环回复借出结果。
	result := <-reply
	return result.groups, result.err
}

// ConfirmInputHandoff 在输入组及其 hook 记录写成后清除对应待办。
func (s *sessionImpl) ConfirmInputHandoff(ctx context.Context, turnID defs.TurnID, inputItemIDs []defs.ItemID) error {
	// 1. 将已写成的输入组交给事件循环，提交后等待持久化确认。
	reply := make(chan error, 1)
	if err := s.sendRequest(ctx, confirmInputHandoffRequested{turnID: turnID, inputItemIDs: slices.Clone(inputItemIDs), reply: reply}); err != nil {
		return err
	}
	return <-reply
}

// sendRequest 向事件循环提交请求。
func (s *sessionImpl) sendRequest(ctx context.Context, request sessionRequest) error {
	// 1. 把请求交给事件循环；Run 已返回时返回 errSessionStopped，调用方取消时返回 ctx 的错误。
	select {
	case s.requests <- request:
		return nil
	case <-s.stopped:
		return errSessionStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleAppendContextItemsRequested 把校验过的 Item 追加到待交给 Turn 的队列；已开始停止时拒收。
func (s *sessionImpl) handleAppendContextItemsRequested(request appendContextItemsRequested) {
	// 1. 已开始停止时拒收，否则按收到的顺序追加。
	if s.stopReason != nil {
		request.reply <- fmt.Errorf("session is stopping: %w", s.stopReason)
		return
	}
	s.pendingContextItems = append(s.pendingContextItems, request.items...)
	request.reply <- nil
}

// loadSessionCheckpoint 读取 Session 待办并恢复运行期队列和 InputID 去重。
func (s *sessionImpl) loadSessionCheckpoint(ctx context.Context) error {
	// 1. 读取最新待办；没有记录时从空 checkpoint 开始。
	checkpoint, err := s.opt.Query.GetSessionCheckpoint(ctx, s.opt.Session.ConversationID, s.opt.Session.ID)
	if err != nil {
		return fmt.Errorf("load Session checkpoint: %w", err)
	}
	if checkpoint == nil {
		return nil
	}
	s.checkpoint = *checkpoint
	for _, batches := range [][]defs.SessionInputBatch{checkpoint.PendingUserInputs, checkpoint.PendingAgentMessages} {
		for _, batch := range batches {
			for _, group := range batch {
				if group.InputID != "" && len(group.Items) > 0 {
					s.acceptedInputs[group.InputID] = group.Items[0].ID
				}
			}
		}
	}
	if inputs := checkpoint.TurnInputs; inputs != nil {
		if inputs.OpeningInput != nil && inputs.OpeningInput.InputID != "" {
			s.acceptedInputs[inputs.OpeningInput.InputID] = inputs.OpeningInput.Items[0].ID
		}
		for _, group := range inputs.SteerInputs {
			if group.InputID != "" {
				s.acceptedInputs[group.InputID] = group.Items[0].ID
			}
		}
	}
	return nil
}

// appendSessionCheckpoint 保存下一份待办并在成功后替换内存记录。
func (s *sessionImpl) appendSessionCheckpoint(ctx context.Context, checkpoint *defs.SessionCheckpoint) error {
	// 1. 先追加完整待办，写成功后更新 Session 的 checkpoint。
	if err := s.opt.Mutation.AppendSessionCheckpoint(ctx, s.opt.Session.ConversationID, s.opt.Session.ID, checkpoint); err != nil {
		return err
	}
	s.checkpoint = *checkpoint
	return nil
}

// handleConfirmInputHandoffRequested 清除已完整写入的输入组并持久化新的待办。
func (s *sessionImpl) handleConfirmInputHandoffRequested(ctx context.Context, request confirmInputHandoffRequested) {
	// 1. 在原轮待办中匹配已交接的开场组及已借出的补充组。
	if request.turnID == "" || request.turnID != s.activeTurnID || s.checkpoint.TurnInputs == nil || s.checkpoint.TurnInputs.TurnID != request.turnID || len(request.inputItemIDs) == 0 {
		request.reply <- errors.New("turn input handoff is no longer pending")
		return
	}
	next := cloneSessionCheckpoint(&s.checkpoint)
	inputs := next.TurnInputs
	for _, itemID := range request.inputItemIDs {
		if inputs.OpeningInput != nil && inputs.OpeningInput.Items[0].ID == itemID {
			inputs.OpeningInput = nil
			continue
		}
		index := getInputGroupIndex(inputs.SteerInputs, itemID)
		if _, borrowed := s.borrowedSteerIDs[itemID]; !borrowed || index < 0 {
			request.reply <- fmt.Errorf("input %q is not borrowed by Turn %q", itemID, request.turnID)
			return
		}
		inputs.SteerInputs = slices.Delete(inputs.SteerInputs, index, index+1)
	}
	// 2. 写成待办后清除已确认的借出标记。
	if inputs.OpeningInput == nil && len(inputs.SteerInputs) == 0 {
		next.TurnInputs = nil
	}
	if err := s.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		request.reply <- fmt.Errorf("confirm input handoff: %w", err)
		return
	}
	for _, itemID := range request.inputItemIDs {
		delete(s.borrowedSteerIDs, itemID)
	}
	request.reply <- nil
}

// handleInputRequested 接收一条输入，按模式挂到活动 Turn 上或排队，空闲时开始下一个 Turn。
func (s *sessionImpl) handleInputRequested(ctx context.Context, events chan<- SessionEvent, request inputRequested) {
	// 1. 已开始停止时拒收，重复 InputID 直接返回首次接收的 ItemID。
	if s.stopReason != nil {
		request.reply <- inputAccepted{err: fmt.Errorf("session is stopping: %w", s.stopReason)}
		return
	}
	if request.inputID != "" {
		if itemID, found := s.acceptedInputs[request.inputID]; found {
			request.reply <- inputAccepted{itemID: itemID}
			return
		}
	}
	if err := s.validateSelectedModel(); err != nil {
		request.reply <- inputAccepted{err: err}
		return
	}
	if request.mode == SessionInputModeIdleOnly && s.activeTurnID != "" {
		request.reply <- inputAccepted{err: ErrSessionBusy}
		return
	}
	// 2. 读取点名的 Skill，组成可持久化的输入组；读取失败时不写待办。
	selected, err := getInputSkills(request.item, s.opt.Skills, s.opt.Agent.Name)
	if err != nil {
		request.reply <- inputAccepted{err: err}
		return
	}
	items := []*defs.Item{request.item}
	for _, selectedSkill := range selected {
		text, err := selectedSkill.GetContent(ctx)
		if err != nil {
			request.reply <- inputAccepted{err: err}
			return
		}
		items = append(items, &defs.Item{
			ID:   defs.NewItemID(),
			Kind: defs.ItemSkillInstruction,
			SkillInstruction: &defs.SkillInstruction{
				InputItemID: request.item.ID,
				Name:        selectedSkill.Name,
				Path:        selectedSkill.Path,
				Executor:    selectedSkill.Executor,
				Text:        text,
			},
		})
	}
	group := defs.SessionInputGroup{InputID: request.inputID, Items: items}
	// 3. 先写新待办，再改变内存队列、回复成功和唤醒 Turn。
	next := cloneSessionCheckpoint(&s.checkpoint)
	isUser := isUserInputGroup(group)
	isAgent := request.item.Message.From != nil && request.item.Message.From.Kind == defs.IdentityAgent
	isSteer := request.mode == SessionInputModeSteer && s.acceptingSteer
	isInterrupt := !isAgent && request.mode == SessionInputModeInterrupt
	if isUser {
		next.IsAgentMessagesPaused = false
	}
	switch {
	case isSteer:
		if next.TurnInputs == nil {
			next.TurnInputs = &defs.SessionTurnInputs{TurnID: s.activeTurnID}
		}
		next.TurnInputs.SteerInputs = append(next.TurnInputs.SteerInputs, group)
	case isInterrupt:
		next.PendingUserInputs = append([]defs.SessionInputBatch{{group}}, next.PendingUserInputs...)
		next.StopRequestedTurnID = s.activeTurnID
	case !isAgent:
		next.PendingUserInputs = append(next.PendingUserInputs, defs.SessionInputBatch{group})
	default:
		next.PendingAgentMessages = append(next.PendingAgentMessages, defs.SessionInputBatch{group})
	}
	if err := s.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		request.reply <- inputAccepted{err: fmt.Errorf("save input checkpoint: %w", err)}
		return
	}
	// 4. 登记首次接收的 ID 和批次追踪信息，再回复并安排投递。
	if request.inputID != "" {
		s.acceptedInputs[request.inputID] = request.item.ID
	}
	if !isSteer {
		s.queuedBatchSpans[request.item.ID] = request.parentSpan
	}
	request.reply <- inputAccepted{itemID: request.item.ID}
	telemetry.GetLogger(ctx).Info("input accepted",
		"openvaala.input.item_id", request.item.ID, "openvaala.input.id", request.inputID, "mode", request.mode)
	if isSteer {
		s.wakeSteerWaiters(s.getUnreadSteerInputs())
	} else if isInterrupt {
		s.interruptActiveTurn()
	}
	s.publishPendingInputs(events)
	s.startNextTurn(ctx, events)
}

// handleCompactRequested 检查 Session 状态，空闲时启动只压缩的 Turn。
func (s *sessionImpl) handleCompactRequested(ctx context.Context, events chan<- SessionEvent, request compactRequested) {
	// 1. 已开始停止或所选模型不在目录里时回复错误；有活动 Turn 或排队的用户输入时回复 ErrSessionBusy。
	if s.stopReason != nil {
		request.reply <- compactAccepted{err: fmt.Errorf("session is stopping: %w", s.stopReason)}
		return
	}
	if err := s.validateSelectedModel(); err != nil {
		request.reply <- compactAccepted{err: err}
		return
	}
	if s.activeTurnID != "" || len(s.checkpoint.PendingUserInputs) > 0 {
		request.reply <- compactAccepted{err: ErrSessionBusy}
		return
	}
	// 2. 启动只压缩的 Turn，等首条 running 写完再回复调用方。
	if err := request.ctx.Err(); err != nil {
		request.reply <- compactAccepted{err: err}
		return
	}
	turnOpt := s.turnOpt
	turnOpt.TurnID, turnOpt.IsCompactOnly = defs.NewTurnID(), true
	if err := s.startTurn(ctx, events, turnOpt, telemetry.SpanRef{}); err != nil {
		request.reply <- compactAccepted{err: err}
		return
	}
	request.reply <- compactAccepted{turnID: turnOpt.TurnID}
}

// getInputSkills 解析用户消息里用斜杠指令选中的 Skill。
func getInputSkills(item *defs.Item, skills []*skill.Skill, agentName string) ([]*skill.Skill, error) {
	// 1. 只处理用户提交的消息，其他来源返回空。
	if item.Message == nil || item.Message.From == nil || item.Message.From.Kind != defs.IdentityUser {
		return nil, nil
	}
	// 2. 扫描不是附件（IsContext）的文本里完整的斜杠指令，按出现顺序去重；显式 /skill: 指向不可用的 Skill 时返回错误。
	var selected []*skill.Skill
	seen := make(map[string]bool)
	for _, content := range item.Message.Content {
		if content == nil || content.Kind != defs.ContentText || content.Text == nil || content.Text.IsContext {
			continue
		}
		for _, token := range strings.Fields(content.Text.Text) {
			if !strings.HasPrefix(token, "/") {
				continue
			}
			name := strings.TrimPrefix(token, "/")
			explicit := strings.HasPrefix(name, "skill:")
			name = strings.TrimPrefix(name, "skill:")
			if !skill.IsNameValid(name) {
				continue
			}
			var found *skill.Skill
			for _, candidate := range skills {
				if candidate.Name == name {
					found = candidate
					break
				}
			}
			if found == nil {
				if explicit {
					return nil, fmt.Errorf("skill %q is not available for agent %q", name, agentName)
				}
				continue
			}
			if !seen[name] {
				selected = append(selected, found)
				seen[name] = true
			}
		}
	}
	return selected, nil
}

// handleSteerTakeRequested 交出活动 Turn 挂着的补充输入，按请求停止接收。
func (s *sessionImpl) handleSteerTakeRequested(events chan<- SessionEvent, request steerTakeRequested) {
	// 1. 借出未读补充组的副本，待办归属保持不变。
	if request.turnID == "" || request.turnID != s.activeTurnID {
		request.reply <- steerTaken{err: errors.New("turn is no longer running")}
		return
	}
	var items [][]*defs.Item
	if s.stopReason == nil {
		for _, group := range s.getUnreadSteerInputs() {
			items = append(items, cloneInputItems(group.Items))
			s.borrowedSteerIDs[group.Items[0].ID] = struct{}{}
		}
	}
	// 2. 无补充且 Turn 请求收尾时停止接收。
	if request.finish && len(items) == 0 {
		s.acceptingSteer = false
	}
	s.publishPendingInputs(events)
	request.reply <- steerTaken{groups: items}
}

// handleSteerWaitRequested 有未读补充时回复副本，否则登记等待者。
func (s *sessionImpl) handleSteerWaitRequested(request steerWaitRequested) {
	// 1. 已停止、无活动轮或有未读补充时立即回复。
	groups := s.getUnreadSteerInputs()
	if s.stopReason != nil || s.activeTurnID == "" || len(groups) > 0 {
		request.reply <- cloneInputGroups(groups)
		return
	}
	s.steerWaiters = append(s.steerWaiters, request.reply)
}

// getUnreadSteerInputs 取尚未借出的活动轮补充组。
func (s *sessionImpl) getUnreadSteerInputs() defs.SessionInputBatch {
	// 1. 从已分配待办中过滤借出的组。
	inputs := s.checkpoint.TurnInputs
	if s.stopReason != nil || inputs == nil || inputs.TurnID != s.activeTurnID {
		return nil
	}
	var groups defs.SessionInputBatch
	for _, group := range inputs.SteerInputs {
		if _, borrowed := s.borrowedSteerIDs[group.Items[0].ID]; !borrowed {
			groups = append(groups, group)
		}
	}
	return groups
}

// handleTurnFinished 保存未交接补充的回队结果，再安排下一轮。
func (s *sessionImpl) handleTurnFinished(ctx context.Context, events chan<- SessionEvent, finished turnFinished) {
	// 1. 登记已结束的轮，释放运行状态和等待者。
	if finished.turnID != s.activeTurnID {
		panic(fmt.Sprintf("Turn %q finished while %q is active", finished.turnID, s.activeTurnID))
	}
	s.lastTurnID = finished.turnID
	s.lastTurnStatus = finished.status
	s.lastTurnContextUsage = finished.contextUsage
	s.cancelActiveTurn(nil)
	s.activeTurnID = ""
	s.cancelActiveTurn = nil
	s.borrowedSteerIDs = nil
	s.acceptingSteer = false
	s.wakeSteerWaiters(nil)

	// 2. 终态没保存或 Session 正在停止时不改变持久待办。
	if errors.Is(finished.err, turn.ErrTurnUnsettled) || finished.status == "" && ctx.Err() == nil && s.stopReason == nil {
		s.stop(finished.err)
	}
	if s.stopReason != nil || ctx.Err() != nil {
		s.publishPendingInputs(events)
		return
	}
	// 3. 终态写成后，将未交接的补充按发送方各合一批放回队首，清除本轮归属和停止请求。
	if inputs := s.checkpoint.TurnInputs; inputs != nil && inputs.TurnID == finished.turnID || s.checkpoint.StopRequestedTurnID == finished.turnID {
		if err := s.requeueTurnInputs(ctx, cloneSessionCheckpoint(&s.checkpoint), finished.turnID); err != nil {
			s.stop(err)
			return
		}
	}
	s.publishPendingInputs(events)
	cp := &s.checkpoint
	if !cp.IsAgentMessagesPaused && cp.TurnInputs == nil && cp.StopRequestedTurnID == "" && len(cp.PendingUserInputs) == 0 && len(cp.PendingAgentMessages) == 0 {
		if err := s.opt.Mutation.DeleteSessionCheckpoints(ctx, s.opt.Session.ConversationID, s.opt.Session.ID); err != nil {
			s.stop(err)
		}
		return
	}
	s.startNextTurn(ctx, events)
}

// requeueTurnInputs 将已终结轮的剩余补充按发送方各合一批回队，清理原轮的归属和停止请求。
func (s *sessionImpl) requeueTurnInputs(ctx context.Context, next *defs.SessionCheckpoint, turnID defs.TurnID) error {
	// 1. 只移动该轮尚未交接的补充，不移动开场。
	if inputs := next.TurnInputs; inputs != nil && inputs.TurnID == turnID {
		var users, agents defs.SessionInputBatch
		for _, group := range inputs.SteerInputs {
			if from := group.Items[0].Message.From; from != nil && from.Kind == defs.IdentityAgent {
				agents = append(agents, group)
			} else {
				users = append(users, group)
			}
		}
		if len(users) > 0 {
			next.PendingUserInputs = append([]defs.SessionInputBatch{users}, next.PendingUserInputs...)
		}
		if len(agents) > 0 {
			next.PendingAgentMessages = append([]defs.SessionInputBatch{agents}, next.PendingAgentMessages...)
		}
		next.TurnInputs = nil
	}
	// 2. 一行写成新的归属及停止请求，保留根 Session 暂停状态。
	if next.StopRequestedTurnID == turnID {
		next.StopRequestedTurnID = ""
	}
	if err := s.appendSessionCheckpoint(ctx, next); err != nil {
		return fmt.Errorf("clear completed Turn inputs: %w", err)
	}
	return nil
}

// startNextTurn 从队列分配输入组并启动普通 Turn。
func (s *sessionImpl) startNextTurn(ctx context.Context, events chan<- SessionEvent) {
	// 1. 空闲时先选首批用户输入，其次选未暂停的所有 Agent 消息。
	if s.stopReason != nil || ctx.Err() != nil || s.activeTurnID != "" || s.checkpoint.TurnInputs != nil {
		return
	}
	var batches []defs.SessionInputBatch
	next := cloneSessionCheckpoint(&s.checkpoint)
	switch {
	case len(next.PendingUserInputs) > 0:
		batches = next.PendingUserInputs[:1]
		next.PendingUserInputs = next.PendingUserInputs[1:]
	case len(next.PendingAgentMessages) > 0 && !next.IsAgentMessagesPaused:
		batches = next.PendingAgentMessages
		next.PendingAgentMessages = nil
	default:
		return
	}
	// 2. 用一条记录把选中的组移到新轮，成功后启动 Turn。
	groups := slices.Concat(batches...)
	turnID := defs.NewTurnID()
	next.TurnInputs = &defs.SessionTurnInputs{TurnID: turnID, OpeningInput: &groups[0], SteerInputs: groups[1:]}
	if err := s.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		s.stop(fmt.Errorf("allocate Turn %q inputs: %w", turnID, err))
		return
	}
	parentSpan := s.queuedBatchSpans[groups[0].Items[0].ID]
	for _, batch := range batches {
		delete(s.queuedBatchSpans, batch[0].Items[0].ID)
	}
	turnOpt := s.turnOpt
	turnOpt.TurnID = turnID
	if err := s.startTurn(ctx, events, turnOpt, parentSpan); err != nil {
		s.stop(err)
	}
}

// startTurn 装配并启动 Turn，登记活动轮并转发该轮事件。
func (s *sessionImpl) startTurn(ctx context.Context, events chan<- SessionEvent, turnOpt turn.TurnOpt, parentSpan telemetry.SpanRef) error {
	// 1. 恢复已有记录时只查原模型；尚未写 running 时才按 Session 的选择开轮。
	turnID := turnOpt.TurnID
	var turnResume turn.TurnResume
	if turnOpt.Resume != nil {
		turnResume = *turnOpt.Resume
	}
	var turnSelection defs.ModelSelection
	if turnResume.Record != nil {
		turnSelection = turnResume.Record.Model
		turnOpt.Model = s.opt.Models[turnSelection.Ref]
	} else {
		var found bool
		turnOpt.Model, turnSelection.ReasoningEffort, found = defs.GetSelectedModel(s.opt.Models, s.selectedModel, s.opt.Agent.Model)
		if !found {
			if !turnResume.StopRequested {
				return s.validateSelectedModel()
			}
			turnSelection = s.selectedModel
			if turnSelection.Ref == (defs.ModelRef{}) {
				turnSelection.Ref = s.opt.Agent.Model
			}
			turnOpt.Model = &defs.ModelDefinition{Ref: turnSelection.Ref}
		}
		turnSelection.Ref = turnOpt.Model.Ref
	}
	turnOpt.ReasoningEffort = turnSelection.ReasoningEffort
	if turnOpt.Model != nil {
		turnOpt.CompactModel = defs.GetCompactModel(s.opt.Models, s.opt.Agent.CompactModel, turnOpt.Model)
	} else {
		turnOpt.CompactModel = s.opt.Models[s.opt.Agent.CompactModel]
	}
	turnOpt.PreviousModel, turnOpt.PreviousContextUsage, turnOpt.PreviousExecutors = s.lastTurnModel, s.lastTurnContextUsage, s.lastTurnExecutors
	turnOpt.Environments, turnOpt.DefaultExecutorID = s.opt.EnvironmentsManager.GetEnvironments()
	// 2. 普通轮只接收自己的开场；恢复已写 running 的旧轮不再注入启动 hook。
	var turnInput []*defs.Item
	if !turnOpt.IsCompactOnly {
		if inputs := s.checkpoint.TurnInputs; inputs != nil && inputs.TurnID == turnID && inputs.OpeningInput != nil {
			turnInput = inputs.OpeningInput.Items
		}
		s.lastTurnModel = turnSelection.Ref
		if turnResume.Record == nil {
			s.lastTurnExecutors = executor.GetTurnExecutors(turnOpt.Environments)
			turnOpt.HookStartSource = s.pendingHookStartSource
			turnOpt.ContextItems, s.pendingContextItems = s.pendingContextItems, nil
		}
		s.pendingHookStartSource = ""
	}
	// 3. 启动 Turn；新手动轮首写失败时等 Turn 返回并停止 Session。
	var runningReply chan error
	if turnOpt.IsCompactOnly && turnResume.Record == nil {
		runningReply = make(chan error, 1)
		turnOpt.RunningReply = runningReply
	}
	turnInstance, err := turn.NewTurn(&turnOpt)
	if err != nil {
		return err
	}
	turnCtx, cancel := context.WithCancelCause(telemetry.ContextWithParentSpan(ctx, parentSpan))
	turnEvents := make(chan turn.TurnEvent)
	var result turn.TurnResult
	var runErr error
	var turnRun sync.WaitGroup
	turnRun.Go(func() {
		result, runErr = turnInstance.Run(turnCtx, turnInput, turnEvents)
		close(turnEvents)
	})
	if runningReply != nil {
		if err := <-runningReply; err != nil {
			turnRun.Wait()
			cancel(nil)
			s.stop(runErr)
			return err
		}
	}

	// 4. 登记活动轮，更新已分配或恢复轮的待办列表。
	s.activeTurnID = turnID
	s.cancelActiveTurn = cancel
	s.acceptingSteer = !turnOpt.IsCompactOnly && !turnResume.StopRequested
	if !turnOpt.IsCompactOnly {
		s.borrowedSteerIDs = make(map[defs.ItemID]struct{})
	}
	if runningReply == nil {
		s.publishPendingInputs(events)
	}

	// 5. 先发布启动，再转发 Turn 事件和结束结果。
	s.tasks.Go(func() {
		events <- &SessionTurnStarted{TurnID: turnID, Model: turnSelection}
		for event := range turnEvents {
			events <- newSessionEvent(turnID, event)
		}
		turnRun.Wait()
		if result.Status != "" {
			events <- &SessionTurnFinished{
				TurnID:           turnID,
				InputItemIDs:     result.InputItemIDs,
				Status:           result.Status,
				Outputs:          result.Outputs,
				FinalModelCallID: result.FinalModelCallID,
				Err:              runErr,
			}
		}
		s.turnEvents <- turnFinished{
			turnID:       turnID,
			status:       result.Status,
			contextUsage: result.ContextUsage,
			err:          runErr,
		}
	})
	return nil
}

// validateSelectedModel 检查 Session 所选模型在目录里，不在时返回让用户重新选择的错误。
func (s *sessionImpl) validateSelectedModel() error {
	// 1. 查询所选模型，没选过时用 Agent 默认模型；找不到时按有没有选过返回不同的错误。
	if _, _, found := defs.GetSelectedModel(s.opt.Models, s.selectedModel, s.opt.Agent.Model); found {
		return nil
	}
	if s.selectedModel.Ref == (defs.ModelRef{}) {
		return errors.New("no model is configured; choose a model")
	}
	return fmt.Errorf("model %s is not configured; choose another model", s.selectedModel.Ref)
}

// stop 记下服务或持久化错误，停止调度并取消活动 Turn。
func (s *sessionImpl) stop(err error) {
	// 1. 合并停止原因，保留已持久化的输入归属并取消还在执行的 Turn。
	s.stopReason = errors.Join(s.stopReason, err)
	s.acceptingSteer = false
	if s.cancelActiveTurn != nil {
		s.cancelActiveTurn(err)
	}
}

// wakeSteerWaiters 回复全部 WaitSteer 等待者并清空登记。
func (s *sessionImpl) wakeSteerWaiters(items defs.SessionInputBatch) {
	// 1. 给每个等待者回复一份未读输入的副本，再清空登记；items 为 nil 表示 Turn 已结束。
	for _, waiter := range s.steerWaiters {
		waiter <- cloneInputGroups(items)
	}
	s.steerWaiters = nil
}

// getStatus 在事件循环里汇总当前状态、排队输入数和最近 Turn。
func (s *sessionImpl) getStatus() SessionStatus {
	// 1. 填写活动 Turn、排队输入和上下文数量、最近 Turn；已开始停止时为 stopping，否则有活动 Turn 时为 running，都没有时为 idle。
	status := SessionStatus{
		State:                   SessionStateIdle,
		ActiveTurnID:            s.activeTurnID,
		PendingInputCount:       countInboxGroups(s.checkpoint.PendingUserInputs) + countInboxGroups(s.checkpoint.PendingAgentMessages),
		PendingContextItemCount: len(s.pendingContextItems),
		LastTurnID:              s.lastTurnID,
		LastTurnStatus:          s.lastTurnStatus,
	}
	if s.activeTurnID != "" {
		status.State = SessionStateRunning
	}
	if s.stopReason != nil {
		status.State = SessionStateStopping
	}
	return status
}

// interruptActiveTurn 以用户停止原因取消活动轮并停止接收补充。
func (s *sessionImpl) interruptActiveTurn() {
	// 1. 只取消已登记的目标轮。
	if s.cancelActiveTurn != nil {
		s.acceptingSteer = false
		s.cancelActiveTurn(turn.ErrTurnInterrupted)
	}
}

// openLatestTurn 核对本 Session 的 metadata、输入归属和进度，优先启动原轮。
func (s *sessionImpl) openLatestTurn(ctx context.Context, events chan<- SessionEvent) error {
	// 1. 以 metadata 日志中的最近 Turn 为准，修复索引，恢复上轮状态。
	conversationID, sessionID := s.opt.Session.ConversationID, s.opt.Session.ID
	latest, err := s.opt.Query.GetLatestLoggedTurn(ctx, conversationID, sessionID)
	if err != nil {
		return fmt.Errorf("read latest Session Turn: %w", err)
	}
	next := cloneSessionCheckpoint(&s.checkpoint)
	turnOpt := s.turnOpt
	if latest != nil {
		if err := s.opt.Mutation.ReindexLoggedTurn(ctx, *latest); err != nil {
			return err
		}
		s.lastTurnID = latest.ID
		s.lastTurnContextUsage = latest.ContextUsage
		s.lastTurnModel = latest.Model.Ref
		s.lastTurnExecutors = latest.Executors
		if latest.Status != defs.TurnStatusRunning {
			s.lastTurnStatus = latest.Status
		}
		// 2. 只读未完成轮，或已终结但仍留有该轮待办的日志尾部。
		inputs := next.TurnInputs
		if latest.Status == defs.TurnStatusRunning || inputs != nil && inputs.TurnID == latest.ID {
			rows, err := turn.GetLoggedTurnItems(ctx, s.opt.Query, *latest)
			if err != nil {
				return err
			}
			if inputs != nil && inputs.TurnID == latest.ID {
				next.TurnInputs, err = getUnhandedTurnInputs(inputs, rows)
				if err != nil {
					return err
				}
				if latest.Status == defs.TurnStatusRunning && next.TurnInputs != inputs {
					if err := s.appendSessionCheckpoint(ctx, next); err != nil {
						return err
					}
				}
			}
			if latest.Status == defs.TurnStatusRunning {
				if inputs != nil && inputs.TurnID != latest.ID {
					return fmt.Errorf("running Turn %q conflicts with allocated Turn %q", latest.ID, inputs.TurnID)
				}
				turnOpt.TurnID, turnOpt.IsCompactOnly = latest.ID, latest.IsCompactOnly
				checkpoint, err := s.opt.Query.GetTurnCheckpoint(ctx, conversationID, sessionID, latest.ID)
				if err != nil {
					return err
				}
				turnOpt.Resume = &turn.TurnResume{
					Record:     latest,
					Checkpoint: checkpoint,
					Items:      rows,
				}
				if checkpoint != nil && checkpoint.Request != nil && checkpoint.Request.Compaction != nil {
					turnOpt.Resume.CompactModel = s.opt.Models[checkpoint.Request.Compaction.CompactModelRef]
				}
			}
		}
		if latest.Status != defs.TurnStatusRunning && (inputs != nil && inputs.TurnID == latest.ID || next.StopRequestedTurnID == latest.ID) {
			if err := s.requeueTurnInputs(ctx, next, latest.ID); err != nil {
				return err
			}
		}
	}
	// 3. 已分配但未写 running 时沿用原 ID；没有原轮时按原队列规则开轮。
	if turnOpt.TurnID == "" && next.TurnInputs != nil {
		turnOpt.TurnID = next.TurnInputs.TurnID
		checkpoint, err := s.opt.Query.GetTurnCheckpoint(ctx, conversationID, sessionID, turnOpt.TurnID)
		if err != nil {
			return err
		}
		if checkpoint != nil {
			return fmt.Errorf("allocated Turn %q has progress without a running record", turnOpt.TurnID)
		}
		if next.TurnInputs.OpeningInput == nil && next.StopRequestedTurnID != turnOpt.TurnID {
			return fmt.Errorf("allocated Turn %q has no opening input", turnOpt.TurnID)
		}
	}
	if next.StopRequestedTurnID != "" && next.StopRequestedTurnID != turnOpt.TurnID {
		return fmt.Errorf("stop request for Turn %q has no matching allocation or running record", next.StopRequestedTurnID)
	}
	if turnOpt.TurnID == "" {
		s.startNextTurn(ctx, events)
		return s.stopReason
	}
	if next.StopRequestedTurnID == turnOpt.TurnID {
		if turnOpt.Resume == nil {
			turnOpt.Resume = &turn.TurnResume{}
		}
		turnOpt.Resume.StopRequested = true
	}
	return s.startTurn(ctx, events, turnOpt, telemetry.SpanRef{})
}

// handleSteerInputRequested 将排队用户输入移到指定轮的补充尾部。
func (s *sessionImpl) handleSteerInputRequested(ctx context.Context, events chan<- SessionEvent, request steerInputRequested) {
	// 1. 检查目标轮并在新待办中把普通队列组移入补充队列。
	if request.turnID == "" || request.turnID != s.activeTurnID || !s.acceptingSteer {
		request.reply <- errors.New("turn is no longer accepting input")
		return
	}
	next := cloneSessionCheckpoint(&s.checkpoint)
	group, oldBatchID, newBatchID := takePendingUserGroup(next, request.itemID)
	if group.Items == nil {
		request.reply <- errors.New("input is no longer pending")
		return
	}
	if next.TurnInputs == nil {
		next.TurnInputs = &defs.SessionTurnInputs{TurnID: request.turnID}
	}
	next.TurnInputs.SteerInputs = append(next.TurnInputs.SteerInputs, group)
	// 2. 记录写成后更新批次追踪信息并通知等待者。
	if err := s.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		request.reply <- fmt.Errorf("save steered input: %w", err)
		return
	}
	s.moveQueuedBatchSpan(oldBatchID, newBatchID)
	s.wakeSteerWaiters(s.getUnreadSteerInputs())
	s.publishPendingInputs(events)
	request.reply <- nil
}

// handleInterruptTurnWithInputRequested 将选中输入与旧未读补充同批置顶并取消旧轮。
func (s *sessionImpl) handleInterruptTurnWithInputRequested(ctx context.Context, events chan<- SessionEvent, request interruptTurnWithInputRequested) {
	// 1. 检查指定轮次，在新待办中将选中输入及未借出的补充组同批置顶。
	if request.turnID == "" || request.turnID != s.activeTurnID || s.stopReason != nil {
		request.reply <- errors.New("turn is no longer running")
		return
	}
	next := cloneSessionCheckpoint(&s.checkpoint)
	group, oldBatchID, newBatchID := takePendingUserGroup(next, request.itemID)
	if group.Items == nil {
		request.reply <- errors.New("input is no longer pending")
		return
	}
	batch := defs.SessionInputBatch{group}
	if inputs := next.TurnInputs; inputs != nil {
		var borrowed defs.SessionInputBatch
		for _, steer := range inputs.SteerInputs {
			if _, found := s.borrowedSteerIDs[steer.Items[0].ID]; found {
				borrowed = append(borrowed, steer)
			} else {
				batch = append(batch, steer)
			}
		}
		inputs.SteerInputs = borrowed
		if inputs.OpeningInput == nil && len(borrowed) == 0 {
			next.TurnInputs = nil
		}
	}
	next.PendingUserInputs = append([]defs.SessionInputBatch{batch}, next.PendingUserInputs...)
	next.StopRequestedTurnID = request.turnID
	// 2. 保存迁移和停止请求，成功后取消旧轮。
	if err := s.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		request.reply <- fmt.Errorf("save interrupted input: %w", err)
		return
	}
	parentSpan := s.queuedBatchSpans[oldBatchID]
	s.moveQueuedBatchSpan(oldBatchID, newBatchID)
	s.queuedBatchSpans[request.itemID] = parentSpan
	s.interruptActiveTurn()
	s.publishPendingInputs(events)
	request.reply <- nil
}

// handleWithdrawInputRequested 原子取走普通排队或未读补充中的用户输入组。
func (s *sessionImpl) handleWithdrawInputRequested(ctx context.Context, events chan<- SessionEvent, request withdrawInputRequested) {
	// 1. 从新待办移除普通队列或未借出的补充组。
	if s.stopReason != nil {
		request.reply <- inputWithdrawn{err: fmt.Errorf("session is stopping: %w", s.stopReason)}
		return
	}
	next := cloneSessionCheckpoint(&s.checkpoint)
	group, oldBatchID, newBatchID := takePendingUserGroup(next, request.itemID)
	if inputs := next.TurnInputs; group.Items == nil && inputs != nil && inputs.TurnID == s.activeTurnID {
		index := getInputGroupIndex(inputs.SteerInputs, request.itemID)
		if _, borrowed := s.borrowedSteerIDs[request.itemID]; !borrowed && index >= 0 && isUserInputGroup(inputs.SteerInputs[index]) {
			group = inputs.SteerInputs[index]
			inputs.SteerInputs = slices.Delete(inputs.SteerInputs, index, index+1)
			if inputs.OpeningInput == nil && len(inputs.SteerInputs) == 0 {
				next.TurnInputs = nil
			}
		}
	}
	if group.Items == nil {
		request.reply <- inputWithdrawn{err: errors.New("input is no longer pending")}
		return
	}
	// 2. 持久移除后发布待办并交还整组 Item。
	if err := s.appendSessionCheckpoint(context.WithoutCancel(ctx), next); err != nil {
		request.reply <- inputWithdrawn{err: fmt.Errorf("save withdrawn input: %w", err)}
		return
	}
	s.moveQueuedBatchSpan(oldBatchID, newBatchID)
	s.publishPendingInputs(events)
	request.reply <- inputWithdrawn{items: group.Items}
}

// moveQueuedBatchSpan 将排队批次的追踪信息转到新组首，批次已空时清除。
func (s *sessionImpl) moveQueuedBatchSpan(oldBatchID, newBatchID defs.ItemID) {
	// 1. 只更新组首有变化的批次。
	if oldBatchID == newBatchID {
		return
	}
	if span, found := s.queuedBatchSpans[oldBatchID]; found {
		delete(s.queuedBatchSpans, oldBatchID)
		if newBatchID != "" {
			s.queuedBatchSpans[newBatchID] = span
		}
	}
}

// publishPendingInputs 发布未读补充及普通队列中的轻量用户列表。
func (s *sessionImpl) publishPendingInputs(events chan<- SessionEvent) {
	// 1. 创建独立轻量切片，先列未读用户补充，再按排队顺序列用户输入。
	pendingInputs := SessionPendingInputs{Inputs: make([]PendingInput, 0)}
	if s.acceptingSteer {
		pendingInputs.SteerTurnID = s.activeTurnID
	}
	for _, group := range s.getUnreadSteerInputs() {
		if isUserInputGroup(group) {
			pendingInputs.Inputs = append(pendingInputs.Inputs, newPendingInput(group.Items[0], true))
		}
	}
	if s.stopReason == nil {
		for _, batch := range s.checkpoint.PendingUserInputs {
			for _, group := range batch {
				if isUserInputGroup(group) {
					pendingInputs.Inputs = append(pendingInputs.Inputs, newPendingInput(group.Items[0], false))
				}
			}
		}
	}
	// 2. 发送新列表。
	events <- &SessionPendingInputsUpdated{PendingInputs: pendingInputs}
}
