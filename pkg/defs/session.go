package defs

import "context"

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
