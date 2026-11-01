package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connection 是到一个 MCP server 的连接；执行环境自己的 MCP server 和客户端登记的 MCP server 都用它。
type Connection interface {
	// Run 持有连接状态：启动后马上连接；断开后立刻重连一次，再断开要等下一次 GetTools 才连；收到 tools/list_changed 后重读工具列表；
	// ctx 结束时断开，stdio 进程随之结束，返回 ctx.Err()。只能调用一次。
	Run(ctx context.Context) error
	// GetTools 返回提供给模型的工具（名字已改成 mcp__<id>__<原名>，MCPServerID 已填）：已连上时返回缓存；
	// 正在连时等这次连接；失败过时再连一次；最多等启动超时，连不上返回错误。Run 已返回时返回错误；
	// Run 还没开始时等它开始，最多等到 ctx 结束。
	GetTools(ctx context.Context) ([]*defs.ToolDefinition, error)
	// CallTool 在已连上的会话上调用一次工具；没连上时返回 mcp_server_unavailable，超时、取消、server 报错和中途断开也都写进结果的 Error。
	// 工具名不在本连接的工具列表里时返回 mcp_request_failed，不发给 server。
	CallTool(ctx context.Context, call *defs.Item) *defs.ToolResult
	// IsRequired 这个 server 是不是强依赖：连不上时让这一轮报错，而不是跳过？
	IsRequired() bool
	// GetInfo 返回连接此刻的情况；还没连上时也立刻返回。
	GetInfo() ConnectionInfo
}

// connectionImpl 的连接状态只由 Run 的事件循环修改，GetTools、CallTool、GetInfo 经 connectionEvents 交给它；
// 每次连接尝试由一个 connectionSession 在后台完成并报告结果，事件循环继续处理其他请求。
type connectionImpl struct {
	opt ConnectionOpt
	// connectionEvents 承载调用方请求和连接会话报告的结果，只由事件循环读取。
	connectionEvents chan connectionEvent
	// isRunStarted Run 是否已经开始？只接受第一次 Run；没开始时 GetInfo 报告 connecting。
	isRunStarted atomic.Bool
	// eventLoopStopped 在事件循环退出时关闭：之后的请求直接报错，GetInfo 直接读取不再变化的最终状态。
	eventLoopStopped chan struct{}
	// tasks 是事件循环启动的连接会话，Run 返回前等它们结束进程、关闭连接。
	tasks sync.WaitGroup

	// 以下只由事件循环读写，eventLoopStopped 关闭后不再修改。
	// state 是 GetInfo 报告的状态：第一次连接有结果之前是 connecting；之后连上为 connected，失败或断开为 failed，
	// 再次连接期间保持 failed，直到连上。
	state ConnectionState
	// isConnecting 是否有一次连接尝试正在进行？同一时间最多一次。
	isConnecting bool
	// clientSession 是已连上的 SDK 会话；没连上时为 nil。
	clientSession *mcp.ClientSession
	// tools 是已连上的 server 提供的工具，已按 ToolNames 过滤并改名；没连上时为 nil。
	tools []ConnectionTool
	// connectionError 是最近一次连接失败或断开的原因；连上后清空。
	connectionError string
	// toolsWaiters 是等正在进行的连接尝试的 GetTools 回复通道（容量 1），这次尝试有结果时统一回复。
	toolsWaiters []chan toolsResult
	// isDropReconnectUsed 上次 GetTools 之后是否已经因为断开立即重连过一次？重连过后再断开，要等下一次 GetTools 才连接。
	isDropReconnectUsed bool
}

// ConnectionOpt 是一个连接的配置和运行位置；Workspace 与 Transport 二选一。
type ConnectionOpt struct {
	Definition *ServerDefinition
	// Workspace 是运行位置：stdio 用 StartProcess 启动，http、sse 用 Dial 连接。
	Workspace workspace.Workspace
	// Transport 是客户端 MCP 用的消息转发传输（RelayTransport）：MCP 消息经客户端连接转发给客户端侧的 MCP server。
	Transport mcp.Transport
}

