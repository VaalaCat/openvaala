package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// relayTestStream 是测试客户端一侧的一条流：RunRelayStream 从 incoming 读 serve 发来的消息，cancel 结束它。
type relayTestStream struct {
	incoming chan json.RawMessage
	ctx      context.Context
	cancel   context.CancelFunc
}

// relayTestSender 在测试里扮演 serve 的客户端连接和客户端：SendMCPConnect 时用假 server 起一个 RunRelayStream，
// 两个方向的消息按 stream_id 在 RelayTransport 和 RunRelayStream 之间转交。
type relayTestSender struct {
	binary string
	// transport 在开始连接之前设置。
	transport RelayTransport
	ctx       context.Context
	tasks     sync.WaitGroup
	streams   syncmap.SyncMap[string, *relayTestStream]
	// connectError 非 nil 时 SendMCPConnect 直接失败，模拟客户端连接已经断开。
	connectError error
	// isStartHeld SendMCPConnect 是否只记录 stream_id、不启动客户端一侧的流？模拟客户端迟迟没有响应。
	isStartHeld atomic.Bool
	// connects 和 disconnects 按顺序记下收到的 stream_id。
	connects    chan string
	disconnects chan string
}

// SendMCPConnect 记录连接请求并运行测试客户端转发流。
func (s *relayTestSender) SendMCPConnect(streamID, serverID string) error {
	// 1. 处理预设错误并校验 server ID，记录 stream ID。
	if s.connectError != nil {
		return s.connectError
	}
	if serverID != "client" {
		return errors.New("unexpected server id " + serverID)
	}
	s.connects <- streamID
	if s.isStartHeld.Load() {
		return nil
	}
	// 2. 没有暂停启动时创建流、运行本机 server，流结束后通知 serve。
	streamCtx, cancel := context.WithCancel(s.ctx)
	stream := &relayTestStream{
		incoming: make(chan json.RawMessage),
		ctx:      streamCtx,
		cancel:   cancel,
	}
	s.streams.Store(streamID, stream)
	s.tasks.Go(func() {
		defer cancel()
		transport := &mcp.CommandTransport{Command: exec.Command(s.binary)}
		err := RunRelayStream(streamCtx, transport, stream.incoming, func(message json.RawMessage) error {
			s.transport.DeliverMessage(streamID, message)
			return nil
		})
		// 客户端一侧的流结束后通知 serve；serve 已经断开的流会丢弃这条通知。
		s.transport.DeliverClosed(streamID, err)
	})
	return nil
}

// SendMCPMessage 把消息交给测试客户端的流。
func (s *relayTestSender) SendMCPMessage(streamID string, message json.RawMessage) error {
	// 1. 查找流，等待流读取消息或上下文结束。
	stream, ok := s.streams.Load(streamID)
	if !ok {
		return nil
	}
	select {
	case stream.incoming <- message:
	case <-stream.ctx.Done():
	}
	return nil
}

// SendMCPDisconnect 记录断开通知并取消测试客户端流。
func (s *relayTestSender) SendMCPDisconnect(streamID string) error {
	// 1. 记录 stream ID，删除已登记的流并取消上下文。
	s.disconnects <- streamID
	if stream, ok := s.streams.LoadAndDelete(streamID); ok {
		stream.cancel()
	}
	return nil
}

// relayTestFixture 是一个用例里正在运行的转发连接。
type relayTestFixture struct {
	connection Connection
	transport  RelayTransport
	sender     *relayTestSender
	// stopRun 取消 Run 并等它返回，可重复调用。
	stopRun func()
}

