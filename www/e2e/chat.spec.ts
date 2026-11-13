import {
  expect,
  expectShimmer,
  expandTurnProcess,
  getComposer,
  getFirstVisibleTurn,
  getMessages,
  getMessageScroller,
  getSmallestMountedTurn,
  openRightPanel,
  openTurnRaw,
  startConversation,
  test,
  type ModelReply,
} from "./fixture"
import type { Locator, Page } from "@playwright/test"
import type {
  ListItemsResult,
  ListTurnsResult,
} from "../src/lib/api/protocol.gen"

// 回到消息区顶部阅读：消息区不再贴底跟随，之后的点击不必等跟随滚动的动画结束
async function readFromTop(page: Page): Promise<void> {
  await getMessageScroller(page).evaluate((node) => {
    node.scrollTop = 0
  })
}

// 在已有对话里发送一条消息，等它出现在消息区
async function sendMessage(page: Page, text: string): Promise<void> {
  await getComposer(page).fill(text)
  await getComposer(page).press("Enter")
  await expect(getMessages(page).getByText(text, { exact: true })).toBeVisible()
}

test(
  "send_streams_markdown_and_keeps_history",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    // 1. 排一段逐字吐出的 Markdown 回复，在新对话页发送。
    await fixture.replies({
      text: "# 流式标题\n\n这是一段**加粗**的回复。\n\n- 第一项\n- 第二项\n\n回复到这里结束。",
      chunkDelayMs: 30,
    })
    await fixture.openApp(page)
    await startConversation(page, "你好")
    // 2. 标题先出现，侧栏列表里已经有这个对话，结尾还没吐出来；之后整段按 Markdown 显示。
    const messages = getMessages(page)
    await expect(
      messages.getByRole("heading", { name: "流式标题" })
    ).toBeVisible()
    await expect(page.getByRole("link", { name: /^你好/ })).toBeVisible()
    await expect(messages.getByText("回复到这里结束。")).toBeHidden()
    await expect(messages.getByText("回复到这里结束。")).toBeVisible()
    await expect(messages.getByText("加粗", { exact: true })).toHaveCSS(
      "font-weight",
      /^[6-9]00$/
    )
    await expect(
      messages.getByRole("listitem").filter({ hasText: "第二项" })
    ).toBeVisible()
    // 3. 刷新后历史还在。
    await page.reload()
    await expect(
      messages.getByRole("heading", { name: "流式标题" })
    ).toBeVisible()
    await expect(messages.getByText("你好", { exact: true })).toBeVisible()
    await expect(messages.getByText("回复到这里结束。")).toBeVisible()
  }
)

test("icon_help_and_truncated_title_tooltips_follow_real_state_without_native_title", async ({
  page,
  fixture,
}) => {
  // 1. 新对话页的图标按钮：悬停和键盘聚焦都出提示，不叠加原生 title。
  await page.setViewportSize({ width: 1280, height: 844 })
  await fixture.openApp(page)
  const addImage = page.getByRole("button", { name: "添加图片", exact: true })
  await addImage.hover()
  const tooltip = page.locator('[data-slot="tooltip-content"]:visible')
  await expect(tooltip).toHaveText("添加图片")
  await expect(tooltip).toHaveCSS("font-size", "12px")
  await expect(tooltip).toHaveCSS("line-height", "16px")
  await expect(addImage).not.toHaveAttribute("title")
  await page.mouse.move(0, 0)
  await expect(tooltip).toBeHidden()
  await addImage.focus()
  await expect(tooltip).toHaveText("添加图片")
  await page.keyboard.press("Escape")
  await expect(tooltip).toBeHidden()
  await expect(addImage).toBeFocused()
  await expect(page.locator("button button")).toHaveCount(0)

  // 2. 有回复后，复制结果入口同样只有提示、没有原生 title。
  await startConversation(page, "短标题")
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  const copy = getMessages(page).getByRole("button", {
    name: "复制结果",
    exact: true,
  })
  await copy.hover()
  await expect(tooltip).toHaveText("复制结果")
  await expect(copy).not.toHaveAttribute("title")

  // 3. 没被截断的标题没有提示；改成超长标题并缩窄到真被截断才有提示，放宽后提示消失。
  const navigation = page.getByRole("navigation", { name: "当前位置" })
  const short = navigation.getByRole("button", { name: "短标题", exact: true })
  await short.hover()
  await expect(tooltip).toHaveCount(0)
  const conversationID = page.url().split("/c/")[1]
  const row = page
    .getByRole("listitem")
    .filter({ has: page.locator(`a[href*="/c/${conversationID}"]`) })
    .filter({ has: page.getByRole("button", { name: /的更多操作/ }) })
  await row.getByRole("button", { name: /的更多操作/ }).click()
  await page.getByRole("menuitem", { name: "改名", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "改名", exact: true })
  const title = "真实会话标题截断检查".repeat(3)
  await dialog.getByRole("textbox", { name: "标题" }).fill(title)
  await dialog.getByRole("button", { name: "保存", exact: true }).click()
  await expect(dialog).toBeHidden()
  const renamed = navigation.getByRole("button", { name: title, exact: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await renamed.hover()
  await expect(tooltip).toHaveText(title)
  await expect(renamed).not.toHaveAttribute("title")
  await page.setViewportSize({ width: 1280, height: 844 })
  await expect(tooltip).toHaveCount(0)
  await renamed.hover()
  await expect(tooltip).toHaveCount(0)
})

test("tooltip_waits_600ms_cancels_and_sidebar_tooltip_hits_only_time_text", async ({
  page,
  fixture,
}) => {
  // 1. 真实回复提供两个独立图标入口，浏览器时钟控制鼠标停留。
  await fixture.openApp(page)
  await startConversation(page, "悬停计时与侧栏提示")
  const copy = getMessages(page).getByRole("button", {
    name: "复制结果",
    exact: true,
  })
  await expect(copy).toBeVisible()
  const addImage = page.getByRole("button", { name: "添加图片", exact: true })
  const tooltip = page.locator('[data-slot="tooltip-content"]:visible')
  await page.clock.install()
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000))

  // 2. 离开会取消待显示提示。
  await copy.hover()
  await page.clock.runFor(500)
  await expect(tooltip).toHaveCount(0)
  await page.mouse.move(0, 0)
  await page.clock.runFor(700)
  await expect(tooltip).toHaveCount(0)

  // 3. 跨目标不继承前一个入口的等待时间。
  await copy.hover()
  await page.clock.runFor(500)
  await expect(tooltip).toHaveCount(0)
  await page.clock.runFor(99)
  await expect(tooltip).toHaveCount(0)
  await page.clock.runFor(1)
  await expect(tooltip).toHaveText("复制结果")
  await addImage.hover()
  await page.clock.runFor(599)
  await expect(tooltip).toHaveCount(0)
  await page.clock.runFor(1)
  await expect(tooltip).toHaveText("添加图片")
  await addImage.click()
  await page.clock.runFor(700)
  await expect(tooltip).toHaveCount(0)

  // 4. 点击也会取消待显示提示。
  await copy.hover()
  await page.clock.runFor(500)
  await expect(tooltip).toHaveCount(0)
  await copy.click()
  await page.clock.runFor(700)
  await expect(tooltip).toHaveCount(0)

  // 5. 当前对话时间文字有日期提示，右侧空白和更多按钮没有提示。
  const link = page.getByRole("link", { name: /^悬停计时与侧栏提示/ })
  const row = link.locator("xpath=ancestor::li[1]")
  const time = link.locator('[data-slot="tooltip-trigger"]').last()
  const more = row.getByRole("button", { name: /的更多操作/ })
  await time.hover()
  await page.clock.runFor(600)
  await expect(tooltip).toContainText(/\d{4}/)
  const timeBounds = (await time.boundingBox())!
  const linkBounds = (await link.boundingBox())!
  expect(timeBounds.width).toBeLessThan(linkBounds.width / 2)
  await page.mouse.move(
    timeBounds.x + timeBounds.width + 16,
    timeBounds.y + timeBounds.height / 2
  )
  await page.clock.runFor(700)
  await expect(tooltip).toHaveCount(0)
  await more.hover()
  await page.clock.runFor(700)
  await expect(tooltip).toHaveCount(0)

  // 6. 键盘打开菜单，入口保持选中背景，操作文字和浮层有独立层级；浅色和深色下都成立。
  await more.focus()
  await more.press("Enter")
  await page.clock.runFor(300)
  const menu = page.getByRole("menu")
  await expect(menu).toBeVisible()
  await expect(tooltip).toHaveCount(0)
  await page.mouse.move(0, 0)
  await expect(link).toHaveAttribute("aria-current", "page")
  const rename = menu.getByRole("menuitem", { name: "改名", exact: true })
  for (const colorScheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme })
    await expect
      .poll(() =>
        more.evaluate((node) => {
          const button = getComputedStyle(node).backgroundColor
          const row = getComputedStyle(node.closest("li")!).backgroundColor
          return button !== "rgba(0, 0, 0, 0)" && button !== row
        })
      )
      .toBe(true)
    await rename.hover()
    await page.clock.runFor(700)
    await expect(tooltip).toHaveCount(0)
    await expect(rename).not.toHaveCSS("background-color", "rgba(0, 0, 0, 0)")
    await page.mouse.move(0, 0)
  }
  await page.keyboard.press("Escape")
  await page.clock.runFor(300)
  await expect(menu).toBeHidden()
  await expect(more).toBeFocused()

  // 7. 从新对话页点击时间文字先导航，不补弹旧目标的日期提示。
  const conversationURL = page.url()
  await page.clock.resume()
  await fixture.openApp(page)
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000))
  await time.hover()
  await page.clock.runFor(500)
  await expect(tooltip).toHaveCount(0)
  await time.click()
  await expect(page).toHaveURL(conversationURL)
  await page.clock.runFor(700)
  await expect(tooltip).toHaveCount(0)
})

