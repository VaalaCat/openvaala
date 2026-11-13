package desktop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/egoist/mygo/ui"
)

// terminalClient 记录终端相关调用，其余 Client 方法不会被这些测试用到。
type terminalClient struct {
	runtimeclient.Client
	mu       sync.Mutex
	resized  []string
	written  []string
	closed   []string
	attached []string
	reset    []string
	opened   []*runtimeclient.TerminalOpenRequest
}

// 连上 serve 时 handleClientEvent 会加载列表并订阅；这些调用的结果不影响终端，返回空值。
func (c *terminalClient) SubscribeRunningConversations(context.Context) error { return nil }

func (c *terminalClient) SubscribeTerminals(context.Context) error { return nil }

func (c *terminalClient) ListConversations(context.Context, *runtimeclient.ListConversationsRequest) (runtimeclient.ListConversationsResult, error) {
	return runtimeclient.ListConversationsResult{}, nil
}

func (c *terminalClient) ListModels(context.Context, string) (runtime.ModelList, error) {
	return runtime.ModelList{}, nil
}

func (c *terminalClient) ListExecutors(context.Context) ([]conversation.ExecutorDetails, error) {
	return nil, nil
}

func (c *terminalClient) ListAgents(context.Context) ([]runtime.AgentInfo, error) { return nil, nil }

func (c *terminalClient) record(list *[]string, entry string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*list = append(*list, entry)
}

func (c *terminalClient) OpenTerminal(_ context.Context, request *runtimeclient.TerminalOpenRequest) (*runtimeprotocol.TerminalOpenResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened = append(c.opened, request)
	return &runtimeprotocol.TerminalOpenResult{TerminalID: "opened", ExecutorID: request.ExecutorID, Directory: request.Directory}, nil
}

func (c *terminalClient) AttachTerminal(_ context.Context, terminalID string, cursor uint64) (*runtimeprotocol.TerminalAttachResult, error) {
	c.record(&c.attached, fmt.Sprintf("%s:%d", terminalID, cursor))
	return &runtimeprotocol.TerminalAttachResult{ExecutorID: "local", Directory: "/work/repo", Rows: 30, Cols: 100}, nil
}

func (c *terminalClient) ResetTerminal(_ context.Context, terminalID string) error {
	c.record(&c.reset, terminalID)
	return nil
}

func (c *terminalClient) CloseTerminal(_ context.Context, terminalID string) error {
	c.record(&c.closed, terminalID)
	return nil
}

func (c *terminalClient) WriteTerminal(terminalID string, data []byte) error {
	c.record(&c.written, terminalID+":"+string(data))
	return nil
}

func (c *terminalClient) ResizeTerminal(terminalID string, rows, cols uint16) error {
	c.record(&c.resized, fmt.Sprintf("%s:%dx%d", terminalID, rows, cols))
	return nil
}

// newTerminalTestDesktop 返回带终端记录客户端的 Desktop，并在测试结束时释放标签的屏幕。
func newTerminalTestDesktop(t *testing.T) (*Desktop, *terminalClient) {
	t.Helper()
	client := &terminalClient{}
	d := testDesktop()
	d.client = client
	t.Cleanup(func() { d.closeTerminalStates() })
	return d, client
}

// addTerminalTab 在 Desktop 末尾加一个指定阶段的标签并选中。
func addTerminalTab(d *Desktop, key, terminalID string, phase terminalPhase) *terminalTab {
	tab := &terminalTab{Key: key, TerminalID: terminalID, ExecutorID: "local", Directory: "/work/repo", State: kit.NewTerminalState(24, 80), Phase: phase}
	d.terminals = append(d.terminals, tab)
	d.selectedTerminal = key
	return tab
}

