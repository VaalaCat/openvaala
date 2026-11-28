import {
  useWorldAdminFilters,
  matchesWorldAdminFilters,
  matchesWorldAdminSearch,
} from "@/hooks/use-world-admin-filters"
import { useId, useState, type FormEvent, type ReactNode } from "react"
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query"
import {
  NetworkIcon,
  PlusIcon,
  UsersIcon,
  KeyRoundIcon,
  ListChecksIcon,
  ServerIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { CopyButton } from "@/components/base/copy-button"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ListEmpty } from "@/components/base/list-empty"
import { RequestError } from "@/components/base/request-error"
import { ScrollableTabsList } from "@/components/base/scrollable-tabs-list"
import { StatusDot } from "@/components/base/status-dot"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import {
  adminQueryKey,
  worldAdminUsersOptions,
  worldMatrixOptions,
} from "@/lib/query/world-admin"
import {
  AdminSection,
  AdminSearch,
  AdminTable,
  AdminAddButton,
  AdminRowMenu,
  AdminTags,
} from "./world-admin-table"
import { WorldRules } from "./world-admin-rules"
import { WorldAdminFilters } from "./world-admin-filters"
import { RpcError } from "@/lib/rpc-error"
import { getWorldErrorHint } from "./message-bubble-helpers"
import { notifySuccess } from "@/lib/notify"
import type { ListDomainsResult } from "@/lib/api/protocol.gen"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Tabs, TabsContent, TabsTrigger } from "@/components/ui/tabs"
import {
  getWorldDomains,
  decideWorldDomain,
  WorldAdminError,
  getWorldAdminNodes,
  saveWorldACL,
  addWorldDomain,
  deleteWorldDomain,
  createWorldJoinToken,
  getWorldJoinTokens,
  getWorldJoinTokenStatus,
  revokeWorldResource,
  worldJoinCommand,
  worldJoinExpiry,
  type WorldACLRule,
  type WorldAdminConnection,
  type WorldAdminNode,
} from "@/lib/api/world-admin"

const tabs = [
  { value: "users", label: "worldAdmin.users", icon: UsersIcon },
  { value: "nodes", label: "worldAdmin.nodes", icon: ServerIcon },
  { value: "tokens", label: "worldAdmin.tokens", icon: KeyRoundIcon },
  { value: "acl", label: "worldAdmin.acl", icon: ListChecksIcon },
  { value: "domains", label: "worldAdmin.domains", icon: NetworkIcon },
] as const

