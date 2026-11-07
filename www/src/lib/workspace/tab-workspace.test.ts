import assert from "node:assert/strict"
import { before, test } from "node:test"
import { QueryClient } from "@tanstack/react-query"
import { atom } from "nanostores"
import {
  Actions,
  DockLocation,
  Orientation,
  type TabNode,
  type TabSetNode,
} from "flexlayout-react"
import { i18n, initializeI18n } from "../../i18n"
import {
  ErrorCodeConversationNotFound,
  type ExecutorSettings,
  type MethodParams,
  type SessionRuntimeInfo,
  type TerminalItem,
} from "../api/protocol.gen"
import type { ServerConnection } from "../api/server-connection"
import { RpcError } from "../rpc-error"
import { sideConversationInstruction } from "./side-conversation"
import { TabWorkspace } from "./tab-workspace"
import type { SavedConversationWorkspace, TabWorkspaceTab } from "./types"

const serverID = "server-1"

// 记录 call 的连接：对话同步的请求一直不返回，停在「同步中」（syncableConversations 里的对话同步成功，
// missingConversations 里的对话打开时报对话不存在）；conversation/read 按 conversationSettings 回复，
// 分叉、注入、提交和终端请求立即成功（failingMethods 里的方法失败；holdTerminalOpen 和 holdTerminalClose 时
// terminal/open、terminal/close 的回复由测试决定什么时候给）
class FakeConnection {
  readonly $state = atom("connected")
  readonly calls: { method: keyof MethodParams; params: unknown }[] = []
  // 对话 ID → 它的 Executor 设置；没有的对话 conversation/read 失败
  readonly conversationSettings: Record<string, ExecutorSettings> = {}
  readonly syncableConversations = new Set<string>()
  readonly missingConversations = new Set<string>()
  readonly failingMethods = new Set<keyof MethodParams>()
  holdTerminalOpen = false
  holdTerminalClose = false
  private readonly pendingTerminalReplies: (() => void)[] = []
  // 每次分叉按顺序给出的新对话 ID
  private readonly forkedConversationIDs = ["side-1", "side-2"]
  private terminalCount = 0

  call(method: keyof MethodParams, params: unknown): Promise<unknown> {
    this.calls.push({ method, params })
    if (this.failingMethods.has(method))
      return Promise.reject(new RpcError(-32603, "boom", method))
    switch (method) {
      case "conversation/fork":
        return Promise.resolve({
          conversation: {
            id: this.forkedConversationIDs.shift(),
            executor_settings: (params as MethodParams["conversation/fork"])
              .executor_settings,
          },
        })
      case "conversation/appendContextItems":
      case "conversation/interruptTurn":
      case "conversation/unsubscribe":
      case "session/unsubscribe":
        return Promise.resolve({})
      case "conversation/submitInput":
        return Promise.resolve({ item_id: "input-1" })
      case "conversation/open": {
        const id = (params as MethodParams["conversation/open"]).conversation_id
        if (this.missingConversations.has(id))
          return Promise.reject(
            new RpcError(ErrorCodeConversationNotFound, "not found", method)
          )
        return this.syncableConversations.has(id)
          ? Promise.resolve({
              conversation: { id, root_session_id: "root-" + id },
            })
          : new Promise(() => {})
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
        const settings = this.conversationSettings[id]
        return settings
          ? Promise.resolve({
              conversation: { id, executor_settings: settings },
            })
          : Promise.reject(new RpcError(-32001, "not found", method))
      }
      case "terminal/open": {
        const { executor_id, directory } =
          params as MethodParams["terminal/open"]
        const opened = {
          terminal_id: `t${++this.terminalCount}`,
          executor_id: executor_id || "local",
          directory: directory || "/work/default",
        }
        return this.holdTerminalOpen
          ? this.holdReply(opened)
          : Promise.resolve(opened)
      }
      case "terminal/attach":
        return Promise.resolve({
          executor_id: "local",
          directory: "/work/default",
          rows: 24,
          cols: 80,
        })
      case "terminal/close":
        return this.holdTerminalClose ? this.holdReply({}) : Promise.resolve({})
      case "terminal/reset":
        return Promise.resolve({})
      default:
        return new Promise(() => {})
    }
  }

  // 回复被扣住的请求，按发出的顺序一次回复一个
  releaseReply(): void {
    this.pendingTerminalReplies.shift()!()
  }

