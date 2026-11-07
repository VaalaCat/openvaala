package agent_context

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/tiktoken-go/tokenizer"
)

// AgentContext 保存一次 Turn 的模型指令和历史，由 Agent 顺序读写；传入和返回的 Item 都使用副本。
type AgentContext interface {
	// Load 检查并替换全部指令和历史；检查失败时保留原内容。
	Load(param *ContextLoadParam) error
	// AppendItems 检查并追加一批 Item，遇到摘要时裁剪已覆盖的历史；失败时保留原内容。
	AppendItems(items ...*defs.Item) error
	// BuildModelRequest 将指令、历史和工具组装成模型请求，补齐缺失的工具结果。
	// 模型不能看图时图片换成占位文字；能看图时只发送最近 20 张，更早的换成占位文字。
	BuildModelRequest(tools []llmkit.Tool) (request *llmkit.Request, inputThroughItemID defs.ItemID, err error)
	// GetMeasurement 返回本地估算、利用最近实际用量计算的预计占用和 Item 分项。
	GetMeasurement(tools []llmkit.Tool) (*ContextMeasurement, error)
	// RecordRequestUsage 替换最近完整普通调用的报告和实际思考模式。
	RecordRequestUsage(contextUsage *defs.TurnContextUsage)
	// GetCompactionSource 返回待压缩历史的副本，包含已有摘要；没有可压缩内容时返回 nil。
	// 自动压缩要求窗口已知且用量达到阈值；手动压缩不检查阈值。两者都按窗口的 20% 保留最近历史，
	// 保留范围覆盖全部原文时，自动压缩返回 nil，手动压缩返回全部历史。
	GetCompactionSource(measurement *ContextMeasurement, isManual bool) []*defs.Item
	// CompactHistory 调用摘要模型，填写 summary 并用它替换已覆盖的历史；失败时保留原上下文。
	// summary 由调用方创建，ContextSummary.CoveredThroughItemID 填 source 最后一条的 ID。
	// 发起过模型调用就返回 CompactionCall，调用失败也返回；组装请求失败、没有发起调用时返回 nil。
	CompactHistory(ctx context.Context, tools []llmkit.Tool, source []*defs.Item, summary *defs.Item) (*CompactionCall, error)
	// IsCompacted 当前历史是否以摘要开头？
	IsCompacted() bool
	// IsAgentsInstructionLoaded 当前历史里是否已有这个 Executor 下这个路径的目录规则？
	IsAgentsInstructionLoaded(executorID, path string) bool
	// IsMemoryIndexLoaded 当前历史里是否已有这个路径、这个项目的记忆目录？
	IsMemoryIndexLoaded(path, project string) bool
}

// agentContextImpl 保存模型配置、指令和本轮有序历史。
type agentContextImpl struct {
	opt *AgentContextOpt
	// system 是系统指令。
	system string
	// items 是当前历史：开头可能是最新摘要，后面是没被摘要覆盖的原文。
	items []*defs.Item
	// tokenizer 和 textTokenCounts 保存本轮固定词表及已计数文本。
	tokenizer       tokenizer.Codec
	textTokenCounts map[string]int
	// reasoningTokenCounts 保存本轮推理内容按实际请求格式计算的用量。
	reasoningTokenCounts map[string]int
	// lastReasoningContext 是最近一次完整普通响应报告的实际思考模式，没有报告时为 unknown；追加摘要时保留。
	lastReasoningContext llmkit.ReasoningContext
	// lastRequestUsage 是仍可作为数值基准的报告；nil 表示只用本地估算。
	lastRequestUsage *defs.TurnContextUsage
	// lastRequestInputCount 是原请求覆盖的历史条数，只在 lastRequestUsage 非 nil 时有意义。
	lastRequestInputCount int
	// lastRequestEstimatedTokens 是原请求按实际模式计算的本地输入量，首次测量时填写；lastRequestUsage 为 nil 时为 nil。
	lastRequestEstimatedTokens *int
}

