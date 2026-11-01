package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
)

// Agent 在一次 Turn 内生成模型回复、执行工具并发送输出事件。
type Agent interface {
	// Run 循环调用模型、执行工具，直到模型不再调用工具且没有新的补充输入；IsCompactOnly 时只压缩上下文。每个实例只 Run 一次。
	// events 非 nil 时调用方要一直读到通道关闭，Run 返回前关闭它；取消后仍会发完剩余事件。
	// 临时模型错误自动重试；失败尝试的输出留在草稿和事件里，只有成功调用的输出加入上下文、执行工具。
	Run(ctx context.Context, events chan<- *AgentEvent) (AgentResult, error)
}

// agentImpl 是一次 Turn 的 Agent，保存配置、本轮草稿、已读的目录规则和运行统计。
type agentImpl struct {
	opt *AgentOpt

	// agentEventChannel 是 Run 传入的事件通道；nil 时不发事件。
	agentEventChannel chan<- *AgentEvent
	// draftItems 是本轮输出草稿，按首次出现顺序排列。
	draftItems []*defs.Item
	// draftItemIndexes 是每个草稿在 draftItems 里的位置。
	draftItemIndexes map[*defs.Item]int
	// draftItemFinishReason 记录已结束草稿的结束事件类型（完成或中断）；不在表里的草稿还没结束。
	draftItemFinishReason map[*defs.Item]AgentEventType
	// workspaceAgentsInstructions 按 Executor 保存本轮读到的工作区规则；键存在就表示已经读过，值可以为空。
	workspaceAgentsInstructions map[string][]*defs.AgentsInstruction
	// turnStats 是本次运行的统计，随循环累计，Run 返回时交给调用方。
	turnStats AgentStats
}

// AgentOpt 指定本轮模型、上下文、工具、输入通道和 hook 环境。
type AgentOpt struct {
	TurnID    defs.TurnID
	SessionID defs.SessionID
	Client    llmkit.Client
	// Model 提供本轮请求目标和消息角色合并选项。
	Model *defs.ModelDefinition
	// Context 保存本轮指令和历史，组装模型请求。
	Context agent_context.AgentContext
	// ToolExecutor 列出本轮工具并执行工具调用；nil 表示没有工具。
	ToolExecutor executor.ToolExecutor
	// Environments 保存本对话的执行环境；第一个是默认环境，提供目录规则和 Stop hook 的工作区。
	Environments []executor.ExecutionEnvironment
	// InputRequests 向 Turn 请求补充输入；nil 时不读补充输入。
	InputRequests chan<- AgentInputRequest
	// IsCompactOnly 这个 Agent 是否只压缩上下文？是的话 Run 只把历史压成摘要就返回，不调用普通模型。
	IsCompactOnly bool
	// Hooks 在模型这一步没有工具调用时触发 Stop 或 SubagentStop；为 nil 或 Environments 为空时不触发。
	Hooks hook.Hooks
	// Session 是本 Agent 运行所在的 Session，随 hook 事件交给脚本；ParentSessionID 非空表示子 Session，触发 SubagentStop。
	Session defs.Session
}

// maxStopHookContinuations 是 Stop、SubagentStop 连续拦下结束时，模型最多续跑的次数。
const maxStopHookContinuations = 8

// NewAgentOpt 填写本轮身份、模型和上下文。
func NewAgentOpt(
	turnID defs.TurnID,
	sessionID defs.SessionID,
	client llmkit.Client,
	model *defs.ModelDefinition,
	agentContext agent_context.AgentContext,
) *AgentOpt {
	// 1. 保存本轮身份、客户端、模型和上下文。
	return &AgentOpt{
		TurnID:    turnID,
		SessionID: sessionID,
		Client:    client,
		Model:     model,
		Context:   agentContext,
	}
}