  private holdReply<T>(reply: T): Promise<T> {
    return new Promise((resolve) =>
      this.pendingTerminalReplies.push(() => resolve(reply))
    )
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

  getCalls(method: keyof MethodParams) {
    return this.calls.filter((call) => call.method === method)
  }
}

// 让已发出的请求回复（终端打开的结果）处理完
function settle() {
  return new Promise<void>((resolve) => setImmediate(resolve))
}

function createWorkspace(
  saved?: SavedConversationWorkspace,
  connection = new FakeConnection(),
  runningTerminals = atom<TerminalItem[] | undefined>(undefined)
) {
  // notices 是工作区发出的提示，按发出的顺序
  const notices: string[] = []
  const workspace = new TabWorkspace(
    "c1",
    connection as unknown as ServerConnection,
    new QueryClient(),
    runningTerminals,
    saved,
    (message) => notices.push(message)
  )
  return { workspace, connection, notices, runningTerminals }
}

const sourceSettings: ExecutorSettings = {
  default_executor_id: "local",
  workspace_directories: { local: "/work/repo" },
}

// 对话 c1 的工作区，可以从它的主对话分出旁支；syncableConversations 里的对话同步成功
function createWorkspaceWithSource(syncableConversations: string[] = []) {
  const connection = new FakeConnection()
  connection.conversationSettings.c1 = sourceSettings
  for (const id of syncableConversations)
    connection.syncableConversations.add(id)
  const created = createWorkspace(undefined, connection)
  return { ...created, mainTabID: created.workspace.openMain() }
}

function getTabSets(workspace: TabWorkspace): TabSetNode[] {
  const tabSets: TabSetNode[] = []
  workspace.$layout.get().visitNodes((node) => {
    if (node.getType() === "tabset") tabSets.push(node as TabSetNode)
  })
  return tabSets
}

// 每个区域里标签页 ID 的顺序
function getTabOrder(workspace: TabWorkspace): string[][] {
  return getTabSets(workspace).map((tabSet) =>
    tabSet.getTabNodes().map((tab) => tab.getId())
  )
}

// 写一份保存好的工作区：所有标签页放在同一个区域里
function createSavedTabs(
  tabs: Record<string, TabWorkspaceTab>
): SavedConversationWorkspace {
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

function createSideTab(conversationID: string): TabWorkspaceTab {
  return {
    kind: "side-conversation",
    conversationID,
    boundaryItemID: "item-1",
    question: "为什么用 SQLite",
  }
}

// serve 上运行中的一个终端
function createRunningTerminal(
  terminalID: string,
  conversationID?: string
): TerminalItem {
  return {
    terminal_id: terminalID,
    executor_id: "ssh-a",
    directory: "/srv/app",
    started_at: "2026-11-06T00:00:00Z",
    conversation_id: conversationID,
  }
}

// 工作区里终端标签页的终端 ID，按标签页登记的顺序
function getTerminalTabIDs(workspace: TabWorkspace): (string | undefined)[] {
  return Object.values(workspace.$tabs.get()).flatMap((tab) =>
    tab.kind === "terminal" ? [tab.terminalID] : []
  )
}

before(async () => {
  await initializeI18n()
  await i18n.changeLanguage("zh-CN")
})

test("a new workspace opens the main tab", async () => {
  const { workspace, connection } = createWorkspace(
    undefined,
    Object.assign(new FakeConnection(), {
      syncableConversations: new Set(["c1"]),
    })
  )
  await settle()
  const [mainTabID] = Object.keys(workspace.$tabs.get())
  assert.deepEqual(Object.values(workspace.$tabs.get()), [{ kind: "main" }])
  assert.equal(workspace.$selectedTabID.get(), mainTabID)
  assert.equal(workspace.$lastConversationTabID.get(), mainTabID)
  assert.equal(getTabSets(workspace).length, 1)
  assert.deepEqual(
    connection.getCalls("conversation/open").map((call) => call.params),
    [{ conversation_id: "c1" }]
  )
  // 主对话显示根 Session 的同步。
  assert.equal(
    workspace.getConversationSync(mainTabID),
    workspace.getRootSync()
  )
  assert.equal(workspace.getRootSync().getFollowedSessionID(), "root-c1")
})

test("opening the same sub agent selects its tab", async () => {
  const cases = [
    { name: "twice in a row", opens: ["s1", "s1"], subAgents: 1, selected: 0 },
    {
      name: "after another sub agent",
      opens: ["s1", "s2", "s1"],
      subAgents: 2,
      selected: 0,
    },
    {
      name: "two different ones",
      opens: ["s1", "s2"],
      subAgents: 2,
      selected: 1,
    },
  ]
  for (const each of cases) {
    const connection = new FakeConnection()
    connection.syncableConversations.add("c1")
    const { workspace } = createWorkspace(undefined, connection)
    const mainTabID = workspace.openMain()
    const tabIDs = each.opens.map((sessionID) =>
      workspace.openSubAgent(sessionID)
    )
    await settle()
    // 一个 Session 只有一个标签页，选中最后打开的那个。
    const uniqueTabIDs = [...new Set(tabIDs)]
    assert.equal(uniqueTabIDs.length, each.subAgents, each.name)
    assert.equal(
      Object.keys(workspace.$tabs.get()).length,
      1 + each.subAgents,
      each.name
    )
    assert.equal(
      workspace.$selectedTabID.get(),
      uniqueTabIDs[each.selected],
      each.name
    )
    // 主对话仍然跟随根 Session，子 Agent 标签页跟随各自的 Session。
    assert.equal(
      workspace.getConversationSync(mainTabID)?.getFollowedSessionID(),
      "root-c1",
      each.name
    )
    assert.deepEqual(
      uniqueTabIDs.map((tabID) =>
        workspace.getConversationSync(tabID)?.getFollowedSessionID()
      ),
      [...new Set(each.opens)],
      each.name
    )
    // 对话级订阅整个工作区只有一份，子 Agent 只多订阅各自 Session 的内容，并共用根 Session 的标题和 Session 信息。
    assert.equal(connection.getCalls("conversation/subscribe").length, 1)
    assert.equal(connection.getCalls("conversation/unsubscribe").length, 1)
    assert.deepEqual(
      connection
        .getCalls("session/subscribe")
        .map((call) => (call.params as { session_id: string }).session_id)
        .sort(),
      ["root-c1", ...new Set(each.opens)].sort(),
      each.name
    )
    for (const tabID of uniqueTabIDs)
      assert.equal(
        workspace.getConversationSync(tabID)?.$sessions,
        workspace.getRootSync().$sessions,
        each.name
      )
  }
})

test("the main tab cannot be closed", () => {
  const terminal: TabWorkspaceTab = {
    kind: "terminal",
    executorID: "local",
    directory: "/w",
  }
  const cases: {
    name: string
    saved?: SavedConversationWorkspace
    wantKinds: string[]
  }[] = [
    { name: "new workspace", wantKinds: ["main"] },
    {
      name: "restored from a layout saved when the main tab was closable",
      saved: createSavedTabs({
        main: { kind: "main" },
        sub: { kind: "sub-agent", sessionID: "s1" },
        terminal,
      }),
      wantKinds: ["main", "sub-agent", "terminal"],
    },
    {
      name: "restored layout without the main tab",
      saved: createSavedTabs({ terminal }),
      wantKinds: ["terminal", "main"],
    },
  ]
  for (const each of cases) {
    const { workspace } = createWorkspace(each.saved)
    const restored = createWorkspace(workspace.toSaved()).workspace
    for (const target of [workspace, restored]) {
      assert.deepEqual(
        Object.values(target.$tabs.get()).map((tab) => tab.kind),
        each.wantKinds,
        each.name
      )
      // 只有主对话标签页不能关：布局节点的属性和标签页的关闭按钮都一样。
      for (const [tabID, tab] of Object.entries(target.$tabs.get())) {
        const node = target.$layout.get().getNodeById(tabID) as TabNode
        assert.equal(node.isEnableClose(), tab.kind !== "main", each.name)
        assert.equal(
          target.isTabClosable(tabID),
          tab.kind !== "main",
          each.name
        )
      }
    }
  }
})

test("closing the other tabs leaves the main tab", async () => {
  const cases = [
    {
      name: "a sub agent",
      open: (workspace: TabWorkspace) => workspace.openSubAgent("s1"),
      wantCalls: [
        ["session/unsubscribe", { conversation_id: "c1", session_id: "s1" }],
      ],
    },
    {
      name: "a terminal",
      open: (workspace: TabWorkspace) => workspace.openTerminal(),
      wantCalls: [["terminal/close", { terminal_id: "t1" }]],
    },
  ]
  for (const each of cases) {
    const connection = new FakeConnection()
    connection.syncableConversations.add("c1")
    const { workspace } = createWorkspace(undefined, connection)
    const mainTabID = workspace.openMain()
    const closedTabID = await each.open(workspace)
    await settle()
    const calls = connection.calls.length
    await workspace.closeTab(closedTabID)
    // 主对话留在原来的标签页里并被选中，仍显示根 Session 的同步。
    assert.deepEqual(Object.keys(workspace.$tabs.get()), [mainTabID], each.name)
    assert.equal(workspace.$selectedTabID.get(), mainTabID, each.name)
    assert.equal(getTabSets(workspace).length, 1, each.name)
    assert.equal(workspace.getConversationSync(closedTabID), undefined)
    assert.equal(
      workspace.getConversationSync(mainTabID),
      workspace.getRootSync(),
      each.name
    )
    // 根 Session 的同步不停；关掉的子 Agent 只退订自己 Session 的内容，终端被关掉。
    assert.deepEqual(
      connection.calls.slice(calls).map((call) => [call.method, call.params]),
      each.wantCalls,
      each.name
    )
  }
})

test("opening the main tab selects it without subscribing again", async () => {
  const cases = [
    {
      name: "from a sub agent tab",
      open: (workspace: TabWorkspace) => workspace.openSubAgent("s1"),
    },
    {
      name: "from a terminal tab",
      open: (workspace: TabWorkspace) => workspace.openTerminal(),
    },
  ]
  for (const each of cases) {
    const connection = new FakeConnection()
    connection.syncableConversations.add("c1")
    const { workspace } = createWorkspace(undefined, connection)
    const mainTabID = workspace.openMain()
    await each.open(workspace)
    await settle()
    const tabs = workspace.$tabs.get()
    const calls = connection.calls.length
    assert.equal(workspace.openMain(), mainTabID, each.name)
    assert.equal(workspace.$selectedTabID.get(), mainTabID, each.name)
    assert.deepEqual(workspace.$tabs.get(), tabs, each.name)
    assert.deepEqual(connection.calls.slice(calls), [], each.name)
  }
})

test("background work means an open side or terminal tab", async () => {
  const { workspace, mainTabID } = createWorkspaceWithSource()
  const cases = [
    { name: "only the main tab", act: async () => {}, want: false },
    {
      name: "with a sub agent",
      act: async () => void workspace.openSubAgent("s1"),
      want: false,
    },
    {
      name: "with a terminal",
      act: async () => void (await workspace.openTerminal()),
      want: true,
    },
    {
      name: "with the terminal closed again",
      act: async () => {
        const terminalTabID = Object.keys(workspace.$tabs.get()).find(
          (tabID) => workspace.$tabs.get()[tabID].kind === "terminal"
        )!
        await workspace.closeTab(terminalTabID)
      },
      want: false,
    },
    {
      name: "with a side conversation",
      act: async () => void (await workspace.openSideConversation("问题")),
      want: true,
    },
  ]
  assert.ok(workspace.getConversationSync(mainTabID))
  for (const each of cases) {
    await each.act()
    assert.equal(workspace.hasBackgroundWork(), each.want, each.name)
  }
})

test("a restored terminal that already ended is not background work", async () => {
  const tabs: Record<string, TabWorkspaceTab> = {
    main: { kind: "main" },
    terminal: { kind: "terminal", executorID: "local", directory: "/work" },
  }
  const cases = [
    {
      name: "restored terminal is exited",
      act: async () => {},
      want: false,
    },
    {
      name: "restarted terminal is running again",
      act: async (workspace: TabWorkspace) =>
        workspace.restartTerminal("terminal"),
      want: true,
    },
  ]
  for (const each of cases) {
    const { workspace } = createWorkspace(createSavedTabs(tabs))
    await each.act(workspace)
    assert.equal(workspace.hasBackgroundWork(), each.want, each.name)
  }
})

test("a restored layout without the main tab gets it, and the root sync stops only on release", async () => {
  const cases: { name: string; tabs: Record<string, TabWorkspaceTab> }[] = [
    {
      name: "only a terminal",
      tabs: {
        terminal: { kind: "terminal", executorID: "local", directory: "/work" },
      },
    },
    {
      name: "only a side conversation",
      tabs: { side: createSideTab("side-1") },
    },
    {
      name: "only a sub agent",
      tabs: { sub: { kind: "sub-agent", sessionID: "s1" } },
    },
  ]
  for (const each of cases) {
    const connection = new FakeConnection()
    connection.syncableConversations.add("c1")
    connection.syncableConversations.add("side-1")
    const { workspace } = createWorkspace(
      createSavedTabs(each.tabs),
      connection
    )
    await settle()
    // 恢复时补上主对话标签页，它显示根 Session 的同步，对话级订阅只有一份。
    assert.equal(
      Object.values(workspace.$tabs.get()).filter((tab) => tab.kind === "main")
        .length,
      1,
      each.name
    )
    assert.equal(workspace.getRootSync().$syncState.get(), "synced", each.name)
    assert.deepEqual(
      connection
        .getCalls("conversation/subscribe")
        .map((call) => call.params)
        .filter(
          (params) =>
            (params as { conversation_id: string }).conversation_id === "c1"
        ),
      [{ conversation_id: "c1" }],
      each.name
    )
    // release 才停掉根 Session 的同步。
    const calls = connection.calls.length
    workspace.release()
    await settle()
    assert.deepEqual(
      connection.calls
        .slice(calls)
        .filter(
          (call) =>
            (call.params as { conversation_id?: string }).conversation_id ===
            "c1"
        )
        .map((call) => [call.method, call.params])
        .sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b))),
      [
        ["conversation/unsubscribe", { conversation_id: "c1" }],
        ...(each.name === "only a sub agent"
          ? [
              [
                "session/unsubscribe",
                { conversation_id: "c1", session_id: "s1" },
              ],
            ]
          : []),
        [
          "session/unsubscribe",
          { conversation_id: "c1", session_id: "root-c1" },
        ],
      ].sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b))),
      each.name
    )
  }
})

