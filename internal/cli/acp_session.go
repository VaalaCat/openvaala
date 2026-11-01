package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/utils/syncmap"
	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// acpRelayStreamBacklog 是每条转发流里等本机 MCP server 读取的消息上限；积压写满时结束这条流，serve 随后重连。
const acpRelayStreamBacklog = 256

// acpSession 处理一个 ACP 会话：收编辑器的会话请求，用自己的 Client 连 serve，把对话内容显示给编辑器；Turn 由 serve 里的 Runtime 执行。
type acpSession struct {
	server    runtimeConnection
	telemetry *telemetry.Telemetry
	// record 打开前只有对话 ID，打开后保存 serve 返回的记录和实际 Executor 设置。
	record defs.Conversation
	// requests 接收 acpAgent 转交的会话请求。
	requests chan acpMessage
	// outgoing 是 acpAgent 写 stdout 的输出通道。
	outgoing chan<- acpMessage
	// opened 把打开对话的结果交给 acpAgent 主循环。
	opened chan<- acpSessionOpened
	// initialRequest 是打开这个会话的 session/new 或 session/load 请求。
	initialRequest acpMessage
	// done 在 Run 返回时关闭。
	done chan struct{}
	// client 是本会话自己的 Client，Run 开始时创建，在 tasks 里运行，Run 返回前取消并等待。
	client runtimeclient.Client
	// queuedClientEvents 是等待 Client 调用期间收到、还没处理的事件，主循环在调用返回后按顺序处理。
	queuedClientEvents []runtimeclient.ClientEvent
	// executors 是对话能用的 Executor 和它们的根目录（来自 GetSessionDetails，第一个是默认 Executor）：
	// 用来生成 executor 设置，并把工具参数里的相对路径拼成绝对位置；取不到时为空，不显示 executor 设置，工具通知不带位置。
	executors []conversation.ExecutorDetails
	// models 是 serve 的模型目录：Run 打开对话时取一次，之后每次重新同步时重新取（serve 可能 reload 过配置）。
	models []*defs.ModelDefinition
	// selectedModel 是根 Session 当前选的模型和推理强度。
	selectedModel defs.ModelSelection
	// pendingPromptID、pendingInputID 是编辑器还没结束的 prompt 请求 ID 和它的输入 Item；期间新 prompt 返回 session is busy。
	// 其他客户端或子代理消息发起的 Turn 不记在这里，新 prompt 插队时会中断这些 Turn。
	pendingPromptID json.RawMessage
	pendingInputID  defs.ItemID
	// executorOptionRequestID 是已经改了设置、等对话重新同步后才回复的 executor 设置请求；同步之前又来一个时，
	// 上一个先用当时的选项回复。
	executorOptionRequestID json.RawMessage
	// isOpenDelivered 编辑器是否已经收到 session/new 或 session/load 的回复？之前的同步由 Run 自己取记录和详情，
	// 之后的每次同步都刷新记录和详情并发 config_option_update。
	isOpenDelivered bool
	// isConversationDropped Client 是否已经放弃这个对话（重新同步时 serve 拒绝打开）？之后的 prompt 先重新打开，
	// executor 设置改完再重新打开。
	isConversationDropped bool
	// streamingItems 是本轮生成中的 Item 的显示记录，按 ItemIndex 登记；Turn 开始时清空。
	streamingItems map[int]*acpStreamingItem
	// displayedItems 是本轮已经显示过的 Item 和它的显示记录：重复交付时据此只补发缺少的部分。
	displayedItems map[defs.ItemID]*acpStreamingItem
	// lastDisplayedOrdinal 是已经显示过的已保存 Item 的最大流水号：重新打开对话时 Client 从头补齐历史，只显示这之后的。
	lastDisplayedOrdinal storage.Ordinal
	// toolOutputs 是本轮已经向编辑器显示过的工具调用（按 CallID）和显示之后收到的执行输出：工具进度按进度带的 CallID 找到调用，
	// 把累计的输出整段发出。回放历史和生成中的调用都在显示时登记。
	toolOutputs map[string]string
	// clientDirectory 是编辑器给的工作目录，映射前的客户端路径（acp 和编辑器在同一台机器上）：stdio 的 MCP server 在这个目录下运行；
	// 已经打开的会话再 session/load 时更新。只由会话的事件循环读写。
	clientDirectory string
	// clientMCPServers 是编辑器带来的 MCP server，id 已转换成合法的 server id；转发流按 serve 请求的 server id 在这里找配置，
	// 已经打开的会话再 session/load 时整体替换。只由会话的事件循环读写。
	clientMCPServers []acpClientMCPServer
	// relayStreams 是 serve 请求、还没结束的转发流（按 stream_id）：事件循环登记，流自己的 goroutine 结束时删除。
	relayStreams syncmap.SyncMap[string, *acpRelayStream]
	// tasks 是 Client 的 Run、正在执行的 Client 调用和各条转发流的 goroutine；Run 返回前等待。
	tasks sync.WaitGroup
}

