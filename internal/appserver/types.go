package appserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// ── RuntimeServerLocator 结果 ──

// RuntimeServerInfo 是连接 serve 要用的 OpenVaala 协议地址和 token，以及地址的来源和本机 server_id。
type RuntimeServerInfo struct {
	URL   string
	Token string // 为空时沿用 URL 里的 ?token=
	// Source 是地址的来源：--url、client.url 或 local，写进连接日志。
	Source string
	// LocalServerID 是本机 runtime-server.yaml 里的 server_id，文件不存在或读不出来时为空；
	// 连上后和 serve 返回的 server_id 比较，判断连的是不是本机 serve。
	LocalServerID string
}

// ── Codex 协议消息 ──

// maxMessageBytes 是单条 Codex 消息的上限，和 serve 的单帧上限一致；超过时回 -32600，连接继续。
const maxMessageBytes = 16 << 20

// errMessageTooLarge 由传输在丢弃一条超过 maxMessageBytes 的消息后返回，之后可以继续读。
var errMessageTooLarge = errors.New("codex message exceeds the 16 MiB limit")

// Codex 协议使用的 JSON-RPC 错误码：请求不合法（含未握手、重复握手、Session 忙）、方法不存在、参数错误、内部错误。
const (
	errorCodeInvalidRequest = -32600
	errorCodeMethodNotFound = -32601
	errorCodeInvalidParams  = -32602
	errorCodeInternal       = -32603
)

// codexMessage 是一条 Codex JSON-RPC 消息：Codex 线上不带 jsonrpc 字段；ID 是字符串或整数，原样保留并在响应里回送。
// 请求带 ID 和 Method，通知只带 Method，响应带 ID 和 Result 或 Error。
type codexMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result any             `json:"result,omitempty"`
	Error  *codexError     `json:"error,omitempty"`
}

// codexError 是返回 Codex 客户端的协议错误。
type codexError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error 返回 Codex 错误文本。
func (e *codexError) Error() string {
	// 1. 返回错误消息。
	return e.Message
}

// MarshalJSON 将 Codex 错误编码为 JSON。
func (e *codexError) MarshalJSON() ([]byte, error) {
	// 1. 填写 code 和 message，方法不存在时附带 status:404，再编码为 JSON。
	wire := struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  int    `json:"status,omitempty"`
	}{
		Code:    e.Code,
		Message: e.Message,
	}
	if e.Code == errorCodeMethodNotFound {
		wire.Status = http.StatusNotFound
	}
	return json.Marshal(wire)
}

// codexResponseHandedOver 是请求处理函数的特殊结果：响应已经由处理函数自己按顺序发出（thread/start 要先回响应再发 thread/started），
// 或交给了 connectionThread 在合适的时机发出（turn/start 等 TurnID），主循环不再回复。
type codexResponseHandedOver struct{}

// ── appServerConnection 事件 ──

// codexReceived 由连接的读任务在读到一条 JSON 消息后交给主循环；消息超过 16 MiB 时只带错误，主循环据此回 -32600，连接继续。
// 不是合法 JSON 的消息由读任务记 Warn 后丢弃，不交给主循环。
type codexReceived struct {
	message codexMessage
	err     *codexError
}

// ── Codex 请求参数 ──

// codexConfigReadParams 是 config/read 里 app-server 认的参数：Cwd 是客户端要确认信任状态的项目目录。
type codexConfigReadParams struct {
	Cwd string `json:"cwd"`
}

// codexConfigBatchWriteParams 是 config/batchWrite 里 app-server 认的参数：只看每项改动的 KeyPath 和值，合并方式、文件路径和版本都不用。
type codexConfigBatchWriteParams struct {
	Edits []codexConfigEdit `json:"edits"`
}

// codexConfigEdit 是 config/batchWrite 的一项改动；KeyPath 是 Codex config.toml 里的点分路径，例如 model；
// Value 是新值，类型随配置项而定，model 和 model_reasoning_effort 是字符串，null 表示清掉。
type codexConfigEdit struct {
	KeyPath string `json:"keyPath"`
	Value   any    `json:"value"`
}

// codexSkillsListParams 是 skills/list 的参数；只用 Cwds 的第一个目录。
type codexSkillsListParams struct {
	Cwds []string `json:"cwds"`
}

// codexHooksListParams 是 hooks/list 里 app-server 认的参数：Cwds 为空时用连接新建 Thread 默认的目录。
type codexHooksListParams struct {
	Cwds []string `json:"cwds"`
}

// codexMcpServerStatusListParams 是 mcpServerStatus/list 里 app-server 认的参数：ThreadID 非空且本连接跟随着它时，列它所在对话的 MCP 连接；
// cursor、limit、detail 忽略，一页返回全部。
type codexMcpServerStatusListParams struct {
	ThreadID string `json:"threadId"`
}

// codexThreadStartParams 是 thread/start 里 app-server 认的参数；审批、沙箱等其余参数忽略。
type codexThreadStartParams struct {
	// Cwd 是客户端路径，换成 serve 路径后作为新对话的 local 目录；临时对话只把它写进回给客户端的 Thread。
	Cwd string `json:"cwd"`
	// Model 是 provider_id/model_id，和 Config 里的推理强度一起用。
	Model  string                 `json:"model"`
	Config codexThreadStartConfig `json:"config"`
	// DynamicTools 非空时拒绝。
	DynamicTools []json.RawMessage `json:"dynamicTools"`
	// Ephemeral 是否新建临时对话？临时对话没有 Executor。
	Ephemeral bool `json:"ephemeral"`
	// ThreadSource 只在临时对话时原样写进回给客户端的 Thread。
	ThreadSource *string `json:"threadSource"`
}

// codexThreadStartConfig 是 thread/start、thread/fork 对新 Thread 的配置覆盖，只认推理强度：Codex app 把 composer 里选的强度放在这里。
type codexThreadStartConfig struct {
	ModelReasoningEffort string `json:"model_reasoning_effort"`
}

// codexThreadResumeParams 是 thread/resume 里 app-server 认的参数；model 等其余参数忽略。
type codexThreadResumeParams struct {
	ThreadID string `json:"threadId"`
	// ExcludeTurns 客户端是不是自己翻页读历史？是时响应的 thread.turns 为空。
	ExcludeTurns bool `json:"excludeTurns"`
	// Cwd 不交给 serve，对话按记录的目录打开；收到盘符路径时记下客户端是 Windows。
	Cwd string `json:"cwd"`
}

// codexThreadForkParams 是 thread/fork 里 app-server 认的参数；其余参数忽略。
type codexThreadForkParams struct {
	// ThreadID 必须是根 Thread。
	ThreadID string `json:"threadId"`
	// LastTurnID 复制到这一轮为止；它和 BeforeTurnID 最多给一个，都不给时复制全部已结束的轮次。
	LastTurnID string `json:"lastTurnId"`
	// BeforeTurnID 复制到这一轮之前。
	BeforeTurnID string `json:"beforeTurnId"`
	// Cwd、Model、Config 的处理同 thread/start，不给或用不上时照抄来源对话。
	Cwd    string                 `json:"cwd"`
	Model  string                 `json:"model"`
	Config codexThreadStartConfig `json:"config"`
	// ExcludeTurns 客户端是不是自己翻页读历史？是时响应的 thread.turns 为空。
	ExcludeTurns bool `json:"excludeTurns"`
	// Ephemeral 是不是要分叉成临时对话？不支持，为 true 时拒绝。
	Ephemeral bool `json:"ephemeral"`
}

