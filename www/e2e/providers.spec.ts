import type { Locator, Page } from "@playwright/test"
import {
  ProtocolVersion,
  type ListProvidersResult,
  type ProviderConfig,
} from "../src/lib/api/protocol.gen"
import { expect, test, startConversation, type WebUIFixture } from "./fixture"

// 右下角的 toast 里出现这段文字
async function expectToast(page: Page, text: string) {
  await expect(
    page.getByRole("region", { name: /通知/ }).getByText(text)
  ).toBeVisible()
}

// 经真实 OpenVaala WebSocket 读取或修改配置。
async function callServer<T>(
  fixture: WebUIFixture,
  method: string,
  params: unknown
): Promise<T> {
  const socket = new WebSocket(
    fixture.state.url.replace(/^http/, "ws") +
      "/api/v1/openvaala?token=" +
      encodeURIComponent(fixture.state.token)
  )
  await new Promise((resolve, reject) => {
    socket.onopen = resolve
    socket.onerror = reject
  })
  const call = (id: number, method: string, params: unknown) =>
    new Promise((resolve, reject) => {
      socket.addEventListener("message", function handle(event) {
        const message = JSON.parse(String(event.data))
        if (message.id !== id) return
        socket.removeEventListener("message", handle)
        if (message.error) reject(new Error(JSON.stringify(message.error)))
        else resolve(message.result)
      })
      socket.send(JSON.stringify({ id, method, params }))
    })
  try {
    await call(1, "initialize", {
      client_info: { name: "openvaala-e2e", version: "0" },
      protocol_version: ProtocolVersion,
    })
    return (await call(2, method, params)) as T
  } finally {
    socket.close()
  }
}

async function setDefaultModel(fixture: WebUIFixture, model: string) {
  await callServer(fixture, "config/setDefaultModel", { model })
}

async function selectValue(page: Page, label: string, option: string | RegExp) {
  const trigger = page.getByRole("combobox", { name: label, exact: true })
  await trigger.click()
  await expect(trigger).toHaveAttribute("aria-expanded", "true")
  const listID = await trigger.getAttribute("aria-controls")
  await page
    .locator(`[id="${listID}"]`)
    .getByRole("option", { name: option, exact: true })
    .click()
}

async function openProviders(page: Page, fixture: WebUIFixture) {
  await fixture.openApp(page)
  await page.getByRole("link", { name: "设置", exact: true }).click()
  await page.getByRole("tab", { name: "模型服务", exact: true }).click()
}

// 触发折叠后在首帧尝试聚焦旧字段，焦点应留在触发器。
async function expectClosingFieldUnfocusable(
  trigger: Locator,
  fieldID: string
) {
  // 1. 点击后下一帧检查尚在退出动画中的字段。
  const focusStayedOnTrigger = await trigger.evaluate(
    async (button: HTMLButtonElement, id) => {
      button.focus()
      button.click()
      await new Promise<void>((resolve) =>
        requestAnimationFrame(() => resolve())
      )
      document.getElementById(id)?.focus()
      return document.activeElement === button
    },
    fieldID
  )
  expect(focusStayedOnTrigger).toBe(true)
}

