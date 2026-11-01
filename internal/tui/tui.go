// Package tui 是 OpenVaala 的终端界面：inline 模式，已定稿的内容按顺序写进终端 scrollback，横向缩放时按新宽度重建，
// 底部实时区域每帧重绘尾部或面板、状态行、排队预览、输入框与 footer；只通过 runtimeclient 与 Runtime 交互。
package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"io"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/charmbracelet/x/ansi"
)

// TUI 是 OpenVaala 的终端界面。
type TUI interface {
	// Run 运行界面直到用户退出、Client 事件流关闭或 ctx 结束；启动时处于草稿状态，第一次发送输入时才新建并打开对话。
	// Client 由调用方运行与停止。
	Run(ctx context.Context) error
}

// tuiImpl 保存构造时校验过的界面配置，每次 Run 用它新建一个 bubbletea 程序。
type tuiImpl struct {
	opt TUIOpt
}

// TUIOpt 保存界面依赖，构造后只读。
type TUIOpt struct {
	Client runtimeclient.Client
	// AgentName 是新建对话使用的根 Agent。
	AgentName string
	Input     io.Reader
	Output    io.Writer
	// MarkdownStyle 是 glamour 样式：markdownStyleAuto 在 Run 启动界面前按终端背景选定（输入输出不是终端时不带颜色），
	// markdownStyleNoTTY 不带颜色。
	MarkdownStyle string
	// WorkspaceDirectory 是 TUI 的 cwd（绝对路径），命中 client.path_mappings 时是调用方换成的 serve 路径：第一次连上本机 serve
	// （或 IsWorkspaceDirectoryMapped）时作为草稿设置的 local 目录，新建对话和草稿 Skills 带上它；打开对话不带目录。
	WorkspaceDirectory string
	// IsWorkspaceDirectoryMapped WorkspaceDirectory 是不是按 client.path_mappings 换成的 serve 路径？是时第一次连上不管本机还是远端
	// serve 都用它；否则只有本机 serve 用它，远端 serve 不带目录。路径由调用方换算好再传进来。
	IsWorkspaceDirectoryMapped bool
	// LocalServerID 是本机 runtime-server.yaml 里的 server_id；连上的 serve 返回同一个 server_id 时才是本机 serve。
	// 为空时连上的 serve 一律按远端处理。
	LocalServerID string
	// ServerURL 是状态面板显示的服务地址，由调用方脱敏；为空时显示 "—"。
	ServerURL string
	// ServerSource 是服务地址的来源（--url、client.url 或 local），状态面板在地址后面用全角括号显示；为空时不显示。
	ServerSource string
	// ConnectURL 是状态面板按 t 显示的、带 token 的可直连地址，供外部客户端直接使用；为空时无法切换。
	ConnectURL string
	// StartupNotice 是启动后在状态行显示 10 秒的提示，按键后提前消失；为空时不显示。
	StartupNotice string
}

// NewTUIOpt 创建终端界面配置。
func NewTUIOpt(client runtimeclient.Client, agentName, workspaceDirectory string, input io.Reader, output io.Writer) *TUIOpt {
	// 1. 保存 Client、Agent、工作目录和输入输出，样式设为 auto。
	return &TUIOpt{
		Client:             client,
		AgentName:          agentName,
		Input:              input,
		Output:             output,
		MarkdownStyle:      markdownStyleAuto,
		WorkspaceDirectory: workspaceDirectory,
	}
}

// NewTUI 校验界面依赖和样式，返回终端界面。
func NewTUI(opt *TUIOpt) (TUI, error) {
	// 1. 检查 Client、Agent 和输入输出是否齐全。
	if opt == nil || opt.Client == nil || opt.AgentName == "" || opt.Input == nil || opt.Output == nil {
		return nil, errors.New("tui requires a client, agent name, input and output")
	}
	// 2. 校验 Markdown 样式并保存配置副本。
	if !isMarkdownStyleValid(opt.MarkdownStyle) {
		return nil, fmt.Errorf("invalid markdown style %q", opt.MarkdownStyle)
	}
	return &tuiImpl{opt: *opt}, nil
}

// Run 解析终端外观并运行界面。
func (t *tuiImpl) Run(ctx context.Context) error {
	// 1. 读取终端外观，创建界面状态和 bubbletea 程序。
	opt := t.opt
	markdownStyle, background := resolveTerminalAppearance(opt.MarkdownStyle, opt.Input, opt.Output)
	opt.MarkdownStyle = markdownStyle
	program := tea.NewProgram(newTUIModel(ctx, &opt, background), tea.WithContext(ctx), tea.WithInput(opt.Input), tea.WithOutput(opt.Output))
	// 2. 运行事件循环；上下文结束时正常返回，其他程序错误原样返回，正常退出时返回重建画面时记下的终端错误。
	model, err := program.Run()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return model.(*tuiModel).terminalError
}

