package desktop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/cli"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/egoist/mygo/ui"
)

// TestMarkdownNativeRendering 验证原生 Markdown 的代码复制、任务列表与表格。
func TestMarkdownNativeRendering(t *testing.T) {
	// 1. 使用真实字体与渲染器，不检查源码或依赖内部样式。
	if err := registerFonts(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, text, want string }{{"code", "# 中文 Heading\n\n```go\nfmt.Println(\"hello\")\n```", "hello"}, {"tasks", "- [x] 完成\n- [ ] pending", "pending"}, {"table", "|Name|Value|\n|-|-|\n|模型|256K|", "256K"}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tester := ui.NewTester(func(c *ui.Context) {
				d.applyAppearance(c)
				ui.Column(c).Fill().Padding(16).Selectable().Children(func() { d.markdownView(c, tt.text, false) })
			}, 640, 480)
			if !tester.HasText(tt.want) {
				t.Fatalf("missing %q: %q", tt.want, tester.Texts())
			}
			if tt.name == "code" {
				if err := tester.Click("Copy code"); err != nil {
					t.Fatal(err)
				}
				if tester.Clipboard() != "fmt.Println(\"hello\")\n" {
					t.Fatalf("clipboard=%q", tester.Clipboard())
				}
			}
			if dir := os.Getenv("OPENVAALA_DESKTOP_TEST_CAPTURE"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				file, err := os.Create(filepath.Join(dir, tt.name+".png"))
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

// TestLoadImageHeaders 检查网络图片下载带上 http.headers，并允许跳到别的域名。
func TestLoadImageHeaders(t *testing.T) {
	// 1. 图片服务记录收到的 header；跳转用例让第一个服务重定向到另一个端口的服务。
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		redirect bool
	}{{"direct", false}, {"cross_origin_redirect", true}} {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan http.Header, 1)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Clone()
				w.Write(picture.Bytes())
			}))
			defer target.Close()
			url := target.URL
			if tt.redirect {
				front := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
				defer front.Close()
				url = front.URL
			}
			// 2. 经 NewDesktop 建立下载客户端，再加载图片。
			d, err := NewDesktop(&DesktopOpt{RuntimeServerStart: cli.RuntimeServerStartOpt{Executable: "openvaala"}, HTTPHeaders: map[string]string{"X-Team": "infra"}})
			if err != nil {
				t.Fatal(err)
			}
			d.ctx = t.Context()
			d.images = map[string]*imageResource{url: {}}
			d.loadImage(url)
			d.tasks.Wait()
			select {
			case header := <-received:
				if header.Get("X-Team") != "infra" {
					t.Fatalf("headers = %v", header)
				}
			default:
				t.Fatal("image server was not reached")
			}
		})
	}
}

// TestItemSynchronization 检查中断片段及重连快照通过可见正文更新。
func TestItemSynchronization(t *testing.T) {
	// 1. 保存条目始终保留，已撤回的生成片段不再展示。
	for _, kind := range []string{"interrupted", "resynced"} {
		t.Run(kind, func(t *testing.T) {
			d := testDesktop()
			d.readingTarget = readingTarget{ServerID: "fixture", ConversationID: "conversation", SessionID: "session"}
			saved := &defs.Item{ID: "saved", Kind: defs.ItemMessage, TurnID: "turn", Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "saved message"}}}}}
			draft := saved.Clone()
			draft.ID = "draft"
			draft.Message.Content[0].Text.Text = "removed fragment"
			d.mergeSavedItems([]runtimeprotocol.LoggedItem{{Ordinal: 1, Item: saved}}, false)
			d.mergeStreamingItem(draft, 0, false)
			if kind == "interrupted" {
				d.handleClientEvent(&runtimeclient.ItemChanged{ConversationID: "conversation", SessionID: "session", TurnID: "turn", ItemIndex: 0, Type: agent.AgentEventItemInterrupted})
			} else {
				d.handleClientEvent(&runtimeclient.SessionSynced{ConversationID: "conversation", SessionID: "session"})
			}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 480)
			if tester.HasText("removed fragment") || !tester.HasText("saved message") {
				t.Fatalf("texts=%q", tester.Texts())
			}
		})
	}
}

