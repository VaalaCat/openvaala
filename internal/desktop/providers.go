package desktop

import (
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/egoist/mygo/ui"
)

// loadProviders 读取完整配置及默认模型，保留有修改的编辑原文。
func (d *Desktop) loadProviders() {
	// 1. 外部变化不覆盖正在编辑的草稿。
	if d.client == nil || d.providerLoading {
		return
	}
	d.providerLoading = true
	client := d.client
	d.tasks.Go(func() {
		providers, defaultModel, defaults, err := client.ListProviders(d.ctx)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.providerLoading = false
			d.providerError = err
			if err != nil {
				return
			}
			d.providers = providers
			d.defaultModel = defaultModel
			d.modelDefaults = defaults
			if d.providerDraft != nil {
				for _, provider := range providers {
					if provider.ID == d.providerDraft.ID {
						fresh := providerToDraft(provider)
						fresh.EditingID = provider.ID
						if defaultModel.ProviderID == provider.ID {
							fresh.DefaultModelKey = defaultModel.ModelID
						}
						if d.isProviderDirty() {
							if !reflect.DeepEqual(fresh, d.providerBaseline) {
								d.showToast(kit.ToastOpt{Title: d.t("configuration.changed.externally.your.draft.is.preserved"), Kind: kit.ToastInfo})
							}
						} else {
							d.providerDraft = fresh
							d.providerBaseline = cloneProviderDraft(fresh)
						}
					}
				}
			}
		})
	})
}

// providersView 绘制模型服务页：服务列表、服务目录、默认模型和编辑表单。
func (d *Desktop) providersView(c *ui.Context) {
	// 1. 列表为空时服务目录直接展开，是页面的主内容；服务目录只在需要显示时才读取。
	isEmpty := len(d.providers) == 0 && d.providerError == nil && !d.providerLoading
	showCatalog := d.catalogOpen || isEmpty
	if showCatalog && !d.catalogRequested {
		d.loadCatalogProviders()
	}
	if d.providerError != nil {
		d.errorMessageView(c, d.providerError)
	}
	if showCatalog && d.catalogError != nil {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			d.errorMessageView(c, d.catalogError).Shrink(1)
			if kit.Button(c, d.t("retry")).Clicked() {
				d.catalogRequested, d.catalogError = false, nil
			}
		})
	}
	if isEmpty {
		kit.FieldMessage(c, kit.MessageDescription, d.t("providers.empty"))
	}
	// 2. 服务列表：服务名、基础地址，行尾是默认标记、模型数量和协议徽标。
	if len(d.providers) > 0 {
		editingID := ""
		if d.providerDraft != nil {
			editingID = d.providerDraft.EditingID
		}
		kit.SettingSection(c, d.t("settings.providers"), func() {
			for _, provider := range d.providers {
				row := kit.ListRow(c.Key("provider:"+provider.ID), kit.ListRowOpt{Title: provider.ID, Description: provider.BaseURL, Selected: provider.ID == editingID, Trailing: func() {
					if d.defaultModel.ProviderID == provider.ID {
						kit.Badge(c, d.t("default.badge"), kit.BadgeAccent)
					}
					ui.Text(c, fmt.Sprintf(d.t("model.count"), len(provider.Models))).SingleLine().Shrink(0).TextColor(kit.GetTheme(c).Colors.Muted)
					kit.Badge(c, getProtocolLabel(provider.Protocol), kit.BadgeNeutral)
				}}).FillWidth().Label(provider.ID)
				if row.Clicked() {
					d.openProvider(provider, provider.ID, "")
				}
			}
		})
	}
	if showCatalog && len(d.catalogProviders) > 0 {
		kit.SettingSection(c, d.t("provider.catalog"), func() {
			for _, provider := range d.catalogProviders {
				row := kit.ListRow(c.Key("catalog:"+provider.ID), kit.ListRowOpt{Title: provider.Name, Description: provider.BaseURL, Trailing: func() {
					kit.Badge(c, getProtocolLabel(provider.Protocol), kit.BadgeNeutral)
				}}).FillWidth().Label(provider.Name)
				if row.Clicked() {
					d.openProvider(defs.ProviderConfig{ID: provider.ID, Protocol: provider.Protocol, BaseURL: provider.BaseURL}, "", provider.ID)
				}
			}
		})
	}
	// 3. 全局默认模型。
	defaults := []string{}
	for _, provider := range d.providers {
		for name := range provider.Models {
			defaults = append(defaults, provider.ID+"/"+name)
		}
	}
	slices.Sort(defaults)
	if len(defaults) > 0 {
		kit.SettingSection(c, d.t("default.model"), func() {
			kit.SettingRow(c, d.t("global.default.model"), "", func() {
				defaultModel := d.defaultModel.String()
				if d.settingSelectView(c, "global-default", d.t("global.default.model"), &defaultModel, kit.SelectOpt{Options: selectOptions(defaults, defaults), Disabled: d.client == nil || d.clientStopped || d.providerSaving}) {
					d.setDefaultModel(defaultModel)
				}
			})
		})
	}
	if d.providerDraft != nil {
		d.providerFormView(c)
	}
}

