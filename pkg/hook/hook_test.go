package hook_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

const testSourcePath = "/etc/openvaala/hooks/test.yaml"

var (
	rootSession = defs.Session{
		ID:             "session-root",
		ConversationID: "conversation-1",
		AgentName:      "main",
	}
	childSession = defs.Session{
		ID:              "session-child",
		ConversationID:  "conversation-1",
		AgentName:       "explorer",
		ParentSessionID: "session-root",
	}
)

// ── 测试夹具 ──

// definitionOption 修改测试用 hook 定义里的一项。
type definitionOption func(*hook.HookDefinition)

// withOrder 返回设置 DisplayOrder 的选项。
func withOrder(order int) definitionOption {
	// 1. 返回写入 DisplayOrder 的函数。
	return func(definition *hook.HookDefinition) { definition.DisplayOrder = order }
}

// withTimeout 返回设置 Timeout 的选项。
func withTimeout(timeout time.Duration) definitionOption {
	// 1. 返回写入 Timeout 的函数。
	return func(definition *hook.HookDefinition) { definition.Timeout = timeout }
}

// withBlockOnFailure 返回打开 IsBlockedOnFailure 的选项。
func withBlockOnFailure() definitionOption {
	// 1. 返回将 IsBlockedOnFailure 设为 true 的函数。
	return func(definition *hook.HookDefinition) { definition.IsBlockedOnFailure = true }
}

// newTestDefinition 创建测试用的 command hook 定义。
func newTestDefinition(t *testing.T, event defs.HookEventName, pattern, command string, options ...definitionOption) *hook.HookDefinition {
	t.Helper()
	// 1. 编译 matcher，设置默认顺序 1 和超时 10 秒，再应用自定义选项。
	matcher, err := hook.NewHookMatcher(pattern)
	if err != nil {
		t.Fatal(err)
	}
	definition := &hook.HookDefinition{
		Event:        event,
		Matcher:      matcher,
		Command:      command,
		Timeout:      10 * time.Second,
		SourcePath:   testSourcePath,
		DisplayOrder: 1,
	}
	for _, option := range options {
		option(definition)
	}
	return definition
}

// newTestScriptDefinition 创建测试用的 script hook 定义。
func newTestScriptDefinition(t *testing.T, event defs.HookEventName, scriptPath string, args []string, options ...definitionOption) *hook.HookDefinition {
	t.Helper()
	// 1. 在空 command 的定义上填入脚本路径和参数。
	definition := newTestDefinition(t, event, "", "", options...)
	definition.ScriptPath = scriptPath
	definition.Args = args
	return definition
}

// argumentRecorderScript 把自己的 $0 和收到的每个参数各占一行写进工作区根目录的 script.out。
const argumentRecorderScript = "#!/bin/sh\n{\nprintf '%s\\n' \"$0\"\nprintf '%s\\n' \"$@\"\n} > script.out\n"

// writeTestScript 写入可执行的测试脚本，返回它的路径。
func writeTestScript(t *testing.T, directory, name, content string) string {
	t.Helper()
	// 1. 在指定目录写入权限为 0755 的脚本。
	scriptPath := filepath.Join(directory, name)
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath
}

// getCopiedScriptPath 算出测试脚本在 home 缓存目录里的副本路径。
func getCopiedScriptPath(t *testing.T, home, scriptPath string) string {
	t.Helper()
	// 1. 用权限和内容算 SHA-256，前 16 位做子目录名，文件名不变。
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte(fmt.Sprintf("%04o\x00", info.Mode().Perm())), content...))
	return filepath.Join(home, ".cache", "openvaala", "hooks", hex.EncodeToString(sum[:])[:16], filepath.Base(scriptPath))
}

// getExpectedHandler 生成运行记录里期望的 Handler。
func getExpectedHandler(definition *hook.HookDefinition) string {
	// 1. command 返回原文，script 返回路径加空格连接的参数。
	if definition.ScriptPath == "" {
		return definition.Command
	}
	return strings.TrimSpace(definition.ScriptPath + " " + strings.Join(definition.Args, " "))
}

// printJSON 生成把 output 原样打印到 stdout 的命令。
func printJSON(output string) string {
	// 1. 用 printf 拼出命令。
	return "printf '%s' '" + output + "'"
}

// newPreToolUseEvent 创建测试用的 PreToolUse 事件。
func newPreToolUseEvent(session defs.Session, toolName, arguments string) *hook.HookEvent {
	// 1. 填入 Session、Turn、调用 ID、工具名和参数。
	return &hook.HookEvent{
		Name:    defs.HookEventPreToolUse,
		Session: session,
		TurnID:  "turn-1",
		ToolCall: &defs.ToolCall{
			CallID:    "call-1",
			Name:      toolName,
			Arguments: arguments,
		},
	}
}

// newPostToolUseEvent 创建测试用的 PostToolUse 事件，在根 Session 上。
func newPostToolUseEvent(toolName, arguments, content string) *hook.HookEvent {
	// 1. 在 PreToolUse 事件上改事件名，补上工具结果。
	event := newPreToolUseEvent(rootSession, toolName, arguments)
	event.Name = defs.HookEventPostToolUse
	event.ToolResult = &defs.ToolResult{
		CallID:  "call-1",
		Content: json.RawMessage(content),
	}
	return event
}

// newStopEvent 创建测试用的 Stop 事件，在根 Session 上。
func newStopEvent() *hook.HookEvent {
	// 1. 填入根 Session、Turn 和最后回复。
	return &hook.HookEvent{
		Name:                 defs.HookEventStop,
		Session:              rootSession,
		TurnID:               "turn-1",
		LastAssistantMessage: "done",
	}
}

// newSessionStartEvent 创建测试用的 SessionStart 事件，在根 Session 上。
func newSessionStartEvent(source string) *hook.HookEvent {
	// 1. 填入根 Session、Turn 和来源。
	return &hook.HookEvent{
		Name:    defs.HookEventSessionStart,
		Session: rootSession,
		TurnID:  "turn-1",
		Source:  source,
	}
}

// newUserPromptSubmitEvent 创建测试用的 UserPromptSubmit 事件，在根 Session 上。
func newUserPromptSubmitEvent() *hook.HookEvent {
	// 1. 填入根 Session、Turn 和用户文本。
	return &hook.HookEvent{
		Name:    defs.HookEventUserPromptSubmit,
		Session: rootSession,
		TurnID:  "turn-1",
		Prompt:  "fix the bug",
	}
}

// collectRuns 读完 runUpdates，核对每次运行先发 running 再发最终记录，按 DisplayOrder 返回最终记录。
func collectRuns(t *testing.T, runUpdates <-chan *defs.HookRun) []*defs.HookRun {
	t.Helper()
	// 1. 按 ID 分组收集，每组必须正好一条 running 加一条最终记录。
	sequences := map[defs.HookRunID][]*defs.HookRun{}
	for run := range runUpdates {
		sequences[run.ID] = append(sequences[run.ID], run)
	}
	var finals []*defs.HookRun
	for id, sequence := range sequences {
		if len(sequence) != 2 || sequence[0].Status != defs.HookRunRunning || sequence[0].CompletedAt != 0 || sequence[1].Status == defs.HookRunRunning {
			t.Fatalf("run %s updates = %+v, want one running record followed by one final record", id, sequence)
		}
		finals = append(finals, sequence[1])
	}
	slices.SortFunc(finals, func(a, b *defs.HookRun) int { return a.DisplayOrder - b.DisplayOrder })
	return finals
}

// getRunResult 只留下运行记录里的结果字段。
func getRunResult(run *defs.HookRun) defs.HookRun {
	// 1. 复制状态、拦下原因、改写参数、附加上下文、结束原因、用户消息和错误。
	return defs.HookRun{
		Status:               run.Status,
		BlockReason:          run.BlockReason,
		UpdatedToolArguments: run.UpdatedToolArguments,
		AdditionalContext:    run.AdditionalContext,
		StopReason:           run.StopReason,
		SystemMessage:        run.SystemMessage,
		ErrorMessage:         run.ErrorMessage,
	}
}

// isErrorMatched 错误文本是否匹配期望值？
func isErrorMatched(got, want string) bool {
	// 1. 期望以 * 结尾时比较前缀，其余比较完整文本。
	if prefix, ok := strings.CutSuffix(want, "*"); ok {
		return strings.HasPrefix(got, prefix)
	}
	return got == want
}

