// Package runtime 提供一个进程内 OpenVaala 的公开根入口：按 ConversationID 路由多个对话并订阅其事件。
package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"golang.org/x/sync/singleflight"
)

// Runtime 保存共享定义和运行中的对话，按 ConversationID 处理对话操作与订阅。
// 对话的装配、设置更新和客户端 MCP 登记在调用方 goroutine 里执行，运行表与定义只由 Runtime 事件循环修改。
type Runtime interface {
	// Run 运行到上下文结束，停止并等待全部对话后返回；只能调用一次。需要运行实例的方法在 Run 启动前等待，Run 返回后返回 ErrRuntimeStopped。
	Run(ctx context.Context) error
	// IsIdle 全部已打开对话是否都没有运行中的 Turn 或后台命令？包括子 Agent；Runtime 已停止时返回 ErrRuntimeStopped。
	IsIdle(ctx context.Context) (bool, error)

	// CreateConversation 按 Agent、模型、Executor 设置和环境创建并启动对话；模型目录为空返回 ErrNoModelConfigured，设置无效返回 conversation.ErrInvalidExecutorSettings。
	CreateConversation(ctx context.Context, request *CreateConversationRequest) (defs.Conversation, error)
	// OpenConversation 对话已打开时返回最新记录，否则按记录里的设置装配并启动；同一对话同时打开只装配一次，记录不存在返回 ErrConversationNotFound。
	OpenConversation(ctx context.Context, request *OpenConversationRequest) (defs.Conversation, error)
	// IsConversationOpen 对话是否有尚未退出的运行实例？
	IsConversationOpen(ctx context.Context, request *GetConversationRequest) (bool, error)
	// SetConversationExecutorSettings 更新完整 Executor 设置并返回记录；有 Session 在执行 Turn 时返回 session.ErrSessionBusy，新增未知 Executor 返回 conversation.ErrInvalidExecutorSettings。
	SetConversationExecutorSettings(ctx context.Context, request *SetConversationExecutorSettingsRequest) (defs.Conversation, error)
	// GetConversation 读取对话记录，对话没打开也能读；找不到时返回 ErrConversationNotFound。
	GetConversation(ctx context.Context, request *GetConversationRequest) (defs.Conversation, error)
	// ListConversations 分页读取匹配的对话及根 Session；Archived 为 true 只查已归档，否则只查未归档。
	ListConversations(ctx context.Context, request *dao.ListConversationsRequest) (storage.Page[dao.ConversationListEntry, dao.RecordCursor[defs.ConversationID]], error)
	// SetConversationTitle 保存标题的前 256 个字符，已打开对话的订阅者会收到标题变化。
	SetConversationTitle(ctx context.Context, request *SetConversationTitleRequest) error
	// ForkConversation 复制已保存对话到指定轮次并打开分叉；没有可复制轮次时新建未保存对话，临时和未保存来源返回 ErrConversationNotFound。
	ForkConversation(ctx context.Context, request *ForkConversationRequest) (defs.Conversation, error)
	// SetConversationArchived 更新归档时间并返回记录，状态相同时不写入；临时和未保存对话返回 ErrConversationNotFound。
	SetConversationArchived(ctx context.Context, request *SetConversationArchivedRequest) (defs.Conversation, error)
	// DeleteConversation 停止对话并删除全部记录和历史；有 Session 在执行 Turn 时返回 session.ErrSessionBusy，临时和未保存对话返回 ErrConversationNotFound。
	DeleteConversation(ctx context.Context, request *DeleteConversationRequest) error

	// SubmitInput 将输入交给已打开对话的根 Session，返回 ItemID；普通新对话先保存，用户输入接收后用正文给空标题命名。
	SubmitInput(ctx context.Context, request *SubmitInputRequest) (defs.ItemID, error)
	// InterruptTurn 请求取消根 Session 的活动 Turn，不等待结束；没有活动 Turn 时返回空 TurnID。
	InterruptTurn(ctx context.Context, request *InterruptTurnRequest) (defs.TurnID, error)
	// SteerInput 将根 Session 的排队用户输入移入指定轮。
	SteerInput(ctx context.Context, request *SteerInputRequest) error
	// InterruptTurnWithInput 将排队用户输入置顶并取消指定轮。
	InterruptTurnWithInput(ctx context.Context, request *InterruptTurnWithInputRequest) error
	// WithdrawInput 原子取走根 Session 的完整待处理用户输入组。
	WithdrawInput(ctx context.Context, request *WithdrawInputRequest) ([]*defs.Item, error)
	// CompactContext 请求根 Session 启动压缩 Turn 并返回 TurnID；有活动 Turn 或排队输入时返回 session.ErrSessionBusy，普通新对话先保存。
	CompactContext(ctx context.Context, request *CompactContextRequest) (defs.TurnID, error)
	// SubscribeConversation 订阅已打开对话的标题与全部 Session 运行信息，先返回初始信息再有序推送变化，订阅上下文结束后退订。
	SubscribeConversation(ctx context.Context, request *SubscribeConversationRequest) (*ConversationSubscription, error)
	// SubscribeSession 订阅已打开对话中一个 Session 的内容，订阅上下文结束后退订。
	SubscribeSession(ctx context.Context, request *SubscribeSessionRequest) (*SessionSubscription, error)
	// SubscribeRunningConversations 返回当前运行集合并订阅变化，ctx 结束后退订。
	SubscribeRunningConversations(ctx context.Context) (subscription *RunningConversationsSubscription, err error)

	// GetSessionContextUsage 读取已打开对话中一个 Session 的上下文占用详情，取自最近的测量和模型调用记录。
	GetSessionContextUsage(ctx context.Context, request *GetSessionContextUsageRequest) (SessionContextUsage, error)
	// GetSessionDetails 读取已打开对话中的 Session 配置和最新标题。
	GetSessionDetails(ctx context.Context, request *GetSessionDetailsRequest) (conversation.SessionDetails, error)
	// GetMCPServers 返回已打开对话的执行环境及客户端 MCP 连接情况，不等待连接；未打开时返回 ErrConversationNotOpen。
	GetMCPServers(ctx context.Context, request *GetConversationRequest) ([]conversation.MCPServerInfo, error)
	// SetClientMCPServers 替换客户端为已打开对话登记的全部 MCP server，下个 Turn 起生效；空列表移除全部，无效登记返回错误且保留原登记。
	SetClientMCPServers(ctx context.Context, request *SetClientMCPServersRequest) error
	// ListModels 返回公开模型目录和指定 Agent 的默认模型，不含连接信息与凭证。
	ListModels(ctx context.Context, request *ListModelsRequest) (ModelList, error)
	// GetHooks 按配置顺序返回 Hook 信息，未配置时返回空切片。
	GetHooks(ctx context.Context) ([]*hook.HookInfo, error)
	// ListAgents 按配置顺序返回当前定义中的 Agent 名、说明和默认模型，不含系统指令。
	ListAgents(ctx context.Context) ([]AgentInfo, error)
	// ListExecutors 按 local、builtin、配置顺序返回 Executor 说明和默认目录。
	ListExecutors(ctx context.Context) ([]conversation.ExecutorDetails, error)
	// ResolveWorkspaceDirectory 按当前 Executor 定义解析工作目录；SSH Executor 每次调用建立连接，结束后断开。
	ResolveWorkspaceDirectory(ctx context.Context, request *ResolveWorkspaceDirectoryRequest) (string, error)
	// OpenTerminalWorkspace 按当前 Executor 定义为一个终端新建独立工作区，不借用任何对话的工作区；virtual Executor 返回 workspace.ErrTTYUnsupported。
	OpenTerminalWorkspace(ctx context.Context, request *OpenTerminalWorkspaceRequest) (*TerminalWorkspace, error)
	// GetWorkspaceDirectories 列出指定 Executor 上一个目录的普通直接子目录，请求结束后关闭临时工作区。
	GetWorkspaceDirectories(ctx context.Context, request *GetWorkspaceDirectoriesRequest) (directories *conversation.WorkspaceDirectories, err error)
	// SetDefinitions 替换整份定义，并行更新空闲对话；等待全部更新结束，结果列出未更新的忙碌对话，单个更新失败只记日志并保留原配置。
	SetDefinitions(ctx context.Context, request *Definitions) (SetDefinitionsResult, error)
	// GetSkills 返回对话根 Agent 或草稿 Agent 的可用 Skill 目录；草稿按 Executor 设置扫描项目目录和主目录。
	GetSkills(ctx context.Context, request *GetSkillsRequest) ([]skill.SkillInfo, error)
	// SetSessionModel 保存已打开对话中 Session 的模型和推理强度，下个 Turn 起生效，返回记录并推送运行信息；已保存但通知 Session 失败时，仍推送运行信息并返回记录和错误。
	SetSessionModel(ctx context.Context, request *SetSessionModelRequest) (defs.Session, error)

	// ListSessions 分页读取对话内的 Session 记录。
	ListSessions(ctx context.Context, request *dao.ListSessionsRequest) (storage.Page[defs.Session, dao.RecordCursor[defs.SessionID]], error)
	// GetSessionByID 从共享库按 SessionID 读取记录（含所属 ConversationID）；未找到时返回 ErrSessionNotFound。
	GetSessionByID(ctx context.Context, request *GetSessionByIDRequest) (defs.Session, error)
	// ListTurns 按请求排序分页读取 Session 的 Turn，默认按 TurnID，也支持 UpdatedAt；不要求对话已打开。
	ListTurns(ctx context.Context, request *ListTurnsRequest) (storage.Page[defs.Turn, dao.RecordCursor[defs.TurnID]], error)
	// GetTurns 按请求顺序返回找到的 Turn，跳过不存在的记录，不要求对话已打开。
	GetTurns(ctx context.Context, request *GetTurnsRequest) ([]defs.Turn, error)
	// ListItems 分页读取对话内一个 Session 的日志。
	ListItems(ctx context.Context, request *ListItemsRequest) (storage.Page[dao.LoggedItem, storage.Ordinal], error)
}

