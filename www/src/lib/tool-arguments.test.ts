import assert from "node:assert/strict"
import { test } from "node:test"
import {
  getArrayField,
  getBooleanField,
  getFileLanguage,
  getFirstStringField,
  getNumberField,
  getPatchFileChanges,
  getReadFileText,
  getStringArrayField,
  getStringField,
  getToolArgumentPreview,
  shortenToolPath,
  isCommandFailed,
  parseCommandResult,
  parseCommandState,
  parseToolArguments,
  stripAnsi,
  type CommandState,
} from "./tool-arguments"

test("parseToolArguments keeps unparsable text as raw", () => {
  const cases: {
    name: string
    text: string
    want: ReturnType<typeof parseToolArguments>
  }[] = [
    {
      name: "object",
      text: '{"command":"ls"}',
      want: { ok: true, value: { command: "ls" } },
    },
    {
      name: "incomplete_while_generating",
      text: '{"command":',
      want: { ok: false, raw: '{"command":' },
    },
    { name: "empty", text: "", want: { ok: false, raw: "" } },
    { name: "array", text: "[1,2]", want: { ok: true, value: [1, 2] } },
  ]
  for (const { name, text, want } of cases)
    assert.deepEqual(parseToolArguments(text), want, name)
})

test("getStringField reads only string fields of objects", () => {
  const cases: {
    name: string
    value: unknown
    field: string
    want: string | undefined
  }[] = [
    { name: "string", value: { path: "a.go" }, field: "path", want: "a.go" },
    { name: "number", value: { limit: 3 }, field: "limit", want: undefined },
    { name: "missing", value: {}, field: "path", want: undefined },
    { name: "array", value: ["a"], field: "0", want: undefined },
    { name: "null", value: null, field: "path", want: undefined },
  ]
  for (const { name, value, field, want } of cases)
    assert.equal(getStringField(value, field), want, name)
})

test("field readers fall back when Go sends null or other types", () => {
  const content = {
    added: ["a.txt", 3],
    updated: null,
    count: 2,
    label: "x",
  }
  const cases: { name: string; got: unknown; want: unknown }[] = [
    {
      name: "string_array_skips_others",
      got: getStringArrayField(content, "added"),
      want: ["a.txt"],
    },
    {
      name: "nil_slice_is_empty",
      got: getArrayField(content, "updated"),
      want: [],
    },
    { name: "missing_array", got: getArrayField(content, "deleted"), want: [] },
    { name: "array_of_null", got: getArrayField(null, "added"), want: [] },
    { name: "number", got: getNumberField(content, "count"), want: 2 },
    {
      name: "boolean_wrong_type",
      got: getBooleanField(content, "count"),
      want: undefined,
    },
    {
      name: "number_wrong_type",
      got: getNumberField(content, "label"),
      want: undefined,
    },
  ]
  for (const { name, got, want } of cases) assert.deepEqual(got, want, name)
})

test("getFirstStringField takes the first string in field order", () => {
  const cases: { name: string; value: unknown; want: string | undefined }[] = [
    { name: "first", value: { limit: 3, query: "a", path: "b" }, want: "a" },
    { name: "none", value: { limit: 3 }, want: undefined },
    { name: "not_object", value: "text", want: undefined },
  ]
  for (const { name, value, want } of cases)
    assert.equal(getFirstStringField(value), want, name)
})

test("shortenToolPath keeps two parent directories and the filename", () => {
  const cases = [
    { path: "config.ts", want: "config.ts" },
    { path: "src/lib/config.ts", want: "src/lib/config.ts" },
    { path: "./src/lib/config.ts", want: "./src/lib/config.ts" },
    { path: "/src/lib/config.ts", want: "/src/lib/config.ts" },
    { path: "~/src/lib/config.ts", want: "~/src/lib/config.ts" },
    {
      path: "/home/me/project/src/lib/config.ts",
      want: ".../src/lib/config.ts",
    },
    { path: "www/src/lib/config.ts", want: ".../src/lib/config.ts" },
    { path: "C:\\src\\lib\\config.ts", want: "C:\\src\\lib\\config.ts" },
    { path: "C:\\project\\src\\lib\\config.ts", want: ".../src/lib/config.ts" },
    {
      path: "\\\\server\\share\\src\\lib\\config.ts",
      want: ".../src/lib/config.ts",
    },
    { path: "", want: "" },
  ]
  for (const { path, want } of cases)
    assert.equal(shortenToolPath(path), want, path)
})

