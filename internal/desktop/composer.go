package desktop

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/session"
	"github.com/VaalaCat/openvaala/pkg/skill"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// composerView 编辑完整 Content 草稿，保留材料、图片及异步提交归属。
func (d *Desktop) composerView(c *ui.Context) {
	// 1. 草稿按服务器和对话只保存一份，文本是本次构建的临时编辑值。
	key := draftKey{d.serverID, d.readingTarget.ConversationID}
	draft := d.drafts[key]
	if draft == nil {
		draft = &conversationDraft{}
		d.drafts[key] = draft
	}
	text := draftText(draft.Content)
	t := kit.GetTheme(c)
	composer := ui.Column(c.Key("composer")).Shrink(0).Gap(0).Background(t.Colors.Editor).BorderWidth(1, 0, 0, 0).BorderColor(t.Colors.Border)
	composer.Children(func() {
		d.pendingInputsView(c)
		// 2. 输入区背景和上边线铺满中间区，内容列最宽 800 并水平居中；列比文字宽 8，是输入框左右各 4 的内边距。
		zone := ui.Column(c).Padding(8, 12)
		// 手测：从文件管理器把图片和非图片文件一起拖到输入区。步骤：拖入一张图片和一个 .txt，
		// 图片出现缩略图，同时弹出「只能添加图片」提示；再从系统剪贴板粘贴一张截图，也应出现缩略图。
		if files := zone.DroppedFiles(); files != nil {
			d.addImageFiles(draft, files)
		}
		zone.Children(func() {
			ui.Row(c).Justify(ui.Center).Children(func() {
				content := ui.Column(c).Grow(1).MaxWidth(conversationColumnWidth + t.Space(2)).Gap(4)
				content.Children(func() {
					d.attachmentsView(c, draft)
					input := d.composerInput(c, key, draft, &text)
					d.slashCompletionView(c, content, input, draft, text)
					if draft.Error != "" {
						ui.Row(c).Gap(t.Space(2)).AlignItems(ui.Center).Padding(0, t.Space(1)).Children(func() {
							kit.FieldMessage(c, kit.MessageError, draft.Error)
							if draft.NeedsProvider {
								if kit.PrimaryButton(c, d.t("onboarding.add")).Clicked() {
									d.openProviderOnboarding()
								}
							}
						})
					}
					d.composerToolbar(c, composer, key, draft)
				})
			})
		})
	})
}

// attachmentsView 显示已添加的图片和上下文材料；有图片时高度动画展开。
func (d *Desktop) attachmentsView(c *ui.Context, draft *conversationDraft) {
	// 1. 有图片时保留一行 40 高的缩略图条。
	attachments := ui.Column(c.Key("attachments")).ClipY()
	height := float32(0)
	for _, part := range draft.Content {
		if part != nil && part.Image != nil {
			height = 40
			break
		}
	}
	height = attachments.Animate("attachment-height", height, 150*time.Millisecond)
	attachments.Height(height).Children(func() {
		ui.ScrollHorizontal(c).Height(40).Children(func() {
			ui.Row(c).Gap(8).Children(func() {
				for index, part := range draft.Content {
					if part == nil || part.Image == nil {
						continue
					}
					name := part.Image.Alt
					if name == "" {
						name = fmt.Sprintf(d.t("image.numbered"), index+1)
					}
					thumb := d.getImageResource(part.Image.DataURL, name).Source
					if kit.AttachmentChip(c.Key(part), name, thumb, func() { draft.Content = slices.Delete(draft.Content, index, index+1) }).Clicked() {
						d.imagePreview = part.Image.DataURL
						d.imagePreviewOpen = true
					}
				}
			})
		})
	})
	// 2. 上下文材料折叠显示，展开状态按材料指针记在桌面上。
	for _, part := range draft.Content {
		if part != nil && part.Text != nil && part.Text.IsContext {
			open := d.itemExpanded[fmt.Sprintf("material-%p", part)]
			kit.Disclosure(c, d.t("context.material"), &open, func() { ui.Text(c, part.Text.Text).Selectable() })
			d.itemExpanded[fmt.Sprintf("material-%p", part)] = open
		}
	}
}

