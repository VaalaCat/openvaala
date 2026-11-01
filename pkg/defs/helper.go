package defs

import (
	"fmt"
	"slices"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
)

// TruncateConversationTitle 截取对话标题，最多保留 256 个 Unicode 字符。
func TruncateConversationTitle(title string) string {
	// 1. 按 Unicode 字符遍历标题，超过 256 个字符时在对应字节位置截断。
	const titleRuneLimit = 256
	count := 0
	for index := range title {
		if count == titleRuneLimit {
			return title[:index]
		}
		count++
	}
	return title
}

// IsConversationMessage 根 Session 的这条 Item 计入对话消息数吗？
func IsConversationMessage(item *Item) bool {
	// 1. 只计用户消息和带 ModelCallID 的 Agent 回复；不检查 From.ID，复制历史也计入。
	if item.Kind != ItemMessage || item.Message == nil || item.Message.From == nil {
		return false
	}
	from := item.Message.From
	return from.Kind == IdentityUser || (from.Kind == IdentityAgent && item.ModelCallID != "")
}

// toolCallFromLLMKit 检查模型工具调用并转换为 ToolCall。
func toolCallFromLLMKit(modelCall *llmkit.ToolCall) (*ToolCall, error) {
	// 1. 要求调用 ID 和工具名非空，再转换完整调用。
	if modelCall == nil || modelCall.ID == "" || modelCall.Name == "" {
		return nil, fmt.Errorf("tool call requires a call ID and name")
	}
	call := &ToolCall{}
	call.FromLLMKit(modelCall)
	return call, nil
}

// Clone 复制 Item 及全部载荷。
func (item *Item) Clone() *Item {
	// 1. nil 返回 nil，否则复制 Item 身份、时间和种类，逐项复制全部载荷。
	if item == nil {
		return nil
	}
	return &Item{
		ID:                item.ID,
		UpdatedAt:         item.UpdatedAt,
		TurnID:            item.TurnID,
		ModelCallID:       item.ModelCallID,
		Kind:              item.Kind,
		Message:           item.Message.Clone(),
		Reasoning:         item.Reasoning.Clone(),
		ToolCall:          item.ToolCall.Clone(),
		ToolResult:        item.ToolResult.Clone(),
		ModelError:        item.ModelError.Clone(),
		ContextSummary:    item.ContextSummary.Clone(),
		SessionFork:       item.SessionFork.Clone(),
		ModelChange:       item.ModelChange.Clone(),
		ExecutorsChange:   item.ExecutorsChange.Clone(),
		SkillInstruction:  item.SkillInstruction.Clone(),
		AgentsInstruction: item.AgentsInstruction.Clone(),
		HookRun:           item.HookRun.Clone(),
	}
}

// Clone 复制上下文摘要。
func (summary *ContextSummary) Clone() *ContextSummary {
	// 1. nil 返回 nil，否则复制摘要的全部字段。
	if summary == nil {
		return nil
	}
	copy := *summary
	return &copy
}

// Clone 复制消息及其内容。
func (message *Message) Clone() *Message {
	// 1. nil 返回 nil，否则复制内容列表及各项载荷，再复制地址、发送方和子 Agent 通知。
	if message == nil {
		return nil
	}
	contents := slices.Clone(message.Content)
	for index := range contents {
		contents[index] = contents[index].Clone()
	}
	return &Message{
		From:                 message.From.Clone(),
		To:                   message.To.Clone(),
		Content:              contents,
		AgentSender:          message.AgentSender.Clone(),
		SubagentNotification: message.SubagentNotification.Clone(),
	}
}

// Clone 复制消息内容。
func (content *Content) Clone() *Content {
	// 1. nil 返回 nil，否则复制内容种类及文本、图片载荷。
	if content == nil {
		return nil
	}
	return &Content{
		Kind:  content.Kind,
		Text:  content.Text.Clone(),
		Image: content.Image.Clone(),
	}
}

// Clone 复制文本内容。
func (content *TextContent) Clone() *TextContent {
	// 1. nil 返回 nil，否则复制文本和附带材料标记。
	if content == nil {
		return nil
	}
	return &TextContent{
		Text:      content.Text,
		IsContext: content.IsContext,
	}
}

// Clone 复制图片内容。
func (content *ImageContent) Clone() *ImageContent {
	// 1. nil 返回 nil，否则复制图片来源和说明。
	if content == nil {
		return nil
	}
	return &ImageContent{
		DataURL: content.DataURL,
		Alt:     content.Alt,
	}
}

// GetDataURLParts 拆分 base64 data URL，返回 MIME 类型、数据和是否匹配。
func (image *ImageContent) GetDataURLParts() (mimeType, data string, isDataURL bool) {
	// 1. 依次检查 data:、逗号、;base64 和非空 MIME 类型，不是 base64 data URL 时返回空值和 false。
	rest, found := strings.CutPrefix(image.DataURL, "data:")
	if !found {
		return "", "", false
	}
	header, data, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mimeType, found = strings.CutSuffix(header, ";base64")
	if !found || mimeType == "" {
		return "", "", false
	}
	return mimeType, data, true
}

