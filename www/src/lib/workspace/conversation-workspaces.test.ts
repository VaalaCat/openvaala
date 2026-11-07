import assert from "node:assert/strict"
import { before, mock, test } from "node:test"
import { QueryClient } from "@tanstack/react-query"
import { atom } from "nanostores"
import { i18n, initializeI18n } from "../../i18n"
import type { MethodParams, TerminalItem } from "../api/protocol.gen"
import type { ServerConnection } from "../api/server-connection"
import {
  ConversationWorkspaces,
  createMemoryStorage,
  getConversationWorkspaces,
  getWorkspaceStorageKey,
} from "./conversation-workspaces"
import type { TabWorkspace } from "./tab-workspace"
import type { SavedServerWorkspaces, TabWorkspaceTab } from "./types"

const serverID = "server-1"

// 记录 call 的连接：每个对话的同步都成功，分叉、终端和读对话记录立即成功
class FakeConnection {
  readonly $state = atom("connected")
  readonly calls: { method: keyof MethodParams; params: unknown }[] = []
  private terminalCount = 0

  call(method: keyof MethodParams, params: unknown): Promise<unknown> {
    this.calls.push({ method, params })
    switch (method) {
      case "conversation/open": {
        const id = (params as MethodParams["conversation/open"]).conversation_id
        return Promise.resolve({
          conversation: { id, root_session_id: "root-" + id },
        })
      }
      case "conversation/subscribe":
        return Promise.resolve({ title: "", sessions: [] })
      case "session/subscribe":
        return Promise.resolve({
          last_saved_ordinal: 0,
          streaming_items: [],
          pending_inputs: { inputs: [] },
        })
      case "session/items/list":
        return Promise.resolve({ items: [] })
      case "conversation/read": {
        const id = (params as MethodParams["conversation/read"]).conversation_id
        return Promise.resolve({ conversation: { id, executor_settings: {} } })
      }
      case "conversation/fork":
        return Promise.resolve({
          conversation: { id: "side-1", executor_settings: {} },
        })
      case "conversation/submitInput":
        return Promise.resolve({ item_id: "input-1" })
      case "terminal/open":
        return Promise.resolve({
          terminal_id: `t${++this.terminalCount}`,
          executor_id: "local",
          directory: "/work/default",
        })
      case "terminal/attach":
        return Promise.resolve({
          executor_id: "local",
          directory: "/work/default",
          rows: 24,
          cols: 80,
        })
      case "conversation/appendContextItems":
      case "conversation/interruptTurn":
      case "conversation/unsubscribe":
      case "session/unsubscribe":
      case "terminal/close":
        return Promise.resolve({})
      default:
        return new Promise(() => {})
    }
  }

  notify(): void {}

  listenServer(): () => void {
    return () => {}
  }

  getServerID(): string {
    return serverID
  }

  listen(): () => void {
    return () => {}
  }

  onConnected(): () => void {
    return () => {}
  }

  getCalls(method: keyof MethodParams, conversationID?: string) {
    return this.calls.filter(
      (call) =>
        call.method === method &&
        (conversationID === undefined ||
          (call.params as { conversation_id?: string }).conversation_id ===
            conversationID)
    )
  }
}

// 让已发出的请求回复（同步、终端打开）处理完
function settle() {
  return new Promise<void>((resolve) => setImmediate(resolve))
}

function createWorkspaces(
  storage: Storage = createMemoryStorage(),
  connection = new FakeConnection(),
  id = serverID
) {
  const workspaces = new ConversationWorkspaces(
    id,
    connection as unknown as ServerConnection,
    new QueryClient(),
    atom<TerminalItem[] | undefined>(undefined),
    storage,
    () => {}
  )
  return { workspaces, connection, storage }
}

// 读出 storage 里保存的全部布局
function readSaved(storage: Storage): SavedServerWorkspaces {
  return JSON.parse(storage.getItem(getWorkspaceStorageKey(serverID)) ?? "null")
}

// 一条保存的布局：所有标签页放在同一个区域里
function createSavedConversation(tabs: Record<string, TabWorkspaceTab>) {
  return {
    layout: {
      global: {},
      layout: {
        type: "row",
        children: [
          {
            type: "tabset",
            active: true,
            children: Object.keys(tabs).map((id) => ({
              type: "tab",
              id,
              name: id,
              component: id,
            })),
          },
        ],
      },
    },
    tabs,
    usedAt: 0,
  }
}

// 这条请求的参数是关于这条对话的吗？
function isAbout(params: unknown, conversationID: string): boolean {
  return (
    (params as { conversation_id?: string }).conversation_id === conversationID
  )
}

