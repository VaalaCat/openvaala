// Package turn 执行一轮 Agent，并保存输入、输出和本轮状态。
package turn

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/agent/agent_context"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/executor"
	"github.com/VaalaCat/openvaala/pkg/hook"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// Turn 保存本轮输入、运行 Agent 并写入输出和结束状态。
type Turn interface {
	// Run 保存输入、加载历史、执行 Agent 并返回本轮结果；写库不受 ctx 取消影响。
	// 写输入前运行启动和用户输入 hook；输入被拦下或 hook 要求结束时跳过 Agent，记录 completed。
	// 被拦下的输入不保存，但仍计入 InputItemIDs；UserPromptSubmit 只处理用户输入。
	// 调用方持续读取 events 到 Run 返回，nil 表示不订阅；IsCompactOnly 时 input 传 nil。
	Run(ctx context.Context, input []*defs.Item, events chan<- TurnEvent) (TurnResult, error)
}

// TurnInbox 由 Session 提供，按 Turn 取出补充输入。
type TurnInbox interface {
	// TakeSteer 取走挂在 turnID 上的补充输入；finish 为 true 且没取到输入时，停止向这个 Turn 投递。
	TakeSteer(ctx context.Context, turnID defs.TurnID, finish bool) ([][]*defs.Item, error)
}

// turnImpl 保存本轮记录、输入 ID、Agent 输出的保存进度和统计。
type turnImpl struct {
	opt *TurnOpt
	// record 是本轮 Turn 记录，状态变化时写库。
	record defs.Turn
	// lastSavedItemOrdinal 是本轮最后保存的 Item 流水号。
	lastSavedItemOrdinal uint64
	// itemSaveErr 保存追加 Item 或记录首批范围时的错误；出错后不再保存后续 Item，本轮留在 running，由 Session 恢复。
	itemSaveErr error

	// resolvedAgentItems 按输出位置暂存已结束、还没保存的 Item；nil 表示这个位置已结束但不用保存。
	resolvedAgentItems map[int]*defs.Item
	// nextPersistedItemIndex 是下一个待保存的 Agent 输出位置。
	nextPersistedItemIndex int
	// inputItemIDs 按读入顺序保存首批输入和补充输入的首个 Item ID，包括被用户输入 hook 拦下的输入。
	inputItemIDs []defs.ItemID
	// events 是 Run 开始时保存的调用方事件通道；nil 表示不订阅。
	events chan<- TurnEvent
	// agentStats 是 Agent 返回的本轮统计；Agent 未运行时为零值。
	agentStats agent.AgentStats
}

// TurnOpt 指定本轮模型、持久化接口、输入来源、执行环境和 hook。
// NewTurnOpt 只填对话、角色、存储和模型客户端，其余字段由 Session 补齐；TurnID、模型和上一轮信息在启动每个 Turn 时填写。
type TurnOpt struct {
	TurnID defs.TurnID
	// ConversationID 是所属对话，读写 Item 和保存 Turn 记录时据此定位对话目录。
	ConversationID defs.ConversationID
	SessionID      defs.SessionID
	// Agent 提供名称、说明和系统指令。
	Agent    *agent.AgentDefinition
	Query    dao.Query
	Mutation dao.Mutation
	Client   llmkit.Client
	// ToolExecutor 是本 Session 的工具表，交给 Agent 列出工具和执行调用；nil 表示没有工具。
	ToolExecutor executor.ToolExecutor
	// Environments 是本对话的执行环境，第一个是默认环境；Turn 用它记录本轮 Executor、交给 Agent 读目录规则，并在默认环境运行 hook。
	Environments []executor.ExecutionEnvironment
	// Inbox 提供本轮的补充输入。
	Inbox TurnInbox
	// Model 是本 Turn 使用的模型，指向目录里的只读定义。
	Model *defs.ModelDefinition
	// ReasoningEffort 是已解析的推理强度；模型不支持选择时为空。
	ReasoningEffort string
	// CompactModel 是本 Turn 压缩使用的模型。
	CompactModel *defs.ModelDefinition
	// PreviousModel 是上一个 Turn 使用的模型；零值表示还没有。
	PreviousModel defs.ModelRef
	// PreviousContextUsage 是最近一次完整普通调用的报告；上一轮没有完整普通调用时沿用更早的报告，没有时为 nil。
	PreviousContextUsage *defs.TurnContextUsage
	// PreviousExecutors 是上一个 Turn 开始时的 Executor 列表；为空表示还没有上一轮。
	PreviousExecutors []defs.TurnExecutor
	// IsCompactOnly 这个 Turn 是否只压缩上下文？是的话 Run 的 input 为空，不写输入和模型、Executor 变化 Item，
	// 只让 Agent 把历史压成摘要；Turn 记录照常写 running 和终态。
	IsCompactOnly bool
	// Hooks 在写输入前触发 SessionStart、SubagentStart 和 UserPromptSubmit，也交给 Agent 触发工具与 Stop 事件；
	// nil 或 Environments 为空表示不触发 hook。
	Hooks hook.Hooks
	// Session 是本 Turn 所属 Session 的记录，随 hook 事件交给脚本；ParentSessionID 非空表示子 Session。
	Session defs.Session
	// HookStartSource 只有 Session 启动后的第一个非压缩 Turn 非空：startup 或 resume。
	// 非空时 Turn 在写输入前触发 SessionStart（子 Session 触发 SubagentStart，不带来源）；为空则不触发。
	HookStartSource string
}

