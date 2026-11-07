package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/telemetry/telemetrytest"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// echoTool 返回所在环境的 ID 和收到的参数；errorCode 非空时改为返回带这个错误码的失败结果。
type echoTool struct {
	executorID string
	name       string
	schema     string
	errorCode  string
}

// GetDefinition 返回回显工具的名称、说明和参数定义。
func (t *echoTool) GetDefinition() *defs.ToolDefinition {
	// 1. 按测试字段构造工具定义。
	return &defs.ToolDefinition{
		Name:        t.name,
		Description: t.name + " 工具",
		InputSchema: json.RawMessage(t.schema),
	}
}

// Execute 返回预设的失败结果，或回显环境 ID 和参数。
func (t *echoTool) Execute(_ context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 有预设错误码时返回失败，否则编码环境及收到的参数。
	if t.errorCode != "" {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  call.ToolCall.CallID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    t.errorCode,
				Message: "exit 1",
			},
		}}
	}
	content, _ := json.Marshal(map[string]string{"executor": t.executorID, "arguments": call.ToolCall.Arguments})
	return &executor.ToolExecutionResult{Result: &defs.ToolResult{
		CallID:  call.ToolCall.CallID,
		Content: content,
	}}
}

const echoSchema = `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`

// labelTool 执行时返回构造时给的 label，同名工具靠它区分属于哪个 ToolExecutor。
type labelTool struct {
	name  string
	label string
}

// GetDefinition 返回标签工具的定义。
func (t *labelTool) GetDefinition() *defs.ToolDefinition {
	// 1. 填入名称、说明和回显参数 schema。
	return &defs.ToolDefinition{
		Name:        t.name,
		Description: t.name + " 工具",
		InputSchema: json.RawMessage(echoSchema),
	}
}

// Execute 返回预设标签。
func (t *labelTool) Execute(_ context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 编码标签并附带调用 ID。
	content, _ := json.Marshal(t.label)
	return &executor.ToolExecutionResult{Result: &defs.ToolResult{
		CallID:  call.ToolCall.CallID,
		Content: content,
	}}
}

type fileArguments struct {
	Path string `json:"path"`
}

// fileTool 先发送 reading 进度，等 release 关闭后读取文件，取消时返回 canceled；返回结果前都发送 done 进度。
type fileTool struct {
	definition *defs.ToolDefinition
	root       string
	release    chan struct{}
}

// GetDefinition 返回测试指定的工具定义。
func (t *fileTool) GetDefinition() *defs.ToolDefinition {
	// 1. 读取保存的工具定义。
	return t.definition
}

// Execute 发送进度，等待放行后读取文件并返回正文。
func (t *fileTool) Execute(ctx context.Context, call *defs.Item, progress chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 解析文件路径参数，失败时返回 invalid_arguments。
	callID := call.ToolCall.CallID
	arguments := &fileArguments{}
	if err := json.Unmarshal([]byte(call.ToolCall.Arguments), arguments); err != nil {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  callID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    "invalid_arguments",
				Message: err.Error(),
			},
		}}
	}
	// 2. 发送 reading，登记返回前的 done 进度，再等待放行或取消。
	progress <- &executor.ExecutionProgressEvent{
		CallID:  callID,
		Stream:  executor.ExecutionStreamTypeTerminal,
		Content: json.RawMessage(`"reading"`),
	}
	defer func() {
		progress <- &executor.ExecutionProgressEvent{
			CallID:  callID,
			Stream:  executor.ExecutionStreamTypeTerminal,
			Content: json.RawMessage(`"done"`),
		}
	}()
	select {
	case <-ctx.Done():
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  callID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    "canceled",
				Message: ctx.Err().Error(),
			},
		}}
	case <-t.release:
	}
	// 3. 读取文件，编码正文或返回 read_failed。
	content, err := os.ReadFile(filepath.Join(t.root, arguments.Path))
	if err != nil {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  callID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    "read_failed",
				Message: err.Error(),
			},
		}}
	}
	encoded, _ := json.Marshal(string(content))
	return &executor.ToolExecutionResult{Result: &defs.ToolResult{
		CallID:  callID,
		Content: encoded,
	}}
}

// identityTool 把收到的调用 Item 复制一份发到 received，再就地改掉它的参数。
type identityTool struct {
	received chan *defs.Item
}

