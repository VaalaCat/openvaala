package agent_context

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// maxRequestImages 限定每次请求最多发送最近 20 张图片，较早图片使用占位文字。
const maxRequestImages = 20

// getExcludedReasoning 标记最终消息序列中最近用户消息之前、不参与计数的思考。
func getExcludedReasoning(messages []*llmkit.Message, mode llmkit.ReasoningContext) map[*llmkit.ReasoningContent]bool {
	// 1. 只有明确的 current_turn 模式排除历史思考。
	if mode != llmkit.ReasoningContextCurrentTurn {
		return nil
	}
	lastUser := len(messages) - 1
	for lastUser >= 0 && messages[lastUser].Role != llmkit.RoleUser {
		lastUser--
	}
	if lastUser < 0 {
		return nil
	}
	// 2. 计数片段与合并消息共享转换后的思考内容，按内容身份标记，不改发给模型的消息。
	excluded := make(map[*llmkit.ReasoningContent]bool)
	for _, message := range messages[:lastUser] {
		for _, block := range message.Content {
			if block.Type == llmkit.ContentTypeThinking {
				excluded[block.Reasoning] = true
			}
		}
	}
	return excluded
}

// isModelOutputItem 这条 Item 是否属于模型生成的正文、思考或工具调用？
func isModelOutputItem(item *defs.Item) bool {
	// 1. 工具结果与其他本地输入不属于生成输出。
	return item.Kind == defs.ItemMessage || item.Kind == defs.ItemReasoning || item.Kind == defs.ItemToolCall
}

// isReasoningSentToModel 当前协议编码是否保留了这段思考的文字或密文？
func isReasoningSentToModel(block llmkit.ContentBlock, protocol llmkit.Protocol) (bool, error) {
	// 1. 沿用请求编码提取内容，只判断是否发送，不把密文经验估数当成存在标记。
	texts, encrypted, err := getReasoningTokenContent(block, protocol)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(texts, func(text string) bool { return text != "" }) ||
		slices.ContainsFunc(encrypted, func(text string) bool { return text != "" }), nil
}

