import { useEffect, useId, useState, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import { QRCodeCanvas } from "qrcode.react"
import { CopyButton } from "@/components/base/copy-button"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { PlusIcon, XIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { useLocation } from "react-router"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { ConfigurationLayout } from "@/components/base/configuration-layout"
import { RowListItem } from "@/components/base/row-list"
import { FormInput } from "@/components/base/form-input"
import { SettingsDraftActions } from "@/components/settings/settings-layout"
import { StatusDot } from "@/components/base/status-dot"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { RequestError } from "@/components/base/request-error"
import { UnsavedChangesGuard } from "@/components/base/unsaved-changes-guard"
import { useServerConnection } from "@/components/server-connection-provider"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogFooter,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  Field,
  FieldLabel,
  FieldGroup,
  FieldDescription,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import type {
  WorldSettings,
  WorldNodeSettings,
  GetWorldResult,
} from "@/lib/api/protocol.gen"
import { getRemoteAccessPage } from "@/lib/api/world-transport"
import { saveServerToken } from "@/lib/api/servers"
import { agentCatalogOptions } from "@/lib/query/agent-settings"
import { worldStatusOptions } from "@/lib/query/world"
import { notifySuccess } from "@/lib/notify"

function Choice({
  label,
  hideLabel,
  action,
  value,
  options,
  onChange,
}: {
  action?: ReactNode
  label: string
  hideLabel?: boolean
  value: string
  options: string[]
  onChange: (v: string) => void
}) {
  const { t } = useTranslation()
  const id = useId()
  const items = options.map((v) => ({
    value: v,
    label: v || t("worldSettings.auto"),
  }))
  return (
    <Field className="min-w-0">
      <FieldLabel htmlFor={id} className={hideLabel ? "sr-only" : "text-ui"}>
        {label}
      </FieldLabel>
      <div className="flex min-w-0 items-center gap-2">
        <Select
          value={value}
          items={items}
          onValueChange={(v) => {
            if (v !== null) onChange(v)
          }}
        >
          <SelectTrigger id={id} className="w-full min-w-0 text-ui">
            <SelectValue className="truncate" />
          </SelectTrigger>
          <SelectContent alignItemWithTrigger={false} align="start">
            {items.map((v) => (
              <SelectItem key={v.value} value={v.value} className="text-table">
                {v.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {action}
      </div>
    </Field>
  )
}

export function WorldLocalSettings({
  restarting,
  localTabActive,
  setRestarting,
}: {
  restarting: boolean
  localTabActive: boolean
  setRestarting: (value: boolean) => void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const connected = useStore(connection.$state) === "connected"
  const query = useQuery({
    queryKey: [connection.getServerID(), "worldSettings"],
    queryFn: () => connection.call("config/getWorld", {}),
    enabled: connected,
  })
  const { refetch } = query
  useEffect(
    () =>
      connection.onConnected(() => {
        void refetch().then((result) => {
          if (result.isSuccess && restarting) {
            setRestarting(false)
            notifySuccess(t("worldSettings.restarted"))
          }
        })
      }),
    [connection, refetch, restarting, setRestarting, t]
  )
  if (restarting && !query.isError)
    return (
      <div
        role="status"
        className="flex min-h-60 items-center justify-center gap-2 p-4"
      >
        <LatticeLoader />
        {t("worldSettings.restarting")}
      </div>
    )
  if (query.isError)
    return (
      <RequestError
        error={query.error}
        onRetry={() => {
          void refetch().then((r) => {
            if (r.isSuccess) setRestarting(false)
          })
        }}
      />
    )
  if (!query.data) return <LatticeLoader aria-label={t("common.loading")} />
  return (
    <WorldEditor
      localTabActive={localTabActive}
      saved={query.data}
      onRestart={() => setRestarting(true)}
    />
  )
}

type LocalSection =
  | "node"
  | "relays"
  | "remoteAccess"
  | "relayMode"
  | "server"
  | "targets"
  | "direct"

// 按 config/setWorld 返回的校验原因定位表单；默认 Agent 必须同时出现在通告目标里。
function getWorldErrorSections(
  error: unknown,
  node: WorldNodeSettings
): LocalSection[] {
  if (!(error instanceof Error)) return []
  const reason = error.message
  if (reason.startsWith("invalid world.web_url")) return ["remoteAccess"]
  if (reason.startsWith("invalid world.relays URL")) return ["relays"]
  if (reason.startsWith("invalid world.node.direct_urls URL")) return ["direct"]
  if (/^(invalid )?world\.relay_mode/.test(reason)) return ["relayMode"]
  if (/^(world\.server\.domain|world server requires)/.test(reason))
    return ["server"]
  if (reason.startsWith("invalid exposed world target")) return ["targets"]
  if (reason === "world default target must be exposed")
    return ["node", "targets"]
  if (reason.startsWith("unknown agent ")) {
    return node.advertise_targets.some(
      (target) => reason === `unknown agent ${target.name} in world settings`
    )
      ? ["targets"]
      : ["node"]
  }
  if (reason.startsWith("world.node requires")) return ["node"]
  return []
}

function WorldEditor({
  saved,
  localTabActive,
  onRestart,
}: {
  saved: GetWorldResult
  localTabActive: boolean
  onRestart: () => void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const connected = useStore(connection.$state) === "connected"
  const agents = useQuery(agentCatalogOptions(connection))
  const { hash } = useLocation()
  const linkedSection = localTabActive
    ? hash === "#world-relays"
      ? "relays"
      : hash === "#world-node"
        ? "node"
        : undefined
    : undefined
  const [expanded, setExpanded] = useState<
    Partial<Record<LocalSection, boolean>>
  >(linkedSection ? { [linkedSection]: true } : {})
  const [lastLink, setLastLink] = useState(linkedSection)
  if (lastLink !== linkedSection) {
    setLastLink(linkedSection)
    if (linkedSection) setExpanded({ ...expanded, [linkedSection]: true })
  }
  useEffect(() => {
    if (linkedSection)
      document
        .getElementById("world-" + linkedSection)
        ?.scrollIntoView({ block: "start" })
  }, [linkedSection])
  const names = agents.data?.agents.map((a) => a.name) ?? []
  const base = saved.settings
  const [editedSettings, editSettings] = useState<WorldSettings>()
  const draft = editedSettings ?? base
  function setDraft(next: WorldSettings) {
    editSettings(
      JSON.stringify(next) === JSON.stringify(base) ? undefined : next
    )
  }
  const node = draft.node
  const registered = !!base.node.server_url
  const { data: statusSnapshot } = useQuery({
    ...worldStatusOptions(connection),
    enabled: connected && localTabActive,
    refetchInterval:
      localTabActive &&
      (saved.running.remote_access_enabled ||
        saved.running.node_enabled ||
        saved.running.server_enabled)
        ? 3000
        : false,
  })
  const liveNode =
    saved.running.node_enabled && statusSnapshot?.enabled
      ? statusSnapshot
      : undefined
  const nodeAddress = liveNode?.node_addr || `${node.name}@${node.domain}`
  const nodeConnection = liveNode?.connected
    ? t("connection.state.connected")
    : t("worldSettings.disconnected")
  const identityDetails = [liveNode?.server_url || node.server_url]
  if (liveNode) {
    identityDetails.push(nodeConnection)
    if (liveNode.pending_outbox_count > 0)
      identityDetails.push(
        t("worldSettings.pendingMessages", {
          count: liveNode.pending_outbox_count,
        })
      )
  }
  const [inviteCode, setInviteCode] = useState("")
  const [targetName, setTargetName] = useState("")
  const [targetVisibility, setTargetVisibility] = useState("domain")
  const [relayURL, setRelayURL] = useState("")
  const relays = draft.relays ?? []
  const attachments = saved.running.node_enabled
    ? (statusSnapshot?.relay_attachments ?? [])
    : (statusSnapshot?.remote_access_relays ??
      statusSnapshot?.relay_attachments ??
      [])
  const home = saved.running.node_enabled ? node.server_url : ""
  const remoteURL = statusSnapshot?.remote_access_url
  const remote = draft.remote_access ?? { enabled: false }
  const [editPage, setEditPage] = useState(false)
  const [resetOpen, setResetOpen] = useState(false)
  const [resetError, setResetError] = useState<unknown>()
  const selectedPage =
    !editedSettings && remoteURL
      ? {
          origin: new URL(remoteURL).origin,
          local: statusSnapshot?.remote_access_local_only,
        }
      : getRemoteAccessPage(
          draft.web_url,
          relays,
          node.direct_urls ?? [],
          remoteURL
        )
  const route = statusSnapshot?.remote_access_relays
    ?.find((r) => r.connected)
    ?.url.replace(/^ws/, "http")
  async function resetToken() {
    setSaving(true)
    try {
      const result = await connection.call("server/resetToken", {})
      saveServerToken(connection.entry.url, result.token)
      setResetOpen(false)
    } catch (err) {
      setResetError(err)
    } finally {
      setSaving(false)
    }
  }
  function relayKey(raw: string) {
    try {
      return new URL(raw.replace(/^http/, "ws")).href.replace(/\/+$/, "")
    } catch {
      return raw
    }
  }
  const connectedRelays = new Set(
    attachments
      .filter((item) => item.connected)
      .map((item) => relayKey(item.url))
  )
  const relayRows = [
    ...(home ? [{ url: home, implicit: true }] : []),
    ...relays.map((url) => ({ url, implicit: false })),
  ].map((row) => ({
    ...row,
    connected: connectedRelays.has(relayKey(row.url)),
  }))
  const relayMode =
    draft.relay_mode || (draft.server.enabled ? "domain" : "off")
  const modeLabels: Record<string, string> = {
    off: t("worldSettings.mode_off"),
    domain: t("worldSettings.mode_domain"),
    public: t("worldSettings.mode_public"),
  }
  const [directURL, setDirectURL] = useState("")
  const newTarget = targetName || names[0] || ""
  const [changing, setChanging] = useState(false)
  const [confirm, setConfirm] = useState<"save" | "join" | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<unknown>()
  const dirty = JSON.stringify(base) !== JSON.stringify(draft)
  const archive =
    base.server.is_database_created &&
    base.server.domain !== draft.server.domain
  const patch = (v: Partial<WorldNodeSettings>) =>
    setDraft({ ...draft, node: { ...node, ...v } })
  function sectionProps(
    section: LocalSection,
    summary: string,
    modified: boolean
  ) {
    return {
      open: expanded[section] ?? false,
      onOpenChange: (open: boolean) =>
        setExpanded((previous) => ({ ...previous, [section]: open })),
      summary: (
        <>
          {modified && (
            <Badge variant="outline" className="text-caption">
              {t("worldSettings.modified")}
            </Badge>
          )}
          <span>{summary}</span>
        </>
      ),
    }
  }
  function status(enabled: boolean, running: boolean, unjoined = false) {
    const label =
      enabled !== running
        ? t("worldSettings.pending")
        : unjoined
          ? t("worldSettings.unjoined")
          : running
            ? t("worldSettings.running")
            : t("worldSettings.stopped")
    return (
      <span className="inline-flex items-center gap-2 text-caption color-muted">
        <StatusDot
          label={label}
          tone={
            enabled !== running ? "warning" : running ? "success" : "neutral"
          }
        />
        {enabled !== running && label}
      </span>
    )
  }
  async function save() {
    setSaving(true)
    setError(undefined)
    try {
      await connection.call("config/setWorld", {
        settings: draft,
        join: confirm === "join" ? { invite_code: inviteCode } : undefined,
      })
      onRestart()
    } catch (e) {
      const sections = getWorldErrorSections(e, node)
      if (!sections.length && confirm === "join") {
        sections.push("node")
        if (registered) setChanging(true)
      }
      if (sections.length)
        setExpanded((previous) => ({
          ...previous,
          ...Object.fromEntries(sections.map((section) => [section, true])),
        }))
      setError(e)
      setSaving(false)
      setConfirm(null)
    }
  }
  const joinFields = (
    <FormInput
      id="world-join-invite"
      required
      label={t("worldSettings.invite")}
      type="password"
      value={inviteCode}
      onChange={(e) => setInviteCode(e.target.value)}
    />
  )
  const joinButton = (
    <Button
      className="self-end text-ui"
      disabled={!connected || saving || !inviteCode.trim()}
      onClick={() => {
        setChanging(false)
        setConfirm("join")
      }}
    >
      {t("worldSettings.join")}
    </Button>
  )
  const remove = (action: () => void) => (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={t("worldSettings.remove")}
            onClick={action}
          />
        }
      >
        <XIcon />
      </TooltipTrigger>
      <TooltipContent className="text-caption">
        {t("worldSettings.remove")}
      </TooltipContent>
    </Tooltip>
  )
  const add = (action: () => void, disabled: boolean) => (
    <Button
      variant="outline"
      className="text-ui"
      disabled={disabled}
      onClick={action}
    >
      <PlusIcon data-icon="inline-start" />
      {t("worldSettings.add")}
    </Button>
  )
  const revokedLabel = liveNode?.revoked
    ? t("worldSettings.revoked", { domain: node.domain })
    : undefined
  const nodeSwitch = (
    <Switch
      aria-label={t("worldSettings.node")}
      checked={node.enabled}
      disabled={!registered}
      onCheckedChange={(enabled) => patch({ enabled })}
    />
  )
  return (
    <div className="flex min-w-0 flex-col gap-4">
      <UnsavedChangesGuard dirty={dirty} />
      {error != null && !changing && <RequestError error={error} />}
      {dirty && localTabActive && (
        <SettingsDraftActions
          dirty={dirty}
          saving={saving}
          saveDisabled={!connected}
          status={t("worldSettings.dirty")}
          revertLabel={t("worldSettings.discard")}
          onRevert={() => {
            setDraft(base)
            setError(undefined)
          }}
          saveLabel={t("worldSettings.save")}
          onSave={() => setConfirm("save")}
        />
      )}
      <ConfigurationLayout
        id="world-node"
        title={t("worldSettings.node")}
        {...sectionProps(
          "node",
          revokedLabel ??
            (registered
              ? `${nodeAddress} · ${nodeConnection}`
              : t("worldSettings.unjoined")),
          node.enabled !== base.node.enabled ||
            node.default_target !== base.node.default_target ||
            !!inviteCode
        )}
        actions={
          <>
            {revokedLabel ? (
              <StatusDot label={revokedLabel} tone="warning" />
            ) : (
              status(base.node.enabled, saved.running.node_enabled, !registered)
            )}
            {registered ? (
              nodeSwitch
            ) : (
              <Tooltip>
                <TooltipTrigger render={<span tabIndex={0} />}>
                  {nodeSwitch}
                </TooltipTrigger>
                <TooltipContent>{t("worldSettings.joinFirst")}</TooltipContent>
              </Tooltip>
            )}
          </>
        }
      >
        {registered ? (
          <FieldGroup className="grid min-w-0 items-start gap-3 sm:grid-cols-2">
            <ul className="*:px-0 sm:col-span-2">
              <RowListItem>
                <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-center">
                  <div className="flex min-w-0 flex-1 flex-col gap-1 wrap-anywhere">
                    <span>{nodeAddress}</span>
                    <span className="color-muted">
                      {liveNode?.revoked
                        ? t("worldSettings.revokedHelp")
                        : identityDetails.join(" · ")}
                    </span>
                  </div>
                  <Button
                    variant="outline"
                    className="self-end text-ui sm:self-center"
                    onClick={() => {
                      setError(undefined)
                      setChanging(true)
                    }}
                  >
                    {t(
                      liveNode?.revoked
                        ? "worldSettings.rejoin"
                        : "worldSettings.change"
                    )}
                  </Button>
                </div>
              </RowListItem>
            </ul>
            <Choice
              label={t("worldSettings.defaultAgent")}
              value={node.default_target}
              options={["", ...names]}
              onChange={(default_target) => patch({ default_target })}
            />
          </FieldGroup>
        ) : (
          <>
            <FieldDescription className="text-caption">
              {t("worldSettings.joinInfo")}
            </FieldDescription>
            {joinFields}
            {joinButton}
          </>
        )}
      </ConfigurationLayout>
      <ConfigurationLayout
        title={t("worldSettings.remoteAccess")}
        actions={
          <Switch
            aria-label={t("worldSettings.remoteAccess")}
            checked={remote.enabled}
            onCheckedChange={(enabled) => {
              setDraft({
                ...draft,
                remote_access: { ...remote, enabled },
                relays:
                  enabled && !relays.length ? ["https://open.vaa.la"] : relays,
              })
              if (enabled) setExpanded({ ...expanded, remoteAccess: true })
            }}
          />
        }
        {...sectionProps(
          "remoteAccess",
          route
            ? route + " · " + t("connection.state.connected")
            : remote.enabled
              ? t("worldSettings.disconnected")
              : t("worldSettings.notEnabled"),
          draft.web_url !== base.web_url ||
            JSON.stringify(remote) !== JSON.stringify(base.remote_access)
        )}
      >
        {remote.enabled && (
          <>
            {remoteURL && (
              <div className="flex min-w-0 flex-col gap-2">
                <div className="flex min-w-0 items-center gap-2">
                  <code className="min-w-0 flex-1 truncate text-code">
                    {remoteURL}
                  </code>
                  <CopyButton value={remoteURL} />
                </div>
                {new TextEncoder().encode(remoteURL).length <= 2953 && (
                  <QRCodeCanvas
                    value={remoteURL}
                    size={512}
                    style={{
                      width: 160,
                      height: 160,
                      imageRendering: "pixelated",
                    }}
                    marginSize={4}
                  />
                )}
                <p className="text-caption color-warning">
                  {t("worldSettings.trustHint")}
                </p>
              </div>
            )}
            <FormInput
              id="world-remote-name"
              label={t("worldSettings.machineName")}
              placeholder={t("worldSettings.hostname")}
              value={remote.name ?? ""}
              onChange={(e) =>
                setDraft({
                  ...draft,
                  remote_access: { ...remote, name: e.target.value },
                })
              }
            />
            <div className="flex min-w-0 items-center gap-2">
              <span className="text-ui">{t("worldSettings.webURL")}</span>
              <span className="min-w-0 flex-1 truncate text-code">
                {selectedPage?.origin || t("worldSettings.auto")}
              </span>
              {selectedPage?.local && (
                <Badge variant="outline">{t("worldSettings.localOnly")}</Badge>
              )}
              <Button variant="outline" onClick={() => setEditPage(!editPage)}>
                {t("worldSettings.editPage")}
              </Button>
            </div>
            {editPage && (
              <Input
                id="world-web-url"
                aria-label={t("worldSettings.webURL")}
                value={draft.web_url ?? ""}
                onChange={(e) =>
                  setDraft({ ...draft, web_url: e.target.value })
                }
              />
            )}
          </>
        )}
        {remoteURL && remote.enabled && (
          <Button
            variant="outline"
            className="self-start color-destructive"
            onClick={() => {
              setResetError(undefined)
              setResetOpen(true)
            }}
          >
            {t("worldSettings.resetToken")}
          </Button>
        )}
      </ConfigurationLayout>
      <ConfigurationLayout
        id="world-relays"
        title={t("worldSettings.relays")}
        {...sectionProps(
          "relays",
          t("worldSettings.relaySummary", {
            total: relayRows.length,
            connected: relayRows.filter((row) => row.connected).length,
          }),
          JSON.stringify(relays) !== JSON.stringify(base.relays)
        )}
      >
        <Field>
          <FieldLabel htmlFor="world-relay-url" className="text-ui">
            {t("worldSettings.relayURL")}
          </FieldLabel>
          <div className="flex min-w-0 items-center gap-2">
            <Input
              id="world-relay-url"
              placeholder="https://open.vaa.la"
              className="min-w-0 flex-1 text-ui"
              value={relayURL}
              onChange={(e) => setRelayURL(e.target.value)}
            />
            {add(
              () => {
                setDraft({ ...draft, relays: [...relays, relayURL.trim()] })
                setRelayURL("")
              },
              !relayURL.trim() ||
                [home, ...relays].some(
                  (url) => url && relayKey(url) === relayKey(relayURL.trim())
                )
            )}
          </div>
        </Field>
        <ul className="divide-y border-t">
          {relayRows.map(({ url, implicit, connected }, i) => {
            const state = connected
              ? t("connection.state.connected")
              : t("worldSettings.disconnected")
            return (
              <li key={i} className="flex min-w-0 items-center gap-2 py-2">
                <StatusDot
                  label={state}
                  tone={connected ? "success" : "neutral"}
                />
                <span className="min-w-0 flex-1 truncate text-code">{url}</span>
                {implicit ? (
                  <span className="text-caption color-muted">
                    {t("worldSettings.homeRelay")}
                  </span>
                ) : (
                  remove(() =>
                    setDraft({
                      ...draft,
                      relays: relays.filter((_, j) => j !== i - (home ? 1 : 0)),
                    })
                  )
                )}
              </li>
            )
          })}
        </ul>
      </ConfigurationLayout>
      <ConfigurationLayout
        title={t("worldSettings.relayMode")}
        {...sectionProps(
          "relayMode",
          modeLabels[relayMode] ?? relayMode,
          draft.relay_mode !== base.relay_mode
        )}
      >
        <ToggleGroup
          variant="outline"
          spacing={0}
          aria-label={t("worldSettings.relayMode")}
          value={[relayMode]}
          onValueChange={(values) => {
            if (values[0]) setDraft({ ...draft, relay_mode: values[0] })
          }}
        >
          {(["off", "domain", "public"] as const).map((mode) => (
            <ToggleGroupItem
              key={mode}
              value={mode}
              disabled={mode === "domain" && !draft.server.enabled}
              className="text-ui aria-pressed:bg-primary aria-pressed:text-primary-foreground"
            >
              {modeLabels[mode]}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      </ConfigurationLayout>
      <ConfigurationLayout
        title={t("worldSettings.server")}
        {...sectionProps(
          "server",
          saved.running.server_enabled
            ? `${t("worldSettings.running")} · ${base.server.domain}`
            : t("worldSettings.notEnabled"),
          draft.server.enabled !== base.server.enabled ||
            draft.server.domain !== base.server.domain
        )}
        actions={
          <>
            {status(base.server.enabled, saved.running.server_enabled)}
            <Switch
              aria-label={t("worldSettings.server")}
              checked={draft.server.enabled}
              onCheckedChange={(enabled) =>
                setDraft({
                  ...draft,
                  relay_mode:
                    !enabled && draft.relay_mode === "domain"
                      ? "off"
                      : draft.relay_mode,
                  server: { ...draft.server, enabled },
                })
              }
            />
          </>
        }
      >
        <FieldGroup className="grid min-w-0 items-start gap-3 sm:grid-cols-2">
          <div className="flex min-w-0 flex-col gap-1">
            <FormInput
              id="world-server-domain"
              aria-describedby={
                base.server.is_database_created
                  ? "world-server-domain-help world-server-domain-hint"
                  : "world-server-domain-help"
              }
              placeholder="home"
              label={t("worldSettings.domain")}
              value={draft.server.domain}
              onChange={(e) =>
                setDraft({
                  ...draft,
                  server: { ...draft.server, domain: e.target.value },
                })
              }
            />
            <p
              id="world-server-domain-help"
              className="text-caption color-muted"
            >
              {t("worldSettings.domainHelp")}
            </p>
            {base.server.is_database_created && (
              <p
                className="text-caption color-muted"
                id="world-server-domain-hint"
              >
                {t("worldSettings.domainHint")}
              </p>
            )}
          </div>
        </FieldGroup>
      </ConfigurationLayout>
      {registered && (
        <>
          <ConfigurationLayout
            title={t("worldSettings.targets")}
            {...sectionProps(
              "targets",
              node.advertise_targets.length
                ? t("worldSettings.targetCount", {
                    count: node.advertise_targets.length,
                  })
                : t("worldSettings.notSet"),
              JSON.stringify(node.advertise_targets) !==
                JSON.stringify(base.node.advertise_targets)
            )}
          >
            <FieldGroup className="grid min-w-0 items-start gap-3 sm:grid-cols-2">
              <Choice
                label={t("settings.sections.agents")}
                options={names}
                value={newTarget}
                onChange={setTargetName}
              />
              <Choice
                label={t("worldSettings.visibility")}
                options={["domain", "public"]}
                value={targetVisibility}
                onChange={setTargetVisibility}
                action={add(
                  () =>
                    patch({
                      advertise_targets: [
                        ...node.advertise_targets,
                        { name: newTarget, visibility: targetVisibility },
                      ],
                    }),
                  !newTarget
                )}
              />
            </FieldGroup>
            {node.advertise_targets.length === 0 ? (
              <p className="text-caption color-muted">
                {t("worldSettings.targetsEmpty")}
              </p>
            ) : (
              <ul className="divide-y border-t">
                {node.advertise_targets.map((target, i) => {
                  const change = (v: Partial<typeof target>) =>
                    patch({
                      advertise_targets: node.advertise_targets.map(
                        (item, j) => (i === j ? { ...item, ...v } : item)
                      ),
                    })
                  return (
                    <li
                      key={i}
                      className="grid min-w-0 grid-cols-2 items-center gap-2 py-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,2fr)]"
                    >
                      <Choice
                        hideLabel
                        label={t("settings.sections.agents")}
                        value={target.name}
                        options={names}
                        onChange={(name) => change({ name })}
                      />
                      <Choice
                        hideLabel
                        label={t("worldSettings.visibility")}
                        value={target.visibility}
                        options={["domain", "public"]}
                        onChange={(visibility) => change({ visibility })}
                      />
                      <div className="col-span-2 flex min-w-0 items-center gap-2 sm:col-span-1">
                        <Input
                          aria-label={t("worldSettings.description")}
                          placeholder={t("worldSettings.description")}
                          className="min-w-0 flex-1 text-ui"
                          value={target.description ?? ""}
                          onChange={(e) =>
                            change({ description: e.target.value })
                          }
                        />
                        {remove(() =>
                          patch({
                            advertise_targets: node.advertise_targets.filter(
                              (_, j) => i !== j
                            ),
                          })
                        )}
                      </div>
                    </li>
                  )
                })}
              </ul>
            )}
          </ConfigurationLayout>
          <ConfigurationLayout
            title={t("worldSettings.direct")}
            {...sectionProps(
              "direct",
              node.direct_urls.length
                ? t("worldSettings.directCount", {
                    count: node.direct_urls.length,
                  })
                : t("worldSettings.directViaRelay"),
              JSON.stringify(node.direct_urls) !==
                JSON.stringify(base.node.direct_urls)
            )}
          >
            <div className="flex min-w-0 items-center gap-2">
              <Input
                aria-label={t("worldSettings.direct")}
                placeholder={t("worldSettings.direct")}
                className="min-w-0 flex-1 text-ui"
                value={directURL}
                onChange={(e) => setDirectURL(e.target.value)}
              />
              {add(() => {
                patch({ direct_urls: [...node.direct_urls, directURL.trim()] })
                setDirectURL("")
              }, !directURL.trim())}
            </div>
            <p className="text-caption color-muted">
              {t("worldSettings.directHint")}
            </p>
            {node.direct_urls.length > 0 && (
              <ul className="divide-y border-t">
                {node.direct_urls.map((v, i) => (
                  <li key={i} className="flex min-w-0 items-center gap-2 py-2">
                    <Input
                      aria-label={t("worldSettings.direct")}
                      className="min-w-0 flex-1 text-ui"
                      value={v}
                      onChange={(e) =>
                        patch({
                          direct_urls: node.direct_urls.map((old, j) =>
                            i === j ? e.target.value : old
                          ),
                        })
                      }
                    />
                    {remove(() =>
                      patch({
                        direct_urls: node.direct_urls.filter((_, j) => i !== j),
                      })
                    )}
                  </li>
                ))}
              </ul>
            )}
          </ConfigurationLayout>
        </>
      )}
      <Dialog open={changing} onOpenChange={setChanging}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle className="text-section">
              {t(
                liveNode?.revoked
                  ? "worldSettings.rejoin"
                  : "worldSettings.change"
              )}
            </DialogTitle>
          </DialogHeader>
          {error != null && <RequestError error={error} />}
          {joinFields}
          <DialogFooter className="flex-row justify-end">
            {joinButton}
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <ConfigurationChangeDialog
        open={resetOpen}
        onOpenChange={setResetOpen}
        title={t("worldSettings.resetToken")}
        description={t("worldSettings.resetWarning")}
        confirmLabel={t("worldSettings.resetToken")}
        pending={saving}
        destructive
        onConfirm={() => void resetToken()}
        preview={resetError ? <RequestError error={resetError} /> : undefined}
      />
      <ConfigurationChangeDialog
        open={confirm !== null}
        title={t("worldSettings.confirm")}
        description={t("worldSettings.warning")}
        preview={
          archive
            ? t("worldSettings.archive", { domain: draft.server.domain })
            : undefined
        }
        confirmLabel={t("worldSettings.save")}
        pending={saving}
        onOpenChange={(open) => {
          if (!open) setConfirm(null)
        }}
        onConfirm={() => void save()}
      />
    </div>
  )
}
