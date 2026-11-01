package desktop

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/egoist/mygo/ui"
)

// sidebarView 展示「对话 / 归档」切换、搜索、游标分页及全局运行状态。
func (d *Desktop) sidebarView(c *ui.Context) {
	// 1. 设置打开时侧栏换成设置导航；否则标题栏右侧是归档开关和新建入口，搜索框紧贴其下，设置入口在状态栏。
	if d.page != "" {
		d.settingsNavView(c)
		return
	}
	t := kit.GetTheme(c)
	ui.Column(c).Fill().Gap(0).Children(func() {
		d.sidebarHeaderView(c)
		d.conversationSearchView(c)
		// 2. 错误、加载中和空状态都显示在列表顶部；归档为空时用自己的空状态占满列表区。
		if d.conversationError != nil {
			ui.Column(c).Padding(t.Space(2), t.Space(3)).Gap(t.Space(1)).Children(func() {
				d.errorMessageView(c, d.conversationError)
				if kit.Button(c, d.t("retry.list")).AlignSelf(ui.Start).Clicked() {
					d.loadConversations(true)
				}
			})
		}
		// 加载提示只在列表还没有内容时出现；刷新、搜索和翻页保留旧列表，不插提示行。
		isLoading := len(d.conversations) == 0 && (d.conversationLoading || d.connectionState != runtimeclient.ConnectionConnected)
		switch {
		case isLoading:
			kit.InlineHint(c, d.t("loading"), true)
		case len(d.conversations) == 0 && d.archived && d.conversationSearch == "":
			kit.EmptyState(c, d.t("no.archived.conversations"), d.t("no.archived.conversations.hint"))
			return
		case len(d.conversations) == 0:
			label := d.t("no.conversations")
			if d.conversationSearch != "" {
				label = d.t("no.matching.conversations")
			}
			kit.InlineHint(c, label, false)
		}
		// 3. 按更新时间分组；对话行整宽可点，运行中显示 Spinner，未读完成显示蓝点，操作按钮只在悬停或选中时出现。
		now := c.Now()
		rows := buildSidebarRows(d.conversations, now)
		d.conversationList.Key = func(i int) any { return rows[i].Key }
		ui.List(c.Key("conversations"), &d.conversationList, len(rows), func(i int) {
			if rows[i].Group != "" {
				ui.Row(c).Height(t.Metrics.ControlHeight).Padding(0, t.Space(3)).AlignItems(ui.Center).Children(func() {
					t.TextRole(ui.Text(c, d.t(rows[i].Group)), kit.TextCaption).SingleLine().TextColor(t.Colors.Muted)
				})
				return
			}
			d.conversationRowView(c, rows[i].Conversation, now)
		}).Grow(1).Padding(0, 0, t.Space(1), 0)
		d.scrollHighlightedConversation(rows)
		if !d.conversationLoading && d.conversationCursor != (dao.RecordCursor[defs.ConversationID]{}) {
			_, last := d.conversationList.Visible()
			if last >= len(rows)-3 {
				d.loadConversations(false)
			}
		}
	})
}

// sidebarHeaderView 绘制侧栏标题栏：标题在左（归档列表时是「归档」），右侧是归档开关和新建对话按钮；切换时重读对应列表。
func (d *Desktop) sidebarHeaderView(c *ui.Context) {
	// 1. 标题随列表种类变化。
	t := kit.GetTheme(c)
	title, toggleLabel := d.t("conversations"), d.t("archive.show")
	if d.archived {
		title, toggleLabel = d.t("archived"), d.t("archive.hide")
	}
	kit.Header(c, title, func() {
		// 2. 归档开关是 28 见方的图标按钮，归档列表显示时用选中底色；切换后清掉键盘高亮并重读列表。
		toggle := kit.IconButtonTip(c.Key("archive-toggle"), kit.IconArchive, toggleLabel)
		if d.archived {
			toggle.Background(t.Colors.Selected)
		}
		if toggle.Clicked() {
			d.archived = !d.archived
			d.conversationHighlight = ""
			d.loadConversations(true)
		}
		if kit.IconButtonTip(c, kit.IconPlus, d.t("new.conversation")).Clicked() {
			d.startNewConversation()
		}
	})
}

// conversationSearchView 绘制搜索行：输入框、清除按钮；Esc 清空，Up、Down 在列表里移动高亮，Enter 打开高亮的对话。
func (d *Desktop) conversationSearchView(c *ui.Context) {
	// 1. 搜索图标在左，输入框居中，有内容时右侧出现清除按钮。
	t := kit.GetTheme(c)
	ui.Row(c).Height(t.Metrics.ControlHeight).Shrink(0).Padding(0, t.Space(3)).Gap(t.Space(2)).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(t.Colors.BorderSubtle).Children(func() {
		ui.Icon(c, kit.IconSearch).Size(kit.IconSizeDefault, kit.IconSizeDefault).Shrink(0).TextColor(t.Colors.Placeholder)
		input := kit.TextInputWith(c.Key("conversation-search"), &d.conversationSearch, kit.InputOpt{Placeholder: d.t("search.conversations"), Borderless: true}).Label(d.t("search.conversations")).Grow(1).Bind(&d.conversationSearchHandle)
		input.OnTextKey(d.handleConversationSearchKey)
		if input.Changed() {
			d.conversationHighlight = ""
			d.loadConversations(true)
		}
		if d.conversationSearch != "" && kit.SmallIconButton(c, kit.IconX, d.t("clear.search")).Clicked() {
			d.clearConversationSearch()
			d.conversationSearchHandle.Focus()
		}
	})
}

// clearConversationSearch 清空搜索词并重读列表。
func (d *Desktop) clearConversationSearch() {
	d.conversationSearch = ""
	d.conversationHighlight = ""
	d.loadConversations(true)
}

// handleConversationSearchKey 处理搜索框的按键：Esc 清空搜索，Up、Down 移动高亮，Enter 打开高亮的对话（没有高亮时打开第一条）；处理了返回 true。
func (d *Desktop) handleConversationSearchKey(k ui.TextKey) bool {
	// 1. 只处理不带修饰键的按键，其余留给输入框编辑。
	if k.Mods != 0 {
		return false
	}
	index := slices.IndexFunc(d.conversations, func(entry dao.ConversationListEntry) bool { return entry.Conversation.ID == d.conversationHighlight })
	switch k.Key {
	case ui.KeyEscape:
		if d.conversationSearch == "" {
			return false
		}
		k.SetText("")
		d.clearConversationSearch()
		return true
	case ui.KeyDown, ui.KeyUp:
		if len(d.conversations) == 0 {
			return false
		}
		// 2. 没有高亮时 Down 到第一条、Up 到最后一条；到头后停住。
		next := index + 1
		if k.Key == ui.KeyUp {
			next = index - 1
			if index < 0 {
				next = len(d.conversations) - 1
			}
		}
		next = max(0, min(next, len(d.conversations)-1))
		d.conversationHighlight = d.conversations[next].Conversation.ID
		return true
	case ui.KeyEnter:
		if index < 0 && d.conversationSearch == "" || len(d.conversations) == 0 {
			return false
		}
		conv := d.conversations[max(index, 0)].Conversation
		d.openConversationFromList(conv)
		return true
	}
	return false
}

