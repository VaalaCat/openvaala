package runtimeserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TestServerConnectionProtocol 检查初始化、请求格式、错误响应和慢请求心跳。
func TestServerConnectionProtocol(t *testing.T) {
	// 1. 准备只给 slow_request_keeps_answering_pings 用的慢模型目录（catalog/providers/list 阻塞到测试放开），
	// 列出初始化、帧格式与慢目录查询场景。
	slowCatalog := &blockingModelCatalog{
		testModelCatalog: newTestModelCatalog(),
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	tests := []struct {
		name string
		// configure 非 nil 时在构造 RuntimeServer 前修改它的配置。
		configure func(opt *runtimeserver.RuntimeServerOpt)
		run       func(t *testing.T, client *testClient)
	}{
		{
			name: "request_before_initialize",
			run: func(t *testing.T, client *testClient) {
				requireErrorCode(t, client.call(t, "conversation/list", nil), -32600, "not initialized")
			},
		},
		{
			name: "duplicate_initialize",
			run: func(t *testing.T, client *testClient) {
				client.initialize(t)
				requireErrorCode(t, client.call(t, "initialize", map[string]any{"protocol_version": "5"}), -32600, "already initialized")
			},
		},
		{
			// 协议版本 4 提供 Turn 查询和手动压缩，版本 3 初始化被拒绝后仍可重试。
			name: "unsupported_protocol_version_then_retry",
			run: func(t *testing.T, client *testClient) {
				requireErrorCode(t, client.call(t, "initialize", map[string]any{"protocol_version": "3"}), -32600, "unsupported protocol version")
				var result struct {
					ProtocolVersion string `json:"protocol_version"`
				}
				client.callResult(t, "initialize", map[string]any{"protocol_version": "5"}, &result)
				if result.ProtocolVersion != "5" {
					t.Fatalf("protocol_version = %q, want 5", result.ProtocolVersion)
				}
			},
		},
		{
			name: "unknown_method",
			run: func(t *testing.T, client *testClient) {
				client.initialize(t)
				requireErrorCode(t, client.call(t, "conversation/unknown", nil), -32601, "method not found")
			},
		},
		{
			name: "invalid_json_keeps_connection",
			run: func(t *testing.T, client *testClient) {
				client.sendRaw(t, "{not json")
				response := client.nextResponse(t)
				requireErrorCode(t, response, -32700, "parse error")
				if string(response.ID) != "null" {
					t.Fatalf("parse error id = %s, want null", response.ID)
				}
				client.initialize(t)
			},
		},
		{
			name: "missing_method_rejected",
			run: func(t *testing.T, client *testClient) {
				client.sendRaw(t, `{"id":7}`)
				response := client.nextResponse(t)
				requireErrorCode(t, response, -32600, "request requires id and method")
				if string(response.ID) != "7" {
					t.Fatalf("error id = %s, want 7", response.ID)
				}
			},
		},
		{
			name: "missing_id_and_method_rejected",
			run: func(t *testing.T, client *testClient) {
				client.sendRaw(t, `{}`)
				response := client.nextResponse(t)
				requireErrorCode(t, response, -32600, "request requires id and method")
				if string(response.ID) != "null" {
					t.Fatalf("error id = %s, want null", response.ID)
				}
			},
		},
		{
			// 无 ID 的 initialize 通知不初始化也不响应，后续初始化请求独立完成并返回对应 ID。
			name: "notification_gets_no_response",
			run: func(t *testing.T, client *testClient) {
				client.sendRaw(t, `{"method":"initialize","params":{"protocol_version":"5"}}`)
				response := client.call(t, "initialize", map[string]any{"protocol_version": "5"})
				if response.Error != nil || string(response.ID) != "1" {
					t.Fatalf("response = %+v (error %+v), want the initialize request to succeed with id 1", response, response.Error)
				}
			},
		},
		{
			name: "invalid_params",
			run: func(t *testing.T, client *testClient) {
				client.initialize(t)
				requireErrorCode(t, client.call(t, "conversation/list", map[string]any{"limit": "many"}), -32602, "")
			},
		},
		{
			name: "runtime_error",
			run: func(t *testing.T, client *testClient) {
				client.initialize(t)
				requireErrorCode(t, client.call(t, "model/list", map[string]any{"agent_name": "missing"}), -32000, "not configured")
			},
		},
		{
			// 目录请求阻塞时连接继续处理 ping 和 pong；放行后原请求正常返回。
			name:      "slow_request_keeps_answering_pings",
			configure: func(opt *runtimeserver.RuntimeServerOpt) { opt.ModelCatalog = slowCatalog },
			run: func(t *testing.T, client *testClient) {
				client.initialize(t)
				client.sendRaw(t, `{"id":"slow","method":"catalog/providers/list"}`)
				select {
				case <-slowCatalog.started:
				case <-time.After(5 * time.Second):
					t.Fatal("catalog/providers/list did not reach the model catalog")
				}
				pingCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				err := client.conn.Ping(pingCtx)
				cancel()
				if err != nil {
					t.Fatalf("ping while a request is blocked = %v, want a pong", err)
				}
				close(slowCatalog.release)
				if response := client.nextResponse(t); response.Error != nil || string(response.ID) != `"slow"` {
					t.Fatalf("response = %+v (error %+v), want the blocked request to succeed", response, response.Error)
				}
			},
		},
	}
	// 2. 发送各类帧并阻塞目录请求，检查初始化限制、响应 ID、错误码、通知丢弃和心跳继续响应。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var configure []func(*runtimeserver.RuntimeServerOpt)
			if tt.configure != nil {
				configure = append(configure, tt.configure)
			}
			tt.run(t, startTestServer(t, configure...).dial(t))
		})
	}
}

