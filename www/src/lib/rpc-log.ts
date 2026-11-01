import type { RpcFrame } from "./api/server-connection"

// RpcLogRow 是 RPC 记录面板里的一行
export interface RpcLogRow {
  frame: RpcFrame
  // 响应距离对应请求发出的毫秒数；请求、通知和找不到请求的响应没有
  latencyMs?: number
  // 帧所属的对话：请求和通知取 params.conversation_id，响应没有 params，跟随对应的请求
  conversationID?: string
}

// RpcLogFilter 是面板的筛选条件，没给的条件不筛
export interface RpcLogFilter {
  // 方法名包含这段文字，不区分大小写
  method?: string
  direction?: RpcFrame["direction"]
  conversationID?: string
}

// 按请求编号给响应配上请求：算出耗时，并让响应跟随请求所属的对话
export function pairRpcFrames(frames: RpcFrame[]): RpcLogRow[] {
  const requests = new Map<number, RpcLogRow>()
  return frames.map((frame) => {
    // 1. 发出的请求记下编号，供之后的响应查找。
    if (frame.direction === "sent") {
      const row = { frame, conversationID: getFrameConversationID(frame) }
      if (frame.id !== undefined) requests.set(frame.id, row)
      return row
    }
    // 2. 收到的帧有编号的是响应，按编号找请求；没有编号的是通知。
    const request = frame.id === undefined ? undefined : requests.get(frame.id)
    if (!request)
      return { frame, conversationID: getFrameConversationID(frame) }
    return {
      frame,
      latencyMs: frame.time - request.frame.time,
      conversationID: request.conversationID,
    }
  })
}

// 留下同时满足全部条件的行，顺序不变
export function filterRpcRows(
  rows: RpcLogRow[],
  filter: RpcLogFilter
): RpcLogRow[] {
  const method = filter.method?.trim().toLowerCase()
  return rows.filter(
    (row) =>
      (!method || !!row.frame.method?.toLowerCase().includes(method)) &&
      (!filter.direction || row.frame.direction === filter.direction) &&
      (!filter.conversationID || row.conversationID === filter.conversationID)
  )
}

// 把帧写成 JSONL：每帧一行，末尾换行；api_key 已遮罩
export function formatRpcLogJsonl(frames: RpcFrame[]): string {
  return frames
    .map((frame) => JSON.stringify(frame, maskSecretField) + "\n")
    .join("")
}

// 供面板显示的帧副本：任意层级里非空的 api_key 换成遮罩，原帧不变
export function maskRpcFrame(frame: RpcFrame): RpcFrame {
  return JSON.parse(JSON.stringify(frame, maskSecretField)) as RpcFrame
}

const maskedSecret = "••••••"

// JSON.stringify 的 replacer：名为 api_key 的非空字符串换成遮罩，其余原样
function maskSecretField(key: string, value: unknown): unknown {
  return key === "api_key" && typeof value === "string" && value !== ""
    ? maskedSecret
    : value
}

// 帧参数里的 conversation_id；没有参数或参数里没有时为空
function getFrameConversationID(frame: RpcFrame): string | undefined {
  const params = (frame.payload as { params?: unknown } | null)?.params
  const conversationID = (params as { conversation_id?: unknown } | null)
    ?.conversation_id
  return typeof conversationID === "string" ? conversationID : undefined
}
