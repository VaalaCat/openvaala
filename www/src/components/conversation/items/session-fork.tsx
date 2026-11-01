import { GitFork } from "lucide"
import { Trans, useTranslation } from "react-i18next"
import { Link } from "react-router"
import { ItemNotice } from "@/components/conversation/item-notice"
import { SessionLink } from "@/components/conversation/session-link"
import { useServerConnection } from "@/components/server-connection-provider"
import type { ItemRendererProps } from "@/lib/item-renderers"
import { getConversationPath } from "@/lib/paths"

// session_fork：历史分界。用户分叉的对话链接到来源对话（更早的部分留在那里）；spawn_agent 建的子 Agent 链接到父 Agent
export function SessionForkItem({ view, sync }: ItemRendererProps) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const fork = view.item.session_fork
  if (!fork) return null
  if (fork.source_conversation_id)
    return (
      <ItemNotice icon={GitFork}>
        <Trans
          t={t}
          i18nKey="conversation.notice.forkedConversation"
          components={{
            source: (
              <Link
                className="text-link"
                to={getConversationPath(
                  connection.getServerID(),
                  fork.source_conversation_id
                )}
              />
            ),
          }}
        />
      </ItemNotice>
    )
  return (
    <ItemNotice icon={GitFork}>
      <span className="inline-flex max-w-full min-w-0 items-center gap-2">
        <span className="min-w-0 truncate">
          {t("conversation.notice.spawnedFrom")}
        </span>
        <SessionLink sync={sync} sessionID={fork.source_session_id} />
      </span>
    </ItemNotice>
  )
}
