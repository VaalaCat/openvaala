package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	acp "github.com/coder/acp-go-sdk"
)

// acpWire 是测试扮演的编辑器：通过真实的 stdio 管道收发 JSON-RPC 消息，按顺序记录收到的 session/update 通知。
type acpWire struct {
	input    io.WriteCloser
	messages chan json.RawMessage
	updates  []acp.SessionNotification
	nextID   int
	// readingAllowed 里有令牌时读循环照常读取 ACP 输出；pauseReading 取走令牌，模拟编辑器暂停读取。
	readingAllowed chan struct{}
	isPaused       bool
	// stop 关闭 stdin、等 acpAgent 正常结束；startACPHost 的通道随后再停掉它连接的 Runtime Server 与 Runtime。
	stop func()
	// stopServer 只停掉 Runtime Server，模拟 serve 退出。
	stopServer func()
	// waitHost 等 acpAgent.Run 返回并返回它的结果；测试取走结果后由测试自己断言，stop 不再检查。
	waitHost func(t *testing.T) error
	// pipeline 是 acpAgent、Runtime 与 Runtime Server 共用的内存遥测。
	pipeline *telemetrytest.Pipeline
	// runtime 是 Runtime Server 托管的 Runtime，测试可以直接订阅它观察 Turn 的进度。
	runtime runtime.Runtime
}

// acpWireMessage 保存测试通道解析出的响应和通知。
type acpWireMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *acpError       `json:"error"`
}

// newACPWire 创建测试编辑器，后台逐行读取 acpAgent 的输出。
func newACPWire(t *testing.T, input io.WriteCloser, output io.ReadCloser) *acpWire {
	t.Helper()
	// 1. 创建消息缓冲和读取令牌，启动可暂停的读取任务。
	wire := &acpWire{
		input:          input,
		messages:       make(chan json.RawMessage, 4096),
		readingAllowed: make(chan struct{}, 1),
	}
	wire.readingAllowed <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(wire.messages)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
		for {
			select {
			case token := <-wire.readingAllowed:
				wire.readingAllowed <- token
			case <-ctx.Done():
				return
			}
			if !scanner.Scan() {
				return
			}
			select {
			case wire.messages <- append(json.RawMessage(nil), scanner.Bytes()...):
			case <-ctx.Done():
				return
			}
		}
	}()
	// 2. 测试结束时恢复读取、取消读取任务、关闭输出并等任务退出。
	t.Cleanup(func() { wire.resumeReading(); cancel(); _ = output.Close(); <-done })
	return wire
}

// pauseReading 让测试编辑器停止读取输出。
func (w *acpWire) pauseReading() {
	// 1. 还没暂停时取走读取令牌并标记暂停，读循环读完手上这一行后停住。
	if !w.isPaused {
		<-w.readingAllowed
		w.isPaused = true
	}
}

// resumeReading 让测试编辑器恢复读取输出。
func (w *acpWire) resumeReading() {
	// 1. 已暂停时交回读取令牌并清除暂停标记。
	if w.isPaused {
		w.readingAllowed <- struct{}{}
		w.isPaused = false
	}
}

