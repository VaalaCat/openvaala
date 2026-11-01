package runtimeserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/coder/websocket"
)

// TestNewRuntimeServer 检查服务配置的必需依赖和监听器选择。
func TestNewRuntimeServer(t *testing.T) {
	// 1. 创建存储和 Runtime，列出服务配置的校验场景。
	store := openServerStore(t)
	rt, err := runtime.NewRuntime(newTestRuntimeOpt(store, newServerModel(t), t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	configuration, modelCatalog := newTestConfiguration(), newTestModelCatalog()
	newOpt := func(listen, token string) *runtimeserver.RuntimeServerOpt {
		opt := runtimeserver.NewRuntimeServerOpt(rt, listen, token)
		opt.Configuration, opt.ModelCatalog = configuration, modelCatalog
		return opt
	}
	tests := []struct {
		name string
		opt  *runtimeserver.RuntimeServerOpt
		// configure 非 nil 时在 opt 上去掉一项必需的依赖。
		configure func(opt *runtimeserver.RuntimeServerOpt)
		wantErr   bool
	}{
		{
			name:    "nil_options",
			opt:     nil,
			wantErr: true,
		},
		{
			name:    "empty_token",
			opt:     newOpt("127.0.0.1:0", ""),
			wantErr: true,
		},
		{
			name:    "empty_listen",
			opt:     newOpt("", testToken),
			wantErr: true,
		},
		{
			name:      "missing_configuration",
			opt:       newOpt("127.0.0.1:0", testToken),
			configure: func(opt *runtimeserver.RuntimeServerOpt) { opt.Configuration = nil },
			wantErr:   true,
		},
		{
			name:      "missing_model_catalog",
			opt:       newOpt("127.0.0.1:0", testToken),
			configure: func(opt *runtimeserver.RuntimeServerOpt) { opt.ModelCatalog = nil },
			wantErr:   true,
		},
		{
			name: "valid",
			opt:  newOpt("127.0.0.1:0", testToken),
		},
		{
			name: "listener_without_listen",
			opt: &runtimeserver.RuntimeServerOpt{
				Runtime:       rt,
				Configuration: configuration,
				ModelCatalog:  modelCatalog,
				Listener:      newTestListener(t),
				Token:         testToken,
			},
		},
	}
	// 2. 逐项创建 Server，检查必需依赖、token、监听地址及传入监听器的配置结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.configure != nil {
				tt.configure(tt.opt)
			}
			if _, err := runtimeserver.NewRuntimeServer(tt.opt); (err != nil) != tt.wantErr {
				t.Fatalf("NewRuntimeServer() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestRuntimeServerAuth 检查访问令牌缺失、错误和两种传入方式。
func TestRuntimeServerAuth(t *testing.T) {
	// 1. 列出访问令牌缺失、错误和两种传入方式的测试场景。
	h := startTestServer(t)
	tests := []struct {
		name       string
		path       string
		header     http.Header
		wantStatus int
	}{
		{
			name:       "missing_token",
			path:       "/api/v1/openvaala",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong_query_token",
			path:       "/api/v1/openvaala?token=wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong_bearer_token",
			path:       "/api/v1/openvaala",
			header:     http.Header{"Authorization": {"Bearer wrong"}},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "query_token",
			path:       "/api/v1/openvaala?token=" + testToken,
			wantStatus: http.StatusSwitchingProtocols,
		},
		{
			name:       "bearer_token",
			path:       "/api/v1/openvaala",
			header:     http.Header{"Authorization": {"Bearer " + testToken}},
			wantStatus: http.StatusSwitchingProtocols,
		},
	}
	// 2. 按各路径和 token 组合发起请求，检查认证失败状态及两种有效 token 的握手结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, response, err := h.dialWith(t, tt.path, tt.header)
			if conn != nil {
				_ = conn.CloseNow()
			}
			if response == nil || response.StatusCode != tt.wantStatus {
				t.Fatalf("dial %s = %+v, %v; want status %d", tt.path, response, err, tt.wantStatus)
			}
		})
	}
}

// TestRuntimeServerWebUI 检查页面路由：根路径跳转、首页、前端路由回退、静态资源及缓存头、缺失资源、没有打包页面和 API 仍需 token。
func TestRuntimeServerWebUI(t *testing.T) {
	// 1. 列出各路径在有页面和没有页面时的测试场景。
	pages := fstest.MapFS{
		"index.html":  {Data: []byte("<html>app</html>")},
		"assets/a.js": {Data: []byte("js")},
	}
	tests := []struct {
		name         string
		webUI        fs.FS
		path         string
		wantStatus   int
		wantLocation string
		wantBody     string
		wantCache    string
	}{
		{
			name:         "root_redirects",
			webUI:        pages,
			path:         "/",
			wantStatus:   http.StatusFound,
			wantLocation: "/web/v1/",
		},
		{
			name:       "index",
			webUI:      pages,
			path:       "/web/v1/",
			wantStatus: http.StatusOK,
			wantBody:   "<html>app</html>",
			wantCache:  "no-cache",
		},
		{
			name:       "index_html",
			webUI:      pages,
			path:       "/web/v1/index.html",
			wantStatus: http.StatusOK,
			wantBody:   "<html>app</html>",
			wantCache:  "no-cache",
		},
		{
			name:       "spa_fallback",
			webUI:      pages,
			path:       "/web/v1/s/x/c/y",
			wantStatus: http.StatusOK,
			wantBody:   "<html>app</html>",
			wantCache:  "no-cache",
		},
		{
			name:       "asset",
			webUI:      pages,
			path:       "/web/v1/assets/a.js",
			wantStatus: http.StatusOK,
			wantBody:   "js",
			wantCache:  "public, max-age=31536000, immutable",
		},
		{
			name:       "missing_asset",
			webUI:      pages,
			path:       "/web/v1/missing.js",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "not_built",
			path:       "/web/v1/",
			wantStatus: http.StatusNotFound,
			wantBody:   "web UI is not built into this binary; build with script/build.sh",
		},
		{
			name:       "api_still_requires_token",
			webUI:      pages,
			path:       "/api/v1/openvaala",
			wantStatus: http.StatusUnauthorized,
		},
	}
	// 2. 逐项启动带（或不带）页面的 Server，不跟随跳转地请求路径，检查状态码、跳转地址、正文和缓存头。
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) { opt.WebUI = tt.webUI })
			response, err := client.Get("http://" + h.address + tt.path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", tt.path, response.StatusCode, tt.wantStatus)
			}
			if location := response.Header.Get("Location"); location != tt.wantLocation {
				t.Fatalf("GET %s Location = %q, want %q", tt.path, location, tt.wantLocation)
			}
			if tt.wantBody != "" && string(body) != tt.wantBody {
				t.Fatalf("GET %s body = %q, want %q", tt.path, body, tt.wantBody)
			}
			if cacheControl := response.Header.Get("Cache-Control"); cacheControl != tt.wantCache {
				t.Fatalf("GET %s Cache-Control = %q, want %q", tt.path, cacheControl, tt.wantCache)
			}
		})
	}
}

