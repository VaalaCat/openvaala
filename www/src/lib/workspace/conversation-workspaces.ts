import type { QueryClient } from "@tanstack/react-query"
import { atom, type ReadableAtom, type WritableAtom } from "nanostores"
import type { ConversationID, TerminalItem } from "../api/protocol.gen"
import type { ServerConnection } from "../api/server-connection"
import { notifyInfo } from "../notify"
import { TabWorkspace } from "./tab-workspace"
import type {
  SavedConversationWorkspace,
  SavedServerWorkspaces,
  TabWorkspaceTab,
} from "./types"

// 最多保存的对话布局数，超过时淘汰 usedAt 最早的
const maxSavedConversations = 50

// 已经打开的工作区和停止保存它的函数
interface OpenWorkspace {
  workspace: TabWorkspace
  stopSaving: () => void
}

// ConversationWorkspaces 持有一个 serve 的全部对话工作区：哪条对话是当前对话、哪些工作区留在后台。
// 切走时开着旁支或终端的对话留在后台，其余释放；每条对话的布局变化后保存到 storage，刷新后点进去时按保存的布局恢复。
// 保存的布局归 storage 所有，同一个浏览器的多个页面共用，每次都读出最新内容，只改一条对话的那一项。
export class ConversationWorkspaces {
  // 当前对话；在新对话页等没有对话的页面时为空
  readonly $currentConversationID: ReadableAtom<ConversationID | undefined>
  private readonly serverID: string
  private readonly connection: ServerConnection
  private readonly queryClient: QueryClient
  // serve 上运行中的终端列表；未知时为 undefined
  private readonly runningTerminals: ReadableAtom<TerminalItem[] | undefined>
  private readonly storage: Storage
  private readonly notifyInfo: (message: string) => void
  private readonly currentConversationID: WritableAtom<
    ConversationID | undefined
  >
  // 当前对话和留在后台的对话的工作区
  private readonly openWorkspaces = new Map<ConversationID, OpenWorkspace>()

  // notifyInfo 在旁支已经不存在、标签页被去掉时给用户一条提示
  constructor(
    serverID: string,
    connection: ServerConnection,
    queryClient: QueryClient,
    runningTerminals: ReadableAtom<TerminalItem[] | undefined>,
    storage: Storage,
    notifyInfo: (message: string) => void
  ) {
    this.serverID = serverID
    this.connection = connection
    this.queryClient = queryClient
    this.runningTerminals = runningTerminals
    this.storage = storage
    this.notifyInfo = notifyInfo
    this.currentConversationID = atom(undefined)
    this.$currentConversationID = this.currentConversationID
  }

  // 返回这条对话已经打开的工作区（当前对话，或留在后台的）；没有打开时返回 undefined
  getWorkspace(conversationID: ConversationID): TabWorkspace | undefined {
    return this.openWorkspaces.get(conversationID)?.workspace
  }

  // 切到这条对话并返回它的工作区：已经在后台就直接用，否则按 storage 里保存的布局建一个（没有保存就只有主对话）；
  // 离开上一条对话时按规则保留或释放
  open(conversationID: ConversationID): TabWorkspace {
    // 1. 已经是当前对话：直接返回。
    const currentID = this.currentConversationID.get()
    if (currentID === conversationID) return this.getWorkspace(conversationID)!
    // 2. 离开上一条对话。
    this.leaveCurrentConversation()
    // 3. 后台留着的直接用，否则按 storage 里的布局新建，之后每次布局变化都保存。
    let opened = this.openWorkspaces.get(conversationID)
    if (!opened) {
      const workspace = new TabWorkspace(
        conversationID,
        this.connection,
        this.queryClient,
        this.runningTerminals,
        readSavedWorkspaces(this.storage, this.serverID).conversations[
          conversationID
        ],
        this.notifyInfo
      )
      opened = {
        workspace,
        stopSaving: workspace.$layoutRevision.listen(() =>
          this.save(conversationID, workspace)
        ),
      }
      this.openWorkspaces.set(conversationID, opened)
    }
    this.currentConversationID.set(conversationID)
    return opened.workspace
  }

  // 离开当前对话，回到新对话页；离开的规则同 open
  leave(): void {
    this.leaveCurrentConversation()
    this.currentConversationID.set(undefined)
  }

  // 对话被删除：释放它的工作区，删掉保存的布局
  forget(conversationID: ConversationID): void {
    // 1. 释放工作区，它是当前对话时回到没有当前对话。
    const opened = this.openWorkspaces.get(conversationID)
    if (opened) this.releaseWorkspace(conversationID, opened)
    if (this.currentConversationID.get() === conversationID)
      this.currentConversationID.set(undefined)
    // 2. 删掉 storage 里保存的布局，别的对话的布局不动。
    const saved = readSavedWorkspaces(this.storage, this.serverID)
    if (!(conversationID in saved.conversations)) return
    delete saved.conversations[conversationID]
    this.writeSavedWorkspaces(saved)
  }

