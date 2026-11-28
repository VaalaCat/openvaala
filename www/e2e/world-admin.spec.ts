import jsQR from "jsqr"
import path from "node:path"
import { mkdir } from "node:fs/promises"
import type { WebSocketRoute } from "@playwright/test"
import { startConversation } from "./fixture"
import type { Page, Locator } from "@playwright/test"
import { captureWorldReview, expect, routeWorldRPC, test } from "./fixture"
import type { WorldAdminNode, WorldJoinToken } from "../src/lib/api/world-admin"

const adminToken = "test-world-admin"
function mockNode(id: string, owner: string, online: boolean): WorldAdminNode {
  return {
    node_id: id,
    node_name: id,
    domain: "alice.dev",
    owner,
    tags: ["team"],
    online,
    targets: online ? [{ name: "default", visibility: "domain" }] : [],
    revoked_at: 0,
    created_at: Date.now(),
  }
}

async function rowAction(page: Page, row: Locator, label: string) {
  await row.getByRole("button", { name: /的操作$/ }).click()
  await page.getByRole("menuitem", { name: label, exact: true }).click()
}

async function mockAdmin(page: Page, prefix: string, token: string) {
  const nodes = [
    mockNode("office", "alice", true),
    mockNode("home", "bob", false),
    mockNode("lab", "bob", true),
  ]
  const tokens: WorldJoinToken[] = []
  const calls: string[] = []
  await page.route("**" + prefix + "/**", async (route) => {
    const request = route.request()
    const suffix = new URL(request.url()).pathname.slice(prefix.length + 1)
    if (request.headers().authorization !== "Bearer " + token)
      return route.fulfill({ status: 401 })
    calls.push(request.method() + " " + suffix)
    if (suffix === "nodes") return route.fulfill({ json: nodes })
    if (suffix === "tokens") {
      if (request.method() === "GET") return route.fulfill({ json: tokens })
      const data = request.postDataJSON()
      expect(data.expires_at).toBeGreaterThan(Date.now())
      const created = { ...data, id: "new-token", used_count: 0, revoked_at: 0 }
      tokens.push(created)
      return route.fulfill({
        status: 201,
        json: {
          ...created,
          token: new URL(request.url()).origin + "#plaintext-token",
        },
      })
    }
    if (suffix === "domains")
      return route.fulfill({
        json: { domain: "alice.dev", self_card: "", domains: [] },
      })
    if (suffix === "users")
      return route.fulfill({
        json: {
          users: ["alice", "bob"].map((owner) => ({
            owner,
            node_count: nodes.filter((n) => n.owner === owner && !n.revoked_at)
              .length,
            online_count: nodes.filter((n) => n.owner === owner && n.online)
              .length,
            token_count: 0,
            rule_count: 0,
          })),
        },
      })
    const [resource, id] = suffix.split("/")
    if (resource === "tokens") {
      tokens.find((item) => item.id === id)!.revoked_at = Date.now()
    } else {
      nodes
        .filter((node) =>
          resource === "owners" ? node.owner === id : node.node_id === id
        )
        .forEach((node) => {
          node.revoked_at = Date.now()
          node.online = false
          node.targets = []
        })
    }
    return route.fulfill({ status: 204 })
  })
  return { calls }
}

test("standalone fragment login is isolated from Runtime credentials and connections", async ({
  page,
  fixture,
}) => {
  const url = fixture.state.url
  const saved = [
    {
      url: url.replace(/^http/, "ws") + "/api/v1/openvaala",
      token: "existing-serve",
    },
  ]
  await page.addInitScript((value) => {
    localStorage.setItem("openvaala.servers", JSON.stringify(value))
  }, saved)
  const sockets: string[] = []
  page.on("websocket", (socket) => sockets.push(socket.url()))
  await mockAdmin(page, "/world/v1/admin", adminToken)

  await page.goto(url + "/web/v1/world/admin#token=" + adminToken)
  await expect(page.getByRole("row").filter({ hasText: "alice" })).toBeVisible()
  await expect(page).toHaveURL(url + "/web/v1/world/admin")
  await expect(
    page.getByText("在控制面配置中继节点或节点直连地址后生成名片")
  ).toBeVisible()
  await expect(page.getByRole("button", { name: "去设置" })).toHaveCount(0)
  expect(
    await page.evaluate(() =>
      JSON.parse(localStorage.getItem("openvaala.servers")!)
    )
  ).toEqual(saved)
  expect(sockets).toEqual([])
  await page.getByRole("button", { name: "退出登录" }).click()
  await page.reload()
  await expect(page.getByLabel("管理令牌")).toBeVisible()
  expect(sockets).toEqual([])
})

test("standalone manages nodes, owners and join tokens", async ({
  page,
  fixture,
}) => {
  const url = fixture.state.url
  await mockAdmin(page, "/world/v1/admin", adminToken)
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  await page.goto(url + "/web/v1/world/admin")
  const token = page.getByLabel("管理令牌")
  const login = page.getByRole("button", { name: "登录", exact: true })
  await token.fill("wrong-token")
  await login.click()
  await expect(page.getByRole("alert")).toHaveText("管理令牌无效")
  await token.fill(adminToken)
  await login.click()

  const row = (name: string) => page.getByRole("row").filter({ hasText: name })
  await page.getByRole("tab", { name: "节点", exact: true }).click()
  await expect(
    row("office@alice.dev").getByRole("img", { name: "在线" })
  ).toBeVisible()
  await rowAction(page, row("office@alice.dev"), "吊销节点")
  await expect(page.getByRole("alertdialog")).toContainText("office@alice.dev")
  await page.getByRole("button", { name: "确认吊销" }).click()
  await expect(
    row("office@alice.dev").getByText("已吊销", { exact: true })
  ).toBeVisible()
  await page.getByRole("tab", { name: "用户", exact: true }).click()
  await rowAction(
    page,
    page
      .getByRole("table", { name: "用户", exact: true })
      .getByRole("row")
      .filter({ hasText: "bob" }),
    "吊销全部"
  )
  await expect(page.getByRole("alertdialog")).toContainText("bob")
  await page.getByRole("button", { name: "确认吊销" }).click()
  await page.getByRole("tab", { name: "节点", exact: true }).click()
  await expect(
    row("lab@alice.dev").getByText("已吊销", { exact: true })
  ).toBeVisible()

  await page.getByRole("tab", { name: "入网码" }).click()
  await page
    .getByRole("tabpanel", { name: "入网码", exact: true })
    .getByRole("button", { name: "签发入网码" })
    .click()
  const create = page.getByRole("dialog")
  await create.getByLabel("归属", { exact: true }).fill("charlie")
  await create.getByLabel("标签（逗号分隔）").fill("ops, device")
  await create.getByRole("checkbox", { name: "可重复使用" }).check()
  await create.getByRole("button", { name: "签发入网码" }).click()
  const join = "openvaala world join " + url + "#plaintext-token"
  await expect(create.getByText(join, { exact: true })).toBeVisible()
  await create.getByRole("button", { name: "复制入网命令" }).click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe(join)
  await create.getByRole("button", { name: "完成" }).click()
  await expect(page.getByText("plaintext-token")).toHaveCount(0)
  await rowAction(page, row("charlie"), "吊销入网码")
  await page.getByRole("button", { name: "确认吊销" }).click()
  await expect(row("charlie")).toContainText("已吊销")
})