// TestToolDisclosure 检查过程和工具分别展开，调试结果按 CallID 关联。
func TestToolDisclosure(t *testing.T) {
	// 1. 通过原生点击读取完整工具内容，不以调用方顺序猜配对。
	for _, name := range []string{"bash", "read_file", "apply_patch", "spawn_agent", "send_message", "wait_agent", "interrupt_agent", "list_agents", "read_session_history", "read_command_output", "write_stdin", "stop_command", "list_commands"} {
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			args := `{"command":"echo native","path":"README.md","patch":"*** Begin Patch\n*** Add File: a\n+line\n*** End Patch","message":"task message"}`
			result := &defs.ToolResult{CallID: "call", Content: json.RawMessage(`{"output":"native output","content":"1: file content","status":"exited","exit_code":0}`)}
			view := viewItem{Item: &defs.Item{ID: "call-item", TurnID: "turn", Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: "call", Name: name, Arguments: args, Status: defs.ToolCallReady}}}
			d.itemExpanded["tool-call"] = true
			tester := ui.NewTester(func(c *ui.Context) {
				d.applyAppearance(c)
				ui.Column(c).Fill().Children(func() { d.toolView(c, view.Item.ToolCall, result, view) })
			}, 640, 800)
			tester.Advance(300 * time.Millisecond)
			if !tester.HasText(name) {
				t.Fatalf("missing tool: %q", tester.Texts())
			}
		})
	}
}

// TestFinalReplyPresentation 检查历史完整后折叠中途回复，复制只包含真正最终正文。
func TestFinalReplyPresentation(t *testing.T) {
	// 1. 同一 Turn 两次模型回复，缺页时不能冒充最终结果。
	for _, complete := range []bool{false, true} {
		name := "partial"
		if complete {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			first := &defs.Item{ID: "first", TurnID: "turn", ModelCallID: "intermediate", Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "intermediate text"}}}}}
			last := first.Clone()
			last.ID = "final"
			last.ModelCallID = "final-call"
			last.Message.Content[0].Text.Text = "final text"
			d.items = []viewItem{{Item: first, Ordinal: 1}, {Item: last, Ordinal: 2}}
			lastOrdinal := uint64(3)
			if complete {
				lastOrdinal = 2
			}
			d.turns["turn"] = defs.Turn{ID: "turn", Status: defs.TurnStatusCompleted, FinalModelCallID: "final-call", ItemsRange: defs.TurnItemRange{FirstOrdinal: 1, LastOrdinal: lastOrdinal}}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 480)
			if tester.HasText("复制结果") != complete {
				t.Fatalf("copy eligibility: %q", tester.Texts())
			}
			if complete {
				if tester.HasText("intermediate text") {
					t.Fatal("intermediate reply expanded")
				}
				if err := tester.Click("复制结果"); err != nil {
					t.Fatal(err)
				}
				if tester.Clipboard() != "final text" {
					t.Fatalf("copy=%q", tester.Clipboard())
				}
				if err := tester.Click("中途回复"); err != nil {
					t.Fatal(err)
				}
				tester.Advance(250 * time.Millisecond)
				if !tester.HasText("intermediate text") {
					t.Fatal("intermediate reply unavailable")
				}
			}
		})
	}
}

// TestForkSourceNavigation 检查分叉和子 Agent 来源通过真实原生控件进入正确目标。
func TestForkSourceNavigation(t *testing.T) {
	// 1. 用户分叉进入来源对话；子 Agent 分界进入原父 Session。
	for _, userFork := range []bool{false, true} {
		name := "agent fork"
		if userFork {
			name = "conversation fork"
		}
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			source := defs.Conversation{ID: "source-conversation", RootSessionID: "source-session", Title: "source title"}
			d.conversations = []dao.ConversationListEntry{{Conversation: source}}
			d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: source.RootSessionID, ConversationID: source.ID, TaskName: "parent task"}}}
			fork := &defs.SessionFork{SourceSessionID: source.RootSessionID}
			label := "Session source-session"
			if userFork {
				fork.SourceConversationID = source.ID
				fork.SourceEndOrdinal = 2
				label = "来源对话 · source-conversation"
			}
			view := viewItem{Item: &defs.Item{ID: "fork", Kind: defs.ItemSessionFork, SessionFork: fork}}
			tester := ui.NewTester(func(c *ui.Context) {
				d.applyAppearance(c)
				ui.Column(c).Fill().Children(func() { d.itemView(c, view, nil, nil, nil) })
			}, 640, 480)
			if err := tester.Click(label); err != nil {
				t.Fatal(err)
			}
			if d.readingTarget.ConversationID != source.ID || d.readingTarget.SessionID != source.RootSessionID {
				t.Fatalf("navigation=%+v", d.readingTarget)
			}
			if d.conversation.Title != "source title" {
				t.Fatalf("wrong conversation after navigation: %+v", d.conversation)
			}
		})
	}
}

