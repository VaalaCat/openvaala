package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/egoist/mygo/ui"
)

// TestNavigateWorkspace 检查进入设置页时右 Dock 暂时收起、侧栏暂时显示，回到对话后各自恢复；临时状态不写进偏好文件，原本的开关状态不被改动。
func TestNavigateWorkspace(t *testing.T) {
	for _, tt := range []struct {
		name             string
		dockOpen         bool
		sidebarOpen      bool
		pages            []string
		wantDockOpen     bool
		wantSidebarOpen  bool
		wantSavedDock    bool // 偏好文件里右 Dock 的 open
		wantSavedSidebar bool // 偏好文件里 sidebar_open
	}{
		{"open dock collapses in settings", true, true, []string{"general"}, false, true, true, true},
		{"open dock is restored after leaving settings", true, true, []string{"general", "hooks", ""}, true, true, true, true},
		{"closed dock stays closed", false, true, []string{"general", ""}, false, true, false, true},
		{"collapsed sidebar is shown in settings", false, false, []string{"general"}, false, true, false, false},
		{"collapsed sidebar stays shown between pages", false, false, []string{"general", "hooks"}, false, true, false, false},
		{"collapsed sidebar is restored after leaving settings", false, false, []string{"general", ""}, false, false, false, false},
		{"open sidebar is kept after leaving settings", false, true, []string{"general", ""}, false, true, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.preferencesPath = filepath.Join(t.TempDir(), "preferences.json")
			d.preferences.Workspace.Right = kit.DockState{Open: tt.dockOpen, ActivePanel: panelAgent}
			d.preferences.Workspace.SidebarOpen = tt.sidebarOpen
			for _, page := range tt.pages {
				d.navigate(page)
			}
			if got := d.preferences.Workspace.Right.Open; got != tt.wantDockOpen {
				t.Fatalf("right dock open = %v, want %v", got, tt.wantDockOpen)
			}
			if got := d.preferences.Workspace.SidebarOpen; got != tt.wantSidebarOpen {
				t.Fatalf("sidebar open = %v, want %v", got, tt.wantSidebarOpen)
			}
			if err := d.savePreferences(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(d.preferencesPath)
			if err != nil {
				t.Fatal(err)
			}
			var saved preferences
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Workspace.Right.Open != tt.wantSavedDock || saved.Workspace.SidebarOpen != tt.wantSavedSidebar {
				t.Fatalf("saved dock open = %v sidebar open = %v", saved.Workspace.Right.Open, saved.Workspace.SidebarOpen)
			}
		})
	}
}

