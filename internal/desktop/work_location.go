package desktop

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"unicode/utf8"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/egoist/mygo/ui"
)

// loadExecutors 查询只读执行环境列表；失败用通知说明，新对话的 Executor 下拉退回只有 local。
func (d *Desktop) loadExecutors() {
	if d.client == nil {
		return
	}
	client := d.client
	d.executorsLoading = true
	d.tasks.Go(func() {
		executors, err := client.ListExecutors(d.ctx)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.executorsLoading = false
			if err != nil {
				d.showFailure(err)
				return
			}
			d.executors = executors
		})
	})
}

// loadAgents 查询配置里的 Agent 列表；失败时新对话的 Agent 改为手填。
func (d *Desktop) loadAgents() {
	if d.client == nil {
		return
	}
	client := d.client
	d.tasks.Go(func() {
		agents, err := client.ListAgents(d.ctx)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.agents, d.agentsError = agents, err
		})
	})
}

// getSelectableExecutors 返回可作为工作位置的 Executor：不含内置的 openvaala 虚拟环境；列表还没读到时只有 local。
func (d *Desktop) getSelectableExecutors() []conversation.ExecutorDetails {
	// 1. openvaala 只存放内置 Skill，不是工作位置。
	executors := slices.DeleteFunc(slices.Clone(d.executors), func(executor conversation.ExecutorDetails) bool { return executor.ID == config.BuiltinExecutorID })
	if len(executors) == 0 {
		return []conversation.ExecutorDetails{{ID: "local"}}
	}
	return executors
}

// newConversationFields 绘制新对话的工作位置表单：Agent、Executor 和目录，每个控件都有标签。
func (d *Desktop) newConversationFields(c *ui.Context) {
	t := kit.GetTheme(c)
	executors := d.getSelectableExecutors()
	ui.Column(c).Width(320).Gap(t.Space(4)).Children(func() {
		// 1. Agent：有 agent/list 时是可搜索的下拉，否则是带标签的文本框。
		d.newAgentField(c)
		// 2. Executor：说明显示在字段下方；换 Executor 后目录属于另一台机器，清空重填。
		options := make([]kit.SelectOption, len(executors))
		description := ""
		for index, executor := range executors {
			options[index] = kit.SelectOption{Value: executor.ID, Label: executor.ID, Description: getSingleLine(executor.Description)}
			if executor.ID == d.newExecutor {
				description = executor.Description
			}
		}
		kit.Field(c, kit.FieldOpt{Label: d.t("executor"), Description: description}, func() ui.Element {
			executor := kit.Select(c.Key("new-executor"), &d.newExecutor, kit.SelectOpt{Options: options, Label: d.t("executor"), Placeholder: d.t("executor"), Bordered: true})
			if executor.Changed() {
				d.newDirectory, d.directoryError = "", nil
				d.directoryListing, d.directoryEditing = directoryListing{}, directoryEditing{}
			}
			return executor
		})
		// 3. 目录：输入时给子目录建议，失焦时解析成绝对路径。
		d.directoryField(c, "new-directory", false, d.newExecutor, &d.newDirectory)
	})
}

// newAgentField 绘制新对话的 Agent 字段：Agent 下拉的说明是所选 Agent 的描述和默认模型。
func (d *Desktop) newAgentField(c *ui.Context) {
	// 1. agent/list 不可用（旧服务）或为空时退回文本框，仍带标签。
	if d.agentsError != nil || len(d.agents) == 0 {
		kit.Field(c, kit.FieldOpt{Label: d.t("agent")}, func() ui.Element {
			input := kit.TextInputWith(c.Key("new-agent"), &d.newAgent, kit.InputOpt{Placeholder: "default"})
			if input.Changed() {
				d.loadModels(d.readingTarget)
			}
			return input
		})
		return
	}
	// 2. 下拉选项是 Agent 名，说明取所选 Agent。
	names := make([]string, len(d.agents))
	description := ""
	for index, agent := range d.agents {
		names[index] = agent.Name
		if agent.Name == d.newAgent {
			description = agent.Description
			if agent.DefaultModel != (defs.ModelRef{}) {
				description += " · " + agent.DefaultModel.String()
			}
		}
	}
	kit.Field(c, kit.FieldOpt{Label: d.t("agent"), Description: description}, func() ui.Element {
		agent := kit.Select(c.Key("new-agent"), &d.newAgent, kit.SelectOpt{Options: selectOptions(names, names), Label: d.t("agent"), Placeholder: d.t("agent"), Searchable: true, SearchPlaceholder: d.t("filter.agents"), Bordered: true})
		if agent.Changed() {
			d.loadModels(d.readingTarget)
		}
		return agent
	})
}

