// Package telemetrytest 提供写入内存的遥测管道，供各包测试断言 span、指标与日志；与 net/http/httptest 同类。
package telemetrytest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Pipeline 是写入内存的遥测管道：span 结束时记录，指标在断言时收集，日志以 Debug 级别的 JSON 行写入内存。
type Pipeline struct {
	Telemetry *telemetry.Telemetry
	// TracerProvider 供测试自己开 span，例如模拟提交输入的请求。
	TracerProvider *sdktrace.TracerProvider

	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
	logs    *lockedBuffer
}

// NewPipeline 创建开启内容采集的内存遥测管道。
func NewPipeline(t testing.TB) *Pipeline {
	t.Helper()
	// 1. 创建内存管道并登记测试结束时关闭 Provider。
	return newPipeline(t, true)
}

// NewPipelineWithoutContent 创建关闭内容采集的内存遥测管道。
func NewPipelineWithoutContent(t testing.TB) *Pipeline {
	t.Helper()
	// 1. 创建内存管道并关闭内容采集。
	return newPipeline(t, false)
}

// newPipeline 组装内存遥测管道并登记清理。
func newPipeline(t testing.TB, captureContent bool) *Pipeline {
	t.Helper()
	// 1. 创建结束 span 记录器、手动指标读取器和 Debug JSON 日志缓冲。
	spans := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	metrics := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metrics))
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// 2. 应用内容采集开关并创建遥测对象。
	opt := telemetry.NewTelemetryOpt(tracerProvider, meterProvider, logger).WithCaptureContent(captureContent)
	memoryTelemetry, err := telemetry.NewTelemetry(opt)
	if err != nil {
		t.Fatal(err)
	}
	// 3. 登记 Provider 清理任务，返回测试管道。
	t.Cleanup(func() {
		_ = tracerProvider.Shutdown(context.Background())
		_ = meterProvider.Shutdown(context.Background())
	})
	return &Pipeline{
		Telemetry:      memoryTelemetry,
		TracerProvider: tracerProvider,
		spans:          spans,
		metrics:        metrics,
		logs:           logs,
	}
}

// GetEndedSpans 按结束顺序返回已结束的 span。
func (p *Pipeline) GetEndedSpans() []sdktrace.ReadOnlySpan {
	// 1. 读取内存中的结束记录。
	return p.spans.Ended()
}

