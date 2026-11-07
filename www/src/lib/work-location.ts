import type { Conversation, ExecutorSettings } from "./api/protocol.gen"

// 默认 Executor 为空的对话使用的 Executor
const fallbackExecutorID = "local"

// 目录下拉里「最近用过」最多列出的目录数
export const recentDirectoryLimit = 5

// NewConversationLocationState 是侧栏「新对话」入口带给新对话页的路由状态
export interface NewConversationLocationState {
  // 正在看的对话的工作位置；没有正在看的对话时为空
  preset?: ExecutorSettings
}

// 对话的工作位置：默认 Executor（为空时是 local）和它在这个对话里的目录；没有保存目录时目录为空
export function getConversationWorkLocation(conversation: Conversation): {
  executorID: string
  directory: string
} {
  // 1. 默认 Executor 为空时按 local 取目录。
  const settings = conversation.executor_settings
  const executorID = settings.default_executor_id || fallbackExecutorID
  return {
    executorID,
    directory: settings.workspace_directories?.[executorID] ?? "",
  }
}

// 新对话照抄这个对话的工作位置：只含默认 Executor 和它的目录，不带可用 Executor 列表和其他 Executor 的目录
export function getWorkLocationSettings(
  conversation: Conversation
): ExecutorSettings {
  // 1. 目录为空时只记默认 Executor。
  const { executorID, directory } = getConversationWorkLocation(conversation)
  return {
    default_executor_id: executorID,
    workspace_directories: directory ? { [executorID]: directory } : undefined,
  }
}

// 这个 Executor 最近用过的目录：默认 Executor 是它的对话的工作目录，最新的在前、去重、最多 limit 个；
// conversations 按更新时间从新到旧排列
export function getRecentDirectories(
  conversations: Conversation[],
  executorID: string,
  limit: number
): string[] {
  // 1. 依次取每个对话的工作位置，只留这个 Executor 的，重复的保留最新的那次。
  const directories = new Set<string>()
  for (const conversation of conversations) {
    const location = getConversationWorkLocation(conversation)
    if (location.executorID === executorID && location.directory)
      directories.add(location.directory)
    if (directories.size === limit) break
  }
  return [...directories]
}
