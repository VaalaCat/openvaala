import type { ComponentProps } from "react"
import { TabsList } from "@/components/ui/tabs"

// 详情页签栏：横向溢出时滚动。只设 overflow-x-auto 会让纵向也变成 auto，
// 所以显式关闭纵向滚动；4px 底部留白容纳页签的指示线。
// 获得焦点的页签只露出一部分时，浏览器不会替它滚动，这里滚进可见范围。
export function ScrollableTabsList(props: ComponentProps<typeof TabsList>) {
  return (
    <div
      className="min-w-0 overflow-x-auto overflow-y-hidden pb-1"
      onFocus={(event) =>
        event.target.scrollIntoView({ block: "nearest", inline: "nearest" })
      }
    >
      <TabsList {...props} />
    </div>
  )
}
