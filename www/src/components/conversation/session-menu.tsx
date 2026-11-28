import { useTranslation } from "react-i18next"
import { ArrowUpRight, RotateCw } from "lucide-react"
import { useMutation } from "@tanstack/react-query"
import type { ReactElement } from "react"
import { useServerConnection } from "@/components/server-connection-provider"
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuGroup,
  ContextMenuItem,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { Session } from "@/lib/api/protocol.gen"
import { notifyFailure } from "@/lib/notify"

export function SessionMenu({
  session,
  sync,
  trigger,
}: {
  session: Session
  sync: ConversationSync
  trigger: ReactElement
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const action = useMutation({
    mutationFn: async (detach: boolean) => {
      if (detach)
        await connection.call("session/detach", {
          conversation_id: sync.conversationID,
          session_id: session.id,
        })
      else await sync.reloadSession(session.id)
    },
    onError: (e) => notifyFailure(e, t),
    onSuccess: (_r, detach, _m, { client }) => {
      if (detach)
        void client.invalidateQueries({
          queryKey: [connection.getServerID(), "conversations"],
        })
    },
  })
  if (!session.external || session.detached_conversation_id) return trigger

  return (
    <ContextMenu>
      <ContextMenuTrigger render={trigger} />
      <ContextMenuContent
        onClick={(event) => event.stopPropagation()}
        onKeyDown={(event) => event.stopPropagation()}
      >
        <ContextMenuGroup>
          <ContextMenuItem
            className="text-ui"
            disabled={action.isPending}
            onClick={() => action.mutate(false)}
          >
            <RotateCw />
            {t("externalSession.reload")}
          </ContextMenuItem>
          {session.parent_session_id && (
            <ContextMenuItem
              className="text-ui"
              disabled={action.isPending}
              onClick={() => action.mutate(true)}
            >
              <ArrowUpRight />
              {t("externalSession.detach")}
            </ContextMenuItem>
          )}
        </ContextMenuGroup>
      </ContextMenuContent>
    </ContextMenu>
  )
}
