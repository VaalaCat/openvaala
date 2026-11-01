package tui

import (
	"image/color"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestComposer 检查输入编辑、粘贴占位、历史和待发送队列。
func TestComposer(t *testing.T) {
	// 1. 准备编辑、粘贴、历史和待发送队列场景。
	bigPaste := strings.Repeat("line\n", 11) + "last"
	tests := []struct {
		name string
		run  func(t *testing.T, c *composer)
	}{
		{
			name: "shift_enter_and_ctrl_j_insert_newline",
			run: func(t *testing.T, c *composer) {
				typeInto(c, "a")
				c.handleKey(newKeyPress("shift+enter"))
				typeInto(c, "b")
				c.handleKey(newKeyPress("ctrl+j"))
				typeInto(c, "c")
				requireComposerValue(t, c, "a\nb\nc")
			},
		},
		{
			name: "paste_keeps_newlines_and_indent",
			run: func(t *testing.T, c *composer) {
				c.handlePaste("x\r\n\tindented\x07\r\ny")
				requireComposerValue(t, c, "x\n    indented\ny")
			},
		},
		{
			name: "large_paste_inserts_placeholder_and_expands_on_submit",
			run: func(t *testing.T, c *composer) {
				typeInto(c, "看：")
				c.handlePaste(bigPaste)
				requireComposerValue(t, c, "看：[粘贴 #1 · 12 行]")
				if got := c.takeSubmission(); got != "看："+bigPaste {
					t.Fatalf("submission = %q", got)
				}
				requireComposerValue(t, c, "")
			},
		},
		{
			name: "long_single_line_paste_uses_placeholder",
			run: func(t *testing.T, c *composer) {
				c.handlePaste(strings.Repeat("字", 1001))
				requireComposerValue(t, c, "[粘贴 #1 · 1 行]")
			},
		},
		{
			name: "backspace_deletes_whole_placeholder",
			run: func(t *testing.T, c *composer) {
				c.handlePaste(bigPaste)
				typeInto(c, "z")
				c.handleKey(newKeyPress("backspace"))
				requireComposerValue(t, c, "[粘贴 #1 · 12 行]")
				c.handleKey(newKeyPress("backspace"))
				requireComposerValue(t, c, "")
				if len(c.pastes) != 0 {
					t.Fatalf("pastes = %v", c.pastes)
				}
			},
		},
		{
			name: "backspace_on_typed_lookalike_deletes_one_character",
			run: func(t *testing.T, c *composer) {
				typeInto(c, "[粘贴 #9 · 1 行]")
				c.handleKey(newKeyPress("backspace"))
				requireComposerValue(t, c, "[粘贴 #9 · 1 行")
			},
		},
		{
			name: "history_up_and_down_restore_draft",
			run: func(t *testing.T, c *composer) {
				c.setHistory([]string{"一", "二"})
				typeInto(c, "草稿")
				c.handleKey(newKeyPress("up"))
				requireComposerValue(t, c, "二")
				c.handleKey(newKeyPress("up"))
				requireComposerValue(t, c, "一")
				c.handleKey(newKeyPress("up"))
				requireComposerValue(t, c, "一")
				c.handleKey(newKeyPress("down"))
				requireComposerValue(t, c, "二")
				c.handleKey(newKeyPress("down"))
				requireComposerValue(t, c, "草稿")
			},
		},
		{
			name: "up_inside_multiline_moves_cursor",
			run: func(t *testing.T, c *composer) {
				c.setHistory([]string{"旧"})
				c.handlePaste("a\nb")
				c.handleKey(newKeyPress("up"))
				requireComposerValue(t, c, "a\nb")
				if c.textarea.Line() != 0 {
					t.Fatalf("cursor line = %d, want 0", c.textarea.Line())
				}
			},
		},
		{
			// ↑ 只在光标位于第一个显示行时翻历史：软折行的长段落里，光标在最后一个显示行时先在输入框内上移。
			name: "up_inside_wrapped_line_moves_cursor_before_history",
			run: func(t *testing.T, c *composer) {
				c.setSize(20, 8)
				c.setHistory([]string{"旧"})
				long := strings.Repeat("x", 48)
				typeInto(c, long)
				before := c.textarea.LineInfo()
				if before.Height < 3 || before.RowOffset != before.Height-1 {
					t.Fatalf("line info = %+v, want the cursor on the last of at least 3 rows", before)
				}
				c.handleKey(newKeyPress("up"))
				requireComposerValue(t, c, long)
				if after := c.textarea.LineInfo(); after.RowOffset != before.RowOffset-1 {
					t.Fatalf("row offset = %d, want %d", after.RowOffset, before.RowOffset-1)
				}
			},
		},
		{
			// ↓ 同理：浏览历史时，光标不在最后一个显示行就先在输入框内下移，到了最后一个显示行才换下一条。
			name: "down_inside_wrapped_history_entry_moves_cursor_before_next_history",
			run: func(t *testing.T, c *composer) {
				c.setSize(20, 8)
				long := strings.Repeat("x", 48)
				c.setHistory([]string{long, "后"})
				c.handleKey(newKeyPress("up"))
				c.handleKey(newKeyPress("up"))
				requireComposerValue(t, c, long)
				last := c.textarea.LineInfo().RowOffset
				c.handleKey(newKeyPress("up"))
				requireComposerValue(t, c, long)
				if row := c.textarea.LineInfo().RowOffset; row != last-1 {
					t.Fatalf("row offset after up = %d, want %d", row, last-1)
				}
				c.handleKey(newKeyPress("down"))
				requireComposerValue(t, c, long)
				if row := c.textarea.LineInfo().RowOffset; row != last {
					t.Fatalf("row offset after down = %d, want %d", row, last)
				}
				c.handleKey(newKeyPress("down"))
				requireComposerValue(t, c, "后")
			},
		},
		{
			// Ctrl+V 不读取剪贴板；粘贴只处理终端括号粘贴消息。
			name: "ctrl_v_does_not_read_the_clipboard",
			run: func(t *testing.T, c *composer) {
				if cmd := c.handleKey(newKeyPress("ctrl+v")); cmd != nil {
					t.Fatal("ctrl+v returned a command")
				}
				requireComposerValue(t, c, "")
			},
		},
		{
			name: "submission_appends_history_without_duplicates",
			run: func(t *testing.T, c *composer) {
				typeInto(c, "同一句")
				c.takeSubmission()
				typeInto(c, "同一句")
				c.takeSubmission()
				if len(c.history) != 1 {
					t.Fatalf("history = %v", c.history)
				}
			},
		},
		{
			name: "restore_puts_original_before_new_text",
			run: func(t *testing.T, c *composer) {
				c.restore("原文")
				requireComposerValue(t, c, "原文")
				c.clear()
				typeInto(c, "新")
				c.restore("原文")
				requireComposerValue(t, c, "原文\n新")
			},
		},
		{
			name: "pending_sends_restore_in_order",
			run: func(t *testing.T, c *composer) {
				c.queuePendingSend("一")
				c.queuePendingSend("二")
				c.queuePendingSend("三")
				first, found := c.takeNextPendingSend()
				if !found || first != "一" {
					t.Fatalf("first = %q %v", first, found)
				}
				c.restorePendingSends(first)
				requireComposerValue(t, c, "一\n二\n三")
				if _, found := c.takeNextPendingSend(); found {
					t.Fatal("queue not cleared")
				}
			},
		},
		{
			// 可见高度为 8，粘贴 10 行后检查光标仍在输入框内。
			name: "paste_scrolls_view_to_cursor",
			run: func(t *testing.T, c *composer) {
				c.handlePaste(strings.TrimSuffix(strings.Repeat("l\n", pasteLineLimit), "\n"))
				if cursor := c.getCursor(); cursor == nil || cursor.Y >= c.getHeight() {
					t.Fatalf("cursor = %+v, composer height = %d", cursor, c.getHeight())
				}
			},
		},
		{
			name: "blank_and_empty",
			run: func(t *testing.T, c *composer) {
				if !c.isEmpty() || !c.isBlank() {
					t.Fatal("new composer is not empty")
				}
				typeInto(c, "  ")
				if c.isEmpty() || !c.isBlank() {
					t.Fatalf("spaces: empty = %v, blank = %v", c.isEmpty(), c.isBlank())
				}
			},
		},
	}
	// 2. 每个场景用一个新的、已获得焦点的 80 宽、最多 8 行的输入框运行。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newComposer(newTUIStyles(markdownStyleNoTTY, nil))
			c.focus()
			c.setSize(80, 8)
			tt.run(t, c)
		})
	}
}

