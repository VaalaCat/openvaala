import type { Locator, Page } from "@playwright/test"
import {
  expect,
  getComposer,
  getFirstVisibleTurn,
  getMessages,
  loadEarlierTurnsUntilFirst,
  openRightPanel,
  recordRequests,
  startConversation,
  test,
} from "./fixture"

// 标签栏里的标签按钮；右栏的页签也是 tab 角色，用 id 前缀区分
function getTabButtons(page: Page): Locator {
  return page.locator('[role="tab"][id^="flexlayout-tabbutton-"]')
}

// 分栏布局里的区域和各标签页的内容面板，按布局顺序排列
function getAreas(page: Page): Locator {
  return page.locator(".flexlayout__tabset")
}

// 右栏的页签内容也是 tabpanel 角色，用类名只取标签页的内容面板
function getTabPanels(page: Page): Locator {
  return page.locator('.flexlayout__tab[role="tabpanel"]')
}

// 打开区域标签栏右端「+」按钮的菜单
async function openAreaMenu(area: Locator): Promise<void> {
  await area.getByRole("button", { name: "区域操作", exact: true }).click()
}

// 向右分栏，新区域里是一个新终端
async function splitRight(page: Page, area: Locator): Promise<void> {
  await openAreaMenu(area)
  await page.getByRole("menuitem", { name: "向右分栏", exact: true }).click()
}

// 标签页面板里的输入框
function getPanelComposer(panel: Locator): Locator {
  return panel.getByRole("textbox", { name: "消息" })
}

// 标签页面板里的消息区
function getPanelMessages(panel: Locator): Locator {
  return panel.getByRole("region", { name: "消息" })
}

// 终端的屏幕：xterm 的 DOM 渲染把每一行画成一个元素
function getTerminalScreen(page: Page): Locator {
  return page.locator(".xterm-rows:visible")
}

// 终端结束后右下角的结束原因和「重新启动」
function getExitNotice(page: Page): Locator {
  return page.getByRole("status").filter({ hasText: "重新启动" })
}

// 在终端里输入一行命令并回车；键盘输入落在 xterm 的输入框上
async function runCommand(page: Page, command: string): Promise<void> {
  await page.keyboard.type(command)
  await page.keyboard.press("Enter")
}

// 点左侧列表里标题以 title 开头的对话
async function openFromList(page: Page, title: string): Promise<void> {
  await page.getByRole("link", { name: new RegExp("^" + title) }).click()
}

// 从左侧列表的「⋯」菜单删除一条对话并确认
async function deleteFromList(
  page: Page,
  conversationID: string
): Promise<void> {
  await page
    .getByRole("listitem")
    .filter({ has: page.locator(`a[href*="/c/${conversationID}"]`) })
    .getByRole("button", { name: /的更多操作/ })
    .click()
  await page.getByRole("menuitem", { name: "删除", exact: true }).click()
  await page
    .getByRole("alertdialog")
    .getByRole("button", { name: "删除", exact: true })
    .click()
}

// 从左侧「新对话」入口进入新对话页，发出第一条消息
async function startAnotherConversation(
  page: Page,
  text: string
): Promise<void> {
  await page.getByRole("link", { name: "新对话", exact: true }).click()
  await startConversation(page, text)
}

// 顶栏始终只有一行，高 40px
async function expectTopBarOneRow(page: Page): Promise<void> {
  const header = page.locator("header").first()
  await expect(header).toBeVisible()
  expect((await header.boundingBox())?.height).toBe(40)
}

// 一段很长的回复，消息区要滚很久
const longReply = Array.from(
  { length: 80 },
  (_, index) => `滚动位置第 ${index + 1} 段。`
).join("\n\n")

// 当前显示的消息区（隐藏的标签页里还挂着别的）
function getVisibleScroller(page: Page): Locator {
  return page.locator('[data-slot="message-scroller"]:visible')
}

async function readScroll(scroller: Locator) {
  return scroller.evaluate((node) => ({
    top: node.scrollTop,
    max: node.scrollHeight - node.clientHeight,
  }))
}

// 等这一轮收尾、贴底的滚动动画结束：消息区在最底部
async function expectAtBottom(page: Page): Promise<void> {
  const scroller = getVisibleScroller(page)
  await expect
    .poll(async () => {
      const { top, max } = await readScroll(scroller)
      return max > 400 && max - top <= 2
    })
    .toBe(true)
}

