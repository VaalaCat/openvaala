package terminal

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// ── TerminalManager 请求 ──

// TerminalRequest 是启动一条命令要的全部事实；实际 Workspace 随请求传入，管理器不绑定环境。
type TerminalRequest struct {
	EnvironmentID string
	Workspace     workspace.Workspace
	Command       string
	// Directory 是工作目录，绝对路径，由工具按工作区根目录算好。
	Directory string
	// Interactive 是否用 PTY 启动命令？是时 stdout 和 stderr 合并成一条输出流，之后可以写入输入。
	Interactive bool
	// Timeout 是从启动成功起的总运行时限，0 表示不限。
	Timeout time.Duration
	// Rows、Cols 是 PTY 初始行列数，为零时按 24×80；非交互命令忽略。
	Rows, Cols uint16
	// OwnsWorkspace 这条命令是否独占 Workspace？是时调用 StartTerminal 后工作区由本包关闭，调用方不再关闭。
	OwnsWorkspace bool
}

// ── Terminal 状态 ──

// TerminalStatus 是命令的运行状态。
type TerminalStatus string

const (
	TerminalStatusRunning TerminalStatus = "running"
	TerminalStatusExited  TerminalStatus = "exited"
	TerminalStatusFailed  TerminalStatus = "failed"
)

// TerminalStopReason 是命令被停下的原因；命令自己结束时为空。
type TerminalStopReason string

const (
	// TerminalStoppedByRequest 是调用方通过 Terminal.Stop 停下的，例如 Agent 的 stop_command 或用户关闭终端。
	TerminalStoppedByRequest TerminalStopReason = "stopped"
	TerminalStoppedByTimeout TerminalStopReason = "timed_out"
	// TerminalStoppedByManager 是 TerminalManager 停止（Run 的 ctx 结束）时停下的；值 session_closed 会进工具结果和已保存的记录，保持不变。
	TerminalStoppedByManager TerminalStopReason = "session_closed"
)

// ── Terminal 结果 ──

// TerminalInfo 是一条命令的信息；结束前只有启动时的字段和 running 状态。
type TerminalInfo struct {
	ID            string
	EnvironmentID string
	Command       string
	Directory     string
	// Interactive 是否用 PTY 启动命令？
	Interactive bool
	// Rows、Cols 是 PTY 行列数，非交互命令为零；GetInfo 在命令结束前返回初始值，TerminalOutput.Info 里是当前值。
	Rows, Cols uint16
	// StartedAt 是命令开始启动的时间。
	StartedAt time.Time
	Status    TerminalStatus
	// ExitCode 只有 exited 有；被信号终止时为 -1。
	ExitCode   *int
	StopReason TerminalStopReason
	// Err 是 failed 的原因，例如 SSH 断开后无法确认远端已停止。
	Err error
}

// TerminalOutput 是一次读取的结果；Info 和输出出自同一时刻，状态已结束时输出也已收全。
type TerminalOutput struct {
	Info TerminalInfo
	// Text 从实际起点起最多 64 KiB，不切断 UTF-8 字符。
	Text       string
	NextCursor uint64
	// EndCursor 是读取这一刻已产生的输出总字节数，包括已被丢弃的部分。
	EndCursor uint64
	// HasMore 读取时还有保留的输出没返回？
	HasMore bool
	// Truncated 请求的 cursor 已被丢弃，Text 从保留的最早位置开始？
	Truncated bool
}

// ── TerminalManager 错误 ──

var (
	ErrTerminalNotFound       = errors.New("command is not available in this session and executor")
	ErrTerminalLimitReached   = errors.New("too many unfinished commands; stop one before starting another")
	ErrTerminalManagerStopped = errors.New("terminal manager has stopped")
)

// ── Terminal 错误 ──

var (
	ErrInvalidCursor    = errors.New("cursor is after the latest output")
	ErrInputUnavailable = errors.New("command was not started with interactive=true")
	ErrTerminalFinished = errors.New("command has finished")
)

// invalidCursorError 是 ReadOutput 的 cursor 越界时的错误：文本写明 cursor 和当时已产生的输出总字节数，errors.Is 匹配 ErrInvalidCursor。
type invalidCursorError struct {
	cursor uint64
	end    uint64
}

// Error 返回输出游标越界的错误文本。
func (e *invalidCursorError) Error() string {
	// 1. 写入请求游标和已产生输出的总字节数。
	return fmt.Sprintf("cursor %d is after the latest output %d", e.cursor, e.end)
}

// Unwrap 返回游标越界的公共错误。
func (e *invalidCursorError) Unwrap() error {
	// 1. 返回供 errors.Is 匹配的 ErrInvalidCursor。
	return ErrInvalidCursor
}

