package tui

import (
	"errors"
	"image/color"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/charmbracelet/x/ansi"
)

// conversationWith 创建恢复对话列表使用的记录。
func conversationWith(id defs.ConversationID, title string, updatedAt int64) defs.Conversation {
	// 1. 写入对话 ID、标题、根 Session 和更新时间。
	return defs.Conversation{
		ID:            id,
		Title:         title,
		RootSessionID: defs.SessionID("s" + strings.TrimPrefix(string(id), "c")),
		UpdatedAt:     updatedAt,
		MessageCount:  4,
	}
}

// TestRenderPanelFrame 检查面板标题、边框、尺寸和底色。
func TestRenderPanelFrame(t *testing.T) {
	// 1. 准备高度不足、正常尺寸和有底色的面板内容。
	tinted := lipgloss.Color("#1e1e1e")
	tests := []struct {
		name       string
		width      int
		maxHeight  int
		body       []string
		background color.Color
		want       []string
	}{
		{
			name:      "title_and_hints",
			width:     30,
			maxHeight: 5,
			body:      []string{"一", "二"},
			want: []string{
				"┌ 标题 " + strings.Repeat("─", 12) + " esc 关闭 ┐",
				"│ 一" + strings.Repeat(" ", 25) + "│",
				"│ 二" + strings.Repeat(" ", 25) + "│",
				"└" + strings.Repeat("─", 28) + "┘",
			},
		},
		{
			name:      "body_cut_to_height",
			width:     20,
			maxHeight: 3,
			body:      []string{"一", "二", "三"},
			want:      []string{"┌ 标题 ── esc 关闭 ┐", "│ 一" + strings.Repeat(" ", 15) + "│", "└" + strings.Repeat("─", 18) + "┘"},
		},
		{
			name:      "too_short_for_border",
			width:     20,
			maxHeight: 2,
			body:      []string{"一", "二", "三"},
			want:      []string{"一" + strings.Repeat(" ", 18), "二" + strings.Repeat(" ", 18)},
		},
		{
			// 边框与内容的每一行都铺满 surface 底色，行内样式复位之后底色重新打开。
			name:       "frame_on_surface",
			width:      20,
			maxHeight:  4,
			body:       []string{lipgloss.NewStyle().Faint(true).Render("一") + "二"},
			background: tinted,
			want:       []string{"┌ 标题 ── esc 关闭 ┐", "│ 一二" + strings.Repeat(" ", 13) + "│", "└" + strings.Repeat("─", 18) + "┘"},
		},
		{
			name:       "too_short_for_border_on_surface",
			width:      20,
			maxHeight:  1,
			body:       []string{"一", "二"},
			background: tinted,
			want:       []string{"一" + strings.Repeat(" ", 18)},
		},
	}
	// 2. 渲染边框并检查宽度、底色和去样式正文。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			styles := newTUIStyles("dark", tt.background)
			got := renderPanelFrame("标题", []keyHint{{
				key:    "esc",
				action: "关闭",
			}}, tt.body, tt.width, tt.maxHeight, styles)
			for index, line := range got {
				if width := ansi.StringWidth(line); width != tt.width {
					t.Fatalf("line %q width %d, want %d", line, width, tt.width)
				}
				if tt.background != nil {
					requireBackgroundFilled(t, line, styles.surface)
				}
				got[index] = ansi.Strip(line)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("frame =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// TestListCursor 检查列表选中位置和可见范围。
func TestListCursor(t *testing.T) {
	// 1. 准备移动、翻页、首尾和空列表场景。
	tests := []struct {
		name         string
		keys         []string
		count        int
		height       int
		wantSelected int
		wantRange    [2]int
	}{
		{
			name:         "down_scrolls_window",
			keys:         []string{"down", "down", "down"},
			count:        10,
			height:       2,
			wantSelected: 3,
			wantRange:    [2]int{2, 4},
		},
		{
			name:         "up_stops_at_top",
			keys:         []string{"up"},
			count:        10,
			height:       3,
			wantSelected: 0,
			wantRange:    [2]int{0, 3},
		},
		{
			name:         "end_and_pgup",
			keys:         []string{"end", "pgup"},
			count:        10,
			height:       3,
			wantSelected: 6,
			wantRange:    [2]int{6, 9},
		},
		{
			name:         "empty_list",
			keys:         []string{"down"},
			count:        0,
			height:       3,
			wantSelected: 0,
			wantRange:    [2]int{0, 0},
		},
	}
	// 2. 逐键移动列表，比较选中项及可见起止位置。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cursor listCursor
			for _, key := range tt.keys {
				if !cursor.handleKey(key, tt.count, tt.height) {
					t.Fatalf("key %q not handled", key)
				}
				cursor.getVisibleRange(tt.count, tt.height)
			}
			start, end := cursor.getVisibleRange(tt.count, tt.height)
			if cursor.selected != tt.wantSelected || [2]int{start, end} != tt.wantRange {
				t.Fatalf("selected = %d, range = [%d %d], want %d %v", cursor.selected, start, end, tt.wantSelected, tt.wantRange)
			}
		})
	}
}

