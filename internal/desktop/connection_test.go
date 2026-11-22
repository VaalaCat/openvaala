package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/egoist/mygo/ui"
)

// TestInitialServerDraft 检查首次识别服务器保留输入，已知服务器草稿不互相迁移。
func TestInitialServerDraft(t *testing.T) {
	// 1. 原生输入写入未知服务器草稿后，服务身份到达仍显示原内容。
	for _, tt := range []struct {
		name, initial string
		visible       bool
	}{{"first identity", "", true}, {"different known server", "original", false}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.serverID = tt.initial
			tester := ui.NewTester(d.view, 1280, 820)
			if err := tester.Click("消息输入"); err != nil {
				t.Fatal(err)
			}
			tester.Type("typing before connection")
			original := d.drafts[draftKey{ServerID: tt.initial}]
			d.setServerIdentity("connected")
			tester.Frame()
			text := draftText(d.drafts[draftKey{ServerID: "connected"}].Content)
			if (text == "typing before connection") != tt.visible {
				t.Fatalf("draft=%q", text)
			}
			if tt.visible && d.drafts[draftKey{ServerID: "connected"}] != original {
				t.Fatal("draft identity changed")
			}
			if !tt.visible && d.drafts[draftKey{ServerID: tt.initial}] != original {
				t.Fatal("known server draft migrated")
			}
		})
	}
}

// TestSessionSourceFilters 检查根子 Session 的迟到事件不会写入新页面。
func TestSessionSourceFilters(t *testing.T) {
	for _, tt := range []struct {
		name         string
		conversation defs.ConversationID
		session      defs.SessionID
		want         bool
	}{{"same", "conversation", "root", true}, {"child", "conversation", "child", false}, {"other", "other", "root", false}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
			if got := d.isReading(tt.conversation, tt.session); got != tt.want {
				t.Fatalf("match=%v", got)
			}
		})
	}
}

// TestConnectionBanner 检查对话区顶部的连接横幅：断线、服务退出、重连各有本地化原因，连接正常和初次连接中不显示。
func TestConnectionBanner(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(d *Desktop)
		want    string
	}{
		{"connected", func(d *Desktop) {}, ""},
		{"first connection", func(d *Desktop) { d.connecting = true }, ""},
		{"first connection dialing", func(d *Desktop) { d.connecting, d.connectionState = true, runtimeclient.ConnectionConnecting }, ""},
		{"connection lost", func(d *Desktop) {
			d.connectionState, d.connectionError = runtimeclient.ConnectionReconnecting, fmt.Errorf("read: %w", runtimeclient.ErrConnectionLost)
		}, "与本机服务的连接已断开"},
		{"server exited", func(d *Desktop) {
			d.connectionState, d.connectionError = runtimeclient.ConnectionConnecting, errors.New("runtime server exited (exit status 1)")
		}, "本机服务已退出"},
		{"cannot connect", func(d *Desktop) {
			d.connectionState, d.connectionError = runtimeclient.ConnectionConnecting, errors.New("dial tcp: connection refused")
		}, "无法连接本机服务"},
		{"reconnecting without error", func(d *Desktop) { d.connectionState = runtimeclient.ConnectionReconnecting }, "正在重新连接本机服务…"},
		{"manual retry in flight", func(d *Desktop) {
			d.connecting, d.connectionError = true, errors.New("dial tcp: connection refused")
		}, "无法连接本机服务"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tt.prepare(d)
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			texts := tester.Texts()
			if tt.want == "" {
				if slices.Contains(texts, "重试") || tester.HasText("正在重新连接本机服务…") {
					t.Fatalf("banner shown: %q", texts)
				}
				return
			}
			if !tester.HasText(tt.want) {
				t.Fatalf("banner reason %q missing in %q", tt.want, texts)
			}
			// 横幅在对话区顶部：原因文字在对话标题下方、消息列左边线上。
			title, _ := tester.Find("新对话")
			reason, _ := tester.Find(tt.want)
			if reason.Y <= title.Y {
				t.Fatalf("banner is not below the header: title %+v reason %+v", title, reason)
			}
		})
	}
}

