package telemetry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// TestContentCapture 验证遥测内容采集和截断。
func TestContentCapture(t *testing.T) {
	// 1. 准备输入输出、工具参数结果、采集开关和超限内容样例。
	contentKeys := []string{
		"gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.tool.definitions",
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "openvaala.content.truncated",
	}
	tests := []struct {
		name           string
		captureContent bool
		run            func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline)
	}{
		{
			name:           "chat_input_and_output_are_json_text",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				if !recorder.IsContentCaptured() {
					t.Fatal("IsContentCaptured() = false, want true")
				}
				recorder.RecordInput([]string{"你是助手"}, []telemetry.ChatMessage{
					{
						Role: telemetry.MessageRoleUser,
						Parts: []telemetry.MessagePart{{
							Type:    telemetry.PartTypeText,
							Content: "列出文件",
						}},
					},
					{
						Role: telemetry.MessageRoleAssistant,
						Parts: []telemetry.MessagePart{
							{
								Type:    telemetry.PartTypeReasoning,
								Content: "需要调用 bash",
							},
							{
								Type:      telemetry.PartTypeToolCall,
								ID:        "call-1",
								Name:      "bash",
								Arguments: telemetry.DecodeJSONContent([]byte(`{"command":"ls"}`)),
							},
						},
					},
					{
						Role: telemetry.MessageRoleTool,
						Parts: []telemetry.MessagePart{
							{
								Type:     telemetry.PartTypeToolCallResponse,
								ID:       "call-1",
								Response: telemetry.DecodeJSONContent([]byte(`{"ok":true}`)),
							},
						},
					},
				}, []string{"bash", "read_file"})
				recorder.RecordOutput([]telemetry.ChatMessage{
					{
						Role: telemetry.MessageRoleAssistant,
						Parts: []telemetry.MessagePart{{
							Type:    telemetry.PartTypeText,
							Content: "答复",
						}},
					},
				})
				recorder.End(nil)
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				system := getContentArray(t, span, "gen_ai.system_instructions")
				if len(system) != 1 || getJSONPath(system[0], "content") != "你是助手" || getJSONPath(system[0], "type") != "text" {
					t.Errorf("system instructions = %v", system)
				}
				input := getContentArray(t, span, "gen_ai.input.messages")
				if len(input) != 3 || getJSONPath(input[2], "role") != "tool" {
					t.Fatalf("input messages = %v", input)
				}
				toolCall := getJSONPath(input[1], "parts", 1)
				if getJSONPath(toolCall, "type") != "tool_call" || getJSONPath(toolCall, "name") != "bash" ||
					getJSONPath(toolCall, "arguments", "command") != "ls" {
					t.Errorf("tool call part = %v", toolCall)
				}
				if response := getJSONPath(input[2], "parts", 0, "response"); getJSONPath(response, "ok") != true {
					t.Errorf("tool call response = %v", response)
				}
				output := getContentArray(t, span, "gen_ai.output.messages")
				if len(output) != 1 || getJSONPath(output[0], "parts", 0, "content") != "答复" {
					t.Errorf("output messages = %v", output)
				}
				if getJSONPath(output[0], "finish_reason") != nil {
					t.Error("output message carries the deprecated finish_reason")
				}
				if got, want := getContentText(t, span, "gen_ai.tool.definitions"),
					`[{"name":"bash","type":"function"},{"name":"read_file","type":"function"}]`; got != want {
					t.Errorf("tool definitions = %s, want %s", got, want)
				}
				if _, found := telemetrytest.GetSpanValue(span, "openvaala.content.truncated"); found {
					t.Error("small content marked as truncated")
				}
				if strings.Contains(p.GetLogText(), "列出文件") || strings.Contains(p.GetLogText(), "答复") {
					t.Errorf("logs contain content: %s", p.GetLogText())
				}
			},
		},
		{
			name: "content_disabled_writes_nothing",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				message := []telemetry.ChatMessage{{
					Role: telemetry.MessageRoleUser,
					Parts: []telemetry.MessagePart{{
						Type:    telemetry.PartTypeText,
						Content: "secret",
					}},
				}}
				turnCtx, turnRecorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				turnRecorder.RecordInput(message)
				chatCtx, chatRecorder := telemetry.StartChat(turnCtx, newChatStart(telemetry.ChatPurposeConversation))
				chatRecorder.RecordInput([]string{"system"}, message, []string{"bash"})
				chatRecorder.RecordOutput(message)
				_, toolRecorder := telemetry.StartExecuteTool(chatCtx, telemetry.ExecuteToolStart{
					Name:   "bash",
					CallID: "call-1",
				})
				toolRecorder.RecordArguments(map[string]any{"command": "secret"})
				toolRecorder.RecordResult("secret")
				// 1. 在 End 之前检查三个记录器的采集开关。
				if turnRecorder.IsContentCaptured() || chatRecorder.IsContentCaptured() || toolRecorder.IsContentCaptured() {
					t.Fatal("IsContentCaptured() = true with capture disabled")
				}
				toolRecorder.End("")
				chatRecorder.End(nil)
				turnRecorder.End(telemetry.InvokeAgentEnd{
					Status:  "completed",
					Summary: telemetry.TurnSummary{Output: message},
				})
				for _, span := range p.GetEndedSpans() {
					for _, key := range contentKeys {
						if _, found := telemetrytest.GetSpanValue(span, key); found {
							t.Errorf("span %s has %s with capture disabled", span.Name(), key)
						}
					}
				}
			},
		},
		{
			name:           "invoke_agent_records_turn_input_and_final_answer",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				recorder.RecordInput([]telemetry.ChatMessage{{
					Role: telemetry.MessageRoleUser,
					Parts: []telemetry.MessagePart{{
						Type:    telemetry.PartTypeText,
						Content: "问题",
					}},
				}})
				recorder.End(telemetry.InvokeAgentEnd{
					Status: "completed",
					Summary: telemetry.TurnSummary{
						Output: []telemetry.ChatMessage{{
							Role: telemetry.MessageRoleAssistant,
							Parts: []telemetry.MessagePart{{
								Type:    telemetry.PartTypeText,
								Content: "回答",
							}},
						}},
					},
				})
				span := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
				input := getContentArray(t, span, "gen_ai.input.messages")
				output := getContentArray(t, span, "gen_ai.output.messages")
				if len(input) != 1 || len(output) != 1 || getJSONPath(input[0], "parts", 0, "content") != "问题" ||
					getJSONPath(output[0], "parts", 0, "content") != "回答" {
					t.Fatalf("input = %v, output = %v", input, output)
				}
			},
		},
		{
			name:           "tool_arguments_and_result_are_json_text",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:   "read_file",
					CallID: "call-1",
				})
				recorder.RecordArguments(telemetry.DecodeJSONContent([]byte(`{"path":"a.txt","limit":10}`)))
				recorder.RecordResult(telemetry.DecodeJSONContent([]byte(`not json`)))
				recorder.End("")
				span := p.WaitEndedSpans(t, "execute_tool read_file", 1)[0]
				arguments := getContentText(t, span, "gen_ai.tool.call.arguments")
				if arguments != `{"limit":10,"path":"a.txt"}` {
					t.Errorf("arguments = %s, want compact JSON text", arguments)
				}
				var decoded map[string]any
				if err := json.Unmarshal([]byte(arguments), &decoded); err != nil || decoded["path"] != "a.txt" || decoded["limit"] != float64(10) {
					t.Errorf("arguments %q do not round-trip: %v, %v", arguments, decoded, err)
				}
				if result := getContentText(t, span, "gen_ai.tool.call.result"); result != "not json" {
					t.Errorf("result = %q, want the raw text", result)
				}
			},
		},
		{
			name:           "tool_result_object_is_json_text_without_html_escaping",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:   "bash",
					CallID: "call-1",
				})
				recorder.RecordResult(telemetry.DecodeJSONContent([]byte(`{"output":"<a&b>","exit_code":0}`)))
				recorder.End("")
				span := p.WaitEndedSpans(t, "execute_tool bash", 1)[0]
				if result := getContentText(t, span, "gen_ai.tool.call.result"); result != `{"exit_code":0,"output":"<a&b>"}` {
					t.Errorf("result = %s", result)
				}
			},
		},
		{
			name:           "deeply_nested_tool_arguments_stay_nested_json",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				nested := `"leaf"`
				for range 20 {
					nested = `{"next":` + nested + `}`
				}
				_, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:   "read_file",
					CallID: "call-1",
				})
				recorder.RecordArguments(telemetry.DecodeJSONContent([]byte(nested)))
				recorder.End("")
				span := p.WaitEndedSpans(t, "execute_tool read_file", 1)[0]
				// 1. 比较输入与输出 JSON 原文，核对深层对象没有被转成字符串。
				if arguments := getContentText(t, span, "gen_ai.tool.call.arguments"); arguments != nested {
					t.Errorf("arguments = %s, want the input nested JSON %s", arguments, nested)
				}
			},
		},
		{
			name:           "oversize_history_keeps_latest_messages",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				var messages []telemetry.ChatMessage
				for index := range 5 {
					text := strings.Repeat(string(rune('a'+index)), 20<<10)
					messages = append(messages, telemetry.ChatMessage{
						Role: telemetry.MessageRoleUser,
						Parts: []telemetry.MessagePart{{
							Type:    telemetry.PartTypeText,
							Content: text,
						}},
					})
				}
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				recorder.RecordInput(nil, messages, nil)
				recorder.End(nil)
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				input := getContentText(t, span, "gen_ai.input.messages")
				kept := decodeJSONArray(t, input)
				if len(kept) != 3 || getJSONPath(kept[len(kept)-1], "parts", 0, "content") != messages[4].Parts[0].Content || len(input) > 64<<10 {
					t.Fatalf("kept %d messages, %d bytes", len(kept), len(input))
				}
				if value, _ := telemetrytest.GetSpanValue(span, "openvaala.content.truncated"); !value.AsBool() {
					t.Error("openvaala.content.truncated not set")
				}
			},
		},
		{
			name:           "oversize_single_message_truncates_its_text",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				huge := telemetry.ChatMessage{
					Role: telemetry.MessageRoleUser,
					Parts: []telemetry.MessagePart{{
						Type:    telemetry.PartTypeText,
						Content: strings.Repeat("长", 50<<10),
					}},
				}
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				recorder.RecordInput(nil, []telemetry.ChatMessage{huge}, nil)
				recorder.End(nil)
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				input := getContentText(t, span, "gen_ai.input.messages")
				content, _ := getJSONPath(decodeJSONArray(t, input)[0], "parts", 0, "content").(string)
				if len(input) > 64<<10 || !strings.Contains(content, "…[truncated ") || !strings.HasPrefix(content, "长长") ||
					!utf8.ValidString(input) || !utf8.ValidString(content) {
					t.Fatalf("encoded %d bytes, content suffix %q", len(input), content[max(0, len(content)-40):])
				}
				if value, _ := telemetrytest.GetSpanValue(span, "openvaala.content.truncated"); !value.AsBool() {
					t.Error("openvaala.content.truncated not set")
				}
			},
		},
		{
			name:           "oversize_tool_result_becomes_truncated_text",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:   "bash",
					CallID: "call-1",
				})
				recorder.RecordResult(map[string]any{"output": strings.Repeat("x", 100<<10)})
				recorder.End("")
				span := p.WaitEndedSpans(t, "execute_tool bash", 1)[0]
				result := getContentText(t, span, "gen_ai.tool.call.result")
				if len(result) > 64<<10 || !strings.Contains(result, "…[truncated ") || !utf8.ValidString(result) {
					t.Fatalf("result %d bytes", len(result))
				}
				if value, _ := telemetrytest.GetSpanValue(span, "openvaala.content.truncated"); !value.AsBool() {
					t.Error("openvaala.content.truncated not set")
				}
			},
		},
		{
			name:           "oversize_tool_result_with_html_characters_stays_within_limit",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:   "bash",
					CallID: "call-1",
				})
				recorder.RecordResult(map[string]any{"output": strings.Repeat("<a&b>", 40<<10)})
				recorder.End("")
				span := p.WaitEndedSpans(t, "execute_tool bash", 1)[0]
				text := getContentText(t, span, "gen_ai.tool.call.result")
				if len(text) > 64<<10 || !strings.Contains(text, "…[truncated ") ||
					!strings.Contains(text, "<a&b>") || strings.Contains(text, `\u003c`) {
					t.Fatalf("result %d bytes, prefix %q", len(text), text[:min(len(text), 40)])
				}
			},
		},
		{
			name:           "oversize_structured_response_reports_exact_cut",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				response := map[string]any{"content": strings.Repeat("fmt.Println(\"代码\")\n", 4200)}
				original, _ := json.Marshal(response)
				message := telemetry.ChatMessage{
					Role: telemetry.MessageRoleTool,
					Parts: []telemetry.MessagePart{
						{
							Type:     telemetry.PartTypeToolCallResponse,
							ID:       "call-1",
							Response: response,
						},
					},
				}
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				recorder.RecordInput(nil, []telemetry.ChatMessage{message}, nil)
				recorder.End(nil)
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				input := getContentText(t, span, "gen_ai.input.messages")
				cut, isText := getJSONPath(decodeJSONArray(t, input)[0], "parts", 0, "response").(string)
				if !isText || len(input) > 64<<10 {
					t.Fatalf("response is text = %v, encoded %d bytes", isText, len(input))
				}
				index := strings.LastIndex(cut, "…[truncated ")
				if index < 0 || strings.Count(cut, "…[truncated ") != 1 {
					t.Fatalf("response must carry exactly one truncation marker, tail %q", cut[max(0, len(cut)-40):])
				}
				reported, err := strconv.Atoi(strings.TrimSuffix(cut[index+len("…[truncated "):], " chars]"))
				kept := cut[:index]
				if err != nil || !strings.HasPrefix(string(original), kept) {
					t.Fatalf("marker count parse error %v, kept text is not a prefix of the original", err)
				}
				if removed := utf8.RuneCountInString(string(original)) - utf8.RuneCountInString(kept); reported != removed {
					t.Errorf("marker reports %d chars, actually removed %d", reported, removed)
				}
			},
		},
		{
			name:           "many_large_tool_calls_fit_within_limit",
			captureContent: true,
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				message := telemetry.ChatMessage{Role: telemetry.MessageRoleAssistant}
				for index := range 30 {
					message.Parts = append(message.Parts, telemetry.MessagePart{
						Type:      telemetry.PartTypeToolCall,
						ID:        fmt.Sprintf("call-%d", index),
						Name:      "bash",
						Arguments: map[string]any{"command": strings.Repeat("x", 10<<10)},
					})
				}
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				recorder.RecordInput(nil, []telemetry.ChatMessage{message}, nil)
				recorder.End(nil)
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				input := getContentText(t, span, "gen_ai.input.messages")
				if len(input) > 64<<10 || !utf8.ValidString(input) {
					t.Fatalf("encoded %d bytes, want at most %d", len(input), 64<<10)
				}
				// 1. 解码截断文本，核对它仍是合法 JSON 数组。
				decodeJSONArray(t, input)
				if value, _ := telemetrytest.GetSpanValue(span, "openvaala.content.truncated"); !value.AsBool() {
					t.Error("openvaala.content.truncated not set")
				}
			},
		},
	}
	// 2. 按开关创建内存管道，记录并比较内容、截断标记和文本字节数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p *telemetrytest.Pipeline
			if tt.captureContent {
				p = telemetrytest.NewPipeline(t)
			} else {
				p = telemetrytest.NewPipelineWithoutContent(t)
			}
			tt.run(t, telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), p)
		})
	}
}