// runtimeImpl 保存当前定义、运行中的对话表、没保存的对话存储和交给 Run 事件循环的请求通道。
type runtimeImpl struct {
	// opt 的 Definitions 只用来建初始的 definitions，构造后清空。
	opt RuntimeOpt
	// query、mutation 是用共用库的 Records、Logs 构造的 DAO，已保存的对话都用它。
	query    dao.Query
	mutation dao.Mutation
	// definitions 是当前的定义：只由 Run 的事件循环整份替换，读的地方每次调用取一次。
	definitions atomic.Pointer[runtimeDefinitions]

	// started 是 Run 只能调用一次的令牌。
	started chan struct{}
	// stopped 在 Run 返回前关闭。
	stopped chan struct{}
	// requests 是交给 Run 事件循环的请求。
	requests chan runtimeRequest

	// runningConversations 是运行中或正在退出的对话；只由 Run 的事件循环读写。
	runningConversations map[defs.ConversationID]*runtimeConversation
	// runningChanged 合并已打开对话运行状态变化的唤醒；对话循环只做非阻塞发送。
	runningChanged chan struct{}
	// runningSubscribers 是全局运行集合的订阅，只由 Runtime 事件循环读写。
	runningSubscribers map[*subscriptionStream[RunningConversations]]*runtimeSubscriber
	// openGroup 让同一个对话 ID 同时只在一个调用方 goroutine 里装配实例，同时打开的调用方共用它的结果。
	openGroup singleflight.Group
	// unsavedConversationStores 是活着的临时对话和还没保存的新对话各自的存储：建对话时存入；
	// 新对话 Persist 成功，或对话结束、被移出运行表时删除并关闭内存库。
	unsavedConversationStores syncmap.SyncMap[defs.ConversationID, *unsavedConversationStore]
	// tasks 是各对话的运行任务，只在 Run 的事件循环内启动，Run 返回前等待。
	tasks sync.WaitGroup
}

// RuntimeOpt 保存全部对话共享的依赖，构造后只读。
type RuntimeOpt struct {
	// Records、Logs 是共用库的记录和日志存储；Runtime 用它们建共用的 DAO，也用作新对话暂存记录的目标。
	Records storage.RecordStorage
	Logs    storage.LogStorage
	// Definitions 是初始的整份定义：Agent 至少一个，每个 Session 按持久化的 AgentName 选择；各 Agent 的默认模型和摘要模型
	// 引用模型目录里的项，模型目录为空时不能新建对话，等 SetDefinitions 加上模型；MCP server 只给至少一个 Agent 会用到的建连接。
	Definitions *Definitions
	// Telemetry 是遥测管道，由 Run 挂到 ctx 供全部对话使用；nil 时使用全局兜底。
	Telemetry *telemetry.Telemetry
	// UnsavedConversationIdleTimeout 是没保存的对话（临时对话、还没收到第一条输入的新对话）没有订阅者、也没有 Session 在跑后，
	// 等多久结束并丢弃；NewRuntimeOpt 默认 60 秒。
	UnsavedConversationIdleTimeout time.Duration
	// MaxImageBytes 是 read_file 交给模型的单张图片上限，来自 tools.max_image_kb；serve 启动时定，reload 不变。
	// NewRuntimeOpt 默认 500 KiB。
	MaxImageBytes int
}

// NewRuntimeOpt 设置共享存储、初始定义和默认运行配置。
func NewRuntimeOpt(records storage.RecordStorage, logs storage.LogStorage, definitions *Definitions) *RuntimeOpt {
	// 1. 填入共享依赖，未保存对话的空闲时长设为 60 秒，单图上限设为 500 KiB。
	return &RuntimeOpt{
		Records:                        records,
		Logs:                           logs,
		Definitions:                    definitions,
		UnsavedConversationIdleTimeout: 60 * time.Second,
		MaxImageBytes:                  500 << 10,
	}
}

// WithUnsavedConversationIdleTimeout 修改未保存对话的空闲丢弃时长。
func (opt *RuntimeOpt) WithUnsavedConversationIdleTimeout(timeout time.Duration) *RuntimeOpt {
	// 1. 保存空闲时长并返回同一份配置。
	opt.UnsavedConversationIdleTimeout = timeout
	return opt
}

// NewRuntime 校验依赖，创建共享 DAO 并保存初始定义。
func NewRuntime(opt *RuntimeOpt) (Runtime, error) {
	// 1. 检查存储和至少一个 Agent 定义，空闲丢弃时长必须为正数。
	if opt == nil || opt.Records == nil || opt.Logs == nil || opt.Definitions == nil || len(opt.Definitions.Agents) == 0 {
		return nil, errors.New("runtime requires record storage, log storage and at least one agent")
	}
	if opt.UnsavedConversationIdleTimeout <= 0 {
		return nil, errors.New("runtime requires a positive unsaved conversation idle timeout")
	}
	// 2. 创建共享 DAO。
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(opt.Records, opt.Logs))
	if err != nil {
		return nil, fmt.Errorf("create runtime DAO: %w", err)
	}
	// 3. 初始化请求通道和运行表，保存初始定义后清掉 opt 里的定义；模型目录可以为空。
	r := &runtimeImpl{
		opt:                  *opt,
		query:                query,
		mutation:             mutation,
		started:              make(chan struct{}, 1),
		stopped:              make(chan struct{}),
		requests:             make(chan runtimeRequest),
		runningConversations: make(map[defs.ConversationID]*runtimeConversation),
		runningChanged:       make(chan struct{}, 1),
		runningSubscribers:   make(map[*subscriptionStream[RunningConversations]]*runtimeSubscriber),
	}
	r.definitions.Store(newRuntimeDefinitions(opt.Definitions))
	r.opt.Definitions = nil
	return r, nil
}

// Run 运行 Runtime 事件循环：串行处理运行表和定义的变更，ctx 结束后等全部对话退出再返回。
func (r *runtimeImpl) Run(ctx context.Context) error {
	// 1. 拒绝重复运行，登记退出通知和遥测。
	select {
	case r.started <- struct{}{}:
	default:
		return errors.New("Runtime Run can only be called once")
	}
	defer close(r.stopped)
	ctx = telemetry.ContextWithTelemetry(ctx, r.opt.Telemetry)
	// 2. 分发查找、启动、停止、记录修改和定义替换请求。
	for {
		select {
		case request := <-r.requests:
			switch request := request.(type) {
			case *runningConversationRequested:
				r.handleRunningConversationRequested(request)
			case *conversationStartRequested:
				r.handleConversationStartRequested(ctx, request)
			case *conversationStopped:
				r.handleConversationStopped(ctx, request)
			case *conversationTitleChangeRequested:
				r.handleConversationTitleChangeRequested(request)
			case *conversationArchiveChangeRequested:
				r.handleConversationArchiveChangeRequested(request)
			case *conversationDeleteRequested:
				r.handleConversationDeleteRequested(ctx, request)
			case *runningConversationsRequested:
				r.handleRunningConversationsRequested(request)
			case *definitionsChangeRequested:
				r.handleDefinitionsChangeRequested(request)
			case *runningSubscribeRequested:
				r.handleRunningSubscribeRequested(ctx, request)
			case *runningUnsubscribeRequested:
				removeSubscriber(r.runningSubscribers, request.stream, request.reason)
			default:
				panic(fmt.Sprintf("unsupported Runtime request %T", request))
			}
		case <-r.runningChanged:
			r.publishRunningConversations(ctx)
		case <-ctx.Done():
			// 3. 上下文结束后等待全部对话退出，关闭仍登记的内存库，再返回上下文错误。
			for stream := range r.runningSubscribers {
				removeSubscriber(r.runningSubscribers, stream, ErrRuntimeStopped)
			}
			r.tasks.Wait()
			for _, conversationID := range r.unsavedConversationStores.Keys() {
				r.discardUnsavedConversationStore(ctx, conversationID)
			}
			return ctx.Err()
		}
	}
}

