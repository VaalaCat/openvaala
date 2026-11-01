import { useSyncExternalStore } from "react"

const desktopQuery = "(min-width: 1024px)"

// 视口是否 ≥1024px？
export function useIsDesktop(): boolean {
  // 用 useSyncExternalStore 在首次渲染就读出真实结果，跨过断点时布局不先闪一次。
  return useSyncExternalStore(
    subscribeDesktopQuery,
    () => window.matchMedia(desktopQuery).matches
  )
}

function subscribeDesktopQuery(onChange: () => void): () => void {
  const mql = window.matchMedia(desktopQuery)
  mql.addEventListener("change", onChange)
  return () => mql.removeEventListener("change", onChange)
}