// TestNewComposerStyles 检查输入框宽度和整行底色。
func TestNewComposerStyles(t *testing.T) {
	// 1. 准备有底色、无底色和软折行内容。
	tests := []struct {
		name       string
		background color.Color
		content    string
	}{
		{
			name:       "empty_on_surface",
			background: lipgloss.Color("#1e1e1e"),
		},
		{
			name:       "text_on_surface",
			background: lipgloss.Color("#1e1e1e"),
			content:    "hello",
		},
		{
			name:       "wrapped_lines_on_surface",
			background: lipgloss.Color("#1e1e1e"),
			content:    strings.Repeat("很长的输入", 8) + "\n\n第三行",
		},
		{
			name:    "no_background",
			content: "hello\n第二行",
		},
	}
	// 2. 生成输入框，检查每行宽度和每个字符的底色。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			styles := newTUIStyles("dark", tt.background)
			c := newComposer(styles)
			c.focus()
			c.setSize(30, 8)
			c.setValue(tt.content)
			// 输入框整块（含提示符与续行）铺满 surface 底色；没有底色时不输出任何背景。
			for _, line := range strings.Split(c.view(), "\n") {
				if width := ansi.StringWidth(line); width != 30 {
					t.Fatalf("line %q width %d, want 30", line, width)
				}
				requireCellsOnBackground(t, line, styles.surface.GetBackground())
			}
		})
	}
}