// setDefaultModel 保存全局默认模型。
func (d *Desktop) setDefaultModel(model string) {
	ref, _ := defs.ParseModelRef(model)
	client := d.client
	d.tasks.Go(func() {
		result, err := client.SetDefaultModel(d.ctx, ref)
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.defaultModel = ref
			d.showSuccess(d.t("default.model.saved"), "")
			if len(result.PendingConversationIDs) > 0 {
				d.showToast(kit.ToastOpt{Title: fmt.Sprintf(d.t("pending.conversations.apply.when.idle"), len(result.PendingConversationIDs)), Description: joinConversationIDs(result.PendingConversationIDs), Kind: kit.ToastInfo})
			}
		})
	})
}

// providerFormView 绘制服务编辑表单：基本信息、自定义端点、请求头、模型；应用和撤销在页头。
func (d *Desktop) providerFormView(c *ui.Context) {
	// 1. 基本信息：已有服务的 ID 不可改，只读显示。
	draft := d.providerDraft
	title := d.t("add.provider")
	if draft.EditingID != "" {
		title = draft.EditingID
	}
	protocols := []string{"openai_chat", "openai_responses", "claude"}
	protocolLabels := make([]string, len(protocols))
	for index, protocol := range protocols {
		protocolLabels[index] = getProtocolLabel(protocol)
	}
	kit.SettingSection(c.Key("provider-basic"), title, func() {
		d.settingInputRow(c, "provider-id", "Provider ID", "", &draft.ID, settingInputOpt{ReadOnly: draft.EditingID != "", Error: draft.IDError})
		kit.SettingRow(c, d.t("protocol"), "", func() {
			d.settingSelectView(c, "provider-protocol", d.t("protocol"), &draft.Protocol, kit.SelectOpt{Options: selectOptions(protocols, protocolLabels)})
		})
		d.settingInputRow(c, "provider-base-url", "Base URL", "", &draft.BaseURL, settingInputOpt{})
		d.settingInputRow(c, "provider-api-key", "API key", "", &draft.APIKey, settingInputOpt{Password: true})
	})
	// 2. 自定义端点：每个协议一行。
	kit.SettingSection(c.Key("provider-endpoints"), d.t("provider.section.endpoints"), func() {
		for index, protocol := range protocols {
			value := draft.Endpoints[protocol]
			if d.settingInputRow(c, "provider-endpoint:"+protocol, fmt.Sprintf(d.t("endpoint.label"), protocolLabels[index]), "", &value, settingInputOpt{}) {
				draft.Endpoints[protocol] = value
			}
		}
	})
	// 3. 请求头：一行一条，可添加和删除。
	d.providerHeadersView(c, draft)
	// 4. 模型：手填、拉取上游、目录模型和应用时的默认模型。
	d.providerModelsView(c, draft, protocols, protocolLabels)
	if d.providerBaseline != nil && d.providerBaseline.ID != "" {
		if kit.DangerButton(c, d.t("delete.provider")).AlignSelf(ui.Start).Disabled(d.isProviderDirty()).Clicked() {
			d.confirm = confirmation{Kind: "delete-provider", ProviderID: d.providerBaseline.ID}
			d.confirmOpen = true
		}
	}
}

