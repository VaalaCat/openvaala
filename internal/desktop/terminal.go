package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/egoist/mygo/ui"
)

// terminalPanel 绘制下 Dock 终端面板的内容：当前标签的终端；标签条在 Dock 标题栏里。
func (d *Desktop) terminalPanel(c *ui.Context) {
	// 1. 终端区域用编辑器底色，填满标题栏之下的空间。
	// 手测（Linux）：在终端里运行 vim、htop，鼠标点击、滚轮和拖选都应有反应，退出后回到 shell 提示符；
	// 拖动窗口或 Dock 边缘缩放后，在终端里执行 stty size，行列数应随可见区域变化，vim 的版面同步重排。
	t := kit.GetTheme(c)
	ui.Column(c).Fill().Gap(0).Background(t.Colors.Editor).Children(func() {
		tab := d.getSelectedTerminal()
		if tab == nil {
			kit.EmptyState(c, d.t("terminal.none"), d.t("terminal.none.detail"))
			return
		}
		// 2. 输入、尺寸变化回到这个标签。
		terminal := kit.Terminal(c.Key(tab.Key), tab.State, kit.TerminalOpt{
			OnInput:  func(data []byte) { d.postTerminalInput(tab, data) },
			OnResize: func(rows, cols int) { d.handleTerminalResize(tab, rows, cols) },
		})
		terminal.Bind(&tab.Handle)
		if tab.FocusRequested {
			tab.FocusRequested = false
			tab.Handle.Focus()
		}
		// 3. 进程已结束时，终端右下角浮出「重新启动」，在同一个标签里重开终端；浮层不占布局，终端的行列数不变，退出说明仍留在屏幕上。
		if tab.Phase == terminalExited {
			ui.Row(c).Absolute().Right(t.Space(3)).Bottom(t.Space(3)).Padding(t.Space(1)).Radius(t.Metrics.RadiusLarge).Background(t.Colors.Elevated).Border(1, t.Colors.Border).Children(func() {
				if kit.Button(c.Key("terminal-restart"), d.t("terminal.restart")).Clicked() {
					d.restartTerminal(tab)
				}
			})
		}
	})
}

// terminalTabsView 在 Dock 标题栏里绘制终端标签：点击选中，关闭按钮结束并移除。
func (d *Desktop) terminalTabsView(c *ui.Context) {
	// 1. 每个终端一个 Zed 风格标签，已结束的标题变淡；点了关闭的标签先记下。
	var closedTab *terminalTab
	titles := d.getTerminalTitles()
	for index, tab := range d.terminals {
		clicked, closed := kit.Tab(c.Key("terminal-tab-"+tab.Key), kit.TabOpt{Icon: kit.IconTerminal, Title: titles[index], Selected: tab.Key == d.selectedTerminal, Faded: tab.Phase == terminalExited, CloseLabel: d.t("terminal.close")})
		if clicked {
			d.selectedTerminal = tab.Key
			tab.FocusRequested = true
		}
		if closed {
			closedTab = tab
		}
	}
	// 2. 遍历结束后再从 d.terminals 删除。
	if closedTab != nil {
		d.closeTerminal(closedTab)
	}
}

// terminalActionsView 在 Dock 标题栏右端绘制新建终端按钮。
func (d *Desktop) terminalActionsView(c *ui.Context) {
	newTerminal := kit.IconButtonTip(c.Key("terminal-new"), kit.IconPlus, d.t("terminal.new"))
	if newTerminal.Clicked() {
		d.openTerminal()
	}
}

// getSelectedTerminal 返回当前选中的终端标签；没有终端时返回 nil。
func (d *Desktop) getSelectedTerminal() *terminalTab {
	return d.getTerminalTab(func(tab *terminalTab) bool { return tab.Key == d.selectedTerminal })
}

// getTerminalTab 返回第一个满足条件的终端标签；没有时返回 nil。
func (d *Desktop) getTerminalTab(matches func(tab *terminalTab) bool) *terminalTab {
	if index := slices.IndexFunc(d.terminals, matches); index >= 0 {
		return d.terminals[index]
	}
	return nil
}

// getTerminalTitle 返回标签标题：优先用 shell 通过转义序列设置的标题，否则是「Executor · 目录名」。
func (d *Desktop) getTerminalTitle(tab *terminalTab) string {
	if title := tab.State.Title(); title != "" {
		return title
	}
	if tab.Directory == "" {
		return tab.ExecutorID
	}
	return tab.ExecutorID + " · " + filepath.Base(tab.Directory)
}