// NewTurnOpt 填写本轮所属对话、角色和执行依赖。
func NewTurnOpt(
	conversationID defs.ConversationID,
	sessionID defs.SessionID,
	agentDefinition *agent.AgentDefinition,
	query dao.Query,
	mutation dao.Mutation,
	client llmkit.Client,
) *TurnOpt {
	// 1. 保存对话和 Session 身份、角色定义、存储接口和模型客户端。
	return &TurnOpt{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Agent:          agentDefinition,
		Query:          query,
		Mutation:       mutation,
		Client:         client,
	}
}

// NewTurn 检查配置并创建本轮 Turn。
func NewTurn(opt *TurnOpt) (Turn, error) {
	// 1. 检查本轮身份、Agent、模型、存储接口、客户端和输入来源。
	if opt == nil || opt.TurnID == "" || opt.ConversationID == "" || opt.SessionID == "" || opt.Agent == nil || opt.Model == nil || opt.CompactModel == nil ||
		opt.Query == nil || opt.Mutation == nil || opt.Client == nil || opt.Inbox == nil {
		return nil, errors.New("turn requires a turn ID, conversation ID, session ID, agent definition, model, compact model, DAO query, DAO mutation, model client and inbox")
	}
	// 2. 保存配置并创建已结束输出暂存表。
	return &turnImpl{
		opt:                opt,
		resolvedAgentItems: make(map[int]*defs.Item),
	}, nil
}

// Run 执行本轮输入、Agent 和持久化流程，并记录遥测。
func (t *turnImpl) Run(
	ctx context.Context,
	input []*defs.Item,
	events chan<- TurnEvent,
) (TurnResult, error) {
	// 1. 克隆输入并填写本轮 TurnID；不是只压缩时把首条输入 ID 加到遥测属性上。
	turnInput := make([]*defs.Item, len(input))
	for index, item := range input {
		turnInput[index] = item.Clone()
		turnInput[index].TurnID = t.opt.TurnID
	}
	if !t.opt.IsCompactOnly {
		ctx = telemetry.ContextWithAttrs(ctx, telemetry.InputItemID(string(turnInput[0].ID)))
	}
	// 2. 开始记录本轮 Agent 调用的遥测，记录内容时写入输入，再执行 Turn。
	ctx, recorder := telemetry.StartInvokeAgent(ctx, telemetry.InvokeAgentStart{
		AgentName:        t.opt.Agent.Name,
		AgentDescription: t.opt.Agent.Description,
		Model:            t.opt.Model.Target.Model,
		TurnID:           string(t.opt.TurnID),
	})
	if recorder.IsContentCaptured() {
		recorder.RecordInput(agent_context.NewTelemetryChatMessages(turnInput))
	}
	result, err := t.runTurn(ctx, turnInput, events)
	// 3. 汇总本轮统计和最后一条回复，记录调用结果。
	summary := newTurnSummary(t.agentStats)
	if recorder.IsContentCaptured() {
		summary.Output = agent_context.NewTelemetryChatOutput(getLastAgentMessage(result.Outputs))
	}
	recorder.End(telemetry.InvokeAgentEnd{
		Status:  string(result.Status),
		Err:     getTurnFailure(result.Status, err),
		Summary: summary,
	})
	return result, err
}

