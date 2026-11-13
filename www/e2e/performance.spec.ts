import {
  expect,
  expandTurnProcess,
  getComposer,
  getFirstVisibleTurn,
  getMessages,
  getMessageScroller,
  loadEarlierTurnsUntilFirst,
  startConversation,
  test,
} from "./fixture"

test("highlight_worker_load_failure_keeps_streaming_plain_text", async ({
  page,
  fixture,
}) => {
  // 1. 只阻断高亮线程资源，模型与页面仍连接真实 serve。
  const errors: string[] = []
  const blockedURLs = new Set<string>()
  page.on("pageerror", (error) => errors.push(error.message))
  page.on("console", (message) => {
    if (message.type() !== "error") return
    if (
      blockedURLs.has(message.location().url) &&
      message.text().includes("net::ERR_FAILED")
    )
      return
    errors.push(message.text())
  })
  await page.route("**/highlighter.worker-*.js", (route) => {
    blockedURLs.add(route.request().url())
    return route.abort()
  })
  const lines = Array.from(
    { length: 150 },
    (_, index) => `const fallback${index}: number = ${index};`
  )
  await fixture.replies({
    reasoningChunks: [
      "```typescript\n" + lines.slice(0, 120).join("\n") + "\n",
      ...lines.slice(120).map((line) => line + "\n"),
      "```",
    ],
    text: "降级完成",
    chunkDelayMs: 100,
  })
  await fixture.openApp(page)
  await startConversation(page, "高亮资源不可用时继续阅读")
  const turn = getMessages(page).locator("[data-turn-id]").last()
  await expect(turn.locator('[data-anchor-key^="group:"]')).toBeVisible()
  await expandTurnProcess(turn)
  const reasoning = turn
    .locator("[data-item-key]")
    .filter({ hasText: /思考中|思考完成/ })
  await reasoning
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    .click()
  const code = reasoning.locator("pre")

  // 2. 线程加载失败后，纯文本持续增长并完整结束，没有未处理异常。
  await expect.poll(() => blockedURLs.size).toBeGreaterThan(0)
  await expect(code).toContainText(lines[119])
  const before = (await code.textContent())!
  expect(before).not.toContain(lines[149])
  await expect
    .poll(async () => (await code.textContent())!.length)
    .toBeGreaterThan(before.length)
  await expect(turn.locator('[data-turn-status="completed"]')).toBeAttached()
  await expect(code).toHaveText(lines.join("\n"))
  await expect(turn.getByText("降级完成", { exact: true })).toBeVisible()
  expect(errors).toEqual([])
})

