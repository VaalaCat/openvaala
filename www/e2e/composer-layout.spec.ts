import {
  expect,
  getComposer,
  getMessages,
  getMessageScroller,
  startConversation,
  test,
} from "./fixture"
import type { ModelList } from "../src/lib/api/protocol.gen"

test("composer_compact_pending_and_stop", async ({ page, fixture }) => {
  // 1. 默认输入框只有一行，Shift+Enter 增高，清空后恢复。
  await page.setViewportSize({ width: 1280, height: 844 })
  const longModelName = "DeepSeek Reasoning Model With A Long Display Name"
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    const modelRequests = new Set<number>()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message))
      if (request.method === "model/list") modelRequests.add(request.id)
      server.send(message)
    })
    server.onMessage((message) => {
      const response = JSON.parse(String(message))
      if (modelRequests.delete(response.id) && response.result) {
        for (const model of (response.result as ModelList).models)
          if (model) model.display_name = longModelName
        socket.send(JSON.stringify(response))
      } else socket.send(message)
    })
  })
  await fixture.openApp(page)
  const composer = getComposer(page)
  await expect(composer).toHaveCSS("height", "40px")
  await expect(composer).toHaveCSS("font-size", "14px")
  await expect(composer).toHaveCSS("line-height", "24px")
  await expect(composer).toHaveCSS("padding-top", "8px")
  await expect(composer).toHaveCSS("padding-bottom", "8px")
  await composer.fill("第一行")
  await composer.press("End")
  await composer.press("Shift+Enter")
  await composer.pressSequentially("第二行")
  await expect(composer).toHaveValue("第一行\n第二行")
  await expect(composer).toHaveCSS("height", "64px")
  await composer.fill("")
  await expect(composer).toHaveCSS("height", "40px")
  const send = page.getByRole("button", { name: "发送", exact: true })
  const sendBackground = await send.evaluate(
    (node) => getComputedStyle(node).backgroundColor
  )

  // 2. Enter 发起真实运行，主按钮原位变形；停止在浅色和深色下都使用红底白图标。
  await fixture.replies({
    text: Array.from(
      { length: 32 },
      (_, index) => `可继续阅读的正文 ${index}。`
    ).join("\n\n"),
    hold: true,
  })
  await startConversation(page, "紧凑输入布局")
  const messages = getMessages(page)
  const scroller = getMessageScroller(page)
  const getBottomGap = () =>
    scroller.evaluate((node) =>
      Math.round(node.scrollHeight - node.scrollTop - node.clientHeight)
    )
  await expect(
    messages.getByText("可继续阅读的正文 31。", { exact: true })
  ).toBeVisible()
  const stop = page.getByRole("button", { name: "中断", exact: true })
  await expect(stop).toBeEnabled()
  const mainButton = await stop.elementHandle()
  const mainIcon = await stop.locator("svg").elementHandle()
  await expect(stop).toHaveCSS("height", "32px")
  for (const { colorScheme, red } of [
    { colorScheme: "light", red: [209, 71, 71] },
    { colorScheme: "dark", red: [209, 97, 97] },
  ] as const) {
    await page.emulateMedia({ colorScheme })
    for (const hovered of [false, true]) {
      if (hovered) await stop.hover()
      else await page.mouse.move(0, 0)
      await expect(stop.locator("svg")).toHaveCSS("color", "rgb(255, 255, 255)")
      await expect
        .poll(async () => {
          const actual = await stop.evaluate((element) => {
            const canvas = document.createElement("canvas")
            canvas.width = canvas.height = 1
            const context = canvas.getContext("2d")!
            context.fillStyle = getComputedStyle(element).backgroundColor
            context.fillRect(0, 0, 1, 1)
            return [...context.getImageData(0, 0, 1, 1).data]
          })
          const expected = [...red, hovered ? 153 : 255]
          return actual.every(
            (value, index) => Math.abs(value - expected[index]) <= 1
          )
        })
        .toBe(true)
    }
  }
  await page.emulateMedia({ colorScheme: "light" })
  await composer.fill("主按钮变为发送")
  await page.mouse.move(0, 0)
  await expect(send).toBeEnabled()
  expect(
    await mainButton!.evaluate(
      (node) => node.isConnected && node.getAttribute("aria-label") === "发送"
    )
  ).toBe(true)
  expect(await mainIcon!.evaluate((node) => node.isConnected)).toBe(true)
  await expect(send).toHaveCSS("background-color", sendBackground)
  await expect.poll(getBottomGap).toBeLessThanOrEqual(1)
  await composer.fill("")
  await expect(stop).toBeEnabled()
  // 清空输入时输入框不会短于一行，消息区仍贴底。
  expect(await getBottomGap()).toBeLessThanOrEqual(1)

  // 3. 排两条队列，托盘放在输入框上沿。
  const getViewportHeight = () => scroller.evaluate((node) => node.clientHeight)
  for (const text of ["队列第一条", "队列第二条"]) {
    await composer.fill(text)
    await page.getByRole("button", { name: "排队", exact: true }).click()
    await expect(composer).toHaveValue("")
  }
  const pending = page.getByRole("region", { name: "待处理", exact: true })
  const inputGroup = composer.locator(
    'xpath=ancestor::*[@data-slot="input-group"][1]'
  )
  const toolbar = inputGroup.locator('[data-align="block-end"]')
  const header = pending.getByRole("button", { name: /^待处理 2/ })
  const rows = pending.locator("[data-pending-input]")
  await expect(inputGroup.locator("section")).toHaveCount(1)
  await expect(rows).toHaveCount(2)
  await expect(toolbar.locator('button[aria-label="排队"]')).toHaveCount(0)

  // 4. 宽屏和最窄屏各检查一遍：每行 32px、文字与输入文字同一起点；收起后标题行预览下一条，消息视口不变且仍贴底；
  //    操作栏单行且没有待处理入口；输入框上方可见的每个文字行都不被托盘或按钮覆盖（输入框下面被遮住是设计）。
  for (const { width, colorScheme } of [
    { width: 1280, colorScheme: "light" },
    { width: 390, colorScheme: "dark" },
    { width: 320, colorScheme: "light" },
  ] as const) {
    await page.setViewportSize({ width, height: 844 })
    await page.emulateMedia({ colorScheme })
    await expect(composer).toHaveCSS("font-size", width < 768 ? "16px" : "14px")
    // 左组与右组各自紧凑排列；输入出现排队按钮后也保持方形、4px间距和同一行。
    const model = toolbar.locator('[aria-label="模型"]')
    const context = toolbar.getByRole("button", { name: /^上下文占用：/ })
    const image = toolbar.getByRole("button", { name: "添加图片", exact: true })
    const queue = toolbar.getByRole("button", { name: "排队", exact: true })
    for (const hasDraft of [true, false]) {
      await composer.fill(hasDraft ? "检查排队按钮尺寸" : "")
      const main = hasDraft ? send : stop
      await expect(main).toBeEnabled()
      if (hasDraft) await expect(queue).toBeVisible()
      else await expect(queue).toHaveCount(0)
      for (const button of hasDraft ? [queue, main] : [main]) {
        await expect(button).toHaveCSS("width", "32px")
        await expect(button).toHaveCSS("height", "32px")
      }
      for (const [left, right] of [
        [image, model],
        [context, hasDraft ? queue : main],
        ...(hasDraft ? [[queue, main]] : []),
      ]) {
        await expect
          .poll(async () => {
            const leftBounds = (await left.boundingBox())!
            const rightBounds = (await right.boundingBox())!
            return rightBounds.x - leftBounds.x - leftBounds.width
          })
          .toBeCloseTo(4, 1)
      }
      const bounds = (await inputGroup.boundingBox())!
      const imageBounds = (await image.boundingBox())!
      const mainBounds = (await main.boundingBox())!
      // 按钮到输入框左、右、下三边等距，正文仍保留12px内边距。
      for (const side of ["left", "right", "bottom"])
        await expect(toolbar).toHaveCSS(`padding-${side}`, "8px")
      const bottomGap =
        bounds.y + bounds.height - mainBounds.y - mainBounds.height
      expect(imageBounds.x - bounds.x).toBeCloseTo(bottomGap, 1)
      expect(
        bounds.x + bounds.width - mainBounds.x - mainBounds.width
      ).toBeCloseTo(bottomGap, 1)
      await expect(composer).toHaveCSS("padding-left", "12px")
      await expect(composer).toHaveCSS("padding-right", "12px")
      const buttons = await toolbar.locator("button:visible").all()
      for (const button of buttons) {
        const box = (await button.boundingBox())!
        expect(box.y).toBe((await main.boundingBox())!.y)
        expect(box.x).toBeGreaterThanOrEqual(bounds.x)
        expect(box.x + box.width).toBeLessThanOrEqual(bounds.x + bounds.width)
      }
      if (hasDraft)
        await page.screenshot({
          path: test.info().outputPath(`${width}-${colorScheme}-queue.png`),
        })
    }
    // 改变窗口宽度不会自动贴底，先回到底部再量。
    await scroller.evaluate((node) => {
      node.scrollTop = node.scrollHeight
    })
    for (const row of await rows.all())
      await expect(row).toHaveCSS("height", "32px")
    await expect(pending).toHaveCSS("height", "97px")
    const getTextStart = () =>
      Promise.all([
        rows
          .first()
          .locator("p")
          .evaluate((node) => {
            const range = document.createRange()
            range.selectNodeContents(node)
            return Math.round(range.getClientRects()[0].left)
          }),
        composer.evaluate((node) =>
          Math.round(
            node.getBoundingClientRect().left +
              parseFloat(getComputedStyle(node).paddingLeft)
          )
        ),
      ])
    const [rowTextStart, inputTextStart] = await getTextStart()
    expect(rowTextStart).toBe(inputTextStart)
    await expect(toolbar.locator('[aria-label="模型"]')).toContainText(
      longModelName
    )
    // 先量展开时的输入框和消息视口，收起后输入框少 64px、视口不变。
    const inputHeight = (await inputGroup.boundingBox())!.height
    const viewportHeight = await getViewportHeight()
    await expect.poll(getBottomGap).toBeLessThanOrEqual(1)
    await header.click()
    await expect(header).toHaveAttribute("aria-expanded", "false")
    await expect(rows.first()).toBeHidden()
    await expect(header).toContainText("队列第一条")
    await expect
      .poll(async () =>
        Math.round(inputHeight - (await inputGroup.boundingBox())!.height)
      )
      .toBe(64)
    expect(await getViewportHeight()).toBe(viewportHeight)
    await expect(composer).toHaveCSS("height", "40px")
    await expect.poll(getBottomGap).toBeLessThanOrEqual(1)

    for (const expanded of [false, true]) {
      if (expanded) {
        await header.click()
        await expect(rows.first()).toBeVisible()
        await expect
          .poll(async () =>
            Math.round((await inputGroup.boundingBox())!.height)
          )
          .toBe(Math.round(inputHeight))
        expect(await getViewportHeight()).toBe(viewportHeight)
      }
      // 输入框贴在消息区底部；回到底部后，托盘标题在输入框内部、消息正文的下方。
      await scroller.evaluate((node) => {
        node.scrollTop = node.scrollHeight
      })
      await expect.poll(getBottomGap).toBeLessThanOrEqual(1)
      const headerBounds = (await header.boundingBox())!
      const messageBounds = (await messages.boundingBox())!
      const inputBounds = (await inputGroup.boundingBox())!
      expect(inputBounds.x).toBeCloseTo(messageBounds.x, 0)
      expect(inputBounds.width).toBeCloseTo(messageBounds.width, 0)
      expect(headerBounds.height).toBe(32)
      expect(headerBounds.y).toBeGreaterThanOrEqual(inputBounds.y)
      expect(headerBounds.y + headerBounds.height).toBeLessThanOrEqual(
        inputBounds.y + inputBounds.height
      )
      expect(headerBounds.y).toBeGreaterThanOrEqual(
        messageBounds.y + messageBounds.height
      )
      await expect(
        toolbar.getByRole("button", { name: /^待处理/ })
      ).toHaveCount(0)
      const controls = await toolbar.locator("button:visible").all()
      const toolbarTop = (await controls[0].boundingBox())!.y
      for (const control of controls) {
        const bounds = (await control.boundingBox())!
        expect(bounds.x).toBeGreaterThanOrEqual(inputBounds.x)
        expect(bounds.x + bounds.width).toBeLessThanOrEqual(
          inputBounds.x + inputBounds.width
        )
        expect(bounds.y + bounds.height).toBeLessThanOrEqual(
          inputBounds.y + inputBounds.height
        )
        expect(bounds.y).toBe(toolbarTop)
        if (await control.isEnabled()) await control.click({ trial: true })
      }
      expect(
        await inputGroup.evaluate(
          (node) => node.scrollWidth <= node.clientWidth
        )
      ).toBe(true)
      const samples = await messages.evaluate((region) => {
        // 只看消息区可视区顶部到输入框所在 form 顶部之间的行。
        const viewport = region.closest('[data-slot="message-scroller"]')!
        const clip = {
          top: viewport.getBoundingClientRect().top,
          bottom: viewport.querySelector("form")!.getBoundingClientRect().top,
        }
        return [...region.querySelectorAll("p")].flatMap((paragraph) => {
          const range = document.createRange()
          range.selectNodeContents(paragraph)
          return [...range.getClientRects()].flatMap((rect) => {
            const top = Math.max(rect.top, clip.top)
            const bottom = Math.min(rect.bottom, clip.bottom)
            if (bottom - top < 2 || rect.width === 0) return []
            const y = (top + bottom) / 2
            const target = document.elementFromPoint(
              rect.left + rect.width / 2,
              y
            )
            return [
              {
                text: paragraph.textContent,
                x: rect.left + rect.width / 2,
                y,
                clip: { top: clip.top, bottom: clip.bottom },
                line: { top: rect.top, bottom: rect.bottom },
                hit: target?.tagName,
                visible:
                  !!target &&
                  region.contains(target) &&
                  target.closest("button, form") === null,
              },
            ]
          })
        })
      })
      expect(samples.length).toBeGreaterThan(0)
      expect(samples.filter((sample) => !sample.visible)).toEqual([])
      const scrollTop = await scroller.evaluate((node) => node.scrollTop)
      await page.mouse.move(
        inputBounds.x + inputBounds.width / 2,
        inputBounds.y - 8
      )
      await page.mouse.wheel(0, -120)
      await expect
        .poll(() => scroller.evaluate((node) => node.scrollTop))
        .toBeLessThan(scrollTop)
      await page.screenshot({
        path: test
          .info()
          .outputPath(`${width}-${expanded ? "expanded" : "collapsed"}.png`),
      })
    }
  }

  // 5. 斜杠菜单与待处理入口分开，展开后取回与菜单照常工作。
  await composer.fill("/display")
  await composer.press("End")
  const skills = page.getByRole("listbox", { name: "Skill", exact: true })
  await expect(skills).toBeVisible()
  await header.click({ trial: true })
  await send.click({ trial: true })
  await page
    .getByRole("button", { name: "排队", exact: true })
    .click({ trial: true })
  await composer.press("Escape")
  await expect(skills).toBeHidden()
  await composer.fill("")
  const first = rows.filter({ hasText: "队列第一条" })
  await first.getByRole("button", { name: "待处理消息的更多操作" }).click()
  await expect(
    page.getByRole("menuitem", { name: "插队", exact: true })
  ).toBeVisible()
  await page.keyboard.press("Escape")
  await first.getByRole("button", { name: "取回修改" }).click()
  await expect(composer).toHaveValue("队列第一条")
  await expect(rows).toHaveCount(1)
  await composer.fill("")
  await rows.getByRole("button", { name: "取回修改" }).click()
  await expect(composer).toHaveValue("队列第二条")
  await expect(pending).toHaveCount(0)
  await expect(
    inputGroup.getByRole("button", { name: /^待处理 \d/ })
  ).toHaveCount(0)
  await composer.fill("")
  await expect
    .poll(() =>
      inputGroup.evaluate(
        (group) =>
          group.getBoundingClientRect().top -
          group.closest("form")!.getBoundingClientRect().top
      )
    )
    .toBe(0)

  // 6. 一次点击直接中断，无确认弹窗；同一主按钮恢复发送样式。
  await stop.click()
  await expect(
    messages.getByText("这一轮已中断", { exact: true })
  ).toBeVisible()
  await expect(page.getByRole("alertdialog")).toHaveCount(0)
  await expect(page.getByRole("dialog")).toHaveCount(0)
  await expect(send).toBeDisabled()
  expect(await mainButton!.evaluate((node) => node.isConnected)).toBe(true)
  await page.mouse.move(0, 0)
  await expect(send).toHaveCSS("background-color", sendBackground)
  await expect(composer).toHaveCSS("height", "40px")
})
