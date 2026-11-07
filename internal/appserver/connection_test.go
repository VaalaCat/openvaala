package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
)

const mcpTestConversationID defs.ConversationID = "00000000-0000-7000-8000-0000000000d1"

// TestSetThreadNameNotifications 检查 thread/name/set 先回响应，成功时再发一次标题通知，失败时不发通知。
func TestSetThreadNameNotifications(t *testing.T) {
	// 1. 准备已加载、未加载、长标题和保存失败场景。
	tests := []struct {
		name     string
		isLoaded bool
		title    string
		err      error
	}{
		{
			name:     "loaded_notifies_once_after_response",
			isLoaded: true,
			title:    "新的标题",
		},
		{
			name:  "not_loaded_still_notifies",
			title: "新的标题",
		},
		{
			name:  "not_loaded_truncates_title",
			title: strings.Repeat("字", 300),
		},
		{
			name:     "loaded_failure_only_responds",
			isLoaded: true,
			err:      errors.New("save failed"),
		},
		{
			name: "not_loaded_failure_only_responds",
			err:  errors.New("save failed"),
		},
	}
	// 2. 执行改名，核对先响应、成功时只通知一次，失败时无通知。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeThreadClient{
				record:   defs.Conversation{ID: mcpTestConversationID},
				titleErr: tt.err,
			}
			outgoing := make(chan codexMessage, 8)
			connection := &appServerConnection{
				client:        client,
				isInitialized: true,
				outgoing:      outgoing,
			}
			handled := make(chan struct{})
			if tt.isLoaded {
				client.events = make(chan runtimeclient.ClientEvent, 1)
				thread := &connectionThread{
					threadID:        string(mcpTestConversationID),
					conversationID:  mcpTestConversationID,
					client:          client,
					outgoing:        outgoing,
					isOpenDelivered: true,
					requests:        make(chan connectionThreadRequest),
				}
				connection.loadedThreads = map[string]*connectionThread{thread.threadID: thread}
				go func() {
					defer close(handled)
					thread.handleRequest(t.Context(), <-thread.requests)
					if err := thread.handleQueuedClientEvents(t.Context()); err != nil {
						t.Error(err)
					}
				}()
			} else {
				close(handled)
			}
			params, err := json.Marshal(codexThreadSetNameParams{
				ThreadID: string(mcpTestConversationID),
				Name:     tt.title,
			})
			if err != nil {
				t.Fatal(err)
			}
			connection.handleReceived(t.Context(), codexReceived{message: codexMessage{
				ID:     json.RawMessage(`1`),
				Method: "thread/name/set",
				Params: params,
			}})
			<-handled
			response := <-outgoing
			if string(response.ID) != "1" || response.Method != "" || (response.Error != nil) != (tt.err != nil) {
				t.Fatalf("first message = %+v, want rename response", response)
			}
			if tt.err == nil {
				notification := <-outgoing
				var update codexThreadNameUpdatedNotification
				if err := json.Unmarshal(notification.Params, &update); err != nil {
					t.Fatal(err)
				}
				if notification.Method != "thread/name/updated" || update.ThreadID != string(mcpTestConversationID) || update.ThreadName != defs.TruncateConversationTitle(tt.title) {
					t.Fatalf("title notification = %+v", notification)
				}
			}
			select {
			case message := <-outgoing:
				t.Fatalf("unexpected extra message = %+v", message)
			default:
			}
		})
	}
}

// fakeMCPClient 是只实现 mcpServerStatus/list 和 config/mcpServer/reload 用到的调用的 runtimeclient.Client。
type fakeMCPClient struct {
	runtimeclient.Client
	servers   []conversation.MCPServerInfo
	executors []conversation.ExecutorDetails
	reloadErr error

	// 以下是收到的调用。
	listedConversations []defs.ConversationID
	reloadCount         int
}

// ListMCPServers 记录对话查询并返回预设 MCP 连接。
func (f *fakeMCPClient) ListMCPServers(_ context.Context, conversationID defs.ConversationID) ([]conversation.MCPServerInfo, error) {
	// 1. 保存请求的对话 ID，返回连接列表。
	f.listedConversations = append(f.listedConversations, conversationID)
	return f.servers, nil
}

// GetSessionDetails 返回测试 Executor 列表。
func (f *fakeMCPClient) GetSessionDetails(context.Context, defs.ConversationID, defs.SessionID) (conversation.SessionDetails, error) {
	// 1. 将预设 Executor 填入 Session 详情。
	return conversation.SessionDetails{Executors: f.executors}, nil
}

