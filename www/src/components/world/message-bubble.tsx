import { RequestError } from "@/components/base/request-error"
import { RpcError } from "@/lib/rpc-error"
import { NavigationButton } from "@/components/base/navigation-button"
import { useServerConnection } from "@/components/server-connection-provider"
import { getSettingsPath } from "@/lib/paths"
import type { ReactNode } from "react"
import { AlertCircleIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { CollapsibleMessageBody } from "@/components/base/collapsible-message-body"
import { CopyButton } from "@/components/base/copy-button"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { StatusDot } from "@/components/base/status-dot"
import { MarkdownView } from "@/components/conversation/markdown-view"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import type { WorldMessage, WorldMessageStatus } from "@/lib/api/world"
import { cn } from "@/lib/utils"
import { CancelWorldSend } from "./cancel-send"
import {
  getWorldErrorHint,
  type WorldErrorDescription,
  worldMessageStatusLabels,
} from "./message-bubble-helpers"

export function WorldSendError({ error }: { error: unknown }) {
  const hint =
    error instanceof RpcError ? getWorldErrorHint(error.data?.code) : undefined
  return hint ? <WorldErrorHint hint={hint} /> : <RequestError error={error} />
}

export function WorldErrorHint({
  hint,
  local = false,
}: {
  hint: WorldErrorDescription
  local?: boolean
}) {
  const { t } = useTranslation()
  const id = useServerConnection().getServerID()
  const action = hint.action === "models" && !local ? undefined : hint.action
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2 text-caption">
      <span>{t(hint.message)}</span>
      {action && (
        <NavigationButton
          className="text-link color-subtle"
          to={
            action === "models"
              ? getSettingsPath(id, "providers")
              : getSettingsPath(id, "world-admin") +
                (action === "node" ? "#world-node" : "?tab=admin&view=domains")
          }
        >
          {t(
            action === "models"
              ? "settings.sections.providers"
              : action === "node"
                ? "worldSettings.node"
                : "worldAdmin.domains"
          )}
        </NavigationButton>
      )}
    </div>
  )
}

export function WorldMessageBubble({
  side,
  subject,
  body,
  status,
  error,
  errorCode,
  cancelMessage,
  conversationLink,
  viewStateKey,
}: {
  side: "self" | "peer"
  subject?: string
  body: string
  status?: WorldMessageStatus
  error?: string
  errorCode?: string
  cancelMessage?: WorldMessage
  conversationLink?: ReactNode
  viewStateKey: string
}) {
  const { t } = useTranslation()
  const self = side === "self"
  const visibleStatus = status !== "replied" && status !== "completed" && status
  const working =
    status === "sending" ||
    status === "remote_running" ||
    status === "accepted" ||
    status === "running"
  const warning = status === "pending_offline" || status === "unknown"
  const failed = status === "failed"
  const hint = getWorldErrorHint(errorCode)

  return (
    <div data-anchor-key={viewStateKey} className="flex min-w-0 flex-col gap-1">
      <div
        className={cn(
          "flex w-fit max-w-[85%] min-w-0 flex-col px-3 py-2",
          self ? "ml-auto bg-primary/10 dark:bg-primary/15" : "mr-auto bg-muted"
        )}
      >
        {subject && (
          <p className="mb-1 text-section wrap-anywhere">{subject}</p>
        )}
        <CollapsibleMessageBody as="div" viewStateKey={viewStateKey}>
          <MarkdownView text={body} viewStateKey={viewStateKey} />
        </CollapsibleMessageBody>
      </div>
      {(body || visibleStatus || conversationLink || cancelMessage) && (
        <div
          className={cn(
            "flex min-w-0 flex-wrap items-center gap-1 text-caption empty:hidden",
            self ? "justify-end" : "justify-start"
          )}
        >
          {visibleStatus && !(failed && hint) && (
            <span
              className={cn(
                "inline-flex items-center gap-1",
                failed ? "color-destructive" : "color-muted"
              )}
            >
              {working && <LatticeLoader aria-label={t("common.loading")} />}
              {warning && (
                <StatusDot
                  tone="warning"
                  label={t(worldMessageStatusLabels[status])}
                  className="shrink-0"
                />
              )}
              {failed && (
                <AlertCircleIcon className="size-3.5 shrink-0 color-destructive" />
              )}
              {t(worldMessageStatusLabels[visibleStatus])}
            </span>
          )}
          {body && (
            <CopyButton
              value={body}
              label={t("conversation.message.copy")}
              size="icon"
              className="color-muted"
            />
          )}
          {cancelMessage && <CancelWorldSend message={cancelMessage} />}
          {conversationLink}
          {hint && <WorldErrorHint hint={hint} local={!self} />}
          {error && (failed || warning) && (
            <details className="max-w-full">
              <summary
                className={cn(
                  "cursor-pointer",
                  errorCode === "world_agent_no_model"
                    ? "text-link color-subtle"
                    : "color-destructive"
                )}
              >
                {t("common.errors.detail")}
              </summary>
              <div className="mt-1 min-w-0">
                <MarkdownView text={error} />
              </div>
            </details>
          )}
        </div>
      )}
    </div>
  )
}

export function WorldMessageTime({ ms }: { ms: number }) {
  const { i18n } = useTranslation()
  const date = new Date(ms)
  const today = date.toDateString() === new Date().toDateString()
  const time = date.toLocaleTimeString(i18n.language, {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  })

  return (
    <Tooltip>
      <TooltipTrigger
        render={<time dateTime={date.toISOString()} tabIndex={0} />}
        className="text-caption color-muted"
      >
        {today ? time : `${date.toLocaleDateString(i18n.language)} ${time}`}
      </TooltipTrigger>
      <TooltipContent className="text-caption">
        {date.toLocaleString(i18n.language)}
      </TooltipContent>
    </Tooltip>
  )
}