// Run 打开对话、回放历史并回复打开请求，之后串行处理会话请求和 Client 事件，直到断线或 ctx 取消。
func (s *acpSession) Run(ctx context.Context) (err error) {
	// 1. 初始化显示记录；返回时关闭 done，打开回复还没发出时把错误交给 acpAgent 回复，并让等待中的请求失败。
	defer close(s.done)
	s.streamingItems = make(map[int]*acpStreamingItem)
	s.displayedItems = make(map[defs.ItemID]*acpStreamingItem)
	s.toolOutputs = make(map[string]string)
	defer func() {
		if !s.isOpenDelivered {
			select {
			case s.opened <- acpSessionOpened{
				sessionID: s.record.ID,
				requestID: s.initialRequest.ID,
				err:       err,
			}:
			case <-ctx.Done():
			}
		}
		s.failPendingRequests(err)
	}()
	// 2. 创建并运行本会话的 Client；返回时取消 Client 和全部转发流，等全部任务结束。
	clientOpt := runtimeclient.NewClientOpt(s.server.URL, s.server.Token)
	clientOpt.Telemetry = s.telemetry
	if s.client, err = runtimeclient.NewClient(clientOpt); err != nil {
		return err
	}
	clientCtx, stopClient := context.WithCancel(ctx)
	clientDone := make(chan error, 1)
	s.tasks.Go(func() { clientDone <- s.client.Run(clientCtx) })
	defer func() {
		stopClient()
		s.relayStreams.Range(func(_ string, stream *acpRelayStream) bool {
			stream.cancel(nil)
			return true
		})
		s.tasks.Wait()
	}()

	// 3. 打开对话（返回前完整历史和生成中的内容已进入 queuedClientEvents），登记编辑器的 MCP，读取会话详情和模型目录。
	record, err := callClient(ctx, s, func(ctx context.Context) (defs.Conversation, error) {
		return s.client.OpenConversation(ctx, &runtimeclient.OpenConversationRequest{ConversationID: s.record.ID})
	})
	if err != nil {
		return err
	}
	s.record = record
	if len(s.clientMCPServers) > 0 {
		s.registerClientMCPServers(ctx)
	}
	s.refreshSessionDetails(ctx)
	catalog, err := callClient(ctx, s, func(ctx context.Context) (runtime.ModelList, error) {
		return s.client.ListModels(ctx, "")
	})
	if err != nil {
		return err
	}
	s.models = catalog.Models
	// 4. 显示已收到的历史和更新，再让 acpAgent 回复打开请求，等回复进入输出通道。
	if err := s.handleQueuedClientEvents(ctx); err != nil {
		return err
	}
	var result any = acp.LoadSessionResponse{ConfigOptions: getACPConfigOptions(s.models, s.selectedModel, s.executors)}
	if s.initialRequest.Method == "session/new" {
		result = acp.NewSessionResponse{
			SessionId:     acp.SessionId(s.record.ID),
			ConfigOptions: getACPConfigOptions(s.models, s.selectedModel, s.executors),
		}
	}
	delivered := make(chan struct{})
	select {
	case s.opened <- acpSessionOpened{
		sessionID: s.record.ID,
		requestID: s.initialRequest.ID,
		result:    result,
		delivered: delivered,
	}:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	select {
	case <-delivered:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	// 5. 发送标题和可用命令，然后串行处理会话请求和 Client 事件，断线或取消时返回结束原因。
	s.isOpenDelivered = true
	if s.record.Title != "" {
		s.updateSessionTitle(s.record.Title)
	}
	s.refreshAvailableCommands(ctx)
	for {
		if err := s.handleQueuedClientEvents(ctx); err != nil {
			return err
		}
		select {
		case request := <-s.requests:
			s.handleRequest(ctx, request)
		case event, ok := <-s.client.Events():
			if !ok {
				// Client 事件流关闭：会话被取消时返回取消原因，否则返回 Client 的结束错误。
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				return <-clientDone
			}
			if err := s.handleClientEvent(ctx, event); err != nil {
				return err
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// callClient 执行一次 Client 调用，等待期间收到的 Client 事件按顺序存进 queuedClientEvents。
func callClient[T any](ctx context.Context, s *acpSession, call func(context.Context) (T, error)) (T, error) {
	// 1. 在 tasks 里执行调用并保存结果。
	var value T
	callDone := make(chan error, 1)
	s.tasks.Go(func() {
		var err error
		value, err = call(ctx)
		callDone <- err
	})
	// 2. 等调用完成，同时暂存 Client 事件；调用出错且会话已取消时改用 context 里的结束原因，例如与 serve 断开。
	events := s.client.Events()
	for {
		select {
		case err := <-callDone:
			if err != nil && ctx.Err() != nil {
				err = context.Cause(ctx)
			}
			return value, err
		case event, ok := <-events:
			if !ok {
				// Client 已停止：不再读事件，只等调用结束。
				events = nil
				continue
			}
			s.queuedClientEvents = append(s.queuedClientEvents, event)
		}
	}
}

// handleQueuedClientEvents 按到达顺序处理暂存的 Client 事件。
func (s *acpSession) handleQueuedClientEvents(ctx context.Context) error {
	// 1. 逐条取出处理，处理期间新暂存的事件也一起处理；出错（例如连接断开）时直接返回。
	for len(s.queuedClientEvents) > 0 {
		event := s.queuedClientEvents[0]
		s.queuedClientEvents = s.queuedClientEvents[1:]
		if err := s.handleClientEvent(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

// handleRequest 处理会话的 prompt、cancel、重复 load 和 set_config_option 请求。
func (s *acpSession) handleRequest(ctx context.Context, request acpMessage) {
	// 1. 按请求方法分别处理。
	var result any
	var err error
	switch request.Method {
	case "session/prompt":
		// 2. 编辑器已有 prompt 没结束时返回 session busy；解析输入，对话被 Client 放弃过时先重新打开。
		if s.pendingPromptID != nil {
			err = session.ErrSessionBusy
			break
		}
		var params acp.PromptRequest
		if err = decodeACPParams(request.Params, &params); err != nil {
			break
		}
		var content []*defs.Content
		content, err = getACPPromptContent(params.Prompt)
		if err != nil {
			break
		}
		if s.isConversationDropped {
			if err = s.openDroppedConversation(ctx); err != nil {
				break
			}
		}
		var inputID defs.ItemID
		// 3. 以插队模式提交输入（中断正在执行的 Turn 后单独执行），记下等待回复的请求和输入；Turn 结束时再回复。
		inputID, err = callClient(ctx, s, func(ctx context.Context) (defs.ItemID, error) {
			return s.client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
				ConversationID: s.record.ID,
				Mode:           session.SessionInputModeInterrupt,
				Content:        content,
			})
		})
		if err != nil {
			break
		}
		s.pendingPromptID, s.pendingInputID = request.ID, inputID
		return
	case "session/cancel":
		// 4. 中断 Turn，失败时记录警告；cancel 是通知，不回复。
		_, err = callClient(ctx, s, func(ctx context.Context) (defs.TurnID, error) {
			return s.client.InterruptTurn(ctx, s.record.ID)
		})
		if err != nil {
			telemetry.GetLogger(ctx).Warn("ACP cancel failed", "error", err)
		}
		return
	case "session/load":
		var params acp.LoadSessionRequest
		if err = decodeACPParams(request.Params, &params); err != nil {
			break
		}
		// 5. 重复加载：编辑器的 MCP 列表新旧有一边非空时整体替换并重新登记，再发送标题，回复当前设置；聊天记录不重放。
		if len(params.McpServers) > 0 || len(s.clientMCPServers) > 0 {
			s.clientDirectory, s.clientMCPServers = params.Cwd, getACPClientMCPServers(ctx, params.McpServers)
			s.registerClientMCPServers(ctx)
		}
		if s.record.Title != "" {
			s.updateSessionTitle(s.record.Title)
		}
		result = acp.LoadSessionResponse{ConfigOptions: getACPConfigOptions(s.models, s.selectedModel, s.executors)}
	case "session/set_config_option":
		var params acp.SetSessionConfigOptionRequest
		if err = decodeACPParams(request.Params, &params); err != nil {
			break
		}
		if params.ValueId == nil {
			err = &acpError{
				-32602,
				"expected a select config option",
			}
			break
		}
		// 6. 修改默认 Executor：等对话重新同步后再回复；之前还有在等的请求时，先用当前选项回复它。
		if params.ValueId.ConfigId == "executor" {
			if err = s.setDefaultExecutor(ctx, string(params.ValueId.Value)); err != nil {
				break
			}
			s.respond(s.executorOptionRequestID, acp.SetSessionConfigOptionResponse{ConfigOptions: getACPConfigOptions(s.models, s.selectedModel, s.executors)}, nil)
			s.executorOptionRequestID = request.ID
			return
		}
		// 7. 修改模型或推理强度：检查选项，更新根 Session 的模型，发送 config_option_update 并回复。
		selection := s.selectedModel
		switch params.ValueId.ConfigId {
		case "model":
			ref := string(params.ValueId.Value)
			index := slices.IndexFunc(s.models, func(model *defs.ModelDefinition) bool { return model.Ref.String() == ref })
			if index < 0 {
				err = &acpError{
					-32602,
					"unknown model",
				}
				break
			}
			selection = defs.ModelSelection{Ref: s.models[index].Ref}
		case "reasoning_effort":
			selection.ReasoningEffort = string(params.ValueId.Value)
		default:
			err = &acpError{
				-32602,
				"unknown config option",
			}
		}
		if err != nil {
			break
		}
		var record defs.Session
		record, err = callClient(ctx, s, func(ctx context.Context) (defs.Session, error) {
			return s.client.SetSessionModel(ctx, &runtimeclient.SetSessionModelRequest{
				ConversationID: s.record.ID,
				SessionID:      s.record.RootSessionID,
				Model:          selection,
			})
		})
		if err != nil {
			break
		}
		s.selectedModel = record.Model
		options := getACPConfigOptions(s.models, s.selectedModel, s.executors)
		s.update(acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
			SessionUpdate: "config_option_update",
			ConfigOptions: options,
		}})
		result = acp.SetSessionConfigOptionResponse{ConfigOptions: options}
	}
	// 8. 回复已完成的请求或错误。
	s.respond(request.ID, result, err)
}

// handleClientEvent 处理一条 Client 事件：更新编辑器显示、回复 prompt 或处理 MCP 转发。
func (s *acpSession) handleClientEvent(ctx context.Context, event runtimeclient.ClientEvent) error {
	// 1. 按事件类型分别处理，Client 开始重连时返回 ErrConnectionLost；Turn 开始时清空本轮显示记录。
	switch event := event.(type) {
	case *runtimeclient.ConnectionChanged:
		if event.State == runtimeclient.ConnectionReconnecting {
			return runtimeclient.ErrConnectionLost
		}
	case *runtimeclient.ClientMCPConnectRequested:
		s.handleClientMCPConnectRequested(ctx, event)
	case *runtimeclient.ClientMCPMessageReceived:
		s.handleClientMCPMessageReceived(ctx, event)
	case *runtimeclient.ClientMCPDisconnectRequested:
		s.handleClientMCPDisconnectRequested(event)
	case *runtimeclient.ConversationSynced:
		s.handleConversationSynced(ctx, event)
	case *runtimeclient.ConversationTitleChanged:
		s.updateSessionTitle(event.Title)
	case *runtimeclient.ConversationSyncFailed:
		// Client 不再交付这个对话的事件：等待中的请求以 serve 的错误结束，之后的请求先重新打开对话。
		s.isConversationDropped = true
		s.failPendingRequests(event.Err)
	case *runtimeclient.SessionStatusChanged:
		s.handleStatus(event.Info)
	case *runtimeclient.SessionSynced:
		s.handleSessionSynced(event)
	case *runtimeclient.TurnStarted:
		clear(s.streamingItems)
		clear(s.displayedItems)
		clear(s.toolOutputs)
	case *runtimeclient.ItemChanged:
		s.handleItemChanged(event)
	case *runtimeclient.ToolProgress:
		s.handleToolProgress(event)
	case *runtimeclient.ItemsSaved:
		for _, logged := range event.Items {
			s.lastDisplayedOrdinal = logged.Ordinal
			if s.displayedItems[logged.Item.ID] != nil {
				continue
			}
			display := &acpStreamingItem{}
			s.displayedItems[logged.Item.ID] = display
			s.displayItem(logged.Item, display)
		}
	case *runtimeclient.TurnFinished:
		s.handleTurnFinished(event)
	}
	return nil
}

// registerClientMCPServers 把编辑器的 MCP server 列表登记给 serve。
func (s *acpSession) registerClientMCPServers(ctx context.Context) {
	// 1. 提交全部 server id，替换本会话之前的登记；失败时记录警告，会话照常运行。
	serverIDs := make([]string, 0, len(s.clientMCPServers))
	for _, server := range s.clientMCPServers {
		serverIDs = append(serverIDs, server.ID)
	}
	_, err := callClient(ctx, s, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, s.client.SetClientMCPServers(ctx, s.record.ID, serverIDs)
	})
	if err != nil {
		telemetry.GetLogger(ctx).Warn("ACP client MCP servers not registered", "error", err)
	}
}

// handleClientMCPConnectRequested serve 请求连接编辑器的 MCP server 时，登记并启动一条转发流。
func (s *acpSession) handleClientMCPConnectRequested(ctx context.Context, event *runtimeclient.ClientMCPConnectRequested) {
	// 1. 创建并登记转发流，查找传输配置后启动转发任务；找不到配置的错误交给转发任务告知 serve。
	streamCtx, cancel := context.WithCancelCause(ctx)
	stream := &acpRelayStream{
		id:       event.StreamID,
		serverID: event.ServerID,
		incoming: make(chan json.RawMessage, acpRelayStreamBacklog),
		cancel:   cancel,
	}
	s.relayStreams.Store(stream.id, stream)
	transport, err := s.getClientMCPTransport(stream.serverID)
	s.tasks.Go(func() { s.runRelayStream(ctx, streamCtx, stream, transport, err) })
}

// getClientMCPTransport 按 server id 找到编辑器的 MCP server 配置，创建连接它的传输。
func (s *acpSession) getClientMCPTransport(serverID string) (mcp.Transport, error) {
	// 1. 找不到时返回错误；找到时在编辑器目录下、用当前进程环境创建传输。
	index := slices.IndexFunc(s.clientMCPServers, func(server acpClientMCPServer) bool { return server.ID == serverID })
	if index < 0 {
		return nil, fmt.Errorf("the editor has no MCP server %q", serverID)
	}
	return getACPClientMCPTransport(s.clientMCPServers[index].Server, s.clientDirectory, os.Environ()), nil
}

// runRelayStream 在 serve 和本机 MCP server 之间转发一条流，结束后告知 serve。
func (s *acpSession) runRelayStream(ctx, streamCtx context.Context, stream *acpRelayStream, transport mcp.Transport, err error) {
	// 1. 返回时取消并删除这条流；有传输时运行转发，发往 serve 的消息用会话 ctx。
	defer func() {
		stream.cancel(nil)
		s.relayStreams.Delete(stream.id)
	}()
	if err == nil {
		err = mcpclient.RunRelayStream(streamCtx, transport, stream.incoming, func(message json.RawMessage) error {
			return s.client.SendClientMCPMessage(ctx, stream.id, message)
		})
	}
	// 2. 确定结束原因：流被取消时用取消原因，否则用转发错误并把本机服务的错误记进日志。
	if streamCtx.Err() != nil {
		// 流是被取消的（serve 要求断开、积压写满或会话结束）：告知 serve 取消时给的原因。
		err = context.Cause(streamCtx)
	} else if err != nil && !errors.Is(err, runtimeclient.ErrConnectionLost) && !errors.Is(err, runtimeclient.ErrClientStopped) {
		// 本机服务启动或运行错误写入本机日志，与 serve 断开或 Client 停止的错误不记录。
		telemetry.GetLogger(ctx).Warn("ACP client MCP stream failed", "mcp_server_id", stream.serverID, "stream_id", stream.id, "error", err)
	}
	// 3. 告知 serve 流已结束（serve 已关闭的流会丢弃这条通知）；发送失败且会话没结束、也不是断线时记录警告。
	sendErr := s.client.SendClientMCPClosed(ctx, stream.id, err)
	if sendErr != nil && ctx.Err() == nil && !errors.Is(sendErr, runtimeclient.ErrConnectionLost) && !errors.Is(sendErr, runtimeclient.ErrClientStopped) {
		telemetry.GetLogger(ctx).Warn("ACP client MCP stream end not reported", "stream_id", stream.id, "error", sendErr)
	}
}

// handleClientMCPMessageReceived 把 serve 发来的 MCP 消息交给对应的转发流。
func (s *acpSession) handleClientMCPMessageReceived(ctx context.Context, event *runtimeclient.ClientMCPMessageReceived) {
	// 1. 查找转发流，不存在时丢弃消息。
	stream, found := s.relayStreams.Load(event.StreamID)
	if !found {
		return
	}
	// 2. 不阻塞地交付消息；积压写满时记录警告并结束这条流。
	select {
	case stream.incoming <- event.Message:
	default:
		telemetry.GetLogger(ctx).Warn("ACP client MCP stream ended: the local MCP server stopped reading messages", "mcp_server_id", stream.serverID, "stream_id", stream.id)
		stream.cancel(errors.New("client MCP stream backlog is full"))
	}
}

// handleClientMCPDisconnectRequested 取消 serve 指定的转发流，关闭对应的本机 MCP 连接。
func (s *acpSession) handleClientMCPDisconnectRequested(event *runtimeclient.ClientMCPDisconnectRequested) {
	// 1. 找到转发流时取消它。
	if stream, found := s.relayStreams.Load(event.StreamID); found {
		stream.cancel(nil)
	}
}

// handleSessionSynced Session 重新同步后，补发编辑器还没看到的历史和生成中的内容。
func (s *acpSession) handleSessionSynced(event *runtimeclient.SessionSynced) {
	// 1. 已保存的 Item：跳过流水号不大于 lastDisplayedOrdinal 的，按 Item ID 复用或新建显示记录，只补发没显示的部分。
	for _, logged := range event.Items {
		if logged.Ordinal <= s.lastDisplayedOrdinal {
			continue
		}
		s.lastDisplayedOrdinal = logged.Ordinal
		display := s.displayedItems[logged.Item.ID]
		if display == nil {
			display = &acpStreamingItem{}
			s.displayedItems[logged.Item.ID] = display
		}
		s.displayItem(logged.Item, display)
	}
	// 2. 生成中的 Item：按 ItemIndex 复用或新建显示记录，补发没显示的部分。
	for _, streaming := range event.StreamingItems {
		display := s.streamingItems[streaming.ItemIndex]
		if display == nil {
			display = &acpStreamingItem{}
			s.streamingItems[streaming.ItemIndex] = display
		}
		s.displayedItems[streaming.Item.ID] = display
		s.displayItem(streaming.Item, display)
	}
}

// handleConversationSynced 对话重新同步后，刷新编辑器里的用量、标题、设置选项和可用命令。
func (s *acpSession) handleConversationSynced(ctx context.Context, event *runtimeclient.ConversationSynced) {
	// 1. 显示各 Session 的用量；打开回复还没发出时到此为止。
	for _, info := range event.SessionInfos {
		s.handleStatus(info)
	}
	if !s.isOpenDelivered {
		return
	}
	// 2. 更新标题和对话记录，重新读取会话详情和模型目录，发送 config_option_update。
	if s.record.Title != event.Conversation.Title {
		s.updateSessionTitle(event.Conversation.Title)
	}
	s.record = event.Conversation
	s.refreshSessionDetails(ctx)
	s.refreshModels(ctx)
	options := getACPConfigOptions(s.models, s.selectedModel, s.executors)
	s.update(acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
		SessionUpdate: "config_option_update",
		ConfigOptions: options,
	}})
	// 3. 刷新命令列表（默认 Executor 的项目和主目录 Skills），再用新设置回复等待中的 executor 设置请求。
	s.refreshAvailableCommands(ctx)
	if s.executorOptionRequestID != nil {
		s.respond(s.executorOptionRequestID, acp.SetSessionConfigOptionResponse{ConfigOptions: options}, nil)
		s.executorOptionRequestID = nil
	}
}

// setDefaultExecutor 修改对话的默认 Executor；新设置由之后的同步事件刷新到编辑器。
func (s *acpSession) setDefaultExecutor(ctx context.Context, executorID string) error {
	// 1. 保留其他 Executor 设置，提交新的默认 Executor。
	settings := s.record.ExecutorSettings
	settings.DefaultExecutorID = executorID
	_, err := callClient(ctx, s, func(ctx context.Context) (defs.Conversation, error) {
		return s.client.SetConversationExecutorSettings(ctx, s.record.ID, settings)
	})
	if err != nil {
		return err
	}
	// 2. 对话被 Client 放弃过时重新打开。
	if s.isConversationDropped {
		return s.openDroppedConversation(ctx)
	}
	return nil
}

// openDroppedConversation 重新打开被 Client 放弃的对话。
func (s *acpSession) openDroppedConversation(ctx context.Context) error {
	// 1. 请求重新打开对话，失败时保留放弃标记并返回错误。
	_, err := callClient(ctx, s, func(ctx context.Context) (defs.Conversation, error) {
		return s.client.OpenConversation(ctx, &runtimeclient.OpenConversationRequest{ConversationID: s.record.ID})
	})
	if err != nil {
		return err
	}
	// 2. 清除放弃标记；重新打开的对话没有之前的 MCP 登记，编辑器有 MCP server 时重新登记。
	s.isConversationDropped = false
	if len(s.clientMCPServers) > 0 {
		s.registerClientMCPServers(ctx)
	}
	return nil
}

// refreshSessionDetails 重新读取根 Session 的 Executor 列表和所选模型。
func (s *acpSession) refreshSessionDetails(ctx context.Context) {
	// 1. 查询根 Session 详情，失败时记录警告，保留原值。
	details, err := callClient(ctx, s, func(ctx context.Context) (conversation.SessionDetails, error) {
		return s.client.GetSessionDetails(ctx, s.record.ID, s.record.RootSessionID)
	})
	if err != nil {
		telemetry.GetLogger(ctx).Warn("ACP session details skipped", "error", err)
		return
	}
	// 2. 更新 Executor 和模型选择。
	s.executors, s.selectedModel = details.Executors, details.Session.Model
}

// refreshModels 重新读取模型目录。
func (s *acpSession) refreshModels(ctx context.Context) {
	// 1. 查询模型列表，失败时记录警告，保留原列表。
	catalog, err := callClient(ctx, s, func(ctx context.Context) (runtime.ModelList, error) {
		return s.client.ListModels(ctx, "")
	})
	if err != nil {
		telemetry.GetLogger(ctx).Warn("ACP model list refresh skipped", "error", err)
		return
	}
	// 2. 替换模型列表。
	s.models = catalog.Models
}

// refreshAvailableCommands 读取对话可用的 Skills，作为可用命令通知编辑器。
func (s *acpSession) refreshAvailableCommands(ctx context.Context) {
	// 1. 查询对话的 Skills，失败时记录警告并返回。
	skills, err := callClient(ctx, s, func(ctx context.Context) ([]skill.SkillInfo, error) {
		return s.client.GetSkills(ctx, &runtimeclient.GetSkillsRequest{ConversationID: s.record.ID})
	})
	if err != nil {
		telemetry.GetLogger(ctx).Warn("ACP commands skipped", "error", err)
		return
	}
	// 2. 把 Skills 转成可用命令，发送 available_commands_update。
	commands := make([]acp.AvailableCommand, 0, len(skills))
	for _, entry := range skills {
		commands = append(commands, acp.AvailableCommand{
			Name:        entry.Name,
			Description: entry.Description,
		})
	}
	s.update(acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
		SessionUpdate:     "available_commands_update",
		AvailableCommands: commands,
	}})
}

