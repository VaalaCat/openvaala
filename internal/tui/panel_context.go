package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
)

// contextPanel 保存上下文用量、所在页、排序方式和滚动位置；包含汇总页和逐条页。
type contextPanel struct {
	fetch panelFetch
	usage *runtime.SessionContextUsage
	// page 是所在页：0 是汇总页，1 是逐条页。
	page int
	// isSortedByTokens 逐条页是否按 token 降序？否则按时间顺序。
	isSortedByTokens bool
	// summary 是汇总页的滚动位置。
	summary scrollView
	// items 是逐条页的列表位置。
	items listCursor
}

// contextCategoryLabels 是 Item 类别的中文名，也是汇总页的显示顺序。
var contextCategoryLabels = []struct {
	category agent_context.ContextCategory
	label    string
}{
	{
		agent_context.ContextCategorySummary,
		"摘要",
	},
	{
		agent_context.ContextCategoryUserInput,
		"用户输入",
	},
	{
		agent_context.ContextCategoryInstruction,
		"文件指令",
	},
	{
		agent_context.ContextCategoryAgentMessage,
		"Agent 消息",
	},
	{
		agent_context.ContextCategoryAssistantText,
		"回复",
	},
	{
		agent_context.ContextCategoryReasoning,
		"推理",
	},
	{
		agent_context.ContextCategoryToolCall,
		"工具调用",
	},
	{
		agent_context.ContextCategoryToolResult,
		"工具结果",
	},
	{
		agent_context.ContextCategoryModelError,
		"模型错误",
	},
	{
		agent_context.ContextCategoryModelChange,
		"模型切换",
	},
	{
		agent_context.ContextCategoryExecutorsChange,
		"Executor",
	},
	{
		agent_context.ContextCategoryTurnInterrupted,
		"中断",
	},
}

const (
	// contextBarWidth 是占比条的格数。
	contextBarWidth = 20
	// contextPreviewByteLimit 是逐条页预览读取的正文前缀字节数。
	contextPreviewByteLimit = 512
	// contextUnmeasuredHint 是还没有测量（没有 Item）时两页显示的提示。
	contextUnmeasuredHint = "还没有测量，发出第一次请求后显示"
)

// runContextCommand 打开上下文详情面板并读取根 Session 用量。
func (m *tuiModel) runContextCommand(string) tea.Cmd {
	// 1. 创建默认按 token 排序的面板，标记加载中并发出查询。
	opened := &contextPanel{isSortedByTokens: true}
	opened.fetch.start()
	return m.openPanel(opened, opened.getFetch(m))
}

// getTitle 返回上下文详情面板标题。
func (p *contextPanel) getTitle() string {
	// 1. 返回固定标题。
	return "上下文详情"
}

// getFetch 返回当前对话根 Session 的上下文用量查询。
func (p *contextPanel) getFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 保存客户端、对话和根 Session，生成查询请求。
	client, conversationID, sessionID := m.opt.Client, m.conversation.ID, m.conversation.RootSessionID
	return func(ctx context.Context) (any, error) {
		return client.GetContextUsage(ctx, conversationID, sessionID)
	}
}

// handleKey 处理上下文面板的切页、排序、刷新和滚动。
func (p *contextPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Esc 关闭，Tab 或 Shift+Tab 切页，r 重新查询，逐条页按 s 切换排序并回到第一条；其余按所在页滚动汇总或移动列表。
	key := msg.String()
	switch {
	case key == "esc":
		return panelKeyClosed, nil
	case key == "tab" || key == "shift+tab":
		p.page = 1 - p.page
	case key == "r":
		p.fetch.start()
		return panelKeyHandled, m.fetchForPanel(p.getFetch(m))
	case key == "s" && p.page == 1:
		p.isSortedByTokens, p.items = !p.isSortedByTokens, listCursor{}
	case p.page == 0 && p.summary.handleKey(key, m.getPanelPageSize()):
	case p.page == 1 && p.usage != nil && p.items.handleKey(key, len(p.usage.Items), m.getPanelPageSize()):
	default:
		return panelKeyIgnored, nil
	}
	return panelKeyHandled, nil
}

