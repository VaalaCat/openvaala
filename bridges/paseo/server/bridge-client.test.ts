import assert from "node:assert/strict"
import test from "node:test"
import { BridgeClient } from "./bridge-client.js"

test("bridge stream accepts a large frame and keeps simultaneous ID 1 requests separate", { timeout: 10_000 }, async () => {
  const child = `
    const rl = require("node:readline").createInterface({ input: process.stdin })
    let body
    const send = (frame) => console.log(JSON.stringify({ jsonrpc: "2.0", ...frame }))
    rl.on("line", (line) => {
      const request = JSON.parse(line)
      if (request.method === "echo") {
        body = request.params.body
        send({ method: "world/event", params: { event: "status_changed" } })
        send({ id: 1, method: "callback", params: { length: body.length } })
      } else if (request.method === "nested") {
        send({ id: request.id, result: "nested reply" })
      } else if (request.id === 1 && request.result === "nested reply") {
        send({ id: 1, result: body })
      }
    })
  `
  const body = "\x01汉".repeat(128 * 1024)
  let bridge: BridgeClient
  bridge = new BridgeClient(process.execPath, ["--eval", child], async (method, params) => {
    assert.equal(method, "callback")
    assert.deepEqual(params, { length: body.length })
    return bridge.request("nested", {})
  })
  try {
    assert.equal(Buffer.byteLength(body), 512 * 1024)
    assert.ok((await bridge.request<string>("echo", { body })) === body)
  } finally {
    await bridge.close()
  }
})

test("closing releases pending requests and waits for the child", { timeout: 10_000 }, async () => {
  const bridge = new BridgeClient(process.execPath, ["--eval", "process.stdin.resume()"], async () => ({}))
  const pending = assert.rejects(bridge.request("unanswered", {}), /World bridge closed/)
  await bridge.close()
  await pending
})

test("an oversized child frame ends the session", { timeout: 10_000 }, async () => {
  const child = 'process.stdout.write("x".repeat(8 * 1024 * 1024 + 4096)); process.stdin.resume()'
  const bridge = new BridgeClient(process.execPath, ["--eval", child], async () => ({}))
  try {
    await assert.rejects(bridge.request("unanswered", {}), /exceeds 8 MiB/)
  } finally {
    await bridge.close()
  }
})
