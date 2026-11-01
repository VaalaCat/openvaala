import {
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
} from "react"
import { useTranslation } from "react-i18next"
import { CopyButton } from "@/components/base/copy-button"
import { Button } from "@/components/ui/button"

// 输出外框：工具栏固定在内容上方，正文共用一个滚动区，纵向溢出时可展开高度。
export function OutputBlock({
  copyValue,
  children,
  toolbar,
  maxHeight = 320,
  heightMode = "expandable",
  followOutput = false,
}: {
  copyValue: string
  children: ReactNode
  toolbar?: ReactNode
  maxHeight?: CSSProperties["maxHeight"]
  // host 模式保留宿主限高，不提供解除限高的操作。
  heightMode?: "expandable" | "host"
  followOutput?: boolean
}) {
  const { t } = useTranslation()
  const [isExpanded, setIsExpanded] = useState(false)
  const [hasVerticalOverflow, setHasVerticalOverflow] = useState(false)
  const viewportRef = useRef<HTMLDivElement>(null)
  const contentRef = useRef<HTMLDivElement>(null)
  const measuredScrollSizeRef = useRef<
    | {
        scrollHeight: number
        clientHeight: number
      }
    | undefined
  >(undefined)
  const collapsedHeightRef = useRef(320)

  // 1. 测量正文高度；命令输出只在原先贴底时跟随新内容。
  useLayoutEffect(() => {
    const viewport = viewportRef.current!
    const content = contentRef.current!
    function measure() {
      if (viewport.clientHeight === 0) return
      const measuredScrollSize = measuredScrollSizeRef.current
      const wasAtBottom =
        measuredScrollSize === undefined ||
        measuredScrollSize.scrollHeight -
          measuredScrollSize.clientHeight -
          viewport.scrollTop <=
          2
      if (!isExpanded || heightMode === "host") {
        collapsedHeightRef.current = viewport.clientHeight
        setHasVerticalOverflow(viewport.scrollHeight > viewport.clientHeight)
      } else {
        setHasVerticalOverflow(
          content.offsetHeight > collapsedHeightRef.current
        )
      }
      measuredScrollSizeRef.current = {
        scrollHeight: viewport.scrollHeight,
        clientHeight: viewport.clientHeight,
      }
      if (followOutput && wasAtBottom)
        viewport.scrollTop = viewport.scrollHeight
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(viewport)
    observer.observe(content)
    return () => observer.disconnect()
  }, [isExpanded, heightMode, maxHeight, followOutput])

  // 2. 工具栏与滚动区分别排版，展开不改变正文组件身份。
  return (
    <div
      data-output-block
      className="flex min-h-0 min-w-0 flex-col border bg-background"
    >
      <div className="flex shrink-0 items-center justify-end gap-2 border-b px-2 py-1 text-ui">
        {toolbar && <div className="mr-auto min-w-0">{toolbar}</div>}
        {heightMode === "expandable" && (hasVerticalOverflow || isExpanded) && (
          <Button
            className="text-ui"
            variant="ghost"
            size="sm"
            aria-expanded={isExpanded}
            onClick={() => setIsExpanded(!isExpanded)}
          >
            {isExpanded ? t("outputBlock.collapse") : t("outputBlock.expand")}
          </Button>
        )}
        <CopyButton value={copyValue} />
      </div>
      <div
        ref={viewportRef}
        data-output-scroll
        data-scroll-density="compact"
        className="min-h-0 overflow-auto"
        style={{
          maxHeight:
            isExpanded && heightMode !== "host" ? undefined : maxHeight,
        }}
      >
        <div ref={contentRef} className="w-max min-w-full">
          {children}
        </div>
      </div>
    </div>
  )
}