// send 发送 ACP 请求并返回请求 ID。
func (w *acpWire) send(t *testing.T, method string, params any) int {
	t.Helper()
	// 1. 分配递增的请求 ID，把请求写成一行 JSON 写进 stdin。
	w.nextID++
	if err := json.NewEncoder(w.input).Encode(map[string]any{"jsonrpc": "2.0", "id": w.nextID, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
	return w.nextID
}

// notify 发送不带 ID 的 ACP 通知。
func (w *acpWire) notify(t *testing.T, method string, params any) {
	t.Helper()
	// 1. 把通知写成一行 JSON 写进 stdin。
	if err := json.NewEncoder(w.input).Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
}

// next 读取下一条输出消息，session/update 同时记进 updates。
func (w *acpWire) next(t *testing.T) acpWireMessage {
	t.Helper()
	// 1. 最多等 10 秒读一条消息并解析，session/update 追加到 updates；输出关闭或超时时结束测试。
	select {
	case raw, ok := <-w.messages:
		if !ok {
			t.Fatal("ACP output closed")
		}
		var message acpWireMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatalf("stdout is not protocol JSON: %s (%v)", raw, err)
		}
		if message.Method == "session/update" {
			var update acp.SessionNotification
			if err := json.Unmarshal(message.Params, &update); err != nil {
				t.Fatalf("invalid ACP update: %s (%v)", raw, err)
			}
			w.updates = append(w.updates, update)
		}
		return message
	case <-time.After(10 * time.Second):
		t.Fatal("ACP message timed out")
	}
	return acpWireMessage{}
}

// response 一直读到 id 对应的响应。
func (w *acpWire) response(t *testing.T, id int) acpWireMessage {
	t.Helper()
	// 1. 持续读取，期间的通知照常记录；读到 id 的响应时返回，读到别的请求的响应时结束测试。
	for {
		message := w.next(t)
		if message.ID == id {
			return message
		}
		if message.ID != 0 {
			t.Fatalf("unexpected response ID %d, waiting for %d", message.ID, id)
		}
	}
}

// call 发送请求并等它成功，result 不为 nil 时解析响应结果。
func (w *acpWire) call(t *testing.T, method string, params, result any) {
	t.Helper()
	// 1. 发送请求并等响应，返回错误时结束测试，result 不为 nil 时解析结果。
	message := w.response(t, w.send(t, method, params))
	if message.Error != nil {
		t.Fatalf("%s: %+v", method, message.Error)
	}
	if result != nil {
		if err := json.Unmarshal(message.Result, result); err != nil {
			t.Fatal(err)
		}
	}
}

// initialize 发送 initialize 并检查 acpAgent 声明的能力。
func (w *acpWire) initialize(t *testing.T) {
	t.Helper()
	// 1. 检查协议版本为 1，支持加载、列表和 http、sse 两种 MCP。
	var result acp.InitializeResponse
	w.call(t, "initialize", acp.InitializeRequest{ProtocolVersion: 1}, &result)
	if result.ProtocolVersion != 1 || !result.AgentCapabilities.LoadSession || result.AgentCapabilities.SessionCapabilities.List == nil || !result.AgentCapabilities.McpCapabilities.Http || !result.AgentCapabilities.McpCapabilities.Sse {
		t.Fatalf("capabilities: %+v", result)
	}
}

// newSession 在 directory 下新建会话，返回会话 ID。
func (w *acpWire) newSession(t *testing.T, directory string) acp.SessionId {
	t.Helper()
	// 1. 带上工作目录和空 MCP 列表发送 session/new，检查返回了会话 ID 和设置选项。
	var result acp.NewSessionResponse
	w.call(t, "session/new", acp.NewSessionRequest{
		Cwd:        directory,
		McpServers: []acp.McpServer{},
	}, &result)
	if result.SessionId == "" || len(result.ConfigOptions) == 0 {
		t.Fatalf("new session: %+v", result)
	}
	return result.SessionId
}

// prompt 向指定会话发送文本 prompt 并返回请求 ID。
func (w *acpWire) prompt(t *testing.T, id acp.SessionId, text string) int {
	t.Helper()
	// 1. 构造文本输入并发送 session/prompt。
	return w.send(t, "session/prompt", acp.PromptRequest{
		SessionId: id,
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
}

// saveACPSession 在会话里完成一轮，让新会话写进存储。
func saveACPSession(t *testing.T, wire *acpWire, model *acpModel, id acp.SessionId) {
	t.Helper()
	// 1. 提交输入、让模型回复，等 prompt 以 end_turn 结束；之后会话能列出，也能在重启后加载。
	promptID := wire.prompt(t, id, "保存")
	model.next(t).reply <- acpModelReply{text: "好"}
	assertACPPromptResult(t, wire.response(t, promptID), acp.StopReasonEndTurn)
}

// newACPConfig 写入并加载 ACP 测试配置。
func newACPConfig(t *testing.T, modelURL string) (*config.Config, string) {
	t.Helper()
	// 1. 在临时目录写入应用、Agent 和模型配置：default 支持图片，second 带两档推理强度。
	directory := t.TempDir()
	files := map[string]string{
		"config.yaml":           "run:\n  agent_name: assistant\n",
		"agents/assistant.yaml": "name: assistant\ninstructions: Be concise.\nmodel:\n  provider: local\n  model: default\n",
		"models/local.yaml":     fmt.Sprintf("id: local\nprotocol: openai_chat\nbase_url: %s\nmodels:\n  default:\n    context_window: 32000\n    capabilities:\n      input: [text, image]\n  second:\n    context_window: 64000\n    reasoning_efforts: [low, high]\n    default_reasoning_effort: low\n", modelURL),
	}
	for name, content := range files {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// 2. 加载配置，返回配置和 config.yaml 路径。
	path := filepath.Join(directory, "config.yaml")
	cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

// acpTestToken、acpTestServerID 是进程内 Runtime Server 使用的固定 token 和 server_id；连接信息里的本机 server_id 也是它，
// 所以 acpAgent 把进程内的 Runtime Server 当作本机 serve。
const (
	acpTestToken    = "acp-test-token"
	acpTestServerID = "acp-test-server"
)

// acpTestServer 是进程内的 Runtime 与 Runtime Server，acpAgent 把它当作本机的 serve 连接。
type acpTestServer struct {
	connection runtimeConnection
	pipeline   *telemetrytest.Pipeline
	runtime    runtime.Runtime
	// stopServer 停掉 Runtime Server，已经停过时什么也不做。
	stopServer func()
	// stop 依次停掉 Runtime Server、Runtime，再关闭存储；测试结束时自动执行，重复调用什么也不做。
	stop func()
}

// acpTestConfiguration、acpTestModelCatalog 是测试 Runtime Server 的配置和模型目录依赖；内嵌接口为 nil，调用方法会 panic。
type acpTestConfiguration struct{ runtimeserver.Configuration }

type acpTestModelCatalog struct{ runtimeserver.ModelCatalog }

// acpTestListener 把接受的连接的发送缓冲调小，编辑器暂停读取时连接上的缓冲很快就满。
type acpTestListener struct{ net.Listener }

// Accept 接受测试连接并缩小 TCP 发送缓冲。
func (l acpTestListener) Accept() (net.Conn, error) {
	// 1. 接受连接，TCP 连接的发送缓冲设为 16KB。
	conn, err := l.Listener.Accept()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(16 << 10)
	}
	return conn, err
}

// startACPTestServer 在进程内运行测试 Runtime 和 Runtime Server。
func startACPTestServer(t *testing.T, cfg *config.Config, configureServer func(opt *runtimeserver.RuntimeServerOpt)) *acpTestServer {
	t.Helper()
	// 1. 创建内存遥测、Runtime 和随机端口的监听器。
	pipeline := telemetrytest.NewPipeline(t)
	rt, closeDAO, err := newRuntime(t.Context(), cfg, pipeline.Telemetry)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = closeDAO()
		t.Fatal(err)
	}
	// 2. 配置并创建 Runtime Server，configureServer 不为 nil 时让测试改配置。
	serverOpt := runtimeserver.NewRuntimeServerOpt(rt, "", acpTestToken)
	serverOpt.Listener = acpTestListener{listener}
	serverOpt.ServerID = acpTestServerID
	serverOpt.Configuration, serverOpt.ModelCatalog = acpTestConfiguration{}, acpTestModelCatalog{}
	serverOpt.Telemetry = pipeline.Telemetry
	if configureServer != nil {
		configureServer(serverOpt)
	}
	server, err := runtimeserver.NewRuntimeServer(serverOpt)
	if err != nil {
		_ = errors.Join(listener.Close(), closeDAO())
		t.Fatal(err)
	}
	// 3. 分别运行 Runtime 和 Server，记下连接信息。
	runtimeCtx, stopRuntime := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- rt.Run(runtimeCtx) }()
	serverCtx, cancelServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(serverCtx) }()
	testServer := &acpTestServer{
		connection: runtimeConnection{
			URL:           "ws://" + listener.Addr().String() + "/api/v1/openvaala",
			Token:         acpTestToken,
			Source:        runtimeConnectionSourceLocal,
			LocalServerID: acpTestServerID,
		},
		pipeline: pipeline,
		runtime:  rt,
	}
	// 4. 准备可重复调用的停止函数（依次停 Server、Runtime、关闭存储），测试结束时自动调用。
	isServerStopped, isStopped := false, false
	testServer.stopServer = func() {
		if isServerStopped {
			return
		}
		isServerStopped = true
		cancelServer()
		if err := <-serverDone; !errors.Is(err, context.Canceled) {
			t.Errorf("runtime server stopped: %v", err)
		}
	}
	testServer.stop = func() {
		if isStopped {
			return
		}
		isStopped = true
		testServer.stopServer()
		stopRuntime()
		<-runtimeDone
		if err := closeDAO(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(testServer.stop)
	return testServer
}

// loadACPConfigWith 在 config.yaml 后面追加一段配置并重新加载。
func loadACPConfigWith(t *testing.T, configPath, section string) *config.Config {
	t.Helper()
	// 1. 追加文本并加载配置。
	appendConfigText(t, configPath, section)
	cfg, err := config.LoadConfig(&config.LoadConfigOpt{Path: configPath})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startACPHost 启动测试 Runtime Server 和连接它的 acpAgent，返回测试编辑器。
func startACPHost(t *testing.T, cfg *config.Config) *acpWire {
	t.Helper()
	// 1. 用默认的 Runtime Server 配置启动。
	return startACPHostWithServer(t, cfg, nil)
}

// startACPHostWithServer 按 configureServer 修改 Runtime Server 配置后，启动它和连接它的 acpAgent。
func startACPHostWithServer(t *testing.T, cfg *config.Config, configureServer func(opt *runtimeserver.RuntimeServerOpt)) *acpWire {
	t.Helper()
	// 1. 启动 Runtime Server 和 acpAgent，wire.stop 改成先停 acpAgent 再停 Runtime Server。
	server := startACPTestServer(t, cfg, configureServer)
	wire := startACPAgent(t, cfg, server)
	stopAgent := wire.stop
	wire.stop = func() {
		stopAgent()
		server.stop()
	}
	return wire
}

// startACPAgent 启动一个连接 server 的 acpAgent，返回驱动它的测试编辑器。
func startACPAgent(t *testing.T, cfg *config.Config, server *acpTestServer) *acpWire {
	t.Helper()
	// 1. 创建 stdin、stdout 管道和 acpAgent，后台运行。
	input, clientInput := io.Pipe()
	clientOutput, output := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	host := &acpAgent{
		config:    cfg,
		server:    server.connection,
		telemetry: server.pipeline.Telemetry,
		input:     input,
		output:    output,
		version:   "test",
	}
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	// 2. 创建测试编辑器，准备 waitHost（最多等 10 秒 Run 返回）和 stop（关闭 stdin、等 Run 正常返回），测试结束时自动 stop。
	wire := newACPWire(t, clientInput, clientOutput)
	wire.pipeline, wire.runtime, wire.stopServer = server.pipeline, server.runtime, server.stopServer
	isHostStopped := false
	var hostErr error
	wire.waitHost = func(t *testing.T) error {
		t.Helper()
		if !isHostStopped {
			select {
			case hostErr = <-done:
				isHostStopped = true
			case <-time.After(10 * time.Second):
				t.Fatal("host did not stop")
			}
		}
		return hostErr
	}
	stopped := false
	wire.stop = func() {
		if stopped {
			return
		}
		stopped = true
		isChecked := isHostStopped
		wire.resumeReading()
		_ = clientInput.Close()
		err := wire.waitHost(t)
		if !isChecked && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			t.Errorf("host stopped: %v", err)
		}
		cancel()
	}
	t.Cleanup(wire.stop)
	return wire
}

// acpModelRequest 是受控模型收到的一次请求：请求正文和等测试给出回复的通道。
type acpModelRequest struct {
	body  map[string]any
	reply chan acpModelReply
}

// acpModelReply 是测试让受控模型做出的一次回复。
type acpModelReply struct {
	// text 按 | 拆成多段文本增量；reasoning 是一段推理增量；tool 非空时调用这个工具，参数是 arguments。
	text, reasoning, tool, arguments string
	// hold 是否发完内容后不结束响应，一直等到请求取消？
	hold bool
	// status 非 0 时直接返回这个 HTTP 错误码。
	status int
	// usage 是否在结束后追加 token 用量？
	usage bool
	// stream 非 nil 时，在 text 之后把收到的每段文本作为一个增量写出并立即发送，通道关闭后正常结束。
	stream <-chan string
}

// acpModel 是由测试控制回复的模型服务。
type acpModel struct {
	server   *httptest.Server
	requests chan acpModelRequest
}

// newACPModel 启动由测试控制回复的流式模型服务。
func newACPModel(t *testing.T) *acpModel {
	t.Helper()
	// 1. 建立请求队列和工具调用计数，每次工具调用分配新的 call ID。
	model := &acpModel{requests: make(chan acpModelRequest, 128)}
	var toolCalls atomic.Int32
	// 2. 每个请求解析 JSON 后等测试给出回复，再按回复写出 SSE：错误码、推理、文本、流式文本、工具调用、挂起或结束和用量。
	model.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		request := acpModelRequest{
			body:  body,
			reply: make(chan acpModelReply, 1),
		}
		model.requests <- request
		var reply acpModelReply
		select {
		case reply = <-request.reply:
		case <-r.Context().Done():
			return
		}
		if reply.status != 0 {
			http.Error(w, "model refused request", reply.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if reply.reasoning != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", reply.reasoning)
		}
		for _, part := range strings.Split(reply.text, "|") {
			if part != "" {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
			}
		}
	stream:
		for reply.stream != nil {
			select {
			case part, ok := <-reply.stream:
				if !ok {
					break stream
				}
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
		if reply.tool != "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-%d\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]}}]}\n\n", toolCalls.Add(1), reply.tool, reply.arguments)
		}
		if reply.hold {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		if reply.usage {
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":4,\"total_tokens\":124}}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	// 3. 测试结束时关闭模型服务。
	t.Cleanup(model.server.Close)
	return model
}

// next 等待受控模型收到的下一条请求。
func (m *acpModel) next(t *testing.T) acpModelRequest {
	t.Helper()
	// 1. 最多等 10 秒，超时时结束测试。
	select {
	case request := <-m.requests:
		return request
	case <-time.After(10 * time.Second):
		t.Fatal("model request timed out")
	}
	return acpModelRequest{}
}

// TestACPProtocolRequests 检查 initialize 之前的请求、无效请求和通知都得到正确处理，之后 acpAgent 照常回复 session/list。
func TestACPProtocolRequests(t *testing.T) {
	// 1. 逐场景发送请求，检查错误码，再确认 session/list 仍能成功。
	for _, scenario := range []string{"initialize_required", "invalid_json", "invalid_frame", "invalid_params", "unknown_method", "relative_cwd", "unknown_session", "unknown_load", "notifications"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, _ := newACPConfig(t, model.server.URL)
			wire := startACPHost(t, cfg)
			if scenario == "initialize_required" {
				result := wire.response(t, wire.send(t, "session/list", map[string]any{}))
				if result.Error == nil || result.Error.Code != -32600 {
					t.Fatalf("result: %+v", result)
				}
				return
			}
			wire.initialize(t)
			var result acpWireMessage
			expected := -32602
			switch scenario {
			case "invalid_json":
				_, _ = io.WriteString(wire.input, "{bad\n")
				result = wire.next(t)
				expected = -32700
			case "invalid_frame":
				_, _ = io.WriteString(wire.input, "[]\n")
				result = wire.next(t)
				expected = -32600
			case "invalid_params":
				result = wire.response(t, wire.send(t, "session/new", []string{}))
			case "unknown_method":
				result = wire.response(t, wire.send(t, "unsupported", map[string]any{}))
				expected = -32601
			case "relative_cwd":
				result = wire.response(t, wire.send(t, "session/new", map[string]any{"cwd": "relative", "mcpServers": []any{}}))
			case "unknown_session":
				result = wire.response(t, wire.prompt(t, "unknown", "hello"))
			case "unknown_load":
				result = wire.response(t, wire.send(t, "session/load", acp.LoadSessionRequest{
					SessionId: "missing",
					Cwd:       t.TempDir(),
				}))
			case "notifications":
				wire.notify(t, "unknown", map[string]any{})
				wire.notify(t, "session/cancel", map[string]any{"sessionId": "unknown"})
				wire.call(t, "session/list", map[string]any{}, nil)
				return
			}
			if result.Error == nil || result.Error.Code != expected {
				t.Fatalf("result = %+v, want %d", result, expected)
			}
			wire.call(t, "session/list", map[string]any{}, nil)
		})
	}
}

// TestACPNoModelConfigured 检查没配置模型时 session/new 返回配置模型的提示，acpAgent 仍能处理后续请求。
func TestACPNoModelConfigured(t *testing.T) {
	// 1. 列出没有配置文件和模型目录为空两种场景。
	const wantMessage = "no model is configured; run openvaala to set up a model provider"
	tests := []struct {
		name  string
		files map[string]string
	}{
		{name: "no_configuration_files"},
		{
			name:  "empty_models_directory",
			files: map[string]string{"config.yaml": "run:\n  agent_name: default\n", "models/.keep": ""},
		},
	}
	// 2. 启动 acpAgent，检查 session/new 返回 -32603 和配置模型的提示，再确认 session/list 仍能成功。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			home := t.TempDir()
			for name, content := range tt.files {
				path := filepath.Join(home, ".config", "openvaala", name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.LoadConfig(&config.LoadConfigOpt{Home: home})
			if err != nil {
				t.Fatal(err)
			}
			wire := startACPHost(t, cfg)
			wire.initialize(t)
			result := wire.response(t, wire.send(t, "session/new", acp.NewSessionRequest{
				Cwd:        t.TempDir(),
				McpServers: []acp.McpServer{},
			}))
			if result.Error == nil || result.Error.Code != -32603 || !strings.Contains(result.Error.Message, wantMessage) {
				t.Fatalf("session/new = %+v, want a -32603 error containing %q", result, wantMessage)
			}
			wire.call(t, "session/list", map[string]any{}, nil)
		})
	}
}

// TestACPListAndDirectories 检查会话目录、列表筛选和分页、重新加载，以及远端 serve 和 Windows 路径映射下的目录处理。
func TestACPListAndDirectories(t *testing.T) {
	// 1. 准备 Windows 编辑器一侧的映射前缀，它在这台机器上不是绝对路径。
	const windowsClientRoot = `C:\Users\me\code`
	// 2. 逐场景按配置启动 acpAgent 并保存一个会话，检查记录的目录、工具读到的目录、列表筛选或分页结果。
	for _, scenario := range []string{"new_session_local_directory", "directory_filter", "pagination", "running_directory", "cold_load_keeps_directory", "remote_server_ignores_cwd", "windows_client_mapping", "unix_client_ignores_windows_mapping"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, configPath := newACPConfig(t, model.server.URL)
			first, second := t.TempDir(), t.TempDir()
			// sessionDirectory 是 session/new 带的编辑器目录；configureServer 调整进程内 Runtime Server 的配置。
			sessionDirectory := first
			var configureServer func(opt *runtimeserver.RuntimeServerOpt)
			switch scenario {
			case "remote_server_ignores_cwd":
				// serve 返回的 server_id 和连接信息里的本机 server_id 不同：acpAgent 把它当远端 serve；它的默认目录是 second。
				cfg = loadACPConfigWith(t, configPath, fmt.Sprintf("server:\n  workspace_directory: %q\n", second))
				configureServer = func(opt *runtimeserver.RuntimeServerOpt) { opt.ServerID = "remote-server" }
			case "windows_client_mapping", "unix_client_ignores_windows_mapping":
				cfg = loadACPConfigWith(t, configPath, fmt.Sprintf("client:\n  path_mappings:\n    - client: %q\n      server: %q\n", windowsClientRoot, first))
				sessionDirectory = filepath.Join(first, "app")
				if err := os.Mkdir(sessionDirectory, 0700); err != nil {
					t.Fatal(err)
				}
				if scenario == "windows_client_mapping" {
					sessionDirectory = windowsClientRoot + `\app`
				}
			}
			wire := startACPHostWithServer(t, cfg, configureServer)
			wire.initialize(t)
			id := wire.newSession(t, sessionDirectory)
			saveACPSession(t, wire, model, id)
			switch scenario {
			case "new_session_local_directory":
				record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
				want := defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: first}}
				if err != nil || !reflect.DeepEqual(record.ExecutorSettings, want) {
					t.Fatalf("executor_settings = %+v, %v; want the cwd as the local directory %+v", record.ExecutorSettings, err, want)
				}
			case "running_directory":
				for directory, content := range map[string]string{first: "FIRST_CONTENT", second: "SECOND_CONTENT"} {
					if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
				wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       first,
				}, nil)
				wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       second,
				}, nil)
				// 已打开的会话沿用第一次的目录，工具读到的仍是 first 目录的文件。
				responseID := wire.prompt(t, id, "read notes")
				model.next(t).reply <- acpModelReply{
					tool:      "read_file",
					arguments: `{"path":"notes.txt"}`,
				}
				next := model.next(t)
				if body := fmt.Sprint(next.body); !strings.Contains(body, "FIRST_CONTENT") || strings.Contains(body, "SECOND_CONTENT") {
					t.Fatalf("tool did not read the first directory: %v", next.body)
				}
				next.reply <- acpModelReply{text: "done"}
				assertACPPromptResult(t, wire.response(t, responseID), acp.StopReasonEndTurn)
			case "cold_load_keeps_directory":
				// 停止 acpAgent 后重新启动并加载，检查记录的目录和列表 cwd 都没变。
				wire.stop()
				wire = startACPHost(t, cfg)
				wire.initialize(t)
				wire.call(t, "session/load", acp.LoadSessionRequest{
					SessionId: id,
					Cwd:       second,
				}, nil)
				var result acp.ListSessionsResponse
				wire.call(t, "session/list", acp.ListSessionsRequest{}, &result)
				if len(result.Sessions) != 1 || result.Sessions[0].Cwd != first {
					t.Fatalf("list = %+v, want the session still in %s", result, first)
				}
			case "remote_server_ignores_cwd":
				// 未映射的远端 cwd 不参与新建和列表筛选，对话使用 serve 默认目录。
				record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
				if err != nil || record.ExecutorSettings.GetConversationDirectory() != second {
					t.Fatalf("executor_settings = %+v, %v; want the serve default directory %s", record.ExecutorSettings, err, second)
				}
				var result acp.ListSessionsResponse
				wire.call(t, "session/list", acp.ListSessionsRequest{Cwd: &first}, &result)
				if len(result.Sessions) != 1 || result.Sessions[0].SessionId != id || result.Sessions[0].Cwd != second {
					t.Fatalf("list by %s = %+v, want the unfiltered session in %s", first, result, second)
				}
			case "windows_client_mapping":
				// Windows 路径按映射换算后用于新建和列表筛选，没映射的 Windows 路径被拒绝。
				record, err := wire.runtime.GetConversation(t.Context(), &runtime.GetConversationRequest{ConversationID: defs.ConversationID(id)})
				if want := filepath.Join(first, "app"); err != nil || record.ExecutorSettings.GetConversationDirectory() != want {
					t.Fatalf("executor_settings = %+v, %v; want the mapped directory %s", record.ExecutorSettings, err, want)
				}
				var result acp.ListSessionsResponse
				wire.call(t, "session/list", acp.ListSessionsRequest{Cwd: &sessionDirectory}, &result)
				if len(result.Sessions) != 1 || result.Sessions[0].SessionId != id || result.Sessions[0].Cwd != sessionDirectory {
					t.Fatalf("list by %s = %+v, want the session with the Windows directory", sessionDirectory, result)
				}
				rejected := wire.response(t, wire.send(t, "session/new", acp.NewSessionRequest{
					Cwd:        `D:\other`,
					McpServers: []acp.McpServer{},
				}))
				if rejected.Error == nil || rejected.Error.Code != -32602 || rejected.Error.Message != "cwd must be an absolute directory" {
					t.Fatalf("session/new with an unmapped Windows directory = %+v, want -32602", rejected)
				}
			case "unix_client_ignores_windows_mapping":
				// Unix 编辑器的列表结果保留 serve 路径，不套用 Windows 映射。
				var result acp.ListSessionsResponse
				wire.call(t, "session/list", acp.ListSessionsRequest{Cwd: &sessionDirectory}, &result)
				if len(result.Sessions) != 1 || result.Sessions[0].SessionId != id || result.Sessions[0].Cwd != sessionDirectory {
					t.Fatalf("list by %s = %+v, want the session with the serve path, not a Windows path", sessionDirectory, result)
				}
			case "directory_filter", "pagination":
				count := 3
				if scenario == "pagination" {
					count = 55
				}
				for index := 1; index < count; index++ {
					saveACPSession(t, wire, model, wire.newSession(t, first))
				}
				saveACPSession(t, wire, model, wire.newSession(t, second))
				seen := map[acp.SessionId]bool{}
				request := acp.ListSessionsRequest{Cwd: &first}
				for {
					var result acp.ListSessionsResponse
					wire.call(t, "session/list", request, &result)
					if len(result.Sessions) > 50 {
						t.Fatal("page too large")
					}
					for _, record := range result.Sessions {
						if seen[record.SessionId] || record.Cwd != first {
							t.Fatalf("bad record = %+v", record)
						}
						seen[record.SessionId] = true
					}
					if result.NextCursor == nil {
						break
					}
					request.Cursor = result.NextCursor
				}
				if len(seen) != count {
					t.Fatalf("list returned %d, want %d", len(seen), count)
				}
			}
		})
	}
}

