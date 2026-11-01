import assert from "node:assert/strict"
import { test } from "node:test"
import { createInstance } from "i18next"
import zhCN from "../i18n/locales/zh-CN.json"
import en from "../i18n/locales/en.json"
import { FallbackItem } from "../components/conversation/items/fallback"
import { BashTool } from "../components/conversation/tools/bash"
import { DefaultTool } from "../components/conversation/tools/default"
import type { ViewItem } from "./api/conversation-items"
import type { Item, ToolCall, ToolResult, Turn } from "./api/protocol.gen"
import {
  getItemRenderer,
  getMessageRows,
  getMessageSource,
  getTurnResult,
  getReplyToCopy,
  getProcessFacts,
  getProcessActionText,
  type ProcessAction,
  isTurnRangeLoaded,
  getToolCallState,
  getToolDisplayName,
  getToolRenderer,
  type ItemRow,
  type MessageRow,
} from "./item-renderers"

test("getItemRenderer falls back for unknown kinds", () => {
  const cases: { kind: string; isFallback: boolean }[] = [
    { kind: "message", isFallback: false },
    { kind: "tool_call", isFallback: false },
    { kind: "tool_result", isFallback: false },
    { kind: "turn_interrupted", isFallback: false },
    { kind: "future_kind", isFallback: true },
  ]
  for (const { kind, isFallback } of cases)
    assert.equal(getItemRenderer(kind) === FallbackItem, isFallback, kind)
})

test("getToolRenderer shows MCP and unknown tools as JSON", () => {
  const cases: { name: string; want: unknown }[] = [
    { name: "bash", want: BashTool },
    { name: "mcp__serena__find", want: DefaultTool },
    { name: "unknown", want: DefaultTool },
  ]
  for (const { name, want } of cases)
    assert.equal(getToolRenderer(name), want, name)
})

test("getToolDisplayName splits MCP server and tool", () => {
  const cases: { name: string; want: string }[] = [
    { name: "bash", want: "bash" },
    { name: "mcp__serena__find_symbol", want: "serena/find_symbol" },
    { name: "mcp__broken", want: "mcp__broken" },
  ]
  for (const { name, want } of cases)
    assert.equal(getToolDisplayName(name), want, name)
})

test("getToolCallState covers the five card states", () => {
  const call = (status?: ToolCall["status"]): ToolCall => ({
    call_id: "c1",
    name: "bash",
    arguments: "{}",
    status,
  })
  const ok: ToolResult = { call_id: "c1", content: {} }
  const failed: ToolResult = {
    call_id: "c1",
    error: { code: "timeout", message: "timed out" },
  }
  const cases: {
    name: string
    call: ToolCall
    result?: ToolResult
    isStreaming: boolean
    isTurnRunning: boolean
    want: ReturnType<typeof getToolCallState>
  }[] = [
    {
      name: "arguments_generating",
      call: call(),
      isStreaming: true,
      isTurnRunning: true,
      want: "generating",
    },
    {
      name: "executing",
      call: call("ready"),
      isStreaming: false,
      isTurnRunning: true,
      want: "running",
    },
    {
      name: "succeeded",
      call: call("ready"),
      result: ok,
      isStreaming: false,
      isTurnRunning: false,
      want: "succeeded",
    },
    {
      name: "failed",
      call: call("ready"),
      result: failed,
      isStreaming: false,
      isTurnRunning: false,
      want: "failed",
    },
    {
      name: "interrupted",
      call: call("interrupted"),
      isStreaming: false,
      isTurnRunning: false,
      want: "interrupted",
    },
    {
      name: "turn_ended_without_result",
      call: call("ready"),
      isStreaming: false,
      isTurnRunning: false,
      want: "interrupted",
    },
  ]
  for (const { name, call, result, isStreaming, isTurnRunning, want } of cases)
    assert.equal(
      getToolCallState(call, result, isStreaming, isTurnRunning),
      want,
      name
    )
})

test("getMessageSource tells user input, model reply and agent messages apart", () => {
  const message = (
    from: "user" | "agent",
    extra: Partial<Item> = {}
  ): Item => ({
    id: "m",
    turn_id: "t1",
    kind: "message",
    message: { from: { kind: from, id: "s" }, to: null, content: [] },
    ...extra,
  })
  const cases: {
    name: string
    item: Item
    want: ReturnType<typeof getMessageSource>
  }[] = [
    { name: "user", item: message("user"), want: "user" },
    {
      name: "reply",
      item: message("agent", { model_call_id: "mc1" }),
      want: "reply",
    },
    { name: "other_agent", item: message("agent"), want: "agent" },
  ]
  for (const { name, item, want } of cases)
    assert.equal(getMessageSource(item), want, name)
})

