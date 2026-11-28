import assert from "node:assert/strict"
import test from "node:test"
import type { InfiniteData } from "@tanstack/react-query"
import {
  selectFollowUpThread,
  type WorldMessage,
  type WorldPage,
} from "@/lib/api/world"
import {
  getWorldErrorHint,
  mergeSentMessagePage,
  shouldShowTimeDivider,
} from "./message-bubble-helpers"

test("显示首条、间隔超过五分钟或跨天的消息时间", () => {
  const noon = new Date(2026, 10, 23, 12).getTime()
  for (const [previous, next, expected] of [
    [undefined, noon, true],
    [noon, noon + 5 * 60_000, false],
    [noon, noon + 5 * 60_000 + 1, true],
    [noon + 5 * 60_000 + 1, noon, true],
    [
      new Date(2026, 10, 23, 23, 59).getTime(),
      new Date(2026, 10, 24, 0, 1).getTime(),
      true,
    ],
  ] as const) {
    assert.equal(shouldShowTimeDivider(previous, next), expected)
  }
})

const sent: WorldMessage = {
  message_id: "03",
  thread_id: "new-thread",
  peer_addr: "home@alice.dev",
  kind: "message",
  direction: "outbound",
  hop_limit: 5,
  notify_conversation: false,
  body: "Hello",
  status: "sending",
  created_at: 20,
  updated_at: 20,
}
const earlier: WorldMessage = {
  ...sent,
  message_id: "02",
  thread_id: "earlier-thread",
  created_at: 10,
}

test("发送后保留已加载页和游标并立即续聊新话题", () => {
  const cursor = { id: earlier.message_id }
  const lastPage = {
    items: [{ ...earlier, message_id: "01", direction: "inbound" as const }],
  }
  const data: InfiniteData<WorldPage<WorldMessage>> = {
    pages: [{ items: [earlier], next_cursor: cursor }, lastPage],
    pageParams: [undefined, cursor],
  }
  const merged = mergeSentMessagePage(data, sent)
  assert.deepEqual(merged.pages[0], {
    items: [sent, earlier],
    next_cursor: cursor,
  })
  assert.equal(merged.pages[1], lastPage)
  assert.equal(merged.pageParams, data.pageParams)
  assert.equal(data.pages[0].items.length, 1)
  assert.equal(
    selectFollowUpThread(merged.pages.flatMap((page) => page.items))?.thread_id,
    sent.thread_id
  )
  assert.deepEqual(mergeSentMessagePage(undefined, sent), {
    pages: [{ items: [sent] }],
    pageParams: [undefined],
  })
})

test("双向消息按 ID 倒序插入并保留游标", () => {
  const incoming: WorldMessage = {
    ...earlier,
    message_id: "04",
    direction: "inbound",
    status: "completed",
  }
  const firstPage = {
    items: [incoming],
    next_cursor: { id: incoming.message_id },
  }
  const secondPage = {
    items: [earlier],
    next_cursor: { id: earlier.message_id },
  }
  const data = {
    pages: [firstPage, secondPage],
    pageParams: [undefined, firstPage.next_cursor],
  }
  const merged = mergeSentMessagePage(data, sent)
  assert.equal(merged.pages[0], firstPage)
  assert.deepEqual(merged.pages[1], { ...secondPage, items: [sent, earlier] })
  assert.equal(merged.pageParams, data.pageParams)
})

test("发送结果与 SSE 同 ID 时保留较新的状态并去重", () => {
  for (const [cachedStatus, cachedTime, sentStatus, sentTime, expected] of [
    ["replied", 21, "sending", 20, "replied"],
    ["failed", 20, "pending_offline", 20, "failed"],
    ["cancelled", 20, "pending_offline", 20, "cancelled"],
    ["remote_running", 20, "sending", 20, "remote_running"],
    ["sending", 19, "remote_running", 20, "remote_running"],
  ] as const) {
    const cached = { ...sent, status: cachedStatus, updated_at: cachedTime }
    const cursor = { id: earlier.message_id }
    const merged = mergeSentMessagePage(
      {
        pages: [{ items: [earlier], next_cursor: cursor }, { items: [cached] }],
        pageParams: [undefined, cursor],
      },
      { ...sent, status: sentStatus, updated_at: sentTime }
    )
    assert.equal(merged.pages[0].items[0].status, expected)
    assert.equal(
      merged.pages
        .flatMap((page) => page.items)
        .filter((message) => message.message_id === sent.message_id).length,
      1
    )
  }
})

test("World 错误码映射到操作提示，未知错误保留原文", () => {
  for (const [code, hint] of [
    ["world_target_not_public", "world.errors.notPublic"],
    ["world_sender_not_allowed", "world.errors.denied"],
    ["world_domain_not_trusted", "world.errors.untrusted"],
    ["world_identity_unverified", "world.errors.unverified"],
    ["world_target_unreachable", "world.errors.unreachable"],
    ["unauthorized", "world.errors.unauthorized"],
    ["world_peer_not_found", "world.errors.notFound"],
    ["world_agent_no_model", "world.errors.noModel"],
    ["world_hop_limit_exceeded", "world.errors.hopLimit"],
    ["world_target_not_exposed", "world.errors.notExposed"],
    ["world_node_revoked", "world.errors.revoked"],
    ["world_domain_name_taken", "world.errors.nameTaken"],
    ["unknown", undefined],
    [undefined, undefined],
  ] as const)
    assert.equal(getWorldErrorHint(code)?.message, hint)
})
