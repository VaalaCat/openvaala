import type { Content } from "./api/protocol.gen"
import type { ServerConnection } from "./api/server-connection"

// 单条消息的附件合计上限：base64 会膨胀约三分之一，10MB 的图片编码后仍在 serve 16MB 的单帧上限以内
export const attachmentBudgetBytes = 10 * 1024 * 1024

// 已有附件加上这次要加的附件，合计是否不超过单条消息的上限？
export function checkAttachmentBudget(
  existingBytes: number[],
  addedBytes: number[]
): { ok: boolean } {
  const total = [...existingBytes, ...addedBytes].reduce(
    (sum, bytes) => sum + bytes,
    0
  )
  return { ok: total <= attachmentBudgetBytes }
}

// 把图片文件读成 data URL，读取失败时拒绝
export function readImageAsDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(reader.result as string)
    reader.onerror = () =>
      reject(reader.error ?? new Error("failed to read " + file.name))
    reader.readAsDataURL(file)
  })
}

// DataURL 中图片的实际字节数。
export function getImageBytes(dataURL: string): number {
  // 1. Base64 按四字符三字节并扣除填充计算。
  const data = dataURL.slice(dataURL.indexOf(",") + 1)
  if (dataURL.slice(0, dataURL.indexOf(",")).endsWith(";base64")) {
    return (
      Math.floor((data.length * 3) / 4) -
      (data.endsWith("==") ? 2 : data.endsWith("=") ? 1 : 0)
    )
  }
  return new TextEncoder().encode(decodeURIComponent(data)).length
}

// 发送时把草稿中的本地文件换成目标对话的文件引用。重试时复用已经成功的上传。
export async function uploadDraftFiles(
  items: Content[],
  conversationID: string,
  upload: (file: File) => Promise<Content>
): Promise<Content[]> {
  const outgoing: Content[] = []
  for (const item of items) {
    const draft = attachmentStatus.get(item)
    if (!draft?.file) {
      outgoing.push(item)
      continue
    }
    if (draft.uploaded?.conversationID !== conversationID) {
      draft.error = undefined
      try {
        const content = await upload(draft.file)
        draft.uploaded = { conversationID, content }
      } catch (error) {
        draft.error = error instanceof Error ? error.message : String(error)
        throw error
      }
    }
    outgoing.push(draft.uploaded.content)
  }
  return outgoing
}

export async function uploadAttachment(
  connection: ServerConnection,
  conversationID: string,
  executorID: string,
  file: File
): Promise<Content> {
  const data = await readImageAsDataURL(file)
  const ref = await connection.call("attachment/upload", {
    conversation_id: conversationID,
    executor_id: executorID,
    name: file.name,
    mime: file.type || "application/octet-stream",
    data: data.slice(data.indexOf(",") + 1),
  })
  return { kind: "file", file: ref }
}

export type DraftAttachment = {
  id: string
  loading?: boolean
  error?: string
  // 新建页保留浏览器 File，发送后再关联这个对话的上传结果。
  file?: File
  uploaded?: { conversationID: string; content: Content }
}
export const attachmentStatus = new WeakMap<Content, DraftAttachment>()
