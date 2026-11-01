package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// expandView 保存全屏历史视图的滚动位置和渲染缓存；由 tuiModel 顺序使用，只读取 transcript 历史。
type expandView struct {
	// topLine 是可见区域第一行在全部内容中的行号。
	topLine int
	// isFollowing 是否停在底部？停在底部时新内容到达继续贴底。
	isFollowing bool
	// contentLineCount 是最近一次渲染的内容行数，按键按它限制滚动范围。
	contentLineCount int
	// renderedTranscript 是缓存对应的 transcript，换了对话时清空缓存。
	renderedTranscript *transcript
	// renderedWidth 是缓存对应的显示宽度，宽度变化时清空缓存。
	renderedWidth int
	// renderedEntries 按历史单元下标缓存已结束单元的渲染结果。
	renderedEntries map[int][]string
}

// expandPositionFormat 是底部提示行的内容位置说明。
const expandPositionFormat = "展开视图 · %d–%d / %d 行"

// expandKeyHints 是展开视图提示行里的按键提示。
var expandKeyHints = []keyHint{{
	key:    "↑↓",
	action: "滚动",
}, {
	key:    "PgUp/PgDn",
	action: "翻页",
}, {
	key:    "g/G",
	action: "顶/底",
}, {
	key:    "Esc",
	action: "返回",
}}

// newExpandView 创建停在内容底部的展开视图。
func newExpandView() *expandView {
	// 1. 开启贴底并初始化已完成历史的渲染缓存。
	return &expandView{
		isFollowing:     true,
		renderedEntries: make(map[int][]string),
	}
}

// runExpandCommand 关闭面板并打开展开视图。
func (m *tuiModel) runExpandCommand(string) tea.Cmd {
	// 1. 关闭现有面板，创建停在底部的展开视图。
	m.closePanel()
	m.expandView = newExpandView()
	return nil
}

// closeExpandView 关闭展开视图。
func (m *tuiModel) closeExpandView() tea.Cmd {
	// 1. 清除展开视图并标记正在离开，延时发出 expandViewLeftMsg；在这之前定稿行继续暂存。
	m.expandView = nil
	m.isLeavingExpandView = true
	return tea.Tick(expandViewLeaveDelay, func(time.Time) tea.Msg { return expandViewLeftMsg{} })
}

// handleExpandViewLeft 在终端退出全屏后清除离开标记，放行暂存的定稿行；用户已要求退出时结束程序。
func (m *tuiModel) handleExpandViewLeft() tea.Cmd {
	// 1. 清除离开标记；用户已要求退出时返回退出命令。
	m.isLeavingExpandView = false
	if m.isQuitting {
		return tea.Quit
	}
	return nil
}

// getExpandedLines 返回对话标题和完整展开历史。
func (m *tuiModel) getExpandedLines() []string {
	// 1. 已打开对话时生成对话标题，再交给展开视图生成内容行。
	header := ""
	if m.conversation != nil {
		header = m.formatConversationHeader(*m.conversation)
	}
	return m.expandView.getContentLines(m.transcript, header)
}

// getContentLines 生成展开历史的内容行：标题在前，各历史单元按顺序渲染，已结束的单元缓存渲染结果。
func (v *expandView) getContentLines(t *transcript, header string) []string {
	// 1. 对话或显示宽度改变时清空缓存。
	if t != v.renderedTranscript || t.renderer.width != v.renderedWidth {
		v.renderedTranscript, v.renderedWidth = t, t.renderer.width
		clear(v.renderedEntries)
	}
	// 2. 添加对话标题；历史为空时显示提示。
	var lines []string
	if header != "" {
		lines = append(lines, header)
	}
	if len(t.history) == 0 {
		return append(lines, t.styles.faint.Render("还没有内容"))
	}
	// 3. 按历史顺序读取缓存或重新渲染；已结束的单元存入缓存，非空单元之间空一行。
	for index, entry := range t.history {
		entryLines, cached := v.renderedEntries[index]
		if !cached {
			entryLines = t.formatExpandedEntry(entry)
			if t.isHistoryEntryFinished(entry) {
				v.renderedEntries[index] = entryLines
			}
		}
		if len(entryLines) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, entryLines...)
	}
	return lines
}

