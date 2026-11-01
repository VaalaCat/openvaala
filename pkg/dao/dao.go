// Package dao 按对话、Session、Turn、Item 读写存储，读和写分成 Query、Mutation 两组接口。
package dao

import (
	"context"
	"errors"
	"iter"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/storage"
)

// Query 读取对话、Session、Turn 和 Item。
type Query interface {
	ConversationQuery
	SessionQuery
	TurnQuery
	ItemQuery
}

// Mutation 保存或删除对话、Session、Turn 和 Item。
type Mutation interface {
	ConversationMutation
	SessionMutation
	TurnMutation
	ItemMutation
	// DeleteConversation 先在 metadata 日志里记下删除，再删除对话的各级索引和全部日志；对话不存在时直接返回。
	DeleteConversation(ctx context.Context, conversationID defs.ConversationID) error
}

// SessionQuery 读取 Session 记录。
type SessionQuery interface {
	// GetSession 读取对话内指定 Session，返回记录及是否找到。
	GetSession(context.Context, defs.ConversationID, defs.SessionID) (defs.Session, bool, error)
	// GetSessionByID 只凭 Session ID 读取记录；Session 还没写过 Item 时返回未找到。
	GetSessionByID(ctx context.Context, sessionID defs.SessionID) (defs.Session, bool, error)
	// ListSessions 分页读取对话内的 Session。
	ListSessions(context.Context, ListSessionsRequest) (storage.Page[defs.Session, RecordCursor[defs.SessionID]], error)
	// ScanSessions 按排序逐条读取对话内全部 Session，调用方 break 后停止。
	ScanSessions(context.Context, defs.ConversationID, storage.Order) iter.Seq2[defs.Session, error]
}

// SessionMutation 保存和删除 Session。
type SessionMutation interface {
	// SetSession 保存 Session 的日志和索引。
	SetSession(context.Context, defs.Session) error
	// DeleteSession 先在 metadata 日志里记下删除，再删除 Session 的 Turn 索引、Item 日志和 Session 索引；Session 不存在时直接返回。
	DeleteSession(context.Context, defs.ConversationID, defs.SessionID) error
}

// TurnQuery 读取 Turn 记录。
type TurnQuery interface {
	// GetTurn 读取 Session 内指定 Turn，返回记录及是否找到。
	GetTurn(context.Context, defs.SessionID, defs.TurnID) (defs.Turn, bool, error)
	// GetTurns 按输入 ID 顺序返回 Session 中找到的 Turn，跳过不存在的记录。
	GetTurns(ctx context.Context, sessionID defs.SessionID, ids []defs.TurnID) ([]defs.Turn, error)
	// ListTurns 分页读取 Session 内的 Turn。
	ListTurns(context.Context, ListTurnsRequest) (storage.Page[defs.Turn, RecordCursor[defs.TurnID]], error)
}

// TurnMutation 保存 Turn 记录。
type TurnMutation interface {
	// SetTurn 保存 Turn 的日志和索引。
	SetTurn(context.Context, defs.Turn) error
}

// ItemQuery 读取 Session 的 Item 日志。
type ItemQuery interface {
	// ListItems 按对外序号分页读取 Session 日志。对外序号是调用方看到的 Item 序号：用户分叉的根 Session 前面接上
	// 来源对话的历史，序号从来源的第 1 条连续排下来，接续标记本身不返回；来源读不到时那一段当作空。
	ListItems(context.Context, ListItemsRequest) (storage.Page[LoggedItem, storage.Ordinal], error)
	// ScanItems 按方向逐条读取 Session 日志，调用方 break 后不再读后续页。
	ScanItems(context.Context, defs.ConversationID, defs.SessionID, storage.Direction) iter.Seq2[*defs.Item, error]
}

// ItemMutation 追加 Session 的 Item 日志。
type ItemMutation interface {
	// AppendItems 追加一批 Item，返回带对外序号和保存时间的副本；不修改传入的 Item。
	AppendItems(context.Context, defs.ConversationID, defs.SessionID, ...*defs.Item) ([]LoggedItem, error)
}

// daoImpl 把各实体的 DAO 组合成一个实例，同时实现 Query 和 Mutation。
type daoImpl struct {
	*conversationDAOImpl
	*sessionDAOImpl
	*turnDAOImpl
	*itemDAOImpl
}

var _ Query = (*daoImpl)(nil)
var _ Mutation = (*daoImpl)(nil)

// scanPageSize 是 Scan 系列方法内部每次翻页读取的条数。
const scanPageSize = 256

// RecordCursor 是对话、Session、Turn 列表的翻页位置；零值从头开始。
type RecordCursor[ID ~string] struct {
	// ID 是上一页最后一条记录的 ID；为空时从头开始。
	ID ID `json:"id"`
	// UpdatedAt 是上一页最后一条记录的更新时间；按 Key 排序时不用。
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// DAOOpt 是 DAO 用的记录存储和日志存储。
type DAOOpt struct {
	Records storage.RecordStorage
	Logs    storage.LogStorage
}

// NewDAOOpt 保存 DAO 使用的记录和日志存储。
func NewDAOOpt(records storage.RecordStorage, logs storage.LogStorage) *DAOOpt {
	// 1. 保存两个存储实例。
	return &DAOOpt{
		Records: records,
		Logs:    logs,
	}
}

// NewDAO 检查存储并创建 DAO，返回同一个实例的 Query 和 Mutation。
func NewDAO(opt *DAOOpt) (Query, Mutation, error) {
	// 1. 检查记录和日志存储是否齐全。
	if opt == nil || opt.Records == nil || opt.Logs == nil {
		return nil, nil, errors.New("DAO requires record and log storage")
	}
	// 2. 创建各实体的 DAO，组合成一个实例返回；底层存储由调用方关闭。
	impl := &daoImpl{
		conversationDAOImpl: &conversationDAOImpl{
			records: opt.Records,
			logs:    opt.Logs,
		},
		sessionDAOImpl: &sessionDAOImpl{
			records: opt.Records,
			logs:    opt.Logs,
		},
		turnDAOImpl: &turnDAOImpl{
			records: opt.Records,
			logs:    opt.Logs,
		},
		itemDAOImpl: &itemDAOImpl{logs: opt.Logs},
	}
	return impl, impl, nil
}

// RecordNamespaces 返回 DAO 使用的记录分组、筛选字段和索引。
func RecordNamespaces() []storage.RecordNamespace {
	// 1. 声明普通对话和归档对话的目录、标题字段及目录索引。
	namespaces := make([]storage.RecordNamespace, 0, 4)
	for _, name := range []string{ConversationNamespace, ArchivedConversationNamespace} {
		namespaces = append(namespaces, storage.RecordNamespace{
			Name: name,
			Fields: []storage.RecordField{
				{
					Name:            "directory",
					PayloadJSONPath: []string{"executor_settings", "workspace_directories", "local"},
				},
				{
					Name:            "title",
					PayloadJSONPath: []string{"title"},
				},
			},
			IndexedFields: []string{"directory"},
		})
	}
	// 2. 加入 Session 和 Turn 记录分组。
	return append(namespaces, storage.RecordNamespace{Name: SessionNamespace}, storage.RecordNamespace{Name: turnNamespace})
}
