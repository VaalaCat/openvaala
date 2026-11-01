import assert from "node:assert/strict"
import { test } from "node:test"
import type { TFunction } from "i18next"
import {
  getRpcErrorDetail,
  getRpcErrorMessage,
  isConnectionLostError,
  isNotFoundError,
  RpcError,
  rpcConnectionLostCode,
  shouldShowLoadError,
} from "./rpc-error"

// 返回 key 本身，方便断言用了哪条文案
const t = ((key: string) => key) as unknown as TFunction

test("RpcError messages follow the error code table", () => {
  const cases: {
    name: string
    error: unknown
    wantMessage: string
    wantDetail: string | undefined
    wantNotFound: boolean
  }[] = [
    {
      name: "connection_lost",
      error: new RpcError(rpcConnectionLostCode, "connection lost", "m"),
      wantMessage: "common.errors.connectionLost",
      wantDetail: undefined,
      wantNotFound: false,
    },
    {
      name: "session_busy",
      error: new RpcError(-32001, "session is running", "m"),
      wantMessage: "common.errors.sessionBusy",
      wantDetail: "session is running",
      wantNotFound: false,
    },
    {
      name: "conversation_not_found",
      error: new RpcError(-32002, "no such conversation", "m"),
      wantMessage: "common.errors.conversationNotFound",
      wantDetail: "no such conversation",
      wantNotFound: true,
    },
    {
      name: "session_not_found",
      error: new RpcError(-32003, "no such session", "m"),
      wantMessage: "common.errors.sessionNotFound",
      wantDetail: "no such session",
      wantNotFound: true,
    },
    {
      name: "invalid_params",
      error: new RpcError(-32602, "bad field", "m"),
      wantMessage: "common.errors.invalidParams",
      wantDetail: "bad field",
      wantNotFound: false,
    },
    {
      name: "other_code_shows_original_text",
      error: new RpcError(-32000, "model failed", "m"),
      wantMessage: "model failed",
      wantDetail: undefined,
      wantNotFound: false,
    },
    {
      name: "plain_error",
      error: new Error("boom"),
      wantMessage: "boom",
      wantDetail: undefined,
      wantNotFound: false,
    },
    {
      name: "not_an_error",
      error: "oops",
      wantMessage: "common.errors.requestFailed",
      wantDetail: undefined,
      wantNotFound: false,
    },
  ]
  for (const { name, error, wantMessage, wantDetail, wantNotFound } of cases) {
    assert.equal(getRpcErrorMessage(error, t), wantMessage, name)
    assert.equal(getRpcErrorDetail(error), wantDetail, name)
    assert.equal(isNotFoundError(error), wantNotFound, name)
  }
})

test("only client connection errors are hidden from read-only loading", () => {
  const cases = [
    {
      name: "connection error uses its code rather than its text",
      error: new RpcError(rpcConnectionLostCode, "socket closed", "model/list"),
      connectionLost: true,
    },
    {
      name: "business error with matching text remains visible",
      error: new RpcError(-32000, "connection lost", "model/list"),
      connectionLost: false,
    },
    {
      name: "plain error with matching text remains visible",
      error: new Error("connection lost"),
      connectionLost: false,
    },
    {
      name: "object with matching code is not an RPC error",
      error: { code: rpcConnectionLostCode, message: "connection lost" },
      connectionLost: false,
    },
    {
      name: "not found retains its original error treatment",
      error: new RpcError(-32002, "not found", "conversation/get"),
      connectionLost: false,
    },
    { name: "no error", error: undefined, connectionLost: false },
  ]
  for (const { name, error, connectionLost } of cases) {
    assert.equal(isConnectionLostError(error), connectionLost, name)
    assert.equal(shouldShowLoadError(error), !connectionLost, name)
  }
})
