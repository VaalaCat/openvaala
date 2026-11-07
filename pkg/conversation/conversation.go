package conversation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/httpclient"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
)

// Conversation 管理一段对话里的根 Session 和子 Session、子任务结果通知、执行环境、Skill 和客户端登记的 MCP。
type Conversation interface {
	// GetID 返回对话 ID。
	GetID() defs.ConversationID
	// GetRootSessionID 返回根 Session ID。
	GetRootSessionID() defs.SessionID
	// GetSkills 返回根 Agent 可用的 Skill 信息。
	GetSkills() []skill.SkillInfo
	// GetMCPServers 按执行环境顺序返回各 MCP 连接的当前状态，客户端登记的排在最后；还在连接的也直接列出。
	GetMCPServers() []MCPServerInfo
	// Run 启动执行环境和根 Session，串行处理换配置和客户端 MCP 登记，等 Session、连接和工作区全部停止后关闭 events；只能调用一次。
	// events 为 nil 时不发事件，否则调用方要一直读到它关闭；返回根 Session 的错误或停止原因，调用时 ctx 已取消则只做清理并返回 ctx.Err()。
	Run(ctx context.Context, events chan<- ConversationEvent) error
	// HandleInput 向根 Session 提交输入并返回 ItemID，执行结果看 Turn 结束事件；先等根 Session 首次启动，对话已停止或正在换配置时返回错误。
	HandleInput(ctx context.Context, input *ConversationInput) (defs.ItemID, error)
	// InterruptTurn 请求取消根 Session 的活动 Turn，返回它的 ID，不等取消完成；先等根 Session 首次启动，根 Session 没在运行或空闲时返回空。
	InterruptTurn(ctx context.Context) (defs.TurnID, error)
	// SteerInput 将根 Session 的排队用户输入移入指定轮。
	SteerInput(ctx context.Context, itemID defs.ItemID, turnID defs.TurnID) error
	// InterruptTurnWithInput 将根 Session 的排队用户输入置顶并取消指定轮。
	InterruptTurnWithInput(ctx context.Context, turnID defs.TurnID, itemID defs.ItemID) error
	// WithdrawInput 取出根 Session 的完整待处理用户输入组。
	WithdrawInput(ctx context.Context, itemID defs.ItemID) ([]*defs.Item, error)
	// AppendContextItems 把 Item 交给根 Session；先等根 Session 首次启动，对话已停止或正在换配置时返回错误。
	AppendContextItems(ctx context.Context, items []*defs.Item) (err error)
	// CompactContext 请求根 Session 压缩上下文并返回 TurnID，不等压缩完成；先等根 Session 首次启动，忙时返回 session.ErrSessionBusy，已停止时返回错误。
	CompactContext(ctx context.Context) (defs.TurnID, error)
	// IsTerminalBusy 运行中的根或子 Session 是否还有未结束的后台命令？
	IsTerminalBusy() bool
	// IsRestorableFromRecord 对话停止后按记录重新打开，能否回到同样的状态？
	IsRestorableFromRecord() bool
	// ListSessionInfos 按 SessionID 返回全部 Session 的记录、状态、已保存用量和所选模型窗口。
	ListSessionInfos(ctx context.Context) ([]SessionInfo, error)
	// GetSessionDetails 返回本对话中 Session 的模型、环境、Skill 和工具信息；Session 不属于本对话时返回错误。
	GetSessionDetails(ctx context.Context, sessionID defs.SessionID) (SessionDetails, error)
	// SetSessionModel 校验并保存模型和推理强度，通知运行中的 Session 从下个 Turn 起使用；已保存但通知失败时仍返回记录和错误。
	// 没指定强度时，原强度新模型也支持就保留，否则用新模型的默认强度。
	SetSessionModel(ctx context.Context, sessionID defs.SessionID, model defs.ModelSelection) (defs.Session, error)
	// GetExecutorSettings 返回当前配置的 Executor 设置，已补上 local 目录。
	GetExecutorSettings() defs.ExecutorSettings
	// SetExecutorSettings 按新设置装配配置，交给 Run 停掉旧 Session 和执行环境、重启根 Session，返回补上 local 目录的设置；客户端 MCP 登记保留。
	// 调用方要先确认没有执行中的 Turn；装配失败时旧配置不变，换上后根 Session 启动失败则停止对话并返回错误。
	SetExecutorSettings(ctx context.Context, settings defs.ExecutorSettings) (defs.ExecutorSettings, error)
	// SetDefinitions 按新定义和当前设置重新装配并替换配置，校验全部 Session 的 Agent 都在新定义里；WithoutExecutors 的对话不装配 Executor 和 Skill。
	SetDefinitions(ctx context.Context, definitions *ConversationDefinitions) error
	// SetClientMCPServers 替换指定客户端登记的全部 MCP server，空列表表示移除；等 Run 换完登记后返回，从下个 Turn 起生效。
	// 创建连接失败、对话已停止或调用方取消时返回错误。
	SetClientMCPServers(ctx context.Context, clientID string, servers []ClientMCPServer) error
}

