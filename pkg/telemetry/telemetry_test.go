package telemetry_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// TestNewTelemetry 验证遥测管道构造。
func TestNewTelemetry(t *testing.T) {
	// 1. 准备完整及缺少出口的配置。
	tracerProvider := tracenoop.NewTracerProvider()
	meterProvider := metricnoop.NewMeterProvider()
	logger := slog.New(slog.DiscardHandler)
	tests := []struct {
		name    string
		opt     *telemetry.TelemetryOpt
		wantErr bool
	}{
		{
			name: "complete_opt",
			opt:  telemetry.NewTelemetryOpt(tracerProvider, meterProvider, logger),
		},
		{
			name:    "nil_opt",
			opt:     nil,
			wantErr: true,
		},
		{
			name:    "nil_tracer_provider",
			opt:     telemetry.NewTelemetryOpt(nil, meterProvider, logger),
			wantErr: true,
		},
		{
			name:    "nil_meter_provider",
			opt:     telemetry.NewTelemetryOpt(tracerProvider, nil, logger),
			wantErr: true,
		},
		{
			name:    "nil_logger",
			opt:     telemetry.NewTelemetryOpt(tracerProvider, meterProvider, nil),
			wantErr: true,
		},
	}
	// 2. 构造遥测管道，比较返回值及错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := telemetry.NewTelemetry(tt.opt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewTelemetry() error = %v, wantErr %v", err, tt.wantErr)
			}
			if (got == nil) != tt.wantErr {
				t.Fatalf("NewTelemetry() = %v, wantErr %v", got, tt.wantErr)
			}
		})
	}
}

