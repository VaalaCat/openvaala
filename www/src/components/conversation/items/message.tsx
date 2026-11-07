import { useLayoutEffect, useRef, useState, type ReactNode } from "react"
import { MessageSquare } from "lucide"
import { ImagePreview } from "@/components/base/image-preview"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import {
  DisclosureChevron,
  DisclosureTrigger,
  ItemHeader,
  ItemStatus,
} from "@/components/conversation/item-notice"
import { MarkdownView } from "@/components/conversation/markdown-view"
import { SessionLink } from "@/components/conversation/session-link"
import { Collapsible, CollapsibleContent } from "@/components/ui/collapsible"
import { getContentText } from "@/lib/api/conversation-items"
import type { Content, TurnStatus } from "@/lib/api/protocol.gen"
import {
  getMessageSource,
  useItemDisclosure,
  type ItemRendererProps,
} from "@/lib/item-renderers"
import { cn } from "@/lib/utils"

// 子 Agent 结束通知里 Turn 终态的颜色和文案
const subagentStatuses = {
  completed: {
    tone: undefined,
    labelKey: undefined,
  },
  failed: {
    tone: "destructive",
    labelKey: "conversation.message.subagent.failed",
  },
  interrupted: {
    tone: "warning",
    labelKey: "conversation.message.subagent.interrupted",
  },
  running: {
    tone: undefined,
    labelKey: "conversation.message.subagent.running",
  },
} as const satisfies Record<
  TurnStatus,
  { tone?: "warning" | "destructive"; labelKey?: ParseKeys }
>

// message：用户输入是靠右的气泡（原文，不按 Markdown 排版，附带材料折叠）；模型回复按 Markdown 显示，生成中逐字补齐；
// 其他 Agent 转来的消息写明来源，子 Agent 的结束通知带上它那一轮的终态
export function MessageItem({
  view,
  sync,
  isProgressMessage = false,
}: ItemRendererProps) {
  // 1. 用户和模型正文直接显示，其他来源显示来信预览。
  const { t } = useTranslation()
  const disclosure = useItemDisclosure()
  const message = view.item.message
  if (!message) return null
  // Go 的 nil 切片编码成 null
  const contents = (message.content ?? []).flatMap((each) =>
    each ? [each] : []
  )
  const source = getMessageSource(view.item)
  if (source === "user") return <UserMessage contents={contents} />
  const text = getContentText(contents)
  if (source === "reply")
    return (
      <Collapsible
        open={!isProgressMessage || (disclosure?.open ?? false)}
        onOpenChange={disclosure?.onOpenChange}
        data-fold-anchor={"summary:" + view.key}
      >
        <div
          hidden={!isProgressMessage}
          data-anchor-key={"summary:" + view.key}
        >
          <ItemHeader
            icon={MessageSquare}
            title={t("conversation.process.updates")}
            open={disclosure?.open}
            onOpenChange={disclosure?.onOpenChange}
          />
        </div>
        <CollapsibleContent
          keepMounted
          data-anchor-key={"item:" + view.key}
          className={cn(
            "min-w-0 [&[hidden]]:hidden",
            isProgressMessage && "process-disclosure-panel"
          )}
        >
          <div
            className={cn(
              "flex min-w-0 flex-col gap-2",
              isProgressMessage &&
                "process-disclosure-content ml-2 border-l py-2 pr-2 pl-6"
            )}
          >
            <MarkdownView text={text} isStreaming={view.isStreaming} />
            <MessageImages contents={contents} />
          </div>
        </CollapsibleContent>
      </Collapsible>
    )
  // 2. 其他 Agent 的来源、状态与展开入口放在公共标题。
  const status = message.subagent_notification?.status
  const senderID = message.from?.id
  return (
    <AgentMessageBody
      title={
        <span className="inline-flex max-w-full min-w-0 items-center gap-2">
          <span className="shrink-0">{t("conversation.message.from")}</span>
          {!senderID && (
            <span className="min-w-0 truncate">
              {message.agent_sender?.task_name}
            </span>
          )}
        </span>
      }
      source={
        senderID && (
          <SessionLink
            sync={sync}
            sessionID={senderID}
            className="agent-message-source color-subtle"
          />
        )
      }
      status={
        status && status !== "completed" ? (
          <ItemStatus
            state={status}
            label={t(subagentStatuses[status].labelKey)}
          />
        ) : undefined
      }
      tone={status ? subagentStatuses[status].tone : undefined}
      text={text}
      contents={contents}
      open={disclosure?.open ?? false}
      isChosen={disclosure?.isChosen ?? false}
      onOpenChange={disclosure?.onOpenChange}
    />
  )
}