test("getMessageRows retains Item references, order, CallID pairing and stable page seams", () => {
  const reasoning = view("reasoning", "t1", "reasoning")
  const call = view("call", "t1", "tool_call", "c1")
  const result = view("result", "t1", "tool_result", "c1")
  const call2 = view("call2", "t1", "tool_call", "c2")
  const reply = view("reply", "t1", "message")
  reply.item.model_call_id = "model-call"
  const user = view("user", "t1", "message")
  user.item.message = {
    from: { kind: "user", id: "user" },
    to: null,
    content: [],
  }
  const letter = view("letter", "t1", "message")
  letter.item.message = {
    from: { kind: "agent", id: "other-agent" },
    to: null,
    content: [],
  }
  const change = view("change", "t1", "model_change")
  const user2 = view("user2", "t2", "message")
  const orphan = view("orphan", "t0", "tool_result", "c0")
  const fork = view("fork", "", "session_fork")
  const cases = [
    {
      name: "pair_and_preserve_call_order",
      items: [reasoning, call, call2, result, reply],
      want: ["t1:[reasoning,call+result,call2]|reply"],
    },
    ...[
      reply,
      user,
      letter,
      view("hook", "t1", "hook_run"),
      view("skill", "t1", "skill_instruction"),
      view("agents", "t1", "agents_instruction"),
    ].map((separator) => ({
      name: separator.key + "_breaks_group",
      items: [reasoning, call, separator, call2],
      want: ["t1:[reasoning,call]|" + separator.key + "|[call2]"],
    })),
    {
      name: "change_breaks_group",
      items: [call, change, call2],
      want: ["t1:[call]|change|[call2]"],
    },
    {
      name: "new_turn_breaks_group",
      items: [call, user2],
      want: ["t1:[call]", "t2:user2"],
    },
    {
      name: "unknown_and_orphan_remain",
      items: [
        orphan,
        fork,
        view("unknown", "t1", "future_kind" as Item["kind"]),
      ],
      want: ["t0:[orphan]", "fork", "t1:unknown"],
    },
    {
      name: "page_seam_preserves_existing_group_start",
      items: [reasoning, call, call2],
      seams: new Set(["call"]),
      want: ["t1:[reasoning]|[call,call2]"],
    },
    {
      name: "interleaved_turns_keep_order",
      items: [reply, user2, call],
      want: ["t1:reply", "t2:user2", "t1:[call]"],
    },
  ]
  for (const { name, items, seams, want } of cases) {
    const rows = getMessageRows(items, seams)
    const label = (row: { view: ViewItem; toolResult?: ViewItem }) =>
      row.view.key + (row.toolResult ? "+" + row.toolResult.key : "")
    assert.deepEqual(
      rows.map((row) =>
        row.kind === "item"
          ? label(row)
          : row.turnID +
            ":" +
            row.rows
              .map((inner) =>
                inner.kind === "group"
                  ? "[" + inner.rows.map(label).join(",") + "]"
                  : label(inner)
              )
              .join("|")
      ),
      want,
      name
    )
    for (const row of rows) {
      const members =
        row.kind === "turn"
          ? row.rows.flatMap((inner) =>
              inner.kind === "group" ? inner.rows : [inner]
            )
          : [row]
      for (const member of members)
        assert.equal(items.includes(member.view), true, name)
    }
  }
})