// TestTerminalOpened 检查打开结果只落到等待它的标签：成功、失败、标签已关闭、连接已断开。
func TestTerminalOpened(t *testing.T) {
	result := &runtimeprotocol.TerminalOpenResult{TerminalID: "term-1", ExecutorID: "ssh-a", Directory: "/srv/app"}
	for _, tt := range []struct {
		name       string
		phase      terminalPhase
		removed    bool
		err        error
		wantPhase  terminalPhase
		wantID     string
		wantScreen string
		wantClosed bool
		stale      bool // 结果属于标签重新启动之前的旧请求
	}{
		{"success", terminalOpening, false, nil, terminalRunning, "term-1", "", false, false},
		{"failure shows error", terminalOpening, false, errors.New("this workspace does not support interactive commands"), terminalExited, "", "this workspace does not support interactive commands", false, false},
		{"closed while opening", terminalOpening, true, nil, terminalExited, "", "", true, false},
		{"disconnected while opening", terminalExited, false, nil, terminalExited, "", "", true, false},
		{"disconnected while opening then failed", terminalExited, false, errors.New("late failure"), terminalExited, "", "", false, false},
		{"stale success after restart", terminalOpening, false, nil, terminalOpening, "", "", true, true},
		{"stale failure after restart", terminalOpening, false, errors.New("late failure"), terminalOpening, "", "", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			d.openingTerminalRequests = 1
			tab := addTerminalTab(d, "terminal-1", "", tt.phase)
			if tt.removed {
				tab.Phase = terminalExited
				d.terminals = nil
			}
			var opened *runtimeprotocol.TerminalOpenResult
			if tt.err == nil {
				opened = result
			}
			tab.OpenRequest = 2
			request := tab.OpenRequest
			if tt.stale {
				request = 1
			}
			d.handleTerminalOpened(client, tab, request, opened, tt.err)
			d.tasks.Wait()
			if tab.Phase != tt.wantPhase || tab.TerminalID != tt.wantID {
				t.Fatalf("phase=%v id=%q", tab.Phase, tab.TerminalID)
			}
			if !strings.Contains(tab.State.Text(), tt.wantScreen) || (tt.wantScreen == "" && tab.State.Text() != "") {
				t.Fatalf("screen = %q", tab.State.Text())
			}
			if tt.wantID != "" && (tab.ExecutorID != "ssh-a" || tab.Directory != "/srv/app" || !slices.Equal(client.resized, []string{"term-1:24x80"})) {
				t.Fatalf("executor=%q directory=%q resized=%v", tab.ExecutorID, tab.Directory, client.resized)
			}
			if tt.wantClosed != slices.Equal(client.closed, []string{"term-1"}) {
				t.Fatalf("closed = %v", client.closed)
			}
		})
	}
}

// TestTerminalEvents 检查输出按终端 ID 进入对应标签，退出把标签标为已结束并写入说明。
func TestTerminalEvents(t *testing.T) {
	for _, tt := range []struct {
		name       string
		event      runtimeclient.ClientEvent
		wantFirst  string
		wantSecond string
		wantPhases [2]terminalPhase
	}{
		{"output to first", &runtimeclient.TerminalOutput{TerminalID: "a", Data: []byte("hello")}, "hello", "", [2]terminalPhase{terminalRunning, terminalRunning}},
		{"output to second", &runtimeclient.TerminalOutput{TerminalID: "b", Data: []byte("world")}, "", "world", [2]terminalPhase{terminalRunning, terminalRunning}},
		{"output to unknown", &runtimeclient.TerminalOutput{TerminalID: "z", Data: []byte("lost")}, "", "", [2]terminalPhase{terminalRunning, terminalRunning}},
		{"exit code", &runtimeclient.TerminalExited{TerminalID: "b", ExitCode: 3}, "", "\n[进程已退出（代码 3）]", [2]terminalPhase{terminalRunning, terminalExited}},
		{"exit error", &runtimeclient.TerminalExited{TerminalID: "a", Error: "ssh session closed"}, "\n[ssh session closed]", "", [2]terminalPhase{terminalExited, terminalRunning}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newTerminalTestDesktop(t)
			first := addTerminalTab(d, "terminal-1", "a", terminalRunning)
			second := addTerminalTab(d, "terminal-2", "b", terminalRunning)
			d.handleClientEvent(tt.event)
			if got := first.State.Text(); got != tt.wantFirst {
				t.Fatalf("first screen = %q", got)
			}
			if got := second.State.Text(); got != tt.wantSecond {
				t.Fatalf("second screen = %q", got)
			}
			if first.Phase != tt.wantPhases[0] || second.Phase != tt.wantPhases[1] {
				t.Fatalf("phases = %v %v", first.Phase, second.Phase)
			}
		})
	}
}

// TestTerminalDisconnected 检查连接事件经 handleClientEvent 的接线：断开时运行中的标签进入重连且屏幕没有结束说明，其他标签不变；连上后重连中的标签从已收到的位置接回。
func TestTerminalDisconnected(t *testing.T) {
	for _, tt := range []struct {
		name       string
		phase      terminalPhase
		wantPhase  terminalPhase
		cursor     uint64
		wantAttach []string
	}{
		{"running keeps tab reconnecting", terminalRunning, terminalReconnecting, 42, []string{"a:42"}},
		{"opening is left to its request", terminalOpening, terminalOpening, 0, nil},
		{"exited stays exited", terminalExited, terminalExited, 0, nil},
	} {
		for _, state := range []runtimeclient.ConnectionState{runtimeclient.ConnectionReconnecting, runtimeclient.ConnectionConnecting} {
			t.Run(tt.name+"/"+string(state), func(t *testing.T) {
				d, client := newTerminalTestDesktop(t)
				tab := addTerminalTab(d, "terminal-1", "a", tt.phase)
				tab.ReceivedCursor = tt.cursor
				d.handleClientEvent(&runtimeclient.ConnectionChanged{State: state})
				if tab.Phase != tt.wantPhase || tab.State.Text() != "" {
					t.Fatalf("phase=%v screen=%q", tab.Phase, tab.State.Text())
				}
				// 连上后只有重连中的标签接回，位置是已写进屏幕的输出位置。
				d.handleClientEvent(&runtimeclient.ConnectionChanged{State: runtimeclient.ConnectionConnected, ServerID: d.serverID})
				d.tasks.Wait()
				if !slices.Equal(client.attached, tt.wantAttach) {
					t.Fatalf("attached = %v, want %v", client.attached, tt.wantAttach)
				}
			})
		}
	}
}

