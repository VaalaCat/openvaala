import assert from "node:assert/strict"
import { test } from "node:test"
import { getAgentPresetName } from "./agent-settings"

for (const { used, want } of [
  { used: [], want: "codex-acp" },
  { used: ["codex-acp", "codex-acp-3"], want: "codex-acp-2" },
  { used: ["codex-acp", "codex-acp-2"], want: "codex-acp-3" },
]) {
  test(`Agent preset name: ${want}`, () => {
    assert.equal(getAgentPresetName("codex-acp", used), want)
  })
}
