package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/charmbracelet/x/ansi"
)

// getState 返回工具调用的运行、成功、失败或未执行状态。
func (c *toolCall) getState() toolCallState {
	// 1. 已有结果时检查工具错误和命令失败；没有结果时按未执行标记区分状态。
	switch {
	case c.result != nil && c.result.Error != nil:
		return toolCallFailed
	case c.result != nil:
		if command, isCommandResult := tools.ParseCommandResult(c.result.Content); isCommandResult && command.IsFailed() {
			return toolCallFailed
		}
		return toolCallSucceeded
	case c.isNotExecuted:
		return toolCallNotExecuted
	}
	return toolCallRunning
}

// getCalls 按出现顺序返回组内工具调用。
func (g *stepGroup) getCalls() []*toolCall {
	// 1. 遍历步骤并收集工具调用。
	var calls []*toolCall
	for _, step := range g.steps {
		if step.call != nil {
			calls = append(calls, step.call)
		}
	}
	return calls
}

// getReasoningCount 返回推理段数。
func (g *stepGroup) getReasoningCount() int {
	// 1. 遍历步骤并统计推理记录，包含没有文字的加密推理。
	count := 0
	for _, step := range g.steps {
		if step.reasoning != nil {
			count++
		}
	}
	return count
}

// getGeneratingReasoning 返回组内仍在生成的推理。
func (g *stepGroup) getGeneratingReasoning() *groupReasoning {
	// 1. 按顺序查找生成中的推理，没有时返回 nil。
	for _, step := range g.steps {
		if step.reasoning != nil && step.reasoning.isGenerating {
			return step.reasoning
		}
	}
	return nil
}

// isComplete 组内是否每个工具调用都有结果？
func (g *stepGroup) isComplete() bool {
	// 1. 检查工具调用中是否还有缺少结果的项。
	return !slices.ContainsFunc(g.getCalls(), func(call *toolCall) bool { return call.result == nil })
}

// markNotExecuted 把组内还没有结果的工具调用标记为未执行。
func (g *stepGroup) markNotExecuted() {
	// 1. 遍历工具调用，给没有结果的设置未执行标记。
	for _, call := range g.getCalls() {
		if call.result == nil {
			call.isNotExecuted = true
		}
	}
}

// countState 返回指定状态的工具调用数。
func (g *stepGroup) countState(state toolCallState) int {
	// 1. 遍历调用并统计状态匹配的项。
	count := 0
	for _, call := range g.getCalls() {
		if call.getState() == state {
			count++
		}
	}
	return count
}

// getSavedDuration 返回组内最早到最晚步骤的保存时长。
func (g *stepGroup) getSavedDuration() (duration time.Duration, found bool) {
	// 1. 收集推理和工具调用的最早开始、最晚结束保存时间。
	var first, last int64
	for _, step := range g.steps {
		start, end := int64(0), int64(0)
		if step.reasoning != nil {
			start, end = step.reasoning.savedAt, step.reasoning.savedAt
		} else {
			start, end = step.call.callSavedAt, step.call.resultSavedAt
		}
		if start > 0 && (first == 0 || start < first) {
			first = start
		}
		last = max(last, end)
	}
	// 2. 时间缺失或先后不符时返回未知，否则换算为时长。
	if first == 0 || last < first {
		return 0, false
	}
	return time.Duration(last-first) * time.Millisecond, true
}

// getText 返回推理摘要或正文。
func (r *groupReasoning) getText() string {
	// 1. 摘要拼接后非空就用摘要，否则用正文。
	return getReasoningText(r.summary, r.content)
}

// setReasoning 更新推理内容和生成状态。
func (r *groupReasoning) setReasoning(item *defs.Item) {
	// 1. 复制摘要与正文，状态为空时算生成中，保存时间取较大值。
	r.summary = slices.Clone(item.Reasoning.Summary)
	r.content = slices.Clone(item.Reasoning.Content)
	r.isGenerating = item.Reasoning.Status == ""
	r.savedAt = max(r.savedAt, item.UpdatedAt)
}

// ── transcript 的思考与工具组 ──

