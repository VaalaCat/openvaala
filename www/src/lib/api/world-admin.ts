import type { ServerConnection } from "./server-connection"
import type {
  ACLRule,
  ACLMatrix,
  ListUsersResult,
  CreateJoinTokenRequest,
  ListDomainsResult,
  PeerDomain,
} from "./protocol.gen"

export type ExternalWorldAdminConnection = {
  controlURL: string
  apiURL: string
  token: string
  scope: string
}

export type WorldAdminConnection =
  | ExternalWorldAdminConnection
  | {
      rpc: ServerConnection
      controlURL: string
      scope: string
    }

export type WorldAdminNode = {
  node_id: string
  node_name: string
  domain: string
  owner: string
  tags: string[] | null
  online: boolean
  targets: { name: string; description?: string; visibility: string }[]
  revoked_at: number
  created_at: number
}

export type WorldJoinToken = {
  id: string
  owner: string
  tags: string[] | null
  reusable: boolean
  used_count: number
  expires_at: number
  revoked_at: number
}

export type CreatedWorldJoinToken = WorldJoinToken & { token: string }

export type WorldACLRule = ACLRule

export class WorldAdminError extends Error {
  readonly status: number
  readonly code?: string

  constructor(status: number, message: string, code?: string) {
    super(message)
    this.code = code
    this.status = status
  }
}

export function normalizeWorldControlURL(value: string): string | undefined {
  try {
    const url = new URL(value.trim())
    if (
      (url.protocol !== "http:" && url.protocol !== "https:") ||
      url.username ||
      url.password
    )
      return
    url.pathname = url.pathname
      .replace(/\/+$/, "")
      .replace(/\/web\/v1(?:\/world\/admin)?$/, "")
    url.search = ""
    url.hash = ""
    return url.href.replace(/\/$/, "")
  } catch {
    return
  }
}

export function externalWorldAdmin(
  controlURL: string,
  token: string
): ExternalWorldAdminConnection {
  return {
    controlURL,
    token,
    apiURL: controlURL + "/world/v1/admin/",
    scope: controlURL,
  }
}

export function cohostedWorldAdmin(
  connection: ServerConnection
): WorldAdminConnection {
  const address = connection.entry.url
  const direct = connection.entry.worldTarget
    ? connection.entry.worldTarget.entries.direct_urls?.[0]
    : address
  const controlURL = direct
    ? normalizeWorldControlURL(
        direct
          .replace(/^ws/, "http")
          .replace(/\/(api\/v1\/openvaala|world\/v1\/direct)\/?$/, "")
      )
    : undefined
  return {
    rpc: connection,
    controlURL: controlURL ?? "",
    scope: connection.getServerID(),
  }
}

async function request(
  connection: ExternalWorldAdminConnection,
  path: string,
  expected: number | number[],
  init: RequestInit
) {
  const headers = new Headers({ Authorization: "Bearer " + connection.token })
  if (init.body) headers.set("Content-Type", "application/json")
  const response = await fetch(new URL(path, connection.apiURL), {
    ...init,
    headers,
    cache: "no-store",
    redirect: "error",
  })
  if (
    Array.isArray(expected)
      ? expected.includes(response.status)
      : response.status === expected
  )
    return response
  const raw = response.status === 401 ? "" : await response.text()
  let detail = raw
  let code: string | undefined
  try {
    const body = JSON.parse(raw)
    detail = body.error
    code = body.code
  } catch {
    /* HTTP 错误也可以是纯文本。 */
  }
  throw new WorldAdminError(
    response.status,
    detail || "HTTP " + response.status,
    code
  )
}

export async function getWorldAdminNodes(
  connection: WorldAdminConnection,
  signal: AbortSignal
): Promise<WorldAdminNode[]> {
  if ("rpc" in connection)
    return connection.rpc.call("world/admin/listNodes", {})
  return (await request(connection, "nodes", 200, { signal })).json()
}

export async function getWorldJoinTokens(
  connection: WorldAdminConnection,
  signal: AbortSignal
): Promise<WorldJoinToken[]> {
  if ("rpc" in connection)
    return connection.rpc.call("world/admin/listTokens", {})
  return (await request(connection, "tokens", 200, { signal })).json()
}

export async function createWorldJoinToken(
  connection: WorldAdminConnection,
  input: CreateJoinTokenRequest
): Promise<CreatedWorldJoinToken> {
  if ("rpc" in connection)
    return connection.rpc.call("world/admin/createToken", input)
  return (
    await request(connection, "tokens", 201, {
      method: "POST",
      body: JSON.stringify(input),
    })
  ).json()
}

