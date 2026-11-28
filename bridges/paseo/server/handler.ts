import type { PaseoAgentCreateOptions, PaseoAgentHandle } from "@getpaseo/client"
import { defineAgentTool, type AgentToolResult } from "@getpaseo/plugin/server"
import { z } from "zod"
import type { BridgeClient } from "./bridge-client.js"
import type { WorldSettings } from "./settings.js"

type Session = Pick<PaseoAgentHandle, "id" | "refresh" | "run" | "send">
export type WorldAgents = {
  ref(id: string): Session
  create(options: PaseoAgentCreateOptions): Promise<Pick<Session, "id">>
}

const openInput = z.object({
  thread_id: z.string(),
  target: z.string(),
  title: z.string(),
  existing_session_id: z.string().optional(),
})
const deliverInput = z.object({
  session_id: z.string(),
  message_id: z.string(),
  from: z.string(),
  subject: z.string().optional(),
  body: z.string(),
})
const notifyInput = z.object({
  session_id: z.string(),
  message_id: z.string(),
  peer_addr: z.string(),
  reply_body: z.string().optional(),
  error_text: z.string().optional(),
})

export function worldCallbacks(agents: WorldAgents, settings: WorldSettings) {
  return async (method: string, params: unknown) => {
    switch (method) {
      case "world/open": {
        const request = openInput.parse(params)
        const existing = request.existing_session_id
        if (existing) {
          const agent = agents.ref(existing)
          if (await agent.refresh()) return { session_id: agent.id }
        }
        const target = settings.targets[request.target]
        if (!target) throw new Error("No Paseo agent configured for World target " + request.target)
        const { cwd, titlePrefix, ...config } = target
        const agent = await agents.create({
          config, cwd, title: (titlePrefix ?? "") + request.title,
          idempotencyKey: "world:" + request.thread_id + (existing ? ":after:" + existing : ""),
        })
        return { session_id: agent.id }
      }
      case "world/deliver": {
        const request = deliverInput.parse(params)
        const agent = agents.ref(request.session_id)
        const subject = request.subject ? "\nSubject: " + request.subject : ""
        const result = await agent.run(
          "World message from " + request.from + subject + "\n\n" + request.body,
          { messageId: request.message_id, timeoutMs: settings.turnTimeoutMs }
        )
        if (result.status !== "idle")
          throw new Error(`Paseo turn ended with ${result.status}${result.error ? ": " + result.error : ""}`)
        return { reply: result.lastMessage ?? "" }
      }
      case "world/notify": {
        const request = notifyInput.parse(params)
        const agent = agents.ref(request.session_id)
        await agent.send(
          "[World reply] " + request.peer_addr + "\n" + (request.error_text || request.reply_body || ""),
          { activeTurnBehavior: "steer", messageId: "world-reply:" + request.message_id }
        )
        return {}
      }
      default:
        throw new Error("Unknown World callback " + method)
    }
  }
}

type Message = {
  message_id: string
  thread_id: string
  status: string
  reply_body?: string
  error_text?: string
}

function summarize({ message_id, thread_id, status, reply_body, error_text }: Message) {
  return {
    message_id, thread_id, status,
    ...(status === "replied" ? { reply: reply_body ?? "" } : {}),
    ...(error_text ? { error: error_text } : {}),
  }
}

export function worldTools(getBridge: () => Pick<BridgeClient, "request"> | undefined) {
  async function run(action: (bridge: Pick<BridgeClient, "request">) => Promise<unknown>): Promise<AgentToolResult> {
    try {
      const bridge = getBridge()
      if (!bridge) throw new Error("World bridge is not ready")
      const result = await action(bridge)
      const text = JSON.stringify(result)
      return Array.isArray(result) ? { text } : { text, structuredContent: result }
    } catch (error) {
      return { isError: true, text: error instanceof Error ? error.message : String(error) }
    }
  }

  return [
    defineAgentTool({
      name: "list_world_peers",
      description: "List visible World contacts, their availability, and advertised targets.",
      input: z.object({}),
      execute: (_, { signal }) => run((bridge) => bridge.request("world/peers", {}, signal)),
    }),
    defineAgentTool({
      name: "send_world_message",
      description: "Send a World message or continue its thread. Wait for a reply by default.",
      input: z.object({
        to: z.string().min(1),
        message: z.string().min(1),
        subject: z.string().optional(),
        thread_id: z.string().optional(),
        wait: z.boolean().default(true),
      }),
      execute: (input, { agent, signal }) => run(async (bridge) => summarize(await bridge.request<Message>(
        "world/send",
        { ...input, wait: input.wait ?? true, source_conversation_id: agent.id, notify: true },
        signal
      ))),
    }),
    defineAgentTool({
      name: "get_world_message",
      description: "Read a World message or list outbound messages in a thread from this agent.",
      input: z.object({ message_id: z.string().min(1).optional(), thread_id: z.string().min(1).optional() }),
      execute: ({ message_id, thread_id }, { agent, signal }) => run(async (bridge) => {
        if (message_id) return summarize(await bridge.request<Message>("world/message",
          { message_id, source_conversation_id: agent.id }, signal))
        if (!thread_id) throw new Error("message_id or thread_id is required")
        const items: Message[] = []
        let cursor: string | undefined
        do {
          const page: { items: Message[]; next_cursor?: { id: string } } = await bridge.request("world/messages", {
            thread_id, direction: "outbound", conversation_id: agent.id, cursor_id: cursor,
          }, signal)
          items.push(...page.items)
          cursor = page.next_cursor?.id
        } while (cursor)
        return items
      }),
    }),
  ]
}
