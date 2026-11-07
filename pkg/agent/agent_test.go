package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
)

const testSessionID defs.SessionID = "0195a6e4-8b73-7cc2-9a40-2d3e4f5a6b7c"
const testTurnID defs.TurnID = "0195a6e4-8b73-7cc2-9a40-2d3e4f5a6b7d"

// testModel 提供不指定服务地址的共用测试模型。
var testModel = &defs.ModelDefinition{
	Ref: defs.ModelRef{
		ProviderID: "test",
		ModelID:    "test",
	},
	Target: &llmkit.Target{},
}

// newTestModel 创建指定连接参数和窗口的测试模型。
func newTestModel(target *llmkit.Target, window int) *defs.ModelDefinition {
	// 1. 填写测试模型身份、请求目标和窗口大小。
	return &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    target.Model,
		},
		Target:        target,
		ContextWindow: window,
	}
}

// TestAgentKeepsReasoningInItsModelCall 检查两次模型调用的推理分别保存在各自输出中。
func TestAgentKeepsReasoningInItsModelCall(t *testing.T) {
	// 1. 创建先调用工具、再给出回复的模型和上下文。
	client := &reasoningAcrossModelCallsClient{}
	history := newTestContext(t, "", []*defs.Item{userMessage("先查再回答")})
	opt := agent.NewAgentOpt(
		testTurnID,
		testSessionID,
		client,
		newTestModel(&llmkit.Target{
			Protocol: llmkit.ProtocolOpenAIChat,
			Model:    "reasoning-model",
		}, 0),
		history,
	)
	opt.ToolExecutor = &testToolExecutor{}
	instance, err := agent.NewAgent(opt)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 运行两步生成，检查推理内容、模型调用 ID 和上下文消息。
	result, err := instance.Run(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	items := result.Items
	for index, request := range client.requests {
		if got := request.Extras["prompt_cache_key"]; got != string(testSessionID) {
			t.Errorf("request %d prompt_cache_key = %v, want %q", index, got, testSessionID)
		}
	}
	if len(items) != 5 || items[0].Reasoning == nil || items[3].Reasoning == nil {
		t.Fatalf("items = %+v, want reasoning, tool call, tool result, reasoning and message", items)
	}
	if !reflect.DeepEqual(items[0].Reasoning.Content, []string{"first reasoning"}) ||
		!reflect.DeepEqual(items[3].Reasoning.Content, []string{"second reasoning"}) {
		t.Fatalf("reasoning contents = %v and %v", items[0].Reasoning.Content, items[3].Reasoning.Content)
	}
	if items[0].ModelCallID == "" || items[3].ModelCallID == "" || items[0].ModelCallID == items[3].ModelCallID {
		t.Fatalf("reasoning model calls = %q and %q", items[0].ModelCallID, items[3].ModelCallID)
	}
	if result.FinalModelCallID != items[4].ModelCallID {
		t.Fatalf("final model call = %q, want %q", result.FinalModelCallID, items[4].ModelCallID)
	}

	messages := contextMessages(t, history)
	if len(messages) != 4 || len(messages[1].ToolCalls) != 1 ||
		messages[3].Role != llmkit.RoleAssistant || len(messages[3].Content) != 2 {
		t.Fatalf("messages = %#v, want user, first assistant, tool result and second assistant", messages)
	}
}

// TestAgentRunCollectsModelResponse 检查真实协议响应中的推理、消息和并行工具调用。
func TestAgentRunCollectsModelResponse(t *testing.T) {
	// 1. 准备 OpenAI Chat 和 Claude 的流式响应及预期 Item、增量。
	tests := []*struct {
		name             string
		protocol         llmkit.Protocol
		response         string
		wantItems        []*defs.Item
		wantError        string
		wantRawReasoning map[string]string
		wantDeltas       map[int][]*agent.AgentItemDelta
	}{
		{
			name:      "chat_reasoning_message_and_parallel_tool_calls",
			wantError: "without an executor",
			protocol:  llmkit.ProtocolOpenAIChat,
			// 模型先输出查询说明，再交错返回两个工具调用的参数。
			response: `data: {"choices":[{"delta":{"reasoning_content":"先确认"}}]}

data: {"choices":[{"delta":{"reasoning_content":"身份与能力。"}}]}

data: {"choices":[{"delta":{"content":"我会查询"}}]}

data: {"choices":[{"delta":{"content":"身份与能力。"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_identity","type":"function","function":{"name":"get_profile","arguments":""}},{"index":1,"id":"call_tools","type":"function","function":{"name":"get_profile","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"section\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"section\":\"identity\"}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"tools\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`,
			wantItems: []*defs.Item{
				{
					Kind: defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:  defs.ReasoningCompleted,
						Summary: []string{"先确认身份与能力。"},
						Content: []string{"先确认身份与能力。"},
					},
				},
				{
					Kind: defs.ItemMessage,
					Message: &defs.Message{
						From: &defs.Address{
							Kind: defs.IdentityAgent,
							ID:   string(testSessionID),
						},
						Content: []*defs.Content{
							{
								Kind: defs.ContentText,
								Text: &defs.TextContent{Text: "我会查询身份与能力。"},
							},
						},
					},
				},
				{
					Kind: defs.ItemToolCall,
					ToolCall: &defs.ToolCall{
						CallID:    "call_identity",
						Name:      "get_profile",
						Arguments: `{"section":"identity"}`,
						Status:    defs.ToolCallReady,
					},
				},
				{
					Kind: defs.ItemToolCall,
					ToolCall: &defs.ToolCall{
						CallID:    "call_tools",
						Name:      "get_profile",
						Arguments: `{"section":"tools"}`,
						Status:    defs.ToolCallReady,
					},
				},
			},
			wantRawReasoning: map[string]string{
				"reasoning_content": "先确认身份与能力。",
			},
			wantDeltas: map[int][]*agent.AgentItemDelta{
				0: {
					{ReasoningSummary: []string{"先确认"}},
					{ReasoningContent: []string{"先确认"}},
					{ReasoningSummary: []string{"身份与能力。"}},
					{ReasoningContent: []string{"身份与能力。"}},
				},
				1: {
					{Text: "我会查询"},
					{Text: "身份与能力。"},
				},
				2: {
					{Arguments: `{"section":"identity"}`},
				},
				3: {
					{Arguments: `{"section":`},
					{Arguments: `"tools"}`},
				},
			},
		},
		{
			name:     "claude_signed_reasoning_and_answer",
			protocol: llmkit.ProtocolClaudeMessages,
			// block stop 携带完整推理和签名，随后分片返回文本回答。
			response: `event: message_start
data: {"type":"message_start","message":{"id":"msg_identity","type":"message","role":"assistant","content":[]}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"根据系统说明，"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"我叫 OpenVaala。"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed-reasoning"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"我是 "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"OpenVaala。"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}

`,
			wantItems: []*defs.Item{
				{
					Kind: defs.ItemReasoning,
					Reasoning: &defs.Reasoning{
						Status:    defs.ReasoningCompleted,
						Content:   []string{"根据系统说明，我叫 OpenVaala。"},
						Encrypted: "signed-reasoning",
					},
				},
				{
					Kind: defs.ItemMessage,
					Message: &defs.Message{
						From: &defs.Address{
							Kind: defs.IdentityAgent,
							ID:   string(testSessionID),
						},
						Content: []*defs.Content{
							{
								Kind: defs.ContentText,
								Text: &defs.TextContent{Text: "我是 OpenVaala。"},
							},
						},
					},
				},
			},
			wantRawReasoning: map[string]string{
				"type":      "thinking",
				"thinking":  "根据系统说明，我叫 OpenVaala。",
				"signature": "signed-reasoning",
			},
			wantDeltas: map[int][]*agent.AgentItemDelta{
				0: {
					{ReasoningContent: []string{"根据系统说明，"}},
					{ReasoningContent: []string{"我叫 OpenVaala。"}},
				},
				1: {
					{Text: "我是 "},
					{Text: "OpenVaala。"},
				},
			},
		},
	}
	// 2. 解码预设协议响应，比较 Item 身份、推理签名、文本和工具参数增量。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				items, events, err := runModelResponse(t, tt.protocol, tt.response)
				if tt.wantError == "" && err != nil || tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
					t.Fatalf("Run error = %v, want %q", err, tt.wantError)
				}
				if len(items) != len(tt.wantItems) {
					t.Fatalf("item count = %d, want %d", len(items), len(tt.wantItems))
				}
				requireSortableItemIDs(t, items)
				modelCallID := items[0].ModelCallID
				if modelCallID == "" {
					t.Fatal("first model output has no ModelCallID")
				}
				for index, item := range items {
					if item.TurnID != testTurnID || item.ModelCallID != modelCallID {
						t.Fatalf("item %d identity = %q/%q, want %q/%q", index, item.TurnID, item.ModelCallID, testTurnID, modelCallID)
					}
				}
				for i, want := range tt.wantItems {
					got := items[i].Clone()
					want = want.Clone()
					want.ID = got.ID
					want.TurnID = got.TurnID
					want.ModelCallID = got.ModelCallID
					if got.Reasoning != nil {
						reasoning := got.Reasoning
						var rawReasoning map[string]string
						if err := json.Unmarshal(reasoning.RawJSON, &rawReasoning); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(rawReasoning, tt.wantRawReasoning) {
							t.Fatalf("raw reasoning = %v, want %v", rawReasoning, tt.wantRawReasoning)
						}
						reasoning.RawJSON = nil
						got.Reasoning = reasoning
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("item %d = %+v (reasoning %+v), want %+v (reasoning %+v)", i, got, got.Reasoning, want, want.Reasoning)
					}
				}
				assertAgentEvents(t, events, items)
				deltas := make(map[int][]*agent.AgentItemDelta)
				for _, event := range events {
					if event.Type == agent.AgentEventItemDelta {
						deltas[event.ItemIndex] = append(deltas[event.ItemIndex], event.Delta)
					}
				}
				if !reflect.DeepEqual(deltas, tt.wantDeltas) {
					got, _ := json.Marshal(deltas)
					want, _ := json.Marshal(tt.wantDeltas)
					t.Fatalf("deltas = %s, want %s", got, want)
				}
			})
	}
}

// runModelResponse 通过本地 HTTP 服务和 llmkit 解码，收集 Agent 输出及事件。
func runModelResponse(t *testing.T, protocol llmkit.Protocol, response string) ([]*defs.Item, []*agent.AgentEvent, error) {
	// 1. 启动返回预设响应的 HTTP 服务，并创建测试 Agent。
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, response)
		}))
	defer server.Close()
	agentContext := newTestContext(t, "你是身份助手。", []*defs.Item{userMessage("你是谁？")})
	instance, err := agent.NewAgent(agent.NewAgentOpt(
		testTurnID,
		testSessionID,
		llmkit.NewClient(llmkit.ClientOptions{}),
		newTestModel(&llmkit.Target{
			Protocol: protocol,
			BaseURL:  server.URL,
			Model:    "identity-model",
		}, 0),
		agentContext,
	))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// 2. 启动事件收集，运行 Agent 并等待事件流读完。
	eventsStream := make(chan *agent.AgentEvent)
	eventsDone := make(chan []*agent.AgentEvent, 1)
	go func() {
		var events []*agent.AgentEvent
		for event := range eventsStream {
			events = append(events, event)
		}
		eventsDone <- events
	}()
	result, err := instance.Run(ctx, eventsStream)
	return result.Items, <-eventsDone, err
}

// TestAgentRunContinuesSharedContext 检查新 Agent 沿用或重新加载同一份历史。
func TestAgentRunContinuesSharedContext(t *testing.T) {
	// 1. 准备事件订阅、重新加载历史和直接追加输入的样例。
	tests := []*struct {
		name          string
		streamEvents  bool
		reloadContext bool
	}{
		{name: "without_event_consumer"},
		{
			name:          "reload_context_from_saved_items",
			reloadContext: true,
		},
		{
			name:         "separate_event_channel_per_agent",
			streamEvents: true,
		},
	}
	// 2. 逐次追加或加载输入并创建新 Agent，检查两次请求的完整历史和返回副本。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				requests := make(chan []map[string]any, 2)
				server := httptest.NewServer(http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						payload := &struct {
							Messages []map[string]any `json:"messages"`
						}{}
						if err := json.NewDecoder(r.Body).Decode(payload); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						requests <- payload.Messages
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"记住了。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`)
					}))
				defer server.Close()
				agentContext := newTestContext(t, "你是记忆助手。", []*defs.Item{userMessage("我叫小林。")})
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var previousOutput *defs.Item
				inputs := []string{"我喜欢 Go。", "我喜欢什么？"}
				for index, input := range inputs {
					var events chan *agent.AgentEvent
					var received chan []*agent.AgentEvent
					if tt.streamEvents {
						events = make(chan *agent.AgentEvent)
						received = make(chan []*agent.AgentEvent, 1)
						go func() {
							var collected []*agent.AgentEvent
							for event := range events {
								collected = append(collected, event)
							}
							received <- collected
						}()
					}
					item := userMessage(input)
					if index > 0 && tt.reloadContext {
						agentContext = newTestContext(t, "你是记忆助手。", []*defs.Item{
							userMessage("我叫小林。"),
							userMessage("我喜欢 Go。"),
							previousOutput,
							item,
						})
					} else if err := agentContext.AppendItems(item); err != nil {
						t.Fatal(err)
					}
					// 为每次输入创建新 Agent，继续使用同一上下文。
					instance, err := agent.NewAgent(agent.NewAgentOpt(
						testTurnID,
						testSessionID,
						llmkit.NewClient(llmkit.ClientOptions{}),
						newTestModel(&llmkit.Target{
							Protocol: llmkit.ProtocolOpenAIChat,
							BaseURL:  server.URL,
							Model:    "memory-model",
						}, 0),
						agentContext,
					))
					if err != nil {
						t.Fatal(err)
					}
					result, err := instance.Run(ctx, events)
					if err != nil {
						t.Fatal(err)
					}
					outputs := result.Items
					if len(outputs) != 1 || outputs[0].Message.Content[0].Text.Text != "记住了。" {
						t.Fatalf("run %d outputs = %+v", index, outputs)
					}
					if received != nil {
						collected := <-received
						assertAgentEvents(t, collected, outputs)
					}
					if outputs[0].Message.From.ID != string(testSessionID) {
						t.Fatalf("output sender = %q, want session %q", outputs[0].Message.From.ID, testSessionID)
					}
					previousOutput = outputs[0].Clone()
					// 修改传入和返回对象，随后检查请求历史仍保留原内容。
					item.Message.Content[0].Text.Text = "篡改输入"
					outputs[0].Message.Content[0].Text.Text = "篡改输出"
				}
				first := <-requests
				second := <-requests
				assertChatMessages(t, first, []string{"system", "user", "user"}, []string{"你是记忆助手。", "我叫小林。", "我喜欢 Go。"})
				assertChatMessages(t, second, []string{"system", "user", "user", "assistant", "user"}, []string{"你是记忆助手。", "我叫小林。", "我喜欢 Go。", "记住了。", "我喜欢什么？"})
				messages := contextMessages(t, agentContext)
				if len(messages) != 6 || messages[5].Role != llmkit.RoleAssistant || messages[5].Content[0].Text != "记住了。" {
					t.Fatalf("context after both runs = %+v", messages)
				}
			})
	}
}

// TestAgentRunRetriesContextAfterFailure 检查模型失败后重试仍使用原始输入。
func TestAgentRunRetriesContextAfterFailure(t *testing.T) {
	// 1. 准备上游失败和工具参数未完成的首个响应。
	tests := []*struct {
		name        string
		status      int
		response    string
		wantPartial bool
	}{
		{
			name:     "upstream_failure_keeps_input",
			status:   http.StatusServiceUnavailable,
			response: `{"error":{"message":"try again"}}`,
		},
		{
			name:   "incomplete_tool_arguments_stay_out_of_history",
			status: http.StatusOK,
			response: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_search","type":"function","function":{"name":"search","arguments":"{"}}]}}]}

`,
			wantPartial: true,
		},
	}
	// 2. 让首次 HTTP 请求失败、第二次成功，检查重试请求相同且上下文只追加成功回复。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				requests := make(chan []map[string]any, 2)
				attempt := 0
				server := httptest.NewServer(http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						payload := &struct {
							Messages []map[string]any `json:"messages"`
						}{}
						if err := json.NewDecoder(r.Body).Decode(payload); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						requests <- payload.Messages
						attempt++
						w.Header().Set("Content-Type", "text/event-stream")
						if attempt == 1 {
							if tt.status == http.StatusOK {
								w.Header().Set("Content-Length", fmt.Sprint(len(tt.response)+100))
							}
							w.WriteHeader(tt.status)
							_, _ = io.WriteString(w, tt.response)
							return
						}
						_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"已恢复。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`)
					}))
				defer server.Close()
				agentContext := newTestContext(t, "", []*defs.Item{userMessage("帮我查一下。")})
				opt := agent.NewAgentOpt(
					testTurnID,
					testSessionID,
					llmkit.NewClient(llmkit.ClientOptions{}),
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						BaseURL:  server.URL,
						Model:    "retry-model",
					}, 0),
					agentContext,
				)
				instance, err := agent.NewAgent(opt)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				events := make(chan *agent.AgentEvent)
				collected := make(chan []*agent.AgentEvent, 1)
				go func() {
					var received []*agent.AgentEvent
					for event := range events {
						received = append(received, event)
					}
					collected <- received
				}()
				result, err := instance.Run(ctx, events)
				if err != nil || result.FinalModelCallID == "" {
					t.Fatalf("Run() = %+v, %v; want recovery in the same Run", result, err)
				}
				assertAgentEvents(t, <-collected, result.Items)
				last := result.Items[len(result.Items)-1]
				if last.Message == nil || last.Message.Content[0].Text.Text != "已恢复。" {
					t.Fatalf("retry output = %+v", result.Items)
				}
				if tt.wantPartial && len(result.Items) != 3 {
					t.Fatalf("items = %+v, want failed tool, error and successful answer", result.Items)
				}
				first, second := <-requests, <-requests
				assertChatMessages(t, first, []string{"user"}, []string{"帮我查一下。"})
				assertChatMessages(t, second, []string{"user"}, []string{"帮我查一下。"})
				if messages := contextMessages(t, agentContext); len(messages) != 2 {
					t.Fatalf("context = %+v, want input and successful answer", messages)
				}
			})
	}
}

// TestAgentRunModelError 检查重试耗尽和取消时保留收到的模型错误。
func TestAgentRunModelError(t *testing.T) {
	// 1. 准备持续模型错误，以及收到错误后取消的样例。
	for _, tt := range []struct {
		name               string
		cancelOnModelError bool
		wantCalls          int
	}{
		{
			name:      "retry_exhaustion_preserves_model_errors",
			wantCalls: 11,
		},
		{
			name:               "consumer_cancellation_preserves_received_model_error",
			cancelOnModelError: true,
			wantCalls:          1,
		},
	} {
		// 2. 运行到重试耗尽或收到模型错误后取消，检查每次尝试都保留错误 Item。
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &sequenceAgentClient{responses: slices.Repeat([][]llmkit.Event{
					{
						{
							Type: llmkit.EventError,
							Error: &llmkit.ErrorPayload{
								Code:    "server_error",
								Message: "model capacity exhausted",
							},
						},
					},
				}, 11)}
				instance, err := agent.NewAgent(agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("请回答。")})))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				events := make(chan *agent.AgentEvent)
				var tasks sync.WaitGroup
				tasks.Go(func() {
					for event := range events {
						if tt.cancelOnModelError && event.Item != nil && event.Item.Kind == defs.ItemModelError {
							cancel()
						}
					}
				})
				result, err := instance.Run(ctx, events)
				tasks.Wait()
				if err == nil || (!tt.cancelOnModelError && !strings.Contains(err.Error(), "model capacity exhausted")) {
					t.Fatalf("Run() error = %v", err)
				}
				if len(result.Items) != tt.wantCalls || len(client.requests) != tt.wantCalls {
					t.Fatalf("items = %d, calls = %d, want %d", len(result.Items), len(client.requests), tt.wantCalls)
				}
				for _, item := range result.Items {
					if item.ModelError == nil || item.ModelError.Code != "server_error" || item.ModelError.Message != "model capacity exhausted" {
						t.Fatalf("error item = %+v", item)
					}
				}
				requireSortableItemIDs(t, result.Items)
			})
		})
	}
}

