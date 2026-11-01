import { useCallback, useSyncExternalStore } from "react"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import { JsonView } from "@/components/base/json-view"
import { SessionStateLabel } from "@/components/conversation/agents-tree"
import { MarkdownView } from "@/components/conversation/markdown-view"
import { SessionLink } from "@/components/conversation/session-link"
import {
  KnownValueLabel,
  ToolCard,
  ToolMeta,
} from "@/components/conversation/tool-call-view"
import type { ToolRendererProps } from "@/lib/item-renderers"
import {
  getArrayField,
  getNumberField,
  getStringArrayField,
  getStringField,
} from "@/lib/tool-arguments"

// wait_agent 的唤醒原因和 interrupt_agent 的结果；serve 新增的取值原样显示
const waitReasonKeys: Record<string, ParseKeys> = {
  user_steer: "conversation.tool.agent.waitReason.userSteer",
  subagent_result: "conversation.tool.agent.waitReason.subagentResult",
  agent_message: "conversation.tool.agent.waitReason.agentMessage",
  timeout: "conversation.tool.agent.waitReason.timeout",
}
const interruptStatusKeys: Record<string, ParseKeys> = {
  cancel_requested: "conversation.tool.agent.interrupt.cancelRequested",
  no_active_turn: "conversation.tool.agent.interrupt.noActiveTurn",
}

// spawn_agent：详情显示任务和所选 Agent、模型，并链接到这次调用创建的子 Session。
export function SpawnAgentTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { call, result, sync } = props
  const { value } = props.arguments
  // 只在这次调用的子 Session 出现或换了时重渲染，不跟着其他 Session 的状态变化
  const subscribeSessions = useCallback(
    (onChange: () => void) => sync.$sessions.listen(onChange),
    [sync]
  )
  const createdSessionID = useSyncExternalStore(
    subscribeSessions,
    () =>
      Object.values(sync.$sessions.get()).find(
        (info) => info.session.parent_call_id === call.call_id
      )?.session.id
  )
  const childSessionID =
    createdSessionID ?? getStringField(result?.content, "session_id")
  const message = getStringField(value, "message")
  return (
    <ToolCard
      {...props}
      argumentSummary={
        getStringField(value, "task_name") || message?.split("\n")[0]
      }
      displayedArguments={[
        "message",
        "agent_name",
        "model",
        "reasoning_effort",
        "fork_turns",
      ]}
      target={
        childSessionID && <SessionLink sync={sync} sessionID={childSessionID} />
      }
    >
      {message && <MarkdownView text={message} />}
      <ToolMeta
        items={[
          getStringField(value, "agent_name"),
          getStringField(value, "model"),
          getStringField(value, "reasoning_effort"),
          getStringField(value, "fork_turns") &&
            t("conversation.tool.agent.forkTurns", {
              turns: getStringField(value, "fork_turns"),
            }),
        ]}
      />
    </ToolCard>
  )
}

// send_message：详情显示接收方链接、消息正文和提交方式。
export function SendMessageTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { sync } = props
  const { value } = props.arguments
  const sessionID = getStringField(value, "session_id")
  const message = getStringField(value, "message")
  const mode = getStringField(value, "mode")
  return (
    <ToolCard
      {...props}
      argumentSummary={message?.split("\n")[0]}
      displayedArguments={["message", "mode", "model"]}
      target={sessionID && <SessionLink sync={sync} sessionID={sessionID} />}
    >
      {message && <MarkdownView text={message} />}
      <ToolMeta
        items={[
          mode === "steer"
            ? t("conversation.tool.agent.mode.steer")
            : t("conversation.tool.agent.mode.normal"),
          getStringField(value, "model"),
        ]}
      />
    </ToolCard>
  )
}

// wait_agent：详情显示唤醒原因，并链接到发来消息的 Agent。
export function WaitAgentTool(props: ToolRendererProps) {
  const { result, sync } = props
  const reason = getStringField(result?.content, "reason")
  // from 没有发送方时省略
  const from = getStringArrayField(result?.content, "from")
  return (
    <ToolCard
      {...props}
      resultSummary={
        reason && <KnownValueLabel labelKeys={waitReasonKeys} value={reason} />
      }
      target={
        from.length > 0 &&
        from.map((sessionID) => (
          <SessionLink key={sessionID} sync={sync} sessionID={sessionID} />
        ))
      }
    />
  )
}

// interrupt_agent：详情显示取消请求的结果，并链接到被中断的子 Agent。
export function InterruptAgentTool(props: ToolRendererProps) {
  const { result, sync } = props
  const sessionID = getStringField(props.arguments.value, "session_id")
  const status = getStringField(result?.content, "status")
  return (
    <ToolCard
      {...props}
      argumentSummary={
        sessionID && <span className="text-code">{sessionID}</span>
      }
      resultSummary={
        status && (
          <KnownValueLabel labelKeys={interruptStatusKeys} value={status} />
        )
      }
      target={sessionID && <SessionLink sync={sync} sessionID={sessionID} />}
    />
  )
}

// list_agents：详情列出每个子 Agent 的链接、查询时的状态和排队的输入数。
// 结果正文同 pkg/conversation 的 listAgentsResult：sessions 没有后代时是 null；
// 每项的 status 是 session.SessionStatus 对象（state、pending_input_count 等）
export function ListAgentsTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { result, sync } = props
  const sessions = getArrayField(result?.content, "sessions").flatMap(
    (info) => {
      const sessionID = getStringField(
        (info as { session?: unknown } | null)?.session,
        "id"
      )
      const status = (info as { status?: unknown } | null)?.status
      return sessionID
        ? [
            {
              sessionID,
              state: getStringField(status, "state"),
              pendingInputCount: getNumberField(status, "pending_input_count"),
            },
          ]
        : []
    }
  )
  return (
    <ToolCard
      {...props}
      resultSummary={
        result &&
        !result.error &&
        t("conversation.tool.agent.listed", { count: sessions.length })
      }
    >
      {sessions.length > 0 && (
        <ul className="flex flex-col gap-1 text-ui">
          {sessions.map(({ sessionID, state, pendingInputCount }) => (
            <li
              key={sessionID}
              className="flex min-w-0 flex-wrap items-center gap-2"
            >
              <span className="min-w-0 text-table">
                <SessionLink sync={sync} sessionID={sessionID} />
              </span>
              {state && (
                <span className="color-muted">
                  <SessionStateLabel state={state} />
                </span>
              )}
              {!!pendingInputCount && (
                <span className="color-muted">
                  {t("conversation.tool.agent.pendingInputs", {
                    count: pendingInputCount,
                  })}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </ToolCard>
  )
}

// read_session_history：详情链接到读取的子 Agent，并用 JsonView 显示这一页历史。
export function ReadSessionHistoryTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { result, sync } = props
  const sessionID = getStringField(props.arguments.value, "session_id")
  // items 没有历史时是 null
  const items = getArrayField(result?.content, "items")
  return (
    <ToolCard
      {...props}
      argumentSummary={
        sessionID && <span className="text-code">{sessionID}</span>
      }
      resultSummary={
        result &&
        !result.error &&
        t("conversation.tool.agent.historyItems", { count: items.length })
      }
      target={sessionID && <SessionLink sync={sync} sessionID={sessionID} />}
    >
      {items.length > 0 && (
        <JsonView value={items} defaultExpandDepth={1} maxHeight="24rem" />
      )}
    </ToolCard>
  )
}
