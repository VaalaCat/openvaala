import { createContext, useContext, type ComponentType } from "react"
import type { ParseKeys, TFunction } from "i18next"
import { AgentsInstructionItem } from "../components/conversation/items/agents-instruction"
import { ClientInstructionItem } from "../components/conversation/items/client-instruction"
import { ContextSummaryItem } from "../components/conversation/items/context-summary"
import { ExecutorsChangeItem } from "../components/conversation/items/executors-change"
import { FallbackItem } from "../components/conversation/items/fallback"
import { HookRunItem } from "../components/conversation/items/hook-run"
import { MemoryIndexItem } from "../components/conversation/items/memory-index"
import { MessageItem } from "../components/conversation/items/message"
import { ModelChangeItem } from "../components/conversation/items/model-change"
import { ModelErrorItem } from "../components/conversation/items/model-error"
import { ReasoningItem } from "../components/conversation/items/reasoning"
import { SessionForkItem } from "../components/conversation/items/session-fork"
import { SkillInstructionItem } from "../components/conversation/items/skill-instruction"
import { ToolCallItem } from "../components/conversation/items/tool-call"
import { ToolResultItem } from "../components/conversation/items/tool-result"
import { TurnInterruptedItem } from "../components/conversation/items/turn-interrupted"
import { ApplyPatchTool } from "../components/conversation/tools/apply-patch"
import {
  BashTool,
  ListCommandsTool,
  ReadCommandOutputTool,
  StopCommandTool,
  WriteStdinTool,
} from "../components/conversation/tools/bash"
import {
  InterruptAgentTool,
  ListAgentsTool,
  ReadSessionHistoryTool,
  SendMessageTool,
  SpawnAgentTool,
  WaitAgentTool,
} from "../components/conversation/tools/collaboration"
import { DefaultTool } from "../components/conversation/tools/default"
import { ReadFileTool } from "../components/conversation/tools/read-file"
import {
  getContentText,
  getToolResultsByCallID,
  type TurnView,
  type ViewItem,
} from "./api/conversation-items"
import type { ConversationSync } from "./api/conversation-sync"
import type {
  Item,
  ItemKind,
  ToolCall,
  ToolResult,
  Turn,
  TurnID,
} from "./api/protocol.gen"
import {
  getStringField,
  getToolArgumentPreview,
  isCommandFailed,
  parseCommandState,
  parseToolArguments,
  type ParsedToolArguments,
} from "./tool-arguments"

// Item 展开选择由消息区按 ItemID 保存，内容组件直接读取同一份选择。
export const ItemDisclosureContext = createContext<
  | {
      open: boolean
      isChosen: boolean
      onOpenChange: (open: boolean) => void
      onInspect?: () => void
    }
  | undefined
>(undefined)

export function useItemDisclosure() {
  return useContext(ItemDisclosureContext)
}

// 一种 Item 的显示组件拿到的内容
export interface ItemRendererProps {
  view: ViewItem
  // tool_call 和它的 tool_result 合成一张工具卡；其他 kind 为空
  toolResult?: ViewItem
  // 已确认最终回复后，其他模型回复以单条中途回复展开
  isProgressMessage?: boolean
  sync: ConversationSync
}

export type ItemRenderer = ComponentType<ItemRendererProps>

// 工具卡的五种状态：参数生成中、执行中、成功、失败、已中断
export type ToolCallState =
  "generating" | "running" | "succeeded" | "failed" | "interrupted"

// 一种工具的显示组件拿到的内容；arguments 是解析后的参数，生成中或写坏了时是原文
export interface ToolRendererProps {
  // 所在 tool_call Item 的 view.key，展开状态按它保存
  viewKey: string
  call: ToolCall
  arguments: ParsedToolArguments
  result?: ToolResult
  state: ToolCallState
  sync: ConversationSync
}

export type ToolRenderer = ComponentType<ToolRendererProps>

// 消息区保留原 Item 引用；连续思考和工具按首成员标识，Turn 从运行时就拥有同一批行。
export interface ItemRow {
  kind: "item"
  view: ViewItem
  toolResult?: ViewItem
}

export interface ProcessGroupRow {
  kind: "group"
  groupKind: "process" | "model_errors"
  key: string
  turnID: TurnID
  rows: ItemRow[]
}

// 聚合当前动作引用原始条目，阶段由真实生成和执行状态计算。
export interface ProcessAction {
  view: ViewItem
  phase: "thinking" | "generating" | "running"
}

export type TurnContentRow = ItemRow | ProcessGroupRow
export type MessageRow =
  | ItemRow
  | {
      kind: "turn"
      key: string
      turnID: TurnID
      items: ViewItem[]
      rows: TurnContentRow[]
    }

