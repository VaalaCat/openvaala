import type { QueryClient } from "@tanstack/react-query"
import {
  Actions,
  DockLocation,
  Model,
  Orientation,
  TabSetNode,
  type IGlobalAttributes,
  type IJsonTabNode,
  type Node as LayoutNode,
} from "flexlayout-react"
import { atom, type ReadableAtom, type WritableAtom } from "nanostores"
import { i18n } from "../../i18n"
import { ConversationSync } from "../api/conversation-sync"
import {
  ErrorCodeConversationNotFound,
  type ConversationID,
  type SessionID,
  type TerminalItem,
} from "../api/protocol.gen"
import type { ServerConnection } from "../api/server-connection"
import { conversationOptions } from "../query/conversation"
import { getRpcErrorMessage, RpcError } from "../rpc-error"
import { getSessionName } from "../session-tree"
import { getConversationWorkLocation } from "../work-location"
import {
  createItemID,
  getSideConversationQuestionPreview,
  getSideConversationTitle,
  sideConversationInstruction,
} from "./side-conversation"
import { TerminalSession } from "./terminal-session"
import type {
  SavedConversationWorkspace,
  SplitDirection,
  TabWorkspaceTab,
} from "./types"

type SubAgentTab = Extract<TabWorkspaceTab, { kind: "sub-agent" }>
type SideConversationTab = Extract<
  TabWorkspaceTab,
  { kind: "side-conversation" }
>
type TerminalTab = Extract<TabWorkspaceTab, { kind: "terminal" }>

// 子 Agent 或旁支标签页正在运行的同步：stop 是 run 返回的停止函数，
// stopWatchingEnd 取消旁支结束的监听（子 Agent 标签页没有监听）。
interface ConversationTabSync {
  sync: ConversationSync
  stop: () => void
  stopWatchingEnd: () => void
}

// 终端标签页正在运行的终端：stopListening 取消标题变化的订阅
interface TerminalTabSession {
  session: TerminalSession
  stopListening: () => void
}

// 区域最小 220×140；标签页不能改名、浮出、弹出窗口；标签页外面那层容器不滚动，滚动只在消息区和终端里。
const layoutGlobalSettings: IGlobalAttributes = {
  tabSetMinWidth: 220,
  tabSetMinHeight: 140,
  tabSetEnableDeleteWhenEmpty: true,
  tabEnableRename: false,
  tabEnableFloat: false,
  tabEnablePopout: false,
  tabEnableScrollbars: false,
}

// TabWorkspace 持有一条对话的标签页布局和全部标签页的运行对象：主对话、子 Agent、旁支和终端。
// 对话的根 Session 同步由工作区自己持有，主对话标签页显示它，release 才停；主对话标签页不能关掉，
// 页面卸载只停掉界面更新，关掉其他标签页或 release 才停掉它们的订阅。
export class TabWorkspace {
  readonly conversationID: ConversationID
  // flexlayout 的布局；布局操作在这个 Model 上原地进行
  readonly $layout: ReadableAtom<Model>
  readonly $tabs: ReadableAtom<Record<string, TabWorkspaceTab>>
  // 当前区域里选中的标签页
  readonly $selectedTabID: ReadableAtom<string>
  // 最近选中的主对话、子 Agent 或旁支标签页，右栏用；它被关掉后为空
  readonly $lastConversationTabID: ReadableAtom<string>
  // 各终端标签页的标题（标签页 ID → 标题）：shell 设的标题或「Executor · 目录名」，重名的从第二个起加「 (n)」
  readonly $terminalTitles: ReadableAtom<Record<string, string>>
  // 布局或标签页每变化一次加一，ConversationWorkspaces 据此保存
  readonly $layoutRevision: ReadableAtom<number>
  private readonly connection: ServerConnection
  private readonly queryClient: QueryClient
  // serve 上运行中的终端列表；未知时为 undefined
  private readonly runningTerminals: ReadableAtom<TerminalItem[] | undefined>
  private readonly notifyInfo: (message: string) => void
  private readonly model: Model
  private readonly tabs: WritableAtom<Record<string, TabWorkspaceTab>>
  private readonly selectedTabID: WritableAtom<string>
  private readonly lastConversationTabID: WritableAtom<string>
  private readonly terminalTitles: WritableAtom<Record<string, string>>
  private readonly layoutRevision: WritableAtom<number>
  // 根 Session 的同步，独占这条对话的标题和 Session 运行信息的订阅
  private readonly rootSync: ConversationSync
  private stopRootSync: () => void
  // 子 Agent 和旁支标签页的同步
  private readonly conversationSyncs = new Map<string, ConversationTabSync>()
  private readonly terminalSessions = new Map<string, TerminalTabSession>()
  // 进行中的 terminal/open 数量，标签页在等待期间被关掉的也算：回复到达前终端还没有对应的标签页，列表里的它会被误认为没有标签页
  private openingTerminals = 0
  // 标签页已关掉、terminal/close 还在进行的终端：运行列表里的它们不加回成标签页，请求结束就移出，让运行列表决定要不要回来
  private readonly closingTerminals = new Set<TerminalSession>()
  private stopWatchingRunningTerminals: () => void = () => {}
  private isReleased = false

