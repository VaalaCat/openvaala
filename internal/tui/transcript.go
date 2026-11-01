package tui

import (
	"maps"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/storage"
	"github.com/charmbracelet/x/ansi"
)

// transcript 保存当前对话的历史 Item、生成中 Item 和回复定稿进度，返回滚动历史行与实时尾部。
// tuiModel 串行调用它；历史原文留在内存中供面板和重新渲染读取。
type transcript struct {
	renderer *markdownRenderer
	styles   tuiStyles
	// statuses 与 modelNames 由 tuiModel 拥有，transcript 只读它们：渲染子 Agent 名、协作工具的目标与模型切换提示时查名字。
	statuses   *sessionStatuses
	modelNames map[defs.ModelRef]string
	// liveHeight 是实时区域最多能显示的行数；未闭合代码块的预览只渲染这么多行。
	liveHeight int

	// lastSavedOrdinal 是已处理的最后一个流水号，重复交付的 Item 按它去重。
	lastSavedOrdinal storage.Ordinal
	// savedItems 按 ItemID 保存已保存的 Item（上下文面板的预览、排队输入是否已开始执行）。
	savedItems map[defs.ItemID]*defs.Item
	// userInputs 是已保存的用户输入正文，按保存顺序（/resume 时输入历史的初始值）。
	userInputs []string
	// turnOpeningInputs 是每一轮开头那批用户消息，按轮次出现的顺序（回退面板列出的轮次）。
	turnOpeningInputs []turnOpeningInput
	// subagentResults 是各子 Session 最近一条结果通知的完整正文（子 Agent 面板的"最近结果"）。
	subagentResults map[defs.SessionID]string
	// activeTurnID 是内容流中正在进行的 Turn：同步时取运行信息，之后由 turn/started 与 turn/finished 更新。
	activeTurnID defs.TurnID
	// streamingItems 保存从开始事件或同步结果里的生成中 Item 登记、尚未移除的 Item，按登记顺序；增量按 ItemIndex 定位。
	streamingItems []*streamingItem
	// replies 是根 Agent 回复的定稿进度，按 ItemID；回复保存后删除。
	replies map[defs.ItemID]*replyStream
	// now 是 tuiModel 的时钟，运行中的组从它取开始时刻与耗时。
	now func() time.Time
	// isLocalServer 读 tuiModel 当前连上的是否本机 serve？Executor 变化一行据此决定 local 目录是否缩写为 ~。
	isLocalServer func() bool
	// getDefaultExecutorID 读 tuiModel 当前设置的默认 Executor：还没有结果的工具调用在参数没写 executor 时显示它。
	getDefaultExecutorID func() string
	// stepGroups 是当前对话的全部思考与工具组，按出现顺序。
	stepGroups []*stepGroup
	// openStepGroup 是还在接收步骤的组，没有时为 nil。
	openStepGroup *stepGroup
	// toolCalls、callGroups 按 CallID 找到调用和它所在的组；工具进度也按 CallID 找到调用。
	toolCalls  map[string]*toolCall
	callGroups map[string]*stepGroup
	// reasonings、reasoningGroups 按 ItemID 找到推理和它所在的组：增量追加片段，中断且不保存时从组里去掉。
	reasonings      map[defs.ItemID]*groupReasoning
	reasoningGroups map[defs.ItemID]*stepGroup
	// history 按首次出现顺序保存历史单元，展开视图读取这些内容。
	history []historyEntry
	// lastPrintedTurnID 是最近一个已登记历史单元所属的非空 Turn。
	lastPrintedTurnID defs.TurnID
}

// streamingItem 保存从开始事件或同步结果里的生成中 Item 登记、尚未移除的 Item，以及它在本轮输出中的位置。
type streamingItem struct {
	itemIndex int
	item      *defs.Item
}

