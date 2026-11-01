package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/charmbracelet/x/ansi"
)

// panel 是实时区域里的可交互面板；同一时间只打开一个，打开时占用尾部的位置，输入框的草稿保持不动。
type panel interface {
	// handleKey 处理按键：panelKeyIgnored 时按键交给全局键，panelKeyClosed 时由 tuiModel 关闭面板。
	handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd)
	// handleResult 接收本面板发出的拉取请求的结果；序号过期的结果已由 tuiModel 丢弃。
	handleResult(m *tuiModel, msg panelResultMsg) tea.Cmd
	// render 按宽度渲染含边框的面板，行数不超过 maxHeight。
	render(m *tuiModel, width, maxHeight int) []string
	// getTitle 返回当前面板标题。
	getTitle() string
}

// pastePanel 是接收粘贴的面板：它打开时 tea.PasteMsg 交给它，不进输入框；面板可以不实现。
type pastePanel interface {
	// handlePaste 接收面板打开时的粘贴文字，并返回后续命令。
	handlePaste(m *tuiModel, text string) tea.Cmd
}

const (
	// panelHeightPercent 是面板最多占终端高度的百分比。
	panelHeightPercent = 60
	// scrollToEnd 是 End 键的滚动位置，渲染时限制在内容范围内。
	scrollToEnd = 1 << 30
	// resumeListLimit 是恢复对话面板列出的最近对话条数。
	resumeListLimit = 20
)

// openPanel 替换当前面板，并执行可选的数据查询。
func (m *tuiModel) openPanel(opened panel, fetch func(ctx context.Context) (any, error)) tea.Cmd {
	// 1. 保存新面板并清除旧请求序号；有查询时发出请求。
	m.activePanel, m.panelRequestSeq = opened, 0
	if fetch == nil {
		return nil
	}
	return m.fetchForPanel(fetch)
}

// closePanel 关闭当前面板。
func (m *tuiModel) closePanel() {
	// 1. 清空面板和请求序号，后续结果按过期处理。
	m.activePanel, m.panelRequestSeq = nil, 0
}

// fetchForPanel 为面板发出带序号的数据请求。
func (m *tuiModel) fetchForPanel(fetch func(ctx context.Context) (any, error)) tea.Cmd {
	// 1. 生成并记录请求序号。
	seq := m.nextRequestSeq()
	m.panelRequestSeq = seq
	ctx := m.ctx
	// 2. 执行查询，把序号、返回值和错误作为面板结果发回。
	return func() tea.Msg {
		value, err := fetch(ctx)
		return panelResultMsg{
			seq:   seq,
			value: value,
			err:   err,
		}
	}
}

// handlePanelResult 把有效请求结果交给当前面板。
func (m *tuiModel) handlePanelResult(msg panelResultMsg) tea.Cmd {
	// 1. 面板已关闭或请求序号已过期时忽略，否则交给面板处理。
	if m.activePanel == nil || msg.seq != m.panelRequestSeq {
		return nil
	}
	return m.activePanel.handleResult(m, msg)
}

// getPanelPageSize 返回面板正文翻页使用的行数。
func (m *tuiModel) getPanelPageSize() int {
	// 1. 从面板高度比例扣除两行边框，最少取一行。
	return max(1, m.height*panelHeightPercent/100-2)
}

// ── 面板共用组件 ──

// panelFetch 保存面板查询是否进行中和最近一次的错误；查到的数据由各面板自己保存。
type panelFetch struct {
	isLoading bool
	err       error
}

// start 将面板查询标记为加载中。
func (f *panelFetch) start() {
	// 1. 设置加载标记。
	f.isLoading = true
}

// finish 记录面板查询结果。
func (f *panelFetch) finish(err error) {
	// 1. 清除加载标记，记录本次查询错误。
	f.isLoading, f.err = false, err
}

