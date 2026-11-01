package storage_test

import (
	"testing"

	"github.com/VaalaCat/openvaala/pkg/storage"
)

// TestPrefixRange 检查 Key 前缀对应的读写范围。
func TestPrefixRange(t *testing.T) {
	// 1. 准备斜杠结尾、末尾 0xff、全 0xff 和空前缀。
	tests := []struct {
		name      string
		prefix    storage.RecordKey
		wantStart storage.RecordKey
		wantEnd   storage.RecordKey
	}{
		{
			name:      "slash_suffix",
			prefix:    "session-1/",
			wantStart: "session-1/",
			wantEnd:   "session-10",
		},
		{
			name:      "trailing_ff_is_dropped",
			prefix:    "a\xff",
			wantStart: "a\xff",
			wantEnd:   "b",
		},
		{
			name:      "all_ff_has_no_upper_bound",
			prefix:    "\xff",
			wantStart: "\xff",
			wantEnd:   "",
		},
		{
			name:      "empty_prefix_covers_everything",
			prefix:    "",
			wantStart: "",
			wantEnd:   "",
		},
	}
	// 2. 检查包含下界和排除上界。
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := storage.PrefixRange(tt.prefix)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("PrefixRange(%q) = %q, %q, want %q, %q", tt.prefix, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}
