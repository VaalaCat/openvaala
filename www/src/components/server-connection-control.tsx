import { useState, type Ref } from "react"
import { useStore } from "@nanostores/react"
import { RefreshCw, Server } from "lucide"
import { MorphIcon } from "morphicons/react"
import { motion, useIsPresent } from "motion/react"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import { StatusDot } from "@/components/base/status-dot"
import { ServerListLabel } from "@/components/server-list-label"
import { ServerManagerDialog } from "@/components/server-manager-dialog"
import {
  useServerConnection,
  usePageServers,
} from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import type { ConnectionState } from "@/lib/api/server-connection"
import {
  $activeServerURL,
  getPageServerURL,
  getServerDisplay,
  connectToServer,
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

// 服务器图标显示连接状态，断线时可从切换菜单原地重连。
export function ServerConnectionControl({
  className,
  layoutId,
  showTooltip = true,
  ref,
}: {
  className?: string
  layoutId?: string
  showTooltip?: boolean
  ref?: Ref<HTMLDivElement>
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const { state, reason } = useStore(connection.$status)
  const pending = useStore(connection.$isManualReconnectPending)
  const entries = usePageServers()
  const activeServerURL = useStore($activeServerURL)
  const isPresent = useIsPresent()
  const [menuOpen, setMenuOpen] = useState(false)
  const [managerOpen, setManagerOpen] = useState(false)

  const pageURL = getPageServerURL(window.location)
  const activeURL = activeServerURL || pageURL
  const activeName = getServerDisplay(
    entries.find((s) => s.url === activeURL) ?? connection.entry
  ).name

  const { tone, labelKey } = connectionStatuses[state]
  const tooltipLabel = `${activeName} · ${t(labelKey)}`

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
        <DropdownMenu open={menuOpen} onOpenChange={setMenuOpen}>
          <Tooltip disabled={!showTooltip || !isPresent || menuOpen}>
            <TooltipTrigger
              delay={600}
              closeOnClick
              render={
                <DropdownMenuTrigger
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
              {reason && connection.entry.worldTarget
                ? t("connection.servers.offline", { name: activeName })
                : reason || tooltipLabel}
            </TooltipContent>
          </Tooltip>

          <DropdownMenuContent
            align="start"
            className="w-max max-w-(--available-width) min-w-40"
          >
            <DropdownMenuRadioGroup
              value={activeURL}
              onValueChange={(url: string) =>
                connectToServer(url, pageURL, connection)
              }
            >
              {entries.map((server) => {
                const disconnected =
                  server.url === activeURL && state === "reconnecting"
                return (
                  <DropdownMenuRadioItem
                    key={server.url}
                    value={server.url}
                    closeOnClick={!disconnected}
                    disabled={disconnected && pending}
                    className="text-ui"
                  >
                    <ServerListLabel
                      server={server}
                      disconnected={disconnected}
                    />
                  </DropdownMenuRadioItem>
                )
              })}
            </DropdownMenuRadioGroup>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              className="text-ui"
              onClick={() => setManagerOpen(true)}
            >
              {t("connection.servers.manageAction")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </motion.div>

      <ServerManagerDialog
        connection={connection}
        entries={entries}
        open={managerOpen}
        onOpenChange={setManagerOpen}
        pageURL={pageURL}
      />
    </>
  )
}
