import { queryOptions } from "@tanstack/react-query"
import {
  getWorldUsers,
  getWorldACLMatrix,
  type WorldAdminConnection,
  type WorldACLRule,
} from "@/lib/api/world-admin"

export function adminQueryKey(
  connection: WorldAdminConnection,
  resource?: string
) {
  const key = [connection.scope, "world-admin"]
  return resource === undefined ? key : [...key, resource]
}

export function worldAdminUsersOptions(connection: WorldAdminConnection) {
  return queryOptions({
    queryKey: adminQueryKey(connection, "users"),
    queryFn: ({ signal }) => getWorldUsers(connection, signal),
  })
}

export function worldMatrixOptions(
  connection: WorldAdminConnection,
  rules?: WorldACLRule[]
) {
  return queryOptions({
    queryKey: [...adminQueryKey(connection, "matrix"), rules ?? null],
    queryFn: ({ signal }) => getWorldACLMatrix(connection, signal, rules),
  })
}
