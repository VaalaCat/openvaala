package worldserver

import "testing"

func TestACL(t *testing.T) {
	office := NodeRecord{
		NodeName: "office",
		Owner:    "alice",
		Tags:     []string{"work"},
	}
	home := NodeRecord{
		NodeName: "home",
		Owner:    "alice",
	}
	partner := NodeRecord{
		NodeName: "partner",
		Owner:    "bob",
		Tags:     []string{"trusted"},
	}
	allowTagged := ACLRule{
		Priority: 10,
		From:     []string{"tag:work"},
		To:       []string{"node:partner", "tag:other"},
		Action:   "allow",
	}
	denyPartners := ACLRule{
		Priority: 20,
		From:     []string{"owner:*"},
		To:       []string{"owner:bob"},
		Action:   "deny",
	}
	denyAll := ACLRule{
		From:   []string{"*"},
		To:     []string{"*"},
		Action: "deny",
	}
	for _, tt := range []struct {
		name  string
		to    NodeRecord
		rules []ACLRule
		// allowed 是否放行测试请求？
		allowed bool
		index   int
	}{
		{
			name:    "same owner",
			index:   -1,
			to:      home,
			allowed: true,
		},
		{
			name:  "different owners",
			index: -1,
			to:    partner,
		},
		{
			name:    "first matching rule wins",
			to:      partner,
			rules:   []ACLRule{denyPartners, allowTagged},
			allowed: true,
		},
		{
			name:  "explicit deny wins over same owner",
			to:    home,
			rules: []ACLRule{denyAll},
		},
		{
			name:  "explicit deny wins over later allow",
			to:    partner,
			rules: []ACLRule{allowTagged, denyAll},
		},
		{name: "no matching rule", to: home, rules: []ACLRule{allowTagged}, allowed: true, index: -1},
		{
			name: "second matching rule", to: home,
			rules:   []ACLRule{allowTagged, {Priority: 20, From: []string{"owner:alice"}, To: []string{"*"}, Action: "allow"}},
			allowed: true, index: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 1. 检查有序规则的首条匹配结果。
			if got, index := isAllowedByACL(sortedACL(tt.rules), office, tt.to); got != tt.allowed || index != tt.index {
				t.Fatalf("ACL = (%t, %d), want (%t, %d)", got, index, tt.allowed, tt.index)
			}
		})
	}
}
