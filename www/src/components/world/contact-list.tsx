import type { ReactNode } from "react"
import type { UseQueryResult } from "@tanstack/react-query"
import { BotIcon, ServerIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ListEmpty } from "@/components/base/list-empty"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { RequestError } from "@/components/base/request-error"
import { StatusDot } from "@/components/base/status-dot"
import { Button } from "@/components/ui/button"
import {
  getVisibleTargets,
  type WorldMessageStatus,
  parseWorldAddress,
} from "@/lib/api/world"
import type { ListPeersResult } from "@/lib/api/protocol.gen"
import { cn } from "@/lib/utils"
import { worldMessageStatusLabels } from "./message-bubble-helpers"

// eslint-disable-next-line react-refresh/only-export-components -- 共用列表同时提供目标可见性规则。
export { getVisibleTargets }

export function WorldIconWithStatus({
  icon,
  online,
  label,
  ringClassName = "ring-background",
}: {
  icon: ReactNode
  online: boolean
  label: string
  ringClassName?: string
}) {
  return (
    <span className="relative flex size-4 shrink-0">
      {icon}
      <StatusDot
        tone={online ? "success" : "warning"}
        label={label}
        className={cn("absolute -right-1 -bottom-1 ring-2", ringClassName)}
      />
    </span>
  )
}

export function WorldDeliveryStatus({ status }: { status: string }) {
  const { t } = useTranslation()
  return (
    <span className="text-ui">
      {Object.hasOwn(worldMessageStatusLabels, status)
        ? t(worldMessageStatusLabels[status as WorldMessageStatus])
        : status}
    </span>
  )
}

export function WorldContactList({
  peers,
  selected,
  onSelect,
  disabled = false,
  bordered = true,
}: {
  peers: UseQueryResult<ListPeersResult>
  selected: string
  onSelect(address: string): void
  disabled?: boolean
  bordered?: boolean
}) {
  const { t } = useTranslation()
  if (peers.isPending)
    return <LatticeLoader className="m-3" aria-label={t("common.loading")} />
  if (peers.isError && !peers.data)
    return (
      <div className="p-3">
        <RequestError
          error={peers.error}
          onRetry={() => void peers.refetch()}
        />
      </div>
    )
  if (!peers.data?.peers?.length && !peers.data?.recent?.length)
    return <ListEmpty title={t("world.emptyContacts")} />

  return (
    <>
      <ul className={cn("min-w-0 divide-y", bordered && "border")}>
        {peers.data.peers?.map((peer) => {
          const targets = getVisibleTargets(peer)
          const nodeSelected =
            selected === peer.address ||
            (peer.targets?.length === 1 &&
              targets.length === 0 &&
              selected === `${peer.address}/default`)
          return (
            <li key={peer.address} className="min-w-0">
              <Button
                type="button"
                variant="ghost"
                className={cn(
                  "h-10 w-full min-w-0 justify-start gap-2 px-2 text-ui",
                  nodeSelected && "bg-muted hover:bg-muted"
                )}
                aria-pressed={nodeSelected}
                onClick={() => onSelect(peer.address)}
                disabled={disabled}
              >
                <WorldIconWithStatus
                  icon={<ServerIcon className="size-4" />}
                  online={peer.online}
                  label={t(peer.online ? "world.online" : "world.offline")}
                  ringClassName={
                    nodeSelected ? "ring-muted" : "ring-background"
                  }
                />
                <OverflowTooltip className="max-w-1/2 min-w-0 truncate text-ui">
                  {peer.node_name || peer.address}
                </OverflowTooltip>
                {peer.node_name && (
                  <OverflowTooltip className="min-w-0 truncate text-ui color-subtle">
                    {peer.address}
                  </OverflowTooltip>
                )}
              </Button>
              {targets.length > 0 && (
                <ul
                  className="divide-y border-t"
                  aria-label={t("world.targets")}
                >
                  {targets.map((target) => {
                    const address = `${peer.address}/${target.name}`
                    return (
                      <li key={address}>
                        <Button
                          type="button"
                          variant="ghost"
                          className={cn(
                            "w-full min-w-0 justify-start gap-2 pr-2 pl-8 text-ui",
                            selected === address && "bg-muted hover:bg-muted"
                          )}
                          aria-pressed={selected === address}
                          onClick={() => onSelect(address)}
                          disabled={disabled}
                        >
                          <BotIcon className="size-3.5 shrink-0" />
                          <OverflowTooltip className="max-w-1/2 min-w-0 truncate text-ui">
                            {target.name}
                          </OverflowTooltip>
                          {target.description && (
                            <OverflowTooltip className="min-w-0 truncate text-ui color-subtle">
                              {target.description}
                            </OverflowTooltip>
                          )}
                        </Button>
                      </li>
                    )
                  })}
                </ul>
              )}
            </li>
          )
        })}
      </ul>
      {!!peers.data.recent?.length && (
        <section aria-label={t("world.recent")}>
          <h3 className="px-2 py-1 text-section">{t("world.recent")}</h3>
          <ul className="divide-y">
            {peers.data.recent.map(({ address }) => {
              const peer = parseWorldAddress(address)!
              return (
                <li key={address}>
                  <Button
                    variant="ghost"
                    type="button"
                    className="w-full justify-start gap-2 px-2 text-ui"
                    aria-pressed={address === selected}
                    onClick={() => onSelect(address)}
                    disabled={disabled}
                  >
                    <ServerIcon />
                    {peer.node} · {peer.domain}
                  </Button>
                </li>
              )
            })}
          </ul>
        </section>
      )}
    </>
  )
}
