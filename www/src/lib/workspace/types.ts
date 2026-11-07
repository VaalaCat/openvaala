import type { IJsonModel } from "flexlayout-react"
import type { ConversationID, ItemID, SessionID } from "../api/protocol.gen"

// ── TabWorkspace 标签页 ──

// TabWorkspaceTab 是一个标签页要怎么打开；也是 localStorage 里存的内容。
// 主对话、子 Agent 属于所在工作区的对话，所以标签页里不记对话 ID。
export type TabWorkspaceTab =
  | { kind: "main" }
  | { kind: "sub-agent"; sessionID: SessionID }
  | {
      kind: "side-conversation"
      // conversationID 是旁支自己的临时对话，不是它的来源对话
      conversationID: ConversationID
      // boundaryItemID 是 Web 写入的旁支说明 Item 的 ID；从它所在的那一轮开始显示。
      boundaryItemID: ItemID
      // question 是第一条问题的前 20 个字，用作标题；只输入 /btw 时为空。
      question: string
    }
  | {
      kind: "terminal"
      executorID: string
      directory: string
      // terminalID 是 serve 上终端的 ID，打开成功后才有；刷新后凭它接回原来的终端
      terminalID?: string
    }

// ── TabWorkspace 保存内容 ──

// SavedConversationWorkspace 是一条对话保存的工作区：flexlayout 的布局和标签页 ID 对应的标签页，
// usedAt 是最近一次使用的时间（毫秒），保存满了按它淘汰最早的。
export type SavedConversationWorkspace = {
  layout: IJsonModel
  tabs: Record<string, TabWorkspaceTab>
  usedAt: number
}

// SavedServerWorkspaces 是 localStorage 里 openvaala.workspace.<serverID> 的内容。
export type SavedServerWorkspaces = {
  conversations: Record<ConversationID, SavedConversationWorkspace>
}

// ── TabWorkspace 布局操作 ──

// SplitDirection 是新区域相对被分栏区域的方向。
export type SplitDirection = "RIGHT" | "BOTTOM"
