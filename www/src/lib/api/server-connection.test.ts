import assert from "node:assert/strict"
import { test } from "node:test"
import { RpcError } from "../rpc-error"
import { ProtocolVersion } from "./protocol.gen"
import {
  ServerConnection,
  type ServerConnectionDeps,
  type ServerNotification,
  type GlobalServerNotification,
  type WebSocketLike,
} from "./server-connection"

const serveURL = "ws://127.0.0.1:8790/api/v1/openvaala"

// 内存 WebSocket：测试用 open、receive、drop 扮演浏览器和 serve
class FakeWebSocket implements WebSocketLike {
  onopen: WebSocketLike["onopen"] = null
  onmessage: WebSocketLike["onmessage"] = null
  onclose: WebSocketLike["onclose"] = null
  readonly sent: string[] = []
  isClosed = false
  readonly url: string
  constructor(url: string) {
    this.url = url
  }
  send(data: string) {
    this.sent.push(data)
  }
  close() {
    this.drop()
  }
  open() {
    this.onopen?.({} as Event)
  }
  receive(message: unknown) {
    this.onmessage?.({ data: JSON.stringify(message) } as MessageEvent)
  }
  // 连接断开：只触发一次 close 事件
  drop() {
    if (this.isClosed) return
    this.isClosed = true
    this.onclose?.({} as CloseEvent)
  }
  getSentFrame(index: number) {
    return JSON.parse(this.sent[index]) as {
      id: number
      method: string
      params: Record<string, unknown>
    }
  }
}

type FakeTimer = { callback: () => void; delayMs: number; isCleared: boolean }
type ProbeResult = number | "network-error" | "pending"

function createHarness(
  options: { probe?: ProbeResult; url?: string; token?: string } = {}
) {
  const sockets: FakeWebSocket[] = []
  const timers: FakeTimer[] = []
  const probes: {
    url: string
    headers: Headers
    signal: RequestInit["signal"]
    resolve?: (response: Response) => void
  }[] = []
  const deps: ServerConnectionDeps = {
    createWebSocket(url) {
      const socket = new FakeWebSocket(url)
      sockets.push(socket)
      return socket
    },
    async fetch(input, init) {
      const probe: (typeof probes)[number] = {
        url: String(input),
        headers: new Headers(init?.headers),
        signal: init?.signal,
      }
      probes.push(probe)
      if (options.probe === "pending")
        return new Promise<Response>((resolve) => {
          probe.resolve = resolve
        })
      if (options.probe === "network-error") throw new TypeError("offline")
      return new Response(null, { status: options.probe ?? 400 })
    },
    setTimeout(callback, delayMs) {
      const timer = { callback, delayMs, isCleared: false }
      timers.push(timer)
      return timer
    },
    clearTimeout(handle) {
      ;(handle as FakeTimer).isCleared = true
    },
  }
  const connection = new ServerConnection(
    { url: options.url ?? serveURL, token: options.token ?? "secret" },
    deps
  )
  return {
    connection,
    sockets,
    timers,
    probes,
    // 触发最近一个还没取消的重连定时器
    fireReconnectTimer() {
      const timer = timers.findLast((candidate) => !candidate.isCleared)
      assert.ok(timer, "no reconnect timer is pending")
      timer.isCleared = true
      timer.callback()
    },
    getPendingTimers() {
      return timers.filter((timer) => !timer.isCleared)
    },
  }
}
type Harness = ReturnType<typeof createHarness>

// 等待已排队的 Promise 回调（连接探测、请求回复）执行完
function settle() {
  return new Promise<void>((resolve) => setImmediate(resolve))
}

// 打开最近创建的 socket 并回复 initialize，返回这个已连上的 socket
async function connect(harness: Harness, serverID = "srv") {
  const socket = harness.sockets[harness.sockets.length - 1]
  socket.open()
  socket.receive({
    id: socket.getSentFrame(0).id,
    result: {
      server_info: { name: "openvaala" },
      protocol_version: ProtocolVersion,
      server_id: serverID,
    },
  })
  await settle()
  return socket
}

