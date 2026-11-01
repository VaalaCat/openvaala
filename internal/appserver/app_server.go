// Package appserver 用 Codex app-server 协议服务 Codex 客户端（Codex TUI、Paseo）：每条 Codex 连接经 runtimeclient
// 连共享的 serve（本机的，或命令行、配置指定的远端），把 Codex 请求翻译成 OpenVaala 协议，再把 OpenVaala 事件翻译成 Codex 通知。
package appserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/coder/websocket"
)

// AppServer 用 Codex app-server 协议服务客户端：stdio 模式只服务一条连接，监听模式服务每个接入的连接；
// 每条连接通过 runtimeclient 连共享 serve，把 Codex 请求翻译成 OpenVaala 协议。
type AppServer interface {
	// Run 服务到 ctx 结束；监听失败时提前返回，stdio 模式在输入结束或与 serve 断开时也返回。返回前关闭全部连接并等待它们的任务。
	// stdio 输入结束时返回 nil，与 serve 断开时返回 runtimeclient.ErrConnectionLost；监听模式在 ctx 结束时返回 ctx.Err()。
	Run(ctx context.Context) error
}

// RuntimeServerLocator 为每条 Codex 连接选择 serve，支持并发查询。
type RuntimeServerLocator interface {
	// GetRuntimeServer 返回地址、token、地址来源和本机 server_id；本机 serve 没在运行时启动它。
	GetRuntimeServer(ctx context.Context) (RuntimeServerInfo, error)
}

// appServerImpl 保存已校验的配置；监听模式下每个接入的连接各运行一个 appServerConnection。
type appServerImpl struct {
	opt AppServerOpt
	// connections 是监听模式下各连接的处理任务：升级前登记，Run 在 http.Server.Shutdown 之后取消并等待它们。
	connections sync.WaitGroup
}

// AppServerOpt 指定连哪个 serve、新对话用哪个 Agent，以及 stdio 和监听两种入口用哪一种。
type AppServerOpt struct {
	// RuntimeServer 为每条连接选择要连的 serve。
	RuntimeServer RuntimeServerLocator
	// AgentName 是新建对话使用的 Agent。
	AgentName string
	// Version 写进 initialize 的 userAgent 和 Thread.cliVersion。
	Version string
	// ConfigDirectory 作为 initialize 的 codexHome 返回。
	ConfigDirectory string
	Telemetry       *telemetry.Telemetry
	// PathMappings 把 Codex 客户端给的目录换成 serve 路径（新建对话的 cwd、skills/list、thread/list 的筛选），
	// 再把对话归属的目录换回客户端路径写进 Thread 的 cwd；其他路径原样返回。
	PathMappings config.PathMappings
	// Input、Output 是 stdio 模式的协议流，每行一条 JSON。
	Input  io.ReadCloser
	Output io.WriteCloser
	// Listener 非空时改为监听模式，按 WebSocket 收发。
	Listener net.Listener
}

// NewAppServerOpt 设置 serve 查询入口和新对话使用的 Agent。
func NewAppServerOpt(runtimeServer RuntimeServerLocator, agentName string) *AppServerOpt {
	// 1. 保存 serve 查询入口和 Agent 名称。
	return &AppServerOpt{
		RuntimeServer: runtimeServer,
		AgentName:     agentName,
	}
}

// WithStdio 设置按行收发消息的 stdio 模式。
func (opt *AppServerOpt) WithStdio(input io.ReadCloser, output io.WriteCloser) *AppServerOpt {
	// 1. 设置输入和输出流。
	opt.Input, opt.Output = input, output
	return opt
}

// WithListener 设置接收 WebSocket 连接的监听器。
func (opt *AppServerOpt) WithListener(listener net.Listener) *AppServerOpt {
	// 1. 保存监听器，由 Run 接收连接并在结束时关闭。
	opt.Listener = listener
	return opt
}

// NewAppServer 校验依赖和连接入口，创建 AppServer。
func NewAppServer(opt *AppServerOpt) (AppServer, error) {
	// 1. 检查配置、serve 查询入口和 Agent 名称。
	if opt == nil {
		return nil, errors.New("app server options are required")
	}
	if opt.RuntimeServer == nil {
		return nil, errors.New("app server needs a runtime server locator")
	}
	if opt.AgentName == "" {
		return nil, errors.New("app server needs an agent name")
	}
	// 2. 检查 stdio 和监听模式恰好选择一种。
	isStdio := opt.Input != nil && opt.Output != nil
	if (opt.Input != nil) != (opt.Output != nil) || isStdio == (opt.Listener != nil) {
		return nil, errors.New("app server needs either stdio input and output or a listener")
	}
	// 3. 保存配置并返回服务。
	return &appServerImpl{opt: *opt}, nil
}

