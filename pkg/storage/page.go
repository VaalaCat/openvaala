// Package storage 定义 JSON 记录和追加日志的读写接口。
package storage

import "fmt"

// Direction 是 Record 与 Log 读取的方向。
type Direction string

const (
	// DirectionAsc 按排序依据从小到大读取。
	DirectionAsc Direction = "asc"
	// DirectionDesc 按排序依据从大到小读取。
	DirectionDesc Direction = "desc"
)

// WithDefault 补齐并检查读取方向。
func (d Direction) WithDefault() (Direction, error) {
	// 1. 空方向按升序处理，保留已支持的方向，不支持的值返回错误。
	switch d {
	case "":
		return DirectionAsc, nil
	case DirectionAsc, DirectionDesc:
		return d, nil
	}
	return "", fmt.Errorf("unsupported direction %q", d)
}

// OrderBy 是 Record 的排序依据。
type OrderBy string

const (
	// OrderByKey 按 Key 字节序排序，是默认依据。
	OrderByKey OrderBy = "key"
	// OrderByUpdatedAt 按最后写入时间排序，时间相同再按 Key。
	OrderByUpdatedAt OrderBy = "updated_at"
)

// Order 描述 Record 的排序：按什么、往哪个方向；零值表示按 Key 升序。
type Order struct {
	By        OrderBy   `json:"by,omitempty"`
	Direction Direction `json:"direction,omitempty"`
}

// WithDefaults 补齐并检查排序字段和方向。
func (o Order) WithDefaults() (Order, error) {
	// 1. 补齐并检查读取方向。
	direction, err := o.Direction.WithDefault()
	if err != nil {
		return Order{}, err
	}
	// 2. 空排序字段按 Key 处理，保留已支持的字段，不支持的值返回错误。
	switch o.By {
	case "":
		return Order{
			By:        OrderByKey,
			Direction: direction,
		}, nil
	case OrderByKey, OrderByUpdatedAt:
		return Order{
			By:        o.By,
			Direction: direction,
		}, nil
	}
	return Order{}, fmt.Errorf("unsupported order by %q", o.By)
}

// Page 是一页结果；Next 为零值表示没有更多。
type Page[T any, C comparable] struct {
	Items []T
	Next  C
}

// PrefixRange 返回覆盖指定前缀的 [start, end) 范围。
func PrefixRange(prefix RecordKey) (RecordKey, RecordKey) {
	// 1. 从末尾找到可递增的字节，截断后作为范围上界。
	end := []byte(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return prefix, RecordKey(end[:i+1])
		}
	}
	// 2. 空前缀或全为 0xff 时不设上界。
	return prefix, ""
}
