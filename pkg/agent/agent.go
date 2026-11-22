package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// Agent 在一次 Turn 内生成模型回复、执行工具并发送输出事件。
type Agent interface {
	// Run 循环调用模型、执行工具，直到模型不再调用工具且没有新的补充输入；IsCompactOnly 时只压缩上下文。每个实例只 Run 一次。
	// events 非 nil 时调用方要一直读到通道关闭，Run 返回前关闭它；取消后仍会发完剩余事件。
	// 临时模型错误自动重试；失败尝试的输出留在草稿和事件里，只有成功调用的输出加入上下文、执行工具。
	Run(ctx context.Context, events chan<- *AgentEvent) (AgentResult, error)
}

// agentTask 是 Agent 按阶段启动、等待并结算的一次内存任务。
type agentTask interface {
	// GetID 返回构造时分配的任务标识。
	GetID() agentTaskID
	// GetStatus 返回任务的阶段状态，只在 Run 外读取。
	GetStatus() agentTaskStatus
	// GetWaitingTaskIDs 返回只读的依赖 ID，只在 Run 外读取。
	GetWaitingTaskIDs() []agentTaskID
	// GetCompletion 返回任务的终态结果，只在 Run 返回后读取。
	GetCompletion() agentTaskCompletion
	// Run 执行至下一次等待或终态，返回前发完本次事件。
	Run(ctx context.Context, input agentTaskInput, events chan<- agentTaskEvent) (result agentTaskRunResult, err error)
	// Cancel 将尚未运行或正在等待的任务记为取消。
	Cancel(cause error)
}