export function WorldAdminConsole({
  connection,
  initialNodes,
  onOpenRelaySettings,
}: {
  connection: WorldAdminConnection
  initialNodes?: WorldAdminNode[]
  onOpenRelaySettings?: () => void
}) {
  const { t } = useTranslation()
  const { search, open } = useWorldAdminFilters()
  const requested = search.get("view")
  const view = tabs.find((tab) => tab.value === requested)?.value ?? "users"
  const domains = useQuery({
    queryKey: adminQueryKey(connection, "domains"),
    queryFn: ({ signal }) => getWorldDomains(connection, signal),
    refetchInterval: view === "domains" ? 15_000 : false,
  })
  const [creating, setCreating] = useState<string>()
  const [target, setTarget] = useState<Revocation>()
  const [draft, setDraft] = useState<WorldACLRule[]>()
  const cache = useQueryClient()
  const save = useMutation({
    mutationFn: (rules: WorldACLRule[]) => saveWorldACL(connection, rules),
    onSuccess: async (_, submitted) => {
      await Promise.all([
        cache.invalidateQueries({
          queryKey: adminQueryKey(connection, "acl"),
          exact: true,
        }),
        cache.invalidateQueries({
          queryKey: worldMatrixOptions(connection).queryKey,
          exact: true,
        }),
        cache.invalidateQueries({
          queryKey: adminQueryKey(connection, "users"),
          exact: true,
        }),
      ])
      setDraft((current) => (current === submitted ? undefined : current))
    },
  })
  return (
    <section className="flex w-full min-w-0 flex-col gap-4 py-4">
      <header className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)] items-center gap-x-2 gap-y-1 sm:grid-cols-[auto_fit-content(60%)_minmax(0,1fr)]">
        <NetworkIcon size={18} className="shrink-0 color-muted" aria-hidden />
        <h2 className="min-w-0 truncate font-mono text-base font-bold">
          {domains.data?.domain}
        </h2>
        {domains.data && (
          <div className="col-start-2 flex min-w-0 items-center gap-2 sm:col-start-3">
            {domains.data.self_card ? (
              <>
                <OverflowTooltip
                  render={<code />}
                  className="min-w-0 truncate text-code color-muted sm:max-w-64"
                >
                  {domains.data.self_card}
                </OverflowTooltip>
                <CopyButton
                  value={domains.data.self_card}
                  label={t("worldAdmin.selfCard")}
                />
              </>
            ) : (
              <>
                <span className="min-w-0 text-ui wrap-anywhere color-muted">
                  {t(
                    onOpenRelaySettings
                      ? "worldAdmin.noPublicEntry"
                      : "worldAdmin.configurePublicEntry"
                  )}
                </span>
                {onOpenRelaySettings && (
                  <Button
                    type="button"
                    variant="link"
                    size="sm"
                    className="text-ui"
                    onClick={onOpenRelaySettings}
                  >
                    {t("worldAdmin.openSettings")}
                  </Button>
                )}
              </>
            )}
          </div>
        )}
      </header>
      {domains.isError && (
        <RequestError
          error={domains.error}
          onRetry={() => void domains.refetch()}
        />
      )}
      <Tabs
        value={view}
        onValueChange={(v) => open(v)}
        className="min-w-0 gap-6"
      >
        <ScrollableTabsList variant="line" aria-label={t("worldAdmin.title")}>
          {tabs.map(({ value, label, icon: Icon }) => (
            <TabsTrigger key={value} value={value} className="text-ui">
              <Icon aria-hidden />
              {t(label)}
            </TabsTrigger>
          ))}
        </ScrollableTabsList>
        <TabsContent value="users">
          <WorldUsers
            onRevoke={setTarget}
            connection={connection}
            onCreate={setCreating}
          />
        </TabsContent>
        <TabsContent value="nodes">
          <WorldNodes
            onRevoke={setTarget}
            connection={connection}
            initialNodes={initialNodes}
          />
        </TabsContent>
        <TabsContent value="tokens">
          <WorldTokens
            onRevoke={setTarget}
            connection={connection}
            onCreate={() => setCreating(search.get("owner") ?? "")}
          />
        </TabsContent>
        <TabsContent value="acl" className="min-w-0">
          <WorldRules
            connection={connection}
            draft={draft}
            setDraft={setDraft}
            save={save}
          />
        </TabsContent>
        <TabsContent value="domains">
          <WorldDomains connection={connection} query={domains} />
        </TabsContent>
      </Tabs>
      {target && (
        <RevokeDialog
          connection={connection}
          target={target}
          onClose={() => setTarget(undefined)}
        />
      )}
      {creating !== undefined && (
        <CreateTokenDialog
          connection={connection}
          initialOwner={creating}
          onClose={() => setCreating(undefined)}
        />
      )}
    </section>
  )
}

function QueryResults<T>({
  query,
  columns,
  empty,
  filter,
  clear,
  noMatch,
  children,
}: {
  columns: number
  query: UseQueryResult<T[]>
  empty: string
  filter?: (item: T) => boolean
  clear?: () => void
  noMatch?: string
  children(items: T[]): ReactNode
}) {
  const { t } = useTranslation()
  const items = filter ? query.data?.filter(filter) : query.data
  return (
    <>
      {query.isError && (
        <tr>
          <td colSpan={columns}>
            <RequestError
              error={query.error}
              onRetry={() => void query.refetch()}
            />
          </td>
        </tr>
      )}
      {query.isPending ? (
        <tr>
          <td colSpan={columns}>
            <LatticeLoader aria-label={t("common.loading")} />
          </td>
        </tr>
      ) : items?.length ? (
        children(items)
      ) : query.isSuccess ? (
        <tr>
          <td colSpan={columns}>
            <ListEmpty
              title={empty}
              filtered={!!clear}
              noMatchTitle={noMatch}
              onClearFilters={clear}
            />
          </td>
        </tr>
      ) : null}
    </>
  )
}

type Revocation = {
  resource: "nodes" | "owners" | "tokens"
  id: string
  title: string
}

