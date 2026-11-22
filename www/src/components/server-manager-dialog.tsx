import { useState } from "react"
import { useStore } from "@nanostores/react"
import {
  AlertCircleIcon,
  CheckCircle2Icon,
  PlusIcon,
  ServerIcon,
} from "lucide-react"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import { SecretInput } from "@/components/base/secret-input"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import {
  $activeServerURL,
  $servers,
  getServerHost,
  parseServerInput,
  removeServer,
  setActiveServerURL,
  testServerConnection,
  upsertServer,
  type ParseServerInputError,
  type ServerEntry,
  type ServerTestResult,
} from "@/lib/api/servers"

interface FormState {
  editingURL?: string
  name: string
  url: string
  token: string
  parsedURL: string
  tokenAutoFilled: boolean
  urlError?: ParseServerInputError
}

const emptyForm: FormState = {
  name: "",
  url: "",
  token: "",
  parsedURL: "",
  tokenAutoFilled: false,
}

const serverAddressErrorKeys = {
  required: "connection.servers.errors.required",
  invalid: "connection.servers.errors.invalid",
  unsupportedScheme: "connection.servers.errors.unsupportedScheme",
  missingHost: "connection.servers.errors.missingHost",
} as const satisfies Record<ParseServerInputError, ParseKeys>

const serverTestErrorKeys = {
  unauthorized: "connection.servers.testErrors.unauthorized",
  incompatible: "connection.servers.testErrors.incompatible",
  unreachable: "connection.servers.testErrors.unreachable",
  timeout: "connection.servers.testErrors.timeout",
} as const satisfies Record<
  Extract<ServerTestResult, { ok: false }>["reason"],
  ParseKeys
>

