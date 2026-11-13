package desktop

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

// settingsPages 是设置导航的页面顺序。
var settingsPages = []string{"general", "providers", "executors", "hooks", "skills"}

// settingControlWidth 是设置行右侧下拉和输入控件的统一宽度。
const settingControlWidth = 200

// navigate 切换设置页面；未保存 Provider 原文需要确认放弃。
func (d *Desktop) navigate(page string) {
	// 1. 页面变更前检查编辑草稿。
	if page != d.page && d.isProviderDirty() {
		d.confirm = confirmation{Kind: "navigate", Page: page}
		d.confirmOpen = true
		return
	}
	// 2. 进入设置页时右 Dock 暂时收起、侧栏暂时显示（设置导航在侧栏里），回到对话后各自恢复；临时状态不写入偏好。
	workspace := &d.preferences.Workspace
	if page != "" && d.page == "" {
		if workspace.Right.Open {
			d.rightDockHiddenBySettings = true
			workspace.Right.Open = false
		}
		if workspace.IsNarrow() {
			d.sidebarDrawerOpenedBySettings = true
		} else if !workspace.SidebarOpen {
			d.sidebarShownBySettings = true
		}
		workspace.ShowSidebar()
	}
	if page == "" && d.rightDockHiddenBySettings {
		d.rightDockHiddenBySettings = false
		workspace.Right.Open = true
	}
	if page == "" && d.sidebarShownBySettings {
		d.sidebarShownBySettings = false
		workspace.SidebarOpen = false
	}
	if page == "" && d.sidebarDrawerOpenedBySettings {
		d.closeSidebarDrawer()
	}
	if page == "" {
		d.catalogOpen = false
	}
	d.page = page
	d.settingsRequest++
	d.settingsLoading = false
	d.settingsError = nil
	d.loadSettingsPage(page)
}

// loadSettingsPage 读取页面需要的服务端数据。
func (d *Desktop) loadSettingsPage(page string) {
	switch page {
	case "providers":
		d.catalogRequested, d.catalogError = false, nil
		d.loadProviders()
	case "executors":
		d.loadExecutors()
	case "hooks", "skills":
		d.loadReadOnlySettings(page)
	}
}

// getSettingsPageTitle 返回设置页面在导航和页头里的名称。
func (d *Desktop) getSettingsPageTitle(page string) string {
	return d.t("settings." + page)
}

// settingsNavView 绘制设置打开时的左侧栏：标题加返回按钮，下面是各设置页，选中页高亮。
func (d *Desktop) settingsNavView(c *ui.Context) {
	// 1. 标题栏：返回按钮在最左，其后是「设置」。
	t := kit.GetTheme(c)
	icons := map[string]*ui.SVG{"general": kit.IconSettings, "providers": kit.IconServerCog, "executors": kit.IconTerminal, "hooks": kit.IconWebhook, "skills": kit.IconBookOpen}
	ui.Column(c).Fill().Gap(0).Children(func() {
		kit.HeaderWith(c, d.t("settings"), kit.HeaderOpt{Inset: t.Space(1), Leading: func() {
			if kit.IconButtonTip(c.Key("settings-back"), kit.IconChevronLeft, d.t("settings.back")).Clicked() {
				d.navigate("")
				d.closeSidebarDrawer()
			}
			t.TextRole(ui.Text(c, d.t("settings")).SingleLine().Shrink(1), kit.TextUI)
		}}, nil)
		// 2. 每个页面一行。
		for _, page := range settingsPages {
			title := d.getSettingsPageTitle(page)
			if kit.ListRow(c.Key("settings-nav:"+page), kit.ListRowOpt{Icon: icons[page], Title: title, Selected: page == d.page}).FillWidth().Label(title).Clicked() {
				d.navigate(page)
				d.closeSidebarDrawer()
			}
		}
	})
}

// closeSidebarDrawer 窄窗口里侧栏是抽屉：选定导航项后收起它。
func (d *Desktop) closeSidebarDrawer() {
	d.sidebarDrawerOpenedBySettings = false
	if d.preferences.Workspace.IsNarrow() {
		d.preferences.Workspace.ToggleSidebar()
	}
}

