package desktop

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/mygo-kit"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/egoist/mygo/ui"
)

// itemsView 用消息滚动区虚拟绘制稳定消息；工具调用与结果按 CallID 配对。
func (d *Desktop) itemsView(c *ui.Context) {
	// 1. 派生行与配对结果，不复制后端业务状态。
	items := slices.Clone(d.items)
	target := d.readingTarget
	rows, results := buildMessageRows(items)
	finalKnown := d.getFinalKnownTurns(items)
	if d.messageList == nil {
		d.messageList = &ui.ListState{}
	}
	listState := d.messageList
	listState.Key = func(i int) any { return rows[i].Key }
	if d.turnJumpRequested != "" {
		d.jumpToTurn(listState, rows, items)
	}
	// 2. 只在用户滚到顶部后加载一页，列表保持原行锚点。
	ui.Column(c.Key(target)).Grow(1).Children(func() {
		opt := kit.MessageScrollerOpt{OnReachTop: func() {
			if d.readingTarget.Request == target.Request && d.itemCursor != 0 && !d.historyLoading {
				d.loadHistory()
			}
		}}
		scroller := kit.MessageScroller(c, listState, len(rows), opt, func(index int) {
			if d.readingTarget.Request != target.Request {
				return
			}
			row := rows[index]
			t := kit.GetTheme(c)
			// 每行是整宽外壳，内容列最宽 800 并水平居中；窄于 800 时由列表的 16 内边距提供左右留白。
			ui.Row(c.Key(row.Key)).Justify(ui.Center).Children(func() {
				container := ui.Column(c).Grow(1).MaxWidth(conversationColumnWidth).Gap(t.Space(2))
				view := items[row.Index]
				// 首次实时到达才渐显；历史与重连快照不设置到达时间。
				if arrived := view.ArrivedAt; !arrived.IsZero() && !t.Motion.Reduced && !c.Preferences().ReduceMotion {
					elapsed := c.Now().Sub(arrived)
					if elapsed < itemFadeDuration {
						container.Opacity(max(0, float32(elapsed)/float32(itemFadeDuration)))
						c.AnimationFrame()
					}
				}
				container.Children(func() {
					// 悬停的消息就是右键菜单作用的消息；菜单面板盖在消息上时消息不再被悬停，所以选条目时作用对象不变。
					if container.Hovered() {
						d.messageMenuItem = view
					}
					merged := make([]viewItem, 0, len(row.MergedErrors))
					for _, mergedIndex := range row.MergedErrors {
						merged = append(merged, items[mergedIndex])
					}
					d.itemView(c, view, results, merged, finalKnown)
					d.itemFooter(c, view)
				})
			})
		}).Selectable()
		// 手测：在消息文字上右键，应弹出主题菜单（选中文字时第一项「复制」，用户消息有「复制消息」，所有消息有「调试信息」）；
		// 在选中文字上右键后按 Esc，焦点回到输入框。消息列必须自己是 Selectable 容器 kit.ContextMenu 才接管右键，所以菜单挂在滚动区上。
		d.handleMenuClosed(kit.ContextMenu(c, scroller, d.messageMenuItems(c)))
	})
}

// jumpToTurn 把消息列表滚到 Turn 的输入消息；输入还没加载（更早的历史）时提示用户先向上加载。
func (d *Desktop) jumpToTurn(list *ui.ListState, rows []messageRow, items []viewItem) {
	// 1. 请求只处理一次，无论是否找到。
	turn := d.turns[d.turnJumpRequested]
	d.turnJumpRequested = ""
	itemIndex := getTurnInputIndex(turn, items)
	rowIndex := slices.IndexFunc(rows, func(row messageRow) bool { return row.Index == itemIndex })
	if rowIndex < 0 {
		d.showToast(kit.ToastOpt{Title: d.t("turn.not.loaded"), Kind: kit.ToastInfo})
		return
	}
	// 2. 输入消息显示在消息区顶部。
	list.ScrollTo(rowIndex, ui.Start)
}