test("getMessageRows reuses unchanged rows and invalidates changed members and page seams", () => {
  const fork = view("fork", "", "session_fork")
  const history = view("history", "history", "message")
  const reasoning = view("reasoning", "t1", "reasoning")
  const call = view("call", "t1", "tool_call", "c1")
  const reply = view("reply", "t1", "message")
  const tail = view("tail", "t1", "reasoning")
  const result = view("result", "t1", "tool_result", "c1")
  const error = view("error", "errors", "model_error")
  const laterError = {
    ...view("later-error", "errors", "model_error"),
    ordinal: 2,
  }
  const initial = [fork, history, reasoning, call, reply, tail]
  const stableHistory = [
    "item:fork",
    "turn:history:0",
    "turn:history:0:items",
    "turn:history:0:rows",
  ]
  const cases: {
    name: string
    before: ViewItem[]
    after: ViewItem[]
    beforeSeams?: Set<string>
    afterSeams?: Set<string>
    same: string[]
    changed: string[]
  }[] = [
    {
      name: "identical_members_reuse_all_arrays",
      before: initial,
      after: [...initial],
      same: [
        "rows",
        ...stableHistory,
        "turn:t1:0",
        "turn:t1:0:items",
        "turn:t1:0:rows",
        "group:reasoning",
        "group:reasoning:rows",
      ],
      changed: [],
    },
    {
      name: "delta_only_changes_tail_and_its_turn",
      before: initial,
      after: [
        ...initial.slice(0, -1),
        {
          ...tail,
          isStreaming: true,
          item: {
            ...tail.item,
            reasoning: { status: "completed", summary: ["delta"], content: [] },
          },
        },
      ],
      same: [
        ...stableHistory,
        "group:reasoning",
        "group:reasoning:rows",
        "item:reasoning",
        "item:call",
        "item:reply",
      ],
      changed: [
        "rows",
        "turn:t1:0",
        "turn:t1:0:items",
        "turn:t1:0:rows",
        "group:tail",
        "group:tail:rows",
        "item:tail",
      ],
    },
    {
      name: "result_arrival_replaces_call_row_without_changing_call_view",
      before: initial,
      after: [...initial, result],
      same: [...stableHistory, "item:reasoning", "item:reply", "group:tail"],
      changed: [
        "turn:t1:0",
        "group:reasoning",
        "group:reasoning:rows",
        "item:call",
      ],
    },
    {
      name: "result_update_replaces_pair_without_changing_call_view",
      before: [...initial, result],
      after: [
        ...initial,
        {
          ...result,
          item: {
            ...result.item,
            tool_result: { call_id: "c1", content: "updated" },
          },
        },
      ],
      same: [...stableHistory, "item:reasoning", "item:reply", "group:tail"],
      changed: ["turn:t1:0", "group:reasoning", "item:call"],
    },
    {
      name: "prepend_another_turn_keeps_all_loaded_turns",
      before: initial,
      after: [view("older", "older", "message"), ...initial],
      afterSeams: new Set(["fork"]),
      same: [
        ...stableHistory,
        "turn:t1:0",
        "turn:t1:0:items",
        "turn:t1:0:rows",
      ],
      changed: ["rows"],
    },
    {
      name: "prepend_same_turn_preserves_existing_group_at_seam",
      before: [reasoning, call, reply, tail],
      after: [view("older", "t1", "reasoning"), reasoning, call, reply, tail],
      afterSeams: new Set(["reasoning"]),
      same: [
        "group:reasoning",
        "group:reasoning:rows",
        "item:call",
        "item:reply",
        "group:tail",
      ],
      changed: ["turn:t1:0", "turn:t1:0:items", "turn:t1:0:rows"],
    },
    {
      name: "introducing_error_seam_splits_existing_group",
      before: [error, laterError],
      after: [error, laterError],
      afterSeams: new Set(["later-error"]),
      same: ["turn:errors:0:items", "item:error", "item:later-error"],
      changed: ["turn:errors:0", "group:error", "group:error:rows"],
    },
    {
      name: "removing_error_seam_merges_existing_groups",
      before: [error, laterError],
      after: [error, laterError],
      beforeSeams: new Set(["later-error"]),
      same: ["turn:errors:0:items", "item:error", "item:later-error"],
      changed: ["turn:errors:0", "group:error"],
    },
    {
      name: "interrupted_call_changes_only_its_group",
      before: initial,
      after: [
        fork,
        history,
        reasoning,
        {
          ...call,
          item: {
            ...call.item,
            tool_call: { ...call.item.tool_call!, status: "interrupted" },
          },
        },
        reply,
        tail,
      ],
      same: [...stableHistory, "item:reasoning", "item:reply", "group:tail"],
      changed: ["turn:t1:0", "group:reasoning", "item:call"],
    },
    {
      name: "no_turn_item_keeps_unrelated_turns",
      before: initial,
      after: [{ ...fork, item: { ...fork.item } }, ...initial.slice(1)],
      same: ["turn:history:0", "turn:t1:0"],
      changed: ["rows", "item:fork"],
    },
    {
      name: "interleaved_turn_segments_retain_original_keys",
      before: [reasoning, history, tail],
      after: [reasoning, history, { ...tail, isStreaming: true }],
      same: ["turn:t1:0", "turn:history:0"],
      changed: ["turn:t1:1", "item:tail"],
    },
    {
      name: "history_call_pairs_previously_orphan_result",
      before: [history, result],
      after: [call, history, result],
      same: ["turn:history:0"],
      changed: ["turn:t1:0"],
    },
  ]
  for (const {
    name,
    before,
    after,
    beforeSeams = new Set<string>(),
    afterSeams = new Set<string>(),
    same,
    changed,
  } of cases) {
    const previous = getMessageRows(before, beforeSeams)
    const beforeReferences = getRowReferences(previous)
    const next = getMessageRows(after, afterSeams, {
      rows: previous,
      pageSeams: beforeSeams,
    })
    assert.deepEqual(next, getMessageRows(after, afterSeams), name)
    assert.deepEqual(
      previous,
      getMessageRows(before, beforeSeams),
      name + ": previous rows stay unchanged"
    )
    const afterReferences = getRowReferences(next)
    for (const key of same) {
      assert.ok(
        beforeReferences.has(key) && afterReferences.has(key),
        name + ": " + key
      )
      assert.equal(
        afterReferences.get(key),
        beforeReferences.get(key),
        name + ": " + key
      )
    }
    for (const key of changed) {
      assert.ok(
        beforeReferences.has(key) && afterReferences.has(key),
        name + ": " + key
      )
      assert.notEqual(
        afterReferences.get(key),
        beforeReferences.get(key),
        name + ": " + key
      )
    }
  }
})

