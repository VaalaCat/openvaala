import {
  useCallback,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ComponentProps,
} from "react"
import { useStore } from "@nanostores/react"
import { computed, type ReadableAtom } from "nanostores"
import { useMutation } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useParams, useMatch } from "react-router"
import { usePanelRef } from "react-resizable-panels"
import { DetailPageState } from "@/components/base/detail-page-state"
import {
  ContextUsageButton,
  SessionControls,
} from "@/components/conversation/session-controls"
import { Composer } from "@/components/conversation/composer"
import { ConversationTopBar } from "@/components/conversation/conversation-top-bar"
import { MessageScroller } from "@/components/conversation/message-scroller"
import { RightPanel } from "@/components/conversation/right-panel"
import { useServerConnection } from "@/components/server-connection-provider"
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable"
import { Drawer, DrawerContent, DrawerTitle } from "@/components/ui/drawer"
import { useConversationSync } from "@/hooks/use-conversation-sync"
import { useIsDesktop } from "@/hooks/use-is-desktop"
import { useIsMobile } from "@/hooks/use-mobile"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import {
  ErrorCodeSessionNotFound,
  type SessionID,
  type ModelSelection,
} from "@/lib/api/protocol.gen"
import {
  $conversationLayout,
  $isRightPanelCollapsed,
  $rightPanelTab,
} from "@/lib/appearance/preferences"
import {
  getMessageRows,
  type ItemDebugTarget,
  type MessageRow,
} from "@/lib/item-renderers"
import { getConversationPath, getServerPath } from "@/lib/paths"
import { notifyFailure } from "@/lib/notify"
import { RpcError, shouldShowLoadError } from "@/lib/rpc-error"

// 桌面布局里两个 Panel 的 id，也是 $conversationLayout 里的 key
const conversationPanelID = "conversation"
const rightPanelID = "right-panel"
const emptyMessageRows: MessageRow[] = []

// 对话阅读区与调试页签共用展示分组和已加载分页接缝。
function createConversationMessageStores(sync: ConversationSync) {
  // 1. 从唯一消息源派生展示行，保留前一次行引用和历史页接缝。
  let firstKey: string | undefined
  let previous: { rows: MessageRow[]; pageSeams: ReadonlySet<string> } = {
    rows: [],
    pageSeams: new Set(),
  }
  const $messageRows = computed(sync.$items, (items) => {
    let pageSeams = previous.pageSeams
    if (firstKey !== items[0]?.key) {
      if (firstKey && items.some((view) => view.key === firstKey))
        pageSeams = new Set([...pageSeams, firstKey])
      firstKey = items[0]?.key
    }
    const rows = getMessageRows(items, pageSeams, previous)
    previous = { rows, pageSeams }
    return rows
  })
  // 2. 非调试页签不随消息正文更新。
  const $debugRows = computed([$messageRows, $rightPanelTab], (rows, tab) =>
    tab === "debug" ? rows : emptyMessageRows
  )
  return { $messageRows, $debugRows }
}

// 对话路由族保留公共布局，每个目标单独创建并运行同步。
export function ConversationPage() {
  // 1. 主、子 Session 共用父路由，切换时只替换同步目标。
  const { conversationID = "" } = useParams()
  const sessionID = useMatch(
    "/s/:serverID/c/:conversationID/sessions/:sessionID"
  )?.params.sessionID
  const [syncAttempt, setSyncAttempt] = useState(0)
  const sync = useConversationSync(conversationID, sessionID, syncAttempt)
  return (
    <ConversationView
      sync={sync}
      sessionID={sessionID}
      onRetry={() => setSyncAttempt((attempt) => attempt + 1)}
    />
  )
}

// 在消息位置展示首次同步、资源不存在或业务错误。
function ConversationContentState({
  sync,
  sessionID,
  onRetry,
}: {
  sync: ConversationSync
  sessionID?: SessionID
  onRetry(): void
}) {
  // 1. 子 Session 不存在时提供返回主 Session 的入口。
  const { t } = useTranslation()
  const syncError = useStore(sync.$syncError)
  const isSessionMissing =
    !!sessionID &&
    syncError instanceof RpcError &&
    syncError.code === ErrorCodeSessionNotFound
  const returnPath = isSessionMissing
    ? getConversationPath(sync.serverID, sync.conversationID)
    : getServerPath(sync.serverID)
  return (
    <DetailPageState
      embedded
      id={sync.conversationID}
      title={t("common.loading")}
      entity={t(
        isSessionMissing ? "conversation.session.entity" : "conversation.entity"
      )}
      parents={[]}
      notFoundReturn={{
        to: returnPath,
        label: t(
          isSessionMissing
            ? "conversation.session.backToRoot"
            : "conversation.backToNewConversation"
        ),
      }}
      missingIdText=""
      shouldShowError={shouldShowLoadError}
      query={{ error: syncError, refetch: onRetry }}
    >
      {() => null}
    </DetailPageState>
  )
}

