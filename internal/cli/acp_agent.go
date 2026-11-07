package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	acp "github.com/coder/acp-go-sdk"
)

// acpAgent 服务一个编辑器的 stdio 连接：主循环串行处理编辑器请求，Client#0 列出和新建对话，每个打开的会话由一个 acpSession 用自己的 Client 处理。
type acpAgent struct {
	config    *config.Config
	server    runtimeConnection
	telemetry *telemetry.Telemetry
	input     io.ReadCloser
	output    io.WriteCloser
	version   string
	// client 是 Client#0，负责 session/list 和 session/new 的新建；Run 开始时创建，在 tasks 里运行。
	client runtimeclient.Client
	// isLocalServer 连上的是不是本机 serve？Run 在 Client#0 第一次连上时按 server_id 算出，之后只读；与 serve 断开时整体退出，不再重算。
	isLocalServer bool
	// isWindowsClient 编辑器是不是 Windows 上的？getServerPath 或 session/load 的 cwd 第一次收到盘符开头的路径时置为 true，之后不再改；
	// 只由主循环读写。session/list 回的目录只用和它平台一致的映射换算。
	isWindowsClient bool
	// outgoing 保存等待写入 stdout 的协议消息；Run 等全部会话退出后关闭，写任务持续读取。
	outgoing chan acpMessage
	// received 接收 stdin 读取任务读到的请求。
	received chan acpReceived
	// opened 接收 acpSession 打开对话的结果。
	opened chan acpSessionOpened
	// stopped 接收 acpSession 结束的通知。
	stopped chan acpSessionStopped
	// sessions 是已经打开或正在打开的会话，按对话 ID 登记；只由主循环读写。
	sessions map[defs.ConversationID]*acpSession
	// loadingSessions 是还没回复 session/new 或 session/load 的会话；期间发给它的请求返回 session is loading。
	loadingSessions map[defs.ConversationID]bool
	// isInitialized 编辑器是否已经发过 initialize？之前的其他请求都返回错误。
	isInitialized bool
	// tasks 是 Client#0、它的连接状态读取和各协议会话；Run 结束时先取消并等待它们，再关闭 outgoing。
	tasks sync.WaitGroup
}