// 开始运行并完成第一次连接
async function start(harness: Harness) {
  const stop = harness.connection.run()
  return { stop, socket: await connect(harness) }
}

test("initialize_sends_protocol_version", async () => {
  const harness = createHarness({ token: "a b/c" })
  harness.connection.run()
  const socket = harness.sockets[0]
  assert.equal(socket.url, serveURL + "?token=a%20b%2Fc")
  assert.equal(harness.connection.$state.get(), "connecting")
  assert.throws(() => harness.connection.getServerID())

  socket.open()
  const frame = socket.getSentFrame(0)
  assert.equal(frame.id, 1)
  assert.equal(frame.method, "initialize")
  assert.equal(frame.params.protocol_version, ProtocolVersion)
  assert.equal(
    (frame.params.client_info as { name: string }).name,
    "openvaala-web"
  )
  assert.equal(harness.connection.$state.get(), "connecting")

  socket.receive({
    id: 1,
    result: { protocol_version: ProtocolVersion, server_id: "srv" },
  })
  await settle()
  assert.equal(harness.connection.$state.get(), "connected")
  assert.equal(harness.connection.$serverID.get(), "srv")
  assert.equal(harness.connection.getServerID(), "srv")
})

test("response_matched_by_id", async () => {
  const harness = createHarness()
  const { socket } = await start(harness)
  const executors = harness.connection.call("executor/list", {})
  const providers = harness.connection.call("provider/list", {})
  const [executorFrame, providerFrame] = [
    socket.getSentFrame(1),
    socket.getSentFrame(2),
  ]
  assert.deepEqual(
    [executorFrame.id, executorFrame.method, providerFrame.id],
    [2, "executor/list", 3]
  )

  socket.receive({ id: providerFrame.id, result: { providers: ["p"] } })
  socket.receive({ id: executorFrame.id, result: { executors: ["e"] } })
  assert.deepEqual(await executors, { executors: ["e"] })
  assert.deepEqual(await providers, { providers: ["p"] })
})

test("rpc_error_response_rejects_with_code_and_method", async () => {
  const harness = createHarness()
  const { socket } = await start(harness)
  const result = harness.connection.call("executor/list", {})
  socket.receive({
    id: socket.getSentFrame(1).id,
    error: { code: -32001, message: "session busy" },
  })
  await assert.rejects(result, (error: unknown) => {
    assert.ok(error instanceof RpcError)
    assert.deepEqual(
      [error.code, error.method, error.message],
      [-32001, "executor/list", "session busy"]
    )
    return true
  })
})

test("pending_requests_fail_on_close", async () => {
  const cases: {
    name: string
    close(socket: FakeWebSocket, stop: () => void): void
  }[] = [
    { name: "serve_closes_socket", close: (socket) => socket.drop() },
    { name: "connection_stopped", close: (_, stop) => stop() },
  ]
  for (const { name, close } of cases) {
    const harness = createHarness()
    const { socket, stop } = await start(harness)
    const pending = harness.connection.call("executor/list", {})
    close(socket, stop)
    await assert.rejects(
      pending,
      (error: unknown) =>
        error instanceof RpcError &&
        error.code === -1 &&
        error.method === "executor/list",
      name
    )
  }
})

test("call_fails_immediately_when_not_connected", async () => {
  const harness = createHarness()
  harness.connection.run()
  const cases: { name: string; prepare(): void }[] = [
    { name: "connecting", prepare: () => {} },
    {
      name: "reconnecting",
      prepare: () => harness.sockets[0].drop(),
    },
  ]
  for (const { name, prepare } of cases) {
    prepare()
    const sentBefore = harness.sockets.reduce(
      (total, socket) => total + socket.sent.length,
      0
    )
    await assert.rejects(
      harness.connection.call("executor/list", {}),
      (error: unknown) => error instanceof RpcError && error.code === -1,
      name
    )
    assert.equal(
      harness.sockets.reduce((total, socket) => total + socket.sent.length, 0),
      sentBefore,
      name
    )
  }
})

