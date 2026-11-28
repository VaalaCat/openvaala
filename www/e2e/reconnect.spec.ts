import { hostname } from "node:os"
import {
  captureExternalUI,
  chooseExternalModel,
  expect,
  getComposer,
  getMessages,
  startConversation,
  test,
} from "./fixture"
import type { WebSocketRoute } from "@playwright/test"
import type { SessionContextUsage } from "../src/lib/api/protocol.gen"

test("manual_reconnect_preserves_content_and_draft", async ({
  page,
  fixture,
}) => {
  // 1. 代理真实 socket，仅在断开后暂停握手；绿色图标悬停仍是 Server。
  let mode: "allow" | "hold" = "allow"
  let liveSocket: WebSocketRoute | undefined
  let heldSocket: WebSocketRoute | undefined
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    if (mode === "hold") {
      heldSocket = socket
      return
    }
    liveSocket = socket
    socket.connectToServer()
  })
  await fixture.openApp(page)
  await startConversation(page, "连接入口检查")
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  const control = page.getByRole("button", {
    name: / · (?:已连接|正在连接|已断开)/,
  })
  await expect(control).toHaveAccessibleName(`${hostname()} · 已连接`)
  await expect(control).toHaveCSS("height", "32px")
  const serverPaths = await control.locator("svg").innerHTML()
  await control.hover()
  expect(await control.locator("svg").innerHTML()).toBe(serverPaths)
  const draft = "断线保留草稿"
  await getComposer(page).fill(draft)

  // 2. 断线只集中到服务器入口，消息／草稿继续可读写，不伪造空列表。
  mode = "hold"
  await liveSocket!.close()
  await expect(control).toHaveAccessibleName(`${hostname()} · 已断开，正在重连`)
  await expect(
    page.getByRole("img", { name: "已断开，正在重连" })
  ).toBeVisible()
  await expect(getComposer(page)).toHaveValue(draft)
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  await expect(page.getByRole("alert")).toHaveCount(0)
  await expect(
    page.getByRole("button", { name: "发送", exact: true })
  ).toBeDisabled()
  await control.click()
  const selectedServer = page.getByRole("menuitemradio", { checked: true })
  const address = new URL(fixture.state.url).host
  await expect(selectedServer).toHaveAccessibleName(
    `${hostname()} ${address} · 已断开`
  )
  await expect(
    page.getByRole("menuitem", { name: "重连", exact: true })
  ).toHaveCount(0)
  const manage = page.getByRole("menuitem", { name: "管理服务器…" })
  const [nameX, manageX] = await selectedServer
    .or(manage)
    .evaluateAll((items) =>
      items.map((item) => {
        const range = document.createRange()
        range.selectNodeContents(
          document.createTreeWalker(item, NodeFilter.SHOW_TEXT).nextNode()!
        )
        return range.getBoundingClientRect().x
      })
    )
  expect(nameX).toBe(manageX)
  await selectedServer.focus()
  await selectedServer.press("Enter")
  await expect(selectedServer).toBeDisabled()
  await expect(control).toHaveAttribute("aria-busy", "true")
  await page.mouse.move(1276, 80)
  await expect(control).toHaveAttribute("aria-busy", "true")
  const rotation = control.locator(".server-reconnect-icon")
  await expect(rotation).toHaveCSS("animation-name", "spin")
  const dot = control.getByRole("img")
  await expect(dot).toHaveCSS("transform", "none")
  await page.emulateMedia({ reducedMotion: "reduce" })
  await expect(rotation).toHaveCSS("animation-name", "spin")
  await expect.poll(() => !!heldSocket).toBe(true)

  // 3. 本次失败即停止旋转，下一次点击跳过退避并恢复原数据。
  await heldSocket!.close()
  await expect(selectedServer).toBeEnabled()
  await manage.click()
  const manager = page.getByRole("dialog", { name: "管理服务器" })
  const offlineRow = manager.getByRole("button", { name: /已断开/ })
  await expect(offlineRow).toContainText(address)
  await expect(offlineRow).toHaveAccessibleDescription("连接")
  mode = "allow"
  await offlineRow.click()
  await expect(manager).toBeHidden()
  await expect(control).toHaveAccessibleName(`${hostname()} · 已连接`)
  await expect(control).toHaveAttribute("aria-busy", "false")
  await expect(getComposer(page)).toHaveValue(draft)
  await expect(
    page.getByRole("button", { name: "发送", exact: true })
  ).toBeEnabled()
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()

  // 4. 窄屏下连接入口仍在顶栏，高度不变。
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(control).toBeVisible()
  await expect(control).toHaveCSS("height", "32px")
})

