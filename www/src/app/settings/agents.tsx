import { KeyValueRows } from "@/components/base/key-value-rows"
import { useState, type FormEvent } from "react"
import { useStore } from "@nanostores/react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { PlusIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Navigate, useNavigate, useSearchParams } from "react-router"
import { NavigationButton } from "@/components/base/navigation-button"
import { BrandIcon } from "@/components/base/brand-icon"
import { EntityCombobox } from "@/components/base/entity-combobox"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { ConfigurationLayout } from "@/components/base/configuration-layout"
import { DangerZone } from "@/components/base/danger-zone"
import { FormInput } from "@/components/base/form-input"
import { PageLayout } from "@/components/base/page-layout"
import { QueryState } from "@/components/base/query-state"
import { RequestError } from "@/components/base/request-error"
import { RowList, RowListItem } from "@/components/base/row-list"
import { UnsavedChangesGuard } from "@/components/base/unsaved-changes-guard"
import { ModelPicker } from "@/components/conversation/model-picker"
import {
  SettingsDraftActions,
  SettingsLayout,
} from "@/components/settings/settings-layout"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"
import type {
  AgentConfig,
  AgentInfo,
  AgentModelConfig,
} from "@/lib/api/protocol.gen"
import { notifyDefinitionsUpdated } from "@/lib/notify"
import { getSettingsPath } from "@/lib/paths"
import {
  agentCatalogOptions,
  agentDefinitionOptions,
  agentPresetCatalogOptions,
  deleteAgentOptions,
  getAgentPresetName,
  setAgentOptions,
} from "@/lib/query/agent-settings"
import { shouldShowLoadError } from "@/lib/rpc-error"
import { formatModelRef } from "@/lib/utils"

function agentEditPath(listPath: string, name: string) {
  return listPath + "/edit?" + new URLSearchParams({ name }).toString()
}

function AgentRow({ info, path }: { info: AgentInfo; path: string }) {
  const connection = useServerConnection()
  const definition = useQuery({
    ...agentDefinitionOptions(connection, info.name),
    enabled: info.type !== "native",
  })
  const detail =
    info.type !== "native"
      ? definition.data?.acp?.command
      : info.default_model && formatModelRef(info.default_model)
  return (
    <RowListItem to={agentEditPath(path, info.name)}>
      <span className="flex min-w-0 items-center gap-2 text-table">
        <BrandIcon icon={info.icon ?? ""} />
        <span className="truncate">{info.name}</span>
      </span>
      {detail && (
        <span className="block truncate pl-6 text-table color-muted">
          {detail}
        </span>
      )}
    </RowListItem>
  )
}

export function AgentListPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const agents = useQuery(agentCatalogOptions(connection))
  const path = getSettingsPath(connection.getServerID(), "agents")
  return (
    <SettingsLayout
      section="agents"
      actions={
        <Button
          className="text-ui"
          role="button"
          render={<NavigationButton to={path + "/new"} />}
        >
          <PlusIcon data-icon="inline-start" />
          {t("settings.agents.add")}
        </Button>
      }
    >
      <QueryState query={agents} shouldShowError={shouldShowLoadError}>
        {(data) => (
          <RowList>
            {data.agents.map((info) => (
              <AgentRow key={info.name} info={info} path={path} />
            ))}
          </RowList>
        )}
      </QueryState>
    </SettingsLayout>
  )
}

export function AgentCreatePage() {
  return <AgentSettingsPage />
}

export function AgentEditPage() {
  const [search] = useSearchParams()
  const name = search.get("name") ?? ""
  return <AgentSettingsPage key={name} name={name} />
}

function AgentSettingsPage({ name }: { name?: string }) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const path = getSettingsPath(connection.getServerID(), "agents")
  const agents = useQuery({
    ...agentCatalogOptions(connection),
    enabled: name === undefined,
  })
  const definition = useQuery({
    ...agentDefinitionOptions(connection, name ?? ""),
    enabled: name !== undefined,
  })
  return (
    <PageLayout
      title={name ?? t("settings.agents.add")}
      parents={[
        { label: t("settings.title") },
        { label: t("settings.sections.agents"), to: path },
      ]}
    >
      {name === undefined ? (
        <QueryState query={agents} shouldShowError={shouldShowLoadError}>
          {(data) => (
            <AgentEditor
              path={path}
              saved={{ name: "", type: "native" }}
              creating
              existingAgentNames={data.agents.map((agent) => agent.name)}
            />
          )}
        </QueryState>
      ) : (
        <QueryState query={definition} shouldShowError={shouldShowLoadError}>
          {(saved) => (
            <AgentEditor path={path} saved={saved} creating={false} />
          )}
        </QueryState>
      )}
    </PageLayout>
  )
}

