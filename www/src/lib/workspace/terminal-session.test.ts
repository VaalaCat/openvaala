import assert from "node:assert/strict"
import { before, test } from "node:test"
import { atom } from "nanostores"
import { i18n, initializeI18n } from "../../i18n"
import {
  ErrorCodeRuntime,
  ErrorCodeTerminalNotFound,
  type ClientNotificationParams,
  type MethodParams,
  type TerminalAttachResult,
  type TerminalOpenResult,
  type TerminalOutputParams,
} from "../api/protocol.gen"
import type {
  ConnectionState,
  GlobalServerNotification,
  ServerConnection,
} from "../api/server-connection"
import { RpcError, rpcConnectionLostCode } from "../rpc-error"
import { $appearance, defaultAppearance } from "../appearance/preferences"
import { TerminalSession } from "./terminal-session"

// 内存连接：terminal/open、terminal/attach 默认立即成功，测试用 holdOpen 手动决定 terminal/open 什么时候回复；
// 和真实连接一样，没连上时请求立即以「连接已断开」失败
class FakeConnection {
  readonly $state = atom<ConnectionState>("connected")
  readonly calls: { method: keyof MethodParams; params: unknown }[] = []
  readonly notifications: {
    method: keyof ClientNotificationParams
    params: unknown
  }[] = []
  private readonly handlers = new Set<
    (notification: GlobalServerNotification) => void
  >()
  private nextTerminalNumber = 1
  private pendingOpens: {
    resolve(result: TerminalOpenResult): void
    reject(error: Error): void
  }[] = []
  holdOpen = false
  openError: Error | undefined
  attachResult: TerminalAttachResult = {
    executor_id: "local",
    directory: "/work/default",
    rows: 24,
    cols: 80,
  }
  attachError: Error | undefined

  call(method: keyof MethodParams, params: unknown): Promise<unknown> {
    this.calls.push({ method, params })
    if (this.$state.get() !== "connected")
      return Promise.reject(
        new RpcError(rpcConnectionLostCode, "connection lost", method)
      )
    if (method === "terminal/attach")
      return this.attachError
        ? Promise.reject(this.attachError)
        : Promise.resolve(this.attachResult)
    if (method !== "terminal/open") return Promise.resolve({})
    const { executor_id, directory } = params as MethodParams["terminal/open"]
    if (this.openError) return Promise.reject(this.openError)
    const result = {
      terminal_id: `t${this.nextTerminalNumber++}`,
      executor_id: executor_id || "local",
      directory: directory || "/work/default",
    }
    if (!this.holdOpen) return Promise.resolve(result)
    return new Promise((resolve, reject) =>
      this.pendingOpens.push({ resolve: () => resolve(result), reject })
    )
  }

  notify(method: keyof ClientNotificationParams, params: unknown): void {
    this.notifications.push({ method, params })
  }

  listenServer(
    handler: (notification: GlobalServerNotification) => void
  ): () => void {
    this.handlers.add(handler)
    return () => this.handlers.delete(handler)
  }

  getCalls(method: keyof MethodParams) {
    return this.calls.filter((call) => call.method === method)
  }

  // 已发出的 terminal/resize，按发出的顺序
  getResizes(): { rows: number; cols: number }[] {
    return this.notifications
      .filter((each) => each.method === "terminal/resize")
      .map((each) => {
        const { rows, cols } = each.params as { rows: number; cols: number }
        return { rows, cols }
      })
  }

  // 回复最早一个等待中的 terminal/open
  resolveOpen(): void {
    this.pendingOpens.shift()!.resolve({} as TerminalOpenResult)
  }

  // 像真实连接那样，断线时让最早一个等待中的 terminal/open 以「连接已断开」失败
  rejectOpenWithConnectionLost(): void {
    this.pendingOpens
      .shift()!
      .reject(
        new RpcError(rpcConnectionLostCode, "connection lost", "terminal/open")
      )
  }

  get listenerCount(): number {
    return this.handlers.size
  }

  emit(notification: unknown): void {
    for (const handler of [...this.handlers])
      handler(notification as GlobalServerNotification)
  }
}

function createSession(executorID = "ssh-a", directory = "/srv/app") {
  const connection = new FakeConnection()
  const session = new TerminalSession(
    connection as unknown as ServerConnection,
    "c1",
    executorID,
    directory
  )
  return { session, connection }
}

// 接回 serve 上已有的终端 t9，等 terminal/attach 的回复处理完
async function createResumedSession() {
  const created = createSession()
  created.session.resume("t9")
  await settle()
  return created
}

