package desktop

import (
	"math"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

// TestModelPickerSearch 检查模型搜索仅在弹层内出现，并可按服务或显示名选择真实值。
func TestModelPickerSearch(t *testing.T) {
	// 1. 从真实工作台打开搜索，保留筛选后的模型值而不是显示名。
	for _, query := range []string{"second/model", "Second model"} {
		t.Run(query, func(t *testing.T) {
			d := testDesktop()
			d.models.Models = []*defs.ModelDefinition{{Ref: defs.ModelRef{ProviderID: "first", ModelID: "model"}, DisplayName: "First model"}, {Ref: defs.ModelRef{ProviderID: "second", ModelID: "model"}, DisplayName: "Second model"}}
			tester := ui.NewTester(d.view, 1280, 820)
			if tester.HasText("筛选服务 / 模型") {
				t.Fatal("model search is permanently occupying the composer")
			}
			if err := tester.Click("模型"); err != nil {
				t.Fatal(err)
			}
			if err := tester.Click("筛选服务 / 模型"); err != nil {
				t.Fatal(err)
			}
			tester.Type(query)
			if err := tester.Click("Second model"); err != nil {
				t.Fatal(err)
			}
			if d.newModel != "second/model" {
				t.Fatalf("model=%q", d.newModel)
			}
		})
	}
}

// TestSidebarGroups 检查侧栏按日期分组、显示第二行说明，并区分运行中与未读。
func TestSidebarGroups(t *testing.T) {
	now := time.Now()
	conv := func(id string, ago time.Duration) dao.ConversationListEntry {
		return dao.ConversationListEntry{Conversation: defs.Conversation{ID: defs.ConversationID(id), Title: "title " + id, UpdatedAt: now.Add(-ago).UnixMilli(), ExecutorSettings: defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/work/openvaala"}}}}
	}
	for _, tt := range []struct {
		name    string
		running []defs.ConversationID
		unread  map[defs.ConversationID]bool
		want    []string
		absent  []string
	}{
		{"groups", nil, nil, []string{"更早", "title old", "openvaala · 1 周前"}, []string{"未读", "运行中"}},
		{"unread", nil, map[defs.ConversationID]bool{"old": true}, []string{"未读"}, []string{"运行中"}},
		{"running", []defs.ConversationID{"old"}, map[defs.ConversationID]bool{"old": true}, []string{"运行中"}, []string{"未读"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.conversations = []dao.ConversationListEntry{conv("new", time.Minute), conv("old", 10*24*time.Hour)}
			d.runningConversations = tt.running
			for id := range tt.unread {
				d.unreadConversations[id] = true
			}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.sidebarView(c) }) }, 320, 600)
			tester.SetLocale("zh-Hans")
			for _, want := range append(tt.want, "今天", "title new", "openvaala · 1 分钟前") {
				if !tester.HasText(want) {
					t.Fatalf("missing %q: %q", want, tester.Texts())
				}
			}
			for _, absent := range tt.absent {
				if tester.HasText(absent) {
					t.Fatalf("unexpected %q: %q", absent, tester.Texts())
				}
			}
		})
	}
}

// TestSidebarLoadingHint 检查「加载中…」只在列表还没有内容时出现，刷新和翻页保留旧列表且不插提示行。
func TestSidebarLoadingHint(t *testing.T) {
	for _, tt := range []struct {
		name    string
		entries int
		loading bool
		want    bool
	}{
		{"first load", 0, true, true},
		{"refresh keeps the list", 1, true, false},
		{"idle", 1, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.conversationLoading = tt.loading
			for range tt.entries {
				d.conversations = append(d.conversations, dao.ConversationListEntry{Conversation: defs.Conversation{ID: "existing", Title: "existing title"}})
			}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.sidebarView(c) }) }, 320, 600)
			tester.SetLocale("zh-Hans")
			if tester.HasText("加载中…") != tt.want {
				t.Fatalf("loading hint = %v, texts %q", !tt.want, tester.Texts())
			}
		})
	}
}

