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