// blockingModelCatalog 是测试用的 ModelCatalog：ListCatalogProviders 关闭 started 后等 release 关闭才返回，模拟首次下载 models.dev 的慢请求。
type blockingModelCatalog struct {
	*testModelCatalog
	started chan struct{}
	release chan struct{}
}

// ListCatalogProviders 等待测试放行后返回模型目录。
func (c *blockingModelCatalog) ListCatalogProviders(ctx context.Context) ([]defs.CatalogProvider, error) {
	// 1. 通知测试调用已开始，等待 release 或上下文结束。
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// 2. 放行后返回测试目录。
	return c.testModelCatalog.ListCatalogProviders(ctx)
}

// TestServerConnectionTelemetry 检查 RPC、连接属性、错误日志和通知丢弃。
func TestServerConnectionTelemetry(t *testing.T) {
	// 1. 列出 RPC、连接属性、错误日志和通知丢弃的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline)
	}{
		{
			name: "submit_input_span_is_turn_parent",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.initialize(t)
				conversation := client.createConversation(t)
				var submitted struct {
					ItemID defs.ItemID `json:"item_id"`
				}
				client.callResult(t, "conversation/submitInput", map[string]any{
					"conversation_id": conversation.ID,
					"content":         []map[string]any{{"kind": "text", "text": map[string]any{"text": "你好"}}},
				}, &submitted)
				h.model.receive(t).reply <- serverModelResponse{chunks: []string{"好"}}
				rpcSpan := p.WaitEndedSpans(t, "conversation/submitInput", 1)[0]
				turnSpan := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
				if turnSpan.Parent().SpanID() != rpcSpan.SpanContext().SpanID() {
					t.Fatalf("turn parent = %s, want rpc span %s", turnSpan.Parent().SpanID(), rpcSpan.SpanContext().SpanID())
				}
				if rpcSpan.SpanKind() != trace.SpanKindServer || telemetrytest.GetSpanAttribute(rpcSpan, "openvaala.connection.id") == "" {
					t.Fatalf("rpc span = %v %v", rpcSpan.SpanKind(), rpcSpan.Attributes())
				}
				host, port, err := net.SplitHostPort(h.address)
				if err != nil {
					t.Fatal(err)
				}
				wantPort, err := strconv.Atoi(port)
				if err != nil {
					t.Fatal(err)
				}
				if got := telemetrytest.GetSpanAttribute(rpcSpan, "server.address"); got != host {
					t.Errorf("server.address = %q, want %q", got, host)
				}
				if value, _ := telemetrytest.GetSpanValue(rpcSpan, "server.port"); value.AsInt64() != int64(wantPort) {
					t.Errorf("server.port = %v, want %d", value.Emit(), wantPort)
				}
				if got := telemetrytest.GetSpanAttribute(rpcSpan, "network.peer.address"); got != "127.0.0.1" {
					t.Errorf("network.peer.address = %q, want 127.0.0.1", got)
				}
				if value, _ := telemetrytest.GetSpanValue(rpcSpan, "network.peer.port"); value.AsInt64() == 0 {
					t.Errorf("network.peer.port = %v, want a non-zero client port", value.Emit())
				}
				if telemetrytest.GetSpanAttribute(turnSpan, "network.peer.address") != "" {
					t.Fatalf("turn span inherited connection attributes: %v", turnSpan.Attributes())
				}
				if got := p.GetHistogramCount(t, "rpc.server.call.duration", attribute.String("rpc.method", "conversation/submitInput")); got != 1 {
					t.Fatalf("submitInput duration records = %d, want 1", got)
				}
			},
		},
		{
			name: "failed_request_logs_error",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.initialize(t)
				requireErrorCode(t, client.call(t, "conversation/read", map[string]any{"conversation_id": "missing"}), -32002, "not found")
				record := p.WaitLogRecords(t, "rpc request failed", 1)[0]
				if record["level"] != "ERROR" || record["rpc.response.status_code"] != "-32002" || record["rpc.method"] != "conversation/read" {
					t.Fatalf("log = %v", record)
				}
			},
		},
		{
			// 未注册的方法名由客户端任意给出：span 名为 jsonrpc，指标维度为 _OTHER，原名记在 rpc.method_original。
			name: "unknown_method_uses_jsonrpc_span_name",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.initialize(t)
				requireErrorCode(t, client.call(t, "conversation/unknown", nil), -32601, "method not found")
				span := p.WaitEndedSpans(t, "jsonrpc", 1)[0]
				if telemetrytest.GetSpanAttribute(span, "rpc.method_original") != "conversation/unknown" {
					t.Fatalf("attributes = %v", span.Attributes())
				}
				for _, span := range p.GetEndedSpans() {
					if span.Name() == "conversation/unknown" {
						t.Fatalf("rpc span named after the unknown method: %v", span.Attributes())
					}
				}
				if got := p.GetHistogramCount(t, "rpc.server.call.duration", attribute.String("rpc.method", "_OTHER")); got != 1 {
					t.Fatalf("_OTHER duration records = %d, want 1", got)
				}
				if got := p.GetHistogramCount(t, "rpc.server.call.duration", attribute.String("rpc.method", "conversation/unknown")); got != 0 {
					t.Fatalf("unknown method duration records = %d, want 0", got)
				}
			},
		},
		{
			name: "string_request_id_is_recorded_unquoted",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.sendRaw(t, `{"id":"req-1","method":"initialize","params":{"client_info":{"name":"test"},"protocol_version":"5"}}`)
				if response := client.nextResponse(t); response.Error != nil || string(response.ID) != `"req-1"` {
					t.Fatalf("response = %+v, want success with id \"req-1\"", response)
				}
				span := p.WaitEndedSpans(t, "initialize", 1)[0]
				if got := telemetrytest.GetSpanAttribute(span, "jsonrpc.request.id"); got != "req-1" {
					t.Fatalf("request id attribute = %q, want req-1", got)
				}
			},
		},
		{
			// 不认识的通知方法记 Warn 后丢弃，没有响应，也不产生 rpc span。
			name: "unsupported_notification_logs_warning",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.initialize(t)
				client.sendRaw(t, `{"method":"foo/bar","params":{}}`)
				record := p.WaitLogRecords(t, "notification dropped", 1)[0]
				if record["level"] != "WARN" || record["rpc.method"] != "foo/bar" {
					t.Fatalf("log = %v, want WARN for foo/bar", record)
				}
				if response := client.call(t, "conversation/list", nil); response.Error != nil || string(response.ID) != "2" {
					t.Fatalf("response after the notification = %+v (error %+v), want the next request's own response", response, response.Error)
				}
				for _, span := range p.GetEndedSpans() {
					if span.Name() == "foo/bar" || telemetrytest.GetSpanAttribute(span, "rpc.method_original") == "foo/bar" {
						t.Fatalf("rpc span for a notification: %v", span.Attributes())
					}
				}
			},
		},
		{
			name: "undecodable_client_mcp_notification_logs_warning",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.initialize(t)
				client.sendRaw(t, `{"method":"clientMCP/message","params":"not-an-object"}`)
				client.sendRaw(t, `{"method":"clientMCP/closed"}`)
				records := p.WaitLogRecords(t, "notification dropped", 2)
				if records[0]["rpc.method"] != "clientMCP/message" || records[1]["rpc.method"] != "clientMCP/closed" || records[0]["error"] == nil {
					t.Fatalf("logs = %v, want a dropped clientMCP/message then clientMCP/closed", records)
				}
			},
		},
		{
			// 流已经结束后迟到的 clientMCP/message、clientMCP/closed 是正常的，静默丢弃。
			name: "late_client_mcp_notification_is_dropped_silently",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.initialize(t)
				client.sendRaw(t, `{"method":"clientMCP/message","params":{"stream_id":"gone","message":{"jsonrpc":"2.0","method":"ping"}}}`)
				client.sendRaw(t, `{"method":"clientMCP/closed","params":{"stream_id":"gone","error":"late"}}`)
				// 请求在通知之后串行处理：它的响应到达时前面的通知已处理完。
				client.callResult(t, "conversation/list", nil, nil)
				if got := p.GetLogRecords(t, "notification dropped"); len(got) != 0 {
					t.Fatalf("notification dropped logs = %v, want none", got)
				}
			},
		},
		{
			name: "unparsable_frame_has_no_rpc_span",
			run: func(t *testing.T, h *serverHarness, client *testClient, p *telemetrytest.Pipeline) {
				client.sendRaw(t, "{not json")
				requireErrorCode(t, client.nextResponse(t), -32700, "parse error")
				for _, span := range p.GetEndedSpans() {
					if span.SpanKind() == trace.SpanKindServer {
						t.Fatalf("rpc span %q ended for an unparsable frame", span.Name())
					}
				}
			},
		},
	}
	// 2. 建立受控连接，检查 RPC 父 span、请求 ID、错误日志和无 ID 通知不产生 span。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			h := startTestServerWithTelemetry(t, p.Telemetry)
			tt.run(t, h, h.dial(t), p)
		})
	}
}

