package desktop

import (
	"cmp"
	"slices"
	"strings"
	"time"

	kit "github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/egoist/mygo/ui"
)

// composerTransition 使用原生布局方向，在操作条中收放宽度，在导入列表中收放高度。
var composerTransition = ui.ElementTransition{Duration: 150 * time.Millisecond, Position: true, Size: true, Enter: &ui.Motion{Collapse: true}, Exit: &ui.Motion{Collapse: true}}

// getExternalAgent 返回实际阅读的外部会话或草稿选中的外部模型提供方。
func (d *Desktop) getExternalAgent() *runtime.AgentInfo {
	name := d.newAgent
	var binding *defs.ExternalSession
	if d.readingTarget.ConversationID != "" {
		record := d.getSessionInfo().Session
		binding = record.External
		if binding == nil {
			return nil
		}
		name = record.AgentName
	} else if ref, err := defs.ParseModelRef(d.newModel); err == nil {
		name = ref.ProviderID
	}
	for i := range d.agents {
		if d.agents[i].Name == name && d.agents[i].Type.IsExternal() {
			return &d.agents[i]
		}
	}
	if binding != nil {
		return &runtime.AgentInfo{Name: name, Type: binding.AgentType}
	}
	return nil
}

// probeDraftOptions 在位置或选择改变时请求实际选项；过期响应不能覆盖新选择。
func (d *Desktop) probeDraftOptions(agent *runtime.AgentInfo, refresh bool) {
	if agent == nil {
		d.draftProbe = nil
		d.draftOptions = nil
		d.externalOptions = nil
		d.externalMoreOpen = false
		return
	}
	location := runtime.ExternalAgentLocation{AgentName: agent.Name, ExecutorID: d.newExecutor, Cwd: d.newDirectory}
	if d.draftProbe == nil || d.draftProbe.ExternalAgentLocation != location {
		d.externalOptions = nil
		d.draftOptions = nil
		d.externalMoreOpen = false
	}
	selections := slices.Clone(d.draftOptions)
	if ref, err := defs.ParseModelRef(d.newModel); err == nil && ref.ModelID != "default" {
		selections = append([]defs.ExternalOptionSelection{{Kind: "model", Value: ref.ModelID}}, selections...)
	}
	if !refresh && d.draftProbe != nil && d.draftProbe.ExternalAgentLocation == location && slices.Equal(d.draftProbe.Selections, selections) {
		return
	}
	req := &runtime.AgentOptionsRequest{ExternalAgentLocation: location, Selections: selections, Refresh: refresh}
	d.draftProbe = req
	client := d.client
	d.tasks.Go(func() {
		options, err := client.GetAgentOptions(d.ctx, req)
		d.window.Update(func() {
			if d.closed || d.draftProbe != req || d.client != client {
				return
			}
			if err != nil {
				d.externalOptions = nil
				d.externalMoreOpen = false
				return
			}
			d.externalOptions = &options
			d.loadModels(d.readingTarget)
		})
	})
}

func (d *Desktop) changeExternalOption(choice defs.ExternalOptionSelection) {
	if d.readingTarget.ConversationID == "" {
		d.draftOptions = slices.DeleteFunc(d.draftOptions, func(v defs.ExternalOptionSelection) bool { return v.Kind == choice.Kind && v.ID == choice.ID })
		d.draftOptions = append(d.draftOptions, choice)
		return
	}
	client := d.client
	params := runtimeprotocol.SetSessionOptionParams{SessionParams: runtimeprotocol.SessionParams{ConversationID: d.conversation.ID, SessionID: d.readingTarget.SessionID}, ExternalOptionSelection: choice}
	d.tasks.Go(func() {
		if err := client.SetSessionOption(d.ctx, &params); err != nil {
			d.window.Update(func() {
				if !d.closed {
					d.showFailure(err)
				}
			})
		}
	})
}

