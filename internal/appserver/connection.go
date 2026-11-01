package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	goruntime "runtime"
	"slices"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// appServerConnection 是一条 Codex 连接：读任务逐条读入消息交给主循环，主循环串行处理请求，写任务按入队顺序写出；
// 控制 Client 处理和 Thread 无关的请求。生命周期与连接相同。
type appServerConnection struct {
	transport codexTransport
	// runtimeServerLocator 在连接开始时决定要连的 serve。
	runtimeServerLocator RuntimeServerLocator
	// agentName 是新建对话和草稿查询用的 Agent。
	agentName string
	// version、configDirectory 写进 initialize 的结果。
	version         string
	configDirectory string
	telemetry       *telemetry.Telemetry
	// pathMappings 把客户端给的目录换成 serve 路径（见 getServerPath），也把对话归属的目录换回客户端路径写进 Thread 的 cwd。
	pathMappings config.PathMappings

	// runtimeServer 是 Run 开始时决定的 serve 地址和 token，本连接的每个 Client 都用它连接。
	runtimeServer RuntimeServerInfo
	// isLocalServer 连上的是不是本机 serve？Run 在控制 Client 第一次连上时按 server_id 算出，之后只读；与 serve 断开时整条连接结束，不再重算。
	isLocalServer bool
	// isWindowsClient 客户端是不是 Windows 上的？getServerPath 或 thread/resume 的 cwd 第一次收到盘符开头的路径时置为 true，之后不再改；
	// 只由主循环读写，开始跟随 Thread 时把当时的值交给它。回给客户端的目录只用和它平台一致的映射换算。
	isWindowsClient bool
	// client 是控制 Client：处理 model/list、skills/list 这类和 Thread 无关的请求；Run 开始时创建，在 tasks 里运行。
	client runtimeclient.Client
	// outgoing 是待写出的响应和通知，由写任务按入队顺序写出；连接结束后入队方不再阻塞，未写出的消息丢弃。
	outgoing chan codexMessage
	// stopConnection 以给定原因结束连接，Run 开始时创建；Thread 打开之后自己结束（它的 Client 不能再用）时也经它结束连接。
	stopConnection context.CancelCauseFunc

	// 以下字段只由主循环读写。

	// isInitialized 客户端是否已经完成 initialize 握手？
	isInitialized bool
	// clientDefaultModel 是客户端用 config/batchWrite 设为默认的模型，零值表示没设过，这时 config/read 返回 Agent 的默认模型。
	// 它和 clientDefaultReasoningEffort 都只在本连接里有效、不保存，连接断开后新对话仍用 Agent 定义的模型。
	clientDefaultModel defs.ModelRef
	// clientDefaultReasoningEffort 是客户端用 config/batchWrite 设为默认的推理强度，空表示没设过，config/read 不带强度。
	clientDefaultReasoningEffort string
	// loadedThreads 是本连接加载的 Thread，键是 Thread id：根 Thread 用 ConversationID，子 Thread 用 SessionID。
	loadedThreads map[string]*connectionThread
	// lastLoadedRootThreadID 是本连接最近加载的根 Thread，不算临时 Thread（它的对话没有 Skill）；skills/list 用它的对话。
	lastLoadedRootThreadID string
	// childThreadConversations 记下子 Thread（子 SessionID）属于哪个对话：开始跟随一个 Thread 时记下首次同步时对话里的全部子 Session，
	// 之后根 Thread 发现新的子 Session 时补上；本连接没见过的 Thread id 由 getHistoryThread 去 serve 按 SessionID 找到子 Session 时也补上。
	// thread/read、thread/resume 和翻页据此找到子 Thread 的对话；登记过的子 Thread 不接受直接输入。
	childThreadConversations map[defs.SessionID]defs.ConversationID
	// subAgentDiscoveries 是根 Thread 交来的、需要自动跟随的子 Session，Run 开始时创建，只由主循环读取。
	subAgentDiscoveries chan *subAgentSessionDiscovered
	// tasks 是控制 Client、它的连接状态读取和各 connectionThread；Run 返回前取消并等待它们。
	tasks sync.WaitGroup
}

// outgoingBufferSize 是待写出消息队列的容量。
const outgoingBufferSize = 16

// slowRequestDuration 是记 codex request slow 日志的处理时长下限。
const slowRequestDuration = 500 * time.Millisecond