// providerHeadersView 绘制表单的请求头部分：每行是名称、值和删除按钮，末尾是添加按钮和变量说明。
func (d *Desktop) providerHeadersView(c *ui.Context, draft *providerDraft) {
	// 1. 逐行绘制，删除在本帧绘制完后生效。
	removed := -1
	kit.SettingSection(c.Key("provider-headers"), d.t("provider.section.headers"), func() {
		for index, header := range draft.Headers {
			kit.SettingRow(c.Key("provider-header:"+strconv.Itoa(index)), "", "", func() {
				kit.TextInputWith(c.Key("name"), &header.Name, kit.InputOpt{Placeholder: d.t("header.name")}).Label(d.t("header.name")).Width(settingControlWidth)
				kit.TextInputWith(c.Key("value"), &header.Value, kit.InputOpt{Placeholder: d.t("header.value")}).Label(d.t("header.value")).Width(settingControlWidth)
				if kit.Button(c, d.t("remove.header")).Clicked() {
					removed = index
				}
			})
		}
		kit.SettingRow(c, d.t("add.header"), d.t("headers.hint"), func() {
			if kit.Button(c, d.t("add.header")).Clicked() {
				draft.Headers = append(draft.Headers, &providerHeaderDraft{})
			}
		})
	})
	// 2. 删除被点击的那一行。
	if removed >= 0 {
		draft.Headers = slices.Delete(draft.Headers, removed, removed+1)
	}
}

// providerModelsView 绘制表单的模型部分：添加入口、目录模型选择、默认模型，以及每个模型一个小节。
func (d *Desktop) providerModelsView(c *ui.Context, draft *providerDraft, protocols, protocolLabels []string) {
	kit.SettingSection(c.Key("provider-models"), d.t("provider.section.models"), func() {
		kit.SettingRow(c, d.t("add.model"), d.t("model.ids.separated.by.spaces"), func() {
			kit.TextInput(c.Key("manual-model"), &d.manualModel).Label(d.t("upstream.model.id")).Width(settingControlWidth)
			if kit.Button(c, d.t("add.model")).Clicked() {
				addManualModels(draft, d.manualModel)
				d.manualModel = ""
			}
		})
		kit.SettingRow(c, d.t("fetch.upstream.models"), "", func() {
			if kit.Button(c, d.t("fetch.upstream.models")).Disabled(d.client == nil || d.clientStopped || d.modelsFetching).Clicked() {
				d.fetchProviderModels()
			}
		})
		if d.selectedCatalog != "" {
			kit.SettingRow(c, d.t("catalog.models"), "", func() {
				if kit.Button(c, d.t("catalog.models")).Clicked() {
					d.loadCatalogModels(d.selectedCatalog)
				}
			})
		}
		defaultKeys := []string{""}
		defaultLabels := []string{d.t("keep.global.default")}
		for _, model := range draft.Models {
			defaultKeys = append(defaultKeys, model.Key)
			defaultLabels = append(defaultLabels, model.Alias)
		}
		kit.SettingRow(c, d.t("default.model.on.apply"), "", func() {
			d.settingSelectView(c, "provider-default-model", d.t("default.model.on.apply"), &draft.DefaultModelKey, kit.SelectOpt{Options: selectOptions(defaultKeys, defaultLabels)})
		})
	})
	if len(d.catalogModels) > 0 {
		d.catalogModelsView(c, draft)
	}
	// 每行模型使用稳定 Key，显式 false 和空列表不等同继承。
	remove := ""
	for _, model := range draft.Models {
		kit.SettingSection(c.Key("provider-model:"+model.Key), model.Alias, func() {
			if d.providerModelRows(c, model, protocols, protocolLabels) {
				remove = model.Key
			}
		})
	}
	if remove != "" {
		for _, model := range draft.Models {
			if model.Key == remove {
				draft.RemovedModels[remove] = model
			}
		}
		draft.Models = slices.DeleteFunc(draft.Models, func(model *providerModelDraft) bool { return model.Key == remove })
	}
}

