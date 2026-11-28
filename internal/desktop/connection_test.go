package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/egoist/mygo"
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

// TestConnectionStatusBar 检查状态栏只显示服务器名字，按钮的无障碍名称保留连接状态。
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
			d.localServer = remoteServer{ID: localServerID, Name: "Workstation", URL: "ws://localhost:8790"}
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
			if !slices.Contains(bar, "切换服务器 · Workstation · "+tt.wantStatus) || !slices.Contains(bar, "Workstation") || slices.Contains(bar, "localhost:8790") || slices.Contains(bar, "重试") || slices.ContainsFunc(bar, func(s string) bool { return strings.Contains(s, raw) }) {
				t.Fatalf("status bar texts %q, want server name with accessible status %q", bar, tt.wantStatus)
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

// TestSwitchServerWithAgentDraft 检查切服先确认是否放弃未保存的 Agent 配置。
func TestSwitchServerWithAgentDraft(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprintf("dirty=%t", dirty), func(t *testing.T) {
			d := testDesktop()
			d.preferencesPath = filepath.Join(t.TempDir(), "preferences.json")
			d.setAgentDraft(defs.AgentConfig{Name: "worker"}, "worker")
			if dirty {
				d.agentDraft.definition.Description = "unsaved"
			}
			d.switchServer("remote-1")
			if dirty {
				if !d.confirmOpen || d.confirm.Kind != "switch-server" || d.agentDraft == nil || d.preferences.ActiveServer == "remote-1" {
					t.Fatal("switch discarded the Agent draft before confirmation")
				}
				d.submitConfirmation()
			}
			if d.preferences.ActiveServer != "remote-1" || d.agentDraft != nil {
				t.Fatal("switch did not clear the previous server's Agent draft")
			}
		})
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
	if d.serverForm.URL != "wss://gpu.vaala.dev/api/v1/openvaala" || d.serverForm.Token != "paste-secret" {
		t.Fatalf("form after paste = %+v", d.serverForm)
	}
	d.saveServerForm(false)
	if len(d.preferences.RemoteServers) != 1 || d.preferences.RemoteServers[0].Name != "云端 GPU" || d.preferences.RemoteServers[0].Token != "paste-secret" {
		t.Fatalf("saved servers = %+v", d.preferences.RemoteServers)
	}
}

// historyPageClient 记录桌面阅读器自己发出的分页请求。
type historyPageClient struct {
	runtimeclient.Client
	requests []runtimeclient.ListItemsRequest
	opened   chan runtimeclient.OpenConversationRequest
	inputs   []runtimeclient.SubmitInputRequest
}

func (c *historyPageClient) ListItems(_ context.Context, request *runtimeclient.ListItemsRequest) (runtimeclient.ListItemsResult, error) {
	c.requests = append(c.requests, *request)
	return runtimeclient.ListItemsResult{}, nil
}

func (c *historyPageClient) OpenConversation(_ context.Context, request *runtimeclient.OpenConversationRequest) (defs.Conversation, error) {
	c.opened <- *request
	return defs.Conversation{ID: request.ConversationID, RootSessionID: "s1"}, nil
}

func (c *historyPageClient) SubmitInput(_ context.Context, request *runtimeclient.SubmitInputRequest) (defs.ItemID, error) {
	c.inputs = append(c.inputs, *request)
	return "queued", nil
}

