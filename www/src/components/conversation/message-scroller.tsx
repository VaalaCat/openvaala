import { CircleAlert, ListTree } from "lucide"
import { LayoutGroup, motion } from "motion/react"
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
import {
  measureElement,
  observeElementOffset,
  observeElementRect,
  useVirtualizer,
  type Rect,
  type Virtualizer,
  type VirtualItem,
} from "@tanstack/react-virtual"
import { useTranslation } from "react-i18next"
import { useStickToBottom } from "use-stick-to-bottom"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { KeptViewStateContext } from "@/components/base/kept-view-state"
import { RequestError } from "@/components/base/request-error"
import { ImagePreviewStrip } from "@/components/base/image-preview"
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
import type { GetTurnsResult, Turn, TurnID } from "@/lib/api/protocol.gen"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import {
  ItemDisclosureContext,
  getProcessFacts,
  getProcessActionText,
  getTurnResult,
  getReplyKeysToCopy,
  getMessageSource,
  getMessageRowKey,
  type ItemRow,
  type MessageRow,
  type ItemDebugTarget,
  type TurnContentRow,
} from "@/lib/item-renderers"
import { getMountedRowIndexes } from "@/lib/mounted-message-rows"
import { turnsOptions } from "@/lib/query/session"
import { getSessionName } from "@/lib/session-tree"
import { getStringField, parseToolArguments } from "@/lib/tool-arguments"
import { shouldShowLoadError } from "@/lib/rpc-error"
import { cn } from "@/lib/utils"

// 离底部这么近就算贴底，和滚动库的判断一致（px）
const nearBottomDistance = 70

// 视口上下各最多多画几行；同时只取离视口边缘一屏以内的
const rowOverscan = 4
// 最后几行一直挂载：正在流式输出的那一轮和它前面两轮
const pinnedRecentRowCount = 3
// 一行都没量过时按这个高度估：约是一轮带一段中等长度回复的高度；量过之后按已量行的平均高度估
const estimatedRowHeight = 600

// 消息区的滚动锚点：视口里第一条露出来的 Item，和滚动位置是 knownScrollTop 时它离滚动区顶部的距离
interface ScrollAnchor {
  element: HTMLElement
  viewportTop: number
  knownScrollTop: number
  fallbacks: HTMLElement[]
  // 锚点所在行的 key（行包装上的 data-row-key）
  rowKey: string
  // 记录锚点时，这一行在虚拟列表坐标里的 start
  rowStart: number
}