// newTranscript 创建对话历史和流式回复记录。
func newTranscript(renderer *markdownRenderer, styles tuiStyles, statuses *sessionStatuses, modelNames map[defs.ModelRef]string, liveHeight int, now func() time.Time, isLocalServer func() bool, getDefaultExecutorID func() string) *transcript {
	// 1. 保存渲染依赖和查询函数，初始化 Item、回复、工具与推理索引。
	return &transcript{
		renderer:             renderer,
		styles:               styles,
		statuses:             statuses,
		modelNames:           modelNames,
		liveHeight:           liveHeight,
		savedItems:           make(map[defs.ItemID]*defs.Item),
		subagentResults:      make(map[defs.SessionID]string),
		replies:              make(map[defs.ItemID]*replyStream),
		now:                  now,
		isLocalServer:        isLocalServer,
		getDefaultExecutorID: getDefaultExecutorID,
		toolCalls:            make(map[string]*toolCall),
		callGroups:           make(map[string]*stepGroup),
		reasonings:           make(map[defs.ItemID]*groupReasoning),
		reasoningGroups:      make(map[defs.ItemID]*stepGroup),
	}
}

// setRenderer 换用新宽度的渲染器并刷新实时回复尾部。
func (t *transcript) setRenderer(renderer *markdownRenderer, liveHeight int) {
	// 1. 保存渲染器和实时行数上限，再重新渲染尚未保存的回复。
	t.renderer, t.liveHeight = renderer, liveHeight
	for _, reply := range t.replies {
		t.refreshLiveLines(reply)
	}
}

// getScrollbackLines 按当前宽度重新渲染已经写出的历史内容。
func (t *transcript) getScrollbackLines() []string {
	// 1. 按历史顺序渲染输入、已定稿回复、已写出的步骤组、分隔线和提示。
	var lines []string
	for _, entry := range t.history {
		var block []string
		switch entry.kind {
		case historyUserInput:
			block = t.formatUserInput(entry.userInput)
		case historyReply:
			text := entry.reply.source.String()[:entry.reply.markdown.getFinalizedLength()]
			block = t.renderer.render(text)
			if len(block) > 0 {
				block[0] = t.markReplyStart(block[0])
			}
		case historyStepGroup:
			if !entry.group.isPrinted {
				continue
			}
			if len(entry.group.getCalls()) == 0 {
				lines = append(lines, t.formatReasoningLines(entry.group)...)
				continue
			}
			block = formatStepGroupSummary(entry.group, t.renderer.width, t.styles, t.statuses, t.getDefaultExecutorID())
		case historyTurnDivider:
			block = []string{t.styles.faint.Render(strings.Repeat("─", t.renderer.width))}
		case historyLine:
			block = []string{truncateLine(formatStyledTexts(entry.line), t.renderer.width)}
		case historyHookRun:
			lines = append(lines, t.formatHookRunNotices(entry.hookRun, entry.hookToolName)...)
			continue
		}
		lines = append(lines, formatBlock(block)...)
	}
	return lines
}

// handleSessionSynced 同步已保存内容和生成中内容，收尾同步中已消失的回复。
func (t *transcript) handleSessionSynced(event *runtimeclient.SessionSynced, activeTurnID defs.TurnID) []string {
	// 1. 记下这次同步时根 Session 正在运行的 Turn，处理已保存 Item。
	t.activeTurnID = activeTurnID
	lines := t.handleItemsSaved(event.Items)
	// 2. 用同步结果里的生成中 Item 替换已登记的 Item，沿用已有回复的定稿进度。
	t.streamingItems = nil
	streamingItemIDs := make(map[defs.ItemID]bool, len(event.StreamingItems))
	for _, streaming := range event.StreamingItems {
		streamingItemIDs[streaming.Item.ID] = true
		lines = append(lines, t.handleItemStarted(streaming.ItemIndex, streaming.Item)...)
	}
	// 3. 丢弃既未保存也不在同步结果里的回复；已写出段落的回复补未保存提示。
	for _, id := range slices.Sorted(maps.Keys(t.replies)) {
		if streamingItemIDs[id] {
			continue
		}
		if t.replies[id].hasPrinted {
			lines = append(lines, t.formatUnsavedMarker()...)
		}
		t.replies[id].isDiscarded = true
		delete(t.replies, id)
	}
	// 4. 关闭并写出不属于正在运行的 Turn 的步骤组。
	lines = append(lines, t.flushEndedStepGroups()...)
	return lines
}

