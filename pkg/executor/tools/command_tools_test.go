package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestCommandTools 验证命令启动、进度、续读、输入、停止和列举。
func TestCommandTools(t *testing.T) {
	// 1. 准备命令正常运行、取消和失败用例。
	tests := []struct {
		name string
		run  func(t *testing.T, h *commandToolsHarness)
	}{
		{
			name: "tool_names",
			run: func(t *testing.T, h *commandToolsHarness) {
				definitions, err := h.toolExecutor.GetTools(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, definition := range definitions {
					names = append(names, definition.Name)
				}
				if !slices.Equal(names, tools.GetCommandToolNames()) {
					t.Fatalf("tool names = %v, want %v", names, tools.GetCommandToolNames())
				}
			},
		},
		{
			name: "bash_exits_within_wait",
			run: func(t *testing.T, h *commandToolsHarness) {
				result, content := h.call(t, "bash", map[string]any{"command": "echo hi; exit 2", "wait_seconds": 5})
				if result.Error != nil || content.Status != "exited" || content.ExitCode == nil || *content.ExitCode != 2 || !strings.Contains(content.Output, "hi") {
					t.Fatalf("bash result = %s, error = %+v", result.Content, result.Error)
				}
			},
		},
		{
			name: "bash_returns_running",
			run: func(t *testing.T, h *commandToolsHarness) {
				result, content := h.call(t, "bash", map[string]any{"command": "sleep 30", "wait_seconds": 1})
				if result.Error != nil || content.Status != "running" || !strings.Contains(string(result.Content), `"exit_code":null`) ||
					!strings.HasPrefix(content.CommandID, "cmd_") {
					t.Fatalf("bash result = %s, error = %+v", result.Content, result.Error)
				}
			},
		},
		{
			name: "bash_wait_zero",
			run: func(t *testing.T, h *commandToolsHarness) {
				started := time.Now()
				result, content := h.call(t, "bash", map[string]any{"command": "sleep 30", "wait_seconds": 0})
				if result.Error != nil || content.Status != "running" || time.Since(started) > 5*time.Second {
					t.Fatalf("bash result = %s, error = %+v after %v", result.Content, result.Error, time.Since(started))
				}
			},
		},
		{
			name: "bash_progress",
			run: func(t *testing.T, h *commandToolsHarness) {
				result, progress := h.run(t, t.Context(), "bash", map[string]any{"command": "printf first; sleep 0.2; printf second", "wait_seconds": 5}, nil)
				content := decodeCommandToolContent(t, result)
				var progressText strings.Builder
				for _, event := range progress {
					var text string
					if event.Stream != executor.ExecutionStreamTypeTerminal || json.Unmarshal(event.Content, &text) != nil {
						t.Fatalf("progress = %+v", event)
					}
					progressText.WriteString(text)
				}
				if content.Status != "exited" || content.Output != "firstsecond" || progressText.String() != content.Output {
					t.Fatalf("bash result = %s, progress = %q", result.Content, progressText.String())
				}
			},
		},
		{
			name: "bash_canceled_during_wait",
			run: func(t *testing.T, h *commandToolsHarness) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				started := time.Now()
				result, _ := h.run(t, ctx, "bash", map[string]any{"command": "printf started; sleep 30", "wait_seconds": 30}, cancel)
				content := decodeCommandToolContent(t, result)
				if result.Error != nil || content.Status != "running" || content.CommandID == "" || content.Output != "started" || time.Since(started) > 5*time.Second {
					t.Fatalf("bash result = %s, error = %+v after %v", result.Content, result.Error, time.Since(started))
				}
			},
		},
		{
			name: "read_after_running",
			run: func(t *testing.T, h *commandToolsHarness) {
				_, started := h.call(t, "bash", map[string]any{"command": "printf first; sleep 0.5; printf second", "wait_seconds": 0})
				if started.Status != "running" {
					t.Fatalf("bash status = %q, want running", started.Status)
				}
				output := started.Output
				read := started
				for read.Status == "running" || read.HasMore {
					var result *defs.ToolResult
					result, read = h.call(t, "read_command_output", map[string]any{"command_id": started.CommandID, "cursor": read.NextCursor, "wait_seconds": 5})
					if result.Error != nil {
						t.Fatal(result.Error)
					}
					output += read.Output
				}
				if output != "firstsecond" || read.Status != "exited" || read.ExitCode == nil || *read.ExitCode != 0 {
					t.Fatalf("read output = %q, last read = %+v", output, read)
				}
				if _, reread := h.call(t, "read_command_output", map[string]any{"command_id": started.CommandID}); reread.Output != "firstsecond" {
					t.Fatalf("reread output = %q, want the same output from cursor 0", reread.Output)
				}
				h.requireError(t, "read_command_output", map[string]any{"command_id": "cmd_000000000000"},
					"command_not_found", "command cmd_000000000000 is not available in this session and executor")
				h.requireError(t, "read_command_output", map[string]any{"command_id": started.CommandID, "cursor": 1000},
					"invalid_cursor", "cursor 1000 is after the latest output 11")
			},
		},
		{
			name: "write_and_stop",
			run: func(t *testing.T, h *commandToolsHarness) {
				_, interactive := h.call(t, "bash", map[string]any{"command": "read line; echo got:$line; sleep 30", "interactive": true, "wait_seconds": 0})
				result, written := h.call(t, "write_stdin", map[string]any{"command_id": interactive.CommandID, "chars": "hi\n"})
				if result.Error != nil || written.WrittenBytes != 3 || written.Status != "running" {
					t.Fatalf("write_stdin result = %s, error = %+v", result.Content, result.Error)
				}
				read := interactive
				output := interactive.Output
				for !strings.Contains(output, "got:hi") {
					_, read = h.call(t, "read_command_output", map[string]any{"command_id": interactive.CommandID, "cursor": read.NextCursor, "wait_seconds": 5})
					if read.Output == "" {
						t.Fatalf("output = %q, want got:hi", output)
					}
					output += read.Output
				}

				_, plain := h.call(t, "bash", map[string]any{"command": "sleep 30", "wait_seconds": 0})
				h.requireError(t, "write_stdin", map[string]any{"command_id": plain.CommandID, "chars": "x"},
					"stdin_unavailable", "command "+plain.CommandID+" was not started with interactive=true")

				result, stopped := h.call(t, "stop_command", map[string]any{"command_id": interactive.CommandID})
				if result.Error != nil || stopped.StopReason != "stopped" || stopped.Status != "exited" {
					t.Fatalf("stop_command result = %s, error = %+v", result.Content, result.Error)
				}
				h.requireError(t, "write_stdin", map[string]any{"command_id": interactive.CommandID, "chars": "x"},
					"command_finished", "command "+interactive.CommandID+" has finished and no longer accepts input")
			},
		},
		{
			// 程序读一行就退出、剩下的输入没人读：写入在命令结束、关闭终端后返回，不会一直卡住。
			name: "write_after_exit_returns",
			run: func(t *testing.T, h *commandToolsHarness) {
				_, interactive := h.call(t, "bash", map[string]any{"command": "head -n 1", "interactive": true, "wait_seconds": 0})
				started := time.Now()
				result, written := h.call(t, "write_stdin", map[string]any{"command_id": interactive.CommandID, "chars": strings.Repeat(strings.Repeat("x", 63)+"\n", 1<<14)})
				if time.Since(started) > 5*time.Second || (result.Error != nil && result.Error.Code != "command_input_failed" && result.Error.Code != "command_finished") {
					t.Fatalf("write_stdin result = %+v, error = %+v after %s", written, result.Error, time.Since(started))
				}
				read := interactive
				for read.Status == "running" || read.HasMore {
					var readResult *defs.ToolResult
					readResult, read = h.call(t, "read_command_output", map[string]any{"command_id": interactive.CommandID, "cursor": read.NextCursor, "wait_seconds": 5})
					if readResult.Error != nil || (read.Status == "running" && read.Output == "") {
						t.Fatalf("read_command_output result = %+v, error = %+v, want the command to exit", read, readResult.Error)
					}
				}
				if read.Status != "exited" {
					t.Fatalf("command status = %q, want exited", read.Status)
				}
			},
		},
		{
			name: "write_canceled",
			run: func(t *testing.T, h *commandToolsHarness) {
				_, interactive := h.call(t, "bash", map[string]any{"command": "sleep 30", "interactive": true, "wait_seconds": 0})
				ctx, cancel := context.WithCancel(t.Context())
				defer time.AfterFunc(200*time.Millisecond, cancel).Stop()
				result, _ := h.run(t, ctx, "write_stdin", map[string]any{"command_id": interactive.CommandID, "chars": strings.Repeat(strings.Repeat("x", 63)+"\n", 1<<14)}, nil)
				if result.Error == nil || result.Error.Code != "canceled" {
					t.Fatalf("write_stdin result = %s, error = %+v, want canceled", result.Content, result.Error)
				}
			},
		},
		{
			name: "write_failure_reports_input_failed",
			run: func(t *testing.T, _ *commandToolsHarness) {
				failing := &commandToolsHarness{toolExecutor: newCommandToolExecutor(t, startTerminalManager(t, nil),
					newCommandEnvironment(t, "local", &inputFailingWorkspace{Workspace: newTestWorkspace(t, t.TempDir())}))}
				_, interactive := failing.call(t, "bash", map[string]any{"command": "sleep 30", "interactive": true, "wait_seconds": 0})
				failing.requireError(t, "write_stdin", map[string]any{"command_id": interactive.CommandID, "chars": "hi\n"},
					"command_input_failed", "wrote 0 bytes: terminal input broken")
			},
		},
		{
			name: "command_limit_reached",
			run: func(t *testing.T, _ *commandToolsHarness) {
				manager := startTerminalManager(t, &terminal.TerminalManagerOpt{
					MaxTerminals:   1,
					MaxOutputBytes: 1 << 20,
				})
				limited := &commandToolsHarness{toolExecutor: newCommandToolExecutor(t, manager, newCommandEnvironment(t, "local", newTestWorkspace(t, t.TempDir())))}
				if _, running := limited.call(t, "bash", map[string]any{"command": "sleep 30", "wait_seconds": 0}); running.Status != "running" {
					t.Fatalf("first bash status = %q, want running", running.Status)
				}
				limited.requireError(t, "bash", map[string]any{"command": "true"}, "command_limit_reached", terminal.ErrTerminalLimitReached.Error())
			},
		},
		{
			name: "terminal_stopped",
			run: func(t *testing.T, _ *commandToolsHarness) {
				manager, err := terminal.NewTerminalManager(terminal.NewTerminalManagerOpt())
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				// ctx 已结束，Run 关闭管理器后立即返回。
				_ = manager.Run(ctx)
				stopped := &commandToolsHarness{toolExecutor: newCommandToolExecutor(t, manager, newCommandEnvironment(t, "local", newTestWorkspace(t, t.TempDir())))}
				stopped.requireError(t, "bash", map[string]any{"command": "true"}, "terminal_stopped", terminal.ErrTerminalManagerStopped.Error())
			},
		},
		{
			name: "list_commands",
			run: func(t *testing.T, h *commandToolsHarness) {
				_, local := h.call(t, "bash", map[string]any{"command": "true", "wait_seconds": 5})
				h.requireError(t, "bash", map[string]any{"command": "true", "cwd": "missing"}, "command_start_failed", "")
				if _, other := h.call(t, "bash", map[string]any{"command": "true", "executor": "other"}); other.CommandID == "" {
					t.Fatal("bash on the other executor did not start")
				}
				result, listed := h.call(t, "list_commands", map[string]any{})
				if result.Error != nil || len(listed.Commands) != 1 {
					t.Fatalf("list_commands result = %s, error = %+v, want only the started local command", result.Content, result.Error)
				}
				if got := listed.Commands[0]; got.CommandID != local.CommandID || got.Command != "true" || got.CWD != h.root || got.Status != "exited" ||
					got.ExitCode == nil || *got.ExitCode != 0 {
					t.Fatalf("listed command = %+v", got)
				}
			},
		},
		{
			name: "html_characters_stay_literal",
			run: func(t *testing.T, h *commandToolsHarness) {
				result, content := h.call(t, "bash", map[string]any{"command": "printf '<a>&'", "wait_seconds": 5})
				if result.Error != nil || content.Output != "<a>&" || !strings.Contains(string(result.Content), `"output":"<a>&"`) {
					t.Fatalf("bash result = %s, error = %+v", result.Content, result.Error)
				}
			},
		},
		{
			// 不支持 PTY 的工作区启动时就失败，不退成普通 stdin。
			name: "interactive_unsupported",
			run: func(t *testing.T, _ *commandToolsHarness) {
				virtualWorkspace, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("", nil))
				if err != nil {
					t.Fatal(err)
				}
				virtual := &commandToolsHarness{toolExecutor: newCommandToolExecutor(t, startTerminalManager(t, nil), newCommandEnvironment(t, "virtual", virtualWorkspace))}
				virtual.requireError(t, "bash", map[string]any{"command": "true", "interactive": true}, "command_start_failed", "")
			},
		},
		{
			name: "invalid_wait",
			run: func(t *testing.T, h *commandToolsHarness) {
				h.requireError(t, "bash", map[string]any{"command": "true", "wait_seconds": -1}, "invalid_arguments", "")
			},
		},
	}
	// 2. 逐项创建测试环境，执行命令并核对输出、状态或错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newCommandToolsHarness(t))
		})
	}
}