const (
	// processExitTimeout 是关闭 stdio server 的 stdin 后等它自己退出的时间，到时 Terminate。
	processExitTimeout = 5 * time.Second
	// maxStderrTailBytes 是 stdio server 的 stderr 保留下来、接在失败原因后面的字节数。
	maxStderrTailBytes = 2 * 1024
)

// errConnectionNotRunning 是事件循环退出（Run 结束）后，发来的请求和还在等工具的 GetTools 收到的错误。
var errConnectionNotRunning = errors.New("MCP connection is not running")

// NewConnectionOpt 保存 MCP server 定义及运行工作区。
func NewConnectionOpt(definition *ServerDefinition, executorWorkspace workspace.Workspace) *ConnectionOpt {
	// 1. 创建连接配置。
	return &ConnectionOpt{
		Definition: definition,
		Workspace:  executorWorkspace,
	}
}

// WithTransport 设置客户端消息转发传输并返回本配置。
func (o *ConnectionOpt) WithTransport(transport mcp.Transport) *ConnectionOpt {
	// 1. 保存传输对象；设了传输时 Workspace 必须为 nil。
	o.Transport = transport
	return o
}

// NewConnection 校验配置，创建还没运行的连接。
func NewConnection(opt *ConnectionOpt) (Connection, error) {
	// 1. 检查 server ID，要求 Workspace 和 Transport 恰好设置一个。
	if opt == nil || opt.Definition == nil || opt.Definition.ID == "" {
		return nil, errors.New("MCP connection requires a server definition with an ID")
	}
	if (opt.Workspace == nil) == (opt.Transport == nil) {
		return nil, fmt.Errorf("MCP connection %q requires exactly one of a workspace and a transport", opt.Definition.ID)
	}
	return &connectionImpl{
		opt:              *opt,
		connectionEvents: make(chan connectionEvent),
		eventLoopStopped: make(chan struct{}),
		state:            ConnectionStateConnecting,
	}, nil
}

// Run 运行连接事件循环，结束时等待全部连接会话退出。
func (c *connectionImpl) Run(ctx context.Context) error {
	// 1. 检查 Run 只调用一次，开始首次连接尝试。
	if !c.isRunStarted.CompareAndSwap(false, true) {
		return errors.New("MCP connection can only run once")
	}
	c.startConnectionAttempt(ctx)
	// 2. 处理事件直到取消，等待所有连接会话结束后返回 ctx.Err()。
	c.runEventLoop(ctx)
	c.tasks.Wait()
	return ctx.Err()
}

// runEventLoop 处理连接请求和会话通知。
func (c *connectionImpl) runEventLoop(ctx context.Context) {
	// 1. 依次分发工具请求、调用请求、状态查询和会话结果。
	for {
		select {
		case event := <-c.connectionEvents:
			switch event := event.(type) {
			case toolsRequested:
				c.handleToolsRequested(ctx, event)
			case toolCallRequested:
				c.handleToolCallRequested(event)
			case infoRequested:
				event.reply <- c.getInfo()
			case connectionAttemptFinished:
				c.handleConnectionAttemptFinished(ctx, event)
			case toolListReloadFinished:
				c.handleToolListReloadFinished(ctx, event)
			case connectionSessionDisconnected:
				c.handleConnectionSessionDisconnected(ctx, event)
			default:
				panic(fmt.Sprintf("unsupported MCP connection event %T", event))
			}
		case <-ctx.Done():
			// 2. 回复等待者，保存失败终态，再关闭事件循环结束通道。
			c.replyToolsWaiters(errConnectionNotRunning)
			c.state, c.isConnecting, c.clientSession, c.tools = ConnectionStateFailed, false, nil, nil
			c.connectionError = errConnectionNotRunning.Error()
			close(c.eventLoopStopped)
			return
		}
	}
}

