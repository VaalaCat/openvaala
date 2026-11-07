package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	// telemetryServiceName 是 OTel resource 的 service.name。
	telemetryServiceName = "openvaala"
	// logFileMaxMegabytes 是单个日志文件轮转前的大小上限，单位 MB。
	logFileMaxMegabytes = 10
	// logFileMaxBackups 是轮转后保留的旧日志文件份数。
	logFileMaxBackups = 5
	// telemetryShutdownTimeout 是命令结束时 flush 与关闭 Provider 的最长等待。
	telemetryShutdownTimeout = 5 * time.Second
)

// newTelemetry 创建写日志文件、stderr 和 OTLP 的遥测，接管全局日志，返回遥测对象和关闭函数。
func newTelemetry(ctx context.Context, cfg *config.Config, stderr io.Writer) (*telemetry.Telemetry, func(context.Context) error, error) {
	// 1. 打开轮转日志文件，stderr 不为 nil 时再写一份文本日志；localLogger 只写这两处，记录 OTel 内部错误和关闭错误。
	level := cfg.GetTelemetryLogLevel()
	logFile, err := openLogFile(cfg.Telemetry.LogFile)
	if err != nil {
		return nil, nil, err
	}
	handlers := []slog.Handler{slog.NewJSONHandler(logFile, &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
	})}
	if stderr != nil {
		handlers = append(handlers, slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	}
	localLogger := slog.New(slog.NewMultiHandler(handlers...)).With("pid", os.Getpid())
	// 2. 创建进程的资源信息；配置了 otlp_endpoint 时创建 trace、指标和日志导出器，日志也经 OTLP 发出。
	res, err := newTelemetryResource(defs.GetBuildVersion())
	if err != nil {
		return nil, nil, errors.Join(err, logFile.Close())
	}
	tracerOptions := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	var meterProvider metric.MeterProvider = noop.NewMeterProvider()
	shutdownExport := func(context.Context) error { return nil }
	if cfg.Telemetry.OTLPEndpoint != "" {
		export, err := newOTLPExport(ctx, cfg.Telemetry.OTLPEndpoint, cfg.Telemetry.OTLPHeaders, res)
		if err != nil {
			return nil, nil, errors.Join(err, logFile.Close())
		}
		tracerOptions = append(tracerOptions, sdktrace.WithBatcher(export.spanExporter))
		meterProvider = export.meterProvider
		otlpHandler := otelslog.NewHandler(telemetry.ScopeName, otelslog.WithLoggerProvider(export.loggerProvider))
		handlers = append(handlers, newMinimumLevelHandler(level, otlpHandler))
		shutdownExport = export.shutdown
	}
	// 3. 创建 Trace Provider（没配 OTLP 时照样生成 span，日志里有 trace_id，只是不导出）和限时关闭函数；
	// Provider 关闭错误只记日志，关闭函数只返回日志文件的关闭错误。
	tracerProvider := sdktrace.NewTracerProvider(tracerOptions...)
	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, telemetryShutdownTimeout)
		defer cancel()
		for _, err := range []error{tracerProvider.Shutdown(ctx), shutdownExport(ctx)} {
			if err != nil {
				localLogger.Warn("telemetry shutdown failed", "error", err)
			}
		}
		// Provider 关闭后再关闭日志文件。
		return logFile.Close()
	}
	// 4. 创建遥测对象，capture_content 开启且配置了 otlp_endpoint 时才采集对话内容；再接管全局日志。
	logger := slog.New(slog.NewMultiHandler(handlers...)).With("pid", os.Getpid())
	captureContent := cfg.Telemetry.CaptureContent && cfg.Telemetry.OTLPEndpoint != ""
	pipeline, err := telemetry.NewTelemetry(telemetry.NewTelemetryOpt(tracerProvider, meterProvider, logger).WithCaptureContent(captureContent))
	if err != nil {
		return nil, nil, errors.Join(err, shutdown(ctx))
	}
	takeOverGlobalLogging(logger, localLogger)
	return pipeline, shutdown, nil
}

// openLogFile 创建日志目录，打开按大小轮转的日志文件。
func openLogFile(path string) (*lumberjack.Logger, error) {
	// 1. 以 0700 创建日志目录并配置轮转上限。
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	logFile := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    logFileMaxMegabytes,
		MaxBackups: logFileMaxBackups,
	}
	// 2. 写入空内容，检查文件能否打开写入。
	if _, err := logFile.Write(nil); err != nil {
		return nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	return logFile, nil
}

