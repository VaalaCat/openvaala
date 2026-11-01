import type {
  AgentItemDelta,
  Content,
  Item,
  ModelCallID,
  Ordinal,
  TurnID,
  TurnStatus,
} from "./protocol.gen"

// ViewItem 是消息区的一条：已保存的 Item（带 ordinal）或生成中的 Item（带 itemIndex），按首次出现顺序
export interface ViewItem {
  // 列表 key：Item ID，生成中的换成已保存的之后不变
  key: string
  item: Item
  // 已保存 Item 的日志流水号；还没保存时为空
  ordinal?: Ordinal
  turnID: TurnID
  // 生成中 Item 在本轮输出里的位置，通知按 turn_id + item_index 定位它；已保存的 Item 没有
  itemIndex?: number
  // 是否还在生成？item/completed、item/interrupted 之后为 false
  isStreaming: boolean
}

// TurnView 是推送里看到的一轮执行：turn/started 记为 running，turn/finished 写入终态、最终调用和错误
export interface TurnView {
  // 终态没有保存时为空
  status?: TurnStatus
  finalModelCallID?: ModelCallID
  error?: string
}

// 未保存的 Item 里位于 turnID + itemIndex 的那一条，没有时返回 -1
export function findStreamingItemIndex(
  items: ViewItem[],
  turnID: TurnID,
  itemIndex: number
): number {
  return items.findLastIndex(
    (view) =>
      view.ordinal === undefined &&
      view.turnID === turnID &&
      view.itemIndex === itemIndex
  )
}

// 把一段增量接到 Item 上，返回新的 Item：消息文本接到第一个内容块，推理片段接到摘要和内容末尾，
// 参数接到工具参数末尾；同 pkg/agent 的 ApplyItemDelta
export function applyItemDelta(item: Item, delta: AgentItemDelta): Item {
  switch (item.kind) {
    case "message": {
      const [first, ...rest] = item.message?.content ?? []
      if (!item.message || !first?.text) return item
      const text = { ...first.text, text: first.text.text + (delta.text ?? "") }
      return {
        ...item,
        message: { ...item.message, content: [{ ...first, text }, ...rest] },
      }
    }
    case "reasoning": {
      if (!item.reasoning) return item
      const { summary = [], content = [] } = item.reasoning
      return {
        ...item,
        reasoning: {
          ...item.reasoning,
          summary: [...summary, ...(delta.summary ?? [])],
          content: [...content, ...(delta.content ?? [])],
        },
      }
    }
    case "tool_call": {
      if (!item.tool_call) return item
      const args = item.tool_call.arguments + (delta.arguments ?? "")
      return { ...item, tool_call: { ...item.tool_call, arguments: args } }
    }
    default:
      return item
  }
}

// 每个 call_id 对应的工具结果：tool_call 和 tool_result 按它合成一张工具卡
export function getToolResultsByCallID(
  items: ViewItem[]
): Map<string, ViewItem> {
  const results = new Map<string, ViewItem>()
  for (const view of items) {
    const callID = view.item.tool_result?.call_id
    if (callID) results.set(callID, view)
  }
  return results
}

// 每一轮已保存 Item 的耗时：最早到最晚 updated_at 的毫秒差，没有保存时间的轮次不在结果里；
// 同 internal/tui/step_group.go 的 getSavedDuration
export function getTurnSavedDurations(items: ViewItem[]): Map<TurnID, number> {
  // 1. 一次遍历记下每轮最早和最晚的保存时间。
  const savedRanges = new Map<TurnID, { first: number; last: number }>()
  for (const view of items) {
    const savedAt = view.item.updated_at
    if (view.ordinal === undefined || !savedAt) continue
    const range = savedRanges.get(view.turnID)
    if (!range) savedRanges.set(view.turnID, { first: savedAt, last: savedAt })
    else {
      range.first = Math.min(range.first, savedAt)
      range.last = Math.max(range.last, savedAt)
    }
  }
  // 2. 换算成毫秒差。
  return new Map(
    [...savedRanges].map(([turnID, { first, last }]) => [turnID, last - first])
  )
}

// 输入内容里的文字，多段按换行拼接，图片不计
export function getContentText(content: Content[]): string {
  return content
    .flatMap((each) => (each.text ? [each.text.text] : []))
    .join("\n")
}