// NewAgent 检查依赖并创建本轮 Agent。
func NewAgent(opt *AgentOpt) (Agent, error) {
	// 1. 检查身份、模型客户端、请求目标和上下文。
	if opt == nil || opt.TurnID == "" || opt.SessionID == "" || opt.Client == nil || opt.Model == nil || opt.Model.Target == nil || opt.Context == nil {
		return nil, errors.New("agent requires a turn ID, session ID, model client, model with target and context")
	}
	// 2. 保存配置，创建草稿索引、结束状态和工作区规则表。
	return &agentImpl{
		opt:                         opt,
		draftItemIndexes:            make(map[*defs.Item]int),
		draftItemFinishReason:       make(map[*defs.Item]AgentEventType),
		workspaceAgentsInstructions: make(map[string][]*defs.AgentsInstruction),
	}, nil
}

// Run 执行本轮生成和工具调用，返回草稿、最终模型调用 ID 和统计。
func (a *agentImpl) Run(ctx context.Context, events chan<- *AgentEvent) (AgentResult, error) {
	// 1. 保存事件通道，非 nil 时在返回前关闭。
	a.agentEventChannel = events
	if events != nil {
		defer close(events)
	}
	// 2. 运行模型循环并返回本轮结果。
	finalModelCallID, err := a.callModelUntilFinished(ctx)
	return AgentResult{
		Items:            a.draftItems,
		FinalModelCallID: finalModelCallID,
		Stats:            a.turnStats,
	}, err
}

