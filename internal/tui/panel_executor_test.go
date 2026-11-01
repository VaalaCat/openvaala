package tui

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestExecutorPanel 检查 Executor 过滤、勾选、默认项提交和加载失败。
func TestExecutorPanel(t *testing.T) {
	// 1. 准备打开面板和读取已提交设置的方法。
	cwd := map[string]string{"local": tuiTestWorkspaceDirectory}
	// 打开 Executor 面板并等待列表加载。
	openPanel := func(h *tuiHarness) *executorPanel {
		h.typeText("/executor")
		h.press("enter")
		opened := h.model.activePanel.(*executorPanel)
		h.waitFor("executors listed", func() bool { return !opened.fetch.isLoading })
		return opened
	}
	// 读取客户端收到的 Executor 设置。
	getSent := func(h *tuiHarness) []defs.ExecutorSettings {
		var sent []defs.ExecutorSettings
		h.client.set(func(c *fakeClient) { sent = slices.Clone(c.settingsRequests) })
		return sent
	}
	// 2. 准备列表显示、设置修改和请求失败场景。
	tests := []struct {
		name string
		run  func(t *testing.T, h *tuiHarness)
	}{
		{
			// 默认 Executor 标"默认"，目录列是当前设置下的目录；取消勾选的 gpu 标 ○。
			name: "render_marks_default_scope_and_directories",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.press("down", "down", "space")
				requireGolden(t, "executor_panel_80x24", h.view())
			},
		},
		{
			// Space 只在面板里切换，不提交；全部可用时取消一项会展开成其余各项。
			name: "space_toggles_without_submitting",
			run: func(t *testing.T, h *tuiHarness) {
				opened := openPanel(h)
				h.press("down", "down", "space")
				if want := []string{"local", "openvaala"}; !slices.Equal(opened.settings.ExecutorIDs, want) {
					t.Fatalf("scope after unchecking gpu = %v, want %v", opened.settings.ExecutorIDs, want)
				}
				h.press("space")
				if want := []string{"local", "openvaala", "gpu"}; !slices.Equal(opened.settings.ExecutorIDs, want) {
					t.Fatalf("scope after checking gpu again = %v, want %v", opened.settings.ExecutorIDs, want)
				}
				if h.model.activePanel != opened || len(h.client.getCalls("SetConversationExecutorSettings")) != 0 ||
					!reflect.DeepEqual(h.model.draftExecutorSettings, defs.ExecutorSettings{WorkspaceDirectories: cwd}) {
					t.Fatalf("panel = %v, calls = %v, draft = %+v", h.model.activePanel, h.client.getCalls(), h.model.draftExecutorSettings)
				}
			},
		},
		{
			// 默认 Executor（这里是列表里第一个在范围内的 local）不能取消。
			name: "default_cannot_be_unchecked",
			run: func(t *testing.T, h *tuiHarness) {
				opened := openPanel(h)
				h.press("space")
				if opened.settings.ExecutorIDs != nil {
					t.Fatalf("scope after toggling the default = %v", opened.settings.ExecutorIDs)
				}
			},
		},
		{
			// Enter 把光标所在项设为默认并勾上，按列表顺序存勾上的 ID，对已打开对话只提交一次。
			name: "enter_sets_cursor_default_and_submits_once",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				openPanel(h)
				h.press("down", "space", "down", "space", "enter")
				h.waitFor("settings set", func() bool { return h.model.notice.text != "" })
				want := defs.ExecutorSettings{
					DefaultExecutorID:    "gpu",
					ExecutorIDs:          []string{"local", "gpu"},
					WorkspaceDirectories: cwd,
				}
				if calls := h.client.getCalls("SetConversationExecutorSettings"); !slices.Equal(calls, []string{"SetConversationExecutorSettings c1"}) || !reflect.DeepEqual(getSent(h), []defs.ExecutorSettings{want}) {
					t.Fatalf("set calls = %v, settings = %+v", calls, getSent(h))
				}
				if h.model.activePanel != nil || h.model.notice.text != "工作位置：gpu:/data/proj，下一轮生效" {
					t.Fatalf("panel = %v, notice = %q", h.model.activePanel, h.model.notice.text)
				}
			},
		},
		{
			// 全部勾上时 ExecutorIDs 存空：以后新加的 Executor 也可用。草稿只改草稿设置，不调用 set。
			name: "all_checked_stores_empty_scope",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.press("down", "enter")
				want := defs.ExecutorSettings{
					DefaultExecutorID:    "openvaala",
					WorkspaceDirectories: cwd,
				}
				if !reflect.DeepEqual(h.model.draftExecutorSettings, want) || len(h.client.getCalls("SetConversationExecutorSettings")) != 0 {
					t.Fatalf("draft = %+v, calls = %v", h.model.draftExecutorSettings, h.client.getCalls())
				}
			},
		},
		{
			// 拉取失败时丢掉之前的列表：面板只显示错误、没有可选的行，Enter 不提交。
			name: "fetch_failure_shows_only_error",
			run: func(t *testing.T, h *tuiHarness) {
				h.openDraftConversation("开场")
				h.client.set(func(c *fakeClient) { c.listExecutorsErr = errors.New("boom") })
				openPanel(h)
				view := h.view()
				if !strings.Contains(view, "✗ boom") || strings.Contains(view, "● local") || strings.Contains(view, "没有匹配") {
					t.Fatalf("panel after a failed fetch:\n%s", view)
				}
				h.press("enter")
				if calls := h.client.getCalls("SetConversationExecutorSettings"); len(calls) != 0 || h.model.executors != nil {
					t.Fatalf("set calls = %v, executors = %+v", calls, h.model.executors)
				}
			},
		},
		{
			// 面板开着时列表被重连拉取的更短列表替换：光标收回到最后一项，Space、Enter 作用于它。
			name: "shrunk_list_clamps_cursor",
			run: func(t *testing.T, h *tuiHarness) {
				opened := openPanel(h)
				h.press("down", "down")
				h.send(executorsLoadedMsg{executors: testExecutors[:2]})
				h.press("space")
				if want := []string{"local"}; !slices.Equal(opened.settings.ExecutorIDs, want) {
					t.Fatalf("scope after toggling the clamped item = %v, want %v", opened.settings.ExecutorIDs, want)
				}
				h.press("enter")
				want := defs.ExecutorSettings{
					DefaultExecutorID:    "openvaala",
					WorkspaceDirectories: cwd,
				}
				if !reflect.DeepEqual(h.model.draftExecutorSettings, want) {
					t.Fatalf("draft = %+v, want %+v", h.model.draftExecutorSettings, want)
				}
			},
		},
		{
			// 打字按 ID 和说明过滤。
			name: "typing_filters_by_description",
			run: func(t *testing.T, h *tuiHarness) {
				openPanel(h)
				h.typeText("训练")
				h.press("enter")
				if h.model.draftExecutorSettings.DefaultExecutorID != "gpu" {
					t.Fatalf("draft = %+v", h.model.draftExecutorSettings)
				}
			},
		},
	}
	// 3. 每个场景在一个新的 80x24 TUI 里运行：带测试 Executor 列表，并连上本机服务。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTUIHarness(t, 80, 24, func(c *fakeClient) { c.executors = testExecutors })
			h.connectLocalServer()
			tt.run(t, h)
		})
	}
}