// messageMenuItems 返回消息右键菜单：用户消息可复制全文，所有消息可打开调试信息；选中文字的「复制」由 kit 在菜单顶部加上。
func (d *Desktop) messageMenuItems(c *ui.Context) []kit.MenuItem {
	// 1. 菜单作用于最近悬停的消息；还没悬停过任何消息时没有条目。
	view := d.messageMenuItem
	if view.Item == nil {
		return nil
	}
	items := []kit.MenuItem{}
	if isUserMessage(view.Item) {
		text := contentText(view.Item.Message.Content)
		items = append(items, kit.MenuItem{Icon: kit.IconCopy, Title: d.t("copy.message"), OnSelect: func() { c.WriteClipboard(text) }})
	}
	return append(items, kit.MenuItem{Icon: kit.IconBug, Title: d.t("inspect"), OnSelect: func() { d.inspectItem(view) }})
}

// itemFooter 在真正最终结果下方显示右对齐的复制和调试图标。
func (d *Desktop) itemFooter(c *ui.Context, view viewItem) {
	// 1. 只有最终结果占用一行，其它消息不增加高度。
	t := kit.GetTheme(c)
	if view.Item.Kind != defs.ItemMessage || !d.isFinalResult(view) {
		return
	}
	ui.Row(c).Height(t.Space(6)).Gap(0).Justify(ui.End).AlignItems(ui.Center).Children(func() {
		copy := kit.SmallIconButton(c, kit.IconCopy, d.t("copy.result"))
		kit.Tooltip(c, copy, d.t("copy.result"))
		if copy.Clicked() {
			c.WriteClipboard(d.getFinalResultText(view.Item.TurnID))
		}
		d.inspectButton(c, view, true)
	})
}

// inspectButton 绘制打开调试面板的图标按钮，点击后选中该 Item；small 为真时用消息下方的 20 见方紧凑样式。
func (d *Desktop) inspectButton(c *ui.Context, view viewItem, small bool) ui.Element {
	var button ui.Element
	if small {
		button = kit.SmallIconButton(c, kit.IconBug, d.t("inspect"))
		kit.Tooltip(c, button, d.t("inspect"))
	} else {
		button = kit.IconButtonTip(c, kit.IconBug, d.t("inspect")).TextColor(kit.GetTheme(c).Colors.Muted)
	}
	if button.Clicked() {
		d.inspectItem(view)
	}
	return button
}

// inspectItem 打开下 Dock 的调试面板并选中该 Item。
func (d *Desktop) inspectItem(view viewItem) {
	d.inspectedItem = string(view.Item.ID)
	d.openPanel(panelDebug)
}

// buildMessageRows 为每个 Item 生成一行，并把工具结果按 CallID 配给调用；没有调用的结果单独成行；同一 Turn 内紧邻的模型错误合成一行。
func buildMessageRows(items []viewItem) ([]messageRow, map[string]*defs.ToolResult) {
	// 1. 结果按 CallID 合并到调用；没有调用的结果保留。
	results := map[string]*defs.ToolResult{}
	calls := map[string]bool{}
	for _, view := range items {
		if view.Item.ToolResult != nil {
			results[view.Item.ToolResult.CallID] = view.Item.ToolResult
		}
		if view.Item.ToolCall != nil {
			calls[view.Item.ToolCall.CallID] = true
		}
	}
	rows := []messageRow{}
	for index, view := range items {
		item := view.Item
		if item.ToolResult != nil && calls[item.ToolResult.CallID] {
			continue
		}
		if item.Kind == defs.ItemModelError && len(rows) > 0 {
			previous := &rows[len(rows)-1]
			if first := items[previous.Index].Item; first.Kind == defs.ItemModelError && first.TurnID == item.TurnID {
				previous.MergedErrors = append(previous.MergedErrors, index)
				continue
			}
		}
		key := string(item.ID)
		if item.HookRun != nil {
			key = "hook:" + string(item.HookRun.ID)
		}
		if item.ToolCall != nil {
			key = "tool:" + item.ToolCall.CallID
		} else if item.ToolResult != nil {
			key = "tool:" + item.ToolResult.CallID
		}
		rows = append(rows, messageRow{Key: key, Index: index})
	}
	return rows, results
}

