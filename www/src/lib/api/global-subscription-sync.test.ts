import assert from "node:assert/strict"
import { test, type TestContext } from "node:test"
import { atom, computed, type ReadableAtom } from "nanostores"
import { RpcError } from "../rpc-error"
import type { GlobalSubscriptionConnection } from "./global-subscription-sync"
import type { MethodParams, TerminalItem } from "./protocol.gen"
import { RunningConversationsSync } from "./running-conversations-sync"
import { RunningTerminalsSync } from "./running-terminals-sync"
import type {
  ConnectionState,
  GlobalServerNotification,
} from "./server-connection"

type FakeTimer = { callback(): void; delayMs: number; isCleared: boolean }
type Deps = {
  setTimeout(callback: () => void, delayMs: number): unknown
  clearTimeout(handle: unknown): void
}

// Subject 是被测的一种全局订阅：用 ID 列表表示它同步到的值，未知时是 undefined。
interface Subject {
  run(): () => void
  $ids: ReadableAtom<string[] | undefined>
}

// SubscriptionCase 描述共用的同步类被两种订阅使用时的差别：方法名、通知和值的形状。
interface SubscriptionCase {
  name: string
  subscribeMethod: keyof MethodParams
  unsubscribeMethod: keyof MethodParams
  create(connection: FakeConnection, deps: Deps): Subject
  // 订阅请求的回复、完整值通知和关闭通知
  getResult(ids: string[]): unknown
  getChangedNotification(ids: string[]): GlobalServerNotification
  getClosedNotification(): GlobalServerNotification
}

function createTerminalItem(terminalID: string): TerminalItem {
  return {
    terminal_id: terminalID,
    executor_id: "local",
    directory: "/work",
    started_at: "2026-11-06T00:00:00Z",
  }
}

const subscriptionCases: SubscriptionCase[] = [
  {
    name: "running_conversations",
    subscribeMethod: "conversation/running/subscribe",
    unsubscribeMethod: "conversation/running/unsubscribe",
    create(connection, deps) {
      const sync = new RunningConversationsSync(connection, deps)
      return {
        run: () => sync.run(),
        $ids: computed(sync.$conversationIDs, (ids) =>
          ids ? [...ids] : undefined
        ),
      }
    },
    getResult: (ids) => ({ conversation_ids: ids }),
    getChangedNotification: (ids) => ({
      method: "conversation/runningChanged",
      params: { conversation_ids: ids },
    }),
    getClosedNotification: () => ({
      method: "conversation/runningClosed",
      params: { reason: "slow subscription" },
    }),
  },
  {
    name: "running_terminals",
    subscribeMethod: "terminal/subscribe",
    unsubscribeMethod: "terminal/unsubscribe",
    create(connection, deps) {
      const sync = new RunningTerminalsSync(connection, deps)
      return {
        run: () => sync.run(),
        $ids: computed(sync.$terminals, (terminals) =>
          terminals?.map((terminal) => terminal.terminal_id)
        ),
      }
    },
    getResult: (ids) => ({ terminals: ids.map(createTerminalItem) }),
    getChangedNotification: (ids) => ({
      method: "terminal/listChanged",
      params: { terminals: ids.map(createTerminalItem) },
    }),
    getClosedNotification: () => ({
      method: "terminal/listClosed",
      params: { reason: "slow subscription" },
    }),
  },
]

// 内存连接按测试控制响应完成顺序，并发送真实形状的全局通知。
class FakeConnection implements GlobalSubscriptionConnection {
  readonly $state = atom<ConnectionState>("connected")
  readonly $serverID = atom<string | undefined>("server-1")
  readonly calls: { method: keyof MethodParams; params: unknown }[] = []
  readonly subscriptions: {
    resolve(result: unknown): void
    reject(error: unknown): void
  }[] = []
  private readonly notificationHandlers = new Set<
    (notification: GlobalServerNotification) => void
  >()
  private readonly connectedHandlers = new Set<() => void>()
  private readonly subscription: SubscriptionCase

  constructor(subscription: SubscriptionCase) {
    this.subscription = subscription
  }

  call = ((method: keyof MethodParams, params: unknown) => {
    this.calls.push({ method, params })
    if (method === this.subscription.unsubscribeMethod)
      return Promise.resolve({})
    assert.equal(method, this.subscription.subscribeMethod)
    return new Promise((resolve, reject) => {
      this.subscriptions.push({ resolve, reject })
    })
  }) as GlobalSubscriptionConnection["call"]

  listenServer(handler: (notification: GlobalServerNotification) => void) {
    this.notificationHandlers.add(handler)
    return () => this.notificationHandlers.delete(handler)
  }