// TestRenderListRows 检查列表选中行高亮和未选中行样式。
func TestRenderListRows(t *testing.T) {
	// 1. 准备行内样式和选中底色场景。
	colored, tinted := newTUIStyles("dark", nil), newTUIStyles("dark", lipgloss.Color("#1e1e1e"))
	rows := []string{colored.faint.Render("回复") + "  ≈30K  很长的回答", colored.faint.Render("用户输入") + "  ≈500  你好呀"}
	tests := []struct {
		name     string
		styles   tuiStyles
		selected int
		index    int
		want     string
		// isSelectionFilled 这一行是否整行铺满 selection 底色？是的话 want 是去掉样式后的内容。
		isSelectionFilled bool
	}{
		// 选中行去掉行内样式后统一套强调色，无底色时补空格。
		{
			name:     "selected_row_is_fully_accent",
			styles:   colored,
			selected: 0,
			index:    0,
			want:     colored.accent.Render("› 回复  ≈30K  很长的回答") + "  ",
		},
		{
			name:     "unselected_row_keeps_inner_styles",
			styles:   colored,
			selected: 0,
			index:    1,
			want:     "  " + rows[1],
		},
		{
			name:     "selected_second_row",
			styles:   colored,
			selected: 1,
			index:    1,
			want:     colored.accent.Render("› 用户输入  ≈500  你好呀") + "  ",
		},
		{
			name:              "selected_row_fills_selection_background",
			styles:            tinted,
			selected:          0,
			index:             0,
			want:              "› 回复  ≈30K  很长的回答  ",
			isSelectionFilled: true,
		},
		{
			name:     "unselected_row_has_no_selection_background",
			styles:   tinted,
			selected: 0,
			index:    1,
			want:     "  " + rows[1],
		},
	}
	// 2. 生成列表并检查指定行文字、样式和底色。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := renderListRows(&tuiModel{styles: tt.styles}, rows, &listCursor{selected: tt.selected}, 10, 26)
			if len(lines) != len(rows) {
				t.Fatalf("lines = %q", lines)
			}
			line := lines[tt.index]
			if !tt.isSelectionFilled {
				if line != tt.want {
					t.Fatalf("line %d = %q, want %q", tt.index, line, tt.want)
				}
				return
			}
			if ansi.Strip(line) != tt.want || ansi.StringWidth(line) != 26 {
				t.Fatalf("line %d = %q, want %q", tt.index, line, tt.want)
			}
			requireBackgroundFilled(t, line, tt.styles.selection)
		})
	}
}