function RevokeDialog({
  connection,
  target,
  onClose,
}: {
  connection: WorldAdminConnection
  target: Revocation
  onClose(): void
}) {
  const { t } = useTranslation()
  const cache = useQueryClient()
  const revoke = useMutation({
    mutationFn: () => revokeWorldResource(connection, target),
    onSuccess: async () => {
      await cache.invalidateQueries({
        queryKey: adminQueryKey(connection),
      })
      onClose()
    },
  })
  return (
    <ConfigurationChangeDialog
      open
      destructive
      title={target.title}
      description={t(
        target.resource === "owners"
          ? "worldAdmin.revokeOwnerDetail"
          : "worldAdmin.revokeDetail"
      )}
      preview={revoke.isError && <RequestError error={revoke.error} />}
      pending={revoke.isPending}
      confirmLabel={t("worldAdmin.revoke")}
      onConfirm={() => revoke.mutate()}
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
    />
  )
}

function CountLink({
  count,
  label,
  onClick,
  online,
}: {
  count: number
  label: string
  onClick(): void
  online?: boolean
}) {
  const { t } = useTranslation()
  return (
    <td className="text-right tabular-nums">
      <button
        aria-label={label}
        className="inline-flex min-w-6 items-center justify-end gap-2 py-2 underline-offset-4 hover:underline"
        onClick={onClick}
      >
        {online && count > 0 && (
          <StatusDot tone="success" label={t("worldAdmin.online")} />
        )}
        {count}
      </button>
    </td>
  )
}

function WorldUsers({
  connection,
  onCreate,
  onRevoke,
}: {
  connection: WorldAdminConnection
  onCreate(owner: string): void
  onRevoke(target: Revocation): void
}) {
  const { t } = useTranslation()
  const { open, search } = useWorldAdminFilters()
  const users = useQuery({
    ...worldAdminUsersOptions(connection),
    refetchInterval: 15_000,
  })
  return (
    <AdminSection
      title={t("worldAdmin.users")}
      description={t("worldAdmin.usersDescription")}
      actions={
        <AdminAddButton
          label={t("worldAdmin.createToken")}
          onClick={() => onCreate("")}
        />
      }
    >
      <AdminSearch placeholder={t("worldAdmin.searchUsers")} />
      <AdminTable
        label={t("worldAdmin.users")}
        numeric={[1, 2, 3, 4]}
        headings={[
          t("worldAdmin.users"),
          t("worldAdmin.machines"),
          t("worldAdmin.online"),
          t("worldAdmin.tokens"),
          t("worldAdmin.rules"),
          "",
        ]}
      >
        <QueryResults
          query={users}
          columns={6}
          empty={t("worldAdmin.emptyUsers")}
          noMatch={t("worldAdmin.noUsers")}
          clear={search.has("adminSearch") ? () => open("users") : undefined}
          filter={(u) => matchesWorldAdminSearch(search, [u.owner])}
        >
          {(items) =>
            items.map((u) => (
              <tr key={u.owner}>
                <td className="w-2/5">
                  <div className="flex items-center gap-3">
                    <span
                      aria-hidden
                      className="flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-caption font-semibold"
                    >
                      {Array.from(u.owner)[0]?.toUpperCase()}
                    </span>
                    <span className="font-medium">{u.owner}</span>
                  </div>
                </td>
                <CountLink
                  count={u.node_count}
                  label={t("worldAdmin.nodeCount", { n: u.node_count })}
                  onClick={() =>
                    open("nodes", { owner: u.owner, online: "active" })
                  }
                />
                <CountLink
                  count={u.online_count}
                  online
                  label={t("worldAdmin.onlineCount", { n: u.online_count })}
                  onClick={() => open("nodes", { owner: u.owner, online: "1" })}
                />
                <CountLink
                  count={u.token_count}
                  label={t("worldAdmin.tokenCount", { n: u.token_count })}
                  onClick={() =>
                    open("tokens", { owner: u.owner, status: "available" })
                  }
                />
                <CountLink
                  count={u.rule_count}
                  label={t("worldAdmin.ruleCount", { n: u.rule_count })}
                  onClick={() => open("acl", { ruleOwner: u.owner })}
                />
                <td className="text-right">
                  <AdminRowMenu
                    label={u.owner}
                    action={{
                      label: t("worldAdmin.createToken"),
                      run: () => onCreate(u.owner),
                    }}
                    destructiveLabel={t("worldAdmin.revokeOwner")}
                    disabled={!u.node_count}
                    onRemove={() =>
                      onRevoke({
                        resource: "owners",
                        id: u.owner,
                        title: t("worldAdmin.revokeOwnerTitle", {
                          name: u.owner,
                        }),
                      })
                    }
                  />
                </td>
              </tr>
            ))
          }
        </QueryResults>
      </AdminTable>
    </AdminSection>
  )
}