  onConnected(handler: () => void) {
    this.connectedHandlers.add(handler)
    return () => this.connectedHandlers.delete(handler)
  }

  push(notification: GlobalServerNotification) {
    for (const handler of [...this.notificationHandlers]) handler(notification)
  }

  connect(serverID = "server-1") {
    this.$serverID.set(serverID)
    this.$state.set("connected")
    for (const handler of [...this.connectedHandlers]) handler()
  }

  reply(index: number, ids: string[]) {
    this.subscriptions[index].resolve(this.subscription.getResult(ids))
  }
}

function createHarness(subscription: SubscriptionCase) {
  const connection = new FakeConnection(subscription)
  const timers: FakeTimer[] = []
  const subject = subscription.create(connection, {
    setTimeout(callback, delayMs) {
      const timer = { callback, delayMs, isCleared: false }
      timers.push(timer)
      return timer
    },
    clearTimeout(handle) {
      ;(handle as FakeTimer).isCleared = true
    },
  })
  return {
    connection,
    subject,
    changed: (ids: string[]) => subscription.getChangedNotification(ids),
    closed: () => subscription.getClosedNotification(),
    fireRetryTimer() {
      const timer = timers.findLast((timer) => !timer.isCleared)
      assert.ok(timer)
      timer.isCleared = true
      timer.callback()
    },
    getPendingTimers() {
      return timers.filter((timer) => !timer.isCleared)
    },
  }
}

type Harness = ReturnType<typeof createHarness>

function flush(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve))
}

// 每种订阅各跑一遍同一组场景
function testEachSubscription(
  name: string,
  run: (
    subscription: SubscriptionCase,
    harness: Harness,
    t: TestContext
  ) => Promise<void>
) {
  test(name, async (t) => {
    for (const subscription of subscriptionCases)
      await t.test(subscription.name, (subTest) =>
        run(subscription, createHarness(subscription), subTest)
      )
  })
}

testEachSubscription(
  "initial_and_changed_values_replace_members_without_turning_unknown_into_empty",
  async (subscription, { connection, subject, changed }) => {
    const cases = [
      { name: "empty_initial", initial: [], changes: [["a"], ["b"], []] },
      { name: "running_initial", initial: ["a", "b"], changes: [["b"], []] },
    ]
    for (const scenario of cases) {
      const stop = subject.run()
      assert.equal(subject.$ids.get(), undefined, scenario.name)
      assert.deepEqual(
        connection.calls.at(-1),
        { method: subscription.subscribeMethod, params: {} },
        scenario.name
      )
      connection.reply(connection.subscriptions.length - 1, scenario.initial)
      await flush()
      assert.deepEqual(subject.$ids.get(), scenario.initial, scenario.name)
      for (const ids of scenario.changes) {
        connection.push(changed(ids))
        assert.deepEqual(subject.$ids.get(), ids, scenario.name)
      }
      stop()
      assert.equal(subject.$ids.get(), undefined, scenario.name)
      assert.deepEqual(
        connection.calls.at(-1),
        { method: subscription.unsubscribeMethod, params: {} },
        scenario.name
      )
    }
  }
)

testEachSubscription(
  "notifications_during_initial_subscribe_are_applied_after_initial_in_receive_order",
  async (_, { connection, subject, changed }) => {
    const received: (readonly string[] | undefined)[] = []
    const stopObserving = subject.$ids.listen((ids) => received.push(ids))
    const stop = subject.run()
    connection.reply(0, ["initial"])
    connection.push(changed(["later", "newest"]))
    connection.push(changed([]))
    assert.equal(subject.$ids.get(), undefined)
    await flush()
    assert.deepEqual(received, [["initial"], ["later", "newest"], []])
    stopObserving()
    stop()
  }
)

testEachSubscription(
  "disconnect_is_unknown_and_reconnect_replaces_the_previous_value",
  async (_, { connection, subject, changed }) => {
    const stop = subject.run()
    connection.reply(0, ["old"])
    await flush()
    connection.$state.set("reconnecting")
    assert.equal(subject.$ids.get(), undefined)
    connection.push(changed(["stale"]))
    assert.equal(subject.$ids.get(), undefined)
    assert.equal(connection.subscriptions.length, 1)
    connection.connect()
    assert.equal(connection.subscriptions.length, 2)
    connection.reply(1, [])
    await flush()
    assert.deepEqual(subject.$ids.get(), [])
    stop()
  }
)