// GetDefinition 返回记录调用 Item 的工具定义。
func (t *identityTool) GetDefinition() *defs.ToolDefinition {
	// 1. 创建 file 工具的对象参数定义。
	return &defs.ToolDefinition{
		Name:        "file",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

// Execute 记录调用副本，再修改收到的参数。
func (t *identityTool) Execute(_ context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 发送调用副本，修改参数并返回成功结果。
	t.received <- call.Clone()
	call.ToolCall.Arguments = "changed"
	return &executor.ToolExecutionResult{Result: &defs.ToolResult{
		CallID:  call.ToolCall.CallID,
		Content: json.RawMessage(`null`),
	}}
}

// toolProbe 记录各 probeTool 实际收到的参数；没有记录说明工具没被调用。
type toolProbe struct {
	mu        sync.Mutex
	arguments []string
}

// record 记录工具收到的参数。
func (p *toolProbe) record(arguments string) {
	// 1. 加锁追加参数。
	p.mu.Lock()
	defer p.mu.Unlock()
	p.arguments = append(p.arguments, arguments)
}

// getArguments 返回已记录参数的副本。
func (p *toolProbe) getArguments() []string {
	// 1. 加锁复制参数列表。
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.arguments)
}

// probeTool 把收到的参数记进 probe；failureCode 非空时返回带该错误码的失败结果。
type probeTool struct {
	name        string
	failureCode string
	probe       *toolProbe
}

// GetDefinition 返回参数探测工具的定义。
func (t *probeTool) GetDefinition() *defs.ToolDefinition {
	// 1. 按测试名称构造回显参数定义。
	return &defs.ToolDefinition{
		Name:        t.name,
		Description: t.name + " 工具",
		InputSchema: json.RawMessage(echoSchema),
	}
}

// Execute 记录参数并生成探测工具的测试结果。
func (t *probeTool) Execute(_ context.Context, call *defs.Item, _ chan<- *executor.ExecutionProgressEvent) *executor.ToolExecutionResult {
	// 1. 保存收到的参数，有预设错误码时返回错误，否则返回参数正文。
	t.probe.record(call.ToolCall.Arguments)
	if t.failureCode != "" {
		return &executor.ToolExecutionResult{Result: &defs.ToolResult{
			CallID:  call.ToolCall.CallID,
			Content: json.RawMessage("null"),
			Error: &defs.ToolError{
				Code:    t.failureCode,
				Message: "failed",
			},
		}}
	}
	content, _ := json.Marshal(map[string]string{"arguments": call.ToolCall.Arguments})
	return &executor.ToolExecutionResult{Result: &defs.ToolResult{
		CallID:  call.ToolCall.CallID,
		Content: content,
	}}
}

// stubHooks 按事件名返回预设的 HookOutcome、记录收到的请求；每个请求先后往 runUpdates 发 running 和 completed 两条运行记录。
type stubHooks struct {
	outcomes map[defs.HookEventName]*hook.HookOutcome
	// gatedCallIDs 里的调用，PreToolUse 要等 release 关闭才返回。
	gatedCallIDs []string
	release      chan struct{}

	mu       sync.Mutex
	requests []*hook.HookEventRequest
}

// RunEvent 记录 hook 请求，发送运行状态并返回预设结果。
func (s *stubHooks) RunEvent(_ context.Context, request *hook.HookEventRequest, runUpdates chan<- *defs.HookRun) *hook.HookOutcome {
	// 1. 复制请求及调用、结果，追加到请求记录。
	recorded := *request
	event := *request.Event
	event.ToolCall = request.Event.ToolCall.Clone()
	event.ToolResult = request.Event.ToolResult.Clone()
	recorded.Event = &event
	s.mu.Lock()
	s.requests = append(s.requests, &recorded)
	s.mu.Unlock()

	// 2. 发送 running，按调用 ID 等待放行后发送 completed，再返回预设结果。
	callID := request.Event.ToolCall.CallID
	run := &defs.HookRun{
		ID:         defs.HookRunID(string(request.Event.Name) + "-" + callID),
		Event:      request.Event.Name,
		CallID:     callID,
		ExecutorID: request.ExecutorID,
		Status:     defs.HookRunRunning,
	}
	runUpdates <- run.Clone()
	if request.Event.Name == defs.HookEventPreToolUse && slices.Contains(s.gatedCallIDs, callID) {
		<-s.release
	}
	run.Status = defs.HookRunCompleted
	runUpdates <- run.Clone()
	if outcome := s.outcomes[request.Event.Name]; outcome != nil {
		return outcome
	}
	return &hook.HookOutcome{}
}

// GetHookInfos 返回空的 hook 信息列表。
func (s *stubHooks) GetHookInfos() []*hook.HookInfo {
	// 1. 返回 nil。
	return nil
}

// getRequests 返回收到的 hook 请求列表副本。
func (s *stubHooks) getRequests() []*hook.HookEventRequest {
	// 1. 加锁复制请求列表。
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// slowEnvironment 是一个执行环境，GetMCPTools 先等 delay 再返回 tools。
type slowEnvironment struct {
	executor.ExecutionEnvironment
	delay time.Duration
	tools []executor.Tool
}

// GetMCPTools 延迟返回测试 MCP 工具。
func (e *slowEnvironment) GetMCPTools(ctx context.Context, _ defs.OptionalStringList) ([]executor.Tool, error) {
	// 1. 等待 delay 到期后返回工具列表，ctx 取消时返回错误。
	select {
	case <-time.After(e.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return e.tools, nil
}

// newSlowEnvironment 创建延迟提供 MCP 工具的测试环境。
func newSlowEnvironment(t *testing.T, id string, delay time.Duration, toolNames ...string) *slowEnvironment {
	// 1. 创建空环境及按名称回显环境的工具，保存读取延时。
	t.Helper()
	environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(id, id+" 环境", nil))
	if err != nil {
		t.Fatal(err)
	}
	var tools []executor.Tool
	for _, name := range toolNames {
		tools = append(tools, &echoTool{
			executorID: id,
			name:       name,
			schema:     echoSchema,
		})
	}
	return &slowEnvironment{
		ExecutionEnvironment: environment,
		delay:                delay,
		tools:                tools,
	}
}

// newEchoEnvironment 创建带虚拟工作区和回显工具的测试环境。
func newEchoEnvironment(t *testing.T, id string, toolNames ...string) executor.ExecutionEnvironment {
	// 1. 为每个名称创建回显工具，并创建 /workspace/<id> 工作区和环境。
	t.Helper()
	var tools []executor.Tool
	for _, name := range toolNames {
		tools = append(tools, &echoTool{
			executorID: id,
			name:       name,
			schema:     echoSchema,
		})
	}
	work, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/workspace/"+id, nil))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(id, id+" 环境", work, tools...))
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

// newEchoToolExecutorOpt 组合 local、sandbox 环境和协作工具 list_agents。
func newEchoToolExecutorOpt(t *testing.T) *executor.ToolExecutorOpt {
	// 1. 创建各环境的回显工具，保存为执行器配置。
	t.Helper()
	return executor.NewToolExecutorOpt(
		[]executor.ExecutionEnvironment{
			newEchoEnvironment(t, "local", "bash", "read_file", "apply_patch"),
			newEchoEnvironment(t, "sandbox", "bash", "read_file"),
		},
		&echoTool{
			executorID: "agent",
			name:       "list_agents",
			schema:     echoSchema,
		},
	)
}

// newEchoToolExecutor 创建按名称筛选内置工具的测试执行器。
func newEchoToolExecutor(t *testing.T, toolNames []string) executor.ToolExecutor {
	// 1. 创建双环境配置，设置工具名单并构造执行器。
	t.Helper()
	opt := newEchoToolExecutorOpt(t)
	opt.ToolNames = toolNames
	toolExecutor, err := executor.NewToolExecutor(opt)
	if err != nil {
		t.Fatal(err)
	}
	return toolExecutor
}

// newHookToolExecutorOpt 创建带参数探测工具的环境和协作工具配置。
func newHookToolExecutorOpt(t *testing.T, probe *toolProbe, failureCode string, withEnvironments bool) *executor.ToolExecutorOpt {
	// 1. 按 withEnvironments 创建 first、second 环境，再加入 agent_probe 并设置 Session。
	t.Helper()
	var environments []executor.ExecutionEnvironment
	if withEnvironments {
		for _, id := range []string{"first", "second"} {
			work, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/workspace/"+id, nil))
			if err != nil {
				t.Fatal(err)
			}
			environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt(id, id+" 环境", work,
				&probeTool{
					name:        "probe",
					failureCode: failureCode,
					probe:       probe,
				}))
			if err != nil {
				t.Fatal(err)
			}
			environments = append(environments, environment)
		}
	}
	opt := executor.NewToolExecutorOpt(environments, &probeTool{
		name:        "agent_probe",
		failureCode: failureCode,
		probe:       probe,
	})
	opt.Session = defs.Session{ID: "session-1"}
	return opt
}

// executorCallItems 把工具调用包成 Item 列表。
func executorCallItems(calls []*defs.ToolCall) []*defs.Item {
	// 1. 为每次调用生成 Item ID 并设置工具调用类型。
	var items []*defs.Item
	for _, call := range calls {
		items = append(items, &defs.Item{
			ID:       defs.NewItemID(),
			Kind:     defs.ItemToolCall,
			ToolCall: call,
		})
	}
	return items
}

// newHookToolCallItem 创建 turn-1 中的工具调用 Item。
func newHookToolCallItem(call *defs.ToolCall) *defs.Item {
	// 1. 填入新 Item ID、Turn ID 和工具调用。
	return &defs.Item{
		ID:       defs.NewItemID(),
		TurnID:   "turn-1",
		Kind:     defs.ItemToolCall,
		ToolCall: call,
	}
}

// runToolCalls 执行一批调用，返回各调用的结果和完成次数。
func runToolCalls(t *testing.T, toolExecutor executor.ToolExecutor, calls []*defs.ToolCall) (map[string]*defs.ToolResult, map[string]int) {
	// 1. 使用测试上下文执行本批调用。
	t.Helper()
	return runToolCallsWithContext(t, t.Context(), toolExecutor, calls)
}

// runToolCallsWithContext 在给定上下文中执行一批调用，收集结果和完成次数。
func runToolCallsWithContext(t *testing.T, ctx context.Context, toolExecutor executor.ToolExecutor, calls []*defs.ToolCall) (map[string]*defs.ToolResult, map[string]int) {
	// 1. 设置五秒超时并后台执行工具调用。
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	events := make(chan executor.ExecutorEvent)
	var tasks sync.WaitGroup
	tasks.Go(func() { toolExecutor.Run(ctx, executorCallItems(calls), events) })
	// 2. 按调用 ID 收集结果和完成次数，等待本批执行结束。
	results := make(map[string]*defs.ToolResult)
	finishedCounts := make(map[string]int)
	for event := range events {
		if finished, ok := event.(*executor.ExecutionFinishedEvent); ok {
			results[finished.Result.CallID] = finished.Result
			finishedCounts[finished.Result.CallID]++
		}
	}
	tasks.Wait()
	return results, finishedCounts
}

// collectToolEvents 执行一批调用并按到达顺序返回事件。
func collectToolEvents(t *testing.T, toolExecutor executor.ToolExecutor, items []*defs.Item) []executor.ExecutorEvent {
	// 1. 以五秒超时运行调用，收集到通道关闭并等待执行结束。
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	events := make(chan executor.ExecutorEvent)
	var tasks sync.WaitGroup
	tasks.Go(func() { toolExecutor.Run(ctx, items, events) })
	var collected []executor.ExecutorEvent
	for event := range events {
		collected = append(collected, event)
	}
	tasks.Wait()
	return collected
}

// getExecutorEnum 返回工具 executor 参数的枚举，使用逗号连接。
func getExecutorEnum(t *testing.T, definition *defs.ToolDefinition) string {
	// 1. 解码输入 schema 并连接枚举值，无 executor 时返回空字符串。
	t.Helper()
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	return strings.Join(schema.Properties["executor"].Enum, ",")
}

// hasExecutorParameter 工具输入 schema 是否声明 executor 参数？
func hasExecutorParameter(t *testing.T, definition *defs.ToolDefinition) bool {
	// 1. 解码属性列表，检查 executor 是否存在。
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	_, found := schema.Properties["executor"]
	return found
}

// TestToolExecutorRunResults 验证每次调用的唯一结果及进度顺序。
func TestToolExecutorRunResults(t *testing.T) {
	// 1. 准备成功、参数错误、未知工具、取消和空批次用例。
	tests := []*struct {
		name      string
		calls     []*defs.ToolCall
		cancel    bool
		wantCodes map[string]string
	}{
		{
			name: "each_call_gets_one_result",
			calls: []*defs.ToolCall{
				{
					CallID:    "read_a",
					Name:      "read_file",
					Arguments: `{"path":"note.txt"}`,
				},
				{
					CallID:    "read_b",
					Name:      "read_file",
					Arguments: `{"path":"note.txt"}`,
				},
			},
			wantCodes: map[string]string{"read_a": "", "read_b": ""},
		},
		{
			name: "unknown_tool_returns_tool_not_found",
			calls: []*defs.ToolCall{
				{
					CallID:    "read_a",
					Name:      "read_file",
					Arguments: `{"path":"note.txt"}`,
				},
				{
					CallID:    "unknown",
					Name:      "unknown_tool",
					Arguments: `{}`,
				},
			},
			wantCodes: map[string]string{"read_a": "", "unknown": "tool_not_found"},
		},
		{
			name: "invalid_arguments_returned_by_tool",
			calls: []*defs.ToolCall{
				{
					CallID:    "read_a",
					Name:      "read_file",
					Arguments: `{"path":"note.txt"}`,
				},
				{
					CallID:    "invalid",
					Name:      "read_file",
					Arguments: `{`,
				},
			},
			wantCodes: map[string]string{"read_a": "", "invalid": "invalid_arguments"},
		},
		{
			name: "tool_business_error",
			calls: []*defs.ToolCall{
				{
					CallID:    "read_a",
					Name:      "read_file",
					Arguments: `{"path":"missing.txt"}`,
				},
			},
			wantCodes: map[string]string{"read_a": "read_failed"},
		},
		{
			name: "cancel_still_delivers_each_result",
			calls: []*defs.ToolCall{
				{
					CallID:    "read_a",
					Name:      "read_file",
					Arguments: `{"path":"note.txt"}`,
				},
				{
					CallID:    "read_b",
					Name:      "read_file",
					Arguments: `{"path":"note.txt"}`,
				},
			},
			cancel:    true,
			wantCodes: map[string]string{"read_a": "canceled", "read_b": "canceled"},
		},
		{
			name:      "empty_batch_closes_events",
			wantCodes: map[string]string{},
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("executor reads files"), 0600); err != nil {
				t.Fatal(err)
			}
			tool := &fileTool{
				definition: &defs.ToolDefinition{
					Name:        "read_file",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				root:    root,
				release: make(chan struct{}),
			}
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, tool))
			if err != nil {
				t.Fatal(err)
			}
			executing := 0
			for _, code := range tt.wantCodes {
				if code != "tool_not_found" && code != "invalid_arguments" {
					executing++
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			events := make(chan executor.ExecutorEvent)
			var tasks sync.WaitGroup
			tasks.Go(func() { toolExecutor.Run(ctx, executorCallItems(tt.calls), events) })
			progress := make(map[string][]string)
			// 按调用 ID 收集进度和结果，同一调用收到第二个结果时直接失败。
			results := make(map[string]*defs.ToolResult)
			started := 0
			for event := range events {
				switch event := event.(type) {
				case *executor.ExecutionProgressEvent:
					if results[event.CallID] != nil {
						t.Fatalf("progress for %q arrived after its result", event.CallID)
					}
					progress[event.CallID] = append(progress[event.CallID], string(event.Content))
					if string(event.Content) != `"reading"` {
						continue
					}
					// 全部调用发出 reading 后统一放行或取消。
					if started++; started == executing {
						if tt.cancel {
							cancel()
						} else {
							close(tool.release)
						}
					}
				case *executor.ExecutionFinishedEvent:
					if results[event.Result.CallID] != nil {
						t.Fatalf("duplicate result for %q", event.Result.CallID)
					}
					results[event.Result.CallID] = event.Result
				}
			}
			tasks.Wait()
			if len(results) != len(tt.wantCodes) {
				t.Fatalf("results = %d, want %d", len(results), len(tt.wantCodes))
			}
			for callID, wantCode := range tt.wantCodes {
				result := results[callID]
				if result == nil {
					t.Fatalf("missing result for %q", callID)
				}
				gotCode := ""
				if result.Error != nil {
					gotCode = result.Error.Code
				}
				if gotCode != wantCode {
					t.Fatalf("%s error code = %q, want %q", callID, gotCode, wantCode)
				}
				if wantCode == "" && string(result.Content) != `"executor reads files"` {
					t.Fatalf("%s content = %s", callID, result.Content)
				}
				wantProgress := []string(nil)
				if wantCode != "tool_not_found" && wantCode != "invalid_arguments" {
					wantProgress = []string{`"reading"`, `"done"`}
				}
				if !slices.Equal(progress[callID], wantProgress) {
					t.Fatalf("%s progress = %v, want %v", callID, progress[callID], wantProgress)
				}
			}
		})
	}
}

// TestToolExecutorRunMCPTools 验证 MCP 工具在提供它的连接上执行。
func TestToolExecutorRunMCPTools(t *testing.T) {
	// 1. 准备已选中、未选中和不存在的 MCP 工具调用。
	binary := buildFakeMCPServer(t)
	tests := []struct {
		name     string
		toolName string
		// mcpServerIDs 是 ToolExecutor 可用的 MCP server；nil 表示全部。
		mcpServerIDs defs.OptionalStringList
		wantCode     string
		// wantContent 是成功时的结果正文。
		wantContent string
	}{
		{
			name:        "mcp_tool_runs_on_its_connection",
			toolName:    "mcp__ok__echo",
			wantContent: `"hello"`,
		},
		{
			name:     "unknown_mcp_tool_returns_tool_not_found",
			toolName: "mcp__ok__nope",
			wantCode: "tool_not_found",
		},
		{
			name:     "mcp_tool_of_another_server_returns_tool_not_found",
			toolName: "mcp__other__echo",
			wantCode: "tool_not_found",
		},
		{
			name:         "mcp_tool_of_unselected_server_returns_tool_not_found",
			toolName:     "mcp__ok__echo",
			mcpServerIDs: defs.OptionalStringList{},
			wantCode:     "tool_not_found",
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newEnvironmentFixture(t, "local", newLocalWorkspace(t),
				[]*mcpclient.ServerDefinition{newFakeServerDefinition("ok", binary, []string{"local"})}, "bash")
			toolExecutor := fixture.newToolExecutor(t, tt.mcpServerIDs)
			if _, err := toolExecutor.GetTools(t.Context()); err != nil {
				t.Fatal(err)
			}
			results, finishedCounts := runToolCalls(t, toolExecutor, []*defs.ToolCall{{
				CallID:    "call-1",
				Name:      tt.toolName,
				Arguments: `{"text":"hello"}`,
			}})
			result := results["call-1"]
			if finishedCounts["call-1"] != 1 || result == nil {
				t.Fatalf("results = %v, finished counts = %v, want exactly one result for call-1", results, finishedCounts)
			}
			gotCode := ""
			if result.Error != nil {
				gotCode = result.Error.Code
			}
			if gotCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q", gotCode, tt.wantCode)
			}
			if tt.wantCode == "" && string(result.Content) != tt.wantContent {
				t.Fatalf("content = %s, want %s", result.Content, tt.wantContent)
			}
		})
	}
}

