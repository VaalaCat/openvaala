package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// acpProcessTest 是一个真实进程场景的环境：临时 HOME、使用空闲端口的配置、可控的模型服务和编译好的二进制。
// 测试结束时执行 serve stop，并确认没有留下 serve 进程。
type acpProcessTest struct {
	binary     string
	configPath string
	listen     string
	model      *acpModel
	// executables 是本场景里可能拉起 serve 的可执行文件，结束时确认它们都没有留下 serve 进程。
	executables []string
}

// TestACPCommandProcess 检查真实 ACP 进程的会话、共享服务、工具和退出行为。
func TestACPCommandProcess(t *testing.T) {
	// 1. 编译程序并列出进程场景。
	binary := buildTestBinary(t)
	tests := []struct {
		name string
		run  func(t *testing.T, test *acpProcessTest)
	}{
		{
			name: "restart_and_continue",
			run: func(t *testing.T, test *acpProcessTest) {
				first, second := t.TempDir(), t.TempDir()
				writeTestFiles(t, map[string]string{filepath.Join(first, "notes.txt"): "FIRST_MARKER", filepath.Join(second, "notes.txt"): "SECOND_MARKER"})
				editor := startACPProcess(t, test.binary, test.configPath)
				editor.wire.initialize(t)
				id := editor.wire.newSession(t, first)
				promptID := editor.wire.prompt(t, id, "read file")
				test.model.next(t).reply <- acpModelReply{
					tool:      "read_file",
					arguments: `{"path":"notes.txt"}`,
				}
				request := test.model.next(t)
				if !strings.Contains(fmt.Sprint(request.body), "FIRST_MARKER") {
					t.Fatal("process did not execute read_file in the session directory")
				}
				request.reply <- acpModelReply{text: "saved answer"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				promptID = editor.wire.prompt(t, id, "cancel me")
				test.model.next(t).reply <- acpModelReply{
					text: "partial",
					hold: true,
				}
				editor.wire.notify(t, "session/cancel", acp.CancelNotification{SessionId: id})
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonCancelled)
				promptID = editor.wire.prompt(t, id, "continue")
				test.model.next(t).reply <- acpModelReply{text: "continued"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				if err := editor.close(t); err != nil {
					t.Fatalf("acp exited with %v\n%s", err, editor.getStderr())
				}
				requireACPServeStop(t, test, "stopped\n")
				// 重启 acp 和 serve，改写已保存目录里的文件，再检查加载后工具读取的是新内容。
				writeTestFiles(t, map[string]string{filepath.Join(first, "notes.txt"): "RECORDED_DIRECTORY_MARKER"})
				editor = startACPProcess(t, test.binary, test.configPath)
				editor.wire.initialize(t)
				var listed acp.ListSessionsResponse
				editor.wire.call(t, "session/list", acp.ListSessionsRequest{}, &listed)
				if len(listed.Sessions) != 1 || listed.Sessions[0].SessionId != id {
					t.Fatalf("list = %+v", listed)
				}
				editor.wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       second,
				}, nil)
				if text := getACPAgentText(editor.wire); !strings.Contains(text, "saved answer") || !strings.Contains(text, "continued") {
					t.Fatalf("replay = %q", text)
				}
				promptID = editor.wire.prompt(t, id, "read file again")
				test.model.next(t).reply <- acpModelReply{
					tool:      "read_file",
					arguments: `{"path":"notes.txt"}`,
				}
				request = test.model.next(t)
				if body := fmt.Sprint(request.body); !strings.Contains(body, "RECORDED_DIRECTORY_MARKER") || strings.Contains(body, "SECOND_MARKER") {
					t.Fatal("tool did not run in the recorded directory after the cold load")
				}
				request.reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
			},
		},
		{
			name: "sdk_client",
			run: func(t *testing.T, test *acpProcessTest) {
				editor := startACPRawProcess(t, test.binary, test.configPath)
				client := &acpSDKClient{updates: make(chan acp.SessionNotification, 256)}
				connection := acp.NewClientSideConnection(client, editor.input, editor.output)
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				if _, err := connection.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 1}); err != nil {
					t.Fatal(err)
				}
				session, err := connection.NewSession(ctx, acp.NewSessionRequest{
					Cwd:        t.TempDir(),
					McpServers: []acp.McpServer{},
				})
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					response, err := connection.Prompt(ctx, acp.PromptRequest{
						SessionId: session.SessionId,
						Prompt:    []acp.ContentBlock{acp.TextBlock("SDK prompt")},
					})
					if err == nil && response.StopReason != acp.StopReasonEndTurn {
						err = fmt.Errorf("stop reason = %s", response.StopReason)
					}
					done <- err
				}()
				test.model.next(t).reply <- acpModelReply{text: "SDK response"}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if err := editor.close(t); err != nil {
					t.Fatalf("acp exited with %v\n%s", err, editor.getStderr())
				}
				select {
				case <-connection.Done():
				case <-time.After(time.Second):
					t.Fatal("SDK connection did not close")
				}
			},
		},
		{
			name: "two_editors_share_runtime",
			run: func(t *testing.T, test *acpProcessTest) {
				directory := t.TempDir()
				first := startACPProcess(t, test.binary, test.configPath)
				first.wire.initialize(t)
				id := first.wire.newSession(t, directory)
				promptID := first.wire.prompt(t, id, "shared turn")
				chunks := make(chan string)
				test.model.next(t).reply <- acpModelReply{stream: chunks}
				chunks <- "BEFORE_LOAD "
				waitACPAgentChunk(t, first.wire)
				second := startACPProcess(t, test.binary, test.configPath)
				second.wire.initialize(t)
				second.wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       directory,
				}, nil)
				chunks <- "AFTER_LOAD"
				for !strings.Contains(getACPAgentText(second.wire), "AFTER_LOAD") {
					second.wire.next(t)
				}
				close(chunks)
				assertACPPromptResult(t, first.wire.response(t, promptID), acp.StopReasonEndTurn)
				if text := getACPAgentText(second.wire); text != "BEFORE_LOAD AFTER_LOAD" {
					t.Fatalf("second editor text = %q", text)
				}
			},
		},
		{
			name: "turn_continues_after_editor_closes",
			run: func(t *testing.T, test *acpProcessTest) {
				directory := t.TempDir()
				first := startACPProcess(t, test.binary, test.configPath)
				first.wire.initialize(t)
				id := first.wire.newSession(t, directory)
				first.wire.prompt(t, id, "keep running")
				chunks := make(chan string)
				test.model.next(t).reply <- acpModelReply{stream: chunks}
				chunks <- "BEFORE_CLOSE "
				waitACPAgentChunk(t, first.wire)
				if err := first.close(t); err != nil {
					t.Fatalf("acp exited with %v\n%s", err, first.getStderr())
				}
				requireACPServeReachable(t, test, true)
				chunks <- "AFTER_CLOSE"
				close(chunks)
				requireACPServeReachable(t, test, true)
				second := startACPProcess(t, test.binary, test.configPath)
				second.wire.initialize(t)
				second.wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       directory,
				}, nil)
				// 等待加载中的 Turn 完成保存并收到完整回答。
				for !strings.Contains(getACPAgentText(second.wire), "BEFORE_CLOSE AFTER_CLOSE") {
					second.wire.next(t)
				}
				requireACPServeReachable(t, test, true)
			},
		},
		{
			name: "load_running_from_other_directory",
			run: func(t *testing.T, test *acpProcessTest) {
				first, second := t.TempDir(), t.TempDir()
				writeTestFiles(t, map[string]string{filepath.Join(first, "notes.txt"): "FIRST_MARKER", filepath.Join(second, "notes.txt"): "SECOND_MARKER"})
				editor := startACPProcess(t, test.binary, test.configPath)
				editor.wire.initialize(t)
				id := editor.wire.newSession(t, first)
				promptID := editor.wire.prompt(t, id, "hello")
				test.model.next(t).reply <- acpModelReply{text: "hi"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				other := startACPProcess(t, test.binary, test.configPath)
				other.wire.initialize(t)
				other.wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       second,
				}, nil)
				promptID = other.wire.prompt(t, id, "read notes")
				test.model.next(t).reply <- acpModelReply{
					tool:      "read_file",
					arguments: `{"path":"notes.txt"}`,
				}
				request := test.model.next(t)
				if body := fmt.Sprint(request.body); !strings.Contains(body, "FIRST_MARKER") || strings.Contains(body, "SECOND_MARKER") {
					t.Fatalf("running conversation did not keep its directory: %s", body)
				}
				request.reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, other.wire.response(t, promptID), acp.StopReasonEndTurn)
				if !hasACPToolLocation(other.wire, filepath.Join(first, "notes.txt")) {
					t.Fatal("tool location is not the absolute path in the running directory")
				}
			},
		},
		{
			name: "serve_stop_fails_prompt",
			run: func(t *testing.T, test *acpProcessTest) {
				editor := startACPProcess(t, test.binary, test.configPath)
				editor.wire.initialize(t)
				id := editor.wire.newSession(t, t.TempDir())
				promptID := editor.wire.prompt(t, id, "hold")
				test.model.next(t).reply <- acpModelReply{
					text: "partial",
					hold: true,
				}
				waitACPAgentChunk(t, editor.wire)
				requireACPServeStop(t, test, "stopped\n")
				result := editor.wire.response(t, promptID)
				if result.Error == nil || result.Error.Code != -32603 || result.Error.Message != "connection to runtime server lost" {
					t.Fatalf("prompt = %+v, want -32603 connection to runtime server lost", result)
				}
				if err := editor.wait(t, 10*time.Second); err == nil {
					t.Fatal("acp exited with status 0 after losing the runtime server")
				}
				requireACPServeReachable(t, test, false)
			},
		},
		{
			name: "startup_failure",
			run: func(t *testing.T, test *acpProcessTest) {
				occupied, err := net.Listen("tcp", test.listen)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = occupied.Close() })
				started := time.Now()
				editor := startACPProcess(t, test.binary, test.configPath)
				if err := editor.wait(t, 15*time.Second); err == nil {
					t.Fatal("acp exited with status 0 without a runtime server")
				}
				if elapsed := time.Since(started); elapsed > 10*time.Second {
					t.Fatalf("acp exited after %s, want within 10s", elapsed)
				}
				if stderr := editor.getStderr(); !strings.Contains(stderr, "runtime server exited") {
					t.Fatalf("stderr = %q, want runtime server exited", stderr)
				}
			},
		},
		{
			name: "large_image",
			run: func(t *testing.T, test *acpProcessTest) {
				directory := t.TempDir()
				image := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("image"), (6<<20)/4*3/5))
				editor := startACPProcess(t, test.binary, test.configPath)
				editor.wire.initialize(t)
				id := editor.wire.newSession(t, directory)
				promptID := editor.wire.send(t, "session/prompt", acp.PromptRequest{
					SessionId: id,
					Prompt:    []acp.ContentBlock{acp.TextBlock("describe"), acp.ImageBlock(image, "image/png")},
				})
				request := test.model.next(t)
				if !strings.Contains(fmt.Sprint(request.body), image) {
					t.Fatal("model request is missing the image")
				}
				request.reply <- acpModelReply{text: "REPLY_0"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				for round := 1; round <= 3; round++ {
					promptID = editor.wire.prompt(t, id, fmt.Sprintf("round %d", round))
					test.model.next(t).reply <- acpModelReply{text: fmt.Sprintf(" REPLY_%d", round)}
					assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				}
				other := startACPProcess(t, test.binary, test.configPath)
				other.wire.initialize(t)
				other.wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       directory,
				}, nil)
				images := 0
				for _, notification := range other.wire.updates {
					if chunk := notification.Update.UserMessageChunk; chunk != nil && chunk.Content.Image != nil && chunk.Content.Image.Data == image {
						images++
					}
				}
				if text := getACPAgentText(other.wire); images != 1 || text != "REPLY_0 REPLY_1 REPLY_2 REPLY_3" {
					t.Fatalf("replay images = %d, text = %q", images, text)
				}
			},
		},
		{
			// 两个编辑器使用不同环境，工具仍使用首次启动 serve 的环境。
			name: "tools_use_serve_environment",
			run: func(t *testing.T, test *acpProcessTest) {
				runEcho := func(editor *acpProcess, id acp.SessionId) string {
					t.Helper()
					promptID := editor.wire.prompt(t, id, "print the mark")
					test.model.next(t).reply <- acpModelReply{
						tool:      "bash",
						arguments: `{"command":"echo $TOOL_ENV_MARK"}`,
					}
					test.model.next(t).reply <- acpModelReply{text: "done"}
					assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
					return getACPLastToolOutput(editor.wire)
				}
				first := startACPProcess(t, test.binary, test.configPath, "TOOL_ENV_MARK=from_a")
				first.wire.initialize(t)
				firstID := first.wire.newSession(t, t.TempDir())
				if output := runEcho(first, firstID); output != "from_a" {
					t.Fatalf("first editor tool output = %q, want from_a", output)
				}
				second := startACPProcess(t, test.binary, test.configPath, "TOOL_ENV_MARK=from_b")
				second.wire.initialize(t)
				secondID := second.wire.newSession(t, t.TempDir())
				if output := runEcho(second, secondID); output != "from_a" {
					t.Fatalf("second editor tool output = %q, want the serve environment from_a", output)
				}
			},
		},
		{
			name: "stale_serve_notice",
			run: func(t *testing.T, test *acpProcessTest) {
				original := copyTestBinary(t, test.binary)
				copied := copyTestBinary(t, test.binary)
				test.executables = append(test.executables, original, copied)
				first := startACPProcess(t, original, test.configPath)
				first.wire.initialize(t)
				first.wire.newSession(t, t.TempDir())
				other := startACPProcess(t, copied, test.configPath)
				other.wire.initialize(t)
				other.wire.newSession(t, t.TempDir())
				same := startACPProcess(t, original, test.configPath)
				same.wire.initialize(t)
				for _, editor := range []*acpProcess{first, other, same} {
					if err := editor.close(t); err != nil {
						t.Fatalf("acp exited with %v\n%s", err, editor.getStderr())
					}
				}
				const notice = "runtime server was started from a different openvaala executable"
				if !strings.Contains(other.getStderr(), notice) {
					t.Fatalf("copied executable stderr = %q, want the notice", other.getStderr())
				}
				if strings.Contains(same.getStderr(), notice) {
					t.Fatalf("serve executable stderr = %q, want no notice", same.getStderr())
				}
			},
		},
		{
			// 改选 extra Executor 后，检查 bash 工作目录和模型收到的变更说明。
			name: "executor_option_switches_tools",
			run: func(t *testing.T, test *acpProcessTest) {
				first, second := t.TempDir(), t.TempDir()
				writeTestFiles(t, map[string]string{filepath.Join(filepath.Dir(test.configPath), "executors", "extra.yaml"): "id: extra\ndescription: Second local directory\nkind: local\nworkspace_directory: " + second + "\n"})
				editor := startACPProcess(t, test.binary, test.configPath)
				editor.wire.initialize(t)
				id := editor.wire.newSession(t, first)
				promptID := editor.wire.prompt(t, id, "hello")
				test.model.next(t).reply <- acpModelReply{text: "hi"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				var response acp.SetSessionConfigOptionResponse
				editor.wire.call(t, "session/set_config_option", map[string]any{"sessionId": id, "configId": "executor", "value": "extra"}, &response)
				if current, _ := getACPSelectOption(response.ConfigOptions, "executor"); current != "extra" {
					t.Fatalf("executor option = %q, want extra", current)
				}
				promptID = editor.wire.prompt(t, id, "where am I")
				request := test.model.next(t)
				if !strings.Contains(requestText(request.body), "Executor 已变更") {
					t.Fatalf("model request does not mention the executor change: %s", requestText(request.body))
				}
				request.reply <- acpModelReply{
					tool:      "bash",
					arguments: `{"command":"pwd"}`,
				}
				test.model.next(t).reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				if output := getACPLastToolOutput(editor.wire); output != second {
					t.Fatalf("bash pwd = %q, want the extra executor directory %s", output, second)
				}
			},
		},
		{
			// 使用不存在的客户端路径新建和筛选会话，工具使用映射后的 serve 路径，列表返回客户端路径。
			name: "path_mappings_new_and_list",
			run: func(t *testing.T, test *acpProcessTest) {
				clientRoot, serverRoot := "/openvaala-acp-client/projects", t.TempDir()
				appendConfigText(t, test.configPath, fmt.Sprintf("client:\n  path_mappings:\n    - client: %q\n      server: %q\n", clientRoot, serverRoot))
				clientDirectory, serverDirectory := clientRoot+"/app", filepath.Join(serverRoot, "app")
				if err := os.Mkdir(serverDirectory, 0700); err != nil {
					t.Fatal(err)
				}
				editor := startACPProcess(t, test.binary, test.configPath)
				editor.wire.initialize(t)
				id := editor.wire.newSession(t, clientDirectory)
				promptID := editor.wire.prompt(t, id, "where am I")
				test.model.next(t).reply <- acpModelReply{
					tool:      "bash",
					arguments: `{"command":"pwd"}`,
				}
				test.model.next(t).reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, editor.wire.response(t, promptID), acp.StopReasonEndTurn)
				if output := getACPLastToolOutput(editor.wire); output != serverDirectory {
					t.Fatalf("bash pwd = %q, want the serve path %s", output, serverDirectory)
				}
				for _, tc := range []struct {
					name string
					cwd  string
					want []acp.SessionId
				}{
					{
						name: "mapped_directory",
						cwd:  clientDirectory,
						want: []acp.SessionId{id},
					},
					{
						name: "other_mapped_directory",
						cwd:  clientRoot + "/other",
						want: []acp.SessionId{},
					},
				} {
					var listed acp.ListSessionsResponse
					editor.wire.call(t, "session/list", acp.ListSessionsRequest{Cwd: &tc.cwd}, &listed)
					ids := []acp.SessionId{}
					for _, session := range listed.Sessions {
						if session.Cwd != clientDirectory {
							t.Fatalf("%s: listed session cwd = %q, want the editor directory %s", tc.name, session.Cwd, clientDirectory)
						}
						ids = append(ids, session.SessionId)
					}
					if !slices.Equal(ids, tc.want) {
						t.Fatalf("%s: session/list by %s = %v, want %v", tc.name, tc.cwd, ids, tc.want)
					}
				}
			},
		},
		{
			name: "two_acp_start_together",
			run: func(t *testing.T, test *acpProcessTest) {
				editors := []*acpProcess{startACPProcess(t, test.binary, test.configPath), startACPProcess(t, test.binary, test.configPath)}
				var ids []acp.SessionId
				for _, editor := range editors {
					editor.wire.initialize(t)
					id := editor.wire.newSession(t, t.TempDir())
					saveACPSession(t, editor.wire, test.model, id)
					ids = append(ids, id)
				}
				for _, editor := range editors {
					var listed acp.ListSessionsResponse
					editor.wire.call(t, "session/list", acp.ListSessionsRequest{}, &listed)
					seen := map[acp.SessionId]bool{}
					for _, session := range listed.Sessions {
						seen[session.SessionId] = true
					}
					if len(seen) != 2 || !seen[ids[0]] || !seen[ids[1]] {
						t.Fatalf("list = %+v, want sessions %v", listed, ids)
					}
				}
			},
		},
	}
	// 2. 创建独立 HOME、模型服务和服务配置，执行场景后停止 serve 并检查残留进程。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			_, configPath := newACPConfig(t, model.server.URL)
			listen := getFreeServeAddress(t)
			appendServerConfig(t, configPath, listen, "")
			test := &acpProcessTest{
				binary:      binary,
				configPath:  configPath,
				listen:      listen,
				model:       model,
				executables: []string{binary},
			}
			// 收尾使用独立 context 停止 serve，并检查残留进程。
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if _, err := runCLIWithContext(ctx, configPath, "serve", "stop"); err != nil {
					t.Errorf("serve stop: %v", err)
				}
				requireNoServeProcess(t, test.executables...)
			})
			tt.run(t, test)
		})
	}
}

