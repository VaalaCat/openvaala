// Package desktop 绘制原生工作台并连接本机 Runtime Server。
package desktop

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/httpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/egoist/mygo"
	_ "github.com/egoist/mygo/locales/zh-Hans"
	"github.com/egoist/mygo/ui"
)

// Desktop 在窗口主线程持有阅读内容、编辑草稿和界面选择。
type Desktop struct {
	opt                           DesktopOpt
	window                        *mygo.Window
	ctx                           context.Context
	cancel                        context.CancelFunc
	tasks                         sync.WaitGroup
	client                        runtimeclient.Client
	preferences                   preferences
	translations                  map[string]map[string]string
	preferencesPath               string
	connectionState               runtimeclient.ConnectionState
	connectionError               error
	serverID                      string
	closed                        bool
	allowClose                    bool
	confirmExit                   bool
	commandPaletteOpen            bool
	conversationSearchHandle      ui.Handle
	shownRightPanel               string
	shownBottomPanel              string
	page                          string
	connecting                    bool
	clientStopped                 bool
	clientNeedsReading            bool
	preferencesBroken             bool
	conversationSearch            string
	conversations                 []dao.ConversationListEntry
	conversationCursor            dao.RecordCursor[defs.ConversationID]
	conversationLoading           bool
	conversationError             error
	conversationRequest           uint64
	archived                      bool
	conversationHighlight         defs.ConversationID
	runningKnown                  bool
	runningConversations          []defs.ConversationID
	unreadConversations           map[defs.ConversationID]bool
	queueExpanded                 bool
	pendingToasts                 []kit.ToastOpt
	composerFocusRequested        bool
	dialogWasOpen                 bool
	readingRequests               chan readingRequested
	cancelReading                 context.CancelFunc
	readingTarget                 readingTarget
	conversation                  defs.Conversation
	sessionInfos                  []runtime.SessionRuntimeInfo
	items                         []viewItem
	toolProgress                  map[string]string
	itemCursor                    storage.Ordinal
	readingReady                  bool
	historyLoading                bool
	historyError                  error
	messageList                   *ui.ListState
	conversationList              ui.ListState
	pendingInputs                 session.SessionPendingInputs
	turns                         map[defs.TurnID]defs.Turn
	drafts                        map[draftKey]*conversationDraft
	composerHandle                ui.Handle
	models                        runtime.ModelList
	modelLoading                  bool
	modelsLoaded                  bool
	newModel                      string
	newEffort                     string
	newAgent                      string
	executors                     []conversation.ExecutorDetails
	executorsLoading              bool
	newExecutor                   string
	newDirectory                  string
	recentConversations           []defs.Conversation
	recentConversationsRequest    uint64
	directoryError                error
	directoryRequest              uint64
	directoryListing              directoryListing
	directoryEditing              directoryEditing
	agents                        []runtime.AgentInfo
	agentsError                   error
	workLocationPopoverOpen       bool
	skillCatalogs                 map[skillCatalogKey]*skillCatalog
	configuredSkills              []skill.SkillInfo
	recalledOffset                int
	recalledMessage               string
	composerCaret                 int
	composerRequestedCaret        int
	composerCaretRequested        bool
	itemExpanded                  map[string]bool
	inspectedItem                 string
	inspectedTurn                 defs.TurnID
	contextUsage                  *runtime.SessionContextUsage
	sessionDetails                *conversation.SessionDetails
	contextLoading                bool
	sessionDetailsLoading         bool
	turnsLoading                  bool
	turnList                      *ui.ListState
	detailsError                  error
	sessionDetailsOpen            bool
	turnCursor                    dao.RecordCursor[defs.TurnID]
	turnPage                      []defs.Turn
	rpcFrames                     []runtimeclient.RPCFrame
	rpcRows                       []rpcRow // rpcFrames 配对后的行，随 rpcFrames 同时更新
	rpcLoading                    bool
	rpcPaused                     bool
	rpcMethod                     string
	rpcDirection                  string
	rpcConversation               string
	rpcSelected                   uint64
	rpcList                       ui.ListState
	rpcListWidth                  float32
	providers                     []defs.ProviderConfig
	defaultModel                  defs.ModelRef
	modelDefaults                 defs.ModelDefaults
	providerDraft                 *providerDraft
	providerBaseline              *providerDraft
	providerLoading               bool
	providerSaving                bool
	providerError                 error
	catalogOpen                   bool
	catalogRequested              bool
	catalogError                  error
	modelsFetching                bool
	skillsAgent                   string
	previousConversation          defs.Conversation
	catalogProviders              []defs.CatalogProvider
	catalogModels                 map[string]*defs.ProviderModelConfig
	providerModelSearch           string
	manualModel                   string
	selectedCatalog               string
	hooks                         []*hook.HookInfo
	mcpServers                    []conversation.MCPServerInfo
	mcpLoading                    bool
	mcpError                      error
	settingsError                 error
	settingsLoading               bool
	settingsRequest               uint64
	confirm                       confirmation
	confirmOpen                   bool
	confirmError                  error
	confirmPending                bool
	renameText                    string
	titleEditingID                defs.ConversationID
	titleDraft                    string
	titleWasFocused               bool
	titleFocusRequested           bool
	messageMenuItem               viewItem
	turnJumpRequested             defs.TurnID
	rightDockHiddenBySettings     bool
	sidebarShownBySettings        bool
	sidebarDrawerOpenedBySettings bool
	locationOpen                  bool
	locationTab                   int
	locationDirectories           map[string]string
	locationAvailable             map[string]bool
	locationDefault               string
	locationError                 error
	locationSaving                bool
	images                        map[string]*imageResource
	imageHTTPClient               *http.Client // 下载 Markdown 里的网络图片：带 http.headers，允许跨域重定向，超时 20 秒
	imagePreview                  string
	imagePreviewOpen              bool
	imagePreviewState             kit.ImagePreviewState
	terminals                     []*terminalTab
	selectedTerminal              string
	terminalSequence              int
	// runningTerminals 是 serve 上全部运行中的终端；terminalsKnown 为 false 时列表尚未取得，不代表没有终端。
	runningTerminals []runtimeprotocol.TerminalItem
	terminalsKnown   bool
	// openingTerminalRequests 是还没返回的 terminal/open 请求数，大于 0 时运行列表不加标签；
	// closingTerminalIDs 是 terminal/close 请求还没返回的终端，运行列表不把它们加回成标签。
	openingTerminalRequests int
	closingTerminalIDs      []string
}