// TestRuntimeServerRun 检查健康检查、大帧、监听失败和连接关闭。
func TestRuntimeServerRun(t *testing.T) {
	// 1. 列出健康检查、大帧、监听失败和连接关闭的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *serverHarness)
	}{
		{
			name: "healthz_without_token",
			run: func(t *testing.T, h *serverHarness) {
				response, err := http.Get("http://" + h.address + "/api/v1/healthz")
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("healthz = %+v, %v", response, err)
				}
				_ = response.Body.Close()
			},
		},
		{
			// 10 MiB 的请求帧在 16 MiB 上限内：正常应答，连接不被关闭。
			name: "large_request_frame",
			run: func(t *testing.T, h *serverHarness) {
				client := h.dial(t)
				client.initialize(t)
				conversation := client.createConversation(t)
				client.callResult(t, "conversation/submitInput", map[string]any{
					"conversation_id": conversation.ID,
					"content":         textContent(strings.Repeat("x", 10<<20)),
				}, nil)
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"收到"}}
				client.callResult(t, "conversation/read", map[string]any{"conversation_id": conversation.ID}, nil)
			},
		},
		{
			name: "shutdown_closes_open_connections",
			run: func(t *testing.T, h *serverHarness) {
				client := h.dial(t)
				client.initialize(t)
				if err := h.stop(t); !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() = %v, want context.Canceled", err)
				}
				select {
				case _, open := <-client.responses:
					if open {
						t.Fatal("unexpected response after shutdown")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("connection was not closed by shutdown")
				}
			},
		},
		{
			name: "listen_failure_returns_error",
			run: func(t *testing.T, h *serverHarness) {
				rt, err := runtime.NewRuntime(newTestRuntimeOpt(h.store, h.model, h.defaultDirectory))
				if err != nil {
					t.Fatal(err)
				}
				opt := runtimeserver.NewRuntimeServerOpt(rt, h.address, testToken)
				opt.Configuration, opt.ModelCatalog = newTestConfiguration(), newTestModelCatalog()
				server, err := runtimeserver.NewRuntimeServer(opt)
				if err != nil {
					t.Fatal(err)
				}
				if err := server.Run(t.Context()); err == nil || errors.Is(err, context.Canceled) {
					t.Fatalf("Run() on a used address = %v, want listen error", err)
				}
			},
		},
		{
			name: "provided_listener",
			run: func(t *testing.T, h *serverHarness) {
				runProvidedListener(t, h, "")
			},
		},
		{
			// Listen 指向已被占用的地址：若 Run 没有优先使用 Listener，会以监听错误结束。
			name: "listener_wins_over_listen",
			run: func(t *testing.T, h *serverHarness) {
				runProvidedListener(t, h, h.address)
			},
		},
	}
	// 2. 启动服务并访问 HTTP 与 WebSocket，检查健康响应、大帧、监听失败和取消时连接关闭。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestServer(t))
		})
	}
}

// TestRuntimeServerShutdown 检查关闭路由的认证和退出结果。
func TestRuntimeServerShutdown(t *testing.T) {
	// 1. 列出关闭路由的认证和退出结果的测试场景。
	tests := []struct {
		name       string
		authorized bool
		wantStatus int
		wantExit   bool
	}{
		{
			name:       "without_token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "with_token",
			authorized: true,
			wantStatus: http.StatusAccepted,
			wantExit:   true,
		},
	}
	// 2. 按各认证和请求方式访问关闭路由，检查 HTTP 状态及服务是否正常退出。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startTestServer(t)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+h.address+"/api/v1/shutdown", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.authorized {
				request.Header.Set("Authorization", "Bearer "+testToken)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("shutdown status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
			timeout := 500 * time.Millisecond
			if tt.wantExit {
				timeout = 2 * time.Second
			}
			exited, err := h.waitExit(t, timeout)
			if exited != tt.wantExit || err != nil {
				t.Fatalf("Run() exited = %v, err = %v; want exited = %v and nil error", exited, err, tt.wantExit)
			}
		})
	}
}

// TestRuntimeServerIdleTimeout 检查空闲退出、连接与活动 Turn 保留服务。
func TestRuntimeServerIdleTimeout(t *testing.T) {
	// 1. 准备短、长两种空闲时长，列出空闲退出、连接保留服务和 Turn 保留服务的场景；需要先连上再观察的场景用较长的
	// IdleTimeout：第一次空闲检查发生在 Run 开始后一个 IdleTimeout，慢速或加了 -race 的运行里，等待就绪和建立连接可能超过 200ms。
	const (
		shortIdleTimeout = 200 * time.Millisecond
		longIdleTimeout  = time.Second
	)
	tests := []struct {
		name        string
		idleTimeout time.Duration
		run         func(t *testing.T, h *serverHarness)
	}{
		{
			name:        "idle_runtime_exits",
			idleTimeout: shortIdleTimeout,
			run: func(t *testing.T, h *serverHarness) {
				requireExit(t, h, 2*time.Second)
			},
		},
		{
			name:        "open_connection_keeps_running",
			idleTimeout: longIdleTimeout,
			run: func(t *testing.T, h *serverHarness) {
				client := h.dial(t)
				client.initialize(t)
				requireRunning(t, h, 3*longIdleTimeout)
				if err := client.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
					t.Fatal(err)
				}
				requireExit(t, h, 5*longIdleTimeout)
			},
		},
		{
			name:        "running_turn_keeps_running",
			idleTimeout: longIdleTimeout,
			run: func(t *testing.T, h *serverHarness) {
				client := h.dial(t)
				client.initialize(t)
				conversation := client.createConversation(t)
				submitText(t, client, conversation, "开始")
				request := h.model.receive(t)
				if err := client.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
					t.Fatal(err)
				}
				requireRunning(t, h, 3*longIdleTimeout)
				request.reply <- serverModelResponse{chunks: []string{"完成"}}
				requireExit(t, h, 5*longIdleTimeout)
			},
		},
	}
	// 2. 按场景建立连接或运行 Turn，检查忙碌期间保持服务及空闲超时退出。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, startTestServerWithIdleTimeout(t, nil, tt.idleTimeout))
		})
	}
}

