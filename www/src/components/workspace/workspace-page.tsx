import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import {
  useLocation,
  useMatch,
  useNavigate,
  useNavigationType,
  useParams,
} from "react-router"
import { usePanelRef } from "react-resizable-panels"
import { RightPanel } from "@/components/conversation/right-panel"
import { SessionLinkContext } from "@/components/conversation/session-link-context"
import { useServerConnection } from "@/components/server-connection-provider"
import { Drawer, DrawerContent, DrawerTitle } from "@/components/ui/drawer"
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable"
import { WorkspaceLayout } from "@/components/workspace/workspace-layout"
import { WorkspaceTabDrawer } from "@/components/workspace/workspace-tab-drawer"
import { WorkspaceTopBar } from "@/components/workspace/workspace-top-bar"
import { useIsDesktop } from "@/hooks/use-is-desktop"
import { getIsMobile, useIsMobile } from "@/hooks/use-mobile"
import { useConversationWorkspaces } from "@/hooks/use-conversation-workspaces"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import {
  $conversationLayout,
  $isRightPanelCollapsed,
  $rightPanelTab,
} from "@/lib/appearance/preferences"
import { getConversationMessageStores } from "@/lib/conversation-message-stores"
import type { ItemDebugTarget } from "@/lib/item-renderers"
import {
  getConversationListSearch,
  getConversationPath,
  getSessionPath,
} from "@/lib/paths"
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"
import type { TabWorkspaceTab } from "@/lib/workspace/types"

// 桌面布局里两个 Panel 的 id，也是 $conversationLayout 里的 key
const conversationPanelID = "conversation"
const rightPanelID = "right-panel"

// 对话地址的页面：显示地址里那条对话的工作区，进入时切到它，换对话或离开页面时离开它
export function WorkspacePage() {
  const workspaces = useConversationWorkspaces()
  const conversationID = useParams().conversationID!
  const currentConversationID = useStore(workspaces.$currentConversationID)
  useEffect(() => {
    // 1. 切到这条对话的工作区；换对话或离开页面时离开它，开着旁支或终端的留在后台。
    workspaces.open(conversationID)
    return () => workspaces.leave()
  }, [workspaces, conversationID])
  // 2. 工作区切过来之前不显示；换对话时整个工作区重新挂载，右栏和抽屉的状态不带到另一条对话。
  const workspace =
    currentConversationID === conversationID
      ? workspaces.getWorkspace(conversationID)
      : undefined
  if (!workspace) return null
  return (
    <SessionLinkContext.Provider value={conversationID}>
      <ConversationWorkspace key={conversationID} workspace={workspace} />
    </SessionLinkContext.Provider>
  )
}

