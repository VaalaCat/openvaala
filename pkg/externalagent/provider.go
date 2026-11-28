// Package externalagent 将外部 Agent 的进程和历史接入 Session。
package externalagent

import (
	"context"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/workspace"
)

// ExternalAgentProvider 探测和访问同一外部 Agent 定义下的会话。
type ExternalAgentProvider interface {
	// GetCachedOptions 只读指定位置未带 selections 的探测结果。原生 Session 无对应，模型目录由 Conversation 提供。
	GetCachedOptions(executorID, cwd string) (options defs.ExternalOptions, found bool)
	// GetOptions 通过临时会话探测联动选项，按工作位置和 selections 缓存。原生 Session 无对应的后端探测。
	GetOptions(ctx context.Context, optionsReq ExternalAgentOptionsRequest) (options defs.ExternalOptions, err error)
	// ListSessions 分页列举可导入的会话，带 query 时匹配标题和目录，最多扫描 500 条。原生 Session 无对应，列表由 DAO 提供。
	ListSessions(ctx context.Context, ws workspace.Workspace, cwd string, query string, cursor string) (sessions []ExternalSessionInfo, nextCursor string, err error)
	// PreviewSession 临时回放选中的会话以取得首末用户消息。原生 Session 无对应，历史直接从 DAO 读取。
	PreviewSession(ctx context.Context, ws workspace.Workspace, binding defs.ExternalSession) (preview ExternalSessionPreview, err error)
	// OpenSession 与原生 NewSession 一样只构造实例。Run 根据绑定创建或恢复，恢复时忽略 InitialOptions。
	OpenSession(openReq ExternalSessionOpenRequest) (externalSession ExternalAgentSession)
}

// ExternalAgentSession 持有一个外部会话的协议连接和进程，事件按发生顺序移交给包装层。
type ExternalAgentSession interface {
	// Run 与原生 Run 一样阻塞到生命周期结束，负责进程、握手、回放与通知。返回前等全部发送者退出，events 由包装层持有。返回原始错误原因，包装层按恢复或进程退出场景补上说明。
	Run(ctx context.Context, events chan<- ExternalSessionEvent) (err error)
	// StartTurn 按 PrepareExternalPrompt 排好的内容顺序发出整批输入，发送后即返回，结束另发事件。原生 Session 无对应，HandleInput 只确认入队。
	StartTurn(ctx context.Context, turnID defs.TurnID, inputItems []*defs.Item) (err error)
	// SteerInput 与原生同名方法都给活动轮补充输入，驱动接收整组内容。
	SteerInput(ctx context.Context, turnID defs.TurnID, inputItems []*defs.Item) (err error)
	// InterruptTurn 与原生同名方法都请求取消并由终态事件确认，TurnID 由包装层持有。
	InterruptTurn(ctx context.Context) (err error)
	// SetOption 与原生 SelectModel 都修改后续轮的选择，外部驱动还可选择模式和其他配置项。
	SetOption(ctx context.Context, selection defs.ExternalOptionSelection) (err error)
}
