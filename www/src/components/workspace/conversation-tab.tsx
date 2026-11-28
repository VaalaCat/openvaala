import { NavigationButton } from "@/components/base/navigation-button"
import { motion } from "motion/react"
import { BrandIcon } from "@/components/base/brand-icon"
import { RequestError } from "@/components/base/request-error"
import { agentCatalogOptions } from "@/lib/query/agent-settings"
import { executorListOptions } from "@/lib/query/executor"
import { uploadAttachment } from "@/lib/attachments"
import { useCallback, useMemo, type ComponentProps } from "react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import type { ReadableAtom } from "nanostores"
import { useTranslation } from "react-i18next"
import { DetailPageState } from "@/components/base/detail-page-state"
import { Composer } from "@/components/conversation/composer"
import {
  ForkOriginNotice,
  type ForkOriginNoticeProps,
} from "@/components/conversation/fork-origin-notice"
import { MessageScroller } from "@/components/conversation/message-scroller"
import {
  ContextUsageButton,
  SessionControls,
} from "@/components/conversation/session-controls"
import { useServerConnection } from "@/components/server-connection-provider"
import { useIsMobile } from "@/hooks/use-mobile"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import {
  ErrorCodeSessionNotFound,
  type ItemID,
  type ModelSelection,
  type SessionID,
} from "@/lib/api/protocol.gen"
import { getConversationMessageStores } from "@/lib/conversation-message-stores"
import { shouldShowForkOrigin } from "@/lib/fork-origin"
import type { ItemDebugTarget, MessageRow } from "@/lib/item-renderers"
import { notifyFailure } from "@/lib/notify"
import { getConversationPath, getServerPath } from "@/lib/paths"
import { conversationOptions } from "@/lib/query/conversation"
import { RpcError, shouldShowLoadError } from "@/lib/rpc-error"
import { getSideConversationRows } from "@/lib/workspace/side-conversation"
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"
import type { TabWorkspaceTab } from "@/lib/workspace/types"