test("ime_enter_does_not_send", { tag: "@p0" }, async ({ page, fixture }) => {
  // 1. 记下页面发出的每个 conversation/submitInput 请求。
  const submitFrames: string[] = []
  page.on("websocket", (socket) =>
    socket.on("framesent", (frame) => {
      if (
        typeof frame.payload === "string" &&
        frame.payload.includes('"conversation/submitInput"')
      )
        submitFrames.push(frame.payload)
    })
  )
  // 2. 在新对话页输入后派发组字中的 Enter：不发送，文字留在输入框里。
  await fixture.openApp(page)
  const composer = getComposer(page)
  await composer.fill("输入法组字中")
  await composer.evaluate((element) =>
    element.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "Enter",
        isComposing: true,
        bubbles: true,
      })
    )
  )
  await expect(composer).toHaveValue("输入法组字中")
  // 3. 补一个字再按真正的 Enter：发出的是补字后的全文，而且只发了这一次。组字中的 Enter 要是发送了，
  //    发出的会是补字前的文字，后一次 Enter 也会因为上一次还在提交而被忽略。
  await composer.press("End")
  await composer.pressSequentially("！")
  await composer.press("Enter")
  await expect(page).toHaveURL(/\/c\/[^/]+$/)
  await expect(
    getMessages(page).getByText("输入法组字中！", { exact: true })
  ).toBeVisible()
  expect(submitFrames).toHaveLength(1)
})

test("turn_usage_aligns_compact_and_small_values_and_opens_full_numbers", async ({
  page,
  fixture,
}) => {
  // 1. 九次真实模型调用累计出 1080 输入和 36 输出。
  await fixture.replies(
    ...Array.from({ length: 8 }, () => ({
      tool: "bash",
      arguments: JSON.stringify({ command: "printf usage-check" }),
    })),
    { text: "用量累计完成" }
  )
  await fixture.openApp(page)
  await startConversation(page, "累计用量对齐")
  const messages = getMessages(page)
  await expect(
    messages.getByText("用量累计完成", { exact: true })
  ).toBeVisible()
  const divider = messages.locator('[data-turn-status="completed"]').last()
  const compact = divider.getByRole("button", {
    name: "完整数值：1,080",
    exact: true,
  })
  const small = divider.getByText("36", { exact: true })
  // 2. 桌面和窄屏都保持同一行内高度；键盘打开完整整数。
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 844 })
    await expect(compact).toHaveText("1.1K")
    await expect(compact).toHaveCSS("height", "20px")
    await expect(small).toHaveCSS("height", "20px")
    expect((await compact.boundingBox())!.y).toBe(
      (await small.boundingBox())!.y
    )
    await compact.focus()
    await compact.press("Enter")
    await expect(
      page.getByRole("dialog", { name: "完整数值：1,080", exact: true })
    ).toContainText("1,080")
    await page.keyboard.press("Escape")
    await expect(compact).toBeFocused()
  }
  // 3. 右侧轮次用量与展开原始记录的触发器分别操作。
  await page.setViewportSize({ width: 1280, height: 844 })
  await openRightPanel(page)
  await page.getByRole("tab", { name: "Turn", exact: true }).click()
  const turns = page.getByRole("tabpanel", { name: "Turn", exact: true })
  const fullValue = turns.getByRole("button", {
    name: "完整数值：1,080",
    exact: true,
  })
  const disclosure = turns.getByRole("button", { name: "已完成", exact: true })
  await expect(disclosure).toHaveAttribute("aria-expanded", "false")
  await fullValue.click()
  await expect(
    page.getByRole("dialog", { name: "完整数值：1,080", exact: true })
  ).toContainText("1,080")
  await expect(disclosure).toHaveAttribute("aria-expanded", "false")
  await page.keyboard.press("Escape")
  await disclosure.click()
  await expect(disclosure).toHaveAttribute("aria-expanded", "true")
})

test("turn_divider_lines_and_item_actions_align", async ({ page, fixture }) => {
  // 1. 同一对话里依次生成完成、中断、失败三轮；仅读取历史时放大返回的耗时与用量。
  await page.setViewportSize({ width: 320, height: 844 })
  let enlargeHistory = false
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    const methods = new Map<number, string>()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message))
      methods.set(request.id, request.method)
      server.send(message)
    })
    server.onMessage((message) => {
      const response = JSON.parse(String(message))
      if (enlargeHistory && response.result) {
        if (methods.get(response.id) === "session/turns/get") {
          for (const turn of (response.result as ListTurnsResult).turns) {
            turn.usage = {
              ...turn.usage!,
              input: 123_456_789,
              output: 98_765_432,
            }
          }
        }
        if (methods.get(response.id) === "session/items/list") {
          for (const { item } of (response.result as ListItemsResult).items) {
            if (!item) continue
            item.updated_at =
              1_700_000_000_000 +
              (item.message?.from?.kind === "user"
                ? 0
                : (999 * 60 + 59) * 60_000)
          }
        }
      }
      socket.send(JSON.stringify(response))
    })
  })
  const alignmentTool: ModelReply = {
    tool: "bash",
    arguments: JSON.stringify({ command: "printf alignment-check" }),
  }
  await fixture.replies(
    alignmentTool,
    { text: "分隔行布局结果" },
    alignmentTool,
    { text: "等待用户中断", hold: true },
    alignmentTool,
    { status: 400, error: "divider-alignment-failure" }
  )
  const turns = [
    { status: "completed", input: "分隔行与按钮右侧对齐：完成" },
    { status: "interrupted", input: "分隔行与按钮右侧对齐：中断" },
    { status: "failed", input: "分隔行与按钮右侧对齐：失败" },
  ] as const
  await fixture.openApp(page)
  const messages = getMessages(page)
  await startConversation(page, turns[0].input)
  await expect(messages.locator('[data-turn-status="completed"]')).toBeVisible()
  await sendMessage(page, turns[1].input)
  await expect(
    messages.getByText("等待用户中断", { exact: true })
  ).toBeVisible()
  await page.getByRole("button", { name: "中断", exact: true }).click()
  await expect(
    messages.locator('[data-turn-status="interrupted"]')
  ).toBeVisible()
  await sendMessage(page, turns[2].input)
  await expect(messages.locator('[data-turn-status="failed"]')).toBeVisible()
  // 正常中断不展示取消原文，真实失败继续展示原因；刷新前后表现相同。
  for (const reload of [false, true]) {
    if (reload) await page.reload()
    await expect(
      messages.locator('[data-turn-status="interrupted"]')
    ).toBeAttached()
    await expect(
      messages.getByText("context canceled", { exact: true })
    ).toHaveCount(0)
    await expect(messages.locator('[data-turn-status="failed"]')).toContainText(
      "divider-alignment-failure"
    )
  }
  enlargeHistory = true
  await page.reload()
  await expect(messages.locator("[data-turn-id]")).toHaveCount(3)

  // 2. 两侧线同高、等宽且至少 24px，整轮调试入口仍夹在两线之间；
  //    图标从各层标题左边缘开始，调试和重来按钮对齐用户气泡与消息轮次右边缘。
  const expectTurnAligned = async (index: number) => {
    const { status, input } = turns[index]
    const turn = messages.locator("[data-turn-id]").nth(index)
    const divider = turn.locator(`[data-turn-status="${status}"]`)
    await expect(divider).toContainText("999")
    await divider.scrollIntoViewIfNeeded()
    const lines = divider.locator('[data-slot="separator"]')
    await expect(lines).toHaveCount(2)
    const left = (await lines.first().boundingBox())!
    const right = (await lines.last().boundingBox())!
    expect(left.width).toBeGreaterThanOrEqual(24)
    expect(right.width).toBeGreaterThanOrEqual(24)
    expect(Math.abs(left.width - right.width)).toBeLessThanOrEqual(1)
    expect(Math.abs(left.y - right.y)).toBeLessThanOrEqual(1)
    const turnDebug = (await divider
      .getByRole("button", { name: "调试信息", exact: true })
      .boundingBox())!
    expect(turnDebug.x).toBeGreaterThan(left.x + left.width)
    expect(turnDebug.x + turnDebug.width).toBeLessThan(right.x)
    for (const area of [messages, divider, page.locator("body")])
      expect(
        await area.evaluate((node) => node.scrollWidth <= node.clientWidth)
      ).toBe(true)

    await expandTurnProcess(turn)
    for (const header of await turn
      .locator("[data-item-header]:visible")
      .all()) {
      const headerBounds = (await header.boundingBox())!
      const iconBounds = (await header
        .locator("[data-item-icon]")
        .boundingBox())!
      expect(Math.abs(iconBounds.x - headerBounds.x)).toBeLessThanOrEqual(1)
    }
    const bubbleBounds = (await turn
      .getByText(input, { exact: true })
      .locator("..")
      .boundingBox())!
    const turnBounds = (await turn.boundingBox())!
    const rightEdge = turnBounds.x + turnBounds.width
    expect(
      Math.abs(bubbleBounds.x + bubbleBounds.width - rightEdge)
    ).toBeLessThanOrEqual(1)
    const rewind = turn.getByRole("button", {
      name: "从这里重来",
      exact: true,
    })
    const itemDebug = turn
      .locator("[data-item-key] [data-item-header]")
      .getByRole("button", { name: "调试信息", exact: true })
    expect(await itemDebug.count()).toBeGreaterThan(0)
    for (const button of [rewind, ...(await itemDebug.all())]) {
      const bounds = (await button.boundingBox())!
      expect(bounds.width).toBe(32)
      expect(bounds.height).toBe(32)
      expect(Math.abs(bounds.x + bounds.width - rightEdge)).toBeLessThanOrEqual(
        1
      )
    }
  }

  // 3. 箭头有独立的悬停与读屏说明，缩略用量仍可用键盘打开完整值。
  const expectUsageReadable = async (index: number) => {
    const divider = messages
      .locator("[data-turn-id]")
      .nth(index)
      .locator(`[data-turn-status="${turns[index].status}"]`)
    for (const usage of [
      { label: "输入", arrow: "↑", compact: "123.5M", full: "123,456,789" },
      { label: "输出", arrow: "↓", compact: "98.8M", full: "98,765,432" },
    ]) {
      const arrow = divider.getByRole("img", { name: usage.label, exact: true })
      await expect(arrow).toHaveText(usage.arrow)
      await arrow.hover()
      await expect(
        page.locator('[data-slot="tooltip-content"]:visible')
      ).toHaveText(usage.label)
      await expect(arrow).not.toHaveAttribute("title")
      await page.mouse.move(0, 0)
      const valueName = `完整数值：${usage.full}`
      const value = divider.getByRole("button", {
        name: valueName,
        exact: true,
      })
      await expect(value).toHaveText(usage.compact)
      await value.focus()
      await value.press("Enter")
      await expect(
        page.getByRole("dialog", { name: valueName, exact: true })
      ).toContainText(usage.full)
      await page.keyboard.press("Escape")
      await expect(value).toBeFocused()
    }
  }

  // 4. 窄屏逐轮核对三种终态和用量，桌面再核对一轮已完成的。
  await expectTurnAligned(0)
  await expectUsageReadable(0)
  await expectTurnAligned(1)
  await expectTurnAligned(2)
  await page.setViewportSize({ width: 1280, height: 844 })
  await expectTurnAligned(0)
  const raw = await openTurnRaw(messages.locator("[data-turn-id]").nth(1))
  await expect(raw).toContainText("context canceled")
})

