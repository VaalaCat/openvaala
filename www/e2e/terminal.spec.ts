import path from "node:path"
import type { Locator, Page } from "@playwright/test"
import {
  closeTerminalTabs,
  controlConnection,
  expect,
  expectDocumentNotScrollable,
  getComposer,
  pasteIntoTerminal,
  startConversation,
  test,
  type WebUIFixture,
} from "./fixture"

// 标签栏里的标签按钮；右栏的页签也是 tab 角色，用 id 前缀区分
function getTabButtons(page: Page): Locator {
  return page.locator('[role="tab"][id^="flexlayout-tabbutton-"]')
}

function getAreas(page: Page): Locator {
  return page.locator(".flexlayout__tabset")
}

// 终端的屏幕：xterm 的 DOM 渲染把每一行画成一个元素；有几个终端同时显示时用 scope 限定在某个终端里
function getScreen(scope: Page | Locator): Locator {
  return scope.locator(".xterm-rows:visible")
}

// 终端结束后右下角的结束原因和「重新启动」
function getExitNotice(page: Page): Locator {
  return page.getByRole("status").filter({ hasText: "重新启动" })
}

// 连接断开时终端右下角的「连接已断开，正在重连」
function getReconnectingNotice(page: Page): Locator {
  return page.getByRole("status").filter({ hasText: "连接已断开，正在重连" })
}

// 区域「+」菜单里重置终端
async function resetTerminalFromAreaMenu(page: Page): Promise<void> {
  await getAreas(page)
    .first()
    .getByRole("button", { name: "区域操作", exact: true })
    .click()
  await page.getByRole("menuitem", { name: "重置终端", exact: true }).click()
}

// 刷新后终端标签页不一定是选中的，点它
async function selectLastTab(page: Page): Promise<void> {
  await getTabButtons(page).last().click()
  await page.locator(".terminal-viewport:visible").click()
}

// 屏幕上最后一行 tick：后台循环输出的 shell 进程号和序号
async function readLastTick(
  page: Page
): Promise<{ pid: number; count: number } | undefined> {
  const text = await getScreen(page).innerText()
  const last = [...text.matchAll(/tick (\d+) (\d+)/g)].at(-1)
  return last ? { pid: Number(last[1]), count: Number(last[2]) } : undefined
}

// 区域「+」菜单里新开终端
async function openTerminalFromAreaMenu(page: Page): Promise<void> {
  await getAreas(page)
    .first()
    .getByRole("button", { name: "区域操作", exact: true })
    .click()
  await page.getByRole("menuitem", { name: "新终端", exact: true }).click()
}

// 在终端里输入一行命令并回车；键盘输入落在 xterm 的输入框上
async function runCommand(page: Page, command: string): Promise<void> {
  await page.keyboard.type(command)
  await page.keyboard.press("Enter")
}

// 终端里的行列数：命令 echo "size=$(stty size)" 输出的最后一次
async function readTerminalSize(
  page: Page,
  scope: Page | Locator = page
): Promise<{ rows: number; cols: number }> {
  await runCommand(page, 'echo "size=$(stty size)"')
  const sizes = () =>
    getScreen(scope)
      .innerText()
      .then((text) => [...text.matchAll(/size=(\d+) (\d+)/g)])
  await expect.poll(async () => (await sizes()).length).toBeGreaterThan(0)
  const last = (await sizes()).at(-1)!
  return { rows: Number(last[1]), cols: Number(last[2]) }
}

// 对话已建好，终端开在这条对话的工作目录里
async function startConversationWithTerminal(
  page: Page,
  fixture: WebUIFixture
): Promise<void> {
  await fixture.replies({ text: "好的" })
  await fixture.openApp(page)
  await startConversation(page, "终端用的对话")
  await openTerminalFromAreaMenu(page)
  await expect(page.locator(".xterm-helper-textarea:visible")).toBeFocused()
}

