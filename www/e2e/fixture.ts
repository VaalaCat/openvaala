import type {
  Content,
  ExecutorSettings,
  Message,
} from "../src/lib/api/protocol.gen"
import { spawn } from "node:child_process"
import { once } from "node:events"
import { existsSync } from "node:fs"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import {
  test as base,
  expect,
  type Locator,
  type Page,
  type WebSocketRoute,
} from "@playwright/test"

// 写配置、起 serve、等健康检查通过的最长时间
const fixtureStartTimeout = 60_000

// webuifixture 写出的状态文件
interface FixtureState {
  // serve 的回环地址
  url: string
  token: string
  // serve 的非回环地址，页面在这里是非安全上下文；没有非回环 IPv4 时为空
  lanURL: string
  // 控制接口和假模型的地址，假模型的 base_url 是 control + "/v1"
  control: string
  workspaceDirectory: string
  sandboxDirectory: string
}

// 假模型的一次回复：依次吐出推理、文字（chunkDelayMs 大于 0 时逐字吐，字间隔这么久）和一次工具调用；
// hold 时写完后不结束，直到这次请求被取消；status 非零时返回带 error 原文的 HTTP 错误
export interface ModelReply {
  text?: string
  reasoning?: string
  reasoningChunks?: string[]
  tool?: string
  // 工具参数的 JSON 文本
  arguments?: string
  argumentChunks?: string[]
  chunkDelayMs?: number
  hold?: boolean
  toolDelayMs?: number
  // 工具调用增量刷新后，等待这么久才发送模型结束标记。
  finishDelayMs?: number
  tools?: { name: string; arguments: string }[]
  status?: number
  error?: string
}

// 用例使用的 fixture 控制接口
export class WebUIFixture {
  readonly state: FixtureState

  constructor(state: FixtureState) {
    this.state = state
  }

  // 假模型的 base_url，新增模型服务时填它
  get modelBaseURL(): string {
    return this.state.control + "/v1"
  }

  // 把回复按顺序追加到假模型的队列末尾；队列空时模型回 ok
  async replies(...replies: ModelReply[]): Promise<void> {
    await this.post("/replies", { replies })
  }

  // 清空假模型还没被取走的回复
  async resetReplies(): Promise<void> {
    await this.post("/replies/reset", {})
  }

  // 设置假模型 GET /v1/models 列出的模型 id
  async models(ids: string[]): Promise<void> {
    await this.post("/models", { ids })
  }

  // 在同一端口重启 serve，健康检查通过后返回
  async restart(): Promise<void> {
    await this.post("/restart", {})
  }

  // 预置 conversations 个各一轮的对话（「预置对话 001」起），再建一个 longConversationTurns 轮的长对话
  // （每轮输入「长对话第 N 轮」，回复 ok），全部结束后返回长对话的 ID
  async seed(request: {
    conversations: number
    longConversationTurns: number
  }): Promise<{ longConversationID: string }> {
    return (await this.post("/seed", request)) as {
      longConversationID: string
    }
  }

  // 经真实 runtimeclient 提交复杂 Content，不改浏览器草稿或队列。
  async submitInput(
    conversationID: string,
    content: Content[],
    mode: "normal" | "steer" = "normal"
  ): Promise<{ itemID: string }> {
    return (await this.post("/input", {
      operation: "submit",
      conversationID,
      content,
      mode,
    })) as { itemID: string }
  }

  // 从另一个客户端取回完整输入。
  async retrieveInput(
    conversationID: string,
    itemID: string
  ): Promise<{ content: Content[] }> {
    return (await this.post("/input", {
      operation: "retrieve",
      conversationID,
      itemID,
    })) as { content: Content[] }
  }

  // 读取真实保存的消息，核对 Content 的顺序和字段。
  async messages(conversationID: string): Promise<Message[]> {
    return (
      (await this.post("/input", {
        operation: "messages",
        conversationID,
      })) as { messages: Message[] }
    ).messages
  }

  // 经 conversation/read 核对真实保存的 Executor 设置。
  async executorSettings(conversationID: string): Promise<ExecutorSettings> {
    // 1. 读取对话记录，不读取或修改页面草稿。
    return (await this.post("/input", {
      operation: "executorSettings",
      conversationID,
    })) as ExecutorSettings
  }

