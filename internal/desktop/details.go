package desktop

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/egoist/mygo/ui"
)

// loadShownPanels 在 Dock 里新显示的面板按需读取数据，与上一帧相比未变化时不重复读取。
func (d *Desktop) loadShownPanels() {
	// 1. 比较两个 Dock 本帧显示的面板与上一帧。
	right, bottom := d.getShownRightPanelID(), d.getShownBottomPanelID()
	previousRight, previousBottom := d.shownRightPanel, d.shownBottomPanel
	d.shownRightPanel, d.shownBottomPanel = right, bottom
	if right != previousRight {
		d.loadPanel(right)
	}
	if bottom != previousBottom {
		d.loadPanel(bottom)
	}
}

// loadPanel 读取面板显示所需的数据；Agent 与调试面板直接使用已订阅的数据。
func (d *Desktop) loadPanel(panelID string) {
	switch panelID {
	case panelContext:
		d.loadContext(false)
	case panelTurns:
		d.loadTurns(true)
	case panelRPC:
		d.loadRPCFrames()
	case panelTerminal:
		d.openFirstTerminal()
	}
}

// getShownRightPanelID 返回右侧当前显示的面板 ID（宽窗口为 Dock，窄窗口为抽屉）；没有显示时返回空字符串。
func (d *Desktop) getShownRightPanelID() string {
	workspace := &d.preferences.Workspace
	for _, panelID := range rightPanelIDs {
		if workspace.IsPanelShown(&workspace.Right, panelID) {
			return panelID
		}
	}
	return ""
}

// getShownBottomPanelID 返回下 Dock 当前显示的面板 ID；Dock 关闭时返回空字符串。
func (d *Desktop) getShownBottomPanelID() string {
	workspace := &d.preferences.Workspace
	for _, panelID := range bottomPanelIDs {
		if workspace.IsPanelShown(&workspace.Bottom, panelID) {
			return panelID
		}
	}
	return ""
}

// panelBody 绘制面板共有的错误行与无对话空状态；emptyTitle 非空表示面板依赖当前对话，没有对话时显示它，为空（RPC）时始终显示内容。
func (d *Desktop) panelBody(c *ui.Context, emptyTitle string, body func()) {
	// 1. 详情读取失败显示在面板顶部，没有对话时显示各面板自己的空状态。
	t := kit.GetTheme(c)
	ui.Column(c).Fill().Gap(0).Background(t.Colors.Panel).Children(func() {
		if d.detailsError != nil {
			d.errorMessageView(c, d.detailsError).Margin(t.Space(2), t.Space(3), 0, t.Space(3))
		}
		if emptyTitle != "" && d.readingTarget.ConversationID == "" {
			kit.EmptyState(c, emptyTitle, "")
			return
		}
		body()
	})
}

// panelScroll 绘制可滚动的面板内容，文字起点与 Dock 标题同在 x=12；body 里每个块是一个自己 0 间距的列，块与块之间 12。
func (d *Desktop) panelScroll(c *ui.Context, panelID string, body func()) {
	t := kit.GetTheme(c)
	ui.Scroll(c.Key(panelID)).Grow(1).Padding(t.Space(3)).Gap(t.Space(3)).Children(body)
}

// agentPanel 右 Dock 的 Agent 面板：Session 层级树。
func (d *Desktop) agentPanel(c *ui.Context) {
	d.panelBody(c, d.t("panel.agent.empty"), func() {
		ui.Scroll(c.Key(panelAgent)).Grow(1).Padding(0, 0, kit.GetTheme(c).Space(1), 0).Gap(0).Children(func() { d.agentsTreeView(c) })
	})
}

// contextPanel 右 Dock 的上下文面板：用量与会话配置。
func (d *Desktop) contextPanel(c *ui.Context) {
	d.panelBody(c, d.t("panel.context.empty"), func() { d.panelScroll(c, panelContext, func() { d.contextView(c) }) })
}

// turnsPanel 右 Dock 的 Turn 面板：游标分页的 Turn 历史。
func (d *Desktop) turnsPanel(c *ui.Context) {
	d.panelBody(c, d.t("panel.turns.empty"), func() { d.turnsView(c) })
}