// conversationImpl 由 Run 串行处理换配置和客户端 MCP 登记，每个 Session 各有一个 Run 任务和一个事件转发任务。
type conversationImpl struct {
	// opt 的 Query、Mutation、Environment、MaxImageBytes、WithoutExecutors 整个生命周期都用；Definitions 只用来建初始配置，之后以 config 为准。
	opt           ConversationOpt
	id            defs.ConversationID
	rootSessionID defs.SessionID
	// modelClient 是本对话全部 Session 共用的模型客户端。
	modelClient llmkit.Client
	// config 是当前配置，只由 Run 在全部 Session 任务结束后整份替换；其他任务通过 Load 读取。
	config atomic.Pointer[conversationConfig]
	// clientMCPServers 给 Session 提供客户端登记的 MCP 工具，构造时建好，换配置时不动。
	clientMCPServers executor.ClientMCPServers
	// clientMCPRunCancels 以客户端 ID 为键，值是取消这个客户端全部连接的 Run 的函数；只由 Run 读写。
	clientMCPRunCancels map[string]context.CancelFunc
	// configChanges 是交给 Run 串行处理的换配置请求。
	configChanges chan *configChange
	// clientMCPChanges 是交给 Run 串行处理的客户端 MCP 登记。
	clientMCPChanges chan *clientMCPChange
	// stopped 在 Run 不再处理 configChanges、clientMCPChanges 时关闭。
	stopped chan struct{}

	// sessionRecords 是全部 Session 的持久记录，每次写库后整条替换，不就地修改。
	sessionRecords syncmap.SyncMap[defs.SessionID, defs.Session]
	// runningSessions 是正在运行的 Session 实例。
	runningSessions syncmap.SyncMap[defs.SessionID, session.Session]

	// started 是 Run 只能调用一次的令牌（容量 1）。
	started chan struct{}
	// rootStarted 在 Run 第一次尝试启动根 Session 后关闭，成功失败都关。
	rootStarted chan struct{}
	// runCtx 是 Run 创建的对话运行 ctx；stopRun 以停止原因取消它，根 Session 退出或启动失败时调用。
	runCtx  context.Context
	stopRun context.CancelCauseFunc
	// appEvents 是 Run 收到的应用事件通道，nil 时不发事件。
	appEvents chan<- ConversationEvent
	// rootRunErr 由根 Session 的事件转发任务在事件全部转发后（handleSessionStopped）写入，Run 在 tasks.Wait 之后读取；
	// 换配置时根 Session 的退出不写它。
	rootRunErr error
	// tasks 保存每个 Session 的 Run 与事件转发任务，只在 Run 或已有 Session 任务内追加。
	tasks sync.WaitGroup
	// environmentTasks 是当前执行环境的 MCP 连接任务；只由 Run 读写，换配置时和 Run 返回前，在 Session 停完后停止。
	environmentTasks sync.WaitGroup
	// stopEnvironmentRuns 取消 environmentTasks；只由 Run 读写。
	stopEnvironmentRuns context.CancelFunc
	// clientMCPTasks 是客户端登记的 MCP 连接任务，只由 Run 启动，Run 返回前在 Session 停完后取消并等待。
	clientMCPTasks sync.WaitGroup
}

// ConversationOpt 是对话整个生命周期都不变的依赖，加上建对话时的定义。
type ConversationOpt struct {
	Query    dao.Query
	Mutation dao.Mutation
	// Definitions 是建对话时的定义；之后由 SetDefinitions 整份替换。
	Definitions *ConversationDefinitions
	// Environment 是客户端的环境变量（格式同 os.Environ），只给 local 种类的 Executor 执行命令用；
	// nil 时继承 serve 进程的环境。它只存在内存里，重新装配时沿用。
	Environment []string
	// MaxImageBytes 交给每个执行环境的 read_file，必须大于 0。
	MaxImageBytes int
	// WithoutExecutors 是否不装配 Executor 和 Skill？换定义时也不装配。
	WithoutExecutors bool
}

// CreateConversation 用 conversationID 装配配置并保存新对话，返回还没启动的对话。
func CreateConversation(ctx context.Context, conversationID defs.ConversationID, rootAgentName string, model defs.ModelSelection, settings, storedSettings defs.ExecutorSettings,
	opt *ConversationOpt) (Conversation, error) {
	// 1. 校验依赖，按新旧设置装配配置；旧记录中的未知 Executor 只记警告。
	if err := validateConversationDependencies(opt); err != nil {
		return nil, err
	}
	config, err := newConversationConfig(ctx, conversationID, settings, storedSettings, opt, opt.Definitions, nil)
	if err != nil {
		return nil, err
	}
	// 2. 保存根 Session 和对话记录，创建尚未启动的对话实例；保存失败时关闭已创建的工作区。
	record, root, err := saveNewConversation(ctx, conversationID, rootAgentName, model, config, opt)
	if err != nil {
		closeWorkspaces(ctx, config.workspaces)
		return nil, err
	}
	return newConversation(record, []defs.Session{root}, opt, config), nil
}

