package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RelaySender 把转发消息发给客户端；由 serve 的客户端连接实现。
// MCP SDK 会并发写消息，实现必须允许并发调用。
type RelaySender interface {
	// SendMCPConnect 请求客户端为 streamID 连接指定 server。
	SendMCPConnect(streamID, serverID string) error
	// SendMCPMessage 向客户端发送流上的完整 JSON-RPC 消息。
	SendMCPMessage(streamID string, message json.RawMessage) error
	// SendMCPDisconnect 请求客户端断开指定流。
	SendMCPDisconnect(streamID string) error
}

// RelayTransport 是经客户端连接收发 MCP 消息的传输：Connect 每次新开一条转发流，
// 客户端发来的 clientMCP/message、clientMCP/closed 由 serve 按 stream_id 交给 DeliverMessage、DeliverClosed。
// 同一个 RelayTransport 可以反复 Connect（重连），各条流互不影响。
type RelayTransport interface {
	mcp.Transport
	// DeliverMessage 交付客户端在 streamID 这条流上发来的一条完整 JSON-RPC 消息，直到 MCP SDK 读走它或这条流结束才返回，
	// 同一条流按到达顺序交付消息，流不存在或已结束时丢弃。
	DeliverMessage(streamID string, message json.RawMessage)
	// DeliverClosed 告知客户端已结束 streamID 这条流：MCP SDK 的读取随之以 err 结束，err 为 nil 表示正常结束。
	// 流不存在或已结束时丢弃。
	DeliverClosed(streamID string, err error)
}

// relayTransportImpl 每次 Connect 登记一条 relayStream，按 stream_id 分发客户端发来的事件。
type relayTransportImpl struct {
	opt RelayTransportOpt
	// relayStreams 是还没结束的转发流；流结束时自己从这里删除。
	relayStreams syncmap.SyncMap[string, *relayStream]
}

// RelayTransportOpt 保存客户端 server ID 和消息发送对象。
type RelayTransportOpt struct {
	// ServerID 是客户端登记的 MCP server id，每次连接都带给客户端，让它找到对应的配置。
	ServerID string
	// Sender 把消息发给客户端。
	Sender RelaySender
}

// NewRelayTransportOpt 保存客户端 MCP server ID 和消息发送对象。
func NewRelayTransportOpt(serverID string, sender RelaySender) *RelayTransportOpt {
	// 1. 创建转发传输配置。
	return &RelayTransportOpt{
		ServerID: serverID,
		Sender:   sender,
	}
}

// NewRelayTransport 创建转发传输。
func NewRelayTransport(opt *RelayTransportOpt) RelayTransport {
	// 1. 复制配置并返回实例。
	return &relayTransportImpl{opt: *opt}
}

// Connect 登记新的转发流并请求客户端连接 MCP server。
func (t *relayTransportImpl) Connect(context.Context) (mcp.Connection, error) {
	// 1. 创建新的 stream ID、输入通道和结束通道。
	stream := &relayStream{
		transport: t,
		id:        uuid.NewString(),
		incoming:  make(chan json.RawMessage),
		ended:     make(chan struct{}),
	}
	// 2. 登记流，再发送连接请求；发送失败时删除登记。
	t.relayStreams.Store(stream.id, stream)
	if err := t.opt.Sender.SendMCPConnect(stream.id, t.opt.ServerID); err != nil {
		t.relayStreams.Delete(stream.id)
		return nil, fmt.Errorf("ask the client to connect MCP server %q: %w", t.opt.ServerID, err)
	}
	return stream, nil
}

// DeliverMessage 把客户端消息交给对应的流。
func (t *relayTransportImpl) DeliverMessage(streamID string, message json.RawMessage) {
	// 1. 查找流并等待 SDK 读取消息，流不存在或已结束时丢弃消息。
	stream, ok := t.relayStreams.Load(streamID)
	if !ok {
		return
	}
	select {
	case stream.incoming <- message:
	case <-stream.ended:
	}
}

// DeliverClosed 结束客户端指定的流。
func (t *relayTransportImpl) DeliverClosed(streamID string, err error) {
	// 1. 查找流并设置结束原因，未提供错误时按 EOF 处理，流不存在时跳过。
	stream, ok := t.relayStreams.Load(streamID)
	if !ok {
		return
	}
	if err == nil {
		err = io.EOF
	}
	stream.end(err)
}