// loadOrOpenStepGroup 读取同一 Turn 的打开组，或关闭旧组并登记新组。
func (t *transcript) loadOrOpenStepGroup(turnID defs.TurnID, startedAt time.Time) (*stepGroup, []string) {
	// 1. 当前打开组属于该 Turn 时直接返回。
	if t.openStepGroup != nil && t.openStepGroup.turnID == turnID {
		return t.openStepGroup, nil
	}
	// 2. 关闭旧组并生成 Turn 分隔线，新建步骤组并登记历史。
	lines := append(t.closeStepGroup(), t.formatTurnDivider(turnID)...)
	t.openStepGroup = &stepGroup{
		turnID:    turnID,
		startedAt: startedAt,
	}
	t.stepGroups = append(t.stepGroups, t.openStepGroup)
	t.history = append(t.history, historyEntry{
		kind:  historyStepGroup,
		group: t.openStepGroup,
	})
	return t.openStepGroup, lines
}

// addToolCall 按 CallID 更新工具调用或加入新步骤。
func (t *transcript) addToolCall(item *defs.Item, startedAt time.Time) []string {
	// 1. 复制调用并清理工具名中的终端控制；已有 CallID 时更新参数和保存时间。
	displayed := *item.ToolCall
	displayed.Name = stripTerminalControls(displayed.Name)
	if call, found := t.toolCalls[displayed.CallID]; found {
		call.call = displayed
		call.callSavedAt = max(call.callSavedAt, item.UpdatedAt)
		return nil
	}
	// 2. 读取或打开步骤组，追加新调用并登记 CallID 对应组。
	group, lines := t.loadOrOpenStepGroup(item.TurnID, startedAt)
	call := &toolCall{
		call:        displayed,
		callSavedAt: item.UpdatedAt,
	}
	group.steps = append(group.steps, &groupStep{call: call})
	t.toolCalls[call.call.CallID] = call
	t.callGroups[call.call.CallID] = group
	return lines
}

// addReasoning 按 Item ID 更新推理或加入新步骤。
func (t *transcript) addReasoning(item *defs.Item, startedAt time.Time) []string {
	// 1. 已有推理时替换内容和状态。
	if reasoning, found := t.reasonings[item.ID]; found {
		reasoning.setReasoning(item)
		return nil
	}
	// 2. 读取或打开步骤组，追加推理并登记 Item ID 对应组。
	group, lines := t.loadOrOpenStepGroup(item.TurnID, startedAt)
	reasoning := &groupReasoning{itemID: item.ID}
	reasoning.setReasoning(item)
	group.steps = append(group.steps, &groupStep{reasoning: reasoning})
	t.reasonings[item.ID] = reasoning
	t.reasoningGroups[item.ID] = group
	return lines
}

// appendReasoningDelta 追加已登记推理的摘要和正文增量。
func (t *transcript) appendReasoningDelta(itemID defs.ItemID, delta *agent.AgentItemDelta) {
	// 1. 查找推理，未登记时忽略；已登记时依次追加片段。
	reasoning, found := t.reasonings[itemID]
	if !found {
		return
	}
	reasoning.summary = append(reasoning.summary, delta.ReasoningSummary...)
	reasoning.content = append(reasoning.content, delta.ReasoningContent...)
}

// removeReasoning 删除中断后不保存的推理步骤。
func (t *transcript) removeReasoning(itemID defs.ItemID) {
	// 1. 查找推理所在组，未登记时直接返回。
	group, found := t.reasoningGroups[itemID]
	if !found {
		return
	}
	// 2. 从步骤列表和推理索引中移除该记录。
	group.steps = slices.DeleteFunc(group.steps, func(step *groupStep) bool {
		return step.reasoning != nil && step.reasoning.itemID == itemID
	})
	delete(t.reasonings, itemID)
	delete(t.reasoningGroups, itemID)
}

// setToolResult 按 CallID 关联工具结果并尝试打印所属组。
func (t *transcript) setToolResult(item *defs.Item) []string {
	// 1. 查找调用，未登记时忽略。
	call, found := t.toolCalls[item.ToolResult.CallID]
	if !found {
		return nil
	}
	// 2. 保存结果和最新保存时间，再尝试打印所属组。
	call.result = item.ToolResult
	call.resultSavedAt = max(call.resultSavedAt, item.UpdatedAt)
	return t.flushStepGroup(t.callGroups[call.call.CallID])
}