// Run 按配置运行 stdio 连接或监听服务。
func (s *appServerImpl) Run(ctx context.Context) error {
	// 1. 设置遥测，监听模式交给监听服务运行。
	ctx = telemetry.ContextWithTelemetry(ctx, s.opt.Telemetry)
	if s.opt.Listener != nil {
		return s.serveListener(ctx)
	}
	// 2. 运行 stdio 连接；输入结束时正常返回，其他错误原样返回。
	err := s.newConnection(newStdioTransport(s.opt.Input, s.opt.Output)).Run(ctx)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// serveListener 接收并运行 WebSocket 连接；ctx 结束或监听失败时关闭 HTTP 服务，等全部连接结束后返回。
func (s *appServerImpl) serveListener(ctx context.Context) error {
	// 1. 创建连接共用的取消上下文和 HTTP 服务。
	connectionsCtx, stopConnections := context.WithCancel(ctx)
	defer stopConnections()
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleHTTPRequest(connectionsCtx, w, r) }),
		ReadHeaderTimeout: 10 * time.Second,
		// HTTP 服务自身的错误写进遥测日志。
		ErrorLog: slog.NewLogLogger(telemetry.GetLogger(ctx).Handler(), slog.LevelError),
	}
	// 2. 启动监听，等待取消或监听失败。
	serveErrs := make(chan error, 1)
	go func() {
		serveErrs <- server.Serve(s.opt.Listener)
	}()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveErrs:
	}
	// 3. 关闭监听器和还没升级的连接，等进行中的升级请求结束。
	shutdownErr := server.Shutdown(context.WithoutCancel(ctx))
	if serveErr == nil {
		if err := <-serveErrs; !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}
	// 4. 取消并等待已升级的连接，返回监听、关闭或取消错误。
	stopConnections()
	s.connections.Wait()
	if err := errors.Join(serveErr, shutdownErr); err != nil {
		return err
	}
	return ctx.Err()
}

// handleHTTPRequest 拒绝浏览器请求，回复健康检查，或升级为 WebSocket。
func (s *appServerImpl) handleHTTPRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// 1. 带 Origin 的请求返回 403。
	if origin := r.Header.Get("Origin"); origin != "" {
		telemetry.GetLogger(ctx).Warn("codex connection rejected: browser origin is not allowed", "origin", origin)
		http.Error(w, "browser origins are not allowed", http.StatusForbidden)
		return
	}
	// 2. 就绪和健康检查返回 200。
	if r.Method == http.MethodGet && (r.URL.Path == "/readyz" || r.URL.Path == "/healthz") {
		w.WriteHeader(http.StatusOK)
		return
	}
	// 3. 其他请求交给 WebSocket 升级处理。
	s.handleUpgrade(ctx, w, r)
}

// handleUpgrade 登记并运行一条 WebSocket 连接，记录连接结束结果。
func (s *appServerImpl) handleUpgrade(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// 1. 登记连接任务，返回时结束登记。
	s.connections.Add(1)
	defer s.connections.Done()
	logger := telemetry.GetLogger(ctx)
	// 2. 升级为 WebSocket，跳过库的同源检查；Origin 已由请求处理函数检查。
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		logger.Warn("codex websocket upgrade failed", "error", err)
		return
	}
	// 3. 运行连接，按结束原因记录日志。
	logger.Info("codex connection opened")
	err = s.newConnection(newWebSocketTransport(conn)).Run(ctx)
	level := slog.LevelWarn
	if isNormalConnectionEnd(err) {
		level = slog.LevelInfo
	}
	logger.Log(ctx, level, "codex connection closed", "reason", err)
}

// newConnection 创建一条 Codex 连接。
func (s *appServerImpl) newConnection(transport codexTransport) *appServerConnection {
	// 1. 将传输方式、serve 查询入口和展示设置交给连接。
	return &appServerConnection{
		transport:            transport,
		runtimeServerLocator: s.opt.RuntimeServer,
		agentName:            s.opt.AgentName,
		version:              s.opt.Version,
		configDirectory:      s.opt.ConfigDirectory,
		telemetry:            s.opt.Telemetry,
		pathMappings:         s.opt.PathMappings,
	}
}

// ── Codex 连接传输 ──

// stdioReadBufferSize 是 stdio 读缓冲的大小；更长的行分段读入，总长受 maxMessageBytes 限制。
const stdioReadBufferSize = 64 << 10

