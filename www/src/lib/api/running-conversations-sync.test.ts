import assert from "node:assert/strict"
import { test } from "node:test"
import { atom } from "nanostores"
import { RpcError } from "../rpc-error"
import type { MethodParams, MethodResult } from "./protocol.gen"
import {
  RunningConversationsSync,
  type RunningConversationsConnection,
} from "./running-conversations-sync"
import type {
  ConnectionState,
  GlobalServerNotification,
} from "./server-connection"

type SubscriptionReply = MethodResult["conversation/running/subscribe"]

// 内存连接按测试控制响应完成顺序，并发送真实形状的全局通知。
class FakeRunningConnection implements RunningConversationsConnection {
  readonly $state = atom<ConnectionState>("connected")
  readonly $serverID = atom<string | undefined>("server-1")
  readonly calls: { method: keyof MethodParams; params: unknown }[] = []
  readonly subscriptions: {
    resolve(result: SubscriptionReply): void
    reject(error: unknown): void
  }[] = []
  private readonly notificationHandlers = new Set<
    (notification: GlobalServerNotification) => void
  >()
  private readonly connectedHandlers = new Set<() => void>()

  call<M extends keyof MethodParams>(
    method: M,
    params: MethodParams[M]
  ): Promise<MethodResult[M]> {
    this.calls.push({ method, params })
    if (method === "conversation/running/unsubscribe")
      return Promise.resolve({} as MethodResult[M])
    assert.equal(method, "conversation/running/subscribe")
    return new Promise<SubscriptionReply>((resolve, reject) => {
      this.subscriptions.push({ resolve, reject })
    }) as Promise<MethodResult[M]>
  }

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

  reply(index: number, conversationIDs: string[]) {
    this.subscriptions[index].resolve({ conversation_ids: conversationIDs })
  }
}

type FakeTimer = { callback(): void; delayMs: number; isCleared: boolean }

