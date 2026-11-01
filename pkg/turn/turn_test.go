package turn_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/storage"
	storagefile "github.com/VaalaCat/openvaala/pkg/storage/file"
	storagesqlite "github.com/VaalaCat/openvaala/pkg/storage/sqlite"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/turn"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestTurnRun 检查输入、输出、运行状态和保存失败后的记录。
func TestTurnRun(t *testing.T) {
	// 1. 准备正常回复、模型错误及不同阶段的日志追加失败。
	tests := []struct {
		name         string
		protocol     llmkit.Protocol
		response     string
		statusCode   int
		holdResponse bool
		failLogAfter int
		wantStatus   defs.TurnStatus
		wantLogKinds []defs.ItemKind
		wantErr      string
		wantEvents   int
	}{
		{
			name:     "persists_input_and_completed_output",
			protocol: llmkit.ProtocolOpenAIChat,
			response: `data: {"choices":[{"delta":{"content":"完成。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`,
			wantStatus:   defs.TurnStatusCompleted,
			wantLogKinds: []defs.ItemKind{defs.ItemMessage, defs.ItemMessage},
			wantEvents:   1,
		},
		{
			name:         "persists_model_error_and_fails_turn",
			protocol:     llmkit.ProtocolOpenAIResponses,
			response:     `{"error":{"code":"invalid_request_error","message":"model capacity exhausted"}}`,
			statusCode:   http.StatusBadRequest,
			wantStatus:   defs.TurnStatusFailed,
			wantLogKinds: []defs.ItemKind{defs.ItemMessage, defs.ItemModelError},
			wantErr:      "model capacity exhausted",
			wantEvents:   1,
		},
		{
			name:     "log_failure_cancels_agent_and_fails_turn",
			protocol: llmkit.ProtocolOpenAIChat,
			response: `data: {"choices":[{"delta":{"content":"不能持久化。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`,
			failLogAfter: 1,
			wantStatus:   defs.TurnStatusRunning,
			wantLogKinds: []defs.ItemKind{defs.ItemMessage},
			wantErr:      "append agent item",
		},
		{
			name:     "later_log_failure_keeps_earlier_item",
			protocol: llmkit.ProtocolOpenAIChat,
			response: `data: {"choices":[{"delta":{"reasoning_content":"先思考。"}}]}

data: {"choices":[{"delta":{"content":"最终答案。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`,
			holdResponse: true,
			failLogAfter: 2,
			wantStatus:   defs.TurnStatusRunning,
			wantLogKinds: []defs.ItemKind{defs.ItemMessage, defs.ItemReasoning},
			wantErr:      "append agent item",
			wantEvents:   1,
		},
	}
	// 2. 运行模型及日志故障样例，比较输入、输出、Turn 状态、日志范围和事件数量。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			requestStopped := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					payload := map[string]any{}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					requests <- payload
					if tt.statusCode != 0 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tt.statusCode)
						_, _ = io.WriteString(w, tt.response)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tt.response)
					if tt.holdResponse {
						w.(http.Flusher).Flush()
						<-r.Context().Done()
						close(requestStopped)
					}
				}))
			defer server.Close()

			stores := openTurnStorage(t)
			if tt.failLogAfter != 0 {
				failingLogs := &failingLogStorage{
					LogStorage:      stores.logs,
					successfulLimit: tt.failLogAfter,
				}
				var err error
				stores.query, stores.mutation, err = dao.NewDAO(dao.NewDAOOpt(stores.records, failingLogs))
				if err != nil {
					t.Fatal(err)
				}
			}
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			turnOpt := turn.NewTurnOpt(
				conversationID,
				sessionID,
				&agent.AgentDefinition{
					Name:         "assistant",
					Instructions: "Answer briefly.",
				},
				stores.query,
				stores.mutation,
				llmkit.NewClient(llmkit.ClientOptions{}),
			)
			turnOpt.Model = newTestModel(&llmkit.Target{
				Protocol: tt.protocol,
				BaseURL:  server.URL,
				Model:    "turn-test",
			}, 0)
			turnInstance, err := newTestTurn(turnOpt)
			if err != nil {
				t.Fatal(err)
			}
			input := turnUserMessage("请处理这个请求。")
			events := make(chan turn.TurnEvent, 32)
			turnResult, runErr := turnInstance.Run(t.Context(), []*defs.Item{input}, events)
			if tt.wantErr == "" && runErr != nil {
				t.Fatal(runErr)
			}
			if tt.wantErr != "" && (runErr == nil || !strings.Contains(runErr.Error(), tt.wantErr)) {
				t.Fatalf("Run() error = %v, want %q", runErr, tt.wantErr)
			}
			record, found, err := stores.query.GetTurn(t.Context(), sessionID, turnOpt.TurnID)
			if err != nil || !found {
				t.Fatalf("GetTurn() = %+v, %v, %v", record, found, err)
			}
			if !slices.Equal(turnResult.InputItemIDs, []defs.ItemID{input.ID}) {
				t.Fatalf("InputItemIDs = %v, want [%s]", turnResult.InputItemIDs, input.ID)
			}
			if record.SessionID != sessionID || record.Status != tt.wantStatus {
				t.Fatalf("Turn = %+v, want session %q and status %q", record, sessionID, tt.wantStatus)
			}
			if record.FinalModelCallID != turnResult.FinalModelCallID || (record.FinalModelCallID != "") != (tt.wantStatus == defs.TurnStatusCompleted) {
				t.Fatalf("saved final model call = %q, result = %q, status = %q", record.FinalModelCallID, turnResult.FinalModelCallID, tt.wantStatus)
			}
			if tt.wantStatus == defs.TurnStatusFailed && !strings.Contains(record.Error, tt.wantErr) {
				t.Fatalf("Turn.Error = %q, want %q", record.Error, tt.wantErr)
			}

			logItems := readTurnLog(t, stores.query, conversationID, sessionID)
			wantLast := uint64(len(logItems))
			if tt.failLogAfter != 0 {
				wantLast = 0
				if !errors.Is(runErr, turn.ErrTurnUnsettled) {
					t.Fatalf("append failure was settled: %v", runErr)
				}
			}
			if record.ItemsRange != (defs.TurnItemRange{
				FirstOrdinal: 1,
				LastOrdinal:  wantLast,
			}) {
				t.Fatalf("range = %+v", record.ItemsRange)
			}
			gotKinds := make([]defs.ItemKind, len(logItems))
			for index, item := range logItems {
				gotKinds[index] = item.Kind
			}
			if fmt.Sprint(gotKinds) != fmt.Sprint(tt.wantLogKinds) {
				t.Fatalf("Log kinds = %v, want %v", gotKinds, tt.wantLogKinds)
			}
			if len(logItems) == 0 || logItems[0].ID != input.ID {
				t.Fatalf("Log input = %+v, want item %q", logItems, input.ID)
			}
			if logItems[0].TurnID != turnOpt.TurnID {
				t.Fatalf("input TurnID = %q, want %q", logItems[0].TurnID, turnOpt.TurnID)
			}
			for index, item := range logItems[1:] {
				if item.TurnID != turnOpt.TurnID {
					t.Fatalf("output %d TurnID = %q, want %q", index, item.TurnID, turnOpt.TurnID)
				}
			}
			if tt.wantStatus == defs.TurnStatusCompleted && (len(turnResult.Outputs) != 1 || turnResult.Outputs[0].Kind != defs.ItemMessage) {
				t.Fatalf("outputs = %+v, want one message", turnResult.Outputs)
			}

			request := <-requests
			inputCount := countJSONText(request, "请处理这个请求。")
			if inputCount != 1 {
				t.Fatalf("model input count = %d, want 1; request = %+v", inputCount, request)
			}
			if tt.holdResponse {
				select {
				case <-requestStopped:
				case <-time.After(time.Second):
					t.Fatal("Turn did not cancel the active model request after Log failure")
				}
			}
			var terminalEvents int
			close(events)
			for event := range events {
				agentEvent, ok := event.(*turn.TurnAgentEvent)
				if ok && (agentEvent.Event.Type == agent.AgentEventItemCompleted || agentEvent.Event.Type == agent.AgentEventItemInterrupted) {
					terminalEvents++
				}
			}
			if terminalEvents != tt.wantEvents {
				t.Fatalf("terminal AgentEvents = %d, want %d", terminalEvents, tt.wantEvents)
			}
		})
	}
}

// TestTurnFinalModelCallID 检查多段回复、空回复和工具 hook 结束时最终调用的保存及重读。
func TestTurnFinalModelCallID(t *testing.T) {
	// 1. 准备带中途正文和工具的调用，以及不同的最终输出。
	progressEvents := []llmkit.Event{
		{
			Type:  llmkit.EventContentDelta,
			Delta: &llmkit.DeltaPayload{Text: "progress"},
		},
		{Type: llmkit.EventContentBlockStop},
	}
	for _, callID := range []string{"call-a", "call-b"} {
		progressEvents = append(progressEvents, llmkit.Event{
			Type: llmkit.EventToolCallEnd,
			ToolCall: &llmkit.StreamingToolCall{
				CallID:    callID,
				Name:      "lookup",
				Arguments: `{}`,
			},
		})
	}
	progressEvents = append(progressEvents, llmkit.Event{Type: llmkit.EventDone})
	for _, tt := range []struct {
		name          string
		finalEvents   []llmkit.Event
		stopAfterTool bool
		wantMessages  []string
	}{
		{
			name: "multiple_final_messages",
			finalEvents: []llmkit.Event{
				{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "first"},
				},
				{Type: llmkit.EventContentBlockStop},
				{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "second"},
				},
				{Type: llmkit.EventDone},
			},
			wantMessages: []string{"first", "second"},
		},
		{
			name: "reasoning_only_final_keeps_its_call",
			finalEvents: []llmkit.Event{
				{
					Type:            llmkit.EventReasoningDone,
					ReasoningStatus: llmkit.ReasoningCompleted,
					Reasoning:       &llmkit.ReasoningContent{Content: []string{"thinking"}},
				},
				{Type: llmkit.EventDone},
			},
		},
		{
			name:        "empty_final_does_not_use_progress_call",
			finalEvents: []llmkit.Event{{Type: llmkit.EventDone}},
		},
		{
			name:          "tool_hook_stop_keeps_tool_call_id",
			stopAfterTool: true,
			wantMessages:  []string{"progress"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 运行真实 Turn 和 Agent，比较结果与已保存调用的身份。
			store := openTurnStorage(t)
			client := &usageTurnClient{calls: []usageTurnCall{{events: progressEvents}}}
			if !tt.stopAfterTool {
				client.calls = append(client.calls, usageTurnCall{events: tt.finalEvents})
			}
			conversationID, sessionID := defs.NewConversationID(), defs.NewSessionID()
			opt := turn.NewTurnOpt(conversationID, sessionID, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, client)
			opt.Model = newTestModel(&llmkit.Target{}, 0)
			opt.ToolExecutor = &reverseTurnExecutor{stopTurn: tt.stopAfterTool}
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(t.Context(), []*defs.Item{turnUserMessage("final call")}, nil)
			if err != nil || result.Status != defs.TurnStatusCompleted || result.FinalModelCallID == "" {
				t.Fatalf("Run() = %+v, %v", result, err)
			}
			if len(client.requests) != len(client.calls) {
				t.Fatalf("model calls = %d, want %d", len(client.requests), len(client.calls))
			}
			items := readTurnLog(t, store.query, conversationID, sessionID)
			if (items[1].ModelCallID == result.FinalModelCallID) != tt.stopAfterTool {
				t.Fatalf("progress call = %q, final = %q", items[1].ModelCallID, result.FinalModelCallID)
			}
			var messages []string
			for _, item := range items {
				if item.ModelCallID == result.FinalModelCallID && item.Kind == defs.ItemMessage {
					messages = append(messages, item.Message.Content[0].Text.Text)
				}
			}
			if !slices.Equal(messages, tt.wantMessages) {
				t.Fatalf("final messages = %v, want %v", messages, tt.wantMessages)
			}
			// 3. 重新打开存储，检查历史中的最终调用与本次返回相同。
			reloaded := openTurnStorageAt(t, store.root)
			record, found, err := reloaded.query.GetTurn(t.Context(), sessionID, opt.TurnID)
			if err != nil || !found || record.FinalModelCallID != result.FinalModelCallID {
				t.Fatalf("reloaded Turn = %+v, %v, %v; result final = %q", record, found, err, result.FinalModelCallID)
			}
		})
	}
}

// TestTurnItemsSaved 检查 Item 保存事件与实际日志的顺序。
func TestTurnItemsSaved(t *testing.T) {
	// 1. 准备普通回复、补充输入和输出追加失败样例。
	reply := []llmkit.Event{
		{
			Type:  llmkit.EventContentDelta,
			Delta: &llmkit.DeltaPayload{Text: "完成。"},
		},
		{Type: llmkit.EventContentBlockStop},
		{Type: llmkit.EventDone},
	}
	tests := []struct {
		name         string
		steer        bool
		failLogAfter int
		wantSaved    int
	}{
		{
			name:      "input_and_reply_saved_in_log_order",
			wantSaved: 2,
		},
		{
			name:      "steer_input_saved",
			steer:     true,
			wantSaved: 3,
		},
		{
			name:         "failed_append_is_not_reported",
			failLogAfter: 1,
			wantSaved:    1,
		},
	}
	// 2. 运行普通和补充输入流程，比较保存事件与日志的 ID、流水号和完成事件顺序。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stores := openTurnStorage(t)
			if tt.failLogAfter != 0 {
				failingLogs := &failingLogStorage{
					LogStorage:      stores.logs,
					successfulLimit: tt.failLogAfter,
				}
				var err error
				stores.query, stores.mutation, err = dao.NewDAO(dao.NewDAOOpt(stores.records, failingLogs))
				if err != nil {
					t.Fatal(err)
				}
			}
			inbox := &testTurnInbox{}
			var steer *defs.Item
			if tt.steer {
				steer = turnUserMessage("补充要求")
				inbox.add(steer)
			}
			opt := turn.NewTurnOpt(
				defs.NewConversationID(),
				defs.NewSessionID(),
				&agent.AgentDefinition{Name: "assistant"},
				stores.query,
				stores.mutation,
				&scriptedTurnClient{events: reply},
			)
			opt.Model = newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				Model:    "turn-test",
			}, 0)
			opt.Inbox = inbox
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan turn.TurnEvent, 64)
			_, runErr := instance.Run(t.Context(), []*defs.Item{turnUserMessage("请处理")}, events)
			if (runErr != nil) != (tt.failLogAfter != 0) {
				t.Fatalf("Run() error = %v", runErr)
			}
			close(events)

			var saved []dao.LoggedItem
			completedAt := map[defs.ItemID]int{}
			savedAt := map[defs.ItemID]int{}
			position := 0
			for event := range events {
				position++
				switch event := event.(type) {
				case *turn.TurnAgentEvent:
					if event.Event.Type == agent.AgentEventItemCompleted {
						completedAt[event.Event.Item.ID] = position
					}
				case *turn.TurnItemsSaved:
					for _, logged := range event.Items {
						saved = append(saved, logged)
						savedAt[logged.Item.ID] = position
					}
				}
			}
			logged := readTurnLog(t, stores.query, opt.ConversationID, opt.SessionID)
			if len(saved) != tt.wantSaved || len(logged) != tt.wantSaved {
				t.Fatalf("saved = %d, log = %d, want %d", len(saved), len(logged), tt.wantSaved)
			}
			for index, item := range logged {
				if saved[index].Ordinal != storage.Ordinal(index+1) || saved[index].Item.ID != item.ID {
					t.Fatalf("saved[%d] = %d %s, want %d %s", index, saved[index].Ordinal, saved[index].Item.ID, index+1, item.ID)
				}
			}
			for id, at := range completedAt {
				if savedAt[id] != 0 && savedAt[id] <= at {
					t.Fatalf("Item %s saved event at %d, completed at %d", id, savedAt[id], at)
				}
			}
			if tt.steer && !slices.ContainsFunc(saved, func(logged dao.LoggedItem) bool { return logged.Item.ID == steer.ID }) {
				t.Fatalf("steer %s not reported as saved", steer.ID)
			}
		})
	}
}

