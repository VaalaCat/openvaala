import { shouldShowLoadError } from "@/lib/rpc-error"
import { useStore } from "@nanostores/react"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useInfiniteQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { DataGlyph } from "@/components/base/data-glyph"
import { InfiniteList } from "@/components/base/infinite-list"
import { JsonView } from "@/components/base/json-view"
import { ListEmpty } from "@/components/base/list-empty"
import { TokenNumberValue } from "@/components/base/token-number-value"
import { DisclosureChevron } from "@/components/conversation/item-notice"
import { TurnStatusLabel } from "@/components/conversation/turn-divider"
import { useServerConnection } from "@/components/server-connection-provider"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { Turn } from "@/lib/api/protocol.gen"
import { turnListOptions } from "@/lib/query/session"
import { formatRelativeTime } from "@/lib/relative-time"

// Turn 页签：跟随 Session 的 Turn 列表，最新的在上面，滚到底加载更早的；
// 每行是状态、输入用量柱（相对已加载里输入最多的一轮）、用量和保存时间，点开看原始 Turn 记录
export function TurnsTab({ sync }: { sync: ConversationSync }) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const turns = useInfiniteQuery(
    turnListOptions(
      connection,
      sync.conversationID,
      sync.getFollowedSessionID()
    )
  )
  const maxInput = Math.max(
    0,
    ...(turns.data?.pages ?? []).flatMap((page) =>
      page.turns.map((turn) => turn.usage?.input ?? 0)
    )
  )
  return (
    <InfiniteList
      shouldShowError={shouldShowLoadError}
      paused={!isConnected}
      query={turns}
      direction="down"
      getItems={(page) => page.turns}
      renderItem={(turn) => (
        <TurnRow key={turn.id} turn={turn} maxInput={maxInput} />
      )}
      emptyState={<ListEmpty title={t("conversation.panel.turns.empty")} />}
    />
  )
}

// 一轮的摘要行，点开在下方显示原始记录
function TurnRow({ turn, maxInput }: { turn: Turn; maxInput: number }) {
  const { t, i18n } = useTranslation()
  const usage = turn.status !== "running" ? turn.usage : undefined
  const hasUsage = !!usage && (usage.input > 0 || usage.output > 0)
  const inputPercent =
    usage && maxInput > 0 ? (usage.input / maxInput) * 100 : 0
  return (
    <li className="flex min-w-0 flex-col border-b">
      <Collapsible>
        <div className="flex min-h-8 min-w-0 flex-wrap items-center gap-2 text-ui">
          <CollapsibleTrigger className="group flex h-8 shrink-0 items-center gap-2 text-left text-ui outline-none hover:bg-muted focus-visible:bg-muted">
            <DisclosureChevron className="color-muted" />
            <TurnStatusLabel status={turn.status} />
          </CollapsibleTrigger>
          {hasUsage && (
            <>
              <DataGlyph
                kind="bar"
                values={[inputPercent]}
                label={t("conversation.panel.turns.inputShare", {
                  percent: Math.round(inputPercent),
                })}
                className="shrink-0 text-section color-muted"
              />
              <span className="inline-flex min-w-0 flex-wrap items-center gap-2 text-caption color-muted tabular-nums">
                <span className="inline-flex items-center gap-1">
                  {t("conversation.panel.context.input")}
                  <TokenNumberValue value={usage.input} />
                </span>
                <span className="inline-flex items-center gap-1">
                  {t("conversation.panel.context.output")}
                  <TokenNumberValue value={usage.output} />
                </span>
              </span>
            </>
          )}
          {turn.updated_at !== undefined && (
            <Tooltip>
              <TooltipTrigger
                render={
                  <span className="ml-auto shrink-0 text-caption color-muted" />
                }
              >
                {formatRelativeTime(turn.updated_at, i18n.language)}
              </TooltipTrigger>
              <TooltipContent className="text-caption">
                {new Date(turn.updated_at).toLocaleString(i18n.language)}
              </TooltipContent>
            </Tooltip>
          )}
        </div>
        <CollapsibleContent
          className="process-disclosure-panel"
          render={(props, state) => (
            <div
              {...props}
              inert={!state.open}
              aria-hidden={!state.open || undefined}
            />
          )}
        >
          <div className="process-disclosure-content pb-2">
            <JsonView value={turn} maxHeight="24rem" />
          </div>
        </CollapsibleContent>
      </Collapsible>
    </li>
  )
}
