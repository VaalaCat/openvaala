import assert from "node:assert/strict"
import { test } from "node:test"
import { readWorldToolMessages, readWorldToolPeers } from "./world-tool-results"

test("reads persisted single-message and thread result shapes", () => {
  for (const [value, expected] of [
    [
      { message_id: "one", status: "replied", reply: "**done**" },
      [
        {
          messageID: "one",
          status: "replied",
          reply: "**done**",
          errorCode: undefined,
          error: undefined,
        },
      ],
    ],
    [
      [
        {
          message_id: "two",
          status: "failed",
          error_text: "delivery failed",
          error_code: "world_agent_no_model",
        },
        { message_id: "one", status: "replied", reply_body: "**done**" },
        null,
        { status: "failed" },
      ],
      [
        {
          messageID: "two",
          status: "failed",
          reply: undefined,
          errorCode: "world_agent_no_model",
          error: "delivery failed",
        },
        {
          messageID: "one",
          status: "replied",
          reply: "**done**",
          errorCode: undefined,
          error: undefined,
        },
      ],
    ],
    [undefined, []],
    [[], []],
  ] as const) {
    assert.deepEqual(readWorldToolMessages(value), expected)
  }
})

test("reads peer snapshots and treats a null list as empty", () => {
  const peer = {
    address: "office@example.test",
    node_name: "office",
    online: true,
    targets: [{ name: "assistant", visibility: "domain" }],
  }
  for (const [value, expected] of [
    [{ peers: [peer, null] }, [peer]],
    [{ peers: null }, []],
    [undefined, []],
  ] as const) {
    assert.deepEqual(readWorldToolPeers(value), expected)
  }
})
