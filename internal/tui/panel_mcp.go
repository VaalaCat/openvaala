package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/charmbracelet/x/ansi"
)

// mcpPanel 保存当前对话的 MCP 连接、展开的工具列表和光标位置；连接在打开面板时读取。
type mcpPanel struct {
	fetch panelFetch
	// conversationID 是查询的对话；还没有对话（草稿）时为空，面板不发请求。
	conversationID defs.ConversationID
	// servers 是拉取到的连接。
	servers []conversation.MCPServerInfo
	// cursor 是光标所在的一行；零值是第一个 server 的 server 行。
	cursor mcpCursorStop
	// expandedServers 是展开了工具列表的 server 在 servers 里的下标。
	expandedServers map[int]bool
	// scroll 是正文的滚动位置，渲染时跟着光标所在行调整。
	scroll scrollView
}

// mcpCursorStop 是光标能停的一行：一个 server 行，或展开了的 server 的一个工具行。
type mcpCursorStop struct {
	// serverIndex 是 server 在 servers 里的下标。
	serverIndex int
	// isTool 是否是工具行？否则是 server 行。
	isTool bool
	// toolIndex 是工具行在该 server 工具列表里的下标；server 行为 0。
	toolIndex int
}

const (
	// mcpServerIDWidth 是 server 名一列的最大显示宽度：超宽时截断加 "…"。
	mcpServerIDWidth = 16
	// mcpReasonMaxLines 是连接失败原因最多显示的行数，超出时末行以“…”结尾。
	mcpReasonMaxLines = 3
)

// runMCPCommand 打开 MCP 面板。
func (m *tuiModel) runMCPCommand(string) tea.Cmd {
	// 1. 初始化展开记录；草稿直接打开面板，不发请求。
	opened := &mcpPanel{expandedServers: make(map[int]bool)}
	if m.conversation == nil {
		return m.openPanel(opened, nil)
	}
	// 2. 记录对话，标记加载中并查询连接。
	opened.conversationID = m.conversation.ID
	opened.fetch.start()
	return m.openPanel(opened, opened.getFetch(m))
}

// getTitle 返回当前对话的 MCP 面板标题。
func (p *mcpPanel) getTitle() string {
	// 1. 返回固定标题。
	return "MCP（当前对话）"
}

// getFetch 返回当前对话的 MCP 连接查询。
func (p *mcpPanel) getFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 保存客户端和对话 ID，生成查询请求。
	client, conversationID := m.opt.Client, p.conversationID
	return func(ctx context.Context) (any, error) {
		return client.ListMCPServers(ctx, conversationID)
	}
}

