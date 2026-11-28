import type { ReactNode } from "react"
import { NavigationButton } from "@/components/base/navigation-button"
import { cn } from "@/lib/utils"

// 带外框、行间有分隔线的列表，子元素是 RowListItem 或自己排版的 <li>
export function RowList({ children }: { children: ReactNode }) {
  return <ul className="flex min-w-0 flex-col divide-y border">{children}</ul>
}

const rowClassName = "flex min-w-0 flex-col gap-1 p-2 text-table"

// RowList 的一行：几行文字上下排列；传 to 时整行是进入详情的链接
export function RowListItem({
  to,
  children,
}: {
  to?: string
  children: ReactNode
}) {
  if (!to) return <li className={rowClassName}>{children}</li>
  return (
    <li>
      <NavigationButton
        to={to}
        className={cn(
          rowClassName,
          "w-full outline-none hover:bg-muted focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
        )}
      >
        {children}
      </NavigationButton>
    </li>
  )
}