type WorldRevocation = { resource: "nodes" | "owners" | "tokens"; id: string }

export async function revokeWorldResource(
  connection: WorldAdminConnection,
  { resource, id }: WorldRevocation
) {
  if ("rpc" in connection) {
    switch (resource) {
      case "nodes":
        await connection.rpc.call("world/admin/revokeNode", { node_id: id })
        return
      case "owners":
        await connection.rpc.call("world/admin/revokeOwner", { owner: id })
        return
      case "tokens":
        await connection.rpc.call("world/admin/revokeToken", { id })
        return
    }
  }
  const path = resource + "/" + encodeURIComponent(id)
  await request(
    connection,
    resource === "tokens" ? path : path + "/revoke",
    204,
    {
      method: resource === "tokens" ? "DELETE" : "POST",
    }
  )
}

export async function getWorldACL(
  connection: WorldAdminConnection,
  signal: AbortSignal
): Promise<WorldACLRule[]> {
  if ("rpc" in connection) return connection.rpc.call("world/admin/getACL", {})
  return (await request(connection, "acl", 200, { signal })).json()
}

export async function saveWorldACL(
  connection: WorldAdminConnection,
  rules: WorldACLRule[]
) {
  if ("rpc" in connection) {
    await connection.rpc.call("world/admin/replaceACL", rules)
    return
  }
  await request(connection, "acl", 204, {
    method: "PUT",
    body: JSON.stringify(rules),
  })
}

export function getWorldJoinTokenStatus(
  token: WorldJoinToken,
  now = Date.now()
) {
  if (token.revoked_at) return "revoked"
  if (token.expires_at <= now) return "expired"
  if (!token.reusable && token.used_count > 0) return "used"
  return "available"
}

export function worldJoinExpiry(hours: number, now = Date.now()) {
  const expiry = now + Math.round(hours * 3_600_000)
  return Number.isSafeInteger(expiry) && expiry > now ? expiry : undefined
}

export function worldJoinCommand(token: string) {
  const shellWord = (value: string) =>
    /^[A-Za-z0-9_.:/#-]+$/.test(value)
      ? value
      : "'" + value.replaceAll("'", "'\\''") + "'"
  return "openvaala world join " + shellWord(token)
}

export async function getWorldUsers(
  connection: WorldAdminConnection,
  signal: AbortSignal
) {
  const result: ListUsersResult =
    "rpc" in connection
      ? await connection.rpc.call("world/admin/listUsers", {})
      : await (await request(connection, "users", 200, { signal })).json()
  return result.users
}

export async function getWorldACLMatrix(
  connection: WorldAdminConnection,
  signal: AbortSignal,
  rules?: ACLRule[]
): Promise<ACLMatrix> {
  const params = { rules }
  if ("rpc" in connection)
    return connection.rpc.call("world/admin/getACLMatrix", params)
  return (
    await request(connection, "acl/matrix", 200, {
      method: "POST",
      body: JSON.stringify(params),
      signal,
    })
  ).json()
}

export async function getWorldDomains(
  connection: WorldAdminConnection,
  signal: AbortSignal
): Promise<ListDomainsResult> {
  if ("rpc" in connection)
    return connection.rpc.call("world/admin/listDomains", {})
  return (await request(connection, "domains", 200, { signal })).json()
}

export async function addWorldDomain(
  connection: WorldAdminConnection,
  card: string
) {
  if ("rpc" in connection)
    return connection.rpc.call("world/admin/addDomain", { card })
  return (
    await request(connection, "domains", [200, 201], {
      method: "POST",
      body: JSON.stringify({ card }),
    })
  ).json() as Promise<PeerDomain>
}

export async function decideWorldDomain(
  connection: WorldAdminConnection,
  domain: string,
  accept: boolean
) {
  if ("rpc" in connection)
    return connection.rpc.call(
      accept ? "world/admin/acceptDomain" : "world/admin/rejectDomain",
      { domain }
    )
  await request(
    connection,
    "domains/" + encodeURIComponent(domain) + (accept ? "/accept" : "/reject"),
    204,
    { method: "POST" }
  )
}

export async function deleteWorldDomain(
  connection: WorldAdminConnection,
  domain: string
) {
  if ("rpc" in connection)
    return connection.rpc.call("world/admin/deleteDomain", { domain })
  await request(connection, "domains/" + encodeURIComponent(domain), 204, {
    method: "DELETE",
  })
}