// itemsToLLMKitMessages 转换有序历史，返回模型消息及按 Item 顺序排列的计数片段。
// 同次助手输出合并后，只有首片段保留角色；补齐的工具结果归入对应工具调用 Item。
func itemsToLLMKitMessages(items []*defs.Item, isImageInputSupported bool) ([]*llmkit.Message, [][]*llmkit.Message, error) {
	// 1. 选择本次发送的图片，准备消息列表和待延后发送的工具图片、hook 消息。
	sentImages := getSentImages(items, isImageInputSupported)
	messages := make([]*llmkit.Message, 0, len(items))
	itemMessages := make([][]*llmkit.Message, len(items))
	toolCallIndexes := make(map[string]int)
	// heldMessages 暂存带 CallID 的 hook 和工具图片消息，在连续工具结果之后按原顺序发送。
	var heldMessages []*llmkit.Message
	// appendMessage 在非工具结果消息之前，先追加暂存的 hook 和图片消息。
	appendMessage := func(message *llmkit.Message, index int) {
		if index >= 0 {
			itemMessages[index] = append(itemMessages[index], message)
		}
		if message.Role != llmkit.RoleTool {
			messages = append(messages, heldMessages...)
			heldMessages = nil
		}
		messages = append(messages, message)
	}
	var assistantMessage *llmkit.Message
	var assistantModelCallID defs.ModelCallID
	closedModelCalls := make(map[defs.ModelCallID]bool)
	toolCallModelCalls := make(map[string]defs.ModelCallID)
	// flushAssistant 把正在合并的助手消息追加到列表，并记下它的模型调用已经结束。
	flushAssistant := func() {
		if assistantMessage == nil {
			return
		}
		appendMessage(assistantMessage, -1)
		closedModelCalls[assistantModelCallID] = true
		assistantMessage = nil
		assistantModelCallID = ""
	}
	// 2. 逐条检查 Item；开头的摘要转换为用户消息，不在开头的摘要返回错误。
	for index, item := range items {
		if err := validateContextItem(item); err != nil {
			return nil, nil, fmt.Errorf("context item %d: %w", index, err)
		}
		if item.Kind == defs.ItemContextSummary {
			if index != 0 {
				return nil, nil, fmt.Errorf("context summary must be the first and only summary")
			}
			appendMessage(&llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{{
					Type: llmkit.ContentTypeText,
					Text: prompt.FormatContextSummary(*item.ContextSummary),
				}},
			}, index)
			continue
		}

		// 3. 识别角色：推理、工具调用和带 ModelCallID 的消息算助手输出，其他消息用用户角色，工具结果用 tool 角色；
		// 消息缺少发送方或发送方类型不支持时返回错误；跳过不发给模型的 Item，消息里不发送的图片换成占位文字。
		role := llmkit.RoleAssistant
		assistantOutput := false
		switch item.Kind {
		case defs.ItemMessage:
			if item.Message == nil {
				return nil, nil, fmt.Errorf("context item %d has no message", index)
			}
			if item.Message.From == nil {
				return nil, nil, fmt.Errorf("context item %d has no sender", index)
			}
			if item.Message.From.Kind != defs.IdentityUser && item.Message.From.Kind != defs.IdentityAgent {
				return nil, nil, fmt.Errorf("unsupported conversation sender kind %q", item.Message.From.Kind)
			}
			assistantOutput = item.ModelCallID != ""
			if !assistantOutput {
				role = llmkit.RoleUser
			}
		case defs.ItemReasoning, defs.ItemToolCall:
			assistantOutput = true
		case defs.ItemToolResult:
			role = llmkit.RoleTool
		}

		if !IsSentToModel(item) {
			continue
		}
		if item.Kind == defs.ItemMessage {
			item = omitUnsentMessageImages(item, sentImages, isImageInputSupported)
		}

		// 4. 按 ModelCallID 合并助手输出；CallID 重复或已结束的模型调用又出现时返回错误。
		if assistantOutput {
			if assistantMessage == nil || assistantModelCallID != item.ModelCallID {
				flushAssistant()
				if closedModelCalls[item.ModelCallID] {
					return nil, nil, fmt.Errorf("context item %d reopens model call %q", index, item.ModelCallID)
				}
				assistantMessage = &llmkit.Message{Role: llmkit.RoleAssistant}
				assistantModelCallID = item.ModelCallID
			}
			converted, err := item.ToLLMKit(llmkit.RoleAssistant)
			if err != nil {
				return nil, nil, fmt.Errorf("convert context item %d: %w", index, err)
			}
			if item.Kind == defs.ItemToolCall {
				callID := item.ToolCall.CallID
				if _, exists := toolCallModelCalls[callID]; exists {
					return nil, nil, fmt.Errorf("context item %d has duplicate tool call ID %q", index, callID)
				}
				toolCallModelCalls[callID] = item.ModelCallID
				toolCallIndexes[callID] = index
			}
			fragment := *converted
			if len(assistantMessage.Content) > 0 || len(assistantMessage.ToolCalls) > 0 {
				fragment.Role = ""
			}
			itemMessages[index] = append(itemMessages[index], &fragment)
			assistantMessage.Content = append(assistantMessage.Content, converted.Content...)
			assistantMessage.ToolCalls = append(assistantMessage.ToolCalls, converted.ToolCalls...)
			continue
		}

		// 5. 结束已合并的助手消息，把模型错误、变化通知、中断通知、指令和 hook 输出转换为用户消息；
		// 带 CallID 的 hook 消息暂存到连续工具结果之后。
		flushAssistant()
		if item.Kind == defs.ItemModelError {
			if item.ModelError == nil {
				return nil, nil, fmt.Errorf("context item %d has no model error", index)
			}
			appendMessage(modelErrorToMessage(item.ModelError), index)
			continue
		}
		if item.Kind == defs.ItemModelChange {
			if item.ModelChange == nil {
				return nil, nil, fmt.Errorf("context item %d has no model change", index)
			}
			appendMessage(modelChangeToMessage(item.ModelChange), index)
			continue
		}
		if item.Kind == defs.ItemTurnInterrupted {
			appendMessage(turnInterruptedToMessage(), index)
			continue
		}
		if item.Kind == defs.ItemExecutorsChange {
			if item.ExecutorsChange == nil {
				return nil, nil, fmt.Errorf("context item %d has no executors change", index)
			}
			appendMessage(executorsChangeToMessage(item.ExecutorsChange), index)
			continue
		}
		if item.Kind == defs.ItemSkillInstruction {
			if item.SkillInstruction == nil {
				return nil, nil, fmt.Errorf("context item %d has no skill instruction", index)
			}
			appendMessage(&llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{{
					Type: llmkit.ContentTypeText,
					Text: prompt.FormatSkillInstruction(*item.SkillInstruction),
				}},
			}, index)
			continue
		}
		if item.Kind == defs.ItemAgentsInstruction {
			if item.AgentsInstruction == nil {
				return nil, nil, fmt.Errorf("context item %d has no agents instruction", index)
			}
			appendMessage(&llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{{
					Type: llmkit.ContentTypeText,
					Text: prompt.FormatAgentsInstruction(*item.AgentsInstruction),
				}},
			}, index)
			continue
		}
		if item.Kind == defs.ItemClientInstruction {
			if item.ClientInstruction == nil {
				return nil, nil, fmt.Errorf("context item %d has no client instruction", index)
			}
			appendMessage(&llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{{
					Type: llmkit.ContentTypeText,
					Text: prompt.FormatClientInstruction(*item.ClientInstruction),
				}},
			}, index)
			continue
		}
		if item.Kind == defs.ItemMemoryIndex {
			if item.MemoryIndex == nil {
				return nil, nil, fmt.Errorf("context item %d has no memory index", index)
			}
			appendMessage(&llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{{
					Type: llmkit.ContentTypeText,
					Text: prompt.FormatMemoryIndex(*item.MemoryIndex),
				}},
			}, index)
			continue
		}
		if item.Kind == defs.ItemHookRun {
			if item.HookRun == nil {
				return nil, nil, fmt.Errorf("context item %d has no hook run", index)
			}
			message := &llmkit.Message{
				Role: llmkit.RoleUser,
				Content: []llmkit.ContentBlock{{
					Type: llmkit.ContentTypeText,
					Text: prompt.FormatHookRun(*item.HookRun),
				}},
			}
			if item.HookRun.CallID == "" {
				appendMessage(message, index)
			} else {
				heldMessages = append(heldMessages, message)
				itemMessages[index] = append(itemMessages[index], message)
			}
			continue
		}
		// 6. 检查工具结果对应的调用，转换消息并包装 Agent 来源信息，暂存工具图片。
		if item.Kind == defs.ItemToolResult {
			if item.ToolResult == nil {
				return nil, nil, fmt.Errorf("context item %d has no tool result", index)
			}
			modelCallID, exists := toolCallModelCalls[item.ToolResult.CallID]
			if !exists {
				return nil, nil, fmt.Errorf("context item %d has tool result for unknown call ID %q", index, item.ToolResult.CallID)
			}
			if item.ModelCallID != modelCallID {
				return nil, nil, fmt.Errorf("context item %d belongs to model call %q, want %q", index, item.ModelCallID, modelCallID)
			}
		}
		message, err := item.ToLLMKit(role)
		if err != nil {
			return nil, nil, fmt.Errorf("convert context item %d: %w", index, err)
		}
		if item.Kind == defs.ItemMessage && item.Message.SubagentNotification != nil {
			if message, err = subagentNotificationToMessage(item, message); err != nil {
				return nil, nil, fmt.Errorf("convert context item %d: %w", index, err)
			}
		} else if item.Kind == defs.ItemMessage && item.Message.AgentSender != nil {
			if message, err = agentMessageToMessage(item, message); err != nil {
				return nil, nil, fmt.Errorf("convert context item %d: %w", index, err)
			}
		}
		appendMessage(message, index)
		if item.Kind == defs.ItemToolResult && len(item.ToolResult.Images) > 0 {
			imageMessage, err := toolResultImagesToMessage(item.ToolResult, sentImages, isImageInputSupported)
			if err != nil {
				return nil, nil, fmt.Errorf("convert context item %d: %w", index, err)
			}
			heldMessages = append(heldMessages, imageMessage)
			itemMessages[index] = append(itemMessages[index], imageMessage)
		}
	}
	// 7. 追加末尾助手消息和暂存消息，再补齐缺失工具结果。
	flushAssistant()
	messages = append(messages, heldMessages...)
	completed, missingResults, err := completeToolResults(messages)
	if err != nil {
		return nil, nil, err
	}
	for _, message := range missingResults {
		index := toolCallIndexes[message.ToolCallID]
		itemMessages[index] = append(itemMessages[index], message)
	}
	return completed, itemMessages, nil
}