test("disclosure_lines_follow_icon_centers_and_preserve_reading", async ({
  page,
  fixture,
}) => {
  // 1. 真实推理、中途回复与两种工具生成独立详情和嵌套过程组。
  await page.setViewportSize({ width: 1280, height: 844 })
  await fixture.replies(
    {
      reasoning: "逐项核对竖线和图标中心",
      text: "中途正文起点保持原位",
      tool: "bash",
      arguments: JSON.stringify({
        command: "printf line-check > line-check.txt",
      }),
    },
    {
      tool: "read_file",
      arguments: JSON.stringify({ path: "line-check.txt" }),
    },
    {
      text: Array.from(
        { length: 35 },
        (_, index) => `阅读位置段落 ${index}。`
      ).join("\n\n"),
    }
  )
  await fixture.openApp(page)
  await startConversation(page, "展开竖线对齐")
  const messages = getMessages(page)
  const scroller = getMessageScroller(page)
  const turn = messages.locator("[data-turn-id]").last()
  await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()
  await readFromTop(page)
  await expandTurnProcess(turn)
  const toggleSelector =
    'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
  const itemHeaders = turn.locator("[data-item-key] [data-item-header]:visible")
  const groupHeaders = turn.locator(
    '[data-anchor-key^="group:"] > [data-item-header]'
  )
  await expect(itemHeaders).toHaveCount(4)
  for (const header of await itemHeaders.all())
    await header.locator(toggleSelector).click()

  // 2. 读取实际边框和内容矩形；聚合下缩进17px，详情正文仍缩进33px；桌面和窄屏都成立。
  const expectLinesFollowIcons = async () => {
    for (const [kind, headers] of [
      ["group", groupHeaders],
      ["item", itemHeaders],
    ] as const) {
      for (const header of await headers.all()) {
        const measured = await header.evaluate((node, kind) => {
          const header = node.getBoundingClientRect()
          const icon = node
            .querySelector("[data-item-icon]")!
            .getBoundingClientRect()
          const root =
            kind === "group"
              ? node.parentElement!.parentElement!
              : node.closest("[data-item-key]")!
          const border = [...root.querySelectorAll("div")].find((element) => {
            const style = getComputedStyle(element)
            return (
              style.borderLeftStyle === "solid" &&
              parseFloat(style.borderLeftWidth) === 1
            )
          })!
          const style = getComputedStyle(border)
          const line = border.getBoundingClientRect()
          const content = border.firstElementChild!.getBoundingClientRect()
          const group = node.closest('[data-fold-anchor^="group:"]')
          const groupIcon = group?.parentElement
            ?.querySelector("[data-item-icon]")
            ?.getBoundingClientRect()
          return {
            iconWidth: icon.width,
            lineOffset:
              line.left +
              parseFloat(style.borderLeftWidth) / 2 -
              (icon.left + icon.width / 2),
            contentOffset: content.left - header.left,
            contentRightInset: header.right - content.right,
            nestedOffset: groupIcon ? icon.left - groupIcon.left : null,
          }
        }, kind)
        expect(measured.iconWidth).toBe(16)
        expect(Math.abs(measured.lineOffset)).toBeLessThanOrEqual(0.5)
        expect(measured.contentOffset).toBe(kind === "group" ? 17 : 33)
        expect(measured.contentRightInset).toBe(kind === "group" ? 0 : 8)
        if (measured.nestedOffset !== null)
          expect(measured.nestedOffset).toBe(17)
      }
    }
    const turnBounds = (await turn.boundingBox())!
    for (const button of await turn
      .locator("[data-item-key] [data-item-header]")
      .getByRole("button", { name: "调试信息", exact: true })
      .all()) {
      const bounds = (await button.boundingBox())!
      expect(bounds.width).toBe(32)
      expect(
        Math.abs(bounds.x + bounds.width - turnBounds.x - turnBounds.width)
      ).toBeLessThanOrEqual(1)
    }
  }
  await expectLinesFollowIcons()
  await page.setViewportSize({ width: 390, height: 844 })
  await expectLinesFollowIcons()

  // 3. 窄屏下单条详情与外层聚合反复收放后保留原DOM、子项选择和阅读锚点。
  const mounted = await turn
    .locator(
      "[data-item-key] .markdown-view, [data-item-key] [data-output-block]"
    )
    .elementHandles()
  expect(mounted.length).toBeGreaterThanOrEqual(4)
  for (const header of [
    ...(await itemHeaders.all()),
    ...(await groupHeaders.all()),
  ]) {
    const toggle = header.locator(toggleSelector)
    await toggle.scrollIntoViewIfNeeded()
    await toggle.focus()
    const top = (await header.boundingBox())!.y
    const scrollTop = await scroller.evaluate((node) => node.scrollTop)
    await toggle.press("Enter")
    await expect(toggle).toHaveAttribute("aria-expanded", "false")
    for (const node of mounted)
      expect(await node.evaluate((element) => element.isConnected)).toBe(true)
    await toggle.press("Enter")
    await expect(toggle).toHaveAttribute("aria-expanded", "true")
    expect(Math.abs((await header.boundingBox())!.y - top)).toBeLessThanOrEqual(
      2
    )
    expect(
      Math.abs((await scroller.evaluate((node) => node.scrollTop)) - scrollTop)
    ).toBeLessThanOrEqual(2)
  }
  for (const header of await itemHeaders.all())
    await expect(header.locator(toggleSelector)).toHaveAttribute(
      "aria-expanded",
      "true"
    )
})

test("image_preview_is_shared_by_draft_message_and_markdown", async ({
  page,
  fixture,
}) => {
  await fixture.openApp(page)
  const imageData = await page.evaluate(() => {
    const canvas = document.createElement("canvas")
    canvas.width = 1800
    canvas.height = 1200
    const ctx = canvas.getContext("2d")!
    ctx.fillStyle = "#bbb"
    ctx.fillRect(0, 0, 1800, 1200)
    ctx.fillStyle = "#333"
    ctx.fillRect(100, 100, 1600, 1000)
    return canvas.toDataURL("image/png").split(",")[1]
  })
  const buffer = Buffer.from(imageData, "base64")
  const upload = () =>
    page
      .locator('input[type="file"]')
      .setInputFiles({ name: "wide.png", mimeType: "image/png", buffer })
  await upload()
  const draftTrigger = page.getByRole("button", {
    name: "预览图片 wide.png",
    exact: true,
  })
  await draftTrigger.click()
  const dialog = page.getByRole("dialog", { name: "图片预览" })
  const scroll = dialog.locator("[data-image-scroll]")
  await expect(dialog).toBeVisible()
  expect(
    await scroll.evaluate(
      (node) =>
        node.scrollHeight === node.clientHeight &&
        node.scrollWidth === node.clientWidth
    )
  ).toBe(true)
  await dialog.getByRole("button", { name: "100%", exact: true }).click()
  expect(
    await scroll.evaluate(
      (node) =>
        node.scrollWidth > node.clientWidth &&
        node.scrollHeight > node.clientHeight
    )
  ).toBe(true)
  await page.keyboard.press("Escape")
  await expect(dialog).toBeHidden()
  await expect(draftTrigger).toBeFocused()
  await page
    .getByRole("button", { name: "移除图片「wide.png」", exact: true })
    .click()
  await expect(dialog).toBeHidden()
  await expect(draftTrigger).toHaveCount(0)
  await upload()
  let imageRequests = 0
  await page.route("**/preview-wide.png", (route) =>
    route.fulfill({ contentType: "image/png", body: buffer })
  )
  await page.route("**/preview-retry.png", (route) => {
    imageRequests++
    return imageRequests === 1
      ? route.fulfill({ status: 404 })
      : route.fulfill({ contentType: "image/png", body: buffer })
  })
  await fixture.replies({
    text: "正文仍可阅读\n\n![Markdown宽图](https://preview.test/preview-wide.png)\n\n![重试图片](https://preview.test/preview-retry.png)",
  })
  await startConversation(page, "发送图片预览")
  const messages = getMessages(page)
  const scroller = getMessageScroller(page)
  await expect(
    messages.getByText("正文仍可阅读", { exact: true })
  ).toBeVisible()
  await expect(messages.getByRole("alert")).toHaveText(/图片加载失败/)
  await readFromTop(page)
  await messages.getByRole("button", { name: "重试", exact: true }).click()
  await expect(
    messages.getByRole("button", { name: "预览图片 重试图片", exact: true })
  ).toBeVisible()
  // 消息图片和 Markdown 图片打开的是同一个 Dialog：桌面用消息图片，手机用 Markdown 图片，
  // 手机全屏，Esc恢复焦点与阅读位置。
  for (const { width, name } of [
    { width: 1280, name: "预览图片 wide.png" },
    { width: 390, name: "预览图片 Markdown宽图" },
  ]) {
    await page.setViewportSize({ width, height: 844 })
    // 视口变化后等布局和滚动位置连续几帧不变，再记录打开前的阅读位置。
    await scroller.evaluate(async (node) => {
      let last = ""
      for (let stable = 0; stable < 5;) {
        await new Promise((resolve) => requestAnimationFrame(resolve))
        const now = `${node.scrollTop}:${node.scrollHeight}:${node.clientHeight}`
        stable = now === last ? stable + 1 : 0
        last = now
      }
    })
    const trigger = messages.getByRole("button", { name, exact: true })
    await trigger.scrollIntoViewIfNeeded()
    await trigger.click({ trial: true })
    const before = await scroller.evaluate((node) => node.scrollTop)
    await trigger.click()
    await expect(dialog).toBeVisible()
    const bounds = (await dialog.boundingBox())!
    if (width === 390) {
      await expect
        .poll(async () => Math.round((await dialog.boundingBox())!.width))
        .toBe(390)
      await expect
        .poll(async () => Math.round((await dialog.boundingBox())!.height))
        .toBe(844)
    } else expect(bounds.width).toBeGreaterThan(1000)
    await page.keyboard.press("Escape")
    await expect(dialog).toBeHidden()
    await expect(trigger).toBeFocused()
    // 触发按钮刚好有 1px 在可视区外时，焦点恢复会把它补回来，所以允许 1px 差。
    expect(
      Math.abs((await scroller.evaluate((node) => node.scrollTop)) - before),
      `width=${width} before=${before}`
    ).toBeLessThanOrEqual(1)
  }
})

