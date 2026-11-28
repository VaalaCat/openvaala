import type { ComponentProps } from "react"
import { useHref, useLinkClickHandler, type LinkProps } from "react-router"
import { cn } from "@/lib/utils"

// 站内导航使用原生按钮响应键盘操作，并向读屏器保留链接语义。
export function NavigationButton({
  to,
  state,
  className,
  onClick,
  onAuxClick,
  ...props
}: ComponentProps<"button"> & Pick<LinkProps, "to" | "state">) {
  const href = useHref(to)
  const navigate = useLinkClickHandler<HTMLButtonElement>(to, { state })
  return (
    <button
      type="button"
      role="link"
      className={cn("cursor-pointer text-start", className)}
      {...props}
      onClick={(event) => {
        onClick?.(event)
        if (event.defaultPrevented) return
        if (event.button === 0 && (event.ctrlKey || event.metaKey)) {
          window.open(href, "_blank", "noopener")
        } else {
          navigate(event)
        }
      }}
      onAuxClick={(event) => {
        onAuxClick?.(event)
        if (event.button !== 1 || event.defaultPrevented) return
        event.preventDefault()
        window.open(href, "_blank", "noopener")
      }}
    />
  )
}