// handleResult 接收上下文用量。
func (p *contextPanel) handleResult(_ *tuiModel, msg panelResultMsg) tea.Cmd {
	// 1. 结束加载，成功时替换用量记录，失败时保留已有数据。
	p.fetch.finish(msg.err)
	if msg.err == nil {
		usage := msg.value.(runtime.SessionContextUsage)
		p.usage = &usage
	}
	return nil
}

// render 显示上下文汇总页或逐条用量页。
func (p *contextPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 写入加载状态；逐条页显示还没测量的提示或 Item 列表，加上按键提示和边框后返回。
	var lines []string
	if status := p.fetch.formatStatusLine(m); status != "" {
		lines = append(lines, status)
	}
	if p.page == 1 {
		switch {
		case p.usage != nil && len(p.usage.Items) == 0:
			lines = append(lines, m.styles.faint.Render(contextUnmeasuredHint))
		case p.usage != nil:
			lines = append(lines, "  "+m.styles.faint.Render(padRight("类别", 10)+padLeft("估算", 7)+"  预览"))
			lines = append(lines, renderListRows(m, p.getItemRows(m), &p.items, maxHeight-2-len(lines), width-4)...)
		}
		hints := []keyHint{{
			key:    "tab",
			action: "切页",
		}, {
			key:    "s",
			action: "排序",
		}, {
			key:    "r",
			action: "刷新",
		}, {
			key:    "↑↓",
			action: "滚动",
		}, {
			key:    "esc",
			action: "关闭",
		}}
		return renderPanelFrame("上下文详情 · 逐条", hints, lines, width, maxHeight, m.styles)
	}
	// 2. 汇总页生成占用、最近请求和类别统计，再按滚动位置显示。
	if p.usage != nil {
		lines = append(lines, formatContextSummary(*p.usage, width-4, m.styles)...)
	}
	hints := []keyHint{{
		key:    "tab",
		action: "切页",
	}, {
		key:    "r",
		action: "刷新",
	}, {
		key:    "↑↓",
		action: "滚动",
	}, {
		key:    "esc",
		action: "关闭",
	}}
	return renderPanelFrame(p.getTitle(), hints, p.summary.getVisible(lines, maxHeight-2), width, maxHeight, m.styles)
}

// getItemRows 按当前排序返回 Item 类别、估算 token 和预览。
func (p *contextPanel) getItemRows(m *tuiModel) []string {
	// 1. 复制 Item 用量，按设置选择 token 降序或保留时间顺序。
	items := slices.Clone(p.usage.Items)
	if p.isSortedByTokens {
		slices.SortStableFunc(items, func(a, b agent_context.ItemTokens) int { return cmp.Compare(b.Tokens, a.Tokens) })
	}
	// 2. 读取已保存 Item 的预览，拼接类别和估算用量。
	rows := make([]string, len(items))
	for index, item := range items {
		preview := getItemPreview(m.transcript.getSavedItem(item.ItemID), m.sessionStatuses, m.isLocalServer)
		rows[index] = m.styles.faint.Render(padRight(getContextCategoryLabel(item.Category), 10)) + padLeft("≈"+formatTokens(item.Tokens), 7) + "  " + orDash(preview)
	}
	return rows
}

// formatContextSummary 生成上下文占用、最近请求和类别统计三部分内容。
func formatContextSummary(usage runtime.SessionContextUsage, width int, styles tuiStyles) []string {
	// 1. 读取压缩阈值和请求字段，计算共同的标签宽度。
	threshold := "—"
	if usage.ThresholdTokens > 0 {
		threshold = formatTokens(usage.ThresholdTokens)
	}
	occupancyFields := []panelField{
		{
			label: "下一次请求",
			value: formatNextRequest(usage.NextRequest),
		},
		{
			label: "压缩阈值",
			value: threshold,
		},
	}
	lastRequestFields := getLastRequestFields(usage.LastRequest, styles)
	labelWidth := getFieldLabelWidth(occupancyFields, lastRequestFields)

	// 2. 窗口已知时显示占用条，否则显示窗口未知；再写入占用字段和最近请求，没有最近请求时显示破折号。
	occupancy := []string{"  " + styles.faint.Render("窗口未知")}
	if usage.Window > 0 {
		occupancy = []string{"  " + formatContextBar(usage, styles)}
	}
	occupancy = append(occupancy, formatFieldRows(occupancyFields, labelWidth, width, styles)...)
	lines := appendPanelSection(nil, "占用", occupancy, styles)
	lastRequest := formatFieldRows(lastRequestFields, labelWidth, width, styles)
	if len(lastRequest) == 0 {
		lastRequest = []string{"  " + styles.faint.Render("—")}
	}
	lines = appendPanelSection(lines, "最近请求", lastRequest, styles)
	// 3. 没有 Item 时显示尚未测量提示，否则添加类别统计。
	if len(usage.Items) == 0 {
		return append(lines, "", styles.faint.Render(contextUnmeasuredHint))
	}
	return appendPanelSection(lines, "按类别（估算）", formatCategoryRows(usage, styles), styles)
}

