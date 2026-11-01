package tui

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/charmbracelet/x/ansi"
)

// sessionStatuses 保存当前对话各 Session 的最新运行信息和已观察到的开始时间；由 tuiModel 修改，转录和面板读取。
type sessionStatuses struct {
	// rootSessionID 是对话根 Session 的 ID，草稿时为空。
	rootSessionID defs.SessionID
	// infos 是各 Session 最新的运行信息。
	infos map[defs.SessionID]runtime.SessionRuntimeInfo
	// runningSince 是 TUI 观察到 Session 开始当前这次运行（活动 Turn）的时刻；同步时已在运行、不知道开始时间的不记录。
	runningSince map[defs.SessionID]time.Time
}

// newSessionStatuses 创建对话的 Session 运行信息记录。
func newSessionStatuses(rootSessionID defs.SessionID) *sessionStatuses {
	// 1. 保存根 Session ID，初始化运行信息和开始时间表。
	return &sessionStatuses{
		rootSessionID: rootSessionID,
		infos:         make(map[defs.SessionID]runtime.SessionRuntimeInfo),
		runningSince:  make(map[defs.SessionID]time.Time),
	}
}

// handleSynced 用同步到的全部 Session 信息替换现有记录，只保留仍然有效的开始时间。
func (s *sessionStatuses) handleSynced(infos []runtime.SessionRuntimeInfo) {
	// 1. 收集仍在运行、活动 Turn 没变的 Session 已知的开始时间。
	runningSince := make(map[defs.SessionID]time.Time)
	for _, info := range infos {
		old, found := s.infos[info.Session.ID]
		since, known := s.runningSince[info.Session.ID]
		if found && known && isSessionRunning(info) && old.ActiveTurnID == info.ActiveTurnID {
			runningSince[info.Session.ID] = since
		}
	}
	// 2. 替换全部 Session 信息和开始时间表。
	s.infos = make(map[defs.SessionID]runtime.SessionRuntimeInfo, len(infos))
	for _, info := range infos {
		s.infos[info.Session.ID] = info
	}
	s.runningSince = runningSince
}

// handleChanged 更新一个 Session 的运行信息和本次运行开始时间。
func (s *sessionStatuses) handleChanged(info runtime.SessionRuntimeInfo, now time.Time) {
	// 1. 保存最新信息；不在运行时清掉开始时间，第一次出现或换了活动 Turn 时记下当前时刻。
	id := info.Session.ID
	old, found := s.infos[id]
	s.infos[id] = info
	switch {
	case !isSessionRunning(info):
		delete(s.runningSince, id)
	case !found || old.ActiveTurnID != info.ActiveTurnID:
		s.runningSince[id] = now
	}
}

// getRoot 返回根 Session 运行信息。
func (s *sessionStatuses) getRoot() (runtime.SessionRuntimeInfo, bool) {
	// 1. 按根 Session ID 读取信息，未同步或草稿返回未找到。
	return s.getInfo(s.rootSessionID)
}

// getInfo 返回指定 Session 的运行信息和是否存在。
func (s *sessionStatuses) getInfo(sessionID defs.SessionID) (runtime.SessionRuntimeInfo, bool) {
	// 1. 从信息表读取 Session。
	info, found := s.infos[sessionID]
	return info, found
}

// isRootRunning 根 Session 是否有活动 Turn 正在运行？
func (s *sessionStatuses) isRootRunning() bool {
	// 1. 读取根 Session 并检查运行状态。
	root, found := s.getRoot()
	return found && isSessionRunning(root)
}

// isAnyRunning 对话中是否有 Session 正在运行？
func (s *sessionStatuses) isAnyRunning() bool {
	// 1. 有一个 Session 在运行就算。
	for info := range maps.Values(s.infos) {
		if isSessionRunning(info) {
			return true
		}
	}
	return false
}

// getRunningSubagentCount 返回正在运行的非根 Session 数量。
func (s *sessionStatuses) getRunningSubagentCount() int {
	// 1. 遍历运行信息，累加运行中的非根 Session。
	count := 0
	for id, info := range s.infos {
		if id != s.rootSessionID && isSessionRunning(info) {
			count++
		}
	}
	return count
}

