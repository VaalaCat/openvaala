package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// executorBuildDirectory 是本包测试共用的编译目录，TestMain 创建，全部测试结束后删除。
var executorBuildDirectory string

// TestMain 创建共享编译目录，运行测试后删除目录。
func TestMain(m *testing.M) {
	// 1. 创建编译目录并保存路径。
	directory, err := os.MkdirTemp("", "openvaala-executor-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	executorBuildDirectory = directory
	// 2. 运行测试，删除编译目录并返回退出码。
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

// buildFakeMCPServerOnce 在第一次用到时把 pkg/mcpclient 的假 MCP server 编译到 executorBuildDirectory；
// 测试本身带 -race 时同样带 -race 编译。
var buildFakeMCPServerOnce = sync.OnceValues(func() (string, error) {
	binary := filepath.Join(executorBuildDirectory, "mcpserver")
	arguments := []string{"build", "-o", binary}
	if isRaceEnabled() {
		arguments = append(arguments, "-race")
	}
	command := exec.Command("go", append(arguments, "../mcpclient/testdata/mcpserver")...)
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build fake MCP server: %w\n%s", err, output)
	}
	return binary, nil
})

// buildFakeMCPServer 取得共用的假 MCP server 二进制。
func buildFakeMCPServer(t *testing.T) string {
	// 1. 调用一次性编译函数，编译失败时结束测试。
	t.Helper()
	binary, err := buildFakeMCPServerOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// isRaceEnabled 测试二进制是否带 -race 编译？
func isRaceEnabled() bool {
	// 1. 读取编译设置并查找启用的 -race。
	info, ok := debug.ReadBuildInfo()
	return ok && slices.ContainsFunc(info.Settings, func(setting debug.BuildSetting) bool {
		return setting.Key == "-race" && setting.Value == "true"
	})
}

// newLocalWorkspace 创建临时目录中的本机工作区。
func newLocalWorkspace(t *testing.T) workspace.Workspace {
	// 1. 用测试临时目录构造工作区。
	t.Helper()
	localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return localWorkspace
}

// newFakeServerDefinition 创建分配给指定环境的 stdio 测试 server 定义。
func newFakeServerDefinition(id, binary string, executorIDs []string, arguments ...string) *mcpclient.ServerDefinition {
	// 1. 保存命令、参数和环境列表，设置启动超时 5 秒、工具超时 30 秒。
	return &mcpclient.ServerDefinition{
		ID:             id,
		Type:           mcpclient.ServerTransportStdio,
		Command:        binary,
		Args:           arguments,
		ExecutorIDs:    executorIDs,
		StartupTimeout: 5 * time.Second,
		ToolTimeout:    30 * time.Second,
	}
}

// isProcessAlive 指定 PID 的进程是否仍在运行？
func isProcessAlive(pid int) bool {
	// 1. 使用信号 0 查询进程。
	return syscall.Kill(pid, 0) == nil
}

// singleStartWorkspace 是只允许启动一次进程的本机工作区，断开后的重连会失败。
type singleStartWorkspace struct {
	workspace.Workspace
	startCount atomic.Int32
}

// StartProcess 限制测试工作区的进程启动次数。
func (w *singleStartWorkspace) StartProcess(ctx context.Context, request *workspace.ProcessRequest) (workspace.Process, error) {
	// 1. 增加启动次数，首次交给本机工作区启动，之后返回错误。
	if w.startCount.Add(1) > 1 {
		return nil, errors.New("only one process may be started")
	}
	return w.Workspace.StartProcess(ctx, request)
}

// getServerPID 通过工具执行器读取 MCP server 的进程号。
func getServerPID(t *testing.T, toolExecutor executor.ToolExecutor, serverID string) int {
	// 1. 调用指定 server 的 pid 工具并解码结果。
	t.Helper()
	callID := "pid"
	results, _ := runToolCalls(t, toolExecutor, []*defs.ToolCall{{
		CallID:    callID,
		Name:      "mcp__" + serverID + "__pid",
		Arguments: `{}`,
	}})
	var text string
	if err := json.Unmarshal(results[callID].Content, &text); err != nil || results[callID].Error != nil {
		t.Fatalf("pid result = %+v, want the server process ID", results[callID])
	}
	pid, err := strconv.Atoi(text)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// getServerIDs 按状态列表顺序提取 server ID。
func getServerIDs(infos []mcpclient.ConnectionInfo) []string {
	// 1. 逐项收集 server ID。
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.ServerID)
	}
	return ids
}

// getToolServerIDs 返回工具所属 server 的去重排序列表。
func getToolServerIDs(tools []*defs.ToolDefinition) []string {
	// 1. 收集不重复的 MCPServerID 并排序。
	var ids []string
	for _, tool := range tools {
		if !slices.Contains(ids, tool.MCPServerID) {
			ids = append(ids, tool.MCPServerID)
		}
	}
	slices.Sort(ids)
	return ids
}

// getToolNames 按定义顺序提取工具名称。
func getToolNames(tools []*defs.ToolDefinition) []string {
	// 1. 逐项收集工具名称。
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

// getToolDefinitions 按工具顺序读取定义。
func getToolDefinitions(tools []executor.Tool) []*defs.ToolDefinition {
	// 1. 逐个调用工具的 GetDefinition。
	definitions := make([]*defs.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, tool.GetDefinition())
	}
	return definitions
}

// environmentFixture 是一个用例里正在运行的执行环境。
type environmentFixture struct {
	environment executor.ExecutionEnvironment
	// stopRun 取消 Run 并返回它的结果，可重复调用。
	stopRun func() error
	// runDone 在 Run 返回后关闭；关闭后才能读取 runErr。
	runDone <-chan struct{}
	runErr  *error
}

// newEnvironmentFixture 创建并运行带回显工具和 MCP 连接的测试环境。
func newEnvironmentFixture(t *testing.T, id string, environmentWorkspace workspace.Workspace, servers []*mcpclient.ServerDefinition, toolNames ...string) *environmentFixture {
	// 1. 创建各个回显工具和执行环境。
	t.Helper()
	var tools []executor.Tool
	for _, name := range toolNames {
		tools = append(tools, &echoTool{
			executorID: id,
			name:       name,
			schema:     echoSchema,
		})
	}
	environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(id, id+" 环境", environmentWorkspace, tools...).WithMCPServers(servers))
	if err != nil {
		t.Fatal(err)
	}
	// 2. 后台运行环境，登记取消和等待函数，测试结束时停止环境。
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = environment.Run(ctx)
		close(runDone)
	}()
	stopRun := func() error {
		cancel()
		<-runDone
		return runErr
	}
	t.Cleanup(func() { _ = stopRun() })
	return &environmentFixture{
		environment: environment,
		stopRun:     stopRun,
		runDone:     runDone,
		runErr:      &runErr,
	}
}

