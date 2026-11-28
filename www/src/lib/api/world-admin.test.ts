import type { ServerConnection } from "./server-connection"
import assert from "node:assert/strict"
import { test } from "node:test"
import {
  cohostedWorldAdmin,
  createWorldJoinToken,
  externalWorldAdmin,
  getWorldAdminNodes,
  getWorldJoinTokenStatus,
  normalizeWorldControlURL,
  revokeWorldResource,
  saveWorldACL,
  worldJoinCommand,
  worldJoinExpiry,
  WorldAdminError,
  type WorldACLRule,
} from "./world-admin"

const connection = externalWorldAdmin(
  "https://world.example/deploy",
  "test-token"
)
const rule: WorldACLRule = {
  priority: 5,
  from: ["owner:alice"],
  to: ["node:office"],
  action: "allow",
}

test("control URLs preserve the deployment prefix and their distinct tokens", () => {
  for (const [input, expected] of [
    [
      " https://world.example/deploy/web/v1/world/admin#token=secret ",
      "https://world.example/deploy",
    ],
    ["http://127.0.0.1:1312/", "http://127.0.0.1:1312"],
    ["https://alice:password@world.example", undefined],
    ["ws://world.example", undefined],
  ] as const)
    assert.equal(normalizeWorldControlURL(input), expected)

  const connection = {
    entry: {
      url: "wss://serve.example/openvaala/api/v1/openvaala",
      token: "serve-token",
    },
    getServerID: () => "serve",
  } as ServerConnection
  assert.deepEqual(cohostedWorldAdmin(connection), {
    controlURL: "https://serve.example/openvaala",
    rpc: connection,
    scope: "serve",
  })
})

test("token state and expiry follow the wire timestamps", () => {
  const now = 1_700_000_000_000
  const token = {
    id: "id",
    owner: "alice",
    tags: [],
    reusable: false,
    used_count: 0,
    expires_at: now + 100,
    revoked_at: 0,
  }
  for (const [changes, state] of [
    [{}, "available"],
    [{ used_count: 1 }, "used"],
    [{ used_count: 1, reusable: true }, "available"],
    [{ expires_at: now, used_count: 1 }, "expired"],
    [{ expires_at: now, revoked_at: now }, "revoked"],
  ] as const)
    assert.equal(getWorldJoinTokenStatus({ ...token, ...changes }, now), state)

  assert.equal(worldJoinExpiry(1.5, now), now + 5_400_000)
  for (const hours of [0, -1, Infinity, NaN])
    assert.equal(worldJoinExpiry(hours, now), undefined)
  assert.equal(
    worldJoinCommand(connection.controlURL + "#jt_secret"),
    "openvaala world join https://world.example/deploy#jt_secret"
  )
})

test("admin HTTP sends JSON and handles 201, empty 204, and empty 401", async (t) => {
  const create = {
    owner: "alice",
    tags: ["ops"],
    reusable: false,
    expires_at: 1_900_000_000_000,
  }
  const cases = [
    {
      endpoint: "tokens",
      method: "POST",
      status: 201,
      body: create,
      run: () => createWorldJoinToken(connection, create),
    },
    {
      endpoint: "tokens/id",
      method: "DELETE",
      status: 204,
      run: () =>
        revokeWorldResource(connection, { resource: "tokens", id: "id" }),
    },
    {
      endpoint: "nodes/id/revoke",
      method: "POST",
      status: 204,
      run: () =>
        revokeWorldResource(connection, { resource: "nodes", id: "id" }),
    },
    {
      endpoint: "owners/alice%20lee/revoke",
      method: "POST",
      status: 204,
      run: () =>
        revokeWorldResource(connection, {
          resource: "owners",
          id: "alice lee",
        }),
    },
    {
      endpoint: "acl",
      method: "PUT",
      status: 204,
      body: [rule],
      run: () => saveWorldACL(connection, [rule]),
    },
    {
      endpoint: "nodes",
      method: "GET",
      status: 401,
      run: () => getWorldAdminNodes(connection, AbortSignal.timeout(5000)),
    },
  ]
  for (const entry of cases) {
    await t.test(entry.method + " " + entry.endpoint, async (t) => {
      const created = {
        ...create,
        id: "id",
        token: "jt_value",
        revoked_at: 0,
        used_count: 0,
      }
      const spy = t.mock.method(
        globalThis,
        "fetch",
        (input: URL, init: RequestInit) => {
          assert.equal(input.href, connection.apiURL + entry.endpoint)
          assert.equal(init.method ?? "GET", entry.method)
          const headers = new Headers(init.headers)
          assert.equal(headers.get("Authorization"), "Bearer test-token")
          if (entry.body) {
            assert.equal(headers.get("Content-Type"), "application/json")
            assert.deepEqual(JSON.parse(String(init.body)), entry.body)
          }
          return Promise.resolve(
            new Response(
              entry.status === 201 ? JSON.stringify(created) : null,
              { status: entry.status }
            )
          )
        }
      )
      if (entry.status === 401) {
        await assert.rejects(
          entry.run,
          (err) => err instanceof WorldAdminError && err.status === 401
        )
      } else {
        assert.deepEqual(
          await entry.run(),
          entry.status === 201 ? created : undefined
        )
      }
      assert.equal(spy.mock.callCount(), 1)
    })
  }
})