// TestHistoryPrependReading 检查变高消息顶部追加历史后可见消息的像素位置不跳。
func TestHistoryPrependReading(t *testing.T) {
	// 1. 显示中间消息，再按真实 ordinal 合入更早一页。
	for _, multiline := range []bool{false, true} {
		name := "single line"
		if multiline {
			name = "variable height"
		}
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			makeItem := func(index int) runtimeprotocol.LoggedItem {
				text := fmt.Sprintf("history-row-%d", index)
				if multiline && index%3 == 0 {
					text += "\n\nsecond paragraph\n\nthird paragraph"
				}
				return runtimeprotocol.LoggedItem{Ordinal: storage.Ordinal(index), Item: &defs.Item{ID: defs.ItemID(fmt.Sprint(index)), Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}}}
			}
			page := []runtimeprotocol.LoggedItem{}
			for index := 11; index <= 70; index++ {
				page = append(page, makeItem(index))
			}
			d.mergeSavedItems(page, false)
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 700)
			d.messageList.ScrollTo(20, ui.Start)
			tester.Frame()
			before, ok := tester.Find("history-row-31")
			if !ok {
				t.Fatal("anchor missing before prepend")
			}
			older := []runtimeprotocol.LoggedItem{}
			for index := 1; index <= 10; index++ {
				older = append(older, makeItem(index))
			}
			d.mergeSavedItems(older, false)
			tester.Frame()
			after, ok := tester.Find("history-row-31")
			if !ok || math.Abs(float64(before.Y-after.Y)) > 1 {
				t.Fatalf("anchor moved: before=%+v after=%+v present=%v", before, after, ok)
			}
		})
	}
}

// TestRealtimeItemFade 检查真实保存事件只在首次出现淡入，历史不播放。
func TestRealtimeItemFade(t *testing.T) {
	// 1. 对比原生实际像素，不仅检查是否存有计时字段。
	for _, tt := range []struct {
		name           string
		live           bool
		osReduceMotion bool
	}{
		{"live", true, false},
		{"history", false, false},
		{"live with OS motion preference", true, true},
		{"history with OS motion preference", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.readingTarget = readingTarget{ConversationID: "c", SessionID: "s"}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 400)
			tester.SetPreferences(ui.Preferences{ReduceMotion: tt.osReduceMotion})
			item := runtimeprotocol.LoggedItem{Ordinal: 1, Item: &defs.Item{ID: "live", Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "new native message"}}}}}}
			if tt.live {
				d.handleClientEvent(&runtimeclient.ItemsSaved{ConversationID: "c", SessionID: "s", Items: []runtimeprotocol.LoggedItem{item}})
			} else {
				d.handleClientEvent(&runtimeclient.SessionSynced{ConversationID: "c", SessionID: "s", Items: []runtimeprotocol.LoggedItem{item}})
			}
			tester.Frame()
			before := append([]byte(nil), tester.Image().Pix...)
			tester.Advance(200 * time.Millisecond)
			after := tester.Image().Pix
			if bytes.Equal(before, after) == tt.live {
				t.Fatalf("unexpected animation live=%v", tt.live)
			}
			stable := append([]byte(nil), after...)
			tester.Advance(time.Second)
			if !bytes.Equal(stable, tester.Image().Pix) {
				t.Fatal("item animation replayed")
			}
		})
	}
}

// TestJumpToLatest 检查回底动作到达最新消息并恢复追随。
func TestJumpToLatest(t *testing.T) {
	// 1. 原生滚动离开底部，再点击产品入口。
	for _, row := range []int{20, 40} {
		t.Run(fmt.Sprint(row), func(t *testing.T) {
			d := testDesktop()
			for index := range 80 {
				d.items = append(d.items, viewItem{Ordinal: storage.Ordinal(index + 1), Item: &defs.Item{ID: defs.ItemID(fmt.Sprint(index)), Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: fmt.Sprintf("reading row %d", index)}}}}}})
			}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 480)
			tester.SetLocale("zh-Hans")
			d.messageList.ScrollTo(row, ui.Start)
			tester.Frame()
			if err := tester.Click("回到底部"); err != nil {
				t.Fatal(err)
			}
			tester.Advance(200 * time.Millisecond)
			if !d.messageList.AtEnd() || !tester.HasText("reading row 79") {
				t.Fatal("jump did not reach newest row")
			}
		})
	}
}