// handleItemsSaved 去重并处理已保存的 Item。
func (t *transcript) handleItemsSaved(items []runtimeprotocol.LoggedItem) []string {
	// 1. 跳过已处理流水号，更新已保存 Item 和轮次输入记录。
	var lines []string
	for _, logged := range items {
		if logged.Ordinal <= t.lastSavedOrdinal {
			continue
		}
		t.lastSavedOrdinal = logged.Ordinal
		t.savedItems[logged.Item.ID] = logged.Item
		t.recordTurnOpeningInput(logged.Item)
		// 2. 按 Item ID 移除生成中记录，再处理跨轮次步骤组和已保存内容。
		t.streamingItems = slices.DeleteFunc(t.streamingItems, func(streaming *streamingItem) bool { return streaming.item.ID == logged.Item.ID })
		lines = append(lines, t.closeStepGroupOfOtherTurn(logged.Item.TurnID)...)
		lines = append(lines, t.handleSavedItem(logged.Item)...)
	}
	return lines
}

// recordTurnOpeningInput 记录每轮模型输出之前保存的用户消息。
func (t *transcript) recordTurnOpeningInput(item *defs.Item) {
	// 1. 跳过无 Turn 的 Item，首次遇到轮次时建立输入记录。
	if item.TurnID == "" {
		return
	}
	if count := len(t.turnOpeningInputs); count == 0 || t.turnOpeningInputs[count-1].turnID != item.TurnID {
		t.turnOpeningInputs = append(t.turnOpeningInputs, turnOpeningInput{turnID: item.TurnID})
	}
	// 2. 遇到带 ModelCallID 的 Item 时标记这一轮已有模型输出；在这之前保存的用户消息记为开头输入。
	opening := &t.turnOpeningInputs[len(t.turnOpeningInputs)-1]
	switch {
	case item.ModelCallID != "":
		opening.hasModelOutput = true
	case !opening.hasModelOutput && item.Kind == defs.ItemMessage && item.Message.From != nil && item.Message.From.Kind == defs.IdentityUser:
		opening.texts = append(opening.texts, getMessageText(item.Message))
	}
}

// handleSavedItem 按种类处理已保存 Item，返回待写出的历史行。
func (t *transcript) handleSavedItem(item *defs.Item) []string {
	// 1. 消息按来源处理，工具和推理归组；单行状态提示关闭当前组，指令只登记历史。
	switch item.Kind {
	case defs.ItemMessage:
		return t.handleSavedMessage(item)
	case defs.ItemToolCall:
		return t.addToolCall(item, time.Time{})
	case defs.ItemToolResult:
		return t.setToolResult(item)
	case defs.ItemReasoning:
		return t.addReasoning(item, time.Time{})
	case defs.ItemContextSummary:
		return append(t.closeStepGroup(), t.formatHistoryLine(item.TurnID, styledText{
			style: t.styles.faint,
			text:  "≡ 上下文已压缩",
		})...)
	case defs.ItemModelError:
		return append(t.closeStepGroup(), t.formatHistoryLine(item.TurnID, styledText{
			style: t.styles.failure,
			text:  "✗ 模型错误：" + item.ModelError.Message,
		})...)
	case defs.ItemSkillInstruction, defs.ItemAgentsInstruction:
		lines := t.closeStepGroup()
		t.history = append(t.history, historyEntry{
			kind:        historyInstruction,
			instruction: item,
		})
		return lines
	case defs.ItemHookRun:
		return t.handleSavedHookRun(item)
	case defs.ItemModelChange:
		return append(t.closeStepGroup(), t.formatHistoryLine(item.TurnID, styledText{
			style: t.styles.faint,
			text:  "已切换到 " + t.getModelName(item.ModelChange.Model),
		})...)
	case defs.ItemTurnInterrupted:
		return append(t.closeStepGroup(), t.formatHistoryLine(item.TurnID, styledText{
			style: t.styles.faint,
			text:  "已中断",
		})...)
	case defs.ItemExecutorsChange:
		return append(t.closeStepGroup(), t.formatHistoryLine(item.TurnID, styledText{
			style: t.styles.faint,
			text:  formatExecutorsChange(item.ExecutorsChange, t.isLocalServer()),
		})...)
	}
	return nil
}

