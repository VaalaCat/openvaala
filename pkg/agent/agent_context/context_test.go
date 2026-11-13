package agent_context_test

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/tiktoken-go/tokenizer"
)

const contextSessionID defs.SessionID = "c4b86503-4a60-416d-9300-34b1a7203457"
const contextTurnID defs.TurnID = "turn-context"
const contextModelCallID defs.ModelCallID = "model-call-context"

// TestNewAgentContextValidation 检查缺少依赖和负数窗口被拒绝。
func TestNewAgentContextValidation(t *testing.T) {
	// 1. 逐项构造无效配置并检查创建失败。
	model := newTestModel(&llmkit.Target{}, 0)
	for _, tt := range []struct {
		name string
		opt  *agent_context.AgentContextOpt
	}{
		{
			"negative window",
			&agent_context.AgentContextOpt{
				SessionID: contextSessionID,
				TurnID:    contextTurnID,
				Client:    &compactClient{},
				Model: &defs.ModelDefinition{
					Target:        &llmkit.Target{},
					ContextWindow: -1,
				},
				CompactModel: model,
			},
		},
		{
			"missing session",
			&agent_context.AgentContextOpt{
				TurnID:       contextTurnID,
				Client:       &compactClient{},
				Model:        model,
				CompactModel: model,
			},
		},
		{
			"missing turn",
			&agent_context.AgentContextOpt{
				SessionID:    contextSessionID,
				Client:       &compactClient{},
				Model:        model,
				CompactModel: model,
			},
		},
		{
			"missing client",
			&agent_context.AgentContextOpt{
				SessionID:    contextSessionID,
				TurnID:       contextTurnID,
				Model:        model,
				CompactModel: model,
			},
		},
		{
			"missing model",
			&agent_context.AgentContextOpt{
				SessionID:    contextSessionID,
				TurnID:       contextTurnID,
				Client:       &compactClient{},
				CompactModel: model,
			},
		},
		{
			"missing compact model",
			&agent_context.AgentContextOpt{
				SessionID: contextSessionID,
				TurnID:    contextTurnID,
				Client:    &compactClient{},
				Model:     model,
			},
		},
		{
			"compact model without target",
			&agent_context.AgentContextOpt{
				SessionID:    contextSessionID,
				TurnID:       contextTurnID,
				Client:       &compactClient{},
				Model:        model,
				CompactModel: &defs.ModelDefinition{},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := agent_context.NewAgentContext(tt.opt); err == nil {
				t.Fatalf("invalid option accepted: %+v", tt.opt)
			}
		})
	}
}

// TestAgentContextMessages 检查历史转换后的角色、模型调用和消息顺序。
func TestAgentContextMessages(t *testing.T) {
	// 1. 准备用户、Agent、推理及并行工具调用的历史和预期消息。
	tests := []struct {
		name    string
		history []*defs.Item
		append  []*defs.Item
		want    []*llmkit.Message
	}{
		{
			name: "history_and_new_input_use_agent_perspective",
			history: []*defs.Item{
				contextMessage("alice", defs.IdentityUser, "Review this file"),
				contextMessage("c4b86503-4a60-416d-9300-34b1a7203457", defs.IdentityAgent, "I will check it"),
			},
			append: []*defs.Item{
				contextMessage("899b74ef-1a90-4ab9-8c1f-03a702c580c9", defs.IdentityAgent, "Check the error handling"),
			},
			want: []*llmkit.Message{
				{
					Role: llmkit.RoleUser,
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeText,
							Text: "Review this file",
						},
					},
				},
				{
					Role: llmkit.RoleAssistant,
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeText,
							Text: "I will check it",
						},
					},
				},
				{
					Role: llmkit.RoleUser,
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeText,
							Text: "Check the error handling",
						},
					},
				},
			},
		},
		{
			name: "reasoning_call_and_result_preserve_order_and_identity",
			history: []*defs.Item{
				{
					TurnID:      contextTurnID,
					ModelCallID: contextModelCallID,
					Kind:        defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:    defs.ReasoningCompleted,
						Summary:   []string{"Read the file first"},
						Encrypted: "opaque",
					},
				},
				{
					TurnID:      contextTurnID,
					ModelCallID: contextModelCallID,
					Kind:        defs.ItemToolCall,
					ToolCall: &defs.ToolCall{
						CallID:    "call-read",
						Name:      "read_file",
						Arguments: `{"path":"main.go"}`,
						Status:    defs.ToolCallReady,
					},
				},
			},
			append: []*defs.Item{
				{
					TurnID:      contextTurnID,
					ModelCallID: contextModelCallID,
					Kind:        defs.ItemToolResult,
					ToolResult: &defs.ToolResult{
						CallID:  "call-read",
						Content: json.RawMessage(`"package main"`),
					},
				},
				{
					TurnID:      contextTurnID,
					ModelCallID: defs.ModelCallID("model-call-error"),
					Kind:        defs.ItemModelError,
					ModelError: &defs.ModelError{
						Code:    "overloaded",
						Message: "try later",
					},
				},
			},
			want: []*llmkit.Message{
				{
					Role: llmkit.RoleAssistant,
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeThinking,
							Reasoning: &llmkit.ReasoningContent{
								Summary:   []string{"Read the file first"},
								Encrypted: "opaque",
							},
						},
					},
					ToolCalls: []llmkit.ToolCall{
						{
							ID:        "call-read",
							Name:      "read_file",
							Arguments: `{"path":"main.go"}`,
						},
					},
				},
				{
					Role:       llmkit.RoleTool,
					ToolCallID: "call-read",
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeText,
							Text: `{"content":"package main"}`,
						},
					},
				},
				{
					Role: llmkit.RoleUser,
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeText,
							// 模型错误通知放在 openvaala-system-info 中。
							Text: "<openvaala-system-info>\n<openvaala-model-error-event>\n上一轮模型请求未完成。\n错误代码：overloaded\n错误信息：try later\n</openvaala-model-error-event>\n</openvaala-system-info>",
						},
					},
				},
			},
		},
		{
			name: "message_reasoning_and_parallel_calls_share_assistant_message",
			history: []*defs.Item{
				contextMessage(string(contextSessionID), defs.IdentityAgent, "I will inspect both files."),
				{
					TurnID:      contextTurnID,
					ModelCallID: contextModelCallID,
					Kind:        defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:  defs.ReasoningCompleted,
						Summary: []string{"Inspect both"},
					},
				},
				contextToolCallWithArguments("call-a", `{"path":"a.go"}`),
				contextToolCallWithArguments("call-b", `{"path":"b.go"}`),
			},
			want: []*llmkit.Message{
				{
					Role: llmkit.RoleAssistant,
					Content: []llmkit.ContentBlock{
						{
							Type: llmkit.ContentTypeText,
							Text: "I will inspect both files.",
						},
						{
							Type:      llmkit.ContentTypeThinking,
							Reasoning: &llmkit.ReasoningContent{Summary: []string{"Inspect both"}},
						},
					},
					ToolCalls: []llmkit.ToolCall{
						{
							ID:        "call-a",
							Name:      "read_file",
							Arguments: `{"path":"a.go"}`,
						},
						{
							ID:        "call-b",
							Name:      "read_file",
							Arguments: `{"path":"b.go"}`,
						},
					},
				},
				contextMissingToolResultMessage(t, "call-a"),
				contextMissingToolResultMessage(t, "call-b"),
			},
		},
	}
	// 2. 加载并追加历史，比较系统指令、消息角色、推理、工具调用及结果顺序。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				conversation := newAgentContext(t)
				if err := conversation.Load(&agent_context.ContextLoadParam{
					System: "You are a code reviewer.",
					Items:  tt.history,
				}); err != nil {
					t.Fatal(err)
				}
				if err := conversation.AppendItems(tt.append...); err != nil {
					t.Fatal(err)
				}
				system, got := buildContextView(t, conversation)
				if system != "You are a code reviewer." {
					t.Fatalf("system = %q", system)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("messages = %#v, want %#v", got, tt.want)
				}
				req, _, err := conversation.BuildModelRequest(nil)
				if err != nil {
					t.Fatal(err)
				}
				measurement := getContextMeasurement(t, conversation, nil)
				if measurement.EstimatedTokens != requestEstimatedTokens(t, req) {
					t.Fatalf("measurement = %+v, want actual merged request tokens %d", measurement, requestEstimatedTokens(t, req))
				}
			})
	}
}

// TestAgentContextRepairsExecutionHistory 检查缺失工具结果的补齐和中断输出的处理。
func TestAgentContextRepairsExecutionHistory(t *testing.T) {
	// 1. 准备已有错误结果，以及缺失结果、乱序结果和中断输出的样例。
	existingErrorResult := &defs.ToolResult{
		CallID:  "call-existing",
		Content: json.RawMessage(`null`),
		Error: &defs.ToolError{
			Code:    "permission_denied",
			Message: "Access denied",
		},
	}
	tests := []struct {
		name    string
		history []*defs.Item
		want    []*llmkit.Message
	}{
		{
			name: "missing_tool_result_is_inserted_before_next_user_message",
			history: []*defs.Item{
				contextToolCall("call-read", defs.ToolCallReady),
				contextMessage("alice", defs.IdentityUser, "请重试。"),
			},
			want: []*llmkit.Message{
				contextToolCallMessage("call-read"),
				contextMissingToolResultMessage(t, "call-read"),
				contextUserMessage("请重试。"),
			},
		},
		{
			name: "multiple_missing_results_keep_call_order",
			history: []*defs.Item{
				contextToolCall("call-first", defs.ToolCallReady),
				contextToolCall("call-second", defs.ToolCallReady),
			},
			want: []*llmkit.Message{
				contextToolCallsMessage("call-first", "call-second"),
				contextMissingToolResultMessage(t, "call-first"),
				contextMissingToolResultMessage(t, "call-second"),
			},
		},
		{
			name: "missing_result_precedes_next_model_call",
			history: func() []*defs.Item {
				first := contextToolCall("call-first", defs.ToolCallReady)
				second := contextToolCall("call-second", defs.ToolCallReady)
				second.ModelCallID = "model-call-second"
				return []*defs.Item{first, second}
			}(),
			want: []*llmkit.Message{
				contextToolCallMessage("call-first"),
				contextMissingToolResultMessage(t, "call-first"),
				contextToolCallMessage("call-second"),
				contextMissingToolResultMessage(t, "call-second"),
			},
		},
		{
			name: "existing_tool_error_is_preserved",
			history: []*defs.Item{
				contextToolCall("call-existing", defs.ToolCallReady),
				{
					TurnID:      contextTurnID,
					ModelCallID: contextModelCallID,
					Kind:        defs.ItemToolResult,
					ToolResult:  existingErrorResult,
				},
			},
			want: []*llmkit.Message{
				contextToolCallMessage("call-existing"),
				contextToolResultMessage(t, existingErrorResult),
			},
		},
		{
			name: "interrupted_reasoning_is_hidden",
			history: []*defs.Item{
				{
					TurnID:      contextTurnID,
					ModelCallID: contextModelCallID,
					Kind:        defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:  defs.ReasoningInterrupted,
						Content: []string{"unfinished"},
					},
				},
				contextMessage("alice", defs.IdentityUser, "继续。"),
			},
			want: []*llmkit.Message{contextUserMessage("继续。")},
		},
		{
			name: "interrupted_tool_call_is_closed",
			history: []*defs.Item{
				contextToolCall("call-interrupted", defs.ToolCallInterrupted),
			},
			want: []*llmkit.Message{
				contextToolCallMessage("call-interrupted"),
				contextMissingToolResultMessage(t, "call-interrupted"),
			},
		},
		{
			name: "reverse_tool_results_keep_completion_order",
			history: []*defs.Item{
				contextToolCall("call-a", defs.ToolCallReady),
				contextToolCall("call-b", defs.ToolCallReady),
				contextToolResultItem("call-b", contextModelCallID),
				contextToolResultItem("call-a", contextModelCallID),
			},
			want: []*llmkit.Message{
				contextToolCallsMessage("call-a", "call-b"),
				contextToolResultMessage(t, contextToolResultItem("call-b", contextModelCallID).ToolResult),
				contextToolResultMessage(t, contextToolResultItem("call-a", contextModelCallID).ToolResult),
			},
		},
	}
	// 2. 加载各类执行历史，比较补齐结果及中断推理处理，并检查原 Item 不变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := make([]*defs.Item, len(tt.history))
			for index, item := range tt.history {
				before[index] = item.Clone()
			}
			conversation := newAgentContext(t)
			if err := conversation.Load(&agent_context.ContextLoadParam{Items: tt.history}); err != nil {
				t.Fatal(err)
			}
			_, got := buildContextView(t, conversation)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("messages = %#v, want %#v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.history, before) {
				t.Fatalf("BuildModelRequest changed source items: %#v", tt.history)
			}
		})
	}
}

// TestAgentContextLoadReplacesContent 检查整体加载替换指令和历史，失败时保留原内容。
func TestAgentContextLoadReplacesContent(t *testing.T) {
	// 1. 准备替换、清空和无效历史的加载样例。
	tests := []struct {
		name       string
		load       *agent_context.ContextLoadParam
		wantSystem string
		wantText   string
		wantErr    bool
	}{
		{
			name: "replace_system_and_history",
			load: &agent_context.ContextLoadParam{
				System: "New instructions",
				Items: []*defs.Item{
					contextMessage("alice", defs.IdentityUser, "New history"),
				},
			},
			wantSystem: "New instructions",
			wantText:   "New history",
		},
		{
			name: "empty_context_clears_previous_content",
			load: &agent_context.ContextLoadParam{},
		},
		{
			name: "invalid_history_preserves_previous_context",
			load: &agent_context.ContextLoadParam{
				System: "New instructions",
				Items: []*defs.Item{
					contextMessage("alice", defs.IdentityUser, "Valid prefix"),
					{Kind: defs.ItemMessage},
				},
			},
			wantSystem: "Original instructions",
			wantText:   "Original history",
			wantErr:    true,
		},
	}
	// 2. 先加载原历史再加载样例，比较替换或清空结果，失败时仍保留原内容。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				conversation := newAgentContext(t)
				if err := conversation.Load(&agent_context.ContextLoadParam{
					System: "Original instructions",
					Items: []*defs.Item{
						contextMessage("alice", defs.IdentityUser, "Original history"),
					},
				}); err != nil {
					t.Fatal(err)
				}
				if err := conversation.Load(tt.load); (err != nil) != tt.wantErr {
					t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
				}
				system, messages := buildContextView(t, conversation)
				if system != tt.wantSystem {
					t.Fatalf("system = %q, want %q", system, tt.wantSystem)
				}
				if tt.wantText == "" {
					if len(messages) != 0 {
						t.Fatalf("messages = %#v, want empty", messages)
					}
					return
				}
				if len(messages) != 1 || len(messages[0].Content) != 1 || messages[0].Content[0].Text != tt.wantText {
					t.Fatalf("messages = %#v, want text %q", messages, tt.wantText)
				}
			})
	}
}

