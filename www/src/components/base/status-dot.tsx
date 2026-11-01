import { motion } from "motion/react"
import { useSyncExternalStore } from "react"
import { cn } from "@/lib/utils"

const reducedMotionQuery = "(prefers-reduced-motion: reduce)"

type StatusDotProps = {
  tone: "destructive" | "warning" | "neutral" | "success" | "info"
  ripple?: boolean
  label: string
  className?: string
}

// 状态点保留静态中心，按调用方选择显示同色波纹。
export function StatusDot({
  tone,
  ripple = false,
  label,
  className,
}: StatusDotProps) {
  // 1. 减少动态效果时只显示带状态名称的实心点。
  const reducedMotion = useSyncExternalStore(
    subscribeReducedMotion,
    () => window.matchMedia(reducedMotionQuery).matches
  )
  return (
    <span
      data-slot="status-dot"
      data-status-tone={tone}
      role="img"
      aria-label={label}
      className={cn(
        "pointer-events-none relative inline-flex size-2 shrink-0 rounded-full bg-current",
        className
      )}
    >
      {ripple && !reducedMotion && (
        <motion.span
          data-slot="status-ripple"
          aria-hidden
          className="absolute inset-0 rounded-full border border-current"
          initial={{ scale: 1, opacity: 0.35 }}
          animate={{ scale: 2.4, opacity: 0 }}
          transition={{ duration: 1.6, repeat: Infinity, ease: "easeOut" }}
        />
      )}
    </span>
  )
}

// 系统减少动画设置变化时更新状态点。
function subscribeReducedMotion(onChange: () => void): () => void {
  // 1. 监听当前媒体查询，组件卸载时取消监听。
  const mediaQuery = window.matchMedia(reducedMotionQuery)
  mediaQuery.addEventListener("change", onChange)
  return () => mediaQuery.removeEventListener("change", onChange)
}
