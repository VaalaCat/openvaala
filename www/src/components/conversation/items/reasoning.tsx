import { Brain } from "lucide"
import { useTranslation } from "react-i18next"
import { ItemNotice, ItemStatus } from "@/components/conversation/item-notice"
import { MarkdownView } from "@/components/conversation/markdown-view"
import type { ItemRendererProps } from "@/lib/item-renderers"

// 思考文本优先使用摘要，展开后显示原有 Markdown。
export function ReasoningItem({ view }: ItemRendererProps) {
  // 1. 读取文本和真实生成状态。
  const { t } = useTranslation()
  const { summary = [], content = [] } = view.item.reasoning ?? {}
  const text = summary.join("").trim() || content.join("").trim()
  const interrupted = view.item.reasoning?.status === "interrupted"
  const thinking = !interrupted && view.isStreaming
  return (
    <ItemNotice
      icon={Brain}
      status={
        interrupted ? (
          <ItemStatus
            state="interrupted"
            label={t("conversation.reasoning.interrupted")}
          />
        ) : undefined
      }
      details={
        text ? (
          <MarkdownView
            text={text}
            isStreaming={view.isStreaming}
            viewStateKey={view.key}
          />
        ) : undefined
      }
    >
      <span className={thinking ? "shimmer" : undefined}>
        {t(
          thinking
            ? "conversation.reasoning.thinking"
            : interrupted
              ? "conversation.reasoning.title"
              : "conversation.reasoning.completed"
        )}
      </span>
    </ItemNotice>
  )
}