// TestMessageSharedSelection 检查跨 Markdown 段及跨消息的真实拖选保留全部正文。
func TestMessageSharedSelection(t *testing.T) {
	// 1. 用鼠标坐标拖过两块正文，再执行原生复制快捷键。
	for _, separate := range []bool{false, true} {
		name := "paragraphs"
		if separate {
			name = "messages"
		}
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			texts := []string{"first selection paragraph\n\nlast selection paragraph"}
			if separate {
				texts = []string{"first selection paragraph", "last selection paragraph"}
			}
			for index, text := range texts {
				d.items = append(d.items, viewItem{Ordinal: storage.Ordinal(index + 1), Item: &defs.Item{ID: defs.ItemID(fmt.Sprint(index)), Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}}})
			}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 700, 600)
			first, ok := tester.Find("first selection paragraph")
			if !ok {
				t.Fatal("first paragraph missing")
			}
			last, ok := tester.Find("last selection paragraph")
			if !ok {
				t.Fatal("last paragraph missing")
			}
			tester.Press(first.X+1, first.Y+first.H/2)
			tester.Move(last.X+last.W-1, last.Y+last.H/2)
			tester.Release(last.X+last.W-1, last.Y+last.H/2)
			tester.Command("copy")
			copied := tester.Clipboard()
			if !strings.Contains(copied, "first selection paragraph") || !strings.Contains(copied, "last selection paragraph") {
				t.Fatalf("incomplete selection copy %q", copied)
			}
		})
	}
}

// TestSessionLinkDuringListBuild 检查消息内来源链接切换目标时原列表仍能安全完成后续行构建。
func TestSessionLinkDuringListBuild(t *testing.T) {
	// 1. 来源链接在虚拟列表内，后面还保留实际可见消息；不缩减为独立单控件测试。
	for _, sourceKind := range []string{"session fork", "agent tool"} {
		t.Run(sourceKind, func(t *testing.T) {
			d := testDesktop()
			d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "task"}
			seed := func() {
				d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "root", ConversationID: "conversation"}}, {Session: defs.Session{ID: "task", ConversationID: "conversation", ParentSessionID: "root"}}}
				source := &defs.Item{ID: "source", Kind: defs.ItemSessionFork, SessionFork: &defs.SessionFork{SourceSessionID: "root"}}
				if sourceKind == "agent tool" {
					source = &defs.Item{ID: "source", Kind: defs.ItemToolCall, ToolCall: &defs.ToolCall{CallID: "spawn", Name: "send_message", Arguments: `{"session_id":"root","message":"navigate"}`, Status: defs.ToolCallReady}}
					d.itemExpanded["tool-spawn"] = true
				}
				d.items = []viewItem{{Ordinal: 1, Item: source}}
				for index := range 3 {
					d.items = append(d.items, viewItem{Ordinal: storage.Ordinal(index + 2), Item: &defs.Item{ID: defs.ItemID(fmt.Sprintf("following-%d", index)), Kind: defs.ItemMessage, Message: &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: fmt.Sprintf("following row %d", index)}}}}}})
				}
			}
			seed()
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 700, 1600)
			for range 3 {
				if err := tester.Click("Session root"); err != nil {
					t.Fatal(err)
				}
				if d.readingTarget.SessionID != "root" {
					t.Fatalf("target=%+v", d.readingTarget)
				}
				d.openConversation("conversation", "task")
				seed()
				tester.Frame()
			}
		})
	}
}