// tuiModel 是唯一的 bubbletea Model：负责按键路由、行数预算、临时通知与对话导航；输入框等状态由各自的组件拥有。
// 只在 bubbletea 的 Update 与 View 中使用，不是并发安全的。
type tuiModel struct {
	ctx    context.Context
	opt    *TUIOpt
	now    func() time.Time
	width  int
	height int
	styles tuiStyles
	// hasWindowSize 是否收到过终端尺寸？首次尺寸通知只初始化布局，之后横向缩放才重建 scrollback。
	hasWindowSize bool

	connection runtimeclient.ConnectionChanged
	// isLocalServer 当前连上的是否本机 serve？每次连上时按 ConnectionChanged.ServerID 与 TUIOpt.LocalServerID 比较。
	isLocalServer bool
	// hasConnected 是否连上过 serve？第一次连上时初始化草稿设置，之后重连不再重置。
	hasConnected bool
	// executors 是 executor/list 的结果，每次连上时拉取，/executor 面板打开时刷新；面板、footer 用它。拉取失败时清空。
	executors []conversation.ExecutorDetails
	// supportsKeyDisambiguation 终端是否支持键盘增强（能区分 Shift+Enter）？帮助面板据此决定提示哪个换行键。
	supportsKeyDisambiguation bool

	// conversation 是当前对话；nil 表示草稿（启动后或 /new 之后），第一次发送时才新建。它的 ExecutorSettings 随 ConversationSynced
	// 和改设置的结果更新。
	conversation *defs.Conversation
	// draftExecutorSettings 是还没新建的对话要用的 Executor 设置：第一次连上本机 serve 时 local 目录是 TUI 的 cwd，连上远端时为空；
	// /new 时沿用当前对话的设置；草稿里的 /cd、/executor 只改它。
	draftExecutorSettings defs.ExecutorSettings
	// openFailedConversation 是打开失败的对话（默认 Executor 不可用等）：没有打开对话时 /executor、/cd 作用于它，改成功后按新设置打开；
	// 新建或打开别的对话、/new 时清空。
	openFailedConversation *defs.Conversation
	// requestSeq 是异步请求的单调序号。
	requestSeq int
	// navigationSeq 是最近一次新建、打开或分叉对话请求的序号，结果序号不同时丢弃。
	navigationSeq int
	// isOpeningDraft 草稿的输入是否正在新建、打开对话并依次提交？期间按 Enter 的输入进入待发送队列。
	isOpeningDraft bool

	composer *composer
	// renderer 按当前宽度渲染 Markdown，宽度变化时重建。
	renderer *markdownRenderer
	// transcript 属于当前对话，切换对话时重建。
	transcript *transcript
	// skills 只保存当前 Agent 可用的 Skill 目录元数据。
	skills []skill.SkillInfo
	// skillListSeq 是最近一次 Skill 目录查询的序号，切换对话前发出的查询结果按它丢弃。
	skillListSeq int
	// commands 是静态注册的命令表。
	commands []command
	// slashCursor 是 slash 弹窗中选中的命令。
	slashCursor int
	// isSlashDismissed slash 弹窗是否被 Esc 关掉了？输入框内容或光标变化后重新打开。
	isSlashDismissed bool
	// activePanel 是打开的面板。
	activePanel panel
	// panelRequestSeq 是面板最近一次拉取的序号，结果序号不同时丢弃。
	panelRequestSeq int
	// expandView 是打开的展开视图；非 nil 时界面进入全屏，按键只交给它，定稿行暂存到关闭后写出。
	expandView *expandView
	// isLeavingExpandView 展开视图是否已关闭、但终端还没退出全屏？期间定稿行继续暂存，收到 expandViewLeftMsg 后写出。
	isLeavingExpandView bool
	// shouldLoadHistory 同步完成后是否要用这个对话已保存的用户输入初始化输入历史？只在 /resume 打开已有对话时为 true。
	shouldLoadHistory bool
	// sessionStatuses、queuedInputs 属于当前对话，切换对话时重建；草稿时是空的。
	sessionStatuses *sessionStatuses
	queuedInputs    *queuedInputs
	// modelDisplayNames 是 model/list 给出的模型显示名；只在原地更新，transcript 持有同一个 map 只读。
	modelDisplayNames map[defs.ModelRef]string
	// agentDefaultModel 是 model/list 给出的新建对话所用 Agent 的默认模型。
	agentDefaultModel defs.ModelRef
	// draftModel 是草稿里在 /model 选择、新建对话时带上的模型，零值表示用 Agent 默认模型。
	draftModel defs.ModelSelection
	// isModelListEmpty 最近一次拉到的 model/list 是否没有任何模型？是时状态行一直提示用 /provider 添加。
	isModelListEmpty bool
	// hasDismissedProviderOnboarding 本次运行里用户是否按 Esc 关掉过 /provider 面板？关掉后再连上没有模型的 serve 也不自动打开引导。
	hasDismissedProviderOnboarding bool
	// modelSetSeq 是最近一次 setSessionModel 请求的序号，结果序号不同时丢弃。
	modelSetSeq int
	// spinner 是运行中的转圈。
	spinner spinner.Model
	// isSpinnerTicking 转圈是否在转？有运行中的 Session 时开始，没有时停下。
	isSpinnerTicking bool

	// notice 是状态行上当前的临时通知。
	notice notice
	// noticeSeq 是最近一次通知的编号。
	noticeSeq int
	// confirmKey 是等待第二次按下的确认标识，Esc 中断、Esc 回退、Ctrl+C 退出各用自己的标识。
	confirmKey string
	// confirmDeadline 是第二次按下的截止时间。
	confirmDeadline time.Time
	// pendingPrints 是本次 Update 积累的定稿行，由 Update 统一写进 scrollback。
	pendingPrints []string
	// isPrintingScrollback 是否有一批定稿输出或画面重建还没完成？期间新的定稿行和重建都等它完成。
	isPrintingScrollback bool
	// shouldRebuildScrollback 是否有横向缩放还没重建？展开视图内暂存，返回 inline 后处理。
	shouldRebuildScrollback bool
	// terminalError 是重建画面时写终端失败的错误，记下后界面退出，Run 返回它。
	terminalError error
	// lastLiveHeight 是 View 最近一次返回的 inline 实时区域行数，flushPrints 据此分批。
	// 展开视图（全屏）与退出时的空画面不记，它们不是写定稿行时 bubbletea 用到的 inline 帧。
	lastLiveHeight int
	// liveShrunkAt 是 inline 实时区域最近一次变矮的时刻，flushPrints 据此暂存定稿行。
	liveShrunkAt time.Time
	// isWaitingLiveFrame 定稿行是否在等变矮的实时区域画到终端上？期间定稿行继续暂存，收到 liveFrameSettledMsg 后写出。
	isWaitingLiveFrame bool
	// isQuitting 界面是否正在退出？
	isQuitting bool
}

const (
	defaultWidth   = 80
	defaultHeight  = 24
	noticeDuration = 5 * time.Second
	// startupNoticeDuration 是启动提示 TUIOpt.StartupNotice 在状态行停留的时长。
	startupNoticeDuration = 10 * time.Second
	confirmWindow         = time.Second
	// expandViewLeaveDelay 是关闭展开视图后等待终端退出全屏的时长，期间暂存定稿行。
	expandViewLeaveDelay = 100 * time.Millisecond
	// liveFrameSettleDelay 是实时区域缩短后等待新帧显示的时长，期间暂存定稿行。
	liveFrameSettleDelay = 50 * time.Millisecond
)

// newTUIModel 创建终端界面的初始状态。
func newTUIModel(ctx context.Context, opt *TUIOpt, background color.Color) *tuiModel {
	// 1. 按背景色建立样式，初始化输入框、命令、渲染器和转圈动画。
	styles := newTUIStyles(opt.MarkdownStyle, background)
	m := &tuiModel{
		ctx:               ctx,
		opt:               opt,
		now:               time.Now,
		width:             defaultWidth,
		height:            defaultHeight,
		styles:            styles,
		connection:        runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionConnecting},
		commands:          newCommands(),
		composer:          newComposer(styles),
		renderer:          mustNewMarkdownRenderer(opt.MarkdownStyle, defaultWidth),
		modelDisplayNames: make(map[defs.ModelRef]string),
		spinner: spinner.New(spinner.WithSpinner(spinner.Spinner{
			Frames: []string{"◐", "◓", "◑", "◒"},
			FPS:    time.Second / 8,
		}), spinner.WithStyle(getStateStyle(stateRunning, styles))),
	}
	// 2. 设置输入框尺寸并进入草稿状态。
	m.composer.setSize(m.width, getComposerMaxHeight(m.height))
	m.setConversation(nil)
	return m
}

// Init 启动输入焦点、Client 事件读取和 Skill 目录查询。
func (m *tuiModel) Init() tea.Cmd {
	// 1. 收集初始化命令，有启动通知时同时安排通知计时。
	cmds := []tea.Cmd{m.composer.focus(), m.waitClientEvent(), m.loadSkills()}
	if m.opt.StartupNotice != "" {
		cmds = append(cmds, m.showNotice(m.opt.StartupNotice, false, startupNoticeDuration))
	}
	return tea.Batch(cmds...)
}

// Update 处理界面消息并安排定稿内容输出。
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// 1. 处理消息，先写出本次累积的历史行，再运行后续命令。
	cmd := m.update(msg)
	return m, m.flushPrints(cmd)
}