// CreateConversation 创建并启动新对话。
func (r *runtimeImpl) CreateConversation(ctx context.Context, request *CreateConversationRequest) (defs.Conversation, error) {
	// 1. 新对话没有已存的 Executor 设置，传零值创建。
	return r.createConversation(ctx, request, defs.ExecutorSettings{})
}

// createConversation 打开内存库，装配新对话并登记运行实例；storedSettings 是校验用的原设置，分叉时传来源的设置，新建时为零值。
func (r *runtimeImpl) createConversation(ctx context.Context, request *CreateConversationRequest, storedSettings defs.ExecutorSettings) (defs.Conversation, error) {
	// 1. 读取当前定义，模型目录为空时返回 ErrNoModelConfigured。
	definitions := r.definitions.Load()
	if len(definitions.conversationDefinitions.Models) == 0 {
		return defs.Conversation{}, ErrNoModelConfigured
	}
	// 2. 打开新对话的内存库。
	store, err := r.openUnsavedConversationStore(request.Ephemeral)
	if err != nil {
		return defs.Conversation{}, err
	}
	// 3. 按环境变量、Executor 设置和原设置装配对话；设置无效时返回 conversation.ErrInvalidExecutorSettings，装配失败时关闭内存库。
	opt := &conversation.ConversationOpt{
		Query:         store.query,
		Mutation:      store.mutation,
		Definitions:   definitions.conversationDefinitions,
		Environment:   request.Environment,
		MaxImageBytes: r.opt.MaxImageBytes,
		Ephemeral:     request.Ephemeral,
	}
	instance, err := conversation.CreateConversation(ctx, request.AgentName, request.Model, request.ExecutorSettings, storedSettings, opt)
	if err != nil {
		return defs.Conversation{}, errors.Join(err, store.database.Close())
	}
	// 4. 登记并启动实例，登记失败时关闭内存库。
	result := r.startConversation(ctx, instance, store, nil)
	if result.err != nil {
		return defs.Conversation{}, errors.Join(result.err, store.database.Close())
	}
	return result.record, nil
}

// OpenConversation 返回已打开对话的记录，或装配并启动已有对话。
func (r *runtimeImpl) OpenConversation(ctx context.Context, request *OpenConversationRequest) (defs.Conversation, error) {
	// 1. 查找运行实例，已有实例时返回最新记录并忽略请求环境变量。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err == nil {
		return running.getRecord(ctx)
	}
	if !errors.Is(err, ErrConversationNotOpen) {
		return defs.Conversation{}, err
	}
	// 2. 同一对话共用一次装配结果；登记时已有实例则改取它的记录。
	opened, err, _ := r.openGroup.Do(string(request.ConversationID), func() (any, error) {
		record, err := r.GetConversation(ctx, &GetConversationRequest{ConversationID: request.ConversationID})
		if err != nil {
			return nil, err
		}
		result := r.openConversation(ctx, record, record.ExecutorSettings, request.Environment, false)
		if errors.Is(result.err, errConversationAlreadyOpen) {
			return result.runningConversation.getRecord(ctx)
		}
		return result.record, result.err
	})
	if err != nil {
		return defs.Conversation{}, err
	}
	return opened.(defs.Conversation), nil
}

// IsConversationOpen 对话是否有尚未退出的运行实例？
func (r *runtimeImpl) IsConversationOpen(ctx context.Context, request *GetConversationRequest) (bool, error) {
	// 1. 查找运行实例，将未打开错误转换为 false。
	_, err := r.getRunningConversation(ctx, request.ConversationID)
	if errors.Is(err, ErrConversationNotOpen) {
		return false, nil
	}
	return err == nil, err
}

// SetConversationExecutorSettings 更新运行对话的 Executor 设置，或按新设置打开已有对话。
func (r *runtimeImpl) SetConversationExecutorSettings(ctx context.Context, request *SetConversationExecutorSettingsRequest) (defs.Conversation, error) {
	// 1. 循环查找运行实例；装配期间被别人打开时重新查找。
	for {
		running, err := r.getRunningConversation(ctx, request.ConversationID)
		// 2. 已打开时：有 Session 在执行 Turn 返回 session.ErrSessionBusy；否则让对话原地换上新设置，再读取最新记录。
		if err == nil {
			if running.isAnySessionRunning(ctx) {
				return defs.Conversation{}, session.ErrSessionBusy
			}
			if _, err := running.conversation.SetExecutorSettings(ctx, request.ExecutorSettings); err != nil {
				return defs.Conversation{}, err
			}
			return running.getRecord(ctx)
		}
		if !errors.Is(err, ErrConversationNotOpen) {
			return defs.Conversation{}, err
		}
		// 3. 未打开时保留原设置用来校验，按新设置装配，登记时写入补齐 local 目录的设置。
		record, err := r.GetConversation(ctx, &GetConversationRequest{ConversationID: request.ConversationID})
		if err != nil {
			return defs.Conversation{}, err
		}
		storedSettings := record.ExecutorSettings
		record.ExecutorSettings = request.ExecutorSettings
		result := r.openConversation(ctx, record, storedSettings, nil, true)
		if !errors.Is(result.err, errConversationAlreadyOpen) {
			return result.record, result.err
		}
	}
}

// SetClientMCPServers 替换客户端为已打开对话登记的 MCP server。
func (r *runtimeImpl) SetClientMCPServers(ctx context.Context, request *SetClientMCPServersRequest) error {
	// 1. 取得运行对话。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return err
	}
	// 2. 交给对话换掉这个客户端的全部登记。
	return running.conversation.SetClientMCPServers(ctx, request.ClientID, request.Servers)
}

// GetConversation 读取对话记录。
func (r *runtimeImpl) GetConversation(ctx context.Context, request *GetConversationRequest) (defs.Conversation, error) {
	// 1. 从对话所用存储读取记录；不存在时返回 ErrConversationNotFound。
	query, _, _ := r.getConversationDAO(request.ConversationID)
	record, found, err := query.GetConversation(ctx, request.ConversationID)
	if err != nil {
		return defs.Conversation{}, err
	}
	if !found {
		return defs.Conversation{}, fmt.Errorf("%w: %s", ErrConversationNotFound, request.ConversationID)
	}
	// 2. 截取标题并返回记录。
	record.Title = defs.TruncateConversationTitle(record.Title)
	return record, nil
}

// ListConversations 返回一页对话和根 Session 记录。
func (r *runtimeImpl) ListConversations(
	ctx context.Context,
	request *dao.ListConversationsRequest,
) (storage.Page[dao.ConversationListEntry, dao.RecordCursor[defs.ConversationID]], error) {
	// 1. 从共享库查询匹配的对话。
	page, err := r.query.ListConversations(ctx, *request)
	if err != nil {
		return page, err
	}
	// 2. 截取各条标题并返回分页结果。
	for index := range page.Items {
		page.Items[index].Conversation.Title = defs.TruncateConversationTitle(page.Items[index].Conversation.Title)
	}
	return page, nil
}

// SetConversationTitle 经 Runtime 事件循环保存对话标题。
func (r *runtimeImpl) SetConversationTitle(ctx context.Context, request *SetConversationTitleRequest) error {
	// 1. 提交改名请求；Runtime 停止或上下文结束时返回对应错误。
	change := &conversationTitleChangeRequested{
		ctx:     ctx,
		request: request,
		reply:   make(chan error, 1),
	}
	select {
	case r.requests <- change:
	case <-r.stopped:
		return ErrRuntimeStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	// 2. 等待保存结果。
	return <-change.reply
}

// SubmitInput 保存新对话，把输入交给根 Session，用户输入时给空标题自动命名。
func (r *runtimeImpl) SubmitInput(ctx context.Context, request *SubmitInputRequest) (defs.ItemID, error) {
	// 1. 取得运行实例，并将尚未保存的新对话写进共享库；写入失败时不转交输入。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return "", err
	}
	if err := r.saveConversation(ctx, request.ConversationID); err != nil {
		return "", err
	}
	// 2. 把输入交给对话根 Session。
	itemID, err := running.conversation.HandleInput(ctx, &conversation.ConversationInput{
		Mode:    request.Mode,
		From:    request.From,
		Content: request.Content,
		InputID: request.InputID,
	})
	if err != nil {
		return "", err
	}
	// 3. 用户输入接收后，标题为空时用正文开头命名；请求取消也照样保存，失败只记日志。
	if request.From.Kind == defs.IdentityUser {
		if title := getTitleFromInput(request.Content); title != "" {
			titleCtx := context.WithoutCancel(ctx)
			if err := running.setTitle(titleCtx, title, true); err != nil {
				telemetry.GetLogger(ctx).Warn("save automatic conversation title failed", "error", err)
			}
		}
	}
	return itemID, nil
}