// AgentContextOpt 指定本轮身份、模型客户端和普通模型、摘要模型的配置。
type AgentContextOpt struct {
	// SessionID 标识本轮 Session，也用作 prompt_cache_key。
	SessionID defs.SessionID
	// TurnID 是摘要 Item 所属的 Turn。
	TurnID defs.TurnID
	Client llmkit.Client
	// Model 提供普通请求参数和窗口；自动压缩预留最大输出，窗口为 0 时不自动压缩。
	Model *defs.ModelDefinition
	// ReasoningEffort 是普通请求的推理强度，可以为空。
	ReasoningEffort string
	// CompactModel 提供摘要模型和它的默认推理强度。
	CompactModel *defs.ModelDefinition
}

// NewAgentContextOpt 填写本轮身份和两个模型的配置。
func NewAgentContextOpt(sessionID defs.SessionID, turnID defs.TurnID, client llmkit.Client, model, compactModel *defs.ModelDefinition) *AgentContextOpt {
	// 1. 保存本轮身份、客户端和模型配置。
	return &AgentContextOpt{
		SessionID:    sessionID,
		TurnID:       turnID,
		Client:       client,
		Model:        model,
		CompactModel: compactModel,
	}
}

// NewAgentContext 检查配置并创建空上下文。
func NewAgentContext(opt *AgentContextOpt) (AgentContext, error) {
	// 1. 检查本轮身份、客户端、模型和窗口大小。
	if opt == nil || opt.SessionID == "" || opt.TurnID == "" || opt.Client == nil || opt.Model == nil || opt.CompactModel == nil || opt.CompactModel.Target == nil {
		return nil, errors.New("agent context requires a session ID, turn ID, model client, model and compact model with target")
	}
	if opt.Model.ContextWindow < 0 {
		return nil, errors.New("context window must not be negative")
	}
	// 2. 创建本轮离线 tokenizer，保存配置并返回空上下文。
	codec, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return nil, fmt.Errorf("create context tokenizer: %w", err)
	}
	return &agentContextImpl{
		opt:                  opt,
		tokenizer:            codec,
		textTokenCounts:      make(map[string]int),
		reasoningTokenCounts: make(map[string]int),
	}, nil
}

// Load 检查传入历史，再替换指令和 Item。
func (c *agentContextImpl) Load(param *ContextLoadParam) error {
	// 1. 检查参数和历史能否转换为模型消息。
	if param == nil {
		return errors.New("context load parameters are required")
	}
	if _, _, err := itemsToLLMKitMessages(param.Items, true); err != nil {
		return err
	}
	// 2. 克隆历史并替换指令。
	c.items = cloneItems(param.Items)
	c.system = param.System
	clear(c.textTokenCounts)
	clear(c.reasoningTokenCounts)
	c.RecordRequestUsage(param.ContextUsage)
	return nil
}

// AppendItems 追加一批 Item，并应用摘要覆盖范围。
func (c *agentContextImpl) AppendItems(items ...*defs.Item) error {
	// 1. 检查本批所有 Item。
	for index, item := range items {
		if err := validateContextItem(item); err != nil {
			return fmt.Errorf("append context item %d: %w", index, err)
		}
	}
	// 2. 在候选历史中追加副本，遇到摘要时裁剪已覆盖内容。
	candidate := c.items
	for _, item := range items {
		if item.Kind != defs.ItemContextSummary {
			candidate = append(candidate, item.Clone())
			continue
		}
		compacted, err := defs.SelectContextHistoryItems(append(candidate, item))
		if err != nil {
			return err
		}
		candidate = compacted
	}
	// 3. 全部追加成功后替换历史；摘要替换历史时数值基准失效，实际思考模式保留。
	c.items = candidate
	for _, item := range items {
		if item.Kind == defs.ItemContextSummary {
			c.invalidateLastRequestUsage()
			clear(c.textTokenCounts)
			clear(c.reasoningTokenCounts)
			break
		}
	}
	return nil
}