// codexThreadParams 是只带 Thread id 的请求参数：thread/unsubscribe、thread/compact/start、thread/archive、thread/unarchive、thread/delete。
type codexThreadParams struct {
	ThreadID string `json:"threadId"`
}

// codexThreadSetNameParams 是 thread/name/set 的参数：Name 是新的对话标题。
type codexThreadSetNameParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

// codexThreadLoadedListParams 是 thread/loaded/list 的参数：Cursor 是上一页最后一个 Thread id；Limit 为 nil 时不分页。
type codexThreadLoadedListParams struct {
	Cursor *string `json:"cursor"`
	Limit  *int    `json:"limit"`
}

// codexThreadReadParams 是 thread/read 的参数：IncludeTurns 为 true 时 thread.turns 带全部 Turn 和它们的 Item。
type codexThreadReadParams struct {
	ThreadID     string `json:"threadId"`
	IncludeTurns bool   `json:"includeTurns"`
}

// codexThreadListParams 是 thread/list 里 app-server 认的参数；modelProviders、sortKey 等其余参数忽略。
type codexThreadListParams struct {
	Cursor *string `json:"cursor"`
	Limit  *int    `json:"limit"`
	// SortDirection 默认倒序。
	SortDirection *string `json:"sortDirection"`
	// Cwd 是客户端路径，换成 serve 路径后按对话归属的目录相等过滤。
	Cwd codexCwdFilter `json:"cwd"`
	// SearchTerm 按标题包含过滤。
	SearchTerm *string `json:"searchTerm"`
	// Archived 是否只列归档的对话？不带或为 false 时只列没归档的。
	Archived *bool `json:"archived"`
	// SourceKinds 含子 Agent 类时改列 AncestorThreadID 或 ParentThreadID 下的子 Agent。
	SourceKinds      []string `json:"sourceKinds"`
	AncestorThreadID *string  `json:"ancestorThreadId"`
	ParentThreadID   *string  `json:"parentThreadId"`
}

// codexCwdFilter 是 thread/list 的 cwd 过滤：线上可以是一个目录，也可以是目录数组；没有过滤时为空。
type codexCwdFilter []string

// UnmarshalJSON 将单个目录或目录数组解码为 cwd 筛选条件。
func (f *codexCwdFilter) UnmarshalJSON(data []byte) error {
	// 1. null 保留现值，字符串转换为单项列表，其他输入按字符串数组解码。
	if string(data) == "null" {
		return nil
	}
	var directory string
	if json.Unmarshal(data, &directory) == nil {
		*f = codexCwdFilter{directory}
		return nil
	}
	var directories []string
	if err := json.Unmarshal(data, &directories); err != nil {
		return err
	}
	*f = directories
	return nil
}

// codexThreadTurnsListParams 是 thread/turns/list 的参数。
type codexThreadTurnsListParams struct {
	ThreadID string `json:"threadId"`
	// Cursor 是上一页最后一个 TurnID。
	Cursor *string `json:"cursor"`
	Limit  *int    `json:"limit"`
	// SortDirection 默认倒序。
	SortDirection *string `json:"sortDirection"`
	// ItemsView 为 notLoaded、summary 或 full，默认 summary。
	ItemsView *string `json:"itemsView"`
}

// codexThreadItemsListParams 是 thread/items/list 的参数。
type codexThreadItemsListParams struct {
	ThreadID string `json:"threadId"`
	// TurnID 给出时只保留这个 Turn 的 Item。
	TurnID *string `json:"turnId"`
	// Cursor 是上一页最后一条原始 Item 的 Ordinal（十进制字符串）。
	Cursor *string `json:"cursor"`
	Limit  *int    `json:"limit"`
	// SortDirection 默认正序（Codex 0.163.0 的约定，和 thread/turns/list 相反）。
	SortDirection *string `json:"sortDirection"`
}

// codexTurnStartParams 是 turn/start 里 app-server 认的参数；cwd 等其余参数忽略。
type codexTurnStartParams struct {
	ThreadID string                `json:"threadId"`
	Input    []codexUserInputParam `json:"input"`
	// Model、Effort 为 nil 表示沿用根 Session 当前的选择。
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
	// CollaborationMode 带模型时用它的模型和推理强度，不看 Model、Effort（照 Codex 协议，Codex app 每次发消息都这样带）；
	// 它的模型为空时（Codex app 给还没打开的 Thread 的占位值）仍看 Model、Effort。
	CollaborationMode *codexCollaborationModeParam `json:"collaborationMode"`
	// ClientUserMessageID 是客户端给这条输入的标识，回填到发起连接的 userMessage.clientId。
	ClientUserMessageID string `json:"clientUserMessageId"`
	// OutputSchema 是要求最后回复满足的 JSON Schema 原文，不带或为 null 时没有要求。
	OutputSchema json.RawMessage `json:"outputSchema"`
}

// codexCollaborationModeParam 是 turn/start 带的协作模式，只认 Settings 里的模型和推理强度。
type codexCollaborationModeParam struct {
	Settings codexCollaborationModeSettings `json:"settings"`
}

// codexCollaborationModeSettings 是协作模式选的模型（provider_id/model_id）和推理强度；强度为 null 时和 turn/start 不带 effort 一样。
type codexCollaborationModeSettings struct {
	Model           string  `json:"model"`
	ReasoningEffort *string `json:"reasoning_effort"`
}

// codexTurnSteerParams 是 turn/steer 的参数：ExpectedTurnID 必须是根 Session 正在跑的 Turn。
type codexTurnSteerParams struct {
	ThreadID            string                `json:"threadId"`
	ExpectedTurnID      string                `json:"expectedTurnId"`
	Input               []codexUserInputParam `json:"input"`
	ClientUserMessageID string                `json:"clientUserMessageId"`
}

// codexTurnInterruptParams 是 turn/interrupt 的参数；中断的总是根 Session 的活动 Turn，TurnID 不参与判断。
type codexTurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// codexUserInputParam 是 turn/start、turn/steer 的一段输入，按 Type 只用其中一项：text 用 Text，image 用 URL，localImage 用 Path。
type codexUserInputParam struct {
	Type string `json:"type"`
	Text string `json:"text"`
	URL  string `json:"url"`
	Path string `json:"path"`
}

// ── Codex 启动查询和配置写入结果 ──

// codexStartupConfigResult 是 --print-startup-config 的一次性输出；托管策略要求和 configRequirements/read 使用同一格式。
type codexStartupConfigResult struct {
	AppServerVersion   string                            `json:"appServerVersion"`
	ConfigRequirements codexConfigRequirementsReadResult `json:"configRequirements"`
}

// codexInitializeResult 是 initialize 的结果：CodexHome 是 OpenVaala 配置文件所在目录。
type codexInitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOs     string `json:"platformOs"`
}

// codexAccountReadResult 是 account/read 的结果：OpenVaala 不提供 OpenAI 账号，也不要求 OpenAI 登录。
type codexAccountReadResult struct {
	Account            *codexAccount `json:"account"`
	RequiresOpenaiAuth bool          `json:"requiresOpenaiAuth"`
}

// codexAccount 是 Codex 的账号类型。
type codexAccount struct {
	Type string `json:"type"`
}

// codexGetAuthStatusResult 是 getAuthStatus 的结果：和 Codex 对不需要 OpenAI 登录的自定义 provider 的回答一样，
// 没有登录方式和 token，客户端看到 requiresOpenaiAuth 为 false 就不要求 OpenAI 登录。
type codexGetAuthStatusResult struct {
	AuthMethod         *string `json:"authMethod"`
	AuthToken          *string `json:"authToken"`
	RequiresOpenaiAuth bool    `json:"requiresOpenaiAuth"`
}

