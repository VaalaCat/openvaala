import { motion } from "motion/react"
import { cn } from "@/lib/utils"

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
  // 1. 显示带状态名称的实心点，运行时叠加同色波纹。
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
      {ripple && (
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