// WaitEndedSpans 等待指定名称的结束 span 达到要求数量。
func (p *Pipeline) WaitEndedSpans(t testing.TB, name string, count int) []sdktrace.ReadOnlySpan {
	t.Helper()
	// 1. 每 10 毫秒检查已结束 span，至少 count 个时返回，超过 5 秒则测试失败。
	deadline := time.Now().Add(5 * time.Second)
	for {
		var found []sdktrace.ReadOnlySpan
		for _, span := range p.spans.Ended() {
			if span.Name() == name {
				found = append(found, span)
			}
		}
		if len(found) >= count {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("ended spans named %q = %d, want %d", name, len(found), count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// GetLogRecords 解析内存日志并筛选消息。
func (p *Pipeline) GetLogRecords(t testing.TB, msg string) []map[string]any {
	t.Helper()
	// 1. 逐行解码 JSON，msg 非空时只保留消息相等的记录。
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(p.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if msg == "" || record["msg"] == msg {
			records = append(records, record)
		}
	}
	return records
}

// WaitLogRecords 等待指定消息的日志达到要求数量。
func (p *Pipeline) WaitLogRecords(t testing.TB, msg string, count int) []map[string]any {
	t.Helper()
	// 1. 每 10 毫秒检查日志，至少 count 条时返回，超过 5 秒则测试失败。
	deadline := time.Now().Add(5 * time.Second)
	for {
		records := p.GetLogRecords(t, msg)
		if len(records) >= count {
			return records
		}
		if time.Now().After(deadline) {
			t.Fatalf("log records %q = %d, want %d", msg, len(records), count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// GetLogText 返回全部内存日志原文。
func (p *Pipeline) GetLogText() string {
	// 1. 读取日志缓冲的文本。
	return p.logs.String()
}

// GetMetricSum 收集并汇总指定指标的数据点。
func (p *Pipeline) GetMetricSum(t testing.TB, name string, attrs ...attribute.KeyValue) float64 {
	t.Helper()
	// 1. 筛选名称和指定属性相符的数据点，直方图累加 Sum，计数器累加 Value。
	var sum float64
	for _, data := range p.collectMetricData(t, name) {
		switch data := data.(type) {
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				if isAttrsMatched(point.Attributes, attrs) {
					sum += point.Sum
				}
			}
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				if isAttrsMatched(point.Attributes, attrs) {
					sum += float64(point.Sum)
				}
			}
		case metricdata.Sum[int64]:
			for _, point := range data.DataPoints {
				if isAttrsMatched(point.Attributes, attrs) {
					sum += float64(point.Value)
				}
			}
		default:
			t.Fatalf("metric %q has unsupported aggregation %T", name, data)
		}
	}
	return sum
}

// GetHistogramCount 汇总指定直方图的记录次数。
func (p *Pipeline) GetHistogramCount(t testing.TB, name string, attrs ...attribute.KeyValue) uint64 {
	t.Helper()
	// 1. 收集指定名称指标，筛选包含 attrs 的数据点并累加 Count。
	var count uint64
	for _, data := range p.collectMetricData(t, name) {
		switch data := data.(type) {
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				if isAttrsMatched(point.Attributes, attrs) {
					count += point.Count
				}
			}
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				if isAttrsMatched(point.Attributes, attrs) {
					count += point.Count
				}
			}
		default:
			t.Fatalf("metric %q has unsupported aggregation %T", name, data)
		}
	}
	return count
}

// GetMetricAttributeKeys 收集指定指标的全部属性键。
func (p *Pipeline) GetMetricAttributeKeys(t testing.TB, name string) []string {
	t.Helper()
	// 1. 收集各数据点的属性键。
	var keys []string
	addKeys := func(set attribute.Set) {
		for _, attr := range set.ToSlice() {
			keys = append(keys, string(attr.Key))
		}
	}
	for _, data := range p.collectMetricData(t, name) {
		switch data := data.(type) {
		case metricdata.Histogram[float64]:
			for _, point := range data.DataPoints {
				addKeys(point.Attributes)
			}
		case metricdata.Histogram[int64]:
			for _, point := range data.DataPoints {
				addKeys(point.Attributes)
			}
		case metricdata.Sum[int64]:
			for _, point := range data.DataPoints {
				addKeys(point.Attributes)
			}
		default:
			t.Fatalf("metric %q has unsupported aggregation %T", name, data)
		}
	}
	// 2. 按字典序排序并去重。
	slices.Sort(keys)
	return slices.Compact(keys)
}

// GetHistogramBounds 读取指定直方图的分桶。
func (p *Pipeline) GetHistogramBounds(t testing.TB, name string) []float64 {
	t.Helper()
	// 1. 返回首个直方图数据点的 Bounds，没有数据或类型不支持时测试失败。
	for _, data := range p.collectMetricData(t, name) {
		switch data := data.(type) {
		case metricdata.Histogram[float64]:
			if len(data.DataPoints) > 0 {
				return data.DataPoints[0].Bounds
			}
		case metricdata.Histogram[int64]:
			if len(data.DataPoints) > 0 {
				return data.DataPoints[0].Bounds
			}
		default:
			t.Fatalf("metric %q has unsupported aggregation %T", name, data)
		}
	}
	t.Fatalf("metric %q has no histogram data points", name)
	return nil
}

// collectMetricData 收集指定名称的指标数据。
func (p *Pipeline) collectMetricData(t testing.TB, name string) []metricdata.Aggregation {
	t.Helper()
	// 1. 手动采集指标，遍历各 scope，返回名称相同的聚合数据。
	var collected metricdata.ResourceMetrics
	if err := p.metrics.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	var found []metricdata.Aggregation
	for _, scope := range collected.ScopeMetrics {
		for _, collectedMetric := range scope.Metrics {
			if collectedMetric.Name == name {
				found = append(found, collectedMetric.Data)
			}
		}
	}
	return found
}

// GetSpanAttribute 读取 span 属性的文本形式。
func GetSpanAttribute(span sdktrace.ReadOnlySpan, key string) string {
	// 1. 按 key 查找属性，缺少时返回空串。
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.String()
		}
	}
	return ""
}

// GetSpanValue 读取 span 属性的原始值及存在标记。
func GetSpanValue(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	// 1. 按 key 查找属性，缺少时返回零值和 false。
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}

// isAttrsMatched 属性集合是否包含指定的全部键值？
func isAttrsMatched(set attribute.Set, attrs []attribute.KeyValue) bool {
	// 1. 逐个比较键、值类型和内容，任一不符返回 false。
	for _, attr := range attrs {
		value, found := set.Value(attr.Key)
		if !found || value != attr.Value {
			return false
		}
	}
	return true
}

// lockedBuffer 是可并发写入与读取的内存缓冲：被测代码在多个 goroutine 写日志，测试同时读取。
type lockedBuffer struct {
	lock   sync.Mutex
	buffer bytes.Buffer
}

// Write 加锁写入内存缓冲。
func (b *lockedBuffer) Write(p []byte) (int, error) {
	// 1. 持锁写入字节，返回后释放锁。
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buffer.Write(p)
}

// String 加锁读取内存缓冲文本。
func (b *lockedBuffer) String() string {
	// 1. 持锁读取字符串，返回后释放锁。
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buffer.String()
}