  // 开始根 Session 的同步，再按保存的布局还原：子 Agent 和旁支标签页重新订阅；没有可用的保存内容，或保存的布局里没有主对话时补上主对话。
  // notifyInfo 在旁支已经不存在、标签页被去掉时给用户一条提示
  constructor(
    conversationID: ConversationID,
    connection: ServerConnection,
    queryClient: QueryClient,
    runningTerminals: ReadableAtom<TerminalItem[] | undefined>,
    saved: SavedConversationWorkspace | undefined,
    notifyInfo: (message: string) => void
  ) {
    this.conversationID = conversationID
    this.connection = connection
    this.queryClient = queryClient
    this.runningTerminals = runningTerminals
    this.notifyInfo = notifyInfo
    // 1. 开始根 Session 的同步。
    this.rootSync = new ConversationSync(
      connection,
      queryClient,
      conversationID
    )
    this.stopRootSync = this.rootSync.run()
    // 2. 还原保存的布局和标签页。
    const restored = saved && restoreLayout(saved)
    this.model = restored?.model ?? createLayoutModel()
    this.$layout = atom(this.model)
    this.tabs = atom(restored?.tabs ?? {})
    this.$tabs = this.tabs
    this.selectedTabID = atom("")
    this.$selectedTabID = this.selectedTabID
    this.lastConversationTabID = atom("")
    this.$lastConversationTabID = this.lastConversationTabID
    this.terminalTitles = atom({})
    this.$terminalTitles = this.terminalTitles
    this.layoutRevision = atom(0)
    this.$layoutRevision = this.layoutRevision
    // 3. 子 Agent 和旁支标签页重新订阅，旁支已经不存在时由监听去掉标签页；终端标签页按保存的终端 ID 接回原来的终端，没有 ID 的恢复成已结束。
    for (const [tabID, tab] of Object.entries(this.tabs.get())) {
      if (tab.kind === "terminal" && !tab.terminalID)
        this.createTerminalSession(tabID, tab).markExited(
          i18n.t("workspace.terminal.endedWhenRestored")
        )
      else if (tab.kind === "terminal") this.startTerminalTab(tabID, tab)
      else if (tab.kind !== "main") this.startConversationSync(tabID, tab)
    }
    // 4. 选中的不是对话时，最近的对话标签页取布局里第一个主对话、子 Agent 或旁支标签页。
    this.updateSelectedTabs()
    if (!this.lastConversationTabID.get()) {
      this.model.visitNodes((node) => {
        if (
          !this.lastConversationTabID.get() &&
          node.getType() === "tab" &&
          isConversationTab(this.tabs.get()[node.getId()])
        )
          this.lastConversationTabID.set(node.getId())
      })
    }
    // 5. 保存的布局里没有主对话（没有可用的保存内容，或旧版本关掉过）时，加进当前区域。
    if (!Object.values(this.tabs.get()).some((tab) => tab.kind === "main"))
      this.addTab({ kind: "main" })
    // 6. 本对话运行中、还没有标签页的终端加成标签页。
    this.stopWatchingRunningTerminals = this.runningTerminals.subscribe(() =>
      this.syncRunningTerminals()
    )
  }

  // 选中主对话标签页，返回它的 ID
  openMain(): string {
    const mainTabID = this.getMainTabID()
    this.selectTab(mainTabID)
    return mainTabID
  }

