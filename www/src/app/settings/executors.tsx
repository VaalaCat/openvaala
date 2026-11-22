import { useState, type FormEvent } from "react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import {
  AlertCircleIcon,
  CheckCircle2Icon,
  PlusIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { Link, useNavigate, useSearchParams } from "react-router"
import { ConfigurationLayout } from "@/components/base/configuration-layout"
import { DangerZone } from "@/components/base/danger-zone"
import { DetailPageState } from "@/components/base/detail-page-state"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ListEmpty } from "@/components/base/list-empty"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { PageActions, PageLayout } from "@/components/base/page-layout"
import { QueryState } from "@/components/base/query-state"
import { RequestError } from "@/components/base/request-error"
import { RowList, RowListItem } from "@/components/base/row-list"
import { UnsavedChangesGuard } from "@/components/base/unsaved-changes-guard"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { useServerConnection } from "@/components/server-connection-provider"
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import type { ExecutorConfig, SSHConfigHost } from "@/lib/api/protocol.gen"
import { notifyDefinitionsUpdated, notifyFailure } from "@/lib/notify"
import {
  getExecutorCreatePath,
  getExecutorEditPath,
  getSettingsPath,
} from "@/lib/paths"
import {
  deleteExecutorOptions,
  executorConfigListOptions,
  executorListOptions,
  setExecutorOptions,
  sshConfigHostsOptions,
  testSSHExecutorOptions,
} from "@/lib/query/executor"
import { shouldShowLoadError } from "@/lib/rpc-error"

const builtinExecutorIDs = new Set(["local", "openvaala", "memory"])
const executorIDPattern = /^[A-Za-z0-9._-]+$/

interface SSHExecutorDraft {
  id: string
  description: string
  address: string
  identityFile: string
  workspaceDirectory: string
}

function toDraft(cfg?: ExecutorConfig): SSHExecutorDraft {
  return {
    id: cfg?.id ?? "",
    description: cfg?.description ?? "",
    address: cfg?.ssh?.address ?? "",
    identityFile: cfg?.ssh?.identity_file ?? "",
    workspaceDirectory: cfg?.workspace_directory ?? "",
  }
}

function isSameDraft(a: SSHExecutorDraft, b: SSHExecutorDraft): boolean {
  return (
    a.id === b.id &&
    a.description === b.description &&
    a.address === b.address &&
    a.identityFile === b.identityFile &&
    a.workspaceDirectory === b.workspaceDirectory
  )
}

// Executor 列表：内置项带徽标只读展示，配置的 SSH Executor 支持点选编辑和右上新增。
export function ExecutorSettingsPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const serverID = connection.getServerID()
  const executors = useQuery(executorListOptions(connection))
  const executorConfigs = useQuery(executorConfigListOptions(connection))

  const configByID = new Map(
    (executorConfigs.data?.executors ?? []).map((cfg) => [cfg.id, cfg])
  )

  return (
    <SettingsLayout
      section="executors"
      actions={
        <Button
          className="text-ui"
          nativeButton={false}
          render={<Link to={getExecutorCreatePath(serverID)} />}
        >
          <PlusIcon data-icon="inline-start" />
          {t("settings.executors.addSSH")}
        </Button>
      }
    >
      <QueryState shouldShowError={shouldShowLoadError} query={executors}>
        {({ executors: list }) =>
          list.length === 0 ? (
            <ListEmpty title={t("settings.executors.empty")} />
          ) : (
            <RowList>
              {list.map((executor) => {
                const cfg = configByID.get(executor.id)
                const isBuiltin = builtinExecutorIDs.has(executor.id)
                const isEditableSSH = !isBuiltin && cfg?.kind === "ssh"
                const subtitleParts = [
                  cfg?.ssh?.address,
                  executor.workspace_directory
                    ? t("settings.executors.workspaceDirectory", {
                        directory: executor.workspace_directory,
                      })
                    : t("settings.executors.noWorkspaceDirectory"),
                ].filter(Boolean)

                return (
                  <RowListItem
                    key={executor.id}
                    to={
                      isEditableSSH
                        ? getExecutorEditPath(serverID, executor.id)
                        : undefined
                    }
                  >
                    <div className="flex min-w-0 items-center gap-2">
                      <OverflowTooltip
                        render={<span className="min-w-0 truncate" />}
                      >
                        {executor.id}
                      </OverflowTooltip>
                      <Badge variant="secondary" className="shrink-0">
                        {isBuiltin
                          ? t("settings.executors.builtinBadge")
                          : executor.kind === "ssh"
                            ? "SSH"
                            : executor.kind}
                      </Badge>
                      {executor.description && (
                        <OverflowTooltip
                          render={
                            <span className="ml-auto max-w-1/2 min-w-0 truncate text-caption color-muted" />
                          }
                        >
                          {executor.description}
                        </OverflowTooltip>
                      )}
                    </div>
                    <OverflowTooltip
                      render={<p className="truncate text-table color-muted" />}
                    >
                      {subtitleParts.join(" · ")}
                    </OverflowTooltip>
                  </RowListItem>
                )
              })}
            </RowList>
          )
        }
      </QueryState>
    </SettingsLayout>
  )
}

