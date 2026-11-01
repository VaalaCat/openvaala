package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
)

// interruptedCommand 是第三轮启动的命令，Turn 在它第一次等待时被中断；它先输出 waiting，测试据此知道 bash 已经在等。
const interruptedCommand = "echo waiting; sleep 30"

// TestTerminalCommands 检查后台命令跨 Turn 的启动、读取、中断、恢复和停止。
func TestTerminalCommands(t *testing.T) {
	// 1. 构建服务，分别测试普通命令和 PTY 命令。
	binary := buildOpenVaala(t)
	for _, test := range []struct {
		name        string
		interactive bool
		// command 是第一轮 bash 启动的命令，在工作区根目录运行。
		command string
		// wantSecond 是创建 ready 文件之后续读要读到的输出。
		wantSecond string
	}{
		{
			name:       "plain",
			command:    "echo first; until [ -f ready ]; do sleep 0.1; done; echo second",
			wantSecond: "second",
		},
		{
			name:        "pty",
			interactive: true,
			command:     `echo first; read line; until [ -f ready ]; do sleep 0.1; done; echo "second $line"`,
			wantSecond:  "second hello",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 打开对话并取得工作区目录。
			f := newConversationManagementFixture(t, binary)
			observer := f.openNewConversation(t)
			workspace := observer.conversation.ExecutorSettings.WorkspaceDirectories[defs.LocalExecutorID]

			// 2. 第一轮启动命令，检查等待 1 秒后返回 running 和命令编号。
			observer.startTurn(t, "start the command")
			f.model.next(t).reply <- e2eModelReply{
				tool: "bash",
				arguments: encodeE2EArguments(t, map[string]any{
					"command": test.command, "interactive": test.interactive, "wait_seconds": 1,
				}),
			}
			started := readCommandToolResult(t, observer)
			requireCommandResultFields(t, "bash", started)
			if started.Status != "running" || started.CommandID == "" {
				t.Fatalf("bash result = %+v, want a running command with its ID", started)
			}
			f.model.next(t).reply <- e2eModelReply{text: "started"}
			readTurnFinished(t, observer, defs.TurnStatusCompleted)

			// 3. 第二轮 PTY 命令先写入输入，再按编号读到 first。
			observer.startTurn(t, "read the output")
			if test.interactive {
				f.model.next(t).reply <- e2eModelReply{
					tool: "write_stdin",
					arguments: encodeE2EArguments(t, map[string]any{
						"command_id": started.CommandID, "chars": "hello\n",
					}),
				}
				if written := readCommandToolResult(t, observer); written.Status != "running" || written.WrittenBytes != len("hello\n") {
					t.Fatalf("write_stdin result = %+v, want %d bytes written to the running command", written, len("hello\n"))
				}
			}
			f.model.next(t).reply <- e2eModelReply{
				tool: "read_command_output",
				arguments: encodeE2EArguments(t, map[string]any{
					"command_id": started.CommandID, "wait_seconds": 1,
				}),
			}
			firstPage := readCommandToolResult(t, observer)
			requireCommandResultFields(t, "read_command_output", firstPage)
			if firstPage.Status != "running" || !strings.Contains(firstPage.Output, "first") {
				t.Fatalf("read_command_output result = %+v, want first from the running command", firstPage)
			}
			f.model.next(t).reply <- e2eModelReply{text: "read"}
			readTurnFinished(t, observer, defs.TurnStatusCompleted)

			// 4. 第三轮启动另一条命令，检查中断后 Turn 在 10 秒内结束。
			observer.startTurn(t, "start another command")
			f.model.next(t).reply <- e2eModelReply{
				tool: "bash",
				arguments: encodeE2EArguments(t, map[string]any{
					"command": interruptedCommand, "wait_seconds": 30,
				}),
			}
			waitToolProgress(t, observer, "waiting")
			interruptedAt := time.Now()
			if turnID, err := f.control.InterruptTurn(t.Context(), observer.conversation.ID); err != nil || turnID == "" {
				t.Fatalf("InterruptTurn() = %q, %v; want the running turn", turnID, err)
			}
			readTurnFinished(t, observer, defs.TurnStatusInterrupted)
			if elapsed := time.Since(interruptedAt); elapsed > 10*time.Second {
				t.Fatalf("interrupted turn took %s to finish, want bash to stop waiting soon", elapsed)
			}

			// 5. 第四轮列出命令找回被中断的那条，创建 ready 文件并续读第一条命令直到退出。
			observer.startTurn(t, "finish up")
			f.model.next(t).reply <- e2eModelReply{
				tool:      "list_commands",
				arguments: "{}",
			}
			listed := readCommandToolResult(t, observer)
			index := slices.IndexFunc(listed.Commands, func(command e2eCommandInfo) bool { return command.Command == interruptedCommand })
			if len(listed.Commands) != 2 || index < 0 || listed.Commands[index].Status != "running" {
				t.Fatalf("list_commands result = %+v, want both commands with %q still running", listed, interruptedCommand)
			}
			if err := os.WriteFile(filepath.Join(workspace, "ready"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			// 每次从上一页的 next_cursor 继续读取，直到命令结束。
			var output strings.Builder
			lastPage := firstPage
			for attempt := 0; lastPage.Status == "running"; attempt++ {
				if attempt == 10 {
					t.Fatalf("command still running after %d reads, read %q so far", attempt, output.String())
				}
				f.model.next(t).reply <- e2eModelReply{
					tool: "read_command_output",
					arguments: encodeE2EArguments(t, map[string]any{
						"command_id": started.CommandID, "cursor": lastPage.NextCursor, "wait_seconds": 5,
					}),
				}
				lastPage = readCommandToolResult(t, observer)
				requireCommandResultFields(t, "read_command_output", lastPage)
				output.WriteString(lastPage.Output)
			}
			if lastPage.Status != "exited" || lastPage.ExitCode == nil || *lastPage.ExitCode != 0 || !strings.Contains(output.String(), test.wantSecond) {
				t.Fatalf("last read_command_output result = %+v after reading %q, want the command exited with 0 after printing %q", lastPage, output.String(), test.wantSecond)
			}
			// 6. 停止被中断的命令，检查最后一轮正常结束。
			f.model.next(t).reply <- e2eModelReply{
				tool: "stop_command",
				arguments: encodeE2EArguments(t, map[string]any{
					"command_id": listed.Commands[index].CommandID,
				}),
			}
			if stopped := readCommandToolResult(t, observer); stopped.Status != "exited" || stopped.StopReason != "stopped" {
				t.Fatalf("stop_command result = %+v, want the interrupted command stopped", stopped)
			}
			f.model.next(t).reply <- e2eModelReply{text: "all done"}
			readTurnFinished(t, observer, defs.TurnStatusCompleted)
		})
	}
}

// e2eCommandResult 是 runtimeclient 收到的一次命令工具结果：fields 是结果 JSON 的原始字段，其余是从中解出的值。
type e2eCommandResult struct {
	fields       map[string]json.RawMessage
	CommandID    string           `json:"command_id"`
	Status       string           `json:"status"`
	ExitCode     *int             `json:"exit_code"`
	StopReason   string           `json:"stop_reason"`
	Output       string           `json:"output"`
	NextCursor   uint64           `json:"next_cursor"`
	WrittenBytes int              `json:"written_bytes"`
	Commands     []e2eCommandInfo `json:"commands"`
}

// e2eCommandInfo 是 list_commands 结果里的一条命令。
type e2eCommandInfo struct {
	CommandID string `json:"command_id"`
	Command   string `json:"command"`
	Status    string `json:"status"`
}

// encodeE2EArguments 把工具参数编成假模型工具调用里的 JSON 文本。
func encodeE2EArguments(t *testing.T, arguments map[string]any) string {
	t.Helper()
	// 1. 将参数编码为 JSON；编码失败时结束测试。
	payload, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// readCommandToolResult 读取并解码观察连接收到的命令工具结果。
func readCommandToolResult(t *testing.T, observer *runtimeObserver) e2eCommandResult {
	t.Helper()
	// 1. 等待已保存的工具结果，检查调用成功并解码结果字段。
	for {
		saved, isSaved := observer.next(t).(*runtimeclient.ItemsSaved)
		if !isSaved {
			continue
		}
		for _, logged := range saved.Items {
			if logged.Item.Kind != defs.ItemToolResult {
				continue
			}
			toolResult := logged.Item.ToolResult
			if toolResult.Error != nil {
				t.Fatalf("tool result %s failed: %s: %s", toolResult.CallID, toolResult.Error.Code, toolResult.Error.Message)
			}
			var result e2eCommandResult
			if err := json.Unmarshal(toolResult.Content, &result); err != nil {
				t.Fatalf("tool result %s: %v", toolResult.Content, err)
			}
			if err := json.Unmarshal(toolResult.Content, &result.fields); err != nil {
				t.Fatalf("tool result %s: %v", toolResult.Content, err)
			}
			return result
		}
	}
}

// requireCommandResultFields 检查命令工具结果的必要字段。
func requireCommandResultFields(t *testing.T, toolName string, result e2eCommandResult) {
	t.Helper()
	// 1. 检查 bash、read_command_output 的结果带 status、exit_code、next_cursor。
	for _, field := range []string{"status", "exit_code", "next_cursor"} {
		if _, found := result.fields[field]; !found {
			t.Fatalf("%s result fields = %v, want %s", toolName, result.fields, field)
		}
	}
	// 2. 检查运行中的退出码为 null。
	if result.Status == "running" && string(result.fields["exit_code"]) != "null" {
		t.Fatalf("%s running result exit_code = %s, want null", toolName, result.fields["exit_code"])
	}
}

// waitToolProgress 读到 observer 的工具执行输出里出现 text 为止。
func waitToolProgress(t *testing.T, observer *runtimeObserver, text string) {
	t.Helper()
	// 1. 累积工具进度输出，读到指定文字后返回。
	var output strings.Builder
	for !strings.Contains(output.String(), text) {
		if progress, isProgress := observer.next(t).(*runtimeclient.ToolProgress); isProgress {
			output.WriteString(executor.GetExecutionOutputText(progress.Progress.Content))
		}
	}
}
