import type { Locator, Page } from "@playwright/test"
import { ServerConnection } from "../src/lib/api/server-connection"
import { defaultAppearance } from "../src/lib/appearance/preferences"
import {
  expect,
  getComposer,
  getMessages,
  openTurnRaw,
  startConversation,
  test,
} from "./fixture"

// 从列表行的共用菜单改名。
async function renameConversation(page: Page, row: Locator, name: string) {
  // 1. 打开改名弹窗并保存标题。
  await row.getByRole("button", { name: /的更多操作/ }).click()
  await page.getByRole("menuitem", { name: "改名", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "改名", exact: true })
  await dialog.getByRole("textbox", { name: "标题" }).fill(name)
  await dialog.getByRole("button", { name: "保存", exact: true }).click()
  await expect(dialog).toBeHidden()
}

test(
  "archive_running_conversation_search_open_fork_restore_and_delete",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    // 1. 在工具执行途中归档；页面和运行任务保留，运行中提交的继续输入照常得到回复，对话仍是归档。

    const gate = fixture.createCommandGate("archived-running")
    await fixture.replies(
      {
        tool: "bash",
        arguments: JSON.stringify({
          command: `${gate.wait}; printf archived-running`,
        }),
      },
      { text: "归档后的继续回复" }
    )
    await fixture.openApp(page)
    const serverURL = page.url()
    await startConversation(page, "归档运行验证")
    const conversationURL = page.url()
    const conversationID = conversationURL.split("/c/")[1]
    const sidebarRow = page
      .getByRole("listitem")
      .filter({ has: page.locator(`a[href*="/c/${conversationID}"]`) })
      .filter({ has: page.getByRole("button", { name: /的更多操作/ }) })
    const conversationLink = sidebarRow.getByRole("link")
    await renameConversation(page, sidebarRow, "归档运行验证")
    await sidebarRow.getByRole("button", { name: /的更多操作/ }).click()
    await page.getByRole("menuitem", { name: "归档", exact: true }).click()
    await expect(conversationLink).toHaveCount(0)
    await expect(page).toHaveURL(conversationURL)
    await expect(
      page.getByRole("button", { name: "中断", exact: true })
    ).toBeVisible()
    await getComposer(page).fill("归档后继续输入")
    await getComposer(page).press("Enter")
    await expect(
      page
        .getByRole("region", { name: "待处理", exact: true })
        .getByText("归档后继续输入", { exact: true })
    ).toBeVisible()
    await gate.release()
    await expect(
      getMessages(page).getByText("归档后的继续回复", { exact: true })
    ).toBeVisible()
    await expect(conversationLink).toHaveCount(0)
    await gate.cleanup()

    // 2. 侧栏搜索和归档搜索分别保存在地址里，互不覆盖。
    await page.getByRole("link", { name: "新对话", exact: true }).click()
    await expect(page).toHaveURL(serverURL)
    await expect(
      page.getByRole("combobox", { name: "工作目录", exact: true })
    ).toBeVisible()
    await startConversation(page, "侧栏保留验证")
    const secondID = page.url().split("/c/")[1]
    const secondRow = page
      .getByRole("listitem")
      .filter({ has: page.locator(`a[href*="/c/${secondID}"]`) })
      .filter({ has: page.getByRole("button", { name: /的更多操作/ }) })
    const secondLink = secondRow.getByRole("link")
    await renameConversation(page, secondRow, "侧栏保留验证")
    await page
      .getByRole("textbox", { name: "搜索对话", exact: true })
      .fill("侧栏保留验证")
    await expect(page).toHaveURL(/q=/)
    await page.getByRole("link", { name: "设置", exact: true }).click()
    await expect(
      page.getByRole("tab", { name: "通用", selected: true })
    ).toBeVisible()
    await page.getByRole("tab", { name: "归档对话", exact: true }).click()
    const archive = page.getByRole("region", { name: "归档对话", exact: true })
    const archiveSearch = page.getByRole("textbox", {
      name: "搜索归档对话",
      exact: true,
    })
    await archiveSearch.fill("归档运行验证")
    await expect(page).toHaveURL(/archived_q=/)
    await expect(
      page.getByRole("textbox", { name: "搜索对话", exact: true })
    ).toHaveValue("侧栏保留验证")
    await expect(secondLink).toBeVisible()
    await expect(
      page.getByRole("button", { name: "重新加载配置" })
    ).toHaveCount(0)

    // 3. 打开归档对话不恢复；返回后保留归档搜索，改名和分叉使用共用菜单。
    await archive
      .getByRole("link", { name: "归档运行验证", exact: true })
      .click()
    expect(new URL(page.url()).searchParams.has("archived_q")).toBe(false)
    await expect(
      getMessages(page).getByText("归档后的继续回复", { exact: true })
    ).toBeVisible()
    await page.goBack()
    await expect(archiveSearch).toHaveValue("归档运行验证")
    const archivedRow = archive
      .getByRole("listitem")
      .filter({ hasText: "归档运行验证" })
    await renameConversation(page, archivedRow, "归档已改名")
    await archiveSearch.fill("")
    await expect(
      archive.getByRole("link", { name: "归档已改名", exact: true })
    ).toBeVisible()
    const renamedRow = archive
      .getByRole("listitem")
      .filter({ hasText: "归档已改名" })
    await renamedRow.getByRole("button", { name: /的更多操作/ }).click()
    await page.getByRole("menuitem", { name: "分叉", exact: true }).click()
    await expect(page).toHaveURL(/\/c\/[^/?]+\?q=/)
    expect(new URL(page.url()).pathname).not.toBe(
      new URL(conversationURL).pathname
    )
    expect(new URL(page.url()).searchParams.has("archived_q")).toBe(false)
    await page.goBack()

    // 4. 恢复不弹确认，立即回到侧栏；再次归档后删除复用同一确认框。
    await renamedRow.getByRole("button", { name: /的更多操作/ }).click()
    await page.getByRole("menuitem", { name: "取消归档", exact: true }).click()
    await expect(page.getByRole("alertdialog")).toHaveCount(0)
    await expect(
      archive.getByRole("link", { name: "归档已改名", exact: true })
    ).toHaveCount(0)
    await page.getByRole("textbox", { name: "搜索对话", exact: true }).fill("")
    await expect(conversationLink).toBeVisible()
    await sidebarRow.getByRole("button", { name: /的更多操作/ }).click()
    await page.getByRole("menuitem", { name: "归档", exact: true }).click()
    await expect(
      archive.getByRole("link", { name: "归档已改名", exact: true })
    ).toBeVisible()
    await renamedRow.getByRole("button", { name: /的更多操作/ }).click()
    await page.getByRole("menuitem", { name: "删除", exact: true }).click()
    const confirmation = page.getByRole("alertdialog", {
      name: "删除对话「归档已改名」？",
    })
    await expect(confirmation).toBeVisible()
    await confirmation
      .getByRole("button", { name: "删除", exact: true })
      .click()
    await expect(confirmation).toBeHidden()
    await expect(
      archive.getByRole("link", { name: "归档已改名", exact: true })
    ).toHaveCount(0)
    await expect(page).toHaveURL(
      new RegExp(new URL(serverURL).pathname + "/settings/archived$")
    )
  }
)