// failPendingRequests 用 err 回复等待中的 prompt 和 executor 设置请求，并清掉它们。
func (s *acpSession) failPendingRequests(err error) {
	// 1. 回复错误，清空等待中的请求 ID 和输入 ID。
	s.respond(s.pendingPromptID, nil, err)
	s.respond(s.executorOptionRequestID, nil, err)
	s.pendingPromptID, s.pendingInputID, s.executorOptionRequestID = nil, "", nil
}

// handleItemChanged 把生成中 Item 的变化显示给编辑器：开始、完成、中断时补发完整 Item，增量直接发出。
func (s *acpSession) handleItemChanged(event *runtimeclient.ItemChanged) {
	// 1. 按 ItemIndex 找到显示记录，按事件类型处理。
	display := s.streamingItems[event.ItemIndex]
	switch event.Type {
	case agent.AgentEventItemStarted, agent.AgentEventItemCompleted, agent.AgentEventItemInterrupted:
		// 2. 开始、完成、中断：没有显示记录时新建，补发完整 Item 还没显示的部分。
		if event.Item == nil {
			return
		}
		if display == nil {
			display = &acpStreamingItem{}
			s.streamingItems[event.ItemIndex] = display
		}
		s.displayedItems[event.Item.ID] = display
		s.displayItem(event.Item, display)
	case agent.AgentEventItemDelta:
		// 3. 增量：发出新增文本；还没显示过推理时按这段有没有摘要定下推理用摘要还是正文，再发出对应的推理文字。
		if display == nil || event.Delta == nil {
			return
		}
		if event.Delta.Text != "" {
			s.update(acp.UpdateAgentMessageText(event.Delta.Text))
			display.textBytes += len(event.Delta.Text)
		}
		if display.reasoningBytes == 0 {
			display.reasoningUsesContent = len(event.Delta.ReasoningSummary) == 0
		}
		thought := strings.Join(event.Delta.ReasoningSummary, "")
		if display.reasoningUsesContent {
			thought = strings.Join(event.Delta.ReasoningContent, "")
		}
		if thought != "" {
			s.update(acp.UpdateAgentThoughtText(thought))
			display.reasoningBytes += len(thought)
		}
	}
}

