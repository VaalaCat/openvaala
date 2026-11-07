package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"go.opentelemetry.io/otel"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// TestNewTelemetry 检查日志去向、级别和不可写日志路径。
func TestNewTelemetry(t *testing.T) {
	// 1. 列出日志输出与路径错误场景。
	tests := []struct {
		name string
		// section 是 telemetry 段的内容，{dir} 替换为测试临时目录；目录中预置名为 blocker 的普通文件。
		section    string
		withStderr bool
		wantErr    string
		check      func(t *testing.T, logText, stderrText string)
	}{
		{
			name:    "global_log_sources_go_to_file",
			section: "  log_file: {dir}/state/openvaala.log\n  otlp_endpoint: http://127.0.0.1:1\n",
			check: func(t *testing.T, logText, stderrText string) {
				for _, want := range []string{"business warning", "stdlib message", "otel internal error"} {
					if !strings.Contains(logText, want) {
						t.Errorf("log file lacks %q:\n%s", want, logText)
					}
				}
			},
		},
		{
			name:       "serve_also_writes_stderr",
			section:    "  log_file: {dir}/state/openvaala.log\n",
			withStderr: true,
			check: func(t *testing.T, logText, stderrText string) {
				if !strings.Contains(stderrText, "business warning") || !strings.Contains(logText, "business warning") {
					t.Fatalf("stderr = %q, log = %q", stderrText, logText)
				}
			},
		},
		{
			name:    "log_level_filters_info",
			section: "  log_level: warn\n  log_file: {dir}/state/openvaala.log\n",
			check: func(t *testing.T, logText, stderrText string) {
				if strings.Contains(logText, "business info") || !strings.Contains(logText, "business warning") {
					t.Fatalf("log = %q", logText)
				}
			},
		},
		{
			name:    "unwritable_log_directory_fails",
			section: "  log_file: {dir}/blocker/openvaala.log\n",
			wantErr: "create log directory",
		},
	}
	// 2. 创建遥测并写入业务、标准库和 OTel 日志，关闭后检查文件和 stderr。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreProcessLogging(t)
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "blocker"), []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := loadTestTelemetryConfig(t, strings.ReplaceAll(tt.section, "{dir}", directory))
			var stderr bytes.Buffer
			var stderrWriter io.Writer
			if tt.withStderr {
				stderrWriter = &stderr
			}
			pipeline, shutdown, err := newTelemetry(t.Context(), cfg, stderrWriter)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("newTelemetry() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := telemetry.ContextWithTelemetry(t.Context(), pipeline)
			telemetry.GetLogger(ctx).Info("business info")
			telemetry.GetLogger(ctx).Warn("business warning")
			log.Print("stdlib message")
			otel.Handle(errors.New("export failed"))
			shutdownCtx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			// 关闭时忽略不可访问导出端的刷新错误，读取本地日志。
			_ = shutdown(shutdownCtx)
			logText, err := os.ReadFile(cfg.Telemetry.LogFile)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, string(logText), stderr.String())
		})
	}
}

// TestNewTelemetryCaptureContent 检查内容采集开关与 OTLP 地址的组合。
func TestNewTelemetryCaptureContent(t *testing.T) {
	// 1. 列出开关与导出地址场景。
	tests := []struct {
		name string
		// section 是 telemetry 段的内容，{dir} 替换为测试临时目录，{endpoint} 替换为假采集端的地址。
		section string
		want    bool
	}{
		{
			name:    "default_enabled_with_endpoint",
			section: "  log_file: {dir}/openvaala.log\n  otlp_endpoint: {endpoint}\n",
			want:    true,
		},
		{
			name:    "explicitly_enabled_with_endpoint",
			section: "  log_file: {dir}/openvaala.log\n  otlp_endpoint: {endpoint}\n  capture_content: true\n",
			want:    true,
		},
		{
			name:    "disabled_with_endpoint",
			section: "  log_file: {dir}/openvaala.log\n  otlp_endpoint: {endpoint}\n  capture_content: false\n",
		},
		// 未配置导出目标时不采集内容。
		{
			name:    "enabled_without_endpoint",
			section: "  log_file: {dir}/openvaala.log\n  capture_content: true\n",
		},
		{
			name:    "default_without_endpoint",
			section: "  log_file: {dir}/openvaala.log\n",
		},
	}
	// 2. 创建遥测和模型记录器，检查是否采集内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreProcessLogging(t)
			collector := newFakeCollector(t)
			cfg := loadTestTelemetryConfig(t, strings.NewReplacer("{dir}", t.TempDir(), "{endpoint}", collector.URL).Replace(tt.section))
			pipeline, shutdown, err := newTelemetry(t.Context(), cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = shutdown(context.Background()) }()
			_, recorder := telemetry.StartChat(telemetry.ContextWithTelemetry(t.Context(), pipeline), telemetry.ChatStart{
				Model:    "m",
				Provider: "openai",
			})
			defer recorder.End(nil)
			if got := recorder.IsContentCaptured(); got != tt.want {
				t.Fatalf("IsContentCaptured() = %v, want %v", got, tt.want)
			}
		})
	}
}

