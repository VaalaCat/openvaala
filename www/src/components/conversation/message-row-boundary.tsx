import { Component, type ReactNode } from "react"
import { TriangleAlert } from "lucide"
import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import {
  DetailSection,
  ItemNotice,
} from "@/components/conversation/item-notice"

interface MessageRowBoundaryProps {
  // 这一行显示的记录：Item（工具卡另带它的结果），或 Turn 记录
  record: unknown
  result?: unknown
  children: ReactNode
}

// 消息区一行的错误边界：这一行渲染出错时只把它换成「这条记录无法显示」和折叠的原始数据，
// 不影响其他行，也不冒到整页的错误页；记录变了（例如生成中又收到增量）再试着正常显示
export class MessageRowBoundary extends Component<
  MessageRowBoundaryProps,
  { error?: unknown }
> {
  state: { error?: unknown } = {}

  static getDerivedStateFromError(error: unknown) {
    return { error }
  }

  // 出错之后记录又变了：清掉错误重新渲染；出错的这一次更新里不清，避免来回重试
  componentDidUpdate(
    previousProps: MessageRowBoundaryProps,
    previousState: { error?: unknown }
  ) {
    const isRecordChanged =
      previousProps.record !== this.props.record ||
      previousProps.result !== this.props.result
    if (previousState.error !== undefined && isRecordChanged)
      this.setState({ error: undefined })
  }

  render() {
    if (this.state.error === undefined) return this.props.children
    const { record, result } = this.props
    return (
      <RenderFailedNotice
        error={this.state.error}
        value={result === undefined ? record : [record, result]}
      />
    )
  }
}

// 出错行的替代显示：一行说明，点开看出错原因和原始数据
function RenderFailedNotice({
  error,
  value,
}: {
  error: unknown
  value: unknown
}) {
  const { t } = useTranslation()
  return (
    <ItemNotice
      icon={TriangleAlert}
      tone="destructive"
      details={
        <div className="flex flex-col gap-2">
          <DetailSection label={t("common.errors.detail")}>
            <pre className="text-code-reading wrap-anywhere whitespace-pre-wrap color-destructive">
              {error instanceof Error ? error.message : String(error)}
            </pre>
          </DetailSection>
          <JsonView value={value} maxHeight="24rem" />
        </div>
      }
    >
      {t("conversation.item.renderFailed")}
    </ItemNotice>
  )
}