// handleToolsRequested 返回已缓存的工具，或登记等待连接结果的请求。
func (c *connectionImpl) handleToolsRequested(ctx context.Context, request toolsRequested) {
	// 1. 清除断开后立即重连的标记；已连上时立即回复工具定义。
	c.isDropReconnectUsed = false
	if c.state == ConnectionStateConnected {
		request.reply <- toolsResult{tools: newModelToolDefinitions(c.opt.Definition.ID, c.tools)}
		return
	}
	// 2. 尚未连接时复用正在进行的尝试，没有尝试则启动一次，再登记回复通道。
	if !c.isConnecting {
		c.startConnectionAttempt(ctx)
	}
	c.toolsWaiters = append(c.toolsWaiters, request.reply)
}

// handleToolCallRequested 回复工具调用要用的会话和工具原名。
func (c *connectionImpl) handleToolCallRequested(request toolCallRequested) {
	// 1. 检查连接状态，未连接时回复 mcp_server_unavailable。
	if c.state != ConnectionStateConnected {
		request.reply <- toolCallTarget{
			errorCode: "mcp_server_unavailable",
			err:       c.getNotConnectedError(),
		}
		return
	}
	// 2. 查找工具模型名，回复工具原名和会话，未找到时返回 mcp_request_failed。
	index := slices.IndexFunc(c.tools, func(tool ConnectionTool) bool { return tool.Name == request.toolName })
	if index < 0 {
		request.reply <- toolCallTarget{
			errorCode: "mcp_request_failed",
			err:       fmt.Errorf("MCP server %q does not provide tool %q", c.opt.Definition.ID, request.toolName),
		}
		return
	}
	request.reply <- toolCallTarget{
		clientSession: c.clientSession,
		toolName:      c.tools[index].OriginalName,
	}
}

// handleConnectionAttemptFinished 保存连接尝试结果并回复工具等待者。
func (c *connectionImpl) handleConnectionAttemptFinished(ctx context.Context, finished connectionAttemptFinished) {
	// 1. 结束连接中标记，按成功或失败更新状态、日志和工具等待结果。
	c.isConnecting = false
	logger := c.getLogger(ctx)
	if finished.err != nil {
		c.state, c.connectionError = ConnectionStateFailed, finished.err.Error()
		logger.Warn("mcp server connection failed", "error", finished.err)
		c.replyToolsWaiters(finished.err)
		return
	}
	c.state, c.clientSession, c.tools, c.connectionError = ConnectionStateConnected, finished.clientSession, finished.tools, ""
	logger.Info("mcp server connected", "tools", len(finished.tools))
	c.replyToolsWaiters(nil)
}

// handleToolListReloadFinished 处理工具列表重读结果。
func (c *connectionImpl) handleToolListReloadFinished(ctx context.Context, finished toolListReloadFinished) {
	// 1. 失败时保留原列表并记录 Warn，成功时保存新工具列表。
	if finished.err != nil {
		c.getLogger(ctx).Warn("mcp server tools reload failed", "error", finished.err)
		return
	}
	c.tools = finished.tools
}

// handleConnectionSessionDisconnected 记录连接断开并安排重连。
func (c *connectionImpl) handleConnectionSessionDisconnected(ctx context.Context, disconnected connectionSessionDisconnected) {
	// 1. 清空会话和工具，保存断开原因并记录 Warn。
	c.state, c.clientSession, c.tools, c.connectionError = ConnectionStateFailed, nil, nil, disconnected.err.Error()
	c.getLogger(ctx).Warn("mcp server disconnected", "error", disconnected.err)
	// 2. 上次 GetTools 之后还没立即重连过时发起一次；已经重连过就等下一次 GetTools。
	if c.isDropReconnectUsed {
		return
	}
	c.isDropReconnectUsed = true
	c.startConnectionAttempt(ctx)
}

// startConnectionAttempt 创建并后台运行一次连接会话。
func (c *connectionImpl) startConnectionAttempt(ctx context.Context) {
	// 1. 标记正在连接，创建会话并放进 tasks 后台运行。
	c.isConnecting = true
	session := newConnectionSession(&c.opt, c.connectionEvents)
	c.tasks.Go(func() { session.Run(ctx) })
}