// 主对话、子 Agent 和旁支标签页：消息区和输入框；详情放在工作区右边的右栏里。
// 子 Agent 只读，输入框的位置放模型和上下文用量。
// 旁支只显示分叉之后的内容，第一行写明从哪条对话分出；分叉出来的主对话在历史最前面写明来源。
// 对话里已经有内容时输入框支持 /btw，旁支里不支持
export function ConversationTab({
  workspace,
  tabID,
  tab,
  onInspect,
}: {
  workspace: TabWorkspace
  tabID: string
  tab: Extract<
    TabWorkspaceTab,
    { kind: "main" | "sub-agent" | "side-conversation" }
  >
  onInspect(
    tabID: string,
    sync: ConversationSync,
    target: ItemDebugTarget
  ): void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const sync = workspace.getConversationSync(tabID)
  if (!sync) throw new Error("conversation tab has no sync: " + tabID)
  const sessionID = tab.kind === "sub-agent" ? tab.sessionID : undefined
  const isMobile = useIsMobile()
  const syncState = useStore(sync.$syncState)
  const hasInitialData = sync.hasInitialData()
  const isReady = syncState === "synced"
  const isConnected = useStore(connection.$state) === "connected"
  const rootSessionID = useStore(sync.$rootSessionID)
  const followedSessionID = hasInitialData
    ? sync.getFollowedSessionID()
    : (sessionID ?? rootSessionID ?? "")
  const { $messageRows, $hasItems } = getConversationMessageStores(sync)
  const hasItems = useStore($hasItems)
  const inspectTabItem = useCallback(
    (target: ItemDebugTarget) => onInspect(tabID, sync, target),
    [onInspect, tabID, sync]
  )
  // 主对话标签页读对话记录里的分叉来源；旁支指回主对话标签页；子 Agent 没有来源提示
  const conversation = useQuery({
    ...conversationOptions(connection, sync.conversationID),
    enabled: tab.kind !== "sub-agent",
  })
  const forkedFromID = conversation.data?.conversation.forked_from_id
  const forkOrigin: ForkOriginNoticeProps | undefined =
    tab.kind === "side-conversation"
      ? {
          kind: "side-conversation",
          sourceConversationID: workspace.conversationID,
          onOpenSource: () => workspace.openMain(),
        }
      : tab.kind === "main" && forkedFromID
        ? { kind: "fork", sourceConversationID: forkedFromID }
        : undefined
  const sessionInfo = useStore(sync.$sessions, { keys: [followedSessionID] })[
    followedSessionID
  ]
  const agents = useQuery(agentCatalogOptions(connection))
  const executors = useQuery(executorListOptions(connection))
  const external = sessionInfo?.session.external
  const agent = external
    ? (agents.data?.agents?.find(
        (a) => a.name === sessionInfo.session.agent_name
      ) ?? {
        name: sessionInfo.session.agent_name,
        type: external.agent_type,
        icon: "",
      })
    : undefined
  const externalState = useStore(sync.$externalState)
  const restoring = useStore(sync.$restoring)
  const restoreError = useStore(sync.$restoreError)
  const detached = sessionInfo?.session.detached_conversation_id
  const scope = {
    conversation_id: sync.conversationID,
    session_id: followedSessionID,
  }
  const onError = (error: unknown) => notifyFailure(error, t)
  const setModel = useMutation({
    mutationFn: (model: ModelSelection) =>
      connection.call("session/setModel", { ...scope, model }),
    onError,
  })
  const setOption = useMutation({
    mutationFn: (
      option: import("@/lib/api/protocol.gen").ExternalOptionSelection
    ) => connection.call("session/setOption", { ...scope, ...option }),
    onError,
  })
  const reload = useMutation({
    mutationFn: () => sync.reloadSession(followedSessionID),
    onError,
  })
  const externalModel = externalState.options?.model?.current_value || "default"
  const sessionControls = (
    <SessionControls
      agentName={sessionInfo?.session.agent_name}
      scope={scope}
      value={
        agent
          ? { ref: { provider_id: agent.name, model_id: externalModel } }
          : sessionInfo?.session.model
      }
      onChange={(model) => {
        if (agent)
          setOption.mutate({ kind: "model", value: model.ref.model_id })
        else setModel.mutate(model)
      }}
      externalOptions={externalState.options}
      onOption={setOption.mutate}
      disabled={!sessionInfo || !isReady || !isConnected || !!detached}
    />
  )
  const contextUsage = (
    <ContextUsageButton
      sync={sync}
      sessionInfo={sessionInfo}
      canCompact={!sessionID && !external}
      disabled={!sessionInfo || !isReady || !isConnected}
    />
  )
  return (
    // 消息区占满标签页，滚动条贴着右边。
    <div className="flex h-full min-h-0 flex-1 flex-col">
      <ConversationMessages
        sync={sync}
        isReady={
          hasInitialData && syncState !== "failed" && !restoring && !detached
        }
        loadingContent={
          detached ? (
            <div className="flex items-center gap-2 py-4 text-body color-muted">
              {t("externalSession.detached")}
              <NavigationButton
                className="text-link"
                to={getConversationPath(sync.serverID, detached)}
              >
                {t("externalSession.openDetached")}
              </NavigationButton>
            </div>
          ) : restoreError ? (
            <div className="py-4">
              <RequestError
                title={t("externalSession.failed")}
                error={new Error(restoreError)}
                retryLabel={t("externalSession.reload")}
                retryDisabled={reload.isPending}
                onRetry={() => reload.mutate()}
              />
            </div>
          ) : restoring ? (
            <motion.div
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0 }}
              transition={{ duration: 0.15, ease: "easeOut" }}
              className="flex items-center gap-2 py-4 text-body color-muted"
            >
              <BrandIcon icon={agent?.icon ?? ""} className="animate-pulse" />
              {t("externalSession.restoring")}
            </motion.div>
          ) : (
            <ConversationContentState
              sync={sync}
              sessionID={sessionID}
              onRetry={() => workspace.restartConversationSync(tabID)}
            />
          )
        }
        isSubSession={!!sessionID}
        $messageRows={$messageRows}
        forkOrigin={forkOrigin}
        boundaryItemID={
          tab.kind === "side-conversation" ? tab.boundaryItemID : undefined
        }
        footer={
          sessionID ? (
            <div className="flex items-center gap-4 py-2">
              {sessionControls}
              {contextUsage}
            </div>
          ) : (
            <Composer
              serverID={sync.serverID}
              conversationID={sync.conversationID}
              sync={sync}
              isReady={isReady}
              isRunning={!!sessionInfo?.active_turn_id}
              externalAgent={agent}
              commands={externalState.commands?.filter((command) => command.name !== "btw")}
              supportsSteer={externalState.capabilities?.supports_steer}
              uploadFile={(file) => {
                const settings =
                  conversation.data?.conversation.executor_settings
                const executor =
                  sessionInfo?.session.external?.executor_id ||
                  settings?.default_executor_id ||
                  executors.data?.executors.find(
                    (e) =>
                      e.kind !== "memory" &&
                      (!settings?.executor_ids?.length ||
                        settings.executor_ids.includes(e.id))
                  )?.id
                if (!executor)
                  return Promise.reject(new Error(t("workLocation.executor")))
                return uploadAttachment(
                  connection,
                  sync.conversationID,
                  executor,
                  file
                )
              }}
              sessionControls={sessionControls}
              contextUsage={contextUsage}
              onSideConversation={
                tab.kind === "main" &&
                hasItems &&
                (!external || externalState.capabilities?.supports_ephemeral_session)
                  ? async (question) => {
                      await workspace.openSideConversation(question, isMobile)
                    }
                  : undefined
              }
              autoFocus={tab.kind === "side-conversation" && !tab.question}
            />
          )
        }
        onInspect={inspectTabItem}
      />
    </div>
  )
}