// TestAgentRunCancellation 检查调用前和生成中取消后的输出及历史。
func TestAgentRunCancellation(t *testing.T) {
	// 1. 准备调用前取消和收到文本后取消的样例。
	tests := []*struct {
		name            string
		cancelBeforeRun bool
		wantOutputs     int
	}{
		{
			name:            "cancelled_before_model_call",
			cancelBeforeRun: true,
		},
		{
			name:        "cancelled_during_text_generation",
			wantOutputs: 1,
		},
	}
	// 2. 在调用前或收到文本时取消，检查返回草稿数量及最终上下文保留的有效输出。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"未完成"}}]}

`)
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}))
				defer server.Close()
				input := userMessage("写一段话。")
				agentContext := newTestContext(t, "", []*defs.Item{input})
				instance, err := agent.NewAgent(agent.NewAgentOpt(
					testTurnID,
					testSessionID,
					llmkit.NewClient(llmkit.ClientOptions{}),
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						BaseURL:  server.URL,
						Model:    "cancel-model",
					}, 0),
					agentContext,
				))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if tt.cancelBeforeRun {
					cancel()
				}
				events := make(chan *agent.AgentEvent)
				eventsDone := make(chan struct{})
				var finalEvent *agent.AgentEvent
				go func() {
					defer close(eventsDone)
					for event := range events {
						finalEvent = event
						if event.Type == agent.AgentEventItemDelta && event.Delta.Text != "" {
							cancel()
						}
					}
				}()
				result, err := instance.Run(ctx, events)
				<-eventsDone
				if err == nil || len(result.Items) != tt.wantOutputs {
					t.Fatalf("Run() = %+v, %v; want %d outputs and cancellation", result.Items, err, tt.wantOutputs)
				}
				// 检查取消后保留原输入和已生成的有效文本，最终测量与重建历史相同。
				rebuilt := newTestContext(t, "", append([]*defs.Item{input}, result.Items...))
				if messages := contextMessages(t, agentContext); !reflect.DeepEqual(messages, contextMessages(t, rebuilt)) {
					t.Fatalf("context after cancellation = %+v, want input and %d retained outputs", messages, tt.wantOutputs)
				}
				wantMeasurement, err := rebuilt.GetMeasurement(nil)
				if err != nil {
					t.Fatal(err)
				}
				if finalEvent == nil || finalEvent.Type != agent.AgentEventContextMeasured || !reflect.DeepEqual(finalEvent.Measurement, wantMeasurement) {
					t.Fatalf("final event = %+v, want retained history measurement %+v", finalEvent, wantMeasurement)
				}
			})
	}
}

// TestAgentRunInterruptedItems 检查推理、工具调用和消息草稿的中断事件。
func TestAgentRunInterruptedItems(t *testing.T) {
	// 1. 准备不同类型的部分输出及空白消息。
	tests := []struct {
		name                  string
		events                []llmkit.Event
		cancelAfterCompletion bool
		wantMeasurementGrowth bool
		wantItem              func(*defs.Item) bool
	}{
		{
			name: "reasoning_is_saved_as_interrupted",
			events: []llmkit.Event{{
				Type:      llmkit.EventReasoningContentDelta,
				Reasoning: &llmkit.ReasoningContent{Content: []string{"partial"}},
			}},
			wantItem: func(item *defs.Item) bool {
				return item != nil && item.Reasoning.Status == defs.ReasoningInterrupted && item.Reasoning.Content[0] == "partial"
			},
		},
		{
			name: "tool_call_is_saved_as_interrupted",
			events: []llmkit.Event{
				{
					Type: llmkit.EventToolCallStart,
					ToolCall: &llmkit.StreamingToolCall{
						CallID: "call-partial",
						Name:   "lookup",
					},
				},
				{
					Type: llmkit.EventToolCallArgumentsDelta,
					ToolCall: &llmkit.StreamingToolCall{
						CallID:    "call-partial",
						Arguments: "{",
					},
				},
			},
			wantItem: func(item *defs.Item) bool {
				return item != nil && item.ToolCall.Status == defs.ToolCallInterrupted && item.ToolCall.CallID == "call-partial" && item.ToolCall.Arguments == "{"
			},
		},
		{
			name: "completed_tool_call_is_retained_on_cancellation",
			events: []llmkit.Event{{
				Type: llmkit.EventToolCallEnd,
				ToolCall: &llmkit.StreamingToolCall{
					CallID:    "call-complete",
					Name:      "lookup",
					Arguments: `{"q":"x"}`,
				},
			}},
			cancelAfterCompletion: true,
			wantMeasurementGrowth: true,
			wantItem: func(item *defs.Item) bool {
				return item != nil && item.ToolCall.Status == defs.ToolCallReady && item.ToolCall.CallID == "call-complete" && item.ToolCall.Arguments == `{"q":"x"}`
			},
		},
		{
			name: "message_draft_with_text_is_saved",
			events: []llmkit.Event{{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: "partial"},
			}},
			wantMeasurementGrowth: true,
			wantItem: func(item *defs.Item) bool {
				return item != nil && item.Kind == defs.ItemMessage && item.Message.Content[0].Text.Text == "partial"
			},
		},
		{
			name: "whitespace_message_draft_is_not_saved",
			events: []llmkit.Event{{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: " \n"},
			}},
			wantItem: func(item *defs.Item) bool {
				return item == nil
			},
		},
	}
	// 2. 收到输出增量后取消，检查推理和调用标记中断、空白消息事件不带 Item。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &blockingAgentClient{events: tt.events}
			input := userMessage("开始。")
			agentContext := newTestContext(t, "", []*defs.Item{input})
			instance, err := agent.NewAgent(agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, agentContext))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			events := make(chan *agent.AgentEvent)
			collected := make(chan []*agent.AgentEvent, 1)
			go func() {
				var received []*agent.AgentEvent
				for event := range events {
					received = append(received, event)
					// 收到指定的输出事件后取消，继续收集后续结束事件。
					if (!tt.cancelAfterCompletion && event.Type == agent.AgentEventItemDelta) || (tt.cancelAfterCompletion && event.Type == agent.AgentEventItemCompleted) {
						cancel()
					}
				}
				collected <- received
			}()
			result, err := instance.Run(ctx, events)
			received := <-collected
			if !errors.Is(err, context.Canceled) || result.FinalModelCallID != "" || len(result.Items) != 1 {
				t.Fatalf("Run() = %+v, %v; want one draft and cancellation", result, err)
			}
			calls := getAgentEventsOfType(received, agent.AgentEventModelCallFinished)
			if len(calls) != 1 || calls[0].ModelCall.ContextUsage != nil || calls[0].ModelCall.ModelCallID == "" {
				t.Fatalf("cancelled model calls = %+v, want one call with identity and no context usage", calls)
			}
			wantTerminalType := agent.AgentEventItemInterrupted
			if tt.cancelAfterCompletion {
				wantTerminalType = agent.AgentEventItemCompleted
			}
			terminalItems := getAgentEventsOfType(received, wantTerminalType)
			if len(terminalItems) != 1 {
				t.Fatalf("terminal item events = %d, want 1", len(terminalItems))
			}
			last := terminalItems[len(terminalItems)-1]
			if last.ItemIndex != 0 || !tt.wantItem(last.Item) {
				t.Fatalf("last event = %+v (item %+v)", last, last.Item)
			}
			assertAgentEvents(t, received, result.Items)
			// 3. 用实际保存的终态输出重建历史，检查取消前的有效文本和完整工具调用进入最终测量。
			retained := []*defs.Item{input}
			if last.Item != nil {
				retained = append(retained, last.Item)
			}
			retained, err = defs.SelectContextHistoryItems(retained)
			if err != nil {
				t.Fatal(err)
			}
			rebuilt := newTestContext(t, "", retained)
			wantMeasurement, err := rebuilt.GetMeasurement(nil)
			if err != nil {
				t.Fatal(err)
			}
			final := received[len(received)-1]
			if final.Type != agent.AgentEventContextMeasured || !reflect.DeepEqual(final.Measurement, wantMeasurement) {
				t.Fatalf("final measurement = %+v, want rebuilt history measurement %+v", final.Measurement, wantMeasurement)
			}
			initial := getAgentEventsOfType(received, agent.AgentEventContextMeasured)[0].Measurement
			if (final.Measurement.RequestTokens > initial.RequestTokens) != tt.wantMeasurementGrowth {
				t.Fatalf("tokens before/after cancellation = %d/%d, want growth=%v", initial.RequestTokens, final.Measurement.RequestTokens, tt.wantMeasurementGrowth)
			}
			if messages := contextMessages(t, agentContext); !reflect.DeepEqual(messages, contextMessages(t, rebuilt)) {
				t.Fatalf("context after cancellation = %+v, want rebuilt retained history", messages)
			}
		})
	}
}

// TestAgentInputRequests 检查生成前和结束前读取补充输入。
func TestAgentInputRequests(t *testing.T) {
	// 1. 准备空输入、结束前补充输入和输入保存失败的回复。
	supplement := userMessage("补充说明")
	tests := []struct {
		name         string
		replies      []agent.AgentInputResult
		wantRequests []bool
		wantCalls    int
		wantErr      string
	}{
		{
			name:         "no_supplementary_input",
			replies:      []agent.AgentInputResult{{}, {}},
			wantRequests: []bool{false, true},
			wantCalls:    1,
		},
		{
			name:         "input_before_finish_continues_model",
			replies:      []agent.AgentInputResult{{}, {Items: []*defs.Item{supplement}}, {}, {}},
			wantRequests: []bool{false, true, false, true},
			wantCalls:    2,
		},
		{
			name:         "input_error_stops_run",
			replies:      []agent.AgentInputResult{{Err: errors.New("save steer failed")}},
			wantRequests: []bool{false},
			wantErr:      "save steer failed",
		},
	}
	// 2. 逐次回复输入请求，检查结束标记、模型调用次数及补充文本进入下一次请求。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &textAgentClient{}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("开始。")}))
			inputRequests := make(chan agent.AgentInputRequest)
			opt.InputRequests = inputRequests
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type runOutput struct {
				result agent.AgentResult
				err    error
			}
			done := make(chan runOutput, 1)
			go func() {
				result, err := instance.Run(ctx, nil)
				done <- runOutput{
					result,
					err,
				}
			}()
			var gotRequests []bool
			for _, reply := range tt.replies {
				request := <-inputRequests
				gotRequests = append(gotRequests, request.FinishIfEmpty)
				request.Reply <- reply
			}
			output := <-done
			if tt.wantErr != "" {
				if output.err == nil || !strings.Contains(output.err.Error(), tt.wantErr) {
					t.Fatalf("Run() error = %v, want %q", output.err, tt.wantErr)
				}
			} else if output.err != nil || output.result.FinalModelCallID == "" {
				t.Fatalf("Run() = %+v, %v", output.result, output.err)
			}
			if !reflect.DeepEqual(gotRequests, tt.wantRequests) || len(client.requests) != tt.wantCalls {
				t.Fatalf("input requests = %v, model calls = %d; want %v, %d", gotRequests, len(client.requests), tt.wantRequests, tt.wantCalls)
			}
			if tt.wantCalls == 2 {
				payload, _ := json.Marshal(client.requests[1].Messages)
				if !strings.Contains(string(payload), "补充说明") {
					t.Fatalf("second request lost supplementary input: %s", payload)
				}
			}
		})
	}
}

// TestAgentRequestsStreamUsage 检查普通模型请求的流式用量选项。
func TestAgentRequestsStreamUsage(t *testing.T) {
	// 1. 准备 OpenAI Chat、Responses 和 Claude 的预期流选项。
	tests := []struct {
		name     string
		protocol llmkit.Protocol
		want     map[string]any
	}{
		{
			name:     "openai_chat_requests_usage",
			protocol: llmkit.ProtocolOpenAIChat,
			want:     map[string]any{"include_usage": true},
		},
		{
			name:     "openai_responses_unchanged",
			protocol: llmkit.ProtocolOpenAIResponses,
		},
		{
			name:     "claude_unchanged",
			protocol: llmkit.ProtocolClaude,
		},
	}
	// 2. 分别运行各协议，检查客户端收到的 StreamOptions。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &textAgentClient{}
			instance, err := agent.NewAgent(agent.NewAgentOpt(
				testTurnID, testSessionID, client,
				newTestModel(&llmkit.Target{
					Protocol: tt.protocol,
					Model:    "model",
				}, 0),
				newTestContext(t, "", []*defs.Item{userMessage("开始。")}),
			))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := instance.Run(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			if len(client.requests) != 1 {
				t.Fatalf("model calls = %d, want 1", len(client.requests))
			}
			if got := client.requests[0].StreamOptions; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("stream options = %v, want %v", got, tt.want)
			}
		})
	}
}

// blockingAgentClient 发送给定模型事件后保持流打开，模拟生成中途被取消。
type blockingAgentClient struct {
	events []llmkit.Event
}

// Call 返回发送预设事件后保持打开的事件流。
func (c *blockingAgentClient) Call(context.Context, llmkit.Request, llmkit.Target, llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 将预设事件放入缓冲通道，不关闭事件流。
	stream := make(chan llmkit.Event, len(c.events))
	for _, event := range c.events {
		stream <- event
	}
	return stream, nil
}

// textAgentClient 每次调用都返回一条完整文本回答，并记录请求。
type textAgentClient struct {
	requests []llmkit.Request
}

// Call 记录请求并返回完整文本回答。
func (c *textAgentClient) Call(_ context.Context, req llmkit.Request, _ llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 保存请求，发送文本和 stop 结束事件后关闭通道。
	c.requests = append(c.requests, req)
	stream := make(chan llmkit.Event, 2)
	stream <- llmkit.Event{
		Type:  llmkit.EventContentDelta,
		Delta: &llmkit.DeltaPayload{Text: "answer"},
	}
	stream <- llmkit.Event{
		Type:         llmkit.EventDone,
		FinishReason: "stop",
	}
	close(stream)
	return stream, nil
}

// newTestContext 创建已加载指令和历史的测试上下文。
func newTestContext(t *testing.T, system string, items []*defs.Item) agent_context.AgentContext {
	// 1. 创建测试上下文，再加载指令和传入 Item。
	t.Helper()
	agentContext, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, llmkit.NewClient(llmkit.ClientOptions{}), testModel, testModel))
	if err != nil {
		t.Fatal(err)
	}
	if err := agentContext.Load(&agent_context.ContextLoadParam{
		System: system,
		Items:  items,
	}); err != nil {
		t.Fatal(err)
	}
	return agentContext
}

// contextMessages 通过公开模型请求读取可见历史。
func contextMessages(t *testing.T, agentContext agent_context.AgentContext) []llmkit.Message {
	// 1. 组装无工具的请求并返回消息列表。
	t.Helper()
	req, _, err := agentContext.BuildModelRequest(nil)
	if err != nil {
		t.Fatal(err)
	}
	return req.Messages
}

// newTestEnvironment 创建指定 ID、工作区和工具的执行环境。
func newTestEnvironment(t *testing.T, id string, environmentWorkspace workspace.Workspace, tools ...executor.Tool) executor.ExecutionEnvironment {
	// 1. 保存环境 ID、可为空的工作区和工具。
	t.Helper()
	environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(id, "", environmentWorkspace, tools...))
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

// newTestToolExecutor 创建使用指定环境的测试工具执行器。
func newTestToolExecutor(t *testing.T, environments ...executor.ExecutionEnvironment) executor.ToolExecutor {
	// 1. 将环境列表交给 ToolExecutor 构造函数。
	t.Helper()
	toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(environments))
	if err != nil {
		t.Fatal(err)
	}
	return toolExecutor
}

// userMessage 创建本轮用户文本输入。
func userMessage(text string) *defs.Item {
	// 1. 填写测试 Turn、用户发送方和文本块。
	return &defs.Item{
		TurnID: testTurnID,
		Kind:   defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: defs.IdentityUser,
				ID:   "user",
			},
			Content: []*defs.Content{
				{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: text},
				},
			},
		},
	}
}

// requireSortableItemIDs 检查输出 Item ID 是递增的 UUIDv7。
func requireSortableItemIDs(t *testing.T, items []*defs.Item) {
	// 1. 依次检查 Item 非空、UUID 版本和 ID 排序。
	t.Helper()
	previous := ""
	for index, item := range items {
		if item == nil {
			t.Fatalf("item %d is nil", index)
		}
		value := string(item.ID)
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.Version() != 7 {
			t.Fatalf("item %d ID %q is not UUIDv7: %v", index, value, err)
		}
		if previous != "" && value <= previous {
			t.Fatalf("item %d ID %q is not after %q", index, value, previous)
		}
		previous = value
	}
}

// assertChatMessages 检查 Chat 请求的消息角色和文本。
func assertChatMessages(t *testing.T, messages []map[string]any, roles, contents []string) {
	// 1. 检查消息数量，再逐条读取字符串或内容块文本并比较角色及内容。
	t.Helper()
	if len(messages) != len(roles) {
		t.Fatalf("messages = %+v, want roles %v", messages, roles)
	}
	for index, message := range messages {
		content := message["content"]
		// 将字符串或内容块数组统一读为文本。
		if blocks, ok := content.([]any); ok {
			text := ""
			for _, block := range blocks {
				text += fmt.Sprint(block.(map[string]any)["text"])
			}
			content = text
		}
		if message["role"] != roles[index] || content != contents[index] {
			t.Fatalf("message %d = %+v, want %s %q", index, message, roles[index], contents[index])
		}
	}
}

// assertAgentEvents 用开始、增量和结束事件重建 Item，并与返回输出比较。
func assertAgentEvents(t *testing.T, events []*agent.AgentEvent, outputs []*defs.Item) {
	// 1. 检查 Item 位置和结束状态，依次应用开始、增量及结束事件。
	t.Helper()
	var collected []*defs.Item
	ended := make(map[int]bool)
	for _, event := range getItemEvents(events) {
		if event.ItemIndex < 0 || event.ItemIndex > len(collected) || ended[event.ItemIndex] {
			t.Fatalf("event targets missing or ended item: %+v", event)
		}
		switch event.Type {
		case agent.AgentEventItemStarted:
			if event.ItemIndex != len(collected) || event.Item == nil || event.Delta != nil {
				t.Fatalf("invalid start event: %+v", event)
			}
			collected = append(collected, event.Item.Clone())
		case agent.AgentEventItemDelta:
			if event.ItemIndex == len(collected) || event.Item != nil || event.Delta == nil {
				t.Fatalf("invalid delta event: %+v", event)
			}
			item := collected[event.ItemIndex]
			switch item.Kind {
			case defs.ItemMessage:
				item.Message.Content[0].Text.Text += event.Delta.Text
			case defs.ItemReasoning:
				item.Reasoning.Summary = append(item.Reasoning.Summary, event.Delta.ReasoningSummary...)
				item.Reasoning.Content = append(item.Reasoning.Content, event.Delta.ReasoningContent...)
			case defs.ItemToolCall:
				item.ToolCall.Arguments += event.Delta.Arguments
			default:
				t.Fatalf("unexpected delta for item kind %s", item.Kind)
			}
		case agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
			if event.ItemIndex == len(collected) || event.Delta != nil {
				t.Fatalf("invalid end event: %+v", event)
			}
			// 空白中断消息没有 Item，保留按增量重建的草稿。
			if event.Item == nil {
				if event.Type != agent.AgentEventItemInterrupted || collected[event.ItemIndex].Kind != defs.ItemMessage {
					t.Fatalf("end event without item: %+v", event)
				}
			} else {
				collected[event.ItemIndex] = event.Item
			}
			ended[event.ItemIndex] = true
		default:
			t.Fatalf("unexpected event: %+v", event)
		}
	}
	// 2. 比较结束位置数量及重建内容与完整输出。
	if len(ended) != len(outputs) || !reflect.DeepEqual(collected, outputs) {
		t.Fatalf("events reconstruct %+v with %d ended items, want output %+v", collected, len(ended), outputs)
	}
}

// getItemEvents 返回去掉不对应 Item 的事件后的新切片。
func getItemEvents(events []*agent.AgentEvent) []*agent.AgentEvent {
	// 1. 复制切片，删去上下文测量、模型调用结束和 hook 运行事件。
	return slices.DeleteFunc(slices.Clone(events), func(event *agent.AgentEvent) bool {
		return event.Type == agent.AgentEventContextMeasured || event.Type == agent.AgentEventModelCallFinished || event.Type == agent.AgentEventHookRun
	})
}

// TestAgentEventDeltasAndCompletion 检查文本、拒绝回复的增量和完成内容互不共享。
func TestAgentEventDeltasAndCompletion(t *testing.T) {
	// 1. 准备文本和拒绝回复的分片响应。
	tests := []*struct {
		name       string
		response   string
		wantDeltas []*agent.AgentItemDelta
		wantText   string
	}{
		{
			name: "text_streams_deltas_then_complete_snapshot",
			response: `data: {"choices":[{"delta":{"content":"你好"}}]}

data: {"choices":[{"delta":{"content":"。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`,
			wantDeltas: []*agent.AgentItemDelta{
				{Text: "你好"},
				{Text: "。"},
			},
			wantText: "你好。",
		},
		{
			name: "refusal_streams_as_message_text",
			response: `data: {"choices":[{"delta":{"refusal":"无法"}}]}

data: {"choices":[{"delta":{"refusal":"处理。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`,
			wantDeltas: []*agent.AgentItemDelta{
				{Text: "无法"},
				{Text: "处理。"},
			},
			wantText: "无法处理。",
		},
	}
	// 2. 收集文本和拒绝内容的增量，比较完整回复，再修改事件内容检查输出副本。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				items, events, err := runModelResponse(t, llmkit.ProtocolOpenAIChat, tt.response)
				if err != nil {
					t.Fatal(err)
				}
				assertAgentEvents(t, events, items)
				var deltas []*agent.AgentItemDelta
				for _, event := range events {
					if event.Type == agent.AgentEventItemDelta {
						deltas = append(deltas, event.Delta)
					}
				}
				if !reflect.DeepEqual(deltas, tt.wantDeltas) {
					t.Fatalf("deltas = %+v, want %+v", deltas, tt.wantDeltas)
				}
				itemEvents := getItemEvents(events)
				started := itemEvents[0]
				completed := itemEvents[len(itemEvents)-1]
				if started.Item.Message.Content[0].Text.Text != "" || completed.Type != agent.AgentEventItemCompleted || completed.Item.Message.Content[0].Text.Text != tt.wantText {
					t.Fatalf("start = %+v, completion = %+v", started, completed)
				}
				started.Item.Message.From.ID = "modified"
				completed.Item.Message.Content[0].Text.Text = "modified"
				if items[0].Message.Content[0].Text.Text != tt.wantText || items[0].Message.From.ID != string(testSessionID) || completed.Item.Message.From.ID != string(testSessionID) {
					t.Fatal("event mutation changed another snapshot or final output")
				}
			})
	}
}

// reasoningAcrossModelCallsClient 第一次回复推理和工具调用，第二次回复推理和回答。
type reasoningAcrossModelCallsClient struct {
	calls    int
	requests []llmkit.Request
}

// Call 分别返回带推理的工具调用和最终回答。
func (c *reasoningAcrossModelCallsClient) Call(
	_ context.Context,
	request llmkit.Request,
	_ llmkit.Target,
	_ llmkit.CallOptions,
) (<-chan llmkit.Event, error) {
	// 1. 记录请求和调用次数。
	c.calls++
	c.requests = append(c.requests, request)
	var events []llmkit.Event
	// 2. 首次返回推理和工具调用，第二次返回完整推理和回答，其他次数返回错误；返回的事件流已关闭。
	switch c.calls {
	case 1:
		events = []llmkit.Event{
			{
				Type:      llmkit.EventReasoningContentDelta,
				Reasoning: &llmkit.ReasoningContent{Content: []string{"first reasoning"}},
			},
			{
				Type: llmkit.EventToolCallDelta,
				Delta: &llmkit.DeltaPayload{ToolCall: &llmkit.ToolCallDelta{
					ID:        "call-reasoning",
					Name:      "lookup",
					Arguments: `{}`,
				}},
			},
			{Type: llmkit.EventDone},
		}
	case 2:
		events = []llmkit.Event{
			{
				Type:      llmkit.EventReasoningContentDelta,
				Reasoning: &llmkit.ReasoningContent{Content: []string{"second reasoning"}},
			},
			{
				Type:            llmkit.EventReasoningDone,
				Reasoning:       &llmkit.ReasoningContent{Content: []string{"second reasoning"}},
				ReasoningStatus: llmkit.ReasoningCompleted,
			},
			{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: "answer"},
			},
			{Type: llmkit.EventDone},
		}
	default:
		return nil, fmt.Errorf("unexpected model call %d", c.calls)
	}
	stream := make(chan llmkit.Event, len(events))
	for _, event := range events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

