import type { QueryClient } from "@tanstack/react-query"
import {
  atom,
  listenKeys,
  map,
  type MapStore,
  type WritableAtom,
} from "nanostores"
import { recordConversationResult } from "../conversation-results"
import { invalidateForNotification } from "../query/invalidation"
import { RpcError, rpcConnectionLostCode } from "../rpc-error"
import {
  applyItemDelta,
  findStreamingItemIndex,
  type TurnView,
  type ViewItem,
} from "./conversation-items"
import type {
  AgentToolProgress,
  Content,
  ConversationID,
  Item,
  ItemEventParams,
  LoggedItem,
  MethodParams,
  MethodResult,
  NotificationTarget,
  Ordinal,
  SessionID,
  SessionInputMode,
  SessionRuntimeInfo,
  SessionPendingInputs,
  StreamingItem,
  TurnFinishedParams,
  TurnID,
} from "./protocol.gen"
import type { ServerConnection, ServerNotification } from "./server-connection"

// SyncConnection 是 ConversationSync 用到的连接能力，测试用脚本实现替换
export type SyncConnection = Pick<
  ServerConnection,
  "call" | "listen" | "onConnected" | "getServerID"
>

export type SyncState = "syncing" | "synced" | "failed"

// ConversationNotification 是带对话和订阅种类的通知；listen 只交付带 conversation_id 的通知，clientMCP/* 不会到这里
type ConversationNotification = Extract<
  ServerNotification,
  { params: NotificationTarget }
>

// SyncResult 是一次同步读到的内容，全部读完后一起写进 store
interface SyncResult {
  // 对话的标题和全部 Session 运行信息；只读 Session 内容的同步（有 rootSync）不订阅对话状态，没有这一项
  conversationStatus?: { title: string; sessions: SessionRuntimeInfo[] }
  lastSavedOrdinal: Ordinal
  streamingItems: StreamingItem[]
  pendingInputs: SessionPendingInputs
  // 按流水号升序，不超过 lastSavedOrdinal
  savedItems: LoggedItem[]
  // 首次同步读最近一页时 serve 返回的下一页游标
  earlierItemsCursor?: Ordinal
}

const historyPageSize = 50
// 执行中的终端输出最多保留的字符数，超出时丢掉最早的部分；与着色上限一致
const maxToolProgressLength = 200 * 1024

// ConversationSync 跟随一个对话里的一个 Session：打开并订阅对话，补齐已保存的 Item，把推送写进 store；
// 断线重连或订阅被 serve 关闭后重新同步，从已交付的位置继续，不缺不重。照 internal/runtimeclient 的同步规则。
export class ConversationSync {
  readonly $title: WritableAtom<string>
  readonly $sessions: MapStore<Record<SessionID, SessionRuntimeInfo>>
  // 已保存的和生成中的 Item，按首次出现顺序
  readonly $items = atom<ViewItem[]>([])
  readonly $turns = map<Record<TurnID, TurnView>>({})
  readonly $hasEarlierItems = atom(false)
  readonly $syncState = atom<SyncState>("syncing")
  // 这条对话的根 Session，第一次打开对话之后才知道
  readonly $rootSessionID = atom<SessionID | undefined>(undefined)
  // 最近一次同步被 serve 拒绝的错误，例如对话不存在；同步成功后清空
  readonly $syncError = atom<unknown>(undefined)
  // 服务端尚未交给 Turn 的用户输入列表。
  readonly $pendingInputs = atom<SessionPendingInputs>({ inputs: [] })
  // 工具执行中、结果还没保存时的输出，key 是工具调用的 call_id；终端输出按到达顺序接在一起
  readonly $toolProgress = map<Record<string, AgentToolProgress>>({})
  readonly conversationID: ConversationID
  readonly serverID: string
  private readonly connection: SyncConnection
  private readonly queryClient: QueryClient
  // 这条对话根 Session 的同步，见构造函数
  private readonly rootSync: ConversationSync | undefined
  // 跟随的 Session：构造时指定，没指定时取第一次 conversation/open 返回的根 Session
  private followedSessionID: SessionID | undefined
  // 已交付的最后一个已保存 Item 的流水号：items/saved 按它去重，重新同步从它往后补
  private lastDeliveredOrdinal: Ordinal = 0
  // 是否完成过一次同步？完成后重新同步只补齐，不再读最近一页
  private hasSynced = false
  // 加载更早 Item 的游标：最近一页和每次向前加载时 serve 返回的 next_cursor
  private earlierItemsCursor: Ordinal | undefined
  private earlierItemsLoading: Promise<void> | undefined
  // 每次开始同步或停止都加一，让过期同步的请求不再发出、结果不再写入
  private syncGeneration = 0
  // 同步期间到达的通知，按到达顺序，同步完成后交付
  private stagedNotifications: ConversationNotification[] = []
  // 实时新增且尚未经过消息列展示的 Item；折叠成员也在展示提交后消耗。
  private readonly unpresentedLiveItemKeys = new Set<string>()

