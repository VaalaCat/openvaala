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
