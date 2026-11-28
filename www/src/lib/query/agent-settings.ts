import {
  mutationOptions,
  queryOptions,
  type QueryClient,
} from "@tanstack/react-query"
import type { AgentConfig } from "../api/protocol.gen"
import type { ServerConnection } from "../api/server-connection"

export function agentCatalogOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "agentCatalog"],
    queryFn: () => connection.call("agent/list", {}),
  })
}

export function agentPresetCatalogOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "agentPresets"],
    queryFn: () => connection.call("catalog/agents/list", {}),
  })
}

export function getAgentPresetName(name: string, existingNames: string[]) {
  let available = name
  for (let suffix = 2; existingNames.includes(available); suffix++) {
    available = `${name}-${suffix}`
  }
  return available
}

export function agentDefinitionOptions(
  connection: ServerConnection,
  name: string
) {
  return queryOptions({
    queryKey: [connection.getServerID(), "agentDefinitions", name],
    queryFn: () => connection.call("agent/get", { name }),
  })
}

function invalidateAgents(client: QueryClient, connection: ServerConnection) {
  for (const segment of [
    "agentCatalog",
    "agentDefinitions",
    "models",
    "agentOptions",
  ]) {
    void client.invalidateQueries({
      queryKey: [connection.getServerID(), segment],
    })
  }
}

export function setAgentOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (definition: AgentConfig) =>
      connection.call("agent/set", { definition }),
    onSuccess: (_result, definition, _mutation, { client }) => {
      client.setQueryData(
        agentDefinitionOptions(connection, definition.name).queryKey,
        definition
      )
      invalidateAgents(client, connection)
    },
  })
}

export function deleteAgentOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (name: string) => connection.call("agent/delete", { name }),
    onSuccess: (_result, _name, _mutation, { client }) =>
      invalidateAgents(client, connection),
  })
}