test("co-hosted settings reuse the serve token and can switch to external credentials", async ({
  page,
  fixture,
}) => {
  const local: string[] = []
  await routeWorldRPC(page, (method) => {
    local.push(method)
    if (method === "world/getStatus")
      return { enabled: false, admin_enabled: true }
    if (method === "world/admin/listDomains")
      return { domain: "alice.dev", self_card: "", domains: [] }
    if (method === "world/admin/listUsers")
      return {
        users: [
          {
            owner: "alice",
            node_count: 1,
            online_count: 1,
            token_count: 0,
            rule_count: 0,
          },
        ],
      }
    if (method === "world/admin/listNodes")
      return [mockNode("office", "alice", true)]
    throw new Error("unexpected World method " + method)
  })
  const remote = await mockAdmin(page, "/world/v1/admin", adminToken)
  await fixture.openApp(page)
  await page.goto(page.url() + "/settings/world-admin")
  await page.getByRole("tab", { name: "域管理", exact: true }).click()
  await page.getByRole("button", { name: "机器 1" }).click()
  await expect(page.getByText("office@alice.dev")).toBeVisible()
  expect(local).toContain("world/admin/listNodes")
  await expect(page.getByLabel("管理令牌")).toHaveCount(0)
  await page.getByRole("button", { name: "外部 WorldServer" }).click()
  await page.getByLabel("管理令牌").fill(adminToken)
  await page.getByRole("button", { name: "登录", exact: true }).click()
  await expect(page.getByRole("button", { name: "退出登录" })).toBeVisible()
  await expect(
    page.getByText("在控制面配置中继节点或节点直连地址后生成名片")
  ).toBeVisible()
  await expect(page.getByRole("button", { name: "去设置" })).toHaveCount(0)
  expect(remote.calls).toContain("GET nodes")
  await page.getByRole("button", { name: "本机 WorldServer" }).click()
  await expect(page.getByRole("button", { name: "退出登录" })).toHaveCount(0)
})

function localSection(page: Page, name: string) {
  return page.getByRole("button", { name, exact: true })
}