// ── Terminal 事件 ──

// terminalEvent 是 Terminal 事件循环处理的事件：调用方的读取、停止、清除输出请求，和读输出、等待进程、写入循环、后台 Terminate 任务报告的事实。
type terminalEvent interface{ isTerminalEvent() }

// terminalOutputRead 由读输出任务在读到一段完整 UTF-8 文本时发出；遇到非法字节或输出流结束时，剩下的字节原样发出。
type terminalOutputRead struct {
	data []byte
}

// terminalOutputClosed 由读输出任务在它的输出流结束时发出，每条流一次；读取失败时带错误。
type terminalOutputClosed struct {
	err error
}

// terminalProcessExited 由等待进程任务在 Process.Wait 返回、输出写端已关闭后发出。
type terminalProcessExited struct {
	exitCode int
	err      error
}

// terminalReadRequested 由 ReadOutput 在命令结束前发出；事件循环立即回复当前这一页和当前的 outputChanged。
type terminalReadRequested struct {
	cursor uint64
	reply  chan terminalOutputPage
}

// terminalStopRequested 由 Stop 发出，stopResult 是容量 1 的回复通道；总时限到期、管理器停止（stopping 关闭）时
// 由事件循环自己发出，stopResult 为 nil。
type terminalStopRequested struct {
	reason     TerminalStopReason
	stopResult chan error
}

// terminalClearRequested 由 ClearOutput 在命令结束前发出；事件循环丢掉保留的输出，只留 terminalClearSequence。
type terminalClearRequested struct{}

// terminalResized 由写入循环在 Process.ResizeTerminal 成功后发出；事件循环据此更新 info 的行列数。
type terminalResized struct {
	rows uint16
	cols uint16
}

// terminalStopFinished 由后台 Terminate 任务在 Terminate 返回时发出；Terminate 失败且停止由管理器停止发起，
// 或请求的 OwnsWorkspace 为 true 时，任务已调过 Process.Close。
type terminalStopFinished struct {
	reason TerminalStopReason
	err    error
}

func (terminalOutputRead) isTerminalEvent()     {}
func (terminalOutputClosed) isTerminalEvent()   {}
func (terminalProcessExited) isTerminalEvent()  {}
func (terminalReadRequested) isTerminalEvent()  {}
func (terminalStopRequested) isTerminalEvent()  {}
func (terminalClearRequested) isTerminalEvent() {}
func (terminalResized) isTerminalEvent()        {}
func (terminalStopFinished) isTerminalEvent()   {}

// ── Terminal 写入请求 ──

// terminalWrite 是 Terminal 写入循环按调用顺序处理的请求：PTY 输入和改尺寸，只由 WriteInput、Resize 发出。
type terminalWrite interface{ isTerminalWrite() }

// terminalInputWrite 由 WriteInput 发出；写入循环把 data 写进 PTY，结果回复到容量 1 的 written。
type terminalInputWrite struct {
	data    []byte
	written chan terminalInputWritten
}

// terminalResizeWrite 由 Resize 发出；写入循环修改 PTY 行列数，结果回复到容量 1 的 resized。
type terminalResizeWrite struct {
	rows    uint16
	cols    uint16
	resized chan error
}

func (terminalInputWrite) isTerminalWrite()  {}
func (terminalResizeWrite) isTerminalWrite() {}

// ── Terminal 请求回复 ──

// terminalOutputPage 是事件循环对 terminalReadRequested 的回复：一页输出，和回复这一刻的 outputChanged；
// cursor 越界时 err 非 nil。
type terminalOutputPage struct {
	output        TerminalOutput
	outputChanged <-chan struct{}
	err           error
}

// terminalInputWritten 是写入循环对 terminalInputWrite 的回复：实际写入的字节数和写入错误。
type terminalInputWritten struct {
	count int
	err   error
}

// ── Terminal 输出 ──

// terminalClearSequence 恢复颜色和字符集，清屏并清滚动历史，保留终端模式。
const terminalClearSequence = "\x1b[0m\x1b(B\x1b[H\x1b[2J\x1b[3J"

// terminalOutputBuffer 只保留输出末尾：retained 是保留的字节，startOffset 是它第一个字节在全部输出里的位置。
type terminalOutputBuffer struct {
	retained    []byte
	startOffset uint64
	// overflowOffset 是溢出裁剪到的位置，主动清除输出不改它。
	overflowOffset uint64
}

// terminalOutputPipe 是命令一条输出流的管道：Workspace 写 writer，读输出任务读 reader。
type terminalOutputPipe struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}
