import { FitAddon } from "@xterm/addon-fit"
import { Terminal, type ITheme } from "@xterm/xterm"
import { i18n } from "../../i18n"
import {
  ErrorCodeTerminalNotFound,
  type ConversationID,
  type TerminalAttachResult,
  type TerminalOutputParams,
} from "../api/protocol.gen"
import type {
  ConnectionState,
  GlobalServerNotification,
  ServerConnection,
} from "../api/server-connection"
import {
  getRpcErrorMessage,
  isConnectionLostError,
  RpcError,
} from "../rpc-error"
import { $appearance } from "../appearance/preferences"
import {
  getControlCharacter,
  getTerminalKeySequence,
  type TerminalKey,
} from "./terminal-input"

// TerminalSessionPhase 是终端的阶段：opening 在等 terminal/open 的回复，running 可以输入，
// reconnecting 在接回 serve 上已有的终端（连接断开，或刷新后恢复标签页），exited 已结束（标签页保留，可重新启动）。
export type TerminalSessionPhase =
  "opening" | "running" | "reconnecting" | "exited"

// TerminalSessionSnapshot 是界面要显示的终端状态；状态变化时换成新对象。
export type TerminalSessionSnapshot = {
  phase: TerminalSessionPhase
  // exited 时的结束原因：退出码、serve 返回的错误或终端已不存在；其他阶段为空
  exitReason: string
  // 标题：shell 用转义序列设的标题，没有时是「Executor · 目录名」
  title: string
  // 窄屏按键条的 Ctrl 是否按下？按下后下一个输入的字符变成控制字节
  isControlPending: boolean
  // reconnecting 时是否在等连接恢复？刷新后恢复标签页时连接是好的，只等接入的回复
  isWaitingForConnection: boolean
}

const textEncoder = new TextEncoder()

// 重画终端时两次改尺寸之间的间隔：连续改两次尺寸全屏程序可能不重画
const redrawIntervalMs = 100

// TerminalSession 持有一个终端标签页的 xterm 实例和它在 serve 上的终端。
// 终端由 serve 持有：连接断开或页面刷新后用 resume 接回，从已收到的输出位置续读，终端结束后才标为已结束。
// 结束后保留 xterm 的屏幕内容，restart 清屏后在同一个位置重新打开。
export class TerminalSession {
  readonly terminal: Terminal
  readonly executorID: string
  readonly directory: string
  private readonly connection: ServerConnection
  // 打开终端的对话，只决定其他页面把这个终端显示在哪条对话
  private readonly conversationID: ConversationID
  private readonly fitAddon = new FitAddon()
  private readonly listeners = new Set<() => void>()
  private phase: TerminalSessionPhase = "opening"
  private exitReason = ""
  private shellTitle = ""
  private isControlPending = false
  // serve 上终端的 ID，打开成功后才有
  private terminalID = ""
  // serve 实际使用的 Executor 和起始目录，打开成功后才有
  private openedExecutorID = ""
  private openedDirectory = ""
  // serve 已知的终端行列数
  private reportedRows = 0
  private reportedCols = 0
  // 已写进 xterm 的输出位置；新 xterm 是 0
  private receivedCursor = 0
  // 回放期间为 true：xterm 产生的输入不发给 serve
  private isReplaying = false
  // 这次回放是否遇到过截断
  private wasTruncated = false
  // 最近一次 terminal/open 的处理；打开还没回复时关闭标签页，等它回来才知道要结束哪个终端
  private openRequest: Promise<void> = Promise.resolve()
  private redrawTimer: ReturnType<typeof setTimeout> | undefined
  // 已关闭的终端：xterm 已销毁，不再处理任何事件
  private isClosed = false
  // 停止接收终端通知和连接状态的函数，只在 opening、running 和 reconnecting 时有
  private stopListening: (() => void) | undefined
  private snapshot: TerminalSessionSnapshot
  private container: HTMLElement | undefined
  private readonly stopAppearance: () => void