// handleSavedHookRun 登记已保存的 hook 运行，并写出需要显示的提示。
func (t *transcript) handleSavedHookRun(item *defs.Item) []string {
	// 1. 建立 hook 历史记录，按 CallID 补上工具名。
	entry := historyEntry{
		kind:    historyHookRun,
		hookRun: item.HookRun,
	}
	if call, found := t.toolCalls[item.HookRun.CallID]; found {
		entry.hookToolName = call.call.Name
	}
	// 2. 无主视图提示时只登记历史和分隔线；有提示时先关闭步骤组再写出。
	notices := getHookRunNotices(entry.hookRun, entry.hookToolName, t.styles)
	if len(notices) == 0 {
		lines := t.formatTurnDivider(item.TurnID)
		t.history = append(t.history, entry)
		return lines
	}
	lines := append(t.closeStepGroup(), t.formatTurnDivider(item.TurnID)...)
	t.history = append(t.history, entry)
	return append(lines, t.formatHookRunNotices(entry.hookRun, entry.hookToolName)...)
}

// formatHookRunNotices 将 hook 提示逐条渲染为历史内容块。
func (t *transcript) formatHookRunNotices(run *defs.HookRun, toolName string) []string {
	// 1. 按宽度截断各条提示，每条末尾补空行。
	var lines []string
	for _, notice := range getHookRunNotices(run, toolName, t.styles) {
		lines = append(lines, formatBlock([]string{truncateLine(formatStyledTexts([]styledText{notice}), t.renderer.width)})...)
	}
	return lines
}

// handleSavedMessage 按来源写出用户输入、模型回复或 Agent 消息。
func (t *transcript) handleSavedMessage(item *defs.Item) []string {
	// 1. 用户输入登记历史并整段写出，模型回复补写尾部，其他消息转为来源提示。
	message := item.Message
	switch {
	case message.From != nil && message.From.Kind == defs.IdentityUser:
		text := getMessageText(message)
		t.userInputs = append(t.userInputs, text)
		lines := append(t.closeStepGroup(), t.formatTurnDivider(item.TurnID)...)
		t.history = append(t.history, historyEntry{
			kind:      historyUserInput,
			userInput: text,
		})
		return append(lines, formatBlock(t.formatUserInput(text))...)
	case isReplyItem(item):
		return t.handleSavedReply(item)
	}
	return t.handleAgentMessage(item)
}

// handleSavedReply 写出已保存回复的剩余内容。
func (t *transcript) handleSavedReply(item *defs.Item) []string {
	// 1. 取得回复进度并从生成中回复移除，按完整原文定稿剩余部分。
	text := getMessageText(item.Message)
	reply, lines := t.loadReplyForText(item, text)
	delete(t.replies, item.ID)
	reply.markdown = newMarkdownStream(text, reply.markdown.getFinalizedLength())
	reply.setSource(text)
	return append(lines, t.formatReplyChunk(reply, reply.markdown.takeRest())...)
}