test("reconnect_backoff_delays", async () => {
  const harness = createHarness()
  harness.connection.run()
  const delays: number[] = []
  for (let attempt = 0; attempt < 6; attempt++) {
    harness.sockets[harness.sockets.length - 1].drop()
    await settle()
    assert.equal(harness.connection.$state.get(), "reconnecting")
    delays.push(harness.getPendingTimers()[0].delayMs)
    harness.fireReconnectTimer()
  }
  assert.deepEqual(delays, [1000, 2000, 4000, 8000, 10000, 10000])
  assert.equal(harness.sockets.length, 7)
})

test("reconnect_resets_backoff_and_notifies_connected", async () => {
  const harness = createHarness()
  let connectedCount = 0
  harness.connection.onConnected(() => connectedCount++)
  harness.connection.run()
  const reconnectDelays: number[] = []
  for (let attempt = 0; attempt < 3; attempt++) {
    harness.sockets[harness.sockets.length - 1].drop()
    await settle()
    reconnectDelays.push(harness.getPendingTimers()[0].delayMs)
    harness.fireReconnectTimer()
  }
  assert.deepEqual(reconnectDelays, [1000, 2000, 4000])

  const socket = await connect(harness)
  assert.equal(harness.connection.$state.get(), "connected")
  assert.equal(connectedCount, 1)

  socket.drop()
  await settle()
  assert.equal(harness.connection.$state.get(), "reconnecting")
  assert.equal(harness.timers[harness.timers.length - 1].delayMs, 1000)
  harness.fireReconnectTimer()
  await connect(harness)
  assert.equal(connectedCount, 2)
  assert.equal(harness.connection.$serverID.get(), "srv")
})

test("unauthorized_stops_reconnecting", async () => {
  const cases: {
    name: string
    probe: ProbeResult
    url: string
    wantState: string
    wantProbeURL: string
    wantTimers: number
  }[] = [
    {
      name: "401_marks_token_invalid",
      probe: 401,
      url: serveURL,
      wantState: "unauthorized",
      wantProbeURL: "http://127.0.0.1:8790/api/v1/openvaala",
      wantTimers: 0,
    },
    {
      name: "wss_probes_https",
      probe: 401,
      url: "wss://example.com/api/v1/openvaala",
      wantState: "unauthorized",
      wantProbeURL: "https://example.com/api/v1/openvaala",
      wantTimers: 0,
    },
    {
      name: "other_status_keeps_reconnecting",
      probe: 502,
      url: serveURL,
      wantState: "reconnecting",
      wantProbeURL: "http://127.0.0.1:8790/api/v1/openvaala",
      wantTimers: 1,
    },
    {
      name: "network_error_keeps_reconnecting",
      probe: "network-error",
      url: serveURL,
      wantState: "reconnecting",
      wantProbeURL: "http://127.0.0.1:8790/api/v1/openvaala",
      wantTimers: 1,
    },
  ]
  for (const {
    name,
    probe,
    url,
    wantState,
    wantProbeURL,
    wantTimers,
  } of cases) {
    const harness = createHarness({ probe, url, token: "bad token" })
    harness.connection.run()
    harness.sockets[0].drop()
    await settle()
    assert.equal(harness.connection.$state.get(), wantState, name)
    assert.equal(harness.probes.length, 1, name)
    assert.equal(harness.probes[0].url, wantProbeURL, name)
    assert.equal(
      harness.probes[0].headers.get("Authorization"),
      "Bearer bad token",
      name
    )
    assert.equal(harness.getPendingTimers().length, wantTimers, name)
    if (wantTimers > 0) harness.fireReconnectTimer()
    await settle()
    assert.equal(harness.sockets.length, 1 + wantTimers, name)
  }
})