test("completed_turn_collapses_each_progress_item_and_copies_only_final_result", async ({
  page,
  fixture,
}) => {
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  await fixture.replies(
    {
      reasoning: "核对输入",
      text: "这是中途进度，不是最终结果",
      tool: "bash",
      arguments: JSON.stringify({ command: "printf result-process" }),
    },
    { text: "最终结果只复制这一段" }
  )
  await fixture.openApp(page)
  await startConversation(page, "核对结果优先")
  const messages = getMessages(page)
  const turn = messages.locator("[data-turn-id]").last()
  await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()
  await expect(
    turn.getByRole("button", { name: "复制结果", exact: true })
  ).toBeVisible()
  await expect(
    turn.getByText("这是中途进度，不是最终结果", { exact: true })
  ).toBeHidden()
  await expect(turn.getByText("核对结果优先", { exact: true })).toBeVisible()
  await expect(
    turn.locator("[data-item-key]").getByRole("button", { name: /原始数据/ })
  ).toHaveCount(0)
  await turn.getByRole("button", { name: "复制结果", exact: true }).click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe("最终结果只复制这一段")
  await expect(turn.locator('[data-anchor-key^="turn:"]')).toHaveCount(0)
  const progress = turn.locator("[data-item-key]").filter({
    has: page.getByText("中途回复", { exact: true }),
  })
  const progressToggle = progress.locator(
    'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
  )
  const itemToggles = turn
    .locator("[data-item-key]")
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
  await expandTurnProcess(turn)
  await expect(itemToggles).toHaveCount(3)
  for (const toggle of await itemToggles.all())
    await expect(toggle).toHaveAttribute("aria-expanded", "false")
  await expect(progressToggle).toHaveAttribute("aria-expanded", "false")
  await progressToggle.click()
  await expect(
    turn.getByText("这是中途进度，不是最终结果", { exact: true })
  ).toBeVisible()
  await expect(
    turn
      .locator("[data-turn-status]")
      .getByRole("button", { name: "调试信息", exact: true })
  ).toHaveCount(1)
  await expect(progressToggle).toHaveAttribute("aria-expanded", "true")
  await expect(
    turn
      .locator("[data-item-key]")
      .locator(
        'button[aria-expanded="false"]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
  ).toHaveCount(2)
})

test("single_reasoning_always_has_a_group", async ({ page, fixture }) => {
  // 1. 同一对话里先后发出一次完成、一次中断的纯推理回复，都生成单成员聚合。
  await fixture.replies(
    { reasoning: "一条真实推理的正文", finishDelayMs: 2000 },
    { reasoning: "一条真实推理的正文", hold: true }
  )
  await fixture.openApp(page)
  await startConversation(page, "单成员思考聚合")
  // 右栏先展开：之后点调试信息不会再改变消息列的宽度
  await openRightPanel(page)
  const messages = getMessages(page)
  const collapsedToggle =
    'button[aria-expanded="false"]:has([data-item-title]):not([tabindex="-1"]):visible'

  // 2. 推理运行中，聚合里只有计数和滚动的动作文字，展开后能读到单条推理。
  const openRunningReasoning = async (turn: Locator) => {
    const group = turn.locator('[data-anchor-key^="group:"]')
    const counts = group.locator(
      "[data-process-counts] [data-rotating-text-label]"
    )
    const action = group.locator(
      "[data-rotating-text]:not([data-process-counts] *)"
    )
    await expect(group).toHaveCount(1)
    await expect(group).toBeVisible()
    await expect(counts).toHaveText("思考 1 次")
    await expect(action.locator("[data-rotating-text-label]")).toHaveText(
      "思考中 一条真实推理的正文"
    )
    await expect(
      group.getByRole("button", { name: "调试信息", exact: true })
    ).toHaveCount(0)
    await group.locator(collapsedToggle).click()
    const item = turn
      .locator("[data-item-key]")
      .filter({ has: page.getByText("思考中", { exact: true }) })
    await expect(item).toBeVisible()
    await item.locator(collapsedToggle).click()
    return { group, counts, action, item }
  }

  // 3. 正常结束：动作文字消失，推理记录改为「思考完成」，聚合不带成功标记。
  const completedTurn = messages.locator("[data-turn-id]").nth(0)
  const completed = await openRunningReasoning(completedTurn)
  await expect(
    completedTurn.locator('[data-turn-status="completed"]')
  ).toBeVisible()
  await expect(completed.counts).toHaveText("思考 1 次")
  await expect(completed.action).toHaveCount(0)
  await expect(
    completedTurn.locator("[data-item-key]").getByText("思考完成", {
      exact: true,
    })
  ).toBeVisible()
  await expect(
    completed.group.locator('[data-status-tone="success"]')
  ).toHaveCount(0)

  // 4. 被中断：聚合不显示「思考完成」，改为「已中断」图标。
  await sendMessage(page, "单成员思考被中断")
  const interruptedTurn = messages.locator("[data-turn-id]").nth(1)
  const interrupted = await openRunningReasoning(interruptedTurn)
  await expect(interrupted.item).toContainText("一条真实推理的正文")
  await page.getByRole("button", { name: "中断", exact: true }).click()
  await expect(
    interruptedTurn.getByText("这一轮已中断", { exact: true })
  ).toBeVisible()
  await expect(
    interrupted.group.getByText("思考完成", { exact: true })
  ).toHaveCount(0)
  await expect(
    interrupted.group.getByRole("img", { name: "已中断", exact: true })
  ).toBeVisible()

  // 5. 中断留下的提示记录没有展开详情：悬停和调试操作都不把类型图标变成箭头，也没有展开控件。
  const header = interruptedTurn
    .locator("[data-item-key]")
    .filter({ hasText: "这一轮已中断" })
    .locator("[data-item-header]")
  const icon = header.locator("svg[data-item-icon]")
  await expect(header).toBeVisible()
  const iconShape = () =>
    icon.evaluate((node) =>
      [...node.querySelectorAll("path")]
        .map((path) => path.getAttribute("d"))
        .join("|")
    )
  const shape = await iconShape()
  const title = header.locator("[data-item-title]")
  const titleX = (await title.boundingBox())!.x
  await header.hover()
  await expect(
    header.locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
  ).toHaveCount(0)
  expect(await iconShape()).toBe(shape)
  const debug = header.getByRole("button", { name: "调试信息", exact: true })
  await page.keyboard.press("Tab")
  await debug.focus()
  await expect(
    page.locator('[data-slot="tooltip-content"]:visible')
  ).toHaveText("调试信息")
  await debug.press("Enter")
  await expect(page.locator('[data-debug-kind="item"]')).toContainText(
    "turn_interrupted"
  )
  expect((await title.boundingBox())!.x).toBe(titleX)
  expect(await iconShape()).toBe(shape)

  // 6. 刷新后两轮的聚合都回到收起状态。
  await page.reload()
  const groups = messages.locator('[data-anchor-key^="group:"]')
  await expect(groups).toHaveCount(2)
  await expect(groups.locator(collapsedToggle)).toHaveCount(2)
})

test("context_summary_title_tracks_real_compaction", async ({
  page,
  fixture,
}) => {
  // 1. 先有一轮真实历史可供压缩。
  await fixture.replies({ text: "供真实压缩使用的历史内容" })
  await fixture.openApp(page)
  await startConversation(page, "压缩状态检查")
  const messages = getMessages(page)
  await expect(
    messages.getByText("供真实压缩使用的历史内容", { exact: true })
  ).toBeVisible()
  await expect(messages.locator('[data-turn-status="completed"]')).toBeVisible()
  const running = messages.getByText(/^正在压缩上下文/)
  const completed = messages.getByText("上下文已压缩", { exact: true })
  const compact = async () => {
    await page.getByRole("button", { name: /^上下文占用：/ }).click()
    await page.getByRole("button", { name: "压缩上下文", exact: true }).click()
    await expect(running).toBeVisible()
    await expect(completed).toHaveCount(0)
    await expectShimmer(running)
  }

  // 2. 压缩中被中断：运行标题消失，也不留下「上下文已压缩」。
  await fixture.replies({
    text: "这是一份真实摘要",
    chunkDelayMs: 150,
    hold: true,
  })
  await compact()
  await page.getByRole("button", { name: "中断", exact: true }).click()
  await expect(running).toHaveCount(0)
  await expect(completed).toHaveCount(0)

  // 3. 再压缩一次并正常结束：标题变成「上下文已压缩」，展开可读摘要。
  await fixture.replies({ text: "这是一份真实摘要", chunkDelayMs: 150 })
  await compact()
  await expect(completed).toBeVisible()
  await expect(completed).toHaveCSS("animation-name", "none")
  await expect(running).toHaveCount(0)
  const row = completed.locator("xpath=ancestor::*[@data-item-key][1]")
  await expect(row.locator('[data-status-tone="success"]')).toHaveCount(0)
  await row
    .locator(
      'button[aria-expanded="false"]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    .click()
  await expect(row).toContainText("这是一份真实摘要")
})

test("expanded_single_reasoning_retains_markdown_when_tools_append_and_turn_finishes", async ({
  page,
  fixture,
}) => {
  const gate = fixture.createCommandGate("reasoning")
  try {
    // 1. 推理先到，工具延迟追加；工具执行后停在门上，等原始数据展开完再放行。
    const reasoning =
      "```text\n" +
      Array.from({ length: 45 }, (_, index) => `thought-${index}`).join("\n") +
      "\n```"
    await fixture.replies(
      {
        reasoning,
        tool: "bash",
        arguments: JSON.stringify({
          command: `${gate.wait}; printf reasoning-tool`,
        }),
        toolDelayMs: 2000,
      },
      { text: "追加工具后的最终结果" }
    )
    await fixture.openApp(page)
    await startConversation(page, "思考追加工具")
    const turn = getMessages(page).locator("[data-turn-id]").last()
    const group = turn
      .locator('[data-anchor-key^="group:"]')
      .locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
      .first()
    const reasoningItem = turn
      .locator("[data-item-key]")
      .filter({ hasText: /思考中|思考完成/ })
    await expect(
      turn.locator('[data-anchor-key^="group:"]').first()
    ).toBeVisible()
    await expect(group).toHaveAttribute("aria-expanded", "false")
    await expect(reasoningItem).toBeHidden()

    // 2. 展开聚合、推理和长输出，记下 Markdown 节点。
    await group.click()
    await expect(
      reasoningItem.getByText("思考中", { exact: true })
    ).toBeVisible()
    await reasoningItem
      .locator(
        'button[aria-expanded="false"]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
      .click()
    const output = turn.locator("[data-item-key] [data-output-block]").first()
    await readFromTop(page)
    await output.getByRole("button", { name: "展开输出", exact: true }).click()
    const markdownNode = await output.elementHandle()

    // 3. 工具追加并开始执行后，展开选择和节点保持不变，再展开原始数据里的工具调用。
    const toolItem = turn.locator("[data-item-key]").filter({ hasText: "执行" })
    const tool = toolItem.locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    await expect(tool).toBeVisible()
    await expect(tool).toHaveAttribute("aria-expanded", "false")
    await expect(
      toolItem.getByRole("img", { name: "执行中", exact: true })
    ).toBeVisible()
    await expect(
      output.getByRole("button", { name: "收起输出", exact: true })
    ).toBeVisible()
    expect(await markdownNode!.evaluate((node) => node.isConnected)).toBe(true)
    const raw = await openTurnRaw(turn)
    const rawTool = raw.locator(`[data-json-path$=',"tool_call"]']`).first()
    await rawTool.press("Enter")
    await raw
      .getByRole("button", { name: "按 JSON 展开", exact: true })
      .first()
      .click()
    const nestedJSON = raw
      .getByRole("button", { name: "按字符串显示", exact: true })
      .first()
    const jsonNode = await nestedJSON.elementHandle()

    // 4. 放行工具，轮次结束后各节点仍是原来的。
    await gate.release()
    await expect(
      turn.getByRole("button", { name: "复制结果", exact: true })
    ).toBeVisible()
    await expect(group).toHaveAttribute("aria-expanded", "true")
    await expect(
      output.getByRole("button", { name: "收起输出", exact: true })
    ).toBeVisible()
    expect(await markdownNode!.evaluate((node) => node.isConnected)).toBe(true)
    await expect(nestedJSON).toBeVisible()
    expect(await jsonNode!.evaluate((node) => node.isConnected)).toBe(true)
  } finally {
    await gate.cleanup()
  }
})

test("auto_collapsing_a_scrolled_progress_item_places_its_summary_in_view", async ({
  page,
  fixture,
}) => {
  const gate = fixture.createCommandGate("progress")
  try {
    // 1. 中途回复很长，工具停在门上，轮次不会在滚动前结束。
    const progress = Array.from(
      { length: 100 },
      (_, index) => `中途段落 ${index}。`
    ).join("\n\n")
    const final = Array.from(
      { length: 100 },
      (_, index) => `最终段落 ${index}。`
    ).join("\n\n")
    await fixture.replies(
      {
        text: progress,
        tool: "bash",
        arguments: JSON.stringify({ command: gate.wait }),
      },
      { text: final }
    )
    await fixture.openApp(page)
    await startConversation(page, "折叠阅读锚点")
    const messages = getMessages(page)
    const scroller = getMessageScroller(page)
    const turn = messages.locator("[data-turn-id]").last()
    const progressRow = turn
      .locator("[data-item-key]")
      .filter({ has: page.getByText("中途段落 0。", { exact: true }) })
    await expect(progressRow).toBeVisible()
    const progressItem = turn.locator(
      `[data-item-key="${await progressRow.getAttribute("data-item-key")}"]`
    )

    // 2. 读到中途回复深处后放行工具，轮次结束，中途回复自动折叠。
    await scroller.evaluate(
      (scroller, itemID) => {
        const item = [
          ...scroller.querySelectorAll<HTMLElement>("[data-item-key]"),
        ].find((node) => node.dataset.itemKey === itemID)!
        scroller.scrollTop +=
          item.getBoundingClientRect().top -
          scroller.getBoundingClientRect().top +
          480
      },
      await progressItem.getAttribute("data-item-key")
    )
    await expect
      .poll(
        async () =>
          (await progressItem.boundingBox())!.y -
          (await scroller.boundingBox())!.y
      )
      .toBeLessThan(-400)
    await gate.release()
    await expect(
      turn.getByRole("button", { name: "复制结果", exact: true })
    ).toBeAttached()
    await expect(
      progressItem.getByText("中途段落 0。", { exact: true })
    ).toBeHidden()

    // 3. 折叠后的摘要行落在可视区顶部。
    const summary = progressItem.locator("[data-item-header]")
    await expect
      .poll(async () =>
        Math.abs(
          (await summary.boundingBox())!.y - (await scroller.boundingBox())!.y
        )
      )
      .toBeLessThanOrEqual(2)
  } finally {
    await gate.cleanup()
  }
})

test("adjacent_model_errors_keep_each_failure_and_break_at_tools", async ({
  page,
  fixture,
}) => {
  // 1. 真正的 HTTP 503 触发模型重试，成功工具把前后两段错误分开。
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  await fixture.replies(
    { status: 503, error: "first-model-failure" },
    { status: 503, error: "second-model-failure" },
    {
      tool: "bash",
      arguments: JSON.stringify({ command: "printf error-group-separator" }),
    },
    { status: 503, error: "third-model-failure" },
    { status: 503, error: "fourth-model-failure" },
    { text: "错误之后的最终结果" }
  )
  await fixture.openApp(page)
  await startConversation(page, "真实模型错误分组")
  const messages = getMessages(page)
  await expect(
    messages.getByText("错误之后的最终结果", { exact: true })
  ).toBeVisible()
  await page.reload()
  const turn = messages.locator("[data-turn-id]").last()
  const groups = turn
    .locator('[data-anchor-key^="group:"]')
    .filter({ hasText: "模型调用失败" })
  await expect(groups).toHaveCount(2)
  const firstGroup = groups.first()
  const toggle = firstGroup.locator(
    'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
  )
  await expect(toggle).toHaveAttribute("aria-expanded", "false")
  await expect(turn.getByText(/first-model-failure/)).toHaveCount(0)

  // 2. 聚合标题没有调试入口，整轮原始数据保留四条真实模型错误。
  for (const group of await groups.all())
    await expect(
      group
        .locator(":scope > [data-item-header]")
        .getByRole("button", { name: "调试信息", exact: true })
    ).toHaveCount(0)
  const raw = await openTurnRaw(turn)
  await raw.getByRole("button", { name: "复制", exact: true }).click()
  const turnData = JSON.parse(
    await page.evaluate(() => navigator.clipboard.readText())
  )
  expect(
    turnData.items.filter(
      (item: { kind: string }) => item.kind === "model_error"
    )
  ).toHaveLength(4)
  expect(JSON.stringify(turnData)).toContain("first-model-failure")
  expect(JSON.stringify(turnData)).toContain("fourth-model-failure")
  await page.getByRole("button", { name: "关闭对话详情", exact: true }).click()
  await expect(toggle).toHaveAttribute("aria-expanded", "false")

  // 3. 展开组后检查单条真实错误；桌面和窄屏都能打开单条调试信息；父组收放保留单条展开选择。
  await toggle.focus()
  await toggle.press("Enter")
  const firstError = turn
    .locator("[data-item-key]")
    .filter({ hasText: "模型调用失败" })
    .first()
  const itemToggle = firstError.locator(
    'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
  )
  await itemToggle.press("Enter")
  await expect(firstError).toContainText("first-model-failure")
  const openItemDebug = async () => {
    await firstError
      .getByRole("button", { name: "调试信息", exact: true })
      .click()
    return page.locator('[data-debug-kind="item"]')
  }
  const itemDebug = await openItemDebug()
  await itemDebug.getByRole("button", { name: "复制", exact: true }).click()
  const itemData = JSON.parse(
    await page.evaluate(() => navigator.clipboard.readText())
  )
  expect(itemData.item.kind).toBe("model_error")
  expect(itemData.item.model_error.message).toContain("first-model-failure")
  expect(itemData).not.toHaveProperty("members")
  await page.getByRole("button", { name: "关闭对话详情", exact: true }).click()
  await expect(itemToggle).toHaveAttribute("aria-expanded", "true")
  await page.setViewportSize({ width: 390, height: 844 })
  await openItemDebug()
  await expect(
    page.getByRole("dialog", { name: "对话详情", exact: true })
  ).toBeVisible()
  await page.getByRole("button", { name: "关闭对话详情", exact: true }).click()
  await toggle.press("Space")
  await expect(firstError).toBeHidden()
  await toggle.press("Space")
  await expect(firstError.getByText(/first-model-failure/)).toBeVisible()
  await expect(itemToggle).toHaveAttribute("aria-expanded", "true")
})

test("opened_skill_stays_visible_when_tools_append_and_turn_completes", async ({
  page,
  fixture,
}) => {
  // 1. 用户通过真实斜杠指令加载测试技能，在模型工具到达前展开正文。
  await fixture.replies(
    {
      reasoning: "技能检查中",
      tool: "bash",
      arguments: JSON.stringify({ command: "printf skill-complete" }),
      toolDelayMs: 1500,
    },
    { text: "技能检查最终结果" }
  )
  await fixture.openApp(page)
  await startConversation(page, "/display-check 检查技能选择")
  const turn = getMessages(page).locator("[data-turn-id]").last()
  const skill = turn.locator("[data-item-key]").filter({ hasText: "加载技能" })
  await skill
    .locator(
      'button[aria-expanded="false"]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    .click()
  const content = skill.getByRole("heading", {
    name: "技能展示检查",
    exact: true,
  })
  await expect(content).toBeVisible()
  const node = await content.elementHandle()
  // 2. 工具追加和成功折叠过程之后，已选技能正文保持原节点和展开选择。
  await expect(
    turn.getByText("技能检查最终结果", { exact: true })
  ).toBeVisible()
  await expect(
    skill.locator(
      'button[aria-expanded="true"]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
  ).toBeVisible()
  await expect(content).toBeVisible()
  expect(await node!.evaluate((element) => element.isConnected)).toBe(true)
})

test("conversation_switch_edits_only_target_draft_while_loading", async ({
  page,
  fixture,
}) => {
  // 1. 代理真实 RPC，仅暂存一次 A 的打开响应。先后发出 A、B 两条对话，各有各的草稿。
  let holdConversationID: string | undefined
  let releaseOpen: (() => void) | undefined
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    let heldRequestID: number | undefined
    socket.onMessage((message) => {
      const request = JSON.parse(String(message))
      if (
        request.method === "conversation/open" &&
        request.params.conversation_id === holdConversationID
      ) {
        heldRequestID = request.id
        holdConversationID = undefined
      }
      server.send(message)
    })
    server.onMessage((message) => {
      const response = JSON.parse(String(message))
      if (heldRequestID !== undefined && response.id === heldRequestID) {
        releaseOpen = () => socket.send(message)
        heldRequestID = undefined
      } else socket.send(message)
    })
  })
  await fixture.openApp(page)
  await startConversation(page, "常驻布局目标A")
  const conversationA = page.url().split("/c/")[1]
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  await getComposer(page).fill("A原草稿")
  await page.getByRole("link", { name: "新对话", exact: true }).click()
  await startConversation(page, "常驻布局目标B")
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  await getComposer(page).fill("B草稿")
  const modelControl = page.locator('form [aria-label="模型"]:visible')
  await expect(modelControl).toBeEnabled()
  const modelLabel = await modelControl.textContent()

  // 2. 从列表打开 A：A 尚未同步时，B 消息不冒充 A；输入框带着 A 的草稿并可编辑，但不可发送。
  holdConversationID = conversationA
  await page.getByRole("link", { name: /^常驻布局目标A/ }).click()
  await expect.poll(() => !!releaseOpen).toBe(true)
  await expect(getComposer(page)).toHaveValue("A原草稿")
  await expect(modelControl).toBeDisabled()
  await expect(modelControl).toHaveText("正在加载…")
  await getComposer(page).fill("A加载中编辑")
  await expect(
    page.getByRole("button", { name: "发送", exact: true })
  ).toBeDisabled()
  await expect(
    getMessages(page).getByText("常驻布局目标B", { exact: true })
  ).toHaveCount(0)
  await expect(
    page.getByRole("navigation", { name: "当前位置" })
  ).toContainText("常驻布局目标A")

  // 3. 尚未返回就切 B，旧响应不会把 A 内容和草稿写进 B；再切回 A 可正常继续订阅。
  await page.getByRole("link", { name: /^常驻布局目标B/ }).click()
  await expect(getComposer(page)).toHaveValue("B草稿")
  releaseOpen!()
  await expect(
    getMessages(page).getByText("常驻布局目标B", { exact: true })
  ).toBeVisible()
  await expect(getComposer(page)).toHaveValue("B草稿")
  await page.getByRole("link", { name: /^常驻布局目标A/ }).click()
  await expect(getComposer(page)).toHaveValue("A加载中编辑")
  await expect(
    page.getByRole("button", { name: "发送", exact: true })
  ).toBeEnabled()
  await expect(modelControl).toBeEnabled()
  await expect(modelControl).toHaveText(modelLabel!)
  await getComposer(page).press("Enter")
  await expect(
    getMessages(page).getByText("A加载中编辑", { exact: true })
  ).toBeVisible()
})

test("late_send_success_clears_only_original_conversation_draft", async ({
  page,
  fixture,
}) => {
  // 1. 持有一次真实提交的响应，服务端继续处理输入和推送。
  let holdSubmit = false
  let releaseSubmit: (() => void) | undefined
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    let heldRequestID: number | undefined
    socket.onMessage((message) => {
      const request = JSON.parse(String(message))
      if (holdSubmit && request.method === "conversation/submitInput") {
        heldRequestID = request.id
        holdSubmit = false
      }
      server.send(message)
    })
    server.onMessage((message) => {
      const response = JSON.parse(String(message))
      if (heldRequestID !== undefined && response.id === heldRequestID) {
        releaseSubmit = () => socket.send(message)
        heldRequestID = undefined
      } else socket.send(message)
    })
  })
  await fixture.openApp(page)
  await startConversation(page, "延迟发送目标B")
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  await getComposer(page).fill("B不能被清空")
  await page.getByRole("link", { name: "新对话", exact: true }).click()
  await startConversation(page, "延迟发送目标A")
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  holdSubmit = true
  await getComposer(page).fill("A已发送正文")
  await getComposer(page).press("Enter")
  await expect.poll(() => !!releaseSubmit).toBe(true)
  await getComposer(page).fill("A已发送正文 后续草稿")

  // 2. 切 B 后收到 A 的成功响应，只清理 A 已发送的部分。
  await page.getByRole("link", { name: /^延迟发送目标B/ }).click()
  await expect(getComposer(page)).toHaveValue("B不能被清空")
  await expect(
    page.getByRole("button", { name: "发送", exact: true })
  ).toBeEnabled()
  releaseSubmit!()
  await expect(getComposer(page)).toHaveValue("B不能被清空")
  await page.getByRole("link", { name: /^延迟发送目标A/ }).click()
  await expect(getComposer(page)).toHaveValue("后续草稿")
  await expect(
    getMessages(page).getByText("A已发送正文", { exact: true })
  ).toBeVisible()
})

test("history_pagination_recovers_after_connection_error_without_reloading_page", async ({
  page,
  fixture,
}) => {
  test.setTimeout(120_000)
  // 1. 长历史首屏只读最近一页；下一次向前分页在真实 socket 上断开。
  const { longConversationID } = await fixture.seed({
    conversations: 0,
    longConversationTurns: 30,
  })
  let interruptPagination = false
  let didInterrupt = false
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message))
      if (
        interruptPagination &&
        request.method === "session/items/list" &&
        request.params.cursor !== undefined &&
        request.params.order === "desc"
      ) {
        interruptPagination = false
        didInterrupt = true
        void socket.close()
        return
      }
      server.send(message)
    })
    server.onMessage((message) => socket.send(message))
  })
  await fixture.openApp(page)
  await page.goto(page.url() + "/c/" + longConversationID)
  const messages = getMessages(page)
  const scroller = getMessageScroller(page)
  await expect(
    messages.getByText("长对话第 30 轮", { exact: true })
  ).toBeVisible()
  await expect(
    messages.getByText("长对话第 1 轮", { exact: true })
  ).toHaveCount(0)
  interruptPagination = true
  await scroller.evaluate((node) => {
    node.scrollTop = 0
  })
  const { text: earliest } = await getFirstVisibleTurn(page)
  const earlierTurn = Number(earliest.match(/\d+/)![0]) - 1
  await expect.poll(() => didInterrupt).toBe(true)

  // 2. 重连后继续上翻，连接错误不留下隐形的分页锁。
  await expect(
    page.getByRole("button", { name: "已连接", exact: true })
  ).toBeVisible({ timeout: 20_000 })
  await expect(
    page.getByRole("button", { name: "发送", exact: true })
  ).toBeDisabled()
  await expect(async () => {
    await scroller.evaluate((node) => {
      node.scrollTop = 0
    })
    await scroller.hover()
    await page.mouse.wheel(0, -600)
    await expect(
      messages.getByText(`长对话第 ${earlierTurn} 轮`, { exact: true })
    ).toBeAttached({ timeout: 1000 })
  }).toPass({ timeout: 20_000 })
  await expect(messages.getByRole("alert")).toHaveCount(0)
})

