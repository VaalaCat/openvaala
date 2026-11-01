package defs

// ExecutorKind 是 Executor 的种类。
type ExecutorKind string

const (
	ExecutorKindLocal   ExecutorKind = "local"
	ExecutorKindSSH     ExecutorKind = "ssh"
	ExecutorKindVirtual ExecutorKind = "virtual"
)

// ExecutorDefinition 是一个 Executor 的定义：local 和 builtin 由加载配置时补上，其余来自 executors/。
type ExecutorDefinition struct {
	ID          string
	Kind        ExecutorKind
	Description string
	// WorkspaceDirectory 是对话设置里没给目录时用的根目录：local 是 server.workspace_directory，
	// 没配时是 serve 的启动目录；virtual 没配时是 /workspace。
	WorkspaceDirectory string
	// SSH 是 ssh 的连接参数，其他种类为 nil。
	SSH *SSHExecutorDefinition
	// Files 是 virtual 工作区建好时预先放进去的文件，键为绝对路径；builtin 用它放内置 skill。
	// 每个对话各拷一份，对话之间互不可见。
	Files map[string][]byte
}

// SSHExecutorDefinition 是 SSH Executor 的连接参数。
type SSHExecutorDefinition struct {
	// Address 是 [user@]host[:port]。
	Address string
	// IdentityFile 是本机私钥路径，已展开 ~；为空时依次使用 ssh-agent 与默认私钥。
	IdentityFile string
}