// update 按消息类型更新界面状态并安排后续命令。
func (m *tuiModel) update(msg tea.Msg) tea.Cmd {
	// 1. 分发尺寸、按键、粘贴、Client 事件和异步请求结果。
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.handleWindowSize(msg)
	case tea.KeyboardEnhancementsMsg:
		m.supportsKeyDisambiguation = msg.SupportsKeyDisambiguation()
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		return m.handlePaste(msg.Content)
	case clientEventMsg:
		return tea.Batch(m.handleClientEvent(msg.event), m.waitClientEvent())
	case clientEventsClosedMsg:
		return m.quit()
	case conversationCreatedMsg:
		return m.handleConversationCreated(msg)
	case conversationOpenedMsg:
		return m.handleConversationOpened(msg)
	case conversationForkedMsg:
		return m.handleConversationForked(msg)
	case inputSubmittedMsg:
		return m.handleInputSubmitted(msg)
	case modelListLoadedMsg:
		return m.handleModelListLoaded(msg)
	case configReloadedMsg:
		return m.handleConfigReloaded(msg)
	case skillListLoadedMsg:
		if msg.seq != m.skillListSeq {
			return nil
		}
		if msg.err != nil {
			return m.showNotice("加载 Skill 失败："+msg.err.Error(), true, noticeDuration)
		}
		m.skills = msg.skills
		m.slashCursor = 0
	case modelSetMsg:
		return m.handleModelSet(msg)
	case executorsLoadedMsg:
		m.handleExecutorsLoaded(msg)
	case workspaceDirectoryResolvedMsg:
		return m.handleWorkspaceDirectoryResolved(msg)
	case executorSettingsSetMsg:
		return m.handleExecutorSettingsSet(msg)
	case panelResultMsg:
		return m.handlePanelResult(msg)
	case spinner.TickMsg:
		return m.handleSpinnerTick(msg)
	case turnInterruptedMsg:
		return m.handleTurnInterrupted(msg)
	case compactRequestedMsg:
		return m.handleCompactRequested(msg)
	case noticeExpiredMsg:
		m.handleNoticeExpired(msg)
	case expandViewLeftMsg:
		return m.handleExpandViewLeft()
	case liveFrameSettledMsg:
		m.isWaitingLiveFrame = false
	case scrollbackPrintedMsg:
		m.isPrintingScrollback = false
		if msg.err != nil {
			m.terminalError = fmt.Errorf("rebuild terminal scrollback: %w", msg.err)
			m.isQuitting = true
			return tea.Quit
		}
		if m.isQuitting {
			return tea.Quit
		}
	}
	return nil
}

// View 按行数预算绘制实时内容、输入框和 footer。
func (m *tuiModel) View() tea.View {
	// 1. 退出时返回空画面，展开视图打开时返回全屏内容。
	if m.isQuitting {
		return tea.NewView("")
	}
	if m.expandView != nil {
		view := tea.NewView(strings.Join(m.expandView.render(m.getExpandedLines(), m.width, m.height, m.styles), "\n"))
		view.AltScreen = true
		return view
	}
	// 2. 计算尾部、面板、状态、队列、弹窗和输入块的行数预算。
	matches := m.getSlashMatches()
	body := m.transcript.getLiveLines(m.spinner.View())
	wantedBody := len(body)
	if m.activePanel != nil {
		wantedBody = m.height * panelHeightPercent / 100
	}
	status := m.getStatusLines()
	queue := m.queuedInputs.getPreviewLines(m.width, m.styles)
	composerPadding := 0
	if m.styles.hasBackground {
		composerPadding = 1
	}
	layout := getLiveLayout(m.height, liveLayout{
		body:            wantedBody,
		status:          len(status),
		queue:           len(queue),
		popup:           min(len(matches), slashPopupRowLimit),
		composer:        m.composer.getHeight(),
		composerPadding: composerPadding,
		footer:          1,
	})
	if m.activePanel != nil {
		body = m.activePanel.render(m, m.width, layout.body)
	}
	var lines []string
	lines = append(lines, body[max(0, len(body)-layout.body):]...)
	lines = append(lines, status[:layout.status]...)
	lines = append(lines, queue[:layout.queue]...)
	lines = append(lines, m.getPopupLines(matches, layout.popup)...)
	// 3. 输入框上下铺底色空行，footer 放在输入块下方。
	padding := slices.Repeat([]string{fillBackground("", m.width, m.styles.surface)}, layout.composerPadding)
	lines = append(lines, padding...)
	composerTop := len(lines)
	composerLines := strings.Split(m.composer.view(), "\n")
	lines = append(lines, composerLines[:min(len(composerLines), layout.composer)]...)
	lines = append(lines, padding...)
	if layout.footer > 0 {
		lines = append(lines, m.getFooterLine())
	}
	// 4. 记录实时帧缩短时间和高度，将光标移到输入框所在行。
	if len(lines) < m.lastLiveHeight {
		m.liveShrunkAt = m.now()
	}
	m.lastLiveHeight = len(lines)
	view := tea.NewView(strings.Join(lines, "\n"))
	if cursor := m.composer.getCursor(); cursor != nil {
		cursor.Y += composerTop
		view.Cursor = cursor
	}
	return view
}

// getLiveLayout 按终端高度分配各部分可见行数。
func getLiveLayout(height int, wanted liveLayout) liveLayout {
	// 1. 高度不足两行只留输入框，不足五行只留输入框和 footer。
	if height < 2 {
		return liveLayout{composer: 1}
	}
	if height < 5 {
		return liveLayout{
			composer: 1,
			footer:   1,
		}
	}
	// 2. 先分配输入块和 footer，剩余高度依次分给状态、队列、弹窗和正文。
	layout := liveLayout{
		footer:          1,
		composer:        min(max(wanted.composer, 1), getComposerMaxHeight(height)),
		composerPadding: wanted.composerPadding,
	}
	remaining := height - layout.footer - layout.composer - 2*layout.composerPadding
	layout.status, remaining = takeLines(wanted.status, remaining)
	layout.queue, remaining = takeLines(wanted.queue, remaining)
	layout.popup, remaining = takeLines(wanted.popup, remaining)
	layout.body, _ = takeLines(wanted.body, remaining)
	return layout
}

// getComposerMaxHeight 返回输入框可见行数上限。
func getComposerMaxHeight(height int) int {
	// 1. 高度不足五行时返回一行，否则取终端高度三分之一，限制在一到十行。
	if height < 5 {
		return 1
	}
	return max(1, min(10, height/3))
}

// takeLines 从剩余高度分配至多 wanted 行。
func takeLines(wanted, remaining int) (taken, rest int) {
	// 1. 取需求和剩余高度的较小值，返回分配行数与剩余行数。
	taken = min(wanted, remaining)
	return taken, remaining - taken
}

// handleWindowSize 记录终端尺寸并刷新输入框和 Markdown 宽度。
func (m *tuiModel) handleWindowSize(msg tea.WindowSizeMsg) {
	// 1. 将宽高限制为正数，已有尺寸后的宽度变化登记为待重建历史。
	width, height := max(msg.Width, 1), max(msg.Height, 1)
	if m.hasWindowSize && width != m.width {
		m.shouldRebuildScrollback = true
	}
	// 2. 换用新宽度的渲染器，更新组件尺寸和实时尾部。
	m.hasWindowSize = true
	if width != m.renderer.width {
		m.renderer = mustNewMarkdownRenderer(m.opt.MarkdownStyle, width)
	}
	m.width, m.height = width, height
	m.composer.setSize(m.width, getComposerMaxHeight(m.height))
	m.transcript.setRenderer(m.renderer, m.height)
}

// handleKey 清除临时通知并处理按键。
func (m *tuiModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	// 1. 记下输入内容与光标位置，再路由按键。
	m.notice = notice{}
	before := m.composer.getValue()
	row, column := m.composer.textarea.Line(), m.composer.textarea.Column()
	cmd := m.routeKey(msg)
	// 2. 内容或光标变化后重置 slash 选中项并重新允许弹窗。
	if m.composer.getValue() != before || row != m.composer.textarea.Line() || column != m.composer.textarea.Column() {
		m.slashCursor, m.isSlashDismissed = 0, false
	}
	return cmd
}