// TestAgentCompactsContext 检查普通生成前和工具执行后的自动压缩。
func TestAgentCompactsContext(t *testing.T) {
	// 1. 准备大历史、并行工具输出、未达阈值和未订阅事件的样例。
	for _, tt := range []struct {
		name                              string
		window                            int
		largeHistory, toolLoop, nilEvents bool
		wantSummary                       bool
	}{
		{
			"before_first_request",
			1000,
			true,
			false,
			false,
			true,
		},
		{
			"after_parallel_tools",
			1000,
			false,
			true,
			false,
			true,
		},
		{
			"below_threshold",
			1000,
			false,
			false,
			false,
			false,
		},
		{
			"disabled",
			0,
			true,
			false,
			false,
			false,
		},
		{
			"nil_events",
			1000,
			true,
			false,
			true,
			true,
		},
	} {
		// 2. 运行首次生成和工具循环，检查摘要覆盖范围、开始完成事件和后续请求历史。
		t.Run(tt.name, func(t *testing.T) {
			client := &compactingAgentClient{toolLoop: tt.toolLoop}
			instance := newCompactionTestAgent(t, client, tt.window, tt.largeHistory, tt.toolLoop)
			var events chan *agent.AgentEvent
			if !tt.nilEvents {
				events = make(chan *agent.AgentEvent, 64)
			}
			result, err := instance.Run(t.Context(), events)
			if err != nil {
				t.Fatal(err)
			}
			items := result.Items
			summaryCount, summaryIndex := 0, -1
			for index, item := range items {
				if item.Kind == defs.ItemContextSummary {
					summaryCount++
					summaryIndex = index
				}
			}
			want := 0
			if tt.wantSummary {
				want = 1
			}
			if summaryCount != want {
				t.Fatalf("summary count = %d, want %d", summaryCount, want)
			}
			if tt.wantSummary {
				wantIndex := 0
				if tt.toolLoop {
					wantIndex = 5
				}
				if summaryIndex != wantIndex {
					t.Fatalf("summary index = %d, want %d", summaryIndex, wantIndex)
				}
				boundary := defs.ItemID("old")
				if tt.toolLoop {
					boundary = "recent"
				}
				if items[summaryIndex].ContextSummary.CoveredThroughItemID != boundary {
					t.Fatal("unexpected coverage boundary")
				}
			}
			if events != nil {
				// 检查摘要开始和完成事件对应同一 Item 和输出位置。
				var gotTypes, wantTypes []agent.AgentEventType
				if tt.wantSummary {
					wantTypes = []agent.AgentEventType{agent.AgentEventItemStarted, agent.AgentEventItemCompleted}
				}
				for event := range events {
					if event.Item == nil || event.Item.Kind != defs.ItemContextSummary {
						continue
					}
					gotTypes = append(gotTypes, event.Type)
					saved := items[summaryIndex]
					if event.ItemIndex != summaryIndex || event.Item.ID != saved.ID || event.Item.ContextSummary.CoveredThroughItemID != saved.ContextSummary.CoveredThroughItemID {
						t.Fatalf("summary event = %+v, want item %s at index %d", event, saved.ID, summaryIndex)
					}
					if event.Type == agent.AgentEventItemStarted && (event.Item.ContextSummary.Text != "" || event.Item.ModelCallID != "") {
						t.Fatalf("started summary = %+v, want no text or model call yet", event.Item)
					}
					if event.Type == agent.AgentEventItemCompleted && !reflect.DeepEqual(event.Item, saved) {
						t.Fatalf("completed summary = %+v, want %+v", event.Item, saved)
					}
				}
				if !reflect.DeepEqual(gotTypes, wantTypes) {
					t.Fatalf("summary event types = %v, want %v", gotTypes, wantTypes)
				}
			}
			compactRequests := 0
			for _, req := range client.requests {
				if req.Extras["prompt_cache_key"] != string(testSessionID) {
					t.Fatal("cache key changed")
				}
				if req.ToolChoice != nil && req.ToolChoice.Type == "none" {
					compactRequests++
					continue
				}
				if compactRequests == 0 {
					continue
				}
				payload, _ := json.Marshal(req.Messages)
				if strings.Contains(string(payload), "old-history") || !strings.Contains(string(payload), "concise summary") {
					t.Fatalf("normal request did not use summary: %s", payload)
				}
				if tt.toolLoop && len(req.Messages[1].ToolCalls) != 2 {
					t.Fatal("parallel tool group split")
				}
			}
			if compactRequests != want {
				t.Fatalf("compact requests = %d", compactRequests)
			}
		})
	}
}

// TestAgentUsesRequestUsageForNextStepCompaction 检查同一 Turn 的输入用量校正控制下一步压缩。
func TestAgentUsesRequestUsageForNextStepCompaction(t *testing.T) {
	// 1. 准备实测高于本地估算和实测低于本地估算的两步工具循环。
	for _, tt := range []struct {
		name           string
		oldRepeats     int
		inputTokens    int
		wantCompaction bool
	}{
		{"high_usage_compacts_even_when_estimate_is_below_threshold", 60, 1000, true},
		{"low_usage_does_not_compact_when_estimate_crosses_threshold", 180, 100, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			responses := newToolLoopResponses()
			responses[0][1].Usage.PromptTokens = tt.inputTokens
			responses[0] = append([]llmkit.Event{{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: strings.Repeat("intermediate ", 250)},
			}}, responses[0]...)
			if tt.wantCompaction {
				responses = slices.Insert(responses, 1, []llmkit.Event{
					{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "concise summary"}},
					{Type: llmkit.EventDone, FinishReason: "stop"},
				})
			}
			client := &sequenceAgentClient{responses: responses}
			model := newTestModel(&llmkit.Target{Model: "model"}, 1000)
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, model, model))
			if err != nil {
				t.Fatal(err)
			}
			old := userMessage(strings.Repeat("old-history ", tt.oldRepeats))
			old.ID = "old"
			recent := userMessage(strings.Repeat("recent ", 220))
			recent.ID = "recent"
			if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{old, recent}}); err != nil {
				t.Fatal(err)
			}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, model, history)
			opt.ToolExecutor = &testToolExecutor{}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 运行两步模型与工具，检查第一次用量校正及成功输出追加后的占用。
			events := make(chan *agent.AgentEvent, 64)
			result, err := instance.Run(t.Context(), events)
			if err != nil {
				t.Fatal(err)
			}
			var received []*agent.AgentEvent
			for event := range events {
				received = append(received, event)
			}
			measured := getAgentEventsOfType(received, agent.AgentEventContextMeasured)
			if len(measured) != 7 {
				t.Fatalf("measurement events = %d, want 7 across both model steps and exit", len(measured))
			}
			initial, calibrated, appended := measured[0].Measurement, measured[1].Measurement, measured[2].Measurement
			if initial.IsUsageBased || initial.RequestTokens != initial.EstimatedTokens || initial.RequestTokens >= initial.ThresholdTokens {
				t.Fatalf("initial measurement = %+v, want uncalibrated request below threshold", initial)
			}
			if !calibrated.IsUsageBased || calibrated.RequestTokens != tt.inputTokens || calibrated.EstimatedTokens != initial.EstimatedTokens {
				t.Fatalf("calibrated measurement = %+v, want input usage %d and unchanged estimate %d", calibrated, tt.inputTokens, initial.EstimatedTokens)
			}
			wantAppendedTokens := tt.inputTokens + 10
			if !appended.IsUsageBased || appended.RequestTokens < wantAppendedTokens || appended.RequestTokens >= tt.inputTokens+appended.EstimatedTokens-initial.EstimatedTokens || appended.EstimatedTokens <= initial.EstimatedTokens {
				t.Fatalf("measurement after output = %+v, want reported input/output %d plus uncovered fragments, below re-estimated output", appended, wantAppendedTokens)
			}
			if (appended.RequestTokens >= appended.ThresholdTokens) != tt.wantCompaction || (appended.EstimatedTokens < appended.ThresholdTokens) != tt.wantCompaction {
				t.Fatalf("measurement after output = %+v, want corrected and raw estimates on opposite sides of threshold (compact=%v)", appended, tt.wantCompaction)
			}
			// 3. 检查下一步是否先压缩、后续请求内容，以及第二次用量替换第一次基准。
			wantPurposes := []agent.ModelCallPurpose{agent.ModelCallPurposeConversation, agent.ModelCallPurposeConversation}
			if tt.wantCompaction {
				wantPurposes = slices.Insert(wantPurposes, 1, agent.ModelCallPurposeCompaction)
			}
			calls := getAgentEventsOfType(received, agent.AgentEventModelCallFinished)
			var purposes []agent.ModelCallPurpose
			for _, event := range calls {
				purposes = append(purposes, event.ModelCall.Purpose)
			}
			if !slices.Equal(purposes, wantPurposes) || len(client.requests) != len(wantPurposes) || result.Stats.Steps != 2 {
				t.Fatalf("model purposes = %v, requests = %d, steps = %d; want %v and two steps", purposes, len(client.requests), result.Stats.Steps, wantPurposes)
			}
			payload, err := json.Marshal(client.requests[len(client.requests)-1].Messages)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(payload), "concise summary") != tt.wantCompaction || strings.Contains(string(payload), "old-history") == tt.wantCompaction {
				t.Fatalf("last request does not match compaction=%v: %s", tt.wantCompaction, payload)
			}
			beforeSecondCall, secondUsage, final := measured[3].Measurement, measured[4].Measurement, measured[len(measured)-1].Measurement
			if beforeSecondCall.IsUsageBased == tt.wantCompaction || !secondUsage.IsUsageBased || secondUsage.RequestTokens != 200 {
				t.Fatalf("second step measurements: before=%+v, usage=%+v", beforeSecondCall, secondUsage)
			}
			if !final.IsUsageBased || final.RequestTokens != 220 || received[len(received)-1].Type != agent.AgentEventContextMeasured {
				t.Fatalf("final measurement = %+v, want latest input and output usage", final)
			}
		})
	}
}

// compactingAgentClient 是压缩测试用的模型客户端，按 ToolChoice 区分摘要请求和普通请求。
type compactingAgentClient struct {
	requests    []llmkit.Request
	toolLoop    bool
	normalCalls int
	// summaryUsage、normalUsage 非 nil 时，摘要调用和普通调用在结束前上报这份用量。
	summaryUsage, normalUsage *llmkit.Usage
}

// Call 生成压缩测试的模型回复。
func (c *compactingAgentClient) Call(_ context.Context, req llmkit.Request, _ llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 记录请求。
	c.requests = append(c.requests, req)
	// 2. 禁用工具的摘要请求返回固定摘要文本，普通请求按配置返回文本、工具调用和用量。
	events := make(chan llmkit.Event, 8)
	if req.ToolChoice != nil && req.ToolChoice.Type == "none" {
		events <- llmkit.Event{
			Type:  llmkit.EventContentDelta,
			Delta: &llmkit.DeltaPayload{Text: "concise summary"},
		}
		if c.summaryUsage != nil {
			events <- llmkit.Event{
				Type:  llmkit.EventUsage,
				Usage: c.summaryUsage,
			}
		}
	} else {
		c.normalCalls++
		text := "answer"
		if c.toolLoop && c.normalCalls == 1 {
			text = strings.Repeat("intermediate ", 700)
		}
		events <- llmkit.Event{
			Type:  llmkit.EventContentDelta,
			Delta: &llmkit.DeltaPayload{Text: text},
		}
		if c.toolLoop && c.normalCalls == 1 {
			for _, id := range []string{"call_a", "call_b"} {
				events <- llmkit.Event{
					Type: llmkit.EventToolCallStart,
					ToolCall: &llmkit.StreamingToolCall{
						CallID: id,
						Name:   "lookup",
					},
				}
				events <- llmkit.Event{
					Type: llmkit.EventToolCallEnd,
					ToolCall: &llmkit.StreamingToolCall{
						CallID:    id,
						Name:      "lookup",
						Arguments: "{}",
					},
				}
			}
		}
		if c.normalUsage != nil {
			events <- llmkit.Event{
				Type:  llmkit.EventUsage,
				Usage: c.normalUsage,
			}
		}
	}
	// 3. 发送结束事件并关闭事件流。
	events <- llmkit.Event{
		Type:         llmkit.EventDone,
		FinishReason: "stop",
	}
	close(events)
	return events, nil
}

// newCompactionTestAgent 创建可测试大历史或工具循环压缩的 Agent。
func newCompactionTestAgent(t *testing.T, client llmkit.Client, window int, largeHistory, toolLoop bool) agent.Agent {
	// 1. 按样例准备较早和最近输入，创建指定窗口的上下文。
	t.Helper()
	old := userMessage(strings.Repeat("old-history ", 60))
	if largeHistory {
		old.Message.Content[0].Text.Text = strings.Repeat("old-history ", 400)
	}
	old.ID = "old"
	recent := userMessage(strings.Repeat("recent ", 220))
	recent.ID = "recent"
	model := newTestModel(&llmkit.Target{Model: "model"}, window)
	history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, model, model))
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{old, recent}}); err != nil {
		t.Fatal(err)
	}
	// 2. 按样例启用并行工具，再创建 Agent。
	opt := agent.NewAgentOpt(testTurnID, testSessionID, client, model, history)
	if toolLoop {
		opt.ToolExecutor = &testToolExecutor{parallel: true}
	}
	instance, err := agent.NewAgent(opt)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

// TestAgentCompactionFailure 检查摘要失败后保留既有输出并移除摘要草稿。
func TestAgentCompactionFailure(t *testing.T) {
	// 1. 准备首次请求前和工具步骤后的摘要调用失败。
	summaryErr := errors.New("summary model offline")
	for _, tt := range []struct {
		name            string
		largeHistory    bool
		toolLoop        bool
		wantItems       int
		wantNormalCalls int
	}{
		{
			name:         "before_first_request",
			largeHistory: true,
		},
		{
			name:            "after_tool_step",
			toolLoop:        true,
			wantItems:       5,
			wantNormalCalls: 1,
		},
	} {
		// 2. 运行到摘要失败，检查中断事件不带摘要，已有工具输出仍保留。
		t.Run(tt.name, func(t *testing.T) {
			client := &failingCompactionAgentClient{
				compactingAgentClient: compactingAgentClient{toolLoop: tt.toolLoop},
				summaryErr:            summaryErr,
			}
			instance := newCompactionTestAgent(t, client, 1000, tt.largeHistory, tt.toolLoop)
			events := make(chan *agent.AgentEvent, 64)
			result, err := instance.Run(t.Context(), events)
			if !errors.Is(err, summaryErr) || !strings.Contains(err.Error(), "compact agent context") {
				t.Fatalf("Run() error = %v, want compaction failure wrapping %v", err, summaryErr)
			}
			var got []string
			var started, interrupted *agent.AgentEvent
			for event := range events {
				switch {
				case event.Type == agent.AgentEventItemStarted && event.Item.Kind == defs.ItemContextSummary:
					started = event
					got = append(got, "started:context_summary")
				case event.Type == agent.AgentEventItemInterrupted:
					interrupted = event
					got = append(got, "interrupted")
				case event.Type == agent.AgentEventModelCallFinished && event.ModelCall.Purpose == agent.ModelCallPurposeCompaction:
					got = append(got, "model_call_finished(compaction)")
				}
			}
			if want := []string{"started:context_summary", "model_call_finished(compaction)", "interrupted"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("compaction events = %v, want %v", got, want)
			}
			if interrupted.Item != nil || interrupted.ItemIndex != started.ItemIndex || started.ItemIndex != tt.wantItems {
				t.Fatalf("interrupted event = %+v, started event = %+v, want no item to save at index %d", interrupted, started, tt.wantItems)
			}
			if len(result.Items) != tt.wantItems || slices.ContainsFunc(result.Items, func(item *defs.Item) bool { return item.Kind == defs.ItemContextSummary }) {
				t.Fatalf("result items = %+v, want %d items without a context summary", result.Items, tt.wantItems)
			}
			if client.normalCalls != tt.wantNormalCalls {
				t.Fatalf("normal model calls = %d, want %d", client.normalCalls, tt.wantNormalCalls)
			}
		})
	}
}

// failingCompactionAgentClient 让摘要请求直接失败，普通请求照常回答。
type failingCompactionAgentClient struct {
	compactingAgentClient
	summaryErr error
}

// Call 让摘要请求返回预设错误，普通请求使用测试回复。
func (c *failingCompactionAgentClient) Call(ctx context.Context, req llmkit.Request, target llmkit.Target, options llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 检查 ToolChoice，摘要请求失败，普通请求交给内嵌客户端。
	if req.ToolChoice != nil && req.ToolChoice.Type == "none" {
		return nil, c.summaryErr
	}
	return c.compactingAgentClient.Call(ctx, req, target, options)
}

// TestAgentCompactOnly 检查只压缩运行的摘要、测量和目录读取。
func TestAgentCompactOnly(t *testing.T) {
	// 1. 准备长历史和空历史的只压缩样例。
	old := userMessage(strings.Repeat("old-history ", 400))
	old.ID = "old"
	recent := userMessage(strings.Repeat("recent ", 220))
	recent.ID = "recent"
	for _, tt := range []struct {
		name        string
		items       []*defs.Item
		wantSummary bool
	}{
		{
			"summarizes_history_then_measures_compacted_context",
			[]*defs.Item{old, recent},
			true,
		},
		{
			"nothing_to_summarize_still_reports_context",
			nil,
			false,
		},
	} {
		// 2. 运行只压缩 Agent，检查未读取目录或调用普通模型，摘要后发送一次新测量。
		t.Run(tt.name, func(t *testing.T) {
			client := &compactingAgentClient{}
			model := newTestModel(&llmkit.Target{Model: "model"}, 1000)
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, model, model))
			if err != nil {
				t.Fatal(err)
			}
			if err := history.Load(&agent_context.ContextLoadParam{Items: tt.items}); err != nil {
				t.Fatal(err)
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			// 配置规则不可读的默认工作区，检查只压缩运行不预读目录规则。
			offline := &testAgentsWorkspace{
				Workspace: work,
				operation: "metadata",
			}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, model, history)
			opt.Environments = []executor.ExecutionEnvironment{newTestEnvironment(t, "offline", offline)}
			opt.IsCompactOnly = true
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan *agent.AgentEvent, 64)
			result, err := instance.Run(t.Context(), events)
			if err != nil {
				t.Fatal(err)
			}
			var collected []*agent.AgentEvent
			for event := range events {
				collected = append(collected, event)
			}
			if offline.metadataCalls != 0 || client.normalCalls != 0 || result.FinalModelCallID != "" {
				t.Fatalf("rule reads = %d, conversation model calls = %d, final model call = %q; want none", offline.metadataCalls, client.normalCalls, result.FinalModelCallID)
			}
			if !tt.wantSummary {
				if len(client.requests) != 0 || len(result.Items) != 0 || len(collected) != 1 || collected[0].Type != agent.AgentEventContextMeasured {
					t.Fatalf("model requests = %d, events = %+v, items = %d; want only a context measurement", len(client.requests), collected, len(result.Items))
				}
				return
			}
			if len(client.requests) != 1 || client.requests[0].ToolChoice == nil || client.requests[0].ToolChoice.Type != "none" {
				t.Fatalf("model requests = %+v, want one summary request", client.requests)
			}
			if len(result.Items) != 1 || result.Items[0].Kind != defs.ItemContextSummary {
				t.Fatalf("items = %+v, want one context summary", result.Items)
			}
			summary := result.Items[0]
			completedIndex := slices.IndexFunc(collected, func(event *agent.AgentEvent) bool {
				return event.Type == agent.AgentEventItemCompleted && event.Item.Kind == defs.ItemContextSummary
			})
			measured := getAgentEventsOfType(collected, agent.AgentEventContextMeasured)
			if completedIndex < 0 || len(measured) != 1 || collected[len(collected)-1] != measured[0] {
				t.Fatalf("events = %+v, want one context measurement as the last event after the summary completed", collected)
			}
			// 检查测量包含摘要和保留原文，不包含摘要已覆盖内容。
			want, err := history.GetMeasurement(nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(measured[0].Measurement, want) {
				t.Fatalf("measurement = %+v, want %+v", measured[0].Measurement, want)
			}
			var measuredIDs []defs.ItemID
			for _, item := range measured[0].Measurement.Items {
				measuredIDs = append(measuredIDs, item.ItemID)
			}
			if wantIDs := []defs.ItemID{summary.ID, recent.ID}; !slices.Equal(measuredIDs, wantIDs) {
				t.Fatalf("measured items = %v, want the summary and the retained recent message %v", measuredIDs, wantIDs)
			}
		})
	}
}

// TestAgentFinalModelCallID 检查空输出、仅推理和调用失败时的最终调用 ID。
func TestAgentFinalModelCallID(t *testing.T) {
	// 1. 准备各类模型结束和错误事件。
	for _, tt := range []struct {
		name    string
		events  []llmkit.Event
		wantErr bool
	}{
		{
			name:   "empty",
			events: []llmkit.Event{{Type: llmkit.EventDone}},
		},
		{
			name: "reasoning_only",
			events: []llmkit.Event{
				{
					Type:  llmkit.EventReasoningSummaryDelta,
					Delta: &llmkit.DeltaPayload{Text: "thinking"},
				},
				{Type: llmkit.EventDone},
			},
		},
		{
			name:    "failed",
			events:  []llmkit.Event{{Type: llmkit.EventError}},
			wantErr: true,
		},
	} {
		// 2. 执行成功、空输出和失败响应，检查最终调用 ID 只在成功时非空。
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &sequenceAgentClient{responses: slices.Repeat([][]llmkit.Event{tt.events}, 11)}
				instance, err := agent.NewAgent(agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("answer")})))
				if err != nil {
					t.Fatal(err)
				}
				result, err := instance.Run(t.Context(), nil)
				id := result.FinalModelCallID
				if (err != nil) != tt.wantErr || (id == "") != tt.wantErr {
					t.Fatalf("id=%s err=%v", id, err)
				}
			})
		})
	}
}

// TestAgentCallUsesModelTargetAndMergeRoles 检查模型请求目标和角色合并选项。
func TestAgentCallUsesModelTargetAndMergeRoles(t *testing.T) {
	// 1. 准备角色合并、无合并和缺失模型配置。
	textReply := []llmkit.Event{{
		Type: llmkit.EventContentDelta,
		Delta: &llmkit.DeltaPayload{
			ContentType: llmkit.ContentTypeText,
			Text:        "好",
		},
	}, {
		Type:         llmkit.EventDone,
		FinishReason: "stop",
	}}
	tests := []struct {
		name    string
		model   *defs.ModelDefinition
		wantErr bool
	}{
		{
			name: "merge_user_messages",
			model: &defs.ModelDefinition{
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					Model:    "upstream-a",
				},
				MergeMessageRoles: []llmkit.Role{llmkit.RoleUser},
			},
		},
		{
			name: "no_merge",
			model: &defs.ModelDefinition{Target: &llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				Model:    "upstream-a",
			}},
		},
		{
			name:    "missing_model",
			model:   nil,
			wantErr: true,
		},
		{
			name:    "model_without_target",
			model:   &defs.ModelDefinition{},
			wantErr: true,
		},
	}
	// 2. 创建并运行各模型配置，检查调用目标、角色合并选项和无效配置错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedAgentClient{events: textReply}
			instance, err := agent.NewAgent(agent.NewAgentOpt(testTurnID, testSessionID, client, tt.model, newTestContext(t, "", []*defs.Item{userMessage("answer")})))
			if tt.wantErr {
				if err == nil {
					t.Fatal("NewAgent accepted an invalid model")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := instance.Run(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			if len(client.targets) != 1 || client.targets[0].Model != "upstream-a" ||
				!slices.Equal(client.options[0].Conversion.MergeMessageRoles, tt.model.MergeMessageRoles) {
				t.Fatalf("targets = %+v options = %+v", client.targets, client.options)
			}
		})
	}
}

// TestApplyItemDelta 检查消息、推理和工具参数的增量追加。
func TestApplyItemDelta(t *testing.T) {
	// 1. 准备原 Item、增量和合并后的检查条件。
	tests := []struct {
		name  string
		item  *defs.Item
		delta *agent.AgentItemDelta
		check func(item *defs.Item) bool
	}{
		{
			name: "message_appends_text",
			item: &defs.Item{
				Kind: defs.ItemMessage,
				Message: &defs.Message{Content: []*defs.Content{{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: "今天"},
				}}},
			},
			delta: &agent.AgentItemDelta{Text: "晴"},
			check: func(item *defs.Item) bool { return item.Message.Content[0].Text.Text == "今天晴" },
		},
		{
			name: "reasoning_appends_fragments",
			item: &defs.Item{
				Kind:      defs.ItemReasoning,
				Reasoning: &defs.Reasoning{Summary: []string{"a"}},
			},
			delta: &agent.AgentItemDelta{
				ReasoningSummary: []string{"b"},
				ReasoningContent: []string{"c"},
			},
			check: func(item *defs.Item) bool {
				return reflect.DeepEqual(item.Reasoning.Summary, []string{"a", "b"}) && reflect.DeepEqual(item.Reasoning.Content, []string{"c"})
			},
		},
		{
			name: "tool_call_appends_arguments",
			item: &defs.Item{
				Kind:     defs.ItemToolCall,
				ToolCall: &defs.ToolCall{Arguments: `{"q":`},
			},
			delta: &agent.AgentItemDelta{Arguments: `"天气"}`},
			check: func(item *defs.Item) bool { return item.ToolCall.Arguments == `{"q":"天气"}` },
		},
	}
	// 2. 应用增量，比较追加后的消息文本、推理片段和工具参数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent.ApplyItemDelta(tt.item, tt.delta)
			if !tt.check(tt.item) {
				t.Fatalf("item after delta = %+v", tt.item)
			}
		})
	}
}

