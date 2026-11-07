import type { Locator, Page } from "@playwright/test"
import {
  expandTurnProcess,
  expect,
  getComposer,
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

// 面板里的输入框
function getPanelComposer(panel: Locator | Page): Locator {
  return panel.getByRole("textbox", { name: "消息" })
}

// 面板里的消息区
function getPanelMessages(panel: Locator | Page): Locator {
  return panel.getByRole("region", { name: "消息" })
}

// 在对话面板里输入 /btw 命令并发送
async function sendBtw(panel: Locator | Page, command: string): Promise<void> {
  await getPanelComposer(panel).fill(command)
  await getPanelComposer(panel).press("Enter")
}

test.describe("desktop", () => {
  test.use({ viewport: { width: 1280, height: 800 } })

  test("btw_opens_a_side_tab_right_of_the_source_with_only_its_own_turn", async ({
    page,
    fixture,
  }) => {
    // 1. 对话完成一轮后，输入 /btw 加问题。
    await fixture.replies({ text: "主对话回复" }, { text: "旁支回复" })
    await fixture.openApp(page)
    await startConversation(page, "主对话问题")
    const source = getTabPanels(page).first()
    await expect(getPanelMessages(source)).toContainText("主对话回复")
    const sourceURL = page.url()
    await sendBtw(source, "/btw 旁支问题")
    // 2. 旁支开在来源区域右边：标签页的标题是「旁支 · 问题」，输入框清空，来源的消息区不显示旁支的问答。
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page).last()).toHaveText("旁支 · 旁支问题")
    await expect(getTabButtons(page).last()).toHaveAttribute(
      "aria-selected",
      "true"
    )
    await expect(getPanelComposer(source)).toHaveValue("")
    const side = getTabPanels(page).last()
    const [sourceBox, sideBox] = await Promise.all([
      source.boundingBox(),
      side.boundingBox(),
    ])
    expect(sideBox!.x).toBeGreaterThanOrEqual(sourceBox!.x + sourceBox!.width)
    // 3. 旁支只显示分叉之后的内容：说明折叠成一行，问题和回复在，继承来的历史不在。
    const sideMessages = getPanelMessages(side)
    await expect(sideMessages).toContainText("旁支问题")
    await expect(sideMessages).toContainText("旁支回复")
    await expect(sideMessages).toContainText("客户端指令")
    const sideTurns = sideMessages.locator("[data-turn-id]")
    await expect(sideTurns).not.toContainText("主对话问题")
    await expect(sideTurns).not.toContainText("主对话回复")
    await expect(getPanelMessages(source)).not.toContainText("旁支问题")
    await expect(getPanelMessages(source)).not.toContainText("旁支回复")
    // 4. 消息列的第一行写明来源：和第一轮同在消息列里，左右边缘对齐，排在第一轮之前。
    const origin = sideMessages.locator(
      '[data-fork-origin="side-conversation"]'
    )
    await expect(origin).toHaveText("从『主对话问题』分出的旁支")
    const [originBox, firstTurnBox] = await Promise.all([
      origin.boundingBox(),
      sideTurns.first().boundingBox(),
    ])
    expect(Math.abs(originBox!.x - firstTurnBox!.x)).toBeLessThanOrEqual(1)
    expect(
      Math.abs(originBox!.width - firstTurnBox!.width)
    ).toBeLessThanOrEqual(1)
    expect(originBox!.y).toBeLessThan(firstTurnBox!.y)
    // 5. 旁支选中时右栏 Agent 页签里的「主 Agent」是旁支自己的，没有页面可去，只是文字，不是链接。
    await openRightPanel(page)
    await page.getByRole("tab", { name: "Agent", exact: true }).click()
    const agents = page.getByRole("tabpanel", { name: "Agent", exact: true })
    await expect(agents.getByText("主 Agent", { exact: true })).toBeVisible()
    await expect(agents.getByRole("link", { name: "主 Agent" })).toHaveCount(0)
    // 6. 点「从『…』分出的旁支」切回来源标签页，地址不变；来源对话的「主 Agent」仍是链接。
    await origin
      .getByRole("button", { name: "从『主对话问题』分出的旁支" })
      .click()
    await expect(agents.getByRole("link", { name: "主 Agent" })).toBeVisible()
    await expect(getTabButtons(page).first()).toHaveAttribute(
      "aria-selected",
      "true"
    )
    await expect(page).toHaveURL(sourceURL)
    await expect(getPanelMessages(source)).not.toContainText("旁支回复")
  })

  test("btw_session_names_are_plain_text_not_links", async ({
    page,
    fixture,
  }) => {
    // 1. 旁支里委派一个子 Agent。
    await fixture.replies(
      { text: "主对话回复" },
      {
        tool: "spawn_agent",
        arguments: JSON.stringify({
          task_name: "side-helper",
          message: "旁支委派的任务",
        }),
      }
    )
    await fixture.openApp(page)
    await startConversation(page, "旁支名字第一条")
    const source = getTabPanels(page).first()
    await expect(getPanelMessages(source)).toContainText("主对话回复")
    await sendBtw(source, "/btw 旁支里委派")
    const side = getTabPanels(page).last()
    // 2. 旁支的消息里和右栏的树里，子 Agent 的名字都是文字，没有链接。
    await openRightPanel(page)
    await page.getByRole("tab", { name: "Agent", exact: true }).click()
    const agents = page.getByRole("tabpanel", { name: "Agent", exact: true })
    await expect(agents.getByText("side-helper").first()).toBeVisible()
    await expect(agents.getByRole("link")).toHaveCount(0)
    await expect(
      getPanelMessages(side).getByText("side-helper").first()
    ).toBeVisible()
    await expect(
      getPanelMessages(side).getByRole("link", { name: /side-helper/ })
    ).toHaveCount(0)
    // 3. 来源对话里没有这个子 Agent，右栏的「主 Agent」又是链接。
    await side.getByRole("button", { name: /^从『.+』分出的旁支$/ }).click()
    await expect(agents.getByRole("link", { name: "主 Agent" })).toBeVisible()
    await expect(agents.getByText("side-helper")).toHaveCount(0)
  })

  test("btw_is_offered_only_where_it_can_run", async ({ page, fixture }) => {
    // 1. 新对话标签页的输入框没有 /btw。
    await fixture.replies({ text: "主对话回复" }, { text: "旁支回复" })
    await fixture.openApp(page)
    await getComposer(page).fill("/")
    await expect(page.getByRole("option").first()).toBeVisible()
    await expect(page.getByRole("option", { name: /\/btw/ })).toHaveCount(0)
    // 2. 有内容的对话标签页里，/btw 排在补全列表最前面。
    await getComposer(page).fill("")
    await startConversation(page, "补全第一条")
    const source = getTabPanels(page).first()
    await expect(getPanelMessages(source)).toContainText("主对话回复")
    await getPanelComposer(source).fill("/")
    const options = page.getByRole("option")
    await expect(options.first()).toHaveText(/^\/btw/)
    // 3. 旁支标签页的输入框没有 /btw。
    await getPanelComposer(source).fill("")
    await sendBtw(source, "/btw 补全旁支")
    const side = getTabPanels(page).last()
    await expect(getPanelMessages(side)).toContainText("旁支回复")
    await getPanelComposer(side).fill("/")
    await expect(page.getByRole("option", { name: /\/btw/ })).toHaveCount(0)
  })

  test("btw_without_question_opens_an_empty_side_tab_with_focus", async ({
    page,
    fixture,
  }) => {
    // 1. 对话完成一轮后输入 /btw：第一次回车选中补全，第二次回车才发送。
    await fixture.replies({ text: "主对话回复" }, { text: "旁支回复" })
    await fixture.openApp(page)
    await startConversation(page, "空旁支第一条")
    const source = getTabPanels(page).first()
    await expect(getPanelMessages(source)).toContainText("主对话回复")
    await getPanelComposer(source).fill("/btw")
    await getPanelComposer(source).press("Enter")
    await expect(getPanelComposer(source)).toHaveValue("/btw ")
    await getPanelComposer(source).press("Enter")
    // 2. 旁支标签页只叫「旁支」，输入框直接获得焦点，还没有问题和回复。
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page).last()).toHaveText("旁支")
    const side = getTabPanels(page).last()
    await expect(getPanelComposer(side)).toBeFocused()
    await expect(getPanelMessages(side)).not.toContainText("主对话回复")
    // 3. 在旁支里发问题，说明和问题在同一轮里。
    await getPanelComposer(side).fill("补一个问题")
    await getPanelComposer(side).press("Enter")
    await expect(getPanelMessages(side)).toContainText("补一个问题")
    await expect(getPanelMessages(side)).toContainText("旁支回复")
    await expect(getPanelMessages(side)).toContainText("客户端指令")
    await expect(getTabButtons(page).last()).toHaveText("旁支")
  })

  test("btw_works_while_the_main_turn_keeps_running", async ({
    page,
    fixture,
  }) => {
    // 1. 主对话的第一轮一直在生成，这时输入 /btw。
    await fixture.replies(
      { text: "主对话生成中", hold: true },
      { text: "旁支回复" }
    )
    await fixture.openApp(page)
    await startConversation(page, "运行中第一条")
    const source = getTabPanels(page).first()
    await expect(getPanelMessages(source)).toContainText("主对话生成中")
    await sendBtw(source, "/btw 运行时提问")
    // 2. 旁支开在右边并回复，主对话没有被中断。
    await expect(getAreas(page)).toHaveCount(2)
    const side = getTabPanels(page).last()
    await expect(getPanelMessages(side)).toContainText("旁支回复")
    await expect(source.getByRole("button", { name: "中断" })).toBeVisible()
  })

  test("btw_reads_a_file_while_the_main_tool_runs_and_stays_out_of_main_history", async ({
    page,
    fixture,
  }) => {
    // 1. 主对话停在受控命令上，这时 /btw 问一个要读文件的问题：模型先调 read_file，再回答，最后主对话收尾。
    const gate = fixture.createCommandGate("btw-main-running")
    await fixture.replies(
      {
        tool: "bash",
        arguments: JSON.stringify({
          command: `${gate.wait}; printf main-done`,
        }),
      },
      { tool: "read_file", arguments: JSON.stringify({ path: "readme.txt" }) },
      { text: "旁支读完文件的回答" },
      { text: "主对话收尾回复" }
    )
    await fixture.openApp(page)
    await startConversation(page, "并行主任务")
    const source = getTabPanels(page).first()
    await expect(source.getByRole("button", { name: "中断" })).toBeVisible()
    const mainID = page.url().split("/c/")[1]
    await sendBtw(source, "/btw 文件里写了什么")
    // 2. 旁支答完，它的 read_file 真的读到了文件内容；这时主对话仍在运行，没有被中断。
    await expect(getAreas(page)).toHaveCount(2)
    const side = getTabPanels(page).last()
    const sideMessages = getPanelMessages(side)
    await expect(sideMessages).toContainText("旁支读完文件的回答")
    const sideTurn = sideMessages.locator("[data-turn-id]").last()
    await expect(
      sideTurn.locator('[data-turn-status="completed"]')
    ).toBeVisible()
    await expandTurnProcess(sideTurn)
    const readRow = sideTurn.locator("[data-item-key]").filter({
      has: page.locator("[data-item-title]").filter({ hasText: "读取" }),
    })
    await readRow
      .locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
      .click()
    await expect(readRow).toContainText("workspace fixture")
    await expect(source.getByRole("button", { name: "中断" })).toBeVisible()
    await expect(
      source.locator('[data-turn-status="interrupted"]')
    ).toHaveCount(0)
    // 3. 放行主对话的命令，它正常收尾；主对话里没有旁支的问答、说明和读文件的工具。
    await gate.release()
    const sourceMessages = getPanelMessages(source)
    await expect(sourceMessages).toContainText("主对话收尾回复")
    await expect(source.locator('[data-turn-status="completed"]')).toBeVisible()
    await expect(
      source.locator('[data-turn-status="interrupted"]')
    ).toHaveCount(0)
    for (const leaked of ["文件里写了什么", "旁支读完文件的回答", "客户端指令"])
      await expect(sourceMessages).not.toContainText(leaked)
    await expect(sourceMessages).not.toContainText("readme.txt")
    // 4. 刷新后从服务端重新读出的主对话历史同样干净，保存的消息里也没有旁支内容。
    await page.reload()
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getPanelMessages(getTabPanels(page).first())).toContainText(
      "主对话收尾回复"
    )
    await expect(
      getPanelMessages(getTabPanels(page).first())
    ).not.toContainText("旁支读完文件的回答")
    const saved = JSON.stringify(await fixture.messages(mainID))
    for (const leaked of ["文件里写了什么", "旁支读完文件的回答", "客户端指令"])
      expect(saved).not.toContain(leaked)
    expect(saved).toContain("并行主任务")
    await gate.cleanup()
  })

  test("reload_within_the_idle_window_keeps_the_side_tab", async ({
    page,
    fixture,
  }) => {
    // 1. 开一个旁支，等它回复完。
    await fixture.replies({ text: "主对话回复" }, { text: "旁支回复" })
    await fixture.openApp(page)
    await startConversation(page, "刷新第一条")
    const source = getTabPanels(page).first()
    await expect(getPanelMessages(source)).toContainText("主对话回复")
    await sendBtw(source, "/btw 刷新后还在吗")
    await expect(getPanelMessages(getTabPanels(page).last())).toContainText(
      "旁支回复"
    )
    // 2. 刷新后旁支标签页还在，重新订阅出同样的内容。
    await page.reload()
    await expect(getAreas(page)).toHaveCount(2)
    await expect(getTabButtons(page)).toHaveCount(2)
    await expect(getTabButtons(page).last()).toHaveText("旁支 · 刷新后还在吗")
    const side = getTabPanels(page).last()
    await expect(getPanelMessages(side)).toContainText("刷新后还在吗")
    await expect(getPanelMessages(side)).toContainText("旁支回复")
    await expect(getPanelMessages(side)).not.toContainText("主对话回复")
  })

  test("closing_a_running_side_tab_interrupts_it_first", async ({
    page,
    fixture,
  }) => {
    const requests = recordRequests(page)
    // 1. 先开一个已经回复完的旁支，关掉它不会发中断。
    await fixture.replies(
      { text: "主对话回复" },
      { text: "第一个旁支回复" },
      { text: "第二个旁支生成中", hold: true }
    )
    await fixture.openApp(page)
    await startConversation(page, "关闭第一条")
    const source = getTabPanels(page).first()
    await expect(getPanelMessages(source)).toContainText("主对话回复")
    await sendBtw(source, "/btw 第一个旁支")
    await expect(getPanelMessages(getTabPanels(page).last())).toContainText(
      "第一个旁支回复"
    )
    await getTabButtons(page).last().getByTitle("关闭标签页").click()
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect
      .poll(
        () =>
          requests.filter((r) => r.method === "conversation/unsubscribe").length
      )
      .toBeGreaterThan(0)
    expect(
      requests.some((r) => r.method === "conversation/interruptTurn")
    ).toBe(false)
    // 2. 再开一个还在生成的旁支，关掉它：先中断这个旁支，再退订它，主对话不受影响。
    await sendBtw(getTabPanels(page).first(), "/btw 第二个旁支")
    const side = getTabPanels(page).last()
    await expect(getPanelMessages(side)).toContainText("第二个旁支生成中")
    await expect(side.getByRole("button", { name: "中断" })).toBeVisible()
    const before = requests.length
    await getTabButtons(page).last().getByTitle("关闭标签页").click()
    await expect(getTabButtons(page)).toHaveCount(1)
    await expect
      .poll(() => requests.slice(before).map((r) => r.method))
      .toEqual(
        expect.arrayContaining([
          "conversation/interruptTurn",
          "conversation/unsubscribe",
        ])
      )
    const after = requests.slice(before)
    const interrupt = after.find(
      (r) => r.method === "conversation/interruptTurn"
    )!
    const unsubscribe = after.find(
      (r) => r.method === "conversation/unsubscribe"
    )!
    expect(after.indexOf(interrupt)).toBeLessThan(after.indexOf(unsubscribe))
    expect(interrupt.conversationID).toBe(unsubscribe.conversationID)
    expect(
      after.filter((r) => r.method === "conversation/interruptTurn")
    ).toHaveLength(1)
  })
})

