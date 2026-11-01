package telemetry

import (
	"context"
	"errors"
	"runtime"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// mergeAttrs 复制属性列表并合并新增属性。
func mergeAttrs(existing, added []attribute.KeyValue) []attribute.KeyValue {
	// 1. 先复制已有属性，新增属性按原顺序追加；与新增键重复的旧值先删除，输入列表保持不变。
	merged := make([]attribute.KeyValue, 0, len(existing)+len(added))
	merged = append(merged, existing...)
	for _, attr := range added {
		merged = slices.DeleteFunc(merged, func(mergedAttr attribute.KeyValue) bool { return mergedAttr.Key == attr.Key })
		merged = append(merged, attr)
	}
	return merged
}

// getCallerFunctionName 返回调用栈指定层的函数全名。
func getCallerFunctionName(skip int) string {
	// 1. 按 skip 读取调用栈，0 指本函数；无法找到栈帧或函数时返回空串。
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return ""
	}
	function := runtime.FuncForPC(pc)
	if function == nil {
		return ""
	}
	return function.Name()
}

// metricAttrKeys 是 ctx 身份中可作为指标维度的键：Agent 名和执行环境 ID。
var metricAttrKeys = []attribute.Key{agentNameKey, executorIDKey}

// getMetricAttrs 提取可作为指标维度的身份属性。
func getMetricAttrs(ctx context.Context) []attribute.KeyValue {
	// 1. 从 ctx 属性中保留 Agent 名和执行环境 ID。
	var attrs []attribute.KeyValue
	for _, attr := range getAttrs(ctx) {
		if slices.Contains(metricAttrKeys, attr.Key) {
			attrs = append(attrs, attr)
		}
	}
	return attrs
}

// getErrorType 返回操作的错误分类。
func getErrorType(err error, errorCode string) string {
	// 1. 取消记为 canceled，其次使用错误码，其他错误记为 _OTHER，成功返回空串。
	switch {
	case errors.Is(err, context.Canceled):
		return errorTypeCanceled
	case errorCode != "":
		return errorCode
	case err != nil:
		return semconv.ErrorTypeOther.Value.AsString()
	}
	return ""
}

// isFailure 错误分类是否表示失败？
func isFailure(errorType string) bool {
	// 1. 排除表示成功的空分类和表示取消的 canceled。
	return errorType != "" && errorType != errorTypeCanceled
}

// getCanceledLogAttrs 生成取消操作的结束日志字段。
func getCanceledLogAttrs(errorType string, err error) []any {
	// 1. canceled 返回 error.type 和取消原因，其余返回 nil。
	if errorType != errorTypeCanceled {
		return nil
	}
	return []any{string(semconv.ErrorTypeKey), errorType, "error", err}
}

// getUsageAttrs 生成 token 用量属性。
func getUsageAttrs(usage defs.TokenUsage) []attribute.KeyValue {
	// 1. 总是写输入和输出 token，缓存读写和推理 token 只写非零值。
	attrs := []attribute.KeyValue{
		genAIUsageInputTokensKey.Int(usage.Input),
		genAIUsageOutputTokensKey.Int(usage.Output),
	}
	if usage.CacheRead > 0 {
		attrs = append(attrs, genAIUsageCacheReadTokensKey.Int(usage.CacheRead))
	}
	if usage.CacheWrite > 0 {
		attrs = append(attrs, genAIUsageCacheWriteTokensKey.Int(usage.CacheWrite))
	}
	if usage.Reasoning > 0 {
		attrs = append(attrs, genAIUsageReasoningTokensKey.Int(usage.Reasoning))
	}
	return attrs
}

// getFinishReasons 生成模型结束原因属性。
func getFinishReasons(finishReason, errorType string) []string {
	// 1. 优先采用已收到的原因，未收到且失败或取消时填 error，成功且没有原因时返回 nil。
	switch {
	case finishReason != "":
		return []string{finishReason}
	case errorType != "":
		return []string{FinishReasonError}
	}
	return nil
}

// getServerAttrs 生成服务地址属性。
func getServerAttrs(address string, port int) []attribute.KeyValue {
	// 1. 地址为空时返回 nil，有地址时写主机，端口为正时另写端口。
	if address == "" {
		return nil
	}
	attrs := []attribute.KeyValue{semconv.ServerAddress(address)}
	if port > 0 {
		attrs = append(attrs, semconv.ServerPort(port))
	}
	return attrs
}

// getErrorDescription 返回错误文本。
func getErrorDescription(err error) string {
	// 1. nil 返回空串，其他错误返回 Error 文本。
	if err == nil {
		return ""
	}
	return err.Error()
}
