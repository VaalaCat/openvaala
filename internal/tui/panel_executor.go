package tui

import (
	"cmp"
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/charmbracelet/x/ansi"
)

// executorPanel 保存 Executor 面板的过滤词、列表位置和待提交设置。
type executorPanel struct {
	fetch panelFetch
	// conversationID 是打开面板时要修改的对话，草稿时为空。
	conversationID defs.ConversationID
	// settings 是这个对话 Executor 设置的副本；Space 只改副本的 ExecutorIDs，Enter 时提交。
	settings defs.ExecutorSettings
	filter   string
	list     listCursor
}

// Executor 列表各列的最大显示宽度：ID、说明超宽时截断加 "…"，列之间隔两个空格。
const (
	executorIDWidth          = 16
	executorDescriptionWidth = 24
)

// runExecutorCommand 打开 Executor 面板并刷新列表。
func (m *tuiModel) runExecutorCommand(string) tea.Cmd {
	// 1. 复制当前对话或草稿的 Executor 设置。
	conversationID, settings := m.getEditableExecutorSettings()
	opened := &executorPanel{
		conversationID: conversationID,
		settings:       cloneExecutorSettings(settings),
	}
	// 2. 标记加载中并打开面板，发出列表请求。
	opened.fetch.start()
	return m.openPanel(opened, opened.getFetch(m))
}

// getTitle 返回 Executor 面板标题。
func (p *executorPanel) getTitle() string {
	// 1. 返回固定标题。
	return "Executor"
}

// getFetch 返回拉取 Executor 列表的请求。
func (p *executorPanel) getFetch(m *tuiModel) func(ctx context.Context) (any, error) {
	// 1. 返回调用客户端读取 Executor 列表的请求。
	client := m.opt.Client
	return func(ctx context.Context) (any, error) {
		return client.ListExecutors(ctx)
	}
}

// getFiltered 返回按 ID 或说明匹配过滤词的 Executor。
func (p *executorPanel) getFiltered(executors []conversation.ExecutorDetails) []conversation.ExecutorDetails {
	// 1. 忽略大小写匹配 ID 和说明，按原顺序收集结果。
	filter := strings.ToLower(p.filter)
	var filtered []conversation.ExecutorDetails
	for _, each := range executors {
		if strings.Contains(strings.ToLower(each.ID+" "+each.Description), filter) {
			filtered = append(filtered, each)
		}
	}
	return filtered
}

// handleKey 处理 Executor 选择、设置提交、刷新和过滤按键。
func (p *executorPanel) handleKey(m *tuiModel, msg tea.KeyPressMsg) (panelKeyResult, tea.Cmd) {
	// 1. 读取过滤结果，并把光标移回列表范围。
	filtered, key := p.getFiltered(m.executors), msg.String()
	p.list.clamp(len(filtered))
	// 2. Esc 关闭；Space 切换选中项是否可用；Enter 把选中项设为默认、提交设置并关闭面板；
	// Ctrl+R 刷新；Backspace 删掉过滤词最后一个字；移动键选择；其他文字追加到过滤词。
	switch {
	case key == "esc":
		return panelKeyClosed, nil
	case key == "space":
		if len(filtered) > 0 {
			p.toggle(m.executors, filtered[p.list.selected].ID)
		}
		return panelKeyHandled, nil
	case key == "enter":
		// memory 不能当默认 Executor。
		if len(filtered) == 0 || filtered[p.list.selected].Kind == defs.ExecutorKindMemory {
			return panelKeyHandled, nil
		}
		return panelKeyClosed, m.setExecutorSettings(p.conversationID, p.getSubmittedSettings(m.executors, filtered[p.list.selected].ID))
	case key == "ctrl+r":
		p.fetch.start()
		return panelKeyHandled, m.fetchForPanel(p.getFetch(m))
	case key == "backspace":
		if p.filter != "" {
			p.filter, p.list = trimLastRune(p.filter), listCursor{}
		}
		return panelKeyHandled, nil
	case p.list.handleKey(key, len(filtered), m.getPanelPageSize()):
		return panelKeyHandled, nil
	case msg.Text != "":
		p.filter, p.list = p.filter+msg.Text, listCursor{}
		return panelKeyHandled, nil
	}
	return panelKeyIgnored, nil
}

