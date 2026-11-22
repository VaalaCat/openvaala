package desktop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// getActiveServerTarget 返回当前应当连接的远端服务器档案；isLocal 为 true 时连接本机 serve。
func (d *Desktop) getActiveServerTarget() (target *remoteServer, isLocal bool) {
	// 1. 命令行 --url 一次性覆盖优先级最高。
	if d.serverOverride != nil {
		server := *d.serverOverride
		return &server, false
	}
	// 2. 偏好里显式选中的远端服务器。
	if d.preferences.ActiveServer != "" && d.preferences.ActiveServer != localServerID {
		if index := slices.IndexFunc(d.preferences.RemoteServers, func(s remoteServer) bool { return s.ID == d.preferences.ActiveServer }); index >= 0 {
			server := d.preferences.RemoteServers[index]
			return &server, false
		}
	}
	// 3. 偏好还没显式选过服务器时，回落到 config.yaml 的 client.url；否则连接本机。
	if d.preferences.ActiveServer == "" && d.defaultRemote != nil {
		server := *d.defaultRemote
		return &server, false
	}
	return nil, true
}

// getActiveServerLabel 返回状态栏和横幅里显示的当前服务器名称。
func (d *Desktop) getActiveServerLabel() string {
	target, isLocal := d.getActiveServerTarget()
	if isLocal {
		return d.t("server.local")
	}
	if target.Name != "" {
		return target.Name
	}
	return getRemoteServerHost(target.URL)
}

// connect 异步连接当前选定的本机或远端 serve，启动独立上下文的 Client 与事件读取任务。
func (d *Desktop) connect() {
	// 1. 发现服务或重连进行中不再重复发起；普通重连使用同一 Client，不再创建连接。
	if d.connecting {
		return
	}
	if d.client != nil && !d.clientStopped {
		d.connecting = true
		client := d.client
		connCtx := d.clientCtx
		if connCtx == nil {
			connCtx = d.ctx
		}
		generation := d.connectGeneration
		d.tasks.Go(func() {
			err := client.Reconnect(connCtx)
			d.window.Update(func() {
				if d.closed || d.connectGeneration != generation {
					return
				}
				d.connecting = false
				if err != nil && !errors.Is(err, connCtx.Err()) {
					d.connectionError = err
				}
			})
		})
		return
	}
	// 2. 启动参数解析失败时直接暴露错误，不静默切回本机。
	if d.startTargetError != nil && d.serverOverride == nil && d.preferences.ActiveServer == "" {
		d.connectionError = d.startTargetError
		d.connectionState = runtimeclient.ConnectionConnecting
		return
	}
	// 3. 为本次连接建立独立上下文，切服时可立即取消而不影响窗口生命周期。
	if d.clientCancel != nil {
		d.clientCancel()
	}
	connCtx, connCancel := context.WithCancel(d.ctx)
	d.clientCtx = connCtx
	d.clientCancel = connCancel
	d.connectGeneration++
	generation := d.connectGeneration
	d.connecting = true
	d.connectionError = nil
	target, isLocal := d.getActiveServerTarget()
	d.tasks.Go(func() {
		var (
			runtimeURL string
			token      string
			serverID   string
			err        error
		)
		if isLocal {
			var connection cli.RuntimeServerConnection
			connection, err = cli.EnsureRuntimeServer(connCtx, &d.opt.RuntimeServerStart)
			if err == nil {
				runtimeURL, token, serverID = connection.URL, connection.Token, connection.ServerID
			}
		} else {
			runtimeURL, token = target.URL, target.Token
		}
		var client runtimeclient.Client
		if err == nil {
			opt := runtimeclient.NewClientOpt(runtimeURL, token)
			opt.FailRequestsWhenDisconnected = true
			opt.DisableSubmitInputRetry = true
			opt.RPCFrameCapacity = 2000
			client, err = runtimeclient.NewClient(opt)
		}
		d.window.Update(func() {
			if d.closed || d.connectGeneration != generation {
				connCancel()
				return
			}
			d.connecting = false
			d.connectionError = err
			if err != nil {
				return
			}
			d.client = client
			d.clientStopped = false
			d.clientNeedsReading = d.readingTarget.ConversationID != ""
			d.setServerIdentity(serverID)
			d.tasks.Go(func() {
				err := client.Run(connCtx)
				d.window.Update(func() {
					if d.closed || d.client != client || d.connectGeneration != generation {
						return
					}
					d.clientStopped = true
					if err != nil && !errors.Is(err, connCtx.Err()) {
						d.connectionError = err
					}
				})
			})
			d.tasks.Go(func() {
				for event := range client.Events() {
					d.window.Update(func() {
						if !d.closed && d.client == client && d.connectGeneration == generation {
							d.handleClientEvent(event)
						}
					})
				}
			})
		})
	})
}