// TestToolExecutorRunTelemetry 验证工具 span 带上工具说明，开启内容记录时才带参数和结果。
func TestToolExecutorRunTelemetry(t *testing.T) {
	// 1. 准备开启和关闭内容记录的用例。
	tests := []struct {
		name            string
		toolName        string
		captureContent  bool
		wantDescription string
		wantArguments   string
		wantResult      string
	}{
		{
			name:            "registered_tool_records_content",
			toolName:        "read_file",
			captureContent:  true,
			wantDescription: "read_file 工具",
			wantArguments:   `{"path":"a.txt"}`,
			wantResult:      `"executor":"local"`,
		},
		{
			name:            "content_disabled_records_no_content",
			toolName:        "read_file",
			wantDescription: "read_file 工具",
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipelineWithoutContent(t)
			if tt.captureContent {
				p = telemetrytest.NewPipeline(t)
			}
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, &echoTool{
				executorID: "local",
				name:       "read_file",
				schema:     echoSchema,
			}))
			if err != nil {
				t.Fatal(err)
			}
			runToolCallsWithContext(t, telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), toolExecutor,
				[]*defs.ToolCall{{
					CallID:    "call-1",
					Name:      tt.toolName,
					Arguments: `{"path":"a.txt"}`,
				}})

			span := p.WaitEndedSpans(t, "execute_tool "+tt.toolName, 1)[0]
			if got := telemetrytest.GetSpanAttribute(span, "error.type"); got != "" {
				t.Errorf("error.type = %q, want none for a successful call", got)
			}
			if span.Status().Code == codes.Error {
				t.Errorf("span status = %v, want no error for a successful call", span.Status())
			}
			if got := telemetrytest.GetSpanAttribute(span, "gen_ai.tool.description"); got != tt.wantDescription {
				t.Errorf("gen_ai.tool.description = %q, want %q", got, tt.wantDescription)
			}
			if got := telemetrytest.GetSpanAttribute(span, "gen_ai.tool.call.arguments"); got != tt.wantArguments {
				t.Errorf("gen_ai.tool.call.arguments = %q, want %q", got, tt.wantArguments)
			}
			if value, found := telemetrytest.GetSpanValue(span, "gen_ai.tool.call.arguments"); found && value.Type() != attribute.STRING {
				t.Errorf("gen_ai.tool.call.arguments type = %v, want JSON text string", value.Type())
			}
			result := telemetrytest.GetSpanAttribute(span, "gen_ai.tool.call.result")
			if (tt.wantResult == "" && result != "") || !strings.Contains(result, tt.wantResult) {
				t.Errorf("gen_ai.tool.call.result = %q, want it to contain %q", result, tt.wantResult)
			}
		})
	}
}

// TestToolExecutorToolDefinitions 验证工具定义去重和返回副本。
func TestToolExecutorToolDefinitions(t *testing.T) {
	// 1. 准备不同名称和重复名称的工具。
	tests := []*struct {
		name      string
		toolName  string
		duplicate bool
	}{
		{
			name:     "distinct_tool_names",
			toolName: "write_file",
		},
		{
			name:      "duplicate_tool_name",
			toolName:  "read_file",
			duplicate: true,
		},
	}
	// 2. 逐项创建工具执行器，核对工具定义和返回副本。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &fileTool{definition: &defs.ToolDefinition{
				Name:        "read_file",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}}
			second := &fileTool{definition: &defs.ToolDefinition{
				Name:        tt.toolName,
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}}
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, first, second))
			if tt.duplicate {
				if err == nil || !strings.Contains(err.Error(), "duplicate") {
					t.Fatalf("duplicate error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			definitions, err := toolExecutor.GetTools(t.Context())
			if err != nil || len(definitions) != 2 || definitions[0].Name != "read_file" || definitions[1].Name != tt.toolName {
				t.Fatalf("definitions = %+v, error = %v", definitions, err)
			}
			definitions[0].InputSchema[0] = 'x'
			definitions, err = toolExecutor.GetTools(t.Context())
			if err != nil || !json.Valid(definitions[0].InputSchema) {
				t.Fatalf("definition snapshot changed: %v", err)
			}
		})
	}
}

// TestToolExecutorCallItemIdentity 验证交给工具的 Item 是保留身份的副本。
func TestToolExecutorCallItemIdentity(t *testing.T) {
	// 1. 分别执行单个和多个调用，核对 Item 身份及源参数未被修改。
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			received := make(chan *defs.Item, count)
			tool := &identityTool{received: received}
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(nil, tool))
			if err != nil {
				t.Fatal(err)
			}
			var calls []*defs.Item
			for i := range count {
				calls = append(calls, &defs.Item{
					ID:          defs.NewItemID(),
					TurnID:      defs.NewTurnID(),
					ModelCallID: defs.NewModelCallID(),
					Kind:        defs.ItemToolCall,
					ToolCall: &defs.ToolCall{
						CallID:    fmt.Sprint(i),
						Name:      "file",
						Arguments: "{}",
					},
				})
			}
			events := make(chan executor.ExecutorEvent, 10)
			toolExecutor.Run(t.Context(), calls, events)
			for range count {
				got := <-received
				found := false
				for _, call := range calls {
					if got.ID == call.ID {
						found = true
						if got.TurnID != call.TurnID || got.ModelCallID != call.ModelCallID || call.ToolCall.Arguments != "{}" {
							t.Fatal("identity or source changed")
						}
					}
				}
				if !found {
					t.Fatal("unknown item")
				}
			}
		})
	}
}

// TestToolExecutorRouteCalls 验证 executor 选择、参数删除和分发错误。
func TestToolExecutorRouteCalls(t *testing.T) {
	// 1. 逐项调用默认环境、指定环境、协作工具和各类分发错误，核对提供方、参数和错误。
	for _, tt := range []struct {
		name          string
		toolNames     []string
		call          *defs.ToolCall
		wantCode      string
		wantMessage   string
		wantExecutor  string
		wantArguments string
		// wantResultExecutor 是 Result.Executor：实际执行的环境；协作工具和没能分派的调用为空。
		wantResultExecutor string
	}{
		{
			name: "omitted_uses_default",
			call: &defs.ToolCall{
				Name:      "read_file",
				Arguments: `{"path":"a"}`,
			},
			wantExecutor:       "local",
			wantArguments:      `{"path":"a"}`,
			wantResultExecutor: "local",
		},
		{
			name: "explicit_sandbox",
			call: &defs.ToolCall{
				Name:      "read_file",
				Arguments: `{"path":"a","executor":"sandbox"}`,
			},
			wantExecutor:       "sandbox",
			wantArguments:      `{"path":"a"}`,
			wantResultExecutor: "sandbox",
		},
		{
			name: "single_provider_tool",
			call: &defs.ToolCall{
				Name:      "apply_patch",
				Arguments: `{}`,
			},
			wantExecutor:       "local",
			wantArguments:      `{}`,
			wantResultExecutor: "local",
		},
		{
			name: "single_provider_named",
			call: &defs.ToolCall{
				Name:      "apply_patch",
				Arguments: `{"executor":"local"}`,
			},
			wantExecutor:       "local",
			wantArguments:      `{}`,
			wantResultExecutor: "local",
		},
		{
			name: "executor_without_tool",
			call: &defs.ToolCall{
				Name:      "apply_patch",
				Arguments: `{"executor":"sandbox"}`,
			},
			wantCode:    "invalid_arguments",
			wantMessage: "does not provide",
		},
		{
			name: "unknown_executor",
			call: &defs.ToolCall{
				Name:      "read_file",
				Arguments: `{"executor":"missing"}`,
			},
			wantCode:    "invalid_arguments",
			wantMessage: "not configured",
		},
		{
			name: "non_string_executor",
			call: &defs.ToolCall{
				Name:      "read_file",
				Arguments: `{"executor":1}`,
			},
			wantCode:    "invalid_arguments",
			wantMessage: "string",
		},
		{
			name:      "unselected_tool",
			toolNames: []string{"bash", "read_file"},
			call: &defs.ToolCall{
				Name:      "apply_patch",
				Arguments: `{}`,
			},
			wantCode: "tool_not_found",
		},
		{
			name: "unknown_tool",
			call: &defs.ToolCall{
				Name:      "missing",
				Arguments: `{}`,
			},
			wantCode: "tool_not_found",
		},
		{
			name: "agent_tool_untouched",
			call: &defs.ToolCall{
				Name:      "list_agents",
				Arguments: `{"executor":"sandbox"}`,
			},
			wantExecutor:  "agent",
			wantArguments: `{"executor":"sandbox"}`,
		},
		{
			name: "non_object_arguments_forwarded",
			call: &defs.ToolCall{
				Name:      "read_file",
				Arguments: `[1]`,
			},
			wantExecutor:       "local",
			wantArguments:      `[1]`,
			wantResultExecutor: "local",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.call.CallID = "call"
			results, _ := runToolCalls(t, newEchoToolExecutor(t, tt.toolNames), []*defs.ToolCall{tt.call})
			result := results["call"]
			if result.Executor != tt.wantResultExecutor {
				t.Fatalf("result executor = %q, want %q", result.Executor, tt.wantResultExecutor)
			}
			if tt.wantCode != "" {
				if result.Error == nil || result.Error.Code != tt.wantCode || !strings.Contains(result.Error.Message, tt.wantMessage) {
					t.Fatalf("error = %+v, want %s containing %q", result.Error, tt.wantCode, tt.wantMessage)
				}
				return
			}
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			var echoed map[string]string
			if err := json.Unmarshal(result.Content, &echoed); err != nil {
				t.Fatal(err)
			}
			if echoed["executor"] != tt.wantExecutor || echoed["arguments"] != tt.wantArguments {
				t.Fatalf("echoed = %v, want executor %s arguments %s", echoed, tt.wantExecutor, tt.wantArguments)
			}
		})
	}
}

// TestToolExecutorConcurrentCalls 验证混合工具批次中的调用均返回一次结果。
func TestToolExecutorConcurrentCalls(t *testing.T) {
	// 1. 创建不同环境、协作工具和无效环境的调用。
	calls := []*defs.ToolCall{
		{
			CallID:    "local_a",
			Name:      "read_file",
			Arguments: `{"path":"a"}`,
		},
		{
			CallID:    "sandbox_b",
			Name:      "bash",
			Arguments: `{"executor":"sandbox"}`,
		},
		{
			CallID:    "agent_c",
			Name:      "list_agents",
			Arguments: `{}`,
		},
		{
			CallID:    "invalid_d",
			Name:      "read_file",
			Arguments: `{"executor":"missing"}`,
		},
		{
			CallID:    "local_e",
			Name:      "apply_patch",
			Arguments: `{}`,
		},
	}
	// 2. 执行混合批次并核对每个调用恰好一次完成。
	results, finishedCounts := runToolCalls(t, newEchoToolExecutor(t, nil), calls)
	for _, call := range calls {
		if finishedCounts[call.CallID] != 1 || results[call.CallID] == nil {
			t.Fatalf("call %s finished %d times", call.CallID, finishedCounts[call.CallID])
		}
	}
}

