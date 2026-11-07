import { memo, useCallback, useMemo, useState, type CSSProperties } from "react"
import { ChevronRightIcon } from "lucide-react"
import { FoldVertical, UnfoldVertical } from "lucide"
import { MorphIcon } from "morphicons/react"
import { useTranslation } from "react-i18next"
import { CodeTokens } from "@/components/base/code-view"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { CopyButton } from "@/components/base/copy-button"
import { OutputBlock } from "@/components/base/output-block"
import { useHighlightedLines, type HighlightedLine } from "@/lib/highlighter"
import {
  formatJsonLines,
  getJsonCopyText,
  getJsonPathKey,
  getVisibleJsonRows,
  type JsonFoldMode,
  type JsonLine,
} from "@/lib/json-lines"
import { cn } from "@/lib/utils"

// 一次最多显示的行数，超出时底部给「显示全部」
const maxInitialRows = 5000

// 按行显示 JSON：行首箭头折叠对象和数组，值是 JSON 的字符串可原地展开，长字符串截断后可展开；
// 着色交给 shiki，生成中（isStreaming）按纯文本显示；不折行，横向滚动，右上角复制整个值
export function JsonView({
  value,
  defaultExpandDepth = 2,
  isStreaming = false,
  maxHeight,
  heightMode,
}: {
  value: unknown
  // 层级小于它的对象和数组默认展开
  defaultExpandDepth?: number
  isStreaming?: boolean
  maxHeight?: CSSProperties["maxHeight"]
  heightMode?: "expandable" | "host"
}) {
  const { t } = useTranslation()
  const [expandedNestedPaths, setExpandedNestedPaths] = useState(
    () => new Set<string>()
  )
  const [expandedStringPaths, setExpandedStringPaths] = useState(
    () => new Set<string>()
  )
  // 用户点过折叠箭头的节点，折叠状态与默认相反
  const [toggledFoldPaths, setToggledFoldPaths] = useState(
    () => new Set<string>()
  )
  const [isShowingAllRows, setIsShowingAllRows] = useState(false)
  const [foldMode, setFoldMode] = useState<JsonFoldMode>("default")
  const lines = useMemo(
    () => formatJsonLines(value, { expandedNestedPaths, expandedStringPaths }),
    [value, expandedNestedPaths, expandedStringPaths]
  )
  const text = useMemo(() => lines.map((line) => line.text).join("\n"), [lines])
  const copyValue = useMemo(() => getJsonCopyText(value), [value])
  const highlighted = useHighlightedLines(text, "json", !isStreaming)

  const rows = useMemo(
    () =>
      getVisibleJsonRows(lines, defaultExpandDepth, toggledFoldPaths, foldMode),
    [lines, defaultExpandDepth, toggledFoldPaths, foldMode]
  )
  const visibleRows = isShowingAllRows ? rows : rows.slice(0, maxInitialRows)
  const isFullyExpanded =
    visibleRows.length === rows.length && rows.every((row) => !row.isFolded)
  const foldLabel = isFullyExpanded
    ? t("jsonView.collapseAll")
    : t("jsonView.expandAll")

  const handleToggleFold = useCallback((line: JsonLine) => {
    setToggledFoldPaths((paths) => toggleKey(paths, getJsonPathKey(line.path)))
  }, [])
  const handleToggleNested = useCallback((line: JsonLine) => {
    setExpandedNestedPaths((paths) =>
      toggleKey(paths, getJsonPathKey(line.path))
    )
  }, [])
  const handleToggleString = useCallback((line: JsonLine) => {
    setExpandedStringPaths((paths) =>
      toggleKey(paths, getJsonPathKey(line.path))
    )
  }, [])

  return (
    <OutputBlock
      copyValue={copyValue}
      maxHeight={maxHeight}
      heightMode={heightMode}
      toolbar={
        lines.some((line) => line.foldEnd !== undefined) && (
          <Tooltip>
            <TooltipTrigger
              delay={600}
              closeOnClick
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label={foldLabel}
                  onClick={() => {
                    setFoldMode(isFullyExpanded ? "collapsed" : "expanded")
                    setToggledFoldPaths(new Set())
                    setIsShowingAllRows(!isFullyExpanded)
                  }}
                />
              }
            >
              <MorphIcon
                icon={isFullyExpanded ? FoldVertical : UnfoldVertical}
                size={16}
                aria-hidden
              />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {foldLabel}
            </TooltipContent>
          </Tooltip>
        )
      }
    >
      <div className="w-max min-w-full code-padding text-code-reading">
        {visibleRows.map(({ lineIndex, isFolded }) => {
          const line = lines[lineIndex]
          const pathKey = getJsonPathKey(line.path)
          const closingIndex = isFolded ? line.foldEnd : undefined
          return (
            <JsonRowView
              key={`${pathKey}:${line.isClosing ? "close" : "node"}`}
              line={line}
              tokens={highlighted?.[lineIndex]}
              closingLine={
                closingIndex === undefined ? undefined : lines[closingIndex]
              }
              closingTokens={
                closingIndex === undefined
                  ? undefined
                  : highlighted?.[closingIndex]
              }
              isNestedExpanded={expandedNestedPaths.has(pathKey)}
              isStringExpanded={expandedStringPaths.has(pathKey)}
              onToggleFold={handleToggleFold}
              onToggleNested={handleToggleNested}
              onToggleString={handleToggleString}
            />
          )
        })}
      </div>
      {visibleRows.length < rows.length && (
        <div className="sticky left-0 w-fit px-5 pb-2">
          <Button
            variant="outline"
            className="text-ui"
            onClick={() => setIsShowingAllRows(true)}
          >
            {t("jsonView.showAllRows", { total: rows.length })}
          </Button>
        </div>
      )}
    </OutputBlock>
  )
}

