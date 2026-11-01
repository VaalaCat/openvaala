package desktop

import (
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/egoist/mygo/ui"
)

// 命令面板与菜单加速键共用的命令 ID。
const (
	commandPalette              = "palette"
	commandNewConversation      = "new-conversation"
	commandSearch               = "search-conversations"
	commandSettings             = "settings"
	commandToggleSidebar        = "toggle-sidebar"
	commandToggleRightDock      = "toggle-right-dock"
	commandToggleBottomDock     = "toggle-bottom-dock"
	commandTerminal             = "terminal"
	commandNewTerminal          = "new-terminal"
	commandCompactContext       = "compact-context"
	commandStop                 = "stop"
	commandPreviousConversation = "previous-conversation"
	commandRename               = "rename"
	commandArchive              = "archive"
	commandFork                 = "fork"
	commandExport               = "export"
	commandDelete               = "delete"
	commandSwitchModel          = "switch-model"
	commandSwitchReasoning      = "switch-reasoning"
	commandOpenWorkLocation     = "open-work-location"
	commandReloadConfig         = "reload-config"
	commandSwitchTheme          = "switch-theme"
	commandSwitchLanguage       = "switch-language"
	commandPanelPrefix          = "panel."
	commandJumpPrefix           = "jump:"
)

// getCommands 返回此刻可见的命令；只在可执行时才出现的命令（打开了对话、有运行中的 Turn 等）按条件列出。
func (d *Desktop) getCommands(c *ui.Context) []kit.Command {
	// 1. 对话命令：新建、搜索、上一个对话，以及当前对话上的操作。
	conversationGroup, viewGroup, composerGroup, terminalGroup, settingsGroup := d.t("conversations"), d.t("view"), d.t("composer"), d.t("terminal"), d.t("settings")
	commands := []kit.Command{
		{ID: commandNewConversation, Title: d.t("new.conversation"), Group: conversationGroup, Keys: shortcutHint("N", false), Icon: kit.IconPlus},
		{ID: commandSearch, Title: d.t("search.conversations"), Group: conversationGroup, Icon: kit.IconSearch},
	}
	if d.previousConversation.ID != "" {
		commands = append(commands, kit.Command{ID: commandPreviousConversation, Title: d.t("command.previous.conversation"), Group: conversationGroup, Keys: previousConversationHint(), Icon: kit.IconMessageSquare})
	}
	canEditConversation := d.page == "" && d.conversation.ID != "" && !d.isReadingChildSession()
	if canEditConversation {
		commands = append(commands,
			kit.Command{ID: commandRename, Title: d.t("rename.conversation"), Group: conversationGroup, Icon: kit.IconPencil},
			kit.Command{ID: commandArchive, Title: d.t("command.archive"), Group: conversationGroup, Icon: kit.IconArchive},
			kit.Command{ID: commandFork, Title: d.t("command.fork"), Group: conversationGroup, Icon: kit.IconGitFork},
			kit.Command{ID: commandExport, Title: d.t("export.markdown"), Group: conversationGroup, Icon: kit.IconFileText},
			kit.Command{ID: commandDelete, Title: d.t("command.delete"), Group: conversationGroup, Icon: kit.IconTrash},
		)
	}
	if d.isCompactAvailable() {
		commands = append(commands, kit.Command{ID: commandCompactContext, Title: d.t("compact.context"), Group: conversationGroup, Icon: kit.IconFold})
	}
	if d.getSessionInfo().ActiveTurnID != "" {
		commands = append(commands, kit.Command{ID: commandStop, Title: d.t("stop"), Group: conversationGroup, Keys: "Esc", Icon: kit.IconStop})
	}
	// 2. 输入区命令：只在输入区可见时列出。
	if d.page == "" && !d.isReadingChildSession() {
		model, _ := d.getModelSelection()
		commands = append(commands, kit.Command{ID: commandSwitchModel, Title: d.t("command.switch.model"), Group: composerGroup, Icon: kit.IconBot})
		if len(getReasoningEfforts(d.models, model)) > 0 {
			commands = append(commands, kit.Command{ID: commandSwitchReasoning, Title: d.t("command.switch.reasoning"), Group: composerGroup, Icon: kit.IconBrain})
		}
		commands = append(commands, kit.Command{ID: commandOpenWorkLocation, Title: d.t("command.open.workspace"), Group: composerGroup, Icon: kit.IconFolder})
	}
	// 3. 终端命令。
	commands = append(commands,
		kit.Command{ID: commandTerminal, Title: d.t("terminal.focus"), Group: terminalGroup, Keys: shortcutHint("`", false), Icon: kit.IconTerminal},
		kit.Command{ID: commandNewTerminal, Title: d.t("terminal.new"), Group: terminalGroup, Icon: kit.IconPlus},
	)
	// 4. 视图与面板：面板名不带冒号，「Agent 面板」。
	commands = append(commands,
		kit.Command{ID: commandToggleSidebar, Title: d.t("toggle.sidebar"), Group: viewGroup, Keys: shortcutHint("B", false), Icon: kit.IconPanelLeft},
		kit.Command{ID: commandToggleRightDock, Title: d.t("toggle.right.dock"), Group: viewGroup, Keys: shortcutHint("R", true), Icon: kit.IconPanelRight},
		kit.Command{ID: commandToggleBottomDock, Title: d.t("toggle.bottom.dock"), Group: viewGroup, Keys: shortcutHint("J", false), Icon: kit.IconPanelBottom},
	)
	for _, panel := range append(d.getRightPanels(c), d.getBottomPanels(c)...) {
		commands = append(commands, kit.Command{ID: commandPanelPrefix + panel.ID, Title: d.getPanelCommandTitle(panel.Title), Group: viewGroup, Icon: panel.Icon})
	}
	// 5. 设置：打开设置、重载配置、切换主题与语言。
	commands = append(commands,
		kit.Command{ID: commandSettings, Title: d.t("settings"), Group: settingsGroup, Keys: shortcutHint(",", false), Icon: kit.IconSettings},
		kit.Command{ID: commandReloadConfig, Title: d.t("reload.configuration"), Group: settingsGroup, Icon: kit.IconRefresh, Disabled: d.client == nil},
		kit.Command{ID: commandSwitchTheme, Title: d.t("command.switch.theme"), Group: settingsGroup, Icon: kit.IconSettings},
		kit.Command{ID: commandSwitchLanguage, Title: d.t("command.switch.language"), Group: settingsGroup, Icon: kit.IconSettings},
	)
	// 6. 跳转到已加载列表里的对话，标题就是命令名，只按标题搜索。
	for _, entry := range d.conversations {
		if entry.Conversation.ID != d.conversation.ID {
			commands = append(commands, kit.Command{ID: commandJumpPrefix + string(entry.Conversation.ID), Title: conversationTitle(entry.Conversation), Group: d.t("command.group.jump"), Icon: kit.IconMessageSquare})
		}
	}
	return commands
}