// getRunningSince 返回已观察到的本次运行开始时间及是否已知。
func (s *sessionStatuses) getRunningSince(sessionID defs.SessionID) (time.Time, bool) {
	// 1. 从开始时间表读取 Session。
	since, found := s.runningSince[sessionID]
	return since, found
}

// getSessionName 返回 Session 的显示名称。
func (s *sessionStatuses) getSessionName(sessionID defs.SessionID) string {
	// 1. 空 ID 直接返回；依次选择任务名、Agent 名或 ID 末八字符，并清理终端控制。
	if sessionID == "" {
		return ""
	}
	info := s.infos[sessionID]
	return stripTerminalControls(cmp.Or(info.Session.TaskName, info.Session.AgentName, lastRunes(string(sessionID), 8)))
}

// getSubagentRows 按深度优先顺序返回根 Session 下的子 Session 行。
func (s *sessionStatuses) getSubagentRows() []sessionTreeRow {
	// 1. 按父 Session 收集子 Session ID。
	children := make(map[defs.SessionID][]defs.SessionID)
	for id, info := range s.infos {
		if id != s.rootSessionID {
			children[info.Session.ParentSessionID] = append(children[info.Session.ParentSessionID], id)
		}
	}
	// 2. 定义深度优先遍历，同一父 Session 的各子 Session 按 ID 排序。
	var rows []sessionTreeRow
	var visit func(parent defs.SessionID, depth int)
	visit = func(parent defs.SessionID, depth int) {
		ids := children[parent]
		slices.Sort(ids)
		for _, id := range ids {
			rows = append(rows, sessionTreeRow{
				info:  s.infos[id],
				depth: depth,
			})
			visit(id, depth+1)
		}
	}
	// 3. 根 Session 非空时从根开始遍历。
	if s.rootSessionID != "" {
		visit(s.rootSessionID, 0)
	}
	return rows
}

// getTotalUsage 返回包括根 Session 的总用量。
func (s *sessionStatuses) getTotalUsage() defs.TokenUsage {
	// 1. 累加各 Session 的用量记录。
	var total defs.TokenUsage
	for _, info := range s.infos {
		total = total.Add(info.Usage)
	}
	return total
}

// queuedInputs 保存本客户端已提交、还没开始执行的 normal 输入，按提交顺序；输入开始执行的信号是它的 items/saved，
// 被 UserPromptSubmit 拦下的输入不保存，以所在 Turn 的结束为准。
type queuedInputs struct {
	entries []queuedInput
}

// queuedInput 是一条排队中的输入。
type queuedInput struct {
	itemID defs.ItemID
	text   string
}

// add 记录已成功提交但尚未开始执行的普通输入。
func (q *queuedInputs) add(itemID defs.ItemID, text string) {
	// 1. 按提交顺序保存服务端返回的输入 Item ID 和原文。
	q.entries = append(q.entries, queuedInput{
		itemID: itemID,
		text:   text,
	})
}

// handleItemsSaved 移除已经保存并开始执行的输入。
func (q *queuedInputs) handleItemsSaved(items []runtimeprotocol.LoggedItem) {
	// 1. 按保存记录中的 Item ID 删除排队项，包含重连补齐的历史。
	q.entries = slices.DeleteFunc(q.entries, func(entry queuedInput) bool {
		return slices.ContainsFunc(items, func(logged runtimeprotocol.LoggedItem) bool { return logged.Item.ID == entry.itemID })
	})
}

// handleTurnFinished 移除已由结束 Turn 读入的输入。
func (q *queuedInputs) handleTurnFinished(inputItemIDs []defs.ItemID) {
	// 1. 按 Turn 列出的输入 ID 删除排队项，包含被 UserPromptSubmit 拦下而未保存的输入。
	q.entries = slices.DeleteFunc(q.entries, func(entry queuedInput) bool { return slices.Contains(inputItemIDs, entry.itemID) })
}

// handleRootSynced 按同步到的根 Session 信息清理排队预览。
func (q *queuedInputs) handleRootSynced(root runtime.SessionRuntimeInfo) {
	// 1. 根 Session 没有活动 Turn 时清空本地排队记录。
	if !isSessionRunning(root) {
		q.entries = nil
	}
}

// handleRootChanged 按根 Session 状态更新排队预览。
func (q *queuedInputs) handleRootChanged(root runtime.SessionRuntimeInfo) {
	// 1. 根 Session 停止中或已停止时清空排队记录，两轮之间的空闲推送保留记录。
	if root.State == session.SessionStateStopping || root.State == session.SessionStateStopped {
		q.entries = nil
	}
}

