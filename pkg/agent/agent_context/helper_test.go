package agent_context_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// TestNewTokenUsage 检查输入量包含缓存的计算方式和各项用量。
func TestNewTokenUsage(t *testing.T) {
	// 1. 逐项转换模型上报用量，比较输入、输出、缓存和推理数值。
	tests := []struct {
		name  string
		usage llmkit.Usage
		want  defs.TokenUsage
	}{
		{
			name: "openai_prompt_tokens_already_include_cache",
			usage: llmkit.Usage{
				PromptTokens:     100,
				CompletionTokens: 20,
				CachedTokens:     60,
				ReasoningTokens:  5,
			},
			want: defs.TokenUsage{
				Input:     100,
				Output:    20,
				CacheRead: 60,
				Reasoning: 5,
			},
		},
		{
			name: "claude_input_adds_cache_read_and_write",
			usage: llmkit.Usage{
				PromptTokens:     100,
				CompletionTokens: 20,
				CacheReadTokens:  60,
				CacheWriteTokens: 10,
			},
			want: defs.TokenUsage{
				Input:      170,
				Output:     20,
				CacheRead:  60,
				CacheWrite: 10,
			},
		},
		{
			name: "llama_cpp_timings_cache_is_disjoint",
			usage: llmkit.Usage{
				PromptTokens:    100,
				CacheReadTokens: 60,
			},
			want: defs.TokenUsage{
				Input:     160,
				CacheRead: 60,
			},
		},
		{
			name:  "zero_usage",
			usage: llmkit.Usage{},
			want:  defs.TokenUsage{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.NewTokenUsage(&tt.usage); got != tt.want {
				t.Fatalf("NewTokenUsage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestGetModelProviderName 检查已知、未知和空协议的遥测服务商名称。
func TestGetModelProviderName(t *testing.T) {
	// 1. 逐个协议读取遥测服务商名称，比较已知协议、未知协议和空协议结果。
	tests := []struct {
		name     string
		protocol llmkit.Protocol
		want     string
	}{
		{
			name:     "openai_chat",
			protocol: llmkit.ProtocolOpenAIChat,
			want:     "openai",
		},
		{
			name:     "openai_responses",
			protocol: llmkit.ProtocolOpenAIResponses,
			want:     "openai",
		},
		{
			name:     "claude",
			protocol: llmkit.ProtocolClaude,
			want:     "anthropic",
		},
		{
			name:     "gemini",
			protocol: llmkit.ProtocolGemini,
			want:     "gcp.gemini",
		},
		{
			name:     "unknown_keeps_protocol_name",
			protocol: llmkit.Protocol("custom"),
			want:     "custom",
		},
		{
			name:     "empty",
			protocol: "",
			want:     "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.GetModelProviderName(tt.protocol); got != tt.want {
				t.Fatalf("GetModelProviderName(%q) = %q, want %q", tt.protocol, got, tt.want)
			}
		})
	}
}

// TestNewTelemetryFinishReason 检查模型结束原因转换后的遥测取值。
func TestNewTelemetryFinishReason(t *testing.T) {
	// 1. 逐个转换结束原因，检查 tool_calls 改为 tool_call，其他取值不变。
	tests := []struct{ reason, want string }{
		{
			reason: "tool_calls",
			want:   "tool_call",
		},
		{
			reason: "stop",
			want:   "stop",
		},
		{
			reason: "length",
			want:   "length",
		},
		{
			reason: "",
			want:   "",
		},
		{
			reason: "end_turn",
			want:   "end_turn",
		},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			if got := agent_context.NewTelemetryFinishReason(tt.reason); got != tt.want {
				t.Fatalf("NewTelemetryFinishReason(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

// TestGetModelServerAddress 检查 URL 主机名、端口以及无效 URL 返回值。
func TestGetModelServerAddress(t *testing.T) {
	// 1. 解析各个服务 URL，比较主机、显式或默认端口及无效地址返回值。
	tests := []struct {
		name     string
		baseURL  string
		wantHost string
		wantPort int
	}{
		{
			name:     "https_default_port",
			baseURL:  "https://llm.example.com/v1",
			wantHost: "llm.example.com",
			wantPort: 443,
		},
		{
			name:     "http_explicit_port",
			baseURL:  "http://127.0.0.1:8080",
			wantHost: "127.0.0.1",
			wantPort: 8080,
		},
		{
			name:     "credentials_are_dropped",
			baseURL:  "https://user:secret@llm.example.com/v1?token=secret",
			wantHost: "llm.example.com",
			wantPort: 443,
		},
		{
			name:    "empty",
			baseURL: "",
		},
		{
			name:    "invalid",
			baseURL: "://bad",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port := agent_context.GetModelServerAddress(&llmkit.Target{BaseURL: tt.baseURL})
			if host != tt.wantHost || port != tt.wantPort {
				t.Fatalf("GetModelServerAddress(%q) = %q, %d; want %q, %d", tt.baseURL, host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestGetTokenModality 检查纯文本、图片和空请求在遥测里的 token 模态。
func TestGetTokenModality(t *testing.T) {
	// 1. 为文本、图片和空内容组装请求，检查返回 text 或 unknown。
	tests := []struct {
		name   string
		blocks []llmkit.ContentBlock
		want   string
	}{
		{
			name: "text_only",
			blocks: []llmkit.ContentBlock{{
				Type: llmkit.ContentTypeText,
				Text: "hi",
			}},
			want: telemetry.TokenModalityText,
		},
		{
			name: "image_makes_unknown",
			blocks: []llmkit.ContentBlock{{
				Type:     llmkit.ContentTypeImage,
				MediaURL: "https://a/b.png",
			}},
			want: telemetry.TokenModalityUnknown,
		},
		{
			name: "empty_request_is_text",
			want: telemetry.TokenModalityText,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &llmkit.Request{Messages: []llmkit.Message{{
				Role:    llmkit.RoleUser,
				Content: tt.blocks,
			}}}
			if got := agent_context.GetTokenModality(req); got != tt.want {
				t.Fatalf("GetTokenModality() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIsOutputChunk 检查内容、推理、工具及控制事件是否计为分块。
func TestIsOutputChunk(t *testing.T) {
	// 1. 分别判断内容、推理、工具、用量和结束事件，比较分块判断结果。
	tests := []struct {
		name      string
		eventType llmkit.EventType
		want      bool
	}{
		{
			name:      "content_delta",
			eventType: llmkit.EventContentDelta,
			want:      true,
		},
		{
			name:      "reasoning_content_delta",
			eventType: llmkit.EventReasoningContentDelta,
			want:      true,
		},
		{
			name:      "tool_call_arguments_delta",
			eventType: llmkit.EventToolCallArgumentsDelta,
			want:      true,
		},
		{
			name:      "tool_call_end",
			eventType: llmkit.EventToolCallEnd,
			want:      true,
		},
		{
			name:      "usage",
			eventType: llmkit.EventUsage,
			want:      false,
		},
		{
			name:      "done",
			eventType: llmkit.EventDone,
			want:      false,
		},
		{
			name:      "stream_start",
			eventType: llmkit.EventStreamStart,
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.IsOutputChunk(&llmkit.Event{Type: tt.eventType}); got != tt.want {
				t.Fatalf("IsOutputChunk(%v) = %v, want %v", tt.eventType, got, tt.want)
			}
		})
	}
}

// TestNewTelemetryChatInput 检查请求指令、图片、消息和工具的遥测内容。
func TestNewTelemetryChatInput(t *testing.T) {
	// 1. 准备 system、developer、普通消息、工具和图片的模型请求。
	text := func(role llmkit.Role, content string) llmkit.Message {
		return llmkit.Message{
			Role: role,
			Content: []llmkit.ContentBlock{{
				Type: llmkit.ContentTypeText,
				Text: content,
			}},
		}
	}
	tests := []struct {
		name         string
		req          *llmkit.Request
		wantSystem   []string
		wantMessages []telemetry.ChatMessage
		wantTools    []string
	}{
		{
			name: "system_user_assistant_and_tool_messages",
			req: &llmkit.Request{
				Messages: []llmkit.Message{
					text(llmkit.RoleSystem, "你是助手"),
					{
						Role: llmkit.RoleUser,
						Content: []llmkit.ContentBlock{
							{
								Type: llmkit.ContentTypeText,
								Text: "看图",
							},
							{
								Type:     llmkit.ContentTypeImage,
								MediaURL: "https://example.com/a.png",
							},
							{
								Type:     llmkit.ContentTypeImage,
								MediaURL: "data:image/png;base64,AAAA",
							},
						},
					},
					{
						Role: llmkit.RoleAssistant,
						Content: []llmkit.ContentBlock{{
							Type:      llmkit.ContentTypeThinking,
							Reasoning: &llmkit.ReasoningContent{Content: []string{"想一想"}},
						}},
						ToolCalls: []llmkit.ToolCall{{
							ID:        "call-1",
							Name:      "bash",
							Arguments: `{"command":"ls"}`,
						}},
					},
					{
						Role:       llmkit.RoleTool,
						ToolCallID: "call-1",
						Content: []llmkit.ContentBlock{{
							Type: llmkit.ContentTypeText,
							Text: `{"content":"a.txt"}`,
						}},
					},
				},
				Tools: []llmkit.Tool{{Name: "bash"}, {Name: "read_file"}},
			},
			wantSystem: []string{"你是助手"},
			wantMessages: []telemetry.ChatMessage{
				{
					Role: "user",
					Parts: []telemetry.MessagePart{
						{
							Type:    "text",
							Content: "看图",
						},
						{
							Type:     "uri",
							Modality: "image",
							URI:      "https://example.com/a.png",
						},
						{
							Type:     "blob",
							Modality: "image",
							MimeType: "image/png",
						},
					},
				},
				{
					Role: "assistant",
					Parts: []telemetry.MessagePart{
						{
							Type:    "reasoning",
							Content: "想一想",
						},
						{
							Type:      "tool_call",
							ID:        "call-1",
							Name:      "bash",
							Arguments: map[string]any{"command": "ls"},
						},
					},
				},
				{
					Role: "tool",
					Parts: []telemetry.MessagePart{{
						Type:     "tool_call_response",
						ID:       "call-1",
						Response: map[string]any{"content": "a.txt"},
					}},
				},
			},
			wantTools: []string{"bash", "read_file"},
		},
		{
			name: "developer_message_is_a_system_instruction",
			req: &llmkit.Request{Messages: []llmkit.Message{
				text(llmkit.RoleSystem, "系统指令"),
				text(llmkit.RoleDeveloper, "开发者指令"),
				text(llmkit.RoleUser, "问题"),
			}},
			wantSystem: []string{"系统指令", "开发者指令"},
			wantMessages: []telemetry.ChatMessage{{
				Role: "user",
				Parts: []telemetry.MessagePart{{
					Type:    "text",
					Content: "问题",
				}},
			}},
		},
		{
			name: "base64_only_image_is_a_blob_without_content",
			req: &llmkit.Request{Messages: []llmkit.Message{{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{
					{
						Type:     llmkit.ContentTypeImage,
						MediaB64: "AAAA",
						MimeType: "image/jpeg",
					},
				},
			}}},
			wantMessages: []telemetry.ChatMessage{{
				Role: "user",
				Parts: []telemetry.MessagePart{
					{
						Type:     "blob",
						Modality: "image",
						MimeType: "image/jpeg",
					},
				},
			}},
		},
	}
	// 2. 转换请求，比较系统指令、工具名和各消息的文本、图片、推理、调用内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			system, messages, toolNames := agent_context.NewTelemetryChatInput(tt.req)
			if !reflect.DeepEqual(system, tt.wantSystem) || !reflect.DeepEqual(toolNames, tt.wantTools) {
				t.Fatalf("system = %v, tools = %v; want %v, %v", system, toolNames, tt.wantSystem, tt.wantTools)
			}
			if !reflect.DeepEqual(messages, tt.wantMessages) {
				got, _ := json.Marshal(messages)
				t.Fatalf("messages = %s", got)
			}
		})
	}
}

// TestNewTelemetryChatOutput 检查模型输出合并为遥测助手消息。
func TestNewTelemetryChatOutput(t *testing.T) {
	// 1. 准备推理、文本、工具调用和结果的输出组合。
	text := &defs.Item{
		Kind:        defs.ItemMessage,
		ModelCallID: "m",
		Message: &defs.Message{Content: []*defs.Content{{
			Kind: defs.ContentText,
			Text: &defs.TextContent{Text: "答复"},
		}}},
	}
	reasoning := &defs.Item{
		Kind:        defs.ItemReasoning,
		ModelCallID: "m",
		Reasoning:   &defs.Reasoning{Summary: []string{"概要"}},
	}
	call := &defs.Item{
		Kind:        defs.ItemToolCall,
		ModelCallID: "m",
		ToolCall: &defs.ToolCall{
			CallID:    "call-1",
			Name:      "bash",
			Arguments: `not json`,
		},
	}
	result := &defs.Item{
		Kind:        defs.ItemToolResult,
		ModelCallID: "m",
		ToolResult:  &defs.ToolResult{CallID: "call-1"},
	}
	tests := []struct {
		name  string
		items []*defs.Item
		want  []telemetry.ChatMessage
	}{
		{
			name:  "one_assistant_message_in_order",
			items: []*defs.Item{reasoning, text, call, result},
			want: []telemetry.ChatMessage{{
				Role: "assistant",
				Parts: []telemetry.MessagePart{
					{
						Type:    "reasoning",
						Content: "概要",
					},
					{
						Type:    "text",
						Content: "答复",
					},
					{
						Type:      "tool_call",
						ID:        "call-1",
						Name:      "bash",
						Arguments: "not json",
					},
				},
			}},
		},
		{
			name:  "no_output",
			items: []*defs.Item{result},
		},
		{name: "empty"},
	}
	// 2. 转换输出，检查非助手内容被跳过，剩余内容按顺序组成一条助手消息。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.NewTelemetryChatOutput(tt.items); !reflect.DeepEqual(got, tt.want) {
				encoded, _ := json.Marshal(got)
				t.Fatalf("NewTelemetryChatOutput() = %s", encoded)
			}
		})
	}
}

// TestNewTelemetryChatMessages 检查输入 Item 转换为遥测消息及无效历史返回空结果。
func TestNewTelemetryChatMessages(t *testing.T) {
	// 1. 准备有效用户输入、无效工具关联和空输入。
	input := &defs.Item{
		Kind:   defs.ItemMessage,
		TurnID: contextTurnID,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: defs.IdentityUser,
				ID:   "user",
			},
			Content: []*defs.Content{{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: "问题"},
			}},
		},
	}
	tests := []struct {
		name  string
		items []*defs.Item
		want  []telemetry.ChatMessage
	}{
		{
			name:  "user_input",
			items: []*defs.Item{input},
			want: []telemetry.ChatMessage{{
				Role: "user",
				Parts: []telemetry.MessagePart{{
					Type:    "text",
					Content: "问题",
				}},
			}},
		},
		{
			name: "invalid_items_give_nil",
			items: []*defs.Item{{
				Kind:        defs.ItemToolResult,
				TurnID:      contextTurnID,
				ModelCallID: contextModelCallID,
				ToolResult:  &defs.ToolResult{CallID: "x"},
			}},
		},
		{name: "empty"},
	}
	// 2. 转换输入，比较用户消息和无效历史返回的空结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.NewTelemetryChatMessages(tt.items); !reflect.DeepEqual(got, tt.want) {
				encoded, _ := json.Marshal(got)
				t.Fatalf("NewTelemetryChatMessages() = %s", encoded)
			}
		})
	}
}

// TestGetUsageStreamOptions 检查各协议的用量上报选项及输入选项不被修改。
func TestGetUsageStreamOptions(t *testing.T) {
	// 1. 准备不同协议和已有流选项，记录调用前的选项副本。
	tests := []struct {
		name     string
		options  map[string]any
		protocol llmkit.Protocol
		want     map[string]any
	}{
		{
			name:     "openai_chat_without_options",
			protocol: llmkit.ProtocolOpenAIChat,
			want:     map[string]any{"include_usage": true},
		},
		{
			name:     "openai_chat_keeps_existing_keys",
			options:  map[string]any{"include_obfuscation": false},
			protocol: llmkit.ProtocolOpenAIChat,
			want:     map[string]any{"include_obfuscation": false, "include_usage": true},
		},
		{
			name:     "openai_chat_overrides_disabled_usage",
			options:  map[string]any{"include_usage": false},
			protocol: llmkit.ProtocolOpenAIChat,
			want:     map[string]any{"include_usage": true},
		},
		{
			name:     "openai_responses_unchanged",
			protocol: llmkit.ProtocolOpenAIResponses,
			want:     nil,
		},
		{
			name:     "claude_keeps_existing_options",
			options:  map[string]any{"a": 1},
			protocol: llmkit.ProtocolClaude,
			want:     map[string]any{"a": 1},
		},
		{
			name:     "unknown_unchanged",
			protocol: llmkit.Protocol("custom"),
			want:     nil,
		},
		{
			name:     "empty_protocol_unchanged",
			protocol: "",
			want:     nil,
		},
	}
	// 2. 读取流选项，比较 include_usage，并检查传入选项仍与副本相同。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := maps.Clone(tt.options)
			got := agent_context.GetUsageStreamOptions(tt.options, tt.protocol)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetUsageStreamOptions() = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.options, original) {
				t.Fatalf("input options were mutated: %v, want %v", tt.options, original)
			}
		})
	}
}

