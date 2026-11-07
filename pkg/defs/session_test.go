package defs_test

import (
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// TestSessionCtxMeta 检查 SessionCtxMeta 沿 ctx 合并：非空字段覆盖已有值，空字段保留已有值。
func TestSessionCtxMeta(t *testing.T) {
	// 1. 准备依次放入的 meta 和期望读到的结果。
	tests := []struct {
		name  string
		metas []defs.SessionCtxMeta
		want  defs.SessionCtxMeta
	}{
		{
			name: "empty_ctx",
			want: defs.SessionCtxMeta{},
		},
		{
			name:  "conversation_then_session",
			metas: []defs.SessionCtxMeta{{ConversationID: "c1"}, {SessionID: "s1", AgentName: "assistant"}},
			want:  defs.SessionCtxMeta{ConversationID: "c1", SessionID: "s1", AgentName: "assistant"},
		},
		{
			name:  "later_non_empty_wins",
			metas: []defs.SessionCtxMeta{{SessionID: "parent"}, {SessionID: "child"}},
			want:  defs.SessionCtxMeta{SessionID: "child"},
		},
		{
			name:  "empty_field_keeps_earlier",
			metas: []defs.SessionCtxMeta{{SessionID: "s1", AgentName: "a"}, {AgentName: ""}},
			want:  defs.SessionCtxMeta{SessionID: "s1", AgentName: "a"},
		},
		{
			name:  "child_session_keeps_conversation",
			metas: []defs.SessionCtxMeta{{ConversationID: "c1"}, {SessionID: "child", ParentSessionID: "parent"}},
			want:  defs.SessionCtxMeta{ConversationID: "c1", SessionID: "child", ParentSessionID: "parent"},
		},
	}
	// 2. 依次放入 meta 后读取。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			for _, meta := range tt.metas {
				ctx = defs.ContextWithSessionCtxMeta(ctx, meta)
			}
			if got := defs.GetSessionCtxMeta(ctx); got != tt.want {
				t.Fatalf("GetSessionCtxMeta() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
