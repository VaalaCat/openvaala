import { createContext, useContext, useState, type ReactNode } from "react"
import { createPortal } from "react-dom"
import { usePageNavigation } from "@/hooks/use-page-navigation"

export type PageNavigation = {
  locationKey: string
  title: string
  parent?: { label: string; to?: string }
  parents?: { label: string; to?: string }[]
  // 点顶栏标题时调用，例如对话页改名；有它时标题是按钮，没有时标题只是文字
  onTitleClick?: () => void
}

// PageActions 放进去的位置：PageLayout 的页头；自己排版的整页（对话页）没有页头，用 AppLayout 顶栏右侧
// eslint-disable-next-line react-refresh/only-export-components
export const PageActionsTargetContext = createContext<HTMLDivElement | null>(
  null
)

// 编辑区保留草稿和提交逻辑，只把操作按钮放到共用页头。
export function PageActions({ children }: { children: ReactNode }) {
  const target = useContext(PageActionsTargetContext)
  return target ? createPortal(children, target) : null
}

// 带页头的页面：标题和面包屑父级登记到 AppLayout 顶栏，页面里的 h1 只给读屏；
// 页头一行左边是 toolbar（例如页签），右边是 PageActions 和 actions，没有控件时不占位
export function PageLayout({
  title,
  parent,
  parents,
  actions,
  toolbar,
  children,
}: {
  title: string
  parent?: PageNavigation["parent"]
  parents?: PageNavigation["parents"]
  actions?: ReactNode
  toolbar?: ReactNode
  children: ReactNode
}) {
  const [actionsTarget, setActionsTarget] = useState<HTMLDivElement | null>(
    null
  )
  usePageNavigation(title, parent, parents)
  return (
    <PageActionsTargetContext.Provider value={actionsTarget}>
      <section className="flex min-w-0 flex-col gap-4 p-4">
        <h1 className="sr-only">{title}</h1>
        <header className="flex min-w-0 flex-wrap items-start gap-2 not-has-[a,button]:hidden">
          {toolbar && <div className="min-w-0">{toolbar}</div>}
          <div className="ml-auto flex max-w-full min-w-0 flex-wrap items-center gap-2">
            <div ref={setActionsTarget} className="contents" />
            {actions}
          </div>
        </header>
        <div className="flex min-w-0 flex-col gap-4">{children}</div>
      </section>
    </PageActionsTargetContext.Provider>
  )
}
