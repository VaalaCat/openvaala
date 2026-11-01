import { map } from "nanostores"
import type { Content, ConversationID } from "./api/protocol.gen"

export const newConversationDraftID: ConversationID = ""
export type ConversationDraft = { content: Content[] }
const emptyDraft: ConversationDraft = { content: [] }

// 每个对话未发送的完整内容；切换对话保留，刷新后清空。
export const $drafts = map<Record<string, ConversationDraft>>({})

// 返回服务器和对话共同组成的草稿身份。
export function getDraftKey(
  serverID: string,
  conversationID: ConversationID
): string {
  // 1. 空对话 ID 只对应这一服务器的新对话草稿。
  return JSON.stringify([serverID, conversationID])
}

// 写入对话草稿。
export function setDraft(
  serverID: string,
  conversationID: ConversationID,
  draft: ConversationDraft
): void {
  // 1. 替换这一份草稿。
  $drafts.setKey(getDraftKey(serverID, conversationID), draft)
}

// 读取对话草稿。
export function getDraft(
  serverID: string,
  conversationID: ConversationID
): ConversationDraft {
  // 1. 没有草稿时返回空内容。
  return $drafts.get()[getDraftKey(serverID, conversationID)] ?? emptyDraft
}

// 这段内容是可编辑的普通正文吗？
export function isDraftText(content: Content): boolean {
  // 1. 附带材料不属于普通正文。
  return content.kind === "text" && !content.text?.is_context
}

// 按内容顺序连接普通正文。
export function getDraftText(content: Content[]): string {
  // 1. 仅连接普通文本，不插入分隔符或修改空白。
  return content
    .filter(isDraftText)
    .map((each) => each.text?.text ?? "")
    .join("")
}

// 编辑正文时合并普通文本，保留其他内容的顺序。
export function replaceDraftText(content: Content[], text: string): Content[] {
  // 1. 新正文放在原第一段普通文本处，没有普通文本时放开头。
  const first = content.findIndex(isDraftText)
  const remaining = content.filter((each) => !isDraftText(each))
  if (!text) return remaining
  const position =
    first < 0
      ? 0
      : content.slice(0, first).filter((each) => !isDraftText(each)).length
  return [
    ...remaining.slice(0, position),
    { kind: "text", text: { text } },
    ...remaining.slice(position),
  ]
}

// 发送结束后，移除已发送正文，保留期间追加或重写的文字。
export function removeSentText(draft: string, sentText: string): string {
  // 1. 未改就清空，追加则只保留后面的内容，重写则保留原样。
  if (draft === sentText) return ""
  if (!sentText.trim() || !draft.startsWith(sentText)) return draft
  return draft.slice(sentText.length).trimStart()
}

// 发送成功后移除本次内容，保留新正文、图片和材料。
export function removeSentContent(
  draft: ConversationDraft,
  sent: Content[]
): ConversationDraft {
  // 1. 按原内容引用移除已发图片和材料，保留后来加入的内容。
  const sentOther = sent.filter((each) => !isDraftText(each))
  const content = draft.content.filter((each) => {
    if (isDraftText(each)) return true
    const index = sentOther.indexOf(each)
    if (index < 0) return true
    sentOther.splice(index, 1)
    return false
  })
  // 2. 普通正文沿追加规则清除，仅发生变化时合并文本。
  const text = getDraftText(content)
  const remainingText = removeSentText(text, getDraftText(sent))
  return {
    content:
      text === remainingText
        ? content
        : replaceDraftText(content, remainingText),
  }
}