// scrollHighlightedConversation 键盘移动高亮后把对应行滚进可见区。
func (d *Desktop) scrollHighlightedConversation(rows []sidebarRow) {
	if d.conversationHighlight == "" {
		return
	}
	if index := slices.IndexFunc(rows, func(row sidebarRow) bool { return row.Conversation.ID == d.conversationHighlight }); index >= 0 {
		d.conversationList.ScrollIntoView(index)
	}
}

// conversationRowView 绘制侧栏的一条对话：标题与「目录名 · 相对时间」两行，行尾是状态指示，悬停时出现操作菜单；右键弹出同一份菜单。
func (d *Desktop) conversationRowView(c *ui.Context, conv defs.Conversation, now time.Time) {
	// 1. 整行是 ListRow：选中、悬停底色和文字起点 x=12 由组件给出；键盘高亮的行与选中行同底色。
	t := kit.GetTheme(c)
	selected := conv.ID == d.readingTarget.ConversationID && d.page == "" || conv.ID == d.conversationHighlight
	title := conversationTitle(conv)
	items := d.conversationMenuItems(conv, false)
	closed := false
	row := kit.ListRow(c.Key(conv.ID), kit.ListRowOpt{
		Title:       title,
		Description: getConversationSubtitle(conv, now, d.t),
		Selected:    selected,
		// 2. 运行中显示 Spinner，未读完成显示蓝点。
		Trailing: func() {
			if slices.Contains(d.runningConversations, conv.ID) {
				kit.Spinner(c, kit.IconSizeLarge).Label(d.t("running"))
			} else if d.unreadConversations[conv.ID] {
				kit.StatusDot(c, t.Colors.Accent).Label(d.t("unread"))
			}
		},
		// 3. ⋯ 按钮打开菜单，右键同一份。
		Actions: func() {
			closed = kit.MenuButton(c, kit.IconButtonTip(c, kit.IconEllipsis, d.t("conversation.actions")), items)
		},
	}).FillWidth().Label(title).Description(getConversationSubtitle(conv, now, d.t))
	closed = kit.ContextMenu(c, row, items) || closed
	d.handleMenuClosed(closed)
	// 4. 悬停提示完整目录；标题被截断时一并带上完整标题。
	full, _ := c.MeasureText(0, ui.Span{Text: title})
	if tip := getConversationTooltip(title, getConversationDirectory(conv), row.Bounds().W > 0 && full > row.Bounds().W-t.Space(6)); tip != "" {
		kit.Tooltip(c, row, tip)
	}
	if row.Clicked() {
		d.openConversationFromList(conv)
		d.closeSidebarDrawer()
	}
}

// openConversationFromList 打开侧栏里选中的对话；未保存的 Provider 原文需要确认放弃。
func (d *Desktop) openConversationFromList(conv defs.Conversation) {
	// 1. 未保存的设置草稿先确认，确认后由确认框继续导航。
	d.conversationHighlight = ""
	if d.isProviderDirty() {
		d.confirm = confirmation{Kind: "navigate-conversation", Conversation: conv}
		d.confirmOpen = true
		return
	}
	d.navigate("")
	d.openConversation(conv.ID, conv.RootSessionID)
}

// handleMenuClosed 菜单关闭的那一帧把键盘焦点还给输入框；选项打开了弹窗或表单浮层时让它们保留焦点。
func (d *Desktop) handleMenuClosed(closed bool) {
	if closed && !d.confirmOpen && !d.locationOpen && !d.workLocationPopoverOpen {
		d.composerFocusRequested = true
	}
}

// conversationMenuItems 返回对话操作菜单：重命名、分叉、导出（仅当前对话）、归档或恢复、删除。
func (d *Desktop) conversationMenuItems(conv defs.Conversation, withExport bool) []kit.MenuItem {
	// 1. 重命名与删除只打开确认框，真正写入仍在确认之后。
	// 手测（Linux X11）：点对话行的「⋯」和在对话行上右键，菜单应出现在指针或按钮旁、不越出窗口；
	// 菜单外观跟主题（不是 GTK 原生菜单）；按上下方向键移动高亮，Enter 执行，Esc 关闭后焦点回到输入框。
	archiveLabel := d.t("archive")
	if conv.ArchivedAt != 0 {
		archiveLabel = d.t("restore")
	}
	items := []kit.MenuItem{
		{Title: d.t("rename"), OnSelect: func() { d.requestRename(conv) }},
		{Title: d.t("fork"), OnSelect: func() { d.forkConversation(conv, "") }},
	}
	if withExport {
		items = append(items, kit.MenuItem{Title: d.t("export.markdown"), OnSelect: func() { d.exportConversation(conv) }})
	}
	return append(items,
		kit.MenuItem{Title: archiveLabel, OnSelect: func() { d.archiveConversation(conv) }},
		kit.MenuItem{Separator: true},
		kit.MenuItem{Title: d.t("delete"), Danger: true, OnSelect: func() { d.requestDelete(conv) }},
	)
}

// requestRename 打开重命名确认框。
func (d *Desktop) requestRename(conv defs.Conversation) {
	d.confirm = confirmation{Kind: "rename", Conversation: conv}
	d.renameText = conv.Title
	d.confirmOpen = true
}

// requestDelete 打开删除确认框，确认之后才真正删除。
func (d *Desktop) requestDelete(conv defs.Conversation) {
	d.confirm = confirmation{Kind: "delete-conversation", Conversation: conv, Name: conversationTitle(conv)}
	d.confirmOpen = true
}

// startNewConversation 离开设置页并打开空白新对话；未保存的 Provider 原文需要确认放弃。
func (d *Desktop) startNewConversation() {
	// 1. 未保存的设置草稿先确认，确认后由确认框继续导航。
	if d.isProviderDirty() {
		d.confirm = confirmation{Kind: "navigate-conversation"}
		d.confirmOpen = true
		return
	}
	d.navigate("")
	d.openConversation("", "")
}

