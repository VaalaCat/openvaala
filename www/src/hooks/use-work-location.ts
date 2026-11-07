import { useState } from "react"
import { useMutation } from "@tanstack/react-query"
import { useServerConnection } from "@/components/server-connection-provider"
import type { ExecutorDetails, ExecutorSettings } from "@/lib/api/protocol.gen"

// WorkLocation 是正在编辑的工作位置：用哪个 Executor、在它的哪个目录；新对话页和对话设置共用
export interface WorkLocation {
  // 用户选过的 Executor；没选时为空
  chosenExecutorID?: string
  // 实际使用的 Executor：选过的，否则是已保存设置的默认，再否则是列表里第一个可用且不是 memory 的
  executorID?: string
  executor?: ExecutorDetails
  // 输入框里的目录文字；没改过时是已保存设置里这个 Executor 的目录
  directory: string
  // 已保存的目录是相对路径补全和校验共用的起点。
  baseDirectory?: string
  // 输入框文字已经是解析好的绝对路径时为这个路径
  resolvedDirectory?: string
  // 最近一次解析失败的错误
  directoryError: unknown
  isResolvingDirectory: boolean
  // 输入框里的目录是用户自己输入的吗？预填值、已保存值和点选「最近用过」填入的值都不算；点选子目录往下走算
  isDirectoryTyped: boolean
  chooseExecutor(executorID: string): void
  setDirectory(directory: string): void
  // 点选一个已经解析好的绝对路径：填入输入框并记为已解析，发送时不再解析
  chooseDirectory(directory: string): void
  // 解析输入框里的目录并换成绝对路径；留空时为空（用 Executor 的默认根目录），失败时抛错
  resolveDirectory(): Promise<string | undefined>
}

// 编辑工作位置：每个 Executor 各自记目录文字，已保存的目录视为已解析；
// 解析用 executor/resolveDirectory，以这个 Executor 已保存的目录为相对路径的起点
export function useWorkLocation(
  executors: ExecutorDetails[],
  saved?: ExecutorSettings
): WorkLocation {
  const connection = useServerConnection()
  const [chosenExecutorID, setChosenExecutorID] = useState<string>()
  // 用户改过的目录文字和解析结果，key 是 Executor ID
  const [editedDirectories, setEditedDirectories] = useState<
    Record<string, string>
  >({})
  const [resolvedDirectories, setResolvedDirectories] = useState<
    Record<string, string>
  >({})
  const [isDirectoryTyped, setIsDirectoryTyped] = useState(false)
  const resolve = useMutation({
    mutationFn: (variables: { executorID: string; directory: string }) =>
      connection.call("executor/resolveDirectory", {
        executor_id: variables.executorID,
        directory: variables.directory,
        base_directory: saved?.workspace_directories?.[variables.executorID],
      }),
  })
  const executorID =
    chosenExecutorID ??
    (saved?.default_executor_id ||
      executors.find(
        (each) => each.kind !== "memory" && isExecutorAvailable(saved, each.id)
      )?.id)
  const executor = executors.find((each) => each.id === executorID)
  const savedDirectory = executorID
    ? saved?.workspace_directories?.[executorID]
    : undefined
  const directory = executorID
    ? (editedDirectories[executorID] ?? savedDirectory ?? "")
    : ""
  const knownDirectory = executorID
    ? (resolvedDirectories[executorID] ?? savedDirectory)
    : undefined
  const resolvedDirectory =
    directory.trim() && knownDirectory === directory.trim()
      ? knownDirectory
      : undefined

  async function resolveDirectory(): Promise<string | undefined> {
    const input = directory.trim()
    if (!input || !executorID) return undefined
    if (resolvedDirectory) return resolvedDirectory
    const result = await resolve.mutateAsync({ executorID, directory: input })
    const resolved = result.workspace_directory
    setEditedDirectories((edited) => ({ ...edited, [executorID]: resolved }))
    setResolvedDirectories((known) => ({ ...known, [executorID]: resolved }))
    return resolved
  }

  return {
    chosenExecutorID,
    executorID,
    executor,
    directory,
    baseDirectory: savedDirectory,
    resolvedDirectory,
    directoryError: resolve.error,
    isResolvingDirectory: resolve.isPending,
    isDirectoryTyped,
    chooseExecutor(next) {
      setChosenExecutorID(next)
      setIsDirectoryTyped(false)
      resolve.reset()
    },
    setDirectory(next) {
      if (!executorID) return
      setEditedDirectories((edited) => ({ ...edited, [executorID]: next }))
      setIsDirectoryTyped(true)
      resolve.reset()
    },
    chooseDirectory(next) {
      if (!executorID) return
      setEditedDirectories((edited) => ({ ...edited, [executorID]: next }))
      setResolvedDirectories((known) => ({ ...known, [executorID]: next }))
      setIsDirectoryTyped(false)
      resolve.reset()
    },
    resolveDirectory,
  }
}

// 这个 Executor 在设置下可用吗？可用列表为空时全部可用
export function isExecutorAvailable(
  settings: ExecutorSettings | undefined,
  executorID: string
): boolean {
  const executorIDs = settings?.executor_ids
  return !executorIDs?.length || executorIDs.includes(executorID)
}