// serve 推来终端 t9 的一段输出
function emitOutput(
  connection: FakeConnection,
  output: Partial<TerminalOutputParams> & { text: string }
): void {
  connection.emit({
    method: "terminal/output",
    params: {
      terminal_id: "t9",
      data: encode(output.text),
      next_cursor: output.next_cursor ?? 0,
      truncated: output.truncated ?? false,
      has_more: output.has_more ?? false,
    },
  })
}

// 等 redraw 的第二次改尺寸发出
function waitForRedraw() {
  return new Promise<void>((resolve) => setTimeout(resolve, 150))
}

// 让 terminal/open 的回复处理完
function settle() {
  return new Promise<void>((resolve) => setImmediate(resolve))
}

// 等 xterm 把已经写入的内容处理完，再读第一行屏幕文字
async function readFirstLine(session: TerminalSession): Promise<string> {
  await new Promise<void>((resolve) => session.terminal.write("", resolve))
  return (
    session.terminal.buffer.active.getLine(0)?.translateToString(true) ?? ""
  )
}

function encode(text: string): string {
  return Buffer.from(text).toString("base64")
}

before(async () => {
  await initializeI18n()
  await i18n.changeLanguage("zh-CN")
})

test("start opens the terminal and moves to running", async () => {
  const cases = [
    {
      name: "with a location",
      executorID: "ssh-a",
      directory: "/srv/app",
      title: "ssh-a · app",
    },
    {
      name: "without a location the serve decides",
      executorID: "",
      directory: "",
      title: "local · default",
    },
  ]
  for (const each of cases) {
    const { session, connection } = createSession(
      each.executorID,
      each.directory
    )
    assert.equal(session.getSnapshot().phase, "opening", each.name)
    session.start()
    assert.deepEqual(
      connection.getCalls("terminal/open"),
      [
        {
          method: "terminal/open",
          params: {
            executor_id: each.executorID,
            directory: each.directory,
            rows: 24,
            cols: 80,
            conversation_id: "c1",
          },
        },
      ],
      each.name
    )
    await settle()
    const snapshot = session.getSnapshot()
    assert.equal(snapshot.phase, "running", each.name)
    assert.equal(snapshot.title, each.title, each.name)
    await session.close()
  }
})

test("open failure keeps the tab exited with the reason", async () => {
  const cases = [
    {
      name: "serve error text is shown as is",
      error: new RpcError(
        -32000,
        "executor virtual does not support terminals",
        "terminal/open"
      ),
      reason: "executor virtual does not support terminals",
    },
    {
      name: "connection lost",
      error: new RpcError(
        rpcConnectionLostCode,
        "connection lost",
        "terminal/open"
      ),
      reason: "连接已断开",
    },
  ]
  for (const each of cases) {
    const { session, connection } = createSession()
    connection.openError = each.error
    session.start()
    await settle()
    const snapshot = session.getSnapshot()
    assert.equal(snapshot.phase, "exited", each.name)
    assert.equal(snapshot.exitReason, each.reason, each.name)
    assert.equal(connection.listenerCount, 0, each.name)
    await session.close()
  }
})

test("output is decoded into the screen and other terminals are ignored", async () => {
  const { session, connection } = createSession()
  session.start()
  await settle()
  connection.emit({
    method: "terminal/output",
    params: { terminal_id: "other", data: encode("ignored") },
  })
  connection.emit({
    method: "terminal/output",
    params: { terminal_id: "t1", data: encode("你好 hello") },
  })
  assert.equal(await readFirstLine(session), "你好 hello")
  await session.close()
})

test("appearance updates every open terminal without restarting or losing output", async () => {
  const sessions = [createSession(), createSession()]
  try {
    for (const { session, connection } of sessions) {
      session.start()
      await settle()
      connection.emit({
        method: "terminal/output",
        params: { terminal_id: "t1", data: encode("kept output") },
      })
    }
    for (const terminal of [
      { fontSize: 18, lineHeight: 1.4, letterSpacing: 1, padding: 16 },
      defaultAppearance.terminal,
    ]) {
      $appearance.set({ ...$appearance.get(), terminal })
      for (const { session, connection } of sessions) {
        assert.equal(session.terminal.options.fontSize, terminal.fontSize)
        assert.equal(session.terminal.options.lineHeight, terminal.lineHeight)
        assert.equal(
          session.terminal.options.letterSpacing,
          terminal.letterSpacing
        )
        assert.equal(await readFirstLine(session), "kept output")
        assert.equal(connection.getCalls("terminal/open").length, 1)
        assert.equal(connection.getCalls("terminal/close").length, 0)
        assert.equal(session.getSnapshot().phase, "running")
      }
    }
  } finally {
    for (const { session } of sessions) await session.close()
    $appearance.set({ ...defaultAppearance })
  }
})