// closeStepGroup 关闭当前步骤组并尝试打印。
func (t *transcript) closeStepGroup() []string {
	// 1. 没有打开组时直接返回；否则清除打开记录，再尝试打印这一组。
	group := t.openStepGroup
	if group == nil {
		return nil
	}
	t.openStepGroup = nil
	return t.flushStepGroup(group)
}

// closeStepGroupOfOtherTurn 打开组属于别的 Turn 时关闭它。
func (t *transcript) closeStepGroupOfOtherTurn(turnID defs.TurnID) []string {
	// 1. Turn 非空且不同于打开组时关闭，否则返回空。
	if t.openStepGroup == nil || turnID == "" || turnID == t.openStepGroup.turnID {
		return nil
	}
	return t.closeStepGroup()
}

// closeStepGroupForReply 回复第一次出现非空文字时，关闭它之前的步骤组。
func (t *transcript) closeStepGroupForReply(reply *replyStream, text string) []string {
	// 1. 已有正文或本次只有空白时保持组打开，否则标记正文已出现并关闭组。
	if reply.hasText || strings.TrimSpace(text) == "" {
		return nil
	}
	reply.hasText = true
	return t.closeStepGroup()
}

// flushStepGroup 步骤组可以打印时生成它的汇总行，并标记已打印。
func (t *transcript) flushStepGroup(group *stepGroup) []string {
	// 1. 已打印、仍打开，或属于活动 Turn 且还有调用没结果时不打印。
	if group.isPrinted || group == t.openStepGroup || (group.turnID == t.activeTurnID && !group.isComplete()) {
		return nil
	}
	// 2. 标记缺少结果的调用并记录已打印；纯推理组显示推理行，其他组显示工具汇总。
	group.markNotExecuted()
	group.isPrinted = true
	if len(group.getCalls()) == 0 {
		return t.formatReasoningLines(group)
	}
	return formatBlock(formatStepGroupSummary(group, t.renderer.width, t.styles, t.statuses, t.getDefaultExecutorID()))
}

// formatReasoningLines 为纯推理组生成逐段的暗色推理行。
func (t *transcript) formatReasoningLines(group *stepGroup) []string {
	// 1. 跳过无文字推理，把其余推理作为独立显示块。
	var lines []string
	for _, step := range group.steps {
		if text := step.reasoning.getText(); text != "" {
			lines = append(lines, t.formatLine(t.styles.faint, "∴ "+text)...)
		}
	}
	return lines
}

// flushEndedStepGroups 关闭并打印不属于活动 Turn 的步骤组。
func (t *transcript) flushEndedStepGroups() []string {
	// 1. 先关闭其他 Turn 的打开组，再逐组打印已经结束 Turn 的内容。
	var lines []string
	if t.openStepGroup != nil && t.openStepGroup.turnID != t.activeTurnID {
		lines = t.closeStepGroup()
	}
	for _, group := range t.stepGroups {
		if group.turnID != t.activeTurnID {
			lines = append(lines, t.flushStepGroup(group)...)
		}
	}
	return lines
}

// handleToolProgress 按 CallID 更新工具最新一行输出。
func (t *transcript) handleToolProgress(event *runtimeclient.ToolProgress) {
	// 1. 查找对应调用，只在进度包含非空文本时替换已有输出。
	call, found := t.toolCalls[event.Progress.CallID]
	if !found {
		return
	}
	if line := getProgressLine(event.Progress.Content); line != "" {
		call.progress = line
	}
}

// ── 思考与工具组渲染 ──

// formatStepCounts 生成思考和工具次数说明。
func formatStepCounts(group *stepGroup) string {
	// 1. 分别读取推理段数和工具调用数，只连接非零计数的说明。
	var parts []string
	if count := group.getReasoningCount(); count > 0 {
		parts = append(parts, fmt.Sprintf("思考 %d 次", count))
	}
	if calls := group.getCalls(); len(calls) > 0 {
		parts = append(parts, fmt.Sprintf("工具 %d 次", len(calls)))
	}
	return strings.Join(parts, " · ")
}

