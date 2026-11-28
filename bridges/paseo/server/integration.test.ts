import assert from "node:assert/strict"
import { execFile, spawn, type ChildProcess } from "node:child_process"
import { once } from "node:events"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { createServer } from "node:http"
import type { AddressInfo } from "node:net"
import { tmpdir } from "node:os"
import { join } from "node:path"
import test from "node:test"
import { setTimeout as delay } from "node:timers/promises"
import { fileURLToPath } from "node:url"
import { promisify } from "node:util"
import type {
  AgentToolContext,
  PluginHookPayload,
  PluginHookResult,
  PluginServerContext,
  PluginSettingsState,
} from "@getpaseo/plugin/server"
import { WebSocket, WebSocketServer } from "ws"
import contribute from "../index.server.js"
import { settingsDefinition } from "./settings.js"

const exec = promisify(execFile)
const root = fileURLToPath(new URL("../../../", import.meta.url))
const adminToken = "world-integration-admin"
const serveToken = "world-integration-serve"
const domain = "world.test"

async function openRuntimeRPC(baseURL: string, token: string) {
  const socket = new WebSocket(baseURL.replace(/^http/, "ws") + "/api/v1/openvaala", {
    headers: { Authorization: "Bearer " + token },
  })
  let nextID = 0
  async function call<T>(method: string, params: unknown): Promise<T> {
    const id = ++nextID
    let cleanupCall = () => {}
    const result = new Promise<T>((resolve, reject) => {
      const onMessage = (data: WebSocket.RawData) => {
        const frame = JSON.parse(data.toString())
        if (frame.id !== id) return
        if (frame.error) reject(new Error(frame.error.message))
        else resolve(frame.result as T)
      }
      const onClose = () => reject(new Error("Runtime RPC disconnected"))
      socket.on("message", onMessage)
      socket.once("close", onClose)
      socket.send(JSON.stringify({ id, method, params }), (err) => {
        if (err) reject(err)
      })
      cleanupCall = () => {
        socket.off("message", onMessage)
        socket.off("close", onClose)
      }
    })
    try {
      return await result
    } finally {
      cleanupCall()
    }
  }
  try {
    await once(socket, "open")
    await call("initialize", { protocol_version: "6" })
    return { call, close: () => socket.close() }
  } catch (err) {
    socket.close()
    throw err
  }
}

async function stop(child: ChildProcess) {
  if (child.exitCode !== null || child.signalCode !== null) return
  const closed = once(child, "close")
  child.kill("SIGTERM")
  const deadline = setTimeout(() => child.kill("SIGKILL"), 3000)
  try {
    await closed
  } finally {
    clearTimeout(deadline)
  }
}

async function eventually<T>(fn: () => Promise<T | undefined>): Promise<T> {
  const until = Date.now() + 10_000
  while (Date.now() < until) {
    const value = await fn()
    if (value !== undefined) return value
    await delay(25)
  }
  throw new Error("Timed out waiting for the integration state")
}

async function listen(server: ReturnType<typeof createServer>) {
  server.listen(0, "127.0.0.1")
  await once(server, "listening")
  return "http://127.0.0.1:" + (server.address() as AddressInfo).port
}

async function unusedAddress() {
  const server = createServer()
  const base = await listen(server)
  await new Promise<void>((resolve) => server.close(() => resolve()))
  return base
}

