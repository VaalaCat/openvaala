import { useId, useLayoutEffect, useState, type ReactElement } from "react"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { useIsMobile } from "@/hooks/use-mobile"

// Trigger 始终保留，错误出现和消失时不会重建正在编辑的输入。
export function FieldTooltip({
  message,
  children,
  severity = "error",
  focusOnly = false,
  menuOpen = false,
  menuElement,
}: {
  message?: string
  children: ReactElement<{
    "aria-describedby"?: string
    "aria-invalid"?: boolean
    "data-slot"?: string
  }>
  severity?: "error" | "notice"
  focusOnly?: boolean
  menuOpen?: boolean
  menuElement?: HTMLDivElement | null
}) {
  const descriptionId = useId()
  const isMobile = useIsMobile()
  const [triggerElement, setTriggerElement] = useState<HTMLElement | null>(null)
  const [tooltipElement, setTooltipElement] = useState<HTMLDivElement | null>(
    null
  )
  const [open, setOpen] = useState(false)
  const [placement, setPlacement] = useState<{
    side: "left" | "right" | "top" | "bottom"
    offset: number
  }>({ side: "left", offset: 4 })
  const visible = !!message && (open || menuOpen)
  useLayoutEffect(() => {
    const anchor = triggerElement
    if (!visible || !anchor || !tooltipElement) return
    const updateSide = () => {
      let rect = anchor.getBoundingClientRect()
      const viewport = window.visualViewport
      const viewportLeft = viewport?.offsetLeft ?? 0
      const viewportRight =
        viewportLeft + (viewport?.width ?? window.innerWidth)
      const width = tooltipElement.offsetWidth + 8
      const viewportTop = viewport?.offsetTop ?? 0
      const viewportBottom =
        viewportTop + (viewport?.height ?? window.innerHeight)
      const menuAbove = menuElement?.dataset.side === "top"
      const verticalRoom = menuAbove
        ? viewportBottom - rect.bottom
        : rect.top - viewportTop
      if (
        menuOpen &&
        (isMobile ||
          (rect.left - viewportLeft < width &&
            viewportRight - rect.right < width)) &&
        verticalRoom < tooltipElement.offsetHeight + 8
      ) {
        anchor.scrollIntoView({ block: "center", inline: "nearest" })
        rect = anchor.getBoundingClientRect()
      }
      const popup = menuOpen ? menuElement : null
      const popupRect = popup?.parentElement?.getBoundingClientRect()
      // 菜单可能比输入本身更宽（例如尾部清除按钮）；错误放到整个菜单外侧。
      const left = Math.min(
        rect.left,
        popupRect ? popupRect.right - (popup?.offsetWidth ?? 0) : rect.left
      )
      const right = Math.max(
        rect.right,
        popupRect ? popupRect.left + (popup?.offsetWidth ?? 0) : rect.right
      )
      let side: "left" | "right" | "top" | "bottom" =
        !isMobile && left - viewportLeft >= width
          ? "left"
          : !isMobile && viewportRight - right >= width
            ? "right"
            : menuElement?.dataset.side === "top"
              ? "bottom"
              : "top"
      let offset =
        side === "left"
          ? rect.left - left + 4
          : side === "right"
            ? right - rect.right + 4
            : 4
      if (popup && popupRect) {
        const requiredHeight = tooltipElement.offsetHeight + 12
        // 无法滚动的短视口里，放到菜单另一端，防止自动翻转后与菜单重叠。
        if (side === "top" && rect.top - viewportTop < requiredHeight) {
          side = "bottom"
          offset =
            Math.max(popupRect.bottom, popupRect.top + popup.offsetHeight) -
            rect.bottom +
            4
        } else if (
          side === "bottom" &&
          viewportBottom - rect.bottom < requiredHeight
        ) {
          side = "top"
          offset =
            rect.top -
            Math.min(popupRect.top, popupRect.bottom - popup.offsetHeight) +
            4
        }
      }
      setPlacement((current) =>
        current.side === side && current.offset === offset
          ? current
          : { side, offset }
      )
    }
    updateSide()
    const placement = new MutationObserver(updateSide)
    if (menuElement)
      placement.observe(menuElement, {
        attributes: true,
        attributeFilter: ["data-side"],
      })
    if (menuElement?.parentElement)
      placement.observe(menuElement.parentElement, {
        attributes: true,
        attributeFilter: ["style"],
      })
    const sizes = new ResizeObserver(updateSide)
    sizes.observe(anchor)
    sizes.observe(tooltipElement)
    if (menuElement) sizes.observe(menuElement)
    window.addEventListener("scroll", updateSide, true)
    window.visualViewport?.addEventListener("resize", updateSide)
    return () => {
      placement.disconnect()
      sizes.disconnect()
      window.removeEventListener("scroll", updateSide, true)
      window.visualViewport?.removeEventListener("resize", updateSide)
    }
  }, [
    visible,
    isMobile,
    triggerElement,
    menuOpen,
    menuElement,
    tooltipElement,
    message,
  ])
  const invalid = !!message && severity === "error"
  return (
    <Tooltip
      disabled={!message}
      open={visible}
      onOpenChange={(open, details) => {
        if (open && focusOnly && details.reason === "trigger-hover") {
          details.cancel()
          return
        }
        setOpen(open)
      }}
    >
      <TooltipTrigger
        ref={setTriggerElement}
        render={children}
        data-slot={children.props["data-slot"] ?? "tooltip-trigger"}
        delay={600}
        closeOnClick={false}
        aria-invalid={invalid || children.props["aria-invalid"] || undefined}
        aria-describedby={
          [children.props["aria-describedby"], message && descriptionId]
            .filter(Boolean)
            .join(" ") || undefined
        }
        data-field-error={invalid || undefined}
      />
      <span id={descriptionId} className="sr-only">
        {message}
      </span>
      <TooltipContent
        ref={setTooltipElement}
        side={placement.side}
        sideOffset={placement.offset}
        className="text-caption data-closed:hidden"
      >
        {message}
      </TooltipContent>
    </Tooltip>
  )
}
