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
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/egoist/mygo/ui"
)

// terminalClient 记录终端相关调用，其余 Client 方法不会被这些测试用到。
type terminalClient struct {
	runtimeclient.Client
	mu      sync.Mutex
	resized []string
	written []string
	closed  []string
}

func (c *terminalClient) record(list *[]string, entry string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*list = append(*list, entry)
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

// TestTerminalDisconnected 检查连接断开把运行中和打开中的标签标为已结束，重新连上不改变它们。
func TestTerminalDisconnected(t *testing.T) {
	for _, state := range []runtimeclient.ConnectionState{runtimeclient.ConnectionReconnecting, runtimeclient.ConnectionConnecting} {
		t.Run(string(state), func(t *testing.T) {
			d, _ := newTerminalTestDesktop(t)
			running := addTerminalTab(d, "terminal-1", "a", terminalRunning)
			opening := addTerminalTab(d, "terminal-2", "", terminalOpening)
			d.handleClientEvent(&runtimeclient.ConnectionChanged{State: state})
			for _, tab := range []*terminalTab{running, opening} {
				if tab.Phase != terminalExited || tab.State.Text() != "\n[连接已断开]" {
					t.Fatalf("%s phase=%v screen=%q", tab.Key, tab.Phase, tab.State.Text())
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

// TestOpenFirstTerminal 检查只有终端面板显示、已连接且没有标签时才自动新开；未建立 Client 时标签显示原因。
func TestOpenFirstTerminal(t *testing.T) {
	for _, tt := range []struct {
		name       string
		shown      string
		state      runtimeclient.ConnectionState
		existing   bool
		wantTabs   int
		wantScreen string
	}{
		{"other panel", panelRPC, runtimeclient.ConnectionConnected, false, 0, ""},
		{"not connected", panelTerminal, runtimeclient.ConnectionConnecting, false, 0, ""},
		{"already has terminal", panelTerminal, runtimeclient.ConnectionConnected, true, 1, ""},
		{"connected without client", panelTerminal, runtimeclient.ConnectionConnected, false, 1, "\n[尚未连接到本机服务]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			t.Cleanup(d.closeTerminalStates)
			d.shownBottomPanel, d.connectionState = tt.shown, tt.state
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
		phases []terminalPhase
		want   []string
		deny   []string
	}{
		{"no terminal", nil, []string{"没有终端"}, nil},
		{"tab title", []terminalPhase{terminalRunning}, []string{"local · repo"}, []string{"重新启动", "local · repo (2)"}},
		{"duplicate titles get a suffix", []terminalPhase{terminalRunning, terminalRunning}, []string{"local · repo", "local · repo (2)"}, []string{"重新启动"}},
		{"exited terminal offers a restart", []terminalPhase{terminalExited}, []string{"重新启动"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := newTerminalTestDesktop(t)
			d.preferences.Workspace.Bottom = kit.DockState{Open: true, ActivePanel: panelTerminal}
			d.connectionState = runtimeclient.ConnectionConnecting // 避免面板显示时自动新开终端
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
		selected     string
		wantSelected string
	}{
		{"close first while last selected", "terminal-3", "terminal-3"},
		{"close first while first selected", "terminal-1", "terminal-2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, client := newTerminalTestDesktop(t)
			d.preferences.Workspace.Bottom = kit.DockState{Open: true, ActivePanel: panelTerminal}
			d.connectionState = runtimeclient.ConnectionConnecting // 避免面板显示时自动新开终端
			for i := 1; i <= 3; i++ {
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
