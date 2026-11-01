package desktop

import (
	"strings"
	"testing"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

// TestCommandList 检查命令面板按当前状态列出命令：压缩只在可压缩时出现，停止只在有运行中 Turn 时出现。
func TestCommandList(t *testing.T) {
	// 1. 同一套固定命令加上随状态出现的命令。
	for _, tt := range []struct {
		name    string
		prepare func(d *Desktop)
		want    []string
		missing []string
	}{
		{"fixed", func(d *Desktop) {}, []string{commandNewConversation, commandSearch, commandSettings, commandToggleSidebar, commandToggleRightDock, commandToggleBottomDock, "panel.agent", "panel.context", "panel.turns", "panel.rpc", "panel.debug"}, []string{commandCompactContext, commandStop}},
		{"idle conversation", func(d *Desktop) { openIdleConversation(d, "") }, []string{commandCompactContext}, []string{commandStop}},
		{"running turn", func(d *Desktop) { openIdleConversation(d, "turn") }, []string{commandStop}, []string{commandCompactContext}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tt.prepare(d)
			ids := map[string]bool{}
			for _, command := range d.getCommands(nil) {
				ids[command.ID] = true
			}
			for _, id := range tt.want {
				if !ids[id] {
					t.Fatalf("missing command %q in %v", id, ids)
				}
			}
			for _, id := range tt.missing {
				if ids[id] {
					t.Fatalf("unexpected command %q", id)
				}
			}
		})
	}
}

// TestRunCommand 检查命令只改变对应的工作台状态。
func TestRunCommand(t *testing.T) {
	// 1. 每条命令执行后检查布局偏好或面板状态。
	for _, tt := range []struct {
		name    string
		command string
		check   func(d *Desktop) bool
	}{
		{"palette", commandPalette, func(d *Desktop) bool { return d.commandPaletteOpen }},
		{"sidebar", commandToggleSidebar, func(d *Desktop) bool { return !d.preferences.Workspace.SidebarOpen }},
		{"right dock", commandToggleRightDock, func(d *Desktop) bool { return !d.preferences.Workspace.Right.Open }},
		{"bottom dock", commandToggleBottomDock, func(d *Desktop) bool { return d.preferences.Workspace.Bottom.Open }},
		{"search opens sidebar", commandSearch, func(d *Desktop) bool { return d.preferences.Workspace.SidebarOpen }},
		{"settings", commandSettings, func(d *Desktop) bool { return d.page == "general" }},
		{"context panel", commandPanelPrefix + panelContext, func(d *Desktop) bool {
			return d.preferences.Workspace.Right.Open && d.preferences.Workspace.Right.ActivePanel == panelContext
		}},
		{"debug panel", commandPanelPrefix + panelDebug, func(d *Desktop) bool {
			return d.preferences.Workspace.Right.Open && d.preferences.Workspace.Right.ActivePanel == panelDebug
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.runCommand(tt.command)
			if !tt.check(d) {
				t.Fatalf("workspace after %s = %+v page=%q", tt.command, d.preferences.Workspace, d.page)
			}
		})
	}
	// 2. 设置页里再次执行设置命令回到对话。
	d := testDesktop()
	d.page = "general"
	d.runCommand(commandSettings)
	if d.page != "" {
		t.Fatalf("page = %q", d.page)
	}
}

// TestLoadShownPanels 检查面板只在新显示时读取：切到 RPC 面板会请求日志，保持显示不重复请求。
func TestLoadShownPanels(t *testing.T) {
	// 1. 逐帧记录 Dock 显示的面板。
	d := testDesktop()
	d.preferences.Workspace = kit.WorkspaceState{Right: kit.DockState{Open: true, ActivePanel: panelAgent}}
	d.loadShownPanels()
	if d.shownRightPanel != panelAgent || d.shownBottomPanel != "" {
		t.Fatalf("shown = %q %q", d.shownRightPanel, d.shownBottomPanel)
	}
	d.openPanel(panelRPC)
	d.loadShownPanels()
	if d.shownRightPanel != panelRPC {
		t.Fatalf("right = %q", d.shownRightPanel)
	}
}

