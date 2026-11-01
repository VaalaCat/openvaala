import { memo, type ReactNode } from "react"
import type { ParseKeys } from "i18next"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { TokenNumberValue } from "@/components/base/token-number-value"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { Separator } from "@/components/ui/separator"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { Turn, TurnID, TurnStatus } from "@/lib/api/protocol.gen"
import { formatDuration } from "@/lib/relative-time"
import { cn } from "@/lib/utils"

// Turn 终态的颜色和文案
const turnStatuses = {
  running: { tone: "neutral", labelKey: "conversation.turn.status.running" },
  completed: {
    tone: "success",
    labelKey: "conversation.turn.status.completed",
  },
  failed: { tone: "destructive", labelKey: "conversation.turn.status.failed" },
  interrupted: {
    tone: "warning",
    labelKey: "conversation.turn.status.interrupted",
  },
} as const satisfies Record<TurnStatus, { tone: string; labelKey: ParseKeys }>

// 一轮执行结束处的分隔行：状态、已保存 Item 的耗时、用量与整轮调试入口。
// 状态优先用推送来的，没有时用 Turn 记录；记录还在读时先占住一行高度（只画分隔线），
// 读完仍没有状态（例如子 Agent 继承来的历史）时不显示
export const TurnDivider = memo(function TurnDivider({
  turnID,
  turn,
  isLoadingTurn,
  savedDurationMs,
  sync,
  rawDataAction,
}: {
  turnID: TurnID
  // session/turns/get 读到的记录；还没读到或不存在时为空
  turn?: Turn
  // 这一轮的记录是否还在读？
  isLoadingTurn: boolean
  savedDurationMs?: number
  sync: ConversationSync
  rawDataAction?: ReactNode
}) {
  const { t, i18n } = useTranslation()
  const turnView = useStore(sync.$turns, { keys: [turnID] })[turnID]
  const status = turnView?.status ?? turn?.status
  // 记录读到之前只画分隔线占位，读到后这一行高度不变，上方插入时正在看的位置不跳
  if (!status)
    return isLoadingTurn ? (
      <div aria-hidden className="flex h-8 items-center">
        <Separator />
      </div>
    ) : null
  const error = turnView?.error ?? turn?.error
  const usage = turn?.usage
  const hasDuration = status !== "running" && savedDurationMs !== undefined
  const hasUsage =
    status !== "running" && !!usage && (usage.input > 0 || usage.output > 0)
  return (
    <div className="flex flex-col gap-2" data-turn-status={status}>
      <div className="flex min-h-8 items-center gap-2 text-caption color-muted">
        <Separator
          aria-hidden
          className="min-w-6 flex-1 data-horizontal:w-auto"
        />
        <div className="flex min-w-0 flex-wrap items-center justify-center gap-x-2 gap-y-1">
          <TurnStatusLabel
            status={status}
            isIconOnlyWhenCompleted
            iconContainerClassName="size-4"
          />
          {hasDuration && (
            <span className="shrink-0 text-caption whitespace-nowrap">
              {formatDuration(savedDurationMs, i18n.language)}
            </span>
          )}
          {hasUsage && (
            <span className="inline-flex flex-wrap items-center justify-center gap-2">
              {[
                {
                  value: usage.input,
                  arrow: "↑",
                  label: t("conversation.panel.context.input"),
                },
                {
                  value: usage.output,
                  arrow: "↓",
                  label: t("conversation.panel.context.output"),
                },
              ].map(({ value, arrow, label }) => (
                <span
                  key={arrow}
                  className="inline-flex shrink-0 items-center gap-1"
                >
                  <Tooltip>
                    <TooltipTrigger
                      render={<span />}
                      role="img"
                      aria-label={label}
                      tabIndex={0}
                      delay={600}
                      closeOnClick
                      className="inline-flex h-5 items-center rounded-sm outline-none focus-visible:ring-1 focus-visible:ring-ring"
                    >
                      <span aria-hidden>{arrow}</span>
                    </TooltipTrigger>
                    <TooltipContent className="text-caption">
                      {label}
                    </TooltipContent>
                  </Tooltip>
                  <TokenNumberValue value={value} />
                </span>
              ))}
            </span>
          )}
          {rawDataAction}
        </div>
        <Separator
          aria-hidden
          className="min-w-6 flex-1 data-horizontal:w-auto"
        />
      </div>
      {/* 正常中断只展示状态；失败原因保留在分隔行下方。 */}
      {status === "failed" && error && (
        <p className="text-center text-caption wrap-anywhere color-destructive">
          {error}
        </p>
      )}
    </div>
  )
})

// Turn 状态：运行中显示加载格点，结束后是带颜色的点和文字，serve 新增的状态用中性色、原样显示；分隔行和右栏 Turn 列表共用
export function TurnStatusLabel({
  status,
  isIconOnlyWhenCompleted = false,
  iconContainerClassName,
}: {
  status: TurnStatus
  // 已完成时只画绿点，文字只给读屏（分隔行每一轮都有，失败、中断仍写文字）
  isIconOnlyWhenCompleted?: boolean
  iconContainerClassName?: string
}) {
  const { t } = useTranslation()
  const known = turnStatuses[status]
  const tone = known?.tone ?? "neutral"
  return (
    <span className="flex shrink-0 items-center gap-2">
      <span
        className={cn(
          "inline-flex shrink-0 items-center justify-center",
          iconContainerClassName
        )}
      >
        {status === "running" ? (
          <LatticeLoader aria-hidden />
        ) : (
          <span
            aria-hidden
            data-status-tone={tone}
            className="size-2 shrink-0 rounded-full bg-current"
          />
        )}
      </span>
      <span
        data-status-tone={status === "running" ? undefined : tone}
        className={cn(
          isIconOnlyWhenCompleted && status === "completed" && "sr-only"
        )}
      >
        {known ? t(known.labelKey) : status}
      </span>
    </span>
  )
}
