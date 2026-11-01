import { useState, type FormEvent } from "react"
import { useMutation } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { RequestError } from "@/components/base/request-error"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { LatticeLoader } from "@/components/base/lattice-loader"
import type { ConversationID } from "@/lib/api/protocol.gen"
import { renameConversationOptions } from "@/lib/query/conversation"

// 改名对话框；标题留空表示清除标题。对话列表的行菜单和对话页点顶栏标题共用
export function RenameConversationDialog({
  conversationID,
  title,
  open,
  onOpenChange,
  onRenamed,
}: {
  conversationID: ConversationID
  // 对话当前的标题，没有标题时为空
  title: string
  open: boolean
  onOpenChange(open: boolean): void
  onRenamed(): void
}) {
  const { t } = useTranslation()
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent showCloseButton={false} className="text-body">
        <DialogHeader>
          <DialogTitle className="text-section">
            {t("conversationList.rename.title")}
          </DialogTitle>
        </DialogHeader>
        <RenameConversationForm
          conversationID={conversationID}
          initialTitle={title}
          onCancel={() => onOpenChange(false)}
          onRenamed={onRenamed}
        />
      </DialogContent>
    </Dialog>
  )
}

// 对话框每次打开才挂载，输入框从对话当前标题开始
function RenameConversationForm({
  conversationID,
  initialTitle,
  onCancel,
  onRenamed,
}: {
  conversationID: ConversationID
  initialTitle: string
  onCancel(): void
  onRenamed(): void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const connected = useStore(connection.$state) === "connected"
  const rename = useMutation(renameConversationOptions(connection))
  const [title, setTitle] = useState(initialTitle)
  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    if (!connected || rename.isPending) return
    rename.mutate(
      { conversationID, title: title.trim() },
      { onSuccess: onRenamed }
    )
  }
  return (
    <form className="flex flex-col gap-4" onSubmit={handleSubmit}>
      <Input
        className="text-ui"
        aria-label={t("conversationList.rename.label")}
        value={title}
        autoFocus
        autoComplete="off"
        onChange={(event) => setTitle(event.target.value)}
      />
      {rename.isError && <RequestError error={rename.error} />}
      <DialogFooter>
        <Button
          className="text-ui"
          type="button"
          variant="outline"
          onClick={onCancel}
        >
          {t("common.actions.cancel")}
        </Button>
        <Button
          className="text-ui"
          type="submit"
          disabled={rename.isPending || !connected}
        >
          {rename.isPending && (
            <LatticeLoader
              data-icon="inline-start"
              aria-label={t("common.loading")}
            />
          )}
          {t("conversationList.rename.save")}
        </Button>
      </DialogFooter>
    </form>
  )
}
