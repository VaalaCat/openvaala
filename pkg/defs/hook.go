package defs

// HookEventName 是触发 hook 的时刻。
type HookEventName string

const (
	HookEventSessionStart     HookEventName = "SessionStart"
	HookEventUserPromptSubmit HookEventName = "UserPromptSubmit"
	HookEventPreToolUse       HookEventName = "PreToolUse"
	HookEventPostToolUse      HookEventName = "PostToolUse"
	HookEventStop             HookEventName = "Stop"
	HookEventSubagentStart    HookEventName = "SubagentStart"
	HookEventSubagentStop     HookEventName = "SubagentStop"
)

// HookRunStatus 是一次 hook 运行所处的阶段或结局。
type HookRunStatus string

const (
	HookRunRunning   HookRunStatus = "running"
	HookRunCompleted HookRunStatus = "completed"
	HookRunBlocked   HookRunStatus = "blocked"
	HookRunFailed    HookRunStatus = "failed"
	// HookRunStopped 表示 hook 回了 continue: false。
	HookRunStopped HookRunStatus = "stopped"
)

// HookRun 是一个 hook 的一次运行：开始时为 running，结束后更新为最终状态和输出；有效果的运行作为 hook_run Item 保存。
type HookRun struct {
	ID    HookRunID     `json:"id"`
	Event HookEventName `json:"event"`
	// CallID 是工具事件对应的调用。
	CallID string `json:"call_id,omitempty"`
	// ExecutorID 是运行这个 hook 的 Executor。
	ExecutorID string `json:"executor_id"`
	// SourcePath 是定义这个 hook 的配置文件。
	SourcePath string `json:"source_path"`
	// Handler 是显示用的命令：command 原文，或 script 路径加 args。
	Handler      string        `json:"handler"`
	DisplayOrder int           `json:"display_order"`
	Status       HookRunStatus `json:"status"`
	// StartedAt 和 CompletedAt 是 Unix 毫秒；运行中时 CompletedAt 为 0。
	StartedAt   int64 `json:"started_at"`
	CompletedAt int64 `json:"completed_at,omitempty"`

	BlockReason          string `json:"block_reason,omitempty"`
	UpdatedToolArguments string `json:"updated_tool_arguments,omitempty"`
	AdditionalContext    string `json:"additional_context,omitempty"`
	StopReason           string `json:"stop_reason,omitempty"`
	SystemMessage        string `json:"system_message,omitempty"`
	// ErrorMessage 在 failed 时说明原因，例如超时、脚本不存在。
	ErrorMessage string `json:"error_message,omitempty"`
}

// Clone 复制 hook 运行记录。
func (run *HookRun) Clone() *HookRun {
	// 1. nil 返回 nil，否则复制记录中的全部字段。
	if run == nil {
		return nil
	}
	copy := *run
	return &copy
}