// toggle 切换一项 Executor 的可用状态。
func (p *executorPanel) toggle(executors []conversation.ExecutorDetails, executorID string) {
	// 1. 默认 Executor 总是可用，不切换。
	if defaultID, _ := getDefaultExecutorLocation(p.settings, executors); executorID == defaultID {
		return
	}
	// 2. 可用列表为空表示全部可用，先展开成全部 ID，再添加或移除选中项。
	executorIDs := p.settings.ExecutorIDs
	if len(executorIDs) == 0 {
		for _, each := range executors {
			executorIDs = append(executorIDs, each.ID)
		}
	}
	if slices.Contains(executorIDs, executorID) {
		p.settings.ExecutorIDs = slices.DeleteFunc(executorIDs, func(id string) bool { return id == executorID })
		return
	}
	p.settings.ExecutorIDs = append(executorIDs, executorID)
}

// getSubmittedSettings 返回包含默认项和可用列表的待提交设置。
func (p *executorPanel) getSubmittedSettings(executors []conversation.ExecutorDetails, defaultID string) defs.ExecutorSettings {
	// 1. 设置默认 Executor，按列表顺序收集默认项和已勾选项。
	settings := p.settings
	settings.DefaultExecutorID = defaultID
	var executorIDs []string
	for _, each := range executors {
		if each.ID == defaultID || settings.IsExecutorAvailable(each.ID) {
			executorIDs = append(executorIDs, each.ID)
		}
	}
	// 2. 全部勾选时存空列表，后续新增的 Executor 也可用。
	if len(executorIDs) == len(executors) {
		executorIDs = nil
	}
	settings.ExecutorIDs = executorIDs
	return settings
}

// handleResult 结束加载，把查到的 Executor 列表或错误交给 tuiModel 更新。
func (p *executorPanel) handleResult(m *tuiModel, msg panelResultMsg) tea.Cmd {
	// 1. 结束加载，交给 tuiModel 更新 Executor 列表。
	p.fetch.finish(msg.err)
	executors, _ := msg.value.([]conversation.ExecutorDetails)
	m.handleExecutorsLoaded(executorsLoadedMsg{
		executors: executors,
		err:       msg.err,
	})
	return nil
}

// render 拼出 Executor 搜索框、列表、标记说明和操作提示。
func (p *executorPanel) render(m *tuiModel, width, maxHeight int) []string {
	// 1. 写入搜索词、加载状态和空列表提示。
	lines := []string{"搜索：" + p.filter + "▏"}
	if status := p.fetch.formatStatusLine(m); status != "" {
		lines = append(lines, status)
	}
	filtered := p.getFiltered(m.executors)
	if len(filtered) == 0 && !p.fetch.isLoading && p.fetch.err == nil {
		lines = append(lines, m.styles.faint.Render("没有匹配的 Executor"))
	}
	// 2. 读取各项工作目录（对话设置里有就用设置的，否则用 Executor 自己的），计算对齐宽度。
	defaultID, _ := getDefaultExecutorLocation(p.settings, m.executors)
	directories := make([]string, len(filtered))
	idWidth, descriptionWidth, directoryWidth := 0, 0, 0
	for index, each := range filtered {
		directories[index] = getDisplayWorkspaceDirectory(each.ID, cmp.Or(p.settings.WorkspaceDirectories[each.ID], each.WorkspaceDirectory), m.isLocalServer)
		idWidth = max(idWidth, min(ansi.StringWidth(each.ID), executorIDWidth))
		descriptionWidth = max(descriptionWidth, min(ansi.StringWidth(each.Description), executorDescriptionWidth))
		directoryWidth = max(directoryWidth, ansi.StringWidth(directories[index]))
	}
	// 3. 生成可用标记、ID、说明、目录和默认标记。
	rows := make([]string, len(filtered))
	for index, each := range filtered {
		marker := ""
		if each.ID == defaultID {
			marker = "  默认"
		}
		rows[index] = getSelectionMark(each.ID == defaultID || p.settings.IsExecutorAvailable(each.ID)) + " " +
			padRight(ansi.Truncate(each.ID, executorIDWidth, "…"), idWidth) + "  " +
			padRight(ansi.Truncate(each.Description, executorDescriptionWidth, "…"), descriptionWidth) + "  " +
			padRight(directories[index], directoryWidth) + marker
	}
	// 4. 按剩余高度显示列表，再加说明和操作提示。
	legend := m.styles.faint.Render("● 可用  ○ 不可用；全部可用时，以后新加的 Executor 也自动可用")
	lines = append(lines, renderListRows(m, rows, &p.list, maxHeight-3-len(lines), width-4)...)
	lines = append(lines, legend)
	hints := []keyHint{{action: "打字过滤"}, {
		key:    "enter",
		action: "设为默认",
	}, {
		key:    "space",
		action: "切换可用",
	}, {
		key:    "esc",
		action: "取消",
	}}
	return renderPanelFrame(p.getTitle(), hints, lines, width, maxHeight, m.styles)
}
