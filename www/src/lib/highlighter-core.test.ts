import assert from "node:assert/strict"
import { test } from "node:test"
import { highlightLines } from "./highlighter-core"
import { shouldHighlight } from "./highlighter-types"

const highlightLimit = 200 * 1024

test("shouldHighlight skips text over 200KB", () => {
  const cases = [
    { name: "at_limit", text: "a".repeat(highlightLimit), want: true },
    { name: "over_limit", text: "a".repeat(highlightLimit + 1), want: false },
  ]
  for (const { name, text, want } of cases)
    assert.equal(shouldHighlight(text), want, name)
})

test("highlightLines colors known languages line by line", async () => {
  const cases: {
    name: string
    text: string
    lang: string
    // undefined 表示不着色，按纯文本显示
    wantLines: string[] | undefined
  }[] = [
    { name: "json", text: '{"a":1}', lang: "json", wantLines: ['{"a":1}'] },
    {
      name: "alias",
      text: "a: 1\nb: 2",
      lang: "yml",
      wantLines: ["a: 1", "b: 2"],
    },
    {
      name: "typescript",
      text: "const value: number = 1\nconsole.log(value)",
      lang: "TS",
      wantLines: ["const value: number = 1", "console.log(value)"],
    },
    {
      name: "python",
      text: "def greeting():\n    return 'hello'",
      lang: "py",
      wantLines: ["def greeting():", "    return 'hello'"],
    },
    {
      name: "go",
      text: "package main\nfunc main() {}",
      lang: "go",
      wantLines: ["package main", "func main() {}"],
    },
    {
      name: "diff",
      text: "-before\n+after",
      lang: "patch",
      wantLines: ["-before", "+after"],
    },
    { name: "unknown_language", text: "x", lang: "nope", wantLines: undefined },
    {
      name: "too_large",
      text: "a".repeat(highlightLimit + 1),
      lang: "json",
      wantLines: undefined,
    },
  ]
  for (const { name, text, lang, wantLines } of cases) {
    const lines = await highlightLines(text, lang)
    assert.deepEqual(
      lines?.map((line) => line.map((token) => token.content).join("")),
      wantLines,
      name
    )
    if (!lines) continue
    const styles = lines.flat().map((token) => token.style ?? {})
    assert.ok(
      styles.every(
        (style) => "--shiki-light" in style && "--shiki-dark" in style
      ),
      name + ": tokens carry both theme colors"
    )
  }
})
