package runtimeserver

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/terminal"
)

const (
	// terminalReadWait 是接入后等待终端产生新输出的单次上限：到时没有新输出就再读一次。
	terminalReadWait = 30 * time.Second
	// terminalInputBufferSize 是客户端输入已收到、writeTerminal 还没写进终端的块数上限；满了的新输入丢弃。
	terminalInputBufferSize = 256
	// terminalCloseTimeout 是 terminal/close 等待终端结束的上限。
	terminalCloseTimeout = 5 * time.Second
)

// terminalAttachment 是一条连接接入的一个终端：按位置把输出发给客户端，把客户端输入写进终端，不拥有进程。
type terminalAttachment struct {
	terminal   terminal.Terminal
	terminalID string
	connection *serverConnection
	// windowSize 是这条连接最新的窗口尺寸，接入时取 PTY 当前尺寸；handleResize 覆盖，writeTerminal 读取。
	windowSize atomic.Pointer[terminalWindowSize]
	// windowSizeChanged 合并尺寸变化的唤醒，容量 1。
	windowSizeChanged chan struct{}
	// input 是客户端输入：handleInput 是唯一发送方，writeTerminal 是唯一接收方，满了的新输入丢弃。
	input chan []byte
	// ctx 随连接取消，重复接入同一终端时也取消。
	ctx    context.Context
	cancel context.CancelFunc
	// replayEnd 是接入时已产生的输出总字节数，取自第一页的 EndCursor；发到这个位置回放就结束。
	replayEnd uint64
	// done 在 run 返回时关闭。
	done chan struct{}
}

// terminalWindowSize 是客户端窗口的行列数。
type terminalWindowSize struct {
	rows, cols uint16
}

// newTerminalAttachment 创建这条连接对一个终端的接入，回放终点取自第一页的 EndCursor。
func newTerminalAttachment(connection *serverConnection, runtimeTerminal terminal.Terminal, firstOutput terminal.TerminalOutput) *terminalAttachment {
	// 1. 接入的 ctx 随连接取消，创建输入队列和尺寸唤醒通道；窗口尺寸先取 PTY 当前尺寸，客户端没发过 resize 时输入也会把 PTY 改回这个尺寸。
	ctx, cancel := context.WithCancel(connection.connectionCtx)
	attachment := &terminalAttachment{
		terminal:          runtimeTerminal,
		terminalID:        firstOutput.Info.ID,
		connection:        connection,
		windowSizeChanged: make(chan struct{}, 1),
		input:             make(chan []byte, terminalInputBufferSize),
		ctx:               ctx,
		cancel:            cancel,
		replayEnd:         firstOutput.EndCursor,
		done:              make(chan struct{}),
	}
	attachment.windowSize.Store(&terminalWindowSize{rows: firstOutput.Info.Rows, cols: firstOutput.Info.Cols})
	return attachment
}

// run 发送第一页输出并继续转发，同时把输入写进终端；终端结束时发 terminal/exited，接入被取消或连接结束时安静返回。
func (a *terminalAttachment) run(firstOutput terminal.TerminalOutput) {
	// 1. 启动 writeTerminal；返回前把自己从连接上移除，取消并等待 writeTerminal，再关闭 done。
	var tasks sync.WaitGroup
	tasks.Go(a.writeTerminal)
	defer func() {
		a.connection.terminalAttachments.DeleteIf(a.terminalID, func(registered *terminalAttachment) bool { return registered == a })
		a.cancel()
		tasks.Wait()
		close(a.done)
	}()

	// 2. 第一页即使为空也发，之后的空页不发。
	output := firstOutput
	for isFirstPage := true; ; isFirstPage = false {
		if isFirstPage || output.Text != "" {
			if !a.connection.send(a.ctx, &runtimeprotocol.JSONRPCMessage{
				Method: "terminal/output",
				Params: &runtimeprotocol.TerminalOutputParams{
					TerminalID: a.terminalID,
					Data:       []byte(output.Text),
					NextCursor: output.NextCursor,
					Truncated:  output.Truncated,
					HasMore:    output.NextCursor < a.replayEnd,
				},
			}) {
				return
			}
		}

		// 3. 终端已结束且输出已收全时发 terminal/exited，接入到此为止。
		if output.Info.Status != terminal.TerminalStatusRunning && !output.HasMore {
			exited := &runtimeprotocol.TerminalExitedParams{TerminalID: a.terminalID, ExitCode: -1}
			if output.Info.ExitCode != nil {
				exited.ExitCode = *output.Info.ExitCode
			}
			if output.Info.Err != nil {
				exited.Error = output.Info.Err.Error()
			}
			a.connection.send(a.ctx, &runtimeprotocol.JSONRPCMessage{Method: "terminal/exited", Params: exited})
			return
		}

		// 4. 从下一个位置继续读，等待新输出；接入被取消时返回。
		var err error
		output, err = a.terminal.ReadOutput(a.ctx, output.NextCursor, terminalReadWait)
		if err != nil {
			if a.ctx.Err() == nil {
				telemetry.GetLogger(a.ctx).Warn("terminal output read failed", "terminal.id", a.terminalID, "error", err)
			}
			return
		}
	}
}