// agentImpl 是一次 Turn 的 Agent，保存配置、本轮草稿、已读的目录规则和记忆目录，以及运行统计。
type agentImpl struct {
	// 本轮配置与输入
	opt *AgentOpt
	// modelTools 是供本轮模型请求和上下文测量复用的工具定义。
	modelTools []llmkit.Tool
	// shouldRefreshTools 是否有工具请求在下一次生成前刷新工具表？
	shouldRefreshTools bool
	// generationOpt 是 Run 初始化时组装的只读生成任务配置，创建首个和后续生成任务时传入。
	generationOpt *generationTaskOpt
	// workspaceAgentsInstructions 按 Executor 保存本轮读到的全局和目录规则；键存在就表示已经读过，值可以为空。
	workspaceAgentsInstructions map[string][]*defs.AgentsInstruction
	// memoryIndexes 是本轮开始时读到的 MEMORY.md；文件不存在、读失败或内容为空时没有对应的项。
	memoryIndexes []*defs.MemoryIndex

	// 任务执行
	// tasks 保存本轮尚未结算的任务实例，包括等待依赖或待交付结果的任务；结果交付给等待者、独立任务结束或取消清理后移除。
	tasks map[agentTaskID]agentTask
	// runningTaskIDs 启动任务 Run 时加入，Agent 收到该次 Run 的返回通知后移除。等待依赖的任务仍在 tasks 中。
	runningTaskIDs map[agentTaskID]struct{}
	// nextTaskID 是单次 Run 内递增的任务 ID 计数器，每创建一个新任务前加 1。
	nextTaskID agentTaskID
	// taskRuns 跟踪所有已启动的任务 goroutine，Run 在事件循环退出后等待它们全部结束。
	taskRuns sync.WaitGroup
	// taskEvents 接收各任务发回的进度事件、同步请求和 Run 返回通知；由 Run 创建并在全部任务退出后关闭。
	taskEvents chan agentTaskEvent
	// executorEvents 由 ToolTask 直接传给 ToolExecutor.Run，接收执行器向 Agent 直发的进度、hook 和结果事件；由 Run 创建并在全部任务退出后关闭。
	executorEvents chan executor.ExecutorEvent

	// 输出草稿
	// draftItems 是本轮输出草稿，按首次出现顺序排列。
	draftItems []*defs.Item
	// draftItemIndexes 是每个草稿在 draftItems 里的位置。
	draftItemIndexes map[*defs.Item]int
	// draftItemFinishReason 记录已结束草稿的结束事件类型（完成或中断）；不在表里的草稿还没结束。
	draftItemFinishReason map[*defs.Item]AgentEventType
	// compactionDraft 保存已发出开始事件但尚未写入上下文的摘要草稿；应用成功后清空，失败或 Run 退出未应用时发中断事件并移除。
	compactionDraft *defs.Item
	// checkpointAfterItemID 是最近交给 Turn 的历史末项；本次运行还没有交付 Item 时为空。
	checkpointAfterItemID defs.ItemID
	// checkpointAfterDraftCount 是该末项之前（含末项本身）的草稿数，供按首次出现顺序更新末项；尚无交付时为零。
	checkpointAfterDraftCount int

	// 当前模型调用与工具批次
	// modelStream 保存当前正在接收事件的普通模型调用状态；收到开始请求时创建，开始处理完成请求时取出并清空。
	modelStream *agentModelStream
	// toolCallsByCallID 按 CallID 暂存本批工具草稿及已合并的执行结果；模型返回工具调用时建立，准备阶段已结束或被 ToolTask 取走结果后移除。
	toolCallsByCallID map[string]agentToolCall
	// toolBatchModelCallID 记录当前工具批次所属的模型调用 ID，供 Run 退出前补加残留目录规则时使用。
	toolBatchModelCallID defs.ModelCallID
	// toolAgentsInstructions 暂存本批工具执行结果带回的目录规则；在批收尾时追加并清空，未及批收尾则在 Run 退出前补加。
	toolAgentsInstructions []*defs.AgentsInstruction

	// 对外事件与最终结果
	// agentEventChannel 是 Run 传入的事件通道；nil 时不发事件。
	agentEventChannel chan<- *AgentEvent
	// turnStats 是本次运行的统计，随循环累计，Run 返回时交给调用方。
	turnStats AgentStats
	// finalModelCallID 普通生成不再请求下一步时记下的模型调用 ID，供组装 AgentResult 使用。
	finalModelCallID defs.ModelCallID
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
	// Environments 保存本轮开场的执行环境，供工具、目录规则和记忆使用。
	Environments []executor.ExecutionEnvironment
	// DefaultExecutorID 指定提供开场目录规则和 Stop hook 工作区的稳定环境。
	DefaultExecutorID string
	// HasOpeningEnvironments 是否已提供开场环境列表？已提供的空列表也用于建立开场工具表。
	HasOpeningEnvironments bool
	// Requests 供 Agent 请求 Turn 读取补充输入或保存 checkpoint；nil 时独立运行，不读补充输入也不等待持久化。
	Requests chan<- AgentRequest
	// Resume 是 Turn 根据已保存历史计算的本次 Run 恢复输入；nil 表示从第一步开始。
	Resume *AgentResumeInput
	// IsCompactOnly 这个 Agent 是否只压缩上下文？是的话 Run 只把历史压成摘要就返回，不调用普通模型。
	IsCompactOnly bool
	// Hooks 在模型这一步没有工具调用时触发 Stop 或 SubagentStop；未配置 hook 或没有稳定默认环境时跳过。
	Hooks hook.Hooks
	// Session 是本 Agent 运行所在的 Session，随 hook 事件交给脚本；ParentSessionID 非空表示子 Session，触发 SubagentStop。
	Session defs.Session
	// MaxSteps 是本轮普通生成的步数上限，不含摘要或重试；0 表示不限。最后一步禁用工具并要求模型总结进度。
	MaxSteps int
}

// maxMemoryIndexBytes 是每份 MEMORY.md 放进上下文的字节数上限。
const maxMemoryIndexBytes = 8 << 10

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
	// 1. 检查身份和执行依赖；只收尾或只压缩时不需要普通模型。
	if opt == nil || opt.TurnID == "" || opt.SessionID == "" || opt.Client == nil {
		return nil, errors.New("agent requires a turn ID, session ID and model client")
	}
	finishOnly := opt.Resume != nil && (opt.Resume.Finish != nil || opt.Resume.ToolBatch != nil && opt.Resume.ToolBatch.IsTurnStopped)
	if !finishOnly && (opt.Context == nil || !opt.IsCompactOnly && (opt.Model == nil || opt.Model.Target == nil)) {
		return nil, errors.New("agent requires a model with target and context")
	}
	// 2. 保存配置，创建草稿索引、结束状态和工作区规则表。
	return &agentImpl{
		opt:                         opt,
		draftItemIndexes:            make(map[*defs.Item]int),
		draftItemFinishReason:       make(map[*defs.Item]AgentEventType),
		workspaceAgentsInstructions: make(map[string][]*defs.AgentsInstruction),
	}, nil
}

