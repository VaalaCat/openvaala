package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/VaalaCat/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestCompactionTaskResultAcknowledgement 检查摘要任务在 Agent 应用结果或决定不应用后才完成。
func TestCompactionTaskResultAcknowledgement(t *testing.T) {
	for _, tt := range []struct {
		name     string
		cancel   bool
		callFail bool
	}{
		{name: "applied_before_ack"},
		{name: "canceled_before_apply", cancel: true},
		{name: "failed_call_keeps_usage", callFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 1. 用真实上下文创建摘要请求，启动只接收该请求的压缩任务。
				end := llmkit.Event{Type: llmkit.EventDone, FinishReason: "stop"}
				if tt.callFail {
					end.Type = llmkit.EventToolCallEnd
				}
				client := taskTestModel{
					{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "compacted history"}},
					{Type: llmkit.EventUsage, Usage: &llmkit.Usage{PromptTokens: 17, CompletionTokens: 3}},
					end,
				}
				a := newTaskTestAgent(t, client)
				input, err := a.prepareCompaction(true, nil)
				if err != nil || input == nil {
					t.Fatalf("prepareCompaction = %v, %v", input, err)
				}
				input.Generation.Step = 1
				task := newCompactionTask(1)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				events := make(chan agentTaskEvent)
				returned := make(chan error, 1)
				go func() { _, err := task.Run(ctx, input, events); returned <- err }()

				// 2. 确认前置进度；收到模型结果后，先让 Agent 处理，暂不回 ACK。
				if err := a.handleTaskEvent(ctx, <-events); err != nil {
					t.Fatal(err)
				}
				request := (<-events).(*agentTaskCompactionFinishedRequest)
				if tt.cancel {
					cancel()
				}
				applyErr := a.handleCompactionFinished(ctx, request.Output, request.Err)
				synctest.Wait()
				if len(returned) != 0 {
					t.Error("compaction task returned without acknowledgement")
				}
				requestAfter, _, historyErr := a.opt.Context.BuildModelRequest(nil)
				request.Reply <- applyErr
				close(request.Reply)
				runErr := <-returned

				// 3. 只有成功摘要已修改历史；已发起的调用在取消或失败时也累计用量。
				wantSuccess := !tt.callFail && !tt.cancel
				if historyErr != nil || (runErr == nil) != wantSuccess {
					t.Fatalf("history error = %v, task error = %v", historyErr, runErr)
				}
				messages, err := json.Marshal(requestAfter.Messages)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(messages), "compacted history") != wantSuccess || strings.Contains(string(messages), "original history") == wantSuccess {
					t.Fatalf("history before ack = %s, success=%v", messages, wantSuccess)
				}
				if a.turnStats.InferenceCalls != 1 || a.turnStats.Steps != 0 || a.turnStats.Usage != (defs.TokenUsage{Input: 17, Output: 3}) {
					t.Fatalf("compaction stats = %+v", a.turnStats)
				}
			})
		})
	}
}

// TestAgentManualCompactionCancellation 检查手动摘要写入和取消的先后如何决定最终结果。
func TestAgentManualCompactionCancellation(t *testing.T) {
	appendErr := errors.New("summary append failed")
	for _, tt := range []struct {
		name             string
		cancelBeforeSave bool
		appendErr        error
		wantErr          error
	}{
		{name: "canceled_before_save", cancelBeforeSave: true, wantErr: context.Canceled},
		{name: "saved_before_cancel"},
		{name: "append_error_during_cancel", appendErr: appendErr, wantErr: appendErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 1. 使用真实上下文保存摘要，在发布调用报告时或写入结束后取消。
				a := newTaskTestAgent(t, taskTestModel{
					{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "compacted history"}},
					{Type: llmkit.EventDone, FinishReason: "stop"},
				})
				a.opt.IsCompactOnly = true
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if !tt.cancelBeforeSave {
					a.opt.Context = &cancelingCompactionContext{AgentContext: a.opt.Context, cancel: cancel, appendErr: tt.appendErr}
				}
				events := make(chan *AgentEvent)
				done := make(chan struct{})
				var result AgentResult
				var runErr error
				go func() { defer close(done); result, runErr = a.Run(ctx, events) }()
				if tt.cancelBeforeSave {
					<-events // 摘要草稿开始；Agent 接下来发布调用报告才会写入。
					synctest.Wait()
					cancel()
				}
				for range events {
				}
				<-done

				// 2. 写入成功时才有摘要和成功结果；真实写入错误不被取消覆盖。
				if !errors.Is(runErr, tt.wantErr) || ctx.Err() == nil || result.FinalModelCallID != "" {
					t.Fatalf("Run() error=%v, want %v; canceled=%v, final=%q", runErr, tt.wantErr, ctx.Err(), result.FinalModelCallID)
				}
				request, _, err := a.opt.Context.BuildModelRequest(nil)
				if err != nil {
					t.Fatal(err)
				}
				wantSuccess := tt.wantErr == nil
				if got := strings.Contains(request.Messages[len(request.Messages)-1].Content[0].Text, "compacted history"); got != wantSuccess || (len(result.Items) == 1) != wantSuccess {
					t.Fatalf("summary saved=%v, items=%v, want success=%v", got, result.Items, wantSuccess)
				}
			})
		})
	}
}

