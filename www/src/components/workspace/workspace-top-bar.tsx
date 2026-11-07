import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useCallback, useState } from "react"
import {
  ChevronDownIcon,
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
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"
import type { TabWorkspaceTab } from "@/lib/workspace/types"

// RightPanelToggle 是顶栏控制右栏的按钮状态：桌面收起或展开右栏，窄屏打开抽屉（手机上是底部抽屉，按钮显示 ⋯）
export interface RightPanelToggle {
  isOpen: boolean
  isMobile: boolean
  onToggle(): void
}

// 工作区顶栏：标题、面包屑和操作按钮跟着当前选中的标签页走。
// 主对话标签页点标题改名，子 Agent 的面包屑从「主 Agent」起逐级链回上级 Session；
// 旁支和终端的标题只是文字。右侧从左到右是窄屏的标签页列表、对话设置和右栏开关
export function WorkspaceTopBar({
  workspace,
  selectedTabID,
  tab,
  tabCount,
  rightPanel,
  onOpenTabDrawer,
}: {
  workspace: TabWorkspace
  // 当前选中的标签页
  selectedTabID: string
  tab: TabWorkspaceTab
  tabCount: number
  rightPanel: RightPanelToggle
  onOpenTabDrawer(): void
}) {
  const { t } = useTranslation()
  const isMobile = useIsMobile()
  const sync = workspace.getConversationSync(selectedTabID)
  // 终端标题跟着 shell 变化
  useStore(workspace.$terminalTitles)
  const isSubSession = tab.kind === "sub-agent"
  const RightPanelIcon = rightPanel.isMobile
    ? EllipsisIcon
    : rightPanel.isOpen
      ? PanelRightCloseIcon
      : PanelRightOpenIcon
  return (
    <>
      {(tab.kind === "main" || tab.kind === "sub-agent") && sync ? (
        <ConversationTabTitle
          key={selectedTabID}
          sync={sync}
          sessionID={tab.kind === "sub-agent" ? tab.sessionID : undefined}
        />
      ) : (
        <PlainTabTitle title={workspace.getTabTitle(selectedTabID)} />
      )}
      <PageActions>
        {/* 宽屏有标签栏，窄屏才用这个按钮拉起标签页列表 */}
        <Button
          variant="ghost"
          className="shrink-0 gap-1 px-2 text-ui md:hidden"
          aria-label={t("workspace.tabs.open")}
          onClick={onOpenTabDrawer}
        >
          {tabCount}
          <ChevronDownIcon />
        </Button>
        {/* 对话设置属于整个对话，手机宽度的子 Session 从面包屑回主 Session 打开 */}
        {(tab.kind === "main" || tab.kind === "sub-agent") &&
          !(isSubSession && isMobile) && (
            <ConversationSettingsDialog
              key={workspace.conversationID}
              conversationID={workspace.conversationID}
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
    </>
  )
}

// 旁支和终端标签页的标题：只是文字，点了没有反应
function PlainTabTitle({ title }: { title: string }) {
  usePageNavigation(title)
  return <h1 className="sr-only">{title}</h1>
}

// 对话标签页的标题（主 Session 点标题改名；子 Session 的标题是子 Agent 名，面包屑链回上级 Session）和改名对话框
function ConversationTabTitle({
  sync,
  sessionID,
}: {
  sync: ConversationSync
  sessionID?: SessionID
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isSubSession = !!sessionID
  useStore(sync.$syncState)
  const isReady = sync.hasInitialData()
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
  const openRenameDialog = useCallback(() => setIsRenaming(true), [])
  const rootName = t("conversation.session.root")
  const followedSessionID = isReady
    ? sync.getFollowedSessionID()
    : (sessionID ?? "")
  const sessionInfo = sessions[followedSessionID]
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
  return (
    <>
      <h1 className="sr-only">{pageTitle}</h1>
      {!isSubSession && (
        <RenameConversationDialog
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