function WorldNodes({
  connection,
  initialNodes,
  onRevoke,
}: {
  connection: WorldAdminConnection
  initialNodes?: WorldAdminNode[]
  onRevoke(target: Revocation): void
}) {
  const { t } = useTranslation()
  const { open, search } = useWorldAdminFilters()
  const query = useQuery({
    queryKey: adminQueryKey(connection, "nodes"),
    queryFn: ({ signal }) => getWorldAdminNodes(connection, signal),
    initialData: initialNodes,
    refetchInterval: 15_000,
  })
  const online = search.get("online")
  return (
    <AdminSection
      title={t("worldAdmin.nodes")}
      description={t("worldAdmin.nodesDescription")}
    >
      <AdminSearch placeholder={t("worldAdmin.searchNodes")}>
        <WorldAdminFilters
          items={query.data ?? []}
          statusKey="online"
          statuses={[
            { value: "active", label: t("worldAdmin.unrevoked") },
            { value: "1", label: t("worldAdmin.online") },
            { value: "0", label: t("worldAdmin.offline") },
            { value: "revoked", label: t("worldAdmin.revoked") },
          ]}
        />
      </AdminSearch>
      <AdminTable
        label={t("worldAdmin.nodes")}
        headings={[
          t("worldAdmin.machines"),
          t("worldAdmin.owner"),
          t("worldAdmin.status"),
          "",
        ]}
      >
        <QueryResults
          columns={4}
          query={query}
          empty={t("worldAdmin.emptyNodes")}
          noMatch={t("worldAdmin.noNodes")}
          clear={
            ["adminSearch", "owner", "tag", "online"].some((k) => search.has(k))
              ? () => open("nodes")
              : undefined
          }
          filter={(n) =>
            matchesWorldAdminFilters(n, search) &&
            matchesWorldAdminSearch(search, [
              n.node_name + "@" + n.domain,
              n.owner,
              ...(n.tags ?? []),
            ]) &&
            (!online ||
              (n.revoked_at
                ? online === "revoked"
                : online === "active" || online === (n.online ? "1" : "0")))
          }
        >
          {(nodes) =>
            nodes.map((node) => {
              const address = node.node_name + "@" + node.domain
              const status = t(
                node.revoked_at
                  ? "worldAdmin.revoked"
                  : node.online
                    ? "worldAdmin.online"
                    : "worldAdmin.offline"
              )
              return (
                <tr key={node.node_id}>
                  <td className="w-1/2">
                    <div className="flex flex-col items-start gap-1">
                      <span className="font-medium">{address}</span>
                      <div className="flex flex-wrap items-center gap-1 empty:hidden">
                        {!!node.tags?.length && <AdminTags tags={node.tags} />}
                        {node.targets.map((target) => (
                          <Badge
                            key={target.name}
                            variant="outline"
                            className="text-caption"
                          >
                            {target.name}
                            {target.description && " / " + target.description}
                          </Badge>
                        ))}
                      </div>
                    </div>
                  </td>
                  <td>{node.owner}</td>
                  <td>
                    <span className="inline-flex items-center gap-2 color-subtle">
                      {!node.revoked_at && (
                        <StatusDot
                          label={status}
                          tone={node.online ? "success" : "neutral"}
                        />
                      )}
                      {status}
                    </span>
                  </td>
                  <td className="text-right">
                    {!node.revoked_at && (
                      <AdminRowMenu
                        label={address}
                        destructiveLabel={t("worldAdmin.revokeNode")}
                        onRemove={() =>
                          onRevoke({
                            resource: "nodes",
                            id: node.node_id,
                            title: t("worldAdmin.revokeNodeTitle", {
                              name: address,
                            }),
                          })
                        }
                      />
                    )}
                  </td>
                </tr>
              )
            })
          }
        </QueryResults>
      </AdminTable>
    </AdminSection>
  )
}

const tokenStatusLabels = {
  available: "worldAdmin.available",
  used: "worldAdmin.used",
  expired: "worldAdmin.expired",
  revoked: "worldAdmin.revoked",
} as const

