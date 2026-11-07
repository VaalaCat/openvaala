import assert from "node:assert/strict"
import { before, test } from "node:test"
import { i18n, initializeI18n } from "../../i18n"
import type { ViewItem } from "../api/conversation-items"
import type { Item } from "../api/protocol.gen"
import type { ItemRow, MessageRow } from "../item-renderers"
import {
  createItemID,
  getSideConversationRows,
  getSideConversationTitle,
} from "./side-conversation"

before(async () => {
  await initializeI18n()
  await i18n.changeLanguage("zh-CN")
})

function createView(id: string, turnID: string): ViewItem {
  return {
    key: id,
    item: { id, turn_id: turnID, kind: "message" } as Item,
    turnID,
    isStreaming: false,
  }
}

function createItemRow(id: string): ItemRow {
  return { kind: "item", view: createView(id, "") }
}

function createTurnRow(turnID: string, itemIDs: string[]): MessageRow {
  return {
    kind: "turn",
    key: turnID + ":0",
    turnID,
    items: itemIDs.map((id) => createView(id, turnID)),
    rows: [],
  }
}

test("title keeps first 20 characters", () => {
  const cases: { name: string; question: string; want: string }[] = [
    {
      name: "short question",
      question: "为什么用 SQLite",
      want: "旁支 · 为什么用 SQLite",
    },
    { name: "empty question", question: "", want: "旁支" },
    { name: "blank question", question: "  \n ", want: "旁支" },
    {
      name: "exactly 20",
      question: "一二三四五六七八九十一二三四五六七八九十",
      want: "旁支 · 一二三四五六七八九十一二三四五六七八九十",
    },
    {
      name: "more than 20",
      question: "一二三四五六七八九十一二三四五六七八九十多",
      want: "旁支 · 一二三四五六七八九十一二三四五六七八九十",
    },
    {
      name: "multi line question becomes one line",
      question: "  第一行\n\n  第二行  ",
      want: "旁支 · 第一行 第二行",
    },
    {
      name: "counts characters, not UTF-16 units",
      question: "😀".repeat(25),
      want: "旁支 · " + "😀".repeat(20),
    },
  ]
  for (const { name, question, want } of cases)
    assert.equal(getSideConversationTitle(question), want, name)
})

test("rows start at the turn containing the boundary item", () => {
  const rows: MessageRow[] = [
    createItemRow("i0"),
    createTurnRow("t1", ["i1", "i2"]),
    createTurnRow("t2", ["i3", "boundary", "i5"]),
    createTurnRow("t3", ["i6"]),
  ]
  const cases: { name: string; boundary: string; want: MessageRow[] }[] = [
    {
      name: "boundary inside a turn",
      boundary: "boundary",
      want: rows.slice(2),
    },
    { name: "first item of a turn", boundary: "i1", want: rows.slice(1) },
    { name: "item outside any turn", boundary: "i0", want: rows },
    { name: "boundary not shown yet", boundary: "missing", want: [] },
  ]
  for (const { name, boundary, want } of cases)
    assert.deepEqual(getSideConversationRows(rows, boundary), want, name)
  assert.deepEqual(getSideConversationRows([], "boundary"), [])
})

test("item IDs are time ordered UUID v7", () => {
  const before = Date.now()
  const id = createItemID()
  const after = Date.now()
  assert.match(
    id,
    /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
  )
  const timestamp = parseInt(id.replaceAll("-", "").slice(0, 12), 16)
  assert.ok(timestamp >= before && timestamp <= after, id)
  assert.notEqual(createItemID(), createItemID())
})
