package desktop

import (
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/conversation"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/egoist/mygo/ui"
)

// newWorkLocationTestDesktop 返回已打开一个对话、有 local、openvaala 和 sandbox 三个 Executor 的桌面。
func newWorkLocationTestDesktop() *Desktop {
	d := testDesktop()
	d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
	d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root", ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: "local", WorkspaceDirectories: map[string]string{"local": "/work/project"}}}
	d.readingReady = true
	d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "root", ConversationID: "conversation", AgentName: "reviewer"}}}
	d.executors = []conversation.ExecutorDetails{{ID: "local", Description: "本机", WorkspaceDirectory: "/work"}, {ID: "openvaala", Description: "内置"}, {ID: "sandbox", Description: "沙箱", WorkspaceDirectory: "/sandbox"}}
	return d
}

// TestWorkLocationDialog 检查对话工作位置弹窗的三页：不出现内置 openvaala，MCP 显示状态和失败原因，Skill 显示名称和描述。
func TestWorkLocationDialog(t *testing.T) {
	servers := []conversation.MCPServerInfo{
		{ExecutorID: "local", ConnectionInfo: mcpclient.ConnectionInfo{ServerID: "fake", State: mcpclient.ConnectionStateConnected, Tools: []mcpclient.ConnectionTool{{Name: "echo"}}}},
		{ExecutorID: "local", ConnectionInfo: mcpclient.ConnectionInfo{ServerID: "broken", State: mcpclient.ConnectionStateFailed, Error: "exec: not found"}},
		{ExecutorID: "sandbox", ConnectionInfo: mcpclient.ConnectionInfo{ServerID: "slow", State: mcpclient.ConnectionStateConnecting}},
	}
	for _, tt := range []struct {
		name    string
		tab     int
		servers []conversation.MCPServerInfo
		catalog *skillCatalog
		want    []string
		missing []string
	}{
		{"work location", 0, nil, nil, []string{"Agent", "reviewer", "默认 Executor", "目录", "可用的 Executor", "local", "sandbox", "保存"}, []string{"openvaala", "内置"}},
		{"mcp states", 1, servers, nil, []string{"fake", "已连接", "local · 1 个工具", "broken", "失败", "local · exec: not found", "slow", "连接中"}, []string{"没有 MCP 服务"}},
		{"mcp empty", 1, nil, nil, []string{"没有 MCP 服务"}, nil},
		{"skills", 2, nil, &skillCatalog{Skills: []skill.SkillInfo{{Name: "pdf-reader", Description: "读取 PDF"}}}, []string{"pdf-reader", "读取 PDF"}, []string{"没有可用的 Skill"}},
		{"skills empty", 2, nil, &skillCatalog{}, []string{"没有可用的 Skill"}, nil},
		{"skills loading", 2, nil, &skillCatalog{Loading: true}, []string{"加载中…"}, []string{"没有可用的 Skill"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newWorkLocationTestDesktop()
			d.openWorkLocation()
			d.locationTab, d.mcpServers = tt.tab, tt.servers
			if tt.catalog != nil {
				d.skillCatalogs[getSkillCatalogKey("conversation", "", "", "")] = tt.catalog
			}
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			texts := tester.Texts()
			for _, name := range tt.want {
				if !slices.Contains(texts, name) {
					t.Fatalf("%q is missing in %q", name, texts)
				}
			}
			for _, name := range tt.missing {
				if slices.Contains(texts, name) {
					t.Fatalf("%q must not be shown in %q", name, texts)
				}
			}
		})
	}
}