// runTurn 保存本轮输入和状态，加载上下文并执行 Agent。
func (t *turnImpl) runTurn(
	ctx context.Context,
	turnInput []*defs.Item,
	events chan<- TurnEvent,
) (TurnResult, error) {
	// 1. 保存事件通道和首条输入 ID，创建不随取消结束的写库上下文。
	t.events = events
	persistCtx := context.WithoutCancel(ctx)
	if !t.opt.IsCompactOnly {
		t.inputItemIDs = []defs.ItemID{turnInput[0].ID}
	}

	// 2. 记录本轮模型和 Executor，沿用上一轮的上下文报告，保存 running 状态；保存失败时返回 ErrTurnUnsettled。
	turnExecutors := executor.GetTurnExecutors(t.opt.Environments)
	previousContextUsage := t.opt.PreviousContextUsage
	if previousContextUsage != nil && t.opt.PreviousModel != (defs.ModelRef{}) && t.opt.PreviousModel != t.opt.Model.Ref {
		telemetry.GetLogger(ctx).Warn("context usage restore skipped", "reason", "model_changed", "model_call_id", previousContextUsage.ModelCallID)
		previousContextUsage = nil
	}
	t.record = defs.Turn{
		ID:             t.opt.TurnID,
		ConversationID: t.opt.ConversationID,
		SessionID:      t.opt.SessionID,
		Status:         defs.TurnStatusRunning,
		ContextUsage:   previousContextUsage,
		Model: defs.ModelSelection{
			Ref:             t.opt.Model.Ref,
			ReasoningEffort: t.opt.ReasoningEffort,
		},
		Executors: turnExecutors,
	}
	if err := t.opt.Mutation.SetTurn(persistCtx, t.record); err != nil {
		return TurnResult{InputItemIDs: t.inputItemIDs}, errors.Join(ErrTurnUnsettled, fmt.Errorf("create running turn: %w", err))
	}
	// 3. 只压缩的 Turn 跳过输入和输入 hook；其他 Turn 保存输入，输入被拦下或 hook 要求结束时直接记为 completed。
	if !t.opt.IsCompactOnly {
		isAgentSkipped, err := t.appendTurnInput(ctx, turnExecutors, turnInput)
		if err != nil {
			return t.finishTurn(ctx, nil, err)
		}
		if isAgentSkipped {
			return t.completeTurn(persistCtx, nil, "")
		}
	}

	// 4. 加载 Session 历史并创建 Agent 上下文。
	items, err := dao.LoadContextHistory(ctx, t.opt.Query, t.opt.ConversationID, t.opt.SessionID, "")
	if err != nil {
		return t.finishTurn(ctx, nil, fmt.Errorf("load context history: %w", err))
	}
	contextOpt := agent_context.NewAgentContextOpt(t.opt.SessionID, t.opt.TurnID, t.opt.Client, t.opt.Model, t.opt.CompactModel)
	contextOpt.ReasoningEffort = t.opt.ReasoningEffort
	agentContext, err := agent_context.NewAgentContext(contextOpt)
	if err != nil {
		return t.finishTurn(ctx, nil, err)
	}
	if err := agentContext.Load(&agent_context.ContextLoadParam{
		System:       t.opt.Agent.Instructions,
		Items:        items,
		ContextUsage: t.record.ContextUsage,
	}); err != nil {
		return t.finishTurn(ctx, nil, fmt.Errorf("load agent context: %w", err))
	}
	// 5. 创建本轮 Agent 和补充输入通道，再执行 Agent。
	inputRequests := make(chan agent.AgentInputRequest)
	agentOpt := agent.NewAgentOpt(t.opt.TurnID, t.opt.SessionID, t.opt.Client, t.opt.Model, agentContext)
	agentOpt.ToolExecutor = t.opt.ToolExecutor
	agentOpt.Environments = t.opt.Environments
	agentOpt.InputRequests = inputRequests
	agentOpt.IsCompactOnly = t.opt.IsCompactOnly
	agentOpt.Hooks = t.opt.Hooks
	agentOpt.Session = t.opt.Session
	agentInstance, err := agent.NewAgent(agentOpt)
	if err != nil {
		return t.finishTurn(ctx, nil, err)
	}

	return t.runAgent(ctx, agentInstance, inputRequests)
}

