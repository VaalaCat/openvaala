import { CircleAlert, ListTree } from "lucide"
import { LayoutGroup, motion, useReducedMotion } from "motion/react"
import {
  memo,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react"
import { ArrowDownIcon, BracesIcon } from "lucide-react"
import { useStore } from "@nanostores/react"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useStickToBottom } from "use-stick-to-bottom"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { RequestError } from "@/components/base/request-error"
import { RotatingText } from "@/components/base/rotating-text"
import { ItemHeader, ItemStatus } from "@/components/conversation/item-notice"
import { ItemView } from "@/components/conversation/item-view"
import { MessageRowBoundary } from "@/components/conversation/message-row-boundary"
import { TurnDivider } from "@/components/conversation/turn-divider"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent } from "@/components/ui/collapsible"
import { LatticeLoader } from "@/components/base/lattice-loader"
import {
  getTurnSavedDurations,
  type ViewItem,
  type TurnView,
} from "@/lib/api/conversation-items"
import type { Turn } from "@/lib/api/protocol.gen"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import {
  ItemDisclosureContext,
  getProcessFacts,
  getProcessActionText,
  getTurnResult,
  getReplyToCopy,
  getMessageSource,
  type ItemRow,
  type MessageRow,
  type ItemDebugTarget,
  type TurnContentRow,
} from "@/lib/item-renderers"
import { turnsOptions } from "@/lib/query/session"
import { getSessionName } from "@/lib/session-tree"
import { getStringField, parseToolArguments } from "@/lib/tool-arguments"
import { shouldShowLoadError } from "@/lib/rpc-error"
import { cn } from "@/lib/utils"

// 消息区的滚动锚点：视口里第一条露出来的 Item，和滚动位置是 knownScrollTop 时它离滚动区顶部的距离
interface ScrollAnchor {
  element: HTMLElement
  viewportTop: number
  knownScrollTop: number
  fallbacks: HTMLElement[]
}

