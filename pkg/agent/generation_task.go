package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/VaalaCat/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/prompt"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
)

// generationTaskImpl 执行一次生成及其工具批次或停止 hook。
type generationTaskImpl struct {
	// id 是构造时分配的任务标识，随任务事件、请求和 GetCompletion 传给 Agent。
	id agentTaskID
	// opt 是构造时传入的只读生成配置，供 Run 调用模型、准备工具和运行停止 hook 使用。
	opt *generationTaskOpt
	// phase 记录当前生成任务执行到的步骤，在 Run 内推进，使等待后再次调用 Run 时从后续步骤继续。
	phase generationPhase
	// status 记录生成任务的生命周期状态，由构造、Run 和 Cancel 更新，供 Agent 通过 GetStatus 读取。
	status agentTaskStatus
	// step 是本轮普通生成的步号，在首次 Run 时由 Start.Step 写入，随任务请求和下一步配置传出。
	step int
	// stopHookContinuations 是连续被 Stop 或 SubagentStop hook 拦截并续跑的次数，首次 Run 写入，进入工具阶段清零，拦截时加 1。
	stopHookContinuations int
	// isContinuedByStopHook 本轮是否曾因 Stop 或 SubagentStop hook 拦截而续跑？首次 Run 写入，拦截续跑后置为 true 并传给后续 hook 和下一步生成。
	isContinuedByStopHook bool
	// lastModelCallID 是本任务最近一次成功模型调用的 ID，在模型阶段结束时写入，随工具批完成、停止 hook 完成请求和任务输出传出。
	lastModelCallID defs.ModelCallID
	// resumeLastAssistantMessage 是恢复已有完整回复时供停止 hook 使用的文本。
	resumeLastAssistantMessage string
	// resumeHasInput 表示完整回复之后已有有效补充输入。
	resumeHasInput bool
	// waitingTaskIDs 是当前等待完成的摘要或工具任务 ID 列表，在 Run 返回等待时写入，供 Agent 通过 GetWaitingTaskIDs 检查依赖。
	waitingTaskIDs []agentTaskID
	// output 是生成任务在工具批结束或停止 hook 结束后写入的最终输出，供 Agent 通过 GetCompletion 读取。
	output *generationTaskOutput
	// err 是 Run 返回错误或 Cancel 时记录的失败原因，通过 GetCompletion 交给 Agent。
	err error
}

// generationTaskOpt 是 Agent 在 Run 初始化时组装的只读生成任务配置，创建首个及后续生成任务时传入。
type generationTaskOpt struct {
	// Client 是调用普通模型的客户端，由 Agent 填入并在 callModel 中发起模型请求。
	Client llmkit.Client
	// Model 是普通模型定义，由 Agent 填入，供任务设置流选项、记录遥测并传入 Client.Call。
	Model *defs.ModelDefinition
	// ToolExecutor 用于在工具阶段调用 PrepareCalls 准备整批工具调用，为 nil 且模型请求工具时任务报错。
	ToolExecutor executor.ToolExecutor
	// Hooks 用于在无工具调用时执行 Stop 或 SubagentStop hook，为 nil 时跳过停止 hook。
	Hooks hook.Hooks
	// Session 是当前运行的会话信息，供 runStopHooks 判断触发 Stop 还是 SubagentStop 并传给 hook 脚本。
	Session defs.Session
	// TurnID 是本轮所属 Turn 的标识，由 runStopHooks 随 HookEvent 传给停止 hook。
	TurnID defs.TurnID
	// DefaultEnvironment 是默认执行环境，在 runStopHooks 中为停止 hook 提供工作区和执行器 ID；为 nil 时跳过停止 hook。
	DefaultEnvironment executor.ExecutionEnvironment
	// MaxSteps 是本轮普通生成的步数上限，0 表示不限；达到上限时禁用工具并在无工具或拒绝工具后结束本轮。
	MaxSteps int
}

// maxStopHookContinuations 是 Stop、SubagentStop 连续拦下结束时，模型最多续跑的次数。
const maxStopHookContinuations = 8

// newGenerationTask 创建从准备阶段开始的生成任务。
func newGenerationTask(id agentTaskID, opt *generationTaskOpt) agentTask {
	// 1. 保存身份、依赖和首个阶段。
	return &generationTaskImpl{id: id, opt: opt, phase: generationPrepare, status: agentTaskPending}
}