// getSentImages 选择本次请求发送的历史图片。
func getSentImages(items []*defs.Item, isImageInputSupported bool) map[*defs.ImageContent]bool {
	// 1. 模型不支持图片时返回空集合，否则从末尾收集消息和工具结果中的有效图片，最多 20 张。
	sentImages := make(map[*defs.ImageContent]bool)
	if !isImageInputSupported {
		return sentImages
	}
	for index := len(items) - 1; index >= 0 && len(sentImages) < maxRequestImages; index-- {
		item := items[index]
		if item == nil || !IsSentToModel(item) {
			continue
		}
		var images []*defs.ImageContent
		switch {
		case item.Kind == defs.ItemMessage && item.Message != nil:
			for _, content := range item.Message.Content {
				if isImageContent(content) {
					images = append(images, content.Image)
				}
			}
		case item.Kind == defs.ItemToolResult && item.ToolResult != nil:
			for _, image := range item.ToolResult.Images {
				if image != nil {
					images = append(images, image)
				}
			}
		}
		for imageIndex := len(images) - 1; imageIndex >= 0 && len(sentImages) < maxRequestImages; imageIndex-- {
			sentImages[images[imageIndex]] = true
		}
	}
	return sentImages
}

// omitUnsentMessageImages 将未发送的消息图片替换为占位文字。
func omitUnsentMessageImages(item *defs.Item, sentImages map[*defs.ImageContent]bool, isImageInputSupported bool) *defs.Item {
	// 1. 检查消息是否有未选中的图片，没有时返回原 Item。
	isUnsent := func(content *defs.Content) bool { return isImageContent(content) && !sentImages[content.Image] }
	if !slices.ContainsFunc(item.Message.Content, isUnsent) {
		return item
	}
	// 2. 克隆消息，再原位替换副本中未发送的图片。
	omitted := item.Clone()
	for index, content := range item.Message.Content {
		if isUnsent(content) {
			omitted.Message.Content[index] = &defs.Content{
				Kind: defs.ContentText,
				Text: &defs.TextContent{
					Text: getOmittedImageText(content.Image, isImageInputSupported),
				},
			}
		}
	}
	return omitted
}

// toolResultImagesToMessage 将工具结果图片转换为用户消息。
func toolResultImagesToMessage(result *defs.ToolResult, sentImages map[*defs.ImageContent]bool, isImageInputSupported bool) (*llmkit.Message, error) {
	// 1. 转换每张工具图片，未选中的图片替换为占位文字，无效图片返回错误，并收集图片说明。
	alts := make([]string, len(result.Images))
	imageBlocks := make([]llmkit.ContentBlock, len(result.Images))
	for index, image := range result.Images {
		block, err := (&defs.Content{
			Kind:  defs.ContentImage,
			Image: image,
		}).ToLLMKit()
		if err != nil {
			return nil, fmt.Errorf("tool result image %d: %w", index, err)
		}
		if !sentImages[image] {
			block = &llmkit.ContentBlock{
				Type: llmkit.ContentTypeText,
				Text: getOmittedImageText(image, isImageInputSupported),
			}
		}
		alts[index] = image.Alt
		imageBlocks[index] = *block
	}
	// 2. 在图片块之前加入工具调用和图片说明，返回用户消息。
	text := llmkit.ContentBlock{
		Type: llmkit.ContentTypeText,
		Text: prompt.FormatToolResultImages(result.CallID, alts),
	}
	return &llmkit.Message{
		Role:    llmkit.RoleUser,
		Content: append([]llmkit.ContentBlock{text}, imageBlocks...),
	}, nil
}

