package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// mcpclientBuildDirectory 是本包测试共用的编译目录，TestMain 创建，全部测试结束后删除。
var mcpclientBuildDirectory string

// TestMain 创建共享编译目录，测试结束后删除。
func TestMain(m *testing.M) {
	// 1. 创建并保存临时编译目录，运行测试后清理并退出。
	directory, err := os.MkdirTemp("", "openvaala-mcpclient-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mcpclientBuildDirectory = directory
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// buildFakeMCPServerOnce 在第一次用到时把 testdata/mcpserver 编译到 mcpclientBuildDirectory；
// 测试本身带 -race 时同样带 -race 编译。
var buildFakeMCPServerOnce = sync.OnceValues(func() (string, error) {
	binary := filepath.Join(mcpclientBuildDirectory, "mcpserver")
	arguments := []string{"build", "-o", binary}
	if isRaceEnabled() {
		arguments = append(arguments, "-race")
	}
	command := exec.Command("go", append(arguments, "./testdata/mcpserver")...)
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build fake MCP server: %w\n%s", err, output)
	}
	return binary, nil
})

// buildFakeMCPServer 返回共用的假 MCP server 二进制。
func buildFakeMCPServer(t *testing.T) string {
	// 1. 触发一次性编译，编译失败时结束测试。
	t.Helper()
	binary, err := buildFakeMCPServerOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// isRaceEnabled 测试二进制是否带 -race 编译？
func isRaceEnabled() bool {
	// 1. 读取构建信息并查找启用的 -race 设置。
	info, ok := debug.ReadBuildInfo()
	return ok && slices.ContainsFunc(info.Settings, func(setting debug.BuildSetting) bool {
		return setting.Key == "-race" && setting.Value == "true"
	})
}

// countingWorkspace 是本机工作区，记下 StartProcess 的次数，并可让第一次启动失败、第二次启动等待放行。
type countingWorkspace struct {
	workspace.Workspace
	startCount atomic.Int32
	// isFirstStartFailed 第一次 StartProcess 是否直接返回错误？
	isFirstStartFailed bool
	// isRestartHeld 第二次 StartProcess 是否等到 releaseRestart 或启动 ctx 结束才继续？
	isRestartHeld   bool
	restartReleased chan struct{}
	releaseRestart  func()
}

// newCountingWorkspace 创建记录启动次数的本机工作区。
func newCountingWorkspace(t *testing.T) *countingWorkspace {
	// 1. 创建重启放行通道和只关闭一次的放行函数。
	restartReleased := make(chan struct{})
	return &countingWorkspace{
		Workspace:       newLocalWorkspace(t),
		restartReleased: restartReleased,
		releaseRestart:  sync.OnceFunc(func() { close(restartReleased) }),
	}
}

// newLocalWorkspace 创建临时目录中的本机工作区。
func newLocalWorkspace(t *testing.T) workspace.Workspace {
	// 1. 构造工作区，失败时结束测试。
	t.Helper()
	localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return localWorkspace
}

// StartProcess 记录并控制测试进程的启动。
func (w *countingWorkspace) StartProcess(ctx context.Context, request *workspace.ProcessRequest) (workspace.Process, error) {
	// 1. 增加启动计数，按配置让首次失败或第二次等待，放行后启动真实进程。
	count := w.startCount.Add(1)
	if count == 1 && w.isFirstStartFailed {
		return nil, errors.New("first start fails")
	}
	if count == 2 && w.isRestartHeld {
		select {
		case <-w.restartReleased:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return w.Workspace.StartProcess(ctx, request)
}

// connectionTestFixture 是一个用例里正在运行的连接。
type connectionTestFixture struct {
	connection Connection
	workspace  *countingWorkspace
	// stopRun 取消 Run 并等它返回，可重复调用。
	stopRun func()
}

// newFakeServerDefinition 创建 stdio 假 server 定义。
func newFakeServerDefinition(binary string, arguments ...string) *ServerDefinition {
	// 1. 保存命令和参数，设置五秒启动超时和三十秒调用超时。
	return &ServerDefinition{
		ID:             "fake",
		Type:           ServerTransportStdio,
		Command:        binary,
		Args:           arguments,
		StartupTimeout: 5 * time.Second,
		ToolTimeout:    30 * time.Second,
	}
}

// startFakeHTTPServer 启动随机端口的 HTTP 或 SSE 测试 server。
func startFakeHTTPServer(t *testing.T, binary, flag string) string {
	// 1. 启动命令，登记测试后终止及等待，再从 stdout 读取监听地址。
	t.Helper()
	command := exec.Command(binary, flag, "127.0.0.1:0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read fake server address: %v", err)
	}
	return strings.TrimSpace(line)
}

// callTool 构造一次工具调用并在连接上执行。
func callTool(ctx context.Context, connection Connection, name, arguments string) *defs.ToolResult {
	// 1. 填入 call-1 和参数，调用 CallTool。
	return connection.CallTool(ctx, &defs.Item{
		Kind: defs.ItemToolCall,
		ToolCall: &defs.ToolCall{
			CallID:    "call-1",
			Name:      name,
			Arguments: arguments,
			Status:    defs.ToolCallReady,
		},
	})
}

// getTextContent 读取成功结果中的 JSON 字符串正文。
func getTextContent(t *testing.T, result *defs.ToolResult) string {
	// 1. 检查工具错误，再解码正文；失败时结束测试。
	t.Helper()
	if result.Error != nil {
		t.Fatalf("tool result error = %+v, want content", result.Error)
	}
	var text string
	if err := json.Unmarshal(result.Content, &text); err != nil {
		t.Fatalf("decode tool content %s: %v", result.Content, err)
	}
	return text
}

// getToolNames 按定义顺序提取工具名称。
func getToolNames(tools []*defs.ToolDefinition) []string {
	// 1. 逐项收集 Name。
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// getServerPID 调用 pid 工具读取 server 进程号。
func getServerPID(t *testing.T, connection Connection) int {
	// 1. 取得文本结果并解析整数。
	t.Helper()
	pid, err := strconv.Atoi(getTextContent(t, callTool(t.Context(), connection, "mcp__fake__pid", "{}")))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// isProcessAlive 指定 PID 的进程是否仍存在？
func isProcessAlive(pid int) bool {
	// 1. 使用信号 0 查询进程。
	return syscall.Kill(pid, 0) == nil
}

// waitForCondition 等待测试条件成立。
func waitForCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	// 1. 每十毫秒检查一次条件和截止时间，超时则结束测试。
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settleDuration 是启动次数达到预期后再多等的时间，这段时间里不应再有新的启动。
const settleDuration = 500 * time.Millisecond

// TestConnection 验证 MCP 连接、工具调用、重连和关闭。
func TestConnection(t *testing.T) {
	// 1. 编译测试 server，准备传输、错误和重连用例。
	binary := buildFakeMCPServer(t)
	tests := []struct {
		name string
		// newDefinition 返回用例的 server 定义；为 nil 时用不带参数的 stdio 假 server。
		newDefinition      func(t *testing.T) *ServerDefinition
		isFirstStartFailed bool
		isRestartHeld      bool
		run                func(t *testing.T, fixture *connectionTestFixture)
	}{
		{
			name: "stdio server provides renamed tools",
			run: func(t *testing.T, fixture *connectionTestFixture) {
				tools, err := fixture.connection.GetTools(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				index := slices.IndexFunc(tools, func(tool *defs.ToolDefinition) bool { return tool.Name == "mcp__fake__echo" })
				if index < 0 {
					t.Fatalf("tools = %v, want mcp__fake__echo", getToolNames(tools))
				}
				if tools[index].MCPServerID != "fake" || tools[index].Description != "fake echo" {
					t.Fatalf("echo tool = %+v, want MCPServerID fake and the server description", tools[index])
				}
				info := fixture.connection.GetInfo()
				if info.ServerID != "fake" || info.State != ConnectionStateConnected || info.Error != "" || len(info.Tools) != len(tools) {
					t.Fatalf("GetInfo() = %+v, want connected fake with %d tools", info, len(tools))
				}
			},
		},
		{
			name: "plugin server reports its plugin name",
			newDefinition: func(*testing.T) *ServerDefinition {
				definition := newFakeServerDefinition(binary)
				definition.Plugin = &defs.Plugin{Name: "demo"}
				return definition
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				if info := fixture.connection.GetInfo(); info.State != ConnectionStateConnected || info.PluginName != "demo" {
					t.Fatalf("GetInfo() = %+v, want connected with plugin demo", info)
				}
			},
		},
		{
			name: "tool names keep only listed tools",
			newDefinition: func(*testing.T) *ServerDefinition {
				definition := newFakeServerDefinition(binary)
				definition.ToolNames = []string{"echo"}
				return definition
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				tools, err := fixture.connection.GetTools(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if names := getToolNames(tools); !slices.Equal(names, []string{"mcp__fake__echo"}) {
					t.Fatalf("tools = %v, want only mcp__fake__echo", names)
				}
				if result := callTool(t.Context(), fixture.connection, "mcp__fake__pid", "{}"); result.Error == nil || result.Error.Code != "mcp_request_failed" {
					t.Fatalf("calling an unlisted tool = %+v, want mcp_request_failed", result)
				}
			},
		},
		{
			name:          "server without tools connects with none",
			newDefinition: func(*testing.T) *ServerDefinition { return newFakeServerDefinition(binary, "-empty") },
			run: func(t *testing.T, fixture *connectionTestFixture) {
				tools, err := fixture.connection.GetTools(t.Context())
				if err != nil || len(tools) != 0 {
					t.Fatalf("GetTools() = %v, %v, want no tools", getToolNames(tools), err)
				}
				if state := fixture.connection.GetInfo().State; state != ConnectionStateConnected {
					t.Fatalf("state = %q, want connected", state)
				}
			},
		},
		{
			name: "tool calls convert results and errors",
			newDefinition: func(*testing.T) *ServerDefinition {
				definition := newFakeServerDefinition(binary)
				definition.ToolTimeout = time.Second
				return definition
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				calls := []struct {
					name        string
					tool        string
					arguments   string
					wantText    string
					wantJSON    string
					wantCode    string
					wantMessage string
					wantLength  int
				}{
					{
						name:      "echo",
						tool:      "mcp__fake__echo",
						arguments: `{"text":"hello"}`,
						wantText:  "hello",
					},
					{
						name:        "isError",
						tool:        "mcp__fake__fail",
						arguments:   `{"message":"boom"}`,
						wantCode:    "mcp_tool_error",
						wantMessage: "boom",
					},
					{
						name:        "arguments rejected by the server",
						tool:        "mcp__fake__echo",
						arguments:   `{"text":1}`,
						wantCode:    "mcp_request_failed",
						wantMessage: "invalid echo arguments",
					},
					{
						name:        "unknown tool",
						tool:        "mcp__fake__nope",
						arguments:   `{}`,
						wantCode:    "mcp_request_failed",
						wantMessage: "nope",
					},
					{
						name:      "tool timeout",
						tool:      "mcp__fake__slow",
						arguments: `{"seconds":30}`,
						wantCode:  "timeout",
					},
					{
						name:      "image placeholder",
						tool:      "mcp__fake__image",
						arguments: `{}`,
						wantText:  "here\n[image omitted: image/png, 1 KB]",
					},
					{
						name:       "large result is truncated",
						tool:       "mcp__fake__big",
						arguments:  `{"bytes":2097152}`,
						wantLength: maxToolResultBytes,
					},
					{
						name:      "structured content",
						tool:      "mcp__fake__structured",
						arguments: `{}`,
						wantJSON:  `{"ok":true}`,
					},
					{
						name:       "empty arguments",
						tool:       "mcp__fake__pid",
						arguments:  ``,
						wantLength: -1,
					},
				}
				for _, call := range calls {
					t.Run(call.name, func(t *testing.T) {
						result := callTool(t.Context(), fixture.connection, call.tool, call.arguments)
						if result.CallID != "call-1" {
							t.Fatalf("CallID = %q, want call-1", result.CallID)
						}
						switch {
						case call.wantCode != "":
							if result.Error == nil || result.Error.Code != call.wantCode || !strings.Contains(result.Error.Message, call.wantMessage) {
								t.Fatalf("result = %+v, want error %s containing %q", result, call.wantCode, call.wantMessage)
							}
						case call.wantJSON != "":
							if result.Error != nil || string(result.Content) != call.wantJSON {
								t.Fatalf("result = %+v, want content %s", result, call.wantJSON)
							}
						case call.wantLength > 0:
							if text := getTextContent(t, result); len(text) != call.wantLength || !result.Truncated {
								t.Fatalf("content length = %d, truncated = %v, want %d and truncated", len(text), result.Truncated, call.wantLength)
							}
						case call.wantLength < 0:
							getTextContent(t, result)
						default:
							if text := getTextContent(t, result); text != call.wantText {
								t.Fatalf("content = %q, want %q", text, call.wantText)
							}
						}
					})
				}
			},
		},
		{
			name: "canceled call",
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				time.AfterFunc(300*time.Millisecond, cancel)
				result := callTool(ctx, fixture.connection, "mcp__fake__slow", `{"seconds":30}`)
				if result.Error == nil || result.Error.Code != "canceled" {
					t.Fatalf("result = %+v, want canceled", result)
				}
				// 取消只结束这一次调用，连接照常可用。
				if text := getTextContent(t, callTool(t.Context(), fixture.connection, "mcp__fake__echo", `{"text":"after"}`)); text != "after" {
					t.Fatalf("echo after cancel = %q", text)
				}
			},
		},
		{
			name: "arguments and env reach the process verbatim",
			newDefinition: func(*testing.T) *ServerDefinition {
				definition := newFakeServerDefinition(binary, "--name", "a b", "it's", "$HOME")
				definition.Env = map[string]string{"MCP_TEST_ENV": "x"}
				return definition
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				var argv struct {
					Args []string `json:"args"`
					Env  string   `json:"env"`
				}
				text := getTextContent(t, callTool(t.Context(), fixture.connection, "mcp__fake__argv", "{}"))
				if err := json.Unmarshal([]byte(text), &argv); err != nil {
					t.Fatal(err)
				}
				if want := []string{"--name", "a b", "it's", "$HOME"}; !slices.Equal(argv.Args, want) || argv.Env != "x" {
					t.Fatalf("argv = %+v, want args %q and env x", argv, want)
				}
			},
		},
		{
			name: "stderr explains a failed start",
			newDefinition: func(*testing.T) *ServerDefinition {
				return newFakeServerDefinition(binary, "-stderr", "missing GITHUB_TOKEN")
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				const wantReason = "MCP server process exited with code 1; stderr: missing GITHUB_TOKEN"
				started := time.Now()
				_, err := fixture.connection.GetTools(t.Context())
				if err == nil || err.Error() != wantReason {
					t.Fatalf("GetTools() error = %v, want %q", err, wantReason)
				}
				if elapsed := time.Since(started); elapsed >= 5*time.Second {
					t.Fatalf("GetTools() took %s, want less than the startup timeout", elapsed)
				}
				if info := fixture.connection.GetInfo(); info.State != ConnectionStateFailed || info.Error != wantReason {
					t.Fatalf("GetInfo() = %+v, want failed with %q", info, wantReason)
				}
			},
		},
		{
			name: "missing command fails",
			newDefinition: func(*testing.T) *ServerDefinition {
				definition := newFakeServerDefinition(binary)
				definition.Command = "/nonexistent"
				return definition
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err == nil {
					t.Fatal("GetTools() succeeded, want an error")
				}
				info := fixture.connection.GetInfo()
				if info.State != ConnectionStateFailed || !strings.HasPrefix(info.Error, "MCP server process exited with code 127; stderr: ") ||
					!strings.Contains(info.Error, "/nonexistent") || strings.Contains(info.Error, "client is closing") {
					t.Fatalf("GetInfo() = %+v, want failed with the exit code and a stderr line naming the command", info)
				}
			},
		},
		{
			name:          "junk output does not hang",
			newDefinition: func(*testing.T) *ServerDefinition { return newFakeServerDefinition(binary, "-junk") },
			run: func(t *testing.T, fixture *connectionTestFixture) {
				started := time.Now()
				_, err := fixture.connection.GetTools(t.Context())
				if elapsed := time.Since(started); elapsed > 6*time.Second {
					t.Fatalf("GetTools() took %s (error %v), want it within the startup timeout", elapsed, err)
				}
			},
		},
		{
			name: "hanging server times out while connecting",
			newDefinition: func(*testing.T) *ServerDefinition {
				definition := newFakeServerDefinition(binary, "-hang")
				definition.StartupTimeout = time.Second
				return definition
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				time.Sleep(200 * time.Millisecond)
				if state := fixture.connection.GetInfo().State; state != ConnectionStateConnecting {
					t.Fatalf("state while starting = %q, want connecting", state)
				}
				started := time.Now()
				_, err := fixture.connection.GetTools(t.Context())
				if err == nil || err.Error() != "MCP server did not start within 1s" {
					t.Fatalf("GetTools() error = %v, want the startup timeout reason unchanged", err)
				}
				if elapsed := time.Since(started); elapsed > 2*time.Second {
					t.Fatalf("GetTools() took %s, want about the startup timeout", elapsed)
				}
				if state := fixture.connection.GetInfo().State; state != ConnectionStateFailed {
					t.Fatalf("state after timeout = %q, want failed", state)
				}
			},
		},
		{
			name: "streamable HTTP server",
			newDefinition: func(t *testing.T) *ServerDefinition {
				return &ServerDefinition{
					ID:             "fake",
					Type:           ServerTransportHTTP,
					URL:            startFakeHTTPServer(t, binary, "-http"),
					StartupTimeout: 5 * time.Second,
					ToolTimeout:    30 * time.Second,
				}
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				tools, err := fixture.connection.GetTools(t.Context())
				if err != nil || !slices.Contains(getToolNames(tools), "mcp__fake__echo") {
					t.Fatalf("GetTools() = %v, %v, want mcp__fake__echo", getToolNames(tools), err)
				}
				if text := getTextContent(t, callTool(t.Context(), fixture.connection, "mcp__fake__echo", `{"text":"over http"}`)); text != "over http" {
					t.Fatalf("echo = %q", text)
				}
			},
		},
		{
			name: "SSE server",
			newDefinition: func(t *testing.T) *ServerDefinition {
				return &ServerDefinition{
					ID:             "fake",
					Type:           ServerTransportSSE,
					URL:            startFakeHTTPServer(t, binary, "-sse"),
					StartupTimeout: 5 * time.Second,
					ToolTimeout:    30 * time.Second,
				}
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				if text := getTextContent(t, callTool(t.Context(), fixture.connection, "mcp__fake__echo", `{"text":"over sse"}`)); text != "over sse" {
					t.Fatalf("echo = %q", text)
				}
			},
		},
		{
			name: "HTTP authentication failure names the status",
			newDefinition: func(t *testing.T) *ServerDefinition {
				var isHeaderReceived atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Header.Get("X-Test") == "v" {
						isHeaderReceived.Store(true)
					}
					http.Error(writer, "token required", http.StatusUnauthorized)
				}))
				t.Cleanup(server.Close)
				t.Cleanup(func() {
					if !isHeaderReceived.Load() {
						t.Error("configured header X-Test: v was not sent")
					}
				})
				return &ServerDefinition{
					ID:             "fake",
					Type:           ServerTransportHTTP,
					URL:            server.URL,
					Headers:        map[string]string{"X-Test": "v"},
					StartupTimeout: 5 * time.Second,
					ToolTimeout:    30 * time.Second,
				}
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				started := time.Now()
				if _, err := fixture.connection.GetTools(t.Context()); err == nil {
					t.Fatal("GetTools() succeeded, want an error")
				}
				if elapsed := time.Since(started); elapsed >= 5*time.Second {
					t.Fatalf("GetTools() took %s, want less than the startup timeout", elapsed)
				}
				if info := fixture.connection.GetInfo(); !strings.Contains(info.Error, "401") {
					t.Fatalf("GetInfo().Error = %q, want the 401 status", info.Error)
				}
			},
		},
		{
			name: "killed server fails the running call and reconnects once",
			// 第二次启动等用例看到 failed 之后再继续，让断开后的状态可以被观察到。
			isRestartHeld: true,
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				pid := getServerPID(t, fixture.connection)
				slowResult := make(chan *defs.ToolResult, 1)
				go func() { slowResult <- callTool(t.Context(), fixture.connection, "mcp__fake__slow", `{"seconds":30}`) }()
				time.Sleep(500 * time.Millisecond)
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				select {
				case result := <-slowResult:
					if result.Error == nil || result.Error.Code != "mcp_server_unavailable" {
						t.Fatalf("running call = %+v, want mcp_server_unavailable", result)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("running call did not end after the server was killed")
				}
				waitForCondition(t, 5*time.Second, "failed state", func() bool {
					return fixture.connection.GetInfo().State == ConnectionStateFailed
				})
				if info := fixture.connection.GetInfo(); !strings.HasPrefix(info.Error, "MCP server process exited with code ") ||
					strings.Contains(info.Error, "client is closing") || len(info.Tools) != 0 {
					t.Fatalf("GetInfo() after the drop = %+v, want the process exit reason and no tools", info)
				}
				if result := callTool(t.Context(), fixture.connection, "mcp__fake__echo", `{"text":"x"}`); result.Error == nil || result.Error.Code != "mcp_server_unavailable" {
					t.Fatalf("call while disconnected = %+v, want mcp_server_unavailable", result)
				}
				fixture.workspace.releaseRestart()
				waitForCondition(t, 5*time.Second, "reconnected state", func() bool {
					return fixture.connection.GetInfo().State == ConnectionStateConnected
				})
				if newPID := getServerPID(t, fixture.connection); newPID == pid {
					t.Fatalf("pid after reconnect = %d, want a new process", newPID)
				}
				if count := fixture.workspace.startCount.Load(); count != 2 {
					t.Fatalf("process starts = %d, want 2", count)
				}
			},
		},
		{
			name:          "server that drops after every handshake reconnects once per GetTools",
			newDefinition: func(*testing.T) *ServerDefinition { return newFakeServerDefinition(binary, "-exit-after-list") },
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				// 等首次后台重连结束，再核对没有第三次启动。
				waitForCondition(t, 5*time.Second, "the background reconnect", func() bool {
					return fixture.workspace.startCount.Load() >= 2 && fixture.connection.GetInfo().State == ConnectionStateFailed
				})
				time.Sleep(settleDuration)
				if count := fixture.workspace.startCount.Load(); count != 2 {
					t.Fatalf("process starts while the server keeps dropping = %d, want 2: the first start and one background reconnect", count)
				}
				if state := fixture.connection.GetInfo().State; state != ConnectionStateFailed {
					t.Fatalf("state after the reconnect dropped = %q, want failed", state)
				}
				// 下一次 GetTools 发起连接，并重新提供一次断开后立即重连的机会。
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				waitForCondition(t, 5*time.Second, "the background reconnect after GetTools", func() bool {
					return fixture.workspace.startCount.Load() >= 4 && fixture.connection.GetInfo().State == ConnectionStateFailed
				})
				time.Sleep(settleDuration)
				if count := fixture.workspace.startCount.Load(); count != 4 {
					t.Fatalf("process starts after the next GetTools = %d, want 4: one for GetTools and one background reconnect", count)
				}
			},
		},
		{
			name: "tool list change is picked up",
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				if text := getTextContent(t, callTool(t.Context(), fixture.connection, "mcp__fake__change", "{}")); text != "changed" {
					t.Fatalf("change = %q", text)
				}
				waitForCondition(t, 5*time.Second, "mcp__fake__added", func() bool {
					tools, err := fixture.connection.GetTools(t.Context())
					return err == nil && slices.Contains(getToolNames(tools), "mcp__fake__added")
				})
				if text := getTextContent(t, callTool(t.Context(), fixture.connection, "mcp__fake__added", "{}")); text != "added" {
					t.Fatalf("added = %q", text)
				}
			},
		},
		{
			name: "concurrent GetTools share one process",
			run: func(t *testing.T, fixture *connectionTestFixture) {
				assertConcurrentGetTools(t, fixture.connection)
				if count := fixture.workspace.startCount.Load(); count != 1 {
					t.Fatalf("process starts = %d, want 1", count)
				}
				if first, second := getServerPID(t, fixture.connection), getServerPID(t, fixture.connection); first != second {
					t.Fatalf("pids = %d and %d, want the same process", first, second)
				}
			},
		},
		{
			name:               "GetTools after a failure connects once more",
			isFirstStartFailed: true,
			run: func(t *testing.T, fixture *connectionTestFixture) {
				waitForCondition(t, 5*time.Second, "failed state", func() bool {
					return fixture.connection.GetInfo().State == ConnectionStateFailed
				})
				if info := fixture.connection.GetInfo(); !strings.Contains(info.Error, "first start fails") {
					t.Fatalf("GetInfo().Error = %q, want the start error", info.Error)
				}
				// 失败后不在后台自动重试，等 GetTools 触发。
				time.Sleep(300 * time.Millisecond)
				if count := fixture.workspace.startCount.Load(); count != 1 {
					t.Fatalf("process starts before GetTools = %d, want 1", count)
				}
				assertConcurrentGetTools(t, fixture.connection)
				if count := fixture.workspace.startCount.Load(); count != 2 {
					t.Fatalf("process starts = %d, want 2", count)
				}
				if info := fixture.connection.GetInfo(); info.State != ConnectionStateConnected || info.Error != "" {
					t.Fatalf("GetInfo() = %+v, want connected without error", info)
				}
			},
		},
		{
			name: "required follows the definition",
			newDefinition: func(*testing.T) *ServerDefinition {
				definition := newFakeServerDefinition(binary)
				definition.Required = true
				return definition
			},
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if !fixture.connection.IsRequired() || !fixture.connection.GetInfo().Required {
					t.Fatal("IsRequired() = false, want true")
				}
			},
		},
		{
			name: "stopping Run ends the process and later requests fail",
			run: func(t *testing.T, fixture *connectionTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				pid := getServerPID(t, fixture.connection)
				fixture.stopRun()
				if isProcessAlive(pid) {
					t.Fatalf("server process %d is still alive after Run returned", pid)
				}
				if _, err := fixture.connection.GetTools(t.Context()); err == nil {
					t.Fatal("GetTools() after Run returned succeeded, want an error")
				}
				if result := callTool(t.Context(), fixture.connection, "mcp__fake__echo", `{"text":"x"}`); result.Error == nil || result.Error.Code != "mcp_server_unavailable" {
					t.Fatalf("call after Run returned = %+v, want mcp_server_unavailable", result)
				}
				if info := fixture.connection.GetInfo(); info.State != ConnectionStateFailed {
					t.Fatalf("GetInfo() after Run returned = %+v, want failed", info)
				}
			},
		},
	}
	// 2. 逐项创建连接并运行，核对工具、错误、状态及资源收尾。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition := newFakeServerDefinition(binary)
			if tt.newDefinition != nil {
				definition = tt.newDefinition(t)
			}
			executorWorkspace := newCountingWorkspace(t)
			executorWorkspace.isFirstStartFailed = tt.isFirstStartFailed
			executorWorkspace.isRestartHeld = tt.isRestartHeld
			connection, err := NewConnection(NewConnectionOpt(definition, executorWorkspace))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			runDone := make(chan struct{})
			go func() {
				defer close(runDone)
				_ = connection.Run(ctx)
			}()
			stopRun := func() {
				cancel()
				select {
				case <-runDone:
				case <-time.After(15 * time.Second):
					t.Fatal("Run did not return after its context ended")
				}
			}
			t.Cleanup(stopRun)
			tt.run(t, &connectionTestFixture{
				connection: connection,
				workspace:  executorWorkspace,
				stopRun:    stopRun,
			})
		})
	}
}

