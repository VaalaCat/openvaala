import { highlightLines } from "./highlighter-core"
import type { HighlightRequest, HighlightResponse } from "./highlighter-types"

// 高亮线程接收一段文本，加载语言并返回两套主题的逐行颜色。
self.onmessage = async (event: MessageEvent<HighlightRequest>) => {
  // 1. 在高亮线程中计算并交回结果。
  const { id, text, lang } = event.data
  const lines = await highlightLines(text, lang)
  self.postMessage({ id, lines } satisfies HighlightResponse)
}