// TestErrorCallouts 检查模型错误按 Turn 合并成一组并带重新发送操作，中断只给警告条目。
func TestErrorCallouts(t *testing.T) {
	modelError := func(id, message string) viewItem {
		return viewItem{Item: &defs.Item{ID: defs.ItemID(id), TurnID: "turn", Kind: defs.ItemModelError, ModelError: &defs.ModelError{Code: "rate_limit", Message: message}}}
	}
	for _, tt := range []struct {
		name    string
		items   []viewItem
		want    []string
		details []string
		rows    int
		resend  bool
	}{
		{"model error", []viewItem{modelError("error", "upstream timeout")}, []string{"模型调用失败"}, []string{"rate_limit: upstream timeout"}, 1, true},
		{"adjacent errors merge", []viewItem{modelError("first", "first failure"), modelError("second", "second failure")}, []string{"模型调用失败", "×2"}, []string{"first failure", "second failure"}, 1, true},
		{"interrupted", []viewItem{{Item: &defs.Item{ID: "stop", TurnID: "turn", Kind: defs.ItemTurnInterrupted}}}, []string{"这一轮已中断"}, nil, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			for index := range tt.items {
				tt.items[index].Ordinal = storage.Ordinal(index + 1)
			}
			d.items = tt.items
			if rows, _ := buildMessageRows(d.items); len(rows) != tt.rows {
				t.Fatalf("rows=%d want %d", len(rows), tt.rows)
			}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 480)
			for _, want := range tt.want {
				if !tester.HasText(want) {
					t.Fatalf("missing %q: %q", want, tester.Texts())
				}
			}
			for _, detail := range tt.details {
				if tester.HasText(detail) {
					t.Fatalf("detail %q visible while collapsed", detail)
				}
			}
			if _, found := tester.Find("重新发送"); found != tt.resend {
				t.Fatalf("resend action mismatch: %q", tester.Texts())
			}
			if len(tt.details) == 0 {
				return
			}
			if err := tester.Click("模型调用失败"); err != nil {
				t.Fatal(err)
			}
			tester.Advance(300 * time.Millisecond)
			for _, detail := range tt.details {
				if !tester.HasText(detail) {
					t.Fatalf("missing detail %q: %q", detail, tester.Texts())
				}
			}
		})
	}
}

// TestItemActivityTitles 检查各类条目的标题行和用户卡片「从这里重来」的出现条件。
func TestItemActivityTitles(t *testing.T) {
	text := func(value string) []*defs.Content {
		return []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: value}}}
	}
	user := func(ordinal storage.Ordinal) viewItem {
		return viewItem{Ordinal: ordinal, Item: &defs.Item{ID: "user", TurnID: "turn", Kind: defs.ItemMessage, Message: &defs.Message{From: &defs.Address{Kind: defs.IdentityUser}, Content: text("user text")}}}
	}
	for _, tt := range []struct {
		name    string
		item    viewItem
		session defs.SessionID
		want    []string
		fork    bool
	}{
		{"thinking", viewItem{Streaming: true, Item: &defs.Item{ID: "r", Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{}}}, "root", []string{"思考中"}, false},
		{"thought", viewItem{Item: &defs.Item{ID: "r", Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{Status: defs.ReasoningCompleted}}}, "root", []string{"思考完成"}, false},
		{"interrupted thought", viewItem{Item: &defs.Item{ID: "r", Kind: defs.ItemReasoning, Reasoning: &defs.Reasoning{Status: defs.ReasoningInterrupted}}}, "root", []string{"思考已中断"}, false},
		{"summary", viewItem{Item: &defs.Item{ID: "s", Kind: defs.ItemContextSummary, ContextSummary: &defs.ContextSummary{Text: "summary"}}}, "root", []string{"上下文已压缩"}, false},
		{"skill", viewItem{Item: &defs.Item{ID: "k", Kind: defs.ItemSkillInstruction, SkillInstruction: &defs.SkillInstruction{Name: "review"}}}, "root", []string{"加载 Skill"}, false},
		{"agents", viewItem{Item: &defs.Item{ID: "a", Kind: defs.ItemAgentsInstruction, AgentsInstruction: &defs.AgentsInstruction{Path: "AGENTS.md"}}}, "root", []string{"加载目录规则"}, false},
		{"client instruction", viewItem{Item: &defs.Item{ID: "c", Kind: defs.ItemClientInstruction, ClientInstruction: &defs.ClientInstruction{Text: "boundary"}}}, "root", []string{"客户端指令"}, false},
		{"hook", viewItem{Item: &defs.Item{ID: "h", Kind: defs.ItemHookRun, HookRun: &defs.HookRun{ID: "run", Status: defs.HookRunBlocked, Event: "pre_tool_use", Handler: "guard.sh"}}}, "root", []string{"Hook 事件", "Blocked"}, false},
		{"model change", viewItem{Item: &defs.Item{ID: "m", Kind: defs.ItemModelChange, ModelChange: &defs.ModelChange{PreviousModel: defs.ModelRef{ProviderID: "p", ModelID: "a"}, Model: defs.ModelRef{ProviderID: "p", ModelID: "b"}}}}, "root", []string{"模型从"}, false},
		{"unknown", viewItem{Item: &defs.Item{ID: "u", Kind: "mystery"}}, "root", []string{"未知类型的记录：mystery"}, false},
		{"saved root user message", user(1), "root", []string{"user text"}, true},
		{"unsaved user message", user(0), "root", []string{"user text"}, false},
		{"sub session user message", user(1), "task", []string{"user text"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: tt.session}
			d.items = []viewItem{tt.item}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 480)
			for _, want := range tt.want {
				if !tester.HasText(want) {
					t.Fatalf("missing %q: %q", want, tester.Texts())
				}
			}
			if _, found := tester.Find("从这里分叉"); found != tt.fork {
				t.Fatalf("fork action=%v texts=%q", found, tester.Texts())
			}
		})
	}
}

