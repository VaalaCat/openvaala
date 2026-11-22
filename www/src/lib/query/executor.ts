import {
  mutationOptions,
  queryOptions,
  type QueryClient,
} from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type {
  ExecutorConfig,
  ListDirectoriesParams,
  TestSSHExecutorParams,
} from "../api/protocol.gen"

const executorInvalidationSegments = [
  "executors",
  "executorConfigs",
  "workspaceDirectories",
  "skills",
]

function invalidateExecutors(
  queryClient: QueryClient,
  connection: ServerConnection
): Promise<void[]> {
  return Promise.all(
    executorInvalidationSegments.map((segment) =>
      queryClient.invalidateQueries({
        queryKey: [connection.getServerID(), segment],
      })
    )
  )
}

// 全部 Executor 和它们的默认根目录（运行时列表，含内置 Executor）
export function executorListOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "executors"],
    queryFn: () => connection.call("executor/list", {}),
  })
}

// executors/*.yaml 里的原样 Executor 配置，含 SSH 地址和私钥路径
export function executorConfigListOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "executorConfigs"],
    queryFn: () => connection.call("executor/config/list", {}),
  })
}

// serve 本机 ~/.ssh/config 里的非通配符 Host 列表（弱依赖，失败时当空处理）
export function sshConfigHostsOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "sshConfigHosts"],
    queryFn: () => connection.call("executor/sshConfigHosts", {}),
    retry: false,
  })
}

// 原子写入 Executor 配置并热重载
export function setExecutorOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (executor: ExecutorConfig) =>
      connection.call("executor/set", { executor }),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateExecutors(context.client, connection),
  })
}

// 删除 Executor 配置并热重载
export function deleteExecutorOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (executorID: string) =>
      connection.call("executor/delete", { executor_id: executorID }),
    onSuccess: (_result, _variables, _onMutateResult, context) => {
      void invalidateExecutors(context.client, connection)
    },
  })
}

// 测试 SSH 连接与远端工作目录，不写入任何文件
export function testSSHExecutorOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (params: TestSSHExecutorParams) =>
      connection.call("executor/test", params),
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