// Run 连接 serve 并串行处理 Codex 请求，直到输入结束、连接取消或与 serve 断开；返回前等待全部任务结束并关闭传输。
func (c *appServerConnection) Run(ctx context.Context) (err error) {
	// 1. 查询 serve，创建控制 Client；失败时带错误关闭传输。
	if c.runtimeServer, err = c.runtimeServerLocator.GetRuntimeServer(ctx); err != nil {
		c.transport.closeWithError("cannot start the openvaala runtime server: " + err.Error())
		return err
	}
	clientOpt := runtimeclient.NewClientOpt(c.runtimeServer.URL, c.runtimeServer.Token)
	clientOpt.Telemetry = c.telemetry
	if c.client, err = runtimeclient.NewClient(clientOpt); err != nil {
		c.transport.closeWithError(err.Error())
		return err
	}
	// 2. 创建消息队列、Thread 登记和连接取消上下文。
	c.outgoing = make(chan codexMessage, outgoingBufferSize)
	c.loadedThreads = make(map[string]*connectionThread)
	c.childThreadConversations = make(map[defs.SessionID]defs.ConversationID)
	c.subAgentDiscoveries = make(chan *subAgentSessionDiscovered)
	var connectionCtx context.Context
	connectionCtx, c.stopConnection = context.WithCancelCause(ctx)

	// 3. 运行控制 Client 并读取它的连接状态：首次连上时经 serverConnected 交出 server_id，断线时取消整条连接。
	c.tasks.Go(func() { c.stopConnection(c.client.Run(connectionCtx)) })
	serverConnected := make(chan string, 1)
	c.tasks.Go(func() {
		isConnected := false
		for event := range c.client.Events() {
			changed, ok := event.(*runtimeclient.ConnectionChanged)
			switch {
			case !ok:
			case changed.State == runtimeclient.ConnectionConnected && !isConnected:
				isConnected = true
				serverConnected <- changed.ServerID
			case changed.State == runtimeclient.ConnectionReconnecting:
				// 首次失败附上拨号原因，仍可通过 errors.Is 识别 ErrConnectionLost。
				cause := runtimeclient.ErrConnectionLost
				if !isConnected && changed.Err != nil {
					cause = fmt.Errorf("%w: %v", runtimeclient.ErrConnectionLost, changed.Err)
				}
				c.stopConnection(cause)
			}
		}
	})
	// 4. 等待首次连接，按 server_id 记下是不是本机 serve；连不上时等任务结束，带原因关闭传输。
	select {
	case serverID := <-serverConnected:
		c.isLocalServer = serverID != "" && serverID == c.runtimeServer.LocalServerID
		telemetry.GetLogger(ctx).Info("connected to runtime server", "openvaala.runtime_server.url", runtimeclient.RedactURL(c.runtimeServer.URL),
			"openvaala.runtime_server.source", c.runtimeServer.Source)
	case <-connectionCtx.Done():
		err = context.Cause(connectionCtx)
		c.tasks.Wait()
		c.transport.closeWithError("cannot connect to the openvaala runtime server: " + err.Error())
		return err
	}

	// 5. 启动写出和传输关闭任务；app-server 取消时立即关闭传输。
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		if err := c.writeOutgoing(ctx); err != nil {
			c.stopConnection(err)
			// 写失败后丢弃队列消息，直到 Run 关闭 outgoing。
			for range c.outgoing {
			}
		}
	}()
	transportCloserDone := make(chan struct{})
	go func() {
		defer close(transportCloserDone)
		select {
		case <-ctx.Done():
			c.transport.close()
		case <-writerDone:
		}
	}()
	// 6. 启动读任务，输入结束后关闭 received，结束原因存进 inputErr；
	// 登记收尾：取消连接、等待 Client 和 Thread、写完队列、关闭传输、等待读任务。
	received := make(chan codexReceived)
	var inputErr error
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		inputErr = c.readIncoming(connectionCtx, received)
		close(received)
	}()
	defer func() {
		c.stopConnection(err)
		c.tasks.Wait()
		close(c.outgoing)
		<-writerDone
		<-transportCloserDone
		c.transport.close()
		<-readerDone
	}()

	// 7. 串行处理读到的请求和根 Thread 交来的子 Session，输入结束或连接取消时返回原因。
	for {
		select {
		case incoming, ok := <-received:
			if !ok {
				return inputErr
			}
			c.handleReceived(connectionCtx, incoming)
		case discovered := <-c.subAgentDiscoveries:
			c.followSubAgentThread(connectionCtx, discovered)
		case <-connectionCtx.Done():
			return context.Cause(connectionCtx)
		}
	}
}

// handleReceived 处理一条已读消息，记录请求结果并发送响应。
func (c *appServerConnection) handleReceived(ctx context.Context, incoming codexReceived) {
	// 1. 读取错误直接响应，忽略通知和客户端响应。
	if incoming.err != nil {
		c.send(ctx, newCodexResponse(nil, nil, incoming.err))
		return
	}
	request := incoming.message
	if len(request.ID) == 0 || request.Method == "" {
		return
	}
	// 2. 处理请求；耗时达到 slowRequestDuration 时记警告，失败时记下方法、Thread ID、错误码和错误原文。
	requestStartedAt := time.Now()
	result, err := c.handleRequest(ctx, request)
	if duration := time.Since(requestStartedAt); duration >= slowRequestDuration {
		telemetry.GetLogger(ctx).Warn("codex request slow", "method", request.Method, "duration", duration)
	}
	if err != nil {
		codexErr := getCodexError(err, "")
		telemetry.GetLogger(ctx).Warn("codex request failed", "method", request.Method, "thread_id", getCodexRequestThreadID(request.Params), "code", codexErr.Code, "error", codexErr.Message)
	}
	// 3. 处理函数已经自己发出响应或交给 connectionThread 时直接返回，否则把响应入队。
	if _, isHandedOver := result.(codexResponseHandedOver); isHandedOver {
		return
	}
	c.send(ctx, newCodexResponse(request.ID, result, err))
}

// handleRequest 检查握手状态，按方法处理 Codex 请求。
func (c *appServerConnection) handleRequest(ctx context.Context, request codexMessage) (any, error) {
	// 1. 拒绝重复握手和握手前的其他请求。
	if request.Method == "initialize" && c.isInitialized {
		return nil, &codexError{
			errorCodeInvalidRequest,
			"Already initialized",
		}
	}
	if request.Method != "initialize" && !c.isInitialized {
		return nil, &codexError{
			errorCodeInvalidRequest,
			"Not initialized",
		}
	}
	// 2. 按方法解码参数并分派请求，未知方法返回 -32601。
	switch request.Method {
	case "initialize":
		c.isInitialized = true
		return c.getInitializeResult(), nil
	case "account/read":
		return codexAccountReadResult{RequiresOpenaiAuth: false}, nil
	case "getAuthStatus":
		return codexGetAuthStatusResult{RequiresOpenaiAuth: false}, nil
	case "model/list":
		return c.listModels(ctx)
	case "configRequirements/read":
		return codexConfigRequirementsReadResult{}, nil
	case "config/read":
		var params codexConfigReadParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.readConfig(ctx, params)
	case "config/batchWrite":
		var params codexConfigBatchWriteParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.writeConfig(params)
	case "collaborationMode/list":
		return codexCollaborationModeListResult{Data: []codexCollaborationMode{{
			Name: "Default",
			Mode: "default",
		}}}, nil
	case "skills/list":
		var params codexSkillsListParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.listSkills(ctx, params), nil
	case "mcpServerStatus/list":
		var params codexMcpServerStatusListParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.listMcpServerStatuses(ctx, params)
	case "config/mcpServer/reload":
		return c.reloadMcpServers(ctx)
	case "experimentalFeature/list", "permissionProfile/list", "threadSection/list":
		return codexEmptyPageResult{Data: []struct{}{}}, nil
	case "hooks/list":
		var params codexHooksListParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.listHooks(ctx, params)
	// 插件、App 和可导入历史查询返回空结果。
	case "plugin/list":
		return codexPluginListResult{
			Marketplaces:          []struct{}{},
			MarketplaceLoadErrors: []struct{}{},
			FeaturedPluginIDs:     []string{},
		}, nil
	case "plugin/installed":
		return codexPluginInstalledResult{
			Marketplaces:          []struct{}{},
			MarketplaceLoadErrors: []struct{}{},
		}, nil
	case "app/installed":
		return codexAppInstalledResult{Apps: []struct{}{}}, nil
	case "externalAgentConfig/import/readHistories":
		return codexExternalAgentHistoriesResult{
			Data:       []struct{}{},
			Connectors: []struct{}{},
		}, nil
	case "thread/start":
		var params codexThreadStartParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.startThread(ctx, request.ID, params)
	case "thread/resume":
		var params codexThreadResumeParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.resumeThread(ctx, request.ID, params)
	case "thread/fork":
		var params codexThreadForkParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.forkThread(ctx, request.ID, params)
	case "thread/archive", "thread/unarchive":
		var params codexThreadParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.setThreadArchived(ctx, request.ID, request.Method, params)
	case "thread/delete":
		var params codexThreadParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.deleteThread(ctx, request.ID, params)
	case "thread/unsubscribe":
		var params codexThreadParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.unsubscribeThread(ctx, params)
	case "thread/loaded/list":
		var params codexThreadLoadedListParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return getLoadedThreadsPage(slices.Collect(maps.Keys(c.loadedThreads)), params.Cursor, params.Limit), nil
	case "thread/read":
		var params codexThreadReadParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.readThread(ctx, params)
	case "thread/list":
		var params codexThreadListParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.listThreads(ctx, params)
	case "thread/turns/list":
		var params codexThreadTurnsListParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.listThreadTurns(ctx, params)
	case "thread/items/list":
		var params codexThreadItemsListParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.listThreadItems(ctx, params)
	case "thread/name/set":
		var params codexThreadSetNameParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.setThreadName(ctx, request.ID, params)
	case "thread/compact/start":
		var params codexThreadParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.startThreadCompaction(ctx, params)
	case "turn/start":
		var params codexTurnStartParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.startTurn(ctx, request.ID, params)
	case "turn/steer":
		var params codexTurnSteerParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.steerTurn(ctx, request.ID, params)
	case "turn/interrupt":
		var params codexTurnInterruptParams
		if err := decodeCodexParams(request.Params, &params); err != nil {
			return nil, err
		}
		return c.interruptTurn(ctx, params)
	default:
		return nil, &codexError{
			errorCodeMethodNotFound,
			"method not found: " + request.Method,
		}
	}
}