const contextHookSource = "/home/u/.config/openvaala/hooks/team.yaml"

// contextKindItems 创建消息、推理、工具、摘要和通知的分类测试样例。
func contextKindItems() map[string]*defs.Item {
	// 1. 创建不同发送方消息、推理状态和中断工具调用，再返回按类别命名的 Item。
	userMessage := contextMessage("alice", defs.IdentityUser, "hello")
	peerMessage := contextMessage("other-session", defs.IdentityAgent, "done")
	ownMessage := contextMessage(string(contextSessionID), defs.IdentityAgent, "reply")
	reasoning := func(status defs.ReasoningStatus) *defs.Item {
		return &defs.Item{
			TurnID:      contextTurnID,
			ModelCallID: contextModelCallID,
			Kind:        defs.ItemReasoning,
			Reasoning: &defs.Reasoning{
				Status:  status,
				Content: []string{"thinking"},
			},
		}
	}
	brokenCall := func(callID, name string) *defs.Item {
		item := contextToolCall(callID, defs.ToolCallInterrupted)
		item.ToolCall.Name = name
		return item
	}
	partialArgumentsCall := brokenCall("call-1", "read_file")
	partialArgumentsCall.ToolCall.Arguments = "{\"path\":"
	return map[string]*defs.Item{
		"summary":                contextSummary("S", "A", "earlier"),
		"user_message":           userMessage,
		"peer_message":           peerMessage,
		"own_message":            ownMessage,
		"reasoning":              reasoning(defs.ReasoningCompleted),
		"interrupted_reasoning":  reasoning(defs.ReasoningInterrupted),
		"tool_call":              contextToolCall("call-1", defs.ToolCallReady),
		"interrupted_tool_call":  brokenCall("call-1", "read_file"),
		"tool_call_without_name": brokenCall("call-1", ""),
		"tool_call_without_id":   brokenCall("", "read_file"),
		"interrupted_tool_call_with_partial_arguments": partialArgumentsCall,
		"tool_result": contextToolResultItem("call-1", contextModelCallID),
		"model_error": {
			TurnID:      contextTurnID,
			ModelCallID: contextModelCallID,
			Kind:        defs.ItemModelError,
			ModelError:  &defs.ModelError{Message: "boom"},
		},
		"model_change": {
			TurnID: contextTurnID,
			Kind:   defs.ItemModelChange,
			ModelChange: &defs.ModelChange{
				PreviousModel: defs.ModelRef{
					ProviderID: "p",
					ModelID:    "a",
				},
				Model: defs.ModelRef{
					ProviderID: "p",
					ModelID:    "b",
				},
			},
		},
		"executors_change": {
			TurnID: contextTurnID,
			Kind:   defs.ItemExecutorsChange,
			ExecutorsChange: &defs.ExecutorsChange{
				PreviousExecutors: []defs.TurnExecutor{{
					ID:                 "local",
					WorkspaceDirectory: "/a",
				}},
				Executors: []defs.TurnExecutor{{
					ID:                 "gpu",
					WorkspaceDirectory: "/data",
				}},
			},
		},
		"turn_interrupted": {
			TurnID: contextTurnID,
			Kind:   defs.ItemTurnInterrupted,
		},
		"client_instruction": {
			TurnID:            contextTurnID,
			Kind:              defs.ItemClientInstruction,
			ClientInstruction: &defs.ClientInstruction{Text: "boundary"},
		},
		"hook_run": contextHookRun(defs.HookRun{
			Event:             defs.HookEventSessionStart,
			AdditionalContext: "branch main",
		}),
		"hook_run_user_only": contextHookRun(defs.HookRun{
			Event:         defs.HookEventPreToolUse,
			CallID:        "call-1",
			BlockReason:   "no rm -rf",
			SystemMessage: "blocked",
		}),
	}
}

