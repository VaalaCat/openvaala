import { shouldShowLoadError } from "@/lib/rpc-error"
import { useStore } from "@nanostores/react"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useState, type ReactNode } from "react"
import type { ParseKeys } from "i18next"
import { RefreshCwIcon } from "lucide-react"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import { TokenNumberValue } from "@/components/base/token-number-value"
import { QueryState } from "@/components/base/query-state"
import { DisclosureChevron } from "@/components/conversation/item-notice"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import { LatticeLoader } from "@/components/base/lattice-loader"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type {
  ContextUsageSource,
  ModelDefinition,
  SessionContextUsage,
  SessionDetails,
} from "@/lib/api/protocol.gen"
import { contextUsageOptions, sessionDetailsOptions } from "@/lib/query/session"
import { formatModelRef } from "@/lib/utils"

const contextSourceKeys = {
  actual: "conversation.context.source.actual",
  estimated: "conversation.context.source.estimated",
  unknown: "conversation.context.source.unknown",
} as const satisfies Record<ContextUsageSource, ParseKeys>

// 展示下一次预计占用和最近请求用量，按需读取 Session 详情。
export function ContextTab({ sync }: { sync: ConversationSync }) {
  // 1. 查询用量，Session 详情只在展开时读取。
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const sessionID = sync.getFollowedSessionID()
  const [isDetailsOpen, setIsDetailsOpen] = useState(false)
  const usage = useQuery(
    contextUsageOptions(connection, sync.conversationID, sessionID)
  )
  const details = useQuery({
    ...sessionDetailsOptions(connection, sync.conversationID, sessionID),
    enabled: isDetailsOpen,
  })
  const isFetching = usage.isFetching || details.isFetching

  // 2. 主数字保持外露，配置和原始数据各自折叠。
  return (
    <div className="flex min-w-0 flex-col gap-4">
      <section className="flex min-w-0 flex-col gap-2">
        <div className="flex items-center justify-between gap-2">
          <h3 className="text-section">
            {t("conversation.panel.context.nextRequest")}
          </h3>
          <Button
            variant="outline"
            className="text-ui"
            disabled={isFetching || !isConnected}
            onClick={() => {
              void usage.refetch()
              if (isDetailsOpen) void details.refetch()
            }}
          >
            {isFetching ? (
              <LatticeLoader
                data-icon="inline-start"
                aria-label={t("common.loading")}
              />
            ) : (
              <RefreshCwIcon data-icon="inline-start" />
            )}
            {t("conversation.panel.refresh")}
          </Button>
        </div>
        <QueryState shouldShowError={shouldShowLoadError} query={usage}>
          {(data) => <ContextUsageFacts usage={data} />}
        </QueryState>
      </section>
      <Collapsible
        open={isDetailsOpen}
        onOpenChange={setIsDetailsOpen}
        className="flex min-w-0 flex-col"
      >
        <CollapsibleTrigger className="group flex h-8 items-center gap-2 text-section">
          <DisclosureChevron />
          {t("conversation.panel.details.title")}
        </CollapsibleTrigger>
        <CollapsibleContent
          className="process-disclosure-panel min-w-0"
          inert={!isDetailsOpen}
          aria-hidden={!isDetailsOpen || undefined}
        >
          <div className="process-disclosure-content flex min-w-0 flex-col gap-2 pt-2">
            <QueryState shouldShowError={shouldShowLoadError} query={details}>
              {(data) => <SessionDetailsFacts details={data} />}
            </QueryState>
          </div>
        </CollapsibleContent>
      </Collapsible>
      <RawData value={usage.data} />
    </div>
  )
}