test("area maximize follows the area count and the maximized state", async () => {
  const { workspace } = createWorkspace()
  const [first] = getTabSets(workspace)
  // 只有一个区域时没有最大化一项。
  assert.equal(workspace.getAreaMaximizeAction(first.getId()), undefined)
  assert.equal(workspace.getAreaMaximizeAction("missing"), undefined)
  await workspace.split(first.getId(), "RIGHT")
  const [left, right] = getTabSets(workspace)
  const cases = [
    { name: "two areas", maximize: undefined, want: ["maximize", "maximize"] },
    { name: "maximize right", maximize: right, want: ["maximize", "restore"] },
    { name: "restore right", maximize: right, want: ["maximize", "maximize"] },
  ]
  for (const each of cases) {
    if (each.maximize) workspace.toggleMaximize(each.maximize.getId())
    assert.deepEqual(
      [left, right].map((tabSet) =>
        workspace.getAreaMaximizeAction(tabSet.getId())
      ),
      each.want,
      each.name
    )
  }
})

test("opening a tab outside the maximized area restores it first", async () => {
  // 两个区域：左边是主对话，右边是终端；每个用例先最大化左边。
  const cases = [
    {
      name: "select a tab in the maximized area",
      act: async (w: TabWorkspace, tabIDs: string[]) => w.selectTab(tabIDs[0]),
      maximized: true,
      selected: 0,
    },
    {
      name: "select a tab in the other area",
      act: async (w: TabWorkspace, tabIDs: string[]) => w.selectTab(tabIDs[1]),
      maximized: false,
      selected: 1,
    },
    {
      name: "open the main tab that lives in the maximized area",
      act: async (w: TabWorkspace) => w.openMain(),
      maximized: true,
      selected: 0,
    },
    {
      name: "open a sub agent beside the main tab",
      act: async (w: TabWorkspace) => w.openSubAgent("s1"),
      maximized: false,
      selected: -1,
    },
    {
      name: "open a terminal in the maximized area",
      act: async (w: TabWorkspace, _: string[], first: string) =>
        w.openTerminal(first),
      maximized: true,
      selected: -1,
    },
    {
      name: "split the maximized area",
      act: async (w: TabWorkspace, _: string[], first: string) =>
        w.split(first, "RIGHT"),
      maximized: false,
      selected: -1,
    },
  ]
  for (const each of cases) {
    const { workspace } = createWorkspace()
    const mainTabID = workspace.openMain()
    const terminalTabID = await workspace.split(
      getTabSets(workspace)[0].getId(),
      "RIGHT"
    )
    const [first] = getTabSets(workspace)
    workspace.toggleMaximize(first.getId())
    assert.equal(
      workspace.$layout.get().getMaximizedTabset()?.getId(),
      first.getId(),
      each.name
    )
    await each.act(workspace, [mainTabID, terminalTabID], first.getId())
    assert.equal(
      workspace.$layout.get().getMaximizedTabset() !== undefined,
      each.maximized,
      each.name
    )
    // 选中的标签页总是看得到：要么它在最大化的区域里，要么已经还原。
    const selected = workspace.$layout
      .get()
      .getNodeById(workspace.$selectedTabID.get())
    const isVisible =
      !workspace.$layout.get().getMaximizedTabset() ||
      selected?.getParent() === workspace.$layout.get().getMaximizedTabset()
    assert.ok(isVisible, each.name)
    if (each.selected >= 0)
      assert.equal(
        workspace.$selectedTabID.get(),
        [mainTabID, terminalTabID][each.selected],
        each.name
      )
  }
})

