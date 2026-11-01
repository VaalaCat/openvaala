package tui

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// composer 拥有输入框、粘贴占位表、输入历史，以及对话新建期间的待发送队列。只由 tuiModel 调用。
type composer struct {
	textarea textarea.Model
	// pastes 是占位编号到粘贴原文的映射：发送时展开，整段删除占位时移除。
	pastes map[int]string
	// lastPasteID 是上一次分配的占位编号，新粘贴先加一再用。
	lastPasteID int
	// history 是可用 ↑↓ 取回的已发送输入，旧的在前。
	history []string
	// historyIndex 是正在显示的历史下标；等于 len(history) 时在编辑草稿。
	historyIndex int
	// historyDraft 是开始浏览历史前输入框里的草稿，越过最新一条历史时放回。
	historyDraft string
	// pendingSends 是在新对话草稿上按 Enter 的输入：从新建对话开始，到这些输入逐条提交完为止都排在这里，按顺序发送。
	pendingSends []string
}

const (
	// pasteLineLimit、pasteRuneLimit 是直接插入粘贴内容的上限，超过任一个时插入占位。
	pasteLineLimit = 10
	pasteRuneLimit = 1000
	// composerContentLineLimit 是输入框内容的显示行数上限（textarea 的 MaxContentHeight，软折行也计入）；
	// 可见高度另由 MaxHeight 控制，超出时在输入框内滚动。
	composerContentLineLimit = 100000
)

var (
	// pastePlaceholderPattern 匹配粘贴占位 "[粘贴 #n · L 行]"。
	pastePlaceholderPattern = regexp.MustCompile(`\[粘贴 #(\d+) · \d+ 行\]`)
	// pastePlaceholderSuffixPattern 只匹配文本末尾的一个占位，用来找紧挨在光标前的占位。
	pastePlaceholderSuffixPattern = regexp.MustCompile(`\[粘贴 #(\d+) · \d+ 行\]$`)
)

// newComposer 创建可编辑多行、支持粘贴占位的输入框。
func newComposer(styles tuiStyles) *composer {
	// 1. 创建输入框，设置提示符和动态高度。
	input := textarea.New()
	input.ShowLineNumbers = false
	input.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return "› "
		}
		return "  "
	})
	input.DynamicHeight = true
	input.MinHeight = 1
	input.MaxContentHeight = composerContentLineLimit
	// 2. 换行键改为 Shift+Enter 和 Ctrl+J，Enter 留给 tuiModel 发送；关闭 textarea 的 Ctrl+V 剪贴板读取（粘贴走 tea.PasteMsg），
	// 使用终端真实光标并设置样式。
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"))
	input.KeyMap.Paste.SetEnabled(false)
	input.SetVirtualCursor(false)
	input.SetStyles(newComposerStyles(styles))
	// 3. 保存输入框并初始化粘贴原文表。
	return &composer{
		textarea: input,
		pastes:   map[int]string{},
	}
}

// newComposerStyles 生成输入框样式。
func newComposerStyles(styles tuiStyles) textarea.Styles {
	// 1. 设置整块输入底色、正文、强调色提示符和占位提示样式。
	plain := lipgloss.NewStyle()
	state := textarea.StyleState{
		Base:             styles.surface,
		Text:             plain,
		LineNumber:       plain,
		CursorLineNumber: plain,
		CursorLine:       plain,
		EndOfBuffer:      plain,
		Placeholder:      styles.faint,
		Prompt:           styles.accent,
	}
	// 2. 为有焦点和无焦点状态使用相同样式，并设置竖线光标。
	return textarea.Styles{
		Focused: state,
		Blurred: state,
		Cursor:  textarea.CursorStyle{Shape: tea.CursorBar},
	}
}

// focus 让输入框获得焦点。
func (c *composer) focus() tea.Cmd {
	// 1. 更新焦点并返回光标命令。
	return c.textarea.Focus()
}

// setSize 设置输入框宽度和最多可见行数。
func (c *composer) setSize(width, maxHeight int) {
	// 1. 设置高度上限和显示宽度。
	c.textarea.MaxHeight = maxHeight
	c.textarea.SetWidth(width)
}

// getValue 返回输入框原文。
func (c *composer) getValue() string {
	// 1. 读取输入框中的文字。
	return c.textarea.Value()
}

// setValue 替换输入框内容并刷新显示。
func (c *composer) setValue(text string) {
	// 1. 替换文字，将光标移到末尾并刷新视口。
	c.textarea.SetValue(text)
	c.refreshViewport()
}

// refreshViewport 刷新输入框视口并滚到光标位置。
func (c *composer) refreshViewport() {
	// 1. 用空消息跑一次 textarea.Update，按现有内容重算视口。
	c.textarea, _ = c.textarea.Update(nil)
}

// isEmpty 输入框是否没有任何内容？
func (c *composer) isEmpty() bool {
	// 1. 检查原文是否为空。
	return c.textarea.Value() == ""
}

