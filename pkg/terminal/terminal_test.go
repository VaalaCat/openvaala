package terminal_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestTerminal 验证终端运行、输出和停止。
func TestTerminal(t *testing.T) {
	// 1. 准备命令、分页、PTY、取消、超时和异常样例。
	tests := []struct {
		name string
		// opt 为 nil 时用默认配置。
		opt *terminal.TerminalManagerOpt
		run func(t *testing.T, f *terminalManagerFixture)
	}{
		{
			name: "short_command_exits",
			run: func(t *testing.T, f *terminalManagerFixture) {
				command := f.startTerminal(t, f.newRequest("echo hi; exit 3"))
				text, output := readUntilFinished(t, command)
				info := output.Info
				if info.Status != terminal.TerminalStatusExited || info.ExitCode == nil || *info.ExitCode != 3 || info.StopReason != "" {
					t.Fatalf("Info = %+v, want exited with code 3", info)
				}
				if !strings.Contains(text, "hi") {
					t.Fatalf("output = %q, want hi", text)
				}
				if !strings.HasPrefix(info.ID, "cmd_") || len(info.ID) != len("cmd_")+12 {
					t.Fatalf("ID = %q, want cmd_ and 12 hex digits", info.ID)
				}
			},
		},
		{
			name: "long_command_keeps_running",
			run: func(t *testing.T, f *terminalManagerFixture) {
				command := f.startTerminal(t, f.newRequest("sleep 30"))
				output, err := command.ReadOutput(t.Context(), 0, 100*time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				if output.Info.Status != terminal.TerminalStatusRunning || output.Info.ExitCode != nil || output.Text != "" {
					t.Fatalf("ReadOutput() = %+v, want running without output", output)
				}
				if status := command.GetInfo().Status; status != terminal.TerminalStatusRunning {
					t.Fatalf("GetInfo().Status = %q, want running", status)
				}
			},
		},
		{
			name: "paging_and_reread",
			run: func(t *testing.T, f *terminalManagerFixture) {
				command := f.startTerminal(t, f.newRequest("printf first; sleep 1; head -c 100000 /dev/zero | tr '\\0' a"))
				first, err := command.ReadOutput(t.Context(), 0, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if first.Text != "first" || first.NextCursor != uint64(len("first")) {
					t.Fatalf("first read = %q, next %d, want first and %d", first.Text, first.NextCursor, len("first"))
				}
				reread, err := command.ReadOutput(t.Context(), 0, 0)
				if err != nil || reread.Text != first.Text {
					t.Fatalf("reread = %q, %v, want %q", reread.Text, err, first.Text)
				}
				waitFinished(t, command)
				page, err := command.ReadOutput(t.Context(), first.NextCursor, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Text) != 64<<10 || !page.HasMore {
					t.Fatalf("page after finish = %d bytes, HasMore %v, want 64 KiB and more", len(page.Text), page.HasMore)
				}
				last, err := command.ReadOutput(t.Context(), page.NextCursor, 0)
				if err != nil {
					t.Fatal(err)
				}
				if last.HasMore || last.NextCursor != uint64(len("first")+100000) {
					t.Fatalf("last page HasMore %v, next %d, want no more at %d", last.HasMore, last.NextCursor, len("first")+100000)
				}
				readAt := time.Now()
				empty, err := command.ReadOutput(t.Context(), last.NextCursor, 5*time.Second)
				if err != nil || empty.Text != "" || empty.HasMore || time.Since(readAt) > time.Second {
					t.Fatalf("read at the end = %+v, %v after %s, want empty text immediately", empty, err, time.Since(readAt))
				}
			},
		},
		{
			name: "tail_retained_after_overflow",
			opt: &terminal.TerminalManagerOpt{
				MaxTerminals:   64,
				MaxOutputBytes: 4096,
			},
			run: func(t *testing.T, f *terminalManagerFixture) {
				// 6667 个三字节的「中」共 20001 字节，再接 TAIL。
				command := f.startTerminal(t, f.newRequest("printf '中%.0s' $(seq 6667); printf TAIL"))
				if info := waitFinished(t, command); info.ExitCode == nil || *info.ExitCode != 0 {
					t.Fatalf("Info = %+v, want exit code 0", info)
				}
				var text strings.Builder
				var cursor uint64
				for pageIndex := 0; ; pageIndex++ {
					page, err := command.ReadOutput(t.Context(), cursor, 0)
					if err != nil {
						t.Fatal(err)
					}
					if pageIndex == 0 && !page.Truncated {
						t.Fatal("first page Truncated = false, want true")
					}
					if len(page.Text) > 64<<10 {
						t.Fatalf("page has %d bytes, want at most 64 KiB", len(page.Text))
					}
					text.WriteString(page.Text)
					cursor = page.NextCursor
					if !page.HasMore {
						break
					}
				}
				if !utf8.ValidString(text.String()) || !strings.HasSuffix(text.String(), "TAIL") || text.Len() > 4096 {
					t.Fatalf("retained output = %d bytes, valid UTF-8 %v, want at most 4096 valid bytes ending with TAIL", text.Len(), utf8.ValidString(text.String()))
				}
			},
		},
		{
			name: "invalid_cursor",
			run: func(t *testing.T, f *terminalManagerFixture) {
				command := f.startTerminal(t, f.newRequest("printf abc"))
				waitFinished(t, command)
				if _, err := command.ReadOutput(t.Context(), 4, 0); !errors.Is(err, terminal.ErrInvalidCursor) || err.Error() != "cursor 4 is after the latest output 3" {
					t.Fatalf("ReadOutput(4) error = %v, want ErrInvalidCursor at 3", err)
				}
				if output, err := command.ReadOutput(t.Context(), 3, 0); err != nil || output.Text != "" {
					t.Fatalf("ReadOutput(3) = %+v, %v, want empty output", output, err)
				}
			},
		},
		{
			name: "invalid_utf8_page_progresses",
			run: func(t *testing.T, f *terminalManagerFixture) {
				// 70000 个续字节 0x80，页尾落在续字节中间也要按字节切。
				command := f.startTerminal(t, f.newRequest("head -c 70000 /dev/zero | tr '\\0' '\\200'"))
				waitFinished(t, command)
				var cursor uint64
				pages := 0
				for {
					page, err := command.ReadOutput(t.Context(), cursor, 0)
					if err != nil {
						t.Fatal(err)
					}
					if page.NextCursor <= cursor {
						t.Fatalf("page %d NextCursor = %d, want after %d", pages, page.NextCursor, cursor)
					}
					cursor = page.NextCursor
					pages++
					if !page.HasMore {
						break
					}
				}
				if cursor != 70000 || pages != 2 {
					t.Fatalf("read %d bytes in %d pages, want 70000 bytes in 2 pages", cursor, pages)
				}
			},
		},
		{
			name: "pty_input",
			run: func(t *testing.T, f *terminalManagerFixture) {
				request := f.newRequest("read line; echo got:$line")
				request.Interactive = true
				command := f.startTerminal(t, request)
				if written, err := command.WriteInput(t.Context(), "hi\n"); written != 3 || err != nil {
					t.Fatalf("WriteInput() = %d, %v, want 3", written, err)
				}
				if text, _ := readUntilFinished(t, command); !strings.Contains(text, "got:hi") {
					t.Fatalf("output = %q, want got:hi", text)
				}
				if _, err := command.WriteInput(t.Context(), "x"); !errors.Is(err, terminal.ErrTerminalFinished) {
					t.Fatalf("WriteInput after finish error = %v, want ErrTerminalFinished", err)
				}
				plain := f.startTerminal(t, f.newRequest("sleep 30"))
				if _, err := plain.WriteInput(t.Context(), "x"); !errors.Is(err, terminal.ErrInputUnavailable) {
					t.Fatalf("WriteInput to a plain command error = %v, want ErrInputUnavailable", err)
				}
			},
		},
		{
			// 程序不读输入、写入超出 PTY 缓冲：取消调用立即返回；停止命令时关闭终端打断后台写入，命令照常结束。
			name: "canceled_input_returns",
			run: func(t *testing.T, f *terminalManagerFixture) {
				request := f.newRequest("sleep 30")
				request.Interactive = true
				command := f.startTerminal(t, request)
				ctx, cancel := context.WithCancel(t.Context())
				defer time.AfterFunc(200*time.Millisecond, cancel).Stop()
				started := time.Now()
				written, err := command.WriteInput(ctx, strings.Repeat(strings.Repeat("x", 63)+"\n", 1<<14))
				if !errors.Is(err, context.Canceled) || time.Since(started) > 2*time.Second {
					t.Fatalf("WriteInput() = %d, %v after %s, want context.Canceled within 2s", written, err, time.Since(started))
				}
				stopCtx, cancelStop := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancelStop()
				info, err := command.Stop(stopCtx)
				if err != nil || info.Status != terminal.TerminalStatusExited || info.StopReason != terminal.TerminalStoppedByTool {
					t.Fatalf("Stop() = %+v, %v, want exited with stopped", info, err)
				}
			},
		},
		{
			name: "stop_kills_group",
			run: func(t *testing.T, f *terminalManagerFixture) {
				command := f.startTerminal(t, f.newRequest("sh -c 'sleep 30 & echo $! > child.pid; sleep 30'"))
				childPID := waitChildPID(t, filepath.Join(f.root, "child.pid"))
				info, err := command.Stop(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if info.Status != terminal.TerminalStatusExited || info.StopReason != terminal.TerminalStoppedByTool || info.ExitCode == nil || *info.ExitCode != -1 {
					t.Fatalf("Stop() = %+v, want exited by signal with stopped", info)
				}
				waitCondition(t, 2*time.Second, func() bool { return isProcessGone(childPID) })
				again, err := command.Stop(t.Context())
				if err != nil || !reflect.DeepEqual(again, info) {
					t.Fatalf("second Stop() = %+v, %v, want %+v", again, err, info)
				}
			},
		},
		{
			name: "timeout_stops_command",
			run: func(t *testing.T, f *terminalManagerFixture) {
				request := f.newRequest("sleep 30")
				request.Timeout = 200 * time.Millisecond
				command := f.startTerminal(t, request)
				if info := waitFinished(t, command); info.Status != terminal.TerminalStatusExited || info.StopReason != terminal.TerminalStoppedByTimeout {
					t.Fatalf("Info = %+v, want exited with timed_out", info)
				}
			},
		},
		{
			name: "background_child_does_not_hold_output",
			run: func(t *testing.T, f *terminalManagerFixture) {
				command := f.startTerminal(t, f.newRequest("sleep 30 & echo started"))
				waitCondition(t, 2*time.Second, func() bool { return command.GetInfo().Status == terminal.TerminalStatusExited })
				output, err := command.ReadOutput(t.Context(), 0, 0)
				if err != nil || !strings.Contains(output.Text, "started") {
					t.Fatalf("ReadOutput() = %q, %v, want started", output.Text, err)
				}
			},
		},
		{
			name: "wait_error_marks_failed",
			run: func(t *testing.T, f *terminalManagerFixture) {
				process := newFakeProcess(nil)
				_ = process.Close()
				request := f.newRequest("ignored")
				request.Workspace = &fakeProcessWorkspace{process: process}
				info := waitFinished(t, f.startTerminal(t, request))
				if info.Status != terminal.TerminalStatusFailed || info.ExitCode != nil || info.Err == nil ||
					!strings.Contains(info.Err.Error(), "termination was not confirmed") {
					t.Fatalf("Info = %+v, want failed without exit code", info)
				}
			},
		},
		{
			name: "stop_failure_keeps_command_until_session_close",
			run: func(t *testing.T, f *terminalManagerFixture) {
				pipeline := telemetrytest.NewPipeline(t)
				process := newFakeProcess(errors.New("kill session refused"))
				request := f.newRequest("ignored")
				request.Workspace = &fakeProcessWorkspace{process: process}
				command, err := f.manager.StartTerminal(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), request)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := command.Stop(t.Context()); err == nil || err.Error() != "kill session refused" {
					t.Fatalf("Stop() error = %v, want the Terminate error", err)
				}
				if _, err := f.manager.GetTerminal("local", command.GetInfo().ID); err != nil || !f.manager.IsBusy() {
					t.Fatalf("after a failed Stop GetTerminal error = %v, IsBusy %v, want the command still managed", err, f.manager.IsBusy())
				}
				// 1. 关闭 Session，核对 Terminate 失败后释放进程，Run 返回 failed 且没有已确认的停止原因。
				f.stopManager(t)
				info := command.GetInfo()
				if info.Status != terminal.TerminalStatusFailed || info.StopReason != "" || !strings.Contains(info.Err.Error(), "termination was not confirmed") {
					t.Fatalf("Info after Run = %+v, want failed without a confirmed stop", info)
				}
				// 2. 核对工具停止失败不记日志，Session 关闭的终止失败只写一条 Warn。
				records := pipeline.GetLogRecords(t, "terminate command failed")
				if len(records) != 1 || records[0]["stop_reason"] != string(terminal.TerminalStoppedBySession) {
					t.Fatalf("terminate warnings = %v, want one for session_closed", records)
				}
			},
		},
		{
			name: "start_failure",
			run: func(t *testing.T, f *terminalManagerFixture) {
				request := f.newRequest("true")
				request.Directory = filepath.Join(f.root, "missing")
				if command, err := f.manager.StartTerminal(t.Context(), request); err == nil || command != nil {
					t.Fatalf("StartTerminal(missing directory) = %v, %v, want an error", command, err)
				}
			},
		},
	}
	// 2. 逐例创建终端夹具并执行运行、读取或停止断言。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTerminalManagerFixture(t, tt.opt))
		})
	}
}