// TestRunCommandInNarrowWindow 检查窄窗口里的命令打开侧栏和右侧抽屉，而不是只改写已保存的布局。
func TestRunCommandInNarrowWindow(t *testing.T) {
	// 1. 窄窗口启动时两个抽屉都关闭；命令执行后下一帧能看到抽屉内容。
	for _, tt := range []struct {
		name    string
		command string
		want    func(d *Desktop) string
	}{
		{"context panel", commandPanelPrefix + panelContext, func(d *Desktop) string { return d.t("panel.context.empty") }},
		{"toggle right dock", commandToggleRightDock, func(d *Desktop) string { return d.t("panel.agent.empty") }},
		{"toggle sidebar", commandToggleSidebar, func(d *Desktop) string { return d.t("search.conversations") }},
		{"search conversations", commandSearch, func(d *Desktop) string { return d.t("search.conversations") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tester := ui.NewTester(d.view, 680, 820)
			want := tt.want(d)
			if tester.HasText(want) {
				t.Fatalf("%q visible before the command", want)
			}
			saved := d.preferences.Workspace
			d.runCommand(tt.command)
			tester.Frame()
			if !tester.HasText(want) {
				t.Fatalf("%q not visible after %s: %v", want, tt.command, tester.Texts())
			}
			if d.preferences.Workspace.SidebarOpen != saved.SidebarOpen || d.preferences.Workspace.Right.Open != saved.Right.Open {
				t.Fatalf("saved layout changed: %+v", d.preferences.Workspace)
			}
			if tt.command == commandPanelPrefix+panelContext {
				d.loadShownPanels()
				if d.shownRightPanel != panelContext {
					t.Fatalf("drawer panel not loaded: %q", d.shownRightPanel)
				}
			}
		})
	}
}

// openIdleConversation 准备一个已同步的根 Session；turnID 非空时它有运行中的 Turn。
func openIdleConversation(d *Desktop, turnID defs.TurnID) {
	d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
	d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
	d.readingReady = true
	d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "root", ConversationID: "conversation"}, ActiveTurnID: turnID}}
}

// TestCommandTitles 检查命令标题不带冒号拼接；会话上的命令只在打开对话后出现，跳转命令列出其他对话，快捷键显示在命令上。
func TestCommandTitles(t *testing.T) {
	d := testDesktop()
	d.conversations = []dao.ConversationListEntry{
		{Conversation: defs.Conversation{ID: "conversation", Title: "当前对话"}},
		{Conversation: defs.Conversation{ID: "other", Title: "另一个对话"}},
	}
	titles := func() map[string]kit.Command {
		result := map[string]kit.Command{}
		for _, command := range d.getCommands(nil) {
			result[command.ID] = command
			if strings.ContainsAny(command.Title, ":：") {
				t.Fatalf("command %q title %q has a colon", command.ID, command.Title)
			}
		}
		return result
	}
	before := titles()
	for _, id := range []string{commandRename, commandArchive, commandFork, commandExport, commandDelete, commandPreviousConversation} {
		if _, found := before[id]; found {
			t.Fatalf("%q is listed without an open conversation", id)
		}
	}
	if got := before[commandPanelPrefix+panelAgent].Title; got != "Agent 面板" {
		t.Fatalf("agent panel title = %q", got)
	}
	if got := before[commandPanelPrefix+panelContext].Title; got != "上下文面板" {
		t.Fatalf("context panel title = %q", got)
	}
	if got := before[commandJumpPrefix+"conversation"].Title; got != "当前对话" {
		t.Fatalf("jump to a conversation title = %q", got)
	}
	openIdleConversation(d, "turn")
	d.previousConversation = defs.Conversation{ID: "other", RootSessionID: "root-other"}
	after := titles()
	for _, id := range []string{commandRename, commandArchive, commandFork, commandExport, commandDelete, commandPreviousConversation, commandSwitchModel, commandOpenWorkLocation, commandNewTerminal, commandReloadConfig, commandSwitchTheme, commandSwitchLanguage} {
		if _, found := after[id]; !found {
			t.Fatalf("missing command %q", id)
		}
	}
	if _, found := after[commandJumpPrefix+"conversation"]; found {
		t.Fatal("the open conversation is offered as a jump target")
	}
	if after[commandStop].Keys != "Esc" || after[commandPreviousConversation].Keys == "" {
		t.Fatalf("shortcuts: stop=%q previous=%q", after[commandStop].Keys, after[commandPreviousConversation].Keys)
	}
}

