import { mkdir } from "node:fs/promises"
import { hostname } from "node:os"
import path from "node:path"
import { expect, test } from "./fixture"

for (const health of ["available", "unreachable"] as const) {
  test(`same-origin Runtime shows token entry when health is ${health}`, async ({
    page,
    fixture,
  }) => {
    if (health === "unreachable")
      await page.route("**/api/v1/healthz", (route) => route.abort())
    await page.goto(fixture.state.url + "/web/v1/")
    await expect(
      page.getByRole("heading", { name: "输入 Token" })
    ).toBeVisible()
    await expect(page.getByLabel("Token", { exact: true })).toBeVisible()
  })
}

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

test("internal navigation supports pointer and keyboard without a hover URL", async ({
  page,
  context,
  fixture,
}) => {
  await fixture.openApp(page)
  const home = page.url()
  const destination = home + "/settings/general"
  const settings = page.getByRole("link", { name: "设置", exact: true })
  await settings.hover()
  expect(await settings.evaluate((el) => el.tagName)).toBe("BUTTON")
  await expect(settings).not.toHaveAttribute("href")
  await expect(page.locator("a[href]:hover")).toHaveCount(0)
  for (const openTab of [
    () => settings.click({ modifiers: ["Control"] }),
    () => settings.click({ modifiers: ["Meta"] }),
    () => settings.click({ button: "middle" }),
  ]) {
    const [tab] = await Promise.all([
      context.waitForEvent("page"),
      openTab(),
    ])
    await expect(tab).toHaveURL(destination)
    await expect(page).toHaveURL(home)
    await tab.close()
  }
  for (const key of ["Enter", "Space", "click"]) {
    if (key === "click") await settings.click()
    else await settings.press(key)
    await expect(page).toHaveURL(destination)
    await page.goBack()
    await expect(page).toHaveURL(home)
  }
})

for (const scenario of ["standalone", "joined", "remote"] as const) {
  test(`server switcher shows name and address for ${scenario}`, async ({
    page,
    fixture,
  }) => {
    const directory = process.env.SIDEBAR_NAME_SHOTS
    async function check(name: string, address: string) {
      for (const width of [1280, 390]) {
        await page.setViewportSize({ width, height: 844 })
        if (width === 390)
          await page.getByRole("button", { name: "切换侧栏" }).click()
        const header = page.locator("[data-server-connection-header]:visible")
        await expect(header.getByRole("link")).toHaveText(name)
        const trigger = header.getByRole("button", { name: name + " · 已连接" })
        async function capture(state: string) {
          if (!directory) return
          await mkdir(directory, { recursive: true })
          await page.screenshot({
            path: path.join(
              directory,
              scenario + "-" + width + "-" + state + ".png"
            ),
            animations: "disabled",
          })
        }
        await expect
          .poll(async () => {
            const icon = await trigger.boundingBox()
            const label = await header.getByRole("link").boundingBox()
            return !!icon && !!label && icon.x + icon.width <= label.x + 0.5
          })
          .toBe(true)
        await capture("closed")
        await trigger.click()
        const row = page.getByRole("menuitemradio", {
          name: name + " " + address,
          exact: true,
        })
        await expect(row).toBeChecked()
        const title = row.getByText(name, { exact: true })
        const subtitle = row.getByText(address, { exact: true })
        await expect(title).toBeVisible()
        await expect(subtitle).toHaveCSS("text-overflow", "ellipsis")
        expect((await subtitle.boundingBox())!.y).toBeGreaterThan(
          (await title.boundingBox())!.y
        )
        await capture("open")
        await page.mouse.click(16, 500)
        await expect(row).toBeHidden()
        await trigger.press("ArrowDown")
        await expect(row).toBeFocused()
        await page.keyboard.press("ArrowDown")
        await expect(
          page.getByRole("menuitem", { name: "管理服务器…" })
        ).toBeFocused()
        await page.keyboard.press("Escape")
        await expect(trigger).toBeFocused()
        if (scenario === "standalone" && width === 390) {
          await trigger.click()
          await page.getByRole("menuitem", { name: "管理服务器…" }).click()
          const overlay = page.locator('[data-slot="sheet-overlay"]')
          const sidebar = page.locator('[data-sidebar="sidebar"][data-mobile]')
          for (const title of ["管理服务器", "添加服务器"]) {
            await expect(
              page.getByRole("dialog", { name: title })
            ).toBeVisible()
            await expect(overlay).toHaveCSS("backdrop-filter", /blur\(/)
            const layer = await overlay.evaluate((e) =>
              Number(getComputedStyle(e).zIndex)
            )
            expect(
              await sidebar.evaluate((e) => Number(getComputedStyle(e).zIndex))
            ).toBeLessThan(layer)
            if (title === "管理服务器") {
              await page.getByRole("button", { name: "添加服务器" }).click()
            } else {
              const form = page.getByRole("dialog", { name: title })
              await expect(
                form.getByLabel("地址", { exact: true })
              ).toHaveAttribute(
                "placeholder",
                "粘贴远程访问链接，或 ws://host:8790"
              )
              await expect(
                form.getByLabel("Token", { exact: true })
              ).toBeHidden()
              const testButton = page.getByRole("button", {
                name: "测试",
                exact: true,
              })
              const save = page.getByRole("button", { name: "保存并切换" })
              await expect(testButton).toBeDisabled()
              const testBounds = (await testButton.boundingBox())!
              const saveBounds = (await save.boundingBox())!
              expect(testBounds.y).toBeCloseTo(saveBounds.y, 0)
              expect(testBounds.x + testBounds.width).toBeLessThan(saveBounds.x)
            }
          }
          await page.keyboard.press("Escape")
        }
      }
    }
    if (scenario === "standalone") {
      await fixture.openApp(page)
      await check(hostname(), new URL(fixture.state.url).host)
    } else {
      await fixture.withWorldRuntime(
        async (world) => {
          await page.goto(
            scenario === "remote"
              ? world.link
              : world.url + "/web/v1/#token=" + world.token
          )
          await expect(page).toHaveURL(/\/s\/[^/#]+$/)
          await check(
            "studio",
            scenario === "remote"
              ? new URL(world.relay).host
              : "bob@partner.test"
          )
          if (scenario === "remote") {
            await page.goto(new URL(world.link).origin)
            await page.getByRole("button", { name: "使用地址连接" }).click()
            const form = page.getByRole("dialog")
            const host = new URL(fixture.state.url).host
            await form.getByLabel("名称").fill(host)
            await form
              .getByLabel("地址", { exact: true })
              .fill(fixture.state.url)
            await form
              .getByLabel("Token", { exact: true })
              .fill(fixture.state.token)
            await form
              .getByRole("button", { name: "保存", exact: true })
              .click()
            const savedRow = form
              .getByRole("button", { name: host, exact: true })
              .locator("..")
            await savedRow.getByRole("button", { name: "编辑" }).click()
            await expect(form.getByLabel("Token", { exact: true })).toHaveValue(
              fixture.state.token
            )
            await form
              .getByRole("button", { name: "返回", exact: true })
              .click()
            await form
              .getByRole("button", { name: "关闭", exact: true })
              .click()
            await expect(
              page.getByRole("button", { name: host, exact: true })
            ).toBeVisible()
            await expect(
              page.getByRole("button", {
                name: "studio " + new URL(world.relay).host,
                exact: true,
              })
            ).toBeVisible()
          }
        },
        {
          nodeName: "bob",
          domainName: "partner.test",
          joinDomain: scenario === "joined",
        }
      )
    }
  })
}
