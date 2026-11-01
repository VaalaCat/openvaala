package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"slices"
	"testing"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtime"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/egoist/mygo/ui"
)

type unavailableReadingClient struct{ runtimeclient.Client }

// TestUnsyncedComposerSubmit 检查键盘发送与按钮同样等待当前目标完成同步。
func TestUnsyncedComposerSubmit(t *testing.T) {
	// 1. 尚未同步时真实键盘和按钮均不进入 RPC。
	for _, keyboard := range []bool{false, true} {
		name := "button"
		if keyboard {
			name = "keyboard"
		}
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			d.client = unavailableReadingClient{}
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
			d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
			tester := ui.NewTester(d.view, 1280, 820)
			if err := tester.Click("消息输入"); err != nil {
				t.Fatal(err)
			}
			tester.Type("preserved unsynced draft")
			if keyboard {
				tester.Key(0, ui.KeyEnter)
			} else {
				_ = tester.Click("发送")
			}
			draft := d.drafts[draftKey{"fixture", "conversation"}]
			if draft.Pending || draftText(draft.Content) != "preserved unsynced draft" {
				t.Fatal("unsynced composer submitted or lost draft")
			}
		})
	}
}

// TestComposerInput 验证 IME 组字与换行由原生输入控件处理。
func TestComposerInput(t *testing.T) {
	// 1. 原生组字不触发发送，Shift+Enter 保留换行。
	for _, name := range []string{"composition", "newline"} {
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			tester := ui.NewTester(d.view, 1280, 820)
			if err := tester.Click("消息输入"); err != nil {
				t.Fatal(err)
			}
			if name == "composition" {
				tester.Compose("中文", 2)
				tester.Key(0, ui.KeyEnter)
			} else {
				tester.Type("line")
				tester.Key(ui.Shift, ui.KeyEnter)
				tester.Type("two")
			}
			draft := d.drafts[draftKey{"fixture", ""}]
			if draft.Pending {
				t.Fatal("IME/newline submitted")
			}
			if name == "newline" && draftText(draft.Content) != "line\ntwo" {
				t.Fatalf("draft=%q", draftText(draft.Content))
			}
		})
	}
}

// TestDraftCompletion 检查迟到发送只清理原内容，保留图片和新文字。
func TestDraftCompletion(t *testing.T) {
	// 1. 发送材料用真实指针身份，不将图片降成纯文本。
	image := &defs.Content{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "first"}}
	laterImage := &defs.Content{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: "second"}}
	sent := []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: "sent"}}, image}
	for _, tt := range []struct{ name, text, want string }{{"append", "sent next", "next"}, {"rewrite", "changed", "changed"}, {"same", "sent", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			content := []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: tt.text}}, image, laterImage}
			got := removeSentContent(content, sent)
			if draftText(got) != tt.want {
				t.Fatalf("text=%q", draftText(got))
			}
			count := 0
			for _, part := range got {
				if part.Image != nil {
					count++
					if part != laterImage {
						t.Fatal("wrong image retained")
					}
				}
			}
			if count != 1 {
				t.Fatalf("images=%d", count)
			}
		})
	}
}

// TestMigratedDraftImage 检查异步图片返回仍进入创建前发起的同一草稿对象。
func TestMigratedDraftImage(t *testing.T) {
	// 1. 对话创建只迁移草稿键，图片任务握有原对象。
	for _, name := range []string{"text draft", "image only"} {
		t.Run(name, func(t *testing.T) {
			d := testDesktop()
			draft := &conversationDraft{Pending: true}
			if name == "text draft" {
				draft.Content = replaceDraftText(nil, "keep")
			}
			d.drafts[draftKey{"fixture", ""}] = draft
			d.drafts[draftKey{"fixture", "created"}] = draft
			delete(d.drafts, draftKey{"fixture", ""})
			var data bytes.Buffer
			if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			attachments, err := prepareImageAttachments(d.ctx, [][]byte{data.Bytes()}, []string{"image.png"})
			d.acceptImageAttachments(draft, attachments, err)
			if d.drafts[draftKey{"fixture", ""}] != nil {
				t.Fatal("old draft key recreated")
			}
			if !draft.Pending || !isContentPresent(draft.Content) {
				t.Fatal("draft lifecycle or image lost")
			}
			if draft.Content[len(draft.Content)-1].Image == nil {
				t.Fatal("image lost")
			}
		})
	}
}

