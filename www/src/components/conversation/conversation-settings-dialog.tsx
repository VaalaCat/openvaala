import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useState, type FormEvent } from "react"
import { useStore } from "@nanostores/react"
import type { ParseKeys } from "i18next"
import { RefreshCwIcon, SaveIcon, Settings2Icon } from "lucide-react"
import {
  useMutation,
  useQuery,
  type UseMutationResult,
} from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { ListEmpty } from "@/components/base/list-empty"
import { RequestError } from "@/components/base/request-error"
import { QueryState } from "@/components/base/query-state"
import { RowList, RowListItem } from "@/components/base/row-list"
import {
  ExecutorOptionDetails,
  WorkLocationFields,
} from "@/components/conversation/work-location-fields"
import { SkillList } from "@/components/settings/skill-list"
import { useServerConnection } from "@/components/server-connection-provider"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { FieldSet, FieldLegend } from "@/components/ui/field"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { isExecutorAvailable, useWorkLocation } from "@/hooks/use-work-location"
import type {
  ConnectionState as MCPConnectionState,
  ConversationID,
  ConversationResult,
  ExecutorDetails,
  ExecutorSettings,
} from "@/lib/api/protocol.gen"
import {
  conversationOptions,
  setExecutorSettingsOptions,
} from "@/lib/query/conversation"
import { executorListOptions } from "@/lib/query/executor"
import { mcpServerListOptions } from "@/lib/query/mcp"
import { skillListOptions } from "@/lib/query/skill"
import { shouldShowLoadError } from "@/lib/rpc-error"

// MCP 连接状态的颜色和文案
const mcpStates = {
  connected: {
    tone: "success",
    labelKey: "conversationSettings.mcp.connected",
  },
  connecting: {
    tone: "warning",
    labelKey: "conversationSettings.mcp.connecting",
  },
  failed: { tone: "destructive", labelKey: "conversationSettings.mcp.failed" },
} as const satisfies Record<
  MCPConnectionState,
  { tone: "success" | "warning" | "destructive"; labelKey: ParseKeys }
>

