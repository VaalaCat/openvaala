import type { Browser, Locator, Page } from "@playwright/test"
import {
  closeTerminalTabs,
  controlConnection,
  expect,
  getComposer,
  getMessages,
  pasteIntoTerminal,
  startConversation,
  test,
  type WebUIFixture,
} from "./fixture"

// 标签栏里的标签按钮
function getTabButtons(page: Page): Locator {
  return page.locator('[role="tab"][id^="flexlayout-tabbutton-"]')
}

// 终端的屏幕：xterm 的 DOM 渲染把每一行画成一个元素
function getScreen(page: Page): Locator {
  return page.locator(".xterm-rows:visible")
}

// 终端结束后右下角的结束原因和「重新启动」
function getExitNotice(page: Page): Locator {
  return page.getByRole("status").filter({ hasText: "重新启动" })
}

// 在终端里输入一行命令并回车；键盘输入落在 xterm 的输入框上
async function runCommand(page: Page, command: string): Promise<void> {
  await page.keyboard.type(command)
  await page.keyboard.press("Enter")
}

// 区域「+」菜单里的一项
async function chooseAreaMenuItem(page: Page, name: string): Promise<void> {
  await page
    .locator(".flexlayout__tabset")
    .first()
    .getByRole("button", { name: "区域操作", exact: true })
    .click()
  await page.getByRole("menuitem", { name, exact: true }).click()
}

// 区域「+」菜单里新开终端，等键盘焦点落进终端
async function openTerminal(page: Page): Promise<void> {
  await chooseAreaMenuItem(page, "新终端")
  await expect(page.locator(".xterm-helper-textarea:visible")).toBeFocused()
}

// 终端里的行列数：在 page 里键入 stty size，等这一次的输出出现；每次用不同的标记，旧输出不会被当成这次的
let sizeReads = 0
async function readTerminalSize(
  page: Page
): Promise<{ rows: number; cols: number }> {
  const tag = `size${++sizeReads}`
  await page.locator(".terminal-viewport:visible").click()
  await runCommand(page, `echo "${tag}=$(stty size)"`)
  const output = new RegExp(`${tag}=(\\d+) (\\d+)`)
  await expect(getScreen(page)).toContainText(output)
  const match = (await getScreen(page).innerText()).match(output)!
  return { rows: Number(match[1]), cols: Number(match[2]) }
}

// 另一个浏览器上下文里的页面打开同一条对话；两个上下文不共用保存的布局
async function openConversationInNewContext(
  browser: Browser,
  fixture: WebUIFixture,
  conversationURL: string,
  viewport = { width: 1280, height: 720 }
) {
  const context = await browser.newContext({ locale: "zh-CN", viewport })
  const other = await context.newPage()
  return {
    context,
    other,
    async open() {
      await fixture.openApp(other)
      await other.goto(conversationURL)
    },
  }
}

test(
  "two_pages_show_each_other_messages_live",
  { tag: "@p1" },
  async ({ page, browser, fixture }) => {
    // 1. 页面 A 建对话；页面 B 在另一个浏览器上下文里打开同一个对话。
    await fixture.openApp(page)
    await startConversation(page, "两个页面")
    await expect(
      getMessages(page).getByText("ok", { exact: true })
    ).toBeVisible()
    const otherContext = await browser.newContext({ locale: "zh-CN" })
    const other = await otherContext.newPage()
    await fixture.openApp(other)
    await other.goto(page.url())
    await expect(
      getMessages(other).getByText("两个页面", { exact: true })
    ).toBeVisible()
    // 2. A 发一条逐字吐出的回复：B 实时显示这条输入和回复。
    await fixture.replies({ text: "两边同时看到", chunkDelayMs: 50 })
    await getComposer(page).fill("从 A 发出")
    await getComposer(page).press("Enter")
    await expect(
      getMessages(other).getByText("从 A 发出", { exact: true })
    ).toBeVisible()
    await expect(getMessages(other).getByText("两边同时看到")).toBeVisible()
    await otherContext.close()
  }
)