test("context_usage_restores_before_first_new_response_after_restart", async ({
  page,
  fixture,
}) => {
  const usageResponses: SessionContextUsage[] = []
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    const usageRequestIDs = new Set<number>()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message)) as {
        id: number
        method?: string
      }
      if (request.method === "session/contextUsage")
        usageRequestIDs.add(request.id)
      server.send(message)
    })
    server.onMessage((message) => {
      const response = JSON.parse(String(message)) as {
        id?: number
        result?: SessionContextUsage
      }
      if (
        response.id !== undefined &&
        usageRequestIDs.delete(response.id) &&
        response.result
      )
        usageResponses.push(response.result)
      socket.send(message)
    })
  })
  // 1. 完成真实请求后，底栏展示占用环，浮层保留估算标记。
  await fixture.openApp(page)
  await startConversation(page, "上下文测量重启检查")
  const messages = getMessages(page)
  const firstTurn = messages.locator('[data-turn-status="completed"]').first()
  await expect(firstTurn).toContainText("↑120")
  await expect(firstTurn).toContainText("↓4")
  const context = page.getByRole("button", { name: /^上下文占用：/ })
  const popover = page.locator('[data-slot="popover-content"]:visible')
  await expect(context).not.toHaveAccessibleName(/还没有测量/)
  await expect(context.getByRole("img")).toBeVisible()
  await context.click()
  await expect(popover).toContainText("≈")
  await expect(
    popover.getByRole("button", { name: "压缩上下文" })
  ).toBeEnabled()
  await page.keyboard.press("Escape")

  // 2. 重启清空测量后，入口只画空环，浮层显示未知。
  await fixture.restart()
  await expect(context).toHaveAccessibleName("上下文占用：还没有测量", {
    timeout: 20_000,
  })
  await expect(context).not.toContainText(/还没有测量|0%|≈0/)
  await expect(context.getByRole("img", { name: "还没有测量" })).toBeVisible()
  await expect(context).toHaveCSS("height", "32px")
  await expect(context).toHaveCSS("font-size", "14px")
  await expect(context).toHaveCSS("line-height", "20px")
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 844 })
    const bounds = (await context.boundingBox())!
    expect(bounds.width).toBeLessThanOrEqual(48)
    expect(bounds.x).toBeGreaterThanOrEqual(0)
    expect(bounds.x + bounds.width).toBeLessThanOrEqual(width)
  }
  await context.focus()
  await context.press("Enter")
  await expect(popover).toContainText("还没有测量")
  await expect(popover).not.toContainText("≈")
  await page.screenshot({
    path: test.info().outputPath("unknown-context.png"),
  })
  await page.keyboard.press("Escape")
  await expect(context).toBeFocused()

  // 3. 新响应挂起且没有新 Usage 时，首次测量已恢复上次实测依据。
  await fixture.replies({ text: "新响应保持挂起", hold: true })
  await getComposer(page).fill("再次测量")
  await getComposer(page).press("Enter")
  await expect(
    messages.getByText("新响应保持挂起", { exact: true })
  ).toBeVisible()
  await expect(context).not.toHaveAccessibleName(/还没有测量/)
  await expect(context.getByRole("img")).toBeVisible()
  await context.click()
  await expect(popover).toContainText("≈")
  await expect(popover).not.toContainText("还没有测量")
  await expect(
    popover.getByRole("button", { name: "压缩上下文" })
  ).toBeDisabled()
  await page.keyboard.press("Escape")
  await expect(firstTurn).toContainText("↑120")
  await expect(firstTurn).toContainText("↓4")

  // 4. 真实上下文查询返回恢复后的 RequestTokens，最近调用详情仍为空。
  await page.getByRole("button", { name: "切换对话详情" }).click()
  await page.getByRole("tab", { name: "上下文", exact: true }).click()
  const contextTab = page.getByRole("tabpanel", {
    name: "上下文",
    exact: true,
  })
  await expect(contextTab.locator("dd").first()).toContainText("≈")
  await expect(contextTab).toContainText("还没有上报过用量的请求")
  await expect.poll(() => usageResponses.length).toBeGreaterThan(0)
  const usage = usageResponses.at(-1)!
  expect(usage.next_request.source).toBe("actual")
  expect(usage.last_request).toBeUndefined()
  expect(usage.next_request.tokens).toBeGreaterThanOrEqual(124)
  expect(usage.next_request.tokens).toBeLessThan(
    usage.system_tokens + usage.tool_tokens
  )

  // 5. 明暗主题和桌面、390px下保留估算说明、文字角色和可关闭的详情。
  for (const colorScheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme })
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 844 })
      // 等布局切换完成：只有桌面布局有右栏分隔条。
      const panelSeparator = page.getByRole("separator")
      if (width === 1280) await expect(panelSeparator).toBeVisible()
      else await expect(panelSeparator).toHaveCount(0)
      if (!(await contextTab.isVisible()))
        await page.getByRole("button", { name: "切换对话详情" }).click()
      const estimate = contextTab.locator("dd").first()
      await expect(estimate).toHaveCSS("font-size", "14px")
      await expect(estimate).toHaveCSS("line-height", "20px")
      await estimate.locator('[data-slot="tooltip-trigger"]').first().hover()
      await expect(
        page.getByText("基于上次实际用量，结合内容变化估算", { exact: true })
      ).toBeVisible()
      await page.mouse.move(0, 0)
      await page.screenshot({
        path: test
          .info()
          .outputPath(`restored-context-${colorScheme}-${width}.png`),
      })
      await page.getByRole("button", { name: "关闭对话详情" }).click()
    }
  }
  await page.getByRole("button", { name: "中断", exact: true }).click()
})

