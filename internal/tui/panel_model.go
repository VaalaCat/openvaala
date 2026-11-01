package tui

import (
	"cmp"
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/charmbracelet/x/ansi"
)

// modelPanel 保存模型目录、过滤词，以及模型和推理强度的选择位置。
type modelPanel struct {
	fetch panelFetch
	// agentName 是最近一次拉取目录所用的 Agent，结果按它决定是否更新 Agent 默认模型。
	agentName string
	// list 是查到的模型目录，还没加载时为 nil。
	list   *runtime.ModelList
	filter string
	// selected 非 nil 时处于第二层，为它选择推理强度。
	selected *defs.ModelDefinition
	models   listCursor
	efforts  listCursor
}

// 模型列表各列的显示宽度：provider、显示名超宽时截断加 "…"，列之间隔一个空格。
const (
	modelProviderWidth = 10
	modelNameWidth     = 24
	modelWindowWidth   = 6
)

// runModelCommand 打开模型选择面板并读取目录。
func (m *tuiModel) runModelCommand(string) tea.Cmd {
	// 1. 记录查询所用的 Agent，标记加载中并发出请求。
	opened := &modelPanel{agentName: m.getModelListAgentName()}
	opened.fetch.start()
	return m.openPanel(opened, opened.getFetch(m))
}

// getModelListAgentName 返回模型目录查询使用的 Agent 名称。
func (m *tuiModel) getModelListAgentName() string {
	// 1. 优先读取根 Session 的 Agent，否则使用新建对话的 Agent。
	if root, found := m.sessionStatuses.getRoot(); found && root.Session.AgentName != "" {
		return root.Session.AgentName
	}
	return m.opt.AgentName
}

// getTitle 返回模型选择面板标题。
func (p *modelPanel) getTitle() string {
	// 1. 返回固定标题。
	return "选择模型"
}

// getFetch 返回按已记录 Agent 读取模型目录的请求。
func (p *modelPanel) getFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 保存客户端和 Agent 名称，生成列表查询。
	client, agentName := m.opt.Client, p.agentName
	return func(ctx context.Context) (any, error) {
		return client.ListModels(ctx, agentName)
	}
}

// getFiltered 按显示名或模型引用过滤模型，并按服务商分组。
func (p *modelPanel) getFiltered() []*defs.ModelDefinition {
	// 1. 目录还没加载时没有模型。
	if p.list == nil {
		return nil
	}
	// 2. 忽略大小写匹配模型，记录服务商首次出现顺序。
	filter := strings.ToLower(p.filter)
	var filtered []*defs.ModelDefinition
	var providers []string
	for _, model := range p.list.Models {
		if !strings.Contains(strings.ToLower(model.DisplayName+" "+model.Ref.String()), filter) {
			continue
		}
		if !slices.Contains(providers, model.Ref.ProviderID) {
			providers = append(providers, model.Ref.ProviderID)
		}
		filtered = append(filtered, model)
	}
	// 3. 按服务商顺序稳定排列匹配模型。
	slices.SortStableFunc(filtered, func(a, b *defs.ModelDefinition) int {
		return cmp.Compare(slices.Index(providers, a.Ref.ProviderID), slices.Index(providers, b.Ref.ProviderID))
	})
	return filtered
}