// TestTelemetryContext 验证管道和身份的上下文传递。
func TestTelemetryContext(t *testing.T) {
	// 1. 准备身份覆盖、日志字段、span 信息和全局备用管道样例。
	tests := []struct {
		name string
		run  func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline)
	}{
		{
			name: "identity_in_log_fields",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{ConversationID: "conv-1", SessionID: "session-1"})
				telemetry.GetLogger(ctx).Info("session started")
				records := p.GetLogRecords(t, "session started")
				if len(records) != 1 || records[0]["gen_ai.conversation.id"] != "conv-1" || records[0]["openvaala.session.id"] != "session-1" {
					t.Fatalf("records = %v", records)
				}
			},
		},
		{
			name: "later_attr_with_same_key_wins",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				parentCtx := defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{SessionID: "parent", AgentName: "assistant"})
				childCtx := defs.ContextWithSessionCtxMeta(parentCtx, defs.SessionCtxMeta{SessionID: "child"})
				telemetry.GetLogger(childCtx).Info("child log")
				telemetry.GetLogger(parentCtx).Info("parent log")
				child, parent := p.GetLogRecords(t, "child log")[0], p.GetLogRecords(t, "parent log")[0]
				if child["openvaala.session.id"] != "child" || child["gen_ai.agent.name"] != "assistant" || parent["openvaala.session.id"] != "parent" {
					t.Fatalf("child = %v, parent = %v", child, parent)
				}
			},
		},
		{
			name: "empty_meta_fields_not_in_log_fields",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{SessionID: "session-1", AgentName: "assistant"})
				telemetry.GetLogger(ctx).Info("root session")
				record := p.GetLogRecords(t, "root session")[0]
				for _, key := range []string{"gen_ai.conversation.id", "openvaala.parent_session.id"} {
					if _, found := record[key]; found {
						t.Fatalf("record = %v, want no %s", record, key)
					}
				}
				if record["openvaala.session.id"] != "session-1" || record["gen_ai.agent.name"] != "assistant" {
					t.Fatalf("record = %v", record)
				}
			},
		},
		{
			name: "meta_after_attrs_overrides_identity_once",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{SessionID: "parent"})
				ctx = telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID("local"))
				ctx = defs.ContextWithSessionCtxMeta(ctx, defs.SessionCtxMeta{SessionID: "child"})
				telemetry.GetLogger(ctx).Info("child log")
				record := p.GetLogRecords(t, "child log")[0]
				if record["openvaala.session.id"] != "child" || record["openvaala.executor.id"] != "local" {
					t.Fatalf("record = %v", record)
				}
				if count := strings.Count(p.GetLogText(), `"openvaala.session.id"`); count != 1 {
					t.Fatalf("session id key appears %d times in %s, want once", count, p.GetLogText())
				}
			},
		},
		{
			name: "same_call_duplicate_key_keeps_last",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				ctx = telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID("a"), telemetry.ExecutorID("b"))
				telemetry.GetLogger(ctx).Info("duplicate key")
				records := p.GetLogRecords(t, "duplicate key")
				if len(records) != 1 || records[0]["openvaala.executor.id"] != "b" {
					t.Fatalf("records = %v", records)
				}
				if count := strings.Count(p.GetLogText(), `"openvaala.executor.id"`); count != 1 {
					t.Fatalf("executor id key appears %d times in %s, want once", count, p.GetLogText())
				}
			},
		},
		{
			name: "no_attrs_keeps_context",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				if got := telemetry.ContextWithAttrs(ctx); got != ctx {
					t.Fatal("ContextWithAttrs() without attrs returned a different ctx")
				}
				telemetry.GetLogger(telemetry.ContextWithAttrs(ctx)).Info("no identity")
				record := p.GetLogRecords(t, "no identity")[0]
				for _, key := range []string{"gen_ai.conversation.id", "openvaala.session.id", "trace_id", "span_id"} {
					if _, found := record[key]; found {
						t.Fatalf("record = %v, want no %s", record, key)
					}
				}
			},
		},
		{
			name: "span_ids_in_log_fields",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				spanCtx, span := p.TracerProvider.Tracer("test").Start(ctx, "operation")
				telemetry.GetLogger(spanCtx).Warn("inside span")
				span.End()
				record := p.GetLogRecords(t, "inside span")[0]
				if record["trace_id"] != span.SpanContext().TraceID().String() || record["span_id"] != span.SpanContext().SpanID().String() {
					t.Fatalf("record = %v, want span %s", record, span.SpanContext().SpanID())
				}
			},
		},
		{
			name: "context_pipeline_takes_precedence_over_fallback",
			run: func(t *testing.T, ctx context.Context, p *telemetrytest.Pipeline) {
				if telemetry.GetTelemetry(ctx) != p.Telemetry {
					t.Fatal("GetTelemetry() did not return the pipeline on ctx")
				}
				if telemetry.GetTelemetry(telemetry.ContextWithTelemetry(ctx, nil)) != p.Telemetry {
					t.Fatal("ContextWithTelemetry(nil) replaced the pipeline on ctx")
				}
				fallback := telemetry.GetTelemetry(context.Background())
				if fallback == nil || fallback == p.Telemetry || fallback != telemetry.GetTelemetry(context.Background()) {
					t.Fatal("fallback telemetry must be a single instance distinct from the pipeline")
				}
			},
		},
	}
	// 2. 记录日志或查询管道，核对身份覆盖、trace 信息和全局备用实例。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			tt.run(t, telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), p)
		})
	}
}

// TestContextWithParentSpan 验证父 span 引用的上下文传递。
func TestContextWithParentSpan(t *testing.T) {
	// 1. 准备零值和有效引用。
	tests := []struct {
		name       string
		withParent bool
	}{
		{
			name:       "zero_ref_keeps_context",
			withParent: false,
		},
		{
			name:       "valid_ref_becomes_parent",
			withParent: true,
		},
	}
	// 2. 创建子 span，比较有效父引用和零值引用的结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			tracer := p.TracerProvider.Tracer("test")
			var parent telemetry.SpanRef
			var submitterSpanID string
			if tt.withParent {
				submitCtx, submitSpan := tracer.Start(t.Context(), "conversation/submitInput")
				parent = telemetry.GetSpanRef(submitCtx)
				submitterSpanID = submitSpan.SpanContext().SpanID().String()
				submitSpan.End()
			}
			_, child := tracer.Start(telemetry.ContextWithParentSpan(t.Context(), parent), "turn")
			child.End()
			ended := p.WaitEndedSpans(t, "turn", 1)[0]
			if tt.withParent && ended.Parent().SpanID().String() != submitterSpanID {
				t.Fatalf("parent = %s, want %s", ended.Parent().SpanID(), submitterSpanID)
			}
			if !tt.withParent && ended.Parent().IsValid() {
				t.Fatalf("parent = %s, want none", ended.Parent().SpanID())
			}
		})
	}
}