test("fork_running_conversation_shows_the_running_turn_as_interrupted", async ({
  page,
  fixture,
}) => {
  // 1. 主对话停在受控命令上，从侧栏菜单分叉：新对话带着这一轮已保存的部分，这一轮显示已中断。
  const gate = fixture.createCommandGate("fork-running")
  await fixture.replies(
    {
      text: "分叉前已保存的回复",
      tool: "bash",
      arguments: JSON.stringify({ command: `${gate.wait}; printf fork-done` }),
    },
    { text: "原对话继续完成" }
  )
  await fixture.openApp(page)
  await startConversation(page, "运行中分叉")
  const sourceURL = page.url()
  const sourceID = sourceURL.split("/c/")[1]
  await expect(getMessages(page).getByText("分叉前已保存的回复")).toBeVisible()
  await expect(
    page.getByRole("button", { name: "中断", exact: true })
  ).toBeVisible()
  const sourceRow = page
    .getByRole("listitem")
    .filter({ has: page.locator(`a[href*="/c/${sourceID}"]`) })
    .filter({ has: page.getByRole("button", { name: /的更多操作/ }) })
  await sourceRow.getByRole("button", { name: /的更多操作/ }).click()
  await page.getByRole("menuitem", { name: "分叉", exact: true }).click()
  await expect(page).not.toHaveURL(sourceURL)
  await expect(page).toHaveURL(/\/c\/[^/?]+/)
  const forkID = new URL(page.url()).pathname.split("/c/")[1]
  expect(forkID).not.toBe(sourceID)
  const messages = getMessages(page)
  await expect(messages.getByText("运行中分叉", { exact: true })).toBeVisible()
  await expect(messages.getByText("分叉前已保存的回复")).toBeVisible()
  await expect(
    messages.locator('[data-turn-status="interrupted"]')
  ).toHaveCount(1)
  await expect(messages.locator('[data-turn-status="running"]')).toHaveCount(0)
  await expect(
    page.getByRole("button", { name: "中断", exact: true })
  ).toHaveCount(0)
  // 2. 历史最前面写明来源：同消息列边缘对齐，排在第一轮之前，链接指向来源对话。
  const origin = messages.locator('[data-fork-origin="fork"]')
  await expect(origin).toHaveText("从『运行中分叉』分叉")
  const [originBox, firstTurnBox] = await Promise.all([
    origin.boundingBox(),
    messages.locator("[data-turn-id]").first().boundingBox(),
  ])
  expect(Math.abs(originBox!.x - firstTurnBox!.x)).toBeLessThanOrEqual(1)
  expect(originBox!.y).toBeLessThan(firstTurnBox!.y)
  const originLink = origin.getByRole("link", { name: "从『运行中分叉』分叉" })
  await expect(originLink).toHaveAttribute(
    "href",
    new RegExp(`/c/${sourceID}$`)
  )
  // 3. 回到原对话：它没有被打断，放行命令后照常收尾，新对话里这一轮仍是已中断；原对话不是分叉，没有来源提示。
  await originLink.click()
  await expect(page).toHaveURL(sourceURL)
  await expect(messages.locator("[data-fork-origin]")).toHaveCount(0)
  await expect(
    page.getByRole("button", { name: "中断", exact: true })
  ).toBeVisible()
  await expect(
    getMessages(page).locator('[data-turn-status="interrupted"]')
  ).toHaveCount(0)
  await gate.release()
  await expect(getMessages(page).getByText("原对话继续完成")).toBeVisible()
  await expect(
    getMessages(page).locator('[data-turn-status="completed"]')
  ).toBeVisible()
  await page.goto(sourceURL.replace(sourceID, forkID))
  await expect(
    getMessages(page).locator('[data-turn-status="interrupted"]')
  ).toHaveCount(1)
  await expect(getMessages(page).getByText("原对话继续完成")).toHaveCount(0)
  await gate.cleanup()
})