// 一条对话的工作区：标签页和分栏在中间，最近选中的主对话、子 Agent 或旁支的详情在右栏；
// 顶栏跟着当前标签页走，地址栏和选中的标签页互相跟随
function ConversationWorkspace({ workspace }: { workspace: TabWorkspace }) {
  const { t } = useTranslation()
  useWorkspaceRouteSync(workspace)
  const tabs = useStore(workspace.$tabs)
  const selectedTabID = useStore(workspace.$selectedTabID)
  // 1. 右栏显示最近选中的主对话、子 Agent 或旁支；它被关掉而选中的是终端时显示根 Session。
  const lastConversationTabID = useStore(workspace.$lastConversationTabID)
  const lastSync =
    workspace.getConversationSync(lastConversationTabID) ??
    workspace.getRootSync()
  const isDesktopViewport = useIsDesktop()
  const viewRef = useRef<HTMLDivElement>(null)
  const [hasPanelSpace, setHasPanelSpace] = useState(false)
  useLayoutEffect(() => {
    const view = viewRef.current!
    const measure = () => setHasPanelSpace(view.clientWidth >= 648)
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(view)
    return () => observer.disconnect()
  }, [])
  const isDesktop = isDesktopViewport && hasPanelSpace
  const isMobile = useIsMobile()
  const layout = useStore($conversationLayout)
  const isRightPanelCollapsed = useStore($isRightPanelCollapsed)
  const rightPanelRef = usePanelRef()
  const panelGroupRef = useRef<HTMLDivElement>(null)
  const panelElementRef = useRef<HTMLDivElement>(null)
  const [hasPanelContent, setHasPanelContent] = useState(!isRightPanelCollapsed)
  useLayoutEffect(() => {
    // 1. 收起后等待实际尺寸过渡结束，再停止隐藏详情的查询和重算。
    if (!isRightPanelCollapsed) return
    const panel = panelElementRef.current
    if (!panel) return
    void getComputedStyle(panel).flexGrow
    let cancelled = false
    void Promise.allSettled(
      panel.getAnimations().map((animation) => animation.finished)
    ).then(() => {
      if (!cancelled) setHasPanelContent(false)
    })
    return () => {
      cancelled = true
    }
  }, [isRightPanelCollapsed])
  if (!isRightPanelCollapsed && !hasPanelContent) setHasPanelContent(true)
  const [isDrawerOpen, setIsDrawerOpen] = useState(false)
  const [isTabDrawerOpen, setIsTabDrawerOpen] = useState(false)
  const [debugSelection, setDebugSelection] = useState<{
    sync: ConversationSync
    target: ItemDebugTarget
  }>()
  const debugTarget =
    debugSelection?.sync === lastSync ? debugSelection.target : undefined

  // 桌面收起右栏，或按最近一次展开时的宽度展开；窄屏开关抽屉
  function toggleRightPanel() {
    if (!isDesktop) {
      setIsDrawerOpen(!isDrawerOpen)
      return
    }
    const panel = rightPanelRef.current
    if (!panel) return
    panelGroupRef.current?.setAttribute("data-panel-transition", "")
    if (!panel.isCollapsed()) panel.collapse()
    else if (layout[rightPanelID]) panel.resize(layout[rightPanelID] + "%")
    else panel.expand()
  }

  // 打开选择的记录并显示现有详情区域，不切换正文的展开选择。
  const inspectItem = useCallback(
    (tabID: string, sync: ConversationSync, target: ItemDebugTarget) => {
      // 1. 先把这个标签页设成最近选中的对话，保存定位信息，切到调试页签，再展开当前尺寸下的详情布局。
      workspace.selectTab(tabID)
      setDebugSelection({ sync, target })
      $rightPanelTab.set("debug")
      if (!isDesktop) {
        setIsDrawerOpen(true)
        return
      }
      const panel = rightPanelRef.current
      if (!panel?.isCollapsed()) return
      panelGroupRef.current?.setAttribute("data-panel-transition", "")
      if (layout[rightPanelID]) panel.resize(layout[rightPanelID] + "%")
      else panel.expand()
    },
    [workspace, isDesktop, rightPanelRef, layout]
  )

  return (
    // 工作区占满内容区域，右栏在它右边。
    <div ref={viewRef} className="flex min-h-0 flex-1 flex-col">
      <WorkspaceTopBar
        workspace={workspace}
        selectedTabID={selectedTabID}
        tab={tabs[selectedTabID]}
        tabCount={Object.keys(tabs).length}
        rightPanel={{
          isOpen: isDesktop ? !isRightPanelCollapsed : isDrawerOpen,
          isMobile,
          onToggle: toggleRightPanel,
        }}
        onOpenTabDrawer={() => setIsTabDrawerOpen(true)}
      />
      {/* 窄屏也保留同一个 Group，跨过断点时工作区不重新挂载 */}
      <ResizablePanelGroup
        elementRef={panelGroupRef}
        onPointerDownCapture={() =>
          panelGroupRef.current?.removeAttribute("data-panel-transition")
        }
        onKeyDownCapture={() =>
          panelGroupRef.current?.removeAttribute("data-panel-transition")
        }
        defaultLayout={
          isRightPanelCollapsed
            ? { [conversationPanelID]: 100, [rightPanelID]: 0 }
            : layout
        }
        // 窄屏没有右栏时不记；收起时只记收起，宽度留给下次展开
        onLayoutChanged={(next) => {
          if (!(rightPanelID in next)) return
          $isRightPanelCollapsed.set(next[rightPanelID] === 0)
          if (next[rightPanelID] > 0) $conversationLayout.set(next)
        }}
        className="conversation-panel-transition min-h-0 flex-1"
      >
        <ResizablePanel
          id={conversationPanelID}
          minSize={isDesktop ? "360px" : 0}
          className="flex min-h-0 flex-col"
          style={{ overflow: "hidden" }}
        >
          <WorkspaceLayout workspace={workspace} onInspect={inspectItem} />
        </ResizablePanel>
        {isDesktop && (
          <>
            {/* 和区域之间的分隔条一样：1px 的线，可拖动的范围 6px，悬停和拖动时才有高亮 */}
            <ResizableHandle className="after:w-1.5 data-[separator=active]:after:bg-ring/35 data-[separator=hover]:after:bg-ring/35" />
            <ResizablePanel
              id={rightPanelID}
              panelRef={rightPanelRef}
              elementRef={panelElementRef}
              collapsible
              defaultSize="35%"
              minSize="280px"
              maxSize="60%"
              className="flex min-h-0 flex-col"
              style={{ overflow: "hidden" }}
            >
              {/* 尺寸过渡结束后停止隐藏详情的查询与 RPC 重算。 */}
              {(!isRightPanelCollapsed || hasPanelContent) && (
                <WorkspaceDetails
                  sync={lastSync}
                  debugTarget={debugTarget}
                  onClose={() => {
                    panelGroupRef.current?.setAttribute(
                      "data-panel-transition",
                      ""
                    )
                    rightPanelRef.current?.collapse()
                  }}
                />
              )}
            </ResizablePanel>
          </>
        )}
      </ResizablePanelGroup>
      {!isDesktop && (
        <Drawer open={isDrawerOpen} onOpenChange={setIsDrawerOpen}>
          <DrawerContent className="text-body [--drawer-content-height:85svh] [--drawer-content-max-height:85svh]">
            <DrawerTitle className="sr-only text-section">
              {t("conversation.panel.title")}
            </DrawerTitle>
            <WorkspaceDetails
              sync={lastSync}
              debugTarget={debugTarget}
              onClose={() => setIsDrawerOpen(false)}
            />
          </DrawerContent>
        </Drawer>
      )}
      {isMobile && (
        <WorkspaceTabDrawer
          workspace={workspace}
          open={isTabDrawerOpen}
          onOpenChange={setIsTabDrawerOpen}
        />
      )}
    </div>
  )
}