// requireRunning 检查服务在指定时长内保持运行。
func requireRunning(t *testing.T, h *serverHarness, duration time.Duration) {
	t.Helper()
	// 1. 等待指定时长，服务提前返回则结束测试。
	if exited, err := h.waitExit(t, duration); exited {
		t.Fatalf("Run() returned %v within %v, want it to keep running", err, duration)
	}
}

// requireExit 检查服务在指定时限内正常退出。
func requireExit(t *testing.T, h *serverHarness, timeout time.Duration) {
	t.Helper()
	// 1. 等待服务返回 nil，超时或返回错误则结束测试。
	if exited, err := h.waitExit(t, timeout); !exited || err != nil {
		t.Fatalf("Run() exited = %v, err = %v; want it to return nil within %v", exited, err, timeout)
	}
}

// TestRuntimeServerTelemetry 检查监听和连接日志、对端地址与连接计数。
func TestRuntimeServerTelemetry(t *testing.T) {
	// 1. 准备请求头里伪造的客户端地址 spoofedAddress（日志必须记录 TCP 对端而不是它），列出监听、拒绝连接、升级失败和连接开关的遥测场景。
	const spoofedAddress = "203.0.113.9"
	tests := []struct {
		name string
		run  func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline)
	}{
		{
			name: "listening_log",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				record := p.WaitLogRecords(t, "runtime server listening", 1)[0]
				host, port, err := net.SplitHostPort(h.address)
				if err != nil {
					t.Fatal(err)
				}
				wantPort, err := strconv.Atoi(port)
				if err != nil {
					t.Fatal(err)
				}
				if record["level"] != "INFO" || record["server.address"] != host || record["server.port"] != float64(wantPort) {
					t.Fatalf("log = %v, want INFO with server.address %s and server.port %d", record, host, wantPort)
				}
			},
		},
		{
			name: "rejected_connection_logs_reason_without_token",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				conn, response, _ := h.dialWith(t, "/api/v1/openvaala?token=wrong-token", nil)
				if conn != nil {
					_ = conn.CloseNow()
				}
				if response == nil || response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("dial = %+v, want 401", response)
				}
				record := p.WaitLogRecords(t, "connection rejected", 1)[0]
				if record["level"] != "WARN" || record["reason"] != "invalid token" || record["network.peer.address"] != "127.0.0.1" {
					t.Fatalf("log = %v", record)
				}
				if strings.Contains(p.GetLogText(), "wrong-token") {
					t.Fatalf("logs contain the rejected token: %s", p.GetLogText())
				}
			},
		},
		{
			name: "rejected_connection_ignores_forwarded_header",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				conn, _, _ := h.dialWith(t, "/api/v1/openvaala?token=wrong-token", http.Header{"X-Forwarded-For": {spoofedAddress}})
				if conn != nil {
					_ = conn.CloseNow()
				}
				record := p.WaitLogRecords(t, "connection rejected", 1)[0]
				if record["network.peer.address"] != "127.0.0.1" {
					t.Fatalf("log = %v, want the TCP peer 127.0.0.1 rather than %s", record, spoofedAddress)
				}
			},
		},
		{
			name: "upgrade_failure_logs_warning",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+h.address+"/api/v1/openvaala?token="+testToken, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Forwarded-For", spoofedAddress)
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				record := p.WaitLogRecords(t, "websocket upgrade failed", 1)[0]
				if id, _ := record["openvaala.connection.id"].(string); record["level"] != "WARN" || id == "" || record["error"] == nil || record["network.peer.address"] != "127.0.0.1" {
					t.Fatalf("log = %v", record)
				}
				if got := p.GetLogRecords(t, "connection opened"); len(got) != 0 {
					t.Fatalf("connection opened logs = %v, want none", got)
				}
			},
		},
		{
			name: "connection_opened_logs_peer_address",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				conn, _, err := h.dialWith(t, "/api/v1/openvaala?token="+testToken, http.Header{"X-Forwarded-For": {spoofedAddress}})
				if err != nil {
					t.Fatal(err)
				}
				newTestClient(t, conn).initialize(t)
				record := p.WaitLogRecords(t, "connection opened", 1)[0]
				if id, _ := record["openvaala.connection.id"].(string); record["level"] != "INFO" || id == "" || record["network.peer.address"] != "127.0.0.1" || record["network.peer.port"] == nil {
					t.Fatalf("log = %v", record)
				}
			},
		},
		{
			name: "connection_closed_logs_reason",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				client := h.dial(t)
				client.initialize(t)
				opened := p.WaitLogRecords(t, "connection opened", 1)[0]
				if err := client.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
					t.Fatal(err)
				}
				closed := p.WaitLogRecords(t, "connection closed", 1)[0]
				reason, _ := closed["reason"].(string)
				if closed["level"] != "INFO" || closed["openvaala.connection.id"] != opened["openvaala.connection.id"] || !strings.Contains(reason, "read frame") {
					t.Fatalf("opened = %v, closed = %v", opened, closed)
				}
				if got := p.GetMetricSum(t, "openvaala.server.connection.active"); got != 0 {
					t.Fatalf("active connections = %v, want 0", got)
				}
			},
		},
		{
			name: "open_connection_counts_as_active",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				h.dial(t).initialize(t)
				p.WaitLogRecords(t, "connection opened", 1)
				if got := p.GetMetricSum(t, "openvaala.server.connection.active"); got != 1 {
					t.Fatalf("active connections = %v, want 1", got)
				}
			},
		},
		{
			name: "server_shutdown_logs_context_canceled",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				h.dial(t).initialize(t)
				p.WaitLogRecords(t, "connection opened", 1)
				h.stop(t)
				closed := p.WaitLogRecords(t, "connection closed", 1)[0]
				if reason, _ := closed["reason"].(string); closed["level"] != "INFO" || reason != "context canceled" {
					t.Fatalf("closed = %v, want INFO with reason context canceled", closed)
				}
			},
		},
		{
			// 客户端不发关闭帧直接断开 TCP，属于异常断开。
			name: "connection_dropped_without_close_frame_logs_warning",
			run: func(t *testing.T, h *serverHarness, p *telemetrytest.Pipeline) {
				client := h.dial(t)
				client.initialize(t)
				opened := p.WaitLogRecords(t, "connection opened", 1)[0]
				if err := client.conn.CloseNow(); err != nil {
					t.Fatal(err)
				}
				closed := p.WaitLogRecords(t, "connection closed", 1)[0]
				reason, _ := closed["reason"].(string)
				if closed["level"] != "WARN" || closed["openvaala.connection.id"] != opened["openvaala.connection.id"] || !strings.Contains(reason, "read frame") {
					t.Fatalf("opened = %v, closed = %v, want WARN with a read frame reason", opened, closed)
				}
			},
		},
	}
	// 2. 运行各监听和连接场景，检查实际地址、对端属性、连接计数及正常和异常日志。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			tt.run(t, startTestServerWithTelemetry(t, p.Telemetry), p)
		})
	}
}