// ReloadConfig 记录重新加载次数并返回预设错误。
func (f *fakeMCPClient) ReloadConfig(context.Context) (runtime.SetDefinitionsResult, error) {
	// 1. 累计调用次数，返回空结果及预设错误。
	f.reloadCount++
	return runtime.SetDefinitionsResult{}, f.reloadErr
}

// newMCPConnection 创建已握手并加载根 Thread 的测试连接。
func newMCPConnection(client runtimeclient.Client) *appServerConnection {
	// 1. 设置控制 Client 和预设对话的根 Thread。
	return &appServerConnection{
		client:        client,
		isInitialized: true,
		loadedThreads: map[string]*connectionThread{
			string(mcpTestConversationID): {
				threadID:       string(mcpTestConversationID),
				conversationID: mcpTestConversationID,
				sessionID:      "00000000-0000-7000-8000-0000000000a0",
			},
		},
	}
}

// newMCPServer 创建指定 Executor 下的测试 MCP 连接。
func newMCPServer(executorID, serverID string, state mcpclient.ConnectionState, tools ...mcpclient.ConnectionTool) conversation.MCPServerInfo {
	// 1. 填写 server、状态和工具，失败状态附带连接超时错误。
	info := mcpclient.ConnectionInfo{
		ServerID: serverID,
		State:    state,
		Tools:    tools,
	}
	if state == mcpclient.ConnectionStateFailed {
		info.Error = "dial tcp: i/o timeout"
	}
	return conversation.MCPServerInfo{
		ExecutorID:     executorID,
		ConnectionInfo: info,
	}
}

// newMCPTool 创建测试 MCP 工具。
func newMCPTool(originalName, serverID, description string) mcpclient.ConnectionTool {
	// 1. 填写线上工具名、原始名称、描述和空对象 schema。
	return mcpclient.ConnectionTool{
		Name:         "mcp__" + serverID + "__" + originalName,
		OriginalName: originalName,
		Description:  description,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
	}
}

// getMCPStatusJSON 拼接预期 MCP 状态 JSON。
func getMCPStatusJSON(name, runtimeStatus, tools, toolsError string) string {
	// 1. 填入名称、状态、工具和错误，资源为空、认证为 unsupported。
	return `{"name":"` + name + `","runtimeStatus":"` + runtimeStatus + `","pluginId":null,"serverInfo":null,"tools":` + tools +
		`,"toolsError":` + toolsError + `,"resources":[],"resourceTemplates":[],"authStatus":"unsupported"}`
}

