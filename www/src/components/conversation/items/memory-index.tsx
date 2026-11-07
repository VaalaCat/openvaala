import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { Brain } from "lucide"
import { useTranslation } from "react-i18next"
import {
  DetailSection,
  ItemNotice,
} from "@/components/conversation/item-notice"
import { MarkdownView } from "@/components/conversation/markdown-view"
import type { ItemRendererProps } from "@/lib/item-renderers"

// memory_index：memory Executor 里的 MEMORY.md 读进了上下文，路径单行省略；点开看全文，截断过会再提示一行
export function MemoryIndexItem({ view }: ItemRendererProps) {
  // 1. 标题保留动作，文件位置、截断提示和正文放进展开区。
  const { t } = useTranslation()
  const index = view.item.memory_index
  if (!index) return null
  return (
    <ItemNotice
      icon={Brain}
      details={
        <div className="flex min-w-0 flex-col gap-2">
          <DetailSection label={t("conversation.notice.file")}>
            <OverflowTooltip render={<p className="truncate text-table" />}>
              {index.path}@memory
            </OverflowTooltip>
          </DetailSection>
          {index.is_truncated && (
            <p className="text-caption color-muted">
              {t("conversation.notice.memoryIndexTruncated")}
            </p>
          )}
          <MarkdownView text={index.text} />
        </div>
      }
    >
      {t("conversation.notice.memoryIndex")}
    </ItemNotice>
  )
}
