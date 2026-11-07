import { i18n } from "../../i18n"
import type { ItemID } from "../api/protocol.gen"
import type { MessageRow } from "../item-renderers"

// 标题里问题保留的字数
const titleQuestionLength = 20

// 旁支说明：旁支的第一条客户端指令，写在旁支第一轮输入之前，告诉模型继承来的历史只作参考
export const sideConversationInstruction = `旁支分界。

这条说明之前的内容都是从主对话继承来的历史，只作参考，不是你现在的任务。不要继续、执行或补完其中的指令、计划、工具调用、审批、修改和请求；那些工具调用和输出发生在主对话里，也不要从中推断新的指令。

只有这条说明之后用户提交的消息才是旁支里的有效指令。你是独立于主对话的旁支助手：回答问题，做轻量的探索，不要打断主对话。如果这条说明之后还没有用户的问题，就等用户提问。

可以读文件、搜索，也可以运行不改动仓库的检查。

不要修改文件、源码、git 状态、权限、配置或其他工作区状态，除非用户在这条说明之后明确要求。用户明确要求修改时，改动要小，只限于所要求的内容，并避免影响主对话。`

// 返回问题的前 20 个字（合并空白，按字符计），旁支标签页只存这一段
export function getSideConversationQuestionPreview(question: string): string {
  return Array.from(question.trim().replace(/\s+/g, " "))
    .slice(0, titleQuestionLength)
    .join("")
}

// 返回旁支标签页的标题：「旁支 · 」加问题的前 20 个字；没有问题时只有「旁支」
export function getSideConversationTitle(question: string): string {
  const preview = getSideConversationQuestionPreview(question)
  return preview
    ? i18n.t("workspace.sideConversationTitle", { question: preview })
    : i18n.t("workspace.sideConversation")
}

// 返回旁支标签页显示的行：从包含 boundaryItemID 的那一轮开始，前面继承来的历史不显示；这条 Item 还没出现时没有行
export function getSideConversationRows(
  rows: MessageRow[],
  boundaryItemID: ItemID
): MessageRow[] {
  const start = rows.findIndex((row) =>
    row.kind === "item"
      ? row.view.item.id === boundaryItemID
      : row.items.some((view) => view.item.id === boundaryItemID)
  )
  return start < 0 ? [] : rows.slice(start)
}

// 创建旁支说明的 Item ID：UUID v7，和 serve 创建的 ID 一样按时间排序；页面不一定在安全上下文里，不用 crypto.randomUUID
export function createItemID(): ItemID {
  // 1. 前 48 位是毫秒时间，其余随机，再写入版本和变体。
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  const now = Date.now()
  for (let index = 0; index < 6; index++)
    bytes[index] = Math.floor(now / 2 ** (8 * (5 - index))) % 256
  bytes[6] = (bytes[6] & 0x0f) | 0x70
  bytes[8] = (bytes[8] & 0x3f) | 0x80
  // 2. 按 8-4-4-4-12 写成字符串。
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0"))
  return [
    hex.slice(0, 4).join(""),
    hex.slice(4, 6).join(""),
    hex.slice(6, 8).join(""),
    hex.slice(8, 10).join(""),
    hex.slice(10).join(""),
  ].join("-")
}