  // rootSync 是这条对话根 Session 的同步，它独占对话的标题和 Session 运行信息的订阅；
  // 给了它，这个同步只订阅自己 Session 的内容，不调用 conversation/subscribe 和 conversation/unsubscribe，
  // $title 和 $sessions 就是 rootSync 的，对话订阅的通知也只由 rootSync 处理
  constructor(
    connection: SyncConnection,
    queryClient: QueryClient,
    conversationID: ConversationID,
    sessionID?: SessionID,
    rootSync?: ConversationSync
  ) {
    this.connection = connection
    this.queryClient = queryClient
    this.conversationID = conversationID
    this.serverID = connection.getServerID()
    this.followedSessionID = sessionID
    this.rootSync = rootSync
    this.$title = rootSync?.$title ?? atom("")
    this.$sessions =
      rootSync?.$sessions ?? map<Record<SessionID, SessionRuntimeInfo>>({})
  }

  // 开始同步并跟随推送；返回退订并停止的函数。停止后可以再次 run，从已交付的位置继续
  run(): () => void {
    // 1. 登记这个对话的通知和重连处理，开始同步。
    const stopListening = this.connection.listen(
      this.conversationID,
      (notification) =>
        this.handleNotification(notification as ConversationNotification)
    )
    const stopFollowingReconnect = this.connection.onConnected(() =>
      this.startSync()
    )
    // 有 rootSync 时，对话订阅的通知不在这里处理，跟随的 Session 的运行信息变了就按 rootSync 的更新 Turn 状态。
    const stopFollowingRootSession =
      this.rootSync &&
      listenKeys(
        this.rootSync.$sessions,
        [this.followedSessionID!],
        (sessions) =>
          this.applyFollowedSessionTurns(sessions[this.followedSessionID!])
      )
    this.startSync()
    // 2. 停止：作废进行中的同步，取消登记，退订跟随的两个订阅；连接已断开时退订失败不处理。
    return () => {
      this.syncGeneration++
      this.stagedNotifications = []
      stopListening()
      stopFollowingReconnect()
      stopFollowingRootSession?.()
      const sessionID = this.followedSessionID
      if (sessionID === undefined) return
      void Promise.all([
        ...(this.rootSync
          ? []
          : [
              this.connection.call("conversation/unsubscribe", {
                conversation_id: this.conversationID,
              }),
            ]),
        this.connection.call("session/unsubscribe", {
          conversation_id: this.conversationID,
          session_id: sessionID,
        }),
      ]).catch(() => undefined)
    }
  }

  // 向前加载一页更早的 Item（session/items/list，desc，从上一页的 next_cursor 往前）；加载中再调用时等同一次
  loadEarlierItems(): Promise<void> {
    if (this.earlierItemsLoading) return this.earlierItemsLoading
    const cursor = this.earlierItemsCursor
    if (cursor === undefined) return Promise.resolve()
    this.earlierItemsLoading = this.prependEarlierItems(cursor).finally(() => {
      this.earlierItemsLoading = undefined
    })
    return this.earlierItemsLoading
  }

