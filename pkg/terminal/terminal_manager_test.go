package terminal_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/terminal"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestTerminalManager 验证终端管理和容量限制。
func TestTerminalManager(t *testing.T) {
	// 1. 准备淘汰、满载、环境隔离、启动失败、忙闲、关闭、跨环境查询、独占工作区和结束通知样例。
	terminalFinished := make(chan struct{}, 1)
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
					if info.Status != terminal.TerminalStatusExited || info.StopReason != terminal.TerminalStoppedByManager {
						t.Fatalf("GetInfo() = %+v, want exited with session_closed", info)
					}
				}
				if _, err := f.manager.StartTerminal(t.Context(), f.newRequest("true")); !errors.Is(err, terminal.ErrTerminalManagerStopped) {
					t.Fatalf("StartTerminal after Run error = %v, want ErrTerminalManagerStopped", err)
				}
			},
		},
		{
			name: "lookups_cross_environments",
			run: func(t *testing.T, f *terminalManagerFixture) {
				first := f.startTerminal(t, f.newRequest("sleep 30"))
				secondRequest := f.newRequest("sleep 30")
				secondRequest.EnvironmentID = "ssh"
				second := f.startTerminal(t, secondRequest)
				failedRequest := f.newRequest("true")
				failedRequest.Directory = filepath.Join(f.root, "missing")
				if _, err := f.manager.StartTerminal(t.Context(), failedRequest); err == nil {
					t.Fatal("StartTerminal(missing directory) error = nil")
				}
				for _, command := range []terminal.Terminal{first, second} {
					id := command.GetInfo().ID
					if found, err := f.manager.GetTerminalByID(id); err != nil || found.GetInfo().ID != id {
						t.Fatalf("GetTerminalByID(%s) = %v, %v, want the command", id, found, err)
					}
				}
				if _, err := f.manager.GetTerminalByID("cmd_unknown"); !errors.Is(err, terminal.ErrTerminalNotFound) {
					t.Fatalf("GetTerminalByID(unknown) error = %v, want ErrTerminalNotFound", err)
				}
				want := getTerminalIDs([]terminal.Terminal{first, second})
				if listed := getTerminalIDs(f.manager.ListAllTerminals()); !slices.Equal(listed, want) {
					t.Fatalf("ListAllTerminals() = %v, want %v in start order without the failed start", listed, want)
				}
			},
		},
		{
			name: "owned_workspace_is_closed",
			run: func(t *testing.T, _ *terminalManagerFixture) {
				tests := []struct {
					name           string
					opt            *terminal.TerminalManagerOpt
					ownsWorkspace  bool
					wantCloseCount int
					// run 触发工作区关闭的那一步：核对启动结果，返回时关闭已经发生。
					run func(t *testing.T, f *terminalManagerFixture, request *terminal.TerminalRequest, fakeWorkspace *fakeProcessWorkspace)
				}{
					{
						name:           "command_finished",
						ownsWorkspace:  true,
						wantCloseCount: 1,
						run: func(t *testing.T, f *terminalManagerFixture, request *terminal.TerminalRequest, fakeWorkspace *fakeProcessWorkspace) {
							command := f.startTerminal(t, request)
							if closeCount := fakeWorkspace.getCloseCount(); closeCount != 0 {
								t.Fatalf("Close count while running = %d, want 0", closeCount)
							}
							if _, err := command.Stop(t.Context()); err != nil {
								t.Fatal(err)
							}
							waitFinished(t, command)
						},
					},
					{
						name:           "command_finished_without_ownership",
						ownsWorkspace:  false,
						wantCloseCount: 0,
						run: func(t *testing.T, f *terminalManagerFixture, request *terminal.TerminalRequest, fakeWorkspace *fakeProcessWorkspace) {
							command := f.startTerminal(t, request)
							if _, err := command.Stop(t.Context()); err != nil {
								t.Fatal(err)
							}
							waitFinished(t, command)
						},
					},
					{
						name:           "limit_reached",
						opt:            &terminal.TerminalManagerOpt{MaxTerminals: 1, MaxOutputBytes: 1 << 20},
						ownsWorkspace:  true,
						wantCloseCount: 1,
						run: func(t *testing.T, f *terminalManagerFixture, request *terminal.TerminalRequest, fakeWorkspace *fakeProcessWorkspace) {
							f.startTerminal(t, f.newRequest("sleep 30"))
							if _, err := f.manager.StartTerminal(t.Context(), request); !errors.Is(err, terminal.ErrTerminalLimitReached) {
								t.Fatalf("StartTerminal() error = %v, want ErrTerminalLimitReached", err)
							}
						},
					},
					{
						name:           "manager_stopped",
						ownsWorkspace:  true,
						wantCloseCount: 1,
						run: func(t *testing.T, f *terminalManagerFixture, request *terminal.TerminalRequest, fakeWorkspace *fakeProcessWorkspace) {
							f.stopManager(t)
							if _, err := f.manager.StartTerminal(t.Context(), request); !errors.Is(err, terminal.ErrTerminalManagerStopped) {
								t.Fatalf("StartTerminal() error = %v, want ErrTerminalManagerStopped", err)
							}
						},
					},
					{
						name:           "start_failed",
						ownsWorkspace:  true,
						wantCloseCount: 1,
						run: func(t *testing.T, f *terminalManagerFixture, request *terminal.TerminalRequest, fakeWorkspace *fakeProcessWorkspace) {
							fakeWorkspace.startErr = errors.New("ssh dial failed")
							if _, err := f.manager.StartTerminal(t.Context(), request); err == nil || err.Error() != "ssh dial failed" {
								t.Fatalf("StartTerminal() error = %v, want the start error", err)
							}
						},
					},
				}
				for _, tt := range tests {
					t.Run(tt.name, func(t *testing.T) {
						f := newTerminalManagerFixture(t, tt.opt)
						fakeWorkspace := newFakeProcessWorkspace(newFakeInteractiveProcess(false))
						request := f.newRequest("ignored")
						request.Workspace = fakeWorkspace
						request.Interactive = true
						request.OwnsWorkspace = tt.ownsWorkspace
						tt.run(t, f, request, fakeWorkspace)
						if closeCount := fakeWorkspace.getCloseCount(); closeCount != tt.wantCloseCount {
							t.Fatalf("Close count = %d, want %d", closeCount, tt.wantCloseCount)
						}
					})
				}
			},
		},
		{
			// Terminate 卡住时，5 秒后关闭独占的工作区，卡住的 Terminate 随之返回。
			name: "stuck_terminate_closes_workspace",
			run: func(t *testing.T, f *terminalManagerFixture) {
				pipeline := telemetrytest.NewPipeline(t)
				process := newFakeInteractiveProcess(false)
				fakeWorkspace := newFakeProcessWorkspace(process)
				process.terminateUnblock = fakeWorkspace.closed
				request := f.newRequest("ignored")
				request.Workspace = fakeWorkspace
				request.Interactive = true
				request.OwnsWorkspace = true
				command, err := f.manager.StartTerminal(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), request)
				if err != nil {
					t.Fatal(err)
				}
				stopStarted := time.Now()
				stopResult := make(chan error, 1)
				go func() {
					_, err := command.Stop(t.Context())
					stopResult <- err
				}()
				select {
				case <-fakeWorkspace.closed:
					if elapsed := time.Since(stopStarted); elapsed < 4*time.Second {
						t.Fatalf("workspace closed after %s, want after the 5 second Terminate limit", elapsed)
					}
				case <-time.After(8 * time.Second):
					t.Fatal("workspace was not closed after Terminate got stuck")
				}
				select {
				case err := <-stopResult:
					if err == nil || err.Error() != "connection closed" {
						t.Fatalf("Stop() error = %v, want the Terminate error", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Stop() did not return after the workspace was closed")
				}
			},
		},
		{
			// Terminate 立刻失败：独占工作区的命令放手进程，让关标签页仍能结束 shell；不独占的命令继续受管。
			name: "owned_workspace_stop_failure_closes_process",
			run: func(t *testing.T, f *terminalManagerFixture) {
				tests := []struct {
					name              string
					ownsWorkspace     bool
					wantProcessClosed bool
				}{
					{name: "owned", ownsWorkspace: true, wantProcessClosed: true},
					{name: "not_owned", ownsWorkspace: false, wantProcessClosed: false},
				}
				for _, tt := range tests {
					t.Run(tt.name, func(t *testing.T) {
						pipeline := telemetrytest.NewPipeline(t)
						process := newFakeInteractiveProcess(false)
						process.terminateErr = errors.New("kill script failed")
						request := f.newRequest("ignored")
						request.Workspace = newFakeProcessWorkspace(process)
						request.Interactive = true
						request.OwnsWorkspace = tt.ownsWorkspace
						command, err := f.manager.StartTerminal(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), request)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := command.Stop(t.Context()); err == nil || err.Error() != "kill script failed" {
							t.Fatalf("Stop() error = %v, want the Terminate error", err)
						}
						// Close 在 Terminate 失败后、Stop 返回之前调用，这里直接检查。
						isProcessClosed := false
						select {
						case <-process.released:
							isProcessClosed = true
						default:
						}
						if isProcessClosed != tt.wantProcessClosed {
							t.Fatalf("process closed = %v, want %v", isProcessClosed, tt.wantProcessClosed)
						}
						if !tt.wantProcessClosed && command.GetInfo().Status != terminal.TerminalStatusRunning {
							t.Fatalf("Status = %q, want the command still running and managed", command.GetInfo().Status)
						}
					})
				}
			},
		},
		{
			name: "finished_signal_is_sent",
			opt:  terminal.NewTerminalManagerOpt().WithTerminalFinished(terminalFinished),
			run: func(t *testing.T, f *terminalManagerFixture) {
				first := f.startTerminal(t, f.newRequest("sleep 30"))
				second := f.startTerminal(t, f.newRequest("sleep 30"))
				select {
				case <-terminalFinished:
					t.Fatal("received a finished signal while both commands were running")
				default:
				}
				// 通道没人取，第二条命令结束时的唤醒与第一条合并，不阻塞命令收尾。
				for _, command := range []terminal.Terminal{first, second} {
					if _, err := command.Stop(t.Context()); err != nil {
						t.Fatal(err)
					}
					waitFinished(t, command)
				}
				select {
				case <-terminalFinished:
				case <-time.After(2 * time.Second):
					t.Fatal("did not receive a finished signal after the commands finished")
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

// startFakeTerminal 启动一条进程由 fakeWorkspace 提供的交互命令，rows、cols 为零时按默认尺寸。
func (f *terminalManagerFixture) startFakeTerminal(t *testing.T, fakeWorkspace *fakeProcessWorkspace, rows, cols uint16) terminal.Terminal {
	t.Helper()
	// 1. 使用假工作区、交互模式和初始尺寸填充请求，交给管理器启动。
	request := f.newRequest("ignored")
	request.Workspace = fakeWorkspace
	request.Interactive = true
	request.Rows, request.Cols = rows, cols
	return f.startTerminal(t, request)
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
