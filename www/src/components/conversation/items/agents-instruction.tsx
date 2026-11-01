import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { FileText } from "lucide"
import { useTranslation } from "react-i18next"
import {
  DetailSection,
  ItemNotice,
} from "@/components/conversation/item-notice"
import { MarkdownView } from "@/components/conversation/markdown-view"
import type { ItemRendererProps } from "@/lib/item-renderers"

// agents_instruction：工作目录里的 AGENTS.md 读进了上下文，路径单行省略；点开看全文
export function AgentsInstructionItem({ view }: ItemRendererProps) {
  // 1. 标题保留动作，文件位置和正文放进展开区。
  const { t } = useTranslation()
  const instruction = view.item.agents_instruction
  if (!instruction) return null
  return (
    <ItemNotice
      icon={FileText}
      details={
        <div className="flex min-w-0 flex-col gap-2">
          <DetailSection label={t("conversation.notice.file")}>
            <OverflowTooltip render={<p className="truncate text-table" />}>
              {instruction.path}@{instruction.executor}
            </OverflowTooltip>
          </DetailSection>
          <MarkdownView text={instruction.text} />
        </div>
      }
    >
      {t("conversation.notice.agentsInstruction")}
    </ItemNotice>
  )
}