// 来信收起时只排版四行纯文本，溢出时底部渐隐；展开后显示完整 Markdown 和图片。
function AgentMessageBody({
  title,
  source,
  status,
  tone,
  text,
  contents,
  open,
  isChosen,
  onOpenChange,
}: {
  title: ReactNode
  source?: ReactNode
  status?: ReactNode
  tone?: "warning" | "destructive"
  text: string
  contents: Content[]
  open: boolean
  isChosen: boolean
  onOpenChange?: (open: boolean) => void
}) {
  const previewRef = useRef<HTMLParagraphElement>(null)
  const [isLong, setIsLong] = useState(false)
  const hasImages = contents.some((content) => content.image)
  // 1. 依据实际四行预览的高度判断是否需要渐隐和展开入口。
  useLayoutEffect(() => {
    const preview = previewRef.current
    if (!preview) return
    const measure = () => {
      if (preview.clientHeight > 0)
        setIsLong(preview.scrollHeight > preview.clientHeight + 1)
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(preview)
    return () => observer.disconnect()
  }, [text])
  return (
    <Collapsible open={open} onOpenChange={onOpenChange}>
      <ItemHeader
        icon={MessageSquare}
        title={title}
        actions={source}
        status={status}
        tone={tone}
        open={open}
        onOpenChange={isLong || hasImages || open ? onOpenChange : undefined}
      />
      <div
        hidden={!text && !open}
        className="ml-2 min-w-0 border-l py-2 pr-2 pl-6"
      >
        <p
          ref={previewRef}
          className={cn(
            "max-h-[4lh] overflow-hidden text-reading wrap-anywhere whitespace-pre-wrap color-muted",
            !open && isLong && "agent-message-preview-fade"
          )}
          hidden={open}
          aria-hidden={open || undefined}
        >
          {text}
        </p>
        <CollapsibleContent keepMounted className="process-disclosure-panel">
          {isChosen && (
            <div className="process-disclosure-content flex flex-col gap-2">
              <MarkdownView text={text} />
              <MessageImages contents={contents} />
            </div>
          )}
        </CollapsibleContent>
      </div>
    </Collapsible>
  )
}

// 用户输入：文字和图片在靠右的气泡里；附带材料（is_context）默认收起
function UserMessage({ contents }: { contents: Content[] }) {
  const { t } = useTranslation()
  const disclosure = useItemDisclosure()
  const typed = contents.filter((each) => !each.text?.is_context)
  const context = contents.filter((each) => each.text?.is_context)
  const typedText = getContentText(typed)
  return (
    <div className="ml-auto flex max-w-[85%] min-w-0 flex-col gap-2 bg-muted px-3 py-2">
      {typedText && (
        <p className="text-reading wrap-anywhere whitespace-pre-wrap">
          {typedText}
        </p>
      )}
      <MessageImages contents={typed} />
      {context.length > 0 && (
        <Collapsible
          open={disclosure?.open ?? false}
          onOpenChange={disclosure?.onOpenChange}
        >
          <DisclosureTrigger className="gap-1">
            <DisclosureChevron />
            {t("conversation.message.context")}
          </DisclosureTrigger>
          <CollapsibleContent
            keepMounted
            className="process-disclosure-panel"
            inert={!disclosure?.open}
            aria-hidden={!disclosure?.open || undefined}
          >
            <div className="process-disclosure-content pt-1">
              <pre className="max-h-60 overflow-auto text-code-reading whitespace-pre-wrap">
                {getContentText(context)}
              </pre>
            </div>
          </CollapsibleContent>
        </Collapsible>
      )}
    </div>
  )
}

// 消息里的图片，缩略图排成一行
function MessageImages({ contents }: { contents: Content[] }) {
  const images = contents.flatMap((each) => (each.image ? [each.image] : []))
  if (images.length === 0) return null
  return (
    <div className="flex flex-wrap gap-2">
      {images.map((image, index) => (
        <ImagePreview
          key={index}
          src={image.data_url}
          alt={image.alt ?? ""}
          className="max-h-40 max-w-full border object-contain"
        />
      ))}
    </div>
  )
}
