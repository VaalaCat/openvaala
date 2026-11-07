import type { ReactNode } from "react"
import "./tab-bar.css"

// 面板的标签栏：36px 高的连续面板外观，左边是标签（放一个 TabsList，横向放不下时滚动），右端是操作按钮。
// 样式和工作区各区域的标签栏共用 tab-bar.css
export function TabBar({
  children,
  actions,
}: {
  children: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="tab-bar">
      <div
        className="tab-bar-strip"
        // 获得焦点的标签只露出一部分时，浏览器不会替它滚动，这里滚进可见范围
        onFocus={(event) =>
          event.target.scrollIntoView({ block: "nearest", inline: "nearest" })
        }
      >
        {children}
      </div>
      {actions && <div className="tab-bar-actions">{actions}</div>}
    </div>
  )
}