// acpProcess 是一个真实的 openvaala acp 进程：wire 驱动它的 stdio，stderr 在进程退出后可读。
type acpProcess struct {
	wire   *acpWire
	input  io.WriteCloser
	output io.ReadCloser
	stderr bytes.Buffer
	done   chan error
	// isExited 在 wait 取到退出结果后为 true，exitErr 是这个结果。
	isExited bool
	exitErr  error
}

// startACPProcess 启动 ACP 进程并接入测试协议通道。
func startACPProcess(t *testing.T, binary, configPath string, environment ...string) *acpProcess {
	t.Helper()
	// 1. 启动原始进程，再创建读写标准流的协议通道。
	editor := startACPRawProcess(t, binary, configPath, environment...)
	editor.wire = newACPWire(t, editor.input, editor.output)
	return editor
}

// startACPRawProcess 启动独立 ACP 进程，将标准流交给测试读写。
func startACPRawProcess(t *testing.T, binary, configPath string, environment ...string) *acpProcess {
	t.Helper()
	// 1. 设置配置路径和叠加环境，建立标准流管道。
	command := exec.Command(binary, "--config-file", configPath, "acp")
	command.Env = append(os.Environ(), environment...)
	editor := &acpProcess{done: make(chan error, 1)}
	command.Stderr = &editor.stderr
	var err error
	if editor.input, err = command.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if editor.output, err = command.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	// 2. 启动进程并异步等待，登记关闭输入和限时等待退出的收尾操作。
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { editor.done <- command.Wait() }()
	t.Cleanup(func() {
		_ = editor.input.Close()
		if !editor.isExited {
			select {
			case editor.exitErr = <-editor.done:
				editor.isExited = true
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				<-editor.done
				t.Errorf("acp process did not stop on EOF\n%s", editor.stderr.String())
			}
		}
	})
	return editor
}