// TestToolExecutorDefinitionsExecutorParameter 验证多环境工具的 executor 枚举及默认环境说明。
func TestToolExecutorDefinitionsExecutorParameter(t *testing.T) {
	// 1. 逐项建立单环境和多环境工具表，核对 executor 参数。
	for _, tt := range []struct {
		name         string
		environments func(t *testing.T) []executor.ExecutionEnvironment
		// wantEnums 是工具名 → executor 参数 enum（逗号连接）；空串表示这个工具没有 executor 参数。
		wantEnums   map[string]string
		wantDefault map[string]string
	}{
		{
			name: "single_executor_unchanged",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{newEchoEnvironment(t, "local", "bash", "read_file")}
			},
			wantEnums: map[string]string{"bash": "", "read_file": "", "list_agents": ""},
		},
		{
			name: "shared_tools_get_parameter",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{
					newEchoEnvironment(t, "local", "bash", "read_file", "apply_patch"),
					newEchoEnvironment(t, "sandbox", "bash", "read_file"),
				}
			},
			wantEnums:   map[string]string{"bash": "local,sandbox", "read_file": "local,sandbox", "apply_patch": "", "list_agents": ""},
			wantDefault: map[string]string{"bash": "local", "read_file": "local"},
		},
		{
			name: "default_not_provider",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{
					newEchoEnvironment(t, "local", "read_file"),
					newEchoEnvironment(t, "sandbox", "bash"),
					newEchoEnvironment(t, "gpu", "bash"),
				}
			},
			wantEnums:   map[string]string{"read_file": "", "bash": "sandbox,gpu", "list_agents": ""},
			wantDefault: map[string]string{"bash": "sandbox"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(tt.environments(t),
				&echoTool{
					executorID: "agent",
					name:       "list_agents",
					schema:     echoSchema,
				}))
			if err != nil {
				t.Fatal(err)
			}
			definitions, err := toolExecutor.GetTools(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(definitions) != len(tt.wantEnums) {
				t.Fatalf("definitions = %d, want %d", len(definitions), len(tt.wantEnums))
			}
			// 环境工具在前，协作工具在后。
			if last := definitions[len(definitions)-1].Name; last != "list_agents" {
				t.Fatalf("last tool = %s, want agent tool list_agents", last)
			}
			for _, definition := range definitions {
				var schema struct {
					Properties map[string]struct {
						Enum        []string `json:"enum"`
						Description string   `json:"description"`
					} `json:"properties"`
				}
				if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
					t.Fatal(err)
				}
				parameter, found := schema.Properties["executor"]
				wantEnum, known := tt.wantEnums[definition.Name]
				if !known || found != (wantEnum != "") || strings.Join(parameter.Enum, ",") != wantEnum {
					t.Fatalf("tool %s executor parameter = %+v (found %v), want enum %q", definition.Name, parameter, found, wantEnum)
				}
				if wantDefault := tt.wantDefault[definition.Name]; wantDefault != "" &&
					parameter.Description != "执行此调用的 Executor，见系统提示 <openvaala-executors>；省略时使用 "+wantDefault {
					t.Fatalf("tool %s description = %q, want default %s", definition.Name, parameter.Description, wantDefault)
				}
			}
		})
	}
}

// TestToolExecutorInvalid 验证缺失环境、重复 ID 和协作工具重名错误。
func TestToolExecutorInvalid(t *testing.T) {
	// 1. 逐项构造无效环境配置，核对构造或读取工具时的错误。
	for _, tt := range []struct {
		name         string
		environments func(t *testing.T) []executor.ExecutionEnvironment
		wantBuildErr string
		wantToolsErr string
	}{
		{
			name: "agent_tool_conflict",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{newEchoEnvironment(t, "local", "list_agents")}
			},
			wantToolsErr: "conflicts with an agent tool",
		},
		{
			name: "duplicate_executor_id",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{newEchoEnvironment(t, "local", "bash"), newEchoEnvironment(t, "local", "read_file")}
			},
			wantBuildErr: "more than once",
		},
		{
			name: "missing_executor",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{nil}
			},
			wantBuildErr: "execution environment is missing",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(tt.environments(t),
				&echoTool{
					executorID: "agent",
					name:       "list_agents",
					schema:     echoSchema,
				}))
			if tt.wantBuildErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantBuildErr) {
					t.Fatalf("NewToolExecutor error = %v, want %q", err, tt.wantBuildErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := toolExecutor.GetTools(t.Context()); err == nil || !strings.Contains(err.Error(), tt.wantToolsErr) {
				t.Fatalf("GetTools error = %v, want %q", err, tt.wantToolsErr)
			}
		})
	}
}

// TestToolExecutorTelemetry 验证工具的环境身份、父 span、日志和耗时指标。
func TestToolExecutorTelemetry(t *testing.T) {
	// 1. 创建双环境及客户端 MCP 工具，在父 span 下执行混合调用。
	p := telemetrytest.NewPipeline(t)
	client := newClientMCPServersFixture(t)
	client.register(t, "c1", clientServer{id: "remote"})
	opt := newEchoToolExecutorOpt(t)
	opt.ClientMCPServers = client.client
	toolExecutor, err := executor.NewToolExecutor(opt)
	if err != nil {
		t.Fatal(err)
	}
	turnCtx, turnSpan := p.TracerProvider.Tracer("test").Start(telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), "invoke_agent assistant")
	results, _ := runToolCallsWithContext(t, turnCtx, toolExecutor, []*defs.ToolCall{
		{
			CallID:    "call-sandbox",
			Name:      "read_file",
			Arguments: `{"path":"a","executor":"sandbox"}`,
		},
		{
			CallID:    "call-local",
			Name:      "bash",
			Arguments: `{}`,
		},
		{
			CallID:    "call-agent",
			Name:      "list_agents",
			Arguments: `{}`,
		},
		{
			CallID:    "call-missing",
			Name:      "read_file",
			Arguments: `{"executor":"missing"}`,
		},
		{
			CallID:    "call-client",
			Name:      "mcp__remote__echo",
			Arguments: `{"text":"hello"}`,
		},
	})
	turnSpan.End()
	if results["call-missing"].Error == nil || results["call-missing"].Error.Code != "invalid_arguments" {
		t.Fatalf("call-missing result = %+v", results["call-missing"])
	}
	if result := results["call-client"]; result.Error != nil || result.Executor != "" {
		t.Fatalf("call-client result = %+v, want success without a result executor", result)
	}

	tests := []struct {
		name       string
		spanName   string
		callID     string
		executorID string
	}{
		{
			name:       "routed_to_sandbox",
			spanName:   "execute_tool read_file",
			callID:     "call-sandbox",
			executorID: "sandbox",
		},
		{
			name:       "default_local",
			spanName:   "execute_tool bash",
			callID:     "call-local",
			executorID: "local",
		},
		{
			name:       "agent_tool_without_executor",
			spanName:   "execute_tool list_agents",
			callID:     "call-agent",
			executorID: "",
		},
		{
			name:       "client_mcp_tool_on_client",
			spanName:   "execute_tool mcp__remote__echo",
			callID:     "call-client",
			executorID: defs.ClientExecutorID,
		},
	}
	// 2. 逐项读取遥测，核对环境身份、父 span、日志和指标。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spans := p.WaitEndedSpans(t, tt.spanName, 1)
			if len(spans) != 1 {
				t.Fatalf("spans named %q = %d, want 1", tt.spanName, len(spans))
			}
			span := spans[0]
			if got := telemetrytest.GetSpanAttribute(span, "gen_ai.tool.call.id"); got != tt.callID {
				t.Errorf("gen_ai.tool.call.id = %q, want %q", got, tt.callID)
			}
			if got := telemetrytest.GetSpanAttribute(span, "openvaala.executor.id"); got != tt.executorID {
				t.Errorf("openvaala.executor.id = %q, want %q", got, tt.executorID)
			}
			if got, want := span.Parent().SpanID(), turnSpan.SpanContext().SpanID(); got != want {
				t.Errorf("parent span id = %s, want %s", got, want)
			}
		})
	}
	t.Run("undispatched_call_logged_without_span", func(t *testing.T) {
		records := p.GetLogRecords(t, "tool call not dispatched")
		if len(records) != 1 {
			t.Fatalf("records = %v, want 1", records)
		}
		for key, want := range map[string]string{"gen_ai.tool.name": "read_file", "gen_ai.tool.call.id": "call-missing", "error.type": "invalid_arguments", "level": "WARN"} {
			if got := records[0][key]; got != want {
				t.Errorf("record %q = %v, want %q", key, got, want)
			}
		}
	})
	t.Run("tool_duration_by_executor", func(t *testing.T) {
		if got := p.GetHistogramCount(t, "gen_ai.execute_tool.duration", attribute.String("openvaala.executor.id", "sandbox")); got != 1 {
			t.Fatalf("sandbox tool records = %d, want 1", got)
		}
	})
	t.Run("client_mcp_tool_metric_and_log_on_client", func(t *testing.T) {
		if got := p.GetHistogramCount(t, "gen_ai.execute_tool.duration", attribute.String("openvaala.executor.id", defs.ClientExecutorID)); got != 1 {
			t.Errorf("client tool duration records = %d, want 1", got)
		}
		var clientRecords []map[string]any
		for _, record := range p.GetLogRecords(t, "tool call finished") {
			if record["gen_ai.tool.call.id"] == "call-client" {
				clientRecords = append(clientRecords, record)
			}
		}
		if len(clientRecords) != 1 || clientRecords[0]["openvaala.executor.id"] != defs.ClientExecutorID {
			t.Errorf("tool call finished records for call-client = %v, want one with executor %s", clientRecords, defs.ClientExecutorID)
		}
	})
}

// TestToolExecutorToolFailureTelemetry 验证工具失败和取消在遥测中的记录。
func TestToolExecutorToolFailureTelemetry(t *testing.T) {
	// 1. 准备普通失败与取消错误码。
	tests := []struct {
		name      string
		errorCode string
		wantError bool
	}{
		{
			name:      "tool_failure_marks_span_error",
			errorCode: "tool_exit_nonzero",
			wantError: true,
		},
		{
			name:      "canceled_tool_is_not_span_error",
			errorCode: "canceled",
			wantError: false,
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := telemetrytest.NewPipeline(t)
			environment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt("local", "", nil,
				&echoTool{
					executorID: "local",
					name:       "bash",
					schema:     echoSchema,
					errorCode:  tt.errorCode,
				}))
			if err != nil {
				t.Fatal(err)
			}
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{environment}))
			if err != nil {
				t.Fatal(err)
			}
			runToolCallsWithContext(t, telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), toolExecutor, []*defs.ToolCall{{
				CallID:    "call-1",
				Name:      "bash",
				Arguments: `{}`,
			}})

			spans := p.WaitEndedSpans(t, "execute_tool bash", 1)
			if got := spans[0].Status().Code == codes.Error; got != tt.wantError {
				t.Errorf("span status = %v, want error = %v", spans[0].Status(), tt.wantError)
			}
			if got := telemetrytest.GetSpanAttribute(spans[0], "error.type"); got != tt.errorCode {
				t.Errorf("error.type = %q, want %q", got, tt.errorCode)
			}
			if got := p.GetHistogramCount(t, "gen_ai.execute_tool.duration", attribute.String("error.type", tt.errorCode)); got != 1 {
				t.Errorf("tool duration records with error.type %q = %d, want 1", tt.errorCode, got)
			}
		})
	}
}