test("getToolArgumentPreview uses only meaningful built-in arguments", () => {
  const cases = [
    {
      name: "read_file",
      value: { path: "/repo/www/src/lib/a.ts" },
      want: ".../src/lib/a.ts",
    },
    {
      name: "bash",
      value: { command: "pnpm test\npnpm build" },
      want: "pnpm test",
    },
    {
      name: "apply_patch",
      value: {
        patch:
          "*** Begin Patch\n*** Update File: repo/src/lib/a.ts\n*** Add File: repo/src/lib/b.ts\n",
      },
      want: ".../src/lib/a.ts, .../src/lib/b.ts",
    },
    {
      name: "spawn_agent",
      value: { task_name: "审查配置", message: "message" },
      want: "审查配置",
    },
    { name: "spawn_agent", value: { message: "first\nsecond" }, want: "first" },
    {
      name: "send_message",
      value: { message: "first\r\nsecond" },
      want: "first",
    },
    {
      name: "write_stdin",
      value: { command_id: "cmd1", chars: "y\n" },
      want: 'cmd1 ← "y\\n"',
    },
    ...["read_command_output", "stop_command"].map((name) => ({
      name,
      value: { command_id: "cmd1" },
      want: "cmd1",
    })),
    ...["interrupt_agent", "read_session_history"].map((name) => ({
      name,
      value: { session_id: "session1" },
      want: "session1",
    })),
    ...[
      "mcp__server__search",
      "unknown",
      "list_agents",
      "wait_agent",
      "list_commands",
    ].map((name) => ({ name, value: { query: "must not preview" }, want: "" })),
    { name: "read_file", value: { path: 123 }, want: "" },
    { name: "bash", value: undefined, want: "" },
  ]
  for (const { name, value, want } of cases) {
    const original = structuredClone(value)
    assert.equal(getToolArgumentPreview(name, value), want, name)
    assert.deepEqual(value, original, name + " keeps raw parameters")
  }
})

test("getPatchFileChanges counts lines per file in patch order", () => {
  const cases: {
    name: string
    patch: string
    want: ReturnType<typeof getPatchFileChanges>
  }[] = [
    {
      name: "add_update_delete",
      patch: [
        "*** Begin Patch",
        "*** Add File: docs/new.md",
        "+# Title",
        "+body",
        "*** Update File: main.go",
        "@@ func main()",
        " package main",
        "-old()",
        "+first()",
        "+second()",
        "*** Delete File: old.txt",
        "*** End Patch",
      ].join("\n"),
      want: [
        { path: "docs/new.md", kind: "add", addedLines: 2, deletedLines: 0 },
        { path: "main.go", kind: "update", addedLines: 2, deletedLines: 1 },
        { path: "old.txt", kind: "delete", addedLines: 0, deletedLines: 0 },
      ],
    },
    {
      name: "still_generating",
      patch: "*** Begin Patch\n*** Update File: a.ts\n@@\n-x\n+y",
      want: [{ path: "a.ts", kind: "update", addedLines: 1, deletedLines: 1 }],
    },
    { name: "not_a_patch", patch: "hello", want: [] },
  ]
  for (const { name, patch, want } of cases)
    assert.deepEqual(getPatchFileChanges(patch), want, name)
})

test("getReadFileText removes the line number prefixes", () => {
  const cases: { name: string; content: string; want: string }[] = [
    {
      name: "numbered_lines",
      content: "1: package main\n2: \n3: 4: not a prefix\n",
      want: "package main\n\n4: not a prefix\n",
    },
    { name: "empty", content: "", want: "" },
    { name: "from_offset", content: "120: }", want: "}" },
  ]
  for (const { name, content, want } of cases)
    assert.equal(getReadFileText(content), want, name)
})