// rpcPanel 右 Dock 的 RPC 面板：全局日志，不依赖当前对话。
func (d *Desktop) rpcPanel(c *ui.Context) {
	d.panelBody(c, "", func() { d.rpcLogView(c) })
}

// debugPanel 右 Dock 的调试面板：展示被选中的 Item 或 Turn 原文；还没选时显示空状态。
func (d *Desktop) debugPanel(c *ui.Context) {
	d.panelBody(c, d.t("panel.debug.empty"), func() {
		if d.inspectedItem == "" && d.inspectedTurn == "" {
			kit.EmptyState(c, d.t("choose.an.item.or.turn.to.inspect"), d.t("panel.debug.none.detail"))
			return
		}
		d.panelScroll(c, panelDebug, func() {
			if d.inspectedItem != "" {
				found := false
				for _, view := range d.items {
					if string(view.Item.ID) != d.inspectedItem {
						continue
					}
					found = true
					d.jsonView(c, "item-"+string(view.Item.ID), "Item", view.Item)
					if view.Item.ToolCall != nil {
						for _, result := range d.items {
							if result.Item.ToolResult != nil && result.Item.ToolResult.CallID == view.Item.ToolCall.CallID {
								d.jsonView(c, "result-"+string(result.Item.ID), d.t("associated.result"), result.Item)
							}
						}
					}
				}
				if !found {
					ui.Text(c, d.t("item.is.outside.loaded.history"))
				}
			}
			if d.inspectedTurn != "" {
				if turn, ok := d.turns[d.inspectedTurn]; ok {
					d.jsonView(c, "turn-"+string(turn.ID), "Turn", turn)
				}
			}
		})
	})
}

// agentsTreeView 从完整对话订阅构建父子层级，不为 Session 另开分页。
func (d *Desktop) agentsTreeView(c *ui.Context) {
	// 1. 深度从真实父 Session 链计算。
	t := kit.GetTheme(c)
	for _, info := range orderedSessionInfos(d.sessionInfos) {
		depth := 0
		parent := info.Session.ParentSessionID
		seen := map[defs.SessionID]bool{}
		for parent != "" && !seen[parent] {
			seen[parent] = true
			depth++
			next := defs.SessionID("")
			for _, candidate := range d.sessionInfos {
				if candidate.Session.ID == parent {
					next = candidate.Session.ParentSessionID
					break
				}
			}
			parent = next
		}
		// 2. 每个 Session 一行 ListRow：名称在行内，Agent 名、用量和非空闲状态在行尾；当前阅读的 Session 选中，运行中显示 Spinner。
		meta := info.Session.AgentName + " · " + string(info.State)
		if info.LastTurnStatus != "" {
			meta += " · " + string(info.LastTurnStatus)
		}
		row := kit.ListRow(c.Key(info.Session.ID), kit.ListRowOpt{
			Icon:     kit.IconBot,
			Title:    d.getSessionName(info.Session.ID),
			Selected: info.Session.ID == d.readingTarget.SessionID,
			Depth:    depth,
			Trailing: func() {
				if info.State == session.SessionStateRunning {
					kit.Spinner(c, kit.IconSizeDefault).Label(d.t("running"))
				}
				t.TextRole(ui.Text(c, info.Session.AgentName).SingleLine().Shrink(1).MinWidth(0), kit.TextCaption).TextColor(t.Colors.Placeholder)
				t.TextRole(ui.Text(c, fmt.Sprintf("↑%s ↓%s", tokenNumber(info.Usage.Input), tokenNumber(info.Usage.Output))).SingleLine().Shrink(0), kit.TextToken).TextColor(t.Colors.Muted)
				if info.State != session.SessionStateIdle && info.State != session.SessionStateRunning {
					t.TextRole(ui.Text(c, d.getSessionStateLabel(info.State)).SingleLine().Shrink(0), kit.TextCaption).TextColor(t.Colors.Placeholder)
				}
			},
		}).FillWidth().Label("Session " + string(info.Session.ID))
		kit.Tooltip(c, row, fmt.Sprintf("%s · %s %s · %s %s", meta, d.t("input.tokens"), tokenNumber(info.Usage.Input), d.t("output.tokens"), tokenNumber(info.Usage.Output)))
		if row.Clicked() {
			d.openSession(info.Session.ID)
		}
	}
}

