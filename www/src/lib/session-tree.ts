import type { SessionID, SessionRuntimeInfo } from "./api/protocol.gen"

// SessionTreeNode 是子 Agent 树里的一个 Session 和它创建的子 Agent
export interface SessionTreeNode {
  info: SessionRuntimeInfo
  children: SessionTreeNode[]
}

// 按 session.parent_session_id 把对话里的 Session 挂成树，同级按 session.id（创建顺序）排列；
// 父 Session 不在列表里的挂到根下，根也不在时放在最外层
export function buildSessionTree(
  sessions: SessionRuntimeInfo[],
  rootSessionID: SessionID
): SessionTreeNode[] {
  // 1. 每个 Session 一个节点，按 ID 排好，挂接后同级自然有序。
  const nodes = new Map<SessionID, SessionTreeNode>()
  const sorted = [...sessions].sort((a, b) =>
    a.session.id < b.session.id ? -1 : a.session.id > b.session.id ? 1 : 0
  )
  for (const info of sorted) nodes.set(info.session.id, { info, children: [] })
  // 2. 根之外的节点挂到父节点下，父节点不存在时挂到根下。
  const root = nodes.get(rootSessionID)
  const topLevel: SessionTreeNode[] = root ? [root] : []
  for (const node of nodes.values()) {
    if (node === root) continue
    const parentID = node.info.session.parent_session_id
    const parent = (parentID && nodes.get(parentID)) || root
    if (parent) parent.children.push(node)
    else topLevel.push(node)
  }
  return topLevel
}

// 从根往下列出这个 Session 的上级 Session，不含它自己；中途有父 Session 不在列表里时只列到断开处之下
export function getSessionAncestors(
  sessions: Record<SessionID, SessionRuntimeInfo>,
  sessionID: SessionID
): SessionRuntimeInfo[] {
  const ancestors: SessionRuntimeInfo[] = []
  for (
    let parentID = sessions[sessionID]?.session.parent_session_id;
    parentID;
    parentID = sessions[parentID]?.session.parent_session_id
  ) {
    const parent = sessions[parentID]
    if (!parent) break
    ancestors.unshift(parent)
  }
  return ancestors
}

// Session 的显示名：根 Session 用 rootName（「主 Agent」），子 Agent 用任务名，没有任务名时用 Agent 名
export function getSessionName(
  info: SessionRuntimeInfo,
  rootName: string
): string {
  if (!info.session.parent_session_id) return rootName
  return info.session.task_name || info.session.agent_name || info.session.id
}
