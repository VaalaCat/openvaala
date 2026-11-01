import type { ParseKeys, TFunction } from "i18next"
import {
  ErrorCodeConversationNotFound,
  ErrorCodeInvalidParams,
  ErrorCodeSessionBusy,
  ErrorCodeSessionNotFound,
} from "./api/protocol.gen"

// 客户端自己产生的错误码：连接已断开，请求没有得到回复。serve 的错误码都是 -32xxx
export const rpcConnectionLostCode = -1

// 一次 JSON-RPC 请求失败：code 是 serve 返回的错误码或 rpcConnectionLostCode，method 是出错的请求方法
export class RpcError extends Error {
  code: number
  method: string
  constructor(code: number, message: string, method: string) {
    super(message)
    this.name = "RpcError"
    this.code = code
    this.method = method
  }
}

// 请求是否因客户端连接断开而没有得到回复？
export function isConnectionLostError(error: unknown): boolean {
  // 1. 只识别客户端连接错误码。
  return error instanceof RpcError && error.code === rpcConnectionLostCode
}

// 只读加载错误是否需要在数据区块展示？
export function shouldShowLoadError(error: unknown): boolean {
  // 1. 连接错误由服务器入口展示。
  return !isConnectionLostError(error)
}

// 有专门文案的错误码；其余错误直接显示 serve 返回的原文
const codeMessageKeys: Record<number, ParseKeys> = {
  [rpcConnectionLostCode]: "common.errors.connectionLost",
  [ErrorCodeSessionBusy]: "common.errors.sessionBusy",
  [ErrorCodeConversationNotFound]: "common.errors.conversationNotFound",
  [ErrorCodeSessionNotFound]: "common.errors.sessionNotFound",
  [ErrorCodeInvalidParams]: "common.errors.invalidParams",
}

function getCodeMessageKey(error: unknown): ParseKeys | undefined {
  return error instanceof RpcError ? codeMessageKeys[error.code] : undefined
}

export function getRpcErrorMessage(error: unknown, t: TFunction): string {
  const key = getCodeMessageKey(error)
  if (key) return t(key)
  return error instanceof Error
    ? error.message
    : t("common.errors.requestFailed")
}

// 标题被错误码文案替换时，serve 的原文放在标题下方；连接断开的原文是客户端自己写的，不重复显示
export function getRpcErrorDetail(error: unknown): string | undefined {
  if (!(error instanceof RpcError) || error.code === rpcConnectionLostCode)
    return undefined
  return getCodeMessageKey(error) ? error.message : undefined
}

// 请求的对话或 Session 不存在
export function isNotFoundError(error: unknown): boolean {
  return (
    error instanceof RpcError &&
    (error.code === ErrorCodeConversationNotFound ||
      error.code === ErrorCodeSessionNotFound)
  )
}
