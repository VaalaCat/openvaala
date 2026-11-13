package tui

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/charmbracelet/x/ansi"
)

// providerPanel 保存服务列表、添加或编辑步骤及待提交配置；默认模型取模型配置，Agent 单独指定的模型用于它的新对话。
type providerPanel struct {
	step providerPanelStep
	// hasProviderList 是否从服务列表进入？直接引导时不是，目录页按 Esc 关闭面板。
	hasProviderList bool
	// loadingText 非空时有请求在进行，面板显示这句话。
	loadingText string
	// failure 是最近一次写配置或表单校验失败的原因，换一步时清掉。
	failure string

	// providers 是第一层的 Provider（provider/list），按 id 排序。
	providers    []defs.ProviderConfig
	providerList listCursor
	// deletingProviderID 是按过一次 d、等待确认删除的 Provider。
	deletingProviderID string
	// defaultModel 是 provider/list 给出的模型配置的默认模型（models.default_model，没写时是排序后的第一个，没有模型时为零值）：
	// 第一层「默认模型」一行、第 4 步「保持当前默认模型」和它写进配置的值都用它。
	defaultModel defs.ModelRef

	// catalogProviders 是第 1 步的模型目录，按名称排序。
	catalogProviders []defs.CatalogProvider
	// catalogFailure 是拿不到目录的原因，这时只有「自定义」。
	catalogFailure string
	catalogFilter  string
	catalogList    listCursor

	// editedProvider 是编辑流程里文件中的原值，添加流程为 nil。
	editedProvider *defs.ProviderConfig
	// catalogProviderID 是第 1 步选中的目录服务，自定义时为空。
	catalogProviderID string
	form              providerForm

	// modelOptions 是第 3 步可选的模型，按 id 排序；勾选记在每一项上，换过滤词不变。
	modelOptions []*providerModelOption
	modelFilter  string
	modelList    listCursor
	// isManualModelInput 第 3 步是否在手动输入模型 id？拉不到模型或一个都没有时切到手动输入。
	isManualModelInput bool
	// manualModelIDs 是手动输入的模型 id，逗号分隔。
	manualModelIDs string
	// modelsFailure 是拉不到模型的原因。
	modelsFailure string
	// modelEditor 非 nil 时处于第 3 步的第二层，编辑一个模型的描述、上下文窗口和输出上限。
	modelEditor *providerModelEditor

	// defaultModelChoices 是第 4 步或第一层「默认模型」的选项，零值表示「保持当前默认模型」。
	defaultModelChoices []defs.ModelRef
	defaultModelList    listCursor
}

// providerPanelStep 是模型服务面板所在的一层或一步；添加流程的 4 步按顺序排列，标题里的序号由它算出。
type providerPanelStep int

const (
	// providerStepList 是第一层：默认模型、各 Provider 和「+ 添加」。
	providerStepList providerPanelStep = iota
	// providerStepDefaultModel 是第一层选「默认模型」后，从已配置的全部模型里换默认模型。
	providerStepDefaultModel
	// providerStepCatalog 是添加流程第 1 步：选服务。
	providerStepCatalog
	// providerStepForm 是第 2 步：填 ID、协议、base_url 和 API key。
	providerStepForm
	// providerStepModels 是第 3 步：勾选模型，拉不到时手动输入。
	providerStepModels
	// providerStepFlowDefaultModel 是第 4 步：从刚勾选的模型和已有的默认模型里选，选完保存。
	providerStepFlowDefaultModel
)

// providerModelOption 是第 3 步列表里的一个模型。
type providerModelOption struct {
	id string
	// config 是要写进文件的配置：目录、上游或文件里的值，带上第二层改过的上限。
	config *defs.ProviderModelConfig
	// isChecked 是否勾选了这个模型？
	isChecked bool
}

// providerProtocols 是第 2 步可选的协议，按显示顺序；自定义服务默认第一个。
var providerProtocols = []string{"openai_chat", "openai_responses", "claude"}

const (
	// customProviderName 是第 1 步「自定义」一项的名称；这一项的 ID 为空。
	customProviderName = "自定义"
	// providerOmittedProtocol 是 Provider 文件没写 protocol 时用的协议。
	providerOmittedProtocol = "openai_responses"

	providerListLoadingText   = "正在读取模型服务…"
	catalogLoadingText        = "正在获取模型目录…"
	providerModelsLoadingText = "正在获取模型列表…"
	providerSavingText        = "正在保存…"
	providerDeletingText      = "正在删除…"

	// 列表各列的显示宽度：超宽时截断加 "…"；模型 id 和显示名按列表里最宽的一项取，不超过上限。
	providerIDWidth           = 16
	catalogNameWidth          = 24
	providerModelIDMaxWidth   = 36
	providerModelNameMaxWidth = 24
	providerModelLimitWidth   = 6
)

// runProviderCommand 打开模型服务列表并读取已配置服务。
func (m *tuiModel) runProviderCommand(string) tea.Cmd {
	// 1. 创建列表面板，设置加载提示并发出服务查询。
	opened := &providerPanel{
		hasProviderList: true,
		loadingText:     providerListLoadingText,
	}
	return m.openPanel(opened, opened.getProvidersFetch(m))
}

// openProviderOnboarding 直接打开模型服务添加引导：先读已有服务和默认模型，读完再读模型目录。
func (m *tuiModel) openProviderOnboarding() tea.Cmd {
	// 1. 停在服务选择页、显示读取提示，查询已有服务和默认模型。
	opened := &providerPanel{
		step:        providerStepCatalog,
		loadingText: providerListLoadingText,
	}
	return m.openPanel(opened, opened.getProvidersFetch(m))
}

// getTitle 返回模型服务列表或当前添加、编辑步骤的标题。
func (p *providerPanel) getTitle() string {
	// 1. 列表和默认模型页使用固定标题，添加或编辑页附上步骤编号。
	switch p.step {
	case providerStepList:
		return "模型服务"
	case providerStepDefaultModel:
		return "默认模型"
	}
	action := "添加模型服务"
	if p.editedProvider != nil {
		action = "编辑模型服务"
	}
	return fmt.Sprintf("%s %d/4", action, p.step-providerStepCatalog+1)
}

// handleKey 把按键交给当前步骤处理。
func (p *providerPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 按列表、目录、表单和模型页分派，其余交给默认模型选择页。
	switch p.step {
	case providerStepList:
		return p.handleListKey(m, msg)
	case providerStepCatalog:
		return p.handleCatalogKey(m, msg)
	case providerStepForm:
		return p.handleFormKey(m, msg)
	case providerStepModels:
		return p.handleModelsKey(m, msg)
	}
	return p.handleDefaultModelKey(m, msg)
}

