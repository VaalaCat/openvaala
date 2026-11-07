import { MessageSquareText } from "lucide"
import { useTranslation } from "react-i18next"
import { ItemNotice } from "@/components/conversation/item-notice"
import { MarkdownView } from "@/components/conversation/markdown-view"
import type { ItemRendererProps } from "@/lib/item-renderers"

// client_instruction：客户端写进上下文的指令（例如旁支的分界说明），不是用户消息；折叠成一行，点开看全文
export function ClientInstructionItem({ view }: ItemRendererProps) {
  // 1. 标题只写来源，全文放进展开区。
  const { t } = useTranslation()
  const instruction = view.item.client_instruction
  if (!instruction) return null
  return (
    <ItemNotice
      icon={MessageSquareText}
      details={<MarkdownView text={instruction.text} />}
    >
      {t("conversation.notice.clientInstruction")}
    </ItemNotice>
  )
}