// saveNewConversation 选择根 Agent 和模型，保存根 Session 和对话记录。
func saveNewConversation(ctx context.Context, conversationID defs.ConversationID, rootAgentName string, model defs.ModelSelection, config *conversationConfig, opt *ConversationOpt) (defs.Conversation, defs.Session, error) {
	// 1. 查找根 Agent，未指定模型时使用 Agent 默认模型。
	definition, found := config.agentDefinitions[rootAgentName]
	if !found {
		return defs.Conversation{}, defs.Session{}, fmt.Errorf("root agent %q is not configured", rootAgentName)
	}
	selection, err := getRequestedModelSelection(config.models, model, definition.Model)
	if err != nil {
		return defs.Conversation{}, defs.Session{}, err
	}
	// 2. 分配根 Session ID，先保存根 Session，再保存含 Executor 设置的对话记录。
	record := defs.Conversation{
		ID:               conversationID,
		RootSessionID:    defs.NewSessionID(),
		ExecutorSettings: config.settings,
	}
	root := defs.Session{
		ID:             record.RootSessionID,
		ConversationID: record.ID,
		AgentName:      rootAgentName,
		Model:          selection,
	}
	if err := opt.Mutation.SetSession(ctx, root); err != nil {
		return defs.Conversation{}, defs.Session{}, fmt.Errorf("create Conversation %s root %s: %w", record.ID, root.ID, err)
	}
	if err := opt.Mutation.SetConversation(ctx, record); err != nil {
		return defs.Conversation{}, defs.Session{}, fmt.Errorf("create Conversation %s; root %s saved: %w", record.ID, root.ID, err)
	}
	return record, root, nil
}

// GetConversation 按已保存的对话记录读取全部 Session、装配配置，返回还没启动的对话。
func GetConversation(ctx context.Context, record defs.Conversation, storedSettings defs.ExecutorSettings, opt *ConversationOpt) (Conversation, error) {
	// 1. 校验依赖和根 Session 的对话归属、父级信息。
	if err := validateConversationDependencies(opt); err != nil {
		return nil, err
	}
	id := record.ID
	ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{ConversationID: id})
	root, found, err := opt.Query.GetSession(ctx, id, record.RootSessionID)
	if err != nil {
		return nil, err
	}
	if !found || root.ConversationID != id || root.ParentSessionID != "" || root.ParentTurnID != "" || root.ParentCallID != "" {
		return nil, errors.New("stored Conversation root relationship is invalid")
	}
	// 2. 读取全部 Session 并校验父子关系。
	var records []defs.Session
	for sessionRecord, err := range opt.Query.ScanSessions(ctx, id, storage.Order{Direction: storage.DirectionAsc}) {
		if err != nil {
			return nil, err
		}
		records = append(records, sessionRecord)
	}
	if err := validateSessionTree(record, records); err != nil {
		return nil, err
	}
	// 3. 按记录设置装配配置，校验各 Session 的 Agent，再创建尚未启动的对话实例。
	config, err := newConversationConfig(ctx, record.ID, record.ExecutorSettings, storedSettings, opt, opt.Definitions, records)
	if err != nil {
		return nil, err
	}
	return newConversation(record, records, opt, config), nil
}

// newConversation 用已校验的记录和配置创建对话实例。
func newConversation(record defs.Conversation, records []defs.Session, opt *ConversationOpt, config *conversationConfig) *conversationImpl {
	// 1. 创建共享模型客户端、空的客户端 MCP 登记和运行通道。
	c := &conversationImpl{
		opt:                 *opt,
		id:                  record.ID,
		rootSessionID:       record.RootSessionID,
		modelClient:         llmkit.NewClient(llmkit.ClientOptions{HTTPClient: httpclient.NewHTTPClient(httpclient.NewHTTPClientOpt())}),
		clientMCPServers:    executor.NewClientMCPServers(),
		clientMCPRunCancels: make(map[string]context.CancelFunc),
		configChanges:       make(chan *configChange),
		clientMCPChanges:    make(chan *clientMCPChange),
		stopped:             make(chan struct{}),
		started:             make(chan struct{}, 1),
		rootStarted:         make(chan struct{}),
	}
	// 2. 保存初始配置和各 Session 记录。
	c.config.Store(config)
	for _, value := range records {
		c.sessionRecords.Store(value.ID, value)
	}
	return c
}

// GetID 返回对话 ID。
func (c *conversationImpl) GetID() defs.ConversationID {
	// 1. 返回保存的对话 ID。
	return c.id
}

// GetRootSessionID 返回根 Session ID。
func (c *conversationImpl) GetRootSessionID() defs.SessionID {
	// 1. 返回保存的根 Session ID。
	return c.rootSessionID
}

// GetExecutorSettings 返回当前配置的 Executor 设置。
func (c *conversationImpl) GetExecutorSettings() defs.ExecutorSettings {
	// 1. 读取当前配置已补全 local 目录的 Executor 设置。
	return c.config.Load().settings
}

// GetSkills 返回根 Agent 可用的 Skill 信息。
func (c *conversationImpl) GetSkills() []skill.SkillInfo {
	// 1. 按根 Agent 的 SkillNames 筛选 Skill，并提取公开信息。
	config := c.config.Load()
	root, _ := c.sessionRecords.Load(c.rootSessionID)
	selected := skill.GetSkillsByNames(config.agentDefinitions[root.AgentName].SkillNames, config.skills)
	result := make([]skill.SkillInfo, 0, len(selected))
	for _, item := range selected {
		result = append(result, skill.SkillInfo{
			Name:        item.Name,
			Description: item.Description,
			Path:        item.Path,
			Executor:    item.Executor,
		})
	}
	return result
}