function getRowReferences(rows: MessageRow[]): Map<string, object> {
  const references = new Map<string, object>([["rows", rows]])
  for (const row of rows) {
    if (row.kind === "item") references.set("item:" + row.view.key, row)
    else {
      references.set("turn:" + row.key, row)
      references.set("turn:" + row.key + ":items", row.items)
      references.set("turn:" + row.key + ":rows", row.rows)
      for (const content of row.rows) {
        if (content.kind === "item")
          references.set("item:" + content.view.key, content)
        else {
          references.set("group:" + content.key, content)
          references.set("group:" + content.key + ":rows", content.rows)
          for (const member of content.rows)
            references.set("item:" + member.view.key, member)
        }
      }
    }
  }
  return references
}

test("getReplyToCopy only selects the last finished reply without requiring full history", () => {
  const first: ViewItem = {
    ...view("first", "t1", "message"),
    ordinal: 4290,
    item: {
      id: "first",
      turn_id: "t1",
      kind: "message",
      model_call_id: "reply-call",
      message: {
        from: { kind: "agent", id: "s" },
        to: null,
        content: [{ kind: "text", text: { text: "first reply" } }],
      },
    },
  }
  const last: ViewItem = {
    ...first,
    key: "last",
    ordinal: 4291,
    turnID: "t2",
    item: { ...first.item, id: "last", turn_id: "t2" },
  }
  const cases: { name: string; items: ViewItem[]; want?: string }[] = [
    { name: "empty_history", items: [] },
    { name: "latest_page_only", items: [last], want: "last" },
    {
      name: "one_copy_across_multiple_turns",
      items: [first, last],
      want: "last",
    },
    {
      name: "earlier_gap_does_not_block_copy",
      items: [{ ...first, ordinal: 20 }, last],
      want: "last",
    },
    {
      name: "new_reply_still_streaming",
      items: [first, { ...last, isStreaming: true }],
    },
    {
      name: "new_reply_not_saved",
      items: [first, { ...last, ordinal: undefined }],
    },
    {
      name: "later_tool_does_not_replace_reply",
      items: [last, view("tool", "t1", "tool_call", "call")],
      want: "last",
    },
    {
      name: "user_is_not_a_model_reply",
      items: [
        {
          ...last,
          item: {
            ...last.item,
            message: {
              ...last.item.message!,
              from: { kind: "user", id: "user" },
            },
          },
        },
      ],
    },
    ...["", "  "].map((text) => ({
      name: `empty_last_reply_${JSON.stringify(text)}`,
      items: [
        first,
        {
          ...last,
          item: {
            ...last.item,
            message: {
              ...last.item.message!,
              content: [{ kind: "text" as const, text: { text } }],
            },
          },
        },
      ],
    })),
  ]
  for (const { name, items, want } of cases)
    assert.equal(getReplyToCopy(items)?.key, want, name)
})