// formatStepGroupHeadline 返回步骤组的完成标记、计数和耗时汇总。
func formatStepGroupHeadline(group *stepGroup, width int, styles tuiStyles) string {
	// 1. 统计失败和未执行调用，选择成功或失败标记。
	failed, notExecuted := group.countState(toolCallFailed), group.countState(toolCallNotExecuted)
	mark, markState := "✓", stateCompleted
	if failed+notExecuted > 0 {
		mark, markState = "✗", stateFailed
	}
	// 2. 组合思考工具次数、失败计数和已知耗时，按宽度截断。
	text := styles.faint.Render(formatStepCounts(group)) + formatFailureCounts(failed, notExecuted, styles)
	if duration, found := group.getSavedDuration(); found {
		text += styles.faint.Render(" · " + formatToolDuration(duration))
	}
	return getStateStyle(markState, styles).Render(mark) + " " + truncateLine(text, width-2)
}

// formatStepGroupSummary 生成步骤组汇总及工具树。
func formatStepGroupSummary(group *stepGroup, width int, styles tuiStyles, statuses *sessionStatuses, defaultExecutorID string) []string {
	// 1. 以汇总行为开头，按顺序追加工具树。
	return appendTreeLines([]string{formatStepGroupHeadline(group, width, styles)}, getToolNodes(group, "", width, styles, statuses, defaultExecutorID), "  ", width, styles)
}

// formatRunningStepGroup 生成运行中的步骤组汇总、工具树和实时推理。
func formatRunningStepGroup(group *stepGroup, now time.Time, spinner string, width int, styles tuiStyles, statuses *sessionStatuses, defaultExecutorID string) []string {
	// 1. 生成思考、工具次数和运行耗时，再读取工具节点。
	text := formatStepCounts(group) + " · 运行中"
	if !group.startedAt.IsZero() {
		text += " " + formatElapsed(now.Sub(group.startedAt))
	}
	header := spinner + " " + styles.faint.Render(text)
	nodes := getToolNodes(group, spinner, width, styles, statuses, defaultExecutorID)
	// 2. 生成中的推理追加到树尾，超宽时保留文字末尾，再按宽度绘制工具树。
	if reasoning := group.getGeneratingReasoning(); reasoning != nil {
		const label = "思考中："
		available := width - 5 - ansi.StringWidth(spinner+" "+label)
		nodes = append(nodes, treeNode{lines: []string{spinner + " " + styles.faint.Render(label+getTextTail(reasoning.getText(), available))}})
	}
	return appendTreeLines([]string{truncateLine(header, width)}, nodes, "  ", width, styles)
}

// formatFailureCounts 生成失败和中断计数文字。
func formatFailureCounts(failed, notExecuted int, styles tuiStyles) string {
	// 1. 只写入非零计数，失败用失败色，中断用警告色；都为零时返回空，否则用暗色的括号和分隔符连起来。
	var parts []string
	if failed > 0 {
		parts = append(parts, getStateStyle(stateFailed, styles).Render(fmt.Sprintf("失败 %d", failed)))
	}
	if notExecuted > 0 {
		parts = append(parts, getStateStyle(stateInterrupted, styles).Render(fmt.Sprintf("中断 %d", notExecuted)))
	}
	if len(parts) == 0 {
		return ""
	}
	return styles.faint.Render("（") + strings.Join(parts, styles.faint.Render(" · ")) + styles.faint.Render("）")
}

// formatToolDuration 生成工具耗时文字。
func formatToolDuration(duration time.Duration) string {
	// 1. 不足一分钟保留一位小数，否则使用累计耗时格式。
	if duration < time.Minute {
		return fmt.Sprintf("%.1fs", duration.Seconds())
	}
	return formatElapsed(duration)
}

// treeNode 是汇总树里的一个节点。
type treeNode struct {
	// lines 是节点自身的文字：第一行接在树线后，其余是它的续行。
	lines []string
	// children 是下一层节点。
	children []treeNode
}