  // 离开当前对话：开着旁支或终端的工作区留在后台，其余释放
  private leaveCurrentConversation(): void {
    const currentID = this.currentConversationID.get()
    if (currentID === undefined) return
    const opened = this.openWorkspaces.get(currentID)!
    if (opened.workspace.hasBackgroundWork()) return
    this.releaseWorkspace(currentID, opened)
  }

  // 停止保存并释放工作区，从已打开的工作区里移出
  private releaseWorkspace(
    conversationID: ConversationID,
    opened: OpenWorkspace
  ): void {
    opened.stopSaving()
    opened.workspace.release()
    this.openWorkspaces.delete(conversationID)
  }

  // 保存这条对话现在的布局，超过上限时淘汰 usedAt 最早的
  private save(conversationID: ConversationID, workspace: TabWorkspace): void {
    // 1. 读出 storage 里现在的内容，只写入这条对话的布局。
    const saved = readSavedWorkspaces(this.storage, this.serverID)
    saved.conversations[conversationID] = workspace.toSaved()
    // 2. 超过上限时从 usedAt 最早的开始淘汰。
    const evicted = Object.entries(saved.conversations)
      .sort(([, a], [, b]) => b.usedAt - a.usedAt)
      .slice(maxSavedConversations)
    for (const [evictedID] of evicted) delete saved.conversations[evictedID]
    this.writeSavedWorkspaces(saved)
  }

  // 把改过的布局写回 storage；写失败时本次不保存
  private writeSavedWorkspaces(saved: SavedServerWorkspaces): void {
    try {
      this.storage.setItem(
        getWorkspaceStorageKey(this.serverID),
        JSON.stringify(saved)
      )
    } catch {
      // 存储不可用或已满时工作区照常使用，刷新后不恢复
    }
  }
}

const conversationWorkspacesByServer = new Map<string, ConversationWorkspaces>()

// 返回这个 serve 的对话工作区
export function getConversationWorkspaces(
  serverID: string,
  connection: ServerConnection,
  queryClient: QueryClient,
  runningTerminals: ReadableAtom<TerminalItem[] | undefined>
): ConversationWorkspaces {
  let workspaces = conversationWorkspacesByServer.get(serverID)
  if (!workspaces) {
    workspaces = new ConversationWorkspaces(
      serverID,
      connection,
      queryClient,
      runningTerminals,
      getBrowserStorage(),
      notifyInfo
    )
    conversationWorkspacesByServer.set(serverID, workspaces)
  }
  return workspaces
}

// 返回浏览器的 localStorage；被禁用时读取属性就会抛错，改用只在本次页面有效的内存存储
function getBrowserStorage(): Storage {
  try {
    return window.localStorage
  } catch {
    return createMemoryStorage()
  }
}

// 创建只放在内存里的 Storage
export function createMemoryStorage(): Storage {
  const items = new Map<string, string>()
  return {
    get length() {
      return items.size
    },
    clear: () => items.clear(),
    getItem: (key) => items.get(key) ?? null,
    key: (index) => [...items.keys()][index] ?? null,
    removeItem: (key) => {
      items.delete(key)
    },
    setItem: (key, value) => {
      items.set(key, value)
    },
  }
}

// 返回这个 serve 的对话布局在 localStorage 里的 key
export function getWorkspaceStorageKey(serverID: string): string {
  return "openvaala.workspace." + serverID
}

// 读回保存的布局；读不出来、解析不了或内容不是 {conversations: {...}} 时是空的，
// 单条布局的内容不完整时只丢掉这一条
function readSavedWorkspaces(
  storage: Storage,
  serverID: string
): SavedServerWorkspaces {
  // 1. 读出并解析保存的内容。
  let saved: unknown
  try {
    saved = JSON.parse(
      storage.getItem(getWorkspaceStorageKey(serverID)) ?? "null"
    )
  } catch {
    return { conversations: {} }
  }
  if (!isRecord(saved) || !isRecord(saved.conversations))
    return { conversations: {} }
  // 2. 只留下内容完整的布局。
  return {
    conversations: Object.fromEntries(
      Object.entries(saved.conversations).filter(
        (entry): entry is [ConversationID, SavedConversationWorkspace] =>
          isSavedConversationWorkspace(entry[1])
      )
    ),
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

// 解析出来的内容是一条完整的保存布局吗？
function isSavedConversationWorkspace(
  value: unknown
): value is SavedConversationWorkspace {
  return (
    isRecord(value) &&
    isRecord(value.layout) &&
    typeof value.usedAt === "number" &&
    isRecord(value.tabs) &&
    Object.values(value.tabs).every(isTabWorkspaceTab)
  )
}

// 解析出来的内容是一种标签页吗？
function isTabWorkspaceTab(value: unknown): value is TabWorkspaceTab {
  if (!isRecord(value)) return false
  switch (value.kind) {
    case "main":
      return true
    case "sub-agent":
      return typeof value.sessionID === "string"
    case "side-conversation":
      return (
        typeof value.conversationID === "string" &&
        typeof value.boundaryItemID === "string" &&
        typeof value.question === "string"
      )
    case "terminal":
      return (
        typeof value.executorID === "string" &&
        typeof value.directory === "string" &&
        (value.terminalID === undefined || typeof value.terminalID === "string")
      )
    default:
      return false
  }
}