  // 打开子 Agent 的只读标签页：已经开着就选中，否则放在主对话标签页旁边（规则同旁支）
  openSubAgent(sessionID: SessionID, isNarrow = false): string {
    // 1. 已经开着：选中。
    const subAgentTabID = this.findTabID(
      (tab) => tab.kind === "sub-agent" && tab.sessionID === sessionID
    )
    if (subAgentTabID) {
      this.selectTab(subAgentTabID)
      return subAgentTabID
    }
    // 2. 没开：放在主对话标签页旁边。
    return this.addTabBesideMain({ kind: "sub-agent", sessionID }, isNarrow)
  }

  // 在指定区域（默认当前区域）新开一个终端标签页，location 不是中间时分出新区域放它。位置取本对话的默认 Executor 和它的工作目录；
  // 读不到对话记录时都留空，由 serve 用 local 的默认目录
  async openTerminal(
    tabSetID?: string,
    location: DockLocation = DockLocation.CENTER
  ): Promise<string> {
    const { executorID, directory } = await this.getTerminalLocation()
    return this.addTab(
      { kind: "terminal", executorID, directory },
      tabSetID,
      location
    )
  }

  // 从本对话分出一个旁支标签页并返回它的 ID：临时分叉本对话，写入旁支说明，有问题时提交，再放进工作区。
  // 任何一步失败都抛错、不开标签页
  async openSideConversation(
    question: string,
    isNarrow = false
  ): Promise<string> {
    // 1. 按本对话现在的 Executor 设置临时分叉。
    const { conversation: sourceConversation } =
      await this.queryClient.fetchQuery(
        conversationOptions(this.connection, this.conversationID)
      )
    const { conversation: sideConversation } = await this.connection.call(
      "conversation/fork",
      {
        conversation_id: this.conversationID,
        executor_settings: sourceConversation.executor_settings,
        ephemeral: true,
      }
    )
    // 2. 写入旁支说明，它所在的那一轮就是旁支开始显示的位置。
    const boundaryItemID = createItemID()
    await this.connection.call("conversation/appendContextItems", {
      conversation_id: sideConversation.id,
      items: [
        {
          id: boundaryItemID,
          turn_id: "",
          kind: "client_instruction",
          client_instruction: { text: sideConversationInstruction },
        },
      ],
    })
    // 3. 有问题时提交，说明会和它一起写进第一轮。
    const questionText = question.trim()
    if (questionText)
      await this.connection.call("conversation/submitInput", {
        conversation_id: sideConversation.id,
        content: [{ kind: "text", text: { text: questionText } }],
      })
    // 4. 放在主对话标签页旁边。
    return this.addTabBesideMain(
      {
        kind: "side-conversation",
        conversationID: sideConversation.id,
        boundaryItemID,
        question: getSideConversationQuestionPreview(question),
      },
      isNarrow
    )
  }

  // 重新启动已结束的终端标签页：在原来的 Executor 和目录打开新的终端
  restartTerminal(tabID: string): void {
    const running = this.terminalSessions.get(tabID)
    if (running) this.trackTerminalOpen(running.session.restart())
  }

  // 重置运行中的终端标签页：清掉保留的输出，接入这个终端的所有客户端一起清屏；失败时提示原因
  resetTerminal(tabID: string): void {
    void this.terminalSessions
      .get(tabID)
      ?.session.reset()
      .catch((error: unknown) =>
        this.notifyInfo(getRpcErrorMessage(error, i18n.t))
      )
  }

  // 重新开始标签页的同步，同步失败后重试用（主对话重试的是根 Session 的同步）；终端标签页和已关掉的标签页不处理
  restartConversationSync(tabID: string): void {
    if (this.tabs.get()[tabID]?.kind === "main") {
      this.stopRootSync()
      this.stopRootSync = this.rootSync.run()
      return
    }
    const running = this.conversationSyncs.get(tabID)
    if (!running) return
    running.stop()
    running.stop = running.sync.run()
  }

  // 选中标签页，并让它所在的区域成为当前区域
  selectTab(tabID: string): void {
    // 1. 标签页已被关掉：不处理。
    if (!this.tabs.get()[tabID]) return
    // 2. 标签页在别的区域时先还原最大化的区域，否则选中的标签页被藏在后面；选中后更新当前选中的标签页并保存。
    this.restoreMaximizedAreaExcept(
      this.model.getNodeById(tabID)?.getParent()?.getId()
    )
    this.model.doAction(Actions.selectTab(tabID))
    this.handleLayoutChanged()
  }