// TestContentCaptureRequiresRecordingSpan 验证内容采集取决于 span 记录状态。
func TestContentCaptureRequiresRecordingSpan(t *testing.T) {
	// 1. 准备正常采样、不采样和 noop Provider。
	tests := []struct {
		name           string
		tracerProvider trace.TracerProvider
		want           bool
	}{
		{
			name:           "recording_span",
			tracerProvider: sdktrace.NewTracerProvider(),
			want:           true,
		},
		{
			name:           "unsampled_span",
			tracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample())),
		},
		{
			name:           "noop_provider",
			tracerProvider: tracenoop.NewTracerProvider(),
		},
	}
	// 2. 创建三种记录器，在 End 前核对内容采集判断，再结束记录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opt := telemetry.NewTelemetryOpt(tt.tracerProvider, metricnoop.NewMeterProvider(), slog.New(slog.DiscardHandler)).WithCaptureContent(true)
			instance, err := telemetry.NewTelemetry(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx := telemetry.ContextWithTelemetry(t.Context(), instance)
			_, turnRecorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
			_, chatRecorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
			_, toolRecorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
				Name:   "bash",
				CallID: "call-1",
			})
			for name, got := range map[string]bool{
				"invoke_agent": turnRecorder.IsContentCaptured(),
				"chat":         chatRecorder.IsContentCaptured(),
				"execute_tool": toolRecorder.IsContentCaptured(),
			} {
				if got != tt.want {
					t.Errorf("%s IsContentCaptured() = %v, want %v", name, got, tt.want)
				}
			}
			turnRecorder.End(telemetry.InvokeAgentEnd{Status: "completed"})
			chatRecorder.End(nil)
			toolRecorder.End("")
		})
	}
}

