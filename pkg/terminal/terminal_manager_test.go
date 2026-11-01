package terminal_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestTerminalManager 验证终端管理和容量限制。
func TestTerminalManager(t *testing.T) {
	// 1. 准备淘汰、满载、环境隔离、启动失败、忙闲和关闭样例。
	tests := []struct {
		name string
		// opt 为 nil 时用默认配置。
		opt *terminal.TerminalManagerOpt
		run func(t *testing.T, f *terminalManagerFixture)
	}{
		{
			name: "limit_evicts_earliest_finished",
			opt: &terminal.TerminalManagerOpt{
				MaxTerminals:   2,
				MaxOutputBytes: 1 << 20,
			},
			run: func(t *testing.T, f *terminalManagerFixture) {
				first := f.startTerminal(t, f.newRequest("true"))
				waitFinished(t, first)
				second := f.startTerminal(t, f.newRequest("true"))
				waitFinished(t, second)
				third := f.startTerminal(t, f.newRequest("true"))
				if _, err := f.manager.GetTerminal("local", first.GetInfo().ID); !errors.Is(err, terminal.ErrTerminalNotFound) {
					t.Fatalf("GetTerminal(first) error = %v, want ErrTerminalNotFound", err)
				}
				for _, kept := range []terminal.Terminal{second, third} {
					if _, err := f.manager.GetTerminal("local", kept.GetInfo().ID); err != nil {
						t.Fatalf("GetTerminal(%s) = %v", kept.GetInfo().ID, err)
					}
				}
			},
		},
		{
			name: "limit_rejects_when_all_running",
			opt: &terminal.TerminalManagerOpt{
				MaxTerminals:   2,
				MaxOutputBytes: 1 << 20,
			},
			run: func(t *testing.T, f *terminalManagerFixture) {
				running := []terminal.Terminal{
					f.startTerminal(t, f.newRequest("sleep 30")),
					f.startTerminal(t, f.newRequest("sleep 30")),
				}
				if _, err := f.manager.StartTerminal(t.Context(), f.newRequest("sleep 30")); !errors.Is(err, terminal.ErrTerminalLimitReached) {
					t.Fatalf("third StartTerminal error = %v, want ErrTerminalLimitReached", err)
				}
				for _, command := range running {
					found, err := f.manager.GetTerminal("local", command.GetInfo().ID)
					if err != nil || found.GetInfo().Status != terminal.TerminalStatusRunning {
						t.Fatalf("GetTerminal(%s) = %v, %v, want running", command.GetInfo().ID, found, err)
					}
				}
			},
		},
		{
			name: "environment_isolation",
			run: func(t *testing.T, f *terminalManagerFixture) {
				command := f.startTerminal(t, f.newRequest("sleep 30"))
				id := command.GetInfo().ID
				if _, err := f.manager.GetTerminal("ssh", id); !errors.Is(err, terminal.ErrTerminalNotFound) {
					t.Fatalf("GetTerminal(ssh) error = %v, want ErrTerminalNotFound", err)
				}
				if listed := f.manager.ListTerminals("ssh"); len(listed) != 0 {
					t.Fatalf("ListTerminals(ssh) = %d commands, want none", len(listed))
				}
				if listed := getTerminalIDs(f.manager.ListTerminals("local")); !slices.Equal(listed, []string{id}) {
					t.Fatalf("ListTerminals(local) = %v, want [%s]", listed, id)
				}
			},
		},
		{
			name: "start_failure_not_listed",
			opt: &terminal.TerminalManagerOpt{
				MaxTerminals:   2,
				MaxOutputBytes: 1 << 20,
			},
			run: func(t *testing.T, f *terminalManagerFixture) {
				request := f.newRequest("true")
				request.Directory = filepath.Join(f.root, "missing")
				if _, err := f.manager.StartTerminal(t.Context(), request); err == nil {
					t.Fatal("StartTerminal(missing directory) error = nil")
				}
				first := f.startTerminal(t, f.newRequest("sleep 30"))
				if listed := getTerminalIDs(f.manager.ListTerminals("local")); !slices.Equal(listed, getTerminalIDs([]terminal.Terminal{first})) {
					t.Fatalf("ListTerminals = %v, want only the started command", listed)
				}
				// 登记表已满：启动失败的记录结束得最早，先被淘汰。
				second := f.startTerminal(t, f.newRequest("sleep 30"))
				want := getTerminalIDs([]terminal.Terminal{first, second})
				if listed := getTerminalIDs(f.manager.ListTerminals("local")); !slices.Equal(listed, want) {
					t.Fatalf("ListTerminals = %v, want %v", listed, want)
				}
			},
		},
		{
			name: "busy_until_finished",
			run: func(t *testing.T, f *terminalManagerFixture) {
				if f.manager.IsBusy() {
					t.Fatal("IsBusy() = true without commands")
				}
				command := f.startTerminal(t, f.newRequest("sleep 30"))
				if !f.manager.IsBusy() {
					t.Fatal("IsBusy() = false while sleep is running")
				}
				if _, err := command.Stop(t.Context()); err != nil {
					t.Fatal(err)
				}
				if f.manager.IsBusy() {
					t.Fatal("IsBusy() = true after Stop")
				}
			},
		},
		{
			name: "run_stops_all",
			run: func(t *testing.T, f *terminalManagerFixture) {
				interactive := f.newRequest("sleep 30")
				interactive.Interactive = true
				commands := []terminal.Terminal{
					f.startTerminal(t, f.newRequest("sleep 30")),
					f.startTerminal(t, interactive),
				}
				f.stopManager(t)
				for _, command := range commands {
					info := command.GetInfo()
					if info.Status != terminal.TerminalStatusExited || info.StopReason != terminal.TerminalStoppedBySession {
						t.Fatalf("GetInfo() = %+v, want exited with session_closed", info)
					}
				}
				if _, err := f.manager.StartTerminal(t.Context(), f.newRequest("true")); !errors.Is(err, terminal.ErrTerminalManagerStopped) {
					t.Fatalf("StartTerminal after Run error = %v, want ErrTerminalManagerStopped", err)
				}
			},
		},
	}
	// 2. 逐例创建管理器夹具并核对终端管理行为。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTerminalManagerFixture(t, tt.opt))
		})
	}
}