// TestConnectionStatusBar 检查状态栏只显示连接状态：圆点加简短状态文字，没有错误原文，也没有「重试」（重试只在对话区横幅里）。
func TestConnectionStatusBar(t *testing.T) {
	const raw = "dial tcp: connection refused"
	for _, tt := range []struct {
		name       string
		prepare    func(d *Desktop)
		wantStatus string
	}{
		{"connected", func(d *Desktop) {}, "已连接"},
		{"reconnecting", func(d *Desktop) { d.connectionState = runtimeclient.ConnectionReconnecting }, "正在重连…"},
		{"failed", func(d *Desktop) {
			d.connectionState, d.connectionError = runtimeclient.ConnectionConnecting, errors.New(raw)
		}, "连接失败"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tt.prepare(d)
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			// 状态栏的文字排在侧栏开关之后（文档顺序里最后一段）。
			texts := tester.Texts()
			start := slices.Index(texts, "切换侧栏")
			if start < 0 {
				t.Fatalf("status bar not found: %q", texts)
			}
			bar := texts[start:]
			if !slices.Contains(bar, tt.wantStatus) || slices.Contains(bar, "重试") || slices.ContainsFunc(bar, func(s string) bool { return strings.Contains(s, raw) }) {
				t.Fatalf("status bar texts %q, want %q only", bar, tt.wantStatus)
			}
		})
	}
}

// TestDesktopSwitchServer 检查切服取消旧连接、清空旧会话数据、保留按 serverID 隔离的草稿，并在远端失败横幅提供「切回本机」。
func TestDesktopSwitchServer(t *testing.T) {
	// 1. 准备带远端档案和本机草稿的桌面状态。
	d := testDesktop()
	d.preferencesPath = filepath.Join(t.TempDir(), "preferences.json")
	d.preferences.RemoteServers = []remoteServer{{
		ID:    "remote-1",
		Name:  "公司开发机",
		URL:   "ws://10.0.0.5:8790/api/v1/openvaala",
		Token: "tok",
	}}
	connCtx, connCancel := context.WithCancel(context.Background())
	d.clientCtx = connCtx
	d.clientCancel = connCancel
	d.conversations = []dao.ConversationListEntry{{Conversation: defs.Conversation{ID: "c1", Title: "旧会话"}}}
	d.drafts[draftKey{ServerID: "fixture"}] = &conversationDraft{
		Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "本机未发送草稿"}}},
	}

	// 2. 切到远端：旧连接被取消、旧会话列表清空、草稿保留、偏好持久化。
	d.switchServer("remote-1")
	if connCtx.Err() != context.Canceled {
		t.Fatalf("previous client context not canceled: %v", connCtx.Err())
	}
	if len(d.conversations) != 0 {
		t.Fatalf("conversations not cleared: %+v", d.conversations)
	}
	if got := draftText(d.drafts[draftKey{ServerID: "fixture"}].Content); got != "本机未发送草稿" {
		t.Fatalf("local draft lost: %q", got)
	}
	if d.preferences.ActiveServer != "remote-1" {
		t.Fatalf("ActiveServer = %q, want remote-1", d.preferences.ActiveServer)
	}

	// 3. 远端失败时对话区横幅显示远端名称和「切回本机」，点击后切回本机。
	d.connectionError = errors.New("dial tcp: connection refused")
	tester := ui.NewTester(d.view, 1280, 820)
	tester.SetLocale("zh-Hans")
	if !tester.HasText("无法连接「公司开发机」") {
		t.Fatalf("missing remote banner text in %q", tester.Texts())
	}
	if !tester.HasText("切回本机") {
		t.Fatalf("missing switch-to-local button in %q", tester.Texts())
	}
	if err := tester.Click("切回本机"); err != nil {
		t.Fatalf("click switch-to-local: %v", err)
	}
	if d.preferences.ActiveServer != localServerID {
		t.Fatalf("ActiveServer after switch back = %q, want %q", d.preferences.ActiveServer, localServerID)
	}
}

// TestServersDialogPasteAndSave 检查「管理服务器」弹窗粘贴 openvaala web 链接时自动拆出 URL 与 Token，并可保存。
func TestServersDialogPasteAndSave(t *testing.T) {
	d := testDesktop()
	d.preferencesPath = filepath.Join(t.TempDir(), "preferences.json")
	d.openServersDialog()
	d.openServerForm(nil)
	d.serverForm.Name = "云端 GPU"
	d.serverForm.URL = "https://gpu.vaala.dev/web/v1/#token=paste-secret"
	d.handleServerAddressChanged()
	if d.serverForm.URL != "wss://gpu.vaala.dev/api/v1/openvaala" || d.serverForm.Token != "paste-secret" || !d.serverForm.TokenAutoFilled {
		t.Fatalf("form after paste = %+v", d.serverForm)
	}
	d.saveServerForm(false)
	if len(d.preferences.RemoteServers) != 1 || d.preferences.RemoteServers[0].Name != "云端 GPU" || d.preferences.RemoteServers[0].Token != "paste-secret" {
		t.Fatalf("saved servers = %+v", d.preferences.RemoteServers)
	}
}