test("closing the last tab of a split merges the area", async () => {
  const cases = [
    { direction: "RIGHT", orientation: Orientation.HORZ },
    { direction: "BOTTOM", orientation: Orientation.VERT },
  ] as const
  for (const each of cases) {
    const { workspace } = createWorkspace()
    const mainTabID = workspace.openMain()
    const firstTabSet = getTabSets(workspace)[0]
    // 1. 分栏：新区域里是一个新终端标签页，并被选中。
    const newTabID = await workspace.split(firstTabSet.getId(), each.direction)
    const tabSets = getTabSets(workspace)
    assert.equal(tabSets.length, 2, each.direction)
    assert.equal(workspace.$selectedTabID.get(), newTabID)
    assert.equal(workspace.$tabs.get()[newTabID].kind, "terminal")
    const newTabSet = tabSets.find((tabSet) =>
      tabSet.getTabNodes().some((tab) => tab.getId() === newTabID)
    )
    assert.equal(newTabSet?.getParent()?.getOrientation(), each.orientation)
    // 2. 关掉新区域的标签页：区域合并，只剩主对话。
    await workspace.closeTab(newTabID)
    assert.equal(getTabSets(workspace).length, 1, each.direction)
    assert.deepEqual(Object.keys(workspace.$tabs.get()), [mainTabID])
    assert.equal(workspace.$selectedTabID.get(), mainTabID)
  }
})

test("tabs removed by the layout release their sync", () => {
  const { workspace, connection } = createWorkspace()
  const keptTabID = workspace.openMain()
  const removedTabID = workspace.openSubAgent("s2")
  // flexlayout 自己的关闭按钮直接改 Model，再通知 handleLayoutChanged。
  workspace.$layout.get().doAction(Actions.deleteTab(removedTabID))
  workspace.handleLayoutChanged()
  assert.deepEqual(Object.keys(workspace.$tabs.get()), [keptTabID])
  assert.equal(workspace.getConversationSync(removedTabID), undefined)
  assert.ok(workspace.getConversationSync(keptTabID))
  // 子 Agent 只退订自己 Session 的内容，对话级订阅归根 Session 的同步。
  assert.deepEqual(
    connection.getCalls("session/unsubscribe").map((call) => call.params),
    [{ conversation_id: "c1", session_id: "s2" }]
  )
  assert.deepEqual(connection.getCalls("conversation/unsubscribe"), [])
  assert.equal(workspace.$selectedTabID.get(), keptTabID)
})

test("restarting the sync only reopens a conversation tab", async () => {
  const cases = [
    { name: "main tab", kind: "main", reopened: 1 },
    { name: "sub agent tab", kind: "sub-agent", reopened: 1 },
    { name: "terminal tab", kind: "terminal", reopened: 0 },
    { name: "closed tab", kind: "closed", reopened: 0 },
  ]
  for (const each of cases) {
    const { workspace, connection } = createWorkspace()
    const tabID =
      each.kind === "main"
        ? workspace.openMain()
        : each.kind === "sub-agent"
          ? workspace.openSubAgent("s1")
          : each.kind === "terminal"
            ? await workspace.openTerminal()
            : "closed"
    const sync = workspace.getConversationSync(tabID)
    const opened = connection.getCalls("conversation/open").length
    workspace.restartConversationSync(tabID)
    // 重试沿用同一个同步，只重新打开对话。
    assert.equal(workspace.getConversationSync(tabID), sync, each.name)
    assert.equal(
      connection.getCalls("conversation/open").length - opened,
      each.reopened,
      each.name
    )
  }
})

test("last conversation tab follows the selected tab and survives terminal selection", async () => {
  const { workspace } = createWorkspace()
  const mainTabID = workspace.openMain()
  assert.equal(workspace.$lastConversationTabID.get(), mainTabID)
  // 选中子 Agent 标签页时跟着变。
  const subAgentTabID = workspace.openSubAgent("s1")
  assert.equal(workspace.$lastConversationTabID.get(), subAgentTabID)
  // 选中终端标签页时，最近的对话标签页不变。
  await workspace.openTerminal()
  assert.equal(workspace.$lastConversationTabID.get(), subAgentTabID)
  // 它被关掉后没有最近的对话标签页。
  await workspace.closeTab(subAgentTabID)
  assert.equal(workspace.$lastConversationTabID.get(), "")
})

test("save and restore keeps layout and tab order", async () => {
  type TabIDs = { main: string; subAgent: string }
  const cases: {
    name: string
    arrange: (workspace: TabWorkspace, tabIDs: TabIDs) => Promise<void>
    wantSelectedKind: string
    wantFirstArea: (tabIDs: TabIDs) => string[]
    wantLast: (workspace: TabWorkspace, tabIDs: TabIDs) => string
  }[] = [
    {
      name: "selected sub agent tab",
      arrange: async (workspace, tabIDs) => {
        await workspace.split(getTabSets(workspace)[0].getId(), "RIGHT")
        // 拖动：子 Agent 标签页移到主对话前面。
        workspace.$layout
          .get()
          .doAction(
            Actions.moveNode(
              tabIDs.subAgent,
              getTabSets(workspace)[0].getId(),
              DockLocation.CENTER,
              0
            )
          )
        workspace.handleLayoutChanged()
      },
      wantSelectedKind: "sub-agent",
      wantFirstArea: (tabIDs) => [tabIDs.subAgent, tabIDs.main],
      wantLast: (workspace) => workspace.$lastConversationTabID.get(),
    },
    {
      // 选中的是终端标签页：最近的对话标签页取布局里第一个主对话、子 Agent 或旁支标签页。
      name: "selected terminal tab",
      arrange: async (workspace) => {
        await workspace.openTerminal()
      },
      wantSelectedKind: "terminal",
      wantFirstArea: (tabIDs) => [tabIDs.main],
      wantLast: (_workspace, tabIDs) => tabIDs.main,
    },
  ]
  for (const each of cases) {
    const { workspace } = createWorkspace()
    const tabIDs = {
      main: workspace.openMain(),
      subAgent: workspace.openSubAgent("s2"),
    }
    await each.arrange(workspace, tabIDs)
    assert.deepEqual(
      getTabOrder(workspace)[0],
      each.wantFirstArea(tabIDs),
      each.name
    )
    assert.equal(
      workspace.$tabs.get()[workspace.$selectedTabID.get()].kind,
      each.wantSelectedKind,
      each.name
    )

    const saved = workspace.toSaved()
    assert.equal(typeof saved.usedAt, "number", each.name)
    const { workspace: restored, connection } = createWorkspace(saved)
    assert.deepEqual(
      restored.$layout.get().toJson(),
      workspace.$layout.get().toJson(),
      each.name
    )
    assert.deepEqual(restored.$tabs.get(), workspace.$tabs.get(), each.name)
    assert.deepEqual(getTabOrder(restored), getTabOrder(workspace), each.name)
    assert.equal(
      restored.$selectedTabID.get(),
      workspace.$selectedTabID.get(),
      each.name
    )
    assert.equal(
      restored.$lastConversationTabID.get(),
      each.wantLast(workspace, tabIDs),
      each.name
    )
    // 主对话和子 Agent 标签页重新订阅，终端标签页不订阅。
    assert.deepEqual(
      connection.getCalls("conversation/open").map((call) => call.params),
      [{ conversation_id: "c1" }, { conversation_id: "c1" }],
      each.name
    )
    assert.ok(restored.getConversationSync(tabIDs.main), each.name)
    assert.ok(restored.getConversationSync(tabIDs.subAgent), each.name)
  }
})

