package storage

import (
	"context"
	"encoding/json"
)

// RecordKey 是 Record 在一个 namespace 里的 Key，按字节序排列。
type RecordKey string

// Record 是一条已保存的 JSON Record。
type Record struct {
	Key RecordKey
	// UpdatedAt 是最后一次写入的 Unix 毫秒，由写入方给出。
	UpdatedAt int64
	Payload   json.RawMessage
}

// RecordCursor 是上一页最后一条 Record 的位置；零值表示从起点开始。
// 按 Key 排序只用 Key；按 UpdatedAt 排序用 (UpdatedAt, Key)。按 UpdatedAt 倒序翻页期间被更新的 Record 会排到最前面，
// 之后的页里不再出现：还没读到的就跳过了，读过的也不会重复。
type RecordCursor struct {
	Key       RecordKey
	UpdatedAt int64
}

// RecordField 声明 Payload 内允许筛选的字符串字段；路径逐段表示 JSON 对象键，不解释通配符或数组下标。
type RecordField struct {
	Name            string
	PayloadJSONPath []string
}

// RecordNamespace 声明一组记录及可以筛选的字段。
type RecordNamespace struct {
	Name   string
	Fields []RecordField
	// IndexedFields 里每个字段都建立字段值、UpdatedAt、Key 的联合索引，字段必须已在 Fields 里声明。
	IndexedFields []string
}

// RecordFilterLogic 指定筛选组内各条件按 AND 还是 OR 组合。
type RecordFilterLogic string

const (
	RecordFilterAnd RecordFilterLogic = "and"
	RecordFilterOr  RecordFilterLogic = "or"
)

// RecordFilterOperator 指定字符串字段的匹配方式，均区分大小写。
type RecordFilterOperator string

const (
	RecordFilterIn       RecordFilterOperator = "in"
	RecordFilterContains RecordFilterOperator = "contains"
)

// RecordFilterCondition 筛选一个已声明字段；缺失、null 或非字符串值不匹配。
type RecordFilterCondition struct {
	Field    string
	Operator RecordFilterOperator
	// Values 在 in 时至少一项，命中任意一项即匹配；contains 时恰好一项，按普通子串匹配。
	Values []string
}

// RecordFilter 组合一组条件；空 Conditions 不筛选，Logic 零值按 AND，不支持嵌套分组。
type RecordFilter struct {
	Logic      RecordFilterLogic
	Conditions []RecordFilterCondition
}

// ListRecordsRequest 是在一个 namespace 里按 Key 范围、排序和游标分页读取的条件。
type ListRecordsRequest struct {
	Namespace string
	// Start 是 Key 范围下界（含），两种排序都生效；为空时没有下界。
	Start RecordKey
	// End 是 Key 范围上界（不含）；为空时没有上界。
	End    RecordKey
	Order  Order
	Cursor RecordCursor
	Limit  int
	// Filter 先筛选再排序分页；整组与 Key 范围及游标取 AND。
	Filter RecordFilter
}

// RecordStorage 保存按 namespace 和 Key 定位的 JSON Record。
type RecordStorage interface {
	// Set 原样保存 record 的 Key、UpdatedAt 和 Payload；UpdatedAt 由调用方给出，必须大于 0。
	Set(ctx context.Context, namespace string, record Record) error
	// MSet 批量写入同一 namespace，每条使用自己的 UpdatedAt；同一批里重复的 Key 以后一条为准；失败时可能已经写入部分记录。
	MSet(ctx context.Context, namespace string, records []Record) error
	// Get 读取指定记录，同时返回是否找到。
	Get(ctx context.Context, namespace string, key RecordKey) (Record, bool, error)
	// MGet 返回找到的 Record；不同 Key 的结果可能来自不同写入时刻。
	MGet(ctx context.Context, namespace string, keys []RecordKey) (map[RecordKey]Record, error)
	// Delete 删除指定记录，记录不存在时不报错。
	Delete(ctx context.Context, namespace string, key RecordKey) error
	// DeleteRange 删除 [start, end) 内的记录，end 为空时不设上界。
	DeleteRange(ctx context.Context, namespace string, start, end RecordKey) error
	// List 按 Order 分页；还有下一页时 Next 为本页最后一条的位置。
	List(ctx context.Context, request ListRecordsRequest) (Page[Record, RecordCursor], error)
}
