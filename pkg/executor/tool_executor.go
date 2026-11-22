package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/sourcegraph/conc/pool"
)

// ToolExecutor 是一个 Session 的工具表：列出本轮工具，整批完成路由和前置 hook 准备，再单次执行工具调用。
type ToolExecutor interface {
	// GetTools 建出本轮工具表并存下，返回交给模型的工具定义；多个环境提供的工具带 executor 参数。返回内容由调用方持有。
	GetTools(ctx context.Context) ([]*defs.ToolDefinition, error)
	// GetToolsForEnvironments 按环境管理器返回的轮次开场列表和默认 ID 建表；列表为空时只提供协作工具。
	GetToolsForEnvironments(ctx context.Context, environments []ExecutionEnvironment, defaultID string) ([]*defs.ToolDefinition, error)
	// PrepareCalls 为整批调用选择提供方并运行 PreToolUse hook，发送准备阶段的 *ExecutionHookRunEvent 与已结束调用的 *ExecutionFinishedEvent，
	// 返回与输入同顺序、同数量的准备结果；不关闭 events。
	PrepareCalls(ctx context.Context, calls []*defs.Item, events chan<- ExecutorEvent) []*PreparedToolCall
	// Run 执行一次已准备的工具调用，发送 *ExecutionProgressEvent、后置 *ExecutionHookRunEvent 与 *ExecutionFinishedEvent；
	// call.Finished 非 nil 时直接返回，不关闭 events。
	Run(ctx context.Context, call *PreparedToolCall, events chan<- ExecutorEvent)
}

// toolExecutorImpl 一个 Session 一个实例；执行环境由 Conversation 持有，各 Session 共用。
type toolExecutorImpl struct {
	opt *ToolExecutorOpt
	// turnToolRoutes 是最近成功发布的工具表，PrepareCalls 直接按它分发；没有工具表时现建。
	turnToolRoutes atomic.Pointer[toolRoutes]
}

// ToolExecutorOpt 保存环境管理器、工具筛选条件和 hook 配置，构造后只读。
type ToolExecutorOpt struct {
	// EnvironmentsManager 提供在线环境和默认环境，nil 表示没有执行环境。
	EnvironmentsManager ExecutionEnvironmentsManager
	// SessionTools 是本 Session 为各稳定环境构造的工具，键是环境 ID；和环境自带的工具一样参与 ToolNames 过滤、executor 参数和 hook。
	SessionTools map[string][]Tool
	// AgentTools 是协作工具，不带 executor 参数，不受 ToolNames 过滤。
	AgentTools []Tool
	// ToolNames 是内置工具和 Session 工具的可用名单；为空时全部可用。MCP 工具不受它过滤。
	ToolNames []string
	// MCPServerIDs 是可用的 MCP server；nil 表示全部，[] 表示不用。
	MCPServerIDs defs.OptionalStringList
	// Hooks 触发 PreToolUse、PostToolUse；nil 表示没有。没有环境时不触发。
	Hooks hook.Hooks
	// Session 是调用所在的 Session，随 hook 事件交给脚本。
	Session defs.Session
}

// NewToolExecutorOpt 保存环境管理器和协作工具。
func NewToolExecutorOpt(manager ExecutionEnvironmentsManager, agentTools ...Tool) *ToolExecutorOpt {
	return &ToolExecutorOpt{
		EnvironmentsManager: manager,
		AgentTools:          slices.Clone(agentTools),
	}
}

// NewToolExecutor 校验配置并创建 Session 的工具执行器。
func NewToolExecutor(opt *ToolExecutorOpt) (ToolExecutor, error) {
	// 1. 校验配置及各组工具定义。
	if opt == nil {
		return nil, errors.New("tool executor options are required")
	}
	if err := validateToolDefinitions(opt.AgentTools); err != nil {
		return nil, err
	}
	for _, tools := range opt.SessionTools {
		if err := validateToolDefinitions(tools); err != nil {
			return nil, err
		}
	}
	// 2. 复制配置并保存工具执行器。
	executorOpt := NewToolExecutorOpt(opt.EnvironmentsManager, opt.AgentTools...)
	executorOpt.SessionTools = maps.Clone(opt.SessionTools)
	executorOpt.ToolNames = slices.Clone(opt.ToolNames)
	executorOpt.MCPServerIDs = slices.Clone(opt.MCPServerIDs)
	executorOpt.Hooks = opt.Hooks
	executorOpt.Session = opt.Session
	return &toolExecutorImpl{opt: executorOpt}, nil
}