// 返回工作区里各标签页的种类
function getTabKinds(workspace: TabWorkspace): string[] {
  return Object.values(workspace.$tabs.get()).map((tab) => tab.kind)
}

before(async () => {
  await initializeI18n()
  await i18n.changeLanguage("zh-CN")
})

test("open follows the current conversation", () => {
  const cases = [
    {
      name: "the same conversation again",
      act: (workspaces: ConversationWorkspaces) => {
        const first = workspaces.open("c1")
        return { first, second: workspaces.open("c1") }
      },
      current: "c1",
      sameInstance: true,
      opened: ["c1"],
    },
    {
      name: "another conversation",
      act: (workspaces: ConversationWorkspaces) => {
        const first = workspaces.open("c1")
        return { first, second: workspaces.open("c2") }
      },
      current: "c2",
      sameInstance: false,
      opened: ["c2"],
    },
    {
      name: "leaving",
      act: (workspaces: ConversationWorkspaces) => {
        const first = workspaces.open("c1")
        workspaces.leave()
        return { first, second: first }
      },
      current: undefined,
      sameInstance: true,
      opened: [] as string[],
    },
  ]
  for (const each of cases) {
    const { workspaces } = createWorkspaces()
    assert.equal(workspaces.$currentConversationID.get(), undefined)
    const { first, second } = each.act(workspaces)
    assert.equal(
      workspaces.$currentConversationID.get(),
      each.current,
      each.name
    )
    assert.equal(first === second, each.sameInstance, each.name)
    assert.deepEqual(getTabKinds(second), ["main"], each.name)
    // getWorkspace 只返回还开着的工作区：没有后台任务的上一条对话已经释放。
    for (const id of ["c1", "c2"])
      assert.equal(
        workspaces.getWorkspace(id) !== undefined,
        each.opened.includes(id),
        each.name + " " + id
      )
    if (each.current)
      assert.equal(workspaces.getWorkspace(each.current), second, each.name)
  }
})

test("leaving releases a workspace without background work", async () => {
  const cases = [
    {
      name: "opening another conversation",
      leave: (workspaces: ConversationWorkspaces) => void workspaces.open("c2"),
    },
    {
      name: "going back to the new conversation page",
      leave: (workspaces: ConversationWorkspaces) => workspaces.leave(),
    },
  ]
  for (const each of cases) {
    const { workspaces, connection } = createWorkspaces()
    const first = workspaces.open("c1")
    first.openSubAgent("s1")
    await settle()
    // 对话级订阅整个工作区只有一份，子 Agent 只多订阅自己 Session 的内容。
    assert.equal(connection.getCalls("conversation/subscribe", "c1").length, 1)
    assert.equal(connection.getCalls("session/subscribe", "c1").length, 2)
    const before = connection.calls.length
    each.leave(workspaces)
    await settle()
    // 1. 根 Session 和子 Agent 的订阅全部停掉，对话级订阅只退订一次。
    const stopped = connection.calls
      .slice(before)
      .filter((call) => isAbout(call.params, "c1"))
    assert.equal(
      stopped.filter((call) => call.method === "conversation/unsubscribe")
        .length,
      1,
      each.name
    )
    assert.deepEqual(
      stopped
        .filter((call) => call.method === "session/unsubscribe")
        .map((call) => (call.params as { session_id: string }).session_id)
        .sort(),
      ["root-c1", "s1"],
      each.name
    )
    // 2. 回来时是新的工作区，布局照旧，根 Session 和子 Agent 重新订阅，对话级订阅又只有一份。
    const subscribed = connection.getCalls(
      "conversation/subscribe",
      "c1"
    ).length
    const reopened = workspaces.open("c1")
    await settle()
    assert.notEqual(reopened, first, each.name)
    assert.equal(
      connection.getCalls("conversation/subscribe", "c1").length - subscribed,
      1,
      each.name
    )
    assert.deepEqual(getTabKinds(reopened), ["main", "sub-agent"], each.name)
    assert.deepEqual(reopened.$tabs.get(), first.$tabs.get(), each.name)
  }
})

