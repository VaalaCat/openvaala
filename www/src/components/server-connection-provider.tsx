import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react"
import { useStore } from "@nanostores/react"
import { useQueryClient } from "@tanstack/react-query"
import { ServerIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { NavigationButton } from "@/components/base/navigation-button"
import { TokenEntryPage } from "@/app/token-entry"
import { StandalonePage } from "@/components/base/standalone-page"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ServerListLabel } from "@/components/server-list-label"
import { ServerManagerDialog } from "@/components/server-manager-dialog"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { getWorldTargetName } from "@/lib/api/world-transport"
import { ServerConnection } from "@/lib/api/server-connection"
import { RunningConversationsSync } from "@/lib/api/running-conversations-sync"
import { RunningTerminalsSync } from "@/lib/api/running-terminals-sync"
import {
  $activeServerURL,
  $servers,
  getPageServerURL,
  getServerDisplay,
  saveServerIdentity,
  saveServerToken,
  setActiveServerURL,
  takeTokenFromHash,
  upsertServer,
  parseServerInput,
  type ServerEntry,
} from "@/lib/api/servers"
import { invalidateAfterReconnect } from "@/lib/query/invalidation"

const ServerConnectionContext = createContext<ServerConnection | undefined>(
  undefined
)

const RunningConversationsSyncContext = createContext<
  RunningConversationsSync | undefined
>(undefined)

// 读取当前服务器的全局运行集合同步对象。
// eslint-disable-next-line react-refresh/only-export-components
export function useRunningConversationsSync(): RunningConversationsSync {
  // 1. 读取服务器作用域创建的同步对象。
  const sync = useContext(RunningConversationsSyncContext)
  if (!sync)
    throw new Error(
      "useRunningConversationsSync needs a connected ServerConnectionProvider"
    )
  return sync
}

const RunningTerminalsSyncContext = createContext<
  RunningTerminalsSync | undefined
>(undefined)

// 读取当前服务器的运行中终端列表同步对象。
// eslint-disable-next-line react-refresh/only-export-components
export function useRunningTerminalsSync(): RunningTerminalsSync {
  // 1. 读取服务器作用域创建的同步对象。
  const sync = useContext(RunningTerminalsSyncContext)
  if (!sync)
    throw new Error(
      "useRunningTerminalsSync needs a connected ServerConnectionProvider"
    )
  return sync
}

// 当前活跃 serve 的连接，只在 ServerConnectionProvider 连上之后的子树里可用
// eslint-disable-next-line react-refresh/only-export-components
export function useServerConnection(): ServerConnection {
  const connection = useContext(ServerConnectionContext)
  if (!connection)
    throw new Error(
      "useServerConnection needs a connected ServerConnectionProvider"
    )
  return connection
}

// 读出地址里的 token 存下来并去掉 #；重复执行时 # 已经不在，不会覆盖已存的 token
function takePageToken(): void {
  const token = takeTokenFromHash(window.location.hash)
  if (!token) return
  saveServerToken(getPageServerURL(window.location), token)
  window.history.replaceState(
    window.history.state,
    "",
    window.location.pathname + window.location.search
  )
}