// TestTerminalAttached 检查接入结果：成功后按 PTY 尺寸换新屏幕并开始接收输出；终端已不存在时标为已结束；断线继续等待重连；其他错误写进标签；结果到达时连接已断开则不改标签。
func TestTerminalAttached(t *testing.T) {
	result := &runtimeprotocol.TerminalAttachResult{ExecutorID: "ssh-a", Directory: "/srv/app", Rows: 30, Cols: 100}
	notFound := fmt.Errorf("terminal/attach: %w", &runtimeclient.ResponseError{Code: runtimeprotocol.ErrorCodeTerminalNotFound, Message: "terminal not found"})
	for _, tt := range []struct {
		name           string
		phase          terminalPhase
		cursor         uint64
		result         *runtimeprotocol.TerminalAttachResult
		err            error
		isDisconnected bool // 结果到达时连接已经断开
		wantPhase      terminalPhase
		wantScreen     string
		wantRows       int // 非零时屏幕按这个行数换了新的
		wantKept       bool
	}{
		{"missing terminal marks exited", terminalReconnecting, 0, nil, notFound, false, terminalExited, "\n[终端已结束]", 0, true},
		{"connection lost keeps waiting", terminalReconnecting, 0, nil, fmt.Errorf("terminal/attach: %w", runtimeclient.ErrConnectionLost), false, terminalReconnecting, "", 0, true},
		{"other error is shown", terminalReconnecting, 0, nil, errors.New("cursor 9 is after the latest output 3"), false, terminalExited, "cursor 9 is after the latest output 3", 0, true},
		{"first attach uses PTY size", terminalReconnecting, 0, result, nil, false, terminalRunning, "", 30, false},
		{"reconnect keeps the screen", terminalReconnecting, 42, result, nil, false, terminalRunning, "", 0, true},
		{"result for a closed tab is ignored", terminalExited, 0, result, nil, false, terminalExited, "", 0, true},
		{"result after disconnect keeps waiting", terminalReconnecting, 0, result, nil, true, terminalReconnecting, "", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newTerminalTestDesktop(t)
			if tt.isDisconnected {
				d.connectionState = runtimeclient.ConnectionReconnecting
			}
			tab := addTerminalTab(d, "terminal-1", "a", tt.phase)
			tab.ReceivedCursor = tt.cursor
			screen := tab.State
			d.handleTerminalAttached(tab, tt.result, tt.err)
			if tab.Phase != tt.wantPhase || !strings.Contains(tab.State.Text(), tt.wantScreen) {
				t.Fatalf("phase=%v screen=%q", tab.Phase, tab.State.Text())
			}
			if (tab.State == screen) != tt.wantKept {
				t.Fatalf("screen kept = %v", tab.State == screen)
			}
			if tt.wantRows != 0 {
				rows, cols := tab.State.Size()
				if rows != 30 || cols != 100 || tab.ExecutorID != "ssh-a" || tab.Directory != "/srv/app" || tab.AttachedRows != 30 || tab.AttachedCols != 100 {
					t.Fatalf("size=%dx%d executor=%q directory=%q attached=%dx%d", rows, cols, tab.ExecutorID, tab.Directory, tab.AttachedRows, tab.AttachedCols)
				}
			}
		})
	}
}

// TestTerminalInputAndResize 检查只有运行中的标签把输入和尺寸变化发给 serve。
func TestTerminalInputAndResize(t *testing.T) {
	for _, tt := range []struct {
		name      string
		phase     terminalPhase
		wantWrite bool
	}{{"running", terminalRunning, true}, {"opening", terminalOpening, false}, {"exited", terminalExited, false}} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			tab := addTerminalTab(d, "terminal-1", "a", tt.phase)
			d.handleTerminalInput(tab, []byte("ls\r"))
			d.handleTerminalResize(tab, 30, 100)
			if got := len(client.written) == 1 && client.written[0] == "a:ls\r"; got != tt.wantWrite {
				t.Fatalf("written = %q", client.written)
			}
			if got := len(client.resized) == 1 && client.resized[0] == "a:30x100"; got != tt.wantWrite {
				t.Fatalf("resized = %q", client.resized)
			}
		})
	}
}