// 把消息区滚到中间，等位置稳定，返回滚到的位置
async function scrollToMiddle(page: Page): Promise<number> {
  const scroller = getVisibleScroller(page)
  await scroller.evaluate((node) => {
    node.scrollTop = (node.scrollHeight - node.clientHeight) / 2
  })
  await page.waitForTimeout(400)
  const { top, max } = await readScroll(scroller)
  expect(top).toBeGreaterThan(max * 0.3)
  expect(top).toBeLessThan(max * 0.7)
  return top
}

// 隐藏又显示后消息区回到原来的位置，之后也不再动
async function expectScrollAt(page: Page, before: number): Promise<void> {
  const scroller = getVisibleScroller(page)
  await expect
    .poll(async () => Math.abs((await readScroll(scroller)).top - before))
    .toBeLessThanOrEqual(2)
  await page.waitForTimeout(500)
  expect(
    Math.abs((await readScroll(scroller)).top - before)
  ).toBeLessThanOrEqual(2)
}

test.describe("desktop", () => {
  test.use({ viewport: { width: 1280, height: 800 } })

  test("switching_keeps_terminals_of_the_previous_conversation", async ({
    page,
    fixture,
  }) => {
    // 1. A 向右分栏开出终端并输出一行。
    await fixture.replies({ text: "回复A" }, { text: "回复B" })
    await fixture.openApp(page)
    await startConversation(page, "切换对话A")
    const conversationA = page.url()
    await splitRight(page, getAreas(page).first())
    await expect(getAreas(page)).toHaveCount(2)
    await runCommand(page, "echo kept-output")
    await expect(getTerminalScreen(page)).toContainText("kept-output")
    // 2. 列表切到 B：B 只有主对话，没有 A 的分栏和终端。
    await startAnotherConversation(page, "切换对话B")
    await expect(getAreas(page)).toHaveCount(1)
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect(getTerminalScreen(page)).toHaveCount(0)
    // 3. 切回 A：分栏、终端和那行输出都在，终端还能输入。
    await openFromList(page, "切换对话A")
    await expect(page).toHaveURL(conversationA)
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page)).toHaveCount(2)
    await expect(getTerminalScreen(page)).toContainText("kept-output")
    await page.locator(".terminal-viewport:visible").click()
    await runCommand(page, "echo still-alive")
    await expect(getTerminalScreen(page)).toContainText("still-alive")
  })

  test("sub_agent_opens_beside_the_main_tab", async ({ page, fixture }) => {
    // 1. 主 Agent 委派 helper；右栏 Agent 页签的树里点它。
    await fixture.replies({
      tool: "spawn_agent",
      arguments: JSON.stringify({
        task_name: "helper",
        message: "帮主 Agent 看一下",
      }),
    })
    await fixture.openApp(page)
    await startConversation(page, "并排看子 Agent")
    const conversationURL = page.url()
    await openRightPanel(page)
    await page.getByRole("tab", { name: "Agent", exact: true }).click()
    const helper = page
      .getByRole("tabpanel", { name: "Agent", exact: true })
      .getByRole("link", { name: /helper/ })
    await expect(helper).toContainText("空闲")
    await helper.click()
    // 2. 子 Agent 在主对话右边开成新标签页，地址是子 Agent 的；主对话仍是主 Agent，输入框还在，子 Agent 页没有输入框。
    await expect(page).toHaveURL(/\/c\/[^/]+\/sessions\/[^/]+$/)
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page)).toHaveCount(2)
    await expect(getTabButtons(page).last()).toHaveText("helper")
    await expect(getTabButtons(page).last()).toHaveAttribute(
      "aria-selected",
      "true"
    )
    const [main, subAgent] = [
      getTabPanels(page).first(),
      getTabPanels(page).last(),
    ]
    await expect(getPanelComposer(main)).toBeVisible()
    await expect(getPanelMessages(main)).toContainText("并排看子 Agent")
    await expect(getPanelMessages(main)).not.toContainText("帮主 Agent 看一下")
    await expect(getPanelComposer(subAgent)).toHaveCount(0)
    await expect(getPanelMessages(subAgent)).toContainText("帮主 Agent 看一下")
    await expect(getPanelMessages(subAgent)).not.toContainText("并排看子 Agent")
    // 3. 浏览器后退回到对话地址，选中主对话；子 Agent 标签页仍开着。
    await page.goBack()
    await expect(page).toHaveURL(conversationURL)
    await expect(getTabButtons(page).first()).toHaveAttribute(
      "aria-selected",
      "true"
    )
    await expect(getTabButtons(page)).toHaveCount(2)
  })

  test("root_session_address_opens_the_main_conversation", async ({
    page,
    fixture,
  }) => {
    // 1. 记下 serve 返回的根 Session ID，开一条对话。
    let rootSessionID = ""
    page.on("websocket", (socket) =>
      socket.on("framereceived", ({ payload }) => {
        rootSessionID =
          /"root_session_id":"([^"]+)"/.exec(String(payload))?.[1] ??
          rootSessionID
      })
    )
    await fixture.openApp(page)
    await startConversation(page, "根 Session 地址")
    const conversationURL = page.url()
    await expect.poll(() => rootSessionID).not.toBe("")
    // 2. 直接打开根 Session 的地址：地址栏回到对话地址，只有主对话一个标签页，不开子 Agent 标签页。
    await page.goto(conversationURL + "/sessions/" + rootSessionID)
    await expect(page).toHaveURL(conversationURL)
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect(getPanelComposer(getTabPanels(page).first())).toBeVisible()
  })

  test("area_menu_is_the_only_bar_button_and_maximizes_and_restores", async ({
    page,
    fixture,
  }) => {
    // 1. 区域标签栏右端只有「+」一个按钮，菜单里有新终端、分栏，没有新对话；只有一个区域时没有最大化。
    await fixture.replies({ text: "回复一" })
    await fixture.openApp(page)
    await startConversation(page, "菜单第一条")
    const first = getAreas(page).first()
    await expect(first.locator(".flexlayout__tab_toolbar button")).toHaveCount(
      1
    )
    await openAreaMenu(first)
    await expect(page.getByRole("menuitem", { name: "新终端" })).toBeVisible()
    await expect(page.getByRole("menuitem", { name: "新对话" })).toHaveCount(0)
    await expect(page.getByRole("menuitem", { name: "向下分栏" })).toBeVisible()
    await expect(
      page.getByRole("menuitem", { name: /最大化|还原/ })
    ).toHaveCount(0)
    await page.getByRole("menuitem", { name: "向下分栏", exact: true }).click()
    await expect(getAreas(page)).toHaveCount(2)
    // 2. 两个区域时能最大化；最大化后菜单变成还原，再还原两个区域都回来。
    await openAreaMenu(getAreas(page).first())
    await page
      .getByRole("menuitem", { name: "最大化此区域", exact: true })
      .click()
    await expect(getTabPanels(page).and(page.locator(":visible"))).toHaveCount(
      1
    )
    await openAreaMenu(page.locator(".flexlayout__tabset:visible"))
    await page
      .getByRole("menuitem", { name: "还原此区域", exact: true })
      .click()
    await expect(getTabPanels(page).and(page.locator(":visible"))).toHaveCount(
      2
    )
  })

  test("hidden_tab_keeps_its_scroll_position_in_the_same_area", async ({
    page,
    fixture,
  }) => {
    // 1. 一条很长的回复，等收尾后滚到中间。
    await fixture.replies({ text: longReply })
    await fixture.openApp(page)
    await startConversation(page, "滚动桌面第一条")
    await expect(
      getMessages(page).locator('[data-turn-status="completed"]')
    ).toBeVisible()
    await expectAtBottom(page)
    const before = await scrollToMiddle(page)
    // 2. 同一个区域里新开一个终端标签页（主对话被隐藏），再切回来：位置不变。
    await openAreaMenu(getAreas(page).first())
    await page.getByRole("menuitem", { name: "新终端", exact: true }).click()
    await expect(getTabButtons(page)).toHaveCount(2)
    await expect(getVisibleScroller(page)).toHaveCount(0)
    await getTabButtons(page).filter({ hasText: "滚动桌面第一条" }).click()
    await expectScrollAt(page, before)
  })

  test("hidden_tab_keeps_reading_position_in_a_long_conversation", async ({
    page,
    fixture,
  }) => {
    test.setTimeout(120_000)
    // 1. 60 轮长回复的对话加载到第 1 轮，滚到中间，记下视口顶部那一条和视口里的行。
    const turns = 60
    const historyReply =
      "## 历史回复\n\n" +
      Array.from(
        { length: 12 },
        (_, index) => `- 要点 ${index + 1}：保留历史消息、阅读位置和文字选择。`
      ).join("\n")
    await fixture.replies(
      ...Array.from({ length: turns }, () => ({ text: historyReply }))
    )
    const { longConversationID } = await fixture.seed({
      conversations: 0,
      longConversationTurns: turns,
    })
    await fixture.openApp(page)
    await page.goto(page.url() + "/c/" + longConversationID)
    const messages = getMessages(page)
    await expect(
      messages.getByText(`长对话第 ${turns} 轮`, { exact: true })
    ).toBeVisible()
    await loadEarlierTurnsUntilFirst(page)
    await scrollToMiddle(page)
    const { text } = await getFirstVisibleTurn(page)
    const message = messages.getByText(text, { exact: true })
    const top = (await message.boundingBox())!.y
    const getRowKeys = (isVisibleOnly: boolean) =>
      getVisibleScroller(page).evaluate((node, isVisibleOnly) => {
        const viewport = node.getBoundingClientRect()
        return [...node.querySelectorAll<HTMLElement>("[data-row-key]")]
          .filter((row) => {
            const rect = row.getBoundingClientRect()
            return (
              !isVisibleOnly ||
              (rect.bottom > viewport.top && rect.top < viewport.bottom)
            )
          })
          .map((row) => row.dataset.rowKey!)
      }, isVisibleOnly)
    const visibleRowKeys = await getRowKeys(true)
    expect(visibleRowKeys.length).toBeGreaterThan(0)
    // 2. 同一个区域里新开一个终端标签页（主对话被隐藏），过一会儿再切回来。
    await openAreaMenu(getAreas(page).first())
    await page.getByRole("menuitem", { name: "新终端", exact: true }).click()
    await expect(getTabButtons(page)).toHaveCount(2)
    await expect(getVisibleScroller(page)).toHaveCount(0)
    await page.waitForTimeout(500)
    await getTabButtons(page).filter({ hasText: "长对话第 1 轮" }).click()
    // 3. 同一条消息回到原位，隐藏前视口里的行仍然挂着。
    await expect
      .poll(async () => Math.abs((await message.boundingBox())!.y - top))
      .toBeLessThanOrEqual(2)
    expect(await getRowKeys(false)).toEqual(
      expect.arrayContaining(visibleRowKeys)
    )
  })

  test("hidden_tab_at_the_bottom_keeps_following_new_output", async ({
    page,
    fixture,
  }) => {
    // 1. 回复收尾后停在最底部；隐藏再显示，仍在最底部。
    await fixture.replies(
      { text: longReply },
      { text: longReply + "\n\n追加结尾。" }
    )
    await fixture.openApp(page)
    await startConversation(page, "滚动贴底第一条")
    await expect(
      getMessages(page).locator('[data-turn-status="completed"]')
    ).toBeVisible()
    await expectAtBottom(page)
    await openAreaMenu(getAreas(page).first())
    await page.getByRole("menuitem", { name: "新终端", exact: true }).click()
    await expect(getTabButtons(page)).toHaveCount(2)
    await getTabButtons(page).filter({ hasText: "滚动贴底第一条" }).click()
    await expectAtBottom(page)
    // 2. 再发一条，新的输出到达后仍然贴着底部。
    await getComposer(page).fill("再来一条")
    await getComposer(page).press("Enter")
    await expect(
      getMessages(page).locator('[data-turn-status="completed"]')
    ).toHaveCount(2)
    await expect(getMessages(page).getByText("追加结尾。")).toBeVisible()
    await expectAtBottom(page)
  })

  test("right_panel_starts_collapsed_and_remembers_the_choice", async ({
    page,
    fixture,
  }) => {
    // 1. 全新的浏览器档案里，右栏默认收起：开关未按下，页签没有渲染。
    await fixture.replies({ text: "回复一" })
    await fixture.openApp(page)
    await startConversation(page, "右栏默认收起")
    const toggle = page.getByRole("button", { name: "切换对话详情" })
    await expect(toggle).toHaveAttribute("aria-pressed", "false")
    await expect(
      page.getByRole("tab", { name: "Agent", exact: true })
    ).toHaveCount(0)
    // 2. 展开后出现页签，刷新后仍保持展开。
    await toggle.click()
    await expect(toggle).toHaveAttribute("aria-pressed", "true")
    await expect(
      page.getByRole("tab", { name: "Agent", exact: true })
    ).toBeVisible()
    await page.reload()
    await expect(toggle).toHaveAttribute("aria-pressed", "true")
    await expect(
      page.getByRole("tab", { name: "Agent", exact: true })
    ).toBeVisible()
  })

  test("main_tab_cannot_be_closed", async ({ page, fixture }) => {
    // 1. 只有主对话时，标签上没有关闭按钮。
    await fixture.replies({
      tool: "spawn_agent",
      arguments: JSON.stringify({
        task_name: "helper",
        message: "帮主 Agent 看一下",
      }),
    })
    await fixture.openApp(page)
    await startConversation(page, "主对话不能关")
    const conversationURL = page.url()
    const mainTab = getTabButtons(page).filter({ hasText: "主对话不能关" })
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect(mainTab.getByTitle("关闭标签页")).toHaveCount(0)
    // 没有关闭按钮的标签左右内边距对称，都是 12px。
    const getPadding = (tab: Locator) =>
      tab.evaluate((element) => {
        const style = getComputedStyle(element)
        return [style.paddingLeft, style.paddingRight]
      })
    expect(await getPadding(mainTab)).toEqual(["12px", "12px"])
    // 2. 再开一个子 Agent 和一个终端：它们有关闭按钮，主对话仍然没有。
    const closeButtons = getTabButtons(page).getByTitle("关闭标签页")
    await openRightPanel(page)
    await page.getByRole("tab", { name: "Agent", exact: true }).click()
    await page
      .getByRole("tabpanel", { name: "Agent", exact: true })
      .getByRole("link", { name: /helper/ })
      .click()
    await openAreaMenu(getAreas(page).first())
    await page.getByRole("menuitem", { name: "新终端", exact: true }).click()
    await expect(getTabButtons(page)).toHaveCount(3)
    await expect(closeButtons).toHaveCount(2)
    await expect(mainTab.getByTitle("关闭标签页")).toHaveCount(0)
    expect(await getPadding(mainTab)).toEqual(["12px", "12px"])
    expect(await getPadding(getTabButtons(page).last())).toEqual([
      "12px",
      "4px",
    ])
    // 3. 把另外两个关掉后只剩主对话，仍没有关闭按钮，地址和输入框不变。
    await closeButtons.last().click()
    await expect(getTabButtons(page)).toHaveCount(2)
    await closeButtons.last().click()
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect(getAreas(page)).toHaveCount(1)
    await expect(closeButtons).toHaveCount(0)
    await expect(page).toHaveURL(conversationURL)
    await expect(getComposer(page)).toBeVisible()
  })

  test("closing_the_last_tab_of_an_area_merges_it", async ({
    page,
    fixture,
  }) => {
    // 1. 分栏后，新区域里只有一个终端标签页。
    await fixture.replies({ text: "回复一" })
    await fixture.openApp(page)
    await startConversation(page, "合并第一条")
    const conversationURL = page.url()
    await splitRight(page, getAreas(page).first())
    await expect(getAreas(page)).toHaveCount(2)
    // 2. 关掉这个标签页，区域合并，选中回到主对话。
    await getAreas(page).last().getByTitle("关闭标签页").click()
    await expect(getAreas(page)).toHaveCount(1)
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect(getTabButtons(page)).toContainText("合并第一条")
    await expect(page).toHaveURL(conversationURL)
    await expect(getComposer(page)).toBeVisible()
  })

  test("new_conversation_page_is_outside_the_workspace", async ({
    page,
    fixture,
  }) => {
    // 1. 新对话页没有标签栏和分栏，只有标题和输入框。
    await fixture.replies({ text: "回复一" })
    await fixture.openApp(page)
    await expect(
      page.getByRole("heading", { name: "新对话", level: 1 })
    ).toBeVisible()
    await expect(page).toHaveURL(/\/web\/v1\/s\/[^/]+$/)
    await expect(getAreas(page)).toHaveCount(0)
    await expect(getComposer(page)).toBeVisible()
    // 2. 发出第一条后进入这条对话，工作区里只有主对话。
    await startConversation(page, "独立新对话")
    await expect(getAreas(page)).toHaveCount(1)
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect(getTabButtons(page)).toContainText("独立新对话")
    // 3. 分栏后再回新对话页，没有标签栏；浏览器后退，这条对话的分栏还在。
    const conversationURL = page.url()
    await splitRight(page, getAreas(page).first())
    await expect(getAreas(page)).toHaveCount(2)
    await page.getByRole("link", { name: "新对话", exact: true }).click()
    await expect(
      page.getByRole("heading", { name: "新对话", level: 1 })
    ).toBeVisible()
    await expect(getAreas(page)).toHaveCount(0)
    await page.goBack()
    await expect(page).toHaveURL(conversationURL)
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page)).toHaveCount(2)
  })

  test("deleting_conversations_forgets_their_workspaces", async ({
    page,
    fixture,
  }) => {
    // 1. A 开着终端后切到 B，终端留在后台。
    const requests = recordRequests(page)
    await fixture.replies({ text: "回复A" }, { text: "回复B" })
    await fixture.openApp(page)
    await startConversation(page, "删除第一条")
    const conversationA = page.url().split("/c/")[1]
    await splitRight(page, getAreas(page).first())
    await startAnotherConversation(page, "删除第二条")
    const conversationB = page.url().split("/c/")[1]
    expect(requests.some((r) => r.method === "terminal/close")).toBe(false)
    // 2. 从左侧列表删除 A：B 照常显示，A 后台的终端被关掉。
    await deleteFromList(page, conversationA)
    await expect
      .poll(() => requests.some((r) => r.method === "terminal/close"))
      .toBe(true)
    await expect(page).toHaveURL(new RegExp("/c/" + conversationB + "$"))
    await expect(getTabButtons(page)).toContainText("删除第二条")
    // 3. 删除当前打开的 B：回到新对话页，没有标签栏。
    await deleteFromList(page, conversationB)
    await expect(page).toHaveURL(/\/web\/v1\/s\/[^/]+$/)
    await expect(
      page.getByRole("heading", { name: "新对话", level: 1 })
    ).toBeVisible()
    await expect(getAreas(page)).toHaveCount(0)
  })

  test("missing_conversation_offers_the_way_back_to_new_conversation", async ({
    page,
    fixture,
  }) => {
    // 1. 打开一个不存在的对话地址：主对话标签页显示「对话不存在」，只有主对话一个标签页。
    await fixture.openApp(page)
    await page.goto(page.url() + "/c/missing-conversation")
    await expect(page.getByText("对话不存在", { exact: true })).toBeVisible()
    await expect(getTabButtons(page)).toHaveCount(1)
    // 2. 「回到新对话」回到新对话页。
    await page.getByRole("button", { name: "回到新对话", exact: true }).click()
    await expect(page).toHaveURL(/\/web\/v1\/s\/[^/]+$/)
    await expect(
      page.getByRole("heading", { name: "新对话", level: 1 })
    ).toBeVisible()
  })

  test("reload_restores_only_the_current_conversation_layout", async ({
    page,
    fixture,
  }) => {
    // 1. A 分栏出终端，B 向下分栏出终端，各有两个区域。
    const requests = recordRequests(page)
    await fixture.replies({ text: "回复A" }, { text: "回复B" })
    await fixture.openApp(page)
    await startConversation(page, "刷新对话A")
    await splitRight(page, getAreas(page).first())
    await expect(getAreas(page)).toHaveCount(2)
    await startAnotherConversation(page, "刷新对话B")
    await openAreaMenu(getAreas(page).first())
    await page.getByRole("menuitem", { name: "向下分栏", exact: true }).click()
    await expect(getAreas(page)).toHaveCount(2)
    const conversationB = page.url().split("/c/")[1]
    // 2. 刷新后地址栏这条（B）的分栏和标签页都在，终端接回原来的 shell，只订阅了 B。
    const before = requests.length
    await page.reload()
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page)).toHaveCount(2)
    await expect(getTabButtons(page).first()).toContainText("刷新对话B")
    await expect(getTabPanels(page).first()).toContainText("回复B")
    await expect(getExitNotice(page)).toHaveCount(0)
    await page.locator(".terminal-viewport:visible").click()
    await runCommand(page, "echo reattached-$((6*7))")
    await expect(getTerminalScreen(page)).toContainText("reattached-42")
    const subscribed = requests
      .slice(before)
      .filter((r) => r.method === "conversation/subscribe")
    expect(subscribed.length).toBeGreaterThan(0)
    expect(subscribed.every((r) => r.conversationID === conversationB)).toBe(
      true
    )
    // 3. 切到 A：保存的布局恢复，两个区域、主对话和接回的终端。
    await openFromList(page, "刷新对话A")
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page)).toHaveCount(2)
    await expect(getTabButtons(page).first()).toContainText("刷新对话A")
    await expect(getTabPanels(page).first()).toContainText("回复A")
    await expect(getExitNotice(page)).toHaveCount(0)
    await page.locator(".terminal-viewport:visible").click()
    await runCommand(page, "echo reattached-$((1+1))")
    await expect(getTerminalScreen(page)).toContainText("reattached-2")
  })

  test("splitter_is_reachable_above_the_composer", async ({
    page,
    fixture,
  }) => {
    // 1. 在对话下方分栏，取分割线的中心。
    await fixture.replies({ text: "回复一" })
    await fixture.openApp(page)
    await startConversation(page, "分割线第一条")
    await openAreaMenu(getAreas(page).first())
    await page.getByRole("menuitem", { name: "向下分栏", exact: true }).click()
    await expect(getAreas(page)).toHaveCount(2)
    const splitter = page.locator(".flexlayout__splitter")
    const box = (await splitter.boundingBox())!
    const x = box.x + box.width / 2
    const y = box.y + box.height / 2
    // 2. 中心上方 1 到 3px 取到的都是分割线，不是上面对话贴底的输入框。
    for (const offset of [1, 2, 3]) {
      const hit = await page.evaluate(
        ({ x, y }) =>
          document.elementFromPoint(x, y)?.closest(".flexlayout__splitter") !=
          null,
        { x, y: y - offset }
      )
      expect(hit, `分割线上方 ${offset}px`).toBe(true)
    }
    // 3. 在上方 2px 处按下向下拖 40px，上面的区域变高、下面的变矮。
    const [topBefore, bottomBefore] = await Promise.all([
      getAreas(page).first().boundingBox(),
      getAreas(page).last().boundingBox(),
    ])
    await page.mouse.move(x, y - 2)
    await page.mouse.down()
    await page.mouse.move(x, y - 2 + 40, { steps: 8 })
    await page.mouse.up()
    await expect
      .poll(async () => (await getAreas(page).first().boundingBox())?.height)
      .toBeGreaterThan(topBefore!.height + 30)
    expect((await getAreas(page).last().boundingBox())!.height).toBeLessThan(
      bottomBefore!.height - 30
    )
  })

  test("message_area_keeps_overscroll_inside", async ({ page, fixture }) => {
    // 消息区滚到头不把滚动交给外层；flexlayout 包在标签页外面的那层不是滚动容器。
    await fixture.replies({ text: "回复一" })
    await fixture.openApp(page)
    await startConversation(page, "回弹第一条")
    await expect(getVisibleScroller(page)).toHaveCSS(
      "overscroll-behavior-y",
      "contain"
    )
    await expect(
      getTabPanels(page).first().locator(".flexlayout__tab_moveable")
    ).toHaveCSS("overflow", "hidden")
  })
})

