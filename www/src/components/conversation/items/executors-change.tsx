import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { ServerCog } from "lucide"
import { useTranslation } from "react-i18next"
import {
  DetailSection,
  ItemNotice,
} from "@/components/conversation/item-notice"
import type { TurnExecutor } from "@/lib/api/protocol.gen"
import type { ItemRendererProps } from "@/lib/item-renderers"

// executors_change：列出变更前后的 Executor，展开后显示目录或说明。
export function ExecutorsChangeItem({ view }: ItemRendererProps) {
  const { t } = useTranslation()
  const change = view.item.executors_change
  if (!change) return null
  // Go 的 nil 切片编码成 null
  const previousExecutors = change.previous_executors ?? []
  const executors = change.executors ?? []
  return (
    <ItemNotice
      icon={ServerCog}
      details={
        <div className="flex flex-col gap-2">
          <DetailSection label={t("conversation.notice.previousExecutors")}>
            <ExecutorList executors={previousExecutors} />
          </DetailSection>
          <DetailSection label={t("conversation.notice.currentExecutors")}>
            <ExecutorList executors={executors} />
          </DetailSection>
        </div>
      }
    >
      {t("conversation.notice.executorsChange", {
        previous: formatExecutorIDs(previousExecutors),
        executors: formatExecutorIDs(executors),
      })}
    </ItemNotice>
  )
}

function ExecutorList({ executors }: { executors: TurnExecutor[] }) {
  return (
    <ul className="flex flex-col">
      {executors.map((executor) => {
        const detail = executor.workspace_directory || executor.description
        return (
          <OverflowTooltip
            key={executor.id}
            render={<li className="truncate text-table" />}
          >
            {detail ? `${executor.id}: ${detail}` : executor.id}
          </OverflowTooltip>
        )
      })}
    </ul>
  )
}

function formatExecutorIDs(executors: TurnExecutor[]): string {
  return executors.map((executor) => executor.id).join(", ") || "—"
}
