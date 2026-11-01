package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/rpcconv"
	"go.opentelemetry.io/otel/trace"
)

// recordedOperation 是一次操作开始时记下的管道、带 span 的 ctx、span、开始时间与指标维度；四种 Recorder 共用。
type recordedOperation struct {
	telemetry   *Telemetry
	ctx         context.Context
	span        trace.Span
	startedAt   time.Time
	metricAttrs []attribute.KeyValue
}

// startOperation 创建带操作身份的 span 和记录信息。
func startOperation(
	ctx context.Context,
	spanName string,
	kind trace.SpanKind,
	identity []attribute.KeyValue,
	operationAttrs []attribute.KeyValue,
) (context.Context, recordedOperation) {
	// 1. 读取管道并挂入身份，组装 ctx 属性、操作属性和业务调用方函数名；Start 直接调用时业务方在调用栈第 3 层。
	telemetry := GetTelemetry(ctx)
	ctx = ContextWithAttrs(ctx, identity...)
	spanAttrs := slices.Concat(getAttrs(ctx), operationAttrs, []attribute.KeyValue{
		semconv.CodeFunctionName(getCallerFunctionName(3)),
	})
	// 2. 创建 span，记录开始时间和指标维度，返回带 span 的 ctx。
	ctx, span := telemetry.tracer.Start(ctx, spanName, trace.WithSpanKind(kind), trace.WithAttributes(spanAttrs...))
	return ctx, recordedOperation{
		telemetry:   telemetry,
		ctx:         ctx,
		span:        span,
		startedAt:   time.Now(),
		metricAttrs: getMetricAttrs(ctx),
	}
}

// getLogger 返回绑定本操作上下文的日志对象。
func (o recordedOperation) getLogger() *slog.Logger {
	// 1. 使用操作 ctx 获取带身份和 span 的 Logger。
	return o.telemetry.GetLogger(o.ctx)
}

// endSpan 记录错误分类并结束 span，返回耗时。
func (o recordedOperation) endSpan(errorType, description string) time.Duration {
	// 1. 非空分类写 error.type，真正失败时设置 Error 状态和补充错误说明；取消只写分类。
	if errorType != "" {
		o.span.SetAttributes(semconv.ErrorTypeKey.String(errorType))
	}
	if isFailure(errorType) {
		o.span.SetStatus(codes.Error, description)
	}
	// 2. 结束 span 并返回耗时，不另记异常事件。
	o.span.End()
	return time.Since(o.startedAt)
}

// isContentCaptured 本操作是否采集内容？
func (o recordedOperation) isContentCaptured() bool {
	// 1. 同时检查管道已开启内容采集和本操作的 span 正在记录。
	return o.telemetry.captureContent && o.span.IsRecording()
}

// setContent 写入内容属性及截断标记。
func (o recordedOperation) setContent(key attribute.Key, value attribute.Value, truncated bool) {
	// 1. 将已按 64 KiB 裁剪的结构化值写为紧凑 JSON 文本，发生截断时加 openvaala.content.truncated=true。
	o.span.SetAttributes(key.String(value.String()))
	if truncated {
		o.span.SetAttributes(contentTruncatedKey.Bool(true))
	}
}

// ── RPC 请求 ──

// RPCServerRecorder 记录 Runtime Server 处理的一次 JSON-RPC 请求；由处理这个请求的 goroutine 单独使用，End 恰好调用一次，End 之后不得再使用。
type RPCServerRecorder struct {
	operation recordedOperation
	method    string
}

// StartRPCServer 开始记录一次 JSON-RPC 请求。
func StartRPCServer(ctx context.Context, method, originalMethod, requestID string) (context.Context, *RPCServerRecorder) {
	// 1. 记录方法名；未注册方法使用 _OTHER 并保留原方法，span 名使用 jsonrpc，非空请求 ID 另写属性。
	spanName := method
	attrs := []attribute.KeyValue{semconv.RPCSystemNameJSONRPC, semconv.RPCMethodKey.String(method)}
	if method == RPCMethodOther {
		spanName = rpcSystemJSONRPC
		attrs = append(attrs, semconv.RPCMethodOriginal(originalMethod))
	}
	if requestID != "" {
		attrs = append(attrs, semconv.JSONRPCRequestID(requestID))
	}
	// 2. 创建服务端 span 并返回请求记录器及其上下文。
	ctx, operation := startOperation(ctx, spanName, trace.SpanKindServer, nil, attrs)
	return ctx, &RPCServerRecorder{
		operation: operation,
		method:    method,
	}
}