// getSessionStateLabel 返回非空闲 Session 状态的本地化短词。
func (d *Desktop) getSessionStateLabel(state session.SessionState) string {
	key := "agent.state." + string(state)
	if label := d.t(key); label != key {
		return label
	}
	return string(state)
}

// orderedSessionInfos 按父子关系深度优先排列，同级使用稳定 Session ID。
func orderedSessionInfos(infos []runtime.SessionRuntimeInfo) []runtime.SessionRuntimeInfo {
	// 1. 原记录只读，父节点紧邻其全部后代。
	sorted := slices.Clone(infos)
	slices.SortFunc(sorted, func(a, b runtime.SessionRuntimeInfo) int {
		return strings.Compare(string(a.Session.ID), string(b.Session.ID))
	})
	known := map[defs.SessionID]bool{}
	for _, info := range infos {
		known[info.Session.ID] = true
	}
	visited := map[defs.SessionID]bool{}
	result := []runtime.SessionRuntimeInfo{}
	var appendSession func(runtime.SessionRuntimeInfo)
	appendSession = func(info runtime.SessionRuntimeInfo) {
		if visited[info.Session.ID] {
			return
		}
		visited[info.Session.ID] = true
		result = append(result, info)
		for _, candidate := range sorted {
			if candidate.Session.ParentSessionID == info.Session.ID {
				appendSession(candidate)
			}
		}
	}
	for _, info := range sorted {
		if info.Session.ParentSessionID == "" || !known[info.Session.ParentSessionID] {
			appendSession(info)
		}
	}
	for _, info := range sorted {
		appendSession(info)
	}
	return result
}

// sessionLink 导航至工具、来信或层级树指向的真实 Session。
func (d *Desktop) sessionLink(c *ui.Context, sessionID defs.SessionID) ui.Element {
	// 1. 已订阅 Session 直接切换；其他 Session 先查询所属对话。
	if sessionID == "" {
		return ui.Element{}
	}
	link := kit.Link(c, d.getSessionName(sessionID)).Label("Session " + string(sessionID))
	if link.Clicked() {
		d.openSession(sessionID)
	}
	return link
}

// getSessionName 返回 Session 的显示名：主 Session 是「主 Session」，子 Agent 优先用任务名，否则是 Session ID。
func (d *Desktop) getSessionName(sessionID defs.SessionID) string {
	name := string(sessionID)
	if sessionID == d.conversation.RootSessionID {
		name = d.t("root.session")
	}
	for _, info := range d.sessionInfos {
		if info.Session.ID == sessionID && info.Session.TaskName != "" {
			name = info.Session.TaskName
		}
	}
	return name
}

// openSession 切换到已订阅的 Session；未订阅时先查询所属对话再打开。
func (d *Desktop) openSession(sessionID defs.SessionID) {
	for _, info := range d.sessionInfos {
		if info.Session.ID == sessionID {
			d.openConversation(info.Session.ConversationID, sessionID)
			return
		}
	}
	client := d.client
	d.tasks.Go(func() {
		session, err := client.GetSessionByID(d.ctx, sessionID)
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.openConversation(session.ConversationID, sessionID)
		})
	})
}

// loadContext 按需读取用量和静态详情，刷新时保留已显示值。
func (d *Desktop) loadContext(details bool) {
	// 1. 绑定原阅读目标和本次详情查询。
	if d.client == nil || !d.readingReady || details && d.sessionDetailsLoading || !details && d.contextLoading {
		return
	}
	target, client := d.readingTarget, d.client
	if details {
		d.sessionDetailsLoading = true
	} else {
		d.contextLoading = true
	}
	d.detailsError = nil
	if details {
		d.tasks.Go(func() {
			result, err := client.GetSessionDetails(d.ctx, target.ConversationID, target.SessionID)
			d.window.Update(func() {
				if d.closed || d.readingTarget.Request != target.Request {
					return
				}
				d.sessionDetailsLoading = false
				d.detailsError = err
				if err == nil {
					d.sessionDetails = &result
				}
			})
		})
		return
	}
	d.tasks.Go(func() {
		usage, err := client.GetContextUsage(d.ctx, target.ConversationID, target.SessionID)
		d.window.Update(func() {
			if d.closed || d.readingTarget.Request != target.Request {
				return
			}
			d.contextLoading = false
			d.detailsError = err
			if err == nil {
				d.contextUsage = &usage
			}
		})
	})
}