// TestDecodeJSONContent 验证 JSON 内容解析与原文保留。
func TestDecodeJSONContent(t *testing.T) {
	// 1. 准备合法、无效、尾随数据和空输入。
	tests := []struct {
		name string
		raw  string
		want any
	}{
		{
			name: "object",
			raw:  `{"a":1}`,
			want: map[string]any{"a": json.Number("1")},
		},
		{
			name: "string",
			raw:  `"text"`,
			want: "text",
		},
		{
			name: "invalid_json_kept_as_text",
			raw:  `{"a":`,
			want: `{"a":`,
		},
		{
			name: "trailing_data_kept_as_text",
			raw:  `{} {}`,
			want: `{} {}`,
		},
		{
			name: "trailing_closing_brace_kept_as_text",
			raw:  `{"a":1}}`,
			want: `{"a":1}}`,
		},
		{
			name: "trailing_closing_bracket_kept_as_text",
			raw:  `1]`,
			want: `1]`,
		},
		{
			name: "empty_kept_as_empty_text",
			raw:  ``,
			want: ``,
		},
	}
	// 2. 解析并序列化内容，将结果与期望 JSON 值比较。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := json.Marshal(telemetry.DecodeJSONContent([]byte(tt.raw)))
			want, _ := json.Marshal(tt.want)
			if string(got) != string(want) {
				t.Fatalf("DecodeJSONContent(%q) = %s, want %s", tt.raw, got, want)
			}
		})
	}
}