// render 返回一屏内容和底部位置、按键提示。
func (v *expandView) render(content []string, width, height int, styles tuiStyles) []string {
	// 1. 记录内容行数，贴底时把顶部移到最后一屏，再限制在滚动范围内。
	bodyHeight := max(1, height-1)
	v.contentLineCount = len(content)
	maxTop := max(0, len(content)-bodyHeight)
	if v.isFollowing {
		v.topLine = maxTop
	}
	v.topLine = max(0, min(v.topLine, maxTop))
	// 2. 截取可见行并补空行，再追加按宽度截断的位置和按键提示。
	visible := content[v.topLine:min(len(content), v.topLine+bodyHeight)]
	first := v.topLine + 1
	if len(visible) == 0 {
		first = 0
	}
	lines := append(slices.Clone(visible), make([]string, bodyHeight-len(visible))...)
	position := fmt.Sprintf(expandPositionFormat, first, v.topLine+len(visible), len(content))
	return append(lines, truncateLine(styles.faint.Render(position+" · ")+formatKeyHints(expandKeyHints, styles), width))
}

// handleKey 处理展开视图里的按键，滚动键更新位置；返回是否要关闭视图。
func (v *expandView) handleKey(key string, height int) (isClosed bool) {
	// 1. 按一屏高度和内容行数算出最大滚动位置。
	bodyHeight := max(1, height-1)
	maxTop := max(0, v.contentLineCount-bodyHeight)
	// 2. Esc、q、Ctrl+O、Ctrl+C 关闭视图；滚动键按行、按屏或到顶到底移动；其他按键忽略。
	switch key {
	case "esc", "q", "ctrl+o", "ctrl+c":
		return true
	case "up", "k":
		v.topLine--
	case "down", "j":
		v.topLine++
	case "pgup", "b":
		v.topLine -= bodyHeight
	case "pgdown", "space":
		v.topLine += bodyHeight
	case "g", "home":
		v.topLine = 0
	case "G", "end":
		v.topLine = maxTop
	default:
		return false
	}
	// 3. 把位置限制在滚动范围内；停在底部时继续贴底，按 g/Home 到顶时不贴底，即使当时内容不足一屏。
	v.topLine = max(0, min(v.topLine, maxTop))
	v.isFollowing = v.topLine == maxTop && key != "g" && key != "home"
	return false
}

// ── transcript 的展开渲染 ──

// isHistoryEntryFinished 历史单元是否已结束且不再变化？
func (t *transcript) isHistoryEntryFinished(entry historyEntry) bool {
	// 1. 回复已不在 replies 里（已保存或被丢弃）时算结束，步骤组整组打印后算结束；其他单元写进历史后就不再变化。
	switch entry.kind {
	case historyReply:
		return t.replies[entry.replyID] != entry.reply
	case historyStepGroup:
		return entry.group.isPrinted
	}
	return true
}

// formatExpandedEntry 按历史单元类型生成展开视图里的行。
func (t *transcript) formatExpandedEntry(entry historyEntry) []string {
	// 1. 用户输入整行铺底色；回复渲染 Markdown 并给首行加回复标记；步骤组逐步展开；
	// 指令先写来源（名称 · Executor · 路径）再渲染正文；hook 运行逐项展开；Turn 分隔线画满一行。
	switch entry.kind {
	case historyUserInput:
		lines := t.formatUserInput(entry.userInput)
		for index, line := range lines {
			lines[index] = fillBackground(line, t.renderer.width, t.styles.surface)
		}
		return lines
	case historyReply:
		// 被丢弃且从没写出过段落的回复不显示；写出过的照常显示正文，"未保存"提示是后面单独的一条历史。
		if entry.reply.isDiscarded && !entry.reply.hasPrinted {
			return nil
		}
		lines := t.renderer.render(entry.reply.source.String())
		if len(lines) > 0 {
			lines[0] = t.markReplyStart(lines[0])
		}
		return lines
	case historyStepGroup:
		return t.formatExpandedGroup(entry.group)
	case historyInstruction:
		name := "AGENTS.md"
		var executorID, path, text string
		if instruction := entry.instruction.SkillInstruction; instruction != nil {
			name, executorID, path, text = "Skill: "+instruction.Name, instruction.Executor, instruction.Path, instruction.Text
		} else {
			instruction := entry.instruction.AgentsInstruction
			executorID, path, text = instruction.Executor, instruction.Path, instruction.Text
		}
		header := formatStyledTexts([]styledText{{
			style: t.styles.faint,
			text:  name + " · " + executorID + " · " + path,
		}})
		return append(wrapTextKeepingStyles(header, t.renderer.width), t.renderer.render(text)...)
	case historyHookRun:
		return t.formatExpandedHookRun(entry.hookRun, entry.hookToolName)
	case historyTurnDivider:
		return []string{t.styles.faint.Render(strings.Repeat("─", t.renderer.width))}
	}
	// 2. 其余提示按当前宽度折行。
	return wrapTextKeepingStyles(formatStyledTexts(entry.line), t.renderer.width)
}

