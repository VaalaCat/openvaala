import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"
import { useServerConnection } from "@/components/server-connection-provider"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { SessionID } from "@/lib/api/protocol.gen"
import { getConversationPath, getSessionPath } from "@/lib/paths"
import { getSessionName } from "@/lib/session-tree"
import { cn } from "@/lib/utils"

// 对话里一个 Session 的链接，点击进入它的页面：子 Agent 显示任务名（没有时用 Agent 名），根 Session 叫「主 Agent」；
// 还不在 $sessions 里的 Session 显示 ID
export function SessionLink({
  sync,
  sessionID,
  className,
}: {
  sync: ConversationSync
  sessionID: SessionID
  className?: string
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const info = useStore(sync.$sessions, { keys: [sessionID] })[sessionID]
  const serverID = connection.getServerID()
  const isRoot = !!info && !info.session.parent_session_id
  const name = info
    ? getSessionName(info, t("conversation.session.root"))
    : sessionID
  return (
    <OverflowTooltip
      render={
        <Link
          to={
            isRoot
              ? getConversationPath(serverID, sync.conversationID)
              : getSessionPath(serverID, sync.conversationID, sessionID)
          }
          className={cn("max-w-48 min-w-0 truncate text-link", className)}
        />
      }
    >
      {name}
    </OverflowTooltip>
  )
}
