package defs

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