// InterruptTurn 请求中断已打开对话根 Session 的活动 Turn。
func (r *runtimeImpl) InterruptTurn(ctx context.Context, request *InterruptTurnRequest) (defs.TurnID, error) {
	// 1. 取得运行实例。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return "", err
	}
	// 2. 请求中断，不等待 Turn 结束。
	return running.conversation.InterruptTurn(ctx)
}

// CompactContext 保存新对话并请求根 Session 启动压缩 Turn。
func (r *runtimeImpl) CompactContext(ctx context.Context, request *CompactContextRequest) (defs.TurnID, error) {
	// 1. 取得运行实例。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return "", err
	}
	// 2. 写入尚未保存的新对话，失败时不启动压缩。
	if err := r.saveConversation(ctx, request.ConversationID); err != nil {
		return "", err
	}
	// 3. 请求压缩并返回 TurnID，不等待压缩结束。
	return running.conversation.CompactContext(ctx)
}

// SubscribeRunningConversations 经 Runtime 事件循环登记全局运行集合订阅。
func (r *runtimeImpl) SubscribeRunningConversations(ctx context.Context) (*RunningConversationsSubscription, error) {
	// 1. 提交订阅上下文，Runtime 停止或请求取消时返回错误。
	request := &runningSubscribeRequested{ctx: ctx, reply: make(chan runningSubscribeReply, 1)}
	select {
	case r.requests <- request:
	case <-r.stopped:
		return nil, ErrRuntimeStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 2. 返回初始集合及之后的事件流。
	reply := <-request.reply
	return reply.subscription, reply.err
}

// handleRunningSubscribeRequested 登记运行订阅并读取初始集合。
func (r *runtimeImpl) handleRunningSubscribeRequested(ctx context.Context, request *runningSubscribeRequested) {
	// 1. 登记订阅和取消监听；取消请求交回 Runtime 事件循环处理。
	stream := &subscriptionStream[RunningConversations]{events: make(chan RunningConversations, subscriptionBufferSize)}
	r.runningSubscribers[stream] = &runtimeSubscriber{
		stopWatching: context.AfterFunc(request.ctx, func() {
			select {
			case r.requests <- &runningUnsubscribeRequested{stream: stream, reason: context.Cause(request.ctx)}:
			case <-r.stopped:
			}
		}),
	}
	// 2. 初始读取同时响应 Runtime 和订阅方取消；失败时撤销登记并回复错误。
	readCtx, cancel := context.WithCancel(request.ctx)
	stopWatchingRuntime := context.AfterFunc(ctx, cancel)
	initial, err := r.getRunningConversations(readCtx)
	stopWatchingRuntime()
	cancel()
	if ctx.Err() != nil {
		err = ErrRuntimeStopped
	}
	if err != nil {
		removeSubscriber(r.runningSubscribers, stream, err)
		request.reply <- runningSubscribeReply{err: err}
		return
	}
	// 3. 返回当前完整集合，留在唤醒通道中的变化由后续循环重新读取。
	request.reply <- runningSubscribeReply{subscription: &RunningConversationsSubscription{
		Initial: initial, Events: stream.events, stream: stream,
	}}
}

// getRunningConversations 读取已打开对话中正在执行 Turn 的完整 ID 集合。
func (r *runtimeImpl) getRunningConversations(ctx context.Context) (RunningConversations, error) {
	// 1. 逐个询问已打开实例；已经退出的对话不属于运行集合。
	result := RunningConversations{ConversationIDs: make([]defs.ConversationID, 0)}
	for conversationID, running := range r.runningConversations {
		if !running.isStopped() && running.isAnySessionRunning(ctx) {
			result.ConversationIDs = append(result.ConversationIDs, conversationID)
		}
		if err := ctx.Err(); err != nil {
			return RunningConversations{}, err
		}
	}
	// 2. 取消不返回部分或假空集合，成功时按 ID 排序。
	if err := ctx.Err(); err != nil {
		return RunningConversations{}, err
	}
	slices.Sort(result.ConversationIDs)
	return result, nil
}

// publishRunningConversations 重新读取完整运行集合并发送给已登记的订阅者。
func (r *runtimeImpl) publishRunningConversations(ctx context.Context) {
	// 1. 没有订阅者时不读取；读取失败时关闭全部流。
	if len(r.runningSubscribers) == 0 {
		return
	}
	running, err := r.getRunningConversations(ctx)
	if err != nil {
		if ctx.Err() != nil {
			err = ErrRuntimeStopped
		}
		for stream := range r.runningSubscribers {
			removeSubscriber(r.runningSubscribers, stream, err)
		}
		return
	}
	// 2. 入队完整集合，缓冲已满的订阅以 ErrSubscriberTooSlow 关闭。
	for stream := range r.runningSubscribers {
		select {
		case stream.events <- running:
		default:
			removeSubscriber(r.runningSubscribers, stream, ErrSubscriberTooSlow)
		}
	}
}

// signalRunningConversationsChanged 非阻塞唤醒 Runtime 重新读取运行集合。
func signalRunningConversationsChanged(changed chan<- struct{}) {
	// 1. 容量已满时合并本次唤醒。
	select {
	case changed <- struct{}{}:
	default:
	}
}

// SubscribeConversation 登记已打开对话的标题和 Session 运行信息订阅。
func (r *runtimeImpl) SubscribeConversation(ctx context.Context, request *SubscribeConversationRequest) (*ConversationSubscription, error) {
	// 1. 取得运行实例。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return nil, err
	}
	// 2. 登记对话订阅并取得同一时刻的初始信息。
	reply, err := running.subscribe(ctx, "")
	if err != nil {
		return nil, err
	}
	// 3. 返回标题、Session 信息和后续事件通道。
	return &ConversationSubscription{
		Title:    reply.title,
		Sessions: reply.sessions,
		Events:   reply.statusStream.events,
		stream:   reply.statusStream,
	}, nil
}

// handleConversationTitleChangeRequested 按对话是否已打开，交给对话保存标题或直接写库。
func (r *runtimeImpl) handleConversationTitleChangeRequested(change *conversationTitleChangeRequested) {
	// 1. 已打开时让对话事件循环保存标题并回复。
	request, ctx := change.request, change.ctx
	if running := r.runningConversations[request.ConversationID]; running != nil && !running.isStopped() {
		change.reply <- running.setTitle(ctx, request.Title, false)
		return
	}
	// 2. 其余对话读取记录、截取新标题后写回，回复保存结果。
	_, mutation, _ := r.getConversationDAO(request.ConversationID)
	record, err := r.GetConversation(ctx, &GetConversationRequest{ConversationID: request.ConversationID})
	if err == nil {
		record.Title = defs.TruncateConversationTitle(request.Title)
		err = mutation.SetConversation(ctx, record)
	}
	change.reply <- err
}