// callModelUntilFinished 运行模型和工具循环，返回最后一次成功普通模型调用的 ID。
func (a *agentImpl) callModelUntilFinished(ctx context.Context) (finalModelCallID defs.ModelCallID, runErr error) {
	// 1. 获取本轮工具，退出模型循环前发送最终上下文用量。
	tools, err := a.getTools(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		if _, err := a.measureContext(tools); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	// 2. 只压缩时手动压缩历史，然后返回。
	if a.opt.IsCompactOnly {
		_, err := a.compactContext(ctx, tools, true)
		if err != nil {
			return "", err
		}
		return "", nil
	}
	// 3. 预读默认环境的工作区规则，读取失败则结束本轮。
	if len(a.opt.Environments) > 0 {
		defaultEnvironment := a.opt.Environments[0]
		if _, err := a.getWorkspaceAgentsInstructions(ctx, defaultEnvironment); err != nil {
			return "", fmt.Errorf("load AGENTS.md from executor %q: %w", defaultEnvironment.GetID(), err)
		}
	}
	// 4. 逐步循环：每步先检查取消，再读取补充输入、追加目录规则并按需压缩。
	// isTurnContinuedByStopHook 本轮是否被 Stop、SubagentStop 续跑过？变成 true 后不再改回。
	isTurnContinuedByStopHook := false
	// stopHookContinuations 是 Stop、SubagentStop 连续续跑的次数，执行工具后清零。
	stopHookContinuations := 0
	for step := 1; ; step++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// 把本步序号挂到 ctx 的遥测属性上，模型、压缩和工具调用都会带上。
		stepCtx := telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(step))
		if _, err := a.readInputs(stepCtx, false); err != nil {
			return "", err
		}
		var instructions []*defs.AgentsInstruction
		for _, environment := range a.opt.Environments {
			instructions = append(instructions, a.workspaceAgentsInstructions[environment.GetID()]...)
		}
		if err := a.appendAgentsInstructions(instructions, ""); err != nil {
			return "", err
		}
		if _, err := a.compactContext(stepCtx, tools, false); err != nil {
			return "", err
		}
		// 5. 重新追加被摘要覆盖的目录规则，再测量本步上下文、发送用量并组装请求。
		if err := a.appendAgentsInstructions(instructions, ""); err != nil {
			return "", err
		}
		_, err := a.measureContext(tools)
		if err != nil {
			return "", err
		}
		req, inputThroughItemID, err := a.opt.Context.BuildModelRequest(tools)
		if err != nil {
			return "", fmt.Errorf("build model request: %w", err)
		}
		// 6. 调用普通模型，每次尝试都发送 ModelCallFinished；失败时把这次尝试里还没结束的草稿记为中断，临时错误按重试规则再试。
		var modelCallID defs.ModelCallID
		var modelOutputStart int
		err = failsafe.With(newModelRetryPolicy()).WithContext(stepCtx).Run(func() error {
			modelCallID = defs.NewModelCallID()
			modelOutputStart = len(a.draftItems)
			modelCall, err := a.callModel(stepCtx, req, inputThroughItemID, modelCallID, modelOutputStart)
			if modelCall.ContextUsage != nil {
				a.opt.Context.RecordRequestUsage(modelCall.ContextUsage)
				if _, measureErr := a.measureContext(tools); measureErr != nil {
					telemetry.GetLogger(stepCtx).Warn("context usage measurement skipped", "error", measureErr, "model_call_id", modelCallID)
				}
			}
			a.sendEvents([]*AgentEvent{
				{
					Type:      AgentEventModelCallFinished,
					ModelCall: modelCall,
				},
			})
			if err == nil {
				return nil
			}
			output := a.draftItems[modelOutputStart:]
			a.sendEvents(a.finishDraftItemsWithInterrupted(output))
			// 草稿里没有模型错误 Item 且 Turn 没取消时补一条；流提前关闭时错误码是 unexpected_eof。
			if stepCtx.Err() == nil && !slices.ContainsFunc(output, func(item *defs.Item) bool {
				return item.Kind == defs.ItemModelError
			}) {
				payload := &llmkit.ErrorPayload{
					Code:    "model_call_error",
					Message: err.Error(),
				}
				if err == io.ErrUnexpectedEOF {
					payload.Code = "unexpected_eof"
					payload.Message = "model stream closed before completion: unexpected EOF"
				}
				a.sendEvents(a.handleModelErrorEvent(&llmkit.Event{
					Type:  llmkit.EventError,
					Error: payload,
				}, modelCallID))
			}
			return err
		})
		if err != nil {
			// 取消后的有效输出会留在历史中，退出前将它们纳入最终测量；失败模型调用仍按历史规则排除。
			if ctx.Err() != nil {
				output := slices.DeleteFunc(slices.Clone(a.draftItems[modelOutputStart:]), func(item *defs.Item) bool {
					return item.Kind == defs.ItemMessage && !hasMessageText(item)
				})
				retained, selectErr := defs.SelectContextHistoryItems(output)
				if selectErr != nil {
					return "", errors.Join(err, selectErr)
				}
				if appendErr := a.opt.Context.AppendItems(retained...); appendErr != nil {
					return "", errors.Join(err, appendErr)
				}
			}
			return "", err
		}
		// 7. 把成功调用中正常完成的输出加入上下文。
		completedItems, toolCalls := a.collectCompletedDraftItems(a.draftItems[modelOutputStart:])
		if err := a.opt.Context.AppendItems(completedItems...); err != nil {
			return "", fmt.Errorf("append agent output: %w", err)
		}
		if _, err := a.measureContext(tools); err != nil {
			return "", err
		}
		// 8. 没有工具调用时运行 Stop hook：要求结束就返回；拦下结束时在次数上限内续跑，超过上限记警告；
		// 然后再读一次补充输入，有新输入就继续，没有就结束本轮。
		if len(toolCalls) == 0 {
			outcome, err := a.runStopHooks(stepCtx, modelCallID, completedItems, isTurnContinuedByStopHook)
			if err != nil {
				return "", err
			}
			if outcome.IsTurnStopped {
				return modelCallID, nil
			}
			if outcome.IsBlocked {
				if stopHookContinuations < maxStopHookContinuations {
					stopHookContinuations++
					isTurnContinuedByStopHook = true
					continue
				}
				telemetry.GetLogger(stepCtx).Warn("stop hook continuation limit reached", "continuations", stopHookContinuations)
			}
			hasInput, err := a.readInputs(stepCtx, true)
			if err != nil {
				return "", err
			}
			if hasInput {
				continue
			}
			return modelCallID, nil
		}
		// 9. 有工具调用时清零续跑次数并执行工具；hook 要求结束就返回，否则进入下一步。
		stopHookContinuations = 0
		isTurnStopped, err := a.execTools(stepCtx, toolCalls)
		if err != nil {
			return "", err
		}
		if isTurnStopped {
			return modelCallID, nil
		}
	}
}

