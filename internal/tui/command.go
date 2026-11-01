package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/charmbracelet/x/ansi"
)

// command 是斜杠弹窗里的一个候选：内置命令还用于全局快捷键和帮助，Skill 候选按读到的 Skill 列表临时生成。
type command struct {
	// name 是斜杠后面的名字，不含 "/"。
	name string
	// argumentHint 是参数提示，例如 "<目录>"，弹窗显示 "/cd <目录>"；为空时命令不带参数。
	argumentHint string
	// key 是可选的全局快捷键，与 tea.KeyPressMsg.String() 一致，例如 "ctrl+o"。
	key         string
	description string
	// isSkill 是否是 Skill 候选？选中时只替换光标处的词，不执行命令。
	isSkill bool
	// isAvailable 判断命令在当前界面状态下能不能用，不能用的不进候选。
	isAvailable func(m *tuiModel) bool
	// run 执行命令；argument 是命令名后面的文字（去掉首尾空白），不带参数的命令忽略它。
	run func(m *tuiModel, argument string) tea.Cmd
}

// slashPopupRowLimit 是 slash 弹窗最多显示的命令行数。
const slashPopupRowLimit = 8

// newCommands 返回按弹窗和帮助显示顺序排列的命令表。
func newCommands() []command {
	// 1. 列出命令名、说明、可用条件和执行方法。
	return []command{
		{
			name:        "new",
			description: "开始新对话",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runNewCommand,
		},
		{
			name:        "resume",
			description: "恢复最近的对话",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runResumeCommand,
		},
		{
			name:        "fork",
			description: "分叉当前对话",
			isAvailable: isConversationOpen,
			run:         (*tuiModel).runForkCommand,
		},
		{
			name:        "rewind",
			description: "回退到之前的消息重来",
			isAvailable: isConversationOpen,
			run:         (*tuiModel).runRewindCommand,
		},
		{
			name:        "model",
			description: "选择模型和推理强度",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runModelCommand,
		},
		{
			name:        "provider",
			description: "模型服务",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runProviderCommand,
		},
		{
			name:        "reload",
			description: "重新加载配置",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runReloadCommand,
		},
		{
			name:        "context",
			description: "上下文详情",
			isAvailable: isConversationOpen,
			run:         (*tuiModel).runContextCommand,
		},
		{
			name:        "compact",
			description: "压缩上下文",
			isAvailable: isConversationOpen,
			run:         (*tuiModel).runCompactCommand,
		},
		{
			name:         "cd",
			argumentHint: "<目录>",
			description:  "切换工作目录",
			isAvailable:  isAlwaysAvailable,
			run:          (*tuiModel).runCDCommand,
		},
		{
			name:        "executor",
			description: "选择默认 Executor 和可用范围",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runExecutorCommand,
		},
		{
			name:        "expand",
			key:         "ctrl+o",
			description: "展开视图",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runExpandCommand,
		},
		{
			name:        "agents",
			description: "子 Agent",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runAgentsCommand,
		},
		{
			name:        "status",
			description: "会话信息",
			isAvailable: isConversationOpen,
			run:         (*tuiModel).runStatusCommand,
		},
		{
			name:        "mcp",
			description: "MCP 服务",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runMCPCommand,
		},
		{
			name:        "help",
			key:         "?",
			description: "帮助",
			isAvailable: isAlwaysAvailable,
			run:         (*tuiModel).runHelpCommand,
		},
		{
			name:        "quit",
			description: "退出",
			isAvailable: isAlwaysAvailable,
			run:         func(m *tuiModel, _ string) tea.Cmd { return m.quit() },
		},
	}
}

// getUsage 返回命令名称和参数提示组成的调用写法。
func (c command) getUsage() string {
	// 1. 不带参数时只返回斜杠名称，否则追加参数提示。
	if c.argumentHint == "" {
		return "/" + c.name
	}
	return "/" + c.name + " " + c.argumentHint
}

// isAlwaysAvailable 命令现在能用吗？随时可用的命令用它，总是返回能用。
func isAlwaysAvailable(*tuiModel) bool {
	// 1. 总是可用。
	return true
}

// isConversationOpen 当前是否打开了对话？还没发送第一条消息的新对话草稿不算。
func isConversationOpen(m *tuiModel) bool {
	// 1. 有对话记录就算打开。
	return m.conversation != nil
}