function createHarness() {
  const connection = new FakeRunningConnection()
  const timers: FakeTimer[] = []
  const sync = new RunningConversationsSync(connection, {
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
    sync,
    timers,
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

function flush(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve))
}

function changed(conversationIDs: string[]): GlobalServerNotification {
  return {
    method: "conversation/runningChanged",
    params: { conversation_ids: conversationIDs },
  }
}

function closed(): GlobalServerNotification {
  return {
    method: "conversation/runningClosed",
    params: { reason: "slow subscription" },
  }
}

test("initial_and_changed_sets_replace_members_without_turning_unknown_into_empty", async (t) => {
  const cases = [
    { name: "empty_initial", initial: [], changes: [["a"], ["b"], []] },
    { name: "running_initial", initial: ["a", "b"], changes: [["b"], []] },
  ]
  for (const scenario of cases)
    await t.test(scenario.name, async () => {
      const { connection, sync } = createHarness()
      assert.equal(sync.$conversationIDs.get(), undefined)
      const stop = sync.run()
      assert.equal(sync.$conversationIDs.get(), undefined)
      assert.deepEqual(connection.calls, [
        { method: "conversation/running/subscribe", params: {} },
      ])
      connection.reply(0, scenario.initial)
      await flush()
      assert.deepEqual(sync.$conversationIDs.get(), new Set(scenario.initial))
      for (const ids of scenario.changes) {
        connection.push(changed(ids))
        assert.deepEqual(sync.$conversationIDs.get(), new Set(ids))
      }
      stop()
      assert.equal(sync.$conversationIDs.get(), undefined)
      assert.deepEqual(connection.calls.at(-1), {
        method: "conversation/running/unsubscribe",
        params: {},
      })
    })
})

test("notifications_during_initial_subscribe_are_applied_after_initial_in_receive_order", async () => {
  const { connection, sync } = createHarness()
  const received: (string[] | undefined)[] = []
  const stopObserving = sync.$conversationIDs.listen((ids) =>
    received.push(ids ? [...ids] : undefined)
  )
  const stop = sync.run()
  connection.reply(0, ["initial"])
  connection.push(changed(["later", "newest"]))
  connection.push(changed([]))
  assert.equal(sync.$conversationIDs.get(), undefined)
  await flush()
  assert.deepEqual(received, [["initial"], ["later", "newest"], []])
  stopObserving()
  stop()
})

test("disconnect_is_unknown_and_reconnect_replaces_the_previous_set", async () => {
  const { connection, sync } = createHarness()
  const stop = sync.run()
  connection.reply(0, ["old"])
  await flush()
  connection.$state.set("reconnecting")
  assert.equal(sync.$conversationIDs.get(), undefined)
  connection.push(changed(["stale"]))
  assert.equal(sync.$conversationIDs.get(), undefined)
  assert.equal(connection.subscriptions.length, 1)
  connection.connect()
  assert.equal(connection.subscriptions.length, 2)
  connection.reply(1, [])
  await flush()
  assert.deepEqual(sync.$conversationIDs.get(), new Set())
  stop()
})

test("old_sync_results_do_not_replace_new_results", async (t) => {
  const cases = [
    "reconnect",
    "server_identity",
    "running_closed",
    "cleanup_and_run",
    "repeat_run",
  ] as const
  for (const scenario of cases)
    await t.test(scenario, async () => {
      const { connection, sync } = createHarness()
      let stop = sync.run()
      switch (scenario) {
        case "reconnect":
          connection.$state.set("reconnecting")
          connection.connect()
          break
        case "server_identity":
          connection.$state.set("reconnecting")
          connection.connect("server-2")
          break
        case "running_closed":
          connection.push(closed())
          break
        case "cleanup_and_run":
          stop()
          stop = sync.run()
          break
        case "repeat_run": {
          const oldStop = stop
          stop = sync.run()
          oldStop()
          break
        }
      }
      assert.equal(connection.subscriptions.length, 2)
      connection.reply(1, ["new"])
      await flush()
      connection.reply(0, ["old"])
      await flush()
      assert.deepEqual(sync.$conversationIDs.get(), new Set(["new"]))
      stop()
      stop()
      const callsBefore = connection.calls.length
      connection.connect()
      connection.push(changed(["stale"]))
      connection.push(closed())
      await flush()
      assert.equal(connection.calls.length, callsBefore)
      assert.equal(sync.$conversationIDs.get(), undefined)
    })
})

test("running_closed_recovers_only_the_running_subscription_with_capped_backoff", async () => {
  const harness = createHarness()
  const { connection, sync } = harness
  const stop = sync.run()
  connection.reply(0, ["a"])
  await flush()
  connection.push(closed())
  assert.equal(sync.$conversationIDs.get(), undefined)
  assert.equal(connection.subscriptions.length, 2)
  const delays: number[] = []
  for (let attempt = 0; attempt < 6; attempt++) {
    connection.subscriptions
      .at(-1)!
      .reject(
        new RpcError(
          -32000,
          "runtime unavailable",
          "conversation/running/subscribe"
        )
      )
    await flush()
    assert.equal(sync.$conversationIDs.get(), undefined)
    assert.equal(harness.getPendingTimers().length, 1)
    delays.push(harness.getPendingTimers()[0].delayMs)
    harness.fireRetryTimer()
  }
  assert.deepEqual(delays, [1000, 2000, 4000, 8000, 10000, 10000])
  connection.reply(connection.subscriptions.length - 1, ["restored"])
  await flush()
  assert.deepEqual(sync.$conversationIDs.get(), new Set(["restored"]))
  assert.ok(
    connection.calls.every(
      (call) => call.method === "conversation/running/subscribe"
    )
  )
  assert.equal(connection.$state.get(), "connected")
  stop()
})

test("retry_is_cancelled_on_disconnect_or_stop", async (t) => {
  for (const scenario of ["disconnect", "stop"] as const)
    await t.test(scenario, async () => {
      const harness = createHarness()
      const { connection, sync } = harness
      const stop = sync.run()
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
        assert.deepEqual(sync.$conversationIDs.get(), new Set())
      }
      stop()
    })
})

test("run_while_disconnected_waits_for_connected_callback", async () => {
  const { connection, sync } = createHarness()
  connection.$state.set("reconnecting")
  const stop = sync.run()
  assert.equal(connection.subscriptions.length, 0)
  connection.connect()
  assert.equal(connection.subscriptions.length, 1)
  connection.reply(0, [])
  await flush()
  assert.deepEqual(sync.$conversationIDs.get(), new Set())
  stop()
})