// TestRunConversationCommands 检查会话命令各自触发对应的确认框、切换和跳转。
func TestRunConversationCommands(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command string
		check   func(d *Desktop) bool
	}{
		{"rename", commandRename, func(d *Desktop) bool { return d.confirmOpen && d.confirm.Kind == "rename" }},
		{"delete", commandDelete, func(d *Desktop) bool { return d.confirmOpen && d.confirm.Kind == "delete-conversation" }},
		{"previous", commandPreviousConversation, func(d *Desktop) bool { return d.readingTarget.ConversationID == "other" }},
		{"jump", commandJumpPrefix + "other", func(d *Desktop) bool { return d.readingTarget.ConversationID == "other" }},
		{"theme", commandSwitchTheme, func(d *Desktop) bool { return d.preferences.Theme == "light" }},
		{"language", commandSwitchLanguage, func(d *Desktop) bool { return d.preferences.Language == "en" }},
		{"new terminal", commandNewTerminal, func(d *Desktop) bool { return d.preferences.Workspace.Bottom.Open }},
		{"switch model", commandSwitchModel, func(d *Desktop) bool { return d.newModel == "" && len(d.pendingToasts) == 1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.conversations = []dao.ConversationListEntry{{Conversation: defs.Conversation{ID: "other", RootSessionID: "root-other"}}}
			openIdleConversation(d, "")
			d.previousConversation = defs.Conversation{ID: "other", RootSessionID: "root-other"}
			d.runCommand(tt.command)
			if !tt.check(d) {
				t.Fatalf("state after %s: confirm=%+v target=%+v theme=%q", tt.command, d.confirm, d.readingTarget, d.preferences.Theme)
			}
		})
	}
	// 切换对话时记住上一个，再按一次回到原对话。
	d := testDesktop()
	openIdleConversation(d, "")
	d.openConversation("other", "root-other")
	if d.previousConversation.ID != "conversation" {
		t.Fatalf("previous = %q", d.previousConversation.ID)
	}
}

// TestPaletteCommandFocus 检查命令面板执行命令后键盘焦点的归属：搜索命令把焦点留在搜索框，其他命令和 Esc 关闭回到输入框。
func TestPaletteCommandFocus(t *testing.T) {
	for _, tt := range []struct {
		name  string
		query string
		enter bool
		check func(d *Desktop) bool
	}{
		{"search conversations", "搜索对话", true, func(d *Desktop) bool {
			return d.conversationSearch == "abc" && draftText(d.drafts[draftKey{ServerID: "fixture"}].Content) == ""
		}},
		{"new conversation", "新对话", true, func(d *Desktop) bool {
			return d.conversationSearch == "" && draftText(d.drafts[draftKey{ServerID: "fixture"}].Content) == "abc"
		}},
		{"escape", "搜索对话", false, func(d *Desktop) bool {
			return d.conversationSearch == "" && draftText(d.drafts[draftKey{ServerID: "fixture"}].Content) == "abc"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			d.commandPaletteOpen = true
			tester.Frame()
			tester.Type(tt.query)
			if tt.enter {
				tester.Key(0, ui.KeyEnter)
			} else {
				tester.Key(0, ui.KeyEscape)
			}
			tester.Frame()
			tester.Type("abc")
			tester.Frame()
			if d.commandPaletteOpen || !tt.check(d) {
				t.Fatalf("search=%q draft=%+v", d.conversationSearch, d.drafts)
			}
		})
	}
}