  // 在指定区域的右边或下边分出一个新区域，里面开一个新终端
  split(tabSetID: string, direction: SplitDirection): Promise<string> {
    return this.openTerminal(tabSetID, DockLocation[direction])
  }

  // 返回区域菜单里最大化一项的动作：区域已经最大化时是还原；工作区只有一个区域时没有这一项
  getAreaMaximizeAction(tabSetID: string): "maximize" | "restore" | undefined {
    // 1. 已经最大化的区域总能还原。
    const tabSet = this.model.getNodeById(tabSetID)
    if (!(tabSet instanceof TabSetNode)) return undefined
    if (tabSet.isMaximized()) return "restore"
    // 2. 其余区域要有别的区域一起才有最大化的意义。
    let areaCount = 0
    this.model.visitNodes((node) => {
      if (node.getType() === "tabset") areaCount++
    })
    return areaCount > 1 ? "maximize" : undefined
  }

  // 最大化指定的区域，已经最大化时还原
  toggleMaximize(tabSetID: string): void {
    this.model.doAction(Actions.maximizeToggle(tabSetID))
    this.handleLayoutChanged()
  }

  // 关掉标签页，停掉它的订阅；区域里最后一个标签页关掉后区域合并
  async closeTab(tabID: string): Promise<void> {
    // 1. 标签页已被关掉：不处理。
    if (!this.tabs.get()[tabID]) return
    // 2. 从布局删除，释放标签页的运行对象。
    this.model.doAction(Actions.deleteTab(tabID))
    this.handleLayoutChanged()
  }

  // 返回根 Session 的同步
  getRootSync(): ConversationSync {
    return this.rootSync
  }

  // 返回区域里选中的标签页；区域不存在或没有标签页时返回 undefined
  getAreaSelectedTabID(tabSetID: string): string | undefined {
    const tabSet = this.model.getNodeById(tabSetID)
    return tabSet instanceof TabSetNode
      ? tabSet.getSelectedNode()?.getId()
      : undefined
  }

  // 返回主对话、子 Agent 或旁支标签页的同步（主对话是根 Session 的同步）；终端标签页和已关掉的标签页没有
  getConversationSync(tabID: string): ConversationSync | undefined {
    if (this.tabs.get()[tabID]?.kind === "main") return this.rootSync
    return this.conversationSyncs.get(tabID)?.sync
  }

  // 返回终端标签页的终端；其他标签页和已关掉的标签页没有
  getTerminalSession(tabID: string): TerminalSession | undefined {
    return this.terminalSessions.get(tabID)?.session
  }

  // 这个标签页能关掉吗？主对话标签页不能
  isTabClosable(tabID: string): boolean {
    return this.model.getNodeById(tabID)?.isCloseable() ?? false
  }

  // 返回标签页上显示的标题
  getTabTitle(tabID: string): string {
    // 1. 主对话取同步到的标题，还没有时显示未命名；子 Agent 取它的名字；旁支和终端按各自的信息组成标题。
    const tab = this.tabs.get()[tabID]
    switch (tab?.kind) {
      case "main":
        return this.rootSync.$title.get() || i18n.t("conversationList.untitled")
      case "sub-agent": {
        const info = this.rootSync.$sessions.get()[tab.sessionID]
        return info
          ? getSessionName(info, i18n.t("conversation.session.root"))
          : i18n.t("common.loading")
      }
      case "side-conversation":
        return getSideConversationTitle(tab.question)
      case "terminal":
        return (
          this.terminalTitles.get()[tabID] ??
          this.terminalSessions.get(tabID)?.session.getSnapshot().title ??
          ""
        )
      default:
        return ""
    }
  }

  // 还开着旁支或没结束的终端吗？有的话离开这条对话后工作区要留在后台；从保存的布局恢复出来、已结束的终端不算
  hasBackgroundWork(): boolean {
    return Object.entries(this.tabs.get()).some(
      ([tabID, tab]) =>
        tab.kind === "side-conversation" ||
        (tab.kind === "terminal" &&
          this.terminalSessions.get(tabID)?.session.getSnapshot().phase !==
            "exited")
    )
  }