// TestAgentContextAppendItems 检查追加内容和追加失败后的历史。
func TestAgentContextAppendItems(t *testing.T) {
	// 1. 准备有效批次、空批次和无效 Item、摘要样例。
	summaryWithoutBoundary := contextSummary("S", "", "summary")
	tests := []struct {
		name    string
		append  []*defs.Item
		wantErr string
	}{
		{
			name:   "valid_items_follow_existing_history",
			append: []*defs.Item{contextMessage("alice", defs.IdentityUser, "New input"), contextMessage(string(contextSessionID), defs.IdentityAgent, "New answer")},
		},
		{
			name:   "empty_batch_keeps_history",
			append: nil,
		},
		{
			name:    "nil_item",
			append:  []*defs.Item{contextMessage("alice", defs.IdentityUser, "Do not partially append"), nil},
			wantErr: "is nil",
		},
		{
			name: "missing_turn_id",
			append: []*defs.Item{contextMessage("alice", defs.IdentityUser, "Do not partially append"), func() *defs.Item {
				item := contextMessage("alice", defs.IdentityUser, "missing turn")
				item.TurnID = ""
				return item
			}()},
			wantErr: "has no turn ID",
		},
		{
			name: "agent_output_without_model_call_id",
			append: []*defs.Item{contextMessage("alice", defs.IdentityUser, "Do not partially append"), func() *defs.Item {
				item := contextToolCall("call-missing-model", defs.ToolCallReady)
				item.ModelCallID = ""
				return item
			}()},
			wantErr: "has no model call ID",
		},
		{
			name:    "malformed_summary",
			append:  []*defs.Item{contextMessage("alice", defs.IdentityUser, "Do not partially append"), summaryWithoutBoundary},
			wantErr: "context summary requires",
		},
		{
			name:    "summary_boundary_outside_history",
			append:  []*defs.Item{contextMessage("alice", defs.IdentityUser, "Do not partially append"), contextSummary("S", "missing", "summary")},
			wantErr: "missing boundary",
		},
	}
	// 2. 加载已有历史后追加样例，比较新历史，失败时检查整批未写入。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := []*defs.Item{contextMessage("alice", defs.IdentityUser, "Keep")}
			conversation := newAgentContext(t)
			if err := conversation.Load(&agent_context.ContextLoadParam{Items: existing}); err != nil {
				t.Fatal(err)
			}
			_, before := buildContextView(t, conversation)
			err := conversation.AppendItems(tt.append...)
			_, after := buildContextView(t, conversation)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("AppendItems() error = %v, want %q", err, tt.wantErr)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("failed append changed history: %#v", after)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before)+len(tt.append) || !reflect.DeepEqual(after[:len(before)], before) {
				t.Fatalf("history after append = %#v, want existing %#v followed by %d items", after, before, len(tt.append))
			}
			for index, item := range tt.append {
				if got := after[len(before)+index].Content[0].Text; got != item.Message.Content[0].Text.Text {
					t.Fatalf("appended message %d = %q", index, got)
				}
			}
		})
	}
}

// TestAgentContextRejectsInvalidItemIdentity 检查历史身份和工具调用关联错误。
func TestAgentContextRejectsInvalidItemIdentity(t *testing.T) {
	// 1. 准备缺少身份、重新出现的模型调用和不匹配工具结果。
	missingTurn := contextMessage("alice", defs.IdentityUser, "missing turn")
	missingTurn.TurnID = ""
	missingModelCall := contextToolCall("call-missing-model", defs.ToolCallReady)
	missingModelCall.ModelCallID = ""
	mismatchedResult := contextToolResultItem("call-mismatch", defs.ModelCallID("model-call-other"))
	lateFirstCall := contextToolCall("call-late", defs.ToolCallReady)
	nextCall := contextToolCall("call-next", defs.ToolCallReady)
	nextCall.ModelCallID = "model-call-next"

	tests := []struct {
		name    string
		invalid []*defs.Item
		wantErr string
	}{
		{
			name:    "missing_turn_id",
			invalid: []*defs.Item{missingTurn},
			wantErr: "has no turn ID",
		},
		{
			name:    "missing_model_call_id",
			invalid: []*defs.Item{missingModelCall},
			wantErr: "has no model call ID",
		},
		{
			name: "reopened_model_call",
			invalid: []*defs.Item{
				contextMessage(string(contextSessionID), defs.IdentityAgent, "first output"),
				contextMessage("alice", defs.IdentityUser, "continue"),
				contextToolCall("call-reopened", defs.ToolCallReady),
			},
			wantErr: "reopens model call",
		},
		{
			name: "tool_result_model_call_mismatch",
			invalid: []*defs.Item{
				contextToolCall("call-mismatch", defs.ToolCallReady),
				mismatchedResult,
			},
			wantErr: "belongs to model call",
		},
		{
			name: "tool_result_after_next_model_call",
			invalid: []*defs.Item{
				lateFirstCall,
				nextCall,
				contextToolResultItem("call-late", contextModelCallID),
			},
			wantErr: "has no pending tool call",
		},
		{
			name: "missing_message",
			invalid: []*defs.Item{{
				TurnID: contextTurnID,
				Kind:   defs.ItemMessage,
			}},
			wantErr: "has no message",
		},
		{
			name:    "unknown_sender_kind",
			invalid: []*defs.Item{contextMessage("alice", "unknown", "Invalid sender")},
			wantErr: "unsupported conversation sender kind",
		},
		{
			name: "invalid_tool_result_json",
			invalid: []*defs.Item{
				contextToolCall("call-read", defs.ToolCallReady),
				{
					TurnID:      contextTurnID,
					ModelCallID: contextModelCallID,
					Kind:        defs.ItemToolResult,
					ToolResult: &defs.ToolResult{
						CallID:  "call-read",
						Content: json.RawMessage(`invalid`),
					},
				},
			},
			wantErr: "convert context item 1",
		},
	}
	// 2. 尝试加载无效历史，比较错误片段和原指令、历史均未改变。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversation := newAgentContext(t)
			if err := conversation.Load(&agent_context.ContextLoadParam{
				System: "keep",
				Items:  []*defs.Item{contextMessage("alice", defs.IdentityUser, "keep")},
			}); err != nil {
				t.Fatal(err)
			}
			beforeSystem, before := buildContextView(t, conversation)
			err := conversation.Load(&agent_context.ContextLoadParam{
				System: "replace",
				Items:  tt.invalid,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			}
			afterSystem, after := buildContextView(t, conversation)
			if afterSystem != beforeSystem || !reflect.DeepEqual(after, before) {
				t.Fatalf("failed Load changed context: %q %#v", afterSystem, after)
			}
		})
	}
}

// TestAgentContextBuildModelRequest 检查指令、历史、工具和缺失结果组成的模型请求。
func TestAgentContextBuildModelRequest(t *testing.T) {
	// 1. 准备有指令、工具、缺失结果和空历史的请求样例。
	tools := []llmkit.Tool{{
		Name:        "read_file",
		Description: "读取文件",
		InputSchema: map[string]any{"type": "object"},
	}}
	tests := []struct {
		name   string
		system string
		items  []*defs.Item
		tools  []llmkit.Tool
		want   []llmkit.Message
	}{
		{
			name:   "system_history_and_tools",
			system: "You are a code reviewer.",
			items:  []*defs.Item{contextMessage("alice", defs.IdentityUser, "Review")},
			tools:  tools,
			want: []llmkit.Message{
				{
					Role: llmkit.RoleSystem,
					Content: []llmkit.ContentBlock{{
						Type: llmkit.ContentTypeText,
						Text: "You are a code reviewer.",
					}},
				},
				*contextUserMessage("Review"),
			},
		},
		{
			name:  "missing_tool_result_is_completed",
			items: []*defs.Item{contextMessage("alice", defs.IdentityUser, "Read"), contextToolCall("call-read", defs.ToolCallReady)},
			want: []llmkit.Message{
				*contextUserMessage("Read"),
				*contextToolCallMessage("call-read"),
				*contextMissingToolResultMessage(t, "call-read"),
			},
		},
		{
			name: "invisible_tail_is_input_boundary",
			items: []*defs.Item{contextSource("visible", "Review"), {
				ID:          "hidden-tail",
				TurnID:      contextTurnID,
				ModelCallID: contextModelCallID,
				Kind:        defs.ItemReasoning,
				Reasoning:   &defs.Reasoning{Status: defs.ReasoningInterrupted},
			}},
			want: []llmkit.Message{*contextUserMessage("Review")},
		},
		{
			name: "empty_context",
			want: []llmkit.Message{},
		},
	}
	// 2. 加载指令和历史，组装请求并比较流参数、工具、缓存键和消息列表。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversation := newAgentContext(t)
			if err := conversation.Load(&agent_context.ContextLoadParam{
				System: tt.system,
				Items:  tt.items,
			}); err != nil {
				t.Fatal(err)
			}
			req, inputThroughItemID, err := conversation.BuildModelRequest(tt.tools)
			if err != nil {
				t.Fatal(err)
			}
			var wantInputThrough defs.ItemID
			if len(tt.items) > 0 {
				wantInputThrough = tt.items[len(tt.items)-1].ID
			}
			if inputThroughItemID != wantInputThrough {
				t.Fatalf("input end = %q, want %q", inputThroughItemID, wantInputThrough)
			}
			want := &llmkit.Request{
				Stream:          true,
				ThinkingEnabled: new(true),
				Tools:           tt.tools,
				Messages:        tt.want,
				Extras:          map[string]any{"prompt_cache_key": string(contextSessionID)},
			}
			if !reflect.DeepEqual(req, want) {
				t.Fatalf("request = %#v, want %#v", req, want)
			}
		})
	}
}

// TestBuildModelRequestEncodesToolResultImages 检查不同协议中工具结果、图片和 hook 消息的排列。
func TestBuildModelRequestEncodesToolResultImages(t *testing.T) {
	// 1. 准备历史：助手读取 a.png 和 b.go，a.png 的结果带图片，它的 PostToolUse 返回补充上下文。
	history := []*defs.Item{
		contextMessage("alice", defs.IdentityUser, "看一下 a.png 和 b.go"),
		contextToolCallWithArguments("call-1", `{"path":"a.png"}`), contextToolCallWithArguments("call-2", `{"path":"b.go"}`),
		contextToolResultWithImage("call-1", "a.png"),
		contextToolResultItem("call-2", contextModelCallID),
		contextHookRun(defs.HookRun{
			Event:             defs.HookEventPostToolUse,
			CallID:            "call-1",
			AdditionalContext: "lint ok",
		}),
	}
	const unsupported = "当前模型不能接收图片"
	tests := []struct {
		name                  string
		protocol              llmkit.Protocol
		isImageInputSupported bool
		// entriesKey 指定消息数组字段。
		entriesKey string
		// toolResult 是工具结果消息必须包含的片段。
		toolResult string
		// wantAfterResults 列出工具结果后第一条消息必须包含的片段。
		wantAfterResults []string
	}{
		{
			name:                  "openai_chat",
			protocol:              llmkit.ProtocolOpenAIChat,
			isImageInputSupported: true,
			entriesKey:            "messages",
			toolResult:            `"role":"tool"`,
			wantAfterResults:      []string{`"role":"user"`, `"image_url"`, `data:image/png;base64,`},
		},
		{
			name:             "openai_chat_text_only",
			protocol:         llmkit.ProtocolOpenAIChat,
			entriesKey:       "messages",
			toolResult:       `"role":"tool"`,
			wantAfterResults: []string{`"role":"user"`, unsupported},
		},
		{
			name:                  "openai_responses",
			protocol:              llmkit.ProtocolOpenAIResponses,
			isImageInputSupported: true,
			entriesKey:            "input",
			toolResult:            `"type":"function_call_output"`,
			wantAfterResults:      []string{`"type":"message"`, `"input_image"`},
		},
		{
			name:             "openai_responses_text_only",
			protocol:         llmkit.ProtocolOpenAIResponses,
			entriesKey:       "input",
			toolResult:       `"type":"function_call_output"`,
			wantAfterResults: []string{`"type":"message"`, unsupported},
		},
		{
			name:                  "claude_messages",
			protocol:              llmkit.ProtocolClaudeMessages,
			isImageInputSupported: true,
			entriesKey:            "messages",
			toolResult:            `"type":"tool_result"`,
			wantAfterResults:      []string{`"role":"user"`, `"type":"image"`, `"source":{"type":"base64"`, `"media_type":"image/png"`},
		},
		{
			name:             "claude_messages_text_only",
			protocol:         llmkit.ProtocolClaudeMessages,
			entriesKey:       "messages",
			toolResult:       `"type":"tool_result"`,
			wantAfterResults: []string{`"role":"user"`, unsupported},
		},
	}
	// 2. 按各协议编码请求，检查连续工具结果之后依次出现图片消息和 hook 反馈。
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
			if err := c.Load(&agent_context.ContextLoadParam{Items: history}); err != nil {
				t.Fatal(err)
			}
			req, _, err := c.BuildModelRequest(nil)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := llmkit.NewCodec().EncodeRequest(llmkit.EncodeRequestInput{
				Request: *req,
				Target: llmkit.Target{
					Protocol: tt.protocol,
					Model:    "m",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			body := string(encoded.Body)
			if !tt.isImageInputSupported && (strings.Contains(body, "base64,") || strings.Contains(body, `"type":"base64"`)) {
				t.Fatalf("text-only request carries image data: %s", body)
			}
			var fields map[string]json.RawMessage
			var entries []json.RawMessage
			if err := json.Unmarshal(encoded.Body, &fields); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if err := json.Unmarshal(fields[tt.entriesKey], &entries); err != nil {
				t.Fatalf("decode %s of %s: %v", tt.entriesKey, body, err)
			}
			var results []int
			for index, entry := range entries {
				for range strings.Count(string(entry), tt.toolResult) {
					results = append(results, index)
				}
			}
			if len(results) != 2 || results[1] > results[0]+1 || results[1]+2 >= len(entries) {
				t.Fatalf("tool results at %v of %d entries, want two adjacent results followed by images and hook: %s", results, len(entries), body)
			}
			afterResults, hook := string(entries[results[1]+1]), string(entries[results[1]+2])
			for _, fragment := range tt.wantAfterResults {
				if !strings.Contains(afterResults, fragment) {
					t.Fatalf("entry after tool results = %s, want it to contain %q", afterResults, fragment)
				}
			}
			if !strings.Contains(hook, "lint ok") {
				t.Fatalf("entry after image message = %s, want the hook context", hook)
			}
		})
	}
}