// BuildModelRequest 把当前指令、历史和工具转换为普通模型请求。
func (c *agentContextImpl) BuildModelRequest(tools []llmkit.Tool) (*llmkit.Request, defs.ItemID, error) {
	// 1. 按模型的图片能力转换历史消息。
	messages, _, err := itemsToLLMKitMessages(c.items, c.opt.Model.IsImageInputSupported())
	if err != nil {
		return nil, "", err
	}
	// 2. 填写模型参数、指令和工具，返回本次输入最后一条 Item。
	var inputThroughItemID defs.ItemID
	if len(c.items) > 0 {
		inputThroughItemID = c.items[len(c.items)-1].ID
	}
	return newModelRequest(c.system, c.opt.SessionID, c.opt.Model, c.opt.ReasoningEffort, messages, tools), inputThroughItemID, nil
}

// GetMeasurement 转换当前请求，汇总本地估算并按最近输入用量校正。
func (c *agentContextImpl) GetMeasurement(tools []llmkit.Tool) (*ContextMeasurement, error) {
	// 1. 沿用普通请求的历史转换，获得每条 Item 对应的消息片段。
	messages, itemMessages, err := itemsToLLMKitMessages(c.items, c.opt.Model.IsImageInputSupported())
	if err != nil {
		return nil, err
	}
	measurement := &ContextMeasurement{
		Model:           c.opt.Model.Ref,
		Window:          c.opt.Model.ContextWindow,
		ThresholdTokens: GetThresholdTokens(c.opt.Model.ContextWindow, c.opt.Model.MaxOutputTokens),
	}
	excludedReasoning := getExcludedReasoning(messages, c.lastReasoningContext)
	var protocol llmkit.Protocol
	if c.opt.Model.Target != nil {
		protocol = c.opt.Model.Target.Protocol
	}
	// 2. 估算系统消息和工具定义。
	if c.system != "" {
		measurement.SystemTokens, err = c.getMessageTokens(&llmkit.Message{
			Role:    llmkit.RoleSystem,
			Content: []llmkit.ContentBlock{{Type: llmkit.ContentTypeText, Text: c.system}},
		}, nil)
		if err != nil {
			return nil, err
		}
	}
	if len(tools) > 0 {
		toolsJSON, err := json.Marshal(tools)
		if err != nil {
			return nil, fmt.Errorf("encode context tools: %w", err)
		}
		measurement.ToolTokens, err = c.getTextTokens(string(toolsJSON))
		if err != nil {
			return nil, fmt.Errorf("estimate context tool tokens: %w", err)
		}
	}
	measurement.EstimatedTokens = measurement.SystemTokens + measurement.ToolTokens
	// 3. 累加实际消息片段，单独收集报告对应的模型输出；工具结果及补齐片段仍本地计数。
	outputEstimatedTokens := 0
	hasOutput, isOutputComplete := false, true
	hasIncludedReasoning, hasExcludedReasoning := false, false
	sentImages := getSentImages(c.items, c.opt.Model.IsImageInputSupported())
	for index, messages := range itemMessages {
		item := c.items[index]
		isReportedOutput := c.lastRequestUsage != nil && index >= c.lastRequestInputCount &&
			item.ModelCallID == c.lastRequestUsage.ModelCallID && isModelOutputItem(item)
		if isReportedOutput {
			hasOutput = true
			if len(messages) == 0 {
				if item.Kind == defs.ItemReasoning {
					hasExcludedReasoning = true
				} else {
					isOutputComplete = false
				}
			}
			if item.Message != nil && slices.ContainsFunc(item.Message.Content, func(content *defs.Content) bool {
				return isImageContent(content) && !sentImages[content.Image]
			}) {
				isOutputComplete = false
			}
		}
		if len(messages) == 0 {
			continue
		}
		tokens := 0
		for _, message := range messages {
			count, err := c.getMessageTokens(message, excludedReasoning)
			if err != nil {
				return nil, err
			}
			tokens += count
			if isReportedOutput && (message.Role == llmkit.RoleAssistant || message.Role == "") {
				outputEstimatedTokens += count
				for _, block := range message.Content {
					if block.Type != llmkit.ContentTypeThinking {
						continue
					}
					if excludedReasoning[block.Reasoning] {
						hasExcludedReasoning = true
					} else {
						isSent, err := isReasoningSentToModel(block, protocol)
						if err != nil {
							return nil, err
						}
						hasIncludedReasoning = hasIncludedReasoning || isSent
						hasExcludedReasoning = hasExcludedReasoning || !isSent
					}
				}
			}
		}
		measurement.Items = append(measurement.Items, ItemTokens{
			ItemID:   item.ID,
			TurnID:   item.TurnID,
			Category: GetContextCategory(item),
			Tokens:   tokens,
		})
		measurement.EstimatedTokens += tokens
	}
	// 4. 没有可用的数值基准时仅返回本地计数。
	measurement.RequestTokens = measurement.EstimatedTokens
	if c.lastRequestUsage == nil {
		return measurement, nil
	}
	// 5. 首次测量时计算原输入的本地占用；失败时数值基准失效。
	if c.lastRequestEstimatedTokens == nil {
		inputTokens, err := c.getLastRequestInputTokens(measurement.SystemTokens, measurement.ToolTokens, c.lastReasoningContext)
		if err != nil {
			c.invalidateLastRequestUsage()
			telemetry.GetLogger(context.Background()).Warn("context usage restore skipped", "reason", "input_tokens_failed", "error", err)
			return measurement, nil
		}
		c.lastRequestEstimatedTokens = &inputTokens
	}
	// 6. 完整保留的输出优先采用实际用量；需要扣思考但细分不可用或只保留部分输出时局部估算。
	usage := c.lastRequestUsage.Usage
	outputTokens := outputEstimatedTokens
	// 上报了思考消耗但没有可回传思考时，这部分也不属于下一请求的生成输出。
	if !hasIncludedReasoning && usage.Reasoning > 0 {
		hasExcludedReasoning = true
	}
	if hasOutput && isOutputComplete && usage.Output > 0 {
		if !hasExcludedReasoning {
			outputTokens = usage.Output
		} else if !hasIncludedReasoning && usage.Reasoning > 0 && usage.Reasoning <= usage.Output {
			outputTokens = usage.Output - usage.Reasoning
		}
	}
	requestTokens := usage.Input + measurement.EstimatedTokens - *c.lastRequestEstimatedTokens - outputEstimatedTokens + outputTokens
	if requestTokens < 0 {
		telemetry.GetLogger(context.Background()).Warn("context usage restore skipped", "reason", "negative_adjusted_tokens", "model_call_id", c.lastRequestUsage.ModelCallID)
		c.invalidateLastRequestUsage()
		return measurement, nil
	}
	measurement.RequestTokens = requestTokens
	measurement.IsUsageBased = true
	return measurement, nil
}

