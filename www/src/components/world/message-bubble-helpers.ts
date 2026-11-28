import type { ParseKeys } from "i18next"
import type { InfiniteData } from "@tanstack/react-query"
import type { WorldMessage, WorldPage } from "@/lib/api/world"

export function shouldShowTimeDivider(
  prevMs: number | undefined,
  currMs: number
): boolean {
  return (
    prevMs === undefined ||
    Math.abs(currMs - prevMs) > 5 * 60_000 ||
    new Date(prevMs).toDateString() !== new Date(currMs).toDateString()
  )
}

export function mergeSentMessagePage(
  data: InfiniteData<WorldPage<WorldMessage>> | undefined,
  sent: WorldMessage
): InfiniteData<WorldPage<WorldMessage>> {
  if (!data) return { pages: [{ items: [sent] }], pageParams: [undefined] }

  let message = sent
  // 按服务端的消息 ID 倒序插入，保留已经返回的分页游标。
  const pageIndex = data.pages.findIndex((page) =>
    page.items.some((item) => item.message_id <= sent.message_id)
  )
  const pages = data.pages.map((page) => {
    const cached = page.items.find(
      (item) => item.message_id === sent.message_id
    )
    if (!cached) return page
    // 同毫秒的查询结果比发送响应更晚读取，保留已读到的投递状态。
    if (cached.updated_at >= message.updated_at) message = cached
    return {
      ...page,
      items: page.items.filter((item) => item.message_id !== sent.message_id),
    }
  })
  const index = pageIndex === -1 ? pages.length - 1 : pageIndex
  const page = pages[index]
  const before = page.items.findIndex(
    (item) => item.message_id < sent.message_id
  )
  const items = page.items.slice()
  items.splice(before === -1 ? items.length : before, 0, message)
  pages[index] = { ...page, items }
  return { ...data, pages }
}

export const worldMessageStatusLabels = {
  pending_offline: "world.delivery.pendingOffline",
  sending: "world.delivery.sending",
  unknown: "world.delivery.unknown",
  remote_running: "world.delivery.remoteRunning",
  replied: "world.delivery.replied",
  failed: "world.delivery.failed",
  cancelled: "world.delivery.cancelled",
  accepted: "world.receipt.accepted",
  running: "world.receipt.running",
  completed: "world.receipt.completed",
} as const satisfies Record<WorldMessage["status"], string>

export type WorldErrorDescription = {
  message: ParseKeys
  action?: "models" | "domains" | "node"
}

const worldErrorHints: Record<string, WorldErrorDescription> = {
  world_node_revoked: { message: "world.errors.revoked", action: "node" },
  world_domain_name_taken: { message: "world.errors.nameTaken" },
  world_agent_no_model: { message: "world.errors.noModel", action: "models" },
  world_target_not_public: { message: "world.errors.notPublic" },
  world_sender_not_allowed: { message: "world.errors.denied" },
  world_domain_not_trusted: {
    message: "world.errors.untrusted",
    action: "domains",
  },
  world_identity_unverified: { message: "world.errors.unverified" },
  world_target_unreachable: { message: "world.errors.unreachable" },
  world_peer_not_found: { message: "world.errors.notFound" },
  world_hop_limit_exceeded: { message: "world.errors.hopLimit" },
  world_target_not_exposed: { message: "world.errors.notExposed" },
  unauthorized: { message: "world.errors.unauthorized" },
}

export function getWorldErrorHint(
  code?: string
): WorldErrorDescription | undefined {
  if (code && Object.hasOwn(worldErrorHints, code)) return worldErrorHints[code]
}
