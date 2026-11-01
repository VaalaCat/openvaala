package runtimeserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// serverConnection 是一个已认证的 WebSocket 连接在服务端的运行记录，生命周期与连接相同：
// 读任务是唯一读连接的任务，把帧交给请求主循环串行处理；writer 是唯一写连接的任务；订阅转发任务把 Runtime 事件翻译为通知交给 writer。
type serverConnection struct {
	conn          *websocket.Conn
	runtime       runtime.Runtime
	configuration Configuration
	modelCatalog  ModelCatalog
	// serverID 是 initialize 返回给客户端的 serve 标识。
	serverID string
	methods  map[string]methodHandler

	// outgoing 是待写出的响应与通知，按入队顺序写出。
	outgoing chan *runtimeprotocol.JSONRPCMessage
	// isInitialized 这个连接是否已经 initialize 成功？只由请求主循环读写。
	isInitialized bool
	// tasksAfterResponse 是当前请求登记的后台任务，在它的响应入队之后才启动，订阅结果先于该订阅的通知；只由请求主循环读写。
	tasksAfterResponse []func()
	// subscriptions 是本连接的订阅：键包含种类；目标订阅包含对话与 Session，全局运行订阅不带目标。
	subscriptions syncmap.SyncMap[subscriptionKey, *connectionSubscription]
	// connectionCtx 是 Run 的 ctx，即连接的生命周期；转发给客户端的 MCP 消息没有调用方 ctx，用它发送，连接结束时发送立即返回。
	// 由 Run 在启动任何任务之前设置，之后只读。
	connectionCtx context.Context
	// clientID 是这条连接在 Runtime 里登记客户端 MCP 时的 ID。
	clientID string
	// clientMCPConversations 是本连接登记过客户端 MCP 的对话：连接结束时对它们移除登记。只由请求主循环读写，Run 在主循环结束后读取。
	clientMCPConversations map[defs.ConversationID]struct{}
	// clientMCPStreams 是还没结束的转发流：stream_id 对应所属 server 的转发传输，客户端发来的消息按它分发；
	// 流由 clientMCPSender 在连接和断开时登记与移除，客户端发来 clientMCP/closed 或连接结束时也移除。
	clientMCPStreams syncmap.SyncMap[string, mcpclient.RelayTransport]
	// terminals 是本连接打开且还没结束的终端，键是 terminal_id：终端由 terminal/open 登记，进程结束时由终端自己的转发任务移除，
	// 连接结束时由 releaseTerminals 结束全部进程。
	terminals syncmap.SyncMap[string, *connectionTerminal]
	// tasks 是读任务、writer、心跳与各订阅转发任务；Run 返回前等待。
	tasks sync.WaitGroup
}

const (
	// maxFrameBytes 是服务端单帧读取上限，与 ACP 单行上限和 runtimeclient 发送上限一致。
	// prompt 中的 <、>、& 经 JSON 转义后可能变长，超过上限的请求由 runtimeclient 拒绝。
	maxFrameBytes      = 16 << 20
	pingInterval       = 30 * time.Second
	pingTimeout        = 15 * time.Second
	outgoingBufferSize = 16
	// incomingBufferSize 是读任务已读、请求主循环还没取走的帧数上限；满了读任务就停在交帧上，不再读新帧。
	incomingBufferSize = 4
	// clientMCPReleaseTimeout 是连接结束时向 Runtime 移除客户端 MCP 登记的等待上限，所有登记过的对话共用。
	clientMCPReleaseTimeout = 5 * time.Second
)

// newServerConnection 创建协议连接的处理实例。
func newServerConnection(conn *websocket.Conn, opt *RuntimeServerOpt, methods map[string]methodHandler) *serverConnection {
	// 1. 填入连接配置，创建发送队列、客户端 ID 和 MCP 对话登记表。
	return &serverConnection{
		conn:          conn,
		runtime:       opt.Runtime,
		configuration: opt.Configuration,
		modelCatalog:  opt.ModelCatalog,
		serverID:      opt.ServerID,
		methods:       methods,
		outgoing:      make(chan *runtimeprotocol.JSONRPCMessage, outgoingBufferSize),

		clientID:               uuid.NewString(),
		clientMCPConversations: make(map[defs.ConversationID]struct{}),
	}
}