// TestCloseTerminal 检查关闭标签后选中项落到相邻标签，只有运行中的终端请求 serve 结束。
func TestCloseTerminal(t *testing.T) {
	for _, tt := range []struct {
		name       string
		phases     []terminalPhase
		active     int
		closeIndex int
		wantKeys   []string
		wantActive string
		wantClosed []string
	}{
		{"close active middle", []terminalPhase{terminalRunning, terminalRunning, terminalRunning}, 1, 1, []string{"terminal-0", "terminal-2"}, "terminal-2", []string{"id-1"}},
		{"close active last", []terminalPhase{terminalRunning, terminalRunning}, 1, 1, []string{"terminal-0"}, "terminal-0", []string{"id-1"}},
		{"close inactive", []terminalPhase{terminalRunning, terminalRunning}, 1, 0, []string{"terminal-1"}, "terminal-1", []string{"id-0"}},
		{"close reconnecting ends the terminal", []terminalPhase{terminalReconnecting}, 0, 0, nil, "", []string{"id-0"}},
		{"close exited needs no request", []terminalPhase{terminalExited}, 0, 0, nil, "", nil},
		{"close opening needs no request", []terminalPhase{terminalOpening}, 0, 0, nil, "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			var tabs []*terminalTab
			for i, phase := range tt.phases {
				id := ""
				if phase != terminalOpening {
					id = "id-" + string(rune('0'+i))
				}
				tabs = append(tabs, addTerminalTab(d, "terminal-"+string(rune('0'+i)), id, phase))
			}
			d.selectedTerminal = tabs[tt.active].Key
			d.closeTerminal(tabs[tt.closeIndex])
			d.tasks.Wait()
			var keys []string
			for _, tab := range d.terminals {
				keys = append(keys, tab.Key)
			}
			if !slices.Equal(keys, tt.wantKeys) || d.selectedTerminal != tt.wantActive || !slices.Equal(client.closed, tt.wantClosed) {
				t.Fatalf("keys=%v active=%q closed=%v", keys, d.selectedTerminal, client.closed)
			}
		})
	}
}

// TestGetTerminalLocation 检查新终端的位置：有对话取对话的 Executor 与目录，没有对话取新对话表单。
func TestGetTerminalLocation(t *testing.T) {
	for _, tt := range []struct {
		name                        string
		conversation                defs.Conversation
		wantExecutor, wantDirectory string
	}{
		{"new conversation form", defs.Conversation{}, "ssh-form", "/form"},
		{"conversation executor", defs.Conversation{ID: "c", ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: "ssh-a", WorkspaceDirectories: map[string]string{"ssh-a": "/srv/app", "local": "/home"}}}, "ssh-a", "/srv/app"},
		{"conversation default local", defs.Conversation{ID: "c", ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/home"}}}, "local", "/home"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.newExecutor, d.newDirectory = "ssh-form", "/form"
			d.conversation = tt.conversation
			executor, directory := d.getTerminalLocation()
			if executor != tt.wantExecutor || directory != tt.wantDirectory {
				t.Fatalf("location = %q %q", executor, directory)
			}
		})
	}
}

// TestOpenFirstTerminal 检查只有终端面板显示、已连接、运行中的终端列表已取得且没有标签时才自动新开；未建立 Client 时标签显示原因。
func TestOpenFirstTerminal(t *testing.T) {
	for _, tt := range []struct {
		name       string
		shown      string
		state      runtimeclient.ConnectionState
		known      bool
		existing   bool
		wantTabs   int
		wantScreen string
	}{
		{"other panel", panelRPC, runtimeclient.ConnectionConnected, true, false, 0, ""},
		{"not connected", panelTerminal, runtimeclient.ConnectionConnecting, true, false, 0, ""},
		{"first_terminal_waits_for_list", panelTerminal, runtimeclient.ConnectionConnected, false, false, 0, ""},
		{"already has terminal", panelTerminal, runtimeclient.ConnectionConnected, true, true, 1, ""},
		{"connected without client", panelTerminal, runtimeclient.ConnectionConnected, true, false, 1, "\n[尚未连接到本机服务]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			t.Cleanup(d.closeTerminalStates)
			d.shownBottomPanel, d.connectionState, d.terminalsKnown = tt.shown, tt.state, tt.known
			if tt.existing {
				addTerminalTab(d, "terminal-0", "a", terminalRunning)
			}
			d.openFirstTerminal()
			if len(d.terminals) != tt.wantTabs {
				t.Fatalf("tabs = %d", len(d.terminals))
			}
			if tt.wantScreen != "" && d.terminals[0].State.Text() != tt.wantScreen {
				t.Fatalf("screen = %q", d.terminals[0].State.Text())
			}
		})
	}
}

