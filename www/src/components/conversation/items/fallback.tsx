import { Braces } from "lucide"
import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import { ItemNotice } from "@/components/conversation/item-notice"
import type { ItemRendererProps } from "@/lib/item-renderers"

// 表里没有的 kind（serve 新增的种类）：写出种类，点开看整条 Item 的 JsonView
export function FallbackItem({ view }: ItemRendererProps) {
  const { t } = useTranslation()
  return (
    <ItemNotice
      icon={Braces}
      details={
        <JsonView
          value={view.item}
          maxHeight="24rem"
          viewStateKey={view.key + ":item"}
        />
      }
    >
      {t("conversation.notice.unknownItem", { kind: view.item.kind })}
    </ItemNotice>
  )
}