// 消息区：拥有消息列的滚动。贴底跟随交给 use-stick-to-bottom，用户往上翻就停止跟随并出现「回到底部」；
// 滚到顶部时加载更早的 Item。滚动锚定自己做（同浏览器的 overflow-anchor）：锚点总是视口里第一条露出来的 Item，
// 插入更早的 Item、图片撑开、Turn 记录读到后行高变化让它挪了位置，就补回 scrollTop，正在看的位置不跳。每一行各有错误边界；
export function MessageScroller({
  sync,
  isReady,
  loadingContent,
  isSubSession,
  rows,
  footer,
  onInspect,
}: {
  sync: ConversationSync
  isReady: boolean
  loadingContent: ReactNode
  isSubSession: boolean
  rows: MessageRow[]
  footer: ReactNode
  onInspect(target: ItemDebugTarget): void
}) {
  const { t } = useTranslation()
  const reducedMotion = useReducedMotion()
  const connection = useServerConnection()
  const items = useStore(sync.$items)
  const replyToCopy = useMemo(() => getReplyToCopy(items), [items])
  const hasEarlierItems = useStore(sync.$hasEarlierItems)
  const turnViews = useStore(sync.$turns)
  useLayoutEffect(() => {
    if (isReady) sync.markItemsPresented(items.filter(hasItemPresentation))
  }, [sync, items, isReady])
  // 1. 消息区保存所有展开选择，阅读与调试使用父层提供的同一批行。
  const [choices, setChoices] = useState<Record<string, boolean | undefined>>(
    {}
  )
  const chooseOpen = useCallback((key: string | string[], open: boolean) => {
    setChoices((previous) => ({
      ...previous,
      ...Object.fromEntries(
        (Array.isArray(key) ? key : [key]).map((entry) => [entry, open])
      ),
    }))
  }, [])
  const turnSavedDurations = useMemo(
    () => getTurnSavedDurations(items),
    [items]
  )
  const turnIDs = useMemo(
    () => rows.flatMap((row) => (row.kind === "turn" ? [row.turnID] : [])),
    [rows]
  )
  const turns = useQuery({
    ...turnsOptions(
      connection,
      sync.conversationID,
      isReady ? sync.getFollowedSessionID() : "",
      turnIDs
    ),
    enabled: isReady && turnIDs.length > 0,
  })
  const turnsByID = useMemo(
    () => new Map(turns.data?.turns.map((turn) => [turn.id, turn])),
    [turns.data]
  )
  const {
    scrollRef,
    contentRef,
    isAtBottom,
    scrollToBottom,
    stopScroll,
    state: stickState,
  } = useStickToBottom({ initial: "instant", resize: "smooth" })
  const messagesRef = useRef<HTMLElement>(null)
  const anchorRef = useRef<ScrollAnchor | undefined>(undefined)
  const anchorFrameRef = useRef<number | undefined>(undefined)
  const updateAnchor = useCallback(() => {
    // 1. 提交和尺寸变化立即校正，并合并尚未执行的滚动测量。
    if (anchorFrameRef.current !== undefined) {
      cancelAnimationFrame(anchorFrameRef.current)
      anchorFrameRef.current = undefined
    }
    const scroller = scrollRef.current
    const content = contentRef.current
    if (scroller && content)
      keepAnchorInPlace(anchorRef, scroller, content, stickState.isAtBottom)
  }, [scrollRef, contentRef, stickState])

  // 1. 加载更早的 Item，失败时停下等用户重试。
  const [isLoadingEarlier, setIsLoadingEarlier] = useState(false)
  const [earlierItemsError, setEarlierItemsError] = useState<unknown>()
  const syncRef = useRef(sync)
  const [readingTarget, setReadingTarget] = useState(sync)
  if (readingTarget !== sync) {
    setReadingTarget(sync)
    setChoices({})
    setIsLoadingEarlier(false)
    setEarlierItemsError(undefined)
  }
  const isConnected = useStore(connection.$state) === "connected"
  const loadEarlierItems = useCallback(async () => {
    if (!isConnected || !isReady) return
    stopScroll()
    updateAnchor()
    setIsLoadingEarlier(true)
    setEarlierItemsError(undefined)
    try {
      await sync.loadEarlierItems()
    } catch (error) {
      if (syncRef.current === sync) setEarlierItemsError(error)
    } finally {
      if (syncRef.current === sync) setIsLoadingEarlier(false)
    }
  }, [sync, stopScroll, updateAnchor, isConnected, isReady])

  // 2. 顶部哨兵进入视口时加载：每次滚动和依赖变化时直接比较哨兵和滚动区的位置，不依赖 IntersectionObserver 报告的进出变化。
  // 首屏哨兵要先离开过视口才按滚动触发，短内容由用户向上翻页的手势触发。
  const sentinelRef = useRef<HTMLDivElement>(null)
  const hasLeftTopRef = useRef(false)
  useEffect(() => {
    const sentinel = sentinelRef.current
    const scroller = scrollRef.current
    if (!sentinel || !scroller || !hasEarlierItems) return
    if (
      !isConnected ||
      !isReady ||
      isLoadingEarlier ||
      (earlierItemsError && shouldShowLoadError(earlierItemsError))
    )
      return
    const checkSentinel = () => {
      const isSentinelVisible =
        sentinel.getBoundingClientRect().bottom >
        scroller.getBoundingClientRect().top
      if (!isSentinelVisible) {
        hasLeftTopRef.current = true
        return
      }
      if (hasLeftTopRef.current) {
        hasLeftTopRef.current = false
        void loadEarlierItems()
      }
    }
    // 内容不足一屏时，向上滚轮或下拉手势每次只加载一页。
    const loadFromGesture = () => {
      if (scroller.scrollTop <= 0) void loadEarlierItems()
    }
    const handleWheel = (event: WheelEvent) => {
      if (event.deltaY < 0) loadFromGesture()
    }
    let touchStartY = 0
    const handleTouchStart = (event: TouchEvent) => {
      touchStartY = event.touches[0]?.clientY ?? 0
    }
    const handleTouchMove = (event: TouchEvent) => {
      if ((event.touches[0]?.clientY ?? 0) > touchStartY) loadFromGesture()
    }
    const handleKeyDown = (event: KeyboardEvent) => {
      if (
        event.target !== messagesRef.current ||
        !["ArrowUp", "PageUp", "Home"].includes(event.key) ||
        scroller.scrollTop > 0
      )
        return
      event.preventDefault()
      loadFromGesture()
    }
    checkSentinel()
    scroller.addEventListener("keydown", handleKeyDown)
    scroller.addEventListener("scroll", checkSentinel, { passive: true })
    scroller.addEventListener("wheel", handleWheel, { passive: true })
    scroller.addEventListener("touchstart", handleTouchStart, { passive: true })
    scroller.addEventListener("touchmove", handleTouchMove, { passive: true })
    return () => {
      scroller.removeEventListener("keydown", handleKeyDown)
      scroller.removeEventListener("scroll", checkSentinel)
      scroller.removeEventListener("wheel", handleWheel)
      scroller.removeEventListener("touchstart", handleTouchStart)
      scroller.removeEventListener("touchmove", handleTouchMove)
    }
  }, [
    scrollRef,
    isConnected,
    isReady,
    hasEarlierItems,
    isLoadingEarlier,
    earlierItemsError,
    loadEarlierItems,
  ])

  // 3. 提交和尺寸变化同步校正阅读位置，滚动事件每帧测量一次。
  useLayoutEffect(() => {
    // 1. 新目标重置阅读锚点，保留公共滚动容器。
    syncRef.current = sync
    anchorRef.current = undefined
    hasLeftTopRef.current = false
    if (scrollRef.current) scrollRef.current.scrollTop = 0
  }, [sync, scrollRef])
  useLayoutEffect(() => {
    updateAnchor()
  }, [rows, choices, turns.data, hasEarlierItems, updateAnchor])
  useEffect(() => {
    const scroller = scrollRef.current
    const content = contentRef.current
    if (!scroller || !content) return
    const handleScroll = () => {
      anchorFrameRef.current ??= requestAnimationFrame(updateAnchor)
    }
    const observer = new ResizeObserver(updateAnchor)
    observer.observe(content)
    // 窗口高度或手机键盘改变消息区高度时，贴底阅读继续停在最底部。
    let viewportHeight = scroller.clientHeight
    const viewportObserver = new ResizeObserver(() => {
      // 1. 和内容尺寸变化一样记下高度差，浏览器随之收回滚动位置不算用户上滚。
      const difference = viewportHeight - scroller.clientHeight
      viewportHeight = scroller.clientHeight
      if (difference === 0) return
      stickState.resizeDifference = difference
      requestAnimationFrame(() => {
        setTimeout(() => {
          if (stickState.resizeDifference === difference)
            stickState.resizeDifference = 0
        }, 1)
      })
      // 2. 贴底时本帧补上被遮住的距离。
      if (
        stickState.isAtBottom &&
        scroller.scrollTop < stickState.targetScrollTop
      )
        stickState.scrollTop = stickState.targetScrollTop
    })
    viewportObserver.observe(scroller)
    scroller.addEventListener("scroll", handleScroll, { passive: true })
    return () => {
      observer.disconnect()
      viewportObserver.disconnect()
      scroller.removeEventListener("scroll", handleScroll)
      if (anchorFrameRef.current !== undefined) {
        cancelAnimationFrame(anchorFrameRef.current)
        anchorFrameRef.current = undefined
      }
    }
  }, [scrollRef, contentRef, updateAnchor, stickState])

  return (
    <div
      ref={scrollRef}
      data-slot="message-scroller"
      // 滚动锚定由第 3 步做，关掉浏览器自己的，避免补两次；relative 让 sr-only 这类绝对定位的文字随消息滚动、在这里裁剪
      className="relative min-h-0 flex-1 [scrollbar-gutter:stable] overflow-y-auto [overflow-anchor:none]"
    >
      {/* 消息和输入区共用这一列的宽度；输入区滚动时贴在底部 */}
      <div
        ref={contentRef}
        className="mx-auto flex min-h-full w-full max-w-3xl min-w-0 flex-col gap-4 px-3 pt-4 md:px-6"
      >
        <section
          ref={messagesRef}
          tabIndex={0}
          aria-label={t("conversation.messages")}
          className="flex min-w-0 flex-col gap-4"
        >
          {!isReady && loadingContent}
          {isReady && hasEarlierItems && (
            <div
              ref={sentinelRef}
              className="flex min-h-8 flex-col items-center justify-center"
            >
              {earlierItemsError && shouldShowLoadError(earlierItemsError) ? (
                <RequestError
                  error={earlierItemsError}
                  onRetry={() => void loadEarlierItems()}
                />
              ) : (
                <LatticeLoader
                  aria-label={t("conversation.loadingEarlier")}
                  className={isLoadingEarlier ? "color-muted" : "invisible"}
                />
              )}
            </div>
          )}
          {isReady && rows.length === 0 && (
            <p className="py-8 text-center text-body color-muted">
              {t("conversation.empty")}
            </p>
          )}
          {isReady &&
            rows.map((row) =>
              row.kind === "turn" ? (
                <TurnTimeline
                  key={row.key}
                  turnID={row.turnID}
                  rows={row.rows}
                  items={row.items}
                  replyKeyToCopy={
                    replyToCopy?.turnID === row.turnID
                      ? replyToCopy.key
                      : undefined
                  }
                  turn={turnsByID.get(row.turnID)}
                  turnView={turnViews[row.turnID]}
                  isLoadingTurn={turns.isFetching && !turnsByID.has(row.turnID)}
                  savedDurationMs={turnSavedDurations.get(row.turnID)}
                  sync={sync}
                  isSubSession={isSubSession}
                  choices={choices}
                  chooseOpen={chooseOpen}
                  onInspect={onInspect}
                />
              ) : (
                <TimelineItem
                  key={row.view.key}
                  row={row}
                  copyReply={row.view.key === replyToCopy?.key}
                  sync={sync}
                  isSubSession={isSubSession}
                  choices={choices}
                  chooseOpen={chooseOpen}
                  onInspect={onInspect}
                />
              )
            )}
        </section>
        <div className="sticky bottom-0 z-10 mt-auto bg-background pb-3">
          <motion.div
            initial={false}
            animate={{
              opacity: isAtBottom ? 0 : 1,
              y: isAtBottom && !reducedMotion ? 4 : 0,
            }}
            transition={{
              duration: reducedMotion ? 0 : 0.14,
              ease: "easeOut",
            }}
            inert={isAtBottom}
            aria-hidden={isAtBottom}
            className={cn(
              "absolute bottom-full left-1/2 mb-4 -translate-x-1/2",
              isAtBottom && "pointer-events-none"
            )}
          >
            <Tooltip disabled={isAtBottom}>
              <TooltipTrigger
                render={
                  <Button
                    variant="outline"
                    size="icon"
                    disabled={isAtBottom}
                    tabIndex={isAtBottom ? -1 : 0}
                    aria-label={t("conversation.scrollToBottom")}
                    className="shadow-sm"
                    onClick={() => {
                      messagesRef.current?.focus({ preventScroll: true })
                      void scrollToBottom({
                        animation: reducedMotion ? "instant" : "smooth",
                      })
                    }}
                  />
                }
              >
                <ArrowDownIcon />
              </TooltipTrigger>
              <TooltipContent className="text-caption">
                {t("conversation.scrollToBottom")}
              </TooltipContent>
            </Tooltip>
          </motion.div>
          {footer}
        </div>
      </div>
    </div>
  )
}