test("user_message_copies_only_typed_text_while_running", async ({
  page,
  fixture,
}) => {
  // 1. 真实提交包含两段正文、附带材料和图片的用户消息，模型保持生成中。
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  await fixture.openApp(page)
  await startConversation(page, "用户消息复制检查")
  const messages = getMessages(page)
  await expect(messages.locator('[data-turn-status="completed"]')).toBeVisible()
  const conversationURL = page.url()
  const conversationID = conversationURL.split("/c/")[1]
  const textParts = ["  **原始正文**\n保留换行", "末段  "]
  const image = {
    kind: "image" as const,
    image: {
      data_url:
        "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aJxkAAAAASUVORK5CYII=",
      alt: "复制检查图片",
    },
  }
  await fixture.replies({ text: "复制时仍在生成", hold: true })
  await fixture.submitInput(conversationID, [
    { kind: "text", text: { text: textParts[0] } },
    { kind: "text", text: { text: "附带材料不复制", is_context: true } },
    image,
    { kind: "text", text: { text: textParts[1] } },
  ])
  await expect(
    messages.getByText("复制时仍在生成", { exact: true })
  ).toBeVisible()
  const userItem = messages.locator("[data-item-key]").filter({
    has: page.getByText(textParts.join("\n"), { exact: true }),
  })
  const copy = userItem.getByRole("button", { name: "复制消息", exact: true })
  const rewind = userItem.getByRole("button", {
    name: "从这里重来",
    exact: true,
  })
  await expect(
    userItem.getByRole("button", { name: "附带材料", exact: true })
  ).toBeVisible()
  await expect(
    userItem.getByRole("button", {
      name: "预览图片 复制检查图片",
      exact: true,
    })
  ).toBeVisible()

  // 2. 生成期间复制原文。
  await copy.click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(textParts.join("\n"))
  await expect(userItem.getByRole("status")).toHaveText("已复制")
  await expect(
    page.getByRole("button", { name: "中断", exact: true })
  ).toBeVisible()
  await expect(copy).toBeEnabled()

  // 3. 两个按钮均为 32px，间距 8px，靠齐气泡右边缘；桌面和窄屏都成立。
  const expectActionsAligned = async () => {
    for (const button of [copy, rewind]) {
      await expect(button).toHaveCSS("width", "32px")
      await expect(button).toHaveCSS("height", "32px")
    }
    const copyBounds = (await copy.boundingBox())!
    const rewindBounds = (await rewind.boundingBox())!
    const bubbleBounds = (await userItem
      .locator("p")
      .filter({ hasText: textParts.join("\n") })
      .locator("..")
      .boundingBox())!
    expect(rewindBounds.x - copyBounds.x - copyBounds.width).toBe(8)
    expect(rewindBounds.y).toBe(copyBounds.y)
    expect(
      Math.abs(
        rewindBounds.x +
          rewindBounds.width -
          bubbleBounds.x -
          bubbleBounds.width
      )
    ).toBeLessThanOrEqual(1)
  }
  await expectActionsAligned()
  await page.setViewportSize({ width: 390, height: 844 })
  await expectActionsAligned()

  // 4. 纯图片消息仍显示图片和重来入口，不生成空的复制按钮。
  await page.getByRole("button", { name: "中断", exact: true }).click()
  await expect(
    messages.locator('[data-turn-status="interrupted"]')
  ).toBeVisible()
  await fixture.submitInput(conversationID, [
    { ...image, image: { ...image.image, alt: "只有图片" } },
  ])
  const imageItem = messages.locator("[data-item-key]").filter({
    has: page.getByRole("button", { name: "预览图片 只有图片", exact: true }),
  })
  await expect(imageItem).toBeVisible()
  await expect(
    imageItem.getByRole("button", { name: "复制消息", exact: true })
  ).toHaveCount(0)
  await expect(
    imageItem.getByRole("button", { name: "从这里重来", exact: true })
  ).toBeVisible()
  await expect(messages.locator('[data-turn-status="completed"]')).toHaveCount(
    2
  )

  // 5. 既有重来操作仍分叉并恢复完整草稿，正文、图片和附带材料均保留。
  await readFromTop(page)
  await rewind.click()
  await expect(page).not.toHaveURL(conversationURL)
  await expect(getComposer(page)).toHaveValue(textParts.join(""))
  await expect(
    page.getByRole("button", { name: "预览图片 复制检查图片", exact: true })
  ).toBeVisible()
  await page.getByRole("button", { name: "附带材料", exact: true }).click()
  // 原对话的标签页还挂着，只看输入框里恢复出来的附带材料。
  await expect(
    page.locator("form").getByText("附带材料不复制", { exact: true })
  ).toBeVisible()
})

