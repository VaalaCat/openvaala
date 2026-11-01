import assert from "node:assert/strict"
import { test } from "node:test"
import { formatDuration, formatRelativeTime } from "./relative-time"

const now = new Date(2026, 9, 27, 12, 0, 0).getTime()
const minute = 60_000
const hour = 60 * minute
const day = 24 * hour

test("formatRelativeTime picks the unit by elapsed time", () => {
  const cases: {
    name: string
    elapsed: number
    language: string
    want: string
  }[] = [
    { name: "just_now", elapsed: 30_000, language: "en", want: "now" },
    {
      name: "future_clamped",
      elapsed: -5 * minute,
      language: "en",
      want: "now",
    },
    {
      name: "minutes",
      elapsed: 5 * minute + 20_000,
      language: "en",
      want: "5 min. ago",
    },
    {
      name: "hours",
      elapsed: 2 * hour + minute,
      language: "en",
      want: "2 hr. ago",
    },
    {
      name: "yesterday",
      elapsed: day + hour,
      language: "en",
      want: "yesterday",
    },
    { name: "days_zh", elapsed: 3 * day, language: "zh-CN", want: "3天前" },
    {
      name: "same_year_date",
      elapsed: 30 * day,
      language: "en",
      want: "Sep 27",
    },
    {
      name: "other_year_date_zh",
      elapsed: 365 * day,
      language: "zh-CN",
      want: "2025年10月27日",
    },
  ]
  for (const { name, elapsed, language, want } of cases)
    assert.equal(formatRelativeTime(now - elapsed, language, now), want, name)
})

test("formatDuration keeps the two largest units", () => {
  const cases: { name: string; ms: number; language: string; want: string }[] =
    [
      { name: "under_a_second", ms: 400, language: "en", want: "0s" },
      { name: "seconds", ms: 12_400, language: "en", want: "12s" },
      {
        name: "minutes",
        ms: 3 * minute + 4_000,
        language: "en",
        want: "3m 4s",
      },
      { name: "whole_minutes", ms: 2 * minute, language: "en", want: "2m" },
      {
        name: "hours",
        ms: hour + 2 * minute + 5_000,
        language: "en",
        want: "1h 2m",
      },
      {
        name: "zh",
        ms: 3 * minute + 4_000,
        language: "zh-CN",
        want: "3分钟4秒",
      },
    ]
  for (const { name, ms, language, want } of cases)
    assert.equal(formatDuration(ms, language), want, name)
})