// TestBashDirectory 验证 bash 工作目录解析和越界错误。
func TestBashDirectory(t *testing.T) {
	// 1. 列出省略、相对、绝对和越界工作目录用例。
	// 用例中的 $ROOT 和 $PARENT 分别代表测试根目录及其父目录。
	tests := []struct {
		name          string
		cwd           string
		wantDirectory string
		wantError     bool
	}{
		{
			name:          "omitted_runs_in_root",
			wantDirectory: "$ROOT",
		},
		{
			name:          "relative_directory",
			cwd:           "project",
			wantDirectory: "$ROOT/project",
		},
		{
			name:          "absolute_directory_inside_root",
			cwd:           "$ROOT/other/../project",
			wantDirectory: "$ROOT/project",
		},
		{
			name:      "relative_escapes_root",
			cwd:       "../outside",
			wantError: true,
		},
		{
			name:      "absolute_outside_root",
			cwd:       "$PARENT",
			wantError: true,
		},
		{
			name:      "sibling_sharing_root_prefix",
			cwd:       "$ROOT-other",
			wantError: true,
		},
	}
	// 2. 逐项在测试环境运行 pwd，核对实际工作目录或 invalid_arguments。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCommandToolsHarness(t)
			if err := os.Mkdir(filepath.Join(h.root, "project"), 0700); err != nil {
				t.Fatal(err)
			}
			replacer := strings.NewReplacer("$ROOT", h.root, "$PARENT", filepath.Dir(h.root))
			arguments := map[string]any{"command": "pwd", "wait_seconds": 5}
			if tt.cwd != "" {
				arguments["cwd"] = replacer.Replace(tt.cwd)
			}
			if tt.wantError {
				h.requireError(t, "bash", arguments, "invalid_arguments", "")
				return
			}
			result, content := h.call(t, "bash", arguments)
			if result.Error != nil || content.Output != replacer.Replace(tt.wantDirectory)+"\n" {
				t.Fatalf("bash result = %s, error = %+v", result.Content, result.Error)
			}
		})
	}
}