// itemView 按十三种业务 Item 绘制原生正文；除正文外都是 ActivityCard 标题行，展开状态各自独立。
func (d *Desktop) itemView(c *ui.Context, view viewItem, results map[string]*defs.ToolResult, mergedErrors []viewItem, finalKnown map[defs.TurnID]bool) {
	// 1. 内容缺失以局部错误显示，未知类型保留原始数据。
	item := view.Item
	key := string(item.ID)
	if item.HookRun != nil {
		key = "hook:" + string(item.HookRun.ID)
	}
	switch item.Kind {
	case defs.ItemMessage:
		d.messageView(c, view, key, finalKnown)
	case defs.ItemReasoning:
		d.reasoningView(c, view, key)
	case defs.ItemToolCall:
		if item.ToolCall != nil {
			d.toolView(c, item.ToolCall, results[item.ToolCall.CallID], view)
		}
	case defs.ItemToolResult:
		if item.ToolResult != nil {
			d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconWrench, Title: d.t("tool.result.earlier")}, nil, func() { d.jsonView(c, "tool-result", d.t("tool.result"), item.ToolResult) })
		}
	case defs.ItemModelError:
		d.modelErrorView(c, view, key, mergedErrors)
	case defs.ItemContextSummary:
		if value := item.ContextSummary; value != nil {
			title := d.t("context.compressed")
			if view.Streaming {
				title = d.t("context.compressing")
			}
			d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconArchive, Title: title, Shimmer: view.Streaming}, nil, func() { d.markdownView(c, value.Text, false) })
		}
	case defs.ItemSkillInstruction:
		if value := item.SkillInstruction; value != nil {
			d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconBookOpen, Title: d.t("skill.loaded"), Summary: value.Name}, nil, func() {
				d.instructionSource(c, value.Path, value.Executor)
				d.markdownView(c, value.Text, false)
			})
		}
	case defs.ItemAgentsInstruction:
		if value := item.AgentsInstruction; value != nil {
			d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconFileText, Title: d.t("agents.loaded"), Summary: value.Path}, nil, func() {
				d.instructionSource(c, value.Path, value.Executor)
				d.markdownView(c, value.Text, false)
			})
		}
	case defs.ItemHookRun:
		d.hookRunView(c, view, key)
	case defs.ItemSessionFork:
		d.sessionForkView(c, view, key)
	case defs.ItemModelChange:
		if value := item.ModelChange; value != nil {
			d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconReplace, Title: fmt.Sprintf(d.t("model.changed.to"), value.PreviousModel.String(), value.Model.String())}, nil, nil)
		}
	case defs.ItemExecutorsChange:
		d.executorsChangeView(c, view, key)
	case defs.ItemTurnInterrupted:
		d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconBan, Title: d.t("turn.interrupted"), Tone: kit.ActivityWarning}, nil, nil)
	default:
		d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconBraces, Title: fmt.Sprintf(d.t("unknown.item.kind"), item.Kind)}, nil, func() { d.jsonView(c, "unknown-item", d.t("unknown.item"), item) })
	}
}

// activityItem 把 Item 画成一行 ActivityCard；展开状态按 key 保存，extraActions 和调试按钮只在悬停或聚焦本行时出现。
func (d *Desktop) activityItem(c *ui.Context, key string, view viewItem, opt kit.ActivityCardOpt, extraActions, detail func()) {
	// 1. 没有详情的条目不可展开，也不记录展开状态。
	open := d.itemExpanded[key]
	kit.ActivityCard(c, &open, opt, func() {
		if extraActions != nil {
			extraActions()
		}
		d.inspectButton(c, view, false)
	}, detail)
	d.itemExpanded[key] = open
}

// instructionSource 在 Skill 和目录规则详情顶部显示「路径@Executor」。
func (d *Desktop) instructionSource(c *ui.Context, path, executor string) {
	t := kit.GetTheme(c)
	t.TextRole(ui.Text(c, path+"@"+executor).Selectable(), kit.TextCaption).TextColor(t.Colors.Muted)
}