// newRelayTestFixture 创建并运行客户端转发连接。
func newRelayTestFixture(t *testing.T, connectError error) *relayTestFixture {
	// 1. 创建测试客户端、转发传输和 MCP 连接。
	t.Helper()
	senderCtx, cancelSender := context.WithCancel(context.Background())
	sender := &relayTestSender{
		binary:       buildFakeMCPServer(t),
		ctx:          senderCtx,
		connectError: connectError,
		connects:     make(chan string, 16),
		disconnects:  make(chan string, 16),
	}
	t.Cleanup(func() {
		cancelSender()
		sender.tasks.Wait()
	})
	transport := NewRelayTransport(NewRelayTransportOpt("client", sender))
	sender.transport = transport

	definition := &ServerDefinition{
		ID:             "client",
		StartupTimeout: 5 * time.Second,
		ToolTimeout:    30 * time.Second,
	}
	connection, err := NewConnection(NewConnectionOpt(definition, nil).WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	// 2. 后台运行连接，测试结束时取消并等待退出。
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = connection.Run(runCtx)
	}()
	stopRun := func() {
		cancelRun()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not return after its context ended")
		}
	}
	t.Cleanup(stopRun)
	return &relayTestFixture{
		connection: connection,
		transport:  transport,
		sender:     sender,
		stopRun:    stopRun,
	}
}

// receiveStreamID 读取下一个 stream ID。
func receiveStreamID(t *testing.T, streamIDs <-chan string) string {
	// 1. 最多等待流编号五秒，超时则结束测试。
	t.Helper()
	select {
	case streamID := <-streamIDs:
		return streamID
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a stream id")
		return ""
	}
}

// drainStreamIDs 取出已经缓存的全部流编号。
func drainStreamIDs(streamIDs <-chan string) []string {
	// 1. 循环读取现有编号，没有数据时立即返回。
	var drained []string
	for {
		select {
		case streamID := <-streamIDs:
			drained = append(drained, streamID)
		default:
			return drained
		}
	}
}

// TestRelayTransport 验证客户端 MCP 转发、重连和断开通知。
func TestRelayTransport(t *testing.T) {
	// 1. 准备正常调用、未知流、客户端关闭及请求失败用例。
	tests := []struct {
		name string
		// connectError 非 nil 时客户端连接一开始就发不出 clientMCP/connect。
		connectError error
		run          func(t *testing.T, fixture *relayTestFixture)
	}{
		{
			name: "tools and calls go through the client",
			run: func(t *testing.T, fixture *relayTestFixture) {
				tools, err := fixture.connection.GetTools(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if names := getToolNames(tools); !slices.Contains(names, "mcp__client__echo") {
					t.Fatalf("tools = %v, want mcp__client__echo", names)
				}
				if text := getTextContent(t, callTool(t.Context(), fixture.connection, "mcp__client__echo", `{"text":"hello"}`)); text != "hello" {
					t.Fatalf("echo text = %q, want hello", text)
				}
			},
		},
		{
			name: "messages for unknown streams are dropped",
			run: func(t *testing.T, fixture *relayTestFixture) {
				fixture.transport.DeliverMessage("unknown", json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
				fixture.transport.DeliverClosed("unknown", errors.New("late"))
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatalf("GetTools() after stray deliveries: %v", err)
				}
			},
		},
		{
			name: "client closing a stream fails the connection and reconnects on a new stream",
			run: func(t *testing.T, fixture *relayTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				firstStreamID := receiveStreamID(t, fixture.sender.connects)

				// 暂停新流启动，读取 failed 状态和断开原因。
				fixture.sender.isStartHeld.Store(true)
				fixture.transport.DeliverClosed(firstStreamID, errors.New("gone"))
				secondStreamID := receiveStreamID(t, fixture.sender.connects)
				if secondStreamID == "" || secondStreamID == firstStreamID {
					t.Fatalf("reconnect stream id = %q, want a new id different from %q", secondStreamID, firstStreamID)
				}
				if info := fixture.connection.GetInfo(); info.State != ConnectionStateFailed || !strings.Contains(info.Error, "gone") {
					t.Fatalf("GetInfo() = %+v, want failed with a reason containing gone", info)
				}

				// 客户端自己结束的流不用再通知客户端断开；只有还没结束的重连流在 Run 结束时断开。
				fixture.stopRun()
				if disconnects := drainStreamIDs(fixture.sender.disconnects); !slices.Equal(disconnects, []string{secondStreamID}) {
					t.Fatalf("disconnected streams = %v, want only %q", disconnects, secondStreamID)
				}
			},
		},
		{
			name: "Run ending disconnects the stream once",
			run: func(t *testing.T, fixture *relayTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
				streamID := receiveStreamID(t, fixture.sender.connects)
				fixture.stopRun()
				if disconnects := drainStreamIDs(fixture.sender.disconnects); !slices.Equal(disconnects, []string{streamID}) {
					t.Fatalf("disconnected streams = %v, want exactly %q", disconnects, streamID)
				}
			},
		},
		{
			name:         "connect request failure fails the attempt",
			connectError: errors.New("client offline"),
			run: func(t *testing.T, fixture *relayTestFixture) {
				if _, err := fixture.connection.GetTools(t.Context()); err == nil || !strings.Contains(err.Error(), "client offline") {
					t.Fatalf("GetTools() error = %v, want the connect failure", err)
				}
				if info := fixture.connection.GetInfo(); info.State != ConnectionStateFailed || !strings.Contains(info.Error, "client offline") {
					t.Fatalf("GetInfo() = %+v, want failed with the connect failure", info)
				}
			},
		},
	}
	// 2. 逐项创建转发流，执行用例并核对结束结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, newRelayTestFixture(t, tt.connectError))
		})
	}
}

