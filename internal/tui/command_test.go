package tui

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/charmbracelet/x/ansi"
)

// TestSlashPopup 检查斜杠命令的候选、补全和执行。
func TestSlashPopup(t *testing.T) {
	// 1. 准备命令匹配、按键和执行场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "slash_lists_available_commands",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/")
				names := getCommandNames(h.model.getSlashMatches())
				// /status 与 /context 需要已打开对话，草稿里不出现。
				if !strings.HasPrefix(names, "new resume") || !strings.HasSuffix(names, "help quit") ||
					strings.Contains(names, "status") || strings.Contains(names, "context") {
					t.Fatalf("draft matches = %q", names)
				}
				requireGolden(t, "slash_popup_80x24", h.view())
			},
		},
		{
			name: "prefix_is_case_insensitive",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/RES")
				if names := getCommandNames(h.model.getSlashMatches()); names != "resume" {
					t.Fatalf("matches = %q", names)
				}
			},
		},
		{
			name: "tab_completes_and_enter_runs",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/ne")
				h.press("tab")
				if value := h.model.composer.getValue(); value != "/new" {
					t.Fatalf("completed = %q", value)
				}
				h.press("enter")
				if h.model.composer.getValue() != "" || !strings.Contains(h.getPrinted(), "── 新对话") {
					t.Fatalf("composer = %q, printed = %q", h.model.composer.getValue(), h.getPrinted())
				}
			},
		},
		{
			name: "command_with_trailing_spaces_runs",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/model  ")
				h.press("enter")
				if h.model.activePanel == nil || h.model.composer.getValue() != "" || len(h.client.getCalls("SubmitInput")) != 0 {
					t.Fatalf("panel = %v, draft = %q, submits = %v", h.model.activePanel, h.model.composer.getValue(), h.client.getCalls("SubmitInput"))
				}
			},
		},
		{
			// 命令名后面还有文字时只匹配带参数、名字完全相同的命令，Enter 把后面的文字作为参数执行。
			name: "argument_matches_exact_command_and_runs_with_it",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/cd ../lib")
				if names := getCommandNames(h.model.getSlashMatches()); names != "cd" {
					t.Fatalf("matches = %q", names)
				}
				if view := h.view(); !strings.Contains(view, "› /cd <目录>  命令 · 切换工作目录") {
					t.Fatalf("popup:\n%s", view)
				}
				h.press("enter")
				var resolved []runtimeclient.ResolveWorkspaceDirectoryRequest
				h.waitFor("directory resolved", func() bool {
					h.client.set(func(c *fakeClient) { resolved = c.resolveRequests })
					return len(resolved) == 1
				})
				if resolved[0].Directory != "../lib" || h.model.composer.getValue() != "" || len(h.client.getCalls("CreateConversation", "SubmitInput")) != 0 {
					t.Fatalf("resolved = %+v, composer = %q, calls = %v", resolved, h.model.composer.getValue(), h.client.getCalls())
				}
			},
		},
		{
			// 不带参数的命令后面写了文字时匹配不到，按普通输入发送。
			name: "argument_to_command_without_argument_is_sent_as_text",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/new foo")
				if matches := h.model.getSlashMatches(); len(matches) != 0 {
					t.Fatalf("matches = %q", getCommandNames(matches))
				}
				h.press("enter")
				h.waitFor("submitted", func() bool { return len(h.client.getCalls("SubmitInput")) == 1 })
				if call := h.client.getCalls("SubmitInput")[0]; call != "SubmitInput c1 normal /new foo" {
					t.Fatalf("submit = %q", call)
				}
			},
		},
		{
			// Tab 把带参数的命令补成 "/cd "，接着写参数。
			name: "tab_completes_command_with_argument",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.typeText("/c")
				if names := getCommandNames(h.model.getSlashMatches()); names != "context compact cd" {
					t.Fatalf("matches = %q", names)
				}
				h.press("down", "down", "tab")
				if value := h.model.composer.getValue(); value != "/cd " {
					t.Fatalf("completed = %q", value)
				}
				if names := getCommandNames(h.model.getSlashMatches()); names != "cd" {
					t.Fatalf("matches after completion = %q", names)
				}
			},
		},
		{
			name: "arrows_wrap_around",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/")
				h.press("up")
				if last := len(h.model.getSlashMatches()) - 1; h.model.slashCursor != last {
					t.Fatalf("cursor after up = %d, want %d", h.model.slashCursor, last)
				}
				h.press("down")
				if h.model.slashCursor != 0 {
					t.Fatalf("cursor after down = %d", h.model.slashCursor)
				}
			},
		},
		{
			name: "esc_closes_popup_and_keeps_draft_until_edit",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/he")
				h.press("esc")
				if h.model.getSlashMatches() != nil || h.model.composer.getValue() != "/he" {
					t.Fatalf("popup open after esc, composer = %q", h.model.composer.getValue())
				}
				h.typeText("l")
				if names := getCommandNames(h.model.getSlashMatches()); names != "help" {
					t.Fatalf("popup after edit = %q", names)
				}
			},
		},
		{
			name: "unknown_command_is_sent_as_text",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/没有这个命令")
				h.press("enter")
				h.waitFor("submitted", func() bool { return len(h.client.getCalls("SubmitInput")) == 1 })
				if call := h.client.getCalls("SubmitInput")[0]; call != "SubmitInput c1 normal /没有这个命令" {
					t.Fatalf("submit = %q", call)
				}
			},
		},
		{
			name: "multiline_draft_has_no_popup",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/new")
				h.press("ctrl+j")
				if h.model.getSlashMatches() != nil {
					t.Fatal("popup shown for a multi-line draft")
				}
			},
		},
		{
			name: "question_mark_opens_help_only_when_empty",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("为什么?")
				if h.model.activePanel != nil || h.model.composer.getValue() != "为什么?" {
					t.Fatalf("panel = %v, composer = %q", h.model.activePanel, h.model.composer.getValue())
				}
				h.press("ctrl+c", "?")
				if h.model.activePanel == nil || h.model.activePanel.getTitle() != "帮助" {
					t.Fatal("? on an empty composer did not open help")
				}
			},
		},
		{
			name: "status_listed_once_a_conversation_is_open",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.typeText("/")
				if names := getCommandNames(h.model.getSlashMatches()); names != "new resume fork rewind model provider reload context compact cd executor expand agents status mcp help quit" {
					t.Fatalf("matches = %q", names)
				}
			},
		},
		{
			name: "compact_calls_client_when_conversation_is_open",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.typeText("/compact")
				h.press("enter")
				h.waitFor("compact requested", func() bool { return len(h.client.getCalls("CompactContext")) == 1 })
				if calls := h.client.getCalls("CompactContext"); !slices.Equal(calls, []string{"CompactContext c1"}) {
					t.Fatalf("calls = %v", calls)
				}
				if h.model.composer.getValue() != "" || h.model.notice.text != "" {
					t.Fatalf("composer = %q, notice = %q", h.model.composer.getValue(), h.model.notice.text)
				}
			},
		},
		{
			// 草稿里没有对话可压缩：命令不出现在候选里，按普通输入处理。
			name: "compact_unavailable_without_conversation",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/compact")
				if matches := h.model.getSlashMatches(); len(matches) != 0 {
					t.Fatalf("matches = %q", getCommandNames(matches))
				}
				h.press("enter")
				h.waitFor("submitted", func() bool { return len(h.client.getCalls("SubmitInput")) == 1 })
				if calls := h.client.getCalls("CompactContext"); len(calls) != 0 {
					t.Fatalf("compact calls = %v", calls)
				}
			},
		},
		{
			name: "compact_while_busy_shows_notice",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.client.set(func(c *fakeClient) { c.compactErr = fmt.Errorf("conversation c1: %w", session.ErrSessionBusy) })
				h.typeText("/compact")
				h.press("enter")
				h.waitFor("busy notice", func() bool { return h.model.notice.isError })
				if h.model.notice.text != "正在执行，稍后再压缩" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			name: "compact_failure_shows_reason",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.client.set(func(c *fakeClient) { c.compactErr = errors.New("summary model unavailable") })
				h.typeText("/compact")
				h.press("enter")
				h.waitFor("error notice", func() bool { return h.model.notice.isError })
				if h.model.notice.text != "压缩上下文失败：summary model unavailable" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			name: "new_returns_to_draft_and_hides_conversation_commands",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.typeText("/new")
				h.press("enter")
				if h.model.conversation != nil || !strings.Contains(h.getPrinted(), "── 新对话") {
					t.Fatalf("conversation = %v, printed = %q", h.model.conversation, h.getPrinted())
				}
				h.typeText("/")
				if names := getCommandNames(h.model.getSlashMatches()); strings.Contains(names, "status") {
					t.Fatalf("draft matches = %q", names)
				}
			},
		},
		{
			// 矮终端按可用行数显示选中项附近的候选。
			name: "short_terminal_keeps_selected_row_visible",
			run: func(t *testing.T, h *tuiHarness) {
				h.send(tea.WindowSizeMsg{
					Width:  80,
					Height: 6,
				})
				h.typeText("/")
				h.press("up")
				view := h.view()
				if lines := strings.Split(view, "\n"); !strings.Contains(view, "› /quit") || len(lines) > 6 {
					t.Fatalf("view with the cursor on the last command:\n%s", view)
				}
			},
		},
		{
			name: "quit_runs_from_popup",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/qu")
				h.press("enter")
				if !h.model.isQuitting {
					t.Fatal("/quit did not quit")
				}
			},
		},
		{
			// /fork 复制全部轮次（不指定截断轮次），新对话沿用当前对话的 Executor 设置。
			name: "fork_requests_all_turns_with_current_executor_settings",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				settings := defs.ExecutorSettings{
					DefaultExecutorID:    "gpu",
					ExecutorIDs:          []string{"local", "gpu"},
					WorkspaceDirectories: map[string]string{"gpu": "/data/proj"},
				}
				h.emit(&runtimeclient.ConversationSynced{
					ConversationID: "c1",
					Conversation: defs.Conversation{
						ID:               "c1",
						ExecutorSettings: settings,
					},
				})
				h.typeText("/fork")
				h.press("enter")
				h.waitFor("forked", func() bool { return len(h.client.getCalls("OpenConversation c2")) == 1 })
				want := []runtimeclient.ForkConversationRequest{{
					ConversationID:   "c1",
					ExecutorSettings: settings,
				}}
				if got := h.client.getForkRequests(); !reflect.DeepEqual(got, want) {
					t.Fatalf("fork requests = %+v, want %+v", got, want)
				}
			},
		},
		{
			name: "new_while_opening_draft_is_refused",
			run: func(t *testing.T, h *tuiHarness) {
				gate := make(chan struct{})
				h.client.set(func(c *fakeClient) { c.createGate = gate })
				h.typeText("你好")
				h.press("enter")
				h.typeText("/new")
				h.press("enter")
				if !strings.Contains(h.model.notice.text, "正在新建对话") || !h.model.isOpeningDraft {
					t.Fatalf("notice = %q, opening = %v", h.model.notice.text, h.model.isOpeningDraft)
				}
				close(gate)
				h.waitFor("opened", func() bool { return !h.model.isOpeningDraft })
			},
		},
	}
	// 2. 每个场景在一个新的 80x24 TUI 里操作，检查候选、显示和客户端请求。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 24))
		})
	}
}

