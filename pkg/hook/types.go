package hook

import (
	"encoding/json"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// ── Hooks 事件请求 ──

// HookEventRequest 是一次事件：发给 hook 的内容和运行它的 Executor。
type HookEventRequest struct {
	Event *HookEvent
	// ExecutorID 是运行 hook 的 Executor。
	ExecutorID string
	// Workspace 是这个 Executor 的工作区，命令在它的根目录里运行。
	Workspace workspace.Workspace
}

// HookEvent 由 ToolExecutor 在工具调用前后交给 Hooks，其他事件由 Session 或 Agent 在启动、提交输入或结束时交给 Hooks；
// 命令 hook 从 stdin 收到它的 JSON。Source 及之后的字段只在对应事件里填写，其他事件留空。
type HookEvent struct {
	Name    defs.HookEventName
	Session defs.Session
	TurnID  defs.TurnID
	// Source 是 SessionStart 的来源：startup 或 resume。
	Source string
	// Prompt 是 UserPromptSubmit 这组输入里的文本。
	Prompt string
	// ToolCall 是 PreToolUse、PostToolUse 的调用，已经过路由，参数不含 executor。
	ToolCall *defs.ToolCall
	// ToolResult 是 PostToolUse 的工具结果。
	ToolResult *defs.ToolResult
	// LastAssistantMessage 是 Stop、SubagentStop 时模型最后一条回复的文本。
	LastAssistantMessage string
	// IsStopHookActive 这一轮是不是已经被 Stop hook 续过？只用于 Stop、SubagentStop。
	IsStopHookActive bool
}

// ── Hooks 事件结果 ──

// HookOutcome 是一个事件上所有命中 hook 合并后的决定；触发事件的那一层据此改流程。
type HookOutcome struct {
	// IsBlocked 是否有 hook 拦下了这个事件？
	IsBlocked bool
	// BlockReason 是拦下原因，多个时按配置顺序用空行拼接。
	BlockReason string
	// UpdatedToolArguments 只有 PreToolUse 会填，替换 defs.ToolCall.Arguments；多个 hook 都改写时取配置顺序最后的。
	UpdatedToolArguments string
	// IsTurnStopped 是否有 hook 要求结束本轮？
	IsTurnStopped bool
	// StopReason 是结束本轮的原因，只给用户看；多个时按配置顺序用空行拼接。
	StopReason string
	// SavedRuns 是要保存进历史的运行记录，按配置顺序；正常完成且没有任何输出的不在其中。
	SavedRuns []*defs.HookRun
}

// ── Hooks 配置定义 ──

// HookDefinition 是校验后的一个 hook。
type HookDefinition struct {
	// Key 标识配置中的 hook：文件项为 <文件名>:<事件>:<序号>，插件项以 plugin:<插件名>: 开头。
	Key   string
	Event defs.HookEventName
	// Matcher 按事件挑选命中的 hook，见 HookMatcher。
	Matcher *HookMatcher
	// Command 与 ScriptPath 二选一；ScriptPath 是 serve 本机的绝对路径。
	Command    string
	ScriptPath string
	// Args 是跟在 script 路径后面的参数。
	Args []string
	// Timeout 是最多运行多久。
	Timeout time.Duration
	// IsBlockedOnFailure 出错时是否按拦下处理？只用于 PreToolUse 和 UserPromptSubmit；本轮被中断不算出错。
	IsBlockedOnFailure bool
	// SourcePath 是声明这个 hook 的文件：hooks/ 下的 yaml 或插件的 manifest。
	SourcePath string
	// Plugin 是 hook 所属的插件；hooks/*.yaml 里的为 nil。插件的 script 在插件根目录内。
	Plugin *defs.Plugin
	// DisplayOrder 是展示顺序，也是运行结果的合并顺序：文件 hook 在插件 hook 前，各自按名字排序，内部按固定事件、matcher 组和 hook 顺序。
	DisplayOrder int
}

// HookInfo 是一个配置好的 hook，供 hooks/list 显示。
type HookInfo struct {
	// Key 是 HookDefinition.Key。
	Key   string             `json:"key"`
	Event defs.HookEventName `json:"event"`
	// Matcher 是配置里 matcher 的原文，没写时为空。
	Matcher string `json:"matcher,omitempty"`
	// Handler 是显示用的命令：command 原文，或 script 路径加 args。
	Handler        string `json:"handler"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	SourcePath     string `json:"source_path"`
	DisplayOrder   int    `json:"display_order"`
	// CurrentHash 是这个 hook 定义的哈希：定义不变则不变，改了事件、matcher、命令、脚本、参数、超时或 on_failure 就变。
	CurrentHash string `json:"current_hash"`
	// PluginName 是 hook 所属插件的名字；用户自己的 hook 为空。
	PluginName string `json:"plugin_name,omitempty"`
}

// ── Hooks 脚本拷贝 ──

// scriptCopyKey 标识一次脚本拷贝：某个 Executor 上某个文件名、某份权限位加内容（SHA-256 十六进制）的脚本。
type scriptCopyKey struct {
	executorID string
	fileName   string
	hash       string
}

// scriptCopy 是一个 Executor 上一份脚本的拷贝任务：第一个需要它的运行负责拷贝，其余的运行等 done 关闭后读取结果。
// targetPath 和 err 由负责拷贝的运行在关闭 done 之前写入，done 关闭后只读。
type scriptCopy struct {
	done chan struct{}
	// targetPath 是脚本在目标机器上的绝对路径。
	targetPath string
	err        error
}

// ── Hooks 插件目录拷贝 ──

// pluginCopyKey 标识一次插件目录拷贝：某个 Executor 上的某个插件。一份 Hooks 对应一版配置，插件内容在这版配置里视为不变。
type pluginCopyKey struct {
	executorID string
	pluginName string
}

// pluginCopy 是一个 Executor 上一个插件目录的拷贝任务：第一个需要它的运行负责算哈希和拷贝，其余的运行等 done 关闭后读取结果。
// targetDirectory 和 err 由负责拷贝的运行在关闭 done 之前写入，done 关闭后只读。
type pluginCopy struct {
	done chan struct{}
	// targetDirectory 是插件根目录在目标机器上的绝对路径。
	targetDirectory string
	err             error
}

// ── Hooks 命令输出 ──

// commandOutput 是命令 hook 在退出码 0 时 stdout 里的 JSON；不认识的字段忽略。
type commandOutput struct {
	Decision           string                `json:"decision"`
	Reason             string                `json:"reason"`
	Continue           *bool                 `json:"continue"`
	StopReason         string                `json:"stopReason"`
	SystemMessage      string                `json:"systemMessage"`
	HookSpecificOutput commandSpecificOutput `json:"hookSpecificOutput"`
}

// commandSpecificOutput 是 commandOutput 里按事件区分的部分。
type commandSpecificOutput struct {
	PermissionDecision       string          `json:"permissionDecision"`
	PermissionDecisionReason string          `json:"permissionDecisionReason"`
	UpdatedInput             json.RawMessage `json:"updatedInput"`
	AdditionalContext        string          `json:"additionalContext"`
}

// commandResult 是命令跑到结束的结果：退出码和两个输出端保留下来的内容。
type commandResult struct {
	exitCode int
	stdout   *limitedBuffer
	stderr   *limitedBuffer
}