// 对话设置：顶栏按钮打开的弹窗，放只属于这个对话的三项，对应 TUI 的 /executor、/cd、/mcp：
// Executor 设置和工作目录（conversation/setExecutorSettings）、MCP 连接状态、这个对话可用的 Skill。
// 点弹窗外面不关，只有 Esc 和关闭按钮能关；保存中哪种方式都不关，保存结果和失败原因才看得到
export function ConversationSettingsDialog({
  conversationID,
}: {
  conversationID: ConversationID
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const [open, setOpen] = useState(false)
  const save = useMutation(setExecutorSettingsOptions(connection))
  return (
    <Dialog
      open={open}
      disablePointerDismissal
      onOpenChange={(next) => {
        if (save.isPending) return
        // 关闭时清掉这次的保存结果，下次打开不带着旧的失败原因
        if (!next) save.reset()
        setOpen(next)
      }}
    >
      <Tooltip>
        <TooltipTrigger
          render={
            <DialogTrigger
              render={
                <Button variant="ghost" size="icon" className="shrink-0" />
              }
              aria-label={t("conversationSettings.title")}
            />
          }
        >
          <Settings2Icon />
        </TooltipTrigger>
        <TooltipContent className="text-caption">
          {t("conversationSettings.title")}
        </TooltipContent>
      </Tooltip>
      {/* 固定高度：切页签时弹窗不变高变矮、不重新居中，内容超出时页签内滚动 */}
      <DialogContent className="flex h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] flex-col text-body sm:h-[min(36rem,calc(100dvh-2rem))] sm:max-w-xl">
        <DialogHeader className="shrink-0">
          <DialogTitle className="text-section">
            {t("conversationSettings.title")}
          </DialogTitle>
        </DialogHeader>
        <Tabs defaultValue="executor" className="flex min-h-0 flex-1 flex-col">
          <TabsList className="shrink-0">
            <TabsTrigger value="executor" className="text-ui">
              {t("conversationSettings.tabs.executor")}
            </TabsTrigger>
            <TabsTrigger value="mcp" className="text-ui">
              {t("conversationSettings.tabs.mcp")}
            </TabsTrigger>
            <TabsTrigger value="skills" className="text-ui">
              {t("conversationSettings.tabs.skills")}
            </TabsTrigger>
          </TabsList>
          <TabsContent
            value="executor"
            keepMounted
            className="flex min-h-0 flex-col overflow-hidden text-body [&[hidden]]:hidden"
          >
            <ExecutorSettingsSection
              conversationID={conversationID}
              save={save}
            />
          </TabsContent>
          <TabsContent
            value="mcp"
            className="min-h-0 overflow-y-auto text-body"
          >
            <McpServerSection conversationID={conversationID} />
          </TabsContent>
          <TabsContent
            value="skills"
            className="min-h-0 overflow-y-auto text-body"
          >
            <SkillSection conversationID={conversationID} />
          </TabsContent>
        </Tabs>
      </DialogContent>
    </Dialog>
  )
}

// 保存 Executor 设置的提交，由弹窗持有：保存中弹窗不能关
type SaveExecutorSettings = UseMutationResult<
  ConversationResult,
  Error,
  { conversationID: ConversationID; executorSettings: ExecutorSettings }
>

// Executor 页签：读对话记录里的设置和 Executor 列表，都到了就显示表单（之后后台重取失败也不换掉正在编辑的表单）；
// 还缺数据时有错误就显示错误，否则显示加载
function ExecutorSettingsSection({
  conversationID,
  save,
}: {
  conversationID: ConversationID
  save: SaveExecutorSettings
}) {
  const connection = useServerConnection()
  const conversation = useQuery(conversationOptions(connection, conversationID))
  const executors = useQuery(executorListOptions(connection))
  return (
    <QueryState query={conversation} shouldShowError={shouldShowLoadError}>
      {(conversationData) => (
        <QueryState query={executors} shouldShowError={shouldShowLoadError}>
          {(executorData) => (
            <ExecutorSettingsForm
              conversationID={conversationID}
              saved={conversationData.conversation.executor_settings}
              executors={executorData.executors}
              save={save}
            />
          )}
        </QueryState>
      )}
    </QueryState>
  )
}

// Executor 设置表单：默认 Executor 和它的工作目录，以及对话能用哪些 Executor；保存时先解析目录，再提交完整的新设置
function ExecutorSettingsForm({
  conversationID,
  saved,
  executors,
  save,
}: {
  conversationID: ConversationID
  saved: ExecutorSettings
  executors: ExecutorDetails[]
  save: SaveExecutorSettings
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const location = useWorkLocation(executors, saved)
  // 勾选的 Executor；默认 Executor 总是可用，不看这里
  const [checkedExecutorIDs, setCheckedExecutorIDs] = useState(
    () =>
      new Set(
        executors
          .filter((each) => isExecutorAvailable(saved, each.id))
          .map((each) => each.id)
      )
  )
  const defaultExecutorID = location.executorID

  async function handleSubmit(event: FormEvent) {
    event.preventDefault()
    if (!defaultExecutorID || connection.$state.get() !== "connected") return
    // 1. 先解析工作目录；失败时原因显示在输入框旁，不提交。
    let directory: string | undefined
    try {
      directory = await location.resolveDirectory()
    } catch {
      return
    }
    // 2. 提交完整的新设置：全部勾选时可用列表为空，以后新加的 Executor 也可用；目录留空时用默认根目录。
    const executorIDs = executors
      .map((each) => each.id)
      .filter((id) => id === defaultExecutorID || checkedExecutorIDs.has(id))
    const workspaceDirectories = { ...saved.workspace_directories }
    if (directory) workspaceDirectories[defaultExecutorID] = directory
    else delete workspaceDirectories[defaultExecutorID]
    if (connection.$state.get() !== "connected") return
    save.mutate({
      conversationID,
      executorSettings: {
        default_executor_id: defaultExecutorID,
        executor_ids:
          executorIDs.length === executors.length ? undefined : executorIDs,
        workspace_directories: workspaceDirectories,
      },
    })
  }

  return (
    <form
      className="flex min-h-0 flex-1 flex-col"
      onSubmit={handleSubmit}
      onFocusCapture={() => {
        if (save.isSuccess) save.reset()
      }}
      onChange={() => {
        if (save.isSuccess || save.isError) save.reset()
      }}
    >
      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-1">
        <FieldSet className="min-w-0 gap-2">
          <FieldLegend className="text-section">
            {t("conversationSettings.executor.location")}
          </FieldLegend>
          <div className="flex flex-wrap items-center gap-2">
            <WorkLocationFields executors={executors} location={location} />
          </div>
        </FieldSet>
        <FieldSet className="min-w-0 gap-2">
          <FieldLegend className="text-section">
            {t("conversationSettings.executor.available")}
          </FieldLegend>
          <ul className="flex flex-col">
            {executors.map((each) => {
              const isDefault = each.id === defaultExecutorID
              return (
                <li key={each.id}>
                  <label className="flex min-h-8 min-w-0 items-center gap-2 p-2 text-table">
                    <Checkbox
                      checked={isDefault || checkedExecutorIDs.has(each.id)}
                      disabled={isDefault}
                      onCheckedChange={(checked: boolean) => {
                        const next = new Set(checkedExecutorIDs)
                        if (checked) next.add(each.id)
                        else next.delete(each.id)
                        setCheckedExecutorIDs(next)
                      }}
                    />
                    <ExecutorOptionDetails
                      executor={each}
                      directory={
                        saved.workspace_directories?.[each.id] ||
                        each.workspace_directory
                      }
                    />
                    {isDefault && (
                      <Badge variant="outline" className="text-caption">
                        {t("conversationSettings.executor.default")}
                      </Badge>
                    )}
                  </label>
                </li>
              )
            })}
          </ul>
        </FieldSet>
      </div>
      <div className="flex shrink-0 flex-col gap-2 px-1 pt-3">
        {save.isError && <RequestError error={save.error} />}
        {save.isSuccess && (
          <p role="status" className="text-caption color-success">
            {t("conversationSettings.saved")}
          </p>
        )}
        {/* 目录还在解析时也能点：提交时会等它解析完 */}
        <Button
          type="submit"
          className="self-end text-ui"
          disabled={save.isPending || !isConnected}
        >
          {save.isPending ? (
            <LatticeLoader
              data-icon="inline-start"
              aria-label={t("common.loading")}
            />
          ) : (
            <SaveIcon data-icon="inline-start" />
          )}
          {t("conversationSettings.save")}
        </Button>
      </div>
    </form>
  )
}

// MCP 页签：对话里每个 Executor 上的 MCP 连接，按 Executor 顺序；连接状态会变，可以手动刷新
function McpServerSection({
  conversationID,
}: {
  conversationID: ConversationID
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const servers = useQuery(mcpServerListOptions(connection, conversationID))
  return (
    <QueryState query={servers} shouldShowError={shouldShowLoadError}>
      {(data) => (
        <div className="flex flex-col gap-2">
          <Button
            variant="outline"
            className="self-start text-ui"
            disabled={servers.isFetching || !isConnected}
            onClick={() => void servers.refetch()}
          >
            {servers.isFetching ? (
              <LatticeLoader
                data-icon="inline-start"
                aria-label={t("common.loading")}
              />
            ) : (
              <RefreshCwIcon data-icon="inline-start" />
            )}
            {t("conversation.panel.refresh")}
          </Button>
          {data.servers.length === 0 ? (
            <ListEmpty
              title={t("conversationSettings.mcp.empty")}
              description={t("conversationSettings.mcp.emptyDescription")}
            />
          ) : (
            <RowList>
              {data.servers.map((server) => {
                // serve 新增的状态原样显示
                const state = mcpStates[server.state] ?? {
                  tone: undefined,
                  labelKey: undefined,
                }
                return (
                  <RowListItem
                    key={server.executor_id + "/" + server.server_id}
                  >
                    <div className="flex min-w-0 items-center gap-2">
                      <OverflowTooltip
                        render={<span className="min-w-0 truncate" />}
                      >
                        {server.server_id}
                      </OverflowTooltip>
                      <OverflowTooltip
                        render={
                          <span className="max-w-32 min-w-0 shrink-0 truncate color-muted" />
                        }
                      >
                        @{server.executor_id}
                      </OverflowTooltip>
                      <span
                        data-status-tone={state.tone}
                        className="ml-auto shrink-0 text-caption"
                      >
                        {state.labelKey ? t(state.labelKey) : server.state}
                      </span>
                    </div>
                    <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-caption color-muted">
                      <span>
                        {t("conversationSettings.mcp.tools", {
                          count: server.tools.length,
                        })}
                      </span>
                      {server.required && (
                        <span>{t("conversationSettings.mcp.required")}</span>
                      )}
                      {server.plugin_name && (
                        <OverflowTooltip
                          render={<span className="min-w-0 truncate" />}
                        >
                          {t("conversationSettings.mcp.plugin", {
                            name: server.plugin_name,
                          })}
                        </OverflowTooltip>
                      )}
                    </div>
                    {server.error && (
                      <p className="text-caption wrap-anywhere color-destructive">
                        {server.error}
                      </p>
                    )}
                  </RowListItem>
                )
              })}
            </RowList>
          )}
        </div>
      )}
    </QueryState>
  )
}

// Skill 页签：这个对话可用的 Skill，按 serve 返回的顺序
function SkillSection({ conversationID }: { conversationID: ConversationID }) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const skills = useQuery(skillListOptions(connection, { conversationID }))
  return (
    <QueryState query={skills} shouldShowError={shouldShowLoadError}>
      {(data) => (
        <SkillList
          skills={data}
          emptyTitle={t("conversationSettings.skills.empty")}
          emptyDescription={t("conversationSettings.skills.emptyDescription")}
        />
      )}
    </QueryState>
  )
}
