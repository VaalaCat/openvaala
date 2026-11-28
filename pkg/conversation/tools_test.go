package conversation

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/session"
)

// TestCollaborationToolDefinitions 检查模型收到的协作工具说明和参数，每个参数都有说明。
func TestCollaborationToolDefinitions(t *testing.T) {
	// 1. 启动对话并取得模型请求中的工具定义。
	store := openConversationDAO(t)
	model := newConversationModel(t)
	h := startTestConversation(t, store, model, createTestConversation(t, store, model))
	id := h.input(t, session.SessionInputModeNormal, "delegate a review")
	request := receiveModelRequest(t, model)
	definitions := make(map[string]map[string]any)
	for _, value := range request.body["tools"].([]any) {
		definition := value.(map[string]any)["function"].(map[string]any)
		definitions[definition["name"].(string)] = definition
	}
	// 2. 逐个工具核对完整说明、字段、字段说明和默认值。
	wantDescriptions := map[string]string{
		"list_executors": prompt.ListExecutorsDescription,
		"spawn_agent": "将可独立执行的工作委派给一个直接子 Agent，子任务可以并行运行。" +
			"message 应提供目标、必要材料和预期交付物；默认不继承历史，需要时明确设置 fork_turns。" +
			"返回 session_id 和 input_item_id 表示首个任务已接收。默认在每轮结束时回报，notify_on_finish=false 关闭自动回报。" +
			"需要等待子任务结果时使用 wait_agent，可读取最终正文。" + "可选 agent_name（省略时与当前 Agent 相同）：\n- assistant：主 Agent\n- worker：只读调查\n可选 model（省略时同一 Agent 继承你当前的模型，不同 Agent 使用它的默认模型）：\n- test/other（）：窗口 64000，reasoning_effort 可选 low|high，默认 low\n- test/test（）：窗口 0\n- test/third（）：窗口 0，reasoning_effort 可选 high|medium，默认 medium\n- test/worker-model（）：窗口 0",
		"send_message": "向已知后代或直接父 Agent 发送任务、补充要求或信息；重开对话后，发到原 SessionID 会先续跑其未完成轮次。" +
			"normal 安排新 Turn，忙碌时排队；steer 补充活动 Turn，在下一次模型请求前读入，无可接收 Turn 时自动按 normal 安排。" +
			"进度汇报、提问和对当前任务的补充要求应显式设置 mode: \"steer\"；独立的后续任务用 normal。" +
			"返回 accepted 只表示已接收；steer 不会中断正在执行的模型或工具，需要中断时使用 interrupt_agent。" +
			"给后代发送时可以用 model、reasoning_effort 换它的模型，可选值见 spawn_agent 的 model 列表；不能改直接父方的模型。",
		"interrupt_agent": "请求取消指定后代当前正在执行的 Turn。" +
			"返回 cancel_requested 和选定的 turn_id，或无活动 Turn 时返回 no_active_turn；不等待取消完成。" +
			"排队输入和它的子任务继续运行；可通过 wait_agent 或 list_agents 查看后续结果与状态。",
		"list_agents": "默认查询当前工作目录最近 24 小时内更新的非后台后代，最多 20 条。agents 中每项的 session 保存身份与父子关系，status 保存活动 Turn、排队数量和最近 Turn 状态，" +
			"usage 是已保存 Turn 的累计 token 用量，context_window 是该 Session 所选模型的上下文窗口。" +
			"使用 session.id 选择通信、等待或中断的目标；这是查询时的工作快照，不等待任务完成、不领取结果正文，也不启动历史任务。",
		"read_session_history": "需要子任务的详细执行过程时，读取指定后代在 fork 之后自身已保存的消息、工具调用与结果。" +
			"从最新内容向更早历史分页，每页 Items 按时间正序返回；使用非零 next_cursor 继续，为 0 时结束。" +
			"返回资料省略加密推理、provider RawJSON 和图片 base64；查询不启动执行，也不影响结果通知。",
		"wait_agent": "暂停当前工作，直到收到 steer 消息（Agent 消息或用户补充输入）、子任务结果通知或超时。排入下一轮的 Agent 回报也会唤醒等待。" +
			"返回 reason：user_steer、subagent_result、agent_message 或 timeout（同时存在时按此顺序），from 为发送消息的 Agent SessionID，text 为子任务最终正文。" +
			"可选 wait_for: \"subagent_result\" 优先返回已有未读补充；没有未读补充且没有运行中的后代时立即返回 no_running_subagents，可用 list_agents 查看状态。" +
			"消息正文会在下一次模型请求中读入；省略参数默认等待 30 秒，超时不会取消子任务。",
	}
	for _, tt := range []struct {
		name   string
		fields []string
	}{
		{
			name:   "spawn_agent",
			fields: []string{"task_name", "message", "fork_turns", "agent_name", "model", "reasoning_effort", "notify_on_finish", "initial_options", "labels", "background", "cwd", "executor_id"},
		},
		{
			name:   "send_message",
			fields: []string{"session_id", "message", "mode", "model", "reasoning_effort"},
		},
		{
			name:   "wait_agent",
			fields: []string{"timeout_seconds", "wait_for"},
		},
		{
			name:   "interrupt_agent",
			fields: []string{"session_id"},
		},
		{name: "list_agents", fields: []string{"session_id", "statuses", "cwd", "since_hours", "labels", "all", "limit"}},
		{name: "list_executors", fields: []string{"refresh_tools"}},
		{
			name:   "read_session_history",
			fields: []string{"session_id", "cursor", "limit"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			definition, found := definitions[tt.name]
			if !found {
				t.Fatal("tool not exposed to model")
			}
			if description, _ := definition["description"].(string); description != wantDescriptions[tt.name] {
				t.Fatalf("description = %q, want %q", description, wantDescriptions[tt.name])
			}
			parameters := definition["parameters"].(map[string]any)
			properties, _ := parameters["properties"].(map[string]any)
			var fields []string
			for field, value := range properties {
				fields = append(fields, field)
				if description, _ := value.(map[string]any)["description"].(string); strings.TrimSpace(description) == "" {
					t.Errorf("parameter %s has no explanation in model schema", field)
				}
			}
			slices.Sort(fields)
			want := slices.Sorted(slices.Values(tt.fields))
			if !slices.Equal(fields, want) {
				t.Fatalf("fields=%v, want %v", fields, want)
			}
			if tt.name == "list_executors" {
				description := properties["refresh_tools"].(map[string]any)["description"].(string)
				if !strings.Contains(description, "前缀缓存失效") || !strings.Contains(description, "不要高频调用") {
					t.Fatalf("refresh_tools description = %q", description)
				}
			}
			if tt.name == "spawn_agent" && properties["fork_turns"].(map[string]any)["default"] != "none" {
				t.Fatal("model schema does not default fork_turns to none")
			}
			if tt.name == "spawn_agent" {
				description := definition["description"].(string)
				for _, want := range []string{"- assistant：主 Agent", "- worker：只读调查", "- test/other", "窗口 64000，reasoning_effort 可选 low|high，默认 low"} {
					if !strings.Contains(description, want) {
						t.Fatalf("spawn_agent description missing %q: %s", want, description)
					}
				}
			}
		})
	}
	// 3. 回复模型并等待本轮结束。
	request.reply <- modelResponse{text: "done"}
	if finished, _ := h.waitTurnFinished(t, id); finished.Err != nil {
		t.Fatal(finished.Err)
	}
}