// requireErrorCode 检查响应错误码和文本。
func requireErrorCode(t *testing.T, response clientMessage, wantCode int, wantMessage string) {
	t.Helper()
	// 1. 比较错误码和目标文本，不匹配则结束测试。
	if response.Error == nil || response.Error.Code != wantCode || !strings.Contains(response.Error.Message, wantMessage) {
		t.Fatalf("response = %+v (error %+v), want code %d containing %q", response, response.Error, wantCode, wantMessage)
	}
}

// TestServerConnectionClientMCP 检查客户端 MCP 双向消息、流分发和断开收尾。
func TestServerConnectionClientMCP(t *testing.T) {
	// 1. 列出客户端 MCP 双向消息、流分发和断开收尾的测试场景。
	tests := []struct {
		name string
		run  func(t *testing.T, f *clientMCPFixture)
	}{
		{
			// serve 写的消息作为 clientMCP/message 发给客户端（并发写互不干扰），客户端发来的消息交给对应流的 Read；不认识的 stream_id 丢弃。
			name: "messages_flow_in_both_directions",
			run: func(t *testing.T, f *clientMCPFixture) {
				stream := f.connect(t, "conv-1", "fake")
				const writers = 8
				var wg sync.WaitGroup
				for index := range writers {
					wg.Go(func() {
						if err := stream.connection.Write(t.Context(), newMCPNotification(t, "notifications/from-serve-"+strconv.Itoa(index))); err != nil {
							t.Errorf("Write() error = %v", err)
						}
					})
				}
				wg.Wait()
				var methods []string
				for _, message := range f.client.collectNotifications(t, isMethodTimes("clientMCP/message", writers)) {
					if message.Method != "clientMCP/message" {
						continue
					}
					var params struct {
						StreamID string `json:"stream_id"`
						Message  struct {
							Method string `json:"method"`
						} `json:"message"`
					}
					if err := json.Unmarshal(message.Params, &params); err != nil || params.StreamID != stream.streamID {
						t.Fatalf("clientMCP/message params = %s (error %v), want stream %s", message.Params, err, stream.streamID)
					}
					methods = append(methods, params.Message.Method)
				}
				slices.Sort(methods)
				var wantMethods []string
				for index := range writers {
					wantMethods = append(wantMethods, "notifications/from-serve-"+strconv.Itoa(index))
				}
				slices.Sort(wantMethods)
				if !slices.Equal(methods, wantMethods) {
					t.Fatalf("messages sent to the client = %v, want %v", methods, wantMethods)
				}

				f.client.sendRaw(t, `{"method":"clientMCP/message","params":{"stream_id":"unknown","message":{"jsonrpc":"2.0","method":"notifications/lost"}}}`)
				f.client.sendRaw(t, `{"method":"clientMCP/message","params":{"stream_id":"`+stream.streamID+`","message":{"jsonrpc":"2.0","method":"notifications/from-client"}}}`)
				if got := stream.read(t); got != "notifications/from-client" {
					t.Fatalf("message read from the stream = %q, want notifications/from-client", got)
				}
			},
		},
		{
			// 同一个 server id 登记在两个对话里：连接请求只带 server_id，消息要靠 stream_id 找到各自的传输。
			name: "same_server_id_routes_by_stream_id",
			run: func(t *testing.T, f *clientMCPFixture) {
				first := f.connect(t, "conv-1", "fake")
				second := f.connect(t, "conv-2", "fake")
				if first.streamID == second.streamID {
					t.Fatalf("both conversations connected on stream %q, want one stream each", first.streamID)
				}
				f.client.sendRaw(t, `{"method":"clientMCP/message","params":{"stream_id":"`+second.streamID+`","message":{"jsonrpc":"2.0","method":"notifications/for-second"}}}`)
				f.client.sendRaw(t, `{"method":"clientMCP/message","params":{"stream_id":"`+first.streamID+`","message":{"jsonrpc":"2.0","method":"notifications/for-first"}}}`)
				if got := second.read(t); got != "notifications/for-second" {
					t.Fatalf("second stream read %q, want notifications/for-second", got)
				}
				if got := first.read(t); got != "notifications/for-first" {
					t.Fatalf("first stream read %q, want notifications/for-first", got)
				}
			},
		},
		{
			// clientMCP/closed 结束流：带 error 时 Read 返回该文本，不带时是正常结束（io.EOF）。
			name: "closed_notification_ends_the_stream",
			run: func(t *testing.T, f *clientMCPFixture) {
				failed := f.connect(t, "conv-1", "fake")
				finished := f.connect(t, "conv-2", "fake")
				f.client.sendRaw(t, `{"method":"clientMCP/closed","params":{"stream_id":"`+failed.streamID+`","error":"server exited"}}`)
				f.client.sendRaw(t, `{"method":"clientMCP/closed","params":{"stream_id":"`+finished.streamID+`"}}`)
				if err := failed.readError(t); err == nil || err.Error() != "server exited" {
					t.Fatalf("failed stream error = %v, want server exited", err)
				}
				if err := finished.readError(t); !errors.Is(err, io.EOF) {
					t.Fatalf("finished stream error = %v, want io.EOF", err)
				}
			},
		},
		{
			// serve 一侧关闭流：通知客户端断开。
			name: "closing_the_stream_notifies_the_client",
			run: func(t *testing.T, f *clientMCPFixture) {
				stream := f.connect(t, "conv-1", "fake")
				if err := stream.connection.Close(); err != nil {
					t.Fatal(err)
				}
				notifications := f.client.collectNotifications(t, isMethod("clientMCP/disconnect"))
				var params struct {
					StreamID string `json:"stream_id"`
				}
				if err := json.Unmarshal(notifications[len(notifications)-1].Params, &params); err != nil || params.StreamID != stream.streamID {
					t.Fatalf("clientMCP/disconnect params = %s (error %v), want stream %s", notifications[len(notifications)-1].Params, err, stream.streamID)
				}
			},
		},
		{
			// 连接断开：未结束的流以连接结束的原因失败，登记过的对话各移除一次（清空过的不再移除），之后同一个传输连不上。
			name: "connection_close_ends_streams_and_removes_registrations",
			run: func(t *testing.T, f *clientMCPFixture) {
				stream := f.connect(t, "conv-1", "fake")
				f.register(t, "conv-2", "fake")
				f.register(t, "conv-3", "fake")
				f.register(t, "conv-3")
				if err := f.client.conn.CloseNow(); err != nil {
					t.Fatal(err)
				}
				// 失败原因是连接真实的结束原因（读帧失败），不只是 context canceled。
				if err := stream.readError(t); err == nil || !strings.Contains(err.Error(), "client connection closed") || !strings.Contains(err.Error(), "read frame") {
					t.Fatalf("stream error = %v, want one saying the client connection closed because of a read frame failure", err)
				}
				var removed []defs.ConversationID
				for range 2 {
					request := f.runtime.receive(t)
					if request.ClientID != stream.clientID || len(request.Servers) != 0 {
						t.Fatalf("removal request = %+v, want an empty registration from client %s", request, stream.clientID)
					}
					removed = append(removed, request.ConversationID)
				}
				slices.Sort(removed)
				if !slices.Equal(removed, []defs.ConversationID{"conv-1", "conv-2"}) {
					t.Fatalf("removed conversations = %v, want [conv-1 conv-2]", removed)
				}
				select {
				case request := <-f.runtime.requests:
					t.Fatalf("unexpected removal request %+v", request)
				case <-time.After(100 * time.Millisecond):
				}
				connectCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if _, err := stream.transport.Connect(connectCtx); err == nil || connectCtx.Err() != nil {
					t.Fatalf("Connect() after the connection ended = %v (context error %v), want an immediate error", err, connectCtx.Err())
				}
			},
		},
	}
	// 2. 通过转发流双向读写，检查同名 server 的 stream 隔开、关闭原因和连接断开后的登记清理。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &clientMCPRuntime{
				isRecordOnly: true,
				requests:     make(chan *runtime.SetClientMCPServersRequest, 16),
			}
			h := startTestServer(t, func(opt *runtimeserver.RuntimeServerOpt) {
				rt.Runtime = opt.Runtime
				opt.Runtime = rt
			})
			client := h.dial(t)
			client.initialize(t)
			tt.run(t, &clientMCPFixture{
				client:  client,
				runtime: rt,
			})
		})
	}
}