// appendTreeLines 按节点顺序绘制树线并追加显示行。
func appendTreeLines(lines []string, nodes []treeNode, prefix string, width int, styles tuiStyles) []string {
	// 1. 为末节点选择结束树线，其余节点保留续线；按宽度截断正文并递归添加下层节点。
	for index, node := range nodes {
		branch, continuation := "├─ ", "│  "
		if index == len(nodes)-1 {
			branch, continuation = "└─ ", "   "
		}
		for lineIndex, text := range node.lines {
			lead := prefix + branch
			if lineIndex > 0 {
				lead = prefix + continuation
			}
			lines = append(lines, styles.faint.Render(lead)+truncateLine(text, width-ansi.StringWidth(lead)))
		}
		lines = appendTreeLines(lines, node.children, prefix+continuation, width, styles)
	}
	return lines
}

// getToolNodes 按工具与 Executor 分组生成调用树节点。
func getToolNodes(group *stepGroup, spinner string, width int, styles tuiStyles, statuses *sessionStatuses, defaultExecutorID string) []treeNode {
	// 1. 按首次出现顺序收集工具标签（工具名加 @Executor），再按调用次数降序稳定排列。
	calls := group.getCalls()
	var labels []string
	callsByLabel := make(map[string][]*toolCall)
	for _, call := range calls {
		label := formatToolLabel(call, defaultExecutorID)
		if len(callsByLabel[label]) == 0 {
			labels = append(labels, label)
		}
		callsByLabel[label] = append(callsByLabel[label], call)
	}
	slices.SortStableFunc(labels, func(a, b string) int { return cmp.Compare(len(callsByLabel[b]), len(callsByLabel[a])) })
	// 2. 组还在运行（传了 spinner）时选出全组最早的两个运行中调用。
	shownRunning := make(map[*toolCall]bool)
	if spinner != "" {
		running := slices.DeleteFunc(slices.Clone(calls), func(call *toolCall) bool { return call.getState() != toolCallRunning })
		for _, call := range running[:min(2, len(running))] {
			shownRunning[call] = true
		}
	}
	// 3. 为每个标签添加失败项、改动文件和选中的运行中调用。
	nodes := make([]treeNode, len(labels))
	for index, label := range labels {
		named := callsByLabel[label]
		node := treeNode{lines: []string{styles.faint.Render(fmt.Sprintf("%s %d 次", label, len(named)))}}
		node.children = append(node.children, getFailedCallNodes(named, styles, statuses)...)
		if rows := formatChangedFileRows(getChangedFileEntries(named), width-8); len(rows) > 0 {
			for rowIndex, row := range rows {
				rows[rowIndex] = styles.faint.Render(row)
			}
			node.children = append(node.children, treeNode{lines: rows})
		}
		for _, call := range named {
			if shownRunning[call] {
				node.children = append(node.children, treeNode{lines: []string{formatRunningCallText(call, spinner, styles, statuses)}})
			}
		}
		nodes[index] = node
	}
	return nodes
}

// getFailedCallNodes 生成失败调用树节点。
func getFailedCallNodes(calls []*toolCall, styles tuiStyles, statuses *sessionStatuses) []treeNode {
	// 1. 筛选失败调用，最多生成三个明细节点，读取错误或命令状态首行并加上主参数。
	failed := slices.DeleteFunc(slices.Clone(calls), func(call *toolCall) bool { return call.getState() != toolCallFailed })
	var nodes []treeNode
	for _, call := range failed[:min(3, len(failed))] {
		// 命令失败时调用本身没有错误，用命令的状态行 "exit N" 或 "failed: 原因"。
		message := formatToolResultContent(call.result.Content)
		if call.result.Error != nil {
			message = call.result.Error.Message
		}
		text := getFirstLine(stripTerminalControls(message))
		if argument := getToolMainArgument(call.call, statuses); argument != "" {
			text = argument + "：" + text
		}
		nodes = append(nodes, treeNode{lines: []string{getStateStyle(stateFailed, styles).Render("✗") + " " + styles.faint.Render(text)}})
	}
	// 2. 超过三项时追加剩余失败数量。
	if len(failed) > 3 {
		nodes = append(nodes, treeNode{lines: []string{styles.faint.Render(fmt.Sprintf("另有 %d 个失败", len(failed)-3))}})
	}
	return nodes
}