// contextView 展示下一次请求的预计用量、阈值与最近模型上报用量；事实用两列，估算加「≈」，未知写「未知」。
// 每个块（下一次请求、最近一次请求、会话配置、原始数据）是 0 间距的列，事实行 28 高紧挨着，块与块之间由 panelScroll 隔开。
func (d *Desktop) contextView(c *ui.Context) {
	// 1. 第一块：小节标题右侧是刷新图标按钮。
	t := kit.GetTheme(c)
	usage := d.contextUsage
	ui.Column(c).Gap(0).Shrink(0).Children(func() {
		d.contextSection(c, d.t("next.request"), true)
		if usage == nil {
			t.TextRole(ui.Text(c, d.t("context.has.not.been.loaded")), kit.TextUI).TextColor(t.Colors.Muted)
			return
		}
		// 2. 来源未知时不画进度也不写 0。
		next := usage.NextRequest
		estimated := "≈"
		if next.Source == runtime.ContextUsageSourceActual {
			estimated = ""
		}
		if next.Source == runtime.ContextUsageSourceUnknown {
			d.contextFact(c, d.t("estimated.tokens"), d.t("unknown"), "")
		} else {
			if next.Window > 0 {
				ratio := min(float32(next.Tokens)/float32(next.Window), 1)
				ui.Row(c).Height(t.Metrics.ControlHeight).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
					kit.ProgressBar(c, ratio).Grow(1)
					t.TextRole(ui.Text(c, fmt.Sprintf("%.1f%%", float64(next.Tokens)*100/float64(next.Window))), kit.TextToken).TextColor(t.Colors.Muted)
				})
			}
			d.contextFact(c, d.t("estimated.tokens"), estimated+tokenNumber(next.Tokens), fmt.Sprint(next.Tokens))
			d.contextTokenFact(c, d.t("context.window"), "", next.Window)
		}
		d.contextFact(c, d.t("source"), d.t("context.source."+string(next.Source)), "")
		d.contextTokenFact(c, d.t("compaction.threshold"), "", usage.ThresholdTokens)
		d.contextTokenFact(c, d.t("system.prompt.tokens"), "≈", usage.SystemTokens)
		d.contextTokenFact(c, d.t("tool.definition.tokens"), "≈", usage.ToolTokens)
	})
	if usage == nil {
		return
	}
	// 3. 第二块：最近一次模型请求实际上报的用量。
	if last := usage.LastRequest; last != nil {
		ui.Column(c).Gap(0).Shrink(0).Children(func() {
			d.contextSection(c, d.t("last.request"), false)
			d.contextFact(c, d.t("model"), last.Model.String(), "")
			d.contextTokenFact(c, d.t("input"), "", last.Usage.Input)
			d.contextTokenFact(c, d.t("cache.read"), "", last.Usage.CacheRead)
			d.contextTokenFact(c, d.t("output"), "", last.Usage.Output)
		})
	}
	// 4. 第三块：会话配置，展开时才读取；Agent 和模型同样是事实行。
	wasOpen := d.sessionDetailsOpen
	kit.Disclosure(c, d.t("session.configuration"), &d.sessionDetailsOpen, func() {
		details := d.sessionDetails
		if details == nil {
			return
		}
		ui.Column(c).Gap(0).Children(func() {
			d.contextFact(c, d.t("agent"), details.Session.AgentName, "")
			if details.Model != nil {
				d.contextFact(c, d.t("model"), details.Model.Ref.String(), "")
			}
			if details.CompactModel != nil {
				d.contextFact(c, d.t("summary.model"), details.CompactModel.Ref.String(), "")
			}
		})
		d.jsonView(c, "executors-details", d.t("executors.skills.and.tools"), details)
	})
	if !wasOpen && d.sessionDetailsOpen {
		d.loadContext(true)
	}
	// 5. 第四块：原始 JSON 默认收起。
	open := d.itemExpanded["raw-context"]
	kit.Disclosure(c, d.t("raw.data"), &open, func() { d.jsonView(c, "raw-context", d.t("raw.context.data"), usage) })
	d.itemExpanded["raw-context"] = open
}

