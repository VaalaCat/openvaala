import type { ConversationID, SessionID } from "./api/protocol.gen"

// serve 首页，也是新对话页
export function getServerPath(serverID: string): string {
  return "/s/" + serverID
}

// 对话的主 Session 页
export function getConversationPath(
  serverID: string,
  conversationID: ConversationID
): string {
  return getServerPath(serverID) + "/c/" + conversationID
}

// 对话里一个子 Agent 的 Session 页
export function getSessionPath(
  serverID: string,
  conversationID: ConversationID,
  sessionID: SessionID
): string {
  return (
    getConversationPath(serverID, conversationID) + "/sessions/" + sessionID
  )
}

// SettingsSection 是设置页的一节，对应 /settings/<section>
export type SettingsSection =
  | "general"
  | "appearance"
  | "providers"
  | "executors"
  | "hooks"
  | "skills"
  | "archived"

// 页面跳转只保留侧栏搜索词。
export function getConversationListSearch(search: string): string {
  // 1. 取出侧栏搜索条件。
  const keyword = new URLSearchParams(search).get("q")
  return keyword ? "?" + new URLSearchParams({ q: keyword }).toString() : ""
}

// 设置页的一节
export function getSettingsPath(
  serverID: string,
  section: SettingsSection
): string {
  return getServerPath(serverID) + "/settings/" + section
}

// 新增模型服务页
export function getProviderCreatePath(serverID: string): string {
  return getSettingsPath(serverID, "providers") + "/new"
}

// 编辑模型服务页，Provider id 放在 search param 里
export function getProviderEditPath(
  serverID: string,
  providerID: string
): string {
  // id 可能带 "."，放进路径段时刷新会被 serve 当成静态文件。
  return (
    getSettingsPath(serverID, "providers") +
    "/edit?" +
    new URLSearchParams({ id: providerID }).toString()
  )
}