// Run 运行连接的读写、心跳和请求处理，返回连接结束的原因。
func (c *serverConnection) Run(ctx context.Context) (closeReason error) {
	// 1. 创建带结束原因的连接上下文；退出时（含 panic）依次以结束原因取消连接、移除客户端 MCP、等待后台任务、关闭连接。
	ctx, cancel := context.WithCancelCause(ctx)
	c.connectionCtx = ctx
	defer func() {
		cancel(closeReason)
		c.releaseClientMCP(ctx)
		c.releaseTerminals(ctx)
		c.tasks.Wait()
		_ = c.conn.CloseNow()
	}()
	// 2. 设置帧上限，启动写出、心跳和读帧任务；写出或心跳结束时以各自的原因取消连接，先发生的原因就是连接结束原因。
	c.conn.SetReadLimit(maxFrameBytes)
	c.tasks.Go(func() {
		cancel(c.writeOutgoing(ctx))
	})
	c.tasks.Go(func() {
		cancel(c.keepAlive(ctx))
	})
	frames := make(chan receivedFrame, incomingBufferSize)
	c.tasks.Go(func() {
		c.readFrames(ctx, frames)
	})
	// 3. 串行处理请求，返回最先发生的取消原因；没有取消原因时返回读错误。
	readErr := c.handleRequest(ctx, frames)
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return readErr
}

// handleRequest 按帧到达顺序串行处理请求。
func (c *serverConnection) handleRequest(ctx context.Context, frames <-chan receivedFrame) error {
	// 1. 遇到读错误时返回；处理请求并入队响应，入队失败时返回取消原因。
	for frame := range frames {
		if frame.err != nil {
			return frame.err
		}
		if response := c.handleFrame(ctx, frame.payload); response != nil && !c.send(ctx, response) {
			return context.Cause(ctx)
		}
		// 2. 响应入队后启动本次登记的任务，清空登记后处理下一帧。
		for _, task := range c.tasksAfterResponse {
			c.tasks.Go(task)
		}
		c.tasksAfterResponse = nil
	}
	// 3. 读帧通道关闭（读任务因 ctx 结束退出，没交出读错误）时返回连接取消原因。
	return context.Cause(ctx)
}