// codexModelListResult 是 model/list 的结果：整个模型目录在一页里，没有下一页。
type codexModelListResult struct {
	Data       []codexModel `json:"data"`
	NextCursor *string      `json:"nextCursor"`
}

// codexModel 是模型目录里的一个模型：ID 和 Model 都是 provider_id/model_id。
type codexModel struct {
	ID                        string                       `json:"id"`
	Model                     string                       `json:"model"`
	DisplayName               string                       `json:"displayName"`
	Description               string                       `json:"description"`
	Hidden                    bool                         `json:"hidden"`
	IsDefault                 bool                         `json:"isDefault"`
	SupportedReasoningEfforts []codexReasoningEffortOption `json:"supportedReasoningEfforts"`
	DefaultReasoningEffort    string                       `json:"defaultReasoningEffort"`
}

// codexReasoningEffortOption 是模型可选的一个推理强度。
type codexReasoningEffortOption struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description"`
}

// codexConfigRequirementsReadResult 是 configRequirements/read 的结果：OpenVaala 没有托管策略，Requirements 总为 null。
type codexConfigRequirementsReadResult struct {
	Requirements *struct{} `json:"requirements"`
}

// codexConfigReadResult 是 config/read 的结果：只含所选模型、模型 provider 和不拦截命令的审批、沙箱设置，Origins 总为空对象。
type codexConfigReadResult struct {
	Config  codexConfig `json:"config"`
	Origins struct{}    `json:"origins"`
}

// codexConfig 是 config/read 返回的配置，字段名使用 Codex config.toml 的写法。
type codexConfig struct {
	// Model 和 ModelReasoningEffort 是本连接显示的默认选择，空强度不输出。
	Model                string `json:"model"`
	ModelReasoningEffort string `json:"model_reasoning_effort,omitempty"`
	ModelProvider        string `json:"model_provider"`
	// ModelProviders 包含模型目录里的全部 provider。
	ModelProviders map[string]codexModelProviderConfig `json:"model_providers"`
	ApprovalPolicy string                              `json:"approval_policy"`
	SandboxMode    string                              `json:"sandbox_mode"`
	// Projects 只在请求带 cwd 时包含这个目录，信任级别为 trusted。
	Projects map[string]codexProjectConfig `json:"projects,omitempty"`
}

// codexModelProviderConfig 是 config/read 里一个模型 provider 的配置，Name 用 provider id。
type codexModelProviderConfig struct {
	Name string `json:"name"`
}

// codexProjectConfig 是 config/read 里一个项目目录的配置，只有信任级别。
type codexProjectConfig struct {
	TrustLevel string `json:"trust_level"`
}

// codexConfigWriteResult 是 config/batchWrite 的结果；OpenVaala 不保存 Codex 的配置。
type codexConfigWriteResult struct {
	// Status 总为 ok。
	Status string `json:"status"`
	// Version 总为空。
	Version string `json:"version"`
	// FilePath 是 codexHome（OpenVaala 配置所在目录，Codex 要求是绝对路径）。
	FilePath string `json:"filePath"`
	// OverriddenMetadata 总为 null。
	OverriddenMetadata *struct{} `json:"overriddenMetadata"`
}

// codexCollaborationModeListResult 是 collaborationMode/list 的结果：只有 Default 一种模式。
type codexCollaborationModeListResult struct {
	Data []codexCollaborationMode `json:"data"`
}

// codexCollaborationMode 是一种协作模式预设；不带模型、强度和指令。
type codexCollaborationMode struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// codexSkillsListResult 是 skills/list 的结果：只有一项，对应请求的第一个目录。
type codexSkillsListResult struct {
	Data []codexSkillsListEntry `json:"data"`
}

// codexSkillsListEntry 是一个目录下可用的 Skills；OpenVaala 不报告单个 Skill 的解析错误，Errors 总为空。
type codexSkillsListEntry struct {
	Cwd    string               `json:"cwd"`
	Skills []codexSkillMetadata `json:"skills"`
	Errors []struct{}           `json:"errors"`
}

// codexSkillMetadata 是一个 Skill：Path 是它在所在 Executor 上的路径，Scope 总为 user。
type codexSkillMetadata struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Scope       string `json:"scope"`
	Enabled     bool   `json:"enabled"`
}

// codexEmptyPageResult 是 experimentalFeature/list、permissionProfile/list、threadSection/list 的结果：
// OpenVaala 没有对应能力，Data 总为空、没有下一页；mcpServerStatus/list 没带 threadId 或 Thread 本连接没有跟随时也回它。
type codexEmptyPageResult struct {
	Data       []struct{} `json:"data"`
	NextCursor *string    `json:"nextCursor"`
}

// codexMcpServerStatusListResult 是 mcpServerStatus/list 带着本连接跟随的 threadId 时的结果：对话里每个 Executor 上的每个 MCP 连接一项，
// 一页返回全部、没有下一页。Data 必须是非 nil 切片。
type codexMcpServerStatusListResult struct {
	Data       []codexMcpServerStatus `json:"data"`
	NextCursor *string                `json:"nextCursor"`
}

// codexMcpServerStatus 是一个 MCP 连接；OpenVaala 没有 server 信息、资源和认证。
type codexMcpServerStatus struct {
	// Name 是 server id；同一个 server 跑在多个 Executor 上时，默认 Executor 以外的写成 <server id>@<executor id>。
	Name string `json:"name"`
	// RuntimeStatus 是 starting、connected 或 failed。
	RuntimeStatus *string `json:"runtimeStatus"`
	// PluginID 是声明这个 server 的插件名，不是插件声明的为 null。
	PluginID *string `json:"pluginId"`
	// ServerInfo 总是 null。
	ServerInfo *struct{} `json:"serverInfo"`
	// Tools 的键和 name 都是工具原名，没有工具时是空对象。
	Tools map[string]codexMcpTool `json:"tools"`
	// ToolsError 是最近一次连接失败的原因，没有时为 null。
	ToolsError *string `json:"toolsError"`
	// Resources 和 ResourceTemplates 总为空。
	Resources         []struct{} `json:"resources"`
	ResourceTemplates []struct{} `json:"resourceTemplates"`
	// AuthStatus 总是 unsupported。
	AuthStatus string `json:"authStatus"`
}

// codexMcpTool 是 MCP server 提供的一个工具；Description 为空时不写。
type codexMcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// codexHooksListResult 是 hooks/list 的结果：请求的每个目录一项，每项里都是同一份用户级 hook 列表。
type codexHooksListResult struct {
	Data []codexHooksListEntry `json:"data"`
}

// codexHooksListEntry 是一个目录下的 hook，Warnings 和 Errors 总为空；配置错误由 serve 加载返回。
type codexHooksListEntry struct {
	Cwd      string              `json:"cwd"`
	Hooks    []codexHookMetadata `json:"hooks"`
	Warnings []string            `json:"warnings"`
	Errors   []struct{}          `json:"errors"`
}

// codexPluginListResult 是 plugin/list 的结果：OpenVaala 没有插件市场，各项总为空。
type codexPluginListResult struct {
	Marketplaces          []struct{} `json:"marketplaces"`
	MarketplaceLoadErrors []struct{} `json:"marketplaceLoadErrors"`
	FeaturedPluginIDs     []string   `json:"featuredPluginIds"`
}

// codexPluginInstalledResult 是 plugin/installed 的结果：没有装任何插件。
type codexPluginInstalledResult struct {
	Marketplaces          []struct{} `json:"marketplaces"`
	MarketplaceLoadErrors []struct{} `json:"marketplaceLoadErrors"`
}

