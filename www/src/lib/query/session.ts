import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type {
  ConversationID,
  RecordCursor,
  SessionID,
  TurnID,
} from "../api/protocol.gen"

// Session 的 Turn 列表，最新的一轮在前，按 next_cursor 向更早的轮次翻页
export function turnListOptions(
  connection: ServerConnection,
  conversationID: ConversationID,
  sessionID: SessionID
) {
  return infiniteQueryOptions({
    queryKey: [connection.getServerID(), "turns", conversationID, sessionID],
    queryFn: ({ pageParam }) =>
      connection.call("session/turns/list", {
        conversation_id: conversationID,
        session_id: sessionID,
        order: { by: "key", direction: "desc" },
        cursor: pageParam,
      }),
    initialPageParam: undefined as RecordCursor<TurnID> | undefined,
    getNextPageParam: (page) => page.next_cursor,
  })
}

// 按 ID 读一批 Turn 的完整记录（状态、用量、模型），消息区的 Turn 分隔行用；
// key 在 Turn 列表之下，一轮结束时和 Turn 列表一起失效
export function turnsOptions(
  connection: ServerConnection,
  conversationID: ConversationID,
  sessionID: SessionID,
  turnIDs: TurnID[]
) {
  return queryOptions({
    queryKey: [
      connection.getServerID(),
      "turns",
      conversationID,
      sessionID,
      { turnIDs },
    ],
    queryFn: () =>
      connection.call("session/turns/get", {
        conversation_id: conversationID,
        session_id: sessionID,
        turn_ids: turnIDs,
      }),
    enabled: turnIDs.length > 0,
  })
}

// Session 当前的上下文用量
export function contextUsageOptions(
  connection: ServerConnection,
  conversationID: ConversationID,
  sessionID: SessionID
) {
  return queryOptions({
    queryKey: [
      connection.getServerID(),
      "contextUsage",
      conversationID,
      sessionID,
    ],
    queryFn: () =>
      connection.call("session/contextUsage", {
        conversation_id: conversationID,
        session_id: sessionID,
      }),
  })
}

// Session 的完整运行详情，调试用
export function sessionDetailsOptions(
  connection: ServerConnection,
  conversationID: ConversationID,
  sessionID: SessionID
) {
  return queryOptions({
    queryKey: [
      connection.getServerID(),
      "sessionDetails",
      conversationID,
      sessionID,
    ],
    queryFn: () =>
      connection.call("session/details", {
        conversation_id: conversationID,
        session_id: sessionID,
      }),
  })
}