  constructor(
    connection: ServerConnection,
    conversationID: ConversationID,
    executorID: string,
    directory: string
  ) {
    this.connection = connection
    this.conversationID = conversationID
    this.executorID = executorID
    this.directory = directory
    const { fontSize, lineHeight, letterSpacing } = $appearance.get().terminal
    this.terminal = new Terminal({
      cursorBlink: true,
      scrollback: 5000,
      fontSize,
      lineHeight,
      letterSpacing,
    })
    this.terminal.loadAddon(this.fitAddon)
    this.terminal.onData((data) => this.handleInput(data))
    this.terminal.onBinary((data) =>
      this.sendInput(Uint8Array.from(data, (char) => char.charCodeAt(0)))
    )
    this.terminal.onTitleChange((title) => {
      this.shellTitle = title
      this.publish()
    })
    this.snapshot = this.createSnapshot()
    this.stopAppearance = $appearance.listen((appearance, previous) => {
      if (appearance.terminal === previous?.terminal) return
      const { fontSize, lineHeight, letterSpacing } = appearance.terminal
      this.terminal.options = { fontSize, lineHeight, letterSpacing }
      if (this.container) this.fit(this.container)
    })
  }

  // 向 serve 打开新终端：先按 xterm 当前的行列（新终端是 24×80），打开后再把界面排好的行列补发过去；
  // 返回的 Promise 在打开请求处理完后兑现（成功、失败，或标签页已关闭），不会拒绝
  start(): Promise<void> {
    // 1. 进入 opening，开始接收这个终端的输出、结束通知和连接状态；新终端的输出从 0 开始。
    this.phase = "opening"
    this.exitReason = ""
    this.terminalID = ""
    this.receivedCursor = 0
    this.isReplaying = false
    this.wasTruncated = false
    this.stopListening = this.listenConnection()
    this.publish()
    // 2. 打开终端；回复到达时界面可能已经关闭或连接已断开，由回复的处理函数收尾。
    const { rows, cols } = this.terminal
    this.openRequest = this.connection
      .call("terminal/open", {
        executor_id: this.executorID,
        directory: this.directory,
        rows,
        cols,
        conversation_id: this.conversationID,
      })
      .then(
        (opened) => this.handleOpened(opened, rows, cols),
        (error: unknown) => this.handleOpenFailed(error)
      )
    return this.openRequest
  }

  // 接回 serve 上已有的终端：新 xterm 从 0 开始回放，连接断开后重连从 receivedCursor 续读
  resume(terminalID: string): void {
    this.terminalID = terminalID
    this.stopListening = this.listenConnection()
    this.attachTerminal()
  }

  // 返回 serve 上终端的 ID，打开成功后才有
  getTerminalID(): string {
    return this.terminalID
  }