// TestToolExecutorRoutesUnavailable 验证强依赖失败时每个调用均返回环境不可用。
func TestToolExecutorRoutesUnavailable(t *testing.T) {
	// 1. 创建无法启动的强依赖 server 和两次工具调用。
	p := telemetrytest.NewPipeline(t)
	strong := newFakeServerDefinition("strong", "/nonexistent/openvaala-mcp-strong", []string{"local"})
	strong.Required = true
	toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{
		newEnvironmentFixture(t, "local", newLocalWorkspace(t), []*mcpclient.ServerDefinition{strong}, "bash", "read_file").environment,
	}))
	if err != nil {
		t.Fatal(err)
	}
	results, finishedCounts := runToolCallsWithContext(t, telemetry.ContextWithTelemetry(t.Context(), p.Telemetry), toolExecutor, []*defs.ToolCall{
		{
			CallID:    "call-1",
			Name:      "bash",
			Arguments: `{}`,
		},
		{
			CallID:    "call-2",
			Name:      "read_file",
			Arguments: `{}`,
		},
	})

	tests := []struct {
		callID   string
		toolName string
	}{
		{
			callID:   "call-1",
			toolName: "bash",
		},
		{
			callID:   "call-2",
			toolName: "read_file",
		},
	}
	records := p.GetLogRecords(t, "tool call not dispatched")
	if len(records) != len(tests) {
		t.Fatalf("records = %v, want %d", records, len(tests))
	}
	for index, tt := range tests {
		t.Run(tt.callID, func(t *testing.T) {
			if finishedCounts[tt.callID] != 1 || results[tt.callID].Error == nil || results[tt.callID].Error.Code != "executor_unavailable" {
				t.Errorf("result = %+v, finished = %d", results[tt.callID], finishedCounts[tt.callID])
			}
			wantFields := map[string]string{
				"level": "WARN", "gen_ai.tool.call.id": tt.callID, "gen_ai.tool.name": tt.toolName,
				"error.type": "executor_unavailable",
			}
			record := records[index]
			for key, want := range wantFields {
				if got := record[key]; got != want {
					t.Errorf("record %q = %v, want %q", key, got, want)
				}
			}
			// 核对强依赖 MCP 连接失败的环境和 server 错误前缀。
			const wantErrorPrefix = `get tools of executor "local": mcp server "strong" is unavailable`
			if got := fmt.Sprint(record["error"]); !strings.HasPrefix(got, wantErrorPrefix) {
				t.Errorf("record error = %q, want prefix %q", got, wantErrorPrefix)
			}
		})
	}
	for _, span := range p.GetEndedSpans() {
		if strings.HasPrefix(span.Name(), "execute_tool") {
			t.Errorf("unexpected span %q for undispatched calls", span.Name())
		}
	}
}

// TestToolExecutorMCPTools 验证 MCP 工具筛选、多环境枚举及强依赖错误。
func TestToolExecutorMCPTools(t *testing.T) {
	// 1. 准备 server 筛选、工具名单、挂起连接和强依赖失败用例。
	binary := buildFakeMCPServer(t)
	bothEnvironments := []string{"a", "b"}
	const promptly = 10 * time.Second
	tests := []struct {
		name         string
		toolNames    []string
		mcpServerIDs defs.OptionalStringList
		// hangingServer a、b 是否另外带一个握手永远不完成、启动超时 20 秒的 server？碰了它的读取会等满 20 秒。
		hangingServer bool
		// requiredMissingServer a、b 是否另外带一个起不来的强依赖 server？
		requiredMissingServer bool
		wantBuiltinTools      []string
		// wantEchoEnum 是 mcp__fake__echo 的 executor 参数 enum；空表示不应有任何 MCP 工具。
		wantEchoEnum string
		wantErr      string
	}{
		{
			name:             "nil selection offers MCP tools of every server on both executors",
			wantBuiltinTools: []string{"bash", "read_file"},
			wantEchoEnum:     "a,b",
		},
		{
			name:                  "empty selection offers no MCP tools and does not wait for connections",
			mcpServerIDs:          defs.OptionalStringList{},
			hangingServer:         true,
			requiredMissingServer: true,
			wantBuiltinTools:      []string{"bash", "read_file"},
		},
		{
			name:                  "listed server is used without waiting for the others",
			mcpServerIDs:          defs.OptionalStringList{"fake"},
			hangingServer:         true,
			requiredMissingServer: true,
			wantBuiltinTools:      []string{"bash", "read_file"},
			wantEchoEnum:          "a,b",
		},
		{
			name:             "listed server that no executor owns offers no MCP tools",
			mcpServerIDs:     defs.OptionalStringList{"other"},
			wantBuiltinTools: []string{"bash", "read_file"},
		},
		{
			name:             "tool names filter built-in tools only",
			toolNames:        []string{"bash"},
			wantBuiltinTools: []string{"bash"},
			wantEchoEnum:     "a,b",
		},
		{
			name:                  "selected required server that cannot start fails the executor",
			requiredMissingServer: true,
			wantErr:               `get tools of executor "a": mcp server "strong" is unavailable`,
		},
	}
	// 2. 逐项读取工具表，核对筛选结果、环境枚举和依赖错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers := []*mcpclient.ServerDefinition{newFakeServerDefinition("fake", binary, bothEnvironments)}
			if tt.hangingServer {
				hang := newFakeServerDefinition("hang", binary, bothEnvironments, "-hang")
				hang.StartupTimeout = 20 * time.Second
				servers = append(servers, hang)
			}
			if tt.requiredMissingServer {
				strong := newFakeServerDefinition("strong", "/nonexistent/openvaala-mcp-strong", bothEnvironments)
				strong.Required = true
				servers = append(servers, strong)
			}
			opt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{
				newEnvironmentFixture(t, "a", newLocalWorkspace(t), servers, "bash", "read_file").environment,
				newEnvironmentFixture(t, "b", newLocalWorkspace(t), servers, "bash", "read_file").environment,
			})
			opt.ToolNames = tt.toolNames
			opt.MCPServerIDs = tt.mcpServerIDs
			toolExecutor, err := executor.NewToolExecutor(opt)
			if err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			definitions, err := toolExecutor.GetTools(t.Context())
			if elapsed := time.Since(start); elapsed > promptly {
				t.Fatalf("GetTools() took %v, want it to wait only for selected servers", elapsed)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GetTools() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var builtinTools []string
			echoEnum, hasMCPTools := "", false
			for _, definition := range definitions {
				if !strings.HasPrefix(definition.Name, "mcp__") {
					builtinTools = append(builtinTools, definition.Name)
					continue
				}
				hasMCPTools = true
				if definition.MCPServerID != "fake" {
					t.Errorf("tool %s MCPServerID = %q, want fake", definition.Name, definition.MCPServerID)
				}
				if definition.Name == "mcp__fake__echo" {
					echoEnum = getExecutorEnum(t, definition)
				}
			}
			if !slices.Equal(builtinTools, tt.wantBuiltinTools) {
				t.Fatalf("built-in tools = %v, want %v", builtinTools, tt.wantBuiltinTools)
			}
			if hasMCPTools != (tt.wantEchoEnum != "") || echoEnum != tt.wantEchoEnum {
				t.Fatalf("MCP tools present = %v, mcp__fake__echo executor enum = %q, want enum %q", hasMCPTools, echoEnum, tt.wantEchoEnum)
			}
		})
	}
}

// TestToolExecutorRunUsesTurnRoutes 验证 Run 使用本轮工具表，连接断开后不重新读取工具。
func TestToolExecutorRunUsesTurnRoutes(t *testing.T) {
	// 1. 准备直接运行及建表后断开的用例。
	binary := buildFakeMCPServer(t)
	bothEnvironments := []string{"a", "b"}
	const promptly = 3 * time.Second
	tests := []struct {
		name string
		// getToolsFirst Run 之前是否先 GetTools？不先调用时 Run 自己建一次工具表。
		getToolsFirst bool
		// killServer Run 之前是否结束 a 上的 server？a 的工作区只允许启动一次进程，重连会失败。
		killServer bool
		wantCode   string
	}{
		{name: "run without GetTools reads the routes itself"},
		{
			name:          "run after GetTools keeps the routes of a disconnected server",
			getToolsFirst: true,
			killServer:    true,
			wantCode:      "mcp_server_unavailable",
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			servers := []*mcpclient.ServerDefinition{newFakeServerDefinition("fake", binary, bothEnvironments)}
			a := newEnvironmentFixture(t, "a", &singleStartWorkspace{Workspace: newLocalWorkspace(t)}, servers, "bash", "read_file").environment
			b := newEnvironmentFixture(t, "b", newLocalWorkspace(t), servers, "bash", "read_file").environment
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{a, b}))
			if err != nil {
				t.Fatal(err)
			}
			if tt.getToolsFirst {
				if _, err := toolExecutor.GetTools(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if tt.killServer {
				pid := getServerPID(t, toolExecutor, "fake")
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for a.GetMCPServers()[0].State == mcpclient.ConnectionStateConnected {
					if time.Now().After(deadline) {
						t.Fatalf("a.GetMCPServers() = %+v, want the killed server to be reported as disconnected", a.GetMCPServers())
					}
					time.Sleep(10 * time.Millisecond)
				}
			}

			start := time.Now()
			results, _ := runToolCalls(t, toolExecutor, []*defs.ToolCall{{
				CallID:    "echo",
				Name:      "mcp__fake__echo",
				Arguments: `{"text":"hello"}`,
			}})
			if elapsed := time.Since(start); elapsed > promptly {
				t.Fatalf("Run() took %v, want it to return without waiting for a connection", elapsed)
			}
			result := results["echo"]
			if got := result.Error; (tt.wantCode == "") != (got == nil) || (got != nil && got.Code != tt.wantCode) {
				t.Fatalf("echo result = %+v, want error code %q", result, tt.wantCode)
			}
			if tt.killServer {
				if state := a.GetMCPServers()[0].State; state == mcpclient.ConnectionStateConnected {
					t.Fatalf("a server state = %s after Run, want it not to be reconnected by Run", state)
				}
			}
		})
	}
}

// TestToolExecutorReadsEnvironmentsInParallel 验证各环境并发读取工具，结果仍按配置顺序排列。
func TestToolExecutorReadsEnvironmentsInParallel(t *testing.T) {
	// 1. 准备不同读取延时的环境组合。
	tests := []struct {
		name         string
		environments func(t *testing.T) []executor.ExecutionEnvironment
		maxElapsed   time.Duration
		wantNames    []string
		// wantBashEnum 是 bash 的 executor 参数 enum；空表示不检查。
		wantBashEnum string
	}{
		{
			name: "two executors sleeping one second are read together",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{
					newSlowEnvironment(t, "a", time.Second, "a_tool"),
					newSlowEnvironment(t, "b", time.Second, "b_tool"),
				}
			},
			maxElapsed: 1800 * time.Millisecond,
			wantNames:  []string{"a_tool", "b_tool"},
		},
		{
			name: "output follows the configured order although the later executor answers first",
			environments: func(t *testing.T) []executor.ExecutionEnvironment {
				return []executor.ExecutionEnvironment{
					newSlowEnvironment(t, "a", 400*time.Millisecond, "bash", "a_tool"),
					newSlowEnvironment(t, "b", 0, "bash", "b_tool"),
				}
			},
			maxElapsed:   1800 * time.Millisecond,
			wantNames:    []string{"bash", "a_tool", "b_tool"},
			wantBashEnum: "a,b",
		},
	}
	// 2. 逐项读取工具表，核对耗时、工具顺序和环境枚举。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt(tt.environments(t)))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			definitions, err := toolExecutor.GetTools(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > tt.maxElapsed {
				t.Fatalf("GetTools() took %v, want less than %v", elapsed, tt.maxElapsed)
			}
			if got := getToolNames(definitions); !slices.Equal(got, tt.wantNames) {
				t.Fatalf("GetTools() = %v, want %v", got, tt.wantNames)
			}
			if tt.wantBashEnum != "" {
				if got := getExecutorEnum(t, definitions[slices.Index(tt.wantNames, "bash")]); got != tt.wantBashEnum {
					t.Fatalf("bash executor enum = %q, want %q", got, tt.wantBashEnum)
				}
			}
		})
	}
}

