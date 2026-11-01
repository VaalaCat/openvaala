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