// TestMemoryIndexItem 检查记忆目录条目：标题行带路径，展开后有「路径@memory」和正文，截断过的多一行提示，不显示成未知类型的记录。
func TestMemoryIndexItem(t *testing.T) {
	for _, tt := range []struct {
		name  string
		index defs.MemoryIndex
		// wantText 是正文按 Markdown 渲染之后的列表项文字。
		wantText string
	}{
		{"complete", defs.MemoryIndex{Path: "user/MEMORY.md", Text: "- 回复用中文"}, "回复用中文"},
		{"truncated", defs.MemoryIndex{Path: "project/MEMORY.md", Text: "- 测试用 table-driven", IsTruncated: true}, "测试用 table-driven"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.items = []viewItem{{Ordinal: 1, Item: &defs.Item{ID: "memory", TurnID: "turn", Kind: defs.ItemMemoryIndex, MemoryIndex: &tt.index}}}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, 640, 480)
			// 折叠时只有标题和路径，正文要点开才有。
			for _, want := range []string{"带上了记忆目录", tt.index.Path} {
				if !tester.HasText(want) {
					t.Fatalf("missing %q: %q", want, tester.Texts())
				}
			}
			if tester.HasText(tt.wantText) {
				t.Fatalf("body visible while collapsed: %q", tester.Texts())
			}
			if err := tester.Click("带上了记忆目录"); err != nil {
				t.Fatal(err)
			}
			tester.Advance(300 * time.Millisecond)
			for _, want := range []string{tt.index.Path + "@memory", tt.wantText} {
				if !tester.HasText(want) {
					t.Fatalf("missing %q after expanding: %q", want, tester.Texts())
				}
			}
			if got := tester.HasText("超过 8 KiB，已截断"); got != tt.index.IsTruncated {
				t.Fatalf("truncated note shown = %v, want %v: %q", got, tt.index.IsTruncated, tester.Texts())
			}
			if slices.ContainsFunc(tester.Texts(), func(text string) bool { return strings.Contains(text, "未知类型") }) {
				t.Fatalf("memory index shown as an unknown record: %q", tester.Texts())
			}
		})
	}
}

// TestConversationColumn 检查消息列最宽 800 并在中间区居中，窄窗口铺满并保留 16 左右留白。
func TestConversationColumn(t *testing.T) {
	// 1. 同一条消息在不同窗口宽度下，正文左缘随中间区宽度变化。
	for _, tt := range []struct {
		name  string
		width int
		left  float32
	}{
		{"wide window centers", 1600, 0},
		{"narrow window pads", 700, 16},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
			d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
			message := &defs.Message{Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "column body"}}}}
			d.items = []viewItem{{Ordinal: 1, Item: &defs.Item{ID: "reply", Kind: defs.ItemMessage, Message: message}}}
			tester := ui.NewTester(func(c *ui.Context) { d.applyAppearance(c); ui.Column(c).Fill().Children(func() { d.itemsView(c) }) }, tt.width, 400)
			rect, ok := tester.Find("column body")
			if !ok {
				t.Fatalf("missing body: %q", tester.Texts())
			}
			want := tt.left
			if want == 0 {
				want = (float32(tt.width) - conversationColumnWidth) / 2
			}
			if math.Abs(float64(rect.X-want)) > 1 {
				t.Fatalf("body x=%v want %v", rect.X, want)
			}
		})
	}
}
