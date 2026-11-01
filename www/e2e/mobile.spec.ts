import {
  expect,
  getComposer,
  getMessages,
  startConversation,
  test,
} from "./fixture"

const viewport = { width: 390, height: 844 }

test.use({ viewport })

test("model_controls_fit_text_and_preserve_composer_actions", async ({
  page,
  fixture,
}) => {
  // 1. 经模型服务表单保存短、长显示名和三种推理强度。
  await page.setViewportSize({ width: 1440, height: 900 })
  await fixture.openApp(page)
  await page.getByRole("link", { name: "设置", exact: true }).click()
  await page.getByRole("tab", { name: "模型服务", exact: true }).click()
  await page.getByRole("button", { name: "新增模型服务" }).first().click()
  await page.getByLabel("ID", { exact: true }).fill("fit-model-controls")
  await page.getByLabel("base_url", { exact: true }).fill(fixture.modelBaseURL)
  await page.getByLabel("模型 ID", { exact: true }).fill("short long")
  await page.getByRole("button", { name: "添加", exact: true }).click()
  const longName = "DeepSeek Reasoning Model"
  for (const [id, name] of [
    ["short", "DS"],
    ["long", longName],
  ]) {
    await page
      .getByRole("button", { name: `编辑模型 ${id}`, exact: true })
      .click()
    const fields = page.locator(`#model-fields-${id}`)
    await fields.getByLabel("显示名", { exact: true }).fill(name)
    await fields.getByLabel("上下文窗口", { exact: true }).fill("128K")
    await fields
      .getByRole("combobox", { name: "推理强度列表", exact: true })
      .click()
    await page.getByRole("option", { name: "自定义", exact: true }).click()
    await fields
      .getByLabel("强度（逗号分隔）", { exact: true })
      .fill("low, max, medium")
    await fields
      .getByRole("combobox", { name: "默认推理强度", exact: true })
      .click()
    await page.getByRole("option", { name: "low", exact: true }).click()
  }
  await page.getByRole("button", { name: "创建", exact: true }).click()
  try {
    await expect(
      page.getByRole("link", { name: /fit-model-controls/ })
    ).toBeVisible()
    await page.getByRole("link", { name: "新对话", exact: true }).click()
    await startConversation(page, "模型入口宽度")
    const composer = page.locator("form").filter({ has: getComposer(page) })
    const send = composer.getByRole("button", { name: "发送", exact: true })
    await expect(send).toBeDisabled()
    await getComposer(page).fill("保留发送入口")
    await expect(send).toBeEnabled()

    // 2. 桌面独立按钮和最窄屏合并入口随文字展开，最窄屏仍放得下最长的模型名，Range 测量真实字形后的间隙。
    for (const width of [1440, 320]) {
      await page.setViewportSize({ width, height: 900 })
      const compact = width < 400
      const model = composer.getByRole(compact ? "button" : "combobox", {
        name: "模型",
        exact: true,
      })
      const drawer = page.getByRole("dialog", { name: "模型", exact: true })
      const widths: number[] = []
      for (const name of ["DS", longName]) {
        await model.click()
        await page
          .getByRole(compact ? "searchbox" : "combobox", {
            name: "搜索模型",
            exact: true,
          })
          .fill(name)
        const option = page.getByRole(compact ? "button" : "option", {
          name: new RegExp(`^${name}`),
        })
        await option.click()
        if (compact) {
          await expect(option).toHaveAttribute("aria-pressed", "true")
          await expect(option.getByText(name, { exact: true })).toBeVisible()
          expect(
            await option
              .getByText(name, { exact: true })
              .evaluate((node) => node.scrollWidth <= node.clientWidth)
          ).toBe(true)
          await drawer.getByRole("button", { name: "关闭模型选择" }).click()
          await expect(drawer).toHaveCount(0)
        }
        await expect(model).toContainText(name)
        // 短名称从默认的 low 依次切换：桌面经过字数相同的 max 再到 medium，最窄屏直接到 medium；长名称只检查 medium。
        const efforts =
          name !== "DS"
            ? ["medium"]
            : compact
              ? ["low", "medium"]
              : ["low", "max", "medium"]
        for (const effort of efforts) {
          const effortControl = compact
            ? model
            : composer.getByRole("combobox", { name: /^推理强度：/ })
          await effortControl.click()
          await page
            .getByRole(compact ? "button" : "option", {
              name: effort,
              exact: true,
            })
            .click()
          if (compact)
            await drawer.getByRole("button", { name: "关闭模型选择" }).click()
          await expect(effortControl).toContainText(effort)
          for (const control of compact ? [model] : [model, effortControl]) {
            await expect(control).toHaveCSS("height", "32px")
            await expect(control).toHaveCSS("font-weight", "400")
          }
          if (name === "DS") {
            const labels = [name, effort]
            for (const label of labels) {
              const control = label === name ? model : effortControl
              const gap = await control
                .getByText(label, { exact: true })
                .evaluate((node) => {
                  const range = document.createRange()
                  range.selectNodeContents(node)
                  const next = node.nextElementSibling!
                  return (
                    next.getBoundingClientRect().left -
                    range.getBoundingClientRect().right
                  )
                })
              expect(gap).toBeGreaterThanOrEqual(5)
              expect(gap).toBeLessThanOrEqual(9)
            }
            widths.push((await effortControl.boundingBox())!.width)
          }
          const controls = compact ? [model] : [model, effortControl]
          controls.push(
            composer.getByRole("button", { name: /^上下文占用：/ }),
            send
          )
          let right = 0
          for (const control of controls) {
            await expect(control).toBeVisible()
            const box = (await control.boundingBox())!
            expect(box.x).toBeGreaterThanOrEqual(right)
            expect(box.x + box.width).toBeLessThanOrEqual(width)
            right = box.x + box.width
          }
        }
        if (name === longName) {
          const label = model.getByText(name, { exact: true })
          await expect(label).toHaveCSS("text-overflow", "ellipsis")
          expect(
            await label.evaluate((node) => node.scrollWidth > node.clientWidth)
          ).toBe(true)
          if (!compact) {
            expect((await model.boundingBox())!.width).toBeLessThanOrEqual(192)
            await label.hover()
            await expect(
              page.locator('[data-slot="tooltip-content"]:visible')
            ).toHaveText(name)
            await getComposer(page).hover()
          }
        }
      }
      // 入口随文字变宽：low 与 max 字数相同，只有真实字形更宽；medium 字数最多。
      for (let index = 1; index < widths.length; index++)
        expect(widths[index]).toBeGreaterThan(widths[index - 1])
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth
        )
      ).toBe(true)
    }
  } finally {
    // 3. 删除本用例创建的模型服务，不改变共享默认模型。
    await page.setViewportSize({ width: 1440, height: 900 })
    await fixture.openApp(page)
    await page.getByRole("link", { name: "设置", exact: true }).click()
    await page.getByRole("tab", { name: "模型服务", exact: true }).click()
    await page.getByRole("link", { name: /fit-model-controls/ }).click()
    await page.getByRole("button", { name: "删除", exact: true }).click()
    await page
      .getByRole("alertdialog")
      .getByRole("button", { name: "删除", exact: true })
      .click()
    await expect(
      page.getByRole("link", { name: /fit-model-controls/ })
    ).toHaveCount(0)
  }
})