test(
  "provider_create_fetch_set_default_and_delete",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    // 1. 假模型的 /models 列出两个模型；打开设置里的模型服务列表。

    await fixture.models(["e2e-model-a", "e2e-model-b"])
    await fixture.openApp(page)
    await page.getByRole("link", { name: "设置" }).click()
    await page.getByRole("tab", { name: "模型服务" }).click()
    const providersTab = page.getByRole("tab", {
      name: "模型服务",
      selected: true,
    })
    await expect(providersTab).toBeVisible()

    // 2. 新增：填 ID 和 base_url，拉取模型并全部勾上，创建后回到列表。
    await page.getByRole("button", { name: "新增模型服务" }).first().click()
    await page.getByLabel("ID", { exact: true }).fill("e2e-added")
    await page
      .getByLabel("base_url", { exact: true })
      .fill(fixture.modelBaseURL)
    await page.getByRole("button", { name: "拉取模型" }).click()
    const dialog = page.getByRole("dialog", { name: "拉取模型" })
    for (const id of ["e2e-model-a", "e2e-model-b"])
      await dialog.getByRole("checkbox", { name: id }).check()
    await dialog.getByRole("button", { name: "确定" }).click()
    await expect(dialog).toBeHidden()
    await expect(
      page.getByRole("button", { name: "移除模型 e2e-model-b" })
    ).toBeVisible()
    await page.getByRole("button", { name: "创建" }).click()
    await expectToast(page, "已新增模型服务「e2e-added」")
    const addedRow = page.getByRole("link", { name: /e2e-added/ })
    await expect(addedRow).toContainText("2 个模型")

    // 3. 桌面用 Combobox、手机设置页用 Drawer，选择后真实保存默认模型。
    for (const { width, modelID } of [
      { width: 1280, modelID: "e2e-model-a" },
      { width: 390, modelID: "e2e-model-b" },
    ]) {
      await page.setViewportSize({ width, height: 844 })
      await page
        .getByRole(width < 768 ? "button" : "combobox", {
          name: "模型",
          exact: true,
        })
        .click()
      if (width < 768) {
        const picker = page.getByRole("dialog", { name: "模型", exact: true })
        await expect(picker).toHaveAttribute("data-slot", "drawer-popup")
        await picker
          .getByRole("searchbox", { name: "搜索模型", exact: true })
          .fill(modelID)
        await picker.getByRole("button", { name: new RegExp(modelID) }).click()
        await expect(picker).toBeHidden()
      } else {
        await expect(page.locator('[data-slot="drawer-popup"]')).toHaveCount(0)
        await page.getByRole("option", { name: new RegExp(modelID) }).click()
      }
      await expectToast(page, `默认模型改成了 e2e-added/${modelID}`)
      await expect(addedRow).toContainText("默认")
    }
    await page.setViewportSize({ width: 1280, height: 844 })

    // 4. 删除：进编辑页，在危险操作里删除并确认，列表里没有它了。
    await addedRow.click()
    await page.getByRole("button", { name: "删除", exact: true }).click()
    const confirm = page.getByRole("alertdialog", {
      name: "删除模型服务「e2e-added」？",
    })
    await confirm.getByRole("button", { name: "删除", exact: true }).click()
    await expectToast(page, "已删除模型服务「e2e-added」")
    await expect(providersTab).toBeVisible()
    await expect(addedRow).toHaveCount(0)

    // 5. 把默认模型显式写回 local/default，后面的用例不靠「只剩 local 时自动回落」。
    await setDefaultModel(fixture, "local/default")
  }
)

test("provider_disclosures_preserve_drafts", async ({ page, fixture }) => {
  // 1. 展开地址覆盖，收起再打开后保留输入和触发器焦点。
  await openProviders(page, fixture)
  await page.getByRole("button", { name: "新增模型服务" }).first().click()
  const endpoints = page.getByRole("button", {
    name: "协议地址覆盖",
    exact: true,
  })
  const endpoint = page.locator("#provider-endpoint-openai_chat")
  await endpoints.click()
  await endpoint.fill("https://example.com/custom-chat")
  await expectClosingFieldUnfocusable(
    endpoints,
    "provider-endpoint-openai_chat"
  )
  await expect(endpoint).toHaveCount(0)
  await expect(endpoints).toBeFocused()
  await endpoints.press("Enter")
  await expect(endpoint).toHaveValue("https://example.com/custom-chat")

  // 2. 配置章节保持挂载，收起后不再进入键盘焦点顺序。
  const connection = page.getByRole("button", { name: "连接", exact: true })
  await page.getByLabel("API key", { exact: true }).fill("draft-secret")
  await expectClosingFieldUnfocusable(connection, "provider-api-key")
  await expect(endpoint).toBeHidden()
  await expect(connection).toBeFocused()
  await connection.press("Enter")
  await expect(endpoint).toHaveValue("https://example.com/custom-chat")
  await expect(page.getByLabel("API key", { exact: true })).toHaveValue(
    "draft-secret"
  )

  // 3. 只展开一个模型，关闭完成后再切回仍保留尚未完成的输入。
  await page.getByLabel("模型 ID", { exact: true }).fill("first second")
  await page.getByRole("button", { name: "添加", exact: true }).click()
  const first = page.getByRole("button", {
    name: "编辑模型 first",
    exact: true,
  })
  const second = page.getByRole("button", {
    name: "编辑模型 second",
    exact: true,
  })
  await first.click()
  await page.locator("#model-first-context_window").fill("1.")
  await expectClosingFieldUnfocusable(second, "model-first-context_window")
  await expect(first).toHaveAttribute("aria-expanded", "false")
  await expect(second).toHaveAttribute("aria-expanded", "true")
  await expect(page.locator("#model-fields-first")).toHaveCount(0)
  await expect(second).toBeFocused()
  await first.click()
  await expect(page.locator("#model-fields-second")).toHaveCount(0)
  await expect(page.locator("#model-first-context_window")).toHaveValue("1.")
})