// wait 限时等待 ACP 进程退出。
func (p *acpProcess) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	// 1. 尚未退出时等待并保存执行结果，超时则结束测试，再次调用时返回已保存的结果。
	if !p.isExited {
		select {
		case p.exitErr = <-p.done:
			p.isExited = true
		case <-time.After(timeout):
			t.Fatalf("acp process did not exit within %s", timeout)
		}
	}
	return p.exitErr
}

// close 关闭 ACP 输入并等待进程退出。
func (p *acpProcess) close(t *testing.T) error {
	t.Helper()
	// 1. 关闭 stdin，最多等 10 秒返回进程结果。
	_ = p.input.Close()
	return p.wait(t, 10*time.Second)
}

// getStderr 读取测试进程的标准错误内容。
func (p *acpProcess) getStderr() string {
	// 1. 在进程退出后读取已保存的 stderr 文本。
	return p.stderr.String()
}

// requireACPServeStop 使用测试二进制停止 serve 并检查输出。
func requireACPServeStop(t *testing.T, test *acpProcessTest, wantStdout string) {
	t.Helper()
	// 1. 执行 serve stop，检查命令成功且标准输出匹配。
	output, err := exec.Command(test.binary, "--config-file", test.configPath, "serve", "stop").Output()
	if err != nil || string(output) != wantStdout {
		t.Fatalf("serve stop = %q, %v; want %q", output, err, wantStdout)
	}
}