// TestTurnModelRetryHistory 检查重试尝试分别保存，重新加载时只发送成功历史。
func TestTurnModelRetryHistory(t *testing.T) {
	// 1. 准备流提前关闭和模型流错误的首次尝试。
	for _, tt := range []struct {
		name    string
		failure []llmkit.Event
	}{
		{name: "stream_closed_before_done"},
		{
			name: "stream_error",
			failure: []llmkit.Event{
				{
					Type: llmkit.EventError,
					Error: &llmkit.ErrorPayload{
						Code:    "overloaded",
						Message: "retry this stream",
					},
				},
			},
		},
	} {
		// 2. 逐例运行本轮和下一轮，检查本轮保存的各次尝试和下一轮的请求历史。
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 1. 让首次模型响应失败、重试成功，检查各次输出分别保存、请求历史相同和总用量。
				stores := openTurnStorage(t)
				failedEvents := []llmkit.Event{
					{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "failed completed block"},
					},
					{Type: llmkit.EventContentBlockStop},
					{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "failed partial block"},
					},
					{
						Type: llmkit.EventUsage,
						Usage: &llmkit.Usage{
							PromptTokens:     10,
							CompletionTokens: 2,
						},
					},
				}
				failedEvents = append(failedEvents, tt.failure...)
				client := &usageTurnClient{calls: []usageTurnCall{
					{events: failedEvents},
					{events: []llmkit.Event{
						{
							Type:  llmkit.EventContentDelta,
							Delta: &llmkit.DeltaPayload{Text: "successful answer"},
						},
						{
							Type: llmkit.EventUsage,
							Usage: &llmkit.Usage{
								PromptTokens:     12,
								CompletionTokens: 3,
							},
						},
						{
							Type:         llmkit.EventDone,
							FinishReason: "stop",
						},
					}},
				}}
				opt := turn.NewTurnOpt(defs.NewConversationID(), defs.NewSessionID(), &agent.AgentDefinition{Name: "assistant"}, stores.query, stores.mutation, client)
				opt.Model = newTestModel(&llmkit.Target{Model: "turn-test"}, 0)
				instance, err := newTestTurn(opt)
				if err != nil {
					t.Fatal(err)
				}
				events := make(chan turn.TurnEvent, 64)
				result, err := instance.Run(t.Context(), []*defs.Item{turnUserMessage("original input")}, events)
				if err != nil || result.Status != defs.TurnStatusCompleted || result.FinalModelCallID == "" {
					t.Fatalf("Run() = %+v, %v", result, err)
				}
				if len(client.requests) != 2 || !reflect.DeepEqual(client.requests[0], client.requests[1]) {
					t.Fatalf("retry requests changed: %+v", client.requests)
				}
				items := readTurnLog(t, stores.query, opt.ConversationID, opt.SessionID)
				var kinds []defs.ItemKind
				for _, item := range items {
					kinds = append(kinds, item.Kind)
				}
				wantKinds := []defs.ItemKind{defs.ItemMessage, defs.ItemMessage, defs.ItemMessage, defs.ItemModelError, defs.ItemMessage}
				if !slices.Equal(kinds, wantKinds) {
					t.Fatalf("log kinds = %v, want %v", kinds, wantKinds)
				}
				for index, text := range map[int]string{1: "failed completed block", 2: "failed partial block", 4: "successful answer"} {
					if got := items[index].Message.Content[0].Text.Text; got != text {
						t.Fatalf("log[%d] = %q, want %q", index, got, text)
					}
				}
				failedCallID := items[3].ModelCallID
				if failedCallID == "" || items[1].ModelCallID != failedCallID || items[2].ModelCallID != failedCallID || items[4].ModelCallID != result.FinalModelCallID || failedCallID == result.FinalModelCallID {
					t.Fatalf("model call IDs do not distinguish attempts: %+v", items)
				}
				record, found, err := stores.query.GetTurn(t.Context(), opt.SessionID, opt.TurnID)
				if err != nil || !found || record.Status != defs.TurnStatusCompleted || record.Error != "" || record.Usage != (defs.TokenUsage{
					Input:  22,
					Output: 5,
				}) || record.ItemsRange.LastOrdinal != uint64(len(items)) {
					t.Fatalf("persisted turn = %+v, %v, %v", record, found, err)
				}
				close(events)
				var calls []defs.ModelCallID
				var savedIDs []defs.ItemID
				for event := range events {
					switch event := event.(type) {
					case *turn.TurnAgentEvent:
						if event.Event.Type == agent.AgentEventModelCallFinished {
							calls = append(calls, event.Event.ModelCall.ModelCallID)
						}
					case *turn.TurnItemsSaved:
						for _, logged := range event.Items {
							savedIDs = append(savedIDs, logged.Item.ID)
						}
					}
				}
				if !slices.Equal(calls, []defs.ModelCallID{failedCallID, result.FinalModelCallID}) || len(savedIDs) != len(items) {
					t.Fatalf("calls = %v, saved IDs = %v", calls, savedIDs)
				}
				for index, item := range items {
					if savedIDs[index] != item.ID {
						t.Fatalf("saved order = %v", savedIDs)
					}
				}
				// 2. 重新打开存储并运行下一轮，检查请求只包含成功回复。
				if err := stores.database.Close(); err != nil {
					t.Fatal(err)
				}
				stores = openTurnStorageAt(t, stores.root)
				nextClient := &summaryTurnClient{}
				nextOpt := turn.NewTurnOpt(opt.ConversationID, opt.SessionID, opt.Agent, stores.query, stores.mutation, nextClient)
				nextOpt.Model = opt.Model
				next, err := newTestTurn(nextOpt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := next.Run(t.Context(), []*defs.Item{turnUserMessage("next input")}, nil); err != nil {
					t.Fatal(err)
				}
				payload, err := json.Marshal(nextClient.requests[0].Messages)
				if err != nil {
					t.Fatal(err)
				}
				for _, text := range []string{"original input", "successful answer", "next input"} {
					if !strings.Contains(string(payload), text) {
						t.Fatalf("request missing %q: %s", text, payload)
					}
				}
				for _, text := range []string{"failed completed block", "failed partial block", "retry this stream", "unexpected EOF"} {
					if strings.Contains(string(payload), text) {
						t.Fatalf("request contains failed output %q: %s", text, payload)
					}
				}
			})
		})
	}
}

// TestTurnModelRetryTermination 检查重试耗尽、等待期间取消和保存失败后的终态。
func TestTurnModelRetryTermination(t *testing.T) {
	// 1. 准备持续失败，以及定时取消或日志失败的样例。
	for _, tt := range []struct {
		name        string
		cancelAfter time.Duration
		failLog     bool
		wantCalls   int
		wantStatus  defs.TurnStatus
		wantErrors  int
	}{
		{
			name:       "exhausted",
			wantCalls:  11,
			wantStatus: defs.TurnStatusFailed,
			wantErrors: 11,
		},
		{
			name:        "cancel_during_thirty_second_backoff",
			cancelAfter: 32 * time.Second,
			wantCalls:   6,
			wantStatus:  defs.TurnStatusInterrupted,
			wantErrors:  6,
		},
		{
			name:       "item_write_failure_stops_retry",
			failLog:    true,
			wantCalls:  1,
			wantStatus: defs.TurnStatusRunning,
		},
	} {
		// 2. 在虚拟时间中运行到耗尽、取消或保存失败，检查调用次数、间隔和持久化终态。
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stores := openTurnStorage(t)
				if tt.failLog {
					var err error
					stores.query, stores.mutation, err = dao.NewDAO(dao.NewDAOOpt(stores.records, &failingLogStorage{
						LogStorage:      stores.logs,
						successfulLimit: 1,
					}))
					if err != nil {
						t.Fatal(err)
					}
				}
				client := &usageTurnClient{}
				for attempt := range 11 {
					client.calls = append(client.calls, usageTurnCall{events: []llmkit.Event{
						{
							Type:  llmkit.EventContentDelta,
							Delta: &llmkit.DeltaPayload{Text: fmt.Sprintf("partial attempt %d", attempt+1)},
						},
						{Type: llmkit.EventContentBlockStop},
					}})
				}
				opt := turn.NewTurnOpt(defs.NewConversationID(), defs.NewSessionID(), &agent.AgentDefinition{Name: "assistant"}, stores.query, stores.mutation, client)
				opt.Model = newTestModel(&llmkit.Target{Model: "turn-test"}, 0)
				instance, err := newTestTurn(opt)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.cancelAfter != 0 {
					defer time.AfterFunc(tt.cancelAfter, cancel).Stop()
				}
				started := time.Now()
				result, runErr := instance.Run(ctx, []*defs.Item{turnUserMessage("retry input")}, nil)
				if runErr == nil || result.FinalModelCallID != "" || len(client.requests) != tt.wantCalls {
					t.Fatalf("Run() = %+v, %v; calls = %d, want %d", result, runErr, len(client.requests), tt.wantCalls)
				}
				record, found, err := stores.query.GetTurn(t.Context(), opt.SessionID, opt.TurnID)
				if err != nil || !found || record.Status != tt.wantStatus {
					t.Fatalf("turn = %+v, %v, %v", record, found, err)
				}
				items := readTurnLog(t, stores.query, opt.ConversationID, opt.SessionID)
				modelErrors := 0
				for _, item := range items {
					if item.Kind == defs.ItemModelError {
						modelErrors++
					}
				}
				if modelErrors != tt.wantErrors {
					t.Fatalf("model errors = %d, want %d", modelErrors, tt.wantErrors)
				}
				switch {
				case tt.failLog:
					if !errors.Is(runErr, turn.ErrTurnUnsettled) || !strings.Contains(runErr.Error(), "append agent item") || len(items) != 1 {
						t.Fatalf("write failure = %v, log = %+v", runErr, items)
					}
				case tt.cancelAfter != 0:
					if !errors.Is(runErr, context.Canceled) || result.Status != defs.TurnStatusInterrupted || items[len(items)-1].Kind != defs.ItemTurnInterrupted || time.Since(started) != tt.cancelAfter {
						t.Fatalf("cancellation = %+v, %v, elapsed %v", result, runErr, time.Since(started))
					}
				default:
					if !errors.Is(runErr, io.ErrUnexpectedEOF) || result.Status != defs.TurnStatusFailed || len(items) != 23 || time.Since(started) != 181*time.Second {
						t.Fatalf("exhaustion = %+v, %v, %d items, elapsed %v", result, runErr, len(items), time.Since(started))
					}
				}
				for index := 1; index < len(client.callTimes); index++ {
					want := min(time.Second<<(index-1), 30*time.Second)
					if got := client.callTimes[index].Sub(client.callTimes[index-1]); got != want {
						t.Fatalf("delay %d = %v, want %v", index, got, want)
					}
				}
			})
		})
	}
}

// TestTurnTelemetryExportFailure 检查遥测导出失败时本轮仍正常保存。
func TestTurnTelemetryExportFailure(t *testing.T) {
	// 1. 分别准备导出成功和导出失败的遥测处理器。
	for _, tt := range []struct {
		name       string
		failExport bool
	}{
		{name: "export_succeeds"},
		{
			name:       "export_fails",
			failExport: true,
		},
	} {
		// 2. 运行 Turn，检查导出错误未改变 completed、回复落盘和模型调用次数。
		t.Run(tt.name, func(t *testing.T) {
			pipeline := telemetrytest.NewPipeline(t)
			exporter := &turnTestExporter{fail: tt.failExport}
			pipeline.TracerProvider.RegisterSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter))
			stores := openTurnStorage(t)
			client := &usageTurnClient{calls: []usageTurnCall{
				{events: []llmkit.Event{
					{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "answer despite exporter failure"},
					},
					{
						Type:         llmkit.EventDone,
						FinishReason: "stop",
					},
				}},
			}}
			opt := turn.NewTurnOpt(defs.NewConversationID(), defs.NewSessionID(), &agent.AgentDefinition{Name: "assistant"}, stores.query, stores.mutation, client)
			opt.Model = newTestModel(&llmkit.Target{Model: "turn-test"}, 0)
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(telemetry.ContextWithTelemetry(t.Context(), pipeline.Telemetry), []*defs.Item{turnUserMessage("input")}, nil)
			if err != nil || result.Status != defs.TurnStatusCompleted || len(client.requests) != 1 || exporter.calls == 0 {
				t.Fatalf("Run() = %+v, %v; model calls %d, exports %d", result, err, len(client.requests), exporter.calls)
			}
			items := readTurnLog(t, stores.query, opt.ConversationID, opt.SessionID)
			if len(items) != 2 || items[1].Message.Content[0].Text.Text != "answer despite exporter failure" {
				t.Fatalf("log = %+v", items)
			}
		})
	}
}

// turnTestExporter 统计 span 导出次数，可按配置返回导出错误。
type turnTestExporter struct {
	fail  bool
	calls int
}

// ExportSpans 累计导出次数，并按配置返回导出错误。
func (exporter *turnTestExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	// 1. 累计次数，配置失败时返回测试错误，否则正常返回。
	exporter.calls++
	if exporter.fail {
		return errors.New("injected telemetry export failure")
	}
	return nil
}

// Shutdown 正常结束测试导出器。
func (*turnTestExporter) Shutdown(context.Context) error {
	// 1. 返回成功。
	return nil
}

// TestTurnTerminalStatus 检查完成、模型失败和取消时的持久化及遥测状态。
func TestTurnTerminalStatus(t *testing.T) {
	// 1. 准备 completed、failed、interrupted 的运行样例。
	tests := []struct {
		name          string
		cancelRun     bool
		failModel     bool
		wantStatus    defs.TurnStatus
		wantErrorType string
		wantSpanError bool
	}{
		{
			name:       "completed",
			wantStatus: defs.TurnStatusCompleted,
		},
		{
			name:          "model_failure",
			failModel:     true,
			wantStatus:    defs.TurnStatusFailed,
			wantErrorType: "_OTHER",
			wantSpanError: true,
		},
		// 中断样例记录 canceled，span 状态为 Unset。
		{
			name:          "canceled",
			cancelRun:     true,
			wantStatus:    defs.TurnStatusInterrupted,
			wantErrorType: "canceled",
		},
	}
	// 2. 按样例完成、失败或取消运行，比较数据库状态和调用 span 的错误类型。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			stores := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			var client llmkit.Client = &scriptedTurnClient{events: []llmkit.Event{{Type: llmkit.EventDone}}}
			var blocking *blockingTurnClient
			if tt.failModel {
				client = &failingTurnClient{}
			}
			if tt.cancelRun {
				blocking = &blockingTurnClient{started: make(chan struct{})}
				client = blocking
			}
			turnOpt := turn.NewTurnOpt(
				conversationID,
				sessionID,
				&agent.AgentDefinition{Name: "assistant"},
				stores.query,
				stores.mutation,
				client,
			)
			turnOpt.Model = newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				Model:    "turn-test",
			}, 0)
			turnInstance, err := newTestTurn(turnOpt)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			completed := make(chan error, 1)
			var turnResult turn.TurnResult
			go func() {
				var err error
				turnResult, err = turnInstance.Run(telemetry.ContextWithTelemetry(ctx, p.Telemetry), []*defs.Item{turnUserMessage("status")}, nil)
				completed <- err
			}()
			if tt.cancelRun {
				<-blocking.started
				cancel()
			}
			runErr := <-completed
			if tt.wantStatus == defs.TurnStatusCompleted && runErr != nil {
				t.Fatal(runErr)
			}
			if tt.wantStatus != defs.TurnStatusCompleted && runErr == nil {
				t.Fatal("Run() error = nil")
			}
			record, found, err := stores.query.GetTurn(t.Context(), sessionID, turnOpt.TurnID)
			if err != nil || !found || record.Status != tt.wantStatus {
				t.Fatalf("GetTurn() = %+v, %v, %v, want status %q", record, found, err, tt.wantStatus)
			}
			if record.FinalModelCallID != turnResult.FinalModelCallID || (record.FinalModelCallID != "") != (tt.wantStatus == defs.TurnStatusCompleted) {
				t.Fatalf("saved final model call = %q, result = %q, status = %q", record.FinalModelCallID, turnResult.FinalModelCallID, tt.wantStatus)
			}

			span := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
			if got := telemetrytest.GetSpanAttribute(span, "openvaala.turn.status"); got != string(tt.wantStatus) {
				t.Errorf("openvaala.turn.status = %q, want %q", got, tt.wantStatus)
			}
			if got := telemetrytest.GetSpanAttribute(span, "error.type"); got != tt.wantErrorType {
				t.Errorf("error.type = %q, want %q", got, tt.wantErrorType)
			}
			if got := span.Status().Code == codes.Error; got != tt.wantSpanError {
				t.Errorf("span status = %v, want error = %v", span.Status(), tt.wantSpanError)
			}
		})
	}
}

