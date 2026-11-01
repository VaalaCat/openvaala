import { StrictMode } from "react"
import { QueryClientProvider } from "@tanstack/react-query"
import { createRoot } from "react-dom/client"
import { createBrowserRouter, RouterProvider } from "react-router"
import { AppRouter } from "./router"
import { AppToaster } from "./components/base/app-toaster"
import { RouteLoadError } from "./components/route-load-error"
import { ServerConnectionProvider } from "./components/server-connection-provider"
import { initializeAppearance } from "./lib/appearance/preferences"
import { createQueryClient } from "./lib/query/client"
import { initializeI18n } from "./i18n"
import "./index.css"

initializeAppearance()
await initializeI18n()
const queryClient = createQueryClient()
const router = createBrowserRouter(
  [{ path: "*", element: <AppRouter />, errorElement: <RouteLoadError /> }],
  { basename: "/web/v1" }
)
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ServerConnectionProvider>
        <RouterProvider router={router} />
      </ServerConnectionProvider>
    </QueryClientProvider>
    <AppToaster />
  </StrictMode>
)