// measureContext 测量本轮请求占用并发送上下文用量事件。
func (a *agentImpl) measureContext(tools []llmkit.Tool) (*agent_context.ContextMeasurement, error) {
	// 1. 计算请求占用，失败时返回错误。
	measurement, err := a.opt.Context.GetMeasurement(tools)
	if err != nil {
		return nil, fmt.Errorf("measure agent context: %w", err)
	}
	// 2. 发送完整测量结果。
	a.sendEvents([]*AgentEvent{{
		Type:        AgentEventContextMeasured,
		Measurement: measurement,
	}})
	return measurement, nil
}

// compactContext 选出待压缩的历史，调用摘要模型生成摘要 Item 并发送事件；返回是否生成了摘要。
func (a *agentImpl) compactContext(ctx context.Context, tools []llmkit.Tool, isManual bool) (bool, error) {
	// 1. 测量上下文并选出待压缩历史；没有可压缩历史时直接返回。
	measurement, err := a.opt.Context.GetMeasurement(tools)
	if err != nil {
		return false, fmt.Errorf("measure context before compaction: %w", err)
	}
	source := a.opt.Context.GetCompactionSource(measurement, isManual)
	if source == nil {
		return false, nil
	}
	// 2. 创建摘要草稿并发送开始事件。
	summary := &defs.Item{
		ID:             defs.NewItemID(),
		TurnID:         a.opt.TurnID,
		Kind:           defs.ItemContextSummary,
		ContextSummary: &defs.ContextSummary{CoveredThroughItemID: source[len(source)-1].ID},
	}
	a.appendDraftItem(summary)
	a.sendEvents([]*AgentEvent{a.newItemEvent(AgentEventItemStarted, summary, nil)})
	// 3. 调用摘要模型并重试临时错误；每次发起过的调用都累计次数和用量，并发送 ModelCallFinished。
	err = failsafe.With(newModelRetryPolicy()).WithContext(ctx).Run(func() error {
		compaction, err := a.opt.Context.CompactHistory(ctx, tools, source, summary)
		if compaction != nil {
			a.turnStats.InferenceCalls++
			if compaction.Usage != nil {
				a.turnStats.Usage = a.turnStats.Usage.Add(*compaction.Usage)
			}
			a.sendEvents([]*AgentEvent{
				{
					Type: AgentEventModelCallFinished,
					ModelCall: &AgentModelCall{
						ModelCallID: compaction.ModelCallID,
						Purpose:     ModelCallPurposeCompaction,
						Usage:       compaction.Usage,
					},
				},
			})
		}
		return err
	})
	// 4. 失败时发送不带 Item 的中断事件，再移除摘要草稿。
	if err != nil {
		a.sendEvents([]*AgentEvent{{
			Type:      AgentEventItemInterrupted,
			ItemIndex: a.draftItemIndexes[summary],
		}})
		a.draftItems = a.draftItems[:len(a.draftItems)-1]
		delete(a.draftItemIndexes, summary)
		return false, fmt.Errorf("compact agent context: %w", err)
	}
	// 5. 成功时发送摘要完成事件。
	a.sendEvents([]*AgentEvent{a.finishItemWithReason(summary, AgentEventItemCompleted)})
	return true, nil
}

// newModelRetryPolicy 创建普通模型和摘要模型共用的重试规则。
func newModelRetryPolicy() retrypolicy.RetryPolicy[any] {
	// 1. 取消和超时不重试；其他可重试错误最多重试 10 次，等待从 1 秒翻倍到 30 秒，用完次数返回最后一次的错误。
	return retrypolicy.NewBuilder[any]().
		HandleIf(func(_ any, err error) bool { return isRetryableModelError(err) }).
		AbortOnErrors(context.Canceled, context.DeadlineExceeded).
		WithMaxRetries(10).
		WithBackoff(time.Second, 30*time.Second).
		ReturnLastFailure().
		Build()
}

