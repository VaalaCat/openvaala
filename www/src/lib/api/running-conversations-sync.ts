import { atom } from "nanostores"
import type { ConversationID } from "./protocol.gen"
import type {
  GlobalServerNotification,
  ServerConnection,
  ServerConnectionDeps,
} from "./server-connection"

// RunningConversationsConnection 提供全局运行订阅使用的连接能力。
export type RunningConversationsConnection = Pick<
  ServerConnection,
  "call" | "listenServer" | "onConnected" | "$state" | "$serverID"
>

type RunningConversationsSyncDeps = Pick<
  ServerConnectionDeps,
  "setTimeout" | "clearTimeout"
>

type RunningChangedNotification = Extract<
  GlobalServerNotification,
  { method: "conversation/runningChanged" }
>

const defaultDeps: RunningConversationsSyncDeps = {
  setTimeout: (callback, delayMs) => setTimeout(callback, delayMs),
  clearTimeout: (handle) =>
    clearTimeout(handle as ReturnType<typeof setTimeout>),
}

// RunningConversationsSync 同步服务器当前执行 Turn 的对话集合，并恢复断线或关闭的订阅。
export class RunningConversationsSync {
  // undefined 表示尚未同步或连接断开；空集合表示已确认没有运行中的对话。
  readonly $conversationIDs = atom<ReadonlySet<ConversationID> | undefined>(
    undefined
  )
  private readonly connection: RunningConversationsConnection
  private readonly deps: RunningConversationsSyncDeps
  private stopRunning: (() => void) | undefined
  private syncGeneration = 0
  private isSubscribing = false
  private stagedNotifications: RunningChangedNotification[] = []
  private retryTimer: unknown
  private failedSubscriptions = 0

  constructor(
    connection: RunningConversationsConnection,
    deps: RunningConversationsSyncDeps = defaultDeps
  ) {
    this.connection = connection
    this.deps = deps
  }

  // 登记全局通知并建立订阅；返回停止同步及退订的函数，可以再次运行。
  run(): () => void {
    // 1. 结束此前的运行，登记通知、连接状态和重连处理。
    this.stopRunning?.()
    const stopListening = this.connection.listenServer((notification) =>
      this.handleNotification(notification)
    )
    const stopFollowingState = this.connection.$state.listen((state) => {
      if (state !== "connected") this.resetSync()
    })
    const stopFollowingServerID = this.connection.$serverID.listen(() =>
      this.resetSync()
    )
    const stopFollowingReconnect = this.connection.onConnected(() =>
      this.startSync()
    )
    const stop = () => {
      if (this.stopRunning !== stop) return
      // 2. 先移除监听及恢复回调，再作废同步并退订。
      this.stopRunning = undefined
      stopListening()
      stopFollowingState()
      stopFollowingServerID()
      stopFollowingReconnect()
      this.resetSync()
      void this.connection
        .call("conversation/running/unsubscribe", {})
        .catch(() => undefined)
    }
    this.stopRunning = stop
    this.failedSubscriptions = 0
    if (this.connection.$state.get() === "connected") this.startSync()
    return stop
  }

  // 先标记未知，再订阅当前运行集合，按收到顺序应用初始值和后续通知。
  private startSync(): void {
    // 1. 作废旧同步，只在本对象运行且连接有效时发送订阅。
    this.resetSync()
    if (!this.stopRunning || this.connection.$state.get() !== "connected")
      return
    const generation = this.syncGeneration
    this.isSubscribing = true
    // 2. 应用初始集合及订阅期间的完整集合通知。
    void this.connection.call("conversation/running/subscribe", {}).then(
      (result) => {
        if (generation !== this.syncGeneration) return
        this.$conversationIDs.set(new Set(result.conversation_ids))
        for (const notification of this.stagedNotifications)
          this.$conversationIDs.set(
            new Set(notification.params.conversation_ids)
          )
        this.stagedNotifications = []
        this.isSubscribing = false
        this.failedSubscriptions = 0
      },
      () => {
        if (generation !== this.syncGeneration) return
        this.isSubscribing = false
        this.stagedNotifications = []
        this.scheduleRetry(generation)
      }
    )
  }

  // 接收完整运行集合，或在服务端关闭订阅时重新建立这一条订阅。
  private handleNotification(notification: GlobalServerNotification): void {
    // 1. 异常关闭后立即标记未知，并开始一次恢复。
    if (notification.method === "conversation/runningClosed") {
      this.startSync()
      return
    }
    // 2. 初始订阅期间暂存通知，同步完成后直接替换集合。
    if (notification.method !== "conversation/runningChanged") return
    if (this.isSubscribing) {
      this.stagedNotifications.push(notification)
      return
    }
    if (this.$conversationIDs.get() !== undefined)
      this.$conversationIDs.set(new Set(notification.params.conversation_ids))
  }

  // 订阅失败后有限退避，断线时等待连接恢复。
  private scheduleRetry(generation: number): void {
    // 1. 只恢复仍有效且已连接的这次运行。
    if (!this.stopRunning || this.connection.$state.get() !== "connected")
      return
    this.failedSubscriptions++
    const delayMs = Math.min(1000 * 2 ** (this.failedSubscriptions - 1), 10_000)
    this.retryTimer = this.deps.setTimeout(() => {
      if (generation !== this.syncGeneration) return
      this.retryTimer = undefined
      this.startSync()
    }, delayMs)
  }

  // 作废同步结果，取消恢复计时，并将运行集合设为未知。
  private resetSync(): void {
    // 1. 清理旧同步和重试，保留本对象的通知登记。
    this.syncGeneration++
    this.isSubscribing = false
    this.stagedNotifications = []
    if (this.retryTimer !== undefined) this.deps.clearTimeout(this.retryTimer)
    this.retryTimer = undefined
    this.$conversationIDs.set(undefined)
  }
}