// TestWorkLocationDialogSections 检查弹窗顶部的分段切换：点击 MCP、Skill 后分别显示对应的页。
func TestWorkLocationDialogSections(t *testing.T) {
	for _, tt := range []struct {
		name    string
		clicks  []string
		want    string
		wantTab int
	}{
		{"mcp", []string{"MCP"}, "没有 MCP 服务", 1},
		{"skill", []string{"Skill"}, "没有可用的 Skill", 2},
		{"skill then mcp", []string{"Skill", "MCP"}, "没有 MCP 服务", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newWorkLocationTestDesktop()
			d.openWorkLocation()
			d.skillCatalogs[getSkillCatalogKey("conversation", "", "", "")] = &skillCatalog{}
			d.mcpLoading = false
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			for _, label := range tt.clicks {
				if err := tester.Click(label); err != nil {
					t.Fatalf("click %q: %v", label, err)
				}
			}
			if d.locationTab != tt.wantTab || !slices.Contains(tester.Texts(), tt.want) {
				t.Fatalf("tab=%d want %d, %q missing in %q", d.locationTab, tt.wantTab, tt.want, tester.Texts())
			}
		})
	}
}

// TestNewConversationFields 检查新对话的工作位置表单：每个控件有标签，Executor 不含内置 openvaala，没有「候选」「解析」按钮；
// agent/list 不可用时 Agent 是带标签的文本框。
func TestNewConversationFields(t *testing.T) {
	for _, tt := range []struct {
		name   string
		agents []runtime.AgentInfo
		err    error
		want   []string
	}{
		{"agent select with description", []runtime.AgentInfo{{Name: "default", Description: "通用助手", DefaultModel: defs.ModelRef{ProviderID: "p", ModelID: "m"}}}, nil, []string{"Agent", "通用助手 · p/m", "Executor", "目录"}},
		{"agent list unavailable", nil, runtime.ErrConversationNotFound, []string{"Agent", "Executor", "目录"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newWorkLocationTestDesktop()
			d.conversation, d.readingTarget = defs.Conversation{}, readingTarget{ServerID: "fixture"}
			d.agents, d.agentsError = tt.agents, tt.err
			d.workLocationPopoverOpen = true
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			texts := tester.Texts()
			for _, name := range tt.want {
				if !slices.Contains(texts, name) {
					t.Fatalf("%q is missing in %q", name, texts)
				}
			}
			for _, name := range []string{"候选", "解析", "openvaala"} {
				if slices.Contains(texts, name) {
					t.Fatalf("%q must not be shown in %q", name, texts)
				}
			}
		})
	}
}

