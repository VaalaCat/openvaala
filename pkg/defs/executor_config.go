package defs

// ExecutorConfig 是 executors/<id>.yaml 声明的额外 Executor 配置。
type ExecutorConfig struct {
	ID string `yaml:"id" json:"id"`
	// Kind 是 local（本机进程内执行）、virtual（go-bash 内存虚拟环境）或 ssh（经 SSH 访问远端机器）。
	Kind ExecutorKind `yaml:"kind" json:"kind"`
	// Description 是给模型看的说明，模型据此选 Executor。
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// WorkspaceDirectory 是内置工具的根目录：local 是本机目录，virtual 是虚拟文件系统里的路径，ssh 是远端绝对路径。
	WorkspaceDirectory string `yaml:"workspace_directory,omitempty" json:"workspace_directory,omitempty"`
	// SSH 是 kind 为 ssh 时的连接参数，其他 kind 不使用。
	SSH *SSHExecutorConfig `yaml:"ssh,omitempty" json:"ssh,omitempty"`
}

// SSHExecutorConfig 是 kind 为 ssh 时的连接参数。
type SSHExecutorConfig struct {
	// Address 是 [user@]host[:port]。
	Address string `yaml:"address" json:"address"`
	// IdentityFile 是 serve 所在机器上的私钥路径，加载时展开 ~；为空时依次用 ssh-agent 和默认私钥。
	IdentityFile string `yaml:"identity_file,omitempty" json:"identity_file,omitempty"`
}

// SSHConfigHost 是从 serve 机器 ~/.ssh/config 解析出的单条非通配符主机项，供客户端快捷填表。
type SSHConfigHost struct {
	// Alias 是 Host 段的名字，可直接作为默认 Executor ID。
	Alias string `json:"alias"`
	// Address 是按 User、HostName、Port 拼好的 [user@]host[:port]。
	Address string `json:"address"`
	// IdentityFile 是该 Host 配置的首个私钥路径（原样保留 ~ 便于阅读），未配置时为空。
	IdentityFile string `json:"identity_file,omitempty"`
}
