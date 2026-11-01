package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/executor/tools"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// TestAgentToolLoop 检查工具循环、并行结果和执行进度进入模型上下文。
func TestAgentToolLoop(t *testing.T) {
	// 1. 准备工具成功、失败、默认执行器和并行调用样例。
	tests := []*struct {
		name            string
		toolError       *defs.ToolError
		stream          bool
		parallel        bool
		defaultExecutor bool
	}{
		{
			name:   "results_continue_model_with_progress",
			stream: true,
		},
		{
			name: "results_continue_model_without_progress",
		},
		{
			name:            "default_executor_with_progress",
			stream:          true,
			defaultExecutor: true,
		},
		{
			name:            "default_executor_without_progress",
			defaultExecutor: true,
		},
		{
			name:     "parallel_tools_finish_in_reverse_order",
			stream:   true,
			parallel: true,
		},
		{
			name: "tool_failure_is_returned_to_model",
			toolError: &defs.ToolError{
				Code:    "not_found",
				Message: "没有找到资料",
			},
			stream: true,
		},
	}
	// 2. 运行两次模型调用，检查工具执行顺序、结果、进度和模型收到的历史。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				requests := make(chan *toolModelRequest, 4)
				server := httptest.NewServer(http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						request := &toolModelRequest{}
						if err := json.NewDecoder(r.Body).Decode(request); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						requests <- request
						w.Header().Set("Content-Type", "text/event-stream")
						if len(request.Messages) == 1 {
							_, _ = io.WriteString(w, toolCallResponse)
							return
						}
						_, _ = io.WriteString(w, toolAnswerResponse)
					}))
				defer server.Close()
				backend := &testToolExecutor{
					toolError: tt.toolError,
					parallel:  tt.parallel,
				}
				history := newTestContext(t, "", []*defs.Item{userMessage("查两份资料")})
				opt := agent.NewAgentOpt(testTurnID, testSessionID, llmkit.NewClient(llmkit.ClientOptions{}), newTestModel(&llmkit.Target{
					Protocol: llmkit.ProtocolOpenAIChat,
					BaseURL:  server.URL,
					Model:    "tool-model",
				}, 0), history)
				opt.ToolExecutor = backend
				if tt.defaultExecutor {
					backend.receivedCalls = make(chan string, 4)
					instance, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, &testTool{backend: backend}))
					if err != nil {
						t.Fatal(err)
					}
					opt.ToolExecutor = instance
				}
				instance, err := agent.NewAgent(opt)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var events chan *agent.AgentEvent
				if tt.stream {
					events = make(chan *agent.AgentEvent, 64)
				}
				result, err := instance.Run(ctx, events)
				if err != nil {
					t.Fatal(err)
				}
				items := result.Items
				requireSortableItemIDs(t, items)
				if len(items) != 5 || items[2].Kind != defs.ItemToolResult || items[3].Kind != defs.ItemToolResult || items[4].Message.Content[0].Text.Text != "查好了。" {
					t.Fatalf("unexpected output: %+v", items)
				}
				firstModelCallID := items[0].ModelCallID
				if firstModelCallID == "" || items[1].ModelCallID != firstModelCallID {
					t.Fatalf("tool calls have different model calls: %+v", items[:2])
				}
				for _, item := range items[2:4] {
					if item.TurnID != testTurnID || item.ModelCallID != firstModelCallID {
						t.Fatalf("tool result inherited wrong identity: %+v", item)
					}
				}
				if items[4].ModelCallID == "" || items[4].ModelCallID == firstModelCallID || result.FinalModelCallID != items[4].ModelCallID {
					t.Fatalf("second model response has wrong identity: %+v, final model call %q", items[4], result.FinalModelCallID)
				}
				if tt.defaultExecutor {
					for len(backend.receivedCalls) > 0 {
						backend.calls = append(backend.calls, <-backend.receivedCalls)
					}
					slices.Sort(backend.calls)
				}
				if !reflect.DeepEqual(backend.calls, []string{"call_a", "call_b"}) {
					t.Fatalf("execution order = %v", backend.calls)
				}
				if tt.parallel && (items[2].ToolResult.CallID != "call_b" || items[3].ToolResult.CallID != "call_a") {
					t.Fatalf("parallel result order = %s, %s", items[2].ToolResult.CallID, items[3].ToolResult.CallID)
				}
				first, second := <-requests, <-requests
				if len(first.Tools) != 1 || first.Tools[0].Function.Name != "lookup" || string(first.Tools[0].Function.Parameters) != `{"type":"object"}` {
					t.Fatalf("model tools = %+v", first.Tools)
				}
				var resultIDs []string
				var assistantCallIDs []string
				var assistantMessages int
				for _, message := range second.Messages {
					if message.Role == "assistant" {
						assistantMessages++
						for _, call := range message.ToolCalls {
							assistantCallIDs = append(assistantCallIDs, call.ID)
						}
					}
					if message.Role != "tool" {
						continue
					}
					resultIDs = append(resultIDs, message.ToolCallID)
					result := &defs.ToolResult{}
					if err := json.Unmarshal([]byte(message.Content), result); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(result.Error, tt.toolError) || string(result.Content) != `{"found":true}` || !result.Truncated {
						t.Fatalf("model received wrong result: %+v", result)
					}
				}
				if assistantMessages != 1 || !reflect.DeepEqual(assistantCallIDs, []string{"call_a", "call_b"}) {
					t.Fatalf("assistant messages = %d, tool calls = %v", assistantMessages, assistantCallIDs)
				}
				if tt.parallel && !reflect.DeepEqual(resultIDs, []string{"call_b", "call_a"}) {
					t.Fatalf("model tool result order = %v, want [call_b call_a]", resultIDs)
				}
				slices.Sort(resultIDs)
				if !reflect.DeepEqual(resultIDs, []string{"call_a", "call_b"}) {
					t.Fatalf("result IDs = %v", resultIDs)
				}
				if events != nil {
					var itemEvents []*agent.AgentEvent
					var progressIDs []string
					for event := range events {
						if event.Type == agent.AgentEventToolProgress {
							progress := event.ToolProgress
							if progress == nil || items[event.ItemIndex].Kind != defs.ItemToolCall || progress.CallID != items[event.ItemIndex].ToolCall.CallID ||
								string(progress.Content) != `"searching"` {
								t.Fatalf("wrong progress: %+v", event)
							}
							progressIDs = append(progressIDs, items[event.ItemIndex].ToolCall.CallID)
							continue
						}
						if tt.parallel && event.Type == agent.AgentEventItemStarted && event.Item.Kind == defs.ItemToolResult && len(progressIDs) != 2 {
							t.Fatal("a tool finished before both tools produced progress")
						}
						itemEvents = append(itemEvents, event)
					}
					assertAgentEvents(t, itemEvents, items)
					slices.Sort(progressIDs)
					if !reflect.DeepEqual(progressIDs, []string{"call_a", "call_b"}) {
						t.Fatalf("progress IDs = %v", progressIDs)
					}
				}
				if messages := contextMessages(t, history); len(messages) != 5 {
					t.Fatalf("history count = %d, want input, tool calls, two results and answer", len(messages))
				}
			})
	}
}