// fakeProcess 是测试用的假进程：Wait 阻塞到 Close 后返回错误，模拟 SSH 断开后无法确认远端已停止；Terminate 返回 terminateErr。
type fakeProcess struct {
	terminateErr error
	released     chan struct{}
	releaseOnce  sync.Once
}

// newFakeProcess 创建可控制结束的测试进程。
func newFakeProcess(terminateErr error) *fakeProcess {
	// 1. 初始化结束通道和停止错误。
	return &fakeProcess{
		terminateErr: terminateErr,
		released:     make(chan struct{}),
	}
}

// Wait 等待测试进程结束。
func (p *fakeProcess) Wait() (int, error) {
	// 1. 等待退出信号，再返回模拟的退出码和错误。
	<-p.released
	return -1, errors.New("ssh connection lost")
}

// Terminate 返回预设的终止错误。
func (p *fakeProcess) Terminate() error {
	// 1. 直接返回 terminateErr。
	return p.terminateErr
}

// WriteTerminalInput 模拟不支持 PTY 输入的进程。
func (p *fakeProcess) WriteTerminalInput([]byte) (int, error) {
	// 1. 返回不支持终端输入的错误。
	return 0, workspace.ErrTTYInputUnsupported
}

// Close 关闭测试进程。
func (p *fakeProcess) Close() error {
	// 1. 关闭结束信号，重复关闭时不再操作。
	p.releaseOnce.Do(func() { close(p.released) })
	return nil
}

