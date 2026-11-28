import type { WorldPeer } from "./api/world"
import {
  getArrayField,
  getBooleanField,
  getStringField,
} from "./tool-arguments"

// 单条工具结果是简化快照，线程结果使用完整发件记录。
export function readWorldToolMessages(content: unknown) {
  return (Array.isArray(content) ? content : [content]).flatMap((message) => {
    const messageID = getStringField(message, "message_id")
    if (!messageID) return []
    return [
      {
        messageID,
        status: getStringField(message, "status"),
        reply:
          getStringField(message, "reply_body") ||
          getStringField(message, "reply"),
        errorCode: getStringField(message, "error_code"),
        error:
          getStringField(message, "error_text") ||
          getStringField(message, "error"),
      },
    ]
  })
}

export function readWorldToolPeers(content: unknown): WorldPeer[] {
  return getArrayField(content, "peers").flatMap((peer) => {
    const address = getStringField(peer, "address")
    if (!address) return []
    return [
      {
        address,
        node_name: getStringField(peer, "node_name") ?? "",
        online: getBooleanField(peer, "online") === true,
        targets: getArrayField(peer, "targets").flatMap((target) => {
          const name = getStringField(target, "name")
          const visibility = getStringField(target, "visibility")
          if (!name || !visibility) return []
          const description = getStringField(target, "description")
          return [{ name, visibility, ...(description && { description }) }]
        }),
      },
    ]
  })
}