// 消息区订阅展示行，正文更新只进入阅读区域。
function ConversationMessages({
  $messageRows,
  ...props
}: Omit<ComponentProps<typeof MessageScroller>, "rows"> & {
  $messageRows: ReadableAtom<MessageRow[]>
}) {
  // 1. 读取消息区与调试共用的展示行。
  const rows = useStore($messageRows)
  return <MessageScroller {...props} rows={rows} />
}

// 详情区只在调试页签订阅消息内容。
function ConversationDetails({
  $debugRows,
  ...props
}: Omit<ComponentProps<typeof RightPanel>, "messageRows"> & {
  $debugRows: ReadableAtom<MessageRow[]>
}) {
  // 1. 读取调试页签当前需要的展示行。
  const messageRows = useStore($debugRows)
  return <RightPanel {...props} messageRows={messageRows} />
}

// 展示同步好的对话；宽容器保留可拖右栏，窄容器从底部打开详情抽屉。
function ConversationView({
  sync,
  sessionID,
  onRetry,
}: {
  sync: ConversationSync
  sessionID?: SessionID
  onRetry(): void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
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
  const syncState = useStore(sync.$syncState)
  const hasInitialData = sync.hasInitialData()
  const isReady = syncState === "synced"
  const isConnected = useStore(connection.$state) === "connected"
  const followedSessionID = hasInitialData
    ? sync.getFollowedSessionID()
    : (sessionID ?? "")
  // 1. 阅读区与调试共用同一份展示分组，历史向前追加时保留已有组的首成员。
  const { $messageRows, $debugRows } = useMemo(
    () => createConversationMessageStores(sync),
    [sync]
  )
  const [debugSelection, setDebugSelection] = useState<{
    sync: ConversationSync
    target: ItemDebugTarget
  }>()
  const debugTarget =
    debugSelection?.sync === sync ? debugSelection.target : undefined
  const sessionInfo = useStore(sync.$sessions, { keys: [followedSessionID] })[
    followedSessionID
  ]

  const setModel = useMutation({
    mutationFn: (params: {
      conversation_id: string
      session_id: SessionID
      model: ModelSelection
    }) => connection.call("session/setModel", params),
    onError: (error) => notifyFailure(error, t),
  })
  const sessionControls = (
    <SessionControls
      agentName={sessionInfo?.session.agent_name}
      value={sessionInfo?.session.model}
      onChange={(model) =>
        sessionInfo &&
        setModel.mutate({
          conversation_id: sync.conversationID,
          session_id: followedSessionID,
          model,
        })
      }
      disabled={!sessionInfo || !isReady || !isConnected}
    />
  )
  const contextUsage = (
    <ContextUsageButton
      sync={sync}
      sessionInfo={sessionInfo}
      canCompact={!sessionID}
      disabled={!sessionInfo || !isReady || !isConnected}
    />
  )

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
    (target: ItemDebugTarget) => {
      // 1. 保存定位信息，切到调试页签，再展开当前尺寸下的详情布局。
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
    [isDesktop, layout, rightPanelRef, sync]
  )

  return (
    // 消息区占满内容区域，滚动条贴着右边。
    <div ref={viewRef} className="flex min-h-0 flex-1 flex-col">
      <ConversationTopBar
        sync={sync}
        sessionInfo={sessionInfo}
        isReady={hasInitialData}
        sessionID={sessionID}
        isSubSession={!!sessionID}
        rightPanel={{
          isOpen: isDesktop ? !isRightPanelCollapsed : isDrawerOpen,
          isMobile,
          onToggle: toggleRightPanel,
        }}
      />
      {/* 窄屏也保留同一个 Group，跨过断点时消息区不重新挂载 */}
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
          <ConversationMessages
            sync={sync}
            isReady={hasInitialData && syncState !== "failed"}
            loadingContent={
              <ConversationContentState
                sync={sync}
                sessionID={sessionID}
                onRetry={onRetry}
              />
            }
            isSubSession={!!sessionID}
            $messageRows={$messageRows}
            footer={
              sessionID ? (
                <div className="flex items-center gap-4 py-2">
                  {sessionControls}
                  {contextUsage}
                </div>
              ) : (
                <Composer
                  serverID={sync.serverID}
                  conversationID={sync.conversationID}
                  sync={sync}
                  isReady={isReady}
                  isRunning={!!sessionInfo?.active_turn_id}
                  sessionControls={sessionControls}
                  contextUsage={contextUsage}
                />
              )
            }
            onInspect={inspectItem}
          />
        </ResizablePanel>
        {isDesktop && (
          <>
            <ResizableHandle />
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
                <ConversationDetails
                  sync={sync}
                  isReady={hasInitialData && syncState !== "failed"}
                  debugTarget={debugTarget}
                  $debugRows={$debugRows}
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
            <ConversationDetails
              sync={sync}
              isReady={hasInitialData && syncState !== "failed"}
              debugTarget={debugTarget}
              $debugRows={$debugRows}
              onClose={() => setIsDrawerOpen(false)}
            />
          </DrawerContent>
        </Drawer>
      )}
    </div>
  )
}