// codexAppInstalledResult 是 app/installed 的结果：没有装任何 App。
type codexAppInstalledResult struct {
	Apps []struct{} `json:"apps"`
}

// codexExternalAgentHistoriesResult 是 externalAgentConfig/import/readHistories 的结果：没有可从其他 Agent 导入的历史和连接器。
type codexExternalAgentHistoriesResult struct {
	Data       []struct{} `json:"data"`
	Connectors []struct{} `json:"connectors"`
}

// ── Codex 对话和执行结果 ──

// codexThreadOpenResult 是 thread/start、thread/resume、thread/fork 的结果，模型、强度和目录与 Thread 一致。
type codexThreadOpenResult struct {
	Thread        codexThread `json:"thread"`
	Model         string      `json:"model"`
	ModelProvider string      `json:"modelProvider"`
	// ServiceTier 总为 null。
	ServiceTier     *string `json:"serviceTier"`
	ReasoningEffort *string `json:"reasoningEffort"`
	Cwd             string  `json:"cwd"`
	// ApprovalPolicy 总是 never。
	ApprovalPolicy    string `json:"approvalPolicy"`
	ApprovalsReviewer string `json:"approvalsReviewer"`
	// Sandbox 按普通或临时 Thread 区分。
	Sandbox codexSandboxPolicy `json:"sandbox"`
	// TurnsBackwardsCursor 和 ItemsBackwardsCursor 都是 codexNewestHistoryCursor，新 Thread 也带游标、读到空历史。
	TurnsBackwardsCursor string `json:"turnsBackwardsCursor"`
	ItemsBackwardsCursor string `json:"itemsBackwardsCursor"`
}

// codexSandboxPolicy 是 Thread 的沙箱：普通 Thread 是 dangerFullAccess；临时 Thread 的对话没有 Executor，不能读写文件、执行命令，是 readOnly。
type codexSandboxPolicy struct {
	Type string `json:"type"`
}

// codexTurnStartResult 是 turn/start 的结果：拿到 TurnID 后回复，Turn 是 inProgress、没有 Item。
type codexTurnStartResult struct {
	Turn codexTurn `json:"turn"`
}

// codexTurnSteerResult 是 turn/steer 的结果：输入挂到的 Turn。
type codexTurnSteerResult struct {
	TurnID string `json:"turnId"`
}

// codexThreadUnsubscribeResult 是 thread/unsubscribe 的结果：Status 为 unsubscribed（本连接停止跟随）或 notLoaded（本连接没有跟随）。
type codexThreadUnsubscribeResult struct {
	Status string `json:"status"`
}

// codexThreadLoadedListResult 是 thread/loaded/list 的结果：本连接跟随的 Thread id，按 id 排序；还有下一页时 NextCursor 是本页最后一个 id。
// Data 必须是非 nil 切片。
type codexThreadLoadedListResult struct {
	Data       []string `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}

// codexEmptyObjectResult 是 turn/interrupt、thread/compact/start、thread/name/set、config/mcpServer/reload、thread/archive、thread/delete 的结果，
// 总是空对象。
type codexEmptyObjectResult struct{}

// codexThreadUnarchiveResult 是 thread/unarchive 的结果：取消归档后的 Thread，和 thread/list 里的一样不带 turns。
type codexThreadUnarchiveResult struct {
	Thread codexThread `json:"thread"`
}

// codexThreadReadResult 是 thread/read 的结果；includeTurns 为 true 时 Thread.Turns 带全部 Turn。
type codexThreadReadResult struct {
	Thread codexThread `json:"thread"`
}

// codexThreadListResult 是 thread/list 的结果：还有下一页时 NextCursor 是接着读的游标；BackwardsCursor 总是 null。Data 必须是非 nil 切片。
type codexThreadListResult struct {
	Data            []codexThread `json:"data"`
	NextCursor      *string       `json:"nextCursor"`
	BackwardsCursor *string       `json:"backwardsCursor"`
}

// codexThreadTurnsListResult 是 thread/turns/list 的结果：NextCursor 是本页最后一个 TurnID，没有下一页时为 null；BackwardsCursor 总是 null。
// Data 必须是非 nil 切片。
type codexThreadTurnsListResult struct {
	Data            []codexTurn `json:"data"`
	NextCursor      *string     `json:"nextCursor"`
	BackwardsCursor *string     `json:"backwardsCursor"`
}

// codexThreadItemsListResult 是 thread/items/list 的结果：NextCursor 是本页最后一条原始 Item 的 Ordinal（十进制字符串），
// 没有下一页时为 null；BackwardsCursor 总是 null。Data 必须是非 nil 切片。
type codexThreadItemsListResult struct {
	Data            []codexThreadItemEntry `json:"data"`
	NextCursor      *string                `json:"nextCursor"`
	BackwardsCursor *string                `json:"backwardsCursor"`
}

// ── Codex 通知 ──

// codexThreadStartedNotification 由连接在创建或分叉响应后发出；自动跟随子 Thread 时在启动跟随前发出。
type codexThreadStartedNotification struct {
	Thread codexThread `json:"thread"`
}

// codexThreadIDNotification 由连接在归档、取消归档或删除成功并响应后发给请求方连接。
type codexThreadIDNotification struct {
	ThreadID string `json:"threadId"`
}

// codexThreadStatusChangedNotification 由 Thread 在跟随 Session 的状态变化、同步被拒绝或之后重新打开时发出。
type codexThreadStatusChangedNotification struct {
	ThreadID string            `json:"threadId"`
	Status   codexThreadStatus `json:"status"`
}

// codexThreadNameUpdatedNotification 由根 Thread 在收到标题变化或同步到新标题时发出；未跟随时由连接在改名响应后发出。
type codexThreadNameUpdatedNotification struct {
	ThreadID   string `json:"threadId"`
	ThreadName string `json:"threadName"`
}

// codexThreadTokenUsageUpdatedNotification 由 Thread 在跟随 Session 的用量变化或响应恢复请求后发出。
// TurnID 是活动 Turn，没有时是最后一个 Turn。
type codexThreadTokenUsageUpdatedNotification struct {
	ThreadID   string                `json:"threadId"`
	TurnID     string                `json:"turnId"`
	TokenUsage codexThreadTokenUsage `json:"tokenUsage"`
}

// codexThreadTokenUsage 是 Thread 的用量。
type codexThreadTokenUsage struct {
	// Total 是累计用量。
	Total codexTokenUsageBreakdown `json:"total"`
	// Last 是当前上下文占用。
	Last codexTokenUsageBreakdown `json:"last"`
	// ModelContextWindow 是上下文窗口，未知时为 null。
	ModelContextWindow *int `json:"modelContextWindow"`
}

// codexTokenUsageBreakdown 是一组 token 计数。
type codexTokenUsageBreakdown struct {
	InputTokens           int `json:"inputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
	TotalTokens           int `json:"totalTokens"`
}

// codexContextCompactedNotification 由 Thread 在摘要 Item 的完成通知之后发出。
type codexContextCompactedNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

// codexTurnNotification 由 Thread 在 Turn 开始或结束时发出，同步补齐时也会发出；Turn 不含 Item，ItemsView 是 notLoaded。
type codexTurnNotification struct {
	ThreadID string    `json:"threadId"`
	Turn     codexTurn `json:"turn"`
}

// codexHookNotification 是 hook/started 和 hook/completed：connectionThread 收到跟随的 Session 的一次 hook 运行记录（HookRunChanged）时发出，
// 运行中是 hook/started，其余状态是 hook/completed；同一次运行的 Run.ID 不变。
type codexHookNotification struct {
	ThreadID string              `json:"threadId"`
	TurnID   string              `json:"turnId"`
	Run      codexHookRunSummary `json:"run"`
}