// TestBuildSidebarRows 检查分组标题只在分组变化处插入，并保持对话顺序。
func TestBuildSidebarRows(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	entry := func(id string, ago time.Duration) dao.ConversationListEntry {
		return dao.ConversationListEntry{Conversation: defs.Conversation{ID: defs.ConversationID(id), UpdatedAt: now.Add(-ago).UnixMilli()}}
	}
	for _, tt := range []struct {
		name    string
		entries []dao.ConversationListEntry
		want    []string
	}{
		{"empty", nil, []string{}},
		{"one group", []dao.ConversationListEntry{entry("a", time.Hour), entry("b", 2*time.Hour)}, []string{"group:group.today", "a", "b"}},
		{"all groups", []dao.ConversationListEntry{entry("a", time.Hour), entry("b", 20*time.Hour), entry("c", 3*24*time.Hour), entry("d", 30*24*time.Hour)}, []string{"group:group.today", "a", "group:group.yesterday", "b", "group:group.week", "c", "group:group.earlier", "d"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := buildSidebarRows(tt.entries, now)
			got := []string{}
			for _, row := range rows {
				got = append(got, row.Key)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("keys=%v want %v", got, tt.want)
			}
			for index := range got {
				if got[index] != tt.want[index] {
					t.Fatalf("keys=%v want %v", got, tt.want)
				}
			}
		})
	}
}

// TestBuildMarkdownExport 检查导出按角色分节、合并连续同角色消息，工具调用列为列表项。
func TestBuildMarkdownExport(t *testing.T) {
	message := func(id string, user bool, text string) viewItem {
		message := &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}
		if user {
			message.From = &defs.Address{Kind: defs.IdentityUser}
		}
		return viewItem{Item: &defs.Item{ID: defs.ItemID(id), Kind: defs.ItemMessage, Message: message}}
	}
	call := func(name, args string) viewItem {
		return viewItem{Item: &defs.Item{ID: "call-" + defs.ItemID(name), Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: name, Name: name, Arguments: args}}}
	}
	for _, tt := range []struct {
		name  string
		items []viewItem
		want  string
	}{
		{"empty", nil, "# T\n"},
		{"conversation", []viewItem{message("1", true, "你好"), call("bash", `{"command":"ls -la"}`), call("read_file", `{"path":"a.go"}`), message("2", false, "完成")}, "# T\n\n## U\n\n你好\n\n## A\n\n- `bash` ls -la\n- `read_file` a.go\n\n完成\n"},
		{"consecutive assistant", []viewItem{message("1", false, "第一段"), message("2", false, "第二段")}, "# T\n\n## A\n\n第一段\n\n第二段\n"},
		{"tool without arguments", []viewItem{call("list_agents", `{}`)}, "# T\n\n## A\n\n- `list_agents`\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildMarkdownExport("T", "U", "A", tt.items); got != tt.want {
				t.Fatalf("markdown=%q want %q", got, tt.want)
			}
		})
	}
}

// TestConversationGrouping 检查侧栏分组按本地日期计算。
func TestConversationGrouping(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	for _, tt := range []struct {
		name      string
		updatedAt int64
		group     string
	}{
		{"just now", ago(10 * time.Second), "group.today"},
		{"two hours", ago(2 * time.Hour), "group.today"},
		{"earlier today", time.Date(2026, 10, 31, 0, 1, 0, 0, time.UTC).UnixMilli(), "group.today"},
		{"yesterday evening", time.Date(2026, 10, 30, 23, 59, 0, 0, time.UTC).UnixMilli(), "group.yesterday"},
		{"three days", ago(3 * 24 * time.Hour), "group.week"},
		{"six days", ago(6 * 24 * time.Hour), "group.week"},
		{"seven days", ago(7 * 24 * time.Hour), "group.earlier"},
		{"two months", ago(60 * 24 * time.Hour), "group.earlier"},
		{"two years", ago(800 * 24 * time.Hour), "group.earlier"},
		{"future clock skew", now.Add(time.Minute).UnixMilli(), "group.today"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getConversationGroup(tt.updatedAt, now); got != tt.group {
				t.Fatalf("group=%q want %q", got, tt.group)
			}
		})
	}
}

