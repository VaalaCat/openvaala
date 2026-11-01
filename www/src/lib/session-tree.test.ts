import assert from "node:assert/strict"
import { test } from "node:test"
import type { SessionRuntimeInfo } from "./api/protocol.gen"
import {
  buildSessionTree,
  getSessionAncestors,
  type SessionTreeNode,
} from "./session-tree"

function sessionInfo(id: string, parentSessionID?: string): SessionRuntimeInfo {
  return {
    session: {
      id,
      conversation_id: "c1",
      agent_name: "default",
      parent_session_id: parentSessionID,
    },
    state: "idle",
    context: { window: 0, tokens: 0, source: "unknown" },
    usage: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
  }
}

// 只比较树形：每个节点的 Session ID 和子节点
type TreeShape = { id: string; children: TreeShape[] }
function getShape(nodes: SessionTreeNode[]): TreeShape[] {
  return nodes.map((node) => ({
    id: node.info.session.id,
    children: getShape(node.children),
  }))
}

test("buildSessionTree hangs sessions under their parents", () => {
  const cases: {
    name: string
    sessions: SessionRuntimeInfo[]
    want: TreeShape[]
  }[] = [
    {
      name: "root_children_grandchild_sorted_by_id",
      sessions: [
        sessionInfo("s4", "s2"),
        sessionInfo("s3", "s1"),
        sessionInfo("s1"),
        sessionInfo("s2", "s1"),
      ],
      want: [
        {
          id: "s1",
          children: [
            { id: "s2", children: [{ id: "s4", children: [] }] },
            { id: "s3", children: [] },
          ],
        },
      ],
    },
    {
      name: "missing_parent_goes_under_root",
      sessions: [
        sessionInfo("s1"),
        sessionInfo("s3", "gone"),
        sessionInfo("s2", "s1"),
      ],
      want: [
        {
          id: "s1",
          children: [
            { id: "s2", children: [] },
            { id: "s3", children: [] },
          ],
        },
      ],
    },
    {
      name: "only_root",
      sessions: [sessionInfo("s1")],
      want: [{ id: "s1", children: [] }],
    },
  ]
  for (const { name, sessions, want } of cases)
    assert.deepEqual(getShape(buildSessionTree(sessions, "s1")), want, name)
})

test("getSessionAncestors lists parents from the root down", () => {
  const sessions = Object.fromEntries(
    [
      sessionInfo("s1"),
      sessionInfo("s2", "s1"),
      sessionInfo("s3", "s2"),
      sessionInfo("s4", "gone"),
    ].map((info) => [info.session.id, info])
  )
  const cases: { name: string; sessionID: string; want: string[] }[] = [
    { name: "grandchild", sessionID: "s3", want: ["s1", "s2"] },
    { name: "child", sessionID: "s2", want: ["s1"] },
    { name: "root", sessionID: "s1", want: [] },
    { name: "missing_parent", sessionID: "s4", want: [] },
    { name: "unknown_session", sessionID: "s9", want: [] },
  ]
  for (const { name, sessionID, want } of cases)
    assert.deepEqual(
      getSessionAncestors(sessions, sessionID).map((info) => info.session.id),
      want,
      name
    )
})