// Clone 复制模型推理。
func (reasoning *Reasoning) Clone() *Reasoning {
	// 1. nil 返回 nil，否则复制状态和加密内容，分别复制正文、摘要和原始 JSON 切片。
	if reasoning == nil {
		return nil
	}
	return &Reasoning{
		Status:    reasoning.Status,
		Summary:   slices.Clone(reasoning.Summary),
		Content:   slices.Clone(reasoning.Content),
		Encrypted: reasoning.Encrypted,
		RawJSON:   slices.Clone(reasoning.RawJSON),
	}
}

// Clone 复制工具定义。
func (definition *ToolDefinition) Clone() *ToolDefinition {
	// 1. nil 返回 nil，否则复制工具名、描述及所属 MCP server，另行复制参数 schema。
	if definition == nil {
		return nil
	}
	return &ToolDefinition{
		Name:        definition.Name,
		Description: definition.Description,
		InputSchema: slices.Clone(definition.InputSchema),
		MCPServerID: definition.MCPServerID,
	}
}

// Clone 复制工具调用。
func (call *ToolCall) Clone() *ToolCall {
	// 1. nil 返回 nil，否则复制调用 ID、工具名、参数和状态。
	if call == nil {
		return nil
	}
	return &ToolCall{
		CallID:    call.CallID,
		Name:      call.Name,
		Arguments: call.Arguments,
		Status:    call.Status,
	}
}

// Clone 复制工具结果。
func (result *ToolResult) Clone() *ToolResult {
	// 1. nil 返回 nil，否则复制内容字节、图片列表及各张图片，再复制错误和其余字段。
	if result == nil {
		return nil
	}
	images := slices.Clone(result.Images)
	for index := range images {
		images[index] = images[index].Clone()
	}
	return &ToolResult{
		CallID:    result.CallID,
		Executor:  result.Executor,
		Content:   slices.Clone(result.Content),
		Images:    images,
		Error:     result.Error.Clone(),
		Truncated: result.Truncated,
	}
}

// Clone 复制工具错误。
func (toolError *ToolError) Clone() *ToolError {
	// 1. nil 返回 nil，否则复制工具错误码和错误消息。
	if toolError == nil {
		return nil
	}
	return &ToolError{
		Code:    toolError.Code,
		Message: toolError.Message,
	}
}

// Clone 复制模型错误。
func (modelError *ModelError) Clone() *ModelError {
	// 1. nil 返回 nil，否则复制模型错误码和错误消息。
	if modelError == nil {
		return nil
	}
	return &ModelError{
		Code:    modelError.Code,
		Message: modelError.Message,
	}
}

// validateMessageRole 检查普通消息的模型角色。
func validateMessageRole(role llmkit.Role) error {
	// 1. 接受 user、assistant、system 和 developer，其他角色返回错误。
	switch role {
	case llmkit.RoleUser, llmkit.RoleAssistant, llmkit.RoleSystem, llmkit.RoleDeveloper:
		return nil
	default:
		return fmt.Errorf("unsupported message role %q", role)
	}
}

// Clone 复制消息地址。
func (address *Address) Clone() *Address {
	// 1. nil 返回 nil，否则复制地址种类和 ID。
	if address == nil {
		return nil
	}
	return &Address{
		Kind: address.Kind,
		ID:   address.ID,
	}
}

// Clone 复制分叉标记。
func (fork *SessionFork) Clone() *SessionFork {
	// 1. nil 返回 nil，否则复制来源对话、Session 和结束序号。
	if fork == nil {
		return nil
	}
	cloned := *fork
	return &cloned
}

// Clone 复制模型切换记录。
func (change *ModelChange) Clone() *ModelChange {
	// 1. nil 返回 nil，否则复制切换前后的模型选择。
	if change == nil {
		return nil
	}
	cloned := *change
	return &cloned
}

// Clone 复制 Executor 切换记录。
func (change *ExecutorsChange) Clone() *ExecutorsChange {
	// 1. nil 返回 nil，否则分别复制切换前后的 Executor 列表。
	if change == nil {
		return nil
	}
	return &ExecutorsChange{
		PreviousExecutors: slices.Clone(change.PreviousExecutors),
		Executors:         slices.Clone(change.Executors),
	}
}