// messageView 绘制用户卡片、其他 Agent 来信、中途回复折叠条目和助手正文。
func (d *Desktop) messageView(c *ui.Context, view viewItem, key string, finalKnown map[defs.TurnID]bool) {
	// 1. 内容缺失以局部错误显示。
	item := view.Item
	t := kit.GetTheme(c)
	if item.Message == nil {
		kit.FieldMessage(c, kit.MessageError, d.t("invalid.message.payload"))
		return
	}
	// 2. Turn 完成并确认最终回复后，中途回复折叠成条目。
	if item.ModelCallID != "" && finalKnown[item.TurnID] && !d.isFinalResult(view) {
		d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconMessageSquare, Title: d.t("intermediate.response")}, nil, func() { d.contentView(c, item.Message.Content, false) })
		return
	}
	// 3. 其他 Agent 的来信：标题加 Session 链接，正文预览最多 4 行，超过 4 行才可展开。
	if item.Message.From != nil && item.Message.From.Kind == defs.IdentityAgent && item.ModelCallID == "" {
		preview := contentText(item.Message.Content)
		var detail func()
		if strings.Count(preview, "\n") >= 4 {
			detail = func() { d.contentView(c, item.Message.Content, view.Streaming) }
		}
		open := d.itemExpanded[key]
		opt := kit.ActivityCardOpt{Icon: kit.IconMessageSquare, Title: d.t("agent.mail.from"), Extra: func() { d.sessionLink(c, defs.SessionID(item.Message.From.ID)) }}
		kit.ActivityCard(c, &open, opt, func() { d.inspectButton(c, view, false) }, detail)
		d.itemExpanded[key] = open
		if !open {
			t.TextRole(ui.Text(c, preview).MaxLines(4), kit.TextProse).TextColor(t.Colors.Muted).Margin(0, 0, 0, t.Space(6))
		}
		return
	}
	if item.Message.From != nil && item.Message.From.Kind == defs.IdentityUser {
		d.userMessageView(c, view)
		return
	}
	d.contentView(c, item.Message.Content, view.Streaming)
}

// userMessageView 绘制用户整宽卡片：原文不按 Markdown 渲染，图片缩略图跟在下面，右上角是悬停出现的「复制消息」和「从这里分叉」。
func (d *Desktop) userMessageView(c *ui.Context, view viewItem) {
	// 1. 只有主 Session 已保存的消息可以从这里分叉；右侧按钮贴着卡片右缘，让右缘与对话列右缘对齐。
	item := view.Item
	t := kit.GetTheme(c)
	canFork := item.TurnID != "" && view.Ordinal != 0 && d.readingTarget.SessionID == d.conversation.RootSessionID
	card := ui.Row(c).Padding(t.Space(1), 0, t.Space(1), t.Space(3)).Gap(t.Space(2)).AlignItems(ui.Start).Radius(t.Metrics.RadiusLarge).Background(t.Colors.Editor).Border(1, t.Colors.Border)
	card.Children(func() {
		ui.Column(c).Grow(1).Shrink(1).Padding(t.Space(1), 0).Gap(t.Space(2)).Children(func() {
			for _, part := range item.Message.Content {
				if part == nil {
					continue
				}
				if part.Text != nil {
					t.TextRole(ui.Text(c, part.Text.Text), kit.TextCode)
				}
				if part.Image != nil {
					d.imageView(c, part.Image.DataURL, part.Image.Alt, 240)
				}
			}
		})
		// 2. 两个 28 见方图标按钮，平时透明，悬停或聚焦卡片时出现。
		actions := ui.Row(c).Shrink(0).AlignItems(ui.Center)
		if !card.Hovered() && !card.FocusWithin() {
			actions.Opacity(0)
		}
		actions.Children(func() {
			if kit.IconButtonTip(c, kit.IconCopy, d.t("copy.message")).TextColor(t.Colors.Muted).Clicked() {
				c.WriteClipboard(contentText(item.Message.Content))
			}
			if !canFork {
				return
			}
			if kit.IconButtonTip(c, kit.IconGitFork, d.t("fork.from.here")).TextColor(t.Colors.Muted).Clicked() {
				d.forkConversation(d.conversation, item.TurnID)
			}
		})
	})
}

