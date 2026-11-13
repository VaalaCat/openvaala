import { ImagePreviewStrip } from "@/components/base/image-preview"
import { JsonView } from "@/components/base/json-view"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { OutputBlock } from "@/components/base/output-block"
import { createElement, type ComponentProps, type ReactNode } from "react"
import type { ParseKeys } from "i18next"
import {
  Bot,
  CircleSlash,
  Clock,
  FilePen,
  FileText,
  History,
  Send,
  Terminal,
  Wrench,
} from "lucide"
import { MorphIcon } from "morphicons/react"
import { useTranslation } from "react-i18next"
import { Collapsible, CollapsibleContent } from "@/components/ui/collapsible"
import {
  DetailSection,
  ItemHeader,
  ItemStatus,
} from "@/components/conversation/item-notice"
import {
  useItemDisclosure,
  getToolActionLabel,
  getToolRenderer,
  type ToolCallState,
  type ToolRendererProps,
} from "@/lib/item-renderers"
import { getStringField } from "@/lib/tool-arguments"

// 工具真实状态的颜色和文案。
const toolCallStates = {
  generating: {
    tone: undefined,
    labelKey: "conversation.tool.state.generating",
  },
  running: { tone: undefined, labelKey: "conversation.tool.state.running" },
  succeeded: { tone: undefined, labelKey: undefined },
  failed: { tone: "destructive", labelKey: "conversation.tool.state.failed" },
  interrupted: {
    tone: "warning",
    labelKey: "conversation.tool.state.interrupted",
  },
} as const satisfies Record<
  ToolCallState,
  { tone?: "warning" | "destructive"; labelKey?: ParseKeys }
>

// 工具卡：按工具名查 toolRenderers，交给对应工具的显示组件
export function ToolCallView(props: ToolRendererProps) {
  // 表里是模块级的组件，每次查到的是同一个，不会重建
  return createElement(getToolRenderer(props.call.name), props)
}