// restoreProcessLogging 登记测试结束时恢复标准库日志和默认 slog 输出。
func restoreProcessLogging(t *testing.T) {
	t.Helper()
	// 1. 保存当前日志配置并登记恢复操作；OTel 的全局委托设置不恢复。
	previousLogger, previousLogWriter, previousLogFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})
}

// collectorRequest 是假采集端收到的一次 OTLP HTTP 请求。
type collectorRequest struct {
	method        string
	path          string
	authorization string
	body          []byte
}

// fakeCollector 记录收到的全部 OTLP HTTP 请求并对每个请求返回 200；请求体为未压缩的 protobuf，字符串字段以原文出现。
type fakeCollector struct {
	URL string

	mu       sync.Mutex
	requests []collectorRequest
}

// newFakeCollector 启动记录 OTLP 请求的测试采集服务。
func newFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	// 1. 启动 HTTP 服务，记录方法、路径、认证头和请求体，测试结束时关闭服务。
	collector := &fakeCollector{}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collector.mu.Lock()
		collector.requests = append(collector.requests, collectorRequest{
			r.Method,
			r.URL.Path,
			r.Header.Get("Authorization"),
			body,
		})
		collector.mu.Unlock()
	}))
	t.Cleanup(server.Close)
	collector.URL = server.URL
	return collector
}

// getRequests 返回已经收到的采集请求副本。
func (c *fakeCollector) getRequests() []collectorRequest {
	// 1. 加锁复制请求列表后释放锁。
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requests)
}

// getBody 拼接指定信号收到的全部请求体。
func (c *fakeCollector) getBody(signal string) string {
	// 1. 筛选路径以信号名结尾的请求并拼接正文。
	var body []byte
	for _, request := range c.getRequests() {
		if strings.HasSuffix(request.path, signal) {
			body = append(body, request.body...)
		}
	}
	return string(body)
}

