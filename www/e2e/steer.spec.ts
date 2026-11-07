import type { Content } from "../src/lib/api/protocol.gen"
import {
  expect,
  getComposer,
  getMessages,
  startConversation,
  test,
} from "./fixture"

const imageURL =
  "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aJxkAAAAASUVORK5CYII="
const mixed: Content[] = [
  { kind: "image", image: { data_url: imageURL, alt: "第一张原始说明" } },
  { kind: "text", text: { text: "  第一段\n" } },
  { kind: "text", text: { text: "/not-a-skill 材料保留", is_context: true } },
  { kind: "image", image: { data_url: imageURL, alt: "第二张原始说明" } },
  { kind: "text", text: { text: "第二段  " } },
]

// 显式点击排队按钮，主发送仍使用 steer。
async function queue(page: import("@playwright/test").Page, text: string) {
  await getComposer(page).fill(text)
  await page.getByRole("button", { name: "排队", exact: true }).click()
  await expect(getComposer(page)).toHaveValue("")
}

test(
  "direct_send_queue_join_prioritize_and_stop",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    await fixture.replies({ text: "第一轮生成中", hold: true })
    await fixture.openApp(page)
    await startConversation(page, "开始")
    const messages = getMessages(page)
    await expect(messages.getByText("第一轮生成中")).toBeVisible()
    await expect(
      page.getByRole("button", { name: "中断", exact: true })
    ).toBeVisible()
    await getComposer(page).fill("补充一句")
    await expect(
      page.getByRole("button", { name: "中断", exact: true })
    ).toHaveCount(0)
    await getComposer(page).press("Enter")
    const pending = page.getByRole("region", { name: "待处理", exact: true })
    await expect(pending.getByText("补充一句", { exact: true })).toBeVisible()
    await queue(page, "加入的一句")
    await pending
      .locator("[data-pending-input]")
      .filter({ hasText: "加入的一句" })
      .getByRole("button", { name: "加入当前轮" })
      .click()
    await expect(
      pending
        .locator("[data-pending-input]")
        .filter({ hasText: "加入的一句" })
        .getByRole("button", { name: "加入当前轮" })
    ).toHaveCount(0)
    await expect(
      page.locator('[data-sonner-toast][data-type="success"]')
    ).toHaveCount(0)
    await queue(page, "普通排队")
    await queue(page, "优先的一句")
    await fixture.replies({ text: "优先这一轮生成中", hold: true })
    await pending
      .locator("[data-pending-input]")
      .filter({ hasText: "优先的一句" })
      .getByRole("button", { name: "待处理消息的更多操作" })
      .click()
    await page.getByRole("menuitem", { name: "插队", exact: true }).click()
    await expect(
      messages.getByText("这一轮已中断", { exact: true })
    ).toHaveCount(1)
    await expect(messages.getByText("优先这一轮生成中")).toBeVisible()
    await expect(
      messages.getByText("优先的一句", { exact: true })
    ).toBeVisible()
    // 新轮启动时即读取同批补充，显示在历史而非等待模型调用结束。
    await expect(messages.getByText("补充一句", { exact: true })).toBeVisible()
    await page.getByRole("button", { name: "中断", exact: true }).click()
    await expect(messages.getByText("普通排队", { exact: true })).toBeVisible()
    await expect(pending).toHaveCount(0)
  }
)

