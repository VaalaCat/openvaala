import { useState } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { XIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { RequestError } from "@/components/base/request-error"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import type { WorldMessage } from "@/lib/api/world"
import { useNotifyIfUnmounted } from "@/lib/notify"
import { invalidateWorldQueries } from "@/lib/query/invalidation"

export function CancelWorldSend({
  message,
  allowed = true,
}: {
  message: Pick<WorldMessage, "message_id" | "peer_addr" | "status">
  allowed?: boolean
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const cancel = useMutation({
    mutationFn: () =>
      connection.call("world/cancelMessage", {
        message_id: message.message_id,
      }),
    onSettled: () =>
      invalidateWorldQueries(queryClient, connection, "message_updated"),
    onError: useNotifyIfUnmounted(t),
  })
  const cancellable = allowed && message.status === "pending_offline"

  return (
    <>
      {cancellable && (
        <Tooltip>
          <TooltipTrigger
            delay={600}
            closeOnClick
            render={
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="color-muted"
                aria-label={t("world.cancelSend")}
                onClick={() => setOpen(true)}
              />
            }
          >
            <XIcon aria-hidden />
          </TooltipTrigger>
          <TooltipContent className="text-caption">
            {t("world.cancelSend")}
          </TooltipContent>
        </Tooltip>
      )}
      {open && (
        <ConfigurationChangeDialog
          open
          title={t("world.cancelTitle", { name: message.peer_addr })}
          confirmLabel={t("world.cancelSend")}
          confirmDisabled={!cancellable}
          pending={cancel.isPending}
          destructive
          preview={
            cancel.isError ? <RequestError error={cancel.error} /> : undefined
          }
          onConfirm={() =>
            cancel.mutate(undefined, { onSuccess: () => setOpen(false) })
          }
          onOpenChange={(nextOpen) => {
            if (nextOpen) return
            setOpen(false)
            cancel.reset()
          }}
        />
      )}
    </>
  )
}
