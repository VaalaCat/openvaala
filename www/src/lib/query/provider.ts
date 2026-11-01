import { mutationOptions, queryOptions } from "@tanstack/react-query"
import type { QueryClient } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type { ProviderConfig, SetProviderParams } from "../api/protocol.gen"

// 配置里的全部模型服务和默认模型
export function providerListOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [connection.getServerID(), "providers"],
    queryFn: () => connection.call("provider/list", {}),
  })
}

// 改了配置之后让 segments 对应的查询过期，按 [serverID, segment] 前缀失效；返回的 Promise 在正在用的查询重新取回后完成
function invalidateConfiguration(
  queryClient: QueryClient,
  connection: ServerConnection,
  segments: string[]
): Promise<void[]> {
  return Promise.all(
    segments.map((segment) =>
      queryClient.invalidateQueries({
        queryKey: [connection.getServerID(), segment],
      })
    )
  )
}

// 改模型服务、设默认模型之后，模型服务列表和各 Agent 能用的模型都过期
const providerSegments = ["providers", "models"]

// 写入整份 Provider，可同时改默认模型（provider/model）；等列表重新取回才算完成
export function setProviderOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (params: SetProviderParams) =>
      connection.call("provider/set", params),
    // 返回失效的 Promise，编辑页提交新基线时拿到的已是新数据。
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConfiguration(context.client, connection, providerSegments),
  })
}

// 删除 Provider；默认模型属于它时 serve 一起清掉。不等列表重新取回
export function deleteProviderOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (providerID: string) =>
      connection.call("provider/delete", { provider_id: providerID }),
    onSuccess: (_result, _variables, _onMutateResult, context) => {
      // 不等失效完成：编辑页要先跳走，否则会先显示「不存在」。
      void invalidateConfiguration(context.client, connection, providerSegments)
    },
  })
}

// 改默认模型，model 写成 provider/model
export function setDefaultModelOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (model: string) =>
      connection.call("config/setDefaultModel", { model }),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConfiguration(context.client, connection, providerSegments),
  })
}

// 重新读整份配置；除了模型服务，Executor、hook 和 Skill 也可能变了
export function reloadConfigOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: () => connection.call("config/reload", {}),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConfiguration(context.client, connection, [
        ...providerSegments,
        "executors",
        "workspaceDirectories",
        "hooks",
        "skills",
      ]),
  })
}

// 按表单里的 Provider 请求上游 /models，结果不缓存
export function fetchProviderModelsOptions(connection: ServerConnection) {
  return mutationOptions({
    // 参数带 API key，不做成按参数缓存的查询。
    mutationFn: (provider: ProviderConfig) =>
      connection.call("provider/fetchModels", { provider }),
  })
}