// reasoningView 绘制思考条目：思考中闪烁，完成或中断后显示对应标题，展开看摘要或原文。
func (d *Desktop) reasoningView(c *ui.Context, view viewItem, key string) {
	// 1. 摘要优先，没有摘要时展示原文。
	reasoning := view.Item.Reasoning
	if reasoning == nil {
		return
	}
	text := strings.TrimSpace(strings.Join(reasoning.Summary, ""))
	if text == "" {
		text = strings.TrimSpace(strings.Join(reasoning.Content, ""))
	}
	opt := kit.ActivityCardOpt{Icon: kit.IconBrain, Title: d.t("reasoning.completed")}
	switch {
	case reasoning.Status == defs.ReasoningInterrupted:
		opt.Title, opt.Status = d.t("reasoning.interrupted"), kit.ActivityInterrupted
	case view.Streaming:
		opt.Title, opt.Shimmer = d.t("reasoning.thinking"), true
	}
	var detail func()
	if text != "" {
		detail = func() { d.markdownView(c, text, view.Streaming) }
	}
	d.activityItem(c, key, view, opt, nil, detail)
}

// modelErrorView 绘制同一 Turn 内紧邻的模型错误组：默认折叠，标题后附最后一次错误码；Turn 仍在运行时说明是第几次重试，带重新发送。
func (d *Desktop) modelErrorView(c *ui.Context, view viewItem, key string, merged []viewItem) {
	// 1. 摘要依次是错误码、重试次数（Turn 仍在运行说明模型还在重试）和合并的条数。
	group := append([]viewItem{view}, merged...)
	t := kit.GetTheme(c)
	turnID := view.Item.TurnID
	running := d.turns[turnID].Status == defs.TurnStatusRunning
	summary := []string{}
	if failure := group[len(group)-1].Item.ModelError; failure != nil && failure.Code != "" {
		summary = append(summary, failure.Code)
	}
	if running {
		summary = append(summary, fmt.Sprintf(d.t("model.error.retrying"), len(group)))
	} else if len(group) > 1 {
		summary = append(summary, fmt.Sprintf("×%d", len(group)))
	}
	opt := kit.ActivityCardOpt{Icon: kit.IconTriangleAlert, Title: d.t("model.error.title"), Summary: strings.Join(summary, " · "), Tone: kit.ActivityDanger}
	resend := func() {
		button := kit.IconButtonTip(c, kit.IconRefresh, d.t("resend")).TextColor(t.Colors.Muted).Disabled(d.connectionState != runtimeclient.ConnectionConnected || running)
		if button.Clicked() {
			d.resendTurn(turnID)
		}
	}
	d.activityItem(c, key, view, opt, resend, func() {
		for _, entry := range group {
			if failure := entry.Item.ModelError; failure != nil {
				text := failure.Message
				if failure.Code != "" {
					text = failure.Code + ": " + text
				}
				t.TextRole(ui.Text(c, text).Selectable(), kit.TextUI).TextColor(t.Colors.Danger)
			}
		}
	})
}

// hookRunView 绘制 Hook 运行条目：只有非完成状态显示状态图标。
func (d *Desktop) hookRunView(c *ui.Context, view viewItem, key string) {
	// 1. 状态决定图标和语义色。
	value := view.Item.HookRun
	if value == nil {
		return
	}
	opt := kit.ActivityCardOpt{Icon: kit.IconWebhook, Title: d.t("hook.run"), Summary: string(value.Event) + " · " + value.Handler}
	switch value.Status {
	case defs.HookRunRunning:
		opt.Status = kit.ActivityRunning
	case defs.HookRunBlocked:
		opt.Status, opt.Tone = kit.ActivityBlocked, kit.ActivityWarning
	case defs.HookRunStopped:
		opt.Status, opt.Tone = kit.ActivityStopped, kit.ActivityWarning
	case defs.HookRunFailed:
		opt.Status, opt.Tone = kit.ActivityFailed, kit.ActivityDanger
	}
	d.activityItem(c, key, view, opt, nil, func() {
		ui.Text(c, value.ExecutorID+" · "+value.SourcePath).Selectable()
		d.outputView(c, d.t("output"), d.t("copy.output"), value.Handler+"\n"+value.AdditionalContext+"\n"+value.ErrorMessage+"\n"+value.BlockReason)
		d.jsonView(c, "hook-details", d.t("hook.details"), value)
	})
}