// 一行：行首折叠箭头；折叠时接着显示 …、收尾符号和子项数；文字后是按 JSON 展开、展开长字符串；
// 行末固定一列放复制节点，悬停或键盘聚焦时显示，触屏上一直显示
const JsonRowView = memo(function JsonRowView({
  line,
  tokens,
  closingLine,
  closingTokens,
  isNestedExpanded,
  isStringExpanded,
  onToggleFold,
  onToggleNested,
  onToggleString,
}: {
  line: JsonLine
  tokens: HighlightedLine | undefined
  // 折叠时这个节点的收尾行
  closingLine: JsonLine | undefined
  closingTokens: HighlightedLine | undefined
  isNestedExpanded: boolean
  isStringExpanded: boolean
  onToggleFold(line: JsonLine): void
  onToggleNested(line: JsonLine): void
  onToggleString(line: JsonLine): void
}) {
  const { t } = useTranslation()
  const isFolded = closingLine !== undefined
  const canToggleString =
    line.isTruncated || (isStringExpanded && typeof line.value === "string")
  return (
    <div
      data-json-path={getJsonPathKey(line.path)}
      role={line.foldEnd !== undefined ? "button" : undefined}
      tabIndex={line.foldEnd !== undefined ? 0 : undefined}
      aria-expanded={line.foldEnd !== undefined ? !isFolded : undefined}
      aria-label={
        line.foldEnd !== undefined
          ? isFolded
            ? t("jsonView.unfold")
            : t("jsonView.fold")
          : undefined
      }
      className="json-row group/row flex items-center outline-none hover:bg-muted focus-visible:bg-muted"
      onClick={(event) => {
        if (
          line.foldEnd === undefined ||
          (event.target as Element).closest("button")
        )
          return
        if (!window.getSelection()?.isCollapsed) return
        onToggleFold(line)
      }}
      onKeyDown={(event) => {
        if (event.target !== event.currentTarget || line.foldEnd === undefined)
          return
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault()
          onToggleFold(line)
        }
      }}
    >
      <span className="json-row-control sticky left-0 z-10 flex shrink-0 items-center justify-center bg-background group-hover/row:bg-muted">
        {line.foldEnd !== undefined && (
          <ChevronRightIcon
            className={cn("size-3 color-muted", !isFolded && "rotate-90")}
          />
        )}
      </span>
      <span className="whitespace-pre">
        {tokens ? <CodeTokens tokens={tokens} /> : line.text}
        {closingLine && (
          <>
            <span className="mx-1 color-muted">…</span>
            {closingTokens ? (
              <CodeTokens tokens={trimIndent(closingTokens)} />
            ) : (
              closingLine.text.trimStart()
            )}
            <span className="ml-2 text-caption color-muted">
              {t("jsonView.childCount", { count: line.childCount })}
            </span>
          </>
        )}
      </span>
      <span className="ml-2 flex items-center gap-1 opacity-0 group-focus-within/row:opacity-100 group-hover/row:opacity-100 pointer-coarse:opacity-100">
        {line.isNestedJSON && (
          <Button
            variant="outline"
            size="xs"
            className="h-4 px-1 text-caption font-normal"
            onClick={(event) => {
              event.stopPropagation()
              onToggleNested(line)
            }}
          >
            {isNestedExpanded
              ? t("jsonView.collapseNested")
              : t("jsonView.expandNested")}
          </Button>
        )}
        {canToggleString && (
          <Button
            variant="outline"
            size="xs"
            className="h-4 px-1 text-caption font-normal"
            onClick={(event) => {
              event.stopPropagation()
              onToggleString(line)
            }}
          >
            {isStringExpanded
              ? t("jsonView.collapseString")
              : t("jsonView.expandString")}
          </Button>
        )}
      </span>
      <div className="ml-auto flex shrink-0 pr-1 pl-2 opacity-0 group-focus-within/row:opacity-100 group-hover/row:opacity-100 has-[[data-copy-result=failed]]:opacity-100 pointer-coarse:opacity-100">
        {!line.isClosing && line.path.length > 0 && (
          <CopyButton
            value={getJsonCopyText(line.value)}
            label={t("jsonView.copyNode")}
            size="icon-xs"
            className="json-row-control color-muted"
          />
        )}
      </div>
    </div>
  )
})

// 去掉一行开头的缩进，用于折叠时接在开头行后面的收尾符号
function trimIndent(tokens: HighlightedLine): HighlightedLine {
  let isIndent = true
  return tokens.flatMap((token) => {
    if (!isIndent) return [token]
    const content = token.content.trimStart()
    if (!content) return []
    isIndent = false
    return [{ ...token, content }]
  })
}

function toggleKey(keys: Set<string>, key: string): Set<string> {
  const next = new Set(keys)
  if (!next.delete(key)) next.add(key)
  return next
}