test("mobile_editing_text_is_larger_but_message_and_selection_stay_regular", async ({
  page,
  fixture,
}) => {
  // 1. 窄屏只放大编辑文字，模型入口和选项使用常规字重。
  await fixture.openApp(page)
  await expect(getComposer(page)).toHaveCSS("font-size", "16px")
  const model = page.getByRole("button", { name: "模型", exact: true })
  await expect(model).toHaveCSS("font-weight", "400")
  await model.click()
  const drawer = page.getByRole("dialog", { name: "模型", exact: true })
  await expect(drawer.getByRole("searchbox")).toHaveCSS("font-size", "16px")
  await expect(drawer.getByRole("button", { name: /default/ })).toHaveCSS(
    "font-weight",
    "400"
  )
  await drawer.getByRole("button", { name: "关闭模型选择" }).click()
  await expect(drawer).toHaveCount(0)

  // 2. 消息正文不跟随输入字号，桌面编辑文字仍为 14px。
  const message = "输入字号检查"
  await startConversation(page, message)
  await expect(getMessages(page).getByText(message, { exact: true })).toHaveCSS(
    "font-size",
    "14px"
  )
  await expect(getComposer(page)).toHaveCSS("font-size", "16px")
  await page.setViewportSize({ width: 1280, height: 800 })
  await expect(getComposer(page)).toHaveCSS("font-size", "14px")
})

