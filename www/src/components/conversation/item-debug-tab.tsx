import { useStore } from "@nanostores/react"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import { ListEmpty } from "@/components/base/list-empty"
import { RequestError } from "@/components/base/request-error"
import { Skeleton } from "@/components/ui/skeleton"
import { useServerConnection } from "@/components/server-connection-provider"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import {
  isTurnRangeLoaded,
  type ItemDebugTarget,
  type MessageRow,
} from "@/lib/item-renderers"
import { turnsOptions } from "@/lib/query/session"
import { shouldShowLoadError } from "@/lib/rpc-error"

// 调试页签按选择读取当前消息行，单条和整轮使用同一个 JSON 查看器。
export function ItemDebugTab({
  sync,
  target,
  rows,
}: {
  sync: ConversationSync
  target?: ItemDebugTarget
  rows: MessageRow[]
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const turnViews = useStore(sync.$turns)
  // 1. 复用消息区的同一份 Turn 查询，不为调试加载更早的历史。
  const turns = useQuery(
    turnsOptions(
      connection,
      sync.conversationID,
      sync.getFollowedSessionID(),
      rows.flatMap((row) => (row.kind === "turn" ? [row.turnID] : []))
    )
  )
  if (!target) return <ListEmpty title={t("conversation.debug.empty")} />
  const contentRows = rows.flatMap((row) =>
    row.kind === "turn" ? row.rows : [row]
  )
  const itemRows = contentRows.flatMap((row) =>
    row.kind === "group" ? row.rows : [row]
  )
  // 2. 选择只负责定位，流式追加与结果到达始终读取最新记录。
  let value: unknown
  let isPartial = false
  let isStreaming = false
  if (target.kind === "item") {
    const row = itemRows.find(
      (row) => row.view.key === target.key || row.toolResult?.key === target.key
    )
    if (row) {
      const isResultSelected = row.toolResult?.key === target.key
      const view = isResultSelected ? row.toolResult! : row.view
      value = {
        item: view.item,
        call: isResultSelected ? row.view.item : undefined,
        result: isResultSelected ? undefined : row.toolResult?.item,
        view: {
          key: view.key,
          ordinal: view.ordinal,
          is_streaming: view.isStreaming,
        },
      }
      isStreaming = row.view.isStreaming || !!row.toolResult?.isStreaming
    }
  } else {
    const items = rows.flatMap((row) =>
      row.kind === "turn" && row.turnID === target.turnID ? row.items : []
    )
    const turn = turns.data?.turns.find((turn) => turn.id === target.turnID)
    const turnView = turnViews[target.turnID]
    if (items.length || turn || turnView) {
      value = { turn: turn ?? turnView, items: items.map((view) => view.item) }
      isPartial = !isTurnRangeLoaded(items, turn)
      isStreaming = items.some((view) => view.isStreaming)
    }
  }
  if (value === undefined && target.kind === "turn") {
    if (turns.isError && shouldShowLoadError(turns.error))
      return (
        <RequestError
          error={turns.error}
          onRetry={() => void turns.refetch()}
        />
      )
    if (turns.isPending || turns.isError)
      return <Skeleton className="h-28 w-full" />
  }
  if (value === undefined)
    return <ListEmpty title={t("conversation.debug.unavailable")} />
  return (
    <div
      className="flex min-h-0 min-w-0 flex-1 flex-col gap-2"
      data-debug-kind={target.kind}
    >
      <h2 className="text-section">
        {target.kind === "turn"
          ? t("conversation.turn.rawData")
          : t("conversation.debug.item")}
      </h2>
      {isPartial && (
        <p className="text-caption color-muted">
          {t("conversation.turn.partialRawData")}
        </p>
      )}
      {target.kind === "turn" &&
        turns.isError &&
        shouldShowLoadError(turns.error) && (
          <RequestError
            error={turns.error}
            onRetry={() => void turns.refetch()}
          />
        )}
      <JsonView
        key={
          target.kind +
          ":" +
          (target.kind === "turn" ? target.turnID : target.key)
        }
        value={value}
        isStreaming={isStreaming}
        heightMode="host"
        maxHeight="none"
      />
    </div>
  )
}