// formatContextBar 按已知窗口生成下一次请求的占用条和压缩阈值标记。
func formatContextBar(usage runtime.SessionContextUsage, styles tuiStyles) string {
	// 1. 按窗口计算填充格数，并在压缩阈值位置写入竖线。
	const width = contextBarWidth * 3 / 2
	tokens, isKnown := usage.NextRequest.Tokens, usage.NextRequest.Source != runtime.ContextUsageSourceUnknown
	filled := 0
	if isKnown {
		filled = min(width, (tokens*width+usage.Window/2)/usage.Window)
	}
	cells := []rune(strings.Repeat("█", filled) + strings.Repeat("░", width-filled))
	if usage.ThresholdTokens > 0 {
		cells[min(width-1, usage.ThresholdTokens*width/usage.Window)] = '│'
	}
	// 2. 达到压缩阈值时使用警告色，否则使用强调色，未填充部分用暗色。
	fillStyle := styles.accent
	if isKnown && usage.ThresholdTokens > 0 && tokens >= usage.ThresholdTokens {
		fillStyle = styles.warning
	}
	var bar strings.Builder
	if filled > 0 {
		bar.WriteString(fillStyle.Render(string(cells[:filled])))
	}
	if filled < width {
		bar.WriteString(styles.faint.Render(string(cells[filled:])))
	}
	// 3. 请求用量未知时显示破折号，否则追加 token、窗口和占比。
	if !isKnown {
		return bar.String() + "  — / " + formatTokens(usage.Window)
	}
	// NextRequest 与 usage 使用相同窗口，按下一次请求读取百分比。
	percent, _ := getContextUsagePercent(usage.NextRequest)
	return bar.String() + fmt.Sprintf("  %s / %s  %d%%", formatTokens(tokens), formatTokens(usage.Window), percent)
}

// getLastRequestFields 返回最近请求的实际用量、缓存用量和估算偏差字段。
func getLastRequestFields(last *runtime.LastRequestUsage, styles tuiStyles) []panelField {
	// 1. 没有最近请求时没有字段。
	if last == nil {
		return nil
	}
	// 2. 组合实际输入、输出、缓存读写和相对本次估算的偏差。
	pair := func(left int, label string, right int) string {
		return padRight(formatTokens(left), 12) + styles.faint.Render(label) + "  " + formatTokens(right)
	}
	return []panelField{
		{
			label: "实际输入",
			value: pair(last.Usage.Input, "缓存读", last.Usage.CacheRead),
		},
		{
			label: "输出",
			value: pair(last.Usage.Output, "缓存写", last.Usage.CacheWrite),
		},
		{
			label: "估算偏差",
			value: formatDeviation(last.Usage.Input - last.EstimatedTokens),
		},
	}
}

// formatDeviation 返回带正负号的估算偏差和增减说明。
func formatDeviation(deviation int) string {
	// 1. 按偏差正负选择说明，再附上带符号的 token 数。
	explanation := "与估算一致"
	switch {
	case deviation > 0:
		explanation = "实际输入比它自己的估算多"
	case deviation < 0:
		explanation = "实际输入比它自己的估算少"
	}
	return formatSignedTokens(deviation) + "（" + explanation + "）"
}

