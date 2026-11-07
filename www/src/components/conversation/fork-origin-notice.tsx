import { GitFork } from "lucide"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"
import { ItemNotice } from "@/components/conversation/item-notice"
import { useServerConnection } from "@/components/server-connection-provider"
import type { ConversationID } from "@/lib/api/protocol.gen"
import { getConversationPath } from "@/lib/paths"
import { conversationOptions } from "@/lib/query/conversation"

// ForkOriginNoticeProps 决定提示的文案和点击动作：
// 旁支回到同一个工作区里的主对话标签页，普通分叉对话跳转到来源对话
export type ForkOriginNoticeProps = {
  // 来源对话，标题由提示自己读取
  sourceConversationID: ConversationID
} & ({ kind: "side-conversation"; onOpenSource(): void } | { kind: "fork" })

// 分叉来源提示：「从『来源对话』分叉」或「从『来源对话』分出的旁支」，和消息列里的 Item 同宽同边距。
// 来源标题读不到（加载中或已删除）时用「未命名」，已删除的分叉来源不再可点
export function ForkOriginNotice(props: ForkOriginNoticeProps) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const source = useQuery(
    conversationOptions(connection, props.sourceConversationID)
  )
  const title =
    source.data?.conversation.title || t("conversationList.untitled")
  const text =
    props.kind === "side-conversation"
      ? t("conversation.notice.sideConversationFrom", { title })
      : t("conversation.notice.forkedFrom", { title })
  return (
    <div data-fork-origin={props.kind}>
      <ItemNotice icon={GitFork}>
        {props.kind === "side-conversation" ? (
          <button
            type="button"
            className="max-w-full cursor-pointer truncate text-link"
            onClick={props.onOpenSource}
          >
            {text}
          </button>
        ) : source.isError ? (
          text
        ) : (
          <Link
            className="text-link"
            to={getConversationPath(
              connection.getServerID(),
              props.sourceConversationID
            )}
          >
            {text}
          </Link>
        )}
      </ItemNotice>
    </div>
  )
}
