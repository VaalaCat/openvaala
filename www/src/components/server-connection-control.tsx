import { useState, type Ref } from "react"
import { useStore } from "@nanostores/react"
import { Check, Plus, RefreshCw, Server } from "lucide"
import { MorphIcon } from "morphicons/react"
import { motion, useIsPresent } from "motion/react"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import { StatusDot } from "@/components/base/status-dot"
import { ServerManagerDialog } from "@/components/server-manager-dialog"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover"
import { Separator } from "@/components/ui/separator"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import type { ConnectionState } from "@/lib/api/server-connection"
import {
  $activeServerURL,
  $servers,
  getPageServerURL,
  getServerHost,
  setActiveServerURL,
} from "@/lib/api/servers"
import { cn } from "@/lib/utils"

const connectionStatuses = {
  connected: { tone: "success", labelKey: "connection.state.connected" },
  connecting: { tone: "warning", labelKey: "connection.state.connecting" },
  reconnecting: { tone: "warning", labelKey: "connection.state.reconnecting" },
  unauthorized: {
    tone: "destructive",
    labelKey: "connection.state.unauthorized",
  },
  incompatible: {
    tone: "destructive",
    labelKey: "connection.state.incompatible",
  },
} as const satisfies Record<
  ConnectionState,
  { tone: "success" | "warning" | "destructive"; labelKey: ParseKeys }
>

// 服务器图标显示连接状态，点击弹出快捷切换器；断线时在面板顶部保留原地重连按钮。
export function ServerConnectionControl({
  className,
  layoutId,
  ref,
}: {
  className?: string
  layoutId?: string
  ref?: Ref<HTMLDivElement>
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const state = useStore(connection.$state)
  const pending = useStore(connection.$isManualReconnectPending)
  const servers = useStore($servers)
  const activeServerURL = useStore($activeServerURL)
  const isPresent = useIsPresent()
  const [popoverOpen, setPopoverOpen] = useState(false)
  const [managerOpen, setManagerOpen] = useState(false)

  const pageURL = getPageServerURL(window.location)
  const activeURL = activeServerURL || pageURL
  const remoteServers = servers.filter((server) => server.url !== pageURL)
  const activeEntry = servers.find((server) => server.url === activeURL)
  const activeName =
    activeURL === pageURL
      ? t("connection.servers.pageDefault")
      : activeEntry?.name || getServerHost(activeURL)

  const canReconnect = state === "reconnecting" || state === "connecting"
  const { tone, labelKey } = connectionStatuses[state]
  const tooltipLabel = `${activeName} · ${t(labelKey)}`

  function switchTo(url: string) {
    setPopoverOpen(false)
    if (url !== activeURL) setActiveServerURL(url, pageURL)
  }

  return (
    <>
      <motion.div
        ref={ref}
        layoutId={layoutId}
        layout="position"
        transition={{ duration: 0.2, ease: "easeInOut" }}
        className="size-8 shrink-0"
        inert={!isPresent}
        aria-hidden={!isPresent || undefined}
      >
        <Popover open={popoverOpen} onOpenChange={setPopoverOpen}>
          <Tooltip disabled={!isPresent || popoverOpen}>
            <TooltipTrigger
              delay={600}
              closeOnClick
              render={
                <PopoverTrigger
                  render={
                    <Button
                      variant="ghost"
                      size="icon"
                      className={cn(
                        "relative text-ui transition-colors",
                        className
                      )}
                      aria-label={tooltipLabel}
                      aria-busy={pending}
                    />
                  }
                />
              }
            >
              <span className="relative flex size-4">
                <span
                  className={cn(
                    "server-reconnect-icon flex",
                    pending && "animate-spin"
                  )}
                >
                  <MorphIcon
                    icon={pending ? RefreshCw : Server}
                    size={16}
                    aria-hidden
                  />
                </span>
                <StatusDot
                  tone={tone}
                  label={t(labelKey)}
                  className="absolute -right-1 -bottom-1 ring-2 ring-background"
                />
              </span>
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {tooltipLabel}
            </TooltipContent>
          </Tooltip>

          <PopoverContent align="start" className="w-76 gap-2 p-2">
            {/* 当前连接概览 + 断线重连 */}
            <div className="flex items-start justify-between gap-2 px-1.5 py-1">
              <div className="min-w-0">
                <div className="flex items-center gap-1.5 text-ui font-medium">
                  <StatusDot tone={tone} label={t(labelKey)} />
                  <span className="truncate">{activeName}</span>
                  <span className="shrink-0 text-caption font-normal text-muted-foreground">
                    · {t(labelKey)}
                  </span>
                </div>
                <div className="truncate pl-3.5 text-caption text-muted-foreground">
                  {getServerHost(activeURL)}
                </div>
              </div>
              {canReconnect && (
                <Button
                  size="sm"
                  variant="outline"
                  disabled={pending}
                  className="shrink-0"
                  onClick={() => connection.reconnect()}
                >
                  {pending
                    ? t("connection.reconnectPending")
                    : t("connection.reconnect")}
                </Button>
              )}
            </div>

            <Separator />

            {/* 本页默认 + 已保存远端快捷切换 */}
            <div className="flex flex-col gap-0.5">
              <button
                type="button"
                className="flex w-full items-center justify-between gap-2 px-1.5 py-1.5 text-left text-ui transition-colors hover:bg-accent"
                onClick={() => switchTo(pageURL)}
              >
                <span className="flex min-w-0 items-center gap-2">
                  <span className="flex size-3.5 shrink-0 items-center justify-center">
                    {activeURL === pageURL && <MorphIcon icon={Check} size={14} />}
                  </span>
                  <span className="truncate">
                    {t("connection.servers.pageDefault")}
                  </span>
                </span>
                <span className="shrink-0 truncate text-caption text-muted-foreground">
                  {getServerHost(pageURL)}
                </span>
              </button>

              {remoteServers.map((server) => {
                const isSelected = activeURL === server.url
                const displayName = server.name || getServerHost(server.url)
                return (
                  <button
                    key={server.url}
                    type="button"
                    className="flex w-full items-center justify-between gap-2 px-1.5 py-1.5 text-left text-ui transition-colors hover:bg-accent"
                    onClick={() => switchTo(server.url)}
                  >
                    <span className="flex min-w-0 items-center gap-2">
                      <span className="flex size-3.5 shrink-0 items-center justify-center">
                        {isSelected && <MorphIcon icon={Check} size={14} />}
                      </span>
                      <span className="truncate">{displayName}</span>
                    </span>
                    <span className="shrink-0 truncate text-caption text-muted-foreground">
                      {getServerHost(server.url)}
                    </span>
                  </button>
                )
              })}
            </div>

            <Separator />

            <button
              type="button"
              className="flex w-full items-center gap-2 px-1.5 py-1.5 text-left text-ui transition-colors hover:bg-accent"
              onClick={() => {
                setPopoverOpen(false)
                setManagerOpen(true)
              }}
            >
              <MorphIcon icon={Plus} size={14} />
              <span>{t("connection.servers.manageAction")}</span>
            </button>
          </PopoverContent>
        </Popover>
      </motion.div>

      <ServerManagerDialog
        open={managerOpen}
        onOpenChange={setManagerOpen}
        pageURL={pageURL}
      />
    </>
  )
}