// contextHookRun 创建指定运行记录的 hook Item。
func contextHookRun(run defs.HookRun) *defs.Item {
	// 1. 填写固定来源文件和测试 Turn，再保存传入记录。
	run.SourcePath = contextHookSource
	return &defs.Item{
		TurnID:  contextTurnID,
		Kind:    defs.ItemHookRun,
		HookRun: &run,
	}
}

// TestItemsToLLMKitMessagesHookRun 检查 hook 反馈在模型历史中的内容和位置。
func TestItemsToLLMKitMessagesHookRun(t *testing.T) {
	// 1. 准备工具结果间、结束通知前和普通历史中的 hook 记录。
	nextModelOutput := contextMessage(string(contextSessionID), defs.IdentityAgent, "done")
	nextModelOutput.ModelCallID = "model-call-next"
	// wantMessage 指定消息角色、关联调用和正文片段；片段为空时不检查正文。
	type wantMessage struct {
		role       llmkit.Role
		toolCallID string
		contains   []string
	}
	hookText := []string{"openvaala-hook", contextHookSource}
	postToolUse := func(callID, reason string) *defs.Item {
		return contextHookRun(defs.HookRun{
			Event:       defs.HookEventPostToolUse,
			CallID:      callID,
			Status:      defs.HookRunBlocked,
			BlockReason: reason,
		})
	}
	tests := []struct {
		name  string
		items []*defs.Item
		want  []wantMessage
	}{
		{
			name: "session_start_additional_context",
			items: []*defs.Item{contextHookRun(defs.HookRun{
				Event:             defs.HookEventSessionStart,
				AdditionalContext: "branch main",
			})},
			want: []wantMessage{{
				role:     llmkit.RoleUser,
				contains: append([]string{"SessionStart", "branch main"}, hookText...),
			}},
		},
		{
			name: "call_hook_waits_for_all_results_of_the_run",
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady), contextToolCall("call-b", defs.ToolCallReady),
				contextToolResultItem("call-a", contextModelCallID),
				postToolUse("call-a", "lint failed"),
				contextToolResultItem("call-b", contextModelCallID),
			},
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-b",
				},
				{
					role:     llmkit.RoleUser,
					contains: append([]string{"PostToolUse hook 对这次工具结果的反馈：lint failed"}, hookText...),
				},
			},
		},
		{
			name: "call_hooks_keep_original_order",
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady), contextToolCall("call-b", defs.ToolCallReady),
				contextToolResultItem("call-a", contextModelCallID),
				postToolUse("call-a", "first"),
				contextToolResultItem("call-b", contextModelCallID),
				postToolUse("call-b", "second"),
			},
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-b",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"first"},
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"second"},
				},
			},
		},
		{
			name: "call_hook_is_sent_before_next_message",
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady),
				contextToolResultItem("call-a", contextModelCallID),
				postToolUse("call-a", "lint failed"),
				contextMessage("alice", defs.IdentityUser, "next"),
			},
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"lint failed"},
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"next"},
				},
			},
		},
		{
			name: "call_hook_is_sent_before_next_model_output",
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady),
				contextToolResultItem("call-a", contextModelCallID),
				postToolUse("call-a", "lint failed"),
				nextModelOutput,
			},
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"lint failed"},
				},
				{role: llmkit.RoleAssistant},
			},
		},
		{
			name: "call_hook_is_sent_before_turn_interrupted",
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady),
				contextToolResultItem("call-a", contextModelCallID),
				postToolUse("call-a", "lint failed"),
				{
					TurnID: contextTurnID,
					Kind:   defs.ItemTurnInterrupted,
				},
			},
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"lint failed"},
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"上一轮已中断"},
				},
			},
		},
		{
			name: "call_hook_without_result_follows_completed_result",
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady),
				postToolUse("call-a", "lint failed"),
			},
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"lint failed"},
				},
			},
		},
		{
			name: "stop_block_reason_keeps_position",
			items: []*defs.Item{
				contextMessage("alice", defs.IdentityUser, "go"),
				contextMessage(string(contextSessionID), defs.IdentityAgent, "done"),
				contextHookRun(defs.HookRun{
					Event:       defs.HookEventStop,
					Status:      defs.HookRunBlocked,
					BlockReason: "run tests",
				}),
			},
			want: []wantMessage{
				{
					role:     llmkit.RoleUser,
					contains: []string{"go"},
				},
				{role: llmkit.RoleAssistant},
				{
					role:     llmkit.RoleUser,
					contains: append([]string{"Stop hook 要求继续：run tests"}, hookText...),
				},
			},
		},
		{
			name: "subagent_stop_block_reason",
			items: []*defs.Item{contextHookRun(defs.HookRun{
				Event:       defs.HookEventSubagentStop,
				Status:      defs.HookRunBlocked,
				BlockReason: "run tests",
			})},
			want: []wantMessage{{
				role:     llmkit.RoleUser,
				contains: []string{"SubagentStop hook 要求继续：run tests"},
			}},
		},
		{
			name: "system_message_is_user_only",
			items: []*defs.Item{contextHookRun(defs.HookRun{
				Event:         defs.HookEventSessionStart,
				SystemMessage: "hooks loaded",
			})},
		},
		{
			name: "pre_tool_use_block_reason_is_user_only",
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady),
				contextToolResultItem("call-a", contextModelCallID),
				contextHookRun(defs.HookRun{
					Event:       defs.HookEventPreToolUse,
					CallID:      "call-a",
					Status:      defs.HookRunBlocked,
					BlockReason: "no rm -rf",
				}),
			},
			want: []wantMessage{{role: llmkit.RoleAssistant}, {
				role:       llmkit.RoleTool,
				toolCallID: "call-a",
			}},
		},
		{
			name: "failed_run_error_message_is_user_only",
			items: []*defs.Item{contextHookRun(defs.HookRun{
				Event:        defs.HookEventPostToolUse,
				CallID:       "call-a",
				Status:       defs.HookRunFailed,
				ErrorMessage: "timed out",
			})},
		},
	}
	// 2. 加载历史并组装模型请求，比较角色、CallID、反馈正文和连续工具结果后的排列。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newAgentContext(t)
			if err := c.Load(&agent_context.ContextLoadParam{Items: tt.items}); err != nil {
				t.Fatal(err)
			}
			_, messages := buildContextView(t, c)
			if len(messages) != len(tt.want) {
				t.Fatalf("messages = %+v, want %d messages", messages, len(tt.want))
			}
			for index, want := range tt.want {
				got := messages[index]
				if got.Role != want.role || got.ToolCallID != want.toolCallID {
					t.Fatalf("message %d = %s/%q, want %s/%q", index, got.Role, got.ToolCallID, want.role, want.toolCallID)
				}
				var text strings.Builder
				for _, block := range got.Content {
					text.WriteString(block.Text)
				}
				for _, fragment := range want.contains {
					if !strings.Contains(text.String(), fragment) {
						t.Fatalf("message %d text = %q, want it to contain %q", index, text.String(), fragment)
					}
				}
			}
		})
	}
}