// handleAgentMessage 写出其他 Session 的消息，并记录子 Agent 最近结果。
func (t *transcript) handleAgentMessage(item *defs.Item) []string {
	// 1. 读取来源 Session 的名字和正文，普通消息只写正文第一行。
	message := item.Message
	var source defs.SessionID
	if message.From != nil {
		source = defs.SessionID(message.From.ID)
	}
	name, text := t.statuses.getSessionName(source), getMessageText(message)
	notification := message.SubagentNotification
	if notification == nil {
		return append(t.closeStepGroup(), t.formatHistoryLine(item.TurnID, styledText{
			style: t.styles.faint,
			text:  "← " + name + "：" + getFirstLine(text),
		})...)
	}
	// 2. 保存子 Agent 完整结果，通知行只给状态词加状态颜色。
	t.subagentResults[source] = text
	status, state := formatSubagentStatus(notification.Status)
	texts := []styledText{{
		style: t.styles.faint,
		text:  "← 子 Agent " + name + " ",
	}, {
		style: getStateStyle(state, t.styles),
		text:  status,
	}}
	if first := getFirstLine(text); first != "" {
		texts = append(texts, styledText{
			style: t.styles.faint,
			text:  " " + first,
		})
	}
	return append(t.closeStepGroup(), t.formatHistoryLine(item.TurnID, texts...)...)
}

// handleItemChanged 按开始、增量、完成或中断事件更新生成中内容。
func (t *transcript) handleItemChanged(event *runtimeclient.ItemChanged) []string {
	// 1. 按事件类型处理 Item；中断且无保存内容时丢弃生成中记录。
	switch event.Type {
	case agent.AgentEventItemStarted:
		return t.handleItemStarted(event.ItemIndex, event.Item)
	case agent.AgentEventItemDelta:
		return t.handleItemDelta(event.ItemIndex, event.Delta)
	case agent.AgentEventItemCompleted:
		return t.handleItemFinished(event.ItemIndex, event.Item)
	case agent.AgentEventItemInterrupted:
		if event.Item == nil {
			return t.handleItemDiscarded(event.ItemIndex)
		}
		return t.handleItemFinished(event.ItemIndex, event.Item)
	}
	return nil
}

// handleItemStarted 登记生成中 Item，并输出已经稳定的回复内容。
func (t *transcript) handleItemStarted(itemIndex int, item *defs.Item) []string {
	// 1. 保存 Item 位置，关闭其他 Turn 的步骤组。
	t.setStreamingItem(itemIndex, item)
	lines := t.closeStepGroupOfOtherTurn(item.TurnID)
	// 2. 工具与推理归组；回复沿用已定稿长度并写出稳定块。
	switch {
	case item.Kind == defs.ItemToolCall:
		return append(lines, t.addToolCall(item, t.now())...)
	case item.Kind == defs.ItemToolResult:
		return append(lines, t.setToolResult(item)...)
	case item.Kind == defs.ItemReasoning:
		return append(lines, t.addReasoning(item, t.now())...)
	case isReplyItem(item):
		text := getMessageText(item.Message)
		reply, replyLines := t.loadReplyForText(item, text)
		lines = append(lines, replyLines...)
		reply.markdown = newMarkdownStream(text, reply.markdown.getFinalizedLength())
		reply.setSource(text)
		return append(lines, t.takeStableReply(reply)...)
	}
	return lines
}

// handleItemDelta 追加推理或回复增量，并定稿稳定的回复块。
func (t *transcript) handleItemDelta(itemIndex int, delta *agent.AgentItemDelta) []string {
	// 1. 查找生成中 Item；缺 Item 或增量时不处理。
	streaming := t.getStreamingItem(itemIndex)
	if streaming == nil || delta == nil {
		return nil
	}
	// 2. 推理增量追加到组内；其他非回复 Item 不显示增量。
	if streaming.item.Kind == defs.ItemReasoning {
		t.appendReasoningDelta(streaming.item.ID, delta)
		return nil
	}
	if !isReplyItem(streaming.item) {
		return nil
	}
	// 3. 追加回复原文和 Markdown 增量，写出稳定块。
	reply, lines := t.loadReplyForText(streaming.item, delta.Text)
	reply.markdown.appendText(delta.Text)
	reply.source.WriteString(delta.Text)
	return append(lines, t.takeStableReply(reply)...)
}

