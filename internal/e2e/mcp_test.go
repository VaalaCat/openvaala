package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
)

// buildFakeMCPServerOnce 在本包第一次用到时把 pkg/mcpclient 的假 MCP server 编译到 e2eBuildDirectory，之后的测试共用同一个二进制；
// 测试本身带 -race 时同样带 -race 编译。
var buildFakeMCPServerOnce = sync.OnceValues(func() (string, error) {
	// 1. 按当前测试的 race 设置编译假 MCP 服务并缓存结果。
	binary := filepath.Join(e2eBuildDirectory, "mcpserver")
	arguments := []string{"build", "-o", binary}
	if isRaceEnabled() {
		arguments = append(arguments, "-race")
	}
	command := exec.Command("go", append(arguments, "../../pkg/mcpclient/testdata/mcpserver")...)
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build fake MCP server: %w\n%s", err, output)
	}
	return binary, nil
})

// buildFakeMCPServer 返回测试共用的假 MCP 服务程序。
func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	// 1. 取得一次性编译结果，失败时结束测试。
	binary, err := buildFakeMCPServerOnce()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// mcpFakeServerPlaceholder 在各用例的配置文件里代表假 MCP server 二进制的路径，写入 HOME 时换成真实路径。
const mcpFakeServerPlaceholder = "$FAKE_MCP_SERVER"

const (
	// mcpFakeServerFile 配一个 stdio 假 server fake，提供 echo 等工具。
	mcpFakeServerFile = "id: fake\ntype: stdio\ncommand: " + mcpFakeServerPlaceholder + "\n"
	// mcpBrokenServerFile 配一个命令不存在的弱依赖 server broken。
	mcpBrokenServerFile = "id: broken\ntype: stdio\ncommand: /nonexistent\n"
	// mcpBrokenRequiredServerFile 是 broken 改成强依赖之后的内容。
	mcpBrokenRequiredServerFile = mcpBrokenServerFile + "required: true\n"
	// mcpLateServerFile 配一个运行中才新增的 stdio 假 server late。
	mcpLateServerFile = "id: late\ntype: stdio\ncommand: " + mcpFakeServerPlaceholder + "\n"
	// mcpPlainAgentFile 配一个 mcp_servers 为 [] 的 Agent plain：不用任何 MCP。
	mcpPlainAgentFile = "name: plain\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: default\nmcp_servers: []\n"
)

