import type { CSSProperties } from "react"

// 一段着色文字，颜色变量同时保存浅色和深色主题。
export interface HighlightedToken {
  content: string
  style?: CSSProperties
}

// 按顺序拼起来就是一行原文。
export type HighlightedLine = HighlightedToken[]

// 主线程在上一段计算结束后向高亮线程发送的文本。
export interface HighlightRequest {
  id: number
  text: string
  lang: string
}

// 高亮线程计算结束时返回；不能着色时 lines 为空。
export interface HighlightResponse {
  id: number
  lines: HighlightedLine[] | undefined
}

// 文本是否小到可以着色？
export function shouldHighlight(text: string): boolean {
  // 1. 超过 200 KiB 时显示纯文本。
  return text.length <= 200 * 1024
}
