import { persistentAtom } from "@nanostores/persistent"

// 浏览器记住的一个 serve；一期只有页面所在的 serve 这一项
export interface ServerEntry {
  // ws(s)://host:port/api/v1/openvaala
  url: string
  token: string
  // initialize 返回后记下
  serverID?: string
}

// 存储里的值损坏时当作没有记录
function parseServers(value: string): ServerEntry[] {
  try {
    const servers: unknown = JSON.parse(value)
    if (Array.isArray(servers))
      return servers.filter(
        (server): server is ServerEntry =>
          typeof server?.url === "string" && typeof server?.token === "string"
      )
  } catch {
    // 按没有记录处理，用户重新输入 token
  }
  return []
}

export const $servers = persistentAtom<ServerEntry[]>("openvaala.servers", [], {
  encode: JSON.stringify,
  decode: parseServers,
})

// 页面所在的 serve 的 WebSocket 地址，协议跟着页面走
export function getPageServerURL(
  location: Pick<Location, "protocol" | "host">
): string {
  const protocol = location.protocol === "https:" ? "wss:" : "ws:"
  return `${protocol}//${location.host}/api/v1/openvaala`
}

// 读出 openvaala web 放在地址 # 后面的 token；没有时是 undefined
export function takeTokenFromHash(hash: string): string | undefined {
  return new URLSearchParams(hash.replace(/^#/, "")).get("token") || undefined
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

// 记下 initialize 返回的 server_id
export function saveServerID(url: string, serverID: string): void {
  $servers.set(
    $servers
      .get()
      .map((server) => (server.url === url ? { ...server, serverID } : server))
  )
}