function WorldTokens({
  connection,
  onCreate,
  onRevoke,
}: {
  connection: WorldAdminConnection
  onCreate(): void
  onRevoke(target: Revocation): void
}) {
  const { t, i18n } = useTranslation()
  const { open, search } = useWorldAdminFilters()
  const query = useQuery({
    queryKey: adminQueryKey(connection, "tokens"),
    queryFn: ({ signal }) => getWorldJoinTokens(connection, signal),
  })
  const date = new Intl.DateTimeFormat(i18n.language, {
    dateStyle: "medium",
    timeStyle: "short",
  })
  return (
    <AdminSection
      title={t("worldAdmin.tokens")}
      description={t("worldAdmin.tokensDescription")}
      actions={
        <AdminAddButton
          label={t("worldAdmin.createToken")}
          onClick={onCreate}
        />
      }
    >
      <AdminSearch placeholder={t("worldAdmin.searchTokens")}>
        <WorldAdminFilters
          items={query.data ?? []}
          statusKey="status"
          statuses={Object.entries(tokenStatusLabels).map(([value, label]) => ({
            value,
            label: t(label),
          }))}
        />
      </AdminSearch>
      <AdminTable
        label={t("worldAdmin.tokens")}
        headings={[
          t("worldAdmin.tokens"),
          t("worldAdmin.owner"),
          t("worldAdmin.tag"),
          t("worldAdmin.status"),
          t("worldAdmin.expiry"),
          "",
        ]}
      >
        <QueryResults
          columns={6}
          query={query}
          empty={t("worldAdmin.emptyTokens")}
          noMatch={t("worldAdmin.noTokens")}
          clear={
            ["adminSearch", "owner", "tag", "status"].some((k) => search.has(k))
              ? () => open("tokens")
              : undefined
          }
          filter={(v) =>
            matchesWorldAdminFilters(v, search) &&
            matchesWorldAdminSearch(search, [
              v.id,
              v.owner,
              ...(v.tags ?? []),
            ]) &&
            (!search.has("status") ||
              search.get("status") === getWorldJoinTokenStatus(v))
          }
        >
          {(tokens) =>
            tokens.map((token) => {
              const status = getWorldJoinTokenStatus(token)
              return (
                <tr key={token.id}>
                  <td>
                    <OverflowTooltip
                      render={<code />}
                      className="block max-w-40 truncate text-code"
                    >
                      {token.id}
                    </OverflowTooltip>
                    <div className="text-caption color-muted">
                      {token.reusable && t("worldAdmin.reusable") + " · "}
                      {t("worldAdmin.usedCount", { count: token.used_count })}
                    </div>
                  </td>
                  <td>{token.owner}</td>
                  <td>
                    {token.tags?.length ? (
                      <AdminTags tags={token.tags} />
                    ) : (
                      <span className="color-muted">—</span>
                    )}
                  </td>
                  <td>
                    <Badge
                      variant={status === "available" ? "secondary" : "outline"}
                      className={
                        status === "available"
                          ? "bg-success/10 text-caption color-success"
                          : "text-caption color-muted"
                      }
                    >
                      {t(tokenStatusLabels[status])}
                    </Badge>
                  </td>
                  <td className="text-caption color-subtle tabular-nums">
                    {date.format(token.expires_at)}
                  </td>
                  <td className="text-right">
                    {status === "available" && (
                      <AdminRowMenu
                        label={token.id}
                        destructiveLabel={t("worldAdmin.revokeToken")}
                        onRemove={() =>
                          onRevoke({
                            resource: "tokens",
                            id: token.id,
                            title: t("worldAdmin.revokeTokenTitle", {
                              name: token.owner,
                            }),
                          })
                        }
                      />
                    )}
                  </td>
                </tr>
              )
            })
          }
        </QueryResults>
      </AdminTable>
    </AdminSection>
  )
}