test("mobile_server_moves_to_sidebar_and_title_yields", async ({
  page,
  fixture,
}) => {
  const toggle = page
    .getByRole("button", { name: "切换侧栏" })
    .filter({ visible: true })
  const getTitleX = () =>
    page
      .locator("header nav")
      .evaluate((node) => node.getBoundingClientRect().x)
  const sidebar = page.getByRole("dialog").filter({
    has: page.getByRole("link", { name: "新对话" }),
  })
  for (const reducedMotion of ["no-preference", "reduce"] as const) {
    // 1. 减少动画偏好在页面加载时读取，换偏好后重新打开页面再采样同一个交接。
    await page.emulateMedia({ reducedMotion })
    await fixture.openApp(page)
    // 窄屏检测后，先等顶栏完成首次让位，再采样抽屉交接。
    await expect.poll(getTitleX).toBeCloseTo(96, 1)
    const { positions, servers } = await toggle.evaluate(async (button) => {
      const title = document.querySelector("header nav")!
      const positions = [title.getBoundingClientRect().x]
      const servers: { x: number; opacity: number; inert: boolean }[][] = []
      ;(button as HTMLElement).click()
      for (let index = 0; index < 24; index++) {
        await new Promise<void>((resolve) =>
          requestAnimationFrame(() => resolve())
        )
        positions.push(title.getBoundingClientRect().x)
        servers.push(
          [...document.querySelectorAll('button[aria-label="已连接"]')].map(
            (node) => {
              let opacity = 1
              for (
                let parent: Element | null = node;
                parent;
                parent = parent.parentElement
              )
                opacity *= Number(getComputedStyle(parent).opacity)
              return {
                x: node.getBoundingClientRect().x,
                opacity,
                inert: !!node.closest("[inert]"),
              }
            }
          )
        )
      }
      return { positions, servers }
    })
    expect(positions[0] - positions.at(-1)!).toBeCloseTo(40, 0)
    const hasIntermediatePosition = positions.some(
      (x) => x < positions[0] - 1 && x > positions.at(-1)! + 1
    )
    expect(hasIntermediatePosition).toBe(reducedMotion === "no-preference")
    expect(
      servers.every((frame) => frame.some(({ opacity }) => opacity > 0.01))
    ).toBe(true)
    expect(
      servers.every((frame) => frame.filter(({ inert }) => !inert).length <= 1)
    ).toBe(true)
    for (const { x } of servers.flat()) {
      expect(x).toBeGreaterThanOrEqual(8 - 0.1)
      expect(x).toBeLessThanOrEqual(56 + 0.1)
    }
    await expect(
      sidebar.getByRole("button", { name: "已连接", exact: true })
    ).toBeVisible()
    await expect(
      page.getByRole("button", { name: "已连接", exact: true })
    ).toHaveCount(1)

    // 2. 关闭抽屉后入口恢复到顶栏，不留下额外可访问的连接按钮。
    await page.keyboard.press("Escape")
    await expect(sidebar).toHaveCount(0)
    await expect(
      page.getByRole("button", { name: "已连接", exact: true })
    ).toHaveCount(1)
    await expect.poll(getTitleX).toBeCloseTo(positions[0], 0)
  }
})

test("confirmation_and_toast_keep_roles_and_fit_short_viewports", async ({
  page,
  fixture,
}) => {
  // 1. 从真实对话行改成长标题，归档错误经 RPC 返回并保留详情。
  const archiveError = "Archive validation failed for this conversation"
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message)) as {
        id: number
        method: string
      }
      if (request.method === "conversation/setArchived")
        socket.send(
          JSON.stringify({
            id: request.id,
            error: { code: -32602, message: archiveError },
          })
        )
      else server.send(message)
    })
    server.onMessage((message) => socket.send(message))
  })
  await page.setViewportSize({ width: 1200, height: 800 })
  await fixture.openApp(page)
  await startConversation(page, "弹窗排版")
  const conversationID = page.url().split("/c/")[1]
  const row = page
    .getByRole("listitem")
    .filter({ has: page.locator(`a[href*="/c/${conversationID}"]`) })
    .filter({ has: page.getByRole("button", { name: /的更多操作/ }) })
  await row.getByRole("button", { name: /的更多操作/ }).click()
  await page.getByRole("menuitem", { name: "改名", exact: true }).click()
  const rename = page.getByRole("dialog", { name: "改名", exact: true })
  const title = "长对话名称".repeat(24) + "LongConversationName".repeat(5)
  await rename.getByRole("textbox", { name: "标题" }).fill(title)
  await rename.getByRole("button", { name: "保存", exact: true }).click()
  await expect(rename).toBeHidden()
  const confirmation = page.getByRole("alertdialog")

  // 2. 宽屏保留内容宽度；窄短屏标题和取消按钮都能滚动到弹窗内。
  for (const size of [
    { width: 1200, height: 800, dialogWidth: 672 },
    { width: 390, height: 320, dialogWidth: 358 },
  ]) {
    await page.setViewportSize(size)
    if (!(await row.getByRole("button", { name: /的更多操作/ }).isVisible()))
      await page
        .getByRole("button", { name: "切换侧栏" })
        .filter({ visible: true })
        .click()
    await row.getByRole("button", { name: /的更多操作/ }).click()
    await page.getByRole("menuitem", { name: "删除", exact: true }).click()
    await expect(confirmation).toBeVisible()
    await expect
      .poll(async () => (await confirmation.boundingBox())!.width)
      .toBeCloseTo(size.dialogWidth, 0)
    const box = (await confirmation.boundingBox())!
    expect(box.y).toBeGreaterThanOrEqual(15)
    expect(box.y + box.height).toBeLessThanOrEqual(size.height - 15)
    expect(
      await confirmation.evaluate(
        (node) => node.scrollWidth <= node.clientWidth
      )
    ).toBe(true)
    if (size.height === 320)
      expect(
        await confirmation.evaluate(
          (node) => node.scrollHeight > node.clientHeight
        )
      ).toBe(true)
    for (const content of [
      confirmation.getByRole("heading"),
      confirmation.getByRole("button", { name: "取消", exact: true }),
    ]) {
      await content.scrollIntoViewIfNeeded()
      const contentBox = (await content.boundingBox())!
      expect(contentBox.y).toBeGreaterThanOrEqual(box.y - 1)
      expect(contentBox.y).toBeLessThan(box.y + box.height)
    }
    await confirmation
      .getByRole("button", { name: "取消", exact: true })
      .click()
    await expect(confirmation).toBeHidden()
  }

  // 3. 行菜单失败的 toast 按正文、辅助文字角色显示，详情仍可展开。
  await page.setViewportSize({ width: 1200, height: 800 })
  await row.getByRole("button", { name: /的更多操作/ }).click()
  await page.getByRole("menuitem", { name: "归档", exact: true }).click()
  const toast = page
    .locator("[data-sonner-toast]")
    .filter({ hasText: "参数不合法" })
  await expect(toast).toBeVisible()
  for (const { selector, fontSize, lineHeight } of [
    { selector: "[data-title]", fontSize: "14px", lineHeight: "24px" },
    { selector: "[data-description]", fontSize: "12px", lineHeight: "16px" },
  ])
    expect(
      await toast.locator(selector).evaluate((node) => ({
        fontSize: getComputedStyle(node).fontSize,
        lineHeight: getComputedStyle(node).lineHeight,
      }))
    ).toEqual({ fontSize, lineHeight })
  await toast.getByText("详情", { exact: true }).click()
  await expect(toast.getByText(archiveError, { exact: true })).toBeVisible()
})

