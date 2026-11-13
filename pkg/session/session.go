package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/turn"
)

// Session 接收输入、串行执行 Turn，管理本 Session 的补充输入和后台命令。
type Session interface {
	// GetID 返回 Session ID。
	GetID() defs.SessionID
	// Run 修复最近的 Turn 后串行处理收件箱；停止时取消活动 Turn、丢弃排队输入，等 Turn 和后台命令结束后返回。
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
	// WaitSteer 等活动 Turn 的未读补充输入，返回副本，不取走；已有输入时立即返回，没有活动 Turn、Turn 结束或 Session 已停止时返回空、不报错。
	// 调用方先取消时返回 ctx.Err()。
	WaitSteer(ctx context.Context) ([]*defs.Item, error)
	// CompactContext 空闲时启动只压缩的 Turn 并返回它的 ID，不等压缩完成；有活动 Turn 或排队的用户输入时返回 ErrSessionBusy，
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
	// turnOpt 是 Run 开始时组装一次的 Turn 配置，带上工具表、执行环境、模型客户端，Inbox 是本 Session。
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

	// pendingUserInputs 是等待开新 Turn 的用户输入，先于 Agent 消息开始；插队的放在最前。
	pendingUserInputs []inboxInput
	// pendingAgentMessages 是等待开新 Turn 的 Agent 消息（send_message、子任务结果通知），用户队列空了才开始。
	pendingAgentMessages []inboxInput
	// isAgentMessagesPaused Agent 消息是否暂停开新 Turn？用户中断主 Agent 的 Turn 后暂停，Agent 消息照常排队，收到下一条用户输入时恢复。
	isAgentMessagesPaused bool
	// activeTurnID 是正在执行的 Turn，没有时为空。
	activeTurnID defs.TurnID
	// cancelActiveTurn 取消活动 Turn 的 ctx。
	cancelActiveTurn context.CancelFunc
	// activeSteer 是挂在活动 Turn 上、还没被取走的补充输入组。
	activeSteer [][]*defs.Item
	// acceptingSteer 活动 Turn 是否还接收补充输入？不接收后 steer 输入改为排队。
	acceptingSteer bool
	// steerWaiters 是 WaitSteer 登记的回复通道（容量 1），在下次挂上补充输入或活动 Turn 结束时统一回复并清空。
	// 调用方 ctx 已结束的等待者也保留到那时；回复只写入容量 1 的通道，不会阻塞事件循环。
	steerWaiters []chan []*defs.Item
	// acceptedInputs 记录本次运行期已接收的 InputID 和对应的 ItemID，只增不删；只由事件循环读写。
	acceptedInputs map[defs.InputID]defs.ItemID

	// lastTurnID 是最近一个 Turn，repairLatestTurn 从 Turn 记录读出，每个 Turn 结束时更新。
	lastTurnID defs.TurnID
	// lastTurnStatus 是最近一个 Turn 保存的终态，终态没保存时为空。
	lastTurnStatus defs.TurnStatus
	// lastTurnContextUsage 是最近一次完整普通调用的报告；最近一个 Turn 没有完整普通调用时沿用更早的报告，模型切换后为空；只由事件循环更新。
	lastTurnContextUsage *defs.TurnContextUsage
	// selectedModel 是下一个 Turn 使用的选择：Run 开始时取 opt.Session.Model，SelectModel 时替换；只由事件循环读写。
	selectedModel defs.ModelSelection
	// lastTurnModel 是上一个普通 Turn 使用的模型：repairLatestTurn 从最近的 Turn 记录读出，每次启动普通 Turn 时更新；只压缩的 Turn 不更新。
	lastTurnModel defs.ModelRef
	// lastTurnExecutors 是上一个普通 Turn 开始时的 Executor 列表：repairLatestTurn 从最近的 Turn 记录读出，每次启动普通 Turn 时更新；只压缩的 Turn 不更新。
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
	// Environments 是本对话的执行环境实例，第一个为默认；由 Conversation 持有，Session 只借用。
	Environments []executor.ExecutionEnvironment
	// AgentTools 是 Conversation 为本 Session 建的协作工具，在 Agent 侧执行。
	AgentTools []executor.Tool
	// ClientMCPServers 提供 Conversation 持有的客户端 MCP 工具，nil 表示没有。
	ClientMCPServers executor.ClientMCPServers
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
	toolExecutorOpt := executor.NewToolExecutorOpt(sessionOpt.Environments, sessionOpt.AgentTools...)
	toolExecutorOpt.SessionTools = sessionTools
	toolExecutorOpt.ClientMCPServers = sessionOpt.ClientMCPServers
	toolExecutorOpt.ToolNames = sessionOpt.Agent.ToolNames
	toolExecutorOpt.MCPServerIDs = sessionOpt.Agent.MCPServerIDs
	toolExecutorOpt.Hooks = sessionOpt.Hooks
	toolExecutorOpt.Session = sessionOpt.Session
	toolExecutor, err := executor.NewToolExecutor(toolExecutorOpt)
	if err != nil {
		return nil, err
	}
	return &sessionImpl{
		opt:             sessionOpt,
		toolExecutor:    toolExecutor,
		terminalManager: terminalManager,
		requests:        make(chan sessionRequest),
		turnEvents:      make(chan turnFinished),
		runStarted:      make(chan struct{}, 1),
		stopped:         make(chan struct{}),
		acceptedInputs:  make(map[defs.InputID]defs.ItemID),
	}, nil
}