// DesktopOpt 指定本机服务启动参数、可选的偏好文件路径和图片下载带的 header。
type DesktopOpt struct {
	RuntimeServerStart cli.RuntimeServerStartOpt
	PreferencesPath    string
	// HTTPHeaders 是配置里的 http.headers（已合并内置默认）；原生界面自己下载 Markdown 里的网络图片时带上。
	HTTPHeaders map[string]string
}

// NewDesktop 校验并保存桌面启动参数。
func NewDesktop(opt *DesktopOpt) (*Desktop, error) {
	// 1. 保存启动配置。
	if opt == nil || opt.RuntimeServerStart.Executable == "" {
		return nil, errors.New("desktop requires a runtime executable")
	}
	// 2. 建立图片下载客户端，整个桌面进程共用。
	imageHTTPClient := httpclient.NewHTTPClient(httpclient.NewHTTPClientOpt().WithHeaders(opt.HTTPHeaders).WithCrossOriginRedirect())
	imageHTTPClient.Timeout = 20 * time.Second
	return &Desktop{opt: *opt, imageHTTPClient: imageHTTPClient}, nil
}

// Run 在主 goroutine 运行窗口，退出后取消并等待桌面任务。
func (d *Desktop) Run(ctx context.Context) error {
	// 1. 取得单实例身份，读取桌面偏好及字体。
	mygo.App.SetName("OpenVaala")
	if !mygo.App.RequestSingleInstanceLock() {
		return nil
	}
	d.ctx, d.cancel = context.WithCancel(context.WithoutCancel(ctx))
	d.readingRequests = make(chan readingRequested, 1)
	d.tasks.Go(d.runReadingRequests)
	d.preferences = defaultPreferences()
	d.translations = loadTranslations()
	d.newAgent = "default"
	d.newExecutor = "local"
	d.drafts = make(map[draftKey]*conversationDraft)
	d.turns = make(map[defs.TurnID]defs.Turn)
	d.itemExpanded = make(map[string]bool)
	d.unreadConversations = make(map[defs.ConversationID]bool)
	d.images = make(map[string]*imageResource)
	d.skillCatalogs = make(map[skillCatalogKey]*skillCatalog)
	d.preferencesPath = d.opt.PreferencesPath
	if d.preferencesPath == "" {
		if dir, err := mygo.App.Path(mygo.PathUserData); err == nil {
			d.preferencesPath = filepath.Join(dir, "preferences.json")
		} else {
			d.showFailure(err)
		}
	}
	if err := d.loadPreferences(); err != nil {
		d.showFailure(err)
		d.preferencesBroken = true
	}
	if err := registerFonts(); err != nil {
		d.showFailure(err)
	}
	// 2. 就绪后建立窗口、菜单与连接任务。
	mygo.App.WhenReady(func() {
		d.window = mygo.NewWindow(mygo.WindowOptions{Title: "OpenVaala", Width: d.preferences.Width, Height: d.preferences.Height, MinWidth: 640, MinHeight: 480, TitleBarStyle: mygo.TitleBarHidden, TitleBarHeight: 36, Content: ui.View(d.view)})
		d.window.OnClose(func(e *mygo.CloseEvent) {
			if !d.allowClose && d.isUnsavedWorkPresent() {
				e.PreventDefault()
				d.confirmExit = true
				d.window.Invalidate()
			}
		})
		d.window.OnClosed(func() { d.closed = true; d.cancel() })
		d.window.OnResize(func() { d.preferences.Width, d.preferences.Height = d.window.Size() })
		d.window.OnFocus(func() {
			delete(d.unreadConversations, d.readingTarget.ConversationID)
			d.window.Invalidate()
		})
		d.installMenu()
		d.connect()
		d.tasks.Go(func() {
			select {
			case <-ctx.Done():
				mygo.App.Quit()
			case <-d.ctx.Done():
			}
		})
	})
	mygo.App.OnSecondInstance(func(_ []string, _ string) {
		if d.window != nil {
			d.window.Restore()
			d.window.Show()
			d.window.Focus()
		}
	})
	// 3. 等待框架结束，再收尾本进程任务。
	err := mygo.App.Run()
	d.cancel()
	d.tasks.Wait()
	d.closeTerminalStates()
	if saveErr := d.savePreferences(); saveErr != nil && err == nil {
		err = saveErr
	}
	return err
}