  // 提交输入；队列只由服务端订阅更新。
  async submitInput(content: Content[], mode: SessionInputMode): Promise<void> {
    await this.connection.call("conversation/submitInput", {
      conversation_id: this.conversationID,
      mode,
      content,
    })
  }

  // 将排队用户输入移入指定轮。
  async steerInput(itemID: string, turnID: TurnID): Promise<void> {
    await this.connection.call("conversation/steerInput", {
      conversation_id: this.conversationID,
      item_id: itemID,
      turn_id: turnID,
    })
  }

  // 将排队用户输入置顶并取消指定轮。
  async interruptTurnWithInput(turnID: TurnID, itemID: string): Promise<void> {
    await this.connection.call("conversation/interruptTurnWithInput", {
      conversation_id: this.conversationID,
      item_id: itemID,
      turn_id: turnID,
    })
  }

  // 取回或撤回输入；成功后仍等待服务端列表更新。
  async withdrawInput(
    itemID: string,
    returnContent: boolean
  ): Promise<Content[] | undefined> {
    const result = await this.connection.call("conversation/withdrawInput", {
      conversation_id: this.conversationID,
      item_id: itemID,
      return_content: returnContent,
    })
    return result.content?.filter(
      (content): content is Content => content !== null
    )
  }

  // 跟随的 Session；没指定时第一次打开对话之后才知道，之前调用直接抛错
  getFollowedSessionID(): SessionID {
    if (this.followedSessionID === undefined)
      throw new Error("conversation is not opened yet")
    return this.followedSessionID
  }

  // 这个目标是否已完成过首次同步？
  hasInitialData(): boolean {
    // 1. 首次数据归这一 Sync 实例保存。
    return this.hasSynced
  }

  // 这个 Item 是否由实时通知新增，且尚未经过消息列展示？
  isUnpresentedLiveItem(key: string): boolean {
    // 1. 只读取这一 Sync 的展示记录。
    return this.unpresentedLiveItemKeys.has(key)
  }

  // 消息列提交后消耗本次展示的 Item，包含折叠组内成员。
  markItemsPresented(items: readonly ViewItem[]): void {
    // 1. 展示记录不触发业务 store 更新。
    for (const view of items) this.unpresentedLiveItemKeys.delete(view.key)
  }

  // 作废上一次同步，开始暂存通知并同步；完成后写入结果，失败时断线等重连、其他错误标记失败
  private startSync(): void {
    const generation = ++this.syncGeneration
    this.stagedNotifications = []
    this.unpresentedLiveItemKeys.clear()
    this.$syncError.set(undefined)
    this.$syncState.set("syncing")
    void this.syncConversation(generation).then(
      (result) => this.handleSyncFinished(generation, result),
      (error: unknown) => this.handleSyncFailed(generation, error)
    )
  }

  // 退订旧订阅、打开对话、重新订阅、读已保存 Item，返回读到的内容；同 internal/runtimeclient 的 syncConversation
  private async syncConversation(generation: number): Promise<SyncResult> {
    const isResync = this.hasSynced
    // 1. 重新同步时，先退订自己上一次的两个订阅。
    if (isResync)
      await this.unsubscribeForSync(generation, this.getFollowedSessionID())
    // 2. 打开对话；没有指定 Session 时跟随根 Session。
    const opened = await this.callForSync(generation, "conversation/open", {
      conversation_id: this.conversationID,
    })
    this.$rootSessionID.set(opened.conversation.root_session_id)
    this.followedSessionID ??= opened.conversation.root_session_id
    const sessionID = this.followedSessionID
    // 3. 为防止残留，再退订一次目标对话和跟随 Session 的订阅；同步期间的 subscription/closed 由 handleNotification 处理。
    await this.unsubscribeForSync(generation, sessionID)
    // 4. 订阅对话状态：标题和全部 Session 运行信息；有 rootSync 时由它订阅，这里等它第一次同步完成。
    let conversationStatus: SyncResult["conversationStatus"]
    if (this.rootSync) await this.waitForRootSync(generation)
    else {
      const status = await this.callForSync(
        generation,
        "conversation/subscribe",
        { conversation_id: this.conversationID }
      )
      conversationStatus = { title: status.title, sessions: status.sessions }
    }
    // 5. 订阅 Session 内容：已保存位置和生成中 Item。
    const content = await this.callForSync(generation, "session/subscribe", {
      conversation_id: this.conversationID,
      session_id: sessionID,
    })
    const result = {
      conversationStatus,
      lastSavedOrdinal: content.last_saved_ordinal,
      streamingItems: content.streaming_items,
      pendingInputs: content.pending_inputs,
    }
    // 6. 重新同步从已交付的位置往后按 asc 补齐到 last_saved_ordinal。
    if (isResync) {
      const savedItems = await this.listItemsBetween(
        generation,
        sessionID,
        this.lastDeliveredOrdinal,
        content.last_saved_ordinal
      )
      return { ...result, savedItems }
    }
    // 7. 首次同步按 desc 读最近一页，订阅之后才保存的留给暂存的 items/saved。
    const page = await this.callForSync(generation, "session/items/list", {
      conversation_id: this.conversationID,
      session_id: sessionID,
      order: "desc",
      limit: historyPageSize,
    })
    return {
      ...result,
      savedItems: page.items
        .filter((logged) => logged.ordinal <= content.last_saved_ordinal)
        .reverse(),
      earlierItemsCursor: page.next_cursor,
    }
  }

