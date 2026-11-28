import assert from "node:assert/strict"
import { test } from "node:test"
import { QueryClient } from "@tanstack/react-query"
import { ServerConnection } from "../api/server-connection"
import {
  invalidateAfterReconnect,
  invalidateWorldQueries,
} from "./invalidation"
import {
  worldMessagesInfiniteOptions,
  worldMessagesOptions,
  worldThreadsOptions,
} from "./world"

test("World 消息更新和重连刷新对应查询", async (t) => {
  const url = "ws://localhost/api/v1/openvaala"
  const serverID = "server"
  const connection = new ServerConnection({ url, token: "", serverID })
  t.mock.method(connection, "getServerID", () => serverID)

  for (const [event, keys] of [
    [
      "message_updated",
      [
        [serverID, "world", "messages", url],
        [serverID, "world", "threads", url],
        [serverID, "world", "status", url],
        [serverID, "world", "peers", url],
        [serverID, "conversations"],
      ],
    ],
    ["peers_changed", [[serverID, "world", "peers", url]]],
    ["status_changed", [[serverID, "world", "status", url]]],
    [
      undefined,
      [
        [serverID, "world"],
        [serverID, "conversations"],
      ],
    ],
  ] as const) {
    await t.test(event ?? "reconnect", (t) => {
      const client = new QueryClient()
      const invalidate = t.mock.method(client, "invalidateQueries")
      invalidateWorldQueries(client, connection, event)
      assert.deepEqual(
        invalidate.mock.calls.map(({ arguments: args }) => args[0]?.queryKey),
        keys
      )
    })
  }

  const client = new QueryClient()
  const page = worldMessagesOptions(connection, { direction: "outbound" })
  const infinite = worldMessagesInfiniteOptions(connection, { kind: "message" })
  const threads = worldThreadsOptions(connection)
  const conversations = [serverID, "conversations"]
  const other = ["other", "world", "messages", url]
  const keys = [
    page.queryKey,
    infinite.queryKey,
    threads.queryKey,
    conversations,
  ]
  for (const key of [...keys, other]) client.setQueryData(key, [])
  invalidateWorldQueries(client, connection, "message_updated")
  for (const key of keys)
    assert.equal(client.getQueryState(key)?.isInvalidated, true)
  assert.equal(client.getQueryState(other)?.isInvalidated, false)

  client.setQueryData(page.queryKey, [])
  invalidateAfterReconnect(client, serverID)
  assert.equal(client.getQueryState(page.queryKey)?.isInvalidated, true)
})