test("streaming_code_preserves_completed_tokens_and_selection", async ({
  page,
  fixture,
}) => {
  // 1. 真实模型先输出 120 行，再每 100ms 追加一行。
  const errors: string[] = []
  page.on("pageerror", (error) => errors.push(error.message))
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text())
  })
  await page.setViewportSize({ width: 1280, height: 844 })
  const lines = Array.from(
    { length: 180 },
    (_, index) =>
      `const value${String(index).padStart(3, "0")}: string = "value-${String(index).padStart(3, "0")}";`
  )
  await fixture.replies({
    reasoningChunks: [
      "```typescript\n" + lines.slice(0, 120).join("\n") + "\n",
      ...lines.slice(120).map((line) => line + "\n"),
      "```",
    ],
    text: "代码流结束",
    chunkDelayMs: 100,
  })
  await fixture.openApp(page)
  await startConversation(page, "持续着色")
  const turn = getMessages(page).locator("[data-turn-id]").last()
  await expect(turn.locator('[data-anchor-key^="group:"]')).toBeVisible()
  await expandTurnProcess(turn)
  const reasoning = turn
    .locator("[data-item-key]")
    .filter({ hasText: /思考中|思考完成/ })
  const reasoningToggle = reasoning.locator(
    'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
  )
  await reasoningToggle.click()
  const output = reasoning.locator("[data-output-block]")
  await output.getByRole("button", { name: "展开输出", exact: true }).click()
  const code = output.locator("pre")
  await expect(code).toContainText(lines[119])
  await expect
    .poll(() =>
      code.evaluate(
        (node) =>
          new Set(
            [...node.querySelectorAll("span")].map(
              (token) => getComputedStyle(token).color
            )
          ).size
      )
    )
    .toBeGreaterThan(1)

  // 2. 选中第一行已着色的文字，追加过程中原文字节点和选区不变。
  const firstLineNodes = await code.evaluateHandle((node) => {
    const nodes: Node[] = []
    const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT)
    for (let token = walker.nextNode(); token; token = walker.nextNode()) {
      if (token.textContent?.includes("\n")) break
      nodes.push(token, token.parentElement!)
    }
    if (!nodes.length) throw new Error("首行代码节点未找到")
    return nodes
  })
  const selectedToken = await code.evaluateHandle((node) => {
    const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT)
    let token = walker.nextNode()
    while (token && !token.textContent?.includes("value-000"))
      token = walker.nextNode()
    if (!token) throw new Error("首行代码文字未找到")
    const range = document.createRange()
    const start = token.textContent!.indexOf("value-000")
    range.setStart(token, start)
    range.setEnd(token, start + "value-000".length)
    const selection = window.getSelection()!
    selection.removeAllRanges()
    selection.addRange(range)
    return token
  })
  const codeNode = await code.elementHandle()
  const initialText = (await code.textContent())!
  expect(initialText).not.toContain(lines[179])
  const frameDurations = await page.evaluate(async () => {
    const durations: number[] = []
    const started = performance.now()
    let previous = started
    while (performance.now() - started < 1200) {
      const now = await new Promise<number>((resolve) =>
        requestAnimationFrame(resolve)
      )
      durations.push(now - previous)
      previous = now
    }
    return durations
  })
  await test.info().attach("streaming-frame-durations-ms", {
    body: JSON.stringify(frameDurations),
    contentType: "application/json",
  })
  expect((await code.textContent())!.length).toBeGreaterThan(initialText.length)
  expect(await selectedToken.evaluate((node) => node.isConnected)).toBe(true)
  expect(
    await firstLineNodes.evaluate((nodes) =>
      nodes.every((node) => node.isConnected)
    )
  ).toBe(true)
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(
    "value-000"
  )
  await expect(turn.locator('[data-turn-status="completed"]')).toBeAttached()
  await expect(code).toHaveText(lines.join("\n"))
  expect(await selectedToken.evaluate((node) => node.isConnected)).toBe(true)
  expect(
    await firstLineNodes.evaluate((nodes) =>
      nodes.every((node) => node.isConnected)
    )
  ).toBe(true)
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(
    "value-000"
  )

  // 3. 输出及思考收放后仍使用同一份已着色 DOM。
  await output.getByRole("button", { name: "收起输出", exact: true }).click()
  await output.getByRole("button", { name: "展开输出", exact: true }).click()
  await reasoningToggle.click()
  await expect(code).toBeHidden()
  await reasoningToggle.click()
  await expect(code).toBeVisible()
  expect(await codeNode!.evaluate((node) => node.isConnected)).toBe(true)
  expect(await selectedToken.evaluate((node) => node.isConnected)).toBe(true)
  await expect(code).toHaveText(lines.join("\n"))
  expect(errors).toEqual([])
})