// composerInput 绘制消息输入框并处理发送、排队、停止、取回上一条和粘贴图片；text 在输入变化时被改写。
func (d *Desktop) composerInput(c *ui.Context, key draftKey, draft *conversationDraft, text *string) ui.Element {
	// 1. 有内容时用等宽字体；为空时用正文字体，占位符不带 Lilex 的字形。
	t := kit.GetTheme(c)
	area := kit.TextArea(c.Key("composer-text"), text).Label(d.t("message")).Placeholder(d.t("message.skill")).Border(0, ui.Transparent).Background(ui.Transparent).Padding(4).FocusRing(false)
	if *text == "" {
		t.TextRole(area, kit.TextUI).FixedLineHeight(t.Space(5))
	} else {
		t.TextRole(area, kit.TextCode)
	}
	input := area.Lines(2, 8).Bind(&d.composerHandle)
	_, d.composerCaret = input.TextAnchorCaret()
	if d.composerCaretRequested {
		input.SetTextSelection(d.composerRequestedCaret, d.composerRequestedCaret)
		d.composerCaret = d.composerRequestedCaret
		d.composerCaretRequested = false
	}
	input.OnPaste(func(_ string) bool {
		png := mygo.Clipboard.ReadImage()
		if len(png) == 0 {
			return false
		}
		d.addImageBytes(draft, [][]byte{png}, []string{"clipboard.png"})
		return true
	})
	input.OnTextKey(func(k ui.TextKey) bool {
		// 1. MyGo 在组字期间不分发此回调；技能补全浮层在它之前接管 Enter、Tab、方向键和 Esc。
		// 手测：用拼音输入法组字时按 Enter 选词，消息不应被发送（TestComposerInput 只覆盖框架的组字事件，真实输入法要人工确认）。
		draft.Content = replaceDraftText(draft.Content, k.Text())
		switch {
		case k.Key == ui.KeyEnter && k.Mods == 0:
			d.submitDraft(key, session.SessionInputModeSteer)
			return true
		// 手测：真实按 Ctrl+Enter（macOS 是 Command+Enter）。Turn 运行中且输入框有内容时，消息进入排队区而不是插队，
		// 同时排队按钮的提示文字显示同一个快捷键；Turn 空闲时按它与 Enter 一样直接发送。
		case k.Key == ui.KeyEnter && k.Mods == ui.Cmd:
			d.submitDraft(key, session.SessionInputModeNormal)
			return true
		// 2. 有运行中的 Turn 且输入框为空时，Escape 停止它。
		case k.Key == ui.KeyEscape && k.Mods == 0 && d.getSessionInfo().ActiveTurnID != "" && !isContentPresent(draft.Content):
			d.interruptTurn(key.ConversationID)
			return true
		// 3. 输入框为空或内容还是上次取回的原样时，Up 取回更早的一条自己发的消息。
		case k.Key == ui.KeyUp && k.Mods == 0 && (k.Text() == "" || k.Text() == d.recalledMessage):
			offset := d.recalledOffset + 1
			if k.Text() == "" {
				offset = 1
			}
			previous, found := getPreviousUserMessage(d.items, offset)
			if !found {
				return false
			}
			d.recalledOffset, d.recalledMessage = offset, previous
			d.composerRequestedCaret, d.composerCaretRequested = len([]rune(previous)), true
			k.SetText(previous)
			draft.Content = replaceDraftText(draft.Content, previous)
			return true
		}
		return false
	})
	if input.Changed() {
		draft.Content = replaceDraftText(draft.Content, *text)
	}
	// 先处理本帧的输入变化，再把焦点交给输入框，避免同一帧写入的文字被焦点切换吞掉。
	if d.composerFocusRequested {
		input.Focus()
		d.composerFocusRequested = false
	}
	return input
}