test("local World summaries reflect live status and keep edits visible when collapsed", async ({
  page,
  fixture,
}) => {
  await fixture.withWorldRuntime(async (world) => {
    await page.goto(world.url + "/web/v1/#token=" + world.token)
    await expect(page).toHaveURL(/\/s\/[^/#]+$/)
    await page.goto(page.url() + "/settings/world-admin")
    const summaries = [
      ["本机节点", "b@home.test · 已连接"],
      ["远程访问", world.relay + " · 已连接"],
      ["作为中继节点", "不对外转发"],
      ["在本机托管控制面", "运行中 · home.test"],
      ["通告目标", "未设置"],
      ["直连地址", "未设置，经中继节点连接"],
    ]
    for (const [name, summary] of summaries) {
      const title = localSection(page, name)
      await expect(title).toHaveAttribute("aria-expanded", "false")
      await expect(title).toHaveAccessibleDescription(summary)
    }
    const entries = localSection(page, "中继节点")
    await entries.click()
    await captureWorldReview(
      page,
      "relay-settings",
      page.getByRole("group", { name: "中继节点", exact: true })
    )
    await entries.click()
    const remote = localSection(page, "远程访问")
    await remote.click()
    await captureWorldReview(
      page,
      "remote-access",
      page.getByRole("group", { name: "远程访问", exact: true })
    )
    const direct = localSection(page, "直连地址")
    await direct.click()
    const addresses = page.getByRole("group", { name: "直连地址", exact: true })
    await addresses.getByRole("textbox").fill("invalid")
    await addresses.getByRole("button", { name: "添加" }).click()
    await direct.click()
    await expect(direct).toHaveAccessibleDescription("已修改 1 个")
    await page
      .getByRole("group", { name: "远程访问", exact: true })
      .getByRole("button", { name: "更改" })
      .click()
    await page.getByLabel("页面地址").fill("https://page.test")
    await remote.click()
    await page.getByRole("button", { name: "保存并重启", exact: true }).click()
    await page
      .getByRole("alertdialog")
      .getByRole("button", { name: "保存并重启" })
      .click()
    await expect(
      addresses.getByRole("listitem").getByRole("textbox")
    ).toHaveValue("invalid")
    await expect(direct).toHaveAttribute("aria-expanded", "true")
    await expect(remote).toHaveAttribute("aria-expanded", "false")
    await page.getByRole("button", { name: "放弃", exact: true }).click()
    await expect(page.getByText("已修改", { exact: true })).toHaveCount(0)
    await expect(direct).toHaveAccessibleDescription("未设置，经中继节点连接")
  })
})

test("a domain without a public entry links to local relay settings", async ({
  page,
  fixture,
}) => {
  await fixture.withWorldRuntime(async (world) => {
    await page.goto(world.url + "/web/v1/#token=" + world.token)
    await expect(page).toHaveURL(/\/s\/[^/#]+$/)
    await page.goto(page.url() + "/settings/world-admin")
    const domain = page.getByRole("heading", { name: "home.test", exact: true })
    const adminTab = page.getByRole("tab", { name: "域管理", exact: true })
    const header = page.locator("header").filter({ has: domain })
    const copy = header.getByRole("button", { name: "本域的名片" })
    const shots = process.env.WORLD_ADMIN_SHOTS
    async function capture(name: string) {
      if (!shots) return
      await mkdir(shots, { recursive: true })
      await expect(
        page.getByRole("table", { name: "用户" }).getByText("studio")
      ).toBeVisible()
      await page.mouse.move(0, 0)
      await page.screenshot({
        path: path.join(shots, name + ".png"),
        animations: "disabled",
      })
    }
    async function checkHeader(label: Locator, action: Locator, width: number) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 720 })
      const [heading, text, button] = await Promise.all(
        [domain, label, action].map((element) =>
          element.evaluate((el) => el.getBoundingClientRect().toJSON())
        )
      )
      const middle = text.y + text.height / 2
      expect(button.y + button.height / 2).toBeCloseTo(middle, 0)
      expect(button.x).toBeGreaterThanOrEqual(text.right)
      expect(button.right).toBeLessThanOrEqual(width)
      if (width === 390) {
        expect(text.x).toBeCloseTo(heading.x, 0)
        expect(text.top).toBeGreaterThanOrEqual(heading.bottom)
        expect(
          await page.evaluate(() => document.documentElement.scrollWidth)
        ).toBe(width)
      } else {
        expect(heading.y + heading.height / 2).toBeCloseTo(middle, 0)
      }
    }

    await adminTab.click()
    const card = header.locator("code")
    await expect(copy).toBeVisible()
    for (const width of [1280, 390]) await checkHeader(card, copy, width)
    expect(
      await card.evaluate(
        (el) =>
          el.scrollWidth > el.clientWidth &&
          getComputedStyle(el).textOverflow === "ellipsis"
      )
    ).toBe(true)
    await capture("card-390")
    await page.setViewportSize({ width: 1280, height: 720 })
    await page.getByRole("tab", { name: "本机", exact: true }).click()
    const relays = page.getByRole("group", {
      name: "中继节点",
      exact: true,
    })
    await localSection(page, "中继节点").click()
    await relays.getByRole("button", { name: "移除" }).click()
    await page.getByRole("button", { name: "保存并重启", exact: true }).click()
    await page
      .getByRole("alertdialog")
      .getByRole("button", { name: "保存并重启" })
      .click()
    const restarted = page.getByText("serve 已重启", { exact: true })
    await expect(restarted).toBeVisible()
    await expect(localSection(page, "远程访问")).toHaveAccessibleDescription(
      "未连接"
    )
    await adminTab.click()
    const hint = header.getByText("还没有对外入口，别的域连不到这里")
    const settings = header.getByRole("button", { name: "去设置" })
    await expect(copy).toHaveCount(0)
    await expect(hint).toBeVisible()
    await expect(settings).toBeVisible()
    if (shots) await expect(restarted).toBeHidden()
    for (const width of [1280, 390]) {
      await checkHeader(hint, settings, width)
      const typography = (el: Element) => {
        const style = getComputedStyle(el)
        return [style.fontSize, style.lineHeight]
      }
      expect(await hint.evaluate(typography)).toEqual(
        await settings.evaluate(typography)
      )
      expect(
        await hint.evaluate(
          (el) =>
            el.clientWidth >= el.scrollWidth &&
            el.clientHeight >= el.scrollHeight
        )
      ).toBe(true)
      await capture(width === 390 ? "no-card-hint-390" : "no-card-hint")
    }
    await page.setViewportSize({ width: 1024, height: 450 })
    await settings.click()
    await expect(
      page.getByRole("tab", { name: "本机", exact: true })
    ).toHaveAttribute("aria-selected", "true")
    await expect(localSection(page, "中继节点")).toHaveAttribute(
      "aria-expanded",
      "true"
    )
    await expect(relays.getByLabel("中继节点地址")).toBeVisible()
    await expect(relays).toBeInViewport()
  })
})

test("local World settings confirm restart and refresh the running switches", async ({
  page,
  fixture,
}) => {
  await fixture.openApp(page)
  await page.goto(page.url() + "/settings/world-admin")
  const node = page.getByRole("switch", { name: "本机节点" })
  const server = page.getByRole("switch", { name: "在本机托管控制面" })
  await expect(node).toBeDisabled()
  await expect(server).not.toBeChecked()
  const nodeSection = localSection(page, "本机节点")
  const serverSection = localSection(page, "在本机托管控制面")
  await expect(nodeSection).toHaveAccessibleDescription("未加入")
  await expect(
    page.getByRole("group", { name: "远程访问", exact: true })
  ).toContainText("未开启")
  await expect(serverSection).toHaveAccessibleDescription("未开启")
  await nodeSection.click()
  await expect(page.getByLabel("入网码", { exact: true })).toBeVisible()
  await captureWorldReview(
    page,
    "join",
    page.getByRole("group", { name: "本机节点", exact: true })
  )
  const save = page
    .locator("header")
    .getByRole("button", { name: "保存并重启", exact: true })
  await expect(save).toHaveCount(0)
  await nodeSection.click()
  await serverSection.click()
  const help = page.getByText(
    "域是你和成员共用的名字，出现在地址 alice@home 里，不需要拥有真实域名。"
  )
  await expect(help).toBeVisible()
  await captureWorldReview(page, "domain-form", help)
  await serverSection.click()
  await nodeSection.click()
  await page.getByLabel("入网码", { exact: true }).fill("unsubmitted-code")
  await expect(save).toHaveCount(0)
  const join = page.getByRole("button", { name: "加入并重启" })
  const rightEdge = (element: Element) => element.getBoundingClientRect().right
  const fieldRight = await page
    .getByLabel("入网码", { exact: true })
    .evaluate(rightEdge)
  expect(await join.evaluate(rightEdge)).toBeCloseTo(fieldRight, 0)
  const observer = await page.context().newPage()
  await observer.goto(page.url())
  const observerSwitch = observer.getByRole("switch", {
    name: "在本机托管控制面",
  })
  await expect(observerSwitch).not.toBeChecked()
  await server.click()
  await expect(
    page.locator("header").getByRole("button", { name: "保存并重启" })
  ).toBeVisible()
  await expect(serverSection).toHaveAttribute("aria-expanded", "false")
  await expect(serverSection).toHaveAccessibleDescription("已修改 未开启")
  // 关闭的控制面表单在缺少域名时自动展开。
  await save.click()
  await page
    .getByRole("alertdialog")
    .getByRole("button", { name: "保存并重启" })
    .click()
  await expect(serverSection).toHaveAttribute("aria-expanded", "true")
  await expect(
    page.getByText("world.server.domain must not be empty", { exact: true })
  ).toBeVisible()
  await page.getByLabel("域", { exact: true }).fill("settings.test")
  await save.click()
  const confirmation = page.getByRole("alertdialog")
  await expect(confirmation).toContainText("正在执行的轮次会被中断")
  await confirmation.getByRole("button", { name: "保存并重启" }).click()
  await expect(page.getByText("serve 已重启", { exact: true })).toBeVisible()
  await expect(server).toBeChecked()
  await expect(observerSwitch).toBeChecked()
  await localSection(observer, "在本机托管控制面").click()
  await expect(observer.getByLabel("域", { exact: true })).toHaveValue(
    "settings.test"
  )
  await expect(observer.getByText("有未保存的更改")).toHaveCount(0)
  await observer.close()
  await nodeSection.click()
  await serverSection.click()
  await page.getByLabel("域", { exact: true }).fill("next.test")
  await save.click()
  await expect(confirmation).toContainText("原有节点、邀请码、ACL 会失效")
  await confirmation.getByRole("button", { name: "取消", exact: true }).click()
  await expect(page.getByLabel("域", { exact: true })).toHaveValue("next.test")
  await page.getByRole("button", { name: "放弃", exact: true }).click()
  await page.getByRole("tab", { name: "域管理", exact: true }).click()
  await page.getByRole("tab", { name: "入网码" }).click()
  await page
    .getByRole("tabpanel", { name: "入网码", exact: true })
    .getByRole("button", { name: "签发入网码" })
    .click()
  const create = page.getByRole("dialog")
  await create.getByLabel("归属", { exact: true }).fill("settings")
  await create.getByRole("button", { name: "签发入网码" }).click()
  const joinCommand = create.getByText(/^openvaala world join /)
  await expect(joinCommand).toBeVisible()
  await expect(page.getByText("serve 已重启", { exact: true })).toBeHidden()
  await captureWorldReview(page, "invite", create)
  const invite = (await joinCommand.textContent())!.slice(
    "openvaala world join ".length
  )
  await create.getByRole("button", { name: "完成" }).click()
  await page.getByRole("tab", { name: "本机", exact: true }).click()
  await page
    .getByRole("group", { name: "本机节点", exact: true })
    .getByLabel("入网码", { exact: true })
    .fill(invite)
  await expect(save).toHaveCount(0)
  await page.getByRole("button", { name: "加入并重启" }).click()
  await confirmation.getByRole("button", { name: "保存并重启" }).click()
  await expect(nodeSection).toHaveAccessibleDescription(
    /@settings\.test · 已连接$/
  )
  await nodeSection.click()
  await page.getByRole("button", { name: "更换控制面" }).click()
  const change = page.getByRole("dialog", { name: "更换控制面" })
  await change.getByLabel("入网码").fill("unsubmitted-code")
  await page.keyboard.press("Escape")
  await expect(change).toBeHidden()
  await expect(save).toHaveCount(0)

  const localNode = page.getByRole("group", { name: "本机节点", exact: true })
  const identity = localNode.getByRole("listitem")
  const address = identity.getByText(/@settings\.test$/)
  const nodeAddress = await address.innerText()
  const details = identity.getByText(fixture.state.url + " · 已连接", {
    exact: true,
  })
  await expect(address).toBeVisible()
  await expect(details).toBeVisible()
  const domain = page
    .getByRole("group", { name: "在本机托管控制面", exact: true })
    .getByLabel("域")
  await serverSection.click()
  await localSection(page, "直连地址").click()
  const defaultAgent = page.getByRole("combobox", { name: "默认 Agent" })
  const direct = page.getByRole("group", { name: "直连地址", exact: true })
  const addAddress = direct.getByRole("button", { name: "添加" })
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 960 })
    const inputWidth = (e: Element) => e.getBoundingClientRect().width
    expect(await defaultAgent.evaluate(inputWidth)).toBeCloseTo(
      await domain.evaluate(inputWidth),
      0
    )
    const button = await identity
      .getByRole("button", { name: "更换控制面" })
      .boundingBox()
    const text = await details.boundingBox()
    expect(button).not.toBeNull()
    expect(text).not.toBeNull()
    expect((await address.boundingBox())!.x).toBeCloseTo(
      (await defaultAgent.boundingBox())!.x,
      0
    )
    expect(button!.x + button!.width).toBeCloseTo(
      await addAddress.evaluate(rightEdge),
      0
    )
    if (width === 390)
      expect(button!.y).toBeGreaterThanOrEqual(text!.y + text!.height)
    else expect(button!.x).toBeGreaterThanOrEqual(text!.x + text!.width)
  }
  await localSection(page, "通告目标").click()
  const targets = page.getByRole("group", { name: "通告目标", exact: true })
  await expect(targets.getByText("未设置时自动通告默认 Agent")).toBeVisible()
  const directHint = direct.getByText(
    "其他节点可直接连入本机的地址；未设置时经控制面中继。",
    { exact: true }
  )
  await expect(directHint).toBeVisible()
  await expect(addAddress).toBeDisabled()
  await direct.getByRole("textbox").fill("wss://studio.example/world/v1/direct")
  await expect(save).toHaveCount(0)
  await addAddress.click()
  await targets.getByRole("button", { name: "添加" }).click()
  const target = targets.getByRole("listitem")
  const description = target.getByPlaceholder("描述")
  for (const width of [390, 1280]) {
    await page.setViewportSize({ width, height: 960 })
    const [agent, visibility, input, remove] = await Promise.all(
      [
        target.getByRole("combobox", { name: "Agent", exact: true }),
        target.getByRole("combobox", { name: "可见性" }),
        description,
        target.getByRole("button", { name: "移除" }),
      ].map((field) =>
        field.evaluate((e) => e.getBoundingClientRect().toJSON())
      )
    )
    expect(visibility.top).toBeCloseTo(agent.top, 0)
    expect(visibility.height).toBeCloseTo(agent.height, 0)
    expect(input.height).toBeCloseTo(agent.height, 0)
    expect(remove.top + remove.height / 2).toBeCloseTo(
      input.top + input.height / 2,
      0
    )
    if (width === 390) expect(input.top).toBeGreaterThanOrEqual(agent.bottom)
    else expect(input.top).toBeCloseTo(agent.top, 0)
  }
  await description.fill("Studio")
  await save.click()
  await confirmation.getByRole("button", { name: "保存并重启" }).click()
  await expect(localSection(page, "通告目标")).toHaveAccessibleDescription(
    "1 个目标"
  )
  await expect(localSection(page, "直连地址")).toHaveAccessibleDescription(
    "1 个"
  )
  await localSection(page, "通告目标").click()
  await localSection(page, "直连地址").click()
  await expect(targets.getByRole("listitem").getByLabel("描述")).toHaveValue(
    "Studio"
  )
  await expect(direct.getByRole("listitem").getByRole("textbox")).toHaveValue(
    "wss://studio.example/world/v1/direct"
  )
  await expect(directHint).toBeVisible()
  for (const list of [targets, direct]) {
    await list.getByRole("button", { name: "移除" }).click()
    await expect(list.getByRole("listitem")).toHaveCount(0)
  }
  await expect(targets.getByText("未设置时自动通告默认 Agent")).toBeVisible()
  await expect(directHint).toBeVisible()
  await page.getByRole("button", { name: "放弃", exact: true }).click()
  // 停止同机控制面后，节点继续运行；再停止节点时只显示注册身份。
  for (const [toggle, expected] of [
    [server, fixture.state.url + " · 未连接"],
    [node, fixture.state.url],
  ] as const) {
    await toggle.click()
    await save.click()
    await confirmation.getByRole("button", { name: "保存并重启" }).click()
    await expect(toggle).not.toBeChecked()
    await expect(save).toHaveCount(0)
    await expect(nodeSection).toHaveAccessibleDescription(
      nodeAddress + " · 未连接"
    )
    await nodeSection.click()
    await expect(identity.getByText(expected, { exact: true })).toBeVisible()
    await expect(address).toBeVisible()
  }
})