// getTerminalTitles 返回与 d.terminals 等长的标签标题；同名标签从第二个起追加「 (n)」。
func (d *Desktop) getTerminalTitles() []string {
	titles := make([]string, len(d.terminals))
	for index, tab := range d.terminals {
		titles[index] = d.getTerminalTitle(tab)
	}
	return getUniqueTitles(titles)
}

// getTerminalLocation 返回新终端的 Executor 与起始目录：有对话时取对话的默认 Executor 及其工作目录，否则取新对话表单的选择。
func (d *Desktop) getTerminalLocation() (executor, directory string) {
	if d.conversation.ID == "" {
		return d.newExecutor, d.newDirectory
	}
	settings := d.conversation.ExecutorSettings
	executor = settings.DefaultExecutorID
	if executor == "" {
		executor = "local"
	}
	return executor, settings.WorkspaceDirectories[executor]
}

// openFirstTerminal 下 Dock 显示终端面板、已连接且还没有终端时自动新开一个。
func (d *Desktop) openFirstTerminal() {
	if d.shownBottomPanel != panelTerminal || len(d.terminals) > 0 || d.connectionState != runtimeclient.ConnectionConnected {
		return
	}
	d.openTerminal()
	if d.window != nil {
		d.window.Invalidate()
	}
}

// openTerminal 新增一个标签并在后台向 serve 请求终端。
func (d *Desktop) openTerminal() {
	// 1. 标签立即出现，终端先按 24×80 打开，布局后的真实尺寸由 handleTerminalOpened 补发。
	executor, directory := d.getTerminalLocation()
	d.terminalSequence++
	tab := &terminalTab{Key: fmt.Sprintf("terminal-%d", d.terminalSequence), ExecutorID: executor, Directory: directory, State: kit.NewTerminalState(24, 80), FocusRequested: true}
	d.terminals = append(d.terminals, tab)
	d.selectedTerminal = tab.Key
	d.startTerminal(tab)
}

// restartTerminal 在已结束的标签里重新打开终端：沿用标签的 Executor 和目录，换新的屏幕和终端 ID。
func (d *Desktop) restartTerminal(tab *terminalTab) {
	// 1. 旧屏幕释放，标签回到打开中；旧终端已结束，不会再有输出。
	tab.State.Close()
	tab.State = kit.NewTerminalState(24, 80)
	tab.TerminalID = ""
	tab.Phase = terminalOpening
	tab.FocusRequested = true
	d.startTerminal(tab)
}

// startTerminal 向 serve 请求标签所在的 Executor 与目录的终端；结果由 handleTerminalOpened 在主线程应用。
func (d *Desktop) startTerminal(tab *terminalTab) {
	// 1. 未连接时直接标为已结束，说明原因。
	client := d.client
	if client == nil || d.connectionState != runtimeclient.ConnectionConnected {
		d.setTerminalExited(tab, d.t("terminal.not.connected"), true)
		return
	}
	// 2. 后台发起请求，环境变量用本机的，让 local Executor 的 shell 与桌面启动环境一致。
	tab.OpenRequest++
	openRequest := tab.OpenRequest
	request := &runtimeclient.TerminalOpenRequest{ExecutorID: tab.ExecutorID, Directory: tab.Directory, Rows: 24, Cols: 80, Environment: os.Environ()}
	d.tasks.Go(func() {
		result, err := client.OpenTerminal(d.ctx, request)
		d.window.Update(func() { d.handleTerminalOpened(client, tab, openRequest, result, err) })
	})
}

// handleTerminalOpened 应用 terminal/open 的结果；标签在等待期间已被关闭、连接已断开或已重新启动时，结束刚打开的终端。
func (d *Desktop) handleTerminalOpened(client runtimeclient.Client, tab *terminalTab, openRequest uint64, result *runtimeprotocol.TerminalOpenResult, err error) {
	// 1. 标签已不在等待这次请求的结果：失败不再提示，成功则关掉服务端多出来的终端。
	if d.closed {
		return
	}
	if tab.Phase != terminalOpening || tab.OpenRequest != openRequest {
		if err == nil {
			d.tasks.Go(func() { _ = client.CloseTerminal(d.ctx, result.TerminalID) })
		}
		return
	}
	// 2. 失败的原因写进标签，让用户看到（例如 virtual Executor 不支持终端）。
	if err != nil {
		d.setTerminalExited(tab, d.getErrorText(err), true)
		return
	}
	// 3. 成功后登记终端 ID，并把布局后的真实行列数发给终端。
	tab.TerminalID = result.TerminalID
	tab.ExecutorID = result.ExecutorID
	tab.Directory = result.Directory
	tab.Phase = terminalRunning
	rows, cols := tab.State.Size()
	if err := client.ResizeTerminal(tab.TerminalID, uint16(rows), uint16(cols)); err != nil {
		d.showFailure(err)
	}
}