// TestServerConnectionClientMCPRemovalErrors 检查断开后移除客户端 MCP 的错误日志。
func TestServerConnectionClientMCPRemovalErrors(t *testing.T) {
	// 1. 列出断开后移除客户端 MCP 的错误日志的测试场景。
	tests := []struct {
		name        string
		err         error
		wantWarning bool
	}{
		{
			name: "conversation_not_open",
			err:  runtime.ErrConversationNotOpen,
		},
		{
			name: "runtime_stopped",
			err:  runtime.ErrRuntimeStopped,
		},
		{
			name:        "other_error",
			err:         errors.New("storage unavailable"),
			wantWarning: true,
		},
	}
	// 2. 注入不同移除错误并断开连接，检查已停止或未打开错误不记日志，其余错误记 Warn。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			rt := &clientMCPRuntime{
				isRecordOnly: true,
				removeErr:    tt.err,
				requests:     make(chan *runtime.SetClientMCPServersRequest, 16),
			}
			h := startTestServerWithIdleTimeout(t, p.Telemetry, 0, func(opt *runtimeserver.RuntimeServerOpt) {
				rt.Runtime = opt.Runtime
				opt.Runtime = rt
			})
			client := h.dial(t)
			client.initialize(t)
			f := &clientMCPFixture{
				client:  client,
				runtime: rt,
			}
			f.register(t, "conv-1", "fake")
			if err := client.conn.CloseNow(); err != nil {
				t.Fatal(err)
			}
			if removal := rt.receive(t); removal.ConversationID != "conv-1" || len(removal.Servers) != 0 {
				t.Fatalf("removal request = %+v, want an empty registration for conv-1", removal)
			}
			// "connection closed" 在 Run 返回之后记录，此时移除登记的结果已处理。
			p.WaitLogRecords(t, "connection closed", 1)
			records := p.GetLogRecords(t, "remove client MCP servers failed")
			if !tt.wantWarning {
				if len(records) != 0 {
					t.Fatalf("removal failure logs = %v, want none for %v", records, tt.err)
				}
				return
			}
			if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["gen_ai.conversation.id"] != "conv-1" {
				t.Fatalf("removal failure logs = %v, want one WARN for conv-1", records)
			}
		})
	}
}