  // 按流水号正序读 from 之后、到 to 为止的已保存 Item；同 internal/runtimeclient 的 listItemsBetween，是唯一循环读完的地方
  private async listItemsBetween(
    generation: number,
    sessionID: SessionID,
    from: Ordinal,
    to: Ordinal
  ): Promise<LoggedItem[]> {
    const items: LoggedItem[] = []
    for (let cursor = from; cursor < to;) {
      const page = await this.callForSync(generation, "session/items/list", {
        conversation_id: this.conversationID,
        session_id: sessionID,
        order: "asc",
        cursor,
        limit: historyPageSize,
      })
      // 空页时结束；按顺序收集不超过终点的 Item 并推进游标。
      if (page.items.length === 0) break
      for (const logged of page.items) {
        if (logged.ordinal > to) return items
        items.push(logged)
        cursor = logged.ordinal
      }
    }
    return items
  }

  // 同步完成：写入标题、Session 和 Turn 状态，应用已保存与生成中的 Item，再按到达顺序交付暂存的通知
  private handleSyncFinished(generation: number, result: SyncResult): void {
    // 1. 忽略过期的结果。
    if (generation !== this.syncGeneration) return
    // 2. 写入标题、全部 Session 运行信息和跟随 Session 的 Turn 状态；
    //    记为 running 但已不在执行的轮次（断开期间结束的）清掉，改看 Turn 记录。
    if (result.conversationStatus) {
      const { title, sessions } = result.conversationStatus
      this.$title.set(title)
      this.$sessions.set(
        Object.fromEntries(sessions.map((info) => [info.session.id, info]))
      )
      for (const info of sessions)
        recordConversationResult(
          this.serverID,
          this.conversationID,
          this.$rootSessionID.get()!,
          info
        )
    }
    this.applyFollowedSessionTurns(
      this.$sessions.get()[this.followedSessionID!]
    )
    // 3. 首次同步记下向前加载的游标；应用已保存 Item，再用生成中 Item 替换未保存的。
    if (!this.hasSynced) {
      this.earlierItemsCursor = result.earlierItemsCursor
      this.$hasEarlierItems.set(result.earlierItemsCursor !== undefined)
    }
    this.applySavedItems(result.savedItems)
    this.lastDeliveredOrdinal = result.lastSavedOrdinal
    this.applyStreamingItems(result.streamingItems)
    this.$pendingInputs.set(result.pendingInputs)
    // 4. 结束同步，按到达顺序交付暂存的通知。
    this.hasSynced = true
    this.$syncError.set(undefined)
    this.$syncState.set("synced")
    const staged = this.stagedNotifications
    this.stagedNotifications = []
    for (const notification of staged)
      this.deliverNotification(notification, false)
  }

