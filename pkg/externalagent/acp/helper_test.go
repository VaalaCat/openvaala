package acp

import (
	"encoding/json"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func TestMergeParamOverwrite(t *testing.T) {
	req := sdk.NewSessionRequest{Cwd: "/main", McpServers: []sdk.McpServer{}, Meta: map[string]any{"keep": true}}
	const method = "session/new"
	base := map[string]map[string]any{method: {"_meta": map[string]any{"options": map[string]any{"keep": true, "save": true}}, "cwd": "/other", "additionalDirectories": []string{"/one", "/two"}}}
	side := map[string]map[string]any{method: {"_meta": map[string]any{"options": map[string]any{"save": false}}, "additionalDirectories": []string{"/side"}, "unknown": true}}
	for _, tc := range []struct {
		name   string
		layers []map[string]map[string]any
		want   string
	}{
		{"different method", []map[string]map[string]any{{"session/load": {"cwd": "/wrong"}}}, `{"_meta":{"keep":true},"cwd":"/main","mcpServers":[]}`},
		{"nested object and scalar", []map[string]map[string]any{base}, `{"_meta":{"keep":true,"options":{"keep":true,"save":true}},"additionalDirectories":["/one","/two"],"cwd":"/other","mcpServers":[]}`},
		{"second layer replaces array and drops unknown field", []map[string]map[string]any{base, side}, `{"_meta":{"keep":true,"options":{"keep":true,"save":false}},"additionalDirectories":["/side"],"cwd":"/other","mcpServers":[]}`},
		{"base unchanged", []map[string]map[string]any{base}, `{"_meta":{"keep":true,"options":{"keep":true,"save":true}},"additionalDirectories":["/one","/two"],"cwd":"/other","mcpServers":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeParamOverwrite(req, method, tc.layers...)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(got)
			if err != nil || string(b) != tc.want {
				t.Fatalf("request = %s, err %v, want %s", b, err, tc.want)
			}
		})
	}
}
