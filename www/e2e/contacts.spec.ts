import type { Locator, Page } from "@playwright/test"
import type {
  ListMessagesParams,
  NodeStatus,
  SendMessageRequest,
  WorldMessageParams,
} from "../src/lib/api/protocol.gen"
import type { WorldMessage, WorldPeer } from "../src/lib/api/world"
import {
  captureWorldReview,
  expect,
  expandTurnProcess,
  getMessages,
  getMessageScroller,
  routeWorldRPC,
  startConversation,
  test,
  type WebUIFixture,
} from "./fixture"

const home = "home@alice.dev"
const work = "work@alice.dev"
const defaultHome = home + "/default"

function message(
  sequence: number,
  changes: Partial<WorldMessage> = {}
): WorldMessage {
  const id = String(sequence).padStart(4, "0")
  const created = 1_790_000_000_000 + sequence * 60_000
  const outbound = sequence % 2 === 0
  return {
    message_id: id,
    thread_id: id,
    direction: outbound ? "outbound" : "inbound",
    peer_addr: home,
    kind: "message",
    body: "往来消息 " + id,
    reply_body: "简短回复 " + id,
    status: outbound ? "replied" : "completed",
    hop_limit: 3,
    notify_conversation: false,
    created_at: created,
    updated_at: created,
    ...changes,
  }
}

async function mockWorld(page: Page, messages: WorldMessage[]) {
  const peers: WorldPeer[] = [home, work].map((address) => ({
    address,
    node_name: address.split("@")[0],
    online: true,
    targets: [{ name: "default", visibility: "domain" }],
  }))
  let releaseEarlier: (() => void) | undefined
  let holdEarlier: Promise<void> | undefined
  let canceled: string | undefined
  const lists: ListMessagesParams[] = []
  const sends: SendMessageRequest[] = []
  const publish = await routeWorldRPC(page, async (method, params) => {
    switch (method) {
      case "world/getStatus":
        return {
          enabled: true,
          admin_enabled: false,
          revoked: false,
          connected: true,
          node_addr: "office@alice.dev",
          server_url: "http://world.example",
          pending_outbox_count: 0,
        } satisfies NodeStatus
      case "world/listPeers":
        return { peers }
      case "world/listThreads":
        return { items: [] }
      case "world/subscribe":
      case "world/unsubscribe":
        return {}
      case "world/sendMessage": {
        const input = params as SendMessageRequest
        sends.push(input)
        const sent = message(198 + sends.length * 2, {
          peer_addr: input.to,
          ...(input.thread_id && { thread_id: input.thread_id }),
          body: input.message,
          reply_body: undefined,
          status: "sending",
        })
        messages.push(sent)
        return sent
      }
      case "world/listMessages": {
        const query = params as ListMessagesParams
        lists.push(query)
        const {
          peer_addr: addresses = [],
          cursor_id: cursor,
          direction,
          limit = 50,
        } = query
        if (cursor) await holdEarlier
        const matching = messages
          .filter(
            (row) =>
              (!addresses.length || addresses.includes(row.peer_addr)) &&
              (!cursor || row.message_id < cursor) &&
              (!direction || row.direction === direction)
          )
          .sort((a, b) => b.message_id.localeCompare(a.message_id))
        const items = matching.slice(0, limit)
        return {
          items,
          ...(matching.length > items.length && {
            next_cursor: { id: items.at(-1)!.message_id },
          }),
        }
      }
      case "world/cancelMessage": {
        canceled = (params as WorldMessageParams).message_id
        const row = messages.find((entry) => entry.message_id === canceled)!
        row.status = "cancelled"
        return row
      }
      default:
        throw new Error("unexpected World method " + method)
    }
  })
  return {
    lists,
    sends,
    publish,
    pauseEarlier() {
      holdEarlier = new Promise<void>((resolve) => {
        releaseEarlier = resolve
      })
    },
    releaseEarlier() {
      releaseEarlier?.()
    },
    canceled: () => canceled,
  }
}

async function openContacts(page: Page, fixture: WebUIFixture) {
  await fixture.openApp(page)
  await page.getByRole("link", { name: "联系人", exact: true }).click()
}

const row = (list: Locator, id: string) =>
  list.locator(`[data-row-key="${id}"]`)