// sessionForkView 绘制历史分界：用户分叉链接来源对话，子 Agent 链接来源 Session；不可展开。
func (d *Desktop) sessionForkView(c *ui.Context, view viewItem, key string) {
	fork := view.Item.SessionFork
	if fork == nil {
		return
	}
	opt := kit.ActivityCardOpt{Icon: kit.IconGitFork, Title: d.t("spawned.from")}
	if fork.SourceConversationID != "" {
		opt.Title = d.t("fork.from.source")
		opt.Extra = func() {
			if kit.Link(c, d.t("source.conversation")+" · "+string(fork.SourceConversationID)).Clicked() {
				d.openConversation(fork.SourceConversationID, fork.SourceSessionID)
			}
		}
	} else {
		opt.Extra = func() { d.sessionLink(c, fork.SourceSessionID) }
	}
	d.activityItem(c, key, view, opt, nil, nil)
}

// executorsChangeView 绘制 Executor 变化：一行前后对比，详情是前后列表。
func (d *Desktop) executorsChangeView(c *ui.Context, view viewItem, key string) {
	change := view.Item.ExecutorsChange
	if change == nil {
		return
	}
	t := kit.GetTheme(c)
	ids := func(executors []defs.TurnExecutor) string {
		names := make([]string, len(executors))
		for index, executor := range executors {
			names[index] = executor.ID
		}
		if len(names) == 0 {
			return "—"
		}
		return strings.Join(names, ", ")
	}
	list := func(label string, executors []defs.TurnExecutor) {
		ui.Column(c.Key(label)).Gap(0).Children(func() {
			t.TextRole(ui.Text(c, label), kit.TextCaption).TextColor(t.Colors.Muted)
			for _, executor := range executors {
				ui.Text(c, executor.ID+": "+executor.WorkspaceDirectory).SingleLine().Selectable()
			}
		})
	}
	title := fmt.Sprintf(d.t("executors.changed.to"), ids(change.PreviousExecutors), ids(change.Executors))
	d.activityItem(c, key, view, kit.ActivityCardOpt{Icon: kit.IconServerCog, Title: title}, nil, func() {
		list(d.t("executors.previous"), change.PreviousExecutors)
		list(d.t("executors.current"), change.Executors)
	})
}

// contentView 按材料原序展示文字和共享图片预览。
func (d *Desktop) contentView(c *ui.Context, content []*defs.Content, streaming bool) {
	for _, part := range content {
		if part == nil {
			continue
		}
		if part.Text != nil {
			d.markdownView(c, part.Text.Text, streaming)
		}
		if part.Image != nil {
			d.imageView(c, part.Image.DataURL, part.Image.Alt, 240)
		}
	}
}

// markdownView 将消息内容与桌面共享图片资源交给原生内容组件。
func (d *Desktop) markdownView(c *ui.Context, text string, streaming bool) {
	// 1. 网络读取和预览选择仍归 Desktop，排版、选择及代码块由 kit 负责。
	kit.Markdown(c, text, kit.MarkdownOpt{Streaming: streaming, ShareSelection: true, Image: func(url string) kit.ImageResource { return d.getImageResource(url, "") }, OnImage: func(url string) {
		d.imagePreview = url
		d.imagePreviewOpen = true
		d.imagePreviewState = kit.ImagePreviewState{Fit: true}
	}})
}

// getImageResource 复用本桌面已加载的图片，并按需启动所属任务。
func (d *Desktop) getImageResource(url, alt string) kit.ImageResource {
	// 1. 同一 URL 的材料、消息和工具结果共享解码资源。
	resource := d.images[url]
	if resource == nil {
		resource = &imageResource{}
		d.images[url] = resource
		d.loadImage(url)
	}
	image := kit.ImageResource{Source: resource.Source, Alt: alt, Loading: resource.Loading, Retry: func() { d.loadImage(url) }}
	if resource.Err != nil {
		image.Error = d.getImageErrorMessage(resource.Err)
	}
	return image
}