// TestToolExecutorSchemaMismatch 验证环境间同名工具 schema 不同时保留首个提供方。
func TestToolExecutorSchemaMismatch(t *testing.T) {
	// 1. 创建 schema 不同的双环境工具。
	p := telemetrytest.NewPipeline(t)
	ctx := telemetry.ContextWithTelemetry(t.Context(), p.Telemetry)
	otherBash := &echoTool{
		executorID: "b",
		name:       "bash",
		schema:     `{"type":"object","properties":{"command":{"type":"string"}}}`,
	}
	otherEnvironment, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt("b", "b 环境", nil,
		otherBash, &echoTool{
			executorID: "b",
			name:       "read_file",
			schema:     echoSchema,
		}))
	if err != nil {
		t.Fatal(err)
	}
	toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{
		newEchoEnvironment(t, "a", "bash", "read_file"),
		otherEnvironment,
	}))
	if err != nil {
		t.Fatal(err)
	}

	// 2. 读取工具表，核对保留的定义、提供方和 Warn 日志。
	definitions, err := toolExecutor.GetTools(ctx)
	if err != nil {
		t.Fatalf("GetTools() error = %v, want the first executor's definition to win", err)
	}
	wantEnums := map[string]string{"bash": "", "read_file": "a,b"}
	if len(definitions) != len(wantEnums) {
		t.Fatalf("GetTools() = %v, want %d tools", getToolNames(definitions), len(wantEnums))
	}
	for _, definition := range definitions {
		if got := getExecutorEnum(t, definition); got != wantEnums[definition.Name] {
			t.Errorf("tool %s executor enum = %q, want %q", definition.Name, got, wantEnums[definition.Name])
		}
		if definition.Name == "bash" && string(definition.InputSchema) != echoSchema {
			t.Errorf("bash schema = %s, want the first executor's %s", definition.InputSchema, echoSchema)
		}
	}
	records := p.GetLogRecords(t, "tool schema differs between executors")
	if len(records) != 1 {
		t.Fatalf("schema mismatch records = %v, want exactly one", records)
	}
	for key, want := range map[string]string{"level": "WARN", "gen_ai.tool.name": "bash", "kept_executor_id": "a", "skipped_executor_id": "b"} {
		if got := records[0][key]; got != want {
			t.Errorf("record %q = %v, want %q", key, got, want)
		}
	}

	tests := []struct {
		name         string
		arguments    string
		wantExecutor string
		wantMessage  string
	}{
		{
			name:         "default executor runs the kept definition",
			arguments:    `{"path":"x"}`,
			wantExecutor: "a",
		},
		{
			name:         "first executor runs the kept definition",
			arguments:    `{"path":"x","executor":"a"}`,
			wantExecutor: "a",
		},
		{
			name:        "second executor does not provide the tool",
			arguments:   `{"executor":"b"}`,
			wantMessage: `executor "b" does not provide tool "bash"`,
		},
	}
	// 3. 调用保留和被跳过的环境，核对结果和分发错误。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, _ := runToolCallsWithContext(t, ctx, toolExecutor, []*defs.ToolCall{{
				CallID:    "call",
				Name:      "bash",
				Arguments: tt.arguments,
			}})
			result := results["call"]
			if tt.wantMessage != "" {
				if result.Error == nil || result.Error.Code != "invalid_arguments" || !strings.Contains(result.Error.Message, tt.wantMessage) {
					t.Fatalf("result = %+v, want invalid_arguments containing %q", result, tt.wantMessage)
				}
				return
			}
			var echoed map[string]string
			if result.Error != nil || json.Unmarshal(result.Content, &echoed) != nil || echoed["executor"] != tt.wantExecutor {
				t.Fatalf("result = %+v, want it executed on %s", result, tt.wantExecutor)
			}
		})
	}
}

// TestToolExecutorClientTools 验证客户端 MCP 工具的筛选、参数和实际调用。
func TestToolExecutorClientTools(t *testing.T) {
	// 1. 准备省略、指定和空 MCP server 名单的用例。
	binary := buildFakeMCPServer(t)
	const promptly = 10 * time.Second
	tests := []struct {
		name         string
		toolNames    []string
		mcpServerIDs defs.OptionalStringList
		// hangingClientServer 客户端是否另外登记一个握手永远不完成、启动超时 20 秒（大于 promptly）的 server？
		// 碰了客户端的读取会等满 20 秒，GetTools 的耗时就会超过 promptly。
		hangingClientServer bool
		wantClientTools     bool
	}{
		{
			name:            "nil selection offers client tools",
			wantClientTools: true,
		},
		{
			name:            "tool names do not filter client tools",
			toolNames:       []string{"bash"},
			wantClientTools: true,
		},
		{
			name:                "listed selection offers no client tools",
			mcpServerIDs:        defs.OptionalStringList{"fake"},
			hangingClientServer: true,
		},
		{
			name:                "empty selection offers no client tools",
			mcpServerIDs:        defs.OptionalStringList{},
			hangingClientServer: true,
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClientMCPServersFixture(t)
			servers := []clientServer{{id: "remote"}}
			if tt.hangingClientServer {
				servers = append(servers, clientServer{
					id:             "hang",
					arguments:      []string{"-hang"},
					startupTimeout: 20 * time.Second,
				})
			}
			client.register(t, "c1", servers...)
			opt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{
				newEnvironmentFixture(t, "a", newLocalWorkspace(t), []*mcpclient.ServerDefinition{newFakeServerDefinition("fake", binary, []string{"a"})}, "bash", "read_file").environment,
			}, &echoTool{
				executorID: "agent",
				name:       "list_agents",
				schema:     echoSchema,
			})
			opt.ToolNames = tt.toolNames
			opt.MCPServerIDs = tt.mcpServerIDs
			opt.ClientMCPServers = client.client
			toolExecutor, err := executor.NewToolExecutor(opt)
			if err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			definitions, err := toolExecutor.GetTools(client.ctx)
			if elapsed := time.Since(start); elapsed > promptly {
				t.Fatalf("GetTools() took %v, want it not to wait for unselected client servers", elapsed)
			}
			if err != nil {
				t.Fatal(err)
			}
			var clientTools []*defs.ToolDefinition
			for _, definition := range definitions {
				if definition.MCPServerID == "remote" {
					clientTools = append(clientTools, definition)
				}
			}
			if (len(clientTools) > 0) != tt.wantClientTools {
				t.Fatalf("client tools = %v, want present = %v", getToolNames(clientTools), tt.wantClientTools)
			}
			for _, definition := range clientTools {
				if hasExecutorParameter(t, definition) {
					t.Errorf("client tool %s has an executor parameter, want none", definition.Name)
				}
			}
			if !tt.wantClientTools {
				return
			}
			results, _ := runToolCalls(t, toolExecutor, []*defs.ToolCall{{
				CallID:    "echo",
				Name:      "mcp__remote__echo",
				Arguments: `{"text":"hello"}`,
			}})
			result := results["echo"]
			if result.Error != nil || string(result.Content) != `"hello"` || result.Executor != "" {
				t.Fatalf("client echo result = %+v, want hello without an executor", result)
			}
		})
	}
}

// TestToolExecutorClientToolConflicts 验证客户端同名工具让位于环境工具和协作工具。
func TestToolExecutorClientToolConflicts(t *testing.T) {
	// 1. 准备环境、协作工具重名及无冲突用例。
	binary := buildFakeMCPServer(t)
	tests := []struct {
		name string
		// agentTools 是协作工具。
		agentTools []executor.Tool
		// environmentServerArguments 非 nil 时环境 a 带 server fake，参数为它。
		environmentServerArguments []string
		// wantExecutor 是调用 mcp__fake__argv 时实际执行的一方：executor、agent 或 client。
		wantExecutor string
		// wantReason 是 Warn 里记的重名对象；空表示不应有 Warn。
		wantReason string
	}{
		{
			name:                       "executor MCP tool wins over the client tool",
			environmentServerArguments: []string{"from-executor"},
			wantExecutor:               "executor",
			wantReason:                 "tool name conflicts with an executor tool",
		},
		{
			name: "agent tool wins over the client tool",
			agentTools: []executor.Tool{&echoTool{
				executorID: "agent",
				name:       "mcp__fake__argv",
				schema:     echoSchema,
			}},
			wantExecutor: "agent",
			wantReason:   "tool name conflicts with an agent tool",
		},
		{
			name:         "client tool is offered when nothing clashes",
			wantExecutor: "client",
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClientMCPServersFixture(t)
			client.register(t, "c1", clientServer{
				id:        "fake",
				arguments: []string{"from-client"},
			})
			var environments []executor.ExecutionEnvironment
			if tt.environmentServerArguments != nil {
				servers := []*mcpclient.ServerDefinition{newFakeServerDefinition("fake", binary, []string{"a"}, tt.environmentServerArguments...)}
				environments = append(environments, newEnvironmentFixture(t, "a", newLocalWorkspace(t), servers, "bash", "read_file").environment)
			}
			opt := executor.NewToolExecutorOpt(environments, tt.agentTools...)
			opt.ClientMCPServers = client.client
			toolExecutor, err := executor.NewToolExecutor(opt)
			if err != nil {
				t.Fatal(err)
			}

			definitions, err := toolExecutor.GetTools(client.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if count := len(slices.DeleteFunc(getToolNames(definitions), func(name string) bool { return name != "mcp__fake__argv" })); count != 1 {
				t.Fatalf("mcp__fake__argv offered %d times in %v, want once", count, getToolNames(definitions))
			}
			results, _ := runToolCalls(t, toolExecutor, []*defs.ToolCall{{
				CallID:    "argv",
				Name:      "mcp__fake__argv",
				Arguments: `{}`,
			}})
			result := results["argv"]
			if result.Error != nil {
				t.Fatalf("argv result = %+v, want success", result)
			}
			gotExecutor := "client"
			switch content := string(result.Content); {
			case strings.Contains(content, "from-executor"):
				gotExecutor = "executor"
			case strings.Contains(content, `"executor":"agent"`):
				gotExecutor = "agent"
			case !strings.Contains(content, "from-client"):
				t.Fatalf("argv content = %s, want one of the executor, agent and client servers", content)
			}
			if gotExecutor != tt.wantExecutor {
				t.Errorf("mcp__fake__argv ran on %s, want %s", gotExecutor, tt.wantExecutor)
			}
			// 每个跳过的工具都有日志，这里提取 argv 的记录。
			var reasons []string
			for _, record := range client.pipeline.GetLogRecords(t, "client mcp tool skipped") {
				if record["level"] == "WARN" && record["gen_ai.tool.name"] == "mcp__fake__argv" && record["mcp_server_id"] == "fake" {
					reasons = append(reasons, fmt.Sprint(record["reason"]))
				}
			}
			if wantReasons := slices.DeleteFunc([]string{tt.wantReason}, func(reason string) bool { return reason == "" }); !slices.Equal(reasons, wantReasons) {
				t.Fatalf("client mcp tool skipped reasons for argv = %v, want %v", reasons, wantReasons)
			}
		})
	}
}

