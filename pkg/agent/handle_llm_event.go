package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/VaalaCat/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
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
			CallID:       delta.ID,
			Name:         delta.Name,
			Arguments:    delta.Arguments,
			ProviderData: delta.ProviderData,
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
	// 3. 更新工具名和厂商数据（事件带了就覆盖，所以 ToolCallEnd 的完整数据最终生效）；ToolCallEnd 和 ToolCallDelta 带完整参数，覆盖参数、标记 ready 并完成 Item；其他事件追加参数增量。
	call := callItem.ToolCall
	if toolEvent.Name != "" {
		call.Name = toolEvent.Name
	}
	if toolEvent.ProviderData != nil {
		call.ProviderData = defs.CloneToolCallProviderData(toolEvent.ProviderData)
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

// applyEventToDraft 按模型事件类别更新草稿，返回对应的 Agent 事件。
func (a *agentImpl) applyEventToDraft(event *llmkit.Event, modelCallID defs.ModelCallID) []*AgentEvent {
	// 1. 分发消息、推理、工具和错误事件；生成结束时完成所有未结束草稿。
	switch event.Type {
	case llmkit.EventDone:
		var events []*AgentEvent
		for _, item := range a.draftItems {
			if _, ended := a.draftItemFinishReason[item]; !ended {
				events = append(events, a.finishItemWithReason(item, AgentEventItemCompleted))
			}
		}
		return events
	case llmkit.EventContentDelta, llmkit.EventContentBlockStop:
		return a.handleMessageEvent(event, modelCallID)
	case llmkit.EventReasoningSummaryDelta,
		llmkit.EventReasoningContentDelta,
		llmkit.EventReasoningDone:
		return a.handleReasoningEvent(event, modelCallID)
	case llmkit.EventToolCallStart,
		llmkit.EventToolCallArgumentsDelta,
		llmkit.EventToolCallEnd,
		llmkit.EventToolCallDelta:
		return a.handleToolCallEvent(event, modelCallID)
	case llmkit.EventError:
		return a.handleModelErrorEvent(event, modelCallID)
	case llmkit.EventThinkingDelta, llmkit.EventSignatureDelta:
		// thinking 和签名增量不单独处理，推理内容以 Reasoning 事件为准。
	}
	return nil
}

// handleModelCallStarted 登记本次模型流的身份、输入位置和输出起点。
func (a *agentImpl) handleModelCallStarted(modelCallID defs.ModelCallID, inputThroughItemID defs.ItemID) {
	// 1. 登记流并累计一次普通模型尝试。
	a.modelStream = &agentModelStream{
		inputThroughItemID: inputThroughItemID,
		outputStart:        len(a.draftItems),
		call: AgentModelCall{
			ModelCallID: modelCallID,
			Purpose:     defs.ModelCallPurposeConversation,
		},
	}
	a.turnStats.Steps++
	a.turnStats.InferenceCalls++
	a.turnStats.FinishReason = ""
}

// handleTaskModelEvent 合并当前模型流的用量、结束原因和输出事件。
func (a *agentImpl) handleTaskModelEvent(event *llmkit.Event, modelCallResult defs.ModelCallResult) {
	// 1. 记录本次调用上报的用量和结束原因。
	stream := a.modelStream
	if event.Usage != nil {
		usage := agent_context.NewTokenUsage(event.Usage)
		stream.call.Usage = &usage
	}
	if event.Type == llmkit.EventDone {
		report := &defs.TurnContextUsage{
			ModelCallID:        stream.call.ModelCallID,
			InputThroughItemID: stream.inputThroughItemID,
			ReasoningContext:   event.ReasoningContext,
		}
		if stream.call.Usage != nil {
			report.Usage = *stream.call.Usage
		}
		stream.call.ContextUsage = report
		a.turnStats.FinishReason = agent_context.NewTelemetryFinishReason(event.FinishReason)
	}
	// 2. 发送输出事件；调用完成事实随 ModelCallFinished 交给 Turn 收尾写入。
	if event.Type == llmkit.EventDone {
		stream.call.Result = modelCallResult
	}
	a.sendEvents(a.applyEventToDraft(event, stream.call.ModelCallID))
}

// handleModelCallFinished 结算一次模型尝试，发布调用报告并把可用输出交给生成任务。
func (a *agentImpl) handleModelCallFinished(ctx context.Context, request *agentTaskModelCallFinishedRequest) agentTaskModelCallResult {
	// 1. 取出已合并的调用，结束遥测，发送原输入测量和调用报告。
	stream := a.modelStream
	a.modelStream = nil
	call := &stream.call
	modelCallID := call.ModelCallID
	output := a.draftItems[stream.outputStart:]
	if call.Usage != nil {
		a.turnStats.Usage = a.turnStats.Usage.Add(*call.Usage)
	}
	if request.CallErr != nil && a.turnStats.FinishReason == "" {
		a.turnStats.FinishReason = telemetry.FinishReasonError
	}
	if request.Recorder.IsContentCaptured() {
		request.Recorder.RecordOutput(agent_context.NewTelemetryChatOutput(output))
	}
	request.Recorder.End(request.CallErr)
	if call.ContextUsage != nil {
		contextUsage := *call.ContextUsage
		a.opt.Context.RecordRequestUsage(&contextUsage)
		if _, err := a.measureContext(a.modelTools); err != nil {
			telemetry.GetLogger(ctx).Warn("context usage measurement skipped", "error", err, "model_call_id", modelCallID)
		}
	}
	a.sendEvents([]*AgentEvent{{Type: AgentEventModelCallFinished, ModelCall: call}})
	result := agentTaskModelCallResult{ModelCallID: modelCallID}

	// 2. 失败时收尾未结束的草稿；本轮未取消且没有流错误 Item 时补一条错误记录。
	if callErr := request.CallErr; callErr != nil {
		a.sendEvents(a.finishDraftItemsWithInterrupted(output))
		if ctx.Err() == nil && !slices.ContainsFunc(output, func(item *defs.Item) bool { return item.Kind == defs.ItemModelError }) {
			payload := &llmkit.ErrorPayload{Code: "model_call_error", Message: callErr.Error()}
			if callErr == io.ErrUnexpectedEOF {
				payload.Code = "unexpected_eof"
				payload.Message = "model stream closed before completion: unexpected EOF"
			}
			a.sendEvents(a.handleModelErrorEvent(&llmkit.Event{Type: llmkit.EventError, Error: payload}, modelCallID))
		}
	}
	// 3. 取消时只保留可进入历史的输出，不派发工具；非取消的失败尝试不写入上下文。
	if err := ctx.Err(); err != nil {
		output = slices.DeleteFunc(slices.Clone(a.draftItems[stream.outputStart:]), func(item *defs.Item) bool {
			return item.Kind == defs.ItemMessage && !hasMessageText(item)
		})
		retained, selectErr := defs.SelectContextHistoryItems(output)
		if selectErr != nil {
			result.Err = errors.Join(err, selectErr)
			return result
		}
		if appendErr := a.opt.Context.AppendItems(retained...); appendErr != nil {
			result.Err = errors.Join(err, appendErr)
			return result
		}
		result.Err = err
		return result
	}
	if request.CallErr != nil {
		return result
	}
	// 4. 追加已完成的模型输出、发送用量，再把完整调用的独立副本交给生成任务。
	completed, toolCalls := a.collectCompletedDraftItems(output)
	if err := a.opt.Context.AppendItems(completed...); err != nil {
		result.Err = fmt.Errorf("append agent output: %w", err)
		return result
	}
	if _, err := a.measureContext(a.modelTools); err != nil {
		result.Err = err
		return result
	}
	if len(toolCalls) == 0 {
		result.LastAssistantMessage = getLastMessageText(completed)
		return result
	}
	a.toolCallsByCallID = make(map[string]agentToolCall, len(toolCalls))
	for _, item := range toolCalls {
		callID := item.ToolCall.CallID
		if callID == "" {
			result.Err = errors.New("tool execution requires complete tool call items")
			return result
		}
		if _, exists := a.toolCallsByCallID[callID]; exists {
			result.Err = fmt.Errorf("duplicate tool call ID %q", callID)
			return result
		}
		a.toolCallsByCallID[callID] = agentToolCall{item: item}
		result.ToolCalls = append(result.ToolCalls, item.Clone())
	}
	return result
}