// GetDetails 按种类列出 Executor 列表的变化。
func (change *ExecutorsChange) GetDetails() ExecutorsChangeDetails {
	// 1. 按列表首项找出默认 Executor 的变化。
	var details ExecutorsChangeDetails
	if len(change.PreviousExecutors) > 0 && len(change.Executors) > 0 && change.PreviousExecutors[0].ID != change.Executors[0].ID {
		details.NewDefault = &change.Executors[0]
		details.PreviousDefaultID = change.PreviousExecutors[0].ID
	}
	// 2. 按 ID 找出已移除的 Executor。
	for _, previous := range change.PreviousExecutors {
		if !slices.ContainsFunc(change.Executors, func(current TurnExecutor) bool { return current.ID == previous.ID }) {
			details.Removed = append(details.Removed, previous)
		}
	}
	// 3. 按 ID 找出新增和目录变化，新默认及其目录不再重复列入。
	for index, current := range change.Executors {
		if details.NewDefault != nil && index == 0 {
			continue
		}
		previousIndex := slices.IndexFunc(change.PreviousExecutors, func(previous TurnExecutor) bool { return previous.ID == current.ID })
		switch {
		case previousIndex < 0:
			details.Added = append(details.Added, current)
		case change.PreviousExecutors[previousIndex].WorkspaceDirectory != current.WorkspaceDirectory:
			details.DirectoryChanges = append(details.DirectoryChanges, ExecutorDirectoryChange{
				ID:                current.ID,
				PreviousDirectory: change.PreviousExecutors[previousIndex].WorkspaceDirectory,
				Directory:         current.WorkspaceDirectory,
			})
		}
	}
	return details
}

// Clone 复制 Agent 消息发送方。
func (sender *AgentSender) Clone() *AgentSender {
	// 1. nil 返回 nil，否则复制 Agent 消息发送方的全部字段。
	if sender == nil {
		return nil
	}
	cloned := *sender
	return &cloned
}

// Clone 复制子 Agent 通知。
func (notification *SubagentNotification) Clone() *SubagentNotification {
	// 1. nil 返回 nil，否则复制子 Agent 通知的全部字段。
	if notification == nil {
		return nil
	}
	cloned := *notification
	return &cloned
}

// SelectContextHistoryItems 从正序历史里选出发给模型的上下文：最新摘要和它没覆盖的 Item。
func SelectContextHistoryItems(items []*Item) ([]*Item, error) {
	// 1. 找到最新的分叉标记和最新的摘要。
	forkIndex, summaryIndex := -1, -1
	for i, item := range items {
		switch item.Kind {
		case ItemSessionFork:
			forkIndex = i
		case ItemContextSummary:
			summaryIndex = i
		}
	}
	// 2. 往前找摘要覆盖到的最后一条 Item，从它之后开始取；分叉标记之前继承来的摘要找不到时从摘要之后开始取，
	// 本 Session 的摘要找不到时返回错误。
	start := 0
	var result []*Item
	if summaryIndex >= 0 {
		summary := items[summaryIndex]
		boundary := -1
		for i := summaryIndex - 1; i >= 0; i-- {
			if items[i].ID == summary.ContextSummary.CoveredThroughItemID && items[i].Kind != ItemSessionFork && items[i].Kind != ItemContextSummary {
				boundary = i
				break
			}
		}
		if boundary < 0 {
			if forkIndex < summaryIndex {
				return nil, fmt.Errorf("context summary %q has missing boundary %q", summary.ID, summary.ContextSummary.CoveredThroughItemID)
			}
			start = summaryIndex + 1
		} else {
			start = boundary + 1
		}
		result = append(result, summary.Clone())
	}
	// 3. 收集失败的模型调用，复制起点之后的 Item，跳过分叉标记、其他摘要和失败模型调用的全部 Item。
	failedModelCalls := make(map[ModelCallID]bool)
	for _, item := range items {
		if item.Kind == ItemModelError {
			failedModelCalls[item.ModelCallID] = true
		}
	}
	for _, item := range items[start:] {
		if item.Kind != ItemSessionFork && item.Kind != ItemContextSummary && !failedModelCalls[item.ModelCallID] {
			result = append(result, item.Clone())
		}
	}
	// 4. 去掉最近一次模型切换之前的推理。
	return dropReasoningBeforeModelChange(result), nil
}

// dropReasoningBeforeModelChange 原地移除最新模型切换之前的推理 Item。
func dropReasoningBeforeModelChange(items []*Item) []*Item {
	// 1. 定位最新模型切换，没有切换时返回原切片。
	changeIndex := -1
	for index, item := range items {
		if item.Kind == ItemModelChange {
			changeIndex = index
		}
	}
	if changeIndex < 0 {
		return items
	}
	// 2. 保留其余 Item，复用输入切片。
	kept := items[:0]
	for index, item := range items {
		if index < changeIndex && item.Kind == ItemReasoning {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// Clone 复制 Skill 指令。
func (instruction *SkillInstruction) Clone() *SkillInstruction {
	// 1. nil 返回 nil，否则复制 Skill 指令的全部字段。
	if instruction == nil {
		return nil
	}
	copy := *instruction
	return &copy
}

// Clone 复制 AGENTS.md 指令。
func (instruction *AgentsInstruction) Clone() *AgentsInstruction {
	// 1. nil 返回 nil，否则复制 AGENTS.md 指令的全部字段。
	if instruction == nil {
		return nil
	}
	copy := *instruction
	return &copy
}