function CreateTokenDialog({
  connection,
  initialOwner,
  onClose,
}: {
  initialOwner: string
  connection: WorldAdminConnection
  onClose(): void
}) {
  const { t } = useTranslation()
  const cache = useQueryClient()
  const id = useId()
  const [owner, setOwner] = useState(initialOwner)
  const [tags, setTags] = useState("")
  const [hours, setHours] = useState("24")
  const [reusable, setReusable] = useState(false)
  const create = useMutation({
    mutationFn: (expiresAt: number) =>
      createWorldJoinToken(connection, {
        owner: owner.trim(),
        tags: tags
          .split(",")
          .map((tag) => tag.trim())
          .filter(Boolean),
        reusable,
        expires_at: expiresAt,
      }),
    onSuccess: () => {
      void cache.invalidateQueries({
        queryKey: adminQueryKey(connection),
      })
    },
  })
  const expiry = worldJoinExpiry(Number(hours))
  const result = create.data
  const command = result && worldJoinCommand(result.token)
  function submit(event: FormEvent) {
    event.preventDefault()
    const expiresAt = worldJoinExpiry(Number(hours))
    if (owner.trim() && expiresAt && !create.isPending) create.mutate(expiresAt)
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !create.isPending) onClose()
      }}
    >
      <DialogContent className="max-h-[90dvh] overflow-y-auto text-body">
        <DialogHeader>
          <DialogTitle className="text-section">
            {t(result ? "worldAdmin.tokenCreated" : "worldAdmin.createToken")}
          </DialogTitle>
        </DialogHeader>
        {result ? (
          <div className="flex min-w-0 flex-col gap-3">
            <p className="text-caption color-muted">
              {t("worldAdmin.tokenOnce")}
            </p>
            <div className="flex min-w-0 items-start gap-2 rounded-md border bg-muted p-2">
              <code className="min-w-0 flex-1 text-code wrap-anywhere">
                {result.token}
              </code>
              <CopyButton
                value={result.token}
                label={t("worldAdmin.copyToken")}
              />
            </div>
            {command && (
              <div className="flex min-w-0 flex-col gap-1">
                <label className="text-ui" htmlFor={id + "-command"}>
                  {t("worldAdmin.joinCommand")}
                </label>
                <div className="flex min-w-0 items-start gap-2 rounded-md border bg-muted p-2">
                  <code
                    id={id + "-command"}
                    className="min-w-0 flex-1 overflow-x-auto text-code whitespace-pre"
                  >
                    {command}
                  </code>
                  <CopyButton
                    value={command}
                    label={t("worldAdmin.copyJoinCommand")}
                  />
                </div>
              </div>
            )}
            <DialogFooter>
              <Button className="text-ui" onClick={onClose}>
                {t("worldAdmin.done")}
              </Button>
            </DialogFooter>
          </div>
        ) : (
          <form className="flex flex-col gap-3" onSubmit={submit}>
            {create.isError && <RequestError error={create.error} />}
            <Field>
              <FieldLabel className="text-ui" htmlFor={id + "-owner"}>
                {t("worldAdmin.owner")}
              </FieldLabel>
              <Input
                className="text-ui"
                id={id + "-owner"}
                required
                value={owner}
                onChange={(e) => setOwner(e.target.value)}
              />
            </Field>
            <Field>
              <FieldLabel className="text-ui" htmlFor={id + "-tags"}>
                {t("worldAdmin.tags")}
              </FieldLabel>
              <Input
                className="text-ui"
                id={id + "-tags"}
                value={tags}
                onChange={(e) => setTags(e.target.value)}
              />
            </Field>
            <Field>
              <FieldLabel className="text-ui" htmlFor={id + "-ttl"}>
                {t("worldAdmin.ttl")}
              </FieldLabel>
              <Input
                className="text-ui"
                id={id + "-ttl"}
                type="number"
                min="0.01"
                step="any"
                required
                value={hours}
                onChange={(e) => setHours(e.target.value)}
              />
            </Field>
            <label className="flex items-center gap-2 text-ui">
              <Checkbox checked={reusable} onCheckedChange={setReusable} />
              {t("worldAdmin.reusable")}
            </label>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                className="text-ui"
                onClick={onClose}
                disabled={create.isPending}
              >
                {t("common.actions.cancel")}
              </Button>
              <Button
                type="submit"
                className="text-ui"
                disabled={!owner.trim() || !expiry || create.isPending}
              >
                {create.isPending && <LatticeLoader data-icon="inline-start" />}
                {t("worldAdmin.createToken")}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

function WorldDomains({
  connection,
  query,
}: {
  connection: WorldAdminConnection
  query: UseQueryResult<ListDomainsResult>
}) {
  const { t } = useTranslation()
  const [card, setCard] = useState("")
  const [deleting, setDeleting] = useState("")
  const add = useMutation({
    mutationFn: () => addWorldDomain(connection, card.trim()),
    onSuccess: async (peer) => {
      notifySuccess(
        t(peer.status === "mutual" ? "worldAdmin.mutual" : "worldAdmin.invited")
      )
      setCard("")
      await query.refetch()
    },
  })
  const remove = useMutation({
    mutationFn: () => deleteWorldDomain(connection, deleting),
    onSuccess: async () => {
      setDeleting("")
      await query.refetch()
    },
  })
  const { search, update } = useWorldAdminFilters()
  const decision = useMutation({
    mutationFn: ({ domain, accept }: { domain: string; accept: boolean }) =>
      decideWorldDomain(connection, domain, accept),
    onSuccess: () => query.refetch(),
  })
  const code =
    add.error instanceof RpcError
      ? add.error.data?.code
      : add.error instanceof WorldAdminError
        ? add.error.code
        : undefined
  const hint = getWorldErrorHint(code)
  const statuses = {
    pending_inbound: "worldAdmin.pending_inbound",
    pending_outbound: "worldAdmin.pending_outbound",
    mutual: "worldAdmin.mutual",
    rejected: "worldAdmin.rejected",
  } as const
  const peers = query.data?.domains.filter((peer) =>
    matchesWorldAdminSearch(search, [peer.domain])
  )
  return (
    <AdminSection
      title={t("worldAdmin.domains")}
      description={t("worldAdmin.domainsDescription")}
    >
      <AdminSearch placeholder={t("worldAdmin.searchDomains")} />
      <AdminTable
        label={t("worldAdmin.domains")}
        headings={[t("worldAdmin.domain"), t("worldAdmin.status"), ""]}
      >
        {query.isPending && (
          <tr>
            <td colSpan={3}>
              <LatticeLoader />
            </td>
          </tr>
        )}
        {peers?.map((peer) => (
          <tr key={peer.domain}>
            <td className="font-medium">{peer.domain}</td>
            <td>{t(statuses[peer.status as keyof typeof statuses])}</td>
            <td className="text-right">
              <div className="flex flex-wrap items-center justify-end gap-1">
                {peer.status === "pending_inbound" &&
                  [true, false].map((accept) => (
                    <Button
                      key={String(accept)}
                      size="sm"
                      variant="outline"
                      disabled={decision.isPending}
                      onClick={() =>
                        decision.mutate({ domain: peer.domain, accept })
                      }
                    >
                      {t(accept ? "worldAdmin.accept" : "worldAdmin.reject")}
                    </Button>
                  ))}
                <AdminRowMenu
                  label={peer.domain}
                  destructiveLabel={t("worldSettings.remove")}
                  onRemove={() => {
                    remove.reset()
                    setDeleting(peer.domain)
                  }}
                />
              </div>
            </td>
          </tr>
        ))}
        {peers?.length === 0 && (
          <tr>
            <td colSpan={3}>
              <ListEmpty
                title={t("worldAdmin.emptyDomains")}
                filtered={search.has("adminSearch")}
                noMatchTitle={t("worldAdmin.noDomains")}
                onClearFilters={() => update("adminSearch", "")}
              />
            </td>
          </tr>
        )}
      </AdminTable>
      <div className="mt-6 max-w-2xl">
        <Field>
          <FieldLabel htmlFor="peer-card" className="text-ui">
            {t("worldAdmin.peerCard")}
          </FieldLabel>
          <div className="flex min-w-0 items-center gap-2">
            <Input
              id="peer-card"
              className="min-w-0 flex-1 text-ui"
              value={card}
              onChange={(e) => setCard(e.target.value)}
            />
            <Button
              variant="outline"
              disabled={!card.trim() || add.isPending}
              onClick={() => add.mutate()}
            >
              <PlusIcon data-icon="inline-start" />
              {t("worldAdmin.importCard")}
            </Button>
          </div>
          {add.isError &&
            (hint ? (
              <p role="alert" className="text-caption color-destructive">
                {t(hint.message, { name: card.split("#")[0] })}
              </p>
            ) : (
              <RequestError error={add.error} />
            ))}
          {decision.isError && <RequestError error={decision.error} />}
        </Field>
      </div>
      <ConfigurationChangeDialog
        destructive
        open={!!deleting}
        title={t("worldAdmin.deleteDomain", { domain: deleting })}
        preview={remove.isError && <RequestError error={remove.error} />}
        pending={remove.isPending}
        confirmLabel={t("worldSettings.remove")}
        onConfirm={() => remove.mutate()}
        onOpenChange={(open) => {
          if (!open) setDeleting("")
        }}
      />
    </AdminSection>
  )
}