// TestConversationSubtitle 检查第二行说明取默认 Executor 的目录名，缺目录时只显示相对时间。
func TestConversationSubtitle(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	updated := now.Add(-2 * time.Hour).UnixMilli()
	for _, tt := range []struct {
		name     string
		settings defs.ExecutorSettings
		want     string
	}{
		{"local directory", defs.ExecutorSettings{WorkspaceDirectories: map[string]string{"local": "/home/me/openvaala"}}, "openvaala · 2 小时前"},
		{"default executor", defs.ExecutorSettings{DefaultExecutorID: "remote", WorkspaceDirectories: map[string]string{"local": "/a", "remote": "/srv/app"}}, "app · 2 小时前"},
		{"no directory", defs.ExecutorSettings{}, "2 小时前"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conv := defs.Conversation{UpdatedAt: updated, ExecutorSettings: tt.settings}
			if got := getConversationSubtitle(conv, now, getTestTranslator("zh-CN")); got != tt.want {
				t.Fatalf("subtitle=%q want %q", got, tt.want)
			}
		})
	}
}

// TestConversationTooltip 检查侧栏行悬停提示：目录总是显示，标题被截断时第一行补上完整标题。
func TestConversationTooltip(t *testing.T) {
	for _, tt := range []struct {
		name, title, directory string
		truncated              bool
		want                   string
	}{
		{"directory only", "短标题", "/home/me/repo", false, "/home/me/repo"},
		{"truncated title and directory", "很长的标题", "/home/me/repo", true, "很长的标题\n/home/me/repo"},
		{"truncated title without directory", "很长的标题", "", true, "很长的标题"},
		{"nothing to show", "短标题", "", false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := getConversationTooltip(tt.title, tt.directory, tt.truncated); got != tt.want {
				t.Fatalf("tooltip=%q want %q", got, tt.want)
			}
		})
	}
}

// newSidebarTestDesktop 返回侧栏里有三个对话的 Desktop，并画出第一帧。
func newSidebarTestDesktop(search string) (*Desktop, *ui.Tester) {
	d := testDesktop()
	d.conversationSearch = search
	for _, id := range []defs.ConversationID{"c1", "c2", "c3"} {
		d.conversations = append(d.conversations, dao.ConversationListEntry{Conversation: defs.Conversation{ID: id, RootSessionID: defs.SessionID("root-" + id), Title: "对话 " + string(id), UpdatedAt: time.Now().UnixMilli()}})
	}
	tester := ui.NewTester(d.view, 1280, 820)
	tester.SetLocale("zh-Hans")
	return d, tester
}

// TestSidebarSearchKeyboard 检查搜索框：Up、Down 移动高亮，Enter 打开高亮的对话，Esc 和清除按钮清空搜索。
func TestSidebarSearchKeyboard(t *testing.T) {
	for _, tt := range []struct {
		name          string
		search        string
		keys          []ui.Key
		wantHighlight defs.ConversationID
		wantOpened    defs.ConversationID
		wantSearch    string
	}{
		{"down highlights the first", "对话", []ui.Key{ui.KeyDown}, "c1", "", "对话"},
		{"down twice highlights the second", "对话", []ui.Key{ui.KeyDown, ui.KeyDown}, "c2", "", "对话"},
		{"down stops at the last", "对话", []ui.Key{ui.KeyDown, ui.KeyDown, ui.KeyDown, ui.KeyDown}, "c3", "", "对话"},
		{"up from nothing goes to the last", "对话", []ui.Key{ui.KeyUp}, "c3", "", "对话"},
		{"enter opens the highlighted", "对话", []ui.Key{ui.KeyDown, ui.KeyDown, ui.KeyEnter}, "", "c2", "对话"},
		{"enter without highlight opens the first match", "对话", []ui.Key{ui.KeyEnter}, "", "c1", "对话"},
		{"enter on an empty search does nothing", "", []ui.Key{ui.KeyEnter}, "", "", ""},
		{"escape clears the search", "对话", []ui.Key{ui.KeyEscape}, "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, tester := newSidebarTestDesktop(tt.search)
			d.runCommand(commandSearch)
			tester.Frame()
			for _, key := range tt.keys {
				tester.Key(0, key)
			}
			if d.conversationHighlight != tt.wantHighlight || d.readingTarget.ConversationID != tt.wantOpened || d.conversationSearch != tt.wantSearch {
				t.Fatalf("highlight=%q opened=%q search=%q", d.conversationHighlight, d.readingTarget.ConversationID, d.conversationSearch)
			}
		})
	}
}

