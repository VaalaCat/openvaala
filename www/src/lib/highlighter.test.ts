import assert from "node:assert/strict"
import { test } from "node:test"
import {
  HighlightWorkerClient,
  retainHighlightedLines,
  reuseHighlightedLines,
  type HighlightedLine,
  type HighlightDocument,
} from "./highlighter"
import type { HighlightRequest, HighlightResponse } from "./highlighter-types"

class TestHighlightWorker {
  sent: HighlightRequest[] = []
  onmessage: ((event: { data: HighlightResponse }) => void) | null = null
  onerror: (() => void) | null = null
  onmessageerror: (() => void) | null = null
  isTerminated = false

  postMessage(request: HighlightRequest) {
    this.sent.push(request)
  }

  terminate() {
    this.isTerminated = true
  }

  reply(id: number, lines: HighlightedLine[] | undefined) {
    this.onmessage?.({ data: { id, lines } })
  }
}

function coloredLine(content: string, color = "red"): HighlightedLine {
  return [
    { content, style: { "--shiki-light": color } } as HighlightedLine[number],
  ]
}

function recordDocument(lang: string) {
  const results: { text: string; lines: HighlightedLine[] }[] = []
  const document: HighlightDocument = {
    lang,
    onHighlighted: (text, lines) => results.push({ text, lines }),
  }
  return { document, results }
}

test("continuous updates coalesce waiting text and apply completed prefixes before streaming ends", () => {
  for (const lang of ["ts", "json"]) {
    const worker = new TestHighlightWorker()
    const client = new HighlightWorkerClient(() => worker as unknown as Worker)
    const { document, results } = recordDocument(lang)
    client.highlightLines(document, "first")
    client.highlightLines(document, "skipped")
    client.highlightLines(document, "latest")
    assert.deepEqual(
      worker.sent.map(({ text }) => text),
      ["first"],
      lang
    )

    worker.reply(worker.sent[0].id, [coloredLine("first")])
    assert.deepEqual(
      results.map(({ text }) => text),
      ["first"],
      lang
    )
    assert.deepEqual(
      worker.sent.map(({ text }) => text),
      ["first", "latest"],
      lang
    )
    assert.equal(worker.sent[1].lang, lang)

    client.highlightLines(document, "final")
    worker.reply(worker.sent[0].id, [coloredLine("stale duplicate")])
    assert.equal(results.length, 1, lang)
    worker.reply(worker.sent[1].id, [coloredLine("latest")])
    assert.deepEqual(
      results.map(({ text }) => text),
      ["first", "latest"],
      lang
    )
    assert.equal(worker.sent[2].text, "final", lang)
    worker.reply(worker.sent[2].id, [coloredLine("final")])
    assert.deepEqual(
      results.map(({ text }) => text),
      ["first", "latest", "final"],
      lang
    )
  }
})

test("language changes and unmount cancel queued work and ignore old responses", () => {
  for (const name of ["language_change", "unmount"]) {
    const worker = new TestHighlightWorker()
    const client = new HighlightWorkerClient(() => worker as unknown as Worker)
    const oldView = recordDocument("ts")
    client.highlightLines(oldView.document, "old")
    client.highlightLines(oldView.document, "old queued")
    client.disposeDocument(oldView.document)
    client.highlightLines(oldView.document, "after dispose")
    const nextView = recordDocument("json")
    if (name === "language_change")
      client.highlightLines(nextView.document, "new")
    worker.reply(worker.sent[0].id, [coloredLine("old")])
    assert.deepEqual(oldView.results, [], name)
    if (name === "unmount") {
      assert.equal(worker.sent.length, 1, name)
      continue
    }
    assert.deepEqual(
      worker.sent.map(({ text, lang }) => ({ text, lang })),
      [
        { text: "old", lang: "ts" },
        { text: "new", lang: "json" },
      ]
    )
    worker.reply(worker.sent[1].id, [coloredLine("new")])
    assert.deepEqual(
      nextView.results.map(({ text }) => text),
      ["new"],
      name
    )
  }
})

test("independent code views keep their requests when another view changes", () => {
  for (const disposeFirst of [false, true]) {
    const worker = new TestHighlightWorker()
    const client = new HighlightWorkerClient(() => worker as unknown as Worker)
    const firstView = recordDocument("ts")
    const secondView = recordDocument("json")
    client.highlightLines(firstView.document, "view one")
    client.highlightLines(secondView.document, "view two")
    client.highlightLines(firstView.document, "view one final")
    if (disposeFirst) client.disposeDocument(firstView.document)
    const texts = disposeFirst
      ? ["view one", "view two"]
      : ["view one", "view two", "view one final"]
    for (let index = 0; index < texts.length; index++) {
      assert.equal(worker.sent[index].text, texts[index])
      worker.reply(worker.sent[index].id, [coloredLine(texts[index])])
    }
    assert.deepEqual(
      firstView.results.map(({ text }) => text),
      disposeFirst ? [] : ["view one", "view one final"]
    )
    assert.deepEqual(
      secondView.results.map(({ text }) => text),
      ["view two"]
    )
  }
})