// 外观页的字体按钮保持 32px 高，页面不横向溢出。
async function expectAppearanceControlsFit(page: Page) {
  // 1. 读取字体按钮高度和页面横向尺寸。
  await expect
    .poll(() =>
      page
        .getByRole("group", { name: "正文字体", exact: true })
        .getByRole("button")
        .evaluateAll((buttons) =>
          buttons.map((button) => button.getBoundingClientRect().height)
        )
    )
    .toEqual([32, 32, 32])
  const dimensions = await page.evaluate(() => ({
    width: innerWidth,
    content: document.documentElement.scrollWidth,
  }))
  expect(dimensions.content).toBeLessThanOrEqual(dimensions.width)
}

test(
  "general_preferences_are_immediate",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    // 1. 进入默认通用页；本机偏好不提供应用或重载操作。
    await page.setViewportSize({ width: 1280, height: 844 })
    await fixture.openApp(page)
    await page.getByRole("link", { name: "设置", exact: true }).click()
    await expect(page).toHaveURL(/\/settings\/general$/)
    await page.getByRole("tab", { name: "外观", exact: true }).click()
    await expect(
      page.getByRole("button", { name: "重新加载配置" })
    ).toHaveCount(0)
    await expect(
      page.getByRole("button", { name: "应用", exact: true })
    ).toHaveCount(0)

    // 2. 两套配色在浅色和深色下立即生效；切明暗保留字体和配色。
    await page
      .getByRole("group", { name: "正文字体", exact: true })
      .getByRole("button", { name: "系统字体", exact: true })
      .click()
    await page
      .getByRole("group", { name: "标题字体", exact: true })
      .getByRole("button", { name: "Noto Serif", exact: true })
      .click()
    for (const theme of [
      {
        label: "石色",
        value: "stone",
        light: 0.216,
        dark: 0.923,
      },
      { label: "蓝色", value: "blue", light: 0.488, dark: 0.707 },
    ]) {
      await page
        .getByRole("group", { name: "主题配色", exact: true })
        .getByRole("button", { name: theme.label, exact: true })
        .click()
      for (const { name, dark } of [
        { name: "深色", dark: true },
        { name: "浅色", dark: false },
      ]) {
        await page.getByRole("button", { name: /^主题/ }).click()
        await page.getByRole("menuitemradio", { name, exact: true }).click()
        await expect(page.locator("html")).toHaveClass(
          dark ? /dark/ : /^(?!.*dark)/
        )
        await expect(page.locator("html")).toHaveAttribute(
          "data-theme",
          theme.value
        )
        await expect(page.locator("body")).toHaveCSS(
          "font-family",
          "system-ui, sans-serif"
        )
        await expect(
          page.getByRole("heading", { name: "外观", exact: true })
        ).toHaveCSS("font-family", '"Noto Serif Variable", serif')
        const primary = await page.evaluate(() =>
          getComputedStyle(document.documentElement).getPropertyValue(
            "--primary"
          )
        )
        const lightness = primary.match(/oklch\(([\d.]+)(%?)/)!
        const value = Number(lightness[1]) / (lightness[2] ? 100 : 1)
        expect(value).toBeCloseTo(dark ? theme.dark : theme.light)
      }
    }
    await page.getByRole("button", { name: /^主题/ }).click()
    await page
      .getByRole("menuitemradio", { name: "跟随系统", exact: true })
      .click()
    for (const colorScheme of ["dark", "light"] as const) {
      await page.emulateMedia({ colorScheme })
      await expect(page.locator("html")).toHaveClass(
        colorScheme === "dark" ? /dark/ : /^(?!.*dark)/
      )
    }

    // 3. 切换语言立即更新导航、文档语言和存储。
    await page.getByRole("tab", { name: "通用", exact: true }).click()
    await page.getByRole("button", { name: /^语言/ }).click()
    await page
      .getByRole("menuitemradio", { name: "English", exact: true })
      .click()
    await expect(page.locator("html")).toHaveAttribute("lang", "en")
    await expect(
      page.getByRole("tab", { name: "General", selected: true })
    ).toBeVisible()

    // 4. 恢复默认只重置外观，语言保持英文。
    await page.getByRole("tab", { name: "Appearance", exact: true }).click()
    await page
      .getByRole("button", { name: "Reset appearance", exact: true })
      .click()
    await expect(page.locator("html")).toHaveAttribute("lang", "en")
    await expect(page.locator("html")).toHaveAttribute("data-theme", "stone")
    await expect(page.locator("body")).toHaveCSS(
      "font-family",
      '"Noto Serif Variable", serif'
    )
    await expect(
      page.getByRole("heading", { name: "Appearance", exact: true })
    ).toHaveCSS("font-family", '"JetBrains Mono Variable", monospace')
    await page.getByRole("tab", { name: "General", exact: true }).click()
    await page.getByRole("button", { name: /^Language/ }).click()
    await page
      .getByRole("menuitemradio", { name: "简体中文", exact: true })
      .click()
    await expect(page.locator("html")).toHaveAttribute("lang", "zh-CN")
    await expect
      .poll(() =>
        page.evaluate(() => localStorage.getItem("openvaala.language"))
      )
      .toBe("zh-CN")
    await expect
      .poll(() =>
        page.evaluate(() => localStorage.getItem("openvaala.appearance"))
      )
      .toBe(JSON.stringify(defaultAppearance))

    // 5. 桌面和窄屏下控件高度不变，页面都不横向溢出。
    await page.getByRole("tab", { name: "外观", exact: true }).click()
    await expectAppearanceControlsFit(page)
    await page.setViewportSize({ width: 390, height: 844 })
    await expectAppearanceControlsFit(page)
  }
)