// TestGetSelectableExecutors 检查工作位置可选的 Executor 不含内置 openvaala，列表未读到时只有 local。
func TestGetSelectableExecutors(t *testing.T) {
	for _, tt := range []struct {
		name      string
		executors []conversation.ExecutorDetails
		want      []string
	}{
		{"hides builtin", []conversation.ExecutorDetails{{ID: "local"}, {ID: "openvaala"}, {ID: "sandbox"}}, []string{"local", "sandbox"}},
		{"not loaded yet", nil, []string{"local"}},
		{"only builtin", []conversation.ExecutorDetails{{ID: "openvaala"}}, []string{"local"}},
		{"keeps memory for the available switches", []conversation.ExecutorDetails{{ID: "local"}, {ID: "memory", Kind: defs.ExecutorKindMemory}}, []string{"local", "memory"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.executors = tt.executors
			got := []string{}
			for _, executor := range d.getSelectableExecutors() {
				got = append(got, executor.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("executors=%v want %v", got, tt.want)
			}
		})
	}
}

// TestGetDefaultExecutorChoices 检查默认 Executor 的选项不含 memory 和内置 openvaala，memory 只能当可用的 Executor。
func TestGetDefaultExecutorChoices(t *testing.T) {
	for _, tt := range []struct {
		name      string
		executors []conversation.ExecutorDetails
		want      []string
	}{
		{"hides memory and builtin", []conversation.ExecutorDetails{{ID: "local"}, {ID: "openvaala"}, {ID: "memory", Kind: defs.ExecutorKindMemory}, {ID: "sandbox"}}, []string{"local", "sandbox"}},
		{"memory only gives no choice", []conversation.ExecutorDetails{{ID: "memory", Kind: defs.ExecutorKindMemory}}, []string{}},
		{"not loaded yet", nil, []string{"local"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.executors = tt.executors
			got := []string{}
			for _, executor := range d.getDefaultExecutorChoices() {
				got = append(got, executor.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("choices=%v want %v", got, tt.want)
			}
		})
	}
}

// TestWorkLocationDialogMemory 检查弹窗里 memory 只出现在「可用的 Executor」开关里，不进默认 Executor 的分段。
func TestWorkLocationDialogMemory(t *testing.T) {
	// 1. 默认 Executor 分段里每个选项出现两次，「可用的 Executor」里每项一行；memory 只能有那一行。
	d := newWorkLocationTestDesktop()
	d.executors = append(d.executors, conversation.ExecutorDetails{ID: "memory", Kind: defs.ExecutorKindMemory, Description: "跨对话记忆"})
	d.openWorkLocation()
	tester := ui.NewTester(d.view, 1280, 820)
	tester.SetLocale("zh-Hans")
	counts := map[string]int{}
	for _, text := range tester.Texts() {
		counts[text]++
	}
	// 2. memory 的开关还在，用户能在这里关掉记忆；分段里只有 local 和 sandbox。
	if counts["memory · 可用"] != 1 || counts["memory"] != 1 || counts["sandbox"] != counts["memory"]+2 {
		t.Fatalf("memory appears %d times (available switch %d), sandbox %d times; want memory only in the available switches", counts["memory"], counts["memory · 可用"], counts["sandbox"])
	}
}

// newRecentConversations 返回按更新时间从新到旧排列的最近对话：local 的 /work/a、/work/b、/other/c 和 ssh 的 /srv/app。
func newRecentConversations() []defs.Conversation {
	withLocation := func(id, executorID, directory string) defs.Conversation {
		return defs.Conversation{ID: defs.ConversationID(id), ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: executorID, WorkspaceDirectories: map[string]string{executorID: directory}}}
	}
	return []defs.Conversation{
		withLocation("a", "local", "/work/a"),
		withLocation("ssh", "ssh", "/srv/app"),
		withLocation("b", "local", "/work/b"),
		withLocation("c", "local", "/other/c"),
		withLocation("a-again", "local", "/work/a"),
	}
}

// TestHandleRecentConversationsLoaded 检查最近对话读到后：总是保存，只在新对话表单还没动过、也没打开对话时用第一条预填。
func TestHandleRecentConversationsLoaded(t *testing.T) {
	ssh := defs.Conversation{ID: "ssh", ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: "ssh", WorkspaceDirectories: map[string]string{"ssh": "/srv/app"}}}
	local := defs.Conversation{ID: "local", ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/work/a"}}}
	for _, tt := range []struct {
		name                        string
		executor, directory         string
		viewing                     defs.ConversationID
		loaded                      []defs.Conversation
		wantExecutor, wantDirectory string
	}{
		{"untouched form is prefilled", "local", "", "", []defs.Conversation{ssh, local}, "ssh", "/srv/app"},
		{"empty default executor is local", "local", "", "", []defs.Conversation{local}, "local", "/work/a"},
		{"chosen executor is kept", "sandbox", "", "", []defs.Conversation{ssh}, "sandbox", ""},
		{"typed directory is kept", "local", "/typed", "", []defs.Conversation{ssh}, "local", "/typed"},
		{"viewing a conversation is kept", "local", "", "open", []defs.Conversation{ssh}, "local", ""},
		{"no recent conversation", "local", "", "", nil, "local", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.newExecutor, d.newDirectory = tt.executor, tt.directory
			d.conversation = defs.Conversation{ID: tt.viewing}
			d.handleRecentConversationsLoaded(tt.loaded)
			if d.newExecutor != tt.wantExecutor || d.newDirectory != tt.wantDirectory {
				t.Fatalf("form = %q %q, want %q %q", d.newExecutor, d.newDirectory, tt.wantExecutor, tt.wantDirectory)
			}
			if len(d.recentConversations) != len(tt.loaded) {
				t.Fatalf("recent conversations = %d, want %d", len(d.recentConversations), len(tt.loaded))
			}
		})
	}
}

