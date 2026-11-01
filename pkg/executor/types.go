package executor

import (
	"encoding/json"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
)

// ── Execution 输出流 ──

// ExecutionStreamType 是一段执行进度的来源。
type ExecutionStreamType string

// ExecutionStreamTypeTerminal 是命令的输出，stdout、stderr 合在一起，按到达顺序排列。
const ExecutionStreamTypeTerminal ExecutionStreamType = "terminal"

// ── Tool 执行结果 ──

// ToolExecutionResult 是工具一次调用的返回：工具结果和这次调用顺带读到的目录规则。
type ToolExecutionResult struct {
	// Result 是交给模型的工具结果。
	Result *defs.ToolResult
	// AgentsInstructions 是这次调用读到的目录 AGENTS.md，例如 read_file 读到的文件所在目录的规则。
	AgentsInstructions []*defs.AgentsInstruction
}

// ── ToolExecutor 事件 ──

// ExecutorEvent 是 ToolExecutor.Run 发给调用方的事件：工具执行进度、hook 运行记录和调用结果；发出后归调用方所有。
type ExecutorEvent interface {
	executorEvent()
}

// ── Execution 事件 ──

// ExecutionProgressEvent 由工具在 Execute 产生进度或输出时发送；ToolExecutor 转发完这次调用的全部进度后，才发送它的结果。
type ExecutionProgressEvent struct {
	CallID string              `json:"call_id"`
	Stream ExecutionStreamType `json:"stream"`
	// Content 是合法 JSON；terminal 是 JSON 字符串。
	Content json.RawMessage `json:"content"`
}

func (*ExecutionProgressEvent) executorEvent() {}

// ExecutionFinishedEvent 由 ToolExecutor 在一次调用结束、进度转发完毕后发送；每个调用发送一次，被取消也会发送。
type ExecutionFinishedEvent struct {
	// Result 是这次调用的最终结果；工具不存在、参数错误、执行失败和取消都写在 Result.Error 里。
	// Result.Executor 由 ToolExecutor 填入实际执行环境的 ID；协作工具和客户端 MCP 工具为空。
	Result *defs.ToolResult
	// AgentsInstructions 是工具这次调用读到的目录规则，每条的 Executor 字段已填为实际执行环境的 ID。
	AgentsInstructions []*defs.AgentsInstruction
	// HookRuns 是 ToolExecutor 为这次调用触发的 PreToolUse、PostToolUse 要保存的运行记录，Pre 在前、Post 在后；
	// 没有触发 hook 或没有命中时为空。
	HookRuns []*defs.HookRun
}

func (*ExecutionFinishedEvent) executorEvent() {}

// ExecutionHookRunEvent 由 ToolExecutor 在 PreToolUse、PostToolUse hook 开始和结束时发送，同一个 hook 的运行记录 ID 不变；
// 总是先于对应调用的 ExecutionFinishedEvent。
type ExecutionHookRunEvent struct {
	Run *defs.HookRun
}

func (*ExecutionHookRunEvent) executorEvent() {}

// ── ToolExecutor 工具表 ──

// toolRoutes 是本轮的工具表：每个工具名的定义和提供方。
type toolRoutes struct {
	// environments 是建表时 ToolExecutor 的全部执行环境；routeCall 用它区分 executor 参数指向的环境不存在，还是这个环境不提供这个工具。
	environments []ExecutionEnvironment
	// routes 按工具名第一次登记的顺序排列：协作工具、各环境工具、客户端 MCP 工具。
	routes []*toolRoute
}

// toolRoute 是工具表里的一个工具名：定义取自第一个提供方。
type toolRoute struct {
	definition *defs.ToolDefinition
	// providers 按环境顺序排列；协作工具和客户端 MCP 工具只有一个提供方。
	providers []toolProvider
}

// toolProvider 是一个工具名的一处提供方：在哪个环境、由哪个工具对象执行。
type toolProvider struct {
	// environment 是提供工具的执行环境；协作工具和客户端 MCP 工具为 nil。
	environment ExecutionEnvironment
	tool        Tool
	// executorID 是工具在日志和遥测里的 executor ID，建工具表时填入：环境工具和环境 MCP 工具是环境 ID，
	// 客户端 MCP 工具是 defs.ClientExecutorID，协作工具为空。
	executorID string
}

// routedToolCall 是已经选好提供方的一次调用和它的 PreToolUse 结果。
type routedToolCall struct {
	// call 是去掉 executor 参数的调用；PreToolUse 改写参数后换成带新参数的副本。
	call     *defs.Item
	provider toolProvider
	// preOutcome 是 PreToolUse 的合并结果；没有触发 PreToolUse 时为 nil。
	preOutcome *hook.HookOutcome
}