// TestAgentContextSnapshots 检查加载、追加和请求返回的内容使用独立副本。
func TestAgentContextSnapshots(t *testing.T) {
	// 1. 分别从加载和追加入口准备图片、推理、调用及结果。
	tests := []struct {
		name string
		load bool
	}{
		{
			name: "loaded_items",
			load: true,
		},
		{
			name: "appended_items",
		},
	}
	// 2. 加载或追加样例，分别修改输入及请求返回内容，检查再次读取的历史保持原样。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				items := []*defs.Item{
					{
						TurnID: contextTurnID,
						Kind:   defs.ItemMessage,
						Message: &defs.Message{
							From: &defs.Address{Kind: defs.IdentityUser},
							Content: []*defs.Content{
								{
									Kind: defs.ContentImage,
									Image: &defs.ImageContent{
										DataURL: "data:image/png;base64,aGVsbG8=",
										Alt:     "architecture",
									},
								},
							},
						},
					},
					{
						TurnID:      contextTurnID,
						ModelCallID: contextModelCallID,
						Kind:        defs.ItemReasoning,
						Reasoning: &defs.Reasoning{
							Status:  defs.ReasoningCompleted,
							Summary: []string{"summary"},
							Content: []string{"reasoning"},
							RawJSON: json.RawMessage(`{"signature":"signed"}`),
						},
					},
					contextToolCall("call-read", defs.ToolCallReady),
					{
						TurnID:      contextTurnID,
						ModelCallID: contextModelCallID,
						Kind:        defs.ItemToolResult,
						ToolResult: &defs.ToolResult{
							CallID:  "call-read",
							Content: json.RawMessage(`null`),
							Error: &defs.ToolError{
								Code:    "permission_denied",
								Message: "Access denied",
							},
						},
					},
				}
				conversation := newAgentContext(t)
				var err error
				if tt.load {
					err = conversation.Load(&agent_context.ContextLoadParam{Items: items})
				} else {
					err = conversation.AppendItems(items...)
				}
				if err != nil {
					t.Fatal(err)
				}
				_, before := buildContextView(t, conversation)

				items[0].Message.From.Kind = defs.IdentityAgent
				items[0].Message.From.ID = "c4b86503-4a60-416d-9300-34b1a7203457"
				items[0].Message.Content[0].Image.Alt = "changed"
				items[1].Reasoning.Summary[0] = "changed"
				items[1].Reasoning.Content[0] = "changed"
				items[1].Reasoning.RawJSON[0] = 'x'
				items[3].ToolResult.Content[0] = 'x'
				items[3].ToolResult.Error.Message = "changed"
				_, after := buildContextView(t, conversation)
				if !reflect.DeepEqual(after, before) {
					t.Fatal("context retained mutable input references")
				}

				before[0].Content[0].Metadata["alt"] = "changed"
				before[1].Content[0].Reasoning.Summary[0] = "changed"
				before[1].Content[0].Reasoning.RawJSON[0] = 'x'
				before[2].Content[0].Text = "changed"
				_, next := buildContextView(t, conversation)
				if !reflect.DeepEqual(next, after) {
					t.Fatal("BuildModelRequest returned mutable internal references")
				}
			})
	}
}

// newAgentContext 创建空的测试上下文。
func newAgentContext(t *testing.T) agent_context.AgentContext {
	// 1. 使用测试身份、模型和摘要客户端创建上下文。
	t.Helper()
	model := newTestModel(&llmkit.Target{}, 0)
	conversation, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

// buildContextView 通过模型请求读取指令和模型可见消息。
func buildContextView(t *testing.T, c agent_context.AgentContext) (string, []*llmkit.Message) {
	// 1. 组装无工具的模型请求。
	t.Helper()
	req, _, err := c.BuildModelRequest(nil)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 分离首条系统指令和其余历史消息。
	system := ""
	var messages []*llmkit.Message
	for index := range req.Messages {
		if index == 0 && req.Messages[index].Role == llmkit.RoleSystem {
			system = req.Messages[index].Content[0].Text
			continue
		}
		messages = append(messages, &req.Messages[index])
	}
	return system, messages
}

// contextMessage 创建指定发送方的测试文本 Item。
func contextMessage(sender string, kind defs.IdentityKind, text string) *defs.Item {
	// 1. 填写发送方和文本，本 Session 的 Agent 消息另填模型调用 ID。
	item := &defs.Item{
		TurnID: contextTurnID,
		Kind:   defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: kind,
				ID:   sender,
			},
			Content: []*defs.Content{
				{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: text},
				},
			},
		},
	}
	if kind == defs.IdentityAgent && sender == string(contextSessionID) {
		item.ModelCallID = contextModelCallID
	}
	return item
}

// contextToolCall 创建指定状态的测试工具调用。
func contextToolCall(callID string, status defs.ToolCallStatus) *defs.Item {
	// 1. 创建默认读取调用，再填写调用状态。
	item := contextToolCallWithArguments(callID, `{"path":"main.go"}`)
	item.ToolCall.Status = status
	return item
}

// contextToolCallWithArguments 创建带指定 CallID 和参数的读取工具调用。
func contextToolCallWithArguments(callID, arguments string) *defs.Item {
	// 1. 填写测试身份、read_file 工具名和 ready 状态。
	return &defs.Item{
		TurnID:      contextTurnID,
		ModelCallID: contextModelCallID,
		Kind:        defs.ItemToolCall,
		ToolCall: &defs.ToolCall{
			CallID:    callID,
			Name:      "read_file",
			Arguments: arguments,
			Status:    defs.ToolCallReady,
		},
	}
}

// contextToolResultItem 创建指定模型调用的工具结果 Item。
func contextToolResultItem(callID string, modelCallID defs.ModelCallID) *defs.Item {
	// 1. 填写测试 Turn、模型调用、CallID 和成功结果。
	return &defs.Item{
		TurnID:      contextTurnID,
		ModelCallID: modelCallID,
		Kind:        defs.ItemToolResult,
		ToolResult: &defs.ToolResult{
			CallID:  callID,
			Content: json.RawMessage(`{"ok":true}`),
		},
	}
}

// contextToolCallMessage 创建单个读取调用的助手消息。
func contextToolCallMessage(callID string) *llmkit.Message {
	// 1. 填写 CallID、工具名和默认文件参数。
	return &llmkit.Message{
		Role: llmkit.RoleAssistant,
		ToolCalls: []llmkit.ToolCall{
			{
				ID:        callID,
				Name:      "read_file",
				Arguments: `{"path":"main.go"}`,
			},
		},
	}
}

// contextToolCallsMessage 创建包含多个读取调用的助手消息。
func contextToolCallsMessage(callIDs ...string) *llmkit.Message {
	// 1. 按传入顺序追加各个 CallID 的读取调用。
	message := &llmkit.Message{Role: llmkit.RoleAssistant}
	for _, callID := range callIDs {
		message.ToolCalls = append(message.ToolCalls, llmkit.ToolCall{
			ID:        callID,
			Name:      "read_file",
			Arguments: `{"path":"main.go"}`,
		})
	}
	return message
}

// contextUserMessage 创建模型可见的用户文本消息。
func contextUserMessage(text string) *llmkit.Message {
	// 1. 填写用户角色和文本内容块。
	return &llmkit.Message{
		Role: llmkit.RoleUser,
		Content: []llmkit.ContentBlock{
			{
				Type: llmkit.ContentTypeText,
				Text: text,
			},
		},
	}
}

// contextMissingToolResultMessage 创建 missing_output 工具结果消息。
func contextMissingToolResultMessage(t *testing.T, callID string) *llmkit.Message {
	// 1. 填写调用 ID、空结果和缺失输出错误，再转换为消息。
	t.Helper()
	return contextToolResultMessage(t, &defs.ToolResult{
		CallID:  callID,
		Content: json.RawMessage(`null`),
		Error: &defs.ToolError{
			Code:    "missing_output",
			Message: "tool result is missing",
		},
	})
}

// contextToolResultMessage 将测试工具结果转换为模型消息。
func contextToolResultMessage(t *testing.T, result *defs.ToolResult) *llmkit.Message {
	// 1. 转换结果，失败时结束当前测试。
	t.Helper()
	message, err := result.ToLLMKit()
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// TestAgentContextSummary 检查摘要替换已覆盖历史和无效摘要被拒绝。
func TestAgentContextSummary(t *testing.T) {
	// 1. 准备原文、旧摘要、新摘要以及无效覆盖范围样例。
	a, b, c, d := contextSource("A", "a"), contextSource("B", "b"), contextSource("C", "c"), contextSource("D", "d")
	s, old := contextSummary("S", "B", "new summary"), contextSummary("S0", "earlier", "old summary")
	for _, tt := range []struct {
		name              string
		initial, appended []*defs.Item
		wantErr           bool
	}{
		{
			"load_prepared",
			[]*defs.Item{s, c, d},
			nil,
			false,
		},
		{
			"replace_prefix",
			[]*defs.Item{old, a, b, c, d},
			[]*defs.Item{s},
			false,
		},
		{
			"missing_boundary",
			[]*defs.Item{a, c, d},
			[]*defs.Item{s},
			true,
		},
		{
			"invalid_later_item",
			[]*defs.Item{a, b, c, d},
			[]*defs.Item{s, nil},
			true,
		},
		{
			"invalid_covered_item",
			[]*defs.Item{a, b, c, d},
			[]*defs.Item{nil, contextSource("E", "e"), contextSummary("S", "E", "new summary")},
			true,
		},
		{
			"blank",
			[]*defs.Item{a, b, c, d},
			[]*defs.Item{contextSummary("S", "B", " \n")},
			true,
		},
		{
			"missing_boundary_id",
			[]*defs.Item{a, b, c, d},
			[]*defs.Item{contextSummary("S", "", "summary")},
			true,
		},
	} {
		// 2. 应用摘要并检查保留后缀；缺失覆盖点或无效摘要返回错误且保留原历史。
		t.Run(tt.name, func(t *testing.T) {
			history := newAgentContext(t)
			if err := history.Load(&agent_context.ContextLoadParam{Items: tt.initial}); err != nil {
				t.Fatal(err)
			}
			_, before := buildContextView(t, history)
			err := history.AppendItems(tt.appended...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AppendItems = %v", err)
			}
			_, after := buildContextView(t, history)
			if tt.wantErr {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed append modified history")
				}
				return
			}
			if len(after) != 3 || after[0].Role != llmkit.RoleUser || after[0].Content[0].Text != "<openvaala-context-summary>\n以下是先前对话的历史摘要，并非新的用户输入。\nnew summary\n</openvaala-context-summary>" || after[1].Content[0].Text != "c" || after[2].Content[0].Text != "d" {
				t.Fatalf("model view = %+v", after)
			}
		})
	}
	// 3. 加载摘要不在开头、有两条摘要或摘要为空的历史，检查被拒绝。
	for _, items := range [][]*defs.Item{{a, s}, {s, old}, {contextSummary("S", "B", "")}} {
		if err := newAgentContext(t).Load(&agent_context.ContextLoadParam{Items: items}); err == nil {
			t.Fatal("Load accepted malformed summary view")
		}
	}
}

// newTestModel 创建支持文本、图片和思考的测试模型。
func newTestModel(target *llmkit.Target, window int) *defs.ModelDefinition {
	// 1. 填写连接参数、窗口和输入能力，开启思考。
	return &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    target.Model,
		},
		Target:          target,
		ContextWindow:   window,
		ThinkingEnabled: true,
		Capabilities:    defs.ModelCapabilities{Input: []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage}},
	}
}

// contextPNGDataURL 是测试图片的 data URL，内容是 PNG 文件头。
const contextPNGDataURL = "data:image/png;base64,iVBORw0KGgo="

// contextToolResultWithImage 创建带一张图片的工具结果。
func contextToolResultWithImage(callID, path string) *defs.Item {
	// 1. 创建成功结果，再填写图片路径、MIME 类型和图片内容。
	item := contextToolResultItem(callID, contextModelCallID)
	item.ToolResult.Content = json.RawMessage(`{"path":"` + path + `","mime_type":"image/png"}`)
	item.ToolResult.Images = []*defs.ImageContent{{
		DataURL: contextPNGDataURL,
		Alt:     path,
	}}
	return item
}

// contextUserImageMessage 创建带文本和图片的用户消息。
func contextUserImageMessage(text, alt string) *defs.Item {
	// 1. 创建文本消息，再追加指定说明的测试图片。
	item := contextMessage("alice", defs.IdentityUser, text)
	item.Message.Content = append(item.Message.Content, &defs.Content{
		Kind: defs.ContentImage,
		Image: &defs.ImageContent{
			DataURL: contextPNGDataURL,
			Alt:     alt,
		},
	})
	return item
}