test(
  "mobile_drawers_open_and_close_composer_at_bottom",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    // 1. 左栏在抽屉里：点开能看到「新对话」，按一次 Esc 关上。
    await fixture.openApp(page)
    const sidebar = page.getByRole("dialog").filter({
      has: page.getByRole("link", { name: "新对话" }),
    })
    await expect(sidebar).toHaveCount(0)
    await page
      .getByRole("button", { name: "切换侧栏" })
      .filter({ visible: true })
      .click()
    await expect(sidebar).toBeVisible()
    await page.keyboard.press("Escape")
    await expect(sidebar).toHaveCount(0)

    // 2. 对话页的输入框贴着视口底部（只隔底部内边距）。
    await startConversation(page, "手机上发送")
    const composer = page.locator("form").filter({ has: getComposer(page) })
    const box = await composer.boundingBox()
    expect(box).not.toBeNull()
    const gap = viewport.height - ((box?.y ?? 0) + (box?.height ?? 0))
    expect(gap).toBeGreaterThanOrEqual(0)
    expect(gap).toBeLessThanOrEqual(16)

    // 3. 对话详情是从底部弹出的抽屉：点开能看到页签，Esc 关上。
    const details = page.getByRole("dialog", { name: "对话详情" })
    await page.getByRole("button", { name: "切换对话详情" }).click()
    await expect(details).toBeVisible()
    await expect(
      details.getByRole("tab", { name: "Agent", exact: true })
    ).toBeVisible()
    await page.keyboard.press("Escape")
    await expect(details).toHaveCount(0)
  }
)

test(
  "model_drawer_is_direct_and_touch_enter_is_newline",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    await page.emulateMedia({ colorScheme: "dark" })
    await fixture.openApp(page)
    await page.getByRole("button", { name: "模型", exact: true }).click()
    const drawer = page.getByRole("dialog", { name: "模型", exact: true })
    await expect(drawer).toBeVisible()
    await drawer.getByRole("searchbox", { name: "搜索模型" }).fill("local")
    await expect(drawer.getByRole("button", { name: /default/ })).toBeVisible()
    await expect(drawer.getByRole("combobox")).toHaveCount(0)
    await drawer.getByRole("button", { name: /default/ }).click()
    await expect(
      drawer.getByRole("button", { name: /default/ })
    ).toHaveAttribute("aria-pressed", "true")
    await drawer.getByRole("button", { name: "关闭模型选择" }).click()
    await expect(drawer).toHaveCount(0)
    // 触屏输入不触发发送，点击外露发送按钮仍可发送。
    await page.evaluate(() => {
      const original = window.matchMedia.bind(window)
      window.matchMedia = (query) =>
        query === "(pointer: coarse)"
          ? ({ ...original(query), matches: true } as MediaQueryList)
          : original(query)
    })
    await getComposer(page).fill("触屏换行")
    await getComposer(page).press("Enter")
    await expect(getComposer(page)).toHaveValue("触屏换行\n")
    await expect(page).not.toHaveURL(/\/c\/[^/]+$/)
    await page.getByRole("button", { name: "发送", exact: true }).click()
    await expect(page).toHaveURL(/\/c\/[^/]+$/)
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
    // 等这一轮取完回复再结束，下一条用例排的回复不会被它用掉。
    await expect(
      getMessages(page).getByText("ok", { exact: true })
    ).toBeVisible()
  }
)

