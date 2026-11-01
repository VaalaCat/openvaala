import { rm, writeFile } from "node:fs/promises"
import path from "node:path"
import type { Locator } from "@playwright/test"
import {
  expect,
  expectShimmer,
  getItemRow,
  getComposer,
  expandTurnProcess,
  openTurnRaw,
  getMessages,
  getMessageScroller,
  startConversation,
  test,
} from "./fixture"

// 元素和它所在代码块的文字颜色：两者不同说明这段文字单独着了色
async function getColors(element: Locator) {
  return element.evaluate((node) => ({
    own: getComputedStyle(node).color,
    block: getComputedStyle(node.closest("pre") ?? document.body).color,
  }))
}

// 滚动文字区域里还有没结束的动画吗？
function isRotatingTextAnimating(locator: Locator): Promise<boolean> {
  return locator.evaluate((node) =>
    node
      .getAnimations({ subtree: true })
      .some((animation) => animation.playState === "running")
  )
}

// 过程组标题的循环动画开始时间，动作变化不应让它重新开始
function getShimmerStart(title: Locator): Promise<number | null> {
  return title.evaluate(
    (node) =>
      node
        .getAnimations()
        .find(
          (animation) => animation.effect?.getTiming().iterations === Infinity
        )?.startTime ?? null
  ) as Promise<number | null>
}

// 过程组标题行里「动作」滚动文字的选择器（不含计数标题）
const rotatingActionSelector =
  '[data-anchor-key^="group:"] > [data-item-header] [data-rotating-text]:not([data-process-counts] *)'

// 滚动文字依次出现过的一段文案
interface RotatingLabelStep {
  // 屏幕阅读器读到的文案；空串表示文案已清空
  label: string
  // 这段文案出现后，字素是否上下移动或播放过动画
  animated: boolean
  // 这段文案最后一次读到的区域宽度，动画结束后即最终宽度
  width: number
  // 文案变空时视觉上仍保留的文字，以及 400 毫秒后仍保留的文字
  visual: string
  visualLater: string | null
}

// 在页面里记录滚动文字的每次文案变化：DOM 一变就读取，持续很短的文案也不会因测试端的轮询间隔漏掉。
// 文案出现前仍在运动的字素和动画属于上一段文案，不算这段文案播放过动画。
function observeRotatingLabels(root: Element, selector: string) {
  const steps: RotatingLabelStep[] = []
  const motionBeforeSteps: Set<unknown>[] = []
  let motion = new Set<unknown>()
  const read = () => {
    const node = root.querySelector<HTMLElement>(selector)
    const label =
      node?.querySelector("[data-rotating-text-label]")?.textContent ?? ""
    if (steps.at(-1)?.label !== label) {
      const created: RotatingLabelStep = {
        label,
        animated: false,
        width: 0,
        visual:
          node?.querySelector("[data-rotating-text-current]")?.textContent ??
          "",
        visualLater: null,
      }
      if (!label)
        setTimeout(() => {
          created.visualLater =
            root
              .querySelector(selector)
              ?.querySelector("[data-rotating-text-current]")?.textContent ?? ""
        }, 400)
      steps.push(created)
      motionBeforeSteps.push(motion)
    }
    if (!node) {
      motion = new Set()
      return
    }
    const step = steps.at(-1)!
    step.width = node.getBoundingClientRect().width
    motion = new Set<unknown>([
      ...[...node.querySelectorAll("[data-rotating-text-character]")].filter(
        (character) => {
          const transform = getComputedStyle(character).transform
          return (
            transform !== "none" &&
            Math.abs(new DOMMatrixReadOnly(transform).m42) > 0.5
          )
        }
      ),
      ...node
        .getAnimations({ subtree: true })
        .filter((animation) => animation.playState === "running"),
    ])
    const earlier = motionBeforeSteps[steps.length - 1]
    if ([...motion].some((item) => !earlier.has(item))) step.animated = true
  }
  read()
  new MutationObserver(read).observe(root, {
    subtree: true,
    childList: true,
    characterData: true,
    attributes: true,
  })
  return {
    read: () => {
      read()
      return steps
    },
  }
}

// 断言这些文案按顺序都出现过（中间允许有别的文案）
function expectLabelsInOrder(steps: RotatingLabelStep[], expected: string[]) {
  const labels = steps.map((step) => step.label)
  let at = -1
  for (const label of expected) {
    at = labels.indexOf(label, at + 1)
    expect(
      at,
      `文案「${label}」应按顺序出现，实际顺序 ${JSON.stringify(labels)}`
    ).toBeGreaterThan(-1)
  }
}

// 页面里新增出现的可见 Item 或过程组
interface ObservedItem {
  text: string
  isGroup: boolean
  // 从不透明变为透明的次数：渐入一次为 1，减少动画时为 0
  fades: number
  opacity: number
  moved: boolean
}

// 在页面里记录此后新增的可见 Item 和过程组：每次 DOM 变化（含流式追加）时读取透明度和位移，
// 不做逐帧采样；clear 之后只记录更晚出现的元素。
function observeNewItems(root: Element) {
  const select = "[data-item-key], [data-anchor-key^='group:']"
  const existing = new Set<Element>(root.querySelectorAll(select))
  const tracked = new Map<
    HTMLElement,
    { isGroup: boolean; fades: number; opacity: number; moved: boolean }
  >()
  const read = () => {
    for (const element of root.querySelectorAll<HTMLElement>(select)) {
      if (existing.has(element)) continue
      const isGroup = element.dataset.anchorKey?.startsWith("group:") ?? false
      const style = getComputedStyle(isGroup ? element.parentElement! : element)
      const opacity = Number(style.opacity)
      const item = tracked.get(element) ?? {
        isGroup,
        fades: 0,
        opacity: 1,
        moved: false,
      }
      if (item.opacity >= 0.999 && opacity < 0.999) item.fades++
      item.opacity = opacity
      item.moved ||= style.transform !== "none"
      tracked.set(element, item)
    }
  }
  new MutationObserver(read).observe(root, {
    subtree: true,
    childList: true,
    characterData: true,
    attributes: true,
  })
  return {
    read: (): ObservedItem[] => {
      read()
      return [...tracked].map(([element, item]) => ({
        ...item,
        text: element.textContent ?? "",
      }))
    },
    clear: () => {
      read()
      for (const element of tracked.keys()) existing.add(element)
      tracked.clear()
    },
  }
}

test("live_visible_items_fade_once_and_existing_members_do_not", async ({
  page,
  fixture,
}) => {
  await fixture.openApp(page)
  await startConversation(page, "先建立实时订阅")
  const messages = getMessages(page)
  await expect(messages.getByText("ok", { exact: true })).toBeVisible()
  const observer = await messages.evaluateHandle(observeNewItems)
  // 本轮新增的用户消息、过程组和流式正文：各自只渐入一次，持续追加不重播，也没有位移。
  const expectLiveItems = async (userText: string, replyText: string) => {
    await expect
      .poll(() =>
        observer.evaluate((items) =>
          items.read().every((item) => item.opacity === 1)
        )
      )
      .toBe(true)
    const items = await observer.evaluate((items) => items.read())
    const group = items.find((item) => item.isGroup)
    return [
      items.find((item) => item.text.includes(userText)),
      group,
      items.find((item) => item.text.includes(replyText)),
    ]
  }

  // 1. 允许动画时，实时新增的内容各渐入一次。
  await fixture.replies(
    {
      reasoningChunks: ["新的思考", "持续追加"],
      chunkDelayMs: 200,
      tool: "read_file",
      arguments: JSON.stringify({ path: "readme.txt" }),
      finishDelayMs: 300,
    },
    { text: "实时正文持续追加而不重复入场", chunkDelayMs: 30 }
  )
  await startConversation(page, "实时新增可见消息")
  await expect(
    messages.getByText("实时正文持续追加而不重复入场", { exact: true })
  ).toBeVisible()
  for (const item of await expectLiveItems(
    "实时新增可见消息",
    "实时正文持续追加而不重复入场"
  )) {
    expect(item).toBeDefined()
    expect(item!.moved).toBe(false)
    expect(item!.fades).toBe(1)
  }

  // 2. 折叠成员首次展开时已是已有内容，不补播淡入。
  const turn = messages.locator("[data-turn-id]").last()
  await expandTurnProcess(turn)
  for (const row of await turn.locator("[data-item-key]").all()) {
    await expect(row).toHaveCSS("opacity", "1")
    expect(
      await row.evaluate((node) =>
        node
          .getAnimations()
          .some((animation) => animation.playState === "running")
      )
    ).toBe(false)
  }
  await observer.evaluate((items) => items.clear())

  // 3. 减少动画时，实时新增的内容直接显示。
  await page.emulateMedia({ reducedMotion: "reduce" })
  await fixture.replies(
    {
      reasoning: "静态思考",
      tool: "read_file",
      arguments: JSON.stringify({ path: "readme.txt" }),
    },
    { text: "减少动画时正文直接显示" }
  )
  await startConversation(page, "减少动画时新增消息")
  await expect(
    messages.getByText("减少动画时正文直接显示", { exact: true })
  ).toBeVisible()
  for (const item of await expectLiveItems(
    "减少动画时新增消息",
    "减少动画时正文直接显示"
  )) {
    expect(item).toBeDefined()
    expect(item!.moved).toBe(false)
    expect(item!.fades).toBe(0)
  }

  // 4. 允许动画时刷新从历史恢复，已有内容没有入场动画。
  await page.emulateMedia({ reducedMotion: "no-preference" })
  await page.reload()
  await expect(
    messages.getByText("减少动画时正文直接显示", { exact: true })
  ).toBeVisible()
  expect(
    await messages
      .locator("[data-item-key]")
      .evaluateAll((nodes) =>
        nodes.some((node) =>
          node
            .getAnimations()
            .some((animation) => animation.playState === "running")
        )
      )
  ).toBe(false)
})