interface TimelineChoices {
  choices: Record<string, boolean | undefined>
  chooseOpen: (key: string | string[], open: boolean) => void
  onInspect(target: ItemDebugTarget): void
}

// Turn 从运行开始保留同一父节点，收起时只改变原位置记录的可见性。
const TurnTimeline = memo(function TurnTimeline({
  turnID,
  rows,
  items,
  replyKeyToCopy,
  turn,
  turnView,
  isLoadingTurn,
  savedDurationMs,
  sync,
  isSubSession,
  choices,
  chooseOpen,
  onInspect,
}: TimelineChoices & {
  turnID: string
  rows: TurnContentRow[]
  items: ViewItem[]
  replyKeyToCopy?: string
  turn?: Turn
  turnView?: TurnView
  isLoadingTurn: boolean
  savedDurationMs?: number
  sync: ConversationSync
  isSubSession: boolean
}) {
  const { t } = useTranslation()
  const reducedMotion = useReducedMotion()
  // 1. 完整成功轮次确认最终回复，其他模型回复各自显示中途回复入口。
  const result = getTurnResult(items, turn, turnView)
  const resultKeys = new Set(result.map((view) => view.key))
  const isRunning = (turnView?.status ?? turn?.status) === "running"
  const rawAction = useMemo(
    () => (
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              variant="ghost"
              size="icon"
              className="color-muted"
              aria-label={t("conversation.debug.inspect")}
              onClick={() => onInspect({ kind: "turn", turnID })}
            />
          }
        >
          <BracesIcon />
        </TooltipTrigger>
        <TooltipContent className="text-caption">
          {t("conversation.debug.inspect")}
        </TooltipContent>
      </Tooltip>
    ),
    [t, onInspect, turnID]
  )
  // 2. 相邻分组只保留一个标题，Hook 与消息按原顺序独立显示。
  const content = rows.map((row) => {
    if (row.kind === "item")
      return (
        <TimelineItem
          key={row.view.key}
          row={row}
          sync={sync}
          isSubSession={isSubSession}
          choices={choices}
          chooseOpen={chooseOpen}
          onInspect={onInspect}
          copyReply={row.view.key === replyKeyToCopy}
          isProgressMessage={
            result.length > 0 &&
            row.view.item.kind === "message" &&
            getMessageSource(row.view.item) === "reply" &&
            !resultKeys.has(row.view.key)
          }
        />
      )
    const key = row.key
    const isEnteringGroup = sync.isUnpresentedLiveItem(key)
    const groupRows = row.rows
    const isSingle = row.groupKind === "model_errors" && groupRows.length === 1
    const groupOpen =
      choices["group:" + key] ??
      groupRows.some((member) => choices["item:" + member.view.key] === true)
    // 未阅读的过程组只渲染摘要；读过的成员保留 DOM，收起不打断选择或滚动。
    const mountMembers =
      row.groupKind === "model_errors" ||
      choices["group:" + key] !== undefined ||
      groupRows.some(
        (member) => choices["item:" + member.view.key] !== undefined
      )
    return (
      <Collapsible
        key={key}
        render={
          <motion.div
            initial={isEnteringGroup && !reducedMotion ? { opacity: 0 } : false}
            animate={{ opacity: 1 }}
            transition={{ duration: reducedMotion ? 0 : 0.16, ease: "easeOut" }}
          />
        }
        open={isSingle || groupOpen}
        onOpenChange={(open) => chooseOpen("group:" + key, open)}
        className="flex min-w-0 flex-col gap-2"
      >
        <div data-anchor-key={"group:" + key} hidden={isSingle}>
          <SummaryButton
            sync={sync}
            open={groupOpen}
            onOpenChange={(open) => chooseOpen("group:" + key, open)}
            rows={groupRows}
            isTurnRunning={isRunning}
            isTailGroup={row === rows.at(-1)}
            isModelErrors={row.groupKind === "model_errors"}
          />
        </div>
        <CollapsibleContent
          keepMounted
          className="process-disclosure-panel"
          data-fold-anchor={"group:" + key}
        >
          <div
            className={cn(
              "process-disclosure-content flex min-w-0 flex-col gap-2",
              !isSingle && "ml-2 border-l pl-2"
            )}
          >
            {mountMembers &&
              groupRows.map((itemRow) => (
                <TimelineItem
                  key={itemRow.view.key}
                  suppressEntrance={isEnteringGroup || !(isSingle || groupOpen)}
                  row={itemRow}
                  sync={sync}
                  isSubSession={isSubSession}
                  choices={choices}
                  chooseOpen={chooseOpen}
                  onInspect={onInspect}
                />
              ))}
          </div>
        </CollapsibleContent>
      </Collapsible>
    )
  })
  content.push(
    <MessageRowBoundary key="divider" record={turn ?? turnID}>
      <TurnDivider
        turnID={turnID}
        turn={turn}
        isLoadingTurn={isLoadingTurn}
        savedDurationMs={savedDurationMs}
        sync={sync}
        rawDataAction={rawAction}
      />
    </MessageRowBoundary>
  )
  return (
    <div data-turn-id={turnID} className="flex min-w-0 flex-col gap-4">
      {content}
    </div>
  )
})

