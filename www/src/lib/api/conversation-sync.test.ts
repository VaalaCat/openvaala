import assert from "node:assert/strict"
import { mock, test } from "node:test"
import { setTimeout as delay } from "node:timers/promises"
import { QueryClient } from "@tanstack/react-query"
import { RpcError, rpcConnectionLostCode } from "../rpc-error"
import {
  $conversationResults,
  getConversationResultKey,
} from "../conversation-results"
import type { ViewItem } from "./conversation-items"
import { ConversationSync, type SyncConnection } from "./conversation-sync"
import {
  ErrorCodeConversationNotFound,
  type Conversation,
  type Item,
  type LoggedItem,
  type MethodParams,
  type MethodResult,
  type NotificationParams,
  type NotificationTarget,
  type SessionRuntimeInfo,
  type TurnFinishedParams,
} from "./protocol.gen"
import type { ServerNotification } from "./server-connection"

const serverID = "server-1"
const conversationID = "conv-1"
const rootSessionID = "root"

type ScriptedReply<M extends keyof MethodParams> = (
  params: MethodParams[M]
) => MethodResult[M] | Promise<MethodResult[M]>

// 按方法脚本回复的连接：回复函数在请求发出时同步执行，里面 push 的通知先于这个请求的响应到达，
// 和 serve 在退订响应之前推 subscription/closed 的顺序一致
class FakeSyncConnection implements SyncConnection {
  private calls: { method: keyof MethodParams; params: unknown }[] = []
  private readonly replies = new Map<keyof MethodParams, unknown>()
  private readonly notificationHandlers = new Map<
    string,
    Set<(notification: ServerNotification) => void>
  >()
  private readonly connectedHandlers = new Set<() => void>()

  reply<M extends keyof MethodParams>(method: M, reply: ScriptedReply<M>) {
    this.replies.set(method, reply)
  }

  async call<M extends keyof MethodParams>(
    method: M,
    params: MethodParams[M]
  ): Promise<MethodResult[M]> {
    this.calls.push({ method, params })
    const reply = this.replies.get(method) as ScriptedReply<M> | undefined
    if (!reply) throw new Error(`no scripted reply for ${method}`)
    return reply(params)
  }

  getServerID(): string {
    return serverID
  }

  listen(
    conversationID: string,
    handler: (notification: ServerNotification) => void
  ): () => void {
    const handlers = this.notificationHandlers.get(conversationID) ?? new Set()
    handlers.add(handler)
    this.notificationHandlers.set(conversationID, handlers)
    return () => handlers.delete(handler)
  }

  onConnected(handler: () => void): () => void {
    this.connectedHandlers.add(handler)
    return () => this.connectedHandlers.delete(handler)
  }

  // serve 推来一条通知，按 conversation_id 交给登记的处理函数
  push(notification: ServerNotification) {
    const { conversation_id } = notification.params as NotificationTarget
    for (const handler of [
      ...(this.notificationHandlers.get(conversation_id) ?? []),
    ])
      handler(notification)
  }

  // 重连成功
  connect() {
    for (const handler of [...this.connectedHandlers]) handler()
  }

  getMethods(): string[] {
    return this.calls.map((call) => call.method)
  }

  getCalls(): [string, unknown][] {
    return this.calls.map((call) => [call.method, call.params])
  }

  getParams<M extends keyof MethodParams>(method: M): MethodParams[M][] {
    return this.calls
      .filter((call) => call.method === method)
      .map((call) => call.params as MethodParams[M])
  }

  clearCalls() {
    this.calls = []
  }
}

// 等所有脚本回复的 Promise 链走完
function flush(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve))
}

function createHarness() {
  // 1. 默认脚本：空对话，只有根 Session。
  const connection = new FakeSyncConnection()
  connection.reply("conversation/open", (params) => ({
    conversation: conversationRecord(params.conversation_id),
  }))
  connection.reply("conversation/unsubscribe", () => ({}))
  connection.reply("session/unsubscribe", () => ({}))
  connection.reply("conversation/subscribe", () => ({
    title: "标题",
    sessions: [sessionInfo(rootSessionID)],
  }))
  connection.reply("session/subscribe", () => ({
    pending_inputs: { inputs: [] },
    last_saved_ordinal: 0,
    streaming_items: [],
  }))
  connection.reply("session/items/list", () => ({ items: [] }))
  connection.reply("conversation/submitInput", () => ({ item_id: "input-1" }))
  // 2. 记录查询失效的 key。
  const queryClient = new QueryClient()
  const invalidate = mock.method(
    queryClient,
    "invalidateQueries",
    async () => {}
  )
  const sync = new ConversationSync(connection, queryClient, conversationID)
  return {
    connection,
    queryClient,
    sync,
    getInvalidatedKeys: () =>
      invalidate.mock.calls.map((call) => call.arguments[0]?.queryKey),
    // 开始同步并等它完成，返回停止函数
    async start(): Promise<() => void> {
      const stop = sync.run()
      await flush()
      return stop
    },
  }
}

type Harness = ReturnType<typeof createHarness>

function conversationRecord(id: string): Conversation {
  return { id, executor_settings: {}, root_session_id: rootSessionID }
}

function sessionInfo(id: string, parentSessionID?: string): SessionRuntimeInfo {
  return {
    session: {
      id,
      conversation_id: conversationID,
      agent_name: "default",
      parent_session_id: parentSessionID,
    },
    state: "idle",
    context: { window: 0, tokens: 0, source: "unknown" },
    usage: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
  }
}

function messageItem(id: string, turnID: string, text: string): Item {
  return {
    id,
    turn_id: turnID,
    kind: "message",
    message: {
      from: { kind: "agent", id: rootSessionID },
      to: null,
      content: [{ kind: "text", text: { text } }],
    },
  }
}

function toolCallItem(id: string, turnID: string, callID: string): Item {
  return {
    id,
    turn_id: turnID,
    kind: "tool_call",
    tool_call: { call_id: callID, name: "bash", arguments: "" },
  }
}

function logged(ordinal: number, item: Item): LoggedItem {
  return { ordinal, item }
}

// 按流水号生成一批已保存的消息，Item ID 为 item-<流水号>
function loggedMessages(...ordinals: number[]): LoggedItem[] {
  return ordinals.map((ordinal) =>
    logged(ordinal, messageItem(`item-${ordinal}`, "t0", `第 ${ordinal} 条`))
  )
}

// 对话里的一条通知，默认来自根 Session 的 Session 订阅
function notification<M extends keyof NotificationParams>(
  method: M,
  params: Omit<NotificationParams[M], keyof NotificationTarget>,
  target: Partial<NotificationTarget> = {}
): ServerNotification {
  return {
    method,
    params: {
      conversation_id: conversationID,
      session_id: rootSessionID,
      subscription: "session",
      ...target,
      ...params,
    },
  } as ServerNotification
}

function textDelta(turnID: string, itemIndex: number, text: string) {
  return notification("item/delta", {
    turn_id: turnID,
    item_index: itemIndex,
    delta: { text },
  })
}

function getText(view: ViewItem | undefined): string | undefined {
  return view?.item.message?.content[0]?.text?.text
}

function getKeys(sync: ConversationSync): string[] {
  return sync.$items.get().map((view) => view.key)
}

function getOrdinals(sync: ConversationSync): (number | undefined)[] {
  return sync.$items.get().map((view) => view.ordinal)
}

