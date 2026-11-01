import { atom } from "nanostores"
import packageInfo from "../../../package.json" with { type: "json" }
import { RpcError, rpcConnectionLostCode } from "../rpc-error"
import {
  ErrorCodeInvalidRequest,
  ProtocolVersion,
  type ConversationID,
  type JSONRPCMessage,
  type MethodParams,
  type MethodResult,
  type NotificationParams,
} from "./protocol.gen"
import type { ServerEntry } from "./servers"

// ConnectionState 描述页面和 serve 的连接：unauthorized 和 incompatible 要用户处理，不再自动重连
export type ConnectionState =
  "connecting" | "connected" | "reconnecting" | "unauthorized" | "incompatible"

// RpcFrame 是 RPC 记录里的一帧：发出或收到的请求、响应、通知
export interface RpcFrame {
  direction: "sent" | "received"
  // 响应帧的 method 取自对应的请求
  method?: string
  id?: number
  time: number
  size: number
  payload: unknown
}

// ServerNotification 是 serve 推送的一条通知，按 method 区分参数类型
export type ServerNotification = {
  [M in keyof NotificationParams]: { method: M; params: NotificationParams[M] }
}[keyof NotificationParams]

// GlobalServerNotification 是不属于某个对话的服务端通知。
export type GlobalServerNotification = Exclude<
  ServerNotification,
  { params: { conversation_id: ConversationID } }
>

// WebSocketLike 是 ServerConnection 用到的 WebSocket 能力，浏览器的 WebSocket 满足它
export interface WebSocketLike {
  send(data: string): void
  close(): void
  onopen: ((event: Event) => void) | null
  onmessage: ((event: MessageEvent) => void) | null
  onclose: ((event: CloseEvent) => void) | null
}

// ServerConnectionDeps 是连接依赖的外部能力，测试用内存实现替换
export interface ServerConnectionDeps {
  createWebSocket(url: string): WebSocketLike
  fetch: typeof fetch
  setTimeout(callback: () => void, delayMs: number): unknown
  clearTimeout(handle: unknown): void
}

// 等待回复的请求
interface PendingCall {
  method: string
  resolve(result: unknown): void
  reject(error: RpcError): void
}

const defaultDeps: ServerConnectionDeps = {
  createWebSocket: (url) => new WebSocket(url),
  // 包一层，保证 fetch 以全局对象为 this 调用
  fetch: (input, init) => fetch(input, init),
  setTimeout: (callback, delayMs) => setTimeout(callback, delayMs),
  clearTimeout: (handle) =>
    clearTimeout(handle as ReturnType<typeof setTimeout>),
}
const connectionAttemptTimeoutMs = 10_000
const maxRpcLogFrames = 2000
const textEncoder = new TextEncoder()

// ServerConnection 保存页面和一个 serve 之间的 WebSocket 连接：握手、请求与响应、通知分发、断线重连和 RPC 记录。
export class ServerConnection {
  readonly entry: ServerEntry
  readonly $state = atom<ConnectionState>("connecting")
  // 连上之后才有值，之后断线重连也保留
  readonly $serverID = atom<string | undefined>(undefined)
  readonly $rpcLog = atom<RpcFrame[]>([])
  // RPC 记录是否暂停？暂停期间收发的帧不记
  readonly $isRpcLogPaused = atom(false)
  // 用户点击触发或加入的连接尝试是否尚未结束？
  readonly $isManualReconnectPending = atom(false)
  private readonly deps: ServerConnectionDeps
  // 当前这次 run 正在使用的 WebSocket，握手完成前也有值
  private socket: WebSocketLike | undefined
  // 每次 run 加一，stop 也加一，让上一次 run 遗留的异步结果自己作废
  private runID = 0
  private isRunning = false
  private attemptID = 0
  private attemptTimer: unknown
  private reconnectTimer: unknown
  private authProbeController: AbortController | undefined
  private authProbeTimer: unknown
  // 最近一次连上之后连续失败的连接次数，决定重连等待多久
  private failedConnectionAttempts = 0
  private nextRequestID = 1
  private readonly pendingCalls = new Map<number, PendingCall>()
  private readonly notificationHandlers = new Map<
    ConversationID,
    Set<(notification: ServerNotification) => void>
  >()
  private readonly connectedHandlers = new Set<() => void>()
  private readonly serverNotificationHandlers = new Set<
    (notification: GlobalServerNotification) => void
  >()