// RecordRequestUsage 替换最近完整普通响应的实际思考模式和数值基准，定位原请求覆盖的历史。
func (c *agentContextImpl) RecordRequestUsage(contextUsage *defs.TurnContextUsage) {
	// 1. 替换实际思考模式并清除上一份报告的数值基准；没有报告时模式为 unknown。
	c.lastReasoningContext = llmkit.ReasoningContextUnknown
	c.invalidateLastRequestUsage()
	if contextUsage == nil {
		return
	}
	c.lastReasoningContext = contextUsage.ReasoningContext
	// 2. 报告没有可用输入量时，只保留实际思考模式。
	if contextUsage.Usage.Input <= 0 {
		return
	}
	// 3. 原输入不存在或已被较晚摘要替换时，只保留实际思考模式；否则记下原请求覆盖的历史条数。
	index := slices.IndexFunc(c.items, func(item *defs.Item) bool { return item.ID == contextUsage.InputThroughItemID })
	reason := ""
	switch {
	case contextUsage.ModelCallID == "":
		reason = "missing_model_call_id"
	case len(c.items) > 0 && c.items[0].Kind == defs.ItemContextSummary &&
		(c.items[0].ModelCallID == "" || c.items[0].ModelCallID >= contextUsage.ModelCallID):
		reason = "history_compacted"
	case index < 0:
		reason = "input_item_missing"
	}
	if reason != "" {
		telemetry.GetLogger(context.Background()).Warn("context usage restore skipped", "reason", reason, "model_call_id", contextUsage.ModelCallID, "input_item_id", contextUsage.InputThroughItemID)
		return
	}
	c.lastRequestUsage = contextUsage
	c.lastRequestInputCount = index + 1
}

