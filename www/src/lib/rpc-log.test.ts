import assert from "node:assert/strict"
import { test } from "node:test"
import type { RpcFrame } from "./api/server-connection"
import {
  filterRpcRows,
  formatRpcLogJsonl,
  maskRpcFrame,
  pairRpcFrames,
  type RpcLogFilter,
} from "./rpc-log"

const detailsRequest: RpcFrame = {
  direction: "sent",
  method: "session/details",
  id: 3,
  time: 100,
  size: 80,
  payload: {
    id: 3,
    method: "session/details",
    params: { conversation_id: "c1", session_id: "s1" },
  },
}
const detailsResponse: RpcFrame = {
  direction: "received",
  method: "session/details",
  id: 3,
  time: 160,
  size: 400,
  payload: { id: 3, result: {} },
}
const otherRequest: RpcFrame = {
  direction: "sent",
  method: "conversation/open",
  id: 4,
  time: 170,
  size: 60,
  payload: {
    id: 4,
    method: "conversation/open",
    params: { conversation_id: "c2" },
  },
}
const otherResponse: RpcFrame = {
  direction: "received",
  method: "conversation/open",
  id: 4,
  time: 175,
  size: 300,
  payload: { id: 4, result: {} },
}
const statusNotification: RpcFrame = {
  direction: "received",
  method: "session/statusUpdated",
  time: 180,
  size: 200,
  payload: {
    method: "session/statusUpdated",
    params: { conversation_id: "c1", subscription: "conversation" },
  },
}
const initializeRequest: RpcFrame = {
  direction: "sent",
  method: "initialize",
  id: 1,
  time: 10,
  size: 90,
  payload: { id: 1, method: "initialize", params: {} },
}
const frames = [
  initializeRequest,
  detailsRequest,
  detailsResponse,
  otherRequest,
  otherResponse,
  statusNotification,
]

test("pairRpcFrames measures response latency against its request", () => {
  const rows = pairRpcFrames(frames)
  const cases: { name: string; frame: RpcFrame; want?: number }[] = [
    { name: "response", frame: detailsResponse, want: 60 },
    { name: "request", frame: detailsRequest, want: undefined },
    { name: "notification", frame: statusNotification, want: undefined },
  ]
  for (const { name, frame, want } of cases)
    assert.equal(rows.find((row) => row.frame === frame)?.latencyMs, want, name)
})

test("filterRpcRows keeps rows matching every condition", () => {
  const rows = pairRpcFrames(frames)
  const cases: { name: string; filter: RpcLogFilter; want: RpcFrame[] }[] = [
    {
      name: "conversation_follows_request",
      filter: { conversationID: "c1" },
      want: [detailsRequest, detailsResponse, statusNotification],
    },
    {
      name: "method_substring_ignores_case",
      filter: { method: "SESSION/" },
      want: [detailsRequest, detailsResponse, statusNotification],
    },
    {
      name: "direction",
      filter: { direction: "sent" },
      want: [initializeRequest, detailsRequest, otherRequest],
    },
    {
      name: "combined",
      filter: { conversationID: "c1", direction: "received", method: "status" },
      want: [statusNotification],
    },
    { name: "empty", filter: {}, want: frames },
  ]
  for (const { name, filter, want } of cases)
    assert.deepEqual(
      filterRpcRows(rows, filter).map((row) => row.frame),
      want,
      name
    )
})

test("formatRpcLogJsonl writes one frame per line", () => {
  const lines = formatRpcLogJsonl([detailsRequest, detailsResponse])
    .trimEnd()
    .split("\n")
  assert.deepEqual(
    lines.map((line) => JSON.parse(line) as unknown),
    [detailsRequest, detailsResponse]
  )
})

test("maskRpcFrame masks non-empty api_key at any depth", () => {
  const frame = (payload: unknown): RpcFrame => ({
    direction: "sent",
    method: "provider/set",
    id: 7,
    time: 200,
    size: 120,
    payload,
  })
  const cases: { name: string; payload: unknown; want: unknown }[] = [
    {
      name: "request_params",
      payload: {
        id: 7,
        method: "provider/set",
        params: { provider: { id: "p1", api_key: "sk-secret" } },
      },
      want: {
        id: 7,
        method: "provider/set",
        params: { provider: { id: "p1", api_key: "••••••" } },
      },
    },
    {
      name: "response_array",
      payload: {
        id: 7,
        result: {
          providers: [
            { id: "p1", api_key: "sk-one" },
            { id: "p2", api_key: "sk-two" },
          ],
        },
      },
      want: {
        id: 7,
        result: {
          providers: [
            { id: "p1", api_key: "••••••" },
            { id: "p2", api_key: "••••••" },
          ],
        },
      },
    },
    {
      name: "empty_key_kept",
      payload: { params: { provider: { id: "p1", api_key: "" } } },
      want: { params: { provider: { id: "p1", api_key: "" } } },
    },
    {
      name: "other_fields_unchanged",
      payload: { params: { conversation_id: "c1", key: "api_key" } },
      want: { params: { conversation_id: "c1", key: "api_key" } },
    },
  ]
  for (const { name, payload, want } of cases) {
    const original = frame(payload)
    assert.deepEqual(maskRpcFrame(original), frame(want), name)
    assert.deepEqual(
      JSON.parse(formatRpcLogJsonl([original])) as unknown,
      frame(want),
      name + "_jsonl"
    )
  }
  assert.equal(
    formatRpcLogJsonl(cases.map(({ payload }) => frame(payload))).match(
      /sk-(secret|one|two)/
    ),
    null
  )
})