// codexItemStartedNotification 由 Thread 在首次显示 Item 时发出；StartedAtMs 是通知发出时的 Unix 毫秒。
type codexItemStartedNotification struct {
	Item        codexThreadItem `json:"item"`
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	StartedAtMs int64           `json:"startedAtMs"`
}

// codexItemCompletedNotification 由 Thread 在结束显示 Item 时发出，Item 包含完整内容；CompletedAtMs 是通知发出时的 Unix 毫秒。
type codexItemCompletedNotification struct {
	Item          codexThreadItem `json:"item"`
	ThreadID      string          `json:"threadId"`
	TurnID        string          `json:"turnId"`
	CompletedAtMs int64           `json:"completedAtMs"`
}

// codexItemDeltaNotification 由 Thread 在回复或命令出现新增文本时发出，包含目标 ItemID 和本次新增文本。
type codexItemDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

// codexReasoningSummaryTextDeltaNotification 由 Thread 在推理摘要新增文本时发出，SummaryIndex 指定段号。
type codexReasoningSummaryTextDeltaNotification struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	SummaryIndex int    `json:"summaryIndex"`
}

// codexReasoningTextDeltaNotification 由 Thread 在推理正文新增文本时发出，ContentIndex 指定段号。
type codexReasoningTextDeltaNotification struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	ContentIndex int    `json:"contentIndex"`
}

// ── Codex Thread 线上对象 ──

// codexThread 是 Codex 的 Thread 对象。
type codexThread struct {
	ID              string  `json:"id"`
	SessionID       string  `json:"sessionId"`
	Name            *string `json:"name"`
	Preview         string  `json:"preview"`
	Cwd             string  `json:"cwd"`
	ModelProvider   string  `json:"modelProvider"`
	Model           string  `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
	CreatedAt       int64   `json:"createdAt"`
	UpdatedAt       int64   `json:"updatedAt"`
	// Status 的 Type 为 notLoaded、idle、systemError 或 active。
	Status codexThreadStatus `json:"status"`
	// Source 根 Thread 用 codexAppServerSource，子 Thread 用 codexSubAgentSource。
	Source codexSessionSource `json:"source"`
	// ThreadSource 只有临时 Thread 有，原样回 thread/start 带的值：Codex TUI 据此（连同 Ephemeral）认出自己起的隐藏 Thread，
	// 不把它的 thread/started 当成新 Thread；普通 Thread 为 null。
	ThreadSource         *string `json:"threadSource"`
	ParentThreadID       *string `json:"parentThreadId"`
	AgentNickname        *string `json:"agentNickname"`
	AgentRole            *string `json:"agentRole"`
	CanAcceptDirectInput bool    `json:"canAcceptDirectInput"`
	// HistoryMode 总是 paginated。
	HistoryMode  string    `json:"historyMode"`
	Ephemeral    bool      `json:"ephemeral"`
	CLIVersion   string    `json:"cliVersion"`
	ProjectID    *string   `json:"projectId"`
	Path         *string   `json:"path"`
	GitInfo      *struct{} `json:"gitInfo"`
	ForkedFromID *string   `json:"forkedFromId"`
	// Turns 只有 thread/read{includeTurns:true} 和不带 excludeTurns 的 thread/resume、thread/fork 填；
	// 其余必须是空切片而不是 nil，否则线上是 null。
	Turns []codexTurn `json:"turns"`
}

// codexThreadStatus 是 Thread 的运行状态。
type codexThreadStatus struct {
	Type string `json:"type"`
	// ActiveFlags 只在 Type 为 active 时有值，这时必须是空切片而不是 nil，线上是 []；其他状态为 nil，线上没有这个字段。
	ActiveFlags []string `json:"activeFlags,omitzero"`
}

// codexSessionSource 是 Thread 的来源：根 Thread 是 codexAppServerSource，子 Thread 是 codexSubAgentSource。
type codexSessionSource interface{ isCodexSessionSource() }

// codexAppServerSource 是根 Thread 的来源，线上是字符串 "appServer"。
type codexAppServerSource string

func (codexAppServerSource) isCodexSessionSource() {}

// codexSubAgentSource 是子 Thread 的来源，线上是 {"subAgent":{"thread_spawn":{...}}}。
type codexSubAgentSource struct {
	SubAgent codexSubAgentSpawn `json:"subAgent"`
}

func (codexSubAgentSource) isCodexSessionSource() {}

// codexSubAgentSpawn 是子 Agent 由父 Agent spawn 出来的来源。
type codexSubAgentSpawn struct {
	ThreadSpawn codexThreadSpawn `json:"thread_spawn"`
}

// codexThreadSpawn 记录子 Thread 的父 Thread、层数、任务名和 Agent 名，不输出 agent_path。
type codexThreadSpawn struct {
	ParentThreadID string `json:"parent_thread_id"`
	Depth          int    `json:"depth"`
	AgentNickname  string `json:"agent_nickname"`
	AgentRole      string `json:"agent_role"`
}

// ── Codex Hook 线上对象 ──

// codexHookRunSummary 是 hook/started 和 hook/completed 里的一次 hook 运行（Codex 的 HookRunSummary）。
type codexHookRunSummary struct {
	ID        string `json:"id"`
	EventName string `json:"eventName"`
	// HandlerType 总是 command。
	HandlerType string `json:"handlerType"`
	// ExecutionMode 总是 sync。
	ExecutionMode string `json:"executionMode"`
	Scope         string `json:"scope"`
	SourcePath    string `json:"sourcePath"`
	// Source 总是 user。
	Source       string `json:"source"`
	DisplayOrder int    `json:"displayOrder"`
	Status       string `json:"status"`
	// StatusMessage 总为 null。
	StatusMessage *string `json:"statusMessage"`
	// StartedAt 是 Unix 秒。
	StartedAt int64 `json:"startedAt"`
	// CompletedAt 是 Unix 秒，运行中时为 null。
	CompletedAt *int64 `json:"completedAt"`
	// DurationMs 是毫秒，运行中时为 null。
	DurationMs *int64 `json:"durationMs"`
	// Entries 必须是空切片而不是 nil，否则线上是 null。
	Entries []codexHookOutputEntry `json:"entries"`
}

// codexHookOutputEntry 是 hook 运行输出的一段：Kind 是 context、feedback、stop、warning 或 error。
type codexHookOutputEntry struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// codexHookMetadata 是 hooks/list 里的一个 hook（Codex 的 HookMetadata），Codex 把 handler 的字段摊平在对象里。
type codexHookMetadata struct {
	Key       string `json:"key"`
	EventName string `json:"eventName"`
	// HandlerType 总是 command。
	HandlerType string `json:"handlerType"`
	// Command 是命令原文（script 是路径加 args）。
	Command string `json:"command"`
	// Async 总为 false。
	Async bool `json:"async"`
	// Matcher 没写时为 null。
	Matcher    *string `json:"matcher"`
	TimeoutSec int     `json:"timeoutSec"`
	// StatusMessage、AdditionalContextLimit 总为 null。
	StatusMessage          *string `json:"statusMessage"`
	AdditionalContextLimit *int    `json:"additionalContextLimit"`
	SourcePath             string  `json:"sourcePath"`
	// Source 是 user 或 plugin。
	Source string `json:"source"`
	// PluginID 是 hook 所属的插件名，Source 为 user 时为 null。
	PluginID     *string `json:"pluginId"`
	DisplayOrder int     `json:"displayOrder"`
	// Enabled 总为 true。
	Enabled bool `json:"enabled"`
	// IsManaged 总为 false。
	IsManaged   bool   `json:"isManaged"`
	CurrentHash string `json:"currentHash"`
	// TrustStatus 总是 trusted。
	TrustStatus string `json:"trustStatus"`
}

// ── Codex Turn 线上对象 ──

// codexTurn 是 Codex 的 Turn 对象。
type codexTurn struct {
	ID string `json:"id"`
	// Status 为 inProgress、completed、failed 或 interrupted。
	Status string          `json:"status"`
	Error  *codexTurnError `json:"error"`
	// Items 必须是非 nil 切片。
	Items []codexThreadItem `json:"items"`
	// ItemsView 为 notLoaded、summary 或 full。
	ItemsView string `json:"itemsView"`
}

// codexTurnError 是失败 Turn 的错误，Message 取 Turn 记录的错误文本。
type codexTurnError struct {
	Message string `json:"message"`
}

// ── Codex ThreadItem 线上对象 ──

// codexThreadItem 是 Codex 的 ThreadItem，下面每个变体对应线上 type 的一种取值；字段名和必填项照 Codex 0.163.0 schema。
type codexThreadItem interface{ isCodexThreadItem() }

// codexThreadItemEntry 是 thread/items/list 的一项：ThreadItem 和它所在的 Turn；两个时间都是 Item 保存时的 Unix 毫秒。
type codexThreadItemEntry struct {
	TurnID string          `json:"turnId"`
	Item   codexThreadItem `json:"item"`
	// StartedAtMs 取开头的 Item（工具调用是 ToolCall），没有保存时间时为 null。
	StartedAtMs *int64 `json:"startedAtMs"`
	// CompletedAtMs 取结尾的 Item（工具调用是 ToolResult），没有保存时间或工具调用还没有结果时为 null。
	CompletedAtMs *int64 `json:"completedAtMs"`
}

// codexUserMessageItem 是用户输入。
type codexUserMessageItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// ClientID 是发起输入的 Codex 连接在 turn/start 里带的 clientUserMessageId，翻译不填，由连接补；没有时为 null。
	ClientID *string          `json:"clientId"`
	Content  []codexUserInput `json:"content"`
}

func (*codexUserMessageItem) isCodexThreadItem() {}

// codexAgentMessageItem 是模型回复。
type codexAgentMessageItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
	// Phase 在本轮之后还有输出时为 commentary，本轮停在这条回复时为 final_answer；还不知道时不输出。
	Phase string `json:"phase,omitempty"`
}

func (*codexAgentMessageItem) isCodexThreadItem() {}

// codexReasoningItem 是模型的推理；Summary 和 Content 必须是非 nil 切片。
type codexReasoningItem struct {
	Type    string   `json:"type"`
	ID      string   `json:"id"`
	Summary []string `json:"summary"`
	Content []string `json:"content"`
}

func (*codexReasoningItem) isCodexThreadItem() {}

// codexCommandExecutionItem 是一次 bash 调用。
type codexCommandExecutionItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Status  string `json:"status"`
	// AggregatedOutput 在结果到达前为 null；失败的结果没有输出时是结果的错误信息。
	AggregatedOutput *string `json:"aggregatedOutput"`
	// ExitCode 取结果里的退出码，结果到达前或结果没有退出码时为 null。
	ExitCode *int `json:"exitCode"`
	// CommandActions 必须是空切片。
	CommandActions []struct{} `json:"commandActions"`
	// Source 总是 agent。
	Source string `json:"source"`
	// ProcessID 和 DurationMs 总是 null。
	ProcessID  *string `json:"processId"`
	DurationMs *int64  `json:"durationMs"`
}

func (*codexCommandExecutionItem) isCodexThreadItem() {}

// codexFileChangeItem 是一次 apply_patch 调用；Changes 必须是非 nil 切片。
type codexFileChangeItem struct {
	Type    string                  `json:"type"`
	ID      string                  `json:"id"`
	Changes []codexFileUpdateChange `json:"changes"`
	Status  string                  `json:"status"`
}

func (*codexFileChangeItem) isCodexThreadItem() {}

// codexFileUpdateChange 是补丁里一个文件的变更：Path 是所选 Executor 工作目录下的绝对路径。
type codexFileUpdateChange struct {
	Path string               `json:"path"`
	Kind codexPatchChangeKind `json:"kind"`
	Diff string               `json:"diff"`
}

// codexPatchChangeKind 是文件变更的种类，Type 为 add、update 或 delete；update 不带 move_path，OpenVaala 的补丁不支持改名。
type codexPatchChangeKind struct {
	Type string `json:"type"`
}

// codexMcpToolCallItem 是 bash、apply_patch 和子 Agent 协作之外的工具调用，以及协作工具调用失败时的显示。
type codexMcpToolCallItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Status    string          `json:"status"`
	Arguments json.RawMessage `json:"arguments"`
	// Result 在结果到达前或结果带错误时为 null。
	Result *codexMcpToolResult `json:"result"`
	// Error 只在结果带错误时有值。
	Error *codexMcpToolCallError `json:"error"`
}

func (*codexMcpToolCallItem) isCodexThreadItem() {}

// codexMcpToolResult 是工具调用的结果：Content 是结果的可读正文，后面跟工具读到的图片；StructuredContent 是结果原文 JSON，不含图片数据。
type codexMcpToolResult struct {
	Content           []codexMcpContent `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
}