// TestScrollView 检查文本滚动和高度限制。
func TestScrollView(t *testing.T) {
	// 1. 准备滚动、定位、短内容和无可用高度场景。
	lines := []string{"1", "2", "3", "4", "5", "6"}
	tests := []struct {
		name   string
		keys   []string
		height int
		want   []string
	}{
		{
			name:   "down_scrolls_one_line",
			keys:   []string{"down"},
			height: 3,
			want:   []string{"2", "3", "4"},
		},
		{
			name:   "up_stops_at_top",
			keys:   []string{"up", "up"},
			height: 3,
			want:   []string{"1", "2", "3"},
		},
		{
			name:   "end_shows_last_lines",
			keys:   []string{"end"},
			height: 4,
			want:   []string{"3", "4", "5", "6"},
		},
		{
			name:   "pgdown_past_end_is_clamped",
			keys:   []string{"pgdown", "pgdown", "pgdown"},
			height: 4,
			want:   []string{"3", "4", "5", "6"},
		},
		{
			name:   "home_returns_to_top",
			keys:   []string{"end", "home"},
			height: 2,
			want:   []string{"1", "2"},
		},
		{
			name:   "content_shorter_than_height",
			keys:   []string{"down", "end"},
			height: 10,
			want:   lines,
		},
		{
			name:   "zero_height",
			keys:   []string{"end"},
			height: 0,
			want:   []string{},
		},
		// 可用高度为负时按 End 仍返回空正文。
		{
			name:   "negative_height_after_end",
			keys:   []string{"end"},
			height: -2,
			want:   []string{},
		},
	}
	// 2. 逐键滚动并比较可见正文，最后检查普通按键未被处理。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var scroll scrollView
			for _, key := range tt.keys {
				if !scroll.handleKey(key, tt.height) {
					t.Fatalf("key %q not handled", key)
				}
			}
			if got := scroll.getVisible(lines, tt.height); !slices.Equal(got, tt.want) {
				t.Fatalf("visible = %v, want %v", got, tt.want)
			}
		})
	}
	var scroll scrollView
	if scroll.handleKey("x", 3) {
		t.Fatal("scrollView handled a non-scroll key")
	}
}