// getOmittedImageText 返回图片未发送的说明文字。
func getOmittedImageText(image *defs.ImageContent, isImageInputSupported bool) string {
	// 1. 按模型不支持图片或图片超过张数上限，返回对应说明。
	if !isImageInputSupported {
		return prompt.OmittedImageUnsupportedText
	}
	return prompt.FormatOmittedOlderImage(image.Alt, maxRequestImages)
}

// isImageContent 内容块是否带有图片数据？
func isImageContent(content *defs.Content) bool {
	// 1. 检查内容块、图片类别和图片数据。
	return content != nil && content.Kind == defs.ContentImage && content.Image != nil
}

// IsSentToModel 这条 Item 是否会进入模型请求？
func IsSentToModel(item *defs.Item) bool {
	// 1. 排除空内容 hook、中断推理及缺少 CallID、工具名或完整 JSON 参数的中断调用。
	switch {
	case item.Kind == defs.ItemHookRun && item.HookRun != nil && prompt.FormatHookRun(*item.HookRun) == "":
		return false
	case item.Kind == defs.ItemReasoning && item.Reasoning != nil && item.Reasoning.Status == defs.ReasoningInterrupted:
		return false
	case item.Kind == defs.ItemToolCall && item.ToolCall != nil && item.ToolCall.Status == defs.ToolCallInterrupted &&
		(item.ToolCall.CallID == "" || item.ToolCall.Name == "" || !json.Valid([]byte(item.ToolCall.Arguments))):
		return false
	}
	return true
}

// GetContextCategory 返回 Item 的上下文类别。
func GetContextCategory(item *defs.Item) ContextCategory {
	// 1. 按 Item 类别识别指令、摘要、推理、工具结果和通知。
	switch item.Kind {
	case defs.ItemSkillInstruction, defs.ItemAgentsInstruction, defs.ItemMemoryIndex, defs.ItemClientInstruction, defs.ItemHookRun:
		return ContextCategoryInstruction
	case defs.ItemContextSummary:
		return ContextCategorySummary
	case defs.ItemReasoning:
		return ContextCategoryReasoning
	case defs.ItemToolCall:
		return ContextCategoryToolCall
	case defs.ItemToolResult:
		return ContextCategoryToolResult
	case defs.ItemModelError:
		return ContextCategoryModelError
	case defs.ItemModelChange:
		return ContextCategoryModelChange
	case defs.ItemExecutorsChange:
		return ContextCategoryExecutorsChange
	case defs.ItemTurnInterrupted:
		return ContextCategoryTurnInterrupted
	}
	// 2. 消息带 ModelCallID 时归为本 Session 输出，其他消息按发送方区分。
	if item.ModelCallID != "" {
		return ContextCategoryAssistantText
	}
	if item.Message != nil && item.Message.From != nil && item.Message.From.Kind == defs.IdentityUser {
		return ContextCategoryUserInput
	}
	return ContextCategoryAgentMessage
}

// validateContextItem 检查单条历史的身份和内容。
func validateContextItem(item *defs.Item) error {
	// 1. 检查 Item 非空、TurnID 和摘要内容。
	if item == nil {
		return errors.New("item is nil")
	}
	if item.TurnID == "" {
		return errors.New("item has no turn ID")
	}
	if err := item.ValidateContextSummary(); err != nil {
		return err
	}
	// 2. 检查推理、工具调用、工具结果和模型错误带有模型调用 ID。
	switch item.Kind {
	case defs.ItemReasoning, defs.ItemToolCall, defs.ItemToolResult, defs.ItemModelError:
		if item.ModelCallID == "" {
			return errors.New("item has no model call ID")
		}
	}
	return nil
}

// GetThresholdTokens 计算上下文窗口的自动压缩阈值。
func GetThresholdTokens(window, maxOutputTokens int) int {
	// 1. 沿用窗口 80% 的基础阈值；窗口为 0 时返回 0。
	threshold := int(float64(window) * compactionTriggerRatio)
	// 2. 有效输出上限下同时预留最大输出空间。
	if maxOutputTokens > 0 && maxOutputTokens < window {
		threshold = min(threshold, window-maxOutputTokens)
	}
	return threshold
}

