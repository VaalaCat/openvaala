package world

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAddress(t *testing.T) {
	// 1. 按完整地址、简写和非法输入核对解析结果。
	for _, tc := range []struct {
		raw         string
		domain      string
		nodeAddress string
		target      string
	}{
		{
			raw:         "home@alice.dev",
			nodeAddress: "home@alice.dev",
		},
		{
			raw:         "home@alice.dev/ops",
			nodeAddress: "home@alice.dev",
			target:      "ops",
		},
		{
			raw:         "laptop@vaala.corp.example.com/reviewer",
			nodeAddress: "laptop@vaala.corp.example.com",
			target:      "reviewer",
		},
		{
			raw:         "home",
			domain:      "localhost:8080",
			nodeAddress: "home@localhost:8080",
		},
		{
			raw:         "home/ops",
			domain:      "alice.dev",
			nodeAddress: "home@alice.dev",
			target:      "ops",
		},
		{
			raw:         "node_1@[::1]:443/to-2",
			nodeAddress: "node_1@[::1]:443",
			target:      "to-2",
		},
		{raw: ""},
		{raw: "home"},
		{raw: "home@"},
		{raw: "@alice.dev"},
		{raw: "Home@alice.dev"},
		{raw: "home@Alice.dev"},
		{raw: "home@bad..dev"},
		{raw: "home@bad-.dev"},
		{raw: "home@localhost:"},
		{raw: "home@localhost:+80"},
		{raw: "home@localhost:65536"},
		{raw: "home@alice@dev"},
		{raw: "home@alice.dev/"},
		{raw: "home@alice.dev/Ops"},
		{raw: "home@alice.dev/ops/next"},
		{raw: strings.Repeat("x", 64) + "@dev"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			// 1. 非法输入返回可识别的地址错误。
			a, err := ParseAddress(tc.raw, tc.domain)
			if tc.nodeAddress == "" {
				if !errors.Is(err, ErrInvalidWorldAddress) {
					t.Fatalf("got %v, want ErrInvalidWorldAddress", err)
				}
				return
			}
			// 2. 核对节点与目标的规范地址。
			want := tc.nodeAddress
			if tc.target != "" {
				want += "/" + tc.target
			}
			if err != nil || a.NodeAddress() != tc.nodeAddress || a.Target != tc.target || a.String() != want {
				t.Fatalf("address %#v, error %v, want %s", a, err, want)
			}
		})
	}
}