// postTerminalInput 把终端控件产生的字节交回主线程；模拟器的自动应答在后台 goroutine 产生，所以不能直接改 Desktop。
func (d *Desktop) postTerminalInput(tab *terminalTab, data []byte) {
	d.window.Update(func() { d.handleTerminalInput(tab, data) })
}

// handleTerminalInput 把键盘、粘贴或自动应答的字节写给运行中的终端；其他阶段的标签忽略输入。
func (d *Desktop) handleTerminalInput(tab *terminalTab, data []byte) {
	if d.closed || tab.Phase != terminalRunning {
		return
	}
	if err := d.client.WriteTerminal(tab.TerminalID, data); err != nil {
		d.showFailure(err)
	}
}

// handleTerminalResize 终端区域的行列数变化时通知运行中的终端；其他阶段只改本地模拟器，打开完成后会补发。
func (d *Desktop) handleTerminalResize(tab *terminalTab, rows, cols int) {
	if tab.Phase != terminalRunning {
		return
	}
	if err := d.client.ResizeTerminal(tab.TerminalID, uint16(rows), uint16(cols)); err != nil {
		d.showFailure(err)
	}
}

// handleTerminalOutput 把终端的新输出写进对应标签的屏幕。
func (d *Desktop) handleTerminalOutput(event *runtimeclient.TerminalOutput) {
	if tab := d.getTerminalTab(func(tab *terminalTab) bool { return tab.TerminalID == event.TerminalID }); tab != nil {
		tab.State.Write(event.Data)
	}
}

// handleTerminalExited 终端进程结束：标签保留，末尾写一行淡色的退出说明或错误。
func (d *Desktop) handleTerminalExited(event *runtimeclient.TerminalExited) {
	tab := d.getTerminalTab(func(tab *terminalTab) bool { return tab.TerminalID == event.TerminalID })
	if tab == nil {
		return
	}
	message := event.Error
	if message == "" {
		message = fmt.Sprintf(d.t("terminal.exited"), event.ExitCode)
	}
	d.setTerminalExited(tab, message, false)
}

// handleTerminalsDisconnected 连接断开后 serve 已结束本连接的全部终端，所有标签标为已结束。
func (d *Desktop) handleTerminalsDisconnected() {
	for _, tab := range d.terminals {
		if tab.Phase != terminalExited {
			d.setTerminalExited(tab, d.t("terminal.disconnected"), false)
		}
	}
}

// setTerminalExited 标签标为已结束并在屏幕末尾写一行说明；失败说明用红色，其余用淡色。
func (d *Desktop) setTerminalExited(tab *terminalTab, message string, failed bool) {
	tab.Phase = terminalExited
	style := "\x1b[2m"
	if failed {
		style = "\x1b[31m"
	}
	tab.State.Write([]byte("\r\n" + style + "[" + message + "]\x1b[0m\r\n"))
}

// closeTerminal 关闭标签：运行中的终端在后台请求 serve 结束，选中项落到相邻标签。
func (d *Desktop) closeTerminal(tab *terminalTab) {
	// 1. 先从列表移除，终端控件下一帧不再绘制它。
	index := slices.Index(d.terminals, tab)
	if index < 0 {
		return
	}
	d.terminals = slices.Delete(d.terminals, index, index+1)
	if d.selectedTerminal == tab.Key {
		d.selectedTerminal = ""
		if len(d.terminals) > 0 {
			next := d.terminals[min(index, len(d.terminals)-1)]
			d.selectedTerminal, next.FocusRequested = next.Key, true
		}
	}
	// 2. 运行中的终端请求 serve 结束；打开中的终端等结果回来时由 handleTerminalOpened 结束。
	if tab.Phase == terminalRunning {
		client, terminalID := d.client, tab.TerminalID
		d.tasks.Go(func() {
			if err := client.CloseTerminal(d.ctx, terminalID); err != nil && !errors.Is(err, d.ctx.Err()) {
				d.window.Update(func() { d.showFailure(err) })
			}
		})
	}
	tab.Phase = terminalExited
	tab.State.Close()
}

// closeTerminalStates 窗口关闭后释放所有标签的终端屏幕；serve 在连接关闭时结束它们的进程。
func (d *Desktop) closeTerminalStates() {
	for _, tab := range d.terminals {
		tab.State.Close()
	}
}