// End 结束 RPC 请求记录。
func (r *RPCServerRecorder) End(errorCode int, err error) {
	// 1. 错误码非零时记响应状态码，再计算错误分类。
	metricAttrs := []attribute.KeyValue{semconv.RPCMethodKey.String(r.method)}
	statusCode := ""
	if errorCode != 0 {
		statusCode = strconv.Itoa(errorCode)
		r.operation.span.SetAttributes(semconv.RPCResponseStatusCode(statusCode))
		metricAttrs = append(metricAttrs, semconv.RPCResponseStatusCode(statusCode))
	}
	errorType := getErrorType(err, statusCode)
	if errorType != "" {
		metricAttrs = append(metricAttrs, semconv.ErrorTypeKey.String(errorType))
	}
	// 2. 结束 span 并记录 RPC 耗时，失败写 Error，成功或取消写 Debug，取消另附原因。
	duration := r.operation.endSpan(errorType, getErrorDescription(err))
	r.operation.telemetry.instruments.rpcDuration.Record(r.operation.ctx, duration.Seconds(), rpcconv.SystemNameJSONRPC, metricAttrs...)
	logAttrs := []any{string(semconv.RPCMethodKey), r.method, "duration_ms", duration.Milliseconds()}
	if isFailure(errorType) {
		r.operation.getLogger().Error("rpc request failed",
			append(logAttrs, string(semconv.RPCResponseStatusCodeKey), statusCode, "error", err)...)
		return
	}
	r.operation.getLogger().Debug("rpc request handled", append(logAttrs, getCanceledLogAttrs(errorType, err)...)...)
}

// ── Agent Turn ──

// InvokeAgentRecorder 记录一个 Agent Turn；由执行这个 Turn 的 goroutine 单独使用，End 恰好调用一次，End 之后不得再使用。
type InvokeAgentRecorder struct {
	operation recordedOperation
	start     InvokeAgentStart
}

// StartInvokeAgent 开始记录一个 Agent Turn。
func StartInvokeAgent(ctx context.Context, start InvokeAgentStart) (context.Context, *InvokeAgentRecorder) {
	// 1. 写入 Agent 名、非空说明和主模型。
	attrs := []attribute.KeyValue{genAIOperationNameKey.String(genAIOperationInvokeAgent), agentNameKey.String(start.AgentName)}
	if start.AgentDescription != "" {
		attrs = append(attrs, genAIAgentDescriptionKey.String(start.AgentDescription))
	}
	if start.Model != "" {
		attrs = append(attrs, genAIRequestModelKey.String(start.Model))
	}
	// 2. 将 Turn ID 挂入 ctx 并创建 invoke_agent span，写开始日志，返回记录器。
	ctx, operation := startOperation(ctx, genAIOperationInvokeAgent+" "+start.AgentName, trace.SpanKindInternal,
		[]attribute.KeyValue{turnIDKey.String(start.TurnID)}, attrs)
	operation.getLogger().Debug("turn started")
	return ctx, &InvokeAgentRecorder{
		operation: operation,
		start:     start,
	}
}

// IsContentCaptured 本轮是否采集输入和回答内容？
func (r *InvokeAgentRecorder) IsContentCaptured() bool {
	// 1. 检查本操作的内容开关和 span 记录状态。
	return r.operation.isContentCaptured()
}

// RecordInput 记录开启本轮的输入消息。
func (r *InvokeAgentRecorder) RecordInput(messages []ChatMessage) {
	// 1. 内容关闭或消息为空时跳过，否则裁剪并写入输入消息。
	if !r.IsContentCaptured() || len(messages) == 0 {
		return
	}
	value, truncated := newMessagesValue(messages)
	r.operation.setContent(genAIInputMessagesKey, value, truncated)
}