// runCommand 执行命令面板或菜单加速键选定的命令。
func (d *Desktop) runCommand(commandID string) {
	// 1. 全局入口：面板、新对话、设置、搜索。
	workspace := &d.preferences.Workspace
	switch commandID {
	case commandPalette:
		d.commandPaletteOpen = true
	case commandNewConversation:
		d.startNewConversation()
	case commandSettings:
		if d.page != "" {
			d.navigate("")
		} else {
			d.navigate("general")
		}
	case commandSearch:
		workspace.ShowSidebar()
		d.conversationSearchHandle.Focus()
	// 2. 工作台开关：侧栏与两个 Dock。
	case commandToggleSidebar:
		d.sidebarDrawerOpenedBySettings = false
		workspace.ToggleSidebar()
	case commandToggleRightDock:
		workspace.ToggleDock(&workspace.Right)
	case commandToggleBottomDock:
		workspace.ToggleDock(&workspace.Bottom)
	case commandTerminal:
		// 终端面板已显示时把焦点交给当前标签，没有标签就新开一个；打开面板本身会在下一帧自动开第一个终端，这里先开则不会重复。
		d.openPanel(panelTerminal)
		if tab := d.getSelectedTerminal(); tab != nil {
			tab.FocusRequested = true
		} else {
			d.openTerminal()
		}
	case commandNewTerminal:
		d.openPanel(panelTerminal)
		d.openTerminal()
	// 3. 当前对话上的操作。
	case commandCompactContext:
		d.compactContext()
	case commandStop:
		d.interruptTurn(d.readingTarget.ConversationID)
	case commandRename:
		d.requestRename(d.conversation)
	case commandArchive:
		d.archiveConversation(d.conversation)
	case commandFork:
		d.forkConversation(d.conversation, "")
	case commandExport:
		d.exportConversation(d.conversation)
	case commandDelete:
		d.requestDelete(d.conversation)
	case commandPreviousConversation:
		d.openConversationFromList(d.previousConversation)
	// 4. 输入区：切换模型和推理强度逐档轮换并提示结果，工作位置打开表单或弹窗。
	case commandSwitchModel:
		d.cycleModel()
	case commandSwitchReasoning:
		d.cycleReasoning()
	case commandOpenWorkLocation:
		d.openWorkLocationFor(true)
	// 5. 设置。
	case commandReloadConfig:
		d.reloadConfiguration()
	case commandSwitchTheme:
		d.cycleTheme()
	case commandSwitchLanguage:
		d.cycleLanguage()
	default:
		d.runDynamicCommand(commandID)
	}
}