// routeKey 将按键交给展开视图、弹窗、面板、输入框或全局处理。
func (m *tuiModel) routeKey(msg tea.KeyPressMsg) tea.Cmd {
	// 1. 展开视图独占按键；其余按 slash 弹窗、面板、输入框和全局键的顺序处理，面板打开时它不处理的键直接交给全局键。
	if m.expandView != nil {
		if m.expandView.handleKey(msg.String(), m.height) {
			return m.closeExpandView()
		}
		return nil
	}
	if handled, cmd := m.handleSlashPopupKey(msg); handled {
		return cmd
	}
	if m.activePanel != nil {
		result, cmd := m.activePanel.handleKey(m, msg)
		switch result {
		case panelKeyHandled:
			return cmd
		case panelKeyClosed:
			m.closePanel()
			return cmd
		}
		return m.handleGlobalKey(msg)
	}
	if handled, cmd := m.handleComposerKey(msg); handled {
		return cmd
	}
	return m.handleGlobalKey(msg)
}

// handleComposerKey 处理发送、排队和编辑输入的按键，返回按键是否已处理。
func (m *tuiModel) handleComposerKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	// 1. Enter 按运行状态提交；Tab 只在根 Agent 运行且输入非空时按 normal 提交，其余情况吞掉；输入为空时 Ctrl+D、? 交给上层，
	// Ctrl+C、Esc、Ctrl+O 总是交给上层；其余按键交给输入框编辑。
	switch msg.String() {
	case "enter":
		return true, m.submitComposer(m.getEnterMode())
	case "tab":
		if m.sessionStatuses.isRootRunning() && !m.composer.isBlank() {
			return true, m.submitComposer(session.SessionInputModeNormal)
		}
		return true, nil
	case "ctrl+d", "?":
		if m.composer.isEmpty() {
			return false, nil
		}
	case "ctrl+c", "esc", "ctrl+o":
		return false, nil
	}
	return true, m.composer.handleKey(msg)
}

// handleGlobalKey 处理退出、中断、回退和全局快捷键。
func (m *tuiModel) handleGlobalKey(msg tea.KeyPressMsg) tea.Cmd {
	// 1. Esc 双按确认中断或回退；Ctrl+C 先清空输入框，根 Agent 运行时直接中断，否则双按确认退出；空输入的 Ctrl+D 退出。
	key := msg.String()
	switch key {
	case "esc":
		if m.sessionStatuses.isRootRunning() {
			return m.confirmThen("esc", "再按 Esc 中断", m.interruptTurn)
		}
		if m.composer.isEmpty() && isConversationOpen(m) {
			return m.confirmThen("esc-rewind", "再按 Esc 回退", m.openRewindPanel)
		}
		return nil
	case "ctrl+c":
		// 面板打开时同样先清空输入框，面板保持打开。
		if !m.composer.isEmpty() {
			m.composer.clear()
			return nil
		}
		if m.sessionStatuses.isRootRunning() {
			return m.interruptTurn()
		}
		return m.confirmThen("ctrl+c", "再按 Ctrl+C 退出", m.quit)
	case "ctrl+d":
		if m.composer.isEmpty() {
			return m.quit()
		}
		return nil
	}
	// 2. 其他按键执行命令表中当前可用的快捷命令。
	for _, each := range m.commands {
		if each.key == key && each.isAvailable(m) {
			return each.run(m, "")
		}
	}
	return nil
}

// handlePaste 把粘贴文字交给面板或输入框。
func (m *tuiModel) handlePaste(content string) tea.Cmd {
	// 1. 展开视图丢弃粘贴；接收粘贴的面板优先处理，没有面板时写入输入框。
	if m.expandView != nil {
		return nil
	}
	if receiver, ok := m.activePanel.(pastePanel); ok {
		return receiver.handlePaste(m, content)
	}
	if m.activePanel == nil {
		m.composer.handlePaste(content)
		m.slashCursor, m.isSlashDismissed = 0, false
	}
	return nil
}

// confirmThen 在确认时间内第二次触发同一动作时执行。
func (m *tuiModel) confirmThen(key, hint string, action func() tea.Cmd) tea.Cmd {
	// 1. 同一标识在截止前再次触发时清除标识并执行动作。
	now := m.now()
	if m.confirmKey == key && now.Before(m.confirmDeadline) {
		m.confirmKey = ""
		return action()
	}
	// 2. 首次触发或超时后重新记录确认标识和截止时间，并显示提示。
	m.confirmKey, m.confirmDeadline = key, now.Add(confirmWindow)
	return m.showNotice(hint, false, confirmWindow)
}

// showNotice 显示临时状态通知并安排过期消息。
func (m *tuiModel) showNotice(text string, isError bool, duration time.Duration) tea.Cmd {
	// 1. 递增通知编号，保存文字和错误状态，再安排到期计时。
	m.noticeSeq++
	id := m.noticeSeq
	m.notice = notice{
		id:      id,
		text:    text,
		isError: isError,
	}
	return tea.Tick(duration, func(time.Time) tea.Msg { return noticeExpiredMsg{id: id} })
}

// handleNoticeExpired 清除已到期且未被替换的通知。
func (m *tuiModel) handleNoticeExpired(msg noticeExpiredMsg) {
	// 1. 到期的编号就是当前通知时清空通知。
	if m.notice.id == msg.id {
		m.notice = notice{}
	}
}

// waitClientEvent 返回读取下一条 Client 事件的命令。
func (m *tuiModel) waitClientEvent() tea.Cmd {
	// 1. 取得事件流，读取一条事件；流关闭时发送界面退出消息。
	events := m.opt.Client.Events()
	return func() tea.Msg {
		event, open := <-events
		if !open {
			return clientEventsClosedMsg{}
		}
		return clientEventMsg{event: event}
	}
}

// handleClientEvent 更新连接状态，分发当前对话的 Client 事件。
func (m *tuiModel) handleClientEvent(event runtimeclient.ClientEvent) tea.Cmd {
	// 1. 连接事件更新连接状态，连上后查询 Executor 与模型，首次连上时初始化草稿目录。
	if changed, ok := event.(*runtimeclient.ConnectionChanged); ok {
		m.connection = *changed
		if changed.State == runtimeclient.ConnectionConnected {
			return m.handleConnected(changed.ServerID)
		}
		return nil
	}
	// 2. 仅分发当前对话的事件，更新历史、运行状态和输入队列。
	if m.conversation == nil || getEventConversationID(event) != m.conversation.ID {
		return nil
	}
	switch event := event.(type) {
	case *runtimeclient.ConversationTitleChanged:
		m.handleConversationTitleChanged(event.Title)
	case *runtimeclient.ConversationSyncFailed:
		return m.leaveFailedConversation(event.Err)
	case *runtimeclient.ConversationSynced:
		// 已经有根 Session 运行信息时这是重新同步：订阅被关闭过（serve 给对话换了配置或重连），打开后的第一次同步不算。
		_, isResynced := m.sessionStatuses.getRoot()
		m.conversation.ExecutorSettings = event.Conversation.ExecutorSettings
		m.handleConversationTitleChanged(event.Conversation.Title)
		m.sessionStatuses.handleSynced(event.SessionInfos)
		if root, found := m.sessionStatuses.getRoot(); found {
			m.queuedInputs.handleRootSynced(root)
		}
		// 每次同步都按对话当前的默认 Executor 重新拉取 Skills（本端或别的客户端改 Executor 设置后，serve 会让客户端重新同步）。
		cmds := []tea.Cmd{m.loadSkills(), m.startSpinner()}
		if isResynced {
			// 重新同步时还重新拉取模型目录：别的客户端 reload 配置后，模型目录和默认模型可能变了。
			cmds = append(cmds, m.loadModelList())
		}
		return tea.Batch(cmds...)
	case *runtimeclient.SessionStatusChanged:
		m.sessionStatuses.handleChanged(event.Info, m.now())
		if event.Info.Session.ID == m.conversation.RootSessionID {
			m.queuedInputs.handleRootChanged(event.Info)
		}
	case *runtimeclient.SessionSynced:
		root, _ := m.sessionStatuses.getRoot()
		m.print(m.transcript.handleSessionSynced(event, root.ActiveTurnID)...)
		m.queuedInputs.handleItemsSaved(event.Items)
		if m.shouldLoadHistory {
			m.composer.setHistory(m.transcript.getUserInputs())
			m.shouldLoadHistory = false
		}
	case *runtimeclient.ItemsSaved:
		m.print(m.transcript.handleItemsSaved(event.Items)...)
		m.queuedInputs.handleItemsSaved(event.Items)
	case *runtimeclient.ItemChanged:
		m.print(m.transcript.handleItemChanged(event)...)
	case *runtimeclient.ToolProgress:
		m.transcript.handleToolProgress(event)
	case *runtimeclient.TurnStarted:
		m.transcript.handleTurnStarted(event.TurnID)
	case *runtimeclient.TurnFinished:
		m.print(m.transcript.handleTurnFinished(event)...)
		m.queuedInputs.handleTurnFinished(event.InputItemIDs)
	}
	return m.startSpinner()
}