// TestListMcpServerStatuses 检查 MCP 状态列表和同名连接命名。
func TestListMcpServerStatuses(t *testing.T) {
	// 1. 准备未加载、空列表、连接状态和多 Executor 场景。
	const emptyPage = `{"data":[],"nextCursor":null}`
	const echoTool = `{"echo":{"name":"echo","description":"回显","inputSchema":{"type":"object"}}}`
	tests := []struct {
		name      string
		params    string
		servers   []conversation.MCPServerInfo
		executors []conversation.ExecutorDetails
		want      string
		// wantListed 是 serve 收到 mcpServer/list 的对话；没带 threadId 或 Thread 没加载时不查 serve。
		wantListed []defs.ConversationID
	}{
		{
			name:   "without_thread_id_is_empty_page",
			params: `{"detail":"toolsAndAuthOnly"}`,
			servers: []conversation.MCPServerInfo{
				newMCPServer("local", "fake", mcpclient.ConnectionStateConnected, newMCPTool("echo", "fake", "回显")),
			},
			want: emptyPage,
		},
		{
			name:   "null_thread_id_is_empty_page",
			params: `{"threadId":null}`,
			want:   emptyPage,
		},
		{
			name:   "thread_not_loaded_is_empty_page",
			params: `{"threadId":"00000000-0000-7000-8000-0000000000d2"}`,
			servers: []conversation.MCPServerInfo{
				newMCPServer("local", "fake", mcpclient.ConnectionStateConnected, newMCPTool("echo", "fake", "回显")),
			},
			want: emptyPage,
		},
		{
			name:       "loaded_thread_without_servers_is_empty_page",
			params:     `{"threadId":"` + string(mcpTestConversationID) + `"}`,
			want:       emptyPage,
			wantListed: []defs.ConversationID{mcpTestConversationID},
		},
		{
			name:   "connected_server_lists_tools_by_original_name",
			params: `{"threadId":"` + string(mcpTestConversationID) + `"}`,
			servers: []conversation.MCPServerInfo{
				newMCPServer("local", "fake", mcpclient.ConnectionStateConnected, newMCPTool("echo", "fake", "回显"), newMCPTool("ping", "fake", "")),
			},
			executors: []conversation.ExecutorDetails{{ID: "local"}},
			want: `{"data":[` + getMCPStatusJSON("fake", "connected",
				`{"echo":{"name":"echo","description":"回显","inputSchema":{"type":"object"}},"ping":{"name":"ping","inputSchema":{"type":"object"}}}`, "null") +
				`],"nextCursor":null}`,
			wantListed: []defs.ConversationID{mcpTestConversationID},
		},
		{
			name:   "connecting_and_failed_servers",
			params: `{"threadId":"` + string(mcpTestConversationID) + `"}`,
			servers: []conversation.MCPServerInfo{
				newMCPServer("local", "slow", mcpclient.ConnectionStateConnecting),
				newMCPServer("local", "broken", mcpclient.ConnectionStateFailed),
			},
			executors: []conversation.ExecutorDetails{{ID: "local"}},
			want: `{"data":[` + getMCPStatusJSON("slow", "starting", `{}`, "null") + `,` +
				getMCPStatusJSON("broken", "failed", `{}`, `"dial tcp: i/o timeout"`) + `],"nextCursor":null}`,
			wantListed: []defs.ConversationID{mcpTestConversationID},
		},
		{
			name:   "same_server_on_two_executors_suffixes_the_non_default_one",
			params: `{"threadId":"` + string(mcpTestConversationID) + `"}`,
			servers: []conversation.MCPServerInfo{
				newMCPServer("local", "fake", mcpclient.ConnectionStateConnected, newMCPTool("echo", "fake", "回显")),
				newMCPServer("extra", "fake", mcpclient.ConnectionStateConnected, newMCPTool("echo", "fake", "回显")),
			},
			executors: []conversation.ExecutorDetails{{ID: "local"}, {ID: "extra"}},
			want: `{"data":[` + getMCPStatusJSON("fake", "connected", echoTool, "null") + `,` +
				getMCPStatusJSON("fake@extra", "connected", echoTool, "null") + `],"nextCursor":null}`,
			wantListed: []defs.ConversationID{mcpTestConversationID},
		},
		{
			name:   "same_server_only_on_non_default_executors_suffixes_both",
			params: `{"threadId":"` + string(mcpTestConversationID) + `"}`,
			servers: []conversation.MCPServerInfo{
				newMCPServer("first", "fake", mcpclient.ConnectionStateConnecting),
				newMCPServer("second", "fake", mcpclient.ConnectionStateConnecting),
			},
			executors: []conversation.ExecutorDetails{{ID: "local"}, {ID: "first"}, {ID: "second"}},
			want: `{"data":[` + getMCPStatusJSON("fake@first", "starting", `{}`, "null") + `,` +
				getMCPStatusJSON("fake@second", "starting", `{}`, "null") + `],"nextCursor":null}`,
			wantListed: []defs.ConversationID{mcpTestConversationID},
		},
		{
			name:   "server_on_one_non_default_executor_keeps_plain_name",
			params: `{"threadId":"` + string(mcpTestConversationID) + `"}`,
			servers: []conversation.MCPServerInfo{
				newMCPServer("extra", "fake", mcpclient.ConnectionStateConnected, newMCPTool("echo", "fake", "回显")),
			},
			executors:  []conversation.ExecutorDetails{{ID: "local"}, {ID: "extra"}},
			want:       `{"data":[` + getMCPStatusJSON("fake", "connected", echoTool, "null") + `],"nextCursor":null}`,
			wantListed: []defs.ConversationID{mcpTestConversationID},
		},
	}
	// 2. 调用协议请求，核对完整响应和查询的对话 ID。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeMCPClient{
				servers:   tt.servers,
				executors: tt.executors,
			}
			c := newMCPConnection(client)
			result, err := c.handleRequest(t.Context(), codexMessage{
				Method: "mcpServerStatus/list",
				Params: json.RawMessage(tt.params),
			})
			if err != nil {
				t.Fatalf("mcpServerStatus/list error = %v", err)
			}
			got, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("mcpServerStatus/list = %s\nwant %s", got, tt.want)
			}
			if !reflect.DeepEqual(client.listedConversations, tt.wantListed) {
				t.Fatalf("mcpServer/list conversations = %v, want %v", client.listedConversations, tt.wantListed)
			}
		})
	}
}

