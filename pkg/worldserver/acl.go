package worldserver

import (
	"fmt"
	"slices"
	"strings"
)

// isAllowedByACL 当前规则是否允许来源节点访问目标节点，命中了哪条规则？
func isAllowedByACL(rules []ACLRule, from, to NodeRecord) (allowed bool, ruleIndex int) {
	// 1. 按优先级采用首条来源和目标均命中的规则。
	for index, rule := range rules {
		if isACLSelectorMatched(rule.From, from) && isACLSelectorMatched(rule.To, to) {
			return rule.Action == "allow", index
		}
	}

	// 2. 默认允许相同归属的节点互访。
	return from.Owner == to.Owner, -1
}

// isACLSelectorMatched 节点是否命中任一选择器？
func isACLSelectorMatched(selectors []string, node NodeRecord) bool {
	// 1. 按选择器匹配节点名称、归属或标签。
	for _, selector := range selectors {
		if selector == "*" {
			return true
		}
		field, value, _ := strings.Cut(selector, ":")
		switch field {
		case "owner":
			if value == "*" || value == node.Owner {
				return true
			}
		case "tag":
			if slices.Contains(node.Tags, value) {
				return true
			}
		case "node":
			if value == node.NodeName {
				return true
			}
		}
	}
	return false
}

// validateACL 校验管理面提交的操作和选择器。
func validateACL(rules []ACLRule) error {
	// 1. 逐条检查操作和两侧的非空选择器列表。
	for i, rule := range rules {
		if rule.Action != "allow" && rule.Action != "deny" {
			return fmt.Errorf("ACL rule %d has an invalid action", i+1)
		}
		if len(rule.From) == 0 || len(rule.To) == 0 {
			return fmt.Errorf("ACL rule %d requires from and to selectors", i+1)
		}
		for _, selectors := range [][]string{rule.From, rule.To} {
			for _, selector := range selectors {
				if selector == "*" {
					continue
				}
				field, value, _ := strings.Cut(selector, ":")
				if value == "" || (field != "owner" && field != "tag" && field != "node") {
					return fmt.Errorf("ACL rule %d has an invalid selector %q", i+1, selector)
				}
			}
		}
	}
	return nil
}
