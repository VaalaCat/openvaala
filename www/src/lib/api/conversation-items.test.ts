import assert from "node:assert/strict"
import { test } from "node:test"
import {
  getToolResultsByCallID,
  getTurnSavedDurations,
  type ViewItem,
} from "./conversation-items"
import type { Item } from "./protocol.gen"

test("getTurnSavedDurations spans the saved items of each turn", () => {
  const cases: {
    name: string
    items: ViewItem[]
    want: Record<string, number>
  }[] = [
    {
      name: "earliest_to_latest",
      items: [
        saved("a", "t1", 1000),
        saved("b", "t1", 4000),
        saved("c", "t1", 2500),
      ],
      want: { t1: 3000 },
    },
    {
      name: "turns_apart_and_streaming_ignored",
      items: [
        saved("a", "t0", 100),
        saved("b", "t1", 1000),
        { ...saved("c", "t1", 9000), ordinal: undefined },
        saved("d", "t1", 1500),
      ],
      want: { t0: 0, t1: 500 },
    },
    {
      name: "no_saved_time",
      items: [
        { ...saved("a", "t1", 2000), ordinal: undefined },
        saved("b", "t2", 0),
      ],
      want: {},
    },
  ]
  for (const { name, items, want } of cases)
    assert.deepEqual(
      Object.fromEntries(getTurnSavedDurations(items)),
      want,
      name
    )
})

test("getToolResultsByCallID keys results by call_id", () => {
  const result: ViewItem = {
    ...saved("r", "t1", 1),
    item: {
      id: "r",
      turn_id: "t1",
      kind: "tool_result",
      tool_result: { call_id: "c1" },
    },
  }
  const results = getToolResultsByCallID([saved("m", "t1", 1), result])
  assert.deepEqual([...results.keys()], ["c1"])
  assert.equal(results.get("c1"), result)
})

function saved(key: string, turnID: string, updatedAt: number): ViewItem {
  const item: Item = {
    id: key,
    turn_id: turnID,
    kind: "message",
    updated_at: updatedAt || undefined,
  }
  return { key, item, turnID, ordinal: 1, isStreaming: false }
}