// handleResult 按请求结果类型更新模型服务面板。
func (p *providerPanel) handleResult(m *tuiModel, msg panelResultMsg) tea.Cmd {
	// 1. 清除加载提示，再分派服务、目录、模型、保存和删除结果。
	p.loadingText = ""
	switch value := msg.value.(type) {
	case providersLoaded:
		return p.handleProvidersLoaded(m, value, msg.err)
	case catalogProvidersLoaded:
		p.handleCatalogProvidersLoaded(value, msg.err)
	case providerModelsLoaded:
		p.handleModelsLoaded(value, msg.err)
	case providerSaved:
		return p.handleSaved(m, value, msg.err)
	case providerDeleted:
		return p.handleDeleted(m, value, msg.err)
	case defaultModelSaved:
		return p.handleDefaultModelSaved(m, value, msg.err)
	}
	return nil
}

// render 显示模型服务面板的当前步骤。
func (p *providerPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 模型编辑打开时显示设置表单。
	if p.step == providerStepModels && p.modelEditor != nil {
		return p.modelEditor.render(m, width, maxHeight)
	}
	// 2. 按当前步骤读取正文和按键提示，再添加标题及边框。
	var lines []string
	var hints []keyHint
	switch p.step {
	case providerStepList:
		lines, hints = p.renderList(m, width, maxHeight)
	case providerStepCatalog:
		lines, hints = p.renderCatalog(m, width, maxHeight)
	case providerStepForm:
		lines, hints = p.renderForm(m)
	case providerStepModels:
		lines, hints = p.renderModels(m, width, maxHeight)
	default:
		lines, hints = p.renderDefaultModel(m, width, maxHeight)
	}
	return renderPanelFrame(p.getTitle(), hints, lines, width, maxHeight, m.styles)
}

// handlePaste 清理并写入单行粘贴文字。
func (p *providerPanel) handlePaste(_ *tuiModel, text string) tea.Cmd {
	// 1. 规范粘贴文本，去掉首尾空白和换行；API key 栏替换原文，其他输入追加文字。
	text = strings.ReplaceAll(strings.TrimSpace(normalizePastedText(text)), "\n", "")
	switch {
	case text == "":
	case p.step == providerStepForm && p.form.focusedField == providerFieldAPIKey:
		p.form.apiKey = text
	default:
		p.appendText(text)
	}
	return nil
}

// ── 第一层 ──