func (d *Desktop) externalOptionSelect(c *ui.Context, key, label string, g *defs.ExternalOptionGroup) {
	options := make([]kit.SelectOption, 0, len(g.Options))
	for _, v := range g.Options {
		options = append(options, kit.SelectOption{Value: v.Value, Label: cmp.Or(v.Label, v.Value)})
	}
	value := g.CurrentValue
	if kit.Select(c.Key(key), &value, kit.SelectOpt{Options: options, Label: label}).Transition(composerTransition).Changed() {
		d.changeExternalOption(defs.ExternalOptionSelection{Kind: g.Kind, ID: g.ID, Value: value})
	}
}

// externalSessionControls 使用会话订阅或草稿探测的控件，工作位置与导入入口并排。
func (d *Desktop) externalSessionControls(c *ui.Context, composer ui.Element, agent *runtime.AgentInfo) {
	t := kit.GetTheme(c)
	opts := d.externalOptions
	if opts != nil {
		if opts.Thought != nil && len(opts.Thought.Options) > 0 {
			d.externalOptionSelect(c, "reasoning", d.t("reasoning.effort"), opts.Thought)
		}
		if opts.Mode != nil && len(opts.Mode.Options) > 0 || slices.ContainsFunc(opts.Others, func(g defs.ExternalOptionGroup) bool { return len(g.Options) > 0 }) {
			// 退场时 MyGo 重绘上一帧的快照；在本帧取按钮颜色，避免回调再访问旧控件。
			button := kit.ToolButton(c.Key("external-more"), nil, d.t("more"), true).Draw(nil).Transition(composerTransition)
			if button.Pressed() {
				button.Background(t.Colors.Active)
			} else if button.Hovered() {
				button.Background(t.Colors.Hover)
			}
			if button.Clicked() {
				d.externalMoreOpen = !d.externalMoreOpen
			}
			ui.PopoverBase(c, button, &d.externalMoreOpen, func(panel ui.Element) {
				panel.Padding(t.Space(2)).Gap(t.Space(1)).Background(t.Colors.Elevated).Border(1, t.Colors.Border).Radius(t.Metrics.RadiusLarge).MinWidth(176)
				panel.Children(func() {
					render := func(key string, g *defs.ExternalOptionGroup) {
						if len(g.Options) == 0 {
							return
						}
						ui.Row(c.Key(key)).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
							t.TextRole(ui.Text(c, cmp.Or(g.Label, g.ID)), kit.TextUI).Grow(1)
							d.externalOptionSelect(c, key, cmp.Or(g.Label, g.ID), g)
						})
					}
					if opts.Mode != nil {
						render("external-mode", opts.Mode)
					}
					for i := range opts.Others {
						render("external-"+opts.Others[i].ID, &opts.Others[i])
					}
					if d.readingTarget.ConversationID == "" && kit.ToolButton(c, kit.IconRefresh, d.t("refresh.options"), false).Clicked() {
						d.probeDraftOptions(agent, true)
					}
				})
			})
		}
	}
	d.workLocationButton(c, composer)
	if d.readingTarget.ConversationID == "" && kit.IconButtonTip(c.Key("import-session"), kit.IconDownload, d.t("import.session")).Clicked() {
		d.openSessionImport(agent)
	}
}