// commandToolsHarness 是两个本机环境 local（默认）、other 上的命令工具，命令登记在同一个运行中的 TerminalManager。
type commandToolsHarness struct {
	// root 是 local 的工作区根目录。
	root         string
	toolExecutor executor.ToolExecutor
}

// newCommandToolsHarness 创建共享命令集合的 local 和 other 两个环境。
func newCommandToolsHarness(t *testing.T) *commandToolsHarness {
	// 1. 创建工作区和运行中的命令集合，再组装工具执行器。
	t.Helper()
	root := t.TempDir()
	manager := startTerminalManager(t, nil)
	local := newCommandEnvironment(t, "local", newTestWorkspace(t, root))
	other := newCommandEnvironment(t, "other", newTestWorkspace(t, t.TempDir()))
	return &commandToolsHarness{
		root:         root,
		toolExecutor: newCommandToolExecutor(t, manager, local, other),
	}
}

// call 执行调用并解码命令结果。
func (h *commandToolsHarness) call(t *testing.T, name string, arguments map[string]any) (*defs.ToolResult, commandToolContent) {
	// 1. 创建十秒超时的上下文，执行调用并解码正文。
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, _ := h.run(t, ctx, name, arguments, nil)
	return result, decodeCommandToolContent(t, result)
}

// requireError 执行调用并核对错误码及可选错误文本。
func (h *commandToolsHarness) requireError(t *testing.T, name string, arguments map[string]any, code, message string) {
	// 1. 调用工具，按期望核对错误。
	t.Helper()
	result, _ := h.call(t, name, arguments)
	if result.Error == nil || result.Error.Code != code || (message != "" && result.Error.Message != message) {
		t.Fatalf("%s error = %+v, want %s %q", name, result.Error, code, message)
	}
}

