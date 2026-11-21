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
	"github.com/VaalaCat/openvaala/pkg/storage"
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
	// TakeSteer 借出挂在 turnID 上的补充输入；finish 为 true 且没取到输入时，停止向这个 Turn 投递。
	TakeSteer(ctx context.Context, turnID defs.TurnID, finish bool) ([][]*defs.Item, error)
	// ConfirmInputHandoff 在输入组及其 hook 记录写成后清除对应待办。
	ConfirmInputHandoff(ctx context.Context, turnID defs.TurnID, inputItemIDs []defs.ItemID) error
}

// turnImpl 保存本轮记录、输入 ID、Agent 输出的保存进度和统计。
type turnImpl struct {
	opt *TurnOpt
	// record 是本轮 Turn 记录，状态变化时写库。
	record defs.Turn
	// lastSavedItemOrdinal 是本轮最后保存的 Item 流水号。
	lastSavedItemOrdinal uint64
	// savedItemIDs 是已加载的历史和本轮成功追加的 Item ID，供确认 checkpoint 的前置 Item。
	savedItemIDs map[defs.ItemID]struct{}
	// itemSaveErr 保存追加 Item 或记录首批范围时的错误；出错后不再保存后续 Item，本轮留在 running，由 Session 恢复。
	itemSaveErr error
	// inputHandoffErr 保存输入组已经写入 Item 后确认待办失败的错误；这类错误不能被 Agent 的取消错误覆盖。
	inputHandoffErr error

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
	// pendingModelOutputIndex 是本次调用最后出现的文字或工具草稿位置；没有候选输出时为 -1。
	pendingModelOutputIndex int
	// releasedToolCallID 是本进程最近确认放行的工具批；已确认后续请求或尚未放行时为空。
	releasedToolCallID defs.ModelCallID
	// interruptionRecorded 表示本轮已有保存成功的中断 Item。
	interruptionRecorded bool
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
	// CompactModel 是新压缩使用的模型；只恢复已确认的压缩且没有新压缩模型时可为空。
	CompactModel *defs.ModelDefinition
	// PreviousModel 是上一个 Turn 使用的模型；零值表示还没有。
	PreviousModel defs.ModelRef
	// PreviousContextUsage 是最近一次完整普通调用的报告；上一轮没有完整普通调用时沿用更早的报告，没有时为 nil。
	PreviousContextUsage *defs.TurnContextUsage
	// PreviousExecutors 是上一个 Turn 开始时的 Executor 列表；为空表示还没有上一轮。
	PreviousExecutors []defs.TurnExecutor
	// IsCompactOnly 这个 Turn 是否只压缩上下文？是的话 Run 的 input 为空，不写输入和模型、Executor 变化 Item，
	// 只让 Agent 把历史压成摘要；恢复已有 running 记录时以原记录的模式为准。
	IsCompactOnly bool
	// RunningReply 在 running 记录写成后收到确认；nil 表示调用方不等待这条确认。
	RunningReply chan<- error
	// Hooks 在写输入前触发 SessionStart、SubagentStart 和 UserPromptSubmit，也交给 Agent 触发工具与 Stop 事件；
	// nil 或 Environments 为空表示不触发 hook。
	Hooks hook.Hooks
	// Session 是本 Turn 所属 Session 的记录，随 hook 事件交给脚本；ParentSessionID 非空表示子 Session。
	Session defs.Session
	// MaxSteps 是本轮最多调用模型的次数，交给 Agent；0 表示不限。
	MaxSteps int
	// HookStartSource 只有 Session 启动后的第一个非压缩 Turn 非空：startup 或 resume。
	// 非空时 Turn 在写输入前触发 SessionStart（子 Session 触发 SubagentStart，不带来源）；为空则不触发。
	HookStartSource string
	// ContextItems 是 Session 交来的注入 Item，本轮开始时补上 TurnID，写在模型、Executor 变化 Item 之后、本轮输入之前；
	// 只压缩的 Turn 为空。
	ContextItems []*defs.Item
	// Resume 是 Session 交来的原轮恢复资料；没有恢复资料时为空。
	Resume *TurnResume
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

// rebuildAgentResumeInput 根据原 Turn 的进度和 Item 记录重建执行输入和工具结果。
func (t *turnImpl) rebuildAgentResumeInput(ctx context.Context, checkpoint *defs.TurnCheckpoint, items []*defs.Item) (*agent.AgentResumeInput, error) {
	// 1. 按进度选择原模型请求或已放行工具批次。
	if checkpoint == nil || checkpoint.TurnID != t.opt.TurnID {
		return nil, errors.Join(ErrTurnUnsettled, errors.New("corrupt resume checkpoint: expected the original Turn ID"))
	}
	var modelCallID defs.ModelCallID
	var isCompaction bool
	switch checkpoint.Phase {
	case defs.TurnCheckpointPhaseRequest:
		request := checkpoint.Request
		if request == nil || request.ModelCallID == "" {
			return nil, errors.New("resume request has no model call ID")
		}
		isCompaction = request.Purpose == defs.ModelCallPurposeCompaction
		if isCompaction {
			if request.Compaction == nil || request.Compaction.SummaryItemID == "" || request.Compaction.CoveredThroughItemID == "" {
				return nil, errors.New("resume request has an invalid compaction range")
			}
		} else if request.Purpose != defs.ModelCallPurposeConversation || request.ModelCallThroughItemID == "" || t.record.IsCompactOnly {
			return nil, errors.New("resume request has an invalid conversation range")
		}
		modelCallID = request.ModelCallID
	case defs.TurnCheckpointPhaseTools:
		if t.record.IsCompactOnly || checkpoint.Tools == nil || checkpoint.Tools.ModelCallID == "" || checkpoint.Request != nil {
			return nil, errors.Join(ErrTurnUnsettled, errors.New("corrupt released tool batch checkpoint"))
		}
		modelCallID = checkpoint.Tools.ModelCallID
	default:
		return nil, fmt.Errorf("unsupported resume checkpoint phase %q", checkpoint.Phase)
	}
	resume := &agent.AgentResumeInput{Step: checkpoint.Step, StopHookContinuations: checkpoint.StopHookContinuations, IsContinuedByStopHook: checkpoint.IsContinuedByStopHook, Usage: t.record.Usage}
	// 2. 保留本轮已落盘的输出，按原调用报告恢复上下文和统计。
	var hasError, hasOutput, hasInput, isToolHookStopped bool
	existingToolResults := make(map[*defs.Item]struct{})
	var result defs.ModelCallResult
	var lastMessage string
	reported := make(map[defs.ModelCallID]bool)
	for _, item := range items {
		if item.ModelCallID != "" && item.Kind != defs.ItemModelCallResult || item.Kind == defs.ItemAgentsInstruction || item.Kind == defs.ItemMemoryIndex {
			resume.ExistingItems = append(resume.ExistingItems, item)
		}
		if report := item.ModelCallUsage; report != nil && !reported[report.ModelCallID] {
			reported[report.ModelCallID] = true
			resume.Usage = resume.Usage.Add(report.Usage)
			t.record.ContextUsage = report
		}
		if item.HandedOffInputItemID != "" && result != "" && (item.HookRun == nil || item.HookRun.BlockedInputItemID == "") {
			hasInput = true
		}
		if isCompaction || item.ModelCallID != modelCallID {
			continue
		}
		hasOutput = true
		if item.Kind == defs.ItemModelError {
			hasError = true
		}
		if item.Kind == defs.ItemToolResult {
			existingToolResults[item] = struct{}{}
		}
		if run := item.HookRun; run != nil && run.Status == defs.HookRunStopped && (run.Event == defs.HookEventPreToolUse || run.Event == defs.HookEventPostToolUse) {
			isToolHookStopped = true
		}
		if item.Message != nil {
			lastMessage = ""
			for _, block := range item.Message.Content {
				if block.Text != nil {
					lastMessage += block.Text.Text
				}
			}
		}
		if item.ModelCallResult != "" {
			if result != "" {
				return nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt model call %q: multiple completion facts", modelCallID))
			}
			result = item.ModelCallResult
		}
	}
	if isCompaction {
		return resume, nil
	}
	// 3. 已放行批次补缺后交给 Agent 结算，保留已有 Item 的日志顺序。
	if checkpoint.Phase == defs.TurnCheckpointPhaseTools {
		settled, err := t.settleReleasedToolBatch(ctx, modelCallID, items)
		if err != nil {
			return nil, err
		}
		resume.ToolBatch = &agent.AgentResumeToolBatch{ModelCallID: modelCallID, IsTurnStopped: isToolHookStopped}
		for _, item := range settled {
			resume.ToolBatch.Results = append(resume.ToolBatch.Results, item.ToolResult)
			if toolErr := item.ToolResult.Error; toolErr != nil && toolErr.Code == defs.ToolErrorHookStopped {
				resume.ToolBatch.IsTurnStopped = true
			}
			if _, exists := existingToolResults[item]; !exists {
				resume.ExistingItems = append(resume.ExistingItems, item)
			}
		}
		return resume, nil
	}
	// 4. 普通请求没有拒绝标记时不能出现工具结果；按完成事实收尾、重试或补拒绝。
	if len(existingToolResults) != 0 && result != defs.ModelCallResultToolsRejected {
		return nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt model call %q: tool results without a release checkpoint or rejection fact", modelCallID))
	}
	switch result {
	case "":
		resume.Retry = &agent.AgentResumeRetry{InputThroughItemID: checkpoint.Request.ModelCallThroughItemID, NeedsModelError: hasOutput && !hasError}
	case defs.ModelCallResultReply:
		if hasError {
			return nil, fmt.Errorf("model call %q has both an error and a completion fact", modelCallID)
		}
		resume.Finish = &agent.AgentResumeFinish{ModelCallID: modelCallID, LastAssistantMessage: lastMessage, HasInput: hasInput}
	case defs.ModelCallResultToolsRejected:
		missing, err := t.settleRejectedToolBatch(ctx, modelCallID, items)
		if err != nil {
			return nil, err
		}
		resume.ExistingItems = append(resume.ExistingItems, missing...)
		return resume, defs.ErrMaxStepsToolRequested
	default:
		return nil, fmt.Errorf("model call %q ended with unsupported result %q", modelCallID, result)
	}
	return resume, nil
}

// rebuildCompactionResume 核对已保存的摘要，或按原覆盖末项重建重试输入。
func (t *turnImpl) rebuildCompactionResume(compaction *defs.TurnCompactionCheckpoint, history []*defs.Item) (*agent.AgentResumeCompaction, error) {
	// 1. 按原 ID 查找摘要，核对所属 Turn 和覆盖末项。
	resume := &agent.AgentResumeCompaction{SummaryItemID: compaction.SummaryItemID}
	for _, row := range t.opt.Resume.Items {
		item := row.Item
		if item.ID != compaction.SummaryItemID {
			continue
		}
		if item.TurnID != t.record.ID || item.Kind != defs.ItemContextSummary || item.ValidateContextSummary() != nil ||
			item.ContextSummary.CoveredThroughItemID != compaction.CoveredThroughItemID || len(history) == 0 || history[0].ID != item.ID {
			return nil, fmt.Errorf("saved compaction summary %q does not match its checkpoint", item.ID)
		}
		return resume, nil
	}
	// 2. 正文未保存时，历史开头仍是上一份生效的摘要；按逻辑顺序截到原覆盖末项。
	if len(history) != 0 && history[0].ID == compaction.SummaryItemID {
		return nil, fmt.Errorf("saved compaction summary %q is missing from the original Turn items", compaction.SummaryItemID)
	}
	end := slices.IndexFunc(history, func(item *defs.Item) bool {
		return item.ID == compaction.CoveredThroughItemID && item.Kind != defs.ItemContextSummary
	})
	if end < 0 {
		return nil, fmt.Errorf("missing compaction input item %q for summary %q", compaction.CoveredThroughItemID, compaction.SummaryItemID)
	}
	// 3. 只接受原请求记录的实际模型及提供方。
	model := t.opt.Resume.CompactModel
	if model == nil || model.Target == nil || model.Ref != compaction.CompactModelRef {
		return nil, fmt.Errorf("Turn %q requires missing compaction model %s", t.record.ID, compaction.CompactModelRef)
	}
	resume.Source = history[:end+1]
	resume.Model = model
	return resume, nil
}

// settleReleasedToolBatch 核对已放行的工具批，补写未知结果并按调用顺序返回已保存的结果。
func (t *turnImpl) settleReleasedToolBatch(ctx context.Context, modelCallID defs.ModelCallID, items []*defs.Item) ([]*defs.Item, error) {
	// 1. 配对原批次的工具调用和已有结果。
	callIDs, results, err := t.getToolBatchResults(modelCallID, items, "")
	if err != nil {
		return nil, err
	}
	// 2. 按调用顺序为缺结果的调用追加未知结果。
	var missing []*defs.Item
	for index, callID := range callIDs {
		if results[index] != nil {
			continue
		}
		item := &defs.Item{
			ID: defs.NewItemID(), TurnID: t.opt.TurnID, ModelCallID: modelCallID,
			Kind: defs.ItemToolResult, ToolResult: defs.NewUnknownToolResult(callID),
		}
		results[index] = item
		missing = append(missing, item)
	}
	saved, err := t.appendItems(ctx, missing...)
	if err != nil {
		return nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("append unknown tool results: %w", err))
	}
	t.sendItemsSaved(saved)
	return results, nil
}