// TestReloadMcpServers 检查 MCP 配置重新加载结果。
func TestReloadMcpServers(t *testing.T) {
	// 1. 准备加载成功和失败结果。
	tests := []struct {
		name        string
		reloadErr   error
		wantResult  string
		wantMessage string
	}{
		{
			name:       "reload_returns_empty_object",
			wantResult: `{}`,
		},
		{
			name:        "reload_failure_is_reported",
			reloadErr:   errors.New(`agent "assistant" uses model gw/m1, which is not configured`),
			wantMessage: `agent "assistant" uses model gw/m1, which is not configured`,
		},
	}
	// 2. 发送重新加载请求，核对调用次数、响应和错误原文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeMCPClient{reloadErr: tt.reloadErr}
			c := newMCPConnection(client)
			result, err := c.handleRequest(t.Context(), codexMessage{
				Method: "config/mcpServer/reload",
				Params: json.RawMessage(`{}`),
			})
			if client.reloadCount != 1 {
				t.Fatalf("ReloadConfig calls = %d, want 1", client.reloadCount)
			}
			if tt.wantMessage != "" {
				if err == nil || err.Error() != tt.wantMessage {
					t.Fatalf("config/mcpServer/reload error = %v, want %q", err, tt.wantMessage)
				}
				return
			}
			got, marshalErr := json.Marshal(result)
			if err != nil || marshalErr != nil || string(got) != tt.wantResult {
				t.Fatalf("config/mcpServer/reload = %s (%v, %v), want %s", got, err, marshalErr, tt.wantResult)
			}
		})
	}
}

// fakeHooksClient 是只实现 hooks/list 用到的调用的 runtimeclient.Client。
type fakeHooksClient struct {
	runtimeclient.Client
	hooks     []*hook.HookInfo
	hooksErr  error
	executors []conversation.ExecutorDetails
}

// GetHooks 返回测试 hook 或预设错误。
func (f *fakeHooksClient) GetHooks(context.Context) ([]*hook.HookInfo, error) {
	// 1. 读取 hook 列表和查询错误。
	return f.hooks, f.hooksErr
}

// ListExecutors 返回测试 Executor 列表。
func (f *fakeHooksClient) ListExecutors(context.Context) ([]conversation.ExecutorDetails, error) {
	// 1. 读取预设 Executor。
	return f.executors, nil
}