// settingsView 绘制当前设置页：页标题、页头操作和各页内容。
func (d *Desktop) settingsView(c *ui.Context) {
	// 1. 页头的应用与撤销只记录意图，等本页字段处理完本帧输入后再执行。
	apply, revert := false, false
	page := kit.SettingPage(c.Key(d.page), d.getSettingsPageTitle(d.page), func() { d.settingsActionsView(c, &apply, &revert) }, func() {
		if d.settingsError != nil {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				d.errorMessageView(c, d.settingsError).Shrink(1)
				if kit.Button(c, d.t("retry")).Clicked() {
					d.navigate(d.page)
				}
			})
		}
		if d.settingsLoading {
			kit.InlineHint(c, d.t("loading"), true)
		}
		switch d.page {
		case "general":
			d.generalSettingsView(c)
		case "providers":
			d.providersView(c)
		case "executors":
			d.configNoteView(c)
			d.executorsSettingsView(c)
		case "hooks":
			d.configNoteView(c)
			d.hooksSettingsView(c)
		case "skills":
			d.configNoteView(c)
			d.skillsSettingsView(c)
		}
	})
	// 2. 切换页面时渐显。
	shown := ui.Local(page, settingsPageKey{}, func() string { return d.page })
	changed := ui.Local(page, settingsChangedKey{}, func() time.Time { return time.Time{} })
	if *shown != d.page {
		*shown = d.page
		*changed = c.Now()
	}
	if !changed.IsZero() {
		elapsed := c.Now().Sub(*changed)
		if elapsed < panelFadeDuration {
			page.Opacity(max(0, float32(elapsed)/float32(panelFadeDuration)))
			c.AnimationFrame()
		}
	}
	if revert {
		d.providerDraft = cloneProviderDraft(d.providerBaseline)
	}
	if apply {
		d.saveProvider()
	}
}

// settingsActionsView 绘制页头右侧的操作：模型服务页的新增、目录、撤销、应用，以及每页都有的重载配置。
func (d *Desktop) settingsActionsView(c *ui.Context, apply, revert *bool) {
	// 1. 模型服务页：新增服务、服务目录开关；有草稿时再加撤销与应用，应用在可提交时是主按钮。
	if d.page == "providers" {
		if kit.Button(c, d.t("add.provider")).Clicked() {
			d.openProvider(defs.ProviderConfig{Protocol: "openai_chat"}, "", "")
		}
		if kit.Button(c, d.t("provider.catalog")).Clicked() {
			d.catalogOpen = !d.catalogOpen
		}
		if d.providerDraft != nil {
			if kit.Button(c, d.t("revert")).Disabled(d.providerSaving).Clicked() {
				*revert = true
			}
			canApply := d.client != nil && !d.clientStopped && !d.providerSaving && (d.isProviderDirty() || d.providerDraft.EditingID == "")
			applyButton := kit.Button
			if canApply {
				applyButton = kit.PrimaryButton
			}
			if applyButton(c.Key("apply"), d.t("apply")).Disabled(!canApply).Clicked() {
				*apply = true
			}
		}
	}
	// 2. 重载配置在每页都有。
	if kit.Button(c, d.t("reload.configuration")).Disabled(d.client == nil).Clicked() {
		d.reloadConfiguration()
	}
}

// reloadConfiguration 请求 serve 重新读取 config.yaml，成功后刷新依赖配置的列表。
func (d *Desktop) reloadConfiguration() {
	if d.client == nil {
		return
	}
	client := d.client
	d.tasks.Go(func() {
		result, err := client.ReloadConfig(d.ctx)
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			detail := ""
			if len(result.PendingConversationIDs) > 0 {
				detail = fmt.Sprintf("%s: %s", fmt.Sprintf(d.t("pending.conversations.apply.when.idle"), len(result.PendingConversationIDs)), joinConversationIDs(result.PendingConversationIDs))
			}
			// 配置变了：技能、Agent、Executor 和当前设置页的列表都要重新读取。
			clear(d.skillCatalogs)
			d.loadAgents()
			d.loadExecutors()
			d.loadModels(d.readingTarget)
			d.loadSettingsPage(d.page)
			d.showSuccess(d.t("configuration.reloaded"), detail)
		})
	})
}