// loadConversations 查询一页记录；旧筛选请求不改写新列表。
func (d *Desktop) loadConversations(reset bool) {
	// 1. 记录请求原筛选和游标。
	if d.client == nil {
		return
	}
	if d.conversationLoading && !reset {
		return
	}
	d.conversationRequest++
	requestID := d.conversationRequest
	d.conversationLoading = true
	d.conversationError = nil
	cursor := d.conversationCursor
	if reset {
		cursor = dao.RecordCursor[defs.ConversationID]{}
	}
	request := runtimeclient.ListConversationsRequest{SearchTerm: d.conversationSearch, Archived: d.archived, Cursor: cursor, Limit: 40, Order: storage.Order{By: storage.OrderByUpdatedAt, Direction: storage.DirectionDesc}}
	client := d.client
	// 2. 合并当前筛选页，不循环拉取全部历史。
	d.tasks.Go(func() {
		page, err := client.ListConversations(d.ctx, &request)
		d.window.Update(func() {
			if d.closed || requestID != d.conversationRequest {
				return
			}
			d.conversationLoading = false
			d.conversationError = err
			if err != nil {
				return
			}
			if reset {
				d.conversations = nil
			}
			for _, entry := range page.Conversations {
				index := slices.IndexFunc(d.conversations, func(old dao.ConversationListEntry) bool { return old.Conversation.ID == entry.Conversation.ID })
				if index < 0 {
					d.conversations = append(d.conversations, entry)
				} else {
					d.conversations[index] = entry
				}
			}
			d.conversationCursor = page.NextCursor
		})
	})
}

// openConversation 切换唯一阅读目标，保留所有会话的完整草稿。
func (d *Desktop) openConversation(conversationID defs.ConversationID, sessionID defs.SessionID) {
	// 1. 正在行内改名的标题先保存；取消原读取，保存已知的新对话根身份。
	d.commitTitleEdit()
	if d.cancelReading != nil {
		d.cancelReading()
	}
	if previous := d.conversation; previous.ID != "" && previous.ID != conversationID {
		d.previousConversation = previous
	}
	var selected defs.Conversation
	if d.conversation.ID == conversationID {
		selected = d.conversation
	}
	for _, entry := range d.conversations {
		if entry.Conversation.ID == conversationID {
			selected = entry.Conversation
			break
		}
	}
	// 2. 新目标立即移除旧数据，输入控件保持同一位置。
	d.readingTarget = readingTarget{ServerID: d.serverID, ConversationID: conversationID, SessionID: sessionID, Request: d.readingTarget.Request + 1}
	d.readingReady = false
	d.items = nil
	d.sessionInfos = nil
	d.conversation = selected
	d.itemCursor = 0
	d.historyError = nil
	d.historyLoading = false
	d.contextLoading = false
	d.sessionDetailsLoading = false
	d.turnsLoading = false
	d.turnList = &ui.ListState{}
	d.contextUsage = nil
	d.sessionDetails = nil
	d.turnPage = nil
	d.turnCursor = dao.RecordCursor[defs.TurnID]{}
	d.detailsError = nil
	d.inspectedItem = ""
	d.inspectedTurn = ""
	d.messageList = &ui.ListState{}
	d.messageMenuItem, d.turnJumpRequested = viewItem{}, ""
	delete(d.unreadConversations, conversationID)
	d.pendingInputs.Inputs = nil
	d.modelLoading = false
	d.composerFocusRequested = true
	if conversationID == "" {
		d.models, d.modelsLoaded = runtime.ModelList{}, false
		d.loadModels(d.readingTarget)
		return
	}
	if d.client == nil {
		return
	}
	readingContext, cancel := context.WithCancel(d.ctx)
	d.cancelReading = cancel
	d.historyLoading = true
	request := readingRequested{Context: readingContext, Target: d.readingTarget, Client: d.client}
	// 3. 只保留最新待执行目标；同一任务顺序调用 Client.OpenConversation。
	select {
	case <-d.readingRequests:
	default:
	}
	d.readingRequests <- request
}

// runReadingRequests 顺序执行最新阅读目标的订阅与最近页查询。
func (d *Desktop) runReadingRequests() {
	// 1. 旧请求取消后才允许下一个 Open 进入 Client。
	for {
		select {
		case <-d.ctx.Done():
			return
		case request := <-d.readingRequests:
			if request.Context.Err() != nil {
				continue
			}
			target := request.Target
			conv, err := request.Client.OpenConversation(request.Context, &runtimeclient.OpenConversationRequest{ConversationID: target.ConversationID, SessionID: target.SessionID, SkipHistory: true})
			if err != nil {
				d.window.Update(func() {
					if d.closed || d.readingTarget.Request != target.Request {
						return
					}
					d.historyLoading = false
					d.historyError = err
				})
				continue
			}
			if target.SessionID == "" {
				target.SessionID = conv.RootSessionID
			}
			page, err := request.Client.ListItems(request.Context, &runtimeclient.ListItemsRequest{ConversationID: target.ConversationID, SessionID: target.SessionID, Order: storage.DirectionDesc, Limit: 50})
			d.window.Update(func() {
				if d.closed || d.readingTarget.Request != target.Request {
					return
				}
				d.historyLoading = false
				d.historyError = err
				if err == nil {
					d.mergeSavedItems(page.Items, false)
					d.itemCursor = page.NextCursor
				}
				d.loadModels(d.readingTarget)
				d.loadVisibleTurns()
				d.loadPanel(d.getShownRightPanelID())
			})
		}
	}
}

// loadHistory 在顶部追加更早记录并保留 List 的稳定键锚点。
func (d *Desktop) loadHistory() {
	// 1. 一次只请求一页，失败可重试同一游标。
	if d.historyLoading || d.client == nil || !d.readingReady {
		return
	}
	d.historyLoading = true
	d.historyError = nil
	target, client, cursor := d.readingTarget, d.client, d.itemCursor
	d.tasks.Go(func() {
		page, err := client.ListItems(d.ctx, &runtimeclient.ListItemsRequest{ConversationID: target.ConversationID, SessionID: target.SessionID, Cursor: cursor, Limit: 50, Order: storage.DirectionDesc})
		d.window.Update(func() {
			if d.closed || d.readingTarget.Request != target.Request {
				return
			}
			d.historyLoading = false
			d.historyError = err
			if err != nil {
				return
			}
			d.mergeSavedItems(page.Items, false)
			d.itemCursor = page.NextCursor
			d.loadVisibleTurns()
		})
	})
}