function countCalls(h: Harness, method: string): number {
  return h.connection.getMethods().filter((each) => each === method).length
}

const tests: { name: string; run: (h: Harness) => Promise<void> }[] = [
  {
    name: "only_new_live_items_enter_once_without_notifying_on_presentation",
    async run(h) {
      await h.start()
      const reply = messageItem("reply", "t1", "回答")
      const started = notification("item/started", {
        turn_id: "t1",
        item_index: 0,
        item: reply,
      })
      h.connection.push(started)
      assert.equal(h.sync.isUnpresentedLiveItem(reply.id), true)
      const listener = mock.fn()
      const stopListening = h.sync.$items.listen(listener)
      h.sync.markItemsPresented(h.sync.$items.get())
      assert.equal(listener.mock.callCount(), 0)
      assert.equal(h.sync.isUnpresentedLiveItem(reply.id), false)
      stopListening()
      for (const update of [
        started,
        textDelta("t1", 0, "追加"),
        notification("item/completed", {
          turn_id: "t1",
          item_index: 0,
          item: reply,
        }),
        notification("items/saved", {
          turn_id: "t1",
          items: [logged(1, reply)],
        }),
      ]) {
        h.connection.push(update)
        assert.equal(h.sync.isUnpresentedLiveItem(reply.id), false)
      }
      // 1. 没经过流式生成的已保存输入，也属于实时新增。
      h.connection.push(
        notification("items/saved", { turn_id: "t0", items: loggedMessages(2) })
      )
      assert.equal(h.sync.isUnpresentedLiveItem("item-2"), true)
      // 2. 折叠成员随整列提交消耗，之后展开不再待入场。
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 1,
          item: toolCallItem("tool", "t1", "call"),
        })
      )
      h.sync.markItemsPresented(h.sync.$items.get())
      assert.equal(h.sync.isUnpresentedLiveItem("tool"), false)
      h.connection.push(
        notification("items/saved", {
          turn_id: "t1",
          items: [
            logged(3, {
              id: "result",
              turn_id: "t1",
              kind: "tool_result",
              tool_result: { call_id: "call", content: "ok" },
            }),
          ],
        })
      )
      assert.equal(h.sync.isUnpresentedLiveItem("tool"), false)
    },
  },
  {
    name: "history_pagination_and_staged_notifications_do_not_enter",
    async run(h) {
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 2,
        streaming_items: [
          { item_index: 0, item: messageItem("snapshot", "t1", "中途打开") },
        ],
      }))
      h.connection.reply("session/items/list", () => {
        h.connection.push(
          notification("item/started", {
            turn_id: "t1",
            item_index: 1,
            item: messageItem("staged", "t1", "同步期间"),
          })
        )
        return { items: loggedMessages(2), next_cursor: 2 }
      })
      await h.start()
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(1),
      }))
      await h.sync.loadEarlierItems()
      for (const key of ["item-1", "item-2", "snapshot", "staged"])
        assert.equal(h.sync.isUnpresentedLiveItem(key), false, key)
      // 1. 重连补齐和期间暂存通知仍作为已有内容展示。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 3,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => {
        h.connection.push(
          notification("items/saved", {
            turn_id: "t0",
            items: loggedMessages(4),
          })
        )
        return { items: loggedMessages(3) }
      })
      h.connection.connect()
      await flush()
      for (const key of ["item-3", "item-4"])
        assert.equal(h.sync.isUnpresentedLiveItem(key), false, key)
      h.connection.push(
        notification("items/saved", { turn_id: "t0", items: loggedMessages(5) })
      )
      assert.equal(h.sync.isUnpresentedLiveItem("item-5"), true)
    },
  },
  {
    name: "open_and_stream",
    async run(h) {
      // 1. 首次同步：open 之后才知道根 Session，再退订、订阅两种订阅，最后按 desc 读最近一页。
      await h.start()
      assert.deepEqual(h.connection.getMethods(), [
        "conversation/open",
        "conversation/unsubscribe",
        "session/unsubscribe",
        "conversation/subscribe",
        "session/subscribe",
        "session/items/list",
      ])
      assert.deepEqual(h.connection.getParams("session/items/list"), [
        {
          conversation_id: conversationID,
          session_id: rootSessionID,
          order: "desc",
          limit: 50,
        },
      ])
      assert.equal(h.sync.$syncState.get(), "synced")
      assert.equal(h.sync.$title.get(), "标题")
      assert.equal(h.sync.getFollowedSessionID(), rootSessionID)
      assert.equal(h.sync.$rootSessionID.get(), rootSessionID)
      // 2. 推送开始和两段增量，最后一项是两段增量拼起来的文本。
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 0,
          item: messageItem("reply", "t1", ""),
        })
      )
      h.connection.push(textDelta("t1", 0, "你好"))
      h.connection.push(textDelta("t1", 0, "！"))
      await delay(60)
      assert.equal(getText(h.sync.$items.get().at(-1)), "你好！")
      assert.equal(h.sync.$items.get().at(-1)?.isStreaming, true)
      // 3. 完成后换成完整 Item，不再生成。
      h.connection.push(
        notification("item/completed", {
          turn_id: "t1",
          item_index: 0,
          item: messageItem("reply", "t1", "你好！"),
        })
      )
      const items = h.sync.$items.get()
      assert.equal(items.length, 1)
      assert.equal(getText(items[0]), "你好！")
      assert.equal(items[0].isStreaming, false)
    },
  },
  {
    name: "open_existing_loads_history",
    async run(h) {
      // 1. 最近一页按 desc 返回 3 条和下一页游标。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 3,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(3, 2, 1),
        next_cursor: 1,
      }))
      await h.start()
      // 2. 按流水号升序显示，还有更早的 Item。
      assert.deepEqual(getOrdinals(h.sync), [1, 2, 3])
      assert.equal(h.sync.$hasEarlierItems.get(), true)
    },
  },
  {
    name: "load_earlier_items_prepends_page",
    async run(h) {
      // 1. 最近一页是 5、4、3，下一页游标 3。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 5,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(5, 4, 3),
        next_cursor: 3,
      }))
      await h.start()
      // 2. 向前加载一页：desc、从游标 3 往前，结果放到最前面；没有下一页游标时不再有更早的。
      h.connection.clearCalls()
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(2, 1),
      }))
      await h.sync.loadEarlierItems()
      assert.deepEqual(h.connection.getParams("session/items/list"), [
        {
          conversation_id: conversationID,
          session_id: rootSessionID,
          order: "desc",
          cursor: 3,
          limit: 50,
        },
      ])
      assert.deepEqual(getOrdinals(h.sync), [1, 2, 3, 4, 5])
      assert.equal(h.sync.$hasEarlierItems.get(), false)
    },
  },
  {
    name: "reconnect_resumes_without_gap",
    async run(h) {
      // 1. 首次同步已交付到流水号 3。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 3,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(3, 2, 1),
      }))
      await h.start()
      // 2. 断开期间 serve 保存了 4、5、6，每页只回 2 条；补齐期间又推来 6、7。
      h.connection.clearCalls()
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 6,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", (params) => {
        if (params.cursor === 5)
          h.connection.push(
            notification("items/saved", {
              turn_id: "t0",
              items: loggedMessages(6, 7),
            })
          )
        return {
          items: loggedMessages(4, 5, 6)
            .filter((each) => each.ordinal > (params.cursor ?? 0))
            .slice(0, 2),
        }
      })
      h.connection.connect()
      await flush()
      // 3. 重新同步先退订上一次的订阅，再按 asc 从已交付位置补齐，不缺不重。
      assert.deepEqual(h.connection.getMethods(), [
        "conversation/unsubscribe",
        "session/unsubscribe",
        "conversation/open",
        "conversation/unsubscribe",
        "session/unsubscribe",
        "conversation/subscribe",
        "session/subscribe",
        "session/items/list",
        "session/items/list",
      ])
      const listParams = h.connection.getParams("session/items/list")
      assert.deepEqual(listParams[0], {
        conversation_id: conversationID,
        session_id: rootSessionID,
        order: "asc",
        cursor: 3,
        limit: 50,
      })
      assert.equal(listParams[1].cursor, 5)
      assert.deepEqual(getOrdinals(h.sync), [1, 2, 3, 4, 5, 6, 7])
      assert.equal(h.sync.$syncState.get(), "synced")
    },
  },
  {
    name: "reopen_current_conversation_mid_stream_no_duplicate",
    async run(h) {
      // 1. 同步后开始生成回复，收到首段「今天」。
      await h.start()
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 0,
          item: messageItem("reply", "t1", ""),
        })
      )
      h.connection.push(textDelta("t1", 0, "今天"))
      // 2. 重新同步：退订时旧订阅的增量「sunny」先到，随后是 subscription/closed；生成中 Item 已含「今天sunny」。
      h.connection.reply("session/unsubscribe", () => {
        h.connection.push(textDelta("t1", 0, "sunny"))
        h.connection.push(
          notification("subscription/closed", { reason: "unsubscribed" })
        )
        return {}
      })
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 0,
        streaming_items: [
          { item_index: 0, item: messageItem("reply", "t1", "今天sunny") },
        ],
      }))
      h.connection.connect()
      await flush()
      // 3. 生成中的 Item 只有一条，之后的增量接着拼。
      h.connection.push(textDelta("t1", 0, "!"))
      await delay(60)
      assert.deepEqual(getKeys(h.sync), ["reply"])
      assert.equal(getText(h.sync.$items.get()[0]), "今天sunny!")
    },
  },
  {
    name: "resync_drops_unsaved_items_missing_from_snapshot",
    async run(h) {
      // 1. 同步后有两条生成中 Item。
      await h.start()
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 0,
          item: messageItem("reply", "t1", "答"),
        })
      )
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 1,
          item: toolCallItem("call", "t1", "call-1"),
        })
      )
      // 2. 断开期间工具调用被中断且不保存，重新同步的生成中列表里只剩回复。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 0,
        streaming_items: [
          { item_index: 0, item: messageItem("reply", "t1", "答案") },
        ],
      }))
      h.connection.connect()
      await flush()
      assert.deepEqual(getKeys(h.sync), ["reply"])
      assert.equal(getText(h.sync.$items.get()[0]), "答案")
    },
  },
  {
    name: "resync_clears_finished_running_turns",
    async run(h) {
      // 1. 同步后 t1 开始执行。
      await h.start()
      h.connection.push(notification("turn/started", { turn_id: "t1" }))
      // 2. 断开期间 t1、t2 先后结束，t3 正在执行。
      h.connection.reply("conversation/subscribe", () => ({
        title: "标题",
        sessions: [
          {
            ...sessionInfo(rootSessionID),
            last_turn_id: "t2",
            last_turn_status: "completed",
            active_turn_id: "t3",
          },
        ],
      }))
      h.connection.connect()
      await flush()
      // 3. t1 不再记为 running，改看 Turn 记录；t2 是终态，t3 仍在执行。
      assert.deepEqual(h.sync.$turns.get(), {
        t2: { status: "completed" },
        t3: { status: "running" },
      })
    },
  },
  {
    name: "server_closed_subscription_resyncs",
    async run(h) {
      // 1. 同步完成后 serve 关闭 Session 订阅（例如订阅方过慢），重新同步一次。
      await h.start()
      h.connection.push(
        notification("subscription/closed", { reason: "subscriber too slow" })
      )
      await flush()
      assert.equal(countCalls(h, "conversation/open"), 2)
      assert.equal(countCalls(h, "session/subscribe"), 2)
      assert.equal(h.sync.$syncState.get(), "synced")
      // 2. 之后保存的 Item 只交付一次。
      h.connection.push(
        notification("items/saved", {
          turn_id: "t0",
          items: loggedMessages(1),
        })
      )
      assert.deepEqual(getOrdinals(h.sync), [1])
    },
  },
  {
    name: "configuration_applied_after_turn_resyncs_without_duplicate_history",
    async run(h) {
      // 1. 已保存的输出已交付，空闲后应用配置关闭旧订阅。
      await h.start()
      h.connection.push(
        notification("items/saved", {
          turn_id: "t0",
          items: loggedMessages(1),
        })
      )
      h.connection.push(
        notification("subscription/closed", {
          reason: "conversation reconfigured",
        })
      )
      await flush()
      // 2. 沿现有恢复入口重新订阅；已交付历史不重复，后续输出照常到达。
      assert.equal(countCalls(h, "conversation/open"), 2)
      assert.equal(countCalls(h, "session/subscribe"), 2)
      assert.equal(h.sync.$syncState.get(), "synced")
      h.connection.push(
        notification("items/saved", {
          turn_id: "t1",
          items: loggedMessages(2),
        })
      )
      assert.deepEqual(getOrdinals(h.sync), [1, 2])
    },
  },
  {
    name: "closed_during_sync_drops_staged_without_resync",
    async run(h) {
      // 1. 同步自己退订时：旧订阅的通知先到，随后是 Session 订阅的 subscription/closed。
      h.connection.reply("session/unsubscribe", () => {
        h.connection.push(
          notification("item/started", {
            turn_id: "t0",
            item_index: 0,
            item: messageItem("stale", "t0", "旧"),
          })
        )
        h.connection.push(
          notification(
            "session/statusUpdated",
            { info: sessionInfo("child", rootSessionID) },
            { subscription: "conversation", session_id: "child" }
          )
        )
        h.connection.push(
          notification("subscription/closed", { reason: "unsubscribed" })
        )
        return {}
      })
      await h.start()
      // 2. 只删掉同一种订阅暂存的通知，不重新同步。
      assert.equal(countCalls(h, "conversation/open"), 1)
      assert.deepEqual(getKeys(h.sync), [])
      assert.deepEqual(Object.keys(h.sync.$sessions.get()).sort(), [
        "child",
        rootSessionID,
      ])
    },
  },
  {
    name: "switching_conversation_drops_old_events",
    async run(h) {
      // 1. 打开旧对话后停止，停止时退订两个订阅。
      const stop = await h.start()
      h.connection.clearCalls()
      stop()
      assert.deepEqual(h.connection.getParams("conversation/unsubscribe"), [
        { conversation_id: conversationID },
      ])
      assert.deepEqual(h.connection.getParams("session/unsubscribe"), [
        { conversation_id: conversationID, session_id: rootSessionID },
      ])
      // 2. 打开新对话；旧对话的通知不再写入旧 Sync，新对话的照常写入新 Sync。
      const next = new ConversationSync(h.connection, h.queryClient, "conv-2")
      next.run()
      await flush()
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 0,
          item: messageItem("old", "t1", ""),
        })
      )
      h.connection.push(textDelta("t1", 0, "旧"))
      h.connection.push(
        notification(
          "item/started",
          {
            turn_id: "t2",
            item_index: 0,
            item: messageItem("new", "t2", "新"),
          },
          { conversation_id: "conv-2" }
        )
      )
      assert.deepEqual(getKeys(h.sync), [])
      assert.deepEqual(getKeys(next), ["new"])
    },
  },
  {
    name: "run_again_after_stop_subscribes_once",
    async run(h) {
      // 1. React StrictMode：open 还没返回就停止，随即再次运行。
      const stop = h.sync.run()
      stop()
      h.sync.run()
      await flush()
      // 2. 被作废的同步不再订阅，只有第二次运行订阅一次。
      assert.equal(countCalls(h, "conversation/open"), 2)
      assert.equal(countCalls(h, "conversation/subscribe"), 1)
      assert.equal(countCalls(h, "session/subscribe"), 1)
      assert.equal(h.sync.$syncState.get(), "synced")
    },
  },
  {
    name: "notifications_during_sync_delivered_in_order",
    async run(h) {
      // 1. 读 Item 期间推来开始和两段增量，同步完成前不交付。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 1,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => {
        h.connection.push(
          notification("item/started", {
            turn_id: "t1",
            item_index: 0,
            item: messageItem("reply", "t1", ""),
          })
        )
        h.connection.push(textDelta("t1", 0, "x"))
        h.connection.push(textDelta("t1", 0, "y"))
        assert.deepEqual(getKeys(h.sync), [])
        return { items: loggedMessages(1) }
      })
      await h.start()
      // 2. 同步完成后按到达顺序交付在已保存内容之后。
      assert.deepEqual(getKeys(h.sync), ["item-1", "reply"])
      assert.equal(getText(h.sync.$items.get()[1]), "xy")
    },
  },
  {
    name: "stale_items_saved_dropped",
    async run(h) {
      // 1. 同步已交付到流水号 3。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 3,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(3, 2, 1),
      }))
      await h.start()
      // 2. items/saved 里不大于 3 的丢弃，只追加 4。
      h.connection.push(
        notification("items/saved", {
          turn_id: "t0",
          items: [
            logged(2, messageItem("item-2", "t0", "重复")),
            ...loggedMessages(3, 4),
          ],
        })
      )
      assert.deepEqual(getOrdinals(h.sync), [1, 2, 3, 4])
      assert.equal(getText(h.sync.$items.get()[1]), "第 2 条")
    },
  },
  {
    name: "saved_item_replaces_streaming_in_place",
    async run(h) {
      // 1. 已保存一条输入，之后生成回复和工具调用。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 1,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(1),
      }))
      await h.start()
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 0,
          item: messageItem("reply", "t1", "答"),
        })
      )
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 1,
          item: toolCallItem("call", "t1", "call-1"),
        })
      )
      // 2. 回复保存后原位替换：位置不变，带流水号。
      h.connection.push(
        notification("items/saved", {
          turn_id: "t1",
          items: [logged(2, messageItem("reply", "t1", "答案"))],
        })
      )
      assert.deepEqual(getKeys(h.sync), ["item-1", "reply", "call"])
      const reply = h.sync.$items.get()[1]
      assert.equal(reply.ordinal, 2)
      assert.equal(reply.isStreaming, false)
      assert.equal(getText(reply), "答案")
    },
  },
  {
    name: "compaction_placeholder_is_removed_on_unsaved_interrupt_and_saved_summary_survives_later_failure",
    async run(h) {
      await h.start()
      const summary: Item = {
        id: "summary",
        turn_id: "t1",
        kind: "context_summary",
        context_summary: {
          text: "已保存的摘要",
          covered_through_item_id: "input",
        },
      }
      // 1. 未保存的压缩被中断时删除占位，不新增成功或失败摘要。
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 0,
          item: { ...summary, context_summary: undefined },
        })
      )
      assert.equal(h.sync.$items.get()[0].isStreaming, true)
      h.connection.push(
        notification("item/interrupted", { turn_id: "t1", item_index: 0 })
      )
      assert.deepEqual(h.sync.$items.get(), [])
      // 2. 摘要生成完但未保存时仍没有 ordinal；保存后同一 Item 原位替换。
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 1,
          item: summary,
        })
      )
      h.connection.push(
        notification("item/completed", {
          turn_id: "t1",
          item_index: 1,
          item: summary,
        })
      )
      assert.equal(h.sync.$items.get()[0].ordinal, undefined)
      h.connection.push(
        notification("items/saved", {
          turn_id: "t1",
          items: [logged(1, summary)],
        })
      )
      assert.equal(h.sync.$items.get()[0].ordinal, 1)
      // 3. 后续模型失败只更新本轮终态，不抹掉已保存摘要。
      h.connection.push(
        notification("turn/finished", {
          turn_id: "t1",
          status: "failed",
          input_item_ids: [],
          error: "model disconnected",
        })
      )
      assert.equal(h.sync.$items.get()[0].item, summary)
      assert.equal(h.sync.$turns.get().t1.error, "model disconnected")
    },
  },
  {
    name: "tool_progress_delivered_for_history_tool_call",
    async run(h) {
      // 1. 工具执行中途打开页面：tool_call 执行前已保存，只能从 items/list 的历史读到，没有生成时的位置。
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 1,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => ({
        items: [logged(1, toolCallItem("call", "t1", "call-1"))],
      }))
      await h.start()
      // 2. 收到工具进度后按 call_id 能查到。
      const progress = {
        call_id: "call-1",
        stream: "terminal" as const,
        content: "ok\n",
      }
      h.connection.push(
        notification("item/toolProgress", {
          turn_id: "t1",
          item_index: 3,
          tool_progress: progress,
        })
      )
      assert.deepEqual(h.sync.$toolProgress.get()["call-1"], progress)
      // 3. 后续的终端输出接在后面，工具卡能显示到目前为止的全部输出。
      h.connection.push(
        notification("item/toolProgress", {
          turn_id: "t1",
          item_index: 3,
          tool_progress: { ...progress, content: "\u001b[32mdone\u001b[0m\n" },
        })
      )
      assert.equal(
        h.sync.$toolProgress.get()["call-1"]?.content,
        "ok\n\u001b[32mdone\u001b[0m\n"
      )
      // 4. 工具结果保存后不再保留执行中的输出。
      h.connection.push(
        notification("items/saved", {
          turn_id: "t1",
          items: [
            logged(2, {
              id: "result",
              turn_id: "t1",
              kind: "tool_result",
              tool_result: { call_id: "call-1", content: "ok" },
            }),
          ],
        })
      )
      assert.equal(h.sync.$toolProgress.get()["call-1"], undefined)
    },
  },
  {
    name: "conversation_status_for_child_session",
    async run(h) {
      // 1. 对话订阅推来子 Session 的运行信息：$sessions 多出这个 Session。
      await h.start()
      h.connection.push(
        notification(
          "session/statusUpdated",
          { info: sessionInfo("child", rootSessionID) },
          { subscription: "conversation", session_id: "child" }
        )
      )
      assert.equal(
        h.sync.$sessions.get().child?.session.parent_session_id,
        rootSessionID
      )
      // 2. 其他 Session 的 Session 订阅通知不处理：不写 Item，subscription/closed 不重新同步。
      h.connection.push(
        notification(
          "item/started",
          {
            turn_id: "t9",
            item_index: 0,
            item: messageItem("child-reply", "t9", ""),
          },
          { session_id: "child" }
        )
      )
      h.connection.push(
        notification(
          "subscription/closed",
          { reason: "unsubscribed" },
          { session_id: "child" }
        )
      )
      await flush()
      assert.deepEqual(getKeys(h.sync), [])
      assert.equal(countCalls(h, "conversation/open"), 1)
    },
  },
  {
    name: "pending_inputs_come_from_snapshot_and_notifications_only",
    async run(h) {
      const pendingInputs = {
        steer_turn_id: "t1",
        inputs: [
          {
            item_id: "input-1",
            preview: "排队",
            image_count: 2,
            is_steer: false,
          },
        ],
      }
      h.connection.reply("session/subscribe", () => ({
        last_saved_ordinal: 0,
        streaming_items: [],
        pending_inputs: pendingInputs,
      }))
      await h.start()
      assert.deepEqual(h.sync.$pendingInputs.get(), pendingInputs)
      const content = [{ kind: "text" as const, text: { text: "你好" } }]
      await h.sync.submitInput(content, "steer")
      assert.deepEqual(h.connection.getParams("conversation/submitInput"), [
        { conversation_id: conversationID, mode: "steer", content },
      ])
      h.connection.push(
        notification("items/saved", {
          turn_id: "t1",
          items: [logged(1, messageItem("input-1", "t1", "你好"))],
        })
      )
      h.connection.push(
        notification("turn/finished", {
          turn_id: "t1",
          input_item_ids: ["input-1"],
        })
      )
      assert.deepEqual(h.sync.$pendingInputs.get(), pendingInputs)
      // 消费通知即删除，不等待消息保存。
      h.connection.push(
        notification("session/pendingInputsUpdated", {
          pending_inputs: { steer_turn_id: "t1", inputs: [] },
        })
      )
      assert.deepEqual(h.sync.$pendingInputs.get(), {
        steer_turn_id: "t1",
        inputs: [],
      })
    },
  },
  {
    name: "pending_notifications_staged_and_reconnect_replaces_snapshot",
    async run(h) {
      const queued = {
        inputs: [
          {
            item_id: "other-page",
            preview: "第二页提交",
            image_count: 0,
            is_steer: false,
          },
        ],
      }
      h.connection.reply("session/subscribe", () => ({
        last_saved_ordinal: 0,
        streaming_items: [],
        pending_inputs: queued,
      }))
      h.connection.reply("session/items/list", () => {
        h.connection.push(
          notification("session/pendingInputsUpdated", {
            pending_inputs: { inputs: [] },
          })
        )
        assert.deepEqual(h.sync.$pendingInputs.get(), { inputs: [] })
        return { items: [] }
      })
      await h.start()
      assert.deepEqual(h.sync.$pendingInputs.get(), { inputs: [] })
      h.connection.reply("session/subscribe", () => ({
        last_saved_ordinal: 0,
        streaming_items: [],
        pending_inputs: queued,
      }))
      h.connection.connect()
      await flush()
      assert.deepEqual(h.sync.$pendingInputs.get(), queued)
      // 其他 Session 的列表不会污染当前列表。
      h.connection.push(
        notification(
          "session/pendingInputsUpdated",
          { pending_inputs: { inputs: [] } },
          { session_id: "child" }
        )
      )
      assert.deepEqual(h.sync.$pendingInputs.get(), queued)
      h.connection.reply("session/subscribe", () => {
        throw new Error("sync failed")
      })
      h.connection.connect()
      await flush()
      assert.equal(h.sync.$syncState.get(), "failed")
      assert.deepEqual(h.sync.$pendingInputs.get(), queued)
    },
  },
  {
    name: "pending_operations_wait_for_server_list",
    async run(h) {
      const queued = {
        steer_turn_id: "t1",
        inputs: [
          {
            item_id: "input-1",
            preview: "排队",
            image_count: 1,
            is_steer: false,
          },
        ],
      }
      h.connection.reply("session/subscribe", () => ({
        last_saved_ordinal: 0,
        streaming_items: [],
        pending_inputs: queued,
      }))
      const content = [
        {
          kind: "image" as const,
          image: { data_url: "data:image/png;base64,a", alt: "图片" },
        },
      ]
      h.connection.reply("conversation/steerInput", () => ({}))
      h.connection.reply("conversation/interruptTurnWithInput", () => ({}))
      h.connection.reply("conversation/withdrawInput", (params) =>
        params.return_content ? { content } : {}
      )
      await h.start()
      await h.sync.steerInput("input-1", "t1")
      await h.sync.interruptTurnWithInput("t1", "input-1")
      assert.deepEqual(await h.sync.withdrawInput("input-1", true), content)
      assert.equal(await h.sync.withdrawInput("input-1", false), undefined)
      assert.deepEqual(h.sync.$pendingInputs.get(), queued)
    },
  },
  {
    name: "turn_finished_keeps_final_model_call",
    async run(h) {
      // 1. 接收不同结束通知，完成后的最终调用只取服务端传来的值。
      await h.start()
      const cases: Pick<
        TurnFinishedParams,
        "status" | "final_model_call_id" | "error"
      >[] = [
        { status: "completed", final_model_call_id: "final-call" },
        { status: "completed" },
        { status: "failed", error: "model failed" },
        { status: "interrupted", error: "context canceled" },
        { error: "save failed" },
      ]
      for (const params of cases) {
        h.connection.push(notification("turn/started", { turn_id: "t1" }))
        h.connection.push(
          notification("turn/finished", {
            turn_id: "t1",
            input_item_ids: [],
            ...params,
          })
        )
        assert.deepEqual(h.sync.$turns.get().t1, {
          status: params.status,
          finalModelCallID: params.final_model_call_id,
          error: params.error,
        })
      }
    },
  },
  {
    name: "turn_finished_invalidates_queries",
    async run(h) {
      // 1. Turn 结束：记下终态，让上下文用量、Turn 列表和对话列表失效。
      await h.start()
      h.connection.push(notification("turn/started", { turn_id: "t1" }))
      assert.equal(h.sync.$turns.get().t1?.status, "running")
      h.connection.push(
        notification("turn/finished", {
          turn_id: "t1",
          status: "completed",
          input_item_ids: [],
        })
      )
      assert.equal(h.sync.$turns.get().t1?.status, "completed")
      assert.deepEqual(h.getInvalidatedKeys(), [
        [serverID, "contextUsage"],
        [serverID, "turns"],
        [serverID, "conversations"],
      ])
      // 2. 标题变了：只让对话列表失效。
      h.connection.push(
        notification(
          "conversation/titleUpdated",
          { title: "新标题" },
          { subscription: "conversation", session_id: "" }
        )
      )
      assert.equal(h.sync.$title.get(), "新标题")
      assert.deepEqual(h.getInvalidatedKeys().at(-1), [
        serverID,
        "conversations",
      ])
    },
  },
  {
    name: "open_unknown_conversation_fails",
    async run(h) {
      // serve 拒绝打开时同步失败，错误交给页面显示。
      const notFound = new RpcError(
        ErrorCodeConversationNotFound,
        "conversation not found",
        "conversation/open"
      )
      h.connection.reply("conversation/open", () => {
        throw notFound
      })
      await h.start()
      assert.equal(h.sync.$syncState.get(), "failed")
      assert.equal(h.sync.$syncError.get(), notFound)
    },
  },
  {
    name: "connection_lost_waits_for_reconnect",
    async run(h) {
      // 1. 还没连上时请求以「连接已断开」失败，保持同步中。
      h.connection.reply("conversation/open", () => {
        throw new RpcError(
          rpcConnectionLostCode,
          "connection lost",
          "conversation/open"
        )
      })
      await h.start()
      assert.equal(h.sync.$syncState.get(), "syncing")
      // 2. 连上后重新同步成功。
      h.connection.reply("conversation/open", (params) => ({
        conversation: conversationRecord(params.conversation_id),
      }))
      h.connection.connect()
      await flush()
      assert.equal(h.sync.$syncState.get(), "synced")
    },
  },
]

