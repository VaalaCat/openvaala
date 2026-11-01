package executor_test

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// commandTransport 保存命令及参数，每次连接使用新的命令进程。
type commandTransport struct {
	command   string
	arguments []string
}

// Connect 启动新的命令进程并连接 MCP。
func (t *commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	// 1. 创建本次使用的命令传输并连接。
	return (&mcp.CommandTransport{Command: exec.Command(t.command, t.arguments...)}).Connect(ctx)
}

// clientServer 是测试里客户端登记的一个 MCP server，用 command 加 arguments 起进程。
type clientServer struct {
	id string
	// command 为空时用假 MCP server。
	command   string
	arguments []string
	// startupTimeout 为 0 时取 5 秒。
	startupTimeout time.Duration
}

// clientMCPServersFixture 是一个用例的客户端 MCP server 集合和只用它的 ToolExecutor；登记的连接在后台运行，
// 直到登记被 stop 或测试结束。
type clientMCPServersFixture struct {
	client       executor.ClientMCPServers
	toolExecutor executor.ToolExecutor
	binary       string
	pipeline     *telemetrytest.Pipeline
	// ctx 带有记录日志的遥测管道，用来读取工具。
	ctx context.Context
	// stops 是各客户端连接登记的 stop，clientID 为键。
	stops map[string]func()
}

// newClientMCPServersFixture 创建带遥测的客户端 MCP 集合和工具执行器。
func newClientMCPServersFixture(t *testing.T) *clientMCPServersFixture {
	// 1. 创建客户端登记、工具执行器及共享测试资源。
	t.Helper()
	pipeline := telemetrytest.NewPipeline(t)
	client := executor.NewClientMCPServers()
	opt := executor.NewToolExecutorOpt(nil)
	opt.ClientMCPServers = client
	toolExecutor, err := executor.NewToolExecutor(opt)
	if err != nil {
		t.Fatal(err)
	}
	return &clientMCPServersFixture{
		client:       client,
		toolExecutor: toolExecutor,
		binary:       buildFakeMCPServer(t),
		pipeline:     pipeline,
		ctx:          telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry),
		stops:        make(map[string]func()),
	}
}

// register 替换客户端登记并运行新连接。
func (f *clientMCPServersFixture) register(t *testing.T, clientID string, servers ...clientServer) {
	// 1. 停止该客户端原有连接。
	t.Helper()
	if stop, found := f.stops[clientID]; found {
		stop()
	}
	// 2. 创建并运行弱依赖连接，登记本次连接和停止函数。
	ctx, cancel := context.WithCancel(t.Context())
	var runs sync.WaitGroup
	connections := make([]mcpclient.Connection, 0, len(servers))
	for _, server := range servers {
		command := server.command
		if command == "" {
			command = f.binary
		}
		definition := &mcpclient.ServerDefinition{
			ID:             server.id,
			StartupTimeout: cmp.Or(server.startupTimeout, 5*time.Second),
			ToolTimeout:    30 * time.Second,
		}
		connection, err := mcpclient.NewConnection(mcpclient.NewConnectionOpt(definition, nil).
			WithTransport(&commandTransport{
				command:   command,
				arguments: server.arguments,
			}))
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		runs.Go(func() { _ = connection.Run(ctx) })
	}
	stop := func() {
		cancel()
		runs.Wait()
	}
	t.Cleanup(stop)
	f.stops[clientID] = stop
	f.client.SetMCPServers(clientID, connections)
}

// getToolNames 建立本轮工具表并返回名称列表。
func (f *clientMCPServersFixture) getToolNames(t *testing.T) []string {
	// 1. 读取工具定义，核对成功后提取名称。
	t.Helper()
	tools, err := f.toolExecutor.GetTools(f.ctx)
	if err != nil {
		t.Fatalf("GetTools() error = %v", err)
	}
	return getToolNames(tools)
}

// callTool 执行一个工具，确认只收到一次结果后返回它。
func (f *clientMCPServersFixture) callTool(t *testing.T, name, arguments string) *defs.ToolResult {
	// 1. 执行调用并核对完成事件恰好一次。
	t.Helper()
	results, finishedCounts := runToolCalls(t, f.toolExecutor, []*defs.ToolCall{{
		CallID:    "call-1",
		Name:      name,
		Arguments: arguments,
	}})
	if finishedCounts["call-1"] != 1 {
		t.Fatalf("finished counts = %v, want exactly one result for call-1", finishedCounts)
	}
	return results["call-1"]
}

// getServerArguments 读取生效 server 的启动参数。
func (f *clientMCPServersFixture) getServerArguments(t *testing.T) string {
	// 1. 建立工具表，调用 argv 并读取正文。
	t.Helper()
	f.getToolNames(t)
	result := f.callTool(t, "mcp__fake__argv", `{}`)
	if result.Error != nil {
		t.Fatalf("argv result = %+v, want success", result)
	}
	return string(result.Content)
}