// TestTerminalPanelView 检查终端面板在无标签和有标签时都能真实构建，标签标题取目录名。
func TestTerminalPanelView(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  runtimeclient.ConnectionState
		phases []terminalPhase
		want   []string
		deny   []string
	}{
		{"no terminal", runtimeclient.ConnectionConnecting, nil, []string{"没有终端"}, nil},
		{"tab title", runtimeclient.ConnectionConnecting, []terminalPhase{terminalRunning}, []string{"local · repo"}, []string{"重新启动", "local · repo (2)"}},
		{"duplicate titles get a suffix", runtimeclient.ConnectionConnecting, []terminalPhase{terminalRunning, terminalRunning}, []string{"local · repo", "local · repo (2)"}, []string{"重新启动"}},
		{"exited terminal offers a restart", runtimeclient.ConnectionConnecting, []terminalPhase{terminalExited}, []string{"重新启动"}, []string{"正在重连"}},
		{"reconnecting terminal explains the wait", runtimeclient.ConnectionReconnecting, []terminalPhase{terminalReconnecting}, []string{"连接已断开，正在重连"}, []string{"重新启动"}},
		{"attaching terminal on a live connection shows no notice", runtimeclient.ConnectionConnected, []terminalPhase{terminalReconnecting}, []string{"local · repo"}, []string{"正在重连", "重新启动"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newTerminalTestDesktop(t)
			d.preferences.Workspace.Bottom = kit.DockState{Open: true, ActivePanel: panelTerminal}
			d.connectionState = tt.state // 未连接时避免面板显示时自动新开终端
			for index, phase := range tt.phases {
				addTerminalTab(d, fmt.Sprintf("terminal-%d", index+1), fmt.Sprintf("id-%d", index+1), phase)
			}
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			for _, want := range tt.want {
				if !tester.HasText(want) {
					t.Fatalf("missing %q in %q", want, tester.Texts())
				}
			}
			for _, deny := range tt.deny {
				if tester.HasText(deny) {
					t.Fatalf("unexpected %q in %q", deny, tester.Texts())
				}
			}
		})
	}
}

// TestRestartTerminal 检查点击「重新启动」：同一个标签换新的屏幕、清掉旧终端 ID，沿用 Executor 与目录；未连接时再次标为已结束并说明原因。
func TestRestartTerminal(t *testing.T) {
	d, _ := newTerminalTestDesktop(t)
	d.preferences.Workspace.Bottom = kit.DockState{Open: true, ActivePanel: panelTerminal}
	d.connectionState = runtimeclient.ConnectionConnecting
	tab := addTerminalTab(d, "terminal-1", "old-id", terminalExited)
	oldState := tab.State
	tester := ui.NewTester(d.view, 1280, 820)
	tester.SetLocale("zh-Hans")
	if err := tester.Click("重新启动"); err != nil {
		t.Fatal(err)
	}
	if len(d.terminals) != 1 || d.terminals[0] != tab || tab.State == oldState || tab.TerminalID != "" || tab.ExecutorID != "local" || tab.Directory != "/work/repo" {
		t.Fatalf("restart did not reuse the tab: tabs=%d state replaced=%v id=%q executor=%q directory=%q", len(d.terminals), tab.State != oldState, tab.TerminalID, tab.ExecutorID, tab.Directory)
	}
	if tab.Phase != terminalExited || !strings.Contains(tab.State.Text(), "尚未连接到本机服务") {
		t.Fatalf("phase=%v screen=%q", tab.Phase, tab.State.Text())
	}
}

// TestTerminalTabsClose 检查在界面里点击第一个标签的关闭按钮：关闭发生在遍历标签之后，不会 panic，
// 剩下第二、第三个标签，选中项按被关闭的是否为选中标签移动。
func TestTerminalTabsClose(t *testing.T) {
	for _, tt := range []struct {
		name         string
		firstPhase   terminalPhase
		selected     string
		wantSelected string
	}{
		{"close first while last selected", terminalRunning, "terminal-3", "terminal-3"},
		{"close first while first selected", terminalRunning, "terminal-1", "terminal-2"},
		{"close first while it waits to attach", terminalReconnecting, "terminal-3", "terminal-3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			d.preferences.Workspace.Bottom = kit.DockState{Open: true, ActivePanel: panelTerminal}
			d.connectionState = runtimeclient.ConnectionConnecting // 避免面板显示时自动新开终端
			addTerminalTab(d, "terminal-1", "id-1", tt.firstPhase)
			for i := 2; i <= 3; i++ {
				addTerminalTab(d, fmt.Sprintf("terminal-%d", i), fmt.Sprintf("id-%d", i), terminalRunning)
			}
			d.selectedTerminal = tt.selected
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			// 第一个“关闭终端”按钮属于第一个标签。
			closeFirst, ok := tester.Find("关闭终端")
			if !ok {
				t.Fatalf("close button missing in %q", tester.Texts())
			}
			tester.ClickAt(closeFirst.X+closeFirst.W/2, closeFirst.Y+closeFirst.H/2)
			for range 3 {
				tester.Frame()
			}
			d.tasks.Wait()
			var keys []string
			for _, tab := range d.terminals {
				keys = append(keys, tab.Key)
			}
			if !slices.Equal(keys, []string{"terminal-2", "terminal-3"}) || d.selectedTerminal != tt.wantSelected || !slices.Equal(client.closed, []string{"id-1"}) {
				t.Fatalf("keys=%v selected=%q closed=%v", keys, d.selectedTerminal, client.closed)
			}
		})
	}
}

