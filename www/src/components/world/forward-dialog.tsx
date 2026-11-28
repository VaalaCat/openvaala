import { useId, useState } from "react"
import { useStore } from "@nanostores/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { WorldSendError } from "./message-bubble"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Textarea } from "@/components/ui/textarea"
import { WorldContactList } from "@/components/world/contact-list"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { ItemID } from "@/lib/api/protocol.gen"
import { notifySuccess, useNotifyIfUnmounted } from "@/lib/notify"
import { getContactsPath } from "@/lib/paths"
import { invalidateWorldQueries } from "@/lib/query/invalidation"
import { worldPeersOptions } from "@/lib/query/world"

export function ForwardDialog({
  sync,
  onClose,
}: {
  sync: ConversationSync
  onClose(): void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const queryClient = useQueryClient()
  const peers = useQuery(worldPeersOptions(connection))
  const navigate = useNavigate()
  const [recipient, setRecipient] = useState("")
  const [selected, setSelected] = useState<Set<ItemID>>(() => new Set())
  const [note, setNote] = useState("")
  const notesID = useId()
  const items = useStore(sync.$items)
  const messages = items.flatMap(({ ordinal, item }) => {
    if (ordinal === undefined || item.kind !== "message") return []
    const text = item.message?.content
      ?.map((part) => (part?.kind === "text" ? (part.text?.text ?? "") : ""))
      .join("")
    return text?.trim() ? [{ id: item.id, text }] : []
  })
  const selectedIDs = messages
    .filter(({ id }) => selected.has(id))
    .map(({ id }) => id)
  const onError = useNotifyIfUnmounted(t)
  const forward = useMutation({
    mutationFn: () =>
      connection.call("world/forward", {
        conversation_id: sync.conversationID,
        to: recipient,
        item_ids: selectedIDs,
        note: note.trim() || undefined,
      }),
    onSuccess: (message) => {
      invalidateWorldQueries(queryClient, connection, "message_updated")
      const path = getContactsPath(connection.getServerID(), {
        peer: message.peer_addr,
      })
      notifySuccess(t("world.forwarded", { address: message.peer_addr }), {
        label: t("world.viewInContacts"),
        onClick: () => void navigate(path),
      })
      onClose()
    },
    onError,
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !forward.isPending) onClose()
      }}
    >
      <DialogContent
        showCloseButton={false}
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto text-body sm:max-w-md"
      >
        <DialogHeader>
          <DialogTitle className="text-section">
            {t("world.forward")}
          </DialogTitle>
        </DialogHeader>
        <div className="max-h-48 min-w-0 overflow-y-auto">
          <WorldContactList
            peers={peers}
            selected={recipient}
            onSelect={setRecipient}
            disabled={forward.isPending}
            bordered
          />
        </div>
        <form
          className="flex min-w-0 flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (!recipient || forward.isPending) return
            forward.mutate()
          }}
        >
          {messages.length > 0 && (
            <details>
              <summary className="flex cursor-pointer items-center gap-2 text-ui">
                {t("world.selectMessages")}
                <span className="ml-auto text-right text-caption color-subtle">
                  {selectedIDs.length
                    ? t("world.selectedMessages", { count: selectedIDs.length })
                    : t("world.defaultForward")}
                </span>
              </summary>
              <div className="mt-2 flex max-h-32 min-w-0 flex-col gap-2 overflow-y-auto">
                {messages.map(({ id, text }) => (
                  <label
                    key={id}
                    className="flex min-w-0 items-start gap-2 text-ui"
                  >
                    <Checkbox
                      className="mt-1"
                      checked={selected.has(id)}
                      disabled={forward.isPending}
                      onCheckedChange={(checked) =>
                        setSelected((previous) => {
                          const next = new Set(previous)
                          if (checked) next.add(id)
                          else next.delete(id)
                          return next
                        })
                      }
                    />
                    <span className="line-clamp-2 min-w-0 wrap-anywhere">
                      {text}
                    </span>
                  </label>
                ))}
              </div>
            </details>
          )}
          <div className="flex flex-col gap-2">
            <label className="text-ui" htmlFor={notesID}>
              {t("world.note")}
            </label>
            <Textarea
              id={notesID}
              className="text-ui"
              value={note}
              onChange={(event) => setNote(event.target.value)}
              disabled={forward.isPending}
            />
          </div>
          {forward.isError && <WorldSendError error={forward.error} />}
          <DialogFooter className="flex-row justify-end">
            <Button
              type="button"
              variant="outline"
              className="text-ui"
              disabled={forward.isPending}
              onClick={onClose}
            >
              {t("common.actions.cancel")}
            </Button>
            <Button
              type="submit"
              className="text-ui"
              disabled={!recipient || forward.isPending}
            >
              {forward.isPending && (
                <LatticeLoader
                  data-icon="inline-start"
                  aria-label={t("common.loading")}
                />
              )}
              {t("world.forwardAction")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