test("worker failure drops queued work and keeps subsequent text out of the main-thread highlighter", () => {
  for (const failure of ["creation", "send", "runtime", "message"] as const) {
    const worker = new TestHighlightWorker()
    let creations = 0
    const client = new HighlightWorkerClient(() => {
      creations++
      if (failure === "creation") throw new Error("worker unavailable")
      return worker as unknown as Worker
    })
    if (failure === "send")
      worker.postMessage = () => {
        throw new Error("send failed")
      }
    const { document, results } = recordDocument("ts")
    client.highlightLines(document, "first")
    client.highlightLines(document, "queued")
    if (failure === "runtime") worker.onerror?.()
    if (failure === "message") worker.onmessageerror?.()
    const sentCount = worker.sent.length
    client.highlightLines(document, "next")
    if (worker.sent[0]) worker.reply(worker.sent[0].id, [coloredLine("late")])
    assert.deepEqual(results, [], failure)
    assert.equal(worker.sent.length, sentCount, failure)
    assert.equal(creations, 1, failure)
    assert.equal(worker.isTerminated, failure !== "creation", failure)
  }
})

test("an unsupported document does not stop another document from being colored", () => {
  for (const lang of ["unknown", "failed_language_load"]) {
    const worker = new TestHighlightWorker()
    const client = new HighlightWorkerClient(() => worker as unknown as Worker)
    const unsupported = recordDocument(lang)
    const supported = recordDocument("ts")
    client.highlightLines(unsupported.document, "plain")
    client.highlightLines(supported.document, "colored")
    worker.reply(worker.sent[0].id, undefined)
    worker.reply(worker.sent[1].id, [coloredLine("colored")])
    assert.deepEqual(unsupported.results, [])
    assert.deepEqual(
      supported.results.map(({ text }) => text),
      ["colored"]
    )
  }
})

test("disposed and oversized requests do not create a worker", () => {
  for (const name of ["disposed", "oversized"]) {
    let creations = 0
    const client = new HighlightWorkerClient(() => {
      creations++
      throw new Error("must not start")
    })
    const { document, results } = recordDocument("ts")
    if (name === "disposed") client.disposeDocument(document)
    client.highlightLines(
      document,
      name === "oversized" ? "a".repeat(200 * 1024 + 1) : "const n = 1"
    )
    assert.equal(creations, 0)
    assert.deepEqual(results, [])
  }
})

test("pending highlights retain unchanged lines and show changed text immediately", () => {
  const previous = [coloredLine("first"), coloredLine("second")]
  const cases = [
    { text: "first\nsecond\nthird", retained: [true, true, false] },
    { text: "first\nsecond appended", retained: [true, false] },
    { text: "replacement\nsecond", retained: [false, true] },
    { text: "first", retained: [true] },
  ]
  for (const { text, retained } of cases) {
    const lines = retainHighlightedLines(text, "first\nsecond", previous)
    assert.equal(lines.length, retained.length, text)
    for (const [index, isRetained] of retained.entries())
      assert.equal(lines[index], isRetained ? previous[index] : undefined, text)
  }
})

test("finished highlighting reuses only lines whose text and colors did not change", () => {
  const previous = [coloredLine("first"), coloredLine("second")]
  const cases = [
    {
      lines: [
        coloredLine("first"),
        coloredLine("second"),
        coloredLine("third"),
      ],
      reused: [true, true, false],
    },
    {
      lines: [coloredLine("first"), coloredLine("changed")],
      reused: [true, false],
    },
    {
      lines: [coloredLine("first", "blue"), coloredLine("second")],
      reused: [false, true],
    },
    {
      lines: [[{ content: "first" }], coloredLine("second")],
      reused: [false, true],
    },
    {
      lines: [[{ content: "fi" }, { content: "rst" }], coloredLine("second")],
      reused: [false, true],
    },
  ]
  for (const { lines, reused } of cases) {
    const result = reuseHighlightedLines(previous, lines)
    assert.deepEqual(result, lines)
    for (const [index, isReused] of reused.entries())
      assert.equal(result[index], isReused ? previous[index] : lines[index])
  }
})