// handleKey 处理 MCP 服务和工具行的移动、展开及收起。
func (p *mcpPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 读取可选行并定位光标。
	stops := p.getCursorStops()
	moved := listCursor{selected: slices.Index(stops, p.cursor)}
	// 2. Esc 关闭；Enter 展开或收起所在服务的工具列表，光标回到服务行；移动键只在服务行和工具行之间移动。
	switch key := msg.String(); {
	case key == "esc":
		return panelKeyClosed, nil
	case key == "enter":
		if len(p.servers) > 0 {
			p.expandedServers[p.cursor.serverIndex] = !p.expandedServers[p.cursor.serverIndex]
			p.cursor = mcpCursorStop{serverIndex: p.cursor.serverIndex}
		}
		return panelKeyHandled, nil
	case moved.handleKey(key, len(stops), m.getPanelPageSize()):
		if len(stops) > 0 {
			p.cursor = stops[moved.selected]
		}
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// getCursorStops 返回依次可选的 MCP 服务行和已展开的工具行。
func (p *mcpPanel) getCursorStops() []mcpCursorStop {
	// 1. 按服务顺序添加服务行及展开的工具行。
	var stops []mcpCursorStop
	for serverIndex, server := range p.servers {
		stops = append(stops, mcpCursorStop{serverIndex: serverIndex})
		if !p.expandedServers[serverIndex] {
			continue
		}
		for toolIndex := range server.Tools {
			stops = append(stops, mcpCursorStop{
				serverIndex: serverIndex,
				isTool:      true,
				toolIndex:   toolIndex,
			})
		}
	}
	return stops
}

// handleResult 保存 MCP 列表加载结果。
func (p *mcpPanel) handleResult(_ *tuiModel, msg panelResultMsg) tea.Cmd {
	// 1. 结束加载；成功时替换连接列表，失败时保留原数据。
	p.fetch.finish(msg.err)
	if msg.err == nil {
		p.servers = msg.value.([]conversation.MCPServerInfo)
	}
	return nil
}

// render 显示 MCP 连接、工具列表和配置提示。
func (p *mcpPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 写入加载状态、草稿提示或空列表提示。
	var lines []string
	if status := p.fetch.formatStatusLine(m); status != "" {
		lines = append(lines, status)
	}
	switch {
	case p.conversationID == "":
		lines = append(lines, m.styles.faint.Render("对话开始后才有 MCP 连接"))
	case len(p.servers) == 0 && !p.fetch.isLoading && p.fetch.err == nil:
		lines = append(lines, m.styles.faint.Render("没有 MCP 连接"))
	}
	// 2. 生成全部内容行，留出底部提示一行后调整滚动位置：先让选中范围的末尾可见，放不下时优先显示它的开头。
	rows, selectedStart, selectedEnd := p.formatRows(m, width-4)
	height := maxHeight - 2 - len(lines) - 1
	if selectedEnd > p.scroll.offset+height {
		p.scroll.offset = selectedEnd - height
	}
	p.scroll.offset = min(p.scroll.offset, selectedStart)
	// 3. 截取可见内容，追加固定配置提示和操作键。
	lines = append(lines, p.scroll.getVisible(rows, height)...)
	lines = append(lines, m.styles.faint.Render("改 mcp_servers/*.yaml 后 /reload"))
	hints := []keyHint{{
		key:    "↑↓",
		action: "选择",
	}, {
		key:    "enter",
		action: "展开/收起",
	}, {
		key:    "esc",
		action: "关闭",
	}}
	return renderPanelFrame(p.getTitle(), hints, lines, width, maxHeight, m.styles)
}

// formatRows 返回按 Executor 分组的 MCP 内容行，以及选中项要显示出来的行范围。
func (p *mcpPanel) formatRows(m *tuiModel, width int) (rows []string, selectedStart, selectedEnd int) {
	// 1. 计算服务名列宽，准备选中行高亮。
	idWidth := 0
	for _, server := range p.servers {
		idWidth = max(idWidth, min(ansi.StringWidth(sanitizeToolText(server.ServerID)), mcpServerIDWidth))
	}
	// formatCursorRow 返回一行在列表里的样子：光标所在行高亮，其他行缩进两格对齐。
	formatCursorRow := func(stop mcpCursorStop, row string) string {
		if stop == p.cursor {
			return renderSelectedRow(m, row, width)
		}
		return "  " + row
	}
	// 2. 按服务顺序添加 Executor 标题、服务行和错误原因；选中服务时范围从该服务开头（分组首项含 Executor 标题）到原因行结束。
	for serverIndex, server := range p.servers {
		serverStart := len(rows)
		if serverIndex == 0 || server.ExecutorID != p.servers[serverIndex-1].ExecutorID {
			rows = append(rows, m.styles.heading.Render(sanitizeToolText(server.ExecutorID)))
		}
		serverStop := mcpCursorStop{serverIndex: serverIndex}
		rows = append(rows, formatCursorRow(serverStop, formatMCPServerRow(server, idWidth, m.styles)))
		if server.Error != "" {
			rows = append(rows, formatMCPReasonRows(server.Error, width-4)...)
		}
		if p.cursor == serverStop {
			selectedStart, selectedEnd = serverStart, len(rows)
		}
		// 3. 已展开的服务追加工具行；空工具列表显示提示，工具选中范围只占一行。
		if !p.expandedServers[serverIndex] {
			continue
		}
		if len(server.Tools) == 0 {
			rows = append(rows, "    "+m.styles.faint.Render("没有工具"))
			continue
		}
		for toolIndex, toolRow := range formatMCPToolRows(server.Tools, width-4, m.styles) {
			toolStop := mcpCursorStop{
				serverIndex: serverIndex,
				isTool:      true,
				toolIndex:   toolIndex,
			}
			if p.cursor == toolStop {
				selectedStart, selectedEnd = len(rows), len(rows)+1
			}
			rows = append(rows, formatCursorRow(toolStop, toolRow))
		}
	}
	return rows, selectedStart, selectedEnd
}

// formatMCPServerRow 返回带状态标记、工具数量和必需标记的 MCP 服务行。
func formatMCPServerRow(server conversation.MCPServerInfo, idWidth int, styles tuiStyles) string {
	// 1. 读取连接状态对应的符号、文字和颜色。
	mark, label, state := "◌", "连接中", stateConnecting
	switch server.State {
	case mcpclient.ConnectionStateConnected:
		mark, label, state = "●", "已连接", stateConnected
	case mcpclient.ConnectionStateFailed:
		mark, label, state = "✗", "连接失败", stateFailed
	}
	// 2. 已连接时显示工具数，必需服务追加标记，再对齐服务名。
	style := getStateStyle(state, styles)
	toolCount := ""
	if server.State == mcpclient.ConnectionStateConnected {
		toolCount = styles.faint.Render(fmt.Sprintf("%d 个工具", len(server.Tools)))
	}
	required := ""
	if server.Required {
		required = "必需"
	}
	id := ansi.Truncate(sanitizeToolText(server.ServerID), mcpServerIDWidth, "…")
	return style.Render(mark) + " " + padRight(id, idWidth) + "  " + joinNonEmpty([]string{style.Render(label), toolCount, required}, "  ")
}

// formatMCPReasonRows 把连接错误折行为缩进的原因行。
func formatMCPReasonRows(reason string, width int) []string {
	// 1. 清理控制字符、合并空白并折行，超出上限时截断并加省略号。
	lines := wrapText(flattenMCPText(reason), width)
	if len(lines) > mcpReasonMaxLines {
		lines = lines[:mcpReasonMaxLines]
		lines[mcpReasonMaxLines-1] = truncateLine(lines[mcpReasonMaxLines-1]+"…", width)
	}
	// 2. 给每行添加四格缩进。
	for index, line := range lines {
		lines[index] = "    " + line
	}
	return lines
}

// formatMCPToolRows 返回名称对齐、描述用暗色的工具行。
func formatMCPToolRows(tools []mcpclient.ConnectionTool, width int, styles tuiStyles) []string {
	// 1. 清理工具原名并计算名称列宽。
	names := make([]string, len(tools))
	nameWidth := 0
	for index, tool := range tools {
		names[index] = flattenMCPText(tool.OriginalName)
		nameWidth = max(nameWidth, ansi.StringWidth(names[index]))
	}
	// 2. 有描述时在对齐后的名称后接暗色描述，整行截断到可用宽度并缩进两格。
	rows := make([]string, len(tools))
	for index, tool := range tools {
		row := names[index]
		if description := flattenMCPText(tool.Description); description != "" {
			row = padRight(row, nameWidth) + "  " + styles.faint.Render(description)
		}
		rows[index] = "  " + truncateLine(row, width)
	}
	return rows
}

// flattenMCPText 清理 MCP 文本的控制字符并合并空白。
func flattenMCPText(text string) string {
	// 1. 清理终端控制，把空白和换行连接成单个空格。
	return strings.Join(strings.Fields(sanitizeToolText(text)), " ")
}