async function worldShot(page: Page, name: string) {
  const directory = process.env.WORLD_UI_SHOTS
  if (directory) {
    await mkdir(directory, { recursive: true })
    await page.screenshot({
      path: path.join(directory, name + ".png"),
      animations: "disabled",
    })
  }
}

test("imports a World link and manages peer domains", async ({
  page,
  fixture,
}) => {
  await fixture.withWorldRuntime(async (world) => {
    // 在节点原有页面预置对话，独立 relay 只提供静态页面和配对。
    const seed = await page.context().newPage()
    await seed.goto(world.url + "/web/v1/#token=" + world.token)
    await startConversation(seed, "远程工作记录")
    await expect(seed.getByText("ok", { exact: true }).last()).toBeVisible()
    await seed.close()
    const violations: string[] = []
    page.on("request", (req) => {
      if (
        req.url().includes(world.token) ||
        req.headers().authorization?.includes(world.token)
      )
        violations.push(req.url())
    })
    page.on("websocket", (ws) => {
      if (ws.url().includes(world.token)) violations.push(ws.url())
    })
    const root = await page.request.get(world.relay, { maxRedirects: 0 })
    expect(root.status()).toBeGreaterThanOrEqual(300)
    expect(root.headers().location).toBe("/web/v1/")
    const deepLink = await page.request.get(
      world.relay + "/web/v1/settings/world-admin"
    )
    expect(deepLink.ok()).toBe(true)
    const asset = (await deepLink.text()).match(
      /<script[^>]+src="([^"]+)"/
    )?.[1]
    expect(asset).toBeTruthy()
    const script = await page.request.get(new URL(asset!, deepLink.url()).href)
    expect(script.ok()).toBe(true)
    expect(script.headers()["content-type"]).toMatch(/javascript/)
    const insecure = await page.context().newPage()
    await insecure.addInitScript(() =>
      Object.defineProperty(window.crypto, "subtle", { value: undefined })
    )
    await insecure.goto(world.link)
    await expect(
      insecure.getByText("请用 HTTPS 或本机地址打开这个页面")
    ).toBeVisible()
    await expect(insecure.getByLabel("Token", { exact: true })).toHaveCount(0)
    await insecure.close()
    await page.goto(world.relay + "/web/v1/")
    await page.getByRole("link", { name: "域管理" }).click()
    await expect(page).toHaveURL(world.relay + "/web/v1/world/admin")
    const webOrigin = new URL(world.link).origin
    await page.goto(webOrigin)
    await expect(page).toHaveURL(webOrigin + "/web/v1/")
    await expect(
      page.getByRole("heading", { name: "我的服务器" })
    ).toBeVisible()
    await expect(page.getByLabel("Token", { exact: true })).toHaveCount(0)
    await expect(page.getByRole("link", { name: "域管理" })).toHaveCount(0)
    await expect(page.getByLabel("粘贴远程访问链接")).toBeVisible()
    await page.goto(world.link)
    await expect(page).toHaveURL(/\/s\/[^/#]+$/)
    const runtimePath = page.url()
    await expect(
      page.getByText("远程工作记录", { exact: true }).first()
    ).toBeVisible()
    await page.getByText("远程工作记录", { exact: true }).first().click()
    await expect(page.getByText("ok", { exact: true }).last()).toBeVisible()
    await worldShot(page, "web-remote-conversations")
    await page.reload()
    await expect(
      page.getByText("远程工作记录", { exact: true }).first()
    ).toBeVisible()
    expect(
      await page.evaluate(() =>
        JSON.parse(localStorage.getItem("openvaala.servers")!)
      )
    ).toContainEqual(
      expect.objectContaining({
        url: world.link.split("&token=")[0],
        token: world.token,
      })
    )
    const switcher = page.getByRole("button", { name: /studio · 已连接/ })
    await switcher.click()
    await page.getByRole("menuitem", { name: "管理服务器…" }).click()
    const manager = page.getByRole("dialog")
    await manager.getByRole("button", { name: "添加服务器" }).click()
    await manager.getByLabel("名称").fill("测试服务器")
    await manager.getByLabel("地址", { exact: true }).fill(fixture.state.url)
    await manager.getByLabel("Token", { exact: true }).fill(fixture.state.token)
    await manager.getByRole("button", { name: "保存并切换" }).click()
    await page.getByRole("button", { name: "测试服务器 · 已连接" }).click()
    await page.getByRole("menuitemradio", { name: "studio b@home.test" }).click()
    await expect(
      page.getByText("远程工作记录", { exact: true }).first()
    ).toBeVisible()
    await page.goto(runtimePath + "/settings/world-admin")
    const remote = page.getByRole("group", { name: "远程访问", exact: true })
    await localSection(page, "远程访问").click()
    const canvas = remote.locator("canvas")
    await expect(canvas).toBeVisible()
    const pixels = await canvas.evaluate((element) => {
      const c = element as HTMLCanvasElement
      const { width, height } = c
      return {
        width,
        height,
        pixels: Array.from(
          c.getContext("2d")!.getImageData(0, 0, width, height).data
        ),
      }
    })
    expect(
      jsQR(Uint8ClampedArray.from(pixels.pixels), pixels.width, pixels.height)
        ?.data
    ).toBe(world.link)
    const relays = page.getByRole("group", {
      name: "中继节点",
      exact: true,
    })
    await localSection(page, "作为中继节点").click()
    await localSection(page, "中继节点").click()
    await expect(relays.getByRole("listitem")).toHaveCount(2)
    await expect(
      page.getByRole("button", { name: "不对外转发", exact: true })
    ).toBeVisible()
    await page.setViewportSize({ width: 1360, height: 1400 })
    await worldShot(page, "web-local")
    await page.setViewportSize({ width: 390, height: 1550 })
    await page.emulateMedia({ colorScheme: "dark" })
    await page.reload()
    await localSection(page, "远程访问").click()
    await expect(canvas).toBeVisible()
    await expect(page.locator("html")).toHaveClass(/dark/)
    await worldShot(page, "web-local-390")
    await page.emulateMedia({ colorScheme: "light" })
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth)
    ).toBe(390)
    await page.setViewportSize({ width: 1360, height: 960 })
    const serverSwitch = page.getByRole("switch", { name: "在本机托管控制面" })
    await serverSwitch.click()
    await localSection(page, "作为中继节点").click()
    await expect(
      page.getByRole("button", { name: "只给本域", exact: true })
    ).toBeDisabled()
    await expect(
      page.getByRole("button", { name: "公开给所有人", exact: true })
    ).toBeEnabled()
    await page.getByRole("button", { name: "放弃", exact: true }).click()
    await page.getByRole("tab", { name: "域管理", exact: true }).click()
    await page.getByRole("tab", { name: "互通的域" }).click()
    await page.getByLabel("对方域的名片").fill(world.card)
    await page.getByRole("button", { name: "导入", exact: true }).click()
    await expect(page.getByRole("row").getByText("friend.test")).toBeVisible()
    await worldShot(page, "web-peer-domains")
    await page.reload()
    await page.getByRole("tab", { name: "互通的域" }).click()
    const row = page.getByRole("row").filter({ hasText: "friend.test" })
    await rowAction(page, row, "移除")
    await page
      .getByRole("alertdialog")
      .getByRole("button", { name: "移除" })
      .click()
    await expect(row).toHaveCount(0)
    expect(violations).toEqual([])
    await world.stopWebPage()
    const fresh = await page.context().newPage()
    await expect(fresh.goto(world.link)).rejects.toThrow(
      /ERR_CONNECTION_REFUSED/
    )
    await fresh.close()
  })
})

