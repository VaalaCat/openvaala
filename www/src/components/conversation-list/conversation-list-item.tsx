import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useMemo } from "react"
import { computed } from "nanostores"
import { useStore } from "@nanostores/react"
import { StatusDot } from "@/components/base/status-dot"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useTranslation } from "react-i18next"
import { Link, useLocation, useMatch } from "react-router"
import { ConversationActions } from "@/components/conversation-list/conversation-actions"
import {
  useRunningConversationsSync,
  useServerConnection,
} from "@/components/server-connection-provider"
import {
  SidebarMenuButton,
  SidebarMenuAction,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar"
import type { Conversation } from "@/lib/api/protocol.gen"
import { getConversationListSearch, getConversationPath } from "@/lib/paths"
import { formatRelativeTime } from "@/lib/relative-time"
import {
  $conversationResults,
  getConversationResultKey,
  getConversationStatus,
} from "@/lib/conversation-results"

const conversationStatuses = {
  unknown: { tone: "neutral", labelKey: "conversationList.status.unknown" },
  running: { tone: "info", labelKey: "conversationList.status.running" },
  completed: { tone: "success", labelKey: "conversationList.status.completed" },
  failed: { tone: "destructive", labelKey: "conversationList.status.failed" },
  interrupted: {
    tone: "warning",
    labelKey: "conversationList.status.interrupted",
  },
  idle: { tone: "neutral", labelKey: "conversationList.status.idle" },
} as const

// 对话列表的一行显示标题和更新时间，点击打开对话。
export function ConversationListItem({
  conversation,
}: {
  conversation: Conversation
}) {
  const { t, i18n } = useTranslation()
  const connection = useServerConnection()
  const running = useRunningConversationsSync()
  const resultKey = getConversationResultKey(
    connection.getServerID(),
    conversation.id
  )
  const $status = useMemo(
    () =>
      computed(
        [running.$conversationIDs, $conversationResults],
        (ids, results) =>
          getConversationStatus(ids?.has(conversation.id), results[resultKey])
      ),
    [running, conversation.id, resultKey]
  )
  const status = useStore($status)
  const { tone, labelKey } = conversationStatuses[status]
  const statusLabel = t(labelKey)
  const { search } = useLocation()
  const { setOpenMobile } = useSidebar()
  const isOpened =
    useMatch("/s/:serverID/c/:conversationID/*")?.params.conversationID ===
    conversation.id
  const name = conversation.title || t("conversationList.untitled")
  return (
    <SidebarMenuItem>
      <SidebarMenuButton
        isActive={isOpened}
        className="conversation-list-button h-auto py-1 text-ui group-has-data-[sidebar=menu-action]/menu-item:pr-10"
        render={
          <Link
            to={{
              pathname: getConversationPath(
                connection.getServerID(),
                conversation.id
              ),
              search: getConversationListSearch(search),
            }}
            aria-current={isOpened ? "page" : undefined}
            aria-label={name}
            aria-description={statusLabel}
          />
        }
        onClick={() => setOpenMobile(false)}
      >
        <span className="flex min-w-0 flex-col">
          <OverflowTooltip
            render={
              <span className="conversation-list-title truncate text-table color-subtle" />
            }
          >
            {name}
          </OverflowTooltip>
          <span className="flex min-h-4 items-center gap-2 pl-2">
            <Tooltip>
              <TooltipTrigger
                delay={600}
                closeOnClick
                render={<span className="inline-flex" />}
              >
                <StatusDot
                  tone={tone}
                  ripple={status === "running"}
                  label={statusLabel}
                />
              </TooltipTrigger>
              <TooltipContent className="text-caption">
                {statusLabel}
              </TooltipContent>
            </Tooltip>
            {conversation.updated_at !== undefined && (
              <Tooltip>
                <TooltipTrigger
                  delay={600}
                  closeOnClick
                  render={
                    <span className="max-w-full self-start truncate text-caption color-muted" />
                  }
                >
                  {formatRelativeTime(conversation.updated_at, i18n.language)}
                </TooltipTrigger>
                <TooltipContent className="text-caption">
                  {new Date(conversation.updated_at).toLocaleString(
                    i18n.language
                  )}
                </TooltipContent>
              </Tooltip>
            )}
          </span>
        </span>
      </SidebarMenuButton>
      <ConversationActions
        conversation={conversation}
        name={name}
        isOpened={isOpened}
        trigger={
          <SidebarMenuAction
            showOnHover
            className="size-8 -translate-y-1/2 peer-data-[size=default]/menu-button:top-1/2 aria-expanded:bg-sidebar-accent md:pointer-coarse:opacity-100"
          />
        }
      />
    </SidebarMenuItem>
  )
}