test("leaving keeps a workspace with a terminal", async () => {
  const cases = [
    {
      name: "terminal",
      kind: "terminal",
      arrange: (workspace: TabWorkspace) => workspace.openTerminal(),
    },
    {
      name: "side conversation",
      kind: "side-conversation",
      arrange: (workspace: TabWorkspace) =>
        workspace.openSideConversation("问题"),
    },
  ]
  for (const each of cases) {
    const { workspaces, connection } = createWorkspaces()
    const first = workspaces.open("c1")
    const tabID = await each.arrange(first)
    await settle()
    const stopMethods = [
      "conversation/unsubscribe",
      "session/unsubscribe",
      "conversation/interruptTurn",
      "terminal/close",
    ]
    const before = connection.calls.length
    workspaces.open("c2")
    await settle()
    // 1. 订阅一个都没停，没发 terminal/close。
    assert.deepEqual(
      connection.calls
        .slice(before)
        .filter(
          (call) =>
            stopMethods.includes(call.method) &&
            (call.method === "terminal/close" ||
              (call.params as { conversation_id: string }).conversation_id !==
                "c2")
        ),
      [],
      each.name
    )
    // 2. 切回来是同一个工作区，标签页还在。
    const opened = connection.getCalls("conversation/open").length
    assert.equal(workspaces.open("c1"), first, each.name)
    assert.equal(first.$tabs.get()[tabID].kind, each.kind, each.name)
    assert.equal(connection.getCalls("conversation/open").length, opened)
    if (each.kind === "terminal")
      assert.equal(
        first.getTerminalSession(tabID)?.getSnapshot().phase,
        "running"
      )
  }
})

test("a new instance restores the saved layout of the opened conversation", async () => {
  const { workspaces, storage } = createWorkspaces()
  const first = workspaces.open("c1")
  first.openSubAgent("s1")
  await first.openTerminal()
  // 保存的内容按对话 ID 分开，每条有布局、标签页和最近使用时间。
  const saved = readSaved(storage)
  assert.deepEqual(Object.keys(saved.conversations), ["c1"])
  assert.deepEqual(saved.conversations.c1.tabs, first.$tabs.get())
  assert.deepEqual(saved.conversations.c1.layout, first.$layout.get().toJson())
  assert.equal(typeof saved.conversations.c1.usedAt, "number")
  // 刷新：新的实例只恢复被打开的那条对话，没保存过的对话只有主对话。
  const { workspaces: reloaded, connection } = createWorkspaces(storage)
  const restored = reloaded.open("c1")
  assert.deepEqual(restored.$tabs.get(), first.$tabs.get())
  assert.deepEqual(
    restored.$layout.get().toJson(),
    first.$layout.get().toJson()
  )
  assert.deepEqual(getTabKinds(reloaded.open("c2")), ["main"])
  assert.equal(connection.getCalls("terminal/open").length, 0)
})

test("saved terminal id survives reload", async () => {
  const { workspaces, storage } = createWorkspaces()
  const first = workspaces.open("c1")
  const tabID = await first.openTerminal()
  await settle()
  // 终端打开成功后，终端 ID 随布局一起保存。
  assert.deepEqual(readSaved(storage).conversations.c1.tabs[tabID], {
    kind: "terminal",
    executorID: "local",
    directory: "",
    terminalID: "t1",
  })
  // 刷新：新实例的标签页接回原来的终端，而不是新开或显示已结束。
  const { workspaces: reloaded, connection } = createWorkspaces(storage)
  const restored = reloaded.open("c1")
  await settle()
  assert.deepEqual(
    connection.getCalls("terminal/attach").map((call) => call.params),
    [{ terminal_id: "t1", cursor: 0 }]
  )
  assert.equal(connection.getCalls("terminal/open").length, 0)
  assert.equal(
    restored.getTerminalSession(tabID)?.getSnapshot().phase,
    "running"
  )
})

test("saved layouts are capped at 50 by usedAt", () => {
  const cases = [
    { name: "the oldest is dropped", touchOldest: false, dropped: "c0" },
    { name: "a recently used one survives", touchOldest: true, dropped: "c1" },
  ]
  mock.timers.enable({ apis: ["Date"], now: 1000 })
  try {
    for (const each of cases) {
      const { workspaces, storage } = createWorkspaces()
      for (let index = 0; index < 50; index++) {
        mock.timers.tick(1)
        workspaces.open(`c${index}`).openMain()
      }
      assert.equal(Object.keys(readSaved(storage).conversations).length, 50)
      if (each.touchOldest) {
        mock.timers.tick(1)
        workspaces.open("c0").openMain()
      }
      mock.timers.tick(1)
      workspaces.open("c50").openMain()
      const saved = Object.keys(readSaved(storage).conversations)
      assert.equal(saved.length, 50, each.name)
      assert.ok(saved.includes("c50"), each.name)
      assert.ok(!saved.includes(each.dropped), each.name)
    }
  } finally {
    mock.timers.reset()
  }
})

