import {
  expect,
  expectDocumentNotScrollable,
  getComposer,
  getMessages,
  getMessageScroller,
  test,
} from "./fixture"

test("last_finished_reply_can_be_copied_before_earlier_pages_load", async ({
  page,
  fixture,
}) => {
  // 1. 真实工具调用把这一轮扩到两页，最终回复生成期间不显示复制入口。
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  const input = "复制不等待整轮历史"
  const finalText = "最后回复 **原文**\n\n只复制这一条。"
  await fixture.replies(
    {
      text: "这是较早的回复，不应和最后一条拼接复制。",
      tools: Array.from({ length: 30 }, (_, index) => ({
        name: "bash",
        arguments: JSON.stringify({ command: `printf copy-page-${index}` }),
      })),
    },
    { text: finalText, chunkDelayMs: 20, finishDelayMs: 1000 }
  )
  await fixture.openApp(page)
  await getComposer(page).fill(input)
  await getComposer(page).press("Enter")
  const messages = getMessages(page)
  const turn = messages.locator("[data-turn-id]").last()
  const finalItem = turn.locator("[data-item-key]").filter({
    has: page.getByText("只复制这一条。", { exact: true }),
  })
  await expect(finalItem).toBeVisible()
  await expect(
    turn.getByRole("button", { name: "复制结果", exact: true })
  ).toHaveCount(0)
  await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()

  // 2. 刷新后只读最近50条；最后一条已结束回复单独提供一个复制按钮。
  await page.reload()
  await expect(finalItem).toBeVisible()
  await expect(messages.getByText(input, { exact: true })).toHaveCount(0)
  const copy = finalItem.getByRole("button", {
    name: "复制结果",
    exact: true,
  })
  await expect(copy).toBeVisible()
  await expect(
    turn.getByRole("button", { name: "复制结果", exact: true })
  ).toHaveCount(1)
  await expect(copy).toHaveCSS("height", "32px")
  await copy.click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(finalText)
  const node = await copy.elementHandle()
  await page.screenshot({
    path: test.info().outputPath("latest-page-copy.png"),
  })

  // 3. 加载早页后仍复制同一条原文，按钮和消息DOM保留。
  await expect(async () => {
    await getMessageScroller(page).hover()
    await page.mouse.wheel(0, -2000)
    await expect(messages.getByText(input, { exact: true })).toBeAttached({
      timeout: 1000,
    })
  }).toPass()
  expect(await node!.evaluate((element) => element.isConnected)).toBe(true)
  await expect(
    turn.getByRole("button", { name: "复制结果", exact: true })
  ).toHaveCount(1)
  await copy.click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(finalText)

  // 4. 窄屏下复制入口仍是同一个 32px 按钮。
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(copy).toBeVisible()
  await expect(copy).toHaveCSS("height", "32px")
})