test("sidebar_running_is_live_but_results_only_follow_open_conversations", async ({
  page,
  browser,
  fixture,
}) => {
  // 1. A 收到主轮结果；B 只打开新对话页，从未订阅这条对话内容。
  await fixture.openApp(page)
  await startConversation(page, "跨页面运行状态")
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  const ownRow = page.getByRole("link", { name: "跨页面运行状态", exact: true })
  await expect(
    ownRow.getByRole("img", { name: "上次收到的结果：已完成" })
  ).toBeVisible()
  const context = await browser.newContext({ locale: "zh-CN" })
  const observer = await context.newPage()
  const contentSubscriptions: string[] = []
  observer.on("websocket", (socket) =>
    socket.on("framesent", ({ payload }) => {
      const request = JSON.parse(payload.toString()) as { method?: string }
      if (
        request.method === "conversation/subscribe" ||
        request.method === "session/subscribe"
      )
        contentSubscriptions.push(request.method)
    })
  )
  try {
    await fixture.openApp(observer)
    const row = observer.getByRole("link", {
      name: "跨页面运行状态",
      exact: true,
    })
    await expect(row.getByRole("img", { name: "暂无已知结果" })).toBeVisible()
    await fixture.replies({ text: "状态流仍在工作", hold: true })
    await getComposer(page).fill("开始下一轮")
    await getComposer(page).press("Enter")
    await expect(
      getMessages(page).getByText("状态流仍在工作", { exact: true })
    ).toBeVisible()
    const dot = row.getByRole("img", { name: "运行中", exact: true })
    await expect(dot).toBeVisible()
    await expect(dot).toHaveCSS("width", "8px")
    const ripple = dot.locator('[data-slot="status-ripple"]')
    await expect(ripple).toBeVisible()
    // 两种系统动画偏好下，运行中的状态点都持续播放波纹。
    for (const reducedMotion of ["no-preference", "reduce"] as const) {
      await observer.emulateMedia({ reducedMotion })
      const frames = await ripple.evaluate(async (node) => {
        const samples: string[] = []
        for (let index = 0; index < 5; index++) {
          await new Promise<void>((resolve) =>
            requestAnimationFrame(() => resolve())
          )
          const style = getComputedStyle(node)
          samples.push(`${style.transform}/${style.opacity}`)
        }
        return samples
      })
      expect(new Set(frames).size).toBeGreaterThan(1)
    }
    await expect(dot).toHaveCSS("transform", "none")
    await expect(dot).toBeVisible()

    // 2. 停蓝不能推断结束结果；A 已收到中断，B 仍没有本地结果。
    await page.getByRole("button", { name: "中断", exact: true }).click()
    await expect(
      ownRow.getByRole("img", { name: "上次收到的结果：已中断" })
    ).toBeVisible()
    await expect(row.getByRole("img", { name: "暂无已知结果" })).toBeVisible()
    expect(contentSubscriptions).toEqual([])

    // 3. 正常打开才顺带记录结果；切走并刷新后清空，不补查历史终态。
    await row.click()
    await expect(
      getMessages(observer).getByText("状态流仍在工作", { exact: true })
    ).toBeVisible()
    await expect(
      row.getByRole("img", { name: "上次收到的结果：已中断" })
    ).toBeVisible()
    await observer.getByRole("link", { name: "新对话", exact: true }).click()
    await expect(
      row.getByRole("img", { name: "上次收到的结果：已中断" })
    ).toBeVisible()
    await observer.reload()
    await expect(row.getByRole("img", { name: "暂无已知结果" })).toBeVisible()
  } finally {
    await context.close()
  }
})