// mergeSavedItems 将保存记录与实时条目按 Item 身份合并。
func (d *Desktop) mergeSavedItems(items []runtimeprotocol.LoggedItem, realtime bool) {
	// 1. 保存事实覆盖生成内容，同一 ordinal 或 ID 不重复。
	for _, logged := range items {
		if logged.Item == nil {
			continue
		}
		found := -1
		for i := range d.items {
			if d.items[i].Item.ID == logged.Item.ID || d.items[i].Ordinal == logged.Ordinal && logged.Ordinal != 0 {
				found = i
				break
			}
			if logged.Item.HookRun != nil && d.items[i].Item.HookRun != nil && logged.Item.HookRun.ID == d.items[i].Item.HookRun.ID {
				found = i
				break
			}
		}
		view := viewItem{Item: logged.Item.Clone(), Ordinal: logged.Ordinal}
		if realtime {
			view.ArrivedAt = time.Now()
			if found >= 0 {
				view.ArrivedAt = d.items[found].ArrivedAt
			}
		}
		if found < 0 {
			d.items = append(d.items, view)
		} else {
			d.items[found] = view
		}
	}
	slices.SortStableFunc(d.items, func(a, b viewItem) int {
		if a.Ordinal == 0 {
			if b.Ordinal == 0 {
				return 0
			}
			return 1
		}
		if b.Ordinal == 0 {
			return -1
		}
		if a.Ordinal < b.Ordinal {
			return -1
		}
		if a.Ordinal > b.Ordinal {
			return 1
		}
		return 0
	})
}

// mergeStreamingItem 保留真实 Item 身份与生成位置。
func (d *Desktop) mergeStreamingItem(item *defs.Item, index int, realtime bool) {
	// 1. 已保存记录不被迟到草稿覆盖。
	if item == nil {
		return
	}
	for i := range d.items {
		if d.items[i].Item.ID == item.ID {
			if d.items[i].Ordinal == 0 {
				d.items[i].Item = item.Clone()
				d.items[i].Streaming = true
				if !realtime {
					d.items[i].ArrivedAt = time.Time{}
				}
			}
			return
		}
	}
	view := viewItem{Item: item.Clone(), ItemIndex: index, Streaming: true}
	if realtime {
		view.ArrivedAt = time.Now()
	}
	d.items = append(d.items, view)
}

// applyItemChange 将原始增量交给 Agent 已有转换逻辑。
func (d *Desktop) applyItemChange(event *runtimeclient.ItemChanged) {
	// 1. 完整事件替换原条目，增量只修改对应 Turn 和位置。
	if event.Item != nil {
		d.mergeStreamingItem(event.Item, event.ItemIndex, true)
		if event.Type == agent.AgentEventItemCompleted || event.Type == agent.AgentEventItemInterrupted {
			for i := range d.items {
				if d.items[i].Item.ID == event.Item.ID {
					d.items[i].Streaming = false
				}
			}
		}
		return
	}
	if event.Type == agent.AgentEventItemInterrupted {
		d.items = slices.DeleteFunc(d.items, func(view viewItem) bool {
			return view.Ordinal == 0 && view.Item.TurnID == event.TurnID && view.ItemIndex == event.ItemIndex
		})
		return
	}
	for i := range d.items {
		view := &d.items[i]
		if view.Ordinal == 0 && view.Item.TurnID == event.TurnID && view.ItemIndex == event.ItemIndex {
			if event.Delta != nil {
				agent.ApplyItemDelta(view.Item, event.Delta)
			}
			return
		}
	}
}

// loadVisibleTurns 查询已显示内容涉及的 Turn 事实。
func (d *Desktop) loadVisibleTurns() {
	ids := []defs.TurnID{}
	for _, view := range d.items {
		if view.Item.TurnID != "" && !slices.Contains(ids, view.Item.TurnID) {
			ids = append(ids, view.Item.TurnID)
		}
	}
	d.loadTurnRecords(d.readingTarget, ids)
}

// loadTurnRecords 补齐最终回复识别需要的完整 Turn 记录。
func (d *Desktop) loadTurnRecords(target readingTarget, ids []defs.TurnID) {
	// 1. 绑定原目标，返回后合并记录。
	if len(ids) == 0 || d.client == nil {
		return
	}
	client := d.client
	d.tasks.Go(func() {
		turns, err := client.GetTurns(d.ctx, &runtimeclient.GetTurnsRequest{ConversationID: target.ConversationID, SessionID: target.SessionID, TurnIDs: ids})
		d.window.Update(func() {
			if d.closed || d.readingTarget.Request != target.Request || err != nil {
				return
			}
			for _, turn := range turns {
				d.turns[turn.ID] = turn
			}
		})
	})
}

// conversationView 绘制标题、消息历史与固定在底部的输入区。
func (d *Desktop) conversationView(c *ui.Context) {
	// 1. 标题栏左侧是对话标题（根 Session，点击改名）或子 Agent 面包屑，右侧是对话菜单。
	t := kit.GetTheme(c)
	ui.Column(c.Key("conversation-main")).Fill().Gap(0).Background(t.Colors.Panel).Children(func() {
		// 标题栏背景铺满，内容与消息列同宽居中：标题文字在列左缘，⋯ 按钮右缘与消息右侧操作图标对齐。
		ui.Row(c).Height(t.Metrics.HeaderHeight).Shrink(0).Justify(ui.Center).Background(t.Colors.Panel).BorderWidth(0, 0, 1, 0).BorderColor(t.Colors.Border).Children(func() {
			ui.Row(c).Grow(1).MaxWidth(conversationColumnWidth+2*t.Space(4)).Padding(0, t.Space(4)).Gap(t.Space(1)).AlignItems(ui.Center).Children(func() {
				if d.isReadingChildSession() {
					d.sessionCrumbsView(c)
				} else {
					d.conversationTitleView(c)
				}
				if d.conversation.ID != "" {
					closed := kit.MenuButton(c, kit.IconButtonTip(c, kit.IconEllipsis, d.t("conversation.actions")), d.conversationMenuItems(d.conversation, true))
					d.handleMenuClosed(closed)
				}
			})
		})
		d.connectionBannerView(c)
		if d.readingTarget.ConversationID == "" {
			d.newConversationEmptyView(c)
		} else {
			if d.historyError != nil {
				ui.Row(c).Padding(t.Space(2), t.Space(4)).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
					d.errorMessageView(c, d.historyError).Shrink(1)
					if kit.Button(c, d.t("retry.history")).Clicked() {
						if d.readingReady {
							d.loadHistory()
						} else {
							d.openConversation(d.readingTarget.ConversationID, d.readingTarget.SessionID)
						}
					}
				})
			}
			if d.historyLoading {
				kit.InlineHintWith(c, d.t("loading.history"), true, kit.InlineHintOpt{Inset: kit.GetTheme(c).Space(4)})
			}
			d.itemsView(c)
		}
		if !d.isReadingChildSession() {
			d.composerView(c)
		} else {
			// 子 Session 只读：输入区只有说明，不显示模型、推理、工作位置和上下文控件。
			ui.Column(c).Shrink(0).Padding(t.Space(2), t.Space(3)).Background(t.Colors.Editor).BorderWidth(1, 0, 0, 0).BorderColor(t.Colors.Border).Children(func() {
				ui.Row(c).Justify(ui.Center).Children(func() {
					t.TextRole(ui.Text(c, d.t("sub.agent.session.read.only.input")).Grow(1).MaxWidth(conversationColumnWidth+t.Space(2)).Padding(0, t.Space(1)), kit.TextUI).TextColor(t.Colors.Muted)
				})
			})
		}
	})
}