// TestStatusPanel 检查会话详情、token 显隐、刷新和失败处理。
func TestStatusPanel(t *testing.T) {
	// 1. 准备模型、Executor、Skill 和工具详情。
	details := conversation.SessionDetails{
		ConversationID: "c1",
		Title:          "修复输入丢失",
		Session: defs.Session{
			ID:        "s1",
			AgentName: "assistant",
		},
		Model: &defs.ModelDefinition{
			Ref: defs.ModelRef{
				ProviderID: "openai",
				ModelID:    "gpt-x",
			},
			DisplayName:   "GPT X",
			ContextWindow: 128000,
		},
		CompactModel: &defs.ModelDefinition{
			Ref: defs.ModelRef{
				ProviderID: "openai",
				ModelID:    "gpt-mini",
			},
			DisplayName:   "GPT Mini",
			ContextWindow: 64000,
		},
		Executors: []conversation.ExecutorDetails{{
			ID:                 "local",
			Description:        "本机",
			WorkspaceDirectory: "/home/u/code",
		}},
		Skills: []string{"review"},
		Tools:  []string{"bash", "read_file"},
	}
	// 2. 列出显示与刷新、token 显隐、没有直连地址、滚动与矮终端、过期结果和查询失败场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "fetch_render_and_refresh",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) { c.sessionDetails = details })
				h.openDraftConversation("开场")
				h.emit(&runtimeclient.ConversationTitleChanged{
					ConversationID: "c1",
					Title:          details.Title,
				})
				// 保存首条输入，移除对应排队记录。
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					TurnID:         "t1",
					Items:          logItems(userItem("input-1", "开场")),
				})
				h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionConnected})
				h.typeText("/status")
				h.press("enter")
				h.waitFor("details loaded", func() bool { return strings.Contains(h.view(), "GPT X") })
				requireGolden(t, "status_panel_80x24", h.view())
				h.press("r")
				h.waitFor("refreshed", func() bool { return len(h.client.getCalls("GetSessionDetails")) == 2 })
				h.press("esc")
				if h.model.activePanel != nil {
					t.Fatal("esc did not close the panel")
				}
			},
		},
		{
			name: "t_reveals_and_hides_connect_url",
			run: func(t *testing.T, h *tuiHarness) {
				h.model.opt.ServerURL = "ws://127.0.0.1:8080/api/v1/openvaala"
				h.model.opt.ServerSource = "client.url"
				h.model.opt.ConnectURL = "ws://127.0.0.1:8080/api/v1/openvaala?token=secret"
				h.client.set(func(c *fakeClient) { c.sessionDetails = details })
				h.openDraftConversation("开场")
				h.typeText("/status")
				h.press("enter")
				h.waitFor("details loaded", func() bool { return strings.Contains(h.view(), "GPT X") })
				// 分节后 80x24 放不下全部内容，连接一节在末尾，先滚到底。
				h.press("end")
				view := h.view()
				if !strings.Contains(view, "服务      ws://127.0.0.1:8080/api/v1/openvaala（client.url）") || strings.Contains(view, "secret") {
					t.Fatalf("view should show the redacted server url and its source only:\n%s", view)
				}
				if !strings.Contains(view, "t 显示 token") {
					t.Fatalf("view lacks the token hint:\n%s", view)
				}
				h.press("t")
				view = h.view()
				if !strings.Contains(view, "服务      ws://127.0.0.1:8080/api/v1/openvaala?token=secret（client.url）") {
					t.Fatalf("view lacks the connect url and its source:\n%s", view)
				}
				if !strings.Contains(view, "t 隐藏 token") {
					t.Fatalf("view lacks the hide hint:\n%s", view)
				}
				h.press("t")
				if view := h.view(); strings.Contains(view, "secret") {
					t.Fatalf("view should hide the token again:\n%s", view)
				}
			},
		},
		{
			name: "no_connect_url_without_token",
			run: func(t *testing.T, h *tuiHarness) {
				h.model.opt.ServerURL = "ws://127.0.0.1:8080/api/v1/openvaala"
				h.client.set(func(c *fakeClient) { c.sessionDetails = details })
				h.openDraftConversation("开场")
				h.typeText("/status")
				h.press("enter")
				h.waitFor("details loaded", func() bool { return strings.Contains(h.view(), "GPT X") })
				h.press("end")
				h.press("t")
				view := h.view()
				if !strings.Contains(view, "服务      ws://127.0.0.1:8080/api/v1/openvaala") || strings.Contains(view, "token") {
					t.Fatalf("view should show the server url and no token hint:\n%s", view)
				}
			},
		},
		{
			// 面板放不下全部内容时按 End 滚到末尾；终端矮到面板没有行数时也不崩溃。
			name: "scrolls_and_survives_tiny_terminal",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) { c.sessionDetails = details })
				h.openDraftConversation("开场")
				h.typeText("/status")
				h.press("enter")
				h.waitFor("details loaded", func() bool { return strings.Contains(h.view(), "GPT X") })
				h.send(tea.WindowSizeMsg{
					Width:  80,
					Height: 12,
				})
				h.press("end")
				if view := h.view(); !strings.Contains(view, "服务") || strings.Contains(view, "修复输入丢失") {
					t.Fatalf("end did not scroll to the tail:\n%s", view)
				}
				h.send(tea.WindowSizeMsg{
					Width:  80,
					Height: 3,
				})
				h.press("end")
				if view := h.view(); strings.Contains(view, "会话信息") {
					t.Fatalf("panel drawn without room:\n%s", view)
				}
			},
		},
		{
			name: "stale_result_dropped",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.typeText("/status")
				h.press("enter")
				staleSeq := h.model.panelRequestSeq
				h.press("r")
				h.send(panelResultMsg{
					seq:   staleSeq,
					value: details,
				})
				status := h.model.activePanel.(*statusPanel)
				if status.details != nil {
					t.Fatal("stale result applied")
				}
			},
		},
		{
			name: "error_keeps_previous_data",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.typeText("/status")
				h.press("enter")
				status := h.model.activePanel.(*statusPanel)
				h.send(panelResultMsg{
					seq:   h.model.panelRequestSeq,
					value: details,
				})
				h.press("r")
				h.send(panelResultMsg{
					seq: h.model.panelRequestSeq,
					err: errors.New("timeout"),
				})
				if status.details == nil || !strings.Contains(h.view(), "✗ timeout") {
					t.Fatalf("details = %v, view = %q", status.details, h.view())
				}
			},
		},
	}
	// 3. 每个场景在一个新的 80x24 TUI 里操作并检查面板内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 24))
		})
	}
}