// ForkConversation 按指定轮次复制已保存对话并打开分叉。
func (r *runtimeImpl) ForkConversation(ctx context.Context, request *ForkConversationRequest) (defs.Conversation, error) {
	// 1. 从共享库读取来源，读取当前定义并检查模型目录。
	source, err := r.getSavedConversation(ctx, request.ConversationID)
	if err != nil {
		return defs.Conversation{}, err
	}
	definitions := r.definitions.Load()
	if len(definitions.conversationDefinitions.Models) == 0 {
		return defs.Conversation{}, ErrNoModelConfigured
	}
	opt := &conversation.ConversationOpt{
		Query:       r.query,
		Mutation:    r.mutation,
		Definitions: definitions.conversationDefinitions,
	}
	// 2. 按请求截取点复制来源根 Session。
	forked, err := conversation.ForkConversation(ctx, &conversation.ForkConversationRequest{
		Source:           source,
		BeforeTurnID:     request.BeforeTurnID,
		LastTurnID:       request.LastTurnID,
		Model:            request.Model,
		ExecutorSettings: request.ExecutorSettings,
	}, opt)
	// 3. 截取点之前没有轮次时，沿用来源根 Agent 和模型创建未保存对话。
	if errors.Is(err, conversation.ErrNothingToFork) {
		root, err := r.getSession(ctx, source.ID, source.RootSessionID)
		if err != nil {
			return defs.Conversation{}, err
		}
		model := request.Model
		if model == (defs.ModelSelection{}) {
			model = root.Model
		}
		return r.createConversation(ctx, &CreateConversationRequest{
			AgentName:        root.AgentName,
			Model:            model,
			ExecutorSettings: request.ExecutorSettings,
			Environment:      request.Environment,
		}, source.ExecutorSettings)
	}
	if err != nil {
		return defs.Conversation{}, err
	}
	// 4. 按请求设置打开分叉，登记时写入补齐的设置；已被别人打开时返回运行实例的记录，不删除分叉。
	result := r.openConversation(ctx, forked, source.ExecutorSettings, request.Environment, true)
	if errors.Is(result.err, errConversationAlreadyOpen) {
		return result.runningConversation.getRecord(ctx)
	}
	// 5. 其余打开错误时删除复制出的记录与历史，删除失败只记日志。
	if result.err != nil {
		if err := r.mutation.DeleteConversation(context.WithoutCancel(ctx), forked.ID); err != nil {
			telemetry.GetLogger(ctx).Warn("delete forked conversation failed", "error", err)
		}
		return defs.Conversation{}, result.err
	}
	return result.record, nil
}

// SetConversationArchived 经 Runtime 事件循环更新归档状态，返回对话记录。
func (r *runtimeImpl) SetConversationArchived(ctx context.Context, request *SetConversationArchivedRequest) (defs.Conversation, error) {
	// 1. 提交归档请求；Runtime 停止或上下文结束时返回对应错误。
	change := &conversationArchiveChangeRequested{
		ctx:     ctx,
		request: request,
		reply:   make(chan conversationReply, 1),
	}
	select {
	case r.requests <- change:
	case <-r.stopped:
		return defs.Conversation{}, ErrRuntimeStopped
	case <-ctx.Done():
		return defs.Conversation{}, ctx.Err()
	}
	// 2. 等待更新结果，截取标题后返回记录。
	result := <-change.reply
	if result.err != nil {
		return defs.Conversation{}, result.err
	}
	result.record.Title = defs.TruncateConversationTitle(result.record.Title)
	return result.record, nil
}

// handleConversationArchiveChangeRequested 更新已保存对话的归档状态。
func (r *runtimeImpl) handleConversationArchiveChangeRequested(change *conversationArchiveChangeRequested) {
	// 1. 从共享库读取记录；临时和未保存对话按不存在处理。
	request, ctx := change.request, change.ctx
	record, err := r.getSavedConversation(ctx, request.ConversationID)
	if err != nil {
		change.reply <- conversationReply{err: err}
		return
	}
	// 2. 已打开时交给对话事件循环保存并回复。
	if running := r.runningConversations[request.ConversationID]; running != nil && !running.isStopped() {
		record, err = running.setArchived(ctx, request.Archived)
		change.reply <- conversationReply{
			record: record,
			err:    err,
		}
		return
	}
	// 3. 未打开时直接保存并回复。
	record, err = saveConversationArchived(ctx, r.mutation, record, request.Archived)
	change.reply <- conversationReply{
		record: record,
		err:    err,
	}
}

// saveConversationArchived 更新未打开对话的归档记录。
func saveConversationArchived(ctx context.Context, mutation dao.Mutation, record defs.Conversation, archived bool) (defs.Conversation, error) {
	// 1. 按传入的最新记录算出归档后的记录，状态相同时原样返回。
	record, isChanged := getArchivedConversation(record, archived)
	if !isChanged {
		return record, nil
	}
	// 2. 写入归档记录并返回，失败时包装对话 ID。
	if err := mutation.SetConversation(ctx, record); err != nil {
		return defs.Conversation{}, fmt.Errorf("set Conversation %s archived: %w", record.ID, err)
	}
	return record, nil
}

// DeleteConversation 经 Runtime 事件循环删除已保存对话。
func (r *runtimeImpl) DeleteConversation(ctx context.Context, request *DeleteConversationRequest) error {
	// 1. 提交删除请求；Runtime 停止或上下文结束时返回对应错误。
	change := &conversationDeleteRequested{
		request: request,
		reply:   make(chan error, 1),
	}
	select {
	case r.requests <- change:
	case <-r.stopped:
		return ErrRuntimeStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	// 2. 等待删除结果。
	return <-change.reply
}

// handleConversationDeleteRequested 删除空闲对话和它的历史。
func (r *runtimeImpl) handleConversationDeleteRequested(ctx context.Context, request *conversationDeleteRequested) {
	// 1. 确认对话已保存。
	conversationID := request.request.ConversationID
	if _, err := r.getSavedConversation(ctx, conversationID); err != nil {
		request.reply <- err
		return
	}
	// 2. 实例仍在运行时检查 Session，有 Session 在执行 Turn 时回复 session.ErrSessionBusy；否则取消并等待实例退出。
	if running := r.runningConversations[conversationID]; running != nil && !running.isStopped() {
		if running.isAnySessionRunning(ctx) {
			request.reply <- session.ErrSessionBusy
			return
		}
		// 只等 stopped 关闭；运行任务随后发来的 conversationStopped 由之后的事件循环处理，移除实例。
		running.cancel()
		<-running.stopped
	}
	// 3. 删除记录和日志，回复删除结果。
	request.reply <- r.mutation.DeleteConversation(ctx, conversationID)
}

// SubscribeSession 登记已打开对话中一个 Session 的内容订阅。
func (r *runtimeImpl) SubscribeSession(ctx context.Context, request *SubscribeSessionRequest) (*SessionSubscription, error) {
	// 1. 确认 Session 属于对话并取得运行实例。
	if _, err := r.getSession(ctx, request.ConversationID, request.SessionID); err != nil {
		return nil, err
	}
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return nil, err
	}
	// 2. 登记订阅，取得日志续传位置和生成中 Item。
	reply, err := running.subscribe(ctx, request.SessionID)
	if err != nil {
		return nil, err
	}
	// 3. 返回初始信息和后续事件通道。
	return &SessionSubscription{
		LastSavedOrdinal: reply.lastSavedOrdinal,
		StreamingItems:   reply.streamingItems,
		PendingInputs:    reply.pendingInputs,
		Events:           reply.contentStream.events,
		stream:           reply.contentStream,
	}, nil
}

// GetSessionContextUsage 读取已打开对话中一个 Session 的上下文占用。
func (r *runtimeImpl) GetSessionContextUsage(ctx context.Context, request *GetSessionContextUsageRequest) (SessionContextUsage, error) {
	// 1. 取得运行实例。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return SessionContextUsage{}, err
	}
	// 2. 请求对话事件循环读取占用信息并返回结果。
	reply := make(chan contextUsageReply, 1)
	if err := running.sendRequest(ctx, &contextUsageRequested{
		sessionID: request.SessionID,
		reply:     reply,
	}); err != nil {
		return SessionContextUsage{}, err
	}
	result := <-reply
	return result.usage, result.err
}

// GetSessionDetails 读取 Session 的完整配置。
func (r *runtimeImpl) GetSessionDetails(ctx context.Context, request *GetSessionDetailsRequest) (conversation.SessionDetails, error) {
	// 1. 取得运行实例并读取 Session 配置。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return conversation.SessionDetails{}, err
	}
	details, err := running.conversation.GetSessionDetails(ctx, request.SessionID)
	if err != nil {
		return conversation.SessionDetails{}, err
	}
	// 2. 读取对话事件循环里的最新记录，填入标题。
	record, err := running.getRecord(ctx)
	if err != nil {
		return conversation.SessionDetails{}, err
	}
	details.Title = record.Title
	return details, nil
}

// GetMCPServers 读取已打开对话的 MCP 连接情况。
func (r *runtimeImpl) GetMCPServers(ctx context.Context, request *GetConversationRequest) ([]conversation.MCPServerInfo, error) {
	// 1. 取得运行实例。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return nil, err
	}
	// 2. 返回当前连接情况，不等待连接：环境按对话内顺序、环境内按 server ID 排列；客户端登记在最后，ExecutorID 为 defs.ClientExecutorID。
	return running.conversation.GetMCPServers(), nil
}

