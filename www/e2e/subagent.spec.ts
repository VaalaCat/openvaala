import {
  expect,
  getComposer,
  getMessages,
  openTurnRaw,
  startConversation,
  test,
} from "./fixture"

test(
  "sub_agent_appears_in_tree_and_opens_read_only",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    // 1. 主 Agent 调 spawn_agent 委派 helper，之后主 Agent 和子 Agent 的请求都回 ok。
    await fixture.replies({
      tool: "spawn_agent",
      arguments: JSON.stringify({
        task_name: "helper",
        message: "帮主 Agent 看一下",
      }),
    })
    await fixture.openApp(page)
    await startConversation(page, "派一个子 Agent")
    await expect(
      getMessages(page).getByText("ok", { exact: true }).first()
    ).toBeVisible()
    await openTurnRaw(getMessages(page).locator("[data-turn-id]").last())
    // 2. 右栏「Agent」页签的树里出现 helper，等它跑完。
    await page.getByRole("tab", { name: "Agent" }).click()
    const helper = page
      .getByRole("tabpanel", { name: "Agent" })
      .getByRole("link", { name: /helper/ })
    await expect(helper).toBeVisible()
    await expect(helper).toContainText("空闲")
    // 3. 点进去：先等页面换成子 Session（没有输入框），地址刚变时消息区可能还是主 Session 的，那里有任务摘要和好几条 ok；
    //    子 Session 的消息区里是委派的消息和子 Agent 唯一的一条回复。
    await helper.click()
    await expect(page).toHaveURL(/\/sessions\/[^/]+$/)
    await expect(getComposer(page)).toHaveCount(0)
    const messages = getMessages(page)
    await expect(messages.getByText("帮主 Agent 看一下")).toBeVisible()
    await expect(messages.getByText("帮主 Agent 看一下")).toHaveCSS(
      "mask-image",
      "none"
    )
    await expect(
      messages.getByRole("button", { name: /^展开(?:\s|$)/ })
    ).toHaveCount(0)
    await expect(messages.getByText("ok", { exact: true })).toHaveCount(1)
    await expect(messages.getByText("ok", { exact: true })).toBeVisible()
    await page.getByRole("tab", { name: "调试", exact: true }).click()
    await expect(
      page.getByText("选择一条记录查看调试数据", { exact: true })
    ).toBeVisible()
    await expect(page.locator("[data-debug-kind]")).toHaveCount(0)
  }
)

test("long_agent_letters_preview_four_lines_with_fade_and_keep_expanded_content", async ({
  page,
  fixture,
}) => {
  const letter =
    Array.from(
      { length: 12 },
      (_, index) => `来信第${index + 1}行：核对结果与后续操作。`
    ).join("\n") + "\n\n![来信图片](https://preview.test/letter-preview.png)"
  await page.route("**/letter-preview.png", (route) =>
    route.fulfill({
      contentType: "image/png",
      body: Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jewoAAAAASUVORK5CYII=",
        "base64"
      ),
    })
  )
  await fixture.replies({
    tool: "spawn_agent",
    arguments: JSON.stringify({ task_name: "letter-helper", message: letter }),
  })
  await fixture.openApp(page)
  await startConversation(page, "发送长来信")
  await page.getByRole("tab", { name: "Agent" }).click()
  const helper = page
    .getByRole("tabpanel", { name: "Agent" })
    .getByRole("link", { name: /letter-helper/ })
  await expect(helper).toContainText("空闲")
  await helper.click()
  await expect(getComposer(page)).toHaveCount(0)
  const messages = getMessages(page)
  const expand = messages.getByRole("button", { name: /^展开(?:\s|$)/ })
  await expect(expand).toBeVisible()
  const preview = messages.locator("p").filter({ hasText: letter }).first()
  await expect(preview).toHaveCSS("font-size", "14px")
  await expect(preview).toHaveCSS("line-height", "24px")
  const letterItem = preview.locator("xpath=ancestor::*[@data-item-key][1]")
  const markdown = letterItem.locator(".markdown-view")
  await expect(markdown).toHaveCount(0)
  const sender = messages
    .getByRole("link", { name: "主 Agent", exact: true })
    .last()
  await expect(sender).toBeVisible()
  // 浅色宽屏和深色窄屏各走一遍：预览行数、遮罩和层级颜色随版式、配色变化。
  for (const { colorScheme, width } of [
    { colorScheme: "light", width: 1280 },
    { colorScheme: "dark", width: 390 },
  ] as const) {
    await page.emulateMedia({ colorScheme })
    await page.setViewportSize({ width, height: 844 })
    await expect(preview).toHaveCSS("height", "96px")
    await expect(preview).toHaveCSS("mask-image", /linear-gradient/)
    expect(await preview.evaluate((node) => node.scrollHeight)).toBeGreaterThan(
      96
    )
    // 来信预览与来源各用次要层级，普通助手正文不随之变淡。
    await page.mouse.move(width - 4, 4)
    const previewColor = await preview.evaluate(
      (node) => getComputedStyle(node).color
    )
    const sourceColor = await sender.evaluate(
      (node) => getComputedStyle(node).color
    )
    const bodyColor = await messages
      .getByText("ok", { exact: true })
      .evaluate((node) => getComputedStyle(node).color)
    expect(previewColor).not.toBe(bodyColor)
    expect(sourceColor).not.toBe(bodyColor)
    expect(previewColor).not.toBe(sourceColor)
    await sender.hover()
    await expect(preview).toHaveCSS("color", previewColor)
    await expect(sender).not.toHaveCSS("color", sourceColor)
    await expand.click()
    await expect(preview).toBeHidden()
    await expect(preview).toHaveCSS("mask-image", "none")
    await expect(markdown).toBeVisible()
    await expect(markdown).toHaveCSS("mask-image", "none")
    await expect(markdown).toContainText("来信第12行")
    await expect(markdown).toHaveCSS("color", bodyColor)
    await expect(sender).toBeVisible()
    const mountedMarkdown = await markdown.elementHandle()
    await markdown
      .getByRole("button", { name: "预览图片 来信图片", exact: true })
      .click()
    const imageDialog = page.getByRole("dialog", {
      name: "图片预览",
      exact: true,
    })
    await expect(imageDialog.locator("img")).toHaveJSProperty("naturalWidth", 1)
    await page.keyboard.press("Escape")
    await expect(imageDialog).toBeHidden()
    await messages.getByRole("button", { name: /^收起(?:\s|$)/ }).click()
    await expect(expand).toBeVisible()
    await expect(markdown).toBeHidden()
    expect(await mountedMarkdown!.evaluate((node) => node.isConnected)).toBe(
      true
    )
  }
  await expect(messages.getByText("ok", { exact: true })).toBeVisible()
})