// configNoteView 在只读设置页顶部说明数据来自 config.yaml。
func (d *Desktop) configNoteView(c *ui.Context) {
	kit.FieldMessage(c, kit.MessageDescription, d.t("settings.config.note"))
}

// settingSelectView 绘制设置行右侧的带边线下拉，返回本帧是否改动。
func (d *Desktop) settingSelectView(c *ui.Context, key, label string, value *string, opt kit.SelectOpt) bool {
	opt.Label, opt.Placeholder, opt.Bordered = label, label, true
	return kit.Select(c.Key(key), value, opt).Width(settingControlWidth).Changed()
}

// generalSettingsView 即时应用本机外观并非阻塞保存。
func (d *Desktop) generalSettingsView(c *ui.Context) {
	// 1. 主题：外观分段、配色下拉；默认配色标「默认」。
	before := d.preferences
	themes := []string{"system", "light", "dark"}
	kit.SettingSection(c, d.t("settings.theme"), func() {
		kit.SettingRow(c, d.t("appearance"), d.t("appearance.hint"), func() {
			selected := slices.Index(themes, d.preferences.Theme)
			previous := selected
			kit.Segmented(c.Key("appearance"), &selected, []string{d.t("system"), d.t("light"), d.t("dark")}).Label(d.t("appearance"))
			if selected != previous {
				d.preferences.Theme = themes[selected]
			}
		})
		kit.SettingRow(c, d.t("palette"), d.t("palette.hint"), func() {
			palettes := []string{"zed", "stone", "blue"}
			labels := []string{fmt.Sprintf(d.t("default.marker"), d.t("palette.zed")), d.t("palette.stone"), d.t("palette.blue")}
			d.settingSelectView(c, "palette", d.t("palette"), &d.preferences.Palette, kit.SelectOpt{Options: selectOptions(palettes, labels)})
		})
	})
	// 2. 界面：字体和恢复默认。
	fontNames := []string{"", "Noto Serif"}
	fontLabels := []string{kit.FontSans, "Noto Serif"}
	kit.SettingSection(c, d.t("settings.interface"), func() {
		kit.SettingRow(c, d.t("body.font"), d.t("body.font.hint"), func() {
			d.settingSelectView(c, "body-font", d.t("body.font"), &d.preferences.BodyFont, kit.SelectOpt{Options: selectOptions(fontNames, fontLabels)})
		})
		kit.SettingRow(c, d.t("title.font"), d.t("title.font.hint"), func() {
			d.settingSelectView(c, "title-font", d.t("title.font"), &d.preferences.TitleFont, kit.SelectOpt{Options: selectOptions(fontNames, fontLabels)})
		})
		kit.SettingRow(c, d.t("restore.appearance.defaults"), "", func() {
			if kit.Button(c, d.t("restore")).Label(d.t("restore.appearance.defaults")).Clicked() {
				defaults := defaultPreferences()
				d.preferences.Theme = defaults.Theme
				d.preferences.Palette = defaults.Palette
				d.preferences.BodyFont = ""
				d.preferences.TitleFont = ""
				d.preferencesBroken = false
			}
		})
	})
	// 3. 语言与通知。
	kit.SettingSection(c, d.t("language"), func() {
		kit.SettingRow(c, d.t("language"), d.t("language.hint"), func() {
			d.settingSelectView(c, "language", d.t("language"), &d.preferences.Language, kit.SelectOpt{Options: selectOptions([]string{"zh-CN", "en"}, []string{"简体中文", "English"})})
		})
	})
	kit.SettingSection(c, d.t("settings.notifications"), func() {
		kit.SettingRow(c, d.t("notify.on.finish"), d.t("notify.on.finish.hint"), func() {
			kit.Switch(c.Key("notify-on-finish"), &d.preferences.NotifyOnFinish, "").Label(d.t("notify.on.finish"))
		})
	})
	if before != d.preferences {
		d.installMenu()
		if err := d.savePreferences(); err != nil {
			d.showToast(kit.ToastOpt{Title: d.t("preferences.were.not.saved"), Description: err.Error(), Kind: kit.ToastError})
		}
	}
}