test.describe("desktop", () => {
  test.use({ viewport: { width: 1280, height: 800 } })

  // 终端归 serve 持有，页面关掉后 shell 还在运行；用例结束时关掉标签页来结束它们
  test.afterEach(async ({ page }) => {
    await closeTerminalTabs(page).catch(() => undefined)
  })

  test("terminal_opens_in_the_conversation_work_directory_and_takes_input", async ({
    page,
    fixture,
  }) => {
    // 1. 新终端开在这条对话的 local 工作目录，标题是「Executor · 目录名」，打开后键盘焦点就在终端里。
    await startConversationWithTerminal(page, fixture)
    const directory = fixture.state.workspaceDirectory
    await expect(getTabButtons(page).last()).toHaveText(
      `local · ${path.basename(directory)}`
    )
    await expect(getTabButtons(page).last()).toHaveAttribute(
      "aria-selected",
      "true"
    )
    await expect(page.getByRole("region", { name: "终端" })).toBeVisible()
    // 2. 不用先点终端，直接输入 pwd，看到对话的工作目录。
    await runCommand(page, "pwd")
    await expect(getScreen(page)).toContainText(directory)
  })

  test("terminal_follows_the_area_size", async ({ page, fixture }) => {
    // 1. 打开后行列数按区域排好，不再是起始的 24×80。
    await startConversationWithTerminal(page, fixture)
    await expect(getScreen(page)).toBeVisible()
    await expect
      .poll(async () => (await readTerminalSize(page)).rows, {
        message: "terminal is fitted to the area",
      })
      .toBeGreaterThan(24)
    const wide = await readTerminalSize(page)
    // 2. 向右分栏（新区域里另开一个终端）后原来的终端所在区域变窄，stty size 的列数变小。
    const original = page.locator(".terminal-viewport").first()
    await getAreas(page)
      .first()
      .getByRole("button", { name: "区域操作", exact: true })
      .click()
    await page.getByRole("menuitem", { name: "向右分栏", exact: true }).click()
    await expect(getAreas(page)).toHaveCount(2)
    await original.click()
    await expect
      .poll(async () => (await readTerminalSize(page, original)).cols)
      .toBeLessThan(wide.cols)
    // 3. 把浏览器窗口调矮，行数变小。
    const narrow = await readTerminalSize(page, original)
    await page.setViewportSize({ width: 1280, height: 500 })
    await expect
      .poll(async () => (await readTerminalSize(page, original)).rows)
      .toBeLessThan(narrow.rows)
  })

  test("exited_terminal_can_be_restarted_in_the_same_tab", async ({
    page,
    fixture,
  }) => {
    // 1. exit 后标签页保留，屏幕内容还在，右下角显示退出原因和「重新启动」。
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "echo before-exit")
    await expect(getScreen(page)).toContainText("before-exit")
    await runCommand(page, "exit")
    await expect(getExitNotice(page)).toContainText("终端已退出，退出码 0")
    await expect(getScreen(page)).toContainText("before-exit")
    await expect(getTabButtons(page)).toHaveCount(2)
    // 2. 重新启动：同一个标签页换成新的 shell，旧屏幕清空，可以继续输入。
    await getExitNotice(page)
      .getByRole("button", { name: "重新启动", exact: true })
      .click()
    await expect(getExitNotice(page)).toHaveCount(0)
    await expect(getTabButtons(page)).toHaveCount(2)
    await runCommand(page, "echo $((6*7))")
    await expect(getScreen(page)).toContainText("42")
    await expect(getScreen(page)).not.toContainText("before-exit")
  })

  test("reload_keeps_the_same_shell", async ({ page, fixture }) => {
    // 1. 后台循环每 0.3 秒输出 shell 的进程号和序号，刷新前记下最后一行。
    await startConversationWithTerminal(page, fixture)
    await runCommand(
      page,
      'i=0; while :; do i=$((i+1)); echo "tick $$ $i"; sleep 0.3; done &'
    )
    await expect
      .poll(async () => (await readLastTick(page))?.count)
      .toBeGreaterThan(3)
    const before = (await readLastTick(page))!
    // 2. 刷新后标签页接回同一个 shell：进程号不变，循环没有停，不显示结束。
    await page.reload()
    await selectLastTab(page)
    await expect
      .poll(async () => (await readLastTick(page))?.count)
      .toBeGreaterThan(before.count + 3)
    expect((await readLastTick(page))!.pid).toBe(before.pid)
    await expect(getExitNotice(page)).toHaveCount(0)
    await expect(getReconnectingNotice(page)).toHaveCount(0)
    // 3. 接回的终端可以继续输入。
    await runCommand(page, 'echo "alive $$"')
    await expect(getScreen(page)).toContainText(`alive ${before.pid}`)
  })

  test("disconnect_shows_reconnecting", async ({ page, fixture }) => {
    // 1. 终端里让后台任务两秒后输出一行，随即断开页面的连接。
    const connection = await controlConnection(page)
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "(sleep 2; echo offline-$((6*7))) &")
    await connection.disconnect()
    // 2. 终端显示正在重连，不能输入。
    await expect(getReconnectingNotice(page)).toBeVisible()
    await expect(getExitNotice(page)).toHaveCount(0)
    await page.waitForTimeout(2500)
    // 3. 连上后断线期间的输出补齐，提示消失，可以继续输入。
    await connection.reconnect()
    await expect(getScreen(page)).toContainText("offline-42")
    await expect(getReconnectingNotice(page)).toHaveCount(0)
    await page.locator(".terminal-viewport:visible").click()
    await runCommand(page, "echo after-$((1+1))")
    await expect(getScreen(page)).toContainText("after-2")
  })

  test("reconnect_does_not_steal_focus", async ({ page, fixture }) => {
    // 1. 主对话在左边的区域，终端在右边的区域；终端标签页是选中的，用户却在主对话的输入框里打字。
    const connection = await controlConnection(page)
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "重连不抢焦点")
    await getAreas(page)
      .first()
      .getByRole("button", { name: "区域操作", exact: true })
      .click()
    await page.getByRole("menuitem", { name: "向右分栏", exact: true }).click()
    await expect(getAreas(page)).toHaveCount(2)
    await expect(page.locator(".xterm-helper-textarea:visible")).toBeFocused()
    await getComposer(page).focus()
    await page.keyboard.type("正在输入")
    // 2. 断开再恢复连接：终端接回原来的 shell，焦点还在输入框，后面的按键也落在输入框里。
    await connection.disconnect()
    await expect(getReconnectingNotice(page)).toBeVisible()
    await connection.allowReconnect()
    await expect(getReconnectingNotice(page)).toHaveCount(0)
    // 没有可观察的界面变化说明终端已经接回，等一会儿，抢焦点发生在接回的那一刻。
    await page.waitForTimeout(500)
    await expect(getComposer(page)).toBeFocused()
    await page.keyboard.type("继续")
    await expect(getComposer(page)).toHaveValue("正在输入继续")
  })

  test("shell_exits_while_disconnected", async ({ page, fixture }) => {
    // 1. shell 两秒后以退出码 7 退出，退出时页面的连接是断开的。
    const connection = await controlConnection(page)
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "sleep 2; exit 7")
    await connection.disconnect()
    await expect(getReconnectingNotice(page)).toBeVisible()
    await page.waitForTimeout(2500)
    // 2. 重连后显示退出码，并提供重新启动。
    await connection.reconnect()
    await expect(getExitNotice(page)).toContainText("终端已退出，退出码 7")
    await expect(
      getExitNotice(page).getByRole("button", { name: "重新启动", exact: true })
    ).toBeVisible()
  })

  test("replay_does_not_answer_queries", async ({ page, fixture }) => {
    // 1. 前台程序 cat -v 逐字节显示收到的输入（关掉回显和行缓冲）；输出里的光标位置查询 ESC[6n 被 xterm 回答一次。
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "stty -icanon -echo")
    await runCommand(page, "(sleep 1; printf '\\033[6n') &")
    await runCommand(page, "cat -v")
    const answers = async () =>
      ((await getScreen(page).innerText()).match(/\^\[\[\d+;\d+R/g) ?? [])
        .length
    await expect.poll(answers).toBe(1)
    // 2. 刷新后回放同样的输出，xterm 的回答不能再发给 cat：键入一个字符作为标记，它之前不应出现第二个回答。
    await page.reload()
    await selectLastTab(page)
    await expect.poll(answers).toBe(1)
    await page.keyboard.type("marker")
    await expect(getScreen(page)).toContainText("marker")
    expect(await answers()).toBe(1)
  })

  test("reload_after_overflow", async ({ page, fixture }) => {
    // 1. 输出超过保留的 1 MiB（seq 1 200000 约 1.4 MB）。
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "seq 1 200000; echo finished-$((6*7))")
    await expect(getScreen(page)).toContainText("finished-42", {
      timeout: 60_000,
    })
    // 2. 刷新后屏幕是末尾的输出，shell 仍然可用。
    await page.reload()
    await selectLastTab(page)
    await expect(getScreen(page)).toContainText("finished-42", {
      timeout: 30_000,
    })
    await expect(getScreen(page)).toContainText("199999")
    await expect(getExitNotice(page)).toHaveCount(0)
    await runCommand(page, "echo usable-$((1+1))")
    await expect(getScreen(page)).toContainText("usable-2")
  })

  test("reset_clears_screen_and_history", async ({ page, fixture }) => {
    // 1. 打开括号粘贴模式，输出一行。
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "printf '\\033[?2004h'")
    await runCommand(page, "echo before-reset-$((6*7))")
    await expect(getScreen(page)).toContainText("before-reset-42")
    // 2. 主对话标签页没有「重置终端」，终端标签页有；重置后屏幕清空。
    await getTabButtons(page).first().click()
    await getAreas(page)
      .first()
      .getByRole("button", { name: "区域操作", exact: true })
      .click()
    await expect(
      page.getByRole("menuitem", { name: "新终端", exact: true })
    ).toBeVisible()
    await expect(
      page.getByRole("menuitem", { name: "重置终端", exact: true })
    ).toHaveCount(0)
    await page.keyboard.press("Escape")
    await selectLastTab(page)
    await resetTerminalFromAreaMenu(page)
    await expect(getScreen(page)).not.toContainText("before-reset-42")
    // 3. shell 照常可用，括号粘贴模式保留：cat -v 收到的粘贴内容被转义序列包起来。
    await page.locator(".terminal-viewport:visible").click()
    await runCommand(page, "cat -v")
    await pasteIntoTerminal(page, "pasted")
    await expect(getScreen(page)).toContainText("^[[200~pasted^[[201~")
    await page.keyboard.press("Control+C")
    // 4. 刷新后不再回放被清掉的内容。
    await runCommand(page, "echo after-reset-$((1+1))")
    await expect(getScreen(page)).toContainText("after-reset-2")
    await page.reload()
    await selectLastTab(page)
    await expect(getScreen(page)).toContainText("after-reset-2")
    await expect(getScreen(page)).not.toContainText("before-reset-42")
  })

  test("reset_redraws_the_prompt_of_an_interactive_shell", async ({
    page,
    fixture,
  }) => {
    // 1. 换成带 readline 的 bash：改尺寸后它会重画提示符。
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "PS1='ready> ' exec bash --norc --noprofile -i")
    await runCommand(page, "echo before-reset-$((6*7))")
    await expect(getScreen(page)).toContainText("before-reset-42")
    // 2. 重置后屏幕清空，提示符重画在顶部。
    await resetTerminalFromAreaMenu(page)
    await expect(getScreen(page)).not.toContainText("before-reset-42")
    await expect(getScreen(page)).toContainText("ready>")
    // 3. 刷新后不回放被清掉的内容，提示符还在。
    await page.reload()
    await selectLastTab(page)
    await expect(getScreen(page)).toContainText("ready>")
    await expect(getScreen(page)).not.toContainText("before-reset-42")
  })

  test("server_restart_marks_terminal_ended", async ({ page, fixture }) => {
    // 1. serve 重启后原来的 shell 不在了，重新连上后标签页显示终端已结束。
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "echo before-restart-$((6*7))")
    await expect(getScreen(page)).toContainText("before-restart-42")
    await fixture.restart()
    await expect(getExitNotice(page)).toContainText("终端已结束", {
      timeout: 30_000,
    })
    await expect(getScreen(page)).toContainText("before-restart-42")
    // 2. 重新启动后在同一个标签页得到新的 shell。
    await getExitNotice(page)
      .getByRole("button", { name: "重新启动", exact: true })
      .click()
    await expect(getExitNotice(page)).toHaveCount(0)
    await runCommand(page, "echo restarted-$((1+1))")
    await expect(getScreen(page)).toContainText("restarted-2")
  })

  test("terminal_has_no_key_toolbar_on_wide_screens", async ({
    page,
    fixture,
  }) => {
    await startConversationWithTerminal(page, fixture)
    await expect(getScreen(page)).toBeVisible()
    await expect(page.getByRole("toolbar", { name: "终端按键" })).toBeHidden()
  })

  test("terminal_appearance_changes_resize_the_existing_shell", async ({
    page,
    fixture,
  }) => {
    // 1. 用未导出的 shell 变量标记进程，调整外观不能丢掉变量或输出。
    await startConversationWithTerminal(page, fixture)
    await runCommand(page, "appearance_keep=alive; echo kept_before_appearance")
    await expect(getScreen(page)).toContainText("kept_before_appearance")
    const original = await readTerminalSize(page)
    const settings = await page.context().newPage()
    try {
      await settings.goto(page.url().split("/c/")[0] + "/settings/appearance")
      const group = settings.getByRole("region", { name: "终端", exact: true })
      for (const change of [
        { label: "字号", value: "20" },
        { label: "行高", value: "1.6" },
        { label: "字间距", value: "2" },
        { label: "内容内边距", value: "24" },
      ])
        await group
          .getByRole("spinbutton", { name: change.label, exact: true })
          .fill(change.value)
      await page.bringToFront()
      await page.locator(".terminal-viewport").click()
      await expect(page.locator(".terminal-viewport .xterm")).toHaveCSS(
        "padding",
        "24px"
      )
      await expect
        .poll(async () => (await readTerminalSize(page)).rows)
        .toBeLessThan(original.rows)
      const changed = await readTerminalSize(page)
      expect(changed.cols).toBeLessThan(original.cols)
      await runCommand(page, 'echo "state=$appearance_keep"')
      await expect(getScreen(page)).toContainText("state=alive")
      await expect(getScreen(page)).toContainText("kept_before_appearance")
      await group
        .getByRole("button", { name: "恢复本组默认", exact: true })
        .click()
      await page.bringToFront()
      await page.locator(".terminal-viewport").click()
      await expect
        .poll(async () => (await readTerminalSize(page)).rows)
        .toBe(original.rows)
      // 2. 隐藏的终端再次显示时同样按新字号 fit，shell 仍是原进程。
      await getTabButtons(page).first().click()
      await group
        .getByRole("spinbutton", { name: "字号", exact: true })
        .fill("20")
      await getTabButtons(page).last().click()
      await page.locator(".terminal-viewport:visible").click()
      await expect
        .poll(async () => (await readTerminalSize(page)).rows)
        .toBeLessThan(original.rows)
      await runCommand(page, 'echo "hidden_state=$appearance_keep"')
      await expect(getScreen(page)).toContainText("hidden_state=alive")
    } finally {
      await settings.close()
    }
  })
})