// installMenu 注册系统应用菜单、文本编辑命令及工作台快捷键。
func (d *Desktop) installMenu() {
	// 1. 系统角色接入当前原生文本控件。
	if d.window == nil {
		return
	}
	mygo.App.SetLanguages("zh-Hans", "en")
	language := d.preferences.Language
	if language == "zh-CN" {
		language = "zh-Hans"
	}
	mygo.App.SetLocale(language)
	// 2. 快捷键用菜单加速键注册，文本输入获得焦点时同样生效。
	// 手测（Linux 和 macOS 真实按键）：在输入框有焦点时依次按 Ctrl+N（新对话）、Ctrl+,（设置，再按一次回到对话）、
	// Ctrl+B（切换侧栏）、Ctrl+J（切换下 Dock）、Ctrl+Shift+R（切换右 Dock）、Ctrl+`（聚焦终端，没有终端时新开一个），
	// 每个都应立即生效且不往输入框里打出字符；macOS 上把 Ctrl 换成 Command，终端快捷键仍是 Command+`。
	// 手测：在 Linux 和 macOS 上真实按 Ctrl+Tab（macOS 是 Control+Tab，不是 Command）：在两个对话之间来回切换，输入框有焦点时同样生效；
	// 再按 Ctrl+K 打开命令面板，面板里「上一个对话」右侧显示 Ctrl+Tab（macOS 显示 ⌃Tab）。
	item := func(label, accelerator, commandID string) *mygo.MenuItem {
		return &mygo.MenuItem{Label: label, Accelerator: accelerator, Click: func(_ *mygo.MenuItem, _ *mygo.Window) {
			d.runCommand(commandID)
			d.window.Invalidate()
		}}
	}
	mygo.App.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
		{Role: mygo.RoleAppMenu},
		{Role: mygo.RoleEditMenu},
		{Label: d.t("go"), Submenu: []*mygo.MenuItem{
			item(d.t("command.palette"), "CmdOrCtrl+K", commandPalette),
			item(d.t("new.conversation"), "CmdOrCtrl+N", commandNewConversation),
			item(d.t("settings"), "CmdOrCtrl+,", commandSettings),
			item(d.t("command.previous.conversation"), "", commandPreviousConversation),
		}},
		{Label: d.t("view"), Submenu: []*mygo.MenuItem{
			item(d.t("toggle.sidebar"), "CmdOrCtrl+B", commandToggleSidebar),
			item(d.t("toggle.bottom.dock"), "CmdOrCtrl+J", commandToggleBottomDock),
			item(d.t("toggle.right.dock"), "CmdOrCtrl+Shift+R", commandToggleRightDock),
			item(d.t("terminal.focus"), "CmdOrCtrl+`", commandTerminal),
		}},
	}))
}

