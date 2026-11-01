import { queryOptions } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"

// 模型目录里能对应上协议和 base_url 的服务
export function catalogProviderListOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "catalogProviders"],
    queryFn: () => connection.call("catalog/providers/list", {}),
  })
}

// 模型目录里某个服务的模型
export function catalogModelListOptions(
  connection: ServerConnection,
  catalogProviderID: string
) {
  return queryOptions({
    queryKey: [connection.getServerID(), "catalogModels", catalogProviderID],
    queryFn: () =>
      connection.call("catalog/models/list", {
        provider_id: catalogProviderID,
      }),
  })
}
