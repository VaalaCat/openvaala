package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
)

// agentsPanel 保存子 Agent 面板的列表位置；列表按 Session 树缩进，底部显示总用量和选中项的最近结果。
type agentsPanel struct {
	list listCursor
}

// agentResultLineLimit 是底部"最近结果"最多显示的行数。
const agentResultLineLimit = 3

// runAgentsCommand 打开子 Agent 面板。
func (m *tuiModel) runAgentsCommand(string) tea.Cmd {
	// 1. 打开子 Agent 面板，列表直接读本地的 Session 状态。
	return m.openPanel(&agentsPanel{}, nil)
}

// getTitle 返回子 Agent 面板标题。
func (p *agentsPanel) getTitle() string {
	// 1. 返回固定标题。
	return "子 Agent"
}

// handleKey 处理关闭面板和列表选择按键。
func (p *agentsPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 处理 Esc 和列表移动，其他按键交回全局。
	switch key := msg.String(); {
	case key == "esc":
		return panelKeyClosed, nil
	case p.list.handleKey(key, len(m.sessionStatuses.getSubagentRows()), m.getPanelPageSize()):
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// handleResult 忽略子 Agent 面板的数据请求结果。
func (p *agentsPanel) handleResult(*tuiModel, panelResultMsg) tea.Cmd {
	// 1. 返回空命令。
	return nil
}

// render 拼出子 Agent 列表、总用量和最近结果。
func (p *agentsPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 读取子 Session 列表；列表为空时显示提示。
	rows := m.sessionStatuses.getSubagentRows()
	if len(rows) == 0 {
		return renderPanelFrame(p.getTitle(), []keyHint{{
			key:    "esc",
			action: "关闭",
		}}, []string{m.styles.faint.Render("还没有子 Agent")}, width, maxHeight, m.styles)
	}
	// 2. 生成列表行，扣掉合计一行和最近结果的高度后显示列表，再追加合计用量和最近结果。
	texts := make([]string, len(rows))
	for index, row := range rows {
		texts[index] = formatSubagentRow(row, m)
	}
	result := p.getResultLines(m, rows, width-4)
	total := m.sessionStatuses.getTotalUsage()
	listHeight := maxHeight - 2 - 1 - len(result)
	lines := renderListRows(m, texts, &p.list, listHeight, width-4)
	lines = append(lines, m.styles.faint.Render("合计  入 "+formatTokens(total.Input)+" · 出 "+formatTokens(total.Output)))
	lines = append(lines, result...)
	// 3. 加上标题、边框和操作提示。
	return renderPanelFrame(p.getTitle(), []keyHint{{
		key:    "↑↓",
		action: "选择",
	}, {
		key:    "esc",
		action: "关闭",
	}}, lines, width, maxHeight, m.styles)
}

// getResultLines 生成选中子 Agent 的最近结果预览。
func (p *agentsPanel) getResultLines(m *tuiModel, rows []sessionTreeRow, width int) []string {
	// 1. 选中项是根 Session 的直接子 Agent 时读取它的最近结果并清理控制字符，没有结果时显示“—”。
	row := rows[min(p.list.selected, len(rows)-1)]
	text := ""
	if row.depth == 0 {
		text = sanitizeToolText(m.transcript.getSubagentResult(row.info.Session.ID))
	}
	if strings.TrimSpace(text) == "" {
		return []string{"最近结果：—"}
	}
	// 2. 正文最多取 agentResultLineLimit 行，每行按宽度截断。
	lines := []string{"最近结果："}
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if len(lines) > agentResultLineLimit {
			break
		}
		lines = append(lines, "  "+truncateLine(line, width-2))
	}
	return lines
}

// formatSubagentRow 返回按 Session 深度缩进的子 Agent 信息行。
func formatSubagentRow(row sessionTreeRow, m *tuiModel) string {
	// 1. 正在运行时显示已运行时长，否则留空。
	info := row.info
	elapsed := ""
	if since, found := m.sessionStatuses.getRunningSince(info.Session.ID); found {
		elapsed = formatElapsed(m.now().Sub(since))
	}
	// 2. 读取模型和上下文占比；运行中优先使用本轮模型。
	model := info.Session.Model
	if isSessionRunning(info) && info.ActiveTurnModel != (defs.ModelSelection{}) {
		model = info.ActiveTurnModel
	}
	context := "ctx —"
	if percent, isKnown := getContextUsagePercent(info.Context); isKnown {
		context = fmt.Sprintf("ctx %d%%", percent)
	}
	// 3. 拼接名称、状态、耗时、模型、上下文和用量。
	name := strings.Repeat("  ", row.depth) + m.sessionStatuses.getSessionName(info.Session.ID)
	usage := "入 " + formatTokens(info.Usage.Input) + " · 出 " + formatTokens(info.Usage.Output)
	return joinNonEmpty([]string{name, formatSessionState(info, m.styles), elapsed, m.formatModelSelection(model), context, usage}, "  ")
}

// formatSessionState 返回带状态颜色的 Session 状态文字。
func formatSessionState(info runtime.SessionRuntimeInfo, styles tuiStyles) string {
	// 1. 先检查运行和停止状态，再读取最近 Turn 的结束状态。
	text, state := "空闲", stateIdle
	switch {
	case isSessionRunning(info):
		text, state = "运行中", stateRunning
	case info.State == session.SessionStateStopping || info.State == session.SessionStateStopped:
		text, state = "已停止", stateStopped
	case info.LastTurnStatus == defs.TurnStatusCompleted:
		text, state = "完成", stateCompleted
	case info.LastTurnStatus == defs.TurnStatusFailed:
		text, state = "失败", stateFailed
	case info.LastTurnStatus == defs.TurnStatusInterrupted:
		text, state = "已中断", stateInterrupted
	}
	// 2. 使用对应状态的样式渲染文字。
	return getStateStyle(state, styles).Render(text)
}