// Run 准备本步输入、调用模型，并执行工具分支或结束分支；等待依赖时返回。
func (g *generationTaskImpl) Run(ctx context.Context, input agentTaskInput, events chan<- agentTaskEvent) (result agentTaskRunResult, err error) {
	// 1. 首次运行保存步数和停止 hook 续跑次数；摘要或整批工具调用都有结果后，Agent 再次调用同一个任务的 Run，先检查依赖错误。
	generationInput := input.(*generationTaskInput)
	g.status = agentTaskRunning
	g.waitingTaskIDs = nil
	defer func() {
		g.err = err
		switch {
		case ctx.Err() != nil && errors.Is(err, ctx.Err()):
			g.status = agentTaskCanceled
		case err != nil:
			g.status = agentTaskFailed
		case len(result.WaitingOn) != 0:
			g.waitingTaskIDs = result.WaitingOn
			g.status = agentTaskWaiting
		default:
			g.status = agentTaskCompleted
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var resumeToolBatch *AgentResumeToolBatch
	if start := generationInput.Start; start != nil {
		g.step = start.Step
		g.stopHookContinuations = start.StopHookContinuations
		g.isContinuedByStopHook = start.IsContinuedByStopHook
		if start.ResumeFinish != nil {
			g.phase = generationFinish
			g.lastModelCallID = start.ResumeFinish.ModelCallID
			g.resumeLastAssistantMessage = start.ResumeFinish.LastAssistantMessage
			g.resumeHasInput = start.ResumeFinish.HasInput
		}
		if resumeToolBatch = start.ResumeToolBatch; resumeToolBatch != nil {
			g.phase = generationTools
			g.lastModelCallID = resumeToolBatch.ModelCallID
			g.stopHookContinuations = 0
		}
	}
	for _, completion := range generationInput.Completions {
		if completion.Err != nil {
			return result, completion.Err
		}
	}
	stepCtx := telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(g.step))
	isLastStep := g.opt.MaxSteps > 0 && g.step >= g.opt.MaxSteps

	// 2. 读取本步输入与模型请求；需要摘要时交给 Agent 启动压缩任务。
	var modelInput agentTaskGenerationInputResult
	if g.phase == generationPrepare {
		reply := make(chan agentTaskGenerationInputResult, 1)
		request := &agentTaskGenerationInputRequested{TaskID: g.id, Step: g.step, IsAfterCompaction: len(generationInput.Completions) != 0, Reply: reply}
		if start := generationInput.Start; start != nil {
			request.SkipInputRead = start.SkipInputRead
			request.SkipEnvironmentPreparation = start.SkipInputRead || start.ResumeFinish != nil
			request.ResumeCompaction = start.ResumeCompaction
		}
		events <- request
		modelInput = <-reply
		if modelInput.Err != nil {
			return result, modelInput.Err
		}
		if modelInput.Compaction != nil {
			modelInput.Compaction.Generation = generationTaskStart{
				Step: g.step, StopHookContinuations: g.stopHookContinuations, IsContinuedByStopHook: g.isContinuedByStopHook,
			}
			reply := make(chan agentTaskCreationResult, 1)
			events <- &agentTaskCompactionRequested{TaskID: g.id, Step: g.step, Input: *modelInput.Compaction, Reply: reply}
			created := <-reply
			return agentTaskRunResult{WaitingOn: created.TaskIDs}, created.Err
		}
		g.phase = generationModel
	}

	// 3. 调用模型。最后一步禁用工具，模型仍请求工具时由 Agent 保存拒绝结果。
	var modelResult agentTaskModelCallResult
	if g.phase == generationModel {
		if g.opt.Model == nil || g.opt.Model.Target == nil {
			return result, errors.New("resume requires the recorded model to continue")
		}
		req := modelInput.Request
		if isLastStep {
			req.ToolChoice = &llmkit.ToolChoice{Type: "none"}
			req.Messages = append(req.Messages, llmkit.Message{
				Role:    llmkit.RoleUser,
				Content: []llmkit.ContentBlock{{Type: llmkit.ContentTypeText, Text: prompt.MaxStepsReachedInstructions}},
			})
		}
		req.StreamOptions = agent_context.GetUsageStreamOptions(req.StreamOptions, g.opt.Model.Target.Protocol)
		err := failsafe.With(newModelRetryPolicy()).WithContext(stepCtx).Run(func() error {
			var err error
			modelResult, err = g.callModel(stepCtx, req, modelInput.InputThroughItemID, modelInput.IsCompacted, isLastStep, events)
			if modelResult.Err != nil {
				return nil
			}
			return err
		})
		if modelResult.Err != nil {
			return result, modelResult.Err
		}
		if err != nil {
			return result, err
		}
		g.lastModelCallID = modelResult.ModelCallID
		g.phase = generationFinish
		if len(modelResult.ToolCalls) != 0 {
			if isLastStep {
				reply := make(chan error, 1)
				events <- &agentTaskToolCallsRejectedRequest{TaskID: g.id, Calls: modelResult.ToolCalls, Reply: reply}
				return result, <-reply
			}
			g.stopHookContinuations = 0
			g.phase = generationTools
		}
	}

	// 4. 首次进入工具阶段时确认放行、准备整批调用，再返回等待。
	// 这批工具调用都有结果后，Agent 再次运行同一个生成任务；该任务汇总结果，决定是否请 Agent 启动下一步生成。
	if g.phase == generationTools {
		if len(modelResult.ToolCalls) != 0 {
			reply := make(chan error, 1)
			events <- &agentTaskSaveCheckpointRequested{
				Checkpoint: defs.TurnCheckpoint{
					TurnID: g.opt.TurnID, Step: g.step, Phase: defs.TurnCheckpointPhaseTools,
					IsContinuedByStopHook: g.isContinuedByStopHook,
					Tools:                 &defs.TurnToolsCheckpoint{ModelCallID: g.lastModelCallID},
				},
				AfterItemID: modelResult.ToolCalls[len(modelResult.ToolCalls)-1].ID,
				Reply:       reply,
			}
			if err := <-reply; err != nil {
				return result, err
			}
			return g.prepareTools(stepCtx, modelResult.ToolCalls, events)
		}
		var results []*defs.ToolResult
		var isTurnStopped bool
		if resumeToolBatch != nil {
			results = resumeToolBatch.Results
			isTurnStopped = resumeToolBatch.IsTurnStopped
		} else {
			for _, completion := range generationInput.Completions {
				finished := completion.Output.(*toolTaskOutput).Finished
				results = append(results, finished.Result)
				isTurnStopped = isTurnStopped || slices.ContainsFunc(finished.HookRuns, func(run *defs.HookRun) bool { return run.Status == defs.HookRunStopped })
			}
		}
		reply := make(chan error, 1)
		events <- &agentTaskToolBatchFinishedRequest{
			TaskID: g.id, Step: g.step, ModelCallID: g.lastModelCallID, Results: results, Reply: reply,
		}
		if err := <-reply; err != nil {
			return result, err
		}
		g.output = &generationTaskOutput{FinalModelCallID: g.lastModelCallID}
		if isTurnStopped {
			return result, nil
		}
		g.output.NextGeneration = &generationTaskStart{
			Step: g.step + 1, IsContinuedByStopHook: g.isContinuedByStopHook,
		}
		return result, nil
	}

	// 5. 无工具时执行停止 hook，再按停止指令、步数、拦截次数和补充输入决定接续。
	lastAssistantMessage := modelResult.LastAssistantMessage
	if g.resumeLastAssistantMessage != "" {
		lastAssistantMessage = g.resumeLastAssistantMessage
	}
	outcome, err := g.runStopHooks(stepCtx, lastAssistantMessage, events)
	if err != nil {
		return result, err
	}
	g.output = &generationTaskOutput{FinalModelCallID: g.lastModelCallID}
	if outcome.IsTurnStopped || isLastStep {
		return result, nil
	}
	if outcome.IsBlocked && g.stopHookContinuations < maxStopHookContinuations {
		g.stopHookContinuations++
		g.isContinuedByStopHook = true
	} else {
		if outcome.IsBlocked {
			telemetry.GetLogger(stepCtx).Warn("stop hook continuation limit reached", "continuations", g.stopHookContinuations)
		}
		if !g.resumeHasInput {
			reply := make(chan agentTaskFinalInputResult, 1)
			events <- &agentTaskFinalInputRequested{TaskID: g.id, Step: g.step, Reply: reply}
			finalInput := <-reply
			if finalInput.Err != nil || !finalInput.HasInput {
				return result, finalInput.Err
			}
		}
	}
	g.output.NextGeneration = &generationTaskStart{
		Step: g.step + 1, StopHookContinuations: g.stopHookContinuations, IsContinuedByStopHook: g.isContinuedByStopHook,
	}
	return result, nil
}

func (g *generationTaskImpl) GetID() agentTaskID         { return g.id }
func (g *generationTaskImpl) GetStatus() agentTaskStatus { return g.status }
func (g *generationTaskImpl) GetWaitingTaskIDs() []agentTaskID {
	return g.waitingTaskIDs
}
func (g *generationTaskImpl) GetCompletion() agentTaskCompletion {
	return agentTaskCompletion{TaskID: g.id, Output: g.output, Err: g.err}
}
func (g *generationTaskImpl) Cancel(cause error) {
	g.status, g.err, g.waitingTaskIDs = agentTaskCanceled, cause, nil
}

// callModel 执行一次普通模型调用，转发模型流并接收 Agent 合并后的结果。
func (g *generationTaskImpl) callModel(ctx context.Context, req *llmkit.Request, inputThroughItemID defs.ItemID, isCompacted bool, isLastStep bool, events chan<- agentTaskEvent) (agentTaskModelCallResult, error) {
	// 1. 分配本次调用的 ID，确认进度写入后记录遥测并发起请求。
	modelCallID := defs.NewModelCallID()
	started := make(chan error, 1)
	events <- &agentTaskModelCallStartedRequest{
		Checkpoint: defs.TurnCheckpoint{
			TurnID: g.opt.TurnID, Step: g.step, Phase: defs.TurnCheckpointPhaseRequest,
			StopHookContinuations: g.stopHookContinuations, IsContinuedByStopHook: g.isContinuedByStopHook,
			Request: &defs.TurnRequestCheckpoint{
				Purpose: defs.ModelCallPurposeConversation, ModelCallID: modelCallID, ModelCallThroughItemID: inputThroughItemID,
			},
		},
		Reply: started,
	}
	if err := <-started; err != nil {
		return agentTaskModelCallResult{Err: err}, nil
	}
	model := g.opt.Model
	serverAddress, serverPort := agent_context.GetModelServerAddress(model.Target)
	chatCtx, recorder := telemetry.StartChat(ctx, telemetry.ChatStart{
		ModelCallID: string(modelCallID), Model: model.Target.Model,
		Provider: agent_context.GetModelProviderName(model.Target.Protocol), Purpose: telemetry.ChatPurposeConversation,
		ServerAddress: serverAddress, ServerPort: serverPort, Compacted: isCompacted, TokenModality: agent_context.GetTokenModality(req),
	})
	if recorder.IsContentCaptured() {
		recorder.RecordInput(agent_context.NewTelemetryChatInput(req))
	}
	callCtx, cancel := context.WithCancel(chatCtx)
	defer cancel()
	modelEvents, callErr := g.opt.Client.Call(callCtx, *req, *model.Target,
		llmkit.CallOptions{Conversion: llmkit.ConversionOptions{MergeMessageRoles: model.MergeMessageRoles}})
	if callErr == nil {
		callErr = forwardModelEvents(callCtx, modelEvents, recorder, isLastStep, events)
	}
	cancel()

	// 2. 交回调用结果和遥测记录，等待 Agent 合并输出。
	finished := make(chan agentTaskModelCallResult, 1)
	events <- &agentTaskModelCallFinishedRequest{
		Step:     g.step,
		CallErr:  callErr,
		Recorder: recorder,
		Reply:    finished,
	}
	result := <-finished
	return result, callErr
}

// forwardModelEvents 记录模型流遥测并按接收顺序转发事件，返回模型流的结束错误。
func forwardModelEvents(ctx context.Context, modelEvents <-chan llmkit.Event, recorder *telemetry.ChatRecorder, isLastStep bool, events chan<- agentTaskEvent) error {
	hasToolCall := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-modelEvents:
			// 1. 流提前关闭时报错，其余事件记录分块、模型、用量和结束原因。
			if !ok {
				return io.ErrUnexpectedEOF
			}
			if agent_context.IsOutputChunk(&event) {
				recorder.RecordChunk()
			}
			if event.Model != "" {
				recorder.RecordResponseModel(event.Model)
			}
			if event.Usage != nil {
				recorder.RecordUsage(agent_context.NewTokenUsage(event.Usage))
			}
			if event.Type == llmkit.EventDone {
				recorder.RecordFinishReason(agent_context.NewTelemetryFinishReason(event.FinishReason))
			}
			// 2. 在结束事件交给 Agent 前确认调用级结果，再转发模型事件。
			if event.Type == llmkit.EventToolCallStart || event.Type == llmkit.EventToolCallDelta || event.Type == llmkit.EventToolCallEnd || event.Type == llmkit.EventToolCallArgumentsDelta {
				hasToolCall = true
			}
			modelCallResult := defs.ModelCallResult("")
			if event.Type == llmkit.EventDone {
				if hasToolCall && isLastStep {
					modelCallResult = defs.ModelCallResultToolsRejected
				} else if !hasToolCall {
					modelCallResult = defs.ModelCallResultReply
				}
			}
			events <- &agentTaskModelEvent{Event: event, ModelCallResult: modelCallResult}
			switch event.Type {
			case llmkit.EventError:
				err := errors.New("model stream reported an error without details")
				if event.Error != nil {
					err = fmt.Errorf("model stream has error: %s", event.Error.Message)
				}
				return &llmkit.Error{Stage: llmkit.ErrorStageStream, Retryable: true, Cause: err}
			case llmkit.EventDone:
				return nil
			}
		}
	}
}