test("incompatible_on_initialize_error", async () => {
  const harness = createHarness()
  harness.connection.run()
  const socket = harness.sockets[0]
  socket.open()
  socket.receive({
    id: 1,
    error: { code: -32600, message: "protocol version mismatch" },
  })
  await settle()
  assert.equal(harness.connection.$state.get(), "incompatible")
  assert.equal(socket.isClosed, true)
  assert.equal(harness.getPendingTimers().length, 0)
  assert.equal(harness.probes.length, 0)
  assert.equal(harness.sockets.length, 1)
})

test("stop_cancels_reconnect", async () => {
  const harness = createHarness()
  const stop = harness.connection.run()
  harness.sockets[0].drop()
  await settle()
  assert.equal(harness.getPendingTimers().length, 1)
  stop()
  assert.equal(harness.getPendingTimers().length, 0)
  assert.equal(harness.sockets.length, 1)
})

test("notifications_routed_to_conversation_listeners", async () => {
  const harness = createHarness()
  const { socket } = await start(harness)
  const received: Record<string, ServerNotification[]> = { c1: [], c2: [] }
  const stopC1 = harness.connection.listen("c1", (notification) =>
    received.c1.push(notification)
  )
  harness.connection.listen("c2", (notification) =>
    received.c2.push(notification)
  )
  const notify = (conversationID: string, turnID: string) =>
    socket.receive({
      method: "turn/started",
      params: {
        conversation_id: conversationID,
        session_id: "s",
        subscription: "session",
        turn_id: turnID,
      },
    })
  notify("c1", "t1")
  notify("c2", "t2")
  notify("c3", "t3")
  stopC1()
  notify("c1", "t4")
  assert.deepEqual(
    Object.entries(received).map(([id, list]) => [
      id,
      list.map((item) => (item.params as { turn_id: string }).turn_id),
    ]),
    [
      ["c1", ["t1"]],
      ["c2", ["t2"]],
    ]
  )
  assert.equal(received.c1[0].method, "turn/started")
})

test("rpc_log_keeps_latest_2000", async () => {
  const harness = createHarness()
  const { socket } = await start(harness)
  harness.connection.clearRpcLog()
  // 1002 次请求各记发出和收到两帧，再加一条通知，共 2005 帧
  for (let call = 0; call < 1002; call++) {
    const result = harness.connection.call("executor/list", {})
    socket.receive({
      id: socket.getSentFrame(socket.sent.length - 1).id,
      result: { executors: [] },
    })
    await result
  }
  socket.receive({ method: "turn/started", params: { conversation_id: "c" } })

  const log = harness.connection.$rpcLog.get()
  assert.equal(log.length, 2000)
  assert.deepEqual(
    [log[0].direction, log[0].id, log[0].method],
    ["received", 4, "executor/list"]
  )
  const last = log[log.length - 1]
  assert.deepEqual(
    [last.direction, last.method, last.id],
    ["received", "turn/started", undefined]
  )
  assert.ok(last.size > 0 && typeof last.time === "number")
  assert.deepEqual(last.payload, {
    method: "turn/started",
    params: { conversation_id: "c" },
  })
  const sent = log.find((frame) => frame.direction === "sent")
  assert.deepEqual([sent?.method, sent?.id], ["executor/list", 5])

  harness.connection.setRpcLogPaused(true)
  const paused = harness.connection.call("executor/list", {})
  socket.receive({
    id: socket.getSentFrame(socket.sent.length - 1).id,
    result: { executors: [] },
  })
  await paused
  assert.equal(harness.connection.$rpcLog.get().length, 2000)
  assert.equal(harness.connection.$rpcLog.get(), log)

  harness.connection.clearRpcLog()
  assert.equal(harness.connection.$rpcLog.get().length, 0)
})

