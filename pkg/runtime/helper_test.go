package runtime

import (
	"testing"

	"github.com/VaalaCat/openvaala/pkg/dao"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestCountConversationMessages 检查各类 Item 是否计入对话消息数。
func TestCountConversationMessages(t *testing.T) {
	// 1. 准备用户消息、Agent 回复和不计数的 Item。
	message := func(from *defs.Address, modelCallID defs.ModelCallID) *defs.Item {
		return &defs.Item{
			ID:          defs.NewItemID(),
			ModelCallID: modelCallID,
			Kind:        defs.ItemMessage,
			Message:     &defs.Message{From: from},
		}
	}
	tests := []struct {
		name string
		item *defs.Item
		want int
	}{
		{
			name: "user_message",
			item: message(&defs.Address{Kind: defs.IdentityUser}, ""),
			want: 1,
		},
		{
			name: "root_agent_reply",
			item: message(&defs.Address{
				Kind: defs.IdentityAgent,
				ID:   "root",
			}, defs.NewModelCallID()),
			want: 1,
		},
		// 分叉回复保留来源根 Session 的发送地址，ModelCallID 标记它是模型回复。
		{
			name: "copied_agent_reply",
			item: message(&defs.Address{
				Kind: defs.IdentityAgent,
				ID:   "source-root",
			}, defs.NewModelCallID()),
			want: 1,
		},
		{
			name: "child_agent_result",
			item: message(&defs.Address{
				Kind: defs.IdentityAgent,
				ID:   "child",
			}, ""),
			want: 0,
		},
		{
			name: "message_without_sender",
			item: message(nil, defs.NewModelCallID()),
			want: 0,
		},
		{
			name: "reasoning",
			item: &defs.Item{
				ID:          defs.NewItemID(),
				ModelCallID: defs.NewModelCallID(),
				Kind:        defs.ItemReasoning,
				Reasoning:   &defs.Reasoning{},
			},
			want: 0,
		},
		{
			name: "message_kind_without_payload",
			item: &defs.Item{
				ID:   defs.NewItemID(),
				Kind: defs.ItemMessage,
			},
			want: 0,
		},
		{
			name: "tool_result",
			item: &defs.Item{
				ID:         defs.NewItemID(),
				Kind:       defs.ItemToolResult,
				ToolResult: &defs.ToolResult{},
			},
			want: 0,
		},
		{
			name: "context_summary",
			item: &defs.Item{
				ID:             defs.NewItemID(),
				ModelCallID:    defs.NewModelCallID(),
				Kind:           defs.ItemContextSummary,
				ContextSummary: &defs.ContextSummary{},
			},
			want: 0,
		},
		{
			name: "model_error",
			item: &defs.Item{
				ID:         defs.NewItemID(),
				Kind:       defs.ItemModelError,
				ModelError: &defs.ModelError{},
			},
			want: 0,
		},
		{
			name: "tool_call",
			item: &defs.Item{
				ID:          defs.NewItemID(),
				ModelCallID: defs.NewModelCallID(),
				Kind:        defs.ItemToolCall,
				ToolCall:    &defs.ToolCall{},
			},
			want: 0,
		},
	}
	// 2. 逐项核对计数。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countConversationMessages([]dao.LoggedItem{{
				Ordinal: 1,
				Item:    tt.item,
			}}); got != tt.want {
				t.Fatalf("countConversationMessages() = %d, want %d", got, tt.want)
			}
		})
	}
}