  // 按跟随的 Session 的运行信息更新 Turn 状态：记为 running 但已不在执行的轮次（断开期间结束的）清掉，
  // 最近一轮记下终态，正在执行的一轮记为 running
  private applyFollowedSessionTurns(followed: SessionRuntimeInfo | undefined) {
    for (const [turnID, turn] of Object.entries(this.$turns.get()))
      if (turn.status === "running" && turnID !== followed?.active_turn_id)
        this.$turns.setKey(turnID, undefined)
    if (followed?.last_turn_id)
      this.$turns.setKey(followed.last_turn_id, {
        ...this.$turns.get()[followed.last_turn_id],
        status: followed.last_turn_status,
      })
    if (followed?.active_turn_id)
      this.$turns.setKey(followed.active_turn_id, { status: "running" })
  }

  // 同步失败：过期的忽略；连接已断开时保持同步中，等 onConnected 重新同步；serve 拒绝时标记失败
  private handleSyncFailed(generation: number, error: unknown): void {
    if (generation !== this.syncGeneration) return
    if (error instanceof RpcError && error.code === rpcConnectionLostCode)
      return
    this.stagedNotifications = []
    this.$syncError.set(error)
    this.$syncState.set("failed")
  }

  // 收到这个对话的通知：其他 Session 的内容通知丢弃；同步期间暂存，期间的 subscription/closed 只删掉同一种订阅暂存的通知；其余立即交付
  private handleNotification(notification: ConversationNotification): void {
    // 1. 有 rootSync 时对话订阅的通知归它处理；Session 订阅的通知只处理跟随的 Session，
    //    对话订阅的通知属于对话里的任何 Session，不看 session_id。
    const { params } = notification
    if (this.rootSync && params.subscription !== "session") return
    if (
      params.subscription === "session" &&
      params.session_id !== this.followedSessionID
    )
      return
    // 2. 同步期间暂存；subscription/closed 是同步时退订引起的，删掉同一种订阅暂存的通知，不重新同步。
    if (this.$syncState.get() === "syncing") {
      if (notification.method === "subscription/closed")
        this.stagedNotifications = this.stagedNotifications.filter(
          (staged) => staged.params.subscription !== params.subscription
        )
      else this.stagedNotifications.push(notification)
      return
    }
    // 3. 不在同步中时立即交付。
    this.deliverNotification(notification, true)
  }

  // 交付一条通知：订阅被关闭时重新同步，其余按通知更新 store，再让受影响的查询失效
  private deliverNotification(
    notification: ConversationNotification,
    isLive: boolean
  ): void {
    // 1. 订阅被 serve 关闭时重新同步。
    if (notification.method === "subscription/closed") {
      this.startSync()
      return
    }
    // 2. 按通知更新对话内容和状态。
    switch (notification.method) {
      case "item/started":
        if (notification.params.item)
          this.putStreamingItem(
            notification.params.item_index,
            notification.params.item,
            true,
            isLive
          )
        break
      case "item/delta":
        this.handleItemDelta(notification.params)
        break
      case "item/completed":
      case "item/interrupted":
        this.handleItemFinished(notification.params, isLive)
        break
      case "item/toolProgress":
        if (notification.params.tool_progress)
          this.handleToolProgress(notification.params.tool_progress)
        break
      case "items/saved":
        this.applySavedItems(notification.params.items, isLive)
        break
      case "turn/started":
        this.$turns.setKey(notification.params.turn_id, { status: "running" })
        break
      case "turn/finished":
        this.handleTurnFinished(notification.params)
        break
      case "session/pendingInputsUpdated":
        this.$pendingInputs.set(notification.params.pending_inputs)
        break
      case "session/statusUpdated":
        recordConversationResult(
          this.serverID,
          this.conversationID,
          this.$rootSessionID.get()!,
          notification.params.info
        )
        this.$sessions.setKey(
          notification.params.info.session.id,
          notification.params.info
        )
        // Turn 可能在两种订阅建立之间结束；对话通知里的终态也要消除同步快照残留的 running。
        if (
          notification.params.info.session.id === this.followedSessionID &&
          notification.params.info.last_turn_id
        ) {
          const { last_turn_id, last_turn_status } = notification.params.info
          this.$turns.setKey(last_turn_id, {
            ...this.$turns.get()[last_turn_id],
            status: last_turn_status,
          })
        }
        break
      case "conversation/titleUpdated":
        this.$title.set(notification.params.title)
        break
    }
    // 3. 让受这条通知影响的查询失效。
    invalidateForNotification(
      this.queryClient,
      this.connection.getServerID(),
      notification.method
    )
  }