test.describe("narrow", () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test("terminal_follows_the_visual_viewport_without_resizing_the_page", async ({
    page,
    fixture,
  }) => {
    // 1. 打开真实终端，记录键盘弹出前的行数。
    await fixture.openApp(page)
    await startConversation(page, "终端键盘高度")
    await page.getByRole("button", { name: "标签页列表", exact: true }).click()
    const drawer = page.getByRole("dialog", { name: "标签页", exact: true })
    await drawer.getByRole("button", { name: "新终端", exact: true }).click()
    await expect(drawer).toBeHidden()
    const terminal = page.getByRole("region", { name: "终端", exact: true })
    const toolbar = page.getByRole("toolbar", { name: "终端按键" })
    const header = page
      .locator("header")
      .filter({ has: page.getByRole("navigation") })
    await terminal.click()
    await expect
      .poll(async () => (await readTerminalSize(page)).rows)
      .toBeGreaterThan(24)
    const initialSize = await readTerminalSize(page)

    // 2. 模拟 iOS 只缩小、平移 visualViewport；布局视口始终保持 844px。
    // 原生软键盘动画仍需真机确认，这里检查真实页面布局和 PTY 行数。
    let previousSize = initialSize
    let previousHeight = 844
    for (const frame of [
      { height: 520, offsetTop: 0, event: "resize" },
      { height: 360, offsetTop: 48, event: "resize" },
      { height: 360, offsetTop: 96, event: "scroll" },
      { height: 844, offsetTop: 0, event: "resize" },
    ]) {
      await page.evaluate(({ height, offsetTop, event }) => {
        const viewport = window.visualViewport!
        Object.defineProperties(viewport, {
          height: { configurable: true, value: height },
          offsetTop: { configurable: true, value: offsetTop },
        })
        viewport.dispatchEvent(new Event(event))
      }, frame)
      expect(await page.evaluate(() => innerHeight)).toBe(844)
      await expect
        .poll(async () => (await header.boundingBox())!.y)
        .toBe(frame.offsetTop)
      await expect
        .poll(async () => {
          const box = (await toolbar.boundingBox())!
          return box.y + box.height
        })
        .toBe(frame.offsetTop + frame.height)
      await expectDocumentNotScrollable(page)
      await expect(page.locator(".xterm-helper-textarea:visible")).toBeFocused()
      if (frame.height < previousHeight)
        await expect
          .poll(async () => (await readTerminalSize(page)).rows)
          .toBeLessThan(previousSize.rows)
      else if (frame.height === previousHeight)
        await expect
          .poll(async () => (await readTerminalSize(page)).rows)
          .toBe(previousSize.rows)
      else
        await expect
          .poll(async () => (await readTerminalSize(page)).rows)
          .toBe(initialSize.rows)
      const terminalBox = (await terminal.boundingBox())!
      expect(terminalBox.y).toBeGreaterThanOrEqual(frame.offsetTop + 40)
      expect(terminalBox.y + terminalBox.height).toBe(
        (await toolbar.boundingBox())!.y
      )
      previousSize = await readTerminalSize(page)
      previousHeight = frame.height
    }

    // 3. 手动放大改变可见区域时，不把页面重新排成更小的终端。
    await page.evaluate(() => {
      const viewport = window.visualViewport!
      Object.defineProperties(viewport, {
        height: { configurable: true, value: 422 },
        offsetTop: { configurable: true, value: 100 },
        scale: { configurable: true, value: 2 },
      })
      viewport.dispatchEvent(new Event("resize"))
      viewport.dispatchEvent(new Event("scroll"))
    })
    expect((await header.boundingBox())!.y).toBe(0)
    const toolbarBox = (await toolbar.boundingBox())!
    expect(toolbarBox.y + toolbarBox.height).toBe(844)
  })

  test("narrow_tab_list_resets_a_running_terminal", async ({
    page,
    fixture,
  }) => {
    // 1. 标签页列表里当前是对话时没有「重置终端」；新开终端后有。
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "窄屏重置终端")
    const tabsButton = page.getByRole("button", {
      name: "标签页列表",
      exact: true,
    })
    await tabsButton.click()
    const drawer = page.getByRole("dialog", { name: "标签页", exact: true })
    await expect(
      drawer.getByRole("button", { name: "重置终端", exact: true })
    ).toHaveCount(0)
    await drawer.getByRole("button", { name: "新终端", exact: true }).click()
    await expect(drawer).toBeHidden()
    await page.locator(".terminal-viewport").click()
    await runCommand(page, "echo narrow-before-reset-$((6*7))")
    await expect(getScreen(page)).toContainText("narrow-before-reset-42")
    // 2. 重置后收起列表，屏幕清空，shell 照常可用。
    await tabsButton.click()
    await drawer.getByRole("button", { name: "重置终端", exact: true }).click()
    await expect(drawer).toBeHidden()
    await expect(getScreen(page)).not.toContainText("narrow-before-reset-42")
    await page.locator(".terminal-viewport").click()
    await runCommand(page, "echo narrow-after-$((1+1))")
    await expect(getScreen(page)).toContainText("narrow-after-2")
    // 3. 用完关掉终端，结束 serve 上的 shell。
    await tabsButton.click()
    await drawer.getByRole("button", { name: /^关闭标签页/ }).click()
  })

  test("narrow_terminal_has_a_key_toolbar_and_keeps_its_screen_when_switching_tabs", async ({
    page,
    fixture,
  }) => {
    // 1. 从标签页抽屉新开终端，窄屏下面有按键条。
    await fixture.replies({ text: "好的" })
    await fixture.openApp(page)
    await startConversation(page, "窄屏终端对话")
    const tabsButton = page.getByRole("button", {
      name: "标签页列表",
      exact: true,
    })
    await tabsButton.click()
    const drawer = page.getByRole("dialog", { name: "标签页", exact: true })
    await drawer.getByRole("button", { name: "新终端", exact: true }).click()
    await expect(drawer).toBeHidden()
    await expect(tabsButton).toHaveText("2")
    const toolbar = page.getByRole("toolbar", { name: "终端按键" })
    await expect(toolbar).toBeVisible()
    await expect(
      toolbar.getByRole("button", { name: "Ctrl+C", exact: true })
    ).toBeEnabled()
    // 2. 按键条上的 Enter 和 Ctrl+C 能发给终端。
    await page.locator(".terminal-viewport").click()
    await page.keyboard.type("echo $((6*7))")
    await toolbar.getByRole("button", { name: "Enter", exact: true }).click()
    await expect(getScreen(page)).toContainText("42")
    await page.keyboard.type("sleep 100")
    await toolbar.getByRole("button", { name: "Enter", exact: true }).click()
    await toolbar.getByRole("button", { name: "Ctrl+C", exact: true }).click()
    await runCommand(page, "echo back-$((1+1))")
    await expect(getScreen(page)).toContainText("back-2")
    // 3. 切到对话再切回终端，屏幕内容还在。
    await tabsButton.click()
    await drawer.getByRole("button", { name: /^窄屏终端对话/ }).click()
    await expect(page.getByRole("region", { name: "消息" })).toContainText(
      "好的"
    )
    await expect(getScreen(page)).toHaveCount(0)
    await tabsButton.click()
    await drawer.getByRole("button", { name: /^local · / }).click()
    await expect(getScreen(page)).toContainText("42")
    await expect(getScreen(page)).toContainText("back-2")
  })
})