  // 受控命令：bash 命令在工作区里等放行文件出现（最多 20 秒）再继续，测试观察完运行中的界面后放行，
  // 不靠固定睡眠，也不会因机器忙而错过运行中的状态
  createCommandGate(name: string) {
    const file = `.gate-${name}`
    const filePath = path.join(this.state.workspaceDirectory, file)
    return {
      // 放进 bash 命令里的等待片段
      wait: `for n in $(seq 200); do [ -e ${file} ] && break; sleep 0.1; done`,
      release: () => writeFile(filePath, ""),
      cleanup: () => rm(filePath, { force: true }),
    }
  }

  // 用 openvaala web 打印的那种带 token 的地址打开页面，等它连上 serve、进入工作区；origin 默认是回环地址。
  // 同一个浏览器里已有保存的工作区时，地址会跟到恢复出来的对话标签页，否则是新对话页
  async openApp(page: Page, origin = this.state.url): Promise<void> {
    await page.goto(
      origin + "/web/v1/#token=" + encodeURIComponent(this.state.token)
    )
    await expect(page).toHaveURL(/\/web\/v1\/s\/[^/]+(?:\/c\/[^/]+)?$/)
  }

  private async post(path: string, body: unknown): Promise<unknown> {
    const response = await fetch(this.state.control + path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
    if (!response.ok)
      throw new Error(`${path}: ${response.status} ${await response.text()}`)
    return response.json()
  }
}

export const test = base.extend<
  { replyQueueReset: void },
  { fixture: WebUIFixture }
>({
  // 每条用例开始前清空回复队列：前一条用例中途失败留下的回复不会被这一条用掉
  replyQueueReset: [
    async ({ fixture }, provide) => {
      await fixture.resetReplies()
      await provide()
    },
    { auto: true },
  ],
  // 每个 worker 起一份自己的 serve 和假模型：worker 之间不共用对话、配置和回复队列。
  fixture: [
    // eslint-disable-next-line no-empty-pattern
    async ({}, provide) => {
      // 1. 启动 globalSetup 编译好的 webuifixture，等它写出状态文件；它先退出或超时则失败。
      //    stdin 接着 worker 进程，worker 被强行结束时 stdin 关闭，fixture 也会退出。
      const fixtureBinary = process.env.WEBUI_FIXTURE_BINARY
      const openvaalaBinary = process.env.OPENVAALA_BINARY
      if (!fixtureBinary || !openvaalaBinary)
        throw new Error("WEBUI_FIXTURE_BINARY or OPENVAALA_BINARY is not set")
      const directory = await mkdtemp(
        path.join(tmpdir(), "openvaala-webui-worker-")
      )
      const statePath = path.join(directory, "state.json")
      const started = spawn(
        fixtureBinary,
        ["--openvaala", openvaalaBinary, "--out", statePath],
        { stdio: ["pipe", "inherit", "inherit"] }
      )
      try {
        const deadline = Date.now() + fixtureStartTimeout
        while (!existsSync(statePath)) {
          if (started.exitCode !== null || started.signalCode !== null)
            throw new Error(`webuifixture exited with ${started.exitCode}`)
          if (Date.now() > deadline)
            throw new Error("webuifixture was not ready in time")
          await new Promise((resolve) => setTimeout(resolve, 50))
        }
        // 2. 交给本 worker 的用例。
        await provide(
          new WebUIFixture(JSON.parse(await readFile(statePath, "utf8")))
        )
      } finally {
        // 3. 发 SIGTERM，等它停掉 serve、删完临时目录。
        if (started.exitCode === null && started.signalCode === null) {
          const exited = once(started, "exit")
          started.kill("SIGTERM")
          await exited
        }
        await rm(directory, { recursive: true, force: true })
      }
    },
    { scope: "worker", timeout: fixtureStartTimeout },
  ],
})

export { expect }

// 检查运行文字的渐变遮罩确实随浏览器帧移动。
export async function expectShimmer(text: Locator): Promise<void> {
  // 1. 读取两帧之间的绘制位置，文字本身保留正常填充。
  await expect(text).toHaveCSS("mask-image", /linear-gradient/)
  const frames = await text.evaluate(async (node) => {
    const samples = []
    for (let index = 0; index < 4; index++) {
      await new Promise<void>((resolve) =>
        requestAnimationFrame(() => resolve())
      )
      const style = getComputedStyle(node)
      samples.push({
        position: style.maskPosition,
        fill: style.webkitTextFillColor,
      })
    }
    return samples
  })
  expect(new Set(frames.map((frame) => frame.position)).size).toBeGreaterThan(1)
  expect(
    frames.every(
      (frame) =>
        frame.fill !== "transparent" && frame.fill !== "rgba(0, 0, 0, 0)"
    )
  ).toBe(true)
}

// 消息区
export function getMessages(page: Page): Locator {
  return page.getByRole("region", { name: "消息" })
}

// 消息区的滚动容器：滚动位置、视口高度从这里读
export function getMessageScroller(page: Page): Locator {
  return page.locator('[data-slot="message-scroller"]')
}

// 输入框
export function getComposer(page: Page): Locator {
  return page.getByRole("textbox", { name: "消息" })
}

// 整页不滚动：只有页面区、消息区这类内部容器滚动，页面里的绝对定位元素不撑出文档高度
export async function expectDocumentNotScrollable(page: Page): Promise<void> {
  expect(
    await page.evaluate(() => {
      const scrollingElement = document.scrollingElement!
      return scrollingElement.scrollHeight - scrollingElement.clientHeight
    })
  ).toBe(0)
}

// 新对话页的工作位置换回 local 的默认根目录：Executor 不是 local 时选 local，目录不为空时清空。
// 新对话页会预填最近对话的位置，同一个 worker 的用例共用一个 serve，预填会来自别的用例留下的对话
export async function chooseDefaultWorkLocation(page: Page): Promise<void> {
  // 1. 工作位置在预填结束后才出现；宽屏的 Executor 是下拉，窄屏是抽屉。
  const executor = page
    .getByRole("combobox", { name: "Executor", exact: true })
    .or(page.getByRole("button", { name: "Executor", exact: true }))
  await expect(executor).toBeVisible()
  if ((await executor.innerText()).trim() !== "local") {
    await executor.click()
    await page
      .getByRole("option", { name: /^local/ })
      .or(
        page
          .getByRole("dialog", { name: "Executor", exact: true })
          .getByRole("button", { name: /^local/ })
      )
      .click()
    await expect(executor).toHaveText("local")
  }
  // 2. 目录留空就是 local 的默认根目录；清空会打开候选，按 Esc 关掉。
  const directory = page.getByRole("combobox", {
    name: "工作目录",
    exact: true,
  })
  if (await directory.inputValue()) {
    await directory.fill("")
    await page.keyboard.press("Escape")
    await expect(page.getByRole("listbox")).toHaveCount(0)
  }
}

// 在新对话页发出第一条消息，等跳到新对话；对话建在 local 的默认根目录里
export async function startConversation(page: Page, text: string) {
  // 1. 不在对话页时等新对话页标题出现，切换中残留的旧对话输入框不会收到文字；再换回默认工作位置。
  if (!/\/c\/[^/]+$/.test(page.url())) {
    await expect(
      page.getByRole("heading", { name: "新对话", level: 1 })
    ).toBeVisible()
    await chooseDefaultWorkLocation(page)
  }
  // 2. 发出第一条消息。
  await startConversationAtCurrentLocation(page, text)
}

// 用新对话页上现在的工作位置发出第一条消息，等跳到新对话；用例自己选好工作位置时用它
export async function startConversationAtCurrentLocation(
  page: Page,
  text: string
) {
  // 1. 输入并发送，等地址换成新对话、消息出现。
  await getComposer(page).fill(text)
  await getComposer(page).press("Enter")
  await expect(page).toHaveURL(/\/c\/[^/]+$/)
  await expect(getMessages(page).getByText(text, { exact: true })).toBeVisible()
}

// 桌面的右栏默认收起：要看右栏页签的用例先用顶栏的开关展开它
export async function openRightPanel(page: Page): Promise<void> {
  const toggle = page.getByRole("button", { name: "切换对话详情" })
  if ((await toggle.getAttribute("aria-pressed")) !== "true")
    await toggle.click()
  await expect(toggle).toHaveAttribute("aria-pressed", "true")
}

// 记下页面发出的 JSON-RPC 请求的方法和对话，按发出的顺序
export function recordRequests(
  page: Page
): { method: string; conversationID?: string }[] {
  const requests: { method: string; conversationID?: string }[] = []
  page.on("websocket", (socket) =>
    socket.on("framesent", ({ payload }) => {
      const request = JSON.parse(String(payload)) as {
        method?: string
        params?: { conversation_id?: string }
      }
      if (request.method)
        requests.push({
          method: request.method,
          conversationID: request.params?.conversation_id,
        })
    })
  )
  return requests
}

// 通过稳定 Item 标识定位包含给定内容的记录，不依赖原始数据操作。
export function getItemRow(inner: Locator): Locator {
  return inner.locator("xpath=ancestor::*[@data-item-key][1]")
}

// 展开本轮连续的过程组，保留工具各自的默认收起状态。
export async function expandTurnProcess(turn: Locator): Promise<void> {
  const groups = turn
    .locator('[data-anchor-key^="group:"] > [data-item-header]')
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
  for (const group of await groups.all())
    if (
      (await group.isVisible()) &&
      (await group.getAttribute("aria-expanded")) === "false"
    )
      await group.click()
}

// 打开本轮已加载原始数据并展开各 Item，不自动加载额外历史。
export async function openTurnRaw(turn: Locator): Promise<Locator> {
  await turn
    .locator("[data-turn-status]")
    .getByRole("button", { name: "调试信息", exact: true })
    .click()
  const raw = turn.page().locator('[data-debug-kind="turn"]')
  await expect(raw).toBeVisible()
  for (const item of await raw
    .locator('[data-json-path][aria-expanded="false"]')
    .all()) {
    const path = JSON.parse((await item.getAttribute("data-json-path"))!) as (
      string | number
    )[]
    if (path.length === 2 && path[0] === "items") await item.press("Enter")
  }
  return raw
}

// 代理页面到 serve 的 WebSocket，让用例决定何时断开、何时允许重连；要在 openApp 之前调用
export async function controlConnection(page: Page) {
  let isHeld = false
  let live: WebSocketRoute | undefined
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    // 断开期间的重连尝试立即失败，页面按退避继续重试。
    if (isHeld) {
      void socket.close()
      return
    }
    live = socket
    socket.connectToServer()
  })
  return {
    // 断开当前连接，之后的重连一直失败，直到调用 allowReconnect 或 reconnect
    async disconnect(): Promise<void> {
      isHeld = true
      await live!.close()
      await expect(
        page.getByRole("button", { name: "重连", exact: true })
      ).toBeVisible()
    },
    // 允许重连：页面按退避自己连上，不碰页面，键盘焦点留在原处
    async allowReconnect(): Promise<void> {
      isHeld = false
      await expect(
        page.getByRole("button", { name: "已连接", exact: true })
      ).toBeVisible({ timeout: 20_000 })
    },
    // 允许重连并点「重连」跳过退避立即重试，等页面回到已连接；点击会把键盘焦点交给按钮
    async reconnect(): Promise<void> {
      isHeld = false
      // 退避等待刚好结束时页面已经自己连上，按钮不在了。
      await page
        .getByRole("button", { name: "重连", exact: true })
        .click({ timeout: 3000 })
        .catch(() => undefined)
      await expect(
        page.getByRole("button", { name: "已连接", exact: true })
      ).toBeVisible()
    },
  }
}

// 在终端里粘贴一段文字：括号粘贴模式开着时，前台程序收到的是 ESC[200~ 文字 ESC[201~
export async function pasteIntoTerminal(
  page: Page,
  text: string
): Promise<void> {
  await page
    .locator(".xterm-helper-textarea:visible")
    .evaluate((node, text) => {
      const data = new DataTransfer()
      data.setData("text/plain", text)
      node.dispatchEvent(
        new ClipboardEvent("paste", {
          clipboardData: data,
          bubbles: true,
          cancelable: true,
        })
      )
    }, text)
}

// 关掉所有终端标签页，结束 serve 上的 shell；终端归 serve 持有，页面关掉后 shell 还在运行，用例结束时用它清理
export async function closeTerminalTabs(page: Page): Promise<void> {
  const closeButtons = page
    .locator('[role="tab"][id^="flexlayout-tabbutton-"]')
    .getByTitle("关闭标签页")
  for (let attempt = 0; attempt < 5; attempt++) {
    if ((await closeButtons.count()) === 0) return
    await closeButtons.last().click()
  }
}