// switchServer 切换到指定服务器（localServerID 表示本机）；若有未保存设置编辑先弹确认框。
func (d *Desktop) switchServer(serverID string) {
	// 1. 保护未保存的设置草稿。
	if d.isProviderDirty() || d.isExecutorDirty() {
		d.confirm = confirmation{Kind: "switch-server", ServerID: serverID}
		d.confirmOpen = true
		return
	}
	d.forceSwitchServer(serverID)
}

// forceSwitchServer 取消旧连接、清空旧服务器业务状态并连接到新目标，同时保留按 serverID 隔离的输入草稿。
func (d *Desktop) forceSwitchServer(serverID string) {
	// 1. 停止当前连接并使旧异步回调失效。
	if d.clientCancel != nil {
		d.clientCancel()
		d.clientCancel = nil
	}
	d.clientCtx = nil
	d.connectGeneration++
	d.serverOverride = nil
	d.startTargetError = nil
	// 2. 更新并持久化当前选定的服务器。
	if serverID == "" || serverID == localServerID {
		d.preferences.ActiveServer = localServerID
	} else {
		d.preferences.ActiveServer = serverID
	}
	d.resetServerState()
	d.savePreferencesWithToast()
	if d.window != nil {
		d.connect()
	}
}

// resetServerState 清空属于上一台服务器的运行时列表与阅读状态，保留按 serverID 分桶的 d.drafts。
func (d *Desktop) resetServerState() {
	// 1. 取消正在进行的阅读拉取并释放当前终端屏幕。
	if d.cancelReading != nil {
		d.cancelReading()
		d.cancelReading = nil
	}
	d.closeTerminalStates()
	// 2. 重置连接状态、会话、配置与面板数据。
	d.client = nil
	d.connecting = false
	d.clientStopped = false
	d.clientNeedsReading = false
	d.connectionState = runtimeclient.ConnectionConnecting
	d.connectionError = nil
	d.serverID = ""
	d.conversations = nil
	d.conversationCursor = dao.RecordCursor[defs.ConversationID]{}
	d.conversationLoading = false
	d.conversationError = nil
	d.conversationRequest++
	d.conversationHighlight = ""
	d.runningKnown = false
	d.runningConversations = nil
	d.unreadConversations = make(map[defs.ConversationID]bool)
	d.readingTarget = readingTarget{}
	d.conversation = defs.Conversation{}
	d.previousConversation = defs.Conversation{}
	d.sessionInfos = nil
	d.items = nil
	d.toolProgress = nil
	d.itemCursor = 0
	d.readingReady = false
	d.historyLoading = false
	d.historyError = nil
	d.pendingInputs = session.SessionPendingInputs{}
	d.turns = make(map[defs.TurnID]defs.Turn)
	d.models = runtime.ModelList{}
	d.modelLoading = false
	d.modelsLoaded = false
	d.executors = nil
	d.executorsLoading = false
	d.executorConfigs = nil
	d.executorConfigsLoading = false
	d.executorConfigsError = nil
	d.sshConfigHosts = nil
	d.sshConfigHostsLoaded = false
	d.sshImportSelected = ""
	d.executorDraft = nil
	d.executorBaseline = nil
	d.executorSaving = false
	d.executorError = nil
	d.executorTesting = false
	d.executorTestSuccess = ""
	d.executorTestError = nil
	d.executorTestRequest++
	d.recentConversations = nil
	d.recentConversationsRequest++
	d.directoryError = nil
	d.directoryRequest++
	d.directoryListing = directoryListing{}
	d.agents = nil
	d.agentsError = nil
	d.skillCatalogs = make(map[skillCatalogKey]*skillCatalog)
	d.configuredSkills = nil
	d.itemExpanded = make(map[string]bool)
	d.inspectedItem = ""
	d.inspectedTurn = ""
	d.contextUsage = nil
	d.sessionDetails = nil
	d.contextLoading = false
	d.sessionDetailsLoading = false
	d.turnsLoading = false
	d.detailsError = nil
	d.turnCursor = dao.RecordCursor[defs.TurnID]{}
	d.turnPage = nil
	d.rpcFrames = nil
	d.rpcRows = nil
	d.rpcLoading = false
	d.rpcSelected = 0
	d.providers = nil
	d.defaultModel = defs.ModelRef{}
	d.modelDefaults = defs.ModelDefaults{}
	d.providerDraft = nil
	d.providerBaseline = nil
	d.providerLoading = false
	d.providerSaving = false
	d.providerError = nil
	d.hooks = nil
	d.mcpServers = nil
	d.mcpLoading = false
	d.mcpError = nil
	d.settingsError = nil
	d.settingsLoading = false
	d.settingsRequest++
	d.terminals = nil
	d.selectedTerminal = ""
	d.runningTerminals = nil
	d.terminalsKnown = false
	d.openingTerminalRequests = 0
	d.closingTerminalIDs = nil
}