// requireACPServeReachable 检查配置端口上的 serve 是否符合预期的可访问状态。
func requireACPServeReachable(t *testing.T, test *acpProcessTest, want bool) {
	t.Helper()
	// 1. 向 healthz 发起超时 1 秒的请求，关闭响应正文后比较可访问状态。
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + test.listen + "/api/v1/healthz")
	isReachable := err == nil && response.StatusCode == http.StatusOK
	if err == nil {
		_ = response.Body.Close()
	}
	if isReachable != want {
		t.Fatalf("healthz reachable = %v (%v), want %v", isReachable, err, want)
	}
}

// requireNoServeProcess 检查指定二进制启动的 serve 进程均已退出。
func requireNoServeProcess(t *testing.T, binaries ...string) {
	t.Helper()
	// 1. 逐个二进制轮询 pgrep，最多等待 5 秒，仍有匹配进程则报告失败。
	for _, binary := range binaries {
		pattern := "^" + regexp.QuoteMeta(binary) + " .*serve"
		deadline := time.Now().Add(5 * time.Second)
		for {
			output, err := exec.Command("pgrep", "-af", pattern).Output()
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				break
			}
			if err != nil {
				t.Fatalf("pgrep: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("serve process left running:\n%s", output)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// copyTestBinary 将测试二进制复制到新的临时路径。
func copyTestBinary(t *testing.T, binary string) string {
	t.Helper()
	// 1. 读取二进制，以 0755 写入新临时目录并返回路径。
	content, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "openvaala")
	if err := os.WriteFile(copied, content, 0755); err != nil {
		t.Fatal(err)
	}
	return copied
}

// getACPAgentText 拼接测试通道已收到的 Agent 消息文本。
func getACPAgentText(wire *acpWire) string {
	// 1. 按通知顺序拼接全部 agent_message_chunk。
	var text strings.Builder
	for _, notification := range wire.updates {
		if chunk := notification.Update.AgentMessageChunk; chunk != nil {
			text.WriteString(chunk.Content.Text.Text)
		}
	}
	return text.String()
}

// getACPLastToolOutput 读取最后一次工具完成通知的正文。
func getACPLastToolOutput(wire *acpWire) string {
	// 1. 遍历通知，保留最近一次已完成且包含内容的工具输出，去掉首尾空白。
	output := ""
	for _, notification := range wire.updates {
		tool := notification.Update.ToolCallUpdate
		if tool == nil || tool.Status == nil || *tool.Status != acp.ToolCallStatusCompleted || len(tool.Content) == 0 {
			continue
		}
		output = strings.TrimSpace(tool.Content[0].Content.Content.Text.Text)
	}
	return output
}

// hasACPToolLocation 工具通知是否包含指定文件位置？
func hasACPToolLocation(wire *acpWire, path string) bool {
	// 1. 遍历工具开始和更新通知中的文件位置，发现匹配路径时返回 true。
	for _, notification := range wire.updates {
		var locations []acp.ToolCallLocation
		if tool := notification.Update.ToolCall; tool != nil {
			locations = tool.Locations
		}
		if tool := notification.Update.ToolCallUpdate; tool != nil {
			locations = append(locations, tool.Locations...)
		}
		for _, location := range locations {
			if location.Path == path {
				return true
			}
		}
	}
	return false
}

// acpSDKClient 保存 SDK 通知通道；未实现的方法使用内嵌的 nil 接口。
type acpSDKClient struct {
	acp.Client
	updates chan acp.SessionNotification
}

// SessionUpdate 接收 SDK 会话通知并交给测试。
func (c *acpSDKClient) SessionUpdate(ctx context.Context, update acp.SessionNotification) error {
	// 1. 将通知写入更新通道，context 取消时返回错误。
	select {
	case c.updates <- update:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