// directoryField 绘制带标签的目录输入框：输入时给子目录建议，失焦时解析成绝对路径；
// isDialog 为真时编辑的是对话工作位置弹窗里 executorID 的目录，否则是新对话的目录。
func (d *Desktop) directoryField(c *ui.Context, key string, isDialog bool, executorID string, directory *string) {
	// 1. 解析失败显示在字段下方，输入框用危险色边线。
	field := kit.FieldOpt{Label: d.t("directory")}
	if d.directoryError != nil {
		field.Error = d.t("directory.invalid") + "：" + getDirectoryErrorDetail(d.directoryError)
	}
	kit.Field(c, field, func() ui.Element {
		input := kit.TextInputWith(c.Key(key), directory, kit.InputOpt{Placeholder: getDefaultDirectory(d.executors, executorID), Invalid: d.directoryError != nil, Leading: kit.IconFolder})
		editing := &d.directoryEditing
		if editing.IsCaretAtEndRequested {
			end := utf8.RuneCountInString(*directory)
			input.SetTextSelection(end, end)
			editing.IsCaretAtEndRequested = false
		}
		// 2. 内容改过之后开始给建议；失焦时清掉建议，并把目录解析成绝对路径。
		switch focused := input.Focused(); {
		case focused:
			if !editing.IsFocused || editing.ExecutorID != executorID {
				editing.ExecutorID, editing.IsSuggesting = executorID, false
			}
			if input.Changed() {
				editing.IsSuggesting, d.directoryError = true, nil
			}
		case editing.IsFocused:
			resolving := editing.ExecutorID == executorID && *directory != ""
			*editing = directoryEditing{}
			d.directoryListing = directoryListing{}
			if resolving {
				d.resolveDirectory(isDialog, executorID, *directory)
			}
		}
		editing.IsFocused = input.Focused()
		// 3. 建议是上级目录的子目录里名称以输入末段开头的那些，选中后进入下一层。
		open := editing.IsFocused && editing.IsSuggesting
		parent, prefix := splitDirectoryInput(*directory)
		if open {
			d.loadDirectoryListing(executorID, parent)
		}
		listing := d.directoryListing
		names := []string{}
		opt := kit.CompletionOpt{Open: open, Loading: listing.Loading, Empty: d.t("no.subdirectories")}
		for _, name := range listing.Names {
			if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
				names = append(names, name)
				opt.Items = append(opt.Items, kit.CompletionItem{Icon: kit.IconFolder, Title: name + "/"})
			}
		}
		if listing.Err != nil {
			opt.Error = d.getErrorMessage(listing.Err)
		}
		if selected := kit.CompletionMenu(c, input, input, opt); selected >= 0 && selected < len(names) {
			*directory = path.Join(listing.Directory, names[selected]) + "/"
			editing.IsSuggesting, editing.IsCaretAtEndRequested = false, true
			c.AnimationFrame()
		}
		return input
	})
}

