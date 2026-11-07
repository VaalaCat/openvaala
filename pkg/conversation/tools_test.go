package conversation

import (
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
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
		"spawn_agent": "将可独立执行的工作委派给一个直接子 Agent，子任务可以并行运行。" +
			"message 应提供目标、必要材料和预期交付物；默认不继承历史，需要时明确设置 fork_turns。" +
			"返回 session_id、task_name 和 accepted 表示首个任务已接收，不表示已经完成；子任务每个 Turn 结束后，结果以通知消息自动进入你的收件箱，在下一次模型请求前读入。" +
			"需要暂停等待时使用 wait_agent，它只在新消息到达时唤醒、不返回正文。" + "可选 agent_name（省略时与当前 Agent 相同）：\n- assistant：主 Agent\n- worker：只读调查\n可选 model（省略时同一 Agent 继承你当前的模型，不同 Agent 使用它的默认模型）：\n- test/other（）：窗口 64000，reasoning_effort 可选 low|high，默认 low\n- test/test（）：窗口 0\n- test/third（）：窗口 0，reasoning_effort 可选 high|medium，默认 medium\n- test/worker-model（）：窗口 0",
		"send_message": "向已知后代或直接父 Agent 发送任务、补充要求或信息。" +
			"normal 安排新 Turn，忙碌时排队；steer 补充活动 Turn，在下一次模型请求前读入，无可接收 Turn 时自动按 normal 安排。" +
			"进度汇报、提问和对当前任务的补充要求应显式设置 mode: \"steer\"；独立的后续任务用 normal。" +
			"返回 accepted 只表示已接收；steer 不会中断正在执行的模型或工具，需要中断时使用 interrupt_agent。" +
			"给后代发送时可以用 model、reasoning_effort 换它的模型，可选值见 spawn_agent 的 model 列表；不能改直接父方的模型。",
		"interrupt_agent": "请求取消指定后代当前正在执行的 Turn。" +
			"返回 cancel_requested 和选定的 turn_id，或无活动 Turn 时返回 no_active_turn；不等待取消完成。" +
			"排队输入和它的子任务继续运行；可通过 wait_agent 或 list_agents 查看后续结果与状态。",
		"list_agents": "使用空参数对象读取全部后代；sessions 中每项的 session 保存身份与父子关系，status 保存活动 Turn、排队数量和最近 Turn 状态，" +
			"usage 是已保存 Turn 的累计 token 用量，context_window 是该 Session 所选模型的上下文窗口。" +
			"使用 session.id 选择通信、等待或中断的目标；这是查询时的工作快照，不等待任务完成、不领取结果正文，也不启动历史任务。",
		"read_session_history": "需要子任务的详细执行过程时，读取指定后代在 fork 之后自身已保存的消息、工具调用与结果。" +
			"从最新内容向更早历史分页，每页 Items 按时间正序返回；使用非零 next_cursor 继续，为 0 时结束。" +
			"返回资料省略加密推理、provider RawJSON 和图片 base64；查询不启动执行，也不影响结果通知。",
		"wait_agent": "暂停当前工作，直到收到 steer 消息（Agent 消息或用户补充输入）、子任务结果通知或超时。normal 消息排队等待新 Turn，不会唤醒本次等待。" +
			"返回 reason：user_steer、subagent_result、agent_message 或 timeout（同时存在时按此顺序），from 为发送消息的 Agent SessionID；不返回正文。" +
			"消息正文会在下一次模型请求中读入；超时不会取消子任务，子任务每轮结束后的结果都会自动通知。",
	}
	for _, tt := range []struct {
		name   string
		fields []string
	}{
		{
			name:   "spawn_agent",
			fields: []string{"task_name", "message", "fork_turns", "agent_name", "model", "reasoning_effort"},
		},
		{
			name:   "send_message",
			fields: []string{"session_id", "message", "mode", "model", "reasoning_effort"},
		},
		{
			name:   "wait_agent",
			fields: []string{"timeout_seconds"},
		},
		{
			name:   "interrupt_agent",
			fields: []string{"session_id"},
		},
		{name: "list_agents"},
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
				} `json:"sessions"`
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
