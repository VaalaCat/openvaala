package telemetry

import (
	"github.com/VaalaCat/openvaala/pkg/defs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/semconv/v1.43.0/rpcconv"
)

// ScopeName 是全项目 Tracer、Meter 与 OTLP 日志桥接共用的 instrumentation scope。
const ScopeName = "github.com/VaalaCat/openvaala"

// ── GenAI 属性约定：属性键 ──

// GenAI 约定已迁出核心仓库，Go semconv 自 v1.42 起不再提供 GenAI 常量；本组与下面两组是全项目唯一定义 gen_ai.* 名称的位置。
// 来源：open-telemetry/semantic-conventions-genai main@5293effb（2026-10-12，schema gen-ai-dev/1.42.0-dev，Development）。
const (
	genAIOperationNameKey            = attribute.Key("gen_ai.operation.name")
	genAIProviderNameKey             = attribute.Key("gen_ai.provider.name")
	genAIAgentNameKey                = attribute.Key("gen_ai.agent.name")
	genAIAgentDescriptionKey         = attribute.Key("gen_ai.agent.description")
	genAIConversationIDKey           = attribute.Key("gen_ai.conversation.id")
	genAIConversationCompactedKey    = attribute.Key("gen_ai.conversation.compacted")
	genAIRequestModelKey             = attribute.Key("gen_ai.request.model")
	genAIRequestStreamKey            = attribute.Key("gen_ai.request.stream")
	genAIResponseModelKey            = attribute.Key("gen_ai.response.model")
	genAIResponseFinishReasonsKey    = attribute.Key("gen_ai.response.finish_reasons")
	genAIResponseTimeToFirstChunkKey = attribute.Key("gen_ai.response.time_to_first_chunk")
	genAIUsageInputTokensKey         = attribute.Key("gen_ai.usage.input_tokens")
	genAIUsageOutputTokensKey        = attribute.Key("gen_ai.usage.output_tokens")
	genAIUsageCacheReadTokensKey     = attribute.Key("gen_ai.usage.cache_read.input_tokens")
	genAIUsageCacheWriteTokensKey    = attribute.Key("gen_ai.usage.cache_write.input_tokens")
	genAIUsageReasoningTokensKey     = attribute.Key("gen_ai.usage.reasoning.output_tokens")
	genAITokenModalityKey            = attribute.Key("gen_ai.token.modality")
	genAIToolNameKey                 = attribute.Key("gen_ai.tool.name")
	genAIToolTypeKey                 = attribute.Key("gen_ai.tool.type")
	genAIToolDescriptionKey          = attribute.Key("gen_ai.tool.description")
	genAIToolCallIDKey               = attribute.Key("gen_ai.tool.call.id")
	genAIToolCallArgumentsKey        = attribute.Key("gen_ai.tool.call.arguments")
	genAIToolCallResultKey           = attribute.Key("gen_ai.tool.call.result")
	genAIToolDefinitionsKey          = attribute.Key("gen_ai.tool.definitions")
	genAISystemInstructionsKey       = attribute.Key("gen_ai.system_instructions")
	genAIInputMessagesKey            = attribute.Key("gen_ai.input.messages")
	genAIOutputMessagesKey           = attribute.Key("gen_ai.output.messages")
)

// ── GenAI 属性约定：取值 ──

// GenAI 操作名与工具类型取值。
const (
	genAIOperationChat        = "chat"
	genAIOperationInvokeAgent = "invoke_agent"
	genAIOperationExecuteTool = "execute_tool"
	// genAIToolTypeFunction 表示由客户端执行的工具；OpenVaala 的工具都由本进程执行。
	genAIToolTypeFunction = "function"
)

// 结束原因的约定取值（schema FinishReason 枚举），业务包归一结束原因时使用。
const (
	FinishReasonStop          = "stop"
	FinishReasonLength        = "length"
	FinishReasonContentFilter = "content_filter"
	FinishReasonToolCall      = "tool_call"
	// FinishReasonError 表示应收到结束原因却没有收到：调用失败、取消或流中断。
	FinishReasonError = "error"
)

// token 计数器的 gen_ai.token.modality 取值：llmkit 不提供按模态拆分的用量，只区分纯文本调用与其他。
const (
	TokenModalityText    = "text"
	TokenModalityUnknown = "unknown"
)

