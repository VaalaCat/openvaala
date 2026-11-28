package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestAgentPresets(t *testing.T) {
	for _, preset := range GetAgentPresets() {
		t.Run(preset.Name, func(t *testing.T) {
			var want map[string]map[string]any
			if preset.Name == "claude-code" {
				want = map[string]map[string]any{"session/new": {"_meta": map[string]any{"claudeCode": map[string]any{"options": map[string]any{"persistSession": false}}}}}
			}
			if !reflect.DeepEqual(preset.EphemeralParamOverwrite, want) {
				t.Fatalf("temporary session parameters = %v, want %v", preset.EphemeralParamOverwrite, want)
			}
			if strings.TrimSpace(preset.Command) == "" || preset.Icon == "" {
				t.Fatalf("preset command or icon is missing: %+v", preset)
			}
		})
	}
}

func TestGetAgentPresetName(t *testing.T) {
	for _, tc := range []struct {
		want string
		used []string
	}{
		{"codex-acp", nil},
		{"codex-acp-2", []string{"codex-acp", "codex-acp-3"}},
		{"codex-acp-3", []string{"codex-acp", "codex-acp-2"}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := GetAgentPresetName("codex-acp", tc.used); got != tc.want {
				t.Fatalf("name = %q, want %q", got, tc.want)
			}
		})
	}
}