async function request<T>(url: string, token: string, body?: unknown, method = body ? "POST" : "GET") {
  const response = await fetch(url, {
    method,
    headers: { Authorization: "Bearer " + token, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(10_000),
  })
  if (!response.ok) throw new Error(`${url}: ${response.status} ${await response.text()}`)
  return await response.json() as T
}

async function waitHealth(url: string, child: ChildProcess) {
  await eventually(async () => {
    if (child.exitCode !== null) throw new Error("OpenVaala exited before starting: " + child.exitCode)
    const response = await fetch(url, { signal: AbortSignal.timeout(1000) }).catch(() => undefined)
    return response?.ok || undefined
  })
}

type DaemonRequest = {
  type: string
  requestId: string
  agentId: string
  text: string
  messageId: string
  activeTurnBehavior?: string
  idempotencyKey?: string
  config?: { provider: string; model: string; cwd: string; title: string }
}

async function fakePaseo() {
  const socket = new WebSocketServer({ host: "127.0.0.1", port: 0, path: "/ws" })
  await once(socket, "listening")
  const messages: DaemonRequest[] = []
  const unexpected: string[] = []
  let connections = 0
  socket.on("connection", (client) => {
    client.on("message", (json) => {
      const envelope = JSON.parse(String(json)) as { type: string; message?: DaemonRequest }
      const respond = (type: string, payload: unknown) =>
        client.send(JSON.stringify({ type: "session", message: { type, payload } }))
      if (envelope.type === "hello") {
        connections++
        respond("status", { status: "server_info", serverId: "integration-daemon", version: "0.17.3", features: { creationLifecycle: true } })
        return
      }
      const req = envelope.message
      if (!req) return
      messages.push(req)
      const { requestId, agentId } = req
      switch (req.type) {
        case "agent.create.request": {
          const now = new Date().toISOString()
          const agent = {
            id: "paseo-session", provider: req.config?.provider, model: req.config?.model,
            cwd: req.config?.cwd, title: req.config?.title,
            createdAt: now, updatedAt: now, lastUserMessageAt: null, status: "idle",
            currentModeId: null, availableModes: [], pendingPermissions: [], persistence: null,
            capabilities: {
              supportsStreaming: true, supportsSessionPersistence: true, supportsDynamicModes: true,
              supportsMcpServers: true, supportsReasoningStream: true, supportsToolInvocations: true,
            },
          }
          respond("agent.create.response", { requestId, agent, error: null })
          return
        }
        case "send_agent_message_request":
          respond("send_agent_message_response", { requestId, agentId, accepted: true, error: null })
          return
        case "wait_for_finish_request":
          respond("wait_for_finish_response", {
            requestId, status: "idle", final: null, error: null, lastMessage: "reply from Paseo",
          })
          return
        default:
          unexpected.push(req.type)
      }
    })
  })
  return {
    url: "ws://127.0.0.1:" + (socket.address() as AddressInfo).port + "/ws",
    messages,
    unexpected,
    connections: () => connections,
    close: () => new Promise<void>((resolve) => {
      for (const client of socket.clients) client.terminate()
      socket.close(() => resolve())
    }),
  }
}

type SettingsState = PluginSettingsState<typeof settingsDefinition.schema>
type OpenRequest = PluginHookPayload<"agent.open.before">["request"]
type OpenResult = PluginHookResult<"agent.open.before">

function pluginHost(initial: SettingsState | Promise<SettingsState>) {
  let open: ((payload: { request: OpenRequest }) => Promise<OpenResult>) | undefined
  let subscriber: ((state: SettingsState) => void) | undefined
  const registered: string[] = []
  const server = {
    registerSettings(definition: typeof settingsDefinition) {
      registered.push(definition.id)
      return {
        read: async () => initial,
        subscribe(listener: typeof subscriber) {
          subscriber = listener
          return () => { subscriber = undefined }
        },
      }
    },
    addHook(name: string, handler: typeof open) {
      assert.equal(name, "agent.open.before")
      open = handler
      return () => { open = undefined }
    },
  }
  return {
    server: server as unknown as PluginServerContext,
    registered,
    configure(state: SettingsState) { assert.ok(subscriber); subscriber(state) },
    async getTools(purpose: OpenRequest["purpose"] = "interactive") {
      const request: OpenRequest = {
        agentId: "interactive-agent", workspaceId: null, provider: "codex", cwd: root,
        reason: "create", purpose, title: null, background: false, labels: {}, metadata: {},
        tools: [], env: {}, mcpServers: {},
      }
      return (await open?.({ request }))?.tools ?? []
    },
  }
}

type Message = { status: string; message_id: string; thread_id: string; reply?: string; reply_body?: string }

test("interactive tools wait for the first settings read without starting a bridge", async () => {
  const paseo = await fakePaseo()
  let complete!: (state: SettingsState) => void
  const host = pluginHost(new Promise((resolve) => { complete = resolve }))
  const cleanup = contribute(host.server)
  try {
    const firstOpen = host.getTools()
    complete({
      status: "ready", revision: "1",
      values: settingsDefinition.schema.parse({ paseoUrl: paseo.url, enableAgentTools: false }),
    })
    assert.deepEqual(await firstOpen, [])
  } finally {
    await cleanup()
    await paseo.close()
  }
})

test("Paseo entry and real OpenVaala nodes exchange messages in both directions", { timeout: 150_000 }, async (t) => {
  const tmp = await mkdtemp(join(tmpdir(), "openvaala-paseo-"))
  const children: ChildProcess[] = []
  const logs = new Map<ChildProcess, string>()
  const paseo = await fakePaseo()
  let cleanup: (() => void | Promise<void>) | undefined
  const modelPrompts: string[] = []
  const model = createServer(async (req, res) => {
    let body = ""
    for await (const part of req) body += part
    modelPrompts.push(body)
    res.setHeader("Content-Type", "text/event-stream")
    for (const choices of [
      [{ index: 0, delta: { role: "assistant", content: "reply from OpenVaala" }, finish_reason: null }],
      [{ index: 0, delta: {}, finish_reason: "stop" }],
    ]) {
      res.write("data: " + JSON.stringify({
        id: "test-completion", object: "chat.completion.chunk", model: "default", choices,
      }) + "\n\n")
    }
    res.end("data: [DONE]\n\n")
  })
  t.after(async () => {
    await cleanup?.()
    await Promise.all(children.map(stop))
    await paseo.close()
    await new Promise<void>((resolve) => model.close(() => resolve()))
    await rm(tmp, { recursive: true, force: true })
  })

  const binary = join(tmp, "openvaala")
  await exec("go", ["build", "-o", binary, "./cmd/openvaala"], {
    cwd: root, env: { ...process.env, CGO_ENABLED: "0" }, timeout: 90_000,
  })
  const control = await unusedAddress()
  const native = await unusedAddress()
  const modelURL = await listen(model)
  async function config(name: string, yaml: string) {
    const home = join(tmp, name)
    await mkdir(home)
    const path = join(home, "config.yaml")
    await writeFile(path, yaml, { mode: 0o600 })
    return { home, path, env: { ...process.env, HOME: home } }
  }
  const controlConfig = await config("control", `world:
  server:
    enabled: true
    listen: ${new URL(control).host}
    domain: ${domain}
    admin_token: ${adminToken}
`)
  const hostConfig = await config("host", `world:
  node:
    default_target: paseo-only
    advertise_targets: [{name: paseo-only, visibility: domain}]
`)
  const nativeConfig = await config("native", `run:
  agent_name: assistant
server:
  listen: ${new URL(native).host}
  token: ${serveToken}
`)
  await mkdir(join(nativeConfig.home, "agents"))
  await mkdir(join(nativeConfig.home, "models"))
  await writeFile(join(nativeConfig.home, "agents", "assistant.yaml"), "name: assistant\ninstructions: Reply concisely.\nmodel: {provider: local}\n")
  await writeFile(join(nativeConfig.home, "models", "local.yaml"), `id: local
protocol: openai_chat
base_url: ${modelURL}
models:
  default: {}
`)
  function start(cfg: typeof controlConfig, ...args: string[]) {
    const child = spawn(binary, ["--config-file", cfg.path, ...args], { env: cfg.env })
    children.push(child)
    for (const stream of [child.stdout, child.stderr]) {
      stream.on("data", (chunk) => logs.set(child, (logs.get(child) ?? "") + String(chunk)))
    }
    return child
  }
  try {
    await waitHealth(control + "/world/v1/healthz", start(controlConfig, "world", "serve"))
    const { token } = await request<{ token: string }>(control + "/world/v1/admin/tokens", adminToken, {
      owner: "integration", tags: [], reusable: true, expires_at: Date.now() + 60_000,
    })
    for (const [name, cfg] of [["paseo", hostConfig], ["native", nativeConfig]] as const) {
      await exec(binary, ["--config-file", cfg.path, "world", "join", token, "--name", name], { env: cfg.env })
    }
    await waitHealth(native + "/api/v1/healthz", start(nativeConfig, "serve"))

    const rpc = await openRuntimeRPC(native, serveToken)
    t.after(rpc.close)

    const values = settingsDefinition.schema.parse({
      bridgeCommand: "/usr/bin/env",
      bridgeArgs: ["HOME=" + hostConfig.home, binary, "bridge", "--config-file", hostConfig.path],
      paseoUrl: paseo.url,
      targets: { "paseo-only": { provider: "codex/test-model", cwd: tmp, titlePrefix: "World: " } },
    })
    const host = pluginHost({ status: "ready", revision: "1", values })
    cleanup = contribute(host.server)
    assert.deepEqual(host.registered, ["openvaala-world"])
    const [list, send, get] = await host.getTools()
    assert.deepEqual([list.name, send.name, get.name], ["list_world_peers", "send_world_message", "get_world_message"])
    assert.equal((await host.getTools("history")).length, 0)
    const context = { agent: { id: "interactive-agent" }, signal: t.signal } as AgentToolContext
    assert.equal((await list.execute({}, context)).isError, true)
    await eventually(async () => {
      const data = await rpc.call<{ peers: { node_name: string; online: boolean; targets: unknown[] }[] }>("world/listPeers", {})
      return data.peers.find((peer) => peer.node_name === "paseo" && peer.online && peer.targets?.length)
    })
    const inbound = await rpc.call<Message>("world/sendMessage", {
      to: "paseo", subject: "A question", message: "hello from OpenVaala", wait: true,
    })
    assert.equal(inbound.status, "replied")
    assert.equal(inbound.reply_body, "reply from Paseo")
    assert.equal(paseo.connections(), 1)
    assert.equal(paseo.messages.find((req) => req.type === "agent.create.request")?.idempotencyKey, "world:" + inbound.thread_id)
    const delivered = paseo.messages.find((req) => req.messageId === inbound.message_id)
    assert.match(delivered?.text ?? "", /hello from OpenVaala/)
    assert.equal(delivered?.agentId, "paseo-session")

    const listed = await list.execute({}, context)
    assert.match(listed.text ?? "", /native@world\.test/)
    const outbound = await send.execute({ to: "native", message: "hello from Paseo" }, context)
    assert.equal(outbound.isError, undefined, outbound.text)
    const summary = outbound.structuredContent as Message
    assert.equal(summary.status, "replied")
    assert.equal(summary.reply, "reply from OpenVaala")
    assert.ok(modelPrompts.some((prompt) => prompt.includes("hello from Paseo")))
    assert.deepEqual((await get.execute({ message_id: summary.message_id }, context)).structuredContent, summary)
    const threadResult = await get.execute({ thread_id: summary.thread_id }, context)
    assert.equal("structuredContent" in threadResult, false)
    const thread = JSON.parse(threadResult.text!) as Message[]
    assert.deepEqual(thread.map((entry) => entry.message_id), [summary.message_id])
    const anotherAgent = { ...context, agent: { ...context.agent, id: "another-agent" } }
    assert.equal((await get.execute({ message_id: summary.message_id }, anotherAgent)).isError, true)

    const queued = await send.execute({ to: "native", message: "reply asynchronously", wait: false }, context)
    assert.equal(queued.isError, undefined, queued.text)
    const queuedID = (queued.structuredContent as Message).message_id
    const followup = await eventually(async () => paseo.messages.find((req) => req.messageId === "world-reply:" + queuedID))
    assert.equal(followup.agentId, "interactive-agent")
    assert.equal(followup.activeTurnBehavior, "steer")
    assert.match(followup.text, /reply from OpenVaala/)
    assert.deepEqual(paseo.unexpected, [])

    host.configure({ status: "ready", revision: "2", values: { ...values, enableAgentTools: false } })
    assert.equal((await host.getTools()).length, 0)
    await eventually(async () => paseo.connections() === 2 ? true : undefined)
    await cleanup()
    cleanup = undefined
    assert.equal((await host.getTools()).length, 0)
  } catch (error) {
    for (const log of logs.values()) t.diagnostic(log)
    throw error
  }
})