// assertConcurrentGetTools 并发调用两次 GetTools，确认都成功且都包含 mcp__fake__echo。
func assertConcurrentGetTools(t *testing.T, connection Connection) {
	// 1. 启动两个读取任务，核对两次结果都包含 echo，等待结束并合并检查错误。
	t.Helper()
	var tasks sync.WaitGroup
	results := make([]error, 2)
	for index := range results {
		tasks.Go(func() {
			tools, err := connection.GetTools(t.Context())
			if err == nil && !slices.Contains(getToolNames(tools), "mcp__fake__echo") {
				err = fmt.Errorf("tools = %v, want mcp__fake__echo", getToolNames(tools))
			}
			results[index] = err
		})
	}
	tasks.Wait()
	if err := errors.Join(results...); err != nil {
		t.Fatal(err)
	}
}

// proxyWorkspace 是本机工作区，GetHTTPProxy 固定返回 proxyURL。
type proxyWorkspace struct {
	workspace.Workspace
	proxyURL *url.URL
}

// GetHTTPProxy 返回测试指定的代理地址。
func (w *proxyWorkspace) GetHTTPProxy(*http.Request) (*url.URL, error) {
	// 1. 读取保存的代理 URL。
	return w.proxyURL, nil
}

// startForwardingProxy 启动转发代理并记录经过代理的目标主机。
func startForwardingProxy(t *testing.T) (*url.URL, *syncmap.SyncMap[string, struct{}]) {
	// 1. 创建反向代理和主机记录，启动 HTTP 服务并返回地址。
	t.Helper()
	var proxiedHosts syncmap.SyncMap[string, struct{}]
	forwarder := &httputil.ReverseProxy{
		Director:      func(*http.Request) {},
		FlushInterval: -1,
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.IsAbs() {
			proxiedHosts.Store(request.URL.Host, struct{}{})
		}
		forwarder.ServeHTTP(writer, request)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	return proxyURL, &proxiedHosts
}

// TestConnectionHTTPProxy 验证 HTTP 和 SSE 连接使用工作区代理。
func TestConnectionHTTPProxy(t *testing.T) {
	// 1. 准备两类网络传输。
	binary := buildFakeMCPServer(t)
	tests := []struct {
		name           string
		transport      ServerTransportType
		fakeServerFlag string
	}{
		{
			name:           "streamable HTTP goes through the workspace proxy",
			transport:      ServerTransportHTTP,
			fakeServerFlag: "-http",
		},
		{
			name:           "SSE goes through the workspace proxy",
			transport:      ServerTransportSSE,
			fakeServerFlag: "-sse",
		},
	}
	// 2. 逐项连接 HTTP 和 SSE server，核对工具读取、连接状态及代理目标主机。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverURL := startFakeHTTPServer(t, binary, tt.fakeServerFlag)
			parsedServerURL, err := url.Parse(serverURL)
			if err != nil {
				t.Fatal(err)
			}
			proxyURL, proxiedHosts := startForwardingProxy(t)
			executorWorkspace := &proxyWorkspace{
				Workspace: newLocalWorkspace(t),
				proxyURL:  proxyURL,
			}
			definition := &ServerDefinition{
				ID:             "fake",
				Type:           tt.transport,
				URL:            serverURL,
				StartupTimeout: 5 * time.Second,
				ToolTimeout:    30 * time.Second,
			}
			connection, err := NewConnection(NewConnectionOpt(definition, executorWorkspace))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			runDone := make(chan struct{})
			go func() {
				defer close(runDone)
				_ = connection.Run(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				<-runDone
			})

			if _, err := connection.GetTools(ctx); err != nil {
				t.Fatalf("GetTools() error = %v, want the server reached through the proxy", err)
			}
			if info := connection.GetInfo(); info.State != ConnectionStateConnected {
				t.Fatalf("GetInfo() = %+v, want connected", info)
			}
			if _, proxied := proxiedHosts.Load(parsedServerURL.Host); !proxied {
				t.Fatalf("proxied hosts = %v, want MCP requests addressed to %s", proxiedHosts.Keys(), parsedServerURL.Host)
			}
		})
	}
}