// isBlank 输入框是否为空或只有空白？
func (c *composer) isBlank() bool {
	// 1. 去掉首尾空白后检查是否为空。
	return strings.TrimSpace(c.textarea.Value()) == ""
}

// getHeight 返回输入框的可见行数。
func (c *composer) getHeight() int {
	// 1. 读取当前高度。
	return c.textarea.Height()
}

// view 返回输入框显示内容。
func (c *composer) view() string {
	// 1. 渲染输入框。
	return c.textarea.View()
}

// getCursor 返回输入框内的光标位置。
func (c *composer) getCursor() *tea.Cursor {
	// 1. 读取相对输入框左上角的光标，无焦点时返回 nil。
	return c.textarea.Cursor()
}

// handleKey 处理粘贴占位删除、输入历史和文字编辑。
func (c *composer) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	// 1. Backspace 先尝试整段删除光标前的粘贴占位，↑ ↓ 先尝试切换历史；处理成功就不再交给 textarea。
	switch msg.String() {
	case "backspace":
		if c.deletePastePlaceholder() {
			return nil
		}
	case "up":
		// 软折行文字只有在第一个显示行按 ↑ 才切换历史。
		if c.textarea.Line() == 0 && c.textarea.LineInfo().RowOffset == 0 && c.showPreviousHistory() {
			return nil
		}
	case "down":
		// 软折行文字只有在最后一个显示行按 ↓ 才切换历史。
		info := c.textarea.LineInfo()
		if c.textarea.Line() == c.textarea.LineCount()-1 && info.RowOffset == info.Height-1 && c.showNextHistory() {
			return nil
		}
	}
	// 2. 其余按键交给 textarea 编辑文字，返回它的更新命令。
	var cmd tea.Cmd
	c.textarea, cmd = c.textarea.Update(msg)
	return cmd
}

// handlePaste 清理粘贴文本并插入输入框。
func (c *composer) handlePaste(content string) {
	// 1. 保留换行和缩进，去掉控制字符；空文本直接返回。
	text := normalizePastedText(content)
	if text == "" {
		return
	}
	// 2. 超过行数或字符数上限时保存原文，生成占位文字。
	lineCount := strings.Count(text, "\n") + 1
	if lineCount > pasteLineLimit || utf8.RuneCountInString(text) > pasteRuneLimit {
		c.lastPasteID++
		c.pastes[c.lastPasteID] = text
		text = fmt.Sprintf("[粘贴 #%d · %d 行]", c.lastPasteID, lineCount)
	}
	// 3. 在光标处插入文字并刷新视口。
	c.textarea.InsertString(text)
	c.refreshViewport()
}

// takeSubmission 取出发送原文，记录历史并清空输入框。
func (c *composer) takeSubmission() string {
	// 1. 展开已登记的粘贴占位。
	text := c.expandPastes(c.textarea.Value())
	// 2. 记录输入历史，清空输入框后返回原文。
	c.appendHistory(text)
	c.clear()
	return text
}

// clear 清空输入框和粘贴占位，回到草稿编辑。
func (c *composer) clear() {
	// 1. 重置文字、占位表和历史浏览位置。
	c.textarea.Reset()
	clear(c.pastes)
	c.historyIndex, c.historyDraft = len(c.history), ""
}

// restore 把未发送原文放回输入框。
func (c *composer) restore(text string) {
	// 1. 输入框里已有新内容时，原文放前面、换行后接上新内容，再替换输入框内容。
	if current := c.textarea.Value(); current != "" {
		text += "\n" + current
	}
	c.setValue(text)
}

// setHistory 用已保存的用户输入替换历史。
func (c *composer) setHistory(inputs []string) {
	// 1. 复制历史列表（旧的在前），回到草稿编辑位置并清空历史草稿。
	c.history = slices.Clone(inputs)
	c.historyIndex, c.historyDraft = len(c.history), ""
}

// appendHistory 把发送的输入追加到历史。
func (c *composer) appendHistory(text string) {
	// 1. 输入与最后一条不同时追加，连续相同的输入只记一次，再回到草稿编辑位置。
	if count := len(c.history); count == 0 || c.history[count-1] != text {
		c.history = append(c.history, text)
	}
	c.historyIndex, c.historyDraft = len(c.history), ""
}

// showPreviousHistory 在输入框显示上一条历史输入，返回是否切换了。
func (c *composer) showPreviousHistory() bool {
	// 1. 已到最早一条时不切换；刚开始浏览历史时先保存输入框草稿。
	if c.historyIndex == 0 {
		return false
	}
	if c.historyIndex == len(c.history) {
		c.historyDraft = c.textarea.Value()
	}
	// 2. 往前移一条并显示。
	c.historyIndex--
	c.setValue(c.history[c.historyIndex])
	return true
}

