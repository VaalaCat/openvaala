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
import { test as base, expect, type Locator, type Page } from "@playwright/test"

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

  // 用 openvaala web 打印的那种带 token 的地址打开页面，等它连上 serve、进入新对话页；origin 默认是回环地址
  async openApp(page: Page, origin = this.state.url): Promise<void> {
    await page.goto(
      origin + "/web/v1/#token=" + encodeURIComponent(this.state.token)
    )
    await expect(page).toHaveURL(/\/web\/v1\/s\/[^/]+$/)
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

// 在新对话页发出第一条消息，等跳到新对话
export async function startConversation(page: Page, text: string) {
  // 1. 不在对话页时等新对话页标题出现再输入，切换中残留的旧对话输入框不会收到文字。
  if (!/\/c\/[^/]+$/.test(page.url()))
    await expect(
      page.getByRole("heading", { name: "新对话", level: 1 })
    ).toBeVisible()
  await getComposer(page).fill(text)
  await getComposer(page).press("Enter")
  await expect(page).toHaveURL(/\/c\/[^/]+$/)
  await expect(getMessages(page).getByText(text, { exact: true })).toBeVisible()
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