// relayStreamLifecycleFixture 是一条已经 Connect 的转发流；客户端一侧不启动，流上没有人读消息。
type relayStreamLifecycleFixture struct {
	transport RelayTransport
	sender    *relayTestSender
	stream    mcp.Connection
	streamID  string
}

// deliverUnreadMessage 后台发送一条无人读取的消息，并核对发送仍在等待。
func (f *relayStreamLifecycleFixture) deliverUnreadMessage(t *testing.T) <-chan struct{} {
	// 1. 启动消息交付任务，核对一百毫秒内未返回，并返回完成通道。
	t.Helper()
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		f.transport.DeliverMessage(f.streamID, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/message"}`))
	}()
	select {
	case <-delivered:
		t.Fatal("DeliverMessage returned while nobody read the stream")
	case <-time.After(100 * time.Millisecond):
	}
	return delivered
}

// TestRelayStream 验证无人读取的流可结束，重复关闭只通知一次。
func TestRelayStream(t *testing.T) {
	// 1. 准备客户端结束、本端关闭和重复关闭用例。
	tests := []struct {
		name string
		run  func(t *testing.T, fixture *relayStreamLifecycleFixture)
	}{
		{
			name: "DeliverMessage on an unread stream returns once the client closes it",
			run: func(t *testing.T, fixture *relayStreamLifecycleFixture) {
				delivered := fixture.deliverUnreadMessage(t)
				fixture.transport.DeliverClosed(fixture.streamID, errors.New("gone"))
				select {
				case <-delivered:
				case <-time.After(5 * time.Second):
					t.Fatal("DeliverMessage did not return after DeliverClosed")
				}
			},
		},
		{
			name: "DeliverMessage on an unread stream returns once Close ends it",
			run: func(t *testing.T, fixture *relayStreamLifecycleFixture) {
				delivered := fixture.deliverUnreadMessage(t)
				if err := fixture.stream.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-delivered:
				case <-time.After(5 * time.Second):
					t.Fatal("DeliverMessage did not return after Close")
				}
			},
		},
		{
			name: "closing twice tells the client to disconnect once",
			run: func(t *testing.T, fixture *relayStreamLifecycleFixture) {
				for range 2 {
					if err := fixture.stream.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if disconnects := drainStreamIDs(fixture.sender.disconnects); !slices.Equal(disconnects, []string{fixture.streamID}) {
					t.Fatalf("disconnected streams = %v, want exactly %q", disconnects, fixture.streamID)
				}
			},
		},
	}
	// 2. 逐项创建转发流，执行用例并核对结束结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sender := &relayTestSender{
				ctx:         t.Context(),
				connects:    make(chan string, 16),
				disconnects: make(chan string, 16),
			}
			sender.isStartHeld.Store(true)
			transport := NewRelayTransport(NewRelayTransportOpt("client", sender))
			stream, err := transport.Connect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tt.run(t, &relayStreamLifecycleFixture{
				transport: transport,
				sender:    sender,
				stream:    stream,
				streamID:  receiveStreamID(t, sender.connects),
			})
		})
	}
}