// TestMCPServers 检查 MCP 调用、连接失败、配置加载和编辑器工具。
func TestMCPServers(t *testing.T) {
	// 1. 构建主程序与假 MCP 服务，列出各场景的配置和执行步骤。
	binary := buildOpenVaala(t)
	fakeServer := buildFakeMCPServer(t)
	tests := []struct {
		name string
		// files 是除基础配置之外写进临时 HOME 的文件（相对 HOME 的路径 → 内容）。
		files map[string]string
		run   func(t *testing.T, f *mcpFixture)
	}{
		{
			name:  "stdio_tool_call",
			files: map[string]string{"mcp_servers/fake.yaml": mcpFakeServerFile},
			run: func(t *testing.T, f *mcpFixture) {
				// 1. 调用 echo，检查工具结果、Turn 完成和 MCP 连接状态。
				observer := f.openConversation(t, "assistant")
				observer.startTurn(t, "echo hi")
				first := f.model.next(t)
				if !slices.Contains(getModelToolNames(first.body), "mcp__fake__echo") {
					t.Errorf("first model request tools = %v, want mcp__fake__echo", getModelToolNames(first.body))
				}
				first.reply <- e2eModelReply{
					tool:      "mcp__fake__echo",
					arguments: `{"text":"hi"}`,
				}
				second := f.model.next(t)
				result := getModelToolResult(t, second.body)
				if result.Error != nil || string(result.Content) != `"hi"` {
					t.Errorf("tool result sent back to the model = content %s, error %+v, want content \"hi\" and no error", result.Content, result.Error)
				}
				second.reply <- e2eModelReply{text: "done"}
				f.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")
				server := f.waitMCPServer(t, observer.conversation.ID, "fake", mcpclient.ConnectionStateConnected)
				if server.ExecutorID != defs.LocalExecutorID || server.Required || server.Error != "" {
					t.Errorf("fake = executor %q, required %v, error %q, want local, not required and no error", server.ExecutorID, server.Required, server.Error)
				}
				if got := getMCPToolNames(server); !slices.Contains(got, "mcp__fake__echo") {
					t.Errorf("fake tools = %v, want mcp__fake__echo", got)
				}
			},
		},
		{
			name:  "weak_server_failed",
			files: map[string]string{"mcp_servers/fake.yaml": mcpFakeServerFile, "mcp_servers/broken.yaml": mcpBrokenServerFile},
			run: func(t *testing.T, f *mcpFixture) {
				// 1. 启动不可用的弱依赖服务，检查 Turn 完成、其他工具可用及失败状态和警告日志。
				observer := f.openConversation(t, "assistant")
				observer.startTurn(t, "hello")
				request := f.model.next(t)
				if names := getModelToolNames(request.body); !slices.Contains(names, "mcp__fake__echo") {
					t.Errorf("model request tools = %v, want mcp__fake__echo although broken failed", names)
				}
				request.reply <- e2eModelReply{text: "done"}
				f.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")
				broken := f.waitMCPServer(t, observer.conversation.ID, "broken", mcpclient.ConnectionStateFailed)
				if broken.Required || !strings.Contains(broken.Error, "/nonexistent") || len(broken.Tools) != 0 {
					t.Errorf("broken = required %v, error %q, %d tools, want weak, an error naming /nonexistent and no tools", broken.Required, broken.Error, len(broken.Tools))
				}
				f.waitMCPServer(t, observer.conversation.ID, "fake", mcpclient.ConnectionStateConnected)
				if records := getE2ELogRecords(t, f.config, "mcp server skipped"); len(records) == 0 || records[0]["mcp_server_id"] != "broken" {
					t.Errorf("mcp server skipped log records = %v, want one for broken", records)
				}
			},
		},
		{
			name:  "required_after_reload",
			files: map[string]string{"mcp_servers/broken.yaml": mcpBrokenServerFile},
			run: func(t *testing.T, f *mcpFixture) {
				// 1. broken 还是弱依赖时先完成一轮。
				observer := f.openConversation(t, "assistant")
				observer.startTurn(t, "before reload")
				f.model.next(t).reply <- e2eModelReply{text: "done"}
				f.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")

				// 2. 把 broken 改成强依赖并重新加载，检查下一轮直接失败、没有调用模型。
				f.writeFile(t, "mcp_servers/broken.yaml", mcpBrokenRequiredServerFile)
				f.reload(t)
				observer.startTurn(t, "after reload")
				f.assertTurnFinished(t, observer, defs.TurnStatusFailed, `mcp server "broken" is unavailable`)
				select {
				case request := <-f.model.requests:
					t.Fatalf("model was called although the required server is unavailable: %v", getModelToolNames(request.body))
				default:
				}
				broken := f.waitMCPServer(t, observer.conversation.ID, "broken", mcpclient.ConnectionStateFailed)
				if !broken.Required {
					t.Errorf("broken required = false after reload, want true")
				}
			},
		},
		{
			name:  "server_added_after_reload",
			files: map[string]string{"mcp_servers/fake.yaml": mcpFakeServerFile},
			run: func(t *testing.T, f *mcpFixture) {
				// 1. 重新加载前检查工具列表里只有 fake，没有 late。
				observer := f.openConversation(t, "assistant")
				observer.startTurn(t, "before reload")
				request := f.model.next(t)
				if names := getModelToolNames(request.body); slices.Contains(names, "mcp__late__echo") || !slices.Contains(names, "mcp__fake__echo") {
					t.Errorf("tools before reload = %v, want mcp__fake__echo and no mcp__late__echo", names)
				}
				request.reply <- e2eModelReply{text: "done"}
				f.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")

				// 2. 加入 late 并重新加载，检查两组工具都在，再调用 late 的 echo。
				f.writeFile(t, "mcp_servers/late.yaml", mcpLateServerFile)
				f.reload(t)
				observer.startTurn(t, "after reload")
				request = f.model.next(t)
				if names := getModelToolNames(request.body); !slices.Contains(names, "mcp__late__echo") || !slices.Contains(names, "mcp__fake__echo") {
					t.Errorf("tools after reload = %v, want mcp__late__echo and mcp__fake__echo", names)
				}
				request.reply <- e2eModelReply{
					tool:      "mcp__late__echo",
					arguments: `{"text":"late"}`,
				}
				request = f.model.next(t)
				if result := getModelToolResult(t, request.body); result.Error != nil || string(result.Content) != `"late"` {
					t.Errorf("late tool result = content %s, error %+v, want content \"late\" and no error", result.Content, result.Error)
				}
				request.reply <- e2eModelReply{text: "done"}
				f.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")
				f.waitMCPServer(t, observer.conversation.ID, "late", mcpclient.ConnectionStateConnected)
			},
		},
		{
			name:  "agent_without_mcp",
			files: map[string]string{"mcp_servers/fake.yaml": mcpFakeServerFile, "agents/plain.yaml": mcpPlainAgentFile},
			run: func(t *testing.T, f *mcpFixture) {
				// 1. 分别提交不使用 MCP 和使用默认 MCP 列表的 Agent，检查工具列表。
				for _, test := range []struct {
					agentName string
					wantMCP   bool
				}{{
					agentName: "plain",
					wantMCP:   false,
				}, {
					agentName: "assistant",
					wantMCP:   true,
				}} {
					observer := f.openConversation(t, test.agentName)
					observer.startTurn(t, "hello from "+test.agentName)
					request := f.model.next(t)
					hasMCPTool := slices.ContainsFunc(getModelToolNames(request.body), func(name string) bool { return strings.HasPrefix(name, "mcp__") })
					if hasMCPTool != test.wantMCP {
						t.Errorf("agent %s model request tools = %v, want MCP tools: %v", test.agentName, getModelToolNames(request.body), test.wantMCP)
					}
					request.reply <- e2eModelReply{text: "done"}
					f.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")
				}
			},
		},
		{
			name: "acp_editor_server",
			run: func(t *testing.T, f *mcpFixture) {
				// 1. 编辑器在 session/new 中携带 stdio 服务，检查工具调用、环境和父进程号。
				if runtime.GOOS != "linux" {
					t.Skip("the parent process id is read from /proc")
				}
				editor := startACPWire(t, f.binary, f.config)
				var created struct {
					SessionID string `json:"sessionId"`
				}
				editor.call(t, "session/new", map[string]any{"cwd": newE2EWorkspace(t), "mcpServers": []any{map[string]any{
					"name": "fake", "command": f.fakeServer, "args": []string{}, "env": []any{map[string]any{"name": "MCP_TEST_ENV", "value": "from-editor"}},
				}}}, &created)
				promptID := editor.request(t, "session/prompt", map[string]any{"sessionId": created.SessionID, "prompt": []any{map[string]any{"type": "text", "text": "use the editor tools"}}})

				request := f.model.next(t)
				if names := getModelToolNames(request.body); !slices.Contains(names, "mcp__fake__argv") || !slices.Contains(names, "mcp__fake__pid") {
					t.Errorf("first model request tools = %v, want mcp__fake__argv and mcp__fake__pid", names)
				}
				request.reply <- e2eModelReply{
					tool:      "mcp__fake__argv",
					arguments: `{}`,
				}
				request = f.model.next(t)
				var argv struct {
					Env string `json:"env"`
				}
				if err := json.Unmarshal([]byte(getMCPToolResultText(t, request.body)), &argv); err != nil || argv.Env != "from-editor" {
					t.Errorf("argv tool env = %q (%v), want the editor's MCP_TEST_ENV value from-editor", argv.Env, err)
				}
				request.reply <- e2eModelReply{
					tool:      "mcp__fake__pid",
					arguments: `{}`,
				}
				request = f.model.next(t)
				serverPID, err := strconv.Atoi(getMCPToolResultText(t, request.body))
				if err != nil {
					t.Fatalf("pid tool result: %v", err)
				}
				request.reply <- e2eModelReply{text: "done"}
				if response := editor.wire.response(t, promptID); response.Error != nil {
					t.Fatalf("session/prompt = %+v", response.Error)
				}
				if parentPID, acpPID := getParentPID(t, serverPID), editor.wire.process.command.Process.Pid; parentPID != acpPID {
					t.Errorf("fake MCP server parent pid = %d, want the acp process %d", parentPID, acpPID)
				}

				// 2. 关闭 ACP 输入并等待进程退出，检查连接工具被移除且下一轮没有这些工具。
				editor.wire.close(t)
				select {
				case <-editor.wire.process.exited:
				case <-time.After(e2eTimeout):
					t.Fatalf("acp process did not exit\n%s", editor.wire.process.stderr)
				}
				conversationID := defs.ConversationID(created.SessionID)
				f.waitNoMCPServer(t, conversationID, "fake")
				waitProcessGone(t, serverPID)
				observer := openRuntimeObserver(t, f.config, conversationID)
				observer.startTurn(t, "after the editor left")
				request = f.model.next(t)
				if names := getModelToolNames(request.body); slices.ContainsFunc(names, func(name string) bool { return strings.HasPrefix(name, "mcp__fake__") }) {
					t.Errorf("model request tools after acp exited = %v, want no mcp__fake__ tools", names)
				}
				request.reply <- e2eModelReply{text: "done"}
				f.assertTurnFinished(t, observer, defs.TurnStatusCompleted, "")
			},
		},
	}
	// 2. 每个场景使用独立配置和服务，建立控制 Client 后执行测试。
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			f := &mcpFixture{
				binary:     binary,
				config:     config,
				model:      model,
				fakeServer: fakeServer,
			}
			for name, content := range test.files {
				f.writeFile(t, name, content)
			}
			startE2EServe(t, binary, config)
			// 清理时先断开控制 Client 和 ACP，再停止服务。
			stopServeOnCleanup(t, binary, config)
			f.control, _ = newE2ERuntimeClient(t, config)
			// 控制 Client 不打开对话，只有连接状态事件。
			go func() {
				for range f.control.Events() {
				}
			}()
			test.run(t, f)
		})
	}
}

