import { map } from "nanostores"
import type {
  ConversationID,
  SessionID,
  SessionRuntimeInfo,
  TurnID,
} from "./api/protocol.gen"

export type KnownConversationResult = {
  turnID: TurnID
  status: "completed" | "failed" | "interrupted"
}

// 本网页从正常对话订阅收到的根 Session 结束结果，刷新后清空。
export const $conversationResults = map<
  Record<string, KnownConversationResult>
>({})

// 返回服务器和对话共同组成的结果身份。
export function getConversationResultKey(
  serverID: string,
  conversationID: ConversationID
): string {
  // 1. 分别编码服务器与对话身份。
  return JSON.stringify([serverID, conversationID])
}

// 记录根 Session 最近一次已结束的 Turn。
export function recordConversationResult(
  serverID: string,
  conversationID: ConversationID,
  rootSessionID: SessionID,
  info: Pick<
    SessionRuntimeInfo,
    "session" | "last_turn_id" | "last_turn_status"
  >
): void {
  // 1. 子 Session、没有 Turn 和运行中的状态不改变已知结果。
  const status = info.last_turn_status
  if (
    info.session.id !== rootSessionID ||
    !info.last_turn_id ||
    (status !== "completed" && status !== "failed" && status !== "interrupted")
  )
    return
  const key = getConversationResultKey(serverID, conversationID)
  const previous = $conversationResults.get()[key]
  if (previous?.turnID === info.last_turn_id && previous.status === status)
    return
  $conversationResults.setKey(key, { turnID: info.last_turn_id, status })
}

// 按运行集合和已知结果选择对话显示状态。
export function getConversationStatus(
  isRunning: boolean | undefined,
  result?: KnownConversationResult
): "unknown" | "running" | "idle" | KnownConversationResult["status"] {
  // 1. 未知和运行中优先于本地旧结果。
  if (isRunning === undefined) return "unknown"
  if (isRunning) return "running"
  return result?.status ?? "idle"
}
