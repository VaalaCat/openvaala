import { lazy, Suspense, type ComponentType } from "react"
import { Navigate, Route, Routes } from "react-router"
import { AppLayout } from "@/components/app-layout"

// 按需加载页面模块，取其中名为 name 的导出
function lazyPage<K extends string>(
  load: () => Promise<Record<K, ComponentType>>,
  name: K
) {
  return lazy(() => load().then((module) => ({ default: module[name] })))
}

const IndexPage = lazyPage(() => import("@/app/index"), "IndexPage")
const ConversationPage = lazyPage(
  () => import("@/app/conversation"),
  "ConversationPage"
)
const NewConversationPage = lazyPage(
  () => import("@/app/new-conversation"),
  "NewConversationPage"
)
const ProviderListPage = lazyPage(
  () => import("@/app/settings/providers"),
  "ProviderListPage"
)
const GeneralSettingsPage = lazyPage(
  () => import("@/app/settings/general"),
  "GeneralSettingsPage"
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
const HookSettingsPage = lazyPage(
  () => import("@/app/settings/hooks"),
  "HookSettingsPage"
)
const SkillSettingsPage = lazyPage(
  () => import("@/app/settings/skills"),
  "SkillSettingsPage"
)

export function AppRouter() {
  return (
    // 入口页不在 AppLayout 里，它的模块加载由这一层兜住
    <Suspense fallback={null}>
      <Routes>
        <Route path="/" element={<IndexPage />} />
        <Route path="/s/:serverID" element={<AppLayout />}>
          <Route index element={<NewConversationPage />} />
          <Route path="c/:conversationID" element={<ConversationPage />}>
            <Route index element={null} />
            <Route path="sessions/:sessionID" element={null} />
          </Route>
          <Route path="settings">
            <Route index element={<Navigate to="general" replace />} />
            <Route path="general" element={<GeneralSettingsPage />} />
            <Route path="archived" element={<ArchivedConversationsPage />} />
            <Route path="providers" element={<ProviderListPage />} />
            <Route path="providers/new" element={<ProviderCreatePage />} />
            <Route path="providers/edit" element={<ProviderEditPage />} />
            <Route path="executors" element={<ExecutorSettingsPage />} />
            <Route path="hooks" element={<HookSettingsPage />} />
            <Route path="skills" element={<SkillSettingsPage />} />
          </Route>
        </Route>
        {/* 不认识的地址回到入口页 */}
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Suspense>
  )
}