test(
  "retrieve_complex_content_resend_unchanged_then_edit_ordinary_only",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    await fixture.replies({ text: "保持运行", hold: true })
    await fixture.openApp(page)
    await startConversation(page, "复杂内容")
    await expect(getMessages(page).getByText("保持运行")).toBeVisible()
    const conversationID = page.url().split("/c/")[1]
    // 同一个运行中的对话里，先原样重新发送，再只改普通文字后重新发送。
    for (const edited of [false, true]) {
      const { itemID } = await fixture.submitInput(conversationID, mixed)
      const row = page.locator(`[data-pending-input="${itemID}"]`)
      await expect(row.getByText("图片 × 2")).toBeVisible()
      await expect(row.locator("img")).toHaveCount(0)
      // 取回成功覆盖已有文字和图片，不弹覆盖确认。
      await getComposer(page).fill("原来的草稿")
      await row.getByRole("button", { name: "取回修改" }).click()
      await expect(getComposer(page)).toHaveValue("  第一段\n第二段  ")
      await expect(
        page.locator('[data-sonner-toast][data-type="success"]')
      ).toHaveCount(0)
      await expect(
        page.getByRole("button", { name: "预览图片 第一张原始说明" })
      ).toBeVisible()
      await expect(
        page.getByRole("button", { name: "预览图片 第二张原始说明" })
      ).toBeVisible()
      await page.getByRole("button", { name: "附带材料", exact: true }).click()
      await expect(
        page.getByText("/not-a-skill 材料保留", { exact: true })
      ).toBeVisible()
      await expect(page.getByRole("listbox", { name: "Skill" })).toHaveCount(0)
      if (edited) await getComposer(page).fill("修改后的正文\n")
      await page.getByRole("button", { name: "排队", exact: true }).click()
      const pending = page.getByRole("region", { name: "待处理", exact: true })
      await expect(pending.locator("[data-pending-input]")).toHaveCount(1)
      const newID = await pending
        .locator("[data-pending-input]")
        .getAttribute("data-pending-input")
      const returned = await fixture.retrieveInput(conversationID, newID!)
      const expected = edited
        ? [
            mixed[0],
            { kind: "text", text: { text: "修改后的正文\n" } },
            mixed[2],
            mixed[3],
          ]
        : mixed
      expect(returned.content).toEqual(expected)
      await expect(getComposer(page)).toHaveValue("")
      await expect(pending.locator("[data-pending-input]")).toHaveCount(0)
    }
    await page.getByRole("button", { name: "中断", exact: true }).click()
  }
)

test(
  "withdraw_confirmation_failure_keeps_dialog_and_draft",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    await fixture.replies({ text: "不被取消", hold: true })
    await fixture.openApp(page)
    await startConversation(page, "确认失败")
    await expect(getMessages(page).getByText("不被取消")).toBeVisible()
    const conversationID = page.url().split("/c/")[1]
    const { itemID } = await fixture.submitInput(conversationID, [
      { kind: "text", text: { text: "会被别处取回" } },
    ])
    const row = page.locator(`[data-pending-input="${itemID}"]`)
    await row.getByRole("button", { name: "待处理消息的更多操作" }).click()
    await page.getByRole("menuitem", { name: "撤回", exact: true }).click()
    const dialog = page.getByRole("alertdialog")
    await expect(dialog).toBeVisible()
    await fixture.retrieveInput(conversationID, itemID)
    await dialog.getByRole("button", { name: "撤回", exact: true }).click()
    await expect(dialog.getByRole("alert")).toBeVisible()
    await expect(dialog).toBeVisible()
    await dialog.getByRole("button", { name: "取消" }).click()
    await expect(getMessages(page).getByText("这一轮已中断")).toHaveCount(0)
    await queue(page, "真正撤回")
    await getComposer(page).fill("保留草稿")
    const nextRow = page
      .locator("[data-pending-input]")
      .filter({ hasText: "真正撤回" })
    await nextRow.getByRole("button", { name: "待处理消息的更多操作" }).click()
    await page.getByRole("menuitem", { name: "撤回", exact: true }).click()
    await dialog.getByRole("button", { name: "撤回", exact: true }).click()
    await expect(dialog).toHaveCount(0)
    await expect(nextRow).toHaveCount(0)
    await expect(getComposer(page)).toHaveValue("保留草稿")
    await getComposer(page).fill("")
    await page.getByRole("button", { name: "中断", exact: true }).click()
  }
)

test(
  "creating_conversation_migrates_new_text_and_image_while_first_send_waits",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    let release: (() => void) | undefined
    let received: (() => void) | undefined
    const waiting = new Promise<void>((resolve) => {
      received = resolve
    })
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
      const server = socket.connectToServer()
      socket.onMessage(async (message) => {
        const request = JSON.parse(String(message)) as { method?: string }
        if (request.method === "conversation/submitInput") {
          received?.()
          await gate
        }
        server.send(message)
      })
      server.onMessage((message) => socket.send(message))
    })
    await fixture.openApp(page)
    await getComposer(page).fill("第一条")
    await page.getByRole("button", { name: "发送", exact: true }).click()
    await waiting
    await getComposer(page).fill("第一条\n请求期间新写")
    await page.locator('input[type="file"]').setInputFiles({
      name: "期间新增.png",
      mimeType: "image/png",
      buffer: Buffer.from(imageURL.split(",")[1], "base64"),
    })
    await expect(
      page.getByRole("button", { name: "预览图片 期间新增.png" })
    ).toBeVisible()
    release?.()
    await expect(page).toHaveURL(/\/c\/[^/]+$/)
    await expect(getComposer(page)).toHaveValue("请求期间新写")
    await expect(
      page.getByRole("button", { name: "预览图片 期间新增.png" })
    ).toBeVisible()
    await expect(
      getMessages(page).getByText("第一条", { exact: true })
    ).toBeVisible()
  }
)

