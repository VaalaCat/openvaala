import assert from "node:assert/strict"
import { test } from "node:test"
import { checkAttachmentBudget, getImageBytes } from "./attachments"

const megabyte = 1024 * 1024

test("checkAttachmentBudget keeps one message within 10MB", () => {
  const cases: {
    name: string
    existing: number[]
    added: number[]
    want: boolean
  }[] = [
    { name: "exactly_10mb", existing: [], added: [10 * megabyte], want: true },
    {
      name: "over_with_existing",
      existing: [9 * megabyte],
      added: [2 * megabyte],
      want: false,
    },
    { name: "nothing", existing: [], added: [], want: true },
    {
      name: "one_byte_over",
      existing: [5 * megabyte],
      added: [5 * megabyte, 1],
      want: false,
    },
    {
      name: "several_small",
      existing: [megabyte, megabyte],
      added: [megabyte],
      want: true,
    },
  ]
  for (const { name, existing, added, want } of cases)
    assert.equal(checkAttachmentBudget(existing, added).ok, want, name)
})

test("restored DataURL counts actual image bytes", () => {
  const cases = [
    { url: "data:image/png;base64,YQ==", want: 1 },
    { url: "data:image/png;base64,YWI=", want: 2 },
    { url: "data:image/png;base64,YWJj", want: 3 },
    { url: "data:image/svg+xml,%3Csvg%3E", want: 5 },
  ]
  for (const each of cases)
    assert.equal(getImageBytes(each.url), each.want, each.url)
})