// runProvidedListener 使用传入监听器运行服务，检查取消后监听器关闭。
func runProvidedListener(t *testing.T, h *serverHarness, listen string) {
	t.Helper()
	// 1. 创建 Runtime、测试监听器和 Server。
	rt, err := runtime.NewRuntime(newTestRuntimeOpt(h.store, h.model, h.defaultDirectory))
	if err != nil {
		t.Fatal(err)
	}
	listener := newTestListener(t)
	server, err := runtimeserver.NewRuntimeServer(&runtimeserver.RuntimeServerOpt{
		Runtime:       rt,
		Configuration: newTestConfiguration(),
		ModelCatalog:  newTestModelCatalog(),
		Listen:        listen,
		Listener:      listener,
		Token:         testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 2. 启动服务并等待健康检查就绪。
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	other := &serverHarness{
		store:            h.store,
		model:            h.model,
		defaultDirectory: h.defaultDirectory,
		address:          listener.Addr().String(),
		cancel:           cancel,
		done:             done,
	}
	other.waitReady(t)
	// 3. 取消服务，检查返回 context.Canceled 且监听器 Accept 返回 net.ErrClosed。
	if err := other.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept() after Run = %v, want net.ErrClosed", err)
	}
}

// TestRuntimeServerModelSwitching 检查协议模型切换后的请求、运行信息、子任务继承和 Turn 记录。
func TestRuntimeServerModelSwitching(t *testing.T) {
	// 1. 启动服务，创建对话并订阅根 Session 运行信息。
	h := startTestServer(t)
	client := h.dial(t)
	client.initialize(t)
	conversation := client.createConversation(t)
	rootID := conversation.RootSessionID
	client.callResult(t, "conversation/subscribe", map[string]any{"conversation_id": conversation.ID}, nil)
	rootTurnCompleted := func(message clientMessage) bool {
		view := decodeNotification(t, message)
		return view.SessionID == rootID && view.Info["state"] == "idle" && view.Info["last_turn_status"] == "completed"
	}
	wantB := defs.ModelSelection{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "b",
		},
		ReasoningEffort: "high",
	}

	// 2. 第一轮用 Agent 默认模型，返回推理和回复。
	submitText(t, client, conversation, "第一轮")
	first := h.model.receive(t)
	if first.body["model"] != "test" {
		t.Fatalf("first request model = %v", first.body["model"])
	}
	first.reply <- serverModelResponse{
		reasoning: "A-REASONING",
		chunks:    []string{"一"},
	}
	client.collectNotifications(t, rootTurnCompleted)

	// 3. 切到 B 并指定强度；推送里的所选模型是 B。
	var switched struct {
		Session defs.Session `json:"session"`
	}
	client.callResult(t, "session/setModel", map[string]any{
		"conversation_id": conversation.ID, "session_id": rootID,
		"model": map[string]any{"ref": map[string]any{"provider_id": "test", "model_id": "b"}, "reasoning_effort": "high"},
	}, &switched)
	if switched.Session.Model != wantB {
		t.Fatalf("setModel session = %+v", switched.Session.Model)
	}
	client.collectNotifications(t, func(message clientMessage) bool {
		var view struct {
			Info struct {
				Session defs.Session `json:"session"`
			} `json:"info"`
		}
		_ = json.Unmarshal(message.Params, &view)
		return view.Info.Session.ID == rootID && view.Info.Session.Model == wantB
	})

	// 4. 第二轮的真实请求：B 的上游名、输出上限和强度，带切换提示，不带 A 的推理。
	submitText(t, client, conversation, "第二轮")
	second := h.model.receive(t)
	messages, _ := json.Marshal(second.body["messages"])
	if second.body["model"] != "model-b" || second.body["reasoning_effort"] != "high" || second.body["max_tokens"] != float64(4096) ||
		!strings.Contains(string(messages), "openvaala-model-change-event") || strings.Contains(string(messages), "A-REASONING") {
		t.Fatalf("second request = %v", second.body)
	}

	// 5. 第二轮运行期间，推送的本轮模型是 B。
	client.collectNotifications(t, func(message clientMessage) bool {
		var view struct {
			Info struct {
				ActiveTurnModel defs.ModelSelection `json:"active_turn_model"`
			} `json:"info"`
		}
		_ = json.Unmarshal(message.Params, &view)
		return view.Info.ActiveTurnModel == wantB
	})

	// 6. 第二轮调用 spawn_agent 且不传模型：子 Session 继承 B。
	second.reply <- serverModelResponse{calls: []llmkit.ToolCall{{
		ID:        "call-spawn",
		Name:      "spawn_agent",
		Arguments: `{"task_name":"子任务","message":"做点事"}`,
	}}}
	var childBody map[string]any
	for range 2 { // 子 Session 的首轮请求和父 Session 带工具结果的后续请求，到达顺序不定
		request := h.model.receive(t)
		if payload, _ := json.Marshal(request.body["messages"]); strings.Contains(string(payload), "TaskName: 子任务") {
			childBody = request.body
		}
		request.reply <- serverModelResponse{chunks: []string{"好"}}
	}
	if childBody == nil || childBody["model"] != "model-b" || childBody["reasoning_effort"] != "high" {
		t.Fatalf("child request = %v", childBody)
	}
	var sessions struct {
		Sessions []defs.Session `json:"sessions"`
	}
	client.callResult(t, "session/list", map[string]any{"conversation_id": conversation.ID}, &sessions)
	child := sessions.Sessions[slices.IndexFunc(sessions.Sessions, func(record defs.Session) bool { return record.ParentSessionID == rootID })]
	if child.Model != wantB {
		t.Fatalf("child record model = %+v", child.Model)
	}

	// 7. 按 TurnID 创建顺序检查两轮记录分别使用默认模型和 B。
	turns, err := h.store.query.ListTurns(t.Context(), dao.ListTurnsRequest{
		SessionID: rootID,
		Order:     storage.Order{Direction: storage.DirectionAsc},
		Limit:     10,
	})
	if err != nil || len(turns.Items) < 2 || turns.Items[0].Model.Ref != (defs.ModelRef{
		ProviderID: "test",
		ModelID:    "test",
	}) || turns.Items[1].Model != wantB {
		t.Fatalf("turns = %+v, %v", turns.Items, err)
	}
}