// getInitializeResult 返回版本、配置目录和平台信息。
func (c *appServerConnection) getInitializeResult() codexInitializeResult {
	// 1. 区分 Windows 与 Unix，填写握手结果。
	platformFamily := "unix"
	if goruntime.GOOS == "windows" {
		platformFamily = "windows"
	}
	return codexInitializeResult{
		UserAgent:      fmt.Sprintf("openvaala/%s (%s; %s)", c.version, goruntime.GOOS, goruntime.GOARCH),
		CodexHome:      c.configDirectory,
		PlatformFamily: platformFamily,
		PlatformOs:     goruntime.GOOS,
	}
}

// listModels 读取并转换 Codex 模型列表。
func (c *appServerConnection) listModels(ctx context.Context) (codexModelListResult, error) {
	// 1. 读取 Agent 模型目录，空目录返回 -32603 和 ErrNoModelConfigured 原文。
	list, err := c.client.ListModels(ctx, c.agentName)
	if err != nil {
		return codexModelListResult{}, err
	}
	if len(list.Models) == 0 {
		return codexModelListResult{}, &codexError{
			errorCodeInternal,
			runtime.ErrNoModelConfigured.Error(),
		}
	}
	// 2. 转换全部模型并标记 Agent 默认模型。
	result := codexModelListResult{Data: make([]codexModel, 0, len(list.Models))}
	for _, model := range list.Models {
		result.Data = append(result.Data, getCodexModel(model, model.Ref == list.AgentDefaultModel))
	}
	return result, nil
}

// readConfig 返回客户端默认模型、Provider 和执行权限设置。
func (c *appServerConnection) readConfig(ctx context.Context, params codexConfigReadParams) (codexConfigReadResult, error) {
	// 1. 读取模型目录，整理 Provider 列表。
	list, err := c.client.ListModels(ctx, c.agentName)
	if err != nil {
		return codexConfigReadResult{}, err
	}
	providers := make(map[string]codexModelProviderConfig)
	for _, model := range list.Models {
		providers[model.Ref.ProviderID] = codexModelProviderConfig{Name: model.Ref.ProviderID}
	}
	// 2. 优先使用本连接设置的默认模型，填写不审批和全访问设置。
	model := list.AgentDefaultModel
	if c.clientDefaultModel != (defs.ModelRef{}) {
		model = c.clientDefaultModel
	}
	config := codexConfig{
		Model:                model.String(),
		ModelReasoningEffort: c.clientDefaultReasoningEffort,
		ModelProvider:        model.ProviderID,
		ModelProviders:       providers,
		ApprovalPolicy:       "never",
		SandboxMode:          "danger-full-access",
	}
	// 3. 请求指定 cwd 时将目录标为 trusted，返回配置。
	if params.Cwd != "" {
		config.Projects = map[string]codexProjectConfig{params.Cwd: {TrustLevel: "trusted"}}
	}
	return codexConfigReadResult{Config: config}, nil
}

// writeConfig 更新本连接显示的默认模型和推理强度。
func (c *appServerConnection) writeConfig(params codexConfigBatchWriteParams) (codexConfigWriteResult, error) {
	// 1. 逐项校验 model 和 model_reasoning_effort；只接受字符串或 null，非法模型及其他配置返回 -32602。
	model, effort := c.clientDefaultModel, c.clientDefaultReasoningEffort
	for _, edit := range params.Edits {
		if edit.KeyPath != "model" && edit.KeyPath != "model_reasoning_effort" {
			return codexConfigWriteResult{}, &codexError{
				errorCodeInvalidParams,
				"OpenVaala does not save Codex settings: " + edit.KeyPath,
			}
		}
		value, isString := edit.Value.(string)
		if edit.Value != nil && !isString {
			return codexConfigWriteResult{}, &codexError{
				errorCodeInvalidParams,
				edit.KeyPath + " must be a string or null",
			}
		}
		if edit.KeyPath == "model_reasoning_effort" {
			effort = value
			continue
		}
		model = defs.ModelRef{}
		if value != "" {
			ref, err := defs.ParseModelRef(value)
			if err != nil {
				return codexConfigWriteResult{}, &codexError{
					errorCodeInvalidParams,
					err.Error(),
				}
			}
			model = ref
		}
	}
	// 2. 整批校验通过后更新连接默认值；null 清空，空批次同样成功，返回配置目录。
	c.clientDefaultModel, c.clientDefaultReasoningEffort = model, effort
	return codexConfigWriteResult{
		Status:   "ok",
		FilePath: c.configDirectory,
	}, nil
}