// TestTurnPersistsUsage 检查不同结束状态下的累计用量保存。
func TestTurnPersistsUsage(t *testing.T) {
	// 1. 准备两次工具调用和上报用量的模型响应。
	toolCallsStep := []llmkit.Event{
		{
			Type: llmkit.EventToolCallEnd,
			ToolCall: &llmkit.StreamingToolCall{
				CallID:    "call-a",
				Name:      "lookup",
				Arguments: `{}`,
			},
		},
		{
			Type: llmkit.EventToolCallEnd,
			ToolCall: &llmkit.StreamingToolCall{
				CallID:    "call-b",
				Name:      "lookup",
				Arguments: `{}`,
			},
		},
		{
			Type: llmkit.EventUsage,
			Usage: &llmkit.Usage{
				PromptTokens:     100,
				CompletionTokens: 20,
			},
		},
		{
			Type:         llmkit.EventDone,
			FinishReason: "tool_calls",
		},
	}
	tests := []struct {
		name           string
		calls          []usageTurnCall
		cancelRun      bool
		failInputWrite bool
		// previousModel 是上一轮使用的模型；与本轮不同时，上一轮的报告不再沿用。
		previousModel defs.ModelRef
		wantStatus    defs.TurnStatus
		wantUsage     defs.TokenUsage
		// wantOwnContextReport 本轮是否产生了自己的完整普通调用报告？没有时落盘沿用上一轮的报告。
		wantOwnContextReport bool
	}{
		{
			name: "completed",
			calls: []usageTurnCall{{events: []llmkit.Event{
				{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "完成"},
				},
				{
					Type: llmkit.EventUsage,
					Usage: &llmkit.Usage{
						PromptTokens:     100,
						CompletionTokens: 20,
					},
				},
				{
					Type:         llmkit.EventDone,
					FinishReason: "stop",
				},
			}}},
			wantStatus:           defs.TurnStatusCompleted,
			wantOwnContextReport: true,
			wantUsage: defs.TokenUsage{
				Input:  100,
				Output: 20,
			},
		},
		{
			name:                 "failed_after_first_call_keeps_its_usage",
			calls:                []usageTurnCall{{events: toolCallsStep}, {err: errors.New("injected second call failure")}},
			wantStatus:           defs.TurnStatusFailed,
			wantOwnContextReport: true,
			wantUsage: defs.TokenUsage{
				Input:  100,
				Output: 20,
			},
		},
		{
			name:       "failed_first_call_keeps_previous_report",
			calls:      []usageTurnCall{{err: errors.New("injected first call failure")}},
			wantStatus: defs.TurnStatusFailed,
		},
		{
			name:          "model_changed_discards_previous_report",
			calls:         []usageTurnCall{{err: errors.New("injected first call failure")}},
			previousModel: defs.ModelRef{ProviderID: "test", ModelID: "previous-model"},
			wantStatus:    defs.TurnStatusFailed,
		},
		{
			name: "interrupted_without_reported_usage",
			calls: []usageTurnCall{{
				events: []llmkit.Event{{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "partial"},
				}},
				block: true,
			}},
			cancelRun:  true,
			wantStatus: defs.TurnStatusInterrupted,
		},
		{
			name: "interrupted_reported_usage_without_complete_response",
			calls: []usageTurnCall{{events: []llmkit.Event{
				{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "partial"}},
				{Type: llmkit.EventUsage, Usage: &llmkit.Usage{PromptTokens: 70, CompletionTokens: 8}},
			}, block: true}},
			cancelRun:  true,
			wantStatus: defs.TurnStatusInterrupted,
			wantUsage:  defs.TokenUsage{Input: 70, Output: 8},
		},
		{
			name:                 "interrupted_keeps_usage_of_finished_calls",
			wantOwnContextReport: true,
			calls: []usageTurnCall{
				{events: toolCallsStep},
				{
					events: []llmkit.Event{{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "partial"},
					}},
					block: true,
				},
			},
			cancelRun:  true,
			wantStatus: defs.TurnStatusInterrupted,
			wantUsage: defs.TokenUsage{
				Input:  100,
				Output: 20,
			},
		},
		{
			name:           "failed_before_agent_runs",
			failInputWrite: true,
			wantStatus:     defs.TurnStatusRunning,
		},
	}
	// 2. 执行工具后失败、取消及输入保存失败，检查 Turn 记录保留已上报的累计用量。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stores := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			input := turnUserMessage("usage")
			var mutation dao.Mutation = stores.mutation
			if tt.failInputWrite {
				mutation = &failingItemMutation{
					Mutation:   stores.mutation,
					failItemID: input.ID,
				}
			}
			client := &usageTurnClient{
				calls:   tt.calls,
				blocked: make(chan struct{}),
			}
			turnOpt := turn.NewTurnOpt(
				conversationID,
				sessionID,
				&agent.AgentDefinition{Name: "assistant"},
				stores.query,
				mutation,
				client,
			)
			turnOpt.Model = newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				Model:    "turn-test",
			}, 0)
			turnOpt.ToolExecutor = &reverseTurnExecutor{}
			previousContextUsage := &defs.TurnContextUsage{
				ModelCallID:        defs.NewModelCallID(),
				InputThroughItemID: "missing-old-input",
				Usage:              defs.TokenUsage{Input: 9000, Output: 1000},
				ReasoningContext:   llmkit.ReasoningContextAllTurns,
			}
			turnOpt.PreviousContextUsage = previousContextUsage
			turnOpt.PreviousModel = tt.previousModel
			instance, err := newTestTurn(turnOpt)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			completed := make(chan error, 1)
			events := make(chan turn.TurnEvent, 128)
			var result turn.TurnResult
			go func() {
				var err error
				result, err = instance.Run(ctx, []*defs.Item{input}, events)
				close(events)
				completed <- err
			}()
			if tt.cancelRun {
				<-client.blocked
				cancel()
			}
			runErr := <-completed
			if (runErr != nil) != (tt.wantStatus != defs.TurnStatusCompleted) {
				t.Fatalf("Run() error = %v, want status %q", runErr, tt.wantStatus)
			}
			record, found, err := stores.query.GetTurn(t.Context(), sessionID, turnOpt.TurnID)
			if err != nil || !found {
				t.Fatalf("GetTurn() = %+v, %v, %v", record, found, err)
			}
			if record.Status != tt.wantStatus || record.Usage != tt.wantUsage {
				t.Fatalf("Turn = status %q usage %+v, want status %q usage %+v", record.Status, record.Usage, tt.wantStatus, tt.wantUsage)
			}
			// 3. 本轮有完整普通调用时落盘它的报告；没有时沿用上一轮的报告，模型切换后为空。
			var ownContextUsage *defs.TurnContextUsage
			for event := range events {
				agentEvent, ok := event.(*turn.TurnAgentEvent)
				if ok && agentEvent.Event.Type == agent.AgentEventModelCallFinished && agentEvent.Event.ModelCall.ContextUsage != nil {
					ownContextUsage = agentEvent.Event.ModelCall.ContextUsage
				}
			}
			wantContextUsage, wantResultContextUsage := previousContextUsage, previousContextUsage
			if tt.previousModel != (defs.ModelRef{}) {
				wantContextUsage, wantResultContextUsage = nil, nil
			}
			if ownContextUsage != nil {
				wantContextUsage, wantResultContextUsage = ownContextUsage, ownContextUsage
			}
			// 输入保存失败时本轮没有结算，结果不带报告，已保存的 running 记录仍带着沿用的报告。
			if tt.failInputWrite {
				wantResultContextUsage = nil
			}
			if (ownContextUsage != nil) != tt.wantOwnContextReport {
				t.Fatalf("own context usage = %+v, want report %v", ownContextUsage, tt.wantOwnContextReport)
			}
			if !reflect.DeepEqual(record.ContextUsage, wantContextUsage) || !reflect.DeepEqual(result.ContextUsage, wantResultContextUsage) {
				t.Fatalf("saved context usage = %+v, returned = %+v, want %+v / %+v", record.ContextUsage, result.ContextUsage, wantContextUsage, wantResultContextUsage)
			}
			if ownContextUsage != nil && record.ContextUsage.InputThroughItemID != input.ID {
				t.Fatalf("input boundary = %q, want %q", record.ContextUsage.InputThroughItemID, input.ID)
			}
		})
	}
}

// TestTurnLatestContextUsage 检查最后完整普通响应替换旧模式和用量，且不增加逐调用保存。
func TestTurnLatestContextUsage(t *testing.T) {
	// 1. 先完成带已知模式和用量的工具调用，再返回不同模式和用量的普通响应。
	for _, tt := range []struct {
		name                   string
		mode                   llmkit.ReasoningContext
		hasUsage, failTerminal bool
	}{
		{name: "unknown_replaces_known", hasUsage: true},
		{name: "known_without_usage", mode: llmkit.ReasoningContextCurrentTurn},
		{name: "unknown_without_usage"},
		{name: "terminal_failure_remains_unsettled", hasUsage: true, failTerminal: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stores := openTurnStorage(t)
			conversationID, sessionID := defs.NewConversationID(), defs.NewSessionID()
			first := []llmkit.Event{
				{Type: llmkit.EventToolCallEnd, ToolCall: &llmkit.StreamingToolCall{CallID: "call-a", Name: "lookup", Arguments: `{}`}},
				{Type: llmkit.EventToolCallEnd, ToolCall: &llmkit.StreamingToolCall{CallID: "call-b", Name: "lookup", Arguments: `{}`}},
				{Type: llmkit.EventUsage, Usage: &llmkit.Usage{PromptTokens: 100, CompletionTokens: 20}},
				{Type: llmkit.EventDone, FinishReason: "tool_calls", ReasoningContext: llmkit.ReasoningContextAllTurns},
			}
			last := []llmkit.Event{{Type: llmkit.EventContentDelta, Delta: &llmkit.DeltaPayload{Text: "last reply"}}}
			if tt.hasUsage {
				last = append(last, llmkit.Event{Type: llmkit.EventUsage, Usage: &llmkit.Usage{PromptTokens: 140, CompletionTokens: 30}})
			}
			last = append(last, llmkit.Event{Type: llmkit.EventDone, FinishReason: "stop", ReasoningContext: tt.mode})
			client := &usageTurnClient{calls: []usageTurnCall{{events: first}, {events: last}}}
			mutation := &failingTurnMutation{Mutation: stores.mutation, failTerminal: tt.failTerminal}
			opt := turn.NewTurnOpt(conversationID, sessionID, &agent.AgentDefinition{Name: "assistant"}, stores.query, mutation, client)
			opt.Model = newTestModel(&llmkit.Target{Protocol: llmkit.ProtocolOpenAIChat, Model: "turn-test"}, 0)
			opt.ToolExecutor = &reverseTurnExecutor{}
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 运行真实模型和工具主链，核对最新报告与原有累计计费分别保存。
			result, err := instance.Run(t.Context(), []*defs.Item{turnUserMessage("run tools")}, nil)
			if errors.Is(err, turn.ErrTurnUnsettled) != tt.failTerminal {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			if err != nil && !tt.failTerminal {
				t.Fatal(err)
			}
			record, found, err := stores.query.GetTurn(t.Context(), sessionID, opt.TurnID)
			if err != nil || !found {
				t.Fatalf("GetTurn = %+v, %v", record, err)
			}
			if tt.failTerminal {
				if record.Status != defs.TurnStatusRunning || result.ContextUsage != nil {
					t.Fatalf("unsettled = %+v, result %+v", record, result)
				}
			} else {
				if record.ContextUsage == nil || record.ContextUsage.ModelCallID != result.FinalModelCallID || record.ContextUsage.InputThroughItemID == "" || record.ContextUsage.ReasoningContext != tt.mode || (record.ContextUsage.Usage != defs.TokenUsage{}) != tt.hasUsage {
					t.Fatalf("last response report = %+v", record.ContextUsage)
				}
				want := defs.TokenUsage{Input: 100, Output: 20}
				if tt.hasUsage {
					want = want.Add(defs.TokenUsage{Input: 140, Output: 30})
				}
				if record.Usage != want || !reflect.DeepEqual(record.ContextUsage, result.ContextUsage) {
					t.Fatalf("record = %+v, result = %+v", record, result)
				}
			}
			// 3. 两次普通调用仍只写 running、首批范围和终态三条 metadata。
			page, err := stores.logs.List(t.Context(), storage.ListLogEntriesRequest{Path: storage.StreamPath(conversationID), StreamID: dao.MetadataStreamID, Order: storage.DirectionAsc, Limit: 100})
			wantWrites := 3
			if tt.failTerminal {
				wantWrites = 2
			}
			if err != nil || len(page.Items) != wantWrites {
				t.Fatalf("metadata writes = %d, want %d, err %v", len(page.Items), wantWrites, err)
			}
		})
	}
}

// TestTurnUnsettled 检查 running 或终态保存失败时返回未结算错误。
func TestTurnUnsettled(t *testing.T) {
	// 1. 准备首次记录失败和结束记录失败的存储样例。
	tests := []struct {
		name           string
		failRunning    bool
		failTerminal   bool
		wantUnsettled  bool
		wantPersistent bool
	}{
		{
			name:          "initial_running_write",
			failRunning:   true,
			wantUnsettled: true,
		},
		{
			name:           "terminal_write",
			failTerminal:   true,
			wantUnsettled:  true,
			wantPersistent: true,
		},
	}
	// 2. 注入 running 或终态保存错误，检查返回空终态及 ErrTurnUnsettled，并读取已有记录。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stores := openTurnStorage(t)
			mutation := &failingTurnMutation{
				Mutation:     stores.mutation,
				failRunning:  tt.failRunning,
				failTerminal: tt.failTerminal,
			}
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			turnOpt := turn.NewTurnOpt(
				conversationID,
				sessionID,
				&agent.AgentDefinition{Name: "assistant"},
				stores.query,
				mutation,
				&scriptedTurnClient{events: []llmkit.Event{{Type: llmkit.EventDone}}},
			)
			turnOpt.Model = newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				Model:    "turn-test",
			}, 0)
			turnInstance, err := newTestTurn(turnOpt)
			if err != nil {
				t.Fatal(err)
			}
			turnResult, runErr := turnInstance.Run(t.Context(), []*defs.Item{turnUserMessage("settlement")}, nil)
			if turnResult.FinalModelCallID != "" || turnResult.Status != "" {
				t.Fatalf("failed save reported success: %+v", turnResult)
			}
			if runErr == nil || errors.Is(runErr, turn.ErrTurnUnsettled) != tt.wantUnsettled {
				t.Fatalf("Run() error = %v, want unsettled %v", runErr, tt.wantUnsettled)
			}
			record, found, err := stores.query.GetTurn(t.Context(), sessionID, turnOpt.TurnID)
			if err != nil || found != tt.wantPersistent {
				t.Fatalf("GetTurn() = %+v, %v, %v", record, found, err)
			}
			if found && (record.Status != defs.TurnStatusRunning || record.FinalModelCallID != "") {
				t.Fatalf("GetTurn() = %+v, want running without final model call", record)
			}
		})
	}
}

// TestTurnPersistsAgentItemsByFirstAppearance 检查工具调用按首次出现顺序保存。
func TestTurnPersistsAgentItemsByFirstAppearance(t *testing.T) {
	// 1. 准备先出现 call-a、后出现 call-b，但 call-b 先完成的模型事件。
	stores := openTurnStorage(t)
	conversationID := defs.NewConversationID()
	sessionID := defs.NewSessionID()
	client := &scriptedTurnClient{events: []llmkit.Event{
		{
			Type: llmkit.EventToolCallStart,
			ToolCall: &llmkit.StreamingToolCall{
				CallID: "call-a",
				Name:   "lookup",
			},
		},
		{
			Type: llmkit.EventToolCallStart,
			ToolCall: &llmkit.StreamingToolCall{
				CallID: "call-b",
				Name:   "lookup",
			},
		},
		{
			Type: llmkit.EventToolCallEnd,
			ToolCall: &llmkit.StreamingToolCall{
				CallID:    "call-b",
				Name:      "lookup",
				Arguments: `{}`,
			},
		},
		{
			Type: llmkit.EventToolCallEnd,
			ToolCall: &llmkit.StreamingToolCall{
				CallID:    "call-a",
				Name:      "lookup",
				Arguments: `{}`,
			},
		},
		{Type: llmkit.EventDone},
	}}
	turnOpt := turn.NewTurnOpt(
		conversationID,
		sessionID,
		&agent.AgentDefinition{Name: "assistant"},
		stores.query,
		stores.mutation,
		client,
	)
	turnOpt.Model = newTestModel(&llmkit.Target{
		Protocol: llmkit.ProtocolOpenAIChat,
		Model:    "turn-test",
	}, 0)
	turnInstance, err := newTestTurn(turnOpt)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 运行 Turn，检查日志仍按 call-a、call-b 保存。
	_, err = turnInstance.Run(t.Context(), []*defs.Item{turnUserMessage("run tools")}, nil)
	if err == nil || !strings.Contains(err.Error(), "without an executor") {
		t.Fatalf("Run() error = %v", err)
	}
	items := readTurnLog(t, stores.query, conversationID, sessionID)
	if len(items) != 3 || items[1].ToolCall == nil || items[2].ToolCall == nil {
		t.Fatalf("persisted items = %+v", items)
	}
	if items[1].ToolCall.CallID != "call-a" || items[2].ToolCall.CallID != "call-b" {
		t.Fatalf("persisted call order = %q, %q, want call-a, call-b", items[1].ToolCall.CallID, items[2].ToolCall.CallID)
	}
}