// catalogModelsView 绘制可勾选的目录模型：搜索框加有最大高度的滚动列表。
func (d *Desktop) catalogModelsView(c *ui.Context, draft *providerDraft) {
	t := kit.GetTheme(c)
	kit.SettingSection(c.Key("provider-catalog-models"), d.t("catalog.models"), func() {
		ui.Row(c).Padding(t.Space(2), t.Space(3)).Children(func() {
			kit.TextInput(c.Key("catalog-filter"), &d.providerModelSearch).Placeholder(d.t("search.catalog")).Label(d.t("search.catalog")).Grow(1)
		})
		keys := []string{}
		for name := range d.catalogModels {
			keys = append(keys, name)
		}
		slices.Sort(keys)
		ui.Scroll(c).MaxHeight(280).Children(func() {
			ui.Column(c).Padding(t.Space(1), t.Space(3)).Children(func() {
				for _, name := range keys {
					if d.providerModelSearch != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(d.providerModelSearch)) {
						continue
					}
					chosen := slices.ContainsFunc(draft.Models, func(model *providerModelDraft) bool { return model.Key == name })
					if kit.Checkbox(c.Key("catalog-model:"+name), &chosen, name).Changed() {
						d.setCatalogModelChosen(draft, name, chosen)
					}
				}
			})
		})
	})
}

// setCatalogModelChosen 勾选时把目录模型加进草稿（优先恢复刚移除的同名模型），取消时移除并保留以便恢复。
func (d *Desktop) setCatalogModelChosen(draft *providerDraft, name string, chosen bool) {
	if chosen {
		model := draft.RemovedModels[name]
		if model == nil {
			model = modelToDraft(name, d.catalogModels[name])
		}
		delete(draft.RemovedModels, name)
		draft.Models = append(draft.Models, model)
		return
	}
	for _, model := range draft.Models {
		if model.Key == name {
			draft.RemovedModels[name] = model
		}
	}
	draft.Models = slices.DeleteFunc(draft.Models, func(model *providerModelDraft) bool { return model.Key == name })
}

// providerModelRows 绘制一个模型小节的所有设置行；点了「移除模型」时返回 true。
func (d *Desktop) providerModelRows(c *ui.Context, model *providerModelDraft, protocols, protocolLabels []string) (isRemoved bool) {
	// 1. 别名改动且还没有上游 ID 时，把旧别名记作上游 ID。
	oldAlias := model.Alias
	if d.settingInputRow(c, "model-alias", d.t("alias"), "", &model.Alias, settingInputOpt{}) && model.UpstreamModel == "" && strings.TrimSpace(model.Alias) != strings.TrimSpace(oldAlias) {
		model.UpstreamModel = strings.TrimSpace(oldAlias)
	}
	d.settingInputRow(c, "model-upstream", d.t("upstream.id"), "", &model.UpstreamModel, settingInputOpt{})
	d.settingInputRow(c, "model-display", d.t("display.name"), "", &model.DisplayName, settingInputOpt{})
	d.settingInputRow(c, "model-description", d.t("model.description"), "", &model.Description, settingInputOpt{})
	kit.SettingRow(c, d.t("protocol"), "", func() {
		d.settingSelectView(c, "model-protocol", d.t("protocol"), &model.Protocol, kit.SelectOpt{Options: selectOptions(append([]string{""}, protocols...), append([]string{d.t("inherit.provider.protocol")}, protocolLabels...))})
	})
	d.settingInputRow(c, "model-context", d.t("context.window.k.m"), "", &model.ContextWindow, settingInputOpt{})
	d.settingInputRow(c, "model-output", d.t("max.output.k.m"), "", &model.MaxOutputTokens, settingInputOpt{})
	kit.SettingRow(c, d.t("thinking"), "", func() {
		d.settingSelectView(c, "thinking", d.t("thinking"), &model.Thinking, kit.SelectOpt{Options: selectOptions([]string{"inherit", "true", "false"}, []string{d.t("inherit.thinking"), d.t("thinking.enabled"), d.t("thinking.disabled")})})
	})
	// 2. 继承开关：开启时使用全局设置，关闭后才出现对应的输入。
	kit.SettingRow(c, d.t("inherit.reasoning.efforts"), d.t("inherit.reasoning.efforts.hint"), func() {
		kit.Switch(c.Key("inherit-reasoning"), &model.InheritReasoning, "").Label(d.t("inherit.reasoning.efforts"))
	})
	if !model.InheritReasoning {
		d.settingInputRow(c, "model-efforts", d.t("reasoning.efforts.comma.separated.blank.none"), "", &model.ReasoningEfforts, settingInputOpt{})
	}
	d.settingInputRow(c, "model-default-effort", d.t("default.reasoning.effort"), "", &model.DefaultReasoningEffort, settingInputOpt{})
	kit.SettingRow(c, d.t("inherit.input.types"), d.t("inherit.input.types.hint"), func() {
		kit.Switch(c.Key("inherit-inputs"), &model.InheritInputs, "").Label(d.t("inherit.input.types"))
	})
	if !model.InheritInputs {
		d.settingInputRow(c, "model-inputs", d.t("input.types.text.image.audio.video.pdf"), "", &model.Inputs, settingInputOpt{})
	}
	kit.SettingRow(c, d.t("inherit.merged.message.roles"), d.t("inherit.merged.message.roles.hint"), func() {
		kit.Switch(c.Key("inherit-merge"), &model.InheritMerge, "").Label(d.t("inherit.merged.message.roles"))
	})
	if !model.InheritMerge {
		d.settingInputRow(c, "model-merge", d.t("merged.roles.blank.none"), "", &model.MergeRoles, settingInputOpt{})
	}
	// 3. 模型自己的校验错误在小节末尾，最后一行是移除。
	if model.Error != "" {
		t := kit.GetTheme(c)
		ui.Column(c).Padding(t.Space(2), t.Space(3)).Children(func() { kit.FieldMessage(c, kit.MessageError, model.Error) })
	}
	kit.SettingRow(c, d.t("remove.model"), "", func() {
		isRemoved = kit.Button(c, d.t("remove.model")).Clicked()
	})
	return isRemoved
}