test("parseCommandResult normalizes the command result", () => {
  const cases: { name: string; content: unknown; want: unknown }[] = [
    {
      name: "command_result",
      content: {
        command_id: "cmd-1",
        status: "exited",
        exit_code: 0,
        output: "ok\n",
        next_cursor: 3,
        has_more: false,
      },
      want: {
        command_id: "cmd-1",
        status: "exited",
        exit_code: 0,
        stop_reason: undefined,
        error: undefined,
        output: "ok\n",
        has_more: false,
        truncated: false,
      },
    },
    {
      name: "running_without_exit_code",
      content: { command_id: "cmd-2", status: "running", output: "" },
      want: {
        command_id: "cmd-2",
        status: "running",
        exit_code: null,
        stop_reason: undefined,
        error: undefined,
        output: "",
        has_more: false,
        truncated: false,
      },
    },
    {
      name: "stop_command_result",
      content: { command_id: "cmd-1", status: "exited", exit_code: -1 },
      want: undefined,
    },
    { name: "tool_error", content: null, want: undefined },
  ]
  for (const { name, content, want } of cases)
    assert.deepEqual(parseCommandResult(content), want, name)
})

test("parseCommandState needs a status", () => {
  const cases: { name: string; value: unknown; want: unknown }[] = [
    {
      name: "stopped",
      value: {
        command_id: "cmd-1",
        status: "exited",
        exit_code: -1,
        stop_reason: "stopped",
      },
      want: {
        command_id: "cmd-1",
        status: "exited",
        exit_code: -1,
        stop_reason: "stopped",
      },
    },
    {
      name: "null_exit_code",
      value: { command_id: "cmd-1", status: "running", exit_code: null },
      want: {
        command_id: "cmd-1",
        status: "running",
        exit_code: null,
        stop_reason: undefined,
      },
    },
    { name: "no_status", value: { command_id: "cmd-1" }, want: undefined },
  ]
  for (const { name, value, want } of cases)
    assert.deepEqual(parseCommandState(value), want, name)
})

test("isCommandFailed follows the TUI rule", () => {
  const result = (fields: Partial<CommandState>): CommandState => ({
    command_id: "cmd-1",
    status: "exited",
    exit_code: 0,
    ...fields,
  })
  const cases: { name: string; result: CommandState; want: boolean }[] = [
    { name: "exit_zero", result: result({}), want: false },
    { name: "exit_nonzero", result: result({ exit_code: 2 }), want: true },
    {
      name: "start_failed",
      result: result({ status: "failed", exit_code: null }),
      want: true,
    },
    {
      name: "stopped_by_tool",
      result: result({ exit_code: -1, stop_reason: "stopped" }),
      want: false,
    },
    {
      name: "timed_out",
      result: result({ exit_code: -1, stop_reason: "timed_out" }),
      want: true,
    },
    {
      name: "still_running",
      result: result({ status: "running", exit_code: null }),
      want: false,
    },
  ]
  for (const { name, result, want } of cases)
    assert.equal(isCommandFailed(result), want, name)
})

test("stripAnsi keeps only the text", () => {
  const cases: { name: string; text: string; want: string }[] = [
    {
      name: "colors",
      text: "\u001b[1;32mok\u001b[0m done",
      want: "ok done",
    },
    { name: "title", text: "\u001b]0;title\u0007x", want: "x" },
    {
      name: "osc8_hyperlink_with_st_terminator",
      text: "\u001b]8;;file:///tmp/file.txt\u001b\\file.txt\u001b]8;;\u001b\\\n",
      want: "file.txt\n",
    },
    { name: "plain", text: "plain", want: "plain" },
  ]
  for (const { name, text, want } of cases)
    assert.equal(stripAnsi(text), want, name)
})

test("getFileLanguage uses the extension or the file name", () => {
  const cases: { path: string; want: string }[] = [
    { path: "pkg/agent/agent.go", want: "go" },
    { path: "www/src/a.test.ts", want: "ts" },
    { path: "/repo/Dockerfile", want: "dockerfile" },
    { path: ".gitignore", want: ".gitignore" },
  ]
  for (const { path, want } of cases)
    assert.equal(getFileLanguage(path), want, path)
})