// TestTurnModelCallGroupingSurvivesReload 检查模型调用分组和工具结果顺序在重新打开存储后不变。
func TestTurnModelCallGroupingSurvivesReload(t *testing.T) {
	// 1. 创建可重新打开的数据库和日志存储。
	root := t.TempDir()
	recordPath := filepath.Join(root, "records.db")
	logPath := filepath.Join(root, "logs")
	openStorage := func() (storagesqlite.Database, dao.Query, dao.Mutation) {
		database, err := storagesqlite.OpenDatabase(storagesqlite.NewDatabaseOpt(recordPath, dao.RecordNamespaces()...))
		if err != nil {
			t.Fatal(err)
		}
		logs, err := storagefile.NewLogStorage(storagefile.NewLogStorageOpt(logPath))
		if err != nil {
			t.Fatal(err)
		}
		query, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
		if err != nil {
			t.Fatal(err)
		}
		return database, query, mutation
	}

	// 2. 运行并行工具调用，记录实时模型请求。
	database, query, mutation := openStorage()
	conversationID := defs.NewConversationID()
	sessionID := defs.NewSessionID()
	client := &reloadTurnClient{}
	turnOpt := turn.NewTurnOpt(
		conversationID,
		sessionID,
		&agent.AgentDefinition{Name: "assistant"},
		query,
		mutation,
		client,
	)
	turnOpt.Model = newTestModel(&llmkit.Target{
		Protocol: llmkit.ProtocolOpenAIChat,
		Model:    "turn-test",
	}, 0)
	turnOpt.ToolExecutor = &reverseTurnExecutor{}
	turnInstance, err := newTestTurn(turnOpt)
	if err != nil {
		t.Fatal(err)
	}
	_, err = turnInstance.Run(t.Context(), []*defs.Item{turnUserMessage("run both tools")}, nil)
	if err == nil || !strings.Contains(err.Error(), "stop after capture") {
		t.Fatalf("Run() error = %v", err)
	}
	if client.captured == nil {
		t.Fatal("second model request was not captured")
	}
	if len(client.captured.Messages) != 4 {
		t.Fatalf("live messages = %#v, want user, assistant and two tool results", client.captured.Messages)
	}
	assistantMessage := client.captured.Messages[1]
	if assistantMessage.Role != llmkit.RoleAssistant || len(assistantMessage.ToolCalls) != 2 ||
		assistantMessage.ToolCalls[0].ID != "call-a" || assistantMessage.ToolCalls[1].ID != "call-b" {
		t.Fatalf("live assistant message = %#v", assistantMessage)
	}
	if client.captured.Messages[2].ToolCallID != "call-b" || client.captured.Messages[3].ToolCallID != "call-a" {
		t.Fatalf("live tool result order = %q, %q", client.captured.Messages[2].ToolCallID, client.captured.Messages[3].ToolCallID)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. 重新打开存储，检查调用、结果和重新组装的模型请求。
	reopenedDatabase, reopenedQuery, _ := openStorage()
	t.Cleanup(func() { _ = reopenedDatabase.Close() })
	reloadedItems := readTurnLog(t, reopenedQuery, conversationID, sessionID)
	if len(reloadedItems) != 6 {
		t.Fatalf("reloaded items = %+v, want input, two calls, two results and the captured call error", reloadedItems)
	}
	if reloadedItems[5].Kind != defs.ItemModelError || !strings.Contains(reloadedItems[5].ModelError.Message, "stop after capture") || reloadedItems[5].ModelCallID == reloadedItems[1].ModelCallID {
		t.Fatalf("captured call failure = %+v", reloadedItems[5])
	}
	if reloadedItems[1].ToolCall == nil || reloadedItems[2].ToolCall == nil ||
		reloadedItems[1].ToolCall.CallID != "call-a" || reloadedItems[2].ToolCall.CallID != "call-b" {
		t.Fatalf("reloaded tool calls = %+v, %+v", reloadedItems[1], reloadedItems[2])
	}
	if reloadedItems[3].ToolResult == nil || reloadedItems[4].ToolResult == nil ||
		reloadedItems[3].ToolResult.CallID != "call-b" || reloadedItems[4].ToolResult.CallID != "call-a" {
		t.Fatalf("reloaded tool results = %+v, %+v", reloadedItems[3], reloadedItems[4])
	}
	toolCallsByID := map[string]*defs.Item{
		reloadedItems[1].ToolCall.CallID: reloadedItems[1],
		reloadedItems[2].ToolCall.CallID: reloadedItems[2],
	}
	for _, resultItem := range reloadedItems[3:5] {
		callItem := toolCallsByID[resultItem.ToolResult.CallID]
		if resultItem.TurnID != callItem.TurnID || resultItem.ModelCallID != callItem.ModelCallID {
			t.Fatalf("result identity = %q/%q, call identity = %q/%q", resultItem.TurnID, resultItem.ModelCallID, callItem.TurnID, callItem.ModelCallID)
		}
	}

	reloadedModel := newTestModel(&llmkit.Target{}, 0)
	reloadedContext, err := agent_context.NewAgentContext(agent_context.NewAgentContextOpt(sessionID, "reloaded-turn", client, reloadedModel, reloadedModel))
	if err != nil {
		t.Fatal(err)
	}
	contextItems, err := dao.LoadContextHistory(t.Context(), reopenedQuery, conversationID, sessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reloadedContext.Load(&agent_context.ContextLoadParam{Items: contextItems}); err != nil {
		t.Fatal(err)
	}
	reloadedRequest, _, err := reloadedContext.BuildModelRequest(nil)
	if err != nil {
		t.Fatal(err)
	}
	reloadedRequestMessages := reloadedRequest.Messages
	if !reflect.DeepEqual(reloadedRequestMessages, client.captured.Messages) {
		t.Fatalf("reloaded messages = %#v, live request = %#v", reloadedRequestMessages, client.captured.Messages)
	}
}

// countJSONText 递归统计 JSON 中与指定文本相同的字符串数量。
func countJSONText(value any, want string) int {
	// 1. 检查字符串，递归遍历数组和对象并累加数量。
	count := 0
	switch value := value.(type) {
	case string:
		if value == want {
			return 1
		}
	case []any:
		for _, item := range value {
			count += countJSONText(item, want)
		}
	case map[string]any:
		for _, item := range value {
			count += countJSONText(item, want)
		}
	}
	return count
}

// TestTurnCancellationRecordsInterrupted 检查取消时中断 Item、保存事件和本轮状态。
func TestTurnCancellationRecordsInterrupted(t *testing.T) {
	// 1. 准备调用前、调用中、只压缩及中断 Item 保存失败的取消样例。
	tests := []struct {
		name string
		// isCompactOnly 是否运行只压缩的 Turn？是的话历史里先放一条可压缩的输入。
		isCompactOnly bool
		// cancelAfterInput 是否在输入保存后立刻取消？是的话模型请求不会发出；否则等模型请求开始后再取消。
		cancelAfterInput bool
		// failInterruptedWrite 中断 Item 的写入是否失败？
		failInterruptedWrite bool
		// wantInterruptedItem 本轮最后是否保存了一条中断 Item，并发出对应的 TurnItemsSaved？
		wantInterruptedItem bool
		// withPromptHook 输入是否先经过 UserPromptSubmit？是的话它的 hook_run 要排在中断 Item 之前。
		withPromptHook bool
	}{
		{
			name:                "canceled_during_model_call",
			wantInterruptedItem: true,
		},
		{
			name:                "canceled_after_prompt_hook",
			wantInterruptedItem: true,
			withPromptHook:      true,
		},
		{
			name:                "canceled_before_model_call",
			cancelAfterInput:    true,
			wantInterruptedItem: true,
		},
		{
			name:          "compact_only",
			isCompactOnly: true,
		},
		{
			name:                 "interrupted_item_write_failure_keeps_interrupted_status",
			failInterruptedWrite: true,
		},
	}
	// 2. 在指定阶段取消，等待模型退出，检查中断 Item 最后保存及中断写入失败时保留 running。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestStarted := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if flusher, ok := w.(http.Flusher); ok {
						_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"partial"}}]}

