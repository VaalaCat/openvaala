import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import { DetailSection } from "@/components/conversation/item-notice"
import { ToolCard } from "@/components/conversation/tool-call-view"
import type { ToolRendererProps } from "@/lib/item-renderers"
import { getFirstStringField } from "@/lib/tool-arguments"

// MCP 工具和没有专门显示的工具：展开后显示参数摘要、参数和结果的 JsonView。
export function DefaultTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { result } = props
  const { value } = props.arguments
  return (
    <ToolCard {...props} argumentSummary={getFirstStringField(value)}>
      {result && result.content !== undefined && result.content !== null && (
        <DetailSection label={t("conversation.tool.result")}>
          <JsonView
            value={result.content}
            viewStateKey={props.viewKey + ":result"}
          />
        </DetailSection>
      )}
    </ToolCard>
  )
}