// clientMCPFixture 是一条已初始化的客户端连接和只记录登记的 Runtime。
type clientMCPFixture struct {
	client  *testClient
	runtime *clientMCPRuntime
}

// register 通过协议为对话登记客户端 MCP server。
func (f *clientMCPFixture) register(t *testing.T, conversationID defs.ConversationID, serverIDs ...string) *runtime.SetClientMCPServersRequest {
	t.Helper()
	// 1. 提交 server ID 列表，返回 Runtime 收到的登记请求。
	f.client.callResult(t, "conversation/setClientMCPServers", map[string]any{"conversation_id": conversationID, "server_ids": serverIDs}, nil)
	return f.runtime.receive(t)
}

// connect 登记客户端 MCP server 并建立一条测试转发流。
func (f *clientMCPFixture) connect(t *testing.T, conversationID defs.ConversationID, serverID string) *clientMCPStream {
	t.Helper()
	// 1. 登记 server，从 Runtime 请求中取出传输并连接。
	request := f.register(t, conversationID, serverID)
	transport := request.Servers[0].Transport
	connection, err := transport.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 2. 登记流关闭操作，等待客户端连接通知，返回流标识和客户端 ID。
	t.Cleanup(func() { _ = connection.Close() })
	params := f.client.waitClientMCPConnect(t, serverID)
	return &clientMCPStream{
		transport:  transport,
		connection: connection,
		streamID:   params.StreamID,
		clientID:   request.ClientID,
	}
}