`)
						flusher.Flush()
					}
					close(requestStarted)
					<-r.Context().Done()
				}))
			t.Cleanup(server.Close)

			stores := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sessionID := defs.NewSessionID()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			mutation := &interruptTestMutation{
				Mutation:             stores.mutation,
				failInterruptedWrite: tt.failInterruptedWrite,
			}
			if tt.cancelAfterInput {
				mutation.cancelAfterInput = cancel
			}
			turnOpt := turn.NewTurnOpt(
				conversationID,
				sessionID,
				&agent.AgentDefinition{Name: "assistant"},
				stores.query,
				mutation,
				llmkit.NewClient(llmkit.ClientOptions{}),
			)
			turnOpt.IsCompactOnly = tt.isCompactOnly
			turnOpt.Model = newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  server.URL,
				Model:    "cancel-test",
			}, 0)
			if tt.withPromptHook {
				turnOpt.Hooks = &turnHooksStub{outcomes: map[turnHookKey]*hook.HookOutcome{
					{
						name:   defs.HookEventUserPromptSubmit,
						prompt: "等待后取消。",
					}: newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunCompleted),
				}}
				turnOpt.Environments = newTurnEnvironments(t, []defs.TurnExecutor{{
					ID:                 "local",
					WorkspaceDirectory: "/a",
				}})
			}
			turnInstance, err := newTestTurn(turnOpt)
			if err != nil {
				t.Fatal(err)
			}
			var input []*defs.Item
			if tt.isCompactOnly {
				seedTurnSummaryHistory(t, stores.mutation, conversationID, sessionID)
			} else {
				input = []*defs.Item{turnUserMessage("等待后取消。")}
			}
			events := make(chan turn.TurnEvent, 64)
			completed := make(chan error, 1)
			go func() {
				_, err := turnInstance.Run(ctx, input, events)
				completed <- err
			}()
			if !tt.cancelAfterInput {
				select {
				case <-requestStarted:
				case <-time.After(5 * time.Second):
					t.Fatal("model request did not start")
				}
				cancel()
			}
			select {
			case err := <-completed:
				if err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("Run() error = %v, want context cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Turn.Run did not wait for Agent shutdown")
			}
			close(events)
			if tt.cancelAfterInput {
				select {
				case <-requestStarted:
					t.Fatal("model request started after the Turn was canceled")
				default:
				}
			}

			record, found, err := stores.query.GetTurn(t.Context(), sessionID, turnOpt.TurnID)
			if err != nil || !found {
				t.Fatalf("GetTurn() = %+v, %v, %v", record, found, err)
			}
			if tt.failInterruptedWrite {
				if record.Status != defs.TurnStatusRunning || record.ItemsRange.LastOrdinal != 0 {
					t.Fatalf("Turn = %+v, want unsettled running range", record)
				}
			} else if record.Status != defs.TurnStatusInterrupted || !strings.Contains(record.Error, context.Canceled.Error()) {
				t.Fatalf("Turn = %+v, want interrupted", record)
			}
			logItems := readTurnLog(t, stores.query, conversationID, sessionID)
			interrupted := slices.DeleteFunc(slices.Clone(logItems), func(item *defs.Item) bool { return item.Kind != defs.ItemTurnInterrupted })
			if !tt.wantInterruptedItem {
				if len(interrupted) != 0 {
					t.Fatalf("Log items = %+v, want no interrupted item", logItems)
				}
				return
			}
			last := logItems[len(logItems)-1]
			if len(interrupted) != 1 || last.Kind != defs.ItemTurnInterrupted || last.TurnID != turnOpt.TurnID {
				t.Fatalf("Log items = %+v, want one interrupted item of turn %s as the last item", logItems, turnOpt.TurnID)
			}
			if tt.withPromptHook && !slices.ContainsFunc(logItems, func(item *defs.Item) bool { return item.Kind == defs.ItemHookRun }) {
				t.Fatalf("Log items = %+v, want the prompt hook run before the interrupted item", logItems)
			}
			var savedLast *defs.Item
			for event := range events {
				if saved, ok := event.(*turn.TurnItemsSaved); ok {
					savedLast = saved.Items[len(saved.Items)-1].Item
				}
			}
			if savedLast == nil || savedLast.ID != last.ID {
				t.Fatalf("last saved event item = %+v, want the interrupted item %s", savedLast, last.ID)
			}
		})
	}
}

// interruptTestMutation 在输入保存后取消 Turn，或让中断 Item 的写入失败。
type interruptTestMutation struct {
	dao.Mutation
	cancelAfterInput     context.CancelFunc
	failInterruptedWrite bool
}

// AppendItems 注入中断 Item 保存错误，或在输入保存成功后取消 Turn。
func (mutation *interruptTestMutation) AppendItems(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, items ...*defs.Item) ([]dao.LoggedItem, error) {
	// 1. 中断 Item 配置失败时直接返回错误，否则追加内容并按配置取消。
	if mutation.failInterruptedWrite && items[0].Kind == defs.ItemTurnInterrupted {
		return nil, errors.New("injected interrupted item write failure")
	}
	saved, err := mutation.Mutation.AppendItems(ctx, conversationID, sessionID, items...)
	if err == nil && mutation.cancelAfterInput != nil && items[0].Kind == defs.ItemMessage {
		mutation.cancelAfterInput()
	}
	return saved, err
}

// metadataTestStreamID 是对话 metadata.jsonl 的 StreamID；失败注入只针对 Item 日志，metadata 行直接放行。
const metadataTestStreamID storage.StreamID = "metadata"

// failingLogStorage 在 Item 日志成功追加 successfulLimit 次之后让后续追加失败。
type failingLogStorage struct {
	storage.LogStorage
	successfulLimit int
	appendCount     int
}

// failingTurnMutation 按配置让 running 或终态的 Turn 记录保存失败。
type failingTurnMutation struct {
	dao.Mutation
	failRunning  bool
	failTerminal bool
}

// SetTurn 按配置让 running 或结束记录保存失败。
func (mutation *failingTurnMutation) SetTurn(ctx context.Context, value defs.Turn) error {
	// 1. 检查状态对应的失败开关，其他记录交给 DAO 保存。
	if (value.Status == defs.TurnStatusRunning && mutation.failRunning) ||
		(value.Status != defs.TurnStatusRunning && mutation.failTerminal) {
		return errors.New("injected turn persistence failure")
	}
	return mutation.Mutation.SetTurn(ctx, value)
}

// failingTurnClient 的每次模型调用都直接失败。
type failingTurnClient struct{}

// Call 返回预设的模型调用失败。
func (*failingTurnClient) Call(context.Context, llmkit.Request, llmkit.Target, llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 返回空流和测试模型错误。
	return nil, errors.New("injected model failure")
}

// blockingTurnClient 返回一直不结束的模型流，首次调用时关闭 started。
type blockingTurnClient struct {
	started chan struct{}
	once    sync.Once
}

// Call 提供等待取消的测试模型流。
func (client *blockingTurnClient) Call(
	ctx context.Context,
	_ llmkit.Request,
	_ llmkit.Target,
	_ llmkit.CallOptions,
) (<-chan llmkit.Event, error) {
	// 1. 仅首次调用发送启动通知，再等待取消并关闭事件流。
	client.once.Do(func() { close(client.started) })
	events := make(chan llmkit.Event)
	go func() {
		<-ctx.Done()
		close(events)
	}()
	return events, nil
}

// scriptedTurnClient 每次调用都发送同一组预设事件。
type scriptedTurnClient struct {
	events []llmkit.Event
}

// reloadTurnClient 第一次返回两个工具调用，第二次记下请求后返回错误。
type reloadTurnClient struct {
	calls    int
	captured *llmkit.Request
}

// Call 返回重载历史测试的模型响应。
func (c *reloadTurnClient) Call(
	_ context.Context,
	request llmkit.Request,
	_ llmkit.Target,
	_ llmkit.CallOptions,
) (<-chan llmkit.Event, error) {
	// 1. 记录调用次数，第二次保存请求，首次依次发送工具调用及结束事件。
	c.calls++
	if c.calls == 2 {
		captured := request
		c.captured = &captured
		return nil, errors.New("stop after capture")
	}
	stream := make(chan llmkit.Event, 5)
	stream <- llmkit.Event{
		Type: llmkit.EventToolCallStart,
		ToolCall: &llmkit.StreamingToolCall{
			CallID: "call-a",
			Name:   "lookup",
		},
	}
	stream <- llmkit.Event{
		Type: llmkit.EventToolCallStart,
		ToolCall: &llmkit.StreamingToolCall{
			CallID: "call-b",
			Name:   "lookup",
		},
	}
	stream <- llmkit.Event{
		Type: llmkit.EventToolCallEnd,
		ToolCall: &llmkit.StreamingToolCall{
			CallID:    "call-a",
			Name:      "lookup",
			Arguments: `{}`,
		},
	}
	stream <- llmkit.Event{
		Type: llmkit.EventToolCallEnd,
		ToolCall: &llmkit.StreamingToolCall{
			CallID:    "call-b",
			Name:      "lookup",
			Arguments: `{}`,
		},
	}
	stream <- llmkit.Event{Type: llmkit.EventDone}
	close(stream)
	return stream, nil
}

// usageTurnCall 是 usageTurnClient 一次模型调用的表现。
type usageTurnCall struct {
	// events 是调用成功时依次发送的事件。
	events []llmkit.Event
	// err 非空时调用直接返回这个错误。
	err error
	// block 发完事件后是否保持流打开，直到 ctx 取消？
	block bool
}

// usageTurnClient 按顺序执行 calls；调用次数超过 calls 时返回错误。blocked 在第一次进入阻塞流时关闭。
type usageTurnClient struct {
	calls     []usageTurnCall
	next      int
	blocked   chan struct{}
	once      sync.Once
	requests  []llmkit.Request
	callTimes []time.Time
}

// Call 按次序执行预设模型响应，并记录请求和时间。
func (client *usageTurnClient) Call(
	ctx context.Context,
	request llmkit.Request,
	_ llmkit.Target,
	_ llmkit.CallOptions,
) (<-chan llmkit.Event, error) {
	// 1. 记录调用并检查预设数量，按本次配置返回错误或发送事件。
	client.requests = append(client.requests, request)
	client.callTimes = append(client.callTimes, time.Now())
	if client.next >= len(client.calls) {
		return nil, errors.New("unexpected model call")
	}
	call := client.calls[client.next]
	client.next++
	if call.err != nil {
		return nil, call.err
	}
	// 2. 发送预设事件，非阻塞样例关闭通道，阻塞样例等取消后再关闭。
	if call.block {
		stream := make(chan llmkit.Event)
		go func() {
			defer close(stream)
			for _, event := range call.events {
				select {
				case stream <- event:
				case <-ctx.Done():
					return
				}
			}
			client.once.Do(func() { close(client.blocked) })
			<-ctx.Done()
		}()
		return stream, nil
	}
	stream := make(chan llmkit.Event, len(call.events))
	for _, event := range call.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

// reverseTurnExecutor 并行执行 call-a、call-b 两个 lookup 调用，按 call-b、call-a 的顺序返回结果。
type reverseTurnExecutor struct {
	stopTurn bool
}

// GetTools 返回测试 lookup 工具定义。
func (*reverseTurnExecutor) GetTools(context.Context) ([]*defs.ToolDefinition, error) {
	// 1. 返回查询工具的名称、说明和对象参数结构。
	return []*defs.ToolDefinition{
		{
			Name:        "lookup",
			Description: "lookup",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}, nil
}

// Run 并行执行两个调用，按 call-b、call-a 的顺序返回结果。
func (e *reverseTurnExecutor) Run(ctx context.Context, calls []*defs.Item, events chan<- executor.ExecutorEvent) {
	// 1. 检查收到两个预期调用，创建并行启动、释放和结束通知。
	defer close(events)
	if len(calls) != 2 || calls[0].ToolCall.CallID != "call-a" || calls[1].ToolCall.CallID != "call-b" {
		panic(fmt.Sprintf("unexpected calls: %+v", calls))
	}
	started := make(chan struct{}, len(calls))
	release := make(chan struct{})
	callBFinished := make(chan struct{})
	// 2. 并行启动两个调用，call-a 等 call-b 结果交付后再完成。
	var tasks sync.WaitGroup
	for _, call := range calls {
		tasks.Go(func() {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			if call.ToolCall.CallID == "call-a" {
				select {
				case <-callBFinished:
				case <-ctx.Done():
					return
				}
			}
			finishedEvent := &executor.ExecutionFinishedEvent{
				Result: &defs.ToolResult{
					CallID:  call.ToolCall.CallID,
					Content: json.RawMessage(`{"ok":true}`),
				},
			}
			if e.stopTurn {
				finishedEvent.HookRuns = []*defs.HookRun{{
					ID:     defs.NewHookRunID(),
					Event:  defs.HookEventPostToolUse,
					CallID: call.ToolCall.CallID,
					Status: defs.HookRunStopped,
				}}
			}
			select {
			case events <- finishedEvent:
				if call.ToolCall.CallID == "call-b" {
					close(callBFinished)
				}
			case <-ctx.Done():
			}
		})
	}
	// 3. 等待两个调用均已启动，释放执行并等待全部任务结束。
	for range calls {
		<-started
	}
	close(release)
	tasks.Wait()
}

// Call 发送预设模型事件后关闭事件流。
func (c *scriptedTurnClient) Call(
	context.Context,
	llmkit.Request,
	llmkit.Target,
	llmkit.CallOptions,
) (<-chan llmkit.Event, error) {
	// 1. 将预设事件放入缓冲通道，再关闭通道。
	stream := make(chan llmkit.Event, len(c.events))
	for _, event := range c.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

// Append 按成功次数注入 Item 日志追加错误。
func (logs *failingLogStorage) Append(
	ctx context.Context,
	path storage.StreamPath,
	streamID storage.StreamID,
	payloads ...json.RawMessage,
) ([]storage.LogEntryInfo, error) {
	// 1. metadata 日志直接追加；Item 日志达到次数上限后返回错误，其他追加累计次数。
	if streamID == metadataTestStreamID {
		return logs.LogStorage.Append(ctx, path, streamID, payloads...)
	}
	if logs.appendCount >= logs.successfulLimit {
		return nil, errors.New("injected log failure")
	}
	logs.appendCount++
	return logs.LogStorage.Append(ctx, path, streamID, payloads...)
}

// turnTestStorage 是测试用的记录、日志存储和 DAO；root 目录可以重新打开。
type turnTestStorage struct {
	root     string
	database storagesqlite.Database
	query    dao.Query
	mutation dao.Mutation
	records  storage.RecordStorage
	logs     storage.LogStorage
}

// openTurnStorage 在临时目录打开测试记录和日志存储。
func openTurnStorage(t *testing.T) turnTestStorage {
	// 1. 创建临时目录并打开存储。
	t.Helper()
	return openTurnStorageAt(t, t.TempDir())
}

// openTurnStorageAt 在指定目录打开测试数据库、日志和 DAO。
func openTurnStorageAt(t *testing.T, root string) turnTestStorage {
	// 1. 打开记录数据库并登记测试结束时关闭。
	t.Helper()
	database, err := storagesqlite.OpenDatabase(
		storagesqlite.NewDatabaseOpt(filepath.Join(root, "records.db"), dao.RecordNamespaces()...),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	// 2. 打开日志存储，创建 DAO 并返回测试存储。
	logs, err := storagefile.NewLogStorage(
		storagefile.NewLogStorageOpt(filepath.Join(root, "logs")),
	)
	if err != nil {
		t.Fatal(err)
	}
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(database.Record(), logs))
	if err != nil {
		t.Fatal(err)
	}
	return turnTestStorage{
		root:     root,
		database: database,
		query:    query,
		mutation: mutation,
		records:  database.Record(),
		logs:     logs,
	}
}

// readTurnLog 按流水号正序读取 Session 的全部 Item。
func readTurnLog(t *testing.T, query dao.Query, conversationID defs.ConversationID, sessionID defs.SessionID) []*defs.Item {
	// 1. 每页读取 256 条日志，收集 Item 并按下一页游标继续读取。
	t.Helper()
	var result []*defs.Item
	var cursor storage.Ordinal
	for {
		page, err := query.ListItems(t.Context(), dao.ListItemsRequest{
			ConversationID: conversationID,
			SessionID:      sessionID,
			Order:          storage.DirectionAsc,
			Cursor:         cursor,
			Limit:          256,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, logged := range page.Items {
			result = append(result, logged.Item)
		}
		if page.Next == 0 {
			return result
		}
		cursor = page.Next
	}
}

// turnUserMessage 创建带新 ID 的用户文本输入。
func turnUserMessage(text string) *defs.Item {
	// 1. 填写 Item ID、用户发送方和文本内容。
	return &defs.Item{
		ID:   defs.NewItemID(),
		Kind: defs.ItemMessage,
		Message: &defs.Message{
			From: &defs.Address{
				Kind: defs.IdentityUser,
				ID:   "user",
			},
			Content: []*defs.Content{
				{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: text},
				},
			},
		},
	}
}

// TestTurnLoadsCompactedHistory 检查加载摘要、保留原文及无效摘要错误。
func TestTurnLoadsCompactedHistory(t *testing.T) {
	// 1. 准备无摘要、多次摘要、跨页和覆盖范围缺失的历史样例。
	for _, tt := range []struct {
		name                                string
		summary, previous, corrupt, missing bool
		tailCount                           int
	}{
		{
			"no_summary",
			false,
			false,
			false,
			false,
			0,
		},
		{
			"summary_before_retained_tail",
			true,
			false,
			false,
			false,
			0,
		},
		{
			"latest_summary",
			true,
			true,
			false,
			false,
			0,
		},
		{
			"boundary_across_pages",
			true,
			false,
			false,
			false,
			300,
		},
		{
			"missing_boundary",
			true,
			false,
			false,
			true,
			0,
		},
		{
			"corrupt_summary",
			true,
			false,
			true,
			false,
			0,
		},
	} {
		// 2. 保存并重新打开历史，运行下一轮，检查最新摘要在前、已覆盖原文不进入请求。
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			var history []*defs.Item
			for _, text := range []string{"A", "B", "C", "D"} {
				item := turnUserMessage(text)
				item.ID = defs.ItemID(text)
				item.TurnID = "source-turn"
				history = append(history, item)
				if text == "C" && tt.previous {
					history = append(history, turnSummary("old-summary", "A", "old summary"))
				}
			}
			for n := 0; n < tt.tailCount; n++ {
				item := turnUserMessage(fmt.Sprintf("tail-%03d", n))
				item.TurnID = "source-turn"
				history = append(history, item)
			}
			if tt.summary {
				boundary := defs.ItemID("B")
				if tt.missing {
					boundary = "missing"
				}
				summary := turnSummary("S", boundary, "latest summary")
				if tt.corrupt {
					summary.ContextSummary.Text = ""
				}
				history = append(history, summary)
			}
			e := turnUserMessage("E")
			e.TurnID = "source-turn"
			history = append(history, e)
			for _, item := range history {
				payload, err := json.Marshal(item)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.logs.Append(t.Context(), storage.StreamPath(conversationID), storage.StreamID(sid), payload); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.database.Close(); err != nil {
				t.Fatal(err)
			}
			store = openTurnStorageAt(t, store.root)
			client := &summaryTurnClient{}
			opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, client)
			opt.Model = newTestModel(&llmkit.Target{}, 0)
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			_, err = instance.Run(t.Context(), []*defs.Item{turnUserMessage("next")}, nil)
			if tt.corrupt || tt.missing {
				if err == nil || len(client.requests) != 0 {
					t.Fatalf("bad history reached model: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			messages := client.requests[0].Messages
			wantMessages := 6 + tt.tailCount
			if tt.summary {
				wantMessages = 5 + tt.tailCount
			}
			if len(messages) != wantMessages {
				t.Fatalf("messages = %d, want %d", len(messages), wantMessages)
			}
			for _, text := range []string{"A", "B", "C", "D", "E", "next"} {
				want := 1
				if tt.summary && (text == "A" || text == "B") {
					want = 0
				}
				count := 0
				for _, message := range messages {
					for _, block := range message.Content {
						if block.Text == text {
							count++
						}
					}
				}
				if count != want {
					t.Fatalf("%s appears %d times, want %d", text, count, want)
				}
			}
			if tt.summary && !strings.Contains(messages[0].Content[0].Text, "latest summary") {
				t.Fatal("latest summary not first")
			}
		})
	}
}

// TestTurnPersistsContextSummary 检查不同保存阶段失败时摘要的落盘和重新加载。
func TestTurnPersistsContextSummary(t *testing.T) {
	// 1. 准备原文、摘要和后续输出保存失败的样例。
	for _, tt := range []struct {
		name        string
		limit       int
		wantSummary bool
	}{
		{
			"source_write_failure",
			5,
			false,
		},
		{
			"summary_write_failure",
			6,
			false,
		},
		{
			"later_output_failure",
			7,
			true,
		},
	} {
		// 2. 在指定追加次数失败后重新打开存储，检查已保存摘要和原文组成的后续请求。
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			seedTurnSummaryHistory(t, store.mutation, conversationID, sid)
			logs := &failingLogStorage{
				LogStorage:      store.logs,
				successfulLimit: tt.limit,
			}
			query, mutation, err := dao.NewDAO(dao.NewDAOOpt(store.records, logs))
			if err != nil {
				t.Fatal(err)
			}
			client := &summaryTurnClient{toolLoop: true}
			opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, query, mutation, client)
			opt.Model = newTestModel(&llmkit.Target{}, 1000)
			opt.ToolExecutor = &reverseTurnExecutor{}
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			_, err = instance.Run(ctx, []*defs.Item{turnUserMessage(strings.Repeat("recent ", 100))}, nil)
			if err == nil || !strings.Contains(err.Error(), "injected log failure") {
				t.Fatalf("Run = %v", err)
			}
			for _, callCtx := range client.contexts {
				select {
				case <-callCtx.Done():
				default:
					t.Fatal("Turn returned before canceling model request")
				}
			}
			items := readTurnLog(t, store.query, conversationID, sid)
			if len(items) != tt.limit+1 {
				t.Fatalf("saved %d items, want %d", len(items), tt.limit+1)
			}
			summaries := 0
			for _, item := range items {
				if item.Kind == defs.ItemContextSummary {
					summaries++
				}
			}
			want := 0
			if tt.wantSummary {
				want = 1
			}
			if summaries != want {
				t.Fatalf("saved summaries = %d, want %d", summaries, want)
			}
			if err := store.database.Close(); err != nil {
				t.Fatal(err)
			}
			store = openTurnStorageAt(t, store.root)
			reloaded := &summaryTurnClient{}
			nextOpt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, reloaded)
			nextOpt.Model = newTestModel(&llmkit.Target{}, 0)
			next, err := newTestTurn(nextOpt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := next.Run(t.Context(), []*defs.Item{turnUserMessage("continue")}, nil); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(reloaded.requests[0].Messages)
			if strings.Contains(string(payload), "saved summary") != tt.wantSummary || strings.Contains(string(payload), "old-history") == tt.wantSummary {
				t.Fatalf("unexpected recovered view: %s", payload)
			}
		})
	}
}

// TestTurnCompactionFailureDoesNotPersistSummary 检查摘要调用失败不保存摘要草稿。
func TestTurnCompactionFailureDoesNotPersistSummary(t *testing.T) {
	// 1. 准备自动和手动压缩的调用失败样例。
	for _, tt := range []struct {
		name      string
		manual    bool
		wantItems int
	}{
		{
			name:      "automatic",
			wantItems: 7,
		},
		{
			name:      "manual",
			manual:    true,
			wantItems: 1,
		},
	} {
		// 2. 运行到摘要调用失败，检查本轮为 failed、日志只含已有输入和输出。
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			seedTurnSummaryHistory(t, store.mutation, conversationID, sid)
			client := &summaryTurnClient{
				toolLoop:   !tt.manual,
				summaryErr: errors.New("summary model offline"),
			}
			opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, client)
			opt.Model = newTestModel(&llmkit.Target{}, 1000)
			opt.ToolExecutor = &reverseTurnExecutor{}
			opt.IsCompactOnly = tt.manual
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			var input []*defs.Item
			if !tt.manual {
				input = []*defs.Item{turnUserMessage(strings.Repeat("recent ", 100))}
			}
			result, err := instance.Run(t.Context(), input, nil)
			if err == nil || !strings.Contains(err.Error(), "summary model offline") || result.Status != defs.TurnStatusFailed {
				t.Fatalf("Run() = status %q, error %v; want a failed turn caused by the summary model", result.Status, err)
			}
			// 检查已有输入和输出仍在日志中，没有保存失败的摘要。
			items := readTurnLog(t, store.query, conversationID, sid)
			if len(items) != tt.wantItems || slices.ContainsFunc(items, func(item *defs.Item) bool { return item.Kind == defs.ItemContextSummary }) {
				t.Fatalf("saved items = %+v, want %d items without a context summary", items, tt.wantItems)
			}
			if client.summaryAttempts != 1 {
				t.Fatalf("nonretryable compaction error made %d calls", client.summaryAttempts)
			}
		})
	}
}

// TestTurnCompactionRetry 检查摘要重试后只保存成功摘要并累计用量。
func TestTurnCompactionRetry(t *testing.T) {
	// 1. 准备自动和手动压缩中首个摘要流提前结束的样例。
	for _, tt := range []struct {
		name   string
		manual bool
	}{
		{name: "automatic"},
		{
			name:   "manual",
			manual: true,
		},
	} {
		// 2. 让首次摘要流提前关闭并重试，检查两个调用 ID、单个完整摘要及累计用量。
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stores := openTurnStorage(t)
				conversationID, sessionID := defs.NewConversationID(), defs.NewSessionID()
				seedTurnSummaryHistory(t, stores.mutation, conversationID, sessionID)
				client := &summaryTurnClient{
					toolLoop:        !tt.manual,
					summaryFailures: 1,
				}
				opt := turn.NewTurnOpt(conversationID, sessionID, &agent.AgentDefinition{Name: "assistant"}, stores.query, stores.mutation, client)
				opt.Model = newTestModel(&llmkit.Target{Model: "main-model"}, 1000)
				opt.ToolExecutor = &reverseTurnExecutor{}
				opt.IsCompactOnly = tt.manual
				instance, err := newTestTurn(opt)
				if err != nil {
					t.Fatal(err)
				}
				var input []*defs.Item
				if !tt.manual {
					input = []*defs.Item{turnUserMessage(strings.Repeat("recent ", 100))}
				}
				events := make(chan turn.TurnEvent, 128)
				result, err := instance.Run(t.Context(), input, events)
				if err != nil || result.Status != defs.TurnStatusCompleted || client.summaryAttempts != 2 {
					t.Fatalf("Run() = %+v, %v; summary attempts %d", result, err, client.summaryAttempts)
				}
				close(events)
				var compactCalls []defs.ModelCallID
				var started, completed int
				for event := range events {
					if event, ok := event.(*turn.TurnAgentEvent); ok {
						if event.Event.Type == agent.AgentEventModelCallFinished && event.Event.ModelCall.Purpose == agent.ModelCallPurposeCompaction {
							compactCalls = append(compactCalls, event.Event.ModelCall.ModelCallID)
						}
						if event.Event.Item != nil && event.Event.Item.Kind == defs.ItemContextSummary {
							if event.Event.Type == agent.AgentEventItemStarted {
								started++
							}
							if event.Event.Type == agent.AgentEventItemCompleted {
								completed++
							}
						}
					}
				}
				if len(compactCalls) != 2 || compactCalls[0] == compactCalls[1] || started != 1 || completed != 1 {
					t.Fatalf("compact calls %v, started %d, completed %d", compactCalls, started, completed)
				}
				items := readTurnLog(t, stores.query, conversationID, sessionID)
				var summary *defs.Item
				for _, item := range items {
					if item.Kind == defs.ItemModelError {
						t.Fatalf("compaction stored a model error: %+v", item)
					}
					if item.Kind == defs.ItemContextSummary {
						if summary != nil {
							t.Fatal("multiple summaries were persisted")
						}
						summary = item
					}
				}
				if summary == nil || summary.ContextSummary.Text != "saved summary" || summary.ModelCallID != compactCalls[1] {
					t.Fatalf("persisted summary = %+v", summary)
				}
				payload, _ := json.Marshal(items)
				if strings.Contains(string(payload), "discarded partial summary") {
					t.Fatalf("partial summary was persisted: %s", payload)
				}
				var summaryRequests []llmkit.Request
				for _, request := range client.requests {
					if request.ToolChoice != nil && request.ToolChoice.Type == "none" {
						summaryRequests = append(summaryRequests, request)
					}
				}
				if len(summaryRequests) != 2 || !reflect.DeepEqual(summaryRequests[0], summaryRequests[1]) {
					t.Fatalf("compaction retry changed request: %+v", summaryRequests)
				}
				record, found, err := stores.query.GetTurn(t.Context(), sessionID, opt.TurnID)
				if err != nil || !found || record.Status != defs.TurnStatusCompleted || record.Usage != (defs.TokenUsage{
					Input:  28,
					Output: 8,
				}) {
					t.Fatalf("turn = %+v, %v, %v", record, found, err)
				}
				if tt.manual {
					if record.ContextUsage != nil || result.ContextUsage != nil {
						t.Fatalf("manual compaction saved conversation report: %+v", record.ContextUsage)
					}
					if len(items) != 2 || len(client.normalModels) != 0 {
						t.Fatalf("manual compaction log %d items, normal requests %v", len(items), client.normalModels)
					}
				} else {
					if record.ContextUsage == nil || record.ContextUsage.ModelCallID != result.FinalModelCallID || slices.Contains(compactCalls, record.ContextUsage.ModelCallID) {
						t.Fatalf("conversation report overwritten by compaction: %+v", record.ContextUsage)
					}
					if len(items) < 3 || items[len(items)-2].ID != summary.ID || items[len(items)-3].Kind != defs.ItemToolResult || items[len(items)-1].Kind != defs.ItemMessage {
						t.Fatalf("automatic compaction did not preserve tool-result, summary, reply order: %+v", items)
					}
					request, _ := json.Marshal(client.requests[len(client.requests)-1].Messages)
					if !strings.Contains(string(request), "saved summary") || strings.Contains(string(request), "discarded partial summary") {
						t.Fatalf("normal request after compaction = %s", request)
					}
				}
			})
		})
	}
}

// TestTurnCompactionModel 检查摘要模型配置与普通模型调用各自使用的目标。
func TestTurnCompactionModel(t *testing.T) {
	// 1. 准备默认使用本轮模型和指定摘要模型的样例。
	for _, tt := range []struct {
		name         string
		compactModel *defs.ModelDefinition
		wantSummary  string
	}{
		{
			name:        "main_model_by_default",
			wantSummary: "main-model",
		},
		{
			name:         "configured_compact_model",
			compactModel: newTestModel(&llmkit.Target{Model: "summary-model"}, 1000),
			wantSummary:  "summary-model",
		},
	} {
		// 2. 运行工具后压缩，检查摘要请求和普通请求分别使用配置的模型目标。
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			seedTurnSummaryHistory(t, store.mutation, conversationID, sid)
			client := &summaryTurnClient{toolLoop: true}
			opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, client)
			opt.Model = newTestModel(&llmkit.Target{Model: "main-model"}, 1000)
			opt.CompactModel = tt.compactModel
			opt.ToolExecutor = &reverseTurnExecutor{}
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := instance.Run(t.Context(), []*defs.Item{turnUserMessage(strings.Repeat("recent ", 100))}, nil); err != nil {
				t.Fatal(err)
			}
			if len(client.summaryModels) != 1 || client.summaryModels[0] != tt.wantSummary {
				t.Fatalf("summary models = %v, want [%s]", client.summaryModels, tt.wantSummary)
			}
			if len(client.normalModels) == 0 {
				t.Fatal("no normal model request")
			}
			for _, model := range client.normalModels {
				if model != "main-model" {
					t.Fatalf("normal models = %v, want all main-model", client.normalModels)
				}
			}
		})
	}
}

// TestTurnCompactionPersistsInOrder 检查工具结果保存延迟后，摘要和回复仍按顺序写入。
func TestTurnCompactionPersistsInOrder(t *testing.T) {
	// 1. 创建在工具结果追加时阻塞的日志存储和可触发压缩的历史。
	store := openTurnStorage(t)
	conversationID := defs.NewConversationID()
	sid := defs.NewSessionID()
	seedTurnSummaryHistory(t, store.mutation, conversationID, sid)
	logs := &blockedSummaryLog{
		LogStorage: store.logs,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	query, mutation, err := dao.NewDAO(dao.NewDAOOpt(store.records, logs))
	if err != nil {
		t.Fatal(err)
	}
	client := &summaryTurnClient{toolLoop: true}
	opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, query, mutation, client)
	opt.Model = newTestModel(&llmkit.Target{}, 1000)
	opt.ToolExecutor = &reverseTurnExecutor{}
	instance, err := newTestTurn(opt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	// 2. 运行 Turn，等到工具结果写入阻塞后释放，再检查结果、摘要和回复的保存顺序。
	go runSummaryTurn(ctx, instance, done)
	select {
	case <-logs.started:
	case <-ctx.Done():
		t.Fatal("source write did not start")
	}
	close(logs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	items := readTurnLog(t, query, conversationID, sid)
	if len(items) != 9 || items[6].Kind != defs.ItemToolResult || items[7].Kind != defs.ItemContextSummary || items[8].Kind != defs.ItemMessage {
		t.Fatalf("persisted order = %+v", items)
	}
}

// runSummaryTurn 运行可触发压缩的 Turn，并通过通道回复错误。
func runSummaryTurn(ctx context.Context, instance turn.Turn, done chan<- error) {
	// 1. 传入较长的最近输入，运行后发送错误。
	_, err := instance.Run(ctx, []*defs.Item{turnUserMessage(strings.Repeat("recent ", 100))}, nil)
	done <- err
}

// seedTurnSummaryHistory 保存一条可供压缩测试使用的较早输入。
func seedTurnSummaryHistory(t *testing.T, mutation dao.Mutation, conversationID defs.ConversationID, sid defs.SessionID) {
	// 1. 创建带来源 TurnID 的较早历史，再写入 Session 日志。
	t.Helper()
	item := turnUserMessage(strings.Repeat("old-history ", 60))
	item.TurnID = "source-turn"
	if _, err := mutation.AppendItems(t.Context(), conversationID, sid, item); err != nil {
		t.Fatal(err)
	}
}

// turnSummary 创建指定覆盖范围和正文的摘要 Item。
func turnSummary(id, boundary defs.ItemID, text string) *defs.Item {
	// 1. 填写摘要身份、正文和覆盖到的 Item ID。
	return &defs.Item{
		ID:          id,
		TurnID:      "summary-turn",
		ModelCallID: "summary-model",
		Kind:        defs.ItemContextSummary,
		ContextSummary: &defs.ContextSummary{
			Text:                 text,
			CoveredThroughItemID: boundary,
		},
	}
}

// summaryTurnClient 是压缩测试用的模型客户端，按 ToolChoice 区分摘要请求和普通请求。
type summaryTurnClient struct {
	requests    []llmkit.Request
	contexts    []context.Context
	toolLoop    bool
	normalCalls int
	// summaryModels、normalModels 按调用顺序记录摘要请求与普通请求使用的模型名。
	summaryModels, normalModels []string
	// summaryErr 非 nil 时摘要请求直接失败。
	summaryErr      error
	summaryFailures int
	summaryAttempts int
}

// Call 返回摘要或普通回复，可模拟工具调用及摘要重试。
func (c *summaryTurnClient) Call(ctx context.Context, req llmkit.Request, target llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 记录请求和上下文。
	c.requests = append(c.requests, req)
	c.contexts = append(c.contexts, ctx)
	stream := make(chan llmkit.Event, 8)
	text := "answer"
	// 2. 禁用工具的摘要调用按配置失败、返回部分流或完整摘要；普通调用在 toolLoop 时首次返回较长输出和两个工具调用。
	if req.ToolChoice != nil && req.ToolChoice.Type == "none" {
		c.summaryModels = append(c.summaryModels, target.Model)
		c.summaryAttempts++
		if c.summaryErr != nil {
			return nil, c.summaryErr
		}
		if c.summaryAttempts <= c.summaryFailures {
			stream <- llmkit.Event{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: "discarded partial summary"},
			}
			stream <- llmkit.Event{
				Type: llmkit.EventUsage,
				Usage: &llmkit.Usage{
					PromptTokens:     11,
					CompletionTokens: 3,
				},
			}
			close(stream)
			return stream, nil
		}
		if c.summaryFailures != 0 {
			stream <- llmkit.Event{
				Type: llmkit.EventUsage,
				Usage: &llmkit.Usage{
					PromptTokens:     17,
					CompletionTokens: 5,
				},
			}
		}
		text = "saved summary"
	} else {
		c.normalModels = append(c.normalModels, target.Model)
		c.normalCalls++
		if c.toolLoop && c.normalCalls == 1 {
			text = strings.Repeat("source output ", 400)
			for _, id := range []string{"call-a", "call-b"} {
				stream <- llmkit.Event{
					Type: llmkit.EventToolCallStart,
					ToolCall: &llmkit.StreamingToolCall{
						CallID: id,
						Name:   "lookup",
					},
				}
				stream <- llmkit.Event{
					Type: llmkit.EventToolCallEnd,
					ToolCall: &llmkit.StreamingToolCall{
						CallID:    id,
						Name:      "lookup",
						Arguments: "{}",
					},
				}
			}
		}
	}
	// 3. 发送选定文本和结束事件，再关闭事件流。
	stream <- llmkit.Event{
		Type:  llmkit.EventContentDelta,
		Delta: &llmkit.DeltaPayload{Text: text},
	}
	stream <- llmkit.Event{
		Type:         llmkit.EventDone,
		FinishReason: "stop",
	}
	close(stream)
	return stream, nil
}

// blockedSummaryLog 在第 6 次 Item 追加时暂停，等测试放行后再写入。
type blockedSummaryLog struct {
	storage.LogStorage
	count            int
	started, release chan struct{}
}

// Append 暂停指定批次的日志追加。
func (l *blockedSummaryLog) Append(ctx context.Context, path storage.StreamPath, sid storage.StreamID, payloads ...json.RawMessage) ([]storage.LogEntryInfo, error) {
	// 1. metadata 直接追加；第 6 次 Item 追加时通知测试并等待放行或取消，再追加日志。
	if sid == metadataTestStreamID {
		return l.LogStorage.Append(ctx, path, sid, payloads...)
	}
	l.count++
	if l.count == 6 {
		close(l.started)
		select {
		case <-l.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return l.LogStorage.Append(ctx, path, sid, payloads...)
}

// TestTurnWritesModelChange 检查模型变化 Item 位于输入之前。
func TestTurnWritesModelChange(t *testing.T) {
	// 1. 准备首轮、相同模型和模型变化的样例。
	modelA := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "a",
		},
		Target: &llmkit.Target{Model: "a"},
	}
	modelB := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "b",
		},
		Target: &llmkit.Target{Model: "b"},
	}
	tests := []struct {
		name       string
		previous   defs.ModelRef
		wantChange bool
	}{
		{name: "first_turn_without_previous"},
		{
			name:     "same_model",
			previous: modelB.Ref,
		},
		{
			name:       "model_changed",
			previous:   modelA.Ref,
			wantChange: true,
		},
	}
	// 2. 运行本轮，检查模型变化 Item 位于用户输入前，且模型请求包含切换通知。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			client := &summaryTurnClient{}
			opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, client)
			opt.Model, opt.CompactModel, opt.PreviousModel = modelB, modelB, tt.previous
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			input := turnUserMessage("你好")
			result, err := instance.Run(t.Context(), []*defs.Item{input}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(result.InputItemIDs, []defs.ItemID{input.ID}) {
				t.Fatalf("input item IDs = %v, want only the input %s", result.InputItemIDs, input.ID)
			}
			page, err := store.query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sid,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err != nil || len(page.Items) == 0 {
				t.Fatalf("log = %+v, %v", page, err)
			}
			first := page.Items[0].Item
			payload, _ := json.Marshal(client.requests[0].Messages)
			if !tt.wantChange {
				if first.ID != input.ID || strings.Contains(string(payload), "openvaala-model-change-event") {
					t.Fatalf("first item = %+v, request has notice = %v", first, strings.Contains(string(payload), "openvaala-model-change-event"))
				}
				return
			}
			want := defs.ModelChange{
				PreviousModel: modelA.Ref,
				Model:         modelB.Ref,
			}
			if first.Kind != defs.ItemModelChange || *first.ModelChange != want || first.TurnID != opt.TurnID || page.Items[1].Item.ID != input.ID {
				t.Fatalf("log head = %+v, %+v", first, page.Items[1].Item)
			}
			if !strings.Contains(string(payload), "openvaala-model-change-event") {
				t.Fatalf("request lacks the model change notice: %s", payload)
			}
		})
	}
}

// runningTurnRecorder 记下写入的 running Turn 记录。
type runningTurnRecorder struct {
	dao.Mutation
	running []defs.Turn
}

// SetTurn 记录所有 running 写入，再保存 Turn。
func (recorder *runningTurnRecorder) SetTurn(ctx context.Context, value defs.Turn) error {
	// 1. 遇到 running 状态时收集记录，然后交给 DAO 保存。
	if value.Status == defs.TurnStatusRunning {
		recorder.running = append(recorder.running, value)
	}
	return recorder.Mutation.SetTurn(ctx, value)
}

// newTurnEnvironments 按列表顺序创建测试执行环境。
func newTurnEnvironments(t *testing.T, executors []defs.TurnExecutor) []executor.ExecutionEnvironment {
	// 1. 按每项工作区路径创建虚拟工作区，再创建对应 ID 的执行环境。
	t.Helper()
	var environments []executor.ExecutionEnvironment
	for _, each := range executors {
		work, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt(each.WorkspaceDirectory, nil))
		if err != nil {
			t.Fatal(err)
		}
		environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(each.ID, "", work))
		if err != nil {
			t.Fatal(err)
		}
		environments = append(environments, environment)
	}
	return environments
}

// TestTurnExecutorsChange 检查 Executor 变化记录、模型通知和保存顺序。
func TestTurnExecutorsChange(t *testing.T) {
	// 1. 准备不同模型、默认环境和工作区路径的变化组合。
	modelA := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "a",
		},
		Target: &llmkit.Target{Model: "a"},
	}
	modelB := &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    "b",
		},
		Target: &llmkit.Target{Model: "b"},
	}
	previousList := []defs.TurnExecutor{{
		ID:                 "local",
		WorkspaceDirectory: "/a",
	}, {
		ID:                 "openvaala",
		WorkspaceDirectory: "/workspace",
	}}
	tests := []struct {
		name          string
		previous      []defs.TurnExecutor
		previousModel defs.ModelRef
		current       []defs.TurnExecutor
		// wantKinds 是用户输入之前依次写入的 Item 种类。
		wantKinds []defs.ItemKind
	}{
		{
			name:     "new_default_added",
			previous: previousList,
			current: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "openvaala",
				WorkspaceDirectory: "/workspace",
			}},
			wantKinds: []defs.ItemKind{defs.ItemExecutorsChange},
		},
		{
			name:          "after_model_change",
			previous:      previousList,
			previousModel: modelA.Ref,
			current: []defs.TurnExecutor{{
				ID:                 "gpu",
				WorkspaceDirectory: "/data",
			}, {
				ID:                 "local",
				WorkspaceDirectory: "/a",
			}, {
				ID:                 "openvaala",
				WorkspaceDirectory: "/workspace",
			}},
			wantKinds: []defs.ItemKind{defs.ItemModelChange, defs.ItemExecutorsChange},
		},
		{
			name:     "removed_and_directory_changed_in_one_change",
			previous: previousList,
			current: []defs.TurnExecutor{{
				ID:                 "local",
				WorkspaceDirectory: "/b",
			}},
			wantKinds: []defs.ItemKind{defs.ItemExecutorsChange},
		},
		{
			name:      "all_removed",
			previous:  previousList,
			wantKinds: []defs.ItemKind{defs.ItemExecutorsChange},
		},
		{
			name:     "same_list",
			previous: previousList,
			current:  previousList,
		},
		{
			name:    "no_previous_turn",
			current: previousList,
		},
	}
	// 2. 运行不同环境配置，检查 running 记录、输入前变化 Item 和模型请求里的环境通知。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			recorder := &runningTurnRecorder{Mutation: store.mutation}
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			client := &summaryTurnClient{}
			opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, store.query, recorder, client)
			opt.Model, opt.CompactModel, opt.PreviousModel = modelB, modelB, tt.previousModel
			opt.Environments, opt.PreviousExecutors = newTurnEnvironments(t, tt.current), tt.previous
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			input := turnUserMessage("你好")
			if _, err := instance.Run(t.Context(), []*defs.Item{input}, nil); err != nil {
				t.Fatal(err)
			}

			if len(recorder.running) != 2 || !slices.Equal(recorder.running[0].Executors, tt.current) {
				t.Fatalf("running records = %+v, want two with executors %+v", recorder.running, tt.current)
			}
			page, err := store.query.ListItems(t.Context(), dao.ListItemsRequest{
				ConversationID: conversationID,
				SessionID:      sid,
				Order:          storage.DirectionAsc,
				Limit:          10,
			})
			if err != nil {
				t.Fatal(err)
			}
			inputIndex := slices.IndexFunc(page.Items, func(logged dao.LoggedItem) bool { return logged.Item.ID == input.ID })
			var gotKinds []defs.ItemKind
			for _, logged := range page.Items[:inputIndex] {
				gotKinds = append(gotKinds, logged.Item.Kind)
			}
			if !slices.Equal(gotKinds, tt.wantKinds) {
				t.Fatalf("items before input = %v, want %v", gotKinds, tt.wantKinds)
			}
			payload, _ := json.Marshal(client.requests[0].Messages)
			hasNotice := strings.Contains(string(payload), "Executor 已变更")
			if wantNotice := slices.Contains(tt.wantKinds, defs.ItemExecutorsChange); hasNotice != wantNotice {
				t.Fatalf("request has executors change notice = %v, want %v: %s", hasNotice, wantNotice, payload)
			}
			if !slices.Contains(tt.wantKinds, defs.ItemExecutorsChange) {
				return
			}
			change := page.Items[inputIndex-1].Item
			if change.TurnID != opt.TurnID || !slices.Equal(change.ExecutorsChange.PreviousExecutors, tt.previous) || !slices.Equal(change.ExecutorsChange.Executors, tt.current) {
				t.Fatalf("executors change = %+v (turn %s), want %+v -> %+v", change.ExecutorsChange, change.TurnID, tt.previous, tt.current)
			}
		})
	}
}

// TestTurnSteerFinishBoundary 检查结束前补充输入、取消和保存失败时的输入归属。
func TestTurnSteerFinishBoundary(t *testing.T) {
	// 1. 准备补充输入续跑、读取前取消和取走后保存失败的样例。
	for _, tt := range []struct {
		name string
		// canceled 是否在补充输入被读入前取消 Turn？
		canceled bool
		// failSteerWrite 取走的补充输入写库是否失败？
		failSteerWrite bool
		wantErr        bool
		wantInputs     int
		wantInboxItems int
		wantFinished   bool
	}{
		{
			name:         "steer_before_finish_continues_turn",
			wantInputs:   2,
			wantFinished: true,
		},
		{
			name:           "cancel_before_steer_is_read",
			canceled:       true,
			wantErr:        true,
			wantInputs:     1,
			wantInboxItems: 1,
		},
		{
			name:           "failed_steer_write_still_counts_input",
			failSteerWrite: true,
			wantErr:        true,
			wantInputs:     2,
		},
	} {
		// 2. 在模型结束前补充输入或取消，检查读入 ID、Inbox 余量及保存失败时的未结算错误。
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			client := &inputBoundaryClient{calls: make(chan chan llmkit.Event, 2)}
			inbox := &testTurnInbox{}
			steer := turnUserMessage("steer")
			var mutation dao.Mutation = store.mutation
			if tt.failSteerWrite {
				mutation = &failingItemMutation{
					Mutation:   store.mutation,
					failItemID: steer.ID,
				}
			}
			opt := turn.NewTurnOpt(defs.NewConversationID(), defs.NewSessionID(), &agent.AgentDefinition{Name: "assistant"}, store.query, mutation, client)
			opt.Model = newTestModel(&llmkit.Target{}, 0)
			opt.Inbox = inbox
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type runResult struct {
				result turn.TurnResult
				err    error
			}
			done := make(chan runResult, 1)
			first := turnUserMessage("first")
			go func() {
				result, err := instance.Run(ctx, []*defs.Item{first}, nil)
				done <- runResult{
					result: result,
					err:    err,
				}
			}()
			stream := <-client.calls
			inbox.add(steer)
			switch {
			case tt.canceled:
				cancel()
			case tt.failSteerWrite:
				stream <- llmkit.Event{Type: llmkit.EventDone}
			default:
				stream <- llmkit.Event{Type: llmkit.EventDone}
				next := <-client.calls
				next <- llmkit.Event{Type: llmkit.EventDone}
			}
			got := <-done
			if (got.err != nil) != tt.wantErr {
				t.Fatalf("Run() error = %v", got.err)
			}
			if tt.failSteerWrite && (got.result.Status != "" || !errors.Is(got.err, turn.ErrTurnUnsettled) || !strings.Contains(got.err.Error(), "append steer input")) {
				t.Fatalf("failed steer write = %+v, %v", got.result, got.err)
			}
			if !tt.wantErr && (got.result.FinalModelCallID == "" || got.result.Status != defs.TurnStatusCompleted) {
				t.Fatalf("missing terminal identity: %+v", got.result)
			}
			wantInputs := []defs.ItemID{first.ID, steer.ID}[:tt.wantInputs]
			if !slices.Equal(got.result.InputItemIDs, wantInputs) {
				t.Fatalf("InputItemIDs = %v, want %v", got.result.InputItemIDs, wantInputs)
			}
			pending, finished, turnIDs := inbox.get()
			if len(pending) != tt.wantInboxItems || finished != tt.wantFinished {
				t.Fatalf("inbox pending=%d finished=%v, want %d %v", len(pending), finished, tt.wantInboxItems, tt.wantFinished)
			}
			for _, turnID := range turnIDs {
				if turnID != opt.TurnID {
					t.Fatalf("TakeSteer turnID = %q, want %q", turnID, opt.TurnID)
				}
			}
			logged := readTurnLog(t, store.query, opt.ConversationID, opt.SessionID)
			if !tt.wantErr && (len(logged) != 2 || logged[1].ID != steer.ID || logged[1].TurnID != opt.TurnID) {
				t.Fatalf("log = %+v, want steer persisted in Turn", logged)
			}
		})
	}
}

// failingItemMutation 让包含 failItemID 的 Item 批量写入失败。
type failingItemMutation struct {
	dao.Mutation
	failItemID defs.ItemID
}

// AppendItems 拒绝包含指定 Item ID 的批次。
func (mutation *failingItemMutation) AppendItems(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, items ...*defs.Item) ([]dao.LoggedItem, error) {
	// 1. 逐个检查 ID，命中时返回测试错误，其他批次正常追加。
	for _, item := range items {
		if item.ID == mutation.failItemID {
			return nil, errors.New("injected steer write failure")
		}
	}
	return mutation.Mutation.AppendItems(ctx, conversationID, sessionID, items...)
}

// inputBoundaryClient 把每次调用的事件流交给测试，由测试决定何时发送事件。
type inputBoundaryClient struct{ calls chan chan llmkit.Event }

// Call 将可由测试发送事件的模型流交给调用方。
func (c *inputBoundaryClient) Call(ctx context.Context, _ llmkit.Request, _ llmkit.Target, _ llmkit.CallOptions) (<-chan llmkit.Event, error) {
	// 1. 创建事件通道并发送到测试控制通道。
	stream := make(chan llmkit.Event, 1)
	c.calls <- stream
	return stream, nil
}

// TestTurnForkSummaryHistory 检查继承历史和本地摘要覆盖后的模型请求。
func TestTurnForkSummaryHistory(t *testing.T) {
	// 1. 准备继承摘要、本地覆盖、跨页及本地覆盖点缺失的样例。
	for _, tt := range []struct {
		name string
		// localSummary 是否在 fork 之后追加一条覆盖到继承原文 A1 的本地摘要？
		localSummary bool
		// localBoundaryMissing 本地摘要是否指向不存在的 Item？是的话加载必须失败。
		localBoundaryMissing bool
		// localTailCount 是本地摘要前额外追加的 Item 数，数量够大时历史要跨页读取。
		localTailCount int
	}{
		{name: "inherited_missing_boundary"},
		{
			name:         "local_covers_inherited",
			localSummary: true,
		},
		{
			name:           "local_across_pages",
			localSummary:   true,
			localTailCount: 300,
		},
		{
			name:                 "local_missing_boundary",
			localSummary:         true,
			localBoundaryMissing: true,
		},
	} {
		// 2. 追加继承和本地历史后运行，检查本地摘要替换覆盖原文、分叉 Item 不进入请求。
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			conversationID := defs.NewConversationID()
			sid := defs.NewSessionID()
			history := []*defs.Item{turnSummary("S0", "omitted", "inherited summary")}
			for _, text := range []string{"A1", "A2"} {
				item := turnUserMessage(text)
				item.ID = defs.ItemID(text)
				item.TurnID = "source"
				history = append(history, item)
			}
			history = append(history, &defs.Item{
				ID:          defs.NewItemID(),
				Kind:        defs.ItemSessionFork,
				SessionFork: &defs.SessionFork{SourceSessionID: defs.NewSessionID()},
			})
			for _, text := range []string{"B1", "B2"} {
				item := turnUserMessage(text)
				item.TurnID = "local"
				history = append(history, item)
			}
			for i := range tt.localTailCount {
				item := turnUserMessage(fmt.Sprint(i))
				item.TurnID = "local"
				history = append(history, item)
			}
			if tt.localSummary {
				boundary := defs.ItemID("A1")
				if tt.localBoundaryMissing {
					boundary = "missing"
				}
				history = append(history, turnSummary("S1", boundary, "local summary"))
			}
			item := turnUserMessage("B3")
			item.TurnID = "local"
			history = append(history, item)
			for _, item := range history {
				if _, err := store.mutation.AppendItems(t.Context(), conversationID, sid, item); err != nil {
					t.Fatal(err)
				}
			}
			client := &summaryTurnClient{}
			opt := turn.NewTurnOpt(conversationID, sid, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, client)
			opt.Model = newTestModel(&llmkit.Target{}, 0)
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			_, err = instance.Run(t.Context(), []*defs.Item{turnUserMessage("new")}, nil)
			if (err != nil) != tt.localBoundaryMissing {
				t.Fatalf("run=%v", err)
			}
			if tt.localBoundaryMissing {
				return
			}
			payload, _ := json.Marshal(client.requests[0].Messages)
			text := string(payload)
			for _, want := range []string{"A2", "B1", "B2", "B3"} {
				if !strings.Contains(text, want) {
					t.Fatalf("lost %s: %s", want, text)
				}
			}
			if tt.localSummary && (strings.Contains(text, "A1") || strings.Contains(text, "inherited summary")) {
				t.Fatalf("covered history leaked: %s", text)
			}
			if strings.Contains(text, "session_fork") {
				t.Fatal("fork entered model")
			}
		})
	}
}

// newTestTurn 补齐本轮 ID、空收件箱和摘要模型后创建测试 Turn。
func newTestTurn(opt *turn.TurnOpt) (turn.Turn, error) {
	// 1. 缺少 TurnID、Inbox 或 CompactModel 时补齐，摘要默认使用本轮模型。
	if opt.TurnID == "" {
		opt.TurnID = defs.NewTurnID()
	}
	if opt.Inbox == nil {
		opt.Inbox = &testTurnInbox{}
	}
	if opt.CompactModel == nil {
		opt.CompactModel = opt.Model
	}
	return turn.NewTurn(opt)
}

// newTestModel 创建指定连接参数和窗口的测试模型。
func newTestModel(target *llmkit.Target, window int) *defs.ModelDefinition {
	// 1. 填写模型身份、请求目标和窗口。
	return &defs.ModelDefinition{
		Ref: defs.ModelRef{
			ProviderID: "test",
			ModelID:    target.Model,
		},
		Target:        target,
		ContextWindow: window,
	}
}

// testTurnInbox 模拟 Session 收件箱：finish 且为空时停止接收补充输入。
type testTurnInbox struct {
	mutex    sync.Mutex
	pending  []*defs.Item
	finished bool
	turnIDs  []defs.TurnID
}

// TakeSteer 取走测试补充输入并更新接收状态。
func (inbox *testTurnInbox) TakeSteer(_ context.Context, turnID defs.TurnID, finish bool) ([][]*defs.Item, error) {
	// 1. 加锁取出并清空待处理输入，记录 TurnID 和结束标记，再将每条输入组成一组。
	inbox.mutex.Lock()
	defer inbox.mutex.Unlock()
	inbox.turnIDs = append(inbox.turnIDs, turnID)
	items := inbox.pending
	inbox.pending = nil
	if finish && len(items) == 0 {
		inbox.finished = true
	}
	groups := make([][]*defs.Item, len(items))
	for i, item := range items {
		groups[i] = []*defs.Item{item}
	}
	return groups, nil
}

// add 向测试收件箱追加输入。
func (inbox *testTurnInbox) add(item *defs.Item) {
	// 1. 加锁追加待处理 Item。
	inbox.mutex.Lock()
	defer inbox.mutex.Unlock()
	inbox.pending = append(inbox.pending, item)
}

// get 读取测试收件箱的输入、结束标记和访问过的 Turn ID。
func (inbox *testTurnInbox) get() ([]*defs.Item, bool, []defs.TurnID) {
	// 1. 加锁读取待处理输入、结束标记及 Turn ID 记录。
	inbox.mutex.Lock()
	defer inbox.mutex.Unlock()
	return inbox.pending, inbox.finished, inbox.turnIDs
}

// TestTurnTelemetrySummary 检查本轮统计和内容采集开关对应的遥测。
func TestTurnTelemetrySummary(t *testing.T) {
	// 1. 准备开启和关闭内容采集的样例。
	tests := []struct {
		name           string
		captureContent bool
	}{
		{
			name:           "content_enabled",
			captureContent: true,
		},
		{name: "content_disabled"},
	}
	// 2. 运行带用量的回复，比较 span 中的角色说明、用量、结束原因和内容采集结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipelineWithoutContent(t)
			if tt.captureContent {
				p = telemetrytest.NewPipeline(t)
			}
			stores := openTurnStorage(t)
			opt := turn.NewTurnOpt(
				defs.NewConversationID(),
				defs.NewSessionID(),
				&agent.AgentDefinition{
					Name:        "assistant",
					Description: "通用助手",
				},
				stores.query,
				stores.mutation,
				&scriptedTurnClient{events: []llmkit.Event{
					{
						Type:  llmkit.EventContentDelta,
						Delta: &llmkit.DeltaPayload{Text: "答复"},
					},
					{
						Type: llmkit.EventUsage,
						Usage: &llmkit.Usage{
							PromptTokens:     100,
							CompletionTokens: 10,
						},
					},
					{
						Type:         llmkit.EventDone,
						FinishReason: "stop",
					},
				}},
			)
			opt.Model = newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				Model:    "gpt-test",
			}, 0)
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := instance.Run(telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), []*defs.Item{turnUserMessage("问题")}, nil); err != nil {
				t.Fatal(err)
			}
			span := p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0]
			for key, want := range map[string]string{
				"gen_ai.agent.description":       "通用助手",
				"gen_ai.request.model":           "gpt-test",
				"openvaala.turn.step_count":      "1",
				"gen_ai.usage.input_tokens":      "100",
				"gen_ai.usage.output_tokens":     "10",
				"gen_ai.response.finish_reasons": `["stop"]`,
			} {
				if got := telemetrytest.GetSpanAttribute(span, key); got != want {
					t.Errorf("span %s = %q, want %q", key, got, want)
				}
			}
			input, output := telemetrytest.GetSpanAttribute(span, "gen_ai.input.messages"), telemetrytest.GetSpanAttribute(span, "gen_ai.output.messages")
			if tt.captureContent != strings.Contains(input, "问题") || tt.captureContent != strings.Contains(output, "答复") {
				t.Fatalf("captureContent = %v, input = %q, output = %q", tt.captureContent, input, output)
			}
		})
	}
}

// TestTurnTelemetryOutputAfterSteer 检查补充输入续跑后的遥测输出是最后一条 Agent 回复。
func TestTurnTelemetryOutputAfterSteer(t *testing.T) {
	// 1. 准备普通回复和补充输入触发第二次回复的样例。
	tests := []struct {
		name       string
		steer      bool
		wantOutput string
		wantAbsent []string
	}{
		{
			name:       "steered_turn_reports_last_reply",
			steer:      true,
			wantOutput: "第二次答复",
			wantAbsent: []string{"第一次答复", "补充"},
		},
		{
			name:       "plain_turn_reports_only_reply",
			wantOutput: "第一次答复",
			wantAbsent: []string{"第二次答复", "补充"},
		},
	}
	// 2. 按样例在首个回复前加入补充输入，检查最终遥测输出只含最后一条 Agent 回复。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			stores := openTurnStorage(t)
			client := &inputBoundaryClient{calls: make(chan chan llmkit.Event, 2)}
			inbox := &testTurnInbox{}
			opt := turn.NewTurnOpt(
				defs.NewConversationID(),
				defs.NewSessionID(),
				&agent.AgentDefinition{Name: "assistant"},
				stores.query,
				stores.mutation,
				client,
			)
			opt.Model = newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				Model:    "gpt-test",
			}, 0)
			opt.Inbox = inbox
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := instance.Run(telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), []*defs.Item{turnUserMessage("问题")}, nil)
				done <- err
			}()
			first := <-client.calls
			if tt.steer {
				inbox.add(turnUserMessage("补充"))
			}
			first <- llmkit.Event{
				Type:  llmkit.EventContentDelta,
				Delta: &llmkit.DeltaPayload{Text: "第一次答复"},
			}
			first <- llmkit.Event{
				Type:         llmkit.EventDone,
				FinishReason: "stop",
			}
			if tt.steer {
				second := <-client.calls
				second <- llmkit.Event{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "第二次答复"},
				}
				second <- llmkit.Event{
					Type:         llmkit.EventDone,
					FinishReason: "stop",
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			output := telemetrytest.GetSpanAttribute(p.WaitEndedSpans(t, "invoke_agent assistant", 1)[0], "gen_ai.output.messages")
			if !strings.Contains(output, tt.wantOutput) {
				t.Errorf("gen_ai.output.messages = %q, want it to contain %q", output, tt.wantOutput)
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(output, absent) {
					t.Errorf("gen_ai.output.messages = %q, must not contain %q", output, absent)
				}
			}
		})
	}
}

// turnHookKey 标识替身 hook 要回复的事件：prompt 只在 UserPromptSubmit 时有值。
type turnHookKey struct {
	name   defs.HookEventName
	prompt string
}

// turnHooksStub 保存按事件、用户文本匹配的 hook 结果及已收到请求。
type turnHooksStub struct {
	outcomes map[turnHookKey]*hook.HookOutcome
	mutex    sync.Mutex
	requests []*hook.HookEventRequest
}

// RunEvent 按事件和用户文本返回 hook 结果，并发送运行记录。
func (stub *turnHooksStub) RunEvent(_ context.Context, request *hook.HookEventRequest, runUpdates chan<- *defs.HookRun) *hook.HookOutcome {
	// 1. 保存请求，查找预设结果，没有预设时返回空结果。
	stub.mutex.Lock()
	stub.requests = append(stub.requests, request)
	stub.mutex.Unlock()
	outcome, found := stub.outcomes[turnHookKey{
		name:   request.Event.Name,
		prompt: request.Event.Prompt,
	}]
	if !found {
		return &hook.HookOutcome{}
	}
	// 2. 每条保存记录先发送 running，再发送最终状态，返回结果。
	for _, run := range outcome.SavedRuns {
		running := *run
		running.Status = defs.HookRunRunning
		runUpdates <- &running
		runUpdates <- run
	}
	return outcome
}

// GetHookInfos 返回空的 hook 说明列表。
func (*turnHooksStub) GetHookInfos() []*hook.HookInfo {
	// 1. 返回空列表。
	return nil
}

// newTurnHookOutcome 创建带一条运行记录的 hook 结果。
func newTurnHookOutcome(event defs.HookEventName, status defs.HookRunStatus) *hook.HookOutcome {
	// 1. 创建运行记录，再按 blocked 或 stopped 状态填写拦下和结束标记。
	run := &defs.HookRun{
		ID:         defs.NewHookRunID(),
		Event:      event,
		ExecutorID: "local",
		SourcePath: "/hooks/test.yaml",
		Handler:    "test.sh",
		Status:     status,
	}
	return &hook.HookOutcome{
		IsBlocked:     status == defs.HookRunBlocked,
		IsTurnStopped: status == defs.HookRunStopped,
		SavedRuns:     []*defs.HookRun{run},
	}
}

// withTurnHookContext 为 hook 保存记录补上模型可见上下文。
func withTurnHookContext(outcome *hook.HookOutcome, text string) *hook.HookOutcome {
	// 1. 设置首条运行记录的 AdditionalContext 并返回结果。
	outcome.SavedRuns[0].AdditionalContext = text
	return outcome
}

// describeTurnLogItem 将日志 Item 转换为可比较的说明文本。
func describeTurnLogItem(item *defs.Item) string {
	// 1. 消息返回正文，hook 返回事件和状态，其他 Item 返回类别。
	switch item.Kind {
	case defs.ItemMessage:
		return item.Message.Content[0].Text.Text
	case defs.ItemHookRun:
		return fmt.Sprintf("hook:%s:%s", item.HookRun.Event, item.HookRun.Status)
	}
	return string(item.Kind)
}

// TestTurnHooks 检查启动、用户输入和补充输入 hook 的保存、拦截及事件。
func TestTurnHooks(t *testing.T) {
	// 1. 准备输入文本、附带材料、发送方和各类 hook 结果。
	type hookRequest struct {
		name   defs.HookEventName
		source string
		prompt string
	}
	tests := []struct {
		name            string
		startSource     string
		parentSessionID defs.SessionID
		// extraInputTexts 是首轮输入在 "first" 之后追加的文本块。
		extraInputTexts []string
		// attachedContextTexts 是首轮输入末尾追加的附带材料文本块（编辑器附上的文件等）。
		attachedContextTexts []string
		// steerTexts 是开始前已挂在收件箱上的补充输入，每条一组。
		steerTexts []string
		// isAgentInput、isAgentSteer 首轮输入、补充输入是否由 Agent 发来，而不是用户？
		isAgentInput, isAgentSteer bool
		outcomes                   map[turnHookKey]*hook.HookOutcome
		noHooks                    bool
		noExecutors                bool
		// wantLog 是日志里依次保存的 Item。
		wantLog []string
		// wantModelCalls 是模型收到的请求数。
		wantModelCalls int
		// wantModelText、wantNoModelText 是最后一次模型请求里必须出现、不能出现的文本。
		wantModelText, wantNoModelText []string
		// wantRequests 是 hook 依次收到的请求。
		wantRequests []hookRequest
		// wantRunEvents 是 Turn 自己触发的 hook 依次发出的 TurnHookRunEvent，格式为“状态:事件”。
		wantRunEvents []string
	}{
		{
			name: "prompt_submit_blocks_first_input",
			outcomes: map[turnHookKey]*hook.HookOutcome{{
				defs.HookEventUserPromptSubmit,
				"first",
			}: newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunBlocked)},
			wantLog:        []string{"hook:UserPromptSubmit:blocked"},
			wantModelCalls: 0,
			wantRequests: []hookRequest{{
				name:   defs.HookEventUserPromptSubmit,
				prompt: "first",
			}},
			wantRunEvents: []string{"running:UserPromptSubmit", "blocked:UserPromptSubmit"},
		},
		{
			name:            "prompt_joins_text_blocks_with_newline",
			extraInputTexts: []string{"more"},
			wantLog:         []string{"first", "answer"},
			wantModelCalls:  1,
			wantRequests: []hookRequest{{
				name:   defs.HookEventUserPromptSubmit,
				prompt: "first\nmore",
			}, {name: defs.HookEventStop}},
		},
		{
			name:                 "prompt_excludes_attached_context_blocks",
			extraInputTexts:      []string{"more"},
			attachedContextTexts: []string{"attached-file-content"},
			wantLog:              []string{"first", "answer"},
			wantModelCalls:       1,
			wantModelText:        []string{"attached-file-content"},
			wantRequests: []hookRequest{{
				name:   defs.HookEventUserPromptSubmit,
				prompt: "first\nmore",
			}, {name: defs.HookEventStop}},
		},
		{
			name: "prompt_submit_adds_context_after_input",
			outcomes: map[turnHookKey]*hook.HookOutcome{
				{
					defs.HookEventUserPromptSubmit,
					"first",
				}: withTurnHookContext(newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunCompleted), "ctx-first-input"),
			},
			wantLog:        []string{"first", "hook:UserPromptSubmit:completed", "answer"},
			wantModelCalls: 1,
			wantModelText:  []string{"ctx-first-input"},
			wantRequests: []hookRequest{{
				name:   defs.HookEventUserPromptSubmit,
				prompt: "first",
			}, {name: defs.HookEventStop}},
			wantRunEvents: []string{"running:UserPromptSubmit", "completed:UserPromptSubmit"},
		},
		{
			name:        "resumed_session_runs_session_start_before_input",
			startSource: "resume",
			outcomes: map[turnHookKey]*hook.HookOutcome{
				{name: defs.HookEventSessionStart}: newTurnHookOutcome(defs.HookEventSessionStart, defs.HookRunCompleted),
				{
					defs.HookEventUserPromptSubmit,
					"first",
				}: newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunCompleted),
			},
			wantLog:        []string{"hook:SessionStart:completed", "first", "hook:UserPromptSubmit:completed", "answer"},
			wantModelCalls: 1,
			wantRequests: []hookRequest{
				{
					name:   defs.HookEventSessionStart,
					source: "resume",
				},
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "first",
				},
				{name: defs.HookEventStop},
			},
			wantRunEvents: []string{"running:SessionStart", "completed:SessionStart", "running:UserPromptSubmit", "completed:UserPromptSubmit"},
		},
		{
			name:            "new_child_session_runs_subagent_start_without_source",
			startSource:     "startup",
			parentSessionID: "parent-session",
			outcomes:        map[turnHookKey]*hook.HookOutcome{{name: defs.HookEventSubagentStart}: newTurnHookOutcome(defs.HookEventSubagentStart, defs.HookRunCompleted)},
			wantLog:         []string{"hook:SubagentStart:completed", "first", "answer"},
			wantModelCalls:  1,
			wantRequests: []hookRequest{
				{name: defs.HookEventSubagentStart},
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "first",
				},
				{name: defs.HookEventSubagentStop},
			},
			wantRunEvents: []string{"running:SubagentStart", "completed:SubagentStart"},
		},
		{
			name:           "session_start_stops_turn_after_saving_input",
			startSource:    "startup",
			outcomes:       map[turnHookKey]*hook.HookOutcome{{name: defs.HookEventSessionStart}: newTurnHookOutcome(defs.HookEventSessionStart, defs.HookRunStopped)},
			wantLog:        []string{"hook:SessionStart:stopped", "first"},
			wantModelCalls: 0,
			wantRequests: []hookRequest{{
				name:   defs.HookEventSessionStart,
				source: "startup",
			}},
			wantRunEvents: []string{"running:SessionStart", "stopped:SessionStart"},
		},
		{
			name:       "blocked_steer_group_is_dropped_and_next_group_continues",
			steerTexts: []string{"second", "third"},
			outcomes: map[turnHookKey]*hook.HookOutcome{
				{
					defs.HookEventUserPromptSubmit,
					"second",
				}: withTurnHookContext(newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunBlocked), "ctx-blocked-steer"),
				{
					defs.HookEventUserPromptSubmit,
					"third",
				}: withTurnHookContext(newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunCompleted), "ctx-third-steer"),
			},
			wantLog:         []string{"first", "hook:UserPromptSubmit:blocked", "third", "hook:UserPromptSubmit:completed", "answer"},
			wantModelCalls:  1,
			wantModelText:   []string{"third", "ctx-third-steer"},
			wantNoModelText: []string{"second", "ctx-blocked-steer"},
			wantRequests: []hookRequest{
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "first",
				},
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "second",
				},
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "third",
				},
				{name: defs.HookEventStop},
			},
			wantRunEvents: []string{"running:UserPromptSubmit", "blocked:UserPromptSubmit", "running:UserPromptSubmit", "completed:UserPromptSubmit"},
		},
		{
			name:       "steer_group_stop_is_recorded_without_ending_turn",
			steerTexts: []string{"second"},
			outcomes: map[turnHookKey]*hook.HookOutcome{{
				defs.HookEventUserPromptSubmit,
				"second",
			}: newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunStopped)},
			wantLog: []string{"first", "second", "hook:UserPromptSubmit:stopped", "answer"},
			wantRequests: []hookRequest{
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "first",
				},
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "second",
				},
				{name: defs.HookEventStop},
			},
			wantModelCalls: 1,
			wantRunEvents:  []string{"running:UserPromptSubmit", "stopped:UserPromptSubmit"},
		},
		{
			name:         "agent_first_input_skips_prompt_hook",
			isAgentInput: true,
			outcomes: map[turnHookKey]*hook.HookOutcome{{
				defs.HookEventUserPromptSubmit,
				"first",
			}: newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunBlocked)},
			wantLog:        []string{"first", "answer"},
			wantModelCalls: 1,
			wantRequests:   []hookRequest{{name: defs.HookEventStop}},
		},
		{
			name:         "agent_steer_group_skips_prompt_hook",
			steerTexts:   []string{"second"},
			isAgentSteer: true,
			outcomes: map[turnHookKey]*hook.HookOutcome{{
				defs.HookEventUserPromptSubmit,
				"second",
			}: newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunBlocked)},
			wantLog: []string{"first", "second", "answer"},
			wantRequests: []hookRequest{
				{
					name:   defs.HookEventUserPromptSubmit,
					prompt: "first",
				},
				{name: defs.HookEventStop},
			},
			wantModelCalls: 1,
			wantModelText:  []string{"second"},
		},
		{
			name:        "no_executors_skips_hooks",
			startSource: "startup",
			noExecutors: true,
			outcomes: map[turnHookKey]*hook.HookOutcome{{
				defs.HookEventUserPromptSubmit,
				"first",
			}: newTurnHookOutcome(defs.HookEventUserPromptSubmit, defs.HookRunBlocked)},
			wantLog:        []string{"first", "answer"},
			wantModelCalls: 1,
		},
		{
			name:           "nil_hooks_skips_hooks",
			startSource:    "startup",
			noHooks:        true,
			wantLog:        []string{"first", "answer"},
			wantModelCalls: 1,
		},
	}
	// 2. 运行启动和输入 hook，检查日志顺序、模型可见文本、hook 请求与实时运行事件。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 按样例配置 hook、执行环境、首轮输入和补充输入，运行 Turn。
			store := openTurnStorage(t)
			client := &summaryTurnClient{}
			stub := &turnHooksStub{outcomes: tt.outcomes}
			conversationID, sessionID := defs.NewConversationID(), defs.NewSessionID()
			opt := turn.NewTurnOpt(conversationID, sessionID, &agent.AgentDefinition{Name: "assistant"}, store.query, store.mutation, client)
			opt.Model = newTestModel(&llmkit.Target{}, 0)
			opt.Session = defs.Session{
				ID:              sessionID,
				ConversationID:  conversationID,
				ParentSessionID: tt.parentSessionID,
			}
			opt.HookStartSource = tt.startSource
			if !tt.noHooks {
				opt.Hooks = stub
			}
			var environments []executor.ExecutionEnvironment
			if !tt.noExecutors {
				environments = newTurnEnvironments(t, []defs.TurnExecutor{{
					ID:                 "local",
					WorkspaceDirectory: "/a",
				}, {
					ID:                 "gpu",
					WorkspaceDirectory: "/data",
				}})
				opt.Environments = environments
			}
			inbox := &testTurnInbox{}
			input := turnUserMessage("first")
			if tt.isAgentInput {
				input.Message.From = &defs.Address{
					Kind: defs.IdentityAgent,
					ID:   "child",
				}
			}
			for _, text := range tt.extraInputTexts {
				input.Message.Content = append(input.Message.Content, &defs.Content{
					Kind: defs.ContentText,
					Text: &defs.TextContent{Text: text},
				})
			}
			for _, text := range tt.attachedContextTexts {
				input.Message.Content = append(input.Message.Content, &defs.Content{
					Kind: defs.ContentText,
					Text: &defs.TextContent{
						Text:      text,
						IsContext: true,
					},
				})
			}
			wantInputs := []defs.ItemID{input.ID}
			for _, text := range tt.steerTexts {
				steer := turnUserMessage(text)
				if tt.isAgentSteer {
					steer.Message.From = &defs.Address{
						Kind: defs.IdentityAgent,
						ID:   "child",
					}
				}
				inbox.add(steer)
				wantInputs = append(wantInputs, steer.ID)
			}
			opt.Inbox = inbox
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan turn.TurnEvent, 64)
			result, err := instance.Run(t.Context(), []*defs.Item{input}, events)
			close(events)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 检查被拦下的输入仍计入 InputItemIDs、本轮为 completed，最后一次模型请求包含或不含指定文本。
			if result.Status != defs.TurnStatusCompleted || !slices.Equal(result.InputItemIDs, wantInputs) {
				t.Fatalf("result status %q input IDs %v, want completed with %v", result.Status, result.InputItemIDs, wantInputs)
			}
			if len(client.requests) != tt.wantModelCalls {
				t.Fatalf("model calls = %d, want %d", len(client.requests), tt.wantModelCalls)
			}
			if tt.wantModelCalls > 0 {
				payload, err := json.Marshal(client.requests[len(client.requests)-1].Messages)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range tt.wantModelText {
					if !strings.Contains(string(payload), want) {
						t.Fatalf("last model request lacks %q: %s", want, payload)
					}
				}
				for _, unwanted := range tt.wantNoModelText {
					if strings.Contains(string(payload), unwanted) {
						t.Fatalf("last model request contains %q: %s", unwanted, payload)
					}
				}
			}
			// 3. 检查日志顺序和范围、hook 收到的请求，以及 Turn 发出的实时运行事件。
			var gotLog []string
			for _, item := range readTurnLog(t, store.query, conversationID, sessionID) {
				if item.TurnID != opt.TurnID {
					t.Fatalf("item %s turn = %q, want %q", item.ID, item.TurnID, opt.TurnID)
				}
				gotLog = append(gotLog, describeTurnLogItem(item))
			}
			if !slices.Equal(gotLog, tt.wantLog) {
				t.Fatalf("log = %v, want %v", gotLog, tt.wantLog)
			}
			record, _, err := store.query.GetTurn(t.Context(), sessionID, opt.TurnID)
			if err != nil || record.ItemsRange != (defs.TurnItemRange{
				FirstOrdinal: 1,
				LastOrdinal:  uint64(len(gotLog)),
			}) {
				t.Fatalf("hook range = %+v, %v", record.ItemsRange, err)
			}
			var gotRequests []hookRequest
			for _, request := range stub.requests {
				event := request.Event
				if request.ExecutorID != "local" || event.TurnID != opt.TurnID || event.Session.ID != sessionID || event.Session.ParentSessionID != tt.parentSessionID ||
					request.Workspace != environments[0].GetWorkspace() {
					t.Fatalf("request = %+v event = %+v, want the default executor, turn and session", request, event)
				}
				gotRequests = append(gotRequests, hookRequest{
					name:   event.Name,
					source: event.Source,
					prompt: event.Prompt,
				})
			}
			if !slices.Equal(gotRequests, tt.wantRequests) {
				t.Fatalf("hook requests = %v, want %v", gotRequests, tt.wantRequests)
			}
			var gotRunEvents []string
			for event := range events {
				if runEvent, ok := event.(*turn.TurnHookRunEvent); ok {
					gotRunEvents = append(gotRunEvents, fmt.Sprintf("%s:%s", runEvent.Run.Status, runEvent.Run.Event))
				}
			}
			if !slices.Equal(gotRunEvents, tt.wantRunEvents) {
				t.Fatalf("hook run events = %v, want %v", gotRunEvents, tt.wantRunEvents)
			}
		})
	}
}

// rangeTestMutation 记录每次 Turn 保存，可按配置让首批范围或终态保存失败，或在追加成功后仍返回错误。
type rangeTestMutation struct {
	dao.Mutation
	failStart, failTerminal, appendThenFail bool
	records                                 []defs.Turn
}

// SetTurn 记录保存请求，并注入首批范围或终态保存错误。
func (m *rangeTestMutation) SetTurn(ctx context.Context, record defs.Turn) error {
	// 1. 保存写入记录，按状态和范围检查失败开关，其他请求交给 DAO。
	m.records = append(m.records, record)
	if (m.failStart && record.Status == defs.TurnStatusRunning && record.ItemsRange.FirstOrdinal != 0) || (m.failTerminal && record.Status != defs.TurnStatusRunning) {
		return errors.New("injected range save failure")
	}
	return m.Mutation.SetTurn(ctx, record)
}

// AppendItems 可在实际追加成功后返回错误。
func (m *rangeTestMutation) AppendItems(ctx context.Context, conversationID defs.ConversationID, sessionID defs.SessionID, items ...*defs.Item) ([]dao.LoggedItem, error) {
	// 1. 先追加 Item，再按配置丢弃返回结果并返回错误。
	saved, err := m.Mutation.AppendItems(ctx, conversationID, sessionID, items...)
	if err == nil && m.appendThenFail {
		return nil, errors.New("append result lost after persistence")
	}
	return saved, err
}

// TestTurnItemsRangeWrites 检查首批范围、最终范围以及保存失败后的 running 记录。
func TestTurnItemsRangeWrites(t *testing.T) {
	// 1. 准备多批输出、首批范围失败、终态失败、追加后报错和空批次样例。
	for _, tt := range []struct {
		name                                           string
		failStart, failTerminal, appendThenFail, empty bool
	}{
		{name: "multiple_output_batches"}, {
			name:      "first_range_write_failure",
			failStart: true,
		}, {
			name:         "terminal_write_failure",
			failTerminal: true,
		},
		{
			name:           "append_landed_before_error",
			appendThenFail: true,
		}, {
			name:  "blocked_without_saved_hook",
			empty: true,
		},
	} {
		// 2. 注入各阶段保存错误，比较实际记录范围、写入次数和保存事件是否发出。
		t.Run(tt.name, func(t *testing.T) {
			store := openTurnStorage(t)
			mutation := &rangeTestMutation{
				Mutation:       store.mutation,
				failStart:      tt.failStart,
				failTerminal:   tt.failTerminal,
				appendThenFail: tt.appendThenFail,
			}
			opt := turn.NewTurnOpt(defs.NewConversationID(), defs.NewSessionID(), &agent.AgentDefinition{Name: "assistant"}, store.query, mutation, &scriptedTurnClient{events: []llmkit.Event{
				{
					Type:      llmkit.EventReasoningContentDelta,
					Reasoning: &llmkit.ReasoningContent{Content: []string{"thinking"}},
				},
				{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "reply"},
				}, {Type: llmkit.EventDone},
			}})
			opt.Model = newTestModel(&llmkit.Target{}, 0)
			if tt.empty {
				opt.Environments = newTurnEnvironments(t, []defs.TurnExecutor{{
					ID:                 "local",
					WorkspaceDirectory: "/a",
				}})
				opt.Hooks = &turnHooksStub{outcomes: map[turnHookKey]*hook.HookOutcome{{
					defs.HookEventUserPromptSubmit,
					"input",
				}: {IsBlocked: true}}}
			}
			instance, err := newTestTurn(opt)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan turn.TurnEvent, 64)
			_, runErr := instance.Run(t.Context(), []*defs.Item{turnUserMessage("input")}, events)
			close(events)
			failed := tt.failStart || tt.failTerminal || tt.appendThenFail
			if errors.Is(runErr, turn.ErrTurnUnsettled) != failed {
				t.Fatalf("Run = %v", runErr)
			}
			record, _, err := store.query.GetTurn(t.Context(), opt.SessionID, opt.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			if failed && (record.Status != defs.TurnStatusRunning || record.ItemsRange.LastOrdinal != 0) {
				t.Fatalf("failure settled %+v", record)
			}
			if tt.failStart || tt.appendThenFail {
				for event := range events {
					if _, ok := event.(*turn.TurnItemsSaved); ok {
						t.Fatal("published first batch without saved start")
					}
				}
			}
			if !failed {
				wantWrites, wantRange := 3, defs.TurnItemRange{
					FirstOrdinal: 1,
					LastOrdinal:  3,
				}
				if tt.empty {
					wantWrites, wantRange = 2, defs.TurnItemRange{}
				}
				if len(mutation.records) != wantWrites || record.ItemsRange != wantRange {
					t.Fatalf("writes=%d range=%+v, want %d %+v", len(mutation.records), record.ItemsRange, wantWrites, wantRange)
				}
			}
			for _, written := range mutation.records {
				if written.Status == defs.TurnStatusRunning && written.ItemsRange.LastOrdinal != 0 {
					t.Fatalf("running end persisted %+v", written)
				}
			}
		})
	}
}
