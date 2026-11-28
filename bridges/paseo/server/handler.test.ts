import assert from "node:assert/strict"
import test from "node:test"
import type { PaseoAgent, PaseoAgentRunResult } from "@getpaseo/client"
import type { AgentToolContext } from "@getpaseo/plugin/server"
import { worldCallbacks, worldTools, type WorldAgents } from "./handler.js"
import { settingsDefinition } from "./settings.js"

const settings = settingsDefinition.schema.parse({
  turnTimeoutMs: 3210,
  targets: { default: { provider: "codex/gpt", cwd: "/workspace", modeId: "test", titlePrefix: "World: " } },
})

test("World settings validate the WebSocket URL and provider/model", () => {
  assert.equal(settingsDefinition.schema.parse({}).enableAgentTools, true)
  assert.equal(settingsDefinition.schema.parse({
    targets: { default: { provider: "codex/vendor/model", cwd: "/workspace" } },
  }).targets.default.provider, "codex/vendor/model")
  for (const bad of [
    { paseoUrl: "http://localhost:6767/ws" },
    { paseoUrl: "ws://localhost:6767/" },
    { targets: { default: { provider: "codex", cwd: "/workspace" } } },
    { targets: { default: { provider: "/model", cwd: "/workspace" } } },
    { targets: { default: { provider: "codex/", cwd: "/workspace" } } },
  ]) assert.equal(settingsDefinition.schema.safeParse(bad).success, false)
})

test("world/open reuses a live session and only recreates a confirmed missing session", async () => {
  for (const tc of [
    { name: "new session", id: undefined, wantID: "new", key: "world:thread" },
    { name: "existing session", id: "existing", wantID: "existing", key: undefined },
    { name: "missing session", id: "missing", wantID: "new", key: "world:thread:after:missing" },
    { name: "transient failure", id: "transient", wantID: undefined, key: undefined },
  ]) {
    const creates: unknown[] = []
    const refreshes: string[] = []
    const agents: WorldAgents = {
      ref: (id) => ({
        id,
        async refresh() {
          refreshes.push(id)
          if (id === "transient") throw new Error("offline")
          return id === "missing" ? null : { agent: { id } as PaseoAgent, project: null }
        },
        async send() { assert.fail("unexpected send") },
        async run() { assert.fail("unexpected run") },
      }),
      async create(req) { creates.push(req); return { id: "new" } },
    }
    const result = worldCallbacks(agents, settings)("world/open", {
      thread_id: "thread", target: "default", title: "A question", existing_session_id: tc.id,
    })
    if (tc.wantID) assert.deepEqual(await result, { session_id: tc.wantID }, tc.name)
    else await assert.rejects(result, /offline/)
    assert.deepEqual(refreshes, tc.id ? [tc.id] : [])
    assert.deepEqual(creates, tc.key ? [{
      config: { provider: "codex/gpt", modeId: "test" },
      cwd: "/workspace", title: "World: A question", idempotencyKey: tc.key,
    }] : [])
  }
})

test("world/deliver maps run responses and world/notify steers with a stable ID", async () => {
  for (const [status, lastMessage] of [
    ["idle", "answer"], ["idle", null], ["error", null], ["permission", null], ["timeout", null],
  ] as const) {
    const sends: unknown[] = []
    const agents: WorldAgents = {
      ref: (id) => ({
        id,
        async refresh() { return null },
        async run(text, options): Promise<PaseoAgentRunResult> {
          assert.equal(id, "session")
          assert.equal(text, "World message from office@example.test\nSubject: question\n\nbody")
          assert.deepEqual(options, { messageId: "id", timeoutMs: 3210 })
          return { status, lastMessage, final: null, error: null }
        },
        async send(text, options) { sends.push({ id, text, options }) },
      }),
      async create() { assert.fail("unexpected create") },
    }
    const callbacks = worldCallbacks(agents, settings)
    const delivery = callbacks("world/deliver", {
      session_id: "session", message_id: "id", from: "office@example.test", subject: "question", body: "body",
    })
    if (status === "idle") assert.deepEqual(await delivery, { reply: lastMessage ?? "" })
    else await assert.rejects(delivery, new RegExp(status))
    assert.deepEqual(await callbacks("world/notify", {
      session_id: "session", message_id: "id", peer_addr: "office@example.test", reply_body: "answer",
    }), {})
    assert.deepEqual(sends, [{
      id: "session", text: "[World reply] office@example.test\nanswer",
      options: { activeTurnBehavior: "steer", messageId: "world-reply:id" },
    }])
  }
})

test("tools stay registered while connecting and scope message requests to the agent", async () => {
  let request: (<T>(method: string, params: unknown, signal?: AbortSignal) => Promise<T>) | undefined
  const [list, send, get] = worldTools(() => request && { request })
  const signal = AbortSignal.timeout(5_000)
  const context = { agent: { id: "agent" }, signal } as AgentToolContext
  assert.deepEqual(await list.execute({}, context), { isError: true, text: "World bridge is not ready" })
  const calls: unknown[] = []
  request = async <T,>(method: string, params: unknown, seenSignal?: AbortSignal) => {
    assert.equal(seenSignal, signal)
    calls.push([method, params])
    const msg = { message_id: "id", thread_id: "thread", status: "replied", reply_body: "answer" }
    return (method === "world/peers" ? { peers: [] } :
      method === "world/messages" ? { items: [msg] } : msg) as T
  }

  assert.deepEqual((await list.execute({}, context)).structuredContent, { peers: [] })
  for (const toolCall of [
    send.execute({ to: "office", message: "hello" }, context),
    get.execute({ message_id: "id" }, context),
  ]) {
    const result = await toolCall
    assert.deepEqual(result.structuredContent, { message_id: "id", thread_id: "thread", status: "replied", reply: "answer" })
    assert.deepEqual(JSON.parse(result.text!), result.structuredContent)
  }
  const thread = await get.execute({ thread_id: "thread" }, context)
  assert.equal("structuredContent" in thread, false)
  assert.deepEqual(JSON.parse(thread.text!), [
    { message_id: "id", thread_id: "thread", status: "replied", reply_body: "answer" },
  ])
  assert.deepEqual(calls, [
    ["world/peers", {}],
    ["world/send", { to: "office", message: "hello", wait: true, source_conversation_id: "agent", notify: true }],
    ["world/message", { message_id: "id", source_conversation_id: "agent" }],
    ["world/messages", { thread_id: "thread", direction: "outbound", conversation_id: "agent", cursor_id: undefined }],
  ])
  assert.equal((await get.execute({}, context)).isError, true)
})