// GetSkills 返回已打开对话或草稿 Agent 的可用 Skill 目录。
func (r *runtimeImpl) GetSkills(ctx context.Context, request *GetSkillsRequest) ([]skill.SkillInfo, error) {
	// 1. 指定对话时读取它已经加载的 Skill。
	if request.ConversationID != "" {
		running, err := r.getRunningConversation(ctx, request.ConversationID)
		if err != nil {
			return nil, err
		}
		return running.conversation.GetSkills(), nil
	}
	// 2. 草稿按 Agent 名称查定义，不存在时返回错误。
	definitions := r.definitions.Load().conversationDefinitions
	index := slices.IndexFunc(definitions.Agents, func(definition *agent.AgentDefinition) bool { return definition.Name == request.AgentName })
	if index < 0 {
		return nil, fmt.Errorf("agent %q is not configured", request.AgentName)
	}
	// 3. 按草稿 Executor 设置扫描 Skill（默认 Executor 的工作区建不起来时返回错误），筛出 Agent 可用的，返回展示字段。
	skills, err := conversation.LoadDraftSkills(ctx, request.ExecutorSettings, definitions)
	if err != nil {
		return nil, err
	}
	return newSkillInfos(skill.GetSkillsByNames(definitions.Agents[index].SkillNames, skills)), nil
}

// ListModels 返回公开模型目录和指定 Agent 的默认模型。
func (r *runtimeImpl) ListModels(_ context.Context, request *ListModelsRequest) (ModelList, error) {
	// 1. 读取当前定义，指定 Agent 时检查名称并填入默认模型。
	definitions := r.definitions.Load()
	models := definitions.conversationDefinitions.Models
	list := ModelList{Models: make([]*defs.ModelDefinition, 0, len(models))}
	if request.AgentName != "" {
		ref, found := definitions.agentDefaultModels[request.AgentName]
		if !found {
			return ModelList{}, fmt.Errorf("agent %q is not configured", request.AgentName)
		}
		list.AgentDefaultModel = ref
	}
	// 2. 复制各模型的公开字段并返回。
	for _, model := range models {
		list.Models = append(list.Models, model.ClonePublic())
	}
	return list, nil
}

// GetHooks 返回当前配置中的 Hook 说明。
func (r *runtimeImpl) GetHooks(_ context.Context) ([]*hook.HookInfo, error) {
	// 1. 读取当前定义并取出 Hook 信息。
	return r.definitions.Load().conversationDefinitions.Hooks.GetHookInfos(), nil
}

// ListAgents 按定义顺序返回 Agent 的公开说明。
func (r *runtimeImpl) ListAgents(context.Context) ([]AgentInfo, error) {
	// 1. 从当前定义提取 Agent 展示字段。
	return newAgentInfos(r.definitions.Load().conversationDefinitions.Agents), nil
}

// ListExecutors 按定义顺序返回 Executor 说明和默认工作目录。
func (r *runtimeImpl) ListExecutors(context.Context) ([]conversation.ExecutorDetails, error) {
	// 1. 从当前定义提取 Executor 展示字段。
	return newExecutorDetails(r.definitions.Load().conversationDefinitions.Executors), nil
}

// ResolveWorkspaceDirectory 使用当前 Executor 定义解析用户输入的工作目录。
func (r *runtimeImpl) ResolveWorkspaceDirectory(ctx context.Context, request *ResolveWorkspaceDirectoryRequest) (string, error) {
	// 1. 读取当前 Executor 定义，交给对话目录解析方法。
	executors := r.definitions.Load().conversationDefinitions.Executors
	return conversation.ResolveWorkspaceDirectory(ctx, executors, request.ExecutorID, request.Directory, request.BaseDirectory)
}

// OpenTerminalWorkspace 使用当前 Executor 定义为终端建立独立工作区。
func (r *runtimeImpl) OpenTerminalWorkspace(ctx context.Context, request *OpenTerminalWorkspaceRequest) (*TerminalWorkspace, error) {
	// 1. 读取当前 Executor 定义，ExecutorID 为空时用 local；未知 ID 返回 ErrInvalidExecutorSettings，virtual 返回 ErrTTYUnsupported。
	executors := r.definitions.Load().conversationDefinitions.Executors
	executorID := cmp.Or(request.ExecutorID, defs.LocalExecutorID)
	index := slices.IndexFunc(executors, func(definition *defs.ExecutorDefinition) bool { return definition.ID == executorID })
	if index < 0 {
		return nil, fmt.Errorf("%w: unknown executor %q", conversation.ErrInvalidExecutorSettings, executorID)
	}
	definition := executors[index]
	if definition.Kind == defs.ExecutorKindVirtual {
		return nil, workspace.ErrTTYUnsupported
	}
	// 2. 目录为空时用默认工作目录，否则在临时工作区上解析并检查它是目录。
	directory := definition.WorkspaceDirectory
	if request.Directory != "" {
		resolved, err := conversation.ResolveWorkspaceDirectory(ctx, executors, executorID, request.Directory, "")
		if err != nil {
			return nil, err
		}
		directory = resolved
	}
	// 3. 以起始目录为根创建这个终端独占的工作区。
	terminalWorkspace, err := conversation.NewExecutorWorkspace(ctx, definition, directory, request.Environment)
	if err != nil {
		return nil, err
	}
	return &TerminalWorkspace{
		Workspace:  terminalWorkspace,
		ExecutorID: executorID,
		Directory:  directory,
	}, nil
}

// GetWorkspaceDirectories 使用当前 Executor 定义查询工作区目录候选。
func (r *runtimeImpl) GetWorkspaceDirectories(ctx context.Context, request *GetWorkspaceDirectoriesRequest) (*conversation.WorkspaceDirectories, error) {
	// 1. 读取当前 Executor 定义，在本次临时工作区上列出目录。
	executors := r.definitions.Load().conversationDefinitions.Executors
	return conversation.GetWorkspaceDirectories(ctx, executors, request.ExecutorID, request.Directory, request.BaseDirectory)
}

// SetDefinitions 替换 Runtime 定义，并行重新装配空闲对话。
func (r *runtimeImpl) SetDefinitions(ctx context.Context, request *Definitions) (SetDefinitionsResult, error) {
	// 1. 检查至少有一个 Agent，整理新定义。
	if len(request.Agents) == 0 {
		return SetDefinitionsResult{}, errors.New("runtime requires at least one agent")
	}
	definitions := newRuntimeDefinitions(request)
	// 2. 经 Runtime 事件循环替换定义并取出运行对话。
	change := &definitionsChangeRequested{
		definitions: definitions,
		reply:       make(chan []*runtimeConversation, 1),
	}
	select {
	case r.requests <- change:
	case <-r.stopped:
		return SetDefinitionsResult{}, ErrRuntimeStopped
	case <-ctx.Done():
		return SetDefinitionsResult{}, ctx.Err()
	}
	// 3. 按对话 ID 排序；跳过已退出对话，有 Turn 或后台命令在跑的对话记入结果，其余并行更新，失败只记日志。
	runningConversations := <-change.reply
	slices.SortFunc(runningConversations, func(a, b *runtimeConversation) int {
		return cmp.Compare(a.conversation.GetID(), b.conversation.GetID())
	})
	var result SetDefinitionsResult
	var tasks sync.WaitGroup
	for _, running := range runningConversations {
		if running.isStopped() {
			continue
		}
		conversationID := running.conversation.GetID()
		if running.isBusy(ctx) {
			result.BusyConversationIDs = append(result.BusyConversationIDs, conversationID)
			continue
		}
		tasks.Go(func() {
			if err := running.conversation.SetDefinitions(ctx, definitions.conversationDefinitions); err != nil {
				logCtx := telemetry.ContextWithAttrs(ctx, telemetry.ConversationID(string(conversationID)))
				telemetry.GetLogger(logCtx).Warn("set conversation definitions failed", "error", err)
			}
		})
	}
	// 4. 等待所有更新任务结束，返回忙碌对话列表。
	tasks.Wait()
	return result, nil
}

// SetSessionModel 保存 Session 所选模型并更新运行信息。
func (r *runtimeImpl) SetSessionModel(ctx context.Context, request *SetSessionModelRequest) (defs.Session, error) {
	// 1. 取得运行实例并保存所选模型；没保存成功时直接返回错误。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return defs.Session{}, err
	}
	record, err := running.conversation.SetSessionModel(ctx, request.SessionID, request.Model)
	if record.ID == "" {
		return defs.Session{}, err
	}
	// 2. 记录已保存：请求对话更新 Session 运行信息，对话已经退出时忽略；通知 Session 失败的错误随记录一起返回。
	_ = running.sendRequest(ctx, &sessionRecordReplaceRequested{record: record})
	return record, err
}

// ListSessions 返回对话内的一页 Session 记录。
func (r *runtimeImpl) ListSessions(
	ctx context.Context,
	request *dao.ListSessionsRequest,
) (storage.Page[defs.Session, dao.RecordCursor[defs.SessionID]], error) {
	// 1. 选择对话存储并查询一页记录。
	query, _, _ := r.getConversationDAO(request.ConversationID)
	return query.ListSessions(ctx, *request)
}