// 消息区：拥有消息列的滚动。贴底跟随交给 use-stick-to-bottom，用户往上翻就停止跟随并出现「回到底部」；
// 滚到顶部时加载更早的 Item。滚动锚定自己做（同浏览器的 overflow-anchor）：锚点总是视口里第一条露出来的 Item，
// 插入更早的 Item、图片撑开、Turn 记录读到后行高变化让它挪了位置，就补回 scrollTop，正在看的位置不跳。每一行各有错误边界；
// 消息行由虚拟列表挂载，只挂视口附近、最近几轮和锚点所在的行。
export function MessageScroller({
  sync,
  isReady,
  loadingContent,
  isSubSession,
  canLoadEarlier = true,
  header,
  rows,
  footer,
  onInspect,
}: {
  sync: ConversationSync
  isReady: boolean
  loadingContent: ReactNode
  isSubSession: boolean
  // 是否允许向前加载更早的 Item？旁支只显示分叉之后的内容，开头已经显示出来后不再往前翻
  canLoadEarlier?: boolean
  // 消息列的第一行：和 Item 同宽同边距，排在所有行之前
  header?: ReactNode
  rows: MessageRow[]
  footer: ReactNode
  onInspect(target: ItemDebugTarget): void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const items = useStore(sync.$items)
  const replyKeysToCopy = useMemo(() => getReplyKeysToCopy(items), [items])
  const hasEarlierItems = useStore(sync.$hasEarlierItems) && canLoadEarlier
  const turnViews = useStore(sync.$turns)
  useLayoutEffect(() => {
    if (isReady) sync.markItemsPresented(items.filter(hasItemPresentation))
  }, [sync, items, isReady])
  // 1. 消息区按 Turn 分开保存展开选择，阅读与调试使用父层提供的同一批行。
  const [choicesByTurn, setChoicesByTurn] = useState<
    Record<string, TimelineChoiceValues | undefined>
  >({})
  const chooseTurnOpen = useCallback(
    (turnID: string, key: string | string[], open: boolean) => {
      setChoicesByTurn((previous) => ({
        ...previous,
        [turnID]: {
          ...previous[turnID],
          ...Object.fromEntries(
            (Array.isArray(key) ? key : [key]).map((entry) => [entry, open])
          ),
        },
      }))
    },
    []
  )
  const chooseOutsideTurnOpen = useCallback(
    (key: string | string[], open: boolean) =>
      chooseTurnOpen(outsideTurnChoicesKey, key, open),
    [chooseTurnOpen]
  )
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
    select: getTurnsByID,
  })
  const {
    scrollRef,
    contentRef,
    isAtBottom,
    scrollToBottom,
    stopScroll,
    state: stickState,
  } = useStickToBottom({ initial: "instant", resize: "smooth" })
  const messagesRef = useRef<HTMLElement>(null)
  // 最近一次可见时的滚动位置：隐藏（display:none）时浏览器丢掉位置、尺寸变成 0，重新显示时放回去
  const visiblePositionRef = useRef<
    { scrollTop: number; isNearBottom: boolean } | undefined
  >(undefined)
  const anchorRef = useRef<ScrollAnchor | undefined>(undefined)
  const anchorFrameRef = useRef<number | undefined>(undefined)

  // 虚拟列表：只挂载视口附近、最近几轮和锚点所在的行；滚动位置只由贴底跟随和锚点校正改动。
  const rowListRef = useRef<HTMLDivElement>(null)
  // 行列表顶部离滚动区顶部的距离：内边距 + 哨兵 + header
  const [rowListTop, setRowListTop] = useState(0)
  // 进列表的行：去掉没有可显示内容的轮外 Item 行（TimelineItem 对它们返回 null）
  const listRows = useMemo(
    () =>
      rows.filter(
        (row) => row.kind === "turn" || hasItemPresentation(row.view)
      ),
    [rows]
  )
  const rowIndexByKey = useMemo(
    () => new Map(listRows.map((row, index) => [getMessageRowKey(row), index])),
    [listRows]
  )
  const virtualizerRef =
    useRef<Virtualizer<HTMLElement, HTMLDivElement>>(undefined)
  // 没量过的行按已量行的平均高度估
  const averageRowHeightRef = useRef(estimatedRowHeight)
  const rangeExtractor = useCallback((): number[] => {
    // 1. 贴底跟随时，视口按列表底部算；隐藏时滚动库会把尺寸归零当成贴底，这时沿用隐藏前的滚动位置。
    const instance = virtualizerRef.current!
    const rowBounds = instance.measurementsCache
    const viewHeight = instance.scrollRect?.height ?? 0
    const scrollOffset = instance.scrollOffset ?? 0
    const isFollowing = instance.scrollElement?.clientHeight !== 0 && isAtBottom
    const anchor = isFollowing ? undefined : anchorRef.current
    const anchorRowIndex = anchor && rowIndexByKey.get(anchor.rowKey)
    let viewTop = scrollOffset
    if (isFollowing)
      viewTop = Math.max(
        instance.options.scrollMargin,
        rowBounds[rowBounds.length - 1].end - viewHeight
      )
    // 2. 阅读中，锚点那一行如果因为前面插入行而下移，按下移后的位置算视口。
    else if (anchor && anchorRowIndex !== undefined)
      viewTop = scrollOffset + rowBounds[anchorRowIndex].start - anchor.rowStart
    // 3. 交给挂载规则，阅读中锚点所在行一起挂载。
    return getMountedRowIndexes({
      rowBounds,
      viewTop,
      viewHeight,
      overscan: rowOverscan,
      pinnedRecentRowCount,
      anchorRowIndex,
    })
  }, [rowIndexByKey, isAtBottom])
  // 项目没有启用 React Compiler；TanStack Virtual 返回的实例会变，Compiler 跳过这个组件
  // eslint-disable-next-line react-hooks/incompatible-library
  const virtualizer = useVirtualizer({
    count: listRows.length,
    getScrollElement: () => scrollRef.current,
    getItemKey: (index) => getMessageRowKey(listRows[index]),
    estimateSize: () => averageRowHeightRef.current,
    overscan: rowOverscan,
    scrollMargin: rowListTop,
    rangeExtractor,
    observeElementRect: observeVisibleElementRect,
    observeElementOffset: observeVisibleElementOffset,
    measureElement: measureVisibleRow,
  })
  virtualizerRef.current = virtualizer
  averageRowHeightRef.current = getAverageRowHeight(virtualizer.itemSizeCache)
  // 滚动补偿只由 keepAnchorInPlace 和 use-stick-to-bottom 做
  virtualizer.shouldAdjustScrollPositionOnItemSizeChange = () => false
  const virtualRows = virtualizer.getVirtualItems()
  // 行列表离滚动区顶部的距离变了才更新；滚动区被隐藏时不更新
  const updateRowListTop = useCallback(() => {
    if (!rowListRef.current || scrollRef.current?.clientHeight === 0) return
    setRowListTop(rowListRef.current.offsetTop)
  }, [scrollRef])
  useLayoutEffect(() => {
    updateRowListTop()
  })

  const updateAnchor = useCallback(() => {
    // 1. 提交和尺寸变化立即校正，并合并尚未执行的滚动测量。
    if (anchorFrameRef.current !== undefined) {
      cancelAnimationFrame(anchorFrameRef.current)
      anchorFrameRef.current = undefined
    }
    const scroller = scrollRef.current
    const content = contentRef.current
    if (scroller && content)
      keepAnchorInPlace(
        anchorRef,
        scroller,
        content,
        stickState.isAtBottom,
        virtualizer.measurementsCache
      )
  }, [scrollRef, contentRef, stickState, virtualizer])

  // 1. 加载更早的 Item，失败时停下等用户重试。
  const [isLoadingEarlier, setIsLoadingEarlier] = useState(false)
  const [earlierItemsError, setEarlierItemsError] = useState<unknown>()
  const syncRef = useRef(sync)
  const [readingTarget, setReadingTarget] = useState(sync)
  const [keptViewStates, setKeptViewStates] = useState(
    () => new Map<string, unknown>()
  )
  if (readingTarget !== sync) {
    setReadingTarget(sync)
    setKeptViewStates(new Map())
    setChoicesByTurn({})
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
    visiblePositionRef.current = undefined
    hasLeftTopRef.current = false
    if (scrollRef.current) scrollRef.current.scrollTop = 0
  }, [sync, scrollRef])
  useLayoutEffect(() => {
    updateAnchor()
  }, [
    rows,
    choicesByTurn,
    turns.data,
    hasEarlierItems,
    updateAnchor,
    virtualRows,
  ])
  useEffect(() => {
    const scroller = scrollRef.current
    const content = contentRef.current
    if (!scroller || !content) return
    // 隐藏时（尺寸 0）不记位置，也不让隐藏造成的尺寸变化改动滚动状态；重新显示时再放回去
    let isHidden = scroller.clientHeight === 0
    const handleScroll = () => {
      if (!isHidden && scroller.clientHeight > 0)
        visiblePositionRef.current = {
          scrollTop: scroller.scrollTop,
          isNearBottom:
            stickState.targetScrollTop - scroller.scrollTop <=
            nearBottomDistance,
        }
      anchorFrameRef.current ??= requestAnimationFrame(updateAnchor)
    }
    // 隐藏前贴着底部的回到底部继续跟随新输出；在中间阅读的放回原来的位置，不再跟随
    const restoreVisiblePosition = () => {
      const position = visiblePositionRef.current
      if (!position) return
      if (position.isNearBottom) {
        stickState.scrollTop = stickState.targetScrollTop
      } else {
        stopScroll()
        stickState.scrollTop = position.scrollTop
      }
      updateAnchor()
    }
    const observer = new ResizeObserver(() => {
      updateRowListTop()
      updateAnchor()
    })
    observer.observe(content)
    // 窗口高度或手机键盘改变消息区高度时，贴底阅读继续停在最底部。
    let viewportHeight = scroller.clientHeight
    const viewportObserver = new ResizeObserver(() => {
      // 1. 隐藏时只记下隐藏了；滚动库会把尺寸归零当成「贴底」，重新显示时按隐藏前的位置恢复。
      if (scroller.clientHeight === 0) {
        isHidden = true
        viewportHeight = 0
        return
      }
      if (isHidden) {
        isHidden = false
        viewportHeight = scroller.clientHeight
        restoreVisiblePosition()
        return
      }
      // 2. 和内容尺寸变化一样记下高度差，浏览器随之收回滚动位置不算用户上滚。
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
      // 3. 贴底时本帧补上被遮住的距离。
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
  }, [
    scrollRef,
    contentRef,
    updateRowListTop,
    updateAnchor,
    stopScroll,
    stickState,
  ])

  // 回到底部：离底部超过两屏时先瞬间跳到底部往上一屏，再平滑滚完最后一屏
  const scrollBackToBottom = () => {
    // 1. 键盘焦点留在消息区。
    messagesRef.current?.focus({ preventScroll: true })
    // 2. 离底部远时经滚动库跳到底部往上一屏，平滑滚动只经过最后一屏。
    const scroller = scrollRef.current
    if (
      scroller &&
      stickState.targetScrollTop - scroller.scrollTop >
        2 * scroller.clientHeight
    )
      stickState.scrollTop = stickState.targetScrollTop - scroller.clientHeight
    // 3. 平滑滚到底部，恢复贴底跟随。
    void scrollToBottom({ animation: "smooth" })
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div
        ref={scrollRef}
        data-slot="message-scroller"
        // 滚动锚定由第 3 步做，关掉浏览器自己的，避免补两次；relative 让 sr-only 这类绝对定位的文字随消息滚动、在这里裁剪；
        // 滚到头时不把滚动交给外层，回弹只在消息区里
        className="relative min-h-0 flex-1 [scrollbar-gutter:stable] overflow-y-auto overscroll-y-contain [overflow-anchor:none]"
      >
        {/* 消息列：输入区在滚动容器外，两边各自按同样的滚动条槽和列宽对齐 */}
        <div
          ref={contentRef}
          className="mx-auto w-full max-w-3xl min-w-0 px-3 py-4 md:px-6"
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
            {isReady && header}
            {isReady && rows.length === 0 && (
              <p className="py-8 text-center text-body color-muted">
                {t("conversation.empty")}
              </p>
            )}
            {isReady && listRows.length > 0 && (
              <KeptViewStateContext value={keptViewStates}>
                <div
                  ref={rowListRef}
                  className="relative w-full"
                  style={{ height: virtualizer.getTotalSize() }}
                >
                  {virtualRows.map((virtualRow) => {
                    const row = listRows[virtualRow.index]
                    return (
                      <div
                        key={virtualRow.key}
                        ref={virtualizer.measureElement}
                        data-index={virtualRow.index}
                        data-row-key={virtualRow.key}
                        className={cn(
                          "absolute top-0 left-0 w-full",
                          virtualRow.index < listRows.length - 1 && "pb-4"
                        )}
                        style={{
                          transform: `translateY(${virtualRow.start - rowListTop}px)`,
                        }}
                      >
                        {row.kind === "turn" ? (
                          <TurnTimeline
                            turnID={row.turnID}
                            rows={row.rows}
                            items={row.items}
                            replyKeyToCopy={replyKeysToCopy.get(row.turnID)}
                            turn={turns.data?.[row.turnID]}
                            turnView={turnViews[row.turnID]}
                            isLoadingTurn={
                              turns.isFetching && !turns.data?.[row.turnID]
                            }
                            savedDurationMs={turnSavedDurations.get(row.turnID)}
                            sync={sync}
                            isSubSession={isSubSession}
                            choices={
                              choicesByTurn[row.turnID] ?? noTimelineChoices
                            }
                            chooseTurnOpen={chooseTurnOpen}
                            onInspect={onInspect}
                          />
                        ) : (
                          <TimelineItem
                            row={row}
                            copyReply={
                              row.view.key ===
                              replyKeysToCopy.get(row.view.turnID)
                            }
                            sync={sync}
                            isSubSession={isSubSession}
                            choices={
                              choicesByTurn[outsideTurnChoicesKey] ??
                              noTimelineChoices
                            }
                            chooseOpen={chooseOutsideTurnOpen}
                            onInspect={onInspect}
                          />
                        )}
                      </div>
                    )
                  })}
                </div>
              </KeptViewStateContext>
            )}
          </section>
        </div>
      </div>
      {/* 输入区在滚动容器外面，消息区回弹时它不动；消息列和这里各自保留滚动条槽，同列宽对齐。
          槽用独立占位元素保留，输入区外面不设 overflow，斜杠补全列表和回到底部按钮才不会被裁掉 */}
      <div className="flex shrink-0 bg-background">
        <div className="relative z-10 min-w-0 flex-1">
          <motion.div
            initial={false}
            animate={{
              opacity: isAtBottom ? 0 : 1,
              y: isAtBottom ? 4 : 0,
            }}
            transition={{
              duration: 0.14,
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
                    onClick={scrollBackToBottom}
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
          <div className="mx-auto w-full max-w-3xl px-3 pb-3 md:px-6">
            {footer}
          </div>
        </div>
        <div
          aria-hidden
          className="[scrollbar-gutter:stable] overflow-y-hidden"
        />
      </div>
    </div>
  )
}

// 一组展开选择：key 是 "group:" 或 "item:" 开头的行标识，没选过时为 undefined
type TimelineChoiceValues = Record<string, boolean | undefined>

// 不属于任何 Turn 的 Item 的展开选择，保存在 choicesByTurn 的这个 key 下
const outsideTurnChoicesKey = ""

// 还没有展开选择的 Turn 共用的空选择
const noTimelineChoices: TimelineChoiceValues = {}

interface TimelineChoices {
  choices: TimelineChoiceValues
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
  chooseTurnOpen,
  onInspect,
}: Omit<TimelineChoices, "chooseOpen"> & {
  chooseTurnOpen: (
    turnID: string,
    key: string | string[],
    open: boolean
  ) => void
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
  // 1. 本轮的展开选择只写入本轮。
  const chooseOpen = useCallback(
    (key: string | string[], open: boolean) =>
      chooseTurnOpen(turnID, key, open),
    [chooseTurnOpen, turnID]
  )
  // 2. 完整成功轮次确认最终回复，其他模型回复各自显示中途回复入口。
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
  // 3. 相邻分组只保留一个标题，Hook 与消息按原顺序独立显示。
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
          copyReply={!isRunning && row.view.key === replyKeyToCopy}
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
    const images = groupRows.flatMap((member) => {
      const result =
        member.toolResult?.item.tool_result ?? member.view.item.tool_result
      return (result?.images ?? []).flatMap((image, index) =>
        image
          ? [
              {
                key: `${member.view.key}:${index}`,
                src: image.data_url,
                alt: image.alt,
              },
            ]
          : []
      )
    })
    return (
      <Collapsible
        key={key}
        render={
          <motion.div
            initial={isEnteringGroup ? { opacity: 0 } : false}
            animate={{ opacity: 1 }}
            transition={{ duration: 0.16, ease: "easeOut" }}
          />
        }
        open={isSingle || groupOpen}
        onOpenChange={(open) => chooseOpen("group:" + key, open)}
        className="flex min-w-0 flex-col gap-2"
      >
        <div
          data-anchor-key={"group:" + key}
          hidden={isSingle}
          className="flex min-w-0 flex-col gap-2"
        >
          <SummaryButton
            sync={sync}
            open={groupOpen}
            onOpenChange={(open) => chooseOpen("group:" + key, open)}
            rows={groupRows}
            isTurnRunning={isRunning}
            isTailGroup={row === rows.at(-1)}
            isModelErrors={row.groupKind === "model_errors"}
          />
          {!groupOpen && <ImagePreviewStrip images={images} />}
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
    <div
      data-turn-id={turnID}
      className="flex min-w-0 flex-col gap-4 [contain:layout_style]"
    >
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
        !suppressEntrance && sync.isUnpresentedLiveItem(itemKey)
          ? { opacity: 0 }
          : false
      }
      animate={{ opacity: 1 }}
      transition={{ duration: 0.16, ease: "easeOut" }}
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
              layout="position"
              className="min-w-0 empty:hidden"
              transition={{ duration: 0.16 }}
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

// 按 Turn ID 索引一批 Turn 记录；批次变化时，查询的结构共享让内容没变的 Turn 沿用原对象。
function getTurnsByID(result: GetTurnsResult): Partial<Record<TurnID, Turn>> {
  // 1. 用 Turn ID 作为 key 生成普通对象。
  return Object.fromEntries(result.turns.map((turn) => [turn.id, turn]))
}

// 滚动区可见时才把视口尺寸交给虚拟列表；隐藏时保持隐藏前的尺寸
function observeVisibleElementRect(
  instance: Virtualizer<HTMLElement, HTMLDivElement>,
  onRect: (rect: Rect) => void
) {
  // 1. 被 display:none 隐藏时尺寸是 0，不往下报。
  return observeElementRect(instance, (rect) => {
    if (instance.scrollElement?.clientHeight !== 0) onRect(rect)
  })
}

// 滚动区可见时才把滚动位置交给虚拟列表；隐藏时保持隐藏前的位置
function observeVisibleElementOffset(
  instance: Virtualizer<HTMLElement, HTMLDivElement>,
  onOffset: (offset: number, isScrolling: boolean) => void
) {
  // 1. 被 display:none 隐藏时浏览器丢掉滚动位置，不往下报。
  return observeElementOffset(instance, (offset, isScrolling) => {
    if (instance.scrollElement?.clientHeight !== 0)
      onOffset(offset, isScrolling)
  })
}

// 已量行的平均高度；一行都没量过时用估计高度
function getAverageRowHeight(rowHeights: ReadonlyMap<unknown, number>): number {
  if (rowHeights.size === 0) return estimatedRowHeight
  let totalHeight = 0
  for (const rowHeight of rowHeights.values()) totalHeight += rowHeight
  return totalHeight / rowHeights.size
}

// 隐藏时沿用这一行已量的高度，不记成 0
function measureVisibleRow(
  element: HTMLDivElement,
  entry: ResizeObserverEntry | undefined,
  instance: Virtualizer<HTMLElement, HTMLDivElement>
): number {
  // 1. 可见时照常测量；隐藏时读这一行缓存的高度，没量过就按估计高度。
  if (instance.scrollElement?.clientHeight !== 0)
    return measureElement(element, entry, instance)
  const key = instance.options.getItemKey(instance.indexFromElement(element))
  return instance.itemSizeCache.get(key) ?? estimatedRowHeight
}

// 布局变化后将隐藏的 Item 映射至所属摘要，校正首个可见记录的阅读位置。
function keepAnchorInPlace(
  anchorRef: { current: ScrollAnchor | undefined },
  scroller: HTMLElement,
  content: HTMLElement,
  isFollowing: boolean,
  rowBounds: readonly VirtualItem[]
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
  // 2. 锚点离开视口时重新查找视口里第一条露出来的元素，跳过已滚过的整轮和关闭的详情子树。
  let element = preservedElement
  if (!element) {
    const scrollerBottom = scrollerTop + scroller.clientHeight
    const walker = document.createTreeWalker(content, NodeFilter.SHOW_ELEMENT, {
      acceptNode(node) {
        if (!(node instanceof HTMLElement)) return NodeFilter.FILTER_REJECT
        const rect = node.getBoundingClientRect()
        if (
          node.hidden ||
          node.matches('[data-slot="collapsible-content"][data-closed]') ||
          rect.bottom <= scrollerTop ||
          rect.top >= scrollerBottom
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
  // 3. 保存锚点 DOM、阅读位置、所在行和所属折叠摘要。
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
  const row = element.closest<HTMLElement>("[data-row-key]")!
  anchorRef.current = {
    element,
    viewportTop: element.getBoundingClientRect().top - scrollerTop,
    knownScrollTop: scroller.scrollTop,
    fallbacks,
    rowKey: row.dataset.rowKey!,
    rowStart: rowBounds[Number(row.dataset.index)].start,
  }
}
