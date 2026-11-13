import assert from "node:assert/strict"
import { test } from "node:test"
import {
  MutationObserver,
  QueryClient,
  QueryObserver,
} from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type { ListDirectoriesParams } from "../api/protocol.gen"
import { getDirectorySearch, workspaceDirectoriesOptions } from "./executor"
import { reloadConfigOptions } from "./provider"

for (const { input, directory, keyword } of [
  { input: "", directory: ".", keyword: "" },
  { input: " /work/p ", directory: "/work/", keyword: "p" },
  { input: "/work/pro", directory: "/work/", keyword: "pro" },
  { input: "/work/project/", directory: "/work/project/", keyword: "" },
  { input: "/", directory: "/", keyword: "" },
  { input: "projects/p", directory: "projects/", keyword: "p" },
  { input: "project", directory: ".", keyword: "project" },
  { input: "~/p", directory: "~/", keyword: "p" },
  { input: "~", directory: "~/", keyword: "" },
  { input: "..", directory: "../", keyword: "" },
  { input: ".", directory: "./", keyword: "" },
]) {
  test(`directory search: ${JSON.stringify(input)}`, () => {
    assert.deepEqual(getDirectorySearch(input), { directory, keyword })
  })
}

test("opening candidates fetches once per layer and keeps executor, server and base separate", async () => {
  const requests: ListDirectoriesParams[] = []
  const connection = directoryConnection("server-one", requests)
  const client = new QueryClient()
  const params = {
    executor_id: "local",
    directory: "/work/",
    base_directory: "/base",
  }
  const observer = new QueryObserver(client, {
    ...workspaceDirectoriesOptions(connection, params),
    enabled: false,
  })
  const unsubscribe = observer.subscribe(() => undefined)
  try {
    assert.equal(requests.length, 0)
    observer.setOptions({
      ...workspaceDirectoriesOptions(connection, params),
      enabled: true,
    })
    await observer.refetch()
    assert.equal(requests.length, 1)
    for (const input of ["/work/p", "/work/pro", "/work/project"]) {
      const { directory } = getDirectorySearch(input)
      await client.fetchQuery(
        workspaceDirectoriesOptions(connection, { ...params, directory })
      )
    }
    assert.equal(requests.length, 1)
    for (const { executor_id, directory, base_directory } of [
      { ...params, directory: "/work/project/" },
      { ...params, executor_id: "remote" },
      { ...params, base_directory: "/another-base" },
    ]) {
      await client.fetchQuery(
        workspaceDirectoriesOptions(connection, {
          executor_id,
          directory,
          base_directory,
        })
      )
    }
    await client.fetchQuery(
      workspaceDirectoriesOptions(
        directoryConnection("server-two", requests),
        params
      )
    )
    assert.equal(requests.length, 5)
    const remote = client.getQueryData(
      workspaceDirectoriesOptions(connection, {
        ...params,
        executor_id: "remote",
      }).queryKey
    )
    assert.deepEqual(remote?.directories, ["project", "private"])
  } finally {
    unsubscribe()
    client.clear()
  }
})

for (const message of ["connection refused", "request timed out"]) {
  test(`directory failure is not automatically retried: ${message}`, async () => {
    const requests: ListDirectoriesParams[] = []
    let failing = true
    const connection = directoryConnection("server-one", requests, () => {
      if (failing) throw new Error(message)
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: 3, retryDelay: 0 } },
    })
    const options = workspaceDirectoriesOptions(connection, {
      executor_id: "remote",
      directory: "/work/",
    })
    try {
      await assert.rejects(client.fetchQuery(options), { message })
      assert.equal(requests.length, 1)
      failing = false
      const result = await client.fetchQuery(options)
      assert.equal(requests.length, 2)
      assert.deepEqual(result.directories, ["project", "private"])
    } finally {
      client.clear()
    }
  })
}

test("successful config reload invalidates directory candidates for that server", async () => {
  const requests: ListDirectoriesParams[] = []
  const client = new QueryClient()
  const connection = directoryConnection("server-one", requests)
  const params = { executor_id: "local", directory: "/work/" }
  const options = workspaceDirectoriesOptions(connection, params)
  const other = workspaceDirectoriesOptions(
    directoryConnection("server-two", requests),
    params
  )
  try {
    await client.fetchQuery(options)
    await client.fetchQuery(other)
    const reload = new MutationObserver(client, reloadConfigOptions(connection))
    await reload.mutate()
    assert.equal(client.getQueryState(options.queryKey)?.isInvalidated, true)
    assert.equal(client.getQueryState(other.queryKey)?.isInvalidated, false)
    await client.fetchQuery(options)
    assert.equal(requests.length, 3)
  } finally {
    client.clear()
  }
})

// 记录查询经过的公开 RPC，并返回同层候选。
function directoryConnection(
  serverID: string,
  requests: ListDirectoriesParams[],
  beforeRead?: () => void
): ServerConnection {
  // 1. 目录请求记录参数，配置重载走同一连接。
  return {
    getServerID: () => serverID,
    async call(method: string, params: ListDirectoriesParams) {
      if (method === "config/reload") return { pending_conversation_ids: [] }
      assert.equal(method, "executor/listDirectories")
      requests.push(params)
      beforeRead?.()
      return {
        directory: params.directory,
        directories: ["project", "private"],
      }
    },
  } as unknown as ServerConnection
}
