package agent

import (
	"testing"
	"testing/synctest"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestAgentModelCallIdentityAfterPublishing 检查公开报告脱敏后成功和失败的输出仍使用原调用 ID。
func TestAgentModelCallIdentityAfterPublishing(t *testing.T) {
	for _, tt := range []struct {
		name       string
		isComplete bool
	}{
		{name: "success", isComplete: true},
		{name: "incomplete_stream"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 1. 模型输出文本；成功时报告用量和结束事件，失败时断开流并在重试时返回错误。
				client := &recordingTaskTestClient{events: taskTestModel{{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "model output"},
				}}}
				if tt.isComplete {
					client.events = append(client.events, llmkit.Event{
						Type:         llmkit.EventDone,
						FinishReason: "stop",
						Usage:        &llmkit.Usage{PromptTokens: 100, CompletionTokens: 40},
					})
				}
				instance := newTaskTestAgent(t, client)
				events := make(chan *AgentEvent)
				finished := make(chan struct{})
				var result AgentResult
				var runErr error
				go func() {
					defer close(finished)
					result, runErr = instance.Run(t.Context(), events)
				}()

				// 2. 收到原调用报告时脱敏，接着消费后续的用量测量或草稿中断事件。
				var originalID defs.ModelCallID
				var finalMeasurement *AgentEvent
				for event := range events {
					switch event.Type {
					case AgentEventModelCallFinished:
						if originalID == "" {
							originalID = event.ModelCall.ModelCallID
							event.ModelCall.ModelCallID = "redacted"
							if event.ModelCall.ContextUsage != nil {
								event.ModelCall.ContextUsage.ModelCallID = "redacted"
							}
						}
					case AgentEventContextMeasured:
						finalMeasurement = event
					}
				}
				<-finished

				// 3. 对照原报告和本轮输出；成功时用量包含本次输出，失败时错误 Item 仍关联原调用。
				if originalID == "" || len(result.Items) == 0 || result.Items[0].ModelCallID != originalID {
					t.Fatalf("Run() = %+v, %v, want first output for %q", result, runErr, originalID)
				}
				if tt.isComplete {
					if runErr != nil || result.FinalModelCallID != originalID {
						t.Fatalf("final ID = %q, error = %v, want %q", result.FinalModelCallID, runErr, originalID)
					}
					if finalMeasurement == nil || finalMeasurement.Measurement.RequestTokens != 140 {
						t.Fatalf("final measurement = %+v, want 100 input + 40 output tokens", finalMeasurement)
					}
				} else if runErr == nil || len(result.Items) < 2 || result.Items[1].Kind != defs.ItemModelError || result.Items[1].ModelCallID != originalID {
					t.Fatalf("Run() = %+v, %v, want stream failure item for %q", result, runErr, originalID)
				}
			})
		})
	}
}

// TestAgentPartialReasoningRemainsVisibleBeforeCompletion 检查推理块只结束该块，整次调用结束后才发出回复事实。
func TestAgentPartialReasoningRemainsVisibleBeforeCompletion(t *testing.T) {
	for _, tt := range []struct {
		name string
		stop llmkit.Event
		want defs.ModelCallResult
	}{{"done", llmkit.Event{Type: llmkit.EventDone}, defs.ModelCallResultReply}, {"broken", llmkit.Event{Type: llmkit.EventError}, ""}} {
		t.Run(tt.name, func(t *testing.T) {
			client := &recordingTaskTestClient{events: taskTestModel{{Type: llmkit.EventReasoningDone, ReasoningStatus: llmkit.ReasoningCompleted, Reasoning: &llmkit.ReasoningContent{Summary: []string{"partial"}}}, tt.stop}}
			instance := newTaskTestAgent(t, client)
			events := make(chan *AgentEvent, 64)
			result, err := instance.Run(t.Context(), events)
			if (err == nil) != (tt.want != "") {
				t.Fatalf("Run=%+v err=%v", result, err)
			}
			var id defs.ModelCallID
			var gotCall bool
			for e := range events {
				if e.Type == AgentEventItemCompleted && e.Item.Reasoning != nil {
					id = e.Item.ModelCallID
					if gotCall || e.Item.ModelCallResult != "" || e.Item.Reasoning.Summary[0] != "partial" {
						t.Fatalf("premature completion: %+v", e)
					}
				}
				if e.Type == AgentEventModelCallFinished && e.ModelCall.ModelCallID == id {
					if e.ModelCall.Result != tt.want {
						t.Fatalf("result=%q want=%q", e.ModelCall.Result, tt.want)
					}
					gotCall = true
				}
			}
			if !gotCall {
				t.Fatal("did not observe reasoning before its call report")
			}
		})
	}
}