// GetMCPServers 返回环境和客户端登记的 MCP 连接信息。
func (c *conversationImpl) GetMCPServers() []MCPServerInfo {
	// 1. 按执行环境顺序收集 MCP 连接，最后追加客户端登记的连接。
	var servers []MCPServerInfo
	for _, environment := range c.config.Load().environments {
		for _, info := range environment.GetMCPServers() {
			servers = append(servers, MCPServerInfo{
				ExecutorID:     environment.GetID(),
				ConnectionInfo: info,
			})
		}
	}
	for _, info := range c.clientMCPServers.GetMCPServers() {
		servers = append(servers, MCPServerInfo{
			ExecutorID:     defs.ClientExecutorID,
			ConnectionInfo: info,
		})
	}
	return servers
}

// Run 启动执行环境和根 Session，串行处理换配置和客户端 MCP 登记，停止时等全部任务结束再关闭 events。
func (c *conversationImpl) Run(ctx context.Context, events chan<- ConversationEvent) error {
	// 1. 占用单次启动令牌，重复调用时返回错误；创建对话运行 ctx。
	select {
	case c.started <- struct{}{}:
	default:
		return errors.New("Conversation Run can only be called once")
	}
	ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{ConversationID: c.id})
	c.runCtx, c.stopRun = context.WithCancelCause(ctx)
	defer c.stopRun(nil)
	// 2. 调用时 ctx 已取消：关闭 rootStarted、stopped、工作区和 events，返回 ctx 的错误。
	if err := ctx.Err(); err != nil {
		close(c.rootStarted)
		close(c.stopped)
		closeWorkspaces(ctx, c.config.Load().workspaces)
		if events != nil {
			close(events)
		}
		return err
	}
	logger := telemetry.GetLogger(ctx)
	logger.Info("conversation started")
	c.appEvents = events

	// 3. 启动当前配置的执行环境和根 Session，关闭 rootStarted；根 Session 启动失败时停止对话。
	config := c.config.Load()
	config.sessionsCtx, config.stopSessions = context.WithCancelCause(c.runCtx)
	c.startEnvironments(config.environments)
	root, _ := c.sessionRecords.Load(c.rootSessionID)
	_, startErr := c.startSession(root, false)
	close(c.rootStarted)
	if startErr != nil {
		c.stopRun(startErr)
	}
	// 4. 串行处理换配置和客户端 MCP 登记，直到对话 ctx 取消或根 Session 退出。
	for c.runCtx.Err() == nil {
		select {
		case <-c.runCtx.Done():
		case change := <-c.configChanges:
			c.handleConfigChange(change)
		case change := <-c.clientMCPChanges:
			c.handleClientMCPChange(change)
		}
	}
	// 5. 停止接收请求，等全部 Session 任务结束，再停止执行环境和客户端 MCP 连接，最后关闭 events。
	close(c.stopped)
	c.tasks.Wait()
	c.stopEnvironments(c.config.Load())
	for _, cancel := range c.clientMCPRunCancels {
		cancel()
	}
	c.clientMCPTasks.Wait()
	if events != nil {
		close(events)
	}
	// 6. 优先返回根 Session 的启动错误，其次是根 Session.Run 的错误，最后是对话的停止原因。
	runErr := context.Cause(c.runCtx)
	if c.rootRunErr != nil {
		runErr = c.rootRunErr
	}
	if startErr != nil {
		runErr = startErr
	}
	logConversationStopped(logger, runErr)
	return runErr
}

// handleConfigChange 停止旧配置的 Session 和环境，换上新配置并重启根 Session。
func (c *conversationImpl) handleConfigChange(change *configChange) {
	// 1. 装配时读到的配置已经不是当前配置时，关闭新配置的工作区，回复 errConfigChanged。
	current := c.config.Load()
	if change.base != current {
		closeWorkspaces(c.runCtx, change.next.workspaces)
		change.reply <- errConfigChanged
		return
	}
	// 2. 以 errConversationReconfiguring 取消旧 Session，等全部任务结束后停止旧环境。
	current.stopSessions(errConversationReconfiguring)
	c.tasks.Wait()
	c.stopEnvironments(current)
	// 3. 换上新配置，启动执行环境和根 Session；根 Session 启动失败时停止对话。
	next := change.next
	next.sessionsCtx, next.stopSessions = context.WithCancelCause(c.runCtx)
	c.config.Store(next)
	c.startEnvironments(next.environments)
	root, _ := c.sessionRecords.Load(c.rootSessionID)
	if _, err := c.startSession(root, false); err != nil {
		c.stopRun(err)
		change.reply <- err
		return
	}
	// 4. 发出新模型目录、Agent 默认模型和 Executor 设置，回复变更成功。
	c.sendEvent(&ConversationReconfigured{
		Models:             next.models,
		AgentDefaultModels: getAgentDefaultModels(next.definitions.Agents),
		ExecutorSettings:   next.settings,
	})
	change.reply <- nil
}