// TestSidebarHeader 检查侧栏标题栏：归档开关改变列表来源和标题，清除按钮只在有搜索词时出现并清空它。
func TestSidebarHeader(t *testing.T) {
	t.Run("archive toggle", func(t *testing.T) {
		d, tester := newSidebarTestDesktop("")
		if err := tester.Click("显示归档"); err != nil {
			t.Fatal(err)
		}
		if !d.archived {
			t.Fatal("archived view not selected")
		}
		tester.Frame()
		if !tester.HasText("归档") {
			t.Fatalf("header title is not 归档: %q", tester.Texts())
		}
		if err := tester.Click("返回对话"); err != nil || d.archived {
			t.Fatalf("conversation view not restored: %v", err)
		}
	})
	t.Run("clear button", func(t *testing.T) {
		d, tester := newSidebarTestDesktop("abc")
		if err := tester.Click("清除搜索"); err != nil {
			t.Fatal(err)
		}
		if d.conversationSearch != "" {
			t.Fatalf("search = %q", d.conversationSearch)
		}
		tester.Frame()
		if tester.HasText("清除搜索") {
			t.Fatal("clear button still shown for an empty search")
		}
	})
	t.Run("archived empty state", func(t *testing.T) {
		d := testDesktop()
		d.archived = true
		tester := ui.NewTester(d.view, 1280, 820)
		tester.SetLocale("zh-Hans")
		if !tester.HasText("没有归档的对话") {
			t.Fatalf("texts = %q", tester.Texts())
		}
	})
}

// TestConversationTitleEditing 检查标题栏：点击标题进入行内编辑，Esc 取消，Enter 结束；子 Session 显示面包屑而不是可改的标题。
func TestConversationTitleEditing(t *testing.T) {
	newReading := func() (*Desktop, *ui.Tester) {
		d := testDesktop()
		d.conversation = defs.Conversation{ID: "c1", RootSessionID: "root", Title: "原标题"}
		d.readingTarget = readingTarget{ConversationID: "c1", SessionID: "root"}
		d.readingReady = true
		d.preferences.Workspace.Right.Open = false
		d.sessionInfos = []runtime.SessionRuntimeInfo{
			{Session: defs.Session{ID: "root", ConversationID: "c1", AgentName: "default"}},
			{Session: defs.Session{ID: "child", ConversationID: "c1", ParentSessionID: "root", TaskName: "子任务"}},
		}
		tester := ui.NewTester(d.view, 1280, 820)
		tester.SetLocale("zh-Hans")
		return d, tester
	}
	for _, tt := range []struct {
		name        string
		run         func(*testing.T, *Desktop, *ui.Tester)
		wantEditing bool
	}{
		{"click starts editing", func(t *testing.T, d *Desktop, tester *ui.Tester) {
			if err := tester.Click("原标题"); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"escape cancels", func(t *testing.T, d *Desktop, tester *ui.Tester) {
			_ = tester.Click("原标题")
			tester.Frame()
			tester.Key(0, ui.KeyEscape)
		}, false},
		{"enter ends editing", func(t *testing.T, d *Desktop, tester *ui.Tester) {
			_ = tester.Click("原标题")
			tester.Frame()
			tester.Key(0, ui.KeyEnter)
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, tester := newReading()
			tt.run(t, d, tester)
			if (d.titleEditingID == "c1") != tt.wantEditing {
				t.Fatalf("editing = %q", d.titleEditingID)
			}
		})
	}
	t.Run("child session shows breadcrumbs", func(t *testing.T) {
		d, tester := newReading()
		d.readingTarget.SessionID = "child"
		tester.Frame()
		for _, want := range []string{"主 Agent", "子任务"} {
			if !tester.HasText(want) {
				t.Fatalf("missing %q: %q", want, tester.Texts())
			}
		}
		// 手测：多级子 Agent 时每一级祖先都能点，窄窗口里面包屑被截断但当前一级最后可见。
		if err := tester.Click("主 Agent"); err != nil {
			t.Fatal(err)
		}
		if d.readingTarget.SessionID != "root" {
			t.Fatalf("reading session = %q", d.readingTarget.SessionID)
		}
	})
}

// TestContextRingEnabledWhenReady 检查用量读到后圆环按钮可用（点击打开上下文面板），没读到时只画底环、不是按钮。
func TestContextRingEnabledWhenReady(t *testing.T) {
	// 1. 用真实对话绘制输入区，用点击效果判断按钮是否被禁用。
	for _, tt := range []struct {
		name           string
		ready          bool
		wantButton     bool
		wantRightPanel string
	}{{"ready", true, true, panelContext}, {"not ready", false, false, panelAgent}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
			d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
			d.readingReady = tt.ready
			d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "root", ConversationID: "conversation", AgentName: "default"}, Context: runtime.ContextUsage{Source: runtime.ContextUsageSourceActual, Tokens: 90, Window: 100}}}
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			if got := tester.HasText("上下文用量"); got != tt.wantButton {
				t.Fatalf("ring button shown = %v, want %v", got, tt.wantButton)
			}
			if tt.wantButton {
				if err := tester.Click("上下文用量"); err != nil {
					t.Fatal(err)
				}
			}
			if got := d.preferences.Workspace.Right.ActivePanel; got != tt.wantRightPanel {
				t.Fatalf("right panel = %q, want %q", got, tt.wantRightPanel)
			}
		})
	}
}