// getServerPath 把客户端目录换成 serve 路径，换不了时返回 false。
func (c *appServerConnection) getServerPath(clientPath string) (string, bool) {
	// 1. 收到盘符路径时记下 Windows 客户端，先按路径映射换算。
	if config.IsWindowsPath(clientPath) {
		c.isWindowsClient = true
	}
	if serverPath, ok := c.pathMappings.GetServerPath(clientPath); ok {
		return serverPath, true
	}
	// 2. 没有命中映射时，本机 serve 用原路径，远端 serve 返回 false。
	if !c.isLocalServer {
		return "", false
	}
	return clientPath, true
}

// getLocalWorkspaceDirectory 把客户端 cwd 换成 local Executor 的工作目录；cwd 为空或换不了时返回 false。
func (c *appServerConnection) getLocalWorkspaceDirectory(ctx context.Context, clientCwd string) (string, bool) {
	// 1. 空目录返回 false，其他目录换成 serve 路径，换不了时记日志。
	if clientCwd == "" {
		return "", false
	}
	serverPath, ok := c.getServerPath(clientCwd)
	if !ok {
		telemetry.GetLogger(ctx).Info("workspace directory is not mapped; it is not passed to the runtime server", "directory", clientCwd)
	}
	return serverPath, ok
}

// getRequestedModel 按客户端参数选出模型目录里的模型和它支持的推理强度；选不出时返回零值。
func (c *appServerConnection) getRequestedModel(ctx context.Context, model, effort string) (defs.ModelSelection, error) {
	// 1. 解析模型引用并查询模型目录；格式错误返回零值。
	ref, err := defs.ParseModelRef(model)
	if err != nil {
		return defs.ModelSelection{}, nil
	}
	list, err := c.client.ListModels(ctx, "")
	if err != nil {
		return defs.ModelSelection{}, err
	}
	// 2. 模型不在目录时返回零值；在目录时保留模型，强度只在模型支持时保留，否则留空用默认强度。
	index := slices.IndexFunc(list.Models, func(definition *defs.ModelDefinition) bool { return definition.Ref == ref })
	if index < 0 {
		return defs.ModelSelection{}, nil
	}
	selection := defs.ModelSelection{Ref: ref}
	if list.Models[index].IsReasoningEffortSupported(effort) {
		selection.ReasoningEffort = effort
	}
	return selection, nil
}

// listSkills 读取最近根 Thread 或草稿目录的 Skill 列表。
func (c *appServerConnection) listSkills(ctx context.Context, params codexSkillsListParams) codexSkillsListResult {
	// 1. 选取首个 cwd；优先读取最近的非临时根 Thread，否则转换为草稿 local 目录，未映射时使用服务端默认目录。
	request := &runtimeclient.GetSkillsRequest{AgentName: c.agentName}
	entry := codexSkillsListEntry{Errors: []struct{}{}}
	if len(params.Cwds) > 0 {
		entry.Cwd = params.Cwds[0]
	}
	if thread := c.loadedThreads[c.lastLoadedRootThreadID]; thread != nil {
		request = &runtimeclient.GetSkillsRequest{ConversationID: thread.conversationID}
	} else if serverPath, ok := c.getLocalWorkspaceDirectory(ctx, entry.Cwd); ok {
		request.ExecutorSettings.WorkspaceDirectories = map[string]string{defs.LocalExecutorID: serverPath}
	}
	// 2. 查询 Skill，失败时记录警告并返回空列表；下次请求重新查询。
	skills, err := c.client.GetSkills(ctx, request)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("list skills failed", "error", err)
	}
	// 3. 转换 Skill 元数据，cwd 按请求返回。
	entry.Skills = make([]codexSkillMetadata, 0, len(skills))
	for _, info := range skills {
		entry.Skills = append(entry.Skills, codexSkillMetadata{
			Name:        info.Name,
			Description: info.Description,
			Path:        info.Path,
			Scope:       "user",
			Enabled:     true,
		})
	}
	return codexSkillsListResult{Data: []codexSkillsListEntry{entry}}
}

// listHooks 按请求目录返回服务端的 hook 列表。
func (c *appServerConnection) listHooks(ctx context.Context, params codexHooksListParams) (codexHooksListResult, error) {
	// 1. 读取全部用户级 hook。
	infos, err := c.client.GetHooks(ctx)
	if err != nil {
		return codexHooksListResult{}, err
	}
	// 2. 没有请求目录时读取默认 Executor 目录并转换为客户端路径。
	cwds := params.Cwds
	if len(cwds) == 0 {
		executors, err := c.client.ListExecutors(ctx)
		if err != nil {
			return codexHooksListResult{}, err
		}
		cwds = []string{c.pathMappings.GetClientPath(getDefaultExecutorDetails(executors).WorkspaceDirectory, c.isWindowsClient)}
	}
	// 3. 转换 hook 元数据，为每个目录返回同一份列表。
	hooks := make([]codexHookMetadata, 0, len(infos))
	for _, info := range infos {
		hooks = append(hooks, getCodexHookMetadata(info))
	}
	result := codexHooksListResult{Data: make([]codexHooksListEntry, 0, len(cwds))}
	for _, cwd := range cwds {
		result.Data = append(result.Data, codexHooksListEntry{
			Cwd:      cwd,
			Hooks:    hooks,
			Warnings: []string{},
			Errors:   []struct{}{},
		})
	}
	return result, nil
}

// listMcpServerStatuses 读取已跟随 Thread 所属对话的 MCP 状态。
func (c *appServerConnection) listMcpServerStatuses(ctx context.Context, params codexMcpServerStatusListParams) (any, error) {
	// 1. 未跟随指定 Thread 时返回空页，否则读取对话 MCP 连接。
	thread := c.loadedThreads[params.ThreadID]
	if thread == nil {
		return codexEmptyPageResult{Data: []struct{}{}}, nil
	}
	servers, err := c.client.ListMCPServers(ctx, thread.conversationID)
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	// 2. 读取 Session 的默认 Executor，转换连接状态并给重名连接添加 Executor 后缀。
	details, err := c.client.GetSessionDetails(ctx, thread.conversationID, thread.sessionID)
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	return codexMcpServerStatusListResult{Data: getCodexMcpServerStatuses(servers, getDefaultExecutorDetails(details.Executors).ID)}, nil
}

// reloadMcpServers 请求 serve 重新加载配置和 MCP 设置。
func (c *appServerConnection) reloadMcpServers(ctx context.Context) (any, error) {
	// 1. 让 serve 重新加载整份配置，成功时返回空对象；正在忙的对话这次不换新配置，空闲后要再请求一次。
	if _, err := c.client.ReloadConfig(ctx); err != nil {
		return nil, err
	}
	return codexEmptyObjectResult{}, nil
}