// handleToolProgress 把工具的执行输出发给编辑器。
func (s *acpSession) handleToolProgress(event *runtimeclient.ToolProgress) {
	// 1. 按 CallID 查找已显示的工具调用，没显示过时忽略这次进度。
	callID := event.Progress.CallID
	output, isDisplayed := s.toolOutputs[callID]
	if !isDisplayed {
		return
	}
	// 2. 累加执行输出，发送 in_progress 状态和累计的全部输出。
	output += executor.GetExecutionOutputText(event.Progress.Content)
	s.toolOutputs[callID] = output
	s.update(acp.UpdateToolCall(acp.ToolCallId(callID), acp.WithUpdateStatus(acp.ToolCallStatusInProgress), acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(output))})))
}

// handleTurnFinished Turn 结束时回复编辑器等待中的 prompt。
func (s *acpSession) handleTurnFinished(event *runtimeclient.TurnFinished) {
	// 1. 没有等待中的 prompt，或这个 Turn 不包含它的输入时直接返回。
	if s.pendingPromptID == nil || !slices.Contains(event.InputItemIDs, s.pendingInputID) {
		return
	}
	// 2. 完成回复 end_turn，中断回复 cancelled，其他结束状态回复错误。
	var result any
	var err error
	switch event.Status {
	case defs.TurnStatusCompleted:
		result = acp.PromptResponse{StopReason: acp.StopReasonEndTurn}
	case defs.TurnStatusInterrupted:
		result = acp.PromptResponse{StopReason: acp.StopReasonCancelled}
	default:
		err = errors.New(event.Error)
		if event.Error == "" {
			err = fmt.Errorf("turn finished with status %q", event.Status)
		}
	}
	// 3. 回复 prompt，清空等待记录。
	s.respond(s.pendingPromptID, result, err)
	s.pendingPromptID, s.pendingInputID = nil, ""
}