// readFrames 持续读取帧并交给请求循环。
func (c *serverConnection) readFrames(ctx context.Context, frames chan<- receivedFrame) {
	// 1. 登记返回前关闭帧通道，逐帧读取；读操作同时处理 ping、pong 和 close 控制帧。
	defer close(frames)
	for {
		_, payload, err := c.conn.Read(ctx)
		if err != nil {
			err = fmt.Errorf("read frame: %w", err)
		}
		// 2. 按顺序交出帧或最后的读错误，上下文结束时停止交付。
		select {
		case frames <- receivedFrame{
			payload: payload,
			err:     err,
		}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// handleFrame 解析请求或通知，调用方法并生成响应。
func (c *serverConnection) handleFrame(ctx context.Context, frame []byte) *runtimeprotocol.JSONRPCMessage {
	// 1. 解析 JSON，失败时返回解析错误响应。
	var request runtimeprotocol.JSONRPCRequest
	if err := json.Unmarshal(frame, &request); err != nil {
		return newErrorResponse(nullID, newJSONRPCError(runtimeprotocol.ErrorCodeParse, "parse error"))
	}
	// 2. 没有 ID 且带方法时按通知处理；其他缺 ID 或方法的帧返回无效请求。
	if len(request.ID) == 0 && request.Method != "" {
		c.handleNotification(ctx, &request)
		return nil
	}
	if len(request.ID) == 0 || request.Method == "" {
		id := request.ID
		if len(id) == 0 {
			id = nullID
		}
		return newErrorResponse(id, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidRequest, "request requires id and method"))
	}
	// 3. 创建 RPC 遥测，分发方法并返回带原请求 ID 的结果或协议错误。
	ctx, recorder := telemetry.StartRPCServer(ctx, c.getTelemetryMethod(request.Method), request.Method, getRPCRequestID(request.ID))
	result, err := c.dispatchRequest(ctx, request.Method, request.Params)
	if err != nil {
		protocolErr := getJSONRPCError(err)
		recorder.End(protocolErr.Code, err)
		return &runtimeprotocol.JSONRPCMessage{
			ID:    request.ID,
			Error: protocolErr,
		}
	}
	recorder.End(0, nil)
	return &runtimeprotocol.JSONRPCMessage{
		ID:     request.ID,
		Result: result,
	}
}

// handleNotification 将客户端 MCP 消息与关闭通知交给对应转发流，把终端输入和尺寸变化交给对应终端；都不阻塞请求循环。
func (c *serverConnection) handleNotification(ctx context.Context, request *runtimeprotocol.JSONRPCRequest) {
	// 1. 按方法解析并交付；流或终端已结束时丢弃迟到通知，未知方法和无效参数记 Warn。
	switch request.Method {
	case "clientMCP/message":
		var params runtimeprotocol.ClientMCPMessageParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			telemetry.GetLogger(ctx).Warn("notification dropped", "rpc.method", request.Method, "error", err)
			return
		}
		if transport, found := c.clientMCPStreams.Load(params.StreamID); found {
			transport.DeliverMessage(params.StreamID, params.Message)
		}
	case "clientMCP/closed":
		var params runtimeprotocol.ClientMCPClosedParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			telemetry.GetLogger(ctx).Warn("notification dropped", "rpc.method", request.Method, "error", err)
			return
		}
		transport, found := c.clientMCPStreams.LoadAndDelete(params.StreamID)
		if !found {
			return
		}
		var reason error
		if params.Error != "" {
			reason = errors.New(params.Error)
		}
		transport.DeliverClosed(params.StreamID, reason)
	case "terminal/input":
		var params runtimeprotocol.TerminalInputParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			telemetry.GetLogger(ctx).Warn("notification dropped", "rpc.method", request.Method, "error", err)
			return
		}
		if terminal, found := c.terminals.Load(params.TerminalID); found {
			terminal.handleInput(params.Data)
		}
	case "terminal/resize":
		var params runtimeprotocol.TerminalResizeParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			telemetry.GetLogger(ctx).Warn("notification dropped", "rpc.method", request.Method, "error", err)
			return
		}
		if terminal, found := c.terminals.Load(params.TerminalID); found {
			terminal.handleResize(params.Rows, params.Cols)
		}
	default:
		telemetry.GetLogger(ctx).Warn("notification dropped", "rpc.method", request.Method, "reason", "unsupported method")
	}
}

// getTelemetryMethod 返回遥测使用的方法名。
func (c *serverConnection) getTelemetryMethod(method string) string {
	// 1. 已注册方法返回原名，其他方法归入 RPCMethodOther。
	if _, found := c.methods[method]; !found {
		return telemetry.RPCMethodOther
	}
	return method
}

// dispatchRequest 检查初始化状态，再按方法表调用请求处理函数。
func (c *serverConnection) dispatchRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	// 1. 拒绝重复初始化或初始化前的其他方法。
	if method == runtimeprotocol.InitializeMethod && c.isInitialized {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidRequest, "already initialized")
	}
	if method != runtimeprotocol.InitializeMethod && !c.isInitialized {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidRequest, "not initialized")
	}
	// 2. 查找处理函数，未注册时返回方法不存在错误。
	handler, found := c.methods[method]
	if !found {
		return nil, newJSONRPCError(runtimeprotocol.ErrorCodeMethodNotFound, "method not found: "+method)
	}
	// 3. 调用处理函数，initialize 成功后记录已初始化。
	result, err := handler.handle(ctx, c, params)
	if err == nil && method == runtimeprotocol.InitializeMethod {
		c.isInitialized = true
	}
	return result, err
}

// send 把消息加入发送队列，返回是否入队。
func (c *serverConnection) send(ctx context.Context, message *runtimeprotocol.JSONRPCMessage) bool {
	// 1. 入队成功返回 true，ctx 先结束时返回 false。
	select {
	case c.outgoing <- message:
		return true
	case <-ctx.Done():
		return false
	}
}

