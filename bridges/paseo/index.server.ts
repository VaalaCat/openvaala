import { addAbortListener } from "node:events"
import { createPaseoClient } from "@getpaseo/client"
import type { PluginServerContext } from "@getpaseo/plugin/server"
import { BridgeClient } from "./server/bridge-client.js"
import { worldCallbacks, worldTools } from "./server/handler.js"
import { settingsDefinition, type WorldSettings } from "./server/settings.js"

async function runBridge(
  values: WorldSettings,
  signal: AbortSignal,
  setBridge: (bridge?: BridgeClient) => void
) {
  const password = values.paseoPasswordEnv ? process.env[values.paseoPasswordEnv] : undefined
  if (values.paseoPasswordEnv && !password) {
    throw new Error("Missing Paseo password environment variable " + values.paseoPasswordEnv)
  }
  const paseo = createPaseoClient({ url: values.paseoUrl, password })
  let bridge: BridgeClient | undefined
  let closing: Promise<unknown> | undefined
  const close = () => closing ??= Promise.all([bridge?.close(), paseo.close()])
  const abort = addAbortListener(signal, () => { void close().catch(() => {}) })
  try {
    await paseo.connect()
    signal.throwIfAborted()
    bridge = new BridgeClient(values.bridgeCommand, values.bridgeArgs, worldCallbacks(paseo.agents, values))
    const init = await bridge.request<{ protocol_version: number }>(
      "world/initialize", { protocol_version: 1, host_name: "paseo" }, signal
    )
    if (init.protocol_version !== 1) throw new Error("Unsupported World bridge protocol version")
    signal.throwIfAborted()
    setBridge(bridge)
    await bridge.closed
    signal.throwIfAborted()
    throw new Error("World bridge disconnected")
  } finally {
    setBridge(undefined)
    abort[Symbol.dispose]()
    await close()
  }
}

export default function contribute(server: PluginServerContext) {
  const settings = server.registerSettings(settingsDefinition)
  let bridge: BridgeClient | undefined
  let desired: AbortController | undefined
  let transition = Promise.resolve()
  let revision: string | undefined
  let stopped = false
  let enableAgentTools = false
  const tools = worldTools(() => bridge)

  const apply = (state: Awaited<ReturnType<typeof settings.read>>) => {
    if (stopped || revision === state.revision) return
    revision = state.revision
    desired?.abort()
    bridge = undefined
    if (state.status === "invalid") {
      enableAgentTools = false
      console.error("Invalid World settings:", state.error)
      return
    }
    enableAgentTools = state.values.enableAgentTools
    const current = new AbortController()
    desired = current
    transition = transition.then(async () => {
      if (current.signal.aborted) return
      try {
        await runBridge(state.values, current.signal, (ready) => { bridge = ready })
      } catch (error) {
        if (!current.signal.aborted) console.error("World bridge:", error)
      }
    })
  }

  const unsubscribe = settings.subscribe(apply)
  const initial = settings.read().then(
    (state) => { if (revision === undefined) apply(state) },
    (error: unknown) => { if (!stopped) console.error("Read World settings:", error) }
  )
  const unregister = server.addHook("agent.open.before", async ({ request }) => {
    if (request.purpose !== "interactive") return
    await initial
    if (enableAgentTools) return { ...request, tools: [...request.tools, ...tools] }
  })
  return async () => {
    stopped = true
    unregister()
    desired?.abort()
    await unsubscribe()
    await initial
    await transition
  }
}