// TestAgentRecordsModelCallTelemetry 检查模型调用的用量、结束原因、日志和遥测状态。
func TestAgentRecordsModelCallTelemetry(t *testing.T) {
	// 1. 准备带用量的成功响应和持续流错误。
	tests := []struct {
		name       string
		events     []llmkit.Event
		wantErr    bool
		wantStatus codes.Code
		wantInput  string
		wantFinish string
		wantLog    string
	}{
		{
			name: "usage_and_finish_reason",
			events: []llmkit.Event{
				{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "answer"},
				},
				{
					Type: llmkit.EventUsage,
					Usage: &llmkit.Usage{
						PromptTokens:     120,
						CompletionTokens: 30,
						CachedTokens:     80,
					},
				},
				{
					Type:         llmkit.EventDone,
					FinishReason: "stop",
				},
			},
			wantStatus: codes.Unset,
			wantInput:  "120",
			wantFinish: `["stop"]`,
			wantLog:    "model call finished",
		},
		{
			name: "stream_error_fails_model_call",
			events: []llmkit.Event{{
				Type: llmkit.EventError,
				Error: &llmkit.ErrorPayload{
					Code:    "overloaded",
					Message: "busy",
				},
			}},
			wantErr:    true,
			wantStatus: codes.Error,
			wantFinish: `["error"]`,
			wantLog:    "model call failed",
		},
	}
	// 2. 运行成功或持续失败响应，检查调用 span、用量指标和结束日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := telemetrytest.NewPipeline(t)
				ctx := defs.ContextWithSessionCtxMeta(telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), defs.SessionCtxMeta{AgentName: "assistant"})
				opt := agent.NewAgentOpt(
					testTurnID,
					testSessionID,
					&sequenceAgentClient{responses: slices.Repeat([][]llmkit.Event{tt.events}, 11)},
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "gpt-test",
					}, 0),
					newTestContext(t, "", []*defs.Item{userMessage("你好")}),
				)
				instance, err := agent.NewAgent(opt)
				if err != nil {
					t.Fatal(err)
				}
				result, err := instance.Run(ctx, nil)
				if (err != nil) != tt.wantErr {
					t.Fatalf("Run() error = %v, wantErr %v", err, tt.wantErr)
				}
				wantCalls := 1
				if tt.wantErr {
					wantCalls = 11
				}
				spans := p.WaitEndedSpans(t, "chat gpt-test", wantCalls)
				for _, failedSpan := range spans {
					if failedSpan.Status().Code != tt.wantStatus {
						t.Fatalf("span status = %v, want %v", failedSpan.Status(), tt.wantStatus)
					}
				}
				span := spans[len(spans)-1]
				if span.Status().Code != tt.wantStatus ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.usage.input_tokens") != tt.wantInput ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.response.finish_reasons") != tt.wantFinish ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.provider.name") != "openai" {
					t.Fatalf("span status = %v, attributes = %v", span.Status(), span.Attributes())
				}
				if len(p.GetLogRecords(t, tt.wantLog)) != wantCalls {
					t.Fatalf("logs = %s", p.GetLogText())
				}
				if tt.wantErr {
					return
				}
				if len(result.Items) != 1 || result.Items[0].Kind != defs.ItemMessage {
					t.Fatalf("items = %+v, usage must not become an Item", result.Items)
				}
				if telemetrytest.GetSpanAttribute(span, "openvaala.model_call.id") != string(result.Items[0].ModelCallID) ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.usage.cache_read.input_tokens") != "80" {
					t.Fatalf("span attributes = %v", span.Attributes())
				}
				if got := p.GetMetricSum(t, "gen_ai.client.inference.usage.input_tokens"); got != 120 {
					t.Fatalf("input tokens = %v, want 120", got)
				}
			})
		})
	}
}

// TestAgentModelCallTelemetryTreatsInterruptionAsCanceled 检查调用中断的遥测按取消记录。
func TestAgentModelCallTelemetryTreatsInterruptionAsCanceled(t *testing.T) {
	// 1. 准备取消后返回错误或阻塞事件流的样例。
	tests := []struct {
		name    string
		callErr error
	}{
		{name: "stream_wait_is_canceled"},
		{
			name:    "call_error_after_cancel",
			callErr: errors.New("read tcp: use of closed network connection"),
		},
	}
	// 2. 在模型调用时取消，检查 span 记录 canceled，结束日志不记调用失败。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			ctx, cancel := context.WithCancel(telemetry.ContextWithTelemetry(t.Context(), p.Telemetry))
			defer cancel()
			client := &cancelingAgentClient{
				cancel: cancel,
				err:    tt.callErr,
			}
			instance, err := agent.NewAgent(agent.NewAgentOpt(
				testTurnID,
				testSessionID,
				client,
				newTestModel(&llmkit.Target{
					Protocol: llmkit.ProtocolClaude,
					Model:    "claude-test",
				}, 0),
				newTestContext(t, "", []*defs.Item{userMessage("你好")}),
			))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := instance.Run(ctx, nil); err == nil {
				t.Fatal("Run() error = nil, want interruption")
			}
			span := p.WaitEndedSpans(t, "chat claude-test", 1)[0]
			if span.Status().Code != codes.Unset || telemetrytest.GetSpanAttribute(span, "error.type") != "canceled" {
				t.Fatalf("span status = %v, attributes = %v", span.Status(), span.Attributes())
			}
			if len(p.GetLogRecords(t, "model call finished")) != 1 || len(p.GetLogRecords(t, "model call failed")) != 0 {
				t.Fatalf("logs = %s", p.GetLogText())
			}
		})
	}
}

// cancelingAgentClient 在模型调用开始时取消 Agent 的 ctx，模拟 Turn 被中断；err 非 nil 时调用立即返回这个错误，否则返回一直不结束的流。
type cancelingAgentClient struct {
	cancel context.CancelFunc
	err    error
}

// Call 取消运行上下文，再返回预设错误或未关闭的事件流。
func (c *cancelingAgentClient) Call(context.Context, llmkit.Request, llmkit.Target, llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 调用取消函数，按预设返回错误或空事件通道。
	c.cancel()
	if c.err != nil {
		return nil, c.err
	}
	return make(chan llmkit.Event), nil
}

// TestAgentTurnStats 检查工具、压缩、失败和取消时的本轮累计统计。
func TestAgentTurnStats(t *testing.T) {
	// 1. 准备各类模型和工具运行配置及预期统计。
	tests := []struct {
		name    string
		newOpt  func(t *testing.T, cancel context.CancelFunc) *agent.AgentOpt
		want    agent.AgentStats
		wantErr bool
	}{
		{
			name: "tool_loop_counts_steps_tools_and_usage",
			newOpt: func(t *testing.T, _ context.CancelFunc) *agent.AgentOpt {
				opt := agent.NewAgentOpt(testTurnID, testSessionID, &sequenceAgentClient{responses: newToolLoopResponses()},
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "gpt-test",
					}, 0), newTestContext(t, "", []*defs.Item{userMessage("你好")}))
				opt.ToolExecutor = &testToolExecutor{}
				return opt
			},
			want: agent.AgentStats{
				Steps:          2,
				InferenceCalls: 2,
				ToolCalls:      1,
				Usage: defs.TokenUsage{
					Input:  300,
					Output: 30,
				},
				FinishReason: "stop",
			},
		},
		{
			name: "failed_tool_is_counted",
			newOpt: func(t *testing.T, _ context.CancelFunc) *agent.AgentOpt {
				opt := agent.NewAgentOpt(testTurnID, testSessionID, &sequenceAgentClient{responses: newToolLoopResponses()},
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "gpt-test",
					}, 0), newTestContext(t, "", []*defs.Item{userMessage("你好")}))
				opt.ToolExecutor = &testToolExecutor{toolError: &defs.ToolError{
					Code:    "not_found",
					Message: "没有找到资料",
				}}
				return opt
			},
			want: agent.AgentStats{
				Steps:           2,
				InferenceCalls:  2,
				ToolCalls:       1,
				FailedToolCalls: 1,
				Usage: defs.TokenUsage{
					Input:  300,
					Output: 30,
				},
				FinishReason: "stop",
			},
		},
		{
			name: "compaction_counts_as_inference_call",
			newOpt: func(t *testing.T, _ context.CancelFunc) *agent.AgentOpt {
				old := userMessage(strings.Repeat("old-history ", 400))
				old.ID = "old"
				recent := userMessage(strings.Repeat("recent ", 220))
				recent.ID = "recent"
				client := &compactingAgentClient{}
				model := newTestModel(&llmkit.Target{Model: "model"}, 1000)
				history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, model, model))
				if err != nil {
					t.Fatal(err)
				}
				if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{old, recent}}); err != nil {
					t.Fatal(err)
				}
				return agent.NewAgentOpt(testTurnID, testSessionID, client, model, history)
			},
			want: agent.AgentStats{
				Steps:          1,
				InferenceCalls: 2,
				FinishReason:   "stop",
			},
		},
		{
			name: "stream_error_after_tool_step_reports_error_finish_reason",
			newOpt: func(t *testing.T, _ context.CancelFunc) *agent.AgentOpt {
				streamError := []llmkit.Event{{
					Type: llmkit.EventError,
					Error: &llmkit.ErrorPayload{
						Code:    "overloaded",
						Message: "busy",
					},
				}}
				opt := agent.NewAgentOpt(testTurnID, testSessionID, &sequenceAgentClient{responses: append(newToolLoopResponses()[:1], slices.Repeat([][]llmkit.Event{streamError}, 11)...)},
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "gpt-test",
					}, 0), newTestContext(t, "", []*defs.Item{userMessage("你好")}))
				opt.ToolExecutor = &testToolExecutor{}
				return opt
			},
			want: agent.AgentStats{
				Steps:          12,
				InferenceCalls: 12,
				ToolCalls:      1,
				Usage: defs.TokenUsage{
					Input:  100,
					Output: 10,
				},
				FinishReason: telemetry.FinishReasonError,
			},
			wantErr: true,
		},
		{
			name: "call_error_after_tool_step_reports_error_finish_reason",
			newOpt: func(t *testing.T, _ context.CancelFunc) *agent.AgentOpt {
				opt := agent.NewAgentOpt(testTurnID, testSessionID, &sequenceAgentClient{responses: newToolLoopResponses()[:1]},
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "gpt-test",
					}, 0), newTestContext(t, "", []*defs.Item{userMessage("你好")}))
				opt.ToolExecutor = &testToolExecutor{}
				return opt
			},
			want: agent.AgentStats{
				Steps:          2,
				InferenceCalls: 2,
				ToolCalls:      1,
				Usage: defs.TokenUsage{
					Input:  100,
					Output: 10,
				},
				FinishReason: telemetry.FinishReasonError,
			},
			wantErr: true,
		},
		{
			name: "canceled_mid_stream_reports_error_finish_reason",
			newOpt: func(t *testing.T, cancel context.CancelFunc) *agent.AgentOpt {
				return agent.NewAgentOpt(testTurnID, testSessionID, &cancelingAgentClient{cancel: cancel},
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "gpt-test",
					}, 0), newTestContext(t, "", []*defs.Item{userMessage("你好")}))
			},
			want: agent.AgentStats{
				Steps:          1,
				InferenceCalls: 1,
				FinishReason:   telemetry.FinishReasonError,
			},
			wantErr: true,
		},
	}
	// 2. 执行各类模型、工具和压缩流程，比较步骤数、调用数、失败工具数及累计用量。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				instance, err := agent.NewAgent(tt.newOpt(t, cancel))
				if err != nil {
					t.Fatal(err)
				}
				result, err := instance.Run(ctx, nil)
				if (err != nil) != tt.wantErr {
					t.Fatalf("Run() error = %v, wantErr %v", err, tt.wantErr)
				}
				if result.Stats != tt.want {
					t.Fatalf("Stats = %+v, want %+v", result.Stats, tt.want)
				}
			})
		})
	}
}

// TestAgentStepTelemetry 检查模型和工具遥测中的步骤序号。
func TestAgentStepTelemetry(t *testing.T) {
	// 1. 创建两步工具循环、测试遥测和真实工具执行器。
	p := telemetrytest.NewPipeline(t)
	opt := agent.NewAgentOpt(testTurnID, testSessionID, &sequenceAgentClient{responses: newToolLoopResponses()},
		newTestModel(&llmkit.Target{
			Protocol: llmkit.ProtocolOpenAIChat,
			BaseURL:  "https://llm.example.com/v1",
			Model:    "gpt-test",
		}, 0),
		newTestContext(t, "你是助手", []*defs.Item{userMessage("你好")}))
	toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, &testTool{backend: &testToolExecutor{}}))
	if err != nil {
		t.Fatal(err)
	}
	opt.ToolExecutor = toolExecutor
	instance, err := agent.NewAgent(opt)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 运行工具循环，检查两步模型调用及工具调用的遥测。
	if _, err := instance.Run(telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), nil); err != nil {
		t.Fatal(err)
	}
	spans := p.WaitEndedSpans(t, "chat gpt-test", 2)
	tests := []struct {
		name     string
		span     int
		want     map[string]string
		contains map[string]string
	}{
		{
			name: "first_step_requests_tool",
			span: 0,
			want: map[string]string{
				"openvaala.turn.step": "1", "gen_ai.response.finish_reasons": `["tool_call"]`, "gen_ai.response.model": "gpt-upstream",
				"server.address": "llm.example.com", "server.port": "443", "gen_ai.request.stream": "true",
			},
			contains: map[string]string{
				"gen_ai.input.messages": "你好", "gen_ai.system_instructions": "你是助手", "gen_ai.tool.definitions": "lookup",
				"gen_ai.output.messages": "tool_call",
			},
		},
		{
			name:     "second_step_answers",
			span:     1,
			want:     map[string]string{"openvaala.turn.step": "2", "gen_ai.response.finish_reasons": `["stop"]`},
			contains: map[string]string{"gen_ai.input.messages": "tool_call_response", "gen_ai.output.messages": "完成"},
		},
	}
	// 3. 逐项检查模型 span 的步骤、输入和输出，以及工具 span 的步骤。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			span := spans[tt.span]
			for key, want := range tt.want {
				if got := telemetrytest.GetSpanAttribute(span, key); got != want {
					t.Errorf("span %s = %q, want %q", key, got, want)
				}
			}
			for key, want := range tt.contains {
				if got := telemetrytest.GetSpanAttribute(span, key); !strings.Contains(got, want) {
					t.Errorf("span %s = %q, want it to contain %q", key, got, want)
				}
			}
			if telemetrytest.GetSpanAttribute(span, "gen_ai.response.time_to_first_chunk") == "" {
				t.Error("span lacks gen_ai.response.time_to_first_chunk")
			}
		})
	}
	t.Run("tool_span_carries_step_of_its_model_call", func(t *testing.T) {
		toolSpan := p.WaitEndedSpans(t, "execute_tool lookup", 1)[0]
		if got := telemetrytest.GetSpanAttribute(toolSpan, "openvaala.turn.step"); got != "1" {
			t.Errorf("execute_tool span openvaala.turn.step = %q, want %q", got, "1")
		}
	})
}

// TestAgentLogsSupplementaryInput 检查补充输入日志记录 Item ID 而不记录正文。
func TestAgentLogsSupplementaryInput(t *testing.T) {
	// 1. 准备有补充输入和无补充输入的读取回复。
	supplement := userMessage("补充说明")
	supplement.ID = "steer-1"
	tests := []struct {
		name     string
		replies  []agent.AgentInputResult
		wantLogs int
	}{
		{
			name:     "supplement_is_logged_with_item_ids",
			replies:  []agent.AgentInputResult{{}, {Items: []*defs.Item{supplement}}, {}, {}},
			wantLogs: 1,
		},
		{
			name:    "no_supplement_no_log",
			replies: []agent.AgentInputResult{{}, {}},
		},
	}
	// 2. 回复补充输入请求，检查日志只包含输入 ID、步骤和级别，不包含正文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			opt := agent.NewAgentOpt(testTurnID, testSessionID, &textAgentClient{}, testModel, newTestContext(t, "", []*defs.Item{userMessage("开始。")}))
			inputRequests := make(chan agent.AgentInputRequest)
			opt.InputRequests = inputRequests
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := instance.Run(ctx, nil)
				done <- err
			}()
			for _, reply := range tt.replies {
				request := <-inputRequests
				request.Reply <- reply
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			records := p.GetLogRecords(t, "turn input read")
			if len(records) != tt.wantLogs {
				t.Fatalf("turn input read logs = %v, want %d", records, tt.wantLogs)
			}
			if tt.wantLogs == 0 {
				return
			}
			ids, _ := records[0]["openvaala.input.item_ids"].([]any)
			if len(ids) != 1 || ids[0] != "steer-1" || records[0]["openvaala.turn.step"] != float64(1) || records[0]["level"] != "INFO" {
				t.Fatalf("log = %v", records[0])
			}
			if strings.Contains(p.GetLogText(), "补充说明") {
				t.Fatalf("logs contain supplementary input content: %s", p.GetLogText())
			}
		})
	}
}

// TestAgentStepEvents 检查每步输入、压缩、测量、输出和调用结束的事件顺序。
func TestAgentStepEvents(t *testing.T) {
	// 1. 准备普通回复、重试、补充输入、压缩和工具循环的预期事件。
	type wantModelCall struct {
		purpose agent.ModelCallPurpose
		usage   *defs.TokenUsage
	}
	usage := defs.TokenUsage{
		Input:  100,
		Output: 10,
	}
	steer := userMessage("补充说明")
	steer.ID = "steer-1"
	tests := []struct {
		name           string
		newOpt         func(t *testing.T) *agent.AgentOpt
		wantSequence   []string
		wantModelCalls []wantModelCall
		wantStatsUsage defs.TokenUsage
		wantErr        bool
		verify         func(t *testing.T, events []*agent.AgentEvent)
	}{
		{
			name: "single_reply_reports_usage",
			newOpt: func(t *testing.T) *agent.AgentOpt {
				client := &sequenceAgentClient{responses: [][]llmkit.Event{{
					{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "answer"},
					},
					{
						Type: llmkit.EventUsage,
						Usage: &llmkit.Usage{
							PromptTokens:     100,
							CompletionTokens: 10,
						},
					},
					{
						Type:         llmkit.EventDone,
						FinishReason: "stop",
					},
				}}}
				return agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("你好")}))
			},
			wantSequence: []string{
				"context_measured", "item_started:message", "item_completed:message", "context_measured", "model_call_finished(conversation)",
				"context_measured", "context_measured",
			},
			wantModelCalls: []wantModelCall{{
				purpose: agent.ModelCallPurposeConversation,
				usage:   &usage,
			}},
			wantStatsUsage: usage,
		},
		{
			name: "single_reply_without_reported_usage",
			newOpt: func(t *testing.T) *agent.AgentOpt {
				return agent.NewAgentOpt(testTurnID, testSessionID, &textAgentClient{}, testModel, newTestContext(t, "", []*defs.Item{userMessage("你好")}))
			},
			wantSequence: []string{
				"context_measured", "item_started:message", "item_completed:message", "context_measured", "model_call_finished(conversation)",
				"context_measured", "context_measured",
			},
			wantModelCalls: []wantModelCall{{purpose: agent.ModelCallPurposeConversation}},
		},
		{
			name: "stream_error_after_reported_usage_finishes_call_before_interruption",
			newOpt: func(t *testing.T) *agent.AgentOpt {
				client := &sequenceAgentClient{responses: [][]llmkit.Event{{
					{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "part"},
					},
					{
						Type: llmkit.EventUsage,
						Usage: &llmkit.Usage{
							PromptTokens:     100,
							CompletionTokens: 10,
						},
					},
					{
						Type: llmkit.EventError,
						Error: &llmkit.ErrorPayload{
							Code:    "overloaded",
							Message: "busy",
						},
					},
				}, newToolLoopResponses()[1]}}
				return agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("你好")}))
			},
			wantSequence: []string{
				"context_measured", "item_started:message", "item_started:model_error", "item_completed:model_error",
				"model_call_finished(conversation)", "item_interrupted:message",
				"item_started:message", "item_completed:message", "context_measured", "model_call_finished(conversation)",
				"context_measured", "context_measured",
			},
			wantModelCalls: []wantModelCall{
				{
					purpose: agent.ModelCallPurposeConversation,
					usage:   &usage,
				},
				{
					purpose: agent.ModelCallPurposeConversation,
					usage: &defs.TokenUsage{
						Input:  200,
						Output: 20,
					},
				},
			},
			wantStatsUsage: defs.TokenUsage{
				Input:  300,
				Output: 30,
			},
		},
		{
			name: "steer_input_is_measured_in_the_same_step",
			newOpt: func(t *testing.T) *agent.AgentOpt {
				opt := agent.NewAgentOpt(testTurnID, testSessionID, &textAgentClient{}, testModel, newTestContext(t, "", []*defs.Item{userMessage("开始。")}))
				serveAgentInputs(t, opt, agent.AgentInputResult{Items: []*defs.Item{steer}})
				return opt
			},
			wantSequence: []string{
				"context_measured", "item_started:message", "item_completed:message", "context_measured", "model_call_finished(conversation)",
				"context_measured", "context_measured",
			},
			wantModelCalls: []wantModelCall{{purpose: agent.ModelCallPurposeConversation}},
			verify: func(t *testing.T, events []*agent.AgentEvent) {
				measured := getAgentEventsOfType(events, agent.AgentEventContextMeasured)[0].Measurement
				if !slices.ContainsFunc(measured.Items, func(item agent_context.ItemTokens) bool { return item.ItemID == steer.ID }) {
					t.Fatalf("first measurement items = %+v, want them to contain %s", measured.Items, steer.ID)
				}
			},
		},
		{
			name: "compaction_precedes_the_final_measurement",
			newOpt: func(t *testing.T) *agent.AgentOpt {
				old := userMessage(strings.Repeat("old-history ", 400))
				old.ID = "old"
				recent := userMessage(strings.Repeat("recent ", 220))
				recent.ID = "recent"
				client := &compactingAgentClient{
					summaryUsage: &llmkit.Usage{
						PromptTokens:     50,
						CompletionTokens: 5,
					},
					normalUsage: &llmkit.Usage{
						PromptTokens:     100,
						CompletionTokens: 10,
					},
				}
				model := newTestModel(&llmkit.Target{Model: "model"}, 1000)
				history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, model, model))
				if err != nil {
					t.Fatal(err)
				}
				if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{old, recent}}); err != nil {
					t.Fatal(err)
				}
				return agent.NewAgentOpt(testTurnID, testSessionID, client, model, history)
			},
			wantSequence: []string{
				"item_started:context_summary", "model_call_finished(compaction)", "item_completed:context_summary", "context_measured",
				"item_started:message", "item_completed:message", "context_measured", "model_call_finished(conversation)",
				"context_measured", "context_measured",
			},
			wantModelCalls: []wantModelCall{
				{
					purpose: agent.ModelCallPurposeCompaction,
					usage: &defs.TokenUsage{
						Input:  50,
						Output: 5,
					},
				},
				{
					purpose: agent.ModelCallPurposeConversation,
					usage: &defs.TokenUsage{
						Input:  100,
						Output: 10,
					},
				},
			},
			wantStatsUsage: defs.TokenUsage{
				Input:  150,
				Output: 15,
			},
			verify: func(t *testing.T, events []*agent.AgentEvent) {
				var summaryID defs.ItemID
				for _, event := range events {
					if event.Item != nil && event.Item.Kind == defs.ItemContextSummary {
						summaryID = event.Item.ID
					}
				}
				measured := getAgentEventsOfType(events, agent.AgentEventContextMeasured)[0].Measurement
				if len(measured.Items) == 0 || measured.Items[0].ItemID != summaryID || measured.Items[0].Category != agent_context.ContextCategorySummary {
					t.Fatalf("final measurement items = %+v, want the summary %s first", measured.Items, summaryID)
				}
			},
		},
		{
			name: "tool_step_then_text_step",
			newOpt: func(t *testing.T) *agent.AgentOpt {
				opt := agent.NewAgentOpt(testTurnID, testSessionID, &sequenceAgentClient{responses: newToolLoopResponses()},
					newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "gpt-test",
					}, 0), newTestContext(t, "", []*defs.Item{userMessage("你好")}))
				opt.ToolExecutor = &testToolExecutor{}
				return opt
			},
			wantSequence: []string{
				"context_measured", "item_started:tool_call", "item_completed:tool_call", "context_measured", "model_call_finished(conversation)",
				"context_measured",
				"tool_progress", "item_started:tool_result", "item_completed:tool_result",
				"context_measured", "item_started:message", "item_completed:message", "context_measured", "model_call_finished(conversation)",
				"context_measured", "context_measured",
			},
			wantModelCalls: []wantModelCall{
				{
					purpose: agent.ModelCallPurposeConversation,
					usage: &defs.TokenUsage{
						Input:  100,
						Output: 10,
					},
				},
				{
					purpose: agent.ModelCallPurposeConversation,
					usage: &defs.TokenUsage{
						Input:  200,
						Output: 20,
					},
				},
			},
			wantStatsUsage: defs.TokenUsage{
				Input:  300,
				Output: 30,
			},
			verify: func(t *testing.T, events []*agent.AgentEvent) {
				measured := getAgentEventsOfType(events, agent.AgentEventContextMeasured)
				var categories [][]agent_context.ContextCategory
				for _, event := range measured {
					var eventCategories []agent_context.ContextCategory
					for _, item := range event.Measurement.Items {
						eventCategories = append(eventCategories, item.Category)
					}
					categories = append(categories, eventCategories)
				}
				want := [][]agent_context.ContextCategory{
					{agent_context.ContextCategoryUserInput},
					{agent_context.ContextCategoryUserInput},
					{agent_context.ContextCategoryUserInput, agent_context.ContextCategoryToolCall},
					{agent_context.ContextCategoryUserInput, agent_context.ContextCategoryToolCall, agent_context.ContextCategoryToolResult},
					{agent_context.ContextCategoryUserInput, agent_context.ContextCategoryToolCall, agent_context.ContextCategoryToolResult},
					{agent_context.ContextCategoryUserInput, agent_context.ContextCategoryToolCall, agent_context.ContextCategoryToolResult, agent_context.ContextCategoryAssistantText},
					{agent_context.ContextCategoryUserInput, agent_context.ContextCategoryToolCall, agent_context.ContextCategoryToolResult, agent_context.ContextCategoryAssistantText},
				}
				if !reflect.DeepEqual(categories, want) {
					t.Fatalf("measured categories = %v, want %v", categories, want)
				}
			},
		},
	}
	// 2. 运行并收集事件，比较测量、Item、模型用途和调用用量的完整顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				instance, err := agent.NewAgent(tt.newOpt(t))
				if err != nil {
					t.Fatal(err)
				}
				eventChannel := make(chan *agent.AgentEvent)
				collected := make(chan []*agent.AgentEvent, 1)
				go func() {
					var events []*agent.AgentEvent
					for event := range eventChannel {
						events = append(events, event)
					}
					collected <- events
				}()
				result, err := instance.Run(t.Context(), eventChannel)
				if (err != nil) != tt.wantErr {
					t.Fatalf("Run() error = %v, wantErr %v", err, tt.wantErr)
				}
				events := <-collected
				var sequence []string
				var modelCalls []wantModelCall
				for _, event := range events {
					switch event.Type {
					case agent.AgentEventItemDelta:
						continue
					case agent.AgentEventContextMeasured:
						if event.Measurement == nil {
							t.Fatalf("context_measured event without measurement: %+v", event)
						}
						sequence = append(sequence, string(event.Type))
					case agent.AgentEventModelCallFinished:
						if event.ModelCall == nil || event.ModelCall.ModelCallID == "" {
							t.Fatalf("model_call_finished event without model call identity: %+v", event)
						}
						sequence = append(sequence, fmt.Sprintf("%s(%s)", event.Type, event.ModelCall.Purpose))
						modelCalls = append(modelCalls, wantModelCall{
							purpose: event.ModelCall.Purpose,
							usage:   event.ModelCall.Usage,
						})
					default:
						label := string(event.Type)
						if event.Item != nil {
							label += ":" + string(event.Item.Kind)
						}
						sequence = append(sequence, label)
					}
				}
				if !reflect.DeepEqual(sequence, tt.wantSequence) {
					t.Fatalf("event sequence = %v, want %v", sequence, tt.wantSequence)
				}
				if !reflect.DeepEqual(modelCalls, tt.wantModelCalls) {
					t.Fatalf("model calls = %+v, want %+v", modelCalls, tt.wantModelCalls)
				}
				if result.Stats.Usage != tt.wantStatsUsage {
					t.Fatalf("Stats.Usage = %+v, want %+v", result.Stats.Usage, tt.wantStatsUsage)
				}
				// 正常结束时，最后一条调用结束事件应对应最终模型调用 ID。
				finished := getAgentEventsOfType(events, agent.AgentEventModelCallFinished)
				if last := finished[len(finished)-1].ModelCall; !tt.wantErr && last.ModelCallID != result.FinalModelCallID {
					t.Fatalf("last model call = %s, want final model call %s", last.ModelCallID, result.FinalModelCallID)
				}
				if tt.verify != nil {
					tt.verify(t, events)
				}
			})
		})
	}
}