// startThread 创建对话并跟随根 Thread，响应后发送开始通知。
func (c *appServerConnection) startThread(ctx context.Context, requestID json.RawMessage, params codexThreadStartParams) (any, error) {
	// 1. 拒绝动态工具，填写 Agent 和临时对话标记。
	if len(params.DynamicTools) > 0 {
		return nil, &codexError{
			errorCodeInvalidParams,
			"dynamicTools are not supported",
		}
	}
	request := &runtimeclient.CreateConversationRequest{
		AgentName: c.agentName,
		Ephemeral: params.Ephemeral,
	}
	// 2. 非临时对话使用映射后的 local 工作目录，选择目录内的模型和强度。
	if !params.Ephemeral {
		if serverPath, ok := c.getLocalWorkspaceDirectory(ctx, params.Cwd); ok {
			request.ExecutorSettings.WorkspaceDirectories = map[string]string{defs.LocalExecutorID: serverPath}
		}
	}
	var err error
	if request.Model, err = c.getRequestedModel(ctx, params.Model, params.Config.ModelReasoningEffort); err != nil {
		return nil, err
	}
	// 3. 创建对话，开始跟随并等待首次同步。
	record, err := c.client.CreateConversation(ctx, request)
	if err != nil {
		return nil, err
	}
	thread := c.newThread(ctx, string(record.ID), record.ID, record.RootSessionID, "", false, params.Ephemeral)
	opened, err := c.followThread(ctx, thread)
	if err != nil {
		return nil, getCodexError(err, thread.threadID)
	}
	// 4. 临时 Thread 的 cwd 和来源按请求返回；其他根 Thread 记为最近加载。
	if params.Ephemeral {
		opened.thread.Cwd, opened.thread.ThreadSource = params.Cwd, params.ThreadSource
	} else {
		c.lastLoadedRootThreadID = thread.threadID
	}
	// 5. 先发送打开响应，再发送 thread/started，释放 Thread 暂存通知。
	c.send(ctx, newCodexResponse(requestID, newCodexThreadOpenResult(opened.thread), nil))
	c.sendNotification(ctx, "thread/started", codexThreadStartedNotification{Thread: opened.thread})
	close(opened.delivered)
	return codexResponseHandedOver{}, nil
}

// resumeThread 恢复跟随 Thread，响应后继续交付实时内容。
func (c *appServerConnection) resumeThread(ctx context.Context, requestID json.RawMessage, params codexThreadResumeParams) (any, error) {
	// 1. 从 cwd 登记 Windows 客户端。
	if config.IsWindowsPath(params.Cwd) {
		c.isWindowsClient = true
	}
	// 2. 已跟随时按需读取完整历史，交给 Thread 使用现有状态响应并重发生成中内容。
	if thread := c.loadedThreads[params.ThreadID]; thread != nil {
		turns := []codexTurn{}
		if !params.ExcludeTurns {
			sessions, err := c.readSessions(ctx, thread.conversationID)
			if err == nil {
				turns, err = c.readThreadTurns(ctx, thread.conversationID, thread.sessionID, sessions, nil)
			}
			if err != nil {
				return nil, getCodexError(err, thread.threadID)
			}
		}
		if err := c.sendThreadRequest(ctx, thread, &threadResumeRequested{
			requestID: requestID,
			turns:     turns,
		}); err != nil {
			return nil, err
		}
		return codexResponseHandedOver{}, nil
	}
	// 3. 未跟随时定位对话和 Session，开始跟随；未排除历史时读到跟随起点。
	history, err := c.getHistoryThread(ctx, params.ThreadID)
	if err != nil {
		return nil, err
	}
	thread := c.newThread(ctx, history.threadID, history.conversation.ID, history.sessionID, "", params.ExcludeTurns, false)
	opened, err := c.followThreadWithTurns(ctx, thread)
	if err != nil {
		return nil, err
	}
	// 4. 先发送响应，再释放 Thread 暂存的通知。
	c.send(ctx, newCodexResponse(requestID, newCodexThreadOpenResult(opened.thread), nil))
	close(opened.delivered)
	return codexResponseHandedOver{}, nil
}

// forkThread 复制根 Thread 对话并跟随新 Thread，响应后发送开始通知。
func (c *appServerConnection) forkThread(ctx context.Context, requestID json.RawMessage, params codexThreadForkParams) (any, error) {
	// 1. 拒绝临时分叉，读取来源根对话。
	if params.Ephemeral {
		return nil, &codexError{
			errorCodeInvalidParams,
			"ephemeral thread/fork is not supported",
		}
	}
	source, err := c.getRootThreadConversation(ctx, params.ThreadID, "thread/fork")
	if err != nil {
		return nil, err
	}
	// 2. 填写截取点和来源的 Executor 设置；请求带 cwd 时复制来源的目录设置，再替换其中的 local 目录。
	request := &runtimeclient.ForkConversationRequest{
		ConversationID:   source.ID,
		BeforeTurnID:     defs.TurnID(params.BeforeTurnID),
		LastTurnID:       defs.TurnID(params.LastTurnID),
		ExecutorSettings: source.ExecutorSettings,
	}
	if serverPath, ok := c.getLocalWorkspaceDirectory(ctx, params.Cwd); ok {
		directories := make(map[string]string, len(source.ExecutorSettings.WorkspaceDirectories)+1)
		maps.Copy(directories, source.ExecutorSettings.WorkspaceDirectories)
		directories[defs.LocalExecutorID] = serverPath
		request.ExecutorSettings.WorkspaceDirectories = directories
	}
	// 3. 选择请求的模型，复制对话；来源是临时对话时 serve 返回对话不存在，分叉错误转成 Codex 错误。
	if request.Model, err = c.getRequestedModel(ctx, params.Model, params.Config.ModelReasoningEffort); err != nil {
		return nil, err
	}
	record, err := c.client.ForkConversation(ctx, request)
	if err != nil {
		return nil, getThreadForkError(err, params)
	}
	// 4. 跟随新根 Thread，按需读到跟随起点的历史。
	thread := c.newThread(ctx, string(record.ID), record.ID, record.RootSessionID, "", params.ExcludeTurns, false)
	opened, err := c.followThreadWithTurns(ctx, thread)
	if err != nil {
		return nil, err
	}
	// 5. 先发送响应，再发送不带 turns 的开始通知，释放暂存通知。
	c.send(ctx, newCodexResponse(requestID, newCodexThreadOpenResult(opened.thread), nil))
	started := opened.thread
	started.Turns = []codexTurn{}
	c.sendNotification(ctx, "thread/started", codexThreadStartedNotification{Thread: started})
	close(opened.delivered)
	return codexResponseHandedOver{}, nil
}