// run 执行一次工具调用，返回结果及全部进度。
func (h *commandToolsHarness) run(t *testing.T, ctx context.Context, name string, arguments map[string]any, onProgress func()) (*defs.ToolResult, []*executor.ExecutionProgressEvent) {
	// 1. 编码参数并构造调用 Item。
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	call := &defs.Item{
		Kind: defs.ItemToolCall,
		ToolCall: &defs.ToolCall{
			CallID:    "call-" + name,
			Name:      name,
			Arguments: string(encoded),
		},
	}
	// 2. 后台执行调用，收集进度和结果；收到进度时调用 onProgress。
	events := make(chan executor.ExecutorEvent)
	var tasks sync.WaitGroup
	tasks.Go(func() { h.toolExecutor.Run(ctx, []*defs.Item{call}, events) })
	var result *defs.ToolResult
	var progress []*executor.ExecutionProgressEvent
	for event := range events {
		switch event := event.(type) {
		case *executor.ExecutionProgressEvent:
			progress = append(progress, event)
			if onProgress != nil {
				onProgress()
			}
		case *executor.ExecutionFinishedEvent:
			result = event.Result
		}
	}
	tasks.Wait()
	if result == nil || result.CallID != call.ToolCall.CallID {
		t.Fatalf("result = %+v", result)
	}
	return result, progress
}