test.describe("pagination", () => {
  // 预置数据较重：串行跑在同一个 worker 里，只 seed 一次
  test.describe.configure({ mode: "serial" })

  // 预置的短对话数：每页 50 条，101 条要加载三页才全，第三页只有最后一条，游标必须越过第二页
  const seededConversations = 101
  // seed 返回的长对话：40 轮，每轮一条用户消息加一条回复，共 80 个 Item，多于一页（50 条）
  const longConversationTurns = 40
  let longConversationID = ""

  test.beforeAll(async ({ fixture }) => {
    test.setTimeout(180_000)
    // 自动清空队列的 fixture 不在 beforeAll 里运行，这里先清一次，seed 的每一轮都回 ok
    await fixture.resetReplies()
    ;({ longConversationID } = await fixture.seed({
      conversations: seededConversations,
      longConversationTurns,
    }))
  })

  test(
    "conversation_list_loads_next_page_at_bottom",
    { tag: "@p1" },
    async ({ page, fixture }) => {
      // 1. 第一页只有一部分预置对话。
      await fixture.openApp(page)
      const seeded = page.getByRole("link", { name: /^预置对话 \d{3}/ })
      await expect(seeded.first()).toBeVisible()
      expect(await seeded.count()).toBeLessThan(seededConversations)
      // 2. 列表往下滚，滚到底继续加载，直到全部加载完。
      await expect(async () => {
        await seeded.last().hover()
        await page.mouse.wheel(0, 10_000)
        await expect(seeded).toHaveCount(seededConversations, {
          timeout: 1_000,
        })
      }).toPass()
      // 3. 两页最多 100 条：第三页只有最旧的一条，它出现说明游标越过了第二页。
      await expect(seeded.last()).toContainText("预置对话 001")
    }
  )

  test(
    "message_history_keeps_first_visible_item_when_loading_earlier",
    { tag: "@p1" },
    async ({ page, fixture }) => {
      // 1. 打开长对话：先显示最近的一页，贴在底部。
      await fixture.openApp(page)
      await page.goto(page.url() + "/c/" + longConversationID)
      const messages = getMessages(page)
      await expect(
        messages.getByText(`长对话第 ${longConversationTurns} 轮`, {
          exact: true,
        })
      ).toBeVisible()
      await expect(
        messages.getByText("长对话第 1 轮", { exact: true })
      ).toHaveCount(0)
      // 2. 滚到顶，同一帧里记下已加载的最早一条用户消息离视口顶部的距离。
      const earliest = await messages
        .getByText(/^长对话第 \d+ 轮$/)
        .first()
        .innerText()
      const earliestTurn = Number(earliest.match(/\d+/)?.[0])
      const topBefore = await getMessageScroller(page).evaluate(
        (scroller, text) => {
          scroller.scrollTop = 0
          const element = [...scroller.querySelectorAll("p")].find(
            (each) => each.textContent === text
          )
          return element?.getBoundingClientRect().top
        },
        earliest
      )
      expect(topBefore).toBeDefined()
      // 3. 更早的消息加载出来之后，这一条还在原来的位置。
      await expect(
        messages.getByText(`长对话第 ${earliestTurn - 1} 轮`, { exact: true })
      ).toBeAttached()
      const anchor = messages.getByText(earliest, { exact: true })
      await expect
        .poll(async () =>
          Math.abs(((await anchor.boundingBox())?.y ?? 0) - (topBefore ?? 0))
        )
        .toBeLessThanOrEqual(2)
    }
  )

  test("scroll_to_bottom_hides_from_keyboard_and_returns_focus", async ({
    page,
    fixture,
  }) => {
    await fixture.openApp(page)
    await page.goto(page.url() + "/c/" + longConversationID)
    const messages = getMessages(page)
    const scroller = getMessageScroller(page)
    await expect(
      messages.getByText(`长对话第 ${longConversationTurns} 轮`, {
        exact: true,
      })
    ).toBeVisible()
    await expect
      .poll(() =>
        scroller.evaluate(
          (node) => node.scrollHeight - node.clientHeight - node.scrollTop
        )
      )
      .toBeLessThanOrEqual(2)
    // 长历史里每轮都有 sr-only 的复制状态文字，只有消息区滚动，整页不滚动
    await expectDocumentNotScrollable(page)
    const back = page.getByRole("button", { name: "回到底部", exact: true })
    // 1. 键盘回到底部，平滑滚动完成后按钮隐藏并交还焦点。
    await expect(back).toHaveCount(0)
    await scroller.hover()
    await page.mouse.wheel(0, -500)
    await expect(back).toBeVisible()
    await back.focus()
    await expect(back).toBeFocused()
    await back.press("Enter")
    await expect(messages).toBeFocused()
    await expect
      .poll(() =>
        scroller.evaluate(
          (node) => node.scrollHeight - node.clientHeight - node.scrollTop
        )
      )
      .toBeLessThanOrEqual(2)
    await expect(back).toHaveCount(0)
    // 2. 已隐藏按钮留在 DOM 退场，但键盘与程序聚焦均不能进入。
    const hiddenBack = page.getByRole("button", {
      name: "回到底部",
      exact: true,
      includeHidden: true,
    })
    await expect(hiddenBack).toBeDisabled()
    await hiddenBack.evaluate((node) => node.focus())
    await expect(messages).toBeFocused()
    await expect(page.getByRole("tooltip")).toHaveCount(0)
  })
})