// formatStatusLine 返回面板查询状态行。
func (f *panelFetch) formatStatusLine(m *tuiModel) string {
	// 1. 加载时显示转动标记，失败时显示错误，否则没有状态行。
	switch {
	case f.isLoading:
		return m.spinner.View() + " 加载中"
	case f.err != nil:
		return m.styles.failure.Render("✗ " + f.err.Error())
	}
	return ""
}

// listCursor 是面板列表的选中项与滚动位置。
type listCursor struct {
	selected int
	offset   int
}

// handleKey 按列表移动键更新选中位置，返回是否处理了这个按键。
func (c *listCursor) handleKey(key string, count, pageSize int) bool {
	// 1. 按箭头、翻页和首尾键更新选中位置，其他键不处理。
	switch key {
	case "up":
		c.selected--
	case "down":
		c.selected++
	case "pgup":
		c.selected -= max(1, pageSize)
	case "pgdown":
		c.selected += max(1, pageSize)
	case "home":
		c.selected = 0
	case "end":
		c.selected = count - 1
	default:
		return false
	}
	// 2. 把选择限制在列表范围内。
	c.clamp(count)
	return true
}

// clamp 把选中位置限制在列表范围内。
func (c *listCursor) clamp(count int) {
	// 1. 将位置限制在零到最后一项之间，空列表取零。
	c.selected = max(0, min(c.selected, count-1))
}

// getVisibleRange 滚动到选中项可见，返回可见项的起止下标。
func (c *listCursor) getVisibleRange(count, height int) (start, end int) {
	// 1. 把选中位置限制在列表内，显示高度至少一行。
	c.clamp(count)
	height = max(1, height)
	// 2. 按选中项调整滚动位置，再限制到列表范围并返回起止下标。
	if c.selected < c.offset {
		c.offset = c.selected
	}
	if c.selected >= c.offset+height {
		c.offset = c.selected - height + 1
	}
	c.offset = max(0, min(c.offset, count-height))
	return c.offset, min(count, c.offset+height)
}

// scrollView 是长文本的纵向滚动位置。
type scrollView struct {
	offset int
}

// handleKey 按滚动键修改偏移，返回是否处理了这个按键。
func (s *scrollView) handleKey(key string, pageSize int) bool {
	// 1. 按箭头、翻页和首尾键修改偏移，其他键不处理。
	switch key {
	case "up":
		s.offset--
	case "down":
		s.offset++
	case "pgup":
		s.offset -= max(1, pageSize)
	case "pgdown":
		s.offset += max(1, pageSize)
	case "home":
		s.offset = 0
	case "end":
		s.offset = scrollToEnd
	default:
		return false
	}
	// 2. 把偏移限制为非负数。
	s.offset = max(0, s.offset)
	return true
}

// getVisible 返回滚动位置起的可见文本行。
func (s *scrollView) getVisible(lines []string, height int) []string {
	// 1. 把高度限制为非负数，再限制偏移并截取可见行。
	height = max(0, height)
	s.offset = max(0, min(s.offset, len(lines)-height))
	return lines[s.offset:min(len(lines), s.offset+height)]
}

// renderListRows 显示可见列表行并高亮选中项。
func renderListRows(m *tuiModel, rows []string, cursor *listCursor, height, width int) []string {
	// 1. 读取可见范围，再分别显示选中行和保留行内样式的普通行。
	start, end := cursor.getVisibleRange(len(rows), height)
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		if index == cursor.selected {
			lines = append(lines, renderSelectedRow(m, rows[index], width))
			continue
		}
		lines = append(lines, "  "+rows[index])
	}
	return lines
}

// renderSelectedRow 生成高亮的选中行。
func renderSelectedRow(m *tuiModel, row string, width int) string {
	// 1. 清除行内样式、添加选中标记并截断，再填满选中底色。
	row = truncateLine("› "+ansi.Strip(row), width)
	return fillBackground(m.styles.selection.Render(row), width, m.styles.selection)
}