// getCommandNames 返回用空格连接的候选名称。
func getCommandNames(commands []command) string {
	// 1. 按候选顺序收集名称并连接。
	names := make([]string, len(commands))
	for index, each := range commands {
		names[index] = each.name
	}
	return strings.Join(names, " ")
}

// TestCommandLineStyles 检查命令弹窗和帮助内容的样式。
func TestCommandLineStyles(t *testing.T) {
	// 1. 生成有颜色的弹窗行，以及有颜色、无颜色两份帮助内容。
	colored := newTUIStyles("dark", nil)
	m := &tuiModel{
		width:    30,
		styles:   colored,
		commands: newCommands(),
	}
	popup := m.getPopupLines([]command{{
		name:        "new",
		description: "开始新对话",
	}, {
		name:        "help",
		key:         "?",
		description: "帮助",
	}}, 8)
	help := m.getHelpLines()
	plainHelp := (&tuiModel{
		styles:   newTUIStyles(markdownStyleNoTTY, nil),
		commands: newCommands(),
	}).getHelpLines()
	// findLine 按去掉样式后的行首找到帮助面板的一行，不依赖命令表的顺序。
	findLine := func(lines []string, prefix string) string {
		if index := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(ansi.Strip(line), prefix) }); index >= 0 {
			return lines[index]
		}
		return ""
	}
	// 2. 列出选中行、未选中行和帮助行的预期显示。
	tests := []struct {
		name string
		got  string
		want string
	}{
		// 选中行整行用 selection 样式（没有底色时就是强调色），只补空格到终端宽度。
		{
			name: "popup_selected_row_whole_selection",
			got:  popup[0],
			want: colored.selection.Render("› /new   命令 · 开始新对话") + strings.Repeat(" ", 4),
		},
		{
			name: "popup_unselected_row_name_normal_description_faint",
			got:  popup[1],
			want: "  /help  " + colored.faint.Render("命令 · 帮助") + strings.Repeat(" ", 10),
		},
		{
			name: "help_command_name_normal_key_bold_description_faint",
			got:  findLine(help, "  /expand"),
			want: "  /expand（" + colored.hintKey.Render("ctrl+o") + "）" + strings.Repeat(" ", 5) + colored.faint.Render("展开视图"),
		},
		{
			name: "help_key_bold_description_faint",
			got:  findLine(help, "  ↑ / ↓"),
			want: "  " + colored.hintKey.Render("↑ / ↓") + strings.Repeat(" ", 17) + colored.faint.Render("输入历史"),
		},
		// 无颜色时只输出名称、说明和对齐空格。
		{
			name: "help_plain_command_row",
			got:  findLine(plainHelp, "  /expand"),
			want: "  /expand（ctrl+o）     展开视图",
		},
		{
			name: "help_plain_key_row",
			got:  findLine(plainHelp, "  ↑ / ↓"),
			want: "  ↑ / ↓" + strings.Repeat(" ", 17) + "输入历史",
		},
	}
	// 3. 逐行比较显示文字和样式。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("line = %q (%q), want %q", tt.got, ansi.Strip(tt.got), tt.want)
			}
		})
	}
}

