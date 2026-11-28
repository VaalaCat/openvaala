import { useQuery } from "@tanstack/react-query"
import { useServerConnection } from "@/components/server-connection-provider"
import { agentCatalogOptions } from "@/lib/query/agent-settings"
import { BrandIcon } from "@/components/base/brand-icon"
import type { ComponentProps } from "react"
import {
  BotIcon,
  GitBranchIcon,
  MessageSquareIcon,
  TerminalIcon,
} from "lucide-react"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"
import type { TabWorkspaceTab } from "@/lib/workspace/types"

const tabIcons = {
  main: MessageSquareIcon,
  "sub-agent": BotIcon,
  "side-conversation": GitBranchIcon,
  terminal: TerminalIcon,
} as const satisfies Record<TabWorkspaceTab["kind"], unknown>

// 标签页的图标：主对话、子 Agent、旁支、终端各一个；标签页换了种类时跟着更新
export function WorkspaceTabIcon({
  workspace,
  tabID,
  ...props
}: {
  workspace: TabWorkspace
  tabID: string
} & ComponentProps<"svg"> & { "data-tab-closable"?: boolean }) {
  const tab = useStore(workspace.$tabs)[tabID]
  const kind = tab?.kind
  const sync = workspace.getConversationSync(tabID) ?? workspace.getRootSync()
  const root = useStore(sync.$rootSessionID)
  const sessionID = tab?.kind === "sub-agent" ? tab.sessionID : root
  const session = useStore(sync.$sessions, { keys: [sessionID ?? ""] })[
    sessionID ?? ""
  ]?.session
  const agents = useQuery(agentCatalogOptions(useServerConnection()))
  if (kind !== "terminal" && session?.external)
    return (
      <span
        data-tab-closable={props["data-tab-closable"]}
        className="inline-flex"
      >
        <BrandIcon
          className={props.className}
          icon={
            agents.data?.agents?.find((a) => a.name === session.agent_name)
              ?.icon ?? ""
          }
        />
      </span>
    )
  if (!kind) return null
  const Icon = tabIcons[kind]
  return <Icon {...props} />
}

// 标签页的标题：标签栏和窄屏抽屉共用，对话标题、子 Agent 的 Session 信息、终端标题变化和切换语言时跟着更新
export function WorkspaceTabTitle({
  workspace,
  tabID,
}: {
  workspace: TabWorkspace
  tabID: string
}) {
  // 1. 标签页内容、对话标题、子 Agent 的 Session 信息或终端标题变化时重新取标题；语言切换由 useTranslation 触发。
  useTranslation()
  const tab = useStore(workspace.$tabs)[tabID]
  const rootSync = workspace.getRootSync()
  useStore(rootSync.$title)
  useStore(rootSync.$sessions, {
    keys: [tab?.kind === "sub-agent" ? tab.sessionID : ""],
  })
  useStore(workspace.$terminalTitles)
  return workspace.getTabTitle(tabID)
}