// handleClientEvent 按真实事件归属更新连接、列表和当前阅读内容。
func (d *Desktop) handleClientEvent(event runtimeclient.ClientEvent) {
	// 1. 应用连接与全局状态。
	switch event := event.(type) {
	case *runtimeclient.ConnectionChanged:
		d.connectionState = event.State
		d.connectionError = event.Err
		if event.State == runtimeclient.ConnectionConnected {
			d.setServerIdentity(event.ServerID)
			if d.clientNeedsReading {
				d.clientNeedsReading = false
				d.openConversation(d.readingTarget.ConversationID, d.readingTarget.SessionID)
			}
			d.loadConversations(true)
			d.loadRecentConversations()
			d.loadModels(d.readingTarget)
			d.loadExecutors()
			d.loadAgents()
			client := d.client
			d.tasks.Go(func() { _ = client.SubscribeRunningConversations(d.ctx) })
			d.tasks.Go(func() { _ = client.SubscribeTerminals(d.ctx) })
			d.attachReconnectingTerminals()
		} else {
			d.handleTerminalsDisconnected()
		}
	case *runtimeclient.TerminalsChanged:
		d.handleTerminalsChanged(event)
	case *runtimeclient.TerminalOutput:
		d.handleTerminalOutput(event)
	case *runtimeclient.TerminalExited:
		d.handleTerminalExited(event)
	case *runtimeclient.RunningConversationsChanged:
		// 从运行集合中消失的非阅读对话，视为其 Turn 已结束。
		if d.runningKnown && event.IsKnown {
			for _, id := range d.runningConversations {
				if id != d.readingTarget.ConversationID && !slices.Contains(event.ConversationIDs, id) {
					d.handleConversationFinished(id, "")
				}
			}
		}
		d.runningKnown = event.IsKnown
		d.runningConversations = event.ConversationIDs
	case *runtimeclient.RPCLogChanged:
		if d.shownRightPanel == panelRPC {
			d.loadRPCFrames()
		}
	case *runtimeclient.ConversationSynced:
		if event.ConversationID != d.readingTarget.ConversationID {
			return
		}
		d.conversation = event.Conversation
		d.sessionInfos = event.SessionInfos
		if d.readingTarget.SessionID == "" {
			d.readingTarget.SessionID = event.Conversation.RootSessionID
		}
	case *runtimeclient.SessionSynced:
		if !d.isReading(event.ConversationID, event.SessionID) {
			return
		}
		d.readingReady = true
		d.pendingInputs = event.PendingInputs
		d.mergeSavedItems(event.Items, false)
		d.items = slices.DeleteFunc(d.items, func(view viewItem) bool { return view.Ordinal == 0 })
		for _, stream := range event.StreamingItems {
			d.mergeStreamingItem(stream.Item, stream.ItemIndex, false)
		}
	case *runtimeclient.ItemsSaved:
		if d.isReading(event.ConversationID, event.SessionID) {
			d.mergeSavedItems(event.Items, true)
			if event.SessionID == d.conversation.RootSessionID && !slices.ContainsFunc(d.conversations, func(entry dao.ConversationListEntry) bool { return entry.Conversation.ID == event.ConversationID }) && !d.conversationLoading {
				d.loadConversations(true)
			}
		}
	case *runtimeclient.ItemChanged:
		if d.isReading(event.ConversationID, event.SessionID) {
			d.applyItemChange(event)
		}
	case *runtimeclient.TurnStarted:
		if d.isReading(event.ConversationID, event.SessionID) {
			d.turns[event.TurnID] = defs.Turn{ID: event.TurnID, Status: defs.TurnStatusRunning}
		}
	case *runtimeclient.TurnFinished:
		d.handleConversationFinished(event.ConversationID, event.Error)
		if !d.isReading(event.ConversationID, event.SessionID) {
			return
		}
		turn := d.turns[event.TurnID]
		turn.ID = event.TurnID
		turn.Status = event.Status
		turn.Error = event.Error
		d.turns[event.TurnID] = turn
		d.loadTurnRecords(d.readingTarget, []defs.TurnID{event.TurnID})
		d.loadConversations(true)
	case *runtimeclient.SessionPendingInputsChanged:
		if d.isReading(event.ConversationID, event.SessionID) {
			d.pendingInputs = event.PendingInputs
		}
	case *runtimeclient.SessionStatusChanged:
		if event.ConversationID != d.readingTarget.ConversationID {
			return
		}
		found := false
		for i := range d.sessionInfos {
			if d.sessionInfos[i].Session.ID == event.Info.Session.ID {
				d.sessionInfos[i] = event.Info
				found = true
				break
			}
		}
		if !found {
			d.sessionInfos = append(d.sessionInfos, event.Info)
		}
	case *runtimeclient.ConversationTitleChanged:
		if event.ConversationID == d.conversation.ID {
			d.conversation.Title = event.Title
		}
		for i := range d.conversations {
			if d.conversations[i].Conversation.ID == event.ConversationID {
				d.conversations[i].Conversation.Title = event.Title
			}
		}
	case *runtimeclient.ConversationSyncFailed:
		if event.ConversationID == d.readingTarget.ConversationID {
			d.readingReady = false
			d.historyError = event.Err
		}
	case *runtimeclient.ToolProgress:
		if d.isReading(event.ConversationID, event.SessionID) {
			d.applyToolProgress(event)
		}
	case *runtimeclient.HookRunChanged:
		if d.isReading(event.ConversationID, event.SessionID) {
			d.applyHookRun(event)
		}
	}
}

