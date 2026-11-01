import { queryOptions } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type { ListDirectoriesParams } from "../api/protocol.gen"

// 全部 Executor 和它们的默认根目录
export function executorListOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "executors"],
    queryFn: () => connection.call("executor/list", {}),
  })
}

// 读取一个 Executor 的直接子目录；失败后由用户显式重试。
export function workspaceDirectoriesOptions(
  connection: ServerConnection,
  params: ListDirectoriesParams
) {
  // 1. 按服务、Executor、目录和相对路径起点缓存同层候选。
  return queryOptions({
    queryKey: [
      connection.getServerID(),
      "workspaceDirectories",
      params.executor_id,
      params.directory,
      params.base_directory,
    ],
    queryFn: () => connection.call("executor/listDirectories", params),
    staleTime: 60_000,
    retry: false,
    retryOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
}

// 将输入拆成待查询的目录和在前端筛选的末段名称。
export function getDirectorySearch(input: string): {
  directory: string
  keyword: string
} {
  // 1. 显式的当前位置、上层和主目录按完整目录读取，其余只取最后一个斜线前的路径。
  const path = input.trim()
  if (path === "." || path === ".." || path === "~")
    return { directory: path + "/", keyword: "" }
  const separator = path.lastIndexOf("/")
  return {
    directory: separator < 0 ? "." : path.slice(0, separator + 1),
    keyword: path.slice(separator + 1),
  }
}