test("instances sharing one storage keep each other's saved layouts", () => {
  // 两个实例相当于同一个浏览器里的两个页面：先建好，之后各自保存。
  const cases: {
    name: string
    act: (a: ConversationWorkspaces, b: ConversationWorkspaces) => void
    saved: string[]
  }[] = [
    {
      name: "each saves a different conversation",
      act: (a, b) => {
        a.open("c1").openSubAgent("s1")
        b.open("c2").openSubAgent("s2")
      },
      saved: ["c1", "c2"],
    },
    {
      name: "forgetting one keeps the other's conversation",
      act: (a, b) => {
        a.open("c1").openSubAgent("s1")
        b.open("c2").openSubAgent("s2")
        a.forget("c1")
      },
      saved: ["c2"],
    },
    {
      name: "a layout forgotten by the other instance is not written back",
      act: (a, b) => {
        a.open("c1").openSubAgent("s1")
        b.forget("c1")
        a.open("c2").openSubAgent("s2")
      },
      saved: ["c2"],
    },
  ]
  for (const each of cases) {
    const storage = createMemoryStorage()
    const { workspaces: a } = createWorkspaces(storage)
    const { workspaces: b } = createWorkspaces(storage)
    each.act(a, b)
    assert.deepEqual(
      Object.keys(readSaved(storage).conversations).sort(),
      each.saved,
      each.name
    )
  }
})

test("open restores a layout the other instance saved after this one was created", () => {
  const storage = createMemoryStorage()
  const { workspaces: a } = createWorkspaces(storage)
  const { workspaces: b } = createWorkspaces(storage)
  a.open("c1").openSubAgent("s1")
  assert.deepEqual(getTabKinds(b.open("c1")), ["main", "sub-agent"])
})

test("old saved format is ignored", () => {
  const validNeighbour = createSavedConversation({
    main: { kind: "main" },
    sub: { kind: "sub-agent", sessionID: "s9" },
  })
  // 1. 84faf45 的格式：整个 serve 一份 {layout, tabs}。
  const oldFormat = createSavedConversation({
    "tab-1": { kind: "conversation" },
    "tab-2": { kind: "new-conversation" },
  } as unknown as Record<string, TabWorkspaceTab>)
  const cases: { name: string; text: string; neighbour: string[] }[] = [
    {
      name: "the old {layout, tabs} format",
      text: JSON.stringify({ layout: oldFormat.layout, tabs: oldFormat.tabs }),
      neighbour: ["main"],
    },
    { name: "broken json", text: "{", neighbour: ["main"] },
    { name: "null", text: "null", neighbour: ["main"] },
    { name: "string", text: '"text"', neighbour: ["main"] },
    {
      name: "conversations is a number",
      text: '{"conversations":1}',
      neighbour: ["main"],
    },
    {
      name: "conversations is an array",
      text: '{"conversations":[]}',
      neighbour: ["main"],
    },
    {
      name: "one entry has an unknown tab kind",
      text: JSON.stringify({
        conversations: {
          c1: createSavedConversation({
            bogus: { kind: "bogus" } as unknown as TabWorkspaceTab,
          }),
          c2: validNeighbour,
        },
      }),
      neighbour: ["main", "sub-agent"],
    },
    {
      name: "one entry has no usedAt",
      text: JSON.stringify({
        conversations: {
          c1: { ...validNeighbour, usedAt: undefined },
          c2: validNeighbour,
        },
      }),
      neighbour: ["main", "sub-agent"],
    },
    {
      name: "terminal id is not a string",
      text: JSON.stringify({
        conversations: {
          c1: createSavedConversation({
            terminal: {
              kind: "terminal",
              executorID: "local",
              directory: "/work",
              terminalID: 7,
            } as unknown as TabWorkspaceTab,
          }),
          c2: validNeighbour,
        },
      }),
      neighbour: ["main", "sub-agent"],
    },
    {
      name: "one tab is missing a field",
      text: JSON.stringify({
        conversations: {
          c1: createSavedConversation({
            terminal: {
              kind: "terminal",
              executorID: "local",
            } as TabWorkspaceTab,
          }),
          c2: validNeighbour,
        },
      }),
      neighbour: ["main", "sub-agent"],
    },
  ]
  for (const each of cases) {
    const storage = createMemoryStorage()
    storage.setItem(getWorkspaceStorageKey(serverID), each.text)
    const { workspaces, connection } = createWorkspaces(storage)
    // 2. 解析不出来或内容不完整的对话，打开后只有主对话，不抛错。
    const workspace = workspaces.open("c1")
    assert.deepEqual(getTabKinds(workspace), ["main"], each.name)
    assert.equal(connection.getCalls("conversation/open").length, 1, each.name)
    // 3. 内容完整的相邻对话照常恢复。
    assert.deepEqual(
      getTabKinds(workspaces.open("c2")),
      each.neighbour,
      each.name
    )
  }
})