// handleConversationFinished 窗口在后台时把对话标为未读，并发系统通知；点击通知聚焦窗口并打开该对话。
func (d *Desktop) handleConversationFinished(conversationID defs.ConversationID, failure string) {
	// 1. 窗口在前台时用户已经看到结果，不标记也不通知。
	if d.window == nil || d.window.IsFocused() {
		return
	}
	// 手测（macOS 真机）：设置 → 通用里关闭「完成时发送系统通知」，发一条要跑十几秒的消息后切到别的应用，完成时不应弹出系统通知，
	// 但侧栏该对话仍有未读蓝点；再打开开关重复一遍，应弹出通知，点击通知回到该对话。
	d.unreadConversations[conversationID] = true
	if !d.preferences.NotifyOnFinish {
		return
	}
	// 2. 标题和根 Session 优先取侧栏已有记录。
	conv := d.conversation
	if conv.ID != conversationID {
		conv = defs.Conversation{ID: conversationID}
		if index := slices.IndexFunc(d.conversations, func(entry dao.ConversationListEntry) bool { return entry.Conversation.ID == conversationID }); index >= 0 {
			conv = d.conversations[index].Conversation
		}
	}
	body := failure
	if body == "" {
		body = d.t("completed")
	}
	notification := mygo.NewNotification(mygo.NotificationOptions{ID: "conversation-" + string(conversationID), Title: conversationTitle(conv), Body: body})
	notification.OnClick(func() {
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.window.Restore()
			d.window.Show()
			d.window.Focus()
			d.navigate("")
			d.openConversation(conv.ID, conv.RootSessionID)
		})
	})
	// 3. 系统通知是辅助提示，不可用或被拒绝时不打扰用户。
	d.tasks.Go(func() {
		if mygo.NotificationsSupported() {
			_ = notification.Show()
		}
	})
}

// setServerIdentity 首次识别本机服务时迁移连接前的新对话草稿。
func (d *Desktop) setServerIdentity(serverID string) {
	// 1. 只迁移尚无服务器身份的草稿，已知服务器之间保持隔离。
	if serverID == "" {
		return
	}
	if d.serverID == "" {
		unknown := draftKey{}
		if draft := d.drafts[unknown]; draft != nil {
			d.drafts[draftKey{ServerID: serverID}] = draft
			delete(d.drafts, unknown)
		}
	}
	d.serverID = serverID
}