test(
  "tool_calls_render_colors_and_json_arguments",
  { tag: "@p0" },
  async ({ page, fixture }) => {
    // 1. 模型依次调 bash（输出带 ANSI 红色）、apply_patch，最后回一句话。
    await fixture.replies(
      {
        tool: "bash",
        arguments: JSON.stringify({
          command: "printf '\\033[31mred-output\\033[0m\\n'",
        }),
      },
      {
        tool: "apply_patch",
        arguments: JSON.stringify({
          patch:
            "*** Begin Patch\n*** Add File: notes.txt\n+patched line\n*** End Patch",
        }),
      },
      { text: "工具都跑完了" }
    )
    await fixture.openApp(page)
    await startConversation(page, "跑一下工具")
    const messages = getMessages(page)
    await expect(messages.getByText("工具都跑完了")).toBeVisible()

    // 2. 展开 bash 卡片：输出里的红字和普通输出颜色不同。
    const turn = messages.locator("[data-turn-id]").last()
    await expandTurnProcess(turn)
    const bashTrigger = messages
      .locator("[data-item-key]")
      .filter({ hasText: "执行" })
      .locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
    await bashTrigger.click()
    const redOutput = messages.getByText("red-output", { exact: true })
    await expect(redOutput).toBeVisible()
    const outputColors = await getColors(redOutput)
    expect(outputColors.own).not.toBe(outputColors.block)

    // 3. 展开 apply_patch 卡片：补丁按 diff 着色，新增行单独着色。
    await messages
      .locator("[data-item-key]")
      .filter({ hasText: "修改" })
      .locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
      .click()
    const addedLine = messages.getByText("+patched line", { exact: true })
    await expect(addedLine).toBeVisible()
    await expect
      .poll(async () => {
        const colors = await getColors(addedLine)
        return colors.own !== colors.block
      })
      .toBe(true)

    // 4. 本轮原始数据保留工具调用和结果，arguments 仍可按 JSON 展开。
    const bashRow = getItemRow(bashTrigger)
    await expect(bashRow).toHaveAttribute("data-item-key", /.+/)
    const raw = await openTurnRaw(turn)
    const toolCall = raw.locator(`[data-json-path$=',"tool_call"]']`).first()
    await toolCall.press("Enter")
    await raw.getByRole("button", { name: "按 JSON 展开" }).first().click()
    await expect(
      raw.getByRole("button", { name: "按字符串显示" }).first()
    ).toBeVisible()
    await expect(raw.getByText(/"command": "printf/)).toBeVisible()
  }
)

test("output_height_uses_vertical_layout_not_line_count_or_width", async ({
  page,
  fixture,
}) => {
  const shortCode = Array.from(
    { length: 8 },
    (_, index) => `short-${index}`
  ).join("\n")
  const wideCode = "wide-" + "x".repeat(800)
  const tallCode = Array.from(
    { length: 40 },
    (_, index) => `tall-${index}`
  ).join("\n")
  await fixture.replies({
    text: [shortCode, wideCode, tallCode]
      .map((code) => `\n\`\`\`text\n${code}\n\`\`\``)
      .join("\n"),
  })
  await fixture.openApp(page)
  await startConversation(page, "输出布局")
  const blocks = getMessages(page).locator("[data-output-block]")
  await expect(blocks).toHaveCount(3)
  for (const index of [0, 1])
    await expect(
      blocks.nth(index).getByRole("button", { name: "展开输出", exact: true })
    ).toHaveCount(0)
  const wideScroll = blocks.nth(1).locator("[data-output-scroll]")
  expect(
    await wideScroll.evaluate(
      (node) =>
        node.scrollWidth > node.clientWidth &&
        node.scrollHeight === node.clientHeight
    )
  ).toBe(true)
  const tallBlock = blocks.nth(2)
  const tallScroll = tallBlock.locator("[data-output-scroll]")
  await expect(tallScroll).toHaveCSS("max-height", "320px")
  await expect(
    tallBlock.getByRole("button", { name: "展开输出", exact: true })
  ).toBeVisible()
  await tallBlock.getByRole("button", { name: "展开输出", exact: true }).click()
  expect(
    await tallScroll.evaluate((node) => node.clientHeight)
  ).toBeGreaterThan(320)
  await tallBlock.getByRole("button", { name: "收起输出", exact: true }).click()
  await expect(tallScroll).toHaveCSS("max-height", "320px")
  const copyButton = tallBlock.getByRole("button", {
    name: "复制",
    exact: true,
  })
  // 工具栏固定在所属输出块内；外层消息贴底动画不改变这个相对位置。
  const toolbarTop = await copyButton.evaluate(
    (node) =>
      node.getBoundingClientRect().top -
      node.closest("[data-output-block]")!.getBoundingClientRect().top
  )
  await tallScroll.evaluate((node) => {
    node.scrollTop = node.scrollHeight
  })
  expect(await tallScroll.evaluate((node) => node.scrollTop)).toBeGreaterThan(0)
  expect(
    await copyButton.evaluate(
      (node) =>
        node.getBoundingClientRect().top -
        node.closest("[data-output-block]")!.getBoundingClientRect().top
    )
  ).toBe(toolbarTop)
})

test("streamed_code_keeps_the_chosen_output_height", async ({
  page,
  fixture,
}) => {
  const code = Array.from({ length: 60 }, (_, index) => `line-${index}`).join(
    "\n"
  )
  await fixture.replies({
    text: `\`\`\`text\n${code}\n\`\`\`\n完成追加`,
    chunkDelayMs: 4,
  })
  await fixture.openApp(page)
  await startConversation(page, "流式代码高度")
  const block = getMessages(page).locator("[data-output-block]").first()
  await block.getByRole("button", { name: "展开输出", exact: true }).click()
  await expect(
    getMessages(page).getByText("完成追加", { exact: true })
  ).toBeVisible()
  await expect(
    block.getByRole("button", { name: "收起输出", exact: true })
  ).toBeVisible()
  expect(
    await block
      .locator("[data-output-scroll]")
      .evaluate((node) => node.clientHeight)
  ).toBeGreaterThan(1000)
})

test("json_rows_support_keyboard_selection_and_independent_node_copy", async ({
  page,
  fixture,
}) => {
  const argumentsJSON = '{ "command": "printf json-copy" }'
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  await fixture.replies(
    { tool: "bash", arguments: argumentsJSON },
    { text: "JSON就绪" }
  )
  await fixture.openApp(page)
  await startConversation(page, "JSON交互")
  await expect(getMessages(page).getByText("JSON就绪")).toBeVisible()
  const turn = getMessages(page).locator("[data-turn-id]").last()
  const raw = await openTurnRaw(turn)
  const toolCall = raw.locator(`[data-json-path$=',"tool_call"]']`).first()
  await toolCall.press("Enter")
  await expect(toolCall).toHaveAttribute("aria-expanded", "true")
  await toolCall.press("Space")
  await expect(toolCall).toHaveAttribute("aria-expanded", "false")
  await toolCall.press("Enter")
  await expect(toolCall).toHaveAttribute("aria-expanded", "true")
  // 拖选整行的文字不会折叠对象。
  await toolCall.locator("span.whitespace-pre").evaluate((node) => {
    const range = document.createRange()
    range.selectNodeContents(node)
    window.getSelection()!.removeAllRanges()
    window.getSelection()!.addRange(range)
  })
  await toolCall.dispatchEvent("click")
  await expect(toolCall).toHaveAttribute("aria-expanded", "true")
  await page.evaluate(() => window.getSelection()!.removeAllRanges())
  const args = raw
    .locator('[data-json-path$=\',"tool_call","arguments"]\']')
    .first()
  const copy = args.getByRole("button", { name: "复制这个节点", exact: true })
  for (const expanded of [false, true]) {
    if (expanded) {
      await args.hover()
      await args
        .getByRole("button", { name: "按 JSON 展开", exact: true })
        .click()
    }
    await args.hover()
    await copy.click()
    await expect
      .poll(() => page.evaluate(() => navigator.clipboard.readText()))
      .toBe(argumentsJSON)
    await expect(args.getByRole("status")).toHaveText("已复制")
  }
  const command = raw
    .locator('[data-json-path$=\',"tool_call","arguments","command"]\']')
    .first()
  await command.hover()
  await command
    .getByRole("button", { name: "复制这个节点", exact: true })
    .click()
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe("printf json-copy")
  await expect(args).toHaveAttribute("aria-expanded", "true")
})

test("command_output_follows_only_at_bottom_and_keeps_expansion_after_exit", async ({
  page,
  fixture,
}) => {
  const gates = [
    fixture.createCommandGate("output-first"),
    fixture.createCommandGate("output-second"),
    fixture.createCommandGate("output-third"),
  ]
  try {
    await fixture.openApp(page)
    await startConversation(page, "建立输出订阅")
    await expect(
      getMessages(page).getByText("ok", { exact: true })
    ).toBeVisible()
    // 命令每输出一批就停下，等测试放行后再输出下一批。
    const command = ["first", "second", "third"]
      .map(
        (name, index) =>
          `for i in $(seq 1 25); do echo ${name}-$i; done; ${gates[index].wait}`
      )
      .join("; ")
    await fixture.replies(
      { tool: "bash", arguments: JSON.stringify({ command }) },
      { text: "命令完成" }
    )
    await startConversation(page, "贴底跟随")
    const messages = getMessages(page)
    const block = messages.locator("[data-output-block]").filter({
      hasText: "$ for i in $(seq 1 25)",
    })
    const scroll = block.locator("[data-output-scroll]")
    await expandTurnProcess(messages.locator("[data-turn-id]").last())
    const commandTrigger = messages
      .locator("[data-item-key]")
      .filter({ hasText: "执行" })
      .locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
    await expect(commandTrigger).toBeVisible()
    if ((await commandTrigger.getAttribute("aria-expanded")) === "false")
      await commandTrigger.click()
    await expect(block).toContainText("first-25")
    await expect(block).toBeVisible()
    await expect
      .poll(() =>
        scroll.evaluate(
          (node) => node.scrollHeight - node.clientHeight - node.scrollTop
        )
      )
      .toBeLessThanOrEqual(2)
    // 1. 用户滚到顶部后，新输出不把视图拉回底部。
    await scroll.evaluate(
      (node) =>
        new Promise<void>((resolve) => {
          if (node.scrollTop === 0) return resolve()
          node.addEventListener("scroll", () => resolve(), { once: true })
          node.scrollTop = 0
        })
    )
    await gates[0].release()
    await expect(block.getByText(/second-25/)).toBeAttached()
    expect(await scroll.evaluate((node) => node.scrollTop)).toBe(0)
    // 2. 用户回到底部后，新输出继续贴底跟随。
    await scroll.evaluate(
      (node) =>
        new Promise<void>((resolve) => {
          const bottom = Math.max(0, node.scrollHeight - node.clientHeight)
          if (node.scrollTop === bottom) return resolve()
          node.addEventListener("scroll", () => resolve(), { once: true })
          node.scrollTop = bottom
        })
    )
    await gates[1].release()
    await expect(block.getByText(/third-25/)).toBeAttached()
    await expect
      .poll(() =>
        scroll.evaluate(
          (node) => node.scrollHeight - node.clientHeight - node.scrollTop
        )
      )
      .toBeLessThanOrEqual(2)
    // 3. 命令结束后保留用户展开选择及输出高度。
    await block.getByRole("button", { name: "展开输出", exact: true }).click()
    await gates[2].release()
    await expect(messages.getByText("命令完成", { exact: true })).toBeVisible()
    const trigger = messages
      .locator("[data-item-key]")
      .filter({ hasText: "执行" })
      .locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
    if ((await trigger.getAttribute("aria-expanded")) === "false")
      await trigger.click()
    await expect(
      block.getByRole("button", { name: "收起输出", exact: true })
    ).toBeVisible()
  } finally {
    for (const gate of gates) await gate.cleanup()
  }
})

test("read_file_tool_images_use_the_same_preview", async ({
  page,
  fixture,
}) => {
  const png =
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jewoAAAAASUVORK5CYII="
  await fixture.replies(
    {
      tool: "bash",
      arguments: JSON.stringify({
        command: `printf '${png}' | base64 -d > preview.png`,
      }),
    },
    { tool: "read_file", arguments: JSON.stringify({ path: "preview.png" }) },
    { text: "工具图片完成" }
  )
  await fixture.openApp(page)
  await startConversation(page, "工具图片")
  await expect(
    getMessages(page).getByText("工具图片完成", { exact: true })
  ).toBeVisible()
  await expandTurnProcess(getMessages(page).locator("[data-turn-id]").last())
  await getMessages(page)
    .locator("[data-item-key]")
    .filter({ hasText: "读取" })
    .locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    .click()
  await getMessages(page)
    .getByRole("button", { name: /^预览图片/ })
    .click()
  await expect(page.getByRole("dialog", { name: "图片预览" })).toBeVisible()
  await expect(page.getByRole("dialog").locator("img")).toHaveJSProperty(
    "naturalWidth",
    1
  )
  await page.keyboard.press("Escape")
})

test("process_summary_counts_tools_and_shimmers_only_while_its_group_runs", async ({
  page,
  fixture,
}) => {
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  const longPath = "latest-operation-" + "long-path-".repeat(16) + ".txt"
  const gate = fixture.createCommandGate("summary")
  const command = `${gate.wait}; exit 2`
  try {
    await fixture.replies(
      {
        reasoning: "检查工具摘要计数",
        tool: "bash",
        arguments: JSON.stringify({ command }),
      },
      {
        tool: "apply_patch",
        arguments: JSON.stringify({
          patch: `*** Begin Patch\n*** Add File: ${longPath}\n+latest summary content\n*** End Patch`,
        }),
      },
      { tool: "read_file", arguments: JSON.stringify({ path: longPath }) },
      { text: "最新操作完成" }
    )
    await fixture.openApp(page)
    await startConversation(page, "最新工具摘要")
    const turn = getMessages(page).locator("[data-turn-id]").last()
    const summary = turn.locator('[data-anchor-key^="group:"]')
    const header = summary.locator(":scope > [data-item-header]")
    const title = header.locator("[data-process-counts]")
    const action = header.locator(
      "[data-rotating-text]:not([data-process-counts] *) [data-rotating-text-current]"
    )
    await expect(summary).toHaveCount(1)
    await expect(title).toBeVisible()
    await expect(title.locator("[data-rotating-text-label]")).toHaveText(
      "执行 1 次工具，思考 1 次"
    )
    await expect(
      action.locator("xpath=..").locator("[data-rotating-text-label]")
    ).toHaveText(`执行 ${command}`)

    // 1. 命令运行期间，标题发光、动作文字不发光，桌面和窄屏布局一致。
    const expectRunningGroup = async () => {
      await expectShimmer(title)
      await expect(action).toBeVisible()
      await expect(action).toHaveCSS("animation-name", "none")
      await expect(action).toHaveCSS("font-size", "14px")
      await expect(action).toHaveCSS("line-height", "20px")
      const titleBounds = (await title.boundingBox())!
      const actionBounds = (await action.boundingBox())!
      expect(actionBounds.x - titleBounds.x - titleBounds.width).toBe(8)
      expect(actionBounds.y).toBe(titleBounds.y)
      expect(
        await action.evaluate((node) => {
          const reference = document.createElement("span")
          reference.style.color = "var(--muted-foreground)"
          node.append(reference)
          const mutedColor = getComputedStyle(reference).color
          reference.remove()
          return getComputedStyle(node).color === mutedColor
        })
      ).toBe(true)
      await expect(
        header.getByRole("button", { name: "调试信息", exact: true })
      ).toHaveCount(0)
      await expect(header.getByRole("button")).toHaveCount(1)
      await expect(title).toHaveCSS("font-size", "14px")
      await expect(title).toHaveCSS("line-height", "20px")
      await expect(header).toHaveCSS("height", "32px")
      await expect(header).toHaveCSS("padding-left", "0px")
      await expect(header).toHaveCSS("padding-right", "0px")
    }
    await expectRunningGroup()
    await page.setViewportSize({ width: 390, height: 844 })
    await expectRunningGroup()
    await page.emulateMedia({ reducedMotion: "reduce" })
    await expect(title).toHaveCSS("animation-name", "none")
    await expect(title).toBeVisible()
    await expect(action).toBeVisible()
    await page.emulateMedia({ reducedMotion: "no-preference" })

    // 2. 命令结束后，摘要只保留计数和失败标记，刷新后一致。
    await gate.release()
    await expect(
      getMessages(page).getByText("最新操作完成", { exact: true })
    ).toBeVisible()
    const summaryToggle = header.locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    const failure = header.getByRole("img", {
      name: "此组中有工具调用失败",
      exact: true,
    })
    const expectCompletedGroup = async () => {
      await expect(summary).toHaveCount(1)
      await expect(turn.locator('[data-anchor-key^="turn:"]')).toHaveCount(0)
      await expect(summaryToggle).toHaveAttribute("aria-expanded", "false")
      const completedTitle = header.locator("[data-process-counts]")
      await expect(
        completedTitle.locator("[data-rotating-text-label]")
      ).toHaveText("执行 3 次工具，思考 1 次")
      await expect(completedTitle).toHaveCSS("animation-name", "none")
      for (const text of [
        "读取",
        `执行 ${command}`,
        "修改",
        longPath,
        "bash",
        "apply_patch",
      ])
        await expect(header).not.toContainText(text)
      await expect(failure).toBeVisible()
      await expect(failure).not.toHaveAttribute("title")
      await expect(header.locator("svg title")).toHaveCount(0)
      const failureBounds = (await failure.boundingBox())!
      const titleBounds = (await completedTitle.boundingBox())!
      expect(failureBounds.x - titleBounds.x - titleBounds.width).toBe(8)
      expect(
        await getMessageScroller(page).evaluate(
          (node) => node.scrollWidth <= node.clientWidth
        )
      ).toBe(true)
    }
    await page.setViewportSize({ width: 1280, height: 844 })
    await expectCompletedGroup()
    await failure.hover()
    const tooltip = page.locator('[data-slot="tooltip-content"]:visible')
    await expect(tooltip).toHaveText("此组中有工具调用失败")
    await expect(tooltip).toHaveCSS("font-size", "12px")
    await page.mouse.move(0, 0)
    await expect(tooltip).toBeHidden()
    await page.setViewportSize({ width: 390, height: 844 })
    await expectCompletedGroup()
    await page.reload()
    await expectCompletedGroup()

    // 3. 展开摘要后，单个工具的调试数据包含调用和对应结果，关闭后展开状态不变。
    await expect(
      header.getByRole("button", { name: "调试信息", exact: true })
    ).toHaveCount(0)
    await summaryToggle.click()
    const readFile = turn.locator("[data-item-key]").filter({ hasText: "读取" })
    const readFileToggle = readFile.locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    await expect(readFileToggle).toHaveAttribute("aria-expanded", "false")
    await readFile
      .getByRole("button", { name: "调试信息", exact: true })
      .click()
    const itemDebug = page.locator('[data-debug-kind="item"]')
    await expect(itemDebug).toBeVisible()
    await itemDebug.getByRole("button", { name: "复制", exact: true }).click()
    const itemData = JSON.parse(
      await page.evaluate(() => navigator.clipboard.readText())
    )
    expect(itemData.item.kind).toBe("tool_call")
    expect(itemData.item.tool_call.name).toBe("read_file")
    expect(itemData.item.tool_call.arguments).toContain(longPath)
    expect(itemData.result.kind).toBe("tool_result")
    expect(itemData.item.tool_call.call_id).toBe(
      itemData.result.tool_result.call_id
    )
    expect(itemData).not.toHaveProperty("members")
    await page
      .getByRole("button", { name: "关闭对话详情", exact: true })
      .click()
    await expect(readFileToggle).toHaveAttribute("aria-expanded", "false")
  } finally {
    await gate.cleanup()
  }
})

test("process_actions_rotate_only_when_real_activity_changes", async ({
  page,
  fixture,
}) => {
  const first = fixture.createCommandGate("action-first")
  const next = fixture.createCommandGate("action-next")
  const firstCommand = `${first.wait}; printf first-action`
  const nextCommand = `${next.wait}; printf next-action`
  try {
    // 1. 第一条真实命令建立过程组，后续模型先输出推理，再调用 bash 与 read_file。
    await fixture.replies(
      {
        reasoning: "建立真实工具过程组",
        tool: "bash",
        arguments: JSON.stringify({ command: firstCommand }),
      },
      {
        reasoning: "继续核对工具结果",
        tool: "bash",
        toolDelayMs: 600,
        finishDelayMs: 600,
        arguments: JSON.stringify({ command: nextCommand }),
      },
      {
        tool: "read_file",
        arguments: JSON.stringify({ path: "readme.txt" }),
        finishDelayMs: 1200,
      },
      { text: "真实动作切换完成" }
    )
    await fixture.openApp(page)
    await startConversation(page, "整段工具动作")
    const turn = getMessages(page).locator("[data-turn-id]").last()
    const header = turn.locator(
      '[data-anchor-key^="group:"] > [data-item-header]'
    )
    const title = header.locator("[data-process-counts]")
    const reveal = header.locator(
      "[data-rotating-text]:not([data-process-counts] *)"
    )
    const current = reveal.locator("[data-rotating-text-current]")
    const label = current
      .locator("xpath=..")
      .locator("[data-rotating-text-label]")
    await expect(label).toHaveText(`执行 ${firstCommand}`)
    const observer = await header.evaluateHandle(
      observeRotatingLabels,
      rotatingActionSelector
    )
    const titleNode = await title.elementHandle()
    const shimmerStarted = await getShimmerStart(title)

    // 动作稳定显示后：布局不变，标题发光没有重新开始，没有残留或自行轮播的动画。
    const expectActionSettled = async (isShimmerRunning = true) => {
      await expect(header).toHaveCSS("height", "32px")
      await expect(reveal).toHaveCSS("font-size", "14px")
      await expect(reveal).toHaveCSS("line-height", "20px")
      await expect(header.getByRole("button")).toHaveCount(1)
      expect(await titleNode!.evaluate((node) => node.isConnected)).toBe(true)
      if (isShimmerRunning)
        expect(await getShimmerStart(title)).toBe(shimmerStarted)
      await expect.poll(() => isRotatingTextAnimating(reveal)).toBe(false)
      await expect
        .poll(() =>
          current.evaluate((node) => {
            const style = getComputedStyle(node)
            return {
              height: node.getBoundingClientRect().height,
              transform: style.transform,
              translate: style.translate,
              opacity: style.opacity,
              isMaskSettled:
                style.maskImage === "none" ||
                style.maskPosition.endsWith("100%"),
            }
          })
        )
        .toEqual({
          height: 20,
          transform: "none",
          translate: "none",
          opacity: "1",
          isMaskSettled: true,
        })
      const titleBounds = (await title.boundingBox())!
      const actionBounds = (await reveal.boundingBox())!
      expect(actionBounds.x - titleBounds.x - titleBounds.width).toBe(8)
      expect(actionBounds.y).toBe(titleBounds.y)
      await expect(reveal.locator("[data-rotating-text-exit]")).toHaveCount(0)
    }
    await expectActionSettled()
    await first.release()

    // 2. 推理、参数生成与执行依次切换文案；稳定显示的执行动作在桌面和窄屏都不自行轮播。
    await expect(label).toHaveText(`执行 ${nextCommand}`)
    const stepCount = (await observer.evaluate((steps) => steps.read())).length
    await expectActionSettled()
    await page.setViewportSize({ width: 390, height: 844 })
    await expectActionSettled()
    expect((await observer.evaluate((steps) => steps.read())).length).toBe(
      stepCount
    )

    // 3. 减少动画后，后续动作直接切换，不再移动字素。
    await page.emulateMedia({ reducedMotion: "reduce" })
    await next.release()
    await expect(label).toHaveText("准备读取 readme.txt")
    await expectActionSettled(false)

    // 4. 动作变空后，视觉上保留最后一个动作，直到最终回复出现。
    await expect(
      turn.getByText("真实动作切换完成", { exact: true })
    ).toBeVisible()
    await expect
      .poll(() =>
        observer.evaluate(
          (steps) =>
            steps.read().findLast((step) => step.label === "")?.visualLater ??
            null
        )
      )
      .not.toBeNull()
    const steps = await observer.evaluate((steps) => steps.read())
    expectLabelsInOrder(steps, [
      `执行 ${firstCommand}`,
      "思考中 继续核对工具结果",
      `准备执行 ${nextCommand}`,
      `执行 ${nextCommand}`,
      "准备读取 readme.txt",
      "",
    ])
    // 整句替换的第一段文案播放动画，同一阶段随后追加文字时原位补字、不重播。
    for (const prefix of ["思考中", "准备执行"]) {
      const phase = steps.filter((step) => step.label.startsWith(prefix))
      expect(phase[0].animated, `${prefix} 应播放动画`).toBe(true)
      for (const step of phase.slice(1))
        expect(step.animated, `${step.label} 是追加，不应重播`).toBe(false)
    }
    const stepOf = (text: string) =>
      steps.findLast((step) => step.label === text)!
    expect(stepOf(`执行 ${nextCommand}`).animated).toBe(true)
    // 减少动画之后的文案切换都是静态的。
    const reducedSteps = steps.slice(
      steps.findIndex((step) => step.label.startsWith("准备读取"))
    )
    expect(reducedSteps.length).toBeGreaterThan(1)
    for (const step of reducedSteps)
      expect(step.animated, `${step.label} 减少动画时应静态`).toBe(false)
    expect(stepOf("").visual).toContain("readme.txt")
    expect(stepOf("").visualLater).toContain("readme.txt")
    await expect(current).toHaveCount(0)
    await expect(
      header.getByRole("img", { name: "成功", exact: true })
    ).toHaveCount(0)
    await expect(header.locator('[data-status-tone="success"]')).toHaveCount(0)
    await expect(
      header.getByRole("button", { name: "调试信息", exact: true })
    ).toHaveCount(0)
    await expect(header).toHaveCSS("height", "32px")
    await expect(title).toHaveCSS("animation-name", "none")
    await expect(header.getByRole("button")).toHaveCount(1)
    await expect(reveal).toHaveCount(0)
  } finally {
    const interrupt = page.getByRole("button", { name: "中断", exact: true })
    if (await interrupt.isVisible()) await interrupt.click()
    await first.cleanup()
    await next.cleanup()
  }
})

test("tool_count_rolls_and_resizes_only_when_real_count_changes", async ({
  page,
  fixture,
}) => {
  const readReadme = {
    name: "read_file",
    arguments: JSON.stringify({ path: "readme.txt" }),
  }
  // 1. 一次调用 9 个工具，随后每次再调用 1 个工具，计数从 9 跨位数变到 10，再到 11。
  await fixture.replies(
    { tools: Array.from({ length: 9 }, () => readReadme) },
    {
      tool: readReadme.name,
      arguments: readReadme.arguments,
      toolDelayMs: 1200,
    },
    {
      tool: readReadme.name,
      arguments: readReadme.arguments,
      toolDelayMs: 1200,
    },
    { text: "十一次工具计数完成" }
  )
  await fixture.openApp(page)
  const countsSelector = "[data-process-counts] [data-rotating-text]"
  const observer = await page
    .locator("body")
    .evaluateHandle(
      observeRotatingLabels,
      `[data-anchor-key^="group:"] > [data-item-header] ${countsSelector}`
    )
  await startConversation(page, "跨位数工具计数")
  const turn = getMessages(page).locator("[data-turn-id]").last()
  const header = turn.locator(
    '[data-anchor-key^="group:"] > [data-item-header]'
  )
  const number = header.locator(countsSelector)
  const label = number.locator("[data-rotating-text-label]")

  // 2. 9 变 10 时数字滚动、宽度变宽；10 变 11 前改为减少动画，计数直接更新。
  await expect
    .poll(() => label.textContent(), { intervals: [50] })
    .toBe("执行 10 次工具")
  await page.emulateMedia({ reducedMotion: "reduce" })
  await expect(label).toHaveText("执行 11 次工具")
  await expect(
    turn.getByText("十一次工具计数完成", { exact: true })
  ).toBeVisible()
  const steps = await observer.evaluate((steps) => steps.read())
  expectLabelsInOrder(steps, [
    "执行 9 次工具",
    "执行 10 次工具",
    "执行 11 次工具",
  ])
  const stepOf = (text: string) => steps.find((step) => step.label === text)!
  expect(stepOf("执行 10 次工具").width).toBeGreaterThan(
    stepOf("执行 9 次工具").width
  )
  expect(stepOf("执行 10 次工具").animated).toBe(true)
  expect(stepOf("执行 11 次工具").animated).toBe(false)
  await expect(header).toContainText("执行 11 次工具")
  await expect(header.locator('[data-status-tone="success"]')).toHaveCount(0)

  // 3. 刷新后只显示计数，没有动画。
  await page.reload()
  await expect(label).toHaveText("执行 11 次工具")
  expect(
    await number.evaluate(
      (node) => node.getAnimations({ subtree: true }).length
    )
  ).toBe(0)
})

test("aggregate_preview_streams_without_restarting_rotation", async ({
  page,
  fixture,
}) => {
  const fileName = "configuration-".repeat(8) + ".txt"
  const fullPath = `aggregate-preview/project/src/lib/${fileName}`
  const shortPath = `.../src/lib/${fileName}`
  const gate = fixture.createCommandGate("preview")
  const command = `${gate.wait}; printf preview-done`
  try {
    // 1. 推理增量和同一个工具参数的增量经过真实模型流进入当前过程组；
    //    先在已有订阅上观察，新建对话的加载时间不会让前几段文案合并成一次更新。
    await fixture.openApp(page)
    await startConversation(page, "建立流式预览订阅")
    await expect(
      getMessages(page).getByText("ok", { exact: true })
    ).toBeVisible()
    const observer = await page
      .locator("body")
      .evaluateHandle(observeRotatingLabels, rotatingActionSelector)
    await fixture.replies(
      {
        reasoningChunks: ["先检查配置", "\n再检查目录", "与文件内容"],
        chunkDelayMs: 500,
        tool: "read_file",
        arguments: '{"path":',
        argumentChunks: [JSON.stringify(fullPath), "}"],
        finishDelayMs: 1800,
      },
      {
        reasoning: "核对完成",
        tool: "bash",
        arguments: JSON.stringify({ command }),
      },
      { text: "流式预览完成" }
    )
    await startConversation(page, "流式预览与路径省略")
    const turn = getMessages(page).locator("[data-turn-id]").last()
    const header = turn.locator(
      '[data-anchor-key^="group:"] > [data-item-header]'
    )
    const counts = header.locator("[data-process-counts]")
    const countsLabel = counts.locator("[data-rotating-text-label]")
    const rotation = header.locator(
      "[data-rotating-text]:not([data-process-counts] *)"
    )
    const current = rotation.locator("[data-rotating-text-current]")
    const rotationLabel = current
      .locator("xpath=..")
      .locator("[data-rotating-text-label]")
    await expect(countsLabel).toHaveText("思考 1 次")
    const firstLayer = await current.elementHandle()
    const reasoningCharacters = await counts
      .locator("[data-rotating-text-character]:not([data-rotating-text-exit])")
      .elementHandles()
    const reasoningX = (await reasoningCharacters[0].boundingBox())!.x

    // 2. 不完整 JSON 不猜参数，完整后追加末尾路径；计数原位让出空间，已有字素不重建。
    await expect(rotationLabel).toHaveText(`准备读取 ${shortPath}`)
    await expect(countsLabel).toHaveText("执行 1 次工具，思考 1 次")
    for (const character of reasoningCharacters)
      expect(await character.evaluate((node) => node.isConnected)).toBe(true)
    expect(await firstLayer!.evaluate((node) => node.isConnected)).toBe(true)
    expect((await reasoningCharacters[0].boundingBox())!.x).toBeGreaterThan(
      reasoningX
    )
    await expect.poll(() => isRotatingTextAnimating(rotation)).toBe(false)
    await expect(current).not.toHaveAttribute("title")
    const expectPreviewLayout = async () => {
      const layout = await current.evaluate((node) => {
        const style = getComputedStyle(node)
        return {
          text: node.textContent,
          overflow: style.textOverflow,
          whiteSpace: style.whiteSpace,
          clipped: node.scrollWidth > node.clientWidth,
          pageOverflow: document.documentElement.scrollWidth > innerWidth,
        }
      })
      expect(layout.text).toContain(".../src/lib/")
      expect(layout.text).not.toContain(`/project/`)
      expect(layout.overflow).toBe("ellipsis")
      expect(layout.whiteSpace).toBe("pre")
      expect(layout.clipped).toBe(true)
      expect(layout.pageOverflow).toBe(false)
    }
    await expectPreviewLayout()
    await page.setViewportSize({ width: 390, height: 844 })
    await expectPreviewLayout()
    await current.hover()
    await page.waitForTimeout(650)
    await expect(
      page.locator('[data-slot="tooltip-content"]:visible')
    ).toHaveCount(0)
    await expect(header).toHaveCSS("height", "32px")

    // 3. 工具开始执行后，计数继续增加，已有字素仍是原来的节点。
    await expect(rotationLabel).toHaveText(`执行 ${command}`)
    await expect(countsLabel).toHaveText("执行 2 次工具，思考 2 次")
    for (const character of reasoningCharacters)
      if (await character.evaluate((node) => node.textContent !== "1"))
        expect(await character.evaluate((node) => node.isConnected)).toBe(true)
    expect(await firstLayer!.evaluate((node) => node.isConnected)).toBe(true)
    await gate.release()
    await expect(turn.getByText("流式预览完成", { exact: true })).toBeVisible()
    await expect(current).toHaveCount(0)
    await expect(
      header.getByRole("img", { name: "此组中有工具调用失败", exact: true })
    ).toBeVisible()

    // 4. 追加文字原位补字不重播入场；整句替换才播放。
    const steps = await observer.evaluate((steps) => steps.read())
    expectLabelsInOrder(steps, [
      "思考中 先检查配置",
      "思考中 再检查目录",
      "思考中 再检查目录与文件内容",
      "准备读取",
      `准备读取 ${shortPath}`,
      `执行 ${command}`,
    ])
    const stepOf = (text: string) => steps.find((step) => step.label === text)!
    for (const text of ["思考中 再检查目录", "准备读取", `执行 ${command}`])
      expect(stepOf(text).animated, `${text} 应播放动画`).toBe(true)
    for (const text of ["思考中 再检查目录与文件内容", `准备读取 ${shortPath}`])
      expect(stepOf(text).animated, `${text} 是追加，不应重播`).toBe(false)

    // 5. 刷新后只显示计数，没有动画。
    await page.reload()
    await expect(countsLabel).toHaveText("执行 2 次工具，思考 2 次")
    expect(
      await counts.evaluate(
        (node) => node.getAnimations({ subtree: true }).length
      )
    ).toBe(0)
  } finally {
    const interrupt = page.getByRole("button", { name: "中断", exact: true })
    if (await interrupt.isVisible()) await interrupt.click()
    await gate.cleanup()
  }
})

test("real_hook_and_user_inputs_keep_process_groups_in_original_order", async ({
  page,
  fixture,
}) => {
  // 1. 真实 SessionStart Hook 在第一条用户输入之前执行。
  const workspace = fixture.state.workspaceDirectory
  const marker = path.join(workspace, ".display-hook-boundary")
  const first = fixture.createCommandGate("hook-first")
  const second = fixture.createCommandGate("hook-second")
  const firstCommand = `${first.wait}; printf first-group`
  const secondCommand = `${second.wait}; printf second-group`
  await writeFile(marker, "hook boundary")
  try {
    await fixture.replies(
      {
        reasoning: "第一段真实推理",
        tool: "bash",
        arguments: JSON.stringify({ command: firstCommand }),
      },
      {
        reasoning: "第二段真实推理",
        tool: "bash",
        arguments: JSON.stringify({ command: secondCommand }),
      },
      { text: "两段分组完成" }
    )
    await fixture.openApp(page)
    await startConversation(page, "Hook 后的第一条用户消息")
    const turn = getMessages(page).locator("[data-turn-id]").last()
    const groups = turn.locator('[data-anchor-key^="group:"]')
    await expect(
      groups
        .first()
        .locator(":scope > [data-item-header]")
        .locator(
          "[data-rotating-text]:not([data-process-counts] *) [data-rotating-text-label]"
        )
    ).toHaveText(`执行 ${firstCommand}`)
    await fixture.submitInput(
      page.url().split("/c/")[1],
      [{ kind: "text", text: { text: "工具之间插入的用户消息" } }],
      "steer"
    )
    await first.release()
    await expect(groups).toHaveCount(2)
    const firstHeader = groups.first().locator(":scope > [data-item-header]")
    const secondHeader = groups.last().locator(":scope > [data-item-header]")
    // 2. 后一段运行时只有后一组标题发光，前一组已完成且不保留动作。
    await expect(firstHeader.locator("[data-process-counts]")).toHaveCSS(
      "animation-name",
      "none"
    )
    await expect(
      firstHeader.locator("[data-rotating-text]:not([data-process-counts] *)")
    ).toHaveCount(0)
    await expectShimmer(secondHeader.locator("[data-process-counts]"))
    await expect(
      secondHeader.locator(
        "[data-rotating-text]:not([data-process-counts] *) [data-rotating-text-label]"
      )
    ).toHaveText(`执行 ${secondCommand}`)
    await second.release()
    await expect(turn.getByText("两段分组完成", { exact: true })).toBeVisible()
    // 3. 刷新前后，Hook、用户消息和两组过程在真实首次出现的位置。
    for (const refresh of [false, true]) {
      if (refresh) await page.reload()
      await expect(groups).toHaveCount(2)
      await expect(turn.locator('[data-anchor-key^="turn:"]')).toHaveCount(0)
      const hook = turn
        .locator("[data-item-key]")
        .filter({ hasText: "SessionStart" })
      await expect(hook).toBeVisible()
      const initialInput = turn.getByText("Hook 后的第一条用户消息", {
        exact: true,
      })
      const steeringInput = turn.getByText("工具之间插入的用户消息", {
        exact: true,
      })
      const ordered = [
        hook,
        initialInput,
        groups.first(),
        steeringInput,
        groups.last(),
      ]
      const nodes = await Promise.all(
        ordered.map((locator) => locator.elementHandle())
      )
      for (let index = 1; index < nodes.length; index++)
        expect(
          await nodes[index - 1]!.evaluate(
            (before, after) =>
              !!(
                before.compareDocumentPosition(after!) &
                Node.DOCUMENT_POSITION_FOLLOWING
              ),
            nodes[index]
          )
        ).toBe(true)
      await expect(groups.first()).not.toContainText("SessionStart")
      await expect(groups.first()).not.toContainText("Hook 后的第一条用户消息")
      await expect(groups.first()).not.toContainText("工具之间插入的用户消息")
      await expect(groups.last()).not.toContainText("工具之间插入的用户消息")
    }
  } finally {
    await rm(marker, { force: true })
    await first.cleanup()
    await second.cleanup()
  }
})

test("single_tool_debug_keeps_call_result_association_and_disclosure_independent", async ({
  page,
  fixture,
}) => {
  // 1. 单个工具也有聚合标题，成员、参数和输出分层默认收起。
  await page.context().grantPermissions(["clipboard-read", "clipboard-write"])
  await fixture.replies(
    {
      tool: "bash",
      arguments: JSON.stringify({ command: "printf single-tool-inspection" }),
    },
    { text: "单工具检查完成" }
  )
  await fixture.openApp(page)
  await startConversation(page, "核对单工具调试")
  const turn = getMessages(page).locator("[data-turn-id]").last()
  await expect(turn.getByText("单工具检查完成", { exact: true })).toBeVisible()
  const tool = turn.locator("[data-item-key]").filter({ hasText: "执行" })
  const toggle = tool.locator(
    'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
  )
  const group = turn.locator('[data-anchor-key^="group:"]')
  await expect(group).toBeVisible()
  await expect(group).toContainText("执行 1 次工具")
  await expect(
    group.getByRole("button", { name: "调试信息", exact: true })
  ).toHaveCount(0)
  await expect(toggle).toBeHidden()
  await group
    .locator(
      'button[aria-expanded="false"]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    .click()
  await expect(toggle).toBeVisible()
  await expect(toggle).toHaveAttribute("aria-expanded", "false")
  // 2. 桌面与窄屏复用同一调试入口，复制的数据包含真实调用与对应结果。
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 844 })
    await tool.getByRole("button", { name: "调试信息", exact: true }).click()
    const debug = page.locator('[data-debug-kind="item"]')
    await expect(debug).toBeVisible()
    await debug.getByRole("button", { name: "复制", exact: true }).click()
    const data = JSON.parse(
      await page.evaluate(() => navigator.clipboard.readText())
    )
    expect(data.item.kind).toBe("tool_call")
    expect(data.result.kind).toBe("tool_result")
    expect(data.item.tool_call.call_id).toBe(data.result.tool_result.call_id)
    expect(data.item.tool_call.arguments).toContain("single-tool-inspection")
    expect(data).not.toHaveProperty("members")
    await page
      .getByRole("button", { name: "关闭对话详情", exact: true })
      .click()
    await expect(toggle).toHaveAttribute("aria-expanded", "false")
    await toggle.press("Enter")
    await expect(tool.locator("[data-output-block]")).toContainText(
      "single-tool-inspection"
    )
    await toggle.press("Space")
    await expect(toggle).toHaveAttribute("aria-expanded", "false")
  }
})