// TestListHooks 检查各目录的 hook 列表和默认目录映射。
func TestListHooks(t *testing.T) {
	// 1. 准备 hook、Executor、路径映射和各场景响应。
	hooks := []*hook.HookInfo{
		{
			Key:            "go.yaml:PreToolUse:0",
			Event:          defs.HookEventPreToolUse,
			Matcher:        "Bash|Read",
			Handler:        "check.sh --strict",
			TimeoutSeconds: 30,
			SourcePath:     "/hooks/go.yaml",
			DisplayOrder:   1,
			CurrentHash:    "hash-0",
		},
		{
			Key:            "go.yaml:UserPromptSubmit:1",
			Event:          defs.HookEventUserPromptSubmit,
			Handler:        "/hooks/prompt.sh a",
			TimeoutSeconds: 10,
			SourcePath:     "/hooks/go.yaml",
			DisplayOrder:   2,
			CurrentHash:    "hash-1",
		},
	}
	const wantHooks = `[
		{"key":"go.yaml:PreToolUse:0","eventName":"preToolUse","handlerType":"command","command":"check.sh --strict","async":false,"matcher":"Bash|Read","timeoutSec":30,
		 "statusMessage":null,"additionalContextLimit":null,"sourcePath":"/hooks/go.yaml","source":"user","pluginId":null,"displayOrder":1,"enabled":true,"isManaged":false,
		 "currentHash":"hash-0","trustStatus":"trusted"},
		{"key":"go.yaml:UserPromptSubmit:1","eventName":"userPromptSubmit","handlerType":"command","command":"/hooks/prompt.sh a","async":false,"matcher":null,"timeoutSec":10,
		 "statusMessage":null,"additionalContextLimit":null,"sourcePath":"/hooks/go.yaml","source":"user","pluginId":null,"displayOrder":2,"enabled":true,"isManaged":false,
		 "currentHash":"hash-1","trustStatus":"trusted"}
	]`
	executors := []conversation.ExecutorDetails{{
		ID:                 defs.LocalExecutorID,
		WorkspaceDirectory: "/serve/project",
	}, {
		ID:                 "builtin",
		WorkspaceDirectory: "/workspace",
	}}
	tests := []struct {
		name         string
		params       string
		hooks        []*hook.HookInfo
		hooksErr     error
		pathMappings config.PathMappings
		wantResult   string
		wantMessage  string
	}{
		{
			name:   "each_requested_directory_gets_the_same_hooks",
			params: `{"cwds":["/a","/b"]}`,
			hooks:  hooks,
			wantResult: `{"data":[
				{"cwd":"/a","hooks":` + wantHooks + `,"warnings":[],"errors":[]},
				{"cwd":"/b","hooks":` + wantHooks + `,"warnings":[],"errors":[]}
			]}`,
		},
		{
			name:       "empty_cwds_uses_the_directory_for_new_threads",
			params:     `{"cwds":[]}`,
			hooks:      hooks,
			wantResult: `{"data":[{"cwd":"/serve/project","hooks":` + wantHooks + `,"warnings":[],"errors":[]}]}`,
		},
		{
			name:   "missing_cwds_maps_the_directory_back_to_the_client_path",
			params: `{}`,
			hooks:  hooks,
			pathMappings: config.PathMappings{{
				Client: "/client",
				Server: "/serve",
			}},
			wantResult: `{"data":[{"cwd":"/client/project","hooks":` + wantHooks + `,"warnings":[],"errors":[]}]}`,
		},
		{
			name:       "no_hooks_is_an_empty_list",
			params:     `{"cwds":["/a"]}`,
			wantResult: `{"data":[{"cwd":"/a","hooks":[],"warnings":[],"errors":[]}]}`,
		},
		{
			name:        "runtime_error_is_reported",
			params:      `{"cwds":["/a"]}`,
			hooksErr:    errors.New("hook list unavailable"),
			wantMessage: "hook list unavailable",
		},
	}
	// 2. 发送 hooks/list，核对错误原文或完整 JSON。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &appServerConnection{
				client: &fakeHooksClient{
					hooks:     tt.hooks,
					hooksErr:  tt.hooksErr,
					executors: executors,
				},
				pathMappings:  tt.pathMappings,
				isInitialized: true,
			}
			result, err := c.handleRequest(t.Context(), codexMessage{
				Method: "hooks/list",
				Params: json.RawMessage(tt.params),
			})
			if tt.wantMessage != "" {
				if err == nil || err.Error() != tt.wantMessage {
					t.Fatalf("hooks/list error = %v, want %q", err, tt.wantMessage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var gotValue, wantValue any
			if err := json.Unmarshal(got, &gotValue); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.wantResult), &wantValue); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Fatalf("hooks/list = %s, want %s", got, tt.wantResult)
			}
		})
	}
}

// fakeForkClient 是 thread/fork、thread/inject_items、thread/archive、thread/unarchive、thread/delete 用的 runtimeclient.Client：对话和 Session 的查询用
// fakeHistoryClient，ForkConversation 返回 forkErr。
type fakeForkClient struct {
	*fakeHistoryClient
	forkErr error
}

// ForkConversation 返回预设的分叉错误。
func (f *fakeForkClient) ForkConversation(context.Context, *runtimeclient.ForkConversationRequest) (defs.Conversation, error) {
	// 1. 返回空对话和 forkErr。
	return defs.Conversation{}, f.forkErr
}

// TestRootThreadOnlyMethods 检查仅根 Thread 支持的方法拒绝子 Thread 和未知 Thread。
func TestRootThreadOnlyMethods(t *testing.T) {
	// 1. 准备已登记、未登记子 Thread 和不存在 ID。
	tests := []struct {
		name     string
		threadID string
		// isRegistered 子 Thread 是否已经登记在 childThreadConversations 里？本连接跟随根 Thread 时会登记。
		isRegistered bool
		// wantMessage 按 method 生成预期的错误文字。
		wantMessage func(method string) string
	}{
		{
			name:         "registered_child_thread",
			threadID:     string(historyChildOneID),
			isRegistered: true,
			wantMessage:  func(method string) string { return "only root threads support " + method },
		},
		{
			name:        "child_thread_found_on_serve",
			threadID:    string(historyChildOneID),
			wantMessage: func(method string) string { return "only root threads support " + method },
		},
		{
			name:        "missing_thread",
			threadID:    "missing",
			wantMessage: func(string) string { return "thread not found: missing" },
		},
	}
	// 2. 遍历根 Thread 专用方法，核对 -32600 和错误文本。
	for _, tt := range tests {
		for _, method := range []string{"thread/fork", "thread/inject_items", "thread/archive", "thread/unarchive", "thread/delete"} {
			t.Run(tt.name+"/"+method, func(t *testing.T) {
				client := newHistoryConversationClient(nil)
				client.sessions = append(client.sessions, defs.Session{
					ID:              historyChildOneID,
					ConversationID:  historyConversationID,
					AgentName:       "researcher",
					ParentSessionID: historyRootSessionID,
				})
				c := newHistoryConnection(&fakeForkClient{fakeHistoryClient: client})
				c.isInitialized = true
				if tt.isRegistered {
					c.childThreadConversations[historyChildOneID] = historyConversationID
				}
				params, err := json.Marshal(codexThreadParams{ThreadID: tt.threadID})
				if err != nil {
					t.Fatal(err)
				}
				_, err = c.handleRequest(t.Context(), codexMessage{
					ID:     json.RawMessage(`1`),
					Method: method,
					Params: params,
				})
				if got, want := getCodexError(err, ""), (codexError{
					errorCodeInvalidRequest,
					tt.wantMessage(method),
				}); err == nil || *got != want {
					t.Fatalf("%s %s error = %v, want %+v", method, tt.threadID, err, want)
				}
			})
		}
	}
}