// submitComposer 提交输入框内容，草稿输入先进入新建对话流程。
func (m *tuiModel) submitComposer(mode session.SessionInputMode) tea.Cmd {
	// 1. 忽略空白输入，取出待提交文字；已打开且不在新建流程时直接提交。
	if m.composer.isBlank() {
		return nil
	}
	text := m.composer.takeSubmission()
	if m.conversation != nil && !m.isOpeningDraft {
		return m.submitInput(m.conversation.ID, text, mode)
	}
	// 2. 草稿输入加入待发队列，首次提交时开始新建对话。
	m.composer.queuePendingSend(text)
	if m.isOpeningDraft {
		return nil
	}
	return m.createConversation()
}

// submitInput 创建一条普通输入的提交命令。
func (m *tuiModel) submitInput(conversationID defs.ConversationID, text string, mode session.SessionInputMode) tea.Cmd {
	// 1. 构造不属于草稿打开队列的提交命令。
	return m.newSubmitInputCmd(conversationID, text, mode, false)
}

// newSubmitInputCmd 构造输入请求并返回提交结果消息。
func (m *tuiModel) newSubmitInputCmd(conversationID defs.ConversationID, text string, mode session.SessionInputMode, isDraftOpeningInput bool) tea.Cmd {
	// 1. 保存 Client 和上下文，命令执行时提交文本并带回队列标记、Item ID 和错误。
	client, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		itemID, err := client.SubmitInput(ctx, &runtimeclient.SubmitInputRequest{
			ConversationID: conversationID,
			Mode:           mode,
			Content: []*defs.Content{{
				Kind: defs.ContentText,
				Text: &defs.TextContent{Text: text},
			}},
		})
		return inputSubmittedMsg{
			conversationID:      conversationID,
			text:                text,
			mode:                mode,
			isDraftOpeningInput: isDraftOpeningInput,
			itemID:              itemID,
			err:                 err,
		}
	}
}

// createConversation 按草稿设置新建对话。
func (m *tuiModel) createConversation() tea.Cmd {
	// 1. 标记草稿正在打开，清除先前打开失败的对话，登记导航序号。
	m.isOpeningDraft = true
	m.openFailedConversation = nil
	seq := m.nextRequestSeq()
	m.navigationSeq = seq
	client, ctx := m.opt.Client, m.ctx
	// 2. 携带草稿模型和 Executor 设置发送新建请求，返回带序号的结果。
	request := &runtimeclient.CreateConversationRequest{
		AgentName:        m.opt.AgentName,
		Model:            m.draftModel,
		ExecutorSettings: m.draftExecutorSettings,
	}
	return func() tea.Msg {
		conversation, err := client.CreateConversation(ctx, request)
		return conversationCreatedMsg{
			seq:          seq,
			conversation: conversation,
			err:          err,
		}
	}
}

// handleConnected 记录连上的是否本机 serve，拉取 Executor 列表和模型目录，首次连上时初始化草稿的工作目录。
func (m *tuiModel) handleConnected(serverID string) tea.Cmd {
	// 1. 比较 server_id，查询 Executor 和模型；重连时保留已有草稿设置。
	m.isLocalServer = serverID != "" && serverID == m.opt.LocalServerID
	cmds := []tea.Cmd{m.loadExecutors(), m.loadModelList()}
	if m.hasConnected {
		return tea.Batch(cmds...)
	}
	// 2. 首次连上本机 serve 或目录已按路径映射换算时，把启动目录设为草稿的 local 目录；仍是草稿时重新拉取 Skills。
	m.hasConnected = true
	if !m.isLocalServer && !m.opt.IsWorkspaceDirectoryMapped {
		return tea.Batch(cmds...)
	}
	m.draftExecutorSettings = defs.ExecutorSettings{WorkspaceDirectories: map[string]string{defs.LocalExecutorID: m.opt.WorkspaceDirectory}}
	if m.conversation == nil {
		cmds = append(cmds, m.loadSkills())
	}
	return tea.Batch(cmds...)
}

// loadExecutors 创建查询 Executor 列表的命令。
func (m *tuiModel) loadExecutors() tea.Cmd {
	// 1. 调用 ListExecutors，并通过消息返回列表或错误。
	client, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		executors, err := client.ListExecutors(ctx)
		return executorsLoadedMsg{
			executors: executors,
			err:       err,
		}
	}
}

// handleExecutorsLoaded 保存查询到的 Executor 列表。
func (m *tuiModel) handleExecutorsLoaded(msg executorsLoadedMsg) {
	// 1. 查询失败时记录警告并清空列表，成功时替换列表。
	if msg.err != nil {
		telemetry.GetLogger(m.ctx).Warn("list executors failed", "error", msg.err)
		m.executors = nil
		return
	}
	m.executors = msg.executors
}

// getEditableExecutorSettings 返回工作目录和 Executor 操作对应的对话与设置。
func (m *tuiModel) getEditableExecutorSettings() (defs.ConversationID, defs.ExecutorSettings) {
	// 1. 依次选已打开对话、打开失败对话或草稿；草稿的对话 ID 为空。
	switch {
	case m.conversation != nil:
		return m.conversation.ID, m.conversation.ExecutorSettings
	case m.openFailedConversation != nil:
		return m.openFailedConversation.ID, m.openFailedConversation.ExecutorSettings
	}
	return "", m.draftExecutorSettings
}

// getWorkLocation 返回当前设置下的默认 Executor 和目录。
func (m *tuiModel) getWorkLocation() (executorID, directory string) {
	// 1. 读取可编辑设置，再按 Executor 列表计算默认工作位置。
	_, settings := m.getEditableExecutorSettings()
	return getDefaultExecutorLocation(settings, m.executors)
}

// setExecutorSettings 更新草稿设置或请求保存对话的 Executor 设置。
func (m *tuiModel) setExecutorSettings(conversationID defs.ConversationID, settings defs.ExecutorSettings) tea.Cmd {
	// 1. 草稿：正在新建对话时只提示稍候，否则直接更新草稿设置，重新拉取 Skills 并提示工作位置。
	if conversationID == "" {
		if m.isOpeningDraft {
			return m.showNotice("正在新建对话，请稍候", false, noticeDuration)
		}
		m.draftExecutorSettings = settings
		executorID, directory := getDefaultExecutorLocation(settings, m.executors)
		return tea.Batch(m.loadSkills(), m.showNotice("工作位置："+formatWorkLocation(executorID, directory, m.isLocalServer)+"，下一轮生效", false, noticeDuration))
	}
	// 2. 已有对话：请求保存设置，带回保存后的对话或错误。
	client, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		record, err := client.SetConversationExecutorSettings(ctx, conversationID, settings)
		return executorSettingsSetMsg{
			conversationID: conversationID,
			conversation:   record,
			err:            err,
		}
	}
}