for (const { name, run } of tests) test(name, () => run(createHarness()))

for (const itemCount of [1, 3]) {
  test(`streaming_deltas_publish_once_per_interval_for_${itemCount}_items`, async (t) => {
    const h = createHarness()
    const stop = await h.start()
    t.mock.timers.enable({ apis: ["setTimeout"] })
    try {
      // 1. 已保存历史之后同时生成消息、工具参数和推理。
      h.connection.push(
        notification("items/saved", {
          turn_id: "t0",
          items: loggedMessages(
            ...Array.from({ length: 100 }, (_, index) => index + 1)
          ),
        })
      )
      const items: Item[] = [
        messageItem("reply", "t1", ""),
        toolCallItem("call", "t1", "call-1"),
        {
          id: "reasoning",
          turn_id: "t1",
          kind: "reasoning",
          reasoning: { status: "completed" },
        },
      ]
      for (const [itemIndex, item] of items.slice(0, itemCount).entries())
        h.connection.push(
          notification("item/started", {
            turn_id: "t1",
            item_index: itemIndex,
            item,
          })
        )
      const before = h.sync.$items.get()
      const beforeValue = structuredClone(before)
      const listener = mock.fn()
      const stopListening = h.sync.$items.listen(listener)
      t.after(stopListening)
      // 2. 一个间隔内交错发送每项 100 条增量；旧快照不变，还没有发布。
      const chunks = Array.from({ length: 100 }, (_, index) => `${index},`)
      for (const chunk of chunks)
        for (let itemIndex = 0; itemIndex < itemCount; itemIndex++)
          h.connection.push(
            notification("item/delta", {
              turn_id: "t1",
              item_index: itemIndex,
              delta: {
                text: chunk,
                arguments: chunk,
                summary: [chunk],
                content: [chunk],
              },
            })
          )
      assert.equal(h.sync.$items.get(), before)
      assert.deepEqual(before, beforeValue)
      assert.equal(listener.mock.callCount(), 0)
      t.mock.timers.tick(49)
      assert.equal(listener.mock.callCount(), 0)
      // 3. 间隔到期提交一次，增量顺序完整，历史引用及此前快照保持不变。
      t.mock.timers.tick(1)
      assert.equal(listener.mock.callCount(), 1)
      const after = h.sync.$items.get()
      assert.equal(getText(after[100]), chunks.join(""))
      if (itemCount === 3) {
        assert.equal(after[101].item.tool_call?.arguments, chunks.join(""))
        assert.deepEqual(after[102].item.reasoning?.summary, chunks)
        assert.deepEqual(after[102].item.reasoning?.content, chunks)
      }
      for (let index = 0; index < 100; index++)
        assert.equal(after[index], before[index])
      assert.deepEqual(before, beforeValue)
      h.connection.push(textDelta("t1", 0, "next"))
      t.mock.timers.tick(50)
      assert.equal(listener.mock.callCount(), 2)
      assert.equal(getText(h.sync.$items.get()[100]), chunks.join("") + "next")
    } finally {
      stop()
    }
  })
}