// TestExternalClientRestore 验证外部库更换时重新读桌面倒序页，以及等待、失败时的聊天视图。
func TestExternalClientRestore(t *testing.T) {
	for _, action := range []string{"cold_open", "ready_same_tail", "restoring_then_ready", "restoring_then_failure", "native_incremental"} {
		t.Run(action, func(t *testing.T) {
			d := testDesktop()
			client := &historyPageClient{}
			d.client = client
			d.readingCtx = t.Context()
			d.window = &mygo.Window{}
			d.conversation = defs.Conversation{ID: "c1", RootSessionID: "s1"}
			d.readingTarget = readingTarget{ConversationID: "c1", SessionID: "s1"}
			if action == "cold_open" {
				d.ctx, d.cancel = context.WithCancel(t.Context())
				d.readingRequests = make(chan readingRequested, 1)
				client.opened = make(chan runtimeclient.OpenConversationRequest, 1)
				d.tasks.Go(d.runReadingRequests)
				d.openConversation("c1", "s1")
				request := <-client.opened
				d.cancel()
				d.tasks.Wait()
				if !request.SkipHistory || len(client.requests) != 0 {
					t.Fatalf("open=%v premature pages=%v", request, client.requests)
				}
				return
			}
			snapshot := runtimeclient.SessionSynced{ConversationID: "c1", SessionID: "s1", External: action != "native_incremental", LastSavedOrdinal: 53}
			if snapshot.External {
				snapshot.HistoryAction = runtimeclient.HistoryReplace
			}
			d.handleClientEvent(&snapshot)
			d.tasks.Wait()
			want := runtimeclient.ListItemsRequest{ConversationID: "c1", SessionID: "s1", Order: storage.DirectionDesc, Limit: 50}
			if !slices.Equal(client.requests, []runtimeclient.ListItemsRequest{want}) {
				t.Fatalf("first ready requests=%v", client.requests)
			}
			client.requests = nil
			oldTarget := d.readingTarget
			d.handleHistoryPage(oldTarget, runtimeclient.ListItemsResult{
				Items:      []runtimeprotocol.LoggedItem{{Ordinal: 53, Item: &defs.Item{ID: "old-tail"}}, {Ordinal: 3, Item: &defs.Item{ID: "outside"}}},
				NextCursor: 3,
			}, nil)
			if strings.HasPrefix(action, "restoring") {
				waiting := runtimeclient.SessionSynced{ConversationID: "c1", SessionID: "s1", External: true, Restoring: true, HistoryAction: runtimeclient.HistoryWait}
				d.handleClientEvent(&waiting)
				d.loadHistory()
				d.tasks.Wait()
				if d.readingReady || d.itemCursor != 3 || len(client.requests) != 0 {
					t.Fatalf("restoring ready=%v cursor=%d requests=%v", d.readingReady, d.itemCursor, client.requests)
				}
				render := func() string {
					tester := ui.NewTester(d.conversationView, 800, 600)
					tester.SetLocale("zh-Hans")
					return strings.Join(tester.Texts(), " ")
				}
				if view := render(); !strings.Contains(view, "正在从外部 Agent 恢复历史") {
					t.Fatalf("restoring view=%s", view)
				}
				// 恢复中的会话仍可接收输入，历史读取独立等待 Ready。
				key := draftKey{d.serverID, d.conversation.ID}
				d.drafts[key] = &conversationDraft{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "during restoration"}}}}
				d.submitDraft(key, session.SessionInputModeNormal)
				d.tasks.Wait()
				if len(client.inputs) != 1 || client.inputs[0].ConversationID != "c1" {
					t.Fatalf("restoring submissions=%v", client.inputs)
				}
				if action == "restoring_then_failure" {
					d.handleClientEvent(&runtimeclient.ConversationSyncFailed{ConversationID: "c1", Err: errors.New("peer refused history")})
					d.loadHistory()
					if view := render(); !strings.Contains(view, "peer refused history") || strings.Contains(view, "正在从外部") || d.readingReady || d.historyLoading || len(client.requests) != 0 {
						t.Fatalf("failed view=%s requests=%v", view, client.requests)
					}
					return
				}
			}
			d.handleClientEvent(&snapshot)
			d.tasks.Wait()
			if !snapshot.External {
				if len(client.requests) != 0 || len(d.items) != 2 || d.itemCursor != 3 {
					t.Fatalf("native requests=%v items=%v", client.requests, d.items)
				}
				return
			}
			if !slices.Equal(client.requests, []runtimeclient.ListItemsRequest{want}) || len(d.items) != 0 || d.itemCursor != 0 {
				t.Fatalf("requests=%v items=%v cursor=%d", client.requests, d.items, d.itemCursor)
			}
			// 窗口主线程先收到新实时条目，再收到当前页和上一代的分页回调。
			d.mergeSavedItems([]runtimeprotocol.LoggedItem{{Ordinal: 54, Item: &defs.Item{ID: "live"}}}, true)
			page := runtimeclient.ListItemsResult{Items: []runtimeprotocol.LoggedItem{{Ordinal: 53, Item: &defs.Item{ID: "new-tail"}}}, NextCursor: 53}
			d.handleHistoryPage(d.readingTarget, page, nil)
			d.handleHistoryPage(oldTarget, runtimeclient.ListItemsResult{}, errors.New("late response"))
			if len(d.items) != 2 || d.items[0].Item.ID != "new-tail" || d.items[1].Item.ID != "live" || d.itemCursor != 53 || d.historyError != nil {
				t.Fatalf("page=%v cursor=%d err=%v", d.items, d.itemCursor, d.historyError)
			}
		})
	}
}

// TestExternalInitialRestoreFailure 验证首次打开失败时保留所选会话和服务端原文，迟到的历史页不能覆盖原因。
func TestExternalInitialRestoreFailure(t *testing.T) {
	for _, method := range []string{"conversation/open", "session/subscribe"} {
		t.Run(method, func(t *testing.T) {
			d := testDesktop()
			d.conversation = defs.Conversation{ID: "c7", RootSessionID: "s7"}
			d.readingTarget = readingTarget{ConversationID: "c7", SessionID: "s7", Request: 1}
			d.historyLoading = true
			cause := "external session cannot be restored: executor archive not found"
			err := fmt.Errorf("%s: %w", method, &runtimeclient.ResponseError{Code: runtimeprotocol.ErrorCodeRuntime, Message: cause})
			d.handleReadingFailed(d.readingTarget, err)
			tester := ui.NewTester(d.conversationView, 1000, 600)
			d.handleHistoryPage(d.readingTarget, runtimeclient.ListItemsResult{}, nil)
			tester.Advance(time.Minute)
			text := strings.Join(tester.Texts(), " ")
			if d.readingTarget.ConversationID != "c7" || d.readingReady || d.historyLoading || !strings.Contains(text, cause) {
				t.Fatalf("target=%+v view=%s", d.readingTarget, text)
			}
		})
	}
}