// invalidateLastRequestUsage 让上一份报告不再作为数值基准，实际思考模式保留。
func (c *agentContextImpl) invalidateLastRequestUsage() {
	// 1. 清除报告和原输入的本地占用。
	c.lastRequestUsage = nil
	c.lastRequestEstimatedTokens = nil
}

// getLastRequestInputTokens 按实际思考模式计算原请求输入的本地占用。
func (c *agentContextImpl) getLastRequestInputTokens(systemTokens, toolTokens int, reasoningContext llmkit.ReasoningContext) (inputTokens int, err error) {
	// 1. 重建原请求覆盖的历史消息，不含后来的输出和新输入。
	inputMessages, inputFragments, err := itemsToLLMKitMessages(c.items[:c.lastRequestInputCount], c.opt.Model.IsImageInputSupported())
	if err != nil {
		return 0, fmt.Errorf("convert last request input: %w", err)
	}
	// 2. 累加系统指令、工具定义和按实际思考模式保留的各消息片段。
	excludedReasoning := getExcludedReasoning(inputMessages, reasoningContext)
	inputTokens = systemTokens + toolTokens
	for _, fragments := range inputFragments {
		for _, fragment := range fragments {
			count, err := c.getMessageTokens(fragment, excludedReasoning)
			if err != nil {
				return 0, fmt.Errorf("count last request input: %w", err)
			}
			inputTokens += count
		}
	}
	return inputTokens, nil
}

// IsCompacted 当前历史是否以摘要开头？
func (c *agentContextImpl) IsCompacted() bool {
	// 1. 检查首个 Item 的类别。
	return len(c.items) > 0 && c.items[0].Kind == defs.ItemContextSummary
}

// IsAgentsInstructionLoaded 当前历史里是否已有这个 Executor 下这个路径的目录规则？
func (c *agentContextImpl) IsAgentsInstructionLoaded(executorID, path string) bool {
	// 1. 查找 Executor 和路径都匹配的规则 Item。
	for _, item := range c.items {
		if item.Kind == defs.ItemAgentsInstruction && item.AgentsInstruction != nil &&
			item.AgentsInstruction.Executor == executorID && item.AgentsInstruction.Path == path {
			return true
		}
	}
	return false
}

// IsMemoryIndexLoaded 当前历史里是否已有这个路径、这个项目的记忆目录？
func (c *agentContextImpl) IsMemoryIndexLoaded(path, project string) bool {
	// 1. 查找路径和项目都匹配的记忆目录 Item。
	for _, item := range c.items {
		if item.Kind == defs.ItemMemoryIndex && item.MemoryIndex != nil && item.MemoryIndex.Path == path && item.MemoryIndex.Project == project {
			return true
		}
	}
	return false
}

// getMessageTokens 按消息角色、文本、推理、工具调用和固定图片用量估算消息片段。
func (c *agentContextImpl) getMessageTokens(message *llmkit.Message, excludedReasoning map[*llmkit.ReasoningContent]bool) (int, error) {
	// 1. 收集模型实际接收的文字，并累计消息框架和图片用量。
	texts := []string{message.ToolCallID}
	tokens := 0
	if message.Role != "" {
		tokens = 3
		texts = append(texts, string(message.Role))
	}
	for _, block := range message.Content {
		switch block.Type {
		case llmkit.ContentTypeImage, llmkit.ContentTypeImageURL:
			tokens += 1600
		case llmkit.ContentTypeThinking:
			if excludedReasoning[block.Reasoning] {
				continue
			}
			count, err := c.getReasoningTokens(block)
			if err != nil {
				return 0, fmt.Errorf("estimate reasoning tokens: %w", err)
			}
			tokens += count
		default:
			texts = append(texts, block.Text)
		}
	}
	for _, call := range message.ToolCalls {
		texts = append(texts, call.ID, call.Name, call.Arguments)
	}
	// 2. 逐段按 o200k_base 计数，返回计数错误。
	for _, text := range texts {
		count, err := c.getTextTokens(text)
		if err != nil {
			return 0, fmt.Errorf("estimate model message tokens: %w", err)
		}
		tokens += count
	}
	return tokens, nil
}