// 顶层消息行的 key：轮次行用 Turn 的 key，轮外 Item 用它的 view.key
export function getMessageRowKey(row: MessageRow): string {
  return row.kind === "turn" ? row.key : row.view.key
}

// 调试选择只保存前端定位信息，内容始终从当前 Session 的消息行读取。
export type ItemDebugTarget =
  { kind: "item"; key: string } | { kind: "turn"; turnID: TurnID }

// 每种 kind 的显示组件；新增 kind 在 items/ 加一个文件并在这里登记一行
const itemRenderers: Partial<Record<ItemKind, ItemRenderer>> = {
  message: MessageItem,
  reasoning: ReasoningItem,
  tool_call: ToolCallItem,
  tool_result: ToolResultItem,
  model_error: ModelErrorItem,
  context_summary: ContextSummaryItem,
  session_fork: SessionForkItem,
  model_change: ModelChangeItem,
  executors_change: ExecutorsChangeItem,
  skill_instruction: SkillInstructionItem,
  agents_instruction: AgentsInstructionItem,
  memory_index: MemoryIndexItem,
  client_instruction: ClientInstructionItem,
  hook_run: HookRunItem,
  turn_interrupted: TurnInterruptedItem,
}

// 每种工具的显示组件；新增工具在 tools/ 加一个文件并在这里登记一行
const toolRenderers: Record<string, ToolRenderer> = {
  bash: BashTool,
  read_command_output: ReadCommandOutputTool,
  write_stdin: WriteStdinTool,
  stop_command: StopCommandTool,
  list_commands: ListCommandsTool,
  apply_patch: ApplyPatchTool,
  read_file: ReadFileTool,
  spawn_agent: SpawnAgentTool,
  send_message: SendMessageTool,
  wait_agent: WaitAgentTool,
  interrupt_agent: InterruptAgentTool,
  list_agents: ListAgentsTool,
  read_session_history: ReadSessionHistoryTool,
}

// kind 的显示组件；表里没有的 kind 显示折叠的 JsonView
export function getItemRenderer(kind: string): ItemRenderer {
  return Object.hasOwn(itemRenderers, kind)
    ? itemRenderers[kind as ItemKind]!
    : FallbackItem
}

// 工具的显示组件；MCP 工具（mcp__ 开头）和表里没有的工具用 JsonView 显示参数和结果
export function getToolRenderer(name: string): ToolRenderer {
  return Object.hasOwn(toolRenderers, name) ? toolRenderers[name] : DefaultTool
}

// 工具的显示名：MCP 工具写成「server/工具名」，其他原样
export function getToolDisplayName(name: string): string {
  const [prefix, serverID, ...rest] = name.split("__")
  const toolName = rest.join("__")
  return prefix === "mcp" && serverID && toolName
    ? serverID + "/" + toolName
    : name
}

// 内置工具使用简短动作，其他工具保留实际名称。
export function getToolActionLabel(name: string, t: TFunction): string {
  // 1. 按实际工具身份读取动作文案。
  const labelKeys: Record<string, ParseKeys> = {
    bash: "conversation.process.actions.bash",
    read_command_output: "conversation.process.actions.read_command_output",
    write_stdin: "conversation.process.actions.write_stdin",
    stop_command: "conversation.process.actions.stop_command",
    list_commands: "conversation.process.actions.list_commands",
    apply_patch: "conversation.process.actions.apply_patch",
    read_file: "conversation.process.actions.read_file",
    glob: "conversation.process.actions.glob",
    grep: "conversation.process.actions.grep",
    spawn_agent: "conversation.process.actions.spawn_agent",
    send_message: "conversation.process.actions.send_message",
    wait_agent: "conversation.process.actions.wait_agent",
    interrupt_agent: "conversation.process.actions.interrupt_agent",
    list_agents: "conversation.process.actions.list_agents",
    read_session_history: "conversation.process.actions.read_session_history",
  }
  return Object.hasOwn(labelKeys, name)
    ? t(labelKeys[name])
    : getToolDisplayName(name)
}