// handleListKey 处理服务列表选择和删除确认。
func (p *providerPanel) handleListKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Esc 关闭；请求进行中忽略其他按键。
	key := msg.String()
	if key == "esc" {
		return p.close(m)
	}
	if p.loadingText != "" {
		return panelKeyIgnored, nil
	}
	// 2. 读取并清除上次删除目标，重复 d 或 y 确认删除，Enter 进入所选行，移动键更新选择。
	deletingProviderID := p.deletingProviderID
	p.deletingProviderID = ""
	provider, isProviderRow := p.getSelectedProvider()
	switch {
	case (key == "d" || key == "y") && isProviderRow && provider.ID == deletingProviderID:
		return panelKeyHandled, p.deleteProvider(m, provider.ID)
	case key == "d" && isProviderRow:
		p.deletingProviderID = provider.ID
		return panelKeyHandled, nil
	case key == "enter":
		return panelKeyHandled, p.enterListRow(m)
	case p.providerList.handleKey(key, len(p.providers)+2, m.getPanelPageSize()):
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// getSelectedProvider 返回列表选中的 Provider，以及选中的是不是 Provider 行。
func (p *providerPanel) getSelectedProvider() (defs.ProviderConfig, bool) {
	// 1. 把列表位置换算成 Provider 下标，默认模型和添加行返回未找到，其他行返回对应记录。
	index := p.providerList.selected - 1
	if index < 0 || index >= len(p.providers) {
		return defs.ProviderConfig{}, false
	}
	return p.providers[index], true
}

// enterListRow 进入选中的服务编辑、默认模型或添加流程。
func (p *providerPanel) enterListRow(m *tuiModel) tea.Cmd {
	// 1. Provider 行开始编辑，首行选择默认模型，末行进入添加。
	if provider, found := p.getSelectedProvider(); found {
		p.startEditing(provider)
		return nil
	}
	if p.providerList.selected == 0 {
		p.chooseDefaultModel()
		return nil
	}
	return p.startAdding(m)
}

// handleProvidersLoaded 处理服务列表查询结果。
func (p *providerPanel) handleProvidersLoaded(m *tuiModel, loaded providersLoaded, err error) tea.Cmd {
	// 1. 保存查询值并按 ID 排列 Provider。
	p.providers, p.defaultModel = loaded.providers, loaded.defaultModel
	slices.SortFunc(p.providers, func(a, b defs.ProviderConfig) int { return cmp.Compare(a.ID, b.ID) })
	// 2. 列表页失败时显示错误；目录页继续请求服务目录，其他页不再请求。
	switch {
	case err != nil && p.step == providerStepList:
		p.failure = "读取模型服务失败：" + getServerErrorText(err)
	case p.step == providerStepCatalog:
		p.loadingText = catalogLoadingText
		return m.fetchForPanel(p.getCatalogProvidersFetch(m))
	}
	return nil
}

// deleteProvider 请求删除指定 Provider。
func (p *providerPanel) deleteProvider(m *tuiModel, providerID string) tea.Cmd {
	// 1. 设置删除提示并清除旧错误，发出请求后返回 Provider ID 和修改结果。
	p.loadingText, p.failure = providerDeletingText, ""
	client := m.opt.Client
	return m.fetchForPanel(func(ctx context.Context) (any, error) {
		result, err := client.DeleteProvider(ctx, providerID)
		return providerDeleted{
			providerID: providerID,
			result:     result,
		}, err
	})
}

// handleDeleted 处理服务删除结果。
func (p *providerPanel) handleDeleted(m *tuiModel, deleted providerDeleted, err error) tea.Cmd {
	// 1. 失败时显示服务端原文。
	if err != nil {
		return m.showNotice("删除失败："+getServerErrorText(err), true, noticeDuration)
	}
	// 2. 提示已删除（有正在执行的对话时附上数量），同时重新读取模型目录和服务列表。
	p.loadingText = providerListLoadingText
	notice := "已删除 " + deleted.providerID + formatPendingConversations(deleted.result.PendingConversationIDs)
	return tea.Batch(m.loadModelList(), m.fetchForPanel(p.getProvidersFetch(m)), m.showNotice(notice, false, noticeDuration))
}

// renderList 生成默认模型、Provider 和添加入口的列表。
func (p *providerPanel) renderList(m *tuiModel, width, maxHeight int) ([]string, []keyHint) {
	// 1. 读取状态和默认模型，逐个生成 Provider 的模型数及地址。
	lines := p.getStatusLines(m)
	defaultModel := "—"
	if p.defaultModel != (defs.ModelRef{}) {
		defaultModel = p.defaultModel.String()
	}
	rows := []string{padRight("默认模型", providerIDWidth) + "  " + defaultModel}
	for _, provider := range p.providers {
		detail := fmt.Sprintf("%d 个模型 · %s", len(provider.Models), provider.BaseURL)
		rows = append(rows, padRight(ansi.Truncate(provider.ID, providerIDWidth, "…"), providerIDWidth)+"  "+m.styles.faint.Render(detail))
	}
	// 2. 添加入口和待删除确认提示，按高度显示列表并返回操作键。
	rows = append(rows, "+ 添加")
	var confirm []string
	if p.deletingProviderID != "" {
		confirm = append(confirm, m.styles.warning.Render("再按 d 删除 "+p.deletingProviderID+"（y 也可以），按别的键取消"))
	}
	lines = append(lines, renderListRows(m, rows, &p.providerList, maxHeight-2-len(lines)-len(confirm), width-4)...)
	hints := []keyHint{{
		key:    "enter",
		action: "编辑",
	}, {
		key:    "d",
		action: "删除",
	}, {
		key:    "esc",
		action: "关闭",
	}}
	return append(lines, confirm...), hints
}

// ── 第一层：默认模型 ──

// chooseDefaultModel 打开全部已配置模型的默认模型选择页。
func (p *providerPanel) chooseDefaultModel() {
	// 1. 收集所有 Provider 的模型引用；没有模型时保持原页。
	var refs []defs.ModelRef
	for _, provider := range p.providers {
		for modelID := range provider.Models {
			refs = append(refs, defs.ModelRef{
				ProviderID: provider.ID,
				ModelID:    modelID,
			})
		}
	}
	if len(refs) == 0 {
		return
	}
	// 2. 按引用排序，进入选择页并定位当前默认模型。
	slices.SortFunc(refs, func(a, b defs.ModelRef) int { return cmp.Compare(a.String(), b.String()) })
	p.goToStep(providerStepDefaultModel)
	p.defaultModelChoices = refs
	p.defaultModelList = listCursor{selected: max(0, slices.Index(refs, p.defaultModel))}
}

// setDefaultModel 保存默认模型并重新读取 Agent 模型目录。
func (p *providerPanel) setDefaultModel(m *tuiModel, ref defs.ModelRef) tea.Cmd {
	// 1. 设置保存提示，发出默认模型修改请求；成功后读取目录并返回结果。
	p.loadingText, p.failure = providerSavingText, ""
	client, agentName := m.opt.Client, m.opt.AgentName
	return m.fetchForPanel(func(ctx context.Context) (any, error) {
		result, err := client.SetDefaultModel(ctx, ref)
		if err != nil {
			return defaultModelSaved{}, err
		}
		return defaultModelSaved{
			ref:       ref,
			result:    result,
			modelList: getModelListAfterWrite(ctx, client, agentName),
		}, nil
	})
}

// handleDefaultModelSaved 接收默认模型保存结果并更新列表及通知。
func (p *providerPanel) handleDefaultModelSaved(m *tuiModel, saved defaultModelSaved, err error) tea.Cmd {
	// 1. 失败时留在当前页显示错误。
	if err != nil {
		p.failure = "设置默认模型失败：" + getServerErrorText(err)
		return nil
	}
	// 2. 成功时回到列表页，更新默认模型和可用目录，显示忙碌对话与 Agent 指定模型提示。
	p.goToStep(providerStepList)
	p.defaultModel = saved.ref
	notice := "默认模型：" + saved.ref.String() + formatPendingConversations(saved.result.PendingConversationIDs)
	if saved.modelList != nil {
		m.applyModelList(*saved.modelList, m.opt.AgentName)
		notice += formatAgentModelNotice(m.agentDefaultModel, saved.ref)
	}
	return m.showNotice(notice, false, noticeDuration)
}

// ── 第 1 步：选服务 ──

// startAdding 清除上次添加信息并进入服务选择页。
func (p *providerPanel) startAdding(m *tuiModel) tea.Cmd {
	// 1. 进入目录页，重置编辑目标、过滤词和列表位置，重新查询服务目录。
	p.goToStep(providerStepCatalog)
	p.editedProvider, p.catalogFilter, p.catalogList = nil, "", listCursor{}
	p.loadingText = catalogLoadingText
	return m.fetchForPanel(p.getCatalogProvidersFetch(m))
}

// handleCatalogKey 处理服务目录过滤、选择和返回。
func (p *providerPanel) handleCatalogKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 读取选项并修正选择位置。
	choices, key := p.getCatalogChoices(), msg.String()
	p.catalogList.clamp(len(choices))
	// 2. Enter 进入表单，文字和退格修改过滤词，Esc 返回列表或关闭引导；加载中也可选自定义。
	switch {
	case key == "esc":
		if !p.hasProviderList {
			return p.close(m)
		}
		p.goToStep(providerStepList)
		return panelKeyHandled, nil
	case key == "enter":
		if len(choices) > 0 {
			p.chooseCatalogProvider(choices[p.catalogList.selected])
		}
		return panelKeyHandled, nil
	case key == "backspace":
		p.trimText()
		return panelKeyHandled, nil
	case p.catalogList.handleKey(key, len(choices), m.getPanelPageSize()):
		return panelKeyHandled, nil
	case msg.Text != "":
		p.appendText(msg.Text)
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// getCatalogChoices 返回过滤后的自定义入口和目录服务。
func (p *providerPanel) getCatalogChoices() []defs.CatalogProvider {
	// 1. 把自定义放在前面，忽略大小写匹配服务名称或 ID。
	filter := strings.ToLower(p.catalogFilter)
	var choices []defs.CatalogProvider
	for _, choice := range append([]defs.CatalogProvider{{Name: customProviderName}}, p.catalogProviders...) {
		if strings.Contains(strings.ToLower(choice.Name+" "+choice.ID), filter) {
			choices = append(choices, choice)
		}
	}
	return choices
}

// chooseCatalogProvider 进入连接表单并预填所选服务。
func (p *providerPanel) chooseCatalogProvider(choice defs.CatalogProvider) {
	// 1. 切到表单页，记录目录 ID 并填写 ID、协议和地址，自定义使用首个协议。
	p.goToStep(providerStepForm)
	p.catalogProviderID = choice.ID
	p.form = providerForm{
		id:       choice.ID,
		protocol: cmp.Or(choice.Protocol, providerProtocols[0]),
		baseURL:  choice.BaseURL,
	}
}

// handleCatalogProvidersLoaded 处理服务目录查询结果。
func (p *providerPanel) handleCatalogProvidersLoaded(loaded catalogProvidersLoaded, err error) {
	// 1. 保存返回列表和错误文字。
	p.catalogProviders, p.catalogFailure = loaded.providers, ""
	if err != nil {
		p.catalogFailure = getServerErrorText(err)
	}
	// 2. 忽略名称大小写排序，同名时按 ID 排序。
	slices.SortFunc(p.catalogProviders, func(a, b defs.CatalogProvider) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
	})
}

// renderCatalog 生成服务目录搜索、状态和选择列表。
func (p *providerPanel) renderCatalog(m *tuiModel, width, maxHeight int) ([]string, []keyHint) {
	// 1. 显示搜索词、请求状态、目录错误和无匹配提示。
	lines := append([]string{"搜索：" + p.catalogFilter + "▏"}, p.getStatusLines(m)...)
	if p.catalogFailure != "" {
		lines = append(lines, m.styles.faint.Render("拿不到模型目录，只能自定义："+p.catalogFailure))
	}
	choices := p.getCatalogChoices()
	if len(choices) == 0 {
		lines = append(lines, m.styles.faint.Render("没有匹配的服务"))
	}
	// 2. 为自定义和目录服务生成行，按高度显示列表并返回操作提示。
	rows := make([]string, len(choices))
	for index, choice := range choices {
		detail := fmt.Sprintf("%s · %d 个模型", choice.ID, choice.ModelCount)
		if choice.ID == "" {
			detail = "自己填写地址和 key，从上游拉取模型"
		}
		rows[index] = padRight(ansi.Truncate(choice.Name, catalogNameWidth, "…"), catalogNameWidth) + "  " + m.styles.faint.Render(detail)
	}
	lines = append(lines, renderListRows(m, rows, &p.catalogList, maxHeight-2-len(lines), width-4)...)
	return lines, []keyHint{{action: "打字过滤"}, {
		key:    "enter",
		action: "选择",
	}, {
		key:    "esc",
		action: p.getOutermostEscAction(),
	}}
}

// getOutermostEscAction 返回目录页 Esc 的动作说明。
func (p *providerPanel) getOutermostEscAction() string {
	// 1. 从列表进入时显示返回，直接引导时显示关闭。
	if p.hasProviderList {
		return "返回"
	}
	return "关闭"
}

// ── 第 2 步：填表 ──

// startEditing 从服务列表进入预填原值的编辑表单。
func (p *providerPanel) startEditing(provider defs.ProviderConfig) {
	// 1. 记录原 Provider，锁住 ID，填写协议（没写时补默认协议）和地址，保留原 key 并把焦点放到协议栏。
	p.goToStep(providerStepForm)
	p.editedProvider, p.catalogProviderID = &provider, ""
	p.form = providerForm{
		id:           provider.ID,
		protocol:     cmp.Or(provider.Protocol, providerOmittedProtocol),
		baseURL:      provider.BaseURL,
		keptAPIKey:   provider.APIKey,
		focusedField: providerFieldProtocol,
		isIDLocked:   true,
	}
}

// handleFormKey 处理连接表单的输入、前进和返回。
func (p *providerPanel) handleFormKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Esc 编辑时回列表、添加时回目录，Enter 提交，其余按键切换栏位、协议或修改文字。
	switch key := msg.String(); {
	case key == "esc":
		if p.editedProvider != nil {
			p.goToStep(providerStepList)
			return panelKeyHandled, nil
		}
		return panelKeyHandled, p.returnToCatalog(m)
	case key == "enter":
		return panelKeyHandled, p.submitForm(m)
	case p.form.handleKey(key):
		return panelKeyHandled, nil
	case key == "backspace":
		p.trimText()
		return panelKeyHandled, nil
	case msg.Text != "":
		p.appendText(msg.Text)
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// returnToCatalog 返回服务目录。
func (p *providerPanel) returnToCatalog(m *tuiModel) tea.Cmd {
	// 1. 切到目录页；已有数据或错误时保留，否则重新发出请求。
	p.goToStep(providerStepCatalog)
	if p.catalogProviders != nil || p.catalogFailure != "" {
		return nil
	}
	p.loadingText = catalogLoadingText
	return m.fetchForPanel(p.getCatalogProvidersFetch(m))
}

// submitForm 检查必填信息后进入模型选择并查询模型。
func (p *providerPanel) submitForm(m *tuiModel) tea.Cmd {
	// 1. 检查 ID、地址和新增服务的重复 ID，失败时留在表单。
	isExistingID := slices.ContainsFunc(p.providers, func(provider defs.ProviderConfig) bool { return provider.ID == p.form.id })
	switch {
	case p.form.id == "":
		p.failure = "请填写 ID"
		return nil
	case p.form.baseURL == "":
		p.failure = "请填写 base_url"
		return nil
	case p.editedProvider == nil && isExistingID:
		p.failure = "已有 " + p.form.id + "，请换一个 ID，或在列表里编辑它"
		return nil
	}
	// 2. 切到模型页，清除模型选择和设置编辑记录，显示加载提示。
	p.goToStep(providerStepModels)
	p.modelOptions, p.modelFilter, p.modelList = nil, "", listCursor{}
	p.isManualModelInput, p.manualModelIDs, p.modelsFailure, p.modelEditor = false, "", "", nil
	p.loadingText = providerModelsLoadingText
	client, catalogProviderID, provider := m.opt.Client, p.catalogProviderID, p.getFormProvider()
	// 3. 目录服务读取目录模型，自定义或已有 Provider 查询上游模型。
	return m.fetchForPanel(func(ctx context.Context) (any, error) {
		if catalogProviderID != "" {
			models, err := client.ListCatalogModels(ctx, catalogProviderID)
			return providerModelsLoaded{models: models}, err
		}
		models, err := client.FetchProviderModels(ctx, provider)
		return providerModelsLoaded{models: models}, err
	})
}

// getFormProvider 用连接表单的内容生成 Provider 配置。
func (p *providerPanel) getFormProvider() defs.ProviderConfig {
	// 1. 读取表单字段，不含模型；key 留空时使用原 key，编辑时保留原 endpoints 和 headers。
	provider := defs.ProviderConfig{
		ID:       p.form.id,
		Protocol: p.form.protocol,
		BaseURL:  p.form.baseURL,
		APIKey:   cmp.Or(p.form.apiKey, p.form.keptAPIKey),
	}
	if p.editedProvider != nil {
		provider.Endpoints = p.editedProvider.Endpoints
		provider.Headers = p.editedProvider.Headers
	}
	return provider
}

// renderForm 生成连接表单和当前请求状态。
func (p *providerPanel) renderForm(m *tuiModel) ([]string, []keyHint) {
	// 1. 读取表单行与状态，返回切换栏位、协议、前进和返回提示。
	lines := append(p.form.render(m), p.getStatusLines(m)...)
	return lines, []keyHint{{
		key:    "tab",
		action: "切换",
	}, {
		key:    "←→",
		action: "协议",
	}, {
		key:    "enter",
		action: "下一步",
	}, {
		key:    "esc",
		action: "返回",
	}}
}

// ── 第 3 步：选模型 ──

// handleModelsLoaded 处理模型查询结果。
func (p *providerPanel) handleModelsLoaded(loaded providerModelsLoaded, err error) {
	// 1. 已离开模型页时忽略结果。
	if p.step != providerStepModels {
		return
	}
	// 2. 读取已有模型；失败或没有模型时预填已有 ID，切换手动输入。
	existing := p.getEditedModels()
	if err != nil || len(loaded.models)+len(existing) == 0 {
		p.isManualModelInput, p.modelsFailure = true, "没有拿到模型，请手动输入"
		if err != nil {
			p.modelsFailure = "拿不到模型列表：" + getServerErrorText(err)
		}
		p.manualModelIDs = strings.Join(slices.Sorted(maps.Keys(existing)), ", ")
		return
	}
	// 3. 已有模型沿用配置并勾选，其他模型未勾选，按 ID 排序保存。
	var options []*providerModelOption
	for modelID, config := range existing {
		options = append(options, &providerModelOption{
			id:        modelID,
			config:    config,
			isChecked: true,
		})
	}
	for modelID, config := range loaded.models {
		if _, found := existing[modelID]; !found {
			options = append(options, &providerModelOption{
				id:     modelID,
				config: config,
			})
		}
	}
	slices.SortFunc(options, func(a, b *providerModelOption) int { return cmp.Compare(a.id, b.id) })
	p.modelOptions = options
}

// handleModelsKey 处理模型勾选、过滤、设置编辑和前进。
func (p *providerPanel) handleModelsKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 模型编辑或手动输入打开时交给对应按键处理。
	if p.modelEditor != nil {
		return p.handleModelEditorKey(msg)
	}
	if p.isManualModelInput {
		return p.handleManualModelsKey(m, msg)
	}
	// 2. 读取过滤列表并修正位置；Esc 回表单，Enter 在没有请求进行时提交，Space 勾选，Ctrl+E 编辑模型设置，文字和退格修改过滤词。
	filtered, key := p.getFilteredModels(), msg.String()
	p.modelList.clamp(len(filtered))
	switch {
	case key == "esc":
		p.goToStep(providerStepForm)
		return panelKeyHandled, nil
	case key == "enter":
		if p.loadingText != "" {
			return panelKeyHandled, nil
		}
		return panelKeyHandled, p.submitModels(m)
	case key == "space":
		if len(filtered) > 0 {
			option := filtered[p.modelList.selected]
			option.isChecked = !option.isChecked
		}
		return panelKeyHandled, nil
	case key == "ctrl+e":
		if len(filtered) > 0 {
			p.modelEditor = newProviderModelEditor(filtered[p.modelList.selected])
		}
		return panelKeyHandled, nil
	case key == "backspace":
		p.trimText()
		return panelKeyHandled, nil
	case p.modelList.handleKey(key, len(filtered), m.getPanelPageSize()):
		return panelKeyHandled, nil
	case msg.Text != "":
		p.appendText(msg.Text)
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// handleManualModelsKey 处理手动模型 ID 输入、提交和返回。
func (p *providerPanel) handleManualModelsKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Enter 在未加载时提交，Esc 返回表单，其余文字与退格修改 ID。
	switch key := msg.String(); {
	case key == "esc":
		p.goToStep(providerStepForm)
	case key == "enter":
		if p.loadingText == "" {
			return panelKeyHandled, p.submitModels(m)
		}
	case key == "backspace":
		p.trimText()
	case msg.Text != "":
		p.appendText(msg.Text)
	default:
		return panelKeyIgnored, nil
	}
	return panelKeyHandled, nil
}

// getFilteredModels 返回 ID 或显示名匹配过滤词的模型。
func (p *providerPanel) getFilteredModels() []*providerModelOption {
	// 1. 忽略大小写匹配并保持 ID 顺序。
	filter := strings.ToLower(p.modelFilter)
	var filtered []*providerModelOption
	for _, option := range p.modelOptions {
		if strings.Contains(strings.ToLower(option.id+" "+getProviderModelDisplayName(option.config)), filter) {
			filtered = append(filtered, option)
		}
	}
	return filtered
}

// getChosenModels 返回已勾选或手动填写的模型配置。
func (p *providerPanel) getChosenModels() map[string]*defs.ProviderModelConfig {
	// 1. 手动模式把 ID 换成配置 key：编辑中已有的 key 沿用原配置，key 和 ID 不同时把 ID 记进 upstream_model。
	models := make(map[string]*defs.ProviderModelConfig)
	if p.isManualModelInput {
		for id, key := range defs.GetProviderModelKeys(parseModelIDs(p.manualModelIDs)) {
			config, found := p.getEditedModels()[key]
			switch {
			case found:
			case key != id:
				config = &defs.ProviderModelConfig{UpstreamModel: id}
			default:
				config = &defs.ProviderModelConfig{}
			}
			models[key] = config
		}
		return models
	}
	// 2. 列表模式只收集已勾选项。
	for _, option := range p.modelOptions {
		if option.isChecked {
			models[option.id] = option.config
		}
	}
	return models
}

// getEditedModels 返回正在编辑的 Provider 原模型。
func (p *providerPanel) getEditedModels() map[string]*defs.ProviderModelConfig {
	// 1. 检查原 Provider，存在时返回其模型表，添加时返回空。
	if p.editedProvider == nil {
		return nil
	}
	return p.editedProvider.Models
}

// submitModels 提交模型选择，进入默认模型页或直接保存。
func (p *providerPanel) submitModels(m *tuiModel) tea.Cmd {
	// 1. 没有所选模型时保持原页。
	models := p.getChosenModels()
	if len(models) == 0 {
		return nil
	}
	// 2. 原默认模型仍可用时添加保持选项，再按 ID 收集所选模型。
	current := p.defaultModel
	_, isCurrentKept := models[current.ModelID]
	var choices []defs.ModelRef
	if current != (defs.ModelRef{}) && (current.ProviderID != p.form.id || isCurrentKept) {
		choices = append(choices, defs.ModelRef{})
	}
	for _, modelID := range slices.Sorted(maps.Keys(models)) {
		choices = append(choices, defs.ModelRef{
			ProviderID: p.form.id,
			ModelID:    modelID,
		})
	}
	// 3. 只有一项时直接保存，多项时进入默认模型选择页。
	if len(choices) == 1 {
		return p.save(m, choices[0])
	}
	p.goToStep(providerStepFlowDefaultModel)
	p.defaultModelChoices, p.defaultModelList = choices, listCursor{}
	return nil
}

// renderModels 生成手动 ID 输入页或带用量上限的模型选择列表。
func (p *providerPanel) renderModels(m *tuiModel, width, maxHeight int) ([]string, []keyHint) {
	// 1. 手动模式显示失败原因和逗号分隔输入栏。
	if p.isManualModelInput {
		lines := p.getStatusLines(m)
		lines = append(lines, m.styles.faint.Render(p.modelsFailure), "模型 id，逗号分隔："+p.manualModelIDs+"▏")
		return lines, []keyHint{{
			key:    "enter",
			action: "下一步",
		}, {
			key:    "esc",
			action: "返回",
		}}
	}
	// 2. 列表模式显示搜索和状态，统计各列宽度及勾选数量。
	lines := append([]string{"搜索：" + p.modelFilter + "▏"}, p.getStatusLines(m)...)
	filtered := p.getFilteredModels()
	if len(filtered) == 0 && p.loadingText == "" {
		lines = append(lines, m.styles.faint.Render("没有匹配的模型"))
	}
	idWidth, nameWidth, checkedCount := ansi.StringWidth("模型"), ansi.StringWidth("显示名"), 0
	for _, option := range p.modelOptions {
		idWidth = max(idWidth, min(ansi.StringWidth(option.id), providerModelIDMaxWidth))
		nameWidth = max(nameWidth, min(ansi.StringWidth(getProviderModelDisplayName(option.config)), providerModelNameMaxWidth))
		if option.isChecked {
			checkedCount++
		}
	}
	// 3. 先扣掉边框四列、标记和列间隔十列、窗口和输出各六列，再分配模型 ID 和显示名，写入表头。
	columnBudget := width - 4 - 10 - 2*providerModelLimitWidth
	idWidth = max(1, min(idWidth, columnBudget))
	nameWidth = max(0, min(nameWidth, columnBudget-idWidth))
	if len(filtered) > 0 {
		header := "    " + padRight(ansi.Truncate("模型", idWidth, ""), idWidth) + "  " + padRight(ansi.Truncate("显示名", nameWidth, ""), nameWidth) + "  " +
			padLeft("窗口", providerModelLimitWidth) + "  " + padLeft("输出", providerModelLimitWidth)
		lines = append(lines, m.styles.faint.Render(header))
	}
	// 4. 生成勾选、名称、窗口和输出行，未知上限显示破折号，再加勾选总数及操作键。
	rows := make([]string, len(filtered))
	for index, option := range filtered {
		var contextWindow, maxOutputTokens int
		if option.config != nil {
			contextWindow, maxOutputTokens = option.config.ContextWindow, option.config.MaxOutputTokens
		}
		rows[index] = getSelectionMark(option.isChecked) + " " + padRight(ansi.Truncate(option.id, idWidth, "…"), idWidth) + "  " +
			padRight(ansi.Truncate(getProviderModelDisplayName(option.config), nameWidth, "…"), nameWidth) + "  " +
			padLeft(formatProviderModelLimit(contextWindow), providerModelLimitWidth) + "  " +
			padLeft(formatProviderModelLimit(maxOutputTokens), providerModelLimitWidth)
	}
	summary := m.styles.faint.Render(fmt.Sprintf("已勾选 %d 个", checkedCount))
	lines = append(lines, renderListRows(m, rows, &p.modelList, maxHeight-3-len(lines), width-4)...)
	lines = append(lines, summary)
	hints := []keyHint{{
		key:    "space",
		action: "勾选",
	}, {
		key:    "ctrl+e",
		action: "改设置",
	}, {
		key:    "enter",
		action: "下一步",
	}, {
		key:    "esc",
		action: "返回",
	}}
	return lines, hints
}

// ── 第 3 步第二层：模型设置 ──

// providerModelEditor 保存一个模型的描述、窗口、输出上限和输入校验错误。
type providerModelEditor struct {
	option          *providerModelOption
	contextWindow   string
	maxOutputTokens string
	description     string
	focusedField    int
	failure         string
}

// newProviderModelEditor 创建预填模型描述、窗口和输出上限的编辑页。
func newProviderModelEditor(option *providerModelOption) *providerModelEditor {
	// 1. 保存模型选项，有配置时把正数上限转为输入文字。
	editor := &providerModelEditor{option: option}
	if option.config != nil {
		editor.contextWindow = formatPositiveInt(option.config.ContextWindow)
		editor.maxOutputTokens = formatPositiveInt(option.config.MaxOutputTokens)
		editor.description = option.config.Description
	}
	return editor
}

// handleModelEditorKey 处理模型设置编辑的栏位、输入和确认。
func (p *providerPanel) handleModelEditorKey(msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Tab 或上下键换栏，Enter 校验通过后写回并关闭，Esc 放弃，文字和退格修改所选栏。
	editor := p.modelEditor
	switch key := msg.String(); {
	case key == "esc":
		p.modelEditor = nil
	case key == "enter":
		if editor.apply() {
			p.modelEditor = nil
		}
	case key == "tab" || key == "down":
		editor.focusedField = (editor.focusedField + 1) % 3
	case key == "shift+tab" || key == "up":
		editor.focusedField = (editor.focusedField + 2) % 3
	case key == "backspace":
		p.trimText()
	case msg.Text != "":
		p.appendText(msg.Text)
	default:
		return panelKeyIgnored, nil
	}
	return panelKeyHandled, nil
}

// apply 校验两个上限，连同描述写回模型配置，返回是否写入了。
func (e *providerModelEditor) apply() bool {
	// 1. 解析两个上限；任一个既不是正整数也不是空值时记录错误，不写入。
	contextWindow, isContextWindowValid := parsePositiveInt(e.contextWindow)
	maxOutputTokens, isMaxOutputValid := parsePositiveInt(e.maxOutputTokens)
	if !isContextWindowValid || !isMaxOutputValid {
		e.failure = "只能填正整数，留空用 models.defaults"
		return false
	}
	// 2. 复制已有配置并替换两个上限和描述，保存副本且勾选模型。
	var config defs.ProviderModelConfig
	if e.option.config != nil {
		config = *e.option.config
	}
	config.ContextWindow, config.MaxOutputTokens = contextWindow, maxOutputTokens
	config.Description = e.description
	e.option.config, e.option.isChecked = &config, true
	return true
}

// getFocusedText 返回指向正在编辑那一栏文字的指针。
func (e *providerModelEditor) getFocusedText() *string {
	// 1. 按焦点位置返回窗口、输出上限或描述栏。
	switch e.focusedField {
	case 0:
		return &e.contextWindow
	case 1:
		return &e.maxOutputTokens
	}
	return &e.description
}

// render 生成窗口、输出上限和描述编辑表单。
func (e *providerModelEditor) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 读取各栏的文字与焦点，生成表单行。
	fields := []struct {
		label     string
		value     string
		isFocused bool
	}{
		{
			label:     "上下文窗口",
			value:     e.contextWindow,
			isFocused: e.focusedField == 0,
		},
		{
			label:     "输出上限",
			value:     e.maxOutputTokens,
			isFocused: e.focusedField == 1,
		},
		{
			label:     "模型描述",
			value:     e.description,
			isFocused: e.focusedField == 2,
		},
	}
	var lines []string
	for _, field := range fields {
		lines = append(lines, formatProviderFormRow(m, field.label, field.value, field.isFocused, true))
	}
	// 2. 追加正整数说明、校验失败原因和操作提示，再添加边框。
	lines = append(lines, m.styles.faint.Render("窗口和输出只收正整数，留空用 models.defaults；描述可留空"))
	if e.failure != "" {
		lines = append(lines, m.styles.failure.Render("✗ "+e.failure))
	}
	hints := []keyHint{{
		key:    "tab",
		action: "切换",
	}, {
		key:    "enter",
		action: "确定",
	}, {
		key:    "esc",
		action: "取消",
	}}
	return renderPanelFrame("模型设置 · "+e.option.id, hints, lines, width, maxHeight, m.styles)
}

// ── 第 4 步：选默认模型（也用于第一层换默认模型） ──

// handleDefaultModelKey 处理默认模型选择、保存和返回。
func (p *providerPanel) handleDefaultModelKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Esc 回到上一层：第一层的默认模型页回列表，第 4 步回模型页。
	// Enter 在没有请求进行且有选项时保存：第一层只保存默认模型，第 4 步连同 Provider 一起保存，选「保持当前默认模型」时沿用原默认模型。
	// 移动键选择。
	switch key := msg.String(); {
	case key == "esc":
		if p.step == providerStepDefaultModel {
			p.goToStep(providerStepList)
		} else {
			p.goToStep(providerStepModels)
		}
		return panelKeyHandled, nil
	case key == "enter":
		if p.loadingText != "" || len(p.defaultModelChoices) == 0 {
			return panelKeyHandled, nil
		}
		choice := p.defaultModelChoices[p.defaultModelList.selected]
		if p.step == providerStepDefaultModel {
			return panelKeyHandled, p.setDefaultModel(m, choice)
		}
		return panelKeyHandled, p.save(m, cmp.Or(choice, p.defaultModel))
	case p.defaultModelList.handleKey(key, len(p.defaultModelChoices), m.getPanelPageSize()):
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// save 把 Provider（含模型）和所选默认模型一起保存，成功后重新读取 Agent 模型目录。
func (p *providerPanel) save(m *tuiModel, defaultModel defs.ModelRef) tea.Cmd {
	// 1. 读取连接表单及模型选择，显示保存提示。
	provider := p.getFormProvider()
	provider.Models = p.getChosenModels()
	p.loadingText, p.failure = providerSavingText, ""
	client, agentName := m.opt.Client, m.opt.AgentName
	// 2. 提交 Provider 和默认模型；成功后重新读取模型目录并返回结果。
	return m.fetchForPanel(func(ctx context.Context) (any, error) {
		result, err := client.SetProvider(ctx, provider, defaultModel)
		if err != nil {
			return providerSaved{}, err
		}
		return providerSaved{
			providerID:   provider.ID,
			defaultModel: defaultModel,
			result:       result,
			modelList:    getModelListAfterWrite(ctx, client, agentName),
		}, nil
	})
}

// handleSaved 处理服务保存结果。
func (p *providerPanel) handleSaved(m *tuiModel, saved providerSaved, err error) tea.Cmd {
	// 1. 失败时保留当前页并显示服务端错误。
	if err != nil {
		p.failure = "保存失败：" + getServerErrorText(err)
		return nil
	}
	// 2. 成功后关闭面板，应用新目录并显示保存、忙碌对话和 Agent 模型提示。
	m.closePanel()
	notice := "已保存 " + saved.providerID + formatPendingConversations(saved.result.PendingConversationIDs)
	if saved.modelList != nil {
		m.applyModelList(*saved.modelList, m.opt.AgentName)
		notice += formatAgentModelNotice(m.agentDefaultModel, saved.defaultModel)
	}
	return m.showNotice(notice, false, noticeDuration)
}

// renderDefaultModel 生成默认模型选择页和当前默认标记。
func (p *providerPanel) renderDefaultModel(m *tuiModel, width, maxHeight int) ([]string, []keyHint) {
	// 1. 读取状态并生成模型选项，流程页的零值显示保持当前默认模型。
	lines := p.getStatusLines(m)
	rows := make([]string, len(p.defaultModelChoices))
	for index, choice := range p.defaultModelChoices {
		switch {
		case p.step == providerStepDefaultModel:
			rows[index] = getSelectionMark(choice == p.defaultModel) + " " + choice.String()
		case choice == (defs.ModelRef{}):
			rows[index] = "保持当前默认模型 (" + p.defaultModel.String() + ")"
		default:
			rows[index] = choice.String()
		}
	}
	// 2. 按高度显示列表，返回完成或设为默认的按键提示。
	lines = append(lines, renderListRows(m, rows, &p.defaultModelList, maxHeight-2-len(lines), width-4)...)
	action := "完成"
	if p.step == providerStepDefaultModel {
		action = "设为默认"
	}
	return lines, []keyHint{{
		key:    "enter",
		action: action,
	}, {
		key:    "esc",
		action: "返回",
	}}
}

// ── 共用 ──

// goToStep 切换模型服务面板步骤。
func (p *providerPanel) goToStep(step providerPanelStep) {
	// 1. 切换步骤并清掉加载提示和失败原因，各步骤已填的数据保留。
	p.step, p.loadingText, p.failure = step, "", ""
}

// close 关闭模型服务面板。
func (p *providerPanel) close(m *tuiModel) (panelKeyResult, tea.Cmd) {
	// 1. 记录用户已关闭引导，返回面板关闭结果。
	m.hasDismissedProviderOnboarding = true
	return panelKeyClosed, nil
}

// appendText 向当前步骤正在编辑的栏位追加文字。
func (p *providerPanel) appendText(text string) {
	// 1. 按目录、表单、模型设置、手动 ID 或过滤词分派文字，过滤变化时重置列表位置。
	switch {
	case p.step == providerStepCatalog:
		p.catalogFilter, p.catalogList = p.catalogFilter+text, listCursor{}
	case p.step == providerStepForm:
		if focused := p.form.getFocusedText(); focused != nil {
			*focused += text
		}
	case p.step != providerStepModels:
	case p.modelEditor != nil:
		*p.modelEditor.getFocusedText() += text
	case p.isManualModelInput:
		p.manualModelIDs += text
	default:
		p.modelFilter, p.modelList = p.modelFilter+text, listCursor{}
	}
}

// trimText 删除当前步骤输入的最后一个字符。
func (p *providerPanel) trimText() {
	// 1. 找到当前输入栏，过滤词修改时重置列表位置。
	var text *string
	switch {
	case p.step == providerStepCatalog:
		text, p.catalogList = &p.catalogFilter, listCursor{}
	case p.step == providerStepForm:
		text = p.form.getFocusedText()
	case p.step != providerStepModels:
	case p.modelEditor != nil:
		text = p.modelEditor.getFocusedText()
	case p.isManualModelInput:
		text = &p.manualModelIDs
	default:
		text, p.modelList = &p.modelFilter, listCursor{}
	}
	// 2. 输入栏存在且非空时删除最后一个字符。
	if text != nil && *text != "" {
		*text = trimLastRune(*text)
	}
}

// getStatusLines 生成加载说明和失败原因行。
func (p *providerPanel) getStatusLines(m *tuiModel) []string {
	// 1. 依次添加非空的加载说明和失败原因，分别使用暗色和失败色。
	var lines []string
	if p.loadingText != "" {
		lines = append(lines, m.styles.faint.Render(p.loadingText))
	}
	if p.failure != "" {
		lines = append(lines, m.styles.failure.Render("✗ "+p.failure))
	}
	return lines
}

// getProvidersFetch 返回已配置服务和默认模型的查询请求。
func (p *providerPanel) getProvidersFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 保存客户端，生成 ListProviders 请求及结果记录。
	client := m.opt.Client
	return func(ctx context.Context) (any, error) {
		providers, defaultModel, _, err := client.ListProviders(ctx)
		return providersLoaded{
			providers:    providers,
			defaultModel: defaultModel,
		}, err
	}
}

// getModelListAfterWrite 查询配置写入后的 Agent 模型目录，查询失败时返回 nil。
func getModelListAfterWrite(ctx context.Context, client runtimeclient.Client, agentName string) *runtime.ModelList {
	// 1. 读取模型目录；查询失败只返回 nil，写入结果照常算成功。
	list, err := client.ListModels(ctx, agentName)
	if err != nil {
		return nil
	}
	return &list
}

// getCatalogProvidersFetch 返回模型服务目录查询。
func (p *providerPanel) getCatalogProvidersFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 保存客户端，生成目录查询及结果记录。
	client := m.opt.Client
	return func(ctx context.Context) (any, error) {
		providers, err := client.ListCatalogProviders(ctx)
		return catalogProvidersLoaded{providers: providers}, err
	}
}