// appendTurnInput 运行输入 hook 并保存本轮输入与配置变化，返回是否跳过 Agent。
func (t *turnImpl) appendTurnInput(ctx context.Context, turnExecutors []defs.TurnExecutor, turnInput []*defs.Item) (isAgentSkipped bool, err error) {
	// 1. 模型或 Executor 列表与上一轮不同时，依次创建变化 Item。
	var batch []*defs.Item
	if t.opt.PreviousModel != (defs.ModelRef{}) && t.opt.PreviousModel != t.opt.Model.Ref {
		// 记录模型切换；历史加载时不再发送这条记录之前的推理。
		batch = append(batch, &defs.Item{
			ID:     defs.NewItemID(),
			TurnID: t.opt.TurnID,
			Kind:   defs.ItemModelChange,
			ModelChange: &defs.ModelChange{
				PreviousModel: t.opt.PreviousModel,
				Model:         t.opt.Model.Ref,
			},
		})
	}
	if len(t.opt.PreviousExecutors) > 0 && !slices.Equal(t.opt.PreviousExecutors, turnExecutors) {
		batch = append(batch, &defs.Item{
			ID:     defs.NewItemID(),
			TurnID: t.opt.TurnID,
			Kind:   defs.ItemExecutorsChange,
			ExecutorsChange: &defs.ExecutorsChange{
				PreviousExecutors: t.opt.PreviousExecutors,
				Executors:         turnExecutors,
			},
		})
	}
	// 2. HookStartSource 非空时运行 SessionStart 或 SubagentStart，追加保存的运行记录；hook 要求结束时跳过 Agent。
	if t.opt.HookStartSource != "" {
		startEvent := &hook.HookEvent{
			Name:   defs.HookEventSessionStart,
			Source: t.opt.HookStartSource,
		}
		if t.opt.Session.ParentSessionID != "" {
			startEvent = &hook.HookEvent{Name: defs.HookEventSubagentStart}
		}
		startOutcome := t.runHookEvent(ctx, startEvent)
		batch = append(batch, newHookRunItems(startOutcome.SavedRuns, t.opt.TurnID)...)
		isAgentSkipped = startOutcome.IsTurnStopped
	}
	// 3. 启动 hook 要求结束时追加输入；否则运行 UserPromptSubmit，依次追加未被拦下的输入和用户输入 hook 记录。
	if isAgentSkipped {
		batch = append(batch, turnInput...)
	} else {
		promptOutcome := t.runUserPromptSubmitHook(ctx, turnInput)
		if !promptOutcome.IsBlocked {
			batch = append(batch, turnInput...)
		}
		batch = append(batch, newHookRunItems(promptOutcome.SavedRuns, t.opt.TurnID)...)
		isAgentSkipped = promptOutcome.IsBlocked || promptOutcome.IsTurnStopped
	}
	// 4. 使用不受取消影响的上下文保存整批内容，并发送保存事件。
	saved, err := t.appendItems(context.WithoutCancel(ctx), batch...)
	if err != nil {
		return false, fmt.Errorf("append turn input: %w", err)
	}
	t.sendItemsSaved(saved)
	return isAgentSkipped, nil
}