test("restored tab resumes", async () => {
  const cases = [
    {
      name: "saved terminal id attaches to the running terminal",
      terminalID: "t7",
      attaches: [{ terminal_id: "t7", cursor: 0 }],
      phase: "running",
      reason: "",
      isBackgroundWork: true,
    },
    {
      name: "no saved terminal id shows ended",
      terminalID: undefined,
      attaches: [],
      phase: "exited",
      reason: "终端已结束",
      isBackgroundWork: false,
    },
  ]
  for (const each of cases) {
    const tabs: Record<string, TabWorkspaceTab> = {
      main: { kind: "main" },
      terminal: {
        kind: "terminal",
        executorID: "local",
        directory: "/work/repo",
        terminalID: each.terminalID,
      },
    }
    const { workspace, connection } = createWorkspace(createSavedTabs(tabs))
    assert.deepEqual(workspace.$tabs.get(), tabs, each.name)
    assert.equal(workspace.getTabTitle("terminal"), "local · repo", each.name)
    // 刷新后不重新打开终端；有 ID 的接回原来的，没有的显示已结束。
    assert.equal(connection.getCalls("terminal/open").length, 0, each.name)
    assert.equal(
      workspace.getTerminalSession("terminal")?.getSnapshot().phase,
      each.terminalID ? "reconnecting" : "exited",
      each.name
    )
    assert.equal(
      workspace.hasBackgroundWork(),
      each.isBackgroundWork,
      each.name
    )
    await settle()
    assert.deepEqual(
      connection.getCalls("terminal/attach").map((call) => call.params),
      each.attaches,
      each.name
    )
    const snapshot = workspace.getTerminalSession("terminal")!.getSnapshot()
    assert.equal(snapshot.phase, each.phase, each.name)
    assert.equal(snapshot.exitReason, each.reason, each.name)
    // 关掉已结束的终端不会碰连接；还在运行的会结束。
    await workspace.closeTab("terminal")
    await settle()
    assert.deepEqual(
      connection.getCalls("terminal/close").map((call) => call.params),
      each.terminalID ? [{ terminal_id: "t7" }] : [],
      each.name
    )
    assert.equal(workspace.getTerminalSession("terminal"), undefined)
  }
})

test("restores and drops missing side conversation", async () => {
  const cases: {
    name: string
    tabs: Record<string, TabWorkspaceTab>
    missing: string[]
    wantTabs: string[]
    wantNotices: string[]
  }[] = [
    {
      name: "all side conversations still exist",
      tabs: {
        main: { kind: "main" },
        side: createSideTab("side-1"),
      },
      missing: [],
      wantTabs: ["main", "side"],
      wantNotices: [],
    },
    {
      name: "one side conversation is gone",
      tabs: {
        main: { kind: "main" },
        gone: createSideTab("side-gone"),
        side: createSideTab("side-1"),
      },
      missing: ["side-gone"],
      wantTabs: ["main", "side"],
      wantNotices: ["旁支已结束"],
    },
    {
      name: "two are gone and each is announced once",
      tabs: {
        main: { kind: "main" },
        gone1: createSideTab("side-gone-1"),
        gone2: createSideTab("side-gone-2"),
      },
      missing: ["side-gone-1", "side-gone-2"],
      wantTabs: ["main"],
      wantNotices: ["旁支已结束", "旁支已结束"],
    },
  ]
  for (const each of cases) {
    const connection = new FakeConnection()
    for (const id of each.missing) connection.missingConversations.add(id)
    const { workspace, notices } = createWorkspace(
      createSavedTabs(each.tabs),
      connection
    )
    // 恢复时旁支和主对话一样重新订阅，标题照常显示。
    assert.equal(
      connection.getCalls("conversation/open").length,
      Object.keys(each.tabs).length,
      each.name
    )
    for (const [tabID, tab] of Object.entries(each.tabs))
      if (tab.kind === "side-conversation")
        assert.equal(
          workspace.getTabTitle(tabID),
          "旁支 · 为什么用 SQLite",
          each.name
        )
    await settle()
    assert.deepEqual(
      Object.keys(workspace.$tabs.get()),
      each.wantTabs,
      each.name
    )
    assert.deepEqual(notices, each.wantNotices, each.name)
    assert.deepEqual(
      Object.keys(workspace.$tabs.get()).sort(),
      getTabOrder(workspace).flat().sort(),
      each.name
    )
  }
})

test("a restored layout whose only side conversation is gone ends with the main tab", async () => {
  const connection = new FakeConnection()
  connection.missingConversations.add("side-gone")
  const { workspace, notices } = createWorkspace(
    createSavedTabs({ side: createSideTab("side-gone") }),
    connection
  )
  await settle()
  assert.deepEqual(Object.values(workspace.$tabs.get()), [{ kind: "main" }])
  assert.deepEqual(notices, ["旁支已结束"])
})

test("a saved layout that does not match its tabs starts with the main tab", () => {
  const valid = createWorkspace().workspace.toSaved()
  const cases: { name: string; saved: SavedConversationWorkspace }[] = [
    {
      name: "layout without rows",
      saved: { ...valid, layout: { layout: { type: "row", children: [] } } },
    },
    { name: "layout tab has no entry", saved: { ...valid, tabs: {} } },
    {
      name: "entry has no layout tab",
      saved: {
        ...valid,
        tabs: {
          ...valid.tabs,
          extra: { kind: "terminal", executorID: "local", directory: "" },
        },
      },
    },
    { name: "no tabs at all", saved: createSavedTabs({}) },
  ]
  for (const each of cases) {
    const { workspace, connection } = createWorkspace(each.saved)
    assert.deepEqual(
      Object.values(workspace.$tabs.get()),
      [{ kind: "main" }],
      each.name
    )
    const [mainTabID] = Object.keys(workspace.$tabs.get())
    assert.equal(workspace.$selectedTabID.get(), mainTabID, each.name)
    assert.equal(connection.getCalls("conversation/open").length, 1, each.name)
  }
})

test("tab titles follow the tab kind", () => {
  const { workspace } = createWorkspace()
  const mainTabID = workspace.openMain()
  const subAgentTabID = workspace.openSubAgent("s1")
  // 标题还没同步到时，主对话显示未命名，子 Agent 显示加载中。
  assert.equal(workspace.getTabTitle(mainTabID), "新对话")
  assert.equal(workspace.getTabTitle(subAgentTabID), "正在加载…")
  workspace.getConversationSync(mainTabID)?.$title.set("重构登录")
  assert.equal(workspace.getTabTitle(mainTabID), "重构登录")
  // 子 Agent 取任务名，没有任务名时取 Agent 名。
  const subAgentSync = workspace.getConversationSync(subAgentTabID)!
  const cases = [
    {
      name: "task name",
      session: { task_name: "找入口", agent_name: "explorer" },
      want: "找入口",
    },
    {
      name: "agent name",
      session: { agent_name: "explorer" },
      want: "explorer",
    },
  ]
  for (const each of cases) {
    subAgentSync.$sessions.setKey("s1", {
      session: { id: "s1", parent_session_id: "root-c1", ...each.session },
    } as SessionRuntimeInfo)
    assert.equal(workspace.getTabTitle(subAgentTabID), each.want, each.name)
  }
  assert.equal(workspace.getTabTitle("closed"), "")
})