// fakeProcessWorkspace 的 StartProcess 总是返回同一个假进程，其他方法不可用。
type fakeProcessWorkspace struct {
	workspace.Workspace
	process *fakeProcess
}

// StartProcess 返回预设测试进程。
func (w *fakeProcessWorkspace) StartProcess(context.Context, *workspace.ProcessRequest) (workspace.Process, error) {
	// 1. 返回已配置的进程和 nil 错误。
	return w.process, nil
}

// readUntilFinished 持续读取终端输出直到结束。
func readUntilFinished(t *testing.T, command terminal.Terminal) (string, terminal.TerminalOutput) {
	t.Helper()
	// 1. 从游标 0 开始分页读取并累加文本，终端结束且没有更多数据时返回。
	deadline := time.Now().Add(5 * time.Second)
	var text strings.Builder
	var cursor uint64
	for {
		output, err := command.ReadOutput(t.Context(), cursor, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(output.Text)
		cursor = output.NextCursor
		if output.Info.Status != terminal.TerminalStatusRunning && !output.HasMore {
			return text.String(), output
		}
		if time.Now().After(deadline) {
			t.Fatalf("command did not finish, output so far %q", text.String())
		}
	}
}

// waitFinished 等待终端结束并返回最终信息。
func waitFinished(t *testing.T, command terminal.Terminal) terminal.TerminalInfo {
	t.Helper()
	// 1. 轮询终端状态，结束时返回信息，超时则测试失败。
	waitCondition(t, 5*time.Second, func() bool { return command.GetInfo().Status != terminal.TerminalStatusRunning })
	return command.GetInfo()
}

// waitChildPID 等待子进程 PID 文件并读取。
func waitChildPID(t *testing.T, path string) int {
	t.Helper()
	// 1. 等待文件内容可解析为 PID，超时则测试失败。
	var childPID int
	waitCondition(t, 5*time.Second, func() bool {
		content, err := os.ReadFile(path)
		childPID, _ = strconv.Atoi(strings.TrimSpace(string(content)))
		return err == nil && childPID > 0
	})
	return childPID
}

// waitCondition 等待测试条件成立。
func waitCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	// 1. 反复检查条件，超过时限时测试失败。
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isProcessGone 指定进程是否已消失或成为僵尸进程？
func isProcessGone(pid int) bool {
	// 1. 读取 /proc/<PID>/stat，读取失败或状态为 Z 时返回 true。
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] == "Z"
}

// ResizeTerminal 模拟不支持修改终端尺寸的进程。
func (p *fakeProcess) ResizeTerminal(rows, cols uint16) error {
	// 1. 返回不支持终端输入的错误。
	return workspace.ErrTTYInputUnsupported
}