  // 返回现在的布局和标签页，usedAt 是此刻
  toSaved(): SavedConversationWorkspace {
    return {
      layout: this.model.toJson(),
      tabs: this.tabs.get(),
      usedAt: Date.now(),
    }
  }

  // 释放全部运行对象：停掉根 Session 的同步和运行中终端的监听，各标签页里旁支还在跑时先中断，再停掉订阅、结束终端；布局和标签页的记录不动
  release(): void {
    this.isReleased = true
    this.stopWatchingRunningTerminals()
    this.stopRootSync()
    for (const tabID of Object.keys(this.tabs.get())) this.releaseTab(tabID)
  }

  // flexlayout 的 onModelChange：释放被关掉或拖走的标签页，更新选中的标签页并通知保存
  handleLayoutChanged(): void {
    // 1. 布局里已经没有的标签页：释放它们的运行对象。
    const tabs = { ...this.tabs.get() }
    const removedTabIDs = Object.keys(tabs).filter(
      (tabID) => !this.model.getNodeById(tabID)
    )
    for (const tabID of removedTabIDs) {
      this.releaseTab(tabID)
      delete tabs[tabID]
    }
    if (removedTabIDs.length > 0) this.tabs.set(tabs)
    // 2. 更新选中的标签页和终端标题，通知保存。
    this.updateSelectedTabs()
    this.updateTerminalTitles()
    this.layoutRevision.set(this.layoutRevision.get() + 1)
  }

  // 登记并订阅新标签页，加进指定区域（默认当前区域）并选中（isSelected 为 false 时不选中，也不还原最大化的区域），返回标签页 ID
  private addTab(
    tab: TabWorkspaceTab,
    tabSetID?: string,
    location: DockLocation = DockLocation.CENTER,
    isSelected = true
  ): string {
    // 1. 子 Agent 和旁支标签页先开始订阅、终端标签页先开始打开或接回终端，再登记标签页；主对话显示根 Session 的同步，不另订阅。
    const tabID = createTabID()
    if (tab.kind === "terminal") this.startTerminalTab(tabID, tab)
    else if (tab.kind !== "main") this.startConversationSync(tabID, tab)
    this.tabs.set({ ...this.tabs.get(), [tabID]: tab })
    // 2. 新标签页不在最大化的区域里（别的区域，或者分栏出来的新区域）时先还原最大化的区域；加进布局并选中。
    const tabJson: IJsonTabNode = {
      type: "tab",
      id: tabID,
      name: this.getTabTitle(tabID),
      component: tab.kind,
      enableClose: tab.kind !== "main",
    }
    const targetTabSetID = this.getTabSetID(tabSetID)
    if (isSelected)
      this.restoreMaximizedAreaExcept(
        location === DockLocation.CENTER ? targetTabSetID : undefined
      )
    this.model.doAction(
      Actions.addTab(tabJson, targetTabSetID, location, -1, isSelected)
    )
    this.handleLayoutChanged()
    return tabID
  }

  // 把子 Agent 或旁支标签页放在主对话标签页旁边：宽屏放进主对话所在区域右边的区域（没有就向右分栏），
  // 窄屏放进主对话所在的区域
  private addTabBesideMain(
    tab: SubAgentTab | SideConversationTab,
    isNarrow: boolean
  ): string {
    // 1. 主对话所在的区域是来源区域。
    const sourceTabSetID = this.getMainTabSetID()
    // 2. 窄屏放进来源区域；宽屏放进来源右边的区域，没有就向右分栏。
    if (isNarrow) return this.addTab(tab, sourceTabSetID)
    const rightTabSetID = this.getRightTabSetID(sourceTabSetID)
    return rightTabSetID
      ? this.addTab(tab, rightTabSetID)
      : this.addTab(tab, sourceTabSetID, DockLocation.RIGHT)
  }

  // 返回主对话标签页的 ID；它不能被关掉，构造时已经保证有
  private getMainTabID(): string {
    const mainTabID = this.findTabID((tab) => tab.kind === "main")
    if (!mainTabID) throw new Error("tab workspace has no main tab")
    return mainTabID
  }

  // 返回主对话标签页所在区域的 ID
  private getMainTabSetID(): string {
    return this.model.getNodeById(this.getMainTabID())!.getParent()!.getId()
  }