// 原记录保留稳定 Item 标识，内容组件共用消息区按 ItemID 保存的展开选择。
const TimelineItem = memo(function TimelineItem({
  row,
  sync,
  isSubSession,
  choices,
  chooseOpen,
  onInspect,
  isProgressMessage = false,
  copyReply = false,
  suppressEntrance = false,
}: TimelineChoices & {
  row: ItemRow
  sync: ConversationSync
  isSubSession: boolean
  isProgressMessage?: boolean
  copyReply?: boolean
  suppressEntrance?: boolean
}) {
  const reducedMotion = useReducedMotion()
  // 1. 只在本条 Item 的选择变化时更新上下文，其他 Item 的流式通知不刷新它。
  const itemKey = row.view.key
  const key = "item:" + itemKey
  const choice = choices[key]
  const disclosure = useMemo(
    () => ({
      open: choice ?? false,
      isChosen: choice !== undefined,
      onOpenChange: (open: boolean) => chooseOpen(key, open),
      onInspect: () => onInspect({ kind: "item", key: itemKey }),
    }),
    [choice, chooseOpen, itemKey, key, onInspect]
  )
  if (!hasItemPresentation(row.view)) return null
  return (
    <motion.div
      initial={
        !suppressEntrance &&
        !reducedMotion &&
        sync.isUnpresentedLiveItem(itemKey)
          ? { opacity: 0 }
          : false
      }
      animate={{ opacity: 1 }}
      transition={{ duration: reducedMotion ? 0 : 0.16, ease: "easeOut" }}
      data-item-key={row.view.key}
      data-anchor-key={
        row.view.item.kind === "message" &&
        getMessageSource(row.view.item) === "reply"
          ? undefined
          : key
      }
    >
      <ItemDisclosureContext value={disclosure}>
        <MessageRowBoundary
          record={row.view.item}
          result={row.toolResult?.item}
        >
          <ItemView
            view={row.view}
            toolResult={row.toolResult}
            sync={sync}
            isSubSession={isSubSession}
            isProgressMessage={isProgressMessage}
            copyReply={copyReply}
          />
        </MessageRowBoundary>
      </ItemDisclosureContext>
    </motion.div>
  )
})

