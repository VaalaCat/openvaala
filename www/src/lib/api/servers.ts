import { persistentAtom } from "@nanostores/persistent"
import { ServerConnection } from "./server-connection"

// 浏览器记住的一个 serve；包含页面默认 serve 与用户添加的远端 serve
export interface ServerEntry {
  // ws(s)://host:port/api/v1/openvaala
  url: string
  token: string
  // 用户自定义的显示名称，省略时按 host 展示
  name?: string
  // initialize 返回后记下
  serverID?: string
}

export type ParseServerInputError =
  | "required"
  | "invalid"
  | "unsupportedScheme"
  | "missingHost"

export type ParseServerInputResult =
  | {
      ok: true
      url: string
      token: string
      defaultName: string
      extractedToken: boolean
    }
  | {
      ok: false
      error: ParseServerInputError
    }

export type ServerTestResult =
  | { ok: true }
  | {
      ok: false
      reason: "unauthorized" | "incompatible" | "unreachable" | "timeout"
    }

const runtimeServerWSPath = "/api/v1/openvaala"

// 存储里的值损坏时当作没有记录，跳过单条坏记录不丢其余合法条目
function parseServers(value: string): ServerEntry[] {
  try {
    const servers: unknown = JSON.parse(value)
    if (Array.isArray(servers))
      return servers.flatMap((server): ServerEntry[] => {
        if (
          typeof server !== "object" ||
          server === null ||
          typeof (server as ServerEntry).url !== "string" ||
          typeof (server as ServerEntry).token !== "string"
        )
          return []
        const raw = server as ServerEntry
        const parsed = parseServerInput(raw.url, raw.token)
        if (!parsed.ok) return []
        const entry: ServerEntry = {
          url: parsed.url,
          token: parsed.token,
        }
        if (typeof raw.name === "string" && raw.name.trim())
          entry.name = raw.name.trim()
        if (typeof raw.serverID === "string" && raw.serverID)
          entry.serverID = raw.serverID
        return [entry]
      })
  } catch {
    // 按没有记录处理，用户重新输入 token
  }
  return []
}

export const $servers = persistentAtom<ServerEntry[]>("openvaala.servers", [], {
  encode: JSON.stringify,
  decode: parseServers,
})

// 当前选中的目标 serve URL；空串表示跟随当前页面所在的默认 serve
export const $activeServerURL = persistentAtom<string>(
  "openvaala.activeServerURL",
  ""
)

// 页面所在的 serve 的 WebSocket 地址，协议跟着页面走
export function getPageServerURL(
  location: Pick<Location, "protocol" | "host">
): string {
  const protocol = location.protocol === "https:" ? "wss:" : "ws:"
  return `${protocol}//${location.host}${runtimeServerWSPath}`
}

