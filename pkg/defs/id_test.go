package defs_test

import (
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/google/uuid"
)

// TestNewSortableIDs 检查各实体 ID 的版本和递增顺序。
func TestNewSortableIDs(t *testing.T) {
	// 1. 准备各实体的 ID 生成入口。
	tests := []struct {
		name     string
		generate func() string
	}{
		{
			name:     "conversation",
			generate: func() string { return string(defs.NewConversationID()) },
		},
		{
			name:     "item",
			generate: func() string { return string(defs.NewItemID()) },
		},
		{
			name:     "session",
			generate: func() string { return string(defs.NewSessionID()) },
		},
		{
			name:     "turn",
			generate: func() string { return string(defs.NewTurnID()) },
		},
		{
			name:     "model_call",
			generate: func() string { return string(defs.NewModelCallID()) },
		},
		{
			name:     "hook_run",
			generate: func() string { return string(defs.NewHookRunID()) },
		},
	}
	// 2. 连续生成 ID，检查 UUID v7 和字典序递增。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := ""
			for range 128 {
				generated := tt.generate()
				parsed, err := uuid.Parse(generated)
				if err != nil {
					t.Fatal(err)
				}
				if parsed.Version() != 7 {
					t.Fatalf("ID %q has version %d, want 7", generated, parsed.Version())
				}
				if previous != "" && generated <= previous {
					t.Fatalf("ID %q is not after %q", generated, previous)
				}
				previous = generated
			}
		})
	}
}