// End 结束 Turn 记录并写入小结、指标和日志。
func (r *InvokeAgentRecorder) End(end InvokeAgentEnd) {
	// 1. 记录终态、步骤数、工具调用数和 token 合计，并填入结束原因。
	errorType := getErrorType(end.Err, "")
	summary := end.Summary
	r.operation.span.SetAttributes(
		turnStatusKey.String(end.Status),
		turnStepCountKey.Int(summary.Steps),
		turnToolCallsKey.Int(summary.ToolCalls),
		turnFailedToolCallsKey.Int(summary.FailedToolCalls),
		genAIUsageInputTokensKey.Int(summary.Usage.Input),
		genAIUsageOutputTokensKey.Int(summary.Usage.Output),
	)
	if reasons := getFinishReasons(summary.FinishReason, errorType); reasons != nil {
		r.operation.span.SetAttributes(genAIResponseFinishReasonsKey.StringSlice(reasons))
	}
	// 2. 开启内容采集且有最后回复时写入输出消息。
	if r.IsContentCaptured() && len(summary.Output) > 0 {
		value, truncated := newMessagesValue(summary.Output)
		r.operation.setContent(genAIOutputMessagesKey, value, truncated)
	}
	// 3. 结束 span 并记录耗时和调用次数，失败写 Error，正常或取消写 Info，取消附原因。
	duration := r.operation.endSpan(errorType, getErrorDescription(end.Err))
	r.recordMetrics(duration, errorType, end.Status, summary)
	logAttrs := []any{string(turnStatusKey), end.Status, "duration_ms", duration.Milliseconds()}
	if isFailure(errorType) {
		r.operation.getLogger().Error("turn failed", append(logAttrs, "error", end.Err)...)
		return
	}
	r.operation.getLogger().Info("turn finished", append(logAttrs, getCanceledLogAttrs(errorType, end.Err)...)...)
}

// recordMetrics 记录 Turn 耗时和调用次数。
func (r *InvokeAgentRecorder) recordMetrics(duration time.Duration, errorType, status string, summary TurnSummary) {
	// 1. 组装 Agent 及执行环境维度，耗时另带终态、主模型和错误分类。
	ctx := r.operation.ctx
	instruments := r.operation.telemetry.instruments
	agentAttrs := slices.Concat(r.operation.metricAttrs, []attribute.KeyValue{agentNameKey.String(r.start.AgentName)})
	durationAttrs := append(slices.Clip(agentAttrs), turnStatusKey.String(status))
	if r.start.Model != "" {
		durationAttrs = append(durationAttrs, genAIRequestModelKey.String(r.start.Model))
	}
	if errorType != "" {
		durationAttrs = append(durationAttrs, semconv.ErrorTypeKey.String(errorType))
	}
	// 2. 记录耗时、模型调用次数和收到结果的工具调用次数。
	instruments.invokeAgentDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(durationAttrs...))
	instruments.invokeAgentInferenceCalls.Record(ctx, int64(summary.InferenceCalls), metric.WithAttributes(agentAttrs...))
	instruments.invokeAgentToolCalls.Record(ctx, int64(summary.ToolCalls), metric.WithAttributes(agentAttrs...))
}

// ── 模型调用 ──

// ChatRecorder 记录一次模型调用，用量、结束原因、上游模型与输出分块在流式事件中陆续交给它；由执行这次调用的 goroutine 单独使用，
// End 恰好调用一次，End 之后不得再使用。
type ChatRecorder struct {
	operation     recordedOperation
	start         ChatStart
	usage         *defs.TokenUsage
	finishReason  string
	responseModel string
	// firstChunkAt 与 lastChunkAt 是收到首个与最近一个输出分块的时间；chunkIntervals 是第二个起每个分块与上一个的间隔（秒）。
	firstChunkAt   time.Time
	lastChunkAt    time.Time
	chunkIntervals []float64
}

// StartChat 开始记录一次流式模型调用。
func StartChat(ctx context.Context, start ChatStart) (context.Context, *ChatRecorder) {
	// 1. 记录操作名、协议家族、请求模型、流式标记、用途、服务地址和压缩标记。
	attrs := []attribute.KeyValue{
		genAIOperationNameKey.String(genAIOperationChat),
		genAIProviderNameKey.String(start.Provider),
		genAIRequestModelKey.String(start.Model),
		genAIRequestStreamKey.Bool(true),
		modelCallPurposeKey.String(start.Purpose),
	}
	attrs = append(attrs, getServerAttrs(start.ServerAddress, start.ServerPort)...)
	if start.Compacted {
		attrs = append(attrs, genAIConversationCompactedKey.Bool(true))
	}
	// 2. 挂入模型调用 ID 并创建客户端 chat span，返回记录器。
	ctx, operation := startOperation(ctx, genAIOperationChat+" "+start.Model, trace.SpanKindClient,
		[]attribute.KeyValue{modelCallIDKey.String(start.ModelCallID)}, attrs)
	return ctx, &ChatRecorder{
		operation: operation,
		start:     start,
	}
}