// 消息角色、part 类型与模态的约定取值，供业务包构造 ChatMessage。系统指令单独记录，没有 system 角色。
const (
	MessageRoleUser      = "user"
	MessageRoleAssistant = "assistant"
	MessageRoleTool      = "tool"

	PartTypeText             = "text"
	PartTypeReasoning        = "reasoning"
	PartTypeToolCall         = "tool_call"
	PartTypeToolCallResponse = "tool_call_response"
	PartTypeURI              = "uri"
	PartTypeBlob             = "blob"

	ModalityImage = "image"
)

// ── GenAI 属性约定：指标名与分桶 ──

const (
	inferenceDurationName              = "gen_ai.client.inference.duration"
	inferenceTimeToFirstChunkName      = "gen_ai.client.inference.time_to_first_chunk"
	inferenceTimePerOutputChunkName    = "gen_ai.client.inference.time_per_output_chunk"
	inferenceInputTokensName           = "gen_ai.client.inference.usage.input_tokens"
	inferenceOutputTokensName          = "gen_ai.client.inference.usage.output_tokens"
	inferenceCacheReadTokensName       = "gen_ai.client.inference.usage.cache_read.input_tokens"
	inferenceCacheWriteTokensName      = "gen_ai.client.inference.usage.cache_write.input_tokens"
	inferenceReasoningTokensName       = "gen_ai.client.inference.usage.reasoning.output_tokens"
	inferenceOperationInputTokensName  = "gen_ai.client.inference.operation.input_tokens"
	inferenceOperationOutputTokensName = "gen_ai.client.inference.operation.output_tokens"
	invokeAgentDurationName            = "gen_ai.invoke_agent.duration"
	invokeAgentInferenceCallsName      = "gen_ai.invoke_agent.inference_calls"
	invokeAgentToolCallsName           = "gen_ai.invoke_agent.tool_calls"
	executeToolDurationName            = "gen_ai.execute_tool.duration"
)

// 以下耗时直方图使用显式的秒数分桶。
var (
	// inferenceDurationBuckets 是约定对推理耗时、首 token 延迟、分块间隔的建议分桶。
	inferenceDurationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	// tokenCountBuckets 是约定对单次调用 token 数的建议分桶。
	tokenCountBuckets = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	// invokeAgentDurationBuckets 是约定建议值 0.1…409.6 继续倍增到约一小时，覆盖等待子 Agent 的长 Turn。
	invokeAgentDurationBuckets = []float64{0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8, 25.6, 51.2, 102.4, 204.8, 409.6, 819.2, 1638.4, 3276.8}
	// executeToolDurationBuckets 是约定建议值 0.01…81.92 继续倍增到约 45 分钟，覆盖长时间运行的命令。
	executeToolDurationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
		163.84, 327.68, 655.36, 1310.72, 2621.44}
	// callCountBuckets 是约定对每个 Turn 模型调用次数与工具调用次数的建议分桶。
	callCountBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128}
)

// ── 身份属性键 ──

// 实体身份属性键：OTel 已定义的名称直接使用，其余以 openvaala. 为前缀；span 和日志共用。
const (
	conversationIDKey  = genAIConversationIDKey
	sessionIDKey       = attribute.Key("openvaala.session.id")
	parentSessionIDKey = attribute.Key("openvaala.parent_session.id")
	agentNameKey       = genAIAgentNameKey
	turnIDKey          = attribute.Key("openvaala.turn.id")
	turnStepKey        = attribute.Key("openvaala.turn.step")
	inputItemIDKey     = attribute.Key("openvaala.input.item_id")
	modelCallIDKey     = attribute.Key("openvaala.model_call.id")
	toolCallIDKey      = genAIToolCallIDKey
	executorIDKey      = attribute.Key("openvaala.executor.id")
	connectionIDKey    = attribute.Key("openvaala.connection.id")
)

// ── 身份属性构造 ──

// conversationIDAttr 生成对话身份属性。
func conversationIDAttr(id string) attribute.KeyValue {
	// 1. 将对话 ID 写入身份属性。
	return conversationIDKey.String(id)
}

// sessionIDAttr 生成 Session 身份属性。
func sessionIDAttr(id string) attribute.KeyValue {
	// 1. 将 Session ID 写入身份属性。
	return sessionIDKey.String(id)
}

