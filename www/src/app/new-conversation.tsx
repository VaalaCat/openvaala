import { useMemo, useRef, useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useLocation, useNavigate, useNavigationType } from "react-router"
import { RequestError } from "@/components/base/request-error"
import { ImportSessionDialog } from "@/components/conversation/import-session-dialog"
import { agentCatalogOptions } from "@/lib/query/agent-settings"
import { modelListOptions } from "@/lib/query/model"
import { uploadAttachment, uploadDraftFiles } from "@/lib/attachments"
import { Composer } from "@/components/conversation/composer"
import { SessionControls } from "@/components/conversation/session-controls"
import { WorkLocationFields } from "@/components/conversation/work-location-fields"
import { useServerConnection } from "@/components/server-connection-provider"
import { usePageNavigation } from "@/hooks/use-page-navigation"
import { useWorkLocation } from "@/hooks/use-work-location"
import type {
  Content,
  Conversation,
  ExternalOptionSelection,
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
  recentConversationsOptions,
  submitFirstInputOptions,
} from "@/lib/query/conversation"
import { executorListOptions } from "@/lib/query/executor"
import {
  getWorkLocationSettings,
  type NewConversationLocationState,
} from "@/lib/work-location"

// 新对话页：输入框上方选模型和推理强度、Executor、工作目录（预填的和点选「最近用过」的目录视为已解析，手填的在发送时用 executor/resolveDirectory 校验并换成绝对路径）；
// 工作位置预填：侧栏带来的正在看的对话，否则是最近更新的对话，再否则是 Executor 的默认根目录；
// 首次发送时创建对话、上传草稿文件，提交输入后跳到对话。草稿每个服务器一份
export function NewConversationPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const navigate = useNavigate()
  const routeLocation = useLocation()
  const navigationType = useNavigationType()
  usePageNavigation(t("conversationList.newConversation"))
  const executors = useQuery(executorListOptions(connection))
  const createConversation = useMutation(createConversationOptions(connection))
  const submitFirstInput = useMutation(submitFirstInputOptions(connection))
  const [chosenModel, setModel] = useState<ModelSelection>()
  const recentConversations = useQuery(recentConversationsOptions(connection))
  const executorList = executors.data?.executors ?? []
  // 预填的工作位置：从侧栏点进来时用它带来的，刷新和前进后退时用最近更新的对话；
  // 最近对话是弱依赖，读不到时退回 Executor 的默认根目录
  const [routePreset] = useState(() =>
    navigationType === "PUSH"
      ? (routeLocation.state as NewConversationLocationState | null)?.preset
      : undefined
  )
  const latestConversation =
    recentConversations.data?.conversations?.[0]?.conversation
  const preset =
    routePreset ??
    (latestConversation
      ? getWorkLocationSettings(latestConversation)
      : undefined)
  const location = useWorkLocation(executorList, preset)
  const { chosenExecutorID, executorID, resolvedDirectory } = location
  // 预填了 Executor 就记为默认，否则只在用户选过时记
  const defaultExecutorID = chosenExecutorID ?? preset?.default_executor_id
  const executorSettings = useMemo(
    () => getExecutorSettings(defaultExecutorID, executorID, resolvedDirectory),
    [defaultExecutorID, executorID, resolvedDirectory]
  )

  const client = useQueryClient()
  const scope = {
    executor_id: executorID,
    cwd: resolvedDirectory ?? (location.directory.trim() || undefined),
  }
  const agents = useQuery(agentCatalogOptions(connection))
  const modelCatalog = useQuery(
    modelListOptions(connection, newConversationAgentName, scope)
  )
  const agent = agents.data?.agents?.find(
    (a) =>
      a.name === (chosenModel?.ref.provider_id ?? newConversationAgentName) &&
      a.type !== "native"
  )
  // 外部 Agent 的 default 入口跟随后端当前模型，探测后目录可能只包含具体模型。
  const defaultEntry =
    !chosenModel &&
    agent &&
    modelCatalog.data?.models.find((m) => m?.ref.provider_id === agent.name)
  const model =
    chosenModel ??
    (defaultEntry
      ? {
          ref: {
            provider_id: defaultEntry.ref.provider_id,
            model_id: "default",
          },
        }
      : undefined)
  const [choices, setChoices] = useState<ExternalOptionSelection[]>([])
  const selections =
    model && model.ref.model_id !== "default"
      ? [{ kind: "model", value: model.ref.model_id }, ...choices]
      : choices
  const optionsKey = [
    connection.getServerID(),
    "agentOptions",
    agent?.name,
    scope,
    selections,
  ]
  async function fetchOptions(refresh = false) {
    const options = await connection.call("agent/getOptions", {
      agent_name: agent!.name,
      ...scope,
      selections,
      refresh,
    })
    await client.invalidateQueries({
      queryKey: modelListOptions(connection, newConversationAgentName, scope)
        .queryKey,
    })
    return options
  }
  const probe = useQuery({
    queryKey: optionsKey,
    enabled: !!agent,
    retry: false,
    placeholderData: (previous, query) =>
      query?.queryKey[2] === agent?.name ? previous : undefined,
    queryFn: () => fetchOptions(),
  })
  const initialOptions =
    agent && !probe.isError
      ? choices.filter(
          (choice) =>
            probe.isFetching ||
            [
              probe.data?.mode,
              probe.data?.thought,
              ...(probe.data?.others ?? []),
            ].some(
              (g) =>
                g &&
                g.id === choice.id &&
                g.kind === choice.kind &&
                g.options?.some((o) => o.value === choice.value)
            )
        )
      : undefined
  // 首次发送后的上传重试复用该对话；模型或工作位置变更后按新参数创建。
  const pendingConversation = useRef<
    { key: string; request: Promise<Conversation> } | undefined
  >(undefined)
  async function getConversation(workDirectory: string | undefined) {
    const params = {
      agent_name: newConversationAgentName,
      model,
      initial_options: initialOptions,
      executor_settings: getExecutorSettings(
        defaultExecutorID,
        executorID,
        workDirectory
      ),
    }
    const key = JSON.stringify(params)
    if (pendingConversation.current?.key === key)
      return pendingConversation.current.request
    const request = createConversation
      .mutateAsync(params)
      .then((result) => result.conversation)
    pendingConversation.current = { key, request }
    try {
      return await request
    } catch (error) {
      if (pendingConversation.current?.request === request)
        pendingConversation.current = undefined
      throw error
    }
  }

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
    const conversation = await getConversation(workDirectory)
    const outgoing = await uploadDraftFiles(content, conversation.id, (file) =>
      uploadAttachment(connection, conversation.id, executorID!, file)
    )
    // 2. 提交第一条输入；失败时保留正文和目标对话的附件。
    let submitted = false
    try {
      await submitFirstInput.mutateAsync({
        conversation_id: conversation.id,
        mode: "normal",
        content: outgoing,
      })
      submitted = true
    } catch (error) {
      notifyFailure(error, t)
    }
    // 3. 迁移完整剩余草稿，再跳到新对话。
    const draft = getDraft(connection.getServerID(), newConversationDraftID)
    const sentItems = new Map(
      content.map((item, index) => [item, outgoing[index]])
    )
    const nextDraft = submitted
      ? removeSentContent(draft, content)
      : { content: draft.content.map((item) => sentItems.get(item) ?? item) }
    setDraft(connection.getServerID(), conversation.id, nextDraft)
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
          {/* 没有侧栏带来的位置时，等最近对话读完（成功或失败）再显示工作位置，不先显示默认值再跳成预填值 */}
          {(routePreset || recentConversations.isFetched) && (
            <div className="flex items-start gap-2">
              <div className="min-w-0 flex-1">
                <WorkLocationFields
                  executors={executorList}
                  location={location}
                />
              </div>
              {agent && (
                <ImportSessionDialog
                  key={agent.name}
                  agent={agent}
                  executorID={executorID}
                  resolveDirectory={location.resolveDirectory}
                />
              )}
            </div>
          )}
          <Composer
            serverID={connection.getServerID()}
            externalAgent={agent}
            onCreateAndSubmit={handleCreateAndSubmit}
            sessionControls={
              <SessionControls
                agentName={newConversationAgentName}
                value={model}
                scope={scope}
                onChange={(next) => {
                  if (
                    next.ref.provider_id !== model?.ref.provider_id ||
                    next.ref.model_id !== model.ref.model_id
                  )
                    setChoices([])
                  setModel(next)
                }}
                externalOptions={agent ? probe.data : undefined}
                onRefreshOptions={
                  agent
                    ? () => {
                        void client
                          .fetchQuery({
                            queryKey: optionsKey,
                            queryFn: () => fetchOptions(true),
                          })
                          .catch(() => undefined)
                      }
                    : undefined
                }
                onOption={(next) =>
                  setChoices((old) => [
                    ...old.filter(
                      (o) => o.id !== next.id || o.kind !== next.kind
                    ),
                    next,
                  ])
                }
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

// 新对话的 Executor 设置：用户选过或预填了 Executor 时记为默认；有工作目录时记在这个 Executor 下，没有时用它的默认根目录
function getExecutorSettings(
  defaultExecutorID: string | undefined,
  executorID: string | undefined,
  workDirectory: string | undefined
): ExecutorSettings {
  return {
    default_executor_id: defaultExecutorID,
    workspace_directories:
      executorID && workDirectory ? { [executorID]: workDirectory } : undefined,
  }
}