// TestAgentToolCallRequiresExecutor 检查模型请求工具但没有执行器时返回错误。
func TestAgentToolCallRequiresExecutor(t *testing.T) {
	// 1. 启动返回工具调用的模型服务，创建不带执行器的 Agent。
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, toolCallResponse)
		}))
	defer server.Close()
	history := newTestContext(t, "", []*defs.Item{userMessage("查询")})
	instance, err := agent.NewAgent(agent.NewAgentOpt(
		testTurnID,
		testSessionID,
		llmkit.NewClient(llmkit.ClientOptions{}),
		newTestModel(&llmkit.Target{
			Protocol: llmkit.ProtocolOpenAIChat,
			BaseURL:  server.URL,
			Model:    "tool-model",
		}, 0),
		history,
	))
	if err != nil {
		t.Fatal(err)
	}
	// 2. 运行 Agent，检查返回缺少执行器的错误。
	_, err = instance.Run(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "executor") {
		t.Fatalf("error = %v", err)
	}
}

// TestAgentRejectsDuplicateToolCallIDs 检查重复 CallID 在工具执行前被拒绝。
func TestAgentRejectsDuplicateToolCallIDs(t *testing.T) {
	// 1. 准备两个已完成但 CallID 相同的工具调用。
	client := &scriptedAgentClient{events: []llmkit.Event{
		{
			Type: llmkit.EventToolCallDelta,
			Delta: &llmkit.DeltaPayload{ToolCall: &llmkit.ToolCallDelta{
				ID:        "call-duplicate",
				Name:      "lookup",
				Arguments: `{}`,
			}},
		},
		{
			Type: llmkit.EventToolCallDelta,
			Delta: &llmkit.DeltaPayload{ToolCall: &llmkit.ToolCallDelta{
				ID:        "call-duplicate",
				Name:      "lookup",
				Arguments: `{}`,
			}},
		},
		{Type: llmkit.EventDone},
	}}
	history := newTestContext(t, "", []*defs.Item{userMessage("查询")})
	opt := agent.NewAgentOpt(
		testTurnID,
		testSessionID,
		client,
		newTestModel(&llmkit.Target{
			Protocol: llmkit.ProtocolOpenAIChat,
			Model:    "tool-model",
		}, 0),
		history,
	)
	backend := &testToolExecutor{}
	opt.ToolExecutor = backend
	instance, err := agent.NewAgent(opt)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 运行 Agent，检查重复调用错误以及执行器未收到调用。
	_, err = instance.Run(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), `duplicate tool call ID "call-duplicate"`) {
		t.Fatalf("Run() error = %v", err)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("executor received duplicate calls: %v", backend.calls)
	}
}

