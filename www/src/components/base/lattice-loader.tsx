import type { ComponentProps } from "react"
import { cn } from "@/lib/utils"

// 格点序列与亮度节奏改编自 https://reactbits.dev/c/micro/lattice-loader。
// Copyright (c) 2026 David Haz；MIT + Commons Clause，全文见 www/THIRD_PARTY_NOTICES.md。
const orbitSteps = [0, 1, 2, 7, null, 3, 6, 5, 4]

// 显示随所在位置颜色变化的加载格点，无障碍名称由调用方传入。
export function LatticeLoader({ className, ...props }: ComponentProps<"svg">) {
  // 1. 固定视框内绘制圆点，按 Orbit 顺序交给 CSS 改变亮度。
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width={16}
      height={16}
      viewBox="0 0 22 22"
      fill="currentColor"
      data-slot="lattice-loader"
      role="status"
      className={cn("size-4 shrink-0", className)}
      {...props}
    >
      {orbitSteps.map((step, index) => (
        <circle
          key={index}
          className="lattice-loader__cell"
          cx={3 + (index % 3) * 8}
          cy={3 + Math.floor(index / 3) * 8}
          r={3}
          data-hole={step === null ? "" : undefined}
          style={
            step === null ? undefined : { animationDelay: `${step * 108}ms` }
          }
        />
      ))}
    </svg>
  )
}
