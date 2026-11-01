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
      mode: "light",
      theme: "blue",
      bodyFont: "jetbrains-mono",
      headingFont: "noto-serif",
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
