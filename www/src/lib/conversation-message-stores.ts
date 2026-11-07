import { computed, type ReadableAtom } from "nanostores"
import type { ConversationSync } from "./api/conversation-sync"
import { $rightPanelTab } from "./appearance/preferences"
import { getMessageRows, type MessageRow } from "./item-renderers"

// ConversationMessageStores 是一个 Sync 的消息展示行：阅读区和右栏调试页签共用
export interface ConversationMessageStores {
  $messageRows: ReadableAtom<MessageRow[]>
  // 对话里是否已有 Item？还没保存的新对话没有；流式增量不触发订阅它的组件重绘
  $hasItems: ReadableAtom<boolean>
  // 只在调试页签打开时有内容，其他页签不随消息正文更新
  $debugRows: ReadableAtom<MessageRow[]>
}

const emptyMessageRows: MessageRow[] = []
const messageStores = new WeakMap<ConversationSync, ConversationMessageStores>()

// 返回 Sync 的消息展示行；对话标签页和右栏拿到的是同一份，历史页接缝和行引用只算一次
export function getConversationMessageStores(
  sync: ConversationSync
): ConversationMessageStores {
  // 1. 已经建过的直接复用。
  const existing = messageStores.get(sync)
  if (existing) return existing
  // 2. 从唯一消息源派生展示行，保留前一次行引用和历史页接缝。
  let firstKey: string | undefined
  let previous: { rows: MessageRow[]; pageSeams: ReadonlySet<string> } = {
    rows: [],
    pageSeams: new Set(),
  }
  const $messageRows = computed(sync.$items, (items) => {
    let pageSeams = previous.pageSeams
    if (firstKey !== items[0]?.key) {
      if (firstKey && items.some((view) => view.key === firstKey))
        pageSeams = new Set([...pageSeams, firstKey])
      firstKey = items[0]?.key
    }
    const rows = getMessageRows(items, pageSeams, previous)
    previous = { rows, pageSeams }
    return rows
  })
  // 3. 非调试页签不随消息正文更新。
  const $debugRows = computed([$messageRows, $rightPanelTab], (rows, tab) =>
    tab === "debug" ? rows : emptyMessageRows
  )
  const $hasItems = computed(sync.$items, (items) => items.length > 0)
  const stores = { $messageRows, $hasItems, $debugRows }
  messageStores.set(sync, stores)
  return stores
}