// loadDirectoryListing 读取 executorID 上 parent 目录的子目录；同一个 Executor 和上级目录只请求一次。
func (d *Desktop) loadDirectoryListing(executorID, parent string) {
	// 1. 已经请求过（含请求中）的目录直接沿用。
	listing := d.directoryListing
	if d.client == nil || listing.ExecutorID == executorID && listing.Parent == parent && (listing.Loading || listing.Names != nil || listing.Err != nil) {
		return
	}
	d.directoryRequest++
	requestID := d.directoryRequest
	d.directoryListing = directoryListing{ExecutorID: executorID, Parent: parent, Loading: true}
	client := d.client
	d.tasks.Go(func() {
		result, err := client.GetWorkspaceDirectories(d.ctx, &runtimeclient.GetWorkspaceDirectoriesRequest{ExecutorID: executorID, Directory: parent})
		d.window.Update(func() {
			// 2. 只接受仍是当前请求的结果，目录已变化或已失焦时丢弃。
			if d.closed || requestID != d.directoryRequest {
				return
			}
			next := directoryListing{ExecutorID: executorID, Parent: parent, Err: err}
			if err == nil {
				next.Directory, next.Names = result.Directory, result.Directories
				if next.Names == nil {
					next.Names = []string{}
				}
			}
			d.directoryListing = next
		})
	})
}

// resolveDirectory 在 Executor 上把目录解析成绝对路径；仅当用户没有继续编辑时替换输入框内容，失败显示在字段下方。
func (d *Desktop) resolveDirectory(isDialog bool, executorID, directory string) {
	if d.client == nil {
		return
	}
	client := d.client
	d.tasks.Go(func() {
		resolved, err := client.ResolveWorkspaceDirectory(d.ctx, &runtimeclient.ResolveWorkspaceDirectoryRequest{ExecutorID: executorID, Directory: directory})
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.directoryError = err
			if err != nil {
				return
			}
			if isDialog {
				if d.locationDirectories[executorID] == directory {
					d.locationDirectories[executorID] = resolved
				}
			} else if d.newExecutor == executorID && d.newDirectory == directory {
				d.newDirectory = resolved
			}
		})
	})
}

// openWorkLocation 复制对话设置到弹窗草稿，并读取这个对话的 MCP 连接。
func (d *Desktop) openWorkLocation() {
	d.locationOpen = true
	d.locationTab = 0
	d.locationError, d.directoryError = nil, nil
	d.directoryListing, d.directoryEditing = directoryListing{}, directoryEditing{}
	d.locationDefault = d.conversation.ExecutorSettings.DefaultExecutorID
	if d.locationDefault == "" {
		d.locationDefault = "local"
	}
	d.locationDirectories = maps.Clone(d.conversation.ExecutorSettings.WorkspaceDirectories)
	if d.locationDirectories == nil {
		d.locationDirectories = map[string]string{}
	}
	d.locationAvailable = map[string]bool{}
	for _, executor := range d.executors {
		d.locationAvailable[executor.ID] = d.conversation.ExecutorSettings.IsExecutorAvailable(executor.ID)
	}
	d.mcpServers, d.mcpError = nil, nil
	d.loadMCP()
}

// workLocationDialog 绘制对话的工作位置弹窗：工作位置、MCP 和 Skill 三页；保存时写入 Executor 设置。
func (d *Desktop) workLocationDialog(c *ui.Context) {
	// 1. 分段切换在正文顶部、靠正文左线，内容区过高时在弹窗内滚动。
	t := kit.GetTheme(c)
	sections := []kit.SegmentedItem{{Icon: kit.IconFolder, Label: d.t("workspace")}, {Icon: kit.IconServerCog, Label: "MCP"}, {Icon: kit.IconBookOpen, Label: "Skill"}}
	opt := kit.DialogOpt{Title: d.t("workspace"), ConfirmLabel: d.t("save"), ConfirmDisabled: d.locationSaving}
	if kit.DialogWith(c, &d.locationOpen, opt, func() {
		kit.SegmentedWith(c, &d.locationTab, sections, kit.SegmentedOpt{}).Label(d.t("workspace")).AlignSelf(ui.Start)
		ui.Scroll(c).Height(320).Children(func() {
			ui.Column(c).Gap(t.Space(4)).Children(func() {
				switch d.locationTab {
				case 0:
					d.workLocationTab(c)
				case 1:
					d.mcpTab(c)
				default:
					d.conversationSkillsTab(c)
				}
			})
		})
		if d.locationError != nil {
			d.errorMessageView(c, d.locationError)
		}
	}) {
		d.saveWorkLocation()
	}
}