test("terminal opens at the conversation's default executor", async () => {
  const cases: {
    name: string
    settings: ExecutorSettings | undefined
    want: { executorID: string; directory: string }
  }[] = [
    {
      name: "default executor and its directory",
      settings: {
        default_executor_id: "ssh-a",
        workspace_directories: { "ssh-a": "/srv/app", local: "/home/me" },
      },
      want: { executorID: "ssh-a", directory: "/srv/app" },
    },
    {
      name: "empty default executor is local",
      settings: { workspace_directories: { local: "/home/me" } },
      want: { executorID: "local", directory: "/home/me" },
    },
    {
      name: "default executor without a directory",
      settings: {
        default_executor_id: "ssh-a",
        workspace_directories: { local: "/home/me" },
      },
      want: { executorID: "ssh-a", directory: "" },
    },
    {
      name: "conversation record cannot be read",
      settings: undefined,
      want: { executorID: "", directory: "" },
    },
  ]
  for (const each of cases) {
    const { workspace, connection } = createWorkspace()
    if (each.settings) connection.conversationSettings.c1 = each.settings
    const tabID = await workspace.openTerminal()
    assert.deepEqual(
      { ...workspace.$tabs.get()[tabID], terminalID: undefined },
      { kind: "terminal", ...each.want, terminalID: undefined },
      each.name
    )
    assert.deepEqual(
      connection.getCalls("terminal/open").map((call) => call.params),
      [
        {
          executor_id: each.want.executorID,
          directory: each.want.directory,
          rows: 24,
          cols: 80,
          conversation_id: "c1",
        },
      ],
      each.name
    )
    assert.equal(workspace.$selectedTabID.get(), tabID, each.name)
    // 再开一个终端还在同一个位置。
    const secondTabID = await workspace.openTerminal()
    await settle()
    assert.deepEqual(
      workspace.$tabs.get()[secondTabID],
      { ...workspace.$tabs.get()[tabID], terminalID: "t2" },
      each.name
    )
    assert.equal(
      workspace.getTerminalSession(tabID)?.getSnapshot().phase,
      "running",
      each.name
    )
  }
})

test("terminal titles are unique in layout order and follow the shell", async () => {
  const { workspace } = createWorkspace()
  const first = await workspace.openTerminal()
  const second = await workspace.openTerminal()
  await settle()
  assert.deepEqual(
    [workspace.getTabTitle(first), workspace.getTabTitle(second)],
    ["local · default", "local · default (2)"]
  )
  // shell 设了标题就用它，另一个终端不受影响。
  const secondSession = workspace.getTerminalSession(second)!
  secondSession.terminal.write("\x1b]0;vim main.go\x07")
  await new Promise<void>((resolve) =>
    secondSession.terminal.write("", resolve)
  )
  assert.deepEqual(
    [workspace.getTabTitle(first), workspace.getTabTitle(second)],
    ["local · default", "vim main.go"]
  )
  // 重名的第一个关掉后，剩下的恢复原名。
  const third = await workspace.openTerminal()
  await settle()
  assert.equal(workspace.getTabTitle(third), "local · default (2)")
  await workspace.closeTab(first)
  assert.equal(workspace.getTabTitle(third), "local · default")
  assert.deepEqual(Object.keys(workspace.$terminalTitles.get()), [
    second,
    third,
  ])
})

test("closing a terminal tab ends its terminal", async () => {
  const cases = [
    {
      name: "close button",
      close: (workspace: TabWorkspace, tabID: string) =>
        workspace.closeTab(tabID),
    },
    {
      name: "layout removes the tab",
      close: async (workspace: TabWorkspace, tabID: string) => {
        workspace.$layout.get().doAction(Actions.deleteTab(tabID))
        workspace.handleLayoutChanged()
      },
    },
  ]
  for (const each of cases) {
    const { workspace, connection } = createWorkspace()
    const tabID = await workspace.openTerminal()
    await settle()
    await each.close(workspace, tabID)
    await settle()
    assert.deepEqual(
      connection.getCalls("terminal/close").map((call) => call.params),
      [{ terminal_id: "t1" }],
      each.name
    )
    assert.equal(workspace.getTerminalSession(tabID), undefined, each.name)
    assert.equal(workspace.getTabTitle(tabID), "", each.name)
  }
})

test("terminal id is saved after open and after restart", async () => {
  const { workspace, connection } = createWorkspace()
  // 每次通知保存时标签页里的终端 ID。
  const saved: (string | undefined)[][] = []
  workspace.$layoutRevision.listen(() =>
    saved.push(getTerminalTabIDs(workspace))
  )
  const tabID = await workspace.openTerminal()
  await settle()
  assert.deepEqual(saved.at(-1), ["t1"])
  assert.deepEqual(getTerminalTabIDs(workspace), ["t1"])
  // 终端结束后重新启动，换成新的终端 ID。
  const session = workspace.getTerminalSession(tabID)!
  session.markExited("终端已退出，退出码 0")
  workspace.restartTerminal(tabID)
  workspace.restartTerminal("closed")
  await settle()
  assert.equal(connection.getCalls("terminal/open").length, 2)
  assert.deepEqual(saved.at(-1), ["t2"])
  assert.equal(session.getSnapshot().phase, "running")
})

test("a tab without a terminal id restarts in the same place", async () => {
  const tabs: Record<string, TabWorkspaceTab> = {
    main: { kind: "main" },
    terminal: { kind: "terminal", executorID: "ssh-a", directory: "/srv/app" },
  }
  const { workspace, connection } = createWorkspace(createSavedTabs(tabs))
  const session = workspace.getTerminalSession("terminal")!
  assert.equal(session.getSnapshot().phase, "exited")
  workspace.restartTerminal("terminal")
  assert.deepEqual(
    connection.getCalls("terminal/open").map((call) => call.params),
    [
      {
        executor_id: "ssh-a",
        directory: "/srv/app",
        rows: 24,
        cols: 80,
        conversation_id: "c1",
      },
    ]
  )
  await settle()
  assert.equal(session.getSnapshot().phase, "running")
  assert.equal(workspace.getTabTitle("terminal"), "ssh-a · app")
  assert.deepEqual(getTerminalTabIDs(workspace), ["t1"])
})