test("a World link with one entry opens remote conversations through a relay instance", async ({
  page,
  fixture,
}) => {
  await fixture.withWorldRuntime(async (world) => {
    const entry = world.relay.replace("http:", "ws:")
    const target = JSON.parse(
      Buffer.from(
        new URLSearchParams(new URL(world.link).hash.slice(1)).get("world")!,
        "base64url"
      ).toString()
    )
    expect(target.entries.direct_urls ?? []).toEqual([])
    expect(target.entries.relays).toEqual([
      world.url.replace("http:", "ws:"),
      entry,
    ])
    // 只分享节点公布的公开入口，验证浏览器无需本域控制面或具体实例地址。
    const link = new URL(world.link)
    target.entries.relays = [entry]
    link.hash =
      "world=" + Buffer.from(JSON.stringify(target)).toString("base64url")
    const { instances } = await (
      await page.request.get(world.relay + "/world/v1/relay/instances")
    ).json()
    expect(instances).toHaveLength(3)
    const seed = await page.context().newPage()
    await seed.goto(world.url + "/web/v1/#token=" + world.token)
    await startConversation(seed, "多实例远程对话")
    await expect(seed.getByText("ok", { exact: true }).last()).toBeVisible()
    await seed.close()
    const routes: string[] = []
    page.on("websocket", (ws) => routes.push(ws.url()))
    await page.goto(link.href)
    await page.locator("#token").fill(world.token)
    await page.locator("#token").press("Enter")
    await expect(
      page.getByText("多实例远程对话", { exact: true }).first()
    ).toBeVisible()
    const dial = routes.find(
      (route) => new URL(route).pathname === "/world/v1/relay/dial"
    )
    expect(dial).toBeDefined()
    expect(instances).toContain(new URL(dial!).origin)
  }, { multiInstance: true })
})

test("world link reconnects with token and terminal cursor", async ({
  page,
  fixture,
}) => {
  await fixture.withWorldRuntime(async (world) => {
    const leaks: string[] = []
    let relaySocket: WebSocketRoute | undefined
    await page.routeWebSocket(/\/world\/v1\/relay\/dial/, (socket) => {
      if (socket.url().includes(world.token)) leaks.push("socket URL")
      if (socket.url().startsWith(world.relay.replace("http", "ws")))
        relaySocket = socket
      socket.connectToServer()
    })
    page.on("request", (req) => {
      if (
        req.url().includes(world.token) ||
        req.headers().authorization?.includes(world.token)
      )
        leaks.push("HTTP")
    })
    // 在测试页观察加解密的消息类型和终端位置，relay 仍使用真实的配对连接。
    await page.addInitScript((token) => {
      const audit = { frames: [] as string[], cursor: 0, attach: -1 }
      Object.assign(window, { worldAudit: audit })
      const api = crypto.subtle,
        encrypt = api.encrypt.bind(api),
        decrypt = api.decrypt.bind(api)
      api.encrypt = async function (...args) {
        const msg = JSON.parse(new TextDecoder().decode(args[2]))
        if (msg.token === token) audit.frames.push("token")
        else if (msg.method === "initialize") audit.frames.push("initialize")
        if (msg.method === "terminal/attach") audit.attach = msg.params.cursor
        return encrypt(...args)
      }
      api.decrypt = async function (...args) {
        const data = await decrypt(...args)
        const msg = JSON.parse(new TextDecoder().decode(data))
        if (msg.ok === true) audit.frames.push("ok")
        if (msg.method === "terminal/output")
          audit.cursor = msg.params.next_cursor
        return data
      }
    }, world.token)
    const audit = () =>
      page.evaluate(
        () =>
          Reflect.get(window, "worldAudit") as {
            frames: string[]
            cursor: number
            attach: number
          }
      )
    await page.goto(world.link)
    await startConversation(page, "World 终端")
    await page
      .locator(".flexlayout__tabset")
      .first()
      .getByRole("button", { name: "区域操作" })
      .click()
    await page.getByRole("menuitem", { name: "新终端", exact: true }).click()
    const input = page.locator(".xterm-helper-textarea:visible")
    await expect(input).toBeFocused()
    await page.keyboard.type("printf 'entry-%d\\n' 1")
    await page.keyboard.press("Enter")
    const screen = page.locator(".xterm-rows:visible")
    await expect(screen).toContainText("entry-1")
    const before = await audit()
    expect(before.frames).toEqual(["token", "ok", "initialize"])
    await relaySocket!.close()
    await expect.poll(async () => (await audit()).attach).toBe(before.cursor)
    expect((await audit()).frames).toEqual([
      ...before.frames,
      "token",
      "ok",
      "initialize",
    ])
    await input.focus()
    await page.keyboard.type("printf 'entry-%d\\n' 2")
    await page.keyboard.press("Enter")
    await expect(screen).toContainText("entry-2")
    const lines = await screen.innerText()
    for (const n of [1, 2]) expect(lines.split("entry-" + n)).toHaveLength(2)
    expect(leaks).toEqual([])
  })
})

test("reports the World address after all routes fail", async ({
  page,
  fixture,
}) => {
  const origin = fixture.state.url
  const target = {
    node: "remote",
    domain: "partner.test",
    public_key: Buffer.alloc(32).toString("base64"),
    entries: { direct_urls: [origin], relays: [origin] },
  }
  // 直连用完整个预算，再由测试决定每一轮最后一个中继何时失败。
  const dials: WebSocketRoute[] = []
  await page.routeWebSocket(/\/world\/v1\/direct\?/, () => {})
  await page.routeWebSocket(/\/world\/v1\/relay\/dial\?/, (socket) => {
    dials.push(socket)
  })
  await page.goto(
    origin +
      "/web/v1/#world=" +
      Buffer.from(JSON.stringify(target)).toString("base64url")
  )
  await page.getByLabel("Token", { exact: true }).fill("test")
  await page.getByLabel("Token", { exact: true }).press("Enter")
  const offline = page.getByText(
    "remote@partner.test 不在线，或中继节点不可达。"
  )
  const connecting = page.getByRole("heading", {
    name: "正在连接「remote@partner.test」…",
  })
  const switchDefault = page.getByRole("button", { name: "切回默认" })
  for (const attempt of [1, 2]) {
    await expect.poll(() => dials.length).toBe(attempt)
    await expect(connecting).toBeVisible()
    await expect(offline).toBeHidden()
    await expect(switchDefault).toBeVisible()
    await dials[attempt - 1].close()
    await expect(offline).toBeVisible()
    await expect(connecting).toBeHidden()
  }
  await switchDefault.click()
  await expect(page.getByRole("heading", { name: "输入 Token" })).toBeVisible()
})

