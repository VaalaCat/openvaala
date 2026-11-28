package defs

import "context"

// AgentType 标识 Agent 使用的执行类型。
type AgentType string

const (
	AgentTypeNative AgentType = "native"
	AgentTypeACP    AgentType = "acp"
)

// IsExternal 这个 Agent 类型是否使用外部驱动？空类型沿用原生 Agent。
func (t AgentType) IsExternal() bool { return t != "" && t != AgentTypeNative }

// Session 是一个 Agent 在对话里的一段上下文：所属对话、用哪个 Agent 和模型；子 Agent 还记着是谁创建的。
type Session struct {
	ID SessionID `json:"id"`
	// UpdatedAt 是最后一次写入的 Unix 毫秒，由存储在写入时分配；写入时设置的值不会被保存。
	UpdatedAt      int64          `json:"updated_at,omitempty"`
	ConversationID ConversationID `json:"conversation_id"`
	AgentName      string         `json:"agent_name"`
	// TaskName 是 spawn_agent 给子 Agent 起的任务名；根 Session 为空。
	TaskName string `json:"task_name,omitempty"`
	// ParentSessionID、ParentTurnID、ParentCallID 是创建这个子 Agent 的父 Session、Turn 和 spawn_agent 调用；根 Session 为空。
	ParentSessionID SessionID `json:"parent_session_id,omitempty"`
	ParentTurnID    TurnID    `json:"parent_turn_id,omitempty"`
	ParentCallID    string    `json:"parent_call_id,omitempty"`
	// Model 是解析后的模型和推理强度，新建或切换模型时写入；零值按 Agent 默认模型处理。
	Model ModelSelection `json:"model,omitzero"`
	// External 保存外部后端的恢复句柄。
	External *ExternalSession `json:"external,omitempty"`
	// Labels 是会话标签。
	Labels map[string]string `json:"labels,omitempty"`
	// Background 是否在默认会话列表中隐藏？
	Background bool `json:"background,omitempty"`
	// DetachedConversationID 是分离后新对话的 ID。
	DetachedConversationID ConversationID `json:"detached_conversation_id,omitempty"`
	// DetachedFromConversationID 和 DetachedFromSessionID 是转为独立对话前的对话和会话 ID，仅新根保存。
	DetachedFromConversationID ConversationID `json:"detached_from_conversation_id,omitempty"`
	DetachedFromSessionID      SessionID      `json:"detached_from_session_id,omitempty"`
	// SuppressFinishReport 是否跳过结束时发给父会话的回报？
	SuppressFinishReport bool `json:"suppress_finish_report,omitempty"`
}

// ── 外部 Agent 配置与会话选项 ──

// ExternalSession 保存外部 Agent 会话的恢复信息。
type ExternalSession struct {
	AgentType  AgentType      `json:"agent_type"`
	ExecutorID string         `json:"executor_id,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Cwd        string         `json:"cwd,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// AgentConfig 是 agents/ 下一个 Agent 文件的内容；原生 default Agent 沿用未指定的内置字段。
type AgentConfig struct {
	// Name 必须和文件名一致。
	Name string `yaml:"name" json:"name"`
	// Type 为空时使用原生 Agent。
	Type AgentType `yaml:"type,omitempty" json:"type,omitempty"`
	// Description 是 Agent 的用途说明。
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Instructions 是原生 Agent 的指令。
	Instructions string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	// Model 是 Agent 的主模型；省略时加载后取默认模型，模型目录为空时保持为 nil。
	Model *AgentModelConfig `yaml:"model,omitempty" json:"model,omitempty"`
	// CompactModel 是生成上下文摘要使用的模型；省略时使用 Model。
	CompactModel *AgentModelConfig `yaml:"compact_model,omitempty" json:"compact_model,omitempty"`
	// Tools 是可用普通工具名，由使用方的工具目录解释；省略时可用全部普通工具。
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// Skills 是可用 skill 名，由使用方的 skill 目录解释；省略时可用全部 skill。
	Skills []string `yaml:"skills,omitempty" json:"skills,omitempty"`
	// MCPServers 是可用的 MCP server id；省略时全部可用（包括客户端连接带来的 MCP server），[] 表示不用 MCP。
	MCPServers OptionalStringList `yaml:"mcp_servers,omitempty" json:"mcp_servers,omitzero"`
	// MaxSteps 是一轮最多调用模型的次数；省略或 0 表示不限，不能为负。
	MaxSteps *int `yaml:"max_steps,omitempty" json:"max_steps,omitempty"`
	// ACP 保存外部 ACP Agent 的启动命令。
	ACP *ACPCommand `yaml:"acp,omitempty" json:"acp,omitempty"`
}

// AgentModelConfig 引用 Provider 中的模型别名。
type AgentModelConfig struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model,omitempty" json:"model,omitempty"`
}

// ACPCommand 保存 ACP Agent 的启动命令、参数和环境变量。
type ACPCommand struct {
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// ParamOverwrite 按 ACP 方法名合并进驱动发出的请求。
	ParamOverwrite map[string]map[string]any `yaml:"param_overwrite,omitempty" json:"param_overwrite,omitempty"`
	// EphemeralParamOverwrite 只在临时会话叠加，非空表示支持不落盘的临时会话。
	EphemeralParamOverwrite map[string]map[string]any `yaml:"ephemeral_param_overwrite,omitempty" json:"ephemeral_param_overwrite,omitempty"`
}

// ExternalCapabilities 是外部会话公开给客户端的能力。
type ExternalCapabilities struct {
	SupportsSteer            bool `json:"supports_steer"`
	SupportsEphemeralSession bool `json:"supports_ephemeral_session"`
}

