import { useState, type ReactElement } from "react"
import { useMutation } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import {
  ArchiveIcon,
  ArchiveRestoreIcon,
  GitForkIcon,
  MoreHorizontalIcon,
  PencilIcon,
  Trash2Icon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { useLocation, useNavigate } from "react-router"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { RequestError } from "@/components/base/request-error"
import { RenameConversationDialog } from "@/components/conversation-list/rename-conversation-dialog"
import { useServerConnection } from "@/components/server-connection-provider"
import { useConversationWorkspaces } from "@/hooks/use-conversation-workspaces"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import type { Conversation } from "@/lib/api/protocol.gen"
import { notifyFailure, notifySuccess } from "@/lib/notify"
import {
  archiveConversationOptions,
  deleteConversationOptions,
  forkConversationOptions,
} from "@/lib/query/conversation"
import {
  getConversationListSearch,
  getConversationPath,
  getServerPath,
} from "@/lib/paths"

// 对话行的「⋯」菜单：改名、分叉、归档或取消归档、删除；改名和删除的对话框跟着菜单一起挂载
export function ConversationActions({
  conversation,
  name,
  trigger,
}: {
  conversation: Conversation
  // 对话的显示名，没有标题时是「新对话」
  name: string
  trigger: ReactElement
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const connected = useStore(connection.$state) === "connected"
  const navigate = useNavigate()
  const { search } = useLocation()
  const [dialog, setDialog] = useState<"rename" | "delete">()
  const fork = useMutation(forkConversationOptions(connection))
  const archive = useMutation(archiveConversationOptions(connection))
  const isArchived = !!conversation.archived_at
  async function handleFork() {
    try {
      const forked = await fork.mutateAsync({ conversation })
      notifySuccess(t("conversationList.fork.success", { name }))
      void navigate({
        pathname: getConversationPath(
          connection.getServerID(),
          forked.conversation.id
        ),
        search: getConversationListSearch(search),
      })
    } catch (error) {
      notifyFailure(error, t)
    }
  }
  async function handleArchive() {
    try {
      await archive.mutateAsync({
        conversationID: conversation.id,
        archived: !isArchived,
      })
      notifySuccess(
        t(
          isArchived
            ? "conversationList.unarchive.success"
            : "conversationList.archive.success",
          { name }
        )
      )
    } catch (error) {
      notifyFailure(error, t)
    }
  }
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={trigger}
          aria-label={t("conversationList.actions.more", { name })}
        >
          <MoreHorizontalIcon />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="min-w-40">
          <DropdownMenuGroup>
            <DropdownMenuItem
              className="text-ui"
              onClick={() => setDialog("rename")}
            >
              <PencilIcon />
              {t("common.actions.rename")}
            </DropdownMenuItem>
            <DropdownMenuItem
              className="text-ui"
              disabled={fork.isPending || !connected}
              onClick={() => void handleFork()}
            >
              <GitForkIcon />
              {t("conversationList.actions.fork")}
            </DropdownMenuItem>
            <DropdownMenuItem
              className="text-ui"
              disabled={archive.isPending || !connected}
              onClick={() => void handleArchive()}
            >
              {isArchived ? <ArchiveRestoreIcon /> : <ArchiveIcon />}
              {t(
                isArchived
                  ? "conversationList.actions.unarchive"
                  : "conversationList.actions.archive"
              )}
            </DropdownMenuItem>
          </DropdownMenuGroup>
          <DropdownMenuSeparator />
          <DropdownMenuGroup>
            <DropdownMenuItem
              className="text-ui"
              variant="destructive"
              disabled={!connected}
              onClick={() => setDialog("delete")}
            >
              <Trash2Icon />
              {t("conversationList.actions.delete")}
            </DropdownMenuItem>
          </DropdownMenuGroup>
        </DropdownMenuContent>
      </DropdownMenu>
      <RenameConversationDialog
        conversationID={conversation.id}
        title={conversation.title ?? ""}
        open={dialog === "rename"}
        onOpenChange={(open) => setDialog(open ? "rename" : undefined)}
        onRenamed={() => {
          setDialog(undefined)
          notifySuccess(t("conversationList.rename.success"))
        }}
      />
      <DeleteConversationDialog
        conversation={conversation}
        name={name}
        open={dialog === "delete"}
        onOpenChange={(open) => setDialog(open ? "delete" : undefined)}
      />
    </>
  )
}

// 删除对话的确认框；失败时留在框内显示原因。删除的正是当前打开的对话时，回到新对话页
function DeleteConversationDialog({
  conversation,
  name,
  open,
  onOpenChange,
}: {
  conversation: Conversation
  name: string
  open: boolean
  onOpenChange(open: boolean): void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const connected = useStore(connection.$state) === "connected"
  const navigate = useNavigate()
  const { search } = useLocation()
  const workspaces = useConversationWorkspaces()
  const remove = useMutation(deleteConversationOptions(connection))
  function handleOpenChange(next: boolean) {
    if (!next) remove.reset()
    onOpenChange(next)
  }
  function handleConfirm() {
    remove.mutate(conversation.id, {
      onSuccess: () => {
        handleOpenChange(false)
        notifySuccess(t("conversationList.delete.success", { name }))
        const isOpened =
          workspaces.$currentConversationID.get() === conversation.id
        workspaces.forget(conversation.id)
        if (isOpened)
          void navigate({
            pathname: getServerPath(connection.getServerID()),
            search: getConversationListSearch(search),
          })
      },
    })
  }
  return (
    <ConfigurationChangeDialog
      open={open}
      title={t("conversationList.delete.title", { name })}
      description={t("conversationList.delete.description")}
      preview={remove.isError && <RequestError error={remove.error} />}
      confirmLabel={t("conversationList.delete.confirm")}
      destructive
      pending={remove.isPending}
      confirmDisabled={!connected}
      onConfirm={handleConfirm}
      onOpenChange={handleOpenChange}
    />
  )
}