test("tool_argument_summary_is_inline_without_tooltip", async ({
  page,
  fixture,
}) => {
  // 1. 真实工具输入包含长参数、多行命令、补丁或协作任务；spawn_agent 放在最后，
  //    它创建的子 Agent 不会出现在 list_agents 的结果里。
  const longName = (tool: string) =>
    `inline-${tool}-${"long-path-".repeat(14)}.txt`
  const readName = longName("read_file")
  const patchName = longName("apply_patch")
  const command = `# ${"long command argument ".repeat(12)}\nprintf full-command-details`
  const taskName = "inline-collaboration-task"
  const message = "协作消息首句。\n\n完整协作参数在展开内容中读取。"
  const readPath = path.join(fixture.state.workspaceDirectory, readName)
  const patchPath = path.join(fixture.state.workspaceDirectory, patchName)
  await writeFile(readPath, "complete file body")
  try {
    await fixture.replies(
      { tool: "list_agents", arguments: "{}" },
      {
        tools: [
          { name: "read_file", arguments: JSON.stringify({ path: readName }) },
          { name: "bash", arguments: JSON.stringify({ command }) },
          {
            name: "apply_patch",
            arguments: JSON.stringify({
              patch: `*** Begin Patch\n*** Add File: ${patchName}\n+complete patch body\n*** End Patch`,
            }),
          },
        ],
      },
      {
        tool: "spawn_agent",
        arguments: JSON.stringify({ task_name: taskName, message }),
      }
    )
    await fixture.openApp(page)
    await startConversation(page, "参数摘要")
    const turn = getMessages(page)
      .locator("[data-turn-id]")
      .filter({ has: page.getByText("参数摘要", { exact: true }) })
    await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()
    await expandTurnProcess(turn)
    const getTool = (title: string) => {
      const tool = turn.locator("[data-item-key]").filter({
        has: page.locator("[data-item-title]").filter({ hasText: title }),
      })
      const header = tool.locator("[data-item-header]")
      return {
        tool,
        header,
        title: header.locator("[data-item-title]"),
        summary: header.locator("[data-item-summary]"),
        toggle: header.locator(
          'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
        ),
      }
    }
    // 没有参数的工具不显示摘要，展开后才读到结果。
    const listAgents = getTool("查看子 Agent")
    await expect(listAgents.summary).toHaveCount(0)
    await expect(listAgents.header).not.toContainText("0 个子 Agent")
    await listAgents.toggle.click()
    await expect(
      listAgents.tool.getByText("0 个子 Agent", { exact: true })
    ).toBeVisible()

    // 2. 参数和标题在同一行，窄屏先截断参数，不增加悬停或原生提示。
    const tools = [
      {
        ...getTool("读取"),
        summaryText: readName,
        fullText: readName,
        isClipped: true,
      },
      {
        ...getTool("执行"),
        summaryText: command.split("\n")[0],
        fullText: command,
        isClipped: true,
      },
      {
        ...getTool("修改"),
        summaryText: patchName,
        fullText: patchName,
        isClipped: true,
      },
      {
        ...getTool("委派任务"),
        summaryText: taskName,
        fullText: "完整协作参数在展开内容中读取。",
        isClipped: false,
      },
    ]
    for (const item of tools)
      await expect(item.summary).toContainText(item.summaryText)
    await page.clock.install()
    await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000))
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 844 })
      await page.clock.runFor(300)
      for (const item of tools) {
        const titleBounds = (await item.title.boundingBox())!
        const summaryBounds = (await item.summary.boundingBox())!
        expect(
          Math.abs(
            titleBounds.y +
              titleBounds.height / 2 -
              summaryBounds.y -
              summaryBounds.height / 2
          )
        ).toBeLessThanOrEqual(1)
        expect(summaryBounds.x).toBeGreaterThan(
          titleBounds.x + titleBounds.width
        )
        await expect(item.header).toHaveCSS("height", "32px")
        await expect(item.summary).not.toHaveAttribute("title")
        await expect(item.summary).toHaveCSS("white-space", "nowrap")
        await expect(item.summary).toHaveCSS("text-overflow", "ellipsis")
        if (item.isClipped)
          expect(
            await item.summary.evaluate(
              (node) => node.scrollWidth > node.clientWidth
            )
          ).toBe(true)
        await expect(
          item.header.getByRole("button", { name: "调试信息", exact: true })
        ).toBeVisible()
        // 悬停摘要超过提示延迟也不弹出提示。
        if (width === 1280) {
          await item.summary.hover()
          await page.clock.runFor(700)
          await expect(
            page.locator('[data-slot="tooltip-content"]:visible')
          ).toHaveCount(0)
        }
      }
      expect(
        await getMessageScroller(page).evaluate(
          (node) => node.scrollWidth <= node.clientWidth
        )
      ).toBe(true)
    }

    // 3. 点击摘要直接展开完整实参；单独的来源链接不属于展开按钮。
    for (const item of tools) {
      await item.summary.click()
      await page.clock.runFor(300)
      await expect(item.toggle).toHaveAttribute("aria-expanded", "true")
      await expect(
        item.tool.locator('[data-slot="collapsible-content"]')
      ).toContainText(item.fullText)
      await expect(
        item.header.locator("button button, button a, button [tabindex='0']")
      ).toHaveCount(0)
    }
    const spawn = tools[3]
    await expect(
      spawn.tool
        .locator('[data-slot="collapsible-content"]')
        .getByRole("link", { name: taskName })
    ).toBeVisible()
    await expect(spawn.toggle.getByRole("link")).toHaveCount(0)
  } finally {
    await rm(readPath, { force: true })
    await rm(patchPath, { force: true })
  }
})