// TestCollaborationToolParameters 检查协作工具参数不合法时返回 invalid_arguments，协作操作失败时返回 collaboration_error。
func TestCollaborationToolParameters(t *testing.T) {
	// 1. 列出参数缺失、取值无效、多余字段和有效调用。
	for _, tt := range []struct {
		name, tool, args string
		wantCode         string
	}{
		{
			name:     "missing_task_name",
			tool:     "spawn_agent",
			args:     `{"message":"task"}`,
			wantCode: "invalid_arguments",
		},
		{
			name:     "blank_task_name",
			tool:     "spawn_agent",
			args:     `{"task_name":" ","message":"task"}`,
			wantCode: "collaboration_error",
		},
		{
			name:     "bad_fork",
			tool:     "spawn_agent",
			args:     `{"task_name":"task","message":"task","fork_turns":"0"}`,
			wantCode: "collaboration_error",
		},
		// 未声明字段被忽略，来源身份取自工具绑定的 Session。
		{
			name: "spoof_source_ignored",
			tool: "spawn_agent",
			args: `{"task_name":"task","message":"task","source_session_id":"other"}`,
		},
		{
			name:     "explicit_zero_limit",
			tool:     "read_session_history",
			args:     `{"session_id":"unknown","limit":0}`,
			wantCode: "invalid_arguments",
		},
		{
			name:     "history_limit_too_large",
			tool:     "read_session_history",
			args:     `{"session_id":"unknown","limit":101}`,
			wantCode: "invalid_arguments",
		},
		{
			name:     "history_unknown_target",
			tool:     "read_session_history",
			args:     `{"session_id":"unknown"}`,
			wantCode: "collaboration_error",
		},
		{
			name: "list_empty",
			tool: "list_agents",
			args: `{}`,
		},
		{
			name:     "bad_mode",
			tool:     "send_message",
			args:     `{"session_id":"unknown","message":"task","mode":"bad"}`,
			wantCode: "invalid_arguments",
		},
		{
			name:     "send_unknown_target",
			tool:     "send_message",
			args:     `{"session_id":"unknown","message":"task"}`,
			wantCode: "collaboration_error",
		},
		{
			name:     "interrupt_unknown_target",
			tool:     "interrupt_agent",
			args:     `{"session_id":"unknown"}`,
			wantCode: "collaboration_error",
		},
		{name: "invalid_wait_for", tool: "wait_agent", args: `{"wait_for":"steer"}`, wantCode: "invalid_arguments"},
		{
			name:     "wait_negative",
			tool:     "wait_agent",
			args:     `{"timeout_seconds":-1}`,
			wantCode: "invalid_arguments",
		},
		{
			name:     "wait_non_integer",
			tool:     "wait_agent",
			args:     `{"timeout_seconds":1.5}`,
			wantCode: "invalid_arguments",
		},
		{
			name:     "wait_too_large",
			tool:     "wait_agent",
			args:     `{"timeout_seconds":601}`,
			wantCode: "invalid_arguments",
		},
		// session_ids 被忽略，只按 timeout_seconds 等待。
		{
			name: "wait_old_targets_ignored",
			tool: "wait_agent",
			args: `{"session_ids":["target"],"timeout_seconds":1}`,
		},
		{
			name: "wait_one_second",
			tool: "wait_agent",
			args: `{"timeout_seconds":1}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 让模型发出工具调用，检查返回的错误码，再结束本轮。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			h := startTestConversation(t, store, model, createTestConversation(t, store, model))
			id := h.input(t, session.SessionInputModeNormal, "tools")
			receiveModelRequest(t, model).reply <- modelResponse{calls: []llmkit.ToolCall{{
				ID:        "tool",
				Name:      tt.tool,
				Arguments: tt.args,
			}}}
			next := receiveModelRequest(t, model)
			result := requestToolResult(t, next, "tool")
			gotCode := ""
			if result.Error != nil {
				gotCode = result.Error.Code
			}
			if gotCode != tt.wantCode {
				t.Fatalf("result=%+v, want code %q", result, tt.wantCode)
			}
			next.reply <- modelResponse{text: "done"}
			if finished, _ := h.waitTurnFinished(t, id); finished.Err != nil {
				t.Fatal(finished.Err)
			}
		})
	}
}

// TestListAgentsToolReportsUsageAndContextWindow 检查 list_agents 返回后代已保存 Turn 的用量之和与所选模型窗口，模型没有窗口时也带上 0。
func TestListAgentsToolReportsUsageAndContextWindow(t *testing.T) {
	// 1. 准备已保存的用量，以及模型有窗口、没窗口的 Agent。
	usage := defs.TokenUsage{
		Input:      100,
		Output:     20,
		CacheRead:  60,
		CacheWrite: 10,
		Reasoning:  5,
	}
	tests := []struct {
		name       string
		agentName  string
		wantWindow int
	}{
		{
			name:       "agent_with_context_window",
			agentName:  "assistant",
			wantWindow: 8000,
		},
		{
			name:      "agent_without_context_window",
			agentName: "worker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 保存两轮用量并打开对话，通过模型调用 list_agents。
			store := openConversationDAO(t)
			model := newConversationModel(t)
			opt := conversationOpt(store, model)
			opt.Definitions.Models[0].ContextWindow = 8000 // worker-model 的窗口为 0。
			created, err := CreateConversation(t.Context(), defs.NewConversationID(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			id := saveUsageAgent(t, store, created.GetID(), created.GetRootSessionID(), tt.agentName, []defs.TokenUsage{usage, usage})
			c, err := reopenTestConversation(t, created, opt)
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)

			turnID := h.input(t, session.SessionInputModeNormal, "list")
			receiveModelRequest(t, model).reply <- modelResponse{calls: []llmkit.ToolCall{{
				ID:        "list",
				Name:      "list_agents",
				Arguments: `{}`,
			}}}
			next := receiveModelRequest(t, model)
			var result struct {
				Sessions []struct {
					Session       defs.Session     `json:"session"`
					Usage         *defs.TokenUsage `json:"usage"`
					ContextWindow *int             `json:"context_window"`
				} `json:"agents"`
			}
			decodeRequestToolResult(t, next, "list", &result)
			next.reply <- modelResponse{text: "done"}
			if finished, _ := h.waitTurnFinished(t, turnID); finished.Err != nil {
				t.Fatal(finished.Err)
			}

			// 3. 检查目标 Session、用量之和和窗口字段。
			if len(result.Sessions) != 1 || result.Sessions[0].Session.ID != id {
				t.Fatalf("sessions = %+v", result.Sessions)
			}
			got := result.Sessions[0]
			if got.Usage == nil || got.ContextWindow == nil {
				t.Fatalf("list_agents session lacks usage or context_window keys: %+v", got)
			}
			if *got.Usage != usage.Add(usage) || *got.ContextWindow != tt.wantWindow {
				t.Fatalf("usage = %+v, context_window = %d, want %+v, %d", *got.Usage, *got.ContextWindow, usage.Add(usage), tt.wantWindow)
			}
		})
	}
}

// TestListExecutorsTool 检查在线摘要、Agent 白名单和主动刷新后的工具调用。
func TestListExecutorsTool(t *testing.T) {
	for _, tt := range []struct {
		name         string
		args         string
		serverIDs    defs.OptionalStringList
		wantListed   bool
		wantCallable bool
	}{
		{name: "query_keeps_opening_tools", args: `{}`, wantListed: true},
		{name: "refresh_exposes_new_tools", args: `{"refresh_tools":true}`, wantListed: true, wantCallable: true},
		{name: "matching_whitelist_excludes_dynamic_tools", args: `{"refresh_tools":true}`, serverIDs: defs.OptionalStringList{"desk"}},
		{name: "empty_whitelist_excludes_dynamic_tools", args: `{"refresh_tools":true}`, serverIDs: defs.OptionalStringList{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openConversationDAO(t), newConversationModel(t)
			opt := conversationOpt(store, model)
			opt.Definitions.Executors = newTestExecutorProvider(newTestExecutors("local"))
			opt.Definitions.Agents[0].MCPServerIDs = tt.serverIDs
			opt.Definitions.Agents[0].ToolNames = []string{"read_file"}
			c, err := CreateConversation(t.Context(), defs.NewConversationID(), "assistant", defs.ModelSelection{}, defs.ExecutorSettings{}, defs.ExecutorSettings{}, opt)
			if err != nil {
				t.Fatal(err)
			}
			h := startTestConversation(t, store, model, c)
			id := h.input(t, session.SessionInputModeNormal, "list executors")
			opening := receiveModelRequest(t, model)
			if err := c.SetClientExecutor(t.Context(), ClientExecutorRegistration{
				ConnectionID: "ws-1",
				ExecutorID:   "workstation",
				Description:  "Work laptop",
				Servers:      []ClientMCPServer{{ID: "desk", Transport: pingMCPTransport{}}},
			}); err != nil {
				t.Fatal(err)
			}
			waitMCPServerConnected(t, c, "workstation", "desk")
			opening.reply <- modelResponse{calls: []llmkit.ToolCall{{ID: "list", Name: "list_executors", Arguments: tt.args}}}
			next := receiveModelRequest(t, model)

			var result struct {
				DefaultID string           `json:"default_executor_id"`
				Executors []map[string]any `json:"executors"`
			}
			decodeRequestToolResult(t, next, "list", &result)
			if result.DefaultID != "local" || len(result.Executors) != 2 {
				t.Fatalf("list_executors result = %+v", result)
			}
			stable, dynamic := result.Executors[0], result.Executors[1]
			if stable["id"] != "local" || stable["is_dynamic"] != false || stable["workspace_directory"] != "/workspace/local" ||
				!reflect.DeepEqual(stable["tools"], []any{"read_file"}) {
				t.Fatalf("stable executor = %+v", stable)
			}
			if dynamic["id"] != "workstation" || dynamic["description"] != "Work laptop" || dynamic["kind"] != "client" ||
				dynamic["is_dynamic"] != true || dynamic["workspace_directory"] != nil {
				t.Fatalf("dynamic executor = %+v", dynamic)
			}
			wantMCP := []any{map[string]any{"server_id": "desk", "status": "connected"}}
			if !reflect.DeepEqual(dynamic["mcp_servers"], wantMCP) {
				t.Fatalf("mcp_servers = %+v, want %+v", dynamic["mcp_servers"], wantMCP)
			}
			const name = "mcp__desk__ping"
			wantTools := []any{}
			if tt.wantListed {
				wantTools = append(wantTools, name)
			}
			if !reflect.DeepEqual(dynamic["tools"], wantTools) {
				t.Fatalf("listed tools = %+v, want %+v", dynamic["tools"], wantTools)
			}
			if slices.Contains(requestToolNames(next), name) != tt.wantCallable {
				t.Fatalf("next request tools = %+v, want new tool: %t", requestToolNames(next), tt.wantCallable)
			}
			if tt.wantCallable {
				next.reply <- modelResponse{calls: []llmkit.ToolCall{{ID: "ping", Name: name, Arguments: `{}`}}}
				next = receiveModelRequest(t, model)
				got := requestToolResult(t, next, "ping")
				if got.Error != nil || !strings.Contains(string(got.Content), "pong") {
					t.Fatalf("refreshed tool result = %+v", got)
				}
			} else if !reflect.DeepEqual(next.body["tools"], opening.body["tools"]) {
				t.Fatal("tools changed without a newly visible tool")
			}
			next.reply <- modelResponse{text: "done"}
			if finished, _ := h.waitTurnFinished(t, id); finished.Err != nil {
				t.Fatal(finished.Err)
			}
		})
	}
}

// TestClientExecutorToolRouting 检查同名工具按设备路由，以及同一轮内的闪断与重连。
func TestClientExecutorToolRouting(t *testing.T) {
	for _, tt := range []struct {
		name              string
		removeBeforeRetry bool
	}{
		{name: "replacement_ignores_late_unregister"},
		{name: "disconnected_tool_recovers_in_same_turn", removeBeforeRetry: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, model := openConversationDAO(t), newConversationModel(t)
			c := createTestConversation(t, store, model)
			h := startTestConversation(t, store, model, c)
			register := func(executorID, connectionID, response string) {
				t.Helper()
				if err := c.SetClientExecutor(t.Context(), ClientExecutorRegistration{
					ConnectionID: connectionID,
					ExecutorID:   executorID,
					Servers:      []ClientMCPServer{{ID: "desk", Transport: pingMCPTransport{response: response}}},
				}); err != nil {
					t.Fatal(err)
				}
				waitMCPServerConnected(t, c, executorID, "desk")
			}
			unregisterWork := func() {
				t.Helper()
				if err := c.SetClientExecutor(t.Context(), ClientExecutorRegistration{ConnectionID: "ws-old", ExecutorID: "work"}); err != nil {
					t.Fatal(err)
				}
			}
			register("home", "ws-home", "home-pong")
			register("work", "ws-old", "old-work-pong")
			id := h.input(t, session.SessionInputModeNormal, "ping devices")
			request := receiveModelRequest(t, model)
			openingTools := request.body["tools"]
			wantExecutors := make(map[string]string)
			call := func(callID, args, wantExecutor, wantText, wantError string) {
				t.Helper()
				request.reply <- modelResponse{calls: []llmkit.ToolCall{{ID: callID, Name: "mcp__desk__ping", Arguments: args}}}
				request = receiveModelRequest(t, model)
				result := requestToolResult(t, request, callID)
				if wantError != "" {
					if result.Error == nil || result.Error.Code != wantError {
						t.Fatalf("%s result = %+v, want %q", callID, result, wantError)
					}
				} else {
					if result.Error != nil || !strings.Contains(string(result.Content), wantText) {
						t.Fatalf("%s result = %+v, want %s from %s", callID, result, wantText, wantExecutor)
					}
					wantExecutors[callID] = wantExecutor
				}
				if !reflect.DeepEqual(request.body["tools"], openingTools) {
					t.Fatalf("model tools changed after %s", callID)
				}
			}
			call("unspecified", `{}`, "", "", "invalid_arguments")
			call("call-home", `{"executor":"home"}`, "home", "home-pong", "")
			call("call-old", `{"executor":"work"}`, "work", "old-work-pong", "")
			if tt.removeBeforeRetry {
				unregisterWork()
				call("call-offline", `{"executor":"work"}`, "", "", "mcp_server_unavailable")
			}
			register("work", "ws-new", "new-work-pong")
			unregisterWork()
			call("call-reconnected", `{"executor":"work"}`, "work", "new-work-pong", "")
			request.reply <- modelResponse{text: "done"}
			if finished, _ := h.waitTurnFinished(t, id); finished.Err != nil {
				t.Fatal(finished.Err)
			}
			gotExecutors := make(map[string]string)
			for _, item := range listSessionItems(t, store, c.GetID(), c.GetRootSessionID()) {
				if item.ToolResult != nil && item.ToolResult.Error == nil {
					gotExecutors[item.ToolResult.CallID] = item.ToolResult.Executor
				}
			}
			if !reflect.DeepEqual(gotExecutors, wantExecutors) {
				t.Fatalf("saved tool executors = %+v, want %+v", gotExecutors, wantExecutors)
			}
		})
	}
}