// settingsItemView 绘制一条可展开的设置项：一行 ListRow（名称、来源徽标、说明），点开显示 details。
func (d *Desktop) settingsItemView(c *ui.Context, key, title, description, source string, raw any, details func()) {
	// 1. 展开状态按设置项记在桌面上。
	t := kit.GetTheme(c)
	expandedKey := "settings:" + key
	expanded := d.itemExpanded[expandedKey]
	chevron := kit.IconChevronRight
	if expanded {
		chevron = kit.IconChevronDown
	}
	ui.Column(c.Key(key)).Gap(0).Children(func() {
		row := kit.ListRow(c, kit.ListRowOpt{Icon: chevron, Title: title, Description: getSingleLine(description), Trailing: func() {
			if source != "" {
				kit.Badge(c, source, kit.BadgeNeutral)
			}
		}}).FillWidth().Label(title)
		if row.Clicked() {
			d.itemExpanded[expandedKey] = !expanded
		}
		// 2. 详情缩进到标题的文字起点：行首 12 + 图标位 16 + 间距 8。
		if expanded {
			ui.Column(c).Gap(t.Space(2)).Padding(t.Space(1), t.Space(3), t.Space(3), t.Space(9)).Background(t.Colors.Editor).Children(func() {
				details()
				open := d.itemExpanded["settings-raw:"+key]
				kit.Disclosure(c, d.t("raw.data"), &open, func() { d.jsonView(c, "raw:"+key, title, raw) })
				d.itemExpanded["settings-raw:"+key] = open
			})
		}
	})
}

// executorsSettingsView 列出 Executor：名称、来源徽标、说明，点开看默认目录。
func (d *Desktop) executorsSettingsView(c *ui.Context) {
	// 1. local 与 openvaala 是内置的，其余来自 config.yaml。
	if len(d.executors) == 0 {
		if d.executorsLoading {
			kit.InlineHint(c, d.t("loading"), true)
			return
		}
		kit.FieldMessage(c, kit.MessageDescription, d.t("settings.executors.empty"))
		return
	}
	kit.SettingSection(c, d.t("settings.executors"), func() {
		for _, executor := range d.executors {
			source := "config.yaml"
			if executor.ID == defs.LocalExecutorID || executor.ID == config.BuiltinExecutorID || executor.ID == defs.MemoryExecutorID {
				source = d.t("source.builtin")
			}
			d.settingsItemView(c, "executor:"+executor.ID, executor.ID, executor.Description, source, executor, func() {
				if executor.WorkspaceDirectory != "" {
					kit.Facts(c, kit.Fact{Label: d.t("workspace.directory"), Value: executor.WorkspaceDirectory})
				}
			})
		}
	})
}

// hooksSettingsView 列出 Hook：事件、来源徽标（插件名或 config.yaml）、命令，点开看匹配条件、超时和来源文件。
func (d *Desktop) hooksSettingsView(c *ui.Context) {
	if len(d.hooks) == 0 && !d.settingsLoading {
		kit.FieldMessage(c, kit.MessageDescription, d.t("settings.hooks.empty"))
		return
	}
	kit.SettingSection(c, d.t("settings.hooks"), func() {
		for index, info := range d.hooks {
			source := cmp.Or(info.PluginName, "config.yaml")
			d.settingsItemView(c, fmt.Sprintf("hook:%d:%s", index, info.Key), string(info.Event), info.Handler, source, info, func() {
				facts := []kit.Fact{{Label: d.t("hook.event"), Value: string(info.Event)}}
				if info.Matcher != "" {
					facts = append(facts, kit.Fact{Label: d.t("hook.matcher"), Value: info.Matcher})
				}
				facts = append(facts, kit.Fact{Label: d.t("hook.handler"), Value: info.Handler}, kit.Fact{Label: d.t("hook.timeout"), Value: fmt.Sprintf(d.t("hook.timeout.value"), info.TimeoutSeconds)})
				if info.PluginName != "" {
					facts = append(facts, kit.Fact{Label: d.t("hook.plugin"), Value: info.PluginName})
				}
				facts = append(facts, kit.Fact{Label: d.t("hook.source"), Value: info.SourcePath})
				kit.Facts(c, facts...)
			})
		}
	})
}