// Run 连接 serve，处理编辑器请求直到 stdin 结束、连接断开或 ctx 取消；返回前等全部会话退出，再关闭标准流。
func (a *acpAgent) Run(ctx context.Context) (err error) {
	// 1. 创建 Client#0、消息通道和会话表。
	ctx = telemetry.ContextWithTelemetry(ctx, a.telemetry)
	clientOpt := runtimeclient.NewClientOpt(a.server.URL, a.server.Token)
	clientOpt.Telemetry = a.telemetry
	if a.client, err = runtimeclient.NewClient(clientOpt); err != nil {
		return err
	}
	a.outgoing = make(chan acpMessage, 16)
	a.received = make(chan acpReceived)
	a.opened = make(chan acpSessionOpened)
	a.stopped = make(chan acpSessionStopped)
	a.sessions = make(map[defs.ConversationID]*acpSession)
	a.loadingSessions = make(map[defs.ConversationID]bool)
	agentCtx, stopAgent := context.WithCancelCause(ctx)

	// 2. 启动 Client#0 和读取它连接状态的任务：第一次连上时交出 server_id，断线时取消主循环和正在等待的调用；
	// 第一次连接就失败时关闭标准流并返回错误。
	a.tasks.Go(func() { stopAgent(a.client.Run(agentCtx)) })
	serverConnected := make(chan string, 1)
	a.tasks.Go(func() {
		isConnected := false
		for event := range a.client.Events() {
			changed, ok := event.(*runtimeclient.ConnectionChanged)
			switch {
			case !ok:
			case changed.State == runtimeclient.ConnectionConnected && !isConnected:
				isConnected = true
				serverConnected <- changed.ServerID
			case changed.State == runtimeclient.ConnectionReconnecting:
				// 首次连接失败时附带拨号错误；连上后断开时只返回 ErrConnectionLost。
				cause := runtimeclient.ErrConnectionLost
				if !isConnected && changed.Err != nil {
					cause = fmt.Errorf("%w: %v", runtimeclient.ErrConnectionLost, changed.Err)
				}
				stopAgent(cause)
			}
		}
	})
	select {
	case serverID := <-serverConnected:
		a.isLocalServer = serverID != "" && serverID == a.server.LocalServerID
		telemetry.GetLogger(ctx).Info("connected to runtime server", "openvaala.runtime_server.url", runtimeclient.RedactURL(a.server.URL),
			"openvaala.runtime_server.source", a.server.Source)
	case <-agentCtx.Done():
		err = context.Cause(agentCtx)
		a.tasks.Wait()
		_ = a.output.Close()
		_ = a.input.Close()
		return err
	}

	// 3. 启动三个任务：把 outgoing 写到 stdout、ctx 取消时关闭 stdout、逐行读取 stdin。
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		encoder := json.NewEncoder(a.output)
		var writeErr error
		// 写入失败后继续读取并丢弃后续消息。
		for message := range a.outgoing {
			if writeErr != nil {
				continue
			}
			if writeErr = encoder.Encode(message); writeErr != nil {
				stopAgent(writeErr)
			}
		}
	}()
	outputCloserDone := make(chan struct{})
	go func() {
		defer close(outputCloserDone)
		select {
		case <-ctx.Done():
			_ = a.output.Close()
		case <-writerDone:
		}
	}()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		stopAgent(a.readIncoming(agentCtx))
	}()
	// 4. 返回时依次：取消并等待会话和 Client，关闭 outgoing，等输出写完，关闭标准流，等读取任务退出。
	defer func() {
		stopAgent(err)
		a.tasks.Wait()
		close(a.outgoing)
		<-writerDone
		<-outputCloserDone
		_ = a.output.Close()
		_ = a.input.Close()
		<-readerDone
	}()

	// 5. 串行处理编辑器请求、会话打开结果和会话结束通知，agentCtx 结束时返回结束原因。
	for {
		select {
		case incoming := <-a.received:
			if incoming.err != nil {
				a.send(agentCtx, acpResponse(incoming.message.ID, nil, incoming.err))
				continue
			}
			a.handleRequest(agentCtx, incoming.message)
		case opened := <-a.opened:
			delete(a.loadingSessions, opened.sessionID)
			a.send(agentCtx, acpResponse(opened.requestID, opened.result, opened.err))
			if opened.delivered != nil {
				close(opened.delivered)
			}
		case stopped := <-a.stopped:
			if a.sessions[stopped.session.record.ID] == stopped.session {
				delete(a.sessions, stopped.session.record.ID)
			}
			if stopped.err != nil {
				telemetry.GetLogger(agentCtx).Warn("ACP session stopped", "session_id", stopped.session.record.ID, "error", stopped.err)
			}
		case <-agentCtx.Done():
			return context.Cause(agentCtx)
		}
	}
}

