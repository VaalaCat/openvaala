import type { WebSocketLike } from "./server-connection"

type Bytes = Uint8Array<ArrayBuffer>
const encoder = new TextEncoder()
const decoder = new TextDecoder()
const protocol = encoder.encode("openvaala-world-e2ee/v2")
const messageLimit = 16 * 1024 * 1024

export type WorldTarget = {
  name?: string
  node?: string
  domain?: string
  public_key: string
  entries: { direct_urls?: string[]; relays?: string[] }
}

function decode64(value: string): Bytes {
  return Uint8Array.from(atob(value), (c) => c.charCodeAt(0))
}

function baseURL(raw: string, websocket: boolean) {
  if (!/^(?:https?|wss?):\/\/[^/]+/i.test(raw) || /[\\\s]/.test(raw))
    throw new Error("invalid World URL")
  const url = new URL(raw)
  if (
    !(websocket ? /^(https?|wss?):$/ : /^https?:$/).test(url.protocol) ||
    !url.hostname ||
    url.username ||
    url.password ||
    /[?#]/.test(raw)
  )
    throw new Error("invalid World URL")
  return url
}

function isWorldDomain(domain: string) {
  const parts = /^(\[[0-9a-fA-F:.]+\]|[a-z0-9.-]+)(?::([0-9]+))?$/.exec(domain)
  if (!parts) return false
  const [, host, port] = parts
  if (port && (Number(port) < 1 || Number(port) > 65535)) return false
  if (host.startsWith("[")) {
    try {
      return Boolean(new URL("http://" + host).hostname)
    } catch {
      return false
    }
  }
  return (
    host.length <= 253 &&
    host
      .split(".")
      .every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))
  )
}

// 设置草稿和 serve 按相同顺序选择页面来源，已运行的链接提供兜底地址。
export function getRemoteAccessPage(
  webURL: string,
  relays: string[],
  direct: string[],
  runningURL?: string
) {
  function page(raw: string) {
    try {
      const url = new URL(raw.replace(/^ws/, "http"))
      if (!["http:", "https:"].includes(url.protocol)) return undefined
      const host = url.hostname
      const local = host.startsWith("[")
        ? /^\[(?:::1?\]|f[cd]|fe[89ab])/.test(host)
        : !host.includes(".") ||
          /(?:\.local|\.localhost)$/.test(host) ||
          /^(?:127|10|0)\.|^192\.168\.|^169\.254\.|^172\.(?:1[6-9]|2[0-9]|3[01])\./.test(
            host
          )
      return { origin: url.origin, secure: url.protocol === "https:", local }
    } catch {
      return undefined
    }
  }
  if (webURL) return page(webURL)
  const relayPages = relays.map(page).filter((p) => p !== undefined)
  const directPages = direct.map(page).filter((p) => p !== undefined)
  const pages = [...relayPages, ...directPages]
  return (
    pages.find((p) => p.secure && !p.local) ??
    directPages.find((p) => p.secure) ??
    pages[0] ??
    (runningURL ? page(runningURL) : undefined)
  )
}