// writeTerminal 是这个接入的后台循环：尺寸变化时调 Resize；收到输入时先调 Resize，再调 WriteInput。
func (a *terminalAttachment) writeTerminal() {
	for {
		// 1. 等尺寸变化、输入或接入结束。
		select {
		case <-a.ctx.Done():
			return
		case <-a.windowSizeChanged:
			a.resizeTerminal()
		case data := <-a.input:
			// 2. 先按这条连接最新的窗口尺寸改 PTY，再写入输入；失败记 Warn，丢弃该块。
			a.resizeTerminal()
			if _, err := a.terminal.WriteInput(a.ctx, string(data)); err != nil && a.ctx.Err() == nil {
				telemetry.GetLogger(a.ctx).Warn("terminal input dropped", "terminal.id", a.terminalID, "error", err)
			}
		}
	}
}

// resizeTerminal 把终端改成这条连接最新的窗口尺寸，失败记 Warn。
func (a *terminalAttachment) resizeTerminal() {
	// 1. 读取最新窗口尺寸，调 Resize。
	size := a.windowSize.Load()
	if err := a.terminal.Resize(a.ctx, size.rows, size.cols); err != nil && a.ctx.Err() == nil {
		telemetry.GetLogger(a.ctx).Warn("terminal resize failed", "terminal.id", a.terminalID, "error", err)
	}
}

// handleResize 记下最新窗口尺寸并唤醒 writeTerminal，不等修改完成。
func (a *terminalAttachment) handleResize(rows, cols uint16) {
	// 1. 覆盖最新尺寸，中间尺寸可以合并；唤醒已经挂起时不重复。
	a.windowSize.Store(&terminalWindowSize{rows: rows, cols: cols})
	select {
	case a.windowSizeChanged <- struct{}{}:
	default:
	}
}

// handleInput 把输入入队，不等写入完成；队列满时丢弃。
func (a *terminalAttachment) handleInput(data []byte) {
	// 1. 队列有空位时入队，否则记 Warn 并丢弃。
	select {
	case a.input <- data:
	default:
		telemetry.GetLogger(a.ctx).Warn("terminal input dropped", "terminal.id", a.terminalID, "reason", "input queue is full")
	}
}

// attachTerminal 登记本连接对一个终端的接入，响应入队之后才启动转发，客户端先拿到响应再收到输出。
func (c *serverConnection) attachTerminal(runtimeTerminal terminal.Terminal, firstOutput terminal.TerminalOutput) {
	// 1. 创建接入并登记，让后续的 terminal/input、terminal/resize 找得到它。
	attachment := newTerminalAttachment(c, runtimeTerminal, firstOutput)
	c.terminalAttachments.Store(attachment.terminalID, attachment)
	// 2. 响应入队后启动转发。
	c.startAfterResponse(func() { attachment.run(firstOutput) })
}

// cancelTerminalAttachment 取消本连接对一个终端的接入并等待它收尾；没有接入时直接返回。
func (c *serverConnection) cancelTerminalAttachment(ctx context.Context, terminalID string) {
	// 1. 移除登记，不存在时直接返回。
	attachment, found := c.terminalAttachments.LoadAndDelete(terminalID)
	if !found {
		return
	}
	// 2. 取消接入，等待转发收尾；连接结束时停止等待。
	attachment.cancel()
	select {
	case <-attachment.done:
	case <-ctx.Done():
	}
}
