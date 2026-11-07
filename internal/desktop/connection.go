package desktop

import (
	"errors"
	"slices"
	"strings"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// connect 异步发现服务，启动唯一 Client 和顺序事件读取任务。
func (d *Desktop) connect() {
	// 1. 发现服务或重连进行中不再重复发起；普通重连使用同一 Client，不再创建连接。
	if d.connecting {
		return
	}
	d.connecting = true
	if d.client != nil && !d.clientStopped {
		client := d.client
		d.tasks.Go(func() {
			err := client.Reconnect(d.ctx)
			d.window.Update(func() {
				if d.closed {
					return
				}
				d.connecting = false
				if err != nil {
					d.connectionError = err
				}
			})
		})
		return
	}
	// 2. 发现服务，在窗口线程发布 Client 后启动两个常驻任务。
	d.tasks.Go(func() {
		connection, err := cli.EnsureRuntimeServer(d.ctx, &d.opt.RuntimeServerStart)
		var client runtimeclient.Client
		if err == nil {
			opt := runtimeclient.NewClientOpt(connection.URL, connection.Token)
			opt.FailRequestsWhenDisconnected = true
			opt.DisableSubmitInputRetry = true
			opt.RPCFrameCapacity = 2000
			client, err = runtimeclient.NewClient(opt)
		}
		d.window.Update(func() {
			if d.closed {
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
			d.setServerIdentity(connection.ServerID)
			d.tasks.Go(func() {
				err := client.Run(d.ctx)
				d.window.Update(func() {
					if d.closed || d.client != client {
						return
					}
					d.clientStopped = true
					if err != nil && !errors.Is(err, d.ctx.Err()) {
						d.connectionError = err
					}
				})
			})
			d.tasks.Go(func() {
				for event := range client.Events() {
					d.window.Update(func() {
						if !d.closed && d.client == client {
							d.handleClientEvent(event)
						}
					})
				}
			})
		})
	})
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
			kit.Callout(c.Key("connection-banner"), kind, d.getConnectionMessage(), detail, func() {
				if isRetrying {
					kit.Spinner(c, kit.IconSizeDefault).Label(d.t("reconnecting"))
				}
				if kit.Button(c, d.t("retry")).Disabled(d.connecting).Clicked() {
					d.connect()
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