// settingInputRow 绘制「标题在左、输入在右」的设置行，返回本帧输入是否改动；key 是行在所在小节内的稳定键。
func (d *Desktop) settingInputRow(c *ui.Context, key, title, description string, value *string, opt settingInputOpt) (changed bool) {
	t := kit.GetTheme(c)
	kit.SettingRow(c.Key(key), title, description, func() {
		ui.Column(c).Width(settingInputWidth).Gap(t.Space(1)).Children(func() {
			input := kit.TextInputWith(c.Key("value"), value, kit.InputOpt{ReadOnly: opt.ReadOnly, Invalid: opt.Error != ""}).Label(title)
			if opt.Password {
				input.Password()
			}
			changed = input.Changed()
			if opt.Error != "" {
				kit.FieldMessage(c, kit.MessageError, opt.Error)
			}
		})
	})
	return changed
}

// openProvider 切换模型服务编辑目标，有未保存原文时先确认放弃。
func (d *Desktop) openProvider(provider defs.ProviderConfig, editingID, catalogID string) {
	// 1. 保留完整待切换目标，取消确认不改当前编辑器。
	if d.isProviderDirty() {
		d.confirm = confirmation{Kind: "select-provider", Provider: provider, ProviderID: editingID, CatalogID: catalogID}
		d.confirmError = nil
		d.confirmOpen = true
		return
	}
	// 2. 建立本次字段草稿及默认模型的稳定行身份。
	d.providerDraft = providerToDraft(provider)
	d.providerDraft.EditingID = editingID
	if d.defaultModel.ProviderID == editingID && editingID != "" {
		d.providerDraft.DefaultModelKey = d.defaultModel.ModelID
	}
	d.providerBaseline = cloneProviderDraft(d.providerDraft)
	d.providerError = nil
	d.catalogModels = nil
	d.selectedCatalog = catalogID
	if catalogID != "" {
		d.loadCatalogModels(catalogID)
	}
}