// formatRunningCallText 返回转动标记、主参数和最新输出组成的运行行。
func formatRunningCallText(call *toolCall, spinner string, styles tuiStyles, statuses *sessionStatuses) string {
	// 1. 读取主参数和可用的进度输出，再给正文套暗色。
	text := strings.TrimRight(" "+getToolMainArgument(call.call, statuses), " ")
	if call.progress != "" {
		text += "  ▏" + call.progress
	}
	if text == "" {
		return spinner
	}
	return spinner + styles.faint.Render(text)
}

// formatChangedFileRows 生成改动文件列表。
func formatChangedFileRows(entries []string, width int) []string {
	// 1. 没有改动时返回空，否则按宽度拼接文件条目，宽度不足时按文件换行。
	if len(entries) == 0 {
		return nil
	}
	const prefix = "改动 "
	var rows []string
	current := ""
	for index, entry := range entries {
		if index < len(entries)-1 {
			entry += ","
		}
		switch {
		case current == "":
			current = entry
		case ansi.StringWidth(prefix+current+" "+entry) > width:
			rows = append(rows, current)
			current = entry
		default:
			current += " " + entry
		}
	}
	// 2. 追加最后一行，首行使用改动标签，续行按标签宽度缩进。
	rows = append(rows, current)
	indent := strings.Repeat(" ", ansi.StringWidth(prefix))
	for index, row := range rows {
		lead := indent
		if index == 0 {
			lead = prefix
		}
		rows[index] = lead + row
	}
	return rows
}

// getChangedFileEntries 汇总成功补丁调用的文件改动和增删行数。
func getChangedFileEntries(calls []*toolCall) []string {
	// 1. 按调用顺序解析成功的 apply_patch，跳过无效补丁，合并同一文件的增删行数。
	var paths []string
	changesByPath := make(map[string]*tools.PatchFileChange)
	for _, call := range calls {
		if call.call.Name != "apply_patch" || call.getState() != toolCallSucceeded {
			continue
		}
		fileChanges, err := tools.GetPatchFileChanges(getStringArgument(call.call.Arguments, "patch"))
		if err != nil {
			continue
		}
		for _, change := range fileChanges {
			existing, found := changesByPath[change.Path]
			if !found {
				paths = append(paths, change.Path)
				changesByPath[change.Path] = &change
				continue
			}
			existing.Kind = change.Kind
			existing.AddedLines += change.AddedLines
			existing.DeletedLines += change.DeletedLines
		}
	}
	// 2. 按首次出现顺序输出清理后的路径，整文件删除只显示删除和路径。
	entries := make([]string, len(paths))
	for index, path := range paths {
		change := changesByPath[path]
		// 路径来自模型写的补丁，去掉终端控制后再显示。
		path = stripTerminalControls(path)
		if change.Kind == tools.PatchFileDelete {
			entries[index] = "删除 " + path
			continue
		}
		entries[index] = fmt.Sprintf("%s +%d −%d", path, change.AddedLines, change.DeletedLines)
	}
	return entries
}

// ── 工具参数 ──

// getToolMainArgument 按工具类型返回可显示的主参数。
func getToolMainArgument(call defs.ToolCall, statuses *sessionStatuses) string {
	// 1. 读取命令首行、路径、补丁文件或协作任务和 Session 名；其他工具读取第一个字符串字段。
	var argument string
	switch call.Name {
	case "bash":
		argument = getFirstLine(getStringArgument(call.Arguments, "command"))
	case "read_file":
		argument = getStringArgument(call.Arguments, "path")
	case "apply_patch":
		argument = strings.Join(getPatchPaths(getStringArgument(call.Arguments, "patch")), ", ")
	case "spawn_agent":
		argument = getStringArgument(call.Arguments, "task_name")
	case "send_message", "interrupt_agent", "read_session_history":
		return statuses.getSessionName(defs.SessionID(getStringArgument(call.Arguments, "session_id")))
	case "wait_agent":
		return ""
	default:
		argument = getFirstStringArgument(call.Arguments)
	}
	// 2. 清理参数中的终端控制后返回。
	return stripTerminalControls(argument)
}