const bubble = (list: Locator, key: string) =>
  list.locator(`[data-anchor-key="${key}"]`)
const bottomDistance = (list: Locator) =>
  list.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop)
const offset = (anchor: Locator) =>
  anchor.evaluate(
    (el) =>
      el.getBoundingClientRect().top -
      el.closest('[data-slot="message-scroller"]')!.getBoundingClientRect().top
  )

test("contacts prepends at the reading anchor and only local send restores follow", async ({
  page,
  fixture,
}) => {
  const messages = Array.from({ length: 40 }, (_, i) => message(i + 1))
  const world = await mockWorld(page, messages)
  await openContacts(page, fixture)
  const list = getMessageScroller(page)
  await expect(row(list, "0040")).toBeInViewport()
  await expect.poll(() => bottomDistance(list)).toBeLessThanOrEqual(2)
  await expect(row(list, "0010")).toHaveCount(0)

  world.pauseEarlier()
  await list.evaluate((el) => {
    el.scrollTop = 0
  })
  await expect
    .poll(() => world.lists.some((req) => req.cursor_id === "0011"))
    .toBe(true)
  const anchor = bubble(list, "0011:body")
  await expect(anchor).toBeInViewport()
  const before = await offset(anchor)
  world.releaseEarlier()
  await expect(row(list, "0010")).toBeAttached()
  await expect
    .poll(async () => Math.abs((await offset(anchor)) - before))
    .toBeLessThanOrEqual(2)

  const incoming = message(41, { conversation_id: "nested-notification" })
  messages.push(incoming)
  world.publish({ event: "message_updated", data: incoming })
  await expect(row(list, "0041")).toBeAttached()
  await expect
    .poll(async () => Math.abs((await offset(anchor)) - before))
    .toBeLessThanOrEqual(2)

  const composer = page.getByRole("textbox", { name: "消息", exact: true })
  const topic = page.getByRole("button", { name: "新话题", exact: true })
  await expect(topic).toHaveAttribute("aria-pressed", "false")
  await expect(
    page.getByRole("button", { name: "当前话题", exact: true })
  ).toHaveCount(0)
  await topic.click()
  await expect(topic).toHaveAttribute("aria-pressed", "true")
  await composer.hover()
  const colors = await page.evaluate(() => {
    const theme = getComputedStyle(document.body)
    return { foreground: theme.color, background: theme.backgroundColor }
  })
  await expect(topic).toHaveCSS("background-color", colors.foreground)
  await expect(topic).toHaveCSS("color", colors.background)

  await composer.fill("我主动发送的消息")
  await composer.press("Enter")
  await expect.poll(() => world.sends.length).toBe(1)
  expect(world.sends[0]).not.toHaveProperty("thread_id")
  await expect(row(list, "0200")).toBeInViewport()
  await expect(topic).toHaveAttribute("aria-pressed", "false")
  await expect.poll(() => bottomDistance(list)).toBeLessThanOrEqual(2)
  await expect(composer).toBeFocused()
})

test("switching contacts and the explicit default resets position and expansion", async ({
  page,
  fixture,
}) => {
  const long = "同一条较长的历史消息 ".repeat(65).trim()
  const all = [
    ...Array.from({ length: 24 }, (_, i) =>
      message(i + 1, { peer_addr: defaultHome })
    ),
    ...Array.from({ length: 24 }, (_, i) =>
      message(i + 101, { peer_addr: work })
    ),
  ]
  all[23].body = long
  const world = await mockWorld(page, all)
  await openContacts(page, fixture)
  const list = getMessageScroller(page)
  const expand = bubble(list, "0024:body").getByRole("button", {
    name: "展开消息",
  })
  await expand.click()
  await expect(
    bubble(list, "0024:body").getByRole("button", { name: "收起消息" })
  ).toBeVisible()
  await page.getByRole("button", { name: /work@alice.dev/ }).click()
  await expect(row(list, "0124")).toBeInViewport()
  await expect.poll(() => bottomDistance(list)).toBeLessThanOrEqual(2)
  await page.getByRole("button", { name: /home@alice.dev/ }).click()
  await expect(expand).toBeInViewport()
  await expect.poll(() => bottomDistance(list)).toBeLessThanOrEqual(2)

  await expand.click()
  // 仅修改当前路由的 search，覆盖包含旧行 ID 的精确目标筛选。
  await page.evaluate((address) => {
    const url = new URL(location.href)
    url.searchParams.set("peer", address)
    history.pushState(history.state, "", url)
    window.dispatchEvent(
      new PopStateEvent("popstate", { state: history.state })
    )
  }, defaultHome)
  await expect
    .poll(() =>
      world.lists.some((req) => req.peer_addr?.join() === defaultHome)
    )
    .toBe(true)
  await expect(expand).toBeInViewport()
  await expect.poll(() => bottomDistance(list)).toBeLessThanOrEqual(2)
})

