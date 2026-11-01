package agent

import (
	"slices"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// handleMessageEvent 将文本和拒绝回复写入消息草稿，返回消息开始、增量或完成事件。
func (a *agentImpl) handleMessageEvent(event *llmkit.Event, modelCallID defs.ModelCallID) []*AgentEvent {
	// 1. 文本块结束时完成末尾消息，忽略没有文本和拒绝内容的增量。
	if event.Type == llmkit.EventContentBlockStop {
		if len(a.draftItems) > 0 {
			item := a.draftItems[len(a.draftItems)-1]
			if item.Kind == defs.ItemMessage {
				return []*AgentEvent{a.finishItemWithReason(item, AgentEventItemCompleted)}
			}
		}
		return nil
	}
	if event.Delta == nil || (event.Delta.Text == "" && event.Delta.Refusal == "") {
		return nil
	}
	// 2. 复用未结束的末尾消息，或创建消息草稿并发送开始事件。
	var events []*AgentEvent
	var messageItem *defs.Item
	if len(a.draftItems) > 0 {
		lastItem := a.draftItems[len(a.draftItems)-1]
		if _, ended := a.draftItemFinishReason[lastItem]; lastItem.Kind == defs.ItemMessage && !ended {
			messageItem = lastItem
		}
	}
	if messageItem == nil {
		messageItem = &defs.Item{
			ID:          defs.NewItemID(),
			TurnID:      a.opt.TurnID,
			ModelCallID: modelCallID,
			Kind:        defs.ItemMessage,
			Message: &defs.Message{
				From: &defs.Address{
					Kind: defs.IdentityAgent,
					ID:   string(a.opt.SessionID),
				},
				Content: []*defs.Content{
					{
						Kind: defs.ContentText,
						Text: &defs.TextContent{},
					},
				},
			},
		}
		a.appendDraftItem(messageItem)
		events = append(events, a.newItemEvent(AgentEventItemStarted, messageItem, nil))
	}
	// 3. 追加文本和拒绝内容，返回增量事件。
	delta := &AgentItemDelta{Text: event.Delta.Text + event.Delta.Refusal}
	ApplyItemDelta(messageItem, delta)
	return append(events, a.newItemEvent(AgentEventItemDelta, messageItem, delta))
}

// handleReasoningEvent 更新本次模型调用的推理草稿，返回推理开始、增量或结束事件。
func (a *agentImpl) handleReasoningEvent(event *llmkit.Event, modelCallID defs.ModelCallID) []*AgentEvent {
	// 1. 忽略不带推理的事件，从末尾查找本次调用中还没结束、也没有状态的推理 Item。
	if event.Reasoning == nil {
		return nil
	}
	var events []*AgentEvent
	var reasoningItem *defs.Item
	for i := len(a.draftItems) - 1; i >= 0; i-- {
		item := a.draftItems[i]
		_, finished := a.draftItemFinishReason[item]
		if !finished && item.ModelCallID == modelCallID && item.Kind == defs.ItemReasoning && item.Reasoning.Status == "" {
			reasoningItem = item
			break
		}
	}
	// 2. 没有匹配草稿时创建推理 Item，并返回开始事件。
	if reasoningItem == nil {
		reasoningItem = &defs.Item{
			ID:          defs.NewItemID(),
			TurnID:      a.opt.TurnID,
			ModelCallID: modelCallID,
			Kind:        defs.ItemReasoning,
			Reasoning:   &defs.Reasoning{},
		}
		a.appendDraftItem(reasoningItem)
		events = append(events, a.newItemEvent(AgentEventItemStarted, reasoningItem, nil))
	}
	// 3. 追加摘要或内容增量；推理完成事件用整段推理替换已累积的内容，状态为 completed 时记为完成，否则记为中断。
	reasoning := reasoningItem.Reasoning
	switch event.Type {
	case llmkit.EventReasoningSummaryDelta:
		delta := &AgentItemDelta{ReasoningSummary: slices.Clone(event.Reasoning.Summary)}
		ApplyItemDelta(reasoningItem, delta)
		return append(events, a.newItemEvent(AgentEventItemDelta, reasoningItem, delta))
	case llmkit.EventReasoningContentDelta:
		delta := &AgentItemDelta{ReasoningContent: slices.Clone(event.Reasoning.Content)}
		ApplyItemDelta(reasoningItem, delta)
		return append(events, a.newItemEvent(AgentEventItemDelta, reasoningItem, delta))
	case llmkit.EventReasoningDone:
		*reasoning = defs.Reasoning{
			Status:    defs.ReasoningStatus(event.ReasoningStatus),
			Summary:   slices.Clone(event.Reasoning.Summary),
			Content:   slices.Clone(event.Reasoning.Content),
			Encrypted: event.Reasoning.Encrypted,
			RawJSON:   slices.Clone(event.Reasoning.RawJSON),
		}
		if event.ReasoningStatus == llmkit.ReasoningCompleted {
			return append(events, a.finishItemWithReason(reasoningItem, AgentEventItemCompleted))
		}
		return append(events, a.finishItemWithReason(reasoningItem, AgentEventItemInterrupted))
	}
	return nil
}

// handleToolCallEvent 按 CallID 更新工具调用草稿，返回调用开始、参数增量或调用完成事件。
func (a *agentImpl) handleToolCallEvent(event *llmkit.Event, modelCallID defs.ModelCallID) []*AgentEvent {
	// 1. 读取流式调用，或从非流式响应的 ToolCallDelta 中取出完整调用；都没有时忽略。
	toolEvent := event.ToolCall
	if event.Type == llmkit.EventToolCallDelta && event.Delta != nil && event.Delta.ToolCall != nil {
		delta := event.Delta.ToolCall
		toolEvent = &llmkit.StreamingToolCall{
			CallID:    delta.ID,
			Name:      delta.Name,
			Arguments: delta.Arguments,
		}
	}
	if toolEvent == nil {
		return nil
	}
	var events []*AgentEvent
	// 2. 按 CallID 查找未结束的调用；找不到时创建草稿和开始事件。
	var callItem *defs.Item
	for _, item := range a.draftItems {
		_, finished := a.draftItemFinishReason[item]
		if !finished && item.Kind == defs.ItemToolCall && item.ToolCall.CallID == toolEvent.CallID {
			callItem = item
			break
		}
	}
	if callItem == nil {
		callItem = &defs.Item{
			ID:          defs.NewItemID(),
			TurnID:      a.opt.TurnID,
			ModelCallID: modelCallID,
			Kind:        defs.ItemToolCall,
			ToolCall: &defs.ToolCall{
				CallID: toolEvent.CallID,
				Name:   toolEvent.Name,
			},
		}
		a.appendDraftItem(callItem)
		events = append(events, a.newItemEvent(AgentEventItemStarted, callItem, nil))
	}
	// 3. 更新工具名；ToolCallEnd 和 ToolCallDelta 带完整参数，覆盖参数、标记 ready 并完成 Item；其他事件追加参数增量。
	call := callItem.ToolCall
	if toolEvent.Name != "" {
		call.Name = toolEvent.Name
	}
	if event.Type == llmkit.EventToolCallEnd || event.Type == llmkit.EventToolCallDelta {
		call.Arguments = toolEvent.Arguments
		call.Status = defs.ToolCallReady
		return append(events, a.finishItemWithReason(callItem, AgentEventItemCompleted))
	}
	if toolEvent.Arguments == "" {
		return events
	}
	delta := &AgentItemDelta{Arguments: toolEvent.Arguments}
	ApplyItemDelta(callItem, delta)
	return append(events, a.newItemEvent(AgentEventItemDelta, callItem, delta))
}

// handleModelErrorEvent 将模型错误加入草稿，返回错误 Item 的开始和完成事件。
func (a *agentImpl) handleModelErrorEvent(event *llmkit.Event, modelCallID defs.ModelCallID) []*AgentEvent {
	// 1. 读取错误信息，缺失详情时填写默认错误。
	payload := event.Error
	if payload == nil {
		payload = &llmkit.ErrorPayload{
			Code:    "model_stream_error",
			Message: "model stream reported an error without details",
		}
	}
	// 2. 创建错误 Item 并加入草稿，返回开始和完成事件。
	item := &defs.Item{
		ID:          defs.NewItemID(),
		TurnID:      a.opt.TurnID,
		ModelCallID: modelCallID,
		Kind:        defs.ItemModelError,
		ModelError: &defs.ModelError{
			Code:    payload.Code,
			Message: payload.Message,
		},
	}
	a.appendDraftItem(item)
	return []*AgentEvent{
		a.newItemEvent(AgentEventItemStarted, item, nil),
		a.finishItemWithReason(item, AgentEventItemCompleted),
	}
}
