import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import {
  memo,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
} from "react"
import {
  ArrowDownIcon,
  ArrowUpIcon,
  DownloadIcon,
  EraserIcon,
  PauseIcon,
  PlayIcon,
  XIcon,
} from "lucide-react"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import { ListEmpty } from "@/components/base/list-empty"
import { ListSearch } from "@/components/base/list-search"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import type { ConversationID } from "@/lib/api/protocol.gen"
import type { RpcFrame } from "@/lib/api/server-connection"
import {
  filterRpcRows,
  formatRpcLogJsonl,
  maskRpcFrame,
  pairRpcFrames,
  type RpcLogRow,
} from "@/lib/rpc-log"
import { cn, downloadBlob, formatByteSize } from "@/lib/utils"

type DirectionFilter = "all" | RpcFrame["direction"]

// 列表每行的高度；行高固定，只渲染滚动位置附近的行
const rowHeight = 24
// 可见范围上下多渲染的行数，快速滚动时不露白
const overscanRows = 12

// RPC 记录页签：连接最近 2000 帧收发记录，每帧一行（方向、时间、方法、编号、大小、响应耗时），
// 可按方法名、方向和「只看当前对话」筛选；工具栏暂停记录、清空、下载全部帧（JSONL）；点一行在下方看整帧
export function RpcLogPanel({
  conversationID,
}: {
  conversationID: ConversationID
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const frames = useStore(connection.$rpcLog)
  const isPaused = useStore(connection.$isRpcLogPaused)
  const [method, setMethod] = useState("")
  const [direction, setDirection] = useState<DirectionFilter>("all")
  const [isCurrentConversationOnly, setIsCurrentConversationOnly] =
    useState(true)
  const [selectedFrame, setSelectedFrame] = useState<RpcFrame>()
  const rows = useMemo(() => pairRpcFrames(frames), [frames])
  const visibleRows = useMemo(
    () =>
      filterRpcRows(rows, {
        method,
        direction: direction === "all" ? undefined : direction,
        conversationID: isCurrentConversationOnly ? conversationID : undefined,
      }),
    [rows, method, direction, isCurrentConversationOnly, conversationID]
  )
  // 选中的帧被清空或挤出记录后不再显示
  const selectedRow = selectedFrame
    ? rows.find((row) => row.frame === selectedFrame)
    : undefined
  const isFiltered =
    method.trim() !== "" || direction !== "all" || isCurrentConversationOnly

  function handleSearch(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setMethod(String(new FormData(event.currentTarget).get("keyword") ?? ""))
  }

  function clearFilters() {
    setMethod("")
    setDirection("all")
    setIsCurrentConversationOnly(false)
  }

  function downloadFrames() {
    const blob = new Blob([formatRpcLogJsonl(frames)], {
      type: "application/x-ndjson;charset=utf-8",
    })
    downloadBlob(
      blob,
      "openvaala-rpc-" +
        new Date().toISOString().replace(/[:.]/g, "-") +
        ".jsonl"
    )
  }

  return (
    <section
      className="@container flex h-full min-h-0 min-w-0 flex-col p-3"
      aria-label={t("rpcLog.title")}
    >
      <div className="flex shrink-0 flex-col gap-2 border-b pb-3">
        <div className="flex min-w-0 items-center gap-2">
          <form className="min-w-0 flex-1" onSubmit={handleSearch}>
            <ListSearch
              keyword={method}
              onKeywordChange={setMethod}
              onClear={() => setMethod("")}
              placeholder={t("rpcLog.searchPlaceholder")}
              aria-label={t("rpcLog.searchPlaceholder")}
              autoComplete="off"
              spellCheck={false}
            />
          </form>
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  aria-pressed={isPaused}
                  className="aria-pressed:bg-muted"
                  aria-label={t(isPaused ? "rpcLog.resume" : "rpcLog.pause")}
                  onClick={() => connection.setRpcLogPaused(!isPaused)}
                />
              }
            >
              {isPaused ? <PlayIcon /> : <PauseIcon />}
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t(isPaused ? "rpcLog.resume" : "rpcLog.pause")}
            </TooltipContent>
          </Tooltip>
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  disabled={frames.length === 0}
                  aria-label={t("rpcLog.clear")}
                  onClick={() => {
                    connection.clearRpcLog()
                    setSelectedFrame(undefined)
                  }}
                  focusableWhenDisabled
                />
              }
            >
              <EraserIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("rpcLog.clear")}
            </TooltipContent>
          </Tooltip>
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  disabled={frames.length === 0}
                  aria-label={t("rpcLog.download")}
                  onClick={downloadFrames}
                  focusableWhenDisabled
                />
              }
            >
              <DownloadIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("rpcLog.download")}
            </TooltipContent>
          </Tooltip>
        </div>
        <div className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-2">
          <ToggleGroup
            variant="outline"
            spacing={0}
            aria-label={t("rpcLog.direction.label")}
            value={[direction]}
            onValueChange={(values: string[]) => {
              const next = values[0] as DirectionFilter | undefined
              if (next) setDirection(next)
            }}
          >
            <ToggleGroupItem value="all" className="text-ui">
              {t("rpcLog.direction.all")}
            </ToggleGroupItem>
            <ToggleGroupItem value="sent" className="text-ui">
              <RpcDirection direction="sent" />
              {t("rpcLog.direction.sent")}
            </ToggleGroupItem>
            <ToggleGroupItem value="received" className="text-ui">
              <RpcDirection direction="received" />
              {t("rpcLog.direction.received")}
            </ToggleGroupItem>
          </ToggleGroup>
          <Label className="flex items-center gap-2 text-ui">
            <Switch
              checked={isCurrentConversationOnly}
              onCheckedChange={setIsCurrentConversationOnly}
            />
            {t("rpcLog.currentConversationOnly")}
          </Label>
        </div>
      </div>
      {visibleRows.length === 0 ? (
        <div className="min-h-0 flex-1 overflow-y-auto">
          <ListEmpty
            filtered={frames.length > 0 && isFiltered}
            title={t(isPaused ? "rpcLog.pausedEmpty" : "rpcLog.empty")}
            noMatchTitle={t("rpcLog.noMatch")}
            onClearFilters={clearFilters}
          />
        </div>
      ) : (
        <RpcLogList
          rows={visibleRows}
          selectedFrame={selectedRow?.frame}
          onSelect={setSelectedFrame}
        />
      )}
      <div className="flex h-6 shrink-0 items-center border-t text-caption color-muted">
        <span className="min-w-0 truncate tabular-nums">
          {t("rpcLog.count", {
            visible: visibleRows.length,
            total: frames.length,
          })}
        </span>
      </div>
      {selectedRow && (
        <RpcFrameDetail
          row={selectedRow}
          onClose={() => setSelectedFrame(undefined)}
        />
      )}
    </section>
  )
}