// stubHooks 保存预设 hook 结果、实时运行记录和已收到请求。
type stubHooks struct {
	outcomes   []*hook.HookOutcome
	runUpdates []*defs.HookRun
	requests   []*hook.HookEventRequest
}

// RunEvent 记录请求、发送实时记录，并返回按调用次序预设的 hook 结果。
func (h *stubHooks) RunEvent(_ context.Context, request *hook.HookEventRequest, runUpdates chan<- *defs.HookRun) *hook.HookOutcome {
	// 1. 保存请求并发送实时记录，返回本次结果或空结果。
	index := len(h.requests)
	h.requests = append(h.requests, request)
	for _, run := range h.runUpdates {
		runUpdates <- run.Clone()
	}
	if index < len(h.outcomes) {
		return h.outcomes[index]
	}
	return &hook.HookOutcome{}
}

// GetHookInfos 返回空的 hook 说明列表。
func (h *stubHooks) GetHookInfos() []*hook.HookInfo {
	// 1. 返回空列表。
	return nil
}

// newStopHookRun 创建指定状态的 Stop hook 运行记录。
func newStopHookRun(id string, status defs.HookRunStatus) *defs.HookRun {
	// 1. 填写运行 ID、默认环境、脚本来源和状态。
	return &defs.HookRun{
		ID:         defs.HookRunID(id),
		Event:      defs.HookEventStop,
		ExecutorID: "local",
		SourcePath: "/hooks/stop.yaml",
		Handler:    "stop.sh",
		Status:     status,
	}
}

// TestAgentStopHooks 检查结束 hook 的续跑、停止、次数上限和记录。
func TestAgentStopHooks(t *testing.T) {
	// 1. 准备文本、工具响应及拦下结束请求的 hook 结果。
	textResponse := func(text string) []llmkit.Event {
		return []llmkit.Event{
			{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: text},
			},
			{
				Type:         llmkit.EventDone,
				FinishReason: "stop",
			},
		}
	}
	toolLoopResponses := newToolLoopResponses()
	blocked := &hook.HookOutcome{
		IsBlocked:   true,
		BlockReason: "run the tests",
		SavedRuns:   []*defs.HookRun{newStopHookRun("stop-blocked", defs.HookRunBlocked)},
	}
	blocked.SavedRuns[0].BlockReason = "run the tests"
	type wantStop struct {
		name       defs.HookEventName
		isActive   bool
		lastAnswer string
	}
	tests := []*struct {
		name            string
		responses       [][]llmkit.Event
		outcomes        []*hook.HookOutcome
		liveRuns        []*defs.HookRun
		parentSessionID defs.SessionID
		noHooks         bool
		noExecutors     bool
		// steer Agent 结束前是否有一组补充输入等着读取？
		steer          bool
		wantItemKinds  []defs.ItemKind
		wantStops      []wantStop
		wantLimitWarn  bool
		wantFeedback   string
		wantModelCalls int
	}{
		{
			name:           "blocked_once_continues_with_the_reason_in_context",
			responses:      [][]llmkit.Event{textResponse("first"), textResponse("second")},
			outcomes:       []*hook.HookOutcome{blocked},
			liveRuns:       []*defs.HookRun{newStopHookRun("stop-live", defs.HookRunRunning), newStopHookRun("stop-live", defs.HookRunBlocked)},
			wantModelCalls: 2,
			wantItemKinds:  []defs.ItemKind{defs.ItemMessage, defs.ItemHookRun, defs.ItemMessage},
			wantStops: []wantStop{{
				defs.HookEventStop,
				false,
				"first",
			}, {
				defs.HookEventStop,
				true,
				"second",
			}},
			wantFeedback: "Stop hook 要求继续：run the tests",
		},
		{
			name:           "ninth_consecutive_block_is_ignored",
			responses:      slices.Repeat([][]llmkit.Event{textResponse("again")}, 9),
			outcomes:       slices.Repeat([]*hook.HookOutcome{blocked}, 10),
			wantModelCalls: 9,
			wantItemKinds:  slices.Repeat([]defs.ItemKind{defs.ItemMessage, defs.ItemHookRun}, 9),
			wantStops: append([]wantStop{{
				defs.HookEventStop,
				false,
				"again",
			}}, slices.Repeat([]wantStop{{
				defs.HookEventStop,
				true,
				"again",
			}}, 8)...),
			wantLimitWarn: true,
		},
		{
			name:           "stop_hook_stays_active_after_tools_run",
			responses:      [][]llmkit.Event{textResponse("first"), toolLoopResponses[0], toolLoopResponses[1], textResponse("third")},
			outcomes:       []*hook.HookOutcome{blocked, blocked},
			wantModelCalls: 4,
			wantItemKinds: []defs.ItemKind{
				defs.ItemMessage, defs.ItemHookRun, defs.ItemToolCall, defs.ItemToolResult, defs.ItemMessage, defs.ItemHookRun, defs.ItemMessage,
			},
			wantStops: []wantStop{
				{
					defs.HookEventStop,
					false,
					"first",
				}, {
					defs.HookEventStop,
					true,
					"完成",
				}, {
					defs.HookEventStop,
					true,
					"third",
				},
			},
		},
		{
			name: "running_tools_restarts_the_continuation_limit",
			responses: slices.Concat(
				slices.Repeat([][]llmkit.Event{textResponse("again")}, 8),
				[][]llmkit.Event{toolLoopResponses[0], toolLoopResponses[1], textResponse("last")},
			),
			outcomes:       slices.Repeat([]*hook.HookOutcome{blocked}, 9),
			wantModelCalls: 11,
			wantItemKinds: slices.Concat(
				slices.Repeat([]defs.ItemKind{defs.ItemMessage, defs.ItemHookRun}, 8),
				[]defs.ItemKind{defs.ItemToolCall, defs.ItemToolResult, defs.ItemMessage, defs.ItemHookRun, defs.ItemMessage},
			),
			wantStops: slices.Concat(
				[]wantStop{{
					defs.HookEventStop,
					false,
					"again",
				}},
				slices.Repeat([]wantStop{{
					defs.HookEventStop,
					true,
					"again",
				}}, 7),
				[]wantStop{{
					defs.HookEventStop,
					true,
					"完成",
				}, {
					defs.HookEventStop,
					true,
					"last",
				}},
			),
		},
		{
			name:      "turn_stopped_finishes_without_reading_input",
			responses: [][]llmkit.Event{textResponse("first")},
			outcomes: []*hook.HookOutcome{{
				IsBlocked:     true,
				IsTurnStopped: true,
				StopReason:    "enough",
				SavedRuns:     []*defs.HookRun{newStopHookRun("stop-stopped", defs.HookRunStopped)},
			}},
			steer:          true,
			wantModelCalls: 1,
			wantItemKinds:  []defs.ItemKind{defs.ItemMessage, defs.ItemHookRun},
			wantStops: []wantStop{{
				defs.HookEventStop,
				false,
				"first",
			}},
		},
		{
			name:            "child_session_triggers_subagent_stop",
			responses:       [][]llmkit.Event{textResponse("first")},
			parentSessionID: "parent-session",
			wantModelCalls:  1,
			wantItemKinds:   []defs.ItemKind{defs.ItemMessage},
			wantStops: []wantStop{{
				defs.HookEventSubagentStop,
				false,
				"first",
			}},
		},
		{
			name:           "no_hooks_skips_stop",
			responses:      [][]llmkit.Event{textResponse("first")},
			noHooks:        true,
			wantModelCalls: 1,
			wantItemKinds:  []defs.ItemKind{defs.ItemMessage},
		},
		{
			name:           "no_executors_skips_stop",
			responses:      [][]llmkit.Event{textResponse("first")},
			outcomes:       []*hook.HookOutcome{blocked},
			noExecutors:    true,
			wantModelCalls: 1,
			wantItemKinds:  []defs.ItemKind{defs.ItemMessage},
		},
	}
	// 2. 运行结束 hook 的拦下、停止和工具续跑样例，比较调用次数及 hook 请求内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipeline := telemetrytest.NewPipeline(t)
			client := &sequenceAgentClient{responses: tt.responses}
			hooks := &stubHooks{
				outcomes:   tt.outcomes,
				runUpdates: tt.liveRuns,
			}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("你好")}))
			opt.ToolExecutor = &testToolExecutor{}
			opt.Session = defs.Session{
				ID:              testSessionID,
				ParentSessionID: tt.parentSessionID,
			}
			if !tt.noHooks {
				opt.Hooks = hooks
			}
			if !tt.noExecutors {
				opt.Environments = []executor.ExecutionEnvironment{newTestEnvironment(t, "local", nil), newTestEnvironment(t, "remote", nil)}
			}
			if tt.steer {
				serveAgentInputs(t, opt, agent.AgentInputResult{Items: []*defs.Item{userMessage("补充说明")}})
			}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), 5*time.Second)
			defer cancel()
			events := make(chan *agent.AgentEvent, 256)
			result, err := instance.Run(ctx, events)
			if err != nil {
				t.Fatal(err)
			}
			var allEvents []*agent.AgentEvent
			for event := range events {
				allEvents = append(allEvents, event)
			}
			if len(client.requests) != tt.wantModelCalls {
				t.Fatalf("model calls = %d, want %d", len(client.requests), tt.wantModelCalls)
			}
			var kinds []defs.ItemKind
			for _, item := range result.Items {
				kinds = append(kinds, item.Kind)
			}
			if !reflect.DeepEqual(kinds, tt.wantItemKinds) {
				t.Fatalf("item kinds = %v, want %v", kinds, tt.wantItemKinds)
			}
			assertAgentEvents(t, slices.DeleteFunc(slices.Clone(allEvents), func(event *agent.AgentEvent) bool {
				return event.Type == agent.AgentEventToolProgress
			}), result.Items)
			var stops []wantStop
			for _, request := range hooks.requests {
				if request.ExecutorID != "local" {
					t.Fatalf("hook executor = %q, want the default executor local", request.ExecutorID)
				}
				event := request.Event
				if event.TurnID != testTurnID || event.Session.ID != testSessionID || event.Session.ParentSessionID != tt.parentSessionID {
					t.Fatalf("hook event identity = %+v", event)
				}
				stops = append(stops, wantStop{
					event.Name,
					event.IsStopHookActive,
					event.LastAssistantMessage,
				})
			}
			if !reflect.DeepEqual(stops, tt.wantStops) {
				t.Fatalf("stop requests = %+v, want %+v", stops, tt.wantStops)
			}
			// 检查 Stop 记录位于所属模型回复之后，且模型调用 ID 相同。
			for index, item := range result.Items {
				if item.Kind != defs.ItemHookRun {
					continue
				}
				if index == 0 || result.Items[index-1].Kind != defs.ItemMessage || result.Items[index-1].ModelCallID != item.ModelCallID || item.TurnID != testTurnID {
					t.Fatalf("hook run item %d is not placed after its model call's reply: %+v", index, result.Items)
				}
			}
			if last := result.Items[len(result.Items)-1]; result.FinalModelCallID != last.ModelCallID {
				t.Fatalf("final model call = %q, last item = %+v", result.FinalModelCallID, last)
			}
			// 检查每次 Stop 发送的实时运行记录数量和内容。
			liveRuns := getAgentEventsOfType(allEvents, agent.AgentEventHookRun)
			if len(liveRuns) != len(tt.liveRuns)*len(hooks.requests) {
				t.Fatalf("hook run events = %d, want %d per stop request over %d requests", len(liveRuns), len(tt.liveRuns), len(hooks.requests))
			}
			for index, event := range liveRuns {
				if want := tt.liveRuns[index%len(tt.liveRuns)]; event.Item != nil || !reflect.DeepEqual(event.HookRun, want) {
					t.Fatalf("hook run event %d = %+v, want run %+v without item", index, event, want)
				}
			}
			if tt.wantFeedback != "" {
				messages := client.requests[1].Messages
				lastMessage := messages[len(messages)-1]
				if lastMessage.Role != llmkit.RoleUser || !strings.Contains(lastMessage.Content[0].Text, tt.wantFeedback) {
					t.Fatalf("second request ends with %+v, want the hook feedback %q", lastMessage, tt.wantFeedback)
				}
			}
			warnings := pipeline.GetLogRecords(t, "stop hook continuation limit reached")
			if tt.wantLimitWarn != (len(warnings) == 1) || len(warnings) > 1 || (len(warnings) == 1 && warnings[0]["level"] != "WARN") {
				t.Fatalf("limit warnings = %v, want warning %v", warnings, tt.wantLimitWarn)
			}
		})
	}
}