  // item/delta：增量接到同一位置的生成中 Item；找不到时丢弃
  private handleItemDelta(params: ItemEventParams): void {
    const items = this.$items.get()
    const index = findStreamingItemIndex(
      items,
      params.turn_id,
      params.item_index
    )
    if (index < 0 || !params.delta) return
    const view = items[index]
    this.$items.set(
      items.with(index, {
        ...view,
        item: applyItemDelta(view.item, params.delta),
      })
    )
  }

  // item/completed、item/interrupted：换成完整 Item 并结束生成；中断且不保存（没有 item）时移除
  private handleItemFinished(params: ItemEventParams, isLive: boolean): void {
    if (params.item) {
      this.putStreamingItem(params.item_index, params.item, false, isLive)
      return
    }
    const items = this.$items.get()
    const index = findStreamingItemIndex(
      items,
      params.turn_id,
      params.item_index
    )
    if (index >= 0) {
      this.unpresentedLiveItemKeys.delete(items[index].key)
      this.$items.set(items.toSpliced(index, 1))
    }
  }

  // item/toolProgress：终端输出接到这次调用已有的输出后面，只留末尾 maxToolProgressLength 个字符；其他内容换成最新的一段
  private handleToolProgress(progress: AgentToolProgress): void {
    const previous = this.$toolProgress.get()[progress.call_id]?.content
    const content =
      progress.stream === "terminal" &&
      typeof previous === "string" &&
      typeof progress.content === "string"
        ? (previous + progress.content).slice(-maxToolProgressLength)
        : progress.content
    this.$toolProgress.setKey(progress.call_id, { ...progress, content })
  }

  // turn/finished：记下终态、最终调用和错误
  private handleTurnFinished(params: TurnFinishedParams): void {
    this.$turns.setKey(params.turn_id, {
      status: params.status,
      finalModelCallID: params.final_model_call_id,
      error: params.error,
    })
    const info = this.$sessions.get()[this.followedSessionID!]
    if (info)
      recordConversationResult(
        this.serverID,
        this.conversationID,
        this.$rootSessionID.get()!,
        {
          session: info.session,
          last_turn_id: params.turn_id,
          last_turn_status: params.status,
        }
      )
  }

  // 登记生成中 Item：同 ID 已保存的以已保存为准；同 ID 或同位置的未保存 Item 原位替换，否则追加到末尾
  private putStreamingItem(
    itemIndex: number,
    item: Item,
    isStreaming: boolean,
    isLive = false
  ): void {
    const items = this.$items.get()
    const index = items.findLastIndex(
      (view) =>
        view.key === item.id ||
        (view.ordinal === undefined &&
          view.turnID === item.turn_id &&
          view.itemIndex === itemIndex)
    )
    if (index >= 0 && items[index].ordinal !== undefined) return
    const view: ViewItem = {
      key: item.id,
      item,
      turnID: item.turn_id,
      itemIndex,
      isStreaming,
    }
    if (index < 0 && isLive) this.unpresentedLiveItemKeys.add(view.key)
    this.$items.set(index < 0 ? [...items, view] : items.with(index, view))
  }