// settleRejectedToolBatch 核对最后一步的拒绝批，并返回补写的拒绝结果。
func (t *turnImpl) settleRejectedToolBatch(ctx context.Context, modelCallID defs.ModelCallID, items []*defs.Item) ([]*defs.Item, error) {
	// 1. 核对调用与已有拒绝结果，只为缺项创建新结果。
	callIDs, results, err := t.getToolBatchResults(modelCallID, items, defs.ModelCallResultToolsRejected)
	if err != nil {
		return nil, err
	}
	var missing []*defs.Item
	for index, callID := range callIDs {
		if results[index] == nil {
			missing = append(missing, &defs.Item{
				ID: defs.NewItemID(), TurnID: t.opt.TurnID, ModelCallID: modelCallID,
				Kind: defs.ItemToolResult, ToolResult: defs.NewRejectedToolResult(callID),
			})
		}
	}
	// 2. 保存并发布缺失的拒绝结果。
	saved, err := t.appendItems(ctx, missing...)
	if err != nil {
		return nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("append rejected tool results: %w", err))
	}
	t.sendItemsSaved(saved)
	return missing, nil
}

// getToolBatchResults 按原调用次序配对已存结果，缺结果的位置为 nil。
func (t *turnImpl) getToolBatchResults(modelCallID defs.ModelCallID, items []*defs.Item, wantCompletion defs.ModelCallResult) ([]string, []*defs.Item, error) {
	// 1. 检查同轮同批的完成事实、调用和结果；结果必须在对应调用后出现。
	var callIDs []string
	var results []*defs.Item
	var completion defs.ModelCallResult
	callIndexes := make(map[string]int)
	for _, item := range items {
		if item.TurnID != t.opt.TurnID || item.ModelCallID != modelCallID {
			continue
		}
		if item.Kind == defs.ItemModelError || item.ModelCallResult != "" && (item.ModelCallResult != wantCompletion || completion != "") {
			return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: conflicting model completion facts", modelCallID))
		}
		if item.Kind == defs.ItemToolCall && completion != "" {
			return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: tool call after rejection fact", modelCallID))
		}
		if item.Kind == defs.ItemToolResult && completion != wantCompletion {
			return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: tool result before rejection fact", modelCallID))
		}
		if item.ModelCallResult != "" {
			completion = item.ModelCallResult
		}
		switch item.Kind {
		case defs.ItemToolCall:
			call := item.ToolCall
			if call == nil || call.CallID == "" || call.Status != defs.ToolCallReady {
				return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: incomplete call item %q", modelCallID, item.ID))
			}
			if _, exists := callIndexes[call.CallID]; exists {
				return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: duplicate call ID %q", modelCallID, call.CallID))
			}
			callIndexes[call.CallID] = len(callIDs)
			callIDs = append(callIDs, call.CallID)
			results = append(results, nil)
		case defs.ItemToolResult:
			result := item.ToolResult
			if result == nil {
				return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: missing result in item %q", modelCallID, item.ID))
			}
			index, exists := callIndexes[result.CallID]
			if !exists || results[index] != nil {
				return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: unmatched or duplicate result for call ID %q", modelCallID, result.CallID))
			}
			if wantCompletion == defs.ModelCallResultToolsRejected && (result.Error == nil || result.Error.Code != defs.ToolErrorMaxStepsReached) {
				return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: conflicting rejection result for call ID %q", modelCallID, result.CallID))
			}
			results[index] = item
		}
	}
	// 2. 批次没有调用或缺少要求的完成事实时报损坏。
	if len(callIDs) == 0 || completion != wantCompletion {
		return nil, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("corrupt tool batch %q: missing tool calls or completion fact", modelCallID))
	}
	return callIDs, results, nil
}

