import { useQuery } from "@tanstack/react-query"
import { WorldErrorHint } from "@/components/world/message-bubble"
import { getWorldErrorHint } from "@/components/world/message-bubble-helpers"
import { NavigationButton } from "@/components/base/navigation-button"
import { useTranslation } from "react-i18next"
import { StatusDot } from "@/components/base/status-dot"
import { MarkdownView } from "@/components/conversation/markdown-view"
import { ToolCard, ToolMeta } from "@/components/conversation/tool-call-view"
import { useServerConnection } from "@/components/server-connection-provider"
import { CancelWorldSend } from "@/components/world/cancel-send"
import {
  WorldDeliveryStatus,
  getVisibleTargets,
} from "@/components/world/contact-list"
import type { ToolRendererProps } from "@/lib/item-renderers"
import { getWorldContactAddress, parseWorldAddress } from "@/lib/api/world"
import { getContactsPath } from "@/lib/paths"
import { useWorldStatus, worldMessagesOptions } from "@/lib/query/world"
import { getStringField } from "@/lib/tool-arguments"
import {
  readWorldToolMessages,
  readWorldToolPeers,
} from "@/lib/world-tool-results"

export function SendWorldMessageTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const worldStatus = useWorldStatus()
  const to = getStringField(props.arguments.value, "to")
  const request = getStringField(props.arguments.value, "message")
  const [snapshot] = readWorldToolMessages(props.result?.content)
  const canReadMessages = worldStatus?.enabled === true && !!snapshot?.messageID
  const messages = useQuery({
    ...worldMessagesOptions(connection, {
      direction: "outbound",
      conversationID: props.sync.conversationID,
    }),
    enabled: canReadMessages,
  })
  const live =
    canReadMessages && messages.isSuccess
      ? messages.data.find(
          (message) => message.message_id === snapshot.messageID
        )
      : undefined
  const status = live?.status ?? snapshot?.status
  const peer = worldStatus?.enabled
    ? live?.peer_addr ||
      (to &&
        getWorldContactAddress(
          to,
          parseWorldAddress(worldStatus.node_addr)?.domain
        ))
    : undefined
  const hint = getWorldErrorHint(live?.error_code ?? snapshot?.errorCode)

  return (
    <ToolCard
      {...props}
      argumentSummary={to}
      displayedArguments={["to", "message"]}
    >
      {request && <MarkdownView text={request} />}
      <ToolMeta items={[snapshot?.messageID]} />
      <WorldToolReply
        reply={live?.reply_body || snapshot?.reply}
        error={hint ? undefined : live?.error_text || snapshot?.error}
      />
      {(status || peer || live) && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-ui">
          {hint ? (
            <WorldErrorHint hint={hint} />
          ) : (
            status && <WorldDeliveryStatus status={status} />
          )}
          {peer && (
            <NavigationButton
              className="text-link"
              to={getContactsPath(connection.getServerID(), { peer })}
            >
              {t("world.viewInContacts")}
            </NavigationButton>
          )}
          {live && <CancelWorldSend message={live} />}
        </div>
      )}
    </ToolCard>
  )
}

export function ListWorldPeersTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const peers = readWorldToolPeers(props.result?.content)
  const count =
    props.result?.content != null
      ? t("world.peerCount", { count: peers.length })
      : undefined
  return (
    <ToolCard {...props} argumentSummary={count}>
      {peers.length > 0 && (
        <ul className="min-w-0 divide-y">
          {peers.map((peer) => (
            <li
              key={peer.address}
              className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 py-2 text-ui"
            >
              <StatusDot
                tone={peer.online ? "success" : "warning"}
                label={t(peer.online ? "world.online" : "world.offline")}
              />
              {peer.node_name && (
                <span className="wrap-anywhere">{peer.node_name}</span>
              )}
              <span className="wrap-anywhere color-subtle">{peer.address}</span>
              <ToolMeta
                items={getVisibleTargets(peer).map((target) => target.name)}
              />
            </li>
          ))}
        </ul>
      )}
    </ToolCard>
  )
}

export function GetWorldMessageTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const messages = readWorldToolMessages(props.result?.content)
  const { value } = props.arguments
  const id =
    getStringField(value, "message_id") || getStringField(value, "thread_id")
  const statuses = [
    ...new Set(
      messages
        .filter((message) => !getWorldErrorHint(message.errorCode))
        .map((message) => message.status)
        .filter(Boolean)
    ),
  ]
  return (
    <ToolCard
      {...props}
      argumentSummary={
        (id || statuses.length > 0) && (
          <span className="inline-flex max-w-full gap-2">
            {id && <span className="min-w-0 truncate">{id}</span>}
            {statuses.map(
              (status) =>
                status && <WorldDeliveryStatus key={status} status={status} />
            )}
          </span>
        )
      }
    >
      {messages.length ? (
        <ul className="min-w-0 divide-y">
          {messages.map((message) => {
            const hint = getWorldErrorHint(message.errorCode)
            return (
              <li
                key={message.messageID}
                className="flex min-w-0 flex-col gap-2 py-2"
              >
                <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2">
                  <ToolMeta items={[message.messageID]} />
                  {hint ? (
                    <WorldErrorHint hint={hint} />
                  ) : (
                    message.status && (
                      <WorldDeliveryStatus status={message.status} />
                    )
                  )}
                </div>
                <WorldToolReply
                  reply={message.reply}
                  error={hint ? undefined : message.error}
                />
              </li>
            )
          })}
        </ul>
      ) : (
        props.result?.content != null && (
          <p className="text-ui color-subtle">{t("world.emptyMessages")}</p>
        )
      )}
    </ToolCard>
  )
}

function WorldToolReply({ reply, error }: { reply?: string; error?: string }) {
  const { t } = useTranslation()
  return (
    <>
      {reply && <MarkdownView text={reply} />}
      {error && (
        <details>
          <summary className="cursor-pointer text-ui color-destructive">
            {t("common.errors.detail")}
          </summary>
          <div className="mt-2">
            <MarkdownView text={error} />
          </div>
        </details>
      )}
    </>
  )
}