// runUserPromptSubmitHook 对一组用户输入运行 UserPromptSubmit。
func (t *turnImpl) runUserPromptSubmitHook(ctx context.Context, group []*defs.Item) *hook.HookOutcome {
	// 1. 不是用户输入时返回空结果，否则提取正文运行 UserPromptSubmit。
	if !isUserInput(group) {
		return &hook.HookOutcome{}
	}
	return t.runHookEvent(ctx, &hook.HookEvent{
		Name:   defs.HookEventUserPromptSubmit,
		Prompt: getPromptText(group),
	})
}

// runHookEvent 在默认环境运行 hook 并转发运行记录。
func (t *turnImpl) runHookEvent(ctx context.Context, event *hook.HookEvent) *hook.HookOutcome {
	// 1. 没有 hook 或环境时返回空结果，否则填写本轮 Session 和 TurnID。
	if t.opt.Hooks == nil || len(t.opt.Environments) == 0 {
		return &hook.HookOutcome{}
	}
	event.Session = t.opt.Session
	event.TurnID = t.opt.TurnID
	defaultEnvironment := t.opt.Environments[0]
	// 2. 按发生顺序转发开始和结束记录，在默认环境执行 hook。
	runUpdates := make(chan *defs.HookRun)
	var tasks sync.WaitGroup
	tasks.Go(func() {
		for run := range runUpdates {
			t.sendEvent(&TurnHookRunEvent{Run: run})
		}
	})
	outcome := t.opt.Hooks.RunEvent(ctx, &hook.HookEventRequest{
		Event:      event,
		ExecutorID: defaultEnvironment.GetID(),
		Workspace:  defaultEnvironment.GetWorkspace(),
	}, runUpdates)
	// 3. 关闭更新通道并等待转发结束，返回 hook 结果。
	close(runUpdates)
	tasks.Wait()
	return outcome
}

// getTurnFailure 选择本轮遥测使用的错误。
func getTurnFailure(status defs.TurnStatus, err error) error {
	// 1. 本轮保存的结束状态为 interrupted 时返回 context.Canceled，其他状态返回原错误。
	if status == defs.TurnStatusInterrupted {
		return context.Canceled
	}
	return err
}

// newTurnSummary 将 Agent 统计转换为本轮遥测小结。
func newTurnSummary(stats agent.AgentStats) telemetry.TurnSummary {
	// 1. 复制模型步骤、调用次数、工具次数、用量和结束原因。
	return telemetry.TurnSummary{
		Steps:           stats.Steps,
		InferenceCalls:  stats.InferenceCalls,
		ToolCalls:       stats.ToolCalls,
		FailedToolCalls: stats.FailedToolCalls,
		Usage:           stats.Usage,
		FinishReason:    stats.FinishReason,
	}
}

// getLastAgentMessage 提取本轮最后一条回复。
func getLastAgentMessage(items []*defs.Item) []*defs.Item {
	// 1. 从末尾查找消息 Item，返回只含这条消息的切片；没有消息时返回 nil。
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].Kind == defs.ItemMessage {
			return []*defs.Item{items[index]}
		}
	}
	return nil
}

