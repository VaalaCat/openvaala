import assert from "node:assert/strict"
import { test } from "node:test"
import { atom } from "nanostores"
import type { MethodParams, MethodResult } from "./protocol.gen"
import type {
  ConnectionState,
  GlobalServerNotification,
  ServerConnection,
} from "./server-connection"
import {
  getVisibleTargets,
  getInboundThreadBindings,
  getWorldContactAddress,
  getWorldMessages,
  getWorldThreads,
  getWorldSenderLabel,
  parseWorldAddress,
  listenWorldUpdates,
  selectFollowUpThread,
  type WorldEvent,
  type WorldMessage,
  type WorldPeer,
  type WorldThreadBinding,
} from "./world"

test("解析完整联系人地址", () => {
  for (const [address, target] of [
    ["alice@example.test", undefined],
    ["alice@example.test/assistant", "assistant"],
  ] as const) {
    assert.deepEqual(parseWorldAddress(address), {
      node: "alice",
      domain: "example.test",
      nodeAddress: "alice@example.test",
      target,
    })
  }
  for (const address of [
    "alice",
    "alice@",
    "@example.test",
    "alice@example.test/",
    "alice@example.test/a/b",
  ]) {
    assert.equal(parseWorldAddress(address), undefined, address)
  }
})

test("只隐藏唯一名为 default 的目标", () => {
  const fallback = { name: "default", visibility: "domain" }
  const assistant = { name: "assistant", visibility: "domain" }
  for (const [targets, visible] of [
    [null, []],
    [[], []],
    [[fallback], []],
    [[assistant], [assistant]],
    [
      [fallback, assistant],
      [fallback, assistant],
    ],
  ] satisfies [WorldPeer["targets"], NonNullable<WorldPeer["targets"]>][]) {
    assert.deepEqual(getVisibleTargets({ targets }), visible)
  }
})

test("从联系人的全部往来中选择最近的线程", async (t) => {
  const oldest: WorldMessage = {
    message_id: "one",
    thread_id: "first",
    direction: "outbound",
    kind: "message",
    body: "message",
    peer_addr: "alice@example.test",
    hop_limit: 5,
    notify_conversation: false,
    status: "replied",
    created_at: 1,
    updated_at: 10,
  }
  const newest = {
    ...oldest,
    message_id: "two",
    thread_id: "second",
    created_at: 2,
  }
  for (const [name, fields] of [
    ["手动发件", {}],
    ["绑定到对话的来信", { direction: "inbound", conversation_id: "local" }],
    ["Agent 发件", { conversation_id: "from-agent" }],
    ["Agent 自动回复", { kind: "reply" }],
    ["对方回复", { kind: "reply", direction: "inbound" }],
  ] satisfies [string, Partial<WorldMessage>][]) {
    await t.test(name, () => {
      const latest = { ...newest, ...fields }
      assert.equal(selectFollowUpThread([latest, oldest]), latest)
      assert.equal(selectFollowUpThread([oldest, latest]), latest)
    })
  }
  assert.equal(selectFollowUpThread([]), undefined)
})

test("World 更新在服务端关闭或重连后重订，停止监听时退订", async () => {
  const calls: string[] = []
  let reconnect: (() => void) | undefined
  let notify: ((message: GlobalServerNotification) => void) | undefined
  const events: (WorldEvent | "subscribed")[] = []
  const connection: Parameters<typeof listenWorldUpdates>[0] = {
    $state: atom<ConnectionState>("connected"),
    async call(method) {
      calls.push(method)
      return {} as never
    },
    listenServer(listener) {
      notify = listener
      return () => {
        notify = undefined
      }
    },
    onConnected(listener) {
      reconnect = listener
      return () => {
        reconnect = undefined
      }
    },
  }
  const stop = listenWorldUpdates(
    connection,
    () => events.push("subscribed"),
    (event) => events.push(event)
  )
  await Promise.resolve()
  for (const event of [
    "status_changed",
    "peers_changed",
    "message_updated",
    "unrelated",
  ])
    notify?.({
      method: "world/messageUpdated",
      params: { event, data: { conversation_id: "c1" } },
    })
  notify?.({
    method: "world/messageUpdatedClosed",
    params: { reason: "world message subscription closed" },
  })
  await Promise.resolve()
  reconnect?.()
  await Promise.resolve()
  stop()
  assert.deepEqual(calls, [
    "world/subscribe",
    "world/subscribe",
    "world/subscribe",
    "world/unsubscribe",
  ])
  assert.deepEqual(events, [
    "subscribed",
    "status_changed",
    "peers_changed",
    "message_updated",
    "subscribed",
    "subscribed",
  ])
  assert.equal(notify, undefined)
  assert.equal(reconnect, undefined)
})