// slashCompletionView 在光标所在的词以 / 开头时，于输入区上方显示技能补全浮层；选中后把该词换成 /name。
func (d *Desktop) slashCompletionView(c *ui.Context, anchor, input ui.Element, draft *conversationDraft, text string) {
	// 1. 只在输入框有焦点且光标所在的词以 / 开头时打开，同一来源的技能只请求一次。
	query, isSlash := getSlashQuery(text, d.composerCaret)
	key := getSkillCatalogKey(d.readingTarget.ConversationID, d.newAgent, d.newExecutor, d.newDirectory)
	opt := kit.CompletionOpt{Open: isSlash && input.Focused(), Empty: d.t("no.matching.skills")}
	var matches []skill.SkillInfo
	if opt.Open {
		// 两个浮层都在输入区上方，输入技能时先收起工作位置表单。
		d.workLocationPopoverOpen = false
		catalog := d.loadSkillCatalog(key)
		matches = getSkillMatches(catalog.Skills, query)
		opt.Loading = catalog.Loading
		if catalog.Err != nil {
			opt.Error = d.getErrorMessage(catalog.Err)
		}
	} else if catalog := d.skillCatalogs[key]; catalog != nil && catalog.Err != nil {
		// 2. 失败的结果不缓存，下次再输入 / 时重试。
		delete(d.skillCatalogs, key)
	}
	for _, match := range matches {
		opt.Items = append(opt.Items, kit.CompletionItem{Title: "/" + match.Name, Description: getSingleLine(match.Description)})
	}
	// 3. 选中的技能替换光标所在的词，下一帧把光标放到补全后的空格后。
	selected := kit.CompletionMenu(c, anchor, input, opt)
	if selected < 0 || selected >= len(matches) {
		return
	}
	value, caret := insertSlashSkillAt(text, matches[selected].Name, d.composerCaret)
	draft.Content = replaceDraftText(draft.Content, value)
	d.composerRequestedCaret, d.composerCaretRequested = caret, true
	d.composerFocusRequested = true
	c.AnimationFrame()
}