test(
  "sub_session_keeps_model_context_controls_without_input_or_compaction",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    await fixture.replies({
      tool: "spawn_agent",
      arguments: JSON.stringify({ task_name: "底栏助手", message: "核对底栏" }),
    })
    await fixture.openApp(page)
    await startConversation(page, "检查子会话")
    await page.getByRole("button", { name: "切换对话详情" }).click()
    await page.getByRole("tab", { name: "Agent", exact: true }).click()
    const helper = page
      .getByRole("tabpanel", { name: "Agent", exact: true })
      .getByRole("link", { name: /底栏助手/ })
    await expect(helper).toContainText("空闲")
    await helper.click()
    await expect(page).toHaveURL(/\/sessions\/[^/]+$/)
    await expect(getComposer(page)).toHaveCount(0)
    // 子 Session 跳转后关闭仍打开的详情，底栏控件实际可点击。
    const details = page.getByRole("dialog", { name: "对话详情" })
    if (await details.isVisible()) await page.keyboard.press("Escape")
    await expect(
      page.getByRole("button", { name: "模型", exact: true })
    ).toBeVisible()
    await page.getByRole("button", { name: /^上下文占用：/ }).click()
    await expect(
      page.getByRole("button", { name: "压缩上下文", exact: true })
    ).toHaveCount(0)
    await expect(
      page.getByRole("button", { name: "发送", exact: true })
    ).toHaveCount(0)
  }
)

test("details_layout_preserves_messages", async ({ page, fixture }) => {
  await fixture.openApp(page)
  await startConversation(page, "详情布局")
  const messageElement = await page
    .getByRole("region", { name: "消息", exact: true })
    .elementHandle()
  await page.getByRole("button", { name: "切换对话详情" }).click()
  const dialog = page.getByRole("dialog", { name: "对话详情", exact: true })
  await dialog.getByRole("tab", { name: "上下文", exact: true }).click()
  const context = page.getByRole("tabpanel", { name: "上下文", exact: true })
  await expect(context.getByText("预计占用", { exact: true })).toBeVisible()
  expect(await context.evaluate((node) => getComputedStyle(node).padding)).toBe(
    "12px"
  )
  const tabs = dialog.getByRole("tablist")
  expect(
    await tabs.evaluate(
      (node) => getComputedStyle(node.parentElement!.parentElement!).padding
    )
  ).toBe("12px")
  const tabsBox = (await tabs.boundingBox())!
  const closeBox = (await dialog
    .getByRole("button", { name: "关闭对话详情" })
    .boundingBox())!
  expect(Math.abs(tabsBox.y - closeBox.y)).toBeLessThan(5)
  expect(tabsBox.x + tabsBox.width).toBeLessThanOrEqual(closeBox.x)
  await dialog.getByRole("button", { name: "关闭对话详情" }).click()
  await expect(dialog).toHaveCount(0)
  for (const width of [900, 1440, 390]) {
    await page.setViewportSize({ width, height: viewport.height })
    expect(await messageElement!.evaluate((node) => node.isConnected)).toBe(
      true
    )
    if (width !== 1440) {
      await page.getByRole("button", { name: "切换对话详情" }).click()
      await expect(dialog).toBeVisible()
      expect(
        await dialog.evaluate((node) => node.getBoundingClientRect().width)
      ).toBe(width)
      await expect
        .poll(() =>
          dialog.evaluate((node) => node.getBoundingClientRect().bottom)
        )
        .toBeCloseTo(viewport.height, 0)
    }
    await expect(
      page.getByRole("tab", { name: "上下文", selected: true })
    ).toBeVisible()
    if (width !== 1440) await page.keyboard.press("Escape")
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth
      )
    ).toBe(true)
  }
})

test("context_refresh_preserves_values_and_loads_details_on_demand", async ({
  page,
  fixture,
}) => {
  let failUsage = false
  let detailsCalls = 0
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message)) as {
        id: number
        method: string
      }
      if (request.method === "session/details") detailsCalls++
      if (failUsage && request.method === "session/contextUsage")
        socket.send(
          JSON.stringify({
            id: request.id,
            error: { code: -32000, message: "Context refresh unavailable" },
          })
        )
      else server.send(message)
    })
    server.onMessage((message) => socket.send(message))
  })
  await fixture.openApp(page)
  await startConversation(page, "上下文刷新")
  await page.getByRole("button", { name: "切换对话详情" }).click()
  await page.getByRole("tab", { name: "上下文", exact: true }).click()
  const context = page.getByRole("tabpanel", { name: "上下文", exact: true })
  await expect(context.locator("dd").first()).toContainText("≈")
  const estimate = await context.locator("dd").first().innerText()
  expect(detailsCalls).toBe(0)
  for (const name of ["Session 详情", "原始数据"])
    await expect(
      context.getByRole("button", { name, exact: true })
    ).toHaveAttribute("aria-expanded", "false")
  failUsage = true
  await context.getByRole("button", { name: "刷新", exact: true }).click()
  await expect(context.getByRole("alert")).toBeVisible({ timeout: 15000 })
  await expect(context.locator("dd").first()).toHaveText(estimate)
  failUsage = false
  await context.getByRole("button", { name: "重试", exact: true }).click()
  await expect(context.getByRole("alert")).toHaveCount(0)
  await context.getByRole("button", { name: "Session 详情" }).click()
  await expect.poll(() => detailsCalls).toBe(1)
  await expect(context.getByText("压缩模型", { exact: true })).toBeVisible()
})