// mcpFixture 是 TestMCPServers 一个用例的环境：配置所在的临时 HOME、假模型，以及测试自己的控制 Client（新建对话、查 MCP 状态、reload）。
type mcpFixture struct {
	// binary 是 openvaala 二进制。
	binary string
	// config 是配置文件路径，所在目录是临时 HOME。
	config string
	model  *e2eModel
	// fakeServer 是假 MCP server 二进制的路径。
	fakeServer string
	control    runtimeclient.Client
}

// writeFile 写入测试配置文件并替换假 MCP 程序路径。
func (f *mcpFixture) writeFile(t *testing.T, name, content string) {
	t.Helper()
	// 1. 建立父目录，将占位符替换为实际程序路径并写入文件。
	path := filepath.Join(filepath.Dir(f.config), name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	content = strings.ReplaceAll(content, mcpFakeServerPlaceholder, f.fakeServer)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// openConversation 新建指定 Agent 的对话并跟随根 Session。
func (f *mcpFixture) openConversation(t *testing.T, agentName string) *runtimeObserver {
	t.Helper()
	// 1. 设置超时，使用新的本地工作目录创建对话，再打开观察连接。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	record, err := f.control.CreateConversation(ctx, &runtimeclient.CreateConversationRequest{
		AgentName:        agentName,
		ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: newE2EWorkspace(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return openRuntimeObserver(t, f.config, record.ID)
}

// reload 重新加载空闲对话的配置并检查无需等待。
func (f *mcpFixture) reload(t *testing.T) {
	t.Helper()
	// 1. 调用 ReloadConfig，检查请求成功且 PendingConversationIDs 为空。
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	result, err := f.control.ReloadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PendingConversationIDs) != 0 {
		t.Fatalf("config/reload has pending conversations %v, want none", result.PendingConversationIDs)
	}
}

// assertTurnFinished 等待 Turn 完成并检查状态和错误文字。
func (f *mcpFixture) assertTurnFinished(t *testing.T, observer *runtimeObserver, status defs.TurnStatus, wantError string) {
	t.Helper()
	// 1. 持续读取事件，检查下一条 TurnFinished 的状态和错误。
	for {
		finished, isFinished := observer.next(t).(*runtimeclient.TurnFinished)
		if !isFinished {
			continue
		}
		if finished.Status != status || !strings.Contains(finished.Error, wantError) {
			t.Errorf("turn finished with status %s, error %q, want status %s and error containing %q", finished.Status, finished.Error, status, wantError)
		}
		return
	}
}

// waitMCPServer 等待指定 MCP 服务达到连接状态并返回记录。
func (f *mcpFixture) waitMCPServer(t *testing.T, conversationID defs.ConversationID, serverID string, state mcpclient.ConnectionState) conversation.MCPServerInfo {
	t.Helper()
	// 1. 反复查询服务列表，找到目标服务和状态后返回，超时结束测试。
	deadline := time.Now().Add(e2eTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
		servers, err := f.control.ListMCPServers(ctx, conversationID)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		for _, server := range servers {
			if server.ServerID == serverID && server.State == state {
				return server
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcp server %s did not reach state %s, servers = %+v", serverID, state, servers)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitNoMCPServer 等待指定 MCP 服务从对话连接列表移除。
func (f *mcpFixture) waitNoMCPServer(t *testing.T, conversationID defs.ConversationID, serverID string) {
	t.Helper()
	// 1. 反复查询服务列表，目标服务消失后返回，超时结束测试。
	deadline := time.Now().Add(e2eTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
		servers, err := f.control.ListMCPServers(ctx, conversationID)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(servers, func(server conversation.MCPServerInfo) bool { return server.ServerID == serverID }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcp server %s is still listed, servers = %+v", serverID, servers)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// getMCPToolNames 返回 MCP 连接提供的工具名称列表。
func getMCPToolNames(server conversation.MCPServerInfo) []string {
	// 1. 按连接记录中的工具顺序收集名称。
	names := make([]string, 0, len(server.Tools))
	for _, tool := range server.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// getModelToolResult 返回模型请求中最后一条 tool 消息的工具结果。
func getModelToolResult(t *testing.T, body map[string]any) defs.ToolResult {
	t.Helper()
	// 1. 从后往前查找 tool 消息，拼接文字并解码工具结果；找不到时结束测试。
	messages, _ := body["messages"].([]any)
	for _, message := range slices.Backward(messages) {
		fields, _ := message.(map[string]any)
		if fields["role"] != "tool" {
			continue
		}
		var text strings.Builder
		switch content := fields["content"].(type) {
		case string:
			text.WriteString(content)
		case []any:
			for _, part := range content {
				partFields, _ := part.(map[string]any)
				partText, _ := partFields["text"].(string)
				text.WriteString(partText)
			}
		}
		var result defs.ToolResult
		if err := json.Unmarshal([]byte(text.String()), &result); err != nil {
			t.Fatalf("tool message %q is not a tool result: %v", text.String(), err)
		}
		return result
	}
	t.Fatal("model request has no tool message")
	return defs.ToolResult{}
}

// getMCPToolResultText 返回最后一个成功 MCP 工具结果中的字符串。
func getMCPToolResultText(t *testing.T, body map[string]any) string {
	t.Helper()
	// 1. 读取最后一个工具结果，检查无错误并将内容解码为字符串。
	result := getModelToolResult(t, body)
	var text string
	if err := json.Unmarshal(result.Content, &text); err != nil || result.Error != nil {
		t.Fatalf("tool result = content %s, error %+v, want a text content and no error", result.Content, result.Error)
	}
	return text
}

// waitProcessGone 等待 /proc 中的进程记录消失。
func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	// 1. 间隔检查进程目录，目录消失后返回，超时结束测试。
	deadline := time.Now().Add(e2eTimeout)
	for {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake MCP server process %d still exists after its stdin was closed", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// getParentPID 从 /proc 读取指定进程的父进程号。
func getParentPID(t *testing.T, pid int) int {
	t.Helper()
	// 1. 读取 stat，在最后一个右括号后拆分字段并解析父进程号。
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parent pid in %q: %v", data, err)
	}
	return parentPID
}