// composerToolbar 绘制输入区底部：添加图片在左，会话控件、排队和发送（或停止）在右。
func (d *Desktop) composerToolbar(c *ui.Context, composer ui.Element, key draftKey, draft *conversationDraft) {
	t := kit.GetTheme(c)
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		if kit.IconButtonTip(c, kit.IconImagePlus, d.t("add.images")).Clicked() {
			d.chooseImages(draft)
		}
		ui.Spacer(c)
		d.sessionControls(c, composer)
		running := d.getSessionInfo().ActiveTurnID != ""
		canSend := d.connectionState == runtimeclient.ConnectionConnected && !draft.Pending && isContentPresent(draft.Content) && (key.ConversationID == "" || d.readingReady)
		queueVisible := running && isContentPresent(draft.Content)
		// 排队按钮只在运行中且有内容时出现，宽度动画展开。
		queueSlot := ui.Box(c.Key("queue-entry")).ClipX().Disabled(!queueVisible)
		queueWidth := float32(0)
		if queueVisible {
			queueWidth = t.Metrics.ControlHeight
		}
		queueWidth = queueSlot.Animate("queue-width", queueWidth, 150*time.Millisecond)
		queueSlot.Width(queueWidth)
		queueSlot.Children(func() {
			if queueVisible || queueWidth > 0 {
				queue := kit.IconButton(c, kit.IconListEnd, d.t("queue")).Disabled(!canSend || !queueVisible)
				kit.Tooltip(c, queue, fmt.Sprintf(d.t("queue.hint"), getQueueShortcut()))
				if queue.Clicked() {
					d.submitDraft(key, session.SessionInputModeNormal)
				}
			}
		})
		if running && !isContentPresent(draft.Content) {
			stop := kit.DangerButton(c.Key("send-stop"), "").Size(t.Metrics.ControlHeight, t.Metrics.ControlHeight).Padding(0).Justify(ui.Center).AlignItems(ui.Center).Label(d.t("stop")).Disabled(d.connectionState != runtimeclient.ConnectionConnected)
			stop.Children(func() { ui.Icon(c, kit.IconStop).Size(16, 16) })
			kit.Tooltip(c, stop, fmt.Sprintf(d.t("stop.hint"), "Esc"))
			if stop.Clicked() {
				d.interruptTurn(key.ConversationID)
			}
			return
		}
		label := d.t("send")
		if draft.Pending {
			label = d.t("sending")
		}
		sendButton := kit.Button
		if canSend {
			sendButton = kit.PrimaryButton
		}
		send := sendButton(c.Key("send-stop"), "").Size(t.Metrics.ControlHeight, t.Metrics.ControlHeight).Padding(0).Justify(ui.Center).AlignItems(ui.Center).Label(label).Disabled(!canSend)
		send.Children(func() { ui.Icon(c, kit.IconArrowUp).Size(16, 16) })
		hint := fmt.Sprintf(d.t("send.hint"), "Enter")
		if draft.Pending {
			hint = d.t("sending")
		}
		if key.ConversationID != "" && !d.readingReady {
			hint = d.t("loading.history")
		}
		if d.connectionState != runtimeclient.ConnectionConnected {
			hint = d.t("connecting")
		}
		if d.connectionError != nil {
			hint = d.getConnectionTooltip()
		}
		kit.Tooltip(c, send, hint)
		if send.Clicked() {
			d.submitDraft(key, session.SessionInputModeSteer)
		}
	})
}

// submitDraft 提交原草稿快照，成功只移除实际发送部分。
func (d *Desktop) submitDraft(key draftKey, mode session.SessionInputMode) {
	// 1. 捕获完整内容和创建参数，禁止重复 pending 提交。
	draft := d.drafts[key]
	if draft == nil || draft.Pending || !isContentPresent(draft.Content) || d.client == nil || d.connectionState != runtimeclient.ConnectionConnected || key.ConversationID != "" && !d.readingReady {
		return
	}
	draft.Pending = true
	draft.Error, draft.NeedsProvider = "", false
	d.composerFocusRequested = true
	sent := slices.Clone(draft.Content)
	client := d.client
	requestID := d.readingTarget.Request
	var ref defs.ModelRef
	if d.newModel != "" {
		ref, _ = defs.ParseModelRef(d.newModel)
	}
	create := runtimeclient.CreateConversationRequest{AgentName: d.newAgent, Model: defs.ModelSelection{Ref: ref, ReasoningEffort: d.newEffort}, ExecutorSettings: defs.ExecutorSettings{DefaultExecutorID: d.newExecutor, WorkspaceDirectories: map[string]string{d.newExecutor: d.newDirectory}}}
	// 2. 创建成功即迁移草稿身份；首次提交失败仍留在真实对话。
	d.tasks.Go(func() {
		conversationID := key.ConversationID
		var created defs.Conversation
		var err error
		resolveFailed := false
		if conversationID == "" {
			// 目录在 Executor 上解析成绝对路径后才创建对话；解析失败时不创建。
			if directory := create.ExecutorSettings.WorkspaceDirectories[create.ExecutorSettings.DefaultExecutorID]; directory != "" {
				var resolved string
				resolved, err = client.ResolveWorkspaceDirectory(d.ctx, &runtimeclient.ResolveWorkspaceDirectoryRequest{ExecutorID: create.ExecutorSettings.DefaultExecutorID, Directory: directory})
				create.ExecutorSettings.WorkspaceDirectories[create.ExecutorSettings.DefaultExecutorID] = resolved
				resolveFailed = err != nil
			}
			if err == nil {
				created, err = client.CreateConversation(d.ctx, &create)
			}
			if err == nil {
				conversationID = created.ID
				d.window.Update(func() {
					if d.closed {
						return
					}
					newKey := draftKey{key.ServerID, created.ID}
					d.drafts[newKey] = draft
					delete(d.drafts, key)
					if d.readingTarget.Request == requestID {
						d.conversation = created
						d.openConversation(created.ID, created.RootSessionID)
					}
				})
			}
		}
		if err == nil {
			_, err = client.SubmitInput(d.ctx, &runtimeclient.SubmitInputRequest{ConversationID: conversationID, Mode: mode, Content: sent})
		}
		d.window.Update(func() {
			if d.closed {
				return
			}
			draft.Pending = false
			if err != nil {
				draft.Error, draft.NeedsProvider = d.getErrorMessage(err), isNoModelError(err)
				if resolveFailed {
					draft.Error = d.t("directory.invalid") + "：" + getDirectoryErrorDetail(err)
				}
				if errors.Is(err, runtimeclient.ErrConnectionLost) {
					draft.Error = d.t("result.not.confirmed.check.the.latest.conversation.before.sending.again")
				}
				return
			}
			draft.Content = removeSentContent(draft.Content, sent)
			draft.Error, draft.NeedsProvider = "", false
			d.loadConversations(true)
		})
	})
}