// Run 初始化本轮，串行合并任务事件，等待任务结束后返回草稿和统计。
func (a *agentImpl) Run(ctx context.Context, events chan<- *AgentEvent) (result AgentResult, runErr error) {
	// 1. 接管输出通道；从回复或工具批收尾恢复时先不读取工具定义。
	a.agentEventChannel = events
	if events != nil {
		defer close(events)
	}
	resumeAtSettlement := a.opt.Resume != nil && (a.opt.Resume.Finish != nil || a.opt.Resume.ToolBatch != nil)
	if !resumeAtSettlement {
		tools, err := a.getTools(ctx, a.opt.HasOpeningEnvironments)
		if err != nil {
			return result, err
		}
		a.modelTools = tools
	}
	defer func() {
		result = AgentResult{Items: a.draftItems, Stats: a.turnStats}
		if runErr == nil {
			result.FinalModelCallID = a.finalModelCallID
		}
		if (!resumeAtSettlement || a.turnStats.Steps != 0) && a.opt.Context != nil {
			if _, err := a.measureContext(a.modelTools); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
	}()

	// 2. 初始化任务事件循环；按运行模式启动手动压缩或首次普通生成。
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.taskEvents = make(chan agentTaskEvent)
	a.executorEvents = make(chan executor.ExecutorEvent)
	a.tasks = make(map[agentTaskID]agentTask)
	a.runningTaskIDs = make(map[agentTaskID]struct{})
	start := generationTaskStart{Step: 1}
	if resume := a.opt.Resume; resume != nil {
		start.Step = resume.Step
		start.StopHookContinuations = resume.StopHookContinuations
		start.IsContinuedByStopHook = resume.IsContinuedByStopHook
		start.SkipInputRead = resume.Retry != nil
		start.ResumeFinish = resume.Finish
		start.ResumeToolBatch = resume.ToolBatch
		start.ResumeCompaction = resume.Compaction
		a.turnStats.Usage = resume.Usage
		if resume.Finish != nil {
			a.finalModelCallID = resume.Finish.ModelCallID
		}
		if resume.Retry != nil {
			a.checkpointAfterItemID = resume.Retry.InputThroughItemID
		}
		for _, item := range resume.ExistingItems {
			cloned := item.Clone()
			a.appendDraftItem(cloned)
			a.draftItemFinishReason[cloned] = AgentEventItemCompleted
		}
		a.checkpointAfterDraftCount = len(a.draftItems)
	}
	defer close(a.taskEvents)
	defer close(a.executorEvents)
	var manualCompaction agentTask
	if a.opt.IsCompactOnly {
		input, err := a.prepareCompaction(true, start.ResumeCompaction)
		if err != nil || input == nil {
			return result, err
		}
		input.Generation = start
		a.nextTaskID++
		manualCompaction = newCompactionTask(a.nextTaskID)
		a.startTask(taskCtx, manualCompaction, input)
	} else {
		var defaultEnvironment executor.ExecutionEnvironment
		index := slices.IndexFunc(a.opt.Environments, func(environment executor.ExecutionEnvironment) bool {
			return !environment.IsDynamic() && environment.GetID() == a.opt.DefaultExecutorID
		})
		if index >= 0 {
			defaultEnvironment = a.opt.Environments[index]
		}
		if a.opt.Resume == nil {
			if err := a.loadGenerationEnvironment(ctx, defaultEnvironment); err != nil {
				return result, err
			}
		}
		a.generationOpt = &generationTaskOpt{
			Client: a.opt.Client, Model: a.opt.Model, ToolExecutor: a.opt.ToolExecutor, Hooks: a.opt.Hooks,
			Session: a.opt.Session, TurnID: a.opt.TurnID, DefaultEnvironment: defaultEnvironment, MaxSteps: a.opt.MaxSteps,
		}
		a.nextTaskID++
		a.startTask(taskCtx, newGenerationTask(a.nextTaskID, a.generationOpt), &generationTaskInput{Start: &start})
	}

	// 3. 合并事件。遇到取消或首个错误后停止派发，仍处理已启动任务的请求和最终事件。
	taskCtxDone := taskCtx.Done()
	for len(a.tasks) != 0 {
		if taskCtx.Err() != nil {
			for taskID, task := range a.tasks {
				if _, running := a.runningTaskIDs[taskID]; running {
					continue
				}
				if task.GetStatus() == agentTaskWaiting || task.GetStatus() == agentTaskPending {
					task.Cancel(taskCtx.Err())
				}
				delete(a.tasks, taskID)
			}
		}
		if len(a.runningTaskIDs) == 0 {
			if len(a.tasks) != 0 {
				runErr = errors.New("agent task dependencies cannot make progress")
			}
			break
		}
		var eventErr error
		select {
		case <-taskCtxDone:
			taskCtxDone = nil
		case event := <-a.taskEvents:
			eventErr = a.handleTaskEvent(taskCtx, event)
		case event := <-a.executorEvents:
			eventErr = a.handleExecutorEvent(taskCtx, event)
		}
		if eventErr != nil && runErr == nil && eventErr != ctx.Err() {
			runErr = eventErr
			cancel()
		}
	}

	// 4. 收齐返回通知后等待任务退出，再结束未应用的摘要和本轮输出。
	a.taskRuns.Wait()
	a.interruptCompactionDraft()
	if len(a.toolAgentsInstructions) != 0 {
		if err := a.appendAgentsInstructions(a.toolAgentsInstructions, a.toolBatchModelCallID); runErr == nil {
			runErr = err
		}
	}
	if runErr == nil && (manualCompaction == nil || manualCompaction.GetStatus() != agentTaskCompleted) {
		runErr = ctx.Err()
	}
	return result, runErr
}

// startTask 登记一次任务运行，并在它返回后发送通知。
func (a *agentImpl) startTask(ctx context.Context, task agentTask, input agentTaskInput) {
	// 1. 取消后不再启动任务；其余先登记，再启动本次 Run。
	if err := ctx.Err(); err != nil {
		task.Cancel(err)
		return
	}
	taskID := task.GetID()
	a.tasks[taskID] = task
	a.runningTaskIDs[taskID] = struct{}{}
	events := a.taskEvents
	a.taskRuns.Go(func() {
		result, err := task.Run(ctx, input, events)
		events <- &agentTaskRunReturnedEvent{TaskID: taskID, Result: result, Err: err}
	})
}

// handleTaskEvent 合并任务通知，处理任务请求并回复调用方。
func (a *agentImpl) handleTaskEvent(ctx context.Context, event agentTaskEvent) error {
	// 1. 合并无需回复的通知，或按事件类别处理请求并关闭回复通道。
	switch event := event.(type) {
	case *agentTaskRunReturnedEvent:
		return a.handleTaskRunReturned(ctx, event)
	case *agentTaskModelEvent:
		a.handleTaskModelEvent(&event.Event, event.ModelCallResult)
	case *agentTaskExecutorEvent:
		return a.handleExecutorEvent(ctx, event.Event)
	case *agentTaskHookRunEvent:
		a.sendEvents([]*AgentEvent{{Type: AgentEventHookRun, HookRun: event.Run}})
	case *agentTaskGenerationInputRequested:
		input := a.prepareGenerationInput(telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(event.Step)), event.Step, event.IsAfterCompaction, event.SkipInputRead, event.SkipEnvironmentPreparation, event.ResumeCompaction)
		event.Reply <- input
		close(event.Reply)
		return input.Err
	case *agentTaskModelCallStartedRequest:
		err := a.saveCheckpoint(ctx, event.Checkpoint, "")
		if err == nil {
			a.handleModelCallStarted(event.Checkpoint.Request.ModelCallID, event.Checkpoint.Request.ModelCallThroughItemID)
		}
		event.Reply <- err
		close(event.Reply)
		return err
	case *agentTaskSaveCheckpointRequested:
		err := a.saveCheckpoint(ctx, event.Checkpoint, event.AfterItemID)
		event.Reply <- err
		close(event.Reply)
		return err
	case *agentTaskModelCallFinishedRequest:
		call := a.handleModelCallFinished(telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(event.Step)), event)
		event.Reply <- call
		close(event.Reply)
		return call.Err
	case *agentTaskToolResultRequested:
		result := a.takeToolResult(event.CallID)
		event.Reply <- result
		close(event.Reply)
	case *agentTaskToolCallsRejectedRequest:
		err := a.rejectToolCalls(event.Calls)
		event.Reply <- err
		close(event.Reply)
		return err
	case *agentTaskToolsRequested:
		result := a.startToolTasks(telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(event.Step)), event.Calls)
		event.Reply <- result
		close(event.Reply)
		return result.Err
	case *agentTaskCompactionRequested:
		result := agentTaskCreationResult{Err: ctx.Err()}
		if result.Err == nil {
			a.nextTaskID++
			a.startTask(telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(event.Step)), newCompactionTask(a.nextTaskID), &event.Input)
			result.TaskIDs = []agentTaskID{a.nextTaskID}
		}
		event.Reply <- result
		close(event.Reply)
		return result.Err
	case *agentTaskCompactionFinishedRequest:
		err := a.handleCompactionFinished(ctx, event.Output, event.Err)
		event.Reply <- err
		close(event.Reply)
		if err != nil {
			return fmt.Errorf("compact agent context: %w", err)
		}
	case *agentTaskStopHooksFinishedRequest:
		events, err := a.appendCompletedItems(newHookRunItems(event.Outcome.SavedRuns, a.opt.TurnID, event.ModelCallID)...)
		a.sendEvents(events)
		if err != nil {
			err = fmt.Errorf("append stop hook runs: %w", err)
		}
		event.Reply <- err
		close(event.Reply)
		return err
	case *agentTaskToolBatchFinishedRequest:
		err := a.handleToolBatchFinished(telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(event.Step)), event.ModelCallID, event.Results)
		event.Reply <- err
		close(event.Reply)
		return err
	case *agentTaskFinalInputRequested:
		hasInput, err := a.readInputs(telemetry.ContextWithAttrs(ctx, telemetry.TurnStep(event.Step)), true)
		event.Reply <- agentTaskFinalInputResult{HasInput: hasInput, Err: err}
		close(event.Reply)
		return err
	}
	return nil
}