// GetSessionByID 从共享库按 SessionID 读取记录；普通新对话的 Session 要等对话保存后才查得到，临时对话的 Session 查不到。
func (r *runtimeImpl) GetSessionByID(ctx context.Context, request *GetSessionByIDRequest) (defs.Session, error) {
	// 1. 查询共享库；未找到时返回 ErrSessionNotFound。
	record, found, err := r.query.GetSessionByID(ctx, request.SessionID)
	if err != nil {
		return defs.Session{}, err
	}
	if !found {
		return defs.Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, request.SessionID)
	}
	return record, nil
}

// ListTurns 按请求排序和游标分页返回一个 Session 的 Turn 记录。
func (r *runtimeImpl) ListTurns(
	ctx context.Context,
	request *ListTurnsRequest,
) (storage.Page[defs.Turn, dao.RecordCursor[defs.TurnID]], error) {
	// 1. 检查请求包含对话 ID。
	if request.ConversationID == "" {
		return storage.Page[defs.Turn, dao.RecordCursor[defs.TurnID]]{}, errors.New("list Turns requires a conversation ID")
	}
	// 2. 选择对话存储并按请求排序和游标查询。
	query, _, _ := r.getConversationDAO(request.ConversationID)
	return query.ListTurns(ctx, dao.ListTurnsRequest{
		SessionID: request.SessionID,
		Order:     request.Order,
		Cursor:    request.Cursor,
		Limit:     request.Limit,
	})
}

// GetTurns 按请求顺序返回找到的 Turn 记录。
func (r *runtimeImpl) GetTurns(ctx context.Context, request *GetTurnsRequest) ([]defs.Turn, error) {
	// 1. 检查请求包含对话 ID。
	if request.ConversationID == "" {
		return nil, errors.New("get Turns requires a conversation ID")
	}
	// 2. 选择对话存储并批量查询，跳过不存在的 Turn。
	query, _, _ := r.getConversationDAO(request.ConversationID)
	return query.GetTurns(ctx, request.SessionID, request.TurnIDs)
}

// ListItems 分页读取对话内一个 Session 的日志。
func (r *runtimeImpl) ListItems(ctx context.Context, request *ListItemsRequest) (storage.Page[dao.LoggedItem, storage.Ordinal], error) {
	// 1. 选择对话存储，确认 Session 属于对话。
	query, _, _ := r.getConversationDAO(request.ConversationID)
	if _, err := r.getSession(ctx, request.ConversationID, request.SessionID); err != nil {
		return storage.Page[dao.LoggedItem, storage.Ordinal]{}, err
	}
	// 2. 按方向、游标和条数读取日志。
	return query.ListItems(ctx, dao.ListItemsRequest{
		ConversationID: request.ConversationID,
		SessionID:      request.SessionID,
		Order:          request.Order,
		Cursor:         request.Cursor,
		Limit:          request.Limit,
	})
}

// IsIdle 所有已打开对话是否都没有运行中的 Turn 和后台命令？
func (r *runtimeImpl) IsIdle(ctx context.Context) (bool, error) {
	// 1. 经事件循环取得运行对话列表。
	request := &runningConversationsRequested{reply: make(chan []*runtimeConversation, 1)}
	select {
	case r.requests <- request:
	case <-r.stopped:
		return false, ErrRuntimeStopped
	case <-ctx.Done():
		return false, ctx.Err()
	}
	// 2. 逐个询问对话，任一对话忙碌时返回 false。
	for _, running := range <-request.reply {
		if running.isBusy(ctx) {
			return false, nil
		}
	}
	// 3. 检查查询期间上下文是否结束，再返回空闲。
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

// getRunningConversation 经事件循环查找尚未退出的对话实例。
func (r *runtimeImpl) getRunningConversation(ctx context.Context, id defs.ConversationID) (*runtimeConversation, error) {
	// 1. 提交查找请求，返回运行实例或错误。
	request := &runningConversationRequested{
		conversationID: id,
		reply:          make(chan conversationReply, 1),
	}
	result := r.requestConversation(ctx, request, request.reply)
	return result.runningConversation, result.err
}

// requestConversation 向 Runtime 事件循环提交请求并等待回复。
func (r *runtimeImpl) requestConversation(ctx context.Context, request runtimeRequest, reply chan conversationReply) conversationReply {
	// 1. 提交请求；Runtime 已停止或提交前上下文结束时返回对应错误。
	select {
	case r.requests <- request:
	case <-r.stopped:
		return conversationReply{err: ErrRuntimeStopped}
	case <-ctx.Done():
		return conversationReply{err: ctx.Err()}
	}
	// 2. 等待事件循环回复。
	return <-reply
}

// handleRunningConversationRequested 查找已打开的 Conversation，并把查询结果发给请求方。
func (r *runtimeImpl) handleRunningConversationRequested(request *runningConversationRequested) {
	// 1. 查运行表并检查实例是否退出，未打开或已退出时回复 ErrConversationNotOpen。
	if running := r.runningConversations[request.conversationID]; running != nil && !running.isStopped() {
		request.reply <- conversationReply{runningConversation: running}
		return
	}
	request.reply <- conversationReply{err: ErrConversationNotOpen}
}

// openConversation 按记录和当前定义装配对话，并登记运行实例。
func (r *runtimeImpl) openConversation(ctx context.Context, record defs.Conversation, storedSettings defs.ExecutorSettings,
	environment []string, isSettingsChanged bool) conversationReply {
	// 1. 读取当前定义，模型目录为空时返回 ErrNoModelConfigured。
	definitions := r.definitions.Load()
	if len(definitions.conversationDefinitions.Models) == 0 {
		return conversationReply{err: ErrNoModelConfigured}
	}
	// 2. 选择对话存储，按记录设置、原存储设置和客户端环境装配实例。
	query, mutation, unsavedStore := r.getConversationDAO(record.ID)
	opt := &conversation.ConversationOpt{
		Query:         query,
		Mutation:      mutation,
		Definitions:   definitions.conversationDefinitions,
		Environment:   environment,
		MaxImageBytes: r.opt.MaxImageBytes,
	}
	instance, err := conversation.GetConversation(ctx, record, storedSettings, opt)
	if err != nil {
		return conversationReply{err: err}
	}
	// 3. 设置有变更时取出补齐后的设置，随实例交给事件循环保存并启动。
	var executorSettings *defs.ExecutorSettings
	if isSettingsChanged {
		settings := instance.GetExecutorSettings()
		executorSettings = &settings
	}
	return r.startConversation(ctx, instance, unsavedStore, executorSettings)
}

// startConversation 登记并启动已装配实例。
func (r *runtimeImpl) startConversation(ctx context.Context, instance conversation.Conversation, unsavedStore *unsavedConversationStore,
	executorSettings *defs.ExecutorSettings) conversationReply {
	// 1. 提交实例、内存库和待保存的 Executor 设置。
	request := &conversationStartRequested{
		instance:         instance,
		unsavedStore:     unsavedStore,
		executorSettings: executorSettings,
		reply:            make(chan conversationReply, 1),
	}
	// 2. 等待登记结果；失败时用已取消上下文运行实例，关闭它创建的工作区。
	result := r.requestConversation(ctx, request, request.reply)
	if result.err != nil {
		canceledCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		cancel()
		_ = instance.Run(canceledCtx, nil)
	}
	return result
}

// handleConversationStartRequested 登记对话记录和存储，启动实例并回复。
func (r *runtimeImpl) handleConversationStartRequested(ctx context.Context, request *conversationStartRequested) {
	// 1. 同一对话已有尚未退出实例时回复该实例和 errConversationAlreadyOpen。
	conversationID := request.instance.GetID()
	if registered := r.runningConversations[conversationID]; registered != nil && !registered.isStopped() {
		request.reply <- conversationReply{
			runningConversation: registered,
			err:                 errConversationAlreadyOpen,
		}
		return
	}
	// 2. 从内存库或共享库读取最新对话记录，不存在时返回 ErrConversationNotFound。
	query, mutation := r.query, r.mutation
	if request.unsavedStore != nil {
		query, mutation = request.unsavedStore.query, request.unsavedStore.mutation
	}
	record, found, err := query.GetConversation(ctx, conversationID)
	if err != nil {
		request.reply <- conversationReply{err: fmt.Errorf("get Conversation %s record: %w", conversationID, err)}
		return
	}
	if !found {
		request.reply <- conversationReply{err: fmt.Errorf("%w: %s", ErrConversationNotFound, conversationID)}
		return
	}
	// 3. 创建对话运行记录，有待更新的 Executor 设置时先保存。
	runCtx, cancel := context.WithCancel(ctx)
	running := newRuntimeConversation(
		request.instance, cancel, record, query, mutation, r.definitions.Load(), request.unsavedStore, r.opt.UnsavedConversationIdleTimeout, r.runningChanged,
	)
	if request.executorSettings != nil {
		record.ExecutorSettings = *request.executorSettings
		if err := running.saveRecord(ctx, record); err != nil {
			cancel()
			request.reply <- conversationReply{err: fmt.Errorf("set Conversation %s executor settings: %w", conversationID, err)}
			return
		}
	}
	// 4. 登记内存库和运行实例，启动对话任务；任务退出后通知 Runtime 移除实例。
	if request.unsavedStore != nil {
		r.unsavedConversationStores.Store(conversationID, request.unsavedStore)
	}
	r.runningConversations[conversationID] = running
	signalRunningConversationsChanged(r.runningChanged)
	telemetry.AddActiveConversations(ctx, 1)
	r.tasks.Go(func() {
		defer telemetry.AddActiveConversations(ctx, -1)
		running.Run(runCtx)
		cancel()
		select {
		case r.requests <- &conversationStopped{runningConversation: running}:
		case <-ctx.Done():
		}
	})
	// 5. 回复登记后的记录，标题按长度上限截取。
	record = running.record
	record.Title = defs.TruncateConversationTitle(record.Title)
	request.reply <- conversationReply{
		runningConversation: running,
		record:              record,
	}
}

// openUnsavedConversationStore 打开新对话的内存库并创建 DAO。
func (r *runtimeImpl) openUnsavedConversationStore(isEphemeral bool) (*unsavedConversationStore, error) {
	// 1. 打开独立 sqlite 内存库。
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewMemoryDatabaseOpt(dao.RecordNamespaces()...))
	if err != nil {
		return nil, fmt.Errorf("open unsaved conversation database: %w", err)
	}
	// 2. 临时对话全部读写内存库；普通对话暂存 metadata、Session 和对话索引，其余直接读写共享库。
	store := &unsavedConversationStore{database: database}
	var records storage.RecordStorage = database.Record()
	var logs storage.LogStorage = database.Log()
	if !isEphemeral {
		store.stagedLogs = storage.NewStagedLogStorage(database.Log(), r.opt.Logs, dao.MetadataStreamID)
		store.stagedRecords = storage.NewStagedRecordStorage(database.Record(), r.opt.Records, dao.SessionNamespace, dao.ConversationNamespace)
		logs, records = store.stagedLogs, store.stagedRecords
	}
	// 3. 创建 DAO，失败时关闭内存库并合并错误。
	if store.query, store.mutation, err = dao.NewDAO(dao.NewDAOOpt(records, logs)); err != nil {
		return nil, errors.Join(fmt.Errorf("create unsaved conversation DAO: %w", err), database.Close())
	}
	return store, nil
}