  // 把 xterm 接进容器并让它跟随容器大小和页面主题；返回取消的函数。同一个终端可以反复接入不同的容器，屏幕内容不丢
  attach(container: HTMLElement): () => void {
    // 1. 字体加载完成后才接入，免得按备用字体量出错的字符宽度。
    let isDetached = false
    let frame = 0
    const fontFamily = getMonospaceFontFamily(container)
    const fit = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => this.fit(container))
    }
    const applyTheme = () => {
      this.terminal.options.theme = getTerminalTheme(container)
    }
    const sizeObserver = new ResizeObserver(fit)
    const themeObserver = new MutationObserver(applyTheme)
    const mount = () => {
      if (isDetached || this.isClosed) return
      this.terminal.options.fontFamily = fontFamily
      applyTheme()
      if (this.terminal.element) container.append(this.terminal.element)
      else this.terminal.open(container)
      this.container = container
      sizeObserver.observe(container)
      themeObserver.observe(document.documentElement, {
        attributes: true,
        attributeFilter: ["class", "style", "data-theme"],
      })
      fit()
    }
    void document.fonts
      .load(`${this.terminal.options.fontSize}px ${fontFamily}`)
      .then(mount, mount)
    // 2. 取消：停止观察，xterm 留在原处等下一次接入。
    return () => {
      isDetached = true
      if (this.container === container) this.container = undefined
      sizeObserver.disconnect()
      themeObserver.disconnect()
      cancelAnimationFrame(frame)
    }
  }

  // 重新启动已结束的终端：清屏后在同一个 Executor 和目录打开新的；返回的 Promise 同 start
  restart(): Promise<void> {
    // 1. 只有已结束的终端能重新启动，连点两次不会打开两个。
    if (this.phase !== "exited") return Promise.resolve()
    // 2. 清掉旧的屏幕内容和 shell 设的标题，再打开。
    this.terminal.reset()
    this.shellTitle = ""
    return this.start()
  }

  // 重置终端：serve 丢掉保留的输出，所有接入的客户端一起清屏，再让程序重画；只有 running 时能用
  async reset(): Promise<void> {
    // 1. xterm 的屏幕由 serve 发来的清屏序列清掉，这里只请求重置。
    if (this.phase !== "running") return
    await this.connection.call("terminal/reset", {
      terminal_id: this.terminalID,
    })
    // 2. 重画：全屏程序和 shell 收到尺寸变化后重画提示符。
    this.requestRedraw()
  }

  // 把终端标为已结束并保留标签页；reason 显示在界面上
  markExited(reason: string): void {
    this.stopListening?.()
    this.stopListening = undefined
    this.phase = "exited"
    this.exitReason = reason
    this.isControlPending = false
    this.isReplaying = false
    this.publish()
  }

  // 关闭标签页时调用：销毁 xterm，结束 serve 上还在运行的终端；连接断开时发不出去，重连后标签页会随运行中的终端回来
  async close(): Promise<void> {
    // 1. 不再接收任何事件，销毁 xterm。
    this.stopListening?.()
    this.stopListening = undefined
    const wasOpening = this.phase === "opening"
    let hasTerminal = this.phase === "running" || this.phase === "reconnecting"
    this.stopAppearance()
    clearTimeout(this.redrawTimer)
    this.container = undefined
    this.phase = "exited"
    this.isClosed = true
    this.listeners.clear()
    this.terminal.dispose()
    // 2. 打开还没有回复时，等回复记下刚打开的终端 ID。
    if (wasOpening) {
      await this.openRequest
      hasTerminal = this.terminalID !== ""
    }
    // 3. 终端还在运行时通知 serve 结束它；已结束的终端不用通知。
    if (!hasTerminal) return
    await this.connection
      .call("terminal/close", { terminal_id: this.terminalID })
      .catch(() => undefined)
  }

  // 按下或松开窄屏按键条的 Ctrl
  setControlPending(isPending: boolean): void {
    this.isControlPending = isPending
    this.publish()
  }

  // 发出窄屏按键条的一个按键
  sendKey(key: TerminalKey): void {
    this.isControlPending = false
    this.publish()
    this.sendInput(
      textEncoder.encode(
        getTerminalKeySequence(
          key,
          this.terminal.modes.applicationCursorKeysMode
        )
      )
    )
    this.terminal.focus()
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }

  getSnapshot = (): TerminalSessionSnapshot => this.snapshot

  // 登记 serve 的终端通知和连接状态：连接离开 connected 时等连接恢复，连上后重新接入
  private listenConnection(): () => void {
    const stopNotifications = this.connection.listenServer((notification) =>
      this.handleNotification(notification)
    )
    const stopState = this.connection.$state.listen((state) =>
      this.handleConnectionState(state)
    )
    return () => {
      stopNotifications()
      stopState()
    }
  }

  // 连接状态变化：断开时进入 reconnecting，连上后重新接入；打开还没回复的由 terminal/open 的失败收尾
  private handleConnectionState(state: ConnectionState): void {
    if (state === "connected") {
      if (this.phase === "reconnecting") this.attachTerminal()
      return
    }
    if (this.phase !== "running" && this.phase !== "reconnecting") return
    this.phase = "reconnecting"
    this.isControlPending = false
    this.publish()
  }

  // terminal/open 成功：记下终端 ID 并进入 running；打开期间已经关闭的，终端 ID 留给 close 去结束
  private handleOpened(
    opened: { terminal_id: string; executor_id: string; directory: string },
    rows: number,
    cols: number
  ): void {
    // 1. 回复到达前标签页已关闭：serve 上刚打开的终端没有人用，close 会结束它。
    this.terminalID = opened.terminal_id
    if (this.phase !== "opening") return
    // 2. 进入 running；界面在打开期间排好的行列补发给 serve。
    this.openedExecutorID = opened.executor_id
    this.openedDirectory = opened.directory
    this.reportedRows = rows
    this.reportedCols = cols
    this.phase = "running"
    this.publish()
    this.syncSize()
  }

  // terminal/open 失败（Executor 不可用、目录无效、连接断开）：标签页进入已结束，显示原因
  private handleOpenFailed(error: unknown): void {
    if (this.phase !== "opening") return
    this.markExited(getRpcErrorMessage(error, i18n.t))
  }

  // 向 serve 接入已有的终端，从已收到的输出位置续读；连接没连上时等 handleConnectionState 在连上后再调用
  private attachTerminal(): void {
    // 1. 进入 reconnecting，回放期间 xterm 产生的输入不发给 serve。
    this.phase = "reconnecting"
    this.isReplaying = true
    this.publish()
    if (this.connection.$state.get() !== "connected") return
    // 2. 接入；回复先到，输出后到。
    this.connection
      .call("terminal/attach", {
        terminal_id: this.terminalID,
        cursor: this.receivedCursor,
      })
      .then(
        (attached) => this.handleAttached(attached),
        (error: unknown) => this.handleAttachFailed(error)
      )
  }

  // terminal/attach 成功：xterm 先按 PTY 当前尺寸回放，进入 running
  private handleAttached(attached: TerminalAttachResult): void {
    if (this.phase !== "reconnecting") return
    this.openedExecutorID = attached.executor_id
    this.openedDirectory = attached.directory
    this.terminal.resize(attached.cols, attached.rows)
    this.reportedRows = attached.rows
    this.reportedCols = attached.cols
    this.phase = "running"
    this.publish()
  }

  // terminal/attach 失败：终端已不存在时标为已结束；连接断开时等下次连上；其他原因显示出来
  private handleAttachFailed(error: unknown): void {
    if (this.phase !== "reconnecting") return
    if (isConnectionLostError(error)) return
    if (error instanceof RpcError && error.code === ErrorCodeTerminalNotFound)
      this.markExited(i18n.t("workspace.terminal.endedWhenRestored"))
    else this.markExited(getRpcErrorMessage(error, i18n.t))
  }

  // 处理 serve 推来的终端通知：输出写进 xterm，进程结束后标为已结束；其他终端和其他通知忽略
  private handleNotification(notification: GlobalServerNotification): void {
    switch (notification.method) {
      case "terminal/output":
        if (notification.params.terminal_id !== this.terminalID) return
        this.handleOutput(notification.params)
        return
      case "terminal/exited":
        if (notification.params.terminal_id !== this.terminalID) return
        this.markExited(
          notification.params.error ||
            i18n.t("workspace.terminal.exited", {
              code: notification.params.exit_code,
            })
        )
        return
    }
  }

  // 一段输出：位置已被 serve 丢弃时换空屏幕从最早保留处重放；写进 xterm 并记下续读位置；接入时已有的输出写完后结束回放
  private handleOutput(output: TerminalOutputParams): void {
    // 1. 请求的位置已被丢弃：屏幕上的旧内容接不上，清掉再写。
    if (output.truncated) {
      this.wasTruncated = true
      if (this.receivedCursor > 0) this.terminal.reset()
    }
    // 2. 写入输出；回放的最后一批写完才结束回放，xterm 的自动回答在写入期间产生。
    this.receivedCursor = output.next_cursor
    const isLastReplayed = this.isReplaying && !output.has_more
    this.terminal.write(
      decodeBase64(output.data),
      isLastReplayed ? () => this.handleReplayFinished() : undefined
    )
  }

  // 回放结束，恢复发送输入。xterm 尺寸和 PTY 不同时补发自己的尺寸；尺寸没变但回放被截断时让程序重画
  private handleReplayFinished(): void {
    // 1. 重复的通知、已关闭、连接已断开或已结束的终端忽略。
    if (!this.isReplaying || this.isClosed || this.phase !== "running") return
    this.isReplaying = false
    // 2. 按容器重新排行列并补发尺寸。
    const { reportedRows, reportedCols } = this
    if (this.container) this.fit(this.container)
    this.syncSize()
    // 3. 截断后全屏程序的画面不完整，尺寸没变时没有收到 SIGWINCH，改两次尺寸让它重画。
    const isSizeChanged =
      this.reportedRows !== reportedRows || this.reportedCols !== reportedCols
    if (this.wasTruncated && !isSizeChanged) this.requestRedraw()
    this.wasTruncated = false
  }

  // 先把行数减一，隔 100ms 再改回，让全屏程序收到两次尺寸变化后重画
  private requestRedraw(): void {
    // 1. 减一行。
    const { rows, cols } = this.terminal
    this.connection.notify("terminal/resize", {
      terminal_id: this.terminalID,
      rows: Math.max(rows - 1, 1),
      cols,
    })
    // 2. 隔一会儿改回；这期间界面尺寸变过的，以现在的尺寸为准。
    clearTimeout(this.redrawTimer)
    this.redrawTimer = setTimeout(() => {
      if (this.isClosed || this.phase !== "running") return
      this.connection.notify("terminal/resize", {
        terminal_id: this.terminalID,
        rows: this.terminal.rows,
        cols: this.terminal.cols,
      })
    }, redrawIntervalMs)
  }

  // xterm 产生的输入：Ctrl 按下时把这个字符换成控制字节；回放期间 xterm 的自动回答不发给 serve
  private handleInput(data: string): void {
    // 1. 回放期间的输入是 xterm 对历史输出的自动回答，不是用户输入，丢弃。
    if (this.isReplaying) return
    // 2. 按键条的 Ctrl 只作用于下一次输入。
    let text = data
    if (this.isControlPending) {
      text = getControlCharacter(data) ?? data
      this.isControlPending = false
      this.publish()
    }
    // 3. 发给 serve。
    this.sendInput(textEncoder.encode(text))
  }

  // 把输入发给 serve；终端不在运行时丢弃
  private sendInput(data: Uint8Array): void {
    if (this.phase !== "running" || this.isReplaying) return
    this.connection.notify("terminal/input", {
      terminal_id: this.terminalID,
      data: encodeBase64(data),
    })
  }

  // 容器有大小时按它重新排行列，行列和 serve 记的不同就通知 serve；藏起来的标签页没有大小，不处理；
  // 回放期间不处理：回放的字节要按 PTY 当前尺寸解析，回放结束后再按容器排
  private fit(container: HTMLElement): void {
    if (
      this.isClosed ||
      this.isReplaying ||
      !container.isConnected ||
      container.clientWidth <= 0 ||
      container.clientHeight <= 0
    )
      return
    const size = this.fitAddon.proposeDimensions()
    if (
      size &&
      (size.cols !== this.terminal.cols || size.rows !== this.terminal.rows)
    )
      this.fitAddon.fit()
    this.syncSize()
  }

  // serve 记的行列数和 xterm 不一致时，发 terminal/resize；回放期间 xterm 的行列是 PTY 当前的，不发
  private syncSize(): void {
    const { rows, cols } = this.terminal
    if (
      this.phase !== "running" ||
      this.isReplaying ||
      (rows === this.reportedRows && cols === this.reportedCols)
    )
      return
    this.reportedRows = rows
    this.reportedCols = cols
    this.connection.notify("terminal/resize", {
      terminal_id: this.terminalID,
      rows,
      cols,
    })
  }

  // 状态变化后换新快照并通知界面
  private publish(): void {
    this.snapshot = this.createSnapshot()
    for (const listener of [...this.listeners]) listener()
  }

  private createSnapshot(): TerminalSessionSnapshot {
    return {
      phase: this.phase,
      exitReason: this.exitReason,
      title: this.shellTitle || this.getDefaultTitle(),
      isControlPending: this.isControlPending,
      isWaitingForConnection:
        this.phase === "reconnecting" &&
        this.connection.$state.get() !== "connected",
    }
  }

  // 没有 shell 设的标题时的标题：「Executor · 目录名」，serve 打开终端后以它实际使用的位置为准
  private getDefaultTitle(): string {
    const executorID = this.openedExecutorID || this.executorID || "local"
    const directory = this.openedDirectory || this.directory
    const name = directory.split(/[\\/]/).findLast(Boolean) ?? directory
    return name ? `${executorID} · ${name}` : executorID
  }
}