// getAgentEventsOfType 按发送顺序筛选指定类型的事件。
func getAgentEventsOfType(events []*agent.AgentEvent, eventType agent.AgentEventType) []*agent.AgentEvent {
	// 1. 遍历并收集匹配事件。
	var matched []*agent.AgentEvent
	for _, event := range events {
		if event.Type == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// serveAgentInputs 为 Agent 提供测试输入。
func serveAgentInputs(t *testing.T, opt *agent.AgentOpt, replies ...agent.AgentInputResult) {
	// 1. 设置输入请求通道，按顺序回复预设输入，之后回复空输入，测试结束时退出。
	t.Helper()
	requests := make(chan agent.AgentInputRequest)
	opt.InputRequests = requests
	go func() {
		for _, reply := range replies {
			select {
			case request := <-requests:
				request.Reply <- reply
			case <-t.Context().Done():
				return
			}
		}
		for {
			select {
			case request := <-requests:
				request.Reply <- agent.AgentInputResult{}
			case <-t.Context().Done():
				return
			}
		}
	}()
}

// sequenceAgentClient 按调用顺序返回预设的事件序列并记录请求；超出预设次数时报错。
type sequenceAgentClient struct {
	responses  [][]llmkit.Event
	requests   []llmkit.Request
	callErrors []error
	callTimes  []time.Time
}

// Call 按次序返回预设事件或错误，并记录请求与调用时间。
func (c *sequenceAgentClient) Call(_ context.Context, req llmkit.Request, _ llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 检查预设数量，记录本次请求和时间，返回对应错误或事件。
	if len(c.requests) >= len(c.responses) && len(c.requests) >= len(c.callErrors) {
		return nil, errors.New("unexpected model call")
	}
	index := len(c.requests)
	c.requests = append(c.requests, req)
	c.callTimes = append(c.callTimes, time.Now())
	if index < len(c.callErrors) && c.callErrors[index] != nil {
		return nil, c.callErrors[index]
	}
	response := c.responses[index]
	events := make(chan llmkit.Event, len(response))
	for _, event := range response {
		events <- event
	}
	close(events)
	return events, nil
}

// newToolLoopResponses 创建调用工具后回复的两步模型事件。
func newToolLoopResponses() [][]llmkit.Event {
	// 1. 第一步生成 lookup 调用并上报 100/10 用量，第二步生成回答并上报 200/20 用量。
	return [][]llmkit.Event{
		{
			{
				Type:  llmkit.EventToolCallEnd,
				Model: "gpt-upstream",
				ToolCall: &llmkit.StreamingToolCall{
					CallID:    "call-1",
					Name:      "lookup",
					Arguments: `{"q":"x"}`,
				},
			},
			{
				Type: llmkit.EventUsage,
				Usage: &llmkit.Usage{
					PromptTokens:     100,
					CompletionTokens: 10,
				},
			},
			{
				Type:         llmkit.EventDone,
				FinishReason: "tool_calls",
			},
		},
		{
			{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: "完成"},
			},
			{
				Type: llmkit.EventUsage,
				Usage: &llmkit.Usage{
					PromptTokens:     200,
					CompletionTokens: 20,
				},
			},
			{
				Type:         llmkit.EventDone,
				FinishReason: "stop",
			},
		},
	}
}

// TestAgentWorkspaceInstructions 检查全局和目录规则首次加载、去重及压缩后补回。
func TestAgentWorkspaceInstructions(t *testing.T) {
	// 1. 准备首次读取、历史已有规则和规则被摘要覆盖的样例。
	for _, tt := range []struct {
		name             string
		visible, compact bool
		global           bool
	}{
		{name: "first_load"},
		{name: "already_visible", visible: true},
		{name: "reload_after_compaction", visible: true, compact: true},
		{name: "global_first_load", global: true},
		{name: "global_already_visible", global: true, visible: true},
		{name: "global_reload_after_compaction", global: true, visible: true, compact: true},
	} {
		// 2. 运行带规则的上下文，检查首次追加、已加载去重和压缩后重新追加。
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(root, "AGENTS.md")
			if err := os.WriteFile(path, []byte("project-rules-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			var wantInstructions []defs.AgentsInstruction
			if tt.global {
				globalPath := filepath.Join(home, ".agents", "AGENTS.md")
				if err := os.MkdirAll(filepath.Dir(globalPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(globalPath, []byte("global-rules-marker"), 0600); err != nil {
					t.Fatal(err)
				}
				wantInstructions = append(wantInstructions, defs.AgentsInstruction{
					Executor: "local",
					Path:     globalPath,
					Text:     "global-rules-marker",
					IsGlobal: true,
				})
			}
			wantInstructions = append(wantInstructions, defs.AgentsInstruction{
				Executor: "local",
				Path:     path,
				Text:     "project-rules-marker",
			})
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			client := &compactingAgentClient{}
			model := *testModel
			if tt.compact {
				model.ContextWindow = 800
			}
			contextOpt := agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, &model, &model)
			history, err := agent_context.NewAgentContext(contextOpt)
			if err != nil {
				t.Fatal(err)
			}
			var items []*defs.Item
			if tt.visible {
				for index, instruction := range wantInstructions {
					items = append(items, &defs.Item{
						ID:                defs.ItemID(fmt.Sprintf("rule-%d", index)),
						TurnID:            testTurnID,
						Kind:              defs.ItemAgentsInstruction,
						AgentsInstruction: &instruction,
					})
				}
			}
			if tt.compact {
				old := userMessage(strings.Repeat("old work ", 500))
				old.ID = "old-work"
				items = append(items, old)
			}
			items = append(items, userMessage("next"))
			if err := history.Load(&agent_context.ContextLoadParam{Items: items}); err != nil {
				t.Fatal(err)
			}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, &model, history)
			opt.Environments = []executor.ExecutionEnvironment{newTestEnvironment(t, "local", work)}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			rules := 0
			for _, item := range result.Items {
				if item.Kind == defs.ItemAgentsInstruction {
					if rules >= len(wantInstructions) || *item.AgentsInstruction != wantInstructions[rules] {
						t.Fatalf("instruction %d = %+v, want %+v", rules, item.AgentsInstruction, wantInstructions)
					}
					rules++
					if item.TurnID != testTurnID {
						t.Fatal("rule has wrong Turn")
					}
				}
			}
			want := len(wantInstructions)
			if tt.visible && !tt.compact {
				want = 0
			}
			if rules != want {
				t.Fatalf("new rules = %d, want %d", rules, want)
			}
			request := client.requests[len(client.requests)-1]
			var instructionTexts []string
			for _, message := range request.Messages {
				for _, block := range message.Content {
					if strings.Contains(block.Text, "project-rules-marker") || strings.Contains(block.Text, "global-rules-marker") {
						instructionTexts = append(instructionTexts, block.Text)
						if message.Role != llmkit.RoleUser {
							t.Fatal("rules must be user instructions")
						}
					}
				}
			}
			if len(instructionTexts) != len(wantInstructions) {
				t.Fatalf("rule occurrences = %d, want %d", len(instructionTexts), len(wantInstructions))
			}
			for index, instruction := range wantInstructions {
				text := instructionTexts[index]
				if !strings.Contains(text, instruction.Path) || !strings.Contains(text, instruction.Text) {
					t.Fatalf("instruction text %d = %q, want %+v", index, text, instruction)
				}
				if instruction.IsGlobal && (!strings.Contains(text, "此 Executor 的所有工作区") || !strings.Contains(text, "项目目录规则优先")) {
					t.Fatalf("global scope missing: %s", text)
				}
			}
			if tt.compact && len(client.requests) != 2 {
				t.Fatalf("calls = %d, want one compaction and one model call", len(client.requests))
			}
		})
	}
}

// TestAgentMemoryIndexes 检查 Turn 开始时读 memory 环境的 MEMORY.md：按行截断、空文件不放、读失败只记日志。
func TestAgentMemoryIndexes(t *testing.T) {
	// 1. 准备 10 KiB 的多行目录和不含换行的中文，以及刚好 8 KiB、缺文件、空白和读失败的样例。
	var longIndex strings.Builder
	for line := 0; longIndex.Len() < 10<<10; line++ {
		fmt.Fprintf(&longIndex, "- [topic-%04d](topic-%04d.md) — 一句能直接用的结论\n", line, line)
	}
	tests := []struct {
		name string
		// userIndex、projectIndex 是两份 MEMORY.md 的内容，为空表示不建这个文件。
		userIndex, projectIndex string
		// isUserIndexDirectory user/MEMORY.md 是不是一个目录？是的话读它会失败。
		isUserIndexDirectory bool
		// hasNoMemoryEnvironment 对话是不是没有 memory 环境？
		hasNoMemoryEnvironment bool
		// wantPaths 是本轮新加的记忆目录，按顺序。
		wantPaths []string
		// wantTruncated 是 user/MEMORY.md 的预期结果：是否截断，以及检查截断后的正文。
		wantTruncated bool
		checkUserText func(t *testing.T, text string)
		// wantReadWarnings 是读失败的 Warn 日志条数。
		wantReadWarnings int
	}{
		{
			name:         "both_files_loaded_in_order",
			userIndex:    "- 回复用中文\n",
			projectIndex: "- 测试用 table-driven\n",
			wantPaths:    []string{"user/MEMORY.md", "project/MEMORY.md"},
			checkUserText: func(t *testing.T, text string) {
				if text != "- 回复用中文\n" {
					t.Fatalf("text = %q, want the file content unchanged", text)
				}
			},
		},
		{
			name: "missing_files_are_skipped_silently",
		},
		{
			name:         "blank_file_is_skipped",
			userIndex:    "  \n\t\n",
			projectIndex: "- 测试用 table-driven\n",
			wantPaths:    []string{"project/MEMORY.md"},
		},
		{
			name:          "over_limit_cuts_at_last_newline",
			userIndex:     longIndex.String(),
			wantPaths:     []string{"user/MEMORY.md"},
			wantTruncated: true,
			checkUserText: func(t *testing.T, text string) {
				lineLength := len(strings.SplitAfter(longIndex.String(), "\n")[0])
				if len(text) > 8<<10 || len(text)+lineLength <= 8<<10 {
					t.Fatalf("text length = %d, want the longest whole-line prefix not above 8 KiB (line %d bytes)", len(text), lineLength)
				}
				if !strings.HasPrefix(longIndex.String(), text+"\n") {
					t.Fatalf("text does not end at a line of the file: %q", text[max(0, len(text)-60):])
				}
			},
		},
		{
			// 一行 10 KiB 的中文没有换行符，退到上限以内最后一个完整字符：3500 个字符截成 2730 个。
			name:          "no_newline_within_limit",
			userIndex:     strings.Repeat("中", 3500),
			wantPaths:     []string{"user/MEMORY.md"},
			wantTruncated: true,
			checkUserText: func(t *testing.T, text string) {
				if !utf8.ValidString(text) || len(text) > 8<<10 || text != strings.Repeat("中", 2730) {
					t.Fatalf("text has %d bytes (valid UTF-8: %v), want 2730 whole characters", len(text), utf8.ValidString(text))
				}
			},
		},
		{
			name:      "exactly_limit_is_not_truncated",
			userIndex: strings.Repeat("a", 8<<10),
			wantPaths: []string{"user/MEMORY.md"},
			checkUserText: func(t *testing.T, text string) {
				if len(text) != 8<<10 {
					t.Fatalf("text length = %d, want the whole 8 KiB", len(text))
				}
			},
		},
		{
			// 8 KiB 的完整行后面只有一个换行符：整个文件就是这些内容，不算截断，也不丢最后一行。
			name:      "exactly_limit_plus_trailing_newline_is_not_truncated",
			userIndex: strings.Repeat(strings.Repeat("a", 63)+"\n", 127) + strings.Repeat("a", 64) + "\n",
			wantPaths: []string{"user/MEMORY.md"},
			checkUserText: func(t *testing.T, text string) {
				if want := strings.Repeat(strings.Repeat("a", 63)+"\n", 127) + strings.Repeat("a", 64); text != want {
					t.Fatalf("text has %d bytes, want the 8 KiB without the trailing newline", len(text))
				}
			},
		},
		{
			// 上限处正好是行尾、后面还有内容：在上限处截断，不丢最后一整行，并标上截断。
			name:          "line_boundary_at_limit_with_more_content_is_truncated",
			userIndex:     strings.Repeat(strings.Repeat("a", 63)+"\n", 127) + strings.Repeat("a", 64) + "\nrest\n",
			wantPaths:     []string{"user/MEMORY.md"},
			wantTruncated: true,
			checkUserText: func(t *testing.T, text string) {
				if want := strings.Repeat(strings.Repeat("a", 63)+"\n", 127) + strings.Repeat("a", 64); text != want {
					t.Fatalf("text has %d bytes, want the whole 8 KiB of complete lines", len(text))
				}
			},
		},
		{
			name:                 "read_failure_only_logs_a_warning",
			projectIndex:         "- 测试用 table-driven\n",
			isUserIndexDirectory: true,
			wantPaths:            []string{"project/MEMORY.md"},
			wantReadWarnings:     1,
		},
		{
			name:                   "without_memory_environment",
			userIndex:              "- 回复用中文\n",
			hasNoMemoryEnvironment: true,
		},
	}
	// 2. 把目录放进记忆工作区，运行一轮，核对新加的 Item、请求里的消息和 Warn 日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDirectory := t.TempDir()
			const projectName = "projects/local/-repo"
			userDirectory, projectDirectory := filepath.Join(dataDirectory, "user"), filepath.Join(dataDirectory, projectName)
			for directory, content := range map[string]string{userDirectory: tt.userIndex, projectDirectory: tt.projectIndex} {
				if content == "" {
					continue
				}
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "MEMORY.md"), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.isUserIndexDirectory {
				if err := os.MkdirAll(filepath.Join(userDirectory, "MEMORY.md"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			memoryWorkspace, err := workspace.NewMemoryWorkspace(workspace.NewMemoryWorkspaceOpt(dataDirectory, projectName))
			if err != nil {
				t.Fatal(err)
			}
			localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			pipeline := telemetrytest.NewPipeline(t)
			client := &compactingAgentClient{}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("你好")}))
			opt.Environments = []executor.ExecutionEnvironment{newTestEnvironment(t, "local", localWorkspace)}
			if !tt.hasNoMemoryEnvironment {
				opt.Environments = append(opt.Environments, newTestEnvironment(t, defs.MemoryExecutorID, memoryWorkspace))
			}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), nil)
			if err != nil {
				t.Fatal(err)
			}
			var indexes []*defs.MemoryIndex
			for _, item := range result.Items {
				if item.Kind == defs.ItemMemoryIndex {
					if item.TurnID != testTurnID {
						t.Fatalf("memory index item turn = %q, want %q", item.TurnID, testTurnID)
					}
					indexes = append(indexes, item.MemoryIndex)
				}
			}
			var paths []string
			for _, index := range indexes {
				paths = append(paths, index.Path)
			}
			if !slices.Equal(paths, tt.wantPaths) {
				t.Fatalf("memory index paths = %v, want %v", paths, tt.wantPaths)
			}
			if len(indexes) > 0 && indexes[0].Path == "user/MEMORY.md" {
				if indexes[0].IsTruncated != tt.wantTruncated {
					t.Fatalf("IsTruncated = %v, want %v", indexes[0].IsTruncated, tt.wantTruncated)
				}
				if tt.checkUserText != nil {
					tt.checkUserText(t, indexes[0].Text)
				}
			}
			// 3. 每份记忆目录以用户消息发给模型，格式同 FormatMemoryIndex；本轮读失败只记一条 Warn。
			var requestTexts []string
			for _, message := range client.requests[0].Messages {
				for _, block := range message.Content {
					if strings.Contains(block.Text, "跨对话记忆目录") {
						requestTexts = append(requestTexts, block.Text)
						if message.Role != llmkit.RoleUser {
							t.Fatal("memory index must be a user message")
						}
					}
				}
			}
			if len(requestTexts) != len(indexes) {
				t.Fatalf("memory messages in request = %d, want %d", len(requestTexts), len(indexes))
			}
			for position, index := range indexes {
				if requestTexts[position] != prompt.FormatMemoryIndex(*index) {
					t.Fatalf("memory message %d = %q, want %q", position, requestTexts[position], prompt.FormatMemoryIndex(*index))
				}
			}
			if warnings := pipeline.GetLogRecords(t, "memory index read failed"); len(warnings) != tt.wantReadWarnings {
				t.Fatalf("read warnings = %v, want %d", warnings, tt.wantReadWarnings)
			}
		})
	}
}

// TestAgentMemoryIndexesFollowProject 检查项目记忆按项目判断是否已在上下文里：换了项目就放入新项目的目录，用户级目录不重复放。
func TestAgentMemoryIndexesFollowProject(t *testing.T) {
	// 1. 上下文里已有用户级目录和项目 A 的目录，记忆环境指向不同项目。
	const projectA, projectB = "projects/local/-repo-a", "projects/local/-repo-b"
	tests := []struct {
		name string
		// environmentProject 是记忆环境指向的项目。
		environmentProject string
		// wantProjects 是本轮新放进上下文的项目记忆，按顺序；用户级目录已在上下文里，不重复放。
		wantProjects []string
	}{
		{name: "same_project_is_not_appended_again", environmentProject: projectA},
		{name: "other_project_is_appended", environmentProject: projectB, wantProjects: []string{projectB}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 记忆根目录下放好用户级目录和两个项目的目录。
			memoryDirectory := t.TempDir()
			for name, content := range map[string]string{
				"user":   "- 回复用中文\n",
				projectA: "- A 项目用 table-driven\n",
				projectB: "- B 项目用 testify\n",
			} {
				if err := os.MkdirAll(filepath.Join(memoryDirectory, name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(memoryDirectory, name, "MEMORY.md"), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			memoryWorkspace, err := workspace.NewMemoryWorkspace(workspace.NewMemoryWorkspaceOpt(memoryDirectory, tt.environmentProject))
			if err != nil {
				t.Fatal(err)
			}
			localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			// 3. 运行一轮，核对新放进上下文的记忆目录。
			history := newTestContext(t, "", []*defs.Item{
				userMessage("你好"),
				{ID: "user-memory", TurnID: testTurnID, Kind: defs.ItemMemoryIndex, MemoryIndex: &defs.MemoryIndex{Path: "user/MEMORY.md", Text: "- 回复用中文\n"}},
				{ID: "project-memory", TurnID: testTurnID, Kind: defs.ItemMemoryIndex, MemoryIndex: &defs.MemoryIndex{Path: "project/MEMORY.md", Project: projectA, Text: "- A 项目用 table-driven\n"}},
			})
			opt := agent.NewAgentOpt(testTurnID, testSessionID, &compactingAgentClient{}, testModel, history)
			opt.Environments = []executor.ExecutionEnvironment{
				newTestEnvironment(t, "local", localWorkspace),
				newTestEnvironment(t, defs.MemoryExecutorID, memoryWorkspace),
			}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var gotProjects []string
			for _, item := range result.Items {
				if item.Kind == defs.ItemMemoryIndex {
					if item.MemoryIndex.Path != "project/MEMORY.md" {
						t.Fatalf("appended memory index path = %q, want only project/MEMORY.md", item.MemoryIndex.Path)
					}
					gotProjects = append(gotProjects, item.MemoryIndex.Project)
				}
			}
			if !slices.Equal(gotProjects, tt.wantProjects) {
				t.Fatalf("appended projects = %v, want %v", gotProjects, tt.wantProjects)
			}
		})
	}
}

// TestAgentWorkspaceInstructionsFailure 检查默认和非默认环境读取规则失败时的运行结果。
func TestAgentWorkspaceInstructionsFailure(t *testing.T) {
	// 1. 准备元数据失败、规则读取失败和读取时取消的工作区。
	for _, tt := range []struct {
		name      string
		operation string
		cancel    bool
	}{
		{
			name:      "offline_workspace",
			operation: "metadata",
		},
		{
			name:      "unreadable_rules",
			operation: "read",
		},
		{
			name:      "canceled_discovery",
			operation: "metadata",
			cancel:    true,
		},
	} {
		// 2. 执行故障环境的规则读取，检查默认预读取消结束运行，非默认错误只跳过规则。
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("healthy-workspace-rules"), 0600); err != nil {
				t.Fatal(err)
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			broken := &testAgentsWorkspace{
				Workspace: work,
				operation: tt.operation,
			}
			if tt.cancel {
				broken.cancel = cancel
			}
			healthyEnvironment := newTestEnvironment(t, "healthy", work, &testTool{backend: &testToolExecutor{}})
			brokenEnvironment := newTestEnvironment(t, "broken", broken, &testTool{backend: &testToolExecutor{}})
			// 取消样例把故障环境设为默认环境；其他样例在远端工具成功后读取规则并跳过读取错误。
			environments := []executor.ExecutionEnvironment{healthyEnvironment, brokenEnvironment}
			if tt.cancel {
				environments = []executor.ExecutionEnvironment{brokenEnvironment, healthyEnvironment}
			}
			responses := newToolLoopResponses()
			responses[0][0].ToolCall.Arguments = `{"executor":"broken"}`
			client := &sequenceAgentClient{responses: responses}
			history := newTestContext(t, "", []*defs.Item{userMessage("继续工作")})
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, history)
			opt.ToolExecutor, opt.Environments = newTestToolExecutor(t, environments...), environments
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(ctx, nil)
			if tt.cancel {
				if !errors.Is(err, context.Canceled) || len(client.requests) != 0 {
					t.Fatalf("error = %v, model calls = %d", err, len(client.requests))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantAttempts := 1
			if tt.operation == "read" {
				wantAttempts = 2 // 全局规则和项目规则分别尝试读取。
			}
			if broken.attempts != wantAttempts || len(client.requests) != 2 {
				t.Fatalf("failed discovery attempts = %d, model calls = %d; want %d and 2", broken.attempts, len(client.requests), wantAttempts)
			}
			for _, request := range client.requests {
				encoded, err := json.Marshal(request.Messages)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(encoded), "healthy-workspace-rules") {
					t.Fatal("healthy workspace rules missing from model request")
				}
			}
			for _, item := range result.Items {
				if item.AgentsInstruction != nil && item.AgentsInstruction.Executor != "healthy" {
					t.Fatalf("unexpected rule = %+v", item.AgentsInstruction)
				}
			}
		})
	}
}

// testAgentsWorkspace 记录规则读取次数，并可模拟工作区不可用或取消。
type testAgentsWorkspace struct {
	workspace.Workspace
	operation        string
	homeDirectory    string
	homeQueries      int
	attempts         int
	metadataCalls    int
	instructionReads int
	cancel           context.CancelFunc
}

// GetHomeDirectory 记录查询次数，返回指定的测试主目录或工作区自己的主目录。
func (w *testAgentsWorkspace) GetHomeDirectory(ctx context.Context) (string, error) {
	// 1. 累计查询次数，指定主目录时直接返回，否则查询真实工作区。
	w.homeQueries++
	if w.homeDirectory != "" {
		return w.homeDirectory, nil
	}
	return w.Workspace.GetHomeDirectory(ctx)
}

// GetMetadata 记录工作区查询，并可注入元数据错误或取消。
func (w *testAgentsWorkspace) GetMetadata(ctx context.Context, path string, followSymlinks bool) (*workspace.FileMetadata, error) {
	// 1. 累计查询次数，按配置取消或报错，其他查询交给工作区。
	w.metadataCalls++
	if w.operation == "metadata" {
		w.attempts++
		if w.cancel != nil {
			w.cancel()
			return nil, ctx.Err()
		}
		return nil, errors.New("workspace offline")
	}
	return w.Workspace.GetMetadata(ctx, path, followSymlinks)
}

// ReadFile 记录规则读取次数，并可注入读取权限错误。
func (w *testAgentsWorkspace) ReadFile(ctx context.Context, path string, offset int64, limit int) (*workspace.FileContent, error) {
	// 1. 识别 AGENTS.md，累计读取次数并按配置返回权限错误或文件内容。
	if filepath.Base(path) == "AGENTS.md" {
		w.instructionReads++
	}
	if w.operation == "read" && filepath.Base(path) == "AGENTS.md" {
		w.attempts++
		return nil, os.ErrPermission
	}
	return w.Workspace.ReadFile(ctx, path, offset, limit)
}

// TestAgentDefaultExecutorAgentsInstructions 检查默认规则预读和其他环境成功执行后的规则加载。
func TestAgentDefaultExecutorAgentsInstructions(t *testing.T) {
	// 1. 准备默认规则不可读、可读、远端工具调用和无环境的样例。
	for _, tt := range []struct {
		name             string
		noExecutors      bool
		defaultRules     string
		defaultReadFails bool
		remoteRules      string
		readRemoteFile   bool
		// wantToolExecutor 是工具结果记下的 Executor，为空表示没有工具调用。
		wantToolExecutor string
		// wantError 非空表示 Run 失败；失败发生在模型调用之前，wantRequests 为 0。
		wantError    string
		wantRequests int
		// firstRequestHas 与 firstRequestLacks 是第一次模型请求里应出现与不应出现的规则；secondRequestHas 同理指第二次。
		firstRequestHas, firstRequestLacks, secondRequestHas []string
	}{
		{
			name:             "default_rules_unreadable_fails_turn",
			defaultRules:     "default-rules",
			defaultReadFails: true,
			wantError:        `load AGENTS.md from executor "local"`,
		},
		{
			name:              "default_rules_preloaded",
			defaultRules:      "default-rules",
			remoteRules:       "remote-rules",
			wantRequests:      1,
			firstRequestHas:   []string{"default-rules"},
			firstRequestLacks: []string{"remote-rules"},
		},
		{
			name:              "second_executor_rules_after_successful_read_file",
			defaultRules:      "default-rules",
			remoteRules:       "remote-rules",
			readRemoteFile:    true,
			wantToolExecutor:  "remote",
			wantRequests:      2,
			firstRequestHas:   []string{"default-rules"},
			firstRequestLacks: []string{"remote-rules"},
			secondRequestHas:  []string{"default-rules", "remote-rules"},
		},
		{
			name:         "no_executors_no_preload",
			noExecutors:  true,
			wantRequests: 1,
		},
	} {
		// 2. 运行本地和远端读取，检查默认规则在首个请求出现，远端规则在成功使用后出现。
		t.Run(tt.name, func(t *testing.T) {
			defaultRoot, remoteRoot, outsideRoot := t.TempDir(), t.TempDir(), t.TempDir()
			for root, rules := range map[string]string{defaultRoot: tt.defaultRules, remoteRoot: tt.remoteRules} {
				if rules == "" {
					continue
				}
				if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(rules), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// 读取 remote 工作区之外的文件，目录规则由工具成功后的工作区读取提供。
			outsideFile := filepath.Join(outsideRoot, "note.txt")
			if err := os.WriteFile(outsideFile, []byte("note"), 0600); err != nil {
				t.Fatal(err)
			}
			newReadFileEnvironment := func(id string, work workspace.Workspace) executor.ExecutionEnvironment {
				readFileTool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(work))
				if err != nil {
					t.Fatal(err)
				}
				return newTestEnvironment(t, id, work, readFileTool)
			}
			defaultWork, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(defaultRoot))
			if err != nil {
				t.Fatal(err)
			}
			remoteWork, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(remoteRoot))
			if err != nil {
				t.Fatal(err)
			}
			if tt.defaultReadFails {
				defaultWork = &testAgentsWorkspace{
					Workspace: defaultWork,
					operation: "read",
				}
			}
			final := []llmkit.Event{
				{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "done"},
				},
				{
					Type:         llmkit.EventDone,
					FinishReason: "stop",
				},
			}
			client := &sequenceAgentClient{responses: [][]llmkit.Event{final}}
			if tt.readRemoteFile {
				client.responses = [][]llmkit.Event{
					{
						{
							Type: llmkit.EventToolCallEnd,
							ToolCall: &llmkit.StreamingToolCall{
								CallID:    "read",
								Name:      "read_file",
								Arguments: fmt.Sprintf(`{"executor":"remote","path":%q}`, outsideFile),
							},
						},
						{
							Type:         llmkit.EventDone,
							FinishReason: "tool_calls",
						},
					},
					final,
				}
			}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("开始")}))
			if !tt.noExecutors {
				opt.Environments = []executor.ExecutionEnvironment{newReadFileEnvironment("local", defaultWork), newReadFileEnvironment("remote", remoteWork)}
				opt.ToolExecutor = newTestToolExecutor(t, opt.Environments...)
			}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(t.Context(), nil)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) || !errors.Is(err, os.ErrPermission) {
					t.Fatalf("Run error = %v, want %q wrapping the read error", err, tt.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(client.requests) != tt.wantRequests {
				t.Fatalf("model requests = %d, want %d", len(client.requests), tt.wantRequests)
			}
			for _, item := range result.Items {
				if item.ToolResult != nil && item.ToolResult.Executor != tt.wantToolExecutor {
					t.Fatalf("tool result executor = %q, want %q", item.ToolResult.Executor, tt.wantToolExecutor)
				}
			}
			requestHas := func(index int, marker string) bool {
				encoded, err := json.Marshal(client.requests[index].Messages)
				if err != nil {
					t.Fatal(err)
				}
				return strings.Contains(string(encoded), marker)
			}
			for _, marker := range tt.firstRequestHas {
				if !requestHas(0, marker) {
					t.Fatalf("first request lacks %q", marker)
				}
			}
			for _, marker := range tt.firstRequestLacks {
				if requestHas(0, marker) {
					t.Fatalf("first request has %q", marker)
				}
			}
			for _, marker := range tt.secondRequestHas {
				if !requestHas(1, marker) {
					t.Fatalf("second request lacks %q", marker)
				}
			}
		})
	}
}