// workLocationTab 绘制工作位置页：只读的 Agent、默认 Executor、默认 Executor 的目录，以及各 Executor 是否可用。
func (d *Desktop) workLocationTab(c *ui.Context) {
	// 1. Agent 创建后不能改，只读显示。
	kit.Field(c, kit.FieldOpt{Label: d.t("agent")}, func() ui.Element {
		agent := d.getSessionInfo().Session.AgentName
		return kit.TextInputWith(c.Key("location-agent"), &agent, kit.InputOpt{ReadOnly: true})
	})
	// 2. 默认 Executor 只能从可用的 Executor 里选。
	executors := d.getSelectableExecutors()
	ids, description, selected := []string{}, "", 0
	for _, executor := range executors {
		if !d.locationAvailable[executor.ID] && executor.ID != d.locationDefault {
			continue
		}
		if executor.ID == d.locationDefault {
			selected, description = len(ids), executor.Description
		}
		ids = append(ids, executor.ID)
	}
	kit.Field(c, kit.FieldOpt{Label: d.t("default.executor"), Description: description}, func() ui.Element {
		before := selected
		segmented := kit.Segmented(c, &selected, ids).AlignSelf(ui.Start)
		if selected != before {
			d.locationDefault = ids[selected]
			d.directoryError = nil
			d.directoryListing, d.directoryEditing = directoryListing{}, directoryEditing{}
		}
		return segmented
	})
	// 3. 目录属于默认 Executor，各 Executor 的目录原文在弹窗里各自保留。
	directory := d.locationDirectories[d.locationDefault]
	d.directoryField(c, "location-directory", true, d.locationDefault, &directory)
	d.locationDirectories[d.locationDefault] = directory
	// 4. 每个 Executor 一行开关；默认 Executor 始终可用。
	kit.SettingSection(c, d.t("available.executors"), func() {
		for _, executor := range executors {
			kit.SettingRow(c.Key(executor.ID), executor.ID, executor.Description, func() {
				available := d.locationAvailable[executor.ID] || executor.ID == d.locationDefault
				if kit.Switch(c, &available, "").Label(executor.ID + " · " + d.t("available")).Disabled(executor.ID == d.locationDefault).Changed() {
					d.locationAvailable[executor.ID] = available
				}
			})
		}
	})
}

// mcpTab 绘制 MCP 页：每个服务一行，状态点加状态文字，失败时显示原因；标题行有刷新。
func (d *Desktop) mcpTab(c *ui.Context) {
	t := kit.GetTheme(c)
	ui.Row(c).AlignItems(ui.Center).Children(func() {
		t.TextRole(ui.Text(c, "MCP"), kit.TextTitle).Grow(1)
		if kit.IconButtonTip(c, kit.IconRefresh, d.t("refresh.mcp")).Disabled(d.mcpLoading).Clicked() {
			d.loadMCP()
		}
	})
	if d.mcpError != nil {
		d.errorMessageView(c, d.mcpError)
	}
	switch {
	case d.mcpLoading && len(d.mcpServers) == 0:
		kit.InlineHint(c, "", true)
	case len(d.mcpServers) == 0 && d.mcpError == nil:
		kit.InlineHint(c, d.t("no.mcp.servers"), false)
	}
	for _, server := range d.mcpServers {
		stateLabel, color := d.t("mcp.connecting"), t.Colors.Warning
		switch server.State {
		case mcpclient.ConnectionStateConnected:
			stateLabel, color = d.t("mcp.connected"), t.Colors.Success
		case mcpclient.ConnectionStateFailed:
			stateLabel, color = d.t("mcp.failed"), t.Colors.Danger
		}
		detail := server.ExecutorID + " · " + fmt.Sprintf(d.t("mcp.tools.count"), len(server.Tools))
		if server.Error != "" {
			detail = server.ExecutorID + " · " + server.Error
		}
		row := kit.ListRow(c.Key(server.ExecutorID+"/"+server.ServerID), kit.ListRowOpt{Icon: kit.IconServerCog, Title: server.ServerID, Description: detail, Trailing: func() {
			kit.StatusDot(c, color).Label(stateLabel)
			t.TextRole(ui.Text(c, stateLabel).SingleLine(), kit.TextCaption).TextColor(t.Colors.Muted)
		}})
		if server.Error != "" {
			kit.Tooltip(c, row, server.Error)
		}
	}
}

