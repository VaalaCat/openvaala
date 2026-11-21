package turn

import (
	"errors"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// ── Turn 执行错误 ──

// ErrTurnInterrupted 是用户明确叫停活动轮时传给取消上下文的原因。
var ErrTurnInterrupted = errors.New("turn interrupted by caller")

// ErrTurnUnsettled 由 Turn 在本轮记录或 Item 没能保存时返回，本轮留在 running；Session 收到后停止，按已保存的记录恢复。
var ErrTurnUnsettled = errors.New("turn terminal status was not persisted")

// ── Turn 恢复入参 ──

// TurnResume 是 Session 已读取的原轮资料，交给 Turn 继续执行或收尾。
type TurnResume struct {
	// Record 是原轮的 running 记录；已分配输入但还没写 running 时为空。
	Record *defs.Turn
	// Checkpoint 是原轮最新的请求或工具批次进度；没有进度时为空。
	Checkpoint *defs.TurnCheckpoint
	// CompactModel 是按原摘要请求的完整模型引用找到的模型；没有原摘要请求或找不到模型时为空。
	CompactModel *defs.ModelDefinition
	// Items 是已读取的原轮 Item 及其日志流水号；原轮没有 Item 时为空。
	Items []dao.LoggedItem
	// StopRequested 表示 Session 已保存对原轮的停止请求；没有请求时为 false。
	StopRequested bool
}

// ── Turn 执行结果 ──

// TurnResult 是 Turn 一次 Run 的结果。
type TurnResult struct {
	// InputItemIDs 在终态中包含本轮的开场（含停止时丢弃的开场）及已交接补充的组首 ID，拦截记录也算交接。
	// 写入失败而未结算时也包含已尝试写入的补充；未结算结果不作为终态发布。
	InputItemIDs []defs.ItemID
	// Outputs 是 Agent 返回的全部草稿；Agent 没运行时为空。
	Outputs []*defs.Item
	// FinalModelCallID 是最后一次成功的普通模型调用，只在 completed 时填写。
	FinalModelCallID defs.ModelCallID
	// Status 是已保存的终态；终态没保存时为空。
	Status defs.TurnStatus
	// ContextUsage 是最近一次完整普通调用的报告；本轮没有完整普通调用时沿用上一轮的报告，模型切换后为 nil。
	ContextUsage *defs.TurnContextUsage
}

// ── Turn 对外事件 ──

// TurnEvent 由 Turn 在运行期间按发生顺序发送，包含 Agent 输出、Item 保存和 hook 运行记录。
type TurnEvent interface{ isTurnEvent() }

// TurnAgentEvent 由 Turn 在处理完 Agent 事件后发出；完成和中断 Item 按首次出现顺序保存，前面仍有草稿时暂存。
type TurnAgentEvent struct{ Event *agent.AgentEvent }

func (*TurnAgentEvent) isTurnEvent() {}

// TurnItemsSaved 由 Turn 在一批输入、hook 记录、Agent 输出或中断 Item 写入 Session 日志后发出。
// Items 按流水号递增；Agent 输出的保存事件排在对应完成或中断 TurnAgentEvent 之后。
type TurnItemsSaved struct{ Items []dao.LoggedItem }

func (*TurnItemsSaved) isTurnEvent() {}

// TurnHookRunEvent 由 Turn 在 SessionStart、SubagentStart、UserPromptSubmit 开始和结束时各发出一次，同次运行的 ID 不变。
// 需要保存的记录另外随输入写成 hook_run Item；Agent 触发的 hook 通过 TurnAgentEvent 发送。
type TurnHookRunEvent struct{ Run *defs.HookRun }

func (*TurnHookRunEvent) isTurnEvent() {}