function AgentModelField({
  label,
  value,
  onChange,
  disabled,
}: {
  label: string
  value: AgentModelConfig | undefined
  onChange(model: AgentModelConfig | undefined): void
  disabled: boolean
}) {
  const { t } = useTranslation()
  return (
    <Field>
      <FieldLabel className="text-ui">{label}</FieldLabel>
      <div className="flex items-center gap-2">
        <ModelPicker
          agentName=""
          variant="outline"
          showReasoningEffort={false}
          disabled={disabled}
          value={
            value
              ? {
                  ref: {
                    provider_id: value.provider,
                    model_id: value.model || "default",
                  },
                }
              : undefined
          }
          onChange={({ ref }) =>
            onChange({ provider: ref.provider_id, model: ref.model_id })
          }
        />
        {value && (
          <Button
            type="button"
            variant="ghost"
            className="text-ui"
            disabled={disabled}
            onClick={() => onChange(undefined)}
          >
            {t("settings.agents.clearModel")}
          </Button>
        )}
      </div>
    </Field>
  )
}

function agentEnvRows(env?: Record<string, string>) {
  return Object.entries(env ?? {}).map(([name, value]) => ({ name, value }))
}

function agentListInputs(agent: AgentConfig) {
  return {
    tools: (agent.tools ?? []).join(" "),
    skills: (agent.skills ?? []).join(" "),
    mcp_servers: (agent.mcp_servers ?? []).join(" "),
  }
}