// 消息区订阅展示行，正文更新只进入阅读区域；给了 boundaryItemID（旁支）时只显示从它那一轮开始的行；
// 给了 forkOrigin 时，在条件满足的历史顶部显示分叉来源提示。
function ConversationMessages({
  $messageRows,
  boundaryItemID,
  forkOrigin,
  ...props
}: Omit<
  ComponentProps<typeof MessageScroller>,
  "rows" | "canLoadEarlier" | "header"
> & {
  $messageRows: ReadableAtom<MessageRow[]>
  boundaryItemID?: ItemID
  forkOrigin?: ForkOriginNoticeProps
}) {
  // 1. 读取消息区与调试共用的展示行，旁支截掉继承来的历史。
  const allRows = useStore($messageRows)
  const hasEarlierItems = useStore(props.sync.$hasEarlierItems)
  const rows = useMemo(
    () =>
      boundaryItemID
        ? getSideConversationRows(allRows, boundaryItemID)
        : allRows,
    [allRows, boundaryItemID]
  )
  // 2. 旁支的开头还没出现时才允许往前翻（旁支很长、开头不在最近一页里），出现后不再往前翻。
  return (
    <MessageScroller
      {...props}
      header={
        forkOrigin &&
        shouldShowForkOrigin(forkOrigin.kind, rows, hasEarlierItems) ? (
          <ForkOriginNotice {...forkOrigin} />
        ) : undefined
      }
      rows={rows}
      canLoadEarlier={!boundaryItemID || rows.length === 0}
    />
  )
}

// 在消息位置展示首次同步、资源不存在或业务错误。
function ConversationContentState({
  sync,
  sessionID,
  onRetry,
}: {
  sync: ConversationSync
  sessionID?: SessionID
  onRetry(): void
}) {
  // 1. 子 Session 不存在时提供返回主 Session 的入口，对话不存在时提供回到新对话页的入口。
  const { t } = useTranslation()
  const syncError = useStore(sync.$syncError)
  const isSessionMissing =
    !!sessionID &&
    syncError instanceof RpcError &&
    syncError.code === ErrorCodeSessionNotFound
  return (
    <DetailPageState
      embedded
      id={sync.conversationID}
      title={t("common.loading")}
      entity={t(
        isSessionMissing ? "conversation.session.entity" : "conversation.entity"
      )}
      parents={[]}
      notFoundReturn={{
        to: isSessionMissing
          ? getConversationPath(sync.serverID, sync.conversationID)
          : getServerPath(sync.serverID),
        label: t(
          isSessionMissing
            ? "conversation.session.backToRoot"
            : "conversation.backToNewConversation"
        ),
      }}
      missingIdText=""
      shouldShowError={shouldShowLoadError}
      query={{ error: syncError, refetch: onRetry }}
    >
      {() => null}
    </DetailPageState>
  )
}