// TestItemsToLLMKitMessagesClientInstruction 检查客户端指令以 user 角色发给模型，缺少载荷时加载报错。
func TestItemsToLLMKitMessagesClientInstruction(t *testing.T) {
	// 1. 准备带正文的指令和缺载荷的指令。
	tests := []struct {
		name     string
		item     *defs.Item
		wantText string
		wantErr  bool
	}{
		{
			name:     "with_text",
			item:     &defs.Item{TurnID: contextTurnID, Kind: defs.ItemClientInstruction, ClientInstruction: &defs.ClientInstruction{Text: "boundary"}},
			wantText: "客户端指令\n\nboundary",
		},
		{
			name:    "without_payload",
			item:    &defs.Item{TurnID: contextTurnID, Kind: defs.ItemClientInstruction},
			wantErr: true,
		},
	}
	// 2. 加载并组装模型请求，比较角色和完整正文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newAgentContext(t)
			err := c.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{tt.item}})
			if tt.wantErr {
				if err == nil {
					t.Fatal("Load() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			req, _, err := c.BuildModelRequest(nil)
			if err != nil || len(req.Messages) != 1 {
				t.Fatalf("BuildModelRequest() = %+v, %v", req, err)
			}
			got := req.Messages[0]
			if got.Role != llmkit.RoleUser || len(got.Content) != 1 || got.Content[0].Text != tt.wantText {
				t.Fatalf("message = %+v, want user %q", got, tt.wantText)
			}
		})
	}
}