// handleItemFinished 处理生成结束的 Item，并定稿回复剩余内容。
func (t *transcript) handleItemFinished(itemIndex int, item *defs.Item) []string {
	// 1. 移除生成中位置，更新工具、结果或完整推理。
	t.removeStreamingItem(itemIndex)
	switch {
	case item.Kind == defs.ItemToolCall:
		return t.addToolCall(item, t.now())
	case item.Kind == defs.ItemToolResult:
		return t.setToolResult(item)
	case item.Kind == defs.ItemReasoning:
		return t.addReasoning(item, t.now())
	case !isReplyItem(item):
		return nil
	}
	// 2. 用完整回复替换原文，清空实时尾部并写出剩余内容。
	text := getMessageText(item.Message)
	reply, lines := t.loadReplyForText(item, text)
	reply.markdown = newMarkdownStream(text, reply.markdown.getFinalizedLength())
	reply.setSource(text)
	reply.liveLines = nil
	return append(lines, t.formatReplyChunk(reply, reply.markdown.takeRest())...)
}

// handleItemDiscarded 移除中断且不保存的推理或回复。
func (t *transcript) handleItemDiscarded(itemIndex int) []string {
	// 1. 移除生成中 Item，推理同时从步骤组移除。
	streaming := t.removeStreamingItem(itemIndex)
	if streaming != nil && streaming.item.Kind == defs.ItemReasoning {
		t.removeReasoning(streaming.item.ID)
		return nil
	}
	if streaming == nil || !isReplyItem(streaming.item) {
		return nil
	}
	// 2. 丢弃回复进度；已写入滚动历史时补未保存提示。
	reply, found := t.replies[streaming.item.ID]
	if !found {
		return nil
	}
	delete(t.replies, streaming.item.ID)
	reply.isDiscarded = true
	if reply.hasPrinted {
		return t.formatUnsavedMarker()
	}
	return nil
}

// handleTurnStarted 记录内容流中正在执行的 Turn。
func (t *transcript) handleTurnStarted(turnID defs.TurnID) {
	// 1. 保存新开始的 Turn ID。
	t.activeTurnID = turnID
}

// handleTurnFinished 结束 Turn 的显示记录，写出步骤组和失败提示。
func (t *transcript) handleTurnFinished(event *runtimeclient.TurnFinished) []string {
	// 1. 结束的就是正在运行的 Turn 时清空它，再收尾已结束的步骤组。
	if event.TurnID == t.activeTurnID {
		t.activeTurnID = ""
	}
	lines := t.flushEndedStepGroups()
	if event.Status != defs.TurnStatusFailed {
		return lines
	}
	// 2. 失败的 Turn 追加错误行；未提供原因时显示“未知原因”。
	reason := event.Error
	if reason == "" {
		reason = "未知原因"
	}
	return append(lines, t.formatHistoryLine(event.TurnID, styledText{
		style: t.styles.failure,
		text:  "✗ Turn 失败：" + reason,
	})...)
}

// getLiveLines 返回尚未写出的步骤组和生成中回复尾部。
func (t *transcript) getLiveLines(spinner string) []string {
	// 1. 先渲染未写出的非空步骤组。
	var lines []string
	for _, group := range t.stepGroups {
		if !group.isPrinted && len(group.steps) > 0 {
			lines = append(lines, formatRunningStepGroup(group, t.now(), spinner, t.renderer.width, t.styles, t.statuses, t.getDefaultExecutorID())...)
		}
	}
	// 2. 按生成中 Item 顺序追加回复的未定稿尾部。
	for _, streaming := range t.streamingItems {
		if reply, found := t.replies[streaming.item.ID]; found {
			lines = append(lines, reply.liveLines...)
		}
	}
	return lines
}

// isReasoning 是否还有未带结束状态的推理正在生成？
func (t *transcript) isReasoning() bool {
	// 1. 查找生成中 Item 里状态为空的推理。
	return slices.ContainsFunc(t.streamingItems, func(streaming *streamingItem) bool {
		return streaming.item.Kind == defs.ItemReasoning && streaming.item.Reasoning.Status == ""
	})
}

// isItemSaved 指定 Item 是否已经保存？
func (t *transcript) isItemSaved(itemID defs.ItemID) bool {
	// 1. 按 Item ID 查询已保存记录。
	_, found := t.savedItems[itemID]
	return found
}