// GetLoggedTurnItems 从 Session 日志尾向前读取最近 Turn 已写入的 Item，按写入顺序返回。
func GetLoggedTurnItems(ctx context.Context, query dao.Query, record defs.Turn) ([]dao.LoggedItem, error) {
	// 1. 遇到上一轮即停止翻页。
	request := dao.ListItemsRequest{ConversationID: record.ConversationID, SessionID: record.SessionID, Order: storage.DirectionDesc, Limit: 256}
	var rows []dao.LoggedItem
pages:
	for {
		page, err := query.ListItems(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("read Turn %q items: %w", record.ID, err)
		}
		for _, row := range page.Items {
			if row.Item.TurnID != record.ID {
				break pages
			}
			rows = append(rows, row)
		}
		if page.Next == 0 {
			break
		}
		request.Cursor = page.Next
	}
	// 2. 核对已声明的起始位置。
	slices.Reverse(rows)
	if first := record.ItemsRange.FirstOrdinal; first != 0 && (len(rows) == 0 || uint64(rows[0].Ordinal) != first) {
		return nil, fmt.Errorf("Turn %q item start %d does not match the log", record.ID, first)
	}
	return rows, nil
}

// NewTurn 检查配置并创建本轮 Turn。
func NewTurn(opt *TurnOpt) (Turn, error) {
	// 1. 检查本轮身份、Agent、模型、存储接口、客户端和输入来源。
	hasRunningRecord := opt != nil && opt.Resume != nil && opt.Resume.Record != nil
	if opt == nil || opt.TurnID == "" || opt.ConversationID == "" || opt.SessionID == "" || opt.Agent == nil || (!hasRunningRecord && (opt.Model == nil || opt.CompactModel == nil)) ||
		opt.Query == nil || opt.Mutation == nil || opt.Client == nil || opt.Inbox == nil {
		return nil, errors.New("turn requires a turn ID, conversation ID, session ID, agent definition, model, compact model, DAO query, DAO mutation, model client and inbox")
	}
	// 2. 恢复时沿用原 Turn 保存的压缩模式，再保存配置并创建已结束输出暂存表。
	if hasRunningRecord {
		resumeOpt := *opt
		resumeOpt.IsCompactOnly = opt.Resume.Record.IsCompactOnly
		opt = &resumeOpt
	}
	return &turnImpl{
		opt:                     opt,
		resolvedAgentItems:      make(map[int]*defs.Item),
		savedItemIDs:            make(map[defs.ItemID]struct{}),
		pendingModelOutputIndex: -1,
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
	if !t.opt.IsCompactOnly && len(turnInput) > 0 {
		ctx = telemetry.ContextWithAttrs(ctx, telemetry.InputItemID(string(turnInput[0].ID)))
	}
	// 2. 开始记录本轮 Agent 调用的遥测，记录内容时写入输入，再执行 Turn。
	ctx, recorder := telemetry.StartInvokeAgent(ctx, telemetry.InvokeAgentStart{
		AgentName:        t.opt.Agent.Name,
		AgentDescription: t.opt.Agent.Description,
		Model:            getTurnModelName(t.opt.Model),
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
	// 1. 登记归属本轮的开场输入，创建不随取消结束的写库上下文。
	t.events = events
	persistCtx := context.WithoutCancel(ctx)
	if !t.opt.IsCompactOnly && len(turnInput) > 0 {
		t.inputItemIDs = []defs.ItemID{turnInput[0].ID}
	}

	// 2. 记录本轮模型和 Executor，恢复已有 Turn 时沿用原记录，不重复写 running。
	turnExecutors := executor.GetTurnExecutors(t.opt.Environments)
	var turnItems []*defs.Item
	model := t.opt.Model
	previousContextUsage := t.opt.PreviousContextUsage
	var turnResume TurnResume
	if t.opt.Resume != nil {
		turnResume = *t.opt.Resume
	}
	if turnResume.Record != nil {
		t.record = *turnResume.Record
		if t.record.ID != t.opt.TurnID || t.record.SessionID != t.opt.SessionID || t.record.ConversationID != t.opt.ConversationID || t.record.Status != defs.TurnStatusRunning {
			return TurnResult{}, errors.New("expected matching running Turn record")
		}
		if model != nil && (model.Ref != t.record.Model.Ref || model.Target == nil) {
			model = nil
		}
		t.lastSavedItemOrdinal = t.record.ItemsRange.LastOrdinal
		for _, row := range turnResume.Items {
			item := row.Item
			if item == nil {
				return TurnResult{}, errors.New("resume log contains nil item")
			}
			if item.TurnID != t.opt.TurnID {
				continue
			}
			turnItems = append(turnItems, item)
			if item.Kind == defs.ItemTurnInterrupted {
				t.interruptionRecorded = true
			}
			t.savedItemIDs[item.ID] = struct{}{}
			if t.record.ItemsRange.FirstOrdinal == 0 {
				t.record.ItemsRange.FirstOrdinal = uint64(row.Ordinal)
			}
			t.lastSavedItemOrdinal = max(t.lastSavedItemOrdinal, uint64(row.Ordinal))
			if item.HandedOffInputItemID != "" {
				t.inputItemIDs = append(t.inputItemIDs, item.HandedOffInputItemID)
			}
		}
	} else {
		if previousContextUsage != nil && t.opt.PreviousModel != (defs.ModelRef{}) && t.opt.PreviousModel != t.opt.Model.Ref {
			telemetry.GetLogger(ctx).Warn("context usage restore skipped", "reason", "model_changed", "model_call_id", previousContextUsage.ModelCallID)
			previousContextUsage = nil
		}
		t.record = defs.Turn{
			ID:             t.opt.TurnID,
			ConversationID: t.opt.ConversationID,
			SessionID:      t.opt.SessionID,
			Status:         defs.TurnStatusRunning,
			IsCompactOnly:  t.opt.IsCompactOnly,
			ContextUsage:   previousContextUsage,
			Model: defs.ModelSelection{
				Ref:             t.opt.Model.Ref,
				ReasoningEffort: t.opt.ReasoningEffort,
			},
			Executors: turnExecutors,
		}
		if err := t.opt.Mutation.SetTurn(persistCtx, t.record); err != nil {
			if t.opt.RunningReply != nil {
				t.opt.RunningReply <- err
			}
			return TurnResult{InputItemIDs: t.inputItemIDs}, errors.Join(ErrTurnUnsettled, fmt.Errorf("create running turn: %w", err))
		}
	}
	if t.opt.RunningReply != nil {
		t.opt.RunningReply <- nil
	}
	// 3. 只压缩的 Turn 跳过输入和输入 hook；其他 Turn 保存输入，输入被拦下或 hook 要求结束时直接记为 completed。
	checkpoint := turnResume.Checkpoint
	stoppedByUser := errors.Is(context.Cause(ctx), ErrTurnInterrupted) || turnResume.StopRequested
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrTurnInterrupted) {
		return t.finishTurn(ctx, nil, ctx.Err())
	}
	if stoppedByUser && (checkpoint == nil || checkpoint.Phase != defs.TurnCheckpointPhaseTools) {
		return t.finishTurn(ctx, nil, context.Canceled)
	}
	if turnResume.Record == nil && !t.opt.IsCompactOnly && len(turnInput) == 0 {
		return TurnResult{}, errors.Join(ErrTurnUnsettled, fmt.Errorf("Turn %q has no opening input", t.record.ID))
	}
	if !t.opt.IsCompactOnly && len(turnInput) > 0 {
		if checkpoint != nil {
			return TurnResult{}, errors.Join(ErrTurnUnsettled, fmt.Errorf("Turn %q has confirmed a request before its opening input", t.record.ID))
		}
		isAgentSkipped, err := t.appendTurnInput(ctx, turnExecutors, turnInput)
		if err != nil {
			return t.finishTurn(ctx, nil, err)
		}
		if isAgentSkipped {
			return t.completeTurn(ctx, nil, "")
		}
	}

	// 4. 加载本轮模型历史，并在恢复时先确定同一步重试或收尾分支。
	var resumeInput *agent.AgentResumeInput
	var items []*defs.Item
	if checkpoint == nil && turnResume.Record != nil {
		if t.record.IsCompactOnly && len(turnItems) > 0 {
			return TurnResult{}, errors.Join(ErrTurnUnsettled, errors.New("compaction items without a checkpoint"))
		}
		if !t.record.IsCompactOnly && len(turnInput) == 0 {
			if len(t.inputItemIDs) == 0 {
				return TurnResult{}, errors.Join(ErrTurnUnsettled, fmt.Errorf("Turn %q has no opening input, handoff or checkpoint", t.record.ID))
			}
			// 首次请求前，只有开场的审核结果能结束整轮。
			for _, item := range turnItems {
				if run := item.HookRun; run != nil && (run.BlockedInputItemID == t.inputItemIDs[0] || run.Status == defs.HookRunStopped) {
					return t.completeTurn(ctx, nil, "")
				}
				if item.HandedOffInputItemID == t.inputItemIDs[0] {
					break
				}
			}
		}
		if !t.record.IsCompactOnly && model == nil {
			return t.finishTurn(ctx, nil, fmt.Errorf("Turn %q requires missing model %s", t.record.ID, t.record.Model.Ref))
		}
	}
	if turnResume.Record != nil && checkpoint != nil {
		var err error
		resumeInput, err = t.rebuildAgentResumeInput(persistCtx, checkpoint, turnItems)
		if resumeInput != nil {
			t.agentStats.Usage = resumeInput.Usage
		}
		if errors.Is(err, defs.ErrMaxStepsToolRequested) {
			return t.finishTurn(ctx, resumeInput.ExistingItems, err)
		}
		if err != nil {
			return t.finishTurn(ctx, nil, err)
		}
		var compaction *defs.TurnCompactionCheckpoint
		if request := checkpoint.Request; request != nil && request.Purpose == defs.ModelCallPurposeCompaction {
			compaction = request.Compaction
		}
		if ctx.Err() != nil || turnResume.StopRequested {
			return t.finishTurn(ctx, resumeInput.ExistingItems, context.Canceled)
		}
		if resumeInput.ToolBatch != nil {
			if model == nil && !resumeInput.ToolBatch.IsTurnStopped {
				return t.finishTurn(ctx, resumeInput.ExistingItems, fmt.Errorf("Turn %q requires missing model %s", t.record.ID, t.record.Model.Ref))
			}
		}
		if resumeInput.Retry != nil && resumeInput.Retry.NeedsModelError {
			request := checkpoint.Request
			modelError := &defs.Item{ID: defs.NewItemID(), TurnID: t.opt.TurnID, ModelCallID: request.ModelCallID, Kind: defs.ItemModelError, ModelError: &defs.ModelError{Code: "interrupted_model_call", Message: "model call did not complete before the process stopped"}}
			saved, err := t.appendItems(persistCtx, modelError)
			if err != nil {
				return t.finishTurn(ctx, nil, errors.Join(ErrTurnUnsettled, fmt.Errorf("append interrupted model error: %w", err)))
			}
			t.sendItemsSaved(saved)
			resumeInput.ExistingItems = append(resumeInput.ExistingItems, modelError)
		}
		if resumeInput.Retry != nil {
			if model == nil {
				return t.finishTurn(ctx, resumeInput.ExistingItems, fmt.Errorf("Turn %q requires missing model %s", t.record.ID, t.record.Model.Ref))
			}
			items, err = t.opt.Query.GetModelRequestHistory(ctx, t.opt.ConversationID, t.opt.SessionID, checkpoint)
		} else if model != nil || compaction != nil {
			items, err = dao.LoadContextHistory(ctx, t.opt.Query, t.opt.ConversationID, t.opt.SessionID, "")
		}
		if err != nil {
			return t.finishTurn(ctx, nil, fmt.Errorf("load resume context history: %w", err))
		}
		if compaction != nil {
			resumeInput.Compaction, err = t.rebuildCompactionResume(compaction, items)
			if err != nil {
				return t.finishTurn(ctx, resumeInput.ExistingItems, err)
			}
			if t.record.IsCompactOnly && resumeInput.Compaction.Source == nil {
				return t.completeTurn(ctx, resumeInput.ExistingItems, "")
			}
			if !t.record.IsCompactOnly && model == nil {
				return t.finishTurn(ctx, resumeInput.ExistingItems, fmt.Errorf("Turn %q requires missing model %s", t.record.ID, t.record.Model.Ref))
			}
		}
	} else {
		var err error
		items, err = dao.LoadContextHistory(ctx, t.opt.Query, t.opt.ConversationID, t.opt.SessionID, "")
		if err != nil {
			return t.finishTurn(ctx, nil, fmt.Errorf("load context history: %w", err))
		}
	}
	var agentContext agent_context.AgentContext
	contextModel := model
	if t.record.IsCompactOnly && contextModel == nil {
		contextModel = &defs.ModelDefinition{Ref: t.record.Model.Ref}
	}
	if contextModel != nil {
		contextOpt := agent_context.NewAgentContextOpt(t.opt.SessionID, t.opt.TurnID, t.opt.Client, contextModel, t.opt.CompactModel)
		contextOpt.ReasoningEffort = t.record.Model.ReasoningEffort
		var err error
		agentContext, err = agent_context.NewAgentContext(contextOpt)
		if err != nil {
			return t.finishTurn(ctx, nil, err)
		}
		if err := agentContext.Load(&agent_context.ContextLoadParam{System: t.opt.Agent.Instructions, Items: items, ContextUsage: t.record.ContextUsage}); err != nil {
			return t.finishTurn(ctx, nil, fmt.Errorf("load agent context: %w", err))
		}
	}
	// 5. 登记已落盘的历史 Item，再创建并执行同一个 Agent 主链。
	for _, item := range items {
		t.savedItemIDs[item.ID] = struct{}{}
	}
	if resumeInput != nil {
		t.nextPersistedItemIndex = len(resumeInput.ExistingItems)
	}
	requests := make(chan agent.AgentRequest)
	agentOpt := agent.NewAgentOpt(t.opt.TurnID, t.opt.SessionID, t.opt.Client, model, agentContext)
	agentOpt.ToolExecutor = t.opt.ToolExecutor
	agentOpt.Environments = t.opt.Environments
	agentOpt.Requests = requests
	agentOpt.IsCompactOnly = t.opt.IsCompactOnly
	agentOpt.Hooks = t.opt.Hooks
	agentOpt.Session = t.opt.Session
	agentOpt.MaxSteps = t.opt.MaxSteps
	agentOpt.Resume = resumeInput
	agentInstance, err := agent.NewAgent(agentOpt)
	if err != nil {
		return t.finishTurn(ctx, nil, err)
	}

	if err := ctx.Err(); err != nil {
		return t.finishTurn(ctx, nil, err)
	}
	return t.runAgent(ctx, agentInstance, requests)
}

// getTurnModelName 返回遥测使用的模型名；收尾恢复可能没有模型定义。
func getTurnModelName(model *defs.ModelDefinition) string {
	if model == nil || model.Target == nil {
		return ""
	}
	return model.Target.Model
}

// appendTurnInput 运行输入 hook 并保存本轮输入与配置变化，返回是否跳过 Agent。
func (t *turnImpl) appendTurnInput(ctx context.Context, turnExecutors []defs.TurnExecutor, turnInput []*defs.Item) (isAgentSkipped bool, err error) {
	// 1. 模型或 Executor 列表与上一轮不同时，依次创建变化 Item。
	var batch []*defs.Item
	hasRunningRecord := t.opt.Resume != nil && t.opt.Resume.Record != nil
	if !hasRunningRecord && t.opt.PreviousModel != (defs.ModelRef{}) && t.opt.PreviousModel != t.record.Model.Ref {
		batch = append(batch, &defs.Item{
			ID:     defs.NewItemID(),
			TurnID: t.opt.TurnID,
			Kind:   defs.ItemModelChange,
			ModelChange: &defs.ModelChange{
				PreviousModel: t.opt.PreviousModel,
				Model:         t.record.Model.Ref,
			},
		})
	}
	if !hasRunningRecord && len(t.opt.PreviousExecutors) > 0 && !slices.Equal(t.opt.PreviousExecutors, turnExecutors) {
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
	// 2. 追加 Session 交来的注入 Item，补上本轮 TurnID。
	for _, item := range t.opt.ContextItems {
		item.TurnID = t.opt.TurnID
		batch = append(batch, item)
	}
	// 3. HookStartSource 非空时运行 SessionStart 或 SubagentStart，追加保存的运行记录。
	if t.opt.HookStartSource != "" {
		startEvent := &hook.HookEvent{Name: defs.HookEventSessionStart, Source: t.opt.HookStartSource}
		if t.opt.Session.ParentSessionID != "" {
			startEvent = &hook.HookEvent{Name: defs.HookEventSubagentStart}
		}
		startOutcome := t.runHookEvent(ctx, startEvent)
		batch = append(batch, newHookRunItems(startOutcome.SavedRuns, t.opt.TurnID)...)
		isAgentSkipped = startOutcome.IsTurnStopped
	}
	// 4. 运行 UserPromptSubmit，按原顺序追加未被拦下的输入和 hook 记录，并在组末项写交接事实。
	promptOutcome := &hook.HookOutcome{}
	var promptHookItems []*defs.Item
	if isAgentSkipped {
		batch = append(batch, turnInput...)
	} else {
		promptOutcome = t.runUserPromptSubmitHook(ctx, turnInput)
		if !promptOutcome.IsBlocked {
			batch = append(batch, turnInput...)
		}
		promptHookItems = newHookRunItems(promptOutcome.SavedRuns, t.opt.TurnID)
		batch = append(batch, promptHookItems...)
		isAgentSkipped = promptOutcome.IsBlocked || promptOutcome.IsTurnStopped
	}
	markInputHandoff(turnInput, promptHookItems, promptOutcome, turnInput[0].ID)
	// 5. 保存整批内容，写成后确认该组已经交接。
	if err := ctx.Err(); err != nil {
		return false, err
	}
	saved, err := t.appendItems(context.WithoutCancel(ctx), batch...)
	if err != nil {
		return false, fmt.Errorf("append turn input: %w", err)
	}
	t.sendItemsSaved(saved)
	if err := t.opt.Inbox.ConfirmInputHandoff(context.WithoutCancel(ctx), t.opt.TurnID, []defs.ItemID{turnInput[0].ID}); err != nil {
		return false, errors.Join(ErrTurnUnsettled, fmt.Errorf("confirm turn input handoff: %w", err))
	}
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

// runAgent 运行 Agent，交接补充输入，保存输出并回复进度保存请求。
func (t *turnImpl) runAgent(
	ctx context.Context,
	agentInstance agent.Agent,
	requests <-chan agent.AgentRequest,
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

	// 2. 交接补充输入、接收进度请求，并按首次出现顺序保存 Agent 输出。
	var agentEvents <-chan *agent.AgentEvent = agentEventChannel
	runDone := runCtx.Done()
	var pendingCheckpoint *agent.AgentSaveCheckpointRequest
	var persistErr error
	for agentEvents != nil {
		select {
		case <-runDone:
			runDone = nil
		case request := <-requests:
			switch request := request.(type) {
			case agent.AgentInputRequest:
				t.handleAgentInputRequested(runCtx, request)
				if t.inputHandoffErr != nil || t.itemSaveErr != nil {
					cancel()
				}
			case agent.AgentSaveCheckpointRequest:
				pendingCheckpoint = &request
			}
		case event, open := <-agentEvents:
			if !open {
				agentEvents = nil
			} else if persistErr == nil && t.inputHandoffErr == nil && t.itemSaveErr == nil {
				persistErr = t.handleAgentEvent(persistCtx, event)
				if persistErr != nil {
					cancel()
				}
			}
		}
		// 3. 前置 Item 未写成时继续收事件；写成后追加进度，再回复 Agent。
		if pendingCheckpoint == nil {
			continue
		}
		err := persistErr
		if err == nil {
			err = runCtx.Err()
		}
		if err == nil {
			if pendingCheckpoint.AfterItemID != "" {
				if _, saved := t.savedItemIDs[pendingCheckpoint.AfterItemID]; !saved {
					continue
				}
			}
			err = t.opt.Mutation.AppendTurnCheckpoint(runCtx, t.opt.ConversationID, t.opt.SessionID, &pendingCheckpoint.Checkpoint)
			if err != nil {
				// 取消只在错误树的所有叶子都是当前 Run 的取消错误时走正常中断；混合错误保留完整持久化错误。
				if isOnlyContextCancellation(err, runCtx.Err()) {
					err = runCtx.Err()
				} else {
					persistErr = errors.Join(ErrTurnUnsettled, fmt.Errorf("append turn checkpoint: %w", err))
					cancel()
				}
			}
		}
		if err == nil {
			t.releasedToolCallID = ""
			if tools := pendingCheckpoint.Checkpoint.Tools; tools != nil {
				t.releasedToolCallID = tools.ModelCallID
			}
		}
		pendingCheckpoint.Reply <- err
		close(pendingCheckpoint.Reply)
		pendingCheckpoint = nil
	}
	// 4. 等待 Agent 退出并收集统计，保存失败时按 Turn 持有的错误结束本轮。
	tasks.Wait()
	t.agentStats = agentResult.Stats
	items := agentResult.Items
	if t.inputHandoffErr != nil {
		return t.finishTurn(ctx, items, t.inputHandoffErr)
	}
	if persistErr != nil {
		return t.finishTurn(ctx, items, persistErr)
	}

	// 5. 跳过未结束的位置并保存剩余输出。
	t.skipUnresolvedAgentItems(len(items))
	saved, err := t.persistResolvedAgentItems(persistCtx)
	if err != nil {
		return t.finishTurn(ctx, items, errors.Join(agentErr, err))
	}
	t.sendItemsSaved(saved)
	if agentErr != nil {
		return t.finishTurn(ctx, items, agentErr)
	}

	return t.completeTurn(ctx, items, agentResult.FinalModelCallID)
}

// isOnlyContextCancellation 检查错误树的所有叶子是否都是指定的上下文取消错误。
func isOnlyContextCancellation(err error, cancellation error) bool {
	// 1. 取消错误不存在时不能把任意存储错误当作正常取消。
	if err == nil || cancellation == nil {
		return false
	}
	// 2. 递归展开多错误和包装错误；每个叶子都必须匹配当前取消错误。
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		children := multi.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isOnlyContextCancellation(child, cancellation) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		if child != nil {
			return isOnlyContextCancellation(child, cancellation)
		}
	}
	return errors.Is(err, cancellation)
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
	if err := ctx.Err(); err != nil {
		return t.finishTurn(ctx, items, err)
	}
	// 2. 填写日志末尾、completed、最终调用和累计用量，保存本轮记录；保存失败时返回 ErrTurnUnsettled。
	if t.lastSavedItemOrdinal != 0 {
		t.record.ItemsRange.LastOrdinal = t.lastSavedItemOrdinal
	}
	t.record.Status = defs.TurnStatusCompleted
	t.record.FinalModelCallID = finalModelCallID
	t.record.Usage = t.agentStats.Usage
	if err := t.opt.Mutation.SetTurn(context.WithoutCancel(ctx), t.record); err != nil {
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
	// 2. 借出补充输入，逐组运行 UserPromptSubmit 并保留组边界。
	groups, err := t.opt.Inbox.TakeSteer(ctx, t.opt.TurnID, request.FinishIfEmpty)
	var items, batch []*defs.Item
	var handedOffInputIDs []defs.ItemID
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		for _, item := range group {
			item.TurnID = t.opt.TurnID
		}
		promptOutcome := t.runUserPromptSubmitHook(ctx, group)
		hookRunItems := newHookRunItems(promptOutcome.SavedRuns, t.opt.TurnID)
		if !promptOutcome.IsBlocked {
			items = append(items, group...)
			items = append(items, hookRunItems...)
			batch = append(batch, group...)
		}
		batch = append(batch, hookRunItems...)
		markInputHandoff(group, hookRunItems, promptOutcome, group[0].ID)
		handedOffInputIDs = append(handedOffInputIDs, group[0].ID)
	}
	// 3. 只有整批 Item 成功写入后才清除 Session 待办并回复 Agent。
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && len(batch) > 0 {
		t.inputItemIDs = append(t.inputItemIDs, handedOffInputIDs...)
		saved, appendErr := t.appendItems(context.WithoutCancel(ctx), batch...)
		if appendErr != nil {
			err = fmt.Errorf("append steer input: %w", appendErr)
		} else {
			t.sendItemsSaved(saved)
			if confirmErr := t.opt.Inbox.ConfirmInputHandoff(context.WithoutCancel(ctx), t.opt.TurnID, handedOffInputIDs); confirmErr != nil {
				err = errors.Join(ErrTurnUnsettled, fmt.Errorf("confirm steer input handoff: %w", confirmErr))
				t.inputHandoffErr = err
			}
		}
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
	// 1. 留住最后出现的候选文字或工具调用；其他已结束的位置可以按顺序保存。
	if event.Type == agent.AgentEventItemStarted && event.Item.ModelCallID != "" && (event.Item.Kind == defs.ItemMessage || event.Item.Kind == defs.ItemToolCall) {
		t.pendingModelOutputIndex = event.ItemIndex
	}
	if isAgentTerminalEvent(event) {
		t.resolvedAgentItems[event.ItemIndex] = event.Item
	}
	// 2. 普通模型流结束后，在候选输出上记录调用结果；空回复单独追加日志行。
	var emptyReply *defs.Item
	if call := event.ModelCall; event.Type == agent.AgentEventModelCallFinished && call.Purpose == defs.ModelCallPurposeConversation {
		if call.ContextUsage != nil {
			t.record.ContextUsage = call.ContextUsage
		}
		if item := t.resolvedAgentItems[t.pendingModelOutputIndex]; item != nil && call.ContextUsage != nil {
			completed := item.Clone()
			completed.ModelCallResult = call.Result
			completed.ModelCallUsage = call.ContextUsage
			t.resolvedAgentItems[t.pendingModelOutputIndex] = completed
		} else if call.Result != "" {
			emptyReply = &defs.Item{ID: defs.NewItemID(), TurnID: t.opt.TurnID, ModelCallID: call.ModelCallID,
				Kind: defs.ItemModelCallResult, ModelCallResult: call.Result, ModelCallUsage: call.ContextUsage}
		}
		t.pendingModelOutputIndex = -1
	}
	// 3. 发布块事件，保存已决输出并通知订阅者。
	t.sendEvent(&TurnAgentEvent{Event: event})
	saved, err := t.persistResolvedAgentItems(ctx)
	if err != nil {
		return err
	}
	t.sendItemsSaved(saved)
	if emptyReply != nil {
		saved, err = t.appendItems(ctx, emptyReply)
		if err != nil {
			return fmt.Errorf("append empty model reply: %w", err)
		}
		t.sendItemsSaved(saved)
	}
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
	for t.nextPersistedItemIndex != t.pendingModelOutputIndex {
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
	// 1. 已有中断记录时跳过，否则创建并追加本轮中断 Item。
	if t.interruptionRecorded {
		return
	}
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
	t.interruptionRecorded = true
	t.sendItemsSaved(saved)
}

// finishTurn 保存本轮失败或中断状态并返回结果。
func (t *turnImpl) finishTurn(
	ctx context.Context,
	items []*defs.Item,
	runErr error,
) (TurnResult, error) {
	// 1. 交接或进度保存失败时本轮留在 running，不追加中断 Item。
	if t.inputHandoffErr != nil {
		return TurnResult{InputItemIDs: t.inputItemIDs, Outputs: items}, t.inputHandoffErr
	}
	if errors.Is(runErr, ErrTurnUnsettled) {
		return TurnResult{InputItemIDs: t.inputItemIDs, Outputs: items}, runErr
	}
	// 2. 已取消或收到原轮停止请求时，先追加中断 Item；只压缩时不追加。
	stopRequested := t.opt.Resume != nil && t.opt.Resume.StopRequested
	isInterrupted := errors.Is(context.Cause(ctx), ErrTurnInterrupted) || stopRequested && errors.Is(runErr, context.Canceled)
	if t.itemSaveErr != nil {
		return TurnResult{InputItemIDs: t.inputItemIDs, Outputs: items}, errors.Join(runErr, ErrTurnUnsettled, t.itemSaveErr)
	}
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrTurnInterrupted) {
		return TurnResult{InputItemIDs: t.inputItemIDs, Outputs: items}, ctx.Err()
	}
	// 运行中的用户停止等工具退出后，只对已确认放行的批次补缺。
	if isInterrupted && t.releasedToolCallID != "" {
		persistCtx := context.WithoutCancel(ctx)
		rows, err := GetLoggedTurnItems(persistCtx, t.opt.Query, t.record)
		if err != nil {
			return TurnResult{}, errors.Join(ErrTurnUnsettled, err)
		}
		saved := make([]*defs.Item, len(rows))
		for i, row := range rows {
			saved[i] = row.Item
		}
		if _, err := t.settleReleasedToolBatch(persistCtx, t.releasedToolCallID, saved); err != nil {
			return TurnResult{}, err
		}
	}
	if isInterrupted && !t.opt.IsCompactOnly {
		t.appendTurnInterrupted(context.WithoutCancel(ctx))
	}
	if t.itemSaveErr != nil {
		return TurnResult{
			InputItemIDs: t.inputItemIDs,
			Outputs:      items,
		}, errors.Join(runErr, ErrTurnUnsettled, t.itemSaveErr)
	}
	// 3. 按取消状态填写 failed 或 interrupted，并记录日志末尾、错误和用量。
	t.record.Status = defs.TurnStatusFailed
	if isInterrupted {
		t.record.Status = defs.TurnStatusInterrupted
	}
	if t.lastSavedItemOrdinal != 0 {
		t.record.ItemsRange.LastOrdinal = t.lastSavedItemOrdinal
	}
	t.record.Error = runErr.Error()
	t.record.Usage = t.agentStats.Usage
	result := TurnResult{
		InputItemIDs: t.inputItemIDs,
		Outputs:      items,
	}
	// 4. 不受取消影响地保存本轮记录，失败时返回 ErrTurnUnsettled。
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
	for _, item := range saved {
		t.savedItemIDs[item.Item.ID] = struct{}{}
	}
	return saved, nil
}