// TestAgentWorkspaceInstructionsDiscoveryTiming 检查目录规则在首次成功使用环境后读取并复用。
func TestAgentWorkspaceInstructionsDiscoveryTiming(t *testing.T) {
	// 1. 准备默认远端、首次远端使用、未使用和工具失败的样例。
	for _, tt := range []struct {
		name          string
		remoteDefault bool
		useRemote     bool
		toolFails     bool
	}{
		{
			name:          "default_executor_preloaded",
			remoteDefault: true,
			useRemote:     true,
		},
		{
			name:      "first_successful_use",
			useRemote: true,
		},
		{name: "unused_workspace_not_accessed"},
		{
			name:      "failed_tool_does_not_probe",
			useRemote: true,
			toolFails: true,
		},
	} {
		// 2. 运行两步工具调用，检查未使用和工具失败的环境没有读取规则，成功使用后只读取一次。
		t.Run(tt.name, func(t *testing.T) {
			remote, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/project/app", map[string]*workspace.VirtualFile{
				"/project/.git/config":           {Content: []byte("git-marker")},
				"/project/AGENTS.md":             {Content: []byte("remote-parent-rules")},
				"/home/tester/.agents/AGENTS.md": {Content: []byte("remote-global-rules")},
			}))
			if err != nil {
				t.Fatal(err)
			}
			observed := &testAgentsWorkspace{Workspace: remote, homeDirectory: "/home/tester"}
			backend := &testToolExecutor{}
			if tt.toolFails {
				backend.toolError = &defs.ToolError{
					Code:    "unavailable",
					Message: "offline",
				}
			}
			localEnvironment := newTestEnvironment(t, "local", nil, &testTool{backend: &testToolExecutor{}})
			remoteEnvironment := newTestEnvironment(t, "remote", observed, &testTool{backend: backend})
			environments := []executor.ExecutionEnvironment{localEnvironment, remoteEnvironment}
			if tt.remoteDefault {
				environments = []executor.ExecutionEnvironment{remoteEnvironment, localEnvironment}
			}
			selected := "local"
			if tt.useRemote {
				selected = "remote"
			}
			client := &sequenceAgentClient{}
			for step := range 2 {
				client.responses = append(client.responses, []llmkit.Event{
					{
						Type: llmkit.EventToolCallEnd,
						ToolCall: &llmkit.StreamingToolCall{
							CallID:    fmt.Sprintf("call-%d", step),
							Name:      "lookup",
							Arguments: fmt.Sprintf(`{"executor":%q}`, selected),
						},
					},
					{
						Type:         llmkit.EventDone,
						FinishReason: "tool_calls",
					},
				})
			}
			client.responses = append(client.responses, newToolLoopResponses()[1])
			history := newTestContext(t, "", []*defs.Item{userMessage("完成任务")})
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, history)
			opt.ToolExecutor, opt.Environments = newTestToolExecutor(t, environments...), environments
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := instance.Run(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			wantLoaded := tt.remoteDefault || (tt.useRemote && !tt.toolFails)
			if wantLoaded {
				// 检查全局、根目录和工作目录各读取一次，后续工具步骤复用读取结果。
				if observed.homeQueries != 1 || observed.instructionReads != 3 {
					t.Fatalf("home queries = %d, instruction reads = %d; want 1 and 3", observed.homeQueries, observed.instructionReads)
				}
			} else if observed.homeQueries != 0 || observed.metadataCalls != 0 || observed.instructionReads != 0 {
				t.Fatalf("unexpected workspace access: home = %d, metadata = %d, reads = %d", observed.homeQueries, observed.metadataCalls, observed.instructionReads)
			}
			if len(client.requests) != 3 {
				t.Fatalf("model requests = %d, want 3", len(client.requests))
			}
			for step, request := range client.requests {
				encoded, err := json.Marshal(request.Messages)
				if err != nil {
					t.Fatal(err)
				}
				want := tt.remoteDefault || (step > 0 && wantLoaded)
				if got := strings.Contains(string(encoded), "remote-parent-rules"); got != want {
					t.Fatalf("step %d has rules = %v, want %v", step, got, want)
				}
				if got := strings.Contains(string(encoded), "remote-global-rules"); got != want {
					t.Fatalf("step %d has global rules = %v, want %v", step, got, want)
				}
			}
		})
	}
}

// TestAgentModelRetryPolicy 检查模型错误重试次数、等待间隔和失败记录。
func TestAgentModelRetryPolicy(t *testing.T) {
	// 1. 准备可重试、不可重试、耗尽次数和等待期间取消的错误。
	lastFailure := &llmkit.Error{
		Stage:      llmkit.ErrorStageUpstream,
		StatusCode: 503,
		Retryable:  true,
		Cause:      errors.New("last upstream failure"),
	}
	for _, tt := range []struct {
		name      string
		callError error
		exhaust   bool
		cancelAt  time.Duration
		wantCalls int
		wantWait  time.Duration
	}{
		{
			name: "429_recovers",
			callError: &llmkit.Error{
				Stage:      llmkit.ErrorStageUpstream,
				StatusCode: 429,
				Retryable:  true,
			},
			wantCalls: 2,
			wantWait:  time.Second,
		},
		{
			name: "503_recovers",
			callError: &llmkit.Error{
				Stage:      llmkit.ErrorStageUpstream,
				StatusCode: 503,
				Retryable:  true,
			},
			wantCalls: 2,
			wantWait:  time.Second,
		},
		{
			name: "connection_recovers",
			callError: &llmkit.Error{
				Stage:     llmkit.ErrorStageConnect,
				Retryable: true,
				Cause:     errors.New("connection refused"),
			},
			wantCalls: 2,
			wantWait:  time.Second,
		},
		{
			name: "401_stops",
			callError: &llmkit.Error{
				Stage:      llmkit.ErrorStageUpstream,
				StatusCode: 401,
			},
			wantCalls: 1,
		},
		{
			name: "invalid_parameters_stop",
			callError: &llmkit.Error{
				Stage: llmkit.ErrorStageEncode,
				Cause: errors.New("invalid parameters"),
			},
			wantCalls: 1,
		},
		{
			name: "non_retryable_decode_overrides_eof",
			callError: fmt.Errorf("decode response: %w", &llmkit.Error{
				Stage: llmkit.ErrorStageDecode,
				Cause: io.ErrUnexpectedEOF,
			}),
			wantCalls: 1,
		},
		{
			name:      "upstream_deadline_with_live_parent_has_marker",
			callError: fmt.Errorf("upstream request: %w", context.DeadlineExceeded),
			wantCalls: 1,
		},
		{
			name:      "exhaustion_returns_last_failure_after_181_seconds",
			callError: lastFailure,
			exhaust:   true,
			wantCalls: 11,
			wantWait:  181 * time.Second,
		},
		{
			name:      "cancel_during_30_second_backoff",
			callError: lastFailure,
			exhaust:   true,
			cancelAt:  32 * time.Second,
			wantCalls: 6,
			wantWait:  32 * time.Second,
		},
	} {
		// 2. 在虚拟时间中执行重试，检查间隔、尝试次数、错误标记和每次独立调用 ID。
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &sequenceAgentClient{
					responses:  [][]llmkit.Event{nil, newToolLoopResponses()[1]},
					callErrors: []error{tt.callError},
				}
				if tt.exhaust {
					client.callErrors = make([]error, 11)
					for index := range client.callErrors {
						client.callErrors[index] = &llmkit.Error{
							Stage:     llmkit.ErrorStageUpstream,
							Retryable: true,
							Cause:     fmt.Errorf("transient failure %d", index+1),
						}
					}
					client.callErrors[len(client.callErrors)-1] = tt.callError
				}
				history := newTestContext(t, "retry instructions", []*defs.Item{userMessage("answer")})
				opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, history)
				opt.ToolExecutor = &testToolExecutor{}
				instance, err := agent.NewAgent(opt)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.cancelAt != 0 {
					time.AfterFunc(tt.cancelAt, cancel)
				}
				events := make(chan *agent.AgentEvent)
				collected := make(chan []*agent.AgentEvent, 1)
				go func() {
					var received []*agent.AgentEvent
					for event := range events {
						received = append(received, event)
					}
					collected <- received
				}()
				started := time.Now()
				result, err := instance.Run(ctx, events)
				received := <-collected
				if len(client.requests) != tt.wantCalls || time.Since(started) != tt.wantWait {
					t.Fatalf("calls = %d, wait = %s; want %d, %s", len(client.requests), time.Since(started), tt.wantCalls, tt.wantWait)
				}
				wantIntervals := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30, 30, 30}
				for index := 1; index < len(client.callTimes); index++ {
					if got := client.callTimes[index].Sub(client.callTimes[index-1]); got != wantIntervals[index-1]*time.Second {
						t.Errorf("interval %d = %s", index, got)
					}
				}
				wantFailure := tt.wantCalls == 1 || tt.exhaust
				switch {
				case tt.cancelAt != 0:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("Run() error = %v, want cancellation", err)
					}
				case wantFailure:
					if !errors.Is(err, tt.callError) {
						t.Fatalf("Run() error = %v, want original failure %v", err, tt.callError)
					}
				default:
					if err != nil {
						t.Fatal(err)
					}
				}
				if (result.FinalModelCallID == "") != wantFailure {
					t.Fatalf("final model call = %s", result.FinalModelCallID)
				}
				if result.Stats.Steps != tt.wantCalls || result.Stats.InferenceCalls != tt.wantCalls {
					t.Fatalf("stats = %+v", result.Stats)
				}
				calls := getAgentEventsOfType(received, agent.AgentEventModelCallFinished)
				if len(calls) != tt.wantCalls {
					t.Fatalf("call events = %d", len(calls))
				}
				identities := make(map[defs.ModelCallID]bool)
				for _, event := range calls {
					id := event.ModelCall.ModelCallID
					if id == "" || identities[id] {
						t.Fatalf("duplicate or empty model call ID: %s", id)
					}
					identities[id] = true
				}
				wantMarkers := tt.wantCalls
				if !wantFailure {
					wantMarkers--
				}
				markers := 0
				for _, item := range result.Items {
					if item.ModelError != nil {
						expectedError := client.callErrors[markers]
						markers++
						if item.ModelError.Code != "model_call_error" || item.ModelError.Message != expectedError.Error() || !identities[item.ModelCallID] {
							t.Fatalf("marker = %+v", item)
						}
					}
				}
				if markers != wantMarkers {
					t.Fatalf("markers = %d, want %d", markers, wantMarkers)
				}
				for _, request := range client.requests {
					if !reflect.DeepEqual(request.Messages, client.requests[0].Messages) || !reflect.DeepEqual(request.Tools, client.requests[0].Tools) {
						t.Fatal("retry request changed")
					}
				}
				wantMessages := 2
				if !wantFailure {
					wantMessages++
				}
				if messages := contextMessages(t, history); len(messages) != wantMessages {
					t.Fatalf("context messages = %+v", messages)
				}
				assertAgentEvents(t, received, result.Items)
			})
		})
	}
}

// TestAgentRunKeepsCompletedOutputOnFailure 检查失败尝试保留输出，但不写入后续模型历史和执行工具。
func TestAgentRunKeepsCompletedOutputOnFailure(t *testing.T) {
	// 1. 准备部分文本、完整文本与推理、完整工具调用三种失败输出，各配 EOF、流错误和无详情流错误三种失败方式。
	for _, output := range []struct {
		name   string
		events []llmkit.Event
	}{
		{
			name: "partial_text",
			events: []llmkit.Event{{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: "failed-text"},
			}},
		},
		{
			name: "completed_text_and_reasoning",
			events: []llmkit.Event{
				{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "failed-text"},
				},
				{Type: llmkit.EventContentBlockStop},
				{
					Type:            llmkit.EventReasoningDone,
					Reasoning:       &llmkit.ReasoningContent{Content: []string{"failed-reasoning"}},
					ReasoningStatus: llmkit.ReasoningCompleted,
				},
			},
		},
		{
			name: "completed_tool_call",
			events: []llmkit.Event{
				{
					Type: llmkit.EventToolCallEnd,
					ToolCall: &llmkit.StreamingToolCall{
						CallID:    "failed-tool",
						Name:      "lookup",
						Arguments: `{"q":"failed-arguments"}`,
					},
				},
			},
		},
	} {
		for _, failure := range []struct {
			name          string
			events        []llmkit.Event
			code, message string
		}{
			{
				name:    "eof",
				code:    "unexpected_eof",
				message: "model stream closed before completion: unexpected EOF",
			},
			{
				name: "stream_error",
				events: []llmkit.Event{
					{
						Type: llmkit.EventError,
						Error: &llmkit.ErrorPayload{
							Code:    "overloaded",
							Message: "failed-stream",
						},
					},
				},
				code:    "overloaded",
				message: "failed-stream",
			},
			{
				name:    "stream_error_without_payload",
				events:  []llmkit.Event{{Type: llmkit.EventError}},
				code:    "model_stream_error",
				message: "model stream reported an error without details",
			},
		} {
			// 2. 执行失败后恢复的工具循环，检查失败输出只保留在草稿和事件中，统计包含全部尝试。
			t.Run(output.name+"/"+failure.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					pipeline := telemetrytest.NewPipeline(t)
					failed := slices.Concat(output.events, []llmkit.Event{
						{
							Type: llmkit.EventUsage,
							Usage: &llmkit.Usage{
								PromptTokens:     7,
								CompletionTokens: 3,
							},
						},
					}, failure.events)
					successTool := []llmkit.Event{
						{
							Type: llmkit.EventToolCallEnd,
							ToolCall: &llmkit.StreamingToolCall{
								CallID:    "successful-tool",
								Name:      "lookup",
								Arguments: `{}`,
							},
						},
						{
							Type: llmkit.EventUsage,
							Usage: &llmkit.Usage{
								PromptTokens:     5,
								CompletionTokens: 2,
							},
						},
						{
							Type:         llmkit.EventDone,
							FinishReason: "tool_calls",
						},
					}
					client := &sequenceAgentClient{responses: [][]llmkit.Event{newToolLoopResponses()[0], failed, successTool, newToolLoopResponses()[1]}}
					history := &measuredRequestContext{AgentContext: newTestContext(t, "", []*defs.Item{userMessage("answer")})}
					model := newTestModel(&llmkit.Target{
						Protocol: llmkit.ProtocolOpenAIChat,
						Model:    "retry-test",
					}, 0)
					opt := agent.NewAgentOpt(testTurnID, testSessionID, client, model, history)
					toolExecutor := &testToolExecutor{}
					opt.ToolExecutor = toolExecutor
					instance, err := agent.NewAgent(opt)
					if err != nil {
						t.Fatal(err)
					}
					events := make(chan *agent.AgentEvent)
					collected := make(chan []*agent.AgentEvent, 1)
					go func() {
						var received []*agent.AgentEvent
						for event := range events {
							received = append(received, event)
						}
						collected <- received
					}()
					result, err := instance.Run(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), events)
					received := <-collected
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(toolExecutor.calls, []string{"call-1", "successful-tool"}) {
						t.Fatalf("executed tools = %v, want only successful calls without replay", toolExecutor.calls)
					}
					if result.Stats.Steps != 4 || result.Stats.InferenceCalls != 4 || result.Stats.ToolCalls != 2 || result.Stats.Usage != (defs.TokenUsage{
						Input:  312,
						Output: 35,
					}) {
						t.Fatalf("stats = %+v", result.Stats)
					}
					if !reflect.DeepEqual(client.requests[1].Messages, client.requests[2].Messages) {
						t.Fatal("retry did not reuse original input")
					}
					for _, request := range client.requests {
						payload, err := json.Marshal(request.Messages)
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(string(payload), "failed-") || strings.Contains(string(payload), "model-error-event") || strings.Contains(string(payload), "missing_output") {
							t.Fatalf("failed output entered request: %s", payload)
						}
					}
					payload, err := json.Marshal(contextMessages(t, history))
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(payload), "failed-") || strings.Contains(string(payload), "model-error-event") {
						t.Fatalf("failed output entered Context: %s", payload)
					}
					calls := getAgentEventsOfType(received, agent.AgentEventModelCallFinished)
					if len(calls) != 4 || result.FinalModelCallID != calls[3].ModelCall.ModelCallID {
						t.Fatalf("call events = %+v, result = %+v", calls, result)
					}
					if len(history.reports) != 3 {
						t.Fatalf("context reports = %d, want only the three complete calls", len(history.reports))
					}
					for index, callIndex := range []int{0, 2, 3} {
						if history.reports[index].ModelCallID != calls[callIndex].ModelCall.ModelCallID {
							t.Fatalf("report %d identity = %s, want complete call %s", index, history.reports[index].ModelCallID, calls[callIndex].ModelCall.ModelCallID)
						}
					}
					failedID := calls[1].ModelCall.ModelCallID
					markers, failedOutputs := 0, 0
					for _, item := range result.Items {
						if item.ModelCallID != failedID {
							continue
						}
						if item.ModelError != nil {
							markers++
							if item.ModelError.Code != failure.code || item.ModelError.Message != failure.message {
								t.Fatalf("error marker = %+v", item.ModelError)
							}
						} else {
							failedOutputs++
							switch item.Kind {
							case defs.ItemMessage:
								if item.Message.Content[0].Text.Text != "failed-text" {
									t.Fatalf("failed text was lost: %+v", item)
								}
							case defs.ItemReasoning:
								if !slices.Equal(item.Reasoning.Content, []string{"failed-reasoning"}) || item.Reasoning.Status != defs.ReasoningCompleted {
									t.Fatalf("completed reasoning changed: %+v", item.Reasoning)
								}
							case defs.ItemToolCall:
								if item.ToolCall.CallID != "failed-tool" || item.ToolCall.Status != defs.ToolCallReady {
									t.Fatalf("completed tool call changed: %+v", item.ToolCall)
								}
							}
						}
					}
					wantFailedOutputs := 1
					if output.name == "completed_text_and_reasoning" {
						wantFailedOutputs = 2
					}
					if markers != 1 || failedOutputs != wantFailedOutputs {
						t.Fatalf("failed history = %d outputs, %d markers", failedOutputs, markers)
					}
					spans := pipeline.WaitEndedSpans(t, "chat retry-test", 4)
					identities := make(map[string]bool)
					for index, span := range spans {
						id := telemetrytest.GetSpanAttribute(span, "openvaala.model_call.id")
						if id == "" || identities[id] || id != string(calls[index].ModelCall.ModelCallID) {
							t.Fatalf("span %d model ID = %s", index, id)
						}
						identities[id] = true
						if got, want := telemetrytest.GetSpanAttribute(span, "openvaala.turn.step"), []string{"1", "2", "2", "3"}[index]; got != want {
							t.Fatalf("span %d step = %s, want %s", index, got, want)
						}
						wantStatus := codes.Unset
						if index == 1 {
							wantStatus = codes.Error
						}
						if span.Status().Code != wantStatus {
							t.Fatalf("span %d status = %v", index, span.Status())
						}
					}
					for index, want := range []defs.TokenUsage{
						{
							Input:  100,
							Output: 10,
						},
						{
							Input:  7,
							Output: 3,
						},
						{
							Input:  5,
							Output: 2,
						},
						{
							Input:  200,
							Output: 20,
						},
					} {
						if calls[index].ModelCall.Usage == nil || *calls[index].ModelCall.Usage != want {
							t.Fatalf("call %d usage = %+v, want %+v", index, calls[index].ModelCall.Usage, want)
						}
						if (calls[index].ModelCall.ContextUsage != nil) != (index != 1) {
							t.Fatalf("call %d context usage = %+v, want only the failed retry without one", index, calls[index].ModelCall.ContextUsage)
						}
					}
					assertAgentEvents(t, slices.DeleteFunc(slices.Clone(received), func(event *agent.AgentEvent) bool { return event.Type == agent.AgentEventToolProgress }), result.Items)
				})
			})
		}
	}
}

// measuredRequestContext 保存 Agent 交来的完整报告，并可让更新报告后的第一次测量失败。
type measuredRequestContext struct {
	agent_context.AgentContext
	reports                []*defs.TurnContextUsage
	failRequestMeasurement bool
	requestMeasurementErr  error
}

// RecordRequestUsage 保存收到的报告并更新真实上下文。
func (c *measuredRequestContext) RecordRequestUsage(contextUsage *defs.TurnContextUsage) {
	// 1. 记录收到的报告，交给真实上下文，并安排本次测量错误。
	c.reports = append(c.reports, contextUsage)
	c.AgentContext.RecordRequestUsage(contextUsage)
	if c.failRequestMeasurement {
		c.requestMeasurementErr = io.ErrUnexpectedEOF
	}
}

// GetMeasurement 在指定的原输入测量报错，其余仍使用真实上下文。
func (c *measuredRequestContext) GetMeasurement(tools []llmkit.Tool) (*agent_context.ContextMeasurement, error) {
	// 1. 消费一次测量错误，否则返回真实测量。
	if c.requestMeasurementErr != nil {
		err := c.requestMeasurementErr
		c.requestMeasurementErr = nil
		return nil, err
	}
	return c.AgentContext.GetMeasurement(tools)
}