for (const scenario of [
  { width: 1280, colorScheme: "light" },
  { width: 390, colorScheme: "dark" },
] as const) {
  test(`long_user_messages_fold_after_four_lines_${scenario.width}`, async ({
    page,
    fixture,
  }) => {
    // 1. 五行消息默认只露出前四行，四行消息没有展开入口。
    await page.setViewportSize({ width: scenario.width, height: 844 })
    await page.emulateMedia({ colorScheme: scenario.colorScheme })
    await fixture.openApp(page)
    await startConversation(page, "用户消息折叠")
    const conversationID = page.url().split("/c/")[1]
    const longText = Array.from(
      { length: 5 },
      (_, index) => `用户第${index + 1}行`
    ).join("\n")
    const shortText = Array.from(
      { length: 4 },
      (_, index) => `短消息第${index + 1}行`
    ).join("\n")
    await fixture.replies({ text: "ok" })
    await fixture.submitInput(conversationID, [
      { kind: "text", text: { text: longText } },
      { kind: "text", text: { text: "独立附带材料", is_context: true } },
    ])
    await fixture.submitInput(conversationID, [
      { kind: "text", text: { text: shortText } },
    ])
    const messages = getMessages(page)
    const longItem = messages.locator("[data-item-key]").filter({
      hasText: "用户第1行",
    })
    const preview = longItem.locator("p").filter({ hasText: "用户第1行" })
    const expand = longItem.getByRole("button", {
      name: "展开消息",
      exact: true,
    })
    await expect(expand).toBeVisible()
    await expect(preview).toHaveCSS("height", "96px")
    await expect(preview).toHaveCSS("font-size", "14px")
    await expect(preview).toHaveCSS("line-height", "24px")
    await expect(preview).toHaveCSS(
      "mask-image",
      /linear-gradient.*rgba\(0, 0, 0, 0\)/
    )
    expect(await preview.evaluate((node) => node.scrollHeight)).toBeGreaterThan(
      96
    )
    const shortItem = messages.locator("[data-item-key]").filter({
      hasText: "短消息第1行",
    })
    await expect(
      shortItem.getByRole("button", { name: "展开消息", exact: true })
    ).toHaveCount(0)
    await expect(shortItem).toContainText("短消息第4行")
    await expect(shortItem.locator("p")).toHaveCSS("mask-image", "none")

    // 2. 逐帧检查展开和收起：高度与渐隐均经过中间值，完成后行与行不重叠。
    await readFromTop(page)
    for (const { name, height, opacity } of [
      { name: "展开消息", height: 120, opacity: 1 },
      { name: "收起消息", height: 96, opacity: 0 },
    ]) {
      const samples = await longItem
        .getByRole("button", { name, exact: true })
        .evaluate(async (button) => {
          const text = document.getElementById(
            button.getAttribute("aria-controls")!
          )!
          const samples: { height: number; opacity: number }[] = []
          const start = performance.now()
          ;(button as HTMLButtonElement).click()
          while (performance.now() - start < 350) {
            await new Promise((resolve) => requestAnimationFrame(resolve))
            samples.push({
              height: text.getBoundingClientRect().height,
              opacity: Number(
                getComputedStyle(text).getPropertyValue(
                  "--message-preview-opacity"
                )
              ),
            })
          }
          return samples
        })
      expect(
        samples.some((sample) => sample.height > 96.1 && sample.height < 119.9)
      ).toBe(true)
      expect(
        samples.some((sample) => sample.opacity > 0.01 && sample.opacity < 0.99)
      ).toBe(true)
      expect(samples.at(-1)!.height).toBeCloseTo(height, 0)
      expect(samples.at(-1)!.opacity).toBe(opacity)
      const rows = await messages
        .locator("[data-row-key]")
        .evaluateAll((nodes) =>
          nodes.map((node) => {
            const { top, bottom } = node.getBoundingClientRect()
            return { top, bottom }
          })
        )
      for (let index = 1; index < rows.length; index++)
        expect(rows[index].top).toBeGreaterThanOrEqual(
          rows[index - 1].bottom - 1
        )
    }

    // 3. 附带材料与正文独立展开，复制始终是完整正文。
    await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
    await expand.click()
    await expect(preview).toContainText("用户第5行")
    await expect(preview).toHaveCSS("height", "120px")
    const context = longItem.getByRole("button", {
      name: "附带材料",
      exact: true,
    })
    await expect(context).toHaveAttribute("aria-expanded", "false")
    await context.click()
    await expect(
      longItem.getByText("独立附带材料", { exact: true })
    ).toBeVisible()
    await longItem
      .getByRole("button", { name: "复制消息", exact: true })
      .click()
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()))
      .toBe(longText)
    await longItem
      .getByRole("button", { name: "收起消息", exact: true })
      .click()
    await expect(preview).toHaveCSS("height", "96px")
    expect(await preview.evaluate((node) => node.scrollHeight)).toBeGreaterThan(
      96
    )
    await expect(
      longItem.getByText("独立附带材料", { exact: true })
    ).toBeVisible()
    await page.screenshot({
      path: test.info().outputPath("user-message-folded.png"),
    })
  })
}

