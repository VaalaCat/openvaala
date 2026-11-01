import { expect, test } from "./fixture"

test("connecting_title_omits_server_address", async ({ page, fixture }) => {
  // 1. 保持首次握手未完成，页内与浏览器标题只显示连接状态。
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    socket.onMessage(() => undefined)
  })
  await page.goto(
    fixture.state.url +
      "/web/v1/#token=" +
      encodeURIComponent(fixture.state.token)
  )
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("正在连接…")
  await expect(page).toHaveTitle("正在连接… OpenVaala")
})

test(
  "open_token_link_saves_token_and_shows_empty_list",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    // 1. 打开带 #token= 的地址：连上后进入新对话页，地址里不再有 #。
    await fixture.openApp(page)
    await expect(page).not.toHaveURL(/#/)
    // 2. token 写进了 localStorage。
    await expect
      .poll(async () =>
        JSON.parse(
          (await page.evaluate(() =>
            localStorage.getItem("openvaala.servers")
          )) ?? "[]"
        )
      )
      .toContainEqual(expect.objectContaining({ token: fixture.state.token }))
    // 3. 还没有对话时列表显示空状态。
    await expect(page.getByText("还没有对话")).toBeVisible()
  }
)
