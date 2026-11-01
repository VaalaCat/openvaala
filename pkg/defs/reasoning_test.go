package defs_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/VaalaCat/ai-gateway/pkg/llmkit"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestReasoningLLMKitConversion 检查推理内容和完成状态的转换。
func TestReasoningLLMKitConversion(t *testing.T) {
	// 1. 准备已完成和被中断的状态样例。
	tests := []struct {
		name       string
		status     llmkit.ReasoningStatus
		wantStatus defs.ReasoningStatus
	}{
		{
			name:       "completed",
			status:     llmkit.ReasoningCompleted,
			wantStatus: defs.ReasoningCompleted,
		},
		{
			name:       "interrupted",
			status:     llmkit.ReasoningInterrupted,
			wantStatus: defs.ReasoningInterrupted,
		},
	}
	// 2. 检查完整内容往返及各切片不共享。
	for _, tt := range tests {
		t.Run(tt.name,
			func(t *testing.T) {
				source := &llmkit.ReasoningContent{
					Summary:   []string{"summary"},
					Content:   []string{"reasoning"},
					Encrypted: "opaque",
					RawJSON:   json.RawMessage(`{"signature":"signed"}`),
				}
				want := &defs.Reasoning{
					Status:    tt.wantStatus,
					Summary:   []string{"summary"},
					Content:   []string{"reasoning"},
					Encrypted: "opaque",
					RawJSON:   json.RawMessage(`{"signature":"signed"}`),
				}
				reasoning := &defs.Reasoning{}
				reasoning.FromLLMKit(source, tt.status)
				if !reflect.DeepEqual(reasoning, want) {
					t.Fatalf("FromLLMKit() = %#v, want %#v", reasoning, want)
				}
				encoded := reasoning.ToLLMKit()
				if !reflect.DeepEqual(encoded, source) {
					t.Fatalf("ToLLMKit() = %#v, want %#v", encoded, source)
				}
				source.Summary[0] = "mutated"
				encoded.Content[0] = "mutated"
				encoded.RawJSON[0] = 'x'
				if !reflect.DeepEqual(reasoning, want) {
					t.Fatal("conversion aliases reasoning content")
				}
			})
	}
}