export function getWorldTargetName(target: WorldTarget) {
  return (
    target.name ||
    (target.node && target.domain
      ? target.node + "@" + target.domain
      : target.public_key.replace(/\+/g, "-").replace(/\//g, "_").slice(0, 8))
  )
}

export function parseWorldLink(raw: string) {
  const marker = raw.indexOf("#world=")
  if (marker < 0) throw new Error("invalid World link")
  const fields = new URLSearchParams(raw.slice(marker + 1))
  const payload = fields.get("world") ?? ""
  if (!/^[A-Za-z0-9_-]+$/.test(payload)) throw new Error("invalid World link")
  const target: WorldTarget = JSON.parse(
    decoder.decode(decode64(payload.replace(/-/g, "+").replace(/_/g, "/")))
  )
  if (
    (target.name !== undefined && typeof target.name !== "string") ||
    (target.node != null &&
      (typeof target.node !== "string" ||
        (target.node !== "" &&
          !/^[a-z0-9][a-z0-9_-]{0,62}$/.test(target.node)))) ||
    (target.domain != null &&
      (typeof target.domain !== "string" ||
        (target.domain !== "" && !isWorldDomain(target.domain)))) ||
    !/^[A-Za-z0-9+/]{43}=$/.test(target.public_key) ||
    !target.entries
  )
    throw new Error("invalid World target")
  const direct = target.entries.direct_urls ?? [],
    relays = target.entries.relays ?? []
  if (
    !Array.isArray(direct) ||
    !Array.isArray(relays) ||
    direct.length + relays.length === 0
  )
    throw new Error("missing World entries")
  for (const url of [...direct, ...relays]) baseURL(url, true)
  return {
    target,
    url: raw.slice(0, marker) + "#world=" + payload,
    token: fields.get("token") ?? "",
  }
}

const relayInstanceCount = 2
const relayInstancesTimeout = 5_000
const relayDialTimeout = 15_000
const relayInstances = new Map<
  string,
  { instances: string[]; fetched: number }
>()

// 按原样的实例地址和目标的 Ed25519 公钥选出得分最高的两个实例。
export async function selectRelayInstances(instances: string[], key: Bytes) {
  const ranked = await Promise.all(
    instances.map(async (base) => ({
      base,
      score: new Uint8Array(
        await crypto.subtle.digest(
          "SHA-256",
          concat(encoder.encode("openvaala-relay-hrw/v1\0" + base + "\0"), key)
        )
      ),
    }))
  )
  function compareBytes(a: Bytes, b: Bytes) {
    for (let i = 0; i < Math.min(a.length, b.length); i++) {
      if (a[i] !== b[i]) return a[i] - b[i]
    }
    return a.length - b.length
  }
  ranked.sort(
    (a, b) =>
      compareBytes(b.score, a.score) ||
      compareBytes(encoder.encode(a.base), encoder.encode(b.base))
  )
  return ranked.slice(0, relayInstanceCount).map(({ base }) => base)
}

async function getRelayInstances(base: string): Promise<string[]> {
  const cached = relayInstances.get(base)
  if (cached && Date.now() - cached.fetched < 60_000) return cached.instances
  // 取列表失败时沿用上次的列表，没有就按单实例处理；失败结果同样缓存，避免每次拨号都等超时。
  const instances = (await fetchRelayInstances(base)) ?? cached?.instances ?? []
  relayInstances.set(base, { instances, fetched: Date.now() })
  return instances
}

// fetchRelayInstances 请求入口的实例列表；404 表示单实例，其余失败返回 undefined。
async function fetchRelayInstances(
  base: string
): Promise<string[] | undefined> {
  try {
    const url = baseURL(base, true)
    url.protocol = url.protocol.replace("ws", "http")
    url.pathname =
      url.pathname.replace(/\/+$/, "") + "/world/v1/relay/instances"
    const response = await fetch(url, {
      signal: AbortSignal.timeout(relayInstancesTimeout),
      cache: "no-store",
      credentials: "omit",
    })
    if (!response.ok && response.status !== 404) return undefined
    const instances: unknown =
      response.status === 404 ? [] : (await response.json()).instances
    if (
      !Array.isArray(instances) ||
      !instances.every((url) => typeof url === "string")
    )
      return undefined
    return instances
  } catch {
    return undefined
  }
}

export function connectionBudget(target?: WorldTarget) {
  return target
    ? 12_000 +
        (target.entries.relays?.length ?? 0) *
          (relayInstancesTimeout + relayInstanceCount * relayDialTimeout)
    : 10_000
}

function concat(...values: Bytes[]): Bytes {
  const out = new Uint8Array(values.reduce((n, v) => n + v.length, 0))
  let offset = 0
  for (const value of values) {
    out.set(value, offset)
    offset += value.length
  }
  return out
}

export function createWorldHello(
  id: Bytes,
  targetKey: Bytes,
  ephemeral: Bytes
) {
  return concat(new Uint8Array([2, 2, 0]), id, targetKey, ephemeral)
}

// 核对节点在 ready 中的签名，派生收发两个方向的消息密钥。
export async function openWorldChannel(
  privateKey: CryptoKey,
  hello: Bytes,
  ready: Bytes
) {
  const api = crypto.subtle
  const signer = await api.importKey(
    "raw",
    hello.slice(19, 51),
    "Ed25519",
    false,
    ["verify"]
  )
  const ephemeral = ready.slice(0, 32)
  if (
    ready.length !== 96 ||
    !(await api.verify(
      "Ed25519",
      signer,
      ready.slice(32),
      concat(protocol, hello, ephemeral)
    ))
  )
    throw new Error("invalid world ready signature")
  const peer = await api.importKey("raw", ephemeral, "X25519", false, [])
  const shared = await api.deriveBits(
    { name: "X25519", public: peer },
    privateKey,
    256
  )
  const material = await api.importKey("raw", shared, "HKDF", false, [
    "deriveBits",
  ])
  const id = hello.slice(3, 19)
  const keys = await api.deriveBits(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: id,
      info: concat(protocol, hello, ready),
    },
    material,
    512
  )
  const sendKey = await api.importKey(
    "raw",
    keys.slice(0, 32),
    "AES-GCM",
    false,
    ["encrypt"]
  )
  const recvKey = await api.importKey("raw", keys.slice(32), "AES-GCM", false, [
    "decrypt",
  ])
  let sent = 0n,
    received = 0n
  function params(sequence: bigint) {
    const iv = new Uint8Array(12)
    new DataView(iv.buffer).setBigUint64(4, sequence, true)
    return { name: "AES-GCM", iv, additionalData: id }
  }
  return {
    async encrypt(plain: Bytes) {
      if (plain.length > messageLimit || sent === 0xffffffffffffffffn)
        throw new Error("world message exceeds limit")
      const aes = params(++sent)
      return concat(
        aes.iv.slice(4),
        new Uint8Array(await api.encrypt(aes, sendKey, plain))
      )
    },
    async decrypt(frame: Bytes) {
      if (
        frame.length < 24 ||
        frame.length > messageLimit + 24 ||
        new DataView(frame.buffer, frame.byteOffset).getBigUint64(0, true) !==
          received + 1n
      )
        throw new Error("invalid world frame")
      return new Uint8Array(
        await api.decrypt(params(++received), recvKey, frame.subarray(8))
      )
    },
  }
}

// WorldTransport 在认证成功后向 RPC 层交付 open，读写分别保持帧顺序。
export class WorldTransport implements WebSocketLike {
  onopen: WebSocketLike["onopen"] = null
  onmessage: WebSocketLike["onmessage"] = null
  onclose: WebSocketLike["onclose"] = null
  private socket?: WebSocket
  private channel?: Awaited<ReturnType<typeof openWorldChannel>>
  private writes = Promise.resolve()
  private stopped = false

  constructor(target: WorldTarget, token: string) {
    void this.connect(target, token).catch(() => this.finish())
  }

  send(data: string) {
    this.writes = this.writes
      .then(async () => {
        if (this.stopped) return
        const frame = await this.channel!.encrypt(encoder.encode(data))
        if (!this.stopped) this.socket!.send(frame)
      })
      .catch(() => this.finish())
  }

  close() {
    this.finish()
  }

  private finish(code = 1006, reason = "") {
    if (this.stopped) return
    this.stopped = true
    this.socket?.close()
    this.onclose?.(new CloseEvent("close", { code, reason }))
  }

  private async connect(target: WorldTarget, token: string) {
    const directDeadline = performance.now() + 2000
    const key = decode64(target.public_key)
    for (const base of target.entries.direct_urls ?? []) {
      if (this.stopped) return
      const remaining = directDeadline - performance.now()
      if (remaining <= 0) break
      if (await this.tryRoute(base, true, target, token, remaining)) return
    }
    for (const entry of target.entries.relays ?? []) {
      if (this.stopped) return
      const instances = await getRelayInstances(entry)
      const selected = instances.length
        ? await selectRelayInstances(instances, key)
        : [entry]
      for (const base of selected) {
        if (this.stopped) return
        if (await this.tryRoute(base, false, target, token, relayDialTimeout))
          return
      }
    }
    this.finish(4004, `world_target_unreachable: ${getWorldTargetName(target)}`)
  }

  private async tryRoute(
    base: string,
    direct: boolean,
    target: WorldTarget,
    token: string,
    timeout: number
  ) {
    try {
      const url = baseURL(base, true)
      url.protocol = url.protocol.replace("http", "ws")
      const suffix = direct ? "/world/v1/direct" : "/world/v1/relay/dial"
      const prefix = url.pathname.replace(/\/+$/, "")
      url.pathname = prefix.endsWith(suffix) ? prefix : prefix + suffix
      url.searchParams.set(
        "target",
        target.public_key
          .replace(/\+/g, "-")
          .replace(/\//g, "_")
          .replace(/=+$/, "")
      )
      if (direct) url.searchParams.set("purpose", "rpc")
      await this.runRoute(url, target, token, timeout)
      return true
    } catch {
      if (!this.channel) return false
      this.finish()
      return true
    }
  }

  private async runRoute(
    url: URL,
    target: WorldTarget,
    token: string,
    timeout: number
  ) {
    const socket = new WebSocket(url)
    this.socket = socket
    socket.binaryType = "arraybuffer"
    const opened = new Promise<void>((resolve, reject) => {
      socket.onopen = () => resolve()
      socket.onerror = () => reject(new Error("world socket unavailable"))
    })
    const frames = new ReadableStream<Bytes>({
      start(controller) {
        socket.onmessage = ({ data }) => {
          if (
            data instanceof ArrayBuffer &&
            data.byteLength <= messageLimit + 24
          )
            controller.enqueue(new Uint8Array(data))
          else socket.close()
        }
        socket.onclose = () => controller.close()
      },
    }).getReader()
    const read = async () => {
      const frame = await frames.read()
      if (frame.done) throw new Error("world socket closed")
      return frame.value
    }
    async function handshake() {
      await opened
      const ephemeral = (await crypto.subtle.generateKey("X25519", true, [
        "deriveBits",
      ])) as CryptoKeyPair
      const publicKey = new Uint8Array(
        await crypto.subtle.exportKey("raw", ephemeral.publicKey)
      )
      const hello = createWorldHello(
        crypto.getRandomValues(new Uint8Array(16)),
        decode64(target.public_key),
        publicKey
      )
      socket.send(hello)
      return openWorldChannel(ephemeral.privateKey, hello, await read())
    }
    let timer: ReturnType<typeof setTimeout> | undefined
    const deadline = new Promise<never>((_, reject) => {
      timer = setTimeout(
        () => reject(new Error("world handshake timed out")),
        timeout
      )
    })
    try {
      this.channel = await Promise.race([handshake(), deadline])
    } catch (error) {
      socket.onmessage = null
      socket.onclose = null
      void frames.cancel()
      socket.close()
      throw error
    } finally {
      clearTimeout(timer)
    }
    // 从握手完成起，认证和 initialize 共用十秒。
    const initializeTimer = setTimeout(() => this.finish(), 10_000)
    try {
      socket.send(
        await this.channel.encrypt(encoder.encode(JSON.stringify({ token })))
      )
      const result = JSON.parse(
        decoder.decode(await this.channel.decrypt(await read()))
      )
      if (result.error === "unauthorized") {
        this.finish(4401, "unauthorized")
        return
      }
      if (result.ok !== true)
        throw new Error("invalid World authentication response")
      if (this.stopped) return
      const initialized = this.onopen?.(new Event("open"))
      void Promise.resolve(initialized).finally(() =>
        clearTimeout(initializeTimer)
      )
      while (!this.stopped) {
        const data = decoder.decode(await this.channel.decrypt(await read()))
        this.onmessage?.(new MessageEvent("message", { data }))
      }
    } finally {
      clearTimeout(initializeTimer)
    }
  }
}
