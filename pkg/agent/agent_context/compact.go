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

// BuildCompactionRequest 组装摘要请求，并返回对应的模型和客户端。
func (c *agentContextImpl) BuildCompactionRequest(tools []llmkit.Tool, source []*defs.Item, compactModel *defs.ModelDefinition) (request *llmkit.Request, model *defs.ModelDefinition, client llmkit.Client, err error) {
	// 1. 确定实际摘要模型；与原普通模型不同时，从副本中移除推理 Item。
	if compactModel == nil {
		compactModel = c.opt.CompactModel
	}
	if compactModel == nil || compactModel.Target == nil {
		return nil, nil, nil, fmt.Errorf("compaction requires a model with target")
	}
	if compactModel.Ref != c.opt.Model.Ref {
		source = slices.DeleteFunc(slices.Clone(source), func(item *defs.Item) bool { return item.Kind == defs.ItemReasoning })
	}
	// 2. 按摘要模型的图片能力转换消息。
	messages, _, err := itemsToLLMKitMessages(source, compactModel.IsImageInputSupported())
	if err != nil {
		return nil, nil, nil, err
	}
	// 3. 填写摘要模型参数、工具、缓存键和用量流选项，再禁用工具调用并追加指令。
	req := newModelRequest(c.system, c.opt.SessionID, compactModel, compactModel.DefaultReasoningEffort, messages, tools)
	req.ToolChoice = &llmkit.ToolChoice{Type: "none"}
	req.Messages = append(req.Messages, llmkit.Message{
		Role: llmkit.RoleUser,
		Content: []llmkit.ContentBlock{{
			Type: llmkit.ContentTypeText,
			Text: prompt.ContextCompactionInstructions,
		}},
	})
	req.StreamOptions = GetUsageStreamOptions(req.StreamOptions, compactModel.Target.Protocol)
	return req, compactModel, c.opt.Client, nil
}

// GenerateSummary 调用摘要模型并记录遥测，返回摘要文本和调用记录；未上报用量时用量为 nil。
func GenerateSummary(
	ctx context.Context,
	modelCallID defs.ModelCallID,
	client llmkit.Client,
	model *defs.ModelDefinition,
	request *llmkit.Request,
	isCompacted bool,
) (text string, call *CompactionCall, err error) {
	// 1. 记录该调用 ID 的摘要遥测；记录内容时写入请求。
	serverAddress, serverPort := GetModelServerAddress(model.Target)
	chatCtx, recorder := telemetry.StartChat(ctx, telemetry.ChatStart{
		ModelCallID:   string(modelCallID),
		Model:         model.Target.Model,
		Provider:      GetModelProviderName(model.Target.Protocol),
		Purpose:       telemetry.ChatPurposeCompact,
		ServerAddress: serverAddress,
		ServerPort:    serverPort,
		Compacted:     isCompacted,
		TokenModality: GetTokenModality(request),
	})
	if recorder.IsContentCaptured() {
		recorder.RecordInput(NewTelemetryChatInput(request))
	}
	// 2. 发起模型调用并读取摘要流；发起失败时结束遥测记录并返回调用记录与错误。
	callCtx, cancel := context.WithCancel(chatCtx)
	defer cancel()
	events, err := client.Call(callCtx, *request, *model.Target,
		llmkit.CallOptions{Conversion: llmkit.ConversionOptions{MergeMessageRoles: model.MergeMessageRoles}})
	if err != nil {
		recorder.End(err)
		return "", &CompactionCall{ModelCallID: modelCallID}, fmt.Errorf("call compaction model: %w", err)
	}
	text, usage, err := readCompactText(callCtx, events, recorder)
	// 3. 有文本时写入遥测输出（失败时是已收到的部分文本），再结束遥测记录并返回调用记录。
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
	call = &CompactionCall{
		ModelCallID: modelCallID,
		Usage:       usage,
	}
	if err != nil {
		return "", call, err
	}
	return text, call, nil
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