// newConversationEmptyView 绘制新对话的空状态，居中在对话区；连上服务但没有任何模型时改成引导添加模型服务，带「添加模型服务」主按钮。
func (d *Desktop) newConversationEmptyView(c *ui.Context) {
	// 1. 没有模型服务：标题、说明和主按钮。
	if d.isProviderMissing() {
		kit.EmptyStateWith(c, d.t("error.no.model"), d.t("onboarding.detail"), kit.EmptyStateOpt{ActionLabel: d.t("onboarding.add"), OnAction: d.openProviderOnboarding})
		return
	}
	// 2. 其余情况只显示提示和说明。
	kit.EmptyState(c, d.t("what.would.you.like.to.work.on"), d.t("no.conversations.hint"))
}

// isProviderMissing 是否已连上本机服务、并确认没有任何可用的模型？
func (d *Desktop) isProviderMissing() bool {
	return d.connectionState == runtimeclient.ConnectionConnected && d.modelsLoaded && len(d.models.Models) == 0
}

// openProviderOnboarding 打开设置里的模型服务页，并直接展开服务目录。
func (d *Desktop) openProviderOnboarding() {
	d.catalogOpen = true
	d.navigate("providers")
}

// isReadingChildSession 当前阅读的是不是子 Agent 的 Session？
func (d *Desktop) isReadingChildSession() bool {
	return d.conversation.ID != "" && d.readingTarget.SessionID != "" && d.readingTarget.SessionID != d.conversation.RootSessionID
}

// conversationTitleView 绘制标题栏左侧的对话标题：点击进入行内改名，Enter 保存、Esc 取消、失焦保存；新对话只显示文字。
func (d *Desktop) conversationTitleView(c *ui.Context) {
	// 1. 新对话没有标题可改，只显示文字。
	t := kit.GetTheme(c)
	if d.conversation.ID == "" {
		t.TextRole(ui.Text(c, d.t("new.conversation")), kit.TextUI).SingleLine().Grow(1).Font(d.preferences.TitleFont)
		return
	}
	// 2. 行内编辑：输入框占满标题位置，获得焦点后才开始判断失焦。
	if d.titleEditingID == d.conversation.ID {
		input := kit.TextInputWith(c.Key("title-edit"), &d.titleDraft, kit.InputOpt{}).Label(d.t("rename.conversation")).Grow(1)
		input.OnTextKey(func(k ui.TextKey) bool {
			switch {
			case k.Key == ui.KeyEnter && k.Mods == 0:
				d.titleDraft = k.Text()
				d.commitTitleEdit()
				d.composerFocusRequested = true
				return true
			case k.Key == ui.KeyEscape && k.Mods == 0:
				d.titleEditingID = ""
				d.composerFocusRequested = true
				return true
			}
			return false
		})
		if d.titleFocusRequested {
			d.titleFocusRequested = false
			input.Focus()
		}
		focused := input.Focused()
		if d.titleWasFocused && !focused {
			d.commitTitleEdit()
		}
		d.titleWasFocused = focused
		return
	}
	// 3. 平时是文字按钮：悬停变强调色，点击进入编辑；被截断时提示里带完整标题。
	title := conversationTitle(d.conversation)
	button := ui.ButtonBase(c.Key("title-rename")).Shrink(1).MinWidth(0).Height(t.Metrics.ControlHeight).Padding(0).Radius(0).Justify(ui.Start).AlignItems(ui.Center).Label(title)
	color := t.Colors.Text
	if button.Hovered() {
		color = t.Colors.Accent
	}
	var titleText ui.Element
	button.Children(func() {
		titleText = t.TextRole(ui.Text(c, title), kit.TextUI).SingleLine().Shrink(1).MinWidth(0).TextColor(color).Font(d.preferences.TitleFont)
	})
	hint := d.t("rename.conversation")
	if full, _ := c.MeasureText(0, ui.Span{Text: title, Size: 14, Font: d.preferences.TitleFont}); titleText.Bounds().W > 0 && full > titleText.Bounds().W {
		hint = title + "\n" + hint
	}
	kit.Tooltip(c, button, hint)
	if button.Clicked() {
		d.titleEditingID, d.titleDraft = d.conversation.ID, d.conversation.Title
		d.titleFocusRequested, d.titleWasFocused = true, false
	}
	ui.Spacer(c)
}

// commitTitleEdit 结束行内改名：标题有改动且不为空时保存。
func (d *Desktop) commitTitleEdit() {
	// 1. 编辑目标已切走、没改或清空的标题都当作取消。
	conversationID := d.titleEditingID
	d.titleEditingID, d.titleWasFocused = "", false
	title := strings.TrimSpace(d.titleDraft)
	if conversationID == "" || conversationID != d.conversation.ID || title == "" || title == d.conversation.Title || d.client == nil {
		return
	}
	// 2. 写入后由标题事件更新界面；成功和失败都用通知反馈。
	client := d.client
	d.tasks.Go(func() {
		err := client.SetConversationTitle(d.ctx, conversationID, title)
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.loadConversations(true)
			d.showSuccess(d.t("conversation.renamed"), title)
		})
	})
}

// sessionCrumbsView 绘制子 Session 标题栏的面包屑「主 Agent › … › 当前」：祖先每一级都能点，最后一级是当前 Session。
func (d *Desktop) sessionCrumbsView(c *ui.Context) {
	// 1. 子 Agent 订阅还没到时，先显示主 Agent 加当前 Session ID。
	t := kit.GetTheme(c)
	rootName := d.t("root.session")
	crumbs := getSessionCrumbs(d.sessionInfos, d.conversation.RootSessionID, d.readingTarget.SessionID, rootName)
	if len(crumbs) == 0 {
		crumbs = []sessionCrumb{{SessionID: d.conversation.RootSessionID, Name: rootName}, {SessionID: d.readingTarget.SessionID, Name: string(d.readingTarget.SessionID)}}
	}
	// 2. 祖先是次要色的可点文字，悬停变正文色加下划线；当前一级是正文色文字。
	ui.Row(c.Key("session-crumbs")).Grow(1).Shrink(1).MinWidth(0).Gap(t.Space(1)).AlignItems(ui.Center).Children(func() {
		for index, crumb := range crumbs {
			if index == len(crumbs)-1 {
				current := t.TextRole(ui.Text(c, crumb.Name), kit.TextUI).SingleLine().Shrink(1).MinWidth(0).Font(d.preferences.TitleFont)
				kit.Tooltip(c, current, crumb.Name)
				continue
			}
			link := ui.ButtonBase(c.Key("crumb-" + string(crumb.SessionID))).Shrink(1).MinWidth(0).Height(t.Metrics.ControlHeight).Padding(0).Radius(0).AlignItems(ui.Center).Label(crumb.Name)
			link.Children(func() {
				color := t.Colors.Muted
				if link.Hovered() {
					color = t.Colors.Text
				}
				text := t.TextRole(ui.Text(c, crumb.Name), kit.TextUI).SingleLine().Shrink(1).MinWidth(0).TextColor(color)
				if link.Hovered() {
					text.Underline()
				}
			})
			if link.Clicked() {
				d.openConversation(d.conversation.ID, crumb.SessionID)
			}
			ui.Icon(c, kit.IconChevronRight).Size(kit.IconSizeSmall, kit.IconSizeSmall).Shrink(0).TextColor(t.Colors.Placeholder)
		}
		ui.Spacer(c)
	})
}