// formatNextRequest 返回下一次请求的用量及其数据来源。
func formatNextRequest(next runtime.ContextUsage) string {
	// 1. 实际基准显示数值，估算加 ≈，未知显示破折号。
	switch next.Source {
	case runtime.ContextUsageSourceActual:
		return formatTokens(next.Tokens) + "（实际基准 + 估算）"
	case runtime.ContextUsageSourceEstimated:
		return "≈" + formatTokens(next.Tokens) + "（估算）"
	}
	return "—（未知）"
}

// formatCategoryRows 按类别汇总估算用量并生成占比条。
func formatCategoryRows(usage runtime.SessionContextUsage, styles tuiStyles) []string {
	// 1. 先记录 system 和工具定义，再按类别顺序汇总 Item 用量。
	type categoryTotal struct {
		label  string
		tokens int
	}
	totals := []categoryTotal{{
		"system",
		usage.SystemTokens,
	}, {
		"工具定义",
		usage.ToolTokens,
	}}
	sum := usage.SystemTokens + usage.ToolTokens
	for _, entry := range contextCategoryLabels {
		tokens := 0
		for _, item := range usage.Items {
			if item.Category == entry.category {
				tokens += item.Tokens
			}
		}
		totals = append(totals, categoryTotal{
			entry.label,
			tokens,
		})
		sum += tokens
	}
	// 2. 跳过零用量，按总量生成填充条、token 和百分比。
	var rows []string
	for _, total := range totals {
		if total.tokens == 0 || sum == 0 {
			continue
		}
		filled := (total.tokens*contextBarWidth + sum/2) / sum
		var bar strings.Builder
		if filled > 0 {
			bar.WriteString(styles.accent.Render(strings.Repeat("█", filled)))
		}
		if filled < contextBarWidth {
			bar.WriteString(styles.faint.Render(strings.Repeat("░", contextBarWidth-filled)))
		}
		percent := fmt.Sprintf("%d%%", (total.tokens*100+sum/2)/sum)
		rows = append(rows, "  "+padRight(total.label, 10)+padLeft("≈"+formatTokens(total.tokens), 7)+"  "+bar.String()+"  "+styles.faint.Render(padLeft(percent, 4)))
	}
	return rows
}

// formatSignedTokens 返回带正负号的 token 数。
func formatSignedTokens(tokens int) string {
	// 1. 负值使用减号，其余使用加号。
	if tokens < 0 {
		return "−" + formatTokens(-tokens)
	}
	return "+" + formatTokens(tokens)
}

// getContextCategoryLabel 返回类别显示名称。
func getContextCategoryLabel(category agent_context.ContextCategory) string {
	// 1. 依次匹配类别标签，未匹配时使用原类别值。
	for _, entry := range contextCategoryLabels {
		if entry.category == category {
			return entry.label
		}
	}
	return string(category)
}

// getItemPreview 按 Item 类型生成单行预览。
func getItemPreview(item *defs.Item, statuses *sessionStatuses, isLocalServer bool) string {
	// 1. 找不到 Item 时没有预览；准备截取正文前缀、清理终端控制并取首行的方法。
	if item == nil {
		return ""
	}
	firstLine := func(text string) string {
		return getFirstLine(sanitizeToolText(getTextPrefix(text, contextPreviewByteLimit)))
	}
	// 2. 按消息、推理、工具、摘要、模型错误和各类状态变化分别生成预览。
	switch item.Kind {
	case defs.ItemMessage:
		return firstLine(getMessageText(item.Message))
	case defs.ItemReasoning:
		return firstLine(getReasoningText(item.Reasoning.Summary, item.Reasoning.Content))
	case defs.ItemToolCall:
		return firstLine(item.ToolCall.Name + " " + getToolMainArgument(*item.ToolCall, statuses))
	case defs.ItemToolResult:
		if item.ToolResult.Error != nil {
			return "✗ " + firstLine(item.ToolResult.Error.Message)
		}
		return firstLine(formatToolResultContent(item.ToolResult.Content))
	case defs.ItemContextSummary:
		return firstLine(item.ContextSummary.Text)
	case defs.ItemModelError:
		return firstLine(item.ModelError.Message)
	case defs.ItemModelChange:
		return "切换到 " + item.ModelChange.Model.String()
	case defs.ItemExecutorsChange:
		return formatExecutorsChange(item.ExecutorsChange, isLocalServer)
	case defs.ItemTurnInterrupted:
		return "已中断"
	}
	return ""
}