test("shows the node name for same-domain incoming mail", () => {
  for (const [address, domain, expected] of [
    ["office@example.test", "example.test", "office"],
    ["office@other.test", "example.test", "office@other.test"],
    ["office@example.test", undefined, "office@example.test"],
    ["unknown", "example.test", "unknown"],
  ] as const) {
    assert.equal(getWorldSenderLabel(address, domain), expected)
  }
})

test("completes short tool destinations for contact links", () => {
  for (const [address, domain, expected] of [
    ["office", "example.test", "office@example.test"],
    ["office/assistant", "example.test", "office@example.test/assistant"],
    ["office@other.test/default", "example.test", "office@other.test/default"],
    ["office", undefined, undefined],
    ["office/a/b", "example.test", undefined],
  ] as const) {
    assert.equal(getWorldContactAddress(address, domain), expected)
  }
})

test("indexes only incoming conversation bindings and keeps the latest", () => {
  const first: WorldThreadBinding = {
    peer_domain: "example.test",
    thread_id: "first",
    conversation_id: "conversation",
    peer_addr: "office@example.test",
    target: "default",
    direction: "inbound",
    remaining_hops: 1,
    created_at: 1,
  }
  const latest = { ...first, thread_id: "latest", created_at: 2 }
  assert.deepEqual(
    getInboundThreadBindings([
      latest,
      first,
      { ...first, direction: "outbound", created_at: 3 },
      { ...first, conversation_id: "" },
    ]),
    new Map([["conversation", latest]])
  )
})

test("World 列表按 RPC 传递筛选参数和游标", async (t) => {
  const messageCursor = { id: "message/id" }
  const threadCursor = { id: "thread/id", updated_at: 12345 }
  const addresses = [
    "bob@example.test",
    "bob@example.test/default",
    "bob@example.test",
  ]
  for (const { method, run, params } of [
    {
      method: "world/listMessages",
      run: (connection: Pick<ServerConnection, "call">) =>
        getWorldMessages(
          connection,
          { direction: "outbound", peerAddrs: addresses },
          { limit: 30, cursor: messageCursor }
        ),
      params: {
        direction: "outbound",
        peer_addr: addresses,
        limit: 30,
        cursor_id: "message/id",
      },
    },
    {
      method: "world/listThreads",
      run: (connection: Pick<ServerConnection, "call">) =>
        getWorldThreads(connection, "conversation", {
          limit: 200,
          cursor: threadCursor,
        }),
      params: {
        conversation_id: "conversation",
        limit: 200,
        cursor_id: "thread/id",
        cursor_updated_at: 12345,
      },
    },
  ]) {
    await t.test(method, async () => {
      const response = { items: [], next_cursor: { id: "next" } }
      const connection = {
        async call<M extends keyof MethodParams>(
          actual: M,
          input: MethodParams[M]
        ): Promise<MethodResult[M]> {
          assert.equal(actual, method)
          assert.deepEqual(JSON.parse(JSON.stringify(input)), params)
          return response as unknown as MethodResult[M]
        },
      }
      assert.deepEqual(await run(connection), response)
    })
  }
})
