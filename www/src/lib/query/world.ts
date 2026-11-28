import { createContext, useContext, useEffect } from "react"
import {
  infiniteQueryOptions,
  queryOptions,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query"
import type { RecordCursor } from "../api/protocol.gen"
import type { ServerConnection } from "../api/server-connection"
import {
  getWorldMessages,
  getWorldPeers,
  getWorldThreads,
  listenWorldUpdates,
  type WorldMessagesFilter,
  type WorldStatus,
} from "../api/world"
import { getWorldQueryKey, invalidateWorldQueries } from "./invalidation"

type WorldStatusState = {
  status: WorldStatus | undefined
  isPending: boolean
  isError: boolean
  error: Error | null
  refetch: () => Promise<unknown>
}

export const WorldStatusContext = createContext<WorldStatusState | undefined>(
  undefined
)

export function useWorldStatus() {
  return useWorldStatusState()?.status
}

export function useWorldStatusState() {
  return useContext(WorldStatusContext)
}

export function worldStatusOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: getWorldQueryKey(connection, "status"),
    queryFn: () => connection.call("world/getStatus", {}),
  })
}

export function worldThreadsOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: [...getWorldQueryKey(connection, "threads"), "page"],
    queryFn: async () =>
      (
        await getWorldThreads(connection, undefined, {
          limit: 200,
        })
      ).items,
  })
}

export function worldThreadsInfiniteOptions(
  connection: ServerConnection,
  { limit }: { limit?: number } = {}
) {
  return infiniteQueryOptions({
    queryKey: [...getWorldQueryKey(connection, "threads"), "infinite", limit],
    initialPageParam: undefined as RecordCursor<string> | undefined,
    queryFn: ({ pageParam }) =>
      getWorldThreads(connection, undefined, {
        limit,
        cursor: pageParam,
      }),
    getNextPageParam: (last) => last.next_cursor,
  })
}

export function worldPeersOptions(connection: ServerConnection) {
  return queryOptions({
    queryKey: getWorldQueryKey(connection, "peers"),
    queryFn: () => getWorldPeers(connection),
  })
}

export function worldMessagesOptions(
  connection: ServerConnection,
  filter: WorldMessagesFilter
) {
  return queryOptions({
    queryKey: [...getWorldQueryKey(connection, "messages"), "page", filter],
    queryFn: async () =>
      (await getWorldMessages(connection, filter, { limit: 200 })).items,
  })
}

export function worldMessagesInfiniteOptions(
  connection: ServerConnection,
  filter: WorldMessagesFilter & { limit?: number }
) {
  return infiniteQueryOptions({
    queryKey: [...getWorldQueryKey(connection, "messages"), "infinite", filter],
    initialPageParam: undefined as RecordCursor<string> | undefined,
    queryFn: ({ pageParam }) =>
      getWorldMessages(connection, filter, {
        limit: filter.limit,
        cursor: pageParam,
      }),
    getNextPageParam: (last) => last.next_cursor,
  })
}

// AppLayout 为选中服务器持有唯一的状态查询与事件流。
export function useWorldConnection(
  connection: ServerConnection
): WorldStatusState {
  const queryClient = useQueryClient()
  const {
    data: status,
    isPending,
    isError,
    error,
    refetch,
  } = useQuery(worldStatusOptions(connection))
  const enabled = status?.enabled === true
  useEffect(() => {
    if (!enabled) return

    return listenWorldUpdates(
      connection,
      () => invalidateWorldQueries(queryClient, connection),
      (event) => invalidateWorldQueries(queryClient, connection, event)
    )
  }, [connection, enabled, queryClient])

  return { status, isPending, isError, error, refetch }
}