testEachSubscription(
  "old_sync_results_do_not_replace_new_results",
  async (subscription, _, t) => {
    const cases = [
      "reconnect",
      "server_identity",
      "subscription_closed",
      "cleanup_and_run",
      "repeat_run",
    ] as const
    for (const scenario of cases)
      await t.test(scenario, async () => {
        const { connection, subject } = createHarness(subscription)
        let stop = subject.run()
        switch (scenario) {
          case "reconnect":
            connection.$state.set("reconnecting")
            connection.connect()
            break
          case "server_identity":
            connection.$state.set("reconnecting")
            connection.connect("server-2")
            break
          case "subscription_closed":
            connection.push(subscription.getClosedNotification())
            break
          case "cleanup_and_run":
            stop()
            stop = subject.run()
            break
          case "repeat_run": {
            const oldStop = stop
            stop = subject.run()
            oldStop()
            break
          }
        }
        assert.equal(connection.subscriptions.length, 2)
        connection.reply(1, ["new"])
        await flush()
        connection.reply(0, ["old"])
        await flush()
        assert.deepEqual(subject.$ids.get(), ["new"])
        stop()
        stop()
        const callsBefore = connection.calls.length
        connection.connect()
        connection.push(subscription.getChangedNotification(["stale"]))
        connection.push(subscription.getClosedNotification())
        await flush()
        assert.equal(connection.calls.length, callsBefore)
        assert.equal(subject.$ids.get(), undefined)
      })
  }
)

testEachSubscription(
  "closed_notification_recovers_only_this_subscription_with_capped_backoff",
  async (subscription, harness) => {
    const { connection, subject } = harness
    const stop = subject.run()
    connection.reply(0, ["a"])
    await flush()
    connection.push(harness.closed())
    assert.equal(subject.$ids.get(), undefined)
    assert.equal(connection.subscriptions.length, 2)
    // 订阅一直失败：每次失败后只留一个重试计时，等待时间 1、2、4、8 秒，之后封顶 10 秒。
    const delays: number[] = []
    for (let attempt = 0; attempt < 6; attempt++) {
      connection.subscriptions
        .at(-1)!
        .reject(
          new RpcError(
            -32000,
            "runtime unavailable",
            subscription.subscribeMethod
          )
        )
      await flush()
      assert.equal(subject.$ids.get(), undefined)
      assert.equal(harness.getPendingTimers().length, 1)
      delays.push(harness.getPendingTimers()[0].delayMs)
      harness.fireRetryTimer()
    }
    assert.deepEqual(delays, [1000, 2000, 4000, 8000, 10000, 10000])
    connection.reply(connection.subscriptions.length - 1, ["restored"])
    await flush()
    assert.deepEqual(subject.$ids.get(), ["restored"])
    assert.ok(
      connection.calls.every(
        (call) => call.method === subscription.subscribeMethod
      )
    )
    assert.equal(connection.$state.get(), "connected")
    stop()
  }
)

testEachSubscription(
  "other_subscriptions_notifications_are_ignored",
  async (subscription, { connection, subject, changed }) => {
    const other = subscriptionCases.find(
      (each) => each.name !== subscription.name
    )!
    const stop = subject.run()
    connection.reply(0, ["a"])
    await flush()
    connection.push(other.getChangedNotification(["x"]))
    connection.push(other.getClosedNotification())
    assert.deepEqual(subject.$ids.get(), ["a"])
    assert.equal(connection.subscriptions.length, 1)
    connection.push(changed(["b"]))
    assert.deepEqual(subject.$ids.get(), ["b"])
    stop()
  }
)

testEachSubscription(
  "retry_is_cancelled_on_disconnect_or_stop",
  async (subscription, _, t) => {
    for (const scenario of ["disconnect", "stop"] as const)
      await t.test(scenario, async () => {
        const harness = createHarness(subscription)
        const { connection, subject } = harness
        const stop = subject.run()
        connection.subscriptions[0].reject(new Error("runtime unavailable"))
        await flush()
        const retry = harness.getPendingTimers()[0]
        assert.ok(retry)
        if (scenario === "disconnect") connection.$state.set("reconnecting")
        else stop()
        assert.equal(harness.getPendingTimers().length, 0)
        retry.callback()
        await flush()
        assert.equal(connection.subscriptions.length, 1)
        if (scenario === "disconnect") {
          connection.connect()
          assert.equal(connection.subscriptions.length, 2)
          connection.reply(1, [])
          await flush()
          assert.deepEqual(subject.$ids.get(), [])
        }
        stop()
      })
  }
)

testEachSubscription(
  "run_while_disconnected_waits_for_connected_callback",
  async (_, { connection, subject }) => {
    connection.$state.set("reconnecting")
    const stop = subject.run()
    assert.equal(connection.subscriptions.length, 0)
    connection.connect()
    assert.equal(connection.subscriptions.length, 1)
    connection.reply(0, [])
    await flush()
    assert.deepEqual(subject.$ids.get(), [])
    stop()
  }
)