// isCommandFocusing 命令是否自己把键盘焦点交给搜索框、终端或标题输入框？
func isCommandFocusing(commandID string) bool {
	switch commandID {
	case commandSearch, commandTerminal, commandNewTerminal, commandRename:
		return true
	}
	return false
}

// runDynamicCommand 执行 ID 带前缀的命令：打开面板，或跳转到对话。
func (d *Desktop) runDynamicCommand(commandID string) {
	if panelID, found := strings.CutPrefix(commandID, commandPanelPrefix); found {
		d.openPanel(panelID)
		return
	}
	conversationID, found := strings.CutPrefix(commandID, commandJumpPrefix)
	if !found {
		return
	}
	if index := slices.IndexFunc(d.conversations, func(entry dao.ConversationListEntry) bool { return string(entry.Conversation.ID) == conversationID }); index >= 0 {
		d.openConversationFromList(d.conversations[index].Conversation)
	}
}

// cycleModel 换成下一个模型：Agent 默认之后依次是各服务的模型，最后回到 Agent 默认。
func (d *Desktop) cycleModel() {
	// 1. 候选是 Agent 默认加目录里的全部模型。
	candidates := []string{""}
	for _, model := range d.models.Models {
		if model != nil {
			candidates = append(candidates, model.Ref.String())
		}
	}
	model, effort := d.getModelSelection()
	next := candidates[(slices.Index(candidates, model)+1)%len(candidates)]
	// 2. 新模型不支持当前推理强度时恢复自动。
	if !slices.Contains(getReasoningEfforts(d.models, next), effort) {
		effort = ""
	}
	d.setModelSelection(next, effort)
	d.showToast(kit.ToastOpt{Title: fmt.Sprintf(d.t("command.model.switched"), getModelTooltip(d.models.Models, next, d.t("agent.default"))), Kind: kit.ToastInfo})
}

// cycleReasoning 换成所选模型的下一档推理强度：自动之后依次是各档，最后回到自动。
func (d *Desktop) cycleReasoning() {
	model, effort := d.getModelSelection()
	candidates := append([]string{""}, getReasoningEfforts(d.models, model)...)
	next := candidates[(slices.Index(candidates, effort)+1)%len(candidates)]
	d.setModelSelection(model, next)
	label := d.t("automatic")
	if next != "" {
		label = d.getEffortLabel(next)
	}
	d.showToast(kit.ToastOpt{Title: fmt.Sprintf(d.t("command.reasoning.switched"), label), Kind: kit.ToastInfo})
}

