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
import { useTranslation } from "react-i18next"
import { TokenEntryPage } from "@/app/token-entry"
import { StandalonePage } from "@/components/base/standalone-page"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ServerConnection } from "@/lib/api/server-connection"
import { RunningConversationsSync } from "@/lib/api/running-conversations-sync"
import {
  $servers,
  getPageServerURL,
  saveServerID,
  saveServerToken,
  takeTokenFromHash,
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

// 页面所在 serve 的连接，只在 ServerConnectionProvider 连上之后的子树里可用
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

// 建立并保持页面所在 serve 的连接；连上之前显示 token 输入页、连接中或版本不一致的提示，连上之后渲染子树
export function ServerConnectionProvider({
  children,
}: {
  children: ReactNode
}) {
  const pageURL = getPageServerURL(window.location)
  useState(takePageToken)
  // 在 token 输入页把 openvaala web 打印的地址贴进同一个标签页，浏览器只换 #，不重新加载
  useEffect(() => {
    window.addEventListener("hashchange", takePageToken)
    return () => window.removeEventListener("hashchange", takePageToken)
  }, [])
  const servers = useStore($servers)
  const token = servers.find((server) => server.url === pageURL)?.token
  const connection = useMemo(
    () =>
      token === undefined
        ? undefined
        : new ServerConnection({ url: pageURL, token }),
    [pageURL, token]
  )
  if (!connection)
    return (
      <TokenEntryPage
        isTokenInvalid={false}
        onSubmit={(token) => saveServerToken(pageURL, token)}
      />
    )
  return <ConnectionGate connection={connection}>{children}</ConnectionGate>
}

// 运行连接，重连后让这个 serve 的查询过期，并按连接状态决定显示提示还是子树
function ConnectionGate({
  connection,
  children,
}: {
  connection: ServerConnection
  children: ReactNode
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const state = useStore(connection.$state)
  const serverID = useStore(connection.$serverID)
  const runningConversationsSync = useMemo(
    () => new RunningConversationsSync(connection),
    [connection]
  )
  const url = connection.entry.url
  useEffect(() => {
    // 1. 登记连上后的处理：首次连上时还没有查询，跳过；之后每次重连让全部查询过期。
    let hasConnected = false
    const stopRefreshingAfterReconnect = connection.onConnected(() => {
      if (hasConnected)
        invalidateAfterReconnect(queryClient, connection.getServerID())
      hasConnected = true
    })
    // 2. 登记运行集合订阅，运行连接并保存 server_id。
    const stopRunningConversationsSync = runningConversationsSync.run()
    const stop = connection.run()
    const stopSavingServerID = connection.$serverID.listen((serverID) => {
      if (serverID) saveServerID(url, serverID)
    })
    return () => {
      stopSavingServerID()
      stopRefreshingAfterReconnect()
      stopRunningConversationsSync()
      stop()
    }
  }, [connection, queryClient, runningConversationsSync, url])
  if (state === "unauthorized")
    return (
      <TokenEntryPage
        isTokenInvalid
        onSubmit={(token) => saveServerToken(url, token)}
      />
    )
  if (state === "incompatible")
    return (
      <StandalonePage
        title={t("connection.incompatible.title")}
        description={t("connection.incompatible.description")}
      />
    )
  if (serverID === undefined) {
    const host = new URL(url).host
    return (
      <StandalonePage
        title={t("connection.connecting.title")}
        description={
          state === "reconnecting"
            ? t("connection.connecting.retrying", { host })
            : undefined
        }
      >
        <LatticeLoader aria-label={t("common.loading")} />
      </StandalonePage>
    )
  }
  return (
    <ServerConnectionContext.Provider key={serverID} value={connection}>
      <RunningConversationsSyncContext.Provider
        value={runningConversationsSync}
      >
        {children}
      </RunningConversationsSyncContext.Provider>
    </ServerConnectionContext.Provider>
  )
}