// handleClientMCPChange 替换一个客户端登记的全部 MCP 连接。
func (c *conversationImpl) handleClientMCPChange(change *clientMCPChange) {
	// 1. 取消这个客户端原先登记的连接任务。
	if cancel, found := c.clientMCPRunCancels[change.clientID]; found {
		cancel()
		delete(c.clientMCPRunCancels, change.clientID)
	}
	// 2. 启动新连接；连接 ctx 不随对话 ctx 取消，换登记时或 Run 在全部 Session 停止后取消。
	if len(change.connections) > 0 {
		runCtx, cancel := context.WithCancel(telemetry.ContextWithAttrs(context.WithoutCancel(c.runCtx), telemetry.ExecutorID(defs.ClientExecutorID)))
		for _, connection := range change.connections {
			c.clientMCPTasks.Go(func() {
				if err := connection.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
					telemetry.GetLogger(runCtx).Warn("client MCP connection stopped", "mcp_server_id", connection.GetInfo().ServerID, "error", err)
				}
			})
		}
		c.clientMCPRunCancels[change.clientID] = cancel
	}
	// 3. 替换登记并关闭 applied，Session 从下一个 Turn 起使用新工具。
	c.clientMCPServers.SetMCPServers(change.clientID, change.connections)
	close(change.applied)
}

// startEnvironments 启动各执行环境的 MCP 连接任务。
func (c *conversationImpl) startEnvironments(environments []executor.ExecutionEnvironment) {
	// 1. 创建不随对话 ctx 取消的执行环境 ctx，为每个执行环境启动 MCP 连接任务。
	environmentCtx, cancel := context.WithCancel(context.WithoutCancel(c.runCtx))
	c.stopEnvironmentRuns = cancel
	for _, environment := range environments {
		environmentLogCtx := telemetry.ContextWithAttrs(environmentCtx, telemetry.ExecutorID(environment.GetID()))
		c.environmentTasks.Go(func() {
			// 连接意外断开时记警告，后续工具调用返回 mcp_server_unavailable。
			if err := environment.Run(environmentLogCtx); err != nil && !errors.Is(err, context.Canceled) {
				telemetry.GetLogger(environmentLogCtx).Warn("executor MCP connections stopped", "error", err)
			}
		})
	}
}

// stopEnvironments 停止当前执行环境的 MCP 连接任务，再关闭这套配置的工作区。
func (c *conversationImpl) stopEnvironments(config *conversationConfig) {
	// 1. 取消并等待执行环境的 MCP 连接任务，再关闭这套配置的工作区。
	c.stopEnvironmentRuns()
	c.environmentTasks.Wait()
	closeWorkspaces(c.runCtx, config.workspaces)
}

// SetExecutorSettings 装配并替换对话的 Executor 设置。
func (c *conversationImpl) SetExecutorSettings(ctx context.Context, settings defs.ExecutorSettings) (defs.ExecutorSettings, error) {
	// 1. 按新设置和当前定义装配配置，交给 Run 替换；装配期间对话换过配置时按最新配置重试，成功时返回补上 local 目录的设置。
	ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{ConversationID: c.id})
	for {
		base := c.config.Load()
		next, err := newConversationConfig(ctx, c.id, settings, base.settings, &c.opt, base.definitions, c.sessionRecords.Values())
		if err != nil {
			return defs.ExecutorSettings{}, err
		}
		err = c.applyConfig(ctx, base, next)
		if errors.Is(err, errConfigChanged) {
			continue
		}
		if err != nil {
			return defs.ExecutorSettings{}, err
		}
		return next.settings, nil
	}
}

// SetDefinitions 按新定义重新装配并换上对话配置。
func (c *conversationImpl) SetDefinitions(ctx context.Context, definitions *ConversationDefinitions) error {
	// 1. 按新定义和当前设置装配配置，交给 Run 替换；装配期间对话换过配置时按最新配置重试。
	ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{ConversationID: c.id})
	for {
		base := c.config.Load()
		next, err := newConversationConfig(ctx, c.id, base.settings, base.settings, &c.opt, definitions, c.sessionRecords.Values())
		if err != nil {
			return err
		}
		if err := c.applyConfig(ctx, base, next); !errors.Is(err, errConfigChanged) {
			return err
		}
	}
}

// applyConfig 向 Run 提交换配置请求并等待回复。
func (c *conversationImpl) applyConfig(ctx context.Context, base, next *conversationConfig) error {
	// 1. 提交装配好的配置；提交前对话已停止或调用方取消时，关闭新配置的工作区并返回错误。
	change := &configChange{
		base:  base,
		next:  next,
		reply: make(chan error, 1),
	}
	select {
	case c.configChanges <- change:
	case <-c.stopped:
		closeWorkspaces(ctx, next.workspaces)
		return fmt.Errorf("Conversation has stopped: %w", context.Cause(c.runCtx))
	case <-ctx.Done():
		closeWorkspaces(ctx, next.workspaces)
		return ctx.Err()
	}
	// 2. Run 收到请求后等待它回复，不再因调用方取消而中断。
	return <-change.reply
}