// 右栏内容：只在调试页签订阅消息内容。
function WorkspaceDetails({
  sync,
  debugTarget,
  onClose,
}: {
  sync: ConversationSync
  debugTarget?: ItemDebugTarget
  onClose(): void
}) {
  // 1. 读取调试页签当前需要的展示行和同步状态。
  const syncState = useStore(sync.$syncState)
  const messageRows = useStore(getConversationMessageStores(sync).$debugRows)
  return (
    <RightPanel
      sync={sync}
      isReady={sync.hasInitialData() && syncState !== "failed"}
      debugTarget={debugTarget}
      messageRows={messageRows}
      onClose={onClose}
    />
  )
}

// 让地址栏和工作区互相跟随：用户跳到对话地址时选中主对话，跳到子 Agent 地址时打开子 Agent；
// 根 Session 的地址和对话地址一样，选中主对话后把地址栏改成对话地址；选中的标签页变了就改地址栏
function useWorkspaceRouteSync(workspace: TabWorkspace) {
  const serverID = useServerConnection().getServerID()
  const sessionID = useMatch(
    "/s/:serverID/c/:conversationID/sessions/:sessionID"
  )?.params.sessionID
  const rootSessionID = useStore(workspace.getRootSync().$rootSessionID)
  // 地址里的 Session 要等根 Session 的 ID 知道后，才能判断它是子 Agent 还是根 Session
  const isSessionAddressPending =
    sessionID !== undefined && rootSessionID === undefined
  const subAgentSessionID = sessionID === rootSessionID ? undefined : sessionID
  const { key, pathname, search } = useLocation()
  const navigate = useNavigate()
  const navigationType = useNavigationType()
  const selectedTabID = useStore(workspace.$selectedTabID)
  const tabs = useStore(workspace.$tabs)
  const previousURL = useRef(pathname + search)
  useEffect(() => {
    // 1. 地址变了的 REPLACE 是下面第 4 步改地址栏或侧栏改搜索词，不是用户跳转，不动工作区；
    //    点链接跳到和当前相同的地址也是 REPLACE，但地址没变，仍是用户跳转。
    const url = pathname + search
    const isURLChanged = url !== previousURL.current
    previousURL.current = url
    if (navigationType === "REPLACE" && isURLChanged) return
    // 2. 地址里的 Session 还不知道是不是根 Session：等根 Session 的 ID 知道后这个 effect 会再跑一次。
    if (isSessionAddressPending) return
    // 3. 子 Agent 地址：打开那个子 Agent；对话地址和根 Session 的地址：选中主对话。
    if (subAgentSessionID)
      workspace.openSubAgent(subAgentSessionID, getIsMobile())
    else workspace.openMain()
  }, [
    workspace,
    isSessionAddressPending,
    subAgentSessionID,
    key,
    pathname,
    search,
    navigationType,
  ])
  useEffect(() => {
    // 4. 选中的标签页有自己的地址时，地址栏改成它；读最新的选中状态，上一个 effect 刚改过工作区。
    if (isSessionAddressPending) return
    const path = getTabPath(
      serverID,
      workspace,
      workspace.$tabs.get()[workspace.$selectedTabID.get()]
    )
    if (!path || path === pathname) return
    void navigate(
      { pathname: path, search: getConversationListSearch(search) },
      { replace: true }
    )
  }, [
    workspace,
    serverID,
    isSessionAddressPending,
    selectedTabID,
    tabs,
    pathname,
    search,
    navigate,
  ])
}

// 标签页对应的地址：主对话是对话页，子 Agent 是它的 Session 页，旁支和终端没有自己的地址
function getTabPath(
  serverID: string,
  workspace: TabWorkspace,
  tab: TabWorkspaceTab | undefined
): string | undefined {
  switch (tab?.kind) {
    case "main":
      return getConversationPath(serverID, workspace.conversationID)
    case "sub-agent":
      return getSessionPath(serverID, workspace.conversationID, tab.sessionID)
    default:
      return undefined
  }
}
