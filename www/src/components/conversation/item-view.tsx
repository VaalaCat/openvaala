import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { createElement, memo, useState } from "react"
import { RotateCcwIcon } from "lucide-react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"
import { CopyButton } from "@/components/base/copy-button"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { getContentText } from "@/lib/api/conversation-items"
import { cn } from "@/lib/utils"
import { setDraft } from "@/lib/drafts"
import {
  getItemRenderer,
  getMessageSource,
  type ItemRendererProps,
} from "@/lib/item-renderers"
import { notifyFailure, notifySuccess } from "@/lib/notify"
import { getConversationPath } from "@/lib/paths"
import {
  conversationOptions,
  forkConversationOptions,
} from "@/lib/query/conversation"

// 一条 Item 按 kind 显示；用户正文和会话最后的已结束回复可复制，主 Session 用户消息可重来。
export const ItemView = memo(function ItemView({
  view,
  toolResult,
  isProgressMessage,
  sync,
  isSubSession,
  copyReply = false,
}: ItemRendererProps & { isSubSession: boolean; copyReply?: boolean }) {
  const { t } = useTranslation()
  // 1. 复制用户自己输入的正文；只有主 Session 已保存的用户消息可重来。
  const isUserMessage =
    view.item.kind === "message" && getMessageSource(view.item) === "user"
  const copyValue =
    isUserMessage || copyReply
      ? getContentText(
          (view.item.message?.content ?? []).flatMap((each) =>
            each && (!isUserMessage || !each.text?.is_context) ? [each] : []
          )
        )
      : ""
  const canRewind = isUserMessage && !isSubSession && view.ordinal !== undefined
  return (
    <div className="flex min-w-0 flex-col gap-2">
      {createElement(getItemRenderer(view.item.kind), {
        view,
        toolResult,
        isProgressMessage,
        sync,
      })}
      {(copyValue || canRewind) && (
        <div className={cn("flex gap-2", isUserMessage && "justify-end")}>
          {copyValue && (
            <CopyButton
              value={copyValue}
              label={t(
                isUserMessage
                  ? "conversation.message.copy"
                  : "conversation.turn.copyResult"
              )}
              size="icon"
              className="color-muted"
            />
          )}
          {canRewind && <RewindButton view={view} sync={sync} />}
        </div>
      )}
    </div>
  )
})

// 「从这里重来」：分叉出一个只有这条输入之前各轮的新对话，跳过去，并把这条输入的文字放进新对话的输入框
function RewindButton({
  view,
  sync,
}: Pick<ItemRendererProps, "view" | "sync">) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const fork = useMutation(forkConversationOptions(connection))
  const [isRewinding, setIsRewinding] = useState(false)
  async function handleRewind() {
    setIsRewinding(true)
    try {
      // 1. 分叉要带上来源对话的 Executor 设置，先读对话记录。
      const { conversation } = await queryClient.fetchQuery(
        conversationOptions(connection, sync.conversationID)
      )
      const forked = await fork.mutateAsync({
        conversation,
        beforeTurnID: view.turnID,
      })
      // 2. 输入的文字写进新对话的草稿，再跳过去。
      const contents = (view.item.message?.content ?? []).flatMap((each) =>
        each ? [each] : []
      )
      setDraft(sync.serverID, forked.conversation.id, { content: contents })
      notifySuccess(t("conversation.rewind.success"))
      void navigate({
        pathname: getConversationPath(
          connection.getServerID(),
          forked.conversation.id
        ),
      })
    } catch (error) {
      notifyFailure(error, t)
    } finally {
      setIsRewinding(false)
    }
  }
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            className="color-muted"
            disabled={isRewinding}
            aria-label={t("conversation.rewind.action")}
            onClick={() => void handleRewind()}
            focusableWhenDisabled
          />
        }
      >
        <RotateCcwIcon />
      </TooltipTrigger>
      <TooltipContent className="text-caption">
        {t("conversation.rewind.action")}
      </TooltipContent>
    </Tooltip>
  )
}