test("rpc_virtual_list_compensates_eviction_and_limits_details", async ({
  page,
  fixture,
}) => {
  // 诊断帧经 WebSocket 接收入口进入日志，不修改浏览器 store 或会话状态。
  let sendFrame: ((message: string) => void) | undefined
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    sendFrame = (message) => socket.send(message)
    socket.onMessage((message) => server.send(message))
    server.onMessage((message) => socket.send(message))
  })
  await fixture.openApp(page)
  await startConversation(page, "RPC阅读位置")
  await page.getByRole("button", { name: "切换对话详情" }).click()
  await page.getByRole("tab", { name: "RPC", exact: true }).click()
  const panel = page.getByRole("region", { name: "RPC 记录", exact: true })
  await panel.getByRole("switch", { name: "只看当前对话" }).uncheck()
  await panel.getByRole("button", { name: "清空记录", exact: true }).click()
  const appendFrames = (from: number, count: number) => {
    for (let index = from; index < from + count; index++)
      sendFrame!(
        JSON.stringify({ method: `task10/frame/${index}`, params: {} })
      )
  }
  appendFrames(0, 2000)
  await expect(
    panel.getByText("显示 2000 / 共 2000 帧", { exact: true })
  ).toBeVisible()
  const scroller = panel.locator(".overflow-auto").first()
  await expect
    .poll(() =>
      scroller.evaluate(
        (node) => node.scrollHeight - node.clientHeight - node.scrollTop
      )
    )
    .toBeLessThan(25)
  await scroller.evaluate((node) => {
    node.scrollTop = 24000
  })
  const target = panel
    .getByRole("button")
    .filter({ hasText: "task10/frame/1000" })
  await expect(target).toBeVisible()
  const before = (await target.boundingBox())!.y
  appendFrames(2000, 50)
  await expect
    .poll(() => scroller.evaluate((node) => node.scrollTop))
    .toBe(22800)
  await expect
    .poll(async () => (await target.boundingBox())!.y)
    .toBeCloseTo(before, 0)
  expect(await scroller.locator("button").count()).toBeLessThan(100)
  await target.click()
  await expect(panel.locator("[data-output-block]")).toBeVisible()
  await expect(
    panel.getByRole("button", { name: "展开输出", exact: true })
  ).toHaveCount(0)
  expect(
    (await panel.locator("[data-output-block]").boundingBox())!.height
  ).toBeLessThan(viewport.height * 0.4)
  expect((await scroller.boundingBox())!.height).toBeGreaterThan(100)
})

test("turns_load_older_pages_on_scroll_with_next_cursor", async ({
  page,
  fixture,
}) => {
  const { longConversationID } = await fixture.seed({
    conversations: 0,
    longConversationTurns: 55,
  })
  const requests: { cursor?: unknown }[] = []
  page.on("websocket", (socket) =>
    socket.on("framesent", (frame) => {
      const message = JSON.parse(String(frame.payload)) as {
        method?: string
        params?: { cursor?: unknown }
      }
      if (message.method === "session/turns/list")
        requests.push(message.params ?? {})
    })
  )
  await fixture.openApp(page)
  await page.goto(page.url() + "/c/" + longConversationID)
  await page.getByRole("button", { name: "切换对话详情" }).click()
  await page.getByRole("tab", { name: "Turn", exact: true }).click()
  const turns = page.getByRole("tabpanel", { name: "Turn", exact: true })
  await expect(turns.locator("li").first()).toBeVisible()
  await turns.evaluate((node) => {
    node.scrollTop = node.scrollHeight
  })
  await expect
    .poll(() => requests.some((request) => !!request.cursor))
    .toBe(true)
  await expect(turns.locator("li")).toHaveCount(55)
  await turns
    .locator("li")
    .first()
    .getByRole("button", { name: "已完成", exact: true })
    .click()
  await expect(
    turns.locator("li").first().locator("[data-output-block]")
  ).toBeVisible()
})