test("running terminals become tabs", async () => {
  const { workspace, connection, runningTerminals } = createWorkspace()
  const mainTabID = workspace.openMain()
  const cases = [
    { name: "list unknown", list: undefined, want: [] },
    {
      name: "other conversations and no conversation are not added",
      list: [createRunningTerminal("o1", "c2"), createRunningTerminal("o2")],
      want: [],
    },
    {
      name: "this conversation's terminal is added",
      list: [
        createRunningTerminal("t5", "c1"),
        createRunningTerminal("o1", "c2"),
      ],
      want: ["t5"],
    },
    {
      name: "the same terminal again adds no second tab",
      list: [createRunningTerminal("t5", "c1")],
      want: ["t5"],
    },
    {
      name: "a terminal that left the list keeps its tab",
      list: [],
      want: ["t5"],
    },
  ]
  for (const each of cases) {
    runningTerminals.set(each.list)
    await settle()
    assert.deepEqual(getTerminalTabIDs(workspace), each.want, each.name)
    // 自动加的标签页不抢选中，也不会换掉最近的对话标签页。
    assert.equal(workspace.$selectedTabID.get(), mainTabID, each.name)
  }
  // 加在主对话所在的区域，位置取列表里的，并接入终端。
  const terminalTabID = Object.keys(workspace.$tabs.get()).find(
    (tabID) => workspace.$tabs.get()[tabID].kind === "terminal"
  )!
  assert.deepEqual(getTabOrder(workspace), [[mainTabID, terminalTabID]])
  assert.deepEqual(workspace.$tabs.get()[terminalTabID], {
    kind: "terminal",
    executorID: "ssh-a",
    directory: "/srv/app",
    terminalID: "t5",
  })
  assert.deepEqual(
    connection.getCalls("terminal/attach").map((call) => call.params),
    [{ terminal_id: "t5", cursor: 0 }]
  )
  assert.equal(connection.getCalls("terminal/open").length, 0)
  // 释放后不再跟随运行列表。
  workspace.release()
  runningTerminals.set([createRunningTerminal("t6", "c1")])
  assert.deepEqual(getTerminalTabIDs(workspace), ["t5"])
})

test("list before open result adds no tab", async () => {
  const { workspace, connection, runningTerminals } = createWorkspace()
  connection.holdTerminalOpen = true
  await workspace.openTerminal()
  // terminal/open 的回复可能比列表晚到：这时终端还没有对应的标签页，不能再加一个。
  runningTerminals.set([createRunningTerminal("t1", "c1")])
  assert.deepEqual(getTerminalTabIDs(workspace), [undefined])
  connection.releaseReply()
  await settle()
  assert.deepEqual(getTerminalTabIDs(workspace), ["t1"])
  assert.equal(connection.getCalls("terminal/attach").length, 0)
})

test("closed while opening adds no tab", async () => {
  const { workspace, connection, runningTerminals } = createWorkspace()
  connection.holdTerminalOpen = true
  const tabID = await workspace.openTerminal()
  await workspace.closeTab(tabID)
  // 标签页已经关掉，打开请求还在进行：列表里的它不能变成新标签页。
  runningTerminals.set([createRunningTerminal("t1", "c1")])
  connection.releaseReply()
  await settle()
  assert.deepEqual(getTerminalTabIDs(workspace), [])
  // 刚打开的终端被结束。
  assert.deepEqual(
    connection.getCalls("terminal/close").map((call) => call.params),
    [{ terminal_id: "t1" }]
  )
})

test("closing terminal is not added back", async () => {
  const { workspace, connection, runningTerminals } = createWorkspace()
  const tabID = await workspace.openTerminal()
  await settle()
  connection.holdTerminalClose = true
  await workspace.closeTab(tabID)
  // terminal/close 进行中收到的列表仍有这个终端，不加回。
  runningTerminals.set([createRunningTerminal("t1", "c1")])
  assert.deepEqual(getTerminalTabIDs(workspace), [])
  // 请求结束后列表仍有它，说明没有结束，加回成标签页。
  connection.releaseReply()
  await settle()
  assert.deepEqual(getTerminalTabIDs(workspace), [])
  runningTerminals.set([createRunningTerminal("t1", "c1")])
  assert.deepEqual(getTerminalTabIDs(workspace), ["t1"])
})

test("reset terminal asks the serve and reports failures", async () => {
  const cases = [
    { name: "reset succeeds", fails: false, notices: [] },
    { name: "reset fails", fails: true, notices: ["boom"] },
  ]
  for (const each of cases) {
    const { workspace, connection, notices } = createWorkspace()
    const tabID = await workspace.openTerminal()
    await settle()
    if (each.fails) connection.failingMethods.add("terminal/reset")
    workspace.resetTerminal(tabID)
    workspace.resetTerminal("closed")
    await settle()
    assert.deepEqual(
      connection.getCalls("terminal/reset").map((call) => call.params),
      [{ terminal_id: "t1" }],
      each.name
    )
    assert.deepEqual(notices, each.notices, each.name)
  }
})

// 开旁支的三个请求，按发出的顺序
const sideMethods: (keyof MethodParams)[] = [
  "conversation/fork",
  "conversation/appendContextItems",
  "conversation/submitInput",
]

// 返回开旁支用到的请求方法，按发出的顺序
function getSideMethods(connection: FakeConnection): string[] {
  return connection.calls
    .map((call) => call.method)
    .filter((method) => sideMethods.includes(method))
}

test("btw forks, appends instruction, submits question in order", async () => {
  const { workspace, connection } = createWorkspaceWithSource()
  const tabID = await workspace.openSideConversation(" 为什么用 SQLite ")
  const sideCalls = connection.calls.filter((call) =>
    sideMethods.includes(call.method)
  )
  const tab = workspace.$tabs.get()[tabID]
  assert.equal(tab.kind, "side-conversation")
  const boundaryItemID = tab.kind === "side-conversation" && tab.boundaryItemID
  assert.deepEqual(sideCalls, [
    {
      method: "conversation/fork",
      params: {
        conversation_id: "c1",
        executor_settings: sourceSettings,
        ephemeral: true,
      },
    },
    {
      method: "conversation/appendContextItems",
      params: {
        conversation_id: "side-1",
        items: [
          {
            id: boundaryItemID,
            turn_id: "",
            kind: "client_instruction",
            client_instruction: { text: sideConversationInstruction },
          },
        ],
      },
    },
    {
      method: "conversation/submitInput",
      params: {
        conversation_id: "side-1",
        content: [{ kind: "text", text: { text: "为什么用 SQLite" } }],
      },
    },
  ])
  assert.deepEqual(tab, {
    kind: "side-conversation",
    conversationID: "side-1",
    boundaryItemID,
    question: "为什么用 SQLite",
  })
  // 旁支标签页被选中，开始订阅，标题带问题，右栏也跟着它。
  assert.equal(workspace.$selectedTabID.get(), tabID)
  assert.equal(workspace.$lastConversationTabID.get(), tabID)
  assert.ok(workspace.getConversationSync(tabID))
  assert.deepEqual(
    connection.getCalls("conversation/open").map((call) => call.params),
    [{ conversation_id: "c1" }, { conversation_id: "side-1" }]
  )
  assert.equal(workspace.getTabTitle(tabID), "旁支 · 为什么用 SQLite")
})

test("btw without question does not submit", async () => {
  const cases = [
    { name: "empty", question: "" },
    { name: "blank", question: "  \n " },
  ]
  for (const each of cases) {
    const { workspace, connection } = createWorkspaceWithSource()
    const tabID = await workspace.openSideConversation(each.question)
    assert.deepEqual(
      getSideMethods(connection),
      ["conversation/fork", "conversation/appendContextItems"],
      each.name
    )
    const tab = workspace.$tabs.get()[tabID]
    assert.equal(
      tab.kind === "side-conversation" && tab.question,
      "",
      each.name
    )
    assert.equal(workspace.getTabTitle(tabID), "旁支", each.name)
  }
})