// prepareTools 准备完整工具批次、转发准备事件，再请求 Agent 创建工具任务。
func (g *generationTaskImpl) prepareTools(ctx context.Context, toolCalls []*defs.Item, events chan<- agentTaskEvent) (agentTaskRunResult, error) {
	// 1. 检查执行器并实时转发整批准备事件。
	if g.opt.ToolExecutor == nil {
		return agentTaskRunResult{}, errors.New("agent received tool calls without an executor")
	}
	if err := ctx.Err(); err != nil {
		return agentTaskRunResult{}, err
	}
	executorEvents := make(chan executor.ExecutorEvent)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		for event := range executorEvents {
			events <- &agentTaskExecutorEvent{TaskID: g.id, Event: event}
		}
	})
	preparedCalls := g.opt.ToolExecutor.PrepareCalls(ctx, toolCalls, executorEvents)
	close(executorEvents)
	tasks.Wait()
	// 2. 批准备结束后请求创建任务，返回待完成的 ID。
	reply := make(chan agentTaskCreationResult, 1)
	events <- &agentTaskToolsRequested{TaskID: g.id, Step: g.step, Calls: preparedCalls, Reply: reply}
	created := <-reply
	return agentTaskRunResult{WaitingOn: created.TaskIDs}, created.Err
}