// 当前动作使用明确参数和调用方提供的名称，输出单行纯文本。
export function getProcessActionText(
  action: ProcessAction | undefined,
  t: TFunction,
  options: { recipientName?: string; agentName?: string } = {}
): string {
  // 1. 思考预览取已有摘要或正文的最后非空行。
  if (!action) return ""
  if (action.phase === "thinking") {
    const reasoning = action.view.item.reasoning
    const text =
      reasoning?.summary?.join("").trim() ||
      reasoning?.content?.join("").trim() ||
      ""
    const detail =
      text
        .split(/\r?\n/)
        .map((line) =>
          line
            .replace(/^\s*(?:#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)/, "")
            .replace(/!?\[([^\]]+)\]\([^)]*\)/g, "$1")
            .replace(/(`+|\*\*|__|~~)/g, "")
            .replace(/\*([^*]+)\*/g, "$1")
            .trim()
        )
        .filter(Boolean)
        .at(-1) ?? ""
    const label = t("conversation.reasoning.thinking")
    return detail
      ? t("conversation.process.preview", { action: label, detail })
      : label
  }
  // 2. 只有完整 JSON 参数参与预览，MCP 和未知工具仅显示名称。
  const call = action.view.item.tool_call
  if (!call?.name) return t("conversation.process.preparingTool")
  const { value } = parseToolArguments(call.arguments)
  let label = getToolActionLabel(call.name, t)
  if (call.name === "send_message") {
    const recipient =
      options.recipientName || getStringField(value, "session_id")
    if (recipient)
      label = t("conversation.process.sendTo", {
        recipient: recipient.replace(/[\r\n]+/g, " "),
      })
  } else if (call.name === "spawn_agent") {
    const agent = getStringField(value, "agent_name") || options.agentName
    if (agent)
      label = t("conversation.process.delegateTo", {
        agent: agent.replace(/[\r\n]+/g, " "),
      })
  }
  const detail = getToolArgumentPreview(call.name, value)
  const text = detail
    ? t("conversation.process.preview", { action: label, detail })
    : label
  // 3. 准备阶段保留同一动作和参数，仅增加准备提示。
  return action.phase === "generating"
    ? t("conversation.process.preparing", { action: text })
    : text
}

// 工具卡的状态：先看中断和结果；没有结果时，参数还在生成是 generating，这一轮已结束是 interrupted，否则在执行
export function getToolCallState(
  call: ToolCall,
  result: ToolResult | undefined,
  isStreaming: boolean,
  isTurnRunning: boolean
): ToolCallState {
  if (call.status === "interrupted") return "interrupted"
  if (result) return result.error ? "failed" : "succeeded"
  if (!call.status && isStreaming) return "generating"
  return isTurnRunning ? "running" : "interrupted"
}

// 消息的来源：用户输入、模型回复（有 model_call_id），或其他 Agent 转来的消息
export function getMessageSource(item: Item): "user" | "reply" | "agent" {
  if (item.message?.from?.kind === "user") return "user"
  return item.model_call_id ? "reply" : "agent"
}

// 将工具结果并入调用，在同一 Turn 内按原序组织过程与连续模型错误。
export function getMessageRows(
  items: ViewItem[],
  pageSeams: ReadonlySet<string> = new Set(),
  previous?: { rows: MessageRow[]; pageSeams: ReadonlySet<string> }
): MessageRow[] {
  // 1. 配对工具调用和结果，保留尚未加载调用的结果。
  const resultsByCallID = getToolResultsByCallID(items)
  const callIDs = new Set(
    items.flatMap((view) =>
      view.item.tool_call ? [view.item.tool_call.call_id] : []
    )
  )
  // 2. 按已有行的标识索引成员、组和 Turn。
  const previousItems = new Map<string, ItemRow>()
  const previousGroups = new Map<string, ProcessGroupRow>()
  const previousTurns = new Map<string, Extract<MessageRow, { kind: "turn" }>>()
  for (const row of previous?.rows ?? []) {
    if (row.kind === "item") previousItems.set(row.view.key, row)
    else {
      previousTurns.set(row.key, row)
      for (const content of row.rows) {
        if (content.kind === "item")
          previousItems.set(content.view.key, content)
        else {
          previousGroups.set(content.key, content)
          for (const member of content.rows)
            previousItems.set(member.view.key, member)
        }
      }
    }
  }
  const rows: MessageRow[] = []
  const turnSegments = new Map<TurnID, number>()
  for (let index = 0; index < items.length; index++) {
    const view = items[index]
    let turnRow = rows.at(-1)
    if (
      view.turnID &&
      (turnRow?.kind !== "turn" || turnRow.turnID !== view.turnID)
    ) {
      const segment = turnSegments.get(view.turnID) ?? 0
      turnSegments.set(view.turnID, segment + 1)
      const key = view.turnID + ":" + segment
      const previousTurn = previousTurns.get(key)
      let segmentEnd = index + 1
      while (
        segmentEnd < items.length &&
        items[segmentEnd].turnID === view.turnID
      )
        segmentEnd++
      // 3. 连续段的成员、结果配对与分页接缝未变时，直接保留整轮。
      if (
        previousTurn &&
        (segmentEnd === items.length || !!items[segmentEnd].turnID) &&
        previousTurn.items.length === segmentEnd - index &&
        previousTurn.items.every((member, offset) => {
          if (member !== items[index + offset]) return false
          if (pageSeams.has(member.key) !== previous!.pageSeams.has(member.key))
            return false
          const callID = member.item.tool_call?.call_id
          if (callID)
            return (
              previousItems.get(member.key)?.toolResult ===
              resultsByCallID.get(callID)
            )
          const resultCallID = member.item.tool_result?.call_id
          return (
            !resultCallID ||
            previousItems.has(member.key) === !callIDs.has(resultCallID)
          )
        })
      ) {
        rows.push(previousTurn)
        index = segmentEnd - 1
        continue
      }
      turnRow = {
        kind: "turn",
        key,
        turnID: view.turnID,
        items: [],
        rows: [],
      }
      rows.push(turnRow)
    }
    if (turnRow?.kind === "turn" && view.turnID) turnRow.items.push(view)
    const resultCallID = view.item.tool_result?.call_id
    if (resultCallID && callIDs.has(resultCallID)) continue
    const callID = view.item.tool_call?.call_id
    const toolResult = callID ? resultsByCallID.get(callID) : undefined
    const previousItem = previousItems.get(view.key)
    const itemRow: ItemRow =
      previousItem?.view === view && previousItem.toolResult === toolResult
        ? previousItem
        : { kind: "item", view, toolResult }
    if (!view.turnID || turnRow?.kind !== "turn") {
      rows.push(itemRow)
      continue
    }
    // 4. 过程与模型错误分别成组；错误只合并原始序列直接相邻且没有缺口的成员。
    const groupKind =
      view.item.kind === "model_error"
        ? "model_errors"
        : ["reasoning", "tool_call", "tool_result"].includes(view.item.kind)
          ? "process"
          : undefined
    if (groupKind) {
      const previous = turnRow.rows.at(-1)
      const previousView = items[index - 1]
      const isAdjacentError =
        previousView?.item.kind === "model_error" &&
        (previousView.ordinal === undefined ||
          view.ordinal === undefined ||
          previousView.ordinal + 1 === view.ordinal)
      if (
        previous?.kind === "group" &&
        previous.groupKind === groupKind &&
        !pageSeams.has(view.key) &&
        (groupKind === "process" || isAdjacentError)
      )
        previous.rows.push(itemRow)
      else
        turnRow.rows.push({
          kind: "group",
          groupKind,
          key: view.key,
          turnID: view.turnID,
          rows: [itemRow],
        })
    } else turnRow.rows.push(itemRow)
  }
  // 5. 变化的 Turn 仍保留未变化的组、成员数组和行数组。
  for (const [index, row] of rows.entries()) {
    if (row.kind !== "turn") continue
    const previousTurn = previousTurns.get(row.key)
    if (row === previousTurn) continue
    row.rows = row.rows.map((content) => {
      if (content.kind !== "group") return content
      const previousGroup = previousGroups.get(content.key)
      return previousGroup?.groupKind === content.groupKind &&
        previousGroup.turnID === content.turnID &&
        hasSameMembers(previousGroup.rows, content.rows)
        ? previousGroup
        : content
    })
    if (!previousTurn) continue
    if (hasSameMembers(previousTurn.items, row.items))
      row.items = previousTurn.items
    if (hasSameMembers(previousTurn.rows, row.rows))
      row.rows = previousTurn.rows
    if (row.items === previousTurn.items && row.rows === previousTurn.rows)
      rows[index] = previousTurn
  }
  return previous && hasSameMembers(previous.rows, rows) ? previous.rows : rows
}

// 比较两批成员的顺序和引用是否相同？
function hasSameMembers<T>(first: T[], second: T[]): boolean {
  // 1. 按原序比较成员引用。
  return (
    first.length === second.length &&
    first.every((member, index) => member === second[index])
  )
}

// 本轮已保存的范围是否全部加载，且没有尚未保存或生成中的成员？
export function isTurnRangeLoaded(items: ViewItem[], turn?: Turn): boolean {
  // 1. 检查范围和每个成员的保存状态。
  const range = turn?.items_range
  if (
    !range ||
    !range.first_ordinal ||
    range.last_ordinal < range.first_ordinal
  )
    return false
  if (items.some((view) => view.isStreaming || view.ordinal === undefined))
    return false
  const resultsByCallID = getToolResultsByCallID(items)
  if (
    items.some(
      (view) =>
        view.item.tool_call &&
        view.item.tool_call.status !== "interrupted" &&
        !resultsByCallID.has(view.item.tool_call.call_id)
    )
  )
    return false
  const ordinals = new Set(items.map((view) => view.ordinal!))
  return (
    ordinals.size === range.last_ordinal - range.first_ordinal + 1 &&
    Math.min(...ordinals) === range.first_ordinal &&
    Math.max(...ordinals) === range.last_ordinal
  )
}

// 各轮最后一条有文字的模型回复保存后可复制，不要求完整历史已加载。
export function getReplyKeysToCopy(items: ViewItem[]): Map<TurnID, string> {
  // 1. 按原序更新每轮候选；最后正文尚在生成或保存时，移除该轮入口。
  const keys = new Map<TurnID, string>()
  for (const view of items) {
    if (view.item.kind !== "message" || getMessageSource(view.item) !== "reply")
      continue
    const text = getContentText(
      (view.item.message?.content ?? []).flatMap((content) =>
        content ? [content] : []
      )
    )
    if (!text.trim()) continue
    if (view.isStreaming || view.ordinal === undefined) keys.delete(view.turnID)
    else keys.set(view.turnID, view.key)
  }
  return keys
}

// 只有成功、完整且不含工具的最终调用才产出结果；返回该调用的全部正文。
export function getTurnResult(
  items: ViewItem[],
  turn?: Turn,
  turnView?: TurnView
): ViewItem[] {
  // 1. 检查成功终态、最终调用和完整保存范围。
  const status = turnView?.status ?? turn?.status
  const finalID = turnView?.finalModelCallID ?? turn?.final_model_call_id
  if (status !== "completed" || !finalID || !isTurnRangeLoaded(items, turn))
    return []
  const finalItems = items.filter((view) => view.item.model_call_id === finalID)
  if (finalItems.some((view) => view.item.kind === "tool_call")) return []
  // 2. 该调用有可展示正文时，按原序保留全部 message，不回溯中途正文。
  const messages = finalItems.filter(
    (view) =>
      view.item.kind === "message" && getMessageSource(view.item) === "reply"
  )
  const hasVisibleMessage = messages.some(
    (view) =>
      getContentText(
        (view.item.message?.content ?? []).flatMap((content) =>
          content ? [content] : []
        )
      ).trim() || view.item.message?.content?.some((content) => content?.image)
  )
  return hasVisibleMessage ? messages : []
}

// 汇总相邻思考和工具的数量、阶段状态及仍在执行的最后一个动作。
export function getProcessFacts(
  rows: ItemRow[],
  isTurnRunning: boolean,
  isTailGroup: boolean
): {
  toolCount: number
  reasoningCount: number
  state: "running" | "succeeded" | "failed" | "interrupted"
  action?: ProcessAction
  hasFailures: boolean
  hasInterruptions: boolean
} {
  // 1. 按成员自己的结果累计终态，按原序选择最后一个实际动作。
  let toolCount = 0
  let reasoningCount = 0
  let hasFailures = false
  let hasInterruptions = false
  let action: ProcessAction | undefined
  for (const row of rows) {
    const item = row.view.item
    if (item.kind === "reasoning") {
      reasoningCount++
      hasInterruptions ||= item.reasoning?.status === "interrupted"
      if (
        isTurnRunning &&
        row.view.isStreaming &&
        item.reasoning?.status !== "interrupted"
      ) {
        action = { view: row.view, phase: "thinking" }
      }
      continue
    }
    if (item.kind === "tool_result") {
      hasFailures ||= !!item.tool_result?.error
      continue
    }
    const call = item.tool_call
    if (item.kind !== "tool_call" || !call) continue
    toolCount++
    const result = row.toolResult?.item.tool_result
    const commandState = [
      "bash",
      "read_command_output",
      "stop_command",
    ].includes(call.name)
      ? parseCommandState(result?.content)
      : undefined
    const state =
      commandState && isCommandFailed(commandState)
        ? "failed"
        : getToolCallState(
            call,
            result,
            row.view.isStreaming && isTurnRunning,
            isTurnRunning
          )
    hasFailures ||= state === "failed"
    hasInterruptions ||= state === "interrupted"
    if (state === "running" || state === "generating") {
      action = { view: row.view, phase: state }
    }
  }
  // 2. Turn 仍运行时，尾组等待后续内容；有实际动作的旧组也仍在运行。
  const isRunning = isTurnRunning && (isTailGroup || action !== undefined)
  return {
    toolCount,
    reasoningCount,
    state: isRunning
      ? "running"
      : hasFailures
        ? "failed"
        : hasInterruptions
          ? "interrupted"
          : "succeeded",
    action,
    hasFailures,
    hasInterruptions,
  }
}