// pendingInputsView 在输入区上方显示排队栏；展开后逐条列出服务端队列，操作携带原对话、输入和轮次。
func (d *Desktop) pendingInputsView(c *ui.Context) {
	// 1. 不在本地猜测消费或删除队列行；没有排队输入时不占高度。
	if len(d.pendingInputs.Inputs) == 0 {
		return
	}
	t := kit.GetTheme(c)
	ui.Column(c.Key("queue-bar")).Gap(0).Background(t.Colors.Panel).BorderWidth(0, 0, 1, 0).BorderColor(t.Colors.BorderSubtle).Children(func() {
		// 2. 28 高的活动栏：文字在 x=16，展开箭头在行尾。
		label := fmt.Sprintf(d.t("queued.count"), len(d.pendingInputs.Inputs))
		toggle := ui.ButtonBase(c).Height(t.Metrics.ControlHeight).Padding(0, t.Space(4)).Gap(t.Space(2)).Justify(ui.Start).AlignItems(ui.Center).Radius(0).Label(label)
		background := ui.Transparent
		if toggle.Pressed() {
			background = t.Colors.Active
		} else if toggle.Hovered() {
			background = t.Colors.Hover
		}
		toggle.Background(background)
		toggle.Children(func() {
			ui.Text(c, label).SingleLine().TextColor(t.Colors.Muted)
			ui.Spacer(c)
			chevron := kit.IconChevronRight
			if d.queueExpanded {
				chevron = kit.IconChevronDown
			}
			ui.Icon(c, chevron).Size(16, 16).Shrink(0).TextColor(t.Colors.Muted)
		})
		if toggle.Clicked() {
			d.queueExpanded = !d.queueExpanded
		}
		if !d.queueExpanded {
			return
		}
		// 3. 展开后每条输入一行，沿用取回、插队、撤回动作。
		for _, input := range d.pendingInputs.Inputs {
			ui.Row(c.Key(input.ItemID)).Height(t.Metrics.ControlHeight).Padding(0, t.Space(1), 0, t.Space(4)).Gap(t.Space(1)).AlignItems(ui.Center).Children(func() {
				label := input.Preview
				if input.ImageCount > 0 {
					label += fmt.Sprintf(" [▧ %d]", input.ImageCount)
				}
				preview := ui.Text(c, label).SingleLine().Grow(1)
				full, _ := c.MeasureText(0, ui.Span{Text: label})
				if preview.Bounds().W > 0 && full > preview.Bounds().W {
					kit.Tooltip(c, preview, label)
				}
				if kit.Button(c, d.t("retrieve")).Clicked() {
					d.queueAction("retrieve", input.ItemID, "")
				}
				items := []kit.MenuItem{}
				if d.pendingInputs.SteerTurnID != "" && !input.IsSteer {
					items = append(items, kit.MenuItem{Title: d.t("join.this.turn"), OnSelect: func() { d.queueAction("steer", input.ItemID, d.pendingInputs.SteerTurnID) }})
				}
				if turnID := d.getSessionInfo().ActiveTurnID; turnID != "" && !input.IsSteer {
					items = append(items, kit.MenuItem{Title: d.t("prioritize"), OnSelect: func() { d.queueAction("prioritize", input.ItemID, turnID) }})
				}
				items = append(items, kit.MenuItem{Separator: true}, kit.MenuItem{Title: d.t("withdraw"), Danger: true, OnSelect: func() {
					d.confirm = confirmation{Kind: "withdraw", Conversation: d.conversation, ItemID: input.ItemID, Name: input.Preview}
					d.confirmOpen = true
				}})
				d.handleMenuClosed(kit.MenuButton(c, kit.IconButtonTip(c, kit.IconEllipsis, d.t("pending.actions")), items))
			})
		}
	})
}

