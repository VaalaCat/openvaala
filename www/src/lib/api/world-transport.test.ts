import test from "node:test"
import { EventEmitter, once } from "node:events"
import { setImmediate } from "node:timers/promises"
import assert from "node:assert/strict"
import relayVectors from "../../../../pkg/world/testdata/relay-hrw.json" with { type: "json" }
import vectors from "../../../../pkg/world/testdata/channel-v2.json" with { type: "json" }
import {
  selectRelayInstances,
  WorldTransport,
  createWorldHello,
  openWorldChannel,
} from "./world-transport"

for (const { name, instances, key, selected } of relayVectors) {
  test("relay HRW: " + name, async () => {
    assert.deepEqual(
      await selectRelayInstances(
        instances,
        Uint8Array.from(Buffer.from(key, "base64url"))
      ),
      selected
    )
  })
}

test("world transport matches Go vectors", async () => {
  const v = vectors.cases.find((v) => v.purpose === "rpc")!
  const bytes = (s: string) => Uint8Array.from(Buffer.from(s, "base64"))
  const hello = bytes(v.hello)
  const privateKey = await crypto.subtle.importKey(
    "jwk",
    {
      kty: "OKP",
      crv: "X25519",
      x: Buffer.from(hello.slice(51)).toString("base64url"),
      d: Buffer.from(v.dial_ephemeral_private, "base64").toString("base64url"),
    },
    "X25519",
    false,
    ["deriveBits"]
  )
  assert.deepEqual(
    createWorldHello(bytes(v.channel_id), hello.slice(19, 51), hello.slice(51)),
    hello
  )
  const channel = await openWorldChannel(privateKey, hello, bytes(v.ready))
  assert.deepEqual(
    await channel.encrypt(bytes(v.dial_plaintext)),
    bytes(v.dial_frame)
  )
  assert.deepEqual(
    await channel.decrypt(bytes(v.accept_frame)),
    bytes(v.accept_plaintext)
  )
  await assert.rejects(channel.decrypt(bytes(v.accept_frame)))
  const badReady = bytes(v.ready)
  badReady[95] ^= 1
  await assert.rejects(openWorldChannel(privateKey, hello, badReady))
})

for (const stalledAt of ["open", "ready"]) {
  test(`World dial tries the relay when direct stalls before ${stalledAt}`, async (t) => {
    const routes: URL[] = []
    const closed: URL[] = []
    const events = new EventEmitter()
    const stalled = once(events, "stalled")
    class Socket {
      onopen?: () => void
      readonly url: URL
      constructor(url: URL) {
        this.url = url
        routes.push(url)
        if (routes.length === 1 && stalledAt === "ready")
          queueMicrotask(() => this.onopen?.())
        else events.emit("stalled")
      }
      send() {
        events.emit("stalled")
      }
      close() {
        closed.push(this.url)
      }
    }
    t.mock.property(
      globalThis,
      "WebSocket",
      Socket as unknown as typeof WebSocket
    )
    t.mock.method(
      globalThis,
      "fetch",
      async () => new Response(null, { status: 404 })
    )
    t.mock.timers.enable({ apis: ["setTimeout"] })
    const transport = new WorldTransport(
      {
        node: "b",
        domain: "home.test",
        public_key: Buffer.alloc(32).toString("base64"),
        entries: {
          direct_urls: ["https://direct.test"],
          relays: ["https://relay.test"],
        },
      },
      "secret"
    )
    try {
      await stalled
      assert.equal(routes[0].pathname, "/world/v1/direct")
      assert.equal(routes.length, 1)
      t.mock.timers.tick(2000)
      await setImmediate()
      assert.deepEqual(closed, [routes[0]])
      assert.equal(routes[1].pathname, "/world/v1/relay/dial")
    } finally {
      transport.close()
    }
  })
}