// sendNotification 使用连接上下文将通知加入发送队列。
func (c *serverConnection) sendNotification(method string, params any) error {
	// 1. 先检查连接是否结束，再发送通知；取消或入队失败时返回带连接结束原因的错误。
	if c.connectionCtx.Err() != nil || !c.send(c.connectionCtx, &runtimeprotocol.JSONRPCMessage{
		Method: method,
		Params: params,
	}) {
		return fmt.Errorf("send %s: %w", method, context.Cause(c.connectionCtx))
	}
	return nil
}

// startAfterResponse 登记当前响应入队后启动的后台任务。
func (c *serverConnection) startAfterResponse(task func()) {
	// 1. 将任务追加到当前请求的响应后任务列表。
	c.tasksAfterResponse = append(c.tasksAfterResponse, task)
}

// writeOutgoing 按入队顺序编码并写出响应和通知。
func (c *serverConnection) writeOutgoing(ctx context.Context) error {
	// 1. 等待待写消息，上下文结束时返回 nil。
	for {
		var message *runtimeprotocol.JSONRPCMessage
		select {
		case <-ctx.Done():
			return nil
		case message = <-c.outgoing:
		}
		// 2. 编码消息；通知无法编码时记 Warn 并丢弃，响应无法编码时改为 -32000 错误响应。
		payload, err := json.Marshal(message)
		if err != nil {
			if message.ID == nil {
				telemetry.GetLogger(ctx).Warn("notification dropped", "rpc.method", message.Method, "error", err)
				continue
			}
			payload, _ = json.Marshal(newErrorResponse(message.ID, err))
		}
		// 3. 写出文本帧，失败时返回写错误。
		if err := c.conn.Write(ctx, websocket.MessageText, payload); err != nil {
			return fmt.Errorf("write message: %w", err)
		}
	}
}

// keepAlive 定期发送 Ping 心跳。
func (c *serverConnection) keepAlive(ctx context.Context) error {
	// 1. 启动心跳定时器，返回时停止。
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	// 2. 每次到点发送限时 Ping，上下文结束返回 nil，Ping 失败返回错误。
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := c.conn.Ping(pingCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("ping: %w", err)
		}
	}
}

// registerSubscription 登记连接内的订阅，返回订阅上下文和登记记录。
func (c *serverConnection) registerSubscription(ctx context.Context, key subscriptionKey) (context.Context, *connectionSubscription, error) {
	// 1. 从连接上下文创建可取消订阅和转发完成通道。
	subscriptionCtx, cancel := context.WithCancel(ctx)
	registration := &connectionSubscription{
		cancel:        cancel,
		forwarderDone: make(chan struct{}),
	}
	// 2. 按目标登记，重复订阅时取消新上下文并返回 already subscribed。
	if _, loaded := c.subscriptions.LoadOrStore(key, registration); loaded {
		cancel()
		return nil, nil, newJSONRPCError(runtimeprotocol.ErrorCodeInvalidRequest, "already subscribed")
	}
	return subscriptionCtx, registration, nil
}

// cancelSubscription 取消已启动转发的订阅，等待转发任务结束。
func (c *serverConnection) cancelSubscription(ctx context.Context, key subscriptionKey) {
	// 1. 移除订阅登记，不存在时直接返回。
	registration, found := c.subscriptions.LoadAndDelete(key)
	if !found {
		return
	}
	// 2. 取消订阅，等待转发任务将剩余通知和 subscription/closed 入队后结束；连接结束时停止等待。
	registration.cancel()
	select {
	case <-registration.forwarderDone:
	case <-ctx.Done():
	}
}

// removeSubscription 移除指定登记并取消订阅。
func (c *serverConnection) removeSubscription(key subscriptionKey, registration *connectionSubscription) {
	// 1. 仅删除仍为该订阅的登记，保留同目标的新订阅，再取消旧上下文，不等待转发任务。
	c.subscriptions.DeleteIf(key, func(registered *connectionSubscription) bool { return registered == registration })
	registration.cancel()
}

// newClientMCPTransport 创建经本连接转发指定客户端 MCP server 消息的传输。
func (c *serverConnection) newClientMCPTransport(serverID string) mcpclient.RelayTransport {
	// 1. 创建消息发送者和转发传输，发送者在连接时登记 stream_id。
	sender := &clientMCPSender{connection: c}
	sender.transport = mcpclient.NewRelayTransport(mcpclient.NewRelayTransportOpt(serverID, sender))
	return sender.transport
}