test(
  "pending_snapshot_survives_refresh_collapse_and_other_client_retrieval",
  { tag: "@p0" },
  async ({ page, browser, fixture }) => {
    await fixture.replies({ text: "两页队列运行中", hold: true })
    await fixture.openApp(page)
    await startConversation(page, "共享队列")
    await expect(getMessages(page).getByText("两页队列运行中")).toBeVisible()
    const otherContext = await browser.newContext({ locale: "zh-CN" })
    const other = await otherContext.newPage()
    await fixture.openApp(other)
    await other.goto(page.url())
    await getComposer(page).fill("从 A 排队")
    await page.getByRole("button", { name: "排队", exact: true }).click()
    const pending = other.getByRole("region", { name: "待处理", exact: true })
    await expect(pending.getByText("从 A 排队", { exact: true })).toBeVisible()
    await other.reload()
    await expect(pending.getByText("从 A 排队", { exact: true })).toBeVisible()
    await pending.getByRole("button", { name: /^待处理 \d/ }).click()
    await getComposer(page).fill("从 A 再排队")
    await page.getByRole("button", { name: "排队", exact: true }).click()
    await expect(
      pending.getByRole("button", { name: /^待处理 2/ })
    ).toHaveAttribute("aria-expanded", "false")
    await pending.getByRole("button", { name: /^待处理 2/ }).click()
    await getComposer(other).fill("会被覆盖的草稿")
    await pending
      .locator("[data-pending-input]")
      .filter({ hasText: "从 A 排队" })
      .getByRole("button", { name: "取回修改" })
      .click()
    await expect(getComposer(other)).toHaveValue("从 A 排队")
    await expect(
      page
        .getByRole("region", { name: "待处理", exact: true })
        .getByText("从 A 排队", { exact: true })
    ).toHaveCount(0)
    await otherContext.close()
    await page.getByRole("button", { name: "中断", exact: true }).click()
    await expect(
      getMessages(page).getByText("从 A 再排队", { exact: true })
    ).toBeVisible()
  }
)