// newTextOnlyTestModel 创建只支持文本输入的测试模型。
func newTextOnlyTestModel(target *llmkit.Target, window int) *defs.ModelDefinition {
	// 1. 复制默认测试模型配置，将输入能力改为文本。
	model := newTestModel(target, window)
	model.Capabilities.Input = []defs.ModelInput{defs.ModelInputText}
	return model
}

// contextSource 创建带指定 ID 的历史输入。
func contextSource(id defs.ItemID, text string) *defs.Item {
	// 1. 创建用户文本 Item 并填写指定 ID。
	item := contextMessage("alice", defs.IdentityUser, text)
	item.ID = id
	return item
}

// contextSummary 创建带覆盖范围的摘要 Item。
func contextSummary(id, boundary defs.ItemID, text string) *defs.Item {
	// 1. 填写测试身份、摘要文本和覆盖到的 Item ID。
	return &defs.Item{
		ID:          id,
		TurnID:      contextTurnID,
		ModelCallID: "compact",
		Kind:        defs.ItemContextSummary,
		ContextSummary: &defs.ContextSummary{
			Text:                 text,
			CoveredThroughItemID: boundary,
		},
	}
}

// TestContextMessageOrigins 检查消息是否按模型调用 ID 识别为助手输出。
func TestContextMessageOrigins(t *testing.T) {
	// 1. 分别加载带有和不带模型调用 ID 的 Agent 消息，检查角色及发送方不变。
	for _, modelID := range []defs.ModelCallID{"", "parent-model"} {
		t.Run(string(modelID), func(t *testing.T) {
			model := newTestModel(&llmkit.Target{}, 0)
			c, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt("child", contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			item := &defs.Item{
				ID:          defs.NewItemID(),
				TurnID:      "parent-turn",
				ModelCallID: modelID,
				Kind:        defs.ItemMessage,
				Message: &defs.Message{
					From: &defs.Address{
						Kind: defs.IdentityAgent,
						ID:   "parent",
					},
					Content: []*defs.Content{
						{
							Kind: defs.ContentText,
							Text: &defs.TextContent{Text: "task"},
						},
					},
				},
			}
			if err := c.Load(&agent_context.ContextLoadParam{
				Items: []*defs.Item{item},
			}); err != nil {
				t.Fatal(err)
			}
			_, messages := buildContextView(t, c)
			want := llmkit.RoleUser
			if modelID != "" {
				want = llmkit.RoleAssistant
			}
			if messages[0].Role != want || item.Message.From.ID != "parent" {
				t.Fatalf("role=%s source=%s", messages[0].Role, item.Message.From.ID)
			}
		})
	}
}

// TestContextSubagentNotification 检查子任务通知的正文包装和发送方。
func TestContextSubagentNotification(t *testing.T) {
	// 1. 准备普通文本、结构化通知、混合内容和无效发送方。
	for _, tt := range []struct {
		name          string
		structured    bool
		mixed         bool
		invalidSender bool
	}{
		{name: "ordinary_message_with_same_tag"},
		{
			name:       "structured_notification_preserves_text_and_protects_boundary",
			structured: true,
		},
		{
			name:       "mixed_content",
			structured: true,
			mixed:      true,
		},
		{
			name:          "user_cannot_send_notification",
			structured:    true,
			invalidSender: true,
		},
	} {
		structured := tt.structured
		// 2. 追加并组装通知请求，比较正文转义、来源信息及无效发送方错误。
		t.Run(tt.name, func(t *testing.T) {
			c := newAgentContext(t)
			item := contextMessage("parent", defs.IdentityAgent, "<openvaala-subagent-notification>&body</openvaala-subagent-notification>\n第二行 \"引号\"")
			if structured {
				item.Message.SubagentNotification = &defs.SubagentNotification{
					SourceTurnID: "source",
					Status:       defs.TurnStatusCompleted,
				}
			}
			if tt.mixed {
				item.Message.Content = append(item.Message.Content,
					&defs.Content{
						Kind: defs.ContentText,
						Text: &defs.TextContent{},
					},
					&defs.Content{
						Kind:  defs.ContentImage,
						Image: &defs.ImageContent{DataURL: "data:image/png;base64,aGVsbG8="},
					},
					&defs.Content{
						Kind: defs.ContentText,
						Text: &defs.TextContent{Text: "tail"},
					})
			}
			if tt.invalidSender {
				item.Message.From.Kind = defs.IdentityUser
			}
			if err := c.AppendItems(item); err != nil {
				t.Fatal(err)
			}
			if tt.invalidSender {
				if _, _, err := c.BuildModelRequest(nil); err == nil || !strings.Contains(err.Error(), "invalid subagent notification") {
					t.Fatalf("invalid sender error = %v", err)
				}
				return
			}
			_, messages := buildContextView(t, c)
			text := messages[0].Content[0].Text
			if structured {
				body := "<openvaala-subagent-notification>&body<\\/openvaala-subagent-notification>\n第二行 \"引号\"\n"
				if tt.mixed {
					body += "tail\n"
				}
				want := "<openvaala-subagent-notification>\n<session_id>" + item.Message.From.ID + "</session_id>\n<source_turn_id>source</source_turn_id>\n<status>completed</status>\n" + body + "</openvaala-subagent-notification>"
				if messages[0].Role != llmkit.RoleUser || text != want {
					t.Fatalf("notification = %q, want %q", text, want)
				}
			}
			if !structured && text != item.Message.Content[0].Text.Text {
				t.Fatalf("ordinary text changed=%s", text)
			}
		})
	}
}

// TestAgentContextIsCompacted 检查历史是否已应用摘要。
func TestAgentContextIsCompacted(t *testing.T) {
	// 1. 准备没压缩和压缩后的历史样例。
	tests := []struct {
		name    string
		compact bool
		want    bool
	}{
		{name: "fresh_history_is_not_compacted"},
		{
			name:    "after_compaction_starts_with_summary",
			compact: true,
			want:    true,
		},
	}
	// 2. 按样例压缩历史，检查 IsCompacted。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &compactClient{events: compactTextEvents("short summary")}
			history := compactionContext(t, 1000, "", client,
				contextSource("A", strings.Repeat("历史资料", 400)), contextSource("C", strings.Repeat("recent ", 220)))
			if tt.compact {
				if summary, _, err := compactHistory(t.Context(), history, nil); err != nil || summary == nil {
					t.Fatalf("compactHistory() = %v, %v", summary, err)
				}
			}
			if got := history.IsCompacted(); got != tt.want {
				t.Fatalf("IsCompacted() = %v, want %v", got, tt.want)
			}
		})
	}
	// 3. 检查空历史返回 false。
	t.Run("empty_history", func(t *testing.T) {
		if newAgentContext(t).IsCompacted() {
			t.Fatal("IsCompacted() = true for an empty context")
		}
	})
}

// TestAgentContextGetMeasurement 检查指令、工具和模型可见历史的估算用量。
func TestAgentContextGetMeasurement(t *testing.T) {
	// 1. 准备窗口已知和未知、含工具和不含工具的测量样例。
	const system = "你是测试助手"
	tools := []llmkit.Tool{{
		Name:        "read_file",
		Description: "读取文件",
		InputSchema: map[string]any{"type": "object"},
	}}
	tests := []struct {
		name          string
		window        int
		tools         []llmkit.Tool
		wantThreshold int
	}{
		{name: "unknown_window_without_tools"},
		{
			name:          "known_window_with_tools",
			window:        1000,
			tools:         tools,
			wantThreshold: 800,
		},
	}
	// 2. 加载完整和中断输出，检查测量只包含模型可见 Item，累计值等于分项总和。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := contextSummary("summary", "covered", "earlier work")
			user := contextSource("user", "请读取 main.go")
			peer := contextMessage("other-session", defs.IdentityAgent, "子 Agent 已完成")
			peer.ID = "peer"
			peer.Message.AgentSender = &defs.AgentSender{Relation: defs.AgentRelationChild, TaskName: "review"}
			reasoning := &defs.Item{
				ID:          "reasoning",
				TurnID:      contextTurnID,
				ModelCallID: contextModelCallID,
				Kind:        defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Status:  defs.ReasoningCompleted,
					Content: []string{"需要先读文件"},
				},
			}
			reply := contextMessage(string(contextSessionID), defs.IdentityAgent, "我来读取")
			reply.ID = "reply"
			call := contextToolCall("call-1", defs.ToolCallReady)
			call.ID = "call"
			result := contextToolResultItem("call-1", contextModelCallID)
			result.ID = "result"
			interruptedReasoning := &defs.Item{
				ID:          "interrupted-reasoning",
				TurnID:      contextTurnID,
				ModelCallID: "model-call-interrupted",
				Kind:        defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Status:  defs.ReasoningInterrupted,
					Content: []string{"unfinished"},
				},
			}
			interruptedCall := contextToolCall("call-broken", defs.ToolCallInterrupted)
			interruptedCall.ID, interruptedCall.ModelCallID, interruptedCall.ToolCall.Name = "interrupted-call", "model-call-interrupted", ""

			sent := []*defs.Item{summary, user, peer, reasoning, reply, call, result}
			wantCategories := []agent_context.ContextCategory{
				agent_context.ContextCategorySummary, agent_context.ContextCategoryUserInput, agent_context.ContextCategoryAgentMessage,
				agent_context.ContextCategoryReasoning, agent_context.ContextCategoryAssistantText,
				agent_context.ContextCategoryToolCall, agent_context.ContextCategoryToolResult,
			}
			history := compactionContext(t, tt.window, system, &compactClient{}, append(slices.Clone(sent), interruptedReasoning, interruptedCall)...)

			got := getContextMeasurement(t, history, tt.tools)
			if len(got.Items) != len(sent) {
				t.Fatalf("measured %d items, want %d: %+v", len(got.Items), len(sent), got.Items)
			}
			sum := 0
			for index, want := range sent {
				item := got.Items[index]
				if item.ItemID != want.ID || item.TurnID != want.TurnID || item.Category != wantCategories[index] || item.Tokens <= 0 {
					t.Fatalf("Items[%d] = %+v, want item %q category %q with tokens", index, item, want.ID, wantCategories[index])
				}
				sum += item.Tokens
			}
			req, _, err := history.BuildModelRequest(tt.tools)
			if err != nil {
				t.Fatal(err)
			}
			if got.Window != tt.window || got.ThresholdTokens != tt.wantThreshold ||
				got.EstimatedTokens != got.SystemTokens+got.ToolTokens+sum ||
				got.RequestTokens != requestEstimatedTokens(t, req) || got.IsUsageBased {
				t.Fatalf("measurement = %+v, want window %d threshold %d items sum %d", got, tt.window, tt.wantThreshold, sum)
			}
		})
	}
}

// TestBuildModelRequestUsesModelParameters 检查请求使用模型输出上限、思考开关、思考上下文模式和本轮推理强度。
func TestBuildModelRequestUsesModelParameters(t *testing.T) {
	// 1. 准备不同模型参数和推理强度组合。
	tests := []struct {
		name                 string
		model                *defs.ModelDefinition
		effort               string
		wantMaxTokens        int
		wantThinking         bool
		wantReasoningContext llmkit.ReasoningContext
	}{
		{
			name: "thinking_with_effort",
			model: &defs.ModelDefinition{
				Target:          &llmkit.Target{},
				MaxOutputTokens: 32000,
				ThinkingEnabled: true,
			},
			effort:        "high",
			wantMaxTokens: 32000,
			wantThinking:  true,
		},
		{
			name: "reasoning_context_all_turns",
			model: &defs.ModelDefinition{
				Target:           &llmkit.Target{},
				MaxOutputTokens:  32000,
				ThinkingEnabled:  true,
				ReasoningContext: llmkit.ReasoningContextAllTurns,
			},
			effort:               "high",
			wantMaxTokens:        32000,
			wantThinking:         true,
			wantReasoningContext: llmkit.ReasoningContextAllTurns,
		},
		{
			name: "thinking_disabled_without_effort",
			model: &defs.ModelDefinition{
				Target:          &llmkit.Target{},
				MaxOutputTokens: 4000,
			},
			wantMaxTokens: 4000,
		},
		{
			name: "zero_output_limit_omitted",
			model: &defs.ModelDefinition{
				Target:          &llmkit.Target{},
				ThinkingEnabled: true,
			},
			effort:       "low",
			wantThinking: true,
		},
	}
	// 2. 组装请求，比较输出上限、思考、推理强度、流选项和 Session 缓存键。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt := agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, tt.model, tt.model)
			opt.ReasoningEffort = tt.effort
			history, err := agent_context.NewAgentContext(opt)
			if err != nil {
				t.Fatal(err)
			}
			if err := history.Load(&agent_context.ContextLoadParam{
				System: "system",
				Items:  []*defs.Item{contextSource("A", "你好")},
			}); err != nil {
				t.Fatal(err)
			}
			req, _, err := history.BuildModelRequest(nil)
			if err != nil {
				t.Fatal(err)
			}
			if req.MaxTokens != tt.wantMaxTokens || req.ThinkingEnabled == nil || *req.ThinkingEnabled != tt.wantThinking || req.ReasoningEffort != tt.effort ||
				req.ReasoningContext != tt.wantReasoningContext {
				t.Fatalf("request = max %d thinking %v effort %q context %q", req.MaxTokens, req.ThinkingEnabled, req.ReasoningEffort, req.ReasoningContext)
			}
			if req.Extras["prompt_cache_key"] != string(contextSessionID) || !req.Stream {
				t.Fatalf("request lost stream or cache key: %+v", req)
			}
		})
	}
}

