package prompt

// ── Executor 提示输入 ──

// ExecutorEntry 是系统提示里列出的一个 Executor；传入列表的第一项是默认 Executor。
type ExecutorEntry struct {
	ID                 string
	Description        string
	WorkspaceDirectory string
}

// ── Agent 提示输入 ──

// AgentEntry 是 spawn_agent 说明里列出的一个可选 Agent：名字和用途说明。
type AgentEntry struct {
	Name        string
	Description string
}

// ── XML 提示字段 ──

// xmlField 是 XML 提示块里的一个子字段，在切片里的顺序就是输出顺序。
type xmlField struct {
	name  string
	value string
}