// replyToolsWaiters 回复全部工具等待者。
func (c *connectionImpl) replyToolsWaiters(err error) {
	// 1. 有错误时回复错误，否则为每个等待者生成工具定义，回复后清空等待列表。
	for _, waiter := range c.toolsWaiters {
		if err != nil {
			waiter <- toolsResult{err: err}
			continue
		}
		waiter <- toolsResult{tools: newModelToolDefinitions(c.opt.Definition.ID, c.tools)}
	}
	c.toolsWaiters = nil
}

// GetTools 向事件循环请求模型工具列表。
func (c *connectionImpl) GetTools(ctx context.Context) ([]*defs.ToolDefinition, error) {
	// 1. 发送工具请求，再等待回复或调用取消；Run 未开始时等待它开始。
	reply := make(chan toolsResult, 1)
	if err := c.sendConnectionEvent(ctx, toolsRequested{reply: reply}); err != nil {
		return nil, err
	}
	select {
	case result := <-reply:
		return result.tools, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CallTool 在已有 MCP 会话上调用一次工具，并转换结果或错误。
func (c *connectionImpl) CallTool(ctx context.Context, call *defs.Item) *defs.ToolResult {
	// 1. 请求已连接会话和工具原名，处理取消、未连接及工具不存在。
	callID := call.ToolCall.CallID
	reply := make(chan toolCallTarget, 1)
	if err := c.sendConnectionEvent(ctx, toolCallRequested{
		toolName: call.ToolCall.Name,
		reply:    reply,
	}); err != nil {
		if ctx.Err() != nil {
			return newToolErrorResult(callID, "canceled", fmt.Errorf("MCP tool call canceled: %w", err))
		}
		return newToolErrorResult(callID, "mcp_server_unavailable", fmt.Errorf("MCP server %q is not connected: %w", c.opt.Definition.ID, err))
	}
	target := <-reply
	if target.err != nil {
		return newToolErrorResult(callID, target.errorCode, target.err)
	}

	// 2. 设置工具超时并发送调用，按结果区分取消、超时、协议错误和断开。
	toolTimeout := c.opt.Definition.ToolTimeout
	callCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	params := &mcp.CallToolParams{Name: target.toolName}
	if call.ToolCall.Arguments != "" {
		params.Arguments = json.RawMessage(call.ToolCall.Arguments)
	}
	result, err := target.clientSession.CallTool(callCtx, params)
	var wireError *jsonrpc.Error
	switch {
	case err == nil:
		return newToolResult(callID, result)
	case ctx.Err() != nil:
		return newToolErrorResult(callID, "canceled", fmt.Errorf("MCP tool call canceled: %w", ctx.Err()))
	case callCtx.Err() != nil:
		return newToolErrorResult(callID, "timeout", fmt.Errorf("MCP tool call timed out after %s", toolTimeout))
	case errors.As(err, &wireError):
		return newToolErrorResult(callID, "mcp_request_failed", wireError)
	default:
		return newToolErrorResult(callID, "mcp_server_unavailable", fmt.Errorf("MCP server %q disconnected during the call: %w", c.opt.Definition.ID, err))
	}
}

// IsRequired 这个 MCP server 是不是强依赖？
func (c *connectionImpl) IsRequired() bool {
	// 1. 读取 server 配置中的 Required。
	return c.opt.Definition.Required
}

// GetInfo 返回连接此刻的状态。
func (c *connectionImpl) GetInfo() ConnectionInfo {
	// 1. Run 还没开始时直接报告 connecting。
	if !c.isRunStarted.Load() {
		return ConnectionInfo{
			ServerID:   c.opt.Definition.ID,
			State:      ConnectionStateConnecting,
			Required:   c.opt.Definition.Required,
			PluginName: getPluginName(c.opt.Definition),
		}
	}
	// 2. 向事件循环查询；事件循环已退出时读取保存下来的最终状态。
	reply := make(chan ConnectionInfo, 1)
	if err := c.sendConnectionEvent(context.Background(), infoRequested{reply: reply}); err != nil {
		return c.getInfo()
	}
	return <-reply
}

// sendConnectionEvent 把请求交给事件循环。
func (c *connectionImpl) sendConnectionEvent(ctx context.Context, event connectionEvent) error {
	// 1. 等待事件接收，循环退出或 ctx 取消时返回对应错误。
	select {
	case c.connectionEvents <- event:
		return nil
	case <-c.eventLoopStopped:
		return errConnectionNotRunning
	case <-ctx.Done():
		return ctx.Err()
	}
}

// getInfo 根据保存的连接状态创建状态信息。
func (c *connectionImpl) getInfo() ConnectionInfo {
	// 1. 填入 server、工具、最近错误和插件名称。
	return ConnectionInfo{
		ServerID:   c.opt.Definition.ID,
		State:      c.state,
		Required:   c.opt.Definition.Required,
		Tools:      c.tools,
		Error:      c.connectionError,
		PluginName: getPluginName(c.opt.Definition),
	}
}

// getNotConnectedError 创建未连接错误，并附带最近的失败原因。
func (c *connectionImpl) getNotConnectedError() error {
	// 1. 没有失败原因时报告尚未连接，否则附带保存的错误。
	if c.connectionError == "" {
		return fmt.Errorf("MCP server %q is not connected yet", c.opt.Definition.ID)
	}
	return fmt.Errorf("MCP server %q is not connected: %s", c.opt.Definition.ID, c.connectionError)
}

// getLogger 返回带 MCP server ID 的日志对象。
func (c *connectionImpl) getLogger(ctx context.Context) *slog.Logger {
	// 1. 在上下文日志对象上补充 server ID。
	return telemetry.GetLogger(ctx).With("mcp_server_id", c.opt.Definition.ID)
}

// connectionSession 是一次连接尝试和它建立的会话：建立 stdio 进程或 HTTP 传输、完成 MCP 握手并读工具列表，
// 连上后转告工具列表变化和断开。事件循环为每次连接尝试新建一个，Run 返回前释放它的进程和连接。
type connectionSession struct {
	opt              *ConnectionOpt
	connectionEvents chan<- connectionEvent
	// toolListChanged 是 server 发来 tools/list_changed 的信号，容量 1，连续的通知合并成一次重读。
	toolListChanged chan struct{}

	// 以下只由 Run 的 goroutine 读写。
	clientSession *mcp.ClientSession
	// serverProcess 只在 stdio 时设置。
	serverProcess *serverProcess
	// serverRoundTripper 只在 http、sse 时设置。
	serverRoundTripper *serverRoundTripper
}

// newConnectionSession 创建一次连接尝试使用的会话。
func newConnectionSession(opt *ConnectionOpt, connectionEvents chan<- connectionEvent) *connectionSession {
	// 1. 保存配置及事件通道，创建合并工具变化通知的通道。
	return &connectionSession{
		opt:              opt,
		connectionEvents: connectionEvents,
		toolListChanged:  make(chan struct{}, 1),
	}
}

// Run 连接 MCP server 并报告结果，随后处理工具变化和断开。
func (s *connectionSession) Run(ctx context.Context) {
	// 1. 创建连接上下文，启动超时计时并尝试握手、读取工具；超时只取消连接。
	// 连接成功后 connectCtx 保持到会话结束，报告结果使用原 ctx。
	connectCtx, cancelConnect := context.WithCancel(ctx)
	defer cancelConnect()
	startupTimeout := s.opt.Definition.StartupTimeout
	startupTimer := time.AfterFunc(startupTimeout, cancelConnect)
	tools, err := s.openClientSession(connectCtx)
	if !startupTimer.Stop() {
		err = fmt.Errorf("MCP server did not start within %s", startupTimeout)
	}
	if err != nil {
		// 先报告失败，再释放进程和连接。
		s.sendConnectionEvent(ctx, connectionAttemptFinished{err: s.getFailureError(err)})
		s.close()
		return
	}
	s.sendConnectionEvent(ctx, connectionAttemptFinished{
		clientSession: s.clientSession,
		tools:         tools,
	})

	// 2. 等待会话断开，收到列表变化时重读；取消时关闭资源并等待会话退出。
	clientSessionClosed := make(chan error, 1)
	go func() { clientSessionClosed <- s.clientSession.Wait() }()
	for {
		select {
		case <-s.toolListChanged:
			tools, err := s.reloadTools(ctx)
			s.sendConnectionEvent(ctx, toolListReloadFinished{
				tools: tools,
				err:   err,
			})
		case err := <-clientSessionClosed:
			reason := errors.New("MCP server closed the connection")
			if err != nil {
				reason = fmt.Errorf("MCP server disconnected: %w", err)
			}
			s.sendConnectionEvent(ctx, connectionSessionDisconnected{err: s.getFailureError(reason)})
			s.close()
			return
		case <-ctx.Done():
			s.close()
			<-clientSessionClosed
			return
		}
	}
}

// openClientSession 创建传输、完成握手并读取工具列表。
func (s *connectionSession) openClientSession(ctx context.Context) ([]ConnectionTool, error) {
	// 1. 建立传输，创建监听工具列表变化的客户端。
	transport, err := s.newTransport(ctx)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "openvaala"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { s.signalToolListChanged() },
		// 客户端不声明 roots、sampling 和 elicitation 能力。
		Capabilities: &mcp.ClientCapabilities{},
	})
	// 2. 连接并保存 SDK 会话，读取工具列表。
	clientSession, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	s.clientSession = clientSession
	return s.listTools(ctx)
}