// 这条 Item 是否已经有可显示的正文或标题？
function hasItemPresentation(view: ViewItem): boolean {
  // 1. 模型回复等待第一段正文或图片，其他 Item 从标题开始展示。
  if (view.item.kind !== "message" || getMessageSource(view.item) !== "reply")
    return true
  return (view.item.message?.content ?? []).some(
    (content) => content?.text?.text.trim() || content?.image
  )
}

// 聚合标题显示工具和思考次数，当前动作与参数紧跟计数滚动切换。
function SummaryButton({
  sync,
  rows,
  isTurnRunning,
  isTailGroup,
  open,
  onOpenChange,
  isModelErrors = false,
}: {
  sync: ConversationSync
  rows: ItemRow[]
  isTurnRunning: boolean
  isTailGroup: boolean
  open: boolean
  onOpenChange: (open: boolean) => void
  isModelErrors?: boolean
}) {
  const { t } = useTranslation()
  const reducedMotion = useReducedMotion()
  // 1. 当前动作与整组状态从成员和所在位置计算。
  const facts = getProcessFacts(rows, isTurnRunning, isTailGroup)
  const state = isModelErrors ? "failed" : facts.state
  const isRunning = state === "running"
  const action = isRunning ? facts.action : undefined
  const call = action?.view.item.tool_call
  const recipientID = call
    ? getStringField(parseToolArguments(call.arguments).value, "session_id")
    : undefined
  const sessionID = sync.getFollowedSessionID()
  const sessions = useStore(sync.$sessions, {
    keys: recipientID ? [sessionID, recipientID] : [sessionID],
  })
  const recipient = recipientID ? sessions[recipientID] : undefined
  const actionText = getProcessActionText(action, t, {
    recipientName: recipient
      ? getSessionName(recipient, t("conversation.session.root"))
      : recipientID,
    agentName: sessions[sessionID]?.session.agent_name,
  })
  const label = isModelErrors
    ? t("conversation.modelError.title")
    : t("conversation.notice.toolResult")
  const countsText = isModelErrors
    ? label
    : [
        facts.toolCount > 0
          ? t("conversation.process.tools", { count: facts.toolCount })
          : "",
        facts.reasoningCount > 0
          ? t("conversation.process.reasoning", { count: facts.reasoningCount })
          : "",
      ]
        .filter(Boolean)
        .join(t("conversation.process.countsSeparator")) || label
  const exception = facts.hasFailures
    ? "failed"
    : facts.hasInterruptions
      ? "interrupted"
      : undefined
  return (
    <ItemHeader
      icon={isModelErrors ? CircleAlert : ListTree}
      titleTooltip={false}
      title={
        <LayoutGroup>
          <span className="flex min-w-0 items-center gap-2">
            <span
              data-process-counts=""
              className={cn("shrink-0 tabular-nums", isRunning && "shimmer")}
            >
              <RotatingText text={countsText} transitionKey={countsText} />
            </span>
            <motion.span
              layout={reducedMotion ? false : "position"}
              className="min-w-0 empty:hidden"
              transition={{ duration: reducedMotion ? 0 : 0.16 }}
            >
              <RotatingText
                text={actionText}
                transitionKey={
                  action ? `${action.view.key}:${action.phase}` : ""
                }
                className="process-action"
                exitDelayMs={800}
              />
            </motion.span>
          </span>
        </LayoutGroup>
      }
      status={
        !isModelErrors && exception ? (
          <ItemStatus
            state={exception}
            label={t(
              exception === "failed"
                ? "conversation.process.earlierFailure"
                : "conversation.tool.state.interrupted"
            )}
          />
        ) : undefined
      }
      tone={isModelErrors ? "destructive" : undefined}
      open={open}
      onOpenChange={onOpenChange}
    />
  )
}