for (const event of [
  "item/started",
  "item/completed",
  "item/interrupted",
  "items/saved",
  "turn/finished",
  "stop",
  "reconnect",
  "subscription/closed",
] as const) {
  test(`pending_deltas_flush_before_${event}`, async (t) => {
    const h = createHarness()
    const stop = await h.start()
    t.mock.timers.enable({ apis: ["setTimeout"] })
    try {
      h.connection.push(
        notification("item/started", {
          turn_id: "t1",
          item_index: 0,
          item: messageItem("reply", "t1", "before"),
        })
      )
      h.connection.push(textDelta("t1", 0, "-delta"))
      const observed: (string | undefined)[] = []
      const stopListening = h.sync.$items.listen((items) =>
        observed.push(getText(items[0]))
      )
      t.after(stopListening)
      const final = messageItem("reply", "t1", "final")
      // 1. 生命周期通知不等待提交间隔；此前增量先交付，完整记录随后替换。
      switch (event) {
        case "item/started":
        case "item/completed":
          h.connection.push(
            notification(event, { turn_id: "t1", item_index: 0, item: final })
          )
          break
        case "item/interrupted":
          h.connection.push(
            notification(event, { turn_id: "t1", item_index: 0 })
          )
          break
        case "items/saved":
          h.connection.push(
            notification(event, { turn_id: "t1", items: [logged(1, final)] })
          )
          break
        case "turn/finished":
          h.connection.push(
            notification(event, {
              turn_id: "t1",
              status: "completed",
              input_item_ids: [],
            })
          )
          assert.equal(h.sync.$turns.get().t1.status, "completed")
          break
        case "stop":
          stop()
          break
        case "reconnect":
        case "subscription/closed":
          h.connection.reply("session/subscribe", () => ({
            pending_inputs: { inputs: [] },
            last_saved_ordinal: 0,
            streaming_items: [{ item_index: 0, item: final }],
          }))
          if (event === "reconnect") h.connection.connect()
          else
            h.connection.push(
              notification(event, { reason: "subscriber too slow" })
            )
          break
      }
      assert.equal(observed[0], "before-delta")
      await flush()
      const expected =
        event === "item/interrupted"
          ? undefined
          : event === "stop" || event === "turn/finished"
            ? "before-delta"
            : "final"
      assert.equal(getText(h.sync.$items.get()[0]), expected)
      // 2. 原定时器不能再发布，停止期间的新通知也不写入。
      const settled = h.sync.$items.get()
      const count = observed.length
      if (event === "stop") h.connection.push(textDelta("t1", 0, "stopped"))
      t.mock.timers.tick(100)
      assert.equal(h.sync.$items.get(), settled)
      assert.equal(observed.length, count)
    } finally {
      stop()
    }
  })
}

