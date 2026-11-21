package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// toolTaskImpl 保存一次已准备的工具调用的结果。
type toolTaskImpl struct {
	// id 是构造时分配的任务标识，随取结果请求和 GetCompletion 传给 Agent。
	id agentTaskID
	// toolExecutor 是执行工具调用的执行器，在构造时保存，并在准备阶段未结算时由 Run 调用。
	toolExecutor executor.ToolExecutor
	// status 是工具任务的阶段状态，由构造、Run 和 Cancel 更新，供 Agent 在 Run 外通过 GetStatus 读取。
	status agentTaskStatus
	// output 保存工具调用已结算的最终结果，在 Run 结束前写入，通过 GetCompletion 交给 Agent。
	output *toolTaskOutput
	// err 保存取结果失败或 Cancel 写入的任务级错误，工具自身的业务错误留在 output 内，不写入此字段。
	err error
}

// newToolTask 创建单次工具任务。
func newToolTask(id agentTaskID, toolExecutor executor.ToolExecutor) agentTask {
	// 1. 保存身份和执行器。
	return &toolTaskImpl{id: id, toolExecutor: toolExecutor, status: agentTaskPending}
}

// Run 执行工具任务：准备已有结果时直接结算，否则令执行器向 Agent 直发事件（不由任务转发），再取得 Agent 按 CallID 合并的结果。
func (t *toolTaskImpl) Run(ctx context.Context, input agentTaskInput, events chan<- agentTaskEvent) (agentTaskRunResult, error) {
	// 1. 准备阶段已有结果时只结算，不执行工具或重发事件。
	t.status = agentTaskRunning
	toolInput := input.(*toolTaskInput)
	call := toolInput.Call
	finished := call.Finished
	if finished == nil {
		// 2. 直接向 Agent 发执行事件；执行结束后取回已合并结果。
		t.toolExecutor.Run(ctx, call, toolInput.ExecutorEvents)
		reply := make(chan agentTaskToolResult, 1)
		events <- &agentTaskToolResultRequested{TaskID: t.id, CallID: call.Call.ToolCall.CallID, Reply: reply}
		result := <-reply
		if result.Err != nil {
			t.status, t.err = agentTaskFailed, result.Err
			return agentTaskRunResult{}, result.Err
		}
		finished = result.Finished
	}
	// 3. 工具业务错误留在结果内，不作为任务错误返回。
	t.output = &toolTaskOutput{Finished: finished}
	t.status = agentTaskCompleted
	return agentTaskRunResult{}, nil
}

func (t *toolTaskImpl) GetID() agentTaskID               { return t.id }
func (t *toolTaskImpl) GetStatus() agentTaskStatus       { return t.status }
func (t *toolTaskImpl) GetWaitingTaskIDs() []agentTaskID { return nil }
func (t *toolTaskImpl) GetCompletion() agentTaskCompletion {
	return agentTaskCompletion{TaskID: t.id, Output: t.output, Err: t.err}
}
func (t *toolTaskImpl) Cancel(cause error) { t.status, t.err = agentTaskCanceled, cause }

// handleToolBatchFinished 汇总整批工具的目录规则，并按需读取成功环境的工作区规则。
func (a *agentImpl) handleToolBatchFinished(ctx context.Context, modelCallID defs.ModelCallID, results []*defs.ToolResult) error {
	// 1. 没有上下文时直接收尾；否则汇总有实际执行环境且成功返回的调用。
	if a.opt.Context == nil {
		return nil
	}
	var succeededExecutorIDs []string
	for _, result := range results {
		if result.Executor != "" && result.Error == nil {
			succeededExecutorIDs = append(succeededExecutorIDs, result.Executor)
		}
	}
	// 2. 未取消时读取成功环境的工作区规则；失败只记录日志。
	var workspaceInstructions []*defs.AgentsInstruction
	if ctx.Err() == nil {
		for _, environment := range a.opt.Environments {
			if !slices.Contains(succeededExecutorIDs, environment.GetID()) {
				continue
			}
			loaded, err := a.getWorkspaceAgentsInstructions(ctx, environment)
			if err != nil {
				telemetry.GetLogger(ctx).Warn("workspace agents instructions skipped", "openvaala.executor.id", environment.GetID(), "error", err)
				continue
			}
			workspaceInstructions = append(workspaceInstructions, loaded...)
		}
	}
	// 3. 依次追加工作区和工具返回的目录规则。
	instructions := slices.Concat(workspaceInstructions, a.toolAgentsInstructions)
	a.toolAgentsInstructions = nil
	return a.appendAgentsInstructions(instructions, modelCallID)
}

// rejectToolCalls 不执行工具调用，给每个调用保存一条错误结果，返回达到单轮上限的错误。
func (a *agentImpl) rejectToolCalls(calls []*defs.Item) error {
	// 1. 为每个调用创建错误结果 Item，沿用调用的 TurnID 和 ModelCallID。
	resultItems := make([]*defs.Item, 0, len(calls))
	for _, call := range calls {
		resultItems = append(resultItems, &defs.Item{
			ID:          defs.NewItemID(),
			TurnID:      call.TurnID,
			ModelCallID: call.ModelCallID,
			Kind:        defs.ItemToolResult,
			ToolResult:  defs.NewRejectedToolResult(call.ToolCall.CallID),
		})
	}
	// 2. 追加到上下文和草稿并发送事件。
	events, err := a.appendCompletedItems(resultItems...)
	if err != nil {
		return fmt.Errorf("append rejected tool results: %w", err)
	}
	a.sendEvents(events)
	return defs.ErrMaxStepsToolRequested
}