test.describe("narrow", () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test("hidden_tab_keeps_its_scroll_position_in_the_drawer_flow", async ({
    page,
    fixture,
  }) => {
    // 1. 一条很长的回复，等收尾后滚到中间。
    await fixture.replies({ text: longReply })
    await fixture.openApp(page)
    await startConversation(page, "滚动窄屏第一条")
    await expect(
      getMessages(page).locator('[data-turn-status="completed"]')
    ).toBeVisible()
    await expectAtBottom(page)
    const before = await scrollToMiddle(page)
    // 2. 从底部抽屉新开一个终端（主对话被隐藏），再从抽屉切回来：位置不变。
    const tabsButton = page.getByRole("button", {
      name: "标签页列表",
      exact: true,
    })
    await tabsButton.click()
    const drawer = page.getByRole("dialog", { name: "标签页", exact: true })
    await drawer.getByRole("button", { name: "新终端", exact: true }).click()
    await expect(drawer).toBeHidden()
    await expect(tabsButton).toHaveText("2")
    await tabsButton.click()
    await drawer.getByRole("button", { name: /^滚动窄屏第一条/ }).click()
    await expect(drawer).toBeHidden()
    await expectScrollAt(page, before)
  })

  test("main_tab_cannot_be_closed", async ({ page, fixture }) => {
    // 1. 抽屉里只有主对话时没有关闭按钮；新开终端后，只有终端那一行有。
    await fixture.replies({ text: "回复一" })
    await fixture.openApp(page)
    await startConversation(page, "窄屏主对话")
    const tabsButton = page.getByRole("button", {
      name: "标签页列表",
      exact: true,
    })
    await tabsButton.click()
    const drawer = page.getByRole("dialog", { name: "标签页", exact: true })
    const closeButtons = drawer.getByRole("button", { name: /^关闭标签页/ })
    await expect(drawer.getByRole("listitem")).toHaveCount(1)
    await expect(closeButtons).toHaveCount(0)
    await drawer.getByRole("button", { name: "新终端", exact: true }).click()
    await expect(drawer).toBeHidden()
    await expect(tabsButton).toHaveText("2")
    await tabsButton.click()
    await expect(drawer.getByRole("listitem")).toHaveCount(2)
    await expect(closeButtons).toHaveCount(1)
    await expect(
      drawer
        .getByRole("listitem")
        .filter({ hasText: "窄屏主对话" })
        .getByRole("button", { name: /^关闭标签页/ })
    ).toHaveCount(0)
    // 2. 关掉终端后只剩主对话，仍没有关闭按钮。
    await closeButtons.click()
    await expect(drawer.getByRole("listitem")).toHaveCount(1)
    await expect(closeButtons).toHaveCount(0)
  })

  test("narrow_drawer_lists_only_the_tabs_of_the_current_conversation", async ({
    page,
    fixture,
  }) => {
    // 1. 窄屏不画标签栏，顶栏仍是一行，右侧有标签页个数；抽屉底部只有「新终端」。
    await fixture.replies({ text: "回复一" }, { text: "回复二" })
    await fixture.openApp(page)
    await startConversation(page, "抽屉对话甲")
    await expect(
      page.locator(".flexlayout__tabset_tabbar_outer:visible")
    ).toHaveCount(0)
    await expectTopBarOneRow(page)
    const tabsButton = page.getByRole("button", {
      name: "标签页列表",
      exact: true,
    })
    await expect(tabsButton).toHaveText("1")
    await tabsButton.click()
    const drawer = page.getByRole("dialog", { name: "标签页", exact: true })
    await expect(
      drawer.getByRole("button", { name: /^抽屉对话甲/ })
    ).toBeVisible()
    await expect(drawer.getByRole("button", { name: "新对话" })).toHaveCount(0)
    // 2. 抽屉里新开一个终端，列表出现两项，新的打勾；地址不变。
    const conversationURL = page.url()
    await drawer.getByRole("button", { name: "新终端", exact: true }).click()
    await expect(drawer).toBeHidden()
    await expect(tabsButton).toHaveText("2")
    await expect(page).toHaveURL(conversationURL)
    await expectTopBarOneRow(page)
    // 3. 从抽屉切回主对话。
    await tabsButton.click()
    await expect(drawer.getByRole("listitem")).toHaveCount(2)
    await drawer.getByRole("button", { name: /^抽屉对话甲/ }).click()
    await expect(drawer).toBeHidden()
    await expect(page.getByRole("region", { name: "消息" })).toContainText(
      "回复一"
    )
    // 4. 在左栏切到另一条对话：抽屉里只有它自己的标签页。
    await page
      .getByRole("button", { name: "切换侧栏" })
      .filter({ visible: true })
      .click()
    await startAnotherConversation(page, "抽屉对话乙")
    await expect(tabsButton).toHaveText("1")
    await tabsButton.click()
    await expect(drawer.getByRole("listitem")).toHaveCount(1)
    await expect(
      drawer.getByRole("button", { name: /^抽屉对话乙/ })
    ).toBeVisible()
    await expect(
      drawer.getByRole("button", { name: /^抽屉对话甲/ })
    ).toHaveCount(0)
    // 5. 切回第一条：终端还在，关掉它只剩一项。
    await page.keyboard.press("Escape")
    await expect(drawer).toBeHidden()
    await page
      .getByRole("button", { name: "切换侧栏" })
      .filter({ visible: true })
      .click()
    await openFromList(page, "抽屉对话甲")
    await expect(tabsButton).toHaveText("2")
    await tabsButton.click()
    await expect(drawer.getByRole("listitem")).toHaveCount(2)
    await drawer.getByRole("button", { name: /^关闭标签页 local/ }).click()
    await expect(drawer.getByRole("listitem")).toHaveCount(1)
  })
})