// sgrPattern 匹配 SGR 样式序列。
var sgrPattern = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// requireCellsOnBackground 检查一行里每个可见字符都铺着指定底色；没有底色配置时检查整行不设背景。
func requireCellsOnBackground(t *testing.T, line string, background color.Color) {
	// 1. 无底色配置时检查整行没有背景序列。
	t.Helper()
	if _, isNoColor := background.(lipgloss.NoColor); isNoColor {
		for _, sequence := range sgrPattern.FindAllString(line, -1) {
			if strings.Contains(sequence, "48;") {
				t.Fatalf("line sets a background without one configured: %q", line)
			}
		}
		return
	}
	// 2. 读取目标背景的 SGR 参数，逐段跟踪背景是否开着；背景关着时出现可见字符就失败。
	open := lipgloss.NewStyle().Background(background).Render(" ")
	parameters := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(open, " "+ansi.ResetStyle), "\x1b["), "m")
	isOpen, position := false, 0
	for _, bounds := range sgrPattern.FindAllStringIndex(line, -1) {
		if position < bounds[0] && !isOpen {
			t.Fatalf("%q is outside the background in %q", line[position:bounds[0]], line)
		}
		sequence := line[bounds[0]:bounds[1]]
		switch {
		case sequence == ansi.ResetStyle || sequence == "\x1b[0m":
			isOpen = false
		case strings.Contains(sequence, parameters):
			isOpen = true
		}
		position = bounds[1]
	}
	// 3. 检查最后一个样式序列后是否还有未覆盖字符。
	if position < len(line) {
		t.Fatalf("%q is outside the background in %q", line[position:], line)
	}
}

// typeInto 逐字符向输入框发送文字按键。
func typeInto(c *composer, text string) {
	// 1. 把每个字符作为按键交给输入框。
	for _, r := range text {
		c.handleKey(tea.KeyPressMsg{
			Code: r,
			Text: string(r),
		})
	}
}

// requireComposerValue 检查输入框原文等于预期。
func requireComposerValue(t *testing.T, c *composer, want string) {
	// 1. 读取输入内容并比较。
	t.Helper()
	if got := c.getValue(); got != want {
		t.Fatalf("composer = %q, want %q", got, want)
	}
}