// 工具图标所有路径的 d 属性，形状变化时它会变
function getIconShape(icon: Locator): Promise<string> {
  return icon.evaluate((node) =>
    [...node.querySelectorAll("path")]
      .map((path) => path.getAttribute("d"))
      .join("|")
  )
}

test("process_icon_morphs_without_moving_text", async ({
  page,
  browser,
  fixture,
}) => {
  await fixture.replies({
    tool: "bash",
    arguments: JSON.stringify({ command: "printf morph-details" }),
  })
  await fixture.openApp(page)
  await startConversation(page, "真实图标变形")
  const turn = getMessages(page).locator("[data-turn-id]").last()
  await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()
  await expandTurnProcess(turn)
  await page.mouse.move(0, 0)

  // 1. 收起工具的图标和标题保持同一个布局位置。
  const tool = turn.locator("[data-item-key]").filter({ hasText: "执行" })
  const header = tool.locator("[data-item-header]")
  const title = header.locator("[data-item-title]")
  const icon = header.locator("[data-item-icon]")
  const toggle = header.locator(
    'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
  )
  const argumentSummary = header.locator("[data-item-summary]")
  const titleX = (await title.boundingBox())!.x
  const iconNode = await icon.elementHandle()
  const initialShape = await getIconShape(icon)
  const collapsedColor = await title.evaluate(
    (node) => getComputedStyle(node).color
  )
  const bodyColor = await turn
    .getByText("ok", { exact: true })
    .evaluate((node) => getComputedStyle(node).color)
  expect(collapsedColor).not.toBe(bodyColor)
  expect(
    await argumentSummary.evaluate((node) => getComputedStyle(node).color)
  ).not.toBe(collapsedColor)
  await expect(icon).toHaveCSS("width", "16px")
  await expect(icon).toHaveCSS("height", "16px")

  // 2. 鼠标悬停时图形逐步变形，标题不动；移开恢复，键盘聚焦同样变形。
  const shapes = await icon.evaluateHandle((node) => {
    const observed = { signatures: new Set<string>() }
    new MutationObserver(() => {
      observed.signatures.add(
        [...node.querySelectorAll("path")]
          .map((path) => path.getAttribute("d"))
          .join("|")
      )
    }).observe(node, {
      subtree: true,
      attributes: true,
      attributeFilter: ["d"],
    })
    return observed
  })
  await toggle.hover()
  // 变形过程中记录到至少三种不同的整图形，才是逐步变形而不是一次跳变。
  await expect
    .poll(() => shapes.evaluate((observed) => observed.signatures.size))
    .toBeGreaterThan(2)
  await expect.poll(() => getIconShape(icon)).not.toBe(initialShape)
  expect((await title.boundingBox())!.x).toBe(titleX)
  await expect
    .poll(() => title.evaluate((node) => getComputedStyle(node).color))
    .toBe(bodyColor)
  await expect(argumentSummary).toHaveCSS("color", collapsedColor)
  await page.mouse.move(0, 0)
  await expect.poll(() => getIconShape(icon)).toBe(initialShape)
  await page.keyboard.press("Tab")
  await toggle.focus()
  await expect.poll(() => getIconShape(icon)).not.toBe(initialShape)
  await toggle.press("Enter")
  await expect(toggle).toHaveAttribute("aria-expanded", "true")
  await expect(tool.locator("[data-output-block]")).toContainText(
    "morph-details"
  )
  await page.mouse.move(0, 0)
  await expect.poll(() => getIconShape(icon)).not.toBe(initialShape)
  expect((await title.boundingBox())!.x).toBe(titleX)
  await expect(title).toHaveCSS("color", bodyColor)
  await expect(argumentSummary).toHaveCSS("color", collapsedColor)
  expect(await iconNode!.evaluate((node) => node.isConnected)).toBe(true)
  await expect(
    header.locator("button button, button a, button [tabindex='0']")
  ).toHaveCount(0)

  // 3. 触屏读取同一条记录，未交互时显示相同的类型图形；一次点击展开，再点击收起。
  const touch = await browser.newPage({
    viewport: { width: 390, height: 844 },
    hasTouch: true,
    isMobile: true,
    locale: "zh-CN",
  })
  try {
    await fixture.openApp(touch)
    await touch.goto(page.url())
    expect(
      await touch.evaluate(() => navigator.maxTouchPoints)
    ).toBeGreaterThan(0)
    const touchTurn = getMessages(touch).locator("[data-turn-id]").last()
    await expect(
      touchTurn.locator('[data-turn-status="completed"]')
    ).toBeVisible()
    await expandTurnProcess(touchTurn)
    const touchTool = touchTurn
      .locator("[data-item-key]")
      .filter({ hasText: "执行" })
    const touchHeader = touchTool.locator("[data-item-header]")
    const touchTitle = touchHeader.locator("[data-item-title]")
    const touchIcon = touchHeader.locator("[data-item-icon]")
    const touchToggle = touchHeader.locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    const touchTitleX = (await touchTitle.boundingBox())!.x
    const touchIconNode = await touchIcon.elementHandle()
    expect(await getIconShape(touchIcon)).toBe(initialShape)
    await touchToggle.tap()
    await expect(touchToggle).toHaveAttribute("aria-expanded", "true")
    await expect(touchTool.locator("[data-output-block]")).toContainText(
      "morph-details"
    )
    await expect.poll(() => getIconShape(touchIcon)).not.toBe(initialShape)
    expect((await touchTitle.boundingBox())!.x).toBe(touchTitleX)
    await expect(touchTitle).toHaveCSS("color", bodyColor)
    await touchToggle.tap()
    await expect(touchToggle).toHaveAttribute("aria-expanded", "false")
    await expect.poll(() => getIconShape(touchIcon)).toBe(initialShape)
    expect(await touchIconNode!.evaluate((node) => node.isConnected)).toBe(true)
    expect((await touchTitle.boundingBox())!.x).toBe(touchTitleX)
  } finally {
    await touch.close()
  }
})

