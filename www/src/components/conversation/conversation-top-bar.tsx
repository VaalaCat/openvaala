import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useCallback, useState } from "react"
import {
  EllipsisIcon,
  PanelRightCloseIcon,
  PanelRightOpenIcon,
} from "lucide-react"
import { useStore } from "@nanostores/react"
import { useQueryClient, type InfiniteData } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { PageActions } from "@/components/base/page-layout"
import { RenameConversationDialog } from "@/components/conversation-list/rename-conversation-dialog"
import { ConversationSettingsDialog } from "@/components/conversation/conversation-settings-dialog"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { useIsMobile } from "@/hooks/use-mobile"
import { usePageNavigation } from "@/hooks/use-page-navigation"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type {
  SessionRuntimeInfo,
  SessionID,
  ListConversationsResult,
} from "@/lib/api/protocol.gen"
import { notifySuccess } from "@/lib/notify"
import { getConversationPath, getSessionPath } from "@/lib/paths"
import { getSessionAncestors, getSessionName } from "@/lib/session-tree"

// RightPanelToggle 是顶栏控制右栏的按钮状态：桌面收起或展开右栏，窄屏打开抽屉（手机上是底部抽屉，按钮显示 ⋯）
export interface RightPanelToggle {
  isOpen: boolean
  isMobile: boolean
  onToggle(): void
}

// 对话页顶栏：登记标题（主 Session 点标题改名；子 Session 的标题是子 Agent 名，面包屑从「主 Agent」起逐级链回上级 Session），
// 右侧放对话设置和右栏开关；手机宽度的子 Session 页不放对话设置，位置留给子 Agent 名
export function ConversationTopBar({
  sync,
  sessionInfo,
  isReady,
  sessionID,
  isSubSession,
  rightPanel,
}: {
  sync: ConversationSync
  // 跟随 Session 的运行信息；对话订阅还没给出这个 Session 时为空
  sessionInfo?: SessionRuntimeInfo
  isReady: boolean
  sessionID?: SessionID
  isSubSession: boolean
  rightPanel: RightPanelToggle
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isMobile = useIsMobile()
  const conversationTitle = useStore(sync.$title)
  const sessions = useStore(sync.$sessions)
  const queryClient = useQueryClient()
  const cachedTitle = queryClient
    .getQueriesData<InfiniteData<ListConversationsResult>>({
      queryKey: [sync.serverID, "conversations"],
    })
    .flatMap(([, data]) => data?.pages ?? [])
    .flatMap((page) => page.conversations ?? [])
    .find((entry) => entry.conversation.id === sync.conversationID)
    ?.conversation.title
  const title =
    conversationTitle ||
    cachedTitle ||
    t(isReady ? "conversationList.untitled" : "common.loading")
  const [isRenaming, setIsRenaming] = useState(false)
  const [dialogConversationID, setDialogConversationID] = useState(
    sync.conversationID
  )
  if (dialogConversationID !== sync.conversationID) {
    setDialogConversationID(sync.conversationID)
    setIsRenaming(false)
  }
  const openRenameDialog = useCallback(() => setIsRenaming(true), [])
  const rootName = t("conversation.session.root")
  const followedSessionID = isReady
    ? sync.getFollowedSessionID()
    : (sessionID ?? "")
  const pageTitle = isSubSession
    ? sessionInfo
      ? getSessionName(sessionInfo, rootName)
      : t("common.loading")
    : title
  usePageNavigation(
    pageTitle,
    undefined,
    isSubSession
      ? getSubSessionCrumbs(
          connection.getServerID(),
          sync.conversationID,
          getSessionAncestors(sessions, followedSessionID),
          rootName
        )
      : undefined,
    isSubSession || !isReady ? undefined : openRenameDialog
  )
  const RightPanelIcon = rightPanel.isMobile
    ? EllipsisIcon
    : rightPanel.isOpen
      ? PanelRightCloseIcon
      : PanelRightOpenIcon
  return (
    <>
      <h1 className="sr-only">{pageTitle}</h1>
      <PageActions>
        {/* 对话设置属于整个对话，手机宽度的子 Session 页从面包屑回主 Session 打开 */}
        {!(isSubSession && isMobile) && (
          <ConversationSettingsDialog
            key={sync.conversationID}
            conversationID={sync.conversationID}
          />
        )}
        <Tooltip>
          <TooltipTrigger
            render={
              <Button
                variant="ghost"
                size="icon"
                className="shrink-0 aria-pressed:bg-muted"
                aria-pressed={rightPanel.isOpen}
                aria-label={t("conversation.panel.toggle")}
                onClick={rightPanel.onToggle}
              />
            }
          >
            <RightPanelIcon />
          </TooltipTrigger>
          <TooltipContent className="text-caption">
            {t("conversation.panel.toggle")}
          </TooltipContent>
        </Tooltip>
      </PageActions>
      {!isSubSession && (
        <RenameConversationDialog
          key={sync.conversationID}
          conversationID={sync.conversationID}
          title={conversationTitle}
          open={isRenaming}
          onOpenChange={setIsRenaming}
          onRenamed={() => {
            setIsRenaming(false)
            notifySuccess(t("conversationList.rename.success"))
          }}
        />
      )}
    </>
  )
}

// 子 Session 页的面包屑父级：第一级总是「主 Agent」，链回主 Session；之后是中间各级子 Agent
function getSubSessionCrumbs(
  serverID: string,
  conversationID: string,
  ancestors: SessionRuntimeInfo[],
  rootName: string
): { label: string; to: string }[] {
  const crumbs = ancestors
    .filter((info) => info.session.parent_session_id)
    .map((info) => ({
      label: getSessionName(info, rootName),
      to: getSessionPath(serverID, conversationID, info.session.id),
    }))
  return [
    { label: rootName, to: getConversationPath(serverID, conversationID) },
    ...crumbs,
  ]
}