// TestTerminalOutput 检查输出写进对应标签并记下位置；截断且屏幕已有内容时换新屏幕，首次接入的截断只做标记；最后一批回放输出到达前后输入都被丢弃。
func TestTerminalOutput(t *testing.T) {
	for _, tt := range []struct {
		name         string
		cursor       uint64
		event        *runtimeclient.TerminalOutput
		wantScreen   string
		wantCursor   uint64
		wantSameness bool // 屏幕是否还是原来那一个
		wantTruncate bool
	}{
		{"live output advances the cursor", 5, &runtimeclient.TerminalOutput{Data: []byte("live"), NextCursor: 9}, "oldlive", 9, true, false},
		{"truncated first attach keeps the screen", 0, &runtimeclient.TerminalOutput{Data: []byte("new"), NextCursor: 3, Truncated: true}, "oldnew", 3, true, true},
		{"truncated reconnect starts a new screen", 5, &runtimeclient.TerminalOutput{Data: []byte("new"), NextCursor: 8, Truncated: true}, "new", 8, false, true},
		{"empty first batch", 0, &runtimeclient.TerminalOutput{NextCursor: 0}, "old", 0, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newTerminalTestDesktop(t)
			tab := addTerminalTab(d, "terminal-1", "a", terminalRunning)
			tab.State.Write([]byte("old"))
			tab.ReceivedCursor = tt.cursor
			screen := tab.State
			tt.event.TerminalID = "a"
			d.handleTerminalOutput(tt.event)
			if got := tab.State.Text(); got != tt.wantScreen || tab.ReceivedCursor != tt.wantCursor || (tab.State == screen) != tt.wantSameness || tab.WasTruncated != tt.wantTruncate {
				t.Fatalf("screen=%q cursor=%d same=%v truncated=%v", got, tab.ReceivedCursor, tab.State == screen, tab.WasTruncated)
			}
		})
	}
}

// TestTerminalReplayDropsAutoReplies 检查回放期间终端产生的输入（自动应答）不发给 serve：回放的最后一批输出到达后仍然丢弃，等 100ms 的结束通知回到主线程后才恢复。
func TestTerminalReplayDropsAutoReplies(t *testing.T) {
	d, client := newTerminalTestDesktop(t)
	tab := addTerminalTab(d, "terminal-1", "a", terminalRunning)
	tab.IsReplaying = true
	d.handleTerminalInput(tab, []byte("\x1b[1;1R"))
	d.handleTerminalOutput(&runtimeclient.TerminalOutput{TerminalID: "a", Data: []byte("prompt$ "), NextCursor: 8, HasMore: true})
	d.handleTerminalOutput(&runtimeclient.TerminalOutput{TerminalID: "a", NextCursor: 8})
	d.tasks.Wait()
	d.handleTerminalInput(tab, []byte("\x1b[1;9R"))
	if !tab.IsReplaying || len(client.written) != 0 {
		t.Fatalf("replaying=%v written=%q", tab.IsReplaying, client.written)
	}
	d.handleTerminalReplayFinished(tab)
	d.handleTerminalInput(tab, []byte("ls\r"))
	if tab.IsReplaying || !slices.Equal(client.written, []string{"a:ls\r"}) {
		t.Fatalf("replaying=%v written=%q", tab.IsReplaying, client.written)
	}
}

