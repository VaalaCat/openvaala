import { atom } from "nanostores"
import type {
  MethodParams,
  MethodResult,
  NotificationParams,
} from "./protocol.gen"
import type {
  GlobalServerNotification,
  ServerConnection,
  ServerConnectionDeps,
} from "./server-connection"

// GlobalSubscriptionConnection 提供全局订阅使用的连接能力。
export type GlobalSubscriptionConnection = Pick<
  ServerConnection,
  "call" | "listenServer" | "onConnected" | "$state" | "$serverID"
>

export type GlobalSubscriptionDeps = Pick<
  ServerConnectionDeps,
  "setTimeout" | "clearTimeout"
>

// GlobalSubscriptionDefinition 是一种全局订阅：订阅和退订的方法、推送完整值和服务端关闭订阅的通知，
// 以及怎样从订阅结果和完整值通知里取出值。订阅和退订都不带参数。
export interface GlobalSubscriptionDefinition<
  Subscribe extends keyof MethodParams & keyof MethodResult,
  Changed extends keyof NotificationParams,
  Value,
> {
  subscribeMethod: Subscribe
  unsubscribeMethod: keyof MethodParams
  changedMethod: Changed
  closedMethod: keyof NotificationParams
  getInitialValue(result: MethodResult[Subscribe]): Value
  getChangedValue(params: NotificationParams[Changed]): Value
}

const defaultDeps: GlobalSubscriptionDeps = {
  setTimeout: (callback, delayMs) => setTimeout(callback, delayMs),
  clearTimeout: (handle) =>
    clearTimeout(handle as ReturnType<typeof setTimeout>),
}

// GlobalSubscriptionSync 同步服务器推送的一份全局完整值（不属于某个对话），并恢复断线或关闭的订阅。
export class GlobalSubscriptionSync<
  Subscribe extends keyof MethodParams & keyof MethodResult,
  Changed extends keyof NotificationParams,
  Value,
> {
  // undefined 表示尚未同步或连接断开；其余是已确认的完整值。
  readonly $value = atom<Value | undefined>(undefined)
  private readonly definition: GlobalSubscriptionDefinition<
    Subscribe,
    Changed,
    Value
  >
  private readonly connection: GlobalSubscriptionConnection
  private readonly deps: GlobalSubscriptionDeps
  private stopRunning: (() => void) | undefined
  private syncGeneration = 0
  private isSubscribing = false
  // 订阅请求还没回复期间收到的完整值，回复后按收到的顺序应用
  private stagedValues: Value[] = []
  private retryTimer: unknown
  private failedSubscriptions = 0

  constructor(
    definition: GlobalSubscriptionDefinition<Subscribe, Changed, Value>,
    connection: GlobalSubscriptionConnection,
    deps: GlobalSubscriptionDeps = defaultDeps
  ) {
    this.definition = definition
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
        .call(this.definition.unsubscribeMethod, {})
        .catch(() => undefined)
    }
    this.stopRunning = stop
    this.failedSubscriptions = 0
    if (this.connection.$state.get() === "connected") this.startSync()
    return stop
  }

  // 先标记未知，再订阅当前完整值，按收到顺序应用初始值和后续通知。
  private startSync(): void {
    // 1. 作废旧同步，只在本对象运行且连接有效时发送订阅。
    this.resetSync()
    if (!this.stopRunning || this.connection.$state.get() !== "connected")
      return
    const generation = this.syncGeneration
    this.isSubscribing = true
    // 2. 应用初始值及订阅期间收到的完整值。
    void this.connection
      .call(this.definition.subscribeMethod, {} as MethodParams[Subscribe])
      .then(
        (result) => {
          if (generation !== this.syncGeneration) return
          this.$value.set(this.definition.getInitialValue(result))
          for (const value of this.stagedValues) this.$value.set(value)
          this.stagedValues = []
          this.isSubscribing = false
          this.failedSubscriptions = 0
        },
        () => {
          if (generation !== this.syncGeneration) return
          this.isSubscribing = false
          this.stagedValues = []
          this.scheduleRetry(generation)
        }
      )
  }

  // 接收完整值，或在服务端关闭订阅时重新建立这一条订阅。
  private handleNotification(notification: GlobalServerNotification): void {
    // 1. 异常关闭后立即标记未知，并开始一次恢复。
    if (notification.method === this.definition.closedMethod) {
      this.startSync()
      return
    }
    // 2. 初始订阅期间暂存完整值，同步完成后直接替换。
    if (notification.method !== this.definition.changedMethod) return
    const value = this.definition.getChangedValue(
      notification.params as NotificationParams[Changed]
    )
    if (this.isSubscribing) {
      this.stagedValues.push(value)
      return
    }
    if (this.$value.get() !== undefined) this.$value.set(value)
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

  // 作废同步结果，取消恢复计时，并将值设为未知。
  private resetSync(): void {
    // 1. 清理旧同步和重试，保留本对象的通知登记。
    this.syncGeneration++
    this.isSubscribing = false
    this.stagedValues = []
    if (this.retryTimer !== undefined) this.deps.clearTimeout(this.retryTimer)
    this.retryTimer = undefined
    this.$value.set(undefined)
  }
}
