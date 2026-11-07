package config

import (
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// ── Config 配置错误 ──

// Error 是一条配置错误，带出错的文件路径、字段名和原因。
type Error struct {
	Path   string
	Field  string
	Reason string
}

// Error 返回带配置位置和原因的错误文本。
func (e *Error) Error() string {
	// 1. 把文件路径、字段名（有的话）和原因拼成一行。
	location := e.Path
	if e.Field != "" {
		location += ": " + e.Field
	}
	return "configuration: " + location + ": " + e.Reason
}

// ── HTTP 内置默认请求头 ──

// defaultHTTPHeaders 是发给所有外部请求的内置 header 模板；config.yaml 的 http.headers 按名覆盖，写成 "" 表示不发。
var defaultHTTPHeaders = map[string]string{
	"User-Agent":      "openvaala/${OPENVAALA_VERSION} (${OPENVAALA_OS}; ${OPENVAALA_ARCH})",
	"Session-Id":      "${OPENVAALA_SESSION_ID}",
	"Conversation-Id": "${OPENVAALA_CONVERSATION_ID}",
}

// ── Agent 文件配置 ──

// defaultAgentName 是内置 default Agent 的名字，也是 run.agent_name 的默认值。
const defaultAgentName = "default"

// AgentConfig 是 agents/ 下一个 Agent 文件的内容；agents/default.yaml 没写的字段沿用内置 default Agent。
type AgentConfig struct {
	// Name 必须和文件名一致。
	Name string `yaml:"name"`
	// Description 是 Agent 的用途说明。
	Description string `yaml:"description,omitempty"`
	// Instructions 是 Agent 的指令，不能为空。
	Instructions string `yaml:"instructions"`
	// Model 是 Agent 的主模型；省略时加载后取默认模型，模型目录为空时保持为 nil。
	Model *AgentModelConfig `yaml:"model,omitempty"`
	// CompactModel 是生成上下文摘要使用的模型；省略时使用 Model。
	CompactModel *AgentModelConfig `yaml:"compact_model,omitempty"`
	// Tools 是可用普通工具名，由使用方的工具目录解释；省略时可用全部普通工具。
	Tools []string `yaml:"tools,omitempty"`
	// Skills 是可用 skill 名，由使用方的 skill 目录解释；省略时可用全部 skill。
	Skills []string `yaml:"skills,omitempty"`
	// MCPServers 是可用的 MCP server id；省略时全部可用（包括客户端连接带来的 MCP server），[] 表示不用 MCP。
	MCPServers defs.OptionalStringList `yaml:"mcp_servers,omitempty"`
	// MaxSteps 是一轮最多调用模型的次数；省略或 0 表示不限，不能为负。
	MaxSteps int `yaml:"max_steps,omitempty"`
}

// AgentModelConfig 引用 Provider 中的模型别名。
type AgentModelConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model,omitempty"`
}

// ── Executor 文件配置 ──

// BuiltinExecutorID 是应用内置的虚拟 Executor（存放内置 skill，可做临时工作）的 ID，配置文件不可使用。
const BuiltinExecutorID = "openvaala"

// virtualWorkspaceDirectory 是 virtual Executor 没写 workspace_directory 时的虚拟根目录，builtin 也用它。
const virtualWorkspaceDirectory = "/workspace"

// ExecutorConfig 是 executors/ 下一个文件声明的额外 Executor。
type ExecutorConfig struct {
	ID string `yaml:"id"`
	// Kind 是 local（本机进程内执行）、virtual（go-bash 内存虚拟环境）或 ssh（经 SSH 访问远端机器）。
	Kind defs.ExecutorKind `yaml:"kind"`
	// Description 是给模型看的说明，模型据此选 Executor。
	Description string `yaml:"description"`
	// WorkspaceDirectory 是内置工具的根目录：local 是本机目录，virtual 是虚拟文件系统里的路径，ssh 是远端绝对路径。
	WorkspaceDirectory string `yaml:"workspace_directory"`
	// SSH 是 kind 为 ssh 时的连接参数，其他 kind 不使用。
	SSH *SSHExecutorConfig `yaml:"ssh,omitempty"`
}

// SSHExecutorConfig 是 SSH Executor 的连接参数。
type SSHExecutorConfig struct {
	// Address 是 [user@]host[:port]。
	Address string `yaml:"address"`
	// IdentityFile 是本机私钥路径，加载时展开 ~；为空时依次使用 ssh-agent 与默认私钥。
	IdentityFile string `yaml:"identity_file,omitempty"`
}

// ── MCP server 文件配置 ──