test("short contacts history sits above the composer and log cards link once", async ({
  page,
  fixture,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await mockWorld(page, [
    message(1, { conversation_id: "inbound-conversation" }),
    message(2, { conversation_id: "outbound-conversation" }),
  ])
  await openContacts(page, fixture)

  const list = getMessageScroller(page)
  const latest = row(list, "0002")
  await expect(latest).toBeInViewport()
  await expect
    .poll(() => list.evaluate((el) => el.scrollHeight - el.clientHeight))
    .toBe(0)
  await expect
    .poll(async () => {
      const messageBottom = await latest.evaluate(
        (el) => el.getBoundingClientRect().bottom
      )
      const viewportBottom = await list.evaluate(
        (el) => el.getBoundingClientRect().bottom
      )
      return viewportBottom - messageBottom
    })
    .toBeLessThanOrEqual(24)

  await page.getByRole("tab", { name: "消息记录" }).click()
  for (const [body, label, conversationID] of [
    ["往来消息 0001", "打开对话", "inbound-conversation"],
    ["往来消息 0002", "来源对话", "outbound-conversation"],
  ]) {
    const card = page.getByRole("listitem").filter({ hasText: body })
    await expect(card.getByRole("link")).toHaveCount(1)
    await card.getByRole("link", { name: label, exact: true }).click()
    await expect(page).toHaveURL(new RegExp(`/c/${conversationID}$`))
    await page.goBack()
  }
})

for (const viewport of [
  { width: 1440, height: 900 },
  { width: 390, height: 844 },
]) {
  test(`contacts bubble actions and rich text at ${viewport.width}px`, async ({
    page,
    fixture,
  }) => {
    await page.setViewportSize(viewport)
    await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
    const long = "**复制原文** " + "消息正文可以自然换行 ".repeat(45).trim()
    const code = [
      "```go",
      "package main",
      "",
      "func main() {",
      '    println("hello")',
      "}",
      "```",
    ].join("\n")
    const messages = [
      message(1, { body: long, reply_body: code }),
      message(2, {
        body: long,
        reply_body: undefined,
        status: "pending_offline",
      }),
    ]
    const world = await mockWorld(page, messages)
    await fixture.openApp(page)
    await startConversation(page, "用于消息跳转")
    const conversationURL = page.url()
    messages[0].conversation_id = new URL(conversationURL).pathname.split(
      "/c/"
    )[1]
    if (viewport.width === 390)
      await page.getByRole("button", { name: "切换侧栏", exact: true }).click()
    await page.getByRole("link", { name: "联系人", exact: true }).click()
    if (viewport.width === 390) {
      await expect(getMessageScroller(page)).toHaveCount(0)
      await page.getByRole("button", { name: /home@alice.dev/ }).click()
    }

    const list = getMessageScroller(page)
    for (const key of ["0001:body", "0002:body"]) {
      const item = bubble(list, key)
      const expand = item.getByRole("button", { name: "展开消息" })
      const preview = item.locator(".message-preview-fade")
      await expect(preview).toHaveCSS("height", "96px")
      await expand.click()
      await expect
        .poll(() => preview.evaluate((el) => el.clientHeight))
        .toBeGreaterThan(96)
      await item.getByRole("button", { name: "收起消息" }).click()
      await expect(preview).toHaveCSS("height", "96px")
      await item.getByRole("button", { name: "复制消息" }).click()
      await expect
        .poll(() => page.evaluate(() => navigator.clipboard.readText()))
        .toBe(long)
    }

    const rich = bubble(list, "0001:reply")
    await expect(rich.locator("pre")).toContainText('println("hello")')
    await expect(rich.locator(".message-preview-fade")).toHaveCount(0)
    await expect
      .poll(() => list.evaluate((el) => el.scrollWidth - el.clientWidth))
      .toBe(0)

    await bubble(list, "0002:body")
      .getByRole("button", { name: "取消发送" })
      .click()
    const dialog = page.getByRole("alertdialog")
    await expect(dialog).toBeVisible()
    expect(world.canceled()).toBeUndefined()
    await dialog.getByRole("button", { name: "取消发送" }).click()
    await expect(dialog).toHaveCount(0)
    expect(world.canceled()).toBe("0002")

    const link = bubble(list, "0001:body").getByRole("link", {
      name: "打开对话",
    })
    await link.hover()
    await expect(
      page.locator('[data-slot="tooltip-content"]:visible')
    ).toHaveText("打开对话")
    await link.click()
    await expect(page).toHaveURL(conversationURL)
  })
}

test("sends to a foreign target and continues from its history", async ({
  page,
  fixture,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const world = await mockWorld(page, [])
  await openContacts(page, fixture)
  const address = "research@partner.test/review"
  await page.getByRole("textbox", { name: "联系人地址" }).fill(address)
  await page.getByRole("button", { name: "打开联系人" }).click()
  for (const [index, body] of [
    "请查看本周的研究进展。",
    "请把评审意见发到这个话题。",
  ].entries()) {
    if (index) await page.reload()
    const composer = page.getByRole("textbox", { name: "消息", exact: true })
    await composer.fill(body)
    await composer.press("Enter")
    await expect
      .poll(() => world.sends[index])
      .toMatchObject({ to: address, message: body })
    expect(world.sends[index].thread_id).toBe(index ? "0200" : undefined)
    await expect(page.getByText(body, { exact: true })).toBeVisible()
  }
})

for (const withoutModels of [false, true]) {
  test(
    withoutModels
      ? "delivered to a peer without a model"
      : "manually answers a foreign incoming message in its original thread",
    async ({ page, fixture }) => {
      const alicePage = page
      const bobPage = await page.context().newPage()
      async function openPeer(
        tab: Page,
        url: string,
        token: string,
        peer: string
      ) {
        await tab.goto(url + "/web/v1/#token=" + token)
        await tab.getByRole("link", { name: "联系人", exact: true }).click()
        await tab.getByRole("textbox", { name: "联系人地址" }).fill(peer)
        await tab.getByRole("button", { name: "打开联系人" }).click()
      }
      async function send(tab: Page, body: string) {
        const composer = tab.getByRole("textbox", { name: "消息", exact: true })
        await composer.fill(body)
        await composer.press("Enter")
      }
      await fixture.withWorldRuntime(
        async (alice) => {
          await fixture.withWorldRuntime(
            async (bob) => {
              for (const [local, peer] of [
                [alice, bob],
                [bob, alice],
              ]) {
                const path = "/world/v1/admin/domains"
                const remote = await page.request.get(peer.url + path, {
                  headers: { Authorization: "Bearer " + peer.adminToken },
                })
                const imported = await page.request.post(local.url + path, {
                  headers: { Authorization: "Bearer " + local.adminToken },
                  data: { card: (await remote.json()).self_card },
                })
                expect([200, 201]).toContain(imported.status())
              }
              await openPeer(
                alicePage,
                alice.url,
                alice.token,
                "bob@partner.test"
              )
              await openPeer(bobPage, bob.url, bob.token, "alice@home.test")
              await send(alicePage, "请确认收到。")
              await expect(
                getMessageScroller(bobPage).getByText("请确认收到。", {
                  exact: true,
                })
              ).toBeVisible()
              if (withoutModels) {
                const hint = "已送达，Agent 未运行：还没有配置模型"
                await expect(alicePage.getByText(hint)).toBeVisible()
                await expect(bobPage.getByText(hint)).toBeVisible()
                const models = bobPage.getByRole("link", {
                  name: "模型服务",
                  exact: true,
                })
                await captureWorldReview(bobPage, "delivered-no-model", models)
                await models.click()
                await expect(bobPage).toHaveURL(/settings\/providers$/)
                await fixture.replies(
                  {
                    tool: "send_world_message",
                    arguments: JSON.stringify({
                      to: "bob@partner.test",
                      message: "请核对手册。",
                      wait: true,
                    }),
                  },
                  { text: "已收到投递回执。" }
                )
                await alicePage.goto(
                  alice.url + "/web/v1/#token=" + alice.token
                )
                await startConversation(alicePage, "请发消息给 Bob 核对手册。")
                const turn = getMessages(alicePage)
                await expect(
                  turn.getByText("已收到投递回执。", { exact: true })
                ).toBeVisible()
                await expandTurnProcess(turn)
                await turn
                  .getByRole("button", { name: "展开 发送消息", exact: true })
                  .filter({ hasText: "bob@partner.test" })
                  .click()
                const receipt = turn.getByText(hint, { exact: true })
                await expect(receipt).toBeVisible()
                await expect(
                  turn.getByText("失败", { exact: true })
                ).toHaveCount(0)
                await captureWorldReview(alicePage, "tool-no-model", receipt)
                return
              }
              await expect(
                getMessageScroller(alicePage).getByText("ok", { exact: true })
              ).toBeVisible()
              await alicePage.getByRole("button", { name: "新话题" }).click()
              await send(alicePage, "第二个话题请确认。")
              await expect(
                getMessageScroller(bobPage).getByText("第二个话题请确认。", { exact: true })
              ).toBeVisible()
              await bobPage
                .locator('[data-sidebar="content"]')
                .getByRole("link", { name: "联系人", exact: true })
                .click()
              const recent = bobPage.getByRole("region", { name: "最近联系" })
              const contact = recent.getByRole("button", {
                name: "alice · home.test",
              })
              await expect(contact).toBeVisible()
              await expect(
                contact.locator("[data-slot=status-dot]")
              ).toHaveCount(0)
              await captureWorldReview(bobPage, "recent-contacts", contact)
              await contact.click()
              const heading = bobPage
                .getByText("alice · home.test", { exact: true })
                .last()
              await expect(heading).toBeVisible()
              for (const text of ["请确认收到。", "第二个话题请确认。"])
                await expect(
                  getMessageScroller(bobPage).getByText(text, { exact: true })
                ).toBeVisible()
              await captureWorldReview(bobPage, "recent-conversation", heading)
              await send(bobPage, "我也收到了。")
              await expect(
                getMessageScroller(alicePage).getByText("我也收到了。", {
                  exact: true,
                })
              ).toBeVisible()
              await openPeer(
                bobPage,
                bob.url,
                bob.token,
                "missing@partner.test"
              )
              await send(bobPage, "我有一个问题。")
              const missing = bobPage.getByText(
                "通讯录中没有这个节点，请检查地址或联系域管理员。"
              )
              await expect(missing).toBeVisible()
              await captureWorldReview(bobPage, "world_peer_not_found", missing)
              await openPeer(bobPage, bob.url, bob.token, "alice@unknown.test")
              await send(bobPage, "这个域已互通吗？")
              await bobPage.getByRole("link", { name: "互通的域" }).click()
              await expect(
                bobPage.getByRole("tab", { name: "互通的域" })
              ).toHaveAttribute("aria-selected", "true")
            },
            {
              nodeName: "bob",
              domainName: "partner.test",
              publicDefault: true,
              withoutModels,
            }
          )
        },
        { nodeName: "alice" }
      )
    }
  )
}

test("shows World delivery errors in the same thread", async ({
  page,
  fixture,
}) => {
  const cases = [
    [
      "world_target_not_public",
      "对方目标不接受外域来信，请对方在「设置 › World › 通告目标」中设为公开。",
      "请查看这份报告。",
    ],
    [
      "world_sender_not_allowed",
      "消息被所在域的访问规则拦截，请联系域管理员。",
      "可以开始评审了吗？",
    ],
  ]
  await mockWorld(
    page,
    cases.map(([code, , body], index) =>
      message(2 + index * 2, {
        status: "failed",
        error_code: code,
        error_text: code,
        body,
        reply_body: undefined,
      })
    )
  )
  await openContacts(page, fixture)
  await page.getByRole("button", { name: /home@alice.dev/ }).click()
  for (const [code, hint] of cases) {
    const notice = page.getByText(hint, { exact: true })
    await expect(notice).toBeVisible()
    await captureWorldReview(page, code, notice)
  }
})