test("forget releases and deletes", async () => {
  const cases = [
    {
      name: "a workspace kept in the background",
      arrange: async (workspaces: ConversationWorkspaces) => {
        const workspace = workspaces.open("c1")
        await workspace.openTerminal()
        await settle()
        workspaces.open("c2")
        return { released: true, closedTerminals: 1 }
      },
    },
    {
      name: "the current workspace",
      arrange: async (workspaces: ConversationWorkspaces) => {
        workspaces.open("c1").openMain()
        await settle()
        return { released: true, closedTerminals: 0 }
      },
    },
    {
      name: "a conversation that is only saved",
      arrange: async (workspaces: ConversationWorkspaces) => {
        workspaces.open("c1").openMain()
        workspaces.open("c2")
        await settle()
        return { released: false, closedTerminals: 0 }
      },
    },
  ]
  for (const each of cases) {
    const { workspaces, connection, storage } = createWorkspaces()
    const { released, closedTerminals } = await each.arrange(workspaces)
    assert.ok("c1" in readSaved(storage).conversations, each.name)
    const before = connection.calls.length
    workspaces.forget("c1")
    await settle()
    // 1. 释放了工作区：订阅停掉，终端关掉；当前对话被删时回到没有当前对话。
    const stopped = connection.calls
      .slice(before)
      .filter((call) => isAbout(call.params, "c1"))
    assert.equal(
      stopped.filter((call) => call.method === "session/unsubscribe").length,
      released ? 1 : 0,
      each.name
    )
    assert.equal(
      connection.calls
        .slice(before)
        .filter((call) => call.method === "terminal/close").length,
      closedTerminals,
      each.name
    )
    assert.equal(
      workspaces.$currentConversationID.get(),
      each.name === "the current workspace" ? undefined : "c2",
      each.name
    )
    // 2. 保存的布局没有它了，之后打开是只有主对话的新工作区。
    assert.ok(!("c1" in readSaved(storage).conversations), each.name)
    assert.deepEqual(getTabKinds(workspaces.open("c1")), ["main"], each.name)
  }
})

test("storage errors still open the main tab", () => {
  const fail = () => {
    throw new Error("storage denied")
  }
  const cases = [
    { name: "write fails", getItem: undefined, setItem: fail },
    { name: "read fails", getItem: fail, setItem: undefined },
    { name: "read and write fail", getItem: fail, setItem: fail },
  ]
  for (const each of cases) {
    const storage = createMemoryStorage()
    if (each.getItem) storage.getItem = each.getItem
    if (each.setItem) storage.setItem = each.setItem
    const { workspaces } = createWorkspaces(storage)
    const workspace = workspaces.open("c1")
    const mainTabID = workspace.openMain()
    assert.deepEqual(Object.keys(workspace.$tabs.get()), [mainTabID], each.name)
    assert.equal(workspace.$selectedTabID.get(), mainTabID, each.name)
  }
})

test("a disabled localStorage still gives a working workspace", () => {
  // 存储被禁用时，读取 window.localStorage 这个属性本身就会抛错。
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: {
      get localStorage(): Storage {
        throw new Error("SecurityError")
      },
    },
  })
  try {
    const workspace = getConversationWorkspaces(
      "disabled-storage",
      new FakeConnection() as unknown as ServerConnection,
      new QueryClient(),
      atom<TerminalItem[] | undefined>(undefined)
    ).open("c1")
    const mainTabID = workspace.openMain()
    assert.deepEqual(Object.keys(workspace.$tabs.get()), [mainTabID])
    assert.equal(workspace.$selectedTabID.get(), mainTabID)
  } finally {
    Reflect.deleteProperty(globalThis, "window")
  }
})

test("separate servers use separate keys", async () => {
  const storage = createMemoryStorage()
  const { workspaces: a } = createWorkspaces(storage, new FakeConnection(), "a")
  await a.open("c1").openTerminal()
  assert.equal(getWorkspaceStorageKey("a"), "openvaala.workspace.a")
  assert.equal(storage.getItem(getWorkspaceStorageKey("b")), null)
  // 另一个 serve 上同一个对话 ID 不会读到这份布局。
  const { workspaces: b } = createWorkspaces(storage, new FakeConnection(), "b")
  assert.deepEqual(getTabKinds(b.open("c1")), ["main"])
  const { workspaces: restoredA } = createWorkspaces(
    storage,
    new FakeConnection(),
    "a"
  )
  assert.deepEqual(getTabKinds(restoredA.open("c1")), ["main", "terminal"])
})
