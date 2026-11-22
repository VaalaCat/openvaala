package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
)

// taskTestModel 只返回给定的模型事件，不连接外部模型。
type taskTestModel []llmkit.Event

func (m taskTestModel) Call(context.Context, llmkit.Request, llmkit.Target, llmkit.CallOptions) (<-chan llmkit.Event, error) {
	events := make(chan llmkit.Event, len(m))
	for _, event := range m {
		events <- event
	}
	close(events)
	return events, nil
}

// recordingTaskTestClient 记录目标，返回一次给定的事件流；再次请求时返回错误。
type recordingTaskTestClient struct {
	events  taskTestModel
	targets []string
}

// Call 记录模型目标并返回预设响应。
func (c *recordingTaskTestClient) Call(ctx context.Context, req llmkit.Request, target llmkit.Target, options llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 记录目标，首次请求发出事件流，后续返回错误。
	c.targets = append(c.targets, target.Model)
	if len(c.targets) > 1 {
		return nil, errors.New("unexpected second model call")
	}
	return c.events.Call(ctx, req, target, options)
}

// newTaskTestAgent 创建带真实上下文的 Agent，任务测试通过事件与它交换输入和结果。
func newTaskTestAgent(t *testing.T, client llmkit.Client) *agentImpl {
	t.Helper()
	turnID, sessionID := defs.TurnID("test-turn"), defs.SessionID("test-session")
	model := &defs.ModelDefinition{
		Ref: defs.ModelRef{ProviderID: "test", ModelID: "model"}, Target: &llmkit.Target{Model: "model"},
	}
	history, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(sessionID, turnID, client, model, model))
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Load(&agent_context.ContextLoadParam{Items: []*defs.Item{{
		ID: defs.NewItemID(), TurnID: turnID, Kind: defs.ItemMessage,
		Message: &defs.Message{From: &defs.Address{Kind: defs.IdentityUser}, Content: []*defs.Content{{
			Kind: defs.ContentText, Text: &defs.TextContent{Text: "original history"},
		}}},
	}}}); err != nil {
		t.Fatal(err)
	}
	instance, err := NewAgent(NewAgentOpt(turnID, sessionID, client, model, history))
	if err != nil {
		t.Fatal(err)
	}
	return instance.(*agentImpl)
}

// taskTestToolExecutor 从调用参数构造工具结果，可让指定调用返回业务失败。
type taskTestToolExecutor struct{ failedCallID string }

func (e taskTestToolExecutor) GetTools(context.Context) ([]*defs.ToolDefinition, error) {
	return nil, nil
}
func (e taskTestToolExecutor) GetToolsForEnvironments(ctx context.Context, _ []executor.ExecutionEnvironment, _ string) ([]*defs.ToolDefinition, error) {
	return e.GetTools(ctx)
}
func (e taskTestToolExecutor) PrepareCalls(_ context.Context, items []*defs.Item, _ chan<- executor.ExecutorEvent) []*executor.PreparedToolCall {
	calls := make([]*executor.PreparedToolCall, 0, len(items))
	for _, item := range items {
		calls = append(calls, &executor.PreparedToolCall{Call: item})
	}
	return calls
}
func (e taskTestToolExecutor) Run(_ context.Context, call *executor.PreparedToolCall, events chan<- executor.ExecutorEvent) {
	toolCall := call.Call.ToolCall
	result := &defs.ToolResult{CallID: toolCall.CallID, Content: []byte(toolCall.Arguments)}
	if result.CallID == e.failedCallID {
		result.Error = &defs.ToolError{Code: "not_found", Message: "no result"}
	}
	events <- &executor.ExecutionFinishedEvent{Result: result}
}