// getSavedItem 返回指定的已保存 Item。
func (t *transcript) getSavedItem(itemID defs.ItemID) *defs.Item {
	// 1. 按 Item ID 读取已保存记录，不存在时返回 nil。
	return t.savedItems[itemID]
}

// getUserInputs 按保存顺序返回用户输入正文的副本。
func (t *transcript) getUserInputs() []string {
	// 1. 复制已保存的用户输入列表。
	return slices.Clone(t.userInputs)
}

// getTurnOpeningInputs 按轮次顺序返回带用户开头输入的轮次。
func (t *transcript) getTurnOpeningInputs() []turnOpeningInput {
	// 1. 筛选至少记录了一条用户消息的轮次。
	var openings []turnOpeningInput
	for _, opening := range t.turnOpeningInputs {
		if len(opening.texts) > 0 {
			openings = append(openings, opening)
		}
	}
	return openings
}

// getSubagentResult 返回子 Session 最近一次结果通知的正文。
func (t *transcript) getSubagentResult(sessionID defs.SessionID) string {
	// 1. 按 Session ID 读取最近结果，没有时返回空。
	return t.subagentResults[sessionID]
}

// loadReplyForText 取得回复进度及回复前需要写出的步骤组和 Turn 分隔线。
func (t *transcript) loadReplyForText(item *defs.Item, text string) (*replyStream, []string) {
	// 1. 按 Item ID 查找回复；新回复登记进度、历史和 Turn 分隔线。
	reply, found := t.replies[item.ID]
	var divider []string
	if !found {
		divider = t.formatTurnDivider(item.TurnID)
		reply = &replyStream{markdown: newMarkdownStream("", 0)}
		t.replies[item.ID] = reply
		t.history = append(t.history, historyEntry{
			kind:    historyReply,
			replyID: item.ID,
			reply:   reply,
		})
	}
	// 2. 回复首次出现非空文字时关闭步骤组，返回的行依次是步骤组和分隔线。
	return reply, append(t.closeStepGroupForReply(reply, text), divider...)
}

// setSource 替换回复的完整原文。
func (r *replyStream) setSource(text string) {
	// 1. 清空原文缓冲，再写入新文字。
	r.source.Reset()
	r.source.WriteString(text)
}

// takeStableReply 写出回复中已稳定的块并刷新实时尾部。
func (t *transcript) takeStableReply(reply *replyStream) []string {
	// 1. 渲染并定稿稳定块，再重新渲染尚未定稿的尾部。
	lines := t.formatReplyChunk(reply, reply.markdown.takeStableBlocks())
	t.refreshLiveLines(reply)
	return lines
}

// refreshLiveLines 重新渲染回复的未定稿尾部。
func (t *transcript) refreshLiveLines(reply *replyStream) {
	// 1. 按实时行数上限渲染尾部，尚未写出段落时给首行加回复标记。
	reply.liveLines = t.renderer.render(reply.markdown.getLiveSource(t.liveHeight))
	if len(reply.liveLines) > 0 && !reply.hasPrinted {
		reply.liveLines[0] = t.markReplyStart(reply.liveLines[0])
	}
}

// formatReplyChunk 渲染一段定稿回复，并标记首个回复块。
func (t *transcript) formatReplyChunk(reply *replyStream, source string) []string {
	// 1. 渲染原文，空内容不输出。
	lines := t.renderer.render(source)
	if len(lines) == 0 {
		return nil
	}
	// 2. 首块补回复标记，登记已写出并在块后补空行。
	if !reply.hasPrinted {
		lines[0] = t.markReplyStart(lines[0])
	}
	reply.hasPrinted = true
	return formatBlock(lines)
}

// markReplyStart 在回复首行添加 ● 标记。
func (t *transcript) markReplyStart(line string) string {
	// 1. 将两格左边距替换为标记；没有左边距时直接前置标记。
	marker := t.styles.agent.Render("●") + " "
	if strings.HasPrefix(ansi.Strip(line), "  ") {
		return marker + ansi.TruncateLeft(line, 2, "")
	}
	return marker + line
}