test(
  "retrieval_race_failure_preserves_draft_and_does_not_stop_turn",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    let race = false
    await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
      const server = socket.connectToServer()
      socket.onMessage(async (message) => {
        const request = JSON.parse(String(message)) as {
          method?: string
          params?: {
            conversation_id: string
            item_id: string
            return_content?: boolean
          }
        }
        if (
          race &&
          request.method === "conversation/withdrawInput" &&
          request.params?.return_content
        ) {
          race = false
          await fixture.retrieveInput(
            request.params.conversation_id,
            request.params.item_id
          )
        }
        server.send(message)
      })
      server.onMessage((message) => socket.send(message))
    })
    await fixture.replies({ text: "取回失败仍运行", hold: true })
    await fixture.openApp(page)
    await startConversation(page, "取回竞争")
    await expect(getMessages(page).getByText("取回失败仍运行")).toBeVisible()
    await queue(page, "别处先取走")
    await getComposer(page).fill("不能丢的草稿")
    race = true
    await page
      .locator("[data-pending-input]")
      .getByRole("button", { name: "取回修改" })
      .click()
    await expect(
      page.getByText("input is no longer pending", { exact: true })
    ).toBeVisible()
    await expect(getComposer(page)).toHaveValue("不能丢的草稿")
    await expect(
      getMessages(page).getByText("这一轮已中断", { exact: true })
    ).toHaveCount(0)
    await getComposer(page).fill("")
    await page.getByRole("button", { name: "中断", exact: true }).click()
  }
)

test("composer_main_button_keeps_dom_and_position", async ({
  page,
  fixture,
}) => {
  // 1. 运行中的空草稿显示停止，记录同一主按钮和右侧位置。
  await fixture.replies({ text: "输入控件连续性检查", hold: true })
  await fixture.openApp(page)
  await startConversation(page, "按钮原位切换")
  await expect(getMessages(page).getByText("输入控件连续性检查")).toBeVisible()
  const stop = page.getByRole("button", { name: "中断", exact: true })
  const main = await stop.elementHandle()
  const bounds = (await stop.boundingBox())!
  // 2. 同一主按钮原位在发送和停止间切换。
  // 有内容时原位变为发送，排队按钮完整占位但主按钮不移动。
  await getComposer(page).fill("尚未发送的内容")
  const send = page.getByRole("button", { name: "发送", exact: true })
  const queueButton = page.getByRole("button", { name: "排队", exact: true })
  await expect(send).toBeEnabled()
  await expect(queueButton).toBeEnabled()
  await expect(queueButton).toHaveCSS("width", "32px")
  await expect(queueButton).toHaveCSS("height", "32px")
  await expect(send).toHaveCSS("width", "32px")
  await expect(send).toHaveCSS("height", "32px")
  expect(
    await main!.evaluate(
      (element) =>
        element.isConnected && element.getAttribute("aria-label") === "发送"
    )
  ).toBe(true)
  await expect
    .poll(async () => {
      const queued = (await queueButton.boundingBox())!
      const sent = (await send.boundingBox())!
      return Math.round(sent.x - queued.x - queued.width)
    })
    .toBe(4)
  expect((await send.boundingBox())!.x).toBeCloseTo(bounds.x, 1)
  // 清空时立即变回停止，退出中的排队按钮不可交互。
  await getComposer(page).fill("")
  await expect(stop).toBeEnabled()
  expect(
    await main!.evaluate(
      (element) =>
        element.isConnected && element.getAttribute("aria-label") === "中断"
    )
  ).toBe(true)
  const exitingQueue = page.locator('button[aria-label="排队"]')
  // 一次读完退出中的排队按钮；动画很快结束时它可能已不在 DOM。
  const exitingStates = await exitingQueue.evaluateAll((buttons) =>
    buttons.map((button) => ({
      inert: button.closest("[inert]") !== null,
      disabled:
        (button as HTMLButtonElement).disabled ||
        button.getAttribute("aria-disabled") === "true",
    }))
  )
  for (const state of exitingStates)
    expect(state).toEqual({ inert: true, disabled: true })
  await expect(exitingQueue).toHaveCount(0)
  expect((await stop.boundingBox())!.x).toBeCloseTo(bounds.x, 1)
  await stop.click()
})