// handleExecutorEvent 合并工具进度、最终结果或 hook 更新并发送 Agent 事件。
func (a *agentImpl) handleExecutorEvent(ctx context.Context, event executor.ExecutorEvent) error {
	// 1. 按类别处理事件；停止后只跳过进度，仍合并最终结果和 hook。
	var events []*AgentEvent
	var err error
	switch event := event.(type) {
	case *executor.ExecutionProgressEvent:
		if ctx.Err() == nil {
			events, err = a.handleExecutionProgress(event)
		}
	case *executor.ExecutionFinishedEvent:
		if event.Result == nil {
			return errors.New("executor returned a nil tool result")
		}
		events, err = a.handleExecutionResult(event)
	case *executor.ExecutionHookRunEvent:
		if event.Run == nil {
			return errors.New("executor returned a nil hook run")
		}
		events = []*AgentEvent{{Type: AgentEventHookRun, HookRun: event.Run}}
	default:
		return fmt.Errorf("unknown execution event %T", event)
	}
	a.sendEvents(events)
	return err
}

// handleExecutionProgress 按 CallID 查找工具调用，返回带输出位置的进度事件。
func (a *agentImpl) handleExecutionProgress(
	progress *executor.ExecutionProgressEvent,
) ([]*AgentEvent, error) {
	// 1. 检查 CallID 并填写调用位置、输出流和进度内容。
	call, exists := a.toolCallsByCallID[progress.CallID]
	if !exists || call.finished != nil {
		return nil, fmt.Errorf("execution progress has unknown call ID %q", progress.CallID)
	}
	return []*AgentEvent{
		{
			Type:      AgentEventToolProgress,
			ItemIndex: a.draftItemIndexes[call.item],
			ToolProgress: &AgentToolProgress{
				CallID:  progress.CallID,
				Stream:  progress.Stream,
				Content: progress.Content,
			},
		},
	}, nil
}

// handleExecutionResult 把工具结果和它的 hook 记录依次追加为同一次模型调用的输出 Item。
func (a *agentImpl) handleExecutionResult(
	finished *executor.ExecutionFinishedEvent,
) ([]*AgentEvent, error) {
	// 1. 按 CallID 查找调用，累计工具调用数和失败数。
	result := finished.Result
	call, exists := a.toolCallsByCallID[result.CallID]
	if !exists {
		return nil, fmt.Errorf("execution result has unknown call ID %q", result.CallID)
	}
	if call.finished != nil {
		return nil, fmt.Errorf("duplicate execution result for call ID %q", result.CallID)
	}
	source := call.item
	a.turnStats.ToolCalls++
	if result.Error != nil {
		a.turnStats.FailedToolCalls++
	}
	// 2. 创建工具结果 Item，沿用调用的 TurnID 和 ModelCallID。
	resultItem := &defs.Item{
		ID:          defs.NewItemID(),
		TurnID:      source.TurnID,
		ModelCallID: source.ModelCallID,
		Kind:        defs.ItemToolResult,
		ToolResult:  result,
	}
	// 3. 将结果和 hook 记录追加到上下文、草稿，返回对应事件。
	outputItems := append([]*defs.Item{resultItem}, newHookRunItems(finished.HookRuns, source.TurnID, source.ModelCallID)...)
	events, err := a.appendCompletedItems(outputItems...)
	if err != nil {
		return nil, fmt.Errorf("append tool result %q: %w", result.CallID, err)
	}
	call.finished = finished
	a.toolCallsByCallID[result.CallID] = call
	a.toolAgentsInstructions = append(a.toolAgentsInstructions, finished.AgentsInstructions...)
	a.toolBatchModelCallID = source.ModelCallID
	return events, nil
}

// startToolTasks 为已准备的调用启动工具任务。
func (a *agentImpl) startToolTasks(ctx context.Context, calls []*executor.PreparedToolCall) agentTaskCreationResult {
	// 1. 取消后不派发工具。
	if err := ctx.Err(); err != nil {
		return agentTaskCreationResult{Err: err}
	}
	// 2. 为每个准备结果分配任务 ID。准备阶段已结束的调用也单独结算。
	ids := make([]agentTaskID, 0, len(calls))
	for _, call := range calls {
		a.nextTaskID++
		if call.Finished != nil {
			delete(a.toolCallsByCallID, call.Call.ToolCall.CallID)
		}
		a.startTask(ctx, newToolTask(a.nextTaskID, a.opt.ToolExecutor), &toolTaskInput{
			Call: call, ExecutorEvents: a.executorEvents,
		})
		ids = append(ids, a.nextTaskID)
	}
	return agentTaskCreationResult{TaskIDs: ids}
}

// takeToolResult 取出 Agent 已合并的工具结果，交还发起调用的工具任务。
func (a *agentImpl) takeToolResult(callID string) agentTaskToolResult {
	// 1. 只有收到最终事件并写入上下文的调用才有结果。
	call := a.toolCallsByCallID[callID]
	if call.finished == nil {
		return agentTaskToolResult{Err: fmt.Errorf("executor returned no result for tool call %q", callID)}
	}
	delete(a.toolCallsByCallID, callID)
	return agentTaskToolResult{Finished: call.finished}
}
