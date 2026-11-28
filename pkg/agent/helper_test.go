package agent

import (
	"testing"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

func TestGetAgentIconKey(t *testing.T) {
	for _, tt := range []struct{ name, command, want string }{
		{"agy", "", "antigravity"},
		{"Claude-Code-ACP", "", "claudecode"},
		{"custom", "/opt/bin/Gemini-CLI.exe", "geminicli"},
		{"amp-acp", "", "amp"},
		{"example-sampler", "/opt/bin/agent", ""},
		{"custom", "/opt/cursor/bin/agent", ""},
		{"custom", `C:\bin\codex-acp.exe`, "codex"},
		{"GitHub Copilot", "", "githubcopilot"},
		{"amp", "/bin/codex-acp", "amp"},
	} {
		t.Run(tt.name+tt.command, func(t *testing.T) {
			definition := AgentDefinition{Name: tt.name, Type: "external"}
			if tt.command != "" {
				definition.Type = defs.AgentTypeACP
				definition.ACP = &defs.ACPCommand{Command: tt.command}
			}
			if got := definition.GetIconKey(); got != tt.want {
				t.Fatalf("icon = %q, want %q", got, tt.want)
			}
		})
	}
}