test(
  "provider_model_fields_round_trip_aliases_and_failed_apply",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    test.setTimeout(120_000)

    const providerID = "e2e-fields"
    const list = async () =>
      callServer<ListProvidersResult>(fixture, "provider/list", {})
    const readProvider = async () =>
      (await list()).providers.find((provider) => provider.id === providerID)!
    await fixture.models(["main", "second"])
    await openProviders(page, fixture)
    await page.getByRole("button", { name: "新增模型服务" }).first().click()
    await page.getByLabel("ID", { exact: true }).fill(providerID)
    await page
      .getByLabel("base_url", { exact: true })
      .fill(fixture.modelBaseURL)
    await page.getByLabel("API key", { exact: true }).fill("field-secret")
    await selectValue(page, "协议", /^OpenAI Responses/)
    await page.getByLabel("模型 ID", { exact: true }).fill("main second")
    await page.getByRole("button", { name: "添加", exact: true }).click()

    // 1. 行内只展开一个模型，无效容量和推理列表原文在切换后保留。
    await page
      .getByRole("button", { name: "编辑模型 main", exact: true })
      .click()
    await page.getByLabel("上下文窗口", { exact: true }).fill("1.")
    await page
      .getByRole("button", { name: "编辑模型 second", exact: true })
      .click()
    await expect(page.locator("#model-fields-main")).toHaveCount(0)
    await page.getByLabel("显示名", { exact: true }).fill("Second")
    await page
      .getByRole("button", { name: "编辑模型 main", exact: true })
      .click()
    await expect(page.locator("#model-fields-second")).toHaveCount(0)
    await expect(page.getByLabel("上下文窗口", { exact: true })).toHaveValue(
      "1."
    )
    await page.getByLabel("上下文窗口", { exact: true }).fill("128K")
    await page.getByLabel("最大输出 Token", { exact: true }).fill("1.5K")
    await page.getByLabel("上游模型 ID", { exact: true }).fill("vendor/main")
    await page.locator("#model-main-protocol").click()
    await page
      .getByRole("option", { name: "Claude Messages", exact: true })
      .click()
    await page.getByLabel("显示名", { exact: true }).fill("Primary")
    await page
      .getByLabel("模型描述", { exact: true })
      .fill("复杂设计与代码审查")
    await selectValue(page, "思考", "关闭")
    await selectValue(page, "推理强度列表", "自定义")
    await page
      .getByLabel("强度（逗号分隔）", { exact: true })
      .fill("bespoke, deep")
    await selectValue(page, "默认推理强度", "deep")
    await selectValue(page, "输入类型", "自定义")
    for (const label of ["图片", "音频", "视频", "PDF"])
      await page.getByRole("checkbox", { name: label, exact: true }).check()
    await selectValue(page, "合并角色", "自定义")
    for (const label of ["系统", "开发者", "用户", "助手"])
      await page.getByRole("checkbox", { name: label, exact: true }).check()
    await page
      .getByRole("button", { name: "协议地址覆盖", exact: true })
      .click()
    await page
      .locator("#provider-endpoint-openai_chat")
      .fill(fixture.modelBaseURL + "/chat/completions")
    await page
      .locator("#provider-endpoint-openai_responses")
      .fill(fixture.modelBaseURL + "/responses")
    await page
      .locator("#provider-endpoint-claude")
      .fill(fixture.modelBaseURL + "/messages")
    await selectValue(page, "默认模型", providerID + "/main")
    await page.getByRole("button", { name: "创建", exact: true }).click()
    await expectToast(page, "已新增模型服务「" + providerID + "」")
    await page.getByRole("link", { name: new RegExp(providerID) }).click()
    await expect(page.getByLabel("ID", { exact: true })).toHaveCount(0)
    const defaultModel = page.getByRole("combobox", {
      name: "默认模型",
      exact: true,
    })
    await expect(defaultModel.locator('[data-slot="select-value"]')).toHaveText(
      providerID + "/main"
    )
    await selectValue(page, "默认模型", providerID + "/second")
    await expect(
      page.getByRole("button", { name: "应用", exact: true })
    ).toBeEnabled()
    await selectValue(page, "默认模型", providerID + "/main")
    await expect(
      page.getByRole("button", { name: "应用", exact: true })
    ).toBeDisabled()
    await expect(page.locator("#provider-endpoint-claude")).toBeVisible()
    await page
      .getByRole("button", { name: "编辑模型 main", exact: true })
      .click()
    const saved = await readProvider()
    expect(saved.models?.main).toMatchObject({
      protocol: "claude",
      upstream_model: "vendor/main",
      display_name: "Primary",
      description: "复杂设计与代码审查",
      context_window: 128000,
      max_output_tokens: 1500,
      thinking: false,
      reasoning_efforts: ["bespoke", "deep"],
      default_reasoning_effort: "deep",
      capabilities: { input: ["text", "image", "audio", "video", "pdf"] },
      merge_message_roles: ["system", "developer", "user", "assistant"],
    })

    // 2. 别名冲突旁显；拉取不覆盖高级字段；默认引用随本页别名一起保存。
    await page.getByLabel("本地别名", { exact: true }).fill("second")
    await expect(page.getByLabel("本地别名", { exact: true })).toHaveAttribute(
      "aria-invalid",
      "true"
    )
    await page.getByLabel("本地别名", { exact: true }).fill("renamed")
    await expect(defaultModel.locator('[data-slot="select-value"]')).toHaveText(
      providerID + "/renamed"
    )
    await page.getByRole("button", { name: "拉取模型", exact: true }).click()
    const dialog = page.getByRole("dialog", { name: "拉取模型", exact: true })
    await dialog.getByRole("button", { name: "确定", exact: true }).click()
    await expect(page.getByLabel("显示名", { exact: true })).toHaveValue(
      "Primary"
    )
    await expect(page.getByLabel("模型描述", { exact: true })).toHaveValue(
      "复杂设计与代码审查"
    )
    await page.getByRole("button", { name: "应用", exact: true }).click()
    await expectToast(page, "已保存模型服务「" + providerID + "」")
    expect((await list()).default_model).toBe(providerID + "/renamed")
    expect((await readProvider()).models?.renamed).toEqual(saved.models?.main)
    await page
      .getByRole("button", { name: "编辑模型 renamed", exact: true })
      .click()

    await page
      .getByRole("button", { name: "编辑模型 second", exact: true })
      .click()
    await expect(page.locator("#model-fields-renamed")).toHaveCount(0)
    await page.getByLabel("本地别名", { exact: true }).fill("friendly")
    await page.getByRole("button", { name: "应用", exact: true }).click()
    await expect(
      page.getByRole("button", { name: "应用", exact: true })
    ).toBeDisabled()
    expect((await readProvider()).models?.friendly?.upstream_model).toBe(
      "second"
    )
    await page.reload()
    await page
      .getByRole("button", { name: "编辑模型 friendly", exact: true })
      .click()
    await expect(page.getByLabel("上游模型 ID", { exact: true })).toHaveValue(
      "second"
    )
    await page
      .getByRole("button", { name: "编辑模型 renamed", exact: true })
      .click()
    await expect(page.locator("#model-fields-friendly")).toHaveCount(0)

    // 3. 显式空列表、false 和继承分别往返，空输入能力按文本处理。
    await expect(page.getByLabel("模型描述", { exact: true })).toHaveValue(
      "复杂设计与代码审查"
    )
    await page.getByLabel("模型描述", { exact: true }).fill("")
    await page.getByLabel("强度（逗号分隔）", { exact: true }).fill("")
    await selectValue(page, "默认推理强度", "自动（无）")
    for (const label of [
      "文本",
      "图片",
      "音频",
      "视频",
      "PDF",
      "系统",
      "开发者",
      "用户",
      "助手",
    ])
      await page.getByRole("checkbox", { name: label, exact: true }).uncheck()
    await expect(
      page.getByText("按文本输入处理", { exact: true })
    ).toBeVisible()
    await expect(page.getByText("不合并", { exact: true })).toBeVisible()
    await page.getByRole("button", { name: "应用", exact: true }).click()
    await expect(
      page.getByRole("button", { name: "应用", exact: true })
    ).toBeDisabled()
    const empty = (await readProvider()).models?.renamed
    expect(empty).not.toHaveProperty("description")
    expect(empty).toMatchObject({
      thinking: false,
      reasoning_efforts: [],
      merge_message_roles: [],
      capabilities: { input: [] },
    })

    await selectValue(page, "思考", "继承（开启）")
    await selectValue(page, "推理强度列表", "继承")
    await selectValue(page, "输入类型", "继承")
    await selectValue(page, "合并角色", "继承")
    await page.getByLabel("上下文窗口", { exact: true }).fill("0")
    await page.getByLabel("最大输出 Token", { exact: true }).fill("")
    await page.locator("#model-renamed-protocol").click()
    await page
      .getByRole("option", { name: "继承（OpenAI Responses）", exact: true })
      .click()
    await page.getByRole("button", { name: "应用", exact: true }).click()
    await expect(
      page.getByRole("button", { name: "应用", exact: true })
    ).toBeDisabled()
    const inherited = (await readProvider()).models?.renamed
    expect(inherited).not.toHaveProperty("description")
    for (const field of [
      "thinking",
      "context_window",
      "max_output_tokens",
      "protocol",
      "reasoning_efforts",
      "merge_message_roles",
      "capabilities",
    ])
      expect(inherited).not.toHaveProperty(field)

    // 4. 完整配置加载失败时回滚文件，编辑器保留全部草稿，错误显示在顶部。
    await page
      .locator("#provider-endpoint-claude")
      .fill("ftp://invalid.example/messages")
    await page.getByRole("button", { name: "应用", exact: true }).click()
    await expect(page.getByRole("alert")).toBeVisible()
    await expect(page.locator("#provider-endpoint-claude")).toHaveValue(
      "ftp://invalid.example/messages"
    )
    expect((await readProvider()).endpoints).toEqual(saved.endpoints)
    await page.getByRole("button", { name: "撤销", exact: true }).click()
    await expect(page.locator("#provider-endpoint-claude")).toHaveValue(
      fixture.modelBaseURL + "/messages"
    )

    // 5. 恢复默认模型后删除测试服务，不改其他 Agent 配置。
    await setDefaultModel(fixture, "local/default")
    await page.getByRole("button", { name: "删除", exact: true }).click()
    await page
      .getByRole("alertdialog")
      .getByRole("button", { name: "删除", exact: true })
      .click()
    await expectToast(page, "已删除模型服务「" + providerID + "」")
  }
)