// TestConnectionWithoutRun 验证构造检查及 Run 开始前的查询表现。
func TestConnectionWithoutRun(t *testing.T) {
	// 1. 准备缺失定义、缺失运行位置和未启动连接用例。
	tests := []struct {
		name string
		opt  *ConnectionOpt
		// wantNewError 是 NewConnection 应返回的错误片段；为空时构造成功，并检查 Run 之前的请求。
		wantNewError string
	}{
		{
			name: "requests before Run wait for it",
			opt: NewConnectionOpt(&ServerDefinition{
				ID:      "fake",
				Type:    ServerTransportStdio,
				Command: "true",
				Plugin:  &defs.Plugin{Name: "demo"},
			},
				newLocalWorkspace(t)),
		},
		{
			name:         "definition is required",
			opt:          NewConnectionOpt(nil, nil),
			wantNewError: "server definition",
		},
		{
			name:         "workspace or transport is required",
			opt:          NewConnectionOpt(&ServerDefinition{ID: "fake"}, nil),
			wantNewError: "exactly one",
		},
	}
	// 2. 逐项核对构造错误，以及未运行时请求超时、状态查询立即返回。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connection, err := NewConnection(tt.opt)
			if tt.wantNewError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantNewError) {
					t.Fatalf("NewConnection() error = %v, want %q", err, tt.wantNewError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// 请求等 Run 开始，最多等到调用方的 ctx 结束；GetInfo 不等。
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if _, err := connection.GetTools(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("GetTools() before Run error = %v, want the caller's deadline", err)
			}
			if result := callTool(ctx, connection, "mcp__fake__echo", "{}"); result.Error == nil || result.Error.Code != "canceled" {
				t.Fatalf("CallTool() before Run = %+v, want canceled by the caller's deadline", result)
			}
			if info := connection.GetInfo(); info.ServerID != "fake" || info.State != ConnectionStateConnecting || info.PluginName != "demo" {
				t.Fatalf("GetInfo() before Run = %+v, want connecting with plugin demo", info)
			}
		})
	}
}