test("getTurnResult only exposes all messages from a complete successful final call", () => {
  const progress = view("progress", "t1", "message")
  progress.item = {
    ...progress.item,
    model_call_id: "progress",
    message: {
      from: { kind: "agent", id: "s" },
      to: null,
      content: [{ kind: "text", text: { text: "progress" } }],
    },
  }
  const first = view("first", "t1", "message")
  first.item = {
    ...first.item,
    model_call_id: "final",
    message: {
      from: { kind: "agent", id: "s" },
      to: null,
      content: [{ kind: "text", text: { text: "first" } }],
    },
  }
  const second = view("second", "t1", "message")
  second.item = {
    ...second.item,
    model_call_id: "final",
    message: {
      from: { kind: "agent", id: "s" },
      to: null,
      content: [{ kind: "text", text: { text: "second" } }],
    },
  }
  progress.ordinal = 10
  first.ordinal = 11
  second.ordinal = 12
  const items = [progress, first, second]
  const turn: Turn = {
    id: "t1",
    conversation_id: "c",
    session_id: "s",
    status: "completed",
    final_model_call_id: "final",
    items_range: { first_ordinal: 10, last_ordinal: 12 },
  }
  const cases: {
    name: string
    items: ViewItem[]
    turn?: Turn
    want: string[]
  }[] = [
    { name: "all_final_messages", items, turn, want: ["first", "second"] },
    { name: "metadata_not_loaded", items, want: [] },
    {
      name: "old_record",
      items,
      turn: { ...turn, final_model_call_id: undefined },
      want: [],
    },
    { name: "failed", items, turn: { ...turn, status: "failed" }, want: [] },
    {
      name: "interrupted",
      items,
      turn: { ...turn, status: "interrupted" },
      want: [],
    },
    { name: "range_partially_loaded", items: items.slice(1), turn, want: [] },
    { name: "range_has_gap", items: [progress, second], turn, want: [] },
    {
      name: "unfinished_member",
      items: [progress, { ...first, isStreaming: true }, second],
      turn,
      want: [],
    },
    {
      name: "unsaved_member",
      items: [progress, first, { ...second, ordinal: undefined }],
      turn,
      want: [],
    },
    {
      name: "empty_final_does_not_fall_back_to_progress",
      items,
      turn: { ...turn, final_model_call_id: "empty" },
      want: [],
    },
    {
      name: "final_with_tool_call",
      items: [
        progress,
        first,
        {
          ...second,
          item: {
            ...second.item,
            kind: "tool_call",
            tool_call: { call_id: "c", name: "bash", arguments: "{}" },
          },
        },
      ],
      turn,
      want: [],
    },
  ]
  for (const { name, items, turn, want } of cases)
    assert.deepEqual(
      getTurnResult(items, turn).map((view) => view.key),
      want,
      name
    )
  assert.equal(isTurnRangeLoaded(items, turn), true)
  assert.deepEqual(getTurnResult(items, turn, { status: "running" }), [])
  assert.deepEqual(
    getTurnResult(items, turn, {
      status: "completed",
      finalModelCallID: "final",
    }),
    [first, second]
  )
})

test("model errors group only across adjacent records in one loaded turn segment", () => {
  const error = (key: string, ordinal: number, turnID = "t1"): ViewItem => ({
    ...view(key, turnID, "model_error"),
    ordinal,
    item: {
      id: key,
      turn_id: turnID,
      kind: "model_error",
      model_error: { code: key, message: "failure " + key },
    },
  })
  const first = error("first", 2)
  const second = error("second", 3)
  const call = view("call", "t1", "tool_call", "c")
  const pairedResult = view("result", "t1", "tool_result", "c")
  const cases = [
    {
      name: "different_errors_remain_members",
      items: [first, second],
      want: [["first", "second"]],
    },
    {
      name: "ordinary_item_breaks_group",
      items: [first, view("notice", "t1", "model_change"), second],
      want: [["first"], ["second"]],
    },
    {
      name: "paired_result_still_breaks_raw_adjacency",
      items: [call, first, pairedResult, second],
      want: [["first"], ["second"]],
    },
    {
      name: "turns_are_separate",
      items: [first, error("other", 3, "t2")],
      want: [["first"], ["other"]],
    },
    {
      name: "missing_history_breaks_group",
      items: [first, error("later", 4)],
      want: [["first"], ["later"]],
    },
    {
      name: "loaded_page_seam_preserves_group_identity",
      items: [first, second],
      seams: new Set(["second"]),
      want: [["first"], ["second"]],
    },
    {
      name: "tools_and_errors_have_separate_groups",
      items: [call, first, second],
      want: [["first", "second"]],
    },
  ]
  for (const { name, items, seams, want } of cases) {
    const groups = getMessageRows(items, seams).flatMap((row) =>
      row.kind === "turn"
        ? row.rows.flatMap((content) =>
            content.kind === "group" && content.groupKind === "model_errors"
              ? [content]
              : []
          )
        : []
    )
    assert.deepEqual(
      groups.map((group) => group.rows.map((row) => row.view.key)),
      want,
      name
    )
    for (const group of groups) {
      assert.equal(group.key, group.rows[0].view.key, name)
      for (const row of group.rows)
        assert.equal(items.includes(row.view), true, name)
    }
  }
})

