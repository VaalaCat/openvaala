package desktop

import (
	"embed"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

//go:embed agent_icons/*.svg
var agentIconFiles embed.FS

var agentIcons = func() map[string]*ui.SVG {
	icons := map[string]*ui.SVG{}
	files, _ := agentIconFiles.ReadDir("agent_icons")
	for _, file := range files {
		data, _ := agentIconFiles.ReadFile("agent_icons/" + file.Name())
		icons[strings.TrimSuffix(file.Name(), ".svg")] = ui.MustParseSVG(data)
	}
	return icons
}()

// loadAgentSettings 读取 Agent 列表和原生模型选项。
func (d *Desktop) loadAgentSettings() {
	if d.client == nil || d.settingsLoading {
		return
	}
	client, request := d.client, d.settingsRequest
	d.settingsLoading = true
	d.tasks.Go(func() {
		agents, err := client.ListAgents(d.ctx)
		rows := make([]agentSettingsRow, 0, len(agents))
		for _, info := range agents {
			row := agentSettingsRow{info: info}
			if err == nil && info.Type.IsExternal() {
				var definition defs.AgentConfig
				definition, err = client.GetAgent(d.ctx, info.Name)
				if err == nil && definition.ACP != nil {
					row.command = definition.ACP.Command
				}
			}
			rows = append(rows, row)
		}
		var models []*defs.ModelDefinition
		var presets []agent.AgentPreset
		if err == nil {
			presets, err = client.ListAgentPresets(d.ctx)
		}
		if err == nil {
			var list runtime.ModelList
			list, err = client.ListModels(d.ctx, &runtime.ListModelsRequest{})
			models = slices.DeleteFunc(list.Models, func(model *defs.ModelDefinition) bool {
				return slices.ContainsFunc(agents, func(info runtime.AgentInfo) bool {
					return info.Type.IsExternal() && info.Name == model.Ref.ProviderID
				})
			})
		}
		d.window.Update(func() {
			if d.closed || d.settingsRequest != request {
				return
			}
			d.settingsLoading, d.settingsError = false, err
			if err == nil {
				d.agentRows, d.agentModels = rows, models
				d.agentPresets = presets
			}
		})
	})
}

// agentsSettingsView 将新增和编辑表单放在 Agent 列表上方。
func (d *Desktop) agentsSettingsView(c *ui.Context) {
	if d.agentDraft != nil {
		d.agentFormView(c)
	}
	if len(d.agentRows) == 0 {
		return
	}
	kit.SettingSection(c, d.t("settings.agents"), func() {
		for _, row := range d.agentRows {
			description := row.command
			if !row.info.Type.IsExternal() && row.info.DefaultModel != (defs.ModelRef{}) {
				description = row.info.DefaultModel.String()
			}
			icon := agentBrandIcon(row.info.Icon)
			if kit.ListRow(c.Key("agent:"+row.info.Name), kit.ListRowOpt{
				Title: row.info.Name, Description: description, Icon: icon,
				Selected: d.agentDraft != nil && d.agentDraft.editingName == row.info.Name,
			}).Label(row.info.Name).FillWidth().Clicked() {
				d.openAgentSettings(row.info.Name)
			}
		}
	})
}

// openAgentSettings 打开草稿或读取已保存的配置。
func (d *Desktop) openAgentSettings(name string) {
	if d.isAgentDirty() {
		d.confirm = confirmation{Kind: "select-agent", Name: name}
		d.confirmOpen = true
		return
	}
	d.settingsError = nil
	if name == "" {
		d.setAgentDraft(defs.AgentConfig{Type: defs.AgentTypeNative}, "")
		return
	}
	if d.client == nil {
		return
	}
	client, request := d.client, d.settingsRequest
	d.tasks.Go(func() {
		definition, err := client.GetAgent(d.ctx, name)
		d.window.Update(func() {
			if d.closed || d.settingsRequest != request {
				return
			}
			d.settingsError = err
			if err == nil {
				d.setAgentDraft(definition, name)
			}
		})
	})
}

// setAgentDraft 用服务端配置建立可编辑的原文和保存基线。
func (d *Desktop) setAgentDraft(definition defs.AgentConfig, name string) {
	d.agentPresetQuery = ""
	draft := newAgentSettingsDraft(definition, name)
	d.agentDraft = &draft
	d.agentBaseline = newAgentSettingsDraft(definition, name)
}

// newAgentSettingsDraft 把 Agent 配置转成表单字段。
func newAgentSettingsDraft(definition defs.AgentConfig, name string) agentSettingsDraft {
	draft := agentSettingsDraft{
		definition: definition, editingName: name, kind: string(definition.Type),
		tools: strings.Join(definition.Tools, " "), skills: strings.Join(definition.Skills, " "),
		mcp: strings.Join(definition.MCPServers, " "),
	}
	if draft.kind == "" {
		draft.kind = string(defs.AgentTypeNative)
	}
	if definition.MaxSteps != nil {
		draft.maxSteps = strconv.Itoa(*definition.MaxSteps)
	}
	if definition.ACP != nil {
		draft.command = definition.ACP.Command
		draft.args = strings.Join(definition.ACP.Args, "\n")
		draft.env = newKeyValueRows(definition.ACP.Env)
	}
	return draft
}

func (d *Desktop) isAgentDirty() bool {
	return d.agentDraft != nil && !reflect.DeepEqual(*d.agentDraft, d.agentBaseline)
}

// agentFormView 绘制对应执行类型的 Agent 字段。
func (d *Desktop) agentFormView(c *ui.Context) {
	draft := d.agentDraft
	title := draft.editingName
	if title == "" {
		title = d.t("agent.add")
	}
	kit.SettingSection(c.Key("agent-form"), title, func() {
		if draft.editingName == "" {
			d.agentPresetView(c, draft)
			kit.SettingRow(c, d.t("agent.type"), "", func() {
				d.settingSelectView(c, "agent-type", d.t("agent.type"), &draft.kind, kit.SelectOpt{
					Options: selectOptions([]string{"native", "acp"}, []string{d.t("agent.native"), "ACP"}),
				})
			})
		}
		d.settingInputRow(c, "agent-name", d.t("agent.name"), "", &draft.definition.Name, settingInputOpt{ReadOnly: draft.editingName != ""})
		d.settingInputRow(c, "agent-description", d.t("agent.description"), "", &draft.definition.Description, settingInputOpt{})
		kind := defs.AgentType(draft.kind)
		if kind == defs.AgentTypeACP {
			d.settingInputRow(c, "agent-command", d.t("agent.command"), "", &draft.command, settingInputOpt{})
			kit.SettingRow(c, d.t("agent.args"), "", func() {
				kit.TextArea(c.Key("agent-args"), &draft.args).Label(d.t("agent.args")).Width(settingInputWidth).Height(64)
			})
		} else if !kind.IsExternal() {
			d.agentModelRow(c, "agent-model", d.t("agent.model"), &draft.definition.Model)
			d.agentModelRow(c, "agent-compact-model", d.t("agent.compact.model"), &draft.definition.CompactModel)
			kit.SettingRow(c, d.t("agent.instructions"), "", func() {
				kit.TextArea(c.Key("agent-instructions"), &draft.definition.Instructions).Label(d.t("agent.instructions")).Width(settingInputWidth).Height(96)
			})
			d.settingInputRow(c, "agent-max-steps", d.t("agent.max.steps"), "", &draft.maxSteps, settingInputOpt{})
		}
		tools, skills, hint := d.t("agent.tools"), d.t("agent.skills"), ""
		if kind.IsExternal() {
			tools, skills, hint = d.t("agent.external.tools"), d.t("agent.external.skills"), d.t("agent.external.hint")
		}
		d.settingInputRow(c, "agent-tools", tools, hint, &draft.tools, settingInputOpt{})
		d.settingInputRow(c, "agent-skills", skills, hint, &draft.skills, settingInputOpt{})
		d.settingInputRow(c, "agent-mcp", "MCP", "", &draft.mcp, settingInputOpt{})
	})
	if defs.AgentType(draft.kind) == defs.AgentTypeACP {
		d.keyValueSection(c.Key("agent-env"), d.t("agent.env"), &draft.env, keyValueSectionOpt{
			nameWidth: 160, valueWidth: settingInputWidth, addLabel: d.t("agent.add.env"),
			removeLabel: d.t("delete"), removeIcon: kit.IconX,
		})
	}
	if draft.editingName != "" && draft.editingName != "default" {
		if kit.DangerButton(c.Key("delete-agent"), d.t("agent.delete")).AlignSelf(ui.Start).Disabled(d.isAgentDirty() || d.agentSaving).Clicked() {
			d.confirm = confirmation{Kind: "delete-agent", Name: draft.editingName}
			d.confirmError = nil
			d.confirmOpen = true
		}
	}
	if d.agentError != nil {
		d.errorMessageView(c, d.agentError)
	}
}

// agentPresetView 展开并搜索带品牌图标的 ACP 预设。
func (d *Desktop) agentPresetView(c *ui.Context, draft *agentSettingsDraft) {
	label := d.t("provider.preset")
	kit.SettingRow(c, label, "", func() {
		theme := kit.GetTheme(c)
		query := &d.agentPresetQuery
		picker := ui.ComboboxBase(c.Key("agent-preset"), query)
		input := picker.Input.Label(label).Placeholder(d.t("agent.preset.placeholder")).Disabled(len(d.agentPresets) == 0 || d.agentSaving)
		theme.TextRole(input, kit.TextUI).Width(settingInputWidth).Height(theme.Metrics.ControlHeight).
			Padding(0, theme.Space(2)).Background(theme.Colors.Input).Border(1, theme.Colors.Border).Radius(theme.Metrics.Radius)
		picker.Popup(func(panel ui.Element) {
			panel.Width(settingInputWidth).MaxHeight(320).Padding(theme.Space(1)).Margin(theme.Space(1), 0, 0, 0).
				Background(theme.Colors.Elevated).Border(1, theme.Colors.Border).Radius(theme.Metrics.RadiusLarge)
			ui.Scroll(c).MaxHeight(312).Children(func() {
				for _, preset := range d.agentPresets {
					if picker.Filtering() && !strings.Contains(strings.ToLower(preset.DisplayName+" "+preset.Name+" "+preset.Command), strings.ToLower(*query)) {
						continue
					}
					row := picker.Item(preset.Name).Label(preset.DisplayName).Height(theme.Metrics.ControlHeight).
						Padding(0, theme.Space(2)).Gap(theme.Space(2)).AlignItems(ui.Center).Radius(theme.Metrics.Radius)
					if row.Highlighted() {
						row.Background(theme.Colors.Hover)
					}
					row.Children(func() {
						ui.Icon(c, agentIcons[preset.Icon]).Size(16, 16)
						theme.TextRole(ui.Text(c, preset.DisplayName).SingleLine(), kit.TextUI)
					})
				}
			})
		})
		if name, ok := picker.Chosen(); ok {
			for _, preset := range d.agentPresets {
				if preset.Name != name {
					continue
				}
				used := make([]string, 0, len(d.agentRows))
				for _, row := range d.agentRows {
					used = append(used, row.info.Name)
				}
				filled := newAgentSettingsDraft(defs.AgentConfig{
					Name: agent.GetAgentPresetName(preset.Name, used), Type: defs.AgentTypeACP, Description: preset.Description,
					ACP: &defs.ACPCommand{Command: preset.Command, Args: preset.Args, Env: preset.Env, EphemeralParamOverwrite: preset.EphemeralParamOverwrite},
				}, "")
				draft.definition.Name, draft.definition.Description = filled.definition.Name, filled.definition.Description
				draft.definition.ACP = filled.definition.ACP
				draft.kind, draft.command, draft.args, draft.env = filled.kind, filled.command, filled.args, filled.env
				*query = preset.DisplayName
				break
			}
		}
	})
}

// agentModelRow 使用与其他设置项一致的下拉选择原生模型。
func (d *Desktop) agentModelRow(c *ui.Context, key, label string, model **defs.AgentModelConfig) {
	options := []kit.SelectOption{{Label: d.t("default.badge")}}
	selected := ""
	if *model != nil {
		selected = (*model).Provider + "/" + (*model).Model
	}
	for _, definition := range d.agentModels {
		ref := definition.Ref.String()
		options = append(options, kit.SelectOption{Value: ref, Label: ref})
	}
	kit.SettingRow(c, label, "", func() {
		if d.settingSelectView(c, key, label, &selected, kit.SelectOpt{Options: options, Searchable: true}) {
			*model = nil
			if selected != "" {
				ref, _ := defs.ParseModelRef(selected)
				*model = &defs.AgentModelConfig{Provider: ref.ProviderID, Model: ref.ModelID}
			}
		}
	})
}

// saveAgentSettings 提交完整定义，成功后更新列表与草稿基线。
func (d *Desktop) saveAgentSettings(draft *agentSettingsDraft) {
	value := draft.definition
	value.Type = defs.AgentType(draft.kind)
	value.Name = strings.TrimSpace(value.Name)
	if draft.editingName == "" && slices.ContainsFunc(d.agentRows, func(row agentSettingsRow) bool { return row.info.Name == value.Name }) {
		d.agentError = fmt.Errorf(d.t("agent.name.already.exists"), value.Name)
		return
	}
	value.Tools, value.Skills = strings.Fields(draft.tools), strings.Fields(draft.skills)
	value.MCPServers = nil
	if fields := strings.Fields(draft.mcp); len(fields) > 0 {
		value.MCPServers = fields
	}
	if value.Type == defs.AgentTypeACP {
		value.Model, value.CompactModel, value.Instructions, value.MaxSteps = nil, nil, "", nil
		command := defs.ACPCommand{Command: strings.TrimSpace(draft.command), Env: keyValueMap(draft.env)}
		if value.ACP != nil {
			command.ParamOverwrite = value.ACP.ParamOverwrite
			command.EphemeralParamOverwrite = value.ACP.EphemeralParamOverwrite
		}
		value.ACP = &command
		if draft.args != "" {
			value.ACP.Args = strings.Split(draft.args, "\n")
		}
	} else if !value.Type.IsExternal() {
		value.ACP = nil
		value.MaxSteps = nil
		if strings.TrimSpace(draft.maxSteps) != "" {
			steps, err := strconv.Atoi(strings.TrimSpace(draft.maxSteps))
			if err != nil {
				d.agentError = fmt.Errorf("%s: %w", d.t("agent.max.steps"), err)
				return
			}
			value.MaxSteps = new(steps)
		}
	}
	d.agentSaving, d.agentError = true, nil
	client := d.client
	d.tasks.Go(func() {
		_, err := client.SetAgent(d.ctx, value)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.agentSaving, d.agentError = false, err
			if err == nil {
				d.setAgentDraft(value, value.Name)
				d.loadAgentSettings()
				d.loadAgents()
				d.loadModels(d.readingTarget)
				d.showSuccess(d.t("agent.saved"), value.Name)
			}
		})
	})
}

// agentBrandIcon 按服务端上报的图标键取内嵌 SVG。
func agentBrandIcon(key string) *ui.SVG {
	if icon := agentIcons[key]; icon != nil {
		return icon
	}
	return kit.IconBot
}