// ── Runtime Server 测试夹具 ──

// testToken 是测试 Server 的访问令牌。
const testToken = "lan-secret-token"

// serverStore 是测试 Runtime 的共用库：records、logs 交给 Runtime，query、mutation 供测试直接读写。
type serverStore struct {
	records  storage.RecordStorage
	logs     storage.LogStorage
	query    dao.Query
	mutation dao.Mutation
}

// openServerStore 创建服务测试使用的记录库、日志和 DAO。
func openServerStore(t *testing.T) *serverStore {
	t.Helper()
	// 1. 创建临时 sqlite 库并登记关闭。
	root := t.TempDir()
	database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	// 2. 创建文件日志存储和 DAO，返回共享库夹具。
	logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(filepath.Join(root, "logs")))
	if err != nil {
		t.Fatal(err)
	}
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
	if err != nil {
		t.Fatal(err)
	}
	return &serverStore{
		records:  database.Record(),
		logs:     logs,
		query:    query,
		mutation: mutation,
	}
}

// newTestRuntimeOpt 创建服务测试的 Agent、模型和 Executor 配置。
func newTestRuntimeOpt(store *serverStore, model *serverModel, defaultDirectory string) *runtime.RuntimeOpt {
	// 1. 配置 assistant、默认模型和可切换模型 B，以及本机 local 和虚拟 a。
	opt := runtime.NewRuntimeOpt(store.records, store.logs, &runtime.Definitions{
		Agents: []*agent.AgentDefinition{{
			Name:         "assistant",
			Description:  "主 Agent",
			Instructions: "Test assistant",
			Model: defs.ModelRef{
				ProviderID: "test",
				ModelID:    "test",
			},
		}},
		Models: []*defs.ModelDefinition{
			{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "test",
				},
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "test",
				},
				Capabilities: defs.ModelCapabilities{Input: []defs.ModelInput{defs.ModelInputText, defs.ModelInputImage}},
			},
			{
				Ref: defs.ModelRef{
					ProviderID: "test",
					ModelID:    "b",
				},
				Target: &llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  model.server.URL,
					Model:    "model-b",
				},
				MaxOutputTokens:        4096,
				ReasoningEfforts:       []string{"low", "high"},
				DefaultReasoningEffort: "low",
			},
		},
		Executors: []*defs.ExecutorDefinition{
			{
				ID:                 defs.LocalExecutorID,
				Kind:               defs.ExecutorKindLocal,
				Description:        "本机",
				WorkspaceDirectory: defaultDirectory,
			},
			{
				ID:                 "a",
				Kind:               defs.ExecutorKindVirtual,
				Description:        "虚拟",
				WorkspaceDirectory: "/workspace",
			},
		},
	})

	return opt
}

// testCall 是测试用假实现收到的一次调用：方法名和它唯一的业务参数，没有参数时为 nil。
type testCall struct {
	method   string
	argument any
}

// testCalls 按到达顺序保存假实现收到的调用，供测试确认 serve 调了哪个方法、交了什么参数；写满后丢弃新的。
type testCalls chan testCall

// record 按到达顺序记录一次测试调用。
func (calls testCalls) record(method string, argument any) {
	// 1. 将方法和参数写入队列，队列已满时丢弃。
	select {
	case calls <- testCall{
		method:   method,
		argument: argument,
	}:
	default:
	}
}

// drain 取出已经到达的全部测试调用。
func (calls testCalls) drain() []testCall {
	// 1. 按到达顺序读取队列，暂无调用时返回已收集项。
	var drained []testCall
	for {
		select {
		case call := <-calls:
			drained = append(drained, call)
		default:
			return drained
		}
	}
}

// testConfiguration 是测试用的 Configuration，把收到的调用记进 calls。
type testConfiguration struct {
	// providers、defaultModel、defaults 是 ListProviders 的返回值。
	providers    []defs.ProviderConfig
	defaultModel defs.ModelRef
	defaults     defs.ModelDefaults
	// agents、models 是写方法和 Reload 返回的定义。
	agents []*agent.AgentDefinition
	models []*defs.ModelDefinition
	// err 非 nil 时全部方法只返回它。
	err   error
	calls testCalls
}