test("manual_reconnect_joins_or_starts_one_attempt", async (t) => {
  const cases = [
    {
      name: "connected_is_unchanged",
      phase: "connected",
      wantSockets: 1,
      wantPending: false,
    },
    {
      name: "joins_automatic_attempt",
      phase: "attempt",
      wantSockets: 1,
      wantPending: true,
    },
    {
      name: "skips_backoff",
      phase: "backoff",
      wantSockets: 2,
      wantPending: true,
    },
    {
      name: "replaces_auth_probe",
      phase: "probe",
      wantSockets: 2,
      wantPending: true,
    },
  ] as const
  for (const scenario of cases)
    await t.test(scenario.name, async () => {
      const harness = createHarness({
        probe: scenario.phase === "probe" ? "pending" : 400,
      })
      const stop = harness.connection.run()
      if (scenario.phase === "connected") await connect(harness)
      if (scenario.phase === "backoff" || scenario.phase === "probe") {
        harness.sockets[0].drop()
        await settle()
      }
      const oldTimers = harness.getPendingTimers()
      harness.connection.reconnect()
      harness.connection.reconnect()
      assert.equal(harness.sockets.length, scenario.wantSockets)
      assert.equal(
        harness.connection.$isManualReconnectPending.get(),
        scenario.wantPending
      )
      if (scenario.phase === "backoff" || scenario.phase === "probe") {
        assert.ok(oldTimers.every((timer) => timer.isCleared))
        for (const timer of oldTimers) timer.callback()
        await settle()
        assert.equal(harness.sockets.length, scenario.wantSockets)
      }
      if (scenario.phase === "probe")
        assert.equal(harness.probes[0].signal?.aborted, true)
      stop()
      await settle()
      assert.equal(harness.getPendingTimers().length, 0)
    })
})

test("manual_attempt_finishes_on_success_failure_timeout_or_stop", async (t) => {
  const cases = [
    "connected",
    "closed",
    "socket_timeout",
    "initialize_timeout",
    "incompatible",
    "stopped",
  ] as const
  for (const outcome of cases)
    await t.test(outcome, async () => {
      const harness = createHarness()
      const stop = harness.connection.run()
      harness.connection.reconnect()
      assert.equal(harness.connection.$isManualReconnectPending.get(), true)
      const socket = harness.sockets[0]
      switch (outcome) {
        case "connected":
          await connect(harness)
          break
        case "closed":
          socket.drop()
          break
        case "initialize_timeout":
          socket.open()
          harness.fireReconnectTimer()
          break
        case "socket_timeout":
          harness.fireReconnectTimer()
          break
        case "incompatible":
          socket.open()
          socket.receive({
            id: 1,
            error: { code: -32600, message: "protocol version mismatch" },
          })
          break
        case "stopped":
          stop()
          break
      }
      await settle()
      assert.equal(harness.connection.$isManualReconnectPending.get(), false)
      if (outcome === "closed" || outcome.endsWith("timeout")) {
        assert.equal(socket.isClosed, true)
        assert.equal(harness.connection.$state.get(), "reconnecting")
        assert.deepEqual(
          harness.getPendingTimers().map((timer) => timer.delayMs),
          [1000]
        )
      } else assert.equal(harness.getPendingTimers().length, 0)
      stop()
    })
})