// loadModels 按真实 Agent 查询模型目录，不发送空 Agent 的目标请求。
func (d *Desktop) loadModels(target readingTarget) {
	// 1. 读取原目标 Agent 名称。
	if d.client == nil {
		return
	}
	agentName := d.newAgent
	if agentName == "" && target.ConversationID == "" {
		d.models = runtime.ModelList{}
		return
	}
	if target.ConversationID != "" {
		agentName = d.getSessionInfo().Session.AgentName
		if agentName == "" {
			return
		}
	}
	d.modelLoading = true
	client := d.client
	d.tasks.Go(func() {
		models, err := client.ListModels(d.ctx, agentName)
		d.window.Update(func() {
			if d.closed || d.readingTarget.Request != target.Request {
				return
			}
			if target.ConversationID == "" && d.newAgent != agentName {
				return
			}
			d.modelLoading = false
			if err != nil {
				d.showFailure(err)
				return
			}
			d.models, d.modelsLoaded = models, true
		})
	})
}

// composerFoldWidth 是输入区的折叠阈值：输入区比它窄时，推理强度和工作位置收进「更多」菜单。
const composerFoldWidth float32 = 480

// sessionControls 按上下文圆环、模型、推理强度、工作位置、压缩的顺序显示输入区右侧控件；composer 是输入区容器，窄时折叠、新对话的浮层也靠它定位。
func (d *Desktop) sessionControls(c *ui.Context, composer ui.Element) {
	// 1. 新目标未同步时保留中性禁用控件。
	info := d.getSessionInfo()
	model, effort := d.getModelSelection()
	ready := d.connectionState == runtimeclient.ConnectionConnected && !d.modelLoading
	if d.readingTarget.ConversationID != "" {
		ready = ready && d.readingReady
	}
	setModel := d.setModelSelection
	// 2. 推理强度只列所选模型支持的档位；没有档位时不显示这个控件。
	efforts := getReasoningEfforts(d.models, model)
	if !slices.Contains(efforts, effort) {
		effort = ""
	}
	isFolded := composer.Valid() && composer.Bounds().W > 0 && composer.Bounds().W < composerFoldWidth
	// 3. 顺序固定：上下文圆环、模型、推理强度、工作位置、压缩；输入区太窄时后两项折进「更多」。
	ui.Row(c.Key("model-controls")).Gap(4).AlignItems(ui.Center).Children(func() {
		if d.readingTarget.ConversationID != "" {
			d.contextRingView(c, info, ready)
		}
		modelSelect := kit.Select(c.Key("model"), &model, kit.SelectOpt{Options: getModelOptions(d.models.Models, d.t("agent.default")), Label: d.t("model"), Placeholder: d.t("model"), Searchable: true, SearchPlaceholder: d.t("filter.provider.model"), Disabled: !ready})
		hint := getModelTooltip(d.models.Models, model, d.t("agent.default"))
		if !ready {
			hint = d.t("loading")
			if d.connectionState != runtimeclient.ConnectionConnected {
				hint = d.t("connecting")
			}
			if d.connectionError != nil {
				hint = d.getConnectionTooltip()
			}
		}
		kit.Tooltip(c, modelSelect, hint)
		if modelSelect.Changed() {
			setModel(model, effort)
		}
		if isFolded {
			d.moreControls(c, composer, efforts, effort, ready, func(next string) { setModel(model, next) })
		} else {
			if len(efforts) > 0 {
				// 选项只写档位，触发器显示「推理：档位」。
				values, labels := []string{""}, []string{d.t("automatic")}
				for _, value := range efforts {
					values, labels = append(values, value), append(labels, d.getEffortLabel(value))
				}
				triggerLabel := func(selected kit.SelectOption) string { return fmt.Sprintf(d.t("reasoning.label"), selected.Label) }
				if kit.Select(c.Key("reasoning"), &effort, kit.SelectOpt{Options: selectOptions(values, labels), Label: d.t("reasoning.effort"), Placeholder: d.t("reasoning.effort"), TriggerLabel: triggerLabel, Disabled: !ready}).Changed() {
					setModel(model, effort)
				}
			}
			d.workLocationButton(c, composer)
		}
		if d.conversation.ID != "" && d.readingTarget.SessionID == d.conversation.RootSessionID {
			if kit.IconButtonTip(c, kit.IconFold, d.t("compact.context")).Disabled(!ready || info.ActiveTurnID != "").Clicked() {
				d.compactContext()
			}
		}
	})
}

// getModelSelection 返回输入区当前的模型和推理强度：已有对话取根 Session 的选择，新对话取表单里的；空模型表示 Agent 默认。
func (d *Desktop) getModelSelection() (model, effort string) {
	if d.readingTarget.ConversationID == "" {
		return d.newModel, d.newEffort
	}
	selection := d.getSessionInfo().Session.Model
	if selection.Ref != (defs.ModelRef{}) {
		model = selection.Ref.String()
	}
	return model, selection.ReasoningEffort
}

// setModelSelection 保存模型和推理强度：新对话记在表单里，已有对话写入 Session。
func (d *Desktop) setModelSelection(model, effort string) {
	if d.readingTarget.ConversationID == "" {
		d.newModel, d.newEffort = model, effort
		return
	}
	d.setSessionModel(model, effort)
}

// getEffortLabel 返回推理强度档位的本地化名称；没有对应翻译的档位原样显示。
func (d *Desktop) getEffortLabel(effort string) string {
	key := "effort." + effort
	if text := d.t(key); text != key {
		return text
	}
	return effort
}

