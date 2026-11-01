import type { ParseKeys } from "i18next"
import { Webhook } from "lucide"
import { useTranslation } from "react-i18next"
import {
  DetailSection,
  ItemNotice,
  ItemStatus,
} from "@/components/conversation/item-notice"
import type { HookRunStatus } from "@/lib/api/protocol.gen"
import type { ItemRendererProps } from "@/lib/item-renderers"
import { formatDuration } from "@/lib/relative-time"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"

// hook 运行结局的颜色和文案
const hookRunStatuses = {
  running: { tone: undefined, labelKey: "conversation.hook.status.running" },
  completed: {
    tone: undefined,
    labelKey: undefined,
  },
  blocked: { tone: "warning", labelKey: "conversation.hook.status.blocked" },
  stopped: { tone: "warning", labelKey: "conversation.hook.status.stopped" },
  failed: { tone: "destructive", labelKey: "conversation.hook.status.failed" },
} as const satisfies Record<
  HookRunStatus,
  { tone?: "warning" | "destructive"; labelKey?: ParseKeys }
>

// hook_run：一次 hook 运行的事件、命令和结局；点开看拦截或停止的原因、给模型的补充内容、改写的参数、出错原因和定义位置
export function HookRunItem({ view }: ItemRendererProps) {
  const { t, i18n } = useTranslation()
  const run = view.item.hook_run
  if (!run) return null
  const { tone, labelKey } = hookRunStatuses[run.status]
  const fields: [label: string, value: string | undefined][] = [
    [t("conversation.hook.blockReason"), run.block_reason],
    [t("conversation.hook.stopReason"), run.stop_reason],
    [t("conversation.hook.systemMessage"), run.system_message],
    [t("conversation.hook.additionalContext"), run.additional_context],
    [t("conversation.hook.error"), run.error_message],
  ]
  return (
    <ItemNotice
      icon={Webhook}
      tone={tone}
      status={
        run.status !== "completed" && labelKey ? (
          <ItemStatus state={run.status} label={t(labelKey)} />
        ) : undefined
      }
      details={
        <div className="flex flex-col gap-2">
          {run.completed_at && (
            <p className="text-caption color-muted">
              {formatDuration(run.completed_at - run.started_at, i18n.language)}
            </p>
          )}
          {fields.map(
            ([label, value]) =>
              value && (
                <DetailSection key={label} label={label}>
                  <p className="text-body wrap-anywhere whitespace-pre-wrap">
                    {value}
                  </p>
                </DetailSection>
              )
          )}
          {run.updated_tool_arguments && (
            <DetailSection label={t("conversation.hook.updatedArguments")}>
              <pre className="overflow-x-auto text-code">
                {run.updated_tool_arguments}
              </pre>
            </DetailSection>
          )}
          <DetailSection label={t("conversation.hook.source")}>
            <OverflowTooltip render={<p />} className="truncate text-table">
              {run.source_path}@{run.executor_id}
            </OverflowTooltip>
          </DetailSection>
        </div>
      }
    >
      <span className="inline-flex max-w-full min-w-0 items-center gap-2">
        <span className="shrink-0">
          {t("conversation.hook.event", { event: run.event })}
        </span>
        <OverflowTooltip className="min-w-0 truncate">
          {run.handler}
        </OverflowTooltip>
      </span>
    </ItemNotice>
  )
}
