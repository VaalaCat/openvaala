import { mkdir } from "node:fs/promises"
import path from "node:path"
import type { Locator, Page } from "@playwright/test"
import type { ListConversationsParams } from "../src/lib/api/protocol.gen"
import {
  chooseDefaultWorkLocation,
  expect,
  getMessages,
  recordRequests,
  startConversationAtCurrentLocation,
  test,
  type WebUIFixture,
} from "./fixture"

// 目录输入框
function getDirectory(page: Page): Locator {
  return page.getByRole("combobox", { name: "工作目录", exact: true })
}

// Executor 选择按钮
function getExecutor(page: Page): Locator {
  return page.getByRole("combobox", { name: "Executor", exact: true })
}

// 目录下拉里的「最近用过」分组
function getRecentGroup(page: Page): Locator {
  return page.getByRole("group", { name: "最近用过", exact: true })
}

// 点侧栏的「新对话」，等新对话页出现
async function openNewConversation(page: Page): Promise<void> {
  await page.getByRole("link", { name: "新对话", exact: true }).click()
  await expect(
    page.getByRole("heading", { name: "新对话", level: 1 })
  ).toBeVisible()
}

// 在新对话页选好 Executor 和目录，发出第一条消息并等这一轮结束（对话的更新时间不再变化），返回新对话 ID
async function startConversationIn(
  page: Page,
  executorID: string,
  directory: string,
  text: string
): Promise<string> {
  // 1. 当前不是这个 Executor 时从选择器里选。
  const executor = getExecutor(page)
  if ((await executor.innerText()).trim() !== executorID) {
    await executor.click()
    await page
      .getByRole("combobox", { name: "搜索 Executor", exact: true })
      .fill(executorID)
    await page
      .getByRole("option", { name: new RegExp("^" + executorID) })
      .click()
    await expect(executor).toHaveText(executorID)
  }
  // 2. 手填目录，关掉候选后发送，等模型回复结束。
  await getDirectory(page).fill(directory)
  await page.keyboard.press("Escape")
  await startConversationAtCurrentLocation(page, text)
  await expect(getMessages(page).getByText("ok", { exact: true })).toBeVisible()
  await expect(
    page.getByRole("button", { name: "中断", exact: true })
  ).toHaveCount(0)
  return page.url().split("/c/")[1]
}

// 点侧栏列表里标题以 title 开头的对话
async function openFromList(page: Page, title: string): Promise<void> {
  await page.getByRole("link", { name: new RegExp("^" + title) }).click()
  await expect(
    getMessages(page).getByText(title, { exact: true })
  ).toBeVisible()
}

// 让页面发出的 conversation/list 在 limit 匹配时返回错误，其他请求照常转发；返回的 injected 在注入错误后完成
async function failConversationList(
  page: Page,
  limit: number
): Promise<{ injected: Promise<void> }> {
  let markInjected: () => void
  const injected = new Promise<void>((resolve) => {
    markInjected = resolve
  })
  await page.routeWebSocket(/\/api\/v1\/openvaala/, (socket) => {
    const server = socket.connectToServer()
    socket.onMessage((message) => {
      const request = JSON.parse(String(message)) as {
        id: number
        method?: string
        params?: ListConversationsParams
      }
      if (
        request.method === "conversation/list" &&
        request.params?.limit === limit
      ) {
        socket.send(
          JSON.stringify({
            id: request.id,
            error: {
              code: -32000,
              message: "recent conversations unavailable",
            },
          })
        )
        markInjected()
      } else server.send(message)
    })
    server.onMessage((message) => socket.send(message))
  })
  return { injected }
}

// 来源对话所在的 Executor 和目录：两个本地 Executor 各自的一个已有目录
const sourceCases = [
  {
    name: "local_executor",
    executorID: "local",
    getSourceDirectory: (fixture: WebUIFixture) =>
      path.join(fixture.state.workspaceDirectory, "manual"),
  },
  {
    name: "other_executor",
    executorID: "sandbox",
    getSourceDirectory: (fixture: WebUIFixture) =>
      path.join(fixture.state.sandboxDirectory, "saved"),
  },
]

for (const item of sourceCases) {
  test(`new_conversation_copies_viewed_conversation_location_${item.name}`, async ({
    page,
    fixture,
  }) => {
    // 1. 先建来源对话，再建一个更新的对话，让最近活跃的对话不是来源对话。
    const sourceDirectory = item.getSourceDirectory(fixture)
    const sourceTitle = `位置来源 ${item.name}`
    await fixture.openApp(page)
    await startConversationIn(
      page,
      item.executorID,
      sourceDirectory,
      sourceTitle
    )
    await openNewConversation(page)
    await startConversationIn(
      page,
      "local",
      path.join(fixture.state.workspaceDirectory, "projects", "project-one"),
      `最近活跃 ${item.name}`
    )

    // 2. 打开来源对话再点「新对话」，Executor 和目录照抄来源对话。
    await openFromList(page, sourceTitle)
    await openNewConversation(page)
    await expect(getExecutor(page)).toHaveText(item.executorID)
    await expect(getDirectory(page)).toHaveValue(sourceDirectory)

    // 3. 直接发送，新对话记录里的默认 Executor 和目录与来源对话相同，目录不再解析。
    const requests = recordRequests(page)
    await startConversationAtCurrentLocation(page, `照抄位置 ${item.name}`)
    const saved = await fixture.executorSettings(page.url().split("/c/")[1])
    expect(saved.default_executor_id).toBe(item.executorID)
    expect(saved.workspace_directories?.[item.executorID]).toBe(sourceDirectory)
    expect(
      requests.filter(
        (request) => request.method === "executor/resolveDirectory"
      )
    ).toHaveLength(0)
  })
}