test(
  "reconnect_after_serve_restart_fills_gap",
  { tag: "@p1" },
  async ({ page, browser, fixture }) => {
    // 1. 页面 A 的 WebSocket 经 Playwright 转发：挡住时新连接一律关掉，模拟 A 迟迟连不回去。
    let isReconnectBlocked = false
    // 时钟照常走，只在放开重连后快进，跳过剩余的退避等待。
    await page.clock.install()
    await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
      if (isReconnectBlocked) {
        void socket.close()
        return
      }
      socket.connectToServer()
    })
    await fixture.openApp(page)
    await startConversation(page, "重启之前")
    const messages = getMessages(page)
    await expect(messages.getByText("ok", { exact: true })).toBeVisible()
    const conversationURL = page.url()

    // 2. 挡住 A 的重连，重启 serve：A 显示重连中。
    isReconnectBlocked = true
    await fixture.restart()
    await expect(
      page.getByRole("img", { name: "已断开，正在重连" })
    ).toBeVisible()

    // 3. A 断开期间，页面 B 新建一个对话，再往 A 的对话发一条并收到回复。
    const otherContext = await browser.newContext({ locale: "zh-CN" })
    const other = await otherContext.newPage()
    await fixture.openApp(other)
    await startConversation(other, "断开期间新建")
    await expect(
      getMessages(other).getByText("ok", { exact: true })
    ).toBeVisible()
    await fixture.replies({ text: "断开期间的回复" })
    await other.goto(conversationURL)
    await getComposer(other).fill("断开期间发的")
    await getComposer(other).press("Enter")
    await expect(getMessages(other).getByText("断开期间的回复")).toBeVisible()
    await otherContext.close()

    // 4. 放开 A：退避到期后自动重连成功，补齐断开期间的消息，左栏列出 B 新建的对话。
    isReconnectBlocked = false
    await expect(async () => {
      await page.clock.fastForward(10_000)
      await expect(page.getByRole("img", { name: "已连接" })).toBeVisible({
        timeout: 2_000,
      })
    }).toPass({ timeout: 20_000 })
    await expect(
      messages.getByText("断开期间发的", { exact: true })
    ).toBeVisible()
    await expect(messages.getByText("断开期间的回复")).toBeVisible()
    await expect(page.getByRole("link", { name: "断开期间新建" })).toBeVisible()
  }
)

test("external_restoring_then_failed_and_reloaded", async ({
  page,
  fixture,
}) => {
  const { writeFile, rm } = await import("node:fs/promises")
  const gate = fixture.state.workspaceDirectory + "/load-gate"
  const fail = fixture.state.workspaceDirectory + "/fail-load"
  await fixture.withExternalAgent(
    ["-load-gate", gate, "-fail-load-file", fail],
    async () => {
      await fixture.openApp(page)
      await chooseExternalModel(page)
      await startConversation(page, "恢复历史")
      await expect(
        getMessages(page).getByText("OK", { exact: true })
      ).toBeVisible()
      await writeFile(fail, "fail")
      await fixture.restart()
      await expect(page.getByText("正在从外部 Agent 恢复历史")).toBeVisible()
      await getComposer(page).fill("等待恢复")
      await expect(
        page.getByRole("button", { name: "发送", exact: true })
      ).toBeEnabled()
      await captureExternalUI(page, "restoring")
      await writeFile(gate, "ready")
      await expect(page.getByText("恢复失败", { exact: true })).toBeVisible()
      await expect(
        page.getByText(/fixture partial replay/).first()
      ).toBeVisible()
      const composer = getComposer(page)
      await expect(composer).toHaveValue("等待恢复")
      await composer.fill("")
      await expect(composer).toHaveAttribute(
        "placeholder",
        "输入消息，/ 选择命令"
      )
      const reload = page
        .getByRole("alert")
        .getByRole("button", { name: "重载 Agent" })
      await expect(reload).toBeVisible()
      await captureExternalUI(page, "restore-failed")
      await rm(fail)
      await reload.click()
      await expect(
        getMessages(page).getByText("latest", { exact: true })
      ).toBeVisible()
    }
  )
})

