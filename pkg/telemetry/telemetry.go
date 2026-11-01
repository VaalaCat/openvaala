// Package telemetry 是全项目统一的遥测出口：trace、指标与日志经同一条管道写出，实体身份经 ctx 下传。
// 本包按 OTel 属性名称记录操作、用量和内容。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/semconv/v1.43.0/rpcconv"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry 是一条遥测管道的只读句柄：trace、指标、日志的出口。应用构造一次后传给各入口，可并发使用。
type Telemetry struct {
	tracer trace.Tracer
	// logger 已包上 contextHandler，写出时补齐 ctx 身份与 trace_id / span_id。
	logger *slog.Logger
	// instruments 是按全项目统一命名创建好的指标仪表。
	instruments telemetryInstruments
	// captureContent 是否把对话内容写入 span？日志与指标不受影响，永不含内容。
	captureContent bool
}

// TelemetryOpt 是管道的三个出口；嵌入方可以传入任意 OTel Provider 与 slog Logger。
type TelemetryOpt struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Logger         *slog.Logger
	// CaptureContent 是否把对话内容写入 span 属性？NewTelemetryOpt 默认开启，与配置默认值一致。
	CaptureContent bool
}

// NewTelemetryOpt 创建遥测出口配置。
func NewTelemetryOpt(tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider, logger *slog.Logger) *TelemetryOpt {
	// 1. 保存 Tracer、Meter 和日志出口，开启 CaptureContent。
	return &TelemetryOpt{
		TracerProvider: tracerProvider,
		MeterProvider:  meterProvider,
		Logger:         logger,
		CaptureContent: true,
	}
}

// WithCaptureContent 设置内容采集开关并返回配置。
func (o *TelemetryOpt) WithCaptureContent(capture bool) *TelemetryOpt {
	// 1. 更新是否把对话内容写入 span。
	o.CaptureContent = capture
	return o
}

// NewTelemetry 校验出口并创建遥测管道。
func NewTelemetry(opt *TelemetryOpt) (*Telemetry, error) {
	// 1. 检查三个出口非 nil，再按统一 scope 创建指标仪表。
	if opt == nil || opt.TracerProvider == nil || opt.MeterProvider == nil || opt.Logger == nil {
		return nil, errors.New("telemetry requires a tracer provider, a meter provider and a logger")
	}
	instruments, err := newTelemetryInstruments(opt.MeterProvider.Meter(ScopeName))
	if err != nil {
		return nil, err
	}
	// 2. 保存 Tracer、内容开关和仪表，日志包上上下文身份处理器。
	return &Telemetry{
		tracer:         opt.TracerProvider.Tracer(ScopeName),
		logger:         slog.New(newContextHandler(opt.Logger.Handler())),
		instruments:    instruments,
		captureContent: opt.CaptureContent,
	}, nil
}

// newTelemetryInstruments 创建全部指标仪表并汇总创建错误。
func newTelemetryInstruments(meter metric.Meter) (telemetryInstruments, error) {
	// 1. 准备带单位、说明和显式分桶的直方图与计数器创建函数。
	var errs []error
	float64Histogram := func(name, description string, buckets []float64) metric.Float64Histogram {
		histogram, err := meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(description),
			metric.WithExplicitBucketBoundaries(buckets...))
		errs = append(errs, err)
		return histogram
	}
	int64Histogram := func(name, unit, description string, buckets []float64) metric.Int64Histogram {
		histogram, err := meter.Int64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description),
			metric.WithExplicitBucketBoundaries(buckets...))
		errs = append(errs, err)
		return histogram
	}
	tokenCounter := func(name, description string) metric.Int64Counter {
		counter, err := meter.Int64Counter(name, metric.WithUnit("{token}"), metric.WithDescription(description))
		errs = append(errs, err)
		return counter
	}
	upDownCounter := func(name, unit, description string) metric.Int64UpDownCounter {
		counter, err := meter.Int64UpDownCounter(name, metric.WithUnit(unit), metric.WithDescription(description))
		errs = append(errs, err)
		return counter
	}
	// 2. 创建 RPC 耗时和模型、Turn、工具、常驻计数仪表。
	rpcDuration, err := rpcconv.NewServerCallDuration(meter)
	errs = append(errs, err)
	instruments := telemetryInstruments{
		inferenceDuration:           float64Histogram(inferenceDurationName, "一次模型调用的耗时。", inferenceDurationBuckets),
		inferenceTimeToFirstChunk:   float64Histogram(inferenceTimeToFirstChunkName, "从发起调用到收到首个输出分块的耗时。", inferenceDurationBuckets),
		inferenceTimePerOutputChunk: float64Histogram(inferenceTimePerOutputChunkName, "第二个起每个输出分块与上一个分块的间隔。", inferenceDurationBuckets),
		inferenceInputTokens:        tokenCounter(inferenceInputTokensName, "模型调用的输入 token 数，含缓存命中。"),
		inferenceOutputTokens:       tokenCounter(inferenceOutputTokensName, "模型调用的输出 token 数，含推理 token。"),
		inferenceCacheReadTokens:    tokenCounter(inferenceCacheReadTokensName, "从供应商缓存读取的输入 token 数。"),
		inferenceCacheWriteTokens:   tokenCounter(inferenceCacheWriteTokensName, "写入供应商缓存的输入 token 数。"),
		inferenceReasoningTokens:    tokenCounter(inferenceReasoningTokensName, "用于推理的输出 token 数。"),
		inferenceOperationInputTokens: int64Histogram(inferenceOperationInputTokensName, "{token}",
			"单次模型调用的输入 token 数分布。", tokenCountBuckets),
		inferenceOperationOutputTokens: int64Histogram(inferenceOperationOutputTokensName, "{token}",
			"单次模型调用的输出 token 数分布。", tokenCountBuckets),
		invokeAgentDuration: float64Histogram(invokeAgentDurationName, "Agent Turn 从开始到结束的耗时。", invokeAgentDurationBuckets),
		invokeAgentInferenceCalls: int64Histogram(invokeAgentInferenceCallsName, "{inference_call}",
			"每个 Turn 的模型调用次数，含压缩调用。", callCountBuckets),
		invokeAgentToolCalls: int64Histogram(invokeAgentToolCallsName, "{tool_call}", "每个 Turn 收到结果的工具调用次数。", callCountBuckets),
		executeToolDuration:  float64Histogram(executeToolDurationName, "一次工具调用的执行耗时。", executeToolDurationBuckets),
		rpcDuration:          rpcDuration,
		activeConversations:  upDownCounter(conversationActiveName, "{conversation}", "Runtime 中运行的对话数。"),
		activeConnections:    upDownCounter(connectionActiveName, "{connection}", "Runtime Server 上已建立的 WebSocket 连接数。"),
	}
	// 3. 汇总仪表创建错误，无错误时返回全部仪表。
	if err := errors.Join(errs...); err != nil {
		return telemetryInstruments{}, fmt.Errorf("create metric instruments: %w", err)
	}
	return instruments, nil
}