// cycleTheme 按跟随系统、浅色、深色的顺序切换外观并保存偏好。
func (d *Desktop) cycleTheme() {
	themes := []string{"system", "light", "dark"}
	d.preferences.Theme = themes[(slices.Index(themes, d.preferences.Theme)+1)%len(themes)]
	d.showToast(kit.ToastOpt{Title: fmt.Sprintf(d.t("command.theme.switched"), d.t(d.preferences.Theme)), Kind: kit.ToastInfo})
	d.savePreferencesWithToast()
}

// cycleLanguage 在简体中文和 English 之间切换并保存偏好。
func (d *Desktop) cycleLanguage() {
	if d.preferences.Language == "en" {
		d.preferences.Language = "zh-CN"
	} else {
		d.preferences.Language = "en"
	}
	d.installMenu()
	d.savePreferencesWithToast()
}

// savePreferencesWithToast 保存偏好，失败时用通知说明。
func (d *Desktop) savePreferencesWithToast() {
	if err := d.savePreferences(); err != nil {
		d.showToast(kit.ToastOpt{Title: d.t("preferences.were.not.saved"), Description: err.Error(), Kind: kit.ToastError})
	}
}

// openPanel 显示面板：打开所在的 Dock，窄窗口里的右侧面板打开抽屉。
func (d *Desktop) openPanel(panelID string) {
	workspace := &d.preferences.Workspace
	dock := &workspace.Right
	if slices.Contains(bottomPanelIDs, panelID) {
		dock = &workspace.Bottom
	}
	workspace.ShowPanel(dock, panelID)
}

// isCompactAvailable 当前阅读的根 Session 是否可以压缩上下文？
func (d *Desktop) isCompactAvailable() bool {
	return d.conversation.ID != "" && d.readingTarget.SessionID == d.conversation.RootSessionID && d.connectionState == runtimeclient.ConnectionConnected && d.readingReady && d.getSessionInfo().ActiveTurnID == ""
}

// getPanelCommandTitle 返回打开面板命令的标题，例如「Agent 面板」「上下文面板」；中文里汉字和汉字之间不加空格。
func (d *Desktop) getPanelCommandTitle(panelTitle string) string {
	if d.preferences.Language == "zh-CN" && panelTitle != "" && panelTitle[0] >= 0x80 {
		return panelTitle + "面板"
	}
	return fmt.Sprintf(d.t("panel.title"), panelTitle)
}

// getRightPanels 返回右 Dock 的面板。
func (d *Desktop) getRightPanels(c *ui.Context) []kit.Panel {
	return []kit.Panel{
		{ID: panelAgent, Title: "Agent", Icon: kit.IconBot, Content: func() { d.agentPanel(c) }},
		{ID: panelContext, Title: d.t("context"), Icon: kit.IconGauge, Content: func() { d.contextPanel(c) }},
		{ID: panelTurns, Title: "Turn", Icon: kit.IconListTree, Content: func() { d.turnsPanel(c) }, Actions: func() { d.turnsRefreshView(c) }},
		{ID: panelRPC, Title: "RPC", Icon: kit.IconActivity, Content: func() { d.rpcPanel(c) }},
		{ID: panelDebug, Title: d.t("debug"), Icon: kit.IconBug, Content: func() { d.debugPanel(c) }},
	}
}

// getBottomPanels 返回下 Dock 的面板。
func (d *Desktop) getBottomPanels(c *ui.Context) []kit.Panel {
	return []kit.Panel{
		{ID: panelTerminal, Title: d.t("terminal"), Icon: kit.IconTerminal, Content: func() { d.terminalPanel(c) }, Header: func() { d.terminalTabsView(c) }, Actions: func() { d.terminalActionsView(c) }},
	}
}