test("exited notification keeps the session with the reason", async () => {
  const cases = [
    {
      name: "exit code",
      params: { terminal_id: "t1", exit_code: 3 },
      reason: "终端已退出，退出码 3",
    },
    {
      name: "serve could not get the exit code",
      params: {
        terminal_id: "t1",
        exit_code: -1,
        error: "ssh connection lost",
      },
      reason: "ssh connection lost",
    },
  ]
  for (const each of cases) {
    const { session, connection } = createSession()
    session.start()
    await settle()
    connection.emit({ method: "terminal/exited", params: each.params })
    const snapshot = session.getSnapshot()
    assert.equal(snapshot.phase, "exited", each.name)
    assert.equal(snapshot.exitReason, each.reason, each.name)
    // 已结束的终端不再接收通知，也不用再通知 serve 关闭。
    assert.equal(connection.listenerCount, 0, each.name)
    await session.close()
    assert.equal(connection.getCalls("terminal/close").length, 0, each.name)
  }
})

test("disconnect enters reconnecting", async () => {
  const cases: { name: string; holdOpen: boolean; phase: string }[] = [
    { name: "while running", holdOpen: false, phase: "reconnecting" },
    // 打开还没回复时，阶段由 terminal/open 的失败收尾，不在断线时改。
    { name: "while opening", holdOpen: true, phase: "opening" },
  ]
  for (const each of cases) {
    const { session, connection } = createSession()
    connection.holdOpen = each.holdOpen
    session.start()
    await settle()
    connection.$state.set("reconnecting")
    const snapshot = session.getSnapshot()
    assert.equal(snapshot.phase, each.phase, each.name)
    assert.equal(
      snapshot.isWaitingForConnection,
      each.phase === "reconnecting",
      each.name
    )
    // 重连中不发输入。
    session.terminal.input("x", true)
    assert.equal(connection.notifications.length, 0, each.name)
    if (each.holdOpen) {
      connection.rejectOpenWithConnectionLost()
      await settle()
      assert.equal(session.getSnapshot().phase, "exited", each.name)
      assert.equal(session.getSnapshot().exitReason, "连接已断开", each.name)
    }
    await session.close()
  }
})

test("a reconnecting session waits for the connection only while it is down", async () => {
  const { session, connection } = createSession()
  session.resume("t9")
  // 刷新后恢复标签页：连接是好的，只等接入的回复。
  assert.deepEqual(
    {
      phase: session.getSnapshot().phase,
      isWaiting: session.getSnapshot().isWaitingForConnection,
    },
    { phase: "reconnecting", isWaiting: false }
  )
  await settle()
  assert.equal(session.getSnapshot().phase, "running")
  // 断线后进入等待，连上后重新接入。
  connection.$state.set("reconnecting")
  assert.equal(session.getSnapshot().isWaitingForConnection, true)
  connection.$state.set("connected")
  assert.equal(session.getSnapshot().isWaitingForConnection, false)
  assert.equal(connection.getCalls("terminal/attach").length, 2)
  await settle()
  assert.equal(session.getSnapshot().phase, "running")
  await session.close()
})

test("resume attaches from the start and resizes xterm to the PTY size", async () => {
  const created = createSession()
  created.connection.attachResult = {
    executor_id: "ssh-a",
    directory: "/srv/app",
    rows: 30,
    cols: 100,
  }
  created.session.resume("t9")
  await settle()
  assert.deepEqual(
    created.connection.getCalls("terminal/attach").map((call) => call.params),
    [{ terminal_id: "t9", cursor: 0 }]
  )
  assert.deepEqual(
    [created.session.terminal.rows, created.session.terminal.cols],
    [30, 100]
  )
  assert.equal(created.session.getSnapshot().title, "ssh-a · app")
  // 回放结束后尺寸和 PTY 一样，不用发 resize。
  emitOutput(created.connection, { text: "x", next_cursor: 1 })
  await readFirstLine(created.session)
  assert.deepEqual(created.connection.getResizes(), [])
  await created.session.close()
})