// newTestConfiguration 创建记录调用的配置服务夹具。
func newTestConfiguration() *testConfiguration {
	// 1. 创建容量为 16 的调用队列。
	return &testConfiguration{calls: make(testCalls, 16)}
}

// ListProviders 记录查询并返回预设 Provider、默认模型和模型默认配置。
func (c *testConfiguration) ListProviders(context.Context) ([]defs.ProviderConfig, defs.ModelRef, defs.ModelDefaults, error) {
	// 1. 登记调用；有预设错误时返回错误，否则返回预设配置。
	c.calls.record("ListProviders", nil)
	if c.err != nil {
		return nil, defs.ModelRef{}, defs.ModelDefaults{}, c.err
	}
	return c.providers, c.defaultModel, c.defaults, nil
}

// testProviderWrite 是 testConfiguration.SetProvider 收到的参数。
type testProviderWrite struct {
	provider     defs.ProviderConfig
	defaultModel defs.ModelRef
}

// SetProvider 记录 Provider 写入参数并返回预设定义。
func (c *testConfiguration) SetProvider(_ context.Context, provider defs.ProviderConfig, defaultModel defs.ModelRef) (*runtime.Definitions, error) {
	// 1. 保存 Provider 和默认模型的调用记录，再取预设定义。
	c.calls.record("SetProvider", testProviderWrite{
		provider:     provider,
		defaultModel: defaultModel,
	})
	return c.getDefinitions()
}

// DeleteProvider 记录 Provider 删除参数并返回预设定义。
func (c *testConfiguration) DeleteProvider(_ context.Context, providerID string) (*runtime.Definitions, error) {
	// 1. 登记 Provider ID，再取预设定义。
	c.calls.record("DeleteProvider", providerID)
	return c.getDefinitions()
}

// SetDefaultModel 记录默认模型参数并返回预设定义。
func (c *testConfiguration) SetDefaultModel(_ context.Context, ref defs.ModelRef) (*runtime.Definitions, error) {
	// 1. 登记模型引用，再取预设定义。
	c.calls.record("SetDefaultModel", ref)
	return c.getDefinitions()
}

// Reload 记录重新加载调用并返回预设定义。
func (c *testConfiguration) Reload(context.Context) (*runtime.Definitions, error) {
	// 1. 登记调用，再取预设定义。
	c.calls.record("Reload", nil)
	return c.getDefinitions()
}

// getDefinitions 返回预设 Agent 和模型，或预设错误。
func (c *testConfiguration) getDefinitions() (*runtime.Definitions, error) {
	// 1. 有错误时返回错误，否则将 Agent 和模型封装成定义。
	if c.err != nil {
		return nil, c.err
	}
	return &runtime.Definitions{
		Agents: c.agents,
		Models: c.models,
	}, nil
}

// testModelCatalog 是测试用的 ModelCatalog，把收到的调用记进 calls。
type testModelCatalog struct {
	// providers 是 ListCatalogProviders 的返回值。
	providers []defs.CatalogProvider
	// models 是 ListCatalogModels 和 FetchProviderModels 的返回值。
	models map[string]*defs.ProviderModelConfig
	calls  testCalls
}

// newTestModelCatalog 创建记录调用的模型目录夹具。
func newTestModelCatalog() *testModelCatalog {
	// 1. 创建容量为 16 的调用队列。
	return &testModelCatalog{calls: make(testCalls, 16)}
}

// ListCatalogProviders 记录查询并返回预设模型服务目录。
func (c *testModelCatalog) ListCatalogProviders(context.Context) ([]defs.CatalogProvider, error) {
	// 1. 登记查询调用并返回 Provider 目录。
	c.calls.record("ListCatalogProviders", nil)
	return c.providers, nil
}

// ListCatalogModels 记录服务 ID 并返回预设模型目录。
func (c *testModelCatalog) ListCatalogModels(_ context.Context, catalogProviderID string) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 登记服务 ID 并返回模型映射。
	c.calls.record("ListCatalogModels", catalogProviderID)
	return c.models, nil
}

// FetchProviderModels 记录 Provider 并返回预设上游模型。
func (c *testModelCatalog) FetchProviderModels(_ context.Context, provider defs.ProviderConfig) (map[string]*defs.ProviderModelConfig, error) {
	// 1. 登记完整 Provider 并返回模型映射。
	c.calls.record("FetchProviderModels", provider)
	return c.models, nil
}

// serverModelRequest 是受控模型收到的一次请求：body 是解析后的请求体，测试往 reply 写回复。
type serverModelRequest struct {
	body  map[string]any
	reply chan serverModelResponse
}

// serverModelResponse 是受控模型的一次回复。
type serverModelResponse struct {
	// reasoning 非空时先作为推理增量写出。
	reasoning string
	// chunks 依次作为文本增量写出。
	chunks []string
	// release 非 nil 时写出第一段后等它关闭，让回复停在"生成到一半"。
	release <-chan struct{}
	// calls 是文本之后写出的工具调用。
	calls []llmkit.ToolCall
}

// serverModel 是测试逐次控制回复的流式模型服务：每个请求放进 requests，等测试回复。
type serverModel struct {
	server   *httptest.Server
	requests chan serverModelRequest
}

// newServerModel 启动由测试控制回复的流式模型服务。
func newServerModel(t *testing.T) *serverModel {
	t.Helper()
	// 1. 创建请求队列和 HTTP 服务。
	model := &serverModel{requests: make(chan serverModelRequest, 128)}
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. 解析请求，交给测试并等待回复或请求取消。
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		request := serverModelRequest{
			body:  body,
			reply: make(chan serverModelResponse, 1),
		}
		model.requests <- request
		var response serverModelResponse
		select {
		case response = <-request.reply:
		case <-r.Context().Done():
			return
		}
		// 2. 依次写出推理、文本、工具调用和流结束标记。
		w.Header().Set("Content-Type", "text/event-stream")
		if response.reasoning != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", response.reasoning)
		}
		for index, chunk := range response.chunks {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
			if index == 0 && response.release != nil {
				w.(http.Flusher).Flush()
				select {
				case <-response.release:
				case <-r.Context().Done():
					return
				}
			}
		}
		for index, call := range response.calls {
			fmt.Fprintf(w,
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n",
				index, call.ID, call.Name, call.Arguments,
			)
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	// 2. 登记服务关闭操作并返回模型夹具。
	t.Cleanup(model.server.Close)
	return model
}