test(
  "general_preferences_apply_when_storage_is_denied",
  { tag: "@p1" },
  async ({ context, fixture }) => {
    // 1. 读写存储抛错，或访问 localStorage 本身抛错：本次选择仍立即生效。
    for (const storageInaccessible of [false, true]) {
      const page = await context.newPage()
      try {
        await page.addInitScript((inaccessible) => {
          if (inaccessible) {
            Object.defineProperty(window, "localStorage", {
              get() {
                throw new DOMException("storage denied", "SecurityError")
              },
            })
            return
          }
          Storage.prototype.getItem = () => {
            throw new Error("storage denied")
          }
          Storage.prototype.setItem = () => {
            throw new Error("storage denied")
          }
        }, storageInaccessible)
        await fixture.openApp(page)
        await page.getByRole("link", { name: "设置", exact: true }).click()
        await expect(page).toHaveURL(/\/settings\/general$/)
        await page.getByRole("tab", { name: "外观", exact: true }).click()

        // 2. 配色和明暗立即生效。
        await page
          .getByRole("group", { name: "主题配色", exact: true })
          .getByRole("button", { name: "蓝色", exact: true })
          .click()
        await expect(page.locator("html")).toHaveAttribute("data-theme", "blue")
        await page.getByRole("button", { name: /^主题/ }).click()
        await page
          .getByRole("menuitemradio", { name: "深色", exact: true })
          .click()
        await expect(page.locator("html")).toHaveClass(/dark/)

        // 3. 语言立即更新导航和文档语言；恢复默认只重置外观。
        await page.getByRole("tab", { name: "通用", exact: true }).click()
        await page.getByRole("button", { name: /^语言/ }).click()
        await page
          .getByRole("menuitemradio", { name: "English", exact: true })
          .click()
        await expect(page.locator("html")).toHaveAttribute("lang", "en")
        await expect(
          page.getByRole("tab", { name: "General", selected: true })
        ).toBeVisible()
        await page.getByRole("tab", { name: "Appearance", exact: true }).click()
        await page
          .getByRole("button", { name: "Reset appearance", exact: true })
          .click()
        await expect(page.locator("html")).toHaveAttribute(
          "data-theme",
          "stone"
        )
        await expect(page.locator("html")).toHaveAttribute("lang", "en")
      } finally {
        await page.close()
      }
    }
  }
)