// contextSection 绘制面板内的小节标题；withRefresh 时右侧是刷新上下文的图标按钮。
func (d *Desktop) contextSection(c *ui.Context, title string, withRefresh bool) {
	t := kit.GetTheme(c)
	ui.Row(c.Key(title)).Height(t.Metrics.ControlHeight).Shrink(0).AlignItems(ui.Center).Children(func() {
		t.TextRole(ui.Text(c, title).SingleLine().Grow(1), kit.TextUI)
		if !withRefresh {
			return
		}
		refresh := kit.IconButtonTip(c, kit.IconRefresh, d.t("refresh.context")).TextColor(t.Colors.Muted).Disabled(d.contextLoading || d.sessionDetailsLoading)
		if refresh.Clicked() {
			d.loadContext(false)
			if d.sessionDetailsOpen {
				d.loadContext(true)
			}
		}
	})
}

// contextFact 绘制两列事实：标签次要色靠左，值靠右用等宽数字；tooltip 非空时悬停显示完整值。
func (d *Desktop) contextFact(c *ui.Context, label, value, tooltip string) {
	t := kit.GetTheme(c)
	row := ui.Row(c.Key(label)).Height(t.Metrics.ControlHeight).Shrink(0).Gap(t.Space(2)).AlignItems(ui.Center)
	row.Children(func() {
		ui.Row(c).Grow(1).Shrink(1).MinWidth(0).Gap(t.Space(2)).AlignItems(ui.Baseline).Children(func() {
			t.TextRole(ui.Text(c, label).SingleLine().Grow(1).Shrink(1).MinWidth(0), kit.TextUI).TextColor(t.Colors.Muted)
			t.TextRole(ui.Text(c, value).SingleLine().Shrink(0).Selectable(), kit.TextToken)
		})
	})
	if tooltip != "" {
		kit.Tooltip(c, row, tooltip)
	}
}

// contextTokenFact 绘制 token 数事实；prefix 是估算标记，数值不大于 0 时写「未知」。
func (d *Desktop) contextTokenFact(c *ui.Context, label, prefix string, value int) {
	if value <= 0 {
		d.contextFact(c, label, d.t("unknown"), "")
		return
	}
	d.contextFact(c, label, prefix+tokenNumber(value), fmt.Sprint(value))
}

// loadTurns 按游标读取新到旧的 Turn 页。
func (d *Desktop) loadTurns(reset bool) {
	if d.client == nil || !d.readingReady || d.turnsLoading {
		return
	}
	target, client := d.readingTarget, d.client
	cursor := d.turnCursor
	if reset {
		cursor.ID = ""
		cursor.UpdatedAt = 0
	}
	d.turnsLoading = true
	d.tasks.Go(func() {
		page, err := client.ListTurns(d.ctx, &runtimeclient.ListTurnsRequest{ConversationID: target.ConversationID, SessionID: target.SessionID, Cursor: cursor, Limit: 30, Order: storage.Order{Direction: storage.DirectionDesc}})
		d.window.Update(func() {
			if d.closed || d.readingTarget.Request != target.Request {
				return
			}
			d.turnsLoading = false
			d.detailsError = err
			if err != nil {
				return
			}
			if reset {
				d.turnPage = nil
			}
			for _, turn := range page.Turns {
				if !slices.ContainsFunc(d.turnPage, func(existing defs.Turn) bool { return existing.ID == turn.ID }) {
					d.turnPage = append(d.turnPage, turn)
				}
				d.turns[turn.ID] = turn
			}
			d.turnCursor = page.NextCursor
		})
	})
}

// turnsRefreshView 在 Turn 面板标题栏右端绘制刷新按钮。
func (d *Desktop) turnsRefreshView(c *ui.Context) {
	t := kit.GetTheme(c)
	refresh := kit.IconButtonTip(c.Key("turns-refresh"), kit.IconRefresh, d.t("refresh.turns")).TextColor(t.Colors.Muted).Disabled(d.turnsLoading || d.conversation.ID == "")
	if refresh.Clicked() {
		d.loadTurns(true)
	}
}