// readIncoming 逐行读取编辑器请求交给主循环，读到 stdin 结束时返回 io.EOF。
func (a *acpAgent) readIncoming(ctx context.Context) error {
	// 1. 读取单行最长 16MB 的消息，解析后交给主循环；取消时返回。
	scanner := bufio.NewScanner(a.input)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		message, err := decodeACPMessage(scanner.Bytes())
		select {
		case a.received <- acpReceived{
			message: message,
			err:     err,
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// 2. 返回读取错误（包括单行超长），输入正常结束时返回 io.EOF。
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

// handleRequest 处理一条编辑器请求：自己处理初始化、新建、加载和列表，会话内的请求交给对应的 acpSession。
func (a *acpAgent) handleRequest(ctx context.Context, request acpMessage) {
	// 1. 忽略 session/cancel 以外的通知；还没 initialize 时其他请求返回错误。
	if len(request.ID) == 0 && request.Method != "session/cancel" {
		return
	}
	var result any
	var err error
	if request.Method != "initialize" && !a.isInitialized {
		err = &acpError{
			-32600,
			"initialize is required",
		}
	} else {
		// 2. 按方法处理：initialize 回复能力；session/new、session/load 检查 cwd 后打开会话，打开结果由主循环回复；
		// session/list 列出对话；prompt、cancel、set_config_option 交给对应会话，会话不存在或还在加载时返回错误。
		switch request.Method {
		case "initialize":
			var params acp.InitializeRequest
			if err = decodeACPParams(request.Params, &params); err == nil {
				a.isInitialized = true
				result = acp.InitializeResponse{
					ProtocolVersion: 1,
					AgentInfo: &acp.Implementation{
						Name:    "openvaala",
						Version: a.version,
					},
					AuthMethods: []acp.AuthMethod{},
					AgentCapabilities: acp.AgentCapabilities{
						LoadSession: true,
						McpCapabilities: acp.McpCapabilities{
							Http: true,
							Sse:  true,
						},
						PromptCapabilities: acp.PromptCapabilities{
							Image:           true,
							EmbeddedContext: true,
						},
						SessionCapabilities: acp.SessionCapabilities{List: &acp.SessionListCapabilities{}},
					},
				}
			}
		case "session/new", "session/load":
			var params acp.LoadSessionRequest
			if err = decodeACPParams(request.Params, &params); err == nil {
				// cwd 没命中 client.path_mappings 时必须是绝对路径。
				_, isMapped := a.config.Client.PathMappings.GetServerPath(params.Cwd)
				if !isMapped && !filepath.IsAbs(params.Cwd) {
					err = &acpError{
						-32602,
						"cwd must be an absolute directory",
					}
				} else if request.Method == "session/load" && params.SessionId == "" {
					err = &acpError{
						-32602,
						"sessionId is required",
					}
				} else {
					err = a.openSession(ctx, request, params)
					if err == nil {
						return
					}
				}
			}
		case "session/list":
			var params acp.ListSessionsRequest
			if err = decodeACPParams(request.Params, &params); err == nil {
				result, err = a.listSessions(ctx, params)
			}
		case "session/prompt", "session/cancel", "session/set_config_option":
			var params struct {
				SessionID defs.ConversationID `json:"sessionId"`
			}
			if err = decodeACPParams(request.Params, &params); err == nil {
				protocolSession := a.sessions[params.SessionID]
				if protocolSession == nil {
					err = &acpError{
						-32602,
						"session not found",
					}
				} else if a.loadingSessions[params.SessionID] {
					err = &acpError{
						-32600,
						"session is loading",
					}
				} else {
					select {
					case protocolSession.requests <- request:
					case <-protocolSession.done:
						a.send(ctx, acpResponse(request.ID, nil, errors.New("session has stopped")))
					case <-ctx.Done():
					}
					return
				}
			}
		default:
			err = &acpError{
				-32601,
				"method not found",
			}
		}
	}
	// 3. 请求带 ID 时回复结果或错误。
	if len(request.ID) != 0 {
		a.send(ctx, acpResponse(request.ID, result, err))
	}
}

// openSession 为 session/new 或 session/load 打开会话：已打开的会话直接转交请求，否则创建 acpSession 并启动。
func (a *acpAgent) openSession(ctx context.Context, request acpMessage, params acp.LoadSessionRequest) error {
	// 1. session/load 命中已打开的会话时把请求交给它；session/new 先用 Client#0 创建对话，cwd 能换成 serve 路径时作为 local 目录。
	record := defs.Conversation{ID: defs.ConversationID(params.SessionId)}
	if request.Method == "session/load" {
		// 加载的对话用已保存的目录；请求的 cwd 只用来判断编辑器平台，并作为本机 stdio MCP 的工作目录。
		if config.IsWindowsPath(params.Cwd) {
			a.isWindowsClient = true
		}
		if existing := a.sessions[record.ID]; existing != nil {
			if a.loadingSessions[record.ID] {
				return &acpError{
					-32600,
					"session is loading",
				}
			}
			select {
			case existing.requests <- request:
			case <-existing.done:
				return errors.New("session has stopped")
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		}
	} else {
		request := &runtimeclient.CreateConversationRequest{AgentName: a.config.Run.AgentName}
		if serverPath, ok := a.getServerPath(params.Cwd); ok {
			request.ExecutorSettings.WorkspaceDirectories = map[string]string{defs.LocalExecutorID: serverPath}
		} else {
			telemetry.GetLogger(ctx).Info("workspace directory is not mapped; the runtime server uses its default directory", "directory", params.Cwd)
		}
		var err error
		if record, err = a.client.CreateConversation(ctx, request); err != nil {
			return err
		}
	}
	// 2. 创建 acpSession，登记为正在加载并启动；Run 返回后通知主循环。
	protocolSession := &acpSession{
		server:           a.server,
		telemetry:        a.telemetry,
		record:           record,
		outgoing:         a.outgoing,
		requests:         make(chan acpMessage),
		opened:           a.opened,
		initialRequest:   request,
		done:             make(chan struct{}),
		clientDirectory:  params.Cwd,
		httpHeaders:      a.config.GetHTTPHeaders(),
		clientMCPServers: getACPClientMCPServers(ctx, params.McpServers),
	}
	a.sessions[record.ID] = protocolSession
	a.loadingSessions[record.ID] = true
	a.tasks.Go(func() {
		err := protocolSession.Run(ctx)
		select {
		case a.stopped <- acpSessionStopped{
			session: protocolSession,
			err:     err,
		}:
		case <-ctx.Done():
		}
	})
	return nil
}

// getServerPath 把编辑器的目录换成 serve 上的目录，换不了时返回 false。
func (a *acpAgent) getServerPath(clientPath string) (string, bool) {
	// 1. 收到盘符开头的路径时记下编辑器在 Windows 上。
	if config.IsWindowsPath(clientPath) {
		a.isWindowsClient = true
	}
	// 2. 命中映射时返回映射后的路径；没命中时本机 serve 直接用原目录，远端 serve 返回 false。
	if serverPath, ok := a.config.Client.PathMappings.GetServerPath(clientPath); ok {
		return serverPath, true
	}
	if !a.isLocalServer {
		return "", false
	}
	return clientPath, true
}

// listSessions 按更新时间倒序列出对话，筛选目录和结果里的目录都在编辑器路径和 serve 路径之间转换。
func (a *acpAgent) listSessions(ctx context.Context, request acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	// 1. 解析分页游标，无效时返回 -32602。
	result := acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}
	var cursor dao.RecordCursor[defs.ConversationID]
	if request.Cursor != nil {
		data, err := base64.RawURLEncoding.DecodeString(*request.Cursor)
		if err != nil || json.Unmarshal(data, &cursor) != nil {
			return result, &acpError{
				-32602,
				"invalid cursor",
			}
		}
	}
	// 2. 按更新时间倒序查询，每页最多 50 条；请求目录能换成 serve 路径时按目录筛选。
	query := &runtimeclient.ListConversationsRequest{
		Order: storage.Order{
			By:        storage.OrderByUpdatedAt,
			Direction: storage.DirectionDesc,
		},
		Cursor: cursor,
		Limit:  50,
	}
	if request.Cwd != nil {
		if serverCwd, ok := a.getServerPath(*request.Cwd); ok && serverCwd != "" {
			query.Directories = []string{serverCwd}
		}
	}
	page, err := a.client.ListConversations(ctx, query)
	if err != nil {
		return result, err
	}
	// 3. 把对话转成会话列表，目录按编辑器平台换回客户端路径，再编码下一页游标。
	for _, entry := range page.Conversations {
		record := entry.Conversation
		directory := record.ExecutorSettings.GetConversationDirectory()
		updated := time.UnixMilli(record.UpdatedAt).UTC().Format(time.RFC3339)
		result.Sessions = append(result.Sessions, acp.SessionInfo{
			SessionId: acp.SessionId(record.ID),
			Cwd:       a.config.Client.PathMappings.GetClientPath(directory, a.isWindowsClient),
			Title:     acp.Ptr(defs.TruncateConversationTitle(record.Title)),
			UpdatedAt: &updated,
		})
	}
	if page.NextCursor.ID != "" {
		data, _ := json.Marshal(page.NextCursor)
		result.NextCursor = acp.Ptr(base64.RawURLEncoding.EncodeToString(data))
	}
	return result, nil
}

// send 把一条协议消息交给输出通道。
func (a *acpAgent) send(ctx context.Context, message acpMessage) {
	// 1. 放进 outgoing，ctx 取消时放弃。
	select {
	case a.outgoing <- message:
	case <-ctx.Done():
	}
}