// formatExpandedHookRun 展开显示 hook 的事件、处理命令和各项输出。
func (t *transcript) formatExpandedHookRun(run *defs.HookRun, toolName string) []string {
	// 1. 失败的运行用警告色，其余用暗色；标题依次拼上事件、工具名、状态、Executor 和处理命令。
	style := t.styles.faint
	if run.Status == defs.HookRunFailed {
		style = t.styles.warning
	}
	title := "hook " + string(run.Event)
	if toolName != "" {
		title += " " + toolName
	}
	parts := []string{title, formatHookRunStatus(run.Status)}
	for _, part := range []string{run.ExecutorID, run.Handler} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	// 2. 按宽度折行标题，再清理各项非空输出并以两格缩进显示。
	lines := wrapTextKeepingStyles(formatStyledTexts([]styledText{{
		style: style,
		text:  strings.Join(parts, " · "),
	}}), t.renderer.width)
	for _, output := range []struct{ label, text string }{
		{
			"拦下原因",
			run.BlockReason,
		}, {
			"更新后的工具参数",
			run.UpdatedToolArguments,
		}, {
			"附加上下文",
			run.AdditionalContext,
		},
		{
			"结束原因",
			run.StopReason,
		}, {
			"系统消息",
			run.SystemMessage,
		}, {
			"错误",
			run.ErrorMessage,
		},
	} {
		if output.text == "" {
			continue
		}
		for _, paragraph := range strings.Split(output.label+"："+sanitizeToolText(output.text), "\n") {
			for _, row := range wrapText(paragraph, t.renderer.width-2) {
				if row == "" {
					lines = append(lines, "")
					continue
				}
				lines = append(lines, style.Render("  "+row))
			}
		}
	}
	return lines
}

// formatExpandedGroup 按组内原顺序展开推理和工具调用。
func (t *transcript) formatExpandedGroup(group *stepGroup) []string {
	// 1. 依次显示推理全文或工具标题，工具正文的非空行缩进两格。
	width := t.renderer.width
	var lines []string
	for _, step := range group.steps {
		if step.reasoning != nil {
			lines = append(lines, t.formatExpandedReasoning(step.reasoning, width)...)
			continue
		}
		lines = append(lines, truncateLine(formatToolCallRow(step.call, t.statuses, t.styles, t.getDefaultExecutorID()), width))
		for _, row := range formatToolCallBody(step.call, width-2, t.styles) {
			if row != "" {
				row = "  " + row
			}
			lines = append(lines, row)
		}
	}
	return lines
}

// formatExpandedReasoning 显示推理全文。
func (t *transcript) formatExpandedReasoning(reasoning *groupReasoning, width int) []string {
	// 1. 没有推理文字时显示加密推理提示。
	text := reasoning.getText()
	if text == "" {
		return []string{t.styles.faint.Render("∴ （加密推理）")}
	}
	// 2. 清理终端控制并按宽度折行，首行加 ∴，续行缩进两格。
	var lines []string
	for paragraph := range strings.SplitSeq(sanitizeToolText(text), "\n") {
		for _, row := range wrapText(paragraph, width-2) {
			lead := "  "
			if len(lines) == 0 {
				lead = "∴ "
			}
			lines = append(lines, t.styles.faint.Render(lead+row))
		}
	}
	return lines
}

// ── 工具调用正文 ──