// newTransport 按配置返回转发、stdio、HTTP 或 SSE 传输。
func (s *connectionSession) newTransport(ctx context.Context) (mcp.Transport, error) {
	// 1. 已有转发传输时直接返回，否则按 server 类型启动进程或创建网络传输。
	if s.opt.Transport != nil {
		return s.opt.Transport, nil
	}
	definition := s.opt.Definition
	switch definition.Type {
	case ServerTransportStdio:
		serverProcess, err := startServerProcess(ctx, s.opt)
		if err != nil {
			return nil, err
		}
		s.serverProcess = serverProcess
		return &mcp.IOTransport{
			Reader: serverProcess.stdoutReader,
			Writer: serverProcess.stdinWriter,
		}, nil
	case ServerTransportHTTP:
		return &mcp.StreamableClientTransport{
			Endpoint:   definition.URL,
			HTTPClient: s.newHTTPClient(),
		}, nil
	case ServerTransportSSE:
		return &mcp.SSEClientTransport{
			Endpoint:   definition.URL,
			HTTPClient: s.newHTTPClient(),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported MCP server type %q", definition.Type)
	}
}

// newHTTPClient 创建使用工作区拨号和代理配置的 HTTP 客户端。
func (s *connectionSession) newHTTPClient() *http.Client {
	// 1. 复制 HTTP 传输，设置工作区代理、拨号及请求头。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = s.opt.Workspace.GetHTTPProxy
	transport.DialContext = s.opt.Workspace.Dial
	s.serverRoundTripper = &serverRoundTripper{
		transport: transport,
		headers:   s.opt.Definition.Headers,
	}
	return &http.Client{Transport: s.serverRoundTripper}
}

// listTools 读取 server 全部工具页并按配置转换。
func (s *connectionSession) listTools(ctx context.Context) ([]ConnectionTool, error) {
	// 1. server 未声明 tools 能力时返回空列表。
	capabilities := s.clientSession.InitializeResult().Capabilities
	if capabilities == nil || capabilities.Tools == nil {
		return nil, nil
	}
	// 2. 读完全部工具页，应用名称筛选及模型名转换。
	var serverTools []*mcp.Tool
	for tool, err := range s.clientSession.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list MCP tools: %w", err)
		}
		serverTools = append(serverTools, tool)
	}
	return newConnectionTools(s.opt.Definition, serverTools)
}

