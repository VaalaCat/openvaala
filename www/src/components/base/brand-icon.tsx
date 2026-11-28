import { Bot } from "lucide-react"
import { cn } from "@/lib/utils"
import antigravity from "@lobehub/icons-static-svg/icons/antigravity.svg?url"
import cursor from "@lobehub/icons-static-svg/icons/cursor.svg?url"
import codex from "@lobehub/icons-static-svg/icons/codex.svg?url"
import claudecode from "@lobehub/icons-static-svg/icons/claudecode.svg?url"
import claude from "@lobehub/icons-static-svg/icons/claude.svg?url"
import geminicli from "@lobehub/icons-static-svg/icons/geminicli.svg?url"
import gemini from "@lobehub/icons-static-svg/icons/gemini.svg?url"
import opencode from "@lobehub/icons-static-svg/icons/opencode.svg?url"
import openai from "@lobehub/icons-static-svg/icons/openai.svg?url"
import amp from "@lobehub/icons-static-svg/icons/amp.svg?url"
import goose from "@lobehub/icons-static-svg/icons/goose.svg?url"
import kiro from "@lobehub/icons-static-svg/icons/kiro.svg?url"
import cline from "@lobehub/icons-static-svg/icons/cline.svg?url"
import githubcopilot from "@lobehub/icons-static-svg/icons/githubcopilot.svg?url"
import windsurf from "@lobehub/icons-static-svg/icons/windsurf.svg?url"
import trae from "@lobehub/icons-static-svg/icons/trae.svg?url"
import qwen from "@lobehub/icons-static-svg/icons/qwen.svg?url"
import kimi from "@lobehub/icons-static-svg/icons/kimi.svg?url"

const icons = new Map(
  Object.entries({
    antigravity,
    cursor,
    codex,
    claudecode,
    claude,
    geminicli,
    gemini,
    opencode,
    openai,
    amp,
    goose,
    kiro,
    cline,
    githubcopilot,
    windsurf,
    trae,
    qwen,
    kimi,
  })
)

export function BrandIcon({
  icon,
  className,
  "data-icon": placement,
}: {
  icon: string
  className?: string
  "data-icon"?: "inline-start" | "inline-end"
}) {
  const url = icons.get(icon)
  if (!url)
    return (
      <Bot
        aria-hidden="true"
        data-icon={placement}
        className={cn("size-4 shrink-0", className)}
      />
    )
  return (
    <span
      aria-hidden="true"
      data-icon={placement}
      className={cn("size-4 shrink-0 bg-current", className)}
      style={{
        maskImage: `url(${JSON.stringify(url)})`,
        maskSize: "contain",
        maskRepeat: "no-repeat",
        maskPosition: "center",
      }}
    />
  )
}
