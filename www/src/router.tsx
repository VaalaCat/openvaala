import { lazy, Suspense, type ComponentType } from "react"
import { Navigate, Outlet, Route, Routes } from "react-router"
import { ServerConnectionProvider } from "@/components/server-connection-provider"
import { AppLayout } from "@/components/app-layout"

// 按需加载页面模块，取其中名为 name 的导出
function lazyPage<K extends string>(
  load: () => Promise<Record<K, ComponentType> | undefined>,
  name: K
) {
  // Vite 的加载错误已被接手时返回 undefined，保留 Suspense 和外层页面等候刷新。
  return lazy(async () => {
    const module = await load()
    if (!module) return new Promise<never>(() => {})
    return { default: module[name] }
  })
}

const IndexPage = lazyPage(() => import("@/app/index"), "IndexPage")
const NewConversationPage = lazyPage(
  () => import("@/app/new-conversation"),
  "NewConversationPage"
)
const ContactsPage = lazyPage(() => import("@/app/contacts"), "ContactsPage")
const WorkspacePage = lazyPage(
  () => import("@/components/workspace/workspace-page"),
  "WorkspacePage"
)
const ProviderListPage = lazyPage(
  () => import("@/app/settings/providers"),
  "ProviderListPage"
)
const AgentListPage = lazyPage(
  () => import("@/app/settings/agents"),
  "AgentListPage"
)
const AgentCreatePage = lazyPage(
  () => import("@/app/settings/agents"),
  "AgentCreatePage"
)
const AgentEditPage = lazyPage(
  () => import("@/app/settings/agents"),
  "AgentEditPage"
)
const GeneralSettingsPage = lazyPage(
  () => import("@/app/settings/general"),
  "GeneralSettingsPage"
)
const AppearanceSettingsPage = lazyPage(
  () => import("@/app/settings/appearance"),
  "AppearanceSettingsPage"
)
const ArchivedConversationsPage = lazyPage(
  () => import("@/app/settings/archived"),
  "ArchivedConversationsPage"
)
const ProviderCreatePage = lazyPage(
  () => import("@/app/settings/providers"),
  "ProviderCreatePage"
)
const ProviderEditPage = lazyPage(
  () => import("@/app/settings/providers"),
  "ProviderEditPage"
)
const ExecutorSettingsPage = lazyPage(
  () => import("@/app/settings/executors"),
  "ExecutorSettingsPage"
)
const ExecutorCreatePage = lazyPage(
  () => import("@/app/settings/executors"),
  "ExecutorCreatePage"
)
const ExecutorEditPage = lazyPage(
  () => import("@/app/settings/executors"),
  "ExecutorEditPage"
)
const HookSettingsPage = lazyPage(
  () => import("@/app/settings/hooks"),
  "HookSettingsPage"
)
const SkillSettingsPage = lazyPage(
  () => import("@/app/settings/skills"),
  "SkillSettingsPage"
)

const WorldAdminPage = lazyPage(
  () => import("@/app/world-admin"),
  "WorldAdminPage"
)
const WorldAdminSettingsPage = lazyPage(
  () => import("@/app/settings/world-admin"),
  "WorldAdminSettingsPage"
)

function RuntimeRoutes() {
  return (
    <ServerConnectionProvider>
      <Outlet />
    </ServerConnectionProvider>
  )
}

export function AppRouter() {
  return (
    // 入口页不在 AppLayout 里，它的模块加载由这一层兜住
    <Suspense fallback={null}>
      <Routes>
        <Route path="/world/admin" element={<WorldAdminPage />} />
        <Route element={<RuntimeRoutes />}>
          <Route path="/" element={<IndexPage />} />
          <Route path="/s/:serverID" element={<AppLayout />}>
            <Route index element={<NewConversationPage />} />
            <Route path="contacts" element={<ContactsPage />} />
            <Route path="c/:conversationID" element={<WorkspacePage />}>
              <Route index element={null} />
              <Route path="sessions/:sessionID" element={null} />
            </Route>
            <Route path="settings">
              <Route index element={<Navigate to="general" replace />} />
              <Route path="general" element={<GeneralSettingsPage />} />
              <Route path="appearance" element={<AppearanceSettingsPage />} />
              <Route path="archived" element={<ArchivedConversationsPage />} />
              <Route path="providers" element={<ProviderListPage />} />
              <Route path="providers/new" element={<ProviderCreatePage />} />
              <Route path="providers/edit" element={<ProviderEditPage />} />
              <Route path="agents" element={<AgentListPage />} />
              <Route path="agents/new" element={<AgentCreatePage />} />
              <Route path="agents/edit" element={<AgentEditPage />} />
              <Route path="executors" element={<ExecutorSettingsPage />} />
              <Route path="executors/new" element={<ExecutorCreatePage />} />
              <Route path="executors/edit" element={<ExecutorEditPage />} />
              <Route path="hooks" element={<HookSettingsPage />} />
              <Route path="skills" element={<SkillSettingsPage />} />
              <Route path="world-admin" element={<WorldAdminSettingsPage />} />
            </Route>
          </Route>
        </Route>
        {/* 不认识的地址回到入口页 */}
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Suspense>
  )
}