// ── 第 2 步表单 ──

// providerForm 是第 2 步的表单：ID、协议、base_url 和 API key。
type providerForm struct {
	id       string
	protocol string
	baseURL  string
	// apiKey 是这次打字或粘贴的 key，为空时沿用 keptAPIKey。
	apiKey string
	// keptAPIKey 是文件里原有的 key。
	keptAPIKey string
	// focusedField 是光标所在的一栏。
	focusedField providerFormField
	// isIDLocked ID 是否不能改？编辑已有 Provider 时锁住。
	isIDLocked bool
}

// providerFormField 是表单的一栏，按显示顺序。
type providerFormField int

const (
	providerFieldID providerFormField = iota
	providerFieldProtocol
	providerFieldBaseURL
	providerFieldAPIKey
	// providerFieldCount 是栏数，不对应某一栏。
	providerFieldCount
)

// handleKey 处理换栏和协议切换按键，返回是否处理了。
func (f *providerForm) handleKey(key string) bool {
	// 1. 读取可编辑的首栏和栏位数量，ID 锁定时跳过 ID。
	first := providerFieldID
	if f.isIDLocked {
		first = providerFieldProtocol
	}
	count := providerFieldCount - first
	// 2. 按 Tab 或上下键循环换栏，协议栏按左右键循环选择协议，其他键不处理。
	switch {
	case key == "tab" || key == "down":
		f.focusedField = first + (f.focusedField-first+1)%count
	case key == "shift+tab" || key == "up":
		f.focusedField = first + (f.focusedField-first+count-1)%count
	case f.focusedField == providerFieldProtocol && (key == "left" || key == "right"):
		step := 1
		if key == "left" {
			step = len(providerProtocols) - 1
		}
		index := max(0, slices.Index(providerProtocols, f.protocol))
		f.protocol = providerProtocols[(index+step)%len(providerProtocols)]
	default:
		return false
	}
	return true
}