// TestTerminalReplayFinished 检查回放结束时屏幕尺寸与 PTY 尺寸的处理：不同则发屏幕尺寸，相同且被截断才改两次尺寸重画，相同且没截断什么都不发。
func TestTerminalReplayFinished(t *testing.T) {
	for _, tt := range []struct {
		name         string
		attachedRows int
		attachedCols int
		truncated    bool
		phase        terminalPhase
		wantResized  []string
	}{
		{"screen differs from PTY", 30, 100, false, terminalRunning, []string{"a:24x80"}},
		{"same size and truncated redraws", 24, 80, true, terminalRunning, []string{"a:23x80", "a:24x80"}},
		{"same size and complete", 24, 80, false, terminalRunning, nil},
		{"exited during the quiet period", 30, 100, true, terminalExited, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			tab := addTerminalTab(d, "terminal-1", "a", tt.phase)
			tab.IsReplaying, tab.WasTruncated = true, tt.truncated
			tab.AttachedRows, tab.AttachedCols = tt.attachedRows, tt.attachedCols
			d.handleTerminalReplayFinished(tab)
			d.tasks.Wait()
			if !slices.Equal(client.resized, tt.wantResized) {
				t.Fatalf("resized = %v, want %v", client.resized, tt.wantResized)
			}
			if tt.phase == terminalRunning && tab.IsReplaying {
				t.Fatal("replay did not finish")
			}
		})
	}
}

// TestRunningTerminalsBecomeTabs 检查运行列表里没有标签的终端加成 Dock 标签并接入，已有标签不重复；列表未知时已有标签不受影响；
// 有 terminal/open 请求在进行时不加标签，打开完成后重新同步；已关掉的标签（打开中关掉、关闭请求进行中）不被列表加回来。
func TestRunningTerminalsBecomeTabs(t *testing.T) {
	items := func(ids ...string) []runtimeprotocol.TerminalItem {
		var list []runtimeprotocol.TerminalItem
		for _, id := range ids {
			list = append(list, runtimeprotocol.TerminalItem{TerminalID: id, ExecutorID: "local", Directory: "/work/" + id})
		}
		return list
	}
	for _, tt := range []struct {
		name     string
		existing []string
		opening  bool
		// closeBefore 非空时先关掉这个标签（"opening" 是打开中的标签，其余是终端 ID），再收到列表。
		closeBefore  string
		event        *runtimeclient.TerminalsChanged
		wantIDs      []string
		wantAttached []string
		// wantFinalIDs 是打开请求返回之后（没有打开中的标签时不检查）的标签；wantClosed 是整个过程中请求 serve 结束的终端。
		wantFinalIDs []string
		wantClosed   []string
	}{
		{name: "new terminals get tabs", existing: []string{"a"}, event: &runtimeclient.TerminalsChanged{IsKnown: true, Terminals: items("a", "b", "c")}, wantIDs: []string{"a", "b", "c"}, wantAttached: []string{"b:0", "c:0"}},
		{name: "new tabs retain server order", existing: []string{"a"}, event: &runtimeclient.TerminalsChanged{IsKnown: true, Terminals: items("a", "c", "b")}, wantIDs: []string{"a", "c", "b"}, wantAttached: []string{"c:0", "b:0"}},
		{name: "same list adds nothing", existing: []string{"a"}, event: &runtimeclient.TerminalsChanged{IsKnown: true, Terminals: items("a")}, wantIDs: []string{"a"}},
		{name: "unknown list keeps tabs", existing: []string{"a"}, event: &runtimeclient.TerminalsChanged{Err: errors.New("lost")}, wantIDs: []string{"a"}},
		{name: "list_before_open_result", opening: true, event: &runtimeclient.TerminalsChanged{IsKnown: true, Terminals: items("opened")}, wantIDs: []string{""}, wantFinalIDs: []string{"opened"}},
		{name: "closed_while_opening_adds_no_tab", opening: true, closeBefore: "opening", event: &runtimeclient.TerminalsChanged{IsKnown: true, Terminals: items("opened")}, wantFinalIDs: []string{}, wantClosed: []string{"opened"}},
		{name: "closing_terminal_is_not_added_back", existing: []string{"a", "b"}, closeBefore: "a", event: &runtimeclient.TerminalsChanged{IsKnown: true, Terminals: items("a", "b")}, wantIDs: []string{"b"}, wantClosed: []string{"a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			d.selectedTerminal = ""
			tabs := map[string]*terminalTab{}
			for _, id := range tt.existing {
				tabs[id] = addTerminalTab(d, "terminal-"+id, id, terminalRunning)
			}
			if tt.opening {
				tabs["opening"] = addTerminalTab(d, "terminal-opening", "", terminalOpening)
				tabs["opening"].OpenRequest = 1
				d.openingTerminalRequests = 1
			}
			if tt.closeBefore != "" {
				d.closeTerminal(tabs[tt.closeBefore])
			}
			d.handleClientEvent(tt.event)
			d.tasks.Wait()
			var ids []string
			for _, tab := range d.terminals {
				ids = append(ids, tab.TerminalID)
			}
			// 标签保持列表顺序；并发接入记录排序后比较，仍核对数量和每次接入的参数。
			attached := slices.Sorted(slices.Values(client.attached))
			wantAttached := slices.Sorted(slices.Values(tt.wantAttached))
			if tt.wantIDs != nil && (!slices.Equal(ids, tt.wantIDs) || !slices.Equal(attached, wantAttached)) {
				t.Fatalf("tabs=%v attached=%v", ids, client.attached)
			}
			for _, tab := range d.terminals {
				if !tt.opening && !slices.Contains(tt.existing, tab.TerminalID) && (tab.Phase != terminalReconnecting || tab.Directory != "/work/"+tab.TerminalID) {
					t.Fatalf("new tab %s phase=%v directory=%q", tab.TerminalID, tab.Phase, tab.Directory)
				}
			}
			// 打开请求返回后重新同步：列表里的终端已有这个标签，或者标签已被关掉，都不再多加。
			if tt.opening {
				d.handleTerminalOpened(client, tabs["opening"], 1, &runtimeprotocol.TerminalOpenResult{TerminalID: "opened", ExecutorID: "local", Directory: "/work/opened"}, nil)
				d.tasks.Wait()
				ids = nil
				for _, tab := range d.terminals {
					ids = append(ids, tab.TerminalID)
				}
				if !slices.Equal(ids, tt.wantFinalIDs) || len(client.attached) != 0 || d.openingTerminalRequests != 0 {
					t.Fatalf("final tabs=%v attached=%v opening requests=%d", ids, client.attached, d.openingTerminalRequests)
				}
			}
			if !slices.Equal(client.closed, tt.wantClosed) {
				t.Fatalf("closed = %v, want %v", client.closed, tt.wantClosed)
			}
		})
	}
}