// TestItemsToLLMKitMessagesToolResultImages 检查工具图片的排列、占位和无效图片错误。
func TestItemsToLLMKitMessagesToolResultImages(t *testing.T) {
	// 1. 准备并行工具图片、不能看图、超过图片上限和无效图片的历史。
	// wantMessage 指定消息角色、关联调用、正文片段和图片块数。
	type wantMessage struct {
		role       llmkit.Role
		toolCallID string
		contains   []string
		images     int
	}
	const imageIntro = "以下是工具调用 call-a 读到的图片：a.png"
	const unsupported = "[图片已省略：当前模型不能接收图片]"
	runWithImage := func() []*defs.Item {
		return []*defs.Item{
			contextToolCall("call-a", defs.ToolCallReady), contextToolCall("call-b", defs.ToolCallReady),
			contextToolResultWithImage("call-a", "a.png"),
			contextToolResultItem("call-b", contextModelCallID),
			contextHookRun(defs.HookRun{
				Event:             defs.HookEventPostToolUse,
				CallID:            "call-a",
				AdditionalContext: "lint ok",
			}),
		}
	}
	var latestImages []*defs.Item
	var latestWant []wantMessage
	for index := range 21 {
		alt := "shot-" + strconv.Itoa(index) + ".png"
		latestImages = append(latestImages, contextUserImageMessage("look", alt))
		latestWant = append(latestWant, wantMessage{
			role:   llmkit.RoleUser,
			images: 1,
		})
	}
	latestWant[0] = wantMessage{
		role:     llmkit.RoleUser,
		contains: []string{"[较早的图片已省略：一个请求最多带最近 20 张图片；需要时重新读取 shot-0.png]"},
	}
	brokenImage := func(image *defs.ImageContent) []*defs.Item {
		result := contextToolResultItem("call-a", contextModelCallID)
		result.ToolResult.Images = []*defs.ImageContent{image}
		return []*defs.Item{contextToolCall("call-a", defs.ToolCallReady), result}
	}

	tests := []struct {
		name                  string
		isImageInputSupported bool
		items                 []*defs.Item
		want                  []wantMessage
		// wantErr 是追加 Item 后组装请求应返回的错误片段。
		wantErr string
	}{
		{
			name:                  "image_waits_for_all_results_of_the_run",
			isImageInputSupported: true,
			items:                 runWithImage(),
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-b",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{imageIntro},
					images:   1,
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"lint ok"},
				},
			},
		},
		{
			name:                  "image_result_then_turn_interrupted",
			isImageInputSupported: true,
			items: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady),
				contextToolResultWithImage("call-a", "a.png"),
				{
					TurnID: contextTurnID,
					Kind:   defs.ItemTurnInterrupted,
				},
			},
			want: []wantMessage{
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{imageIntro},
					images:   1,
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"上一轮已中断"},
				},
			},
		},
		{
			name:  "unsupported_model_gets_placeholder",
			items: append([]*defs.Item{contextUserImageMessage("look", "shot.png")}, runWithImage()...),
			want: []wantMessage{
				{
					role:     llmkit.RoleUser,
					contains: []string{"look", unsupported},
				},
				{role: llmkit.RoleAssistant},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-a",
				},
				{
					role:       llmkit.RoleTool,
					toolCallID: "call-b",
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{imageIntro, unsupported},
				},
				{
					role:     llmkit.RoleUser,
					contains: []string{"lint ok"},
				},
			},
		},
		{
			name:                  "only_latest_twenty_images",
			isImageInputSupported: true,
			items:                 latestImages,
			want:                  latestWant,
		},
		{
			name:                  "image_without_data_url_is_conversion_error",
			isImageInputSupported: true,
			items: brokenImage(&defs.ImageContent{
				DataURL: "",
				Alt:     "a.png",
			}),
			wantErr: "convert context item 1",
		},
		{
			name:                  "nil_image_is_conversion_error",
			isImageInputSupported: true,
			items:                 brokenImage(nil),
			wantErr:               "convert context item 1",
		},
	}
	// 2. 按图片能力组装请求，比较消息顺序、图片数量、占位文字和转换错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestModel(&llmkit.Target{}, 0)
			if !tt.isImageInputSupported {
				model = newTextOnlyTestModel(&llmkit.Target{}, 0)
			}
			c, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" {
				// 追加无效图片，再检查组装请求返回转换错误。
				if err := c.AppendItems(tt.items...); err != nil {
					t.Fatal(err)
				}
				if _, _, err := c.BuildModelRequest(nil); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("BuildModelRequest() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err := c.Load(&agent_context.ContextLoadParam{Items: tt.items}); err != nil {
				t.Fatal(err)
			}
			_, messages := buildContextView(t, c)
			if len(messages) != len(tt.want) {
				t.Fatalf("messages = %+v, want %d messages", messages, len(tt.want))
			}
			for index, want := range tt.want {
				got := messages[index]
				if got.Role != want.role || got.ToolCallID != want.toolCallID {
					t.Fatalf("message %d = %s/%q, want %s/%q", index, got.Role, got.ToolCallID, want.role, want.toolCallID)
				}
				var text strings.Builder
				images := 0
				for _, block := range got.Content {
					text.WriteString(block.Text)
					if block.Type == llmkit.ContentTypeImage {
						images++
					}
				}
				for _, fragment := range want.contains {
					if !strings.Contains(text.String(), fragment) {
						t.Fatalf("message %d text = %q, want it to contain %q", index, text.String(), fragment)
					}
				}
				if images != want.images {
					t.Fatalf("message %d has %d image blocks, want %d", index, images, want.images)
				}
			}
		})
	}
}