// TestContextModelChangeMessage 检查模型切换记录转换为用户通知。
func TestContextModelChangeMessage(t *testing.T) {
	// 1. 准备切换记录、前后输入和缺失切换内容的样例。
	change := &defs.Item{
		ID:     "c",
		TurnID: contextTurnID,
		Kind:   defs.ItemModelChange,
		ModelChange: &defs.ModelChange{
			PreviousModel: defs.ModelRef{
				ProviderID: "anthropic",
				ModelID:    "opus",
			},
			Model: defs.ModelRef{
				ProviderID: "openai",
				ModelID:    "gpt-5",
			},
		},
	}
	const notice = "<openvaala-system-info>\n<openvaala-model-change-event>\n从这里开始，对话改由模型 openai/gpt-5 继续；此前的回复来自模型 anthropic/opus\n</openvaala-model-change-event>\n</openvaala-system-info>"
	tests := []struct {
		name    string
		items   []*defs.Item
		want    string // 切换提示的完整文本
		wantErr bool   // 期望加载失败
	}{
		{
			name:  "notice_between_inputs",
			items: []*defs.Item{contextSource("A", "第一轮"), change, contextSource("B", "第二轮")},
			want:  notice,
		},
		{
			name:  "notice_at_start",
			items: []*defs.Item{change, contextSource("B", "第二轮")},
			want:  notice,
		},
		{
			name: "missing_payload_rejected",
			items: []*defs.Item{{
				ID:     "c",
				TurnID: contextTurnID,
				Kind:   defs.ItemModelChange,
			}},
			wantErr: true,
		},
	}
	// 2. 加载变化历史，检查缺失载荷错误或包含完整模型切换通知的用户消息。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestModel(&llmkit.Target{}, 0)
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			err = history.Load(&agent_context.ContextLoadParam{Items: tt.items})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			_, messages := buildContextView(t, history)
			found := slices.ContainsFunc(messages, func(message *llmkit.Message) bool {
				return message.Role == llmkit.RoleUser && message.Content[0].Text == tt.want
			})
			if !found {
				t.Fatalf("messages = %+v, want a user notice %q", messages, tt.want)
			}
		})
	}
}

// TestContextExecutorsChangeMessage 检查 Executor 变化记录转换为用户通知。
func TestContextExecutorsChangeMessage(t *testing.T) {
	// 1. 准备前后 Executor 列表、默认环境变化和缺失内容的样例。
	change := &defs.Item{
		ID:     "c",
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
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}},
		},
	}
	const notice = "<openvaala-system-info>\n<openvaala-executors-change-event>\n" +
		"Executor 已变更：\n- 默认 Executor 从 local 改为 gpu，省略 executor 参数的工具调用在 gpu 的 /data 执行。\n" +
		"当前可用的 Executor 和说明见系统提示 <openvaala-executors>；之前历史里的路径属于原来的位置。\n" +
		"</openvaala-executors-change-event>\n</openvaala-system-info>"
	tests := []struct {
		name    string
		items   []*defs.Item
		wantErr bool
	}{
		{
			name:  "notice_between_inputs",
			items: []*defs.Item{contextSource("A", "第一轮"), change, contextSource("B", "第二轮")},
		},
		{
			name: "missing_payload_rejected",
			items: []*defs.Item{{
				ID:     "c",
				TurnID: contextTurnID,
				Kind:   defs.ItemExecutorsChange,
			}},
			wantErr: true,
		},
	}
	// 2. 加载 Executor 变化，检查缺失载荷错误或包含默认环境变化的用户消息。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestModel(&llmkit.Target{}, 0)
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			err = history.Load(&agent_context.ContextLoadParam{Items: tt.items})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			_, messages := buildContextView(t, history)
			found := slices.ContainsFunc(messages, func(message *llmkit.Message) bool {
				return message.Role == llmkit.RoleUser && message.Content[0].Text == notice
			})
			if !found {
				t.Fatalf("messages = %+v, want a user notice %q", messages, notice)
			}
		})
	}
}

// TestContextTurnInterrupted 检查中断通知和未完成工具参数的模型可见内容。
func TestContextTurnInterrupted(t *testing.T) {
	// 1. 准备部分回复、部分工具参数和下一轮输入。
	interrupted := &defs.Item{
		ID:     "i",
		TurnID: contextTurnID,
		Kind:   defs.ItemTurnInterrupted,
	}
	interruptedCall := contextToolCallWithArguments("call-1", `{"command":"ls`)
	interruptedCall.ToolCall.Status = defs.ToolCallInterrupted
	notice := contextUserMessage(prompt.FormatTurnInterrupted())
	tests := []struct {
		name  string
		items []*defs.Item
		want  []*llmkit.Message
	}{
		{
			name: "partial_reply_then_notice_then_next_input",
			items: []*defs.Item{
				contextMessage("alice", defs.IdentityUser, "做 A"),
				contextMessage(string(contextSessionID), defs.IdentityAgent, "我先看"),
				interrupted,
				contextMessage("alice", defs.IdentityUser, "改做 B"),
			},
			want: []*llmkit.Message{
				contextUserMessage("做 A"),
				{
					Role: llmkit.RoleAssistant,
					Content: []llmkit.ContentBlock{{
						Type: llmkit.ContentTypeText,
						Text: "我先看",
					}},
				},
				notice,
				contextUserMessage("改做 B"),
			},
		},
		{
			name: "interrupted_tool_call_with_partial_arguments_is_not_sent",
			items: []*defs.Item{
				contextMessage("alice", defs.IdentityUser, "做 A"),
				interruptedCall,
				interrupted,
				contextMessage("alice", defs.IdentityUser, "改做 B"),
			},
			want: []*llmkit.Message{contextUserMessage("做 A"), notice, contextUserMessage("改做 B")},
		},
	}
	// 2. 加载历史，比较模型可见消息：部分回复照常发送，缺少完整参数的中断调用不发送，中断通知在下一轮输入之前。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := newAgentContext(t)
			if err := history.Load(&agent_context.ContextLoadParam{Items: tt.items}); err != nil {
				t.Fatal(err)
			}
			_, messages := buildContextView(t, history)
			if !reflect.DeepEqual(messages, tt.want) {
				t.Fatalf("messages = %+v, want %+v", messages, tt.want)
			}
		})
	}
}

// TestInstructionItemsUseOrdinaryHistory 检查 skill、目录规则、记忆目录和覆盖它们的摘要都按普通历史发给模型。
func TestInstructionItemsUseOrdinaryHistory(t *testing.T) {
	// 1. 分别加载 skill、目录规则和记忆目录，检查用户消息、规则识别、副本和摘要替换。
	for _, tt := range []struct {
		name string
		item *defs.Item
	}{
		{
			"skill",
			&defs.Item{
				Kind: defs.ItemSkillInstruction,
				SkillInstruction: &defs.SkillInstruction{
					InputItemID: "input",
					Name:        "review",
					Executor:    "local",
					Path:        "/skills/review/SKILL.md",
					Text:        "check errors",
				},
			},
		},
		{
			"workspace_rules",
			&defs.Item{
				Kind: defs.ItemAgentsInstruction,
				AgentsInstruction: &defs.AgentsInstruction{
					Executor: "local",
					Path:     "/repo/AGENTS.md",
					Text:     "run tests",
				},
			},
		},
		{
			"file_rules",
			&defs.Item{
				Kind:        defs.ItemAgentsInstruction,
				ModelCallID: "read-call",
				AgentsInstruction: &defs.AgentsInstruction{
					Executor: "local",
					Path:     "/repo/AGENTS.md",
					Text:     "run tests",
				},
			},
		},
		{
			"memory_index",
			&defs.Item{
				Kind: defs.ItemMemoryIndex,
				MemoryIndex: &defs.MemoryIndex{
					Path:        "user/MEMORY.md",
					Text:        "- 回复用中文",
					IsTruncated: true,
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestModel(&llmkit.Target{}, 0)
			c, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			tt.item.ID = "instruction"
			tt.item.TurnID = contextTurnID
			if err := c.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{tt.item}}); err != nil {
				t.Fatal(err)
			}
			req, _, err := c.BuildModelRequest(nil)
			if err != nil || len(req.Messages) != 1 || req.Messages[0].Role != llmkit.RoleUser {
				t.Fatalf("request = %+v, %v", req, err)
			}
			measurement := getContextMeasurement(t, c, nil)
			if measurement.EstimatedTokens != requestEstimatedTokens(t, req) {
				t.Fatalf("instruction tokens = %d, want formatted request %d", measurement.EstimatedTokens, requestEstimatedTokens(t, req))
			}
			wantLoaded := tt.item.Kind == defs.ItemAgentsInstruction
			if c.IsAgentsInstructionLoaded("local", "/repo/AGENTS.md") != wantLoaded || c.IsAgentsInstructionLoaded("remote", "/repo/AGENTS.md") {
				t.Fatal("incorrect rule identity")
			}
			wantMemoryLoaded := tt.item.Kind == defs.ItemMemoryIndex
			if c.IsMemoryIndexLoaded("user/MEMORY.md", "") != wantMemoryLoaded || c.IsMemoryIndexLoaded("project/MEMORY.md", "") {
				t.Fatal("incorrect memory index identity")
			}
			switch {
			case tt.item.AgentsInstruction != nil:
				tt.item.AgentsInstruction.Text = "mutated"
			case tt.item.MemoryIndex != nil:
				tt.item.MemoryIndex.Text = "mutated"
			default:
				tt.item.SkillInstruction.Text = "mutated"
			}
			req, _, err = c.BuildModelRequest(nil)
			if err != nil || strings.Contains(req.Messages[0].Content[0].Text, "mutated") {
				t.Fatalf("load did not retain independent contents: %v", err)
			}
			if err := c.AppendItems(&defs.Item{
				ID:          "summary",
				TurnID:      contextTurnID,
				ModelCallID: "compact",
				Kind:        defs.ItemContextSummary,
				ContextSummary: &defs.ContextSummary{
					Text:                 "rules at /repo/AGENTS.md",
					CoveredThroughItemID: "instruction",
				},
			}); err != nil {
				t.Fatal(err)
			}
			if c.IsAgentsInstructionLoaded("local", "/repo/AGENTS.md") || c.IsMemoryIndexLoaded("user/MEMORY.md", "") {
				t.Fatal("summary text counted as a loaded rule")
			}
			req, _, err = c.BuildModelRequest(nil)
			if err != nil || len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content[0].Text, "rules at /repo/AGENTS.md") {
				t.Fatalf("summary = %+v, %v", req, err)
			}
		})
	}
}

// TestIsMemoryIndexLoaded 检查记忆目录按路径和项目一起判断：同一路径的另一个项目不算已加载。
func TestIsMemoryIndexLoaded(t *testing.T) {
	// 1. 历史里有用户级记忆和项目 A 的项目记忆。
	const projectA, projectB = "projects/local/-repo-a", "projects/local/-repo-b"
	items := []*defs.Item{
		{ID: "user", TurnID: contextTurnID, Kind: defs.ItemMemoryIndex, MemoryIndex: &defs.MemoryIndex{Path: "user/MEMORY.md", Text: "- 回复用中文"}},
		{ID: "project", TurnID: contextTurnID, Kind: defs.ItemMemoryIndex, MemoryIndex: &defs.MemoryIndex{Path: "project/MEMORY.md", Project: projectA, Text: "- 测试用 table-driven"}},
	}
	tests := []struct {
		name    string
		path    string
		project string
		want    bool
	}{
		{name: "user_memory", path: "user/MEMORY.md", want: true},
		{name: "project_a_memory", path: "project/MEMORY.md", project: projectA, want: true},
		{name: "project_b_memory", path: "project/MEMORY.md", project: projectB, want: false},
		{name: "project_memory_without_project", path: "project/MEMORY.md", want: false},
		{name: "user_memory_with_project", path: "user/MEMORY.md", project: projectA, want: false},
		{name: "unknown_path", path: "other/MEMORY.md", want: false},
	}
	// 2. 加载历史后逐项判断。
	model := newTestModel(&llmkit.Target{}, 0)
	c, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Load(&agent_context.ContextLoadParam{Items: items}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.IsMemoryIndexLoaded(tt.path, tt.project); got != tt.want {
				t.Fatalf("IsMemoryIndexLoaded(%q, %q) = %v, want %v", tt.path, tt.project, got, tt.want)
			}
		})
	}
}

// getContextMeasurement 读取上下文测量，失败时结束测试。
func getContextMeasurement(t *testing.T, history agent_context.AgentContext, tools []llmkit.Tool) *agent_context.ContextMeasurement {
	// 1. 测量真实请求并检查错误。
	t.Helper()
	measurement, err := history.GetMeasurement(tools)
	if err != nil {
		t.Fatal(err)
	}
	return measurement
}

// requestEstimatedTokens 从实际请求逐项计算预期用量。
func requestEstimatedTokens(t *testing.T, req *llmkit.Request) int {
	// 1. 创建离线词表并收集实际请求的角色、正文、调用与图片。
	t.Helper()
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	tokens := 0
	var texts []string
	for _, message := range req.Messages {
		tokens += 3
		texts = append(texts, string(message.Role), message.ToolCallID)
		for _, block := range message.Content {
			if block.Type == llmkit.ContentTypeImage || block.Type == llmkit.ContentTypeImageURL {
				tokens += 1600
				continue
			}
			texts = append(texts, block.Text)
			if block.Reasoning != nil {
				texts = append(texts, block.Reasoning.Summary...)
				texts = append(texts, block.Reasoning.Content...)
				decodedBytes := len(strings.TrimRight(block.Reasoning.Encrypted, "=")) * 3 / 4
				tokens += (max(0, decodedBytes-28) + 3) / 4
			}
		}
		for _, call := range message.ToolCalls {
			texts = append(texts, call.ID, call.Name, call.Arguments)
		}
	}
	if len(req.Tools) > 0 {
		toolsJSON, err := json.Marshal(req.Tools)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(toolsJSON))
	}
	// 2. 使用相同词表计数，独立核对真实请求与 Item 分项。
	for _, text := range texts {
		count, err := codec.Count(text)
		if err != nil {
			t.Fatal(err)
		}
		tokens += count
	}
	return tokens
}

