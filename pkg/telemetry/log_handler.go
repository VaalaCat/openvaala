package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// contextHandler 在写出前把 ctx 上的实体身份与有效 span 的 trace_id、span_id 加进日志记录，再把同一 ctx 交给内层 handler；
// 内层的 OTLP 桥接据此把日志关联到 trace。
type contextHandler struct {
	next slog.Handler
}

// newContextHandler 创建补充上下文属性的日志处理器。
func newContextHandler(next slog.Handler) *contextHandler {
	// 1. 保存下一层日志处理器。
	return &contextHandler{next: next}
}

// Enabled 内层处理器是否接收此级别的日志？
func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// 1. 用本次 ctx 和级别查询内层处理器。
	return h.next.Enabled(ctx, level)
}

// Handle 向日志添加上下文身份和 trace 信息并写出。
func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	// 1. 克隆记录并追加 ctx 身份。
	record = record.Clone()
	for _, attr := range getAttrs(ctx) {
		record.AddAttrs(slog.Any(string(attr.Key), attr.Value.AsInterface()))
	}
	// 2. 有有效 span 时追加 trace_id 和 span_id，再将同一 ctx 和记录交给内层处理器。
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, record)
}

// WithAttrs 创建附加固定属性的日志处理器。
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// 1. 在内层加属性，再保留上下文属性处理。
	return newContextHandler(h.next.WithAttrs(attrs))
}

// WithGroup 创建指定分组的日志处理器。
func (h *contextHandler) WithGroup(name string) slog.Handler {
	// 1. 在内层增加分组，再保留上下文属性处理。
	return newContextHandler(h.next.WithGroup(name))
}

// boundContextHandler 把 GetLogger 绑定的 ctx 用于每次写出：调用方用不带 ctx 的 Info、Warn 也能补齐身份与 trace。
type boundContextHandler struct {
	ctx  context.Context
	next slog.Handler
}

// Enabled 绑定上下文的处理器是否接收此级别的日志？
func (h *boundContextHandler) Enabled(_ context.Context, level slog.Level) bool {
	// 1. 使用已绑定的 ctx 查询内层处理器。
	return h.next.Enabled(h.ctx, level)
}

// Handle 用已绑定的上下文写出日志。
func (h *boundContextHandler) Handle(_ context.Context, record slog.Record) error {
	// 1. 将绑定的 ctx 和记录交给内层处理器。
	return h.next.Handle(h.ctx, record)
}

// WithAttrs 在保留绑定上下文的同时增加固定属性。
func (h *boundContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// 1. 复制绑定的 ctx，内层处理器加上指定属性。
	return &boundContextHandler{
		ctx:  h.ctx,
		next: h.next.WithAttrs(attrs),
	}
}

// WithGroup 在保留绑定上下文的同时增加分组。
func (h *boundContextHandler) WithGroup(name string) slog.Handler {
	// 1. 复制绑定的 ctx，内层处理器加上指定分组。
	return &boundContextHandler{
		ctx:  h.ctx,
		next: h.next.WithGroup(name),
	}
}