// RecordUsage 记录模型 token 用量。
func (r *ChatRecorder) RecordUsage(usage defs.TokenUsage) {
	// 1. 用本次上报的用量替换此前记录，保留最后一次上报值。
	r.usage = &usage
}

// RecordFinishReason 记录模型结束原因。
func (r *ChatRecorder) RecordFinishReason(reason string) {
	// 1. 保存调用方已归一的结束原因。
	r.finishReason = reason
}

// RecordResponseModel 记录上游模型名。
func (r *ChatRecorder) RecordResponseModel(model string) {
	// 1. 此前模型名为空时采用本次值，首个非空值写入后保持不变。
	if r.responseModel == "" {
		r.responseModel = model
	}
}

// RecordChunk 记录一个输出分块的到达时间。
func (r *ChatRecorder) RecordChunk() {
	// 1. 首块记录首 token 时间，后续块追加与上块的秒数间隔，再更新最近分块时间。
	now := time.Now()
	if r.firstChunkAt.IsZero() {
		r.firstChunkAt = now
	} else {
		r.chunkIntervals = append(r.chunkIntervals, now.Sub(r.lastChunkAt).Seconds())
	}
	r.lastChunkAt = now
}

// IsContentCaptured 本次模型调用是否采集内容？
func (r *ChatRecorder) IsContentCaptured() bool {
	// 1. 检查本操作的内容开关和 span 记录状态。
	return r.operation.isContentCaptured()
}

// RecordInput 记录模型请求的系统指令、消息和工具名称。
func (r *ChatRecorder) RecordInput(systemInstructions []string, messages []ChatMessage, toolNames []string) {
	// 1. 内容采集关闭时跳过。
	if !r.IsContentCaptured() {
		return
	}
	// 2. 分别写入非空系统指令、按发送顺序的输入消息和工具名目录，超限内容裁剪。
	if len(systemInstructions) > 0 {
		value, truncated := newTextPartsValue(systemInstructions)
		r.operation.setContent(genAISystemInstructionsKey, value, truncated)
	}
	if len(messages) > 0 {
		value, truncated := newMessagesValue(messages)
		r.operation.setContent(genAIInputMessagesKey, value, truncated)
	}
	if len(toolNames) > 0 {
		r.operation.setContent(genAIToolDefinitionsKey, newToolDefinitionsValue(toolNames), false)
	}
}

// RecordOutput 记录模型调用已收到的输出消息。
func (r *ChatRecorder) RecordOutput(messages []ChatMessage) {
	// 1. 内容关闭或消息为空时跳过，否则裁剪后写入；失败调用只记录传入的部分输出。
	if !r.IsContentCaptured() || len(messages) == 0 {
		return
	}
	value, truncated := newMessagesValue(messages)
	r.operation.setContent(genAIOutputMessagesKey, value, truncated)
}