// cancelingCompactionContext 在摘要写入返回前取消，其他上下文操作照常执行。
type cancelingCompactionContext struct {
	agent_context.AgentContext
	cancel    context.CancelFunc
	appendErr error
}

func (c *cancelingCompactionContext) AppendItems(items ...*defs.Item) error {
	if len(items) == 1 && items[0].Kind == defs.ItemContextSummary {
		defer c.cancel()
		if c.appendErr != nil {
			return c.appendErr
		}
	}
	return c.AgentContext.AppendItems(items...)
}

// TestAgentCompactionUsesContextClient 检查独立装配时，手动和自动摘要均由 Context 的客户端发起。
func TestAgentCompactionUsesContextClient(t *testing.T) {
	for _, tt := range []struct {
		name              string
		manual            bool
		wantNormalTargets []string
	}{
		{name: "manual", manual: true},
		{name: "automatic", wantNormalTargets: []string{"normal"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 用两个独立的客户端分别提供普通回复和摘要，历史包含可压缩的前缀及应保留的后缀。
			normalClient := &recordingTaskTestClient{events: taskTestModel{
				{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "normal answer"}},
				{Type: llmkit.EventDone, FinishReason: "stop"},
			}}
			summaryClient := &recordingTaskTestClient{events: taskTestModel{
				{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "context summary"}},
				{Type: llmkit.EventDone, FinishReason: "stop"},
			}}
			turnID, sessionID := defs.TurnID("test-turn"), defs.SessionID("test-session")
			normalModel := &defs.ModelDefinition{
				Ref:           defs.ModelRef{ProviderID: "test", ModelID: "normal"},
				Target:        &llmkit.Target{Model: "normal"},
				ContextWindow: 100,
			}
			summaryModel := &defs.ModelDefinition{
				Ref:    defs.ModelRef{ProviderID: "test", ModelID: "summary"},
				Target: &llmkit.Target{Model: "summary"},
			}
			history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(sessionID, turnID, summaryClient, normalModel, summaryModel))
			if err != nil {
				t.Fatal(err)
			}
			var items []*defs.Item
			for _, text := range []string{strings.Repeat("old ", 200), strings.Repeat("recent ", 25)} {
				items = append(items, &defs.Item{
					ID:     defs.NewItemID(),
					TurnID: turnID,
					Kind:   defs.ItemMessage,
					Message: &defs.Message{
						From: &defs.Address{Kind: defs.IdentityUser},
						Content: []*defs.Content{{
							Kind: defs.ContentText,
							Text: &defs.TextContent{Text: text},
						}},
					},
				})
			}
			if err := history.Load(&agent_context.ContextLoadParam{Items: items}); err != nil {
				t.Fatal(err)
			}
			opt := NewAgentOpt(turnID, sessionID, normalClient, normalModel, history)
			opt.IsCompactOnly = tt.manual
			instance, err := NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 通过公开 Run，检查每个客户端收到的目标以及应用后的摘要。
			result, err := instance.Run(t.Context(), nil)
			if !slices.Equal(normalClient.targets, tt.wantNormalTargets) || !slices.Equal(summaryClient.targets, []string{"summary"}) {
				t.Fatalf("Run() error = %v, normal client targets = %v, summary client targets = %v", err, normalClient.targets, summaryClient.targets)
			}
			if err != nil || len(result.Items) != 1+len(tt.wantNormalTargets) || !history.IsCompacted() {
				t.Fatalf("Run() = %+v, %v, want applied summary", result, err)
			}
			summary := result.Items[0].ContextSummary
			if summary == nil || summary.Text != "context summary" || summary.CoveredThroughItemID != items[0].ID {
				t.Fatalf("summary = %+v, want text and coverage through %s", summary, items[0].ID)
			}
		})
	}
}
