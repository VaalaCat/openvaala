import { CircleAlert } from "lucide"
import { useTranslation } from "react-i18next"
import { ItemNotice } from "@/components/conversation/item-notice"
import type { ItemRendererProps } from "@/lib/item-renderers"

// model_error：模型调用失败；标题翻译，错误码和 serve 给的原文照原样显示
export function ModelErrorItem({ view }: ItemRendererProps) {
  // 1. 标题显示失败，展开区保留业务错误码和原文。
  const { t } = useTranslation()
  const error = view.item.model_error
  return (
    <ItemNotice
      icon={CircleAlert}
      tone="destructive"
      details={
        <p className="text-reading wrap-anywhere whitespace-pre-wrap color-destructive">
          {error?.code ? error.code + ": " : ""}
          {error?.message}
        </p>
      }
    >
      <span className="color-destructive">
        {t("conversation.modelError.title")}
      </span>
    </ItemNotice>
  )
}