// showNextHistory 在输入框显示下一条历史输入，越过最新一条时放回草稿；返回是否切换了。
func (c *composer) showNextHistory() bool {
	// 1. 没在浏览历史时不切换，否则往后移一条。
	if c.historyIndex >= len(c.history) {
		return false
	}
	c.historyIndex++
	// 2. 显示这一条历史；越过最新一条时显示草稿。
	text := c.historyDraft
	if c.historyIndex < len(c.history) {
		text = c.history[c.historyIndex]
	}
	c.setValue(text)
	return true
}

// queuePendingSend 把草稿上按 Enter 的输入排到待发送队尾。
func (c *composer) queuePendingSend(text string) {
	// 1. 把原文追加到队尾。
	c.pendingSends = append(c.pendingSends, text)
}

// takeNextPendingSend 取出最早的待发送输入，返回是否取到。
func (c *composer) takeNextPendingSend() (string, bool) {
	// 1. 队列为空时直接返回；否则移除并返回队首。
	if len(c.pendingSends) == 0 {
		return "", false
	}
	text := c.pendingSends[0]
	c.pendingSends = c.pendingSends[1:]
	return text, true
}

// restorePendingSends 清空待发送队列，把没发出去的输入按顺序放回输入框。
func (c *composer) restorePendingSends(failed string) {
	// 1. 取出并清空队列；发送失败的那条已经出队，放回最前面。
	texts := c.pendingSends
	c.pendingSends = nil
	if failed != "" {
		texts = append([]string{failed}, texts...)
	}
	// 2. 把待恢复输入按换行拼接后放回输入框。
	if len(texts) > 0 {
		c.restore(strings.Join(texts, "\n"))
	}
}

// expandPastes 展开文本中的粘贴占位。
func (c *composer) expandPastes(text string) string {
	// 1. 按占位编号查找原文并逐个替换，未登记的占位保留。
	return pastePlaceholderPattern.ReplaceAllStringFunc(text, func(placeholder string) string {
		id, _ := strconv.Atoi(pastePlaceholderPattern.FindStringSubmatch(placeholder)[1])
		if content, found := c.pastes[id]; found {
			return content
		}
		return placeholder
	})
}

// deletePastePlaceholder 整段删除紧挨在光标前的已登记粘贴占位，返回是否删除了。
func (c *composer) deletePastePlaceholder() bool {
	// 1. 读取光标所在行，匹配紧挨在光标前的完整占位；没有占位时不删除。
	lines := strings.Split(c.textarea.Value(), "\n")
	row, column := c.textarea.Line(), c.textarea.Column()
	runes := []rune(lines[row])
	before, after := string(runes[:column]), string(runes[column:])
	match := pastePlaceholderSuffixPattern.FindStringSubmatchIndex(before)
	if match == nil {
		return false
	}
	// 2. 占位编号没登记时不删除。
	id, _ := strconv.Atoi(before[match[2]:match[3]])
	if _, found := c.pastes[id]; !found {
		return false
	}
	// 3. 删除登记和占位文字，把光标放到占位起点。
	delete(c.pastes, id)
	lines[row] = before[:match[0]] + after
	c.setValueWithCursor(strings.Join(lines, "\n"), row, utf8.RuneCountInString(before[:match[0]]))
	return true
}

// setValueWithCursor 替换文字并把光标移到指定行和字符列。
func (c *composer) setValueWithCursor(text string, row, column int) {
	// 1. 替换内容，回到开头并向下移动到指定行。
	c.textarea.SetValue(text)
	c.textarea.MoveToBegin()
	for c.textarea.Line() < row {
		c.textarea.CursorDown()
	}
	// 2. 设置字符列并刷新视口。
	c.textarea.SetCursorColumn(column)
	c.refreshViewport()
}

// getSlashToken 找光标所在、以 / 开头的词，返回它在全文中的起止字符位置、/ 到光标之间的文字，以及是否找到。
func (c *composer) getSlashToken() (start, end int, prefix string, ok bool) {
	// 1. 把光标所在行列换算为全文字符位置。
	text := []rune(c.getValue())
	cursor := c.textarea.Column()
	for _, line := range strings.Split(c.getValue(), "\n")[:c.textarea.Line()] {
		cursor += utf8.RuneCountInString(line) + 1
	}
	// 2. 从光标向两侧找到空白为止定出词的范围；光标在词首或词不以 / 开头时没有斜杠词。
	start, end = cursor, cursor
	for start > 0 && !unicode.IsSpace(text[start-1]) {
		start--
	}
	for end < len(text) && !unicode.IsSpace(text[end]) {
		end++
	}
	if start == cursor || text[start] != '/' {
		return start, end, "", false
	}
	return start, end, string(text[start+1 : cursor]), true
}

// replaceSlashToken 替换选中的斜杠词并把光标放在新内容后。
func (c *composer) replaceSlashToken(start, end int, replacement string) {
	// 1. 拼接替换前后文字，计算插入末尾的行列并更新输入框。
	text := []rune(c.getValue())
	before := string(text[:start]) + replacement
	row := strings.Count(before, "\n")
	column := utf8.RuneCountInString(before[strings.LastIndex(before, "\n")+1:])
	c.setValueWithCursor(before+string(text[end:]), row, column)
}