// view 绘制工作台分区、命令面板、弹窗与退出确认。
func (d *Desktop) view(c *ui.Context) {
	// 1. 应用统一外观；顶栏、侧栏、Dock 与状态栏由 Workspace 排布。
	d.applyAppearance(c)
	kit.Workspace(c, &d.preferences.Workspace, kit.WorkspaceOpt{
		TitleBar: func() { d.titleBarView(c) },
		Sidebar:  func() { d.sidebarView(c) },
		Center: func() {
			if d.page != "" {
				d.settingsView(c)
				return
			}
			d.conversationView(c)
		},
		RightPanels:  d.getRightPanels(c),
		BottomPanels: d.getBottomPanels(c),
		StatusLeft:   func() { d.statusLeftView(c) },
	})
	d.loadShownPanels()
	// Esc 从设置页回到对话；弹窗、下拉打开时 Esc 先关闭它们。
	if d.page != "" && c.Shortcut(0, ui.KeyEscape) {
		d.navigate("")
	}
	// Ctrl+Tab 切到上一个对话：GTK 不接受 Tab 作菜单加速键，所以在窗口里直接读这个组合键，菜单项不带加速键。
	if c.Shortcut(ui.Ctrl, ui.KeyTab) {
		d.runCommand(commandPreviousConversation)
	}
	// 2. 命令面板每帧绘制，选中的命令立即执行；自己要焦点的命令不再把焦点还给输入框。
	focusedByCommand := false
	if commandID := kit.CommandPalette(c, &d.commandPaletteOpen, d.getCommands(c)); commandID != "" {
		d.runCommand(commandID)
		focusedByCommand = isCommandFocusing(commandID)
		// 面板关闭和命令的效果都要再画一帧：Enter 选定后窗口没有别的事件触发重绘。
		c.AnimationFrame()
	}
	d.confirmationView(c)
	d.imagePreviewView(c)
	d.workLocationDialog(c)
	// 通知先入队再画宿主；ShowToast 自己请求下一帧，窗口空闲时通知也能立刻出现。
	for _, toast := range d.pendingToasts {
		kit.ShowToast(c, toast)
	}
	d.pendingToasts = nil
	kit.ToastHost(c)
	// 3. 未保存内容统一确认，取消不改变连接与草稿；Enter 确认、Esc 取消。
	if kit.DialogWith(c, &d.confirmExit, kit.DialogOpt{Title: d.t("discard.unsaved.changes.and.drafts"), ConfirmLabel: d.t("discard.and.quit"), Danger: true}, func() {}) {
		d.allowClose = true
		d.window.Close()
	}
	// 4. 任何弹窗从打开变为关闭的那一帧，焦点回到输入框；命令面板里选了自己要焦点的命令（搜索、终端、重命名）时除外。
	dialogOpen := d.confirmOpen || d.locationOpen || d.confirmExit || d.imagePreviewOpen || d.commandPaletteOpen
	if d.dialogWasOpen && !dialogOpen && !focusedByCommand {
		d.composerFocusRequested = true
	}
	d.dialogWasOpen = dialogOpen
}

// showToast 排队一条通知，下一帧由 view 显示；通知来自任务回调，回调里没有 Context。
func (d *Desktop) showToast(opt kit.ToastOpt) {
	d.pendingToasts = append(d.pendingToasts, opt)
}

// showSuccess 排队一条成功通知；detail 可为空。
func (d *Desktop) showSuccess(title, detail string) {
	d.showToast(kit.ToastOpt{Title: title, Description: detail, Kind: kit.ToastSuccess})
}

