import { StrictMode } from "react"
import { QueryClientProvider } from "@tanstack/react-query"
import { createRoot } from "react-dom/client"
import { createBrowserRouter, RouterProvider } from "react-router"
import { AppRouter } from "./router"
import { AppToaster } from "./components/base/app-toaster"
import { RouteLoadError } from "./components/route-load-error"
import { initializeAppearance } from "./lib/appearance/preferences"
import { createQueryClient } from "./lib/query/client"
import { notifyPageUpdated } from "./lib/notify"
import { isModuleLoadError } from "./lib/route-error"
import { i18n, initializeI18n } from "./i18n"
import "./index.css"

// index.html 在入口模块下载前就已注册白屏恢复函数。
declare function recoverPageModules(): void

// iOS 在渲染输入控件前设置聚焦缩放上限，其他平台保留默认 viewport。
if (
  /iPad|iPhone|iPod/.test(navigator.userAgent) ||
  (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1)
) {
  const viewport = document.querySelector<HTMLMetaElement>(
    'meta[name="viewport"]'
  )!
  viewport.content += ", maximum-scale=1.0"
}

initializeAppearance()
await initializeI18n()
window.addEventListener("vite:preloadError", (event) => {
  if (!isModuleLoadError(event.payload)) return
  event.preventDefault()
  if (document.querySelector("#root main")) notifyPageUpdated(i18n.t)
  else recoverPageModules()
})
const queryClient = createQueryClient()
const router = createBrowserRouter(
  [{ path: "*", element: <AppRouter />, errorElement: <RouteLoadError /> }],
  { basename: "/web/v1" }
)
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
    <AppToaster />
  </StrictMode>
)