// TestNewConversationEmptyState 检查新对话的空状态水平居中在对话区；连上服务但没有模型时改成引导，主按钮同样居中，点击打开设置的模型服务页并展开服务目录。
func TestNewConversationEmptyState(t *testing.T) {
	for _, tt := range []struct {
		name         string
		width        int
		modelsLoaded bool
		wantTitle    string
	}{
		{"wide window", 1600, true, "想一起完成什么？"},
		{"narrow window", 700, true, "想一起完成什么？"},
		{"models not loaded yet", 1280, false, "想一起完成什么？"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.preferences.Workspace.SidebarOpen = false
			d.preferences.Workspace.Right.Open = false
			d.modelsLoaded = tt.modelsLoaded
			d.models = runtime.ModelList{Models: []*defs.ModelDefinition{{Ref: defs.ModelRef{ProviderID: "p", ModelID: "m"}}}}
			tester := ui.NewTester(d.view, tt.width, 820)
			tester.SetLocale("zh-Hans")
			empty, found := tester.Find(tt.wantTitle)
			if !found || math.Abs(float64(empty.X+empty.W/2)-float64(tt.width)/2) > 1.5 {
				t.Fatalf("empty state %v (found %v) is not centered in a %d wide window", empty, found, tt.width)
			}
		})
	}
	t.Run("onboarding", func(t *testing.T) {
		d := testDesktop()
		d.preferences.Workspace.SidebarOpen = false
		d.preferences.Workspace.Right.Open = false
		d.modelsLoaded = true
		tester := ui.NewTester(d.view, 1280, 820)
		tester.SetLocale("zh-Hans")
		onboarding, found := tester.Find("还没有模型服务")
		add, addFound := tester.Find("添加模型服务")
		if !found || !addFound || math.Abs(float64(onboarding.X+onboarding.W/2)-640) > 1.5 || math.Abs(float64(add.X+add.W/2)-640) > 1.5 || add.Y < onboarding.Y+onboarding.H {
			t.Fatalf("onboarding %v (found %v) and button %v (found %v) are not centered", onboarding, found, add, addFound)
		}
		if err := tester.Click("添加模型服务"); err != nil {
			t.Fatal(err)
		}
		if d.page != "providers" || !d.catalogOpen {
			t.Fatalf("page=%q catalogOpen=%v", d.page, d.catalogOpen)
		}
	})
}

// TestSendWithoutModel 检查没有模型时发送失败：提示是中文「还没有模型服务」，旁边有同一个「添加模型服务」入口。
func TestSendWithoutModel(t *testing.T) {
	d := testDesktop()
	d.modelsLoaded = true
	d.models = runtime.ModelList{Models: []*defs.ModelDefinition{{Ref: defs.ModelRef{ProviderID: "p", ModelID: "m"}}}}
	d.drafts[draftKey{ServerID: "fixture"}] = &conversationDraft{Error: d.getErrorMessage(&runtimeclient.ResponseError{Code: -32000, Message: "no model is configured; run openvaala"}), NeedsProvider: true}
	tester := ui.NewTester(d.view, 1280, 820)
	tester.SetLocale("zh-Hans")
	if !tester.HasText("还没有模型服务") {
		t.Fatalf("texts = %q", tester.Texts())
	}
	if err := tester.Click("添加模型服务"); err != nil || d.page != "providers" {
		t.Fatalf("page=%q err=%v", d.page, err)
	}
}