// reloadTools 重读 MCP 工具列表。
func (s *connectionSession) reloadTools(ctx context.Context) ([]ConnectionTool, error) {
	// 1. 以配置的启动超时限制本次读取，再调用工具列表读取。
	ctx, cancel := context.WithTimeout(ctx, s.opt.Definition.StartupTimeout)
	defer cancel()
	return s.listTools(ctx)
}

// signalToolListChanged 通知会话重读工具，合并尚未处理的重复信号。
func (s *connectionSession) signalToolListChanged() {
	// 1. 向容量为一的通道发送信号，已有信号时跳过。
	select {
	case s.toolListChanged <- struct{}{}:
	default:
	}
}

// sendConnectionEvent 把会话结果交给事件循环。
func (s *connectionSession) sendConnectionEvent(ctx context.Context, event connectionEvent) {
	// 1. 等待结果被接收，ctx 结束时丢弃结果。
	select {
	case s.connectionEvents <- event:
	case <-ctx.Done():
	}
}

// getFailureError 为连接失败附加进程退出、stderr 或 HTTP 认证状态。
func (s *connectionSession) getFailureError(err error) error {
	// 1. stdio 已退出时取进程错误，并附上 stderr 尾部。
	if s.serverProcess != nil {
		if exitError := s.serverProcess.exitError.Load(); exitError != nil {
			err = *exitError
		}
		if stderr := s.serverProcess.stderr.getLastLines(); stderr != "" {
			return fmt.Errorf("%w; stderr: %s", err, stderr)
		}
	}
	// 2. HTTP 或 SSE 认证失败时附带最近的 HTTP 状态行。
	if s.serverRoundTripper != nil {
		if status := s.serverRoundTripper.rejectedStatus.Load(); status != nil {
			return fmt.Errorf("%w (HTTP %s)", err, *status)
		}
	}
	return err
}