// GetID 返回 Session ID。
func (s *sessionImpl) GetID() defs.SessionID {
	// 1. 读取配置中的 Session ID。
	return s.opt.Session.ID
}

// Run 修复最近的 Turn，启动后台命令管理，运行事件循环直到停止，再等全部任务结束。
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
	turnOpt.Environments = s.opt.Environments
	turnOpt.Inbox = s
	turnOpt.Hooks = s.opt.Hooks
	turnOpt.Session = s.opt.Session
	turnOpt.MaxSteps = s.opt.Agent.MaxSteps
	s.turnOpt = *turnOpt
	s.selectedModel = s.opt.Session.Model
	s.pendingHookStartSource = s.opt.HookStartSource
	if err := s.repairLatestTurn(ctx); err != nil {
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
			case steerInputRequested:
				s.handleSteerInputRequested(events, request)
			case interruptTurnWithInputRequested:
				s.handleInterruptTurnWithInputRequested(events, request)
			case withdrawInputRequested:
				s.handleWithdrawInputRequested(events, request)
			case steerWaitRequested:
				s.handleSteerWaitRequested(request)
			case statusRequested:
				request.reply <- s.getStatus()
			case interruptRequested:
				turnID := s.interruptActiveTurn()
				// 根 Session 的活动 Turn 被中断后暂停 Agent 消息开新 Turn，消息照常排队。
				if turnID != "" && s.opt.Session.ParentSessionID == "" {
					s.isAgentMessagesPaused = true
				}
				request.reply <- turnID
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
	reply := make(chan defs.TurnID, 1)
	err := s.sendRequest(ctx, interruptRequested{reply: reply})
	if errors.Is(err, errSessionStopped) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return <-reply, nil
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
	if err := s.sendRequest(ctx, compactRequested{reply: reply}); err != nil {
		return "", err
	}
	accepted := <-reply
	return accepted.turnID, accepted.err
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
	reply := make(chan [][]*defs.Item, 1)
	select {
	case s.requests <- steerTakeRequested{
		turnID: turnID,
		finish: finish,
		reply:  reply,
	}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 2. 等事件循环交出补充输入；请求提交后不再因调用方取消放弃回复。
	return <-reply, nil
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

// handleInputRequested 接收一条输入，按模式挂到活动 Turn 上或排队，空闲时开始下一个 Turn。
func (s *sessionImpl) handleInputRequested(ctx context.Context, events chan<- SessionEvent, request inputRequested) {
	// 1. 已开始停止或所选模型不在目录里时拒收，重复 InputID 返回首次接收的 ItemID，有活动 Turn 时拒收 idle_only。
	if s.stopReason != nil {
		request.reply <- inputAccepted{err: fmt.Errorf("session is stopping: %w", s.stopReason)}
		return
	}
	if err := s.validateSelectedModel(); err != nil {
		request.reply <- inputAccepted{err: err}
		return
	}
	if request.inputID != "" {
		if itemID, found := s.acceptedInputs[request.inputID]; found {
			request.reply <- inputAccepted{itemID: itemID}
			return
		}
	}
	if request.mode == SessionInputModeIdleOnly && s.activeTurnID != "" {
		request.reply <- inputAccepted{err: ErrSessionBusy}
		return
	}
	// 2. 解析用户选中的 Skill，读取正文附在消息后面组成一组，读取失败时拒收输入。
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
	// 3. 登记 InputID 并回复接收成功；用户输入让暂停的 Agent 消息恢复开新 Turn。
	if request.inputID != "" {
		s.acceptedInputs[request.inputID] = request.item.ID
	}
	request.reply <- inputAccepted{itemID: request.item.ID}
	telemetry.GetLogger(ctx).Info("input accepted",
		"openvaala.input.item_id", request.item.ID, "openvaala.input.id", request.inputID, "mode", request.mode)
	if request.item.Message.From != nil && request.item.Message.From.Kind == defs.IdentityUser {
		s.isAgentMessagesPaused = false
	}
	// 4. steer 输入遇到还在接收补充输入的活动 Turn 时挂上去，并唤醒 WaitSteer 的等待者。
	if request.mode == SessionInputModeSteer && s.activeTurnID != "" && s.acceptingSteer {
		s.activeSteer = append(s.activeSteer, items)
		s.wakeSteerWaiters(s.activeSteer)
		s.publishPendingInputs(events)
		return
	}
	// 5. 其他输入按发送方排队；用户的 interrupt 插到用户队列最前并取消活动 Turn；最后尝试开始下一个 Turn。
	input := inboxInput{
		groups:     [][]*defs.Item{items},
		parentSpan: request.parentSpan,
	}
	switch {
	case request.item.Message.From != nil && request.item.Message.From.Kind == defs.IdentityAgent:
		s.pendingAgentMessages = append(s.pendingAgentMessages, input)
	case request.mode == SessionInputModeInterrupt:
		s.pendingUserInputs = append([]inboxInput{input}, s.pendingUserInputs...)
		s.interruptActiveTurn()
	default:
		s.pendingUserInputs = append(s.pendingUserInputs, input)
	}
	s.publishPendingInputs(events)
	s.startNextTurn(ctx, events)
}

// handleCompactRequested 检查 Session 状态，空闲时启动只压缩的 Turn。
func (s *sessionImpl) handleCompactRequested(ctx context.Context, events chan<- SessionEvent, request compactRequested) {
	// 1. 已开始停止或所选模型不在目录里时回复错误；有活动 Turn 或排队的用户输入时回复 ErrSessionBusy，排队的 Agent 消息不算。
	if s.stopReason != nil {
		request.reply <- compactAccepted{err: fmt.Errorf("session is stopping: %w", s.stopReason)}
		return
	}
	if err := s.validateSelectedModel(); err != nil {
		request.reply <- compactAccepted{err: err}
		return
	}
	if s.activeTurnID != "" || len(s.pendingUserInputs) > 0 {
		request.reply <- compactAccepted{err: ErrSessionBusy}
		return
	}
	// 2. 启动只压缩的 Turn 并回复它的 ID。
	request.reply <- compactAccepted{turnID: s.startTurn(ctx, events, nil, true)}
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
	// 1. 确认请求来自活动 Turn，取出并清空挂着的补充输入。
	if request.turnID != s.activeTurnID {
		panic(fmt.Sprintf("Turn %q took steer while %q is active", request.turnID, s.activeTurnID))
	}
	items := s.activeSteer
	s.activeSteer = nil
	// 2. finish 为 true 且没有输入时停止接收，再交出取出的输入组。
	if request.finish && len(items) == 0 {
		s.acceptingSteer = false
	}
	s.publishPendingInputs(events)
	request.reply <- items
}

// handleSteerWaitRequested 有可回复的就立即回复 WaitSteer，否则登记等待者。
func (s *sessionImpl) handleSteerWaitRequested(request steerWaitRequested) {
	// 1. 没有活动 Turn 或已有未读输入时立即回复副本，否则登记回复通道。
	if s.activeTurnID == "" || len(s.activeSteer) > 0 {
		request.reply <- cloneItems(s.activeSteer)
		return
	}
	s.steerWaiters = append(s.steerWaiters, request.reply)
}

// handleTurnFinished 记下结束的 Turn，把没读到的补充输入放回队首，再尝试开始下一个 Turn。
func (s *sessionImpl) handleTurnFinished(ctx context.Context, events chan<- SessionEvent, finished turnFinished) {
	// 1. 确认结束的是活动 Turn，记下它和终态，清空活动 Turn，以空结果唤醒 WaitSteer 的等待者。
	if finished.turnID != s.activeTurnID {
		panic(fmt.Sprintf("Turn %q finished while %q is active", finished.turnID, s.activeTurnID))
	}
	unreadSteer := s.activeSteer
	s.lastTurnID = finished.turnID
	s.lastTurnStatus = finished.status
	s.lastTurnContextUsage = finished.contextUsage
	s.cancelActiveTurn()
	s.activeTurnID = ""
	s.cancelActiveTurn = nil
	s.activeSteer = nil
	s.acceptingSteer = false
	s.wakeSteerWaiters(nil)

	// 2. 终态没保存时停止 Session；已开始停止时不再安排输入。
	if errors.Is(finished.err, turn.ErrTurnUnsettled) {
		s.stop(finished.err)
	}
	if s.stopReason != nil {
		s.publishPendingInputs(events)
		return
	}
	// 3. 没读到的补充输入按用户、Agent 分开，各合成一批放回对应队列的最前面，不带提交方 span。
	var unreadUserGroups, unreadAgentGroups [][]*defs.Item
	for _, group := range unreadSteer {
		if from := group[0].Message.From; from != nil && from.Kind == defs.IdentityAgent {
			unreadAgentGroups = append(unreadAgentGroups, group)
			continue
		}
		unreadUserGroups = append(unreadUserGroups, group)
	}
	if len(unreadUserGroups) > 0 {
		s.pendingUserInputs = append([]inboxInput{{groups: unreadUserGroups}}, s.pendingUserInputs...)
	}
	if len(unreadAgentGroups) > 0 {
		s.pendingAgentMessages = append([]inboxInput{{groups: unreadAgentGroups}}, s.pendingAgentMessages...)
	}
	// 4. 尝试开始下一个 Turn。
	s.publishPendingInputs(events)
	s.startNextTurn(ctx, events)
}

// startNextTurn 没有活动 Turn 时，从输入队列取一批输入开始下一个普通 Turn。
func (s *sessionImpl) startNextTurn(ctx context.Context, events chan<- SessionEvent) {
	// 1. 有活动 Turn 时直接返回；否则先取用户队列的第一批，用户队列为空且 Agent 消息没暂停时把全部 Agent 消息合成一批。
	if s.activeTurnID != "" {
		return
	}
	var input inboxInput
	switch {
	case len(s.pendingUserInputs) > 0:
		input, s.pendingUserInputs = s.pendingUserInputs[0], s.pendingUserInputs[1:]
	case len(s.pendingAgentMessages) > 0 && !s.isAgentMessagesPaused:
		input.parentSpan = s.pendingAgentMessages[0].parentSpan
		for _, message := range s.pendingAgentMessages {
			input.groups = append(input.groups, message.groups...)
		}
		s.pendingAgentMessages = nil
	default:
		return
	}
	// 2. 用选出的这批输入启动普通 Turn。
	s.startTurn(ctx, events, &input, false)
}

// startTurn 创建 Turn 并启动它的执行任务，返回 TurnID。
func (s *sessionImpl) startTurn(ctx context.Context, events chan<- SessionEvent, input *inboxInput, isCompactOnly bool) defs.TurnID {
	// 1. 分配 TurnID，填入所选模型、压缩模型，以及上个普通 Turn 的模型和 Executor。
	turnID := defs.NewTurnID()
	turnOpt := s.turnOpt
	turnOpt.TurnID = turnID
	model, reasoningEffort := s.getTurnModel()
	turnOpt.Model = model
	turnOpt.ReasoningEffort = reasoningEffort
	turnOpt.CompactModel = defs.GetCompactModel(s.opt.Models, s.opt.Agent.CompactModel, model)
	turnOpt.PreviousModel = s.lastTurnModel
	turnOpt.PreviousContextUsage = s.lastTurnContextUsage
	turnOpt.PreviousExecutors = s.lastTurnExecutors
	turnOpt.IsCompactOnly = isCompactOnly
	// 2. 普通 Turn 以这批的第一组为输入，更新上个普通 Turn 的模型和 Executor，取走待交出的 SessionStart 来源和注入 Item。
	var turnInput []*defs.Item
	var parentSpan telemetry.SpanRef
	if !isCompactOnly {
		s.lastTurnModel = model.Ref
		s.lastTurnExecutors = executor.GetTurnExecutors(s.opt.Environments)
		turnOpt.HookStartSource, s.pendingHookStartSource = s.pendingHookStartSource, ""
		turnOpt.ContextItems, s.pendingContextItems = s.pendingContextItems, nil
		turnInput = input.groups[0]
		parentSpan = input.parentSpan
	}
	// 3. 创建 Turn，登记活动 Turn 和取消函数；只压缩的 Turn 不接收补充输入，普通 Turn 把这批其余各组挂成补充输入。
	turnSelection := defs.ModelSelection{
		Ref:             model.Ref,
		ReasoningEffort: reasoningEffort,
	}
	turnInstance, err := turn.NewTurn(&turnOpt)
	if err != nil {
		panic(fmt.Sprintf("create Session Turn from validated options: %v", err))
	}
	turnCtx, cancel := context.WithCancel(telemetry.ContextWithParentSpan(ctx, parentSpan))
	s.activeTurnID = turnID
	s.cancelActiveTurn = cancel
	s.acceptingSteer = !isCompactOnly
	if !isCompactOnly {
		s.activeSteer = slices.Clone(input.groups[1:])
	}

	s.publishPendingInputs(events)
	// 4. 在执行任务里发出 SessionTurnStarted，运行 Turn 并按顺序转发它的事件，转发完发出 SessionTurnFinished，再把结果交回事件循环。
	s.tasks.Go(func() {
		events <- &SessionTurnStarted{
			TurnID: turnID,
			Model:  turnSelection,
		}
		turnEvents := make(chan turn.TurnEvent)
		var forwarding sync.WaitGroup
		forwarding.Go(func() {
			for event := range turnEvents {
				events <- newSessionEvent(turnID, event)
			}
		})
		result, err := turnInstance.Run(turnCtx, turnInput, turnEvents)
		close(turnEvents)
		forwarding.Wait()
		events <- &SessionTurnFinished{
			TurnID:           turnID,
			InputItemIDs:     result.InputItemIDs,
			Status:           result.Status,
			Outputs:          result.Outputs,
			FinalModelCallID: result.FinalModelCallID,
			Err:              err,
		}
		s.turnEvents <- turnFinished{
			turnID:       turnID,
			status:       result.Status,
			contextUsage: result.ContextUsage,
			err:          err,
		}
	})
	return turnID
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

// getTurnModel 返回当前选择的模型和推理强度。
func (s *sessionImpl) getTurnModel() (*defs.ModelDefinition, string) {
	// 1. 读取已校验的选择对应的模型和强度，目录里没有时 panic。
	model, reasoningEffort, found := defs.GetSelectedModel(s.opt.Models, s.selectedModel, s.opt.Agent.Model)
	if !found {
		panic(fmt.Sprintf("selected model %s of Session %s is not in the model catalog", s.selectedModel.Ref, s.opt.Session.ID))
	}
	return model, reasoningEffort
}

// stop 记下停止原因，清空排队输入并取消活动 Turn。
func (s *sessionImpl) stop(err error) {
	// 1. 合并停止原因，丢弃还没开始的输入，取消还在执行的 Turn。
	s.stopReason = errors.Join(s.stopReason, err)
	s.pendingUserInputs = nil
	s.pendingAgentMessages = nil
	s.activeSteer = nil
	s.acceptingSteer = false
	if s.cancelActiveTurn != nil {
		s.cancelActiveTurn()
	}
}

// wakeSteerWaiters 回复全部 WaitSteer 等待者并清空登记。
func (s *sessionImpl) wakeSteerWaiters(items [][]*defs.Item) {
	// 1. 给每个等待者回复一份未读输入的副本，再清空登记；items 为 nil 表示 Turn 已结束。
	for _, waiter := range s.steerWaiters {
		waiter <- cloneItems(items)
	}
	s.steerWaiters = nil
}

// getStatus 在事件循环里汇总当前状态、排队输入数和最近 Turn。
func (s *sessionImpl) getStatus() SessionStatus {
	// 1. 填写活动 Turn、排队输入和上下文数量、最近 Turn；已开始停止时为 stopping，否则有活动 Turn 时为 running，都没有时为 idle。
	status := SessionStatus{
		State:                   SessionStateIdle,
		ActiveTurnID:            s.activeTurnID,
		PendingInputCount:       countInboxGroups(s.pendingUserInputs) + countInboxGroups(s.pendingAgentMessages),
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

// interruptActiveTurn 取消活动 Turn，返回其 ID。
func (s *sessionImpl) interruptActiveTurn() defs.TurnID {
	// 1. 没有活动 Turn 时返回空，否则取消它并返回 ID；结束结果仍由 Turn 执行任务交回。
	if s.activeTurnID == "" {
		return ""
	}
	s.cancelActiveTurn()
	return s.activeTurnID
}

// repairLatestTurn 把上次没结束的最近 Turn 保存为 interrupted，再恢复最近 Turn 的状态、模型和 Executor。
func (s *sessionImpl) repairLatestTurn(ctx context.Context) error {
	// 1. 读取最近一条 Turn 记录，没有记录时直接返回。
	page, err := s.opt.Query.ListTurns(ctx, dao.ListTurnsRequest{
		SessionID: s.opt.Session.ID,
		Order:     storage.Order{Direction: storage.DirectionDesc},
		Limit:     1,
	})
	if err != nil {
		return fmt.Errorf("list latest Session Turn: %w", err)
	}
	if len(page.Items) == 0 {
		return nil
	}
	latest := page.Items[0]
	// 2. 最近 Turn 还是 running 时，用日志尾部补全它的 Item 范围：尾部不属于它而记录里已有起始序号时返回错误，
	// 起始序号为空时从尾部往前分页找它的第一条 Item。
	if latest.Status == defs.TurnStatusRunning {
		request := dao.ListItemsRequest{
			ConversationID: s.opt.Session.ConversationID,
			SessionID:      s.opt.Session.ID,
			Order:          storage.DirectionDesc,
			Limit:          1,
		}
		tail, err := s.opt.Query.ListItems(ctx, request)
		if err != nil {
			return fmt.Errorf("recover Turn %q items: %w", latest.ID, err)
		}
		if len(tail.Items) == 0 || tail.Items[0].Item.TurnID != latest.ID {
			if latest.ItemsRange.FirstOrdinal != 0 {
				return fmt.Errorf("recover Turn %q: declared item start has no matching tail", latest.ID)
			}
		} else {
			latest.ItemsRange.LastOrdinal = uint64(tail.Items[0].Ordinal)
			if latest.ItemsRange.FirstOrdinal == 0 {
				latest.ItemsRange.FirstOrdinal = latest.ItemsRange.LastOrdinal
				request.Cursor = tail.Items[0].Ordinal
				request.Limit = 256
				for request.Cursor != 0 {
					page, err := s.opt.Query.ListItems(ctx, request)
					if err != nil {
						return fmt.Errorf("recover Turn %q item start: %w", latest.ID, err)
					}
					for _, logged := range page.Items {
						if logged.Item.TurnID != latest.ID {
							request.Cursor = 0
							break
						}
						latest.ItemsRange.FirstOrdinal = uint64(logged.Ordinal)
					}
					if request.Cursor == 0 || page.Next == 0 {
						break
					}
					request.Cursor = page.Next
				}
			}
		}
		// 3. 把这个 Turn 保存为 interrupted，保存失败时返回错误。
		latest.Status = defs.TurnStatusInterrupted
		latest.Error = "turn execution was interrupted"
		if err := s.opt.Mutation.SetTurn(ctx, latest); err != nil {
			return fmt.Errorf("interrupt latest Session Turn: %w", err)
		}
	}
	// 4. 恢复最近 Turn 的状态、模型和 Executor 列表。
	s.lastTurnID = latest.ID
	s.lastTurnStatus = latest.Status
	s.lastTurnContextUsage = latest.ContextUsage
	s.lastTurnModel = latest.Model.Ref
	s.lastTurnExecutors = latest.Executors
	return nil
}

// handleSteerInputRequested 将排队用户输入移到指定轮的补充尾部。
func (s *sessionImpl) handleSteerInputRequested(events chan<- SessionEvent, request steerInputRequested) {
	// 1. 检查指定轮仍接收补充，再取出排队用户输入。
	if request.turnID == "" || request.turnID != s.activeTurnID || !s.acceptingSteer {
		request.reply <- errors.New("turn is no longer accepting input")
		return
	}
	group, _ := s.takePendingUserGroup(request.itemID)
	if group == nil {
		request.reply <- errors.New("input is no longer pending")
		return
	}
	// 2. 完整组追加到补充尾部，唤醒等待者并发布列表。
	s.activeSteer = append(s.activeSteer, group)
	s.wakeSteerWaiters(s.activeSteer)
	s.publishPendingInputs(events)
	request.reply <- nil
}

// handleInterruptTurnWithInputRequested 将选中输入与旧未读补充同批置顶并取消旧轮。
func (s *sessionImpl) handleInterruptTurnWithInputRequested(events chan<- SessionEvent, request interruptTurnWithInputRequested) {
	// 1. 检查指定轮仍在运行，再取出排队用户输入。
	if request.turnID == "" || request.turnID != s.activeTurnID || s.stopReason != nil {
		request.reply <- errors.New("turn is no longer running")
		return
	}
	group, parentSpan := s.takePendingUserGroup(request.itemID)
	if group == nil {
		request.reply <- errors.New("input is no longer pending")
		return
	}
	// 2. 原序搬移全部未读补充，停止旧轮接收，将整批置顶并取消旧轮。
	groups := append([][]*defs.Item{group}, s.activeSteer...)
	s.activeSteer = nil
	s.acceptingSteer = false
	s.pendingUserInputs = append([]inboxInput{{
		groups:     groups,
		parentSpan: parentSpan,
	}}, s.pendingUserInputs...)
	s.cancelActiveTurn()
	s.publishPendingInputs(events)
	request.reply <- nil
}

// handleWithdrawInputRequested 原子取走普通排队或未读补充中的用户输入组。
func (s *sessionImpl) handleWithdrawInputRequested(events chan<- SessionEvent, request withdrawInputRequested) {
	// 1. 优先取普通排队组，未找到时从未读用户补充中取组。
	group, _ := s.takePendingUserGroup(request.itemID)
	if group == nil {
		for index, steerGroup := range s.activeSteer {
			if isUserInputGroup(steerGroup, request.itemID) {
				group = steerGroup
				s.activeSteer = slices.Delete(s.activeSteer, index, index+1)
				break
			}
		}
	}
	if group == nil {
		request.reply <- inputWithdrawn{err: errors.New("input is no longer pending")}
		return
	}
	// 2. 发布移除后的列表并交出完整组。
	s.publishPendingInputs(events)
	request.reply <- inputWithdrawn{items: group}
}

// takePendingUserGroup 从等待新轮的批次中取走一个完整用户输入组。
func (s *sessionImpl) takePendingUserGroup(itemID defs.ItemID) ([]*defs.Item, telemetry.SpanRef) {
	// 1. 按批次和组顺序查找用户输入，移除后清掉空批次。
	for inputIndex := range s.pendingUserInputs {
		input := &s.pendingUserInputs[inputIndex]
		for groupIndex, group := range input.groups {
			if !isUserInputGroup(group, itemID) {
				continue
			}
			parentSpan := input.parentSpan
			input.groups = slices.Delete(input.groups, groupIndex, groupIndex+1)
			if len(input.groups) == 0 {
				s.pendingUserInputs = slices.Delete(s.pendingUserInputs, inputIndex, inputIndex+1)
			}
			return group, parentSpan
		}
	}
	return nil, telemetry.SpanRef{}
}

// publishPendingInputs 发布未读补充及普通队列中的轻量用户列表。
func (s *sessionImpl) publishPendingInputs(events chan<- SessionEvent) {
	// 1. 创建独立轻量切片，先列未读用户补充，再按排队顺序列用户输入。
	pendingInputs := SessionPendingInputs{Inputs: make([]PendingInput, 0)}
	if s.acceptingSteer {
		pendingInputs.SteerTurnID = s.activeTurnID
	}
	for _, group := range s.activeSteer {
		if isUserInputGroup(group, group[0].ID) {
			pendingInputs.Inputs = append(pendingInputs.Inputs, newPendingInput(group[0], true))
		}
	}
	for _, input := range s.pendingUserInputs {
		for _, group := range input.groups {
			if isUserInputGroup(group, group[0].ID) {
				pendingInputs.Inputs = append(pendingInputs.Inputs, newPendingInput(group[0], false))
			}
		}
	}
	// 2. 发送新列表。
	events <- &SessionPendingInputsUpdated{PendingInputs: pendingInputs}
}