// saveProvider 校验具体字段并提交整份 Provider。
func (d *Desktop) saveProvider() {
	if d.client == nil || d.clientStopped {
		return
	}
	// 1. 无效原文留在各自字段，服务端失败留在表单。
	draft := d.providerDraft
	draft.IDError = ""
	if draft.EditingID != "" && draft.ID != draft.EditingID {
		draft.IDError = d.t("an.existing.provider.id.cannot.be.changed")
		return
	}
	if draft.EditingID == "" && slices.ContainsFunc(d.providers, func(provider defs.ProviderConfig) bool { return provider.ID == strings.TrimSpace(draft.ID) }) {
		draft.IDError = d.t("provider.id.already.exists")
		return
	}
	provider, err := draftToProvider(draft, d.modelDefaults)
	if err != nil {
		d.providerError = err
		return
	}
	defaultModel, err := getProviderDefaultModel(draft, d.defaultModel)
	if err != nil {
		d.providerError = err
		return
	}
	d.providerSaving = true
	d.providerError = nil
	client := d.client
	submitted := cloneProviderDraft(draft)
	d.tasks.Go(func() {
		result, err := client.SetProvider(d.ctx, provider, defaultModel)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.providerSaving = false
			if d.providerDraft != draft {
				if err != nil {
					d.showFailure(err)
				}
				return
			}
			d.providerError = err
			if err != nil {
				return
			}
			submitted.EditingID = provider.ID
			draft.EditingID = provider.ID
			d.providerBaseline = submitted
			detail := ""
			if len(result.PendingConversationIDs) > 0 {
				detail = fmt.Sprintf("%s: %s", fmt.Sprintf(d.t("pending.conversations.apply.when.idle"), len(result.PendingConversationIDs)), joinConversationIDs(result.PendingConversationIDs))
			}
			d.showSuccess(d.t("provider.saved"), detail)
			d.loadProviders()
			d.loadModels(d.readingTarget)
		})
	})
}

// getProviderDefaultModel 按模型稳定行选择同次保存的默认引用。
func getProviderDefaultModel(draft *providerDraft, existing defs.ModelRef) (defs.ModelRef, error) {
	// 1. 别名改变时使用该行的新别名，删除默认行要求选择替代模型。
	if draft.DefaultModelKey != "" {
		for _, model := range draft.Models {
			if model.Key == draft.DefaultModelKey {
				return defs.ModelRef{ProviderID: strings.TrimSpace(draft.ID), ModelID: strings.TrimSpace(model.Alias)}, nil
			}
		}
		return defs.ModelRef{}, fmt.Errorf("select a replacement default model")
	}
	if existing.ProviderID == draft.ID && !slices.ContainsFunc(draft.Models, func(model *providerModelDraft) bool { return strings.TrimSpace(model.Alias) == existing.ModelID }) {
		return defs.ModelRef{}, fmt.Errorf("select a replacement default model")
	}
	return defs.ModelRef{}, nil
}