test.describe("narrow", () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test("btw_opens_a_selected_tab_and_lists_it_in_the_drawer", async ({
    page,
    fixture,
  }) => {
    // 1. 窄屏对话完成一轮后输入 /btw。
    await fixture.replies({ text: "主对话回复" }, { text: "旁支回复" })
    await fixture.openApp(page)
    await startConversation(page, "窄屏第一条")
    const tabsButton = page.getByRole("button", {
      name: "标签页列表",
      exact: true,
    })
    await expect(tabsButton).toHaveText("1")
    await expect(page.getByRole("region", { name: "消息" })).toContainText(
      "主对话回复"
    )
    await sendBtw(page, "/btw 窄屏旁支")
    // 2. 旁支是新的、被选中的标签页，窄屏只显示它。
    await expect(tabsButton).toHaveText("2")
    const visiblePanels = getTabPanels(page).and(page.locator(":visible"))
    await expect(visiblePanels).toHaveCount(1)
    await expect(getPanelMessages(visiblePanels)).toContainText("窄屏旁支")
    await expect(getPanelMessages(visiblePanels)).toContainText("旁支回复")
    await expect(getPanelMessages(visiblePanels)).not.toContainText(
      "主对话回复"
    )
    // 窄屏的来源提示同样和第一轮在同一条消息列边缘上。
    const origin = getPanelMessages(visiblePanels).locator(
      '[data-fork-origin="side-conversation"]'
    )
    await expect(origin).toHaveText("从『窄屏第一条』分出的旁支")
    const [originBox, firstTurnBox] = await Promise.all([
      origin.boundingBox(),
      getPanelMessages(visiblePanels)
        .locator("[data-turn-id]")
        .first()
        .boundingBox(),
    ])
    expect(Math.abs(originBox!.x - firstTurnBox!.x)).toBeLessThanOrEqual(1)
    expect(
      Math.abs(originBox!.width - firstTurnBox!.width)
    ).toBeLessThanOrEqual(1)
    // 3. 抽屉里列出它，打勾的是旁支；切回第一条对话。
    await tabsButton.click()
    const drawer = page.getByRole("dialog", { name: "标签页", exact: true })
    await expect(
      drawer.getByRole("button", { name: "旁支 · 窄屏旁支", exact: true })
    ).toHaveAttribute("aria-current", "true")
    await drawer.getByRole("button", { name: /^窄屏第一条/ }).click()
    await expect(drawer).toBeHidden()
    await expect(page.getByRole("region", { name: "消息" })).toContainText(
      "主对话回复"
    )
    await expect(page.getByRole("region", { name: "消息" })).not.toContainText(
      "旁支回复"
    )
  })
})