// formatUserInput 将用户输入折行并应用用户文字样式。
func (t *transcript) formatUserInput(text string) []string {
	// 1. 按宽度生成带输入提示符的各行，再逐行加样式。
	lines := formatUserMessageLines(text, t.renderer.width)
	for index, line := range lines {
		lines[index] = t.styles.user.Render(line)
	}
	return lines
}

// formatLine 清理并截断一行文字，返回带样式的内容块。
func (t *transcript) formatLine(style lipgloss.Style, text string) []string {
	// 1. 去掉终端控制，按宽度截断后应用样式并补空行。
	return formatBlock([]string{style.Render(truncateLine(stripTerminalControls(text), t.renderer.width))})
}

// formatHistoryLine 登记单行提示并返回渲染后的历史行。
func (t *transcript) formatHistoryLine(turnID defs.TurnID, texts ...styledText) []string {
	// 1. 计算 Turn 分隔线，保存各段文字和样式。
	divider := t.formatTurnDivider(turnID)
	t.history = append(t.history, historyEntry{
		kind: historyLine,
		line: texts,
	})
	// 2. 拼接各段样式文字，截断后作为一个内容块写在分隔线后。
	return append(divider, formatBlock([]string{truncateLine(formatStyledTexts(texts), t.renderer.width)})...)
}

// formatUnsavedMarker 返回回复内容未保存的中断提示。
func (t *transcript) formatUnsavedMarker() []string {
	// 1. 以无 Turn ID 的历史提示写出“已中断 · 以上内容未保存”。
	return t.formatHistoryLine("", styledText{
		style: t.styles.faint,
		text:  "已中断 · 以上内容未保存",
	})
}

// formatTurnDivider 在首轮之后的新 Turn 前登记并返回分隔线。
func (t *transcript) formatTurnDivider(turnID defs.TurnID) []string {
	// 1. 跳过空编号和同一 Turn，更新最近登记的 Turn。
	if turnID == "" || turnID == t.lastPrintedTurnID {
		return nil
	}
	previous := t.lastPrintedTurnID
	t.lastPrintedTurnID = turnID
	// 2. 首轮不输出分隔线；其余新轮次登记满宽暗色分隔线。
	if previous == "" {
		return nil
	}
	t.history = append(t.history, historyEntry{kind: historyTurnDivider})
	return formatBlock([]string{t.styles.faint.Render(strings.Repeat("─", t.renderer.width))})
}

// getModelName 返回模型的显示名称。
func (t *transcript) getModelName(ref defs.ModelRef) string {
	// 1. 查找模型显示名，空名称改用模型引用文字。
	if name := t.modelNames[ref]; name != "" {
		return name
	}
	return ref.String()
}

// setStreamingItem 登记或替换指定位置的生成中 Item。
func (t *transcript) setStreamingItem(itemIndex int, item *defs.Item) {
	// 1. 已有位置替换 Item，否则按出现顺序追加。
	if streaming := t.getStreamingItem(itemIndex); streaming != nil {
		streaming.item = item
		return
	}
	t.streamingItems = append(t.streamingItems, &streamingItem{
		itemIndex: itemIndex,
		item:      item,
	})
}

// getStreamingItem 按本轮输出位置查找生成中 Item。
func (t *transcript) getStreamingItem(itemIndex int) *streamingItem {
	// 1. 查找匹配位置，未找到时返回 nil。
	index := slices.IndexFunc(t.streamingItems, func(streaming *streamingItem) bool { return streaming.itemIndex == itemIndex })
	if index < 0 {
		return nil
	}
	return t.streamingItems[index]
}

// removeStreamingItem 移除并返回指定位置的生成中 Item。
func (t *transcript) removeStreamingItem(itemIndex int) *streamingItem {
	// 1. 查找 Item 后从生成中列表移除；未找到时返回 nil。
	streaming := t.getStreamingItem(itemIndex)
	t.streamingItems = slices.DeleteFunc(t.streamingItems, func(each *streamingItem) bool { return each == streaming })
	return streaming
}
