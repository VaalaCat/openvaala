import { queryOptions } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type { ConversationID } from "../api/protocol.gen"

// 对话里 MCP 服务的连接状态；serve 要求这个对话已经打开
export function mcpServerListOptions(
  connection: ServerConnection,
  conversationID: ConversationID
) {
  return queryOptions({
    queryKey: [connection.getServerID(), "mcpServers", conversationID],
    queryFn: () =>
      connection.call("mcpServer/list", { conversation_id: conversationID }),
  })
}