// codexMcpToolCallError 是工具调用失败的原因。
type codexMcpToolCallError struct {
	Message string `json:"message"`
}

// codexMcpContent 是工具结果的一段 MCP 内容，下面两个变体对应线上 type 的 text 和 image。
type codexMcpContent interface{ isCodexMcpContent() }

// codexMcpTextContent 是一段文本；Text 即使为空串也要输出，MCP 规定它是必需字段。
type codexMcpTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (*codexMcpTextContent) isCodexMcpContent() {}

// codexMcpImageContent 是一张图片：Data 是 base64 数据，MimeType 是图片类型。
type codexMcpImageContent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

func (*codexMcpImageContent) isCodexMcpContent() {}

// codexSubAgentActivityItem 是父 Thread 里的子 Agent 活动，由 spawn_agent、send_message、interrupt_agent 的成功结果和子 Agent 的完成通知推出来。
type codexSubAgentActivityItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// Kind 为 started、interacted、interrupted 或 completed。
	Kind          string `json:"kind"`
	AgentThreadID string `json:"agentThreadId"`
	// AgentPath 只用于显示。
	AgentPath string `json:"agentPath"`
}

func (*codexSubAgentActivityItem) isCodexThreadItem() {}

// codexContextCompactionItem 是一次上下文压缩，对应保存下来的摘要 Item。
type codexContextCompactionItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (*codexContextCompactionItem) isCodexThreadItem() {}

// ── Codex UserInput 线上对象 ──

// codexUserInput 是用户消息的一段内容，下面两个变体对应线上 type 的 text 和 image。
type codexUserInput interface{ isCodexUserInput() }

// codexTextUserInput 是一段文本；TextElements 总是空切片，OpenVaala 不标注文本里的特殊片段。
type codexTextUserInput struct {
	Type         string     `json:"type"`
	Text         string     `json:"text"`
	TextElements []struct{} `json:"text_elements"`
}

func (*codexTextUserInput) isCodexUserInput() {}

// codexImageUserInput 是一张图片，URL 是保存下来的 data URL。
type codexImageUserInput struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

func (*codexImageUserInput) isCodexUserInput() {}

// ── Codex ThreadItem 常量 ──