test("pending_deltas_find_their_item_after_history_is_prepended", async (t) => {
  const h = createHarness()
  h.connection.reply("session/subscribe", () => ({
    pending_inputs: { inputs: [] },
    last_saved_ordinal: 2,
    streaming_items: [],
  }))
  h.connection.reply("session/items/list", () => ({
    items: loggedMessages(2),
    next_cursor: 2,
  }))
  const stop = await h.start()
  t.mock.timers.enable({ apis: ["setTimeout"] })
  try {
    // 1. 增量暂存期间插入更早的历史，目标 Item 在数组中的位置改变。
    h.connection.push(
      notification("item/started", {
        turn_id: "t1",
        item_index: 0,
        item: messageItem("reply", "t1", ""),
      })
    )
    h.connection.push(textDelta("t1", 0, "a"))
    h.connection.reply("session/items/list", () => ({
      items: loggedMessages(1),
    }))
    await h.sync.loadEarlierItems()
    h.connection.push(textDelta("t1", 0, "b"))
    // 2. 提交时仍更新原消息，不覆盖插入的历史。
    t.mock.timers.tick(50)
    assert.deepEqual(getKeys(h.sync), ["item-1", "item-2", "reply"])
    assert.deepEqual(h.sync.$items.get().map(getText), [
      "第 1 条",
      "第 2 条",
      "ab",
    ])
  } finally {
    stop()
  }
})