for (const access of ["standalone HTTP", "co-hosted RPC"] as const) {
  test(
    access + " previews, saves and explains access rules",
    async ({ page, fixture }) => {
      await fixture.withWorldRuntime(async (world) => {
        const standalone = access === "standalone HTTP"
        const url = standalone ? world.relay : world.url
        const domain = standalone ? "relay.test" : "home.test"
        await world.joinNode(url, "alice", "office", ["work"])
        await world.joinNode(url, "alice", "laptop", ["mobile"])
        await world.joinNode(url, "bob", "home", ["home"])
        const headers = { Authorization: "Bearer " + world.adminToken }
        const adminURL = url + "/world/v1/admin"
        const aclURL = adminURL + "/acl"
        // 用真实注册和吊销记录覆盖四种入网码状态。
        await world.joinNode(url, "studio", "workbench", ["build"], false)
        await world.joinNode(url, "studio", "retired", ["archive"])
        const allNodes: WorldAdminNode[] = await (
          await page.request.get(adminURL + "/nodes", { headers })
        ).json()
        const retired = allNodes.find((n) => n.node_name === "retired")!
        expect(
          (
            await page.request.post(
              adminURL + "/nodes/" + retired.node_id + "/revoke",
              { headers }
            )
          ).status()
        ).toBe(204)
        const tokens: WorldJoinToken[] = await (
          await page.request.get(adminURL + "/tokens", { headers })
        ).json()
        const revoked = tokens.find((v) => v.tags?.includes("archive"))!
        expect(
          (
            await page.request.delete(adminURL + "/tokens/" + revoked.id, {
              headers,
            })
          ).status()
        ).toBe(204)
        expect(
          (
            await page.request.post(adminURL + "/tokens", {
              headers,
              data: { owner: "bob", expires_at: Date.now() + 1000 },
            })
          ).status()
        ).toBe(201)
        const studio = {
          priority: 10,
          from: ["owner:studio"],
          to: ["*"],
          action: "allow",
        }
        const home = {
          priority: 20,
          from: ["tag:home"],
          to: ["tag:mobile"],
          action: "allow",
        }
        const deny = {
          priority: 30,
          from: ["owner:alice"],
          to: ["owner:bob"],
          action: "deny",
        }
        expect(
          (
            await page.request.put(aclURL, {
              headers,
              data: [studio, home, deny],
            })
          ).status()
        ).toBe(204)
        if (standalone) {
          await page.goto(url + "/web/v1/world/admin#token=" + world.adminToken)
        } else {
          await page.goto(url + "/web/v1/#token=" + world.token)
          await expect(page).toHaveURL(/\/s\/[^/#]+$/)
          await page.goto(page.url() + "/settings/world-admin?tab=admin")
        }
        const tab = (name: string) =>
          page.getByRole("tab", { name, exact: true })
        const table = (name: string) =>
          page.getByRole("table", { name, exact: true })
        const rows = (name: string) => table(name).locator("tbody > tr")
        const alice = rows("用户").filter({
          has: page.getByText("alice", { exact: true }),
        })
        const select = async (label: string, value: string) => {
          await page.getByRole("combobox", { name: label, exact: true }).click()
          await page.getByRole("option", { name: value, exact: true }).click()
        }
        async function shot(name: string, width = 1360, hover?: Locator) {
          const dir = process.env.WORLD_ADMIN_SHOTS
          if (!dir || standalone) return
          await mkdir(dir, { recursive: true })
          await page.setViewportSize({
            width,
            height: name.startsWith("acl") ? 1200 : width === 390 ? 960 : 1000,
          })
          await expect
            .poll(() =>
              page.evaluate(() => document.documentElement.scrollWidth)
            )
            .toBe(width)
          if (hover) {
            await page.mouse.move(0, 0)
            await hover.hover()
            await expect(page.getByRole("tooltip")).toHaveText(
              (await hover.getAttribute("aria-label")) ?? ""
            )
          } else await page.mouse.move(0, 0)
          await page.screenshot({
            path: path.join(dir, name + ".png"),
            animations: "disabled",
          })
          await page.setViewportSize({ width: 1360, height: 1000 })
        }
        await expect(
          page.getByRole("heading", { name: domain, exact: true })
        ).toBeVisible()
        await expect(
          alice.getByRole("button", { name: "机器 2" })
        ).toBeVisible()
        await shot("users")
        await shot("users-390", 390)
        await rowAction(page, alice, "签发入网码")
        await expect(
          page.getByRole("dialog").getByLabel("归属", { exact: true })
        ).toHaveValue("alice")
        await page.getByRole("button", { name: "取消", exact: true }).click()
        await page.getByRole("textbox", { name: "搜索用户" }).fill("ali")
        await expect(rows("用户")).toHaveCount(1)
        await expect(page).toHaveURL(/adminSearch=ali/)
        await alice.getByRole("button", { name: "机器 2" }).click()
        await expect(page).toHaveURL(/owner=alice/)
        await expect(page).toHaveURL(/online=active/)
        await expect(rows("节点")).toHaveCount(2)
        await expect(table("节点")).not.toContainText("home@")
        await page.reload()
        await expect(rows("节点")).toHaveCount(2)
        await select("全部标签", "work")
        await expect(rows("节点")).toHaveCount(1)
        await select("状态", "在线")
        await expect(page.getByText("没有匹配的机器")).toBeVisible()
        await expect(page).toHaveURL(/online=1/)
        await tab("用户").click()
        const studioRow = rows("用户").filter({
          has: page.getByText("studio", { exact: true }),
        })
        await studioRow
          .getByRole("button", { name: standalone ? "机器 1" : "机器 2" })
          .click()
        await expect(table("节点")).not.toContainText("retired@")
        await select("状态", "全部状态")
        await expect(rows("节点")).toHaveCount(standalone ? 2 : 3)
        await expect(table("节点")).toContainText("已吊销")
        if (!standalone)
          await expect(
            table("节点").getByRole("img", { name: "在线", exact: true })
          ).toBeVisible()
        await shot("nodes-filtered")
        await tab("用户").click()
        await alice.getByRole("button", { name: "入网码 2" }).click()
        await expect(page).toHaveURL(/status=available/)
        await expect(rows("入网码")).toHaveCount(2)
        await select("状态", "已用完")
        await expect(page.getByText("没有匹配的入网码")).toBeVisible()
        await page.getByRole("button", { name: "清除筛选" }).click()
        for (const status of ["可用", "已用完", "已过期", "已吊销"])
          await expect(
            table("入网码").getByText(status, { exact: true }).first()
          ).toBeVisible()
        await shot("tokens")
        await tab("互通的域").click()
        await page.getByLabel("对方域的名片").fill(world.card)
        await page.getByRole("button", { name: "导入", exact: true }).click()
        await expect(table("互通的域")).toContainText("friend.test")
        await shot("peer-domains")
        await tab("用户").click()
        await alice.getByRole("button", { name: "规则 1" }).click()
        const list = rows("访问规则").filter({
          has: page.getByRole("button", { name: /编辑规则/ }),
        })
        const defaultRow = rows("访问规则").filter({ hasText: "默认规则" })
        await expect(list.nth(2)).toHaveAttribute("data-highlighted", "true")
        await expect(page.locator("textarea")).toHaveCount(0)
        await page.getByRole("button", { name: "添加规则" }).click()
        async function chip(label: string, value: string) {
          await list.last().getByRole("combobox", { name: label }).fill(value)
          await page.getByRole("option", { name: value, exact: true }).click()
          await expect(
            list.last().getByText(value, { exact: true })
          ).toBeVisible()
        }
        await chip("发件选择器", "owner:alice")
        await chip("发件选择器", "owner:alcie")
        await expect(list.last().getByText("没有匹配的机器")).toBeVisible()
        await expect(
          list
            .last()
            .locator('[data-slot="combobox-chip"]')
            .filter({ hasText: "owner:alcie" })
        ).toHaveClass(/color-muted/)
        await chip("收件选择器", "owner:bob")
        await list.last().getByRole("button", { name: "上移" }).click()
        const cell = table("效果").getByRole("button", {
          name: "office → home：拒绝 ⇒ 允许，命中规则 #3",
          exact: true,
        })
        const changed = page.getByText("预览草稿，变化 2 格")
        await expect(changed).toBeVisible()
        await select("全部归属", "alice")
        await expect(cell).toHaveCount(0)
        await expect(changed).toBeVisible()
        await select("全部归属", "全部归属")
        await cell.click()
        await expect(list.nth(2)).toHaveAttribute("data-highlighted", "true")
        await expect(list.nth(3)).toHaveAttribute("data-highlighted", "false")
        const defaultCell = table("效果").getByRole("button", {
          name: "office → laptop：允许，命中默认规则",
          exact: true,
        })
        await defaultCell.click()
        await expect(defaultRow).toHaveAttribute("data-highlighted", "true")
        await expect(list.nth(2)).toHaveAttribute("data-highlighted", "false")
        await cell.click()
        const hoverCell = table("效果").getByRole("button", {
          name: "laptop → home：拒绝 ⇒ 允许，命中规则 #3",
          exact: true,
        })
        await shot("acl", 1360, hoverCell)
        if (process.env.WORLD_ADMIN_SHOTS && !standalone) {
          await page.emulateMedia({ colorScheme: "dark" })
          await expect(page.locator("html")).toHaveClass(/dark/)
          await shot("acl-dark", 1360, hoverCell)
          await page.emulateMedia({ colorScheme: "light" })
          await expect(page.locator("html")).not.toHaveClass(/dark/)
        }
        await shot("acl-390", 390, hoverCell)
        const save = page.getByRole("button", { name: "保存规则" })
        await save.click()
        await expect(save).toHaveCount(0)
        await page.reload()
        await expect(list).toHaveCount(4)
        const savedCell = table("效果").getByRole("button", {
          name: "office → home：允许，命中规则 #3",
          exact: true,
        })
        await expect(savedCell).toBeVisible()
        expect(
          await (await page.request.get(aclURL, { headers })).json()
        ).toEqual([
          studio,
          home,
          {
            priority: 30,
            from: ["owner:alice", "owner:alcie"],
            to: ["owner:bob"],
            action: "allow",
          },
          { ...deny, priority: 40 },
        ])
        await list.first().getByText("owner:studio", { exact: true }).click()
        await select("动作", "拒绝")
        await list
          .first()
          .getByRole("combobox", { name: "收件选择器" })
          .press("Escape")
        await expect(list.first().getByRole("combobox")).toHaveCount(0)
        await expect(list.first()).toContainText("拒绝")
        await page.getByRole("button", { name: "放弃", exact: true }).click()
        await list.first().getByRole("button", { name: "编辑规则 #1" }).click()
        await page.getByRole("textbox", { name: "搜索动作或选择器" }).click()
        await expect(list.first().getByRole("combobox")).toHaveCount(0)
        await rowAction(page, list.nth(2), "删除规则")
        await page
          .getByRole("alertdialog")
          .getByRole("button", { name: "删除规则" })
          .click()
        await expect(list).toHaveCount(3)
        await page.getByRole("button", { name: "放弃", exact: true }).click()
        await expect(savedCell).toBeVisible()
        // 切换页签后仍沿用保存请求，已提交的响应保留后续编辑。
        if (standalone) {
          let release!: () => void
          const waiting = new Promise<void>((resolve) => {
            release = resolve
          })
          await page.route(aclURL, async (route) => {
            if (route.request().method() !== "PUT") return route.continue()
            const response = await route.fetch()
            await waiting
            await route.fulfill({ response })
          })
          await list.nth(3).getByRole("button", { name: "上移" }).click()
          await save.click()
          try {
            await tab("节点").click()
            await tab("访问规则").click()
            await expect(save).toBeDisabled()
            await list.first().getByRole("button", { name: "下移" }).click()
          } finally {
            release()
          }
          await expect(save).toBeEnabled()
          await expect(list.first()).toContainText("tag:home")
          await page.getByRole("button", { name: "放弃", exact: true }).click()
          await expect(list.first()).toContainText("owner:studio")
        }
      })
    }
  )
}

test("remote access enables the default public relay only in the settings draft", async ({
  page,
  fixture,
}) => {
  await fixture.openApp(page)
  await page.goto(page.url() + "/settings/world-admin")
  const section = page.getByRole("group", { name: "远程访问", exact: true })
  const toggle = section.getByRole("switch")
  await expect(toggle).not.toBeChecked()
  const summary = section.getByText("未开启", { exact: true })
  const viewport = page.viewportSize()!
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: viewport.height })
    const summaryRight = await summary.evaluate(
      (el) => el.getBoundingClientRect().right
    )
    const switchLeft = await toggle.evaluate(
      (el) => el.getBoundingClientRect().left
    )
    expect(switchLeft - summaryRight).toBeCloseTo(8, 0)
  }
  await page.setViewportSize(viewport)
  await captureWorldReview(page, "remote-access-off", section)
  await captureWorldReview(page, "settings-off", section, "REMOTE_ACCESS_SHOTS")
  await toggle.click()
  const relays = page.getByRole("group", { name: "中继节点", exact: true })
  await localSection(page, "中继节点").click()
  await expect(relays.getByRole("listitem")).toHaveText("https://open.vaa.la")
  await captureWorldReview(page, "settings-on", section, "REMOTE_ACCESS_SHOTS")
  await page.getByRole("button", { name: "放弃", exact: true }).click()
  await page.reload()
  await expect(toggle).not.toBeChecked()
  await localSection(page, "中继节点").click()
  await expect(relays.getByRole("listitem")).toHaveCount(0)
})