// 布局变化后将隐藏的 Item 映射至所属摘要，校正首个可见记录的阅读位置。
function keepAnchorInPlace(
  anchorRef: { current: ScrollAnchor | undefined },
  scroller: HTMLElement,
  content: HTMLElement,
  isFollowing: boolean
): void {
  // 1. 贴底跟随交给滚动库，非贴底时先检查原锚点及其折叠摘要。
  if (isFollowing) {
    anchorRef.current = undefined
    return
  }
  const scrollerTop = scroller.getBoundingClientRect().top
  const anchor = anchorRef.current
  const hasScrolled = !!anchor && scroller.scrollTop !== anchor.knownScrollTop
  let preservedElement: HTMLElement | undefined
  if (anchor) {
    anchor.viewportTop -= scroller.scrollTop - anchor.knownScrollTop
    const element = [anchor.element, ...anchor.fallbacks].find(
      (element) =>
        content.contains(element) &&
        !element.closest('[data-slot="collapsible-content"][data-closed]') &&
        element.getClientRects().length > 0
    )
    if (element) {
      const viewportTop = element === anchor.element ? anchor.viewportTop : 0
      const drift =
        element.getBoundingClientRect().top - scrollerTop - viewportTop
      if (Math.abs(drift) >= 1) scroller.scrollTop += drift
      const rect = element.getBoundingClientRect()
      if (
        rect.bottom > scrollerTop &&
        rect.top < scrollerTop + scroller.clientHeight &&
        (!hasScrolled || rect.top <= scrollerTop)
      )
        preservedElement = element
    }
  }
  // 2. 锚点离开视口时重新查找，跳过已滚过的整轮和关闭的详情子树。
  let element = preservedElement
  if (!element) {
    const walker = document.createTreeWalker(content, NodeFilter.SHOW_ELEMENT, {
      acceptNode(node) {
        if (!(node instanceof HTMLElement)) return NodeFilter.FILTER_REJECT
        if (
          node.hidden ||
          node.matches('[data-slot="collapsible-content"][data-closed]') ||
          node.getBoundingClientRect().bottom <= scrollerTop
        )
          return NodeFilter.FILTER_REJECT
        return node.dataset.anchorKey
          ? NodeFilter.FILTER_ACCEPT
          : NodeFilter.FILTER_SKIP
      },
    })
    element = (walker.nextNode() as HTMLElement | null) ?? undefined
  }
  if (!element) {
    anchorRef.current = undefined
    return
  }
  // 3. 保存锚点 DOM、阅读位置和所属折叠摘要。
  const fallbacks: HTMLElement[] = []
  let parent: HTMLElement | null = element
  while (parent && parent !== content) {
    if (parent.dataset.foldAnchor) {
      const fallback = content.querySelector<HTMLElement>(
        `[data-anchor-key="${CSS.escape(parent.dataset.foldAnchor)}"]`
      )
      if (fallback) fallbacks.push(fallback)
    }
    parent = parent.parentElement
  }
  anchorRef.current = {
    element,
    viewportTop: element.getBoundingClientRect().top - scrollerTop,
    knownScrollTop: scroller.scrollTop,
    fallbacks,
  }
}