test(
  "appearance_persists_and_syncs_between_tabs",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    // 1. 保存配色和双字体，切换页面和刷新后仍使用已选外观。
    await fixture.openApp(page)
    await page.getByRole("link", { name: "设置", exact: true }).click()
    await page.getByRole("tab", { name: "外观", exact: true }).click()
    await page
      .getByRole("group", { name: "主题配色", exact: true })
      .getByRole("button", { name: "蓝色", exact: true })
      .click()
    await page
      .getByRole("group", { name: "正文字体", exact: true })
      .getByRole("button", { name: "JetBrains Mono", exact: true })
      .click()
    await page
      .getByRole("group", { name: "标题字体", exact: true })
      .getByRole("button", { name: "系统字体", exact: true })
      .click()
    await page.getByRole("tab", { name: "模型服务", exact: true }).click()
    await page.reload()
    await expect(page.locator("html")).toHaveAttribute("data-theme", "blue")
    await expect(page.locator("body")).toHaveCSS(
      "font-family",
      '"JetBrains Mono Variable", monospace'
    )
    await expect(
      page.getByRole("heading", { name: "模型服务", exact: true })
    ).toHaveCSS("font-family", "system-ui, sans-serif")
    await page.getByRole("tab", { name: "外观", exact: true }).click()

    // 2. 同源第二标签页修改外观，原页同步；重置和清除存储也同步。
    const secondPage = await page.context().newPage()
    try {
      await secondPage.goto(page.url())
      await expect(
        secondPage.getByRole("button", { name: "恢复默认外观", exact: true })
      ).toBeVisible()
      await secondPage
        .getByRole("group", { name: "正文字体", exact: true })
        .getByRole("button", { name: "系统字体", exact: true })
        .click()
      await expect(page.locator("body")).toHaveCSS(
        "font-family",
        "system-ui, sans-serif"
      )
      await expect(page.locator("html")).toHaveAttribute("data-theme", "blue")
      await secondPage
        .getByRole("button", { name: "恢复默认外观", exact: true })
        .click()
      await expect(page.locator("html")).toHaveAttribute("data-theme", "stone")
      await expect(page.locator("body")).toHaveCSS(
        "font-family",
        '"Noto Serif Variable", serif'
      )
      await secondPage
        .getByRole("group", { name: "主题配色", exact: true })
        .getByRole("button", { name: "蓝色", exact: true })
        .click()
      await expect(page.locator("html")).toHaveAttribute("data-theme", "blue")
      await secondPage.evaluate(() =>
        localStorage.removeItem("openvaala.appearance")
      )
      await expect(page.locator("html")).toHaveAttribute("data-theme", "stone")
    } finally {
      await secondPage.close()
    }

    // 3. 损坏存储按字段回落，合法选择仍保留；代码和指标字形不跟随正文字体。
    await page.evaluate(() =>
      localStorage.setItem(
        "openvaala.appearance",
        JSON.stringify({
          mode: "dark",
          theme: "blue",
          bodyFont: "system",
          headingFont: "unknown",
        })
      )
    )
    await page.reload()
    await expect(page.locator("html")).toHaveClass(/dark/)
    await expect(page.locator("body")).toHaveCSS(
      "font-family",
      "system-ui, sans-serif"
    )
    await expect(
      page.getByRole("heading", { name: "外观", exact: true })
    ).toHaveCSS("font-family", '"JetBrains Mono Variable", monospace')
    await fixture.replies({ text: "```text\nappearance_code_42\n```" })
    await page.getByRole("link", { name: "新对话", exact: true }).click()
    await startConversation(page, "字体隔离验证")
    await expect(
      getMessages(page).getByText("appearance_code_42", { exact: true })
    ).toHaveCSS("font-family", '"JetBrains Mono Variable", monospace')
    await expect(
      page.getByRole("button", { name: /^上下文占用/ }).getByRole("img")
    ).toHaveCSS("font-family", "Datatype, ui-monospace, monospace")
    await page.evaluate(() =>
      localStorage.setItem("openvaala.appearance", "{broken")
    )
    await page.reload()
    await expect(page.locator("html")).toHaveAttribute("data-theme", "stone")
    await expect(page.locator("body")).toHaveCSS(
      "font-family",
      '"Noto Serif Variable", serif'
    )
  }
)

