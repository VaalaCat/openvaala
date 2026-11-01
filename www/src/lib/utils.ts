import { createCn } from "cn/config"
import type { ModelRef } from "./api/protocol.gen"

// 排版和链接角色不属于文字颜色，合并时必须保留组件的主题对比色。
export const cn = createCn({
  extend: {
    classGroups: {
      "font-size": [
        {
          text: [
            "title",
            "section",
            "body",
            "ui",
            "table",
            "caption",
            "token",
            "code",
          ],
        },
      ],
      "text-color": [
        {
          color: [
            "default",
            "muted",
            "subtle",
            "primary",
            "destructive",
            "success",
            "warning",
            "rpc-sent",
            "rpc-received",
            "syntax",
          ],
        },
      ],
      "project-link": ["text-link", "text-help"],
    },
  },
})

// 点击一个指向对象 URL 的下载链接，1 秒后释放 URL
export function downloadBlob(blob: Blob, filename: string) {
  // 1. 用临时链接触发下载。
  const url = URL.createObjectURL(blob)
  const link = document.createElement("a")
  link.href = url
  link.download = filename
  document.body.append(link)
  link.click()
  link.remove()
  // 2. 立即释放时部分浏览器还没开始下载，1 秒后再释放。
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// 按语言缓存的字节、KB、MB 格式；Intl 格式对象创建开销大，长列表里不每次新建
const byteFormatsByLanguage = new Map<string, Intl.NumberFormat[]>()

// 字节数：不到 1KB 用字节，再往上用 KB、MB，保留一位小数
export function formatByteSize(bytes: number, language: string): string {
  let formats = byteFormatsByLanguage.get(language)
  if (!formats) {
    formats = ["byte", "kilobyte", "megabyte"].map(
      (unit) =>
        new Intl.NumberFormat(language, {
          style: "unit",
          unit,
          unitDisplay: "short",
          maximumFractionDigits: 1,
        })
    )
    byteFormatsByLanguage.set(language, formats)
  }
  const [byte, kilobyte, megabyte] = formats
  if (bytes < 1024) return byte.format(bytes)
  if (bytes < 1024 * 1024) return kilobyte.format(bytes / 1024)
  return megabyte.format(bytes / 1024 / 1024)
}

// 模型引用写成「provider/model」
export function formatModelRef(ref: ModelRef): string {
  return ref.provider_id + "/" + ref.model_id
}