// TestToolExecutorHooks 验证 hook 的拦截、参数改写、执行环境和记录顺序。
func TestToolExecutorHooks(t *testing.T) {
	// 1. 准备前置、后置 hook 的结果与期望事件。
	type wantRequest struct {
		event      defs.HookEventName
		executorID string
		arguments  string
	}
	preSaved := &defs.HookRun{ID: "pre-saved"}
	postSaved := &defs.HookRun{ID: "post-saved"}
	preAndPostEvents := []string{"PreToolUse/running", "PreToolUse/completed", "PostToolUse/running", "PostToolUse/completed", "finished"}
	preOnlyEvents := []string{"PreToolUse/running", "PreToolUse/completed", "finished"}
	tests := []struct {
		name            string
		call            *defs.ToolCall
		noHooks         bool
		noEnvironments  bool
		toolFailureCode string
		outcomes        map[defs.HookEventName]*hook.HookOutcome
		wantCode        string
		wantMessage     string
		// wantToolArguments 是工具实际收到的参数；nil 表示工具没有被调用。
		wantToolArguments []string
		wantRequests      []wantRequest
		// wantEvents 是这次调用全部事件的顺序：hook 运行记录写成 事件名/状态，最后是 finished。
		wantEvents []string
		// wantHookRuns 是 ExecutionFinishedEvent.HookRuns 的记录 ID。
		wantHookRuns []defs.HookRunID
	}{
		{
			name: "pre_blocked_does_not_run_tool",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			},
			outcomes: map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {
				IsBlocked:   true,
				BlockReason: "no",
				SavedRuns:   []*defs.HookRun{preSaved},
			}},
			wantCode:    "hook_blocked",
			wantMessage: "blocked by PreToolUse hook: no",
			wantRequests: []wantRequest{{
				defs.HookEventPreToolUse,
				"first",
				`{"path":"a"}`,
			}},
			wantEvents:   preOnlyEvents,
			wantHookRuns: []defs.HookRunID{"pre-saved"},
		},
		{
			name: "blocked_wins_over_stopped",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			},
			outcomes: map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {
				IsBlocked:     true,
				BlockReason:   "no",
				IsTurnStopped: true,
				StopReason:    "done",
			}},
			wantCode:    "hook_blocked",
			wantMessage: "blocked by PreToolUse hook: no",
			wantRequests: []wantRequest{{
				defs.HookEventPreToolUse,
				"first",
				`{"path":"a"}`,
			}},
			wantEvents: preOnlyEvents,
		},
		{
			name: "pre_stopped_with_reason",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			},
			outcomes: map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {
				IsTurnStopped: true,
				StopReason:    "done",
				SavedRuns:     []*defs.HookRun{preSaved},
			}},
			wantCode:    "hook_stopped",
			wantMessage: "turn stopped by hook: done",
			wantRequests: []wantRequest{{
				defs.HookEventPreToolUse,
				"first",
				`{"path":"a"}`,
			}},
			wantEvents:   preOnlyEvents,
			wantHookRuns: []defs.HookRunID{"pre-saved"},
		},
		{
			name: "pre_stopped_without_reason",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			},
			outcomes:    map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {IsTurnStopped: true}},
			wantCode:    "hook_stopped",
			wantMessage: "turn stopped by hook",
			wantRequests: []wantRequest{{
				defs.HookEventPreToolUse,
				"first",
				`{"path":"a"}`,
			}},
			wantEvents: preOnlyEvents,
		},
		{
			name: "pre_rewrites_arguments",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"old","executor":"first"}`,
			},
			outcomes:          map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {UpdatedToolArguments: `{"path":"new"}`}},
			wantToolArguments: []string{`{"path":"new"}`},
			wantRequests: []wantRequest{
				{
					defs.HookEventPreToolUse,
					"first",
					`{"path":"old"}`,
				},
				{
					defs.HookEventPostToolUse,
					"first",
					`{"path":"new"}`,
				},
			},
			wantEvents: preAndPostEvents,
		},
		{
			name: "success_runs_post_after_pre",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			},
			outcomes:          map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {SavedRuns: []*defs.HookRun{preSaved}}, defs.HookEventPostToolUse: {SavedRuns: []*defs.HookRun{postSaved}}},
			wantToolArguments: []string{`{"path":"a"}`},
			wantRequests: []wantRequest{
				{
					defs.HookEventPreToolUse,
					"first",
					`{"path":"a"}`,
				},
				{
					defs.HookEventPostToolUse,
					"first",
					`{"path":"a"}`,
				},
			},
			wantEvents:   preAndPostEvents,
			wantHookRuns: []defs.HookRunID{"pre-saved", "post-saved"},
		},
		{
			name: "failed_tool_skips_post",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			},
			toolFailureCode:   "tool_failed",
			outcomes:          map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {SavedRuns: []*defs.HookRun{preSaved}}, defs.HookEventPostToolUse: {SavedRuns: []*defs.HookRun{postSaved}}},
			wantCode:          "tool_failed",
			wantMessage:       "failed",
			wantToolArguments: []string{`{"path":"a"}`},
			wantRequests: []wantRequest{{
				defs.HookEventPreToolUse,
				"first",
				`{"path":"a"}`,
			}},
			wantEvents:   preOnlyEvents,
			wantHookRuns: []defs.HookRunID{"pre-saved"},
		},
		{
			name: "second_executor_runs_hooks_on_second",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a","executor":"second"}`,
			},
			wantToolArguments: []string{`{"path":"a"}`},
			wantRequests: []wantRequest{
				{
					defs.HookEventPreToolUse,
					"second",
					`{"path":"a"}`,
				},
				{
					defs.HookEventPostToolUse,
					"second",
					`{"path":"a"}`,
				},
			},
			wantEvents: preAndPostEvents,
		},
		{
			name: "agent_tool_runs_hooks_on_default_executor",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "agent_probe",
				Arguments: `{"path":"a"}`,
			},
			wantToolArguments: []string{`{"path":"a"}`},
			wantRequests: []wantRequest{
				{
					defs.HookEventPreToolUse,
					"first",
					`{"path":"a"}`,
				},
				{
					defs.HookEventPostToolUse,
					"first",
					`{"path":"a"}`,
				},
			},
			wantEvents: preAndPostEvents,
		},
		{
			name: "agent_tool_rewrite",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "agent_probe",
				Arguments: `{"path":"old"}`,
			},
			outcomes:          map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {UpdatedToolArguments: `{"path":"new"}`}},
			wantToolArguments: []string{`{"path":"new"}`},
			wantRequests: []wantRequest{
				{
					defs.HookEventPreToolUse,
					"first",
					`{"path":"old"}`,
				},
				{
					defs.HookEventPostToolUse,
					"first",
					`{"path":"new"}`,
				},
			},
			wantEvents: preAndPostEvents,
		},
		{
			name: "nil_hooks_keeps_behavior",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			},
			noHooks:           true,
			wantToolArguments: []string{`{"path":"a"}`},
			wantEvents:        []string{"finished"},
		},
		{
			name: "no_executors_skips_hooks",
			call: &defs.ToolCall{
				CallID:    "call",
				Name:      "agent_probe",
				Arguments: `{"path":"a"}`,
			},
			noEnvironments: true,
			outcomes: map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {
				IsBlocked:   true,
				BlockReason: "no",
			}},
			wantToolArguments: []string{`{"path":"a"}`},
			wantEvents:        []string{"finished"},
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &toolProbe{}
			hooks := &stubHooks{outcomes: tt.outcomes}
			opt := newHookToolExecutorOpt(t, probe, tt.toolFailureCode, !tt.noEnvironments)
			if !tt.noHooks {
				opt.Hooks = hooks
			}
			toolExecutor, err := executor.NewToolExecutor(opt)
			if err != nil {
				t.Fatal(err)
			}

			events := collectToolEvents(t, toolExecutor, []*defs.Item{newHookToolCallItem(tt.call)})

			var gotEvents []string
			var finished *executor.ExecutionFinishedEvent
			for _, event := range events {
				switch event := event.(type) {
				case *executor.ExecutionHookRunEvent:
					gotEvents = append(gotEvents, fmt.Sprintf("%s/%s", event.Run.Event, event.Run.Status))
				case *executor.ExecutionFinishedEvent:
					gotEvents = append(gotEvents, "finished")
					finished = event
				default:
					t.Fatalf("unexpected event %T", event)
				}
			}
			if !slices.Equal(gotEvents, tt.wantEvents) {
				t.Fatalf("events = %v, want %v", gotEvents, tt.wantEvents)
			}
			var gotCode, gotMessage string
			if finished.Result.Error != nil {
				gotCode, gotMessage = finished.Result.Error.Code, finished.Result.Error.Message
			}
			if gotCode != tt.wantCode || gotMessage != tt.wantMessage {
				t.Fatalf("error = %q %q, want %q %q", gotCode, gotMessage, tt.wantCode, tt.wantMessage)
			}
			if got := probe.getArguments(); !slices.Equal(got, tt.wantToolArguments) {
				t.Fatalf("tool arguments = %v, want %v", got, tt.wantToolArguments)
			}
			var gotHookRuns []defs.HookRunID
			for _, run := range finished.HookRuns {
				gotHookRuns = append(gotHookRuns, run.ID)
			}
			if !slices.Equal(gotHookRuns, tt.wantHookRuns) {
				t.Fatalf("hook runs = %v, want %v", gotHookRuns, tt.wantHookRuns)
			}

			requests := hooks.getRequests()
			if len(requests) != len(tt.wantRequests) {
				t.Fatalf("hook requests = %d, want %d", len(requests), len(tt.wantRequests))
			}
			for index, want := range tt.wantRequests {
				got := requests[index]
				if got.Event.Name != want.event || got.ExecutorID != want.executorID || got.Event.ToolCall.Arguments != want.arguments {
					t.Errorf("request %d = %s on %s with %s, want %s on %s with %s", index,
						got.Event.Name, got.ExecutorID, got.Event.ToolCall.Arguments, want.event, want.executorID, want.arguments)
				}
				if root := got.Workspace.GetRootDirectory(); root != "/workspace/"+want.executorID {
					t.Errorf("request %d workspace = %s, want the workspace of %s", index, root, want.executorID)
				}
				if got.Event.Session.ID != "session-1" || got.Event.TurnID != "turn-1" || got.Event.ToolCall.CallID != "call" {
					t.Errorf("request %d session %s turn %s call %s, want session-1 turn-1 call", index,
						got.Event.Session.ID, got.Event.TurnID, got.Event.ToolCall.CallID)
				}
				if want.event == defs.HookEventPreToolUse && got.Event.ToolResult != nil {
					t.Errorf("PreToolUse request carries a tool result: %+v", got.Event.ToolResult)
				}
				if want.event == defs.HookEventPostToolUse &&
					(got.Event.ToolResult == nil || got.Event.ToolResult.Error != nil || string(got.Event.ToolResult.Content) != string(finished.Result.Content)) {
					t.Errorf("PostToolUse tool result = %+v, want the result of the tool %s", got.Event.ToolResult, finished.Result.Content)
				}
			}
		})
	}
}

