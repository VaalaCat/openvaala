import type { QueryClient } from "@tanstack/react-query"
import type { NotificationParams } from "../api/protocol.gen"

// 每种通知让哪些查询过期：列出 query key 的第二段，按 [serverID, 第二段] 前缀失效
const invalidatedQueriesByNotification: Partial<
  Record<keyof NotificationParams, string[]>
> = {
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
  void queryClient.invalidateQueries({ queryKey: [serverID] })
}