// TestDirectoryFieldRecentDirectories 检查点击目录输入框打开候选：最近用过的目录在子目录前面，只列所选 Executor 的目录；
// 打过字之后「最近用过」按输入前缀过滤；选中最近用过的目录直接填入并关闭，选中子目录进入下一层。
func TestDirectoryFieldRecentDirectories(t *testing.T) {
	for _, tt := range []struct {
		name        string
		isDialog    bool
		directory   string
		typed       string
		clicked     string
		want        []string
		missing     []string
		wantInput   string
		wantOrdered bool
	}{
		{name: "prefilled value shows every recent directory", directory: "/work/a", want: []string{"/work/a", "/work/b", "/other/c", "alpha/"}, missing: []string{"/srv/app", "beta/"}, wantInput: "/work/a", wantOrdered: true},
		{name: "typed prefix keeps matching recent directories", typed: "/work/", want: []string{"/work/a", "/work/b", "alpha/", "beta/"}, missing: []string{"/other/c", "/srv/app"}, wantInput: "/work/", wantOrdered: true},
		{name: "typed prefix without a match hides recent directories", typed: "/none", missing: []string{"/work/a", "/work/b", "/other/c"}, wantInput: "/none"},
		{name: "recent directory is filled and closes the menu", directory: "/work/a", clicked: "/other/c", missing: []string{"/work/b", "alpha/"}, wantInput: "/other/c"},
		{name: "dialog lists recent directories of its executor", isDialog: true, clicked: "/work/b", missing: []string{"/srv/app", "/other/c"}, wantInput: "/work/b"},
		{name: "subdirectory goes one level down", typed: "/work/", clicked: "beta/", missing: []string{"/work/b", "alpha/"}, wantInput: "/work/beta/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newWorkLocationTestDesktop()
			d.conversation, d.readingTarget = defs.Conversation{}, readingTarget{ServerID: "fixture"}
			d.workLocationPopoverOpen = true
			d.newDirectory = tt.directory
			d.recentConversations = newRecentConversations()
			if tt.isDialog {
				d.conversation, d.readingTarget = newWorkLocationTestDesktop().conversation, readingTarget{ServerID: "fixture", ConversationID: "conversation", SessionID: "root"}
				d.openWorkLocation()
			}
			d.directoryListing = directoryListing{ExecutorID: "local", Parent: "/work/", Directory: "/work", Names: []string{"alpha", "beta"}}
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			// 弹窗打开时目录框已自动聚焦，但候选要等点击输入框才打开。
			if tt.isDialog && slices.Contains(tester.Texts(), tt.clicked) {
				t.Fatalf("%q is shown before clicking the directory field", tt.clicked)
			}
			clickDirectoryInput(t, tester)
			tester.Type(tt.typed)
			if tt.clicked != "" {
				if err := tester.Click(tt.clicked); err != nil {
					t.Fatal(err)
				}
			}
			texts := tester.Texts()
			for _, name := range tt.want {
				if !slices.Contains(texts, name) {
					t.Fatalf("%q is missing in %q", name, texts)
				}
			}
			for _, name := range tt.missing {
				if slices.Contains(texts, name) {
					t.Fatalf("%q must not be shown in %q", name, texts)
				}
			}
			if tt.wantOrdered && !slices.IsSortedFunc(tt.want, func(a, b string) int { return slices.Index(texts, a) - slices.Index(texts, b) }) {
				t.Fatalf("candidates are not in order %q within %q", tt.want, texts)
			}
			directory := d.newDirectory
			if tt.isDialog {
				directory = d.locationDirectories["local"]
			}
			if directory != tt.wantInput {
				t.Fatalf("directory = %q, want %q", directory, tt.wantInput)
			}
		})
	}
}

// clickDirectoryInput 点击目录输入框本身：输入框在字段标签「目录」正下方，点标签只会聚焦、不打开候选。
func clickDirectoryInput(t *testing.T, tester *ui.Tester) {
	t.Helper()
	// 1. 标签和输入框之间隔一个 Space(1)，点输入框的垂直中线。
	label, ok := tester.Find("目录")
	if !ok {
		t.Fatal("directory label is missing")
	}
	tester.ClickAt(label.X+label.W/2, label.Y+label.H+18)
}
