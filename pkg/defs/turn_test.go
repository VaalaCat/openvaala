package defs_test

import (
	"encoding/json"
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestTokenUsageAdd 检查各项 token 用量相加。
func TestTokenUsageAdd(t *testing.T) {
	// 1. 准备零用量、两边都有值和单边有值的样例。
	tests := []struct {
		name        string
		left, right defs.TokenUsage
		want        defs.TokenUsage
	}{
		{
			name: "zero",
			want: defs.TokenUsage{},
		},
		{
			name: "sum",
			left: defs.TokenUsage{
				Input:      10,
				Output:     2,
				CacheRead:  4,
				CacheWrite: 1,
				Reasoning:  1,
			},
			right: defs.TokenUsage{
				Input:      5,
				Output:     3,
				CacheRead:  1,
				CacheWrite: 2,
				Reasoning:  2,
			},
			want: defs.TokenUsage{
				Input:      15,
				Output:     5,
				CacheRead:  5,
				CacheWrite: 3,
				Reasoning:  3,
			},
		},
		{
			name:  "right only",
			right: defs.TokenUsage{Input: 7},
			want:  defs.TokenUsage{Input: 7},
		},
	}
	// 2. 检查逐项相加结果。
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.left.Add(test.right); got != test.want {
				t.Fatalf("Add() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestTurnUsageJSON 检查 Turn 用量能从 JSON 读出，旧记录没有 usage 时为零，零用量不写出 usage 字段。
func TestTurnUsageJSON(t *testing.T) {
	// 1. 准备带用量和不带用量的 Turn JSON。
	tests := []struct {
		name string
		json string
		want defs.TokenUsage
	}{
		{
			name: "legacy record without usage",
			json: `{"id":"t1","session_id":"s1","status":"completed"}`,
			want: defs.TokenUsage{},
		},
		{
			name: "record with usage",
			json: `{"id":"t1","session_id":"s1","status":"failed","usage":{"input":12,"output":3,"cache_read":4,"cache_write":0,"reasoning":1}}`,
			want: defs.TokenUsage{
				Input:     12,
				Output:    3,
				CacheRead: 4,
				Reasoning: 1,
			},
		},
	}
	// 2. 检查读取的用量。
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var turn defs.Turn
			if err := json.Unmarshal([]byte(test.json), &turn); err != nil {
				t.Fatal(err)
			}
			if turn.Usage != test.want {
				t.Fatalf("Usage = %+v, want %+v", turn.Usage, test.want)
			}
		})
	}
	// 3. 检查零用量不写出 usage 字段。
	data, err := json.Marshal(defs.Turn{
		ID:             "t1",
		ConversationID: "c1",
		SessionID:      "s1",
		Status:         defs.TurnStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"id":"t1","conversation_id":"c1","session_id":"s1","status":"running"}` {
		t.Fatalf("marshal = %s", data)
	}
}

// TestTurnFinalModelCallIDJSON 检查最终调用字段的往返与缺字段记录的读取。
func TestTurnFinalModelCallIDJSON(t *testing.T) {
	// 1. 准备带最终调用和缺少该字段的已完成记录。
	for _, tt := range []struct {
		name string
		json string
		want defs.ModelCallID
	}{
		{
			name: "without_final_call",
			json: `{"id":"t1","session_id":"s1","status":"completed"}`,
		},
		{
			name: "with_final_call",
			json: `{"id":"t1","session_id":"s1","status":"completed","final_model_call_id":"final"}`,
			want: "final",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 2. 读取后重新编码，检查字段值与空值省略。
			var record defs.Turn
			if err := json.Unmarshal([]byte(tt.json), &record); err != nil {
				t.Fatal(err)
			}
			if record.FinalModelCallID != tt.want {
				t.Fatalf("final model call = %q, want %q", record.FinalModelCallID, tt.want)
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			value, found := fields["final_model_call_id"]
			if found != (tt.want != "") || (found && string(value) != `"`+string(tt.want)+`"`) {
				t.Fatalf("encoded final model call = %s, present = %v, want %q", value, found, tt.want)
			}
		})
	}
}
