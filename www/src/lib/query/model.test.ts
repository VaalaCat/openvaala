import assert from "node:assert/strict"
import { test } from "node:test"
import { QueryClient } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import { modelListOptions } from "./model"

test("native model settings exclude every external agent group", async () => {
  const agents = [
    { name: "default", type: "native" },
    { name: "codex", type: "acp" },
    { name: "another-driver", type: "another-protocol" },
  ]
  const models = ["provider", "codex", "another-driver"].map((provider_id) => ({
    ref: { provider_id, model_id: "small" },
  }))
  const connection = {
    getServerID: () => "test",
    call: async (method: string) =>
      method === "model/list" ? { models } : { agents },
  } as unknown as ServerConnection
  const client = new QueryClient()
  const native = await client.fetchQuery(modelListOptions(connection, ""))
  assert.deepEqual(native.models, [models[0]])
  const scoped = await client.fetchQuery(modelListOptions(connection, "default"))
  assert.deepEqual(scoped.models, models)
})
