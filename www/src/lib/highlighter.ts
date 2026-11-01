import { useEffect, useMemo, useRef, useState } from "react"
import {
  shouldHighlight,
  type HighlightedLine,
  type HighlightRequest,
  type HighlightResponse,
} from "./highlighter-types"

export { shouldHighlight } from "./highlighter-types"
export type { HighlightedToken, HighlightedLine } from "./highlighter-types"

// 一个代码视图在同一语言下的着色订阅；卸载或切换语言时释放。
export interface HighlightDocument {
  lang: string
  onHighlighted: ((text: string, lines: HighlightedLine[]) => void) | undefined
}

interface QueuedHighlight extends HighlightRequest {
  document: HighlightDocument
}

// 共用一个高亮线程，串行计算文本，合并每个代码视图尚未开始的请求。
export class HighlightWorkerClient {
  private createWorker: () => Worker
  private worker?: Worker
  private isFailed = false
  private nextRequestID = 0
  private queued: QueuedHighlight[] = []
  private computing?: QueuedHighlight

  constructor(createWorker: () => Worker) {
    this.createWorker = createWorker
  }

  // 更新一个代码视图的待着色文本，已开始的版本仍可返回可用前缀。
  highlightLines(document: HighlightDocument, text: string): void {
    // 1. 同一视图尚未开始的请求只保留最新文本。
    this.queued = this.queued.filter((request) => request.document !== document)
    if (this.isFailed || !document.onHighlighted || !shouldHighlight(text))
      return
    this.queued.push({
      id: ++this.nextRequestID,
      text,
      lang: document.lang,
      document,
    })
    this.sendNextRequest()
  }

  // 视图卸载或切换语言时，释放订阅和等待着色的文本。
  disposeDocument(document: HighlightDocument): void {
    // 1. 已开始的计算结束后不再通知此视图。
    document.onHighlighted = undefined
    this.queued = this.queued.filter((request) => request.document !== document)
  }

  // 创建线程并发送下一段等待着色的文本。
  private sendNextRequest() {
    // 1. 同一时刻只向线程发送一段文本。
    if (this.computing || this.isFailed || this.queued.length === 0) return
    try {
      if (!this.worker) {
        this.worker = this.createWorker()
        this.worker.onmessage = (event: MessageEvent<HighlightResponse>) => {
          const request = this.computing
          if (!request || request.id !== event.data.id) return
          this.computing = undefined
          if (event.data.lines)
            request.document.onHighlighted?.(request.text, event.data.lines)
          this.sendNextRequest()
        }
        this.worker.onerror = () => this.failWorker()
        this.worker.onmessageerror = () => this.failWorker()
      }

      // 2. 每个视图只发送仍需着色的最新文本。
      this.computing = this.queued.shift()!
      const { id, text, lang } = this.computing
      this.worker.postMessage({ id, text, lang } satisfies HighlightRequest)
    } catch {
      this.failWorker()
    }
  }

  // 结束失败线程及其全部请求，后续新增文本继续按纯文本显示。
  private failWorker() {
    // 1. 释放线程、计算中的请求和所有等待请求。
    this.isFailed = true
    this.worker?.terminate()
    this.worker = undefined
    this.computing = undefined
    this.queued = []
  }
}

const highlightWorker = new HighlightWorkerClient(
  () =>
    new Worker(new URL("./highlighter.worker.ts", import.meta.url), {
      type: "module",
    })
)

// 复用内容和颜色都未变的行。
export function reuseHighlightedLines(
  previous: HighlightedLine[] | undefined,
  incoming: HighlightedLine[]
): HighlightedLine[] {
  // 1. 逐行比较文字和颜色，未变的行沿用已有引用。
  return incoming.map((line, lineIndex) => {
    const previousLine = previous?.[lineIndex]
    if (
      previousLine?.length !== line.length ||
      !line.every((token, tokenIndex) => {
        const previousToken = previousLine[tokenIndex]
        if (previousToken.content !== token.content) return false
        const style = token.style ?? {}
        const previousStyle = previousToken.style ?? {}
        const keys = Object.keys(style) as (keyof typeof style)[]
        return (
          keys.length === Object.keys(previousStyle).length &&
          keys.every((key) => style[key] === previousStyle[key])
        )
      })
    )
      return line
    return previousLine
  })
}

// 文本更新后，保留原文未变的行，新行先显示纯文本。
export function retainHighlightedLines(
  text: string,
  highlightedText: string,
  highlightedLines: HighlightedLine[]
): (HighlightedLine | undefined)[] {
  // 1. 按原文比较同一位置的行。
  const previousLines = highlightedText.split("\n")
  return text
    .split("\n")
    .map((line, index) =>
      line === previousLines[index] ? highlightedLines[index] : undefined
    )
}

// 立即显示已到文本，后台着色结束后仅替换变化的行。
export function useHighlightedLines(
  text: string,
  lang: string,
  isEnabled: boolean
): (HighlightedLine | undefined)[] | undefined {
  const [highlighted, setHighlighted] = useState<{
    text: string
    lang: string
    lines: HighlightedLine[]
  }>()

  const documentRef = useRef<HighlightDocument | undefined>(undefined)

  // 1. 同一视图和语言共享订阅，卸载或语言变化时释放。
  useEffect(() => {
    if (!isEnabled) return
    const document: HighlightDocument = {
      lang,
      onHighlighted: (highlightedText, lines) => {
        setHighlighted((previous) => ({
          text: highlightedText,
          lang,
          lines: reuseHighlightedLines(
            previous?.lang === lang ? previous.lines : undefined,
            lines
          ),
        }))
      },
    }
    documentRef.current = document
    return () => {
      highlightWorker.disposeDocument(document)
      documentRef.current = undefined
    }
  }, [lang, isEnabled])

  // 2. 只替换未开始的请求，计算中的版本仍可为未变行着色。
  useEffect(() => {
    if (documentRef.current)
      highlightWorker.highlightLines(documentRef.current, text)
  }, [text, lang, isEnabled])

  // 3. 等待新结果期间，沿用仍对应原文的已有着色。
  return useMemo(() => {
    if (!isEnabled || highlighted?.lang !== lang) return undefined
    if (highlighted.text === text) return highlighted.lines
    return retainHighlightedLines(text, highlighted.text, highlighted.lines)
  }, [text, lang, isEnabled, highlighted])
}