// conversationSkillsTab 绘制 Skill 页：这个对话可用的技能，每行名称加描述。
func (d *Desktop) conversationSkillsTab(c *ui.Context) {
	key := getSkillCatalogKey(d.conversation.ID, "", "", "")
	catalog := d.loadSkillCatalog(key)
	switch {
	case catalog.Err != nil:
		d.errorMessageView(c, catalog.Err)
		if kit.Button(c, d.t("retry")).AlignSelf(ui.Start).Clicked() {
			delete(d.skillCatalogs, key)
		}
	case catalog.Loading:
		kit.InlineHint(c, "", true)
	case len(catalog.Skills) == 0:
		kit.InlineHint(c, d.t("no.skills.available"), false)
	}
	for _, info := range catalog.Skills {
		kit.ListRow(c.Key(info.Name), kit.ListRowOpt{Icon: kit.IconBookOpen, Title: info.Name, Description: getSingleLine(info.Description)})
	}
}

// saveWorkLocation 先把默认 Executor 的目录解析成绝对路径，再保存默认 Executor、它的目录和各 Executor 的可用设置。
func (d *Desktop) saveWorkLocation() {
	// 1. 非默认 Executor 的目录仍在弹窗内保留，不写入本次请求；openvaala 不在弹窗里，沿用原有可用状态。
	settings := d.conversation.ExecutorSettings
	settings.WorkspaceDirectories = maps.Clone(d.conversation.ExecutorSettings.WorkspaceDirectories)
	if settings.WorkspaceDirectories == nil {
		settings.WorkspaceDirectories = map[string]string{}
	}
	settings.DefaultExecutorID = d.locationDefault
	settings.ExecutorIDs = nil
	for _, executor := range d.executors {
		if d.locationAvailable[executor.ID] || executor.ID == d.locationDefault {
			settings.ExecutorIDs = append(settings.ExecutorIDs, executor.ID)
		}
	}
	directory := d.locationDirectories[d.locationDefault]
	target, client := d.readingTarget, d.client
	d.locationSaving = true
	d.tasks.Go(func() {
		// 2. 目录无效时不保存；其余错误来自 serve 的校验。
		var err error
		if directory != "" {
			directory, err = client.ResolveWorkspaceDirectory(d.ctx, &runtimeclient.ResolveWorkspaceDirectoryRequest{ExecutorID: settings.DefaultExecutorID, Directory: directory})
		}
		var saved defs.Conversation
		if err == nil {
			settings.WorkspaceDirectories[settings.DefaultExecutorID] = directory
			saved, err = client.SetConversationExecutorSettings(d.ctx, target.ConversationID, settings)
		}
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.locationSaving = false
			if d.readingTarget.Request != target.Request {
				if err != nil {
					d.showFailure(err)
				}
				return
			}
			// 3. 失败时重新打开弹窗，保留草稿和错误；成功后更新标题栏、侧栏和这个对话的技能目录。
			if err != nil {
				d.locationError, d.locationOpen = err, true
				return
			}
			d.locationError = nil
			d.conversation = saved
			maps.DeleteFunc(d.skillCatalogs, func(key skillCatalogKey, _ *skillCatalog) bool { return key.ConversationID == target.ConversationID })
			d.loadConversations(true)
			d.showSuccess(d.t("workspace.saved"), "")
		})
	})
}

// loadMCP 读取本对话的 MCP 连接事实；弹窗里的刷新也走这里。
func (d *Desktop) loadMCP() {
	target, client := d.readingTarget, d.client
	if client == nil || target.ConversationID == "" {
		return
	}
	d.mcpLoading = true
	d.tasks.Go(func() {
		servers, err := client.ListMCPServers(d.ctx, target.ConversationID)
		d.window.Update(func() {
			if d.closed || d.readingTarget.Request != target.Request {
				return
			}
			d.mcpLoading = false
			d.mcpError = err
			if err == nil {
				d.mcpServers = servers
			}
		})
	})
}