// MCPServerConfig 声明一个 MCP server：stdio 在 Executor 的工作目录里启动命令，http、sse 从 Executor 所在机器连接 URL；
// 工具以 mcp__<id>__<工具原名> 提供给模型。
type MCPServerConfig struct {
	ID      string            `yaml:"id"`
	Type    string            `yaml:"type"` // 传输类型：stdio、http 或 sse。
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	// Executors 是在哪些 Executor 上运行；省略时为 [local]。
	Executors []string `yaml:"executors,omitempty"`
	// Tools 是提供给模型的工具原名名单；省略时全部提供。
	Tools []string `yaml:"tools,omitempty"`
	// Required 为 true 时连不上让这一轮报错；省略时连不上就跳过。
	Required bool `yaml:"required,omitempty"`
	// Disabled 为 true 时保留文件但不使用。
	Disabled bool `yaml:"disabled,omitempty"`
	// StartupTimeoutSeconds 是一次连接最多等多久，省略时 30。
	StartupTimeoutSeconds int `yaml:"startup_timeout_seconds,omitempty"`
	// ToolTimeoutSeconds 是一次工具调用最多等多久，省略时 300。
	ToolTimeoutSeconds int `yaml:"tool_timeout_seconds,omitempty"`
	// Plugin 是声明这个 server 的插件；mcp_servers/ 里的文件为 nil。
	Plugin *defs.Plugin `yaml:"-"`
}

// ── Hook 文件配置 ──

// HookFileConfig 是 hooks/ 下的一个 yaml 文件：事件名到 matcher 组列表；所有文件里的 hook 合在一起生效。
type HookFileConfig struct {
	Hooks map[string][]HookMatcherConfig `yaml:"hooks"`
}

// HookMatcherConfig 是一个事件下的一组 hook 和它们共用的 matcher。
type HookMatcherConfig struct {
	// Matcher 按事件挑选命中的 hook；省略、空串或 * 表示全部匹配。
	Matcher string              `yaml:"matcher,omitempty"`
	Hooks   []HookHandlerConfig `yaml:"hooks"`
}

// HookHandlerConfig 是一个 hook：在目标机器上运行的命令或脚本。
type HookHandlerConfig struct {
	// Type 目前只支持 command。
	Type string `yaml:"type"`
	// Command 与 Script 必须且只能写一个：Command 在目标机器上原样执行，Script 是 serve 本机的脚本文件，运行前拷到目标机器。
	Command string `yaml:"command,omitempty"`
	Script  string `yaml:"script,omitempty"`
	// Args 只能和 Script 一起用，跟在脚本路径后面。
	Args []string `yaml:"args,omitempty"`
	// Timeout 是最多运行多少秒，省略时 60，写了必须为正数。
	Timeout *int `yaml:"timeout,omitempty"`
	// OnFailure 是出错时的处理：allow（默认）或 block；只能用于 PreToolUse 和 UserPromptSubmit。
	OnFailure string `yaml:"on_failure,omitempty"`
}

// ── Plugin 文件配置 ──

// PluginManifest 是插件根目录下的 .openvaala-plugin/plugin.yaml。
type PluginManifest struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version,omitempty"`
	Description string `yaml:"description,omitempty"`
	// Skills 是 skill 目录，相对插件根目录；省略时为 ./skills/，目录不存在就没有 skill。
	Skills     string                         `yaml:"skills,omitempty"`
	Hooks      map[string][]HookMatcherConfig `yaml:"hooks,omitempty"`
	MCPServers []*MCPServerConfig             `yaml:"mcp_servers,omitempty"`
}

// skillPluginManifest 读取 Codex 和 Claude 插件的名称、版本和技能路径。
type skillPluginManifest struct {
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Description string           `json:"description"`
	Skills      pluginSkillPaths `json:"skills"`
}

// pluginSkillPaths 接受插件 manifest 中单一路径或按顺序排列的多个路径。
type pluginSkillPaths []string

// PluginInstallRecord 是 plugin install 写在插件目录里的 .openvaala-install.yaml，plugin update 按它重新拉取。
type PluginInstallRecord struct {
	Source      string    `yaml:"source"`           // git 地址或本机绝对路径
	Ref         string    `yaml:"ref,omitempty"`    // 空时用默认分支
	Path        string    `yaml:"path,omitempty"`   // 插件在仓库里的目录；根目录时为空
	Commit      string    `yaml:"commit,omitempty"` // git 来源安装时的 commit
	InstalledAt time.Time `yaml:"installed_at"`
}

// ── Plugin 命令请求与结果 ──

// InstallPluginRequest 是 plugin install 的参数。
type InstallPluginRequest struct {
	// Source 是 git 地址或已存在的本机目录；是本机目录时直接拷贝，不用 git。
	Source string
	// Path 是插件在仓库里的目录，必须是仓库内的相对路径；为空时是仓库根目录。
	Path string
	// Ref 是 git clone 的分支或 tag，只能用于 git 地址；为空时用默认分支。
	Ref string
}

// PluginListEntry 是 plugin list 的一行；Error 非空时这一项不是可用的插件。
type PluginListEntry struct {
	// Name 是 plugins 目录下的条目名；Version 取自 manifest，读不到 manifest 时为空。
	Name, Version string
	// IsDisabled 名字是否写在 plugins.disabled 里？
	IsDisabled bool
	// Record 是 plugin install 写的安装记录；手动放入的为 nil。
	Record *PluginInstallRecord
	// Error 是这一项不能当插件用的原因，例如没有 manifest。
	Error string
}