// renderPanelFrame 为面板内容添加标题、按键提示和单线边框。
func renderPanelFrame(title string, hints []keyHint, body []string, width, maxHeight int, styles tuiStyles) []string {
	// 1. 高度或宽度不足时只返回截断并铺底色的正文。
	innerWidth := width - 4
	if maxHeight < 3 || innerWidth < 1 {
		var lines []string
		for _, line := range body[:min(len(body), max(0, maxHeight))] {
			lines = append(lines, fillBackground(truncateLine(line, width), width, styles.surface))
		}
		return lines
	}
	// 2. 拼接标题和按键提示，宽度不足时省掉提示。
	top, right := "┌ "+title+" ", " "+formatKeyHints(hints, styles)+" ┐"
	if ansi.StringWidth(top)+ansi.StringWidth(right)+1 > width {
		right = "┐"
	}
	top = ansi.Truncate(top, width-ansi.StringWidth(right)-1, "…")
	lines := []string{fillBackground(top+strings.Repeat("─", width-ansi.StringWidth(top)-ansi.StringWidth(right))+right, width, styles.surface)}
	// 3. 按正文空间截取并补齐内容，最后添加下边框。
	for _, line := range body[:min(len(body), maxHeight-2)] {
		lines = append(lines, fillBackground("│ "+padRight(truncateLine(line, innerWidth), innerWidth)+" │", width, styles.surface))
	}
	return append(lines, fillBackground("└"+strings.Repeat("─", width-2)+"┘", width, styles.surface))
}

// ── 会话信息面板 ──

// statusPanel 保存会话详情、滚动位置和服务地址的 token 显示开关。
type statusPanel struct {
	fetch   panelFetch
	details *conversation.SessionDetails
	scroll  scrollView
	// revealsToken 服务一行是否显示带 token 的直连地址？否则显示脱敏后的服务地址。
	revealsToken bool
}

// getTitle 返回会话信息面板标题。
func (p *statusPanel) getTitle() string {
	// 1. 返回固定标题。
	return "会话信息"
}

// getFetch 返回当前对话根 Session 的详情查询。
func (p *statusPanel) getFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 保存客户端、对话和根 Session ID，生成查询请求。
	client, conversationID, sessionID := m.opt.Client, m.conversation.ID, m.conversation.RootSessionID
	return func(ctx context.Context) (any, error) {
		return client.GetSessionDetails(ctx, conversationID, sessionID)
	}
}