// 新增 SSH Executor 页
export function ExecutorCreatePage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const serverID = connection.getServerID()
  const listPath = getSettingsPath(serverID, "executors")
  const parents = [
    { label: t("settings.title") },
    { label: t("settings.sections.executors"), to: listPath },
  ]
  const configs = useQuery(executorConfigListOptions(connection))
  const existingIDs = (configs.data?.executors ?? []).map((e) => e.id)

  return (
    <PageLayout title={t("settings.executors.createTitle")} parents={parents}>
      <SSHExecutorEditor
        mode="create"
        existingIDs={existingIDs}
        listPath={listPath}
      />
    </PageLayout>
  )
}

// 编辑 SSH Executor 页
export function ExecutorEditPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const serverID = connection.getServerID()
  const [searchParams] = useSearchParams()
  const executorID = searchParams.get("id") ?? ""
  const listPath = getSettingsPath(serverID, "executors")
  const parents = [
    { label: t("settings.title") },
    { label: t("settings.sections.executors"), to: listPath },
  ]
  const configs = useQuery(executorConfigListOptions(connection))
  const saved = configs.data?.executors.find((e) => e.id === executorID)

  return (
    <DetailPageState
      shouldShowError={shouldShowLoadError}
      id={executorID}
      title={executorID || t("settings.sections.executors")}
      entity={t("settings.sections.executors")}
      parents={parents}
      notFoundReturn={{
        to: listPath,
        label: t("settings.executors.backToList"),
      }}
      missingIdText={t("settings.executors.missingID")}
      query={{
        data: saved,
        error: configs.error,
        refetch: () => void configs.refetch(),
      }}
      notFound={!!configs.data && !saved}
    >
      {(executor) => (
        <PageLayout title={executor.id} parents={parents}>
          {configs.isError && shouldShowLoadError(configs.error) && (
            <RequestError
              error={configs.error}
              onRetry={() => void configs.refetch()}
            />
          )}
          <SSHExecutorEditor
            key={executor.id}
            mode="edit"
            saved={executor}
            existingIDs={(configs.data?.executors ?? []).map((e) => e.id)}
            listPath={listPath}
          />
        </PageLayout>
      )}
    </DetailPageState>
  )
}