// formatToolCallRow 返回工具状态、名称、Executor、主参数和耗时组成的标题行。
func formatToolCallRow(call *toolCall, statuses *sessionStatuses, styles tuiStyles, defaultExecutorID string) string {
	// 1. 按工具调用状态选择标记和颜色。
	marks := map[toolCallState]styledText{
		toolCallRunning: {
			style: getStateStyle(stateRunning, styles),
			text:  "◐",
		},
		toolCallSucceeded: {
			style: getStateStyle(stateCompleted, styles),
			text:  "✓",
		},
		toolCallFailed: {
			style: getStateStyle(stateFailed, styles),
			text:  "✗",
		},
		toolCallNotExecuted: {
			style: getStateStyle(stateNotExecuted, styles),
			text:  "⊘",
		},
	}
	// 2. 拼接工具标签和主参数；调用和结果都有保存时间时追加耗时。
	mark := marks[call.getState()]
	parts := []string{mark.style.Render(mark.text) + " " + formatToolLabel(call, defaultExecutorID), getToolMainArgument(call.call, statuses)}
	if call.callSavedAt > 0 && call.resultSavedAt >= call.callSavedAt {
		parts = append(parts, formatToolDuration(time.Duration(call.resultSavedAt-call.callSavedAt)*time.Millisecond))
	}
	return joinNonEmpty(parts, "  ")
}

// formatToolCallBody 显示工具参数、补丁差异、输出和错误。
func formatToolCallBody(call *toolCall, width int, styles tuiStyles) []string {
	// 1. 准备按宽度逐行折行并分别套样式的文本写入方法。
	var rows []string
	// add 把每段文本按换行拆开、按宽度折行，再给折出的每一行单独套 render 样式（nil 表示不套）。
	add := func(render func(...string) string, texts ...string) {
		for _, text := range texts {
			for line := range strings.SplitSeq(text, "\n") {
				rows = append(rows, wrapStyledRows(line, width, render)...)
			}
		}
	}
	// 2. 清理参数并显示；apply_patch 额外显示补丁差异。
	add(nil, "参数", sanitizeToolText(formatJSON(call.call.Arguments)))
	if call.call.Name == "apply_patch" {
		add(nil, "", "改动")
		rows = append(rows, formatPatchDiff(sanitizeToolText(getStringArgument(call.call.Arguments, "patch")), width, styles)...)
	}
	// 3. 结果存在时显示清理后的输出、截断提示及错误。
	if result := call.result; result != nil {
		if output := sanitizeToolText(formatJSON(formatToolResultContent(result.Content))); output != "" {
			add(nil, "", "输出", output)
		}
		if result.Truncated {
			add(styles.faint.Render, "（输出已截断）")
		}
		if result.Error != nil {
			add(nil, "")
			add(styles.failure.Render, "错误")
			add(nil, sanitizeToolText(joinNonEmpty([]string{result.Error.Code, result.Error.Message}, "：")))
		}
	}
	return rows
}

// formatPatchDiff 把补丁显示为文件标题和着色的增删行。
func formatPatchDiff(patch string, width int, styles tuiStyles) []string {
	// 1. 准备新增、修改、删除标题和逐行填色方法。
	headers := []struct{ prefix, label string }{
		{
			"*** Add File: ",
			"新增 ",
		}, {
			"*** Update File: ",
			"修改 ",
		}, {
			"*** Delete File: ",
			"删除 ",
		},
	}
	var rows []string
	// fill 把一行增删内容折行，每一行用 style 套色并铺满 width 列。
	fill := func(line string, style lipgloss.Style) {
		for _, row := range wrapStyledRows(line, width, nil) {
			rows = append(rows, fillBackground(style.Render(row), width, style))
		}
	}
	// 2. 跳过补丁起止标记，按标题、增行、删行和分段标记分别折行渲染。
	for line := range strings.SplitSeq(strings.TrimRight(patch, "\n"), "\n") {
		switch {
		case line == "*** Begin Patch" || line == "*** End Patch":
			continue
		case strings.HasPrefix(line, "*** "):
			for _, header := range headers {
				if path, found := strings.CutPrefix(line, header.prefix); found {
					rows = append(rows, wrapStyledRows(header.label+path, width, nil)...)
				}
			}
		case strings.HasPrefix(line, "+"):
			fill(line, styles.diffAdded)
		case strings.HasPrefix(line, "-"):
			fill(line, styles.diffRemoved)
		case strings.HasPrefix(line, "@@"):
			rows = append(rows, wrapStyledRows(line, width, styles.faint.Render)...)
		default:
			rows = append(rows, wrapStyledRows(line, width, nil)...)
		}
	}
	return rows
}