// 帧列表：行高固定，只渲染可见范围附近的行；停在底部时跟着新帧滚动，往上翻后停住，
// 记录满了丢掉最早的帧时补回 scrollTop，正在看的行不上移
function RpcLogList({
  rows,
  selectedFrame,
  onSelect,
}: {
  rows: RpcLogRow[]
  selectedFrame?: RpcFrame
  onSelect(frame: RpcFrame): void
}) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const [viewport, setViewport] = useState({ scrollTop: 0, height: 0 })
  // 是否停在底部？新帧到达时据此决定要不要跟着滚
  const isAtBottom = useRef(true)
  // 上一次渲染的行，用来算这次从头部丢掉了几行
  const previousRowsRef = useRef(rows)
  // 1. 记下滚动区的高度，决定渲染多少行。
  useLayoutEffect(() => {
    const element = scrollRef.current
    if (!element) return
    const observer = new ResizeObserver(() =>
      setViewport({
        scrollTop: element.scrollTop,
        height: element.clientHeight,
      })
    )
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  // 2. 停在底部时，新帧到达后滚到最底；不在底部时按头部丢掉的行数往回补。
  useLayoutEffect(() => {
    const element = scrollRef.current
    const previousRows = previousRowsRef.current
    previousRowsRef.current = rows
    if (!element) return
    if (isAtBottom.current) {
      element.scrollTop = element.scrollHeight
      return
    }
    // 只在头部丢掉、尾部追加时补；换了筛选条件时行对不上，不补
    const droppedRows = previousRows.findIndex(
      (row) => row.frame === rows[0]?.frame
    )
    const isShifted =
      droppedRows > 0 &&
      rows[previousRows.length - 1 - droppedRows]?.frame ===
        previousRows.at(-1)?.frame
    if (isShifted) element.scrollTop -= droppedRows * rowHeight
  }, [rows])
  const first = Math.max(
    0,
    Math.floor(viewport.scrollTop / rowHeight) - overscanRows
  )
  const last = Math.min(
    rows.length,
    Math.ceil((viewport.scrollTop + viewport.height) / rowHeight) + overscanRows
  )
  return (
    <div
      ref={scrollRef}
      className="min-h-0 flex-1 overflow-auto"
      onScroll={(event) => {
        const element = event.currentTarget
        isAtBottom.current =
          element.scrollTop + element.clientHeight >=
          element.scrollHeight - rowHeight
        setViewport({
          scrollTop: element.scrollTop,
          height: element.clientHeight,
        })
      }}
    >
      <div className="relative" style={{ height: rows.length * rowHeight }}>
        {rows.slice(first, last).map((row, offset) => (
          <RpcLogRowButton
            key={getFrameKey(row.frame)}
            frame={row.frame}
            latencyMs={row.latencyMs}
            top={(first + offset) * rowHeight}
            isSelected={row.frame === selectedFrame}
            onSelect={onSelect}
          />
        ))}
      </div>
    </div>
  )
}

// 一帧一行：方向、时间、方法、请求编号、大小、响应耗时；错误响应的方法名标红。
// 新帧到达时属性不变的行跳过重渲染；记录满了以后每丢一帧，所有行的位置都会变
const RpcLogRowButton = memo(function RpcLogRowButton({
  frame,
  latencyMs,
  top,
  isSelected,
  onSelect,
}: {
  frame: RpcFrame
  latencyMs?: number
  top: number
  isSelected: boolean
  onSelect(frame: RpcFrame): void
}) {
  const { t, i18n } = useTranslation()
  const timeFormat = getFrameTimeFormat(i18n.language)
  return (
    <button
      type="button"
      aria-pressed={isSelected}
      className="absolute inset-x-0 flex items-center gap-2 text-left text-table outline-none hover:bg-muted focus-visible:ring-1 focus-visible:ring-ring focus-visible:ring-inset aria-pressed:bg-muted"
      style={{ top, height: rowHeight }}
      onClick={() => onSelect(frame)}
    >
      <RpcDirection direction={frame.direction} />
      <OverflowTooltip
        render={
          <span
            className={cn(
              "min-w-0 flex-1 truncate text-code",
              isErrorResponse(frame) && "color-destructive"
            )}
          />
        }
      >
        {frame.method ?? "—"}
      </OverflowTooltip>
      <span className="shrink-0 text-caption color-muted tabular-nums">
        {timeFormat.format(frame.time)}
      </span>
      {frame.id !== undefined && (
        <span className="hidden shrink-0 text-caption color-muted tabular-nums @min-[560px]:inline">
          #{frame.id}
        </span>
      )}
      <span className="hidden w-12 shrink-0 text-right text-caption color-muted tabular-nums @min-[560px]:inline">
        {formatByteSize(frame.size, i18n.language)}
      </span>
      <span className="hidden w-12 shrink-0 text-right text-caption tabular-nums @min-[560px]:inline">
        {latencyMs !== undefined && t("rpcLog.latency", { latency: latencyMs })}
      </span>
    </button>
  )
})

// 选中帧的详情：方向、方法、耗时，下面用 JsonView 显示遮罩了 api_key 的整帧
function RpcFrameDetail({ row, onClose }: { row: RpcLogRow; onClose(): void }) {
  const { t, i18n } = useTranslation()
  const { frame, latencyMs } = row
  const maskedFrame = useMemo(() => maskRpcFrame(frame), [frame])
  return (
    <div className="flex max-h-[40vh] min-h-0 shrink-0 flex-col gap-2 border-t pt-3">
      <div className="flex min-w-0 items-center gap-2 text-ui">
        <RpcDirection direction={frame.direction} />
        <OverflowTooltip
          render={
            <span
              className={cn(
                "min-w-0 flex-1 truncate text-code",
                isErrorResponse(frame) && "color-destructive"
              )}
            />
          }
        >
          {frame.method ?? "—"}
        </OverflowTooltip>
        <Tooltip>
          <TooltipTrigger
            render={
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={t("rpcLog.closeDetail")}
                onClick={onClose}
              />
            }
          >
            <XIcon />
          </TooltipTrigger>
          <TooltipContent className="text-caption">
            {t("rpcLog.closeDetail")}
          </TooltipContent>
        </Tooltip>
      </div>
      <div className="flex shrink-0 flex-wrap items-center gap-2 text-caption color-muted tabular-nums">
        <span>{getFrameTimeFormat(i18n.language).format(frame.time)}</span>
        {frame.id !== undefined && <span>#{frame.id}</span>}
        <span>{formatByteSize(frame.size, i18n.language)}</span>
        {latencyMs !== undefined && (
          <span>{t("rpcLog.latency", { latency: latencyMs })}</span>
        )}
      </div>
      <JsonView value={maskedFrame} maxHeight="40vh" heightMode="host" />
    </div>
  )
}

// 发出和收到使用同一方向小标，箭头和可访问名称同时提供方向。
function RpcDirection({ direction }: { direction: RpcFrame["direction"] }) {
  // 1. 方向独立着色，错误和选中样式留给各自内容。
  const { t } = useTranslation()
  const Icon = direction === "sent" ? ArrowUpIcon : ArrowDownIcon
  return (
    <span
      className={cn(
        "inline-flex size-5 shrink-0 items-center justify-center",
        direction === "sent"
          ? "bg-rpc-sent/10 color-rpc-sent"
          : "bg-rpc-received/10 color-rpc-received"
      )}
    >
      <Icon
        role="img"
        className="size-4"
        aria-label={t(
          direction === "sent"
            ? "rpcLog.direction.sent"
            : "rpcLog.direction.received"
        )}
      />
    </span>
  )
}

// 响应带 error 字段吗？
function isErrorResponse(frame: RpcFrame): boolean {
  return (
    frame.direction === "received" &&
    frame.id !== undefined &&
    !!(frame.payload as { error?: unknown } | null)?.error
  )
}

// 按语言缓存的帧时间格式；Intl 格式对象创建开销大，不在每行每次渲染时新建
const frameTimeFormatsByLanguage = new Map<string, Intl.DateTimeFormat>()
// 每帧一个稳定的 React key：记录满了丢掉最早的帧时，已有行不重新挂载
const frameKeys = new WeakMap<RpcFrame, number>()
let nextFrameKey = 0

function getFrameKey(frame: RpcFrame): number {
  let key = frameKeys.get(frame)
  if (key === undefined) {
    key = nextFrameKey++
    frameKeys.set(frame, key)
  }
  return key
}

// 帧时间是 24 小时制的时:分:秒.毫秒
function getFrameTimeFormat(language: string): Intl.DateTimeFormat {
  let format = frameTimeFormatsByLanguage.get(language)
  if (format) return format
  format = new Intl.DateTimeFormat(language, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    fractionalSecondDigits: 3,
    hour12: false,
  })
  frameTimeFormatsByLanguage.set(language, format)
  return format
}