// handleTaskRunReturned 结算本次任务调用，启动已齐备的等待任务或下一次生成。
func (a *agentImpl) handleTaskRunReturned(ctx context.Context, event *agentTaskRunReturnedEvent) error {
	// 1. 只在 Run 返回后读取任务状态。失败的压缩先结束摘要草稿。
	delete(a.runningTaskIDs, event.TaskID)
	task := a.tasks[event.TaskID]
	switch task.(type) {
	case *compactionTaskImpl:
		if event.Err != nil {
			a.interruptCompactionDraft()
		}
		if a.opt.IsCompactOnly {
			delete(a.tasks, event.TaskID)
			return event.Err
		}
	case *generationTaskImpl:
		if len(event.Result.WaitingOn) == 0 {
			delete(a.tasks, event.TaskID)
			if event.Err != nil {
				return event.Err
			}
			if ctx.Err() != nil {
				return nil
			}
			output := task.GetCompletion().Output.(*generationTaskOutput)
			if output.NextGeneration == nil {
				a.finalModelCallID = output.FinalModelCallID
				return nil
			}
			if a.opt.Model == nil || a.opt.Model.Target == nil || a.opt.Context == nil {
				return errors.New("resume requires the recorded model to continue")
			}
			resume := a.opt.Resume
			readDeferredTools := resume != nil &&
				(resume.Finish != nil && output.FinalModelCallID == resume.Finish.ModelCallID ||
					resume.ToolBatch != nil && output.FinalModelCallID == resume.ToolBatch.ModelCallID)
			if readDeferredTools || a.shouldRefreshTools {
				useOpeningEnvironments := a.opt.HasOpeningEnvironments && !a.shouldRefreshTools
				a.shouldRefreshTools = false
				tools, err := a.getTools(ctx, useOpeningEnvironments)
				if err != nil {
					return err
				}
				a.modelTools = tools
			}
			a.nextTaskID++
			a.startTask(ctx, newGenerationTask(a.nextTaskID, a.generationOpt), &generationTaskInput{Start: output.NextGeneration})
			return nil
		}
	}

	// 2. 登记等待和依赖返回时都检查依赖；没有齐备时保持等待。
	if ctx.Err() != nil {
		return nil
	}
	for taskID, waitingTask := range a.tasks {
		if _, running := a.runningTaskIDs[taskID]; running || waitingTask.GetStatus() != agentTaskWaiting {
			continue
		}
		waitingIDs := waitingTask.GetWaitingTaskIDs()
		completions := make([]agentTaskCompletion, 0, len(waitingIDs))
		for _, dependencyID := range waitingIDs {
			if _, running := a.runningTaskIDs[dependencyID]; running {
				break
			}
			dependency := a.tasks[dependencyID]
			if dependency == nil {
				break
			}
			switch dependency.GetStatus() {
			case agentTaskCompleted, agentTaskFailed, agentTaskCanceled:
				completions = append(completions, dependency.GetCompletion())
			}
		}
		if len(completions) != len(waitingIDs) {
			continue
		}
		// 3. 按等待次序交回完整结果；已交付的依赖不再保留。
		for _, completion := range completions {
			delete(a.tasks, completion.TaskID)
		}
		a.startTask(ctx, waitingTask, &generationTaskInput{Completions: completions})
	}
	return nil
}