test("running_lattice_releases_summary_space_and_keeps_status_independent", async ({
  page,
  fixture,
}) => {
  const gate = fixture.createCommandGate("lattice")
  try {
    // 1. 真实执行保留状态图标，聚合文字不叠加加载格点。
    await fixture.replies({
      tool: "bash",
      arguments: JSON.stringify({
        command: `${gate.wait}; printf lattice-completed`,
      }),
    })
    await fixture.openApp(page)
    await startConversation(page, "格点和状态占位")
    const turn = getMessages(page).locator("[data-turn-id]").last()
    await expandTurnProcess(turn)
    const tool = turn.locator("[data-item-key]").filter({ hasText: "执行" })
    const header = tool.locator("[data-item-header]")
    const title = header.locator("[data-item-title]")
    const summary = header.locator("[data-item-summary]")
    const status = header.getByRole("img", { name: "执行中", exact: true })
    const toggle = header.locator(
      'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
    )
    await expect(status).toBeVisible()
    const runningGap =
      (await summary.boundingBox())!.x -
      (await title.boundingBox())!.x -
      (await title.boundingBox())!.width
    const circles = status.locator("svg circle")
    await expect(circles).toHaveCount(9)
    // 动画正在运行、且关键帧里有不同亮度的格点才算在循环变亮。
    const countAnimatedCircles = () =>
      circles.evaluateAll(
        (nodes) =>
          nodes.filter((node) =>
            node
              .getAnimations()
              .some(
                (animation) =>
                  animation.playState === "running" &&
                  new Set(
                    (animation.effect as KeyframeEffect)
                      .getKeyframes()
                      .map((keyframe) => keyframe.opacity)
                  ).size > 1
              )
          ).length
      )
    // 允许动画时八个格点循环变亮，中间的空位不动；减少动画时全部静态。
    expect(await countAnimatedCircles()).toBe(8)
    await page.emulateMedia({ reducedMotion: "reduce" })
    await expect.poll(countAnimatedCircles).toBe(0)
    await expect(status.locator("svg")).toHaveCSS("width", "16px")
    await expect(status.locator("svg")).toHaveCSS("height", "16px")
    await expect(
      turn.locator(
        '[data-anchor-key^="group:"] > [data-item-header] svg circle'
      )
    ).toHaveCount(0)
    await page.keyboard.press("Tab")
    await status.focus()
    await expect(
      page.locator('[data-slot="tooltip-content"]:visible')
    ).toHaveText("执行中")
    await expect(toggle).toHaveAttribute("aria-expanded", "false")

    // 2. 成功后卸载状态，参数收回图标和间距，详情仍能展开。
    await gate.release()
    await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()
    await expect(status).toHaveCount(0)
    await page.mouse.move(0, 0)
    const completedGap =
      (await summary.boundingBox())!.x -
      (await title.boundingBox())!.x -
      (await title.boundingBox())!.width
    expect(runningGap - completedGap).toBe(24)
    expect(completedGap).toBe(8)
    await expect(header.getByRole("img")).toHaveCount(0)
    await toggle.click()
    await expect(tool.locator("[data-output-block]")).toContainText(
      "lattice-completed"
    )
  } finally {
    await gate.cleanup()
  }
})

