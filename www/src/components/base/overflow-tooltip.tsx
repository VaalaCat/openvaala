import { useLayoutEffect, useState, type ComponentProps } from "react"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

// 观察原元素的宽度和文字，只在单行内容被截断时展示完整文本。
export function OverflowTooltip({
  render = <span />,
  content,
  disabled = false,
  children,
  ...props
}: Omit<ComponentProps<typeof TooltipTrigger>, "content" | "ref"> & {
  content?: string
}) {
  // 1. 跟随文字和尺寸变化更新截断状态。
  const [element, setElement] = useState<HTMLElement | null>(null)
  const [text, setText] = useState("")
  const [isOverflowing, setIsOverflowing] = useState(false)
  useLayoutEffect(() => {
    if (!element) return
    const update = () => {
      const overflowing = element.scrollWidth > element.clientWidth
      setText(content ?? element.textContent?.trim() ?? "")
      setIsOverflowing(overflowing)
    }
    update()
    const sizes = new ResizeObserver(update)
    sizes.observe(element)
    const textChanges = new MutationObserver(update)
    textChanges.observe(element, {
      childList: true,
      characterData: true,
      subtree: true,
    })
    return () => {
      sizes.disconnect()
      textChanges.disconnect()
    }
  }, [element, content])

  // 2. 原元素和触发器始终保持挂载，提示使用统一浮层。
  const enabled = !disabled && isOverflowing && !!text
  return (
    <Tooltip disabled={!enabled}>
      <TooltipTrigger {...props} ref={setElement} render={render}>
        {children}
      </TooltipTrigger>
      <TooltipContent className="text-caption wrap-anywhere">
        {text}
      </TooltipContent>
    </Tooltip>
  )
}