// receive 等待受控模型的下一条请求。
func (m *serverModel) receive(t *testing.T) serverModelRequest {
	t.Helper()
	// 1. 最多等待 5 秒，收到请求后返回，超时则结束测试。
	select {
	case request := <-m.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("model request did not arrive")
		return serverModelRequest{}
	}
}

// requireNoRequest 检查短暂等待内没有新模型请求。
func (m *serverModel) requireNoRequest(t *testing.T) {
	t.Helper()
	// 1. 等待 100 毫秒，收到请求则结束测试。
	select {
	case request := <-m.requests:
		t.Fatalf("unexpected model request: %v", request.body)
	case <-time.After(100 * time.Millisecond):
	}
}

// newTestListener 在本机随机端口创建监听器。
func newTestListener(t *testing.T) net.Listener {
	t.Helper()
	// 1. 监听空闲端口，登记测试结束时关闭并返回监听器。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// serverHarness 运行一个真实 Runtime 与监听本机空闲端口的 RuntimeServer；测试结束时先停 Server 再停 Runtime。
type serverHarness struct {
	store *serverStore
	model *serverModel
	// runtime 是 Server 背后的真实 Runtime，供测试换定义。
	runtime runtime.Runtime
	// defaultDirectory 是 local Executor 的默认根目录（serve 的工作目录），其中 .agents/skills 下有一个以目录名为名的 Skill。
	defaultDirectory string
	address          string
	cancel           context.CancelFunc
	// done 收 Server Run 的返回值；runErr 是第一次读到的返回值。
	done   chan error
	runErr error
	// ended 是否已经等到 Server Run 返回？
	ended bool
}

// startTestServer 以默认常驻配置启动 Runtime 和 Server。
func startTestServer(t *testing.T, configure ...func(*runtimeserver.RuntimeServerOpt)) *serverHarness {
	t.Helper()
	// 1. 使用空遥测和零空闲超时，传入可选配置修改。
	return startTestServerWithIdleTimeout(t, nil, 0, configure...)
}

// startTestServerWithTelemetry 启动使用指定遥测的 Runtime 和 Server。
func startTestServerWithTelemetry(t *testing.T, tel *telemetry.Telemetry) *serverHarness {
	t.Helper()
	// 1. 将遥测交给启动夹具，使用零空闲超时。
	return startTestServerWithIdleTimeout(t, tel, 0)
}

// startTestServerWithIdleTimeout 启动指定空闲时长的 Server 及其 Runtime。
func startTestServerWithIdleTimeout(t *testing.T, tel *telemetry.Telemetry, idleTimeout time.Duration, configure ...func(*runtimeserver.RuntimeServerOpt)) *serverHarness {
	t.Helper()
	// 1. 创建共享库、受控模型，把 HOME 换成临时目录（local 会扫主目录的 Skill），在默认目录写入项目 Skill。
	store, model := openServerStore(t), newServerModel(t)
	t.Setenv("HOME", t.TempDir())
	defaultDirectory := t.TempDir()
	writeTestSkill(t, filepath.Join(defaultDirectory, ".agents", "skills"), filepath.Base(defaultDirectory))
	// 2. 创建并启动 Runtime，绑定测试遥测。
	runtimeOpt := newTestRuntimeOpt(store, model, defaultDirectory)
	runtimeOpt.Telemetry = tel
	rt, err := runtime.NewRuntime(runtimeOpt)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- rt.Run(runtimeCtx) }()

	// 3. 分配地址，应用服务配置后创建并启动 Server。
	address := getFreeAddress(t)
	serverOpt := runtimeserver.NewRuntimeServerOpt(rt, address, testToken)
	serverOpt.Configuration, serverOpt.ModelCatalog = newTestConfiguration(), newTestModelCatalog()
	serverOpt.Telemetry = tel
	serverOpt.IdleTimeout = idleTimeout
	for _, apply := range configure {
		apply(serverOpt)
	}
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	h := &serverHarness{
		store:            store,
		model:            model,
		runtime:          rt,
		defaultDirectory: defaultDirectory,
		address:          address,
		cancel:           cancel,
		done:             make(chan error, 1),
	}
	go func() { h.done <- server.Run(serverCtx) }()
	// 4. 登记先停止 Server 再停止 Runtime 的收尾，等待健康检查就绪后返回。
	t.Cleanup(func() {
		h.stop(t)
		stopRuntime()
		<-runtimeDone
	})
	h.waitReady(t)
	return h
}

// stop 停止测试 Server 并等待结束。
func (h *serverHarness) stop(t *testing.T) error {
	t.Helper()
	// 1. 首次调用取消服务并等待最多 10 秒，记录结束状态和错误；重复调用返回首次结果。
	if !h.ended {
		h.cancel()
		select {
		case h.runErr = <-h.done:
		case <-time.After(10 * time.Second):
			t.Fatal("RuntimeServer.Run did not return")
		}
		h.ended = true
	}
	return h.runErr
}

// waitExit 等待 Server 自行退出，返回是否退出和退出错误。
func (h *serverHarness) waitExit(t *testing.T, timeout time.Duration) (exited bool, err error) {
	t.Helper()
	// 1. 已结束时复用结果，否则等到服务返回或超时，超时返回 false。
	if !h.ended {
		select {
		case h.runErr = <-h.done:
			h.ended = true
		case <-time.After(timeout):
			return false, nil
		}
	}
	return true, h.runErr
}

