import type {
  ConversationID,
  NodeStatus,
  RecordCursor,
  ThreadBinding,
  WorldMessage,
  WorldMessageDirection,
  WorldMessageStatus,
  WorldPeer as RPCWorldPeer,
} from "./protocol.gen"
import type { ServerConnection } from "./server-connection"

export type { WorldMessage, WorldMessageDirection, WorldMessageStatus }
export type WorldStatus = NodeStatus
export type WorldThreadBinding = ThreadBinding

export type WorldPeer = Omit<RPCWorldPeer, "targets"> & {
  targets: RPCWorldPeer["targets"] | null
}

type WorldAddress = { node: string; domain: string; target?: string }

export type WorldMessagesFilter = {
  direction?: WorldMessageDirection
  peerAddrs?: readonly string[]
  kind?: WorldMessage["kind"]
  status?: WorldMessageStatus
  conversationID?: ConversationID
  threadID?: string
}

export type WorldPage<T> = {
  items: T[]
  next_cursor?: RecordCursor<string>
}

export type WorldPageOptions = {
  limit?: number
  cursor?: RecordCursor<string>
}

export function parseWorldAddress(
  address: string
): (WorldAddress & { nodeAddress: string }) | undefined {
  const parts = /^([^@/\s]+)@([^@/\s]+)(?:\/([^@/\s]+))?$/.exec(address)
  if (!parts) return undefined
  const [, node, domain, target] = parts
  return { node, domain, target, nodeAddress: `${node}@${domain}` }
}

export function getWorldSenderLabel(peerAddress: string, localDomain?: string) {
  const sender = parseWorldAddress(peerAddress)
  return sender && sender.domain === localDomain ? sender.node : peerAddress
}

export function getWorldContactAddress(address: string, localDomain?: string) {
  if (parseWorldAddress(address)) return address
  if (!localDomain) return undefined
  const parts =
    /^([a-z0-9][a-z0-9_-]{0,62})(\/[a-z0-9][a-z0-9_-]{0,62})?$/.exec(address)
  if (parts) return `${parts[1]}@${localDomain}${parts[2] ?? ""}`
}

export function getInboundThreadBindings(
  bindings: readonly WorldThreadBinding[]
) {
  const byConversation = new Map<ConversationID, WorldThreadBinding>()
  for (const binding of bindings) {
    if (binding.direction !== "inbound" || !binding.conversation_id) continue
    const previous = byConversation.get(binding.conversation_id)
    if (!previous || binding.created_at > previous.created_at)
      byConversation.set(binding.conversation_id, binding)
  }
  return byConversation
}

export function getVisibleTargets({ targets }: Pick<WorldPeer, "targets">) {
  return targets?.length === 1 && targets[0].name === "default"
    ? []
    : (targets ?? [])
}

export function selectFollowUpThread(messages: readonly WorldMessage[]) {
  let newest: WorldMessage | undefined
  for (const message of messages) {
    if (!newest || message.created_at > newest.created_at) newest = message
  }
  return newest
}

export type WorldEvent = "status_changed" | "peers_changed" | "message_updated"

type WorldConnection = Pick<ServerConnection, "call">

export async function getWorldPeers(connection: WorldConnection) {
  return connection.call("world/listPeers", {})
}

export function getWorldMessages(
  connection: WorldConnection,
  filter?: WorldMessagesFilter,
  page?: WorldPageOptions
) {
  return connection.call("world/listMessages", {
    direction: filter?.direction,
    peer_addr: filter?.peerAddrs ? [...filter.peerAddrs] : undefined,
    kind: filter?.kind,
    status: filter?.status,
    conversation_id: filter?.conversationID,
    thread_id: filter?.threadID,
    limit: page?.limit,
    cursor_id: page?.cursor?.id,
  })
}

export function getWorldThreads(
  connection: WorldConnection,
  conversationID?: ConversationID,
  page?: WorldPageOptions
) {
  return connection.call("world/listThreads", {
    conversation_id: conversationID,
    limit: page?.limit,
    cursor_id: page?.cursor?.id,
    cursor_updated_at: page?.cursor?.updated_at,
  })
}

// 连接建立后登记订阅，停止监听时在同一连接上退订。
export function listenWorldUpdates(
  connection: Pick<
    ServerConnection,
    "call" | "listenServer" | "onConnected" | "$state"
  >,
  onSubscribe: () => void,
  onEvent: (event: WorldEvent) => void
) {
  let listening = true
  const subscribe = () => {
    void connection.call("world/subscribe", {}).then(
      () => {
        if (listening) onSubscribe()
      },
      () => undefined
    )
  }
  const stopListening = connection.listenServer((notification) => {
    if (notification.method === "world/messageUpdatedClosed") {
      subscribe()
      return
    }
    if (notification.method !== "world/messageUpdated") return
    const { event } = notification.params
    if (
      event === "status_changed" ||
      event === "peers_changed" ||
      event === "message_updated"
    )
      onEvent(event)
  })
  const stopFollowingConnection = connection.onConnected(subscribe)
  if (connection.$state.get() === "connected") subscribe()
  return () => {
    listening = false
    stopFollowingConnection()
    stopListening()
    if (connection.$state.get() === "connected")
      void connection.call("world/unsubscribe", {}).catch(() => undefined)
  }
}