// handleKey 处理会话信息刷新、token 显隐和滚动。
func (p *statusPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Esc 关闭，r 刷新，有直连地址时 t 切换 token 显示，滚动键移动正文。
	switch key := msg.String(); {
	case key == "esc":
		return panelKeyClosed, nil
	case key == "r":
		p.fetch.start()
		return panelKeyHandled, m.fetchForPanel(p.getFetch(m))
	case key == "t" && m.opt.ConnectURL != "":
		p.revealsToken = !p.revealsToken
		return panelKeyHandled, nil
	case p.scroll.handleKey(key, m.getPanelPageSize()):
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// handleResult 保存会话信息查询结果。
func (p *statusPanel) handleResult(_ *tuiModel, msg panelResultMsg) tea.Cmd {
	// 1. 结束加载；成功时替换详情，失败时保留已有数据。
	p.fetch.finish(msg.err)
	if msg.err == nil {
		details := msg.value.(conversation.SessionDetails)
		p.details = &details
	}
	return nil
}

// render 显示会话详情和与服务的连接状态。
func (p *statusPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 写入加载状态，按 token 显示开关选择服务地址，地址后附上来源。
	var lines []string
	if status := p.fetch.formatStatusLine(m); status != "" {
		lines = append(lines, status)
	}
	serverURL := m.opt.ServerURL
	if p.revealsToken {
		serverURL = m.opt.ConnectURL
	}
	server := orDash(serverURL)
	if m.opt.ServerSource != "" {
		server += "（" + m.opt.ServerSource + "）"
	}
	connection := []panelField{
		{
			label: "状态",
			value: formatConnectionMark(m.connection.State, m.styles),
		},
		{
			label: "服务",
			value: server,
		},
	}
	// 2. 会话详情放在连接之前；详情属于当前对话时，用本地最新标题替换查询到的标题。
	sections := []panelSection{{
		title:  "连接",
		fields: connection,
	}}
	if p.details != nil {
		details := *p.details
		if m.conversation != nil && details.ConversationID == m.conversation.ID {
			details.Title = m.conversation.Title
		}
		sections = append(getSessionDetailSections(details, m.styles), sections...)
	}
	// 3. 计算所有字段标签的统一宽度，再逐节生成正文。
	labelWidth := 0
	for _, section := range sections {
		labelWidth = max(labelWidth, getFieldLabelWidth(section.fields))
	}
	for _, section := range sections {
		lines = appendPanelSection(lines, section.title, formatFieldRows(section.fields, labelWidth, width-4, m.styles), m.styles)
	}
	// 4. 生成刷新、token 显隐和滚动提示，截取可见正文并加边框。
	hints := []keyHint{{
		key:    "r",
		action: "刷新",
	}}
	// 没有可直连地址时不给提示，按 t 也没有任何变化。
	if m.opt.ConnectURL != "" {
		action := "显示 token"
		if p.revealsToken {
			action = "隐藏 token"
		}
		hints = append(hints, keyHint{
			key:    "t",
			action: action,
		})
	}
	hints = append(hints, keyHint{
		key:    "↑↓",
		action: "滚动",
	}, keyHint{
		key:    "esc",
		action: "关闭",
	})
	return renderPanelFrame(p.getTitle(), hints, p.scroll.getVisible(lines, maxHeight-2), width, maxHeight, m.styles)
}

// getSessionDetailSections 生成对话、模型和执行环境的详情字段。
func getSessionDetailSections(details conversation.SessionDetails, styles tuiStyles) []panelSection {
	// 1. 读取各 Executor 的说明和目录，追加 Skill 与工具列表。
	var environment []panelField
	for _, executor := range details.Executors {
		value := executor.Description
		if executor.WorkspaceDirectory != "" {
			value = joinNonEmpty([]string{value, styles.faint.Render(executor.WorkspaceDirectory)}, " · ")
		}
		environment = append(environment, panelField{
			label: executor.ID,
			value: orDash(value),
		})
	}
	environment = append(environment,
		panelField{
			label: "Skills",
			value: orDash(strings.Join(details.Skills, ", ")),
		},
		panelField{
			label: "工具",
			value: orDash(strings.Join(details.Tools, ", ")),
		},
	)
	// 2. 组合对话（标题、ID、Session）、模型（主模型、压缩模型）和执行环境三节。
	return []panelSection{
		{
			title: "对话",
			fields: []panelField{
				{
					label: "标题",
					value: getConversationTitle(defs.Conversation{Title: details.Title}),
				},
				{
					label: "ID",
					value: styles.faint.Render(string(details.ConversationID)),
				},
				{
					label: "Session",
					value: styles.faint.Render(string(details.Session.ID)) + " · " + details.Session.AgentName,
				},
			},
		},
		{
			title: "模型",
			fields: []panelField{
				{
					label: "主模型",
					value: formatModelDefinition(details.Model, styles),
				},
				{
					label: "压缩模型",
					value: formatModelDefinition(details.CompactModel, styles),
				},
			},
		},
		{
			title:  "执行环境",
			fields: environment,
		},
	}
}

// formatModelDefinition 生成模型详情文字。
func formatModelDefinition(model *defs.ModelDefinition, styles tuiStyles) string {
	// 1. 空模型显示破折号；其他模型拼接显示名、引用和已知窗口，详情使用暗色。
	if model == nil {
		return "—"
	}
	detail := model.Ref.String()
	if model.ContextWindow > 0 {
		detail += " · 窗口 " + formatTokens(model.ContextWindow)
	}
	return model.DisplayName + "  " + styles.faint.Render(detail)
}

// formatConnectionMark 返回带状态颜色的连接标记和文字。
func formatConnectionMark(state runtimeclient.ConnectionState, styles tuiStyles) string {
	// 1. 选择状态样式并显示圆点和中文状态。
	return getStateStyle(getConnectionDisplayState(state), styles).Render("● " + formatConnectionLabel(state))
}

// getConnectionDisplayState 返回连接状态对应的显示状态。
func getConnectionDisplayState(state runtimeclient.ConnectionState) displayState {
	// 1. 分别返回已连接、重连中或连接中。
	switch state {
	case runtimeclient.ConnectionConnected:
		return stateConnected
	case runtimeclient.ConnectionReconnecting:
		return stateReconnecting
	}
	return stateConnecting
}

// formatConnectionLabel 返回连接状态的中文名称。
func formatConnectionLabel(state runtimeclient.ConnectionState) string {
	// 1. 分别返回已连接、重连中或连接中。
	switch state {
	case runtimeclient.ConnectionConnected:
		return "已连接"
	case runtimeclient.ConnectionReconnecting:
		return "重连中"
	}
	return "连接中"
}

// ── 恢复对话面板 ──

// resumePanel 保存最近或已归档对话列表、过滤词和选择位置。
type resumePanel struct {
	fetch         panelFetch
	conversations []defs.Conversation
	filter        string
	list          listCursor
	// isArchivedView 是否在看已归档的对话？否则看最近的对话。
	isArchivedView bool
}

// getTitle 返回最近或已归档对话列表的标题。
func (p *resumePanel) getTitle() string {
	// 1. 按归档视图标记选择标题。
	if p.isArchivedView {
		return "已归档"
	}
	return "恢复对话"
}

// getFetch 返回查询最近对话或已归档对话列表的请求。
func (p *resumePanel) getFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 生成按更新时间倒序、最多 resumeListLimit 条、指定归档状态的查询；查询成功后取出分页结果里的对话记录。
	client, archived := m.opt.Client, p.isArchivedView
	return func(ctx context.Context) (any, error) {
		page, err := client.ListConversations(ctx, &runtimeclient.ListConversationsRequest{
			Order: storage.Order{
				By:        storage.OrderByUpdatedAt,
				Direction: storage.DirectionDesc,
			},
			Limit:    resumeListLimit,
			Archived: archived,
		})
		if err != nil {
			return nil, err
		}
		conversations := make([]defs.Conversation, len(page.Conversations))
		for index, entry := range page.Conversations {
			conversations[index] = entry.Conversation
		}
		return conversations, nil
	}
}

// getFiltered 返回标题包含过滤词的对话。
func (p *resumePanel) getFiltered() []defs.Conversation {
	// 1. 忽略大小写匹配标题，保留列表原顺序。
	var filtered []defs.Conversation
	for _, listed := range p.conversations {
		if strings.Contains(strings.ToLower(getConversationTitle(listed)), strings.ToLower(p.filter)) {
			filtered = append(filtered, listed)
		}
	}
	return filtered
}

// handleKey 处理对话选择、归档视图切换、刷新和过滤。
func (p *resumePanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 读取过滤结果；Esc 关闭，Enter、Ctrl+A、Ctrl+D 处理选中对话，Tab 在最近和已归档之间切换并重新查询，
	// Ctrl+R 刷新，Backspace 删掉过滤词最后一个字，移动键选择，其他文字追加到过滤词。
	filtered := p.getFiltered()
	switch key := msg.String(); {
	case key == "esc":
		return panelKeyClosed, nil
	case key == "enter" || key == "ctrl+a" || key == "ctrl+d":
		if len(filtered) == 0 {
			return panelKeyHandled, nil
		}
		return panelKeyHandled, p.handleSelectedKey(m, key, filtered[p.list.selected])
	case key == "tab":
		p.isArchivedView, p.conversations, p.list = !p.isArchivedView, nil, listCursor{}
		p.fetch.start()
		return panelKeyHandled, m.fetchForPanel(p.getFetch(m))
	case key == "ctrl+r":
		p.fetch.start()
		return panelKeyHandled, m.fetchForPanel(p.getFetch(m))
	case key == "backspace":
		if p.filter != "" {
			p.filter, p.list = trimLastRune(p.filter), listCursor{}
		}
		return panelKeyHandled, nil
	case p.list.handleKey(key, len(filtered), m.getPanelPageSize()):
		return panelKeyHandled, nil
	case msg.Text != "":
		p.filter, p.list = p.filter+msg.Text, listCursor{}
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// handleSelectedKey 打开、归档或删除选中对话。
func (p *resumePanel) handleSelectedKey(m *tuiModel, key string, selected defs.Conversation) tea.Cmd {
	// 1. 归档或取消归档后刷新列表；删除要求再次确认，当前打开的对话不能删除。
	client := m.opt.Client
	switch key {
	case "ctrl+a":
		request := &runtimeclient.SetConversationArchivedRequest{
			ConversationID: selected.ID,
			Archived:       !p.isArchivedView,
		}
		return p.refreshAfter(m, func(ctx context.Context) error {
			_, err := client.SetConversationArchived(ctx, request)
			return err
		})
	case "ctrl+d":
		if m.conversation != nil && m.conversation.ID == selected.ID {
			return m.showNotice("当前对话不能删除，先 /new 或切到别的对话", true, noticeDuration)
		}
		request := &runtimeclient.DeleteConversationRequest{ConversationID: selected.ID}
		return m.confirmThen("ctrl+d", "再按 Ctrl+D 删除", func() tea.Cmd {
			return p.refreshAfter(m, func(ctx context.Context) error { return client.DeleteConversation(ctx, request) })
		})
	}
	// 2. 新建草稿尚未结束时显示等待提示，否则恢复所选对话。
	if m.isOpeningDraft {
		return m.showNotice("正在新建对话，请稍候", false, noticeDuration)
	}
	return m.resumeConversation(selected)
}

// refreshAfter 执行对话修改并刷新列表。
func (p *resumePanel) refreshAfter(m *tuiModel, change func(ctx context.Context) error) tea.Cmd {
	// 1. 标记加载中，先执行修改再查询列表；任一步失败都返回错误。
	list := p.getFetch(m)
	p.fetch.start()
	return m.fetchForPanel(func(ctx context.Context) (any, error) {
		if err := change(ctx); err != nil {
			return nil, err
		}
		return list(ctx)
	})
}

// handleResult 接收对话列表结果。
func (p *resumePanel) handleResult(_ *tuiModel, msg panelResultMsg) tea.Cmd {
	// 1. 结束加载，成功时替换对话记录，失败时保留原列表。
	p.fetch.finish(msg.err)
	if msg.err == nil {
		p.conversations = msg.value.([]defs.Conversation)
	}
	return nil
}

// render 显示过滤后的对话列表和归档操作提示。
func (p *resumePanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 写入搜索词、加载状态和空列表提示。
	lines := []string{"搜索：" + p.filter + "▏"}
	if status := p.fetch.formatStatusLine(m); status != "" {
		lines = append(lines, status)
	}
	filtered := p.getFiltered()
	if len(filtered) == 0 && !p.fetch.isLoading {
		lines = append(lines, m.styles.faint.Render("没有对话"))
	}
	// 2. 生成对话行和分叉标记，再按高度显示列表。
	rows := make([]string, len(filtered))
	for index, listed := range filtered {
		titleMark := ""
		if listed.ForkedFromID != "" {
			titleMark = " " + m.styles.faint.Render("分叉")
		}
		rows[index] = formatResumeRow("", listed, titleMark, m.now(), width-6)
	}
	lines = append(lines, renderListRows(m, rows, &p.list, maxHeight-2-len(lines), width-4)...)
	// 3. 按最近或归档视图生成操作提示并添加边框。
	hints := []keyHint{{
		key:    "enter",
		action: "打开",
	}, {
		key:    "tab",
		action: "已归档",
	}, {
		key:    "ctrl+a",
		action: "归档",
	}, {
		key:    "ctrl+d",
		action: "删除",
	}, {
		key:    "esc",
		action: "关闭",
	}}
	if p.isArchivedView {
		hints[1].action, hints[2].action = "最近", "取消归档"
	}
	return renderPanelFrame(p.getTitle(), hints, lines, width, maxHeight, m.styles)
}

// ── 回退面板 ──

// rewindPanel 保存可回退的轮次和列表位置。
type rewindPanel struct {
	// turns 是打开面板时由用户消息开头的各轮，新的在前。
	turns []turnOpeningInput
	list  listCursor
}

// getTitle 返回回退面板标题。
func (p *rewindPanel) getTitle() string {
	// 1. 返回固定标题。
	return "回退重来"
}

// handleKey 处理回退选择和分叉请求。
func (p *rewindPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Esc 关闭，移动键选择轮次；Enter 关闭面板，在选中轮次之前分叉并把这轮开头的输入放回输入框。
	switch key := msg.String(); {
	case key == "esc":
		return panelKeyClosed, nil
	case key == "enter":
		if len(p.turns) == 0 {
			return panelKeyHandled, nil
		}
		selected := p.turns[p.list.selected]
		return panelKeyClosed, m.forkConversation(selected.turnID, strings.Join(selected.texts, "\n\n"), "已在这条消息之前分叉，原对话保留")
	case p.list.handleKey(key, len(p.turns), m.getPanelPageSize()):
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// handleResult 忽略回退面板的数据请求结果。
func (p *rewindPanel) handleResult(*tuiModel, panelResultMsg) tea.Cmd {
	// 1. 返回空命令。
	return nil
}

// render 显示可回退轮次的首条输入预览。
func (p *rewindPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 空列表显示提示，其他轮次取首条消息的第一行。
	var lines []string
	if len(p.turns) == 0 {
		lines = append(lines, m.styles.faint.Render("没有可以回退的消息"))
	}
	rows := make([]string, len(p.turns))
	for index, turn := range p.turns {
		rows[index] = getFirstLine(turn.texts[0])
	}
	// 2. 按高度显示列表并添加分叉和关闭提示。
	lines = append(lines, renderListRows(m, rows, &p.list, maxHeight-2-len(lines), width-4)...)
	hints := []keyHint{{
		key:    "enter",
		action: "在这条之前分叉，原文放回输入框",
	}, {
		key:    "esc",
		action: "关闭",
	}}
	return renderPanelFrame(p.getTitle(), hints, lines, width, maxHeight, m.styles)
}

// ── 帮助面板 ──

// helpPanel 是帮助面板（/help、?）：内容由命令表和全局按键生成。
type helpPanel struct {
	scroll scrollView
}

// getTitle 返回帮助面板标题。
func (p *helpPanel) getTitle() string {
	// 1. 返回固定标题。
	return "帮助"
}

// handleKey 处理帮助面板滚动和关闭。
func (p *helpPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Esc 关闭，滚动键更新正文位置，其他键交回全局。
	switch key := msg.String(); {
	case key == "esc":
		return panelKeyClosed, nil
	case p.scroll.handleKey(key, m.getPanelPageSize()):
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// handleResult 忽略帮助面板的数据请求结果。
func (p *helpPanel) handleResult(*tuiModel, panelResultMsg) tea.Cmd {
	// 1. 返回空命令。
	return nil
}

// render 显示当前可见的命令与全局按键帮助。
func (p *helpPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 读取帮助正文的可见部分，加上滚动和关闭提示及边框。
	hints := []keyHint{{
		key:    "↑↓",
		action: "滚动",
	}, {
		key:    "esc",
		action: "关闭",
	}}
	return renderPanelFrame(p.getTitle(), hints, p.scroll.getVisible(m.getHelpLines(), maxHeight-2), width, maxHeight, m.styles)
}
