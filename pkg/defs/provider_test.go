package defs_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"gopkg.in/yaml.v3"
)

// TestProviderModelConfigKeepsUnsetAndEmptyListsApart 检查模型列表的未填写与空列表在编码后仍可区分。
func TestProviderModelConfigKeepsUnsetAndEmptyListsApart(t *testing.T) {
	// 1. 准备未填写、空和非空列表及 JSON、YAML 编码入口。
	tests := []struct {
		name  string
		model defs.ProviderModelConfig
		// wantEfforts、wantRoles 序列化结果里是否应写出 reasoning_efforts、merge_message_roles？
		wantEfforts bool
		wantRoles   bool
	}{
		{name: "unset_lists_stay_absent"},
		{
			name: "empty_lists_are_written",
			model: defs.ProviderModelConfig{
				ReasoningEfforts:  []string{},
				MergeMessageRoles: []string{},
			},
			wantEfforts: true,
			wantRoles:   true,
		},
		{
			name: "filled_lists_are_written",
			model: defs.ProviderModelConfig{
				ReasoningEfforts:  []string{"low", "high"},
				MergeMessageRoles: []string{"user"},
			},
			wantEfforts: true,
			wantRoles:   true,
		},
		{
			name:        "only_efforts_empty",
			model:       defs.ProviderModelConfig{ReasoningEfforts: []string{}},
			wantEfforts: true,
		},
	}
	formats := []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{
			name:      "yaml",
			marshal:   yaml.Marshal,
			unmarshal: yaml.Unmarshal,
		},
		{
			name:      "json",
			marshal:   json.Marshal,
			unmarshal: json.Unmarshal,
		},
	}
	// 2. 检查字段是否写出及往返后的 nil 和空列表。
	for _, tt := range tests {
		for _, format := range formats {
			t.Run(tt.name+"/"+format.name, func(t *testing.T) {
				payload, err := format.marshal(tt.model)
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Contains(string(payload), "reasoning_efforts"); got != tt.wantEfforts {
					t.Fatalf("reasoning_efforts written = %v, want %v in %s", got, tt.wantEfforts, payload)
				}
				if got := strings.Contains(string(payload), "merge_message_roles"); got != tt.wantRoles {
					t.Fatalf("merge_message_roles written = %v, want %v in %s", got, tt.wantRoles, payload)
				}
				var decoded defs.ProviderModelConfig
				if err := format.unmarshal(payload, &decoded); err != nil {
					t.Fatal(err)
				}
				if (decoded.ReasoningEfforts == nil) != (tt.model.ReasoningEfforts == nil) || !slices.Equal(decoded.ReasoningEfforts, tt.model.ReasoningEfforts) {
					t.Fatalf("reasoning_efforts = %#v, want %#v", decoded.ReasoningEfforts, tt.model.ReasoningEfforts)
				}
				if (decoded.MergeMessageRoles == nil) != (tt.model.MergeMessageRoles == nil) || !slices.Equal(decoded.MergeMessageRoles, tt.model.MergeMessageRoles) {
					t.Fatalf("merge_message_roles = %#v, want %#v", decoded.MergeMessageRoles, tt.model.MergeMessageRoles)
				}
			})
		}
	}
}

// TestGetProviderModelKey 检查上游模型 ID 转换为配置 Key。
func TestGetProviderModelKey(t *testing.T) {
	// 1. 准备合法字符、特殊字符、中文和空 ID。
	tests := []struct {
		name string
		id   string
		want string
	}{
		{
			name: "allowed_characters_stay",
			id:   "gpt-5.1_mini",
			want: "gpt-5.1_mini",
		},
		{
			name: "slash_and_colon_become_dashes",
			id:   "deepseek/deepseek-r1:free",
			want: "deepseek-deepseek-r1-free",
		},
		{
			name: "spaces_and_non_ascii_become_dashes",
			id:   "通义 max",
			want: "---max",
		},
		{
			name: "empty",
			id:   "",
			want: "",
		},
	}
	// 2. 检查替换后的 Key。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defs.GetProviderModelKey(tt.id); got != tt.want {
				t.Fatalf("GetProviderModelKey(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

// TestGetProviderModelKeys 检查批量模型 Key 的撞名处理。
func TestGetProviderModelKeys(t *testing.T) {
	// 1. 准备合法 ID、撞名、重复和空批次。
	tests := []struct {
		name string
		ids  []string
		want map[string]string
	}{
		{
			name: "valid_ids_keep_themselves",
			ids:  []string{"gpt-5", "m1"},
			want: map[string]string{"gpt-5": "gpt-5", "m1": "m1"},
		},
		{
			name: "colliding_keys_get_suffixes_in_sorted_order",
			ids:  []string{"p:q", "p q", "p/q"},
			want: map[string]string{"p q": "p-q", "p/q": "p-q-2", "p:q": "p-q-3"},
		},
		{
			// 本身合法的 id 先用自己当 key；换过字符的 id 加后缀时跳过这些已占用的 key。
			name: "suffixes_skip_real_ids",
			ids:  []string{"a b", "a+b", "a-b-2", "a-b"},
			want: map[string]string{"a-b": "a-b", "a-b-2": "a-b-2", "a b": "a-b-3", "a+b": "a-b-4"},
		},
		{
			name: "duplicates_share_one_key",
			ids:  []string{"a/b", "a/b", "m", "m"},
			want: map[string]string{"a/b": "a-b", "m": "m"},
		},
		{
			name: "empty",
			want: map[string]string{},
		},
	}
	// 2. 检查 Key 映射及后缀分配。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defs.GetProviderModelKeys(tt.ids); !maps.Equal(got, tt.want) {
				t.Fatalf("GetProviderModelKeys(%q) = %v, want %v", tt.ids, got, tt.want)
			}
		})
	}
}