// parentSessionIDAttr 生成父 Session 身份属性。
func parentSessionIDAttr(id string) attribute.KeyValue {
	// 1. 将父 Session ID 写入身份属性。
	return parentSessionIDKey.String(id)
}

// agentNameAttr 生成 Agent 名属性。
func agentNameAttr(name string) attribute.KeyValue {
	// 1. 将 Agent 名写入属性，也用作指标维度。
	return agentNameKey.String(name)
}

// TurnStep 生成 Turn 内的步骤序号属性。
func TurnStep(step int) attribute.KeyValue {
	// 1. 写入从 1 开始的序号，一步包含一次普通模型调用及其工具调用。
	return turnStepKey.Int(step)
}

// InputItemID 生成开启本轮的输入 Item 属性。
func InputItemID(id string) attribute.KeyValue {
	// 1. 写入输入 Item ID。
	return inputItemIDKey.String(id)
}

// ExecutorID 生成工具执行环境的身份属性。
func ExecutorID(id string) attribute.KeyValue {
	// 1. 写入 Executor ID，也用作指标维度。
	return executorIDKey.String(id)
}

// ConnectionID 生成 WebSocket 连接身份属性。
func ConnectionID(id string) attribute.KeyValue {
	// 1. 写入服务端为连接生成的 ID。
	return connectionIDKey.String(id)
}

// ToolCallID 生成工具调用身份属性。
func ToolCallID(id string) attribute.KeyValue {
	// 1. 将工具 CallID 写入属性。
	return toolCallIDKey.String(id)
}

// ToolName 生成工具名称属性。
func ToolName(name string) attribute.KeyValue {
	// 1. 将工具名称写入属性，供未执行调用的日志使用。
	return genAIToolNameKey.String(name)
}

// ServerAddress 生成服务监听地址属性。
func ServerAddress(host string) attribute.KeyValue {
	// 1. 将监听主机写入属性，供服务端 ctx 和 RPC span 使用。
	return semconv.ServerAddress(host)
}

// ServerPort 生成服务监听端口属性。
func ServerPort(port int) attribute.KeyValue {
	// 1. 将监听端口写入属性。
	return semconv.ServerPort(port)
}

// NetworkPeerAddress 生成 WebSocket 连接的 TCP 对端地址属性。
func NetworkPeerAddress(host string) attribute.KeyValue {
	// 1. 写入 TCP 对端主机，不采用 X-Forwarded-For。
	return semconv.NetworkPeerAddress(host)
}

// NetworkPeerPort 生成 WebSocket 连接的 TCP 对端端口属性。
func NetworkPeerPort(port int) attribute.KeyValue {
	// 1. 写入 TCP 对端端口。
	return semconv.NetworkPeerPort(port)
}

// ── OpenVaala 自定义属性与取值 ──

// 约定没有对应物的属性：写入 span 与结束日志，不挂到 ctx；其中 Turn 终态与模型调用用途同时是指标维度。
const (
	modelCallPurposeKey    = attribute.Key("openvaala.model_call.purpose")
	turnStatusKey          = attribute.Key("openvaala.turn.status")
	turnStepCountKey       = attribute.Key("openvaala.turn.step_count")
	turnToolCallsKey       = attribute.Key("openvaala.turn.tool_calls")
	turnFailedToolCallsKey = attribute.Key("openvaala.turn.failed_tool_calls")
	contentTruncatedKey    = attribute.Key("openvaala.content.truncated")
)

// RPCMethodOther 是未注册方法的 rpc.method 取值；此时 span 名使用 rpc.system.name，原始方法名记在 rpc.method_original。
const RPCMethodOther = "_OTHER"

const (
	// rpcSystemJSONRPC 是 Runtime Server 协议的 rpc.system.name 取值，也是未注册方法的 span 名。
	rpcSystemJSONRPC = "jsonrpc"
	// errorTypeCanceled 是取消的 error.type；约定对取消没有规定，OpenVaala 记为 canceled 且不算失败。
	errorTypeCanceled = "canceled"
)

// 约定没有对应物的常驻计数；按命名规则使用单数名。
const (
	conversationActiveName = "openvaala.conversation.active"
	connectionActiveName   = "openvaala.server.connection.active"
)

// ── 指标仪表 ──