// imageView 显示内容附件并连接共享预览选择。
func (d *Desktop) imageView(c *ui.Context, url, alt string, size float32) {
	// 1. 资源加载与选择的业务归属不随组件构建改变。
	if url == "" {
		return
	}
	resource := d.getImageResource(url, alt)
	if resource.Source == nil {
		kit.Image(c, resource)
		return
	}
	if kit.Image(c, resource).Size(size, size).Fit(ui.Contain).Clicked() {
		d.imagePreview = url
		d.imagePreviewOpen = true
		d.imagePreviewState = kit.ImagePreviewState{Fit: true}
	}
}

// loadImage 在可取消任务中加载网络或内嵌图片。
func (d *Desktop) loadImage(url string) {
	// 1. 外部图片只接受 HTTP(S) 或 data URL，不执行路径或协议。
	resource := d.images[url]
	if resource == nil || resource.Loading {
		return
	}
	resource.Loading = true
	resource.Err = nil
	d.tasks.Go(func() {
		var data []byte
		var err error
		if strings.HasPrefix(url, "data:") {
			_, encoded, found := strings.Cut(url, ",")
			if !found {
				err = errInvalidImageDataURL
			} else {
				data, err = base64.StdEncoding.DecodeString(encoded)
			}
		} else if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
			request, e := http.NewRequestWithContext(d.ctx, http.MethodGet, url, nil)
			err = e
			if err == nil {
				response, e := (&http.Client{Timeout: 20 * time.Second}).Do(request)
				err = e
				if err == nil {
					if response.StatusCode != http.StatusOK {
						err = &imageHTTPError{Status: response.StatusCode}
					} else {
						data, err = io.ReadAll(io.LimitReader(response.Body, 20<<20))
					}
					response.Body.Close()
				}
			}
		} else {
			err = errUnsupportedImageURL
		}
		var source ui.ImageSource
		if err == nil {
			source, err = ui.DecodeImage(data)
		}
		d.window.Update(func() {
			if d.closed {
				return
			}
			resource.Loading = false
			resource.Source = source
			resource.Err = err
		})
	})
}

// getImageErrorMessage 把图片加载错误转成本地化的一句说明。
func (d *Desktop) getImageErrorMessage(err error) string {
	// 1. 已知的协议错误给具体说明，其余网络和解码错误给通用说明。
	var httpErr *imageHTTPError
	switch {
	case errors.As(err, &httpErr):
		return fmt.Sprintf(d.t("image.http.error"), httpErr.Status)
	case errors.Is(err, errUnsupportedImageURL):
		return d.t("image.unsupported.url")
	case errors.Is(err, errInvalidImageDataURL):
		return d.t("image.invalid.data")
	}
	return d.t("image.load.failed")
}

// imagePreviewView 在共享弹窗中预览原内容，不复制或重新读取图片。
func (d *Desktop) imagePreviewView(c *ui.Context) {
	// 1. 弹窗只拥有缩放与关闭，资源任务属于桌面。
	width, height := c.Size()
	kit.Dialog(c, &d.imagePreviewOpen, d.t("original.size"), func() {
		if kit.Button(c, d.t("close")).AlignSelf(ui.Start).Clicked() {
			d.imagePreviewOpen = false
		}
		kit.ImagePreview(c, d.getImageResource(d.imagePreview, ""), &d.imagePreviewState).Height(height - 180).FillWidth()
	}).Width(width - 48).Height(height - 48)
}

// outputView 把原文交给通用虚拟阅读器，title 是内容框标题，保留全文复制。
func (d *Desktop) outputView(c *ui.Context, title, copyLabel, text string) {
	// 1. 每个输出块只保存自己的原生滚动状态。
	t := kit.GetTheme(c)
	root := ui.Column(c)
	state := ui.Local(root, outputScrollKey{}, func() ui.ListState { return ui.ListState{FollowEnd: true} })
	root.Children(func() {
		kit.ANSIOutput(c, text, state, kit.ANSIOutputOpt{Title: title, CopyLabel: copyLabel, Plain: true}).Height(getOutputHeight(text, 320, &t)).MinHeight(60)
	})
}

