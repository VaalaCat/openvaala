import assert from "node:assert/strict"
import { test } from "node:test"
import {
  getMountedRowIndexes,
  type MessageRowBounds,
} from "./mounted-message-rows"

// 把行高数组累加成从 0 开始的行边界。
function rows(heights: number[]): MessageRowBounds[] {
  let start = 0
  return heights.map((height) => {
    const bounds = { start, end: start + height }
    start += height
    return bounds
  })
}

test("getMountedRowIndexes picks visible, overscan, pinned and anchor rows", () => {
  const cases: {
    name: string
    rowBounds: MessageRowBounds[]
    viewTop: number
    viewHeight: number
    overscan: number
    pinnedRecentRowCount: number
    anchorRowIndex?: number
    expected: number[]
  }[] = [
    {
      name: "visible rows plus overscan",
      rowBounds: rows(Array(10).fill(100)),
      viewTop: 300,
      viewHeight: 300,
      overscan: 4,
      pinnedRecentRowCount: 0,
      expected: [0, 1, 2, 3, 4, 5, 6, 7, 8],
    },
    {
      name: "overscan limited by row count",
      rowBounds: rows(Array(40).fill(50)),
      viewTop: 1000,
      viewHeight: 300,
      overscan: 4,
      pinnedRecentRowCount: 0,
      expected: [16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29],
    },
    {
      name: "overscan limited to one screen",
      rowBounds: rows(Array(10).fill(1000)),
      viewTop: 5000,
      viewHeight: 800,
      overscan: 4,
      pinnedRecentRowCount: 0,
      expected: [4, 5, 6],
    },
    {
      name: "recent rows stay mounted",
      rowBounds: rows(Array(30).fill(100)),
      viewTop: 0,
      viewHeight: 300,
      overscan: 4,
      pinnedRecentRowCount: 3,
      expected: [0, 1, 2, 3, 4, 5, 27, 28, 29],
    },
    {
      name: "anchor row stays mounted",
      rowBounds: rows(Array(30).fill(100)),
      viewTop: 0,
      viewHeight: 300,
      overscan: 4,
      pinnedRecentRowCount: 0,
      anchorRowIndex: 15,
      expected: [0, 1, 2, 3, 4, 5, 15],
    },
    {
      name: "zero view height",
      rowBounds: rows(Array(10).fill(100)),
      viewTop: 250,
      viewHeight: 0,
      overscan: 4,
      pinnedRecentRowCount: 1,
      expected: [2, 9],
    },
    {
      name: "no rows",
      rowBounds: [],
      viewTop: 0,
      viewHeight: 300,
      overscan: 4,
      pinnedRecentRowCount: 3,
      expected: [],
    },
  ]
  for (const c of cases) {
    assert.deepEqual(getMountedRowIndexes(c), c.expected, c.name)
  }
})
