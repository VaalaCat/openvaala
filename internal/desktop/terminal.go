package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/egoist/mygo/ui"
)

const (
	// terminalReplayQuietPeriod 是回放的最后一批输出到达后继续丢弃终端输入的时长。
	terminalReplayQuietPeriod = 100 * time.Millisecond
	// terminalRedrawInterval 是重画终端时两次改尺寸之间的间隔。
	terminalRedrawInterval = 100 * time.Millisecond
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
		// 3. 进程已结束时，终端右下角浮出「重新启动」，在同一个标签里重开终端；连接断开而等待重连时同一位置显示说明；浮层不占布局，终端的行列数不变。
		isWaitingForConnection := tab.Phase == terminalReconnecting && d.connectionState != runtimeclient.ConnectionConnected
		if tab.Phase == terminalExited || isWaitingForConnection {
			ui.Row(c).Absolute().Right(t.Space(3)).Bottom(t.Space(3)).Padding(t.Space(1)).Radius(t.Metrics.RadiusLarge).Background(t.Colors.Elevated).Border(1, t.Colors.Border).Children(func() {
				if isWaitingForConnection {
					t.TextRole(ui.Text(c, d.t("terminal.reconnecting")), kit.TextCaption).TextColor(t.Colors.Muted).Margin(t.Space(1), t.Space(2))
					return
				}
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
	return getWorkLocation(d.conversation)
}

// openFirstTerminal 下 Dock 显示终端面板、已连接、运行中的终端列表已取得且还没有终端标签时自动新开一个。
func (d *Desktop) openFirstTerminal() {
	if d.shownBottomPanel != panelTerminal || !d.terminalsKnown || len(d.terminals) > 0 || d.connectionState != runtimeclient.ConnectionConnected {
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
	tab.ReceivedCursor = 0
	tab.IsReplaying = false
	tab.WasTruncated = false
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
	d.openingTerminalRequests++
	request := &runtimeclient.TerminalOpenRequest{ExecutorID: tab.ExecutorID, Directory: tab.Directory, Rows: 24, Cols: 80, Environment: os.Environ(), ConversationID: d.conversation.ID}
	d.tasks.Go(func() {
		result, err := client.OpenTerminal(d.ctx, request)
		d.window.Update(func() { d.handleTerminalOpened(client, tab, openRequest, result, err) })
	})
}

// handleTerminalOpened 应用 terminal/open 的结果；标签在等待期间已被关闭或已重新启动时，结束刚打开的终端。
func (d *Desktop) handleTerminalOpened(client runtimeclient.Client, tab *terminalTab, openRequest uint64, result *runtimeprotocol.TerminalOpenResult, err error) {
	// 1. 标签已不在等待这次请求的结果：失败不再提示，成功则关掉服务端多出来的终端；无论哪种结果，打开完成后都重新同步运行中的终端。
	if d.closed {
		return
	}
	d.openingTerminalRequests--
	defer d.syncRunningTerminals()
	if tab.Phase != terminalOpening || tab.OpenRequest != openRequest {
		if err == nil {
			d.requestTerminalClose(client, result.TerminalID)
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

// handleTerminalInput 把键盘、粘贴或自动应答的字节写给运行中的终端；回放期间和其他阶段的标签忽略输入。
func (d *Desktop) handleTerminalInput(tab *terminalTab, data []byte) {
	if d.closed || tab.Phase != terminalRunning || tab.IsReplaying {
		return
	}
	if err := d.client.WriteTerminal(tab.TerminalID, data); err != nil {
		d.showFailure(err)
	}
}

// handleTerminalResize 终端区域的行列数变化时通知运行中的终端；回放期间和其他阶段只改本地模拟器，回放结束或打开完成后会补发。
func (d *Desktop) handleTerminalResize(tab *terminalTab, rows, cols int) {
	if tab.Phase != terminalRunning || tab.IsReplaying {
		return
	}
	if err := d.client.ResizeTerminal(tab.TerminalID, uint16(rows), uint16(cols)); err != nil {
		d.showFailure(err)
	}
}

// handleTerminalOutput 把终端的输出写进对应标签的屏幕并记下输出位置；回放的最后一批输出到达后，再等 100ms 恢复发送输入。
func (d *Desktop) handleTerminalOutput(event *runtimeclient.TerminalOutput) {
	tab := d.getTerminalTab(func(tab *terminalTab) bool { return tab.TerminalID == event.TerminalID })
	if tab == nil {
		return
	}
	// 1. 请求的位置已被 serve 丢弃：屏幕上的旧内容接不上，换一个空屏幕从最早保留处重放。
	if event.Truncated {
		tab.WasTruncated = true
		if tab.ReceivedCursor > 0 {
			rows, cols := tab.State.Size()
			tab.State.Close()
			tab.State = kit.NewTerminalState(rows, cols)
		}
	}
	// 2. 写入输出并记下下一次续读的位置。
	tab.State.Write(event.Data)
	tab.ReceivedCursor = event.NextCursor
	// 3. 已有输出发完后，等 100ms 再恢复发送输入。
	if !event.HasMore && tab.IsReplaying {
		d.scheduleTerminalReplayFinish(tab)
	}
}

// scheduleTerminalReplayFinish 回放的最后一批输出已写入屏幕：等待 100ms 后交回主线程结束回放。
func (d *Desktop) scheduleTerminalReplayFinish(tab *terminalTab) {
	d.tasks.Go(func() {
		select {
		case <-time.After(terminalReplayQuietPeriod):
		case <-d.ctx.Done():
			return
		}
		d.window.Update(func() { d.handleTerminalReplayFinished(tab) })
	})
}

// handleTerminalReplayFinished 回放结束，恢复发送输入。屏幕尺寸与 PTY 不同时发屏幕尺寸；尺寸相同但回放被截断时改两次尺寸，让全屏程序重画。
func (d *Desktop) handleTerminalReplayFinished(tab *terminalTab) {
	// 1. 重复的通知、已关闭或断线后的标签忽略。
	if d.closed || !tab.IsReplaying || tab.Phase != terminalRunning {
		return
	}
	tab.IsReplaying = false
	// 2. 按屏幕尺寸与 PTY 尺寸的比较决定补发尺寸还是重画。
	rows, cols := tab.State.Size()
	switch {
	case rows != tab.AttachedRows || cols != tab.AttachedCols:
		d.handleTerminalResize(tab, rows, cols)
	case tab.WasTruncated:
		client, terminalID := d.client, tab.TerminalID
		d.tasks.Go(func() { d.redrawTerminal(client, terminalID, rows, cols) })
	}
}

// redrawTerminal 先把终端行数减一，隔 100ms 再改回，让全屏程序收到两次尺寸变化后重画；阻塞到两次都发出，在后台 goroutine 中调用。
func (d *Desktop) redrawTerminal(client runtimeclient.Client, terminalID string, rows, cols int) {
	err := client.ResizeTerminal(terminalID, uint16(max(rows-1, 1)), uint16(cols))
	if err == nil {
		select {
		case <-time.After(terminalRedrawInterval):
		case <-d.ctx.Done():
			return
		}
		err = client.ResizeTerminal(terminalID, uint16(rows), uint16(cols))
	}
	if err != nil {
		d.window.Update(func() { d.showFailure(err) })
	}
}

// resetTerminal 请求 serve 丢掉选中终端保留的输出，成功后重画；只对运行中的终端生效。
func (d *Desktop) resetTerminal(tab *terminalTab) {
	if tab == nil || tab.Phase != terminalRunning {
		return
	}
	client, terminalID := d.client, tab.TerminalID
	rows, cols := tab.State.Size()
	d.tasks.Go(func() {
		if err := client.ResetTerminal(d.ctx, terminalID); err != nil {
			if !errors.Is(err, d.ctx.Err()) {
				d.window.Update(func() { d.showFailure(err) })
			}
			return
		}
		d.redrawTerminal(client, terminalID, rows, cols)
	})
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

// handleTerminalsDisconnected 连接断开后终端仍在 serve 上运行：运行中的标签进入重连，不写结束说明，连上后从已收到的位置接回。
func (d *Desktop) handleTerminalsDisconnected() {
	for _, tab := range d.terminals {
		if tab.Phase == terminalRunning {
			tab.Phase = terminalReconnecting
		}
	}
}

// attachReconnectingTerminals 连上 serve 后，把所有等待重连的标签接回各自的终端。
func (d *Desktop) attachReconnectingTerminals() {
	for _, tab := range d.terminals {
		if tab.Phase == terminalReconnecting {
			d.attachTerminal(tab)
		}
	}
}

// attachTerminal 在后台请求 serve 把本连接接入标签的终端，从已收到的输出位置续读；结果由 handleTerminalAttached 在主线程应用。
func (d *Desktop) attachTerminal(tab *terminalTab) {
	// 1. 标签等待接入并开始回放，回放期间终端的输入和尺寸变化不发给 serve。
	tab.Phase = terminalReconnecting
	tab.IsReplaying = true
	tab.WasTruncated = false
	// 2. 后台发起请求。
	client, terminalID, cursor := d.client, tab.TerminalID, tab.ReceivedCursor
	d.tasks.Go(func() {
		result, err := client.AttachTerminal(d.ctx, terminalID, cursor)
		d.window.Update(func() { d.handleTerminalAttached(tab, result, err) })
	})
}

// handleTerminalAttached 应用 terminal/attach 的结果：成功后按 PTY 当前尺寸准备屏幕并开始接收输出；终端已不存在时标签标为已结束；连接断开时继续等待重连。
func (d *Desktop) handleTerminalAttached(tab *terminalTab, result *runtimeprotocol.TerminalAttachResult, err error) {
	// 1. 标签已不在等待这次结果（已关闭或终端已结束），或结果到达时连接已断开（连上后重新接入）时忽略。
	if d.closed || tab.Phase != terminalReconnecting || d.connectionState != runtimeclient.ConnectionConnected {
		return
	}
	// 2. 终端已不存在：标为已结束；连接断开：留在重连中等下次连上；其他原因写进标签。
	if err != nil {
		var responseErr *runtimeclient.ResponseError
		switch {
		case errors.As(err, &responseErr) && responseErr.Code == runtimeprotocol.ErrorCodeTerminalNotFound:
			d.setTerminalExited(tab, d.t("terminal.ended"), false)
		case errors.Is(err, runtimeclient.ErrConnectionLost):
		default:
			d.setTerminalExited(tab, d.getErrorText(err), true)
		}
		return
	}
	// 3. 新屏幕按 PTY 当前尺寸回放；已有内容的屏幕接着写，回放结束时再比较尺寸。
	tab.ExecutorID = result.ExecutorID
	tab.Directory = result.Directory
	tab.AttachedRows, tab.AttachedCols = int(result.Rows), int(result.Cols)
	if tab.ReceivedCursor == 0 {
		tab.State.Close()
		tab.State = kit.NewTerminalState(tab.AttachedRows, tab.AttachedCols)
	}
	tab.Phase = terminalRunning
}

// handleTerminalsChanged 记下 serve 上运行中的终端列表，把没有标签的终端加成 Dock 标签；列表由未知变为已知时，没有任何标签则按需新开第一个。
func (d *Desktop) handleTerminalsChanged(event *runtimeclient.TerminalsChanged) {
	wasKnown := d.terminalsKnown
	d.terminalsKnown = event.IsKnown
	if !event.IsKnown {
		return
	}
	d.runningTerminals = event.Terminals
	d.syncRunningTerminals()
	if !wasKnown {
		d.openFirstTerminal()
	}
}

// syncRunningTerminals 把运行中而没有标签的终端加成 Dock 标签并接入；有 terminal/open 请求在进行时跳过，打开完成后由 handleTerminalOpened 重新执行。
func (d *Desktop) syncRunningTerminals() {
	// 1. 列表未知时不动标签；打开请求还没返回，它的终端会被误认为没有标签。
	if !d.terminalsKnown || d.openingTerminalRequests > 0 {
		return
	}
	// 2. 没有标签、也不在关闭中的终端加成标签并接入，不抢当前选中。
	for _, item := range d.runningTerminals {
		if slices.Contains(d.closingTerminalIDs, item.TerminalID) || d.getTerminalTab(func(tab *terminalTab) bool { return tab.TerminalID == item.TerminalID }) != nil {
			continue
		}
		d.terminalSequence++
		tab := &terminalTab{Key: fmt.Sprintf("terminal-%d", d.terminalSequence), TerminalID: item.TerminalID, ExecutorID: item.ExecutorID, Directory: item.Directory, State: kit.NewTerminalState(24, 80), Phase: terminalReconnecting}
		d.terminals = append(d.terminals, tab)
		if d.selectedTerminal == "" {
			d.selectedTerminal = tab.Key
		}
		d.attachTerminal(tab)
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

// closeTerminal 关闭标签：运行中和等待重连的终端在后台请求 serve 结束，选中项落到相邻标签。
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
	// 2. 运行中和等待重连的终端请求 serve 结束；打开中的终端等结果回来时由 handleTerminalOpened 结束。
	if tab.Phase == terminalRunning || tab.Phase == terminalReconnecting {
		d.requestTerminalClose(d.client, tab.TerminalID)
	}
	tab.Phase = terminalExited
	tab.State.Close()
}

// requestTerminalClose 在后台请求 serve 结束终端；请求进行中，运行列表里的这个终端不会被加回成标签。
func (d *Desktop) requestTerminalClose(client runtimeclient.Client, terminalID string) {
	d.closingTerminalIDs = append(d.closingTerminalIDs, terminalID)
	d.tasks.Go(func() {
		err := client.CloseTerminal(d.ctx, terminalID)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.closingTerminalIDs = slices.DeleteFunc(d.closingTerminalIDs, func(closingID string) bool { return closingID == terminalID })
			if err != nil && !errors.Is(err, d.ctx.Err()) {
				d.showFailure(err)
			}
		})
	})
}

// closeTerminalStates 窗口关闭后释放所有标签的终端屏幕；serve 上的终端继续运行，下次启动时接回。
func (d *Desktop) closeTerminalStates() {
	for _, tab := range d.terminals {
		tab.State.Close()
	}
}
