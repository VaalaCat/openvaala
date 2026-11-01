import { queryOptions } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type { ConversationID, ExecutorSettings } from "../api/protocol.gen"

// SkillTarget 说明查谁的 Skills：已有对话，或者还没创建的对话的 Agent 和 Executor 设置
export type SkillTarget =
  | { conversationID: ConversationID }
  | { agentName: string; executorSettings: ExecutorSettings }

// target 能用的 Skills
export function skillListOptions(
  connection: ServerConnection,
  target: SkillTarget
) {
  return queryOptions({
    queryKey: [connection.getServerID(), "skills", target],
    queryFn: () =>
      connection.call(
        "skill/list",
        "conversationID" in target
          ? { conversation_id: target.conversationID }
          : {
              agent_name: target.agentName,
              executor_settings: target.executorSettings,
            }
      ),
  })
}
