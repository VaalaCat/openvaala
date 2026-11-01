import type { Locator, Page } from "@playwright/test"
import { ServerConnection } from "../src/lib/api/server-connection"
import { defaultAppearance } from "../src/lib/appearance/preferences"
import {
  expect,
  getComposer,
  getMessages,
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
    // 本例核对列表和菜单操作，不看动画：关闭动画省去弹层展开、收起的稳定等待。
    await page.emulateMedia({ reducedMotion: "reduce" })
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

// 通用页的字体按钮保持 32px 高，页面不横向溢出。
async function expectGeneralControlsFit(page: Page) {
  // 1. 读取字体按钮高度和页面横向尺寸。
  const controlHeights = await page
    .getByRole("group", { name: "正文字体", exact: true })
    .getByRole("button")
    .evaluateAll((buttons) =>
      buttons.map((button) => button.getBoundingClientRect().height)
    )
  expect(controlHeights).toEqual([32, 32, 32])
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
    await page.emulateMedia({ reducedMotion: "reduce" })
    await page.setViewportSize({ width: 1280, height: 844 })
    await fixture.openApp(page)
    await page.getByRole("link", { name: "设置", exact: true }).click()
    await expect(page).toHaveURL(/\/settings\/general$/)
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
          page.getByRole("heading", { name: "通用", exact: true })
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
    await page.getByRole("button", { name: /^语言/ }).click()
    await page
      .getByRole("menuitemradio", { name: "English", exact: true })
      .click()
    await expect(page.locator("html")).toHaveAttribute("lang", "en")
    await expect(
      page.getByRole("tab", { name: "General", selected: true })
    ).toBeVisible()

    // 4. 恢复默认只重置外观，语言保持英文。
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
      page.getByRole("heading", { name: "General", exact: true })
    ).toHaveCSS("font-family", '"JetBrains Mono Variable", monospace')
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
    await expectGeneralControlsFit(page)
    await page.setViewportSize({ width: 390, height: 844 })
    await expectGeneralControlsFit(page)
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
        await page.emulateMedia({ reducedMotion: "reduce" })
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
        await page.getByRole("button", { name: /^语言/ }).click()
        await page
          .getByRole("menuitemradio", { name: "English", exact: true })
          .click()
        await expect(page.locator("html")).toHaveAttribute("lang", "en")
        await expect(
          page.getByRole("tab", { name: "General", selected: true })
        ).toBeVisible()
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
    await page.emulateMedia({ reducedMotion: "reduce" })
    await fixture.openApp(page)
    await page.getByRole("link", { name: "设置", exact: true }).click()
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
    await page.getByRole("tab", { name: "通用", exact: true }).click()

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
      page.getByRole("heading", { name: "通用", exact: true })
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