test("streaming_below_paginated_history_keeps_reading_position", async ({
  page,
  fixture,
}) => {
  // 1. 真实长历史先打开最新一页，再发送持续流式回复。
  const turns = 30
  const errors: string[] = []
  page.on("pageerror", (error) => errors.push(error.message))
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text())
  })
  const { longConversationID } = await fixture.seed({
    conversations: 0,
    longConversationTurns: turns,
  })
  await page.setViewportSize({ width: 1280, height: 844 })
  await fixture.openApp(page)
  await page.goto(page.url() + "/c/" + longConversationID)
  const messages = getMessages(page)
  await expect(
    messages.getByText(`长对话第 ${turns} 轮`, { exact: true })
  ).toBeVisible()
  await expect(
    messages.getByText("长对话第 1 轮", { exact: true })
  ).toHaveCount(0)
  const reply = Array.from(
    { length: 12 },
    (_, index) => `持续输出 ${index}。`
  ).join("\n\n")
  await fixture.replies({ text: reply, chunkDelayMs: 50 })
  await getComposer(page).fill("继续生成时阅读历史")
  await getComposer(page).press("Enter")
  const streamingTurn = messages.locator("[data-turn-id]").last()
  await expect(streamingTurn).toContainText("持续输出 0。")

  // 2. 用户向上滚动，顶部加载一页；选中的旧消息仍停在同一位置。
  //    消息区只挂视口附近的行：等更早的一页加载完，视口里第一条用户消息就是原来已加载的最早一条。
  await getMessageScroller(page).hover()
  await page.mouse.wheel(0, -10_000)
  await expect(page.locator('[aria-label="正在加载更早的消息"]')).toHaveCount(0)
  const { text: earliest } = await getFirstVisibleTurn(page)
  const earlierTurn = Number(earliest.match(/\d+/)![0]) - 1
  await expect(
    messages.getByText(`长对话第 ${earlierTurn} 轮`, { exact: true })
  ).toBeAttached()
  const anchor = messages.getByText(earliest, { exact: true })
  await expect(anchor).toBeInViewport()
  const oldMessage = await anchor.elementHandle()
  await anchor.evaluate((node) => {
    const range = document.createRange()
    range.selectNodeContents(node)
    const selection = window.getSelection()!
    selection.removeAllRanges()
    selection.addRange(range)
  })
  const top = (await anchor.boundingBox())!.y
  const textBefore = (await streamingTurn.textContent())!
  const samples = await oldMessage!.evaluate(async (node) => {
    const positions: number[] = []
    const durations: number[] = []
    const started = performance.now()
    let previous = started
    while (performance.now() - started < 1800) {
      const now = await new Promise<number>((resolve) =>
        requestAnimationFrame(resolve)
      )
      durations.push(now - previous)
      positions.push(node.getBoundingClientRect().top)
      previous = now
    }
    return { positions, durations }
  })
  await test.info().attach("history-stream-frame-samples", {
    body: JSON.stringify(samples),
    contentType: "application/json",
  })
  expect(
    Math.max(...samples.positions.map((position) => Math.abs(position - top)))
  ).toBeLessThanOrEqual(2)
  expect((await streamingTurn.textContent())!.length).toBeGreaterThan(
    textBefore.length
  )
  expect(await oldMessage!.evaluate((node) => node.isConnected)).toBe(true)
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(
    earliest
  )

  // 3. 回复完成不会把阅读位置拉到底部，也不会重挂旧消息。
  await expect(
    streamingTurn.locator('[data-turn-status="completed"]')
  ).toBeAttached({ timeout: 30_000 })
  await expect(streamingTurn).toContainText("持续输出 11。")
  expect(Math.abs((await anchor.boundingBox())!.y - top)).toBeLessThanOrEqual(2)
  expect(await oldMessage!.evaluate((node) => node.isConnected)).toBe(true)
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(
    earliest
  )
  await expect(
    page.getByRole("button", { name: "回到底部", exact: true })
  ).toBeVisible()
  expect(errors).toEqual([])
})