test("process facts separate group phase from the last actual action", () => {
  const call = (
    id: string,
    name: string,
    result?: ToolResult,
    status: ToolCall["status"] = "ready"
  ): ItemRow => ({
    kind: "item",
    view: {
      ...view(id, "t", "tool_call", id),
      item: {
        ...view(id, "t", "tool_call", id).item,
        tool_call: { call_id: id, name, arguments: "{}", status },
      },
    },
    toolResult: result
      ? {
          ...view(id + "-result", "t", "tool_result", id),
          item: {
            ...view(id + "-result", "t", "tool_result", id).item,
            tool_result: result,
          },
        }
      : undefined,
  })
  const reasoning = (
    isStreaming: boolean,
    status: "completed" | "interrupted" = "completed"
  ): ItemRow => ({
    kind: "item",
    view: {
      ...view("reasoning", "t", "reasoning"),
      isStreaming,
      item: {
        ...view("reasoning", "t", "reasoning").item,
        reasoning: { status, summary: [], content: [] },
      },
    },
  })
  const succeeded = call("read", "read_file", {
    call_id: "read",
    content: "ok",
  })
  const running = call("run", "bash")
  const generating = call("generate", "mcp__server__search")
  generating.view.isStreaming = true
  delete generating.view.item.tool_call!.status
  const failed = call("failed", "bash", {
    call_id: "failed",
    content: { status: "exited", exit_code: 2, output: "" },
  })
  const interrupted = call("interrupted", "bash", undefined, "interrupted")
  const facts = (
    toolCount: number,
    state: ReturnType<typeof getProcessFacts>["state"],
    action?: ProcessAction,
    reasoningCount = 0,
    hasFailures = false,
    hasInterruptions = state === "interrupted"
  ): ReturnType<typeof getProcessFacts> => ({
    toolCount,
    reasoningCount,
    state,
    action,
    hasFailures,
    hasInterruptions,
  })
  const cases: {
    name: string
    rows: ItemRow[]
    isTurnRunning: boolean
    isTailGroup: boolean
    expected: ReturnType<typeof getProcessFacts>
  }[] = [
    {
      name: "finished_old_group_does_not_follow_running_turn",
      rows: [reasoning(false), succeeded],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(1, "succeeded", undefined, 1),
    },
    {
      name: "tail_group_waits_without_fabricating_an_action",
      rows: [succeeded],
      isTurnRunning: true,
      isTailGroup: true,
      expected: facts(1, "running"),
    },
    {
      name: "tail_group_stops_at_turn_end",
      rows: [succeeded],
      isTurnRunning: false,
      isTailGroup: true,
      expected: facts(1, "succeeded"),
    },
    {
      name: "old_running_tool_is_action_not_the_newer_success",
      rows: [running, succeeded],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(2, "running", { view: running.view, phase: "running" }),
    },
    {
      name: "last_running_tool_wins",
      rows: [running, generating, succeeded],
      isTurnRunning: true,
      isTailGroup: true,
      expected: facts(3, "running", {
        view: generating.view,
        phase: "generating",
      }),
    },
    {
      name: "streaming_reasoning_is_actual_action_even_before_success",
      rows: [running, reasoning(true), succeeded],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(
        2,
        "running",
        { view: reasoning(true).view, phase: "thinking" },
        1
      ),
    },
    {
      name: "later_running_tool_replaces_streaming_reasoning_action",
      rows: [reasoning(true), running],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(
        1,
        "running",
        { view: running.view, phase: "running" },
        1
      ),
    },
    {
      name: "completed_reasoning_is_not_an_action",
      rows: [reasoning(false)],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(0, "succeeded", undefined, 1),
    },
    {
      name: "tail_reasoning_waits_without_thinking_label",
      rows: [reasoning(false)],
      isTurnRunning: true,
      isTailGroup: true,
      expected: facts(0, "running", undefined, 1),
    },
    {
      name: "turn_end_removes_stale_streaming_action",
      rows: [reasoning(true), generating],
      isTurnRunning: false,
      isTailGroup: true,
      expected: facts(1, "interrupted", undefined, 1),
    },
    {
      name: "failure_keeps_running_while_another_tool_executes",
      rows: [failed, running],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(
        2,
        "running",
        { view: running.view, phase: "running" },
        0,
        true
      ),
    },
    {
      name: "failed_tail_stays_running_while_waiting",
      rows: [failed, succeeded],
      isTurnRunning: true,
      isTailGroup: true,
      expected: facts(2, "running", undefined, 0, true),
    },
    {
      name: "finished_group_keeps_any_member_failure",
      rows: [failed, succeeded],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(2, "failed", undefined, 0, true),
    },
    {
      name: "failure_precedes_interruption_when_stopped",
      rows: [interrupted, failed],
      isTurnRunning: false,
      isTailGroup: true,
      expected: facts(2, "failed", undefined, 0, true, true),
    },
    {
      name: "interrupted_member_sets_group_state",
      rows: [interrupted, succeeded],
      isTurnRunning: false,
      isTailGroup: false,
      expected: facts(2, "interrupted"),
    },
    {
      name: "reasoning_interruption_sets_group_state",
      rows: [reasoning(false, "interrupted")],
      isTurnRunning: false,
      isTailGroup: true,
      expected: facts(0, "interrupted", undefined, 1),
    },
    {
      name: "interrupted_streaming_reasoning_is_not_an_action",
      rows: [reasoning(true, "interrupted")],
      isTurnRunning: true,
      isTailGroup: false,
      expected: facts(0, "interrupted", undefined, 1),
    },
    {
      name: "each_reasoning_item_counts_once_despite_streaming_chunks",
      rows: [
        reasoning(false),
        {
          ...reasoning(true),
          view: {
            ...reasoning(true).view,
            key: "second-reasoning",
            item: {
              ...reasoning(true).view.item,
              id: "second-reasoning",
              reasoning: {
                status: "completed",
                summary: ["first", "second", "third"],
                content: [],
              },
            },
          },
        },
      ],
      isTurnRunning: false,
      isTailGroup: false,
      expected: facts(0, "succeeded", undefined, 2),
    },
    {
      name: "tool_error_sets_group_failure",
      rows: [
        call("error", "read_file", {
          call_id: "error",
          error: { code: "missing", message: "missing file" },
        }),
      ],
      isTurnRunning: false,
      isTailGroup: true,
      expected: facts(1, "failed", undefined, 0, true),
    },
    ...(["bash", "read_command_output", "stop_command"] as const).flatMap(
      (name) =>
        [
          { stop_reason: "stopped", state: "succeeded" as const },
          { stop_reason: "timed_out", state: "failed" as const },
        ].map(({ stop_reason, state }) => ({
          name: name + "_" + stop_reason,
          rows: [
            call("command", name, {
              call_id: "command",
              content: { status: "exited", exit_code: -1, stop_reason },
            }),
          ],
          isTurnRunning: false,
          isTailGroup: true,
          expected: facts(1, state, undefined, 0, state === "failed"),
        }))
    ),
    ...[true, false].map((isTurnRunning) => ({
      name: "orphan_failure_remains_visible_" + isTurnRunning,
      rows: [
        {
          kind: "item" as const,
          view: {
            ...view("orphan-error", "t", "tool_result", "orphan"),
            item: {
              ...view("orphan-error", "t", "tool_result", "orphan").item,
              tool_result: {
                call_id: "orphan",
                error: { code: "missing", message: "missing file" },
              },
            },
          },
        },
      ],
      isTurnRunning,
      isTailGroup: true,
      expected: facts(
        0,
        isTurnRunning ? "running" : "failed",
        undefined,
        0,
        true
      ),
    })),
    {
      name: "orphan_result_does_not_count_as_a_tool_call",
      rows: [
        { kind: "item", view: view("result", "t", "tool_result", "orphan") },
      ],
      isTurnRunning: false,
      isTailGroup: true,
      expected: facts(0, "succeeded"),
    },
  ]
  for (const { name, rows, isTurnRunning, isTailGroup, expected } of cases)
    assert.deepEqual(
      getProcessFacts(rows, isTurnRunning, isTailGroup),
      expected,
      name
    )
})