// getSkillsAgent 返回 Skill 页筛选的 Agent：用户选过的，否则 default，没有 default 时取第一个 Agent。
func (d *Desktop) getSkillsAgent() string {
	if d.skillsAgent != "" {
		return d.skillsAgent
	}
	if len(d.agents) > 0 && !slices.ContainsFunc(d.agents, func(agent runtime.AgentInfo) bool { return agent.Name == "default" }) {
		return d.agents[0].Name
	}
	return "default"
}

// skillsSettingsView 列出所选 Agent 可用的 Skill：名称、Executor 徽标、描述，点开看路径。
func (d *Desktop) skillsSettingsView(c *ui.Context) {
	// 1. 有 Agent 列表时可以按 Agent 筛选。
	if d.agentsError == nil && len(d.agents) > 0 {
		names := make([]string, len(d.agents))
		for index, agent := range d.agents {
			names[index] = agent.Name
		}
		kit.SettingSection(c, d.t("agent"), func() {
			kit.SettingRow(c, d.t("agent"), d.t("skills.agent.hint"), func() {
				agent := d.getSkillsAgent()
				if d.settingSelectView(c, "skills-agent", d.t("agent"), &agent, kit.SelectOpt{Options: selectOptions(names, names), Searchable: true, SearchPlaceholder: d.t("filter.agents")}) {
					d.skillsAgent = agent
					d.loadReadOnlySettings("skills")
				}
			})
		})
	}
	// 2. 技能列表。
	if len(d.configuredSkills) == 0 && !d.settingsLoading {
		kit.FieldMessage(c, kit.MessageDescription, d.t("no.skills.available"))
		return
	}
	kit.SettingSection(c, d.t("settings.skills"), func() {
		for _, info := range d.configuredSkills {
			d.settingsItemView(c, "skill:"+info.Executor+"/"+info.Name, info.Name, info.Description, info.Executor, info, func() {
				kit.Facts(c, kit.Fact{Label: d.t("executor"), Value: info.Executor}, kit.Fact{Label: d.t("skill.path"), Value: info.Path})
			})
		}
	})
}

// loadReadOnlySettings 按需查询 Hook 或有效 Skill，不增加配置写入口。
func (d *Desktop) loadReadOnlySettings(page string) {
	// 1. 绑定当前设置查询，迟到结果不串页。
	if d.client == nil {
		return
	}
	d.settingsRequest++
	request := d.settingsRequest
	d.settingsLoading = true
	client := d.client
	if page == "hooks" {
		d.tasks.Go(func() {
			hooks, err := client.GetHooks(d.ctx)
			d.window.Update(func() {
				if d.closed || d.settingsRequest != request {
					return
				}
				d.settingsLoading = false
				d.settingsError = err
				if err == nil {
					d.hooks = hooks
				}
			})
		})
		return
	}
	d.tasks.Go(func() {
		skills, err := client.GetSkills(d.ctx, &runtimeclient.GetSkillsRequest{AgentName: d.getSkillsAgent()})
		d.window.Update(func() {
			if d.closed || d.settingsRequest != request {
				return
			}
			d.settingsLoading = false
			d.settingsError = err
			if err == nil {
				d.configuredSkills = skills
			}
		})
	})
}