test("short_user_messages_mount_without_height_animation", async ({
  page,
  fixture,
}) => {
  // 1. 发送一至三行正文，从节点首次出现开始逐帧取高度。
  await fixture.openApp(page)
  await startConversation(page, "短消息初次显示")
  const conversationID = page.url().split("/c/")[1]
  for (const lines of [1, 2, 3]) {
    const text = Array.from(
      { length: lines },
      (_, index) => `短消息${lines}：第${index + 1}行`
    ).join("\n")
    const sampleHeights = page.evaluate(async (text) => {
      let paragraph: HTMLParagraphElement | undefined
      while (!paragraph) {
        await new Promise((resolve) => requestAnimationFrame(resolve))
        paragraph = [
          ...document.querySelectorAll<HTMLParagraphElement>(
            "[data-item-key] p"
          ),
        ].find((node) => node.textContent === text)
      }
      const samples = []
      for (let index = 0; index < 12; index++) {
        samples.push(paragraph.getBoundingClientRect().height)
        await new Promise((resolve) => requestAnimationFrame(resolve))
      }
      return samples
    }, text)
    await fixture.submitInput(conversationID, [
      { kind: "text", text: { text } },
    ])
    // 2. 每一帧都是正文实际高度，不从四行预览高度收缩。
    for (const height of await sampleHeights) expect(height).toBe(lines * 24)
  }
})

