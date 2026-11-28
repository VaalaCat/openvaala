import {
  useCallback,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react"
import { animate, motion, useMotionValue } from "motion/react"
import { useTranslation } from "react-i18next"
import { useKeptViewState } from "@/components/base/kept-view-state"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

const richContentSelector =
  'pre, table, img, button, a, details, input, textarea, select, [tabindex]:not([tabindex="-1"])'

// 正文折叠为四行，展开选择随虚拟列表保留。块级正文中的富内容保持完整。
export function CollapsibleMessageBody({
  children,
  viewStateKey,
  as = "p",
  afterBody,
}: {
  children: ReactNode
  viewStateKey: string
  as?: "p" | "div"
  afterBody?: ReactNode
}) {
  const { t } = useTranslation()
  const textID = useId()
  const textRef = useRef<HTMLElement>(null)
  const attachText = useCallback((element: HTMLElement | null) => {
    textRef.current = element
  }, [])
  const [textHeights, setTextHeights] = useState<
    { full: number; preview: number } | "unclipped"
  >()
  const [open, setOpen] = useKeptViewState(viewStateKey, false)
  const previousOpenRef = useRef(open)
  const visibleHeight = useMotionValue<number | string>("auto")
  const isClipped = textHeights !== "unclipped"
  const isLong =
    isClipped && !!textHeights && textHeights.full > textHeights.preview + 1
  const Body = as === "p" ? motion.p : motion.div
  const Text = as === "p" ? "span" : "div"

  // 1. 测量未裁切正文的自然高度；块级正文含富内容时保留完整高度。
  useLayoutEffect(() => {
    const text = textRef.current
    if (!text) return
    const measure = () => {
      if (as === "div" && text.querySelector(richContentSelector)) {
        setTextHeights("unclipped")
        return
      }
      if (text.getClientRects().length === 0) return
      const full = text.scrollHeight
      const preview = 4 * Number.parseFloat(getComputedStyle(text).lineHeight)
      setTextHeights((previous) =>
        previous !== "unclipped" &&
        previous?.full === full &&
        previous.preview === preview
          ? previous
          : { full, preview }
      )
    }
    measure()
    const resizeObserver = new ResizeObserver(measure)
    resizeObserver.observe(text)
    const mutationObserver =
      as === "div" ? new MutationObserver(measure) : undefined
    mutationObserver?.observe(text, { childList: true, subtree: true })
    return () => {
      resizeObserver.disconnect()
      mutationObserver?.disconnect()
    }
  }, [as, children])

  // 2. 只有开合播放高度动画；首次挂载和重新排版直接采用实测高度。
  useLayoutEffect(() => {
    if (!textHeights) return
    if (textHeights === "unclipped") {
      visibleHeight.set("auto")
      return
    }
    const height = open
      ? textHeights.full
      : Math.min(textHeights.full, textHeights.preview)
    const toggled = previousOpenRef.current !== open
    previousOpenRef.current = open
    if (!toggled) {
      visibleHeight.set(height)
      return
    }
    const animation = animate(visibleHeight, height, {
      duration: open ? 0.2 : 0.15,
      ease: [0.22, 1, 0.36, 1],
    })
    return () => animation.stop()
  }, [open, textHeights, visibleHeight])

  // 3. 正文、附件和开合按钮按文档顺序排列。
  return (
    <>
      {children && (
        <Body
          id={textID}
          initial={false}
          style={{
            height: visibleHeight,
            maxHeight: !open && !textHeights ? "4lh" : undefined,
          }}
          animate={{
            "--message-preview-opacity": open ? 1 : 0,
          }}
          transition={{
            duration: open ? 0.2 : 0.15,
            ease: [0.22, 1, 0.36, 1],
          }}
          className={cn(
            isClipped && "overflow-hidden",
            "text-reading wrap-anywhere",
            as === "p" && "whitespace-pre-wrap",
            isLong && "message-preview-fade"
          )}
        >
          <Text ref={attachText} className="block">
            {children}
          </Text>
        </Body>
      )}
      {afterBody}
      {isClipped && (isLong || open) && (
        <Button
          type="button"
          variant="ghost"
          className="h-8 w-fit px-1 text-ui color-muted"
          aria-expanded={open}
          aria-controls={textID}
          onClick={() => setOpen((previous) => !previous)}
        >
          {open
            ? t("common.actions.collapseMessage")
            : t("common.actions.expandMessage")}
        </Button>
      )}
    </>
  )
}