// TestResumePanel 检查最近对话过滤、归档、删除和恢复。
func TestResumePanel(t *testing.T) {
	// 1. 准备过滤、空列表、新建中拒绝打开、打开失败、已归档视图、分叉标记、删除失败和恢复对话场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "lists_recent_and_filters_by_title",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) {
					c.conversations = []defs.Conversation{conversationWith("c7", "修复输入丢失", 0), conversationWith("c8", "周报", 0)}
				})
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Contains(h.view(), "周报") })
				if call := h.client.getCalls("ListConversations")[0]; call != "ListConversations updated_at 20" {
					t.Fatalf("list call = %q", call)
				}
				h.typeText("周")
				if view := h.view(); strings.Contains(view, "修复输入丢失") || !strings.Contains(view, "周报") {
					t.Fatalf("filtered view = %q", view)
				}
				h.press("backspace")
				if !strings.Contains(h.view(), "修复输入丢失") {
					t.Fatal("backspace did not clear the filter")
				}
				if h.model.composer.getValue() != "" {
					t.Fatalf("typing in the panel changed the draft: %q", h.model.composer.getValue())
				}
			},
		},
		{
			name: "empty_list_shows_placeholder_and_esc_closes",
			run: func(t *testing.T, h *tuiHarness) {
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("loaded", func() bool { return strings.Contains(h.view(), "没有对话") })
				h.press("enter", "down")
				if h.model.activePanel == nil || len(h.client.getCalls("OpenConversation")) != 0 {
					t.Fatalf("enter on an empty list changed state: panel = %v", h.model.activePanel)
				}
				h.press("esc")
				if h.model.activePanel != nil {
					t.Fatal("esc did not close the panel")
				}
			},
		},
		{
			name: "enter_refused_while_opening_draft",
			run: func(t *testing.T, h *tuiHarness) {
				gate := make(chan struct{})
				h.client.set(func(c *fakeClient) {
					c.createGate = gate
					c.conversations = []defs.Conversation{conversationWith("c7", "修复输入丢失", 0)}
				})
				h.typeText("你好")
				h.press("enter")
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Contains(h.view(), "修复输入丢失") })
				h.press("enter")
				if !strings.Contains(h.model.notice.text, "正在新建对话") || h.model.activePanel == nil || len(h.client.getCalls("OpenConversation")) != 0 {
					t.Fatalf("notice = %q, panel = %v, opens = %v", h.model.notice.text, h.model.activePanel, h.client.getCalls("OpenConversation"))
				}
				close(gate)
				h.waitFor("draft opened", func() bool { return !h.model.isOpeningDraft })
			},
		},
		{
			// 打开失败回到草稿：期间新打开的面板随之关闭，/resume 的输入历史加载也取消。
			name: "open_failure_closes_panel_and_cancels_history_load",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) {
					c.conversations = []defs.Conversation{conversationWith("c7", "修复输入丢失", 0)}
					c.openErr = errors.New("unreachable")
				})
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Contains(h.view(), "修复输入丢失") })
				h.press("enter")
				h.typeText("/status")
				h.press("enter")
				if h.model.activePanel == nil || !h.model.shouldLoadHistory {
					t.Fatalf("panel = %v, shouldLoadHistory = %v", h.model.activePanel, h.model.shouldLoadHistory)
				}
				h.waitFor("open failed", func() bool { return h.model.conversation == nil })
				if h.model.activePanel != nil || h.model.shouldLoadHistory || !strings.Contains(h.model.notice.text, "打开对话失败") {
					t.Fatalf("panel = %v, shouldLoadHistory = %v, notice = %q", h.model.activePanel, h.model.shouldLoadHistory, h.model.notice.text)
				}
			},
		},
		{
			// Tab 切到「已归档」时请求已归档的对话，提示随视图换成「最近」「取消归档」；Ctrl+A 取消归档后刷新当前视图。
			name: "tab_lists_archived_and_ctrl_a_unarchives",
			run: func(t *testing.T, h *tuiHarness) {
				archived := conversationWith("c8", "旧周报", 0)
				archived.ArchivedAt = 1
				h.client.set(func(c *fakeClient) {
					c.conversations = []defs.Conversation{conversationWith("c7", "修复输入丢失", 0), archived}
				})
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("recent listed", func() bool { return strings.Contains(h.view(), "修复输入丢失") })
				if view := h.view(); strings.Contains(view, "旧周报") || !strings.Contains(view, "tab 已归档") || !strings.Contains(view, "ctrl+a 归档") {
					t.Fatalf("recent view:\n%s", view)
				}
				h.press("tab")
				h.waitFor("archived listed", func() bool { return strings.Contains(h.view(), "旧周报") })
				if call := h.client.getCalls("ListConversations")[1]; call != "ListConversations updated_at 20 archived" {
					t.Fatalf("archived list call = %q", call)
				}
				if view := h.view(); strings.Contains(view, "修复输入丢失") || !strings.Contains(view, "tab 最近") || !strings.Contains(view, "ctrl+a 取消归档") {
					t.Fatalf("archived view:\n%s", view)
				}
				h.press("ctrl+a")
				h.waitFor("unarchived", func() bool { return strings.Contains(h.view(), "没有对话") })
				if calls := h.client.getCalls("SetConversationArchived"); !slices.Equal(calls, []string{"SetConversationArchived c8 false"}) {
					t.Fatalf("archive calls = %v", calls)
				}
			},
		},
		{
			name: "forked_row_is_marked",
			run: func(t *testing.T, h *tuiHarness) {
				forked := conversationWith("c9", "修复输入丢失", 0)
				forked.ForkedFromID = "c7"
				h.client.set(func(c *fakeClient) {
					c.conversations = []defs.Conversation{forked, conversationWith("c7", "修复输入丢失", 0)}
				})
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Count(h.view(), "修复输入丢失") == 2 })
				for _, line := range strings.Split(h.view(), "\n") {
					if strings.Contains(line, "修复输入丢失") && strings.Contains(line, "分叉") != strings.Contains(line, "c9") {
						t.Fatalf("fork mark on the wrong row %q:\n%s", line, h.view())
					}
				}
			},
		},
		{
			name: "delete_failure_keeps_list",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) {
					c.conversations = []defs.Conversation{conversationWith("c7", "修复输入丢失", 0)}
					c.deleteErr = errors.New("busy")
				})
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Contains(h.view(), "修复输入丢失") })
				h.press("ctrl+d")
				if h.model.notice.text != "再按 Ctrl+D 删除" || len(h.client.getCalls("DeleteConversation")) != 0 {
					t.Fatalf("notice = %q, calls = %v", h.model.notice.text, h.client.getCalls("DeleteConversation"))
				}
				h.press("ctrl+d")
				h.waitFor("delete failed", func() bool { return strings.Contains(h.view(), "✗ busy") })
				if !strings.Contains(h.view(), "修复输入丢失") || h.model.activePanel == nil {
					t.Fatalf("view after failed delete:\n%s", h.view())
				}
			},
		},
		{
			name: "enter_opens_and_loads_history",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) {
					c.conversations = []defs.Conversation{conversationWith("c7", "修复输入丢失", 0)}
				})
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Contains(h.view(), "修复输入丢失") })
				h.press("enter")
				h.waitFor("opened", func() bool { return len(h.client.getCalls("OpenConversation")) == 1 })
				if h.model.activePanel != nil || h.model.conversation == nil || h.model.conversation.ID != "c7" {
					t.Fatalf("panel = %v, conversation = %v", h.model.activePanel, h.model.conversation)
				}
				h.emit(&runtimeclient.SessionSynced{
					ConversationID: "c7",
					Items:          logItems(userItem("u1", "旧输入")),
				})
				h.press("up")
				if h.model.composer.getValue() != "旧输入" {
					t.Fatalf("history = %q", h.model.composer.getValue())
				}
				if !strings.Contains(h.getPrinted(), "── 修复输入丢失 · c7") {
					t.Fatalf("printed = %q", h.getPrinted())
				}
			},
		},
	}
	// 2. 每个场景在一个新的 80x24 TUI 里检查列表、请求和输入历史。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 24))
		})
	}
}

