import { shouldShowLoadError } from "@/lib/rpc-error"
import { useStore } from "@nanostores/react"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useMutation, useQuery } from "@tanstack/react-query"
import { PlusIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Link, useSearchParams } from "react-router"
import { DetailPageState } from "@/components/base/detail-page-state"
import { ListEmpty } from "@/components/base/list-empty"
import { PageLayout } from "@/components/base/page-layout"
import { QueryState } from "@/components/base/query-state"
import { RequestError } from "@/components/base/request-error"
import { RowList, RowListItem } from "@/components/base/row-list"
import { ModelPicker } from "@/components/conversation/model-picker"
import { ProviderEditor } from "@/components/settings/provider-editor"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { useServerConnection } from "@/components/server-connection-provider"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { LatticeLoader } from "@/components/base/lattice-loader"
import type { ProviderConfig } from "@/lib/api/protocol.gen"
import { notifyDefinitionsUpdated, notifyFailure } from "@/lib/notify"
import {
  getProviderCreatePath,
  getProviderEditPath,
  getSettingsPath,
} from "@/lib/paths"
import {
  isProviderProtocol,
  newProviderConfig,
  omittedProviderProtocol,
  providerProtocolLabelKeys,
} from "@/lib/provider-form"
import { newConversationAgentName } from "@/lib/query/conversation"
import { modelListOptions } from "@/lib/query/model"
import {
  providerListOptions,
  setDefaultModelOptions,
} from "@/lib/query/provider"
import { formatModelRef } from "@/lib/utils"

// 模型服务列表：全部模型服务和下面的默认模型，点一行进入编辑；页头可以新增
export function ProviderListPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const providers = useQuery(providerListOptions(connection))
  return (
    <SettingsLayout
      section="providers"
      actions={
        <Button
          className="text-ui"
          nativeButton={false}
          render={<Link to={getProviderCreatePath(connection.getServerID())} />}
        >
          <PlusIcon data-icon="inline-start" />
          {t("settings.providers.add")}
        </Button>
      }
    >
      <QueryState shouldShowError={shouldShowLoadError} query={providers}>
        {(data) =>
          data.providers.length === 0 ? (
            <ListEmpty title={t("settings.providers.empty")} />
          ) : (
            <>
              <ProviderList
                providers={data.providers}
                defaultModel={data.default_model}
              />
              <DefaultModelSection defaultModel={data.default_model} />
            </>
          )
        }
      </QueryState>
    </SettingsLayout>
  )
}

// 默认模型：从全部已配置的模型里选，选完立刻写进配置；default Agent 自己指定了别的模型时提示新对话仍用它
function DefaultModelSection({ defaultModel }: { defaultModel: string }) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const setDefaultModel = useMutation(setDefaultModelOptions(connection))
  const agentModels = useQuery(
    modelListOptions(connection, newConversationAgentName)
  )
  const [providerID, modelID] = defaultModel.split("/")
  const agentModelRef = agentModels.data?.agent_default_model
  const agentModel = agentModelRef?.provider_id
    ? formatModelRef(agentModelRef)
    : ""
  return (
    <section className="flex min-w-0 flex-col gap-2 border-t pt-4">
      <h2 className="text-section">{t("settings.providers.defaultModel")}</h2>
      <div className="flex min-w-0 items-center gap-2">
        <ModelPicker
          disabled={!isConnected || setDefaultModel.isPending}
          agentName=""
          variant="outline"
          showReasoningEffort={false}
          value={
            defaultModel
              ? { ref: { provider_id: providerID, model_id: modelID } }
              : undefined
          }
          onChange={({ ref }) => {
            if (connection.$state.get() !== "connected") return
            const model = formatModelRef(ref)
            if (model === defaultModel) return
            setDefaultModel.mutate(model, {
              onSuccess: (result) =>
                notifyDefinitionsUpdated(
                  t("settings.providers.defaultModelChanged", { model }),
                  result,
                  t
                ),
              onError: (error) => notifyFailure(error, t),
            })
          }}
        />
        {setDefaultModel.isPending && (
          <LatticeLoader aria-label={t("common.loading")} />
        )}
      </div>
      {agentModel && agentModel !== defaultModel && (
        <p role="status" className="text-caption color-warning">
          {t("settings.providers.agentModel", {
            agent: newConversationAgentName,
            model: agentModel,
          })}
        </p>
      )}
    </section>
  )
}

