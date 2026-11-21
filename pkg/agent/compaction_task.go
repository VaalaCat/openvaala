package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/failsafe-go/failsafe-go"
)

// compactionTaskImpl 生成摘要，并记录 Agent 已确认的结果。
type compactionTaskImpl struct {
	// id 是构造时分配的任务标识，随完成请求和 GetCompletion 返回。
	id agentTaskID
	// status 是摘要任务的阶段状态，由构造、Run 和 Cancel 更新，供 Agent 在 Run 外通过 GetStatus 读取。
	status agentTaskStatus
	// output 保存摘要模型调用结果和已生成摘要，在 Run 内写完后通过 GetCompletion 交给 Agent。
	output *compactionTaskOutput
	// err 保存 Run 失败或 Cancel 写入的错误，通过 GetCompletion 返回给 Agent。
	err error
}

// newCompactionTask 创建摘要任务。
func newCompactionTask(id agentTaskID) agentTask {
	// 1. 保存身份和待运行状态。
	return &compactionTaskImpl{
		id:     id,
		status: agentTaskPending,
	}
}

// Run 调用摘要模型并交回每次尝试的结果，等待 Agent 应用成功的摘要后结束。
func (c *compactionTaskImpl) Run(ctx context.Context, input agentTaskInput, events chan<- agentTaskEvent) (agentTaskRunResult, error) {
	// 1. 每次尝试生成独立摘要结果，交给 Agent 应用并记录调用用量。
	compactionInput := input.(*compactionTaskInput)
	c.status = agentTaskRunning
	var applyErr error
	err := failsafe.With(newModelRetryPolicy()).WithContext(ctx).Run(func() error {
		modelCallID := defs.NewModelCallID()
		generation := compactionInput.Generation
		beforeCall := make(chan error, 1)
		events <- &agentTaskSaveCheckpointRequested{
			Checkpoint: defs.TurnCheckpoint{
				TurnID: compactionInput.Summary.TurnID, Step: generation.Step, Phase: defs.TurnCheckpointPhaseRequest,
				StopHookContinuations: generation.StopHookContinuations, IsContinuedByStopHook: generation.IsContinuedByStopHook,
				Request: &defs.TurnRequestCheckpoint{
					Purpose: defs.ModelCallPurposeCompaction, ModelCallID: modelCallID,
					Compaction: &defs.TurnCompactionCheckpoint{
						SummaryItemID: compactionInput.Summary.ID, CoveredThroughItemID: compactionInput.Summary.ContextSummary.CoveredThroughItemID,
						CompactModelRef: compactionInput.Model.Ref,
					},
				},
			},
			Reply: beforeCall,
		}
		if applyErr = <-beforeCall; applyErr != nil {
			return nil
		}
		text, call, err := agent_context.GenerateSummary(ctx, modelCallID, compactionInput.Client, compactionInput.Model, compactionInput.Request, compactionInput.IsCompacted)
		output := &compactionTaskOutput{Call: call}
		if err == nil {
			output.Summary = compactionInput.Summary.Clone()
			output.Summary.ModelCallID = call.ModelCallID
			output.Summary.ContextSummary.Text = text
		}
		reply := make(chan error, 1)
		events <- &agentTaskCompactionFinishedRequest{TaskID: c.id, Output: *output, Err: err, Reply: reply}
		if applyErr = <-reply; applyErr != nil {
			return nil
		}
		c.output = output
		return err
	})
	// 2. 按摘要写入的回复结算；已应用的摘要不再受重试上下文取消影响。
	if applyErr != nil {
		err = applyErr
	} else if c.output != nil && c.output.Summary != nil {
		err = nil
	}
	if err != nil {
		c.err = fmt.Errorf("compact agent context: %w", err)
		c.status = agentTaskFailed
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			c.status = agentTaskCanceled
		}
		return agentTaskRunResult{}, c.err
	}
	c.status = agentTaskCompleted
	return agentTaskRunResult{}, nil
}

func (c *compactionTaskImpl) GetID() agentTaskID               { return c.id }
func (c *compactionTaskImpl) GetStatus() agentTaskStatus       { return c.status }
func (c *compactionTaskImpl) GetWaitingTaskIDs() []agentTaskID { return nil }
func (c *compactionTaskImpl) GetCompletion() agentTaskCompletion {
	return agentTaskCompletion{TaskID: c.id, Output: c.output, Err: c.err}
}
func (c *compactionTaskImpl) Cancel(cause error) { c.status, c.err = agentTaskCanceled, cause }