// 分开列出预计占用和服务商上报的最近请求；缓存读取归在输入下面。
function ContextUsageFacts({ usage }: { usage: SessionContextUsage }) {
  // 1. 预计值统一带近似标记，尚未测量时显示未知。
  const { t } = useTranslation()
  const { next_request: nextRequest, last_request: lastRequest } = usage
  const isMeasured = nextRequest.source !== "unknown"
  const unknown = t("conversation.context.source.unknown")
  return (
    <div className="flex min-w-0 flex-col gap-4">
      <FactList
        facts={[
          [
            t("conversation.panel.context.estimate"),
            <span className="inline-flex items-center gap-1">
              <Tooltip>
                <TooltipTrigger render={<span className="text-help" />}>
                  {isMeasured ? (
                    <TokenNumberValue value={nextRequest.tokens} prefix="≈" />
                  ) : (
                    unknown
                  )}
                </TooltipTrigger>
                <TooltipContent className="text-caption">
                  {t(contextSourceKeys[nextRequest.source])}
                </TooltipContent>
              </Tooltip>
              {nextRequest.window > 0 && (
                <>
                  {" "}
                  / <TokenNumberValue value={nextRequest.window} />
                </>
              )}
            </span>,
          ],
          [
            t("conversation.panel.context.occupancy"),
            isMeasured && nextRequest.window > 0
              ? `${Math.round((nextRequest.tokens / nextRequest.window) * 100)}%`
              : unknown,
          ],
          [
            t("conversation.panel.context.threshold"),
            usage.threshold_tokens > 0 ? (
              <TokenNumberValue value={usage.threshold_tokens} />
            ) : (
              unknown
            ),
          ],
          [
            t("conversation.panel.context.systemTokens"),
            isMeasured ? (
              <TokenNumberValue value={usage.system_tokens} prefix="≈" />
            ) : (
              unknown
            ),
          ],
          [
            t("conversation.panel.context.toolTokens"),
            isMeasured ? (
              <TokenNumberValue value={usage.tool_tokens} prefix="≈" />
            ) : (
              unknown
            ),
          ],
        ]}
      />
      <section className="flex min-w-0 flex-col gap-2">
        <h3 className="text-section">
          {t("conversation.panel.context.lastRequest")}
        </h3>
        {lastRequest ? (
          <FactList
            facts={[
              [
                t("conversation.panel.details.model"),
                formatModelRef(lastRequest.model),
              ],
              [
                t("conversation.panel.context.input"),
                <TokenNumberValue value={lastRequest.usage.input} />,
              ],
              [
                t("conversation.panel.context.cacheRead"),
                <TokenNumberValue value={lastRequest.usage.cache_read} />,
              ],
              [
                t("conversation.panel.context.output"),
                <TokenNumberValue value={lastRequest.usage.output} />,
              ],
            ]}
            indentLabel={t("conversation.panel.context.cacheRead")}
          />
        ) : (
          <p className="text-caption color-muted">
            {t("conversation.panel.context.noLastRequest")}
          </p>
        )}
      </section>
    </div>
  )
}

// 列出 Session 的 Agent、模型、Executor、Skill 和工具。
function SessionDetailsFacts({ details }: { details: SessionDetails }) {
  // 1. 展示完整配置的摘要和可展开原始数据。
  const { t } = useTranslation()
  return (
    <>
      <FactList
        facts={[
          [t("conversation.panel.details.agent"), details.session.agent_name],
          [t("conversation.panel.details.model"), getModelName(details.model)],
          [
            t("conversation.panel.details.compactModel"),
            getModelName(details.compact_model),
          ],
          [
            t("conversation.panel.details.executors"),
            details.executors.map((each) => each.id).join(", "),
          ],
          [
            t("conversation.panel.details.skills", {
              count: details.skills.length,
            }),
            details.skills.join(", "),
          ],
          [
            t("conversation.panel.details.tools", {
              count: details.tools.length,
            }),
            details.tools.join(", "),
          ],
        ]}
      />
      <RawData value={details} />
    </>
  )
}

// 按需展开已取得的原始数据。
function RawData({ value }: { value: unknown }) {
  // 1. 未取得数据时不展示空入口。
  const { t } = useTranslation()
  if (value === undefined) return null
  return (
    <Collapsible className="min-w-0">
      <CollapsibleTrigger className="group flex h-8 items-center gap-2 text-ui color-muted">
        <DisclosureChevron />
        {t("conversation.panel.rawData")}
      </CollapsibleTrigger>
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
        <div className="process-disclosure-content">
          <JsonView value={value} defaultExpandDepth={1} />
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

// 字段名和数值按两列对齐；长名称只在截断时提供完整提示。
function FactList({
  facts,
  indentLabel,
}: {
  facts: [string, ReactNode][]
  indentLabel?: string
}) {
  // 1. 数量控件保留触摸和键盘入口，名称单行省略。
  return (
    <dl className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)] items-center gap-x-3 gap-y-1 text-table">
      {facts.map(([label, value]) => (
        <div key={label} className="contents">
          <dt
            className="color-muted"
            style={
              label === indentLabel ? { paddingInlineStart: 12 } : undefined
            }
          >
            {label}
          </dt>
          <OverflowTooltip
            disabled={typeof value !== "string"}
            render={<dd className="min-w-0 truncate text-right tabular-nums" />}
          >
            {value || "—"}
          </OverflowTooltip>
        </div>
      ))}
    </dl>
  )
}

// 模型优先使用显示名，其次使用 provider/model。
function getModelName(model: ModelDefinition | null): string {
  // 1. 读取模型显示名。
  return model ? model.display_name || formatModelRef(model.ref) : ""
}