// terminalManagerFixture 是在后台运行的 TerminalManager 和它启动命令用的本机工作区。
type terminalManagerFixture struct {
	manager   terminal.TerminalManager
	workspace workspace.Workspace
	root      string
	cancelRun context.CancelFunc
	// runFinished 在 Run 返回后关闭。
	runFinished chan struct{}
}

// newTerminalManagerFixture 创建运行中的终端管理测试夹具。
func newTerminalManagerFixture(t *testing.T, opt *terminal.TerminalManagerOpt) *terminalManagerFixture {
	t.Helper()
	// 1. 创建管理器、临时工作区及取消信号，启动管理循环并登记清理。
	if opt == nil {
		opt = terminal.NewTerminalManagerOpt()
	}
	root := t.TempDir()
	localWorkspace, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := terminal.NewTerminalManager(opt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &terminalManagerFixture{
		manager:     manager,
		workspace:   localWorkspace,
		root:        root,
		cancelRun:   cancel,
		runFinished: make(chan struct{}),
	}
	go func() {
		defer close(f.runFinished)
		_ = manager.Run(ctx)
	}()
	t.Cleanup(func() { f.stopManager(t) })
	return f
}

// newRequest 创建终端启动请求。
func (f *terminalManagerFixture) newRequest(command string) *terminal.TerminalRequest {
	// 1. 使用夹具工作区和给定命令填充请求。
	return &terminal.TerminalRequest{
		EnvironmentID: "local",
		Workspace:     f.workspace,
		Command:       command,
		Directory:     f.root,
	}
}

// startTerminal 启动测试终端并核对错误。
func (f *terminalManagerFixture) startTerminal(t *testing.T, request *terminal.TerminalRequest) terminal.Terminal {
	t.Helper()
	// 1. 交给管理器启动终端，失败时结束测试。
	command, err := f.manager.StartTerminal(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

// stopManager 停止测试终端管理器。
func (f *terminalManagerFixture) stopManager(t *testing.T) {
	t.Helper()
	// 1. 取消管理上下文并等待管理循环返回。
	f.cancelRun()
	select {
	case <-f.runFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("TerminalManager.Run did not return")
	}
}

// getTerminalIDs 提取终端列表中的 ID。
func getTerminalIDs(commands []terminal.Terminal) []string {
	// 1. 按列表顺序收集终端 ID。
	ids := make([]string, 0, len(commands))
	for _, command := range commands {
		ids = append(ids, command.GetInfo().ID)
	}
	return ids
}