// TestTerminalsChangedOpensFirstTerminal 检查列表由未知变为已知时没有标签就新开第一个；已知后列表再变化不会在用户关掉最后一个标签后又新开。
func TestTerminalsChangedOpensFirstTerminal(t *testing.T) {
	for _, tt := range []struct {
		name      string
		known     bool
		list      []runtimeprotocol.TerminalItem
		wantTabs  int
		wantOpens int
	}{
		{"first known list without terminals", false, nil, 1, 1},
		{"first known list with a running terminal", false, []runtimeprotocol.TerminalItem{{TerminalID: "a", ExecutorID: "local"}}, 1, 0},
		{"later list after the last tab was closed", true, nil, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			d.shownBottomPanel, d.terminalsKnown = panelTerminal, tt.known
			d.handleClientEvent(&runtimeclient.TerminalsChanged{IsKnown: true, Terminals: tt.list})
			d.tasks.Wait()
			if len(d.terminals) != tt.wantTabs || len(client.opened) != tt.wantOpens {
				t.Fatalf("tabs=%d opened=%d", len(d.terminals), len(client.opened))
			}
		})
	}
}

// TestOpenTerminalRequest 检查打开终端的请求带上当前对话，没有对话时为空。
func TestOpenTerminalRequest(t *testing.T) {
	for _, tt := range []struct {
		name           string
		conversationID defs.ConversationID
	}{{"no conversation", ""}, {"with conversation", "conversation-1"}} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			d.conversation = defs.Conversation{ID: tt.conversationID}
			d.openTerminal()
			d.tasks.Wait()
			if len(client.opened) != 1 || client.opened[0].ConversationID != tt.conversationID {
				t.Fatalf("opened = %+v", client.opened)
			}
		})
	}
}

// TestQuitKeepsTerminals 检查退出时只释放本地屏幕，不请求 serve 结束运行中的终端。
func TestQuitKeepsTerminals(t *testing.T) {
	d, client := newTerminalTestDesktop(t)
	addTerminalTab(d, "terminal-1", "a", terminalRunning)
	addTerminalTab(d, "terminal-2", "b", terminalReconnecting)
	d.closeTerminalStates()
	d.tasks.Wait()
	if len(client.closed) != 0 {
		t.Fatalf("closed = %v", client.closed)
	}
}

// TestResetTerminal 检查命令面板的「重置终端」作用于选中的运行中终端：重置后隔 100ms 把行数减一再改回；其他阶段不发请求。
func TestResetTerminal(t *testing.T) {
	for _, tt := range []struct {
		name        string
		phase       terminalPhase
		wantReset   []string
		wantResized []string
	}{
		{"running", terminalRunning, []string{"a"}, []string{"a:23x80", "a:24x80"}},
		{"reconnecting", terminalReconnecting, nil, nil},
		{"exited", terminalExited, nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			addTerminalTab(d, "terminal-1", "a", tt.phase)
			d.runCommand(commandResetTerminal)
			d.tasks.Wait()
			if !slices.Equal(client.reset, tt.wantReset) || !slices.Equal(client.resized, tt.wantResized) {
				t.Fatalf("reset=%v resized=%v", client.reset, client.resized)
			}
		})
	}
}