// handleExecutorSettingsSet 处理 Executor 设置保存结果并恢复打开失败的对话。
func (m *tuiModel) handleExecutorSettingsSet(msg executorSettingsSetMsg) tea.Cmd {
	// 1. 忙碌时提示等任务结束，其他失败显示原因并保留原设置。
	if errors.Is(msg.err, session.ErrSessionBusy) {
		return m.showNotice("有任务正在执行，等它结束或按两次 Esc 中断后再改", true, noticeDuration)
	}
	if msg.err != nil {
		return m.showNotice("修改 Executor 设置失败："+msg.err.Error(), true, noticeDuration)
	}
	// 2. 成功后提示工作位置，更新当前对话设置或重新打开先前失败的对话。
	executorID, directory := getDefaultExecutorLocation(msg.conversation.ExecutorSettings, m.executors)
	notice := m.showNotice("工作位置："+formatWorkLocation(executorID, directory, m.isLocalServer)+"，下一轮生效", false, noticeDuration)
	switch {
	case m.conversation != nil && m.conversation.ID == msg.conversationID:
		m.conversation.ExecutorSettings = msg.conversation.ExecutorSettings
	case m.openFailedConversation != nil && m.openFailedConversation.ID == msg.conversationID:
		return tea.Batch(notice, m.resumeConversation(msg.conversation))
	}
	return notice
}

// handleConversationCreated 处理草稿新建结果。
func (m *tuiModel) handleConversationCreated(msg conversationCreatedMsg) tea.Cmd {
	// 1. 丢弃过期导航结果；失败时恢复全部待发输入，成功时打开新对话。
	if msg.seq != m.navigationSeq {
		return nil
	}
	if msg.err != nil {
		return m.abortDraftOpening("", "新建对话失败："+msg.err.Error())
	}
	return m.openConversation(msg.conversation)
}

// openConversation 切换对话显示并创建打开请求。
func (m *tuiModel) openConversation(conversation defs.Conversation) tea.Cmd {
	// 1. 清除失败记录，重建对话组件并写出对话分隔头。
	m.openFailedConversation = nil
	m.setConversation(&conversation)
	m.print(formatBlock([]string{m.formatConversationHeader(conversation)})...)
	// 2. 登记导航序号，同时拉取 Skills 并请求打开对话。
	seq := m.nextRequestSeq()
	m.navigationSeq = seq
	client, ctx := m.opt.Client, m.ctx
	request := &runtimeclient.OpenConversationRequest{ConversationID: conversation.ID}
	return tea.Batch(m.loadSkills(), func() tea.Msg {
		opened, err := client.OpenConversation(ctx, request)
		return conversationOpenedMsg{
			seq:          seq,
			conversation: opened,
			err:          err,
		}
	})
}

// resumeConversation 打开已有对话并准备恢复输入历史。
func (m *tuiModel) resumeConversation(conversation defs.Conversation) tea.Cmd {
	// 1. 关闭面板，标记待加载输入历史，清空草稿模型后打开对话。
	m.closePanel()
	m.shouldLoadHistory = true
	m.draftModel = defs.ModelSelection{}
	return m.openConversation(conversation)
}

// forkConversation 按指定轮次位置分叉当前对话。
func (m *tuiModel) forkConversation(beforeTurnID defs.TurnID, composerText, notice string) tea.Cmd {
	// 1. 草稿正在打开时提示稍候，否则登记本次导航请求。
	if m.isOpeningDraft {
		return m.showNotice("正在新建对话，请稍候", false, noticeDuration)
	}
	seq := m.nextRequestSeq()
	m.navigationSeq = seq
	client, ctx := m.opt.Client, m.ctx
	// 2. 携带当前 Executor 设置和截断轮次（为空时复制全部），结果带回要填进输入框的原文和提示。
	request := &runtimeclient.ForkConversationRequest{
		ConversationID:   m.conversation.ID,
		BeforeTurnID:     beforeTurnID,
		ExecutorSettings: m.conversation.ExecutorSettings,
	}
	return func() tea.Msg {
		forked, err := client.ForkConversation(ctx, request)
		return conversationForkedMsg{
			seq:          seq,
			conversation: forked,
			composerText: composerText,
			notice:       notice,
			err:          err,
		}
	}
}

// handleConversationForked 处理分叉结果。
func (m *tuiModel) handleConversationForked(msg conversationForkedMsg) tea.Cmd {
	// 1. 丢弃过期结果，失败时显示原因并留在原对话。
	if msg.seq != m.navigationSeq {
		return nil
	}
	if msg.err != nil {
		return m.showNotice("分叉失败："+msg.err.Error(), true, noticeDuration)
	}
	// 2. 打开分叉后的对话，恢复回退原文并显示提示。
	cmd := m.resumeConversation(msg.conversation)
	if msg.composerText != "" {
		m.composer.restore(msg.composerText)
	}
	return tea.Batch(cmd, m.showNotice(msg.notice, false, noticeDuration))
}

// handleConversationOpened 处理打开结果并开始提交草稿队列。
func (m *tuiModel) handleConversationOpened(msg conversationOpenedMsg) tea.Cmd {
	// 1. 丢弃过期结果，失败时退回草稿。
	if msg.seq != m.navigationSeq {
		return nil
	}
	if msg.err != nil {
		return m.leaveFailedConversation(msg.err)
	}
	// 2. 打开成功后清空草稿模型，并提交下一条待发输入。
	m.draftModel = defs.ModelSelection{}
	return m.submitNextPending()
}

// leaveFailedConversation 退出打开失败的对话并显示重试提示。
func (m *tuiModel) leaveFailedConversation(err error) tea.Cmd {
	// 1. 保存失败对话记录，关闭面板并切回草稿。
	failed := *m.conversation
	m.closePanel()
	m.shouldLoadHistory = false
	m.setConversation(nil)
	// 2. 草稿流程失败时恢复队列；已有对话失败时保留记录供 /cd、/executor 修改后重开。
	reason := "打开对话失败：" + err.Error()
	if m.isOpeningDraft {
		return tea.Batch(m.loadSkills(), m.abortDraftOpening("", reason))
	}
	m.openFailedConversation = &failed
	return tea.Batch(m.loadSkills(), m.showNotice(reason+"；可用 /executor 或 /cd 修改它的设置", true, noticeDuration))
}

// submitNextPending 顺序提交下一条草稿输入。
func (m *tuiModel) submitNextPending() tea.Cmd {
	// 1. 取出下一条待发文字；队列为空时结束草稿打开流程。
	text, found := m.composer.takeNextPendingSend()
	if !found {
		m.isOpeningDraft = false
		return nil
	}
	return m.newSubmitInputCmd(m.conversation.ID, text, session.SessionInputModeNormal, true)
}