// getSlashMatches 返回整段输入匹配的命令候选，以及光标处斜杠词匹配的 Skill 候选。
func (m *tuiModel) getSlashMatches() []command {
	// 1. 面板打开或弹窗已被 Esc 关闭时没有候选。
	if m.activePanel != nil || m.isSlashDismissed {
		return nil
	}
	// 2. 输入是以 / 开头的单行时匹配当前可用的命令：没带参数按名称前缀匹配，带了参数要求名称相同且命令接受参数。
	value := m.composer.getValue()
	var matches []command
	if strings.HasPrefix(value, "/") && !strings.Contains(value, "\n") {
		name, argument := splitSlashCommand(value)
		for _, each := range m.commands {
			isMatched := strings.HasPrefix(each.name, name)
			if argument != "" {
				isMatched = each.argumentHint != "" && each.name == name
			}
			if isMatched && each.isAvailable(m) {
				matches = append(matches, each)
			}
		}
	}
	// 3. 光标处有斜杠词时按前缀匹配 Skill（不分大小写）；Skill 和命令同名、或输入以 skill: 开头时，候选名加 skill: 前缀。
	_, _, prefix, ok := m.composer.getSlashToken()
	if !ok {
		return matches
	}
	prefix = strings.ToLower(prefix)
	for _, each := range m.skills {
		name := each.Name
		explicit := strings.HasPrefix(prefix, "skill:")
		for _, builtin := range m.commands {
			if builtin.name == name {
				explicit = true
				break
			}
		}
		if explicit {
			name = "skill:" + name
		}
		// 插件 skill 名为 "<插件名>:<skill>"，输入 skill 名开头也能补全出来。
		_, nameWithoutPlugin, _ := strings.Cut(each.Name, ":")
		if strings.HasPrefix(strings.ToLower(each.Name), prefix) || strings.HasPrefix(strings.ToLower(name), prefix) ||
			strings.HasPrefix(strings.ToLower(nameWithoutPlugin), prefix) {
			matches = append(matches, command{
				name:        name,
				description: each.Description,
				isSkill:     true,
			})
		}
	}
	return matches
}

// handleSlashPopupKey 有斜杠候选时由弹窗处理按键，返回按键是否已被弹窗处理和要执行的命令。
func (m *tuiModel) handleSlashPopupKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	// 1. 没有候选时按键交回输入框处理。
	matches := m.getSlashMatches()
	if len(matches) == 0 {
		return false, nil
	}
	// 2. 选中的是 Skill 时，Tab 和 Enter 都把光标处的词换成 "/<skill> "。
	cursor := min(m.slashCursor, len(matches)-1)
	if matches[cursor].isSkill && (msg.String() == "tab" || msg.String() == "enter") {
		start, end, _, _ := m.composer.getSlashToken()
		m.composer.replaceSlashToken(start, end, "/"+matches[cursor].name+" ")
		return true, nil
	}
	// 3. ↑ ↓ 移动选中项，Tab 补全命令名并保留参数，Enter 清空输入框后执行命令，Esc 关闭弹窗并保留草稿；其他按键交回输入框。
	switch msg.String() {
	case "up":
		m.slashCursor = (cursor - 1 + len(matches)) % len(matches)
	case "down":
		m.slashCursor = (cursor + 1) % len(matches)
	case "tab":
		completed := "/" + matches[cursor].name
		if matches[cursor].argumentHint != "" {
			_, argument := splitSlashCommand(m.composer.getValue())
			completed += " " + argument
		}
		m.composer.setValue(completed)
	case "enter":
		_, argument := splitSlashCommand(m.composer.getValue())
		m.composer.clear()
		return true, matches[cursor].run(m, argument)
	case "esc":
		m.isSlashDismissed = true
	default:
		return false, nil
	}
	return true, nil
}

// getPopupLines 返回选中项附近的斜杠候选显示行。
func (m *tuiModel) getPopupLines(matches []command, rowLimit int) []string {
	// 1. 没有候选或没有可用行时不显示；按最长的命令写法算名称列宽。
	if len(matches) == 0 || rowLimit <= 0 {
		return nil
	}
	cursor := min(m.slashCursor, len(matches)-1)
	nameWidth := 0
	for _, each := range matches {
		nameWidth = max(nameWidth, ansi.StringWidth(each.getUsage()))
	}
	// 2. 截取最多 rowLimit 行并让选中项可见，每行写命令写法、类别和说明；选中行整行高亮。
	start := max(0, cursor-rowLimit+1)
	var lines []string
	for index := start; index < min(len(matches), start+rowLimit); index++ {
		name := padRight(matches[index].getUsage(), nameWidth) + "  "
		kind := "命令"
		if matches[index].isSkill {
			kind = "Skill"
		}
		description := kind + " · " + strings.ReplaceAll(sanitizeToolText(matches[index].description), "\n", " ")
		if index == cursor {
			line := truncateLine(name+description, m.width-2)
			lines = append(lines, fillBackground(m.styles.selection.Render("› "+line), m.width, m.styles.selection))
			continue
		}
		// 未选中行的名称使用正常色，说明使用暗色。
		line := truncateLine(name+m.styles.faint.Render(description), m.width-2)
		lines = append(lines, fillBackground("  "+line, m.width, m.styles.surface))
	}
	return lines
}