// End 结束模型调用记录并写入用量、指标和日志。
func (r *ChatRecorder) End(err error) {
	// 1. 调用出错且开始时的 ctx 已取消时合并取消错误，再计算分类。
	if err != nil && r.operation.ctx.Err() != nil {
		err = errors.Join(r.operation.ctx.Err(), err)
	}
	errorType := getErrorType(err, "")
	span := r.operation.span
	logAttrs := []any{string(genAIRequestModelKey), r.start.Model, string(modelCallPurposeKey), r.start.Purpose}
	// 2. 记录结束原因、上游模型、首 token 延迟和已收到的用量。
	if reasons := getFinishReasons(r.finishReason, errorType); reasons != nil {
		span.SetAttributes(genAIResponseFinishReasonsKey.StringSlice(reasons))
		logAttrs = append(logAttrs, string(genAIResponseFinishReasonsKey), reasons[0])
	}
	if r.responseModel != "" {
		span.SetAttributes(genAIResponseModelKey.String(r.responseModel))
	}
	if !r.firstChunkAt.IsZero() {
		span.SetAttributes(genAIResponseTimeToFirstChunkKey.Float64(r.firstChunkAt.Sub(r.operation.startedAt).Seconds()))
	}
	if r.usage != nil {
		usageAttrs := getUsageAttrs(*r.usage)
		span.SetAttributes(usageAttrs...)
		for _, attr := range usageAttrs {
			logAttrs = append(logAttrs, string(attr.Key), attr.Value.AsInt64())
		}
	}
	// 3. 结束 span 并记录指标；失败写 Warn，正常或取消写 Info，取消附原因。
	duration := r.operation.endSpan(errorType, getErrorDescription(err))
	r.recordMetrics(duration, errorType)
	logAttrs = append(logAttrs, "duration_ms", duration.Milliseconds())
	if isFailure(errorType) {
		r.operation.getLogger().Warn("model call failed", append(logAttrs, "error", err)...)
		return
	}
	r.operation.getLogger().Info("model call finished", append(logAttrs, getCanceledLogAttrs(errorType, err)...)...)
}

// recordMetrics 记录模型调用耗时、分块间隔和 token 用量。
func (r *ChatRecorder) recordMetrics(duration time.Duration, errorType string) {
	// 1. 组装模型、用途、执行环境和服务地址维度，失败或取消时给耗时另加错误分类。
	ctx := r.operation.ctx
	instruments := r.operation.telemetry.instruments
	attrs := slices.Concat(r.operation.metricAttrs, []attribute.KeyValue{
		genAIOperationNameKey.String(genAIOperationChat),
		genAIProviderNameKey.String(r.start.Provider),
		genAIRequestModelKey.String(r.start.Model),
		modelCallPurposeKey.String(r.start.Purpose),
	}, getServerAttrs(r.start.ServerAddress, r.start.ServerPort))
	if r.responseModel != "" {
		attrs = append(attrs, genAIResponseModelKey.String(r.responseModel))
	}
	errorAttrs := attrs
	if errorType != "" {
		errorAttrs = append(slices.Clip(attrs), semconv.ErrorTypeKey.String(errorType))
	}
	withAttrs := metric.WithAttributes(attrs...)
	withErrorAttrs := metric.WithAttributes(errorAttrs...)
	// 2. 记录耗时、已收到首块的延迟和后续分块间隔。
	instruments.inferenceDuration.Record(ctx, duration.Seconds(), withErrorAttrs)
	if !r.firstChunkAt.IsZero() {
		instruments.inferenceTimeToFirstChunk.Record(ctx, r.firstChunkAt.Sub(r.operation.startedAt).Seconds(), withAttrs)
	}
	for _, interval := range r.chunkIntervals {
		instruments.inferenceTimePerOutputChunk.Record(ctx, interval, withAttrs)
	}
	// 3. 有用量时采用上报值，否则用量记 0 且模态为 unknown；缓存与推理计数只写非零值。
	usage, modality := defs.TokenUsage{}, TokenModalityUnknown
	if r.usage != nil {
		usage = *r.usage
		if r.start.TokenModality != "" {
			modality = r.start.TokenModality
		}
	}
	tokenAttrs := metric.WithAttributes(append(slices.Clip(attrs), genAITokenModalityKey.String(modality))...)
	instruments.inferenceInputTokens.Add(ctx, int64(usage.Input), tokenAttrs)
	instruments.inferenceOutputTokens.Add(ctx, int64(usage.Output), tokenAttrs)
	if usage.CacheRead > 0 {
		instruments.inferenceCacheReadTokens.Add(ctx, int64(usage.CacheRead), tokenAttrs)
	}
	if usage.CacheWrite > 0 {
		instruments.inferenceCacheWriteTokens.Add(ctx, int64(usage.CacheWrite), tokenAttrs)
	}
	if usage.Reasoning > 0 {
		instruments.inferenceReasoningTokens.Add(ctx, int64(usage.Reasoning), tokenAttrs)
	}
	// 4. 记录单次输入和输出 token 数直方图，附带错误分类。
	instruments.inferenceOperationInputTokens.Record(ctx, int64(usage.Input), withErrorAttrs)
	instruments.inferenceOperationOutputTokens.Record(ctx, int64(usage.Output), withErrorAttrs)
}

