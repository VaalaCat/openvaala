package runtimeclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/coder/websocket"
)

const (
	// dialTimeout 是一次拨号的超时。
	dialTimeout = 10 * time.Second
	// maxFrameBytes 是 Client 读取单帧的上限：一页历史通常是 4 MiB 加最后 1 条 Item，远小于它；
	// 单条 Item 极大（例如超过 28 MiB 的工具输出）时一页仍可能超过，读取失败后连接断开，Client 自动重连。
	maxFrameBytes = 32 << 20
	// maxRequestFrameBytes 是 serve 的单帧读取上限，与 pkg/runtimeserver 的 maxFrameBytes 一致。
	// 编码后的请求超过上限时，客户端直接返回错误，不发送、不重试。
	maxRequestFrameBytes = 16 << 20
)

// clientConnection 是一条已建立的 WebSocket 连接：为请求分配 id 并配对响应，把通知交给 Client 事件循环。
type clientConnection struct {
	conn *websocket.Conn
	// 调试采样出口由 Client 创建，连接只尽力投递，不拥有记录。
	rpcConnectionID uint64
	rpcSamples      chan<- RPCFrame
	rpcDropped      *atomic.Uint64
	rpcDropWake     chan<- struct{}
	rpcLogPaused    *atomic.Bool
	// loopInputs 是 Client 事件循环的入口，读循环把通知交给它。
	loopInputs chan<- clientLoopInput
	// nextID 是上一个分配出去的请求 id。
	nextID atomic.Int64
	// pendingCalls 是已发出、等待响应的请求：读循环收到响应时取出并回复。
	pendingCalls syncmap.SyncMap[int64, chan *wireMessage]
	// closed 在读循环结束、连接关闭后关闭；等待中的请求随之返回 ErrConnectionLost。
	closed chan struct{}
}

// dialConnection 建立 WebSocket 连接。
func dialConnection(ctx context.Context, url, token string) (*websocket.Conn, error) {
	// 1. 设置 Bearer token，按拨号超时建立连接。
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{HTTPHeader: header})
	// 2. 将 401 转为 ErrUnauthorized，其他拨号错误隐藏地址中的认证信息。
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		return nil, redactDialError(err)
	}
	// 3. 设置单帧读取上限并返回连接。
	conn.SetReadLimit(maxFrameBytes)
	return conn, nil
}

// newClientConnection 保存 WebSocket 和客户端事件入口。
func newClientConnection(conn *websocket.Conn, loopInputs chan<- clientLoopInput) *clientConnection {
	// 1. 保存连接和事件入口，创建连接结束通知。
	return &clientConnection{
		conn:       conn,
		loopInputs: loopInputs,
		closed:     make(chan struct{}),
	}
}

// Run 读取 WebSocket 消息并交付响应和通知。
func (c *clientConnection) Run(ctx context.Context) error {
	// 1. 连接断开或 ctx 结束时返回，先关闭连接，再关闭结束通知并唤醒等待中的请求。
	defer close(c.closed)
	defer c.conn.CloseNow()
	// 2. 持续读取并解码消息，读取或解码失败时返回错误。
	for {
		_, frame, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		c.observeRPCFrame(RPCReceived, frame)
		var message wireMessage
		if err := json.Unmarshal(frame, &message); err != nil {
			return fmt.Errorf("decode frame: %w", err)
		}
		// 3. 将通知交给客户端事件循环。
		if len(message.ID) == 0 {
			select {
			case c.loopInputs <- &notificationReceived{
				connection: c,
				message:    &message,
			}:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		// 4. 按请求 ID 交付响应；忽略 id 为 null 等无法关联的响应。
		id, err := strconv.ParseInt(string(message.ID), 10, 64)
		if err != nil {
			continue
		}
		if reply, found := c.pendingCalls.LoadAndDelete(id); found {
			reply <- &message
		}
	}
}

// call 发出请求并等待响应，解码请求结果。
func (c *clientConnection) call(ctx context.Context, method string, params any, result any) error {
	// 1. 分配请求 ID，登记等待响应的通道。
	id := c.nextID.Add(1)
	reply := make(chan *wireMessage, 1)
	c.pendingCalls.Store(id, reply)
	defer c.pendingCalls.Delete(id)
	// 2. 编码并写出请求。
	payload, err := json.Marshal(&wireRequest{
		ID:     id,
		Method: method,
		Params: params,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if err := c.writeFrame(ctx, method, payload); err != nil {
		return err
	}
	// 3. 等待响应并解码到 result（result 为 nil 时不解码）；服务端错误经 getResponseError 转换，
	// 连接断开时返回 ErrConnectionLost，调用取消时返回取消错误。
	select {
	case message := <-reply:
		if message.Error != nil {
			return getResponseError(method, message.Error)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(message.Result, result)
	case <-c.closed:
		return fmt.Errorf("%s: %w", method, ErrConnectionLost)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// notify 写出一条 WebSocket 通知。
func (c *clientConnection) notify(ctx context.Context, method string, params any) error {
	// 1. 编码并写出通知后返回，不等待服务端响应；连接断开时返回 ErrConnectionLost。
	payload, err := json.Marshal(&wireNotification{
		Method: method,
		Params: params,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return c.writeFrame(ctx, method, payload)
}

// writeFrame 检查消息大小和取消状态，写出一帧请求或通知。
func (c *clientConnection) writeFrame(ctx context.Context, method string, payload []byte) error {
	// 1. 拒绝超过 serve 读取上限的消息。
	if len(payload) > maxRequestFrameBytes {
		return fmt.Errorf("%s: request frame is %d bytes, over the %d byte limit of the runtime server", method, len(payload), maxRequestFrameBytes)
	}
	// 2. 调用已经取消时直接返回；写出期间取消仍会关闭连接。
	if err := ctx.Err(); err != nil {
		return err
	}
	// 3. 写出文本帧，将连接错误转为 ErrConnectionLost。
	c.observeRPCFrame(RPCSent, payload)
	if err := c.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w", method, ErrConnectionLost)
	}
	return nil
}

// isClosed 连接的读循环是否已经结束？
func (c *clientConnection) isClosed() bool {
	// 1. 检查读循环结束通知是否已关闭。
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}
