package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/sourcegraph/conc/pool"
)

// ToolExecutor 是一个 Session 的工具表：列出本轮工具，按工具名和 executor 参数找到工具对象，跑完 hook 后执行。
type ToolExecutor interface {
	// GetTools 建出本轮工具表并存下，返回交给模型的工具定义；多个环境提供的工具带 executor 参数。返回内容由调用方持有。
	GetTools(ctx context.Context) ([]*defs.ToolDefinition, error)
	// Run 并发执行本批调用，按完成顺序发送 *ExecutionProgressEvent、*ExecutionHookRunEvent 与每个调用一次 *ExecutionFinishedEvent，
	// 全部结束后关闭 events。调用方读取 events 到关闭；调用 Item 克隆后才交给工具。
	Run(ctx context.Context, calls []*defs.Item, events chan<- ExecutorEvent)
}

// toolExecutorImpl 一个 Session 一个实例；执行环境由 Conversation 持有，各 Session 共用。
type toolExecutorImpl struct {
	opt *ToolExecutorOpt
	// turnToolRoutes 是最近一次 GetTools 建出的工具表，Run 直接按它分发；
	// 没调过 GetTools 时为 nil，Run 现建一次并存下。
	turnToolRoutes atomic.Pointer[toolRoutes]
}

// ToolExecutorOpt 保存环境、工具筛选条件、客户端 MCP 和 hook 配置，构造后只读。
type ToolExecutorOpt struct {
	// Environments 是能用的执行环境，第一个是默认环境；由 Conversation 持有，ToolExecutor 只借用。
	Environments []ExecutionEnvironment
	// SessionTools 是本 Session 为各环境构造的工具，键是环境 ID；和环境自带的工具一样参与 ToolNames 过滤、executor 参数和 hook。
	SessionTools map[string][]Tool
	// AgentTools 是协作工具，不带 executor 参数，不受 ToolNames 过滤。
	AgentTools []Tool
	// ClientMCPServers 提供客户端登记的 MCP 工具；nil 表示没有。只在 MCPServerIDs 为 nil 时使用，与其他工具重名时让位。
	ClientMCPServers ClientMCPServers
	// ToolNames 是内置工具和 Session 工具的可用名单；为空时全部可用。MCP 工具不受它过滤。
	ToolNames []string
	// MCPServerIDs 是可用的 MCP server；nil 表示全部，[] 表示不用。
	MCPServerIDs defs.OptionalStringList
	// Hooks 触发 PreToolUse、PostToolUse；nil 表示没有。没有环境时不触发。
	Hooks hook.Hooks
	// Session 是调用所在的 Session，随 hook 事件交给脚本。
	Session defs.Session
}

// NewToolExecutorOpt 保存可用环境和协作工具。
func NewToolExecutorOpt(environments []ExecutionEnvironment, agentTools ...Tool) *ToolExecutorOpt {
	// 1. 复制环境列表和协作工具列表。
	return &ToolExecutorOpt{
		Environments: slices.Clone(environments),
		AgentTools:   slices.Clone(agentTools),
	}
}

// NewToolExecutor 校验配置并创建 Session 的工具执行器。
func NewToolExecutor(opt *ToolExecutorOpt) (ToolExecutor, error) {
	// 1. 校验配置和环境都不为空、环境 ID 不重复、Session 工具只指向已有环境，以及各组工具定义。
	if opt == nil {
		return nil, errors.New("tool executor options are required")
	}
	var environmentIDs []string
	for _, environment := range opt.Environments {
		if environment == nil {
			return nil, errors.New("execution environment is missing")
		}
		if slices.Contains(environmentIDs, environment.GetID()) {
			return nil, fmt.Errorf("executor %q is defined more than once", environment.GetID())
		}
		environmentIDs = append(environmentIDs, environment.GetID())
	}
	if err := validateToolDefinitions(opt.AgentTools); err != nil {
		return nil, err
	}
	for environmentID, tools := range opt.SessionTools {
		if !slices.Contains(environmentIDs, environmentID) {
			return nil, fmt.Errorf("session tools refer to unknown execution environment %q", environmentID)
		}
		if err := validateToolDefinitions(tools); err != nil {
			return nil, err
		}
	}
	// 2. 复制配置并保存工具执行器。
	executorOpt := NewToolExecutorOpt(opt.Environments, opt.AgentTools...)
	executorOpt.SessionTools = maps.Clone(opt.SessionTools)
	executorOpt.ClientMCPServers = opt.ClientMCPServers
	executorOpt.ToolNames = slices.Clone(opt.ToolNames)
	executorOpt.MCPServerIDs = slices.Clone(opt.MCPServerIDs)
	executorOpt.Hooks = opt.Hooks
	executorOpt.Session = opt.Session
	return &toolExecutorImpl{opt: executorOpt}, nil
}

