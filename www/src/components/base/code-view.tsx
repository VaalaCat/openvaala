import { Fragment, memo, useMemo, type CSSProperties } from "react"
import { OutputBlock } from "@/components/base/output-block"
import { useHighlightedLines, type HighlightedLine } from "@/lib/highlighter"

// 显示一段代码（工具输出、文件内容、补丁）：按 lang 用 shiki 着色，着色好之前和不能着色时按纯文本显示；
// 不折行，横向滚动，右上角复制全文
export function CodeView({
  code,
  lang,
  maxHeight,
  heightMode,
}: {
  code: string
  // shiki 语言名、常见别名或文件扩展名，例如 diff、ts、json
  lang: string
  maxHeight?: CSSProperties["maxHeight"]
  heightMode?: "expandable" | "host"
}) {
  // 结尾的换行不单独显示成空行
  const text = code.endsWith("\n") ? code.slice(0, -1) : code
  const highlighted = useHighlightedLines(text, lang, true)
  const lines = useMemo(() => text.split("\n"), [text])
  return (
    <OutputBlock copyValue={code} maxHeight={maxHeight} heightMode={heightMode}>
      <pre className="w-max min-w-full px-3 py-2 text-code">
        {lines.map((line, index) => (
          <Fragment key={index}>
            {index > 0 && "\n"}
            <CodeLine text={line} tokens={highlighted?.[index]} />
          </Fragment>
        ))}
      </pre>
    </OutputBlock>
  )
}

// 一行原文始终留在同一个节点中，着色完成后更新其中的颜色。
const CodeLine = memo(function CodeLine({
  text,
  tokens,
}: {
  text: string
  tokens: HighlightedLine | undefined
}) {
  // 1. 已着色的行复用 token，新增的行先显示原文。
  return <span>{tokens ? <CodeTokens tokens={tokens} /> : text}</span>
})

// 一行着色后的文字
export const CodeTokens = memo(function CodeTokens({
  tokens,
}: {
  tokens: HighlightedLine
}) {
  return tokens.map((token, index) => (
    <span key={index} className="color-syntax" style={token.style}>
      {token.content}
    </span>
  ))
})