// TestAgentToolExecutionFailuresContinueModel 检查参数和执行错误作为工具结果继续交给模型。
func TestAgentToolExecutionFailuresContinueModel(t *testing.T) {
	// 1. 准备参数校验失败和执行失败的工具后端。
	tests := []*struct {
		name      string
		backend   *testToolExecutor
		errorCode string
	}{
		{
			name:      "invalid_arguments",
			backend:   &testToolExecutor{argumentsError: errors.New("cannot parse")},
			errorCode: "invalid_arguments",
		},
		{
			name:      "execution_failure",
			backend:   &testToolExecutor{runError: errors.New("connection lost")},
			errorCode: "execution_failed",
		},
	}
	// 2. 执行工具循环，检查错误码和下一次模型请求中的工具结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan *toolModelRequest, 2)
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					request := &toolModelRequest{}
					if err := json.NewDecoder(r.Body).Decode(request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					requests <- request
					w.Header().Set("Content-Type", "text/event-stream")
					if len(request.Messages) == 1 {
						_, _ = io.WriteString(w, toolCallResponse)
						return
					}
					_, _ = io.WriteString(w, toolAnswerResponse)
				}))
			defer server.Close()
			history := newTestContext(t, "", []*defs.Item{userMessage("查询")})
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, &testTool{backend: tt.backend}))
			if err != nil {
				t.Fatal(err)
			}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, llmkit.NewClient(llmkit.ClientOptions{}), newTestModel(&llmkit.Target{
				Protocol: llmkit.ProtocolOpenAIChat,
				BaseURL:  server.URL,
				Model:    "tool-model",
			}, 0), history)
			opt.ToolExecutor = toolExecutor
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			result, err := instance.Run(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var resultIDs []string
			for _, item := range result.Items {
				if item.Kind != defs.ItemToolResult {
					continue
				}
				resultIDs = append(resultIDs, item.ToolResult.CallID)
				if item.ToolResult.Error == nil || item.ToolResult.Error.Code != tt.errorCode {
					t.Fatalf("tool result = %+v, want error code %q", item.ToolResult, tt.errorCode)
				}
			}
			slices.Sort(resultIDs)
			if !reflect.DeepEqual(resultIDs, []string{"call_a", "call_b"}) {
				t.Fatalf("result IDs = %v", resultIDs)
			}
			first, second := <-requests, <-requests
			if len(first.Messages) != 1 {
				t.Fatalf("first request messages = %d", len(first.Messages))
			}
			var modelResultIDs []string
			for _, message := range second.Messages {
				if message.Role == "tool" {
					modelResultIDs = append(modelResultIDs, message.ToolCallID)
				}
			}
			slices.Sort(modelResultIDs)
			if !reflect.DeepEqual(modelResultIDs, []string{"call_a", "call_b"}) {
				t.Fatalf("model result IDs = %v", modelResultIDs)
			}
		})
	}
}

// TestAgentToolCancellationKeepsTerminalResults 检查工具执行中取消时，canceled 结果仍写入输出和上下文。
func TestAgentToolCancellationKeepsTerminalResults(t *testing.T) {
	// 1. 准备生成工具调用的模型和等待取消的执行器。
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, toolCallResponse)
		}))
	defer server.Close()
	backend := &testToolExecutor{waitForCancel: true}
	history := newTestContext(t, "", []*defs.Item{userMessage("查询")})
	opt := agent.NewAgentOpt(testTurnID, testSessionID, llmkit.NewClient(llmkit.ClientOptions{}), newTestModel(&llmkit.Target{
		Protocol: llmkit.ProtocolOpenAIChat,
		BaseURL:  server.URL,
		Model:    "tool-model",
	}, 0), history)
	opt.ToolExecutor = backend
	instance, err := agent.NewAgent(opt)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 收到工具进度后取消，检查输出和上下文都保留 canceled 结果。
	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan *agent.AgentEvent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range events {
			if event.Type == agent.AgentEventToolProgress {
				cancel()
			}
		}
	}()
	result, err := instance.Run(ctx, events)
	<-done
	if !errors.Is(err, context.Canceled) || result.FinalModelCallID != "" {
		t.Fatalf("error = %v, final model call = %q", err, result.FinalModelCallID)
	}
	var resultIDs []string
	for _, item := range result.Items {
		if item.Kind == defs.ItemToolResult {
			resultIDs = append(resultIDs, item.ToolResult.CallID)
			if item.ToolResult.Error == nil || item.ToolResult.Error.Code != "canceled" {
				t.Fatalf("canceled result = %+v", item.ToolResult)
			}
		}
	}
	if !reflect.DeepEqual(resultIDs, []string{"call_a", "call_b"}) {
		t.Fatalf("result IDs = %v", resultIDs)
	}
	var persistedResultIDs []string
	for _, message := range contextMessages(t, history) {
		if message.Role == llmkit.RoleTool {
			persistedResultIDs = append(persistedResultIDs, message.ToolCallID)
		}
	}
	if !reflect.DeepEqual(persistedResultIDs, resultIDs) {
		t.Fatalf("persisted result IDs = %v, want %v", persistedResultIDs, resultIDs)
	}
}

