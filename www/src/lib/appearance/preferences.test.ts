import assert from "node:assert/strict"
import { test } from "node:test"
import { defaultAppearance, parseAppearance } from "./preferences"

for (const scenario of [
  { name: "broken JSON", saved: "{broken", expected: defaultAppearance },
  { name: "null", saved: "null", expected: defaultAppearance },
  { name: "non-object", saved: '"dark"', expected: defaultAppearance },
  {
    name: "missing fields",
    saved: '{"mode":"dark"}',
    expected: { ...defaultAppearance, mode: "dark" },
  },
  {
    name: "independent valid fields",
    saved:
      '{"mode":"unknown","theme":"blue","bodyFont":"system","headingFont":"unknown"}',
    expected: { ...defaultAppearance, theme: "blue", bodyFont: "system" },
  },
  {
    name: "all selected fields",
    saved:
      '{"mode":"light","theme":"blue","bodyFont":"jetbrains-mono","headingFont":"noto-serif"}',
    expected: {
      ...defaultAppearance,
      mode: "light",
      theme: "blue",
      bodyFont: "jetbrains-mono",
      headingFont: "noto-serif",
    },
  },
  {
    name: "independent typography groups",
    saved: JSON.stringify({
      body: { fontSize: 18, lineHeight: 28, paragraphSpacing: 16 },
      code: { paddingX: 20 },
      terminal: { lineHeight: 1.05, padding: 0, letterSpacing: 0.5 },
    }),
    expected: {
      ...defaultAppearance,
      body: { fontSize: 18, lineHeight: 28, paragraphSpacing: 16 },
      code: { ...defaultAppearance.code, paddingX: 20 },
      terminal: {
        ...defaultAppearance.terminal,
        lineHeight: 1.05,
        padding: 0,
        letterSpacing: 0.5,
      },
    },
  },
  {
    name: "invalid sizes do not discard valid siblings",
    saved: JSON.stringify({
      body: { fontSize: "18", lineHeight: 22, paragraphSpacing: -4 },
      code: { fontSize: 1000, paddingX: 3 },
      terminal: { lineHeight: 1.01, padding: 12, letterSpacing: null },
    }),
    expected: {
      ...defaultAppearance,
      body: { ...defaultAppearance.body, lineHeight: 22 },
      terminal: { ...defaultAppearance.terminal, padding: 12 },
    },
  },
  {
    name: "invalid types and extra fields",
    saved:
      '{"mode":true,"theme":1,"bodyFont":[],"headingFont":{},"radius":"large"}',
    expected: defaultAppearance,
  },
]) {
  test(`saved appearance: ${scenario.name}`, () => {
    assert.deepEqual(parseAppearance(scenario.saved), scenario.expected)
  })
}
