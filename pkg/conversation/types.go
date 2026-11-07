package conversation

import (
	"context"
	"errors"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ── Conversation 分叉错误 ──

var (
	// ErrForkBoundaryConflict 是 BeforeTurnID 和 LastTurnID 同时给了时 ForkConversation 返回的错误。
	ErrForkBoundaryConflict = errors.New("fork accepts at most one of before turn and last turn")
	// ErrForkTurnNotFound 是指定的轮次不在来源根 Session 里时 ForkConversation 返回的错误。
	ErrForkTurnNotFound = errors.New("fork turn was not found in the root session")
	// ErrNothingToFork 是截取点之前没有可复制的轮次时 ForkConversation 返回的错误。
	ErrNothingToFork = errors.New("nothing to fork before the requested turn")
)

// ── Conversation 配置错误 ──

// ErrInvalidExecutorSettings 是 Executor 设置或目录不合法时返回的错误：设置里新加了配置中没有的 Executor、
// 默认 Executor 不在 executor_ids 里、解析出的目录不存在或不是目录、要给 virtual Executor 换目录。
var ErrInvalidExecutorSettings = errors.New("invalid executor settings")

// ── Conversation 换配置错误 ──

var (
	// errConversationReconfiguring 是换配置时 Run 取消旧配置下 Session 共用 ctx 的原因；根 Session 因它退出时，对话不停止。
	errConversationReconfiguring = errors.New("conversation is reconfiguring")
	// errConfigChanged 是装配新配置期间对话已经换过一套配置时 Run 的回复；调用方收到后按最新的配置重新装配。
	errConfigChanged = errors.New("conversation config changed while the new one was assembled")
)

// ── Conversation 配置参数 ──

// ConversationDefinitions 保存本对话可用的 Agent、模型、Executor、MCP、hook 和共享 Skill 定义。
type ConversationDefinitions struct {
	// Agents 是本对话可使用的全部 Agent 定义；每个 Session 按持久化的 AgentName 选择其中一项。
	Agents []*agent.AgentDefinition
	// Models 是模型目录；各 Agent 的默认模型和摘要模型必须在其中。
	Models []*defs.ModelDefinition
	// MCPServers 是全部 MCP server 定义；对话只给本对话 Agent 会用到的建连接。
	MCPServers []*mcpclient.ServerDefinition
	// Hooks 是本对话使用的 hook，nil 表示没有 hook。
	Hooks hook.Hooks
	// Executors 决定本对话能用哪些 Executor，不能为 nil，由 Runtime 保证；对话按自己的 Executor 设置从中选出可用的建执行环境。
	Executors executor.ExecutorProvider
	// Skills 是所有对话共用的 skill；和项目目录、主目录里的 skill 同名时，项目目录的优先，其次是这里的，主目录的最后。
	Skills []*skill.Skill
}

// ClientMCPServer 是客户端登记的一个 MCP server；客户端 MCP 一律是弱依赖，超时取 mcpclient 的默认值。
type ClientMCPServer struct {
	// ID 是这个 MCP server 的 ID，工具名里也用它。
	ID string
	// Transport 把请求转发给客户端那边的 MCP server。
	Transport mcp.Transport
}

// ── Conversation 输入参数 ──

// ConversationInput 是应用交给根 Session 的一次输入。
type ConversationInput struct {
	// Mode 是提交方式，含义见 session.SessionInputMode。
	Mode session.SessionInputMode
	// From 是发送方，必须是用户或 Agent。
	From *defs.Address
	// Content 是消息内容，HandleInput 会复制一份。
	Content []*defs.Content
	// InputID 可选：根 Session 本次运行期内重复的 InputID 只接收一次。
	InputID defs.InputID
}

// ── Conversation 对外事件 ──

// ConversationEvent 是 Conversation.Run 发给调用方的事件，覆盖对话内全部 Session。
type ConversationEvent interface{ isConversationEvent() }

// ConversationSessionStarted 由 Conversation 在启动一个 Session 实例前发出：Run 启动根 Session、换配置后重启根 Session、
// spawn_agent 创建子 Session，或向已停止的 Session 投递消息时重启它。同一 Session 的其他事件都在它之后。
type ConversationSessionStarted struct {
	Session defs.Session
	// LastSavedOrdinal 是启动前读到的这个 Session 日志的最新序号，之后写入的 Item 都以 SessionItemsSaved 到达。
	LastSavedOrdinal storage.Ordinal
}

func (*ConversationSessionStarted) isConversationEvent() {}

// ConversationSessionEvent 由 Conversation 在读取某个 Session 的事件流时逐条转发。
type ConversationSessionEvent struct {
	SessionID defs.SessionID
	Event     session.SessionEvent
}

func (*ConversationSessionEvent) isConversationEvent() {}

// ConversationSessionStopped 由 Conversation 在某个 Session.Run 返回、它的事件全部转发之后发出。
type ConversationSessionStopped struct {
	SessionID defs.SessionID
	// Err 是 Session.Run 的返回值。
	Err error
}

func (*ConversationSessionStopped) isConversationEvent() {}

// ConversationReconfigured 由 Conversation 在替换设置或定义、重启根 Session 成功后发出。
type ConversationReconfigured struct {
	// Models 是新配置的模型目录。
	Models map[defs.ModelRef]*defs.ModelDefinition
	// AgentDefaultModels 以 Agent 名为键，值是这个 Agent 的默认模型。
	AgentDefaultModels map[string]defs.ModelRef
	// ExecutorSettings 是新配置的 Executor 设置，已补上 local 目录。
	ExecutorSettings defs.ExecutorSettings
}

func (*ConversationReconfigured) isConversationEvent() {}

// ── Conversation 运行状态 ──

// conversationConfig 保存一套对话设置、定义、执行环境和 Skill，替换配置时整份换掉。
// Run 在启动这套配置的 Session 前填写 sessionsCtx、stopSessions，之后由 Run 和 Session 任务读取。
type conversationConfig struct {
	// settings 是这套配置的 Executor 设置，已补上 local 目录；换定义时沿用。
	settings defs.ExecutorSettings
	// definitions 是这套配置的定义；改设置时沿用。
	definitions *ConversationDefinitions
	// agentDefinitions 是按名称索引的 Agent 目录，这套配置下的全部 Session 共用。
	agentDefinitions map[string]*agent.AgentDefinition
	// models 是按引用索引的模型目录，这套配置下的全部 Session 共用。
	models map[defs.ModelRef]*defs.ModelDefinition
	// environments 是按 settings 建出的执行环境，第一个为默认，这套配置下的全部 Session 共用；
	// 它们的 MCP 连接由 Run 在换上这套配置时启动，换走或 Run 返回前停止。
	environments []executor.ExecutionEnvironment
	// executors 是装配时 GetExecutors 返回的列表，Session 详情从这里取种类。
	executors []executor.Executor
	// workspaces 是这套配置建的全部工作区，只属于本对话；停掉这套执行环境后关闭，没换上就被丢弃的配置由丢弃方关闭。
	workspaces []workspace.Workspace
	// skills 是项目目录、definitions.Skills、主目录三段按顺序去重后的 skill，按名字排序；各 Agent 按 SkillNames 选择。
	skills []*skill.Skill
	// sessionsCtx 是这套配置下全部 Session 共用的 ctx，派生自 Run 的 ctx。
	sessionsCtx context.Context
	// stopSessions 取消 sessionsCtx；换配置时以 errConversationReconfiguring 为原因调用。
	stopSessions context.CancelCauseFunc
}

// ── Conversation Run 请求 ──

// configChange 由 SetExecutorSettings 或 SetDefinitions 在新配置装配完成后交给 Run。
type configChange struct {
	// base 是装配新配置时读到的当前配置；Run 收到时当前配置已经不是它，就回复 errConfigChanged 并关闭 next 的工作区。
	base *conversationConfig
	// next 是装配好的新配置。
	next *conversationConfig
	// reply 收 Run 的替换结果。
	reply chan error
}

// clientMCPChange 由 SetClientMCPServers 在新连接创建后交给 Run，替换 clientID 登记的全部连接。
type clientMCPChange struct {
	clientID string
	// connections 是新连接；为空时移除这个客户端的登记。
	connections []mcpclient.Connection
	// applied 在 Run 换完登记后关闭。
	applied chan struct{}
}

// ── 协作工具参数 ──

// SpawnAgentParam 是 spawn_agent 的模型参数，创建一个直接子 Agent 并提交首个任务。
type SpawnAgentParam struct {
	TaskName        string `json:"task_name" jsonschema:"description=用于识别子任务的简短名称,minLength=1"`
	Message         string `json:"message" jsonschema:"description=子任务目标、必要背景和预期交付物；父方活动 Turn 不会被继承，所需材料应写在这里,minLength=1"`
	ForkTurns       string `json:"fork_turns,omitempty" jsonschema:"description=继承已结束历史的范围：none 不继承；all 继承有效历史；正整数字符串最多选择最近 n 个 Turn。达到 n 后停止；先遇摘要则带上整份摘要并停止,default=none"`
	AgentName       string `json:"agent_name,omitempty" jsonschema:"description=子 Agent 使用的 Agent 名称，可选值见工具说明；省略时与当前 Agent 相同"`
	Model           string `json:"model,omitempty" jsonschema:"description=子 Agent 使用的模型，格式 provider_id/model_id，可选值见工具说明；省略时同一 Agent 继承你当前的模型，不同 Agent 使用它的默认模型"`
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=推理强度，必须是所选模型支持的取值；省略时沿用继承的强度或模型默认值"`
}

// SendMessageParam 是 send_message 的模型参数，向后代或直接父方提交一次输入。
type SendMessageParam struct {
	SessionID defs.SessionID           `json:"session_id" jsonschema:"description=接收方 SessionID；仅允许自己的后代或直接父方,minLength=1"`
	Message   string                   `json:"message" jsonschema:"description=发送给接收方的任务、补充要求或信息,minLength=1"`
	Mode      session.SessionInputMode `json:"mode,omitempty" jsonschema:"description=normal 安排新 Turn，忙碌时排队；steer 补充活动 Turn，在模型边界读入，无可接收 Turn 时自动按 normal 安排,enum=normal,enum=steer,default=normal"`
	// Model 和 ReasoningEffort 只能用于后代；任一非空时先替换接收方的模型选择，再提交输入。
	Model           string `json:"model,omitempty" jsonschema:"description=只能给后代用：接收方改用的模型，格式 provider_id/model_id，可选值见 spawn_agent 的工具说明；省略时沿用它当前的模型"`
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=只能给后代用：推理强度，必须是接收方所用模型支持的取值；省略时沿用它当前的强度或模型默认值"`
}

// WaitAgentParam 是 wait_agent 的模型参数，等待当前 Turn 收到新的补充输入。
type WaitAgentParam struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"description=等待秒数；省略或 0 为 30，有效范围 1–600。被 steer 消息或子任务结果通知唤醒，超时也会返回；normal 消息不会唤醒本次等待，不会取消子任务,minimum=0,maximum=600,default=30"`
}

// interruptAgentParam 是 interrupt_agent 的模型参数，指定要取消哪个后代 Session 的活动 Turn。
type interruptAgentParam struct {
	SessionID defs.SessionID `json:"session_id" jsonschema:"description=要中断当前 Turn 的后代 SessionID,minLength=1"`
}

// listAgentsParam 是 list_agents 的模型参数，没有字段。
type listAgentsParam struct{}

// SessionHistoryParam 是 read_session_history 的模型参数，从新到旧分页读取后代 fork 之后的历史。
type SessionHistoryParam struct {
	SessionID defs.SessionID  `json:"session_id" jsonschema:"description=要读取本地执行历史的后代 SessionID,minLength=1"`
	Cursor    storage.Ordinal `json:"cursor,omitempty" jsonschema:"description=省略或 0 从最新历史开始；读取更早一页使用返回的非零 next_cursor，不能越过 fork 分界,minimum=0"`
	Limit     int             `json:"limit,omitempty" jsonschema:"description=每页最多读取的 Item 数量；默认 20，范围 1–100,minimum=1,maximum=100,default=20"`
}

// ── 协作工具唤醒原因 ──

// wait_agent 的唤醒原因：同时收到几种消息时按 user_steer、subagent_result、agent_message 的顺序取第一个；
// 等到超时也没有消息时为 timeout。
const (
	waitReasonUserSteer      = "user_steer"
	waitReasonSubagentResult = "subagent_result"
	waitReasonAgentMessage   = "agent_message"
	waitReasonTimeout        = "timeout"
)

// ── 协作工具结果 ──

// spawnAgentResult 是 spawn_agent 的结果：Status 为 accepted 表示子 Session 已创建、首个任务已放进它的收件箱，子任务还没做完；
// Session 已保存但首个任务没被接收时，工具返回错误，SessionID 写在错误消息里。
type spawnAgentResult struct {
	SessionID defs.SessionID `json:"session_id"`
	TaskName  string         `json:"task_name"`
	Status    string         `json:"status"`
}

// sendMessageResult 是 send_message 的结果：Status 为 accepted 表示消息已放进目标收件箱，Mode 原样返回调用参数里的提交方式。
type sendMessageResult struct {
	SessionID defs.SessionID           `json:"session_id"`
	Mode      session.SessionInputMode `json:"mode"`
	Status    string                   `json:"status"`
}

// waitAgentResult 只说明唤醒原因与 Agent 发送方；消息正文在下一次模型请求前经收件箱读入。
type waitAgentResult struct {
	Reason string           `json:"reason"`
	From   []defs.SessionID `json:"from,omitempty"`
}

// interruptAgentResult 是 interrupt_agent 的结果：Status 为 cancel_requested 时 TurnID 是被请求取消的活动 Turn，
// 为 no_active_turn 时目标未运行或空闲；不等待取消完成。
type interruptAgentResult struct {
	SessionID defs.SessionID `json:"session_id"`
	TurnID    defs.TurnID    `json:"turn_id,omitempty"`
	Status    string         `json:"status"`
}

// SessionInfo 保存一个 Session 的记录、查询时的状态、已保存用量和所选模型窗口。
type SessionInfo struct {
	Session defs.Session `json:"session"`
	// Status 是查询时的状态；Session 没在运行时取最近保存的 Turn。
	Status session.SessionStatus `json:"status"`
	// Usage 是这个 Session 已保存的全部 Turn 的用量之和，不含正在运行的 Turn。
	Usage defs.TokenUsage `json:"usage"`
	// ContextWindow 是这个 Session 所选模型的上下文窗口，模型不在目录里时为 0。
	ContextWindow int `json:"context_window"`
}

// listAgentsResult 是 list_agents 的结果：来源 Session 的全部后代，按 SessionID 排序。
type listAgentsResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// sessionHistoryResult 是 read_session_history 返回的一页历史。
type sessionHistoryResult struct {
	SessionID defs.SessionID `json:"session_id"`
	// Items 按时间正序，只含 fork 记录之后这个 Session 自己的历史。
	Items []*defs.Item `json:"items"`
	// NextCursor 非零时用来读更早一页；为 0 表示已经读到 fork 记录，没有更早的本地历史。
	NextCursor storage.Ordinal `json:"next_cursor"`
}

// ── Conversation 查询结果 ──

// WorkspaceDirectories 是 Executor 上解析后的目录及其普通直接子目录名称。
type WorkspaceDirectories struct {
	Directory   string   `json:"directory"`
	Directories []string `json:"directories"`
}

// SessionDetails 保存对话里一个 Session 的记录、模型、Executor、Skill 和工具信息。
type SessionDetails struct {
	ConversationID defs.ConversationID `json:"conversation_id"`
	// Title 由 Runtime 按已保存的对话记录填写，GetSessionDetails 留空。
	Title   string       `json:"title"`
	Session defs.Session `json:"session"`
	// Model 是 Session 使用的模型定义（不含连接参数）；所选模型不在模型目录里时为空。
	Model *defs.ModelDefinition `json:"model"`
	// CompactModel 是压缩使用的模型，Agent 没有单独配置时与 Model 相同；Model 为空时它也为空。
	CompactModel *defs.ModelDefinition `json:"compact_model"`
	// Executors 按配置顺序，第一个是默认 Executor。
	Executors []ExecutorDetails `json:"executors"`
	// Skills 是这个 Agent 可用的 Skill 名。
	Skills []string `json:"skills"`
	// Tools 是模型可见的工具名，包括协作工具。
	Tools []string `json:"tools"`
}

// ExecutorDetails 是一个 Executor 的展示字段：ID、种类、说明和工作区根目录。
type ExecutorDetails struct {
	ID string `json:"id"`
	// Kind 是 Executor 的种类；界面选默认 Executor 时用它去掉 memory。
	Kind               defs.ExecutorKind `json:"kind"`
	Description        string            `json:"description,omitempty"`
	WorkspaceDirectory string            `json:"workspace_directory,omitempty"`
}

// MCPServerInfo 是对话里一个执行环境上的一个 MCP 连接；客户端登记的 ExecutorID 为 defs.ClientExecutorID。
type MCPServerInfo struct {
	ExecutorID string `json:"executor_id"`
	mcpclient.ConnectionInfo
}