// releaseClientMCP 释放本连接的客户端 MCP 转发与登记。
func (c *serverConnection) releaseClientMCP(ctx context.Context) {
	// 1. 以连接取消原因为错误，移除并结束全部转发流，使进行中的工具调用失败。
	reason := fmt.Errorf("client connection closed: %w", context.Cause(ctx))
	c.clientMCPStreams.Range(func(streamID string, transport mcpclient.RelayTransport) bool {
		c.clientMCPStreams.Delete(streamID)
		transport.DeliverClosed(streamID, reason)
		return true
	})
	// 2. 创建不随连接取消的限时上下文，所有对话共用移除时限。
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clientMCPReleaseTimeout)
	defer cancel()
	// 3. 逐个移除客户端 MCP 登记；Runtime 停止和对话未打开的错误忽略，其余记 Warn。
	for conversationID := range c.clientMCPConversations {
		err := c.runtime.SetClientMCPServers(removeCtx, &runtime.SetClientMCPServersRequest{
			ConversationID: conversationID,
			ClientID:       c.clientID,
		})
		if err != nil && !errors.Is(err, runtime.ErrConversationNotOpen) && !errors.Is(err, runtime.ErrRuntimeStopped) {
			telemetry.GetLogger(ctx).Warn("remove client MCP servers failed", "gen_ai.conversation.id", string(conversationID), "error", err)
		}
	}
}

// releaseTerminals 结束本连接打开的全部终端进程，终端的转发任务随之关闭各自的工作区。
func (c *serverConnection) releaseTerminals(ctx context.Context) {
	// 1. 创建不随连接取消的限时上下文，所有终端共用结束时限。
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalReleaseTimeout)
	defer cancel()
	// 2. 并行结束各终端进程；结束失败记 Warn，等全部返回后离开。
	var tasks sync.WaitGroup
	c.terminals.Range(func(terminalID string, terminal *connectionTerminal) bool {
		tasks.Go(func() {
			if err := terminal.terminate(releaseCtx); err != nil {
				telemetry.GetLogger(ctx).Warn("terminate terminal failed", "terminal.id", terminalID, "error", err)
			}
		})
		return true
	})
	tasks.Wait()
}

// clientMCPSender 保存一次客户端 MCP server 登记对应的连接和传输，向客户端发送转发通知。
// 同一 server 可在多个对话登记，各次登记分别保存 sender；每次连接的 stream_id 对应各自的传输。
type clientMCPSender struct {
	connection *serverConnection
	transport  mcpclient.RelayTransport
}

// SendMCPConnect 登记转发流并通知客户端连接 MCP server。
func (s *clientMCPSender) SendMCPConnect(streamID, serverID string) error {
	// 1. 先登记 stream_id 对应传输，再发送连接通知；发送失败时撤销登记。
	s.connection.clientMCPStreams.Store(streamID, s.transport)
	err := s.connection.sendNotification("clientMCP/connect", &runtimeprotocol.ClientMCPConnectParams{
		StreamID: streamID,
		ServerID: serverID,
	})
	if err != nil {
		s.connection.clientMCPStreams.Delete(streamID)
	}
	return err
}

// SendMCPMessage 将转发流的一条 MCP 消息通知客户端。
func (s *clientMCPSender) SendMCPMessage(streamID string, message json.RawMessage) error {
	// 1. 发送 stream_id 和完整 MCP JSON 消息。
	return s.connection.sendNotification("clientMCP/message", &runtimeprotocol.ClientMCPMessageParams{
		StreamID: streamID,
		Message:  message,
	})
}

// SendMCPDisconnect 移除转发流登记并通知客户端断开。
func (s *clientMCPSender) SendMCPDisconnect(streamID string) error {
	// 1. 移除 stream_id，发送断开通知；后续迟到消息丢弃。
	s.connection.clientMCPStreams.Delete(streamID)
	return s.connection.sendNotification("clientMCP/disconnect", &runtimeprotocol.ClientMCPDisconnectParams{StreamID: streamID})
}
