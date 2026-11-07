import { createContext, useContext } from "react"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { ConversationID } from "@/lib/api/protocol.gen"

// SessionLinkContext 是工作区所属对话的 ID：只有这条对话的 Session 有自己的页面（主对话和子 Agent 标签页）；
// 旁支是另一条临时对话，它的 Session 没有页面可去
export const SessionLinkContext = createContext<ConversationID | undefined>(
  undefined
)

// 这个同步的 Session 有页面可以跳转吗？
export function useIsSessionLinked(sync: ConversationSync): boolean {
  return sync.conversationID === useContext(SessionLinkContext)
}