// TestNewTelemetryOTLPExport 检查 OTLP 导出路径、请求头、日志级别和关闭失败处理。
func TestNewTelemetryOTLPExport(t *testing.T) {
	// 1. 列出各信号路径、内容和关闭结果。
	const exporting = "  log_file: {dir}/state/openvaala.log\n  otlp_endpoint: {endpoint}\n  otlp_headers:\n    Authorization: Bearer token\n"
	allSignalPaths := []string{"/v1/logs", "/v1/metrics", "/v1/traces"}
	tests := []struct {
		name string
		// section 是 telemetry 段的内容，{dir} 替换为测试临时目录，{endpoint} 替换为假采集端的地址。
		section string
		// canceledShutdown 为 true 时以已取消的 context 调用 shutdown，模拟 Provider 无法 flush。
		canceledShutdown bool
		wantPaths        []string
		// wantBodies 与 wantAbsentBodies 按信号（v1/traces、v1/metrics、v1/logs）断言采集端收到的请求体。
		wantBodies       map[string][]string
		wantAbsentBodies map[string][]string
		wantLogFile      []string
	}{
		{
			name:      "exports_all_signals_to_signal_paths",
			section:   exporting,
			wantPaths: allSignalPaths,
			wantBodies: map[string][]string{
				"v1/traces":  {"execute_tool bash"},
				"v1/metrics": {"gen_ai.execute_tool.duration"},
				// 日志桥接的 scope 与 Tracer、Meter 一致。
				"v1/logs": {"business info", "business warning", telemetry.ScopeName},
			},
			// OTel 内部错误仅出现在本地日志，不出现在 OTLP 请求体。
			wantAbsentBodies: map[string][]string{"v1/logs": {"otel internal error"}},
			wantLogFile:      []string{"otel internal error"},
		},
		{
			name:             "log_level_filters_info_from_otlp",
			section:          "  log_level: warn\n" + exporting,
			wantPaths:        allSignalPaths,
			wantBodies:       map[string][]string{"v1/logs": {"business warning"}},
			wantAbsentBodies: map[string][]string{"v1/logs": {"business info", "otel internal error"}},
		},
		{
			name:      "endpoint_path_prefix_is_kept",
			section:   strings.Replace(exporting, "{endpoint}", "{endpoint}/otlp", 1),
			wantPaths: []string{"/otlp/v1/logs", "/otlp/v1/metrics", "/otlp/v1/traces"},
		},
		{
			name:             "shutdown_failure_does_not_fail_command",
			section:          exporting,
			canceledShutdown: true,
			wantLogFile:      []string{"telemetry shutdown failed"},
		},
	}
	// 2. 生成 Trace、指标和日志，关闭遥测后检查请求和日志文件。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreProcessLogging(t)
			collector := newFakeCollector(t)
			section := strings.NewReplacer("{dir}", t.TempDir(), "{endpoint}", collector.URL).Replace(tt.section)
			cfg := loadTestTelemetryConfig(t, section)
			pipeline, shutdown, err := newTelemetry(t.Context(), cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := telemetry.ContextWithTelemetry(t.Context(), pipeline)
			_, toolRecorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
				Name:   "bash",
				CallID: "call-1",
			})
			toolRecorder.End("")
			telemetry.GetLogger(ctx).Info("business info")
			telemetry.GetLogger(ctx).Warn("business warning")
			otel.Handle(errors.New("export failed"))
			shutdownCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.canceledShutdown {
				cancel()
			}
			// Provider 刷新失败时 shutdown 仍返回 nil。
			if err := shutdown(shutdownCtx); err != nil {
				t.Fatalf("shutdown() = %v, want nil", err)
			}
			var gotPaths []string
			for _, request := range collector.getRequests() {
				if request.method != http.MethodPost || request.authorization != "Bearer token" {
					t.Errorf("request %s %s authorization %q, want POST with the configured header", request.method, request.path, request.authorization)
				}
				if !slices.Contains(gotPaths, request.path) {
					gotPaths = append(gotPaths, request.path)
				}
			}
			slices.Sort(gotPaths)
			if tt.wantPaths != nil && !slices.Equal(gotPaths, tt.wantPaths) {
				t.Errorf("request paths = %v, want %v", gotPaths, tt.wantPaths)
			}
			for signal, wants := range tt.wantBodies {
				for _, want := range wants {
					if !strings.Contains(collector.getBody(signal), want) {
						t.Errorf("%s export lacks %q", signal, want)
					}
				}
			}
			for signal, absents := range tt.wantAbsentBodies {
				for _, absent := range absents {
					if strings.Contains(collector.getBody(signal), absent) {
						t.Errorf("%s export contains %q", signal, absent)
					}
				}
			}
			logText, err := os.ReadFile(cfg.Telemetry.LogFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.wantLogFile {
				if !strings.Contains(string(logText), want) {
					t.Errorf("log file lacks %q:\n%s", want, logText)
				}
			}
		})
	}
}

// TestNewTelemetryResource 检查服务版本和默认 SDK 资源字段。
func TestNewTelemetryResource(t *testing.T) {
	// 1. 列出 OTel 环境变量场景。
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "without_environment"},
		{
			name: "service_name_from_environment",
			env:  map[string]string{"OTEL_SERVICE_NAME": "from-env"},
		},
		{
			name: "attributes_from_environment",
			env:  map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=test"},
		},
	}
	// 2. 设置环境，合并资源并检查服务版本和 SDK 语言字段。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			// 合并 SDK 默认资源和进程资源。
			merged, err := newTelemetryResource("1.2.3")
			if err != nil {
				t.Fatalf("newTelemetryResource() error = %v", err)
			}
			version, found := merged.Set().Value(semconv.ServiceVersionKey)
			if !found || version.AsString() != "1.2.3" {
				t.Fatalf("service.version = %v, found %v", version, found)
			}
			if _, found := merged.Set().Value("telemetry.sdk.language"); !found {
				t.Fatalf("resource lacks telemetry.sdk.language: %v", merged.Attributes())
			}
		})
	}
}

// loadTestTelemetryConfig 写入并加载指定遥测设置的测试配置。
func loadTestTelemetryConfig(t *testing.T, section string) *config.Config {
	t.Helper()
	// 1. 在临时目录写入 run 和 telemetry 配置，加载后返回。
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte("run:\n  agent_name: assistant\ntelemetry:\n"+section), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(&config.LoadConfigOpt{
		Home: home,
		Path: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