// newModelRequest 组装带指令、历史、工具和 Session 缓存键的流式请求。
func newModelRequest(system string, sessionID defs.SessionID, model *defs.ModelDefinition, reasoningEffort string,
	messages []*llmkit.Message, tools []llmkit.Tool) *llmkit.Request {
	// 1. 填写输出上限、思考开关、推理强度、思考上下文模式、工具和 Session 缓存键。
	req := &llmkit.Request{
		Stream:           true,
		MaxTokens:        model.MaxOutputTokens,
		ThinkingEnabled:  new(model.ThinkingEnabled),
		ReasoningEffort:  reasoningEffort,
		ReasoningContext: model.ReasoningContext,
		Tools:            tools,
		Messages:         make([]llmkit.Message, 0, len(messages)+2),
		Extras: map[string]any{
			"prompt_cache_key": string(sessionID),
		},
	}
	// 2. 先追加非空系统指令，再按顺序追加历史消息。
	if system != "" {
		req.Messages = append(req.Messages, llmkit.Message{
			Role: llmkit.RoleSystem,
			Content: []llmkit.ContentBlock{{
				Type: llmkit.ContentTypeText,
				Text: system,
			}},
		})
	}
	for _, message := range messages {
		req.Messages = append(req.Messages, *message)
	}
	return req
}

// GetUsageStreamOptions 按协议返回要求上报用量的流式选项。
func GetUsageStreamOptions(options map[string]any, protocol llmkit.Protocol) map[string]any {
	// 1. 非 OpenAI Chat 协议直接返回传入选项。
	if protocol != llmkit.ProtocolOpenAIChat {
		return options
	}
	// 2. 复制选项并开启 include_usage。
	merged := maps.Clone(options)
	if merged == nil {
		merged = make(map[string]any, 1)
	}
	merged["include_usage"] = true
	return merged
}

// subagentNotificationToMessage 检查子任务通知，并转换为带来源和结束状态的用户消息。
func subagentNotificationToMessage(item *defs.Item, message *llmkit.Message) (*llmkit.Message, error) {
	// 1. 检查发送方、来源 Turn 和 completed、failed、interrupted 状态。
	notification := item.Message.SubagentNotification
	validStatus := notification.Status == defs.TurnStatusCompleted ||
		notification.Status == defs.TurnStatusFailed || notification.Status == defs.TurnStatusInterrupted
	if item.ModelCallID != "" || item.Message.From.Kind != defs.IdentityAgent || notification.SourceTurnID == "" || !validStatus {
		return nil, errors.New("invalid subagent notification")
	}
	// 2. 收集消息文本并包装来源信息，替换为单个文本块。
	textParts := make([]string, len(message.Content))
	for index, block := range message.Content {
		textParts[index] = block.Text
	}
	message.Content = []llmkit.ContentBlock{{
		Type: llmkit.ContentTypeText,
		Text: prompt.FormatSubagentNotification(defs.SessionID(item.Message.From.ID), *notification, textParts),
	}}
	return message, nil
}

// agentMessageToMessage 检查其他 Agent 消息，并包装发送方和回复方式。
func agentMessageToMessage(item *defs.Item, message *llmkit.Message) (*llmkit.Message, error) {
	// 1. 检查发送方为 Agent，且关系是下级或祖先。
	sender := item.Message.AgentSender
	validRelation := sender.Relation == defs.AgentRelationChild || sender.Relation == defs.AgentRelationAncestor
	if item.Message.From.Kind != defs.IdentityAgent || !validRelation {
		return nil, errors.New("invalid agent message sender")
	}
	// 2. 收集消息文本并包装来源信息，替换为单个文本块。
	textParts := make([]string, len(message.Content))
	for index, block := range message.Content {
		textParts[index] = block.Text
	}
	message.Content = []llmkit.ContentBlock{{
		Type: llmkit.ContentTypeText,
		Text: prompt.FormatAgentMessage(defs.SessionID(item.Message.From.ID), *sender, textParts),
	}}
	return message, nil
}

// modelErrorToMessage 将模型错误转换为用户消息。
func modelErrorToMessage(modelError *defs.ModelError) *llmkit.Message {
	// 1. 格式化模型错误并放入用户文本块。
	return &llmkit.Message{
		Role: llmkit.RoleUser,
		Content: []llmkit.ContentBlock{{
			Type: llmkit.ContentTypeText,
			Text: prompt.FormatModelError(*modelError),
		}},
	}
}

// modelChangeToMessage 将模型切换记录转换为用户消息。
func modelChangeToMessage(change *defs.ModelChange) *llmkit.Message {
	// 1. 格式化前后模型信息并放入用户文本块。
	return &llmkit.Message{
		Role: llmkit.RoleUser,
		Content: []llmkit.ContentBlock{{
			Type: llmkit.ContentTypeText,
			Text: prompt.FormatModelChange(*change),
		}},
	}
}

// turnInterruptedToMessage 将上一轮中断通知转换为用户消息。
func turnInterruptedToMessage() *llmkit.Message {
	// 1. 生成中断说明并放入用户文本块。
	return &llmkit.Message{
		Role: llmkit.RoleUser,
		Content: []llmkit.ContentBlock{{
			Type: llmkit.ContentTypeText,
			Text: prompt.FormatTurnInterrupted(),
		}},
	}
}

// executorsChangeToMessage 将 Executor 列表变化转换为用户消息。
func executorsChangeToMessage(change *defs.ExecutorsChange) *llmkit.Message {
	// 1. 格式化前后 Executor 列表并放入用户文本块。
	return &llmkit.Message{
		Role: llmkit.RoleUser,
		Content: []llmkit.ContentBlock{{
			Type: llmkit.ContentTypeText,
			Text: prompt.FormatExecutorsChange(*change),
		}},
	}
}

