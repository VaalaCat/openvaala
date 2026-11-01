import { queryOptions } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"

// 配置里的全部 hook，按配置顺序排列
export function hookListOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "hooks"],
    queryFn: () => connection.call("hook/list", {}),
  })
}