// getWorkLocationExecutor 返回输入区当前的工作位置：已有对话取它的默认 Executor 和目录，新对话取表单里选的；目录为空时用 Executor 的默认根目录。
func (d *Desktop) getWorkLocationExecutor() (executorID, directory string) {
	// 1. 新对话用表单的值。
	executorID, directory = d.newExecutor, d.newDirectory
	if d.conversation.ID != "" {
		executorID = d.conversation.ExecutorSettings.DefaultExecutorID
		if executorID == "" {
			executorID = "local"
		}
		directory = d.conversation.ExecutorSettings.WorkspaceDirectories[executorID]
	}
	if directory == "" {
		directory = getDefaultDirectory(d.executors, executorID)
	}
	return executorID, directory
}

// workLocationButton 绘制工作位置按钮：显示 Executor 和目录名；已有对话打开设置弹窗，新对话在输入区上方展开表单。
func (d *Desktop) workLocationButton(c *ui.Context, composer ui.Element) {
	// 1. 按钮文字截断目录名，悬停显示完整目录和（已有对话的）Agent。
	executorID, directory := d.getWorkLocationExecutor()
	button := kit.ToolButton(c.Key("work-location"), kit.IconFolder, getWorkLocationLabel(executorID, directory, 16), true).Label(d.t("workspace"))
	hint := executorID
	if directory != "" {
		hint += " · " + directory
	}
	if d.conversation.ID != "" {
		hint += "\n" + fmt.Sprintf(d.t("agent.hint"), d.getSessionInfo().Session.AgentName)
	}
	kit.Tooltip(c, button, hint)
	if button.Clicked() {
		d.openWorkLocationFor(!d.workLocationPopoverOpen)
	}
	d.newConversationPopover(c, button, composer)
}

// openWorkLocationFor 打开工作位置：已有对话是设置弹窗，新对话是输入区上方的表单浮层，isOpen 为假时关闭浮层。
func (d *Desktop) openWorkLocationFor(isOpen bool) {
	// 1. 已有对话没有浮层，只有弹窗。
	if d.conversation.ID != "" {
		d.openWorkLocation()
		return
	}
	d.workLocationPopoverOpen = isOpen
}

// newConversationPopover 在新对话时显示工作位置表单：浮层贴着输入区上缘、右缘与输入区对齐，不盖住输入区；anchor 是打开它的按钮。
func (d *Desktop) newConversationPopover(c *ui.Context, anchor, composer ui.Element) {
	// 1. 已有对话没有浮层；窄屏折叠时锚点是「更多」按钮。
	if d.conversation.ID != "" {
		d.workLocationPopoverOpen = false
		return
	}
	t := kit.GetTheme(c)
	ui.PopoverBase(c, anchor, &d.workLocationPopoverOpen, func(panel ui.Element) {
		panel.AttachTo(composer, ui.AnchorTopEnd, ui.AnchorBottomEnd).Margin(0, t.Space(3), t.Space(1), 0)
		panel.Radius(t.Metrics.RadiusLarge).Padding(t.Space(4)).Background(t.Colors.Elevated).Border(1, t.Colors.Border)
		panel.Children(func() { d.newConversationFields(c) })
	})
}

// moreControls 绘制窄输入区里的「更多」菜单：推理强度（有档位时）和工作位置；setEffort 保存所选推理强度。
func (d *Desktop) moreControls(c *ui.Context, composer ui.Element, efforts []string, effort string, ready bool, setEffort func(string)) {
	// 1. 推理强度逐档一项，当前档打勾。
	items := []kit.MenuItem{}
	if len(efforts) > 0 && ready {
		for _, value := range append([]string{""}, efforts...) {
			label := d.getEffortLabel(value)
			if value == "" {
				label = d.t("automatic")
			}
			item := kit.MenuItem{Title: fmt.Sprintf(d.t("reasoning.label"), label), OnSelect: func() { setEffort(value) }}
			if value == effort {
				item.Icon = kit.IconCheck
			}
			items = append(items, item)
		}
		items = append(items, kit.MenuItem{Separator: true})
	}
	// 2. 工作位置一项，选中后和宽屏的按钮一样打开表单或弹窗。
	executorID, directory := d.getWorkLocationExecutor()
	items = append(items, kit.MenuItem{Icon: kit.IconFolder, Title: d.t("workspace") + "：" + getWorkLocationLabel(executorID, directory, 16), OnSelect: func() { d.openWorkLocationFor(true) }})
	trigger := kit.IconButtonTip(c, kit.IconEllipsis, d.t("more"))
	closed := kit.MenuButton(c, trigger, items)
	d.newConversationPopover(c, trigger, composer)
	d.handleMenuClosed(closed)
}

// contextRingView 用环形进度显示下一次请求的上下文占用，悬停提示已用、窗口和压缩阈值，点击打开右 Dock 的上下文面板。
func (d *Desktop) contextRingView(c *ui.Context, info runtime.SessionRuntimeInfo, ready bool) {
	// 1. 窗口或来源未知时不画比例，提示文字说明未知。
	t := kit.GetTheme(c)
	usage := info.Context
	known := usage.Source != runtime.ContextUsageSourceUnknown && usage.Window > 0
	var ratio float32
	hint := d.t("unknown.context")
	if known {
		ratio = float32(usage.Tokens) / float32(usage.Window)
		percent := fmt.Sprintf("%d%%", int(math.Round(float64(ratio)*100)))
		if ratio > 0 && ratio < 0.005 {
			percent = "<1%"
		}
		hint = percent + " · " + fmt.Sprintf(d.t("context.usage.hint"), tokenNumber(usage.Tokens), tokenNumber(usage.Window))
		if d.contextUsage != nil && d.contextUsage.ThresholdTokens > 0 {
			hint += " · " + fmt.Sprintf(d.t("context.threshold.hint"), tokenNumber(d.contextUsage.ThresholdTokens))
		}
	}
	// 2. 用量还没读到时什么都不画，只留出占位，工具栏不跳动；读到后环形进度才出现。
	if !ready {
		ui.Row(c).Size(t.Metrics.ControlHeight, t.Metrics.ControlHeight).Shrink(0)
		return
	}
	// 3. 圆环放进始终可用的方形按钮；悬停时按需读取压缩阈值。
	ring := kit.Button(c, "").Size(t.Metrics.ControlHeight, t.Metrics.ControlHeight).Padding(0).Justify(ui.Center).AlignItems(ui.Center).Label(d.t("context.usage"))
	ring.Children(func() { kit.ProgressRing(c, ratio, t.Space(4)) })
	kit.Tooltip(c, ring, hint)
	if ring.Hovered() && known && d.contextUsage == nil && d.detailsError == nil {
		d.loadContext(false)
	}
	if ring.Clicked() {
		d.openPanel(panelContext)
		d.loadContext(false)
	}
}

// compactContext 请求压缩当前对话的主 Session 上下文，完成后提示结果。
func (d *Desktop) compactContext() {
	target, client := d.readingTarget, d.client
	d.tasks.Go(func() {
		_, err := client.CompactContext(d.ctx, target.ConversationID)
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.showSuccess(d.t("context.compacted"), "")
		})
	})
}