test("relay onboarding saves one server, reconnects from home and replaces a revoked token", async ({
  page,
  fixture,
}) => {
  await fixture.withWorldRuntime(async (world) => {
    const home = new URL(world.link).origin
    const heading = page.getByRole("heading", { name: "我的服务器" })
    // 反向代理可以把缺失的 Runtime 路由返回成其他状态。
    await page.route(home + "/api/v1/healthz", (route) =>
      route.fulfill({ status: 502 })
    )
    await page.goto(home)
    await expect(heading).toBeVisible()
    await expect(page.getByLabel("粘贴远程访问链接")).toBeVisible()
    await expect(page.getByRole("button", { name: "添加服务器" })).toHaveCount(
      0
    )
    await captureWorldReview(page, "relay-empty", undefined, "REMOTE_ACCESS_SHOTS")
    await page.getByRole("button", { name: "使用地址连接" }).click()
    const add = page.getByRole("dialog", { name: "添加服务器" })
    const address = add.getByLabel("地址", { exact: true })
    const token = add.getByLabel("Token", { exact: true })
    await expect(token).toBeHidden()
    await address.fill("ws://localhost:8790")
    await expect(token).toBeVisible()
    await expect(add.getByText(/127.0.0.1 指的是你现在这台设备/)).toBeVisible()
    await token.fill(world.token)
    await captureWorldReview(page, "add-server-address", add, "REMOTE_ACCESS_SHOTS")
    await add.getByRole("button", { name: "返回" }).click()
    await page
      .getByRole("dialog", { name: "管理服务器" })
      .getByRole("button", { name: "添加服务器" })
      .click()
    await expect(address).toBeVisible()
    await expect(add.getByLabel("Token", { exact: true })).toHaveCount(0)
    await captureWorldReview(page, "manager-add", undefined, "REMOTE_ACCESS_SHOTS")
    await address.fill(world.link)
    await expect(token).toBeHidden()
    await expect(add.getByLabel("名称")).toHaveValue("studio")
    await expect(address).toHaveValue(world.link.split("&token=")[0])
    await address.press("Home")
    await captureWorldReview(page, "add-server-link", add, "REMOTE_ACCESS_SHOTS")
    await add.getByRole("button", { name: "保存", exact: true }).click()
    const manager = page.getByRole("dialog", { name: "管理服务器" })
    await manager.getByRole("button", { name: "编辑", exact: true }).click()
    const edit = page.getByRole("dialog", { name: "编辑服务器" })
    await expect(edit.getByLabel("Token", { exact: true })).toBeHidden()
    await edit.getByLabel("名称").fill("我的工作站")
    await edit.getByLabel("地址", { exact: true }).fill(world.link)
    await expect(edit.getByLabel("名称")).toHaveValue("我的工作站")
    await edit.getByRole("button", { name: "返回", exact: true }).click()
    await manager.getByRole("button", { name: "关闭", exact: true }).click()
    await page.getByRole("button", { name: /studio.*home.test/ }).click()
    const connected = page.getByRole("button", { name: "studio · 已连接" })
    await expect(connected).toBeVisible()
    await expect(page).toHaveURL(/\/s\/[^/#]+$/)
    await connected.click()
    await expect(page.getByRole("menuitemradio")).toHaveCount(1)
    await expect(
      page.getByRole("menuitemradio", { name: "studio b@home.test" })
    ).toBeChecked()
    await expect(page.getByRole("menuitem", { name: "管理服务器…" })).toBeVisible()
    await page.keyboard.press("Escape")
    await page.goto(home)
    const studio = page.getByRole("button", { name: "studio b@home.test" })
    await expect(studio).toBeVisible()
    await captureWorldReview(page, "relay-saved", undefined, "REMOTE_ACCESS_SHOTS")
    await studio.click()
    await expect(connected).toBeVisible()

    const owner = await page.context().newPage()
    await owner.goto(world.url + "/web/v1/#token=" + world.token)
    await expect(owner).toHaveURL(/\/s\/[^/#]+$/)
    await owner.goto(owner.url() + "/settings/world-admin")
    await localSection(owner, "远程访问").click()
    const section = owner.getByRole("group", { name: "远程访问", exact: true })
    await expect(section.locator("canvas")).toBeVisible()
    await expect(section.getByText(world.link, { exact: true })).toBeVisible()
    await captureWorldReview(owner, "settings-connected", section, "REMOTE_ACCESS_SHOTS")
    await section.getByRole("button", { name: "重置 Token" }).click()
    await owner
      .getByRole("alertdialog")
      .getByRole("button", { name: "重置 Token" })
      .click()
    await expect(page.getByText(/这台机器的 Token 已更换/)).toBeVisible()
    const expired = page.getByRole("heading", {
      name: "Token 已失效",
      exact: true,
    })
    await expect(expired).toBeVisible()
    await expect(page.getByRole("button", { name: "切回默认" })).toHaveCount(0)
    await expect(expired).toHaveCSS(
      "font-family",
      await page
        .locator("body")
        .evaluate((body) => getComputedStyle(body).fontFamily)
    )
    await captureWorldReview(page, "unauthorized", undefined, "REMOTE_ACCESS_SHOTS")
    // 本机重连后重新取状态，远端只使用新链接里的 fragment。
    if (
      (await localSection(owner, "远程访问").getAttribute("aria-expanded")) ===
      "false"
    )
      await localSection(owner, "远程访问").click()
    const link = section.locator("code").filter({ hasText: "#world=" })
    await expect(link).toBeVisible()
    await expect(link).not.toHaveText(world.link)
    const fragment = new URL((await link.textContent())!).hash
    await page
      .getByLabel("粘贴远程访问链接")
      .fill("https://unused.invalid/" + fragment)
    await page.getByRole("button", { name: "连接", exact: true }).click()
    await expect(connected).toBeVisible()
    await page.goto(home)
    await expect(studio).toHaveCount(1)
    expect(
      await page.evaluate(
        () => JSON.parse(localStorage.getItem("openvaala.servers")!).length
      )
    ).toBe(1)
    await owner.close()
  })
})

test("reviews World domain invitations and explains a conflicting card", async ({
  page,
  fixture,
}) => {
  const url = fixture.state.url
  await mockAdmin(page, "/world/v1/admin", adminToken)
  const domains = [
    { domain: "studio", status: "mutual" },
    { domain: "office", status: "pending_inbound" },
    { domain: "lab", status: "pending_outbound" },
    { domain: "archive", status: "rejected" },
  ]
  await page.route("**/world/v1/admin/domains**", async (route) => {
    const path = new URL(route.request().url()).pathname
    if (route.request().method() === "GET")
      return route.fulfill({ json: { domain: "home", self_card: "", domains } })
    if (path.endsWith("/domains"))
      return route.fulfill({
        status: 409,
        json: {
          code: "world_domain_name_taken",
          error: "domain is already bound to a different public key",
        },
      })
    const [, domain, action] = /domains\/([^/]+)\/(accept|reject)$/.exec(path)!
    domains.find((d) => d.domain === domain)!.status =
      action === "accept" ? "mutual" : "rejected"
    await route.fulfill({ status: 204 })
  })
  await page.goto(url + "/web/v1/world/admin#token=" + adminToken)
  await page.getByRole("tab", { name: "互通的域" }).click()
  const table = page.getByRole("table", { name: "互通的域" })
  await expect(table.getByText("studio", { exact: true })).toBeVisible()
  domains.push({ domain: "garden", status: "pending_inbound" })
  await expect(table.getByText("garden", { exact: true })).toBeVisible({
    timeout: 20_000,
  })
  for (const label of ["等待对方接受", "待我接受", "已互通", "已拒绝"])
    await expect(table.getByText(label, { exact: true }).first()).toBeVisible()
  await captureWorldReview(page, "trust-states", table)
  for (const [domain, action, status] of [
    ["garden", "接受", "已互通"],
    ["office", "拒绝", "已拒绝"],
  ]) {
    const row = table.getByRole("row").filter({ hasText: domain })
    await row.getByRole("button", { name: action, exact: true }).click()
    await expect(row.getByText(status)).toBeVisible()
    if (action === "接受") await captureWorldReview(page, "trust-accepted", row)
  }
  await page.getByLabel("对方域的名片").fill("studio#other")
  await page.getByRole("button", { name: "导入", exact: true }).click()
  const error = page.getByText(
    "已有同名的域 studio，这张名片属于另一个域，请让对方换个名字"
  )
  await expect(error).toBeVisible()
  await captureWorldReview(page, "domain-name-taken", error)
})

test("a removed World node can rejoin using a fresh invitation", async ({
  page,
  fixture,
}) => {
  await fixture.withWorldRuntime(async (world) => {
    const api = world.url + "/world/v1/admin/"
    const headers = { Authorization: "Bearer " + world.adminToken }
    const members = await page.request.get(api + "nodes", { headers })
    const node = (await members.json()).find(
      (n: { node_name: string }) => n.node_name === "b"
    )
    await page.goto(world.url + "/web/v1/#token=" + world.token)
    await page.getByRole("link", { name: "联系人", exact: true }).click()
    await page
      .getByRole("textbox", { name: "联系人地址" })
      .fill("colleague@home.test")
    await page.getByRole("button", { name: "打开联系人" }).click()
    expect(
      (
        await page.request.post(api + "nodes/" + node.node_id + "/revoke", {
          headers,
        })
      ).status()
    ).toBe(204)
    const composer = page.getByRole("textbox", { name: "消息", exact: true })
    await composer.fill("请确认收到。")
    await composer.press("Enter")
    const error = page.getByText("本机已被移出域，请使用新入网码重新加入。")
    await expect(error).toBeVisible()
    await captureWorldReview(page, "node-revoked-error", error)
    await page.getByRole("link", { name: "本机节点" }).click()
    const section = page.getByRole("group", { name: "本机节点", exact: true })
    await expect(section.getByText("已被 home.test 移出")).toBeVisible()
    const join = section.getByRole("button", { name: "重新加入" })
    await captureWorldReview(page, "node-revoked", join)
    await join.click()
    const dialog = page.getByRole("dialog", { name: "重新加入" })
    const issued = await page.request.post(api + "tokens", {
      headers,
      data: { owner: "team", expires_at: Date.now() + 60000 },
    })
    await dialog.getByLabel("入网码").fill((await issued.json()).token)
    await dialog.getByRole("button", { name: "加入并重启" }).click()
    await page
      .getByRole("alertdialog")
      .getByRole("button", { name: "保存并重启" })
      .click()
    await expect(localSection(page, "本机节点")).toHaveAccessibleDescription(
      "b@home.test · 已连接"
    )
  })
})