// TestSkillCompletionAtCaret 检查光标位于正文中间时的键盘补全保留后缀。
func TestSkillCompletionAtCaret(t *testing.T) {
	// 1. 使用原生输入、移动光标及 Tab，不直接调用补全 handler。
	for _, tt := range []struct {
		name, text, want string
		caret            int
	}{{"middle", "before /re after", "before /read  after", 10}, {"Chinese", "中文 /re 后文", "中文 /read  后文", 6}} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.skillCatalogs[getSkillCatalogKey("", d.newAgent, d.newExecutor, d.newDirectory)] = &skillCatalog{Skills: []skill.SkillInfo{{Name: "read"}}}
			tester := ui.NewTester(d.view, 1280, 820)
			if err := tester.Click("消息输入"); err != nil {
				t.Fatal(err)
			}
			tester.Type(tt.text)
			tester.Key(0, ui.KeyHome)
			for range tt.caret {
				tester.Key(0, ui.KeyRight)
			}
			tester.Key(0, ui.KeyTab)
			got := draftText(d.drafts[draftKey{"fixture", ""}].Content)
			if got != tt.want {
				t.Fatalf("draft=%q want=%q", got, tt.want)
			}
		})
	}
}

// TestSlashCompletionMenu 检查输入 / 时技能补全浮层的内容和状态，且打开浮层不改变输入区布局。
func TestSlashCompletionMenu(t *testing.T) {
	// 1. 每个用例预置一个来源的技能目录状态，再在输入框里输入文字。
	skills := []skill.SkillInfo{{Name: "pdf-reader", Description: "读取 PDF 文件"}, {Name: "release-notes", Description: "整理发布说明"}}
	for _, tt := range []struct {
		name    string
		typed   string
		catalog *skillCatalog
		want    []string
		missing []string
	}{
		{"names and descriptions", "/", &skillCatalog{Skills: skills}, []string{"/pdf-reader", "读取 PDF 文件", "/release-notes", "整理发布说明"}, nil},
		{"prefix filters", "/pd", &skillCatalog{Skills: skills}, []string{"/pdf-reader"}, []string{"/release-notes"}},
		{"no match", "/zzz", &skillCatalog{Skills: skills}, []string{"没有匹配的 Skill"}, []string{"/pdf-reader"}},
		{"loading", "/", &skillCatalog{Loading: true}, []string{"加载中…"}, []string{"没有匹配的 Skill"}},
		{"error", "/", &skillCatalog{Err: runtimeclient.ErrConnectionLost}, []string{"与本机服务的连接已断开"}, []string{"没有匹配的 Skill"}},
		{"path is not a trigger", "a/b", &skillCatalog{Skills: skills}, nil, []string{"/pdf-reader", "没有匹配的 Skill"}},
		{"word after space", "查看 /re", &skillCatalog{Skills: skills}, []string{"/release-notes"}, []string{"/pdf-reader"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			tester := ui.NewTester(d.view, 1280, 820)
			tester.SetLocale("zh-Hans")
			before, found := tester.Find("发送")
			if !found {
				t.Fatalf("missing send button: %q", tester.Texts())
			}
			if err := tester.Click("消息输入"); err != nil {
				t.Fatal(err)
			}
			// 失败的目录不缓存，浮层关闭时会被清掉，所以在输入框获得焦点之后再放入。
			d.skillCatalogs[getSkillCatalogKey("", d.newAgent, d.newExecutor, d.newDirectory)] = tt.catalog
			tester.Type(tt.typed)
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
			// 2. 浮层不占布局：发送按钮的位置和打开之前相同。
			if after, _ := tester.Find("发送"); after != before {
				t.Fatalf("send button moved from %+v to %+v", before, after)
			}
		})
	}
}

// TestComposerRecallPreviousMessage 检查输入框为空时按 Up 取回自己发的上一条消息，再按 Up 取更早的一条。
func TestComposerRecallPreviousMessage(t *testing.T) {
	user := func(text string) viewItem {
		return viewItem{Ordinal: storage.Ordinal(len(text)), Item: &defs.Item{ID: defs.ItemID(text), Kind: defs.ItemMessage, Message: &defs.Message{From: &defs.Address{Kind: defs.IdentityUser}, Content: []*defs.Content{{Kind: defs.ContentText, Text: &defs.TextContent{Text: text}}}}}}
	}
	for _, tt := range []struct {
		name  string
		typed string
		ups   int
		want  string
	}{
		{"one up", "", 1, "second"},
		{"two ups", "", 2, "first"},
		{"stops at oldest", "", 3, "first"},
		{"typed text is kept", "draft", 1, "draft"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
			d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
			d.readingReady = true
			d.items = []viewItem{user("first"), user("second")}
			tester := ui.NewTester(d.view, 1280, 820)
			if err := tester.Click("消息输入"); err != nil {
				t.Fatal(err)
			}
			tester.Type(tt.typed)
			for range tt.ups {
				tester.Key(0, ui.KeyUp)
			}
			if got := draftText(d.drafts[draftKey{"fixture", "conversation"}].Content); got != tt.want {
				t.Fatalf("draft=%q want %q", got, tt.want)
			}
		})
	}
}

