package defs

// ExecutorKind 是 Executor 的种类；自定义 Executor 可以用自己的种类。
type ExecutorKind string

const (
	ExecutorKindLocal   ExecutorKind = "local"
	ExecutorKindSSH     ExecutorKind = "ssh"
	ExecutorKindVirtual ExecutorKind = "virtual"
	// ExecutorKindMemory 是内置的跨对话记忆 Executor；没有工作区目录，不能当默认 Executor，工作区要实现 workspace.MemoryWorkspace。
	ExecutorKindMemory ExecutorKind = "memory"
)

// ExecutorDefinition 是一个 Executor 的 ID、种类、说明和默认目录。
type ExecutorDefinition struct {
	ID          string
	Kind        ExecutorKind
	Description string
	// WorkspaceDirectory 是对话设置里没给目录时用的根目录：local 是 server.workspace_directory，
	// 没配时是 serve 的启动目录；virtual 没配时是 /workspace。
	WorkspaceDirectory string
}