// SSH Executor 编辑器：支持从 serve 机器 ~/.ssh/config 导入、测试连接、原子保存、删除与离开拦截。
function SSHExecutorEditor({
  mode,
  saved,
  existingIDs,
  listPath,
}: {
  mode: "create" | "edit"
  saved?: ExecutorConfig
  existingIDs: string[]
  listPath: string
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const navigate = useNavigate()
  const isCreating = mode === "create"

  const initialDraft = toDraft(saved)
  const [baseline, setBaseline] = useState<SSHExecutorDraft>(initialDraft)
  const [draft, setDraft] = useState<SSHExecutorDraft>(initialDraft)
  const [selectedSSHHost, setSelectedSSHHost] = useState<string>("")
  const [deleteDialogOpen, setDeleteDialogOpen] = useState(false)
  const [testResult, setTestResult] = useState<
    { ok: true; resolvedDirectory: string } | { ok: false; error: string } | null
  >(null)

  const sshHostsQuery = useQuery({
    ...sshConfigHostsOptions(connection),
    enabled: isCreating,
  })
  const sshHosts: SSHConfigHost[] = sshHostsQuery.data?.hosts ?? []

  const save = useMutation(setExecutorOptions(connection))
  const remove = useMutation(deleteExecutorOptions(connection))
  const testSSH = useMutation(testSSHExecutorOptions(connection))

  const dirty = !isSameDraft(draft, baseline)
  const trimmedID = draft.id.trim()
  const idIssue = isCreating
    ? !trimmedID
      ? undefined
      : builtinExecutorIDs.has(trimmedID)
        ? t("settings.executors.errors.builtinID")
        : !executorIDPattern.test(trimmedID)
          ? t("settings.executors.errors.invalidID")
          : existingIDs.includes(trimmedID)
            ? t("settings.executors.errors.duplicateID")
            : undefined
    : undefined

  const canTest =
    isConnected &&
    !testSSH.isPending &&
    draft.address.trim() !== "" &&
    draft.workspaceDirectory.trim() !== ""

  const canSave =
    isConnected &&
    !save.isPending &&
    !idIssue &&
    trimmedID !== "" &&
    draft.description.trim() !== "" &&
    draft.address.trim() !== "" &&
    draft.workspaceDirectory.trim() !== "" &&
    (isCreating || dirty)

  function updateDraft(patch: Partial<SSHExecutorDraft>) {
    setDraft((prev) => ({ ...prev, ...patch }))
    if (
      patch.address !== undefined ||
      patch.identityFile !== undefined ||
      patch.workspaceDirectory !== undefined
    )
      setTestResult(null)
  }

  function handleImportSSHHost(alias: string | null) {
    if (!alias) return
    const host = sshHosts.find((item) => item.alias === alias)
    if (!host) return
    setSelectedSSHHost(alias)
    setTestResult(null)
    setDraft((prev) => ({
      ...prev,
      id:
        isCreating && (!prev.id.trim() || prev.id === selectedSSHHost)
          ? host.alias
          : prev.id,
      address: host.address,
      identityFile: host.identity_file ?? "",
    }))
  }

  function handleTest() {
    setTestResult(null)
    testSSH.mutate(
      {
        ssh: {
          address: draft.address.trim(),
          ...(draft.identityFile.trim()
            ? { identity_file: draft.identityFile.trim() }
            : {}),
        },
        workspace_directory: draft.workspaceDirectory.trim(),
      },
      {
        onSuccess: (result) =>
          setTestResult({
            ok: true,
            resolvedDirectory: result.resolved_directory,
          }),
        onError: (error) =>
          setTestResult({
            ok: false,
            error: error instanceof Error ? error.message : String(error),
          }),
      }
    )
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    if (!canSave) return
    const nextConfig: ExecutorConfig = {
      id: trimmedID,
      kind: "ssh",
      description: draft.description.trim(),
      workspace_directory: draft.workspaceDirectory.trim(),
      ssh: {
        address: draft.address.trim(),
        ...(draft.identityFile.trim()
          ? { identity_file: draft.identityFile.trim() }
          : {}),
      },
    }
    save.mutate(nextConfig, {
      onSuccess: (result) => {
        const nextBaseline = toDraft(nextConfig)
        setBaseline(nextBaseline)
        setDraft(nextBaseline)
        notifyDefinitionsUpdated(
          t("settings.executors.saved", { id: nextConfig.id }),
          result,
          t
        )
        if (isCreating) void navigate(listPath, { replace: true })
      },
      onError: (error) => notifyFailure(error, t),
    })
  }

  function handleDelete() {
    if (!saved) return
    remove.mutate(saved.id, {
      onSuccess: (result) => {
        setDeleteDialogOpen(false)
        setBaseline(draft)
        notifyDefinitionsUpdated(
          t("settings.executors.deleted", { id: saved.id }),
          result,
          t
        )
        void navigate(listPath, { replace: true })
      },
      onError: (error) => notifyFailure(error, t),
    })
  }

  return (
    <>
      <UnsavedChangesGuard dirty={dirty && !save.isSuccess && !remove.isSuccess} />
      <PageActions>
        <Button
          type="button"
          variant="outline"
          className="text-ui"
          disabled={!dirty || save.isPending}
          onClick={() => {
            setDraft(baseline)
            setTestResult(null)
          }}
        >
          {t("settings.executors.revert")}
        </Button>
        <Button
          type="button"
          variant="outline"
          className="text-ui"
          disabled={!canTest}
          onClick={handleTest}
        >
          {testSSH.isPending && <Spinner className="size-3.5" />}
          {testSSH.isPending
            ? t("settings.executors.testing")
            : t("settings.executors.test")}
        </Button>
        <Button
          type="submit"
          form="ssh-executor-form"
          className="text-ui"
          disabled={!canSave}
        >
          {save.isPending && <LatticeLoader aria-label={t("common.loading")} />}
          {t("settings.executors.save")}
        </Button>
      </PageActions>

      <form
        id="ssh-executor-form"
        onSubmit={handleSubmit}
        className="mx-auto flex w-full max-w-2xl flex-col gap-4"
      >
        {save.error && <RequestError error={save.error} />}
        {remove.error && <RequestError error={remove.error} />}

        <ConfigurationLayout
          title={t("settings.executors.sshSection")}
          actions={
            isCreating && sshHosts.length > 0 ? (
              <Select
                value={selectedSSHHost}
                onValueChange={handleImportSSHHost}
              >
                <SelectTrigger className="h-7 w-48 text-caption">
                  <SelectValue
                    placeholder={t(
                      "settings.executors.importSSHConfigPlaceholder"
                    )}
                  />
                </SelectTrigger>
                <SelectContent>
                  {sshHosts.map((host) => (
                    <SelectItem key={host.alias} value={host.alias}>
                      {host.alias} ({host.address})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            ) : undefined
          }
        >
          <div className="grid gap-3 sm:grid-cols-2">
            <Field data-invalid={idIssue ? true : undefined}>
              <FieldLabel htmlFor="executor-id">
                {t("settings.executors.idLabel")}
              </FieldLabel>
              <Input
                id="executor-id"
                value={draft.id}
                readOnly={!isCreating}
                disabled={!isCreating}
                placeholder="dev-box"
                onChange={(e) => updateDraft({ id: e.target.value })}
              />
              {idIssue && <FieldError>{idIssue}</FieldError>}
            </Field>

            <Field>
              <FieldLabel htmlFor="executor-description">
                {t("settings.executors.descriptionLabel")}
              </FieldLabel>
              <Input
                id="executor-description"
                value={draft.description}
                placeholder={t("settings.executors.descriptionPlaceholder")}
                onChange={(e) => updateDraft({ description: e.target.value })}
              />
            </Field>
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <Field>
              <FieldLabel htmlFor="executor-ssh-address">
                {t("settings.executors.addressLabel")}
              </FieldLabel>
              <Input
                id="executor-ssh-address"
                value={draft.address}
                placeholder="user@host:22"
                onChange={(e) => updateDraft({ address: e.target.value })}
              />
            </Field>

            <Field>
              <FieldLabel htmlFor="executor-ssh-identity">
                {t("settings.executors.identityFileLabel")}
              </FieldLabel>
              <Input
                id="executor-ssh-identity"
                value={draft.identityFile}
                placeholder={t("settings.executors.identityFilePlaceholder")}
                onChange={(e) => updateDraft({ identityFile: e.target.value })}
              />
            </Field>
          </div>

          <Field>
            <FieldLabel htmlFor="executor-workspace-dir">
              {t("settings.executors.workspaceDirectoryLabel")}
            </FieldLabel>
            <Input
              id="executor-workspace-dir"
              value={draft.workspaceDirectory}
              placeholder="/home/user/workspace"
              onChange={(e) =>
                updateDraft({ workspaceDirectory: e.target.value })
              }
            />
          </Field>

          {testResult?.ok && (
            <div className="flex items-center gap-1.5 text-caption text-emerald-600 dark:text-emerald-400">
              <CheckCircle2Icon className="size-4 shrink-0" />
              <span>
                {t("settings.executors.testSuccess", {
                  directory: testResult.resolvedDirectory,
                })}
              </span>
            </div>
          )}
          {testResult && !testResult.ok && (
            <div className="flex items-start gap-1.5 text-caption text-destructive">
              <AlertCircleIcon className="mt-0.5 size-4 shrink-0" />
              <span className="wrap-anywhere">{testResult.error}</span>
            </div>
          )}
        </ConfigurationLayout>

        {!isCreating && saved && (
          <DangerZone
            actionLabel={t("settings.executors.deleteAction")}
            disabled={!isConnected || remove.isPending}
            onAction={() => setDeleteDialogOpen(true)}
          />
        )}
      </form>

      <AlertDialog open={deleteDialogOpen} onOpenChange={setDeleteDialogOpen}>
        <AlertDialogContent className="text-body">
          <AlertDialogHeader>
            <AlertDialogTitle className="text-section">
              {t("settings.executors.deleteConfirmTitle", {
                id: saved?.id ?? "",
              })}
            </AlertDialogTitle>
            <AlertDialogDescription className="text-caption">
              {t("settings.executors.deleteConfirmDescription", {
                id: saved?.id ?? "",
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <Button
              variant="outline"
              className="text-ui"
              disabled={remove.isPending}
              onClick={() => setDeleteDialogOpen(false)}
            >
              {t("common.actions.cancel")}
            </Button>
            <Button
              variant="destructive"
              className="text-ui"
              disabled={remove.isPending}
              onClick={handleDelete}
            >
              {t("settings.executors.deleteAction")}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