for (const scenario of [
  { width: 1280, colorScheme: "light" },
  { width: 390, colorScheme: "light" },
  { width: 390, colorScheme: "dark" },
] as const) {
  test(`appearance_reading_controls_${scenario.width}_${scenario.colorScheme}`, async ({
    page,
    fixture,
  }) => {
    // 1. 独立外观菜单提供三组阅读尺寸，预览不创建终端进程。
    await page.setViewportSize({ width: scenario.width, height: 844 })
    await page.emulateMedia({
      colorScheme: scenario.colorScheme,
    })
    const terminalCalls: string[] = []
    page.on("websocket", (socket) =>
      socket.on("framesent", (frame) => {
        const payload = String(frame.payload)
        if (payload.includes('"terminal/open"')) terminalCalls.push(payload)
      })
    )
    await fixture.openApp(page)
    const serverURL = page.url().split("/c/")[0]
    await page.goto(serverURL + "/settings/appearance")
    const body = page.getByRole("region", { name: "正文", exact: true })
    const code = page.getByRole("region", { name: "代码", exact: true })
    const terminal = page.getByRole("region", { name: "终端", exact: true })
    await expect(
      terminal.getByRole("spinbutton", { name: "行高", exact: true })
    ).toHaveValue("1.1")
    await expect(terminal.locator(".xterm-rows")).toContainText("Hello, 世界")
    expect(terminalCalls).toHaveLength(0)
    // 段落实际间距跟随小数值，不能被 Markdown 的默认外边距撑大。
    for (const spacing of [0, 8, 12]) {
      await body
        .getByRole("spinbutton", { name: "段落间距", exact: true })
        .fill(String(spacing))
      await expect
        .poll(async () => {
          const paragraphs = await body
            .locator(".markdown-view > p")
            .evaluateAll((nodes) =>
              nodes.map((node) => {
                const box = node.getBoundingClientRect()
                return { top: box.top, bottom: box.bottom }
              })
            )
          return paragraphs[1].top - paragraphs[0].bottom
        })
        .toBe(spacing)
    }

    // 2. 每个输入立即更新对应阅读样式，输入本身维持 14px，打字不丢焦点。
    for (const change of [
      {
        scope: body,
        label: "字号",
        value: "18",
        target: body.locator(".markdown-view"),
        property: "font-size",
        expected: "18px",
      },
      {
        scope: body,
        label: "行高",
        value: "28",
        target: body.locator(".markdown-view"),
        property: "line-height",
        expected: "28px",
      },
      {
        scope: body,
        label: "段落间距",
        value: "20",
        target: body.locator(".markdown-view > p").nth(1),
        property: "margin-block-start",
        expected: "20px",
      },
      {
        scope: code,
        label: "字号",
        value: "16",
        target: code.locator("pre"),
        property: "font-size",
        expected: "16px",
      },
      {
        scope: code,
        label: "行高",
        value: "26",
        target: code.locator("pre"),
        property: "line-height",
        expected: "26px",
      },
      {
        scope: code,
        label: "水平内边距",
        value: "20",
        target: code.locator("pre"),
        property: "padding-left",
        expected: "20px",
      },
      {
        scope: code,
        label: "垂直内边距",
        value: "16",
        target: code.locator("pre"),
        property: "padding-top",
        expected: "16px",
      },
    ]) {
      const input = change.scope.getByRole("spinbutton", {
        name: change.label,
        exact: true,
      })
      await input.fill(change.value)
      await expect(input).toBeFocused()
      await expect(input).toHaveCSS("font-size", "14px")
      await expect(change.target).toHaveCSS(change.property, change.expected)
    }
    await body.getByRole("button", { name: "增大字号", exact: true }).click()
    await expect(
      body.getByRole("spinbutton", { name: "字号", exact: true })
    ).toHaveValue("19")
    await terminal
      .getByRole("button", { name: "减小行高", exact: true })
      .click()
    await expect(
      terminal.getByRole("spinbutton", { name: "行高", exact: true })
    ).toHaveValue("1.05")
    await terminal
      .getByRole("spinbutton", { name: "内容内边距", exact: true })
      .fill("16")
    await expect(terminal.locator(".xterm")).toHaveCSS("padding", "16px")
    await expectAppearanceControlsFit(page)

    // 3. 刷新仍保留；本组重置不改其他组，全部重置恢复原值。
    await page.reload()
    await expect(
      body.getByRole("spinbutton", { name: "字号", exact: true })
    ).toHaveValue("19")
    // 真实消息复用同一阅读设置，输入和模型选择维持原字号。
    await fixture.replies({
      text: "真实阅读正文\n\n```text\nreading_code_42\n```",
    })
    await page.goto(serverURL)
    await startConversation(page, "阅读字号隔离")
    await expect(
      getMessages(page).getByText("阅读字号隔离", { exact: true })
    ).toHaveCSS("font-size", "19px")
    await expect(
      getMessages(page).getByText("真实阅读正文", { exact: true })
    ).toHaveCSS("font-size", "19px")
    await expect(
      getMessages(page).getByText("reading_code_42", { exact: true })
    ).toHaveCSS("font-size", "16px")
    await expect(getComposer(page)).toHaveCSS("font-size", "14px")
    await expect(page.getByLabel("模型", { exact: true })).toHaveCSS(
      "font-size",
      "14px"
    )
    await page.goto(serverURL + "/settings/appearance")
    await body
      .getByRole("button", { name: "恢复本组默认", exact: true })
      .click()
    await expect(body.locator(".markdown-view")).toHaveCSS("font-size", "14px")
    await expect(code.locator("pre")).toHaveCSS("font-size", "16px")
    await page
      .getByRole("button", { name: "恢复默认外观", exact: true })
      .click()
    await expect(code.locator("pre")).toHaveCSS("font-size", "12px")
    await expect(
      terminal.getByRole("spinbutton", { name: "行高", exact: true })
    ).toHaveValue("1.1")
  })
}

