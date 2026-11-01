import { CircleSlash } from "lucide"
import { useTranslation } from "react-i18next"
import { ItemNotice } from "@/components/conversation/item-notice"

// turn_interrupted：这一轮被中断，写在这一轮最后
export function TurnInterruptedItem() {
  const { t } = useTranslation()
  return (
    <ItemNotice icon={CircleSlash} tone="warning">
      {t("conversation.notice.turnInterrupted")}
    </ItemNotice>
  )
}
