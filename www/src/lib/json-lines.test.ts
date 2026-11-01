import assert from "node:assert/strict"
import { test } from "node:test"
import {
  formatJsonLines,
  getJsonPathKey,
  getJsonCopyText,
  getVisibleJsonRows,
  type JsonFormatOptions,
  type JsonLine,
  type JsonPath,
} from "./json-lines"

const longString = "a".repeat(2001)

test("formatJsonLines lays out lines, fold ranges and string states", () => {
  const cases: {
    name: string
    value: unknown
    options?: JsonFormatOptions
    wantTexts?: string[]
    // 按路径找到这个节点的第一行，只比较列出的字段
    wantLines: { path: JsonPath; want: Partial<JsonLine> }[]
  }[] = [
    {
      name: "nested_object",
      value: { a: { b: 1 }, c: [1, 2] },
      wantTexts: [
        "{",
        '  "a": {',
        '    "b": 1',
        "  },",
        '  "c": [',
        "    1,",
        "    2",
        "  ]",
        "}",
      ],
      wantLines: [
        { path: [], want: { depth: 0, foldEnd: 8, childCount: 2 } },
        { path: ["a"], want: { depth: 1, foldEnd: 3, childCount: 1 } },
        { path: ["c"], want: { foldEnd: 7, childCount: 2 } },
        { path: ["c", 1], want: { depth: 2, value: 2 } },
      ],
    },
    {
      name: "empty_containers_do_not_fold",
      value: { a: {}, b: [] },
      wantTexts: ["{", '  "a": {},', '  "b": []', "}"],
      wantLines: [
        { path: ["a"], want: { foldEnd: undefined, childCount: undefined } },
        { path: ["b"], want: { foldEnd: undefined, childCount: undefined } },
      ],
    },
    {
      name: "undefined_field_skipped",
      value: { a: 1, b: undefined },
      wantTexts: ["{", '  "a": 1', "}"],
      wantLines: [{ path: [], want: { childCount: 1 } }],
    },
    {
      name: "nested_json_string",
      value: { arguments: '{"cmd":"ls"}' },
      wantTexts: ["{", '  "arguments": "{\\"cmd\\":\\"ls\\"}"', "}"],
      wantLines: [
        {
          path: ["arguments"],
          want: { isNestedJSON: true, foldEnd: undefined },
        },
      ],
    },
    {
      name: "nested_json_string_expanded",
      value: { arguments: '{"cmd":"ls"}' },
      options: {
        expandedNestedPaths: new Set([getJsonPathKey(["arguments"])]),
      },
      wantTexts: ["{", '  "arguments": {', '    "cmd": "ls"', "  }", "}"],
      wantLines: [
        {
          path: ["arguments"],
          want: {
            isNestedJSON: true,
            foldEnd: 3,
            childCount: 1,
            value: '{"cmd":"ls"}',
          },
        },
        { path: ["arguments", "cmd"], want: { depth: 2, value: "ls" } },
      ],
    },
    {
      name: "invalid_json_string",
      value: { arguments: '{"cmd":' },
      options: {
        expandedNestedPaths: new Set([getJsonPathKey(["arguments"])]),
      },
      wantTexts: ["{", '  "arguments": "{\\"cmd\\":"', "}"],
      wantLines: [{ path: ["arguments"], want: { isNestedJSON: undefined } }],
    },
    {
      name: "long_string_truncated",
      value: { s: longString },
      wantTexts: ["{", '  "s": "' + "a".repeat(2000) + '…"', "}"],
      wantLines: [
        { path: ["s"], want: { isTruncated: true, value: longString } },
      ],
    },
    {
      name: "long_string_expanded",
      value: { s: longString },
      options: { expandedStringPaths: new Set([getJsonPathKey(["s"])]) },
      wantTexts: ["{", '  "s": "' + longString + '"', "}"],
      wantLines: [{ path: ["s"], want: { isTruncated: undefined } }],
    },
  ]
  for (const { name, value, options, wantTexts, wantLines } of cases) {
    const lines = formatJsonLines(value, options ?? {})
    if (wantTexts)
      assert.deepEqual(
        lines.map((line) => line.text),
        wantTexts,
        name
      )
    for (const { path, want } of wantLines) {
      const line = lines.find(
        (line) => getJsonPathKey(line.path) === getJsonPathKey(path)
      )
      assert.ok(line, name + ": no line for " + getJsonPathKey(path))
      const got = Object.fromEntries(
        Object.keys(want).map((key) => [key, line[key as keyof JsonLine]])
      )
      assert.deepEqual(got, want, name + " " + getJsonPathKey(path))
    }
  }
})

