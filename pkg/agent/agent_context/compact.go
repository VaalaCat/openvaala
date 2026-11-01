package agent_context

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

const (
	// compactionTriggerRatio 是触发自动压缩的用量占窗口的比例。
	compactionTriggerRatio = 0.8
	// compactionRetainRatio 是压缩时保留的最近历史占窗口的比例。
	compactionRetainRatio = 0.2
)

// GetCompactionSource 选择待压缩的历史，并返回副本。
func (c *agentContextImpl) GetCompactionSource(measurement *ContextMeasurement, isManual bool) []*defs.Item {
	// 1. 自动压缩时，窗口未知或用量没到阈值就返回 nil。
	if !isManual && (measurement.Window == 0 || measurement.RequestTokens < measurement.ThresholdTokens) {
		return nil
	}
	// 2. 选择历史前缀并克隆。
	return cloneItems(c.selectCompactPrefix(measurement, isManual))
}

// CompactHistory 生成摘要，再用摘要替换已覆盖的历史。
func (c *agentContextImpl) CompactHistory(ctx context.Context, tools []llmkit.Tool, source []*defs.Item, summary *defs.Item) (*CompactionCall, error) {
	// 1. 组装摘要请求，失败时直接返回，没有调用记录。
	req, err := c.buildCompactRequest(source, tools)
	if err != nil {
		return nil, err
	}
	// 2. 要求模型上报用量并调用摘要模型，失败时一并返回调用记录。
	req.StreamOptions = GetUsageStreamOptions(req.StreamOptions, c.opt.CompactModel.Target.Protocol)
	text, modelCallID, usage, err := c.callCompactModel(ctx, req)
	call := &CompactionCall{
		ModelCallID: modelCallID,
		Usage:       usage,
	}
	if err != nil {
		return call, err
	}
	// 3. 填写摘要内容；已取消时返回取消错误，否则把摘要加入上下文。
	summary.ModelCallID = modelCallID
	summary.ContextSummary.Text = text
	if err := ctx.Err(); err != nil {
		return call, err
	}
	if err := c.AppendItems(summary); err != nil {
		return call, err
	}
	return call, nil
}

// callCompactModel 调用摘要模型并记录遥测，返回摘要文本、调用 ID 和用量；没有上报用量时用量为 nil。
func (c *agentContextImpl) callCompactModel(ctx context.Context, req *llmkit.Request) (string, defs.ModelCallID, *defs.TokenUsage, error) {
	// 1. 创建调用 ID，开始记录摘要调用的遥测；记录内容时写入请求。
	modelCallID := defs.NewModelCallID()
	serverAddress, serverPort := GetModelServerAddress(c.opt.CompactModel.Target)
	chatCtx, recorder := telemetry.StartChat(ctx, telemetry.ChatStart{
		ModelCallID:   string(modelCallID),
		Model:         c.opt.CompactModel.Target.Model,
		Provider:      GetModelProviderName(c.opt.CompactModel.Target.Protocol),
		Purpose:       telemetry.ChatPurposeCompact,
		ServerAddress: serverAddress,
		ServerPort:    serverPort,
		Compacted:     c.IsCompacted(),
		TokenModality: GetTokenModality(req),
	})
	if recorder.IsContentCaptured() {
		recorder.RecordInput(NewTelemetryChatInput(req))
	}
	// 2. 发起模型调用并读取摘要流；发起失败时结束遥测记录并返回错误。
	callCtx, cancel := context.WithCancel(chatCtx)
	defer cancel()
	events, err := c.opt.Client.Call(callCtx, *req, *c.opt.CompactModel.Target,
		llmkit.CallOptions{Conversion: llmkit.ConversionOptions{MergeMessageRoles: c.opt.CompactModel.MergeMessageRoles}})
	if err != nil {
		recorder.End(err)
		return "", modelCallID, nil, fmt.Errorf("call compaction model: %w", err)
	}
	text, usage, err := readCompactText(callCtx, events, recorder)
	// 3. 有文本时写入遥测输出（失败时是已收到的部分文本），再结束遥测记录。
	if text != "" {
		recorder.RecordOutput([]telemetry.ChatMessage{{
			Role: telemetry.MessageRoleAssistant,
			Parts: []telemetry.MessagePart{{
				Type:    telemetry.PartTypeText,
				Content: text,
			}},
		}})
	}
	recorder.End(err)
	return text, modelCallID, usage, err
}

