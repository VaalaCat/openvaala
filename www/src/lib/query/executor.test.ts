import assert from "node:assert/strict"
import { test } from "node:test"
import { QueryClient } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import {
  deleteExecutorOptions,
  executorConfigListOptions,
  setExecutorOptions,
  sshConfigHostsOptions,
  testSSHExecutorOptions,
} from "./executor"

function createStubConnection() {
  const calls: { method: string; params: unknown }[] = []
  const connection = {
    getServerID: () => "srv-1",
    call: async (method: string, params: unknown) => {
      calls.push({ method, params })
      if (method === "executor/config/list") return { executors: [] }
      if (method === "executor/sshConfigHosts") return { hosts: [] }
      if (method === "executor/test")
        return { resolved_directory: "/home/coder/workspace" }
      return { pending_conversation_ids: [] }
    },
  } as unknown as ServerConnection
  return { connection, calls }
}

test("executor query and mutation options issue expected RPCs and invalidate caches", async () => {
  const { connection, calls } = createStubConnection()
  const queryClient = new QueryClient()

  const listOpt = executorConfigListOptions(connection)
  assert.deepEqual(listOpt.queryKey, ["srv-1", "executorConfigs"])
  await listOpt.queryFn!({} as never)

  const sshOpt = sshConfigHostsOptions(connection)
  assert.deepEqual(sshOpt.queryKey, ["srv-1", "sshConfigHosts"])
  await sshOpt.queryFn!({} as never)

  const invalidated: unknown[][] = []
  queryClient.invalidateQueries = (async (filters: { queryKey?: unknown[] }) => {
    if (filters?.queryKey) invalidated.push(filters.queryKey)
  }) as typeof queryClient.invalidateQueries

  const setOpt = setExecutorOptions(connection)
  const executor = {
    id: "dev-box",
    kind: "ssh" as const,
    description: "Dev",
    workspace_directory: "/home/coder",
    ssh: { address: "coder@10.0.0.5" },
  }
  await setOpt.mutationFn!(executor, {} as never)
  await setOpt.onSuccess!(
    { pending_conversation_ids: [] },
    executor,
    undefined,
    { client: queryClient } as never
  )
  assert.deepEqual(invalidated, [
    ["srv-1", "executors"],
    ["srv-1", "executorConfigs"],
    ["srv-1", "workspaceDirectories"],
    ["srv-1", "skills"],
  ])

  const testOpt = testSSHExecutorOptions(connection)
  const testResult = await testOpt.mutationFn!(
    {
      ssh: { address: "coder@10.0.0.5" },
      workspace_directory: "/home/coder",
    },
    {} as never
  )
  assert.equal(testResult.resolved_directory, "/home/coder/workspace")

  invalidated.length = 0
  const delOpt = deleteExecutorOptions(connection)
  await delOpt.mutationFn!("dev-box", {} as never)
  await delOpt.onSuccess!(
    { pending_conversation_ids: [] },
    "dev-box",
    undefined,
    { client: queryClient } as never
  )
  assert.equal(invalidated.length, 4)

  assert.deepEqual(
    calls.map((c) => c.method),
    [
      "executor/config/list",
      "executor/sshConfigHosts",
      "executor/set",
      "executor/test",
      "executor/delete",
    ]
  )
})