test("btw keeps only the first 20 characters of the question in the tab", async () => {
  const { workspace, connection } = createWorkspaceWithSource()
  const question = "这是一个很长很长的问题，" + "长".repeat(30)
  const tabID = await workspace.openSideConversation(question)
  const tab = workspace.$tabs.get()[tabID]
  assert.equal(
    tab.kind === "side-conversation" && tab.question,
    Array.from(question).slice(0, 20).join("")
  )
  // 提交的是完整的问题。
  assert.deepEqual(connection.getCalls("conversation/submitInput")[0].params, {
    conversation_id: "side-1",
    content: [{ kind: "text", text: { text: question } }],
  })
})

test("side conversations and sub agents are placed beside the main tab", async () => {
  type Ids = { main: string; others: string[] }
  const cases: {
    name: string
    isNarrow?: boolean
    // 排好主对话之外的区域，返回主对话标签页和其他标签页
    arrange: (workspace: TabWorkspace, main: string) => Promise<Ids>
    want: (ids: Ids, opened: string) => string[][]
  }[] = [
    {
      name: "no area on the right splits to the right",
      arrange: async (_, main) => ({ main, others: [] }),
      want: ({ main }, opened) => [[main], [opened]],
    },
    {
      name: "area on the right takes the tab",
      arrange: async (workspace, main) => {
        await workspace.split(getTabSets(workspace)[0].getId(), "RIGHT")
        return { main, others: [workspace.$selectedTabID.get()] }
      },
      want: ({ main, others }, opened) => [[main], [...others, opened]],
    },
    {
      name: "area below the main tab does not count",
      arrange: async (workspace, main) => {
        await workspace.split(getTabSets(workspace)[0].getId(), "BOTTOM")
        return { main, others: [workspace.$selectedTabID.get()] }
      },
      want: ({ main, others }, opened) => [[main], [opened], others],
    },
    {
      name: "right column takes it in its first area",
      arrange: async (workspace, main) => {
        await workspace.split(getTabSets(workspace)[0].getId(), "RIGHT")
        const upper = workspace.$selectedTabID.get()
        await workspace.split(getTabSets(workspace)[1].getId(), "BOTTOM")
        return { main, others: [upper, workspace.$selectedTabID.get()] }
      },
      want: ({ main, others }, opened) => [
        [main],
        [others[0], opened],
        [others[1]],
      ],
    },
    {
      name: "main tab in a left column finds the area on the right of the column",
      arrange: async (workspace, main) => {
        const upperLeft = await workspace.openTerminal()
        await workspace.split(getTabSets(workspace)[0].getId(), "RIGHT")
        const right = workspace.$selectedTabID.get()
        const lowerLeft = await workspace.split(
          getTabSets(workspace)[0].getId(),
          "BOTTOM"
        )
        // 把主对话拖进左列下面的区域。
        workspace.$layout
          .get()
          .doAction(
            Actions.moveNode(
              main,
              getTabSets(workspace)[1].getId(),
              DockLocation.CENTER,
              -1
            )
          )
        workspace.handleLayoutChanged()
        return { main, others: [upperLeft, lowerLeft, right] }
      },
      want: ({ main, others }, opened) => [
        [others[0]],
        [others[1], main],
        [others[2], opened],
      ],
    },
    {
      name: "narrow screen opens in the main tab's area",
      isNarrow: true,
      arrange: async (_, main) => ({ main, others: [] }),
      want: ({ main }, opened) => [[main, opened]],
    },
  ]
  const openers = [
    {
      kind: "side-conversation",
      open: (workspace: TabWorkspace, isNarrow?: boolean) =>
        workspace.openSideConversation("问题", isNarrow),
    },
    {
      kind: "sub-agent",
      open: async (workspace: TabWorkspace, isNarrow?: boolean) =>
        workspace.openSubAgent("s1", isNarrow),
    },
  ]
  for (const each of cases)
    for (const opener of openers) {
      const name = each.name + " / " + opener.kind
      const { workspace, mainTabID } = createWorkspaceWithSource()
      const ids = await each.arrange(workspace, mainTabID)
      const openedTabID = await opener.open(workspace, each.isNarrow)
      assert.equal(workspace.$tabs.get()[openedTabID].kind, opener.kind, name)
      assert.deepEqual(
        getTabOrder(workspace),
        each.want(ids, openedTabID),
        name
      )
      assert.equal(workspace.$selectedTabID.get(), openedTabID, name)
    }
})

test("btw fork failure creates no tab", async () => {
  const cases: {
    name: string
    failing: keyof MethodParams
    wantMethods: string[]
  }[] = [
    {
      name: "fork fails",
      failing: "conversation/fork",
      wantMethods: ["conversation/fork"],
    },
    {
      name: "injecting the instruction fails",
      failing: "conversation/appendContextItems",
      wantMethods: ["conversation/fork", "conversation/appendContextItems"],
    },
    {
      name: "submitting the question fails",
      failing: "conversation/submitInput",
      wantMethods: [
        "conversation/fork",
        "conversation/appendContextItems",
        "conversation/submitInput",
      ],
    },
  ]
  for (const each of cases) {
    const { workspace, connection } = createWorkspaceWithSource()
    connection.failingMethods.add(each.failing)
    const tabsBefore = workspace.$tabs.get()
    const layoutBefore = workspace.$layout.get().toJson()
    await assert.rejects(workspace.openSideConversation("问题"), each.name)
    assert.deepEqual(workspace.$tabs.get(), tabsBefore, each.name)
    assert.deepEqual(workspace.$layout.get().toJson(), layoutBefore, each.name)
    assert.deepEqual(getSideMethods(connection), each.wantMethods, each.name)
  }
})

test("closing running side conversation interrupts first", async () => {
  const idle = { session: { id: "root-side-1" } }
  const cases: {
    name: string
    sessions: Record<string, unknown>
    wantInterrupt: boolean
  }[] = [
    {
      name: "side conversation with an active turn",
      sessions: { "root-side-1": { ...idle, active_turn_id: "turn-1" } },
      wantInterrupt: true,
    },
    {
      name: "idle side conversation",
      sessions: { "root-side-1": idle },
      wantInterrupt: false,
    },
    {
      name: "only a sub session is running",
      sessions: {
        "root-side-1": idle,
        child: {
          session: { id: "child", parent_session_id: "root-side-1" },
          active_turn_id: "turn-2",
        },
      },
      wantInterrupt: false,
    },
  ]
  for (const each of cases) {
    const { workspace, connection } = createWorkspaceWithSource([
      "c1",
      "side-1",
    ])
    const sideTabID = await workspace.openSideConversation("问题")
    await settle()
    const sync = workspace.getConversationSync(sideTabID)!
    sync.$sessions.set(each.sessions as Record<string, SessionRuntimeInfo>)
    const before = connection.calls.length
    await workspace.closeTab(sideTabID)
    // 中断在退订之前发出。
    assert.deepEqual(
      connection.calls.slice(before).map((call) => call.method),
      [
        ...(each.wantInterrupt ? ["conversation/interruptTurn"] : []),
        "conversation/unsubscribe",
        "session/unsubscribe",
      ],
      each.name
    )
    if (each.wantInterrupt)
      assert.deepEqual(
        connection.getCalls("conversation/interruptTurn")[0].params,
        { conversation_id: "side-1" }
      )
    assert.equal(workspace.getConversationSync(sideTabID), undefined, each.name)
  }
})