test("appearance_code_line_height_changes_actual_json_rows", async ({
  page,
  fixture,
}) => {
  // 1. 打开真实消息的原始 JSON；从同源设置页调节代码行高。
  await fixture.replies({ text: "JSON行高检查" })
  await fixture.openApp(page)
  await startConversation(page, "JSON行高")
  await expect(
    getMessages(page).getByText("JSON行高检查", { exact: true })
  ).toBeVisible()
  const raw = await openTurnRaw(
    getMessages(page).locator("[data-turn-id]").last()
  )
  const settings = await page.context().newPage()
  try {
    await settings.goto(page.url().split("/c/")[0] + "/settings/appearance")
    const code = settings.getByRole("region", { name: "代码", exact: true })
    // 2. 检查整行和相邻行起点，包含折叠首列和复制按钮的行也不能保持20px。
    const rows = raw.locator("[data-json-path]")
    await expect(rows.first()).toBeVisible()
    for (const height of [16, 18, 28]) {
      await code
        .getByRole("spinbutton", { name: "行高", exact: true })
        .fill(String(height))
      await expect
        .poll(async () =>
          rows.evaluateAll((nodes) =>
            nodes.slice(0, 8).map((node, index, list) => ({
              height: node.getBoundingClientRect().height,
              advance:
                index === 0
                  ? node.getBoundingClientRect().height
                  : node.getBoundingClientRect().top -
                    list[index - 1].getBoundingClientRect().top,
            }))
          )
        )
        .toEqual(Array(8).fill({ height, advance: height }))
    }
  } finally {
    await settings.close()
  }
})