// 真实订阅窗口里 Turn 可以先结束，再收到 Session 的终态；不要求漏掉的 turn/finished 重发。
for (const { status, duringSync } of (
  ["completed", "failed", "interrupted"] as const
).flatMap((status) =>
  [true, false].map((duringSync) => ({ status, duringSync }))
)) {
  test(`session_terminal_status_reconciles_running_turn_${status}_${duringSync ? "during_sync" : "after_sync"}`, async () => {
    const h = createHarness()
    h.connection.reply("conversation/subscribe", () => ({
      title: "订阅时执行中",
      sessions: [
        {
          ...sessionInfo(rootSessionID),
          state: "running",
          active_turn_id: "t1",
        },
      ],
    }))
    const finished = notification(
      "session/statusUpdated",
      {
        info: {
          ...sessionInfo(rootSessionID),
          last_turn_id: "t1",
          last_turn_status: status,
        },
      },
      { subscription: "conversation" }
    )
    if (duringSync)
      h.connection.reply("session/items/list", () => {
        h.connection.push(finished)
        return { items: [] }
      })
    const stop = await h.start()
    try {
      if (!duringSync) {
        assert.equal(h.sync.$turns.get().t1?.status, "running")
        h.connection.push(finished)
      }
      assert.equal(h.sync.$turns.get().t1?.status, status)
      assert.equal(h.sync.$sessions.get()[rootSessionID].state, "idle")
      assert.equal(h.sync.$syncState.get(), "synced")
    } finally {
      stop()
    }
  })
}