// getContentText 读取 span 上的内容文本。
func getContentText(t *testing.T, span sdktrace.ReadOnlySpan, key string) string {
	t.Helper()
	// 1. 按 key 取值并检查存在且为字符串，否则测试失败。
	value, found := telemetrytest.GetSpanValue(span, key)
	if !found {
		t.Fatalf("span %s lacks %s: %v", span.Name(), key, span.Attributes())
	}
	if value.Type() != attribute.STRING {
		t.Fatalf("%s type = %v, want a JSON text string", key, value.Type())
	}
	return value.AsString()
}

// getContentArray 读取 span 内容并解析为数组。
func getContentArray(t *testing.T, span sdktrace.ReadOnlySpan, key string) []any {
	t.Helper()
	// 1. 获取内容文本并解码 JSON 数组。
	return decodeJSONArray(t, getContentText(t, span, key))
}

// decodeJSONArray 将内容文本解码为 JSON 数组。
func decodeJSONArray(t *testing.T, text string) []any {
	t.Helper()
	// 1. 解析数组，不合法时输出前缀并结束测试。
	var array []any
	if err := json.Unmarshal([]byte(text), &array); err != nil {
		t.Fatalf("content is not a JSON array: %v, prefix %q", err, text[:min(len(text), 80)])
	}
	return array
}

// getJSONPath 沿给定路径读取 JSON 子值。
func getJSONPath(value any, path ...any) any {
	// 1. 字符串路径读取对象字段，整数读取数组元素，缺少项或类型不符时返回 nil。
	for _, step := range path {
		switch step := step.(type) {
		case string:
			object, _ := value.(map[string]any)
			value = object[step]
		case int:
			array, _ := value.([]any)
			if step >= len(array) {
				return nil
			}
			value = array[step]
		}
	}
	return value
}
