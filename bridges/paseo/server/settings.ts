import { defineSettings } from "@getpaseo/plugin"
import { z } from "zod"

export const settingsDefinition = defineSettings({
  id: "openvaala-world",
  scope: "host",
  version: 1,
  schema: z.object({
    bridgeCommand: z.string().min(1).default("openvaala"),
    bridgeArgs: z.array(z.string()).default(["bridge"]),
    paseoUrl: z.url().refine((value) => {
      const url = new URL(value)
      return (url.protocol === "ws:" || url.protocol === "wss:") &&
        url.pathname.endsWith("/ws") && !url.username && !url.password && !url.hash
    }, "Enter a complete ws:// or wss:// URL ending in /ws").default("ws://127.0.0.1:6767/ws"),
    paseoPasswordEnv: z.string().regex(/^[A-Za-z_][A-Za-z0-9_]*$/).optional(),
    targets: z.record(
      z.string().regex(/^[a-z0-9][a-z0-9_-]{0,62}$/),
      z.object({
        provider: z.string().refine((value) => {
          const separator = value.indexOf("/")
          return separator > 0 && separator < value.length - 1 && !/\s/.test(value)
        }, "Use provider/model"),
        cwd: z.string().min(1),
        modeId: z.string().min(1).optional(),
        thinkingOptionId: z.string().min(1).optional(),
        titlePrefix: z.string().optional(),
      })
    ).default({}),
    enableAgentTools: z.boolean().default(true),
    turnTimeoutMs: z.number().int().positive().default(600_000),
  }),
})

export type WorldSettings = z.output<typeof settingsDefinition.schema>
