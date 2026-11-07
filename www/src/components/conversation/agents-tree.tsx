import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useMemo } from "react"
import type { ParseKeys } from "i18next"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"
import { useServerConnection } from "@/components/server-connection-provider"
import { buttonVariants } from "@/components/ui/button"
import { LatticeLoader } from "@/components/base/lattice-loader"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import { useIsSessionLinked } from "@/components/conversation/session-link-context"
import type { SessionState } from "@/lib/api/protocol.gen"
import { getConversationPath, getSessionPath } from "@/lib/paths"
import {
  buildSessionTree,
  getSessionName,
  type SessionTreeNode,
} from "@/lib/session-tree"
import { cn } from "@/lib/utils"

// Session 运行阶段的颜色和文案；运行中另外显示加载格点
const sessionStates = {
  idle: {
    tone: "success",
    labelKey: "conversation.tool.agent.sessionState.idle",
  },
  running: {
    tone: "neutral",
    labelKey: "conversation.tool.agent.sessionState.running",
  },
  stopping: {
    tone: "warning",
    labelKey: "conversation.tool.agent.sessionState.stopping",
  },
  stopped: {
    tone: "neutral",
    labelKey: "conversation.tool.agent.sessionState.stopped",
  },
} as const satisfies Record<
  SessionState,
  { tone: "success" | "warning" | "neutral"; labelKey: ParseKeys }
>

// 子 Agent 页签：按 $sessions 画 Session 树，主 Agent 在最上面，子 Agent 按创建顺序挂在创建它的 Session 下；
// 每行是名字、Agent 名和运行状态，点击进入对应的 Session 页（旁支的 Session 没有页面，只显示），当前页面跟随的 Session 高亮
export function AgentsTree({ sync }: { sync: ConversationSync }) {
  const { t } = useTranslation()
  const sessions = useStore(sync.$sessions)
  const tree = useMemo(() => {
    const infos = Object.values(sessions)
    const root = infos.find((info) => !info.session.parent_session_id)
    return buildSessionTree(infos, root?.session.id ?? "")
  }, [sessions])
  const hasSubAgents = Object.keys(sessions).length > 1
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <ul className="flex min-w-0 flex-col">
        {tree.map((node) => (
          <SessionTreeItem
            key={node.info.session.id}
            node={node}
            depth={0}
            sync={sync}
          />
        ))}
      </ul>
      {!hasSubAgents && (
        <p className="px-2 text-caption color-muted">
          {t("conversation.panel.noSubAgents")}
        </p>
      )}
    </div>
  )
}

// 树里的一个 Session 和它创建的子 Agent；每深一层缩进 16px
function SessionTreeItem({
  node,
  depth,
  sync,
}: {
  node: SessionTreeNode
  depth: number
  sync: ConversationSync
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const { session, state } = node.info
  const isRoot = !session.parent_session_id
  const serverID = connection.getServerID()
  const isFollowed = session.id === sync.getFollowedSessionID()
  // serve 新增的状态用中性色
  const tone = sessionStates[state]?.tone ?? "neutral"
  const isLinked = useIsSessionLinked(sync)
  const rowStyle = { paddingInlineStart: 8 + depth * 16, paddingInlineEnd: 8 }
  const row = (
    <>
      <span className="flex size-4 shrink-0 items-center justify-center">
        {state === "running" ? (
          <LatticeLoader aria-hidden />
        ) : (
          <span
            aria-hidden
            data-status-tone={tone}
            className="size-2 rounded-full bg-current"
          />
        )}
      </span>
      <OverflowTooltip
        render={<span className="min-w-0 flex-1 truncate text-table" />}
      >
        {getSessionName(node.info, t("conversation.session.root"))}
      </OverflowTooltip>
      {!isRoot && session.task_name && (
        <OverflowTooltip
          render={
            <span className="max-w-20 min-w-0 truncate text-caption color-muted" />
          }
        >
          {session.agent_name}
        </OverflowTooltip>
      )}
      <span className="ml-auto shrink-0 text-caption color-muted">
        <SessionStateLabel state={state} />
      </span>
    </>
  )
  return (
    <li className="flex min-w-0 flex-col">
      {isLinked ? (
        <Link
          to={
            isRoot
              ? getConversationPath(serverID, sync.conversationID)
              : getSessionPath(serverID, sync.conversationID, session.id)
          }
          aria-current={isFollowed ? "page" : undefined}
          className={cn(
            buttonVariants({ variant: "ghost" }),
            "h-8 w-full min-w-0 justify-start gap-2 text-ui aria-[current=page]:bg-muted"
          )}
          style={rowStyle}
        >
          {row}
        </Link>
      ) : (
        <div
          aria-current={isFollowed ? "page" : undefined}
          className="flex h-8 w-full min-w-0 items-center gap-2 border border-transparent text-ui aria-[current=page]:bg-muted"
          style={rowStyle}
        >
          {row}
        </div>
      )}
      {node.children.length > 0 && (
        <ul className="flex min-w-0 flex-col">
          {node.children.map((child) => (
            <SessionTreeItem
              key={child.info.session.id}
              node={child}
              depth={depth + 1}
              sync={sync}
            />
          ))}
        </ul>
      )}
    </li>
  )
}

// Session 运行阶段的文案；子 Agent 树和 list_agents 工具卡共用，serve 新增的阶段原样显示
export function SessionStateLabel({ state }: { state: string }) {
  const { t } = useTranslation()
  return (
    <>
      {Object.hasOwn(sessionStates, state)
        ? t(sessionStates[state as SessionState].labelKey)
        : state}
    </>
  )
}