// TestHooksRunEvent 验证 RunEvent：hook 的 stdin、退出码和输出的解析、超时和中断、matcher 匹配、多个 hook 的合并，以及 script 从缓存副本运行。
func TestHooksRunEvent(t *testing.T) {
	// 1. 准备把 stdin 写进 input.json 的命令和 1 MiB 的工具参数。
	const input = "cat > input.json"
	largeArguments := fmt.Sprintf(`{"content":%q}`, strings.Repeat("x", 1<<20))
	// 2. 把 HOME 指向临时目录，脚本副本会拷到其中的缓存目录；写好测试脚本。
	home := t.TempDir()
	t.Setenv("HOME", home)
	scriptDirectory := t.TempDir()
	guardPath := writeTestScript(t, scriptDirectory, "guard.sh", argumentRecorderScript)
	spacedGuardPath := writeTestScript(t, scriptDirectory, "my guard.sh", argumentRecorderScript)
	// checkScriptOutput 断言脚本是从缓存目录里的拷贝执行的，权限位沿用本机文件，$0 之后每行是一个收到的参数。
	checkScriptOutput := func(scriptPath, wantArguments string) func(t *testing.T, root string, outcome *hook.HookOutcome, runs []*defs.HookRun) {
		return func(t *testing.T, root string, outcome *hook.HookOutcome, runs []*defs.HookRun) {
			t.Helper()
			copiedPath := getCopiedScriptPath(t, home, scriptPath)
			output, err := os.ReadFile(filepath.Join(root, "script.out"))
			if err != nil {
				t.Fatal(err)
			}
			if want := copiedPath + "\n" + wantArguments; string(output) != want {
				t.Fatalf("script.out = %q, want %q", output, want)
			}
			info, err := os.Stat(copiedPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("copied script mode = %v, want 0755", info.Mode().Perm())
			}
		}
	}

	// 3. 准备 stdin、退出码、JSON 输出、纯文本输出、stderr、超时和中断、matcher、合并和脚本的样例。
	postToolUseImageEvent := newPostToolUseEvent("read_file", `{"path":"a.png"}`, `{"path":"a.png","mime_type":"image/png","size":4}`)
	postToolUseImageEvent.ToolResult.Images = []*defs.ImageContent{{
		DataURL: "data:image/png;base64,AAAA",
		Alt:     "a.png",
	}}

	tests := []struct {
		name        string
		definitions []*hook.HookDefinition
		event       *hook.HookEvent
		// cancelAfter 大于 0 时，在这段时间后取消 ctx。
		cancelAfter time.Duration
		// maxDuration 大于 0 时，RunEvent 必须在这段时间内返回。
		maxDuration time.Duration
		// wantOutcome 不含 SavedRuns；SavedRuns 的条数由 wantSaved 给出。
		wantOutcome hook.HookOutcome
		wantSaved   int
		// wantRuns 按 DisplayOrder 排列，只比较结局字段；ErrorMessage 以 * 结尾时只比较前缀，
		// AdditionalContext 为 * 时不比较，由 check 另行断言。
		wantRuns []defs.HookRun
		// wantStderrLog 非空时，唯一那次运行的日志带有 stderr 字段且等于它；为空时所有运行的日志都没有 stderr 字段。
		wantStderrLog string
		// wantInput 非空时，hook 命令把 stdin 写到了根目录的 input.json，内容应与它的 JSON 相等；$ROOT 代表根目录。
		wantInput string
		// isTerminateFailing 为 true 时，工作区启动的命令 Terminate 总是失败，只有 Close 能结束它。
		isTerminateFailing bool
		check              func(t *testing.T, root string, outcome *hook.HookOutcome, runs []*defs.HookRun)
	}{
		// hook 标准输入样例
		{
			name:        "stdin of PreToolUse in a root Session",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", input)},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"PreToolUse","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"tool_name":"bash","tool_input":{"command":"ls"},"tool_use_id":"call-1"}`,
		},
		{
			name:        "stdin of PreToolUse in a child Session carries the agent",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", input)},
			event:       newPreToolUseEvent(childSession, "bash", `{"command":"ls"}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"PreToolUse","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"agent_id":"session-child","agent_type":"explorer",
				"tool_name":"bash","tool_input":{"command":"ls"},"tool_use_id":"call-1"}`,
		},
		{
			name:        "stdin of PreToolUse with arguments that are not JSON",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", input)},
			event:       newPreToolUseEvent(rootSession, "bash", `{bad`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"PreToolUse","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"tool_name":"bash","tool_input":"{bad","tool_use_id":"call-1"}`,
		},
		{
			name:        "stdin of PostToolUse carries the tool result content",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPostToolUse, "", input)},
			event:       newPostToolUseEvent("apply_patch", `{"patch":"x"}`, `{"outcome":"ok","added":1}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"PostToolUse","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"tool_name":"apply_patch","tool_input":{"patch":"x"},"tool_use_id":"call-1","tool_response":{"outcome":"ok","added":1}}`,
		},
		{
			// 工具结果带图片时，tool_response 只取 Content，图片数据不会写进 hook 的 stdin。
			name:        "stdin of PostToolUse with a tool result image carries no image data",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPostToolUse, "", input)},
			event:       postToolUseImageEvent,
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"PostToolUse","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"tool_name":"read_file","tool_input":{"path":"a.png"},"tool_use_id":"call-1","tool_response":{"path":"a.png","mime_type":"image/png","size":4}}`,
			check: func(t *testing.T, root string, outcome *hook.HookOutcome, runs []*defs.HookRun) {
				t.Helper()
				content, err := os.ReadFile(filepath.Join(root, "input.json"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(content), "data:image") || !strings.Contains(string(content), "tool_response") {
					t.Fatalf("stdin = %s, want tool_response without data:image", content)
				}
			},
		},
		{
			name:        "stdin of Stop",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventStop, "", input)},
			event:       newStopEvent(),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"Stop","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"stop_hook_active":false,"last_assistant_message":"done"}`,
		},
		{
			name:        "stdin of SubagentStop in a child Session",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventSubagentStop, "", input)},
			event: &hook.HookEvent{
				Name:                 defs.HookEventSubagentStop,
				Session:              childSession,
				TurnID:               "turn-1",
				LastAssistantMessage: "done",
				IsStopHookActive:     true,
			},
			wantRuns: []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"SubagentStop","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"agent_id":"session-child","agent_type":"explorer","stop_hook_active":true,"last_assistant_message":"done"}`,
		},
		{
			name:        "stdin of SessionStart",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventSessionStart, "", input)},
			event:       newSessionStartEvent("startup"),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"SessionStart","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"source":"startup"}`,
		},
		{
			name:        "stdin of UserPromptSubmit",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventUserPromptSubmit, "", input)},
			event:       newUserPromptSubmitEvent(),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantInput: `{"hook_event_name":"UserPromptSubmit","session_id":"conversation-1","turn_id":"turn-1","cwd":"$ROOT",
				"prompt":"fix the bug"}`,
		},

		// 退出码
		{
			name:        "exit 0 without output is transparent",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "exit 2 blocks with stderr as the reason",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "echo no >&2; exit 2")},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "no",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:      defs.HookRunBlocked,
				BlockReason: "no",
			}},
		},
		{
			name:        "exit 2 without stderr uses the default reason",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventUserPromptSubmit, "", "exit 2")},
			event:       newUserPromptSubmitEvent(),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "hook exited with status 2",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:      defs.HookRunBlocked,
				BlockReason: "hook exited with status 2",
			}},
		},
		{
			name:        "exit 2 on SessionStart fails without blocking",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventSessionStart, "", "echo boom >&2; exit 2")},
			event:       newSessionStartEvent("startup"),
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "exit status 2: boom",
			}},
		},
		{
			name:        "exit 1 fails and lets the event through",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "echo bad >&2; exit 1")},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "exit status 1: bad",
			}},
		},
		{
			name:        "exit 1 blocks when the hook is blocked on failure",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "echo bad >&2; exit 1", withBlockOnFailure())},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "hook failed: exit status 1: bad",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "exit status 1: bad",
			}},
		},

		// JSON 输出
		{
			name: "permissionDecision deny blocks PreToolUse",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "",
				printJSON(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no rm"}}`))},
			event: newPreToolUseEvent(rootSession, "bash", `{"command":"rm -rf build"}`),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "no rm",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:      defs.HookRunBlocked,
				BlockReason: "no rm",
			}},
		},
		{
			name: "decision block on PreToolUse is the same as deny",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "",
				printJSON(`{"decision":"block","reason":"old style"}`))},
			event: newPreToolUseEvent(rootSession, "bash", `{"command":"rm -rf build"}`),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "old style",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:      defs.HookRunBlocked,
				BlockReason: "old style",
			}},
		},
		{
			name: "decision block on Stop keeps the model going",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventStop, "",
				printJSON(`{"decision":"block","reason":"tests are failing"}`))},
			event: newStopEvent(),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "tests are failing",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:      defs.HookRunBlocked,
				BlockReason: "tests are failing",
			}},
		},
		{
			name: "decision block without a reason uses the default reason",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventStop, "",
				printJSON(`{"decision":"block"}`))},
			event: newStopEvent(),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "blocked by hook",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:      defs.HookRunBlocked,
				BlockReason: "blocked by hook",
			}},
		},
		{
			name: "allow with updatedInput rewrites the arguments",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "",
				printJSON(`{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"command":"trash build"}}}`))},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"rm -rf build"}`),
			wantOutcome: hook.HookOutcome{UpdatedToolArguments: `{"command":"trash build"}`},
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:               defs.HookRunCompleted,
				UpdatedToolArguments: `{"command":"trash build"}`,
			}},
		},
		{
			name: "ask is a hook error",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "",
				printJSON(`{"hookSpecificOutput":{"permissionDecision":"ask"}}`))},
			event:     newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: `unsupported permissionDecision "ask"`,
			}},
		},
		{
			name: "allow alone is transparent",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "",
				printJSON(`{"hookSpecificOutput":{"permissionDecision":"allow"}}`))},
			event:    newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns: []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name: "continue false stops the turn",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPostToolUse, "",
				printJSON(`{"continue":false,"stopReason":"done"}`))},
			event: newPostToolUseEvent("bash", `{"command":"ls"}`, `{"exit_code":0}`),
			wantOutcome: hook.HookOutcome{
				IsTurnStopped: true,
				StopReason:    "done",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:     defs.HookRunStopped,
				StopReason: "done",
			}},
		},
		{
			name: "stopped takes priority over blocked",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventStop, "",
				printJSON(`{"decision":"block","reason":"again","continue":false,"stopReason":"enough"}`))},
			event: newStopEvent(),
			wantOutcome: hook.HookOutcome{
				IsBlocked:     true,
				BlockReason:   "again",
				IsTurnStopped: true,
				StopReason:    "enough",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:      defs.HookRunStopped,
				BlockReason: "again",
				StopReason:  "enough",
			}},
		},
		{
			name: "systemMessage is saved for the user",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "",
				printJSON(`{"systemMessage":"careful"}`))},
			event:     newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:        defs.HookRunCompleted,
				SystemMessage: "careful",
			}},
		},
		{
			name: "additionalContext is saved for the model",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPostToolUse, "",
				printJSON(`{"hookSpecificOutput":{"additionalContext":"gofmt changed 2 files"}}`))},
			event:     newPostToolUseEvent("apply_patch", `{"patch":"x"}`, `{"outcome":"ok"}`),
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:            defs.HookRunCompleted,
				AdditionalContext: "gofmt changed 2 files",
			}},
		},
		{
			name:        "unknown fields are ignored",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", printJSON(`{"foo":1}`))},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name: "updatedInput is ignored outside PreToolUse",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPostToolUse, "",
				printJSON(`{"hookSpecificOutput":{"updatedInput":{"command":"x"}}}`))},
			event:    newPostToolUseEvent("bash", `{"command":"ls"}`, `{"exit_code":0}`),
			wantRuns: []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "invalid JSON is a hook error",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", printJSON(`{bad json`))},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "invalid hook output: *",
			}},
		},

		// 纯文本 stdout
		{
			name:        "plain stdout on SessionStart is context",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventSessionStart, "", "echo ctx")},
			event:       newSessionStartEvent("startup"),
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:            defs.HookRunCompleted,
				AdditionalContext: "ctx",
			}},
		},
		{
			name:        "plain stdout on UserPromptSubmit is context",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventUserPromptSubmit, "", "echo ctx")},
			event:       newUserPromptSubmitEvent(),
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:            defs.HookRunCompleted,
				AdditionalContext: "ctx",
			}},
		},
		{
			name:        "plain stdout on PreToolUse is ignored",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "echo debug")},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},

		// stderr 只写日志
		{
			name:          "stderr of a hook that exits 0 only goes to the log",
			definitions:   []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "echo debug >&2; exit 0")},
			event:         newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns:      []defs.HookRun{{Status: defs.HookRunCompleted}},
			wantStderrLog: "debug",
		},
		{
			name: "stderr next to a JSON answer is logged without changing the record",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "",
				printJSON(`{"systemMessage":"careful"}`)+"; echo trace >&2")},
			event:     newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:        defs.HookRunCompleted,
				SystemMessage: "careful",
			}},
			wantStderrLog: "trace",
		},
		{
			name:        "stderr of a hook whose JSON is invalid is logged next to the failure",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", printJSON(`{bad json`)+"; echo trace >&2")},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "invalid hook output: *",
			}},
			wantStderrLog: "trace",
		},

		// 超时、中断、输出上限
		{
			name:        "timeout ends the command",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "sleep 5", withTimeout(time.Second))},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			maxDuration: 1500 * time.Millisecond,
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "timed out after 1s",
			}},
		},
		{
			// 停不下来的命令（例如 SSH 上 kill 失败）在 Terminate 失败后被 Close 放手，RunEvent 不等它自己结束。
			name:               "timeout releases a command that cannot be terminated",
			definitions:        []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "sleep 5", withTimeout(time.Second))},
			event:              newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			isTerminateFailing: true,
			maxDuration:        1500 * time.Millisecond,
			wantSaved:          1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "timed out after 1s",
			}},
		},
		{
			name:        "cancelling the context interrupts the command",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "sleep 30")},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			cancelAfter: 100 * time.Millisecond,
			maxDuration: time.Second,
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "interrupted",
			}},
		},
		{
			// 超时是 hook 自己出错，和中断不同：配置了 on_failure: block 时照样拦下。
			name:        "timeout blocks when the hook is blocked on failure",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "sleep 5", withTimeout(time.Second), withBlockOnFailure())},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			maxDuration: 1500 * time.Millisecond,
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "hook failed: timed out after 1s",
			},
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "timed out after 1s",
			}},
		},
		{
			// 中断不是 hook 出错：配置了 on_failure: block 也不拦下，触发事件的那一层照常走中断收尾。
			name:        "interruption does not block even when the hook is blocked on failure",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventUserPromptSubmit, "", "sleep 30", withBlockOnFailure())},
			event:       newUserPromptSubmitEvent(),
			cancelAfter: 100 * time.Millisecond,
			maxDuration: time.Second,
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "interrupted",
			}},
		},
		{
			name:        "stdout beyond the limit is truncated",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventUserPromptSubmit, "", `head -c 102400 /dev/zero | tr '\0' a`)},
			event:       newUserPromptSubmitEvent(),
			wantSaved:   1,
			wantRuns: []defs.HookRun{{
				Status:            defs.HookRunCompleted,
				AdditionalContext: "*",
			}},
			check: func(t *testing.T, root string, outcome *hook.HookOutcome, runs []*defs.HookRun) {
				text := runs[0].AdditionalContext
				const limit, notice = 64 << 10, "[output truncated]"
				if !strings.HasSuffix(text, notice) || len(text) > limit+len(notice)+1 {
					t.Fatalf("AdditionalContext has %d bytes and suffix %q, want at most %d bytes ending with %q",
						len(text), text[max(0, len(text)-len(notice)):], limit+len(notice)+1, notice)
				}
			},
		},
		{
			name:        "large tool input does not stop a command that never reads stdin",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "write", largeArguments),
			maxDuration: 3 * time.Second,
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "background process holding the output does not outlive the hook",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "", "(sleep 30 &); exit 0", withTimeout(2*time.Second))},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			maxDuration: 3 * time.Second,
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},

		// hook 匹配样例
		{
			name:        "star matches every tool",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "*", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "read_file", `{}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "exact name matches",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "bash", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "bash", `{}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "alias Bash matches bash",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "Bash", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "bash", `{}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "aliases Write and Edit match apply_patch",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "Write|Edit", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "apply_patch", `{}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "regular expression matches MCP tools",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "^mcp__.*", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "mcp__files__read", `{}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "other tool names do not match",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPreToolUse, "bash", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "read_file", `{}`),
		},
		{
			name:        "hooks of another event do not match",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventPostToolUse, "", "exit 0")},
			event:       newPreToolUseEvent(rootSession, "bash", `{}`),
		},
		{
			name:        "SessionStart matcher matches the source",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventSessionStart, "resume", "exit 0")},
			event:       newSessionStartEvent("resume"),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "SessionStart matcher skips another source",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventSessionStart, "resume", "exit 0")},
			event:       newSessionStartEvent("startup"),
		},
		{
			name:        "SubagentStop matcher matches the agent type",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventSubagentStop, "explorer", "exit 0")},
			event: &hook.HookEvent{
				Name:    defs.HookEventSubagentStop,
				Session: childSession,
				TurnID:  "turn-1",
			},
			wantRuns: []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "Stop ignores the matcher",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventStop, "nothing", "exit 0")},
			event:       newStopEvent(),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},
		{
			name:        "UserPromptSubmit ignores the matcher",
			definitions: []*hook.HookDefinition{newTestDefinition(t, defs.HookEventUserPromptSubmit, "nothing", "exit 0")},
			event:       newUserPromptSubmitEvent(),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
		},

		// 合并
		{
			name: "one blocked hook blocks and saved runs follow the display order",
			definitions: []*hook.HookDefinition{
				newTestDefinition(t, defs.HookEventPreToolUse, "", "echo no >&2; exit 2", withOrder(2)),
				newTestDefinition(t, defs.HookEventPreToolUse, "", printJSON(`{"hookSpecificOutput":{"additionalContext":"ctx"}}`), withOrder(1)),
			},
			event: newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "no",
			},
			wantSaved: 2,
			wantRuns: []defs.HookRun{
				{
					Status:            defs.HookRunCompleted,
					AdditionalContext: "ctx",
				},
				{
					Status:      defs.HookRunBlocked,
					BlockReason: "no",
				},
			},
		},
		{
			name: "block reasons are joined in configuration order",
			definitions: []*hook.HookDefinition{
				newTestDefinition(t, defs.HookEventPreToolUse, "", "echo second >&2; exit 2", withOrder(2)),
				newTestDefinition(t, defs.HookEventPreToolUse, "", "echo first >&2; exit 2", withOrder(1)),
			},
			event: newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantOutcome: hook.HookOutcome{
				IsBlocked:   true,
				BlockReason: "first\n\nsecond",
			},
			wantSaved: 2,
			wantRuns: []defs.HookRun{
				{
					Status:      defs.HookRunBlocked,
					BlockReason: "first",
				},
				{
					Status:      defs.HookRunBlocked,
					BlockReason: "second",
				},
			},
		},
		{
			name: "the last hook in configuration order wins the rewritten arguments",
			definitions: []*hook.HookDefinition{
				newTestDefinition(t, defs.HookEventPreToolUse, "", printJSON(`{"hookSpecificOutput":{"updatedInput":{"command":"second"}}}`), withOrder(2)),
				newTestDefinition(t, defs.HookEventPreToolUse, "", printJSON(`{"hookSpecificOutput":{"updatedInput":{"command":"first"}}}`), withOrder(1)),
			},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantOutcome: hook.HookOutcome{UpdatedToolArguments: `{"command":"second"}`},
			wantSaved:   2,
			wantRuns: []defs.HookRun{
				{
					Status:               defs.HookRunCompleted,
					UpdatedToolArguments: `{"command":"first"}`,
				},
				{
					Status:               defs.HookRunCompleted,
					UpdatedToolArguments: `{"command":"second"}`,
				},
			},
		},
		{
			name: "a failed hook does not affect the others",
			definitions: []*hook.HookDefinition{
				newTestDefinition(t, defs.HookEventPreToolUse, "", "exit 1", withOrder(1)),
				newTestDefinition(t, defs.HookEventPreToolUse, "", printJSON(`{"hookSpecificOutput":{"additionalContext":"ctx"}}`), withOrder(2)),
			},
			event:     newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved: 2,
			wantRuns: []defs.HookRun{
				{
					Status:       defs.HookRunFailed,
					ErrorMessage: "exit status 1",
				},
				{
					Status:            defs.HookRunCompleted,
					AdditionalContext: "ctx",
				},
			},
		},
		{
			name: "hooks run in parallel",
			definitions: []*hook.HookDefinition{
				newTestDefinition(t, defs.HookEventPreToolUse, "", "sleep 1", withOrder(1)),
				newTestDefinition(t, defs.HookEventPreToolUse, "", "sleep 1", withOrder(2)),
			},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			maxDuration: 1800 * time.Millisecond,
			wantRuns: []defs.HookRun{
				{Status: defs.HookRunCompleted},
				{Status: defs.HookRunCompleted},
			},
		},

		// hook 脚本样例
		{
			name:        "script runs from the copy in the cache directory",
			definitions: []*hook.HookDefinition{newTestScriptDefinition(t, defs.HookEventPreToolUse, guardPath, []string{"--strict"})},
			event:       newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns:    []defs.HookRun{{Status: defs.HookRunCompleted}},
			check:       checkScriptOutput(guardPath, "--strict\n"),
		},
		{
			name: "script name and arguments with spaces, quotes and dollars arrive verbatim",
			definitions: []*hook.HookDefinition{newTestScriptDefinition(t, defs.HookEventPreToolUse, spacedGuardPath,
				[]string{"a b", "it's", "$HOME"})},
			event:    newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantRuns: []defs.HookRun{{Status: defs.HookRunCompleted}},
			check:    checkScriptOutput(spacedGuardPath, "a b\nit's\n$HOME\n"),
		},
		{
			name: "script that cannot be read fails the run",
			definitions: []*hook.HookDefinition{newTestScriptDefinition(t, defs.HookEventPreToolUse,
				filepath.Join(scriptDirectory, "missing.sh"), nil)},
			event:     newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`),
			wantSaved: 1,
			wantRuns: []defs.HookRun{{
				Status:       defs.HookRunFailed,
				ErrorMessage: "read script: *",
			}},
		},
	}
	// 4. 逐个运行事件，比较决定、运行记录、日志、stdin 和用例自己的检查。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			executorWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			if tt.isTerminateFailing {
				executorWorkspace = terminateFailingWorkspace{Workspace: executorWorkspace}
			}
			pipeline := telemetrytest.NewPipeline(t)
			ctx, cancel := context.WithCancel(telemetry.ContextWithTelemetry(context.Background(), pipeline.Telemetry))
			defer cancel()
			if tt.cancelAfter > 0 {
				defer time.AfterFunc(tt.cancelAfter, cancel).Stop()
			}

			runUpdates := make(chan *defs.HookRun, 16)
			startedAt := time.Now()
			outcome := hook.NewHooks(hook.NewHooksOpt(tt.definitions)).RunEvent(ctx,
				&hook.HookEventRequest{
					Event:      tt.event,
					ExecutorID: "local",
					Workspace:  executorWorkspace,
				}, runUpdates)
			elapsed := time.Since(startedAt)
			// 1. RunEvent 返回后关闭更新通道，收集 running 和最终记录。
			close(runUpdates)
			runs := collectRuns(t, runUpdates)

			if tt.maxDuration > 0 && elapsed > tt.maxDuration {
				t.Fatalf("RunEvent took %s, want at most %s", elapsed, tt.maxDuration)
			}
			gotOutcome := *outcome
			gotOutcome.SavedRuns = nil
			if !reflect.DeepEqual(gotOutcome, tt.wantOutcome) {
				t.Fatalf("outcome = %+v, want %+v", gotOutcome, tt.wantOutcome)
			}
			if len(outcome.SavedRuns) != tt.wantSaved {
				t.Fatalf("saved runs = %d, want %d: %+v", len(outcome.SavedRuns), tt.wantSaved, outcome.SavedRuns)
			}
			if len(runs) != len(tt.wantRuns) {
				t.Fatalf("runs = %d, want %d: %+v", len(runs), len(tt.wantRuns), runs)
			}

			handlers := map[int]string{}
			for _, definition := range tt.definitions {
				handlers[definition.DisplayOrder] = getExpectedHandler(definition)
			}
			for index, run := range runs {
				got, want := getRunResult(run), tt.wantRuns[index]
				gotMessage, wantMessage := got.ErrorMessage, want.ErrorMessage
				if !isErrorMatched(gotMessage, wantMessage) {
					t.Errorf("run %d error = %q, want %q", index, gotMessage, wantMessage)
				}
				got.ErrorMessage, want.ErrorMessage = "", ""
				if want.AdditionalContext == "*" {
					got.AdditionalContext, want.AdditionalContext = "", ""
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("run %d result = %+v, want %+v", index, got, want)
				}
				if run.Event != tt.event.Name || run.ExecutorID != "local" || run.SourcePath != testSourcePath ||
					run.Handler != handlers[run.DisplayOrder] || run.StartedAt == 0 || run.CompletedAt < run.StartedAt {
					t.Errorf("run %d record = %+v, want event %s, executor local, source and handler of its definition, and timestamps", index, run, tt.event.Name)
				}
				if tt.event.ToolCall != nil && run.CallID != tt.event.ToolCall.CallID {
					t.Errorf("run %d call ID = %q, want %q", index, run.CallID, tt.event.ToolCall.CallID)
				}
			}

			// 2. 核对要保存的记录都是最终记录，并按 DisplayOrder 排列。
			previousOrder := -1
			for _, saved := range outcome.SavedRuns {
				index := slices.IndexFunc(runs, func(run *defs.HookRun) bool { return run.ID == saved.ID })
				if index < 0 || !reflect.DeepEqual(saved, runs[index]) || saved.DisplayOrder < previousOrder {
					t.Fatalf("saved run %+v is not a final record in display order, finals: %+v", saved, runs)
				}
				previousOrder = saved.DisplayOrder
			}

			// 3. 核对每次运行只写一条日志，失败用 WARN、其他用 INFO，stderr 按期望写进日志。
			var records []map[string]any
			for _, record := range pipeline.GetLogRecords(t, "") {
				if record["msg"] == "hook finished" || record["msg"] == "hook failed" {
					records = append(records, record)
				}
			}
			if len(records) != len(runs) {
				t.Fatalf("hook log records = %v, want one per run (%d)", records, len(runs))
			}
			wantFailed, gotFailed := 0, 0
			for _, want := range tt.wantRuns {
				if want.Status == defs.HookRunFailed {
					wantFailed++
				}
			}
			for _, record := range records {
				isFailed := record["msg"] == "hook failed"
				if isFailed {
					gotFailed++
				}
				if wantLevel := map[bool]string{true: "WARN", false: "INFO"}[isFailed]; record["level"] != wantLevel {
					t.Errorf("hook log record = %v, want level %s", record, wantLevel)
				}
				gotStderr, hasStderr := record["stderr"]
				if tt.wantStderrLog == "" && hasStderr || tt.wantStderrLog != "" && gotStderr != tt.wantStderrLog {
					t.Errorf("hook log record stderr = %v (present: %v), want %q", gotStderr, hasStderr, tt.wantStderrLog)
				}
			}

			if gotFailed != wantFailed {
				t.Fatalf("hook failed log records = %d, want %d", gotFailed, wantFailed)
			}

			if tt.wantInput != "" {
				content, err := os.ReadFile(filepath.Join(root, "input.json"))
				if err != nil {
					t.Fatal(err)
				}
				var got, want any
				if err := json.Unmarshal(content, &got); err != nil {
					t.Fatalf("stdin %q is not JSON: %v", content, err)
				}
				if err := json.Unmarshal([]byte(strings.ReplaceAll(tt.wantInput, "$ROOT", root)), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("stdin = %s, want %s", content, tt.wantInput)
				}
			}
			if tt.check != nil {
				tt.check(t, root, outcome, runs)
			}
		})
	}
}

// TestNewHookMatcher 验证 matcher 的写法：空串和 * 匹配全部，简单名字准确匹配并认工具别名，其他写法按不加锚点的正则匹配。
func TestNewHookMatcher(t *testing.T) {
	// 1. 准备匹配全部、准确名字、别名和正则的样例。
	tests := []struct {
		name    string
		pattern string
		values  []string
		want    bool
	}{
		{
			name:    "empty matches everything",
			pattern: "",
			values:  []string{"bash"},
			want:    true,
		},
		{
			name:    "star matches everything",
			pattern: "*",
			values:  []string{"anything"},
			want:    true,
		},
		{
			name:    "exact name is not a substring match",
			pattern: "bash",
			values:  []string{"bash2"},
			want:    false,
		},
		{
			name:    "exact names separated by bar",
			pattern: "bash|read_file",
			values:  []string{"read_file"},
			want:    true,
		},
		{
			name:    "alias Bash matches bash",
			pattern: "Bash",
			values:  []string{"bash"},
			want:    true,
		},
		{
			name:    "alias Write matches apply_patch",
			pattern: "Write",
			values:  []string{"apply_patch"},
			want:    true,
		},
		{
			name:    "alias Edit matches apply_patch",
			pattern: "Edit",
			values:  []string{"apply_patch"},
			want:    true,
		},
		{
			name:    "alias Read matches read_file",
			pattern: "Read",
			values:  []string{"read_file"},
			want:    true,
		},
		{
			name:    "alias Agent matches spawn_agent",
			pattern: "Agent",
			values:  []string{"spawn_agent"},
			want:    true,
		},
		{
			name:    "alias does not match other tools",
			pattern: "Edit",
			values:  []string{"read_file"},
			want:    false,
		},
		{
			name:    "exact name does not match as a prefix",
			pattern: "mcp__",
			values:  []string{"mcp__files__read"},
			want:    false,
		},
		{
			name:    "regular expression is not anchored",
			pattern: "files.*read",
			values:  []string{"mcp__files__read"},
			want:    true,
		},
		{
			name:    "regular expression anchors are honored",
			pattern: "^files",
			values:  []string{"mcp__files__read"},
			want:    false,
		},
		{
			name:    "regular expression does not expand aliases",
			pattern: "^Bash$",
			values:  []string{"bash"},
			want:    false,
		},
		{
			name:    "any value may match",
			pattern: "resume",
			values:  []string{"startup", "resume"},
			want:    true,
		},
	}
	// 2. 编译 matcher，核对匹配结果和 String 返回的原文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher, err := hook.NewHookMatcher(tt.pattern)
			if err != nil {
				t.Fatal(err)
			}
			if got := matcher.IsMatched(tt.values...); got != tt.want {
				t.Fatalf("IsMatched(%q) = %v, want %v", tt.values, got, tt.want)
			}
			if matcher.String() != tt.pattern {
				t.Fatalf("String() = %q, want %q", matcher.String(), tt.pattern)
			}
		})
	}

	// 3. 无效的正则返回编译错误。
	if _, err := hook.NewHookMatcher("("); err == nil || !strings.Contains(err.Error(), "missing closing )") {
		t.Fatalf("NewHookMatcher(%q) error = %v, want the regexp compile error", "(", err)
	}
}

// copyRecorder 统计目标工作区上 CreateFileWriter 的调用次数，并可让最前面的几次调用失败或变慢。
type copyRecorder struct {
	creates atomic.Int32
	// failures 是还要失败的次数。
	failures atomic.Int32
	delay    time.Duration
}

// recordingWorkspace 是被 copyRecorder 观察的目标工作区。
type recordingWorkspace struct {
	workspace.Workspace
	recorder *copyRecorder
}

// CreateFileWriter 记下一次调用，按设置先等一会儿，前几次返回错误，之后交给真实工作区。
func (w *recordingWorkspace) CreateFileWriter(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error) {
	// 1. 计数、等待，还有失败次数时返回 disk full。
	w.recorder.creates.Add(1)
	time.Sleep(w.recorder.delay)
	if w.recorder.failures.Add(-1) >= 0 {
		return nil, errors.New("disk full")
	}
	return w.Workspace.CreateFileWriter(ctx, path, mode)
}

// existingScriptTarget 是运行前已经留在缓存目录里的脚本拷贝。
type existingScriptTarget struct {
	content string
	mode    fs.FileMode
}

// TestHooksScriptCopy 验证 script 的拷贝：同一 Executor 上同内容只拷一次，内容或权限变了拷到新目录，并发的第一次运行只拷一次，拷贝失败下次重试，缓存里已有相同副本时直接用。
func TestHooksScriptCopy(t *testing.T) {
	// 1. 准备改内容、改权限、并发、已有副本和拷贝失败的分步样例。
	const scriptV1, scriptV2 = "#!/bin/sh\necho v1\n", "#!/bin/sh\necho v2\n"
	type step struct {
		executorID string
		// scriptContent 非空时，先把脚本改成这个内容。
		scriptContent string
		// scriptMode 非 0 时，先把脚本的权限位改成这个值。
		scriptMode fs.FileMode
		// isNewHooks 为 true 时，这一步用新创建的 Hooks（相当于重新加载配置或重启 serve 后），丢掉进程内的拷贝记录。
		isNewHooks bool
		// parallelRuns 是同时开始的运行次数，为 0 时按 1 次。
		parallelRuns int
		wantStatus   defs.HookRunStatus
		// wantError 以 * 结尾时只比较前缀。
		wantError string
		// wantContext 是每次运行脚本输出的内容。
		wantContext string
		// wantCopies 是这一步结束后，目标工作区上 CreateFileWriter 累计被调用的次数。
		wantCopies int32
	}
	newVirtualWorkspace := func(t *testing.T, root string) workspace.Workspace {
		virtualWorkspace, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("", nil))
		if err != nil {
			t.Fatal(err)
		}
		return virtualWorkspace
	}
	newLocalWorkspace := func(t *testing.T, root string) workspace.Workspace {
		localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
		if err != nil {
			t.Fatal(err)
		}
		return localWorkspace
	}
	tests := []struct {
		name string
		// newWorkspace 创建 Executor 的真实工作区，为 nil 时用以 root 为根目录的本机工作区。
		newWorkspace func(t *testing.T, root string) workspace.Workspace
		// failFirstCopies 让目标工作区最前面的几次 CreateFileWriter 失败，copyDelay 让每次调用先等一会儿。
		failFirstCopies int32
		copyDelay       time.Duration
		// existingTarget 非 nil 时，运行前先在缓存目录里放一个这样的文件，位置按脚本的初始内容算。
		existingTarget *existingScriptTarget
		// initialScriptMode 是脚本最初的权限位，为 0 时按 0755。
		initialScriptMode fs.FileMode
		steps             []step
	}{
		{
			name: "a script is copied once per Executor and a changed script is copied to a new directory",
			steps: []step{
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  1,
				},
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  1,
				},
				{
					executorID:    "local",
					scriptContent: scriptV2,
					wantStatus:    defs.HookRunCompleted,
					wantContext:   "v2",
					wantCopies:    2,
				},
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v2",
					wantCopies:  2,
				},
			},
		},
		{
			name:      "concurrent first runs wait for one copy",
			copyDelay: 200 * time.Millisecond,
			steps: []step{
				{
					executorID:   "local",
					parallelRuns: 2,
					wantStatus:   defs.HookRunCompleted,
					wantContext:  "v1",
					wantCopies:   1,
				},
			},
		},
		{
			name:         "a workspace without a home directory fails the run",
			newWorkspace: newVirtualWorkspace,
			steps: []step{
				{
					executorID: "local",
					wantStatus: defs.HookRunFailed,
					wantError:  "copy script: *",
					wantCopies: 0,
				},
			},
		},
		{
			name:            "a failed copy is retried by the next run",
			failFirstCopies: 1,
			steps: []step{
				{
					executorID: "local",
					wantStatus: defs.HookRunFailed,
					wantError:  "copy script: *",
					wantCopies: 1,
				},
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  2,
				},
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  2,
				},
			},
		},
		{
			name:            "runs waiting for a failed copy fail with it",
			failFirstCopies: 1,
			copyDelay:       200 * time.Millisecond,
			steps: []step{
				{
					executorID:   "local",
					parallelRuns: 2,
					wantStatus:   defs.HookRunFailed,
					wantError:    "copy script: *",
					wantCopies:   1,
				},
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  2,
				},
			},
		},
		{
			name:              "a script that gains the execute bit is copied to a new directory",
			initialScriptMode: 0o644,
			steps: []step{
				{
					executorID: "local",
					wantStatus: defs.HookRunFailed,
					wantError:  "exit status 126*",
					wantCopies: 1,
				},
				{
					executorID: "local",
					wantStatus: defs.HookRunFailed,
					wantError:  "exit status 126*",
					wantCopies: 1,
				},
				{
					executorID:  "local",
					scriptMode:  0o755,
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  2,
				},
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  2,
				},
			},
		},
		{
			name:              "a new Hooks after the script gains the execute bit does not reuse the old copy",
			initialScriptMode: 0o644,
			steps: []step{
				{
					executorID: "local",
					wantStatus: defs.HookRunFailed,
					wantError:  "exit status 126*",
					wantCopies: 1,
				},
				{
					executorID:  "local",
					scriptMode:  0o755,
					isNewHooks:  true,
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  2,
				},
			},
		},
		{
			name:              "an identical read-only copy left by an earlier process is not copied again",
			initialScriptMode: 0o555,
			existingTarget: &existingScriptTarget{
				content: scriptV1,
				mode:    0o555,
			},
			steps: []step{
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  0,
				},
			},
		},
		{
			name: "a partial copy left by a crash is overwritten",
			existingTarget: &existingScriptTarget{
				content: "#!/bin/sh\necho par",
				mode:    0o755,
			},
			steps: []step{
				{
					executorID:  "local",
					wantStatus:  defs.HookRunCompleted,
					wantContext: "v1",
					wantCopies:  1,
				},
			},
		},
	}
	// 2. 逐步修改脚本或重建 Hooks 后运行，核对运行结果和累计拷贝次数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			scriptPath := writeTestScript(t, t.TempDir(), "guard.sh", scriptV1)
			if tt.initialScriptMode != 0 {
				if err := os.Chmod(scriptPath, tt.initialScriptMode); err != nil {
					t.Fatal(err)
				}
			}
			if tt.existingTarget != nil {
				targetPath := getCopiedScriptPath(t, home, scriptPath)
				if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(targetPath, []byte(tt.existingTarget.content), tt.existingTarget.mode); err != nil {
					t.Fatal(err)
				}
			}

			newWorkspace := tt.newWorkspace
			if newWorkspace == nil {
				newWorkspace = newLocalWorkspace
			}
			recorder := &copyRecorder{delay: tt.copyDelay}
			recorder.failures.Store(tt.failFirstCopies)
			executorWorkspaces := map[string]workspace.Workspace{}
			newHooks := func() hook.Hooks {
				return hook.NewHooks(hook.NewHooksOpt([]*hook.HookDefinition{
					newTestScriptDefinition(t, defs.HookEventSessionStart, scriptPath, nil),
				}))
			}
			hooks := newHooks()
			ctx := telemetry.ContextWithTelemetry(context.Background(), telemetrytest.NewPipeline(t).Telemetry)

			for index, step := range tt.steps {
				if step.scriptContent != "" {
					writeTestScript(t, filepath.Dir(scriptPath), filepath.Base(scriptPath), step.scriptContent)
				}
				if step.scriptMode != 0 {
					if err := os.Chmod(scriptPath, step.scriptMode); err != nil {
						t.Fatal(err)
					}
				}
				if step.isNewHooks {
					hooks = newHooks()
				}
				if executorWorkspaces[step.executorID] == nil {
					executorWorkspaces[step.executorID] = &recordingWorkspace{
						Workspace: newWorkspace(t, t.TempDir()),
						recorder:  recorder,
					}
				}

				channels := make([]chan *defs.HookRun, max(step.parallelRuns, 1))
				var running sync.WaitGroup
				for run := range channels {
					channels[run] = make(chan *defs.HookRun, 16)
					running.Add(1)
					go func() {
						defer running.Done()
						defer close(channels[run])
						hooks.RunEvent(ctx, &hook.HookEventRequest{
							Event:      newSessionStartEvent("startup"),
							ExecutorID: step.executorID,
							Workspace:  executorWorkspaces[step.executorID],
						}, channels[run])
					}()
				}
				running.Wait()

				for _, runUpdates := range channels {
					runs := collectRuns(t, runUpdates)
					if len(runs) != 1 {
						t.Fatalf("step %d: runs = %+v, want one", index, runs)
					}
					if runs[0].Status != step.wantStatus || runs[0].AdditionalContext != step.wantContext || !isErrorMatched(runs[0].ErrorMessage, step.wantError) {
						t.Errorf("step %d: run = status %s, context %q, error %q; want status %s, context %q, error %q",
							index, runs[0].Status, runs[0].AdditionalContext, runs[0].ErrorMessage, step.wantStatus, step.wantContext, step.wantError)
					}
				}
				if copies := recorder.creates.Load(); copies != step.wantCopies {
					t.Errorf("step %d: copies = %d, want %d", index, copies, step.wantCopies)
				}
			}
		})
	}
}

// extractRecordingWorkspace 统计目标工作区上解压插件目录的命令（含 tar -xzpf）启动的次数；isExtractFailing 时让这些命令启动失败。
type extractRecordingWorkspace struct {
	workspace.Workspace
	extracts         *atomic.Int32
	isExtractFailing bool
}

// StartProcess 遇到解压插件目录的命令时计数，按设置让它启动失败，其他命令交给真实工作区。
func (w *extractRecordingWorkspace) StartProcess(ctx context.Context, request *workspace.ProcessRequest) (workspace.Process, error) {
	// 1. 命令含 tar -xzpf 时计数，isExtractFailing 时返回 connection refused。
	if strings.Contains(request.Command, "tar -xzpf") {
		w.extracts.Add(1)
		if w.isExtractFailing {
			return nil, errors.New("connection refused")
		}
	}
	return w.Workspace.StartProcess(ctx, request)
}

// terminateFailingWorkspace 启动的命令 Terminate 总是失败，Close 照常结束命令，模拟 kill 失败的远端命令。
type terminateFailingWorkspace struct {
	workspace.Workspace
}

// StartProcess 启动真实进程，包成 Terminate 总是失败的进程返回。
func (w terminateFailingWorkspace) StartProcess(ctx context.Context, request *workspace.ProcessRequest) (workspace.Process, error) {
	// 1. 启动真实进程，再包一层。
	process, err := w.Workspace.StartProcess(ctx, request)
	if err != nil {
		return nil, err
	}
	return terminateFailingProcess{Process: process}, nil
}

// terminateFailingProcess 是 Terminate 总是失败的命令。
type terminateFailingProcess struct {
	workspace.Process
}

// Terminate 总是返回 kill failed。
func (terminateFailingProcess) Terminate() error {
	// 1. 返回错误，不结束进程。
	return errors.New("kill failed")
}

// getCachedPluginDirectories 列出 home 下插件缓存里的 demo 插件目录，并核对目录名和 core.md。
func getCachedPluginDirectories(t *testing.T, home string) []string {
	t.Helper()
	// 1. 读取缓存目录，目录不存在时返回空；每个目录名必须是 demo-<哈希前 16 位>，里面有 core.md。
	cacheDirectory := filepath.Join(home, ".cache", "openvaala", "plugins")
	entries, err := os.ReadDir(cacheDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !regexp.MustCompile(`^demo-[0-9a-f]{16}$`).MatchString(name) {
			t.Fatalf("cached plugin directory %q, want demo-<16 hex>", name)
		}
		if _, err := os.Stat(filepath.Join(cacheDirectory, name, "core.md")); err != nil {
			t.Fatalf("cached plugin directory %q has no core.md: %v", name, err)
		}
		names = append(names, name)
	}
	return names
}

// TestHooksPluginRun 验证插件 hook：local 直接用本机插件目录，其他 Executor 用拷过去的副本，同一份 Hooks 只拷一次，插件变了的新 Hooks 拷到新目录，拷贝失败按 on_failure 处理。
func TestHooksPluginRun(t *testing.T) {
	// 1. 准备在不同 Executor 上运行、修改插件内容和重建 Hooks 的分步样例。
	type step struct {
		executorID string
		// event 选出运行的插件 hook：SessionStart 是 hooks/inject 脚本，UserPromptSubmit 是读 core.md 的 command，
		// PreToolUse 是配置了 on_failure: block 的同一条 command。
		event *hook.HookEvent
		// coreContent 非空时，先把插件的 core.md 改成这个内容。
		coreContent string
		// isNewHooks 为 true 时，这一步用新创建的 Hooks（相当于重新加载配置或重启 serve 后），丢掉进程内的拷贝记录。
		isNewHooks  bool
		wantStatus  defs.HookRunStatus
		wantContext string
		wantError   string
		// wantBlockReason 是合并后决定的拦下原因，为空表示没有拦下。
		wantBlockReason string
		// wantExtracts 是这一步结束后，目标工作区上解压插件目录的命令累计启动的次数。
		wantExtracts int32
		// wantCachedDirectories 是这一步结束后，主目录下插件缓存里 demo 插件目录的个数。
		wantCachedDirectories int
	}
	sessionStart := newSessionStartEvent("startup")
	userPromptSubmit := newUserPromptSubmitEvent()
	preToolUse := newPreToolUseEvent(rootSession, "bash", `{"command":"ls"}`)
	tests := []struct {
		name string
		// isExtractFailing 让目标工作区上解压插件目录的命令启动失败。
		isExtractFailing bool
		steps            []step
	}{
		{
			name: "local_uses_original_directory",
			steps: []step{
				{
					executorID:            "local",
					event:                 sessionStart,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          0,
					wantCachedDirectories: 0,
				},
				// 改插件文件在 local 上立刻生效。
				{
					executorID:            "local",
					event:                 sessionStart,
					coreContent:           "v2",
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v2",
					wantExtracts:          0,
					wantCachedDirectories: 0,
				},
			},
		},
		{
			name: "remote_copies_once_per_hooks",
			steps: []step{
				{
					executorID:            "ssh-dev",
					event:                 sessionStart,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
				// 同一份 Hooks 继续用第一次拷好的目录：本机改了插件也不重拷，脚本读到的仍是拷贝里的内容。
				{
					executorID:            "ssh-dev",
					event:                 sessionStart,
					coreContent:           "v2",
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
			},
		},
		{
			name: "command_hook_sees_plugin_root",
			steps: []step{
				{
					executorID:            "local",
					event:                 userPromptSubmit,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          0,
					wantCachedDirectories: 0,
				},
				{
					executorID:            "ssh-dev",
					event:                 userPromptSubmit,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
				// 改插件后 local 的 OPENVAALA_PLUGIN_ROOT 指向本机插件目录，ssh-dev 的指向拷贝。
				{
					executorID:            "local",
					event:                 userPromptSubmit,
					coreContent:           "v2",
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v2",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
				{
					executorID:            "ssh-dev",
					event:                 userPromptSubmit,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
			},
		},
		{
			name: "new_hooks_unchanged_plugin_does_not_copy",
			steps: []step{
				{
					executorID:            "ssh-dev",
					event:                 sessionStart,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
				{
					executorID:            "ssh-dev",
					event:                 sessionStart,
					isNewHooks:            true,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
			},
		},
		{
			name: "changed_plugin_copies_new_directory",
			steps: []step{
				{
					executorID:            "ssh-dev",
					event:                 sessionStart,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v1",
					wantExtracts:          1,
					wantCachedDirectories: 1,
				},
				{
					executorID:            "ssh-dev",
					event:                 sessionStart,
					coreContent:           "v2",
					isNewHooks:            true,
					wantStatus:            defs.HookRunCompleted,
					wantContext:           "v2",
					wantExtracts:          2,
					wantCachedDirectories: 2,
				},
			},
		},
		{
			name:             "copy_failure_follows_on_failure",
			isExtractFailing: true,
			steps: []step{
				{
					executorID:   "ssh-dev",
					event:        sessionStart,
					wantStatus:   defs.HookRunFailed,
					wantError:    "copy plugin: connection refused",
					wantExtracts: 1,
				},
				{
					executorID:      "ssh-dev",
					event:           preToolUse,
					wantStatus:      defs.HookRunFailed,
					wantError:       "copy plugin: connection refused",
					wantBlockReason: "hook failed: copy plugin: connection refused",
					wantExtracts:    2,
				},
			},
		},
	}
	// 2. 逐步运行插件 hook，比较运行结果、拦下原因、解压次数和缓存目录个数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			pluginDirectory := filepath.Join(t.TempDir(), "my 'plugins'", "demo")
			if err := os.MkdirAll(filepath.Join(pluginDirectory, "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pluginDirectory, "core.md"), []byte("v1"), 0o644); err != nil {
				t.Fatal(err)
			}
			scriptPath := writeTestScript(t, filepath.Join(pluginDirectory, "hooks"), "inject", "#!/bin/sh\ncat \"$(dirname \"$0\")/../core.md\"\n")
			// 1. 加一个指向自身的目录链接，算哈希和拷贝时都要跳过它。
			if err := os.Symlink(".", filepath.Join(pluginDirectory, "loop")); err != nil {
				t.Fatal(err)
			}

			plugin := &defs.Plugin{
				Name:      "demo",
				Directory: pluginDirectory,
			}
			newHooks := func() hook.Hooks {
				const readCore = `cat "${OPENVAALA_PLUGIN_ROOT}/core.md"`
				definitions := []*hook.HookDefinition{
					newTestScriptDefinition(t, defs.HookEventSessionStart, scriptPath, nil),
					newTestDefinition(t, defs.HookEventUserPromptSubmit, "", readCore),
					newTestDefinition(t, defs.HookEventPreToolUse, "", readCore, withBlockOnFailure()),
				}
				for _, definition := range definitions {
					definition.Plugin = plugin
				}
				return hook.NewHooks(hook.NewHooksOpt(definitions))
			}
			hooks := newHooks()
			extracts := &atomic.Int32{}
			executorWorkspaces := map[string]workspace.Workspace{}
			ctx := telemetry.ContextWithTelemetry(context.Background(), telemetrytest.NewPipeline(t).Telemetry)

			for index, step := range tt.steps {
				if step.coreContent != "" {
					if err := os.WriteFile(filepath.Join(pluginDirectory, "core.md"), []byte(step.coreContent), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if step.isNewHooks {
					hooks = newHooks()
				}
				if executorWorkspaces[step.executorID] == nil {
					localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
					if err != nil {
						t.Fatal(err)
					}
					executorWorkspaces[step.executorID] = &extractRecordingWorkspace{
						Workspace:        localWorkspace,
						extracts:         extracts,
						isExtractFailing: tt.isExtractFailing,
					}
				}

				runUpdates := make(chan *defs.HookRun, 16)
				outcome := hooks.RunEvent(ctx, &hook.HookEventRequest{
					Event:      step.event,
					ExecutorID: step.executorID,
					Workspace:  executorWorkspaces[step.executorID],
				}, runUpdates)
				close(runUpdates)

				runs := collectRuns(t, runUpdates)
				if len(runs) != 1 {
					t.Fatalf("step %d: runs = %+v, want one", index, runs)
				}
				if runs[0].Status != step.wantStatus || runs[0].AdditionalContext != step.wantContext || runs[0].ErrorMessage != step.wantError {
					t.Errorf("step %d: run = status %s, context %q, error %q; want status %s, context %q, error %q",
						index, runs[0].Status, runs[0].AdditionalContext, runs[0].ErrorMessage, step.wantStatus, step.wantContext, step.wantError)
				}
				if outcome.BlockReason != step.wantBlockReason || outcome.IsBlocked != (step.wantBlockReason != "") {
					t.Errorf("step %d: outcome blocked %v with %q, want block reason %q", index, outcome.IsBlocked, outcome.BlockReason, step.wantBlockReason)
				}
				if got := extracts.Load(); got != step.wantExtracts {
					t.Errorf("step %d: extracts = %d, want %d", index, got, step.wantExtracts)
				}
				if got := getCachedPluginDirectories(t, home); len(got) != step.wantCachedDirectories {
					t.Errorf("step %d: cached plugin directories = %q, want %d", index, got, step.wantCachedDirectories)
				}
			}
		})
	}
}

// TestHooksGetHookInfos 验证 GetHookInfos 的字段、顺序和 CurrentHash：定义的任何一项变了，哈希就变。
func TestHooksGetHookInfos(t *testing.T) {
	// 1. 准备基准 script 定义和算哈希的函数。
	newBase := func() *hook.HookDefinition {
		definition := newTestScriptDefinition(t, defs.HookEventPreToolUse, "/home/u/hooks/guard.sh", []string{"--strict"})
		definition.Matcher, _ = hook.NewHookMatcher("bash")
		return definition
	}
	getHash := func(definition *hook.HookDefinition) string {
		return hook.NewHooks(hook.NewHooksOpt([]*hook.HookDefinition{definition})).GetHookInfos()[0].CurrentHash
	}

	// 2. 核对各字段和顺序，哈希是 64 位十六进制。
	t.Run("fields", func(t *testing.T) {
		tests := []struct {
			name        string
			definitions []*hook.HookDefinition
			// want 不含 CurrentHash，它另行断言。
			want []hook.HookInfo
		}{
			{
				name: "command handler is the original text",
				definitions: []*hook.HookDefinition{
					newTestDefinition(t, defs.HookEventStop, "", "~/bin/check-tests.sh --all", withTimeout(600*time.Second)),
				},
				want: []hook.HookInfo{{
					Event:          defs.HookEventStop,
					Handler:        "~/bin/check-tests.sh --all",
					TimeoutSeconds: 600,
					SourcePath:     testSourcePath,
					DisplayOrder:   1,
				}},
			},
			{
				name: "script handler is the path followed by the joined arguments",
				definitions: []*hook.HookDefinition{
					newTestScriptDefinition(t, defs.HookEventPreToolUse, "/home/u/hooks/guard.sh", []string{"--strict", "a b"}, withTimeout(60*time.Second)),
				},
				want: []hook.HookInfo{{
					Event:          defs.HookEventPreToolUse,
					Handler:        "/home/u/hooks/guard.sh --strict a b",
					TimeoutSeconds: 60,
					SourcePath:     testSourcePath,
					DisplayOrder:   1,
				}},
			},
			{
				name: "script handler without arguments is the path",
				definitions: []*hook.HookDefinition{
					newTestScriptDefinition(t, defs.HookEventPostToolUse, "/home/u/hooks/gofmt.sh", nil),
				},
				want: []hook.HookInfo{{
					Event:          defs.HookEventPostToolUse,
					Handler:        "/home/u/hooks/gofmt.sh",
					TimeoutSeconds: 10,
					SourcePath:     testSourcePath,
					DisplayOrder:   1,
				}},
			},
			{
				name: "matcher keeps the original text and infos follow the display order",
				definitions: []*hook.HookDefinition{
					withKey(newTestDefinition(t, defs.HookEventPreToolUse, "Bash|Edit", "exit 1", withOrder(2)), "go.yaml:PreToolUse:1"),
					withKey(newTestDefinition(t, defs.HookEventPreToolUse, "", "exit 0", withOrder(1)), "go.yaml:PreToolUse:0"),
				},
				want: []hook.HookInfo{
					{
						Key:            "go.yaml:PreToolUse:0",
						Event:          defs.HookEventPreToolUse,
						Handler:        "exit 0",
						TimeoutSeconds: 10,
						SourcePath:     testSourcePath,
						DisplayOrder:   1,
					},
					{
						Key:            "go.yaml:PreToolUse:1",
						Event:          defs.HookEventPreToolUse,
						Matcher:        "Bash|Edit",
						Handler:        "exit 1",
						TimeoutSeconds: 10,
						SourcePath:     testSourcePath,
						DisplayOrder:   2,
					},
				},
			},
			{
				name: "plugin hook carries the plugin name",
				definitions: []*hook.HookDefinition{func() *hook.HookDefinition {
					definition := newTestDefinition(t, defs.HookEventStop, "", "exit 0")
					definition.Plugin = &defs.Plugin{Name: "demo"}
					return definition
				}()},
				want: []hook.HookInfo{{
					Event:          defs.HookEventStop,
					Handler:        "exit 0",
					TimeoutSeconds: 10,
					SourcePath:     testSourcePath,
					DisplayOrder:   1,
					PluginName:     "demo",
				}},
			},
			{
				name: "no definitions",
				want: []hook.HookInfo{},
			},
		}
		// 1. 取 hook 信息，去掉哈希后比较。
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				infos := hook.NewHooks(hook.NewHooksOpt(tt.definitions)).GetHookInfos()
				got := make([]hook.HookInfo, 0, len(infos))
				for _, info := range infos {
					if len(info.CurrentHash) != 64 {
						t.Errorf("CurrentHash = %q, want 64 hex characters", info.CurrentHash)
					}
					withoutHash := *info
					withoutHash.CurrentHash = ""
					got = append(got, withoutHash)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("infos = %+v, want %+v", got, tt.want)
				}
			})
		}
	})

	// 3. 逐项改动定义，核对哈希是否跟着变。
	t.Run("current hash", func(t *testing.T) {
		tests := []struct {
			name        string
			change      func(definition *hook.HookDefinition)
			wantChanged bool
		}{
			{
				name:        "same definition",
				change:      func(definition *hook.HookDefinition) {},
				wantChanged: false,
			},
			{
				name:        "event",
				change:      func(definition *hook.HookDefinition) { definition.Event = defs.HookEventPostToolUse },
				wantChanged: true,
			},
			{
				name:        "matcher",
				change:      func(definition *hook.HookDefinition) { definition.Matcher, _ = hook.NewHookMatcher("bash|read_file") },
				wantChanged: true,
			},
			{
				name:        "command",
				change:      func(definition *hook.HookDefinition) { definition.ScriptPath, definition.Command = "", "exit 0" },
				wantChanged: true,
			},
			{
				name:        "script path",
				change:      func(definition *hook.HookDefinition) { definition.ScriptPath = "/home/u/hooks/other.sh" },
				wantChanged: true,
			},
			{
				name:        "arguments",
				change:      func(definition *hook.HookDefinition) { definition.Args = []string{"--strict", "--quiet"} },
				wantChanged: true,
			},
			{
				name:        "timeout",
				change:      func(definition *hook.HookDefinition) { definition.Timeout = 30 * time.Second },
				wantChanged: true,
			},
			{
				name:        "on failure",
				change:      func(definition *hook.HookDefinition) { definition.IsBlockedOnFailure = true },
				wantChanged: true,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				changed := newBase()
				tt.change(changed)
				if gotChanged := getHash(newBase()) != getHash(changed); gotChanged != tt.wantChanged {
					t.Fatalf("hash changed = %v, want %v", gotChanged, tt.wantChanged)
				}
			})
		}
	})
}

// withKey 设置定义的 Key 并返回定义。
func withKey(definition *hook.HookDefinition, key string) *hook.HookDefinition {
	// 1. 写入 Key。
	definition.Key = key
	return definition
}