// runAgent 运行 Agent、处理补充输入并保存输出。
func (t *turnImpl) runAgent(
	ctx context.Context,
	agentInstance agent.Agent,
	inputRequests <-chan agent.AgentInputRequest,
) (TurnResult, error) {
	// 1. 创建 Agent 运行上下文和事件通道，启动 Agent。
	persistCtx := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	agentEventChannel := make(chan *agent.AgentEvent)
	var agentResult agent.AgentResult
	var agentErr error
	var tasks sync.WaitGroup
	tasks.Go(func() {
		agentResult, agentErr = agentInstance.Run(runCtx, agentEventChannel)
	})

	// 2. 处理补充输入请求和输出事件；首次保存失败时取消 Agent，继续读到事件关闭。
	var agentEvents <-chan *agent.AgentEvent = agentEventChannel
	var persistErr error
	for agentEvents != nil {
		select {
		case request := <-inputRequests:
			t.handleAgentInputRequested(runCtx, request)
		case event, open := <-agentEvents:
			if !open {
				agentEvents = nil
				continue
			}
			if persistErr != nil {
				continue
			}
			persistErr = t.handleAgentEvent(persistCtx, event)
			if persistErr != nil {
				cancel()
			}
		}
	}
	// 3. 等待 Agent 退出并收集统计，保存失败时结束本轮。
	tasks.Wait()
	t.agentStats = agentResult.Stats
	items := agentResult.Items
	if persistErr != nil {
		return t.finishTurn(ctx, items, errors.Join(agentErr, persistErr))
	}

	// 4. 跳过未结束的位置，保存剩余完整输出，再按 Agent 结果记录本轮终态。
	t.skipUnresolvedAgentItems(len(items))
	saved, err := t.persistResolvedAgentItems(persistCtx)
	if err != nil {
		return t.finishTurn(ctx, items, errors.Join(agentErr, err))
	}
	t.sendItemsSaved(saved)
	if agentErr != nil {
		return t.finishTurn(ctx, items, agentErr)
	}

	return t.completeTurn(persistCtx, items, agentResult.FinalModelCallID)
}

// completeTurn 保存完成状态并返回本轮结果。
func (t *turnImpl) completeTurn(ctx context.Context, items []*defs.Item, finalModelCallID defs.ModelCallID) (TurnResult, error) {
	// 1. 已有 Item 保存错误时保留 running，返回 ErrTurnUnsettled。
	if t.itemSaveErr != nil {
		return TurnResult{
			InputItemIDs: t.inputItemIDs,
			Outputs:      items,
		}, errors.Join(ErrTurnUnsettled, t.itemSaveErr)
	}
	// 2. 填写日志末尾、completed、最终调用和累计用量，保存本轮记录；保存失败时返回 ErrTurnUnsettled。
	t.record.ItemsRange.LastOrdinal = t.lastSavedItemOrdinal
	t.record.Status = defs.TurnStatusCompleted
	t.record.FinalModelCallID = finalModelCallID
	t.record.Usage = t.agentStats.Usage
	if err := t.opt.Mutation.SetTurn(ctx, t.record); err != nil {
		return TurnResult{
				InputItemIDs: t.inputItemIDs,
				Outputs:      items,
			},
			errors.Join(ErrTurnUnsettled, fmt.Errorf("complete turn: %w", err))
	}
	return TurnResult{
		InputItemIDs:     t.inputItemIDs,
		Outputs:          items,
		FinalModelCallID: t.record.FinalModelCallID,
		Status:           t.record.Status,
		ContextUsage:     t.record.ContextUsage,
	}, nil
}

// handleAgentInputRequested 读取补充输入，运行输入 hook 后保存并回复 Agent。
func (t *turnImpl) handleAgentInputRequested(ctx context.Context, request agent.AgentInputRequest) {
	// 1. 本轮已取消时直接回复错误，没取走的输入留在 Inbox。
	if err := ctx.Err(); err != nil {
		replyAgentInput(request, nil, err)
		return
	}
	// 2. 取走补充输入，逐组登记首个 ID、填写 TurnID 并运行 UserPromptSubmit；被拦下的输入不交给 Agent，但仍计入 InputItemIDs；
	// hook 要求结束只留下运行记录，不结束本轮。
	groups, err := t.opt.Inbox.TakeSteer(ctx, t.opt.TurnID, request.FinishIfEmpty)
	var items, batch []*defs.Item
	for _, group := range groups {
		t.inputItemIDs = append(t.inputItemIDs, group[0].ID)
		for _, item := range group {
			item.TurnID = t.opt.TurnID
		}
		promptOutcome := t.runUserPromptSubmitHook(ctx, group)
		hookRunItems := newHookRunItems(promptOutcome.SavedRuns, t.opt.TurnID)
		if !promptOutcome.IsBlocked {
			// 将未被拦下的输入和 hook 记录一起回复给 Agent。
			items = append(items, group...)
			items = append(items, hookRunItems...)
			batch = append(batch, group...)
		}
		batch = append(batch, hookRunItems...)
	}
	// 3. 保存输入和 hook 记录，再向 Agent 回复本次读取结果；取走后才取消的输入照常保存，保存失败的输入也已计入 InputItemIDs。
	if err == nil && len(batch) > 0 {
		saved, appendErr := t.appendItems(context.WithoutCancel(ctx), batch...)
		if appendErr != nil {
			err = fmt.Errorf("append steer input: %w", appendErr)
		}
		t.sendItemsSaved(saved)
	}
	replyAgentInput(request, items, err)
}