  // 应用一批已保存 Item：流水号不大于已交付位置的丢弃；同 ID 的原位替换，其余追加；
  // 保存了结果的工具调用不再保留执行中的输出
  private applySavedItems(loggedItems: LoggedItem[], isLive = false): void {
    // 1. 跳过已交付的流水号，按 Item ID 原位替换或追加；工具结果保存后去掉这次调用的执行中输出。
    const items = [...this.$items.get()]
    let hasSavedItems = false
    for (const { ordinal, item } of loggedItems) {
      if (ordinal <= this.lastDeliveredOrdinal) continue
      this.lastDeliveredOrdinal = ordinal
      if (!item) continue
      hasSavedItems = true
      const callID = item.tool_result?.call_id
      if (callID !== undefined) this.$toolProgress.setKey(callID, undefined)
      const index = items.findIndex((view) => view.key === item.id)
      const view: ViewItem = {
        key: item.id,
        item,
        ordinal,
        turnID: item.turn_id,
        isStreaming: false,
      }
      if (index < 0) {
        if (isLive) this.unpresentedLiveItemKeys.add(view.key)
        items.push(view)
      } else items[index] = view
    }
    if (!hasSavedItems) return
    this.$items.set(items)
  }

  // 用 session/subscribe 返回的生成中 Item 更新未保存的 Item：不在其中的移除（断开期间被丢弃或中断不保存），其余原位替换或追加
  private applyStreamingItems(streamingItems: StreamingItem[]): void {
    this.$items.set(
      this.$items
        .get()
        .filter(
          (view) =>
            view.ordinal !== undefined ||
            streamingItems.some(
              (streaming) =>
                streaming.item?.turn_id === view.turnID &&
                streaming.item_index === view.itemIndex
            )
        )
    )
    for (const { item_index, item } of streamingItems)
      if (item) this.putStreamingItem(item_index, item, true)
  }

  // 向前读一页更早的 Item，按流水号升序放到最前面，记下下一页游标
  private async prependEarlierItems(cursor: Ordinal): Promise<void> {
    const generation = this.syncGeneration
    let page: MethodResult["session/items/list"]
    try {
      page = await this.callForSync(generation, "session/items/list", {
        conversation_id: this.conversationID,
        session_id: this.getFollowedSessionID(),
        order: "desc",
        cursor,
        limit: historyPageSize,
      })
    } catch (error) {
      if (generation !== this.syncGeneration) return
      throw error
    }
    if (generation !== this.syncGeneration) return
    const earlierItems = page.items.flatMap(({ ordinal, item }): ViewItem[] =>
      item
        ? [
            {
              key: item.id,
              item,
              ordinal,
              turnID: item.turn_id,
              isStreaming: false,
            },
          ]
        : []
    )
    this.$items.set([...earlierItems.reverse(), ...this.$items.get()])
    this.earlierItemsCursor = page.next_cursor
    this.$hasEarlierItems.set(page.next_cursor !== undefined)
  }

  // 同步里的请求：本次同步已被新的同步或停止作废时不再发出
  private async callForSync<M extends keyof MethodParams>(
    generation: number,
    method: M,
    params: MethodParams[M]
  ): Promise<MethodResult[M]> {
    if (generation !== this.syncGeneration)
      throw new Error("conversation sync is outdated")
    const result = await this.connection.call(method, params)
    if (generation !== this.syncGeneration)
      throw new Error("conversation sync is outdated")
    return result
  }

  // 退订对话状态和跟随 Session 的内容，两个请求一起发出（有 rootSync 时只退订 Session 的内容）；没有订阅的键也返回成功
  private async unsubscribeForSync(
    generation: number,
    sessionID: SessionID
  ): Promise<void> {
    await Promise.all([
      ...(this.rootSync
        ? []
        : [
            this.callForSync(generation, "conversation/unsubscribe", {
              conversation_id: this.conversationID,
            }),
          ]),
      this.callForSync(generation, "session/unsubscribe", {
        conversation_id: this.conversationID,
        session_id: sessionID,
      }),
    ])
  }

  // 等 rootSync 第一次同步结束（成功或失败），之后才读得到这条对话的 Session 运行信息；等待期间被新的同步或停止作废时抛错
  private async waitForRootSync(generation: number): Promise<void> {
    const root = this.rootSync!
    if (!root.hasInitialData() && root.$syncState.get() === "syncing")
      await new Promise<void>((resolve) => {
        const stopListening = root.$syncState.listen((state) => {
          if (state === "syncing") return
          stopListening()
          resolve()
        })
      })
    if (generation !== this.syncGeneration)
      throw new Error("conversation sync is outdated")
  }
}