// TestAgentModelCallReports 检查完整响应、缺失用量、传输失败和测量失败的调用报告。
func TestAgentModelCallReports(t *testing.T) {
	// 1. 准备带用量、缺失用量、length 结束、测量报错和请求失败的样例。
	for _, tt := range []struct {
		name            string
		hasUsage        bool
		finishReason    string
		failMeasurement bool
		callError       error
	}{
		{name: "completed_usage", hasUsage: true, finishReason: "stop"},
		{name: "completed_without_usage", finishReason: "stop"},
		{name: "length_is_completed", hasUsage: true, finishReason: "length"},
		{name: "measurement_failure_keeps_usage_and_success", hasUsage: true, finishReason: "stop", failMeasurement: true},
		{name: "measurement_failure_without_usage", finishReason: "stop", failMeasurement: true},
		{name: "transport_failure_has_identity", callError: errors.New("transport failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := userMessage("start")
			input.ID = defs.NewItemID()
			response := []llmkit.Event{
				{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "answer"}},
				{Type: llmkit.EventDone, FinishReason: tt.finishReason, ReasoningContext: llmkit.ReasoningContextCurrentTurn},
			}
			if tt.hasUsage {
				response[1].Usage = &llmkit.Usage{PromptTokens: 9000, CompletionTokens: 5000, ReasoningTokens: 4000}
			}
			client := &sequenceAgentClient{responses: [][]llmkit.Event{response}, callErrors: []error{tt.callError}}
			history := &measuredRequestContext{AgentContext: newTestContext(t, "", []*defs.Item{input}), failRequestMeasurement: tt.failMeasurement}
			instance, err := agent.NewAgent(agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, history))
			if err != nil {
				t.Fatal(err)
			}
			// 2. 执行一次请求，测量失败不能重试模型或丢弃调用事实。
			events := make(chan *agent.AgentEvent, 32)
			result, err := instance.Run(t.Context(), events)
			if !errors.Is(err, tt.callError) {
				t.Fatalf("Run() error = %v, want %v", err, tt.callError)
			}
			var received []*agent.AgentEvent
			for event := range events {
				received = append(received, event)
			}
			calls := getAgentEventsOfType(received, agent.AgentEventModelCallFinished)
			if len(client.requests) != 1 || result.Stats.Steps != 1 || len(calls) != 1 {
				t.Fatalf("requests=%d, steps=%d, calls=%d; want one attempt", len(client.requests), result.Stats.Steps, len(calls))
			}
			call := calls[0].ModelCall
			if call.ModelCallID == "" || call.Purpose != agent.ModelCallPurposeConversation || (call.ContextUsage != nil) != (tt.callError == nil) {
				t.Fatalf("call = %+v", call)
			}
			if tt.callError != nil {
				if len(history.reports) != 0 || call.Usage != nil || result.FinalModelCallID != "" {
					t.Fatalf("failed call established a report: call=%+v, reports=%+v", call, history.reports)
				}
				return
			}
			// 3. 检查完整响应先记录原输入，再发布调用事件，并把输出加入上下文。
			if len(history.reports) != 1 || result.FinalModelCallID != call.ModelCallID {
				t.Fatalf("call=%+v, reports=%+v, result=%+v", call, history.reports, result)
			}
			wantReport := &defs.TurnContextUsage{ModelCallID: call.ModelCallID, InputThroughItemID: input.ID, ReasoningContext: llmkit.ReasoningContextCurrentTurn}
			if tt.hasUsage {
				wantReport.Usage = defs.TokenUsage{Input: 9000, Output: 5000, Reasoning: 4000}
			}
			if history.reports[0] != call.ContextUsage || !reflect.DeepEqual(call.ContextUsage, wantReport) || (call.Usage != nil) != tt.hasUsage {
				t.Fatalf("report=%+v, event report=%+v, want one shared report %+v; usage=%+v", history.reports[0], call.ContextUsage, wantReport, call.Usage)
			}
			if tt.hasUsage && (result.Stats.Usage != (defs.TokenUsage{Input: 9000, Output: 5000, Reasoning: 4000}) || *call.Usage != result.Stats.Usage) {
				t.Fatalf("call usage=%+v, stats=%+v", call.Usage, result.Stats.Usage)
			}
			callIndex := slices.Index(received, calls[0])
			if !tt.failMeasurement {
				beforeCall := received[callIndex-1]
				if beforeCall.Type != agent.AgentEventContextMeasured || len(beforeCall.Measurement.Items) != 1 || beforeCall.Measurement.Items[0].ItemID != input.ID {
					t.Fatalf("event before call = %+v, want original input measurement", beforeCall)
				}
			}
			if afterCall := received[callIndex+1]; afterCall.Type != agent.AgentEventContextMeasured || len(afterCall.Measurement.Items) != 2 || len(result.Items) != 1 {
				t.Fatalf("event after call=%+v, output=%+v; want appended output measurement", afterCall, result.Items)
			}
		})
	}
}

// TestAgentUsesReportedReasoningContext 检查实际模式先用于原输入测量，缺失用量和 unknown 仍替换旧模式。
func TestAgentUsesReportedReasoningContext(t *testing.T) {
	// 1. 准备新模式及继承模式；历史思考始终发送给模型。
	for _, tt := range []struct {
		name          string
		previousMode  llmkit.ReasoningContext
		mode          llmkit.ReasoningContext
		hasUsage      bool
		wantReasoning bool
	}{
		{name: "current_turn_with_usage", mode: llmkit.ReasoningContextCurrentTurn, hasUsage: true},
		{name: "current_turn_without_usage", mode: llmkit.ReasoningContextCurrentTurn},
		{name: "all_turns_replaces_current_turn_without_usage", previousMode: llmkit.ReasoningContextCurrentTurn, mode: llmkit.ReasoningContextAllTurns, wantReasoning: true},
		{name: "unknown_replaces_current_turn_without_usage", previousMode: llmkit.ReasoningContextCurrentTurn, mode: llmkit.ReasoningContextUnknown, wantReasoning: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first := userMessage("first")
			first.ID = defs.NewItemID()
			reasoning := &defs.Item{ID: defs.NewItemID(), TurnID: testTurnID, ModelCallID: defs.NewModelCallID(), Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{Status: defs.ReasoningCompleted, Content: []string{strings.Repeat("historical reasoning ", 200)}}}
			last := userMessage("next")
			last.ID = defs.NewItemID()
			model := newTestModel(&llmkit.Target{Protocol: llmkit.ProtocolOpenAIChat, Model: "reasoning-context"}, 0)
			response := []llmkit.Event{
				{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "answer"}},
				{Type: llmkit.EventDone, FinishReason: "stop", ReasoningContext: tt.mode},
			}
			if tt.hasUsage {
				response[1].Usage = &llmkit.Usage{PromptTokens: 9000, CompletionTokens: 100}
			}
			client := &sequenceAgentClient{responses: [][]llmkit.Event{response}}
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, model, model))
			if err != nil {
				t.Fatal(err)
			}
			if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{first, reasoning, last}, ContextUsage: &defs.TurnContextUsage{ModelCallID: reasoning.ModelCallID, InputThroughItemID: first.ID, ReasoningContext: tt.previousMode}}); err != nil {
				t.Fatal(err)
			}
			instance, err := agent.NewAgent(agent.NewAgentOpt(testTurnID, testSessionID, client, model, history))
			if err != nil {
				t.Fatal(err)
			}
			// 2. 比较调用完成前的原输入测量，确认新模式不被 Usage 缺报拦下。
			events := make(chan *agent.AgentEvent, 32)
			if _, err := instance.Run(t.Context(), events); err != nil {
				t.Fatal(err)
			}
			var received []*agent.AgentEvent
			for event := range events {
				received = append(received, event)
			}
			calls := getAgentEventsOfType(received, agent.AgentEventModelCallFinished)
			if len(calls) != 1 || calls[0].ModelCall.ContextUsage == nil || calls[0].ModelCall.ContextUsage.ReasoningContext != tt.mode {
				t.Fatalf("model calls = %+v", calls)
			}
			callIndex := slices.Index(received, calls[0])
			beforeCall := received[callIndex-1]
			if beforeCall.Type != agent.AgentEventContextMeasured {
				t.Fatalf("event before call = %s, want original input measurement", beforeCall.Type)
			}
			initial := getAgentEventsOfType(received, agent.AgentEventContextMeasured)[0].Measurement
			if (beforeCall.Measurement.EstimatedTokens > initial.EstimatedTokens) != tt.wantReasoning || beforeCall.Measurement.EstimatedTokens == initial.EstimatedTokens || beforeCall.Measurement.IsUsageBased != tt.hasUsage {
				t.Fatalf("measurement before/after mode = %+v / %+v, want reasoning growth=%v, usage=%v", initial, beforeCall.Measurement, tt.wantReasoning, tt.hasUsage)
			}
			if tt.hasUsage && beforeCall.Measurement.RequestTokens != 9000 {
				t.Fatalf("request tokens = %d, want unchanged actual input 9000", beforeCall.Measurement.RequestTokens)
			}
			// 3. 检查模型请求仍携带历史思考，不随计数模式改变。
			payload, err := json.Marshal(client.requests[0].Messages)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(payload), "historical reasoning") {
				t.Fatalf("request omitted historical reasoning: %s", payload)
			}
		})
	}
}

// TestAgentRetriesContextCompaction 检查摘要重试、调用用量和最终摘要保存。
func TestAgentRetriesContextCompaction(t *testing.T) {
	// 1. 准备摘要的部分文本和用量，以及重试后恢复、重试耗尽和结果检查失败不重试的样例。
	failedUsage := llmkit.Event{
		Type: llmkit.EventUsage,
		Usage: &llmkit.Usage{
			PromptTokens:     7,
			CompletionTokens: 3,
		},
	}
	failedText := llmkit.Event{
		Type:  llmkit.EventContentDelta,
		Delta: &llmkit.DeltaPayload{Text: "failed-summary"},
	}
	for _, tt := range []struct {
		name         string
		manual       bool
		failure      []llmkit.Event
		exhaust      bool
		wantError    string
		wantAttempts int
	}{
		{
			name:         "automatic_eof_recovers",
			failure:      []llmkit.Event{failedText, failedUsage},
			wantAttempts: 2,
		},
		{
			name:   "manual_stream_error_recovers",
			manual: true,
			failure: []llmkit.Event{
				failedText,
				failedUsage,
				{
					Type: llmkit.EventError,
					Error: &llmkit.ErrorPayload{
						Code:    "overloaded",
						Message: "busy",
					},
				},
			},
			wantAttempts: 2,
		},
		{
			name:         "automatic_exhaustion_keeps_history",
			failure:      []llmkit.Event{failedText, failedUsage},
			exhaust:      true,
			wantError:    "unexpected EOF",
			wantAttempts: 11,
		},
		{
			name:         "manual_exhaustion_keeps_history",
			manual:       true,
			failure:      []llmkit.Event{failedText, failedUsage, {Type: llmkit.EventError}},
			exhaust:      true,
			wantError:    "compaction model error",
			wantAttempts: 11,
		},
		{
			name: "tool_result_check_does_not_retry",
			failure: []llmkit.Event{
				failedUsage,
				{
					Type: llmkit.EventToolCallEnd,
					ToolCall: &llmkit.StreamingToolCall{
						CallID:    "bad",
						Name:      "lookup",
						Arguments: `{}`,
					},
				},
			},
			wantError:    "compaction model attempted a tool call",
			wantAttempts: 1,
		},
		{
			name: "finish_reason_result_check_does_not_retry",
			failure: []llmkit.Event{failedText, failedUsage, {
				Type:         llmkit.EventDone,
				FinishReason: "length",
			}},
			wantError:    `compaction ended with finish reason "length"`,
			wantAttempts: 1,
		},
		{
			name:   "empty_summary_result_check_does_not_retry",
			manual: true,
			failure: []llmkit.Event{failedUsage, {
				Type:         llmkit.EventDone,
				FinishReason: "stop",
			}},
			wantError:    "compaction returned an empty summary",
			wantAttempts: 1,
		},
	} {
		// 2. 执行摘要重试，检查失败部分不入历史、成功摘要只保存一次，累计所有调用用量。
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pipeline := telemetrytest.NewPipeline(t)
				summarySuccess := []llmkit.Event{
					{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "successful-summary"},
					},
					{
						Type: llmkit.EventUsage,
						Usage: &llmkit.Usage{
							PromptTokens:     50,
							CompletionTokens: 5,
						},
					},
					{
						Type:         llmkit.EventDone,
						FinishReason: "stop",
					},
				}
				client := &sequenceAgentClient{responses: [][]llmkit.Event{tt.failure, summarySuccess, newToolLoopResponses()[1]}}
				if tt.exhaust {
					client.responses = slices.Repeat([][]llmkit.Event{tt.failure}, 11)
				}
				model := newTestModel(&llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					Model:    "compact-retry-test",
				}, 1000)
				history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(testSessionID, testTurnID, client, model, model))
				if err != nil {
					t.Fatal(err)
				}
				old := userMessage(strings.Repeat("old-history ", 400))
				old.ID = "old"
				recent := userMessage(strings.Repeat("recent ", 220))
				recent.ID = "recent"
				if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{old, recent}}); err != nil {
					t.Fatal(err)
				}
				original := contextMessages(t, history)
				opt := agent.NewAgentOpt(testTurnID, testSessionID, client, model, history)
				opt.IsCompactOnly = tt.manual
				instance, err := agent.NewAgent(opt)
				if err != nil {
					t.Fatal(err)
				}
				events := make(chan *agent.AgentEvent)
				collected := make(chan []*agent.AgentEvent, 1)
				go func() {
					var received []*agent.AgentEvent
					for event := range events {
						received = append(received, event)
					}
					collected <- received
				}()
				started := time.Now()
				result, err := instance.Run(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), events)
				received := <-collected
				if tt.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantError) || !strings.Contains(err.Error(), "compact agent context") {
						t.Fatalf("Run() error = %v, want %s", err, tt.wantError)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				wantWait := time.Second
				if tt.exhaust {
					wantWait = 181 * time.Second
				} else if tt.wantError != "" {
					wantWait = 0
				}
				if time.Since(started) != wantWait {
					t.Fatalf("wait = %s, want %s", time.Since(started), wantWait)
				}
				wantNormalCalls := 0
				if !tt.manual && tt.wantError == "" {
					wantNormalCalls = 1
				}
				if len(client.requests) != tt.wantAttempts+wantNormalCalls {
					t.Fatalf("model calls = %d, want %d", len(client.requests), tt.wantAttempts+wantNormalCalls)
				}
				for index := 0; index < tt.wantAttempts; index++ {
					if client.requests[index].ToolChoice == nil || client.requests[index].ToolChoice.Type != "none" || !reflect.DeepEqual(client.requests[index].Messages, client.requests[0].Messages) {
						t.Fatalf("compaction request %d changed", index)
					}
				}
				wantUsage := defs.TokenUsage{
					Input:  7 * tt.wantAttempts,
					Output: 3 * tt.wantAttempts,
				}
				if tt.wantError == "" {
					wantUsage = defs.TokenUsage{
						Input:  57 + 200*wantNormalCalls,
						Output: 8 + 20*wantNormalCalls,
					}
				}
				if result.Stats.Steps != wantNormalCalls || result.Stats.InferenceCalls != len(client.requests) || result.Stats.Usage != wantUsage {
					t.Fatalf("stats = %+v, want steps=%d, inference=%d, usage=%+v", result.Stats, wantNormalCalls, len(client.requests), wantUsage)
				}
				calls := getAgentEventsOfType(received, agent.AgentEventModelCallFinished)
				if len(calls) != len(client.requests) {
					t.Fatalf("model call events = %d", len(calls))
				}
				identities := make(map[defs.ModelCallID]bool)
				for index, event := range calls {
					call := event.ModelCall
					if call.ModelCallID == "" || identities[call.ModelCallID] || call.Usage == nil {
						t.Fatalf("model call = %+v", call)
					}
					identities[call.ModelCallID] = true
					wantPurpose := agent.ModelCallPurposeCompaction
					wantCallUsage := defs.TokenUsage{
						Input:  7,
						Output: 3,
					}
					if index == tt.wantAttempts-1 && tt.wantError == "" {
						wantCallUsage = defs.TokenUsage{
							Input:  50,
							Output: 5,
						}
					}
					if index >= tt.wantAttempts {
						wantPurpose = agent.ModelCallPurposeConversation
						wantCallUsage = defs.TokenUsage{
							Input:  200,
							Output: 20,
						}
					}
					if call.Purpose != wantPurpose || *call.Usage != wantCallUsage {
						t.Fatalf("call %d = %+v, want %s %+v", index, call, wantPurpose, wantCallUsage)
					}
				}
				startedSummaries, completedSummaries, interruptedSummaries := 0, 0, 0
				for _, event := range received {
					if event.Type == agent.AgentEventItemInterrupted {
						interruptedSummaries++
						if event.Item != nil {
							t.Fatalf("failed summary was saved: %+v", event.Item)
						}
						continue
					}
					if event.Item == nil || event.Item.Kind != defs.ItemContextSummary {
						continue
					}
					switch event.Type {
					case agent.AgentEventItemStarted:
						startedSummaries++
					case agent.AgentEventItemCompleted:
						completedSummaries++
						if event.Item.ContextSummary.Text != "successful-summary" || event.Item.ModelCallID != calls[tt.wantAttempts-1].ModelCall.ModelCallID {
							t.Fatalf("summary = %+v", event.Item)
						}
					}
				}
				if startedSummaries != 1 {
					t.Fatalf("summary starts = %d", startedSummaries)
				}
				if tt.wantError != "" {
					if completedSummaries != 0 || interruptedSummaries != 1 || len(result.Items) != 0 || !reflect.DeepEqual(contextMessages(t, history), original) {
						t.Fatalf("failed compaction changed history or saved a summary: result=%+v, completed=%d interrupted=%d", result, completedSummaries, interruptedSummaries)
					}
				} else {
					if completedSummaries != 1 || interruptedSummaries != 0 || len(result.Items) != 1+wantNormalCalls {
						t.Fatalf("summary counts: completed=%d interrupted=%d, result=%+v", completedSummaries, interruptedSummaries, result)
					}
					payload, err := json.Marshal(contextMessages(t, history))
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(payload), "failed-summary") || strings.Contains(string(payload), "old-history") || !strings.Contains(string(payload), "successful-summary") {
						t.Fatalf("compacted context = %s", payload)
					}
					if wantNormalCalls > 0 {
						payload, err := json.Marshal(client.requests[tt.wantAttempts].Messages)
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(string(payload), "failed-summary") || !strings.Contains(string(payload), "successful-summary") {
							t.Fatalf("normal request = %s", payload)
						}
					}
				}
				spans := pipeline.WaitEndedSpans(t, "chat compact-retry-test", len(client.requests))
				for index, span := range spans {
					if telemetrytest.GetSpanAttribute(span, "openvaala.model_call.id") != string(calls[index].ModelCall.ModelCallID) {
						t.Fatalf("span %d identity differs from event", index)
					}
					wantStatus := codes.Error
					if tt.wantError == "" && index >= tt.wantAttempts-1 {
						wantStatus = codes.Unset
					}
					if span.Status().Code != wantStatus {
						t.Fatalf("span %d status = %v, want %v", index, span.Status(), wantStatus)
					}
				}
			})
		})
	}
}

// TestAgentMaxSteps 检查单轮模型调用上限：最后一步禁用工具并要求总结，仍请求工具时补错误结果，上限为 0 时不受限。
func TestAgentMaxSteps(t *testing.T) {
	// 1. 准备工具调用、文本回复和拦下结束的 hook 结果。
	toolResponse := func(callID string) []llmkit.Event {
		return []llmkit.Event{
			{
				Type: llmkit.EventToolCallEnd,
				ToolCall: &llmkit.StreamingToolCall{
					CallID:    callID,
					Name:      "lookup",
					Arguments: `{"q":"x"}`,
				},
			},
			{
				Type:         llmkit.EventDone,
				FinishReason: "tool_calls",
			},
		}
	}
	textResponse := []llmkit.Event{
		{
			Type:  llmkit.EventContentDelta,
			Delta: &llmkit.DeltaPayload{Text: "进度总结"},
		},
		{
			Type:         llmkit.EventDone,
			FinishReason: "stop",
		},
	}
	blocked := &hook.HookOutcome{
		IsBlocked:   true,
		BlockReason: "run the tests",
		SavedRuns:   []*defs.HookRun{newStopHookRun("stop-blocked", defs.HookRunBlocked)},
	}
	const wantErr = "agent reached max steps but model still requested tools"
	tests := []struct {
		name      string
		maxSteps  int
		responses [][]llmkit.Event
		// outcomes 非空时挂上 Stop hook，按次序返回。
		outcomes []*hook.HookOutcome
		// steer 是否有补充输入排在队列里？
		steer        bool
		wantErr      string
		wantExecuted []string
		wantCalls    int
		wantHookRuns int
	}{
		{
			name:         "last_step_disables_tools_and_asks_for_summary",
			maxSteps:     3,
			responses:    [][]llmkit.Event{toolResponse("call-1"), toolResponse("call-2"), textResponse},
			wantExecuted: []string{"call-1", "call-2"},
			wantCalls:    3,
		},
		{
			name:         "model_ignores_tool_choice",
			maxSteps:     3,
			responses:    [][]llmkit.Event{toolResponse("call-1"), toolResponse("call-2"), toolResponse("call-3")},
			wantErr:      wantErr,
			wantExecuted: []string{"call-1", "call-2"},
			wantCalls:    3,
		},
		{
			name:         "stop_hook_does_not_continue_at_last_step",
			maxSteps:     1,
			responses:    [][]llmkit.Event{textResponse, textResponse},
			outcomes:     []*hook.HookOutcome{blocked, blocked},
			wantCalls:    1,
			wantHookRuns: 1,
		},
		{
			name:      "queued_input_runs_in_next_turn",
			maxSteps:  1,
			responses: [][]llmkit.Event{textResponse, textResponse},
			steer:     true,
			wantCalls: 1,
		},
		{
			name:     "zero_means_unlimited",
			maxSteps: 0,
			responses: [][]llmkit.Event{
				toolResponse("call-1"), toolResponse("call-2"), toolResponse("call-3"), toolResponse("call-4"), toolResponse("call-5"), textResponse,
			},
			wantExecuted: []string{"call-1", "call-2", "call-3", "call-4", "call-5"},
			wantCalls:    6,
		},
	}
	// 2. 运行 Agent，检查请求、工具执行、结果配对、上下文和 hook 次数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &sequenceAgentClient{responses: tt.responses}
			agentContext := newTestContext(t, "", []*defs.Item{userMessage("你好")})
			toolExecutor := &testToolExecutor{}
			hooks := &stubHooks{outcomes: tt.outcomes}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, agentContext)
			opt.ToolExecutor = toolExecutor
			opt.MaxSteps = tt.maxSteps
			if len(tt.outcomes) > 0 {
				opt.Hooks = hooks
				opt.Environments = []executor.ExecutionEnvironment{newTestEnvironment(t, "local", nil)}
			}
			if tt.steer {
				serveAgentInputs(t, opt, agent.AgentInputResult{}, agent.AgentInputResult{Items: []*defs.Item{userMessage("补充说明")}})
			}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := instance.Run(ctx, nil)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("Run() error = %v, want %q", err, tt.wantErr)
			}
			// 3. 只有最后一步的请求禁用工具并以提示结尾，前面的请求不受影响。
			if len(client.requests) != tt.wantCalls {
				t.Fatalf("model calls = %d, want %d", len(client.requests), tt.wantCalls)
			}
			for index, request := range client.requests {
				isLast := tt.maxSteps > 0 && index+1 == tt.maxSteps
				if !isLast {
					if request.ToolChoice != nil || strings.Contains(fmt.Sprint(request.Messages), prompt.MaxStepsReachedInstructions) {
						t.Fatalf("request %d = %+v, want no tool choice and no max steps instructions", index, request)
					}
					continue
				}
				lastMessage := request.Messages[len(request.Messages)-1]
				if request.ToolChoice == nil || request.ToolChoice.Type != "none" || len(request.Tools) == 0 ||
					lastMessage.Role != llmkit.RoleUser || len(lastMessage.Content) != 1 || lastMessage.Content[0].Text != prompt.MaxStepsReachedInstructions {
					t.Fatalf("last request = tool choice %+v, tools %d, last message %+v", request.ToolChoice, len(request.Tools), lastMessage)
				}
			}
			// 4. 提示和排队的输入都不在上下文里。
			if payload := fmt.Sprint(contextMessages(t, agentContext)); strings.Contains(payload, prompt.MaxStepsReachedInstructions) || strings.Contains(payload, "补充说明") {
				t.Fatalf("context = %s, want no max steps instructions or queued input", payload)
			}
			// 5. 只有上限前的工具调用被执行；每个工具调用都有配对的结果，最后一步的结果是固定错误。
			if !slices.Equal(toolExecutor.calls, tt.wantExecuted) {
				t.Fatalf("executed calls = %v, want %v", toolExecutor.calls, tt.wantExecuted)
			}
			results := make(map[string]*defs.ToolResult)
			var callIDs []string
			for _, item := range result.Items {
				switch item.Kind {
				case defs.ItemToolCall:
					callIDs = append(callIDs, item.ToolCall.CallID)
				case defs.ItemToolResult:
					results[item.ToolResult.CallID] = item.ToolResult
				}
			}
			for _, callID := range callIDs {
				if results[callID] == nil {
					t.Fatalf("tool call %q has no result in %+v", callID, result.Items)
				}
			}
			if tt.wantErr != "" {
				if rejected := results["call-3"]; rejected.Error == nil || rejected.Error.Message != tt.wantErr {
					t.Fatalf("rejected result = %+v, want error %q", rejected, tt.wantErr)
				}
			}
			if len(hooks.requests) != tt.wantHookRuns {
				t.Fatalf("stop hook requests = %d, want %d", len(hooks.requests), tt.wantHookRuns)
			}
		})
	}
}
