import type { MessageRow } from "./item-renderers"

// 对话历史顶部是否显示分叉来源提示？旁支一直显示；普通分叉要等最早一页加载出来，
// 历史开头已经是指向来源对话的 session_fork 分界（压缩过的分叉）时由它表达，不再重复显示
export function shouldShowForkOrigin(
  kind: "side-conversation" | "fork",
  rows: MessageRow[],
  hasEarlierItems: boolean
): boolean {
  if (kind === "side-conversation") return true
  if (hasEarlierItems) return false
  const first = rows[0]
  return !(
    first?.kind === "item" &&
    first.view.item.session_fork?.source_conversation_id
  )
}