// resendTurn 把该 Turn 的用户输入原样重新提交到当前对话。
func (d *Desktop) resendTurn(turnID defs.TurnID) {
	// 1. 取该 Turn 的用户消息内容；找不到时没有可重发的输入。
	var content []*defs.Content
	for _, view := range d.items {
		if view.Item.TurnID == turnID && isUserMessage(view.Item) {
			content = slices.Clone(view.Item.Message.Content)
			break
		}
	}
	if len(content) == 0 || d.client == nil {
		return
	}
	// 2. 失败只提示，不改动草稿。
	conversationID, client := d.readingTarget.ConversationID, d.client
	d.tasks.Go(func() {
		_, err := client.SubmitInput(d.ctx, &runtimeclient.SubmitInputRequest{ConversationID: conversationID, Mode: session.SessionInputModeSteer, Content: content})
		d.window.Update(func() {
			if !d.closed && err != nil {
				d.showFailure(err)
			}
		})
	})
}

// exportConversation 把已加载的 Item 导出为 Markdown，写入用户在保存对话框里选的文件。
func (d *Desktop) exportConversation(conv defs.Conversation) {
	// 1. 导出内容取窗口线程当下已加载的 Item。
	// 手测（macOS 真机）：对话菜单或命令面板选「导出为 Markdown」，应弹出系统保存对话框，默认文件名是对话标题加 .md；
	// 选位置确认后出现「已导出」通知并带路径，文件内容是整段对话；在对话框里点取消不应出现任何提示。
	title := conversationTitle(conv)
	markdown := buildMarkdownExport(title, d.t("you"), d.t("assistant"), d.items)
	win := d.window
	defaultName := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(title) + ".md"
	dialogTitle := d.t("export.markdown")
	// 2. 保存对话框和写文件在任务里进行；取消不产生提示。
	d.tasks.Go(func() {
		path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{Parent: win, Title: dialogTitle, DefaultPath: defaultName, Filters: []mygo.FileFilter{{Name: "Markdown", Extensions: []string{"md"}}}})
		if err == nil && path != "" {
			err = os.WriteFile(path, []byte(markdown), 0o644)
		}
		win.Update(func() {
			if d.closed || err == nil && path == "" {
				return
			}
			if err != nil {
				d.showToast(kit.ToastOpt{Title: d.t("export.failed"), Description: err.Error(), Kind: kit.ToastError})
				return
			}
			d.showSuccess(d.t("export.done"), path)
		})
	})
}