// GetTools 建立并保存本轮工具表，返回工具定义。
func (e *toolExecutorImpl) GetTools(ctx context.Context) ([]*defs.ToolDefinition, error) {
	// 1. 建立工具表，保存后提取定义副本。
	routes, err := e.buildToolRoutes(ctx)
	if err != nil {
		return nil, err
	}
	e.turnToolRoutes.Store(routes)
	return routes.getDefinitions()
}

// Run 分配本批调用，运行前置 hook，再并发执行放行的工具。
func (e *toolExecutorImpl) Run(ctx context.Context, calls []*defs.Item, events chan<- ExecutorEvent) {
	// 1. 取得本轮工具表，失败时给每个调用发送错误结果，返回时关闭 events。
	defer close(events)
	routes, err := e.getTurnToolRoutes(ctx)
	if err != nil {
		for _, call := range calls {
			logUndispatchedCall(ctx, call, "executor_unavailable", "error", err)
			events <- &ExecutionFinishedEvent{Result: newToolErrorResult(call.ToolCall.CallID, "executor_unavailable", err)}
		}
		return
	}
	// 2. 为调用选择提供方，无法分发的调用直接发送错误结果。
	var routedCalls []*routedToolCall
	for _, call := range calls {
		provider, routedCall, failedResult := routes.routeCall(call)
		if failedResult != nil {
			logUndispatchedCall(ctx, call, failedResult.Error.Code)
			events <- &ExecutionFinishedEvent{Result: failedResult}
			continue
		}
		routedCalls = append(routedCalls, &routedToolCall{
			call:     routedCall,
			provider: provider,
		})
	}
	// 3. 等前置 hook 全部结束，再并发执行放行的调用并等待完成。
	e.runPreToolUseHooks(ctx, routedCalls, events)
	tasks := pool.New()
	for _, routed := range e.applyPreToolUseOutcomes(ctx, routedCalls, events) {
		tasks.Go(func() { e.executeCall(ctx, routed, events) })
	}
	tasks.Wait()
}

// buildToolRoutes 汇总协作工具、环境工具、环境 MCP 工具和客户端 MCP 工具，建立本轮工具表。
func (e *toolExecutorImpl) buildToolRoutes(ctx context.Context) (*toolRoutes, error) {
	// 1. 并发读取各环境选中的 MCP 工具；有客户端 MCP 且 MCPServerIDs 为 nil 时也读取客户端 MCP 工具。
	type environmentMCPTools struct {
		tools []Tool
		err   error
	}
	mcpReadings := make([]environmentMCPTools, len(e.opt.Environments))
	var clientTools []Tool
	var clientErr error
	tasks := pool.New()
	for index, environment := range e.opt.Environments {
		tasks.Go(func() {
			mcpReadings[index].tools, mcpReadings[index].err = environment.GetMCPTools(ctx, e.opt.MCPServerIDs)
		})
	}
	if e.opt.ClientMCPServers != nil && e.opt.MCPServerIDs == nil {
		tasks.Go(func() { clientTools, clientErr = e.opt.ClientMCPServers.GetMCPTools(ctx) })
	}
	tasks.Wait()

	// 2. 先登记协作工具，再按环境顺序筛选内置工具和 Session 工具、合并 MCP 工具并检查重名；某个环境读取 MCP 工具失败时返回错误。
	routes := &toolRoutes{environments: e.opt.Environments}
	for _, tool := range e.opt.AgentTools {
		routes.routes = append(routes.routes, &toolRoute{
			definition: tool.GetDefinition(),
			providers:  []toolProvider{{tool: tool}},
		})
	}
	for index, environment := range e.opt.Environments {
		reading := mcpReadings[index]
		if reading.err != nil {
			return nil, fmt.Errorf("get tools of executor %q: %w", environment.GetID(), reading.err)
		}
		selectedTools := slices.DeleteFunc(slices.Concat(environment.GetTools(), e.opt.SessionTools[environment.GetID()]), func(tool Tool) bool {
			return len(e.opt.ToolNames) > 0 && !slices.Contains(e.opt.ToolNames, tool.GetDefinition().Name)
		})
		environmentTools := slices.Concat(selectedTools, reading.tools)
		if name, found := getDuplicateToolName(environmentTools); found {
			return nil, fmt.Errorf("execution environment %q provides tool %q more than once", environment.GetID(), name)
		}
		for _, tool := range environmentTools {
			if err := routes.addEnvironmentTool(ctx, toolProvider{
				environment: environment,
				tool:        tool,
				executorID:  environment.GetID(),
			}); err != nil {
				return nil, err
			}
		}
	}
	// 3. 最后登记客户端 MCP 工具；读取失败时返回错误，与已有工具重名时跳过。
	if clientErr != nil {
		return nil, fmt.Errorf("get tools of executor %q: %w", defs.ClientExecutorID, clientErr)
	}
	for _, tool := range clientTools {
		routes.addClientTool(ctx, toolProvider{
			tool:       tool,
			executorID: defs.ClientExecutorID,
		})
	}
	return routes, nil
}