// TestSlashSkills 检查 Skill 补全、和命令重名时加 skill: 前缀，以及丢弃过期的 Skill 列表结果。
func TestSlashSkills(t *testing.T) {
	// 1. 在读到 4 个 Skill 的 TUI 里分别试连续补全多个 Skill、和命令重名、多行中间位置补全、移动光标重开弹窗、
	// 插件 Skill、skill: 显式引用和过期列表结果，检查补全与发送。
	for _, tt := range []struct {
		name string
		run  func(*testing.T, *tuiHarness)
	}{
		{
			"multiple_skills_insert_before_submit",
			func(t *testing.T, h *tuiHarness) {
				h.typeText("请用 /rev")
				h.press("enter")
				h.typeText("/tes")
				h.press("tab")
				if got := h.model.composer.getValue(); got != "请用 /review /tests " {
					t.Fatalf("draft = %q", got)
				}
				if len(h.client.getCalls("SubmitInput", "CreateConversation")) != 0 {
					t.Fatal("selection submitted input")
				}
				if len(h.model.getSlashMatches()) != 0 {
					t.Fatal("popup stays open after selection")
				}
				h.press("enter")
				h.waitFor("submit", func() bool { return len(h.client.getCalls("SubmitInput")) == 1 })
				if got := h.client.getCalls("SubmitInput")[0]; got != "SubmitInput c1 normal 请用 /review /tests " {
					t.Fatalf("submit = %q", got)
				}
			},
		},
		{
			"command_collision_uses_explicit_skill",
			func(t *testing.T, h *tuiHarness) {
				h.typeText("/help")
				if names := getCommandNames(h.model.getSlashMatches()); names != "help skill:help" {
					t.Fatalf("matches = %q", names)
				}
				h.press("down", "enter")
				if got := h.model.composer.getValue(); got != "/skill:help " {
					t.Fatalf("draft = %q", got)
				}
				if h.model.activePanel != nil {
					t.Fatal("help command executed")
				}
			},
		},
		{
			"multiline_unicode_middle_token",
			func(t *testing.T, h *tuiHarness) {
				h.model.composer.setValueWithCursor("前文\n中文 /rev旧 后文 /tests", 1, 7)
				h.press("tab")
				if got := h.model.composer.getValue(); got != "前文\n中文 /review  后文 /tests" {
					t.Fatalf("draft = %q", got)
				}
				h.typeText("此处")
				if got := h.model.composer.getValue(); got != "前文\n中文 /review 此处 后文 /tests" {
					t.Fatalf("cursor = %q", got)
				}
			},
		},
		{
			"moving_cursor_reopens_dismissed_popup",
			func(t *testing.T, h *tuiHarness) {
				h.typeText("/review")
				h.press("esc", "left")
				if len(h.model.getSlashMatches()) != 1 {
					t.Fatal("cursor move did not reopen popup")
				}
			},
		},
		{
			"plugin_skill_matches_name_after_plugin_prefix",
			func(t *testing.T, h *tuiHarness) {
				h.typeText("/gre")
				if names := getCommandNames(h.model.getSlashMatches()); !strings.Contains(names, "demo:greet") {
					t.Fatalf("matches = %q", names)
				}
				h.press("tab")
				if got := h.model.composer.getValue(); got != "/demo:greet " {
					t.Fatalf("draft = %q", got)
				}
			},
		},
		{
			"explicit_reference_completes",
			func(t *testing.T, h *tuiHarness) {
				h.typeText("用 /skill:rev")
				h.press("enter")
				if got := h.model.composer.getValue(); got != "用 /skill:review " {
					t.Fatalf("draft = %q", got)
				}
			},
		},
		{
			"old_catalog_response_is_ignored",
			func(t *testing.T, h *tuiHarness) {
				oldSeq := h.model.skillListSeq
				h.press("ctrl+c")
				h.typeText("/new")
				h.press("enter")
				h.send(skillListLoadedMsg{
					seq:    oldSeq,
					skills: []skill.SkillInfo{{Name: "stale"}},
				})
				h.typeText("/stale")
				if len(h.model.getSlashMatches()) != 0 {
					t.Fatal("stale catalog applied")
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24, func(c *fakeClient) {
				c.skills = []skill.SkillInfo{{Name: "review"}, {Name: "tests"}, {Name: "help"}, {Name: "demo:greet"}}
			})
			h.waitFor("skills", func() bool { return len(h.model.skills) == 4 })
			tt.run(t, h)
		})
	}
}

