import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { ServerCog } from "lucide"
import { useTranslation } from "react-i18next"
import {
  DetailSection,
  ItemNotice,
} from "@/components/conversation/item-notice"
import type { TurnExecutor } from "@/lib/api/protocol.gen"
import type { ItemRendererProps } from "@/lib/item-renderers"

// executors_change：对话可用的 Executor 变了；一句话列出前后的 Executor，点开看各自的工作目录
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
      {executors.map((executor) => (
        <OverflowTooltip
          key={executor.id}
          render={<li className="truncate text-table" />}
        >
          {executor.id}: {executor.workspace_directory}
        </OverflowTooltip>
      ))}
    </ul>
  )
}

function formatExecutorIDs(executors: TurnExecutor[]): string {
  return executors.map((executor) => executor.id).join(", ") || "—"
}
