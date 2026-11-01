import {
  expect,
  getComposer,
  getMessages,
  startConversation,
  test,
} from "./fixture"

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
    await expect(dot).toHaveCSS("transform", "none")
    await observer.emulateMedia({ reducedMotion: "reduce" })
    await expect(ripple).toHaveCount(0)
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