// close 释放 MCP 会话使用的进程和连接。
func (s *connectionSession) close() {
	// 1. 依次等待进程结束、关闭会话和空闲连接。
	if s.serverProcess != nil {
		s.serverProcess.close()
	}
	if s.clientSession != nil {
		_ = s.clientSession.Close()
	}
	if s.serverRoundTripper != nil {
		s.serverRoundTripper.transport.CloseIdleConnections()
	}
}

// serverProcess 是 stdio server 的进程和接在它上面的管道。
type serverProcess struct {
	process workspace.Process
	// logger 带 mcp_server_id，记录 close 时结束或放手进程失败。
	logger *slog.Logger
	// exited 在进程结束、stdout 管道随之关闭后关闭。
	exited chan struct{}
	// exitError 是进程已经退出的原因（退出码或等待进程失败）；进程退出时先于关闭 stdout 管道写入，仍在运行时为 nil。
	exitError    atomic.Pointer[error]
	stdinWriter  *os.File
	stdoutReader *io.PipeReader
	stderr       *processStderr
}

// startServerProcess 经工作区启动 stdio server 并返回进程记录。
func startServerProcess(ctx context.Context, opt *ConnectionOpt) (*serverProcess, error) {
	// 1. 创建 stdin 文件管道和 stdout 管道。
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutReader, stdoutWriter := io.Pipe()
	logger := telemetry.GetLogger(ctx).With("mcp_server_id", opt.Definition.ID)
	stderr := &processStderr{logger: logger}
	// 2. 在工作区根目录启动 server，stderr 交给日志和尾部缓存。
	process, err := opt.Workspace.StartProcess(ctx, &workspace.ProcessRequest{
		Command:   getProcessCommand(opt.Definition),
		Directory: opt.Workspace.GetRootDirectory(),
		Stdin:     stdinReader,
		Stdout:    stdoutWriter,
		Stderr:    stderr,
	})
	if err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
		return nil, fmt.Errorf("start MCP server process: %w", err)
	}
	started := &serverProcess{
		process:      process,
		logger:       logger,
		exited:       make(chan struct{}),
		stdinWriter:  stdinWriter,
		stdoutReader: stdoutReader,
		stderr:       stderr,
	}
	// 3. 启动后台任务等待进程退出，退出后先保存原因，再关闭 stdout 和 stdin 读端。
	go func() {
		defer close(started.exited)
		exitCode, err := process.Wait()
		if err == nil {
			err = fmt.Errorf("MCP server process exited with code %d", exitCode)
		} else {
			err = fmt.Errorf("wait for MCP server process: %w", err)
		}
		started.exitError.Store(&err)
		_ = stdoutWriter.CloseWithError(err)
		_ = stdinReader.Close()
	}()
	return started, nil
}