// getHelpLines 返回命令和全局按键的帮助内容。
func (m *tuiModel) getHelpLines() []string {
	// 1. 列出命令、快捷键和说明。
	lines := []string{"命令"}
	for _, each := range m.commands {
		name := each.getUsage()
		if each.key != "" {
			name += "（" + m.styles.hintKey.Render(each.key) + "）"
		}
		lines = append(lines, "  "+padRight(name, 22)+m.styles.faint.Render(each.description))
	}
	// 2. 终端能区分 Shift+Enter 时换行提示加上 Shift+Enter，再列出全局按键。
	newline := "Ctrl+J"
	if m.supportsKeyDisambiguation {
		newline = "Shift+Enter / Ctrl+J"
	}
	lines = append(lines, "", "按键")
	for _, binding := range []keyHint{
		{
			key:    "Enter",
			action: "发送；运行中插入到下一步（steer）",
		},
		{
			key:    newline,
			action: "换行",
		},
		{
			key:    "Tab",
			action: "运行中排队，当前 Turn 结束后执行",
		},
		{
			key:    "Esc Esc",
			action: "中断当前 Turn；空闲时回退到之前的消息",
		},
		{
			key:    "Ctrl+C",
			action: "清空输入；运行中中断；空闲时按两次退出",
		},
		{
			key:    "Ctrl+D",
			action: "输入框为空时退出",
		},
		{
			key:    "↑ / ↓",
			action: "输入历史",
		},
	} {
		lines = append(lines, "  "+padRight(m.styles.hintKey.Render(binding.key), 22)+m.styles.faint.Render(binding.action))
	}
	return lines
}

// runNewCommand 回到草稿并打印新对话标题。
func (m *tuiModel) runNewCommand(string) tea.Cmd {
	// 1. 新对话正在创建时显示等待提示。
	if m.isOpeningDraft {
		return m.showNotice("正在新建对话，请稍候", false, noticeDuration)
	}
	// 2. 关闭面板，换一个导航序号让还没返回的新建、打开对话结果作废；草稿沿用当前对话的 Executor 设置。
	m.closePanel()
	m.navigationSeq = m.nextRequestSeq()
	if m.conversation != nil {
		m.draftExecutorSettings = cloneExecutorSettings(m.conversation.ExecutorSettings)
	}
	// 3. 清除旧对话、草稿模型和历史加载标记，打印新对话标题并重新读取 Skill；对话等第一次发送时才创建。
	m.openFailedConversation = nil
	m.setConversation(nil)
	m.draftModel = defs.ModelSelection{}
	m.shouldLoadHistory = false
	m.print(formatBlock([]string{m.formatHeader("新对话")})...)
	return m.loadSkills()
}

// runResumeCommand 打开恢复对话面板并读取列表。
func (m *tuiModel) runResumeCommand(string) tea.Cmd {
	// 1. 创建面板，标记加载中并发出对话列表请求。
	resume := &resumePanel{}
	resume.fetch.start()
	return m.openPanel(resume, resume.getFetch(m))
}

// runForkCommand 分叉当前对话的全部轮次并打开新对话。
func (m *tuiModel) runForkCommand(string) tea.Cmd {
	// 1. 发出不指定截断轮次的分叉请求，复制全部轮次，原对话保留。
	return m.forkConversation("", "", "已分叉到新对话，原对话保留")
}

// runRewindCommand 打开回退面板。
func (m *tuiModel) runRewindCommand(string) tea.Cmd {
	// 1. 列出可回退的输入。
	return m.openRewindPanel()
}

