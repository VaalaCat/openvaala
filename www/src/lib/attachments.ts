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