// modelSelect 沿用原生模型下拉；含 ACP 分组时用同一 SelectBase 在标题与触发器中绘制品牌图标。
func (d *Desktop) modelSelect(c *ui.Context, value *string, agent *runtime.AgentInfo, ready bool) ui.Element {
	options := getModelOptions(d.models.Models, d.t("agent.default"))
	icons := map[string]*ui.SVG{}
	for _, m := range d.models.Models {
		if m == nil || icons[m.Ref.ProviderID] != nil {
			continue
		}
		if m.Icon != "" {
			icons[m.Ref.ProviderID] = agentBrandIcon(m.Icon)
		} else if slices.ContainsFunc(d.agents, func(a runtime.AgentInfo) bool { return a.Name == m.Ref.ProviderID && a.Type.IsExternal() }) {
			icons[m.Ref.ProviderID] = kit.IconBot
		}
	}
	if len(icons) == 0 && agent == nil {
		return kit.Select(c.Key("model"), value, kit.SelectOpt{Options: options, Label: d.t("model"), Placeholder: d.t("model"), Searchable: true, SearchPlaceholder: d.t("filter.provider.model"), Disabled: !ready})
	}
	if agent != nil && d.conversation.ID != "" {
		options = options[1:]
	}
	t := kit.GetTheme(c)
	parts := ui.SelectBase(c.Key("model"), value)
	trigger := parts.Trigger.Label(d.t("model")).Disabled(!ready).Height(t.Metrics.ControlHeight).Padding(0, t.Space(2)).Gap(t.Space(1)).AlignItems(ui.Center).Radius(t.Metrics.Radius)
	if trigger.Hovered() || parts.Open() {
		trigger.Background(t.Colors.Hover)
	}
	label := d.t("agent.default")
	if ref, err := defs.ParseModelRef(*value); err == nil {
		label = ref.ModelID
	}
	for _, option := range options {
		if !option.Header && option.Value == *value {
			label = option.Label
			break
		}
	}
	fade := ui.ContentTransition{Animation: &ui.Animation{Duration: 150 * time.Millisecond, Ease: ui.EaseOut}}
	trigger.Children(func() {
		if agent != nil {
			ui.Icon(c.Key("brand"), agentBrandIcon(agent.Icon)).Size(16, 16).ContentTransition(fade)
		}
		t.TextRole(ui.Text(c.Key("model-label"), label).SingleLine().Shrink(1).ContentTransition(fade), kit.TextUI)
		ui.Icon(c, kit.IconChevronDown).Size(12, 12).TextColor(t.Colors.Muted)
	})
	query := ui.Local(trigger, "model-search", func() string { return "" })
	if !parts.Open() {
		*query = ""
	}
	parts.Popup(func(panel ui.Element) {
		panel.Padding(t.Space(1)).Margin(t.Space(1), 0, 0, 0).MinWidth(180).MaxHeight(320).Background(t.Colors.Elevated).Border(1, t.Colors.Border).Radius(t.Metrics.RadiusLarge)
		search := kit.TextInputWith(c.Key("select-search"), query, kit.InputOpt{Placeholder: d.t("filter.provider.model")}).AutoFocus()
		needle := strings.ToLower(*query)
		header := ""
		var visible []string
		selected := -1
		for _, option := range options {
			if option.Header {
				header = option.Label
				continue
			}
			if !strings.Contains(strings.ToLower(option.Value+" "+option.Label+" "+option.Description), needle) {
				continue
			}
			if header != "" {
				ui.Row(c.Key("group-"+header)).Role(ui.RoleListItem).Label(header).AlignItems(ui.Center).Height(24).Gap(8).Padding(0, 8).Children(func() {
					if icon := icons[header]; icon != nil {
						ui.Icon(c, icon).Size(16, 16).Label(header)
					}
					t.TextRole(ui.Text(c, header), kit.TextCaption).TextColor(t.Colors.Muted)
				})
				header = ""
			}
			row := parts.Item(option.Value).Label(option.Label).Height(t.Metrics.ControlHeight).Padding(0, 8).AlignItems(ui.Center)
			if option.Value == *value {
				row.TextColor(t.Colors.Accent)
			}
			if row.Highlighted() {
				row.Background(t.Colors.Hover)
				selected = len(visible)
			}
			row.Children(func() { t.TextRole(ui.Text(c, option.Label).SingleLine(), kit.TextUI) })
			visible = append(visible, option.Value)
		}
		if len(visible) > 0 {
			switch {
			case search.Submitted():
				*value = visible[max(selected, 0)]
				trigger.SetChanged()
				*ui.Local(trigger, "open", func() bool { return false }) = false
			case panel.OverlayShortcut(0, ui.KeyDown):
				parts.Highlight(visible[min(selected+1, len(visible)-1)])
			case panel.OverlayShortcut(0, ui.KeyUp):
				parts.Highlight(visible[max(selected-1, 0)])
			}
		} else {
			kit.InlineHint(c, d.t("no.matching.models"), false)
		}
	})
	return trigger
}