// followThreadWithTurns 跟随 Thread，客户端不自己翻页时读出跟随起点及之前的历史。
func (c *appServerConnection) followThreadWithTurns(ctx context.Context, thread *connectionThread) (connectionThreadOpened, error) {
	// 1. 启动跟随并等待首次同步。
	opened, err := c.followThread(ctx, thread)
	if err != nil {
		return opened, getCodexError(err, thread.threadID)
	}
	// 2. 非分页模式读取跟随起点及之前的 Turn 和 Item，失败时停止跟随。
	if !thread.isHistoryPaged {
		sessions, err := c.readSessions(ctx, thread.conversationID)
		if err == nil {
			opened.thread.Turns, err = c.readThreadTurns(ctx, thread.conversationID, thread.sessionID, sessions, &opened.followStart)
		}
		if err != nil {
			c.unloadThreads(ctx, thread)
			return opened, getCodexError(err, thread.threadID)
		}
	}
	// 3. 根 Thread 记为最近加载，返回打开结果。
	if thread.isRootThread() {
		c.lastLoadedRootThreadID = thread.threadID
	}
	return opened, nil
}

// newThread 创建跟随指定 Session 的 Thread。
func (c *appServerConnection) newThread(ctx context.Context, threadID string, conversationID defs.ConversationID, sessionID defs.SessionID, startedStatusType string, isHistoryPaged, isEphemeral bool) *connectionThread {
	// 1. 保存 serve、路径映射、消息通道、首次状态和历史模式。
	return newConnectionThread(&connectionThreadOpt{
		RuntimeServer:       c.runtimeServer,
		Telemetry:           c.telemetry,
		Version:             c.version,
		PathMappings:        c.pathMappings,
		IsWindowsClient:     c.isWindowsClient,
		Outgoing:            c.outgoing,
		ConnectionDone:      ctx.Done(),
		SubAgentDiscoveries: c.subAgentDiscoveries,
		ThreadID:            threadID,
		ConversationID:      conversationID,
		SessionID:           sessionID,
		StartedStatusType:   startedStatusType,
		IsHistoryPaged:      isHistoryPaged,
		IsEphemeral:         isEphemeral,
	})
}

// followThread 在连接任务里运行 Thread，等到首次同步后登记它；调用方关闭返回的 delivered 后，Thread 才开始发通知。
func (c *appServerConnection) followThread(ctx context.Context, thread *connectionThread) (connectionThreadOpened, error) {
	// 1. 在连接任务里运行 Thread；Thread 出错返回且连接没有取消时，结束整条连接。
	threadCtx, stopThread := context.WithCancel(ctx)
	c.tasks.Go(func() {
		defer stopThread()
		if err := thread.Run(threadCtx); err != nil && threadCtx.Err() == nil {
			c.stopConnection(err)
		}
	})
	// 2. 等待打开结果，成功时登记 Thread 和各子 Session 所属的对话。
	select {
	case opened := <-thread.opened:
		if opened.err != nil {
			return opened, opened.err
		}
		c.loadedThreads[thread.threadID] = thread
		for _, sessionID := range opened.subAgentSessionIDs {
			c.childThreadConversations[sessionID] = thread.conversationID
		}
		return opened, nil
	case <-ctx.Done():
		return connectionThreadOpened{}, context.Cause(ctx)
	}
}

// followSubAgentThread 登记新发现的子 Thread，发送开始通知并跟随。
func (c *appServerConnection) followSubAgentThread(ctx context.Context, discovered *subAgentSessionDiscovered) {
	// 1. 登记所属对话，跳过已跟随的 Thread。
	c.childThreadConversations[defs.SessionID(discovered.thread.ID)] = discovered.conversationID
	if c.loadedThreads[discovered.thread.ID] != nil {
		return
	}
	// 2. 先发送开始通知，再启动跟随；失败时记警告，历史仍能读取，子 Session 再次开始执行时重新跟随。
	c.sendNotification(ctx, "thread/started", codexThreadStartedNotification{Thread: discovered.thread})
	thread := c.newThread(ctx, discovered.thread.ID, discovered.conversationID, defs.SessionID(discovered.thread.ID), discovered.thread.Status.Type, false, false)
	opened, err := c.followThread(ctx, thread)
	if err != nil {
		if ctx.Err() == nil {
			telemetry.GetLogger(ctx).Warn("follow sub-agent thread failed", "thread_id", thread.threadID, "error", err)
		}
		return
	}
	// 3. 首次同步成功后释放 Thread 通知。
	close(opened.delivered)
}

// startTurn 转换输入和模型设置，交给根 Thread 启动 Turn。
func (c *appServerConnection) startTurn(ctx context.Context, requestID json.RawMessage, params codexTurnStartParams) (any, error) {
	// 1. 检查根 Thread 已加载，转换输入和输出格式要求。
	thread, err := c.getDirectInputThread(params.ThreadID)
	if err != nil {
		return nil, err
	}
	content, err := getCodexInputContent(params.Input, params.OutputSchema)
	if err != nil {
		return nil, err
	}
	// 2. 使用请求模型，协作模式指定了模型时改用协作模式里的设置。
	model, effort := params.Model, params.Effort
	if mode := params.CollaborationMode; mode != nil && mode.Settings.Model != "" {
		model, effort = &mode.Settings.Model, mode.Settings.ReasoningEffort
	}
	// 3. 将内容、模型和客户端消息 ID 交给 Thread，由它在取得 Turn ID 后响应。
	request := &turnStartRequested{
		requestID:           requestID,
		content:             content,
		model:               model,
		effort:              effort,
		clientUserMessageID: params.ClientUserMessageID,
	}
	if err := c.sendThreadRequest(ctx, thread, request); err != nil {
		return nil, err
	}
	return codexResponseHandedOver{}, nil
}

// steerTurn 转换插话输入，交给根 Thread 校验目标 Turn 并提交。
func (c *appServerConnection) steerTurn(ctx context.Context, requestID json.RawMessage, params codexTurnSteerParams) (any, error) {
	// 1. 检查根 Thread 已加载，转换输入内容。
	thread, err := c.getDirectInputThread(params.ThreadID)
	if err != nil {
		return nil, err
	}
	content, err := getCodexInputContent(params.Input, nil)
	if err != nil {
		return nil, err
	}
	// 2. 将目标 Turn、内容和客户端消息 ID 交给 Thread，由它响应。
	request := &turnSteerRequested{
		requestID:           requestID,
		expectedTurnID:      defs.TurnID(params.ExpectedTurnID),
		content:             content,
		clientUserMessageID: params.ClientUserMessageID,
	}
	if err := c.sendThreadRequest(ctx, thread, request); err != nil {
		return nil, err
	}
	return codexResponseHandedOver{}, nil
}

