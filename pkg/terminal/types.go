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
	TerminalStoppedByTool    TerminalStopReason = "stopped"
	TerminalStoppedByTimeout TerminalStopReason = "timed_out"
	TerminalStoppedBySession TerminalStopReason = "session_closed"
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
	Status      TerminalStatus
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

// terminalEvent 是 Terminal 事件循环处理的事件：调用方的读取、停止、写入请求，和读输出、等待进程、后台 Terminate 任务报告的事实。
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

// terminalStopRequested 由 Stop 发出，stopResult 是容量 1 的回复通道；总时限到期、Session 关闭（stopping 关闭）时
// 由事件循环自己发出，stopResult 为 nil。
type terminalStopRequested struct {
	reason     TerminalStopReason
	stopResult chan error
}

// terminalInputRequested 由 WriteInput 在命令结束前发出；事件循环在 tasks 里启动一次写入，不等它结束，
// 写入任务把结果回复到容量 1 的 inputWritten。
type terminalInputRequested struct {
	data         []byte
	inputWritten chan terminalInputWritten
}

// terminalStopFinished 由后台 Terminate 任务在 Terminate 返回时发出；Session 关闭发起的 Terminate 失败时，
// 任务已调过 Process.Close。
type terminalStopFinished struct {
	reason TerminalStopReason
	err    error
}

func (terminalOutputRead) isTerminalEvent()     {}
func (terminalOutputClosed) isTerminalEvent()   {}
func (terminalProcessExited) isTerminalEvent()  {}
func (terminalReadRequested) isTerminalEvent()  {}
func (terminalStopRequested) isTerminalEvent()  {}
func (terminalInputRequested) isTerminalEvent() {}
func (terminalStopFinished) isTerminalEvent()   {}

// ── Terminal 事件回复 ──

// terminalOutputPage 是事件循环对 terminalReadRequested 的回复：一页输出，和回复这一刻的 outputChanged；
// cursor 越界时 err 非 nil。
type terminalOutputPage struct {
	output        TerminalOutput
	outputChanged <-chan struct{}
	err           error
}

// terminalInputWritten 是写入任务对 terminalInputRequested 的回复：实际写入的字节数和写入错误。
type terminalInputWritten struct {
	count int
	err   error
}

// ── Terminal 输出 ──

// terminalOutputBuffer 只保留输出末尾：retained 是保留的字节，startOffset 是它第一个字节在全部输出里的位置。
type terminalOutputBuffer struct {
	retained    []byte
	startOffset uint64
}

// terminalOutputPipe 是命令一条输出流的管道：Workspace 写 writer，读输出任务读 reader。
type terminalOutputPipe struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}
