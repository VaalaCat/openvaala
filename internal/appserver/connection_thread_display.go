package appserver

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
)

// connectionThread 的显示部分：Run 处理事件时，按显示记录给客户端补发 Item 还没发出的开始、增量和完成通知。

// displaySavedItem 登记已保存 Item，补发客户端尚未显示的内容。
func (t *connectionThread) displaySavedItem(ctx context.Context, logged runtimeprotocol.LoggedItem) {
	// 1. 更新已显示流水号，查找或创建 Item 的显示记录。
	t.lastDisplayedOrdinal = logged.Ordinal
	display := t.displayedItems[logged.Item.ID]
	if display == nil {
		display = &threadItemDisplay{}
		t.displayedItems[logged.Item.ID] = display
	}
	// 2. 标记已保存，用保存内容更新生成中 Item 并补发显示。
	display.isSaved = true
	if display.streamingItem != nil {
		display.streamingItem = logged.Item.Clone()
	}
	t.displayItem(ctx, logged.Item, display, true)
}

// ensureStreamingDisplay 取得本轮指定位置的显示记录。
func (t *connectionThread) ensureStreamingDisplay(itemIndex int) *threadItemDisplay {
	// 1. 查询位置，缺少时创建并登记显示记录。
	display := t.streamingItems[itemIndex]
	if display == nil {
		display = &threadItemDisplay{}
		t.streamingItems[itemIndex] = display
	}
	return display
}

// displayItem 按 Item 类型补发开始、增量和完成通知。
func (t *connectionThread) displayItem(ctx context.Context, item *defs.Item, display *threadItemDisplay, isFinished bool) {
	// 1. 先完成本轮中已有后续输出的回复和推理。
	t.completeContinuedReplies(ctx, item, display)
	// 2. 登记工具调用，参数收齐时发送开始，中断或结果到达时发送完成。
	switch item.Kind {
	case defs.ItemToolCall:
		display.itemID = item.ID
		t.toolCallsByCallID[item.ToolCall.CallID] = item
		switch item.ToolCall.Status {
		case defs.ToolCallReady:
			t.startItem(ctx, item.TurnID, display, t.translator.getStartedItem(item))
		case defs.ToolCallInterrupted:
			t.completeItem(ctx, item.TurnID, display, t.translator.getCompletedItem(item, nil))
		}
		return
	case defs.ItemToolResult:
		call := t.toolCallsByCallID[item.ToolResult.CallID]
		if call == nil {
			return
		}
		t.completeItem(ctx, item.TurnID, t.displayedItems[call.ID], t.translator.getCompletedItem(call, item))
		return
	}
	// 3. 转换独立 Item，跳过不显示和已完成的记录。
	threadItem := t.translator.getStandaloneItem(item)
	if threadItem == nil || display.isCompleted {
		return
	}
	// 4. 登记 Item ID，为本连接提交的用户消息填入 clientId。
	display.itemID = item.ID
	if userMessage, isUserMessage := threadItem.(*codexUserMessageItem); isUserMessage {
		if clientID, found := t.clientUserMessageIDs[item.ID]; found {
			userMessage.ClientID = &clientID
			delete(t.clientUserMessageIDs, item.ID)
		}
	}
	// 5. 发送开始和缺少的增量，回复等待 phase 确定，未完成推理登记为待完成。
	t.startItem(ctx, item.TurnID, display, getStartedThreadItem(threadItem))
	switch threadItem.(type) {
	case *codexAgentMessageItem:
		t.sendItemDeltas(ctx, item.TurnID, display, getMissingText(getMessageText(item.Message), display.textBytes), "", "")
		// 本轮已发送 turn/completed 时，回复 phase 使用 final_answer。
		if display.messagePhase == "" && item.TurnID <= t.lastCompletedTurnID {
			display.messagePhase = codexMessagePhaseFinalAnswer
		}
		if display.messagePhase == "" {
			t.addOpenReply(display, item)
			return
		}
	case *codexReasoningItem:
		summary := getMissingText(strings.Join(item.Reasoning.Summary, ""), display.summaryBytes)
		content := getMissingText(strings.Join(item.Reasoning.Content, ""), display.contentBytes)
		t.sendItemDeltas(ctx, item.TurnID, display, "", summary, content)
		if !isFinished {
			t.addOpenReply(display, item)
		}
	}
	// 6. 最终内容发送完成通知，摘要另发 thread/compacted。
	if isFinished && t.completeItem(ctx, item.TurnID, display, threadItem) && item.Kind == defs.ItemContextSummary {
		t.notify(ctx, "thread/compacted", codexContextCompactedNotification{
			ThreadID: t.threadID,
			TurnID:   string(item.TurnID),
		})
	}
}

// addOpenReply 登记尚未完成的回复或推理。
func (t *connectionThread) addOpenReply(display *threadItemDisplay, item *defs.Item) {
	// 1. 跳过已登记的 Item，将显示记录和内容加入待完成列表。
	if slices.ContainsFunc(t.openReplies, func(reply openReply) bool { return reply.display.itemID == display.itemID }) {
		return
	}
	t.openReplies = append(t.openReplies, openReply{
		display: display,
		item:    item,
	})
}

