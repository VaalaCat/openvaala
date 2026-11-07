import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useEffect, useRef, useState, type FormEvent } from "react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import {
  ChevronDownIcon,
  DownloadIcon,
  LibraryIcon,
  PencilIcon,
  PlusIcon,
  XIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { ConfigurationLayout } from "@/components/base/configuration-layout"
import { DangerZone } from "@/components/base/danger-zone"
import { EntityCombobox } from "@/components/base/entity-combobox"
import { NumberUnitInput } from "@/components/base/number-unit-input"
import { TokenNumberValue } from "@/components/base/token-number-value"
import { FormInput } from "@/components/base/form-input"
import { PageActions } from "@/components/base/page-layout"
import { RequestError } from "@/components/base/request-error"
import { RowList } from "@/components/base/row-list"
import { SecretInput } from "@/components/base/secret-input"
import { UnsavedChangesGuard } from "@/components/base/unsaved-changes-guard"
import { FetchModelsDialog } from "@/components/settings/fetch-models-dialog"
import { useServerConnection } from "@/components/server-connection-provider"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import {
  Field,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectGroup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { useDraftBaseline } from "@/hooks/use-draft-baseline"
import type { ModelDefaults, ProviderConfig } from "@/lib/api/protocol.gen"
import type { DraftBaselineRules } from "@/lib/draft-baseline"
import {
  notifyDefinitionsUpdated,
  notifyFailure,
  notifySuccess,
} from "@/lib/notify"
import {
  addManualModels,
  applyFetchedModelSelection,
  changeProviderModelAlias,
  fromProviderConfig,
  getProviderIDIssue,
  getProviderModelIssues,
  getProviderModelReasoning,
  isSameProviderDraft,
  providerProtocolLabelKeys,
  providerProtocols,
  toProviderConfig,
  toProviderHeaders,
  type ProviderDraft,
  type ProviderHeaderDraft,
  type ProviderModelDraft,
  type ProviderModelIssues,
} from "@/lib/provider-form"
import {
  catalogModelListOptions,
  catalogProviderListOptions,
} from "@/lib/query/catalog"
import {
  deleteProviderOptions,
  fetchProviderModelsOptions,
  setProviderOptions,
} from "@/lib/query/provider"
import { parseNumberUnit, formatNumberUnit } from "@/lib/number-unit"
import { shouldShowLoadError } from "@/lib/rpc-error"

// 已保存的 Provider 和表单的比较规则；模块级常量，供 useDraftBaseline 使用
const providerDraftRules: DraftBaselineRules<ProviderConfig, ProviderDraft> = {
  readDraft: fromProviderConfig,
  isSameSaved: (a, b) => JSON.stringify(a) === JSON.stringify(b),
  isSameDraft: isSameProviderDraft,
}

// 拉取模型弹窗的来源：上游 /models（provider/fetchModels），或从模型目录选的服务（catalog/models/list）
type ModelSource = "upstream" | "catalog"

// 模型服务表单：新增时可以先从模型目录选服务，预填 ID、协议和 base_url，目录拿不到时直接手动填；
// 模型可以拉取、从目录选或手动输入，默认模型随保存一起写进配置。页头放「撤销」「应用」（新增时是「创建」），
// 失败原因显示在表单顶部；有未保存修改时拦住离开。新增成功回到 listPath，编辑页最后一节可以删除
export function ProviderEditor({
  mode,
  saved,
  defaultModel,
  defaults,
  existingProviderIDs,
  listPath,
}: {
  mode: "create" | "edit"
  saved: ProviderConfig
  // 配置里现在的默认模型（provider/model），没有时为空
  defaultModel: string
  defaults: ModelDefaults
  // 已有的 Provider id，新增时用来查重名
  existingProviderIDs: string[]
  listPath: string
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const navigate = useNavigate()
  const isCreating = mode === "create"
  const save = useMutation(setProviderOptions(connection))
  const remove = useMutation(deleteProviderOptions(connection))
  const fetchModels = useMutation(fetchProviderModelsOptions(connection))
  // 保存时一起设为默认的模型（这个服务里的模型 id），空表示不改
  const [defaultModelID, setDefaultModelID] = useState("")
  const edit = useDraftBaseline({
    saved,
    pending: save.isPending,
    localDirty: defaultModelID !== "",
    rules: providerDraftRules,
  })
  const draft = edit.draft
  // 新增时从模型目录选的服务，没选时为空
  const [catalogProviderID, setCatalogProviderID] = useState("")
  const [catalogKeyword, setCatalogKeyword] = useState("")
  const [manualModelText, setManualModelText] = useState("")
  const [expandedModelID, setExpandedModelID] = useState<string>()
  const [hasSubmitted, setHasSubmitted] = useState(false)
  const [modelSource, setModelSource] = useState<ModelSource>()
  const [isDeleteOpen, setIsDeleteOpen] = useState(false)
  // 新增成功后先清掉未保存状态，下一次渲染再跳回列表，否则离开拦截会拦住这次跳转
  const [isCreated, setIsCreated] = useState(false)
  const isMounted = useRef(true)
  const catalog = useQuery({
    ...catalogProviderListOptions(connection),
    enabled: isCreating,
  })
  const catalogModels = useQuery({
    ...catalogModelListOptions(connection, catalogProviderID),
    enabled: modelSource === "catalog",
  })
  useEffect(() => {
    isMounted.current = true
    return () => {
      isMounted.current = false
    }
  }, [])
  useEffect(() => {
    if (isCreated) void navigate(listPath)
  }, [isCreated, listPath, navigate])

  const providerID = draft.id.trim()
  const dirty = !isCreated && edit.dirty
  // 保存中和保存成功后列表里已经有这个 ID，不再查重
  const idIssue =
    isCreating && (save.isIdle || save.isError)
      ? getProviderIDIssue(draft.id, existingProviderIDs)
      : undefined
  // 编辑的服务里有现在的默认模型、它又被移出了模型列表时，必须另选一个
  const [defaultProviderID, defaultProviderModelID] = defaultModel.split("/")
  const isDefaultModelRemoved =
    !isCreating &&
    !defaultModelID &&
    defaultProviderID === saved.id &&
    !draft.models.includes(defaultProviderModelID)
  const modelIssues = getProviderModelIssues(draft, defaults)

  function change(patch: Partial<ProviderDraft>) {
    edit.setDraft({ ...draft, ...patch })
  }

  // 选了目录里的服务：ID 还空着或者是上次从目录带出来的，换成这个服务的 ID；协议和 base_url 换成目录里的
  function chooseCatalogProvider(catalogID: string) {
    const choice = catalog.data?.providers.find((each) => each.id === catalogID)
    setCatalogProviderID(catalogID)
    if (!choice) return
    change({
      id: !providerID || draft.id === catalogProviderID ? choice.id : draft.id,
      protocol: choice.protocol || providerProtocols[0],
      baseURL: choice.base_url,
    })
  }

  function changeHeader(index: number, patch: Partial<ProviderHeaderDraft>) {
    change({
      headers: draft.headers.map((each, i) =>
        i === index ? { ...each, ...patch } : each
      ),
    })
  }

  function addTypedModels() {
    change(addManualModels(draft, manualModelText))
    setManualModelText("")
  }

  function removeModel(modelID: string) {
    change({ models: draft.models.filter((each) => each !== modelID) })
    if (modelID === defaultModelID) setDefaultModelID("")
    if (modelID === expandedModelID) setExpandedModelID(undefined)
  }

  function openModelSource(source: ModelSource) {
    if (source === "upstream" && connection.$state.get() !== "connected") return
    setModelSource(source)
    if (source === "upstream") fetchModels.mutate(connectionConfig())
  }

  // 拉取模型只读取连接字段，模型编辑中的原文继续留在草稿。
  function connectionConfig(): ProviderConfig {
    // 1. 不将模型字段带入上游目录请求。
    return {
      id: providerID,
      protocol: draft.protocol,
      base_url: draft.baseURL.trim(),
      api_key: draft.apiKey.trim(),
      endpoints: Object.fromEntries(
        Object.entries(draft.endpoints)
          .filter(([, value]) => value.trim())
          .map(([key, value]) => [key, value.trim()])
      ),
      headers: toProviderHeaders(draft.headers),
      models: null,
    }
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    if (connection.$state.get() !== "connected") return
    // 1. ID 有问题、一个模型都没有、默认模型被移除时停在表单，原因显示在字段旁。
    setHasSubmitted(true)
    if (idIssue || draft.models.length === 0 || isDefaultModelRemoved) return
    const invalidModelID = Object.keys(modelIssues)[0]
    if (invalidModelID) {
      setExpandedModelID(invalidModelID)
      return
    }
    // 2. 提交整份 Provider；选了默认模型时一起写进配置。
    const provider = toProviderConfig(draft)
    const selectedDefaultID =
      defaultModelID ||
      (defaultProviderID === saved.id &&
      draft.models.includes(defaultProviderModelID)
        ? defaultProviderModelID
        : "")
    const selectedDefaultAlias = selectedDefaultID
      ? draft.modelAliases[selectedDefaultID].trim()
      : ""
    void save
      .mutateAsync({
        provider,
        default_model: selectedDefaultAlias
          ? provider.id + "/" + selectedDefaultAlias
          : undefined,
      })
      .then((result) => {
        // 3. 新增成功回到列表；编辑时留在本页，草稿换成刚保存的内容。
        if (!isMounted.current) return
        notifyDefinitionsUpdated(
          t(
            isCreating
              ? "settings.providerEditor.created"
              : "settings.providerEditor.saved",
            { name: provider.id }
          ),
          result,
          t
        )
        setDefaultModelID("")
        setHasSubmitted(false)
        if (isCreating) setIsCreated(true)
        else edit.commit(provider)
      })
      .catch((error: unknown) => {
        // 4. 失败时编辑器还在就显示在表单顶部；已经离开了就弹出提示。
        if (!isMounted.current) notifyFailure(error, t)
      })
  }

  const catalogProviders = [...(catalog.data?.providers ?? [])].sort(
    (a, b) =>
      a.name.toLowerCase().localeCompare(b.name.toLowerCase()) ||
      a.id.localeCompare(b.id)
  )
  const isCatalogUnavailable =
    (catalog.isError && shouldShowLoadError(catalog.error)) ||
    (catalog.isSuccess && catalogProviders.length === 0)
  const catalogOptions = catalogProviders
    .filter((each) =>
      (each.name + " " + each.id)
        .toLowerCase()
        .includes(catalogKeyword.toLowerCase())
    )
    .map((each) => ({
      id: each.id,
      label: each.name,
      description: each.id === each.name ? undefined : each.id,
    }))
  const savedDefaultModelID =
    defaultProviderID === saved.id &&
    draft.models.includes(defaultProviderModelID)
      ? defaultProviderModelID
      : ""
  const defaultModelOptions = [
    ...(!savedDefaultModelID
      ? [
          {
            value: null,
            label: defaultModel || t("settings.providerEditor.noDefault"),
          },
        ]
      : []),
    ...draft.models.map((modelID) => ({
      value: modelID,
      label: providerID + "/" + draft.modelAliases[modelID],
    })),
  ]
  const idError =
    idIssue === "invalid"
      ? t("settings.providerEditor.idInvalid")
      : idIssue === "duplicate"
        ? t("settings.providerEditor.idDuplicate", { name: providerID })
        : undefined

  return (
    <div className="flex min-w-0 flex-col gap-4">
      <UnsavedChangesGuard dirty={dirty} />
      <PageActions>
        {isCreating ? (
          <Button
            className="text-ui"
            type="submit"
            form="provider-editor"
            disabled={save.isPending || isCreated || !isConnected}
          >
            {save.isPending ? (
              <LatticeLoader
                data-icon="inline-start"
                aria-label={t("common.loading")}
              />
            ) : (
              <PlusIcon data-icon="inline-start" />
            )}
            {t("settings.providerEditor.create")}
          </Button>
        ) : (
          <>
            <Button
              className="text-ui"
              type="button"
              variant="outline"
              disabled={save.isPending || !edit.dirty}
              onClick={() => {
                edit.revert()
                setDefaultModelID("")
                setHasSubmitted(false)
                setExpandedModelID(undefined)
                save.reset()
              }}
            >
              {t("settings.providerEditor.revert")}
            </Button>
            <Button
              className="text-ui"
              type="submit"
              form="provider-editor"
              disabled={
                save.isPending || !edit.dirty || edit.stale || !isConnected
              }
            >
              {save.isPending && (
                <LatticeLoader
                  data-icon="inline-start"
                  aria-label={t("common.loading")}
                />
              )}
              {t("settings.providerEditor.apply")}
            </Button>
          </>
        )}
      </PageActions>
      {save.isError && <RequestError error={save.error} />}
      {edit.stale && (
        <p role="status" className="text-caption color-warning">
          {t("settings.providerEditor.savedChanged")}
        </p>
      )}
      <form
        id="provider-editor"
        className="flex min-w-0 flex-col gap-4"
        onSubmit={handleSubmit}
      >
        <fieldset
          disabled={save.isPending}
          className="flex min-w-0 flex-col gap-4"
        >
          <ConfigurationLayout title={t("settings.providerEditor.connection")}>
            {isCreating &&
              (isCatalogUnavailable ? (
                <p
                  role="status"
                  className="text-caption wrap-anywhere color-muted"
                >
                  {catalog.isError
                    ? t("settings.providerEditor.catalogFailed", {
                        detail: catalog.error.message,
                      })
                    : t("settings.providerEditor.catalogEmpty")}
                </p>
              ) : (
                <EntityCombobox
                  label={t("settings.providerEditor.catalog")}
                  placeholder={t("settings.providerEditor.catalogPlaceholder")}
                  value={catalogProviderID}
                  onChange={chooseCatalogProvider}
                  options={catalogOptions}
                  selectedOptions={catalogOptions.filter(
                    (each) => each.id === catalogProviderID
                  )}
                  loading={catalog.isPending}
                  error={catalog.error}
                  shouldShowError={shouldShowLoadError}
                  paused={!isConnected}
                  onRetry={() => void catalog.refetch()}
                  onSearch={setCatalogKeyword}
                />
              ))}
            <FieldGroup className="grid min-w-0 items-start gap-3 sm:grid-cols-2">
              {isCreating && (
                <FormInput
                  id="provider-id"
                  label={t("settings.providerEditor.id")}
                  value={draft.id}
                  required
                  autoComplete="off"
                  spellCheck={false}
                  placeholder={t("settings.providerEditor.idPlaceholder")}
                  error={idError}
                  onChange={(event) => change({ id: event.target.value })}
                />
              )}
              <Field>
                <FieldLabel className="text-ui" htmlFor="provider-protocol">
                  {t("settings.providerEditor.protocol")}
                </FieldLabel>
                <Select
                  value={draft.protocol}
                  onValueChange={(next: string | null) => {
                    if (next) change({ protocol: next })
                  }}
                  items={providerProtocols.map((each) => ({
                    value: each,
                    label: t(providerProtocolLabelKeys[each]),
                  }))}
                >
                  <SelectTrigger
                    id="provider-protocol"
                    className="w-full min-w-0 text-ui"
                  >
                    <SelectValue className="truncate" />
                  </SelectTrigger>
                  <SelectContent alignItemWithTrigger={false} align="start">
                    <SelectGroup>
                      {providerProtocols.map((each) => (
                        <SelectItem
                          key={each}
                          value={each}
                          className="text-table"
                        >
                          {t(providerProtocolLabelKeys[each])}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
              </Field>
              <FormInput
                id="provider-base-url"
                label={t("settings.providerEditor.baseURL")}
                value={draft.baseURL}
                required
                autoComplete="off"
                spellCheck={false}
                placeholder="https://api.openai.com/v1"
                onChange={(event) => change({ baseURL: event.target.value })}
              />
              <SecretInput
                id="provider-api-key"
                label={t("settings.providerEditor.apiKey")}
                value={draft.apiKey}
                onChange={(apiKey) => change({ apiKey })}
              />
            </FieldGroup>
            <Collapsible
              defaultOpen={Object.values(saved.endpoints ?? {}).some(Boolean)}
              className="flex min-w-0 flex-col"
            >
              <CollapsibleTrigger
                render={
                  <Button
                    type="button"
                    variant="ghost"
                    className="self-start text-ui"
                  />
                }
              >
                <ChevronDownIcon data-icon="inline-start" />
                {t("settings.providerEditor.endpoints")}
              </CollapsibleTrigger>
              <CollapsibleContent
                className="process-disclosure-panel"
                render={(props, state) => (
                  <div
                    {...props}
                    inert={!state.open}
                    aria-hidden={!state.open || undefined}
                  />
                )}
              >
                <FieldGroup className="process-disclosure-content grid min-w-0 gap-3 pt-3 sm:grid-cols-2">
                  {providerProtocols.map((protocol) => (
                    <FormInput
                      key={protocol}
                      id={"provider-endpoint-" + protocol}
                      label={t(providerProtocolLabelKeys[protocol])}
                      value={draft.endpoints[protocol] ?? ""}
                      type="url"
                      placeholder={
                        draft.baseURL || "https://api.example.com/v1"
                      }
                      onChange={(event) =>
                        change({
                          endpoints: {
                            ...draft.endpoints,
                            [protocol]: event.target.value,
                          },
                        })
                      }
                    />
                  ))}
                </FieldGroup>
              </CollapsibleContent>
            </Collapsible>
            <Collapsible
              defaultOpen={Object.keys(saved.headers ?? {}).length > 0}
              className="flex min-w-0 flex-col"
            >
              <CollapsibleTrigger
                render={
                  <Button
                    type="button"
                    variant="ghost"
                    className="self-start text-ui"
                  />
                }
              >
                <ChevronDownIcon data-icon="inline-start" />
                {t("settings.providerEditor.headers")}
              </CollapsibleTrigger>
              <CollapsibleContent
                className="process-disclosure-panel"
                render={(props, state) => (
                  <div
                    {...props}
                    inert={!state.open}
                    aria-hidden={!state.open || undefined}
                  />
                )}
              >
                <FieldGroup className="process-disclosure-content min-w-0 gap-3 pt-3">
                  {draft.headers.map((header, index) => (
                    <div
                      key={index}
                      className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_auto]"
                    >
                      <Input
                        aria-label={t("settings.providerEditor.headerName")}
                        className="text-ui"
                        value={header.name}
                        autoComplete="off"
                        spellCheck={false}
                        onChange={(event) =>
                          changeHeader(index, { name: event.target.value })
                        }
                      />
                      <Input
                        aria-label={t("settings.providerEditor.headerValue")}
                        className="text-ui"
                        value={header.value}
                        autoComplete="off"
                        spellCheck={false}
                        onChange={(event) =>
                          changeHeader(index, { value: event.target.value })
                        }
                      />
                      <Tooltip>
                        <TooltipTrigger
                          render={
                            <Button
                              className="col-start-2 row-span-2 row-start-1 text-ui sm:col-start-3 sm:row-span-1"
                              type="button"
                              variant="ghost"
                              size="icon-sm"
                              aria-label={t(
                                "settings.providerEditor.removeHeader"
                              )}
                              onClick={() =>
                                change({
                                  headers: draft.headers.filter(
                                    (_, i) => i !== index
                                  ),
                                })
                              }
                            />
                          }
                        >
                          <XIcon />
                        </TooltipTrigger>
                        <TooltipContent className="text-caption">
                          {t("settings.providerEditor.removeHeader")}
                        </TooltipContent>
                      </Tooltip>
                    </div>
                  ))}
                  <Button
                    className="self-start text-ui"
                    type="button"
                    variant="outline"
                    onClick={() =>
                      change({
                        headers: [...draft.headers, { name: "", value: "" }],
                      })
                    }
                  >
                    <PlusIcon data-icon="inline-start" />
                    {t("settings.providerEditor.addHeader")}
                  </Button>
                  <p className="text-caption wrap-anywhere color-muted">
                    {t("settings.providerEditor.headersHint")}
                  </p>
                </FieldGroup>
              </CollapsibleContent>
            </Collapsible>
          </ConfigurationLayout>
          <ConfigurationLayout
            title={t("settings.providerEditor.models")}
            summary={t("settings.providers.modelCount", {
              count: draft.models.length,
            })}
          >
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <Input
                aria-label={t("settings.providerEditor.modelIDs")}
                placeholder={t("settings.providerEditor.modelIDsPlaceholder")}
                className="min-w-48 flex-1 text-ui"
                value={manualModelText}
                autoComplete="off"
                spellCheck={false}
                onChange={(event) => setManualModelText(event.target.value)}
                onKeyDown={(event) => {
                  // 回车加进列表，不提交整个表单
                  if (event.key !== "Enter" || event.nativeEvent.isComposing)
                    return
                  event.preventDefault()
                  addTypedModels()
                }}
              />
              <Button
                className="text-ui"
                type="button"
                variant="outline"
                disabled={!manualModelText.trim()}
                onClick={addTypedModels}
              >
                <PlusIcon data-icon="inline-start" />
                {t("settings.providerEditor.addModels")}
              </Button>
              <Button
                className="text-ui"
                type="button"
                variant="outline"
                disabled={!draft.baseURL.trim() || !isConnected}
                onClick={() => openModelSource("upstream")}
              >
                <DownloadIcon data-icon="inline-start" />
                {t("settings.providerEditor.fetchModels")}
              </Button>
              {catalogProviderID && (
                <Button
                  className="text-ui"
                  type="button"
                  variant="outline"
                  onClick={() => openModelSource("catalog")}
                >
                  <LibraryIcon data-icon="inline-start" />
                  {t("settings.providerEditor.catalogModels")}
                </Button>
              )}
            </div>
            {draft.models.length > 0 && (
              <RowList>
                {draft.models.map((modelID) => {
                  const config = draft.modelConfigs[modelID]
                  const alias = draft.modelAliases[modelID]
                  const capacity = parseNumberUnit(config.context_window)
                  const contextWindow = capacity.ok
                    ? capacity.value || defaults.context_window
                    : undefined
                  const expanded = expandedModelID === modelID
                  return (
                    <Collapsible
                      key={modelID}
                      render={<li />}
                      open={expanded}
                      onOpenChange={(open) =>
                        setExpandedModelID(open ? modelID : undefined)
                      }
                      className="flex min-w-0 flex-col text-table"
                    >
                      <div className="flex min-h-8 min-w-0 items-center gap-2 p-2">
                        <OverflowTooltip
                          render={<span className="min-w-0 truncate" />}
                        >
                          {alias || modelID}
                        </OverflowTooltip>
                        {config.display_name && (
                          <OverflowTooltip
                            render={
                              <span className="min-w-0 truncate color-muted max-sm:hidden" />
                            }
                          >
                            {config.display_name}
                          </OverflowTooltip>
                        )}
                        <span className="ml-auto flex shrink-0 items-center gap-2">
                          {(defaultModelID ||
                            (defaultProviderID === saved.id
                              ? defaultProviderModelID
                              : "")) === modelID && (
                            <Badge variant="outline" className="text-caption">
                              {t("settings.providers.defaultBadge")}
                            </Badge>
                          )}
                          {contextWindow !== undefined && (
                            <TokenNumberValue
                              value={contextWindow}
                              className="color-muted"
                            />
                          )}
                          <Tooltip>
                            <TooltipTrigger
                              render={
                                <CollapsibleTrigger
                                  render={
                                    <Button variant="ghost" size="icon-sm" />
                                  }
                                  className="text-ui"
                                  type="button"
                                  aria-label={t(
                                    "settings.providerEditor.editModel",
                                    {
                                      name: alias || modelID,
                                    }
                                  )}
                                  aria-expanded={expanded}
                                  aria-controls={"model-fields-" + modelID}
                                />
                              }
                            >
                              <PencilIcon />
                            </TooltipTrigger>
                            <TooltipContent className="text-caption">
                              {t("settings.providerEditor.editModel", {
                                name: alias || modelID,
                              })}
                            </TooltipContent>
                          </Tooltip>
                          <Tooltip>
                            <TooltipTrigger
                              render={
                                <Button
                                  className="text-ui"
                                  type="button"
                                  variant="ghost"
                                  size="icon-sm"
                                  aria-label={t(
                                    "settings.providerEditor.removeModel",
                                    { name: alias || modelID }
                                  )}
                                  onClick={() => removeModel(modelID)}
                                />
                              }
                            >
                              <XIcon />
                            </TooltipTrigger>
                            <TooltipContent className="text-caption">
                              {t("settings.providerEditor.removeModel", {
                                name: alias || modelID,
                              })}
                            </TooltipContent>
                          </Tooltip>
                        </span>
                      </div>
                      <CollapsibleContent
                        id={"model-fields-" + modelID}
                        className="process-disclosure-panel min-w-0"
                        inert={!expanded}
                        aria-hidden={!expanded || undefined}
                      >
                        <div className="process-disclosure-content min-w-0 border-t bg-muted/20 p-3">
                          <ProviderModelFields
                            modelID={modelID}
                            alias={alias}
                            model={config}
                            defaults={defaults}
                            providerProtocol={draft.protocol}
                            issues={modelIssues[modelID] ?? {}}
                            onAliasChange={(value) =>
                              change(
                                changeProviderModelAlias(draft, modelID, value)
                              )
                            }
                            onChange={(patch) =>
                              change({
                                modelConfigs: {
                                  ...draft.modelConfigs,
                                  [modelID]: { ...config, ...patch },
                                },
                              })
                            }
                          />
                        </div>
                      </CollapsibleContent>
                    </Collapsible>
                  )
                })}
              </RowList>
            )}
            {hasSubmitted && draft.models.length === 0 && (
              <FieldError className="text-caption">
                {t("settings.providerEditor.modelsRequired")}
              </FieldError>
            )}
            <Field data-invalid={isDefaultModelRemoved}>
              <FieldLabel className="text-ui" htmlFor="provider-default-model">
                {t("settings.providers.defaultModel")}
              </FieldLabel>
              <Select
                value={defaultModelID || savedDefaultModelID || null}
                onValueChange={(next: string | null) =>
                  setDefaultModelID(
                    next === savedDefaultModelID ? "" : (next ?? "")
                  )
                }
                items={defaultModelOptions}
              >
                <SelectTrigger
                  id="provider-default-model"
                  className="w-full max-w-96 min-w-0 text-ui"
                  aria-invalid={isDefaultModelRemoved || undefined}
                >
                  <OverflowTooltip
                    data-slot="select-value"
                    render={<SelectValue className="truncate" />}
                  />
                </SelectTrigger>
                <SelectContent alignItemWithTrigger={false} align="start">
                  <SelectGroup>
                    {defaultModelOptions.map((option) => (
                      <SelectItem
                        key={option.value ?? "__default"}
                        value={option.value}
                        className="text-table [&>span:first-child]:min-w-0 [&>span:first-child]:shrink"
                      >
                        <OverflowTooltip
                          render={<span className="min-w-0 truncate" />}
                        >
                          {option.label}
                        </OverflowTooltip>
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              {isDefaultModelRemoved && (
                <FieldError className="text-caption">
                  {t("settings.providerEditor.defaultModelRemoved", {
                    model: defaultModel,
                  })}
                </FieldError>
              )}
            </Field>
          </ConfigurationLayout>
        </fieldset>
      </form>
      {!isCreating && (
        <DangerZone
          actionLabel={t("settings.providerEditor.delete")}
          disabled={save.isPending || edit.dirty || !isConnected}
          disabledReason={
            !isConnected
              ? t("common.errors.connectionLost")
              : edit.dirty
                ? t("settings.providerEditor.dirtyActions")
                : undefined
          }
          onAction={() => {
            remove.reset()
            setIsDeleteOpen(true)
          }}
        />
      )}
      <ConfigurationChangeDialog
        open={isDeleteOpen}
        onOpenChange={setIsDeleteOpen}
        title={t("settings.providerEditor.deleteTitle", { name: saved.id })}
        description={t("settings.providerEditor.deleteConfirmDescription", {
          name: saved.id,
        })}
        preview={remove.isError && <RequestError error={remove.error} />}
        confirmLabel={t("settings.providerEditor.delete")}
        destructive
        pending={remove.isPending}
        confirmDisabled={!isConnected}
        onConfirm={() => {
          if (connection.$state.get() !== "connected") return
          remove.mutate(saved.id, {
            onSuccess: () => {
              notifySuccess(
                t("settings.providerEditor.deleted", { name: saved.id })
              )
              setIsDeleteOpen(false)
              void navigate(listPath)
            },
          })
        }}
      />
      <FetchModelsDialog
        open={!!modelSource}
        title={t(
          modelSource === "catalog"
            ? "settings.fetchModels.catalogTitle"
            : "settings.fetchModels.title"
        )}
        models={
          modelSource === "catalog"
            ? catalogModels.data && (catalogModels.data.models ?? {})
            : fetchModels.data && (fetchModels.data.models ?? {})
        }
        error={
          modelSource === "catalog" ? catalogModels.error : fetchModels.error
        }
        shouldShowError={
          modelSource === "catalog" ? shouldShowLoadError : undefined
        }
        configuredModelIDs={draft.models}
        onRetry={() => {
          if (connection.$state.get() !== "connected") return
          if (modelSource === "catalog") void catalogModels.refetch()
          else fetchModels.mutate(connectionConfig())
        }}
        onConfirm={(selectedModelIDs) => {
          const fetched =
            (modelSource === "catalog"
              ? catalogModels.data?.models
              : fetchModels.data?.models) ?? {}
          const next = applyFetchedModelSelection(
            draft,
            fetched,
            selectedModelIDs
          )
          change(next)
          if (expandedModelID && !next.models.includes(expandedModelID))
            setExpandedModelID(undefined)
          // 选作默认的模型被取消勾选时，恢复已保存的默认模型。
          if (defaultModelID && !next.models.includes(defaultModelID))
            setDefaultModelID("")
          setModelSource(undefined)
        }}
        onOpenChange={(open) => {
          if (!open) setModelSource(undefined)
        }}
      />
    </div>
  )
}

// 展示单个模型的全部字段，修改直接写入整页草稿。
function ProviderModelFields({
  modelID,
  alias,
  model,
  defaults,
  providerProtocol,
  issues,
  onAliasChange,
  onChange,
}: {
  modelID: string
  alias: string
  model: ProviderModelDraft
  defaults: ModelDefaults
  providerProtocol: string
  issues: ProviderModelIssues
  onAliasChange(value: string): void
  onChange(patch: Partial<ProviderModelDraft>): void
}) {
  // 1. 从字段本身读取继承模式与有效值。
  const { t } = useTranslation()
  const id = "model-" + modelID + "-"
  const inherited = (value: string) =>
    t("settings.providerEditor.inherited", { value })
  const reasoning = getProviderModelReasoning(model, defaults)
  const inputs = model.capabilities?.input ??
    defaults.capabilities.input ?? ["text"]
  const roles = model.merge_message_roles ?? defaults.merge_message_roles ?? []
  const aliasErrorKeys = {
    required: "settings.providerEditor.aliasRequired",
    invalid: "settings.providerEditor.idInvalid",
    duplicate: "settings.providerEditor.aliasDuplicate",
  } as const
  const aliasError = issues.alias ? t(aliasErrorKeys[issues.alias]) : undefined
  const modeOptions = [
    { value: "inherit", label: t("settings.providerEditor.inherit") },
    { value: "custom", label: t("settings.providerEditor.custom") },
  ]
  const inputLabels = {
    text: "settings.providerEditor.inputs.text",
    image: "settings.providerEditor.inputs.image",
    audio: "settings.providerEditor.inputs.audio",
    video: "settings.providerEditor.inputs.video",
    pdf: "settings.providerEditor.inputs.pdf",
  } as const
  const roleLabels = {
    system: "settings.providerEditor.roles.system",
    developer: "settings.providerEditor.roles.developer",
    user: "settings.providerEditor.roles.user",
    assistant: "settings.providerEditor.roles.assistant",
  } as const
  const effortError = issues.reasoning_efforts
    ? t(
        issues.reasoning_efforts === "empty"
          ? "settings.providerEditor.effortEmpty"
          : "settings.providerEditor.effortDuplicate"
      )
    : undefined
  const knownProtocol = providerProtocols.find(
    (value) => value === providerProtocol
  )
  const protocolLabel = knownProtocol
    ? t(providerProtocolLabelKeys[knownProtocol])
    : providerProtocol
  const numberErrorKeys = {
    negative: "numberUnitInput.errors.negative",
    invalid: "numberUnitInput.errors.invalid",
    fractional: "numberUnitInput.errors.fractional",
    unsafe: "numberUnitInput.errors.unsafe",
  } as const

  // 2. 基本、容量、推理和列表字段按两列展示，窄屏自动变为一列。
  return (
    <FieldGroup className="min-w-0 gap-4">
      <FieldSet className="min-w-0 gap-3">
        <FieldLegend className="text-section">
          {t("settings.providerEditor.basic")}
        </FieldLegend>
        <FieldGroup className="grid min-w-0 gap-3 sm:grid-cols-2">
          <FormInput
            id={id + "alias"}
            label={t("settings.providerEditor.alias")}
            value={alias}
            error={aliasError}
            required
            autoComplete="off"
            spellCheck={false}
            onChange={(event) => onAliasChange(event.target.value)}
          />
          <FormInput
            id={id + "upstream"}
            label={t("settings.providerEditor.upstreamID")}
            value={model.upstream_model ?? ""}
            placeholder={alias}
            autoComplete="off"
            spellCheck={false}
            onChange={(event) =>
              onChange({ upstream_model: event.target.value })
            }
          />
          <FormInput
            id={id + "display"}
            label={t("settings.providerEditor.displayName")}
            value={model.display_name ?? ""}
            placeholder={alias}
            onChange={(event) => onChange({ display_name: event.target.value })}
          />
          <ProviderFieldSelect
            id={id + "protocol"}
            label={t("settings.providerEditor.protocol")}
            value={model.protocol || null}
            options={[
              { value: null, label: inherited(protocolLabel) },
              ...providerProtocols.map((protocol) => ({
                value: protocol,
                label: t(providerProtocolLabelKeys[protocol]),
              })),
            ]}
            onChange={(value) => onChange({ protocol: value ?? undefined })}
          />
        </FieldGroup>
      </FieldSet>
      <FieldSet className="min-w-0 gap-3">
        <FieldLegend className="text-section">
          {t("settings.providerEditor.capacity")}
        </FieldLegend>
        <FieldGroup className="grid min-w-0 gap-3 sm:grid-cols-2">
          {(["context_window", "max_output_tokens"] as const).map((field) => (
            <Field key={field} data-invalid={!!issues[field]}>
              <FieldLabel className="text-ui" htmlFor={id + field}>
                {t(
                  field === "context_window"
                    ? "settings.providerEditor.contextWindow"
                    : "settings.providerEditor.maxOutputTokens"
                )}
              </FieldLabel>
              <NumberUnitInput
                id={id + field}
                value={model[field]}
                error={issues[field]}
                aria-invalid={!!issues[field]}
                placeholder={inherited(formatNumberUnit(defaults[field]))}
                onChange={(value) => onChange({ [field]: value })}
              />
              {issues[field] && (
                <FieldError className="text-caption">
                  {t(numberErrorKeys[issues[field]])}
                </FieldError>
              )}
            </Field>
          ))}
        </FieldGroup>
      </FieldSet>
      <FieldSet className="min-w-0 gap-3">
        <FieldLegend className="text-section">
          {t("settings.providerEditor.reasoning")}
        </FieldLegend>
        <FieldGroup className="grid min-w-0 gap-3 sm:grid-cols-2">
          <ProviderFieldSelect
            id={id + "thinking"}
            label={t("settings.providerEditor.thinking")}
            value={
              model.thinking === undefined
                ? null
                : model.thinking
                  ? "on"
                  : "off"
            }
            options={[
              {
                value: null,
                label: inherited(
                  t(
                    defaults.thinking
                      ? "settings.providerEditor.on"
                      : "settings.providerEditor.off"
                  )
                ),
              },
              { value: "on", label: t("settings.providerEditor.on") },
              { value: "off", label: t("settings.providerEditor.off") },
            ]}
            onChange={(value) =>
              onChange({
                thinking: value === null ? undefined : value === "on",
              })
            }
          />
          <ProviderFieldSelect
            id={id + "effort-mode"}
            label={t("settings.providerEditor.reasoningEfforts")}
            value={model.reasoning_efforts === undefined ? "inherit" : "custom"}
            options={modeOptions}
            onChange={(value) =>
              onChange({
                reasoning_efforts:
                  value === "inherit"
                    ? undefined
                    : (defaults.reasoning_efforts ?? []).join(", "),
              })
            }
          />
          <FormInput
            id={id + "efforts"}
            label={t("settings.providerEditor.effortValues")}
            disabled={model.reasoning_efforts === undefined}
            value={
              model.reasoning_efforts ??
              (defaults.reasoning_efforts ?? []).join(", ")
            }
            placeholder={t("settings.providerEditor.emptyList")}
            error={effortError}
            onChange={(event) =>
              onChange({ reasoning_efforts: event.target.value })
            }
          />
          <ProviderFieldSelect
            id={id + "default-effort"}
            label={t("settings.providerEditor.defaultEffort")}
            value={model.default_reasoning_effort || null}
            error={
              issues.default_reasoning_effort
                ? t("settings.providerEditor.effortUnavailable")
                : undefined
            }
            options={[
              {
                value: null,
                label: t("settings.providerEditor.automatic", {
                  value:
                    reasoning.automatic || t("settings.providerEditor.none"),
                }),
              },
              ...reasoning.efforts.map((effort) => ({
                value: effort,
                label: effort,
              })),
              ...(issues.default_reasoning_effort
                ? [
                    {
                      value: model.default_reasoning_effort!,
                      label: model.default_reasoning_effort!,
                    },
                  ]
                : []),
            ]}
            onChange={(value) =>
              onChange({ default_reasoning_effort: value ?? undefined })
            }
          />
        </FieldGroup>
      </FieldSet>
      <FieldSet className="min-w-0 gap-3">
        <FieldLegend className="text-section">
          {t("settings.providerEditor.inputCapabilities")}
        </FieldLegend>
        <ProviderFieldSelect
          id={id + "input-mode"}
          label={t("settings.providerEditor.inputMode")}
          value={model.capabilities?.input === undefined ? "inherit" : "custom"}
          options={modeOptions}
          onChange={(value) =>
            onChange({
              capabilities: {
                ...model.capabilities,
                input:
                  value === "inherit"
                    ? undefined
                    : [...(defaults.capabilities.input ?? ["text"])],
              },
            })
          }
        />
        <FieldGroup className="flex flex-row flex-wrap gap-4">
          {(Object.keys(inputLabels) as (keyof typeof inputLabels)[]).map(
            (input) => (
              <Field
                key={input}
                orientation="horizontal"
                className="w-auto"
                data-disabled={model.capabilities?.input === undefined}
              >
                <Checkbox
                  id={id + "input-" + input}
                  checked={inputs.includes(input)}
                  disabled={model.capabilities?.input === undefined}
                  onCheckedChange={(checked) =>
                    onChange({
                      capabilities: {
                        ...model.capabilities,
                        input: checked
                          ? [...inputs, input]
                          : inputs.filter((value) => value !== input),
                      },
                    })
                  }
                />
                <FieldLabel className="text-ui" htmlFor={id + "input-" + input}>
                  {t(inputLabels[input])}
                </FieldLabel>
              </Field>
            )
          )}
        </FieldGroup>
        {inputs.length === 0 && (
          <p className="text-caption color-muted">
            {t("settings.providerEditor.textFallback")}
          </p>
        )}
      </FieldSet>
      <FieldSet className="min-w-0 gap-3">
        <FieldLegend className="text-section">
          {t("settings.providerEditor.messageMerge")}
        </FieldLegend>
        <ProviderFieldSelect
          id={id + "merge-mode"}
          label={t("settings.providerEditor.mergeMode")}
          value={model.merge_message_roles === undefined ? "inherit" : "custom"}
          options={modeOptions}
          onChange={(value) =>
            onChange({
              merge_message_roles:
                value === "inherit"
                  ? undefined
                  : [...(defaults.merge_message_roles ?? [])],
            })
          }
        />
        <FieldGroup className="flex flex-row flex-wrap gap-4">
          {(Object.keys(roleLabels) as (keyof typeof roleLabels)[]).map(
            (role) => (
              <Field
                key={role}
                orientation="horizontal"
                className="w-auto"
                data-disabled={model.merge_message_roles === undefined}
              >
                <Checkbox
                  id={id + "role-" + role}
                  checked={roles.includes(role)}
                  disabled={model.merge_message_roles === undefined}
                  onCheckedChange={(checked) =>
                    onChange({
                      merge_message_roles: checked
                        ? [...roles, role]
                        : roles.filter((value) => value !== role),
                    })
                  }
                />
                <FieldLabel className="text-ui" htmlFor={id + "role-" + role}>
                  {t(roleLabels[role])}
                </FieldLabel>
              </Field>
            )
          )}
        </FieldGroup>
        {roles.length === 0 && (
          <p className="text-caption color-muted">
            {t("settings.providerEditor.noMerge")}
          </p>
        )}
      </FieldSet>
    </FieldGroup>
  )
}

// 展示带标签、选项和字段错误的模型选择框。
function ProviderFieldSelect({
  id,
  label,
  value,
  options,
  error,
  onChange,
}: {
  id: string
  label: string
  value: string | null
  options: { value: string | null; label: string }[]
  error?: string
  onChange(value: string | null): void
}) {
  // 1. 选择值直接交回整页草稿。
  return (
    <Field className="min-w-0" data-invalid={!!error}>
      <FieldLabel className="text-ui" htmlFor={id}>
        {label}
      </FieldLabel>
      <Select value={value} items={options} onValueChange={onChange}>
        <SelectTrigger
          id={id}
          className="w-full min-w-0 text-ui"
          aria-invalid={!!error}
        >
          <SelectValue className="truncate" />
        </SelectTrigger>
        <SelectContent alignItemWithTrigger={false} align="start">
          <SelectGroup>
            {options.map((option) => (
              <SelectItem
                className="text-table [&>span:first-child]:min-w-0 [&>span:first-child]:shrink"
                key={option.value ?? "__inherit"}
                value={option.value}
              >
                <OverflowTooltip render={<span className="min-w-0 truncate" />}>
                  {option.label}
                </OverflowTooltip>
              </SelectItem>
            ))}
          </SelectGroup>
        </SelectContent>
      </Select>
      {error && <FieldError className="text-caption">{error}</FieldError>}
    </Field>
  )
}