// TestRewindPanel 检查轮次预览、分叉和开头输入恢复。
func TestRewindPanel(t *testing.T) {
	// 1. 准备三轮用户输入、插话和子 Agent 消息。
	inTurn := func(turnID defs.TurnID, item *defs.Item) *defs.Item {
		item.TurnID = turnID
		return item
	}
	// saveTurns 打开对话 c1 并保存三轮：t1 开头一条两行的消息、模型输出后又插进一条补充；t2 由两条排队消息开头；
	// t3 由子 Agent 结果开头，没有用户消息。
	saveTurns := func(h *tuiHarness) {
		h.openDraftConversation("开场")
		h.emit(&runtimeclient.ItemsSaved{
			ConversationID: "c1",
			Items: logItems(
				inTurn("t1", userItem("u1", "第一轮\n第二行")), inTurn("t1", replyItem("r1", "好")), inTurn("t1", userItem("u2", "补充说明")),
				inTurn("t2", userItem("u3", "排队一")), inTurn("t2", userItem("u4", "排队二")), inTurn("t2", replyItem("r2", "都好")),
				inTurn("t3", agentItem("a1", "s2", "子 Agent 结果", nil)), inTurn("t3", replyItem("r3", "收到")),
			),
		})
	}
	// 2. 列出回退列表、分叉成功、分叉失败、有草稿时不打开和空面板场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "one_row_per_user_turn_newest_first",
			run: func(t *testing.T, h *tuiHarness) {
				saveTurns(h)
				h.press("esc")
				if h.model.notice.text != "再按 Esc 回退" || h.model.activePanel != nil {
					t.Fatalf("notice = %q, panel = %v", h.model.notice.text, h.model.activePanel)
				}
				h.press("esc")
				view := h.view()
				requireOrder(t, view, "回退重来", "enter 在这条之前分叉，原文放回输入框 · esc 关闭", "排队一", "第一轮")
				for _, notWant := range []string{"第二行", "补充说明", "排队二", "子 Agent"} {
					if strings.Contains(view, notWant) {
						t.Fatalf("panel shows %q:\n%s", notWant, view)
					}
				}
			},
		},
		{
			name: "enter_forks_before_turn_and_restores_batch",
			run: func(t *testing.T, h *tuiHarness) {
				saveTurns(h)
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
				h.typeText("/rewind")
				h.press("enter", "enter")
				h.waitFor("forked and opened", func() bool { return len(h.client.getCalls("OpenConversation c2")) == 1 })
				if h.model.activePanel != nil || h.model.composer.getValue() != "排队一\n\n排队二" || h.model.notice.text != "已在这条消息之前分叉，原对话保留" {
					t.Fatalf("panel = %v, composer = %q, notice = %q", h.model.activePanel, h.model.composer.getValue(), h.model.notice.text)
				}
				if calls := h.client.getCalls("ForkConversation", "OpenConversation c2"); !slices.Equal(calls, []string{"ForkConversation c1 t2", "OpenConversation c2"}) {
					t.Fatalf("calls = %v", calls)
				}
				want := []runtimeclient.ForkConversationRequest{{
					ConversationID:   "c1",
					BeforeTurnID:     "t2",
					ExecutorSettings: settings,
				}}
				if got := h.client.getForkRequests(); !reflect.DeepEqual(got, want) {
					t.Fatalf("fork requests = %+v, want %+v", got, want)
				}
			},
		},
		{
			name: "fork_failure_stays_in_conversation",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) { c.forkErr = errors.New("turn not found") })
				saveTurns(h)
				h.press("esc", "esc", "down", "enter")
				h.waitFor("fork failed", func() bool { return h.model.notice.isError })
				if h.model.notice.text != "分叉失败：turn not found" || h.model.conversation.ID != "c1" || h.model.composer.getValue() != "" {
					t.Fatalf("notice = %q, conversation = %s, composer = %q", h.model.notice.text, h.model.conversation.ID, h.model.composer.getValue())
				}
			},
		},
		{
			// 输入框有草稿时两下 Esc 不打开回退面板，草稿不动。
			name: "esc_twice_with_draft_does_nothing",
			run: func(t *testing.T, h *tuiHarness) {
				saveTurns(h)
				h.typeText("草稿")
				h.press("esc", "esc")
				if h.model.activePanel != nil || h.model.composer.getValue() != "草稿" {
					t.Fatalf("panel = %v, composer = %q", h.model.activePanel, h.model.composer.getValue())
				}
			},
		},
		{
			name: "empty_panel_ignores_enter",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.typeText("/rewind")
				h.press("enter")
				if !strings.Contains(h.view(), "没有可以回退的消息") {
					t.Fatalf("view:\n%s", h.view())
				}
				h.press("enter")
				if h.model.activePanel == nil || len(h.client.getCalls("ForkConversation")) != 0 {
					t.Fatalf("panel = %v, calls = %v", h.model.activePanel, h.client.getCalls("ForkConversation"))
				}
			},
		},
	}
	// 3. 每个场景在一个新的 80x24 TUI 里操作回退面板，检查请求和放回的文字。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 24))
		})
	}
}