// runStopHooks 运行 Stop 或 SubagentStop，转发运行更新并等待 Agent 保存记录。
func (g *generationTaskImpl) runStopHooks(ctx context.Context, lastAssistantMessage string, events chan<- agentTaskEvent) (*hook.HookOutcome, error) {
	// 1. 按 Session 是否有父 Session 选择 hook；未配置 hook 或环境时跳过。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	environment := g.opt.DefaultEnvironment
	if g.opt.Hooks == nil || environment == nil {
		return &hook.HookOutcome{}, nil
	}
	eventName := defs.HookEventStop
	if g.opt.Session.ParentSessionID != "" {
		eventName = defs.HookEventSubagentStop
	}
	// 2. 执行 hook 并转发进度，等转发完成后交回保存记录。
	runUpdates := make(chan *defs.HookRun)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		for run := range runUpdates {
			events <- &agentTaskHookRunEvent{TaskID: g.id, Run: run}
		}
	})
	outcome := g.opt.Hooks.RunEvent(ctx, &hook.HookEventRequest{
		Event: &hook.HookEvent{
			Name: eventName, Session: g.opt.Session, TurnID: g.opt.TurnID,
			LastAssistantMessage: lastAssistantMessage, IsStopHookActive: g.isContinuedByStopHook,
		},
		ExecutorID: environment.GetID(), Workspace: environment.GetWorkspace(),
	}, runUpdates)
	close(runUpdates)
	tasks.Wait()
	reply := make(chan error, 1)
	events <- &agentTaskStopHooksFinishedRequest{TaskID: g.id, ModelCallID: g.lastModelCallID, Outcome: outcome, Reply: reply}
	return outcome, <-reply
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
