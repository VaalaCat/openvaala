import assert from "node:assert/strict"
import { test } from "node:test"
import { getControlCharacter, getTerminalKeySequence } from "./terminal-input"

test("control modifier turns a single character into its control byte", () => {
  const cases: { input: string; want: string | undefined }[] = [
    { input: "c", want: "\x03" },
    { input: "D", want: "\x04" },
    { input: "z", want: "\x1a" },
    { input: "[", want: "\x1b" },
    { input: " ", want: "\0" },
    { input: "?", want: "\x7f" },
    { input: "中文", want: undefined },
    { input: "paste", want: undefined },
    { input: "1", want: undefined },
  ]
  for (const each of cases)
    assert.equal(getControlCharacter(each.input), each.want, each.input)
})

test("cursor keys follow the application cursor mode", () => {
  const cases: {
    key: Parameters<typeof getTerminalKeySequence>[0]
    isApplicationCursorMode: boolean
    want: string
  }[] = [
    { key: "up", isApplicationCursorMode: false, want: "\x1b[A" },
    { key: "up", isApplicationCursorMode: true, want: "\x1bOA" },
    { key: "left", isApplicationCursorMode: true, want: "\x1bOD" },
    { key: "home", isApplicationCursorMode: false, want: "\x1b[H" },
    { key: "end", isApplicationCursorMode: true, want: "\x1bOF" },
    { key: "ctrl-c", isApplicationCursorMode: false, want: "\x03" },
    { key: "esc", isApplicationCursorMode: true, want: "\x1b" },
    { key: "enter", isApplicationCursorMode: false, want: "\r" },
  ]
  for (const each of cases)
    assert.equal(
      getTerminalKeySequence(each.key, each.isApplicationCursorMode),
      each.want,
      `${each.key} ${each.isApplicationCursorMode}`
    )
})