// handleKey 处理模型选择、过滤和刷新按键。
func (p *modelPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 推理强度页交给强度按键处理。
	if p.selected != nil {
		return p.handleEffortKey(m, msg)
	}
	// 2. Esc 关闭，Enter 选中模型，Ctrl+R 按当前 Agent 重新查询，Backspace 和文字修改过滤词，移动键选择；其他按键交回全局。
	filtered, key := p.getFiltered(), msg.String()
	switch {
	case key == "esc":
		return panelKeyClosed, nil
	case key == "enter":
		if len(filtered) == 0 {
			return panelKeyHandled, nil
		}
		return p.chooseModel(m, filtered[p.models.selected])
	case key == "ctrl+r":
		p.fetch.start()
		p.agentName = m.getModelListAgentName()
		return panelKeyHandled, m.fetchForPanel(p.getFetch(m))
	case key == "backspace":
		if p.filter != "" {
			p.filter, p.models = trimLastRune(p.filter), listCursor{}
		}
		return panelKeyHandled, nil
	case p.models.handleKey(key, len(filtered), m.getPanelPageSize()):
		return panelKeyHandled, nil
	case msg.Text != "":
		p.filter, p.models = p.filter+msg.Text, listCursor{}
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// chooseModel 选中模型，进入推理强度页或直接确认。
func (p *modelPanel) chooseModel(m *tuiModel, model *defs.ModelDefinition) (panelKeyResult, tea.Cmd) {
	// 1. 没有可选强度时直接提交模型。
	if len(model.ReasoningEfforts) == 0 {
		return p.confirm(m, defs.ModelSelection{Ref: model.Ref})
	}
	// 2. 进入强度页；优先高亮当前强度，不支持时使用模型默认值。
	p.selected = model
	highlight := model.GetReasoningEffort(m.getCurrentModelSelection().ReasoningEffort)
	p.efforts = listCursor{selected: max(0, slices.Index(model.ReasoningEfforts, highlight))}
	return panelKeyHandled, nil
}

// handleEffortKey 处理推理强度选择和返回模型列表。
func (p *modelPanel) handleEffortKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. Enter 提交所选强度；Esc、左箭头和退格返回列表，其余处理光标移动。
	switch key := msg.String(); {
	case key == "esc" || key == "left" || key == "backspace":
		p.selected = nil
		return panelKeyHandled, nil
	case key == "enter":
		return p.confirm(m, defs.ModelSelection{
			Ref:             p.selected.Ref,
			ReasoningEffort: p.selected.ReasoningEfforts[p.efforts.selected],
		})
	case p.efforts.handleKey(key, len(p.selected.ReasoningEfforts), m.getPanelPageSize()):
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// confirm 提交模型选择并关闭面板。
func (p *modelPanel) confirm(m *tuiModel, selection defs.ModelSelection) (panelKeyResult, tea.Cmd) {
	// 1. 新建请求尚未结束时显示等待提示并保持面板打开。
	if m.isOpeningDraft {
		return panelKeyHandled, m.showNotice("正在新建对话，请稍候", false, noticeDuration)
	}
	// 2. 草稿暂存模型；已有对话则发出 Session 模型修改请求。
	if m.conversation == nil {
		m.draftModel = selection
		return panelKeyClosed, m.showNotice("新对话将使用 "+m.formatModelSelection(selection), false, noticeDuration)
	}
	return panelKeyClosed, m.setSessionModel(selection)
}

// handleResult 接收模型目录查询结果。
func (p *modelPanel) handleResult(m *tuiModel, msg panelResultMsg) tea.Cmd {
	// 1. 记录错误；成功时更新面板目录和 TUI 模型信息。
	p.fetch.finish(msg.err)
	if msg.err == nil {
		list := msg.value.(runtime.ModelList)
		p.list = &list
		m.applyModelList(list, p.agentName)
	}
	return nil
}

// render 显示模型列表或推理强度列表。
func (p *modelPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 推理强度页标记当前强度，显示选择列表和返回提示。
	current := m.getCurrentModelSelection()
	if p.selected != nil {
		rows := make([]string, len(p.selected.ReasoningEfforts))
		for index, effort := range p.selected.ReasoningEfforts {
			rows[index] = getSelectionMark(p.selected.Ref == current.Ref && effort == current.ReasoningEffort) + " " + effort
		}
		hints := []keyHint{{
			key:    "enter",
			action: "确认",
		}, {
			key:    "←",
			action: "返回",
		}}
		return renderPanelFrame("推理强度 · "+p.selected.DisplayName, hints, renderListRows(m, rows, &p.efforts, maxHeight-2, width-4), width, maxHeight, m.styles)
	}
	// 2. 模型列表页写入搜索词、加载状态和空列表提示。
	lines := []string{"搜索：" + p.filter + "▏"}
	if status := p.fetch.formatStatusLine(m); status != "" {
		lines = append(lines, status)
	}
	filtered := p.getFiltered()
	if len(filtered) == 0 && !p.fetch.isLoading {
		lines = append(lines, m.styles.faint.Render("没有匹配的模型"))
	}
	// 3. 同一服务商只在第一行写名字，当前模型标 ●，截断并对齐名称和窗口列。
	rows := make([]string, len(filtered))
	for index, model := range filtered {
		provider := model.Ref.ProviderID
		if index > 0 && filtered[index-1].Ref.ProviderID == provider {
			provider = ""
		}
		window := "—"
		if model.ContextWindow > 0 {
			window = formatTokens(model.ContextWindow)
		}
		rows[index] = padRight(ansi.Truncate(provider, modelProviderWidth, "…"), modelProviderWidth) + " " + getSelectionMark(model.Ref == current.Ref) + " " +
			padRight(ansi.Truncate(model.DisplayName, modelNameWidth, "…"), modelNameWidth) + " " + padLeft(window, modelWindowWidth)
	}
	// 4. 按可用高度显示列表，再加操作提示和边框。
	lines = append(lines, renderListRows(m, rows, &p.models, maxHeight-2-len(lines), width-4)...)
	hints := []keyHint{{action: "打字过滤"}, {
		key:    "enter",
		action: "选择",
	}, {
		key:    "esc",
		action: "关闭",
	}}
	return renderPanelFrame(p.getTitle(), hints, lines, width, maxHeight, m.styles)
}

// getSelectionMark 返回选中状态标记。
func getSelectionMark(isCurrent bool) string {
	// 1. 按是否选中返回 ● 或 ○。
	if isCurrent {
		return "●"
	}
	return "○"
}