// TestHelpPanel 检查终端键盘能力对应的帮助内容。
func TestHelpPanel(t *testing.T) {
	// 1. 准备普通键盘和增强键盘的预期提示。
	tests := []struct {
		name string
		// isEnhanced 帮助打开后终端是否报告了键盘增强？报告后帮助列出 Shift+Enter。
		isEnhanced bool
		want       []string
		notWant    []string
		golden     string
	}{
		{
			name:    "basic_keys_without_enhancements",
			want:    []string{"/new", "/resume", "/quit", "Ctrl+J", "Esc Esc"},
			notWant: []string{"Shift+Enter"},
		},
		{
			name:       "enhancements_add_shift_enter",
			isEnhanced: true,
			want:       []string{"/new", "/resume", "/quit", "Shift+Enter / Ctrl+J", "Esc Esc"},
			golden:     "help_panel_80x44",
		},
	}
	// 2. 打开帮助，更新键盘能力后检查显示和隐藏内容。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 44)
			h.press("?")
			if tt.isEnhanced {
				h.send(tea.KeyboardEnhancementsMsg{Flags: 1})
			}
			view := h.view()
			for _, want := range tt.want {
				if !strings.Contains(view, want) {
					t.Fatalf("help lacks %q:\n%s", want, view)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(view, notWant) {
					t.Fatalf("help mentions %q:\n%s", notWant, view)
				}
			}
			if tt.golden != "" {
				requireGolden(t, tt.golden, view)
			}
		})
	}
}
