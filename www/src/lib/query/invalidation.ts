import type { QueryClient } from "@tanstack/react-query"
import type { NotificationParams } from "../api/protocol.gen"
import type { ServerConnection } from "../api/server-connection"
import type { WorldEvent } from "../api/world"

// 每种通知让哪些查询过期：列出 query key 的第二段，按 [serverID, 第二段] 前缀失效
const invalidatedQueriesByNotification: Partial<
  Record<keyof NotificationParams, string[]>
> = {
  "session/externalStateUpdated": ["models"],
  "conversation/titleUpdated": ["conversations"],
  // 一轮结束后上下文用量和 Turn 列表变了，对话的更新时间也变了
  "turn/finished": ["contextUsage", "turns", "conversations"],
}

// 收到推送后让受影响的查询失效；推送触发的失效只写在这里
export function invalidateForNotification(
  queryClient: QueryClient,
  serverID: string,
  method: keyof NotificationParams
): void {
  for (const segment of invalidatedQueriesByNotification[method] ?? [])
    void queryClient.invalidateQueries({ queryKey: [serverID, segment] })
}

// 重连之后让这个 serve 的全部查询过期：断开期间失败的重新取，错过推送的数据刷新
export function invalidateAfterReconnect(
  queryClient: QueryClient,
  serverID: string
): void {
  void queryClient.invalidateQueries({
    queryKey: [serverID],
    predicate: (query) => !query.meta?.externalHistory,
  })
}

const worldSegments = {
  status_changed: ["status"],
  peers_changed: ["peers"],
  message_updated: ["messages", "threads", "status", "peers"],
} as const satisfies Record<WorldEvent, readonly string[]>

// 按服务器和资源分组，URL 区分同一服务器的不同连接地址。
export function getWorldQueryKey(
  connection: ServerConnection,
  segment: "status" | "peers" | "messages" | "threads"
) {
  return [
    connection.getServerID(),
    "world",
    segment,
    connection.entry.url,
  ] as const
}

// 收到联系人事件时刷新当前连接的数据，接通事件流时补齐遗漏的变化。
export function invalidateWorldQueries(
  queryClient: QueryClient,
  connection: ServerConnection,
  event?: WorldEvent
): void {
  const serverID = connection.getServerID()
  if (event) {
    for (const segment of worldSegments[event])
      void queryClient.invalidateQueries({
        queryKey: getWorldQueryKey(connection, segment),
      })
  } else {
    void queryClient.invalidateQueries({ queryKey: [serverID, "world"] })
  }
  if (!event || event === "message_updated")
    void queryClient.invalidateQueries({
      queryKey: [serverID, "conversations"],
    })
}
