import { useLayoutEffect } from "react"
import { useLocation, useOutletContext } from "react-router"
import type { PageNavigation } from "@/components/base/page-layout"

// 向 AppLayout 登记页面标题和面包屑父级，顶栏和文档标题跟着显示；PageLayout 和自己排版的整页（对话页）共用。
// onTitleClick 要保持同一个函数，换函数会重新登记
export function usePageNavigation(
  title: string,
  parent?: PageNavigation["parent"],
  parents?: PageNavigation["parents"],
  onTitleClick?: PageNavigation["onTitleClick"]
): void {
  const { key } = useLocation()
  const setNavigation = useOutletContext<
    ((navigation: PageNavigation) => void) | null
  >()
  const parentLabel = parent?.label
  const parentPath = parent?.to
  const parentsKey = JSON.stringify(parents)
  useLayoutEffect(() => {
    setNavigation?.({
      locationKey: key,
      title,
      parents: parentsKey ? JSON.parse(parentsKey) : undefined,
      parent: parentLabel ? { label: parentLabel, to: parentPath } : undefined,
      onTitleClick,
    })
  }, [
    key,
    title,
    parentLabel,
    parentPath,
    parentsKey,
    onTitleClick,
    setNavigation,
  ])
}