// confirmationView 在独立弹窗中处理删除、撤回、改名与离开确认；Enter 确认、Esc 取消，改名时输入框按 Enter 提交。
func (d *Desktop) confirmationView(c *ui.Context) {
	// 1. 弹窗不依赖已隐藏的列表行；标题和确认按钮随确认类型变化。
	opt := kit.DialogOpt{Title: d.t("discard.unsaved.changes"), ConfirmLabel: d.t("discard"), ConfirmDisabled: d.confirmPending}
	switch d.confirm.Kind {
	case "delete-conversation":
		opt.Title, opt.ConfirmLabel, opt.Danger = fmt.Sprintf(d.t("delete.conversation.confirm"), d.confirm.Name), d.t("delete"), true
	case "delete-provider":
		opt.Title, opt.ConfirmLabel, opt.Danger = fmt.Sprintf(d.t("delete.provider.confirm"), d.confirm.ProviderID), d.t("delete"), true
	case "withdraw":
		opt.Title, opt.ConfirmLabel, opt.Danger = fmt.Sprintf(d.t("withdraw.input.confirm"), d.confirm.Name), d.t("withdraw"), true
	case "rename":
		opt.Title, opt.ConfirmLabel = d.t("rename.conversation"), d.t("rename")
	}
	// 2. 改名弹窗有标题输入；失败的错误显示在正文里。
	confirmed := kit.DialogWith(c, &d.confirmOpen, opt, func() {
		if d.confirm.Kind == "rename" {
			kit.Field(c, kit.FieldOpt{Label: d.t("title")}, func() ui.Element {
				return kit.TextInputWith(c.Key("rename"), &d.renameText, kit.InputOpt{}).AutoFocus()
			})
		}
		if d.confirmError != nil {
			d.errorMessageView(c, d.confirmError)
		}
	})
	if confirmed {
		d.submitConfirmation()
	}
	if !d.confirmOpen {
		d.confirmError = nil
	}
}

// submitConfirmation 执行弹窗原目标，失败保持弹窗与错误可见。
func (d *Desktop) submitConfirmation() {
	// 1. 离开只放弃设置草稿，不丢其他对话输入。
	request := d.confirm
	if request.Kind == "select-provider" {
		d.providerDraft = nil
		d.providerBaseline = nil
		d.confirmOpen = false
		d.openProvider(request.Provider, request.ProviderID, request.CatalogID)
		return
	}
	if request.Kind == "navigate" || request.Kind == "navigate-conversation" {
		d.providerDraft = nil
		d.providerBaseline = nil
		d.confirmOpen = false
		switch {
		case request.Kind == "navigate":
			d.navigate(request.Page)
		case request.Conversation.ID == "":
			d.openNewConversation()
		default:
			d.navigate("")
			d.openConversation(request.Conversation.ID, request.Conversation.RootSessionID)
		}
		return
	}
	// 2. 写请求捕获弹窗目标，返回错误时不关闭。
	d.confirmOpen = true
	if d.client == nil || d.clientStopped {
		d.confirmError = runtimeclient.ErrConnectionLost
		return
	}
	d.confirmPending = true
	d.confirmError = nil
	client := d.client
	title := d.renameText
	d.tasks.Go(func() {
		var err error
		switch request.Kind {
		case "delete-conversation":
			err = client.DeleteConversation(d.ctx, &runtimeclient.DeleteConversationRequest{ConversationID: request.Conversation.ID})
		case "delete-provider":
			_, err = client.DeleteProvider(d.ctx, request.ProviderID)
		case "withdraw":
			_, err = client.WithdrawInput(d.ctx, request.Conversation.ID, request.ItemID, false)
		case "rename":
			err = client.SetConversationTitle(d.ctx, request.Conversation.ID, title)
		}
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.confirmPending = false
			d.confirmError = err
			if err != nil {
				return
			}
			d.confirmOpen = false
			d.loadConversations(true)
			d.showConfirmationSuccess(request)
			if request.Kind == "delete-provider" {
				d.providerDraft = nil
				d.providerBaseline = nil
				d.loadProviders()
				d.loadModels(d.readingTarget)
			}
			if request.Kind == "delete-conversation" && d.readingTarget.ConversationID == request.Conversation.ID {
				d.openConversation(defs.ConversationID(""), "")
			}
		})
	})
}

// showConfirmationSuccess 提示弹窗确认的写操作已完成。
func (d *Desktop) showConfirmationSuccess(request confirmation) {
	switch request.Kind {
	case "delete-conversation":
		d.showSuccess(d.t("conversation.deleted"), request.Name)
	case "rename":
		d.showSuccess(d.t("conversation.renamed"), d.renameText)
	case "withdraw":
		d.showSuccess(d.t("input.withdrawn"), request.Name)
	case "delete-provider":
		d.showSuccess(d.t("provider.deleted"), request.ProviderID)
	}
}