test.describe("shared terminals", () => {
  // 终端归 serve 持有，页面关掉后 shell 还在运行；用例结束时关掉标签页来结束它们
  test.afterEach(async ({ page }) => {
    await closeTerminalTabs(page).catch(() => undefined)
  })

  test("terminal_appears_on_the_other_page", async ({
    page,
    browser,
    fixture,
  }) => {
    // 1. B 页打开同一条对话，只有主对话。
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "两页终端")
    const second = await openConversationInNewContext(
      browser,
      fixture,
      page.url()
    )
    try {
      await second.open()
      await expect(getTabButtons(second.other)).toHaveCount(1)
      // 2. A 页开终端：B 页立即出现终端标签页，不抢当前选中。
      await openTerminal(page)
      await expect(getTabButtons(second.other)).toHaveCount(2)
      await expect(getTabButtons(second.other).first()).toHaveAttribute(
        "aria-selected",
        "true"
      )
      // 3. 两边都能输入，看到同一个 shell 的输出。
      await getTabButtons(second.other).last().click()
      await second.other.locator(".terminal-viewport:visible").click()
      await runCommand(second.other, "echo from-second-$((6*7))")
      await expect(getScreen(page)).toContainText("from-second-42")
      await page.locator(".terminal-viewport:visible").click()
      await runCommand(page, "echo from-first-$((1+1))")
      await expect(getScreen(second.other)).toContainText("from-first-2")
      await closeTerminalTabs(second.other)
    } finally {
      await second.context.close()
    }
  })

  test("typing_decides_the_terminal_size", async ({
    page,
    browser,
    fixture,
  }) => {
    // 1. 宽页打开终端，窄页接入同一个终端。
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "终端尺寸")
    const second = await openConversationInNewContext(
      browser,
      fixture,
      page.url(),
      { width: 900, height: 500 }
    )
    try {
      await second.open()
      await openTerminal(page)
      await expect(getTabButtons(second.other)).toHaveCount(2)
      await getTabButtons(second.other).last().click()
      // 2. 谁最后打字，终端就是谁的尺寸：窄页打字后比宽页小，宽页再打字回到原来的尺寸。
      const wide = await readTerminalSize(page)
      const narrow = await readTerminalSize(second.other)
      expect(narrow.cols).toBeLessThan(wide.cols)
      expect(narrow.rows).toBeLessThan(wide.rows)
      expect(await readTerminalSize(page)).toEqual(wide)
      expect(await readTerminalSize(second.other)).toEqual(narrow)
      await closeTerminalTabs(second.other)
    } finally {
      await second.context.close()
    }
  })

  test("closing_a_tab_ends_it_everywhere", async ({
    page,
    browser,
    fixture,
  }) => {
    // 1. 两页都有终端标签页。
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "关终端")
    await openTerminal(page)
    const second = await openConversationInNewContext(
      browser,
      fixture,
      page.url()
    )
    try {
      await second.open()
      await expect(getTabButtons(second.other)).toHaveCount(2)
      await getTabButtons(second.other).last().click()
      // 2. A 页关掉标签页：B 页的终端显示已结束，两边的标签页都不会自动回来。
      await closeTerminalTabs(page)
      await expect(getTabButtons(page)).toHaveCount(1)
      await expect(getExitNotice(second.other)).toBeVisible()
      await page.waitForTimeout(1500)
      await expect(getTabButtons(page)).toHaveCount(1)
      await expect(getTabButtons(second.other)).toHaveCount(2)
      await expect(getExitNotice(second.other)).toBeVisible()
    } finally {
      await second.context.close()
    }
  })

  test("reset_keeps_modes_on_lagging_page", async ({
    page,
    browser,
    fixture,
  }) => {
    // 1. 终端打开括号粘贴模式；B 页的连接被代理，用例可以让它收不到 serve 的消息。
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "两页重置")
    await openTerminal(page)
    await runCommand(page, "printf '\\033[?2004h'")
    const second = await openConversationInNewContext(
      browser,
      fixture,
      page.url()
    )
    try {
      let isLagging = false
      const heldMessages: (string | Buffer)[] = []
      let releaseHeldMessages = () => {}
      await second.other.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
        const server = socket.connectToServer()
        socket.onMessage((message) => server.send(message))
        server.onMessage((message) => {
          if (isLagging) heldMessages.push(message)
          else socket.send(message)
        })
        releaseHeldMessages = () => {
          isLagging = false
          for (const message of heldMessages.splice(0)) socket.send(message)
        }
      })
      await second.open()
      await expect(getTabButtons(second.other)).toHaveCount(2)
      await getTabButtons(second.other).last().click()
      // 2. B 页收不到输出时 A 页输出一行并重置：A 页清屏，B 页还没收到任何东西。
      isLagging = true
      await page.locator(".terminal-viewport:visible").click()
      await runCommand(page, "echo lagging-$((6*7))")
      await expect(getScreen(page)).toContainText("lagging-42")
      await page.waitForTimeout(300)
      await expect(getScreen(second.other)).not.toContainText("lagging-42")
      await chooseAreaMenuItem(page, "重置终端")
      await expect(getScreen(page)).not.toContainText("lagging-42")
      // 3. B 页追上后一起清屏，两页的括号粘贴模式都保留。
      releaseHeldMessages()
      await page.locator(".terminal-viewport:visible").click()
      await runCommand(page, "cat -v")
      await pasteIntoTerminal(page, "paste-a")
      await pasteIntoTerminal(second.other, "paste-b")
      for (const each of [page, second.other])
        for (const text of ["paste-a", "paste-b"])
          await expect(getScreen(each)).toContainText(`^[[200~${text}^[[201~`)
      for (const each of [page, second.other])
        await expect(getScreen(each)).not.toContainText("lagging-42")
      await page.keyboard.press("Control+C")
      await closeTerminalTabs(second.other)
    } finally {
      await second.context.close()
    }
  })

  test("closed_while_offline_comes_back", async ({ page, fixture }) => {
    // 1. 终端里有一行输出，页面断线。
    const connection = await controlConnection(page)
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "断线关终端")
    await openTerminal(page)
    await runCommand(page, "echo kept-$((6*7))")
    await expect(getScreen(page)).toContainText("kept-42")
    await connection.disconnect()
    // 2. 断线期间关掉标签页：terminal/close 发不出去，shell 还在运行。
    await closeTerminalTabs(page)
    await expect(getTabButtons(page)).toHaveCount(1)
    // 3. 重连后运行列表里还有这个终端，标签页回来并接回原来的屏幕。
    await connection.reconnect()
    await expect(getTabButtons(page)).toHaveCount(2)
    await getTabButtons(page).last().click()
    await expect(getScreen(page)).toContainText("kept-42")
    await page.locator(".terminal-viewport:visible").click()
    await runCommand(page, "echo back-$((1+1))")
    await expect(getScreen(page)).toContainText("back-2")
  })
})
