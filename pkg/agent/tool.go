package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// execTools 执行一批工具调用、保存结果和目录规则，返回是否有 hook 要求结束本轮。
func (a *agentImpl) execTools(ctx context.Context, calls []*defs.Item) (bool, error) {
	// 1. 检查执行器和调用是否完整，按 CallID 建立索引并克隆调用；CallID 重复时返回错误。
	if a.opt.ToolExecutor == nil {
		return false, errors.New("agent received tool calls without an executor")
	}
	toolCallItemsByCallID := make(map[string]*defs.Item, len(calls))
	toolCalls := make([]*defs.Item, 0, len(calls))
	for _, item := range calls {
		if item == nil || item.ToolCall == nil || item.ToolCall.CallID == "" {
			return false, errors.New("tool execution requires complete tool call items")
		}
		callID := item.ToolCall.CallID
		if _, exists := toolCallItemsByCallID[callID]; exists {
			return false, fmt.Errorf("duplicate tool call ID %q", callID)
		}
		toolCallItemsByCallID[callID] = item
		toolCalls = append(toolCalls, item.Clone())
	}
	// 2. 启动工具执行器并处理事件；处理失败时取消执行，再收集剩余结果。
	executorCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	executorEvents := make(chan executor.ExecutorEvent)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		a.opt.ToolExecutor.Run(executorCtx, toolCalls, executorEvents)
	})

	finished, err := a.consumeExecutorEvents(executorEvents, toolCallItemsByCallID)
	if err != nil {
		cancel()
		remaining, cleanupErr := a.collectRemainingToolResults(executorEvents, toolCallItemsByCallID)
		finished = append(finished, remaining...)
		err = errors.Join(err, cleanupErr)
	}
	// 3. 等待执行器退出，汇总目录规则、成功的 Executor 和 hook 结束状态。
	tasks.Wait()
	var instructions []*defs.AgentsInstruction
	var succeededExecutorIDs []string
	isTurnStopped := false
	for _, event := range finished {
		instructions = append(instructions, event.AgentsInstructions...)
		if event.Result.Error == nil {
			succeededExecutorIDs = append(succeededExecutorIDs, event.Result.Executor)
		}
		// 按 hook 运行记录里有没有 stopped 判断是否结束本轮；PreToolUse 拦下调用并要求结束时，结果仍是 hook_blocked。
		if slices.ContainsFunc(event.HookRuns, func(run *defs.HookRun) bool { return run.Status == defs.HookRunStopped }) {
			isTurnStopped = true
		}
	}
	// 4. 没出错也没取消时，读取执行成功的环境的工作区规则；读取失败时记录日志并跳过。
	var workspaceInstructions []*defs.AgentsInstruction
	if err == nil && ctx.Err() == nil {
		for _, environment := range a.opt.Environments {
			if !slices.Contains(succeededExecutorIDs, environment.GetID()) {
				continue
			}
			loaded, loadErr := a.getWorkspaceAgentsInstructions(ctx, environment)
			if loadErr != nil {
				telemetry.GetLogger(ctx).Warn("workspace agents instructions skipped", "openvaala.executor.id", environment.GetID(), "error", loadErr)
				continue
			}
			workspaceInstructions = append(workspaceInstructions, loaded...)
		}
	}
	// 5. 追加工作区和工具返回的目录规则，一并返回执行和追加错误。
	return isTurnStopped, errors.Join(err, a.appendAgentsInstructions(slices.Concat(workspaceInstructions, instructions), calls[0].ModelCallID))
}

// consumeExecutorEvents 依次处理工具事件并发送 Agent 事件，返回已完成的执行记录。
func (a *agentImpl) consumeExecutorEvents(
	executorEvents <-chan executor.ExecutorEvent,
	toolCallItemsByCallID map[string]*defs.Item,
) ([]*executor.ExecutionFinishedEvent, error) {
	// 1. 处理每条执行事件，收集完成记录并发送对应 Agent 事件；处理失败时返回已有记录。
	var finished []*executor.ExecutionFinishedEvent
	for event := range executorEvents {
		agentEvents, err := a.handleExecutorEvent(event, toolCallItemsByCallID)
		if err != nil {
			return finished, err
		}
		if result, ok := event.(*executor.ExecutionFinishedEvent); ok {
			finished = append(finished, result)
		}
		a.sendEvents(agentEvents)
	}
	return finished, nil
}

// collectRemainingToolResults 在取消执行后读完剩余的工具事件，收集剩余结果和错误。
func (a *agentImpl) collectRemainingToolResults(
	executorEvents <-chan executor.ExecutorEvent,
	toolCallItemsByCallID map[string]*defs.Item,
) ([]*executor.ExecutionFinishedEvent, error) {
	// 1. 跳过进度，继续处理结果和 hook 事件，汇总处理错误和处理成功的完成记录。
	var cleanupErr error
	var finished []*executor.ExecutionFinishedEvent
	for event := range executorEvents {
		if _, progress := event.(*executor.ExecutionProgressEvent); progress {
			continue
		}
		_, err := a.handleExecutorEvent(event, toolCallItemsByCallID)
		cleanupErr = errors.Join(cleanupErr, err)
		if result, ok := event.(*executor.ExecutionFinishedEvent); ok && err == nil {
			finished = append(finished, result)
		}
	}
	return finished, cleanupErr
}

// handleExecutorEvent 把工具进度、完成或 hook 事件转换为 Agent 事件；完成事件的结果同时加入上下文和草稿。
func (a *agentImpl) handleExecutorEvent(
	event executor.ExecutorEvent,
	toolCallItemsByCallID map[string]*defs.Item,
) ([]*AgentEvent, error) {
	// 1. 按事件类别分发处理，空结果、空 hook 记录和未知事件返回错误。
	switch event := event.(type) {
	case *executor.ExecutionProgressEvent:
		return a.handleExecutionProgress(event, toolCallItemsByCallID)
	case *executor.ExecutionFinishedEvent:
		if event.Result == nil {
			return nil, errors.New("executor returned a nil tool result")
		}
		return a.handleExecutionResult(event, toolCallItemsByCallID)
	case *executor.ExecutionHookRunEvent:
		if event.Run == nil {
			return nil, errors.New("executor returned a nil hook run")
		}
		return []*AgentEvent{{
			Type:    AgentEventHookRun,
			HookRun: event.Run,
		}}, nil
	default:
		return nil, fmt.Errorf("unknown execution event %T", event)
	}
}

// handleExecutionProgress 按 CallID 查找工具调用，返回带输出位置的进度事件。
func (a *agentImpl) handleExecutionProgress(
	progress *executor.ExecutionProgressEvent,
	toolCallItemsByCallID map[string]*defs.Item,
) ([]*AgentEvent, error) {
	// 1. 检查 CallID 并填写调用位置、输出流和进度内容。
	callItem, exists := toolCallItemsByCallID[progress.CallID]
	if !exists {
		return nil, fmt.Errorf("execution progress has unknown call ID %q", progress.CallID)
	}
	return []*AgentEvent{
		{
			Type:      AgentEventToolProgress,
			ItemIndex: a.draftItemIndexes[callItem],
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
	toolCallItemsByCallID map[string]*defs.Item,
) ([]*AgentEvent, error) {
	// 1. 按 CallID 查找调用，累计工具调用数和失败数。
	result := finished.Result
	source, exists := toolCallItemsByCallID[result.CallID]
	if !exists {
		return nil, fmt.Errorf("execution result has unknown call ID %q", result.CallID)
	}
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
	return events, nil
}
