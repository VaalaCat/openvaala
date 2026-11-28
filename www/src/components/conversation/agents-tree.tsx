import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { ArrowUpRight } from "lucide-react"
import { SessionMenu } from "./session-menu"
import { BrandIcon } from "@/components/base/brand-icon"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { sessionListOptions } from "@/lib/query/session"
import { useInfiniteQuery, useQuery } from "@tanstack/react-query"
import { agentCatalogOptions } from "@/lib/query/agent-settings"
import { useEffect, useMemo, useRef } from "react"
import type { ParseKeys } from "i18next"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { NavigationButton } from "@/components/base/navigation-button"
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
export function AgentsTree({
  sync,
  showBackground,
  setShowBackground,
}: {
  sync: ConversationSync
  showBackground: boolean
  setShowBackground(value: boolean): void
}) {
  const { t } = useTranslation()
  const sessions = useStore(sync.$sessions)
  const connection = useServerConnection()
  const catalog = useQuery(agentCatalogOptions(connection))
  const { data, hasNextPage, isFetching, isError, fetchNextPage } =
    useInfiniteQuery(sessionListOptions(connection, sync.conversationID))
  const sentinel = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!sentinel.current || !hasNextPage || isFetching || isError) return
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) void fetchNextPage()
    })
    observer.observe(sentinel.current)
    return () => observer.disconnect()
  }, [hasNextPage, isFetching, isError, fetchNextPage])
  const infos = useMemo(() => {
    const records = new Map(
      Object.entries(sessions) as [string, SessionTreeNode["info"]][]
    )
    for (const page of data?.pages ?? [])
      for (const session of page.sessions ?? []) {
        const live = records.get(session.id)
        if (!live || (session.updated_at ?? 0) > (live.session.updated_at ?? 0))
          records.set(session.id, { session, state: live?.state })
      }
    return [...records.values()]
  }, [sessions, data])
  const icons = useMemo(
    () => new Map(catalog.data?.agents?.map((a) => [a.name, a.icon ?? ""])),
    [catalog.data]
  )
  const background = infos.filter((i) => i.session.background).length
  const tree = useMemo(() => {
    const root = infos.find((info) => !info.session.parent_session_id)
    return buildSessionTree(
      infos.filter((i) => showBackground || !i.session.background),
      root?.session.id ?? ""
    )
  }, [infos, showBackground])
  const hasSubAgents = infos.length > 1
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <ul className="flex min-w-0 flex-col">
        {tree.map((node) => (
          <SessionTreeItem
            key={node.info.session.id}
            node={node}
            icons={icons}
            depth={0}
            sync={sync}
          />
        ))}
      </ul>
      {hasNextPage && <div ref={sentinel} />}
      {background > 0 && (
        <button
          type="button"
          className={cn(
            "h-8 self-start rounded-sm pr-2 text-left text-caption color-muted hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring",
            tree[0]?.info.session.external ? "pl-12" : "pl-7"
          )}
          aria-expanded={showBackground}
          onClick={() => setShowBackground(!showBackground)}
        >
          {t("externalSession.background", { count: background })}
        </button>
      )}
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
  icons,
  depth,
  sync,
}: {
  icons: Map<string, string>
  node: SessionTreeNode
  depth: number
  sync: ConversationSync
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const { session, state } = node.info
  const labels = Object.entries(session.labels ?? {})
    .map(([key, value]) => (value ? key + "=" + value : key))
    .join(" ")
  const detached = session.detached_conversation_id
  const isRoot = !session.parent_session_id
  const serverID = connection.getServerID()
  const isFollowed = session.id === sync.getFollowedSessionID()
  // serve 新增的状态用中性色
  const tone = (state && sessionStates[state]?.tone) ?? "neutral"
  const isLinked = useIsSessionLinked(sync)
  const rowStyle = { paddingInlineStart: 8 + depth * 16, paddingInlineEnd: 8 }
  const row = (
    <>
      <span className="flex w-full min-w-0 items-center gap-1">
        <span className="flex size-4 shrink-0 items-center justify-center">
          {!detached && state === "running" ? (
            <LatticeLoader aria-hidden />
          ) : (
            <span
              aria-hidden
              data-status-tone={tone}
              className={cn(
                "size-2 rounded-full",
                detached ? "border border-current" : "bg-current"
              )}
            />
          )}
        </span>
        {session.external && (
          <BrandIcon icon={icons.get(session.agent_name) ?? ""} />
        )}
        <span className="grid min-w-0 flex-1 grid-cols-[minmax(0,max-content)_minmax(0,1fr)] items-center text-left">
          <span className="flex min-w-0 items-center gap-1">
            <OverflowTooltip
              render={<span className="min-w-0 truncate text-table" />}
            >
              {getSessionName(node.info, t("conversation.session.root"))}
            </OverflowTooltip>
            {detached && <ArrowUpRight className="size-3 shrink-0" />}
          </span>
          {!isRoot && session.task_name && !detached && (
            <OverflowTooltip
              render={
                <span className="max-w-20 min-w-0 justify-self-end truncate pl-2 text-caption color-muted" />
              }
            >
              {session.agent_name}
            </OverflowTooltip>
          )}
        </span>
        <span className="ml-auto shrink-0 text-caption color-muted">
          {state && <SessionStateLabel state={state} />}
        </span>
      </span>
      {labels && (
        <OverflowTooltip
          render={
            <span
              className={cn(
                "block w-full min-w-0 truncate text-left text-caption color-muted",
                session.external ? "pl-10" : "pl-5"
              )}
            />
          }
        >
          {labels}
        </OverflowTooltip>
      )}
    </>
  )
  const trigger = (
    <div className={session.background ? "color-subtle" : undefined}>
      <Tooltip disabled={!detached}>
        <TooltipTrigger
          render={
            isLinked || detached ? (
              <NavigationButton
                to={
                  detached
                    ? getConversationPath(serverID, detached)
                    : isRoot
                      ? getConversationPath(serverID, sync.conversationID)
                      : getSessionPath(
                          serverID,
                          sync.conversationID,
                          session.id
                        )
                }
                aria-current={isFollowed ? "page" : undefined}
                className={cn(
                  buttonVariants({ variant: "ghost" }),
                  "h-auto min-h-8 w-full min-w-0 flex-col items-stretch justify-center gap-0 border-0 text-ui aria-[current=page]:bg-muted"
                )}
                style={rowStyle}
               />
            ) : (
              <div
                aria-current={isFollowed ? "page" : undefined}
                className="flex min-h-8 w-full min-w-0 flex-col items-stretch justify-center gap-0 text-ui aria-[current=page]:bg-muted"
                style={rowStyle}
              ></div>
            )
          }
        >
          {row}
        </TooltipTrigger>
        <TooltipContent>{t("externalSession.detached")}</TooltipContent>
      </Tooltip>
    </div>
  )
  return (
    <li className="flex min-w-0 flex-col">
      <SessionMenu session={session} sync={sync} trigger={trigger} />
      {node.children.length > 0 && (
        <ul className="flex min-w-0 flex-col">
          {node.children.map((child) => (
            <SessionTreeItem
              key={child.info.session.id}
              node={child}
              icons={icons}
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