// TestContextMeasurementText 检查中文、代码和带转义字符的文本按原文计数。
func TestContextMeasurementText(t *testing.T) {
	// 1. 准备 o200k_base 正文计数样例。
	for _, tt := range []struct {
		name           string
		text           string
		wantTextTokens int
	}{
		{name: "chinese", text: "你好世界", wantTextTokens: 2},
		{name: "code", text: "func main() {\n\tfmt.Println(\"你好 <tag>&\")\n}", wantTextTokens: 13},
		{name: "json", text: `{"text":"你好 <tag>&\n\"quoted\""}`, wantTextTokens: 13},
		{name: "html_and_quotes", text: "<tag>&\"hello\"\n你好", wantTextTokens: 6},
		{name: "large_unbroken_ascii", text: strings.Repeat("x", 3<<19), wantTextTokens: (3 << 19) / 8},
		{name: "long_chinese_keeps_utf8", text: strings.Repeat("你好世界", 400), wantTextTokens: 800},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 测量原文，消息框架为 3，加上 user 角色的 1 个 token。
			history := compactionContext(t, 0, "", &compactClient{}, contextSource("original-id", tt.text))
			got := getContextMeasurement(t, history, nil)
			if got.RequestTokens != tt.wantTextTokens+4 || got.Items[0].Tokens != tt.wantTextTokens+4 {
				t.Fatalf("measurement = %+v, want text %d plus message framing 4", got, tt.wantTextTokens)
			}
			// 3. 修改 Item 内部身份、时间，不改变模型看到的文字和估算。
			item := contextSource(defs.ItemID(strings.Repeat("long-private-item-id-", 100)), tt.text)
			item.TurnID = defs.TurnID(strings.Repeat("long-private-turn-id-", 100))
			item.UpdatedAt = 123456789
			if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{item}}); err != nil {
				t.Fatal(err)
			}
			if after := getContextMeasurement(t, history, nil); after.EstimatedTokens != got.EstimatedTokens {
				t.Fatalf("internal metadata changed tokens from %d to %d", got.EstimatedTokens, after.EstimatedTokens)
			}
		})
	}
}

// TestContextMeasurementEncryptedReasoning 检查 AES-GCM 密文按长度估算，不按随机字符分词。
func TestContextMeasurementEncryptedReasoning(t *testing.T) {
	// 1. 用固定测试密钥和 nonce 生成不同长度、Base64 编码形式的密文。
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name       string
		bytes      int
		encoding   *base64.Encoding
		wantTokens int
	}{
		{name: "empty", encoding: base64.StdEncoding},
		{name: "round_up", bytes: 1, encoding: base64.StdEncoding, wantTokens: 1},
		{name: "standard_padded", bytes: 4000, encoding: base64.StdEncoding, wantTokens: 1000},
		{name: "standard_unpadded", bytes: 4000, encoding: base64.RawStdEncoding, wantTokens: 1000},
		{name: "url_padded", bytes: 4000, encoding: base64.URLEncoding, wantTokens: 1000},
		{name: "url_unpadded", bytes: 4000, encoding: base64.RawURLEncoding, wantTokens: 1000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nonce := make([]byte, gcm.NonceSize())
			ciphertext := gcm.Seal(nonce, nonce, []byte(strings.Repeat("x", tt.bytes)), nil)
			model := newTestModel(&llmkit.Target{Protocol: llmkit.ProtocolOpenAIResponses}, 100000)
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			item := &defs.Item{
				ID: "reasoning", TurnID: contextTurnID, ModelCallID: contextModelCallID, Kind: defs.ItemReasoning,
				Reasoning: &defs.Reasoning{Status: defs.ReasoningCompleted, Encrypted: tt.encoding.EncodeToString(ciphertext)},
			}
			if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{item}}); err != nil {
				t.Fatal(err)
			}
			// 2. 检查首次和重复测量，密文估算加一次消息框架与角色。
			for range 2 {
				got := getContextMeasurement(t, history, nil)
				if got.EstimatedTokens != tt.wantTokens+4 || got.Items[0].Tokens != tt.wantTokens+4 || got.IsUsageBased {
					t.Fatalf("measurement = %+v, want %d encrypted tokens plus framing", got, tt.wantTokens)
				}
			}
			// 3. 原有 usage 校正保留实测输入，原始本地估算不变。
			history.RecordRequestUsage(&defs.TurnContextUsage{
				ModelCallID:        contextModelCallID,
				InputThroughItemID: item.ID,
				Usage:              defs.TokenUsage{Input: 1234},
			})
			got := getContextMeasurement(t, history, nil)
			if got.RequestTokens != 1234 || !got.IsUsageBased || got.EstimatedTokens != tt.wantTokens+4 {
				t.Fatalf("usage changed local estimate: %+v", got)
			}
		})
	}
}

// TestContextMeasurementReasoningWire 检查只统计实际发送的推理明文和原生加密内容。
func TestContextMeasurementReasoningWire(t *testing.T) {
	// 1. 准备 4028 字节密文、跨协议封装以及不同协议的正文与摘要。
	encrypted := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 4028)))
	responsesEnvelope := "llmkit:v1:" + base64.RawURLEncoding.EncodeToString([]byte(`{"protocol":"responses","data":{"type":"reasoning","encrypted_content":"`+encrypted+`"}}`))
	claudeEnvelope := "llmkit:v1:" + base64.RawURLEncoding.EncodeToString([]byte(`{"protocol":"claude","data":{"type":"thinking","signature":"`+encrypted+`"}}`))
	for _, tt := range []struct {
		name       string
		protocol   llmkit.Protocol
		content    []string
		summary    []string
		encrypted  string
		wantTokens int
	}{
		{name: "chat_drops_summary_and_ciphertext", protocol: llmkit.ProtocolOpenAIChat, content: []string{"你好世界"}, summary: []string{"summary"}, encrypted: encrypted, wantTokens: 2},
		{name: "chat_joins_fragments", protocol: llmkit.ProtocolOpenAIChat, content: []string{"你", "好", "世界"}, wantTokens: 2},
		{name: "chat_summary_fallback", protocol: llmkit.ProtocolOpenAIChat, summary: []string{"summary"}, encrypted: encrypted, wantTokens: 1},
		{name: "claude_signature_is_not_ciphertext", protocol: llmkit.ProtocolClaude, content: []string{"你好世界"}, summary: []string{"summary"}, encrypted: encrypted, wantTokens: 2},
		{name: "claude_joins_fragments", protocol: llmkit.ProtocolClaude, content: []string{"你", "好", "世界"}, wantTokens: 2},
		{name: "responses_sends_both_texts", protocol: llmkit.ProtocolOpenAIResponses, content: []string{"你好世界"}, summary: []string{"summary"}, encrypted: encrypted, wantTokens: 1003},
		{name: "responses_short_ciphertext", protocol: llmkit.ProtocolOpenAIResponses, encrypted: "YWJjZA=="},
		{name: "responses_unwraps_own_ciphertext", protocol: llmkit.ProtocolOpenAIResponses, content: []string{"你好世界"}, encrypted: responsesEnvelope, wantTokens: 1002},
		{name: "responses_does_not_count_wrapped_signature", protocol: llmkit.ProtocolOpenAIResponses, content: []string{"你好世界"}, encrypted: claudeEnvelope, wantTokens: 2},
		{name: "claude_unwraps_responses_ciphertext", protocol: llmkit.ProtocolClaude, content: []string{"你好世界"}, encrypted: responsesEnvelope, wantTokens: 1002},
		{name: "chat_drops_wrapped_ciphertext", protocol: llmkit.ProtocolOpenAIChat, content: []string{"你好世界"}, encrypted: responsesEnvelope, wantTokens: 2},
		{name: "custom_client_fallback", content: []string{"你好世界"}, encrypted: encrypted, wantTokens: 1002},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := newTestModel(&llmkit.Target{Protocol: tt.protocol}, 100000)
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			item := &defs.Item{
				ID: "reasoning", TurnID: contextTurnID, ModelCallID: contextModelCallID, Kind: defs.ItemReasoning,
				Reasoning: &defs.Reasoning{Status: defs.ReasoningCompleted, Content: tt.content, Summary: tt.summary, Encrypted: tt.encrypted},
			}
			if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{item}}); err != nil {
				t.Fatal(err)
			}
			before, _, err := history.BuildModelRequest(nil)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 核对本地分项和总量，重复计数不改变结果。
			for range 2 {
				got := getContextMeasurement(t, history, nil)
				if got.EstimatedTokens != tt.wantTokens+4 || got.Items[0].Tokens != tt.wantTokens+4 {
					t.Fatalf("measurement = %+v, want %d reasoning tokens plus framing", got, tt.wantTokens)
				}
			}
			// 3. 估算不改变真实请求的推理内容和签名。
			after, _, err := history.BuildModelRequest(nil)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("measurement changed request: %v", err)
			}
		})
	}
}

// TestContextMeasurementReasoningRawJSON 检查保留原始推理和显式清空正文、摘要时的估算。
func TestContextMeasurementReasoningRawJSON(t *testing.T) {
	// 1. 准备同一份原始推理，分别保留和清空其中的明文。
	for _, tt := range []struct {
		name    string
		raw     json.RawMessage
		summary bool
		reverse bool
	}{
		{name: "summary_retained_first", raw: json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"hello world"}]}`), summary: true},
		{name: "summary_cleared_first", raw: json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"hello world"}]}`), summary: true, reverse: true},
		{name: "content_retained_first", raw: json.RawMessage(`{"type":"reasoning","content":[{"type":"reasoning_text","text":"hello world"}]}`)},
		{name: "content_cleared_first", raw: json.RawMessage(`{"type":"reasoning","content":[{"type":"reasoning_text","text":"hello world"}]}`), reverse: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			items := []*defs.Item{
				{ID: "retained", TurnID: contextTurnID, ModelCallID: contextModelCallID, Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{Status: defs.ReasoningCompleted, RawJSON: tt.raw}},
				{ID: "cleared", TurnID: contextTurnID, ModelCallID: contextModelCallID, Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{Status: defs.ReasoningCompleted, RawJSON: tt.raw}},
			}
			if tt.summary {
				items[1].Reasoning.Summary = []string{}
			} else {
				items[1].Reasoning.Content = []string{}
			}
			if tt.reverse {
				slices.Reverse(items)
			}
			model := newTestModel(&llmkit.Target{Protocol: llmkit.ProtocolOpenAIResponses}, 100000)
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			if err := history.Load(&agent_context.ContextLoadParam{Items: items}); err != nil {
				t.Fatal(err)
			}
			// 2. 两种顺序都只计一份 hello world 和一次消息框架，重复测量结果相同。
			for range 2 {
				got := getContextMeasurement(t, history, nil)
				if got.EstimatedTokens != 6 || got.Items[0].Tokens+got.Items[1].Tokens != 6 {
					t.Fatalf("measurement = %+v, want 2 text tokens plus framing 4", got)
				}
			}
		})
	}
}

// TestContextMeasurementImages 检查图片上限、模型能力和工具图片的实际请求用量。
func TestContextMeasurementImages(t *testing.T) {
	// 1. 准备普通输入和工具结果的图片样例。
	for _, tt := range []struct {
		name           string
		imageSupported bool
		userImages     int
		toolImages     int
		wantImages     int
		largeImage     bool
	}{
		{name: "user_image", imageSupported: true, userImages: 1, wantImages: 1},
		{name: "large_base64_fixed_cost", imageSupported: true, userImages: 1, wantImages: 1, largeImage: true},
		{name: "latest_twenty_across_user_and_tool", imageSupported: true, userImages: 18, toolImages: 5, wantImages: 20},
		{name: "unsupported_uses_placeholders", userImages: 18, toolImages: 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			imageData := "data:image/png;base64,aGVsbG8="
			if tt.largeImage {
				imageData = "data:image/png;base64," + strings.Repeat("A", 1<<20)
			}
			user := contextSource("user", "看图")
			for index := 0; index < tt.userImages; index++ {
				user.Message.Content = append(user.Message.Content, &defs.Content{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: imageData, Alt: "截图"}})
			}
			items := []*defs.Item{user}
			if tt.toolImages > 0 {
				call := contextToolCall("call-1", defs.ToolCallReady)
				call.ID = "call"
				result := contextToolResultItem("call-1", contextModelCallID)
				result.ID = "result"
				for index := 0; index < tt.toolImages; index++ {
					result.ToolResult.Images = append(result.ToolResult.Images, &defs.ImageContent{DataURL: imageData, Alt: "工具截图"})
				}
				items = append(items, call, result)
			}
			model := newTestModel(&llmkit.Target{}, 100000)
			if tt.imageSupported {
				model.Capabilities.Input = []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage}
			} else {
				model.Capabilities.Input = []defs.ModelInput{defs.ModelInputText}
			}
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
			if err != nil {
				t.Fatal(err)
			}
			if err := history.Load(&agent_context.ContextLoadParam{Items: items}); err != nil {
				t.Fatal(err)
			}
			// 2. 从实际发出的消息数图片，按真实文字和固定图片成本校验分项总和。
			req, _, err := history.BuildModelRequest(nil)
			if err != nil {
				t.Fatal(err)
			}
			images := 0
			for _, message := range req.Messages {
				for _, block := range message.Content {
					if block.Type == llmkit.ContentTypeImage || block.Type == llmkit.ContentTypeImageURL {
						images++
					}
				}
			}
			got := getContextMeasurement(t, history, nil)
			sum := got.SystemTokens + got.ToolTokens
			for _, item := range got.Items {
				sum += item.Tokens
			}
			if images != tt.wantImages || got.EstimatedTokens != requestEstimatedTokens(t, req) || got.EstimatedTokens != sum {
				t.Fatalf("images = %d, measurement = %+v, want images %d and actual request tokens %d", images, got, tt.wantImages, requestEstimatedTokens(t, req))
			}
			if tt.userImages == 1 && tt.toolImages == 0 && got.EstimatedTokens != 1606 {
				t.Fatalf("single image tokens = %d, want 1600 image + 2 text + 4 framing", got.EstimatedTokens)
			}
		})
	}
}