// newToolExecutor 创建只使用本环境和选中 server 的工具执行器。
func (f *environmentFixture) newToolExecutor(t *testing.T, serverIDs defs.OptionalStringList) executor.ToolExecutor {
	// 1. 设置环境和 server 筛选条件，构造执行器。
	t.Helper()
	opt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{f.environment})
	opt.MCPServerIDs = serverIDs
	toolExecutor, err := executor.NewToolExecutor(opt)
	if err != nil {
		t.Fatal(err)
	}
	return toolExecutor
}

// TestExecutionEnvironmentMCPTools 验证环境的 MCP 筛选、依赖失败和连接停止。
func TestExecutionEnvironmentMCPTools(t *testing.T) {
	// 1. 准备测试 server 与工作区配置。
	binary := buildFakeMCPServer(t)
	localOnly := []string{"local"}
	defaultServers := func() []*mcpclient.ServerDefinition {
		weak := newFakeServerDefinition("weak", "/nonexistent/openvaala-mcp-weak", localOnly)
		strong := newFakeServerDefinition("strong", "/nonexistent/openvaala-mcp-strong", localOnly)
		strong.Required = true
		return []*mcpclient.ServerDefinition{
			newFakeServerDefinition("ok", binary, localOnly),
			newFakeServerDefinition("ok2", binary, localOnly),
			weak,
			strong,
			newFakeServerDefinition("elsewhere", binary, []string{"other"}),
		}
	}
	// hangingServers 在默认定义之外加一个启动超时 20 秒、握手永远不完成的 server：碰了它的调用会等满 20 秒。
	hangingServers := func() []*mcpclient.ServerDefinition {
		hang := newFakeServerDefinition("hang", binary, localOnly, "-hang")
		hang.StartupTimeout = 20 * time.Second
		return append(defaultServers(), hang)
	}
	const promptly = 10 * time.Second

	tests := []struct {
		name string
		// newServers 返回用例的 MCP 定义；为 nil 时用 defaultServers。
		newServers func() []*mcpclient.ServerDefinition
		// newWorkspace 返回执行环境的工作区；为 nil 时用本机工作区。
		newWorkspace func(t *testing.T) workspace.Workspace
		run          func(t *testing.T, fixture *environmentFixture)
	}{
		{
			name: "GetMCPServers lists only servers assigned to this executor",
			run: func(t *testing.T, fixture *environmentFixture) {
				got := getServerIDs(fixture.environment.GetMCPServers())
				if want := []string{"ok", "ok2", "strong", "weak"}; !slices.Equal(got, want) {
					t.Fatalf("GetMCPServers() server IDs = %v, want %v", got, want)
				}
			},
		},
		{
			name: "all servers selected fails on the required server",
			run: func(t *testing.T, fixture *environmentFixture) {
				_, err := fixture.environment.GetMCPTools(t.Context(), nil)
				if err == nil || !strings.Contains(err.Error(), `mcp server "strong" is unavailable`) {
					t.Fatalf("GetMCPTools(nil) error = %v, want it to contain %q", err, `mcp server "strong" is unavailable`)
				}
			},
		},
		{
			name: "weak server failure is skipped with a warning",
			run: func(t *testing.T, fixture *environmentFixture) {
				pipeline := telemetrytest.NewPipeline(t)
				ctx := telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry)
				tools, err := fixture.environment.GetMCPTools(ctx, defs.OptionalStringList{"ok", "weak"})
				if err != nil {
					t.Fatal(err)
				}
				definitions := getToolDefinitions(tools)
				if got, want := getToolServerIDs(definitions), []string{"ok"}; !slices.Equal(got, want) {
					t.Fatalf("GetMCPTools() tool server IDs = %v, want %v", got, want)
				}
				if !slices.Contains(getToolNames(definitions), "mcp__ok__echo") {
					t.Fatalf("GetMCPTools() tools = %v, want mcp__ok__echo", getToolNames(definitions))
				}
				records := pipeline.GetLogRecords(t, "mcp server skipped")
				if len(records) != 1 {
					t.Fatalf("mcp server skipped records = %v, want exactly one", records)
				}
				record := records[0]
				if record["level"] != "WARN" || record["openvaala.executor.id"] != "local" || record["mcp_server_id"] != "weak" || record["error"] == nil || record["error"] == "" {
					t.Fatalf("mcp server skipped record = %v, want WARN with executor local, server weak and an error", record)
				}
			},
		},
		{
			name:       "empty selection returns nothing without touching connections",
			newServers: hangingServers,
			run: func(t *testing.T, fixture *environmentFixture) {
				start := time.Now()
				tools, err := fixture.environment.GetMCPTools(t.Context(), defs.OptionalStringList{})
				if err != nil || len(tools) != 0 {
					t.Fatalf("GetMCPTools({}) = %v, %v, want no tools and no error", getToolNames(getToolDefinitions(tools)), err)
				}
				if elapsed := time.Since(start); elapsed > promptly {
					t.Fatalf("GetMCPTools({}) took %v, want it to return without waiting for connections", elapsed)
				}
			},
		},
		{
			name:       "selection does not wait for unselected servers",
			newServers: hangingServers,
			run: func(t *testing.T, fixture *environmentFixture) {
				start := time.Now()
				tools, err := fixture.environment.GetMCPTools(t.Context(), defs.OptionalStringList{"ok"})
				if err != nil {
					t.Fatal(err)
				}
				if got, want := getToolServerIDs(getToolDefinitions(tools)), []string{"ok"}; !slices.Equal(got, want) {
					t.Fatalf("GetMCPTools({ok}) tool server IDs = %v, want %v", got, want)
				}
				if elapsed := time.Since(start); elapsed > promptly {
					t.Fatalf("GetMCPTools({ok}) took %v, want it to ignore the hanging server", elapsed)
				}
			},
		},
		{
			name:       "built-in tools never wait for MCP connections",
			newServers: hangingServers,
			run: func(t *testing.T, fixture *environmentFixture) {
				start := time.Now()
				tools := fixture.environment.GetTools()
				if got, want := getToolNames(getToolDefinitions(tools)), []string{"bash"}; !slices.Equal(got, want) {
					t.Fatalf("GetTools() = %v, want only built-in tools %v", got, want)
				}
				if elapsed := time.Since(start); elapsed > promptly {
					t.Fatalf("GetTools() took %v, want it to return without waiting for connections", elapsed)
				}
			},
		},
		{
			name: "Run without assigned servers returns immediately",
			newServers: func() []*mcpclient.ServerDefinition {
				return []*mcpclient.ServerDefinition{newFakeServerDefinition("elsewhere", binary, []string{"other"})}
			},
			run: func(t *testing.T, fixture *environmentFixture) {
				select {
				case <-fixture.runDone:
					if err := *fixture.runErr; err != nil {
						t.Fatalf("Run() = %v, want nil", err)
					}
				case <-time.After(promptly):
					t.Fatal("Run() did not return although no MCP server is assigned")
				}
			},
		},
		{
			name: "canceling Run stops the server processes",
			run: func(t *testing.T, fixture *environmentFixture) {
				if _, err := fixture.environment.GetMCPTools(t.Context(), defs.OptionalStringList{"ok"}); err != nil {
					t.Fatal(err)
				}
				pid := getServerPID(t, fixture.newToolExecutor(t, defs.OptionalStringList{"ok"}), "ok")
				if !isProcessAlive(pid) {
					t.Fatalf("server process %d is not running before Run is canceled", pid)
				}
				if err := fixture.stopRun(); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() after cancel = %v, want context.Canceled", err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for isProcessAlive(pid) {
					if time.Now().After(deadline) {
						t.Fatalf("server process %d still running after Run returned", pid)
					}
					time.Sleep(10 * time.Millisecond)
				}
			},
		},
		{
			name: "calls after a disconnect return mcp_server_unavailable",
			newServers: func() []*mcpclient.ServerDefinition {
				return []*mcpclient.ServerDefinition{newFakeServerDefinition("ok", binary, localOnly)}
			},
			newWorkspace: func(t *testing.T) workspace.Workspace {
				return &singleStartWorkspace{Workspace: newLocalWorkspace(t)}
			},
			run: func(t *testing.T, fixture *environmentFixture) {
				toolExecutor := fixture.newToolExecutor(t, nil)
				if _, err := toolExecutor.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				pid := getServerPID(t, toolExecutor, "ok")
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for fixture.environment.GetMCPServers()[0].State != mcpclient.ConnectionStateFailed {
					if time.Now().After(deadline) {
						t.Fatalf("GetMCPServers() = %+v, want the killed server to be reported as failed", fixture.environment.GetMCPServers())
					}
					time.Sleep(10 * time.Millisecond)
				}
				results, _ := runToolCalls(t, toolExecutor, []*defs.ToolCall{{
					CallID:    "echo",
					Name:      "mcp__ok__echo",
					Arguments: `{"text":"hello"}`,
				}})
				if result := results["echo"]; result == nil || result.Error == nil || result.Error.Code != "mcp_server_unavailable" {
					t.Fatalf("echo result after disconnect = %+v, want error code mcp_server_unavailable", result)
				}
			},
		},
	}
	// 2. 为每个用例创建环境，核对工具、日志、连接状态及进程停止结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newServers := tt.newServers
			if newServers == nil {
				newServers = defaultServers
			}
			newWorkspace := tt.newWorkspace
			if newWorkspace == nil {
				newWorkspace = newLocalWorkspace
			}
			tt.run(t, newEnvironmentFixture(t, "local", newWorkspace(t), newServers(), "bash"))
		})
	}
}
