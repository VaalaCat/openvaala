import { useState } from "react"
import { atom } from "nanostores"
import { useStore } from "@nanostores/react"
import { AlertCircleIcon, CheckCircle2Icon, PlusIcon } from "lucide-react"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import { ServerListLabel } from "@/components/server-list-label"
import { SecretInput } from "@/components/base/secret-input"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import type {
  ConnectionState,
  ServerConnection,
} from "@/lib/api/server-connection"
import { Spinner } from "@/components/ui/spinner"
import {
  $activeServerURL,
  getServerDisplay,
  parseServerInput,
  removeServer,
  connectToServer,
  testServerConnection,
  upsertServer,
  type ParseServerInputError,
  type ServerEntry,
  type ServerTestResult,
} from "@/lib/api/servers"

const $noConnection = atom<ConnectionState | undefined>(undefined)

interface FormState {
  editingURL?: string
  name: string
  url: string
  token: string
}

const emptyForm: FormState = {
  name: "",
  url: "",
  token: "",
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
  initialMode = "list",
  entries,
  onSwitch,
  connection,
}: {
  connection?: ServerConnection
  open: boolean
  onOpenChange: (open: boolean) => void
  pageURL: string
  onSwitch?: () => void
  entries: ServerEntry[]
  initialMode?: "list" | "form"
}) {
  const { t } = useTranslation()
  const state = useStore(connection?.$state ?? $noConnection)
  const activeServerURL = useStore($activeServerURL)
  const activeURL = activeServerURL || pageURL

  const [mode, setMode] = useState(initialMode)
  const [form, setForm] = useState<FormState>(emptyForm)
  const parsed = parseServerInput(form.url, form.token)
  const worldTarget = parsed.ok ? parsed.worldTarget : undefined
  const urlError = form.url.trim() && !parsed.ok ? parsed.error : undefined
  const [deleteConfirmURL, setDeleteConfirmURL] = useState<string | null>(null)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<ServerTestResult | null>(null)

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setMode(initialMode)
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
      const address = parseServerInput(entry.url)
      setForm({
        editingURL: entry.url,
        name: entry.name || (address.ok && address.worldTarget?.name) || "",
        url: entry.url,
        token: entry.token,
      })
    } else {
      setForm(emptyForm)
    }
    setMode("form")
  }

  function handleAddressChange(rawValue: string) {
    setTestResult(null)
    const next = parseServerInput(rawValue)
    const sameAddress = parsed.ok && next.ok && parsed.url === next.url
    const token = sameAddress ? form.token : ""
    setForm({
      ...form,
      url: next.ok && next.extractedToken ? next.url : rawValue,
      token: next.ok && next.extractedToken ? next.token : token,
      name:
        form.name.trim() || !next.ok
          ? form.name
          : (next.worldTarget?.name ?? ""),
    })
  }

  async function handleTest() {
    if (!parsed.ok) return
    setTesting(true)
    setTestResult(null)
    const result = await testServerConnection(parsed)
    setTesting(false)
    setTestResult(result)
  }

  function handleSave(switchNow: boolean) {
    if (!parsed.ok) return
    upsertServer({
      previousURL: form.editingURL,
      url: parsed.url,
      token: parsed.token,
      name: form.name || (form.editingURL ? "" : undefined),
    })
    if (switchNow) {
      handleSwitch(parsed.url)
      return
    }
    setMode("list")
    setForm(emptyForm)
    setTestResult(null)
  }

  function handleSwitch(url: string) {
    connectToServer(url, pageURL, connection)
    handleOpenChange(false)
    onSwitch?.()
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
    ? entries.find((s) => s.url === deleteConfirmURL)
    : undefined

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto text-body sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="text-section">{title}</DialogTitle>
        </DialogHeader>

        {mode === "list" ? (
          <>
            <div className="divide-y divide-border border border-border">
              {entries.map((server) => {
                const isActive = activeURL === server.url
                return (
                  <div
                    key={server.url}
                    className="flex items-center gap-1 px-2 py-1"
                  >
                    <Button
                      variant="ghost"
                      className="h-auto min-w-0 flex-1 justify-start py-2 text-ui"
                      aria-description={t("connection.servers.connect")}
                      onClick={() => handleSwitch(server.url)}
                    >
                      <ServerListLabel
                        server={server}
                        disconnected={isActive && state === "reconnecting"}
                      />
                      {isActive && (
                        <Badge
                          variant="secondary"
                          className="ml-auto shrink-0 text-caption"
                        >
                          {t("connection.servers.current")}
                        </Badge>
                      )}
                    </Button>
                    {server.url !== pageURL && (
                      <>
                        <Button
                          className="text-ui"
                          size="sm"
                          variant="ghost"
                          onClick={() => openForm(server)}
                        >
                          {t("connection.servers.edit")}
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          className="text-ui color-destructive"
                          onClick={() => handleDelete(server.url, isActive)}
                        >
                          {t("connection.servers.delete")}
                        </Button>
                      </>
                    )}
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
                      name: getServerDisplay(deleteConfirmServer).name,
                    })}
                  </span>
                  <div className="flex justify-end gap-2">
                    <Button
                      className="text-ui"
                      size="sm"
                      variant="outline"
                      onClick={() => setDeleteConfirmURL(null)}
                    >
                      {t("common.actions.cancel")}
                    </Button>
                    <Button
                      className="text-ui"
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

            <DialogFooter>
              <Button
                className="text-ui"
                variant="outline"
                onClick={() => handleOpenChange(false)}
              >
                {t("connection.servers.close")}
              </Button>
              <Button className="text-ui" onClick={() => openForm()}>
                <PlusIcon data-icon="inline-start" />
                {t("connection.servers.add")}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <Field data-invalid={urlError ? true : undefined}>
              <FieldLabel className="text-ui" htmlFor="remote-server-url">
                {t("connection.servers.addressLabel")}
              </FieldLabel>
              <Input
                className="text-ui"
                id="remote-server-url"
                value={form.url}
                placeholder={t("connection.servers.addressPlaceholder")}
                onChange={(e) => handleAddressChange(e.target.value)}
              />
              {urlError ? (
                <FieldError className="text-caption">
                  {t(serverAddressErrorKeys[urlError])}
                </FieldError>
              ) : parsed.ok && parsed.url !== form.url.trim() ? (
                <FieldDescription className="text-code">
                  → {parsed.url}
                </FieldDescription>
              ) : null}
            </Field>

            <Field>
              <FieldLabel className="text-ui" htmlFor="remote-server-name">
                {t("connection.servers.nameLabel")}
              </FieldLabel>
              <Input
                className="text-ui"
                id="remote-server-name"
                value={form.name}
                placeholder={t("connection.servers.namePlaceholder")}
                onChange={(e) =>
                  setForm((prev) => ({ ...prev, name: e.target.value }))
                }
              />
            </Field>

            {form.url.trim() && !worldTarget && (
              <SecretInput
                id="remote-server-token"
                label={t("connection.servers.tokenLabel")}
                value={form.token}
                onChange={(token) => {
                  setTestResult(null)
                  setForm({ ...form, token })
                }}
              />
            )}
            {parsed.ok &&
              !worldTarget &&
              /^(?:127\.0\.0\.1|localhost)$/.test(
                new URL(parsed.url).hostname
              ) && (
                <FieldDescription className="text-caption">
                  {t("connection.servers.loopback")}
                </FieldDescription>
              )}

            {testResult?.ok && (
              <span className="flex items-center gap-1 text-caption color-success">
                <CheckCircle2Icon className="size-3.5" />
                {t("connection.servers.testSuccess")}
              </span>
            )}
            {testResult && !testResult.ok && (
              <span className="flex items-center gap-1 text-caption color-destructive">
                <AlertCircleIcon className="size-3.5" />
                {testResult.detail || t(serverTestErrorKeys[testResult.reason])}
              </span>
            )}

            <DialogFooter className="flex-row flex-wrap">
              <Button
                className="mr-auto text-ui"
                type="button"
                variant="outline"
                disabled={testing || !form.url.trim()}
                onClick={handleTest}
              >
                {testing && <Spinner className="size-3.5" />}
                {testing
                  ? t("connection.servers.testing")
                  : t("connection.servers.test")}
              </Button>
              <Button
                type="button"
                className="text-ui"
                variant="outline"
                onClick={() => {
                  setMode("list")
                  setTestResult(null)
                }}
              >
                {t("connection.servers.back")}
              </Button>
              <Button
                className="text-ui"
                type="button"
                variant="outline"
                disabled={!form.url.trim()}
                onClick={() => handleSave(false)}
              >
                {t("connection.servers.saveOnly")}
              </Button>
              <Button
                className="text-ui"
                type="button"
                disabled={!form.url.trim()}
                onClick={() => handleSave(true)}
              >
                {t("connection.servers.saveAndSwitch")}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  )
}