// prepareGenerationInput 读取本步输入、补目录规则与记忆，返回摘要或普通请求。
func (a *agentImpl) prepareGenerationInput(ctx context.Context, step int, isAfterCompaction bool, skipInputRead bool, skipEnvironmentPreparation bool, resumeCompaction *AgentResumeCompaction) agentTaskGenerationInputResult {
	// 1. 检查取消并接续原摘要；没有已确认范围时才读取新的补充输入。
	if err := ctx.Err(); err != nil {
		return agentTaskGenerationInputResult{Err: err}
	}
	if resumeCompaction != nil {
		compaction, err := a.prepareCompaction(false, resumeCompaction)
		if compaction != nil || err != nil {
			return agentTaskGenerationInputResult{Compaction: compaction, Err: err}
		}
		isAfterCompaction = true
	}
	if !isAfterCompaction && !skipInputRead {
		if _, err := a.readInputs(ctx, false); err != nil {
			return agentTaskGenerationInputResult{Err: err}
		}
	}
	// 2. 接续原压缩步的普通请求时读取规则和记忆，再按正常顺序补回被覆盖的 Item。
	if !skipEnvironmentPreparation {
		if resume := a.opt.Resume; isAfterCompaction && resume != nil && resume.Compaction != nil && resume.Step == step {
			if err := a.loadGenerationEnvironment(ctx, a.generationOpt.DefaultEnvironment); err != nil {
				return agentTaskGenerationInputResult{Err: err}
			}
		}
		var instructions []*defs.AgentsInstruction
		for _, environment := range a.opt.Environments {
			instructions = append(instructions, a.workspaceAgentsInstructions[environment.GetID()]...)
		}
		if err := a.appendAgentsInstructions(instructions, ""); err != nil {
			return agentTaskGenerationInputResult{Err: err}
		}
		if err := a.appendMemoryIndexes(""); err != nil {
			return agentTaskGenerationInputResult{Err: err}
		}
	}
	// 3. 只在首次准备时检查压缩，随后测量并组装普通请求。
	if !isAfterCompaction && !skipEnvironmentPreparation {
		compaction, err := a.prepareCompaction(false, nil)
		if compaction != nil || err != nil {
			return agentTaskGenerationInputResult{Compaction: compaction, Err: err}
		}
	}
	if _, err := a.measureContext(a.modelTools); err != nil {
		return agentTaskGenerationInputResult{Err: err}
	}
	req, inputThroughItemID, err := a.opt.Context.BuildModelRequest(a.modelTools)
	if err != nil {
		return agentTaskGenerationInputResult{Err: fmt.Errorf("build model request: %w", err)}
	}
	return agentTaskGenerationInputResult{Request: req, InputThroughItemID: inputThroughItemID, IsCompacted: a.opt.Context.IsCompacted()}
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
	// 1. 按草稿首次出现的顺序记下已结束、有内容可保存的末项。
	for _, event := range pending {
		if event == nil {
			continue
		}
		if a.opt.Requests != nil && event.Item != nil && event.ItemIndex >= a.checkpointAfterDraftCount &&
			(event.Type == AgentEventItemCompleted || event.Type == AgentEventItemInterrupted) {
			a.checkpointAfterItemID = event.Item.ID
			a.checkpointAfterDraftCount = event.ItemIndex + 1
		}
		// 2. 有订阅时按顺序发送非空事件，取消后仍继续发送。
		if a.agentEventChannel != nil {
			a.agentEventChannel <- event
		}
	}
}