// getFocusedText 返回正在编辑的文字栏。
func (f *providerForm) getFocusedText() *string {
	// 1. 焦点在未锁定的 ID、地址或 key 栏时返回那一栏文字的指针；协议栏和锁定的 ID 没有可编辑文字，返回 nil。
	switch f.focusedField {
	case providerFieldID:
		if !f.isIDLocked {
			return &f.id
		}
	case providerFieldBaseURL:
		return &f.baseURL
	case providerFieldAPIKey:
		return &f.apiKey
	}
	return nil
}

// render 生成连接表单各栏。
func (f *providerForm) render(m *tuiModel) []string {
	// 1. key 只显示末尾至多四个字符；编辑时没填新 key 就提示沿用原 key。
	apiKey := maskAPIKey(f.apiKey)
	if f.apiKey == "" && f.keptAPIKey != "" {
		apiKey = m.styles.faint.Render("留空沿用（" + maskAPIKey(f.keptAPIKey) + "）")
	}
	// 2. 依次生成 ID、协议、地址和 API key 行。
	return []string{
		formatProviderFormRow(m, "ID", f.id, f.focusedField == providerFieldID, !f.isIDLocked),
		formatProviderFormRow(m, "协议", "‹ "+f.protocol+" ›", f.focusedField == providerFieldProtocol, false),
		formatProviderFormRow(m, "base_url", f.baseURL, f.focusedField == providerFieldBaseURL, true),
		formatProviderFormRow(m, "API key", apiKey, f.focusedField == providerFieldAPIKey, true),
	}
}