// handleInputSubmitted 处理提交结果，恢复失败输入或更新排队提示。
func (m *tuiModel) handleInputSubmitted(msg inputSubmittedMsg) tea.Cmd {
	// 1. 识别草稿队列结果；提交失败时恢复原文，并终止对应的草稿打开流程。
	isDraftOpeningResult := msg.isDraftOpeningInput && m.isOpeningDraft
	if msg.err != nil {
		reason := "发送失败，草稿已恢复：" + msg.err.Error()
		if isDraftOpeningResult {
			return m.abortDraftOpening(msg.text, reason)
		}
		m.composer.restore(msg.text)
		return m.showNotice(reason, true, noticeDuration)
	}
	// 2. steer 显示插入提示，未保存的 normal 输入登记队列预览。
	var cmd tea.Cmd
	switch {
	case msg.mode == session.SessionInputModeSteer:
		cmd = m.showNotice("已插入，将在下一步读入", false, noticeDuration)
	case m.conversation != nil && msg.conversationID == m.conversation.ID && !m.transcript.isItemSaved(msg.itemID):
		m.queuedInputs.add(msg.itemID, msg.text)
	}
	// 3. 只有本次草稿流程的提交结果继续推进待发队列。
	if isDraftOpeningResult {
		return tea.Batch(cmd, m.submitNextPending())
	}
	return cmd
}

// abortDraftOpening 结束草稿打开流程并恢复未发送文字。
func (m *tuiModel) abortDraftOpening(failed, reason string) tea.Cmd {
	// 1. 清除打开标记，将失败输入放在待发队列之前恢复到输入框，再提示错误。
	m.isOpeningDraft = false
	m.composer.restorePendingSends(failed)
	return m.showNotice(reason, true, noticeDuration)
}

// handleConversationTitleChanged 保存新标题并补写对话分隔头。
func (m *tuiModel) handleConversationTitleChanged(title string) {
	// 1. 标题未变时跳过，否则更新记录并写出新分隔头。
	if m.conversation.Title == title {
		return
	}
	m.conversation.Title = title
	m.print(formatBlock([]string{m.formatConversationHeader(*m.conversation)})...)
}

// setConversation 切换对话并重建这个对话的显示组件。
func (m *tuiModel) setConversation(conversation *defs.Conversation) {
	// 1. 保存对话，按根 Session 创建运行状态、排队输入和历史记录；nil 表示草稿。
	m.conversation = conversation
	rootSessionID := defs.SessionID("")
	if conversation != nil {
		rootSessionID = conversation.RootSessionID
	}
	m.sessionStatuses = newSessionStatuses(rootSessionID)
	m.queuedInputs = &queuedInputs{}
	m.transcript = newTranscript(m.renderer, m.styles, m.sessionStatuses, m.modelDisplayNames, m.height, func() time.Time { return m.now() },
		func() bool { return m.isLocalServer },
		func() string {
			executorID, _ := m.getWorkLocation()
			return executorID
		})
	// 2. 清空 Skill 列表，重置模型和 Skill 请求序号，之前对话还没返回的结果随后按序号丢弃。
	m.modelSetSeq = 0
	m.skills = nil
	m.skillListSeq = 0
}

// loadSkills 查询草稿或已打开对话可用的 Skill 目录。
func (m *tuiModel) loadSkills() tea.Cmd {
	// 1. 登记查询序号；草稿带上 Agent 和新建对话要用的 Executor 设置，已有对话带对话 ID 和 Agent，由 serve 按保存的设置查。
	seq := m.nextRequestSeq()
	m.skillListSeq = seq
	request := &runtimeclient.GetSkillsRequest{
		AgentName:        m.opt.AgentName,
		ExecutorSettings: m.draftExecutorSettings,
	}
	if m.conversation != nil {
		request = &runtimeclient.GetSkillsRequest{
			ConversationID: m.conversation.ID,
			AgentName:      m.opt.AgentName,
		}
	}
	client, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		skills, err := client.GetSkills(ctx, request)
		return skillListLoadedMsg{
			seq:    seq,
			skills: skills,
			err:    err,
		}
	}
}

// loadModelList 创建查询模型目录的命令。
func (m *tuiModel) loadModelList() tea.Cmd {
	// 1. 按界面 Agent 查询模型目录，返回目录或错误。
	client, ctx, agentName := m.opt.Client, m.ctx, m.opt.AgentName
	return func() tea.Msg {
		list, err := client.ListModels(ctx, agentName)
		return modelListLoadedMsg{
			list: list,
			err:  err,
		}
	}
}

// handleModelListLoaded 处理模型目录查询结果。
func (m *tuiModel) handleModelListLoaded(msg modelListLoadedMsg) tea.Cmd {
	// 1. 查询失败时保留原目录；成功时更新显示名和默认模型。
	if msg.err != nil {
		return nil
	}
	m.applyModelList(msg.list, m.opt.AgentName)
	// 2. 无模型、未关闭过引导且没有其他面板时，打开提供商添加流程。
	if len(msg.list.Models) > 0 || m.hasDismissedProviderOnboarding || m.activePanel != nil {
		return nil
	}
	return m.openProviderOnboarding()
}

// applyModelList 更新模型显示名、空目录标记和界面 Agent 的默认模型。
func (m *tuiModel) applyModelList(list runtime.ModelList, agentName string) {
	// 1. 原地更新显示名映射并记录目录是否为空。
	clear(m.modelDisplayNames)
	for _, model := range list.Models {
		m.modelDisplayNames[model.Ref] = model.DisplayName
	}
	m.isModelListEmpty = len(list.Models) == 0
	// 2. 查询的是界面新建对话所用的 Agent 时，才记下它的默认模型。
	if agentName == m.opt.AgentName {
		m.agentDefaultModel = list.AgentDefaultModel
	}
}

// getCurrentModelSelection 返回当前对话或草稿选择的模型。
func (m *tuiModel) getCurrentModelSelection() defs.ModelSelection {
	// 1. 有根 Session 时取它的模型，否则取草稿模型；没指定模型时补 Agent 默认模型。
	selection := m.draftModel
	if root, found := m.sessionStatuses.getRoot(); found {
		selection = root.Session.Model
	}
	if selection.Ref == (defs.ModelRef{}) {
		selection.Ref = m.agentDefaultModel
	}
	return selection
}

// setSessionModel 创建保存根 Session 模型选择的命令。
func (m *tuiModel) setSessionModel(selection defs.ModelSelection) tea.Cmd {
	// 1. 登记模型设置序号，提交对话、根 Session 和选择结果。
	seq := m.nextRequestSeq()
	m.modelSetSeq = seq
	client, ctx := m.opt.Client, m.ctx
	request := &runtimeclient.SetSessionModelRequest{
		ConversationID: m.conversation.ID,
		SessionID:      m.conversation.RootSessionID,
		Model:          selection,
	}
	return func() tea.Msg {
		record, err := client.SetSessionModel(ctx, request)
		return modelSetMsg{
			seq:     seq,
			session: record,
			err:     err,
		}
	}
}

// handleModelSet 显示模型切换结果。
func (m *tuiModel) handleModelSet(msg modelSetMsg) tea.Cmd {
	// 1. 丢弃过期结果；失败显示原因，成功显示模型，运行中追加下一 Turn 生效提示。
	if msg.seq != m.modelSetSeq {
		return nil
	}
	if msg.err != nil {
		return m.showNotice("切换模型失败："+msg.err.Error(), true, noticeDuration)
	}
	text := "已切换到 " + m.formatModelSelection(msg.session.Model)
	if m.sessionStatuses.isRootRunning() {
		text += "，下一 Turn 生效"
	}
	return m.showNotice(text, false, noticeDuration)
}

// getEnterMode 返回 Enter 发送输入时使用的模式。
func (m *tuiModel) getEnterMode() session.SessionInputMode {
	// 1. 根 Session 运行中返回 steer，空闲时返回 normal。
	if m.sessionStatuses.isRootRunning() {
		return session.SessionInputModeSteer
	}
	return session.SessionInputModeNormal
}