// 建立并保持当前选定 serve 的连接；连上之前显示 token 输入页、连接中或版本不一致的提示，连上之后渲染子树
export function ServerConnectionProvider({
  children,
}: {
  children: ReactNode
}) {
  const pageURL = getPageServerURL(window.location)
  const role = document.documentElement.dataset.pageRole
  const hasPageRuntime = role === "runtime"
  const [showHome, setShowHome] = useState(
    () =>
      !window.location.hash &&
      ["/", "/web/v1/"].includes(window.location.pathname)
  )
  const [worldLink, setWorldLink] = useState<string>()
  const [managerOpen, setManagerOpen] = useState(false)
  useEffect(() => {
    function importPageLink() {
      const hash = window.location.hash
      if (!hash.startsWith("#world=")) {
        if (hasPageRuntime) takePageToken()
        return
      }
      const link = window.location.origin + "/web/v1/" + hash
      const parsed = parseServerInput(link)
      setShowHome(false)
      setManagerOpen(false)
      if (parsed.ok && parsed.token && globalThis.crypto?.subtle) {
        upsertServer({ url: parsed.url, token: parsed.token })
        setActiveServerURL(parsed.url, pageURL)
        setWorldLink(undefined)
      } else {
        setWorldLink(link)
      }
      window.history.replaceState(
        window.history.state,
        "",
        window.location.pathname + window.location.search
      )
    }
    importPageLink()
    window.addEventListener("hashchange", importPageLink)
    return () => window.removeEventListener("hashchange", importPageLink)
  }, [pageURL, hasPageRuntime])
  const servers = useStore($servers)
  const activeServerURL = useStore($activeServerURL)
  const entries = servers.filter((s) => s.url !== pageURL)
  if (hasPageRuntime)
    entries.unshift(
      servers.find((s) => s.url === pageURL) ?? { url: pageURL, token: "" }
    )

  const activeURL = worldLink || activeServerURL || pageURL
  const isRemote = activeURL !== pageURL
  const activeEntry = servers.find((server) => server.url === activeURL)
  const parsed = useMemo(
    () =>
      activeURL.includes("#world=") ? parseServerInput(activeURL) : undefined,
    [activeURL]
  )
  const target = parsed?.ok ? parsed.worldTarget : undefined
  const needsSecurePage = !!target && !globalThis.crypto?.subtle
  const token = worldLink ? undefined : activeEntry?.token
  const serverLabel =
    isRemote && parsed?.ok !== false
      ? activeEntry?.name ||
        (target ? getWorldTargetName(target) : new URL(activeURL).host)
      : undefined

  const connection = useMemo(
    () =>
      token === undefined || parsed?.ok === false || needsSecurePage
        ? undefined
        : new ServerConnection({ url: activeURL, token, worldTarget: target }),
    [activeURL, token, parsed, target, needsSecurePage]
  )

  const { t } = useTranslation()
  if (
    window.location.hash.startsWith("#world=") ||
    (hasPageRuntime && takeTokenFromHash(window.location.hash))
  )
    return <StandalonePage title={t("connection.connecting.title")} />
  if (parsed?.ok === false)
    return <StandalonePage title={t("worldSettings.invalidLink")} />
  if (needsSecurePage)
    return <StandalonePage title={t("worldSettings.securePage")} />
  function submitToken(value: string) {
    if (worldLink && parsed?.ok) {
      upsertServer({ url: parsed.url, token: value })
      setActiveServerURL(parsed.url, pageURL)
      setWorldLink(undefined)
    } else saveServerToken(activeURL, value)
  }
  const fallbackActions = !worldLink ? (
    <ServerFallbackActions
      onSwitchDefault={
        isRemote && hasPageRuntime
          ? () => setActiveServerURL("", pageURL)
          : undefined
      }
      onManageServers={() => setManagerOpen(true)}
    />
  ) : undefined

  const runtimePage = (
    <>
      {!connection ? (
        <TokenEntryPage
          isTokenInvalid={false}
          serverLabel={serverLabel}
          onSubmit={submitToken}
          footer={fallbackActions}
        />
      ) : (
        <ConnectionGate
          connection={connection}
          serverLabel={serverLabel}
          fallbackActions={fallbackActions}
          onSubmitToken={submitToken}
        >
          {children}
        </ConnectionGate>
      )}
      <ServerManagerDialog
        connection={connection}
        open={managerOpen}
        onOpenChange={setManagerOpen}
        pageURL={pageURL}
        entries={entries}
      />
    </>
  )
  if (!hasPageRuntime && !worldLink && (showHome || !isRemote))
    return (
      <StandalonePage title={t("connection.servers.myServers")}>
        <div className="flex flex-col gap-4">
          {entries.map((server) => (
            <Button
              key={server.url}
              variant="outline"
              className="h-auto min-w-0 justify-start py-2 text-ui"
              onClick={() => {
                setActiveServerURL(server.url, pageURL)
                setShowHome(false)
              }}
            >
              <ServerIcon />
              <ServerListLabel server={server} />
            </Button>
          ))}
          <RemoteLinkInput onConnect={() => setShowHome(false)} />
          <Button
            variant="ghost"
            className="self-start text-ui"
            onClick={() => setManagerOpen(true)}
          >
            {t("connection.servers.connectByAddress")}
          </Button>
          {role === "control" && (
            <NavigationButton className="text-link" to="/world/admin">
              {t("worldSettings.admin")}
            </NavigationButton>
          )}
        </div>
        <ServerManagerDialog
          open={managerOpen}
          onOpenChange={setManagerOpen}
          onSwitch={() => setShowHome(false)}
          initialMode="form"
          entries={entries}
          pageURL={pageURL}
        />
      </StandalonePage>
    )
  return (
    <PageServersContext.Provider value={entries}>
      {runtimePage}
    </PageServersContext.Provider>
  )
}

const PageServersContext = createContext<ServerEntry[]>([])
// eslint-disable-next-line react-refresh/only-export-components
export function usePageServers() {
  return useContext(PageServersContext)
}

function RemoteLinkInput({
  onConnect,
  publicKey,
}: {
  onConnect?: () => void
  publicKey?: string
}) {
  const { t } = useTranslation()
  const [value, setValue] = useState("")
  const [invalid, setInvalid] = useState(false)
  function connect() {
    const p = parseServerInput(value)
    if (
      !p.ok ||
      !p.worldTarget ||
      (publicKey && publicKey !== p.worldTarget.public_key)
    ) {
      setInvalid(true)
      return
    }
    if (!p.token) {
      window.location.hash = value.slice(value.indexOf("#") + 1)
      return
    }
    upsertServer({ url: p.url, token: p.token })
    setActiveServerURL(p.url, getPageServerURL(window.location))
    onConnect?.()
  }
  return (
    <form
      className="flex min-w-0 flex-col gap-2"
      onSubmit={(e) => {
        e.preventDefault()
        connect()
      }}
    >
      <label className="text-ui" htmlFor="paste-world-link">
        {t("connection.servers.pasteLink")}
      </label>
      <Input
        id="paste-world-link"
        className="text-ui"
        value={value}
        onChange={(e) => {
          setValue(e.target.value)
          setInvalid(false)
        }}
        autoComplete="off"
        type="password"
      />
      {invalid && (
        <p role="alert" className="text-caption color-destructive">
          {t("worldSettings.invalidLink")}
        </p>
      )}
      <Button type="submit" className="text-ui" disabled={!value.trim()}>
        {t("connection.servers.connect")}
      </Button>
    </form>
  )
}