// getTurnToolRoutes 取得本轮工具表。
func (e *toolExecutorImpl) getTurnToolRoutes(ctx context.Context) (*toolRoutes, error) {
	// 1. 有本轮工具表时直接返回。
	if routes := e.turnToolRoutes.Load(); routes != nil {
		return routes, nil
	}
	// 2. 建立并保存首次使用的工具表。
	routes, err := e.buildToolRoutes(ctx)
	if err != nil {
		return nil, err
	}
	e.turnToolRoutes.Store(routes)
	return routes, nil
}

// addEnvironmentTool 登记环境提供的工具，处理同名工具。
func (r *toolRoutes) addEnvironmentTool(ctx context.Context, provider toolProvider) error {
	// 1. 读取工具定义；工具名尚未登记时追加到工具表。
	environmentID := provider.environment.GetID()
	definition := provider.tool.GetDefinition()
	route := r.findRoute(definition.Name)
	if route == nil {
		r.routes = append(r.routes, &toolRoute{
			definition: definition,
			providers:  []toolProvider{provider},
		})
		return nil
	}
	// 2. 与协作工具重名时报错；环境间同名但 schema 不同时记 Warn 后跳过。
	if route.providers[0].environment == nil {
		return fmt.Errorf("executor %q tool %q conflicts with an agent tool", environmentID, definition.Name)
	}
	if !bytes.Equal(route.definition.InputSchema, definition.InputSchema) {
		telemetry.GetLogger(telemetry.ContextWithAttrs(ctx, telemetry.ToolName(definition.Name))).Warn("tool schema differs between executors",
			"kept_executor_id", route.providers[0].environment.GetID(), "skipped_executor_id", environmentID)
		return nil
	}
	// 3. schema 相同时追加环境提供方。
	route.providers = append(route.providers, provider)
	return nil
}

// addClientTool 登记客户端 MCP 工具并处理同名冲突。
func (r *toolRoutes) addClientTool(ctx context.Context, provider toolProvider) {
	// 1. 读取定义，未重名时追加客户端工具。
	definition := provider.tool.GetDefinition()
	route := r.findRoute(definition.Name)
	if route == nil {
		r.routes = append(r.routes, &toolRoute{
			definition: definition,
			providers:  []toolProvider{provider},
		})
		return
	}
	// 2. 保留已有同名工具，识别提供方类型并用 Warn 记录客户端工具被跳过。
	var conflict string
	switch kept := route.providers[0]; {
	case kept.environment != nil:
		conflict = "executor tool"
	case kept.executorID == defs.ClientExecutorID:
		conflict = "client tool"
	default:
		conflict = "agent tool"
	}
	telemetry.GetLogger(telemetry.ContextWithAttrs(ctx, telemetry.ToolName(definition.Name))).Warn("client mcp tool skipped",
		"mcp_server_id", definition.MCPServerID, "reason", "tool name conflicts with an "+conflict)
}