// showFailure 排队一条失败通知：标题是本地化说明，错误原文放在详情里。
func (d *Desktop) showFailure(err error) {
	d.showToast(kit.ToastOpt{Title: d.getErrorMessage(err), Description: err.Error(), Kind: kit.ToastError})
}

// titleBarView 显示应用名；打开对话时接工作位置，点击可修改。
func (d *Desktop) titleBarView(c *ui.Context) {
	// 1. 应用名固定在左侧。
	// 手测（macOS 真机）：标题栏隐藏后红绿灯浮在左上角，应用名和侧栏开关不被它盖住；
	// 把窗口缩到 900 宽以下，打开侧栏抽屉和右 Dock 抽屉，抽屉从标题栏下方展开，红绿灯始终可点。
	t := kit.GetTheme(c)
	t.TextRole(ui.Text(c, "OpenVaala"), kit.TextUI).TextColor(t.Colors.Text).SingleLine().Shrink(0)
	if d.conversation.ID == "" || d.page != "" {
		return
	}
	// 2. 工作位置为幽灵按钮：执行环境与目录名；没有记录目录时用 Executor 的默认根目录。
	executor, directory := d.getWorkLocationExecutor()
	location := getWorkLocationLabel(executor, directory, 0)
	button := kit.ToolButton(c.Key("title-work-location"), kit.IconFolder, location, true).Label(d.t("workspace")).TextColor(t.Colors.Muted).Shrink(1)
	kit.Tooltip(c, button, executor+" · "+directory)
	if button.Clicked() {
		d.openWorkLocation()
	}
}

// statusLeftView 绘制状态栏左侧：设置入口和连接状态；状态栏只显示连接状态，原因和「重试」在对话区的横幅里。
func (d *Desktop) statusLeftView(c *ui.Context) {
	// 1. 设置入口：在设置页内再点一次回到对话。
	t := kit.GetTheme(c)
	settings := kit.StatusButton(c, kit.IconSettings, d.t("settings"), d.page != "")
	kit.Tooltip(c, settings, d.t("settings"))
	if settings.Clicked() {
		d.runCommand(commandSettings)
	}
	// 2. 圆点颜色和一句简短的状态文字表示连接状态，没有错误原文。
	label, color := d.t("connected"), t.Colors.Success
	if d.connectionState != runtimeclient.ConnectionConnected {
		label, color = d.t("connecting"), t.Colors.Warning
		if d.connectionState == runtimeclient.ConnectionReconnecting {
			label = d.t("reconnecting")
		}
	}
	if d.connectionError != nil {
		color = t.Colors.Danger
		if d.connectionState != runtimeclient.ConnectionReconnecting {
			label = d.t("connection.failed")
		}
	}
	// 3. 悬停状态文字时提示完整的错误原文。
	ui.Row(c).Shrink(1).Padding(0, t.Space(1)).Gap(t.Space(2)).AlignItems(ui.Center).Children(func() {
		kit.StatusDot(c, color)
		status := ui.Text(c, label).SingleLine().Shrink(0)
		if d.connectionError != nil {
			kit.Tooltip(c, status, d.connectionError.Error())
		}
	})
}

// isUnsavedWorkPresent 是否有尚未应用的设置或未发送内容？
func (d *Desktop) isUnsavedWorkPresent() bool {
	// 1. 检查完整设置草稿与全部对话草稿。
	if d.isProviderDirty() {
		return true
	}
	for _, draft := range d.drafts {
		if isContentPresent(draft.Content) {
			return true
		}
	}
	return false
}

// getErrorText 返回本地化的错误说明；没有对应说明的错误在「操作失败」后附上原文，表单校验、终端屏幕这类没有详情位置的地方才看得到原因。
func (d *Desktop) getErrorText(err error) string {
	text := d.getErrorMessage(err)
	if text == d.t("error.operation.failed") {
		text += "：" + err.Error()
	}
	return text
}

// errorMessageView 绘制一行本地化的错误说明，悬停时提示错误原文。
func (d *Desktop) errorMessageView(c *ui.Context, err error) ui.Element {
	text := d.getErrorText(err)
	message := kit.FieldMessage(c, kit.MessageError, text)
	kit.Tooltip(c, message, err.Error())
	return message
}