// 把字节编成 base64，分段编码避免一次展开太多参数
function encodeBase64(data: Uint8Array): string {
  let binary = ""
  for (let start = 0; start < data.length; start += 0x8000)
    binary += String.fromCharCode(...data.subarray(start, start + 0x8000))
  return btoa(binary)
}

function decodeBase64(text: string): Uint8Array {
  return Uint8Array.from(atob(text), (char) => char.charCodeAt(0))
}

// 页面的等宽字体，取自 CSS 变量 --font-mono
function getMonospaceFontFamily(element: HTMLElement): string {
  return (
    getComputedStyle(element).getPropertyValue("--font-mono").trim() ||
    "monospace"
  )
}

// 浅色主题的 ANSI 16 色：xterm 默认色板是为深色背景配的，浅色背景上白色、黄色看不清
const lightAnsiColors = [
  "#000000",
  "#cd3131",
  "#00bc00",
  "#949800",
  "#0451a5",
  "#bc05bc",
  "#0598bc",
  "#555555",
  "#666666",
  "#cd3131",
  "#14ce14",
  "#b5ba00",
  "#0451a5",
  "#bc05bc",
  "#0598bc",
  "#a5a5a5",
]

const darkAnsiColors = [
  "#000000",
  "#cd3131",
  "#0dbc79",
  "#e5e510",
  "#2472c8",
  "#bc3fbc",
  "#11a8cd",
  "#e5e5e5",
  "#666666",
  "#f14c4c",
  "#23d18b",
  "#f5f543",
  "#3b8eea",
  "#d670d6",
  "#29b8db",
  "#e5e5e5",
]

