import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react"
import { ArrowDownIcon } from "lucide-react"
import { motion } from "motion/react"
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
import { KeptViewStateContext } from "@/components/base/kept-view-state"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { getMountedRowIndexes } from "@/lib/mounted-message-rows"
import { cn } from "@/lib/utils"

// 离底部这么近就算贴底，和滚动库的判断一致（px）
const nearBottomDistance = 70

// 视口上下各最多多画几行；同时只取离视口边缘一屏以内的
const rowOverscan = 4
// 最后几行一直挂载：支持流式更新和贴底跟随
const pinnedRecentRowCount = 3
// 一行都没量过时按这个高度估：约是一条带中等长度回复的消息；量过之后按已量行的平均高度估
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

interface VirtualMessageListProps<Row> {
  rows: readonly Row[]
  getRowKey(row: Row): string
  renderRow(row: Row, index: number): ReactNode
  header?: ReactNode
  footer?: ReactNode
  ariaLabel: string
  earlier?: {
    hasMore: boolean
    canLoad: boolean
    load(): void
    content?(retry: () => void): ReactNode
  }
  resetKey: unknown
  scrollToBottomKey?: string
  alignBottom?: boolean
}

// 虚拟渲染消息行，管理贴底跟随、向前分页和阅读锚点。
export function VirtualMessageList<Row>({
  rows,
  getRowKey,
  renderRow,
  header,
  footer,
  ariaLabel,
  earlier,
  resetKey,
  scrollToBottomKey,
  alignBottom = false,
}: VirtualMessageListProps<Row>) {
  const { t } = useTranslation()
  const { hasMore, canLoad, load, content: earlierContent } = earlier ?? {}
  const [viewState, setViewState] = useState(() => ({
    resetKey,
    rowGeneration: 0,
    keptViewStates: new Map<string, unknown>(),
  }))
  if (!Object.is(viewState.resetKey, resetKey))
    setViewState({
      resetKey,
      rowGeneration: viewState.rowGeneration + 1,
      keptViewStates: new Map(),
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

  // 虚拟列表：只挂载视口附近、最近几行和锚点所在的行；滚动位置只由贴底跟随和锚点校正改动。
  const rowListRef = useRef<HTMLDivElement>(null)
  // 行列表顶部离滚动区顶部的距离：内边距 + 哨兵 + header
  const [rowListTop, setRowListTop] = useState(0)

  const rowIndexByKey = useMemo(
    () => new Map(rows.map((row, index) => [getRowKey(row), index])),
    [rows, getRowKey]
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
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    getItemKey: (index) => getRowKey(rows[index]),
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
    if (scroller?.clientHeight && content)
      keepAnchorInPlace(
        anchorRef,
        scroller,
        content,
        stickState.isAtBottom,
        virtualizer.measurementsCache
      )
  }, [scrollRef, contentRef, stickState, virtualizer])

  const loadEarlier = useCallback(() => {
    stopScroll()
    updateAnchor()
    load?.()
  }, [stopScroll, updateAnchor, load])

  // 2. 顶部哨兵进入视口时加载：每次滚动和依赖变化时直接比较哨兵和滚动区的位置，不依赖 IntersectionObserver 报告的进出变化。
  // 首屏哨兵要先离开过视口才按滚动触发，短内容由用户向上翻页的手势触发。
  const sentinelRef = useRef<HTMLDivElement>(null)
  const hasLeftTopRef = useRef(false)
  useEffect(() => {
    const sentinel = sentinelRef.current
    const scroller = scrollRef.current
    if (!sentinel || !scroller || !hasMore || !canLoad) return
    const checkSentinel = () => {
      if (scroller.clientHeight === 0) return
      const isSentinelVisible =
        sentinel.getBoundingClientRect().bottom >
        scroller.getBoundingClientRect().top
      if (!isSentinelVisible) {
        hasLeftTopRef.current = true
        return
      }
      if (hasLeftTopRef.current) {
        hasLeftTopRef.current = false
        loadEarlier()
      }
    }
    // 内容不足一屏时，向上滚轮或下拉手势每次只加载一页。
    const loadFromGesture = () => {
      if (scroller.scrollTop <= 0) loadEarlier()
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
  }, [scrollRef, hasMore, canLoad, loadEarlier])

  // 切换阅读目标时只重置列表状态，输入区保留同一 DOM。
  useLayoutEffect(() => {
    anchorRef.current = undefined
    visiblePositionRef.current = undefined
    hasLeftTopRef.current = false
    void scrollToBottom({ animation: "instant" })
  }, [resetKey, scrollToBottom])

  const scrollRequestRef = useRef<{ key?: string; handled: boolean }>({
    handled: false,
  })
  useLayoutEffect(() => {
    if (scrollRequestRef.current.key !== scrollToBottomKey)
      scrollRequestRef.current = { key: scrollToBottomKey, handled: false }
    if (
      scrollToBottomKey !== undefined &&
      !scrollRequestRef.current.handled &&
      rowIndexByKey.has(scrollToBottomKey)
    ) {
      scrollRequestRef.current.handled = true
      visiblePositionRef.current = undefined
      void scrollToBottom({ animation: "instant" })
    }
  }, [scrollToBottomKey, rowIndexByKey, scrollToBottom])

  useLayoutEffect(() => {
    updateAnchor()
  })
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
          className={cn(
            "mx-auto w-full max-w-3xl min-w-0 px-3 py-4 md:px-6",
            alignBottom && "flex min-h-full flex-col"
          )}
        >
          <section
            ref={messagesRef}
            tabIndex={0}
            aria-label={ariaLabel}
            className={cn("flex min-w-0 flex-col gap-4", alignBottom && "grow")}
          >
            {hasMore && (
              <div
                ref={sentinelRef}
                className="flex min-h-8 flex-col items-center justify-center"
              >
                {earlierContent?.(loadEarlier)}
              </div>
            )}
            {header}
            {rows.length > 0 && (
              <KeptViewStateContext
                key={viewState.rowGeneration}
                value={viewState.keptViewStates}
              >
                <div
                  ref={rowListRef}
                  className={cn("relative w-full", alignBottom && "mt-auto")}
                  style={{ height: virtualizer.getTotalSize() }}
                >
                  {virtualRows.map((virtualRow) => {
                    const row = rows[virtualRow.index]
                    return (
                      <div
                        key={virtualRow.key}
                        ref={virtualizer.measureElement}
                        data-index={virtualRow.index}
                        data-row-key={virtualRow.key}
                        className={cn(
                          "absolute top-0 left-0 w-full",
                          virtualRow.index < rows.length - 1 && "pb-4"
                        )}
                        style={{
                          transform: `translateY(${virtualRow.start - rowListTop}px)`,
                        }}
                      >
                        {renderRow(row, virtualRow.index)}
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
                    aria-label={t("common.actions.scrollToBottom")}
                    className="shadow-sm"
                    onClick={scrollBackToBottom}
                  />
                }
              >
                <ArrowDownIcon />
              </TooltipTrigger>
              <TooltipContent className="text-caption">
                {t("common.actions.scrollToBottom")}
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
  // 2. 锚点离开视口时重新查找视口里第一条露出来的元素，跳过已滚过的整行和关闭的详情子树。
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