// newTelemetryResource 合并 OTel SDK 默认资源和本进程的服务信息。
func newTelemetryResource(version string) (*resource.Resource, error) {
	// 1. 在 SDK 默认资源上写入服务名、版本和随机实例 ID，新资源不带 schema URL。
	return resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(telemetryServiceName),
		semconv.ServiceVersion(version),
		semconv.ServiceInstanceID(uuid.NewString()),
	))
}

// otlpExport 是配置了 otlp_endpoint 时创建的 OTLP 导出对象。
type otlpExport struct {
	// spanExporter 是 trace 导出器，交给 TracerProvider 批量导出。
	spanExporter sdktrace.SpanExporter
	// meterProvider 是已接好 OTLP 指标导出器的 Provider。
	meterProvider *sdkmetric.MeterProvider
	// loggerProvider 是已接好 OTLP 日志导出器的 Provider。
	loggerProvider *sdklog.LoggerProvider
}

// newOTLPExport 创建 OTLP HTTP 的 Trace、指标和日志导出器。
func newOTLPExport(ctx context.Context, endpoint string, headers map[string]string, res *resource.Resource) (*otlpExport, error) {
	// 1. 解析基础地址，创建发往 v1/traces 的导出器。
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("parse OTLP endpoint: invalid URL")
	}
	spanExporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(base.JoinPath("v1/traces").String()), otlptracehttp.WithHeaders(headers))
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	// 2. 创建发往 v1/metrics 和 v1/logs 的导出器，失败时关闭之前创建的导出器。
	metricExporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(base.JoinPath("v1/metrics").String()), otlpmetrichttp.WithHeaders(headers))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create OTLP metric exporter: %w", err), spanExporter.Shutdown(ctx))
	}
	logExporter, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(base.JoinPath("v1/logs").String()), otlploghttp.WithHeaders(headers))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create OTLP log exporter: %w", err), spanExporter.Shutdown(ctx), metricExporter.Shutdown(ctx))
	}
	// 3. 创建接好导出器的指标和日志 Provider。
	return &otlpExport{
		spanExporter:   spanExporter,
		meterProvider:  sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter))),
		loggerProvider: sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter))),
	}, nil
}

// shutdown 刷新并关闭指标和日志 Provider。
func (e *otlpExport) shutdown(ctx context.Context) error {
	// 1. 关闭指标和日志 Provider 并合并返回错误；Trace 导出器由 TracerProvider 关闭。
	return errors.Join(e.meterProvider.Shutdown(ctx), e.loggerProvider.Shutdown(ctx))
}

// takeOverGlobalLogging 把标准库日志交给完整遥测日志，把 OTel 内部诊断和错误交给本地日志。
func takeOverGlobalLogging(logger, localLogger *slog.Logger) {
	// 1. 将标准库日志交给完整遥测日志。
	slog.SetDefault(logger)
	// 2. 将 OTel 内部诊断和错误写入不含 OTLP 的本地日志；logr V(1) 诊断对应 slog 级别 -1，info 级别下不输出。
	otel.SetLogger(logr.FromSlogHandler(localLogger.Handler()))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		localLogger.Warn("otel internal error", "error", err)
	}))
}

// minimumLevelHandler 包在 OTLP 日志桥接外面，只放行不低于 level 的日志。
type minimumLevelHandler struct {
	level slog.Level
	next  slog.Handler
}

// newMinimumLevelHandler 创建只放行不低于 level 的日志处理器。
func newMinimumLevelHandler(level slog.Level, next slog.Handler) *minimumLevelHandler {
	// 1. 保存最低级别和下游处理器。
	return &minimumLevelHandler{
		level: level,
		next:  next,
	}
}

// Enabled 这条日志是否不低于最低级别，并且下游也接受？
func (h *minimumLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// 1. 同时检查最低级别和下游过滤结果。
	return level >= h.level && h.next.Enabled(ctx, level)
}

// Handle 把日志记录交给下游处理器。
func (h *minimumLevelHandler) Handle(ctx context.Context, record slog.Record) error {
	// 1. 提交日志记录并返回处理结果。
	return h.next.Handle(ctx, record)
}

// WithAttrs 返回追加了固定字段、最低级别不变的处理器。
func (h *minimumLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// 1. 给下游添加字段并保留最低级别。
	return newMinimumLevelHandler(h.level, h.next.WithAttrs(attrs))
}

// WithGroup 返回追加了字段分组、最低级别不变的处理器。
func (h *minimumLevelHandler) WithGroup(name string) slog.Handler {
	// 1. 给下游添加分组并保留最低级别。
	return newMinimumLevelHandler(h.level, h.next.WithGroup(name))
}