// formatProviderFormRow 生成含标签、文字和焦点标记的表单行。
func formatProviderFormRow(m *tuiModel, label, value string, isFocused, isEditable bool) string {
	// 1. 焦点行添加强调色箭头，可编辑时加光标，再连接标签和值。
	marker := "  "
	if isFocused {
		marker = m.styles.accent.Render("› ")
		if isEditable {
			value += "▏"
		}
	}
	return marker + m.styles.faint.Render(padRight(label, 10)) + "  " + value
}

// ── 拉取结果 ──

// providersLoaded 由服务列表查询返回，包含全部 Provider 和模型配置的默认模型。
type providersLoaded struct {
	providers    []defs.ProviderConfig
	defaultModel defs.ModelRef
}

// catalogProvidersLoaded 由目录查询返回，包含服务选择页的目录服务。
type catalogProvidersLoaded struct {
	providers []defs.CatalogProvider
}

// providerModelsLoaded 由目录模型查询或上游查询返回，包含模型选择页的可选配置。
type providerModelsLoaded struct {
	models map[string]*defs.ProviderModelConfig
}

// providerSaved 由服务保存请求返回，包含同次保存的默认模型和新目录；目录查询失败时 modelList 为 nil。
type providerSaved struct {
	providerID   string
	defaultModel defs.ModelRef
	result       runtime.SetDefinitionsResult
	modelList    *runtime.ModelList
}

// providerDeleted 由服务删除请求返回，包含被删除的服务和受影响对话。
type providerDeleted struct {
	providerID string
	result     runtime.SetDefinitionsResult
}

// defaultModelSaved 由默认模型保存请求返回；保存后目录查询失败时 modelList 为 nil。
type defaultModelSaved struct {
	ref       defs.ModelRef
	result    runtime.SetDefinitionsResult
	modelList *runtime.ModelList
}