// 模型服务的行：ID、是否有默认模型、模型数、协议和 base_url
function ProviderList({
  providers,
  defaultModel,
}: {
  providers: ProviderConfig[]
  defaultModel: string
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const defaultProviderID = defaultModel.split("/")[0]
  return (
    <RowList>
      {providers.map((provider) => {
        const protocol = provider.protocol || omittedProviderProtocol
        return (
          <RowListItem
            key={provider.id}
            to={getProviderEditPath(connection.getServerID(), provider.id)}
          >
            <span className="flex min-w-0 items-center gap-2">
              <OverflowTooltip render={<span className="min-w-0 truncate" />}>
                {provider.id}
              </OverflowTooltip>
              {provider.id === defaultProviderID && (
                <Badge variant="outline" className="text-caption">
                  {t("settings.providers.defaultBadge")}
                </Badge>
              )}
              <span className="ml-auto shrink-0 text-caption color-muted">
                {t("settings.providers.modelCount", {
                  count: Object.keys(provider.models ?? {}).length,
                })}
              </span>
            </span>
            <div className="flex min-w-0 items-center gap-2 text-table color-muted">
              <OverflowTooltip render={<span className="min-w-0 truncate" />}>
                {provider.base_url}
              </OverflowTooltip>
              <OverflowTooltip
                render={
                  <span className="ml-auto max-w-1/2 shrink-0 truncate text-caption" />
                }
              >
                {isProviderProtocol(protocol)
                  ? t(providerProtocolLabelKeys[protocol])
                  : protocol}
              </OverflowTooltip>
            </div>
          </RowListItem>
        )
      })}
    </RowList>
  )
}

// 新增和编辑页的面包屑：设置 › 模型服务
function useProviderListNavigation() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const listPath = getSettingsPath(connection.getServerID(), "providers")
  const parents = [
    { label: t("settings.title") },
    { label: t("settings.sections.providers"), to: listPath },
  ]
  return { listPath, parents }
}

// 新增模型服务；读模型服务列表是为了查重名和显示现在的默认模型，列表后台刷新失败时在表单上方显示原因，填了一半的表单不丢
export function ProviderCreatePage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const providers = useQuery(providerListOptions(connection))
  const { listPath, parents } = useProviderListNavigation()
  return (
    <PageLayout title={t("settings.providers.add")} parents={parents}>
      <QueryState shouldShowError={shouldShowLoadError} query={providers}>
        {(data) => (
          <ProviderEditor
            mode="create"
            saved={newProviderConfig}
            defaultModel={data.default_model}
            defaults={data.defaults}
            existingProviderIDs={data.providers.map((each) => each.id)}
            listPath={listPath}
          />
        )}
      </QueryState>
    </PageLayout>
  )
}

// 编辑模型服务，?id= 是 Provider id；列表里没有它时显示「不存在」，列表后台刷新失败时在表单上方显示原因
export function ProviderEditPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const [params] = useSearchParams()
  const id = params.get("id") ?? ""
  const providers = useQuery(providerListOptions(connection))
  const { listPath, parents } = useProviderListNavigation()
  const provider = providers.data?.providers.find((each) => each.id === id)
  return (
    <DetailPageState
      shouldShowError={shouldShowLoadError}
      id={id}
      title={id || t("settings.sections.providers")}
      entity={t("settings.providers.entity")}
      parents={parents}
      notFoundReturn={{
        to: listPath,
        label: t("settings.providers.backToList"),
      }}
      missingIdText={t("settings.providers.missingID")}
      query={{
        data: provider,
        error: providers.error,
        refetch: () => void providers.refetch(),
      }}
      notFound={!!providers.data && !provider}
    >
      {(saved) => (
        <PageLayout title={saved.id} parents={parents}>
          {providers.isError && shouldShowLoadError(providers.error) && (
            <RequestError
              error={providers.error}
              onRetry={() => void providers.refetch()}
            />
          )}
          <ProviderEditor
            key={saved.id}
            mode="edit"
            saved={saved}
            defaultModel={providers.data?.default_model ?? ""}
            defaults={providers.data!.defaults}
            existingProviderIDs={[]}
            listPath={listPath}
          />
        </PageLayout>
      )}
    </DetailPageState>
  )
}