test("directory_retry_keeps_popup_focus_and_escape_preserves_settings", async ({
  page,
  fixture,
}) => {
  // 1. 延后真实目录 RPC 的返回，在浏览器里观察重试中的候选浮层。
  const directoryRequests = new Set<number>()
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message)) as {
        id: number
        method: string
      }
      if (request.method === "executor/listDirectories")
        directoryRequests.add(request.id)
      server.send(message)
    })
    server.onMessage((message) => {
      const response = JSON.parse(String(message)) as { id?: number }
      if (response.id !== undefined && directoryRequests.delete(response.id))
        setTimeout(() => socket.send(message), 250)
      else socket.send(message)
    })
  })
  await fixture.openApp(page)
  await startConversation(page, "目录重试焦点")
  await page.getByRole("button", { name: "对话设置", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "对话设置", exact: true })
  const directory = dialog.getByRole("combobox", {
    name: "工作目录",
    exact: true,
  })
  const draft = "/definitely-not-a-real-openvaala-directory/"
  await directory.fill(draft)
  await expect(
    page.getByRole("button", { name: "重试", exact: true })
  ).toBeVisible()

  // 2. 点击重试后浮层保持加载；Esc 只关闭候选，父设置和目录草稿保持原样。
  await page.getByRole("button", { name: "重试", exact: true }).click()
  await expect(directory).toBeFocused()
  await expect(directory).toHaveAttribute("aria-expanded", "true")
  await expect(page.getByText("正在加载…", { exact: true })).toBeVisible()
  await page.keyboard.press("Escape")
  await expect(page.getByRole("listbox")).toHaveCount(0)
  await expect(dialog).toBeVisible()
  await expect(directory).toHaveValue(draft)
  await directory.click()
  await expect(
    page.getByRole("button", { name: "重试", exact: true })
  ).toBeVisible()
  await page.keyboard.press("Escape")
  await expect(dialog).toBeVisible()
  await expect(directory).toHaveValue(draft)
})