// TestIsSentToModel 检查各类历史及中断输出是否发送给模型。
func TestIsSentToModel(t *testing.T) {
	// 1. 逐项判断正常、中断、缺少调用参数和空内容 hook 的模型可见状态。
	items := contextKindItems()
	tests := []struct {
		name string
		want bool
	}{
		{
			"summary",
			true,
		},
		{
			"user_message",
			true,
		},
		{
			"peer_message",
			true,
		},
		{
			"own_message",
			true,
		},
		{
			"reasoning",
			true,
		},
		{
			"interrupted_reasoning",
			false,
		},
		{
			"tool_call",
			true,
		},
		{
			"interrupted_tool_call",
			true,
		},
		{
			"tool_call_without_name",
			false,
		},
		{
			"tool_call_without_id",
			false,
		},
		{
			"interrupted_tool_call_with_partial_arguments",
			false,
		},
		{
			"tool_result",
			true,
		},
		{
			"model_error",
			true,
		},
		{
			"model_change",
			true,
		},
		{
			"executors_change",
			true,
		},
		{
			"turn_interrupted",
			true,
		},
		{
			"client_instruction",
			true,
		},
		{
			"hook_run",
			true,
		},
		{
			"hook_run_user_only",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.IsSentToModel(items[tt.name]); got != tt.want {
				t.Fatalf("IsSentToModel() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetContextCategory 检查各类历史 Item 的上下文类别。
func TestGetContextCategory(t *testing.T) {
	// 1. 逐项读取消息、摘要、推理、工具、变化通知和 hook 的上下文类别。
	items := contextKindItems()
	tests := []struct {
		name string
		want agent_context.ContextCategory
	}{
		{
			"summary",
			agent_context.ContextCategorySummary,
		},
		{
			"user_message",
			agent_context.ContextCategoryUserInput,
		},
		{
			"peer_message",
			agent_context.ContextCategoryAgentMessage,
		},
		{
			"own_message",
			agent_context.ContextCategoryAssistantText,
		},
		{
			"reasoning",
			agent_context.ContextCategoryReasoning,
		},
		{
			"tool_call",
			agent_context.ContextCategoryToolCall,
		},
		{
			"tool_result",
			agent_context.ContextCategoryToolResult,
		},
		{
			"model_error",
			agent_context.ContextCategoryModelError,
		},
		{
			"model_change",
			agent_context.ContextCategoryModelChange,
		},
		{
			"executors_change",
			agent_context.ContextCategoryExecutorsChange,
		},
		{
			"turn_interrupted",
			agent_context.ContextCategoryTurnInterrupted,
		},
		{
			"client_instruction",
			agent_context.ContextCategoryInstruction,
		},
		{
			"hook_run",
			agent_context.ContextCategoryInstruction,
		},
		{
			"hook_run_user_only",
			agent_context.ContextCategoryInstruction,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.GetContextCategory(items[tt.name]); got != tt.want {
				t.Fatalf("GetContextCategory() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGetThresholdTokens 检查窗口和最大输出共同决定的自动压缩阈值。
func TestGetThresholdTokens(t *testing.T) {
	// 1. 分别计算未知窗口、无效输出上限和需要预留输出空间的阈值。
	for _, tt := range []struct {
		name                    string
		window, maxOutput, want int
	}{
		{
			name:      "unknown_window",
			maxOutput: 128000,
		},
		{
			name:   "unknown_output",
			window: 1000,
			want:   800,
		},
		{
			name:      "negative_output",
			window:    1000,
			maxOutput: -1,
			want:      800,
		},
		{
			name:      "output_equals_window",
			window:    1000,
			maxOutput: 1000,
			want:      800,
		},
		{
			name:      "output_exceeds_window",
			window:    1000,
			maxOutput: 2000,
			want:      800,
		},
		{
			name:   "large_window",
			window: 256000,
			want:   204800,
		},
		{
			name:      "reserve_128k",
			window:    600000,
			maxOutput: 128000,
			want:      472000,
		},
		{
			name:      "reserve_32k",
			window:    600000,
			maxOutput: 32000,
			want:      480000,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent_context.GetThresholdTokens(tt.window, tt.maxOutput); got != tt.want {
				t.Fatalf("GetThresholdTokens(%d, %d) = %d, want %d", tt.window, tt.maxOutput, got, tt.want)
			}
		})
	}
}