// SetClientMCPServers 创建客户端的新 MCP 连接交给 Run 替换登记，等替换完成后返回。
func (c *conversationImpl) SetClientMCPServers(ctx context.Context, clientID string, servers []ClientMCPServer) error {
	// 1. 创建全部新连接，任一失败时返回错误。
	connections, err := newClientMCPConnections(servers)
	if err != nil {
		return err
	}
	// 2. 向 Run 提交替换请求，提交前对话停止或调用方取消时返回错误。
	change := &clientMCPChange{
		clientID:    clientID,
		connections: connections,
		applied:     make(chan struct{}),
	}
	select {
	case c.clientMCPChanges <- change:
	case <-c.stopped:
		return fmt.Errorf("Conversation has stopped: %w", context.Cause(c.runCtx))
	case <-ctx.Done():
		return ctx.Err()
	}
	// 3. 等待 Run 完成替换。
	<-change.applied
	return nil
}

// HandleInput 向根 Session 提交应用发来的消息，返回接收的 ItemID。
func (c *conversationImpl) HandleInput(ctx context.Context, input *ConversationInput) (defs.ItemID, error) {
	// 1. 校验发送方为用户或 Agent，等待根 Session 首次启动。
	if input == nil || input.From == nil || (input.From.Kind != defs.IdentityUser && input.From.Kind != defs.IdentityAgent) {
		return "", errors.New("conversation input requires a user or agent sender")
	}
	select {
	case <-c.rootStarted:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// 2. 取运行中的根 Session，没在运行时返回错误。
	root, found := c.runningSessions.Load(c.rootSessionID)
	if !found {
		return "", errors.New("Conversation root Session is not running")
	}
	// 3. 复制消息内容并分配 Item ID，按指定模式和 InputID 提交给根 Session。
	item := &defs.Item{
		ID:   defs.NewItemID(),
		Kind: defs.ItemMessage,
		Message: &defs.Message{
			From: input.From.Clone(),
			To: &defs.Address{
				Kind: defs.IdentityAgent,
				ID:   string(c.rootSessionID),
			},
			Content: cloneContent(input.Content),
		},
	}
	itemID, err := root.HandleInput(ctx, &session.SessionInput{
		Mode:    input.Mode,
		Item:    item,
		InputID: input.InputID,
	})
	if err != nil {
		return "", err
	}
	return itemID, nil
}

// InterruptTurn 请求取消根 Session 的活动 Turn。
func (c *conversationImpl) InterruptTurn(ctx context.Context) (defs.TurnID, error) {
	// 1. 等根 Session 首次启动；根 Session 没在运行时返回空，否则提交取消请求。
	select {
	case <-c.rootStarted:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	root, found := c.runningSessions.Load(c.rootSessionID)
	if !found {
		return "", nil
	}
	return root.InterruptTurn(ctx)
}

// CompactContext 请求根 Session 执行一次上下文压缩。
func (c *conversationImpl) CompactContext(ctx context.Context) (defs.TurnID, error) {
	// 1. 等根 Session 首次启动；根 Session 没在运行时返回错误，否则提交压缩请求。
	select {
	case <-c.rootStarted:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	root, found := c.runningSessions.Load(c.rootSessionID)
	if !found {
		return "", errors.New("Conversation root Session is not running")
	}
	return root.CompactContext(ctx)
}

// AppendContextItems 把注入的 Item 交给根 Session，写在它的下一个 Turn 的输入之前。
func (c *conversationImpl) AppendContextItems(ctx context.Context, items []*defs.Item) (err error) {
	// 1. 等待根 Session 首次启动并查找运行实例。
	select {
	case <-c.rootStarted:
	case <-ctx.Done():
		return ctx.Err()
	}
	root, found := c.runningSessions.Load(c.rootSessionID)
	if !found {
		return errors.New("Conversation root Session is not running")
	}
	// 2. 将 Item 交给根 Session。
	return root.AppendContextItems(ctx, items)
}

// IsTerminalBusy 对话内运行的 Session 是否还有未结束的后台命令？
func (c *conversationImpl) IsTerminalBusy() bool {
	// 1. 逐个询问运行中的 Session，有一个还有未结束的后台命令就算忙。
	return slices.ContainsFunc(c.runningSessions.Values(), session.Session.IsTerminalBusy)
}

// IsRestorableFromRecord 对话停止后按记录重新打开，能否回到同样的状态？
func (c *conversationImpl) IsRestorableFromRecord() bool {
	// 1. 不带 Executor 的对话，记录里没有这个选择。
	if c.opt.WithoutExecutors {
		return false
	}
	// 2. 默认 Executor 是第一个环境；它是 virtual 时文件只在内存里。
	config := c.config.Load()
	if len(config.environments) == 0 {
		return true
	}
	defaultID := config.environments[0].GetID()
	index := slices.IndexFunc(config.executors, func(listed executor.Executor) bool { return listed.GetDefinition().ID == defaultID })
	return index < 0 || config.executors[index].GetDefinition().Kind != defs.ExecutorKindVirtual
}

// ListSessionInfos 按 SessionID 返回全部 Session 的查询信息。
func (c *conversationImpl) ListSessionInfos(ctx context.Context) ([]SessionInfo, error) {
	// 1. 查询全部 Session 的执行状态、已保存用量和模型窗口。
	return c.getSessionInfos(ctx, c.sessionRecords.Values())
}

// GetSessionDetails 返回 Session 的模型、Executor、Skill 和可用工具信息。
func (c *conversationImpl) GetSessionDetails(_ context.Context, sessionID defs.SessionID) (SessionDetails, error) {
	// 1. 查找 Session 和对应 Agent 定义，缺失时返回错误。
	record, found := c.sessionRecords.Load(sessionID)
	if !found {
		return SessionDetails{}, fmt.Errorf("Session %s is not in Conversation %s", sessionID, c.id)
	}
	config := c.config.Load()
	definition, found := config.agentDefinitions[record.AgentName]
	if !found {
		return SessionDetails{}, newAgentNotConfiguredError(record)
	}
	// 2. 创建协作工具，汇总 Executor、Skill 和模型可见的工具名（含已连接的 MCP 工具）。
	collaborationTools, err := newCollaborationTools(c, record, config.definitions)
	if err != nil {
		return SessionDetails{}, err
	}
	details := SessionDetails{
		ConversationID: c.id,
		Session:        record,
		Executors:      newExecutorDetails(config.environments, config.executors),
		Skills:         getSkillNames(skill.GetSkillsByNames(definition.SkillNames, config.skills)),
		Tools:          getSessionToolNames(config.environments, c.clientMCPServers, definition, collaborationTools),
	}
	// 3. 所选模型存在时填写公开模型信息和压缩模型，否则两者留空。
	if model, _, found := defs.GetSelectedModel(config.models, record.Model, definition.Model); found {
		details.Model = model.ClonePublic()
		details.CompactModel = defs.GetCompactModel(config.models, definition.CompactModel, model).ClonePublic()
	}
	return details, nil
}

// SetSessionModel 保存 Session 的模型选择，通知运行实例并返回保存后的记录。
func (c *conversationImpl) SetSessionModel(ctx context.Context, sessionID defs.SessionID, model defs.ModelSelection) (defs.Session, error) {
	// 1. 校验 Session 和模型存在，选择受支持的推理强度。
	record, found := c.sessionRecords.Load(sessionID)
	if !found {
		return defs.Session{}, fmt.Errorf("Session %s is not in Conversation %s", sessionID, c.id)
	}
	definition, found := c.config.Load().models[model.Ref]
	if !found {
		return defs.Session{}, fmt.Errorf("model %s is not configured", model.Ref)
	}
	selection, err := newModelSelection(definition, model.ReasoningEffort, record.Model.ReasoningEffort)
	if err != nil {
		return defs.Session{}, err
	}
	// 2. 先保存 Session 记录，再替换内存记录。
	record.Model = selection
	if err := c.opt.Mutation.SetSession(ctx, record); err != nil {
		return defs.Session{}, fmt.Errorf("save Session %s: %w", sessionID, err)
	}
	c.sessionRecords.Store(sessionID, record)
	// 3. 通知运行实例；通知失败时仍返回已保存的记录和错误。
	if instance, found := c.runningSessions.Load(sessionID); found {
		if err := instance.SelectModel(ctx, selection); err != nil {
			return record, fmt.Errorf("notify Session %s of model %s: %w", sessionID, selection.Ref, err)
		}
	}
	return record, nil
}

// startSession 返回 Session 的运行实例；没在运行时新建实例并启动它的 Run 和事件转发任务。
func (c *conversationImpl) startSession(record defs.Session, isSpawned bool) (session.Session, error) {
	// 1. 已在运行时返回现有实例，否则检查这套配置的 Session 还没停止、Agent 在目录里。
	if instance, found := c.runningSessions.Load(record.ID); found {
		return instance, nil
	}
	config := c.config.Load()
	if config.sessionsCtx.Err() != nil {
		return nil, fmt.Errorf("Conversation Sessions have stopped: %w", context.Cause(config.sessionsCtx))
	}
	agentDefinition, found := config.agentDefinitions[record.AgentName]
	if !found {
		return nil, newAgentNotConfiguredError(record)
	}
	// 2. 复制 Agent 定义，依次追加 Skill、执行环境、记忆规则和 Session 身份说明。
	definition := *agentDefinition
	skills := skill.GetSkillsByNames(definition.SkillNames, config.skills)
	if skillInstructions := prompt.FormatSkillInstructions(skills); skillInstructions != "" {
		definition.Instructions += "\n" + skillInstructions
	}
	if executorInstructions := prompt.FormatExecutorInstructions(newExecutorPromptEntries(config.environments)); executorInstructions != "" {
		definition.Instructions += "\n" + executorInstructions
	}
	if slices.ContainsFunc(config.environments, isMemoryEnvironment) {
		definition.Instructions += "\n" + prompt.FormatMemoryInstructions()
	}
	definition.Instructions += "\n" + prompt.FormatSessionIdentity(record)
	// 3. 创建协作工具和 Session 配置，读取日志最新序号并确定 SessionStart hook 的来源。
	collaborationTools, err := newCollaborationTools(c, record, config.definitions)
	if err != nil {
		return nil, err
	}
	opt := session.NewSessionOpt(record, c.opt.Query, c.opt.Mutation, c.modelClient, &definition)
	opt.Models = config.models
	opt.Environments = config.environments
	opt.AgentTools = collaborationTools
	opt.ClientMCPServers = c.clientMCPServers
	opt.Skills = skills
	opt.Hooks = config.definitions.Hooks
	lastSavedOrdinal, err := dao.GetLastOrdinal(config.sessionsCtx, c.opt.Query, c.id, record.ID)
	if err != nil {
		return nil, err
	}
	opt.HookStartSource = getHookStartSource(record, lastSavedOrdinal, isSpawned)
	instance, err := session.NewSession(opt)
	if err != nil {
		return nil, err
	}
	// 4. 登记运行实例；并发登记时只用最先存入的实例。
	if running, loaded := c.runningSessions.LoadOrStore(record.ID, instance); loaded {
		return running, nil
	}
	// 5. 先发出 ConversationSessionStarted，再启动 Session.Run 和事件转发任务。
	c.sendEvent(&ConversationSessionStarted{
		Session:          record,
		LastSavedOrdinal: lastSavedOrdinal,
	})
	events := make(chan session.SessionEvent)
	runErrs := make(chan error, 1)
	c.tasks.Go(func() {
		runErrs <- instance.Run(config.sessionsCtx, events)
	})
	c.tasks.Go(func() {
		c.routeSessionEvents(config, record, events)
		c.handleSessionStopped(config, record, instance, <-runErrs)
	})
	return instance, nil
}

// routeSessionEvents 把 Session 事件转发给应用，子 Session 的 Turn 结束时通知父 Session。
func (c *conversationImpl) routeSessionEvents(config *conversationConfig, record defs.Session, events <-chan session.SessionEvent) {
	// 1. 逐条转发事件；子 Session 的 Turn 结束时，按它启动时的配置把结果通知父 Session。
	isRoot := record.ID == c.rootSessionID
	for event := range events {
		c.sendEvent(&ConversationSessionEvent{
			SessionID: record.ID,
			Event:     event,
		})
		if finished, ok := event.(*session.SessionTurnFinished); ok && !isRoot {
			c.notifyParent(config, record, newSubagentResultMessage(record, finished))
		}
	}
}

// sendEvent 把事件发给应用。
func (c *conversationImpl) sendEvent(event ConversationEvent) {
	// 1. Run 收到的 events 不为 nil 时发送，否则丢弃。
	if c.appEvents != nil {
		c.appEvents <- event
	}
}

// handleSessionStopped 发送 Session 停止事件并移除运行实例。
func (c *conversationImpl) handleSessionStopped(config *conversationConfig, record defs.Session, instance session.Session, err error) {
	// 1. 向应用发送 Session 停止事件。
	c.sendEvent(&ConversationSessionStopped{
		SessionID: record.ID,
		Err:       err,
	})
	// 2. 根 Session 不是因换配置退出时，记下 Run 的错误，以退出原因停止对话，再移除实例。
	if record.ID == c.rootSessionID && !errors.Is(context.Cause(config.sessionsCtx), errConversationReconfiguring) {
		c.rootRunErr = err
		cause := errors.New("root Session exited")
		if err != nil {
			cause = fmt.Errorf("root Session %s exited: %w", record.ID, err)
		}
		c.stopRun(cause)
		c.runningSessions.DeleteIf(record.ID, func(running session.Session) bool { return running == instance })
		return
	}
	// 3. 移除已停止的实例；这套配置的 Session 还没停止时，向父 Session 发送异常退出消息。
	c.runningSessions.DeleteIf(record.ID, func(running session.Session) bool { return running == instance })
	if config.sessionsCtx.Err() != nil {
		return
	}
	c.notifyParent(config, record, newAgentMessage(record, record.ParentSessionID, defs.AgentRelationChild,
		fmt.Sprintf("子任务 %s 异常退出：%v", record.ID, err)))
}

// SteerInput 将排队用户输入交给根 Session 移入指定轮。
func (c *conversationImpl) SteerInput(ctx context.Context, itemID defs.ItemID, turnID defs.TurnID) error {
	// 1. 等待根 Session 首次启动并查找运行实例。
	select {
	case <-c.rootStarted:
	case <-ctx.Done():
		return ctx.Err()
	}
	root, found := c.runningSessions.Load(c.rootSessionID)
	if !found {
		return errors.New("Conversation root Session is not running")
	}
	// 2. 将操作交给根 Session。
	return root.SteerInput(ctx, itemID, turnID)
}

// InterruptTurnWithInput 将优先输入和指定轮交给根 Session。
func (c *conversationImpl) InterruptTurnWithInput(ctx context.Context, turnID defs.TurnID, itemID defs.ItemID) error {
	// 1. 等待根 Session 首次启动并查找运行实例。
	select {
	case <-c.rootStarted:
	case <-ctx.Done():
		return ctx.Err()
	}
	root, found := c.runningSessions.Load(c.rootSessionID)
	if !found {
		return errors.New("Conversation root Session is not running")
	}
	// 2. 将操作交给根 Session。
	return root.InterruptTurnWithInput(ctx, turnID, itemID)
}

// WithdrawInput 从根 Session 取出待处理用户输入组。
func (c *conversationImpl) WithdrawInput(ctx context.Context, itemID defs.ItemID) ([]*defs.Item, error) {
	// 1. 等待根 Session 首次启动并查找运行实例。
	select {
	case <-c.rootStarted:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	root, found := c.runningSessions.Load(c.rootSessionID)
	if !found {
		return nil, errors.New("Conversation root Session is not running")
	}
	// 2. 将操作交给根 Session。
	return root.WithdrawInput(ctx, itemID)
}