// TestReloadCommand 检查配置重载的成功和失败提示。
func TestReloadCommand(t *testing.T) {
	// 1. 准备重载结果、忙碌对话和请求错误。
	tests := []struct {
		name        string
		result      runtime.SetDefinitionsResult
		err         error
		wantNotice  string
		wantIsError bool
	}{
		{
			name:       "reloaded",
			wantNotice: "已重新加载",
		},
		{
			name:       "pending_conversations_apply_when_idle",
			result:     runtime.SetDefinitionsResult{PendingConversationIDs: []defs.ConversationID{"c1", "c2"}},
			wantNotice: "已重新加载，2 个对话将在空闲后自动应用",
		},
		{
			// serve 的 JSON-RPC 错误只显示服务端原文，不带方法名和错误码。
			name: "failure_shows_the_server_message",
			err: fmt.Errorf("config/reload: %w", &runtimeclient.ResponseError{
				Code:    -32000,
				Message: "models.default_model: model x/y is not configured",
			}),
			wantNotice:  "重新加载失败：models.default_model: model x/y is not configured",
			wantIsError: true,
		},
		{
			name:        "failure_without_server_response_shows_the_error",
			err:         errors.New("connection to runtime server lost"),
			wantNotice:  "重新加载失败：connection to runtime server lost",
			wantIsError: true,
		},
	}
	// 2. 执行 /reload，检查通知和请求次数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24, func(c *fakeClient) { c.configWriteResult, c.configWriteErr = tt.result, tt.err })
			h.typeText("/reload")
			h.press("enter")
			h.waitFor("reload notice", func() bool { return h.model.notice.text == tt.wantNotice && h.model.notice.isError == tt.wantIsError })
			if calls := h.client.getCalls("ReloadConfig"); len(calls) != 1 {
				t.Fatalf("reload calls = %v", calls)
			}
		})
	}
}