// 连接前可以选择其他服务器，连接远端时也可以切回本页服务器。
function ServerFallbackActions({
  onSwitchDefault,
  onManageServers,
}: {
  onSwitchDefault?: () => void
  onManageServers: () => void
}) {
  const { t } = useTranslation()
  return (
    <div className="flex flex-wrap items-center justify-center gap-2 border-t border-border pt-3">
      {onSwitchDefault && (
        <Button size="sm" variant="outline" onClick={onSwitchDefault}>
          {t("connection.servers.switchDefault")}
        </Button>
      )}
      <Button size="sm" variant="ghost" onClick={onManageServers}>
        {t("connection.servers.switchOther")}
      </Button>
    </div>
  )
}

// 运行连接，重连后让这个 serve 的查询过期，并按连接状态决定显示提示还是子树
function ConnectionGate({
  connection,
  serverLabel,
  fallbackActions,
  onSubmitToken,
  children,
}: {
  connection: ServerConnection
  serverLabel?: string
  onSubmitToken: (value: string) => void
  fallbackActions?: ReactNode
  children: ReactNode
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const { state, reason } = useStore(connection.$status)
  const serverID = useStore(connection.$serverID)
  const runningConversationsSync = useMemo(
    () => new RunningConversationsSync(connection),
    [connection]
  )
  const runningTerminalsSync = useMemo(
    () => new RunningTerminalsSync(connection),
    [connection]
  )
  const url = connection.entry.url
  useEffect(() => {
    // 1. 登记连上后的处理：首次连上时还没有查询，跳过；之后每次重连让全部查询过期。
    let hasConnected = false
    const stopRefreshingAfterReconnect = connection.onConnected(() => {
      const result = connection.$identity.get()!
      saveServerIdentity(connection.entry, result)
      if (hasConnected)
        invalidateAfterReconnect(queryClient, connection.getServerID())
      hasConnected = true
    })
    // 2. 登记运行集合和终端列表订阅，运行连接并保存 server_id。
    const stopRunningConversationsSync = runningConversationsSync.run()
    const stopRunningTerminalsSync = runningTerminalsSync.run()
    const stop = connection.run()
    return () => {
      stopRefreshingAfterReconnect()
      stopRunningTerminalsSync()
      stopRunningConversationsSync()
      stop()
    }
  }, [
    connection,
    queryClient,
    runningConversationsSync,
    runningTerminalsSync,
    url,
  ])
  if (state === "unauthorized" && connection.entry.worldTarget)
    return (
      <StandalonePage
        title={t("connection.servers.tokenExpired")}
        titleClassName="font-(family-name:--appearance-body)"
        description={t("connection.servers.tokenChanged")}
      >
        <div className="flex flex-col gap-4">
          <RemoteLinkInput
            publicKey={connection.entry.worldTarget.public_key}
          />
          {fallbackActions}
        </div>
      </StandalonePage>
    )
  if (state === "unauthorized")
    return (
      <TokenEntryPage
        isTokenInvalid
        serverLabel={serverLabel}
        onSubmit={onSubmitToken}
        footer={fallbackActions}
      />
    )
  if (state === "incompatible")
    return (
      <StandalonePage
        title={t("connection.incompatible.title")}
        description={t("connection.incompatible.description")}
      >
        {fallbackActions}
      </StandalonePage>
    )
  if (serverID === undefined) {
    const host = getServerDisplay(connection.entry).address
    if (connection.entry.worldTarget && reason)
      return (
        <StandalonePage
          title={t("connection.servers.offline", { name: serverLabel || host })}
        >
          {fallbackActions}
        </StandalonePage>
      )
    return (
      <StandalonePage
        title={
          serverLabel
            ? t("connection.connecting.remoteTitle", { name: serverLabel })
            : t("connection.connecting.title")
        }
        description={
          state === "reconnecting" && !connection.entry.worldTarget
            ? reason || t("connection.connecting.retrying", { host })
            : undefined
        }
      >
        <div className="flex flex-col gap-4">
          <LatticeLoader aria-label={t("common.loading")} />
          {fallbackActions}
        </div>
      </StandalonePage>
    )
  }
  return (
    <ServerConnectionContext.Provider key={serverID} value={connection}>
      <RunningConversationsSyncContext.Provider
        value={runningConversationsSync}
      >
        <RunningTerminalsSyncContext.Provider value={runningTerminalsSync}>
          {children}
        </RunningTerminalsSyncContext.Provider>
      </RunningConversationsSyncContext.Provider>
    </ServerConnectionContext.Provider>
  )
}
