package sqlite

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// getRecordFieldPaths 检查记录分组声明并生成各字段的 SQL JSON 路径。
func getRecordFieldPaths(namespaces []storage.RecordNamespace) (map[string]map[string]string, error) {
	// 1. 检查分组名称和重复声明。
	paths := make(map[string]map[string]string, len(namespaces))
	for _, namespace := range namespaces {
		if !namespacePattern.MatchString(namespace.Name) {
			return nil, fmt.Errorf("invalid record namespace %q", namespace.Name)
		}
		if _, found := paths[namespace.Name]; found {
			return nil, fmt.Errorf("duplicate record namespace %q", namespace.Name)
		}
		// 2. 检查字段名称和路径，逐段编码 JSON 键并转义 SQL 单引号。
		fields := make(map[string]string, len(namespace.Fields))
		for _, field := range namespace.Fields {
			if field.Name == "" || len(field.PayloadJSONPath) == 0 {
				return nil, fmt.Errorf("record namespace %q field requires a name and payload JSON path", namespace.Name)
			}
			if _, found := fields[field.Name]; found {
				return nil, fmt.Errorf("duplicate record field %q in namespace %q", field.Name, namespace.Name)
			}
			path := "$"
			for _, key := range field.PayloadJSONPath {
				encoded, _ := json.Marshal(key)
				path += "." + string(encoded)
			}
			fields[field.Name] = "'" + strings.ReplaceAll(path, "'", "''") + "'"
		}
		// 3. 要求索引字段已在分组中声明。
		for _, field := range namespace.IndexedFields {
			if _, found := fields[field]; !found {
				return nil, fmt.Errorf("unknown indexed field %q in namespace %q", field, namespace.Name)
			}
		}
		paths[namespace.Name] = fields
	}
	return paths, nil
}

// quoteIdentifier 返回加双引号的 SQL 名称。
func quoteIdentifier(value string) string {
	// 1. 将名称内部的双引号写成两个双引号，再包上双引号。
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// getRecordFilterSQL 将记录筛选条件转换为 SQL 表达式和参数。
func getRecordFilterSQL(fieldPaths map[string]string, filter storage.RecordFilter) (string, []any, error) {
	// 1. 空条件返回空表达式，按 Logic 选择 AND 或 OR。
	if len(filter.Conditions) == 0 {
		return "", nil, nil
	}
	separator := " AND "
	switch filter.Logic {
	case "", storage.RecordFilterAnd:
	case storage.RecordFilterOr:
		separator = " OR "
	default:
		return "", nil, fmt.Errorf("unsupported filter logic %q", filter.Logic)
	}
	// 2. 检查字段和操作符，生成仅匹配字符串的条件，值放入独立参数。
	conditions := make([]string, 0, len(filter.Conditions))
	var arguments []any
	for _, condition := range filter.Conditions {
		path, found := fieldPaths[condition.Field]
		if !found {
			return "", nil, fmt.Errorf("unknown filter field %q", condition.Field)
		}
		expression := "json_extract(CAST(payload AS TEXT), " + path + ")"
		var comparison string
		switch condition.Operator {
		case storage.RecordFilterIn:
			if len(condition.Values) == 0 {
				return "", nil, fmt.Errorf("filter %q requires at least one value", condition.Field)
			}
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(condition.Values)), ",")
			comparison = expression + " IN (" + placeholders + ")"
		case storage.RecordFilterContains:
			if len(condition.Values) != 1 {
				return "", nil, fmt.Errorf("filter %q requires exactly one value", condition.Field)
			}
			comparison = "instr(" + expression + ", ?) > 0"
		default:
			return "", nil, fmt.Errorf("unsupported filter operator %q", condition.Operator)
		}
		conditions = append(conditions, "(json_type(CAST(payload AS TEXT), "+path+") = 'text' AND "+comparison+")")
		for _, value := range condition.Values {
			arguments = append(arguments, value)
		}
	}
	// 3. 用选定的连接符组合条件并添加外层括号。
	return "(" + strings.Join(conditions, separator) + ")", arguments, nil
}