test("executor_directory_candidates_are_lazy_and_keep_manual_input", async ({
  page,
  fixture,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const requests: {
    executor_id: string
    directory: string
    base_directory?: string
  }[] = []
  page.on("websocket", (socket) =>
    socket.on("framesent", (frame) => {
      const message = JSON.parse(String(frame.payload)) as {
        method?: string
        params: (typeof requests)[number]
      }
      if (message.method === "executor/listDirectories")
        requests.push(message.params)
    })
  )
  await fixture.openApp(page)
  expect(requests).toHaveLength(0)
  const root = fixture.state.workspaceDirectory
  const directory = page.getByRole("combobox", {
    name: "工作目录",
    exact: true,
  })
  await directory.fill(root + "/p")
  await expect(
    page.getByRole("option", { name: "projects/", exact: true })
  ).toBeVisible()
  await expect(
    page.getByRole("option", { name: "project-link/", exact: true })
  ).toHaveCount(0)
  await directory.fill(root + "/pro")
  await expect(
    page.getByRole("option", { name: "projects/", exact: true })
  ).toBeVisible()
  expect(
    requests.filter((request) => request.directory === root + "/")
  ).toHaveLength(1)
  await page.getByRole("option", { name: "projects/", exact: true }).click()
  await expect(directory).toHaveValue(root + "/projects/")
  await expect(
    page.getByRole("option", { name: "project-one/", exact: true })
  ).toBeVisible()
  await page.keyboard.press("Escape")
  await expect(page.getByRole("listbox")).toHaveCount(0)
  await expect(directory).toHaveValue(root + "/projects/")

  // 搜索弹层不绑定短触发器宽度，长说明和目录分别显示。
  const executor = page.getByRole("combobox", {
    name: "Executor",
    exact: true,
  })
  await executor.click()
  const search = page.getByRole("combobox", {
    name: "搜索 Executor",
    exact: true,
  })
  await search.fill("laboratory-01")
  const option = page.getByRole("option", { name: /laboratory-01/ })
  await expect(option).toBeVisible()
  expect((await option.boundingBox())!.width).toBeGreaterThan(
    (await executor.boundingBox())!.width
  )
  await page.keyboard.press("Escape")
  await expect(page.getByRole("listbox")).toHaveCount(0)

  // 候选失败只停在浮层；关闭候选后手填符号链接，创建仍经过真实目录校验。
  await directory.fill(root + "/missing/")
  await expect(page.getByRole("alert")).toBeVisible()
  expect(
    requests.filter((request) => request.directory === root + "/missing/")
  ).toHaveLength(1)
  await page.keyboard.press("Escape")
  await expect(page.getByRole("listbox")).toHaveCount(0)
  await directory.click()
  await expect(page.getByRole("alert")).toBeVisible()
  expect(
    requests.filter((request) => request.directory === root + "/missing/")
  ).toHaveLength(1)
  await page.getByRole("button", { name: "重试", exact: true }).click()
  await expect
    .poll(
      () =>
        requests.filter((request) => request.directory === root + "/missing/")
          .length
    )
    .toBe(2)
  await directory.fill(root + "/project-link")
  await page.keyboard.press("Escape")
  await expect(directory).toHaveValue(root + "/project-link")
  await startConversation(page, "手填工作目录")
  const conversationID = page.url().split("/").at(-1)!
  const saved = await fixture.executorSettings(conversationID)
  expect(saved.workspace_directories?.local).toBe(root + "/project-link")
})

test("executor_settings_preserve_tab_drafts_and_save_only_default", async ({
  page,
  fixture,
}) => {
  await fixture.openApp(page)
  await startConversation(page, "Executor 保存范围")
  const conversationID = page.url().split("/").at(-1)!
  const original = await fixture.executorSettings(conversationID)
  await page.getByRole("button", { name: "对话设置", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "对话设置", exact: true })
  const directory = dialog.getByRole("combobox", {
    name: "工作目录",
    exact: true,
  })
  const save = dialog.getByRole("button", { name: "保存", exact: true })
  // 长名称和路径不能撑宽中间字段区，说明实际显示不超过两行。
  const fields = dialog.locator("form > div").first()
  expect(
    await fields.evaluate((node) => node.scrollWidth <= node.clientWidth)
  ).toBe(true)
  const longDescription = dialog
    .getByText(
      "用于核对多执行器长表单的本地工作区。这段说明包含足够长的自然语言，应该自然换行并最多展示两行，工作目录另起一行，不能替代说明。",
      { exact: true }
    )
    .first()
  expect(
    await longDescription.evaluate(
      (node) =>
        node.getBoundingClientRect().height <=
        parseFloat(getComputedStyle(node).lineHeight) * 2
    )
  ).toBe(true)
  const localDirectory = dialog
    .getByText(fixture.state.workspaceDirectory, { exact: true })
    .first()
  expect(
    await localDirectory.evaluate((node) => ({
      overflow: getComputedStyle(node).overflowX,
      ellipsis: getComputedStyle(node).textOverflow,
      whitespace: getComputedStyle(node).whiteSpace,
    }))
  ).toEqual({
    overflow: "hidden",
    ellipsis: "ellipsis",
    whitespace: "nowrap",
  })
  await directory.fill(fixture.state.workspaceDirectory + "/manual")
  await page.keyboard.press("Escape")
  await dialog.getByRole("tab", { name: "MCP", exact: true }).click()
  await expect(save).toHaveCount(0)
  await dialog.getByRole("tab", { name: "Skill", exact: true }).click()
  await expect(save).toHaveCount(0)
  await dialog.getByRole("tab", { name: "Executor", exact: true }).click()
  await expect(directory).toHaveValue(
    fixture.state.workspaceDirectory + "/manual"
  )
  await dialog.getByRole("button", { name: "Executor", exact: true }).click()
  const drawer = page.getByRole("dialog", { name: "Executor", exact: true })
  await expect(drawer.getByRole("button", { name: /^local / })).toHaveAttribute(
    "aria-pressed",
    "true"
  )
  await drawer.getByRole("searchbox", { name: "搜索 Executor" }).fill("sandbox")
  await drawer.getByRole("button", { name: /^sandbox / }).click()
  await expect(drawer).toHaveCount(0)
  await expect(directory).toHaveValue("")
  await directory.fill(fixture.state.sandboxDirectory + "/missing/")
  await page.keyboard.press("Escape")
  await save.click()
  await expect(dialog.getByText(/工作目录不可用/)).toBeVisible()
  await expect(directory).toHaveValue(
    fixture.state.sandboxDirectory + "/missing/"
  )
  await directory.fill(fixture.state.sandboxDirectory + "/saved")
  await page.keyboard.press("Escape")
  const saveBefore = (await save.boundingBox())!
  const tabsBefore = (await dialog.getByRole("tablist").boundingBox())!
  await dialog.getByRole("checkbox").last().scrollIntoViewIfNeeded()
  expect((await save.boundingBox())!.y).toBeCloseTo(saveBefore.y, 0)
  expect((await dialog.getByRole("tablist").boundingBox())!.y).toBeCloseTo(
    tabsBefore.y,
    0
  )
  expect(saveBefore.y + saveBefore.height).toBeLessThan(viewport.height)
  await save.click()
  await expect(
    page.getByText("Executor 设置已保存", { exact: true })
  ).toBeVisible()
  await expect(
    dialog.getByRole("tab", { name: "Executor", selected: true })
  ).toBeVisible()
  const saved = await fixture.executorSettings(conversationID)
  expect(saved.default_executor_id).toBe("sandbox")
  expect(saved.workspace_directories?.sandbox).toBe(
    fixture.state.sandboxDirectory + "/saved"
  )
  expect(saved.workspace_directories?.local).toBe(
    original.workspace_directories?.local
  )
  const defaultRow = dialog.locator("label").filter({ hasText: /^sandbox/ })
  await expect(defaultRow.getByRole("checkbox")).toBeChecked()
  await expect(defaultRow.getByRole("checkbox")).toBeDisabled()
  expect(
    await fields.evaluate((node) => node.scrollWidth <= node.clientWidth)
  ).toBe(true)

  // 留空只删默认项，恢复该 Executor 默认根目录。
  await directory.fill("")
  await save.click()
  await expect
    .poll(
      async () =>
        (await fixture.executorSettings(conversationID)).workspace_directories
          ?.sandbox
    )
    .toBeUndefined()
  expect(
    (await fixture.executorSettings(conversationID)).workspace_directories
      ?.local
  ).toBe(original.workspace_directories?.local)
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth
    )
  ).toBe(true)
})
