package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/charmbracelet/x/ansi"
)

// TestModelPanel 检查模型目录、强度选择和对话模型修改。
func TestModelPanel(t *testing.T) {
	// 1. 准备模型目录、根 Session 和面板打开操作。
	gptX := defs.ModelRef{
		ProviderID: "openai",
		ModelID:    "gpt-x",
	}
	catalog := runtime.ModelList{
		Models: []*defs.ModelDefinition{
			{
				Ref:                    gptX,
				DisplayName:            "GPT X",
				ContextWindow:          128000,
				ReasoningEfforts:       []string{"low", "medium", "high"},
				DefaultReasoningEffort: "low",
			},
			{
				Ref: defs.ModelRef{
					ProviderID: "openai",
					ModelID:    "gpt-mini",
				},
				DisplayName:   "GPT Mini",
				ContextWindow: 64000,
			},
			{
				Ref: defs.ModelRef{
					ProviderID: "local",
					ModelID:    "qwen",
				},
				DisplayName:            "Qwen",
				ContextWindow:          32000,
				ReasoningEfforts:       []string{"high", "max"},
				DefaultReasoningEffort: "max",
			},
		},
		AgentDefaultModel: gptX,
	}
	// openWithModel 打开对话 c1，根 Session 选中 gpt-x·medium；running 为 true 时有活动 Turn。
	openWithModel := func(h *tuiHarness, running bool) {
		h.openDraftConversation("开场")
		// 保存首条输入，移除对应排队记录。
		h.emit(&runtimeclient.ItemsSaved{
			ConversationID: "c1",
			TurnID:         "t1",
			Items:          logItems(userItem("input-1", "开场")),
		})
		info := newRootInfo("")
		if running {
			info = newRootInfo("t1")
		}
		info.Session.Model = defs.ModelSelection{
			Ref:             gptX,
			ReasoningEffort: "medium",
		}
		h.emit(&runtimeclient.SessionStatusChanged{
			ConversationID: "c1",
			Info:           info,
		})
	}
	openPanel := func(h *tuiHarness) {
		h.typeText("/model")
		h.press("enter")
		h.waitFor("models listed", func() bool { return strings.Contains(h.view(), "Qwen") })
	}
	// 2. 列出模型显示、草稿暂存、请求失败和对话切换场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			name: "connect_loads_display_names",
			run: func(t *testing.T, h *tuiHarness) {
				h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionConnected})
				h.waitFor("names loaded", func() bool { return h.model.modelDisplayNames[gptX] == "GPT X" })
				if footer := ansi.Strip(h.model.getFooterLine()); !strings.HasPrefix(footer, "GPT X") {
					t.Fatalf("draft footer = %q", footer)
				}
			},
		},
		{
			// 服务商与显示名分别截断，列间留空格并显示窗口列。
			name: "columns_truncate_and_keep_separators",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) {
					c.modelList = runtime.ModelList{Models: []*defs.ModelDefinition{
						{
							Ref: defs.ModelRef{
								ProviderID: "openrouter",
								ModelID:    "a",
							},
							DisplayName:   "A",
							ContextWindow: 8000,
						},
						{
							Ref: defs.ModelRef{
								ProviderID: "very-long-provider",
								ModelID:    "b",
							},
							DisplayName:   strings.Repeat("名", 20),
							ContextWindow: 128000,
						},
					}}
				})
				h.typeText("/model")
				h.press("enter")
				h.waitFor("models listed", func() bool { return strings.Contains(h.view(), "8K") })
				rows := getPanelBodyLines(h.view())
				if len(rows) != 3 || !strings.HasPrefix(rows[1], "› openrouter ○ A") {
					t.Fatalf("rows = %q", rows)
				}
				if !strings.HasPrefix(rows[2], "very-long… ○ "+strings.Repeat("名", 11)+"… ") || !strings.HasSuffix(rows[2], " 128K") {
					t.Fatalf("long row = %q", rows[2])
				}
			},
		},
		{
			// 没有匹配模型时显示提示。
			name: "filter_without_match_shows_hint",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.typeText("zzz")
				if rows := getPanelBodyLines(h.view()); len(rows) != 2 || rows[1] != "没有匹配的模型" {
					t.Fatalf("rows = %q", rows)
				}
			},
		},
		{
			name: "draft_selection_is_sent_on_create",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.typeText("mini")
				h.press("enter")
				if h.model.activePanel != nil || h.model.notice.text != "新对话将使用 GPT Mini" {
					t.Fatalf("panel = %v, notice = %q", h.model.activePanel, h.model.notice.text)
				}
				h.typeText("你好")
				h.press("enter")
				h.waitFor("created", func() bool { return len(h.client.getCalls("CreateConversation")) == 1 })
				if call := h.client.getCalls("CreateConversation")[0]; call != "CreateConversation assistant openai/gpt-mini" {
					t.Fatalf("create = %q", call)
				}
			},
		},
		{
			name: "effort_highlights_current_or_default",
			run: func(t *testing.T, h *tuiHarness) {
				openWithModel(h, false)
				openPanel(h)
				requireGolden(t, "model_panel_80x24", h.view())
				h.press("enter")
				model := h.model.activePanel.(*modelPanel)
				if model.selected == nil || model.selected.Ref != gptX || model.efforts.selected != 1 {
					t.Fatalf("gpt-x efforts: selected = %v, highlight = %d (want medium)", model.selected, model.efforts.selected)
				}
				h.press("esc")
				if h.model.activePanel == nil || model.selected != nil {
					t.Fatal("esc on the effort page did not go back to the model list")
				}
				h.press("down", "down", "enter")
				if model.selected.Ref.ModelID != "qwen" || model.efforts.selected != 1 {
					t.Fatalf("qwen efforts: highlight = %d (want default max)", model.efforts.selected)
				}
			},
		},
		{
			name: "confirm_while_running_says_next_turn",
			run: func(t *testing.T, h *tuiHarness) {
				openWithModel(h, true)
				openPanel(h)
				h.press("enter", "down", "enter")
				h.waitFor("model set", func() bool { return h.model.notice.text != "" })
				if call := h.client.getCalls("SetSessionModel")[0]; call != "SetSessionModel c1 s1 openai/gpt-x·high" {
					t.Fatalf("set model = %q", call)
				}
				if h.model.notice.text != "已切换到 GPT X·high，下一 Turn 生效" {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			name: "model_without_efforts_skips_effort_page",
			run: func(t *testing.T, h *tuiHarness) {
				openWithModel(h, false)
				openPanel(h)
				h.press("down", "enter")
				h.waitFor("model set", func() bool { return len(h.client.getCalls("SetSessionModel")) == 1 })
				if call := h.client.getCalls("SetSessionModel")[0]; call != "SetSessionModel c1 s1 openai/gpt-mini" {
					t.Fatalf("set model = %q", call)
				}
			},
		},
		{
			name: "set_model_error",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) { c.setModelErr = errors.New("unsupported reasoning effort") })
				openWithModel(h, false)
				openPanel(h)
				h.press("down", "enter")
				h.waitFor("error notice", func() bool { return h.model.notice.isError })
				if !strings.Contains(h.model.notice.text, "切换模型失败") {
					t.Fatalf("notice = %q", h.model.notice.text)
				}
			},
		},
		{
			name: "draft_selection_cleared_once_opened",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.typeText("mini")
				h.press("enter")
				if footer := ansi.Strip(h.model.getFooterLine()); !strings.Contains(footer, "GPT Mini") {
					t.Fatalf("draft footer = %q", footer)
				}
				h.typeText("你好")
				h.press("enter")
				h.waitFor("draft flow finished", func() bool { return !h.model.isOpeningDraft && h.model.conversation != nil })
				if h.model.draftModel != (defs.ModelSelection{}) {
					t.Fatalf("draft model after the conversation opened = %+v", h.model.draftModel)
				}
			},
		},
		{
			// 新建后打开失败时保留草稿模型，重试请求仍使用该模型。
			name: "draft_selection_kept_when_open_fails",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.typeText("mini")
				h.press("enter")
				h.client.set(func(c *fakeClient) { c.openErr = errors.New("unreachable") })
				h.typeText("你好")
				h.press("enter")
				h.waitFor("draft restored", func() bool { return !h.model.isOpeningDraft && h.model.composer.getValue() == "你好" })
				if footer := ansi.Strip(h.model.getFooterLine()); !strings.Contains(footer, "GPT Mini") {
					t.Fatalf("footer after failed open = %q", footer)
				}
				h.client.set(func(c *fakeClient) { c.openErr = nil })
				h.press("enter")
				h.waitFor("created again", func() bool { return len(h.client.getCalls("CreateConversation")) == 2 })
				want := []string{"CreateConversation assistant openai/gpt-mini", "CreateConversation assistant openai/gpt-mini"}
				if calls := h.client.getCalls("CreateConversation"); !slices.Equal(calls, want) {
					t.Fatalf("creates = %v, want %v", calls, want)
				}
			},
		},
		{
			// 恢复已有对话时清除草稿模型，底栏等待根 Session 模型信息。
			name: "draft_selection_cleared_by_resume",
			run: func(t *testing.T, h *tuiHarness) {
				h.client.set(func(c *fakeClient) {
					c.conversations = []defs.Conversation{conversationWith("c7", "修复输入丢失", 0)}
				})
				openPanel(h)
				h.typeText("mini")
				h.press("enter")
				h.typeText("/resume")
				h.press("enter")
				h.waitFor("listed", func() bool { return strings.Contains(h.view(), "修复输入丢失") })
				h.press("enter")
				if footer := ansi.Strip(h.model.getFooterLine()); strings.Contains(footer, "GPT Mini") || !strings.Contains(footer, "GPT X") {
					t.Fatalf("footer while resuming = %q", footer)
				}
				h.waitFor("resumed", func() bool { return len(h.client.getCalls("OpenConversation")) == 1 })
			},
		},
		{
			name: "draft_selection_cleared_by_new",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.typeText("mini")
				h.press("enter")
				h.typeText("/new")
				h.press("enter")
				if h.model.draftModel != (defs.ModelSelection{}) {
					t.Fatalf("draft model after /new = %+v", h.model.draftModel)
				}
			},
		},
		{
			name: "confirm_while_opening_draft_is_refused",
			run: func(t *testing.T, h *tuiHarness) {
				release := h.holdDraftCreation("一")
				openPanel(h)
				h.typeText("mini")
				h.press("enter")
				if h.model.activePanel == nil || h.model.draftModel != (defs.ModelSelection{}) || h.model.notice.text != "正在新建对话，请稍候" {
					t.Fatalf("panel = %v, draft model = %+v, notice = %q", h.model.activePanel, h.model.draftModel, h.model.notice.text)
				}
				release()
				h.waitFor("draft flow finished", func() bool { return !h.model.isOpeningDraft })
			},
		},
		{
			// 切换对话之后才到达的模型切换结果按请求序号丢弃，不再提示"已切换到 X"。
			name: "set_result_after_conversation_switch_is_dropped",
			run: func(t *testing.T, h *tuiHarness) {
				openWithModel(h, false)
				openPanel(h)
				h.press("down", "enter")
				seq := h.model.modelSetSeq
				if seq == 0 {
					t.Fatal("setSessionModel did not record its request sequence")
				}
				h.typeText("/new")
				h.press("enter")
				h.send(modelSetMsg{
					seq:     seq,
					session: defs.Session{Model: defs.ModelSelection{Ref: gptX}},
				})
				if h.model.notice.text != "" {
					t.Fatalf("stale result showed notice %q", h.model.notice.text)
				}
			},
		},
		{
			name: "model_change_history_uses_display_name",
			run: func(t *testing.T, h *tuiHarness) {
				h.emit(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionConnected})
				h.waitFor("names loaded", func() bool { return h.model.modelDisplayNames[gptX] == "GPT X" })
				h.openDraftConversation("开场")
				change := &defs.Item{
					ID:     "x1",
					TurnID: "t2",
					Kind:   defs.ItemModelChange,
					ModelChange: &defs.ModelChange{
						PreviousModel: defs.ModelRef{
							ProviderID: "local",
							ModelID:    "qwen",
						},
						Model: gptX,
					},
				}
				h.emit(&runtimeclient.ItemsSaved{
					ConversationID: "c1",
					Items:          logItems(change),
				})
				if !strings.Contains(h.getPrinted(), "已切换到 GPT X") {
					t.Fatalf("printed = %q", h.getPrinted())
				}
			},
		},
	}
	// 3. 每个场景在一个新的、带测试模型目录的 80x24 TUI 里操作，检查模型选择和客户端请求。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTUIHarness(t, 80, 24, func(c *fakeClient) { c.modelList = catalog }))
		})
	}
}