// isRetryableModelError 本次模型调用错误是否可以自动重试？
func isRetryableModelError(err error) bool {
	// 1. 排除空错误、取消和超时。
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 2. llmkit 错误按它的 Retryable 判断，其他错误只重试流提前关闭。
	var modelError *llmkit.Error
	if errors.As(err, &modelError) {
		return modelError.Retryable
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// callModel 调用一次普通模型，更新草稿、遥测和本轮统计，返回带调用身份的结果。
func (a *agentImpl) callModel(
	ctx context.Context,
	req *llmkit.Request,
	inputThroughItemID defs.ItemID,
	modelCallID defs.ModelCallID,
	outputStart int,
) (*AgentModelCall, error) {
	// 1. 要求模型上报用量，开始记录这次调用的遥测；记录内容时写入请求。
	req.StreamOptions = agent_context.GetUsageStreamOptions(req.StreamOptions, a.opt.Model.Target.Protocol)
	serverAddress, serverPort := agent_context.GetModelServerAddress(a.opt.Model.Target)
	chatCtx, recorder := telemetry.StartChat(ctx, telemetry.ChatStart{
		ModelCallID:   string(modelCallID),
		Model:         a.opt.Model.Target.Model,
		Provider:      agent_context.GetModelProviderName(a.opt.Model.Target.Protocol),
		Purpose:       telemetry.ChatPurposeConversation,
		ServerAddress: serverAddress,
		ServerPort:    serverPort,
		Compacted:     a.opt.Context.IsCompacted(),
		TokenModality: agent_context.GetTokenModality(req),
	})
	if recorder.IsContentCaptured() {
		recorder.RecordInput(agent_context.NewTelemetryChatInput(req))
	}
	// 2. 累计调用次数、清空上次的结束原因，发起调用并读取事件流；发起失败时结束原因记为 error。
	a.turnStats.Steps++
	a.turnStats.InferenceCalls++
	a.turnStats.FinishReason = ""
	callCtx, cancel := context.WithCancel(chatCtx)
	defer cancel()
	modelEvents, err := a.opt.Client.Call(callCtx, *req, *a.opt.Model.Target,
		llmkit.CallOptions{Conversion: llmkit.ConversionOptions{MergeMessageRoles: a.opt.Model.MergeMessageRoles}})
	if err != nil {
		a.turnStats.FinishReason = telemetry.FinishReasonError
		recorder.End(err)
		return &AgentModelCall{
			ModelCallID: modelCallID,
			Purpose:     ModelCallPurposeConversation,
		}, err
	}
	modelCall, runErr := a.consumeEvent(callCtx, modelEvents, inputThroughItemID, modelCallID, recorder)
	// 3. 累计用量；失败且没收到结束原因时记为 error；记录内容时写入从 outputStart 开始的草稿，再结束遥测记录。
	if modelCall.Usage != nil {
		a.turnStats.Usage = a.turnStats.Usage.Add(*modelCall.Usage)
	}
	if runErr != nil && a.turnStats.FinishReason == "" {
		a.turnStats.FinishReason = telemetry.FinishReasonError
	}
	if recorder.IsContentCaptured() {
		recorder.RecordOutput(agent_context.NewTelemetryChatOutput(a.draftItems[outputStart:]))
	}
	cancel()
	recorder.End(runErr)
	return modelCall, runErr
}

// finishDraftItemsWithInterrupted 把还没结束的草稿记为中断，返回对应的中断事件。
func (a *agentImpl) finishDraftItemsWithInterrupted(items []*defs.Item) []*AgentEvent {
	// 1. 跳过已结束的 Item，把剩下的推理和工具调用状态改为中断。
	var interruptedEvents []*AgentEvent
	for _, item := range items {
		if _, ended := a.draftItemFinishReason[item]; ended {
			continue
		}
		var event *AgentEvent
		switch item.Kind {
		case defs.ItemReasoning:
			item.Reasoning.Status = defs.ReasoningInterrupted
		case defs.ItemToolCall:
			item.ToolCall.Status = defs.ToolCallInterrupted
		}
		// 2. 没有文字的消息，中断事件只带位置；其他 Item 带中断后的内容。
		if item.Kind == defs.ItemMessage && !hasMessageText(item) {
			a.draftItemFinishReason[item] = AgentEventItemInterrupted
			event = &AgentEvent{
				Type:      AgentEventItemInterrupted,
				ItemIndex: a.draftItemIndexes[item],
			}
		} else {
			event = a.finishItemWithReason(item, AgentEventItemInterrupted)
		}
		interruptedEvents = append(interruptedEvents, event)
	}
	return interruptedEvents
}

// consumeEvent 读取模型事件、更新草稿并发送输出事件，汇总用量；收到正常结束事件时生成上下文用量报告。
func (a *agentImpl) consumeEvent(
	ctx context.Context,
	modelEvents <-chan llmkit.Event,
	inputThroughItemID defs.ItemID,
	modelCallID defs.ModelCallID,
	recorder *telemetry.ChatRecorder,
) (*AgentModelCall, error) {
	// 1. 等待模型事件；取消时返回取消错误，流提前关闭时返回 io.ErrUnexpectedEOF。
	modelCall := &AgentModelCall{
		ModelCallID: modelCallID,
		Purpose:     ModelCallPurposeConversation,
	}
	for {
		select {
		case <-ctx.Done():
			return modelCall, ctx.Err()
		case event, ok := <-modelEvents:
			if !ok {
				return modelCall, io.ErrUnexpectedEOF
			}
			// 2. 把分块、响应模型、用量和结束原因记入遥测，结束原因同时写入本轮统计。
			if agent_context.IsOutputChunk(&event) {
				recorder.RecordChunk()
			}
			if event.Model != "" {
				recorder.RecordResponseModel(event.Model)
			}
			if event.Usage != nil {
				value := agent_context.NewTokenUsage(event.Usage)
				modelCall.Usage = &value
				recorder.RecordUsage(value)
			}
			if event.Type == llmkit.EventDone {
				modelCall.ContextUsage = &defs.TurnContextUsage{
					ModelCallID:        modelCallID,
					InputThroughItemID: inputThroughItemID,
					ReasoningContext:   event.ReasoningContext,
				}
				if modelCall.Usage != nil {
					modelCall.ContextUsage.Usage = *modelCall.Usage
				}
				a.turnStats.FinishReason = agent_context.NewTelemetryFinishReason(event.FinishReason)
				recorder.RecordFinishReason(a.turnStats.FinishReason)
			}
			// 3. 更新草稿并发送事件；模型报错时返回可重试的错误，生成结束时正常返回。
			a.sendEvents(a.applyEventToDraft(&event, modelCallID))
			if event.Type == llmkit.EventError {
				err := errors.New("model stream reported an error without details")
				if event.Error != nil {
					err = fmt.Errorf("model stream has error: %s", event.Error.Message)
				}
				return modelCall, &llmkit.Error{
					Stage:     llmkit.ErrorStageStream,
					Retryable: true,
					Cause:     err,
				}
			}
			if event.Type == llmkit.EventDone {
				return modelCall, nil
			}
		}
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

// finishItemWithReason 把 Item 记为完成或中断，并创建对应事件。
func (a *agentImpl) finishItemWithReason(item *defs.Item, eventType AgentEventType) *AgentEvent {
	// 1. 已结束的 Item 返回 nil，否则记录结束类型并创建事件。
	if _, ended := a.draftItemFinishReason[item]; ended {
		return nil
	}
	a.draftItemFinishReason[item] = eventType
	return a.newItemEvent(eventType, item, nil)
}

// appendDraftItem 追加草稿并记录它在本轮输出中的位置。
func (a *agentImpl) appendDraftItem(item *defs.Item) {
	// 1. 记录输出位置并追加 Item。
	a.draftItemIndexes[item] = len(a.draftItems)
	a.draftItems = append(a.draftItems, item)
}

// sendEvents 按顺序发送 Agent 事件。
func (a *agentImpl) sendEvents(pending []*AgentEvent) {
	// 1. 没有事件通道时直接返回，否则依次发送非空事件，取消后仍继续发送。
	if a.agentEventChannel == nil {
		return
	}
	for _, event := range pending {
		if event == nil {
			continue
		}
		a.agentEventChannel <- event
	}
}

// getTools 获取工具定义并转换为模型工具。
func (a *agentImpl) getTools(ctx context.Context) ([]llmkit.Tool, error) {
	// 1. 未配置工具执行器时返回空列表，否则读取工具定义。
	if a.opt.ToolExecutor == nil {
		return nil, nil
	}
	definitions, err := a.opt.ToolExecutor.GetTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("get executor tools: %w", err)
	}
	// 2. 遇到空定义返回错误，其余转换为模型工具。
	var tools []llmkit.Tool
	for _, definition := range definitions {
		if definition == nil {
			return nil, errors.New("executor returned a nil tool definition")
		}
		tools = append(tools, *definition.ToLLMKit())
	}
	return tools, nil
}

// readInputs 向 Turn 请求补充输入并加入上下文，返回是否读到输入。
func (a *agentImpl) readInputs(ctx context.Context, finish bool) (bool, error) {
	// 1. 没有输入通道时直接返回；否则向 Turn 发送请求，finish 为 true 时没有输入就停止接收补充输入。
	if a.opt.InputRequests == nil {
		return false, nil
	}
	reply := make(chan AgentInputResult, 1)
	select {
	case a.opt.InputRequests <- AgentInputRequest{
		FinishIfEmpty: finish,
		Reply:         reply,
	}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	// 2. 等待回复并把输入加入上下文，等待期间取消则返回错误。
	select {
	case result := <-reply:
		if result.Err != nil {
			return false, result.Err
		}
		if err := a.opt.Context.AppendItems(result.Items...); err != nil {
			return false, err
		}
		if len(result.Items) > 0 {
			telemetry.GetLogger(ctx).Info("turn input read", "openvaala.input.item_ids", getItemIDs(result.Items))
		}
		return len(result.Items) > 0, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// getWorkspaceAgentsInstructions 读取执行环境工作区里的 AGENTS.md 规则，并按 Executor 缓存在本轮。
func (a *agentImpl) getWorkspaceAgentsInstructions(ctx context.Context, environment executor.ExecutionEnvironment) ([]*defs.AgentsInstruction, error) {
	// 1. 没有工作区时直接返回；本轮读过这个 Executor 的规则时返回缓存。
	environmentWorkspace := environment.GetWorkspace()
	if environmentWorkspace == nil {
		return nil, nil
	}
	environmentID := environment.GetID()
	if instructions, loaded := a.workspaceAgentsInstructions[environmentID]; loaded {
		return instructions, nil
	}
	// 2. 先记下已经读过，再加载目录规则并缓存；第一次读取失败返回错误，之后再读返回空规则。
	a.workspaceAgentsInstructions[environmentID] = nil
	files, err := workspace.GetWorkspaceAgentsInstructions(ctx, environmentWorkspace)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		a.workspaceAgentsInstructions[environmentID] = append(a.workspaceAgentsInstructions[environmentID], &defs.AgentsInstruction{
			Executor: environmentID,
			Path:     file.Path,
			Text:     file.Text,
		})
	}
	return a.workspaceAgentsInstructions[environmentID], nil
}

// appendAgentsInstructions 将上下文中尚未加载的目录规则加入历史和草稿，并发送 Item 事件。
func (a *agentImpl) appendAgentsInstructions(instructions []*defs.AgentsInstruction, modelCallID defs.ModelCallID) error {
	// 1. 按 Executor 和路径跳过已加载规则，创建规则 Item。
	for _, instruction := range instructions {
		if a.opt.Context.IsAgentsInstructionLoaded(instruction.Executor, instruction.Path) {
			continue
		}
		item := &defs.Item{
			ID:                defs.NewItemID(),
			TurnID:            a.opt.TurnID,
			ModelCallID:       modelCallID,
			Kind:              defs.ItemAgentsInstruction,
			AgentsInstruction: instruction,
		}
		// 2. 先追加上下文，再加入草稿并发送开始、完成事件。
		if err := a.opt.Context.AppendItems(item); err != nil {
			return fmt.Errorf("append agents instruction: %w", err)
		}
		a.appendDraftItem(item)
		a.sendEvents([]*AgentEvent{a.newItemEvent(AgentEventItemStarted, item, nil), a.finishItemWithReason(item, AgentEventItemCompleted)})
	}
	return nil
}

// runStopHooks 运行 Stop 或 SubagentStop hook，转发运行进度，把保存的运行记录追加为输出；返回 hook 的结果。
func (a *agentImpl) runStopHooks(
	ctx context.Context,
	modelCallID defs.ModelCallID,
	completedItems []*defs.Item,
	isStopHookActive bool,
) (*hook.HookOutcome, error) {
	// 1. 没有 hook 或执行环境时返回空结果，否则按 Session 是否有父 Session 选择 Stop 或 SubagentStop。
	if a.opt.Hooks == nil || len(a.opt.Environments) == 0 {
		return &hook.HookOutcome{}, nil
	}
	defaultEnvironment := a.opt.Environments[0]
	eventName := defs.HookEventStop
	if a.opt.Session.ParentSessionID != "" {
		eventName = defs.HookEventSubagentStop
	}
	// 2. 启动转发任务把运行更新发成 HookRun 事件，再把本步最后一条回复和是否续跑过交给 hook 运行。
	runUpdates := make(chan *defs.HookRun)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		for run := range runUpdates {
			a.sendEvents([]*AgentEvent{{
				Type:    AgentEventHookRun,
				HookRun: run,
			}})
		}
	})
	outcome := a.opt.Hooks.RunEvent(ctx, &hook.HookEventRequest{
		Event: &hook.HookEvent{
			Name:                 eventName,
			Session:              a.opt.Session,
			TurnID:               a.opt.TurnID,
			LastAssistantMessage: getLastMessageText(completedItems),
			IsStopHookActive:     isStopHookActive,
		},
		ExecutorID: defaultEnvironment.GetID(),
		Workspace:  defaultEnvironment.GetWorkspace(),
	}, runUpdates)
	// 3. 关闭更新通道并等待转发结束，再将保存记录追加为本次模型调用的输出。
	close(runUpdates)
	tasks.Wait()
	events, err := a.appendCompletedItems(newHookRunItems(outcome.SavedRuns, a.opt.TurnID, modelCallID)...)
	if err != nil {
		return nil, fmt.Errorf("append stop hook runs: %w", err)
	}
	a.sendEvents(events)
	return outcome, nil
}

// appendCompletedItems 把已经完整的 Item 加入上下文和草稿，返回它们的开始、完成事件。
func (a *agentImpl) appendCompletedItems(items ...*defs.Item) ([]*AgentEvent, error) {
	// 1. 将本批 Item 一起追加到上下文；失败时直接返回，草稿不变。
	if err := a.opt.Context.AppendItems(items...); err != nil {
		return nil, err
	}
	// 2. 按顺序加入草稿，并创建每个 Item 的开始和完成事件。
	events := make([]*AgentEvent, 0, len(items)*2)
	for _, item := range items {
		a.appendDraftItem(item)
		events = append(events, a.newItemEvent(AgentEventItemStarted, item, nil), a.finishItemWithReason(item, AgentEventItemCompleted))
	}
	return events, nil
}
