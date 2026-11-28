import { spawn } from "node:child_process"
import { addAbortListener } from "node:events"

const maxFrameBytes = 8 * 1024 * 1024
type RPCID = string | number | null
type RPCFrame = {
  jsonrpc: "2.0"
  id?: RPCID
  method?: string
  params?: unknown
  result?: unknown
  error?: { code: number; message: string }
}

type PendingRequest = {
  resolve(result: unknown): void
  reject(error: Error): void
}

export class BridgeClient {
  private readonly child
  private readonly pending = new Map<number, PendingRequest>()
  private nextID = 0
  private failure?: Error
  readonly closed: Promise<void>

  constructor(
    command: string,
    args: string[],
    private readonly onRequest: (method: string, params: unknown) => Promise<unknown>
  ) {
    this.child = spawn(command, args, { stdio: ["pipe", "pipe", "pipe"] })
    this.closed = new Promise((resolve) => {
      this.child.once("close", (code, signal) => {
        this.fail(new Error(`World bridge exited (${signal ?? code})`))
        resolve()
      })
    })
    this.child.once("error", (error) => this.fail(error))
    this.child.stdin.on("error", (error) => this.fail(error))
    this.child.stderr.setEncoding("utf8")
    this.child.stderr.on("data", (text: string) => console.error(text.trimEnd()))
    void this.read().catch((error: Error) => this.fail(error))
  }

  async request<T>(method: string, params: unknown, signal?: AbortSignal): Promise<T> {
    signal?.throwIfAborted()
    if (this.failure) throw this.failure
    const id = ++this.nextID
    let abort: ReturnType<typeof addAbortListener> | undefined
    try {
      return await new Promise<T>((resolve, reject) => {
        this.pending.set(id, { resolve: (result) => resolve(result as T), reject })
        if (signal) abort = addAbortListener(signal, () => reject(signal.reason))
        this.write({ jsonrpc: "2.0", id, method, params })
      })
    } finally {
      abort?.[Symbol.dispose]()
      this.pending.delete(id)
    }
  }

  async close() {
    this.fail(new Error("World bridge closed"))
    const deadline = setTimeout(() => this.child.kill("SIGKILL"), 3_000)
    deadline.unref()
    try {
      await this.closed
    } finally {
      clearTimeout(deadline)
    }
  }

  private fail(error: Error) {
    if (this.failure) return
    this.failure = error
    for (const request of this.pending.values()) request.reject(error)
    this.child.stdin.end()
    this.child.kill("SIGTERM")
  }

  private write(frame: RPCFrame) {
    if (this.failure) throw this.failure
    const bytes = Buffer.from(JSON.stringify(frame) + "\n")
    if (bytes.length - 1 > maxFrameBytes) throw new Error("World bridge frame exceeds 8 MiB")
    this.child.stdin.write(bytes)
  }

  private async read() {
    let pieces: Buffer[] = []
    let length = 0
    for await (const chunk of this.child.stdout as AsyncIterable<Buffer>) {
      let offset = 0
      while (offset < chunk.length) {
        const newline = chunk.indexOf(10, offset)
        const part = chunk.subarray(offset, newline < 0 ? chunk.length : newline)
        length += part.length
        if (length > maxFrameBytes) throw new Error("World bridge frame exceeds 8 MiB")
        pieces.push(part)
        if (newline < 0) break
        const frame: RPCFrame = JSON.parse(Buffer.concat(pieces, length).toString("utf8"))
        pieces = []
        length = 0
        offset = newline + 1
        this.dispatch(frame)
      }
    }
    if (length) throw new Error("Incomplete World bridge frame")
  }

  private dispatch(frame: RPCFrame) {
    if (frame.jsonrpc !== "2.0") throw new Error("Invalid World bridge protocol version")
    if ("method" in frame) {
      if (frame.id !== undefined && typeof frame.method === "string")
        void this.respond(frame.id, frame.method, frame.params).catch((error: Error) => this.fail(error))
      return
    }
    if (typeof frame.id !== "number") return
    const pending = this.pending.get(frame.id)
    if (frame.error) pending?.reject(new Error(frame.error.message))
    else pending?.resolve(frame.result)
  }

  private async respond(id: RPCID, method: string, params: unknown) {
    let response: RPCFrame
    try {
      response = { jsonrpc: "2.0", id, result: await this.onRequest(method, params) }
    } catch (error) {
      response = { jsonrpc: "2.0", id, error: { code: -32000, message: String(error instanceof Error ? error.message : error) } }
    }
    if (!this.failure) this.write(response)
  }
}