test("tool_text_colors_follow_collapsed_hover_and_expanded_states", async ({
  page,
  fixture,
}) => {
  // 1. 两个真实工具完成后展开过程组，工具详情保持收起。
  await page.emulateMedia({ reducedMotion: "reduce" })
  await fixture.replies(
    {
      tool: "bash",
      arguments: JSON.stringify({ command: "printf color-details" }),
    },
    { tool: "read_file", arguments: JSON.stringify({ path: "readme.txt" }) },
    { text: "工具颜色检查完成，正文颜色保持不变。" }
  )
  await fixture.openApp(page)
  await startConversation(page, "工具文字层级")
  const turn = getMessages(page).locator("[data-turn-id]").last()
  await expect(turn.locator('[data-turn-status="completed"]')).toBeVisible()
  const body = turn.getByText("工具颜色检查完成，正文颜色保持不变。", {
    exact: true,
  })
  await expandTurnProcess(turn)

  // 2. 浅色和深色主题下，收起标题、参数分别使用次要和辅助文字色。
  for (const scenario of [
    {
      colorScheme: "light",
      secondary: "rgb(113, 113, 113)",
      muted: "rgb(135, 135, 135)",
      foreground: "oklch(0.147 0.004 49.25)",
    },
    {
      colorScheme: "dark",
      secondary: "rgb(161, 161, 161)",
      muted: "rgb(135, 135, 135)",
      foreground: "oklch(0.985 0.001 106.423)",
    },
  ] as const) {
    await page.emulateMedia({ colorScheme: scenario.colorScheme })
    await getComposer(page).focus()
    await page.mouse.move(0, 0)
    await expect(body).toHaveCSS("color", scenario.foreground)
    for (const tool of [
      { title: "执行", summary: "printf color-details" },
      { title: "读取", summary: "readme.txt" },
    ]) {
      const row = turn.locator("[data-item-key]").filter({
        has: page.locator("[data-item-title]").filter({ hasText: tool.title }),
      })
      const header = row.locator("[data-item-header]")
      const title = header.locator("[data-item-title]")
      const summary = header.locator("[data-item-summary]")
      const toggle = header.locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
      await expect(toggle).toHaveAttribute("aria-expanded", "false")
      await expect(summary).toHaveText(tool.summary)
      await expect(title).toHaveCSS("color", scenario.secondary)
      await expect(summary).toHaveCSS("color", scenario.muted)
    }

    // 3. 悬停和展开仅提高工具标题与参数层级，正文仍为原前景色。
    for (const toolTitle of ["执行", "读取"]) {
      const row = turn.locator("[data-item-key]").filter({
        has: page.locator("[data-item-title]").filter({ hasText: toolTitle }),
      })
      const header = row.locator("[data-item-header]")
      const title = header.locator("[data-item-title]")
      const summary = header.locator("[data-item-summary]")
      const toggle = header.locator(
        'button[aria-expanded]:has([data-item-title]):not([tabindex="-1"]):visible'
      )
      await toggle.hover()
      await expect(title).toHaveCSS("color", scenario.foreground)
      await expect(summary).toHaveCSS("color", scenario.secondary)
      await expect(body).toHaveCSS("color", scenario.foreground)
      await toggle.click()
      await page.mouse.move(0, 0)
      await getComposer(page).focus()
      await expect(toggle).toHaveAttribute("aria-expanded", "true")
      await expect(title).toHaveCSS("color", scenario.foreground)
      await expect(summary).toHaveCSS("color", scenario.secondary)
      const outputs = row.locator("[data-output-block]")
      await expect(outputs).toHaveCount(toolTitle === "读取" ? 2 : 1)
      for (const output of await outputs.all())
        await expect(output).toBeVisible()
      await expect(body).toHaveCSS("color", scenario.foreground)
      await toggle.click()
      await page.mouse.move(0, 0)
      await getComposer(page).focus()
      await expect(title).toHaveCSS("color", scenario.secondary)
      await expect(summary).toHaveCSS("color", scenario.muted)
    }
  }
})