// completeContinuedReplies 显示新输出前，给本轮之前已有后续输出的回复和推理发送完成通知。
func (t *connectionThread) completeContinuedReplies(ctx context.Context, item *defs.Item, display *threadItemDisplay) {
	// 1. 已经显示过的工具调用不再触发完成。
	if item.Kind == defs.ItemToolCall && display.itemID != "" {
		return
	}
	// 2. 移除已完成记录，将同轮已有后续输出的回复和推理发送完成通知；回复 phase 使用 commentary。
	t.openReplies = slices.DeleteFunc(t.openReplies, func(reply openReply) bool {
		replyItem := reply.getContent()
		if reply.display.isCompleted {
			return true
		}
		if replyItem.TurnID != item.TurnID || !isTurnContinuedAfter(replyItem, item) {
			return false
		}
		reply.display.messagePhase = codexMessagePhaseCommentary
		t.completeItem(ctx, replyItem.TurnID, reply.display, t.translator.getStandaloneItem(replyItem))
		return true
	})
}

// completeOpenReplies 给结束的 Turn 里还没完成的回复和推理发送完成通知。
func (t *connectionThread) completeOpenReplies(ctx context.Context, turnID defs.TurnID) {
	// 1. 只处理指定 Turn，将回复 phase 设为 final_answer，发送完成通知并移出列表。
	t.openReplies = slices.DeleteFunc(t.openReplies, func(reply openReply) bool {
		replyItem := reply.getContent()
		if replyItem.TurnID != turnID {
			return false
		}
		reply.display.messagePhase = codexMessagePhaseFinalAnswer
		t.completeItem(ctx, turnID, reply.display, t.translator.getStandaloneItem(replyItem))
		return true
	})
}

// startItem 首次显示 Item 时发送开始通知。
func (t *connectionThread) startItem(ctx context.Context, turnID defs.TurnID, display *threadItemDisplay, threadItem codexThreadItem) {
	// 1. 跳过不显示或已发送开始的 Item。
	if threadItem == nil || display.isStarted {
		return
	}
	// 2. 标记已开始，发送带时间的 item/started。
	display.isStarted = true
	t.notify(ctx, "item/started", codexItemStartedNotification{
		Item:        threadItem,
		ThreadID:    t.threadID,
		TurnID:      string(turnID),
		StartedAtMs: time.Now().UnixMilli(),
	})
}

// completeItem 发送 Item 的完成通知，缺少开始通知时先补发开始；返回这次是否发出了完成通知。
func (t *connectionThread) completeItem(ctx context.Context, turnID defs.TurnID, display *threadItemDisplay, threadItem codexThreadItem) bool {
	// 1. 跳过不显示或已经完成的 Item。
	if threadItem == nil || display.isCompleted {
		return false
	}
	// 2. 回复使用显示记录中的 phase，缺少开始通知时先发送开始。
	if message, isAgentMessage := threadItem.(*codexAgentMessageItem); isAgentMessage {
		message.Phase = display.messagePhase
	}
	t.startItem(ctx, turnID, display, threadItem)
	// 3. 标记已完成，发送带时间的 item/completed，返回 true。
	display.isCompleted = true
	t.notify(ctx, "item/completed", codexItemCompletedNotification{
		Item:          threadItem,
		ThreadID:      t.threadID,
		TurnID:        string(turnID),
		CompletedAtMs: time.Now().UnixMilli(),
	})
	return true
}

// sendItemDeltas 发送回复文本、推理摘要和推理正文的新增片段。
func (t *connectionThread) sendItemDeltas(ctx context.Context, turnID defs.TurnID, display *threadItemDisplay, text, summary, content string) {
	// 1. 非空回复文本累计已发字节数，发送回复增量。
	if text != "" {
		display.textBytes += len(text)
		t.notify(ctx, "item/agentMessage/delta", codexItemDeltaNotification{
			ThreadID: t.threadID,
			TurnID:   string(turnID),
			ItemID:   string(display.itemID),
			Delta:    text,
		})
	}
	// 2. 非空推理摘要累计字节数，发送 summaryIndex 为 0 的增量。
	if summary != "" {
		display.summaryBytes += len(summary)
		t.notify(ctx, "item/reasoning/summaryTextDelta", codexReasoningSummaryTextDeltaNotification{
			ThreadID: t.threadID,
			TurnID:   string(turnID),
			ItemID:   string(display.itemID),
			Delta:    summary,
		})
	}
	// 3. 非空推理正文累计字节数，发送 contentIndex 为 0 的增量。
	if content != "" {
		display.contentBytes += len(content)
		t.notify(ctx, "item/reasoning/textDelta", codexReasoningTextDeltaNotification{
			ThreadID: t.threadID,
			TurnID:   string(turnID),
			ItemID:   string(display.itemID),
			Delta:    content,
		})
	}
}