  constructor(entry: ServerEntry, deps: ServerConnectionDeps = defaultDeps) {
    this.entry = entry
    this.deps = deps
  }

  // 建立连接并保持到返回的函数被调用；断线按退避重连
  run(): () => void {
    // 1. 登记这一次运行并开始连接。
    if (this.isRunning) this.stop(this.runID)
    const runID = ++this.runID
    this.isRunning = true
    this.$state.set("connecting")
    this.failedConnectionAttempts = 0
    this.connect(runID)
    // 2. 返回结束这一次运行的函数。
    return () => this.stop(runID)
  }

  // 跳过退避开始连接；正在连接时加入同一次尝试，已连接或停止时不操作。
  reconnect(): void {
    // 1. 仅对普通断线发起手动尝试。
    const state = this.$state.get()
    if (
      !this.isRunning ||
      state === "connected" ||
      state === "unauthorized" ||
      state === "incompatible"
    )
      return
    this.$isManualReconnectPending.set(true)
    if (this.socket) return
    // 2. 取消等待及鉴权探测，立即开始一次新尝试。
    if (this.reconnectTimer !== undefined) {
      this.deps.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = undefined
    }
    this.cancelAuthProbe()
    this.connect(this.runID)
  }

  // 发出一个请求并等待回复；没连上时立即以「连接已断开」失败
  call<M extends keyof MethodParams>(
    method: M,
    params: MethodParams[M]
  ): Promise<MethodResult[M]> {
    const socket = this.socket
    if (!socket || this.$state.get() !== "connected")
      return Promise.reject(
        new RpcError(rpcConnectionLostCode, "connection lost", method)
      )
    return this.sendRequest(socket, method, params)
  }

  // initialize 返回的 server_id；只在连上之后调用，之前调用直接抛错
  getServerID(): string {
    const serverID = this.$serverID.get()
    if (serverID === undefined)
      throw new Error("server connection is not initialized")
    return serverID
  }

  // 登记某个对话的通知处理：对话订阅和 Session 订阅的通知都按 conversation_id 交给它；返回取消登记的函数
  listen(
    conversationID: ConversationID,
    handler: (notification: ServerNotification) => void
  ): () => void {
    const handlers = this.notificationHandlers.get(conversationID) ?? new Set()
    handlers.add(handler)
    this.notificationHandlers.set(conversationID, handlers)
    return () => {
      handlers.delete(handler)
      if (handlers.size === 0) this.notificationHandlers.delete(conversationID)
    }
  }

  // 登记无 conversation_id 的服务器通知；返回取消登记的函数。
  listenServer(
    handler: (notification: GlobalServerNotification) => void
  ): () => void {
    // 1. 登记处理函数并返回清理入口。
    this.serverNotificationHandlers.add(handler)
    return () => this.serverNotificationHandlers.delete(handler)
  }

  // 连上（含重连后）时调用；返回取消登记的函数
  onConnected(handler: () => void): () => void {
    this.connectedHandlers.add(handler)
    return () => this.connectedHandlers.delete(handler)
  }

  setRpcLogPaused(paused: boolean): void {
    this.$isRpcLogPaused.set(paused)
  }

  clearRpcLog(): void {
    this.$rpcLog.set([])
  }

  // 打开一条 WebSocket，握手、消息和断开都交给对应的处理函数
  private connect(runID: number): void {
    // 1. 为本次 socket 和 initialize 登记共同期限。
    const attemptID = ++this.attemptID
    const url = `${this.entry.url}?token=${encodeURIComponent(this.entry.token)}`
    const socket = this.deps.createWebSocket(url)
    this.socket = socket
    socket.onopen = () => void this.handleOpen(socket)
    socket.onmessage = (event) => this.handleMessage(socket, event)
    socket.onclose = () => this.handleClose(socket, runID, attemptID)
    this.attemptTimer = this.deps.setTimeout(() => {
      if (
        runID !== this.runID ||
        attemptID !== this.attemptID ||
        this.socket !== socket ||
        this.$state.get() === "connected"
      )
        return
      this.handleClose(socket, runID, attemptID)
    }, connectionAttemptTimeoutMs)
  }