// 「管理服务器」单弹窗：在同一弹窗内切换列表视图与新增/编辑表单视图，不叠双层弹窗。
export function ServerManagerDialog({
  open,
  onOpenChange,
  pageURL,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  pageURL: string
}) {
  const { t } = useTranslation()
  const servers = useStore($servers)
  const activeServerURL = useStore($activeServerURL)
  const activeURL = activeServerURL || pageURL
  const remoteServers = servers.filter((server) => server.url !== pageURL)

  const [mode, setMode] = useState<"list" | "form">("list")
  const [form, setForm] = useState<FormState>(emptyForm)
  const [deleteConfirmURL, setDeleteConfirmURL] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ServerTestResult | null>(null)

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setMode("list")
      setForm(emptyForm)
      setDeleteConfirmURL(null)
      setTesting(false)
      setTestResult(null)
    }
    onOpenChange(nextOpen)
  }

  function openForm(entry?: ServerEntry) {
    setDeleteConfirmURL(null)
    setTesting(false)
    setTestResult(null)
    if (entry) {
      setForm({
        editingURL: entry.url,
        name: entry.name ?? "",
        url: entry.url,
        token: entry.token,
        parsedURL: entry.url,
        tokenAutoFilled: false,
      })
    } else {
      setForm(emptyForm)
    }
    setMode("form")
  }

  function handleAddressChange(rawValue: string) {
    setTestResult(null)
    if (!rawValue.trim()) {
      setForm((prev) => ({
        ...prev,
        url: rawValue,
        parsedURL: "",
        urlError: undefined,
      }))
      return
    }
    const parsed = parseServerInput(rawValue, form.token)
    if (!parsed.ok) {
      setForm((prev) => ({
        ...prev,
        url: rawValue,
        parsedURL: "",
        urlError: parsed.error,
      }))
      return
    }
    setForm((prev) => ({
      ...prev,
      url: parsed.extractedToken ? parsed.url : rawValue,
      token: parsed.extractedToken ? parsed.token : prev.token,
      parsedURL: parsed.url,
      tokenAutoFilled: parsed.extractedToken ? true : prev.tokenAutoFilled,
      urlError: undefined,
    }))
  }

  async function handleTest() {
    const parsed = parseServerInput(form.url, form.token)
    if (!parsed.ok) {
      setForm((prev) => ({ ...prev, urlError: parsed.error }))
      return
    }
    setTesting(true)
    setTestResult(null)
    const result = await testServerConnection({
      url: parsed.url,
      token: parsed.token,
    })
    setTesting(false)
    setTestResult(result)
  }

  function handleSave(switchNow: boolean) {
    const parsed = parseServerInput(form.url, form.token)
    if (!parsed.ok) {
      setForm((prev) => ({ ...prev, urlError: parsed.error }))
      return
    }
    upsertServer({
      previousURL: form.editingURL,
      url: parsed.url,
      token: parsed.token,
      name: form.name,
    })
    if (switchNow) {
      setActiveServerURL(parsed.url, pageURL)
      handleOpenChange(false)
      return
    }
    setMode("list")
    setForm(emptyForm)
    setTestResult(null)
  }

  function handleSwitch(url: string) {
    setActiveServerURL(url, pageURL)
    handleOpenChange(false)
  }

  function handleDelete(url: string, isActive: boolean) {
    if (isActive) {
      setDeleteConfirmURL(url)
      return
    }
    removeServer(url)
  }

  const title =
    mode === "list"
      ? t("connection.servers.manageTitle")
      : form.editingURL
        ? t("connection.servers.editTitle")
        : t("connection.servers.addTitle")

  const deleteConfirmServer = deleteConfirmURL
    ? remoteServers.find((s) => s.url === deleteConfirmURL)
    : undefined

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>

        {mode === "list" ? (
          <div className="flex flex-col gap-3">
            <div className="divide-y divide-border border border-border">
              {/* 本页默认服务器 */}
              <div className="flex items-center justify-between gap-2 px-3 py-2.5">
                <div className="min-w-0">
                  <div className="flex items-center gap-1.5 text-ui font-medium">
                    <ServerIcon className="size-3.5 shrink-0 text-muted-foreground" />
                    <span className="truncate">
                      {t("connection.servers.pageDefault")}
                    </span>
                  </div>
                  <div className="truncate text-caption text-muted-foreground">
                    {getServerHost(pageURL)}
                  </div>
                </div>
                {activeURL === pageURL ? (
                  <Badge variant="secondary" className="shrink-0">
                    {t("connection.servers.current")}
                  </Badge>
                ) : (
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => handleSwitch(pageURL)}
                  >
                    {t("connection.servers.switch")}
                  </Button>
                )}
              </div>

              {/* 用户添加的远端服务器 */}
              {remoteServers.map((server) => {
                const isActive = activeURL === server.url
                const displayName = server.name || getServerHost(server.url)
                return (
                  <div
                    key={server.url}
                    className="flex items-center justify-between gap-2 px-3 py-2.5"
                  >
                    <div className="min-w-0">
                      <div className="truncate text-ui font-medium">
                        {displayName}
                      </div>
                      <div className="truncate text-caption text-muted-foreground">
                        {getServerHost(server.url)}
                      </div>
                    </div>
                    <div className="flex shrink-0 items-center gap-1.5">
                      {isActive ? (
                        <Badge variant="secondary">
                          {t("connection.servers.current")}
                        </Badge>
                      ) : (
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => handleSwitch(server.url)}
                        >
                          {t("connection.servers.switch")}
                        </Button>
                      )}
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => openForm(server)}
                      >
                        {t("connection.servers.edit")}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => handleDelete(server.url, isActive)}
                      >
                        {t("connection.servers.delete")}
                      </Button>
                    </div>
                  </div>
                )
              })}
            </div>

            {deleteConfirmServer && (
              <Alert variant="destructive">
                <AlertCircleIcon />
                <AlertDescription className="flex flex-col gap-2 text-caption">
                  <span>
                    {t("connection.servers.deleteActiveConfirm", {
                      name:
                        deleteConfirmServer.name ||
                        getServerHost(deleteConfirmServer.url),
                    })}
                  </span>
                  <div className="flex justify-end gap-2">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => setDeleteConfirmURL(null)}
                    >
                      {t("common.actions.cancel")}
                    </Button>
                    <Button
                      size="sm"
                      variant="destructive"
                      onClick={() => {
                        removeServer(deleteConfirmServer.url)
                        setDeleteConfirmURL(null)
                        handleOpenChange(false)
                      }}
                    >
                      {t("connection.servers.deleteConfirm")}
                    </Button>
                  </div>
                </AlertDescription>
              </Alert>
            )}

            <div className="flex items-center justify-between pt-1">
              <Button
                size="sm"
                variant="outline"
                onClick={() => openForm()}
              >
                <PlusIcon className="size-3.5" />
                {t("connection.servers.add")}
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => handleOpenChange(false)}
              >
                {t("connection.servers.close")}
              </Button>
            </div>
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            <Field>
              <FieldLabel htmlFor="remote-server-name">
                {t("connection.servers.nameLabel")}
              </FieldLabel>
              <Input
                id="remote-server-name"
                value={form.name}
                placeholder={t("connection.servers.namePlaceholder")}
                onChange={(e) =>
                  setForm((prev) => ({ ...prev, name: e.target.value }))
                }
              />
            </Field>

            <Field data-invalid={form.urlError ? true : undefined}>
              <FieldLabel htmlFor="remote-server-url">
                {t("connection.servers.addressLabel")}
              </FieldLabel>
              <Input
                id="remote-server-url"
                value={form.url}
                placeholder={t("connection.servers.addressPlaceholder")}
                onChange={(e) => handleAddressChange(e.target.value)}
              />
              {form.urlError ? (
                <FieldError>
                  {t(serverAddressErrorKeys[form.urlError])}
                </FieldError>
              ) : form.parsedURL && form.parsedURL !== form.url.trim() ? (
                <FieldDescription className="font-mono">
                  → {form.parsedURL}
                </FieldDescription>
              ) : null}
            </Field>

            <Field>
              <SecretInput
                id="remote-server-token"
                label={t("connection.servers.tokenLabel")}
                value={form.token}
                onChange={(token) => {
                  setTestResult(null)
                  setForm((prev) => ({
                    ...prev,
                    token,
                    tokenAutoFilled: false,
                  }))
                }}
              />
              {form.tokenAutoFilled && (
                <FieldDescription>
                  {t("connection.servers.tokenAutoFilled")}
                </FieldDescription>
              )}
            </Field>

            <div className="flex flex-wrap items-center gap-2 pt-1">
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={testing || !form.url.trim()}
                onClick={handleTest}
              >
                {testing && <Spinner className="size-3.5" />}
                {testing
                  ? t("connection.servers.testing")
                  : t("connection.servers.test")}
              </Button>
              {testResult?.ok && (
                <span className="flex items-center gap-1 text-caption text-emerald-600 dark:text-emerald-400">
                  <CheckCircle2Icon className="size-3.5" />
                  {t("connection.servers.testSuccess")}
                </span>
              )}
              {testResult && !testResult.ok && (
                <span className="flex items-center gap-1 text-caption text-destructive">
                  <AlertCircleIcon className="size-3.5" />
                  {t(serverTestErrorKeys[testResult.reason])}
                </span>
              )}
            </div>

            <div className="flex items-center justify-between pt-2">
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => {
                  setMode("list")
                  setTestResult(null)
                }}
              >
                {t("connection.servers.back")}
              </Button>
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  disabled={!form.url.trim()}
                  onClick={() => handleSave(false)}
                >
                  {t("connection.servers.saveOnly")}
                </Button>
                <Button
                  type="button"
                  size="sm"
                  disabled={!form.url.trim()}
                  onClick={() => handleSave(true)}
                >
                  {t("connection.servers.saveAndSwitch")}
                </Button>
              </div>
            </div>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