// prepareCompaction 确定本次摘要的历史与身份，再创建摘要草稿和模型请求。
func (a *agentImpl) prepareCompaction(isManual bool, resume *AgentResumeCompaction) (*compactionTaskInput, error) {
	// 1. 恢复时沿用已确认的输入、摘要 ID 和模型；首次压缩时测量并选择历史。
	var source []*defs.Item
	var summaryID defs.ItemID
	var compactModel *defs.ModelDefinition
	if resume != nil {
		source = resume.Source
		summaryID = resume.SummaryItemID
		compactModel = resume.Model
	} else {
		measurement, err := a.opt.Context.GetMeasurement(a.modelTools)
		if err != nil {
			return nil, fmt.Errorf("measure context before compaction: %w", err)
		}
		source = a.opt.Context.GetCompactionSource(measurement, isManual)
	}
	if len(source) == 0 {
		return nil, nil
	}
	if resume == nil {
		summaryID = defs.NewItemID()
	}
	// 2. 发送摘要草稿的开始事件，再获取摘要请求、模型和客户端。
	summary := &defs.Item{
		ID: summaryID, TurnID: a.opt.TurnID, Kind: defs.ItemContextSummary,
		ContextSummary: &defs.ContextSummary{CoveredThroughItemID: source[len(source)-1].ID},
	}
	a.compactionDraft = summary
	a.appendDraftItem(summary)
	a.sendEvents([]*AgentEvent{a.newItemEvent(AgentEventItemStarted, summary, nil)})
	request, model, client, err := a.opt.Context.BuildCompactionRequest(a.modelTools, source, compactModel)
	if err != nil {
		a.interruptCompactionDraft()
		return nil, fmt.Errorf("compact agent context: %w", err)
	}
	return &compactionTaskInput{
		Request:     request,
		Model:       model,
		Client:      client,
		Summary:     summary.Clone(),
		IsManual:    isManual,
		IsCompacted: a.opt.Context.IsCompacted(),
	}, nil
}

// handleCompactionFinished 累计一次摘要调用，并在成功时应用摘要、完成草稿。
func (a *agentImpl) handleCompactionFinished(ctx context.Context, output compactionTaskOutput, callErr error) error {
	// 1. 报告已发起的调用及用量，失败尝试不修改历史。
	call := output.Call
	a.turnStats.InferenceCalls++
	if call.Usage != nil {
		a.turnStats.Usage = a.turnStats.Usage.Add(*call.Usage)
	}
	a.sendEvents([]*AgentEvent{{
		Type: AgentEventModelCallFinished,
		ModelCall: &AgentModelCall{
			ModelCallID: call.ModelCallID,
			Purpose:     defs.ModelCallPurposeCompaction,
			Usage:       call.Usage,
		},
	}})
	if callErr != nil {
		return nil
	}
	// 2. 取消后不应用尚未生效的摘要；成功应用后完成原草稿。
	if err := ctx.Err(); err != nil {
		return err
	}
	summary := a.compactionDraft
	summary.ModelCallID = output.Summary.ModelCallID
	summary.ContextSummary.Text = output.Summary.ContextSummary.Text
	if err := a.opt.Context.AppendItems(summary); err != nil {
		return err
	}
	a.sendEvents([]*AgentEvent{a.finishItemWithReason(summary, AgentEventItemCompleted)})
	a.compactionDraft = nil
	return nil
}

// interruptCompactionDraft 发布未应用摘要的中断事件，并移除草稿。
func (a *agentImpl) interruptCompactionDraft() {
	// 1. 未应用的摘要仍在草稿末尾，发送无 Item 的结束事件后移除它。
	if summary := a.compactionDraft; summary != nil {
		a.sendEvents([]*AgentEvent{{Type: AgentEventItemInterrupted, ItemIndex: a.draftItemIndexes[summary]}})
		a.draftItems = a.draftItems[:len(a.draftItems)-1]
		delete(a.draftItemIndexes, summary)
		a.compactionDraft = nil
	}
}