// TestMCPCommand 检查 MCP 命令打开面板和列表请求。
func TestMCPCommand(t *testing.T) {
	// 1. 准备已打开对话和草稿两种情况。
	tests := []struct {
		name string
		// hasConversation 是否先打开对话？是的话先从草稿发送第一条输入，对话 c1 打开后再执行 /mcp。
		hasConversation bool
		wantCalls       []string
	}{
		{
			name:            "open_conversation_lists_servers_once",
			hasConversation: true,
			wantCalls:       []string{"ListMCPServers c1"},
		},
		// 草稿还没有对话，没有可以查询的对象：面板直接打开，不发请求。
		{name: "draft_opens_panel_without_request"},
	}
	// 2. 执行 /mcp，检查候选、面板类型和列表请求。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24)
			if tt.hasConversation {
				h.openDraftConversation("开场")
			}
			h.typeText("/mcp")
			if names := getCommandNames(h.model.getSlashMatches()); names != "mcp" {
				t.Fatalf("matches of /mcp = %q", names)
			}
			if view := h.view(); !strings.Contains(view, "/mcp") || !strings.Contains(view, "命令 · MCP 服务") {
				t.Fatalf("popup:\n%s", view)
			}
			h.press("enter")
			opened, ok := h.model.activePanel.(*mcpPanel)
			if !ok {
				t.Fatalf("panel = %T, want *mcpPanel", h.model.activePanel)
			}
			h.waitFor("mcp servers listed", func() bool { return !opened.fetch.isLoading })
			if calls := h.client.getCalls("ListMCPServers"); !slices.Equal(calls, tt.wantCalls) {
				t.Fatalf("ListMCPServers calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}