// TestShowImageFailure 检查添加图片失败时 toast 的标题按原因本地化，详情保留文件名和原文。
func TestShowImageFailure(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"not an image", fmt.Errorf("%w: /tmp/a.txt", errNotImage), "只能添加图片"},
		{"too large", fmt.Errorf("%w: /tmp/big.png", errImageTooLarge), "图片超过 10 MiB"},
		{"mixed keeps the most specific", errors.Join(errors.New("decode failed"), fmt.Errorf("%w: /tmp/a.txt", errNotImage)), "只能添加图片"},
		{"decode failure", errors.New("png: invalid format"), "无法读取图片"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.showImageFailure(tt.err)
			if len(d.pendingToasts) != 1 || d.pendingToasts[0].Title != tt.want || d.pendingToasts[0].Description != tt.err.Error() {
				t.Fatalf("toasts=%+v want title %q", d.pendingToasts, tt.want)
			}
		})
	}
}

// TestComposerToolbarOrder 检查输入区工具栏左侧是添加图片，右侧依次为上下文圆环、模型、推理强度、工作位置、压缩和发送；
// 模型没有推理档位时不显示推理强度，输入区窄于 480 时推理强度和工作位置收进「更多」。
func TestComposerToolbarOrder(t *testing.T) {
	// 1. 用真实对话绘制输入区，按可访问名称比较控件的水平位置。
	reasoningModels := runtime.ModelList{Models: []*defs.ModelDefinition{{Ref: defs.ModelRef{ProviderID: "p", ModelID: "m"}, ReasoningEfforts: []string{"low", "high"}}}}
	plainModels := runtime.ModelList{Models: []*defs.ModelDefinition{{Ref: defs.ModelRef{ProviderID: "p", ModelID: "m"}}}}
	for _, tt := range []struct {
		name    string
		models  runtime.ModelList
		width   int
		want    []string
		missing []string
	}{
		{"reasoning", reasoningModels, 1600, []string{"添加图片", "上下文用量", "模型", "推理强度", "工作位置", "压缩上下文", "发送"}, []string{"更多"}},
		{"no reasoning levels", plainModels, 1600, []string{"添加图片", "上下文用量", "模型", "工作位置", "压缩上下文", "发送"}, []string{"推理强度", "更多"}},
		{"narrow folds into more", reasoningModels, 440, []string{"添加图片", "上下文用量", "模型", "更多", "压缩上下文", "发送"}, []string{"推理强度"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := testDesktop()
			d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
			d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
			d.readingReady = true
			d.models = tt.models
			d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "root", ConversationID: "conversation", AgentName: "default", Model: defs.ModelSelection{Ref: defs.ModelRef{ProviderID: "p", ModelID: "m"}}}}}
			tester := ui.NewTester(d.view, tt.width, 820)
			tester.SetLocale("zh-Hans")
			texts := tester.Texts()
			next := slices.Index(texts, "消息输入")
			if next < 0 {
				t.Fatalf("missing composer: %q", texts)
			}
			for _, name := range tt.want {
				found := slices.Index(texts[next:], name)
				if found < 0 {
					t.Fatalf("%q is missing or out of order after index %d in %q", name, next, texts)
				}
				next += found + 1
			}
			for _, name := range tt.missing {
				if slices.Contains(texts, name) {
					t.Fatalf("%q must not be shown in %q", name, texts)
				}
			}
		})
	}
}

// TestReasoningPicker 检查推理强度下拉：触发器显示「推理：自动」，打开后选项只写档位（自动、低、高）。
func TestReasoningPicker(t *testing.T) {
	d := testDesktop()
	d.readingTarget = readingTarget{ConversationID: "conversation", SessionID: "root"}
	d.conversation = defs.Conversation{ID: "conversation", RootSessionID: "root"}
	d.readingReady = true
	d.models = runtime.ModelList{Models: []*defs.ModelDefinition{{Ref: defs.ModelRef{ProviderID: "p", ModelID: "m"}, ReasoningEfforts: []string{"low", "high"}}}}
	d.sessionInfos = []runtime.SessionRuntimeInfo{{Session: defs.Session{ID: "root", ConversationID: "conversation", AgentName: "default", Model: defs.ModelSelection{Ref: defs.ModelRef{ProviderID: "p", ModelID: "m"}}}}}
	tester := ui.NewTester(d.view, 1600, 820)
	tester.SetLocale("zh-Hans")
	if !tester.HasText("推理：自动") {
		t.Fatalf("trigger label missing: %q", tester.Texts())
	}
	if err := tester.Click("推理：自动"); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"自动", "低", "高"} {
		if !slices.Contains(tester.Texts(), option) {
			t.Fatalf("option %q missing in %q", option, tester.Texts())
		}
	}
	if slices.Contains(tester.Texts(), "推理：低") {
		t.Fatal("options repeat the 推理： prefix")
	}
}