// ── 工具调用 ──

// ExecuteToolRecorder 记录一次实际执行的工具调用；由执行这次调用的 goroutine 单独使用，End 恰好调用一次，End 之后不得再使用。
type ExecuteToolRecorder struct {
	operation recordedOperation
	toolName  string
}

// StartExecuteTool 开始记录一次实际执行的工具调用。
func StartExecuteTool(ctx context.Context, start ExecuteToolStart) (context.Context, *ExecuteToolRecorder) {
	// 1. 填入工具名、function 类型和非空用途说明。
	attrs := []attribute.KeyValue{
		genAIOperationNameKey.String(genAIOperationExecuteTool),
		genAIToolNameKey.String(start.Name),
		genAIToolTypeKey.String(genAIToolTypeFunction),
	}
	if start.Description != "" {
		attrs = append(attrs, genAIToolDescriptionKey.String(start.Description))
	}
	// 2. 挂入 CallID，创建 execute_tool span 并返回记录器。
	ctx, operation := startOperation(ctx, genAIOperationExecuteTool+" "+start.Name, trace.SpanKindInternal,
		[]attribute.KeyValue{toolCallIDKey.String(start.CallID)}, attrs)
	return ctx, &ExecuteToolRecorder{
		operation: operation,
		toolName:  start.Name,
	}
}

// IsContentCaptured 本次工具调用是否采集参数和结果？
func (r *ExecuteToolRecorder) IsContentCaptured() bool {
	// 1. 检查本操作的内容开关和 span 记录状态。
	return r.operation.isContentCaptured()
}

// RecordArguments 记录工具调用参数。
func (r *ExecuteToolRecorder) RecordArguments(arguments any) {
	// 1. 内容关闭或参数为 nil 时跳过，否则编码、裁剪并写入参数文本。
	if !r.IsContentCaptured() || arguments == nil {
		return
	}
	value, truncated := newJSONContentValue(arguments)
	r.operation.setContent(genAIToolCallArgumentsKey, value, truncated)
}

// RecordResult 记录工具调用结果。
func (r *ExecuteToolRecorder) RecordResult(result any) {
	// 1. 内容关闭或结果为 nil 时跳过，否则编码、裁剪并写入结果文本。
	if !r.IsContentCaptured() || result == nil {
		return
	}
	value, truncated := newJSONContentValue(result)
	r.operation.setContent(genAIToolCallResultKey, value, truncated)
}

// End 结束工具调用记录并写入指标和日志。
func (r *ExecuteToolRecorder) End(errorCode string) {
	// 1. 空错误码视为成功，非空写 error.type，span 状态说明留空。
	errorType := getErrorType(nil, errorCode)
	duration := r.operation.endSpan(errorType, "")
	// 2. 记录带工具和执行环境维度的耗时，再写 Info 结束日志。
	metricAttrs := slices.Concat(r.operation.metricAttrs, []attribute.KeyValue{
		genAIToolNameKey.String(r.toolName),
		genAIToolTypeKey.String(genAIToolTypeFunction),
	})
	if errorType != "" {
		metricAttrs = append(metricAttrs, semconv.ErrorTypeKey.String(errorType))
	}
	r.operation.telemetry.instruments.executeToolDuration.Record(r.operation.ctx, duration.Seconds(), metric.WithAttributes(metricAttrs...))
	logAttrs := []any{string(genAIToolNameKey), r.toolName, "duration_ms", duration.Milliseconds()}
	if errorType != "" {
		logAttrs = append(logAttrs, string(semconv.ErrorTypeKey), errorType)
	}
	r.operation.getLogger().Info("tool call finished", logAttrs...)
}

// ── 常驻计数 ──

// AddActiveConversations 调整运行中的对话数。
func AddActiveConversations(ctx context.Context, delta int64) {
	// 1. Runtime 开始运行对话时加 1，退出时减 1。
	GetTelemetry(ctx).instruments.activeConversations.Add(ctx, delta)
}

// AddActiveConnections 调整已建立的服务端连接数。
func AddActiveConnections(ctx context.Context, delta int64) {
	// 1. 连接建立时加 1，关闭时减 1。
	GetTelemetry(ctx).instruments.activeConnections.Add(ctx, delta)
}