// displayItem 按 display 记录补发一个 Item 还没显示的消息、推理、工具调用或工具结果，最后标为已完整显示。
func (s *acpSession) displayItem(item *defs.Item, display *acpStreamingItem) {
	// 1. 按 Item 种类分别处理。
	switch item.Kind {
	case defs.ItemMessage:
		// 2. 消息：只显示用户消息和带 ModelCallID 的模型回复；补发没显示的文本，Item 还没完整显示过时发出图片。
		isUser := item.Message.From != nil && item.Message.From.Kind == defs.IdentityUser
		// Agent 间通知没有 ModelCallID；分叉复制的模型回复仍按 ModelCallID 识别。
		if !isUser && item.ModelCallID == "" {
			return
		}
		text := ""
		for _, content := range item.Message.Content {
			if content.Kind == defs.ContentText {
				text += content.Text.Text
			}
			if content.Kind == defs.ContentImage && !display.completed {
				s.update(getACPMessageUpdate(content, isUser))
			}
		}
		if len(text) > display.textBytes {
			chunk := text[display.textBytes:]
			if isUser {
				s.update(acp.UpdateUserMessageText(chunk))
			} else {
				s.update(acp.UpdateAgentMessageText(chunk))
			}
			display.textBytes = len(text)
		}
	case defs.ItemReasoning:
		// 3. 推理：按之前定下的推理来源补发没显示的文字。
		thought := getACPReasoningText(item.Reasoning.Summary, item.Reasoning.Content)
		if display.reasoningUsesContent {
			thought = strings.Join(item.Reasoning.Content, "")
		}
		if len(thought) > display.reasoningBytes {
			s.update(acp.UpdateAgentThoughtText(thought[display.reasoningBytes:]))
			display.reasoningBytes = len(thought)
		}
	case defs.ItemToolCall:
		// 4. 工具调用：第一次发 tool_call 并登记到 toolOutputs，之后参数有效时更新标题、参数和文件位置；调用被中断时标为 failed。
		call := item.ToolCall
		if call.CallID == "" {
			return
		}
		var input any
		if json.Valid([]byte(call.Arguments)) {
			input = json.RawMessage(call.Arguments)
		}
		locations := getACPToolLocations(call, s.executors)
		if display.callID == "" {
			s.update(acp.StartToolCall(acp.ToolCallId(call.CallID), getACPToolTitle(call.Name), acp.WithStartStatus(acp.ToolCallStatusPending), acp.WithStartKind(getACPToolKind(call.Name)), acp.WithStartRawInput(input), acp.WithStartLocations(locations)))
			display.callID = call.CallID
			if _, isDisplayed := s.toolOutputs[call.CallID]; !isDisplayed {
				s.toolOutputs[call.CallID] = ""
			}
		} else if input != nil {
			s.update(acp.UpdateToolCall(acp.ToolCallId(call.CallID), acp.WithUpdateTitle(getACPToolTitle(call.Name)), acp.WithUpdateRawInput(input), acp.WithUpdateLocations(locations)))
		}
		if call.Status == defs.ToolCallInterrupted {
			s.update(acp.UpdateToolCall(acp.ToolCallId(call.CallID), acp.WithUpdateStatus(acp.ToolCallStatusFailed)))
		}
	case defs.ItemToolResult:
		// 5. 工具结果：只显示一次，整理命令输出、错误和图片后发送结束状态。
		if display.completed {
			return
		}
		result := item.ToolResult
		status := acp.ToolCallStatusCompleted
		text := executor.GetToolResultText(result.Content)
		// 命令结果显示输出；状态 failed 或退出码非零时标为失败，并追加错误原因。
		if command, isCommandResult := tools.ParseCommandResult(result.Content); isCommandResult {
			text = command.Output
			if command.IsFailed() {
				status = acp.ToolCallStatusFailed
			}
			if command.Error != "" {
				text += "\n" + command.Error
			}
		}
		if result.Error != nil {
			status = acp.ToolCallStatusFailed
			text += "\n" + result.Error.Message
		}
		content := []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))}
		for _, image := range result.Images {
			content = append(content, acp.ToolContent(getACPImageBlock(image)))
		}
		// content 包含图片，rawOutput 移除图片数据。
		rawOutput := result.Clone()
		rawOutput.Images = nil
		s.update(acp.UpdateToolCall(acp.ToolCallId(result.CallID), acp.WithUpdateStatus(status), acp.WithUpdateRawOutput(rawOutput), acp.WithUpdateContent(content)))
	}
	// 6. 标为已完整显示。
	display.completed = true
}

