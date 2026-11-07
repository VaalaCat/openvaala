import {
  expect,
  openTurnRaw,
  getMessages,
  startConversation,
  test,
} from "./fixture"

test(
  "clipboard_on_insecure_origin",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    test.skip(!fixture.state.lanURL, "this machine has no non-loopback IPv4")
    // 1. 用局域网地址打开：非安全上下文，没有 navigator.clipboard。
    await fixture.openApp(page, fixture.state.lanURL)
    expect(await page.evaluate(() => navigator.clipboard)).toBeUndefined()
    // 2. 在原始数据里复制一个节点：共用按钮就地提示已复制，不弹节点 toast。
    await startConversation(page, "局域网复制")
    await expect(
      getMessages(page).getByText("ok", { exact: true })
    ).toBeVisible()
    const row = await openTurnRaw(
      getMessages(page).locator("[data-turn-id]").last()
    )
    await row.getByRole("button", { name: "复制这个节点" }).first().click()
    await expect(
      row.getByRole("status").filter({ hasText: "已复制" })
    ).toBeVisible()
    await expect(
      page.getByRole("region", { name: /通知/ }).getByText("已复制")
    ).toHaveCount(0)
  }
)

for (const { width, colorScheme } of [
  { width: 1280, colorScheme: "light" },
  { width: 1280, colorScheme: "dark" },
  { width: 390, colorScheme: "light" },
] as const) {
  test(`copy_status_stays_inside_long_json_${width}_${colorScheme}`, async ({
    page,
    fixture,
  }) => {
    await page.setViewportSize({ width, height: 844 })
    await page.emulateMedia({ colorScheme })
    await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
    // 1. 打开真实 Turn 的长记录：JSON 内部可以横滚，消息、输入区与页面不能被复制提示撑宽。
    const response =
      "复制状态不会改变布局。\n\n```json\n" +
      JSON.stringify({ text: "long-json-value-".repeat(80) }) +
      "\n```"
    await fixture.replies({ text: response })
    await fixture.openApp(page)
    await startConversation(page, "检查长记录复制")
    const turn = getMessages(page).locator("[data-turn-id]").last()
    await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()
    const raw = await openTurnRaw(turn)
    const jsonScroll = raw.locator("[data-output-scroll]")
    await expect
      .poll(() =>
        jsonScroll.evaluate((node) => node.scrollWidth - node.clientWidth)
      )
      .toBeGreaterThan(0)
    const expectNoOuterOverflow = async () => {
      await expect
        .poll(() =>
          page.evaluate(() => {
            const candidates = [
              document.documentElement,
              document.querySelector('[data-slot="sidebar-inset"] > div'),
              document.querySelector('[data-slot="message-scroller"]'),
              document.querySelector('[data-slot="drawer-content"]'),
            ].filter((node): node is HTMLElement => node instanceof HTMLElement)
            return Math.max(
              ...candidates.map((node) => node.scrollWidth - node.clientWidth)
            )
          })
        )
        .toBeLessThanOrEqual(1)
    }
    await expectNoOuterOverflow()
    // 2. 使用行尾复制后，隐藏的无障碍提示仍留在当前复制控件；局部横滚照常可用。
    const node = raw.locator(`[data-json-path='["turn","id"]']`)
    await node
      .getByRole("button", { name: "复制这个节点", exact: true })
      .click()
    await expect(
      raw.getByRole("status").filter({ hasText: "已复制" })
    ).toBeAttached()
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()))
      .toMatch(/^[0-9a-f-]+$/)
    await expectNoOuterOverflow()
    await jsonScroll.evaluate((node) => {
      node.scrollLeft = node.scrollWidth
    })
    expect(
      await jsonScroll.evaluate((node) => node.scrollLeft)
    ).toBeGreaterThan(0)
    await expectNoOuterOverflow()
  })
}