// telemetryInstruments 是按全项目统一命名创建好的指标仪表；RPC 耗时用 semconv 构造函数创建，其余按本文件定义的指标名常量手动创建。
type telemetryInstruments struct {
	inferenceDuration              metric.Float64Histogram
	inferenceTimeToFirstChunk      metric.Float64Histogram
	inferenceTimePerOutputChunk    metric.Float64Histogram
	inferenceInputTokens           metric.Int64Counter
	inferenceOutputTokens          metric.Int64Counter
	inferenceCacheReadTokens       metric.Int64Counter
	inferenceCacheWriteTokens      metric.Int64Counter
	inferenceReasoningTokens       metric.Int64Counter
	inferenceOperationInputTokens  metric.Int64Histogram
	inferenceOperationOutputTokens metric.Int64Histogram
	invokeAgentDuration            metric.Float64Histogram
	invokeAgentInferenceCalls      metric.Int64Histogram
	invokeAgentToolCalls           metric.Int64Histogram
	executeToolDuration            metric.Float64Histogram
	rpcDuration                    rpcconv.ServerCallDuration
	activeConversations            metric.Int64UpDownCounter
	activeConnections              metric.Int64UpDownCounter
}

// ── 模型调用入参 ──

// 模型调用的用途：普通对话或生成上下文摘要。
const (
	ChatPurposeConversation = "conversation"
	ChatPurposeCompact      = "compact"
)

// ChatStart 是开始记录一次模型调用所需的事实，由 Agent 与压缩在调用模型前填写。
type ChatStart struct {
	ModelCallID string
	Model       string
	// Provider 是 gen_ai.provider.name；经网关或兼容接口访问时表示协议家族而非实际厂商。
	Provider string
	// Purpose 是 ChatPurposeConversation 或 ChatPurposeCompact。
	Purpose string
	// ServerAddress 与 ServerPort 是模型服务的主机与端口；地址为空时两者都不写。
	ServerAddress string
	ServerPort    int
	// Compacted 本次请求的上下文是否以摘要开头？是时记 gen_ai.conversation.compacted=true。
	Compacted bool
	// TokenModality 是 token 计数器的模态：TokenModalityText 或 TokenModalityUnknown；空值按 unknown 记录。
	TokenModality string
}

// ── Turn 入参与小结 ──

// InvokeAgentStart 是开始记录一个 Turn 所需的事实，由 Turn 在运行 Agent 前填写。
type InvokeAgentStart struct {
	AgentName        string
	AgentDescription string
	// Model 是 Agent 配置的主模型。
	Model  string
	TurnID string
}

// InvokeAgentEnd 是 Turn 结束时的事实：Status 是 Turn 终态，Err 非 nil 表示失败（取消按取消记录）。
type InvokeAgentEnd struct {
	Status  string
	Err     error
	Summary TurnSummary
}

// TurnSummary 是一个 Turn 的小结，由 Agent 在自己的循环中统计、经 Turn 交给 Recorder。
type TurnSummary struct {
	// Steps 是普通模型调用次数。
	Steps int
	// InferenceCalls 是全部模型调用次数，含压缩调用。
	InferenceCalls int
	// ToolCalls 是收到结果的工具调用次数，FailedToolCalls 是其中带错误的次数。
	ToolCalls       int
	FailedToolCalls int
	// Usage 是全部模型调用的 token 合计。
	Usage defs.TokenUsage
	// FinishReason 是最后一次普通模型调用的结束原因（已归一）。
	FinishReason string
	// Output 是本轮最后一条助手消息；内容采集关闭时为空。
	Output []ChatMessage
}

// ── 工具调用入参 ──

// ExecuteToolStart 是开始记录一次工具调用所需的事实，由 ToolExecutor 在执行前填写。
type ExecuteToolStart struct {
	Name   string
	CallID string
	// Description 是工具定义中的描述；为空时不写。
	Description string
}

// ── 对话内容 ──

// ChatMessage 是一条 GenAI 约定格式的消息，与业务类型无关；由业务包从模型请求或 Item 转换而来。
type ChatMessage struct {
	Role  string
	Parts []MessagePart
}

// MessagePart 是消息中的一段内容；Type 决定使用哪些字段，空字段不写出。
type MessagePart struct {
	Type    string
	Content string
	ID      string
	Name    string
	// Arguments 是工具调用参数，Response 是工具结果：能解析为 JSON 时为解析结果，否则为原文字符串；为 nil 时不写出。
	Arguments any
	Response  any
	Modality  string
	MimeType  string
	URI       string
}