// TestContextMeasurementUsage 检查最近请求用量校正、基准替换与上下文替换后的清空。
func TestContextMeasurementUsage(t *testing.T) {
	// 1. 准备基准存在、替换与失败保留的样例。
	for _, tt := range []struct {
		name      string
		action    string
		wantUsage bool
	}{
		{name: "no_usage", action: "no_usage"},
		{name: "same_request", action: "same", wantUsage: true},
		{name: "new_input", action: "append", wantUsage: true},
		{name: "missing_output_replaced_by_result", action: "result", wantUsage: true},
		{
			name:   "zero_input_usage",
			action: "zero_usage",
		},
		{name: "tool_definition_changed", action: "tools", wantUsage: true},
		{name: "latest_usage_replaces_baseline", action: "replace", wantUsage: true},
		{name: "load_clears_baseline", action: "load"},
		{name: "summary_clears_baseline", action: "summary"},
		{name: "failed_load_preserves_baseline", action: "failed_load", wantUsage: true},
		{name: "failed_summary_preserves_baseline", action: "failed_summary", wantUsage: true},
		{name: "another_turn_has_no_baseline", action: "new_turn"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			history := compactionContext(t, 1000, "系统提示", &compactClient{}, contextSource("A", "旧输入"), contextSource("B", "新输入"))
			if tt.action == "result" {
				if err := history.AppendItems(contextToolCall("call-1", defs.ToolCallReady)); err != nil {
					t.Fatal(err)
				}
			}
			baseline := getContextMeasurement(t, history, nil).EstimatedTokens
			inputTokens := 700
			if tt.action == "zero_usage" {
				inputTokens = 0
			}
			if tt.action != "no_usage" {
				_, inputThroughItemID, err := history.BuildModelRequest(nil)
				if err != nil {
					t.Fatal(err)
				}
				history.RecordRequestUsage(&defs.TurnContextUsage{
					ModelCallID:        contextModelCallID,
					InputThroughItemID: inputThroughItemID,
					Usage: defs.TokenUsage{
						Input:      inputTokens,
						Output:     9999,
						CacheRead:  500,
						CacheWrite: 100,
					},
				})
				getContextMeasurement(t, history, nil)
			}
			var tools []llmkit.Tool
			switch tt.action {
			case "append", "replace":
				if err := history.AppendItems(contextSource("C", "追加中文和代码 func main() {}")); err != nil {
					t.Fatal(err)
				}
				if tt.action == "replace" {
					baseline = getContextMeasurement(t, history, nil).EstimatedTokens
					inputTokens = 950
					history.RecordRequestUsage(&defs.TurnContextUsage{
						ModelCallID:        contextModelCallID,
						InputThroughItemID: "C",
						Usage:              defs.TokenUsage{Input: inputTokens},
					})
					getContextMeasurement(t, history, nil)
					if err := history.AppendItems(contextSource("D", "更晚的结果")); err != nil {
						t.Fatal(err)
					}
				}
			case "result":
				if err := history.AppendItems(contextToolResultItem("call-1", contextModelCallID)); err != nil {
					t.Fatal(err)
				}
			case "tools":
				tools = []llmkit.Tool{{Name: "read", Description: "读取", InputSchema: map[string]any{"type": "object"}}}
			case "load":
				if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{contextSource("C", "替换历史")}}); err != nil {
					t.Fatal(err)
				}
			case "summary":
				if err := history.AppendItems(contextSummary("S", "A", "摘要")); err != nil {
					t.Fatal(err)
				}
			case "failed_load":
				if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{{Kind: defs.ItemMessage}}}); err == nil {
					t.Fatal("invalid history accepted")
				}
			case "failed_summary":
				if err := history.AppendItems(contextSummary("S", "unknown", "摘要")); err == nil {
					t.Fatal("invalid summary accepted")
				}
			case "new_turn":
				history = compactionContext(t, 1000, "系统提示", &compactClient{}, contextSource("A", "旧输入"), contextSource("B", "新输入"))
			}
			// 2. 原请求未产生输出时只沿用实际 Input，新增内容和原输入变化按本地分项调整。
			got := getContextMeasurement(t, history, tools)
			want := got.EstimatedTokens
			if tt.wantUsage {
				want = inputTokens + got.EstimatedTokens - baseline
			}
			if got.RequestTokens != want || got.IsUsageBased != tt.wantUsage {
				t.Fatalf("measurement = %+v, want request %d usage based %v", got, want, tt.wantUsage)
			}
			req, _, err := history.BuildModelRequest(tools)
			if err != nil {
				t.Fatal(err)
			}
			if got.EstimatedTokens != requestEstimatedTokens(t, req) {
				t.Fatalf("local tokens = %d, want real request %d", got.EstimatedTokens, requestEstimatedTokens(t, req))
			}
		})
	}
}

// TestContextMeasurementErrors 检查无效历史和工具定义返回普通错误。
func TestContextMeasurementErrors(t *testing.T) {
	// 1. 分别准备不能转换的历史及不能编码的工具 schema。
	for _, tt := range []struct {
		name  string
		item  *defs.Item
		tools []llmkit.Tool
	}{
		{name: "invalid_message", item: &defs.Item{ID: "bad", TurnID: contextTurnID, Kind: defs.ItemMessage}},
		{name: "invalid_tool_schema", tools: []llmkit.Tool{{Name: "bad", InputSchema: func() {}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			history := newAgentContext(t)
			if tt.item != nil {
				if err := history.AppendItems(tt.item); err != nil {
					t.Fatal(err)
				}
			}
			// 2. 测量失败时不返回一个静默忽略内容的估算。
			measurement, err := history.GetMeasurement(tt.tools)
			if err == nil || measurement != nil {
				t.Fatalf("GetMeasurement() = %+v, %v; want an error and no measurement", measurement, err)
			}
		})
	}
}

// TestContextUsageOutputAccounting 检查整组输出只采用一次实际用量，工具结果和补齐片段仍单独计数。
func TestContextUsageOutputAccounting(t *testing.T) {
	// 1. 准备完整输出、思考退出、缺少细分以及部分输出的样例。
	for _, tt := range []struct {
		name                 string
		mode                 llmkit.ReasoningContext
		reasoningTokens      int
		omitReasoning        bool
		encryptedOnly        bool
		interruptedReasoning bool
		partialCall          bool
		newUser              bool
		toolImage            bool
		wantOutput           int
		localOutput          bool
	}{
		{
			name:            "all_turns_multiple_parts",
			mode:            llmkit.ReasoningContextAllTurns,
			reasoningTokens: 4000,
			wantOutput:      5000,
		},
		{
			name:            "unknown_retains_reasoning",
			reasoningTokens: 4000,
			newUser:         true,
			wantOutput:      5000,
		},
		{
			name:            "current_tool_loop_retains_reasoning",
			mode:            llmkit.ReasoningContextCurrentTurn,
			reasoningTokens: 4000,
			wantOutput:      5000,
		},
		{
			name:            "current_new_user_excludes_reasoning",
			mode:            llmkit.ReasoningContextCurrentTurn,
			reasoningTokens: 4000,
			newUser:         true,
			wantOutput:      1000,
		},
		{
			name:            "current_tool_image_is_user",
			mode:            llmkit.ReasoningContextCurrentTurn,
			reasoningTokens: 4000,
			toolImage:       true,
			wantOutput:      1000,
		},
		{
			name:        "zero_reasoning_detail_falls_back",
			mode:        llmkit.ReasoningContextCurrentTurn,
			newUser:     true,
			localOutput: true,
		},
		{
			name:            "invalid_reasoning_detail_falls_back",
			mode:            llmkit.ReasoningContextCurrentTurn,
			reasoningTokens: 6000,
			newUser:         true,
			localOutput:     true,
		},
		{
			name:            "unreturned_reasoning_is_not_input",
			omitReasoning:   true,
			reasoningTokens: 4000,
			wantOutput:      1000,
		},
		{
			name:            "chat_drops_ciphertext",
			encryptedOnly:   true,
			reasoningTokens: 4000,
			wantOutput:      1000,
		},
		{
			name:          "text_only_zero_reasoning_uses_output",
			omitReasoning: true,
			wantOutput:    5000,
		},
		{
			name:                 "interrupted_reasoning_excluded",
			interruptedReasoning: true,
			reasoningTokens:      4000,
			wantOutput:           1000,
		},
		{
			name:            "partial_tool_output_falls_back",
			partialCall:     true,
			reasoningTokens: 4000,
			localOutput:     true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := contextSource("input", "原输入")
			history := compactionContext(t, 100000, "", &compactClient{}, input)
			if tt.encryptedOnly {
				model := newTestModel(&llmkit.Target{Protocol: llmkit.ProtocolOpenAIChat}, 100000)
				var err error
				history, err = agent_context.NewAgentContext(agent_context.NewAgentContextOpt(contextSessionID, contextTurnID, &compactClient{}, model, model))
				if err != nil {
					t.Fatal(err)
				}
				if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{input}}); err != nil {
					t.Fatal(err)
				}
			}
			baseline := getContextMeasurement(t, history, nil)
			history.RecordRequestUsage(&defs.TurnContextUsage{
				ModelCallID:        contextModelCallID,
				InputThroughItemID: input.ID,
				ReasoningContext:   tt.mode,
				Usage: defs.TokenUsage{
					Input:     9000,
					Output:    5000,
					Reasoning: tt.reasoningTokens,
					CacheRead: 2000,
				},
			})
			if got := getContextMeasurement(t, history, nil); !got.IsUsageBased || got.RequestTokens != 9000 {
				t.Fatalf("before output = %+v, want only actual input", got)
			}
			var output []*defs.Item
			if !tt.omitReasoning {
				for _, id := range []defs.ItemID{"thought-a", "thought-b"} {
					status := defs.ReasoningCompleted
					if tt.interruptedReasoning {
						status = defs.ReasoningInterrupted
					}
					reasoning := &defs.Reasoning{
						Status:  status,
						Content: []string{"思考下一步 "},
					}
					if tt.encryptedOnly {
						reasoning.Content = nil
						reasoning.Encrypted = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 4028)))
					}
					output = append(output, &defs.Item{
						ID:          id,
						TurnID:      contextTurnID,
						ModelCallID: contextModelCallID,
						Kind:        defs.ItemReasoning,
						Reasoning:   reasoning,
					})
				}
			}
			text := contextMessage(string(contextSessionID), defs.IdentityAgent, "回答正文")
			text.ID = "answer"
			call := contextToolCall("call-real", defs.ToolCallReady)
			call.ID = "call-real"
			missingCall := contextToolCall("call-missing", defs.ToolCallReady)
			missingCall.ID = "call-missing"
			if tt.partialCall {
				missingCall.ToolCall.Status = defs.ToolCallInterrupted
				missingCall.ToolCall.Arguments = `{"partial":`
			}
			result := contextToolResultItem("call-real", contextModelCallID)
			result.ID = "result"
			if tt.toolImage {
				result.ToolResult.Images = []*defs.ImageContent{{
					DataURL: contextPNGDataURL,
					Alt:     "result.png",
				}}
			}
			output = append(output, text, call, missingCall, result)
			if tt.newUser {
				output = append(output, contextSource("new-user", "继续处理"))
			}
			if err := history.AppendItems(output...); err != nil {
				t.Fatal(err)
			}
			// 2. 按最终请求独立计算保留的助手内容，其余新增量必须继续本地计数。
			req, _, err := history.BuildModelRequest(nil)
			if err != nil {
				t.Fatal(err)
			}
			lastUser := -1
			for i, message := range req.Messages {
				if message.Role == llmkit.RoleUser {
					lastUser = i
				}
			}
			assistant := &llmkit.Request{}
			for i, message := range req.Messages {
				if message.Role != llmkit.RoleAssistant {
					continue
				}
				if tt.encryptedOnly || tt.mode == llmkit.ReasoningContextCurrentTurn && i < lastUser {
					message.Content = slices.DeleteFunc(slices.Clone(message.Content), func(block llmkit.ContentBlock) bool { return block.Type == llmkit.ContentTypeThinking })
				}
				assistant.Messages = append(assistant.Messages, message)
			}
			localOutput := requestEstimatedTokens(t, assistant)
			got := getContextMeasurement(t, history, nil)
			wantOutput := tt.wantOutput
			if tt.localOutput {
				wantOutput = localOutput
			}
			want := 9000 + got.EstimatedTokens - baseline.EstimatedTokens - localOutput + wantOutput
			if !got.IsUsageBased || got.RequestTokens != want {
				t.Fatalf("measurement = %+v, want input 9000 + retained output %d + uncovered %d = %d", got, wantOutput, got.EstimatedTokens-baseline.EstimatedTokens-localOutput, want)
			}
		})
	}
}

