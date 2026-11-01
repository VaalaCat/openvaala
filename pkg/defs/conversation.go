package defs

import "slices"

// ExecutorSettings 是对话的 Executor 设置：默认用哪个、能用哪些、各自在哪个目录。
type ExecutorSettings struct {
	// DefaultExecutorID 是默认 Executor：模型省略 executor 参数时用它，项目 Skills、用户主目录 Skills 和 AGENTS.md 从它读；
	// 为空时是筛选后列表的第一个。
	DefaultExecutorID string `json:"default_executor_id,omitempty"`
	// ExecutorIDs 是对话可用的 Executor，只决定能不能用，不决定顺序；为空时全部可用，包括以后新加的。
	ExecutorIDs []string `json:"executor_ids,omitempty"`
	// WorkspaceDirectories 是各 Executor 在这个对话里的根目录，键为 Executor ID。local 总有一项；
	// 其他 Executor 只记 /cd 改过的，没有的用默认根目录（配置里的 workspace_directory）。
	WorkspaceDirectories map[string]string `json:"workspace_directories,omitempty"`
}

// IsExecutorAvailable 对话能用这个 Executor 吗？
func (s ExecutorSettings) IsExecutorAvailable(id string) bool {
	// 1. 可用列表为空时允许全部 Executor，否则按 ID 查找。
	return len(s.ExecutorIDs) == 0 || slices.Contains(s.ExecutorIDs, id)
}

// GetConversationDirectory 返回 serve 本机的对话目录。
func (s ExecutorSettings) GetConversationDirectory() string {
	// 1. 返回 local 的目录，不受 local 是否可用或默认 Executor 选择影响。
	return s.WorkspaceDirectories[LocalExecutorID]
}

// Conversation 是一个对话的记录；每个对话有唯一的根 Session。
type Conversation struct {
	ID ConversationID `json:"id"`
	// ExecutorSettings 是对话的 Executor 设置：新建时补上缺的 local 目录后保存，之后只由改设置修改；打开对话不写它。
	ExecutorSettings ExecutorSettings `json:"executor_settings"`
	// UpdatedAt 是最后一次写入的 Unix 毫秒，由存储在写入时分配；写入时设置的值不会被保存。
	// 创建、分叉、改标题、改 Executor 设置、归档或取消归档、更新消息数都会刷新这个时间。
	UpdatedAt     int64     `json:"updated_at,omitempty"`
	Title         string    `json:"title,omitempty"`
	RootSessionID SessionID `json:"root_session_id"`
	// MessageCount 是根 Session 中用户消息与主 Agent 回复的条数，每轮结束时累加。
	MessageCount int `json:"message_count,omitempty"`
	// ForkedFromID 是分叉来源的对话；不是分叉出来的为空。
	ForkedFromID ConversationID `json:"forked_from_id,omitempty"`
	// ArchivedAt 是归档时间（Unix 毫秒），0 表示没归档。
	ArchivedAt int64 `json:"archived_at,omitempty"`
}
