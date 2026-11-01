import { useMemo, useState } from "react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"
import { RequestError } from "@/components/base/request-error"
import { Composer } from "@/components/conversation/composer"
import { SessionControls } from "@/components/conversation/session-controls"
import { WorkLocationFields } from "@/components/conversation/work-location-fields"
import { useServerConnection } from "@/components/server-connection-provider"
import { usePageNavigation } from "@/hooks/use-page-navigation"
import { useWorkLocation } from "@/hooks/use-work-location"
import type {
  Content,
  ExecutorSettings,
  ModelSelection,
} from "@/lib/api/protocol.gen"
import {
  getDraft,
  newConversationDraftID,
  removeSentContent,
  setDraft,
} from "@/lib/drafts"
import { shouldShowLoadError } from "@/lib/rpc-error"
import { notifyFailure } from "@/lib/notify"
import { getConversationPath } from "@/lib/paths"
import {
  createConversationOptions,
  newConversationAgentName,
  submitFirstInputOptions,
} from "@/lib/query/conversation"
import { executorListOptions } from "@/lib/query/executor"

// 新对话页：输入框上方选模型和推理强度、Executor、工作目录（失焦时用 executor/resolveDirectory 校验，换成解析后的绝对路径）；
// 第一次发送时依次建对话、提交输入，再跳到新对话
export function NewConversationPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const navigate = useNavigate()
  usePageNavigation(t("conversationList.newConversation"))
  const executors = useQuery(executorListOptions(connection))
  const createConversation = useMutation(createConversationOptions(connection))
  const submitFirstInput = useMutation(submitFirstInputOptions(connection))
  const [model, setModel] = useState<ModelSelection>()
  const executorList = executors.data?.executors ?? []
  const location = useWorkLocation(executorList)
  const { chosenExecutorID, executorID, resolvedDirectory } = location
  const executorSettings = useMemo(
    () => getExecutorSettings(chosenExecutorID, executorID, resolvedDirectory),
    [chosenExecutorID, executorID, resolvedDirectory]
  )

  // 建对话并提交第一条输入：目录不可用时返回 false（原因显示在目录框旁），建对话失败时抛错，都留在这一页；
  // 建好后提交失败也跳过去，文字放进新对话的输入框
  async function handleCreateAndSubmit(content: Content[]): Promise<boolean> {
    // 1. 先确认工作目录，再按选好的模型和 Executor 设置建对话。
    let workDirectory: string | undefined
    try {
      workDirectory = await location.resolveDirectory()
    } catch {
      return false
    }
    const { conversation } = await createConversation.mutateAsync({
      agent_name: newConversationAgentName,
      model,
      executor_settings: getExecutorSettings(
        chosenExecutorID,
        executorID,
        workDirectory
      ),
    })
    // 2. 提交第一条输入；失败时把文字放进新对话的草稿，提示原因。
    let submitted = false
    try {
      await submitFirstInput.mutateAsync({
        conversation_id: conversation.id,
        mode: "normal",
        content,
      })
      submitted = true
    } catch (error) {
      notifyFailure(error, t)
    }
    // 3. 迁移完整剩余草稿，再跳到新对话。
    const draft = getDraft(connection.getServerID(), newConversationDraftID)
    setDraft(
      connection.getServerID(),
      conversation.id,
      submitted ? removeSentContent(draft, content) : draft
    )
    setDraft(connection.getServerID(), newConversationDraftID, { content: [] })
    void navigate(
      getConversationPath(connection.getServerID(), conversation.id)
    )
    return true
  }

  return (
    // 页面占满内容区域，输入框贴着底部。
    <div className="flex min-h-0 flex-1 flex-col">
      <h1 className="sr-only">{t("conversationList.newConversation")}</h1>
      <div className="mt-auto px-3 pb-3 md:px-6">
        <div className="mx-auto flex w-full max-w-3xl flex-col gap-2">
          {executors.isError && shouldShowLoadError(executors.error) && (
            <RequestError
              error={executors.error}
              onRetry={() => void executors.refetch()}
            />
          )}
          <div className="flex flex-wrap items-center gap-2">
            <WorkLocationFields executors={executorList} location={location} />
          </div>
          <Composer
            serverID={connection.getServerID()}
            onCreateAndSubmit={handleCreateAndSubmit}
            sessionControls={
              <SessionControls
                agentName={newConversationAgentName}
                value={model}
                onChange={setModel}
              />
            }
            skillTarget={{
              agentName: newConversationAgentName,
              executorSettings,
            }}
          />
        </div>
      </div>
    </div>
  )
}

// 新对话的 Executor 设置：用户选过 Executor 时记为默认；有工作目录时记在这个 Executor 下，没有时用它的默认根目录
function getExecutorSettings(
  chosenExecutorID: string | undefined,
  executorID: string | undefined,
  workDirectory: string | undefined
): ExecutorSettings {
  return {
    default_executor_id: chosenExecutorID,
    workspace_directories:
      executorID && workDirectory ? { [executorID]: workDirectory } : undefined,
  }
}