// waitReady 等待 Server 健康检查成功。
func (h *serverHarness) waitReady(t *testing.T) {
	t.Helper()
	// 1. 在 5 秒内轮询 healthz，返回 200 后结束等待。
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get("http://" + h.address + "/api/v1/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server at %s is not ready: %v", h.address, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dial 使用查询参数 token 连接 OpenVaala 协议。
func (h *serverHarness) dial(t *testing.T) *testClient {
	t.Helper()
	// 1. 建立连接，失败则结束测试，成功创建读帧客户端。
	conn, _, err := h.dialWith(t, "/api/v1/openvaala?token="+testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newTestClient(t, conn)
}

// dialWith 按指定路径和请求头连接 WebSocket。
func (h *serverHarness) dialWith(t *testing.T, path string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	// 1. 使用 5 秒超时发起握手，返回连接、HTTP 响应和错误。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws://"+h.address+path, &websocket.DialOptions{HTTPHeader: header})
}

// getFreeAddress 取得本机当前空闲 TCP 地址。
func getFreeAddress(t *testing.T) string {
	t.Helper()
	// 1. 随机端口监听，取出地址后关闭监听器。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// testClient 是测试用 JSON-RPC 客户端：后台读取全部帧，带 id 的进入 responses，通知进入 notifications。
type testClient struct {
	conn          *websocket.Conn
	nextID        int
	responses     chan clientMessage
	notifications chan clientMessage
}

// clientMessage 是客户端读到的一帧响应或通知。
type clientMessage struct {
	// sequence 是帧的到达顺序，用于断言响应与通知的先后。
	sequence int
	ID       json.RawMessage `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	Result   json.RawMessage `json:"result"`
	Error    *clientError    `json:"error"`
}

// clientError 是响应里的错误。
type clientError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// newTestClient 创建后台读取响应与通知的协议客户端。
func newTestClient(t *testing.T, conn *websocket.Conn) *testClient {
	t.Helper()
	// 1. 设置与真实客户端相同的 32 MiB 读取上限（装得下带大图片的历史页），创建响应和通知队列。
	conn.SetReadLimit(32 << 20)
	client := &testClient{
		conn:          conn,
		responses:     make(chan clientMessage, 64),
		notifications: make(chan clientMessage, 4096),
	}
	// 2. 启动后台读帧任务，记录到达次序并按 ID 分到响应或通知队列，退出时关闭队列。
	go func() {
		defer close(client.responses)
		defer close(client.notifications)
		for sequence := 1; ; sequence++ {
			_, frame, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var message clientMessage
			if err := json.Unmarshal(frame, &message); err != nil {
				return
			}
			message.sequence = sequence
			if len(message.ID) == 0 {
				client.notifications <- message
			} else {
				client.responses <- message
			}
		}
	}()
	// 3. 登记测试结束时直接关闭连接。
	t.Cleanup(func() { _ = conn.CloseNow() })
	return client
}

// sendRaw 发送一帧原始请求文本。
func (c *testClient) sendRaw(t *testing.T, frame string) {
	t.Helper()
	// 1. 写入文本帧，失败则结束测试。
	if err := c.conn.Write(t.Context(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatal(err)
	}
}

// call 发送请求并等待对应响应。
func (c *testClient) call(t *testing.T, method string, params any) clientMessage {
	t.Helper()
	// 1. 递增请求 ID，编码方法与参数。
	c.nextID++
	payload, err := json.Marshal(map[string]any{"id": c.nextID, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	// 2. 发送文本帧，再读取下一条串行响应。
	c.sendRaw(t, string(payload))
	return c.nextResponse(t)
}

// nextResponse 读取下一条协议响应。
func (c *testClient) nextResponse(t *testing.T) clientMessage {
	t.Helper()
	// 1. 最多等待 5 秒，连接提前关闭或超时则结束测试。
	select {
	case response, open := <-c.responses:
		if !open {
			t.Fatal("connection closed before the response")
		}
		return response
	case <-time.After(5 * time.Second):
		t.Fatal("response did not arrive")
		return clientMessage{}
	}
}

// callResult 调用协议方法并解析成功结果。
func (c *testClient) callResult(t *testing.T, method string, params any, result any) clientMessage {
	t.Helper()
	// 1. 发送请求，错误响应则结束测试。
	response := c.call(t, method, params)
	if response.Error != nil {
		t.Fatalf("%s error = %+v", method, response.Error)
	}
	// 2. 需要结果时解码到指定对象，返回原响应。
	if result != nil {
		if err := json.Unmarshal(response.Result, result); err != nil {
			t.Fatal(err)
		}
	}
	return response
}

// initialize 使用测试客户端身份完成协议初始化。
func (c *testClient) initialize(t *testing.T) {
	t.Helper()
	// 1. 发送客户端名称和协议版本 5。
	c.callResult(t, "initialize", map[string]any{
		"client_info":      map[string]any{"name": "test"},
		"protocol_version": "5",
	}, nil)
}

// createConversation 通过协议创建 assistant 对话。
func (c *testClient) createConversation(t *testing.T) defs.Conversation {
	t.Helper()
	// 1. 发送创建请求并解析对话记录。
	var result struct {
		Conversation defs.Conversation `json:"conversation"`
	}
	c.callResult(t, "conversation/create", map[string]any{"agent_name": "assistant"}, &result)
	return result.Conversation
}

// loggedItemView 是 session/items/list 和 items/saved 里的一条日志。
type loggedItemView struct {
	Ordinal storage.Ordinal `json:"ordinal"`
	Item    *defs.Item      `json:"item"`
}

// waitItems 等待 Session 日志达到指定条数。
func (c *testClient) waitItems(t *testing.T, conversation defs.Conversation, sessionID defs.SessionID, count int) []loggedItemView {
	t.Helper()
	// 1. 在 5 秒内轮询最多 100 条日志，达到条数后返回，否则超时结束测试。
	deadline := time.Now().Add(5 * time.Second)
	for {
		var result struct {
			Items []loggedItemView `json:"items"`
		}
		c.callResult(t, "session/items/list", map[string]any{
			"conversation_id": conversation.ID,
			"session_id":      sessionID,
			"limit":           100,
		}, &result)
		if len(result.Items) >= count {
			return result.Items
		}
		if time.Now().After(deadline) {
			t.Fatalf("session log has %d items, want %d", len(result.Items), count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeTestSkill 在指定目录写入测试 Skill。
func writeTestSkill(t *testing.T, directory, name string) {
	t.Helper()
	// 1. 创建 Skill 目录，写入名称和测试说明。
	path := filepath.Join(directory, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: "+name+" 的测试 Skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