test("new_conversation_page_prefills_latest_conversation_after_reload", async ({
  page,
  fixture,
}) => {
  // 1. 先后建两个目录不同的对话，再打开较早的那个并点「新对话」，预填它的目录。
  const manual = path.join(fixture.state.workspaceDirectory, "manual")
  const projectOne = path.join(
    fixture.state.workspaceDirectory,
    "projects",
    "project-one"
  )
  await fixture.openApp(page)
  await startConversationIn(page, "local", manual, "刷新较早对话")
  await openNewConversation(page)
  await startConversationIn(page, "local", projectOne, "刷新最近对话")
  await openFromList(page, "刷新较早对话")
  await openNewConversation(page)
  await expect(getDirectory(page)).toHaveValue(manual)

  // 2. 刷新后预填的是最近活跃对话的目录。
  await page.reload()
  await expect(getDirectory(page)).toHaveValue(projectOne)
})

test("directory_dropdown_lists_recent_directories_and_filters_by_typed_prefix", async ({
  page,
  fixture,
}) => {
  // 1. 在六个不同目录里各建一个对话，最早的一个排在五个之外。
  const workspace = fixture.state.workspaceDirectory
  const directories = Array.from({ length: 6 }, (_, index) =>
    path.join(workspace, `recent-${index + 1}`)
  )
  for (const directory of directories)
    await mkdir(directory, { recursive: true })
  await fixture.openApp(page)
  for (const [index, directory] of directories.entries()) {
    if (index > 0) await openNewConversation(page)
    await startConversationIn(page, "local", directory, `最近目录 ${index + 1}`)
  }
  await openNewConversation(page)
  const requests = recordRequests(page)
  const directory = getDirectory(page)
  await expect(directory).toHaveValue(directories[5])
  const newestFive = directories.slice(1).reverse()

  // 2. 预填值和打字后的输入分别决定「最近用过」显示哪些目录。
  const cases: { name: string; typed?: string; want: string[] }[] = [
    { name: "prefilled value shows the newest five", want: newestFive },
    {
      name: "typed prefix keeps matching directories",
      typed: path.join(workspace, "recent-"),
      want: newestFive,
    },
    {
      name: "typed longer prefix keeps one directory",
      typed: directories[3],
      want: [directories[3]],
    },
    {
      name: "typed prefix without match hides the group",
      typed: path.join(workspace, "missing"),
      want: [],
    },
  ]
  for (const item of cases) {
    if (item.typed === undefined) await directory.click()
    else await directory.fill(item.typed)
    if (item.want.length === 0) {
      await expect(page.getByText("没有匹配的子目录")).toBeVisible()
      await expect(getRecentGroup(page), item.name).toHaveCount(0)
    } else
      await expect
        .poll(
          async () =>
            await getRecentGroup(page).getByRole("option").allInnerTexts(),
          { message: item.name }
        )
        .toEqual(item.want)
  }

  // 3. 点最近用过的一项：直接填入这个路径并关闭下拉，发送时不再解析目录。
  await directory.fill(path.join(workspace, "recent-"))
  await getRecentGroup(page)
    .getByRole("option", { name: directories[3], exact: true })
    .click()
  await expect(directory).toHaveValue(directories[3])
  await expect(page.getByRole("listbox")).toHaveCount(0)
  const resolveCount = requests.filter(
    (request) => request.method === "executor/resolveDirectory"
  ).length
  await startConversationAtCurrentLocation(page, "点选最近目录")
  expect(
    requests.filter((request) => request.method === "executor/resolveDirectory")
  ).toHaveLength(resolveCount)
  const saved = await fixture.executorSettings(page.url().split("/c/")[1])
  expect(saved.workspace_directories?.local).toBe(directories[3])
})

test("recent_conversations_failure_falls_back_to_default_directory", async ({
  page,
  fixture,
}) => {
  // 1. 先在另一个页面建一个非默认目录的对话。
  const workspace = fixture.state.workspaceDirectory
  const setupPage = await page.context().newPage()
  await fixture.openApp(setupPage)
  await startConversationIn(
    setupPage,
    "local",
    path.join(workspace, "manual"),
    "最近失败来源"
  )
  await setupPage.close()

  // 2. 最近对话请求失败：目录框回到 Executor 的默认根目录，不显示「最近用过」，子目录候选照常。
  const recentList = await failConversationList(page, 50)
  await fixture.openApp(page)
  await recentList.injected
  const directory = getDirectory(page)
  await expect(directory).toHaveValue("")
  await expect(directory).toHaveAttribute("placeholder", workspace)
  await directory.fill(workspace + "/p")
  await expect(
    page.getByRole("option", { name: "projects/", exact: true })
  ).toBeVisible()
  await expect(getRecentGroup(page)).toHaveCount(0)
  await expect(page.getByRole("alert")).toHaveCount(0)
})

test("directory_dropdown_opens_upward_on_first_open_in_short_viewport", async ({
  page,
  fixture,
}) => {
  // 1. 准备一个有 12 个子目录的目录，候选多到输入框下方放不下。
  const parent = path.join(fixture.state.workspaceDirectory, "upward")
  for (let index = 1; index <= 12; index++)
    await mkdir(path.join(parent, `child-${index}`), { recursive: true })

  // 2. 窗口 1280×720，新对话页第一次打开目录下拉：先显示加载行，候选到达后翻到输入框上方。
  await page.setViewportSize({ width: 1280, height: 720 })
  await fixture.openApp(page)
  await chooseDefaultWorkLocation(page)
  await getDirectory(page).fill(parent + "/")
  await expect(
    page.getByRole("option", { name: "child-1/", exact: true })
  ).toBeVisible()
  await expect(page.locator('[data-slot="combobox-content"]')).toHaveAttribute(
    "data-side",
    "top"
  )
})