// interruptTurn 请求中断已加载根 Thread 的 Turn。
func (c *appServerConnection) interruptTurn(ctx context.Context, params codexTurnInterruptParams) (any, error) {
	// 1. 取得接受直接输入的根 Thread，中断根 Session，成功后返回空对象；完成通知由 Thread 发出。
	thread, err := c.getDirectInputThread(params.ThreadID)
	if err != nil {
		return nil, err
	}
	if _, err := c.client.InterruptTurn(ctx, thread.conversationID); err != nil {
		return nil, getCodexError(err, thread.threadID)
	}
	return codexEmptyObjectResult{}, nil
}

// startThreadCompaction 请求已加载根 Thread 运行上下文压缩。
func (c *appServerConnection) startThreadCompaction(ctx context.Context, params codexThreadParams) (any, error) {
	// 1. 检查根 Thread，提交压缩请求并返回空对象；Session 忙时返回 -32600，后续事件由 Thread 发出。
	thread, err := c.getDirectInputThread(params.ThreadID)
	if err != nil {
		return nil, err
	}
	if _, err := c.client.CompactContext(ctx, thread.conversationID); err != nil {
		return nil, getCodexError(err, thread.threadID)
	}
	return codexEmptyObjectResult{}, nil
}

// setThreadName 保存 Thread 标题，先响应再通知。
func (c *appServerConnection) setThreadName(ctx context.Context, requestID json.RawMessage, params codexThreadSetNameParams) (any, error) {
	// 1. 已跟随的根 Thread 自己保存标题和响应，标题事件随后通知。
	if thread := c.loadedThreads[params.ThreadID]; thread != nil && thread.isRootThread() {
		if err := c.sendThreadRequest(ctx, thread, &threadNameSetRequested{
			requestID: requestID,
			title:     params.Name,
		}); err != nil {
			return nil, err
		}
		return codexResponseHandedOver{}, nil
	}
	// 2. 其他 Thread 使用控制 Client 保存对话标题。
	if err := c.client.SetConversationTitle(ctx, defs.ConversationID(params.ThreadID), params.Name); err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	// 3. 发送响应，再通知本连接截断后的标题。
	c.send(ctx, newCodexResponse(requestID, codexEmptyObjectResult{}, nil))
	c.sendNotification(ctx, "thread/name/updated", codexThreadNameUpdatedNotification{
		ThreadID:   params.ThreadID,
		ThreadName: defs.TruncateConversationTitle(params.Name),
	})
	return codexResponseHandedOver{}, nil
}

// setThreadArchived 保存根 Thread 的归档状态，响应后发送归档通知。
func (c *appServerConnection) setThreadArchived(ctx context.Context, requestID json.RawMessage, method string, params codexThreadParams) (any, error) {
	// 1. 检查是根对话，保存归档或取消归档状态。
	if _, err := c.getRootThreadConversation(ctx, params.ThreadID, method); err != nil {
		return nil, err
	}
	isArchived := method == "thread/archive"
	record, err := c.client.SetConversationArchived(ctx, &runtimeclient.SetConversationArchivedRequest{
		ConversationID: defs.ConversationID(params.ThreadID),
		Archived:       isArchived,
	})
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	// 2. 归档请求回复空对象，再发送归档通知。
	if isArchived {
		c.send(ctx, newCodexResponse(requestID, codexEmptyObjectResult{}, nil))
		c.sendNotification(ctx, "thread/archived", codexThreadIDNotification{ThreadID: params.ThreadID})
		return codexResponseHandedOver{}, nil
	}
	// 3. 取消归档时读取 Session，响应不带 Turn 的 Thread，再发送取消归档通知。
	sessions, err := c.readSessions(ctx, record.ID)
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	thread := historyThread{
		threadID:     params.ThreadID,
		conversation: record,
		sessionID:    record.RootSessionID,
	}
	result := codexThreadUnarchiveResult{Thread: newCodexHistoryThread(thread, sessions, c.pathMappings, c.isWindowsClient, c.getThreadStatus(ctx, params.ThreadID), c.version)}
	c.send(ctx, newCodexResponse(requestID, result, nil))
	c.sendNotification(ctx, "thread/unarchived", codexThreadIDNotification{ThreadID: params.ThreadID})
	return codexResponseHandedOver{}, nil
}

// deleteThread 停止跟随根 Thread 所属对话，删除对话并通知客户端。
func (c *appServerConnection) deleteThread(ctx context.Context, requestID json.RawMessage, params codexThreadParams) (any, error) {
	// 1. 检查根对话，停止本连接跟随的这个对话的全部 Thread。
	if _, err := c.getRootThreadConversation(ctx, params.ThreadID, "thread/delete"); err != nil {
		return nil, err
	}
	var threads []*connectionThread
	for _, loaded := range c.loadedThreads {
		if loaded.conversationID == defs.ConversationID(params.ThreadID) {
			threads = append(threads, loaded)
		}
	}
	c.unloadThreads(ctx, threads...)
	// 2. 删除对话、Session 和历史；仍有 Turn 执行时返回 -32600，本连接保持已退订。
	err := c.client.DeleteConversation(ctx, &runtimeclient.DeleteConversationRequest{ConversationID: defs.ConversationID(params.ThreadID)})
	if errors.Is(err, session.ErrSessionBusy) {
		return nil, &codexError{
			errorCodeInvalidRequest,
			"cannot delete thread " + params.ThreadID + " while a turn is running",
		}
	}
	if err != nil {
		return nil, getCodexError(err, params.ThreadID)
	}
	// 3. 先响应删除成功，再发送 thread/deleted。
	c.send(ctx, newCodexResponse(requestID, codexEmptyObjectResult{}, nil))
	c.sendNotification(ctx, "thread/deleted", codexThreadIDNotification{ThreadID: params.ThreadID})
	return codexResponseHandedOver{}, nil
}