// getDefinitions 生成交给模型的工具定义副本：环境工具在前，协作工具和客户端 MCP 工具在后。
func (r *toolRoutes) getDefinitions() ([]*defs.ToolDefinition, error) {
	// 1. 先收集环境工具，多提供方时补充 executor 参数。
	var definitions []*defs.ToolDefinition
	for _, route := range r.routes {
		if route.providers[0].environment == nil {
			continue
		}
		if len(route.providers) < 2 {
			definitions = append(definitions, route.definition.Clone())
			continue
		}
		definition, err := route.getDefinitionWithExecutorParameter()
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	// 2. 按加入顺序收集协作工具和客户端 MCP 工具。
	for _, route := range r.routes {
		if route.providers[0].environment == nil {
			definitions = append(definitions, route.definition.Clone())
		}
	}
	return definitions, nil
}

// routeCall 按工具名和 executor 选择提供方，返回调用或分发错误。
func (r *toolRoutes) routeCall(call *defs.Item) (toolProvider, *defs.Item, *defs.ToolResult) {
	// 1. 查找工具；协作工具和客户端 MCP 工具直接交给它唯一的提供方，参数不动。
	callID := call.ToolCall.CallID
	route := r.findRoute(call.ToolCall.Name)
	if route == nil {
		return toolProvider{}, nil, newToolErrorResult(callID, "tool_not_found", fmt.Errorf("tool %q is not available", call.ToolCall.Name))
	}
	if route.providers[0].environment == nil {
		return route.providers[0], call, nil
	}
	// 2. 取出 executor，省略时用第一个提供方；环境没配置或不提供这个工具时返回 invalid_arguments。
	executorID, arguments, err := takeExecutorArgument(call.ToolCall.Arguments)
	if err != nil {
		return toolProvider{}, nil, newToolErrorResult(callID, "invalid_arguments", err)
	}
	provider, found := route.providers[0], true
	if executorID != "" {
		provider, found = route.findProvider(executorID)
	}
	isEnvironmentConfigured := slices.ContainsFunc(r.environments, func(environment ExecutionEnvironment) bool { return environment.GetID() == executorID })
	if !found && !isEnvironmentConfigured {
		return toolProvider{}, nil, newToolErrorResult(callID, "invalid_arguments",
			fmt.Errorf("executor %q is not configured; available: %s", executorID, strings.Join(route.getProviderIDs(), ", ")))
	}
	if !found {
		return toolProvider{}, nil, newToolErrorResult(callID, "invalid_arguments",
			fmt.Errorf("executor %q does not provide tool %q; available: %s", executorID, call.ToolCall.Name, strings.Join(route.getProviderIDs(), ", ")))
	}
	// 3. 复制调用并删除交给工具的 executor 参数。
	routedCall := call.Clone()
	routedCall.ToolCall.Arguments = arguments
	return provider, routedCall, nil
}

// getHookEnvironment 选择工具 hook 的执行环境。
func (e *toolExecutorImpl) getHookEnvironment(provider toolProvider) ExecutionEnvironment {
	// 1. 优先取提供方环境，非环境工具取配置中的第一个环境，没有环境时返回 nil。
	if provider.environment != nil {
		return provider.environment
	}
	if len(e.opt.Environments) > 0 {
		return e.opt.Environments[0]
	}
	return nil
}

// isHooked 这次调用是否配置了 hook，且有环境运行 hook？
func (e *toolExecutorImpl) isHooked(routed *routedToolCall) bool {
	// 1. 检查 Hooks 和运行 hook 的环境。
	return e.opt.Hooks != nil && e.getHookEnvironment(routed.provider) != nil
}

// runPreToolUseHooks 并发运行前置 hook，并保存各调用的合并结果。
func (e *toolExecutorImpl) runPreToolUseHooks(ctx context.Context, routedCalls []*routedToolCall, events chan<- ExecutorEvent) {
	// 1. 为有 hook 的调用并发触发 PreToolUse，等待全部结束。
	tasks := pool.New()
	for _, routed := range routedCalls {
		if !e.isHooked(routed) {
			continue
		}
		tasks.Go(func() {
			routed.preOutcome = e.runToolHookEvent(ctx, e.getHookEnvironment(routed.provider), &hook.HookEvent{
				Name:     defs.HookEventPreToolUse,
				Session:  e.opt.Session,
				TurnID:   routed.call.TurnID,
				ToolCall: routed.call.ToolCall,
			}, events)
		})
	}
	tasks.Wait()
}

// applyPreToolUseOutcomes 处理前置 hook 的结果，返回放行的调用。
func (e *toolExecutorImpl) applyPreToolUseOutcomes(ctx context.Context, routedCalls []*routedToolCall, events chan<- ExecutorEvent) []*routedToolCall {
	// 1. 逐个处理调用：拦截或停止时发送错误结果，放行时应用改写参数。
	var dispatchedCalls []*routedToolCall
	for _, routed := range routedCalls {
		if !e.isHooked(routed) {
			dispatchedCalls = append(dispatchedCalls, routed)
			continue
		}
		outcome := routed.preOutcome
		var errorCode, message string
		switch {
		case outcome.IsBlocked:
			errorCode, message = "hook_blocked", "blocked by PreToolUse hook: "+outcome.BlockReason
		case outcome.IsTurnStopped && outcome.StopReason == "":
			errorCode, message = "hook_stopped", "turn stopped by hook"
		case outcome.IsTurnStopped:
			errorCode, message = "hook_stopped", "turn stopped by hook: "+outcome.StopReason
		default:
			if outcome.UpdatedToolArguments != "" {
				routed.call = routed.call.Clone()
				routed.call.ToolCall.Arguments = outcome.UpdatedToolArguments
			}
			dispatchedCalls = append(dispatchedCalls, routed)
			continue
		}
		logUndispatchedCall(ctx, routed.call, errorCode)
		events <- &ExecutionFinishedEvent{
			Result:   newToolErrorResult(routed.call.ToolCall.CallID, errorCode, errors.New(message)),
			HookRuns: outcome.SavedRuns,
		}
	}
	return dispatchedCalls
}

// executeCall 执行放行的工具，汇集进度、规则和 hook 记录后发送最终结果。
func (e *toolExecutorImpl) executeCall(ctx context.Context, routed *routedToolCall, events chan<- ExecutorEvent) {
	// 1. 补充提供方的 executor ID，启动工具遥测并按配置记录参数。
	call := routed.call
	if executorID := routed.provider.executorID; executorID != "" {
		ctx = telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID(executorID))
	}
	toolCtx, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
		Name:        call.ToolCall.Name,
		CallID:      call.ToolCall.CallID,
		Description: routed.provider.tool.GetDefinition().Description,
	})
	if recorder.IsContentCaptured() {
		recorder.RecordArguments(telemetry.DecodeJSONContent([]byte(call.ToolCall.Arguments)))
	}
	// 2. 转发工具进度，执行调用副本；工具返回后关闭进度并等待转发结束，再记录结果。
	progress := make(chan *ExecutionProgressEvent)
	var forwarding sync.WaitGroup
	forwarding.Go(func() {
		for event := range progress {
			events <- event
		}
	})
	result := routed.provider.tool.Execute(toolCtx, call.Clone(), progress)
	close(progress)
	forwarding.Wait()
	if recorder.IsContentCaptured() {
		recorder.RecordResult(getToolResultContent(result.Result))
	}
	recorder.End(getToolErrorCode(result.Result))

	// 3. 给结果和目录规则填入实际环境 ID，协作工具和客户端工具留空；没有 hook 时直接发送完成事件。
	environmentID := ""
	if routed.provider.environment != nil {
		environmentID = routed.provider.environment.GetID()
	}
	result.Result.Executor = environmentID
	for _, instruction := range result.AgentsInstructions {
		instruction.Executor = environmentID
	}
	finished := &ExecutionFinishedEvent{
		Result:             result.Result,
		AgentsInstructions: result.AgentsInstructions,
	}
	if !e.isHooked(routed) {
		events <- finished
		return
	}
	// 4. 加入前置 hook 记录；成功时运行后置 hook，最后发送完成事件。
	finished.HookRuns = routed.preOutcome.SavedRuns
	if finished.Result.Error == nil {
		postOutcome := e.runToolHookEvent(ctx, e.getHookEnvironment(routed.provider), &hook.HookEvent{
			Name:       defs.HookEventPostToolUse,
			Session:    e.opt.Session,
			TurnID:     call.TurnID,
			ToolCall:   call.ToolCall,
			ToolResult: finished.Result,
		}, events)
		finished.HookRuns = slices.Concat(finished.HookRuns, postOutcome.SavedRuns)
	}
	events <- finished
}

// runToolHookEvent 运行一个工具 hook 事件，并转发运行记录。
func (e *toolExecutorImpl) runToolHookEvent(ctx context.Context, environment ExecutionEnvironment, event *hook.HookEvent, events chan<- ExecutorEvent) *hook.HookOutcome {
	// 1. 不能跑命令的环境没法运行 hook 脚本，直接返回空结果。
	if !environment.IsCommandSupported() {
		return &hook.HookOutcome{}
	}
	// 2. 启动 hook 记录转发，把每条记录包成 ExecutionHookRunEvent 发出。
	runUpdates := make(chan *defs.HookRun)
	tasks := pool.New()
	tasks.Go(func() {
		for run := range runUpdates {
			events <- &ExecutionHookRunEvent{Run: run}
		}
	})
	// 3. 在指定环境运行 hook，关闭记录通道并等待转发结束。
	outcome := e.opt.Hooks.RunEvent(ctx, &hook.HookEventRequest{
		Event:      event,
		ExecutorID: environment.GetID(),
		Workspace:  environment.GetWorkspace(),
	}, runUpdates)
	close(runUpdates)
	tasks.Wait()
	return outcome
}