// toolModelRequest 是测试模型服务从 OpenAI Chat 请求体解析出的消息和工具。
type toolModelRequest struct {
	Messages []*struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []*struct {
			ID       string `json:"id"`
			Function *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []*struct {
		Function *struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
}

// scriptedAgentClient 用预设事件回复第一次模型调用，第二次调用返回错误。
type scriptedAgentClient struct {
	events  []llmkit.Event
	calls   int
	targets []llmkit.Target
	options []llmkit.CallOptions
}

// Call 记录调用参数并发送预设模型响应。
func (c *scriptedAgentClient) Call(
	_ context.Context,
	_ llmkit.Request,
	target llmkit.Target,
	options llmkit.CallOptions,
) (<-chan llmkit.Event, error) {
	// 1. 累计调用次数和目标、选项，重复调用时报错，否则发送预设事件并关闭通道。
	c.calls++
	c.targets = append(c.targets, target)
	c.options = append(c.options, options)
	if c.calls > 1 {
		return nil, errors.New("unexpected second model call")
	}
	events := make(chan llmkit.Event, len(c.events))
	for _, event := range c.events {
		events <- event
	}
	close(events)
	return events, nil
}

// testToolExecutor 是提供 lookup 工具的测试执行器，可按配置返回失败、等待取消或并行执行。
type testToolExecutor struct {
	toolError      *defs.ToolError
	argumentsError error
	runError       error
	waitForCancel  bool
	parallel       bool
	calls          []string
	finished       bool
	callsStarted   chan struct{}
	callsReleased  chan struct{}
	callBFinished  chan struct{}
	receivedCalls  chan string
}

// GetTools 返回测试查询工具定义。
func (e *testToolExecutor) GetTools(context.Context) ([]*defs.ToolDefinition, error) {
	// 1. 读取查询工具定义并组成列表。
	return []*defs.ToolDefinition{e.GetDefinition()}, nil
}

// GetDefinition 返回 lookup 工具的名称和参数定义。
func (e *testToolExecutor) GetDefinition() *defs.ToolDefinition {
	// 1. 填写查询工具名、说明和对象参数结构。
	return &defs.ToolDefinition{
		Name:        "lookup",
		Description: "查询资料",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

// Run 执行一批测试工具调用。
func (e *testToolExecutor) Run(ctx context.Context, calls []*defs.Item, events chan<- executor.ExecutorEvent) {
	// 1. 设置事件关闭和取消清理，并按并行模式创建启动、释放和结束通知通道。
	defer close(events)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.finished = false
	if e.parallel {
		e.callsStarted = make(chan struct{}, len(calls))
		e.callsReleased = make(chan struct{})
		e.callBFinished = make(chan struct{})
	}
	// 2. 逐个记录调用，按配置顺序或并行执行。
	var tasks sync.WaitGroup
	for _, callItem := range calls {
		call := callItem.ToolCall
		e.calls = append(e.calls, call.CallID)
		if e.parallel {
			tasks.Go(func() {
				e.execCall(ctx, callItem, events)
			})
			continue
		}
		e.execCall(ctx, callItem, events)
	}
	// 3. 并行时等待所有工具发出进度，再一起放行完成步骤。
	if e.parallel {
		for range calls {
			select {
			case <-e.callsStarted:
			case <-ctx.Done():
			}
		}
		close(e.callsReleased)
	}
	// 4. 等待所有执行任务结束，记录整批已完成，返回时关闭事件通道。
	tasks.Wait()
	e.finished = true
}

// execCall 执行一次测试调用，转发进度后发送工具结果。
func (e *testToolExecutor) execCall(ctx context.Context, callItem *defs.Item, events chan<- executor.ExecutorEvent) {
	// 1. 启动进度转发，将每条进度发给执行事件通道。
	progress := make(chan *executor.ExecutionProgressEvent)
	var forwarding sync.WaitGroup
	forwarding.Go(func() {
		for event := range progress {
			events <- event
			if e.parallel {
				e.callsStarted <- struct{}{}
			}
		}
	})
	// 2. 执行工具后关闭进度并等待转发，再发送结果和并行结束通知。
	result := (&testTool{backend: e}).Execute(ctx, callItem, progress)
	close(progress)
	forwarding.Wait()
	events <- &executor.ExecutionFinishedEvent{
		Result:             result.Result,
		AgentsInstructions: result.AgentsInstructions,
	}
	if e.parallel && callItem.ToolCall.CallID == "call_b" {
		close(e.callBFinished)
	}
}

// testTool 是按 testToolExecutor 的配置执行单个 lookup 调用的测试工具。
type testTool struct {
	backend *testToolExecutor
}

// GetDefinition 返回测试后端提供的工具定义。
func (t *testTool) GetDefinition() *defs.ToolDefinition {
	// 1. 从后端读取定义。
	return t.backend.GetDefinition()
}

// Execute 返回测试调用的进度和结果，可按配置等待、取消或失败。
func (t *testTool) Execute(ctx context.Context, callItem *defs.Item, progress chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 记录 CallID，检查工具名及参数 JSON，参数错误时返回错误结果。
	call := callItem.ToolCall
	backend := t.backend
	if backend.receivedCalls != nil {
		backend.receivedCalls <- call.CallID
	}
	argumentsError := backend.argumentsError
	if call.Name != "lookup" || !json.Valid([]byte(call.Arguments)) {
		argumentsError = fmt.Errorf("invalid tool invocation: %+v", call)
	}
	if argumentsError != nil {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  call.CallID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    "invalid_arguments",
				Message: argumentsError.Error(),
			},
		}}
	}
	// 2. 创建取消结果，发送进度并按配置等待其他调用或取消。
	canceledResult := func() *executor.ToolExecutionResult {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  call.CallID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    "canceled",
				Message: ctx.Err().Error(),
			},
		}}
	}
	select {
	case progress <- &executor.ExecutionProgressEvent{
		CallID:  call.CallID,
		Stream:  executor.ExecutionStreamTypeTerminal,
		Content: json.RawMessage(`"searching"`),
	}:
	case <-ctx.Done():
		return canceledResult()
	}
	if backend.parallel {
		select {
		case <-backend.callsReleased:
		case <-ctx.Done():
			return canceledResult()
		}
		if call.CallID == "call_a" {
			select {
			case <-backend.callBFinished:
			case <-ctx.Done():
				return canceledResult()
			}
		}
	}
	if backend.waitForCancel {
		<-ctx.Done()
		return canceledResult()
	}
	// 3. 返回配置的执行错误或查询结果。
	if backend.runError != nil {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  call.CallID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    "execution_failed",
				Message: backend.runError.Error(),
			},
		}}
	}
	return &executor.ToolExecutionResult{Result: &defs.ToolResult{
		CallID:    call.CallID,
		Content:   json.RawMessage(`{"found":true}`),
		Error:     backend.toolError.Clone(),
		Truncated: true,
	}}
}