// isProviderDirty Provider 编辑草稿是否与已保存基线不同？
func (d *Desktop) isProviderDirty() bool {
	return d.providerDraft != nil && !reflect.DeepEqual(d.providerDraft, d.providerBaseline)
}
func providerToDraft(provider defs.ProviderConfig) *providerDraft {
	protocol := provider.Protocol
	if protocol == "" {
		protocol = "openai_responses"
	}
	draft := &providerDraft{ID: provider.ID, Protocol: protocol, BaseURL: provider.BaseURL, APIKey: provider.APIKey, Endpoints: map[string]string{}, RemovedModels: map[string]*providerModelDraft{}}
	for key, value := range provider.Endpoints {
		draft.Endpoints[key] = value
	}
	for _, name := range slices.Sorted(maps.Keys(provider.Headers)) {
		draft.Headers = append(draft.Headers, &providerHeaderDraft{Name: name, Value: provider.Headers[name]})
	}
	keys := []string{}
	for key := range provider.Models {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		draft.Models = append(draft.Models, modelToDraft(key, provider.Models[key]))
	}
	return draft
}
func modelToDraft(alias string, model *defs.ProviderModelConfig) *providerModelDraft {
	if model == nil {
		model = &defs.ProviderModelConfig{}
	}
	draft := &providerModelDraft{Key: alias, Alias: alias, UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Protocol: model.Protocol, Thinking: "inherit", InheritReasoning: model.ReasoningEfforts == nil, InheritInputs: model.Capabilities.Input == nil, InheritMerge: model.MergeMessageRoles == nil, ReasoningEfforts: strings.Join(model.ReasoningEfforts, ", "), DefaultReasoningEffort: model.DefaultReasoningEffort, Inputs: strings.Join(model.Capabilities.Input, ", "), MergeRoles: strings.Join(model.MergeMessageRoles, ", ")}
	draft.Description = model.Description
	if model.ContextWindow != 0 {
		draft.ContextWindow = strconv.Itoa(model.ContextWindow)
	}
	if model.MaxOutputTokens != 0 {
		draft.MaxOutputTokens = strconv.Itoa(model.MaxOutputTokens)
	}
	if model.Thinking != nil {
		draft.Thinking = strconv.FormatBool(*model.Thinking)
	}
	return draft
}
func cloneProviderDraft(draft *providerDraft) *providerDraft {
	if draft == nil {
		return nil
	}
	copy := *draft
	copy.Endpoints = map[string]string{}
	for key, value := range draft.Endpoints {
		copy.Endpoints[key] = value
	}
	copy.Headers = nil
	for _, header := range draft.Headers {
		value := *header
		copy.Headers = append(copy.Headers, &value)
	}
	copy.Models = nil
	copy.RemovedModels = map[string]*providerModelDraft{}
	for key, model := range draft.RemovedModels {
		value := *model
		copy.RemovedModels[key] = &value
	}
	for _, model := range draft.Models {
		value := *model
		copy.Models = append(copy.Models, &value)
	}
	return &copy
}
func addManualModels(draft *providerDraft, raw string) {
	for _, name := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if slices.ContainsFunc(draft.Models, func(model *providerModelDraft) bool { return model.Alias == name || model.UpstreamModel == name }) {
			continue
		}
		restored := false
		for key, model := range draft.RemovedModels {
			if model.Alias == name || model.UpstreamModel == name {
				draft.Models = append(draft.Models, model)
				delete(draft.RemovedModels, key)
				restored = true
				break
			}
		}
		if restored {
			continue
		}
		alias := defs.GetProviderModelKey(name)
		base := alias
		for i := 2; slices.ContainsFunc(draft.Models, func(model *providerModelDraft) bool { return model.Alias == alias }); i++ {
			alias = fmt.Sprintf("%s-%d", base, i)
		}
		model := modelToDraft(alias, nil)
		for suffix := 2; slices.ContainsFunc(draft.Models, func(existing *providerModelDraft) bool { return existing.Key == model.Key }) || draft.RemovedModels[model.Key] != nil; suffix++ {
			model.Key = fmt.Sprintf("%s-%d", alias, suffix)
		}
		if alias != name {
			model.UpstreamModel = name
		}
		draft.Models = append(draft.Models, model)
	}
}

// draftToProvider 逐字段校验并保留可选列表与布尔的继承区别。
func draftToProvider(draft *providerDraft, defaults defs.ModelDefaults) (defs.ProviderConfig, error) {
	// 1. 校验服务 ID、模型别名和数量原文。
	provider := defs.ProviderConfig{ID: strings.TrimSpace(draft.ID), Protocol: draft.Protocol, BaseURL: strings.TrimSpace(draft.BaseURL), APIKey: strings.TrimSpace(draft.APIKey), Endpoints: map[string]string{}, Models: map[string]*defs.ProviderModelConfig{}}
	if !validConfigID(provider.ID) {
		return provider, fmt.Errorf("invalid provider ID")
	}
	for protocol, url := range draft.Endpoints {
		if strings.TrimSpace(url) != "" {
			provider.Endpoints[protocol] = strings.TrimSpace(url)
		}
	}
	provider.Headers = toProviderHeaders(draft.Headers)
	for _, draftModel := range draft.Models {
		draftModel.Error = ""
		alias := strings.TrimSpace(draftModel.Alias)
		if !validConfigID(alias) {
			draftModel.Error = "invalid model alias"
			return provider, fmt.Errorf("%s: %s", alias, draftModel.Error)
		}
		if _, exists := provider.Models[alias]; exists {
			draftModel.Error = "duplicate model alias"
			return provider, fmt.Errorf("%s: %s", alias, draftModel.Error)
		}
		context, err := parseNumberUnit(draftModel.ContextWindow)
		if err != nil {
			draftModel.Error = "context_window: " + err.Error()
			return provider, err
		}
		output, err := parseNumberUnit(draftModel.MaxOutputTokens)
		if err != nil {
			draftModel.Error = "max_output_tokens: " + err.Error()
			return provider, err
		}
		model := &defs.ProviderModelConfig{Protocol: draftModel.Protocol, UpstreamModel: strings.TrimSpace(draftModel.UpstreamModel), DisplayName: draftModel.DisplayName, ContextWindow: context, MaxOutputTokens: output, DefaultReasoningEffort: draftModel.DefaultReasoningEffort}
		model.Description = draftModel.Description
		if draftModel.Thinking != "inherit" {
			enabled := draftModel.Thinking == "true"
			model.Thinking = &enabled
		}
		if !draftModel.InheritReasoning {
			model.ReasoningEfforts, err = parseOptionalList(draftModel.ReasoningEfforts)
			if err != nil {
				draftModel.Error = err.Error()
				return provider, err
			}
		}
		if !draftModel.InheritInputs {
			model.Capabilities.Input, err = parseOptionalList(draftModel.Inputs)
			if err != nil {
				return provider, err
			}
		}
		if !draftModel.InheritMerge {
			model.MergeMessageRoles, err = parseOptionalList(draftModel.MergeRoles)
			if err != nil {
				return provider, err
			}
		}
		efforts := model.ReasoningEfforts
		if efforts == nil {
			efforts = defaults.ReasoningEfforts
		}
		if model.DefaultReasoningEffort != "" && !slices.Contains(efforts, model.DefaultReasoningEffort) {
			draftModel.Error = "default reasoning effort is unavailable"
			return provider, fmt.Errorf("%s", draftModel.Error)
		}
		provider.Models[alias] = model
	}
	return provider, nil
}