// ── ctx 上的管道与身份 ──

// telemetryContextKey 与 attrsContextKey 是本包在 ctx 上保存管道与实体身份的键。
type (
	telemetryContextKey struct{}
	attrsContextKey     struct{}
)

// ContextWithTelemetry 将遥测管道挂到上下文。
func ContextWithTelemetry(ctx context.Context, telemetry *Telemetry) context.Context {
	// 1. nil 管道时原样返回 ctx，否则保存到新 ctx。
	if telemetry == nil {
		return ctx
	}
	return context.WithValue(ctx, telemetryContextKey{}, telemetry)
}

// GetTelemetry 获取上下文使用的遥测管道。
func GetTelemetry(ctx context.Context) *Telemetry {
	// 1. 优先取 ctx 中的管道，未提供时使用首次创建的全局管道。
	if telemetry, found := ctx.Value(telemetryContextKey{}).(*Telemetry); found {
		return telemetry
	}
	return getFallbackTelemetry()
}

// getFallbackTelemetry 是嵌入方未传入管道时的全局兜底：首次使用时用 OTel 全局 Provider 与当时的 slog.Default() 构造一次。
var getFallbackTelemetry = sync.OnceValue(func() *Telemetry {
	telemetry, err := NewTelemetry(NewTelemetryOpt(otel.GetTracerProvider(), otel.GetMeterProvider(), slog.Default()))
	if err != nil {
		// 全局 Provider 均非 nil，指标名都是常量，构造失败不可达。
		panic(fmt.Sprintf("create fallback telemetry: %v", err))
	}
	return telemetry
})

// ContextWithAttrs 把实体身份追加到上下文。
func ContextWithAttrs(ctx context.Context, attrs ...attribute.KeyValue) context.Context {
	// 1. 无新增属性时返回原 ctx，否则合并属性，同键以后挂入的值为准。
	if len(attrs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, attrsContextKey{}, mergeAttrs(getAttrs(ctx), attrs))
}

// getAttrs 返回上下文中的实体身份。
func getAttrs(ctx context.Context) []attribute.KeyValue {
	// 1. 读取已挂入的属性切片，未提供时返回 nil；调用方不得修改返回的切片。
	attrs, _ := ctx.Value(attrsContextKey{}).([]attribute.KeyValue)
	return attrs
}

// GetLogger 返回绑定当前上下文的日志对象。
func GetLogger(ctx context.Context) *slog.Logger {
	// 1. 从当前管道获取带身份和 trace 的日志对象。
	return GetTelemetry(ctx).GetLogger(ctx)
}

// GetLogger 创建绑定上下文的日志对象。
func (t *Telemetry) GetLogger(ctx context.Context) *slog.Logger {
	// 1. 将 ctx 绑定到日志处理器，写入时补充身份和 trace_id、span_id。
	return slog.New(&boundContextHandler{
		ctx:  ctx,
		next: t.logger.Handler(),
	})
}

// ── 跨收件箱传递父 span ──

// SpanRef 保存 span 的 trace ID 和 span ID，不携带 ctx 的取消信号和实体身份。
type SpanRef struct {
	spanContext trace.SpanContext
}

// GetSpanRef 取得上下文中的 span 引用。
func GetSpanRef(ctx context.Context) SpanRef {
	// 1. 读取 trace ID 和 span ID，无 span 时返回零值引用。
	return SpanRef{spanContext: trace.SpanContextFromContext(ctx)}
}

// ContextWithParentSpan 将给定 span 引用设为后续 span 的父节点。
func ContextWithParentSpan(ctx context.Context, parent SpanRef) context.Context {
	// 1. 无效引用时返回原 ctx，否则挂入父 span 上下文。
	if !parent.spanContext.IsValid() {
		return ctx
	}
	return trace.ContextWithSpanContext(ctx, parent.spanContext)
}