const ansiColorNames = [
  "black",
  "red",
  "green",
  "yellow",
  "blue",
  "magenta",
  "cyan",
  "white",
  "brightBlack",
  "brightRed",
  "brightGreen",
  "brightYellow",
  "brightBlue",
  "brightMagenta",
  "brightCyan",
  "brightWhite",
] as const

// 按页面当前的背景色和文字色生成 xterm 主题；背景偏暗时用深色的 ANSI 色板
export function getTerminalTheme(element: HTMLElement): ITheme {
  const background = getCssColor(element, "--background")
  const foreground = getCssColor(element, "--foreground")
  const isDark =
    0.299 * background[0] + 0.587 * background[1] + 0.114 * background[2] < 128
  const ansiColors = isDark ? darkAnsiColors : lightAnsiColors
  const theme: ITheme = {
    background: toHex(background),
    foreground: toHex(foreground),
    cursor: toHex(foreground),
    cursorAccent: toHex(background),
    selectionBackground: toHex(foreground) + "33",
  }
  ansiColorNames.forEach((name, index) => {
    theme[name] = ansiColors[index]
  })
  return theme
}

let colorContext: CanvasRenderingContext2D | undefined

// 把 CSS 变量的颜色（可能是 oklch 等 xterm 不认识的写法）转成 sRGB
function getCssColor(
  element: HTMLElement,
  token: string
): [number, number, number] {
  // 1. 在元素上量出变量的计算值，再借 canvas 转成 rgb。
  const probe = document.createElement("span")
  probe.style.color = `var(${token})`
  element.append(probe)
  const color = getComputedStyle(probe).color
  probe.remove()
  colorContext ??= document
    .createElement("canvas")
    .getContext("2d", { willReadFrequently: true })!
  colorContext.clearRect(0, 0, 1, 1)
  colorContext.fillStyle = color
  colorContext.fillRect(0, 0, 1, 1)
  const [red, green, blue] = colorContext.getImageData(0, 0, 1, 1).data
  return [red, green, blue]
}

function toHex(color: [number, number, number]): string {
  return "#" + color.map((part) => part.toString(16).padStart(2, "0")).join("")
}