// queueAction 执行原子队列操作，取回成功才更新原对话草稿。
func (d *Desktop) queueAction(kind string, itemID defs.ItemID, turnID defs.TurnID) {
	// 1. 保存原目标，异步失败不覆盖草稿；菜单选择后焦点回到输入框。
	d.composerFocusRequested = true
	key := draftKey{d.serverID, d.readingTarget.ConversationID}
	client := d.client
	d.tasks.Go(func() {
		var content []*defs.Content
		var err error
		switch kind {
		case "retrieve":
			content, err = client.WithdrawInput(d.ctx, key.ConversationID, itemID, true)
		case "steer":
			err = client.SteerInput(d.ctx, key.ConversationID, itemID, turnID)
		case "prioritize":
			err = client.InterruptTurnWithInput(d.ctx, key.ConversationID, turnID, itemID)
		}
		d.window.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			if kind == "retrieve" {
				draft := d.drafts[key]
				if draft == nil {
					draft = &conversationDraft{}
					d.drafts[key] = draft
				}
				draft.Content = content
			}
		})
	})
}

// chooseImages 使用系统多图选择，取消不产生错误。
func (d *Desktop) chooseImages(draft *conversationDraft) {
	win := d.window
	d.tasks.Go(func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: win, Multiple: true, Filters: []mygo.FileFilter{{Name: d.t("image.files"), Extensions: []string{"png", "jpg", "jpeg", "gif", "webp", "bmp"}}}})
		win.Update(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.showFailure(err)
				return
			}
			d.addImageFiles(draft, paths)
		})
	})
}

// addImageFiles 在任务中读取附件，结果仍返回原草稿。
func (d *Desktop) addImageFiles(draft *conversationDraft, paths []string) {
	if len(paths) == 0 {
		return
	}
	d.tasks.Go(func() {
		data := [][]byte{}
		names := []string{}
		var readErr error
		for _, path := range paths {
			info, err := os.Stat(path)
			if err == nil && info.Size() > 10<<20 {
				err = fmt.Errorf("%w: %s", errImageTooLarge, path)
			}
			if err != nil {
				readErr = errors.Join(readErr, err)
				continue
			}
			bytes, err := os.ReadFile(path)
			if err != nil {
				readErr = errors.Join(readErr, err)
				continue
			}
			if !strings.HasPrefix(http.DetectContentType(bytes), "image/") {
				readErr = errors.Join(readErr, fmt.Errorf("%w: %s", errNotImage, path))
				continue
			}
			data = append(data, bytes)
			names = append(names, filepath.Base(path))
		}
		attachments, prepareErr := prepareImageAttachments(d.ctx, data, names)
		readErr = errors.Join(readErr, prepareErr)
		d.window.Update(func() {
			if d.closed {
				return
			}
			d.acceptImageAttachments(draft, attachments, readErr)
		})
	})
}

// addImageBytes 在桌面短任务中解码和编码剪贴板图片。
func (d *Desktop) addImageBytes(draft *conversationDraft, data [][]byte, names []string) {
	// 1. 任务捕获实际草稿对象，返回后不重新按当前页面查找。
	d.tasks.Go(func() {
		attachments, err := prepareImageAttachments(d.ctx, data, names)
		d.window.Update(func() {
			if !d.closed {
				d.acceptImageAttachments(draft, attachments, err)
			}
		})
	})
}

// prepareImageAttachments 按输入顺序解码图片并生成完整 Content。
func prepareImageAttachments(ctx context.Context, data [][]byte, names []string) ([]imageAttachment, error) {
	// 1. 图片处理不访问窗口状态，取消后不再读取后续图片。
	attachments := []imageAttachment{}
	var preparationError error
	for index, bytes := range data {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bitmap, err := ui.DecodeBitmap(bytes)
		if err != nil {
			preparationError = errors.Join(preparationError, err)
			continue
		}
		url := "data:" + http.DetectContentType(bytes) + ";base64," + base64.StdEncoding.EncodeToString(bytes)
		attachments = append(attachments, imageAttachment{Content: &defs.Content{Kind: defs.ContentImage, Image: &defs.ImageContent{DataURL: url, Alt: names[index]}}, Bitmap: bitmap, Bytes: len(bytes)})
	}
	return attachments, preparationError
}