// TestContextUsageReasoningModeAndWire 检查实际模式按最终 user 位置计数，测量不改变请求内容。
func TestContextUsageReasoningModeAndWire(t *testing.T) {
	// 1. 工具结果、工具图片和延后 hook 分别形成无新 user、图片 user 和 hook user。
	for _, shape := range []string{"tool_result_only", "tool_image", "held_hook"} {
		t.Run(shape, func(t *testing.T) {
			first := &defs.Item{
				ID:          "old-thought",
				TurnID:      contextTurnID,
				ModelCallID: contextModelCallID,
				Kind:        defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Status:  defs.ReasoningCompleted,
					Content: []string{strings.Repeat("old reasoning ", 20)},
				},
			}
			second := first.Clone()
			second.ID, second.ModelCallID = "new-thought", "next-call"
			second.Reasoning.Content = []string{"new reasoning"}
			call := contextToolCall("read", defs.ToolCallReady)
			call.ID = "call"
			result := contextToolResultItem("read", contextModelCallID)
			result.ID = "result"
			items := []*defs.Item{contextSource("input", "read"), first, call}
			if shape == "held_hook" {
				hook := contextHookRun(defs.HookRun{
					Event:             defs.HookEventPostToolUse,
					CallID:            "read",
					AdditionalContext: "lint result",
				})
				hook.ID = "hook"
				items = append(items, hook)
			}
			if shape == "tool_image" {
				result.ToolResult.Images = []*defs.ImageContent{{
					DataURL: contextPNGDataURL,
					Alt:     "tool.png",
				}}
			}
			items = append(items, result, second)
			history := compactionContext(t, 0, "", &compactClient{}, items...)
			before, _, err := history.BuildModelRequest(nil)
			if err != nil {
				t.Fatal(err)
			}
			wireBefore, err := llmkit.NewCodec().EncodeRequest(llmkit.EncodeRequestInput{
				Target:  llmkit.Target{Protocol: llmkit.ProtocolOpenAIResponses},
				Request: *before,
			})
			if err != nil {
				t.Fatal(err)
			}
			all := getContextMeasurement(t, history, nil)
			var excludedTokens int
			for _, mode := range []llmkit.ReasoningContext{llmkit.ReasoningContextAllTurns, llmkit.ReasoningContextCurrentTurn, llmkit.ReasoningContextUnknown} {
				history.RecordRequestUsage(&defs.TurnContextUsage{ReasoningContext: mode})
				got := getContextMeasurement(t, history, nil)
				if got.IsUsageBased {
					t.Fatal("missing usage established a numeric baseline")
				}
				if mode == llmkit.ReasoningContextCurrentTurn && shape != "tool_result_only" {
					if got.EstimatedTokens >= all.EstimatedTokens {
						t.Fatalf("old reasoning was not excluded: %+v", got)
					}
					excludedTokens = all.EstimatedTokens - got.EstimatedTokens
					codec, err := tokenizer.Get(tokenizer.O200kBase)
					if err != nil {
						t.Fatal(err)
					}
					wantExcluded, err := codec.Count(first.Reasoning.Content[0])
					if err != nil {
						t.Fatal(err)
					}
					if excludedTokens != wantExcluded {
						t.Fatalf("excluded %d tokens, want only old reasoning %d", excludedTokens, wantExcluded)
					}
					for _, item := range got.Items {
						if item.ItemID == second.ID && item.Tokens <= 0 {
							t.Fatal("reasoning after final user was excluded")
						}
					}
				} else if got.EstimatedTokens != all.EstimatedTokens {
					t.Fatalf("mode %q inherited old exclusion: got %d want %d", mode, got.EstimatedTokens, all.EstimatedTokens)
				}
				after, _, err := history.BuildModelRequest(nil)
				if err != nil {
					t.Fatal(err)
				}
				wireAfter, err := llmkit.NewCodec().EncodeRequest(llmkit.EncodeRequestInput{
					Target:  llmkit.Target{Protocol: llmkit.ProtocolOpenAIResponses},
					Request: *after,
				})
				if err != nil || !reflect.DeepEqual(wireBefore.Body, wireAfter.Body) {
					t.Fatalf("mode %q changed wire reasoning: %v", mode, err)
				}
			}
			if shape != "tool_result_only" && excludedTokens <= 0 {
				t.Fatal("no measured reasoning difference")
			}
		})
	}
}

// TestContextUsageLoadRestore 检查新上下文在首次测量前恢复最近调用，原输入按实际模式重算。
func TestContextUsageLoadRestore(t *testing.T) {
	// 1. 原输入已有旧思考和较新 user；调用产生思考与正文，随后再收到新 user。
	for _, mode := range []llmkit.ReasoningContext{llmkit.ReasoningContextCurrentTurn, llmkit.ReasoningContextAllTurns, llmkit.ReasoningContextUnknown} {
		for _, restore := range []string{"complete", "without_usage", "missing_input", "zero_input", "later_summary"} {
			t.Run(string(mode)+"/"+restore, func(t *testing.T) {
				old := &defs.Item{
					ID:          "old-reasoning",
					TurnID:      contextTurnID,
					ModelCallID: "earlier-call",
					Kind:        defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:  defs.ReasoningCompleted,
						Content: []string{strings.Repeat("already ignored old reasoning ", 30)},
					},
				}
				input := []*defs.Item{contextSource("first-user", "hello"), old, contextSource("input-end", "new question")}
				thought := old.Clone()
				thought.ID, thought.ModelCallID = "generated-reasoning", contextModelCallID
				thought.Reasoning.Content = []string{strings.Repeat("new generated reasoning ", 20)}
				text := contextMessage(string(contextSessionID), defs.IdentityAgent, "generated answer")
				text.ID = "generated-answer"
				items := append(slices.Clone(input), thought, text, contextSource("new-user", "follow-up"))
				report := &defs.TurnContextUsage{
					ModelCallID:        contextModelCallID,
					InputThroughItemID: "input-end",
					Usage: defs.TokenUsage{
						Input:     9000,
						Output:    5000,
						Reasoning: 4000,
					},
					ReasoningContext: mode,
				}
				wantUsage := restore == "complete"
				switch restore {
				case "without_usage":
					report.Usage = defs.TokenUsage{}
				case "missing_input":
					report.InputThroughItemID = "missing"
				case "zero_input":
					report.Usage.Input = 0
				case "later_summary":
					summary := contextSummary("summary", "first-user", "old prefix replaced")
					summary.ModelCallID = "zz-newer-summary"
					items = append([]*defs.Item{summary}, items[1:]...)
				}
				history := newAgentContext(t)
				if err := history.Load(&agent_context.ContextLoadParam{
					Items:        items,
					ContextUsage: report,
				}); err != nil {
					t.Fatal(err)
				}
				// 2. 第一次测量前没有收到新的 Usage；实际输出只计一次，新 user 继续使用分项估算。
				for range 2 {
					got := getContextMeasurement(t, history, nil)
					want := got.EstimatedTokens
					if wantUsage {
						output := 5000
						if mode == llmkit.ReasoningContextCurrentTurn {
							output = 1000
						}
						want = 9000 + output
						for _, item := range got.Items {
							if item.ItemID == "new-user" {
								want += item.Tokens
							}
						}
					}
					if got.IsUsageBased != wantUsage || got.RequestTokens != want {
						t.Fatalf("measurement = %+v, want usage %v request %d", got, wantUsage, want)
					}
				}
			})
		}
	}
}

// TestContextUsageReplacesMode 检查最新 unknown 覆盖旧模式，完整调用重新定位同一原输入。
func TestContextUsageReplacesMode(t *testing.T) {
	// 1. 原输入中旧思考已位于最新 user 之前。
	for _, withUsage := range []bool{false, true} {
		t.Run(map[bool]string{
			false: "mode_only",
			true:  "actual_input",
		}[withUsage], func(t *testing.T) {
			old := &defs.Item{
				ID:          "thought",
				TurnID:      contextTurnID,
				ModelCallID: "old-call",
				Kind:        defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Status:  defs.ReasoningCompleted,
					Content: []string{strings.Repeat("old thoughts ", 20)},
				},
			}
			history := compactionContext(t, 0, "", &compactClient{}, contextSource("first", "hello"), old, contextSource("end", "question"))
			all := getContextMeasurement(t, history, nil).EstimatedTokens
			for _, mode := range []llmkit.ReasoningContext{llmkit.ReasoningContextCurrentTurn, llmkit.ReasoningContextUnknown, llmkit.ReasoningContextAllTurns} {
				report := &defs.TurnContextUsage{
					ModelCallID:        contextModelCallID,
					InputThroughItemID: "end",
					ReasoningContext:   mode,
				}
				if withUsage {
					report.Usage = defs.TokenUsage{Input: 9000}
				}
				history.RecordRequestUsage(report)
				got := getContextMeasurement(t, history, nil)
				if mode == llmkit.ReasoningContextCurrentTurn {
					if got.EstimatedTokens >= all {
						t.Fatal("current_turn did not exclude old reasoning")
					}
				} else if got.EstimatedTokens != all {
					t.Fatalf("unknown inherited current_turn: %+v", got)
				}
				want := got.EstimatedTokens
				if withUsage {
					want = 9000
				}
				if got.RequestTokens != want || got.IsUsageBased != withUsage {
					t.Fatalf("measurement = %+v, want %d; original input must be rebuilt under mode %q", got, want, mode)
				}
			}
		})
	}
}

// TestContextUsageKeepsModeWithoutBaseline 检查数值基准失效后实际思考模式仍参与本地计数，空报告才恢复 unknown。
func TestContextUsageKeepsModeWithoutBaseline(t *testing.T) {
	// 1. 原输入中的旧思考位于最新 user 之前；current_turn 模式下它不计入。
	for _, tt := range []struct {
		name         string
		invalidate   func(t *testing.T, history agent_context.AgentContext)
		wantExcluded bool
	}{
		{
			name: "summary_appended",
			invalidate: func(t *testing.T, history agent_context.AgentContext) {
				if err := history.AppendItems(contextSummary("summary", "first", "old prefix replaced")); err != nil {
					t.Fatal(err)
				}
			},
			wantExcluded: true,
		},
		{
			name: "report_without_input",
			invalidate: func(t *testing.T, history agent_context.AgentContext) {
				history.RecordRequestUsage(&defs.TurnContextUsage{
					ModelCallID:        contextModelCallID,
					InputThroughItemID: "end",
					ReasoningContext:   llmkit.ReasoningContextCurrentTurn,
				})
			},
			wantExcluded: true,
		},
		{
			name: "nil_report",
			invalidate: func(t *testing.T, history agent_context.AgentContext) {
				history.RecordRequestUsage(nil)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := &defs.Item{
				ID:          "thought",
				TurnID:      contextTurnID,
				ModelCallID: "old-call",
				Kind:        defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Status:  defs.ReasoningCompleted,
					Content: []string{strings.Repeat("old thoughts ", 20)},
				},
			}
			history := compactionContext(t, 0, "", &compactClient{}, contextSource("first", "hello"), old, contextSource("end", "question"))
			all := getContextMeasurement(t, history, nil).EstimatedTokens
			history.RecordRequestUsage(&defs.TurnContextUsage{
				ModelCallID:        contextModelCallID,
				InputThroughItemID: "end",
				ReasoningContext:   llmkit.ReasoningContextCurrentTurn,
				Usage:              defs.TokenUsage{Input: 9000},
			})
			if got := getContextMeasurement(t, history, nil); !got.IsUsageBased || got.EstimatedTokens >= all {
				t.Fatalf("recorded measurement = %+v, want usage based with old reasoning excluded from %d", got, all)
			}
			// 2. 基准失效后只用本地计数；模式保留时旧思考仍不计入，空报告则恢复 unknown。
			tt.invalidate(t, history)
			got := getContextMeasurement(t, history, nil)
			history.RecordRequestUsage(nil)
			unknown := getContextMeasurement(t, history, nil)
			if got.IsUsageBased || got.RequestTokens != got.EstimatedTokens {
				t.Fatalf("measurement = %+v, want local estimate only", got)
			}
			if tt.wantExcluded && got.EstimatedTokens >= unknown.EstimatedTokens {
				t.Fatalf("estimated tokens = %d, want old reasoning excluded from %d", got.EstimatedTokens, unknown.EstimatedTokens)
			}
			if !tt.wantExcluded && got.EstimatedTokens != all {
				t.Fatalf("estimated tokens = %d, want unknown mode %d", got.EstimatedTokens, all)
			}
		})
	}
}

// TestContextUsageImageBudgetAdjustment 检查新增图片挤掉原输入旧图时，只调整发生变化的输入。
func TestContextUsageImageBudgetAdjustment(t *testing.T) {
	// 1. 原输入含 20 张图片，新输入使旧图退出最近 20 张；覆盖运行中追加和直接 Load 恢复。
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{
			false: "append",
			true:  "load",
		}[restore], func(t *testing.T) {
			old := contextSource("old-images", "old images")
			for range 20 {
				old.Message.Content = append(old.Message.Content, &defs.Content{
					Kind: defs.ContentImage,
					Image: &defs.ImageContent{
						DataURL: contextPNGDataURL,
						Alt:     "old.png",
					},
				})
			}
			latest := contextUserImageMessage("new image", "latest.png")
			latest.ID = "latest-image"
			history := compactionContext(t, 0, "", &compactClient{}, old)
			baseline := getContextMeasurement(t, history, nil).EstimatedTokens
			report := &defs.TurnContextUsage{
				ModelCallID:        contextModelCallID,
				InputThroughItemID: old.ID,
				Usage:              defs.TokenUsage{Input: 100000},
			}
			if restore {
				if err := history.Load(&agent_context.ContextLoadParam{
					Items:        []*defs.Item{old, latest},
					ContextUsage: report,
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				history.RecordRequestUsage(report)
				getContextMeasurement(t, history, nil)
				if err := history.AppendItems(latest); err != nil {
					t.Fatal(err)
				}
			}
			// 2. 完整请求中的旧图替换与新图输入共同决定差值，不重套截断前缀的图片预算。
			got := getContextMeasurement(t, history, nil)
			want := 100000 + got.EstimatedTokens - baseline
			if !got.IsUsageBased || got.RequestTokens != want {
				t.Fatalf("measurement = %+v, want %d", got, want)
			}
			if got.Items[0].Tokens >= baseline {
				t.Fatal("old image was not replaced in original input")
			}
		})
	}
}

// TestContextUsageNegativeAdjustmentFallsBack 检查输入变化抵消了全部实际输入时退回本地估算。
func TestContextUsageNegativeAdjustmentFallsBack(t *testing.T) {
	// 1. 服务端上报很小的输入，后来的 user 令大量旧思考退出。
	for _, tt := range []struct {
		name  string
		input int
	}{{
		name:  "one_token",
		input: 1,
	}, {
		name:  "two_tokens",
		input: 2,
	}} {
		t.Run(tt.name, func(t *testing.T) {
			thought := &defs.Item{
				ID:          "thought",
				TurnID:      contextTurnID,
				ModelCallID: "old-call",
				Kind:        defs.ItemReasoning,
				Reasoning: &defs.Reasoning{
					Status:  defs.ReasoningCompleted,
					Content: []string{strings.Repeat("large old reasoning ", 200)},
				},
			}
			history := compactionContext(t, 0, "", &compactClient{}, contextSource("user", "first"), thought)
			history.RecordRequestUsage(&defs.TurnContextUsage{
				ModelCallID:        contextModelCallID,
				InputThroughItemID: thought.ID,
				ReasoningContext:   llmkit.ReasoningContextCurrentTurn,
				Usage:              defs.TokenUsage{Input: tt.input},
			})
			getContextMeasurement(t, history, nil)
			if err := history.AppendItems(contextSource("new-user", "next")); err != nil {
				t.Fatal(err)
			}
			got := getContextMeasurement(t, history, nil)
			if got.IsUsageBased || got.RequestTokens != got.EstimatedTokens || got.RequestTokens <= 0 {
				t.Fatalf("negative adjustment must use positive local estimate, got %+v", got)
			}
		})
	}
}