test("viewing string JSON preserves the field copy value and exposes parsed subvalues", () => {
  for (const { text, subPath, subValue } of [
    {
      text: '{ "command": "ls" }',
      subPath: ["arguments", "command"],
      subValue: "ls",
    },
    {
      text: '[1,{"ok":true}]',
      subPath: ["arguments", 1],
      subValue: { ok: true },
    },
    { text: "{}", subPath: ["arguments"], subValue: "{}" },
  ]) {
    const value = { arguments: text }
    for (const expanded of [false, true]) {
      const lines = formatJsonLines(value, {
        expandedNestedPaths: expanded
          ? new Set([getJsonPathKey(["arguments"])])
          : undefined,
      })
      const field = lines.find(
        (line) => getJsonPathKey(line.path) === getJsonPathKey(["arguments"])
      )!
      assert.equal(getJsonCopyText(field.value), text)
      assert.equal(getJsonCopyText(value), JSON.stringify(value, null, 2))
      if (expanded) {
        const subNode = lines.find(
          (line) => getJsonPathKey(line.path) === getJsonPathKey(subPath)
        )!
        assert.deepEqual(subNode.value, subValue)
      }
    }
  }
})

test("getVisibleJsonRows folds deep containers and honors toggles", () => {
  const value = { a: { b: { c: 1 } }, s: '{"x":{"y":1}}' }
  const nested = new Set([getJsonPathKey(["s"])])
  const cases: {
    name: string
    defaultExpandDepth: number
    expandedNestedPaths?: Set<string>
    toggledFoldPaths?: JsonPath[]
    // 显示的行和折叠的行，按开头行的文本列出
    wantTexts: string[]
    wantFolded: string[]
  }[] = [
    {
      name: "default_depth_two_folds_third_level",
      defaultExpandDepth: 2,
      wantTexts: [
        "{",
        '  "a": {',
        '    "b": {',
        "  },",
        '  "s": "{\\"x\\":{\\"y\\":1}}"',
        "}",
      ],
      wantFolded: ['    "b": {'],
    },
    {
      name: "toggle_unfolds_folded_node",
      defaultExpandDepth: 2,
      toggledFoldPaths: [["a", "b"]],
      wantTexts: [
        "{",
        '  "a": {',
        '    "b": {',
        '      "c": 1',
        "    }",
        "  },",
        '  "s": "{\\"x\\":{\\"y\\":1}}"',
        "}",
      ],
      wantFolded: [],
    },
    {
      name: "toggle_folds_expanded_node",
      defaultExpandDepth: 2,
      toggledFoldPaths: [["a"]],
      wantTexts: ["{", '  "a": {', '  "s": "{\\"x\\":{\\"y\\":1}}"', "}"],
      wantFolded: ['  "a": {'],
    },
    {
      name: "depth_zero_folds_root",
      defaultExpandDepth: 0,
      wantTexts: ["{"],
      wantFolded: ["{"],
    },
    {
      name: "expanded_nested_json_starts_unfolded",
      defaultExpandDepth: 1,
      expandedNestedPaths: nested,
      wantTexts: ["{", '  "a": {', '  "s": {', '    "x": {', "  }", "}"],
      wantFolded: ['  "a": {', '    "x": {'],
    },
  ]
  for (const {
    name,
    defaultExpandDepth,
    expandedNestedPaths,
    toggledFoldPaths,
    wantTexts,
    wantFolded,
  } of cases) {
    const lines = formatJsonLines(value, { expandedNestedPaths })
    const rows = getVisibleJsonRows(
      lines,
      defaultExpandDepth,
      new Set((toggledFoldPaths ?? []).map(getJsonPathKey))
    )
    assert.deepEqual(
      rows.map((row) => lines[row.lineIndex].text),
      wantTexts,
      name
    )
    assert.deepEqual(
      rows
        .filter((row) => row.isFolded)
        .map((row) => lines[row.lineIndex].text),
      wantFolded,
      name
    )
  }
})