// TestGenerationTaskWaiting 检查生成任务交出等待关系，并用完整工具结果恢复同一个任务。
func TestGenerationTaskWaiting(t *testing.T) {
	for _, tt := range []struct {
		name             string
		finishBeforeWait bool
		failedCall       string
	}{
		{name: "normal"},
		{name: "already_finished", finishBeforeWait: true},
		{name: "tool_business_error", failedCall: "b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 1. 准备返回两个完整调用的模型与真实任务，接收并合并它们的事件。
				client := taskTestModel{
					{Type: llmkit.EventToolCallStart, ToolCall: &llmkit.StreamingToolCall{CallID: "a", Name: "lookup"}},
					{Type: llmkit.EventToolCallEnd, ToolCall: &llmkit.StreamingToolCall{CallID: "a", Name: "lookup", Arguments: `{"q":"first"}`}},
					{Type: llmkit.EventToolCallEnd, ToolCall: &llmkit.StreamingToolCall{CallID: "b", Name: "lookup", Arguments: `{"q":"second"}`}},
					{Type: llmkit.EventDone, FinishReason: "tool_calls"},
				}
				a := newTaskTestAgent(t, client)
				toolExecutor := taskTestToolExecutor{failedCallID: tt.failedCall}
				gen := newGenerationTask(1, &generationTaskOpt{Client: client, Model: a.opt.Model, ToolExecutor: toolExecutor, TurnID: a.opt.TurnID})
				a.opt.ToolExecutor = toolExecutor
				a.taskEvents = make(chan agentTaskEvent)
				a.executorEvents = make(chan executor.ExecutorEvent)
				a.tasks = make(map[agentTaskID]agentTask)
				a.runningTaskIDs = make(map[agentTaskID]struct{})
				a.nextTaskID = gen.GetID()
				ctx := t.Context()
				a.startTask(ctx, gen, &generationTaskInput{Start: &generationTaskStart{Step: 1}})
				var creation *agentTaskToolsRequested
				for creation == nil {
					event := <-a.taskEvents
					if request, ok := event.(*agentTaskToolsRequested); ok {
						creation = request
						break
					}
					if err := a.handleTaskEvent(ctx, event); err != nil {
						t.Fatal(err)
					}
				}
				// 2. 按指定顺序交给 Agent 处理工具返回和生成任务的等待登记。
				created := a.startToolTasks(ctx, creation.Calls)
				if created.Err != nil || !slices.Equal(created.TaskIDs, []agentTaskID{2, 3}) {
					t.Fatalf("created dependencies = %+v", created)
				}
				registerWait := func() {
					creation.Reply <- created
					close(creation.Reply)
					returned := (<-a.taskEvents).(*agentTaskRunReturnedEvent)
					if returned.Err != nil || returned.TaskID != 1 || !slices.Equal(returned.Result.WaitingOn, created.TaskIDs) {
						t.Fatalf("generation did not return with dependencies: %+v", returned)
					}
					if err := a.handleTaskRunReturned(ctx, returned); err != nil {
						t.Fatal(err)
					}
				}
				if !tt.finishBeforeWait {
					registerWait()
				}
				wantArguments := map[string]string{"a": `{"q":"first"}`, "b": `{"q":"second"}`}
				for returnedTools := 0; returnedTools < len(created.TaskIDs); {
					select {
					case event := <-a.executorEvents:
						if err := a.handleExecutorEvent(ctx, event); err != nil {
							t.Fatal(err)
						}
					case event := <-a.taskEvents:
						if returned, ok := event.(*agentTaskRunReturnedEvent); ok {
							if returned.Err != nil || !slices.Contains(created.TaskIDs, returned.TaskID) {
								t.Fatalf("unexpected tool return: %+v", returned)
							}
							completion := a.tasks[returned.TaskID].GetCompletion()
							result := completion.Output.(*toolTaskOutput).Finished.Result
							if string(result.Content) != wantArguments[result.CallID] || (result.Error != nil) != (result.CallID == tt.failedCall) {
								t.Fatalf("unexpected tool completion: %+v", result)
							}
							delete(wantArguments, result.CallID)
							returnedTools++
						}
						if err := a.handleTaskEvent(ctx, event); err != nil {
							t.Fatal(err)
						}
					}
				}
				if tt.finishBeforeWait {
					registerWait()
				}

				// 3. 等待结果齐备后，Agent 只恢复一次同一个生成任务。
				synctest.Wait()
				var batchEvent agentTaskEvent
				select {
				case batchEvent = <-a.taskEvents:
				default:
					t.Fatal("Agent did not resume generation after receiving both results")
				}
				batch, ok := batchEvent.(*agentTaskToolBatchFinishedRequest)
				if !ok || batch.TaskID != 1 || len(batch.Results) != 2 || batch.Results[0].CallID != "a" || batch.Results[1].CallID != "b" {
					t.Fatalf("resumed generation did not finish its tool batch: %+v", batchEvent)
				}
				if err := a.handleTaskEvent(ctx, batch); err != nil {
					t.Fatal(err)
				}
				second := (<-a.taskEvents).(*agentTaskRunReturnedEvent)
				if second.Err != nil || second.TaskID != 1 || len(second.Result.WaitingOn) != 0 {
					t.Fatalf("resumed generation = %+v", second)
				}
				a.taskRuns.Wait()
				output := gen.GetCompletion().Output.(*generationTaskOutput)
				if output.NextGeneration == nil || output.NextGeneration.Step != 2 || output.FinalModelCallID == "" {
					t.Fatalf("generation did not return the next step: %+v", output)
				}
			})
		})
	}
}
