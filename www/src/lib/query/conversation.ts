import {
  infiniteQueryOptions,
  mutationOptions,
  queryOptions,
} from "@tanstack/react-query"
import type { QueryClient } from "@tanstack/react-query"
import type { ServerConnection } from "../api/server-connection"
import type {
  Conversation,
  ConversationID,
  CreateConversationParams,
  ExecutorSettings,
  ListConversationsParams,
  RecordCursor,
  SubmitInputParams,
  TurnID,
} from "../api/protocol.gen"

// 新对话固定用的 Agent，和 TUI 缺省的 run.agent_name 一致
export const newConversationAgentName = "default"

// ConversationListFilter 是对话列表的搜索词、归档、排序等条件；翻页游标由 queryFn 按页补上
export type ConversationListFilter = Omit<ListConversationsParams, "cursor">

// 对话列表，按 next_cursor 翻页；条件都在 key 里，换条件就是另一份缓存。切回窗口时重取，带上其他客户端的新建、改名、归档和删除
export function conversationListOptions(
  connection: ServerConnection,
  filter: ConversationListFilter
) {
  return infiniteQueryOptions({
    queryKey: [connection.getServerID(), "conversations", filter],
    queryFn: ({ pageParam }) =>
      connection.call("conversation/list", { ...filter, cursor: pageParam }),
    initialPageParam: undefined as RecordCursor<ConversationID> | undefined,
    getNextPageParam: (page) => page.next_cursor,
    refetchOnWindowFocus: true,
  })
}

// 一个对话的记录（标题、Executor 设置等），不打开对话；key 在对话列表之下，列表过期时它也过期
export function conversationOptions(
  connection: ServerConnection,
  conversationID: ConversationID
) {
  return queryOptions({
    queryKey: [connection.getServerID(), "conversations", conversationID],
    queryFn: () =>
      connection.call("conversation/read", { conversation_id: conversationID }),
  })
}

// 新对话提交第一条输入、分叉、归档、删除、改标题、改 Executor 设置之后，所有条件下的对话列表和对话记录都过期
function invalidateConversations(
  queryClient: QueryClient,
  connection: ServerConnection
): void {
  void queryClient.invalidateQueries({
    queryKey: [connection.getServerID(), "conversations"],
  })
}

// 建对话；serve 不列出还没有输入的对话，所以列表等第一条输入提交后才过期
export function createConversationOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (params: CreateConversationParams) =>
      connection.call("conversation/create", params),
  })
}

// 新对话的第一条输入，提交成功后对话出现在列表里
export function submitFirstInputOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (params: SubmitInputParams) =>
      connection.call("conversation/submitInput", params),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConversations(context.client, connection),
  })
}

export function renameConversationOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (variables: {
      conversationID: ConversationID
      title: string
    }) =>
      connection.call("conversation/setTitle", {
        conversation_id: variables.conversationID,
        title: variables.title,
      }),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConversations(context.client, connection),
  })
}

// 分叉带上来源对话的 Executor 设置；beforeTurnID 和 lastTurnID 最多给一个，都不给时复制全部已结束的轮次
export function forkConversationOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (variables: {
      conversation: Conversation
      beforeTurnID?: TurnID
      lastTurnID?: TurnID
    }) =>
      connection.call("conversation/fork", {
        conversation_id: variables.conversation.id,
        before_turn_id: variables.beforeTurnID,
        last_turn_id: variables.lastTurnID,
        executor_settings: variables.conversation.executor_settings,
      }),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConversations(context.client, connection),
  })
}

export function archiveConversationOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (variables: {
      conversationID: ConversationID
      archived: boolean
    }) =>
      connection.call("conversation/setArchived", {
        conversation_id: variables.conversationID,
        archived: variables.archived,
      }),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConversations(context.client, connection),
  })
}

export function deleteConversationOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (conversationID: ConversationID) =>
      connection.call("conversation/delete", {
        conversation_id: conversationID,
      }),
    onSuccess: (_result, _variables, _onMutateResult, context) =>
      invalidateConversations(context.client, connection),
  })
}

// 换对话的 Executor 设置（完整的新设置）；可用 Skill、MCP 连接和 Session 详情跟着 Executor 变，一起过期
export function setExecutorSettingsOptions(connection: ServerConnection) {
  return mutationOptions({
    mutationFn: (variables: {
      conversationID: ConversationID
      executorSettings: ExecutorSettings
    }) =>
      connection.call("conversation/setExecutorSettings", {
        conversation_id: variables.conversationID,
        executor_settings: variables.executorSettings,
      }),
    onSuccess: (_result, _variables, _onMutateResult, context) => {
      invalidateConversations(context.client, connection)
      for (const segment of ["skills", "mcpServers", "sessionDetails"])
        void context.client.invalidateQueries({
          queryKey: [connection.getServerID(), segment],
        })
    },
  })
}