test("replay drops xterm auto replies", async () => {
  const { session, connection } = await createResumedSession()
  const autoReplies: string[] = []
  session.terminal.onData((data) => autoReplies.push(data))
  // 回放的历史输出里有光标位置查询，xterm 会自动回答，回答不能变成 shell 的输入。
  emitOutput(connection, { text: "a\x1b[6n", next_cursor: 5, has_more: true })
  await readFirstLine(session)
  assert.deepEqual(autoReplies, ["\x1b[1;2R"])
  assert.equal(connection.notifications.length, 0)
  // has_more 为 false 的最后一批写完后恢复发送。
  emitOutput(connection, { text: "b", next_cursor: 6, has_more: false })
  await readFirstLine(session)
  session.terminal.input("ls\r", true)
  assert.deepEqual(
    connection.notifications.map((each) => each.params),
    [{ terminal_id: "t9", data: encode("ls\r") }]
  )
  await session.close()
})

// 把容器的 fit 换成内存实现：容器能排下 30 行 100 列，fit 把 xterm 改成这个尺寸
function stubContainerFit(session: TerminalSession) {
  const internals = session as unknown as {
    container: HTMLElement
    fitAddon: {
      proposeDimensions(): { rows: number; cols: number }
      fit(): void
    }
    fit(container: HTMLElement): void
  }
  internals.container = {
    isConnected: true,
    clientWidth: 800,
    clientHeight: 600,
  } as HTMLElement
  internals.fitAddon.proposeDimensions = () => ({ rows: 30, cols: 100 })
  internals.fitAddon.fit = () => session.terminal.resize(100, 30)
  // 界面上容器变大小（ResizeObserver、字体加载完成、外观变化）都会走到这里。
  return () => internals.fit(internals.container)
}

test("replay ignores container resize", async () => {
  const cases = [
    {
      name: "the replay ends",
      end: (session: TerminalSession, connection: FakeConnection) => {
        emitOutput(connection, { text: "x", next_cursor: 1 })
        return readFirstLine(session)
      },
      // 回放结束时按容器排好并补发尺寸。
      sizeAfterEnd: [30, 100],
      wantResizes: [{ rows: 30, cols: 100 }],
    },
    {
      name: "the terminal exits during the replay",
      end: (session: TerminalSession, connection: FakeConnection) => {
        connection.emit({
          method: "terminal/exited",
          params: { terminal_id: "t9", exit_code: 0 },
        })
        return readFirstLine(session)
      },
      // 终端结束后不再等回放，之后容器变化照常处理；已结束的终端不用通知 serve。
      sizeAfterEnd: [24, 80],
      wantResizes: [],
    },
  ]
  for (const each of cases) {
    const { session, connection } = await createResumedSession()
    const fitToContainer = stubContainerFit(session)
    // 1. 回放期间容器变化：xterm 保持 PTY 当前尺寸，后面的字节按它解析。
    fitToContainer()
    assert.deepEqual(
      [session.terminal.rows, session.terminal.cols],
      [24, 80],
      each.name
    )
    // 2. 回放结束或终端结束后，容器变化才生效。
    await each.end(session, connection)
    assert.deepEqual(
      [session.terminal.rows, session.terminal.cols],
      each.sizeAfterEnd,
      each.name
    )
    fitToContainer()
    assert.deepEqual(
      [session.terminal.rows, session.terminal.cols],
      [30, 100],
      each.name
    )
    assert.deepEqual(connection.getResizes(), each.wantResizes, each.name)
    await session.close()
  }
})

test("reconnect during replay resumes from received cursor", async () => {
  const { session, connection } = await createResumedSession()
  emitOutput(connection, { text: "abc", next_cursor: 3, has_more: true })
  await readFirstLine(session)
  connection.$state.set("reconnecting")
  connection.$state.set("connected")
  await settle()
  assert.deepEqual(
    connection.getCalls("terminal/attach").map((call) => call.params),
    [
      { terminal_id: "t9", cursor: 0 },
      { terminal_id: "t9", cursor: 3 },
    ]
  )
  // 续读的输出接在已写入的后面，第二次回放结束后恢复发送。
  emitOutput(connection, { text: "def", next_cursor: 6 })
  assert.equal(await readFirstLine(session), "abcdef")
  session.terminal.input("x", true)
  assert.equal(connection.notifications.length, 1)
  await session.close()
})