func validConfigID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func parseNumberUnit(raw string) (int, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, nil
	}
	match := regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?|\.\d+)([km])?$`).FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("invalid nonnegative quantity: %s", raw)
	}
	number, _ := new(big.Rat).SetString(match[1])
	switch strings.ToLower(match[2]) {
	case "k":
		number.Mul(number, big.NewRat(1000, 1))
	case "m":
		number.Mul(number, big.NewRat(1000000, 1))
	}
	if !number.IsInt() || !number.Num().IsInt64() || number.Num().Int64() > 1<<53-1 {
		return 0, fmt.Errorf("quantity must be a safe integer")
	}
	return int(number.Num().Int64()), nil
}
func parseOptionalList(raw string) (defs.OptionalStringList, error) {
	result := defs.OptionalStringList{}
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" || slices.Contains(result, value) {
			return nil, fmt.Errorf("list contains an empty or duplicate value")
		}
		result = append(result, value)
	}
	return result, nil
}

// loadCatalogProviders 查询辅助目录，失败不影响手填草稿；失败后由「重试」或重新进入页面再查询。
func (d *Desktop) loadCatalogProviders() {
	client := d.client
	if client == nil {
		return
	}
	d.catalogRequested = true
	d.tasks.Go(func() {
		providers, err := client.ListCatalogProviders(d.ctx)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.catalogError = err
			if err != nil {
				return
			}
			d.catalogProviders = providers
		})
	})
}

// loadCatalogModels 查询选中目录的模型定义。
func (d *Desktop) loadCatalogModels(id string) {
	client, draft := d.client, d.providerDraft
	if client == nil {
		return
	}
	d.tasks.Go(func() {
		models, err := client.ListCatalogModels(d.ctx, id)
		d.window.Update(func() {
			if d.closed || d.providerDraft != draft {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.catalogModels = models
		})
	})
}

// fetchProviderModels 使用服务草稿连接上游，不要求先保存模型设置。
func (d *Desktop) fetchProviderModels() {
	if d.client == nil || d.clientStopped {
		return
	}
	draft := d.providerDraft
	provider := defs.ProviderConfig{ID: draft.ID, Protocol: draft.Protocol, BaseURL: draft.BaseURL, APIKey: draft.APIKey, Endpoints: draft.Endpoints, Headers: toProviderHeaders(draft.Headers)}
	client := d.client
	d.modelsFetching = true
	d.tasks.Go(func() {
		models, err := client.FetchProviderModels(d.ctx, provider)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.modelsFetching = false
			if d.providerDraft != draft {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.catalogModels = models
		})
	})
}