test(
  "provider_apply_reports_busy_conversations_and_keeps_invalid_raw_dirty",
  { tag: "@p1" },
  async ({ page, fixture }) => {
    await setDefaultModel(fixture, "local/default")
    await fixture.replies({ text: "provider busy", hold: true })
    await fixture.openApp(page)
    await startConversation(page, "provider busy input")
    await expect(
      page.getByRole("button", { name: "中断", exact: true })
    ).toBeVisible()
    const editor = await page.context().newPage()
    try {
      await openProviders(editor, fixture)
      await editor.getByRole("link", { name: /local/ }).click()
      await editor
        .getByRole("button", { name: "编辑模型 default", exact: true })
        .click()
      await editor.getByLabel("上下文窗口", { exact: true }).fill("-1")
      await expect(
        editor.getByRole("button", { name: "应用", exact: true })
      ).toBeEnabled()
      await expect(
        editor.getByRole("button", { name: "撤销", exact: true })
      ).toBeEnabled()
      await editor.getByRole("button", { name: "应用", exact: true }).click()
      await expect(
        editor.getByLabel("上下文窗口", { exact: true })
      ).toHaveValue("-1")
      await editor.getByLabel("上下文窗口", { exact: true }).fill("32K")
      await editor.getByLabel("显示名", { exact: true }).fill("Busy local")
      await editor.getByRole("button", { name: "应用", exact: true }).click()
      await expectToast(
        editor,
        "1 个对话正在执行，没换上新配置；等它们空闲后再重新加载配置才会生效"
      )
    } finally {
      await page.getByRole("button", { name: "中断", exact: true }).click()
      await editor.close()
      const providers = await callServer<ListProvidersResult>(
        fixture,
        "provider/list",
        {}
      )
      const local: ProviderConfig = providers.providers.find(
        (provider) => provider.id === "local"
      )!
      delete local.models!.default!.display_name
      await callServer(fixture, "provider/set", { provider: local })
    }
  }
)