// replyAgentInput 回复补充输入结果。
func replyAgentInput(request agent.AgentInputRequest, items []*defs.Item, err error) {
	// 1. 向容量为 1 的回复通道发送输入和错误，再关闭通道。
	request.Reply <- agent.AgentInputResult{
		Items: items,
		Err:   err,
	}
	close(request.Reply)
}

// handleAgentEvent 保存已结束的 Item，再发送 Agent 事件和 Item 保存事件。
func (t *turnImpl) handleAgentEvent(ctx context.Context, event *agent.AgentEvent) error {
	// 1. 遇到完成或中断事件时暂存对应 Item，再保存从下一个待保存位置起连续已结束的 Item。
	var saved []dao.LoggedItem
	if isAgentTerminalEvent(event) {
		// 事件里的 Item 是要保存的内容，nil 表示这个位置不用保存。
		t.resolvedAgentItems[event.ItemIndex] = event.Item
		var err error
		if saved, err = t.persistResolvedAgentItems(ctx); err != nil {
			return err
		}
	}
	// 2. 完整普通调用替换本轮报告，包括没有用量或实际模式未知的响应。
	if event.Type == agent.AgentEventModelCallFinished && event.ModelCall.ContextUsage != nil {
		t.record.ContextUsage = event.ModelCall.ContextUsage
	}
	// 3. 先发送 Agent 事件，再发送本次保存的 Item。
	t.sendEvent(&TurnAgentEvent{Event: event})
	t.sendItemsSaved(saved)
	return nil
}

// isAgentTerminalEvent 这条事件是否结束了一个输出位置的 Item？
func isAgentTerminalEvent(event *agent.AgentEvent) bool {
	// 1. 检查输出位置有效，且事件类型为完成或中断。
	return event.ItemIndex >= 0 &&
		(event.Type == agent.AgentEventItemCompleted || event.Type == agent.AgentEventItemInterrupted)
}

// skipUnresolvedAgentItems 把 Agent 返回后仍没结束的输出位置记为不用保存。
func (t *turnImpl) skipUnresolvedAgentItems(itemCount int) {
	// 1. 遍历还没保存的位置，把没有结束记录的位置填为 nil。
	for index := t.nextPersistedItemIndex; index < itemCount; index++ {
		if _, resolved := t.resolvedAgentItems[index]; !resolved {
			t.resolvedAgentItems[index] = nil
		}
	}
}