// TestClientMCPServers 验证客户端 MCP 登记、去重、工具执行及并发读取。
func TestClientMCPServers(t *testing.T) {
	// 1. 准备登记变化、重复 server 和调用失败的用例，逐项运行。
	tests := []struct {
		name string
		run  func(t *testing.T, f *clientMCPServersFixture)
	}{
		{
			name: "registered server provides tools",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{id: "fake"})
				tools, err := f.client.GetMCPTools(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				definitions := getToolDefinitions(tools)
				if !slices.Contains(getToolNames(definitions), "mcp__fake__echo") || !slices.Equal(getToolServerIDs(definitions), []string{"fake"}) {
					t.Fatalf("GetMCPTools() = %v with servers %v, want the fake server's tools", getToolNames(definitions), getToolServerIDs(definitions))
				}
				infos := f.client.GetMCPServers()
				if len(infos) != 1 || infos[0].ServerID != "fake" || infos[0].State != mcpclient.ConnectionStateConnected || infos[0].Required {
					t.Fatalf("GetMCPServers() = %+v, want one connected weak fake server", infos)
				}
			},
		},
		{
			name: "empty registration removes tools",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{id: "fake"})
				if names := f.getToolNames(t); !slices.Contains(names, "mcp__fake__echo") {
					t.Fatalf("GetTools() = %v, want the fake server's tools", names)
				}
				f.client.SetMCPServers("c1", nil)
				if names := f.getToolNames(t); len(names) != 0 {
					t.Fatalf("GetTools() after removal = %v, want none", names)
				}
				if infos := f.client.GetMCPServers(); len(infos) != 0 {
					t.Fatalf("GetMCPServers() after removal = %+v, want none", infos)
				}
			},
		},
		{
			name: "removing an unknown client changes nothing",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{id: "fake"})
				f.client.SetMCPServers("c2", nil)
				if names := f.getToolNames(t); !slices.Contains(names, "mcp__fake__echo") {
					t.Fatalf("GetTools() = %v, want the fake server's tools", names)
				}
			},
		},
		{
			name: "replacing a registration swaps its servers",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{id: "first"})
				f.register(t, "c1", clientServer{id: "second"})
				names := f.getToolNames(t)
				if !slices.Contains(names, "mcp__second__echo") || slices.Contains(names, "mcp__first__echo") {
					t.Fatalf("GetTools() = %v, want only the second server's tools", names)
				}
			},
		},
		{
			name: "two clients with different servers both provide tools",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{id: "first"})
				f.register(t, "c2", clientServer{id: "second"})
				names := f.getToolNames(t)
				if !slices.Contains(names, "mcp__first__echo") || !slices.Contains(names, "mcp__second__echo") {
					t.Fatalf("GetTools() = %v, want both servers' tools", names)
				}
				if got := getServerIDs(f.client.GetMCPServers()); !slices.Equal(got, []string{"first", "second"}) {
					t.Fatalf("GetMCPServers() server IDs = %v, want [first second] in registration order", got)
				}
			},
		},
		{
			// 先登记的 c1 生效，c2 的同 ID server 被跳过并记 Warn；状态列表只包含生效连接。
			name: "first registered client wins a duplicate server id",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{
					id:        "fake",
					arguments: []string{"from-c1"},
				})
				f.register(t, "c2", clientServer{
					id:        "fake",
					arguments: []string{"from-c2"},
				})
				if got := f.getServerArguments(t); !strings.Contains(got, "from-c1") || strings.Contains(got, "from-c2") {
					t.Fatalf("argv = %s, want the server of c1", got)
				}
				if infos := f.client.GetMCPServers(); len(infos) != 1 {
					t.Fatalf("GetMCPServers() = %+v, want only the winning server", infos)
				}
				records := f.pipeline.GetLogRecords(t, "client mcp server skipped")
				if len(records) == 0 {
					t.Fatal("no client mcp server skipped record, want a warning for c2")
				}
				record := records[0]
				for key, want := range map[string]string{"level": "WARN", "mcp_server_id": "fake", "client_id": "c2", "kept_client_id": "c1", "openvaala.executor.id": defs.ClientExecutorID} {
					if record[key] != want {
						t.Errorf("record %q = %v, want %q", key, record[key], want)
					}
				}
			},
		},
		{
			name: "removing the winning client hands the id to the next one",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{
					id:        "fake",
					arguments: []string{"from-c1"},
				})
				f.register(t, "c2", clientServer{
					id:        "fake",
					arguments: []string{"from-c2"},
				})
				f.client.SetMCPServers("c1", nil)
				if got := f.getServerArguments(t); !strings.Contains(got, "from-c2") {
					t.Fatalf("argv = %s, want the server of c2", got)
				}
			},
		},
		{
			// c1 替换登记后仍排在 c2 前面。
			name: "re-registering keeps the registration order",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{
					id:        "fake",
					arguments: []string{"from-c1"},
				})
				f.register(t, "c2", clientServer{
					id:        "fake",
					arguments: []string{"from-c2"},
				})
				f.register(t, "c1", clientServer{
					id:        "fake",
					arguments: []string{"from-c1-again"},
				})
				if got := f.getServerArguments(t); !strings.Contains(got, "from-c1-again") {
					t.Fatalf("argv = %s, want the new server of c1", got)
				}
			},
		},
		{
			name: "duplicate id inside one registration keeps the first",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{
					id:        "fake",
					arguments: []string{"first"},
				}, clientServer{
					id:        "fake",
					arguments: []string{"second"},
				})
				if got := f.getServerArguments(t); !strings.Contains(got, "first") || strings.Contains(got, "second") {
					t.Fatalf("argv = %s, want the first server", got)
				}
			},
		},
		{
			name: "failing server is skipped with a warning",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{
					id:      "broken",
					command: "/nonexistent/openvaala-mcp-client",
				}, clientServer{id: "fake"})
				names := f.getToolNames(t)
				if !slices.Contains(names, "mcp__fake__echo") || slices.ContainsFunc(names, func(name string) bool { return strings.HasPrefix(name, "mcp__broken__") }) {
					t.Fatalf("GetTools() = %v, want only the working server's tools", names)
				}
				records := f.pipeline.GetLogRecords(t, "mcp server skipped")
				if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["mcp_server_id"] != "broken" || records[0]["openvaala.executor.id"] != defs.ClientExecutorID {
					t.Fatalf("mcp server skipped records = %v, want one WARN for broken on the client executor", records)
				}
			},
		},
		{
			name: "tool call runs on the connection that provided it",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{id: "fake"})
				f.getToolNames(t)
				result := f.callTool(t, "mcp__fake__echo", `{"text":"hello"}`)
				if result.Error != nil || string(result.Content) != `"hello"` {
					t.Fatalf("echo result = %+v, want hello", result)
				}
			},
		},
		{
			// 本轮建表后新增的 server 要到下一轮才提供工具。
			name: "tool registered after the turn's tools were listed is not found",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.getToolNames(t)
				f.register(t, "c1", clientServer{id: "fake"})
				result := f.callTool(t, "mcp__fake__echo", `{"text":"hello"}`)
				if result.Error == nil || result.Error.Code != "tool_not_found" {
					t.Fatalf("echo result registered after GetTools = %+v, want tool_not_found", result)
				}
			},
		},
		{
			// 客户端连接停止后，本轮工具仍绑定旧连接，调用返回 mcp_server_unavailable。
			name: "tool of a stopped registration is unavailable",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				f.register(t, "c1", clientServer{id: "fake"})
				f.getToolNames(t)
				f.stops["c1"]()
				result := f.callTool(t, "mcp__fake__echo", `{"text":"hello"}`)
				if result.Error == nil || result.Error.Code != "mcp_server_unavailable" {
					t.Fatalf("echo result after the registration stopped = %+v, want mcp_server_unavailable", result)
				}
			},
		},
		{
			// 未启动的连接也可并发登记和读取状态。
			name: "concurrent registration and reads",
			run: func(t *testing.T, f *clientMCPServersFixture) {
				// 1. 创建未启动的连接，由一个 goroutine 更新登记，多个 goroutine 读取状态。
				connections := make(map[string]mcpclient.Connection)
				for index := range 4 {
					clientID := fmt.Sprintf("c%d", index)
					connection, err := mcpclient.NewConnection(mcpclient.NewConnectionOpt(&mcpclient.ServerDefinition{ID: "idle-" + clientID}, nil).
						WithTransport(&commandTransport{command: f.binary}))
					if err != nil {
						t.Fatal(err)
					}
					connections[clientID] = connection
				}
				var tasks sync.WaitGroup
				tasks.Go(func() {
					for range 50 {
						for clientID, connection := range connections {
							f.client.SetMCPServers(clientID, []mcpclient.Connection{connection})
							f.client.SetMCPServers(clientID, nil)
						}
					}
				})
				for range len(connections) {
					tasks.Go(func() {
						for range 50 {
							_ = f.client.GetMCPServers()
						}
					})
				}
				tasks.Wait()
				if infos := f.client.GetMCPServers(); len(infos) != 0 {
					t.Fatalf("GetMCPServers() = %+v, want none after every registration was removed", infos)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newClientMCPServersFixture(t))
		})
	}
}