// turnsView 虚拟绘制 Turn，用服务端游标在列表尾部加载下一页。
func (d *Desktop) turnsView(c *ui.Context) {
	// 1. 刷新不清空旧页，选择与滚动跟随稳定 Turn ID。
	if d.turnList == nil {
		d.turnList = &ui.ListState{}
	}
	listState := d.turnList
	turns := slices.Clone(d.turnPage)
	target := d.readingTarget
	// 2. 序号从最早一轮数起；历史还没读完时不知道总数，不显示序号。
	hasMore := d.turnCursor.ID != ""
	listState.Key = func(index int) any { return turns[index].ID }
	ui.List(c.Key("turn-history"), listState, len(turns), func(index int) {
		if d.readingTarget.Request != target.Request {
			return
		}
		number := 0
		if !hasMore {
			number = len(turns) - index
		}
		d.turnRow(c, turns[index], number)
	}).Grow(1).Gap(0)
	// 3. 一次只请求下一游标页，不循环读取全部历史。
	if d.readingTarget.Request != target.Request {
		return
	}
	_, last := listState.Visible()
	if hasMore && last >= len(d.turnPage)-2 && !d.turnsLoading && d.detailsError == nil {
		d.loadTurns(false)
	}
	if d.turnsLoading {
		kit.InlineHint(c, d.t("loading"), true)
	}
}

// turnRow 绘制一个 Turn：28 高的行显示「#序号 首条输入摘要」、非成功状态图标和右侧相对时间；点击跳到该 Turn 的消息，行尾的调试按钮打开 Turn 原文。
func (d *Desktop) turnRow(c *ui.Context, turn defs.Turn, number int) {
	// 1. 标题是序号加输入摘要；摘要读不到（历史未加载）时用输入输出用量代替。
	t := kit.GetTheme(c)
	tokens := tokenNumber(turn.Usage.Input) + " / " + tokenNumber(turn.Usage.Output)
	title := cmp.Or(getTurnInputSummary(turn, d.items, 60), tokens)
	if number > 0 {
		title = fmt.Sprintf("#%d %s", number, title)
	}
	row := kit.ListRow(c.Key(turn.ID), kit.ListRowOpt{
		Title: title,
		Trailing: func() {
			kit.StatusIcon(c, getTurnActivityStatus(turn.Status))
			if turn.UpdatedAt != 0 {
				when := ui.Text(c, formatRelativeTime(turn.UpdatedAt, c.Now(), d.t)).SingleLine().Shrink(0)
				t.TextRole(when, kit.TextCaption).TextColor(t.Colors.Placeholder)
				kit.Tooltip(c, when, time.UnixMilli(turn.UpdatedAt).Local().Format("2006-01-02 15:04:05"))
			}
		},
		Actions: func() {
			if kit.IconButtonTip(c.Key("inspect-"+string(turn.ID)), kit.IconBug, d.t("inspect.turn")).Clicked() {
				d.inspectedTurn = turn.ID
				d.openPanel(panelDebug)
			}
		},
	}).FillWidth().Label(title)
	// 2. 悬停提示用量；失败的 Turn 带上错误原文。
	hint := d.t("input") + " " + tokenNumber(turn.Usage.Input) + " · " + d.t("output") + " " + tokenNumber(turn.Usage.Output)
	if turn.Error != "" {
		hint += "\n" + turn.Error
	}
	kit.Tooltip(c, row, hint)
	if row.Clicked() {
		d.turnJumpRequested = turn.ID
	}
}

// getTurnActivityStatus 返回 Turn 状态对应的状态图标；完成显示为空。
func getTurnActivityStatus(status defs.TurnStatus) kit.ActivityStatus {
	switch status {
	case defs.TurnStatusRunning:
		return kit.ActivityRunning
	case defs.TurnStatusFailed:
		return kit.ActivityFailed
	case defs.TurnStatusInterrupted:
		return kit.ActivityInterrupted
	}
	return ""
}