test("auth_probe_is_bounded_and_cannot_replace_new_connection", async (t) => {
  const cases = ["timeout", "new_connection", "stop"] as const
  for (const outcome of cases)
    await t.test(outcome, async () => {
      const harness = createHarness({ probe: "pending" })
      const stop = harness.connection.run()
      harness.sockets[0].drop()
      assert.deepEqual(
        harness.getPendingTimers().map((timer) => timer.delayMs),
        [10_000]
      )
      const probe = harness.probes[0]
      if (outcome === "timeout") harness.fireReconnectTimer()
      if (outcome === "new_connection") {
        harness.connection.reconnect()
        await connect(harness)
      }
      if (outcome === "stop") stop()
      await settle()
      assert.equal(probe.signal?.aborted, true)
      probe.resolve!(new Response(null, { status: 401 }))
      await settle()
      if (outcome === "timeout") {
        assert.equal(harness.connection.$state.get(), "reconnecting")
        assert.deepEqual(
          harness.getPendingTimers().map((timer) => timer.delayMs),
          [1000]
        )
      } else {
        assert.notEqual(harness.connection.$state.get(), "unauthorized")
        assert.equal(harness.getPendingTimers().length, 0)
        if (outcome === "new_connection")
          assert.equal(harness.connection.$state.get(), "connected")
      }
      stop()
    })
})

test("write_request_is_not_replayed_after_manual_reconnect", async () => {
  const harness = createHarness()
  const { stop, socket } = await start(harness)
  const pending = harness.connection.call("conversation/submitInput", {
    conversation_id: "c1",
    content: [],
  })
  socket.drop()
  await assert.rejects(
    pending,
    (error: unknown) => error instanceof RpcError && error.code === -1
  )
  harness.connection.reconnect()
  const reconnected = await connect(harness)
  assert.deepEqual(
    reconnected.sent.map((frame) => JSON.parse(frame).method),
    ["initialize"]
  )
  assert.equal(harness.connection.$serverID.get(), "srv")
  stop()
})

test("global_notifications_do_not_enter_conversation_listener", async () => {
  const harness = createHarness()
  const { stop, socket } = await start(harness)
  const globalNotifications: GlobalServerNotification[] = []
  const conversationNotifications: ServerNotification[] = []
  const stopListening = harness.connection.listenServer((notification) =>
    globalNotifications.push(notification)
  )
  harness.connection.listen("c1", (notification) =>
    conversationNotifications.push(notification)
  )
  const changed = {
    method: "conversation/runningChanged",
    params: { conversation_ids: ["c1"] },
  }
  const closed = {
    method: "conversation/runningClosed",
    params: { reason: "slow subscription" },
  }
  socket.receive(changed)
  socket.receive({ method: "turn/started", params: { conversation_id: "c1" } })
  socket.receive(closed)
  stopListening()
  socket.receive(changed)
  assert.deepEqual(globalNotifications, [changed, closed])
  assert.equal(conversationNotifications.length, 1)
  assert.equal(conversationNotifications[0].method, "turn/started")
  stop()
})

test("expired_attempt_callbacks_cannot_close_or_change_the_new_connection", async (t) => {
  for (const scenario of ["completed_attempt", "replaced_attempt"] as const)
    await t.test(scenario, async () => {
      const harness = createHarness()
      const stop = harness.connection.run()
      const socket = harness.sockets[0]
      const attemptTimer = harness.getPendingTimers()[0]
      const oldOpen = socket.onopen
      const oldClose = socket.onclose
      const oldMessage = socket.onmessage
      if (scenario === "replaced_attempt") {
        socket.drop()
        harness.connection.reconnect()
      }
      const connected = await connect(harness)
      attemptTimer.callback()
      if (scenario === "replaced_attempt") {
        oldOpen?.({} as Event)
        oldClose?.({} as CloseEvent)
        oldMessage?.({
          data: JSON.stringify({
            method: "conversation/runningClosed",
            params: { reason: "old" },
          }),
        } as MessageEvent)
      }
      await settle()
      assert.equal(harness.connection.$state.get(), "connected")
      assert.equal(connected.isClosed, false)
      assert.equal(harness.connection.$isManualReconnectPending.get(), false)
      assert.equal(harness.getPendingTimers().length, 0)
      assert.equal(
        harness.sockets.length,
        scenario === "completed_attempt" ? 1 : 2
      )
      stop()
    })
})