// completeToolResults 按调用顺序补齐缺失工具结果，同时返回新补齐的消息。
func completeToolResults(messages []*llmkit.Message) ([]*llmkit.Message, []*llmkit.Message, error) {
	// 1. 记录待补结果的调用顺序，并准备追加缺失结果的操作。
	completed := make([]*llmkit.Message, 0, len(messages))
	var missingResults []*llmkit.Message
	pendingOrder := make([]string, 0)
	pending := make(map[string]bool)
	appendMissingResults := func() error {
		for _, callID := range pendingOrder {
			if !pending[callID] {
				continue
			}
			message, err := missingToolResultMessage(callID)
			if err != nil {
				return err
			}
			completed = append(completed, message)
			missingResults = append(missingResults, message)
		}
		pendingOrder = pendingOrder[:0]
		clear(pending)
		return nil
	}

	// 2. 非工具消息前补齐上一批调用结果，工具消息没有匹配的待处理 CallID 时返回错误；再登记本条消息里的工具调用。
	for _, message := range messages {
		if message.Role != llmkit.RoleTool {
			if err := appendMissingResults(); err != nil {
				return nil, nil, err
			}
		} else if !pending[message.ToolCallID] {
			return nil, nil, fmt.Errorf("tool result %q has no pending tool call", message.ToolCallID)
		}
		completed = append(completed, message)
		for _, call := range message.ToolCalls {
			if !pending[call.ID] {
				pendingOrder = append(pendingOrder, call.ID)
			}
			pending[call.ID] = true
		}
		if message.Role == llmkit.RoleTool {
			delete(pending, message.ToolCallID)
		}
	}
	// 3. 在历史末尾补齐剩余调用的结果。
	if err := appendMissingResults(); err != nil {
		return nil, nil, err
	}
	return completed, missingResults, nil
}

// missingToolResultMessage 生成带 missing_output 错误的工具结果消息。
func missingToolResultMessage(callID string) (*llmkit.Message, error) {
	// 1. 填写 CallID、空结果和缺失输出错误，再转换为工具消息。
	return (&defs.ToolResult{
		CallID:  callID,
		Content: json.RawMessage(`null`),
		Error: &defs.ToolError{
			Code:    "missing_output",
			Message: "tool result is missing",
		},
	}).ToLLMKit()
}

// cloneItems 返回切片及每条 Item 的副本。
func cloneItems(items []*defs.Item) []*defs.Item {
	// 1. 复制切片并依次克隆 Item。
	cloned := slices.Clone(items)
	for index := range cloned {
		cloned[index] = cloned[index].Clone()
	}
	return cloned
}

// NewTokenUsage 将 llmkit 用量转换为输入、输出、缓存和推理用量。
func NewTokenUsage(usage *llmkit.Usage) defs.TokenUsage {
	// 1. 缓存读取优先取 CacheReadTokens，为 0 时改用 PromptTokens 已包含的 CachedTokens。
	cacheRead := usage.CacheReadTokens
	if cacheRead == 0 {
		cacheRead = usage.CachedTokens
	}
	// 2. 输入量加上独立上报的缓存读取、写入量，填写其他用量。
	return defs.TokenUsage{
		Input:      usage.PromptTokens + usage.CacheReadTokens + usage.CacheWriteTokens,
		Output:     usage.CompletionTokens,
		CacheRead:  cacheRead,
		CacheWrite: usage.CacheWriteTokens,
		Reasoning:  usage.ReasoningTokens,
	}
}

// GetModelProviderName 按协议返回遥测服务商名称。
func GetModelProviderName(protocol llmkit.Protocol) string {
	// 1. 转换已知协议名称，网关和兼容接口同样按协议处理；空协议返回 unknown，未知协议返回原名称。
	switch protocol {
	case llmkit.ProtocolOpenAIChat, llmkit.ProtocolOpenAIResponses:
		return "openai"
	case llmkit.ProtocolClaude:
		return "anthropic"
	case llmkit.ProtocolGemini:
		return "gcp.gemini"
	case "":
		return "unknown"
	}
	return string(protocol)
}

// NewTelemetryFinishReason 将模型结束原因转换为遥测取值。
func NewTelemetryFinishReason(reason string) string {
	// 1. 将 tool_calls 改为 tool_call，其他取值原样返回。
	if reason == "tool_calls" {
		return telemetry.FinishReasonToolCall
	}
	return reason
}

// GetModelServerAddress 解析模型服务的主机名和端口。
func GetModelServerAddress(target *llmkit.Target) (string, int) {
	// 1. 解析 URL，读取主机名；解析失败或没有主机名时返回空地址。
	parsed, err := url.Parse(target.BaseURL)
	if err != nil || parsed.Hostname() == "" {
		return "", 0
	}
	// 2. 优先使用显式端口，否则 http 使用 80，其他协议使用 443。
	if port, err := strconv.Atoi(parsed.Port()); err == nil {
		return parsed.Hostname(), port
	}
	if parsed.Scheme == "http" {
		return parsed.Hostname(), 80
	}
	return parsed.Hostname(), 443
}