// interruptTurn 创建中断根 Session 正在运行的 Turn 的命令。
func (m *tuiModel) interruptTurn() tea.Cmd {
	// 1. 向当前对话发出中断请求并返回结果消息。
	client, ctx, conversationID := m.opt.Client, m.ctx, m.conversation.ID
	return func() tea.Msg {
		_, err := client.InterruptTurn(ctx, conversationID)
		return turnInterruptedMsg{err: err}
	}
}

// handleTurnInterrupted 显示中断请求的错误。
func (m *tuiModel) handleTurnInterrupted(msg turnInterruptedMsg) tea.Cmd {
	// 1. 成功时不提示，失败时显示错误原文。
	if msg.err == nil {
		return nil
	}
	return m.showNotice("中断失败："+msg.err.Error(), true, noticeDuration)
}

// handleCompactRequested 处理压缩上下文请求的结果。
func (m *tuiModel) handleCompactRequested(msg compactRequestedMsg) tea.Cmd {
	// 1. 忙碌时提示稍后压缩，其他错误显示原因；成功时不提示，压缩进度由事件流更新。
	if errors.Is(msg.err, session.ErrSessionBusy) {
		return m.showNotice("正在执行，稍后再压缩", true, noticeDuration)
	}
	if msg.err != nil {
		return m.showNotice("压缩上下文失败："+msg.err.Error(), true, noticeDuration)
	}
	return nil
}

// startSpinner 启动转圈动画。
func (m *tuiModel) startSpinner() tea.Cmd {
	// 1. 已有计时或没有运行中 Session 时跳过，否则标记启动并安排下一帧。
	if m.isSpinnerTicking || !m.sessionStatuses.isAnyRunning() {
		return nil
	}
	m.isSpinnerTicking = true
	return m.spinner.Tick
}

// handleSpinnerTick 更新转圈动画。
func (m *tuiModel) handleSpinnerTick(msg spinner.TickMsg) tea.Cmd {
	// 1. 检查运行状态，空闲时清除计时标记，否则更新动画并安排下一帧。
	if !m.sessionStatuses.isAnyRunning() {
		m.isSpinnerTicking = false
		return nil
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return cmd
}

// nextRequestSeq 递增并返回异步请求序号。
func (m *tuiModel) nextRequestSeq() int {
	// 1. 递增界面的请求计数。
	m.requestSeq++
	return m.requestSeq
}

// print 把定稿行追加到本次界面输出队列。
func (m *tuiModel) print(lines ...string) {
	// 1. 按传入顺序追加待写出的行。
	m.pendingPrints = append(m.pendingPrints, lines...)
}

// flushPrints 先写出积累的定稿内容，再执行后续命令。
func (m *tuiModel) flushPrints(cmd tea.Cmd) tea.Cmd {
	// 1. 终端已出错、展开视图打开或正在退出全屏时保留待写内容；上一批输出还没完成时先等它，正在退出时丢掉后续命令。
	if m.terminalError != nil || m.expandView != nil || m.isLeavingExpandView {
		return cmd
	}
	if m.isPrintingScrollback {
		if m.isQuitting {
			return nil
		}
		return cmd
	}
	// 2. 有待重建历史且不在退出时，暂停事件循环和渲染器重建画面，完成后再执行后续命令。
	if m.shouldRebuildScrollback && !m.isQuitting {
		m.isPrintingScrollback = true
		return tea.Sequence(tea.Exec(&scrollbackRebuildCommand{model: m}, func(err error) tea.Msg {
			return scrollbackPrintedMsg{err: err}
		}), cmd)
	}
	if len(m.pendingPrints) == 0 {
		return cmd
	}
	// 3. 实时帧刚缩短时暂存定稿行，等待新帧显示；退出时不等待。
	if !m.isQuitting {
		if m.isWaitingLiveFrame {
			return cmd
		}
		if wait := liveFrameSettleDelay - m.now().Sub(m.liveShrunkAt); wait > 0 {
			m.isWaitingLiveFrame = true
			return tea.Batch(cmd, tea.Tick(wait, func(time.Time) tea.Msg { return liveFrameSettledMsg{} }))
		}
	}
	// 4. 按实时帧外剩余行数分批写出，刷新实时区域和光标，再报告批次完成并执行后续命令。
	chunks := splitPrintChunks(m.pendingPrints, m.width, max(1, m.height-m.lastLiveHeight))
	m.pendingPrints = nil
	m.isPrintingScrollback = true
	cmds := make([]tea.Cmd, 0, len(chunks)+3)
	for _, chunk := range chunks {
		cmds = append(cmds, tea.Println(strings.Join(chunk, "\n")))
	}
	return tea.Sequence(append(cmds, tea.ClearScreen, func() tea.Msg { return scrollbackPrintedMsg{} }, cmd)...)
}

// rebuildScrollback 在渲染暂停期间清屏，按最新宽度重新写出对话历史，并给实时帧留出行数。
func (m *tuiModel) rebuildScrollback(output io.Writer) error {
	// 1. 退出、展开视图打开或正在退出全屏时跳过，否则生成当前对话的分隔头。
	if m.isQuitting || m.expandView != nil || m.isLeavingExpandView {
		return nil
	}
	header := m.formatHeader("新对话")
	if m.conversation != nil {
		header = m.formatConversationHeader(*m.conversation)
	}
	// 2. 清除终端画面和原有滚动历史，按新宽度写出当前历史并预留实时帧行数。
	lines := append(formatBlock([]string{header}), m.transcript.getScrollbackLines()...)
	liveHeight := strings.Count(m.View().Content, "\n") + 1
	content := "\x1b[2J\x1b[H\x1b[3J" + strings.Join(lines, "\r\n") + "\r\n"
	content += strings.Repeat("\r\n", liveHeight-1)
	_, err := io.WriteString(output, content)
	if err != nil {
		return err
	}
	// 3. 写出成功后清空待打印内容和重建标记。
	m.pendingPrints = nil
	m.shouldRebuildScrollback = false
	return nil
}

// Run 在渲染暂停期间重建滚动历史。
func (c *scrollbackRebuildCommand) Run() error {
	// 1. 用 SetStdout 保存的终端输出重建画面。
	return c.model.rebuildScrollback(c.output)
}

// SetStdin 忽略 bubbletea 传入的输入流。
func (c *scrollbackRebuildCommand) SetStdin(io.Reader) {}

// SetStderr 忽略 bubbletea 传入的错误输出流。
func (c *scrollbackRebuildCommand) SetStderr(io.Writer) {}

// SetStdout 保存重建画面使用的输出流。
func (c *scrollbackRebuildCommand) SetStdout(output io.Writer) {
	// 1. 替换输出写入器。
	c.output = output
}

// formatConversationHeader 返回带标题和短 ID 的对话分隔头。
func (m *tuiModel) formatConversationHeader(conversation defs.Conversation) string {
	// 1. 取对话标题与 ID 后八个字符，交给分隔头渲染。
	return m.formatHeader(getConversationTitle(conversation) + " · " + lastRunes(string(conversation.ID), 8))
}

// formatHeader 返回按当前宽度补齐或截断的暗色分隔头。
func (m *tuiModel) formatHeader(text string) string {
	// 1. 拼接文字和横线，再截断到终端宽度并加暗色样式。
	line := "── " + text + " "
	line += strings.Repeat("─", max(0, m.width-ansi.StringWidth(line)))
	return m.styles.faint.Render(ansi.Truncate(line, m.width, ""))
}

// quit 退出界面。
func (m *tuiModel) quit() tea.Cmd {
	// 1. 标记正在退出；展开视图仍打开时先关闭，正在退出全屏时等待，之后再发出退出命令。
	m.isQuitting = true
	switch {
	case m.expandView != nil:
		return m.closeExpandView()
	case m.isLeavingExpandView:
		return nil
	}
	return tea.Quit
}