// getReasoningTokens 按请求格式提取推理明文和密文，分别分词和按长度估算。
func (c *agentContextImpl) getReasoningTokens(block llmkit.ContentBlock) (int, error) {
	// 1. 序列化完整推理内容，复用本轮已计算结果。
	blockJSON, err := json.Marshal(struct {
		Block      llmkit.ContentBlock
		SummaryNil bool
		ContentNil bool
	}{
		Block:      block,
		SummaryNil: block.Reasoning != nil && block.Reasoning.Summary == nil,
		ContentNil: block.Reasoning != nil && block.Reasoning.Content == nil,
	})
	if err != nil {
		return 0, err
	}
	key := string(blockJSON)
	if tokens, found := c.reasoningTokenCounts[key]; found {
		return tokens, nil
	}
	var protocol llmkit.Protocol
	if c.opt.Model.Target != nil {
		protocol = c.opt.Model.Target.Protocol
	}
	// 2. 提取请求实际发送的推理明文和密文。
	texts, encrypted, err := getReasoningTokenContent(block, protocol)
	if err != nil {
		return 0, err
	}
	// 3. 明文使用统一词表；协议封装只提取原生密文，不重复计入其明文和签名。
	tokens := 0
	for _, text := range texts {
		count, err := c.getTextTokens(text)
		if err != nil {
			return 0, err
		}
		tokens += count
	}
	for _, ciphertext := range encrypted {
		for strings.HasPrefix(ciphertext, "llmkit:v1:") {
			data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ciphertext, "llmkit:v1:"))
			if err != nil {
				return 0, fmt.Errorf("decode reasoning envelope: %w", err)
			}
			var envelope struct {
				Data struct {
					EncryptedContent string `json:"encrypted_content"`
				} `json:"data"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				return 0, fmt.Errorf("decode reasoning envelope: %w", err)
			}
			ciphertext = envelope.Data.EncryptedContent
		}
		// AES-GCM + Base64 的经验估算：去掉编码膨胀及 28 字节封装，按 4 字节/token 向上取整。
		encodedBytes := len(strings.TrimRight(ciphertext, "="))
		plaintextBytes := max(0, encodedBytes*3/4-28)
		tokens += (plaintextBytes + 3) / 4
	}
	// 4. 保存完整推理内容的估算。
	c.reasoningTokenCounts[key] = tokens
	return tokens, nil
}

// getTextTokens 返回原始文本的本地 token 估算，同一 Turn 内复用已计数文本。
func (c *agentContextImpl) getTextTokens(text string) (int, error) {
	// 1. 已计数文本直接返回。
	if tokens, found := c.textTokenCounts[text]; found {
		return tokens, nil
	}
	// 2. 按最多约 4 KiB 的 UTF-8 文本段估算，复用重复片段的结果。
	const textChunkBytes = 4096
	tokens := 0
	for start := 0; start < len(text); {
		end := min(start+textChunkBytes, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end++
		}
		chunk := text[start:end]
		count, found := c.textTokenCounts[chunk]
		if !found {
			var err error
			count, err = c.tokenizer.Count(chunk)
			if err != nil {
				return 0, err
			}
			c.textTokenCounts[chunk] = count
		}
		tokens += count
		start = end
	}
	// 3. 保存整段文本的估算。
	c.textTokenCounts[text] = tokens
	return tokens, nil
}