// openRewindPanel 打开回退面板，列出各轮的开场输入，最新的在最上面。
func (m *tuiModel) openRewindPanel() tea.Cmd {
	// 1. 读取各轮开场输入，倒序后打开面板。
	turns := m.transcript.getTurnOpeningInputs()
	slices.Reverse(turns)
	return m.openPanel(&rewindPanel{turns: turns}, nil)
}

// runStatusCommand 打开会话信息面板并读取详情。
func (m *tuiModel) runStatusCommand(string) tea.Cmd {
	// 1. 创建面板，标记加载中并发出请求。
	status := &statusPanel{}
	status.fetch.start()
	return m.openPanel(status, status.getFetch(m))
}

// runCompactCommand 请求压缩当前对话根 Session 的上下文。
func (m *tuiModel) runCompactCommand(string) tea.Cmd {
	// 1. 返回后台命令：调用 CompactContext，把请求结果包成 compactRequestedMsg；压缩出的摘要从事件流到达。
	client, ctx, conversationID := m.opt.Client, m.ctx, m.conversation.ID
	return func() tea.Msg {
		_, err := client.CompactContext(ctx, conversationID)
		return compactRequestedMsg{err: err}
	}
}

// runReloadCommand 请求服务重新加载配置并返回结果消息。
func (m *tuiModel) runReloadCommand(string) tea.Cmd {
	// 1. 返回后台命令：调用 ReloadConfig，把结果和错误包成 configReloadedMsg。
	client, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		result, err := client.ReloadConfig(ctx)
		return configReloadedMsg{
			result: result,
			err:    err,
		}
	}
}

// handleConfigReloaded 处理配置重载结果。
func (m *tuiModel) handleConfigReloaded(msg configReloadedMsg) tea.Cmd {
	// 1. 重载失败时显示服务端错误。
	if msg.err != nil {
		return m.showNotice("重新加载失败："+getServerErrorText(msg.err), true, noticeDuration)
	}
	// 2. 提示已重新加载，有正在执行的对话时附上数量，同时重新读取模型列表。
	notice := m.showNotice("已重新加载"+formatBusyConversations(msg.result.BusyConversationIDs), false, noticeDuration)
	return tea.Batch(m.loadModelList(), notice)
}

// runHelpCommand 打开帮助面板。
func (m *tuiModel) runHelpCommand(string) tea.Cmd {
	// 1. 使用命令表和全局按键显示帮助。
	return m.openPanel(&helpPanel{}, nil)
}

// runCDCommand 显示当前位置，或请求解析并切换工作目录。
func (m *tuiModel) runCDCommand(argument string) tea.Cmd {
	// 1. 读取当前对话或草稿的设置和默认工作位置；没有参数时显示当前位置和用法。
	conversationID, settings := m.getEditableExecutorSettings()
	executorID, directory := getDefaultExecutorLocation(settings, m.executors)
	if argument == "" {
		return m.showNotice("当前位置："+formatWorkLocation(executorID, directory, m.isLocalServer)+"；用法：/cd <目录>", false, noticeDuration)
	}
	// 2. 以默认 Executor 的当前目录为基准生成解析请求，复制一份设置待修改。
	client, ctx := m.opt.Client, m.ctx
	request := &runtimeclient.ResolveWorkspaceDirectoryRequest{
		ExecutorID:    executorID,
		Directory:     argument,
		BaseDirectory: directory,
	}
	settings = cloneExecutorSettings(settings)
	// 3. 请求 Executor 解析目录，返回请求发出时的对话和设置。
	return func() tea.Msg {
		resolved, err := client.ResolveWorkspaceDirectory(ctx, request)
		return workspaceDirectoryResolvedMsg{
			conversationID: conversationID,
			settings:       settings,
			executorID:     executorID,
			directory:      resolved,
			err:            err,
		}
	}
}

// handleWorkspaceDirectoryResolved 处理工作目录解析结果。
func (m *tuiModel) handleWorkspaceDirectoryResolved(msg workspaceDirectoryResolvedMsg) tea.Cmd {
	// 1. 解析失败时显示错误并保留设置。
	if msg.err != nil {
		return m.showNotice("切换目录失败："+msg.err.Error(), true, noticeDuration)
	}
	// 2. 把目录写入请求发出时的设置，再提交到对应对话或草稿。
	settings := msg.settings
	if settings.WorkspaceDirectories == nil {
		settings.WorkspaceDirectories = make(map[string]string, 1)
	}
	settings.WorkspaceDirectories[msg.executorID] = msg.directory
	return m.setExecutorSettings(msg.conversationID, settings)
}
