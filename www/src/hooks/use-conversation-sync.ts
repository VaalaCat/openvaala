import { useEffect, useMemo } from "react"
import { useQueryClient } from "@tanstack/react-query"
import { useServerConnection } from "@/components/server-connection-provider"
import { ConversationSync } from "@/lib/api/conversation-sync"
import type { ConversationID, SessionID } from "@/lib/api/protocol.gen"

// 页面跟随的对话：按对话和 Session 建一个 ConversationSync，挂载期间运行，卸载或换对话时先停掉旧的
export function useConversationSync(
  conversationID: ConversationID,
  sessionID?: SessionID,
  attempt = 0
): ConversationSync {
  const connection = useServerConnection()
  const queryClient = useQueryClient()
  const sync = useMemo(
    () =>
      new ConversationSync(connection, queryClient, conversationID, sessionID),
    [connection, queryClient, conversationID, sessionID]
  )
  useEffect(() => sync.run(), [sync, attempt])
  return sync
}