for (const viewport of [
  { name: "desktop", width: 1280, colorScheme: "light" as const },
  { name: "mobile", width: 390, colorScheme: "dark" as const },
]) {
  test(`long_history_keeps_selection_and_streaming_${viewport.name}`, async ({
    page,
    fixture,
  }) => {
    test.setTimeout(120_000)
    // 1. 预置多轮 Markdown 回复，经消息区分页加载全部历史。
    const turns = 80
    const historyReply =
      "## 历史回复\n\n" +
      Array.from(
        { length: 12 },
        (_, index) => `- 要点 ${index + 1}：保留历史消息、阅读位置和文字选择。`
      ).join("\n")
    const errors: string[] = []
    page.on("pageerror", (error) => errors.push(error.message))
    await page.setViewportSize({ width: viewport.width, height: 844 })
    await page.emulateMedia({ colorScheme: viewport.colorScheme })
    await fixture.replies(
      ...Array.from({ length: turns }, () => ({ text: historyReply }))
    )
    const { longConversationID } = await fixture.seed({
      conversations: 0,
      longConversationTurns: turns,
    })
    // 记下打开对话期间同时挂着的最多行数：首屏不先多挂几行再卸掉
    await page.addInitScript(() => {
      const recorder = window as unknown as { __maxMountedRows: number }
      recorder.__maxMountedRows = 0
      new MutationObserver(() => {
        recorder.__maxMountedRows = Math.max(
          recorder.__maxMountedRows,
          document.querySelectorAll(
            '[data-slot="message-scroller"] [data-row-key]'
          ).length
        )
      }).observe(document, { childList: true, subtree: true })
    })
    await fixture.openApp(page)
    await page.goto(page.url() + "/c/" + longConversationID)
    const messages = getMessages(page)
    const scroller = getMessageScroller(page)
    const oldest = messages.getByText("长对话第 1 轮", { exact: true })
    await expect(
      messages.getByText(`长对话第 ${turns} 轮`, { exact: true })
    ).toBeVisible()
    expect(
      await messages.locator("[data-turn-id]").count()
    ).toBeLessThanOrEqual(10)
    expect(
      await page.evaluate(
        () =>
          (window as unknown as { __maxMountedRows: number }).__maxMountedRows
      )
    ).toBeLessThanOrEqual(5)
    await loadEarlierTurnsUntilFirst(page)

    // 2. 来回滚动后在最早消息选中文字。
    await scroller.evaluate((node) => {
      node.scrollTop = 0
    })
    await expect(oldest).toBeInViewport()
    await scroller.evaluate((node) => {
      node.scrollTop = node.scrollHeight
    })
    await expect(
      messages.getByText(`长对话第 ${turns} 轮`, { exact: true })
    ).toBeInViewport()
    await scroller.evaluate((node) => {
      node.scrollTop = 0
    })
    await expect(oldest).toBeInViewport()
    await oldest.evaluate((node) => {
      const range = document.createRange()
      range.selectNodeContents(node)
      const selection = window.getSelection()!
      selection.removeAllRanges()
      selection.addRange(range)
    })
    const top = (await oldest.boundingBox())!.y

    // 3. 另一客户端触发快速流式回复，页面完整接收，历史位置和选区不变。
    const reply = "持续输出，保持阅读位置。".repeat(80) + "流式结束"
    await fixture.replies({ text: reply, chunkDelayMs: 1 })
    await fixture.submitInput(longConversationID, [
      { kind: "text", text: { text: "继续长对话" } },
    ])
    const streamingTurn = messages.locator("[data-turn-id]").last()
    await expect(streamingTurn).toContainText("继续长对话")
    await expect(
      streamingTurn.locator('[data-turn-status="completed"]')
    ).toBeAttached({ timeout: 30_000 })
    await expect(streamingTurn).toContainText(reply)
    expect(Math.abs((await oldest.boundingBox())!.y - top)).toBeLessThanOrEqual(
      2
    )
    expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(
      "长对话第 1 轮"
    )
    await page.evaluate(() => window.getSelection()?.removeAllRanges())
    await page.getByRole("button", { name: "回到底部", exact: true }).click()
    await expect(
      streamingTurn.getByText(reply, { exact: true })
    ).toBeInViewport()
    expect(errors).toEqual([])
  })
}
