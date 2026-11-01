package turn

import (
	"errors"

	"github.com/VaalaCat/openvaala/pkg/agent"
	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// ── Turn 执行错误 ──

// ErrTurnUnsettled 由 Turn 在本轮记录或 Item 没能保存时返回，本轮留在 running；Session 收到后停止，按已保存的记录恢复。
var ErrTurnUnsettled = errors.New("turn terminal status was not persisted")

// ── Turn 执行结果 ──

// TurnResult 是 Turn 一次 Run 的结果。
type TurnResult struct {
	// InputItemIDs 按读入顺序保存首批输入和每组补充输入的首个 Item ID；被 UserPromptSubmit 拦下、没有保存的输入也算。
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