// TestACPTransportStopsTasks 检查 stdin 结束、stdout 写失败、写阻塞时取消、单行超长和 serve 断开时 acpAgent 都会退出；serve 断开时等待中的 prompt 收到连接丢失错误。
func TestACPTransportStopsTasks(t *testing.T) {
	// 1. 逐场景启动 acpAgent 并触发传输结束，检查 Run 带着原因返回。
	for _, scenario := range []string{"eof", "write_failure", "cancel_blocked_writer", "oversized_line", "server_stopped_fails_prompt"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			model := newACPModel(t)
			cfg, _ := newACPConfig(t, model.server.URL)
			if scenario == "server_stopped_fails_prompt" {
				wire := startACPHost(t, cfg)
				wire.initialize(t)
				id := wire.newSession(t, t.TempDir())
				promptID := wire.prompt(t, id, "hold")
				model.next(t).reply <- acpModelReply{
					text: "partial",
					hold: true,
				}
				waitACPAgentChunk(t, wire)
				wire.stopServer()
				result := wire.response(t, promptID)
				if result.Error == nil || result.Error.Code != -32603 || result.Error.Message != "connection to runtime server lost" {
					t.Fatalf("prompt = %+v, want -32603 connection to runtime server lost", result)
				}
				if err := wire.waitHost(t); !errors.Is(err, runtimeclient.ErrConnectionLost) {
					t.Fatalf("host stopped: %v, want %v", err, runtimeclient.ErrConnectionLost)
				}
				return
			}
			server := startACPTestServer(t, cfg, nil)
			input, clientInput := io.Pipe()
			clientOutput, output := io.Pipe()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			host := &acpAgent{
				config:    cfg,
				server:    server.connection,
				telemetry: server.pipeline.Telemetry,
				input:     input,
				output:    output,
			}
			done := make(chan error, 1)
			go func() { done <- host.Run(ctx) }()
			switch scenario {
			case "eof":
				_ = clientInput.Close()
			case "write_failure":
				_ = clientOutput.Close()
				_, _ = io.WriteString(clientInput, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n")
			case "cancel_blocked_writer":
				_, _ = io.WriteString(clientInput, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n")
				cancel()
			case "oversized_line":
				_, _ = io.CopyN(clientInput, strings.NewReader(strings.Repeat("x", (16<<20)+1)), (16<<20)+1)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("missing transport close reason")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("transport did not close its tasks")
			}
			_ = clientInput.Close()
			_ = clientOutput.Close()
		})
	}
}

// waitACPAgentChunk 一直读到一条 agent_message_chunk 通知。
func waitACPAgentChunk(t *testing.T, wire *acpWire) {
	t.Helper()
	// 1. 持续读取，读到 agent_message_chunk 时返回。
	for {
		message := wire.next(t)
		if message.Method == "session/update" && wire.updates[len(wire.updates)-1].Update.AgentMessageChunk != nil {
			return
		}
	}
}