// relayStreamFixture 是一个运行中的 RunRelayStream 和它连着的本机 server 一侧。
type relayStreamFixture struct {
	// serverConnection 是本机 server 一侧的连接。
	serverConnection mcp.Connection
	// incoming 是交给 RunRelayStream、要写给本机 server 的消息。
	incoming chan json.RawMessage
	// sent 收到 RunRelayStream 经 send 发给 serve 的消息。
	sent   chan json.RawMessage
	cancel context.CancelFunc
	// isSendFailing send 是否返回错误？
	isSendFailing atomic.Bool
	// runDone 收到 RunRelayStream 的返回值。
	runDone chan error
}

// TestRunRelayStream 验证双向消息转发及不同结束方式。
func TestRunRelayStream(t *testing.T) {
	// 1. 准备输入关闭、调用取消、server 断开和发送失败用例。
	tests := []struct {
		name string
		// end 让流结束。
		end func(t *testing.T, fixture *relayStreamFixture)
		// wantError 是 RunRelayStream 应返回的错误；nil 表示正常结束。
		wantError func(err error) bool
	}{
		{
			name:      "incoming closed ends the stream",
			end:       func(t *testing.T, fixture *relayStreamFixture) { close(fixture.incoming) },
			wantError: func(err error) bool { return err == nil },
		},
		{
			name:      "context canceled ends the stream",
			end:       func(t *testing.T, fixture *relayStreamFixture) { fixture.cancel() },
			wantError: func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		{
			name: "local server closing the connection ends the stream",
			end: func(t *testing.T, fixture *relayStreamFixture) {
				if err := fixture.serverConnection.Close(); err != nil {
					t.Fatal(err)
				}
			},
			wantError: func(err error) bool { return err == nil },
		},
		{
			name: "send failure ends the stream",
			end: func(t *testing.T, fixture *relayStreamFixture) {
				fixture.isSendFailing.Store(true)
				message := &jsonrpc.Request{Method: "notifications/message"}
				if err := fixture.serverConnection.Write(t.Context(), message); err != nil {
					t.Fatal(err)
				}
			},
			wantError: func(err error) bool { return err != nil && strings.Contains(err.Error(), "serve is gone") },
		},
	}
	// 2. 逐项创建转发流，执行用例并核对结束结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// 1. 创建内存传输和转发流，后台运行 RunRelayStream。
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			serverConnection, err := serverTransport.Connect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer serverConnection.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture := &relayStreamFixture{
				serverConnection: serverConnection,
				incoming:         make(chan json.RawMessage),
				sent:             make(chan json.RawMessage, 1),
				cancel:           cancel,
				runDone:          make(chan error, 1),
			}
			go func() {
				fixture.runDone <- RunRelayStream(ctx, clientTransport, fixture.incoming, func(message json.RawMessage) error {
					if fixture.isSendFailing.Load() {
						return errors.New("serve is gone")
					}
					fixture.sent <- message
					return nil
				})
			}()

			// 2. 双向发送 ping 请求和响应，核对消息原样转交。
			fixture.incoming <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
			message, err := serverConnection.Read(t.Context())
			if request, ok := message.(*jsonrpc.Request); err != nil || !ok || request.Method != "ping" {
				t.Fatalf("local server read = %#v, %v, want the ping request", message, err)
			}
			response := &jsonrpc.Response{
				ID:     message.(*jsonrpc.Request).ID,
				Result: json.RawMessage(`{}`),
			}
			if err := serverConnection.Write(t.Context(), response); err != nil {
				t.Fatal(err)
			}
			select {
			case sent := <-fixture.sent:
				var decoded struct {
					ID     int             `json:"id"`
					Result json.RawMessage `json:"result"`
				}
				if err := json.Unmarshal(sent, &decoded); err != nil || decoded.ID != 1 || string(decoded.Result) != "{}" {
					t.Fatalf("message sent to serve = %s (%v), want the ping response", sent, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the response to reach serve")
			}

			// 3. 触发本用例的结束方式，核对 RunRelayStream 的返回值。
			tt.end(t, fixture)
			select {
			case err := <-fixture.runDone:
				if !tt.wantError(err) {
					t.Fatalf("RunRelayStream() error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("RunRelayStream did not return")
			}
		})
	}
}