// getTools 获取工具定义并转换为模型工具。
func (a *agentImpl) getTools(ctx context.Context, useOpeningEnvironments bool) ([]llmkit.Tool, error) {
	// 1. 未配置工具执行器时返回空列表，否则读取工具定义。
	if a.opt.ToolExecutor == nil {
		return nil, nil
	}
	var definitions []*defs.ToolDefinition
	var err error
	if useOpeningEnvironments {
		definitions, err = a.opt.ToolExecutor.GetToolsForEnvironments(ctx, a.opt.Environments, a.opt.DefaultExecutorID)
	} else {
		definitions, err = a.opt.ToolExecutor.GetTools(ctx)
	}
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

// saveCheckpoint 向 Turn 请求保存进度，等待写成或取消。
func (a *agentImpl) saveCheckpoint(ctx context.Context, checkpoint defs.TurnCheckpoint, afterItemID defs.ItemID) error {
	// 1. 独立运行直接继续；模型请求以本轮最近交付的 Item 为准，没有时使用已加载历史中的请求末项。
	if ctx.Err() != nil || a.opt.Requests == nil {
		return ctx.Err()
	}
	if afterItemID == "" {
		afterItemID = a.checkpointAfterItemID
		if afterItemID == "" && checkpoint.Request != nil {
			afterItemID = checkpoint.Request.ModelCallThroughItemID
			if compaction := checkpoint.Request.Compaction; compaction != nil {
				afterItemID = compaction.CoveredThroughItemID
			}
		}
	}
	// 2. 发出请求并等待一次性回复。
	reply := make(chan error, 1)
	select {
	case a.opt.Requests <- AgentSaveCheckpointRequest{Checkpoint: checkpoint, AfterItemID: afterItemID, Reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		if err != nil {
			return err
		}
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readInputs 向 Turn 请求补充输入并加入上下文，返回是否读到输入。
func (a *agentImpl) readInputs(ctx context.Context, finish bool) (bool, error) {
	// 1. 没有输入通道时直接返回；否则向 Turn 发送请求，finish 为 true 时没有输入就停止接收补充输入。
	if a.opt.Requests == nil {
		return false, nil
	}
	reply := make(chan AgentInputResult, 1)
	select {
	case a.opt.Requests <- AgentInputRequest{
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
		if len(result.Items) > 0 && a.opt.Context != nil {
			if err := a.opt.Context.AppendItems(result.Items...); err != nil {
				return false, err
			}
		}
		if len(result.Items) > 0 {
			a.checkpointAfterItemID = result.Items[len(result.Items)-1].ID
			a.checkpointAfterDraftCount = len(a.draftItems)
			telemetry.GetLogger(ctx).Info("turn input read", "openvaala.input.item_ids", getItemIDs(result.Items))
		}
		return len(result.Items) > 0, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// loadGenerationEnvironment 读取默认环境的规则和本轮的记忆目录。
func (a *agentImpl) loadGenerationEnvironment(ctx context.Context, defaultEnvironment executor.ExecutionEnvironment) error {
	// 1. 先读默认环境的全局和目录规则，再读用户及项目记忆。
	if defaultEnvironment != nil {
		if _, err := a.getWorkspaceAgentsInstructions(ctx, defaultEnvironment); err != nil {
			return fmt.Errorf("load AGENTS.md from executor %q: %w", defaultEnvironment.GetID(), err)
		}
	}
	a.loadMemoryIndexes(ctx)
	return nil
}

// getWorkspaceAgentsInstructions 读取执行环境的全局和目录规则，并按 Executor 缓存在本轮。
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
			IsGlobal: file.IsGlobal,
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

// loadMemoryIndexes 在 Turn 开始时读 memory 环境的 user/MEMORY.md 和 project/MEMORY.md；
// 没有 memory 环境时什么都不做，读失败只记 Warn。
func (a *agentImpl) loadMemoryIndexes(ctx context.Context) {
	// 1. 只从有记忆工作区的稳定 memory 环境读取目录。
	environmentIndex := slices.IndexFunc(a.opt.Environments, func(environment executor.ExecutionEnvironment) bool {
		return environment.GetID() == defs.MemoryExecutorID && !environment.IsDynamic() && environment.GetWorkspace() != nil
	})
	if environmentIndex < 0 {
		return
	}
	memoryWorkspace, ok := a.opt.Environments[environmentIndex].GetWorkspace().(workspace.MemoryWorkspace)
	if !ok {
		return
	}
	// 2. 依次读两份目录：文件不存在什么都不放，读失败记 Warn，内容是空白也不放；项目记忆带上项目名。
	for _, indexPath := range []string{"user/MEMORY.md", "project/MEMORY.md"} {
		project := ""
		if indexPath == "project/MEMORY.md" {
			project = memoryWorkspace.GetProjectName()
		}
		content, err := memoryWorkspace.ReadFile(ctx, filepath.Join(memoryWorkspace.GetRootDirectory(), indexPath), 0, maxMemoryIndexBytes+1)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			telemetry.GetLogger(ctx).Warn("memory index read failed", "path", indexPath, "error", err)
			continue
		}
		text, isTruncated := truncateMemoryIndex(content.Content, content.EOF)
		if strings.TrimSpace(text) == "" {
			continue
		}
		a.memoryIndexes = append(a.memoryIndexes, &defs.MemoryIndex{
			Path:        indexPath,
			Project:     project,
			Text:        text,
			IsTruncated: isTruncated,
		})
	}
}

// appendMemoryIndexes 把上下文里还没有的记忆目录加入历史和草稿，并发送 Item 事件。
func (a *agentImpl) appendMemoryIndexes(modelCallID defs.ModelCallID) error {
	// 1. 按路径和项目跳过已加载的记忆目录，创建记忆目录 Item。
	for _, index := range a.memoryIndexes {
		if a.opt.Context.IsMemoryIndexLoaded(index.Path, index.Project) {
			continue
		}
		item := &defs.Item{
			ID:          defs.NewItemID(),
			TurnID:      a.opt.TurnID,
			ModelCallID: modelCallID,
			Kind:        defs.ItemMemoryIndex,
			MemoryIndex: index,
		}
		// 2. 先追加上下文，再加入草稿并发送开始、完成事件。
		if err := a.opt.Context.AppendItems(item); err != nil {
			return fmt.Errorf("append memory index: %w", err)
		}
		a.appendDraftItem(item)
		a.sendEvents([]*AgentEvent{a.newItemEvent(AgentEventItemStarted, item, nil), a.finishItemWithReason(item, AgentEventItemCompleted)})
	}
	return nil
}

// appendCompletedItems 把已经完整的 Item 加入上下文和草稿，返回它们的开始、完成事件。
func (a *agentImpl) appendCompletedItems(items ...*defs.Item) ([]*AgentEvent, error) {
	// 1. 将本批 Item 一起追加到上下文；失败时直接返回，草稿不变。
	if a.opt.Context != nil {
		if err := a.opt.Context.AppendItems(items...); err != nil {
			return nil, err
		}
	}
	// 2. 按顺序加入草稿，并创建每个 Item 的开始和完成事件。
	events := make([]*AgentEvent, 0, len(items)*2)
	for _, item := range items {
		a.appendDraftItem(item)
		events = append(events, a.newItemEvent(AgentEventItemStarted, item, nil), a.finishItemWithReason(item, AgentEventItemCompleted))
	}
	return events, nil
}