  // 返回第一个满足条件的标签页的 ID；没有时返回 undefined
  private findTabID(
    isMatch: (tab: TabWorkspaceTab) => boolean
  ): string | undefined {
    return Object.entries(this.tabs.get()).find(([, tab]) => isMatch(tab))?.[0]
  }

  // 有最大化的区域、而且它不是 visibleTabSetID 时，还原它，别的区域才看得到
  private restoreMaximizedAreaExcept(visibleTabSetID?: string): void {
    const maximized = this.model.getMaximizedTabset()
    if (!maximized || maximized.getId() === visibleTabSetID) return
    this.model.doAction(Actions.maximizeToggle(maximized.getId()))
  }

  // 返回指定的区域；没指定时是当前区域，没有当前区域时是第一个区域
  private getTabSetID(tabSetID?: string): string {
    if (tabSetID) return tabSetID
    const tabSet = this.model.getActiveTabset() ?? this.model.getFirstTabSet()
    if (!tabSet) throw new Error("tab workspace layout has no tab set")
    return tabSet.getId()
  }

  // 开始子 Agent 或旁支标签页的同步：子 Agent 只订阅自己 Session 的内容，对话状态用根 Session 的同步；
  // 旁支是另一条对话，自己订阅全部，同步报对话不存在时去掉这个标签页
  private startConversationSync(
    tabID: string,
    tab: SubAgentTab | SideConversationTab
  ): void {
    const sync =
      tab.kind === "sub-agent"
        ? new ConversationSync(
            this.connection,
            this.queryClient,
            this.conversationID,
            tab.sessionID,
            this.rootSync
          )
        : new ConversationSync(
            this.connection,
            this.queryClient,
            tab.conversationID
          )
    const stopWatchingEnd =
      tab.kind === "side-conversation"
        ? sync.$syncError.listen((error) => {
            if (
              error instanceof RpcError &&
              error.code === ErrorCodeConversationNotFound
            )
              this.handleSideConversationEnded(tabID)
          })
        : () => {}
    this.conversationSyncs.set(tabID, {
      sync,
      stop: sync.run(),
      stopWatchingEnd,
    })
  }

  // 停掉子 Agent 或旁支标签页的同步
  private stopConversationSync(tabID: string): void {
    const running = this.conversationSyncs.get(tabID)
    if (!running) return
    running.stopWatchingEnd()
    running.stop()
    this.conversationSyncs.delete(tabID)
  }

  // 释放一个标签页的运行对象：旁支还在跑时先中断，再停掉订阅，结束终端
  private releaseTab(tabID: string): void {
    this.interruptRunningSideConversation(tabID)
    this.stopConversationSync(tabID)
    this.closeTerminalSession(tabID)
  }

  // 旁支标签页被关掉时，旁支的主 Session 还有活动 Turn 就先中断它；中断失败不处理
  private interruptRunningSideConversation(tabID: string): void {
    // 1. 只处理旁支，而且主 Session 有活动 Turn 才中断。
    const tab = this.tabs.get()[tabID]
    const sync = this.conversationSyncs.get(tabID)?.sync
    if (tab?.kind !== "side-conversation" || !sync) return
    const isRunning = Object.values(sync.$sessions.get()).some(
      (info) => !info.session.parent_session_id && !!info.active_turn_id
    )
    if (!isRunning) return
    // 2. 先发出中断，退订在它之后。
    void this.connection
      .call("conversation/interruptTurn", {
        conversation_id: tab.conversationID,
      })
      .catch(() => undefined)
  }

  // 旁支已经不存在（被 serve 丢弃，或 serve 重启过）：去掉它的标签页并提示一次
  private handleSideConversationEnded(tabID: string): void {
    if (!this.tabs.get()[tabID]) return
    void this.closeTab(tabID)
    this.notifyInfo(i18n.t("workspace.sideConversationEnded"))
  }

  // 返回指定区域右边的第一个区域；右边没有区域时返回 undefined
  private getRightTabSetID(tabSetID: string): string | undefined {
    // 1. 从区域往上走，横向排列的行里它（或它的上级）右边还有兄弟时，取那个兄弟里最靠左上的区域。
    let node = this.model.getNodeById(tabSetID)
    let parent = node?.getParent()
    while (node && parent) {
      if (parent.getOrientation() === Orientation.HORZ) {
        const siblings = parent.getChildren()
        const next = siblings[siblings.indexOf(node) + 1]
        if (next) return getFirstTabSetID(next)
      }
      node = parent
      parent = parent.getParent()
    }
    return undefined
  }

