import { useState, type Ref } from "react"
import { useStore } from "@nanostores/react"
import { RefreshCw, Server } from "lucide"
import { MorphIcon } from "morphicons/react"
import { motion, useIsPresent, useReducedMotion } from "motion/react"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import { StatusDot } from "@/components/base/status-dot"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import type { ConnectionState } from "@/lib/api/server-connection"
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

// 服务器图标显示连接状态，断线时原位提供一次重连操作。
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
  const reduceMotion = useReducedMotion()
  const isPresent = useIsPresent()
  const [hovered, setHovered] = useState(false)
  const [focused, setFocused] = useState(false)
  const canReconnect = state === "reconnecting" || state === "connecting"
  const { tone, labelKey } = connectionStatuses[state]
  const showRefresh = canReconnect && (pending || hovered || focused)
  const label = pending
    ? t("connection.reconnectPending")
    : canReconnect
      ? t("connection.reconnect")
      : t(labelKey)
  return (
    <motion.div
      ref={ref}
      layoutId={reduceMotion ? undefined : layoutId}
      layout={reduceMotion ? false : "position"}
      transition={{ duration: reduceMotion ? 0 : 0.2, ease: "easeInOut" }}
      className="size-8 shrink-0"
      inert={!isPresent}
      aria-hidden={!isPresent || undefined}
    >
      <Tooltip disabled={!isPresent}>
        <TooltipTrigger
          delay={600}
          closeOnClick
          render={
            <Button
              variant="ghost"
              size="icon"
              className={cn("relative text-ui transition-colors", className)}
              aria-label={label}
              aria-busy={pending}
              disabled={!canReconnect || pending}
              focusableWhenDisabled
              onPointerEnter={(event) => {
                if (event.pointerType !== "touch") setHovered(true)
              }}
              onPointerLeave={() => setHovered(false)}
              onFocus={(event) =>
                setFocused(event.currentTarget.matches(":focus-visible"))
              }
              onBlur={() => setFocused(false)}
              onClick={() => connection.reconnect()}
            />
          }
        >
          <span className="relative flex size-4">
            <span
              className={cn(
                "server-reconnect-icon flex",
                pending && "animate-spin motion-reduce:animate-none"
              )}
            >
              <MorphIcon
                icon={showRefresh ? RefreshCw : Server}
                reducedMotion="user"
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
        <TooltipContent className="text-caption">{label}</TooltipContent>
      </Tooltip>
    </motion.div>
  )
}