// selectCompactPrefix 按完整模型调用分组选择待压缩的历史前缀。
func (c *agentContextImpl) selectCompactPrefix(measurement *ContextMeasurement, isManual bool) []*defs.Item {
	// 1. 跳过开头已有的摘要，没有原文时返回 nil。
	firstOriginal := 0
	if len(c.items) > 0 && c.items[0].Kind == defs.ItemContextSummary {
		firstOriginal = 1
	}
	if firstOriginal == len(c.items) {
		return nil
	}
	// 2. 按模型调用分组：一组从起点延伸到组内各模型调用的最后一条 Item，夹在中间的 Item 也归入这组。
	modelCallEnds := make(map[defs.ModelCallID]int)
	for index := firstOriginal; index < len(c.items); index++ {
		if id := c.items[index].ModelCallID; id != "" {
			modelCallEnds[id] = index
		}
	}
	var groupStarts []int
	for start := firstOriginal; start < len(c.items); {
		groupStarts = append(groupStarts, start)
		end := start
		for index := start; index <= end; index++ {
			if id := c.items[index].ModelCallID; id != "" {
				end = max(end, modelCallEnds[id])
			}
		}
		start = end + 1
	}
	// 3. 从末尾逐组累加用量，选出达到窗口 20% 的最短后缀。
	retainBudget := int(float64(c.opt.Model.ContextWindow) * compactionRetainRatio)
	itemTokens := make(map[defs.ItemID]int, len(measurement.Items))
	for _, item := range measurement.Items {
		itemTokens[item.ItemID] = item.Tokens
	}
	retained := 0
	boundary := firstOriginal
	for group := len(groupStarts) - 1; group >= 0; group-- {
		end := len(c.items)
		if group+1 < len(groupStarts) {
			end = groupStarts[group+1]
		}
		for _, item := range c.items[groupStarts[group]:end] {
			retained += itemTokens[item.ID]
		}
		if retained >= retainBudget {
			boundary = groupStarts[group]
			break
		}
	}
	// 4. 返回后缀之前的历史；保留范围覆盖全部原文时，手动压缩返回全部历史，自动压缩返回 nil。
	if boundary == firstOriginal {
		if isManual {
			return c.items
		}
		return nil
	}
	return c.items[:boundary]
}

// buildCompactRequest 将待压缩历史转换成摘要请求，禁用工具调用并追加摘要指令。
func (c *agentContextImpl) buildCompactRequest(prefix []*defs.Item, tools []llmkit.Tool) (*llmkit.Request, error) {
	// 1. 摘要模型与本轮模型不同时，从副本中移除推理 Item。
	if c.opt.CompactModel.Ref != c.opt.Model.Ref {
		prefix = slices.DeleteFunc(slices.Clone(prefix), func(item *defs.Item) bool { return item.Kind == defs.ItemReasoning })
	}
	// 2. 按摘要模型的图片能力转换消息。
	messages, _, err := itemsToLLMKitMessages(prefix, c.opt.CompactModel.IsImageInputSupported())
	if err != nil {
		return nil, err
	}
	// 3. 填写摘要模型参数、工具和缓存键，再禁用工具调用并追加指令。
	req := newModelRequest(c.system, c.opt.SessionID, c.opt.CompactModel, c.opt.CompactModel.DefaultReasoningEffort, messages, tools)
	req.ToolChoice = &llmkit.ToolChoice{Type: "none"}
	req.Messages = append(req.Messages, llmkit.Message{
		Role: llmkit.RoleUser,
		Content: []llmkit.ContentBlock{{
			Type: llmkit.ContentTypeText,
			Text: prompt.ContextCompactionInstructions,
		}},
	})
	return req, nil
}

// readCompactText 读取摘要事件流，汇总摘要和上报用量。
func readCompactText(ctx context.Context, events <-chan llmkit.Event, recorder *telemetry.ChatRecorder) (string, *defs.TokenUsage, error) {
	// 1. 等待摘要事件；取消或流提前关闭时返回部分文本和错误，未上报用量时用量为 nil。
	var text strings.Builder
	var usage *defs.TokenUsage
	for {
		select {
		case <-ctx.Done():
			return text.String(), usage, ctx.Err()
		case event, ok := <-events:
			if !ok {
				return text.String(), usage, fmt.Errorf("compaction stream: %w", io.ErrUnexpectedEOF)
			}
			// 2. 记录分块、响应模型和最新用量。
			if IsOutputChunk(&event) {
				recorder.RecordChunk()
			}
			if event.Model != "" {
				recorder.RecordResponseModel(event.Model)
			}
			if event.Usage != nil {
				value := NewTokenUsage(event.Usage)
				usage = &value
				recorder.RecordUsage(value)
			}
			// 3. 拼接文本；工具调用或模型报错时返回已收到的文本和错误；正常结束时结束原因不是 stop 或摘要为空也返回错误。
			switch event.Type {
			case llmkit.EventContentDelta:
				if event.Delta != nil && (event.Delta.ContentType == "" || event.Delta.ContentType == llmkit.ContentTypeText) {
					text.WriteString(event.Delta.Text)
				}
			case llmkit.EventToolCallStart, llmkit.EventToolCallArgumentsDelta, llmkit.EventToolCallEnd, llmkit.EventToolCallDelta:
				return text.String(), usage, fmt.Errorf("compaction model attempted a tool call")
			case llmkit.EventError:
				return text.String(), usage, &llmkit.Error{
					Stage:     llmkit.ErrorStageStream,
					Retryable: true,
					Cause:     fmt.Errorf("compaction model error: %+v", event.Error),
				}
			case llmkit.EventDone:
				recorder.RecordFinishReason(NewTelemetryFinishReason(event.FinishReason))
				if event.FinishReason != "stop" {
					return text.String(), usage, fmt.Errorf("compaction ended with finish reason %q", event.FinishReason)
				}
				summary := strings.TrimSpace(text.String())
				if summary == "" {
					return "", usage, fmt.Errorf("compaction returned an empty summary")
				}
				return summary, usage, nil
			}
		}
	}
}
