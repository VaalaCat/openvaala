package tui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/charmbracelet/x/ansi"
)

// TestExpandViewContent 检查展开历史的原始顺序、分隔和丢弃回复显示。
func TestExpandViewContent(t *testing.T) {
	// 1. 准备输入显示行和历史事件样例。
	// 展开视图里用户输入的每一行补满视图宽度（没有底色时只补空格）。
	userLine := padRight("› 看看", 60)
	tests := []struct {
		name   string
		header string
		feed   func(tr *transcript)
		want   []string
	}{
		{
			name:   "steps_expand_in_original_order",
			header: "── 标题 · c1 ──",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(
					userItem("u1", "看看"),
					reasoningItem("x1", "Let me look.", defs.ReasoningCompleted),
					toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`),
					toolResultItem("c2", "call-1", nil),
					reasoningItem("x2", "", defs.ReasoningCompleted),
					replyItem("r1", "结论"),
				))
			},
			want: []string{
				"── 标题 · c1 ──", "",
				userLine, "",
				"∴ Let me look.", "✓ bash  ls", "  参数", "  {", `    "command": "ls"`, "  }", "", "  输出", "  {", `    "ok": true`, "  }",
				"∴ （加密推理）", "",
				"● 结论",
			},
		},
		{
			// 两个 Turn 之间是一行铺满视图宽度的分隔线，前后各一个空行；同一 Turn 的插话之前没有分隔线。
			name:   "turns_are_separated_by_divider",
			header: "── h ──",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(
					userItem("u1", "看看"),
					replyItem("r1", "好"),
					withTurn(userItem("u2", "插话"), "t1"),
					withTurn(userItem("u3", "第二轮"), "t2"),
					withTurn(replyItem("r2", "也好"), "t2"),
				))
			},
			want: []string{
				"── h ──", "", userLine, "", "● 好", "", padRight("› 插话", 60), "",
				strings.Repeat("─", 60), "", padRight("› 第二轮", 60), "", "● 也好",
			},
		},
		{
			name: "empty_history_shows_hint",
			feed: func(*transcript) {},
			want: []string{"还没有内容"},
		},
		{
			name:   "single_line_notice_wraps_instead_of_truncating",
			header: "── h ──",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(&defs.Item{
					ID:         "x1",
					TurnID:     "t1",
					Kind:       defs.ItemModelError,
					ModelError: &defs.ModelError{Message: strings.Repeat("错", 40)},
				}))
			},
			want: []string{"── h ──", "", "✗ 模型错误：" + strings.Repeat("错", 24), strings.Repeat("错", 16)},
		},
		{
			// 第一段写进 scrollback 之前就被中断且不保存的回复不算写进历史，展开视图里也不显示。
			name:   "discarded_reply_never_printed_is_hidden",
			header: "── h ──",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(userItem("u1", "看看")))
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, "标题还在实时区域"))
				tr.handleItemChanged(itemInterrupted(0))
			},
			want: []string{"── h ──", "", userLine},
		},
		{
			// 已经写出过段落的被丢弃回复保留文字，后面跟着"未保存"那一行。
			name:   "discarded_reply_after_print_keeps_text",
			header: "── h ──",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(userItem("u1", "看看")))
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, "第一段\n\n第二\n"))
				tr.handleItemChanged(itemInterrupted(0))
			},
			want: []string{"── h ──", "", userLine, "", "● 第一段", "", "  第二", "", "已中断 · 以上内容未保存"},
		},
		{
			// 重连同步丢掉既没保存、也不在同步结果里的回复：没写出过时同样不显示。
			name:   "reply_dropped_by_sync_never_printed_is_hidden",
			header: "── h ──",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(userItem("u1", "看看")))
				tr.handleItemChanged(itemStarted(0, replyItem("r1", "")))
				tr.handleItemChanged(itemDelta(0, "半句"))
				tr.handleSessionSynced(&runtimeclient.SessionSynced{}, "")
			},
			want: []string{"── h ──", "", userLine},
		},
	}
	// 2. 写入各场景历史并比较完整展开内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTestTranscript()
			tt.feed(tr)
			requirePrinted(t, newExpandView().getContentLines(tr, tt.header), tt.want...)
		})
	}
}

// TestExpandViewHookRun 检查 hook 的事件、命令、状态和输出展开。
func TestExpandViewHookRun(t *testing.T) {
	// 1. 准备用户输入和不同 hook 运行结果。
	userLine := padRight("› 看看", 60)
	tests := []struct {
		name string
		feed func(tr *transcript)
		want []string
	}{
		{
			// 主视图隐藏的完成记录在展开视图里按位置显示：事件、工具名、状态、Executor、处理命令和输出。
			name: "completed_run_hidden_in_main_view_shows_event_handler_and_outputs",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(
					userItem("u1", "看看"),
					toolCallItem("c1", "call-1", "bash", `{"command":"ls"}`),
					hookRunItem("h1", "t1", &defs.HookRun{
						Event:                defs.HookEventPreToolUse,
						CallID:               "call-1",
						ExecutorID:           "local",
						Handler:              "./audit.sh --strict",
						Status:               defs.HookRunCompleted,
						UpdatedToolArguments: `{"command":"ls -l"}`,
						AdditionalContext:    "已审计\n\n无风险",
					}),
					toolResultItem("c2", "call-1", nil),
				))
			},
			want: []string{
				userLine, "",
				// 工具组在首次调用时登记进历史，同组 hook 记录显示在整组展开之后。
				"✓ bash  ls", "  参数", "  {", `    "command": "ls"`, "  }", "", "  输出", "  {", `    "ok": true`, "  }", "",
				"hook PreToolUse bash · 完成 · local · ./audit.sh --strict",
				`  更新后的工具参数：{"command":"ls -l"}`, "  附加上下文：已审计", "", "  无风险",
			},
		},
		{
			// 主视图写了提示行的运行在展开视图里仍完整显示，且只出现一次。
			name: "blocked_run_shown_in_main_view_is_not_duplicated",
			feed: func(tr *transcript) {
				printed := tr.handleItemsSaved(logItems(
					userItem("u1", "看看"),
					hookRunItem("h1", "t1", &defs.HookRun{
						Event:         defs.HookEventUserPromptSubmit,
						ExecutorID:    "local",
						Handler:       "python3 /h/gate.py",
						Status:        defs.HookRunBlocked,
						BlockReason:   "缺少工单号",
						SystemMessage: "已记入日志",
					}),
				))
				requirePrinted(t, printed, "› 看看", "", "hook UserPromptSubmit 拦下：缺少工单号", "", "hook UserPromptSubmit：已记入日志", "")
			},
			want: []string{
				userLine, "",
				"hook UserPromptSubmit · 拦下 · local · python3 /h/gate.py", "  拦下原因：缺少工单号", "  系统消息：已记入日志",
			},
		},
		{
			name: "failed_run_shows_error_and_stopped_run_shows_reason",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(
					userItem("u1", "看看"),
					hookRunItem("h1", "t1", &defs.HookRun{
						Event:        defs.HookEventSessionStart,
						ExecutorID:   "gpu",
						Handler:      "/h/start.sh",
						Status:       defs.HookRunFailed,
						ErrorMessage: "timed out after 60s",
					}),
					hookRunItem("h2", "t1", &defs.HookRun{
						Event:      defs.HookEventStop,
						ExecutorID: "gpu",
						Handler:    "/h/stop.sh",
						Status:     defs.HookRunStopped,
						StopReason: "预算用完",
					}),
				))
			},
			want: []string{
				userLine, "",
				"hook SessionStart · 出错 · gpu · /h/start.sh", "  错误：timed out after 60s", "",
				"hook Stop · 结束本轮 · gpu · /h/stop.sh", "  结束原因：预算用完",
			},
		},
		{
			// 主视图隐藏的运行是新 Turn 的第一个单元时，Turn 分隔线排在它前面，而不是落到它和后面的输入之间。
			name: "hidden_run_of_new_turn_follows_divider",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(
					withTurn(replyItem("r1", "好"), "t0"),
					hookRunItem("h1", "t1", &defs.HookRun{
						Event:             defs.HookEventSessionStart,
						ExecutorID:        "local",
						Handler:           "/h/start.sh",
						Status:            defs.HookRunCompleted,
						AdditionalContext: "背景",
					}),
					userItem("u1", "看看"),
				))
			},
			want: []string{
				"● 好", "",
				strings.Repeat("─", 60), "",
				"hook SessionStart · 完成 · local · /h/start.sh", "  附加上下文：背景", "",
				userLine,
			},
		},
		{
			// 处理命令太长时按宽度折行，输出的续行同样缩进两格；原文里的终端控制先去掉。
			name: "long_handler_and_output_wrap_and_controls_are_stripped",
			feed: func(tr *transcript) {
				tr.handleItemsSaved(logItems(
					hookRunItem("h1", "t1", &defs.HookRun{
						Event:       defs.HookEventStop,
						ExecutorID:  "local",
						Handler:     "/very/long/path/" + strings.Repeat("x", 40) + ".sh",
						Status:      defs.HookRunBlocked,
						BlockReason: strings.Repeat("错", 40) + "\x1b[31m",
					}),
				))
			},
			want: []string{
				"hook Stop · 拦下 · local ·",
				"/very/long/path/" + strings.Repeat("x", 40) + ".sh",
				"  拦下原因：" + strings.Repeat("错", 24),
				"  " + strings.Repeat("错", 16),
			},
		},
	}
	// 2. 保存运行记录，比较展开顺序、输出和折行。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTestTranscript()
			tt.feed(tr)
			requirePrinted(t, newExpandView().getContentLines(tr, ""), tt.want...)
		})
	}
}

// TestExpandViewHookRunStyle 检查 hook 展开内容的样式。
func TestExpandViewHookRunStyle(t *testing.T) {
	// 1. 分别生成失败与完成运行的展开内容，逐行检查失败使用警告色、其他运行使用暗色。
	colored := newTUIStyles("dark", nil)
	for _, tt := range []struct {
		name  string
		run   *defs.HookRun
		style lipgloss.Style
	}{
		{
			name: "failed_warning",
			run: &defs.HookRun{
				Event:        defs.HookEventStop,
				Handler:      "/h/a.sh",
				Status:       defs.HookRunFailed,
				ErrorMessage: "boom",
			},
			style: colored.warning,
		},
		{
			name: "completed_faint",
			run: &defs.HookRun{
				Event:             defs.HookEventStop,
				Handler:           "/h/a.sh",
				Status:            defs.HookRunCompleted,
				AdditionalContext: "好",
			},
			style: colored.faint,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTranscript(mustNewMarkdownRenderer(markdownStyleNoTTY, 60), colored, newSessionStatuses("s1"), make(map[defs.ModelRef]string), 10,
				func() time.Time { return testNow }, func() bool { return false }, func() string { return "" })
			tr.handleItemsSaved(logItems(hookRunItem("h1", "t1", tt.run)))
			lines := newExpandView().getContentLines(tr, "")
			if len(lines) < 2 {
				t.Fatalf("lines = %q", lines)
			}
			for _, line := range lines {
				if line != tt.style.Render(ansi.Strip(line)) {
					t.Fatalf("line %q is not styled as %q", line, tt.style.Render(ansi.Strip(line)))
				}
			}
		})
	}
}

// TestExpandViewRerendersOnWidthChange 检查宽度变化后历史重新折行。
func TestExpandViewRerendersOnWidthChange(t *testing.T) {
	// 1. 写入长输入并读取宽视图。
	tr := newTestTranscript()
	tr.handleItemsSaved(logItems(userItem("u1", strings.Repeat("长", 50))))
	view := newExpandView()
	wide := view.getContentLines(tr, "")
	// 2. 切换到窄渲染器，检查行数增加且没有超宽。
	tr.setRenderer(mustNewMarkdownRenderer(markdownStyleNoTTY, 30), 10)
	narrow := view.getContentLines(tr, "")
	if len(narrow) <= len(wide) {
		t.Fatalf("width change reused stale lines: wide %d, narrow %d", len(wide), len(narrow))
	}
	for _, line := range narrow {
		if ansi.StringWidth(line) > 30 {
			t.Fatalf("line wider than 30: %q", line)
		}
	}
}

// TestExpandViewBackgrounds 检查展开输入与补丁增删行的整行底色。
func TestExpandViewBackgrounds(t *testing.T) {
	// 1. 写入多行用户输入和补丁调用，按去样式文字收集显示行。
	styles := newTUIStyles("dark", lipgloss.Color("#1e1e1e"))
	tr := newTranscript(mustNewMarkdownRenderer(markdownStyleNoTTY, 60), styles, newSessionStatuses("s1"),
		make(map[defs.ModelRef]string), 10, func() time.Time { return testNow }, func() bool { return false }, func() string { return "" })
	patch := "*** Begin Patch\n*** Update File: a.go\n@@\n ctx\n-x\n+y\n*** End Patch\n"
	tr.handleItemsSaved(logItems(
		userItem("u1", "看看\n第二行"),
		toolCallItem("c1", "call-1", "apply_patch", patchArguments(patch)),
		toolResultItem("c2", "call-1", nil),
	))
	lines := newExpandView().getContentLines(tr, "")
	byPlain := make(map[string]string)
	for _, line := range lines {
		byPlain[strings.TrimRight(ansi.Strip(line), " ")] = line
	}
	// 2. 准备各类输入、增删和上下文行的预期底色。
	tests := []struct {
		name  string
		plain string // 去掉样式与行尾空格后的内容
		// fill 是铺满 indent 列缩进之后整行的底色。
		fill *lipgloss.Style
		// indent 是行首不铺底色的缩进列数。
		indent int
		// isPlain 这一行是否不带任何样式？
		isPlain bool
	}{
		{
			name:  "user_input_first_line",
			plain: "› 看看",
			fill:  &styles.surface,
		},
		{
			name:  "user_input_continuation",
			plain: "  第二行",
			fill:  &styles.surface,
		},
		// diff 行在调用正文里缩进两格，底色铺满缩进之后的行宽。
		{
			name:   "added_line",
			plain:  "  +y",
			fill:   &styles.diffAdded,
			indent: 2,
		},
		{
			name:   "removed_line",
			plain:  "  -x",
			fill:   &styles.diffRemoved,
			indent: 2,
		},
		{
			name:    "context_line_has_no_background",
			plain:   "   ctx",
			isPlain: true,
		},
	}
	// 3. 逐行检查缩进、宽度、纯文本和底色填充。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, found := byPlain[tt.plain]
			if !found {
				t.Fatalf("line %q not found in %q", tt.plain, lines)
			}
			if tt.isPlain {
				if line != tt.plain {
					t.Fatalf("line = %q, want plain %q", line, tt.plain)
				}
				return
			}
			if !strings.HasPrefix(line, strings.Repeat(" ", tt.indent)) || ansi.StringWidth(line) != 60 {
				t.Fatalf("line %q width %d, want %d after a %d-column indent", line, ansi.StringWidth(line), 60, tt.indent)
			}
			line = line[tt.indent:]
			requireBackgroundFilled(t, line, *tt.fill)
		})
	}
}

// TestExpandViewScroll 检查逐行、翻页、定位、贴底和关闭按键。
func TestExpandViewScroll(t *testing.T) {
	// 1. 准备内容生成方法，检查默认贴底的一屏。
	content := func(count int) []string {
		lines := make([]string, count)
		for index := range lines {
			lines[index] = "line " + string(rune('a'+index))
		}
		return lines
	}
	plain := newTUIStyles(markdownStyleNoTTY, nil)
	view := newExpandView()
	// 高度 5：4 行内容加 1 行提示；默认贴底。
	requirePrinted(t, view.render(content(10), 80, 5, plain),
		"line g", "line h", "line i", "line j", "展开视图 · 7–10 / 10 行 · ↑↓ 滚动 · PgUp/PgDn 翻页 · g/G 顶/底 · Esc 返回")
	// 2. 列出按键与内容增加后的预期顶部位置。
	steps := []struct {
		key        string
		total      int
		wantTop    string
		wantClosed bool
	}{
		{
			key:     "up",
			total:   10,
			wantTop: "line f",
		},
		{
			key:     "k",
			total:   12,
			wantTop: "line e",
		}, // 离开底部后新内容不改变位置
		{
			key:     "G",
			total:   12,
			wantTop: "line i",
		},
		{
			key:     "g",
			total:   12,
			wantTop: "line a",
		},
		{
			key:     "pgdown",
			total:   12,
			wantTop: "line e",
		},
		{
			key:     "space",
			total:   12,
			wantTop: "line i",
		},
		{
			key:     "b",
			total:   12,
			wantTop: "line e",
		},
		{
			key:     "down",
			total:   12,
			wantTop: "line f",
		},
		{
			key:     "j",
			total:   12,
			wantTop: "line g",
		},
		{
			key:     "x",
			total:   12,
			wantTop: "line g",
		},
		{
			key:        "q",
			total:      12,
			wantClosed: true,
		},
	}
	// 3. 逐步操作并检查位置，再检查其他关闭键和空内容提示。
	for _, step := range steps {
		closed := view.handleKey(step.key, 5)
		if closed != step.wantClosed {
			t.Fatalf("key %q closed = %v", step.key, closed)
		}
		if closed {
			continue
		}
		if got := ansi.Strip(view.render(content(step.total), 80, 5, plain)[0]); got != step.wantTop {
			t.Fatalf("after %q top = %q, want %q", step.key, got, step.wantTop)
		}
	}
	for _, key := range []string{"esc", "ctrl+o", "ctrl+c"} {
		if !newExpandView().handleKey(key, 5) {
			t.Fatalf("%q did not close", key)
		}
	}
	if hint := ansi.Strip(newExpandView().render(nil, 80, 5, plain)[4]); !strings.HasPrefix(hint, "展开视图 · 0–0 / 0 行") {
		t.Fatalf("empty hint = %q", hint)
	}
}

// TestExpandViewHintStyles 检查展开视图的位置说明、按键样式和截断。
func TestExpandViewHintStyles(t *testing.T) {
	// 1. 准备有颜色和无颜色的提示行。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	const fullHint = "展开视图 · 1–1 / 1 行 · ↑↓ 滚动 · PgUp/PgDn 翻页 · g/G 顶/底 · Esc 返回"
	tests := []struct {
		name   string
		styles tuiStyles
		width  int
		// wantPlain 是去掉样式后的提示行；wantPrefix、wantSuffix 非空时要求带样式的提示行以它们开头、结尾。
		wantPlain, wantPrefix, wantSuffix string
	}{
		{
			name:      "plain_unchanged",
			styles:    plain,
			width:     80,
			wantPlain: fullHint,
		},
		// 位置说明整段暗色，后面的按键提示按键加粗、功能暗色。
		{
			name:       "colored_position_faint_keys_bold",
			styles:     colored,
			width:      80,
			wantPlain:  fullHint,
			wantPrefix: colored.faint.Render("展开视图 · 1–1 / 1 行 · ") + colored.hintKey.Render("↑↓") + colored.faint.Render(" 滚动"),
			wantSuffix: colored.hintKey.Render("Esc") + colored.faint.Render(" 返回"),
		},
		// 宽度不足时按去样式后的显示宽度截断。
		{
			name:      "colored_truncated_like_plain",
			styles:    colored,
			width:     30,
			wantPlain: ansi.Truncate(fullHint, 30, "…"),
		},
	}
	// 2. 生成一屏，比较提示文字及样式前后缀。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := newExpandView().render([]string{"内容"}, tt.width, 3, tt.styles)
			hint := lines[len(lines)-1]
			if got := ansi.Strip(hint); got != tt.wantPlain {
				t.Fatalf("hint = %q, want %q", got, tt.wantPlain)
			}
			if !strings.HasPrefix(hint, tt.wantPrefix) || !strings.HasSuffix(hint, tt.wantSuffix) {
				t.Fatalf("hint = %q, want prefix %q and suffix %q", hint, tt.wantPrefix, tt.wantSuffix)
			}
		})
	}
}

// TestExpandView 检查 TUI 展开视图的打开、关闭和定稿输出。
func TestExpandView(t *testing.T) {
	// 1. 准备对话历史同步和全屏期间的定稿输入。
	// 打开对话并同步输入、推理、工具和回复历史，同时移除首条输入的排队记录。
	openWithHistory := func(h *tuiHarness) {
		h.openDraftConversation("开场")
		h.emit(&runtimeclient.ConversationTitleChanged{
			ConversationID: "c1",
			Title:          "开场",
		})
		h.emit(&runtimeclient.SessionSynced{
			ConversationID: "c1",
			Items: logItems(
				userItem("input-1", "开场"),
				reasoningItem("x1", "先看看目录", defs.ReasoningCompleted),
				toolCallItem("i1", "call-1", "bash", `{"command":"ls -la"}`),
				toolResultItem("i2", "call-1", nil),
				replyItem("i3", "看过了"),
			),
		})
	}
	// holdOnePrint 在展开视图打开时直接走 Update 交付一条已保存的用户输入，返回时它的定稿行应当留在 pendingPrints。
	holdOnePrint := func(t *testing.T, h *tuiHarness) {
		h.model.Update(clientEventMsg{event: &runtimeclient.ItemsSaved{
			ConversationID: "c1",
			Items: []runtimeprotocol.LoggedItem{
				{
					Ordinal: 10,
					Item:    withTurn(userItem("u2", "第二轮"), "t2"),
				},
			},
		}})
		if len(h.model.pendingPrints) == 0 {
			t.Fatal("prints were not held while the expand view was open")
		}
	}
	// 2. 列出快捷键、草稿、暂存输出、退出、尺寸变化和粘贴场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "ctrl_o_opens_full_screen_and_esc_closes",
			run: func(t *testing.T, h *tuiHarness) {
				openWithHistory(h)
				h.press("ctrl+o")
				if h.model.expandView == nil || !h.model.View().AltScreen {
					t.Fatal("ctrl+o did not open the full-screen expand view")
				}
				requireGolden(t, "expand_view_80x24", h.view())
				h.press("esc")
				if h.model.expandView != nil || h.model.View().AltScreen {
					t.Fatal("esc did not close the expand view")
				}
			},
		},
		{
			name: "slash_expand_opens_in_draft",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/expand")
				h.press("enter")
				if h.model.expandView == nil || !strings.Contains(h.view(), "还没有内容") {
					t.Fatalf("view:\n%s", h.view())
				}
			},
		},
		{
			name: "key_routing_keeps_draft_and_skips_confirmations",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.SessionStatusChanged{
					ConversationID: "c1",
					Info:           newRootInfo("t1"),
				})
				h.typeText("草稿")
				for _, key := range []string{"ctrl+c", "esc"} {
					h.press("ctrl+o")
					h.typeText("x")
					h.press(key)
					if h.model.expandView != nil || h.model.composer.getValue() != "草稿" || h.model.confirmKey != "" || h.model.isQuitting {
						t.Fatalf("after %s: open = %v, composer = %q, confirm = %q, quitting = %v",
							key, h.model.expandView != nil, h.model.composer.getValue(), h.model.confirmKey, h.model.isQuitting)
					}
				}
				if calls := h.client.getCalls("InterruptTurn"); len(calls) != 0 {
					t.Fatalf("interrupts = %v", calls)
				}
			},
		},
		{
			name: "held_prints_flush_after_close",
			run: func(t *testing.T, h *tuiHarness) {
				openWithHistory(h)
				h.press("ctrl+o")
				holdOnePrint(t, h)
				h.model.Update(clientEventMsg{event: &runtimeclient.ItemsSaved{
					ConversationID: "c1",
					Items: []runtimeprotocol.LoggedItem{
						{
							Ordinal: 11,
							Item:    withTurn(userItem("u3", "第三轮"), "t3"),
						},
					},
				}})
				// 暂存的定稿行按处理顺序排队，写出时一次拼接写出。
				held := ansi.Strip(strings.Join(h.model.pendingPrints, "\n"))
				if second, third := strings.Index(held, "› 第二轮"), strings.Index(held, "› 第三轮"); second < 0 || third < second {
					t.Fatalf("held prints out of order: %q", held)
				}
				// 关闭视图的 Update 继续暂存定稿行，收到退出全屏消息后才写出。
				_, cmd := h.model.Update(newKeyPress("esc"))
				if len(h.model.pendingPrints) == 0 || cmd == nil {
					t.Fatalf("prints written before the terminal left full screen: pending = %d, cmd nil = %v", len(h.model.pendingPrints), cmd == nil)
				}
				_, cmd = h.model.Update(expandViewLeftMsg{})
				if len(h.model.pendingPrints) != 0 || cmd == nil {
					t.Fatalf("held prints not flushed: pending = %d, cmd nil = %v", len(h.model.pendingPrints), cmd == nil)
				}
			},
		},
		{
			name: "quit_flushes_held_prints",
			run: func(t *testing.T, h *tuiHarness) {
				openWithHistory(h)
				h.press("ctrl+o")
				holdOnePrint(t, h)
				// 事件流关闭触发退出：先关闭视图等终端退出全屏，收到 expandViewLeftMsg 时写出暂存的行再退出。
				_, cmd := h.model.Update(clientEventsClosedMsg{})
				if h.model.expandView != nil || !h.model.isQuitting || cmd == nil {
					t.Fatalf("open = %v, quitting = %v", h.model.expandView != nil, h.model.isQuitting)
				}
				_, cmd = h.model.Update(expandViewLeftMsg{})
				if len(h.model.pendingPrints) != 0 || cmd == nil {
					t.Fatalf("pending = %d, cmd nil = %v after the terminal left full screen", len(h.model.pendingPrints), cmd == nil)
				}
			},
		},
		{
			name: "resize_rerenders",
			run: func(t *testing.T, h *tuiHarness) {
				openWithHistory(h)
				h.press("ctrl+o", "g")
				h.send(tea.WindowSizeMsg{
					Width:  40,
					Height: 12,
				})
				lines := strings.Split(h.view(), "\n")
				if len(lines) != 12 || !strings.HasPrefix(lines[0], "── ") {
					t.Fatalf("view after resize:\n%s", h.view())
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > 40 {
						t.Fatalf("line wider than 40: %q", line)
					}
				}
			},
		},
		{
			// 停在底部时缩小窗口，最后一行内容仍然可见。
			name: "resize_while_following_stays_at_bottom",
			run: func(t *testing.T, h *tuiHarness) {
				openWithHistory(h)
				h.press("ctrl+o")
				h.send(tea.WindowSizeMsg{
					Width:  40,
					Height: 12,
				})
				lines := strings.Split(h.view(), "\n")
				if len(lines) != 12 || lines[10] != "● 看过了" || !strings.HasPrefix(lines[11], "展开视图") {
					t.Fatalf("view after resize:\n%s", h.view())
				}
			},
		},
		{
			name: "paste_ignored_while_open",
			run: func(t *testing.T, h *tuiHarness) {
				h.press("ctrl+o")
				h.send(tea.PasteMsg{Content: "粘贴"})
				h.press("esc")
				if value := h.model.composer.getValue(); value != "" {
					t.Fatalf("composer = %q", value)
				}
			},
		},
	}
	// 3. 每个场景在一个新的 80x24 TUI 里操作，检查视图和暂存输出。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 24))
		})
	}
}

// TestFormatToolCallRow 检查工具调用标题的状态、Executor 和耗时。
func TestFormatToolCallRow(t *testing.T) {
	// 1. 准备构造 bash 调用、指定工具名的调用和补保存时间的方法。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	newCall := func(arguments string, result *defs.ToolResult, isNotExecuted bool) *toolCall {
		return &toolCall{
			call: defs.ToolCall{
				CallID:    "c1",
				Name:      "bash",
				Arguments: arguments,
			},
			result:        result,
			isNotExecuted: isNotExecuted,
		}
	}
	// newNamedCall 构造工具名为 name 的调用，没有结果时还在运行。
	newNamedCall := func(name, arguments string, result *defs.ToolResult) *toolCall {
		return &toolCall{
			call: defs.ToolCall{
				CallID:    "c1",
				Name:      name,
				Arguments: arguments,
			},
			result: result,
		}
	}
	// timed 给调用补上保存时间，行尾写耗时 3.2s。
	timed := func(call *toolCall) *toolCall {
		call.callSavedAt, call.resultSavedAt = 1000, 4200
		return call
	}
	const ls = `{"command":"ls"}`
	// 2. 列出运行、完成、失败、未执行和 MCP 名称场景。
	tests := []struct {
		name              string
		call              *toolCall
		defaultExecutorID string
		// mark 是状态标记。
		mark string
		// want 是状态标记的样式。
		want lipgloss.Style
		// text 是标记后面的正文。
		text string
	}{
		{
			name: "running_accent",
			call: newCall(ls, nil, false),
			mark: "◐",
			want: colored.accent,
			text: "bash  ls",
		},
		{
			name: "succeeded_success",
			call: newCall(ls, &defs.ToolResult{}, false),
			mark: "✓",
			want: colored.success,
			text: "bash  ls",
		},
		{
			name: "failed_failure",
			call: newCall(ls, &defs.ToolResult{Error: &defs.ToolError{Message: "boom"}}, false),
			mark: "✗",
			want: colored.failure,
			text: "bash  ls",
		},
		{
			name: "not_executed_warning",
			call: newCall(ls, nil, true),
			mark: "⊘",
			want: colored.warning,
			text: "bash  ls",
		},
		// 工具名后接 @Executor：有结果取结果记录的，还没有结果取参数、再取默认 Executor。
		{
			name: "result_executor",
			call: timed(newCall(`{"command":"pytest -x"}`, &defs.ToolResult{Executor: "gpu"}, false)),
			mark: "✓",
			want: colored.success,
			text: "bash@gpu  pytest -x  3.2s",
		},
		{
			name:              "old_result_stays_bare_with_default_executor",
			call:              newCall(ls, &defs.ToolResult{}, false),
			defaultExecutorID: "gpu",
			mark:              "✓",
			want:              colored.success,
			text:              "bash  ls",
		},
		{
			name:              "running_uses_argument_executor",
			call:              newCall(`{"command":"ls","executor":"sandbox"}`, nil, false),
			defaultExecutorID: "gpu",
			mark:              "◐",
			want:              colored.accent,
			text:              "bash@sandbox  ls",
		},
		{
			name:              "running_uses_default_executor",
			call:              newCall(ls, nil, false),
			defaultExecutorID: "gpu",
			mark:              "◐",
			want:              colored.accent,
			text:              "bash@gpu  ls",
		},
		// MCP 工具的模型名 mcp__<server>__<工具> 显示成 "server · 工具"，后面照常接 @Executor。
		{
			name: "mcp_tool_shows_server_and_tool",
			call: newNamedCall("mcp__fake__echo", `{"text":"hi"}`, nil),
			mark: "◐",
			want: colored.accent,
			text: "fake · echo  hi",
		},
		{
			name: "mcp_tool_keeps_executor",
			call: newNamedCall("mcp__fake__echo", `{"text":"hi"}`, &defs.ToolResult{Executor: "gpu"}),
			mark: "✓",
			want: colored.success,
			text: "fake · echo@gpu  hi",
		},
		// 名字被截断、拆不出 server 和工具的 MCP 工具名，以及只是长得像的普通工具，原样显示。
		{
			name: "truncated_mcp_name_stays_raw",
			call: newNamedCall("mcp__fake_0a1b2c3d", `{"text":"hi"}`, nil),
			mark: "◐",
			want: colored.accent,
			text: "mcp__fake_0a1b2c3d  hi",
		},
		{
			name: "plain_tool_with_underscores_stays_raw",
			call: newNamedCall("read_file", `{"path":"a.go"}`, nil),
			mark: "◐",
			want: colored.accent,
			text: "read_file  a.go",
		},
	}
	// 3. 比较普通和着色标题的标记与正文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := formatToolCallRow(tt.call, newSessionStatuses("s1"), plain, tt.defaultExecutorID), tt.mark+" "+tt.text; got != want {
				t.Fatalf("plain row = %q, want %q", got, want)
			}
			if got, want := formatToolCallRow(tt.call, newSessionStatuses("s1"), colored, tt.defaultExecutorID), tt.want.Render(tt.mark)+" "+tt.text; got != want {
				t.Fatalf("colored row = %q, want %q", got, want)
			}
		})
	}
}

// TestFormatToolCallBody 检查工具参数、结果、错误和终端控制清理。
func TestFormatToolCallBody(t *testing.T) {
	// 1. 准备失败且截断、运行中、带终端控制字符及非 JSON 参数的样例。
	styles := newTUIStyles(markdownStyleNoTTY, nil)
	tests := []struct {
		name  string
		call  *toolCall
		width int
		want  []string
	}{
		{
			name: "failed_call_shows_error_and_truncation",
			call: &toolCall{
				call: defs.ToolCall{
					CallID:    "call-1",
					Name:      "bash",
					Arguments: `{"command":"false"}`,
				},
				result: &defs.ToolResult{
					Content:   json.RawMessage(`{"command_id":"cmd_1","status":"exited","exit_code":1,"output":"boom\n","next_cursor":5,"has_more":false}`),
					Truncated: true,
					Error: &defs.ToolError{
						Code:    "exit_nonzero",
						Message: "命令失败",
					},
				},
			},
			width: 60,
			want: []string{
				"参数", "{", `  "command": "false"`, "}",
				"", "输出", "exit 1", "boom", "（输出已截断）", "", "错误", "exit_nonzero：命令失败",
			},
		},
		{
			name: "running_call_has_no_output_section",
			call: &toolCall{call: defs.ToolCall{
				CallID:    "call-2",
				Name:      "read_file",
				Arguments: `{"path":"go.mod"}`,
			}},
			width: 60,
			want:  []string{"参数", "{", `  "path": "go.mod"`, "}"},
		},
		{
			// 检查 PTY 换行、进度条回车和未闭合样式已清理。
			name: "terminal_control_characters_are_sanitized",
			call: &toolCall{
				call: defs.ToolCall{
					CallID:    "call-4",
					Name:      "bash",
					Arguments: `{"command":"make"}`,
				},
				result: &defs.ToolResult{
					Content: json.RawMessage(`{"command_id":"cmd_1","status":"exited","exit_code":2,"output":"line1\r\nprogress 10%\rprogress 100%\n\u001b[41mboom","next_cursor":44,"has_more":false}`),
					Error: &defs.ToolError{
						Code:    "exit_nonzero",
						Message: "bad\r\n\u001b[31mnews",
					},
				},
			},
			width: 60,
			want: []string{
				"参数", "{", `  "command": "make"`, "}",
				"", "输出", "exit 2", "line1", "progress 10%", "progress 100%", "boom", "", "错误", "exit_nonzero：bad", "news",
			},
		},
		{
			name: "arguments_that_are_not_json_are_kept_and_wrapped",
			call: &toolCall{
				call: defs.ToolCall{
					CallID:    "call-3",
					Name:      "custom",
					Arguments: "abcdefghij",
				},
				isNotExecuted: true,
			},
			width: 4,
			want:  []string{"参数", "abcd", "efgh", "ij"},
		},
	}
	// 2. 逐项比较正文，并检查没有终端控制字符。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := formatToolCallBody(tt.call, tt.width, styles)
			if !slices.Equal(lines, tt.want) {
				t.Fatalf("lines = %q\nwant %q", lines, tt.want)
			}
			for _, line := range lines {
				if strings.ContainsAny(line, "\r\x1b") {
					t.Fatalf("line %q contains a control character", line)
				}
			}
		})
	}
}

// TestFormatPatchDiff 检查补丁标题、增删颜色和逐行折行。
func TestFormatPatchDiff(t *testing.T) {
	// 1. 准备新增、修改、删除和长行补丁。
	plain, colored := newTUIStyles(markdownStyleNoTTY, nil), newTUIStyles("dark", nil)
	tests := []struct {
		name   string
		patch  string
		width  int
		styles tuiStyles
		want   []string
	}{
		{
			name:   "add_update_and_delete_headers",
			patch:  "*** Begin Patch\n*** Add File: new.go\n+package x\n*** Update File: a.go\n@@ func a\n ctx\n-x\n+y\n*** Delete File: old.go\n*** End Patch\n",
			width:  80,
			styles: plain,
			// 增行、删行补满宽度（没有底色时只补空格）。
			want: []string{"新增 new.go", padRight("+package x", 80), "修改 a.go", "@@ func a", " ctx", padRight("-x", 80), padRight("+y", 80), "删除 old.go"},
		},
		{
			name:   "patch_without_markers",
			patch:  "+only",
			width:  80,
			styles: plain,
			want:   []string{padRight("+only", 80)},
		},
		{
			name:   "empty_patch",
			patch:  "",
			width:  80,
			styles: plain,
			want:   []string{""},
		},
		{
			// 折行后的每一行分别开启和关闭样式。
			name:   "long_diff_lines_are_styled_per_row",
			patch:  "+" + strings.Repeat("a", 14) + "\n-" + strings.Repeat("b", 14) + "\n@@ " + strings.Repeat("c", 12),
			width:  10,
			styles: colored,
			want: []string{
				colored.success.Render("+aaaaaaaaa"), colored.success.Render("aaaaa") + "     ",
				colored.failure.Render("-bbbbbbbbb"), colored.failure.Render("bbbbb") + "     ",
				colored.faint.Render("@@ ccccccc"), colored.faint.Render("ccccc"),
			},
		},
	}
	// 2. 按宽度渲染并比较每行内容和样式。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if lines := formatPatchDiff(tt.patch, tt.width, tt.styles); !slices.Equal(lines, tt.want) {
				t.Fatalf("lines = %q, want %q", lines, tt.want)
			}
		})
	}
}

// TestInstructionHistory 检查 Skill 和 AGENTS.md 指令的历史显示。
func TestInstructionHistory(t *testing.T) {
	// 1. 每种指令连同用户输入保存两次，检查输入历史只有用户输入、写出的行不含指令正文，
	// 展开内容带来源、Executor 和正文且正文只出现一次。
	for _, tt := range []struct {
		name   string
		item   *defs.Item
		source string
	}{
		{
			"skill",
			&defs.Item{
				ID:     "i1",
				TurnID: "t1",
				Kind:   defs.ItemSkillInstruction,
				SkillInstruction: &defs.SkillInstruction{
					InputItemID: "u1",
					Name:        "review",
					Executor:    "local",
					Path:        "/skills/review/SKILL.md",
					Text:        "saved instruction body",
				},
			},
			"Skill: review",
		},
		{
			"agents",
			&defs.Item{
				ID:     "i1",
				TurnID: "t1",
				Kind:   defs.ItemAgentsInstruction,
				AgentsInstruction: &defs.AgentsInstruction{
					Executor: "local",
					Path:     "/work/AGENTS.md",
					Text:     "saved instruction body",
				},
			},
			"AGENTS.md",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTestTranscript()
			items := logItems(userItem("u1", "请检查 /review"), tt.item)
			printed := tr.handleItemsSaved(items)
			tr.handleItemsSaved(items)
			if !slices.Equal(tr.getUserInputs(), []string{"请检查 /review"}) {
				t.Fatalf("history = %v", tr.getUserInputs())
			}
			if strings.Contains(strings.Join(printed, "\n"), "instruction body") {
				t.Fatal("instructions printed as user input")
			}
			expanded := strings.Join(newExpandView().getContentLines(tr, ""), "\n")
			for _, want := range []string{tt.source, "local", "saved instruction body"} {
				if !strings.Contains(expanded, want) {
					t.Fatalf("missing %q: %s", want, expanded)
				}
			}
			if strings.Count(expanded, "saved instruction body") != 1 {
				t.Fatalf("replayed instruction duplicated: %s", expanded)
			}
		})
	}
}