test("truncated output resets xterm once", async () => {
  const cases = [
    {
      name: "after output was already received",
      received: true,
      screen: "tail",
    },
    {
      name: "when nothing was received yet",
      received: false,
      screen: "tail",
    },
  ]
  for (const each of cases) {
    const { session, connection } = await createResumedSession()
    if (each.received) {
      emitOutput(connection, { text: "head", next_cursor: 4, has_more: true })
      await readFirstLine(session)
    }
    emitOutput(connection, {
      text: "tail",
      next_cursor: 1000,
      truncated: true,
      has_more: false,
    })
    // 旧内容接不上就清掉，从最早保留处重放；没有收到过输出时本来就是空的。
    assert.equal(await readFirstLine(session), each.screen, each.name)
    // 回放结束且尺寸没变：先少一行，隔 100ms 再改回。
    assert.deepEqual(
      connection.getResizes(),
      [{ rows: 23, cols: 80 }],
      each.name
    )
    await waitForRedraw()
    assert.deepEqual(
      connection.getResizes(),
      [
        { rows: 23, cols: 80 },
        { rows: 24, cols: 80 },
      ],
      each.name
    )
    await session.close()
  }
})

test("attach failures decide whether the terminal ended", async () => {
  const cases: {
    name: string
    error: Error
    phase: string
    reason: string
    listening: boolean
  }[] = [
    {
      name: "unknown terminal",
      error: new RpcError(
        ErrorCodeTerminalNotFound,
        "terminal not found",
        "terminal/attach"
      ),
      phase: "exited",
      reason: "终端已结束",
      listening: false,
    },
    {
      name: "other serve error",
      error: new RpcError(
        ErrorCodeRuntime,
        "cursor 9 is after the latest output 3",
        "terminal/attach"
      ),
      phase: "exited",
      reason: "cursor 9 is after the latest output 3",
      listening: false,
    },
    {
      name: "connection lost waits for the next connection",
      error: new RpcError(
        rpcConnectionLostCode,
        "connection lost",
        "terminal/attach"
      ),
      phase: "reconnecting",
      reason: "",
      listening: true,
    },
  ]
  for (const each of cases) {
    const { session, connection } = createSession()
    connection.attachError = each.error
    session.resume("t9")
    await settle()
    const snapshot = session.getSnapshot()
    assert.equal(snapshot.phase, each.phase, each.name)
    assert.equal(snapshot.exitReason, each.reason, each.name)
    assert.equal(connection.listenerCount > 0, each.listening, each.name)
    await session.close()
  }
})

test("an ended terminal can be restarted after attach found it gone", async () => {
  const { session, connection } = createSession()
  connection.attachError = new RpcError(
    ErrorCodeTerminalNotFound,
    "terminal not found",
    "terminal/attach"
  )
  session.resume("t9")
  await settle()
  assert.equal(session.getSnapshot().phase, "exited")
  await session.restart()
  assert.equal(connection.getCalls("terminal/open").length, 1)
  assert.equal(session.getSnapshot().phase, "running")
  assert.equal(session.getTerminalID(), "t1")
  await session.close()
})

test("reset requests a redraw", async () => {
  const cases = [
    { name: "running", resume: true },
    { name: "still reconnecting", resume: false },
  ]
  for (const each of cases) {
    const { session, connection } = createSession()
    if (each.resume) {
      session.resume("t9")
      await settle()
    } else {
      connection.$state.set("reconnecting")
      session.resume("t9")
    }
    await session.reset()
    assert.deepEqual(
      connection.getCalls("terminal/reset").map((call) => call.params),
      each.resume ? [{ terminal_id: "t9" }] : [],
      each.name
    )
    assert.deepEqual(
      connection.getResizes(),
      each.resume ? [{ rows: 23, cols: 80 }] : [],
      each.name
    )
    if (each.resume) {
      await waitForRedraw()
      assert.deepEqual(connection.getResizes().at(-1), { rows: 24, cols: 80 })
    }
    await session.close()
  }
})

test("restart opens a new terminal in the same place", async () => {
  const { session, connection } = createSession()
  session.start()
  await settle()
  connection.emit({
    method: "terminal/output",
    params: { terminal_id: "t1", data: encode("old screen") },
  })
  assert.equal(await readFirstLine(session), "old screen")
  // 还在运行的终端不能重新启动，连点也只打开一次。
  session.restart()
  assert.equal(connection.getCalls("terminal/open").length, 1)
  connection.emit({
    method: "terminal/exited",
    params: { terminal_id: "t1", exit_code: 0 },
  })
  session.restart()
  session.restart()
  assert.equal(session.getSnapshot().phase, "opening")
  await settle()
  const opens = connection.getCalls("terminal/open")
  assert.equal(opens.length, 2)
  assert.deepEqual(opens[1].params, opens[0].params)
  const snapshot = session.getSnapshot()
  assert.equal(snapshot.phase, "running")
  assert.equal(snapshot.exitReason, "")
  assert.equal(await readFirstLine(session), "")
  // 旧终端的输出不再写进屏幕，新终端的输出写进去。
  connection.emit({
    method: "terminal/output",
    params: { terminal_id: "t1", data: encode("old") },
  })
  connection.emit({
    method: "terminal/output",
    params: { terminal_id: "t2", data: encode("new") },
  })
  assert.equal(await readFirstLine(session), "new")
  await session.close()
})