test("loading_earlier_members_keeps_the_existing_group_and_tool_choices", async ({
  page,
  fixture,
}) => {
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  const tools = Array.from({ length: 30 }, (_, index) => ({
    name: index === 0 ? "read_file" : "bash",
    arguments: JSON.stringify(
      index === 0
        ? { path: "missing-orphan-result-file.txt" }
        : { command: `printf seam-tool-${index}` }
    ),
  }))
  await fixture.replies({ tools }, { text: "长轮最终结果" })
  await fixture.openApp(page)
  // 工具流可能在订阅建立前保存超过一页，不要求用户输入仍在最新一页。
  await getComposer(page).fill("单轮历史接缝")
  await getComposer(page).press("Enter")
  await expect(page).toHaveURL(/\/c\/[^/]+$/)
  await expect(
    getMessages(page).getByText("长轮最终结果", { exact: true })
  ).toBeVisible()
  await expect(
    getMessages(page).locator('[data-turn-status="completed"]').last()
  ).toBeVisible()
  await page.reload()
  const messages = getMessages(page)
  const turn = messages.locator("[data-turn-id]").last()
  const group = turn.locator('[data-anchor-key^="group:"]').first()
  await expect(
    group.getByRole("img", { name: "此组中有工具调用失败", exact: true })
  ).toBeVisible()
  await group
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    .click()
  const groupKey = (await group.getAttribute("data-anchor-key"))!
  const tool = turn
    .locator("[data-item-key]")
    .filter({ hasText: "执行" })
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    .last()
  await tool.click()
  const toolKey = (await tool
    .locator("xpath=ancestor::*[@data-item-key][1]")
    .getAttribute("data-item-key"))!
  const toolNode = await tool.elementHandle()
  // 1. 当前页尚无调用的工具结果仍能单独检查。
  const orphanResult = turn
    .locator("[data-item-key]")
    .filter({ hasText: "工具结果" })
    .filter({ has: page.getByRole("img", { name: "失败", exact: true }) })
    .first()
  await expect(
    orphanResult.getByRole("img", { name: "失败", exact: true })
  ).toBeVisible()
  await orphanResult
    .getByRole("button", { name: "调试信息", exact: true })
    .click()
  const debug = page.locator('[data-debug-kind="item"]')
  await debug.getByRole("button", { name: "复制", exact: true }).click()
  const before = JSON.parse(
    await page.evaluate(() => navigator.clipboard.readText())
  )
  expect(before.item.kind).toBe("tool_result")
  await getMessageScroller(page).evaluate((node) => {
    node.scrollTop = 0
  })
  await expect(turn.locator('[data-anchor-key^="group:"]')).toHaveCount(2)
  const preservedGroup = turn
    .locator(`[data-anchor-key='${groupKey}']`)
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
  await expect(preservedGroup).toHaveAttribute("aria-expanded", "true")
  const preservedTool = turn
    .locator(`[data-item-key='${toolKey}']`)
    .filter({ hasText: "执行" })
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
  await expect(preservedTool).toHaveAttribute("aria-expanded", "true")
  expect(await toolNode!.evaluate((node) => node.isConnected)).toBe(true)
  // 2. 早页加载出调用后，原调试选择跟随同一个结果，展示真实调用和结果的关联。
  await debug.getByRole("button", { name: "复制", exact: true }).click()
  const after = JSON.parse(
    await page.evaluate(() => navigator.clipboard.readText())
  )
  expect(after.item.kind).toBe("tool_result")
  expect(after.item.id).toBe(before.item.id)
  expect(after.call.kind).toBe("tool_call")
  expect(after.call.tool_call.call_id).toBe(after.item.tool_result.call_id)
  await expect(
    turn.getByRole("button", { name: "复制结果", exact: true })
  ).toBeAttached()
})

test("short_grouped_history_loads_one_page_only_after_keyboard_up", async ({
  page,
  fixture,
}) => {
  const tools = Array.from({ length: 80 }, (_, index) => ({
    name: "bash",
    arguments: JSON.stringify({ command: `printf keyboard-page-${index}` }),
  }))
  await fixture.replies({ tools }, { text: "键盘历史最终结果" })
  await fixture.openApp(page)
  await getComposer(page).fill("键盘逐页上翻")
  await getComposer(page).press("Enter")
  await expect(page).toHaveURL(/\/c\/[^/]+$/)
  await expect(
    getMessages(page).getByText("键盘历史最终结果", { exact: true })
  ).toBeVisible()
  await expect(
    getMessages(page).locator('[data-turn-status="completed"]').last()
  ).toBeVisible()
  const historyRequests: unknown[] = []
  page.on("websocket", (socket) =>
    socket.on("framesent", (frame) => {
      if (typeof frame.payload !== "string") return
      const message = JSON.parse(frame.payload)
      if (
        message.method === "session/items/list" &&
        message.params?.order === "desc"
      )
        historyRequests.push(message)
    })
  )
  await page.reload()
  const messages = getMessages(page)
  await expect(
    messages.locator('[data-anchor-key^="group:"]').first()
  ).toBeVisible()
  expect(historyRequests.length).toBe(1)
  expect(
    await getMessageScroller(page).evaluate(
      (node) => node.scrollHeight <= node.clientHeight
    )
  ).toBe(true)
  await messages.focus()
  await messages.press("PageUp")
  await expect.poll(() => historyRequests.length).toBe(2)
  await expect(messages.getByText("键盘逐页上翻", { exact: true })).toHaveCount(
    0
  )
  // 最后一条回复已加载且结束，不等整轮历史加载完也能复制。
  await expect(
    messages.getByRole("button", { name: "复制结果", exact: true })
  ).toHaveCount(1)
  expect(historyRequests.length).toBe(2)
})