// clientMCPStream 是测试里扮演 Runtime 连上的一条转发流。
type clientMCPStream struct {
	transport  mcp.Transport
	connection mcp.Connection
	streamID   string
	clientID   string
}

// read 读取转发流下一条请求的方法名。
func (s *clientMCPStream) read(t *testing.T) string {
	t.Helper()
	// 1. 限时 5 秒读取 MCP 消息，检查类型是请求并返回 Method。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	message, err := s.connection.Read(ctx)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	request, ok := message.(*jsonrpc.Request)
	if !ok {
		t.Fatalf("message = %T, want a request", message)
	}
	return request.Method
}

// readError 读取转发流结束时的错误。
func (s *clientMCPStream) readError(t *testing.T) error {
	t.Helper()
	// 1. 限时读取，检查发生流错误而不是超时后返回错误。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := s.connection.Read(ctx)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("Read() error = %v (context error %v), want the stream to end", err, ctx.Err())
	}
	return err
}

// newMCPNotification 创建指定方法的 MCP 通知。
func newMCPNotification(t *testing.T, method string) jsonrpc.Message {
	t.Helper()
	// 1. 解码无 ID 的 JSON-RPC 消息并返回，失败则结束测试。
	message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"` + method + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// isMethodTimes 创建检查指定方法通知累计次数的条件。
func isMethodTimes(method string, times int) func(clientMessage) bool {
	// 1. 返回计数匹配函数，指定通知每到一次累加，达到目标次数返回 true。
	seen := 0
	return func(message clientMessage) bool {
		if message.Method == method {
			seen++
		}
		return seen >= times
	}
}
