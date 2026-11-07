import assert from "node:assert/strict"
import { test } from "node:test"
import type { Item } from "./api/protocol.gen"
import type { MessageRow } from "./item-renderers"
import { shouldShowForkOrigin } from "./fork-origin"

function createItemRow(id: string, fields: Partial<Item>): MessageRow {
  return {
    kind: "item",
    view: {
      key: id,
      item: { id, kind: "message", ...fields } as Item,
      turnID: "",
      isStreaming: false,
    },
  }
}

const userForkMarker = createItemRow("fork", {
  kind: "session_fork",
  session_fork: {
    source_session_id: "session-1",
    source_conversation_id: "conversation-1",
  },
})
const subAgentMarker = createItemRow("spawn", {
  kind: "session_fork",
  session_fork: { source_session_id: "session-1" },
})
const message = createItemRow("message", {})
const turn: MessageRow = {
  kind: "turn",
  key: "turn-1:0",
  turnID: "turn-1",
  items: [],
  rows: [],
}

test("fork origin is shown by kind, loaded history and leading marker", () => {
  const cases: {
    name: string
    kind: "side-conversation" | "fork"
    rows: MessageRow[]
    hasEarlierItems: boolean
    want: boolean
  }[] = [
    {
      name: "side conversation with rows",
      kind: "side-conversation",
      rows: [turn],
      hasEarlierItems: false,
      want: true,
    },
    {
      name: "side conversation before the boundary item arrives",
      kind: "side-conversation",
      rows: [],
      hasEarlierItems: true,
      want: true,
    },
    {
      name: "fork with earliest page loaded",
      kind: "fork",
      rows: [turn],
      hasEarlierItems: false,
      want: true,
    },
    {
      name: "fork without rows",
      kind: "fork",
      rows: [],
      hasEarlierItems: false,
      want: true,
    },
    {
      name: "fork with earlier pages still unloaded",
      kind: "fork",
      rows: [turn],
      hasEarlierItems: true,
      want: false,
    },
    {
      name: "fork starting with a user fork marker",
      kind: "fork",
      rows: [userForkMarker, turn],
      hasEarlierItems: false,
      want: false,
    },
    {
      name: "fork starting with a sub-agent marker",
      kind: "fork",
      rows: [subAgentMarker, turn],
      hasEarlierItems: false,
      want: true,
    },
    {
      name: "fork with a marker that is not the first row",
      kind: "fork",
      rows: [message, userForkMarker],
      hasEarlierItems: false,
      want: true,
    },
  ]
  for (const each of cases)
    assert.equal(
      shouldShowForkOrigin(each.kind, each.rows, each.hasEarlierItems),
      each.want,
      each.name
    )
})