// ExternalOptions 保存外部会话上报的选项及其当前值。
type ExternalOptions struct {
	Mode    *ExternalOptionGroup  `json:"mode,omitempty"`
	Model   *ExternalOptionGroup  `json:"model,omitempty"`
	Thought *ExternalOptionGroup  `json:"thought,omitempty"`
	Others  []ExternalOptionGroup `json:"others,omitempty"`
}

// ExternalOptionGroup 保存外部会话的一组可选项。
type ExternalOptionGroup struct {
	// Kind 为 mode 或 config，对应后端设置选项的方法。
	Kind         string                 `json:"kind"`
	ID           string                 `json:"id"`
	Label        string                 `json:"label"`
	CurrentValue string                 `json:"current_value"`
	Options      []ExternalOptionChoice `json:"options"`
}

// ExternalOptionChoice 是外部会话选项的一个可选值。
type ExternalOptionChoice struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ExternalOptionSelection 指定要写入外部会话的选项值。
type ExternalOptionSelection struct {
	Kind string `json:"kind"`
	// ID 是后端的选项组 ID；Kind 为 model 时可省略。
	ID    string `json:"id,omitempty"`
	Value string `json:"value"`
}

// ExternalCommand 是外部后端上报的斜杠命令。
type ExternalCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputHint   string `json:"input_hint,omitempty"`
}

// SessionCtxMeta 是一段工作所属的对话、Session 和 Agent，只沿 ctx 往下传、不保存；请求头变量、日志和 span 的身份都从这里读。
// 只在对话层的工作只填 ConversationID。
type SessionCtxMeta struct {
	ConversationID  ConversationID
	SessionID       SessionID
	ParentSessionID SessionID
	AgentName       string
}

// sessionCtxMetaKey 是 ctx 上保存 SessionCtxMeta 的键。
type sessionCtxMetaKey struct{}

// ContextWithSessionCtxMeta 把 meta 里非空的字段合进 ctx 已有的 SessionCtxMeta，同名字段以后放入的为准。
func ContextWithSessionCtxMeta(ctx context.Context, meta SessionCtxMeta) context.Context {
	// 1. 在 ctx 已有的 SessionCtxMeta 上逐个覆盖非空字段。
	merged := GetSessionCtxMeta(ctx)
	if meta.ConversationID != "" {
		merged.ConversationID = meta.ConversationID
	}
	if meta.SessionID != "" {
		merged.SessionID = meta.SessionID
	}
	if meta.ParentSessionID != "" {
		merged.ParentSessionID = meta.ParentSessionID
	}
	if meta.AgentName != "" {
		merged.AgentName = meta.AgentName
	}
	// 2. 合并后的值放进新 ctx。
	return context.WithValue(ctx, sessionCtxMetaKey{}, merged)
}

// GetSessionCtxMeta 返回 ctx 里的 SessionCtxMeta；没放入的字段为空。
func GetSessionCtxMeta(ctx context.Context) SessionCtxMeta {
	// 1. 读取 ctx 里的值，没放入过时返回零值。
	meta, _ := ctx.Value(sessionCtxMetaKey{}).(SessionCtxMeta)
	return meta
}

// SessionCheckpoint 保存一个 Session 尚未处理完的待办。
type SessionCheckpoint struct {
	// PendingUserInputs 按出队顺序保存未分轮的用户输入批；没有待处理用户输入时为空。
	PendingUserInputs []SessionInputBatch `json:"pending_user_inputs,omitempty"`
	// PendingAgentMessages 按顺序保存未分轮的 Agent 消息批（含子任务结果）；没有待处理 Agent 消息时为空。
	PendingAgentMessages []SessionInputBatch `json:"pending_agent_messages,omitempty"`
	// TurnInputs 是已分配到一轮、尚未交接完的输入；nil 表示没有这种待办，不表示 Turn 已结束。
	TurnInputs *SessionTurnInputs `json:"turn_inputs,omitempty"`
	// StopRequestedTurnID 是用户叫停的那一轮；空值表示没有待处理的停止请求。
	StopRequestedTurnID TurnID `json:"stop_requested_turn_id,omitempty"`
	// IsAgentMessagesPaused 是否禁止仅由 Agent 消息开轮？只在根 Session 用户叫停后保持，新用户输入解除。
	IsAgentMessagesPaused bool `json:"is_agent_messages_paused,omitempty"`
}

// SessionInputBatch 保存准备一起开一轮的各组输入，保持先后顺序。
type SessionInputBatch []SessionInputGroup

// SessionInputGroup 保存一次提交的原消息和随消息读取的正文。
type SessionInputGroup struct {
	// InputID 是提交方可选的重试标识；空值不参与去重，不是消息自己的 ItemID。
	InputID InputID `json:"input_id,omitempty"`
	// Items 第一项是消息，后面是提交时已读取的 skill 正文；保留各自的原 ItemID 和顺序。
	Items []*Item `json:"items"`
}

// SessionTurnInputs 保存已分配到原 Turn 的待交接输入。
type SessionTurnInputs struct {
	// TurnID 是这批输入已分配到的轮次，重启不重新分配。
	TurnID TurnID `json:"turn_id"`
	// OpeningInput 是启动该轮的第一组输入。交接完即为 nil，不从补充输入中递补。
	OpeningInput *SessionInputGroup `json:"opening_input,omitempty"`
	// SteerInputs 是该轮未交接的补充输入，依投递顺序逐组交接；没有时为空。
	SteerInputs []SessionInputGroup `json:"steer_inputs,omitempty"`
}