  // 创建终端标签页的终端（还没有向 serve 打开或接入），标题变化时更新终端标题，终端 ID 变化时保存
  private createTerminalSession(
    tabID: string,
    tab: TerminalTab
  ): TerminalSession {
    const session = new TerminalSession(
      this.connection,
      this.conversationID,
      tab.executorID,
      tab.directory
    )
    this.terminalSessions.set(tabID, {
      session,
      stopListening: session.subscribe(() => {
        this.saveTerminalID(tabID, session)
        this.updateTerminalTitles()
      }),
    })
    return session
  }

  // 开始终端标签页的终端：标签页有终端 ID 时接回 serve 上的终端，否则向 serve 打开新终端
  private startTerminalTab(tabID: string, tab: TerminalTab): void {
    const session = this.createTerminalSession(tabID, tab)
    if (tab.terminalID) session.resume(tab.terminalID)
    else this.trackTerminalOpen(session.start())
  }

  // 记下一个 terminal/open 在进行；处理完后重新同步运行中的终端
  private trackTerminalOpen(opening: Promise<void>): void {
    this.openingTerminals++
    void opening.finally(() => {
      this.openingTerminals--
      this.syncRunningTerminals()
    })
  }

  // 终端打开成功或重新启动后，把新的终端 ID 写进标签页并通知保存
  private saveTerminalID(tabID: string, session: TerminalSession): void {
    const tab = this.tabs.get()[tabID]
    const terminalID = session.getTerminalID()
    if (
      tab?.kind !== "terminal" ||
      !terminalID ||
      tab.terminalID === terminalID
    )
      return
    this.tabs.set({ ...this.tabs.get(), [tabID]: { ...tab, terminalID } })
    this.layoutRevision.set(this.layoutRevision.get() + 1)
  }

  // 把本对话运行中、还没有标签页的终端加成标签页并接入，加在主对话所在的区域，不抢当前选中；
  // 还有终端在 opening 时跳过：terminal/open 的回复可能比列表晚到，这时它还没有标签页，打开完成后重新执行
  private syncRunningTerminals(): void {
    // 1. 列表未知、工作区已释放或有终端在打开时不动标签页。
    const terminals = this.runningTerminals.get()
    if (this.isReleased || !terminals || this.openingTerminals > 0) return
    // 2. 本对话的终端，没有标签页、也不在关闭中的，加成标签页。
    const knownTerminalIDs = new Set<string>()
    for (const { session } of this.terminalSessions.values())
      knownTerminalIDs.add(session.getTerminalID())
    for (const session of this.closingTerminals)
      knownTerminalIDs.add(session.getTerminalID())
    for (const item of terminals) {
      if (
        item.conversation_id !== this.conversationID ||
        knownTerminalIDs.has(item.terminal_id)
      )
        continue
      this.addTab(
        {
          kind: "terminal",
          executorID: item.executor_id,
          directory: item.directory,
          terminalID: item.terminal_id,
        },
        this.getMainTabSetID(),
        DockLocation.CENTER,
        false
      )
    }
  }

  // 关掉终端标签页的终端：运行中的终端在 serve 上结束，请求结束前它在运行列表里不加回成标签页
  private closeTerminalSession(tabID: string): void {
    const running = this.terminalSessions.get(tabID)
    if (!running) return
    running.stopListening()
    this.terminalSessions.delete(tabID)
    this.closingTerminals.add(running.session)
    void running.session
      .close()
      .finally(() => this.closingTerminals.delete(running.session))
  }

  // 返回新终端的位置：本对话的默认 Executor 和它的工作目录，同桌面端的 getTerminalLocation；
  // 读不到对话记录时都留空
  private async getTerminalLocation(): Promise<{
    executorID: string
    directory: string
  }> {
    // 1. 读对话记录，取默认 Executor（为空时是 local）和它在这个对话里的目录。
    try {
      const { conversation } = await this.queryClient.fetchQuery(
        conversationOptions(this.connection, this.conversationID)
      )
      return getConversationWorkLocation(conversation)
    } catch {
      return { executorID: "", directory: "" }
    }
  }