// GetTools 按此刻在线的环境建立并发布工具表。
func (e *toolExecutorImpl) GetTools(ctx context.Context) ([]*defs.ToolDefinition, error) {
	var environments []ExecutionEnvironment
	var defaultID string
	if e.opt.EnvironmentsManager != nil {
		environments, defaultID = e.opt.EnvironmentsManager.GetEnvironments()
	}
	return e.GetToolsForEnvironments(ctx, environments, defaultID)
}

// GetToolsForEnvironments 按给定环境建立工具表，生成模型工具定义后再发布。
func (e *toolExecutorImpl) GetToolsForEnvironments(ctx context.Context, environments []ExecutionEnvironment, defaultID string) ([]*defs.ToolDefinition, error) {
	routes, err := e.buildToolRoutes(ctx, environments, defaultID)
	if err != nil {
		return nil, err
	}
	definitions, err := routes.getDefinitions()
	if err != nil {
		return nil, err
	}
	e.turnToolRoutes.Store(routes)
	return definitions, nil
}

// PrepareCalls 为整批调用选择提供方并运行前置 hook，返回与输入同顺序的准备结果。
func (e *toolExecutorImpl) PrepareCalls(ctx context.Context, calls []*defs.Item, events chan<- ExecutorEvent) []*PreparedToolCall {
	// 1. 取得本轮工具表，失败时给每个调用发送错误结果并记入准备项。
	preparedCalls := make([]*PreparedToolCall, len(calls))
	routes, err := e.getTurnToolRoutes(ctx)
	if err != nil {
		for index, call := range calls {
			logUndispatchedCall(ctx, call, "executor_unavailable", "error", err)
			finished := &ExecutionFinishedEvent{Result: newToolErrorResult(call.ToolCall.CallID, "executor_unavailable", err)}
			events <- finished
			preparedCalls[index] = &PreparedToolCall{
				Call:     call.Clone(),
				Finished: finished,
			}
		}
		return preparedCalls
	}
	// 2. 为调用选择提供方，无法分发的调用直接发送错误结果并记入准备项。
	for index, call := range calls {
		provider, routedCall, failedResult := routes.routeCall(call)
		if failedResult != nil {
			logUndispatchedCall(ctx, call, failedResult.Error.Code)
			finished := &ExecutionFinishedEvent{Result: failedResult}
			events <- finished
			preparedCalls[index] = &PreparedToolCall{
				Call:     call.Clone(),
				Finished: finished,
			}
			continue
		}
		preparedCalls[index] = &PreparedToolCall{
			Call:     routedCall,
			provider: provider,
		}
	}
	// 3. 等前置 hook 全部结束，应用拦截、停止和改写参数后返回准备结果。
	preOutcomes := e.runPreToolUseHooks(ctx, preparedCalls, events)
	e.applyPreToolUseOutcomes(ctx, preparedCalls, preOutcomes, events)
	return preparedCalls
}

// Run 执行一次已准备的工具调用。
func (e *toolExecutorImpl) Run(ctx context.Context, call *PreparedToolCall, events chan<- ExecutorEvent) {
	// 1. 准备阶段已结束的调用直接返回，其余调用执行工具并发送结果。
	if call == nil || call.Finished != nil {
		return
	}
	e.executeCall(ctx, call, events)
}