  // 连接建立后发 initialize；成功才算连上，协议版本不一致时不再重连
  private async handleOpen(socket: WebSocketLike): Promise<void> {
    // 1. 发出 initialize 并等待回复。
    if (this.socket !== socket) return
    let serverID: string
    try {
      const result = await this.sendRequest(socket, "initialize", {
        client_info: { name: "openvaala-web", version: packageInfo.version },
        protocol_version: ProtocolVersion,
      })
      serverID = result.server_id
    } catch (error) {
      // 2. 失败：连接已经断开的由 handleClose 处理；协议版本不一致停止重连；其他错误关闭连接后重连。
      if (this.socket !== socket) return
      if (error instanceof RpcError && error.code === ErrorCodeInvalidRequest) {
        this.clearAttemptTimer()
        this.detachSocket(socket)
        this.$isManualReconnectPending.set(false)
        this.$state.set("incompatible")
        return
      }
      socket.close()
      return
    }
    // 3. 成功：记下 server_id，改成已连上并通知等待连接的处理函数。
    if (this.socket !== socket) return
    this.clearAttemptTimer()
    this.$isManualReconnectPending.set(false)
    this.failedConnectionAttempts = 0
    this.$serverID.set(serverID)
    this.$state.set("connected")
    for (const handler of [...this.connectedHandlers]) handler()
  }

  // 收到一帧：响应交给等待的请求，通知交给对应对话的处理函数
  private handleMessage(socket: WebSocketLike, event: MessageEvent): void {
    // 1. 只处理当前连接上的文本帧，无法解析的帧丢弃。
    if (this.socket !== socket || typeof event.data !== "string") return
    let message: JSONRPCMessage
    try {
      message = JSON.parse(event.data)
    } catch {
      return
    }
    // 2. 带编号的是响应：按编号对回等待的请求。
    if (typeof message.id === "number") {
      const pending = this.pendingCalls.get(message.id)
      this.recordFrame(
        "received",
        event.data,
        message,
        pending?.method,
        message.id
      )
      if (!pending) return
      this.pendingCalls.delete(message.id)
      if (message.error)
        pending.reject(
          new RpcError(
            message.error.code,
            message.error.message,
            pending.method
          )
        )
      else pending.resolve(message.result)
      return
    }
    // 3. 其余是通知：按 params.conversation_id 交给登记过的处理函数。
    this.recordFrame("received", event.data, message, message.method)
    const conversationID = (
      message.params as { conversation_id?: unknown } | undefined
    )?.conversation_id
    if (typeof conversationID !== "string") {
      const notification = message as unknown as GlobalServerNotification
      for (const handler of [...this.serverNotificationHandlers])
        handler(notification)
      return
    }
    const notification = message as unknown as ServerNotification
    for (const handler of [
      ...(this.notificationHandlers.get(conversationID) ?? []),
    ])
      handler(notification)
  }

  // 连接断开或超时：未回复的请求失败，结束手动尝试，再探测鉴权并退避重连。
  private handleClose(
    socket: WebSocketLike,
    runID: number,
    attemptID: number
  ): void {
    // 1. 结束本次连接尝试并关闭 socket。
    if (this.socket !== socket) return
    this.clearAttemptTimer()
    this.detachSocket(socket)
    this.$isManualReconnectPending.set(false)
    this.$state.set("reconnecting")
    this.failedConnectionAttempts++
    // 2. 查 token 是否有效，再安排下一次连接。
    void this.scheduleReconnect(runID, attemptID)
  }

  // 探测 token 后按退避等待；旧尝试的探测和计时结果不再生效。
  private async scheduleReconnect(
    runID: number,
    attemptID: number
  ): Promise<void> {
    // 1. WebSocket 握手失败后用普通请求探测 401。
    const isTokenRejected = await this.isTokenRejected()
    if (runID !== this.runID || attemptID !== this.attemptID || !this.isRunning)
      return
    if (isTokenRejected) {
      this.$state.set("unauthorized")
      return
    }
    // 2. 其他结果继续退避重连。
    this.reconnectTimer = this.deps.setTimeout(() => {
      if (
        runID !== this.runID ||
        attemptID !== this.attemptID ||
        !this.isRunning
      )
        return
      this.reconnectTimer = undefined
      this.connect(runID)
    }, getReconnectDelay(this.failedConnectionAttempts))
  }