// relayStream 是一次连接尝试对应的转发流，实现 mcp.Connection：读到客户端发来的消息，写出的消息交给 Sender。
type relayStream struct {
	transport *relayTransportImpl
	id        string
	// incoming 承载 DeliverMessage 交来的消息，不带缓冲，也不关闭：流结束靠 ended。
	incoming chan json.RawMessage
	// ended 在流结束（本端 Close 或客户端 closed）时关闭，Read 和 DeliverMessage 随之放弃等待。
	ended chan struct{}
	// isEnded 这条流是否已经结束？只有第一次结束生效。
	isEnded atomic.Bool
	// endReason 是 Read 在流结束后返回的原因；先于关闭 ended 写入，之后只读。
	endReason error
}

// end 以 reason 结束这条流，返回这次调用是否第一次结束它；已经结束过时不改动。
func (s *relayStream) end(reason error) bool {
	// 1. 抢占首次结束标记，保存错误、关闭 ended 并删除流登记。
	if !s.isEnded.CompareAndSwap(false, true) {
		return false
	}
	s.endReason = reason
	close(s.ended)
	s.transport.relayStreams.Delete(s.id)
	return true
}

// Read 读取并解码客户端消息。
func (s *relayStream) Read(ctx context.Context) (jsonrpc.Message, error) {
	// 1. 等待输入并按 JSON-RPC 解码，流结束或 ctx 取消时返回对应错误。
	select {
	case message := <-s.incoming:
		decoded, err := jsonrpc.DecodeMessage(message)
		if err != nil {
			return nil, fmt.Errorf("decode MCP message from the client: %w", err)
		}
		return decoded, nil
	case <-s.ended:
		return nil, s.endReason
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Write 编码 JSON-RPC 消息并发送给客户端。
func (s *relayStream) Write(ctx context.Context, message jsonrpc.Message) error {
	// 1. 检查调用取消和流结束，再编码并发送消息。
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.isEnded.Load() {
		return mcp.ErrConnectionClosed
	}
	data, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return fmt.Errorf("encode MCP message for the client: %w", err)
	}
	if err := s.transport.opt.Sender.SendMCPMessage(s.id, data); err != nil {
		return fmt.Errorf("send MCP message to the client: %w", err)
	}
	return nil
}

// Close 结束流并通知客户端断开。
func (s *relayStream) Close() error {
	// 1. 首次结束时发送断开消息，重复关闭或客户端已结束的流不再通知。
	if !s.end(io.EOF) {
		return nil
	}
	return s.transport.opt.Sender.SendMCPDisconnect(s.id)
}

// SessionID 转发流没有 MCP 会话 ID，总是返回空字符串。
func (s *relayStream) SessionID() string {
	// 1. 返回空字符串。
	return ""
}

// RunRelayStream 在客户端一侧连接本机 MCP server：incoming 里 serve 发来的消息写给 server，server 发出的消息交给 send，直到任一方结束。
func RunRelayStream(ctx context.Context, transport mcp.Transport, incoming <-chan json.RawMessage, send func(json.RawMessage) error) error {
	// 1. 创建可取消的连接上下文并连接本机 server；连接失败时取消上下文并返回错误。
	ctx, cancel := context.WithCancel(ctx)
	connection, err := transport.Connect(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("connect the local MCP server: %w", err)
	}
	// 2. 登记收尾：取消上下文（关闭 SSE 等传输的长连接）、关闭 server 连接并等待读取任务；send 必须能自己返回。
	var tasks sync.WaitGroup
	defer func() {
		cancel()
		_ = connection.Close()
		tasks.Wait()
	}()

	// 3. 后台读取 server 消息，同时把 incoming 消息写给 server；incoming 关闭或 server 正常结束时返回 nil，
	// 解码、读写或发送失败时返回错误，ctx 结束时返回 ctx.Err()。
	serverEnded := make(chan error, 1)
	tasks.Go(func() { serverEnded <- relayServerMessages(ctx, connection, send) })
	for {
		select {
		case message, ok := <-incoming:
			if !ok {
				return nil
			}
			decoded, err := jsonrpc.DecodeMessage(message)
			if err != nil {
				return fmt.Errorf("decode MCP message from serve: %w", err)
			}
			if err := connection.Write(ctx, decoded); err != nil {
				return fmt.Errorf("write to the local MCP server: %w", err)
			}
		case err := <-serverEnded:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// relayServerMessages 把本机 server 发来的消息编码后交给 send 发给 serve。
func relayServerMessages(ctx context.Context, connection mcp.Connection, send func(json.RawMessage) error) error {
	// 1. 循环读取消息、编码 JSON-RPC 并发送；server 正常 EOF 时返回 nil，读取、编码或发送失败时返回错误。
	for {
		message, err := connection.Read(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read from the local MCP server: %w", err)
		}
		data, err := jsonrpc.EncodeMessage(message)
		if err != nil {
			return fmt.Errorf("encode MCP message for serve: %w", err)
		}
		if err := send(data); err != nil {
			return fmt.Errorf("send MCP message to serve: %w", err)
		}
	}
}