// jsonView 保留原业务值及解析错误，树导航由通用内容组件负责；key 在同一父级内唯一标识这棵树。
func (d *Desktop) jsonView(c *ui.Context, key any, label string, value any) {
	// 1. 编码时不转义 HTML 字符，json.RawMessage 用原字节；编码失败时显示错误，原始 JSON 退回输出块。
	raw, err := marshalJSONView(value)
	if err != nil {
		kit.FieldMessage(c, kit.MessageError, err.Error())
		if content, ok := value.(json.RawMessage); ok {
			d.outputView(c, d.t("output"), d.t("copy.output"), string(content))
		}
		return
	}
	// 2. 业务快照变化时更新同一棵 JSON 树，不以展示树代替原数据。
	root := ui.Column(c.Key(key))
	state := ui.Local(root, jsonTreeKey{}, func() *kit.JSONState { state, _ := kit.NewJSONState(raw); return state })
	previous := ui.Local(root, jsonSourceKey{}, func() string { return string(raw) })
	if *previous != string(raw) {
		if err := (*state).Set(raw); err != nil {
			kit.FieldMessage(c, kit.MessageError, err.Error())
			return
		}
		*previous = string(raw)
	}
	root.Children(func() {
		t := kit.GetTheme(c)
		kit.JSONTree(c, *state, kit.JSONOpt{Title: label, CopyLabel: d.t("copy.json")}).Height(min(float32(strings.Count(string(raw), "\n")+1)*24+t.Space(9), 320))
	})
}

// getFinalKnownTurns 返回已有最终回复的 Turn 集合；每帧只计算一次。
func (d *Desktop) getFinalKnownTurns(items []viewItem) map[defs.TurnID]bool {
	known := map[defs.TurnID]bool{}
	for _, view := range items {
		if !known[view.Item.TurnID] && d.isFinalResult(view) {
			known[view.Item.TurnID] = true
		}
	}
	return known
}

// isFinalResult 此消息是否属于成功、完整且没有工具的最终模型调用？
func (d *Desktop) isFinalResult(view viewItem) bool {
	turn, ok := d.turns[view.Item.TurnID]
	if !ok || turn.Status != defs.TurnStatusCompleted || turn.FinalModelCallID == "" || view.Item.ModelCallID != turn.FinalModelCallID || view.Item.Kind != defs.ItemMessage {
		return false
	}
	ordinals := map[uint64]bool{}
	for _, item := range d.items {
		if item.Item.TurnID != turn.ID {
			continue
		}
		if item.Ordinal == 0 || item.Streaming {
			return false
		}
		ordinals[uint64(item.Ordinal)] = true
		if item.Item.ModelCallID == turn.FinalModelCallID && item.Item.ToolCall != nil {
			return false
		}
	}
	if turn.ItemsRange.FirstOrdinal == 0 {
		return false
	}
	for ordinal := turn.ItemsRange.FirstOrdinal; ordinal <= turn.ItemsRange.LastOrdinal; ordinal++ {
		if !ordinals[ordinal] {
			return false
		}
	}
	return true
}

// getFinalResultText 返回 Turn 里所有最终回复的文本，以空行分隔。
func (d *Desktop) getFinalResultText(turnID defs.TurnID) string {
	parts := []string{}
	for _, view := range d.items {
		if view.Item.TurnID == turnID && d.isFinalResult(view) && view.Item.Message != nil {
			parts = append(parts, contentText(view.Item.Message.Content))
		}
	}
	return strings.Join(parts, "\n\n")
}

// applyHookRun 合并同一次 Hook 的运行与结束事实。
func (d *Desktop) applyHookRun(event *runtimeclient.HookRunChanged) {
	for i := range d.items {
		if d.items[i].Item.HookRun != nil && d.items[i].Item.HookRun.ID == event.Run.ID {
			if d.items[i].Ordinal == 0 {
				d.items[i].Item.HookRun = event.Run.Clone()
			}
			return
		}
	}
	d.items = append(d.items, viewItem{Item: &defs.Item{ID: defs.ItemID("hook-" + string(event.Run.ID)), TurnID: event.TurnID, Kind: defs.ItemHookRun, HookRun: event.Run.Clone()}})
}