// TestToolExecutorHooksSkipEnvironmentsWithoutCommands 验证不能跑命令的环境上的工具调用不跑 hook，工具照常执行。
func TestToolExecutorHooksSkipEnvironmentsWithoutCommands(t *testing.T) {
	// 1. 列出能跑命令和不能跑命令的环境；hook 都配置为拦下调用。
	tests := []struct {
		name         string
		opt          *executor.ExecutionEnvironmentOpt
		wantRequests int
		wantCode     string
	}{
		{
			name:         "commands_supported_runs_hooks",
			opt:          executor.NewExecutionEnvironmentOpt("files", "文件", nil),
			wantRequests: 1,
			wantCode:     "hook_blocked",
		},
		{
			name:         "commands_unsupported_skips_hooks",
			opt:          executor.NewExecutionEnvironmentOpt("files", "文件", nil).WithoutCommands(),
			wantRequests: 0,
		},
	}
	// 2. 在环境里放一个探测工具，运行一次调用，核对 hook 请求数和工具结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &toolProbe{}
			tt.opt.Tools = []executor.Tool{&probeTool{name: "probe", probe: probe}}
			work, err := workspace.NewVirtualWorkspace(workspace.NewVirtualWorkspaceOpt("/workspace/files", nil))
			if err != nil {
				t.Fatal(err)
			}
			tt.opt.Workspace = work
			environment, err := executor.NewExecutionEnvironment(tt.opt)
			if err != nil {
				t.Fatal(err)
			}
			hooks := &stubHooks{outcomes: map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {
				IsBlocked:   true,
				BlockReason: "no",
			}}}
			toolOpt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{environment})
			toolOpt.Hooks = hooks
			toolExecutor, err := executor.NewToolExecutor(toolOpt)
			if err != nil {
				t.Fatal(err)
			}
			events := collectToolEvents(t, toolExecutor, []*defs.Item{newHookToolCallItem(&defs.ToolCall{
				CallID:    "call",
				Name:      "probe",
				Arguments: `{"path":"a"}`,
			})})
			var finished *executor.ExecutionFinishedEvent
			for _, event := range events {
				if event, ok := event.(*executor.ExecutionFinishedEvent); ok {
					finished = event
				}
			}
			if finished == nil {
				t.Fatalf("events = %v, want a finished event", events)
			}
			var gotCode string
			if finished.Result.Error != nil {
				gotCode = finished.Result.Error.Code
			}
			if gotCode != tt.wantCode {
				t.Fatalf("error code = %q, want %q", gotCode, tt.wantCode)
			}
			if got := len(hooks.getRequests()); got != tt.wantRequests {
				t.Fatalf("hook requests = %d, want %d", got, tt.wantRequests)
			}
			if wantCalled := tt.wantRequests == 0; (len(probe.getArguments()) == 1) != wantCalled {
				t.Fatalf("tool arguments = %v, want the tool called only when hooks are skipped", probe.getArguments())
			}
		})
	}
}

// TestToolExecutorPreToolUseBeforeDispatch 验证所有前置 hook 并发完成后才开始执行工具。
func TestToolExecutorPreToolUseBeforeDispatch(t *testing.T) {
	// 1. 准备分别阻塞环境工具和协作工具前置 hook 的用例。
	tests := []struct {
		name      string
		slowCall  *defs.ToolCall
		otherCall *defs.ToolCall
	}{
		{
			name: "slow_executor_tool_holds_agent_tool",
			slowCall: &defs.ToolCall{
				CallID:    "slow",
				Name:      "probe",
				Arguments: `{"path":"slow"}`,
			},
			otherCall: &defs.ToolCall{
				CallID:    "other",
				Name:      "agent_probe",
				Arguments: `{"path":"other"}`,
			},
		},
		{
			name: "slow_agent_tool_holds_executor_tool",
			slowCall: &defs.ToolCall{
				CallID:    "slow",
				Name:      "agent_probe",
				Arguments: `{"path":"slow"}`,
			},
			otherCall: &defs.ToolCall{
				CallID:    "other",
				Name:      "probe",
				Arguments: `{"path":"other"}`,
			},
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &toolProbe{}
			hooks := &stubHooks{
				gatedCallIDs: []string{"slow"},
				release:      make(chan struct{}),
			}
			opt := newHookToolExecutorOpt(t, probe, "", true)
			opt.Hooks = hooks
			toolExecutor, err := executor.NewToolExecutor(opt)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			events := make(chan executor.ExecutorEvent)
			var tasks sync.WaitGroup
			tasks.Go(func() {
				toolExecutor.Run(ctx, []*defs.Item{newHookToolCallItem(tt.slowCall), newHookToolCallItem(tt.otherCall)}, events)
			})
			finishedCallIDs := make(chan []string, 1)
			tasks.Go(func() {
				var callIDs []string
				for event := range events {
					if finished, ok := event.(*executor.ExecutionFinishedEvent); ok {
						callIDs = append(callIDs, finished.Result.CallID)
					}
				}
				finishedCallIDs <- callIDs
			})

			// 等两个前置 hook 都收到请求，核对工具尚未执行。
			for len(hooks.getRequests()) < 2 {
				select {
				case <-ctx.Done():
					t.Fatalf("PreToolUse requests = %d, want both calls to trigger it in parallel", len(hooks.getRequests()))
				case <-time.After(time.Millisecond):
				}
			}
			time.Sleep(50 * time.Millisecond)
			if got := probe.getArguments(); len(got) != 0 {
				t.Fatalf("tools ran before every PreToolUse finished: %v", got)
			}

			// 放行前置 hook，等待两个工具调用都完成。
			close(hooks.release)
			tasks.Wait()
			if got := <-finishedCallIDs; len(got) != 2 {
				t.Fatalf("finished calls = %v, want both calls", got)
			}
			if got := probe.getArguments(); len(got) != 2 {
				t.Fatalf("tool runs = %v, want both calls after PreToolUse finished", got)
			}
		})
	}
}

// TestToolExecutorSessionTools 验证 Session 工具独立绑定、重名和环境检查。
func TestToolExecutorSessionTools(t *testing.T) {
	// 1. 准备跨执行器隔离、重名、未知环境和没有环境的用例。
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "session_tools_bound_per_executor",
			run: func(t *testing.T) {
				environment := newEchoEnvironment(t, "local")
				labels := []string{"first", "second"}
				// 1. 两个执行器共享环境，各自保存标签工具并建立工具表。
				toolExecutors := make([]executor.ToolExecutor, len(labels))
				for index, label := range labels {
					opt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{environment})
					opt.SessionTools = map[string][]executor.Tool{"local": {&labelTool{
						name:  "probe",
						label: label,
					}}}
					toolExecutor, err := executor.NewToolExecutor(opt)
					if err != nil {
						t.Fatal(err)
					}
					definitions, err := toolExecutor.GetTools(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if got := getToolNames(definitions); !slices.Equal(got, []string{"probe"}) || hasExecutorParameter(t, definitions[0]) {
						t.Fatalf("GetTools() = %v, want a single probe without an executor parameter", got)
					}
					toolExecutors[index] = toolExecutor
				}
				for index, label := range labels {
					results, _ := runToolCalls(t, toolExecutors[index], []*defs.ToolCall{{
						CallID:    "probe",
						Name:      "probe",
						Arguments: `{}`,
					}})
					result := results["probe"]
					if wantContent := fmt.Sprintf("%q", label); result.Error != nil || string(result.Content) != wantContent || result.Executor != "local" {
						t.Fatalf("probe result = %+v, want %s on local", result, wantContent)
					}
				}
			},
		},
		{
			name: "session_tool_conflicts_with_environment_tool",
			run: func(t *testing.T) {
				opt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{newEchoEnvironment(t, "local", "echo")})
				opt.SessionTools = map[string][]executor.Tool{"local": {&labelTool{
					name:  "echo",
					label: "session",
				}}}
				toolExecutor, err := executor.NewToolExecutor(opt)
				if err != nil {
					t.Fatal(err)
				}
				const wantErr = `execution environment "local" provides tool "echo" more than once`
				if _, err := toolExecutor.GetTools(t.Context()); err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("GetTools() error = %v, want it to contain %q", err, wantErr)
				}
			},
		},
		{
			// b 的内置工具和 Session 工具仍然重名，即使它的 schema 与 a 不同。
			name: "duplicate_after_schema_skip",
			run: func(t *testing.T) {
				const otherSchema = `{"type":"object","properties":{"command":{"type":"string"}}}`
				environmentB, err := executor.NewExecutionEnvironment(executor.NewExecutionEnvironmentOpt("b", "b 环境", nil,
					&echoTool{
						executorID: "b",
						name:       "echo",
						schema:     otherSchema,
					}))
				if err != nil {
					t.Fatal(err)
				}
				opt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{newEchoEnvironment(t, "a", "echo"), environmentB})
				opt.SessionTools = map[string][]executor.Tool{"b": {&echoTool{
					executorID: "b",
					name:       "echo",
					schema:     echoSchema,
				}}}
				toolExecutor, err := executor.NewToolExecutor(opt)
				if err != nil {
					t.Fatal(err)
				}
				const wantErr = `execution environment "b" provides tool "echo" more than once`
				if _, err := toolExecutor.GetTools(t.Context()); err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("GetTools() error = %v, want it to contain %q", err, wantErr)
				}
			},
		},
		{
			name: "session_tools_unknown_environment",
			run: func(t *testing.T) {
				opt := executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{newEchoEnvironment(t, "local")})
				opt.SessionTools = map[string][]executor.Tool{"ssh": {&labelTool{
					name:  "probe",
					label: "ssh",
				}}}
				const wantErr = `unknown execution environment "ssh"`
				if _, err := executor.NewToolExecutor(opt); err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("NewToolExecutor() error = %v, want it to contain %q", err, wantErr)
				}
			},
		},
		{
			name: "no_environments",
			run: func(t *testing.T) {
				probe := &toolProbe{}
				hooks := &stubHooks{outcomes: map[defs.HookEventName]*hook.HookOutcome{defs.HookEventPreToolUse: {
					IsBlocked:   true,
					BlockReason: "no",
				}}}
				opt := newHookToolExecutorOpt(t, probe, "", false)
				opt.Hooks = hooks
				toolExecutor, err := executor.NewToolExecutor(opt)
				if err != nil {
					t.Fatal(err)
				}
				events := collectToolEvents(t, toolExecutor, []*defs.Item{newHookToolCallItem(&defs.ToolCall{
					CallID:    "call",
					Name:      "agent_probe",
					Arguments: `{}`,
				})})
				var finished []*executor.ExecutionFinishedEvent
				for _, event := range events {
					switch event := event.(type) {
					case *executor.ExecutionFinishedEvent:
						finished = append(finished, event)
					case *executor.ExecutionHookRunEvent:
						t.Fatalf("hook run event %+v, want no hook without environments", event.Run)
					}
				}
				if len(finished) != 1 || finished[0].Result.Error != nil || finished[0].Result.Executor != "" || len(finished[0].HookRuns) != 0 {
					t.Fatalf("finished = %+v, want one successful result without an executor or hook runs", finished)
				}
				if requests := hooks.getRequests(); len(requests) != 0 {
					t.Fatalf("hook requests = %d, want none without environments", len(requests))
				}
			},
		},
	}
	// 2. 逐项执行并核对 Session 工具的绑定和配置检查。
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestToolExecutorSharedEnvironmentConcurrent 验证多个 Session 并发使用同一个 MCP 环境。
func TestToolExecutorSharedEnvironmentConcurrent(t *testing.T) {
	// 1. 准备共享环境的 Session 数和调用轮数。
	binary := buildFakeMCPServer(t)
	tests := []struct {
		name     string
		sessions int
		rounds   int
	}{
		{
			name:     "two_sessions_share_one_environment",
			sessions: 2,
			rounds:   10,
		},
	}
	// 2. 逐项创建工具执行器，运行调用并核对期望结果。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environment := newEnvironmentFixture(t, "local", newLocalWorkspace(t),
				[]*mcpclient.ServerDefinition{newFakeServerDefinition("fake", binary, []string{"local"})}, "bash", "read_file").environment
			var tasks sync.WaitGroup
			for session := range tt.sessions {
				toolExecutor, err := executor.NewToolExecutor(executor.NewToolExecutorOpt([]executor.ExecutionEnvironment{environment}))
				if err != nil {
					t.Fatal(err)
				}
				tasks.Go(func() {
					for round := range tt.rounds {
						if _, err := toolExecutor.GetTools(t.Context()); err != nil {
							t.Errorf("session %d round %d GetTools() error = %v", session, round, err)
							return
						}
						text := fmt.Sprintf("session-%d-round-%d", session, round)
						results, _ := runToolCalls(t, toolExecutor, []*defs.ToolCall{{
							CallID:    "echo",
							Name:      "mcp__fake__echo",
							Arguments: fmt.Sprintf(`{"text":%q}`, text),
						}})
						if result := results["echo"]; result == nil || result.Error != nil || string(result.Content) != fmt.Sprintf("%q", text) {
							t.Errorf("session %d round %d echo result = %+v, want %q", session, round, result, text)
						}
					}
				})
			}
			tasks.Wait()
		})
	}
}