for (const terminal of ["completed", "failed", "interrupted"] as const) {
  for (const source of ["initial", "status", "finished"] as const) {
    test(`remember_root_result_${terminal}_${source}_while_following_child`, async () => {
      $conversationResults.set({})
      const h = createHarness()
      const root = {
        ...sessionInfo(rootSessionID),
        last_turn_id: "root-turn",
        last_turn_status: terminal,
      }
      h.connection.reply("conversation/subscribe", () => ({
        title: "root-result",
        sessions: [
          source === "initial" ? root : sessionInfo(rootSessionID),
          sessionInfo("child", rootSessionID),
        ],
      }))
      const sync = new ConversationSync(
        h.connection,
        h.queryClient,
        conversationID,
        source === "finished" ? undefined : "child"
      )
      const stop = sync.run()
      await flush()
      try {
        assert.equal(sync.hasInitialData(), true)
        if (source === "status")
          h.connection.push(
            notification(
              "session/statusUpdated",
              { info: root },
              { subscription: "conversation" }
            )
          )
        if (source === "finished")
          h.connection.push(
            notification("turn/finished", {
              turn_id: "root-turn",
              status: terminal,
              input_item_ids: [],
            })
          )
        const key = getConversationResultKey(serverID, conversationID)
        assert.deepEqual($conversationResults.get()[key], {
          turnID: "root-turn",
          status: terminal,
        })
        h.connection.push(
          notification(
            "session/statusUpdated",
            {
              info: {
                ...sessionInfo("child", rootSessionID),
                last_turn_id: "child-turn",
                last_turn_status: "failed",
              },
            },
            { subscription: "conversation", session_id: "child" }
          )
        )
        assert.deepEqual($conversationResults.get()[key], {
          turnID: "root-turn",
          status: terminal,
        })
        assert.equal(
          h.connection.getMethods().includes("session/turns/list"),
          false
        )
      } finally {
        stop()
      }
    })
  }
}

for (const stage of [
  "open",
  "subscribe",
  "history",
  "reconnect",
  "pagination",
] as const) {
  test(`switch_A_B_A_ignores_late_${stage}_reply_and_keeps_only_target_notifications`, async () => {
    const h = createHarness()
    let resolveReply!: () => void
    const wait = new Promise<void>((resolve) => {
      resolveReply = resolve
    })
    if (stage === "pagination") {
      h.connection.reply("session/subscribe", () => ({
        pending_inputs: { inputs: [] },
        last_saved_ordinal: 3,
        streaming_items: [],
      }))
      h.connection.reply("session/items/list", () => ({
        items: loggedMessages(3),
        next_cursor: 3,
      }))
    }
    let stop: () => void
    let pagination: Promise<void> | undefined
    if (stage === "reconnect" || stage === "pagination") {
      stop = await h.start()
      if (stage === "pagination") {
        h.connection.reply("session/items/list", async () => {
          await wait
          return { items: loggedMessages(2, 1) }
        })
        pagination = h.sync.loadEarlierItems()
        void pagination.catch(() => undefined)
      } else {
        h.connection.reply("conversation/open", async (params) => {
          await wait
          return { conversation: conversationRecord(params.conversation_id) }
        })
        h.connection.connect()
      }
    } else {
      const method =
        stage === "open"
          ? "conversation/open"
          : stage === "subscribe"
            ? "conversation/subscribe"
            : "session/items/list"
      if (method === "conversation/open")
        h.connection.reply(method, async (params) => {
          await wait
          return { conversation: conversationRecord(params.conversation_id) }
        })
      if (method === "conversation/subscribe")
        h.connection.reply(method, async () => {
          await wait
          return { title: "old", sessions: [sessionInfo(rootSessionID)] }
        })
      if (method === "session/items/list")
        h.connection.reply(method, async () => {
          await wait
          return { items: loggedMessages(1) }
        })
      stop = h.sync.run()
    }
    await flush()
    stop()
    const itemsBefore = h.sync.$items.get()
    h.connection.reply("conversation/open", (params) => ({
      conversation: conversationRecord(params.conversation_id),
    }))
    h.connection.reply("conversation/subscribe", () => ({
      title: "new",
      sessions: [sessionInfo(rootSessionID)],
    }))
    h.connection.reply("session/subscribe", () => ({
      pending_inputs: { inputs: [] },
      last_saved_ordinal: 0,
      streaming_items: [],
    }))
    h.connection.reply("session/items/list", () => ({ items: [] }))
    const other = new ConversationSync(
      h.connection,
      h.queryClient,
      "B",
      "child"
    )
    const stopOther = other.run()
    await flush()
    stopOther()
    const returned = new ConversationSync(
      h.connection,
      h.queryClient,
      conversationID
    )
    const stopReturned = returned.run()
    await flush()
    try {
      resolveReply()
      if (pagination) await pagination
      await flush()
      h.connection.push(
        notification("item/started", {
          turn_id: "new",
          item_index: 0,
          item: messageItem("target-item", "new", "target"),
        })
      )
      h.connection.push(
        notification(
          "subscription/closed",
          { reason: "old child" },
          { session_id: "child" }
        )
      )
      await flush()
      assert.equal(h.sync.$items.get(), itemsBefore)
      assert.deepEqual(getKeys(other), [])
      assert.deepEqual(getKeys(returned), ["target-item"])
      assert.equal(returned.$syncState.get(), "synced")
    } finally {
      stopReturned()
    }
  })
}