// isReading 事件是否属于此刻阅读的 Session？
func (d *Desktop) isReading(conversationID defs.ConversationID, sessionID defs.SessionID) bool {
	return d.readingTarget.ConversationID == conversationID && d.readingTarget.SessionID == sessionID
}

// getSessionInfo 返回当前阅读 Session 的服务端信息。
func (d *Desktop) getSessionInfo() runtime.SessionRuntimeInfo {
	// 1. 从对话订阅记录查找阅读目标。
	for _, info := range d.sessionInfos {
		if info.Session.ID == d.readingTarget.SessionID {
			return info
		}
	}
	return runtime.SessionRuntimeInfo{}
}

// connectionBannerView 在对话区顶部显示连接问题：本地化原因、原文详情和「重试」；重试期间显示转圈。
func (d *Desktop) connectionBannerView(c *ui.Context) {
	// 1. 初次连接中不打扰；断线重连、出现错误或 Client 已停止后才显示。
	isFailed := d.connectionError != nil || d.clientStopped
	if !isFailed && d.connectionState != runtimeclient.ConnectionReconnecting {
		return
	}
	isRetrying := d.connecting || d.connectionState == runtimeclient.ConnectionReconnecting
	kind := kit.CalloutWarning
	if isFailed {
		kind = kit.CalloutError
	}
	// 2. 原因用本地化文案，Go 错误原文放在详情里。
	t := kit.GetTheme(c)
	detail := ""
	if d.connectionError != nil {
		detail = d.connectionError.Error()
	}
	ui.Row(c).Shrink(0).Justify(ui.Center).Padding(t.Space(2), 0, 0, 0).Children(func() {
		ui.Column(c).Grow(1).MaxWidth(conversationColumnWidth+2*t.Space(4)).Padding(0, t.Space(4)).Children(func() {
			_, isLocal := d.getActiveServerTarget()
			kit.Callout(c.Key("connection-banner"), kind, d.getConnectionMessage(), detail, func() {
				if isRetrying {
					kit.Spinner(c, kit.IconSizeDefault).Label(d.t("reconnecting"))
				}
				if kit.Button(c, d.t("retry")).Disabled(d.connecting).Clicked() {
					d.connect()
				}
				if !isLocal {
					if kit.Button(c.Key("switch-local"), d.t("server.switch.local")).Clicked() {
						d.switchServer(localServerID)
					}
				}
			})
		})
	})
}

// getConnectionTooltip 返回连接问题的悬停提示：本地化原因，有错误时再换行附上原文。
func (d *Desktop) getConnectionTooltip() string {
	if d.connectionError == nil {
		return d.getConnectionMessage()
	}
	return d.getConnectionMessage() + "\n" + d.connectionError.Error()
}

// getConnectionMessage 返回连接问题的本地化原因；没有错误只是在重连时说明正在重连。
func (d *Desktop) getConnectionMessage() string {
	err := d.connectionError
	_, isLocal := d.getActiveServerTarget()
	if !isLocal {
		name := d.getActiveServerLabel()
		switch {
		case err == nil:
			return fmt.Sprintf(d.t("connection.banner.remote.reconnecting"), name)
		case errors.Is(err, runtimeclient.ErrUnauthorized):
			return fmt.Sprintf(d.t("connection.banner.remote.unauthorized"), name)
		case errors.Is(err, runtimeclient.ErrConnectionLost), errors.Is(err, runtimeclient.ErrClientStopped):
			return fmt.Sprintf(d.t("connection.banner.remote.lost"), name)
		}
		return fmt.Sprintf(d.t("connection.banner.remote.failed"), name)
	}
	switch {
	case err == nil:
		return d.t("connection.banner.reconnecting")
	case errors.Is(err, runtimeclient.ErrUnauthorized):
		return d.t("error.unauthorized")
	case strings.Contains(err.Error(), "runtime server exited"):
		return d.t("connection.banner.exited")
	case errors.Is(err, runtimeclient.ErrConnectionLost), errors.Is(err, runtimeclient.ErrClientStopped):
		return d.t("error.connection.lost")
	}
	return d.t("connection.banner.failed")
}