test("restored session can be restarted without having started", () => {
  const { session, connection } = createSession()
  session.markExited("终端已结束")
  assert.equal(session.getSnapshot().phase, "exited")
  assert.equal(connection.calls.length, 0)
  session.restart()
  assert.equal(connection.getCalls("terminal/open").length, 1)
  assert.equal(session.getSnapshot().phase, "opening")
})

test("typed input and keys are sent as base64 only while running", async () => {
  const { session, connection } = createSession()
  session.start()
  // 打开期间的输入丢弃。
  session.terminal.input("early", true)
  assert.equal(connection.notifications.length, 0)
  await settle()
  session.terminal.input("ls 你好\r", true)
  session.sendKey("up")
  session.sendKey("ctrl-c")
  assert.deepEqual(
    connection.notifications.map((each) => each.params),
    [
      { terminal_id: "t1", data: encode("ls 你好\r") },
      { terminal_id: "t1", data: encode("\x1b[A") },
      { terminal_id: "t1", data: encode("\x03") },
    ]
  )
  assert.ok(
    connection.notifications.every((each) => each.method === "terminal/input")
  )
  await session.close()
})

test("control modifier applies to the next input only", async () => {
  const { session, connection } = createSession()
  session.start()
  await settle()
  session.setControlPending(true)
  assert.equal(session.getSnapshot().isControlPending, true)
  session.terminal.input("c", true)
  session.terminal.input("c", true)
  assert.equal(session.getSnapshot().isControlPending, false)
  assert.deepEqual(
    connection.notifications.map((each) => each.params),
    [
      { terminal_id: "t1", data: encode("\x03") },
      { terminal_id: "t1", data: encode("c") },
    ]
  )
  await session.close()
})

test("title follows the shell and falls back to executor and directory", async () => {
  const { session } = createSession("ssh-a", "/srv/app")
  const titles: string[] = []
  session.subscribe(() => titles.push(session.getSnapshot().title))
  session.start()
  assert.equal(session.getSnapshot().title, "ssh-a · app")
  await settle()
  session.terminal.write("\x1b]0;vim main.go\x07")
  await readFirstLine(session)
  assert.equal(session.getSnapshot().title, "vim main.go")
  assert.equal(titles.at(-1), "vim main.go")
  await session.close()
})

test("close ends the running terminal and a terminal that opens later", async () => {
  const cases = [
    { name: "running", holdOpen: false },
    { name: "still opening", holdOpen: true },
  ]
  for (const each of cases) {
    const { session, connection } = createSession()
    connection.holdOpen = each.holdOpen
    session.start()
    await settle()
    const closed = session.close()
    assert.equal(connection.listenerCount, 0, each.name)
    if (each.holdOpen) {
      assert.equal(connection.getCalls("terminal/close").length, 0, each.name)
      connection.resolveOpen()
    }
    await closed
    // 关闭时已经运行的终端，或之后才打开成功的终端，都要通知 serve 结束。
    const closes = connection.getCalls("terminal/close")
    assert.equal(closes.length, 1, each.name)
    assert.equal(
      (closes[0].params as { terminal_id: string }).terminal_id.startsWith("t"),
      true,
      each.name
    )
  }
})

test("close ends a terminal that is still being attached", async () => {
  const cases = [
    { name: "connection is fine", connected: true },
    // 断线时请求发不出去，终端还在运行，重连后标签页会随运行列表回来。
    { name: "connection is down", connected: false },
  ]
  for (const each of cases) {
    const { session, connection } = createSession()
    session.resume("t9")
    if (!each.connected) connection.$state.set("reconnecting")
    await session.close()
    const closes = connection.getCalls("terminal/close")
    assert.deepEqual(
      closes.map((call) => call.params),
      [{ terminal_id: "t9" }],
      each.name
    )
    assert.equal(connection.listenerCount, 0, each.name)
  }
})
