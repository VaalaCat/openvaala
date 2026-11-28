import { CircleAlert, ListTree } from "lucide"
import { LayoutGroup, motion } from "motion/react"
import {
  memo,
  useCallback,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react"
import { BracesIcon } from "lucide-react"
import { useStore } from "@nanostores/react"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { ImagePreviewStrip } from "@/components/base/image-preview"
import { RequestError } from "@/components/base/request-error"
import { RotatingText } from "@/components/base/rotating-text"
import { VirtualMessageList } from "@/components/base/virtual-message-list"
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
import { turnsOptions } from "@/lib/query/session"
import { getSessionName } from "@/lib/session-tree"
import { getStringField, parseToolArguments } from "@/lib/tool-arguments"
import { shouldShowLoadError } from "@/lib/rpc-error"
import { cn } from "@/lib/utils"

// 订阅对话展示行、Turn 记录与展开选择，把 Agent 专属内容交给虚拟消息列表。
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

  const listRows = useMemo(
    () =>
      isReady
        ? rows.filter(
            (row) => row.kind === "turn" || hasItemPresentation(row.view)
          )
        : [],
    [isReady, rows]
  )
  const [isLoadingEarlier, setIsLoadingEarlier] = useState(false)
  const [earlierItemsError, setEarlierItemsError] = useState<unknown>()
  const syncRef = useRef(sync)
  const [readingTarget, setReadingTarget] = useState(sync)
  if (readingTarget !== sync) {
    setReadingTarget(sync)
    setChoicesByTurn({})
    setIsLoadingEarlier(false)
    setEarlierItemsError(undefined)
  }
  useLayoutEffect(() => {
    syncRef.current = sync
  }, [sync])

  const isConnected = useStore(connection.$state) === "connected"
  const loadEarlierItems = useCallback(async () => {
    if (!isConnected || !isReady) return
    setIsLoadingEarlier(true)
    setEarlierItemsError(undefined)
    try {
      await sync.loadEarlierItems()
    } catch (error) {
      if (syncRef.current === sync) setEarlierItemsError(error)
    } finally {
      if (syncRef.current === sync) setIsLoadingEarlier(false)
    }
  }, [sync, isConnected, isReady])
  const showEarlierError =
    !!earlierItemsError && shouldShowLoadError(earlierItemsError)

  return (
    <VirtualMessageList
      rows={listRows}
      getRowKey={getMessageRowKey}
      renderRow={(row) =>
        row.kind === "turn" ? (
          <TurnTimeline
            turnID={row.turnID}
            rows={row.rows}
            items={row.items}
            replyKeyToCopy={replyKeysToCopy.get(row.turnID)}
            turn={turns.data?.[row.turnID]}
            turnView={turnViews[row.turnID]}
            isLoadingTurn={turns.isFetching && !turns.data?.[row.turnID]}
            savedDurationMs={turnSavedDurations.get(row.turnID)}
            sync={sync}
            isSubSession={isSubSession}
            choices={choicesByTurn[row.turnID] ?? noTimelineChoices}
            chooseTurnOpen={chooseTurnOpen}
            onInspect={onInspect}
          />
        ) : (
          <TimelineItem
            row={row}
            copyReply={row.view.key === replyKeysToCopy.get(row.view.turnID)}
            sync={sync}
            isSubSession={isSubSession}
            choices={choicesByTurn[outsideTurnChoicesKey] ?? noTimelineChoices}
            chooseOpen={chooseOutsideTurnOpen}
            onInspect={onInspect}
          />
        )
      }
      header={
        isReady ? (
          <>
            {header}
            {rows.length === 0 && (
              <p className="py-8 text-center text-body color-muted">
                {t("conversation.empty")}
              </p>
            )}
          </>
        ) : (
          loadingContent
        )
      }
      footer={footer}
      ariaLabel={t("conversation.messages")}
      earlier={{
        hasMore: isReady && hasEarlierItems,
        canLoad:
          isConnected && isReady && !isLoadingEarlier && !showEarlierError,
        load: loadEarlierItems,
        content: (retry) =>
          showEarlierError ? (
            <RequestError error={earlierItemsError} onRetry={retry} />
          ) : (
            <LatticeLoader
              aria-label={t("conversation.loadingEarlier")}
              className={isLoadingEarlier ? "color-muted" : "invisible"}
            />
          ),
      }}
      resetKey={sync}
    />
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
