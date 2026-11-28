import { queryOptions } from "@tanstack/react-query"
import { agentCatalogOptions } from "./agent-settings"
import type { ServerConnection } from "../api/server-connection"
import type { ListModelsParams } from "../api/protocol.gen"

// agentName 能用的模型；agentName 为空时不含 Agent 默认模型
export function modelListOptions(
  connection: ServerConnection,
  agentName: string,
  scope?: Omit<ListModelsParams, "agent_name">
) {
  return queryOptions({
    queryKey: [connection.getServerID(), "models", agentName, scope],
    queryFn: async ({ client }) => {
      const result = await connection.call("model/list", {
        agent_name: agentName,
        ...scope,
      })
      if (agentName || scope) return result
      const catalog = await client.fetchQuery(agentCatalogOptions(connection))
      const external = new Set(
        catalog.agents
          .filter((agent) => agent.type !== "native")
          .map((agent) => agent.name)
      )
      return {
        ...result,
        models: result.models.filter(
          (model) => model && !external.has(model.ref.provider_id)
        ),
      }
    },
  })
}

export { agentCatalogOptions } from "./agent-settings"