// TestForkThreadErrors 检查分叉请求的错误转换。
func TestForkThreadErrors(t *testing.T) {
	// 1. 准备临时来源、临时请求和各截取点错误。
	tests := []struct {
		name   string
		params string
		// isEphemeralSource 来源是不是临时对话？临时对话的记录里没有 Executor 设置（WorkspaceDirectories 为 nil）。
		isEphemeralSource bool
		// forkErr 是 serve 拒绝分叉时 Client 返回的错误。
		forkErr error
		want    codexError
	}{
		{
			// 临时来源带 cwd 时复制目录设置，serve 拒绝分叉并返回对话不存在。
			name:              "ephemeral_source_with_cwd",
			params:            `{"threadId":"` + string(historyConversationID) + `","cwd":"/x"}`,
			isEphemeralSource: true,
			forkErr:           fmt.Errorf("conversation/fork: %w: %s", runtime.ErrConversationNotFound, historyConversationID),
			want: codexError{
				errorCodeInvalidRequest,
				"thread not found: " + string(historyConversationID),
			},
		},
		{
			name:    "both_boundaries",
			params:  `{"threadId":"` + string(historyConversationID) + `","beforeTurnId":"t2","lastTurnId":"t1"}`,
			forkErr: fmt.Errorf("conversation/fork: %w", conversation.ErrForkBoundaryConflict),
			want: codexError{
				errorCodeInvalidRequest,
				"`beforeTurnId` cannot be combined with `lastTurnId`",
			},
		},
		{
			name:    "before_turn_not_found",
			params:  `{"threadId":"` + string(historyConversationID) + `","beforeTurnId":"t9"}`,
			forkErr: fmt.Errorf("conversation/fork: %w", conversation.ErrForkTurnNotFound),
			want: codexError{
				errorCodeInvalidRequest,
				"beforeTurnId 't9' was not found in the source thread",
			},
		},
		{
			name:    "last_turn_not_found",
			params:  `{"threadId":"` + string(historyConversationID) + `","lastTurnId":"t9"}`,
			forkErr: fmt.Errorf("conversation/fork: %w", conversation.ErrForkTurnNotFound),
			want: codexError{
				errorCodeInvalidRequest,
				"lastTurnId 't9' was not found in the source thread",
			},
		},
	}
	// 2. 发送分叉请求，核对 Codex 错误码和文本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newHistoryConversationClient(nil)
			if tt.isEphemeralSource {
				client.conversations[0].ExecutorSettings = defs.ExecutorSettings{}
			}
			c := newHistoryConnection(&fakeForkClient{
				fakeHistoryClient: client,
				forkErr:           tt.forkErr,
			})
			c.isInitialized = true
			_, err := c.handleRequest(t.Context(), codexMessage{
				ID:     json.RawMessage(`1`),
				Method: "thread/fork",
				Params: json.RawMessage(tt.params),
			})
			if got := getCodexError(err, ""); err == nil || *got != tt.want {
				t.Fatalf("thread/fork %s error = %v, want %+v", tt.params, err, tt.want)
			}
		})
	}
}

// fakeSideConversationClient 是记录 thread/start、thread/fork、thread/inject_items 交给 serve 的请求的 runtimeclient.Client：
// 对话和 Session 的查询用 fakeHistoryClient。
type fakeSideConversationClient struct {
	*fakeHistoryClient
	createRequests []runtimeclient.CreateConversationRequest
	forkRequests   []runtimeclient.ForkConversationRequest
	appendRequests []runtimeclient.AppendContextItemsRequest
}