// ThreadItem 线上 type 的取值，只列出 OpenVaala 会产生的变体。
const (
	codexItemUserMessage       = "userMessage"
	codexItemAgentMessage      = "agentMessage"
	codexItemReasoning         = "reasoning"
	codexItemCommandExecution  = "commandExecution"
	codexItemFileChange        = "fileChange"
	codexItemMcpToolCall       = "mcpToolCall"
	codexItemSubAgentActivity  = "subAgentActivity"
	codexItemContextCompaction = "contextCompaction"
)

// codexNewestHistoryCursor 是打开响应里倒序翻历史的起点游标：本连接跟随着的 Thread 读到打开响应记下的位置，再接实时推送；
// 普通分页不带它时直接读存储。只在 thread/start、thread/resume、thread/fork 响应的 TurnsBackwardsCursor、ItemsBackwardsCursor 里给出。
const codexNewestHistoryCursor = "newest"

// agentMessage 的 phase 取值。
const (
	codexMessagePhaseCommentary  = "commentary"
	codexMessagePhaseFinalAnswer = "final_answer"
)

// 用户消息内容线上 type 的取值。
const (
	codexUserInputText  = "text"
	codexUserInputImage = "image"
)

// 工具调用 Item 的状态：commandExecution、fileChange、mcpToolCall 的取值相同。
const (
	codexToolStatusInProgress = "inProgress"
	codexToolStatusCompleted  = "completed"
	codexToolStatusFailed     = "failed"
)

// subAgentActivity 的种类。
const (
	codexActivityStarted     = "started"
	codexActivityInteracted  = "interacted"
	codexActivityInterrupted = "interrupted"
	codexActivityCompleted   = "completed"
)

// ── Codex Thread、Turn 常量 ──

// Thread 运行状态的 type 取值：systemError 表示重新同步被 serve 拒绝，Thread 已放弃跟随，下一次 turn/start 重新打开；
// notLoaded 表示本连接没有跟随这个 Thread。
const (
	codexThreadStatusNotLoaded   = "notLoaded"
	codexThreadStatusIdle        = "idle"
	codexThreadStatusActive      = "active"
	codexThreadStatusSystemError = "systemError"
)

// thread/list 的 sourceKinds 里表示子 Agent 的取值：含其中任何一个时 thread/list 改列子 Agent。
const (
	codexSourceKindSubAgent            = "subAgent"
	codexSourceKindSubAgentThreadSpawn = "subAgentThreadSpawn"
)

// 翻页方向的取值。
const (
	codexSortAsc  = "asc"
	codexSortDesc = "desc"
)

// thread/unsubscribe 结果的 status 取值。
const (
	codexThreadUnsubscribed = "unsubscribed"
	codexThreadNotLoaded    = "notLoaded"
)

// Turn 状态的取值；Turn 记录的 running 对应 inProgress，其余同名。
const (
	codexTurnInProgress  = "inProgress"
	codexTurnCompleted   = "completed"
	codexTurnFailed      = "failed"
	codexTurnInterrupted = "interrupted"
)

// Turn 的 itemsView 取值：不带 Item 的 Turn 是 notLoaded，turn/start 的响应、turn/started 和 turn/completed 都这样发，和 Codex 一致；
// 带全部 Item 的 Turn（thread/resume 的 thread.turns、thread/turns/list 的 summary 和 full）是 full；summary 只出现在 thread/turns/list 的请求里。
const (
	codexTurnItemsNotLoaded = "notLoaded"
	codexTurnItemsSummary   = "summary"
	codexTurnItemsFull      = "full"
)

// ── Codex Hook 常量 ──

// OpenVaala 的 hook 在 Codex 协议里固定的取值：只有同步运行的 command，来源是用户级配置或插件，列表里都是已信任、启用、非托管的。
const (
	codexHookHandlerCommand = "command"
	codexHookExecutionSync  = "sync"
	codexHookSourceUser     = "user"
	codexHookSourcePlugin   = "plugin"
	codexHookTrusted        = "trusted"
)

// hook 运行的 scope 取值：SessionStart、SubagentStart 属于 Thread，其余事件属于 Turn。
const (
	codexHookScopeThread = "thread"
	codexHookScopeTurn   = "turn"
)

// hook 运行输出 entries 里 kind 的取值。
const (
	codexHookEntryContext  = "context"
	codexHookEntryFeedback = "feedback"
	codexHookEntryStop     = "stop"
	codexHookEntryWarning  = "warning"
	codexHookEntryError    = "error"
)

// ── Codex MCP server 状态常量 ──

// mcpServerStatus/list 里 runtimeStatus 的取值：连接中、已连上、失败，大小写同 Codex 的序列化（camelCase）。
const (
	codexMcpRuntimeStarting  = "starting"
	codexMcpRuntimeConnected = "connected"
	codexMcpRuntimeFailed    = "failed"
)

// codexMcpAuthUnsupported 是 mcpServerStatus/list 里 authStatus 的取值：OpenVaala 的 MCP 连接没有认证流程。
const codexMcpAuthUnsupported = "unsupported"

// ── connectionThread 事件 ──

// connectionThreadRequest 是连接交给 connectionThread 处理的请求，由 Thread 自己回复。
type connectionThreadRequest interface{ isConnectionThreadRequest() }

// turnStartRequested 由连接在收到根 Thread 的 turn/start、把输入转换成 OpenVaala 内容之后交给 connectionThread：
// Thread 按需切换模型，以 idle_only 提交输入，在保存的 Item 里看到这条输入、拿到 TurnID 后回复（这一轮先结束时在 TurnFinished 里回复），
// 回复之前暂存这个 Thread 的通知。
type turnStartRequested struct {
	requestID json.RawMessage
	content   []*defs.Content
	// model、effort 为 nil 表示沿用根 Session 当前的选择。
	model               *string
	effort              *string
	clientUserMessageID string
}

// turnSteerRequested 由连接在收到根 Thread 的 turn/steer、把输入转换成 OpenVaala 内容之后交给 connectionThread：
// expectedTurnID 是正在跑的 Turn 时，Thread 以 steer 提交输入并回复 {turnId}，否则回 -32600。
type turnSteerRequested struct {
	requestID           json.RawMessage
	expectedTurnID      defs.TurnID
	content             []*defs.Content
	clientUserMessageID string
}

// threadResumeRequested 由连接在收到本连接已经跟随的 Thread 的 thread/resume 后交给 connectionThread：Thread 不重新打开，
// 按现在的对话记录、Session 详情和状态回复，turns 是连接读好的全部 Turn（excludeTurns 时为空切片）。
type threadResumeRequested struct {
	requestID json.RawMessage
	turns     []codexTurn
}

// threadNameSetRequested 由连接在收到已跟随 Thread 的改名请求后交给 connectionThread：Thread 保存标题并先回复，
// 调用期间暂存的标题变更事件随后由主循环通知，不重复发送改名通知。
type threadNameSetRequested struct {
	requestID json.RawMessage
	title     string
}

// threadStatusRequested 由连接在读取 Thread 的状态（thread/read、thread/list、thread/unarchive）时交给本连接跟随着的 connectionThread：
// sessionID 是 Thread 自己的 Session 时回复已报告状态，其他已知 Session 回复最近运行状态，未知 Session 回复 notLoaded。
type threadStatusRequested struct {
	sessionID defs.SessionID
	// reply 容量为 1。
	reply chan codexThreadStatus
}

// threadHistoryEndRequested 由连接在用起点游标翻页读本连接跟随着的 Thread 的历史（thread/turns/list、thread/items/list）时交给 connectionThread：
// Thread 把读到的最后位置（historyEndOrdinal）放进 reply，这个位置之后保存的 Item 由实时推送接上。
type threadHistoryEndRequested struct {
	// reply 容量为 1。
	reply chan storage.Ordinal
}