// commandToolContent 收下五个命令工具结果里测试关心的字段。
type commandToolContent struct {
	CommandID    string               `json:"command_id"`
	Command      string               `json:"command"`
	CWD          string               `json:"cwd"`
	Status       string               `json:"status"`
	ExitCode     *int                 `json:"exit_code"`
	StopReason   string               `json:"stop_reason"`
	Output       string               `json:"output"`
	NextCursor   uint64               `json:"next_cursor"`
	HasMore      bool                 `json:"has_more"`
	WrittenBytes int                  `json:"written_bytes"`
	Commands     []commandToolContent `json:"commands"`
}

// decodeCommandToolContent 解码命令工具返回的 JSON 正文。
func decodeCommandToolContent(t *testing.T, result *defs.ToolResult) commandToolContent {
	// 1. 解码为测试使用的命令字段，失败时结束测试。
	t.Helper()
	var content commandToolContent
	if err := json.Unmarshal(result.Content, &content); err != nil {
		t.Fatalf("result %s: %v", result.Content, err)
	}
	return content
}

// startTerminalManager 创建并运行测试命令集合。
func startTerminalManager(t *testing.T, opt *terminal.TerminalManagerOpt) terminal.TerminalManager {
	// 1. 使用传入配置或默认配置创建命令集合。
	t.Helper()
	if opt == nil {
		opt = terminal.NewTerminalManagerOpt()
	}
	manager, err := terminal.NewTerminalManager(opt)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 后台运行命令集合，测试结束时取消并等待退出。
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		_ = manager.Run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return manager
}

// inputFailingWorkspace 是启动的命令写终端总是失败的工作区，其他行为同 Workspace。
type inputFailingWorkspace struct {
	workspace.Workspace
}

// StartProcess 启动真实进程，包成写终端总是失败的进程。
func (w *inputFailingWorkspace) StartProcess(ctx context.Context, request *workspace.ProcessRequest) (workspace.Process, error) {
	// 1. 启动进程，成功后返回写入失败的进程对象。
	process, err := w.Workspace.StartProcess(ctx, request)
	if err != nil {
		return nil, err
	}
	return &inputFailingProcess{Process: process}, nil
}

// inputFailingProcess 写终端总是返回错误，其他行为同 Process。
type inputFailingProcess struct {
	workspace.Process
}

// WriteTerminalInput 返回终端输入失败。
func (p *inputFailingProcess) WriteTerminalInput([]byte) (int, error) {
	// 1. 返回零写入字节和预设错误。
	return 0, errors.New("terminal input broken")
}

// newCommandEnvironment 创建带指定工具的执行环境。
func newCommandEnvironment(t *testing.T, id string, environmentWorkspace workspace.Workspace, environmentTools ...executor.Tool) executor.ExecutionEnvironment {
	// 1. 按 ID、工作区和工具列表创建环境。
	t.Helper()
	environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(id, "", environmentWorkspace, environmentTools...))
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

// newCommandToolExecutor 为各环境创建绑定 Session 命令集合的工具执行器。
func newCommandToolExecutor(t *testing.T, manager terminal.TerminalManager, environments ...executor.ExecutionEnvironment) executor.ToolExecutor {
	// 1. 逐环境创建五个命令工具，记入 SessionTools 后构造执行器。
	t.Helper()
	toolExecutorOpt := executor.NewToolExecutorOpt(environments)
	toolExecutorOpt.SessionTools = make(map[string][]executor.Tool, len(environments))
	for _, environment := range environments {
		commandTools, err := tools.NewCommandTools(tools.NewCommandToolsOpt(environment.GetID(), environment.GetWorkspace(), manager))
		if err != nil {
			t.Fatal(err)
		}
		toolExecutorOpt.SessionTools[environment.GetID()] = commandTools
	}
	toolExecutor, err := executor.NewToolExecutor(toolExecutorOpt)
	if err != nil {
		t.Fatal(err)
	}
	return toolExecutor
}