test("process action previews follow real phases and explicit arguments", async () => {
  const translation = createInstance()
  await translation.init({
    lng: "zh-CN",
    resources: { "zh-CN": { translation: zhCN }, en: { translation: en } },
    interpolation: { escapeValue: false },
  })
  const tool = (
    name: string,
    args: unknown,
    phase: ProcessAction["phase"] = "running"
  ): ProcessAction => ({
    phase,
    view: {
      ...view("call", "turn", "tool_call", "call"),
      item: {
        id: "call",
        turn_id: "turn",
        kind: "tool_call",
        tool_call: {
          call_id: "call",
          name,
          arguments: typeof args === "string" ? args : JSON.stringify(args),
        },
      },
    },
  })
  const thinking = (
    summary: string[],
    content: string[] = []
  ): ProcessAction => ({
    phase: "thinking",
    view: {
      ...view("reason", "turn", "reasoning"),
      item: {
        id: "reason",
        turn_id: "turn",
        kind: "reasoning",
        reasoning: { status: "completed", summary, content },
      },
    },
  })
  const cases: {
    name: string
    action?: ProcessAction
    options?: { recipientName?: string; agentName?: string }
    want: string
    language?: string
  }[] = [
    { name: "no_action", want: "" },
    {
      name: "reading",
      action: tool("read_file", { path: "/repo/www/src/lib/a.ts" }),
      want: "读取 .../src/lib/a.ts",
    },
    {
      name: "preparing_read",
      action: tool("read_file", { path: "a.ts" }, "generating"),
      want: "准备读取 a.ts",
    },
    {
      name: "incomplete_json_has_no_preview",
      action: tool("read_file", '{"path":"partial', "generating"),
      want: "准备读取",
    },
    {
      name: "name_not_received",
      action: tool("", {}, "generating"),
      want: "准备工具调用中",
    },
    {
      name: "command",
      action: tool("bash", { command: "pnpm test\npnpm build" }),
      want: "执行 pnpm test",
    },
    ...(["generating", "running"] as const).map((phase) => ({
      name: "command_keeps_argument_spaces_" + phase,
      action: tool("bash", { command: 'printf "a  b"\nprintf "next"' }, phase),
      want: (phase === "generating" ? "准备" : "") + '执行 printf "a  b"',
    })),
    {
      name: "mcp_does_not_guess",
      action: tool("mcp__server__search", { query: "secret" }),
      want: "server/search",
    },
    {
      name: "mcp_preparing",
      action: tool("mcp__server__search", { query: "secret" }, "generating"),
      want: "准备server/search",
    },
    {
      name: "unknown_does_not_guess",
      action: tool("other", { message: "secret" }),
      want: "other",
    },
    {
      name: "send_resolved_name",
      action: tool("send_message", {
        session_id: "s1",
        message: "检查测试\n详细描述",
      }),
      options: { recipientName: "aa" },
      want: "发送给 aa 检查测试",
    },
    {
      name: "send_id_fallback",
      action: tool("send_message", { session_id: "s1", message: "检查测试" }),
      want: "发送给 s1 检查测试",
    },
    {
      name: "delegate_explicit_agent",
      action: tool("spawn_agent", { agent_name: "cc", task_name: "审查配置" }),
      options: { agentName: "default" },
      want: "委派给 cc 审查配置",
    },
    {
      name: "delegate_inherited_agent",
      action: tool("spawn_agent", { message: "审查配置\n后续" }),
      options: { agentName: "default" },
      want: "委派给 default 审查配置",
    },
    {
      name: "delegate_without_known_agent",
      action: tool("spawn_agent", { task_name: "审查配置" }),
      want: "委派任务 审查配置",
    },
    {
      name: "summary_preferred",
      action: thinking(["first\n", "**检查配置**\n\n"], ["ignored"]),
      want: "思考中 检查配置",
    },
    {
      name: "body_fallback",
      action: thinking([], ["# Earlier\n", "- 查看 `src/config.ts`\n"]),
      want: "思考中 查看 src/config.ts",
    },
    {
      name: "empty_summary_falls_back",
      action: thinking(["  "], ["读取 [配置](file:///config.ts)"]),
      want: "思考中 读取 配置",
    },
    { name: "empty_thinking", action: thinking([]), want: "思考中" },
    {
      name: "streaming_partial_line",
      action: thinking(["准备\n检", "查"]),
      want: "思考中 检查",
    },
    {
      name: "english_prepare",
      action: tool("bash", { command: "pnpm test" }, "generating"),
      language: "en",
      want: "Preparing: Run pnpm test",
    },
    {
      name: "english_send",
      action: tool("send_message", { message: "check tests" }),
      options: { recipientName: "aa" },
      language: "en",
      want: "Send to aa check tests",
    },
  ]
  for (const { name, action, options, want, language } of cases) {
    await translation.changeLanguage(language ?? "zh-CN")
    assert.equal(
      getProcessActionText(action, translation.t, options),
      want,
      name
    )
  }
})

function view(
  key: string,
  turnID: string,
  kind: Item["kind"],
  callID?: string
): ViewItem {
  const item: Item = { id: key, turn_id: turnID, kind }
  if (kind === "tool_call")
    item.tool_call = { call_id: callID!, name: "bash", arguments: "{}" }
  if (kind === "tool_result") item.tool_result = { call_id: callID }
  return { key, item, turnID, isStreaming: false, ordinal: 1 }
}
