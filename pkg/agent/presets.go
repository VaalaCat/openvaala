package agent

import (
	"slices"
	"strconv"

	"github.com/VaalaCat/openvaala/pkg/defs"
)

// GetAgentPresets 返回可填入新 Agent 配置的 ACP 启动命令。
func GetAgentPresets() []AgentPreset {
	presets := []AgentPreset{
		{Name: "codex-acp", DisplayName: "Codex", Description: "OpenAI's coding agent", Command: "codex-acp"},
		{
			Name: "claude-code", DisplayName: "Claude Code", Description: "Anthropic's coding agent", Command: "claude-agent-acp",
			EphemeralParamOverwrite: map[string]map[string]any{
				"session/new": {"_meta": map[string]any{"claudeCode": map[string]any{"options": map[string]any{"persistSession": false}}}},
			},
		},
		{Name: "gemini-cli", DisplayName: "Gemini CLI", Description: "Google's coding agent", Command: "gemini", Args: []string{"--acp"}},
		{Name: "opencode", DisplayName: "OpenCode", Description: "Open-source coding agent", Command: "opencode", Args: []string{"acp"}},
		{Name: "cursor", DisplayName: "Cursor", Description: "Cursor's coding agent", Command: "cursor-agent", Args: []string{"acp"}},
		{Name: "goose", DisplayName: "Goose", Description: "Block's extensible coding agent", Command: "goose", Args: []string{"acp"}},
		{Name: "qwen-code", DisplayName: "Qwen Code", Description: "Alibaba's coding agent", Command: "qwen", Args: []string{"--acp", "--experimental-skills"}},
		{Name: "kimi-code", DisplayName: "Kimi Code", Description: "Moonshot AI's coding agent", Command: "kimi", Args: []string{"acp"}},
		{Name: "antigravity", DisplayName: "Antigravity", Description: "Google's Antigravity CLI", Command: "agy", Args: []string{"acp"}},
		{Name: "github-copilot", DisplayName: "GitHub Copilot", Description: "GitHub's coding agent", Command: "copilot", Args: []string{"--acp", "--stdio"}},
		{Name: "cline", DisplayName: "Cline", Description: "Open-source coding agent CLI", Command: "cline", Args: []string{"--acp"}},
		{Name: "kiro", DisplayName: "Kiro CLI", Description: "Amazon's coding agent", Command: "kiro-cli", Args: []string{"acp"}},
		{Name: "amp-acp", DisplayName: "Amp", Description: "ACP adapter for Amp CLI", Command: "amp-acp"},
	}
	for i := range presets {
		preset := &presets[i]
		definition := AgentDefinition{Name: preset.Name, Type: defs.AgentTypeACP, ACP: &defs.ACPCommand{Command: preset.Command}}
		preset.Icon = definition.GetIconKey()
	}
	return presets
}

// GetAgentPresetName 返回预设第一个未被占用的名字。
func GetAgentPresetName(name string, usedNames []string) string {
	availableName := name
	for suffix := 2; slices.Contains(usedNames, availableName); suffix++ {
		availableName = name + "-" + strconv.Itoa(suffix)
	}
	return availableName
}
