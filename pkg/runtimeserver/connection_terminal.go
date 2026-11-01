package runtimeserver

import (
	"context"
	"sync"
	"time"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

const (
	// terminalShellCommand 是终端里运行的命令（经 bash -c，本机和 SSH 相同）：先保证 HOME 有值（没有时取当前用户的主目录，再退到 /tmp），
	// 再按 $SHELL、fish、zsh、bash、sh、mksh 的顺序选第一个能找到的 shell，以登录 shell 的方式替换当前进程；都找不到时退出码为 127。
	terminalShellCommand = `if [ -z "$HOME" ]; then HOME=$(cd ~ 2>/dev/null && pwd); fi; [ -n "$HOME" ] || HOME=/tmp; export HOME
for shell in "$SHELL" fish zsh bash sh mksh; do
  shell_path=$(command -v "$shell" 2>/dev/null) && [ -x "$shell_path" ] && exec "$shell_path" -l
done
echo "no usable shell found" >&2
exit 127`
	// terminalOutputBatchInterval 是终端输出合批的等待时间：第一块输出到达后最多再等这么久就发出。
	terminalOutputBatchInterval = 16 * time.Millisecond
	// terminalOutputBatchBytes 是一批终端输出的字节数上限：攒到这个数立即发出。
	terminalOutputBatchBytes = 32 << 10
	// terminalOutputBufferSize 是进程输出已写入、转发任务还没取走的块数上限；满了进程输出停在写入上，终端不再读新输出。
	terminalOutputBufferSize = 16
	// terminalInputBufferSize 是客户端输入已收到、写入任务还没写进终端的块数上限；满了的新输入丢弃。
	terminalInputBufferSize = 256
	// terminalReleaseTimeout 是结束一个终端进程的等待上限：连接结束时所有终端共用，主动关闭时单个终端用。
	terminalReleaseTimeout = 5 * time.Second
)

// connectionTerminal 是一条连接打开的一个终端：持有为它新建的独立工作区和 TTY 进程，生命周期不超过打开它的连接。
// 进程输出经 Write 交给转发任务合批后发给客户端；客户端输入经 handleInput 交给写入任务写进终端。
type connectionTerminal struct {
	id         string
	connection *serverConnection
	workspace  workspace.Workspace
	// process 在 StartProcess 成功后设置，之后只读。
	process workspace.Process
	// ctx 是终端的生命周期，随连接取消；cancel 在转发任务结束时调用，让写入任务退出。
	ctx    context.Context
	cancel context.CancelFunc
	// output 是进程输出：Write 是唯一发送方，转发任务是唯一接收方。
	output chan []byte
	// input 是客户端输入：handleInput 是唯一发送方，写入任务是唯一接收方。
	input chan []byte
}

// run 转发终端输出，进程结束后通知客户端、移除终端并关闭工作区；由 terminal/open 的响应入队后启动。
func (t *connectionTerminal) run() {
	// 1. 启动写入任务和等待进程结束的任务，返回前等待它们。
	var tasks sync.WaitGroup
	var exitCode int
	var exitErr error
	processExited := make(chan struct{})
	tasks.Go(t.writeInput)
	tasks.Go(func() {
		exitCode, exitErr = t.process.Wait()
		close(processExited)
	})
	// 2. 合批转发输出，进程结束后发出最后一批；发送失败说明连接已结束，由连接结束时的释放流程终止进程。
	t.forwardOutput(processExited)
	<-processExited
	t.cancel()
	tasks.Wait()
	// 3. 通知客户端终端已结束，再从连接上移除并关闭工作区。
	exited := &runtimeprotocol.TerminalExitedParams{
		TerminalID: t.id,
		ExitCode:   exitCode,
	}
	if exitErr != nil {
		exited.Error = exitErr.Error()
	}
	_ = t.connection.sendNotification("terminal/exited", exited)
	t.connection.terminals.Delete(t.id)
	t.closeWorkspace()
}

// forwardOutput 把进程输出按时间或大小合批后发给客户端，进程结束并取完输出后返回。
func (t *connectionTerminal) forwardOutput(processExited <-chan struct{}) {
	// 1. 等待输出、合批定时和进程结束；第一块输出到达时启动合批定时。
	var batch []byte
	timer := time.NewTimer(terminalOutputBatchInterval)
	timer.Stop()
	defer timer.Stop()
	var timerChannel <-chan time.Time
	for {
		select {
		case chunk := <-t.output:
			batch = append(batch, chunk...)
			if len(batch) < terminalOutputBatchBytes {
				if timerChannel == nil {
					timer.Reset(terminalOutputBatchInterval)
					timerChannel = timer.C
				}
				continue
			}
		case <-timerChannel:
		case <-processExited:
			// 2. 进程结束时输出已全部写入通道，取完后连同未发出的一批一起发出。
			for len(t.output) > 0 {
				batch = append(batch, <-t.output...)
			}
			_ = t.sendOutput(batch)
			return
		case <-t.ctx.Done():
			return
		}
		// 3. 批满或定时到：发出一批，失败时说明连接已结束。
		timerChannel = nil
		timer.Stop()
		if t.sendOutput(batch) != nil {
			return
		}
		batch = nil
	}
}

// sendOutput 把一批输出作为 terminal/output 发给客户端，批为空时不发。
func (t *connectionTerminal) sendOutput(batch []byte) error {
	// 1. 空批直接返回；否则发送通知。
	if len(batch) == 0 {
		return nil
	}
	return t.connection.sendNotification("terminal/output", &runtimeprotocol.TerminalOutputParams{
		TerminalID: t.id,
		Data:       batch,
	})
}

// writeInput 把客户端输入按到达顺序写进终端，终端结束或连接结束时返回。
func (t *connectionTerminal) writeInput() {
	// 1. 逐块写入；写入失败说明终端已结束，记 Debug 后丢弃该块，由进程结束流程收尾。
	for {
		select {
		case data := <-t.input:
			if _, err := t.process.WriteTerminalInput(data); err != nil {
				telemetry.GetLogger(t.ctx).Debug("terminal input dropped", "terminal.id", t.id, "error", err)
			}
		case <-t.ctx.Done():
			return
		}
	}
}

// Write 接收进程输出并交给转发任务；终端或连接结束时返回错误，让输出复制停止。
func (t *connectionTerminal) Write(data []byte) (int, error) {
	// 1. 复制一份（调用方会复用缓冲）后入队，ctx 结束时放弃。
	select {
	case t.output <- append([]byte(nil), data...):
		return len(data), nil
	case <-t.ctx.Done():
		return 0, context.Cause(t.ctx)
	}
}

// handleInput 收到客户端的终端输入：入队交给写入任务，队列满时丢弃，不阻塞连接。
func (t *connectionTerminal) handleInput(data []byte) {
	// 1. 队列有空位时入队，否则记 Warn 并丢弃。
	select {
	case t.input <- data:
	default:
		telemetry.GetLogger(t.ctx).Warn("terminal input dropped", "terminal.id", t.id, "reason", "input queue is full")
	}
}

// handleResize 收到客户端的终端尺寸变化：直接修改，失败记 Warn。
func (t *connectionTerminal) handleResize(rows, cols uint16) {
	// 1. 修改 TTY 行列数；进程已结束等失败只影响显示，记 Warn。
	if err := t.process.ResizeTerminal(rows, cols); err != nil {
		telemetry.GetLogger(t.ctx).Warn("terminal resize failed", "terminal.id", t.id, "error", err)
	}
}

// terminate 结束终端进程；ctx 到期时直接断开 SSH 连接，让卡住的结束请求返回，结束失败时放手进程。
func (t *connectionTerminal) terminate(ctx context.Context) error {
	// 1. ctx 到期时关闭工作区，打断还在等待的 Terminate。
	stop := context.AfterFunc(ctx, t.closeWorkspace)
	defer stop()
	// 2. 结束整个进程组；失败时放手进程，让 Wait 尽快返回。
	err := t.process.Terminate()
	if err != nil {
		_ = t.process.Close()
	}
	return err
}

// closeWorkspace 关闭为终端新建的工作区：SSH 工作区断开连接，本机工作区没有需要关闭的资源；可重复调用。
func (t *connectionTerminal) closeWorkspace() {
	// 1. 工作区是 SSH 类型时关闭，失败记 Warn。
	if sshWorkspace, ok := t.workspace.(workspace.SSHWorkspace); ok {
		if err := sshWorkspace.Close(); err != nil {
			telemetry.GetLogger(t.ctx).Warn("close terminal workspace failed", "terminal.id", t.id, "error", err)
		}
	}
}