// handleDefinitionsChangeRequested 替换当前定义并回复运行对话列表。
func (r *runtimeImpl) handleDefinitionsChangeRequested(request *definitionsChangeRequested) {
	// 1. 保存新定义，复制运行表中的实例供调用方更新。
	r.definitions.Store(request.definitions)
	request.reply <- slices.Collect(maps.Values(r.runningConversations))
}

// handleConversationStopped 移除已退出实例，丢弃它仍登记的内存库。
func (r *runtimeImpl) handleConversationStopped(ctx context.Context, request *conversationStopped) {
	// 1. 比较退出实例与运行表；对话已重新打开时保留新实例。
	stopped := request.runningConversation
	id := stopped.conversation.GetID()
	if r.runningConversations[id] != stopped {
		return
	}
	// 2. 移除旧实例并关闭仍登记的内存库。
	delete(r.runningConversations, id)
	signalRunningConversationsChanged(r.runningChanged)
	r.discardUnsavedConversationStore(ctx, id)
}

// saveConversation 将普通新对话保存到共享库。
func (r *runtimeImpl) saveConversation(ctx context.Context, conversationID defs.ConversationID) error {
	// 1. 已保存或临时对话直接返回。
	store, found := r.unsavedConversationStores.Load(conversationID)
	if !found || store.stagedLogs == nil {
		return nil
	}
	// 2. 不随请求取消，先搬 metadata，再按 Session、对话顺序搬索引；失败时保留内存库供下次重试。
	ctx = context.WithoutCancel(ctx)
	if err := store.stagedLogs.Persist(ctx); err != nil {
		return fmt.Errorf("save Conversation %s: %w", conversationID, err)
	}
	if err := store.stagedRecords.Persist(ctx); err != nil {
		return fmt.Errorf("save Conversation %s: %w", conversationID, err)
	}
	// 3. 移除登记并关闭内存库，同时到达的调用仅移除一次。
	r.discardUnsavedConversationStore(ctx, conversationID)
	return nil
}

// discardUnsavedConversationStore 丢弃对话的内存库。
func (r *runtimeImpl) discardUnsavedConversationStore(ctx context.Context, conversationID defs.ConversationID) {
	// 1. 取出并移除登记，不存在时返回。
	store, found := r.unsavedConversationStores.LoadAndDelete(conversationID)
	if !found {
		return
	}
	// 2. 关闭内存库，失败只记 Warn。
	if err := store.database.Close(); err != nil {
		logCtx := telemetry.ContextWithAttrs(ctx, telemetry.ConversationID(string(conversationID)))
		telemetry.GetLogger(logCtx).Warn("close unsaved conversation database failed", "error", err)
	}
}

// handleRunningConversationsRequested 回复运行中或正在退出的对话列表。
func (r *runtimeImpl) handleRunningConversationsRequested(request *runningConversationsRequested) {
	// 1. 复制运行表的实例列表并回复。
	request.reply <- slices.Collect(maps.Values(r.runningConversations))
}

// getConversationDAO 返回对话使用的 DAO 及仍登记的内存库。
func (r *runtimeImpl) getConversationDAO(conversationID defs.ConversationID) (dao.Query, dao.Mutation, *unsavedConversationStore) {
	// 1. 有内存库登记时返回独立 DAO，否则返回共享 DAO 和空内存库。
	if store, found := r.unsavedConversationStores.Load(conversationID); found {
		return store.query, store.mutation, store
	}
	return r.query, r.mutation, nil
}

// getSavedConversation 从共享库读取已保存对话。
func (r *runtimeImpl) getSavedConversation(ctx context.Context, conversationID defs.ConversationID) (defs.Conversation, error) {
	// 1. 查询共享库，临时、未保存或不存在的对话返回 ErrConversationNotFound。
	record, found, err := r.query.GetConversation(ctx, conversationID)
	if err != nil {
		return defs.Conversation{}, err
	}
	if !found {
		return defs.Conversation{}, fmt.Errorf("%w: %s", ErrConversationNotFound, conversationID)
	}
	return record, nil
}

// getSession 读取属于指定对话的 Session 记录。
func (r *runtimeImpl) getSession(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID) (defs.Session, error) {
	// 1. 选择对话存储并查询 Session；不属于该对话时返回 ErrSessionNotFound。
	query, _, _ := r.getConversationDAO(conversationID)
	record, found, err := query.GetSession(ctx, conversationID, sessionID)
	if err != nil {
		return defs.Session{}, err
	}
	if !found {
		return defs.Session{}, fmt.Errorf("%w: Session %s in Conversation %s", ErrSessionNotFound, sessionID, conversationID)
	}
	return record, nil
}

// SteerInput 将排队用户输入移入指定轮。
func (r *runtimeImpl) SteerInput(ctx context.Context, request *SteerInputRequest) error {
	// 1. 取得运行对话。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return err
	}
	// 2. 将操作交给对话根 Session。
	return running.conversation.SteerInput(ctx, request.ItemID, request.TurnID)
}

// InterruptTurnWithInput 置顶排队用户输入并取消指定轮。
func (r *runtimeImpl) InterruptTurnWithInput(ctx context.Context, request *InterruptTurnWithInputRequest) error {
	// 1. 取得运行对话。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return err
	}
	// 2. 将操作交给对话根 Session。
	return running.conversation.InterruptTurnWithInput(ctx, request.TurnID, request.ItemID)
}

// WithdrawInput 取走完整待处理用户输入组。
func (r *runtimeImpl) WithdrawInput(ctx context.Context, request *WithdrawInputRequest) ([]*defs.Item, error) {
	// 1. 取得运行对话。
	running, err := r.getRunningConversation(ctx, request.ConversationID)
	if err != nil {
		return nil, err
	}
	// 2. 将操作交给对话根 Session。
	return running.conversation.WithdrawInput(ctx, request.ItemID)
}