// CreateConversation 记录创建请求，返回新对话记录。
func (f *fakeSideConversationClient) CreateConversation(_ context.Context, request *runtimeclient.CreateConversationRequest) (defs.Conversation, error) {
	// 1. 记录请求，返回测试对话。
	f.createRequests = append(f.createRequests, *request)
	return defs.Conversation{ID: "created", RootSessionID: "created-root"}, nil
}

// ForkConversation 记录分叉请求，返回分叉出的对话记录。
func (f *fakeSideConversationClient) ForkConversation(_ context.Context, request *runtimeclient.ForkConversationRequest) (defs.Conversation, error) {
	// 1. 记录请求，返回测试对话。
	f.forkRequests = append(f.forkRequests, *request)
	return defs.Conversation{ID: "forked", RootSessionID: "forked-root"}, nil
}

// AppendContextItems 记录注入请求。
func (f *fakeSideConversationClient) AppendContextItems(_ context.Context, request *runtimeclient.AppendContextItemsRequest) error {
	// 1. 记录请求。
	f.appendRequests = append(f.appendRequests, *request)
	return nil
}

// TestSideConversationRequests 检查 thread/start 的 environments、thread/fork 的 ephemeral 和 developerInstructions，以及 thread/inject_items 转给 serve 的请求。
func TestSideConversationRequests(t *testing.T) {
	// 1. 准备各请求的参数和期望交给 serve 的请求；注入条目期望转成的客户端指令正文按调用分组。
	const noRuntimeURL = "runtime client requires a URL"
	rootID := string(historyConversationID)
	injectParams := func(items string) string {
		return `{"threadId":"` + rootID + `","items":` + items + `}`
	}
	sourceSettings := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: "/new"}}
	const wantInjectError = "thread/inject_items only supports user and developer message items"
	tests := []struct {
		name   string
		method string
		params string
		// wantErr 是请求返回的错误文本。thread/start 和 thread/fork 在测试里没有真实 serve 可跟随，请求交给 serve 后在跟随 Thread 时失败，
		// 失败说明请求已走到跟随这一步；跟随和回复由 internal/e2e 的 TestCodexSideConversation 覆盖。
		wantErr        string
		wantCreate     []runtimeclient.CreateConversationRequest
		wantFork       []runtimeclient.ForkConversationRequest
		wantAppendText [][]string
	}{
		{
			name:    "ephemeral fork with developer instructions",
			method:  "thread/fork",
			params:  `{"threadId":"` + rootID + `","ephemeral":true,"developerInstructions":"side rules"}`,
			wantErr: noRuntimeURL,
			wantFork: []runtimeclient.ForkConversationRequest{{
				ConversationID:   historyConversationID,
				ExecutorSettings: sourceSettings,
				Ephemeral:        true,
			}},
			wantAppendText: [][]string{{"side rules"}},
		},
		{
			name:    "fork without developer instructions",
			method:  "thread/fork",
			params:  `{"threadId":"` + rootID + `","ephemeral":true}`,
			wantErr: noRuntimeURL,
			wantFork: []runtimeclient.ForkConversationRequest{{
				ConversationID:   historyConversationID,
				ExecutorSettings: sourceSettings,
				Ephemeral:        true,
			}},
		},
		{
			name:    "saved fork",
			method:  "thread/fork",
			params:  `{"threadId":"` + rootID + `"}`,
			wantErr: noRuntimeURL,
			wantFork: []runtimeclient.ForkConversationRequest{{
				ConversationID:   historyConversationID,
				ExecutorSettings: sourceSettings,
			}},
		},
		{
			name:           "inject user message",
			method:         "thread/inject_items",
			params:         injectParams(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"boundary"}]}]`),
			wantAppendText: [][]string{{"boundary"}},
		},
		{
			name:           "inject developer message",
			method:         "thread/inject_items",
			params:         injectParams(`[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]}]`),
			wantAppendText: [][]string{{"rules"}},
		},
		{
			name:   "inject text parts joined in order",
			method: "thread/inject_items",
			params: injectParams(`[{"type":"message","role":"user","content":[` +
				`{"type":"input_text","text":"a"},{"type":"input_image","image_url":"data:,"},{"type":"input_text","text":"b"}]}]`),
			wantAppendText: [][]string{{"ab"}},
		},
		{
			name:   "inject several messages in one call",
			method: "thread/inject_items",
			params: injectParams(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]},` +
				`{"type":"message","role":"developer","content":[{"type":"input_text","text":"two"}]}]`),
			wantAppendText: [][]string{{"one", "two"}},
		},
		{
			name:    "inject assistant message",
			method:  "thread/inject_items",
			params:  injectParams(`[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}]}]`),
			wantErr: wantInjectError,
		},
		{
			name:    "inject function call",
			method:  "thread/inject_items",
			params:  injectParams(`[{"type":"function_call","name":"x","arguments":"{}","call_id":"c1"}]`),
			wantErr: wantInjectError,
		},
		{
			name:    "inject image only message",
			method:  "thread/inject_items",
			params:  injectParams(`[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:,"}]}]`),
			wantErr: wantInjectError,
		},
		{
			name:    "inject message without content",
			method:  "thread/inject_items",
			params:  injectParams(`[{"type":"message","role":"developer","content":[]}]`),
			wantErr: wantInjectError,
		},
		{
			name:   "inject rejects the whole batch",
			method: "thread/inject_items",
			params: injectParams(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]},` +
				`{"type":"reasoning","summary":[]}]`),
			wantErr: wantInjectError,
		},
		{
			name:       "start without environments",
			method:     "thread/start",
			params:     `{"ephemeral":true,"environments":[]}`,
			wantErr:    noRuntimeURL,
			wantCreate: []runtimeclient.CreateConversationRequest{{AgentName: "assistant", Ephemeral: true, WithoutExecutors: true}},
		},
		{
			name:       "start ephemeral keeps executors",
			method:     "thread/start",
			params:     `{"ephemeral":true}`,
			wantErr:    noRuntimeURL,
			wantCreate: []runtimeclient.CreateConversationRequest{{AgentName: "assistant", Ephemeral: true}},
		},
		{
			name:       "start saved without environments",
			method:     "thread/start",
			params:     `{"environments":[]}`,
			wantErr:    noRuntimeURL,
			wantCreate: []runtimeclient.CreateConversationRequest{{AgentName: "assistant", WithoutExecutors: true}},
		},
		{
			name:       "start with environments keeps executors",
			method:     "thread/start",
			params:     `{"ephemeral":true,"environments":[{"id":"local"}]}`,
			wantErr:    noRuntimeURL,
			wantCreate: []runtimeclient.CreateConversationRequest{{AgentName: "assistant", Ephemeral: true}},
		},
	}
	// 2. 发送请求，核对返回、交给 serve 的创建、分叉请求和注入的客户端指令。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeSideConversationClient{fakeHistoryClient: newHistoryConversationClient(nil)}
			c := newHistoryConnection(client)
			c.isInitialized = true
			result, err := c.handleRequest(t.Context(), codexMessage{
				ID:     json.RawMessage(`1`),
				Method: tt.method,
				Params: json.RawMessage(tt.params),
			})
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("%s error = %v, want %q", tt.method, err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if result != (codexEmptyObjectResult{}) {
				t.Fatalf("%s result = %#v, want an empty object", tt.method, result)
			}
			if !reflect.DeepEqual(client.createRequests, tt.wantCreate) {
				t.Fatalf("create requests = %+v, want %+v", client.createRequests, tt.wantCreate)
			}
			if !reflect.DeepEqual(client.forkRequests, tt.wantFork) {
				t.Fatalf("fork requests = %+v, want %+v", client.forkRequests, tt.wantFork)
			}
			// 注入写进请求指定的 Thread 对话，开发者指令写进刚分叉出的对话。
			wantAppendConversationID := historyConversationID
			if tt.method == "thread/fork" {
				wantAppendConversationID = "forked"
			}
			var gotAppendText [][]string
			for _, request := range client.appendRequests {
				if request.ConversationID != wantAppendConversationID {
					t.Fatalf("append conversation = %q, want %q", request.ConversationID, wantAppendConversationID)
				}
				var texts []string
				for _, item := range request.Items {
					if item.ID == "" || item.Kind != defs.ItemClientInstruction || item.ClientInstruction == nil {
						t.Fatalf("appended item = %+v, want a client instruction with an ID", item)
					}
					texts = append(texts, item.ClientInstruction.Text)
				}
				gotAppendText = append(gotAppendText, texts)
			}
			if !reflect.DeepEqual(gotAppendText, tt.wantAppendText) {
				t.Fatalf("appended texts = %q, want %q", gotAppendText, tt.wantAppendText)
			}
		})
	}
}
