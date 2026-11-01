import { queryOptions } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"

// agentName 能用的模型；agentName 为空时不含 Agent 默认模型
export function modelListOptions(
  connection: ServerConnection,
  agentName: string
) {
  return queryOptions({
    queryKey: [connection.getServerID(), "models", agentName],
    queryFn: () => connection.call("model/list", { agent_name: agentName }),
  })
}
