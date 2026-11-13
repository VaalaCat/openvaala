package tui

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/charmbracelet/x/ansi"
)

// getConversationTitle 返回对话的显示标题。
func getConversationTitle(conversation defs.Conversation) string {
	// 1. 读取标题，空标题显示“新对话”。
	if conversation.Title == "" {
		return "新对话"
	}
	return conversation.Title
}

// formatBlock 给内容块补上末尾空行。
func formatBlock(lines []string) []string {
	// 1. 跳过空内容，在非空内容末尾追加空行。
	if len(lines) == 0 {
		return nil
	}
	return append(lines, "")
}

// truncateLine 把文本压成一行并截断到显示宽度。
func truncateLine(text string, width int) string {
	// 1. 展开制表符、合并换行，再截断超宽文字。
	return ansi.Truncate(strings.ReplaceAll(expandTabs(text), "\n", " "), width, "…")
}

// getTextTail 返回最后一个非空行的末尾文字。
func getTextTail(text string, width int) string {
	// 1. 去掉终端控制，从后往前找到非空行并展开制表符。
	lines := strings.Split(stripTerminalControls(text), "\n")
	line := ""
	for index := len(lines) - 1; index >= 0 && line == ""; index-- {
		line = strings.TrimSpace(lines[index])
	}
	line = expandTabs(line)
	lineWidth := ansi.StringWidth(line)
	if lineWidth <= width {
		return line
	}
	// 2. 超宽时保留结尾，宽度足够时在前面加省略号。
	if width < 2 {
		return ansi.Truncate(line, max(0, width), "")
	}
	return "…" + ansi.TruncateLeft(line, lineWidth-(width-1), "")
}

// expandTabs 将制表符展开为空格。
func expandTabs(text string) string {
	// 1. 将文本中的每个制表符替换为四个空格。
	return strings.ReplaceAll(text, "\t", "    ")
}

// lastRunes 返回文本末尾至多 count 个字符。
func lastRunes(text string, count int) string {
	// 1. 按 Unicode 字符截取文本末尾。
	runes := []rune(text)
	return string(runes[max(0, len(runes)-count):])
}

// getDisplayDirectory 把目录里的家目录前缀换成 ~ 后返回。
func getDisplayDirectory(directory string) string {
	// 1. 目录就是家目录时返回 ~，在家目录下时换掉前缀；读不到家目录或不在家目录下时原样返回。
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if directory == home {
			return "~"
		}
		if rest, found := strings.CutPrefix(directory, home+string(os.PathSeparator)); found {
			return "~/" + rest
		}
	}
	return directory
}

// getDisplayWorkspaceDirectory 返回 Executor 的显示目录。
func getDisplayWorkspaceDirectory(executorID, directory string, isLocalServer bool) string {
	// 1. 本机 serve 的 local 目录缩写家目录，其余目录原样返回。
	if executorID == defs.LocalExecutorID && isLocalServer {
		return getDisplayDirectory(directory)
	}
	return directory
}

// formatWorkLocation 返回工作位置文字，例如 local:~/code/app。
func formatWorkLocation(executorID, directory string, isLocalServer bool) string {
	// 1. 目录未知时只返回 Executor ID，否则拼接显示目录。
	if directory == "" {
		return executorID
	}
	return executorID + ":" + getDisplayWorkspaceDirectory(executorID, directory, isLocalServer)
}

// getDefaultExecutorLocation 返回默认 Executor 和它的工作目录。
func getDefaultExecutorLocation(settings defs.ExecutorSettings, executors []conversation.ExecutorDetails) (executorID, directory string) {
	// 1. 优先读取 DefaultExecutorID，否则取 Executor 列表里第一个对这个设置可用、且不是 memory 的。
	executorID = settings.DefaultExecutorID
	if executorID == "" {
		if index := slices.IndexFunc(executors, func(each conversation.ExecutorDetails) bool {
			return each.Kind != defs.ExecutorKindMemory && settings.IsExecutorAvailable(each.ID)
		}); index >= 0 {
			executorID = executors[index].ID
		}
	}
	// 2. 优先读取对话工作目录，否则读取 Executor 默认目录；都没有时返回空目录。
	if directory = settings.WorkspaceDirectories[executorID]; directory != "" {
		return executorID, directory
	}
	if index := slices.IndexFunc(executors, func(each conversation.ExecutorDetails) bool { return each.ID == executorID }); index >= 0 {
		directory = executors[index].WorkspaceDirectory
	}
	return executorID, directory
}

// cloneExecutorSettings 复制 Executor 范围和工作目录设置。
func cloneExecutorSettings(settings defs.ExecutorSettings) defs.ExecutorSettings {
	// 1. 分别复制 Executor ID 列表和目录映射。
	settings.ExecutorIDs = slices.Clone(settings.ExecutorIDs)
	settings.WorkspaceDirectories = maps.Clone(settings.WorkspaceDirectories)
	return settings
}