// getPreviewLines 生成排队输入预览。
func (q *queuedInputs) getPreviewLines(width int, styles tuiStyles) []string {
	// 1. 最多显示两行，读取前两项首行；超出两项时在第二行追加剩余数量，再按宽度截断。
	var lines []string
	for index, entry := range q.entries[:min(2, len(q.entries))] {
		suffix := ""
		if index == 1 && len(q.entries) > 2 {
			suffix = fmt.Sprintf("  +%d", len(q.entries)-2)
		}
		line := truncateLine("  "+styles.accent.Render("排队：")+styles.faint.Render(getFirstLine(entry.text)), width-ansi.StringWidth(suffix))
		if suffix != "" {
			line += styles.faint.Render(suffix)
		}
		lines = append(lines, line)
	}
	return lines
}

// getStatusLines 返回连接、生成状态和临时通知组成的状态行。
func (m *tuiModel) getStatusLines() []string {
	// 1. 优先显示重连，其次显示根 Session 运行状态或无模型提示。
	var parts []string
	switch {
	case m.connection.State == runtimeclient.ConnectionReconnecting:
		parts = append(parts, getStateStyle(stateReconnecting, m.styles).Render("⚠ 重连中"+formatReconnectReason(m.connection)))
	case m.sessionStatuses.isRootRunning():
		parts = append(parts, m.formatRunningStatus())
	case m.isModelListEmpty:
		parts = append(parts, m.styles.warning.Render("还没有模型，/provider 添加"))
	}
	// 2. 追加临时通知；没有内容时返回空，否则按宽度截断。
	if m.notice.text != "" {
		parts = append(parts, m.formatNotice(m.notice.text))
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{truncateLine(strings.Join(parts, m.styles.faint.Render(" · ")), m.width)}
}

// formatRunningStatus 返回根 Session 的生成状态、耗时和中断提示。
func (m *tuiModel) formatRunningStatus() string {
	// 1. 正在推理时显示思考中，否则显示生成中。
	verb, state := "生成中", stateGenerating
	if m.transcript.isReasoning() {
		verb, state = "思考中", stateThinking
	}
	// 2. 拼接转动标记和已知耗时，再追加中断按键。
	text := m.spinner.View() + " " + getStateStyle(state, m.styles).Render(verb)
	if since, found := m.sessionStatuses.getRunningSince(m.conversation.RootSessionID); found {
		text += " " + formatElapsed(m.now().Sub(since))
	}
	return text + m.styles.faint.Render(" · ") + formatKeyHints([]keyHint{{
		key:    "esc esc",
		action: "中断",
	}}, m.styles)
}

// formatNotice 返回带错误色或辅助色的临时通知。
func (m *tuiModel) formatNotice(text string) string {
	// 1. 错误通知使用失败色，其他通知使用暗色。
	if m.notice.isError {
		return m.styles.failure.Render(text)
	}
	return m.styles.faint.Render(text)
}

// getFooterLine 生成模型、上下文、子 Agent 和工作位置组成的底栏。
func (m *tuiModel) getFooterLine() string {
	// 1. 读取连接、模型、上下文和运行中子 Agent 数量。
	connection, model, context := m.getFooterConnection(), styledText{text: m.formatFooterModel()}, m.getFooterContext()
	subagents := styledText{style: m.styles.accent}
	if count := m.sessionStatuses.getRunningSubagentCount(); count > 0 {
		subagents.text = fmt.Sprintf("子Agent %d 运行", count)
	}
	// 2. 读取默认 Executor 和目录，生成暗色的工作位置。
	executorID, directory := m.getWorkLocation()
	workLocation := styledText{
		style: m.styles.faint,
		text:  formatWorkLocation(executorID, directory, m.isLocalServer),
	}
	// 3. 宽度不足时依次省掉工作位置和子 Agent。
	for _, parts := range [][]styledText{
		{connection, model, context, subagents, workLocation},
		{connection, model, context, subagents},
		{connection, model, context},
	} {
		if line := m.joinFooterParts(parts); ansi.StringWidth(line) <= m.width {
			return line
		}
	}
	// 4. 仍放不下时，模型至少可留两列则缩短模型名，否则只显示按宽度截断的连接和上下文。
	fixed := m.joinFooterParts([]styledText{connection, context})
	modelWidth := m.width - ansi.StringWidth(fixed) - ansi.StringWidth(" · ")
	if modelWidth < 2 {
		return ansi.Truncate(fixed, m.width, "…")
	}
	model.text = ansi.Truncate(model.text, modelWidth, "…")
	return m.joinFooterParts([]styledText{connection, model, context})
}

// joinFooterParts 为底栏项目上色并连接。
func (m *tuiModel) joinFooterParts(parts []styledText) string {
	// 1. 跳过空项目，套用各自样式，再用暗色间隔符连接。
	var rendered []string
	for _, part := range parts {
		if part.text != "" {
			rendered = append(rendered, part.style.Render(part.text))
		}
	}
	return strings.Join(rendered, m.styles.faint.Render(" · "))
}

// formatFooterModel 返回底栏的当前模型和下一 Turn 模型说明。
func (m *tuiModel) formatFooterModel() string {
	// 1. 没有根 Session 时显示草稿模型，没指定时是 Agent 默认模型。
	root, found := m.sessionStatuses.getRoot()
	if !found {
		return m.formatModelSelection(m.getCurrentModelSelection())
	}
	// 2. 所选模型与本轮模型不同时分别列出，否则只显示所选模型。
	selected := m.formatModelSelection(root.Session.Model)
	if active := root.ActiveTurnModel; active != (defs.ModelSelection{}) && active != root.Session.Model {
		return "执行 " + m.formatModelSelection(active) + " · 下一 Turn " + selected
	}
	return selected
}

// formatModelSelection 返回模型显示名和可选的推理强度。
func (m *tuiModel) formatModelSelection(selection defs.ModelSelection) string {
	// 1. 没有模型引用时显示默认模型。
	if selection.Ref == (defs.ModelRef{}) {
		return "默认模型"
	}
	// 2. 读取模型显示名，强度非空时追加。
	text := m.getModelDisplayName(selection.Ref)
	if selection.ReasoningEffort != "" {
		text += "·" + selection.ReasoningEffort
	}
	return text
}

// getModelDisplayName 返回模型显示名称。
func (m *tuiModel) getModelDisplayName(ref defs.ModelRef) string {
	// 1. 按模型引用查询显示名，缺失时使用 ModelID。
	if name := m.modelDisplayNames[ref]; name != "" {
		return name
	}
	return ref.ModelID
}

// getFooterContext 生成底栏上下文文字和占用颜色。
func (m *tuiModel) getFooterContext() styledText {
	// 1. 读取根 Session，草稿返回空；存在时按上下文占用生成文字和样式。
	root, found := m.sessionStatuses.getRoot()
	if !found {
		return styledText{}
	}
	return styledText{
		style: getContextUsageStyle(root.Context, m.styles),
		text:  "ctx " + formatContextUsage(root.Context),
	}
}

// formatContextUsage 按用量来源生成 token、窗口和占比文字。
func formatContextUsage(usage runtime.ContextUsage) string {
	// 1. 用量未知时显示破折号和已知窗口。
	if usage.Source == runtime.ContextUsageSourceUnknown {
		if usage.Window == 0 {
			return "— 窗口未知"
		}
		return "— / " + formatTokens(usage.Window)
	}
	// 2. 估算加 ≈；窗口未知时注明，否则追加窗口和百分比。
	tokens := formatTokens(usage.Tokens)
	if usage.Source == runtime.ContextUsageSourceEstimated {
		tokens = "≈" + tokens
	}
	if usage.Window == 0 {
		return tokens + " 窗口未知"
	}
	percent, _ := getContextUsagePercent(usage)
	return fmt.Sprintf("%s/%s %d%%", tokens, formatTokens(usage.Window), percent)
}

// getFooterConnection 生成底栏连接状态。
func (m *tuiModel) getFooterConnection() styledText {
	// 1. 已连接时省略；其他状态返回文字和对应颜色。
	if m.connection.State == runtimeclient.ConnectionConnected {
		return styledText{}
	}
	return styledText{
		style: getStateStyle(getConnectionDisplayState(m.connection.State), m.styles),
		text:  formatConnectionLabel(m.connection.State),
	}
}