func (*turnStartRequested) isConnectionThreadRequest()        {}
func (*turnSteerRequested) isConnectionThreadRequest()        {}
func (*threadResumeRequested) isConnectionThreadRequest()     {}
func (*threadNameSetRequested) isConnectionThreadRequest()    {}
func (*threadStatusRequested) isConnectionThreadRequest()     {}
func (*threadHistoryEndRequested) isConnectionThreadRequest() {}

// connectionThreadOpened 由 connectionThread 在首次同步完成或打开失败后交给等待它的连接，每个 Thread 只交一次。
type connectionThreadOpened struct {
	// thread 是回复打开请求用的 Thread 对象。
	thread codexThread
	// followStart 是跟随起点：新开始跟随时 thread/resume、thread/fork 的 thread.turns 读到它为止。
	followStart historyReadEnd
	// subAgentSessionIDs 是首次同步时对话里的全部子 Session，连接据此记下这些子 Thread 属于哪个对话（不论是否跟随）。
	subAgentSessionIDs []defs.SessionID
	// delivered 由连接在回复打开请求后关闭（thread/start、thread/fork 还要先发 thread/started），Thread 这之后才开始发自己的通知；
	// 自动跟随的子 Thread 在首次同步后直接关闭。
	delivered chan struct{}
	// err 是打开失败的原因；失败时只带它，Thread 随即结束。
	err error
}

// subAgentSessionDiscovered 由根 Thread 的 connectionThread 在对话状态（ConversationSynced、SessionStatusChanged）里看到需要自动跟随的子 Session 时
// 交给连接：打开根 Thread 时正在跑 Turn 的、之后新出现的（刚 spawn 出来），以及之后从空闲变成在跑的。
// 连接还没有跟随它时先发它的 thread/started，再开始跟随。
type subAgentSessionDiscovered struct {
	conversationID defs.ConversationID
	// thread 是按当时的对话状态给出的子 Thread 对象，它的 id 就是子 SessionID，状态按它的运行信息。
	thread codexThread
}

// ── appServerConnection 历史读取记录 ──

// historyThread 是 thread/read 和翻页要读的 Thread 在存储里的位置：它所属的对话记录，和日志属于它的那个 Session
// （根 Thread 是对话的根 Session，子 Thread 是子 Session）。
type historyThread struct {
	threadID     string
	conversation defs.Conversation
	sessionID    defs.SessionID
}

// itemsPageQuery 是 thread/items/list 读一页日志的条件。
type itemsPageQuery struct {
	order storage.Direction
	// cursor 为 0 时从 order 方向的起点读起，否则从游标的下一条读起。
	cursor storage.Ordinal
	// limit 是这一页要的原始 Item 条数；ToolCall 和 ToolResult 被拆开时会多读，直到配上。
	limit int
	// turn 非空时只要这个 Turn 范围内的 Item。
	turn *defs.Turn
	// isSubAgentSession 是不是子 Session 的日志？是时日志开头是从父 Session 复制来的历史，session_fork 之前的不属于这个 Thread。
	isSubAgentSession bool
}

// itemsPage 是读到的一页日志。
type itemsPage struct {
	// items 按读取方向排列。
	items []*defs.Item
	// nextCursor 是接着读的游标（最后读到的那条原始 Item 的 Ordinal），0 表示这个方向读完了。
	nextCursor storage.Ordinal
}

// subAgentSession 是对话里的一个子 Agent Session 和它在 Agent 树里的位置；父 Thread 和层数填进子 Thread 来源的 thread_spawn。
type subAgentSession struct {
	record defs.Session
	// parentThreadID 是父 Thread 的 id：父是根 Session 时是 ConversationID，否则是父 Session 的 SessionID。
	parentThreadID string
	// ancestorSessionIDs 是沿父链往上的 SessionID，最近的父在前，最后是根 Session；它的长度是层数，根的直接子 Agent 是 1。
	ancestorSessionIDs []defs.SessionID
}

// ── connectionThread 运行记录 ──

// historyReadEnd 标出读 Thread 历史时读到哪里：新开始跟随时 thread/resume 响应里的 thread.turns 读到本连接开始跟随一个 Thread 的那一刻（首次同步，即跟随起点），
// 用打开响应里的起点游标翻历史时读到 connectionThread 的 historyEndOrdinal；这个位置之后保存的由实时推送发出。
// thread/read 不用它，读到日志末尾。
type historyReadEnd struct {
	// ordinal 是读到的最后一条的流水号，更靠后的不读；为 0（读到位置时日志里还没有 Item）时一条都不读。
	ordinal storage.Ordinal
	// activeTurnID 是首次同步时在跑的 Turn，没有时为空；本轮已保存且仍在执行的调用由实时推送显示，thread.turns 不再包含。
	// 翻页时为空，不移除任何工具调用。
	activeTurnID defs.TurnID
}

// threadItemDisplay 是一个 OpenVaala Item 已经发给 Codex 客户端的部分：标志和已经以增量发出的长度，生成中的 Item 另存一份到现在为止的内容。
// 工具调用的 ToolCall 和 ToolResult 合成一条 Codex Item，它的开始和结束记在 ToolCall 的记录上。
type threadItemDisplay struct {
	// itemID 是对应 Codex Item 的 id，增量通知用它。
	itemID defs.ItemID
	// isStarted item/started 是否已经发出？
	isStarted bool
	// isCompleted item/completed 是否已经发出？
	isCompleted bool
	// isSaved 这个 Item 是不是已经保存、之后的翻页能读到？已经跟随时的 thread/resume 只重新发出还没保存的回复和推理。
	isSaved bool
	// messagePhase 是回复完成时带的 phase：这一轮在它之后还有输出时是 commentary，这一轮停在它上面时是 final_answer；
	// 为空表示还不知道，这时回复不发 item/completed。
	messagePhase string
	// textBytes 是已经以增量发出的回复文本字节数。
	textBytes int
	// summaryBytes 是已经以增量发出的推理摘要字节数。
	summaryBytes int
	// contentBytes 是已经以增量发出的推理正文字节数。
	contentBytes int
	// streamingItem 是本轮输出中这个位置的 Item 到现在为止的内容：开始事件或同步结果里生成中 Item 的副本，之后按增量累加，完成或保存时换成完整内容；
	// 不是生成中交来的 Item 时为 nil。
	// 工具调用开始前提前完成还开着的回复和推理，以及已经跟随时的 thread/resume 重新发出生成中的 Item，都用它。
	streamingItem *defs.Item
}

// openReply 是本轮已经 item/started、还没 item/completed 的回复或推理（connectionThread.openReplies）：回复要等知道这一轮在它之后
// 还有没有输出才能定 phase。
type openReply struct {
	display *threadItemDisplay
	// item 是记下时的内容；生成中的以显示记录的 streamingItem 为准，它之后还按增量累加。
	item *defs.Item
}

// getContent 返回回复或推理目前已有的内容。
func (r openReply) getContent() *defs.Item {
	// 1. 优先使用显示记录中的累计内容，没有时使用登记内容。
	if r.display.streamingItem != nil {
		return r.display.streamingItem
	}
	return r.item
}

// pendingTurnStart 是一个提交了输入、还在等 TurnID 的 turn/start：输入 Item 出现在 ItemsSaved 里时，先回 requestID 的响应，
// 再按顺序发出暂存的通知。
type pendingTurnStart struct {
	requestID   json.RawMessage
	inputItemID defs.ItemID
}
