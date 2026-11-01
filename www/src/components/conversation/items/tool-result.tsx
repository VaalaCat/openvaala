import { Wrench } from "lucide"
import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import { ImagePreview } from "@/components/base/image-preview"
import { ItemNotice, ItemStatus } from "@/components/conversation/item-notice"
import type { ItemRendererProps } from "@/lib/item-renderers"

// 调用还没加载的 tool_result（按页加载时调用落在更早的一页）：写一句说明，点开看整条结果；加载到调用后并进工具卡
export function ToolResultItem({ view }: ItemRendererProps) {
  const { t } = useTranslation()
  return (
    <ItemNotice
      icon={Wrench}
      tone={view.item.tool_result?.error ? "destructive" : undefined}
      status={
        view.item.tool_result?.error ? (
          <ItemStatus
            state="failed"
            label={t("conversation.tool.state.failed")}
          />
        ) : undefined
      }
      details={
        <div className="flex flex-col gap-2">
          <JsonView value={view.item} maxHeight="24rem" />
          <div className="flex flex-wrap gap-2">
            {view.item.tool_result?.images?.map(
              (image, index) =>
                image && (
                  <ImagePreview
                    key={index}
                    src={image.data_url}
                    alt={image.alt ?? ""}
                  />
                )
            )}
          </div>
        </div>
      }
    >
      {t("conversation.notice.toolResult")}
    </ItemNotice>
  )
}