// codexTransport 是一条 Codex 连接的收发方式：stdio 每行一条消息，WebSocket 每个帧一条消息。
// readMessage 仅由连接读任务调用，writeMessage 仅由写任务调用；close 可并发调用并打断读写。
type codexTransport interface {
	// readMessage 返回下一条消息；消息超过 maxMessageBytes 时丢弃它并返回 errMessageTooLarge，之后可以继续读；
	// 其他错误表示连接已经结束，对端正常关闭 stdio 时是 io.EOF。
	readMessage(ctx context.Context) ([]byte, error)
	// writeMessage 写出一条消息；ctx 结束时放弃写出。
	writeMessage(ctx context.Context, payload []byte) error
	// close 关闭连接，打断进行中的读写；可以重复调用。
	close()
	// closeWithError 以 reason 拒绝并关闭连接：WebSocket 发带原因的关闭帧（超长时截断）；stdio 直接关闭，原因由 Run 返回给调用方。
	closeWithError(reason string)
}

// stdioTransport 是 stdio 模式的传输：从 input 逐行读，向 output 逐行写。
type stdioTransport struct {
	input  io.ReadCloser
	output io.WriteCloser
	reader *bufio.Reader
}

// newStdioTransport 创建按行收发的 stdio 传输。
func newStdioTransport(input io.ReadCloser, output io.WriteCloser) *stdioTransport {
	// 1. 保存输入输出流，创建行读取缓冲。
	return &stdioTransport{
		input:  input,
		output: output,
		reader: bufio.NewReaderSize(input, stdioReadBufferSize),
	}
}

// readMessage 读取一行 stdio 消息。
func (t *stdioTransport) readMessage(context.Context) ([]byte, error) {
	// 1. 分段读取一行，超限后继续读完并丢弃；阻塞读取由 close 打断，不随 ctx 取消。
	var line []byte
	isTooLarge := false
	for {
		fragment, err := t.reader.ReadSlice('\n')
		// 行尾换行占用额外一个字节。
		if !isTooLarge && len(line)+len(fragment) > maxMessageBytes+1 {
			isTooLarge, line = true, nil
		}
		if !isTooLarge {
			line = append(line, fragment...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		// 2. 返回读取错误、超限错误或去掉换行的消息；无换行的末行照常返回，之后返回 EOF。
		if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
			return nil, err
		}
		if isTooLarge {
			return nil, errMessageTooLarge
		}
		return bytes.TrimRight(line, "\r\n"), nil
	}
}

// writeMessage 向 stdio 写出一行消息。
func (t *stdioTransport) writeMessage(_ context.Context, payload []byte) error {
	// 1. 添加换行并写入输出流。
	_, err := t.output.Write(append(payload, '\n'))
	return err
}

// close 关闭 stdio 的输入和输出。
func (t *stdioTransport) close() {
	// 1. 依次关闭输入和输出流。
	_ = t.input.Close()
	_ = t.output.Close()
}

// closeWithError 直接关闭 stdio 连接。
func (t *stdioTransport) closeWithError(string) {
	// 1. 关闭输入和输出流。
	t.close()
}

// websocketTransport 是监听模式的传输：Unix socket 或回环 TCP 端口上的 WebSocket，每个帧一条消息。
type websocketTransport struct {
	conn *websocket.Conn
}

// newWebSocketTransport 创建按帧收发的 WebSocket 传输。
func newWebSocketTransport(conn *websocket.Conn) *websocketTransport {
	// 1. 关闭库的读取上限，由 readMessage 检查消息大小。
	conn.SetReadLimit(-1)
	return &websocketTransport{conn: conn}
}

// readMessage 读取一帧 WebSocket 消息。
func (t *websocketTransport) readMessage(ctx context.Context) ([]byte, error) {
	// 1. 打开下一帧，读取至消息上限后的一个字节；ctx 结束时库会关闭连接。
	_, reader, err := t.conn.Reader(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := io.ReadAll(io.LimitReader(reader, maxMessageBytes+1))
	if err != nil {
		return nil, err
	}
	// 2. 返回未超限的消息；超限时丢弃剩余帧内容并返回错误。
	if len(payload) <= maxMessageBytes {
		return payload, nil
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return nil, err
	}
	return nil, errMessageTooLarge
}

// writeMessage 写出一帧文本消息。
func (t *websocketTransport) writeMessage(ctx context.Context, payload []byte) error {
	// 1. 使用调用方上下文写出文本帧。
	return t.conn.Write(ctx, websocket.MessageText, payload)
}

// close 立即关闭 WebSocket 连接。
func (t *websocketTransport) close() {
	// 1. 关闭连接，结束读写。
	_ = t.conn.CloseNow()
}

// closeWithError 发送带原因的错误关闭帧。
func (t *websocketTransport) closeWithError(reason string) {
	// 1. 截取关闭原因并关闭连接。
	_ = t.conn.Close(websocket.StatusInternalError, getWebSocketCloseReason(reason))
}
