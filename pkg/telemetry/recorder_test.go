package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TestRecorders 验证 RPC、Turn、模型和工具记录器。
func TestRecorders(t *testing.T) {
	// 1. 准备成功、失败、取消、用量、分块、指标维度和分桶样例。
	tests := []struct {
		name string
		run  func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline)
	}{
		{
			name: "chat_records_usage_finish_and_stream_facts",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{AgentName: "assistant", SessionID: "session-1"})
				start := newChatStart(telemetry.ChatPurposeConversation)
				start.ServerAddress, start.ServerPort, start.Compacted, start.TokenModality = "llm.example.com", 443, true, telemetry.TokenModalityText
				_, recorder := telemetry.StartChat(ctx, start)
				recorder.RecordChunk()
				recorder.RecordResponseModel("gpt-test-2026")
				recorder.RecordResponseModel("ignored")
				recorder.RecordChunk()
				recorder.RecordChunk()
				recorder.RecordUsage(defs.TokenUsage{
					Input:  10,
					Output: 1,
				})
				recorder.RecordUsage(defs.TokenUsage{
					Input:      120,
					Output:     30,
					CacheRead:  80,
					CacheWrite: 7,
					Reasoning:  12,
				})
				recorder.RecordFinishReason(telemetry.FinishReasonToolCall)
				recorder.End(nil)

				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				if span.SpanKind() != trace.SpanKindClient || span.Status().Code == codes.Error {
					t.Fatalf("kind = %v, status = %v", span.SpanKind(), span.Status())
				}
				for key, want := range map[string]string{
					"gen_ai.operation.name":                 "chat",
					"gen_ai.usage.input_tokens":             "120",
					"gen_ai.usage.output_tokens":            "30",
					"gen_ai.usage.cache_read.input_tokens":  "80",
					"gen_ai.usage.cache_write.input_tokens": "7",
					"gen_ai.usage.reasoning.output_tokens":  "12",
					"openvaala.model_call.id":               "call-1",
					"openvaala.session.id":                  "session-1",
					"gen_ai.provider.name":                  "openai",
					"openvaala.model_call.purpose":          "conversation",
					"gen_ai.request.stream":                 "true",
					"server.address":                        "llm.example.com",
					"server.port":                           "443",
					"gen_ai.conversation.compacted":         "true",
					"gen_ai.response.model":                 "gpt-test-2026",
					"gen_ai.response.finish_reasons":        `["tool_call"]`,
				} {
					if got := telemetrytest.GetSpanAttribute(span, key); got != want {
						t.Errorf("span %s = %q, want %q", key, got, want)
					}
				}
				if telemetrytest.GetSpanAttribute(span, "gen_ai.response.time_to_first_chunk") == "" {
					t.Error("span lacks gen_ai.response.time_to_first_chunk")
				}
				if !strings.Contains(telemetrytest.GetSpanAttribute(span, "code.function.name"), "TestRecorders") {
					t.Errorf("code.function.name = %q", telemetrytest.GetSpanAttribute(span, "code.function.name"))
				}
				agent := attribute.String("gen_ai.agent.name", "assistant")
				text := attribute.String("gen_ai.token.modality", "text")
				for name, want := range map[string]float64{
					"gen_ai.client.inference.usage.input_tokens":             120,
					"gen_ai.client.inference.usage.output_tokens":            30,
					"gen_ai.client.inference.usage.cache_read.input_tokens":  80,
					"gen_ai.client.inference.usage.cache_write.input_tokens": 7,
					"gen_ai.client.inference.usage.reasoning.output_tokens":  12,
				} {
					if got := p.GetMetricSum(t, name, agent, text); got != want {
						t.Errorf("metric %s with text modality = %v, want %v", name, got, want)
					}
				}
				if got := p.GetMetricSum(t, "gen_ai.client.inference.operation.input_tokens", agent); got != 120 {
					t.Errorf("operation input tokens = %v, want 120", got)
				}
				for name, want := range map[string]uint64{
					"gen_ai.client.inference.duration":              1,
					"gen_ai.client.inference.time_to_first_chunk":   1,
					"gen_ai.client.inference.time_per_output_chunk": 2,
				} {
					if got := p.GetHistogramCount(t, name, attribute.String("gen_ai.response.model", "gpt-test-2026")); got != want {
						t.Errorf("metric %s count = %d, want %d", name, got, want)
					}
				}
				record := p.GetLogRecords(t, "model call finished")[0]
				if record["level"] != "INFO" || record["gen_ai.usage.input_tokens"] != float64(120) || record["openvaala.session.id"] != "session-1" ||
					record["gen_ai.usage.cache_write.input_tokens"] != float64(7) || record["gen_ai.response.finish_reasons"] != "tool_call" {
					t.Errorf("log = %v", record)
				}
			},
		},
		{
			name: "chat_without_usage_records_zero_tokens_with_unknown_modality",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeCompact))
				recorder.End(nil)
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				if telemetrytest.GetSpanAttribute(span, "gen_ai.usage.input_tokens") != "" || telemetrytest.GetSpanAttribute(span, "server.address") != "" ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.response.finish_reasons") != "" {
					t.Fatalf("span attributes = %v", span.Attributes())
				}
				if keys := p.GetMetricAttributeKeys(t, "gen_ai.client.inference.usage.input_tokens"); !slices.Contains(keys, "gen_ai.token.modality") {
					t.Fatalf("input token counter keys = %v, want a zero record with gen_ai.token.modality", keys)
				}
				if got := p.GetMetricSum(t, "gen_ai.client.inference.usage.input_tokens", attribute.String("gen_ai.token.modality", "unknown")); got != 0 {
					t.Fatalf("input tokens = %v, want 0", got)
				}
				if keys := p.GetMetricAttributeKeys(t, "gen_ai.client.inference.usage.cache_read.input_tokens"); len(keys) != 0 {
					t.Fatalf("cache read counter has data points with keys %v, want none", keys)
				}
				if got := p.GetHistogramCount(t, "gen_ai.client.inference.time_to_first_chunk"); got != 0 {
					t.Fatalf("time to first chunk records = %d, want 0", got)
				}
				if got := p.GetHistogramCount(t, "gen_ai.client.inference.duration", attribute.String("openvaala.model_call.purpose", "compact")); got != 1 {
					t.Fatalf("duration records = %d, want 1", got)
				}
			},
		},
		{
			name: "chat_failure_logs_warning_without_span_events",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				recorder.End(errors.New("upstream 502"))
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				if span.Status().Code != codes.Error || span.Status().Description != "upstream 502" ||
					telemetrytest.GetSpanAttribute(span, "error.type") != "_OTHER" ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.response.finish_reasons") != `["error"]` || len(span.Events()) != 0 {
					t.Fatalf("status = %v, attributes = %v, events = %v", span.Status(), span.Attributes(), span.Events())
				}
				record := p.GetLogRecords(t, "model call failed")[0]
				if record["level"] != "WARN" || record["error"] != "upstream 502" {
					t.Fatalf("log = %v", record)
				}
				if got := p.GetHistogramCount(t, "gen_ai.client.inference.operation.input_tokens", attribute.String("error.type", "_OTHER")); got != 1 {
					t.Fatalf("operation input token records = %d, want 1", got)
				}
			},
		},
		{
			name: "chat_keeps_received_finish_reason_on_failure",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeCompact))
				recorder.RecordFinishReason(telemetry.FinishReasonLength)
				recorder.End(errors.New("compaction ended with finish reason length"))
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				if span.Status().Code != codes.Error || telemetrytest.GetSpanAttribute(span, "gen_ai.response.finish_reasons") != `["length"]` {
					t.Fatalf("status = %v, attributes = %v", span.Status(), span.Attributes())
				}
			},
		},
		{
			name: "chat_canceled_is_not_failure",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				recorder.End(fmt.Errorf("stream: %w", context.Canceled))
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				if span.Status().Code == codes.Error || telemetrytest.GetSpanAttribute(span, "error.type") != "canceled" ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.response.finish_reasons") != `["error"]` {
					t.Fatalf("status = %v, attributes = %v", span.Status(), span.Attributes())
				}
				if len(p.GetLogRecords(t, "model call failed")) != 0 || len(p.GetLogRecords(t, "model call finished")) != 1 {
					t.Fatalf("logs = %s", p.GetLogText())
				}
				record := p.GetLogRecords(t, "model call finished")[0]
				if record["level"] != "INFO" || record["error.type"] != "canceled" || record["error"] != "stream: context canceled" {
					t.Fatalf("log = %v", record)
				}
				canceled := attribute.String("error.type", "canceled")
				if got := p.GetHistogramCount(t, "gen_ai.client.inference.duration", canceled); got != 1 {
					t.Fatalf("canceled inference duration records = %d, want 1", got)
				}
				if got := p.GetHistogramCount(t, "gen_ai.client.inference.duration"); got != 1 {
					t.Fatalf("inference duration records = %d, want 1", got)
				}
			},
		},
		{
			// 流在取消后先报出不带取消原因的读错误，仍按取消记录。
			name: "chat_read_error_after_cancel_is_not_failure",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				callCtx, cancel := context.WithCancel(ctx)
				_, recorder := telemetry.StartChat(callCtx, newChatStart(telemetry.ChatPurposeCompact))
				cancel()
				recorder.End(errors.New("read frame: unexpected EOF"))
				span := p.WaitEndedSpans(t, "chat gpt-test", 1)[0]
				if span.Status().Code == codes.Error || telemetrytest.GetSpanAttribute(span, "error.type") != "canceled" {
					t.Fatalf("status = %v, error.type = %q", span.Status(), telemetrytest.GetSpanAttribute(span, "error.type"))
				}
				if len(p.GetLogRecords(t, "model call failed")) != 0 || p.GetLogRecords(t, "model call finished")[0]["level"] != "INFO" {
					t.Fatalf("logs = %s", p.GetLogText())
				}
			},
		},
		{
			name: "invoke_agent_records_summary",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				turnCtx, recorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				telemetry.GetLogger(turnCtx).Info("inside turn")
				recorder.End(telemetry.InvokeAgentEnd{
					Status: "completed",
					Summary: telemetry.TurnSummary{
						Steps:           2,
						InferenceCalls:  3,
						ToolCalls:       2,
						FailedToolCalls: 1,
						Usage: defs.TokenUsage{
							Input:  300,
							Output: 40,
						},
						FinishReason: telemetry.FinishReasonStop,
					},
				})
				span := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
				for key, want := range map[string]string{
					"gen_ai.operation.name":            "invoke_agent",
					"gen_ai.agent.description":         "通用助手",
					"gen_ai.request.model":             "gpt-test",
					"openvaala.turn.status":            "completed",
					"openvaala.turn.step_count":        "2",
					"openvaala.turn.tool_calls":        "2",
					"openvaala.turn.failed_tool_calls": "1",
					"gen_ai.usage.input_tokens":        "300",
					"gen_ai.usage.output_tokens":       "40",
					"gen_ai.response.finish_reasons":   `["stop"]`,
				} {
					if got := telemetrytest.GetSpanAttribute(span, key); got != want {
						t.Errorf("span %s = %q, want %q", key, got, want)
					}
				}
				if p.GetLogRecords(t, "inside turn")[0]["openvaala.turn.id"] != "turn-1" {
					t.Fatalf("turn identity missing: %s", p.GetLogText())
				}
				if len(p.GetLogRecords(t, "turn started")) != 1 || p.GetLogRecords(t, "turn finished")[0]["level"] != "INFO" {
					t.Fatalf("logs = %s", p.GetLogText())
				}
				if got := p.GetHistogramCount(t, "gen_ai.invoke_agent.duration", attribute.String("openvaala.turn.status", "completed")); got != 1 {
					t.Errorf("turn duration records = %d, want 1", got)
				}
				if got := p.GetMetricSum(t, "gen_ai.invoke_agent.inference_calls"); got != 3 {
					t.Errorf("inference calls = %v, want 3", got)
				}
				if got := p.GetMetricSum(t, "gen_ai.invoke_agent.tool_calls"); got != 2 {
					t.Errorf("tool calls = %v, want 2", got)
				}
			},
		},
		{
			name: "invoke_agent_failed",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				recorder.End(telemetry.InvokeAgentEnd{
					Status: "failed",
					Err:    errors.New("boom"),
				})
				span := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
				if span.Status().Code != codes.Error || telemetrytest.GetSpanAttribute(span, "gen_ai.response.finish_reasons") != `["error"]` {
					t.Fatalf("status = %v, attributes = %v", span.Status(), span.Attributes())
				}
				attrs := []attribute.KeyValue{attribute.String("openvaala.turn.status", "failed"), attribute.String("error.type", "_OTHER")}
				if got := p.GetHistogramCount(t, "gen_ai.invoke_agent.duration", attrs...); got != 1 {
					t.Fatalf("failed turn records = %d", got)
				}
				if p.GetLogRecords(t, "turn failed")[0]["level"] != "ERROR" {
					t.Fatalf("logs = %s", p.GetLogText())
				}
			},
		},
		{
			name: "invoke_agent_parent_from_span_ref",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				submitCtx, submitSpan := p.TracerProvider.Tracer("test").Start(ctx, "conversation/submitInput")
				parent := telemetry.GetSpanRef(submitCtx)
				submitSpan.End()
				_, recorder := telemetry.StartInvokeAgent(telemetry.ContextWithParentSpan(ctx, parent), newInvokeAgentStart())
				recorder.End(telemetry.InvokeAgentEnd{Status: "completed"})
				span := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
				if span.Parent().SpanID() != submitSpan.SpanContext().SpanID() {
					t.Fatalf("parent = %s, want %s", span.Parent().SpanID(), submitSpan.SpanContext().SpanID())
				}
			},
		},
		{
			name: "invoke_agent_canceled_is_not_failure",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				recorder.End(telemetry.InvokeAgentEnd{
					Status: "interrupted",
					Err:    context.Canceled,
				})
				span := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
				if span.Status().Code == codes.Error || telemetrytest.GetSpanAttribute(span, "error.type") != "canceled" ||
					telemetrytest.GetSpanAttribute(span, "openvaala.turn.status") != "interrupted" {
					t.Fatalf("status = %v, attributes = %v", span.Status(), span.Attributes())
				}
				if len(p.GetLogRecords(t, "turn failed")) != 0 || len(p.GetLogRecords(t, "turn finished")) != 1 {
					t.Fatalf("logs = %s", p.GetLogText())
				}
				record := p.GetLogRecords(t, "turn finished")[0]
				if record["level"] != "INFO" || record["error.type"] != "canceled" || record["error"] != "context canceled" {
					t.Fatalf("log = %v", record)
				}
				canceled := attribute.String("error.type", "canceled")
				if got := p.GetHistogramCount(t, "gen_ai.invoke_agent.duration", canceled); got != 1 {
					t.Fatalf("canceled invoke_agent duration records = %d, want 1", got)
				}
				if got := p.GetHistogramCount(t, "gen_ai.invoke_agent.duration"); got != 1 {
					t.Fatalf("invoke_agent duration records = %d, want 1", got)
				}
			},
		},
		{
			name: "execute_tool_error_code",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				ctx = telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID("local"))
				_, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:        "bash",
					CallID:      "call-1",
					Description: "执行命令",
				})
				recorder.End("tool_exit_nonzero")
				span := p.WaitEndedSpans(t, "execute_tool bash", 1)[0]
				if span.Status().Code != codes.Error || span.Status().Description != "" {
					t.Fatalf("status = %v", span.Status())
				}
				for key, want := range map[string]string{
					"gen_ai.operation.name":   "execute_tool",
					"gen_ai.tool.call.id":     "call-1",
					"gen_ai.tool.type":        "function",
					"gen_ai.tool.description": "执行命令",
					"openvaala.executor.id":   "local",
					"error.type":              "tool_exit_nonzero",
				} {
					if got := telemetrytest.GetSpanAttribute(span, key); got != want {
						t.Errorf("span %s = %q, want %q", key, got, want)
					}
				}
				attrs := []attribute.KeyValue{attribute.String("openvaala.executor.id", "local"), attribute.String("error.type", "tool_exit_nonzero")}
				if got := p.GetHistogramCount(t, "gen_ai.execute_tool.duration", attrs...); got != 1 {
					t.Fatalf("tool records = %d", got)
				}
				if record := p.GetLogRecords(t, "tool call finished")[0]; record["level"] != "INFO" || record["error.type"] != "tool_exit_nonzero" {
					t.Fatalf("log = %v", record)
				}
			},
		},
		{
			name: "execute_tool_success",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:   "read_file",
					CallID: "call-2",
				})
				recorder.End("")
				span := p.WaitEndedSpans(t, "execute_tool read_file", 1)[0]
				if span.Status().Code == codes.Error || telemetrytest.GetSpanAttribute(span, "error.type") != "" ||
					telemetrytest.GetSpanAttribute(span, "gen_ai.tool.description") != "" {
					t.Fatalf("span = %v %v", span.Status(), span.Attributes())
				}
				records := p.GetLogRecords(t, "tool call finished")
				if len(records) != 1 {
					t.Fatalf("tool call finished logs = %d, want 1", len(records))
				}
				if _, ok := records[0]["error.type"]; ok || records[0]["level"] != "INFO" {
					t.Fatalf("log = %v, want INFO without error.type", records[0])
				}
			},
		},
		{
			name: "rpc_server_error_code",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartRPCServer(ctx, "conversation/read", "conversation/read", "7")
				recorder.End(-32000, errors.New("conversation not found"))
				span := p.WaitEndedSpans(t, "conversation/read", 1)[0]
				if span.SpanKind() != trace.SpanKindServer || span.Status().Code != codes.Error {
					t.Fatalf("kind = %v, status = %v", span.SpanKind(), span.Status())
				}
				for key, want := range map[string]string{
					"rpc.system.name":          "jsonrpc",
					"rpc.method":               "conversation/read",
					"rpc.response.status_code": "-32000",
					"jsonrpc.request.id":       "7",
					"error.type":               "-32000",
					"rpc.method_original":      "",
				} {
					if got := telemetrytest.GetSpanAttribute(span, key); got != want {
						t.Errorf("span %s = %q, want %q", key, got, want)
					}
				}
				attrs := []attribute.KeyValue{attribute.String("rpc.method", "conversation/read"), attribute.String("error.type", "-32000")}
				if got := p.GetHistogramCount(t, "rpc.server.call.duration", attrs...); got != 1 {
					t.Fatalf("rpc records = %d", got)
				}
				record := p.GetLogRecords(t, "rpc request failed")[0]
				if record["level"] != "ERROR" || record["rpc.response.status_code"] != "-32000" {
					t.Fatalf("log = %v", record)
				}
			},
		},
		{
			name: "rpc_server_unknown_method_uses_system_name",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartRPCServer(ctx, telemetry.RPCMethodOther, "conversation/unknown", "")
				recorder.End(-32601, errors.New("method not found"))
				span := p.WaitEndedSpans(t, "jsonrpc", 1)[0]
				if telemetrytest.GetSpanAttribute(span, "rpc.method") != "_OTHER" ||
					telemetrytest.GetSpanAttribute(span, "rpc.method_original") != "conversation/unknown" ||
					telemetrytest.GetSpanAttribute(span, "jsonrpc.request.id") != "" {
					t.Fatalf("attributes = %v", span.Attributes())
				}
				if keys := p.GetMetricAttributeKeys(t, "rpc.server.call.duration"); slices.Contains(keys, "rpc.method_original") {
					t.Fatalf("rpc metric keys = %v", keys)
				}
			},
		},
		{
			name: "rpc_server_success",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartRPCServer(ctx, "conversation/read", "conversation/read", "8")
				recorder.End(0, nil)
				span := p.WaitEndedSpans(t, "conversation/read", 1)[0]
				if span.Status().Code == codes.Error || telemetrytest.GetSpanAttribute(span, "rpc.response.status_code") != "" ||
					telemetrytest.GetSpanAttribute(span, "error.type") != "" {
					t.Fatalf("status = %v, attributes = %v", span.Status(), span.Attributes())
				}
				if keys := p.GetMetricAttributeKeys(t, "rpc.server.call.duration"); slices.Contains(keys, "rpc.response.status_code") || slices.Contains(keys, "error.type") {
					t.Fatalf("rpc metric keys = %v", keys)
				}
				if len(p.GetLogRecords(t, "rpc request failed")) != 0 || p.GetLogRecords(t, "rpc request handled")[0]["level"] != "DEBUG" {
					t.Fatalf("logs = %s", p.GetLogText())
				}
			},
		},
		{
			name: "rpc_server_canceled_is_not_failure",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				_, recorder := telemetry.StartRPCServer(ctx, "conversation/read", "conversation/read", "9")
				recorder.End(-32000, context.Canceled)
				span := p.WaitEndedSpans(t, "conversation/read", 1)[0]
				if span.Status().Code == codes.Error || telemetrytest.GetSpanAttribute(span, "error.type") != "canceled" {
					t.Fatalf("status = %v, attributes = %v", span.Status(), span.Attributes())
				}
				if len(p.GetLogRecords(t, "rpc request failed")) != 0 {
					t.Fatalf("logs = %s", p.GetLogText())
				}
				record := p.GetLogRecords(t, "rpc request handled")[0]
				if record["level"] != "DEBUG" || record["error.type"] != "canceled" || record["error"] != "context canceled" {
					t.Fatalf("log = %v", record)
				}
			},
		},
		{
			name: "recorders_without_context_telemetry_use_fallback",
			run: func(t *testing.T, _ context.Context, p *telemetrytest.Pipeline) {
				ctx := context.Background()
				_, rpcRecorder := telemetry.StartRPCServer(ctx, "conversation/read", "conversation/read", "1")
				rpcRecorder.End(0, nil)
				_, turnRecorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				turnRecorder.End(telemetry.InvokeAgentEnd{Status: "completed"})
				_, chatRecorder := telemetry.StartChat(ctx, newChatStart(telemetry.ChatPurposeConversation))
				chatRecorder.RecordUsage(defs.TokenUsage{
					Input:  1,
					Output: 1,
				})
				chatRecorder.End(errors.New("boom"))
				_, toolRecorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
					Name:   "bash",
					CallID: "call-1",
				})
				toolRecorder.End("tool_exit_nonzero")
				telemetry.AddActiveConversations(ctx, 1)
				telemetry.AddActiveConnections(ctx, 1)
				if spans := p.GetEndedSpans(); len(spans) != 0 || p.GetLogText() != "" {
					t.Fatalf("explicit pipeline recorded fallback operations: %d spans, logs = %s", len(spans), p.GetLogText())
				}
			},
		},
		{
			name: "metric_dimensions_exclude_identity_ids",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{ConversationID: "conversation-1", SessionID: "session-1", AgentName: "assistant"})
				ctx = telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID("local"), telemetry.TurnStep(1))
				turnCtx, turnRecorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				start := newChatStart(telemetry.ChatPurposeConversation)
				start.ServerAddress, start.ServerPort = "llm.example.com", 443
				chatCtx, chatRecorder := telemetry.StartChat(turnCtx, start)
				chatRecorder.RecordResponseModel("gpt-test-2026")
				chatRecorder.RecordUsage(defs.TokenUsage{
					Input:  1,
					Output: 1,
				})
				chatRecorder.End(errors.New("boom"))
				_, toolRecorder := telemetry.StartExecuteTool(chatCtx, telemetry.ExecuteToolStart{
					Name:   "bash",
					CallID: "call-1",
				})
				toolRecorder.End("tool_exit_nonzero")
				turnRecorder.End(telemetry.InvokeAgentEnd{
					Status: "failed",
					Err:    errors.New("boom"),
				})
				_, rpcRecorder := telemetry.StartRPCServer(ctx, "conversation/read", "conversation/read", "1")
				rpcRecorder.End(-32000, errors.New("not found"))
				inference := []string{
					"gen_ai.agent.name", "gen_ai.operation.name", "gen_ai.provider.name", "gen_ai.request.model", "gen_ai.response.model",
					"openvaala.executor.id", "openvaala.model_call.purpose", "server.address", "server.port",
				}
				for name, allowed := range map[string][]string{
					"gen_ai.client.inference.usage.input_tokens":     append(slices.Clone(inference), "gen_ai.token.modality"),
					"gen_ai.client.inference.duration":               append(slices.Clone(inference), "error.type"),
					"gen_ai.client.inference.operation.input_tokens": append(slices.Clone(inference), "error.type"),
					"gen_ai.invoke_agent.duration":                   {"error.type", "gen_ai.agent.name", "gen_ai.request.model", "openvaala.executor.id", "openvaala.turn.status"},
					"gen_ai.invoke_agent.tool_calls":                 {"gen_ai.agent.name", "openvaala.executor.id"},
					"gen_ai.execute_tool.duration":                   {"error.type", "gen_ai.agent.name", "gen_ai.tool.name", "gen_ai.tool.type", "openvaala.executor.id"},
					"rpc.server.call.duration":                       {"error.type", "rpc.method", "rpc.response.status_code", "rpc.system.name"},
				} {
					keys := p.GetMetricAttributeKeys(t, name)
					if len(keys) == 0 {
						t.Errorf("metric %s has no data points", name)
					}
					for _, key := range keys {
						if !slices.Contains(allowed, key) {
							t.Errorf("metric %s has dimension %q, allowed %v", name, key, allowed)
						}
					}
				}
			},
		},
		{
			name: "histogram_bucket_bounds_follow_conventions",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				turnCtx, turnRecorder := telemetry.StartInvokeAgent(ctx, newInvokeAgentStart())
				_, chatRecorder := telemetry.StartChat(turnCtx, newChatStart(telemetry.ChatPurposeConversation))
				chatRecorder.RecordUsage(defs.TokenUsage{
					Input:  1,
					Output: 1,
				})
				chatRecorder.End(nil)
				_, toolRecorder := telemetry.StartExecuteTool(turnCtx, telemetry.ExecuteToolStart{
					Name:   "bash",
					CallID: "call-1",
				})
				toolRecorder.End("")
				turnRecorder.End(telemetry.InvokeAgentEnd{
					Status: "completed",
					Summary: telemetry.TurnSummary{
						InferenceCalls: 1,
						ToolCalls:      1,
					},
				})
				_, rpcRecorder := telemetry.StartRPCServer(ctx, "conversation/read", "conversation/read", "1")
				rpcRecorder.End(0, nil)
				inferenceBounds := []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
				for name, want := range map[string][]float64{
					"gen_ai.client.inference.duration":               inferenceBounds,
					"gen_ai.client.inference.operation.input_tokens": {1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864},
					"gen_ai.invoke_agent.duration":                   {0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8, 25.6, 51.2, 102.4, 204.8, 409.6, 819.2, 1638.4, 3276.8},
					"gen_ai.invoke_agent.inference_calls":            {1, 2, 4, 8, 16, 32, 64, 128},
					"gen_ai.execute_tool.duration":                   append(slices.Clone(inferenceBounds), 163.84, 327.68, 655.36, 1310.72, 2621.44),
					"rpc.server.call.duration":                       {0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10},
				} {
					if got := p.GetHistogramBounds(t, name); !slices.Equal(got, want) {
						t.Errorf("metric %s bounds = %v, want %v", name, got, want)
					}
				}
			},
		},
		{
			name: "active_gauges",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				telemetry.AddActiveConversations(ctx, 1)
				telemetry.AddActiveConversations(ctx, 1)
				telemetry.AddActiveConversations(ctx, -1)
				telemetry.AddActiveConnections(ctx, 1)
				if got := p.GetMetricSum(t, "openvaala.conversation.active"); got != 1 {
					t.Fatalf("active conversations = %v", got)
				}
				if got := p.GetMetricSum(t, "openvaala.server.connection.active"); got != 1 {
					t.Fatalf("active connections = %v", got)
				}
			},
		},
	}
	// 2. 执行记录操作，比较 span 属性、状态、指标、分桶及日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			tt.run(t, telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), p)
		})
	}
}

// newChatStart 创建模型调用记录的开始参数。
func newChatStart(purpose string) telemetry.ChatStart {
	// 1. 填入调用 ID、测试模型、协议家族和用途。
	return telemetry.ChatStart{
		ModelCallID: "call-1",
		Model:       "gpt-test",
		Provider:    "openai",
		Purpose:     purpose,
	}
}

// newInvokeAgentStart 创建 Turn 记录的开始参数。
func newInvokeAgentStart() telemetry.InvokeAgentStart {
	// 1. 填入 Agent 名、说明、模型和 Turn ID。
	return telemetry.InvokeAgentStart{
		AgentName:        "assistant",
		AgentDescription: "通用助手",
		Model:            "gpt-test",
		TurnID:           "turn-1",
	}
}