// 读出 openvaala web 放在地址 # 后面的 token；没有时是 undefined
export function takeTokenFromHash(hash: string): string | undefined {
  return new URLSearchParams(hash.replace(/^#/, "")).get("token") || undefined
}

// 把用户粘贴的 openvaala web 链接、ws(s) 链接或 host:port 规范成干净的 WebSocket 地址和 token，规则与桌面端一致
export function parseServerInput(
  rawURL: string,
  fallbackToken = ""
): ParseServerInputResult {
  // 1. 去空白；缺少 scheme 时默认补 ws://。
  const trimmed = rawURL.trim()
  if (!trimmed) return { ok: false, error: "required" }
  const candidate = trimmed.includes("://") ? trimmed : `ws://${trimmed}`
  let parsed: URL
  try {
    parsed = new URL(candidate)
  } catch {
    return { ok: false, error: "invalid" }
  }
  // 2. 从 fragment 或 query 里拆出 token，再按协议规范 scheme 和路径。
  const extracted =
    takeTokenFromHash(parsed.hash)?.trim() ||
    parsed.searchParams.get("token")?.trim() ||
    ""
  const protocol = parsed.protocol.toLowerCase()
  if (protocol === "http:") {
    parsed.protocol = "ws:"
    parsed.pathname = runtimeServerWSPath
  } else if (protocol === "https:") {
    parsed.protocol = "wss:"
    parsed.pathname = runtimeServerWSPath
  } else if (protocol === "ws:" || protocol === "wss:") {
    if (!parsed.pathname || parsed.pathname === "/")
      parsed.pathname = runtimeServerWSPath
  } else {
    return { ok: false, error: "unsupportedScheme" }
  }
  if (!parsed.host) return { ok: false, error: "missingHost" }
  // 3. 清除 query 和 hash，返回干净的 WebSocket URL。
  parsed.search = ""
  parsed.hash = ""
  return {
    ok: true,
    url: parsed.toString(),
    token: extracted || fallbackToken.trim(),
    defaultName: parsed.host,
    extractedToken: Boolean(extracted),
  }
}

// 提取服务器地址的 host[:port] 供展示，解析失败时回退原文
export function getServerHost(rawURL: string): string {
  try {
    return new URL(rawURL).host || rawURL
  } catch {
    return rawURL
  }
}

// 保存某个 serve 的 token；已有这一项就只换 token
export function saveServerToken(url: string, token: string): void {
  const servers = $servers.get()
  $servers.set(
    servers.some((server) => server.url === url)
      ? servers.map((server) =>
          server.url === url ? { ...server, token } : server
        )
      : [...servers, { url, token }]
  )
}

// 新增或更新一条远端服务器档案；改了 URL 时同步更新当前活跃目标
export function upsertServer(input: {
  previousURL?: string
  url: string
  token: string
  name?: string
}): void {
  const matchURL = input.previousURL ?? input.url
  const name = input.name?.trim() || undefined
  const servers = $servers.get()
  const existing = servers.find((server) => server.url === matchURL)
  const nextEntry: ServerEntry = {
    url: input.url,
    token: input.token,
    ...(name ? { name } : {}),
    ...(existing?.serverID && matchURL === input.url
      ? { serverID: existing.serverID }
      : {}),
  }
  const filtered = servers.filter(
    (server) => server.url !== matchURL && server.url !== input.url
  )
  const existingIndex = servers.findIndex((server) => server.url === matchURL)
  if (existingIndex >= 0) {
    const updated = [...filtered]
    updated.splice(Math.min(existingIndex, updated.length), 0, nextEntry)
    $servers.set(updated)
  } else {
    $servers.set([...filtered, nextEntry])
  }
  if (input.previousURL && $activeServerURL.get() === input.previousURL)
    $activeServerURL.set(input.url)
}

// 删除一条远端服务器档案；如果它正是当前活跃服务器，自动退回默认页服务器
export function removeServer(url: string): void {
  $servers.set($servers.get().filter((server) => server.url !== url))
  if ($activeServerURL.get() === url) $activeServerURL.set("")
}

// 切换当前活跃服务器；传空串或等于 pageURL 时表示回到页面默认服务器
export function setActiveServerURL(url: string, pageURL: string): void {
  $activeServerURL.set(!url || url === pageURL ? "" : url)
}

// 记下 initialize 返回的 server_id
export function saveServerID(url: string, serverID: string): void {
  $servers.set(
    $servers
      .get()
      .map((server) => (server.url === url ? { ...server, serverID } : server))
  )
}

// 用独立临时连接测试目标地址的连通性、Token 和协议版本，结束后立即断开，不影响当前主连接
export function testServerConnection(
  entry: Pick<ServerEntry, "url" | "token">,
  timeoutMs = 10_000
): Promise<ServerTestResult> {
  return new Promise((resolve) => {
    const connection = new ServerConnection({
      url: entry.url,
      token: entry.token,
    })
    let settled = false
    const cleanups: (() => void)[] = []
    const finish = (result: ServerTestResult) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      for (const cleanup of cleanups) cleanup()
      resolve(result)
    }
    const timer = setTimeout(() => finish({ ok: false, reason: "timeout" }), timeoutMs)
    cleanups.push(
      connection.$state.listen((state) => {
        if (state === "connected") finish({ ok: true })
        else if (state === "unauthorized")
          finish({ ok: false, reason: "unauthorized" })
        else if (state === "incompatible")
          finish({ ok: false, reason: "incompatible" })
        else if (state === "reconnecting")
          finish({ ok: false, reason: "unreachable" })
      })
    )
    cleanups.push(connection.run())
  })
}
