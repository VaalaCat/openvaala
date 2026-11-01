package desktop

import (
	"context"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/egoist/mygo/ui"
)

// testDesktop 准备无窗口原生视图的业务数据。
func testDesktop() *Desktop {
	// 1. 设置与真实 Run 相同的主线程容器，不启动后台任务。
	return &Desktop{ctx: context.Background(), preferences: defaultPreferences(), translations: loadTranslations(), serverID: "fixture", newAgent: "default", newExecutor: "local", drafts: map[draftKey]*conversationDraft{}, turns: map[defs.TurnID]defs.Turn{}, images: map[string]*imageResource{}, skillCatalogs: map[skillCatalogKey]*skillCatalog{}, itemExpanded: map[string]bool{}, unreadConversations: map[defs.ConversationID]bool{}, connectionState: runtimeclient.ConnectionConnected}
}

// TestDesktopViews 验证主要工作区在正常和窄窗口中能真实构建。
func TestDesktopViews(t *testing.T) {
	// 1. 每页用原生 Tester 绘制，检查业务文字而非源码结构。
	for _, tt := range []struct {
		name, page, want string
		width            int
	}{{"new", "", "想一起完成什么？", 1280}, {"narrow", "", "发送", 680}, {"thread", "", "原生工作台已经完成", 1280}, {"thread-dark", "", "原生工作台已经完成", 1280}, {"general", "general", "恢复默认外观", 1280}, {"provider", "providers", "Provider ID", 1280}, {"hooks", "hooks", "Hook", 1280}, {"archive", "", "没有归档的对话", 1280}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.page = tt.page
			d.archived = tt.name == "archive"
			if tt.name == "thread" || tt.name == "thread-dark" {
				d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root", Title: "OpenVaala 原生工作台"}
				d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
				d.readingReady = true
				d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "root", ConversationID: "conversation", AgentName: "default"}}}
				d.turns["turn"] = defs.Turn{ID: "turn", Status: defs.TurnStatusCompleted}
				d.items = []viewItem{{Ordinal: 1, Item: &defs.Item{ID: "user", Kind: defs.ItemMessage, Message: &defs.Message{From: &defs.Address{Kind: defs.IdentityUser}, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "检查原生桌面布局和代码显示"}}}}}}, {Ordinal: 2, Item: &defs.Item{ID: "reasoning", TurnID: "turn", Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{Summary: []string{"检查布局与基础控件。"}}}}, {Ordinal: 3, Item: &defs.Item{ID: "reply", TurnID: "turn", Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "原生工作台已经完成\n\n使用连续分隔区域、共享主题与原生输入。\n\n```go\nfunc main() {\n    fmt.Println(\"OpenVaala\")\n}\n```\n\n| 组件 | 状态 |\n| --- | --- |\n| 对话工作区 | 已连接 |\n| 模型服务 | 可配置 |"}}}}}}}
				if tt.name == "thread-dark" {
					d.preferences.Theme = "dark"
				}
			}
			if tt.page == "providers" {
				d.providerDraft = providerToDraft(defs.ProviderConfig{ID: "sample", Protocol: "openai_chat", Models: map[string]*defs.ProviderModelConfig{"model": {}}})
				d.providerBaseline = cloneProviderDraft(d.providerDraft)
			}
			tester := ui.NewTester(d.view, tt.width, 820)
			tester.SetLocale("zh-Hans")
			if !tester.HasText(tt.want) {
				t.Fatalf("missing %q in %q", tt.want, tester.Texts())
			}
			if dir := os.Getenv("OPENVAALA_DESKTOP_TEST_CAPTURE"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				file, err := os.Create(filepath.Join(dir, "workbench-"+tt.name+".png"))
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := png.Encode(file, tester.Image()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestDisconnectedActions 检查未建立 Client 的真实页面仍能编辑，不启动写请求。
func TestDisconnectedActions(t *testing.T) {
	// 1. 通过原生控件点击测试连接失败时的设置和日志入口。
	for _, name := range []string{"provider", "rpc"} {
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			d.connectionState = runtimeclient.ConnectionConnecting
			if name == "provider" {
				d.page = "providers"
				d.providerDraft = providerToDraft(defs.ProviderConfig{ID: "valid"})
				d.providerBaseline = providerToDraft(defs.ProviderConfig{})
			} else {
				d.preferences.Workspace.Right = kit.DockState{Open: true, ActivePanel: panelRPC}
			}
			tester := ui.NewTester(d.view, 1280, 820)
			if name == "provider" {
				_ = tester.Click("应用")
				if d.providerSaving {
					t.Fatal("disconnected provider submitted")
				}
			} else {
				_ = tester.Click("暂停")
				_ = tester.Click("清空")
				if d.rpcPaused {
					t.Fatal("disconnected RPC action ran")
				}
			}
		})
	}
}

// TestDesktopUnsavedChecks 检查所有会话及仅图片草稿都触发退出确认。
func TestDesktopUnsavedChecks(t *testing.T) {
	// 1. 不依赖当前选择会话判断未发送内容。
	for _, tt := range []struct {
		name    string
		content []*defs.Content
		want    bool
	}{{"empty", nil, false}, {"text", []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "draft"}}}, true}, {"image", []*defs.Content{{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "data:image/png;base64,AA=="}}}, true}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.drafts[draftKey{"other-server", "other-conversation"}] = &conversationDraft{Content: tt.content}
			if got := d.isUnsavedWorkPresent(); got != tt.want {
				t.Fatalf("unsaved=%v want=%v", got, tt.want)
			}
		})
	}
}