// 有 rootSync 的同步只订阅自己 Session 的内容，对话级订阅留给根 Session 的同步。
test("session_only_sync_leaves_the_conversation_subscription_to_the_root_sync", async () => {
  // 1. 根 Session 的同步先同步完，对话里有一个正在执行 t1 的子 Session。
  const h = createHarness()
  h.connection.reply("conversation/subscribe", () => ({
    title: "标题",
    sessions: [
      sessionInfo(rootSessionID),
      { ...sessionInfo("child", rootSessionID), active_turn_id: "t1" },
    ],
  }))
  const stopRoot = await h.start()
  const sync = new ConversationSync(
    h.connection,
    h.queryClient,
    conversationID,
    "child",
    h.sync
  )
  let stop = () => {}
  // 2. 每一步只看这一步发出的请求。
  const steps: {
    name: string
    act(): void | Promise<void>
    wantCalls: [string, unknown][]
    check(): void
  }[] = [
    {
      name: "starting subscribes only to its own session",
      act() {
        stop = sync.run()
      },
      wantCalls: [
        ["conversation/open", { conversation_id: conversationID }],
        [
          "session/unsubscribe",
          { conversation_id: conversationID, session_id: "child" },
        ],
        [
          "session/subscribe",
          { conversation_id: conversationID, session_id: "child" },
        ],
        [
          "session/items/list",
          {
            conversation_id: conversationID,
            session_id: "child",
            order: "desc",
            limit: 50,
          },
        ],
      ],
      check() {
        // 标题和 Session 信息就是根 Session 的同步的，执行中的 Turn 从中读到。
        assert.equal(sync.$syncState.get(), "synced")
        assert.equal(sync.$title, h.sync.$title)
        assert.equal(sync.$sessions, h.sync.$sessions)
        assert.equal(sync.$title.get(), "标题")
        assert.equal(sync.$turns.get().t1?.status, "running")
      },
    },
    {
      name: "conversation notifications are handled by the root sync only and its session info drives the turns",
      act() {
        h.connection.push(
          notification(
            "conversation/titleUpdated",
            { title: "新标题" },
            { subscription: "conversation", session_id: "" }
          )
        )
        h.connection.push(
          notification(
            "session/statusUpdated",
            {
              info: {
                ...sessionInfo("child", rootSessionID),
                last_turn_id: "t1",
                last_turn_status: "completed",
              },
            },
            { subscription: "conversation", session_id: "child" }
          )
        )
      },
      wantCalls: [],
      check() {
        assert.equal(sync.$title.get(), "新标题")
        assert.equal(h.sync.$title.get(), "新标题")
        assert.equal(sync.$sessions.get().child.last_turn_id, "t1")
        // 只有根 Session 的同步处理了这两条通知，各让查询失效一次。
        assert.equal(
          h.getInvalidatedKeys().filter((key) => key?.[1] === "conversations")
            .length,
          1
        )
        // 根同步写入的 Session 信息里 t1 已结束，只读 Session 内容的同步的 Turn 状态跟着它。
        assert.equal(sync.$turns.get().t1?.status, "completed")
      },
    },
    {
      name: "session notifications still reach the session-only sync",
      act() {
        h.connection.push(
          notification(
            "item/started",
            {
              turn_id: "t1",
              item_index: 0,
              item: messageItem("child-item", "t1", "子"),
            },
            { session_id: "child" }
          )
        )
      },
      wantCalls: [],
      check() {
        assert.deepEqual(getKeys(sync), ["child-item"])
        assert.deepEqual(getKeys(h.sync), [])
      },
    },
    {
      name: "stopping leaves the root subscription intact",
      act() {
        stop()
      },
      wantCalls: [
        [
          "session/unsubscribe",
          { conversation_id: conversationID, session_id: "child" },
        ],
      ],
      check() {
        assert.equal(h.sync.$syncState.get(), "synced")
      },
    },
  ]
  try {
    for (const step of steps) {
      h.connection.clearCalls()
      await step.act()
      await flush()
      assert.deepEqual(h.connection.getCalls(), step.wantCalls, step.name)
      step.check()
    }
  } finally {
    stop()
    stopRoot()
  }
})

// 根 Session 的同步还没第一次同步完时，只读 Session 内容的同步等它，才读得到对话里的 Session 信息。
test("session_only_sync_waits_for_the_root_sync_first_sync", async () => {
  const h = createHarness()
  let releaseSubscribe = () => {}
  h.connection.reply(
    "conversation/subscribe",
    async () =>
      new Promise((resolve) => {
        releaseSubscribe = () =>
          resolve({
            title: "标题",
            sessions: [
              sessionInfo(rootSessionID),
              { ...sessionInfo("child", rootSessionID), active_turn_id: "t1" },
            ],
          })
      })
  )
  const stopRoot = h.sync.run()
  const sync = new ConversationSync(
    h.connection,
    h.queryClient,
    conversationID,
    "child",
    h.sync
  )
  const stop = sync.run()
  try {
    await flush()
    assert.equal(sync.$syncState.get(), "syncing")
    assert.equal(sync.hasInitialData(), false)
    releaseSubscribe()
    await flush()
    assert.equal(h.sync.$syncState.get(), "synced")
    assert.equal(sync.$syncState.get(), "synced")
    assert.equal(sync.$turns.get().t1?.status, "running")
    assert.equal(countCalls(h, "conversation/subscribe"), 1)
  } finally {
    stop()
    stopRoot()
  }
})

// 断线期间 Turn 的状态变了：重连后根 Session 的同步写入新的 Session 信息，只读 Session 内容的同步的 Turn 状态跟着它，
// 不依赖两个同步重连完成的先后。
test("session_only_sync_turns_follow_the_root_session_info_after_reconnect", async () => {
  const cases: {
    name: string
    before: Partial<SessionRuntimeInfo>
    after: Partial<SessionRuntimeInfo>
    wantBefore: Record<string, { status: string }>
    wantAfter: Record<string, { status: string }>
  }[] = [
    {
      name: "turn finished during the disconnect",
      before: { active_turn_id: "t1" },
      after: { last_turn_id: "t1", last_turn_status: "completed" },
      wantBefore: { t1: { status: "running" } },
      wantAfter: { t1: { status: "completed" } },
    },
    {
      name: "new turn started during the disconnect",
      before: { last_turn_id: "t1", last_turn_status: "completed" },
      after: {
        last_turn_id: "t1",
        last_turn_status: "completed",
        active_turn_id: "t2",
      },
      wantBefore: { t1: { status: "completed" } },
      wantAfter: { t1: { status: "completed" }, t2: { status: "running" } },
    },
  ]
  for (const each of cases) {
    const h = createHarness()
    let subscribed = 0
    h.connection.reply("conversation/subscribe", () => ({
      title: "标题",
      sessions: [
        sessionInfo(rootSessionID),
        {
          ...sessionInfo("child", rootSessionID),
          ...(subscribed++ === 0 ? each.before : each.after),
        },
      ],
    }))
    const stopRoot = await h.start()
    const sync = new ConversationSync(
      h.connection,
      h.queryClient,
      conversationID,
      "child",
      h.sync
    )
    const stop = sync.run()
    try {
      await flush()
      assert.deepEqual(sync.$turns.get(), each.wantBefore, each.name)
      // 重连：根 Session 的同步多一次对话级订阅，晚于只读同步完成，新的 Session 信息才写进来。
      h.connection.connect()
      await flush()
      assert.deepEqual(
        h.sync.$sessions.get().child,
        {
          ...sessionInfo("child", rootSessionID),
          ...each.after,
        },
        each.name
      )
      assert.deepEqual(sync.$turns.get(), each.wantAfter, each.name)
      assert.equal(sync.$syncState.get(), "synced", each.name)
    } finally {
      stop()
      stopRoot()
    }
  }
})