// splitSlashCommand 拆出 slash 命令名和参数。
func splitSlashCommand(value string) (name, argument string) {
	// 1. 移除斜线，按第一个空白拆分，将命令名转为小写并去掉参数首尾空白。
	text := strings.TrimPrefix(value, "/")
	index := strings.IndexFunc(text, unicode.IsSpace)
	if index < 0 {
		return strings.ToLower(text), ""
	}
	return strings.ToLower(text[:index]), strings.TrimSpace(text[index:])
}

// normalizePastedText 整理粘贴文本的换行、制表符和控制字符。
func normalizePastedText(text string) string {
	// 1. 统一换行、展开制表符，再去掉换行之外的控制字符。
	text = expandTabs(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text))
	return strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

// stripTerminalControls 去掉终端控制并统一换行，保留制表符。
func stripTerminalControls(text string) string {
	// 1. 移除样式和转义序列，统一换行后过滤其他控制字符。
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(ansi.Strip(text))
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

// sanitizeToolText 返回工具输出的纯文本。
func sanitizeToolText(text string) string {
	// 1. 去掉终端控制并展开制表符。
	return expandTabs(stripTerminalControls(text))
}

// joinNonEmpty 用指定分隔符连接非空字符串。
func joinNonEmpty(parts []string, separator string) string {
	// 1. 复制列表，移除空字符串后连接。
	return strings.Join(slices.DeleteFunc(slices.Clone(parts), func(part string) bool { return part == "" }), separator)
}

// formatKeyHints 渲染按键提示列表。
func formatKeyHints(hints []keyHint, styles tuiStyles) string {
	// 1. 按键用加粗文字色，说明和分隔符用暗色；无按键时只写说明。
	parts := make([]string, len(hints))
	for index, hint := range hints {
		parts[index] = styles.faint.Render(hint.action)
		if hint.key != "" {
			parts[index] = styles.hintKey.Render(hint.key) + styles.faint.Render(" "+hint.action)
		}
	}
	return strings.Join(parts, styles.faint.Render(" · "))
}

// formatElapsed 把耗时写成秒、分秒或小时分钟。
func formatElapsed(elapsed time.Duration) string {
	// 1. 将负耗时按零处理，再按时长选择显示格式。
	seconds := int(max(0, elapsed) / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh%02dm", seconds/3600, seconds%3600/60)
}

// formatReconnectReason 返回包含重试次数、等待时间和错误的重连说明。
func formatReconnectReason(changed runtimeclient.ConnectionChanged) string {
	// 1. 依次收集重试次数、等待时间和错误；没有内容时返回空。
	var parts []string
	if changed.Attempt > 0 {
		parts = append(parts, fmt.Sprintf("第 %d 次，%s 后重试", changed.Attempt, changed.RetryIn))
	}
	if changed.Err != nil {
		parts = append(parts, changed.Err.Error())
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "：") + "）"
}

// getEventConversationID 返回 Client 事件所属的对话。
func getEventConversationID(event runtimeclient.ClientEvent) defs.ConversationID {
	// 1. 按事件类型读取 ConversationID，连接事件等没有对话身份的事件返回空。
	switch event := event.(type) {
	case *runtimeclient.SessionSynced:
		return event.ConversationID
	case *runtimeclient.ItemsSaved:
		return event.ConversationID
	case *runtimeclient.ItemChanged:
		return event.ConversationID
	case *runtimeclient.ToolProgress:
		return event.ConversationID
	case *runtimeclient.HookRunChanged:
		return event.ConversationID
	case *runtimeclient.TurnStarted:
		return event.ConversationID
	case *runtimeclient.TurnFinished:
		return event.ConversationID
	case *runtimeclient.ConversationSynced:
		return event.ConversationID
	case *runtimeclient.ConversationTitleChanged:
		return event.ConversationID
	case *runtimeclient.ConversationSyncFailed:
		return event.ConversationID
	case *runtimeclient.SessionStatusChanged:
		return event.ConversationID
	}
	return ""
}

// formatUserMessageLines 将用户输入折行为带提示符的文本。
func formatUserMessageLines(text string, width int) []string {
	// 1. 展开制表符并逐段折行，首行加 ›，续行缩进两列，再去掉行尾空格。
	var lines []string
	for _, paragraph := range strings.Split(expandTabs(text), "\n") {
		for _, line := range wrapText(paragraph, width-2) {
			prefix := "  "
			if len(lines) == 0 {
				prefix = "› "
			}
			lines = append(lines, strings.TrimRight(prefix+line, " "))
		}
	}
	return lines
}

// getMessageText 拼接消息的显示文字。
func getMessageText(message *defs.Message) string {
	// 1. 逐段追加文字，非文本内容使用种类占位符。
	var text strings.Builder
	for _, content := range message.Content {
		if content.Kind == defs.ContentText {
			text.WriteString(content.Text.Text)
			continue
		}
		text.WriteString("[" + string(content.Kind) + "]")
	}
	return text.String()
}

// getReasoningText 返回推理的显示文字。
func getReasoningText(summary, content []string) string {
	// 1. 拼接并去掉摘要首尾空白，摘要为空时读取正文。
	if text := strings.TrimSpace(strings.Join(summary, "")); text != "" {
		return text
	}
	return strings.TrimSpace(strings.Join(content, ""))
}

// formatSubagentStatus 返回子 Agent 终态的中文文字和配色状态。
func formatSubagentStatus(status defs.TurnStatus) (string, displayState) {
	// 1. 转换完成、失败和中断状态；未知状态原样显示并使用空闲配色。
	switch status {
	case defs.TurnStatusCompleted:
		return "完成", stateCompleted
	case defs.TurnStatusFailed:
		return "失败", stateFailed
	case defs.TurnStatusInterrupted:
		return "中断", stateInterrupted
	}
	return string(status), stateIdle
}

// getStateStyle 返回运行状态对应的文字样式。
func getStateStyle(state displayState, styles tuiStyles) lipgloss.Style {
	// 1. 运行和生成用强调色，思考用回复色，完成和已连接用成功色，失败用失败色，中断、未执行和重连用警告色，其余用暗色。
	switch state {
	case stateRunning, stateGenerating:
		return styles.accent
	case stateThinking:
		return styles.agent
	case stateCompleted, stateConnected:
		return styles.success
	case stateFailed:
		return styles.failure
	case stateInterrupted, stateNotExecuted, stateReconnecting:
		return styles.warning
	}
	return styles.faint
}

// 上下文占用的配色阈值：低于 contextWarningPercent 用暗色，到 contextFailurePercent 之前用警告色，之后用失败色。
const (
	contextWarningPercent = 70
	contextFailurePercent = 90
)

// getContextUsagePercent 返回下一次请求占上下文窗口的百分比，以及占用是否已知。
func getContextUsagePercent(usage runtime.ContextUsage) (percent int, isKnown bool) {
	// 1. 来源或窗口未知时返回未知，否则计算四舍五入的百分比。
	if usage.Source == runtime.ContextUsageSourceUnknown || usage.Window == 0 {
		return 0, false
	}
	return (usage.Tokens*100 + usage.Window/2) / usage.Window, true
}

// getContextUsageStyle 返回上下文占用百分比对应的样式。
func getContextUsageStyle(usage runtime.ContextUsage, styles tuiStyles) lipgloss.Style {
	// 1. 未知或低于 70% 用暗色，70% 至 90% 用警告色，90% 起用失败色。
	percent, isKnown := getContextUsagePercent(usage)
	switch {
	case !isKnown || percent < contextWarningPercent:
		return styles.faint
	case percent < contextFailurePercent:
		return styles.warning
	}
	return styles.failure
}

// formatStyledTexts 清理各段文字并按各自样式连接。
func formatStyledTexts(texts []styledText) string {
	// 1. 去掉终端控制、展开制表符并将换行改为空格，再逐段加样式。
	var line strings.Builder
	for _, each := range texts {
		line.WriteString(each.style.Render(strings.ReplaceAll(sanitizeToolText(each.text), "\n", " ")))
	}
	return line.String()
}

// formatExecutorsChange 返回 Executor 默认项、范围和目录的变更说明。
func formatExecutorsChange(change *defs.ExecutorsChange, isLocalServer bool) string {
	// 1. 依次收集默认项、移除项、新增项和目录变化，拼成一行。
	details := change.GetDetails()
	var parts []string
	if details.NewDefault != nil {
		parts = append(parts, "默认 "+formatWorkLocation(details.NewDefault.ID, details.NewDefault.WorkspaceDirectory, isLocalServer))
	}
	for _, removed := range details.Removed {
		parts = append(parts, "移除 "+removed.ID)
	}
	for _, added := range details.Added {
		parts = append(parts, "新增 "+added.ID)
	}
	for _, changed := range details.DirectoryChanges {
		parts = append(parts, changed.ID+" 目录 "+getDisplayWorkspaceDirectory(changed.ID, changed.Directory, isLocalServer))
	}
	return "Executor 已变更：" + strings.Join(parts, " · ")
}

// getHookRunNotices 返回 hook 运行的主视图提示。
func getHookRunNotices(run *defs.HookRun, toolName string, styles tuiStyles) []styledText {
	// 1. 按拦下、出错或结束本轮生成状态提示；工具被拦下时附工具名。
	var notices []styledText
	event := "hook " + string(run.Event)
	switch run.Status {
	case defs.HookRunBlocked:
		blocked := event + " 拦下"
		if toolName != "" {
			blocked += " " + toolName
		}
		notices = append(notices, styledText{
			style: styles.faint,
			text:  joinHookRunReason(blocked, run.BlockReason),
		})
	case defs.HookRunFailed:
		notices = append(notices, styledText{
			style: styles.warning,
			text:  joinHookRunReason(event+" 出错", run.ErrorMessage),
		})
	case defs.HookRunStopped:
		notices = append(notices, styledText{
			style: styles.faint,
			text:  joinHookRunReason(event+" 结束本轮", run.StopReason),
		})
	}
	// 2. 将非空系统消息追加在状态提示之后。
	if run.SystemMessage != "" {
		notices = append(notices, styledText{
			style: styles.faint,
			text:  joinHookRunReason(event, run.SystemMessage),
		})
	}
	return notices
}

// formatHookRunStatus 返回 hook 运行状态的中文文字。
func formatHookRunStatus(status defs.HookRunStatus) string {
	// 1. 转换已知运行状态，未知状态原样返回。
	switch status {
	case defs.HookRunRunning:
		return "运行中"
	case defs.HookRunCompleted:
		return "完成"
	case defs.HookRunBlocked:
		return "拦下"
	case defs.HookRunFailed:
		return "出错"
	case defs.HookRunStopped:
		return "结束本轮"
	}
	return string(status)
}

// joinHookRunReason 将非空原因接在提示后。
func joinHookRunReason(notice, reason string) string {
	// 1. 原因为空时返回提示，否则用冒号连接原因。
	if reason == "" {
		return notice
	}
	return notice + "：" + reason
}

// isReplyItem Item 是否是模型调用输出的回复消息？
func isReplyItem(item *defs.Item) bool {
	// 1. 检查消息种类、消息内容和 ModelCallID。
	return item.Kind == defs.ItemMessage && item.Message != nil && item.ModelCallID != ""
}

// getFirstLine 返回去掉首尾空白后的第一个非空行。
func getFirstLine(text string) string {
	// 1. 依次检查各行，找到非空文字后返回。
	for line := range strings.SplitSeq(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// formatTokens 将 token 数写成整数、K 或 M。
func formatTokens(tokens int) string {
	// 1. 按数量级选择单位，K 和 M 保留至多一位小数。
	switch {
	case tokens < 1000:
		return strconv.Itoa(tokens)
	// 999_950 起使用 M 单位。
	case tokens < 999_950:
		return trimZeroDecimal(float64(tokens)/1000) + "K"
	}
	return trimZeroDecimal(float64(tokens)/1_000_000) + "M"
}

// trimZeroDecimal 格式化数值并省略零小数部分。
func trimZeroDecimal(value float64) string {
	// 1. 格式化为一位小数，再移除末尾的 .0。
	return strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0")
}

// isSessionRunning Session 是否有尚未结束的 Turn？
func isSessionRunning(info runtime.SessionRuntimeInfo) bool {
	// 1. 检查运行信息里的 ActiveTurnID 是否非空。
	return info.ActiveTurnID != ""
}

// lineIndentPattern 匹配 glamour 输出行首的左边距，以及其后的列表标记或引用竖线（notty 样式为 "| "）。
var lineIndentPattern = regexp.MustCompile(`^( *)(• |\d+\. |│ |\| )?`)

// wrapKeepingIndent 按显示宽度折行并保留正文或引用缩进。
func wrapKeepingIndent(line string, width int) []string {
	// 1. 识别左边距和列表、引用前缀，超宽前缀不计缩进。
	match := lineIndentPattern.FindStringSubmatch(ansi.Strip(line))
	indent := ansi.StringWidth(match[0])
	if indent >= width {
		indent = 0
	}
	prefix := ansi.Truncate(line, indent, "")
	continuation := strings.Repeat(" ", indent)
	if indent > 0 && (match[2] == "│ " || match[2] == "| ") {
		continuation = prefix
	}
	// 2. 折行正文，在首行保留前缀，续行重复缩进或引用竖线。
	wrapped := wrapText(ansi.TruncateLeft(line, indent, ""), width-indent)
	for index, part := range wrapped {
		if index == 0 {
			wrapped[index] = prefix + part
			continue
		}
		wrapped[index] = continuation + part
	}
	return wrapped
}

// wrapCodeLine 硬折一行代码，续行保留行首空格。
func wrapCodeLine(line string, width int) []string {
	// 1. 计算行首空格宽度，缩进达到行宽时改为零。
	plain := ansi.Strip(line)
	indent := len(plain) - len(strings.TrimLeft(plain, " "))
	if indent >= width {
		indent = 0
	}
	prefix := ansi.Truncate(line, indent, "")
	// 2. 按剩余宽度硬折正文，再给每行补上空格缩进。
	wrapped := strings.Split(ansi.Hardwrap(ansi.TruncateLeft(line, indent, ""), width-indent, true), "\n")
	for index, part := range wrapped {
		if index == 0 {
			wrapped[index] = prefix + part
			continue
		}
		wrapped[index] = strings.Repeat(" ", indent) + part
	}
	return wrapped
}

// wrapStyledRows 将文本硬折后逐行套用样式。
func wrapStyledRows(text string, width int, render func(...string) string) []string {
	// 1. 展开制表符并硬折文本；未传样式时直接返回。
	rows := wrapCodeLine(expandTabs(text), width)
	if render == nil {
		return rows
	}
	for index, row := range rows {
		rows[index] = render(row)
	}
	return rows
}

// fillBackground 补齐一行宽度并填充指定底色。
func fillBackground(line string, width int, style lipgloss.Style) string {
	// 1. 截断超宽文字，再补空格到指定宽度。
	line = ansi.Truncate(line, width, "")
	line += strings.Repeat(" ", max(0, width-ansi.StringWidth(line)))
	if _, isNoColor := style.GetBackground().(lipgloss.NoColor); isNoColor {
		return line
	}
	// 2. 样式复位后重新写入底色，再包裹整行。
	open := strings.TrimSuffix(style.Render(" "), " "+ansi.ResetStyle)
	line = strings.ReplaceAll(line, ansi.ResetStyle, ansi.ResetStyle+open)
	return open + line + ansi.ResetStyle
}

// splitPrintChunks 按终端行数上限将定稿行分批。
func splitPrintChunks(lines []string, width, rowLimit int) [][]string {
	// 1. 按换行分段计一行，超宽时再加显示宽度除以终端宽度的整商；累计超限时另起一批，单行超限也独立保留。
	var chunks [][]string
	var chunk []string
	chunkRows := 0
	for _, line := range lines {
		rows := 0
		for part := range strings.SplitSeq(line, "\n") {
			rows++
			if partWidth := ansi.StringWidth(part); width > 0 && partWidth > width {
				rows += partWidth / width
			}
		}
		if len(chunk) > 0 && chunkRows+rows > rowLimit {
			chunks = append(chunks, chunk)
			chunk, chunkRows = nil, 0
		}
		chunk = append(chunk, line)
		chunkRows += rows
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	return chunks
}

// isDarkColor 颜色是否为深色？
func isDarkColor(c color.Color) bool {
	// 1. 用 RGB 最大值和最小值计算 HSL 亮度，判断是否低于一半。
	r, g, b, _ := c.RGBA()
	high, low := max(r, g, b), min(r, g, b)
	return float64(high+low)/2/0xffff < 0.5
}

// lineBreakMark 是折行时临时插入的零宽断行点，折行后移除。
const lineBreakMark = "\u200b"

// 不能出现在行首的闭合标点与不能出现在行尾的开放标点。
const (
	closingPunctuation = "，。、；：！？）」』》…"
	openingPunctuation = "（「『《"
)

// wrapText 按显示宽度折行，保留中日韩标点的位置。
func wrapText(text string, width int) []string {
	// 1. 插入宽字符断行点，折行后移除临时标记。
	wrapped := ansi.Wrap(insertWideCharBreaks(text), width, lineBreakMark)
	return strings.Split(strings.ReplaceAll(wrapped, lineBreakMark, ""), "\n")
}

// wrapTextKeepingStyles 按显示宽度折行，每行分别开关样式。
func wrapTextKeepingStyles(text string, width int) []string {
	// 1. 插入宽字符断行点，逐行保留样式并移除临时标记。
	wrapped := lipgloss.Wrap(insertWideCharBreaks(text), width, lineBreakMark)
	return strings.Split(strings.ReplaceAll(wrapped, lineBreakMark, ""), "\n")
}

// insertWideCharBreaks 在相邻宽字符之间插入可断行标记。
func insertWideCharBreaks(text string) string {
	// 1. 逐个读取字符和控制序列，跳过闭合标点之前及开放标点之后的位置。
	var result strings.Builder
	var state byte
	previousWide := ""
	for len(text) > 0 {
		sequence, width, length, nextState := ansi.DecodeSequence(text, state, nil)
		state, text = nextState, text[length:]
		if width == 0 {
			result.WriteString(sequence)
			continue
		}
		if width == 2 && previousWide != "" && !strings.Contains(closingPunctuation, sequence) && !strings.Contains(openingPunctuation, previousWide) {
			result.WriteString(lineBreakMark)
		}
		result.WriteString(sequence)
		previousWide = ""
		if width == 2 {
			previousWide = sequence
		}
	}
	return result.String()
}

// getCodeFenceOpening 识别代码块的起始围栏。
func getCodeFenceOpening(line string) string {
	// 1. 识别连续的反引号或波浪号；不是起始行，或反引号围栏的说明文字含反引号时返回空。
	if !strings.HasPrefix(line, "```") && !strings.HasPrefix(line, "~~~") {
		return ""
	}
	fence := line[:len(line)-len(strings.TrimLeft(line, line[:1]))]
	if fence[0] == '`' && strings.Contains(line[len(fence):], "`") {
		return ""
	}
	return fence
}

// isCodeFenceClosing 这一行是否关闭了指定的围栏？
func isCodeFenceClosing(line, fence string) bool {
	// 1. 对已去掉首尾空白的行，检查它只含围栏字符，且长度不短于起始围栏。
	return len(line) >= len(fence) && strings.Trim(line, fence[:1]) == ""
}

// getLeadingSpaceCount 返回行首空白的列数。
func getLeadingSpaceCount(line string) int {
	// 1. 将制表符展开为四列，计算移除行首空格前后的长度差。
	expanded := expandTabs(line)
	return len(expanded) - len(strings.TrimLeft(expanded, " "))
}

// trimLinePadding 去掉行尾填充空格。
func trimLinePadding(line string) string {
	// 1. 计算纯文本去掉行尾空格后的宽度，空白行返回空串，其余按宽度截断带样式原文。
	plain := strings.TrimRight(ansi.Strip(line), " ")
	if plain == "" {
		return ""
	}
	return ansi.Truncate(line, ansi.StringWidth(plain), "")
}

// trimBlankEdges 去掉列表首尾的空行。
func trimBlankEdges(lines []string) []string {
	// 1. 分别向内扫描首尾空行，返回剩余切片。
	start, end := 0, len(lines)
	for start < end && lines[start] == "" {
		start++
	}
	for end > start && lines[end-1] == "" {
		end--
	}
	return lines[start:end]
}

// getLastLines 返回文本的最后 count 行。
func getLastLines(text string, count int) string {
	// 1. 行数不是正数时返回空，否则从末尾向前查找换行位置；不足 count 行时返回全文。
	if count <= 0 {
		return ""
	}
	start := len(text)
	for range count {
		index := strings.LastIndexByte(text[:start], '\n')
		if index < 0 {
			return text
		}
		start = index
	}
	return text[start+1:]
}

// getCompleteLines 返回文本中已经以换行结束的部分。
func getCompleteLines(text string) string {
	// 1. 截取最后一个换行及其之前的文字。
	return text[:strings.LastIndexByte(text, '\n')+1]
}

// getStringArgument 读取 JSON 参数中的字符串字段。
func getStringArgument(arguments, name string) string {
	// 1. 解码参数对象，再将指定字段解码为字符串；任一步解码无效时返回空。
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &fields) != nil {
		return ""
	}
	var value string
	if json.Unmarshal(fields[name], &value) != nil {
		return ""
	}
	return value
}

// getToolExecutor 返回工具调用显示的 Executor。
func getToolExecutor(call *toolCall, defaultExecutorID string) string {
	// 1. Agent 协作工具返回空；已有结果时直接取结果中的 Executor，尚无结果时取参数或默认 Executor。
	switch call.call.Name {
	case "spawn_agent", "send_message", "interrupt_agent", "list_agents", "read_session_history", "wait_agent":
		return ""
	}
	if call.result != nil {
		return call.result.Executor
	}
	return cmp.Or(getStringArgument(call.call.Arguments, "executor"), defaultExecutorID)
}

// formatToolLabel 返回工具的显示名称。
func formatToolLabel(call *toolCall, defaultExecutorID string) string {
	// 1. MCP 工具显示服务名和工具名，再追加非空的 Executor。
	name := call.call.Name
	if serverID, toolName, ok := mcpclient.GetToolNameParts(name); ok {
		name = serverID + " · " + toolName
	}
	if executorID := getToolExecutor(call, defaultExecutorID); executorID != "" {
		return name + "@" + executorID
	}
	return name
}

// getFirstStringArgument 按 JSON 字段顺序返回第一个字符串值。
func getFirstStringArgument(arguments string) string {
	// 1. 读取 JSON 对象，依次解码字段并返回首个字符串。
	decoder := json.NewDecoder(strings.NewReader(arguments))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		if _, err := decoder.Token(); err != nil {
			return ""
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			return text
		}
	}
	return ""
}

// getPatchPaths 按补丁顺序返回涉及的文件路径。
func getPatchPaths(patch string) []string {
	// 1. 解析补丁变更，无效补丁返回空，否则收集每条变更的路径。
	changes, err := tools.GetPatchFileChanges(patch)
	if err != nil {
		return nil
	}
	paths := make([]string, len(changes))
	for index, change := range changes {
		paths[index] = change.Path
	}
	return paths
}

// getProgressLine 返回工具文本输出的最后一个非空行。
func getProgressLine(content json.RawMessage) string {
	// 1. 解码文本并清理终端控制，从末尾查找非空行。
	var text string
	if json.Unmarshal(content, &text) != nil {
		return ""
	}
	lines := strings.Split(sanitizeToolText(text), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return ""
}

// /resume 列表各列的显示宽度与标题的最小宽度。
const (
	resumeTimeWidth     = 11 // "09-28 10:02"
	resumeCountWidth    = 7  // "9999 条"
	resumeIDWidth       = 8
	resumeTitleMinWidth = 8
)

// formatActiveTime 将最后活跃时间写成本地日期或相对时间。
func formatActiveTime(updatedAt int64, now time.Time) string {
	// 1. 零时间显示破折号，其他时间换算为 now 所在时区。
	if updatedAt == 0 {
		return "—"
	}
	at := time.UnixMilli(updatedAt).In(now.Location())
	// 2. 一小时内显示刚刚或几分钟前；当天显示几小时前，昨天显示昨天加时刻，同一年显示月日时刻，更早只显示日期。
	elapsed := now.Sub(at)
	switch {
	case elapsed < time.Minute:
		return "刚刚"
	case elapsed < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(elapsed/time.Minute))
	}
	switch {
	case isSameDay(at, now):
		return fmt.Sprintf("%d 小时前", int(elapsed/time.Hour))
	case isSameDay(at, now.AddDate(0, 0, -1)):
		return "昨天 " + at.Format("15:04")
	case at.Year() == now.Year():
		return at.Format("01-02 15:04")
	}
	return at.Format("2006-01-02")
}

// isSameDay 两个时间是否属于同一个日期？
func isSameDay(a, b time.Time) bool {
	// 1. 分别读取年月日并比较。
	aYear, aMonth, aDay := a.Date()
	bYear, bMonth, bDay := b.Date()
	return aYear == bYear && aMonth == bMonth && aDay == bDay
}

// formatResumeRow 渲染对话标题、活跃时间、消息数和短 ID。
func formatResumeRow(prefix string, conversation defs.Conversation, titleMark string, now time.Time, width int) string {
	// 1. 准备右侧信息列，宽度不足时依次移除 ID 和消息数。
	columns := []string{
		padLeft(formatActiveTime(conversation.UpdatedAt, now), resumeTimeWidth),
		padLeft(fmt.Sprintf("%d 条", conversation.MessageCount), resumeCountWidth),
		lastRunes(string(conversation.ID), resumeIDWidth),
	}
	titleWidth := width - ansi.StringWidth(prefix) - getColumnsWidth(columns)
	for len(columns) > 1 && titleWidth < resumeTitleMinWidth {
		columns = columns[:len(columns)-1]
		titleWidth = width - ansi.StringWidth(prefix) - getColumnsWidth(columns)
	}
	titleWidth = max(titleWidth, resumeTitleMinWidth)
	// 2. 将标题压成单行，截断后拼接标记和右侧信息列。
	title := strings.ReplaceAll(expandTabs(getConversationTitle(conversation)), "\n", " ")
	title = padRight(ansi.Truncate(title, max(0, titleWidth-ansi.StringWidth(titleMark)), "…")+titleMark, titleWidth)
	return truncateLine(prefix+title+"  "+strings.Join(columns, "  "), width)
}

// getColumnsWidth 返回各列及间隔的总宽度。
func getColumnsWidth(columns []string) int {
	// 1. 累加每列的显示宽度和前置两格间隔。
	width := 0
	for _, column := range columns {
		width += 2 + ansi.StringWidth(column)
	}
	return width
}

// padLeft 在左侧补空格到指定显示宽度。
func padLeft(text string, width int) string {
	// 1. 按显示宽度计算空格数并添加到文字前。
	return strings.Repeat(" ", max(0, width-ansi.StringWidth(text))) + text
}

// padRight 在右侧补空格到指定显示宽度。
func padRight(text string, width int) string {
	// 1. 按显示宽度计算空格数并添加到文字后。
	return text + strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
}

// getFieldLabelWidth 返回各组字段中最宽标签的显示宽度。
func getFieldLabelWidth(fieldGroups ...[]panelField) int {
	// 1. 遍历每组标签并记录最大显示宽度。
	width := 0
	for _, fields := range fieldGroups {
		for _, field := range fields {
			width = max(width, ansi.StringWidth(field.label))
		}
	}
	return width
}

// formatFieldRows 渲染字段标签和值，超宽值折行后对齐。
func formatFieldRows(fields []panelField, labelWidth int, width int, styles tuiStyles) []string {
	// 1. 取传入宽度和本节标签宽度的较大值，计算值所在列。
	labelWidth = max(labelWidth, getFieldLabelWidth(fields))
	valueColumn := 2 + labelWidth + 2
	// 2. 按行分别开关样式折行字段值，首行显示标签，续行缩进到值所在列。
	var rows []string
	for _, field := range fields {
		for index, row := range wrapTextKeepingStyles(strings.ReplaceAll(field.value, "\n", " "), max(1, width-valueColumn)) {
			lead := strings.Repeat(" ", valueColumn)
			if index == 0 {
				lead = "  " + styles.faint.Render(padRight(field.label, labelWidth)) + "  "
			}
			rows = append(rows, lead+row)
		}
	}
	return rows
}

// appendPanelSection 追加带标题的面板分节。
func appendPanelSection(lines []string, title string, rows []string, styles tuiStyles) []string {
	// 1. 已有内容后先空一行，再追加节标题和字段行。
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	return append(append(lines, styles.heading.Render(title)), rows...)
}

// orDash 把空文本换成破折号。
func orDash(text string) string {
	// 1. 空文本显示破折号，非空文本原样返回。
	if text == "" {
		return "—"
	}
	return text
}

// getTextPrefix 返回至多 limit 字节的完整字符前缀。
func getTextPrefix(text string, limit int) string {
	// 1. 文本超长时回退到 UTF-8 字符起点，再截取前缀。
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// trimLastRune 去掉文本末尾一个 Unicode 字符。
func trimLastRune(text string) string {
	// 1. 读取最后一个字符的字节数并截去。
	_, size := utf8.DecodeLastRuneInString(text)
	return text[:len(text)-size]
}

// formatToolResultContent 格式化工具结果内容。
func formatToolResultContent(content json.RawMessage) string {
	// 1. 解析命令结果，非命令结果保留 JSON 原文。
	command, isCommandResult := tools.ParseCommandResult(content)
	if !isCommandResult {
		return string(content)
	}
	// 2. 写出退出码或失败原因，再接终端输出。
	status := command.Status
	switch {
	case command.ExitCode != nil:
		status = fmt.Sprintf("exit %d", *command.ExitCode)
	case command.Error != "":
		status += ": " + command.Error
	}
	return status + "\n" + strings.TrimRight(command.Output, "\n")
}

// formatJSON 格式化 JSON 文本。
func formatJSON(text string) string {
	// 1. 将合法 JSON 按两格缩进格式化，解析失败时返回原文。
	var formatted bytes.Buffer
	if json.Indent(&formatted, []byte(text), "", "  ") != nil {
		return text
	}
	return formatted.String()
}

// formatPendingConversations 返回等待自动应用配置的对话数量提示。
func formatPendingConversations(pending []defs.ConversationID) string {
	// 1. 没有等待应用的对话时返回空，否则写出数量和空闲后自动应用提示。
	if len(pending) == 0 {
		return ""
	}
	return fmt.Sprintf("，%d 个对话将在空闲后自动应用", len(pending))
}

// formatAgentModelNotice 返回新对话仍使用 Agent 指定模型的提示。
func formatAgentModelNotice(agentDefaultModel, defaultModel defs.ModelRef) string {
	// 1. Agent 默认模型为空或与全局默认相同时不提示，否则写出 Agent 模型。
	if agentDefaultModel == (defs.ModelRef{}) || agentDefaultModel == defaultModel {
		return ""
	}
	return "；新对话仍用 Agent 指定的 " + agentDefaultModel.String()
}

// getServerErrorText 提取错误的显示文字。
func getServerErrorText(err error) string {
	// 1. JSON-RPC 响应错误只取 Message，其他错误调用 Error。
	var responseErr *runtimeclient.ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.Message
	}
	return err.Error()
}

// maskAPIKey 隐去 API key 的主要内容。
func maskAPIKey(key string) string {
	// 1. 空 key 返回空，其余取末尾至多四个字符并加省略号。
	if key == "" {
		return ""
	}
	return "…" + lastRunes(key, 4)
}

// parseModelIDs 拆出逗号分隔的非空模型 ID。
func parseModelIDs(text string) []string {
	// 1. 逐项去掉首尾空白，收集非空 ID。
	var modelIDs []string
	for part := range strings.SplitSeq(text, ",") {
		if modelID := strings.TrimSpace(part); modelID != "" {
			modelIDs = append(modelIDs, modelID)
		}
	}
	return modelIDs
}

// parsePositiveInt 解析正整数输入。
func parsePositiveInt(text string) (value int, isValid bool) {
	// 1. 去掉首尾空白，空文本返回默认值零，否则解析并检查大于零；无效文本返回失败。
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, true
	}
	value, err := strconv.Atoi(text)
	return value, err == nil && value > 0
}

// formatPositiveInt 格式化正整数。
func formatPositiveInt(value int) string {
	// 1. 非正数返回空，正数转成十进制文字。
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value)
}

// getProviderModelDisplayName 返回模型配置的显示名。
func getProviderModelDisplayName(config *defs.ProviderModelConfig) string {
	// 1. 配置为空时返回空，否则读取 DisplayName。
	if config == nil {
		return ""
	}
	return config.DisplayName
}

// formatProviderModelLimit 格式化模型 token 上限。
func formatProviderModelLimit(tokens int) string {
	// 1. 非正上限显示破折号，其他值按紧凑的 token 数格式化。
	if tokens <= 0 {
		return "—"
	}
	return formatTokens(tokens)
}