// updateSessionTitle 更新会话标题，打开回复发出后通知编辑器。
func (s *acpSession) updateSessionTitle(title string) {
	// 1. 更新记录里的标题（空串表示清除）；打开回复还没发出时到此为止。
	s.record.Title = title
	if !s.isOpenDelivered {
		return
	}
	// 2. 发送 session_info_update。
	s.update(acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
		SessionUpdate: "session_info_update",
		Title:         acp.Ptr(title),
	}})
}

// handleStatus 把根 Session 的上下文用量发给编辑器。
func (s *acpSession) handleStatus(info runtime.SessionRuntimeInfo) {
	// 1. 只处理根 Session；窗口大于 0 且用量来源已知时发送 usage_update。
	if info.Session.ID != s.record.RootSessionID {
		return
	}
	if info.Context.Window > 0 && info.Context.Source != runtime.ContextUsageSourceUnknown {
		s.update(acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{
			SessionUpdate: "usage_update",
			Used:          info.Context.Tokens,
			Size:          info.Context.Window,
		}})
	}
}

// update 给编辑器发一条 session/update 通知。
func (s *acpSession) update(update acp.SessionUpdate) {
	// 1. 编码通知放进 outgoing；outgoing 在全部会话退出后才关闭，这里一直等到放进去。
	params, _ := json.Marshal(acp.SessionNotification{
		SessionId: acp.SessionId(s.record.ID),
		Update:    update,
	})
	s.outgoing <- acpMessage{
		JSONRPC: "2.0",
		Method:  "session/update",
		Params:  params,
	}
}

// respond 回复一条编辑器请求，请求没有 ID 时不回复。
func (s *acpSession) respond(id json.RawMessage, result any, err error) {
	// 1. 没有 ID 时直接返回；否则把结果或错误放进 outgoing，一直等到放进去。
	if len(id) == 0 {
		return
	}
	s.outgoing <- acpResponse(id, result, err)
}
