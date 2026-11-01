import { Archive } from "lucide"
import { useTranslation } from "react-i18next"
import { ItemNotice } from "@/components/conversation/item-notice"
import { MarkdownView } from "@/components/conversation/markdown-view"
import type { ItemRendererProps } from "@/lib/item-renderers"

// context_summary：压缩上下文后的摘要，之前的历史不再发给模型；点开看摘要原文
export function ContextSummaryItem({ view }: ItemRendererProps) {
  // 1. 保留摘要身份，以独立状态说明压缩是否完成。
  const { t } = useTranslation()
  const summary = view.item.context_summary
  const isCompacting = view.isStreaming || view.ordinal === undefined
  return (
    <ItemNotice
      icon={Archive}
      details={summary?.text && <MarkdownView text={summary.text} />}
    >
      <span className={isCompacting ? "shimmer" : undefined}>
        {t(
          isCompacting
            ? "conversation.notice.compacting"
            : "conversation.notice.contextSummary"
        )}
      </span>
    </ItemNotice>
  )
}