// persistResolvedAgentItems 保存连续已结束的输出 Item，返回带流水号和保存时间的副本。
func (t *turnImpl) persistResolvedAgentItems(ctx context.Context) ([]dao.LoggedItem, error) {
	// 1. 从下一个待保存位置收集连续已结束的 Item，跳过不用保存的位置并后移待保存位置；一条都没有时直接返回。
	var items []*defs.Item
	for {
		item, resolved := t.resolvedAgentItems[t.nextPersistedItemIndex]
		if !resolved {
			break
		}
		delete(t.resolvedAgentItems, t.nextPersistedItemIndex)
		t.nextPersistedItemIndex++
		if item != nil {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	// 2. 追加这批 Item，返回已保存内容。
	saved, err := t.appendItems(ctx, items...)
	if err != nil {
		return nil, fmt.Errorf("append agent item: %w", err)
	}
	return saved, nil
}

// sendEvent 把事件发给订阅方。
func (t *turnImpl) sendEvent(event TurnEvent) {
	// 1. 没有订阅时直接返回，否则发送事件。
	if t.events != nil {
		t.events <- event
	}
}

// sendItemsSaved 发送 Item 保存事件。
func (t *turnImpl) sendItemsSaved(saved []dao.LoggedItem) {
	// 1. 本批有 Item 时发送保存事件。
	if len(saved) > 0 {
		t.sendEvent(&TurnItemsSaved{Items: saved})
	}
}

// appendTurnInterrupted 保存本轮中断通知并发送保存事件。
func (t *turnImpl) appendTurnInterrupted(ctx context.Context) {
	// 1. 创建并追加本轮中断 Item。
	saved, err := t.appendItems(ctx,
		&defs.Item{
			ID:     defs.NewItemID(),
			TurnID: t.opt.TurnID,
			Kind:   defs.ItemTurnInterrupted,
		})
	// 2. 保存失败时记录日志，本轮留在 running；否则发送保存事件。
	if err != nil {
		telemetry.GetLogger(ctx).Warn("append turn interrupted item failed", "error", err)
		return
	}
	t.sendItemsSaved(saved)
}

// finishTurn 保存本轮失败或中断状态并返回结果。
func (t *turnImpl) finishTurn(
	ctx context.Context,
	items []*defs.Item,
	runErr error,
) (TurnResult, error) {
	// 1. 本轮已取消且不是只压缩时，先追加中断 Item；已有保存错误时本轮留在 running，返回 ErrTurnUnsettled。
	if ctx.Err() != nil && !t.opt.IsCompactOnly {
		t.appendTurnInterrupted(context.WithoutCancel(ctx))
	}
	if t.itemSaveErr != nil {
		return TurnResult{
			InputItemIDs: t.inputItemIDs,
			Outputs:      items,
		}, errors.Join(runErr, ErrTurnUnsettled, t.itemSaveErr)
	}
	// 2. 按取消状态填写 failed 或 interrupted，并记录日志末尾、错误和用量。
	t.record.Status = defs.TurnStatusFailed
	if ctx.Err() != nil {
		t.record.Status = defs.TurnStatusInterrupted
	}
	t.record.ItemsRange.LastOrdinal = t.lastSavedItemOrdinal
	t.record.Error = runErr.Error()
	t.record.Usage = t.agentStats.Usage
	result := TurnResult{
		InputItemIDs: t.inputItemIDs,
		Outputs:      items,
	}
	// 3. 不受取消影响地保存本轮记录，失败时返回 ErrTurnUnsettled。
	if err := t.opt.Mutation.SetTurn(context.WithoutCancel(ctx), t.record); err != nil {
		return result, errors.Join(runErr, ErrTurnUnsettled, err)
	}
	result.Status = t.record.Status
	result.ContextUsage = t.record.ContextUsage
	return result, runErr
}

// appendItems 追加本轮 Item，记下日志末尾，首批保存时写入起始流水号。
func (t *turnImpl) appendItems(ctx context.Context, items ...*defs.Item) ([]dao.LoggedItem, error) {
	// 1. 已有保存错误时直接返回这个错误，不再保存后续内容；空批次直接返回。
	if t.itemSaveErr != nil {
		return nil, t.itemSaveErr
	}
	if len(items) == 0 {
		return nil, nil
	}
	// 2. 追加 Item，保存失败时记录错误并返回。
	saved, err := t.opt.Mutation.AppendItems(ctx, t.opt.ConversationID, t.opt.SessionID, items...)
	if err != nil {
		t.itemSaveErr = err
		return nil, err
	}
	// 3. 更新日志末尾；首批 Item 成功后保存起始流水号，失败时记录错误。
	t.lastSavedItemOrdinal = uint64(saved[len(saved)-1].Ordinal)
	if t.record.ItemsRange.FirstOrdinal == 0 {
		t.record.ItemsRange.FirstOrdinal = uint64(saved[0].Ordinal)
		if err := t.opt.Mutation.SetTurn(ctx, t.record); err != nil {
			t.itemSaveErr = fmt.Errorf("save turn item start: %w", err)
			return nil, t.itemSaveErr
		}
	}
	return saved, nil
}