// close 停止 stdio server 并等待进程收尾。
func (p *serverProcess) close() {
	// 1. 关闭 stdin，最多等待 processExitTimeout。
	_ = p.stdinWriter.Close()
	timer := time.NewTimer(processExitTimeout)
	defer timer.Stop()
	select {
	case <-p.exited:
		return
	case <-timer.C:
	}
	// 2. 超时后终止进程，终止失败时关闭进程句柄并记录错误。
	if err := p.process.Terminate(); err != nil {
		p.logger.Warn("terminate process failed", "error", err)
		if err := p.process.Close(); err != nil {
			p.logger.Warn("close process failed", "error", err)
		}
	}
	// 3. 关闭 stdout 读端，等待输出复制和进程 Wait 结束。
	_ = p.stdoutReader.CloseWithError(errors.New("MCP server process terminated"))
	<-p.exited
}

// processStderr 接收 stdio server 的 stderr：每行记一条 Debug 日志，并保留最后 2KB 供失败原因使用。
// 由工作区的输出复制任务写入，连接会话在失败或断开时读取。
type processStderr struct {
	logger *slog.Logger
	mu     sync.Mutex
	// partialLine 是还没遇到换行的一行。
	partialLine []byte
	// lastBytes 是最近写入的最多 maxStderrTailBytes 字节。
	lastBytes []byte
	// isTruncated 更早的输出是否已经丢弃？
	isTruncated bool
}

// Write 记录 stderr 日志并保留末尾内容。
func (s *processStderr) Write(data []byte) (int, error) {
	// 1. 加锁追加 stderr，保留最近 2 KB 并记录是否截断。
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastBytes = append(s.lastBytes, data...)
	if extra := len(s.lastBytes) - maxStderrTailBytes; extra > 0 {
		s.lastBytes, s.isTruncated = slices.Clone(s.lastBytes[extra:]), true
	}
	// 2. 合并尚未完成的行，每遇到换行就写入一条 Debug 日志。
	s.partialLine = append(s.partialLine, data...)
	for {
		line, rest, found := bytes.Cut(s.partialLine, []byte("\n"))
		if !found {
			break
		}
		s.logger.Debug("mcp server stderr", "line", string(line))
		s.partialLine = rest
	}
	// 3. 未遇换行但已超过 2 KB 时记录当前片段并清空。
	if len(s.partialLine) > maxStderrTailBytes {
		s.logger.Debug("mcp server stderr", "line", string(s.partialLine))
		s.partialLine = nil
	}
	return len(data), nil
}

// getLastLines 返回有效 UTF-8 的 stderr 尾部文本。
func (s *processStderr) getLastLines() string {
	// 1. 加锁读取尾部，截断过时删除开头未完整行，再去掉首尾空白和非法字符。
	s.mu.Lock()
	defer s.mu.Unlock()
	text := string(s.lastBytes)
	if _, rest, found := strings.Cut(text, "\n"); s.isTruncated && found {
		text = rest
	}
	return strings.ToValidUTF8(strings.TrimSpace(text), "")
}

// serverRoundTripper 保存 HTTP 传输、附加请求头和最近一次 401、403 的状态行。
type serverRoundTripper struct {
	transport      *http.Transport
	headers        map[string]string
	rejectedStatus atomic.Pointer[string]
}

// RoundTrip 发送 MCP HTTP 请求，并记录认证拒绝状态。
func (t *serverRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	// 1. 有附加请求头时复制请求并设置各个 header。
	if len(t.headers) > 0 {
		request = request.Clone(request.Context())
		for name, value := range t.headers {
			request.Header.Set(name, value)
		}
	}
	// 2. 发送请求，保存 401 或 403 状态行后返回响应。
	response, err := t.transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		status := response.Status
		t.rejectedStatus.Store(&status)
	}
	return response, nil
}