  // 按布局顺序重算各终端标签页的标题，重名的从第二个起加「 (n)」
  private updateTerminalTitles(): void {
    // 1. 布局里的终端标签页，按区域顺序、区域内的标签页顺序。
    const tabIDs: string[] = []
    this.model.visitNodes((node) => {
      if (node.getType() === "tab" && this.terminalSessions.has(node.getId()))
        tabIDs.push(node.getId())
    })
    // 2. 标题没变时不通知。
    const titles = getUniqueTitles(
      tabIDs.map(
        (tabID) => this.terminalSessions.get(tabID)!.session.getSnapshot().title
      )
    )
    const next = Object.fromEntries(
      tabIDs.map((tabID, index) => [tabID, titles[index]])
    )
    const current = this.terminalTitles.get()
    const isUnchanged =
      Object.keys(next).length === Object.keys(current).length &&
      Object.entries(next).every(([tabID, title]) => current[tabID] === title)
    if (!isUnchanged) this.terminalTitles.set(next)
  }

  // 按布局更新选中的标签页和最近选中的对话标签页
  private updateSelectedTabs(): void {
    // 1. 选中的是当前区域里选中的那个标签页。
    const tabSet = this.model.getActiveTabset() ?? this.model.getFirstTabSet()
    const selectedTabID = tabSet?.getSelectedNode()?.getId() ?? ""
    this.selectedTabID.set(selectedTabID)
    // 2. 选中主对话、子 Agent 或旁支时记下；选中终端时保留，除非它已被关掉。
    const tabs = this.tabs.get()
    if (isConversationTab(tabs[selectedTabID]))
      this.lastConversationTabID.set(selectedTabID)
    else if (!tabs[this.lastConversationTabID.get()])
      this.lastConversationTabID.set("")
  }
}

// 只含一个空区域的布局
function createLayoutModel(): Model {
  return Model.fromJson({
    global: layoutGlobalSettings,
    layout: { type: "row", children: [{ type: "tabset", children: [] }] },
  })
}

// 返回节点里最靠左上的区域的 ID
function getFirstTabSetID(node: LayoutNode): string {
  return node.getType() === "tabset"
    ? node.getId()
    : getFirstTabSetID(node.getChildren()[0])
}

// 这是主对话、子 Agent 或旁支标签页吗？
function isConversationTab(tab: TabWorkspaceTab | undefined): boolean {
  return (
    tab?.kind === "main" ||
    tab?.kind === "sub-agent" ||
    tab?.kind === "side-conversation"
  )
}

// 返回与 titles 等长的标题：第 n 个重复的标题追加「 (n)」，第一个保持原样；已经带后缀的标题也占位，结果互不相同
function getUniqueTitles(titles: string[]): string[] {
  const used = new Set<string>()
  return titles.map((title) => {
    let candidate = title
    for (let number = 2; used.has(candidate); number++)
      candidate = `${title} (${number})`
    used.add(candidate)
    return candidate
  })
}

// 创建新的标签页 ID；页面不一定在安全上下文里，不用 crypto.randomUUID
function createTabID(): string {
  return "tab-" + Math.random().toString(36).slice(2, 10)
}

// 还原保存的布局；布局读不出来或和标签页对不上时返回 undefined
function restoreLayout(
  saved: SavedConversationWorkspace
): { model: Model; tabs: Record<string, TabWorkspaceTab> } | undefined {
  // 1. 还原布局，全局设置以当前版本为准。
  let model: Model
  try {
    model = Model.fromJson({ ...saved.layout, global: layoutGlobalSettings })
  } catch {
    return undefined
  }
  if (!model.getFirstTabSet()) return undefined
  // 2. 布局里的标签页要和保存的标签页一一对应。
  const layoutTabIDs: string[] = []
  model.visitNodes((node) => {
    if (node.getType() === "tab") layoutTabIDs.push(node.getId())
  })
  const savedTabIDs = Object.keys(saved.tabs)
  if (
    layoutTabIDs.length !== savedTabIDs.length ||
    !savedTabIDs.every((tabID) => layoutTabIDs.includes(tabID))
  )
    return undefined
  // 3. 旧版本保存的布局里主对话标签页可以关，一律改成不能关。
  for (const [tabID, tab] of Object.entries(saved.tabs))
    if (tab.kind === "main")
      model.doAction(
        Actions.updateNodeAttributes(tabID, { enableClose: false })
      )
  return { model, tabs: saved.tabs }
}