const toolCallResponse = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}"}},{"index":1,"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

const toolAnswerResponse = `data: {"choices":[{"delta":{"content":"查好了。"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

// TestAgentReadFileInstructions 检查文件读取后的目录规则和工具结果顺序。
func TestAgentReadFileInstructions(t *testing.T) {
	// 1. 逐个读取普通文件、AGENTS.md 和 SKILL.md。
	for _, target := range []string{"pkg/a.txt", "pkg/AGENTS.md", "pkg/SKILL.md"} {
		t.Run(target, func(t *testing.T) {
			// 1. 准备工作区文件、read_file 工具和两次模型响应。
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "pkg"), 0700); err != nil {
				t.Fatal(err)
			}
			for path, text := range map[string]string{"AGENTS.md": "root-rules", "pkg/AGENTS.md": "nested-rules", "pkg/a.txt": "file-content", "pkg/SKILL.md": "skill-content"} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			work, err := workspace.NewLocalWorkspace(workspace.NewLocalWorkspaceOpt(root))
			if err != nil {
				t.Fatal(err)
			}
			tool, err := tools.NewReadFileTool(tools.NewReadFileToolOpt(work))
			if err != nil {
				t.Fatal(err)
			}
			environment := newTestEnvironment(t, "local", work, tool)
			client := &sequenceAgentClient{responses: [][]llmkit.Event{
				{{
					Type: llmkit.EventToolCallEnd,
					ToolCall: &llmkit.StreamingToolCall{
						CallID:    "a",
						Name:      "read_file",
						Arguments: fmt.Sprintf(`{"path":%q}`, target),
					},
				}, {
					Type: llmkit.EventToolCallEnd,
					ToolCall: &llmkit.StreamingToolCall{
						CallID:    "b",
						Name:      "read_file",
						Arguments: `{"path":"pkg/a.txt"}`,
					},
				}, {
					Type:         llmkit.EventDone,
					FinishReason: "tool_calls",
				}},
				{{
					Type:  llmkit.EventContentDelta,
					Delta: &llmkit.DeltaPayload{Text: "done"},
				}, {
					Type:         llmkit.EventDone,
					FinishReason: "stop",
				}},
			}}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("read")}))
			opt.ToolExecutor = newTestToolExecutor(t, environment)
			opt.Environments = []executor.ExecutionEnvironment{environment}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 读取两个文件，检查全部工具结果之后才追加去重规则，SKILL.md 保持普通文件。
			result, err := instance.Run(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			rules, results := 0, 0
			for _, item := range result.Items {
				if item.ToolResult != nil {
					results++
					if item.ToolResult.Error != nil {
						t.Fatal(item.ToolResult.Error)
					}
				}
				if item.AgentsInstruction != nil {
					rules++
					if item.AgentsInstruction.Path == filepath.Join(root, "pkg/AGENTS.md") && results != 2 {
						t.Fatal("nested rules inserted between tool results")
					}
				}
				if item.SkillInstruction != nil {
					t.Fatal("read_file created a selected skill instruction")
				}
			}
			if rules != 2 || results != 2 {
				t.Fatalf("rules=%d results=%d", rules, results)
			}
			req := client.requests[1]
			toolMessages := 0
			for _, message := range req.Messages {
				if message.Role == llmkit.RoleTool {
					toolMessages++
				}
				if message.Role == llmkit.RoleUser {
					for _, block := range message.Content {
						if strings.Contains(block.Text, "nested-rules") && toolMessages != 2 {
							t.Fatal("model saw rules before all results")
						}
					}
				}
			}
		})
	}
}

// TestAgentProbesRulesAfterToolBatch 检查工具进度交付和整批执行结束后才读取目录规则。
func TestAgentProbesRulesAfterToolBatch(t *testing.T) {
	// 1. 准备正常执行和收到进度时取消的样例。
	for _, tt := range []struct {
		name   string
		cancel bool
	}{
		{name: "progress_delivered_before_probe"},
		{
			name:   "canceled_batch_does_not_probe",
			cancel: true,
		},
	} {
		// 2. 交付进度后结束或取消整批，检查非默认规则读取和下一次模型请求。
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			backend := &progressBatchExecutor{
				finished:          make(chan struct{}),
				progressDelivered: make(chan struct{}),
			}
			work, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/project", map[string]*workspace.VirtualFile{
				"/project/.git/config": {Content: []byte("git")},
				"/project/AGENTS.md":   {Content: []byte("batch-rules-marker")},
			}))
			if err != nil {
				t.Fatal(err)
			}
			observed := &afterToolsWorkspace{
				Workspace: work,
				finished:  backend.finished,
			}
			client := &sequenceAgentClient{responses: [][]llmkit.Event{
				{
					{
						Type: llmkit.EventToolCallEnd,
						ToolCall: &llmkit.StreamingToolCall{
							CallID:    "first",
							Name:      "lookup",
							Arguments: `{}`,
						},
					},
					{
						Type: llmkit.EventToolCallEnd,
						ToolCall: &llmkit.StreamingToolCall{
							CallID:    "second",
							Name:      "lookup",
							Arguments: `{}`,
						},
					},
					{
						Type:         llmkit.EventDone,
						FinishReason: "tool_calls",
					},
				},
				newToolLoopResponses()[1],
			}}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("运行任务")}))
			opt.ToolExecutor = backend
			// 默认 Executor 不提供工作区，remote 规则在工具成功后读取。
			opt.Environments = []executor.ExecutionEnvironment{newTestEnvironment(t, "local", nil), newTestEnvironment(t, "remote", observed)}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan *agent.AgentEvent)
			progressReceived := false
			var tasks sync.WaitGroup
			tasks.Go(func() {
				for event := range events {
					if event.Type == agent.AgentEventToolProgress {
						progressReceived = true
						if tt.cancel {
							cancel()
						}
						close(backend.progressDelivered)
					}
				}
			})
			_, err = instance.Run(ctx, events)
			tasks.Wait()
			if !progressReceived {
				t.Fatal("tool progress was not delivered")
			}
			if tt.cancel {
				if !errors.Is(err, context.Canceled) || observed.metadataCalls != 0 {
					t.Fatalf("error = %v, discovery calls = %d", err, observed.metadataCalls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(client.requests) != 2 {
				t.Fatalf("model requests = %d, want 2", len(client.requests))
			}
			messages := client.requests[1].Messages
			if len(messages) != 5 || messages[2].Role != llmkit.RoleTool || messages[3].Role != llmkit.RoleTool ||
				messages[4].Role != llmkit.RoleUser || !strings.Contains(messages[4].Content[0].Text, "batch-rules-marker") {
				t.Fatalf("model history = %+v, want both tool results followed by rules", messages)
			}
		})
	}
}

// TestAgentToolHookRuns 检查工具 hook 实时记录、保存顺序和停止本轮的结果。
func TestAgentToolHookRuns(t *testing.T) {
	// 1. 准备 PreToolUse、PostToolUse 的实时记录、保存记录和工具结果。
	newRun := func(id string, event defs.HookEventName, status defs.HookRunStatus) *defs.HookRun {
		return &defs.HookRun{
			ID:         defs.HookRunID(id),
			Event:      event,
			CallID:     "call-1",
			ExecutorID: "local",
			SourcePath: "/hooks/tool.yaml",
			Handler:    "tool.sh",
			Status:     status,
		}
	}
	preRun := newRun("pre", defs.HookEventPreToolUse, defs.HookRunCompleted)
	preRun.AdditionalContext = "pre-context"
	postRun := newRun("post", defs.HookEventPostToolUse, defs.HookRunBlocked)
	postRun.BlockReason = "post-feedback"
	okResult := &defs.ToolResult{Content: json.RawMessage(`{"found":true}`)}
	blockedResult := &defs.ToolResult{
		Content: json.RawMessage("null"),
		Error: &defs.ToolError{
			Code:    "hook_blocked",
			Message: "blocked by PreToolUse hook: no",
		},
	}
	tests := []*struct {
		name      string
		result    *defs.ToolResult
		liveRuns  []*defs.HookRun
		savedRuns []*defs.HookRun
		// wantFeedback 是第二次模型请求里，工具结果之后依次出现的 hook 反馈文本。
		wantFeedback   []string
		wantItemKinds  []defs.ItemKind
		wantModelCalls int
	}{
		{
			name:   "saved_runs_follow_the_result_in_order",
			result: okResult,
			liveRuns: []*defs.HookRun{
				newRun("pre", defs.HookEventPreToolUse, defs.HookRunRunning), preRun,
				newRun("post", defs.HookEventPostToolUse, defs.HookRunRunning), postRun,
			},
			savedRuns:      []*defs.HookRun{preRun, postRun},
			wantFeedback:   []string{"pre-context", "PostToolUse hook 对这次工具结果的反馈：post-feedback"},
			wantItemKinds:  []defs.ItemKind{defs.ItemToolCall, defs.ItemToolResult, defs.ItemHookRun, defs.ItemHookRun, defs.ItemMessage},
			wantModelCalls: 2,
		},
		{
			name:           "live_runs_without_saved_records",
			result:         okResult,
			liveRuns:       []*defs.HookRun{newRun("pre", defs.HookEventPreToolUse, defs.HookRunRunning), newRun("pre", defs.HookEventPreToolUse, defs.HookRunCompleted)},
			wantItemKinds:  []defs.ItemKind{defs.ItemToolCall, defs.ItemToolResult, defs.ItemMessage},
			wantModelCalls: 2,
		},
		{
			name:           "stopped_record_ends_the_turn_after_the_batch",
			result:         okResult,
			savedRuns:      []*defs.HookRun{newRun("stopped", defs.HookEventPostToolUse, defs.HookRunStopped)},
			wantItemKinds:  []defs.ItemKind{defs.ItemToolCall, defs.ItemToolResult, defs.ItemHookRun},
			wantModelCalls: 1,
		},
		{
			name:           "blocked_and_stopped_pre_tool_use_ends_the_turn",
			result:         blockedResult,
			savedRuns:      []*defs.HookRun{newRun("stopped", defs.HookEventPreToolUse, defs.HookRunStopped)},
			wantItemKinds:  []defs.ItemKind{defs.ItemToolCall, defs.ItemToolResult, defs.ItemHookRun},
			wantModelCalls: 1,
		},
	}
	// 2. 逐例运行 Agent，检查输出、事件和下一次模型请求。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 运行 Agent，检查输出种类和模型调用次数。
			client := &sequenceAgentClient{responses: newToolLoopResponses()[:tt.wantModelCalls]}
			opt := agent.NewAgentOpt(testTurnID, testSessionID, client, testModel, newTestContext(t, "", []*defs.Item{userMessage("查询")}))
			opt.ToolExecutor = &hookRunExecutor{
				result:    tt.result,
				liveRuns:  tt.liveRuns,
				savedRuns: tt.savedRuns,
			}
			instance, err := agent.NewAgent(opt)
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan *agent.AgentEvent, 256)
			result, err := instance.Run(t.Context(), events)
			if err != nil {
				t.Fatal(err)
			}
			var allEvents []*agent.AgentEvent
			for event := range events {
				allEvents = append(allEvents, event)
			}
			var kinds []defs.ItemKind
			for _, item := range result.Items {
				kinds = append(kinds, item.Kind)
			}
			if !reflect.DeepEqual(kinds, tt.wantItemKinds) {
				t.Fatalf("item kinds = %v, want %v", kinds, tt.wantItemKinds)
			}
			if len(client.requests) != tt.wantModelCalls {
				t.Fatalf("model calls = %d, want %d", len(client.requests), tt.wantModelCalls)
			}
			// 2. 检查保存的 hook 记录紧跟工具结果且属于同一次模型调用，事件和输出一致，最终模型调用是最后一个 Item 的。
			resultItem := result.Items[1]
			for index, run := range tt.savedRuns {
				item := result.Items[2+index]
				if item.TurnID != resultItem.TurnID || item.ModelCallID != resultItem.ModelCallID || !reflect.DeepEqual(item.HookRun, run) {
					t.Fatalf("hook run item %d = %+v, want run %+v after result %+v", index, item, run, resultItem)
				}
			}
			assertAgentEvents(t, allEvents, result.Items)
			wantFinal := result.Items[len(result.Items)-1].ModelCallID
			if result.FinalModelCallID != wantFinal {
				t.Fatalf("final model call = %q, want %q", result.FinalModelCallID, wantFinal)
			}
			// 3. 检查实时记录不带 Item，且在工具结果开始事件之前发送。
			liveEvents := getAgentEventsOfType(allEvents, agent.AgentEventHookRun)
			if len(liveEvents) != len(tt.liveRuns) {
				t.Fatalf("hook run events = %d, want %d", len(liveEvents), len(tt.liveRuns))
			}
			for index, event := range liveEvents {
				if event.Item != nil || !reflect.DeepEqual(event.HookRun, tt.liveRuns[index]) {
					t.Fatalf("hook run event %d = %+v, want run %+v without item", index, event, tt.liveRuns[index])
				}
			}
			resultStarted := slices.IndexFunc(allEvents, func(event *agent.AgentEvent) bool {
				return event.Type == agent.AgentEventItemStarted && event.Item.Kind == defs.ItemToolResult
			})
			for _, event := range liveEvents {
				if slices.Index(allEvents, event) > resultStarted {
					t.Fatalf("hook run event %+v is sent after the tool result starts at %d", event, resultStarted)
				}
			}
			// 4. 检查第二次模型请求里，工具结果之后依次是预期的 hook 反馈。
			if len(tt.wantFeedback) > 0 {
				messages := client.requests[1].Messages
				toolIndex := slices.IndexFunc(messages, func(message llmkit.Message) bool { return message.Role == llmkit.RoleTool })
				var feedback []llmkit.Message
				if toolIndex >= 0 {
					feedback = messages[toolIndex+1:]
				}
				if len(feedback) != len(tt.wantFeedback) {
					t.Fatalf("messages after the tool result = %+v, want %d hook feedback messages", feedback, len(tt.wantFeedback))
				}
				for index, message := range feedback {
					if message.Role != llmkit.RoleUser || !strings.Contains(message.Content[0].Text, tt.wantFeedback[index]) {
						t.Fatalf("hook feedback %d = %+v, want text %q", index, message, tt.wantFeedback[index])
					}
				}
			}
		})
	}
}

// hookRunExecutor 保存工具结果及实时、待保存的 hook 记录。
type hookRunExecutor struct {
	testToolExecutor
	result    *defs.ToolResult
	liveRuns  []*defs.HookRun
	savedRuns []*defs.HookRun
}

// Run 发送工具 hook 的实时记录和执行结果。
func (e *hookRunExecutor) Run(_ context.Context, calls []*defs.Item, events chan<- executor.ExecutorEvent) {
	// 1. 依次克隆发送实时记录，再填写本次 CallID 并发送完成事件；退出时关闭通道。
	defer close(events)
	for _, call := range calls {
		for _, run := range e.liveRuns {
			events <- &executor.ExecutionHookRunEvent{Run: run.Clone()}
		}
		result := *e.result
		result.CallID = call.ToolCall.CallID
		events <- &executor.ExecutionFinishedEvent{
			Result:   &result,
			HookRuns: e.savedRuns,
		}
	}
}

// progressBatchExecutor 保存整批结束和进度交付的通知通道。
type progressBatchExecutor struct {
	testToolExecutor
	finished          chan struct{}
	progressDelivered chan struct{}
}

// Run 发送两个工具调用的进度和结果。
func (e *progressBatchExecutor) Run(ctx context.Context, calls []*defs.Item, events chan<- executor.ExecutorEvent) {
	// 1. 发送第一个结果和第二个调用的进度。
	defer close(events)
	defer close(e.finished)
	events <- &executor.ExecutionFinishedEvent{Result: &defs.ToolResult{
		CallID:   calls[0].ToolCall.CallID,
		Executor: "remote",
		Content:  json.RawMessage(`"first result"`),
	}}
	events <- &executor.ExecutionProgressEvent{
		CallID:  calls[1].ToolCall.CallID,
		Stream:  executor.ExecutionStreamTypeTerminal,
		Content: json.RawMessage(`"progress"`),
	}
	// 2. 等待进度交付或取消，再发送第二个结果；退出时关闭整批通知和事件通道。
	select {
	case <-e.progressDelivered:
	case <-ctx.Done():
	}
	events <- &executor.ExecutionFinishedEvent{Result: &defs.ToolResult{
		CallID:   calls[1].ToolCall.CallID,
		Executor: "remote",
		Content:  json.RawMessage(`"second result"`),
	}}
}

// afterToolsWorkspace 记录工作区查询次数，并持有工具批次结束通知。
type afterToolsWorkspace struct {
	workspace.Workspace
	finished      <-chan struct{}
	metadataCalls int
}

// GetMetadata 检查工具批次状态并查询文件信息。
func (w *afterToolsWorkspace) GetMetadata(ctx context.Context, path string, followSymlinks bool) (*workspace.FileMetadata, error) {
	// 1. 累计查询次数，批次已结束时查询工作区，否则返回读取过早错误。
	w.metadataCalls++
	select {
	case <-w.finished:
		return w.Workspace.GetMetadata(ctx, path, followSymlinks)
	default:
		return nil, errors.New("rules probed before tool batch finished")
	}
}