// acceptImageAttachments 在主线程校验最新草稿容量并加入已准备图片。
func (d *Desktop) acceptImageAttachments(draft *conversationDraft, attachments []imageAttachment, preparationError error) {
	// 1. 读取原草稿本次最新附件，容量只在写入处验证。
	total := 0
	for _, part := range draft.Content {
		if part != nil && part.Image != nil {
			_, encoded, found := strings.Cut(part.Image.DataURL, ",")
			if found {
				total += base64.StdEncoding.DecodedLen(len(encoded))
				if strings.HasSuffix(encoded, "==") {
					total -= 2
				} else if strings.HasSuffix(encoded, "=") {
					total--
				}
			}
		}
	}
	for _, attachment := range attachments {
		total += attachment.Bytes
	}
	if total > 10<<20 {
		d.showToast(kit.ToastOpt{Title: d.t("images.exceed.10.mib"), Kind: kit.ToastError})
		return
	}
	// 2. 保留原图片顺序和提交生命周期，部分读取失败不丢其他内容。
	for _, attachment := range attachments {
		draft.Content = append(draft.Content, attachment.Content)
		d.images[attachment.Content.Image.DataURL] = &imageResource{Source: attachment.Bitmap}
	}
	if preparationError != nil {
		d.showImageFailure(preparationError)
	}
}

// showImageFailure 用本地化标题提示添加图片失败，文件名和原因放进详情。
func (d *Desktop) showImageFailure(err error) {
	// 1. 按最具体的原因选标题：不是图片、超过大小、其余读取或解码失败。
	title := d.t("image.read.failed")
	switch {
	case errors.Is(err, errNotImage):
		title = d.t("images.only")
	case errors.Is(err, errImageTooLarge):
		title = d.t("image.too.large")
	}
	d.showToast(kit.ToastOpt{Title: title, Description: err.Error(), Kind: kit.ToastError})
}

// loadSkillCatalog 返回来源 key 下的技能目录；首次请求时发出 skill/list，同一来源之后直接复用结果。
func (d *Desktop) loadSkillCatalog(key skillCatalogKey) *skillCatalog {
	// 1. 已请求过的来源（含请求中）直接返回；没连上时不登记，连上后再请求。
	if catalog := d.skillCatalogs[key]; catalog != nil {
		return catalog
	}
	catalog := &skillCatalog{Loading: true}
	if d.client == nil {
		return catalog
	}
	d.skillCatalogs[key] = catalog
	// 2. 已有对话只带 ConversationID；草稿带 Agent 和默认 Executor 的目录。
	request := runtimeclient.GetSkillsRequest{ConversationID: key.ConversationID}
	if key.ConversationID == "" {
		request.AgentName = key.AgentName
		request.ExecutorSettings = defs.ExecutorSettings{DefaultExecutorID: key.ExecutorID, WorkspaceDirectories: map[string]string{key.ExecutorID: key.Directory}}
	}
	client := d.client
	d.tasks.Go(func() {
		skills, err := client.GetSkills(d.ctx, &request)
		d.window.Update(func() {
			if d.closed {
				return
			}
			catalog.Skills, catalog.Err, catalog.Loading = skills, err, false
		})
	})
	return catalog
}

// interruptTurn 请求停止对话里运行中的 Turn。
func (d *Desktop) interruptTurn(conversationID defs.ConversationID) {
	client := d.client
	d.tasks.Go(func() {
		_, err := client.InterruptTurn(d.ctx, conversationID)
		d.window.Update(func() {
			if !d.closed && err != nil {
				d.showFailure(err)
			}
		})
	})
}