// GetTokenModality 返回请求在遥测里的 token 模态。
func GetTokenModality(req *llmkit.Request) string {
	// 1. 检查每个消息块，发现图片或音频时返回 unknown，其他情况返回 text。
	for _, message := range req.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case llmkit.ContentTypeImage, llmkit.ContentTypeImageURL, llmkit.ContentTypeAudio:
				return telemetry.TokenModalityUnknown
			}
		}
	}
	return telemetry.TokenModalityText
}

// IsOutputChunk 这个流事件是否属于输出分块？
func IsOutputChunk(event *llmkit.Event) bool {
	// 1. 内容和推理增量、工具调用开始、参数增量及调用完成算作分块，其他事件不计入。
	switch event.Type {
	case llmkit.EventContentDelta, llmkit.EventThinkingDelta, llmkit.EventReasoningSummaryDelta, llmkit.EventReasoningContentDelta,
		llmkit.EventToolCallStart, llmkit.EventToolCallArgumentsDelta, llmkit.EventToolCallEnd, llmkit.EventToolCallDelta:
		return true
	}
	return false
}

// NewTelemetryChatInput 将模型请求转换为遥测指令、消息和工具名列表。
func NewTelemetryChatInput(req *llmkit.Request) (systemInstructions []string, messages []telemetry.ChatMessage, toolNames []string) {
	// 1. 提取 system、developer 的文本指令，按顺序转换其余消息。
	for _, message := range req.Messages {
		if message.Role == llmkit.RoleSystem || message.Role == llmkit.RoleDeveloper {
			for _, block := range message.Content {
				if block.Text != "" {
					systemInstructions = append(systemInstructions, block.Text)
				}
			}
			continue
		}
		messages = append(messages, newTelemetryChatMessage(message))
	}
	// 2. 按请求顺序收集工具名。
	for _, tool := range req.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	return systemInstructions, messages, toolNames
}

// NewTelemetryChatOutput 将模型输出转换为遥测助手消息。
func NewTelemetryChatOutput(items []*defs.Item) []telemetry.ChatMessage {
	// 1. 按出现顺序转换消息、推理和工具调用，跳过其他 Item 和转换失败的内容。
	output := llmkit.Message{Role: llmkit.RoleAssistant}
	for _, item := range items {
		if item.Kind != defs.ItemMessage && item.Kind != defs.ItemReasoning && item.Kind != defs.ItemToolCall {
			continue
		}
		converted, err := item.ToLLMKit(llmkit.RoleAssistant)
		if err != nil {
			continue
		}
		output.Content = append(output.Content, converted.Content...)
		output.ToolCalls = append(output.ToolCalls, converted.ToolCalls...)
	}
	// 2. 转换合并后的助手消息，没有内容块时返回 nil。
	message := newTelemetryChatMessage(output)
	if len(message.Parts) == 0 {
		return nil
	}
	return []telemetry.ChatMessage{message}
}

// NewTelemetryChatMessages 将 Turn 输入转换为遥测消息。
func NewTelemetryChatMessages(items []*defs.Item) []telemetry.ChatMessage {
	// 1. 按模型历史规则转换 Item，转换失败时返回 nil。
	converted, _, err := itemsToLLMKitMessages(items, true)
	if err != nil {
		return nil
	}
	// 2. 按顺序转换模型消息。
	var messages []telemetry.ChatMessage
	for _, message := range converted {
		messages = append(messages, newTelemetryChatMessage(*message))
	}
	return messages
}

// newTelemetryChatMessage 将模型消息转换为遥测消息，保留内容块和工具调用。
func newTelemetryChatMessage(message llmkit.Message) telemetry.ChatMessage {
	// 1. 工具结果转换为 tool_call_response，并解析结果 JSON。
	if message.Role == llmkit.RoleTool {
		return telemetry.ChatMessage{
			Role: telemetry.MessageRoleTool,
			Parts: []telemetry.MessagePart{{
				Type:     telemetry.PartTypeToolCallResponse,
				ID:       message.ToolCallID,
				Response: telemetry.DecodeJSONContent([]byte(getBlocksText(message.Content))),
			}},
		}
	}
	// 2. 其他角色按顺序转换内容块，再追加工具调用。
	chatMessage := telemetry.ChatMessage{Role: string(message.Role)}
	for _, block := range message.Content {
		if part, ok := newTelemetryMessagePart(block); ok {
			chatMessage.Parts = append(chatMessage.Parts, part)
		}
	}
	for _, call := range message.ToolCalls {
		chatMessage.Parts = append(chatMessage.Parts, telemetry.MessagePart{
			Type:      telemetry.PartTypeToolCall,
			ID:        call.ID,
			Name:      call.Name,
			Arguments: telemetry.DecodeJSONContent([]byte(call.Arguments)),
		})
	}
	return chatMessage
}

