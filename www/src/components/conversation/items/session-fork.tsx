import { GitFork } from "lucide"
import { useTranslation } from "react-i18next"
import { ForkOriginNotice } from "@/components/conversation/fork-origin-notice"
import { ItemNotice } from "@/components/conversation/item-notice"
import { SessionLink } from "@/components/conversation/session-link"
import type { ItemRendererProps } from "@/lib/item-renderers"

// session_fork：历史分界。用户分叉的对话链接到来源对话（更早的部分留在那里）；spawn_agent 建的子 Agent 链接到父 Agent
export function SessionForkItem({ view, sync }: ItemRendererProps) {
  const { t } = useTranslation()
  const fork = view.item.session_fork
  if (!fork) return null
  if (fork.source_conversation_id)
    return (
      <ForkOriginNotice
        kind="fork"
        sourceConversationID={fork.source_conversation_id}
      />
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