for (const mode of ["normal", "steer"] as const) {
  test(`pending_inputs_stay_with_conversation_after_switch_${mode}`, async ({
    page,
    fixture,
  }) => {
    // 1. 两个运行中的对话各留一条待处理输入，记录各自的真实 ItemID。
    await fixture.openApp(page)
    const conversations: { path: string; itemID: string; text: string }[] = []
    for (const name of ["A", "B"]) {
      if (conversations.length > 0)
        await page
          .getByRole("link", { name: "新对话", exact: true })
          .first()
          .click()
      await fixture.replies({ text: `对话 ${name} 保持运行`, hold: true })
      await startConversation(page, `切换待处理 ${name}`)
      await expect(
        getMessages(page).getByText(`对话 ${name} 保持运行`)
      ).toBeVisible()
      const text = `只提交一次 ${name}`
      if (mode === "normal") await queue(page, text)
      else {
        await getComposer(page).fill(text)
        await getComposer(page).press("Enter")
        await expect(getComposer(page)).toHaveValue("")
      }
      // 先开的对话标签页还挂着，只看当前显示的这一个。
      const rows = page.locator("[data-pending-input]:visible")
      await expect(rows).toHaveCount(1)
      conversations.push({
        path: new URL(page.url()).pathname,
        itemID: (await rows.getAttribute("data-pending-input"))!,
        text,
      })
    }
    // 2. 通过侧栏反复切回，只有目标对话的一份列表，待处理内容尚未进入历史。
    for (const target of [...conversations, ...conversations]) {
      await page.locator(`a[href="${target.path}"]`).first().click()
      const pending = page.getByRole("region", { name: "待处理", exact: true })
      await expect(
        pending.locator(`[data-pending-input="${target.itemID}"]`)
      ).toBeVisible()
      await expect(pending).toHaveCount(1)
      await expect(page.locator("[data-pending-input]:visible")).toHaveCount(1)
      await expect(pending.locator("[data-pending-input]")).toHaveAttribute(
        "data-pending-input",
        target.itemID
      )
      await expect(
        pending.getByText(target.text, { exact: true })
      ).toBeVisible()
      await expect(
        getMessages(page).getByText(target.text, { exact: true })
      ).toHaveCount(0)
    }
    // 3. 取回本对话的输入后列表消失，不残留另一对话的待处理区域。
    for (const target of conversations) {
      await page.locator(`a[href="${target.path}"]`).first().click()
      await page
        .locator(`[data-pending-input="${target.itemID}"]`)
        .getByRole("button", { name: "取回修改", exact: true })
        .click()
      await expect(getComposer(page)).toHaveValue(target.text)
      await expect(
        page.getByRole("region", { name: "待处理", exact: true })
      ).toHaveCount(0)
      await getComposer(page).fill("")
      await page.getByRole("button", { name: "中断", exact: true }).click()
    }
  })
}

test("pending_exit_closes_portalled_menu_and_keeps_one_line", async ({
  page,
  fixture,
}) => {
  await fixture.replies({ text: "等待待处理菜单检查", hold: true })
  await fixture.openApp(page)
  await startConversation(page, "待处理退场")
  await expect(getMessages(page).getByText("等待待处理菜单检查")).toBeVisible()
  const conversationID = page.url().split("/c/")[1]
  const { itemID } = await fixture.submitInput(conversationID, mixed)
  const row = page.locator(`[data-pending-input="${itemID}"]`)
  // 1. 同一条待处理输入的文字、图片数量和操作在桌面和窄屏下都共用 32px 单行。
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 844 })
    await expect(row).toHaveCSS("height", "32px")
    await expect(row.locator("p")).toHaveCSS("white-space", "nowrap")
    await expect(row.getByText("图片 × 2")).toBeVisible()
  }
  await row.getByRole("button", { name: "待处理消息的更多操作" }).click()
  await expect(page.getByRole("menu")).toBeVisible()
  // 2. 外部消费最后一条输入时，退场视觉不能留下 body 上可聚焦的菜单。
  await page.clock.install()
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000))
  await fixture.retrieveInput(conversationID, itemID)
  await expect(page.getByRole("menu")).toBeHidden()
  expect(
    await page.evaluate(
      () => document.activeElement?.closest('[role="menu"]') === null
    )
  ).toBe(true)
  const retainedMenu = page.locator('[data-slot="dropdown-menu-content"]')
  if (await retainedMenu.count()) {
    expect(
      await retainedMenu.evaluate((element) => element.hasAttribute("inert"))
    ).toBe(true)
  }
  await page.clock.runFor(400)
  await expect(row).toHaveCount(0)
  await page.clock.resume()
  await page.getByRole("button", { name: "中断", exact: true }).click()
})
