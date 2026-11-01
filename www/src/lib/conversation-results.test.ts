import assert from "node:assert/strict"
import { test } from "node:test"
import type { SessionRuntimeInfo } from "./api/protocol.gen"
import {
  $conversationResults,
  getConversationResultKey,
  getConversationStatus,
  recordConversationResult,
} from "./conversation-results"

test("normal subscriptions remember only root terminal results per server", () => {
  const cases = [
    { sessionID: "root", status: "completed", want: "completed" },
    { sessionID: "root", status: "failed", want: "failed" },
    { sessionID: "root", status: "interrupted", want: "interrupted" },
    { sessionID: "root", status: "running", want: undefined },
    { sessionID: "root", status: undefined, want: undefined },
    { sessionID: "child", status: "completed", want: undefined },
  ] as const
  for (const each of cases) {
    $conversationResults.set({})
    const info = {
      session: { id: each.sessionID },
      last_turn_id: "turn-1",
      last_turn_status: each.status,
    } as SessionRuntimeInfo
    recordConversationResult("server-A", "conversation", "root", info)
    const result =
      $conversationResults.get()[
        getConversationResultKey("server-A", "conversation")
      ]
    assert.equal(result?.status, each.want, `${each.sessionID}/${each.status}`)
    assert.equal(
      $conversationResults.get()[
        getConversationResultKey("server-B", "conversation")
      ],
      undefined
    )
    if (result) {
      recordConversationResult("server-A", "conversation", "root", {
        ...info,
        last_turn_id: undefined,
      })
      assert.equal(
        $conversationResults.get()[
          getConversationResultKey("server-A", "conversation")
        ],
        result
      )
    }
  }
})

test("running facts override old results without inventing a result", () => {
  const cases = [
    { isRunning: undefined, status: "completed", want: "unknown" },
    { isRunning: true, status: "failed", want: "running" },
    { isRunning: false, status: "completed", want: "completed" },
    { isRunning: false, status: "interrupted", want: "interrupted" },
    { isRunning: false, status: "failed", want: "failed" },
    { isRunning: false, status: undefined, want: "idle" },
    { isRunning: true, status: undefined, want: "running" },
    { isRunning: undefined, status: undefined, want: "unknown" },
  ] as const
  for (const each of cases)
    assert.equal(
      getConversationStatus(
        each.isRunning,
        each.status ? { turnID: "old", status: each.status } : undefined
      ),
      each.want
    )
})