// TestSettingsDrawerInNarrowWindow 检查窄窗口里侧栏是抽屉：进入设置时抽屉带着设置导航打开，Esc 回到对话时抽屉收起；点导航项同样收起。
func TestSettingsDrawerInNarrowWindow(t *testing.T) {
	for _, tt := range []struct {
		name string
		exit func(d *Desktop, tester *ui.Tester)
	}{
		{"leave with the command", func(d *Desktop, tester *ui.Tester) { d.runCommand(commandSettings) }},
		{"pick a page", func(d *Desktop, tester *ui.Tester) {
			if err := tester.Click("模型服务"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tester := ui.NewTester(d.view, 680, 820)
			tester.SetLocale("zh-Hans")
			d.runCommand(commandSettings)
			tester.Frame()
			if !tester.HasText("模型服务") {
				t.Fatalf("settings navigation is not shown in the drawer: %q", tester.Texts())
			}
			tt.exit(d, tester)
			tester.Frame()
			if tester.HasText("Executor") {
				t.Fatalf("the drawer stayed open: %q", tester.Texts())
			}
		})
	}
}

// TestSettingsNavigation 检查设置打开时左侧栏换成设置导航，五页各自渲染页标题和内容，点导航切页，返回按钮和 Esc 回到对话。
func TestSettingsNavigation(t *testing.T) {
	// 1. 每页：侧栏是导航（没有对话列表），中间区是 SettingPage。
	for _, tt := range []struct {
		page  string
		title string
		want  string
	}{
		{"general", "通用", "完成时发送系统通知"},
		{"providers", "模型服务", "重载配置"},
		{"executors", "Executor", "来自 config.yaml，编辑后点重载配置"},
		{"hooks", "Hook", "来自 config.yaml，编辑后点重载配置"},
		{"skills", "Skill", "来自 config.yaml，编辑后点重载配置"},
	} {
		t.Run(tt.page, func(t *testing.T) {
			d := testDesktop()
			d.conversations = []dao.ConversationListEntry{{Conversation: defs.Conversation{ID: "c1", Title: "侧栏对话"}}}
			d.page = tt.page
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			for _, text := range []string{"设置", "通用", "模型服务", "Executor", "Hook", "Skill", tt.want} {
				if !tester.HasText(text) {
					t.Fatalf("missing %q in %q", text, tester.Texts())
				}
			}
			if tester.HasText("侧栏对话") || tester.HasText("搜索对话") {
				t.Fatalf("conversation list still shown in settings: %q", tester.Texts())
			}
		})
	}
	// 2. 点导航切页；返回按钮、Esc 都回到对话并恢复对话列表。
	for _, leave := range []string{"back button", "escape"} {
		t.Run(leave, func(t *testing.T) {
			d := testDesktop()
			d.conversations = []dao.ConversationListEntry{{Conversation: defs.Conversation{ID: "c1", Title: "侧栏对话"}}}
			d.page = "general"
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			if err := tester.Click("Hook"); err != nil || d.page != "hooks" {
				t.Fatalf("page = %q, %v", d.page, err)
			}
			if leave == "back button" {
				if err := tester.Click("返回对话"); err != nil {
					t.Fatal(err)
				}
			} else {
				tester.Key(0, ui.KeyEscape)
			}
			if d.page != "" {
				t.Fatalf("page = %q after leaving", d.page)
			}
			tester.Frame()
			if !tester.HasText("侧栏对话") {
				t.Fatalf("conversation list not restored: %q", tester.Texts())
			}
		})
	}
}

// TestGeneralSettings 检查通用页：外观分段、减少动画和系统通知开关都写进偏好；通知默认开启。
func TestGeneralSettings(t *testing.T) {
	d := testDesktop()
	if !d.preferences.NotifyOnFinish {
		t.Fatal("notifications are off by default")
	}
	d.navigate("general")
	tester := ui.NewTester(d.view, 1280, 820)
	tester.SetLocale("zh-Hans")
	// 开关在设置行右端：点标题所在行靠右的位置（侧栏 241 + 页边距 24 + 内容 672 - 行内边距 12 - 半个开关 18）。
	for _, tt := range []struct {
		name  string
		click func() error
		check func() bool
	}{
		{"dark", func() error { return tester.Click("深色") }, func() bool { return d.preferences.Theme == "dark" }},
		{"notification switch", func() error { return clickSettingSwitch(tester, "完成时发送系统通知") }, func() bool { return !d.preferences.NotifyOnFinish }},
		{"reduce motion switch", func() error { return clickSettingSwitch(tester, "减少动画") }, func() bool { return d.preferences.ReduceMotion }},
	} {
		if err := tt.click(); err != nil {
			t.Fatal(err)
		}
		if !tt.check() {
			t.Fatalf("%s did not update the preferences: %+v", tt.name, d.preferences)
		}
	}
}

// clickSettingSwitch 点击标题所在设置行右端的开关。
func clickSettingSwitch(tester *ui.Tester, title string) error {
	rect, found := tester.Find(title)
	if !found {
		return fmt.Errorf("no row %q", title)
	}
	tester.ClickAt(907, rect.Y+rect.H/2)
	return nil
}

// TestSettingsItems 检查 Executor、Hook、Skill 页：一项一行，点开才显示详情，原始数据默认收起。
func TestSettingsItems(t *testing.T) {
	for _, tt := range []struct {
		page    string
		prepare func(d *Desktop)
		row     string
		detail  string
	}{
		{"executors", func(d *Desktop) {
			d.executors = []conversation.ExecutorDetails{{ID: "sandbox", Description: "第二个执行器", WorkspaceDirectory: "/work/sandbox"}}
		}, "sandbox", "/work/sandbox"},
		{"hooks", func(d *Desktop) {
			d.hooks = []*hook.HookInfo{{Key: "k", Event: "pre_tool_use", Matcher: "bash", Handler: "echo hi", TimeoutSeconds: 5, SourcePath: "/etc/hook.yaml"}}
		}, "pre_tool_use", "/etc/hook.yaml"},
		{"skills", func(d *Desktop) {
			d.configuredSkills = []skill.SkillInfo{{Name: "review", Description: "审查代码", Path: "/skills/review/SKILL.md", Executor: "local"}}
		}, "review", "/skills/review/SKILL.md"},
	} {
		t.Run(tt.page, func(t *testing.T) {
			d := testDesktop()
			d.page = tt.page
			tt.prepare(d)
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			if tester.HasText(tt.detail) || tester.HasText("原始数据") {
				t.Fatalf("details shown before the row is opened: %q", tester.Texts())
			}
			if err := tester.Click(tt.row); err != nil {
				t.Fatal(err)
			}
			tester.Frame()
			if !tester.HasText(tt.detail) || !tester.HasText("原始数据") {
				t.Fatalf("details missing after opening the row: %q", tester.Texts())
			}
			if tester.HasText("\"name\"") && tt.page == "skills" {
				t.Fatal("raw data is expanded by default")
			}
		})
	}
}