// setSessionModel 保存原目标的模型与推理选择。
func (d *Desktop) setSessionModel(model, effort string) {
	// 1. 空模型由后端采用 Agent 默认。
	var ref defs.ModelRef
	if model != "" {
		ref, _ = defs.ParseModelRef(model)
	}
	selection := defs.ModelSelection{Ref: ref, ReasoningEffort: effort}
	if d.client == nil {
		return
	}
	target, client := d.readingTarget, d.client
	d.tasks.Go(func() {
		_, err := client.SetSessionModel(d.ctx, &runtimeclient.SetSessionModelRequest{ConversationID: target.ConversationID, SessionID: target.SessionID, Model: selection})
		d.window.Update(func() {
			if !d.closed && err != nil {
				d.showFailure(err)
			}
		})
	})
}

// archiveConversation 切换归档标志，不停止运行中的 Turn；归档成功的通知带「撤销」。
func (d *Desktop) archiveConversation(conv defs.Conversation) {
	d.setConversationArchived(conv, conv.ArchivedAt == 0)
}

// setConversationArchived 把对话设为已归档或已恢复，成功后刷新列表并提示。
func (d *Desktop) setConversationArchived(conv defs.Conversation, archived bool) {
	client := d.client
	d.tasks.Go(func() {
		_, err := client.SetConversationArchived(d.ctx, &runtimeclient.SetConversationArchivedRequest{ConversationID: conv.ID, Archived: archived})
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.loadConversations(true)
			if !archived {
				d.showSuccess(d.t("conversation.restored"), conversationTitle(conv))
				return
			}
			d.showToast(kit.ToastOpt{Title: d.t("conversation.archived"), Description: conversationTitle(conv), Kind: kit.ToastSuccess, Action: d.t("undo"), OnAction: func() { d.setConversationArchived(conv, false) }})
		})
	})
}

// forkConversation 建立新对话：beforeTurnID 为空时复制完整已结束历史，否则复制到该 Turn 之前（不含）。
func (d *Desktop) forkConversation(conv defs.Conversation, beforeTurnID defs.TurnID) {
	client := d.client
	d.tasks.Go(func() {
		created, err := client.ForkConversation(d.ctx, &runtimeclient.ForkConversationRequest{ConversationID: conv.ID, BeforeTurnID: beforeTurnID, ExecutorSettings: conv.ExecutorSettings})
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.openConversation(created.ID, created.RootSessionID)
			d.loadConversations(true)
			d.showSuccess(d.t("conversation.forked"), conversationTitle(conv))
		})
	})
}

// buildMarkdownExport 把已加载的 Item 导出为 Markdown：用户和助手消息分节，工具调用为列表项。
func buildMarkdownExport(title, userLabel, assistantLabel string, items []viewItem) string {
	// 1. 标题之后按 Item 顺序输出；同一角色的连续消息共用一个小节标题。
	var b strings.Builder
	b.WriteString("# " + title + "\n")
	section, afterTool := "", false
	for _, view := range items {
		item := view.Item
		switch {
		case isUserMessage(item):
			if section != userLabel {
				b.WriteString("\n## " + userLabel + "\n")
				section = userLabel
			}
			b.WriteString("\n" + contentText(item.Message.Content) + "\n")
			afterTool = false
		case item.Kind == defs.ItemMessage && item.Message != nil && (item.Message.From == nil || item.ModelCallID != ""):
			if section != assistantLabel {
				b.WriteString("\n## " + assistantLabel + "\n")
				section = assistantLabel
			}
			b.WriteString("\n" + contentText(item.Message.Content) + "\n")
			afterTool = false
		case item.ToolCall != nil:
			// 2. 工具调用紧跟在所属小节之后，只保留工具名与参数摘要。
			line := "- `" + item.ToolCall.Name + "`"
			if preview := getToolPreview(item.ToolCall.Name, item.ToolCall.Arguments); preview != "" {
				line += " " + preview
			}
			if section != assistantLabel {
				b.WriteString("\n## " + assistantLabel + "\n")
				section = assistantLabel
			}
			if !afterTool {
				b.WriteString("\n")
			}
			b.WriteString(line + "\n")
			afterTool = true
		}
	}
	return b.String()
}

// buildSidebarRows 把按更新时间倒序的对话插入分组标题，得到侧栏列表行。
func buildSidebarRows(entries []dao.ConversationListEntry, now time.Time) []sidebarRow {
	rows := make([]sidebarRow, 0, len(entries)+4)
	group := ""
	for _, entry := range entries {
		if next := getConversationGroup(entry.Conversation.UpdatedAt, now); next != group {
			group = next
			rows = append(rows, sidebarRow{Key: "group:" + group, Group: group})
		}
		rows = append(rows, sidebarRow{Key: string(entry.Conversation.ID), Conversation: entry.Conversation})
	}
	return rows
}

// getConversationGroup 返回对话按本地日期落入的侧栏分组键：今天、昨天、本周（七天内）、更早。
func getConversationGroup(updatedAt int64, now time.Time) string {
	// 1. 按本地日历日期相减，不按 24 小时窗口。
	updated := time.UnixMilli(updatedAt).In(now.Location())
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	day := time.Date(updated.Year(), updated.Month(), updated.Day(), 0, 0, 0, 0, now.Location())
	days := int(math.Round(today.Sub(day).Hours() / 24))
	switch {
	case days <= 0:
		return "group.today"
	case days == 1:
		return "group.yesterday"
	case days < 7:
		return "group.week"
	default:
		return "group.earlier"
	}
}

// getConversationDirectory 返回对话默认 Executor 的工作目录；没有记录目录时返回空串。
func getConversationDirectory(conv defs.Conversation) string {
	executor := conv.ExecutorSettings.DefaultExecutorID
	if executor == "" {
		executor = "local"
	}
	return conv.ExecutorSettings.WorkspaceDirectories[executor]
}

// getConversationSubtitle 返回侧栏第二行：默认 Executor 的工作目录名与相对时间。
func getConversationSubtitle(conv defs.Conversation, now time.Time, translate func(string) string) string {
	// 1. 没有记录目录时只显示相对时间。
	relative := formatRelativeTime(conv.UpdatedAt, now, translate)
	directory := getConversationDirectory(conv)
	if directory == "" {
		return relative
	}
	return filepath.Base(directory) + " · " + relative
}

// getConversationTooltip 返回侧栏对话行的悬停提示：完整目录；标题被截断时第一行是完整标题；两者都没有时为空串。
func getConversationTooltip(title, directory string, isTitleTruncated bool) string {
	if !isTitleTruncated {
		return directory
	}
	if directory == "" {
		return title
	}
	return title + "\n" + directory
}