for (const asset of ["index", "world-admin"]) {
  test(`cold_start_recovers_when_the_${asset}_module_loses_network`, async ({
    page,
    context,
    fixture,
  }) => {
    let documents = 0
    page.on("request", (request) => {
      if (request.resourceType() === "document") documents++
    })
    // 首次恢复探测也等到离线失败，此时仍须停在原文档。
    const offlineProbe = page.waitForEvent(
      "requestfailed",
      (request) =>
        request.resourceType() === "fetch" &&
        new URL(request.url()).pathname === new URL(page.url()).pathname
    )
    await page.route(
      new RegExp(`/assets/${asset}-[\\w-]+\\.js$`),
      async (route) => {
        await context.setOffline(true)
        await route.abort("internetdisconnected")
      },
      { times: 1 }
    )
    await page.goto(fixture.state.url + "/web/v1/world/admin", {
      waitUntil: "commit",
    })
    await offlineProbe
    expect(documents).toBe(1)
    await context.setOffline(false)
    await expect(page.getByLabel("管理令牌")).toBeVisible()
    expect(documents).toBe(2)
  })
}

test("loaded_page_waits_for_click_to_refresh_after_chunk_404", async ({
  page,
  fixture,
}) => {
  let documents = 0
  page.on("request", (request) => {
    if (request.resourceType() === "document") documents++
  })
  const { longConversationID } = await fixture.seed({
    conversations: 0,
    longConversationTurns: 1,
  })
  await page.clock.install()
  await fixture.openApp(page)
  await expect(getComposer(page)).toBeVisible()
  const refresh = page.getByRole("button", { name: "页面已更新，点击刷新" })
  for (const [link, chunk] of [
    [
      page
        .locator(`[data-conversation-id="${longConversationID}"]`)
        .getByRole("link"),
      "workspace-page",
    ],
    [page.getByRole("link", { name: "设置", exact: true }), "general"],
  ] as const) {
    const url = new RegExp(`/assets/${chunk}-[\\w-]+\\.js$`)
    await page.route(url, (route) => route.fulfill({ status: 404 }))
    const missing = page.waitForResponse(url)
    await link.click()
    expect((await missing).status()).toBe(404)
    await expect(refresh).toBeVisible()
    await page.unroute(url)
  }
  await page.clock.fastForward(10_000)
  await expect(
    page.getByRole("link", { name: "设置", exact: true })
  ).toBeVisible()
  expect(documents).toBe(1)
  await refresh.click()
  await expect(
    page.getByRole("tab", { name: "通用", selected: true })
  ).toBeVisible()
  await expect(refresh).toHaveCount(0)
  expect(documents).toBe(2)
})

for (const interrupted of ["conversation/list", "session/items/list"]) {
  test(`reconnect_after_interrupting_${interrupted}`, async ({
    page,
    fixture,
  }) => {
    const { longConversationID } = await fixture.seed({
      conversations: 0,
      longConversationTurns: 1,
    })
    let dropped = false
    const subscriptions: string[][] = []
    let documents = 0
    page.on("request", (request) => {
      if (request.resourceType() === "document") documents++
    })
    await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
      const server = socket.connectToServer()
      const methods: string[] = []
      subscriptions.push(methods)
      socket.onMessage(async (frame) => {
        const { method } = JSON.parse(String(frame))
        if (method === interrupted && !dropped) {
          dropped = true
          await socket.close()
          await server.close()
          return
        }
        if (
          method === "conversation/subscribe" ||
          method === "session/subscribe"
        )
          methods.push(method)
        server.send(frame)
      })
    })
    await fixture.openApp(page)
    await page
      .locator(`[data-conversation-id="${longConversationID}"]`)
      .getByRole("link")
      .click()
    for (const text of ["长对话第 1 轮", "ok"])
      await expect(
        getMessages(page).getByText(text, { exact: true })
      ).toHaveCount(1)
    expect(dropped).toBe(true)
    expect(documents).toBe(1)
    expect(subscriptions).toHaveLength(2)
    expect(subscriptions[1]).toEqual([
      "conversation/subscribe",
      "session/subscribe",
    ])
  })
}