// 工具 Item 在标题显示参数摘要，展开后显示完整参数、结果和来源。
export function ToolCard({
  call,
  arguments: parsedArguments,
  result,
  state,
  viewKey,
  argumentSummary,
  resultSummary,
  displayedArguments = [],
  target,
  children,
}: ToolRendererProps & {
  argumentSummary?: ReactNode
  resultSummary?: ReactNode
  displayedArguments?: string[]
  target?: ReactNode
  children?: ReactNode
}) {
  // 1. 读取真实状态、工具身份和消息区保存的展开选择。
  const { t } = useTranslation()
  const disclosure = useItemDisclosure()
  const { tone, labelKey } = toolCallStates[state]
  const executor =
    result?.executor ?? getStringField(parsedArguments.value, "executor")
  const icons: Record<string, ComponentProps<typeof MorphIcon>["icon"]> = {
    bash: Terminal,
    read_command_output: Terminal,
    write_stdin: Terminal,
    stop_command: Terminal,
    list_commands: Terminal,
    read_file: FileText,
    apply_patch: FilePen,
    spawn_agent: Bot,
    send_message: Send,
    wait_agent: Clock,
    interrupt_agent: CircleSlash,
    list_agents: Bot,
    read_session_history: History,
  }
  // 2. 完整参数排除专用详情已经展示的字段，结果按专用内容或 JSON 显示。
  const value = parsedArguments.value
  const argumentDetails =
    value && typeof value === "object" && !Array.isArray(value)
      ? Object.fromEntries(
          Object.entries(value).filter(
            ([field]) => !displayedArguments.includes(field)
          )
        )
      : value
  const hasArguments =
    argumentDetails !== undefined &&
    argumentDetails !== null &&
    (typeof argumentDetails !== "object" ||
      Object.keys(argumentDetails).length > 0)
  const hasResult = result?.content !== undefined && result.content !== null
  const details = parsedArguments.ok ? (
    hasArguments || children || hasResult ? (
      <>
        {hasArguments && (
          <DetailSection label={t("conversation.tool.arguments")}>
            <JsonView
              value={argumentDetails}
              viewStateKey={viewKey + ":arguments"}
            />
          </DetailSection>
        )}
        {children ??
          (result?.content !== undefined && result.content !== null && (
            <DetailSection label={t("conversation.tool.result")}>
              <JsonView
                value={result.content}
                viewStateKey={viewKey + ":result"}
              />
            </DetailSection>
          ))}
      </>
    ) : undefined
  ) : parsedArguments.raw ? (
    <RawArguments
      text={parsedArguments.raw}
      viewStateKey={viewKey + ":arguments"}
    />
  ) : undefined
  const error = result?.error
  const images = (result?.images ?? []).flatMap((image) =>
    image ? [image] : []
  )
  const hasDetails =
    !!details ||
    !!resultSummary ||
    !!target ||
    !!executor ||
    !!error ||
    images.length > 0
  return (
    <Collapsible
      open={hasDetails && (disclosure?.open ?? false)}
      onOpenChange={disclosure?.onOpenChange}
      data-tool-state={state}
    >
      <ItemHeader
        icon={icons[call.name] ?? Wrench}
        title={getToolActionLabel(call.name, t)}
        summary={argumentSummary}
        status={
          state !== "succeeded" && labelKey ? (
            <ItemStatus state={state} label={t(labelKey)} />
          ) : undefined
        }
        tone={tone}
        open={disclosure?.open}
        onOpenChange={hasDetails ? disclosure?.onOpenChange : undefined}
      />
      <CollapsibleContent
        keepMounted
        className="process-disclosure-panel min-w-0"
      >
        {disclosure?.isChosen && (
          <div className="process-disclosure-content ml-2 flex min-w-0 flex-col gap-3 border-l py-2 pr-2 pl-6">
            {resultSummary && (
              <div className="min-w-0 text-ui color-muted">{resultSummary}</div>
            )}
            {(target || executor) && (
              <div className="flex min-w-0 items-center gap-2 text-table">
                {target && (
                  <span className="flex min-w-0 items-center gap-2">
                    {target}
                  </span>
                )}
                {executor && (
                  <OverflowTooltip className="min-w-0 truncate color-muted">
                    @{executor}
                  </OverflowTooltip>
                )}
              </div>
            )}
            {details}
            <ImagePreviewStrip
              images={images.map((image, index) => ({
                key: String(index),
                src: image.data_url,
                alt: image.alt,
              }))}
            />
            {error && (
              <p className="text-reading wrap-anywhere whitespace-pre-wrap color-destructive">
                {error.code}: {error.message}
              </p>
            )}
          </div>
        )}
      </CollapsibleContent>
    </Collapsible>
  )
}

// 参数原文：生成中按纯文本逐字显示，生成完还解析不了时就是模型写坏的原文
function RawArguments({
  text,
  viewStateKey,
}: {
  text: string
  viewStateKey: string
}) {
  const { t } = useTranslation()
  return (
    <DetailSection label={t("conversation.tool.arguments")}>
      <OutputBlock copyValue={text} viewStateKey={viewStateKey}>
        <pre className="code-padding text-code-reading">{text}</pre>
      </OutputBlock>
    </DetailSection>
  )
}

// serve 返回的枚举值（命令状态、唤醒原因等）：表里有的显示文案，serve 新增的取值原样显示
export function KnownValueLabel({
  labelKeys,
  value,
}: {
  labelKeys: Record<string, ParseKeys>
  value: string
}) {
  const { t } = useTranslation()
  return <>{Object.hasOwn(labelKeys, value) ? t(labelKeys[value]) : value}</>
}

// 工具详情里的一行附加信息（退出码、行号范围等），跳过空项。
export function ToolMeta({ items }: { items: ReactNode[] }) {
  const shown = items.filter(Boolean)
  if (shown.length === 0) return null
  return (
    <p className="flex flex-wrap gap-x-3 gap-y-1 text-caption color-muted">
      {shown.map((item, index) => (
        <span key={index}>{item}</span>
      ))}
    </p>
  )
}