// getRootThreadConversation 读取根 Thread 的对话记录。
func (c *appServerConnection) getRootThreadConversation(ctx context.Context, threadID, method string) (defs.Conversation, error) {
	// 1. 按对话 ID 读取，非不存在错误直接返回。
	record, err := c.client.GetConversation(ctx, defs.ConversationID(threadID))
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, runtime.ErrConversationNotFound) {
		return defs.Conversation{}, getCodexError(err, threadID)
	}
	// 2. 子 Thread 和不存在的 Thread 均返回 -32600，分别说明仅根 Thread 支持此方法或 Thread 不存在。
	if _, historyErr := c.getHistoryThread(ctx, threadID); historyErr == nil {
		return defs.Conversation{}, &codexError{
			errorCodeInvalidRequest,
			"only root threads support " + method,
		}
	}
	return defs.Conversation{}, getCodexError(err, threadID)
}

// unsubscribeThread 停止本连接跟随 Thread。
func (c *appServerConnection) unsubscribeThread(ctx context.Context, params codexThreadParams) (any, error) {
	// 1. 未跟随时返回 notLoaded；退订只停止跟随，运行中的 Turn 继续执行。
	thread := c.loadedThreads[params.ThreadID]
	if thread == nil {
		return codexThreadUnsubscribeResult{Status: codexThreadNotLoaded}, nil
	}
	// 2. 根 Thread 连同本连接跟随的同对话子 Thread 一起退订，返回 unsubscribed。
	threads := []*connectionThread{thread}
	if thread.isRootThread() {
		for _, loaded := range c.loadedThreads {
			if loaded != thread && loaded.conversationID == thread.conversationID {
				threads = append(threads, loaded)
			}
		}
	}
	c.unloadThreads(ctx, threads...)
	return codexThreadUnsubscribeResult{Status: codexThreadUnsubscribed}, nil
}

// unloadThreads 停止并卸载已跟随的 Thread。
func (c *appServerConnection) unloadThreads(ctx context.Context, threads ...*connectionThread) {
	// 1. 关闭各 Thread 的退订通知，已开始的 Client 调用继续执行。
	for _, thread := range threads {
		close(thread.unsubscribed)
	}
	// 2. 等待各 Thread 结束或连接取消，再从 loadedThreads 删除；等待期间主循环不处理其他请求。
	// 还在等 Turn ID 的 turn/start 由 Thread 回复 thread not loaded。
	for _, thread := range threads {
		select {
		case <-thread.done:
		case <-ctx.Done():
		}
		delete(c.loadedThreads, thread.threadID)
	}
}

// getDirectInputThread 取得可接受直接输入的已加载根 Thread。
func (c *appServerConnection) getDirectInputThread(threadID string) (*connectionThread, error) {
	// 1. 子 Thread 拒绝直接输入，返回 -32600。
	if _, isChildThread := c.childThreadConversations[defs.SessionID(threadID)]; isChildThread {
		return nil, &codexError{
			errorCodeInvalidRequest,
			"direct app-server input is not allowed for multi-agent v2 sub-agents",
		}
	}
	// 2. 未加载返回 thread not loaded，否则返回根 Thread。
	thread := c.loadedThreads[threadID]
	if thread == nil {
		return nil, &codexError{
			errorCodeInvalidRequest,
			"thread not loaded: " + threadID,
		}
	}
	return thread, nil
}

// sendThreadRequest 将请求交给 Thread。
func (c *appServerConnection) sendThreadRequest(ctx context.Context, thread *connectionThread, request connectionThreadRequest) error {
	// 1. 发送请求；Thread 结束返回 -32600 和 thread not loaded，连接取消时返回取消原因。
	select {
	case thread.requests <- request:
		return nil
	case <-thread.done:
		return &codexError{
			errorCodeInvalidRequest,
			"thread not loaded: " + thread.threadID,
		}
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// readIncoming 持续读取并解码消息，交给连接主循环。
func (c *appServerConnection) readIncoming(ctx context.Context, received chan<- codexReceived) error {
	// 1. 给日志带上连接标识，逐条读取消息；读取不随 ctx 取消，由关闭传输打断。
	logger := telemetry.GetLogger(ctx).With("connection", fmt.Sprintf("%p", c))
	for {
		payload, err := c.transport.readMessage(context.WithoutCancel(ctx))
		var incoming codexReceived
		// 2. 超限消息交给主循环响应，其他读取错误结束；非法 JSON 记警告后丢弃，合法消息记调试日志。
		switch {
		case errors.Is(err, errMessageTooLarge):
			incoming.err = &codexError{
				errorCodeInvalidRequest,
				errMessageTooLarge.Error(),
			}
		case err != nil:
			return err
		default:
			if err := json.Unmarshal(payload, &incoming.message); err != nil {
				telemetry.GetLogger(ctx).Warn("invalid codex message dropped", "error", err)
				continue
			}
			if logger.Enabled(ctx, slog.LevelDebug) {
				logger.Debug("codex message received", "payload", string(payload))
			}
		}
		// 3. 将消息交给主循环，连接取消时返回。
		select {
		case received <- incoming:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// writeOutgoing 按入队顺序写出响应和通知。
func (c *appServerConnection) writeOutgoing(ctx context.Context) error {
	// 1. 给日志带上连接标识，逐条编码队列里的消息。
	logger := telemetry.GetLogger(ctx).With("connection", fmt.Sprintf("%p", c))
	for message := range c.outgoing {
		payload, err := json.Marshal(message)
		// 2. 无法编码的通知丢弃并记录警告，无法编码的响应替换为 -32603。
		if err != nil {
			if message.Method != "" {
				telemetry.GetLogger(ctx).Warn("codex notification dropped", "rpc.method", message.Method, "error", err)
				continue
			}
			payload, _ = json.Marshal(newCodexResponse(message.ID, nil, err))
		}
		// 3. 写出消息并记录调试日志，写失败返回错误，队列关闭后返回 nil。
		if err := c.transport.writeMessage(ctx, payload); err != nil {
			return fmt.Errorf("write codex message: %w", err)
		}
		if logger.Enabled(ctx, slog.LevelDebug) {
			logger.Debug("codex message sent", "payload", string(payload))
		}
	}
	return nil
}

// send 将消息交给连接写任务。
func (c *appServerConnection) send(ctx context.Context, message codexMessage) {
	// 1. 写入发送队列，连接上下文结束时丢弃消息并返回。
	select {
	case c.outgoing <- message:
	case <-ctx.Done():
	}
}

// sendNotification 编码通知并交给连接写任务。
func (c *appServerConnection) sendNotification(ctx context.Context, method string, params any) {
	// 1. 参数编码失败时记录警告并丢弃，成功时将通知入队。
	message, err := newCodexNotification(method, params)
	if err != nil {
		telemetry.GetLogger(ctx).Warn("codex notification dropped", "rpc.method", method, "error", err)
		return
	}
	c.send(ctx, message)
}