// buildToolRoutes 汇总协作工具、环境工具和 MCP 工具，建立本轮工具表。
func (e *toolExecutorImpl) buildToolRoutes(ctx context.Context, environments []ExecutionEnvironment, defaultID string) (*toolRoutes, error) {
	// 1. 并发读取各环境选中的 MCP 工具。显式选择 MCP server 时只读取稳定环境的 MCP 工具。
	type environmentMCPTools struct {
		tools []Tool
		err   error
	}
	mcpReadings := make([]environmentMCPTools, len(environments))
	tasks := pool.New()
	for index, environment := range environments {
		if environment.IsDynamic() && e.opt.MCPServerIDs != nil {
			continue
		}
		tasks.Go(func() {
			mcpReadings[index].tools, mcpReadings[index].err = environment.GetMCPTools(ctx, e.opt.MCPServerIDs)
		})
	}
	tasks.Wait()

	// 2. 先登记协作工具，随后按环境顺序合并各环境提供的工具。
	routes := &toolRoutes{
		environments: slices.Clone(environments),
		defaultID:    defaultID,
	}
	for _, tool := range e.opt.AgentTools {
		routes.routes = append(routes.routes, &toolRoute{
			definition: tool.GetDefinition(),
			providers:  []toolProvider{{tool: tool}},
		})
	}
	for index, environment := range environments {
		reading := mcpReadings[index]
		environmentID := environment.GetID()
		if reading.err != nil {
			return nil, fmt.Errorf("get tools of executor %q: %w", environmentID, reading.err)
		}
		var sessionTools []Tool
		if !environment.IsDynamic() {
			sessionTools = e.opt.SessionTools[environmentID]
		}
		selectedTools := slices.DeleteFunc(slices.Concat(environment.GetTools(), sessionTools), func(tool Tool) bool {
			return len(e.opt.ToolNames) > 0 && !slices.Contains(e.opt.ToolNames, tool.GetDefinition().Name)
		})
		environmentTools := slices.Concat(selectedTools, reading.tools)
		environmentToolNames := make(map[string]struct{}, len(environmentTools))
		for _, tool := range environmentTools {
			definition := tool.GetDefinition()
			if _, exists := environmentToolNames[definition.Name]; exists {
				if !environment.IsDynamic() {
					return nil, fmt.Errorf("execution environment %q provides tool %q more than once", environmentID, definition.Name)
				}
				telemetry.GetLogger(telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID(environmentID), telemetry.ToolName(definition.Name))).Warn("duplicate dynamic tool skipped",
					"mcp_server_id", definition.MCPServerID)
				continue
			}
			environmentToolNames[definition.Name] = struct{}{}
			if err := routes.addEnvironmentTool(ctx, toolProvider{
				environment: environment,
				tool:        tool,
				executorID:  environmentID,
			}); err != nil {
				return nil, err
			}
		}
	}
	return routes, nil
}

