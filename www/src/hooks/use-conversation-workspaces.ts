import { useQueryClient } from "@tanstack/react-query"
import {
  useRunningTerminalsSync,
  useServerConnection,
} from "@/components/server-connection-provider"
import {
  getConversationWorkspaces,
  type ConversationWorkspaces,
} from "@/lib/workspace/conversation-workspaces"

// 当前 serve 的对话工作区；第一次使用时从 localStorage 读回保存的布局
export function useConversationWorkspaces(): ConversationWorkspaces {
  const connection = useServerConnection()
  const queryClient = useQueryClient()
  const runningTerminals = useRunningTerminalsSync().$terminals
  return getConversationWorkspaces(
    connection.getServerID(),
    connection,
    queryClient,
    runningTerminals
  )
}