  // 探测 HTTP 401；网络失败、取消和十秒无响应都按普通网络故障处理。
  private async isTokenRejected(): Promise<boolean> {
    // 1. 登记可取消的鉴权探测及期限。
    const controller = new AbortController()
    this.authProbeController = controller
    let handleAbort!: () => void
    const aborted = new Promise<boolean>((resolve) => {
      handleAbort = () => resolve(false)
      controller.signal.addEventListener("abort", handleAbort, { once: true })
    })
    const timer = this.deps.setTimeout(
      () => controller.abort(),
      connectionAttemptTimeoutMs
    )
    this.authProbeTimer = timer
    // 2. 等待响应或取消，最后清理这次探测。
    try {
      const response = this.deps
        .fetch(this.entry.url.replace(/^ws/, "http"), {
          headers: { Authorization: `Bearer ${this.entry.token}` },
          signal: controller.signal,
        })
        .then(
          (response) => response.status === 401,
          () => false
        )
      return await Promise.race([response, aborted])
    } catch {
      return false
    } finally {
      controller.signal.removeEventListener("abort", handleAbort)
      this.deps.clearTimeout(timer)
      if (this.authProbeController === controller) {
        this.authProbeController = undefined
        this.authProbeTimer = undefined
      }
    }
  }

  // 取消本对象发起的鉴权探测和期限。
  private cancelAuthProbe(): void {
    // 1. 中止请求并清理期限。
    this.authProbeController?.abort()
    this.authProbeController = undefined
    if (this.authProbeTimer !== undefined)
      this.deps.clearTimeout(this.authProbeTimer)
    this.authProbeTimer = undefined
  }

  // 清理 socket 建立和 initialize 的共同期限。
  private clearAttemptTimer(): void {
    // 1. 取消本次尝试的定时回调。
    if (this.attemptTimer !== undefined)
      this.deps.clearTimeout(this.attemptTimer)
    this.attemptTimer = undefined
  }

  // 结束这一次运行：取消计时器和探测，关闭连接，让等待回复的请求失败。
  private stop(runID: number): void {
    // 1. 作废本次运行，取消所有等待。
    if (runID !== this.runID) return
    this.runID++
    this.isRunning = false
    this.attemptID++
    this.clearAttemptTimer()
    this.cancelAuthProbe()
    if (this.reconnectTimer !== undefined) {
      this.deps.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = undefined
    }
    // 2. 关闭连接并结束手动进行状态。
    if (this.socket) this.detachSocket(this.socket)
    this.$isManualReconnectPending.set(false)
    this.$state.set("reconnecting")
  }

  // 主动关闭连接，不再处理它之后的事件，等待回复的请求以「连接已断开」失败
  private detachSocket(socket: WebSocketLike): void {
    this.socket = undefined
    socket.onopen = null
    socket.onmessage = null
    socket.onclose = null
    socket.close()
    this.failPendingCalls()
  }

  private failPendingCalls(): void {
    const pendingCalls = [...this.pendingCalls.values()]
    this.pendingCalls.clear()
    for (const pending of pendingCalls)
      pending.reject(
        new RpcError(rpcConnectionLostCode, "connection lost", pending.method)
      )
  }

  // 编号请求写入 socket 并登记等待回复
  private sendRequest<M extends keyof MethodParams>(
    socket: WebSocketLike,
    method: M,
    params: MethodParams[M]
  ): Promise<MethodResult[M]> {
    const id = this.nextRequestID++
    const payload = { id, method, params }
    const text = JSON.stringify(payload)
    return new Promise((resolve, reject) => {
      this.pendingCalls.set(id, {
        method,
        resolve: resolve as (result: unknown) => void,
        reject,
      })
      this.recordFrame("sent", text, payload, method, id)
      socket.send(text)
    })
  }

  // 记一帧到 RPC 记录，超过上限丢最早的；暂停时不记
  private recordFrame(
    direction: RpcFrame["direction"],
    text: string,
    payload: unknown,
    method?: string,
    id?: number
  ): void {
    if (this.$isRpcLogPaused.get()) return
    const frame: RpcFrame = {
      direction,
      method,
      id,
      time: Date.now(),
      size: textEncoder.encode(text).length,
      payload,
    }
    const log = this.$rpcLog.get()
    this.$rpcLog.set(
      log.length < maxRpcLogFrames ? [...log, frame] : [...log.slice(1), frame]
    )
  }
}

// 重连前的退避等待：第 1 到 4 次失败后等 1、2、4、8 秒，之后每 10 秒一次；同 internal/runtimeclient/helper.go 的 getReconnectDelay
function getReconnectDelay(attempt: number): number {
  return attempt >= 5 ? 10_000 : 1000 * 2 ** (attempt - 1)
}