// newTelemetryMessagePart 转换可记录的文本、推理和图片，返回内容及是否可记录。
func newTelemetryMessagePart(block llmkit.ContentBlock) (telemetry.MessagePart, bool) {
	// 1. 按内容类别转换；文本和推理保留原文，网络图片保留 URI，其他图片只记录类别和 MIME 类型。
	switch block.Type {
	case llmkit.ContentTypeText, llmkit.ContentTypeInputText, llmkit.ContentTypeOutputText:
		return telemetry.MessagePart{
			Type:    telemetry.PartTypeText,
			Content: block.Text,
		}, block.Text != ""
	case llmkit.ContentTypeThinking:
		text := block.Text
		if block.Reasoning != nil {
			text = getReasoningText(block.Reasoning)
		}
		return telemetry.MessagePart{
			Type:    telemetry.PartTypeReasoning,
			Content: text,
		}, text != ""
	case llmkit.ContentTypeImage, llmkit.ContentTypeImageURL:
		if strings.HasPrefix(block.MediaURL, "http://") || strings.HasPrefix(block.MediaURL, "https://") {
			return telemetry.MessagePart{
				Type:     telemetry.PartTypeURI,
				Modality: telemetry.ModalityImage,
				URI:      block.MediaURL,
				MimeType: block.MimeType,
			}, true
		}
		mimeType := block.MimeType
		if mimeType == "" {
			mimeType = getDataURLMimeType(block.MediaURL)
		}
		return telemetry.MessagePart{
			Type:     telemetry.PartTypeBlob,
			Modality: telemetry.ModalityImage,
			MimeType: mimeType,
		}, true
	}
	return telemetry.MessagePart{}, false
}

// getReasoningTokenContent 从请求编码结果提取推理明文、加密内容和包含跨协议封装的签名。
func getReasoningTokenContent(block llmkit.ContentBlock, protocol llmkit.Protocol) ([]string, []string, error) {
	var texts, encrypted []string
	// 1. 已知协议沿用发送请求的编码器；自定义客户端的其他协议按统一字段估算。
	switch protocol {
	case llmkit.ProtocolOpenAIChat, llmkit.ProtocolOpenAIResponses, llmkit.ProtocolClaude:
		encoded, err := llmkit.NewCodec().EncodeRequest(llmkit.EncodeRequestInput{
			Target: llmkit.Target{Protocol: protocol},
			Request: llmkit.Request{Messages: []llmkit.Message{{
				Role: llmkit.RoleAssistant, Content: []llmkit.ContentBlock{block},
			}}},
		})
		if err != nil {
			return nil, nil, err
		}
		var request struct {
			Messages []struct {
				ReasoningContent string          `json:"reasoning_content"`
				Content          json.RawMessage `json:"content"`
			} `json:"messages"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(encoded.Body, &request); err != nil {
			return nil, nil, err
		}
		reasoningBlocks := request.Input
		for _, message := range request.Messages {
			if protocol == llmkit.ProtocolOpenAIChat {
				texts = append(texts, message.ReasoningContent)
				continue
			}
			var blocks []json.RawMessage
			if len(message.Content) > 0 {
				if err := json.Unmarshal(message.Content, &blocks); err != nil {
					return nil, nil, err
				}
			}
			reasoningBlocks = append(reasoningBlocks, blocks...)
		}
		for _, raw := range reasoningBlocks {
			var reasoning struct {
				Type             string                  `json:"type"`
				Thinking         string                  `json:"thinking"`
				EncryptedContent string                  `json:"encrypted_content"`
				Signature        string                  `json:"signature"`
				Data             string                  `json:"data"`
				Summary          []struct{ Text string } `json:"summary"`
				Content          []struct{ Text string } `json:"content"`
			}
			if err := json.Unmarshal(raw, &reasoning); err != nil {
				return nil, nil, err
			}
			switch reasoning.Type {
			case "reasoning":
				for _, part := range reasoning.Summary {
					texts = append(texts, part.Text)
				}
				for _, part := range reasoning.Content {
					texts = append(texts, part.Text)
				}
				encrypted = append(encrypted, reasoning.EncryptedContent)
			case "thinking":
				texts = append(texts, reasoning.Thinking)
				if strings.HasPrefix(reasoning.Signature, "llmkit:v1:") {
					encrypted = append(encrypted, reasoning.Signature)
				}
			case "redacted_thinking":
				encrypted = append(encrypted, reasoning.Data)
			}
		}
	default:
		if block.Reasoning == nil {
			texts = append(texts, block.Text)
		} else {
			texts = append(texts, block.Reasoning.Summary...)
			texts = append(texts, block.Reasoning.Content...)
			encrypted = append(encrypted, block.Reasoning.Encrypted)
		}
	}
	return texts, encrypted, nil
}

// getReasoningText 提取推理的可读文本。
func getReasoningText(reasoning *llmkit.ReasoningContent) string {
	// 1. 优先使用完整推理内容，没有内容时使用摘要，多段用换行连接。
	if len(reasoning.Content) > 0 {
		return strings.Join(reasoning.Content, "\n")
	}
	return strings.Join(reasoning.Summary, "\n")
}

// getBlocksText 拼接所有内容块的文本。
func getBlocksText(blocks []llmkit.ContentBlock) string {
	// 1. 按内容块顺序追加文本。
	var text strings.Builder
	for _, block := range blocks {
		text.WriteString(block.Text)
	}
	return text.String()
}

// getDataURLMimeType 提取 data URL 声明的 MIME 类型。
func getDataURLMimeType(dataURL string) string {
	// 1. 不是 data URL 时返回空串，否则拆分头部并提取分号前的 MIME 类型，如 image/png。
	header, _, found := strings.Cut(strings.TrimPrefix(dataURL, "data:"), ",")
	if !found || !strings.HasPrefix(dataURL, "data:") {
		return ""
	}
	mimeType, _, _ := strings.Cut(header, ";")
	return mimeType
}