// getTurnToolRoutes 取得本轮工具表，首次使用时读取在线环境建表。
func (e *toolExecutorImpl) getTurnToolRoutes(ctx context.Context) (*toolRoutes, error) {
	if routes := e.turnToolRoutes.Load(); routes != nil {
		return routes, nil
	}
	if _, err := e.GetTools(ctx); err != nil {
		return nil, err
	}
	return e.turnToolRoutes.Load(), nil
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

// getDefinitions 按稳定环境工具、协作工具、纯动态环境工具的顺序返回定义副本。
func (r *toolRoutes) getDefinitions() ([]*defs.ToolDefinition, error) {
	var staticTools, agentTools, dynamicTools []*defs.ToolDefinition
	for _, route := range r.routes {
		definition := route.definition
		if len(route.providers) == 1 {
			definition = definition.Clone()
		} else {
			withExecutor, err := route.getDefinitionWithExecutorParameter(r.defaultID, !r.hasDefaultProvider(route))
			if err != nil {
				return nil, err
			}
			definition = withExecutor
		}
		// 稳定环境先登记，同名工具有稳定提供方时一定排在首位。
		switch environment := route.providers[0].environment; {
		case environment == nil:
			agentTools = append(agentTools, definition)
		case environment.IsDynamic():
			dynamicTools = append(dynamicTools, definition)
		default:
			staticTools = append(staticTools, definition)
		}
	}
	return slices.Concat(staticTools, agentTools, dynamicTools), nil
}

// routeCall 按工具名和 executor 选择提供方，返回调用或分发错误。
func (r *toolRoutes) routeCall(call *defs.Item) (toolProvider, *defs.Item, *defs.ToolResult) {
	// 1. 协作工具或声明自有 executor 参数的单来源工具直接转发。
	callID := call.ToolCall.CallID
	route := r.findRoute(call.ToolCall.Name)
	if route == nil {
		return toolProvider{}, nil, newToolErrorResult(callID, "tool_not_found", fmt.Errorf("tool %q is not available", call.ToolCall.Name))
	}
	provider := route.providers[0]
	if provider.environment == nil || (len(route.providers) == 1 && route.isExecutorArgumentDeclared()) {
		return provider, call.Clone(), nil
	}

	// 2. 取出 executor；有多个提供方且默认环境不提供工具时要求显式指定。
	executorID, arguments, err := takeExecutorArgument(call.ToolCall.Arguments)
	if err != nil {
		return toolProvider{}, nil, newToolErrorResult(callID, "invalid_arguments", err)
	}
	if executorID == "" && len(route.providers) > 1 && !r.hasDefaultProvider(route) {
		const hint = `; retry with an explicit "executor", or ask the user which one to use if unclear. Available: %s`
		available := route.getProviderDescriptions()
		if r.defaultID != "" {
			err = fmt.Errorf("default executor %q does not provide tool %q, and multiple other executors provide it"+hint, r.defaultID, call.ToolCall.Name, available)
		} else {
			err = fmt.Errorf("multiple executors provide tool %q with no default executor"+hint, call.ToolCall.Name, available)
		}
		return toolProvider{}, nil, newToolErrorResult(callID, "invalid_arguments", err)
	}
	if executorID != "" {
		var found bool
		provider, found = route.findProvider(executorID)
		if !found {
			available := route.getProviderDescriptions()
			if !slices.ContainsFunc(r.environments, func(environment ExecutionEnvironment) bool { return environment.GetID() == executorID }) {
				return toolProvider{}, nil, newToolErrorResult(callID, "invalid_arguments",
					fmt.Errorf("executor %q is not configured; available: %s", executorID, available))
			}
			return toolProvider{}, nil, newToolErrorResult(callID, "invalid_arguments",
				fmt.Errorf("executor %q does not provide tool %q; available: %s", executorID, call.ToolCall.Name, available))
		}
	}

	// 3. 复制调用并剥离路由参数。
	routedCall := call.Clone()
	routedCall.ToolCall.Arguments = arguments
	return provider, routedCall, nil
}

// getHookEnvironment 选择能执行命令的提供方环境，协作工具使用默认稳定环境。
func (e *toolExecutorImpl) getHookEnvironment(provider toolProvider) ExecutionEnvironment {
	environment := provider.environment
	if environment == nil && e.opt.EnvironmentsManager != nil {
		environments, defaultID := e.opt.EnvironmentsManager.GetEnvironments()
		if defaultID != "" {
			environment = environments[0]
		}
	}
	if environment == nil || !environment.IsCommandSupported() {
		return nil
	}
	return environment
}

// runPreToolUseHooks 并发运行前置 hook，并返回与 preparedCalls 位置对应的合并结果。
func (e *toolExecutorImpl) runPreToolUseHooks(ctx context.Context, preparedCalls []*PreparedToolCall, events chan<- ExecutorEvent) []*hook.HookOutcome {
	// 1. 为未提前结束且配置了 hook 的调用并发触发 PreToolUse，等待全部结束。
	preOutcomes := make([]*hook.HookOutcome, len(preparedCalls))
	if e.opt.Hooks == nil {
		return preOutcomes
	}
	tasks := pool.New()
	for index, prepared := range preparedCalls {
		if prepared.Finished != nil {
			continue
		}
		environment := e.getHookEnvironment(prepared.provider)
		if environment == nil {
			continue
		}
		tasks.Go(func() {
			preOutcomes[index] = e.runToolHookEvent(ctx, environment, &hook.HookEvent{
				Name:     defs.HookEventPreToolUse,
				Session:  e.opt.Session,
				TurnID:   prepared.Call.TurnID,
				ToolCall: prepared.Call.ToolCall,
			}, events)
		})
	}
	tasks.Wait()
	return preOutcomes
}

// applyPreToolUseOutcomes 处理前置 hook 的结果，写回已结束结果或改写后的调用参数。
func (e *toolExecutorImpl) applyPreToolUseOutcomes(
	ctx context.Context,
	preparedCalls []*PreparedToolCall,
	preOutcomes []*hook.HookOutcome,
	events chan<- ExecutorEvent,
) {
	// 1. 逐个处理调用：拦截或停止时记录并发送错误结果，放行时应用改写参数与前置运行记录。
	for index, prepared := range preparedCalls {
		outcome := preOutcomes[index]
		if outcome == nil {
			continue
		}
		var errorCode, message string
		switch {
		case outcome.IsBlocked:
			errorCode, message = "hook_blocked", "blocked by PreToolUse hook: "+outcome.BlockReason
		case outcome.IsTurnStopped && outcome.StopReason == "":
			errorCode, message = defs.ToolErrorHookStopped, "turn stopped by hook"
		case outcome.IsTurnStopped:
			errorCode, message = defs.ToolErrorHookStopped, "turn stopped by hook: "+outcome.StopReason
		default:
			if outcome.UpdatedToolArguments != "" {
				prepared.Call = prepared.Call.Clone()
				prepared.Call.ToolCall.Arguments = outcome.UpdatedToolArguments
			}
			prepared.preRuns = outcome.SavedRuns
			continue
		}
		logUndispatchedCall(ctx, prepared.Call, errorCode)
		finished := &ExecutionFinishedEvent{
			Result:   newToolErrorResult(prepared.Call.ToolCall.CallID, errorCode, errors.New(message)),
			HookRuns: outcome.SavedRuns,
		}
		prepared.Finished = finished
		events <- finished
	}
}

// executeCall 执行放行的工具，汇集进度、规则和 hook 记录后发送最终结果。
func (e *toolExecutorImpl) executeCall(ctx context.Context, prepared *PreparedToolCall, events chan<- ExecutorEvent) {
	// 1. 补充提供方的 executor ID，启动工具遥测并按配置记录参数。
	call := prepared.Call
	if executorID := prepared.provider.executorID; executorID != "" {
		ctx = telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID(executorID))
	}
	toolCtx, recorder := telemetry.StartExecuteTool(ctx, telemetry.ExecuteToolStart{
		Name:        call.ToolCall.Name,
		CallID:      call.ToolCall.CallID,
		Description: prepared.provider.tool.GetDefinition().Description,
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
	var result *ToolExecutionResult
	if tool, ok := prepared.provider.tool.(*mcpTool); ok && tool.isDynamic {
		connection, found := e.getDynamicMCPConnection(tool.executorID, tool.serverID)
		if found {
			result = &ToolExecutionResult{Result: connection.CallTool(toolCtx, call.Clone())}
		} else {
			err := fmt.Errorf("executor %q MCP server %q is not connected", tool.executorID, tool.serverID)
			result = &ToolExecutionResult{Result: newToolErrorResult(call.ToolCall.CallID, "mcp_server_unavailable", err)}
		}
	} else {
		result = prepared.provider.tool.Execute(toolCtx, call.Clone(), progress)
	}
	close(progress)
	forwarding.Wait()
	if recorder.IsContentCaptured() {
		recorder.RecordResult(getToolResultContent(result.Result))
	}
	recorder.End(getToolErrorCode(result.Result))

	// 3. 给结果和目录规则填入实际环境 ID，协作工具留空；没有 hook 时直接发送完成事件。
	environmentID := ""
	if prepared.provider.environment != nil {
		environmentID = prepared.provider.environment.GetID()
	}
	result.Result.Executor = environmentID
	for _, instruction := range result.AgentsInstructions {
		instruction.Executor = environmentID
	}
	finished := &ExecutionFinishedEvent{
		Result:             result.Result,
		AgentsInstructions: result.AgentsInstructions,
		ShouldRefreshTools: result.ShouldRefreshTools,
		HookRuns:           prepared.preRuns,
	}
	// 4. 调用成功且有可用的 hook 环境时运行后置 hook，最后发送完成事件。
	if e.opt.Hooks == nil || finished.Result.Error != nil {
		events <- finished
		return
	}
	if environment := e.getHookEnvironment(prepared.provider); environment != nil {
		postOutcome := e.runToolHookEvent(ctx, environment, &hook.HookEvent{
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

// getDynamicMCPConnection 查询同一动态环境上登记的同名 MCP 连接。
func (e *toolExecutorImpl) getDynamicMCPConnection(executorID, serverID string) (mcpclient.Connection, bool) {
	if e.opt.EnvironmentsManager == nil {
		return nil, false
	}
	environment, _ := e.opt.EnvironmentsManager.GetEnvironment(executorID)
	dynamic, ok := environment.(*executionEnvironmentImpl)
	if !ok || !dynamic.IsDynamic() {
		return nil, false
	}
	return dynamic.getMCPConnection(serverID)
}

// runToolHookEvent 运行一个工具 hook 事件，并转发运行记录。
func (e *toolExecutorImpl) runToolHookEvent(ctx context.Context, environment ExecutionEnvironment, event *hook.HookEvent, events chan<- ExecutorEvent) *hook.HookOutcome {
	// 1. 启动 hook 记录转发，把每条记录包成 ExecutionHookRunEvent 发出。
	runUpdates := make(chan *defs.HookRun)
	tasks := pool.New()
	tasks.Go(func() {
		for run := range runUpdates {
			events <- &ExecutionHookRunEvent{Run: run}
		}
	})
	// 2. 在指定环境运行 hook，关闭记录通道并等待转发结束。
	outcome := e.opt.Hooks.RunEvent(ctx, &hook.HookEventRequest{
		Event:      event,
		ExecutorID: environment.GetID(),
		Workspace:  environment.GetWorkspace(),
	}, runUpdates)
	close(runUpdates)
	tasks.Wait()
	return outcome
}