test(
  "standalone_authentication_keeps_language_switcher",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    // 1. 未连接时也能切换语言。
    await page.goto(fixture.state.url + "/web/v1/")
    await page.getByRole("button", { name: /^语言/ }).click()
    await page
      .getByRole("menuitemradio", { name: "English", exact: true })
      .click()
    await expect(
      page.getByRole("heading", { name: "Enter token", exact: true })
    ).toBeVisible()
    await expect(page.locator("html")).toHaveAttribute("lang", "en")
  }
)

test(
  "archived_conversations_load_next_cursor_at_bottom",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    test.setTimeout(180_000)
    // 归档数量要多于一页（50 条），第二页只有几条
    const archivedCount = 52
    // 1. 连接真实协议，读取 seed 前后的预置对话 ID。
    const connection = new ServerConnection({
      url: fixture.state.url.replace(/^http/, "ws") + "/api/v1/openvaala",
      token: fixture.state.token,
    })
    const stop = connection.run()
    async function getSeededConversations() {
      // 1. 按游标取回所有未归档的预置对话。
      const filter = { search_term: "预置对话", archived: false, limit: 100 }
      let result = await connection.call("conversation/list", filter)
      const conversations = [...result.conversations]
      while (result.next_cursor) {
        result = await connection.call("conversation/list", {
          ...filter,
          cursor: result.next_cursor,
        })
        conversations.push(...result.conversations)
      }
      return conversations
    }
    try {
      await expect.poll(() => connection.$state.get()).toBe("connected")
      const existingIDs = new Set(
        (await getSeededConversations()).map(
          ({ conversation }) => conversation.id
        )
      )
      const { longConversationID } = await fixture.seed({
        conversations: archivedCount,
        longConversationTurns: 1,
      })
      const archiveTitlePrefix = `归档分页 ${longConversationID}`
      const seeded = (await getSeededConversations()).filter(
        ({ conversation }) => !existingIDs.has(conversation.id)
      )
      expect(seeded).toHaveLength(archivedCount)

      // 2. 只改名和归档本次新增的这些，用独立标题前缀筛选。
      await Promise.all(
        seeded.map(async ({ conversation }, index) => {
          await connection.call("conversation/setTitle", {
            conversation_id: conversation.id,
            title: `${archiveTitlePrefix} ${index + 1}`,
          })
          await connection.call("conversation/setArchived", {
            conversation_id: conversation.id,
            archived: true,
          })
        })
      )

      // 3. 归档页先只展示第一页，滚到底按 next_cursor 取得剩余记录。
      await fixture.openApp(page)
      await page.getByRole("link", { name: "设置", exact: true }).click()
      await page.getByRole("tab", { name: "归档对话", exact: true }).click()
      await page
        .getByRole("textbox", { name: "搜索归档对话", exact: true })
        .fill(archiveTitlePrefix)
      await expect(page).toHaveURL(/archived_q=/)
      const archive = page.getByRole("region", {
        name: "归档对话",
        exact: true,
      })
      const rows = archive.getByRole("link", {
        name: new RegExp(`^${archiveTitlePrefix} \\d+$`),
      })
      await expect(rows.first()).toBeVisible()
      expect(await rows.count()).toBeLessThan(archivedCount)
      await expect(async () => {
        await rows.last().hover()
        await page.mouse.wheel(0, 10_000)
        await expect(rows).toHaveCount(archivedCount, { timeout: 1_000 })
      }).toPass()
    } finally {
      stop()
    }
  }
)