test("user_message_expansion_survives_virtual_list_unmount", async ({
  page,
  fixture,
}) => {
  // 1. 展开第一条用户消息，再发送足够多的后续轮次让它离开挂载范围。
  await fixture.openApp(page)
  const text = Array.from(
    { length: 8 },
    (_, index) => `保留展开第${index + 1}行`
  ).join("\n")
  await startConversation(page, text)
  const conversationID = page.url().split("/c/")[1]
  const messages = getMessages(page)
  const scroller = getMessageScroller(page)
  const item = messages
    .locator("[data-item-key]")
    .filter({ has: page.getByText(text, { exact: true }) })
  await item.getByRole("button", { name: "展开消息", exact: true }).click()
  await expect(item.locator("p")).toHaveCSS("height", "192px")
  const mountedText = await item.locator("p").elementHandle()
  for (let index = 0; index < 24; index++)
    await fixture.submitInput(conversationID, [
      { kind: "text", text: { text: `后续第${index + 1}条` } },
    ])
  await expect(async () => {
    await scroller.evaluate((node) => {
      node.scrollTop = node.scrollHeight
    })
    await expect(messages.getByText("后续第24条", { exact: true })).toBeVisible(
      { timeout: 1000 }
    )
  }).toPass()
  await expect(item).toHaveCount(0)
  expect(await mountedText!.evaluate((node) => node.isConnected)).toBe(false)
  expect(await messages.locator("[data-turn-id]").count()).toBeLessThan(25)

  // 2. 滚回后立即恢复已展开高度，不重播折叠动画；短消息没有多余入口。
  await expect(async () => {
    await scroller.evaluate((node) => {
      node.dispatchEvent(new WheelEvent("wheel", { deltaY: -100 }))
      node.scrollTop = 0
    })
    await expect(item).toBeVisible({ timeout: 1000 })
  }).toPass()
  await expect(
    item.getByRole("button", { name: "收起消息", exact: true })
  ).toBeVisible()
  await expect(item.locator("p")).toHaveCSS("height", "192px")
  await item.getByRole("button", { name: "收起消息", exact: true }).click()
  await expect(item.locator("p")).toHaveCSS("height", "96px")
  expect(await messages.locator("[data-turn-id]").count()).toBeLessThan(25)
})

for (const scenario of [
  { width: 1280, colorScheme: "light" },
  { width: 390, colorScheme: "dark" },
] as const) {
  test(`each_turn_last_text_has_copy_${scenario.width}`, async ({
    page,
    fixture,
  }) => {
    test.setTimeout(120_000)
    // 1. 真实长会话每轮保留一个复制入口，同轮中途正文不重复提供入口。
    await page.setViewportSize({ width: scenario.width, height: 844 })
    await page.emulateMedia({ colorScheme: scenario.colorScheme })
    await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
    const { longConversationID } = await fixture.seed({
      conversations: 0,
      longConversationTurns: 30,
    })
    await fixture.openApp(page)
    await page.goto(page.url() + "/c/" + longConversationID)
    const messages = getMessages(page)
    const copies = messages.getByRole("button", {
      name: "复制结果",
      exact: true,
    })
    // 挂着的每一轮各有几个复制入口，按轮的顺序用逗号连起来；消息区只挂视口附近的几轮
    const getCopyCountsByTurn = async () => {
      const turns = await messages.locator("[data-turn-id]").all()
      const counts = await Promise.all(
        turns.map((turn) =>
          turn.getByRole("button", { name: "复制结果", exact: true }).count()
        )
      )
      return counts.join(",")
    }
    await expect.poll(() => copies.count()).toBeGreaterThan(1)
    const oldReplyKeys: string[] = []
    for (const round of [1, 2]) {
      const reply = `历史正文第 ${round} 条`
      const gate = fixture.createCommandGate(
        `turn-copy-${scenario.width}-${round}`
      )
      try {
        await fixture.replies(
          {
            text: `第 ${round} 轮中途文字`,
            tool: "bash",
            arguments: JSON.stringify({
              command: `${gate.wait}; printf per-turn-copy`,
            }),
          },
          { text: reply }
        )
        await getComposer(page).fill(`独立输入第 ${round} 条`)
        await getComposer(page).press("Enter")
        const runningTurn = messages.locator("[data-turn-id]").last()
        await expect(
          runningTurn.getByText(`第 ${round} 轮中途文字`, { exact: true })
        ).toBeVisible()
        await expandTurnProcess(runningTurn)
        await expect(
          runningTurn.locator('[data-tool-state="running"]')
        ).toBeVisible()
        await expect(
          runningTurn.getByRole("button", { name: "复制结果", exact: true })
        ).toHaveCount(0)
        await expect.poll(getCopyCountsByTurn).toMatch(/^(1,)+0$/)
        await gate.release()
        const replyItem = messages.locator("[data-item-key]").filter({
          has: page.getByText(reply, { exact: true }),
        })
        await expect(
          replyItem.getByRole("button", { name: "复制结果", exact: true })
        ).toBeVisible()
        await expect.poll(getCopyCountsByTurn).toMatch(/^1(,1)+$/)
        await expect(
          messages
            .locator("[data-turn-id]")
            .last()
            .getByRole("button", { name: "复制结果", exact: true })
        ).toHaveCount(1)
        oldReplyKeys.push((await replyItem.getAttribute("data-item-key"))!)
        await expect(
          messages
            .locator("[data-item-key]")
            .filter({
              has: page.getByText(`独立输入第 ${round} 条`, { exact: true }),
            })
            .getByRole("button", { name: "复制消息", exact: true })
        ).toBeVisible()
      } finally {
        await gate.cleanup()
      }
    }

    // 2. 新轮流式生成只隐藏本轮入口，旧轮保留；结束后复制该轮末条原始 Markdown。
    const latestText = "最新流式正文 **保留 Markdown**\n\n第二段原文"
    await fixture.replies({
      text: latestText,
      chunkDelayMs: 20,
      finishDelayMs: 6000,
    })
    await getComposer(page).fill("最新流式输入")
    await getComposer(page).press("Enter")
    await expect(
      messages.getByText("保留 Markdown", { exact: true })
    ).toBeVisible()
    await expect(
      page.getByRole("button", { name: "中断", exact: true })
    ).toBeVisible()
    await expect.poll(getCopyCountsByTurn).toMatch(/^(1,)+0$/)
    const latestInput = messages.locator("[data-item-key]").filter({
      has: page.getByText("最新流式输入", { exact: true }),
    })
    await latestInput
      .getByRole("button", { name: "复制消息", exact: true })
      .click()
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()))
      .toBe("最新流式输入")
    await expect
      .poll(getCopyCountsByTurn, { timeout: 15_000 })
      .toMatch(/^1(,1)+$/)
    const latestReply = messages.locator("[data-item-key]").filter({
      has: page.getByText("保留 Markdown", { exact: true }),
    })
    const latestReplyKey = (await latestReply.getAttribute("data-item-key"))!
    await latestReply
      .getByRole("button", { name: "复制结果", exact: true })
      .click()
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()))
      .toBe(latestText)
    for (const key of oldReplyKeys)
      await expect(
        messages
          .locator(`[data-item-key="${key}"]`)
          .getByRole("button", { name: "复制结果", exact: true })
      ).toHaveCount(1)

    // 3. 刷新只加载尾页；每轮尾部正文可复制，加载旧轮后增加对应入口。
    await page.reload()
    await expect.poll(() => copies.count()).toBeGreaterThan(1)
    await expect(
      messages.getByText("长对话第 1 轮", { exact: true })
    ).toHaveCount(0)
    await expect(
      messages
        .locator(`[data-item-key="${latestReplyKey}"]`)
        .getByRole("button", { name: "复制结果", exact: true })
    ).toHaveCount(1)
    const scroller = getMessageScroller(page)
    await scroller.evaluate((node) => {
      node.scrollTop = 0
    })
    const { text: earliest } = await getFirstVisibleTurn(page)
    const earlierTurn = Number(earliest.match(/\d+/)![0]) - 1
    expect(earlierTurn).toBeGreaterThan(0)
    await expect(async () => {
      await scroller.evaluate((node) => {
        node.scrollTop = 0
      })
      await scroller.hover()
      await page.mouse.wheel(0, -600)
      expect(await getSmallestMountedTurn(messages)).toBeLessThanOrEqual(
        earlierTurn
      )
    }).toPass({ timeout: 20_000 })
    const loadedTurn = await getSmallestMountedTurn(messages)
    await expect(
      messages
        .locator("[data-turn-id]")
        .filter({
          has: page.getByText(`长对话第 ${loadedTurn} 轮`, { exact: true }),
        })
        .getByRole("button", { name: "复制结果", exact: true })
    ).toHaveCount(1)
    for (const key of oldReplyKeys)
      await expect(
        messages
          .locator(`[data-item-key="${key}"]`)
          .getByRole("button", { name: "复制结果", exact: true })
      ).toHaveCount(1)
    await expect(
      messages
        .locator(`[data-item-key="${latestReplyKey}"]`)
        .getByRole("button", { name: "复制结果", exact: true })
    ).toHaveCount(1)
  })
}