function AgentEditor({
  saved,
  creating,
  path,
  existingAgentNames = [],
}: {
  saved: AgentConfig
  creating: boolean
  path: string
  existingAgentNames?: string[]
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const connected = useStore(connection.$state) === "connected"
  const navigate = useNavigate()
  const save = useMutation(setAgentOptions(connection))
  const remove = useMutation(deleteAgentOptions(connection))
  const presets = useQuery({
    ...agentPresetCatalogOptions(connection),
    enabled: creating,
  })
  const [presetName, setPresetName] = useState("")
  const [presetSearch, setPresetSearch] = useState("")
  const [baseline, setBaseline] = useState(saved)
  const [draft, setDraft] = useState(saved)
  const [env, setEnv] = useState(() => agentEnvRows(saved.acp?.env))
  // 输入保留编辑中的空格，保存时按名字拆分。
  const [names, setNames] = useState(() => agentListInputs(saved))
  const [deleting, setDeleting] = useState(false)
  const [leaving, setLeaving] = useState(false)
  const type = draft.type ?? "native"
  const external = type !== "native"
  const canWrite = connected && !leaving && !save.isPending && !remove.isPending
  const name = draft.name.trim()
  const duplicateName =
    creating &&
    (save.isIdle || save.isError) &&
    existingAgentNames.includes(name)
  const environment = Object.fromEntries(
    env.map(({ name, value }) => [name.trim(), value]).filter(([name]) => name)
  )
  const dirty =
    JSON.stringify(draft) !== JSON.stringify(baseline) ||
    JSON.stringify(environment) !== JSON.stringify(baseline.acp?.env ?? {}) ||
    env.length !== Object.keys(environment).length
  function change(patch: Partial<AgentConfig>) {
    setDraft((previous) => ({ ...previous, ...patch }))
  }
  function changeACP(patch: Partial<NonNullable<AgentConfig["acp"]>>) {
    change({ acp: { command: "", ...draft.acp, ...patch } })
  }
  const presetOptions = (presets.data?.presets ?? []).map((preset) => ({
    id: preset.name,
    label: preset.display_name,
    description: preset.command,
    icon: <BrandIcon icon={preset.icon} />,
  }))
  function choosePreset(name: string) {
    setPresetName(name)
    const preset = presets.data?.presets.find((preset) => preset.name === name)
    if (!preset) return
    change({
      name: getAgentPresetName(name, existingAgentNames),
      type: "acp",
      description: preset.description,
      acp: {
        command: preset.command,
        args: preset.args,
        env: preset.env,
        ephemeral_param_overwrite: preset.ephemeral_param_overwrite,
      },
    })
    setEnv(agentEnvRows(preset.env))
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!canWrite || duplicateName) return
    const next: AgentConfig = {
      ...draft,
      name,
      type,
      tools: draft.tools?.length ? draft.tools : undefined,
      skills: draft.skills?.length ? draft.skills : undefined,
      mcp_servers: draft.mcp_servers?.length ? draft.mcp_servers : undefined,
    }
    if (type === "acp") {
      next.acp = {
        ...draft.acp,
        command: draft.acp?.command.trim() ?? "",
        args: draft.acp?.args,
        env: environment,
      }
      delete next.model
      delete next.compact_model
      delete next.instructions
      delete next.max_steps
    } else if (!external) {
      delete next.acp
    }
    save.mutate(next, {
      onSuccess: (result) => {
        setBaseline(next)
        setDraft(next)
        notifyDefinitionsUpdated(t("settings.agents.saved"), result, t)
        if (creating) setLeaving(true)
      },
    })
  }

  const fields = [
    {
      key: "tools",
      label: external
        ? t("settings.agents.externalTools")
        : t("settings.agents.tools"),
    },
    {
      key: "skills",
      label: external
        ? t("settings.agents.externalSkills")
        : t("settings.agents.skills"),
    },
    { key: "mcp_servers", label: "MCP" },
  ] as const
  function revert() {
    setDraft(baseline)
    setEnv(agentEnvRows(baseline.acp?.env))
    setNames(agentListInputs(baseline))
    save.reset()
  }
  return (
    <div className="flex min-w-0 flex-col gap-4">
      <UnsavedChangesGuard dirty={dirty && !leaving} />
      <SettingsDraftActions
        dirty={dirty}
        saving={save.isPending}
        saveDisabled={!canWrite}
        saveLabel={t("settings.agents.save")}
        form="agent-editor"
        onRevert={creating ? undefined : revert}
      />
      {leaving && <Navigate to={path} replace />}
      {save.isError && <RequestError error={save.error} />}
      <form
        id="agent-editor"
        className="flex min-w-0 flex-col gap-4"
        onSubmit={submit}
      >
        <ConfigurationLayout title={t("settings.agents.definition")}>
          <fieldset disabled={!canWrite} className="grid min-w-0 gap-4">
            {creating && (
              <EntityCombobox
                label={t("settings.agents.preset")}
                placeholder={t("settings.agents.presetPlaceholder")}
                value={presetName}
                onChange={choosePreset}
                options={presetOptions.filter((option) =>
                  [option.id, option.label, option.description]
                    .join(" ")
                    .toLowerCase()
                    .includes(presetSearch.toLowerCase())
                )}
                selectedOptions={presetOptions.filter(
                  (option) => option.id === presetName
                )}
                loading={presets.isPending}
                error={presets.error}
                shouldShowError={shouldShowLoadError}
                onRetry={() => void presets.refetch()}
                onSearch={setPresetSearch}
              />
            )}
            {creating && (
              <Field>
                <FieldLabel htmlFor="agent-type" className="text-ui">
                  {t("settings.agents.type")}
                </FieldLabel>
                <Select
                  value={type}
                  onValueChange={(type) => {
                    if (type === "native" || type === "acp") change({ type })
                  }}
                  items={[
                    { value: "native", label: t("settings.agents.native") },
                    { value: "acp", label: "ACP" },
                  ]}
                >
                  <SelectTrigger id="agent-type" className="text-ui">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="native">
                      {t("settings.agents.native")}
                    </SelectItem>
                    <SelectItem value="acp">ACP</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
            )}
            {creating && (
              <FormInput
                id="agent-name"
                label={t("settings.agents.name")}
                aria-invalid={duplicateName || undefined}
                error={
                  duplicateName
                    ? t("settings.agents.nameDuplicate", { name })
                    : undefined
                }
                value={draft.name}
                required
                onChange={(e) => change({ name: e.target.value })}
              />
            )}
            <FormInput
              id="agent-description"
              label={t("settings.agents.description")}
              value={draft.description ?? ""}
              onChange={(e) => change({ description: e.target.value })}
            />
            {type === "acp" && (
              <>
                <FormInput
                  id="agent-command"
                  label={t("settings.agents.command")}
                  value={draft.acp?.command ?? ""}
                  required
                  onChange={(e) => changeACP({ command: e.target.value })}
                />
                <Field>
                  <FieldLabel htmlFor="agent-args" className="text-ui">
                    {t("settings.agents.args")}
                  </FieldLabel>
                  <Textarea
                    id="agent-args"
                    className="text-ui"
                    value={draft.acp?.args?.join("\n") ?? ""}
                    onChange={(e) =>
                      changeACP({
                        args: e.target.value
                          ? e.target.value.split("\n")
                          : undefined,
                      })
                    }
                  />
                </Field>
                <Field>
                  <FieldLabel className="text-ui">
                    {t("settings.agents.env")}
                  </FieldLabel>
                  <KeyValueRows
                    rows={env}
                    onChange={setEnv}
                    nameLabel={t("settings.providerEditor.headerName")}
                    valueLabel={t("settings.providerEditor.headerValue")}
                    addLabel={t("settings.agents.addEnv")}
                    removeLabel={t("settings.agents.removeEnv")}
                  />
                </Field>
              </>
            )}
            {!external && (
              <>
                <div className="grid min-w-0 gap-4 sm:grid-cols-2">
                  <AgentModelField
                    label={t("settings.agents.model")}
                    value={draft.model}
                    disabled={!canWrite}
                    onChange={(model) => change({ model })}
                  />
                  <AgentModelField
                    label={t("settings.agents.compactModel")}
                    value={draft.compact_model}
                    disabled={!canWrite}
                    onChange={(compact_model) => change({ compact_model })}
                  />
                </div>
                <Field>
                  <FieldLabel htmlFor="agent-instructions" className="text-ui">
                    {t("settings.agents.instructions")}
                  </FieldLabel>
                  <Textarea
                    id="agent-instructions"
                    rows={5}
                    className="text-ui"
                    value={draft.instructions ?? ""}
                    onChange={(e) => change({ instructions: e.target.value })}
                  />
                </Field>
                <FormInput
                  id="agent-max-steps"
                  type="number"
                  min="0"
                  label={t("settings.agents.maxSteps")}
                  value={draft.max_steps ?? ""}
                  onChange={(e) =>
                    change({
                      max_steps: e.target.value
                        ? Number(e.target.value)
                        : undefined,
                    })
                  }
                />
              </>
            )}
            {fields.map(({ key, label }) => (
              <div key={key} className="flex flex-col gap-1">
                <FormInput
                  id={"agent-" + key}
                  label={label}
                  value={names[key]}
                  onChange={(event) => {
                    const raw = event.target.value
                    setNames({ ...names, [key]: raw })
                    const values = raw.trim().split(/\s+/).filter(Boolean)
                    change({ [key]: values.length ? values : undefined })
                  }}
                />
                {external && key !== "mcp_servers" && (
                  <FieldDescription className="text-caption">
                    {t("settings.agents.externalHint")}
                  </FieldDescription>
                )}
              </div>
            ))}
          </fieldset>
        </ConfigurationLayout>
      </form>
      {!creating && saved.name !== "default" && (
        <DangerZone
          actionLabel={t("settings.agents.delete")}
          disabled={!canWrite || dirty}
          disabledReason={
            !connected
              ? t("common.errors.connectionLost")
              : dirty
                ? t("settings.providerEditor.dirtyActions")
                : undefined
          }
          onAction={() => setDeleting(true)}
        />
      )}
      <ConfigurationChangeDialog
        open={deleting}
        onOpenChange={setDeleting}
        title={t("settings.agents.confirmDelete", { name: saved.name })}
        description={t("settings.agents.deleteWarning")}
        preview={remove.isError && <RequestError error={remove.error} />}
        confirmLabel={t("settings.agents.delete")}
        destructive
        pending={remove.isPending}
        confirmDisabled={!connected}
        onConfirm={() =>
          remove.mutate(saved.name, {
            onSuccess: (result) => {
              notifyDefinitionsUpdated(t("settings.agents.deleted"), result, t)
              void navigate(path, { replace: true })
            },
          })
        }
      />
    </div>
  )
}
