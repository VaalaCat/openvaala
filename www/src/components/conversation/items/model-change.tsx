import { Replace } from "lucide"
import { useTranslation } from "react-i18next"
import { ItemNotice } from "@/components/conversation/item-notice"
import type { ItemRendererProps } from "@/lib/item-renderers"
import { formatModelRef } from "@/lib/utils"

// model_change：从这一条起换了模型
export function ModelChangeItem({ view }: ItemRendererProps) {
  const { t } = useTranslation()
  const change = view.item.model_change
  if (!change) return null
  return (
    <ItemNotice icon={Replace}>
      {t("conversation.notice.modelChange", {
        previous: formatModelRef(change.previous_model),
        model: formatModelRef(change.model),
      })}
    </ItemNotice>
  )
}
