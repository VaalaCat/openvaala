import assert from "node:assert/strict"
import { test } from "node:test"
import {
  formatFullInteger,
  formatNumberUnit,
  parseNumberUnit,
  type NumberUnitResult,
} from "./number-unit"

test("十进制数量保留空值、0 和完整安全整数", () => {
  const cases: { raw: string; expected: NumberUnitResult }[] = [
    { raw: "128K", expected: { ok: true, value: 128_000 } },
    { raw: "1M", expected: { ok: true, value: 1_000_000 } },
    { raw: "1.5K", expected: { ok: true, value: 1_500 } },
    { raw: " 1.001k ", expected: { ok: true, value: 1_001 } },
    { raw: "1.000001m", expected: { ok: true, value: 1_000_001 } },
    { raw: ".5K", expected: { ok: true, value: 500 } },
    { raw: "1.0000", expected: { ok: true, value: 1 } },
    { raw: "0", expected: { ok: true, value: 0 } },
    { raw: "0.0K", expected: { ok: true, value: 0 } },
    { raw: "", expected: { ok: true, value: undefined } },
    { raw: "  \n ", expected: { ok: true, value: undefined } },
    {
      raw: "9007199254740991",
      expected: { ok: true, value: Number.MAX_SAFE_INTEGER },
    },
    {
      raw: "9007199254740.991K",
      expected: { ok: true, value: Number.MAX_SAFE_INTEGER },
    },
    { raw: "-1K", expected: { ok: false, error: "negative" } },
    { raw: "-0", expected: { ok: false, error: "negative" } },
    { raw: "no", expected: { ok: false, error: "invalid" } },
    { raw: "1KB", expected: { ok: false, error: "invalid" } },
    { raw: "1e3", expected: { ok: false, error: "invalid" } },
    { raw: "1 K", expected: { ok: false, error: "invalid" } },
    { raw: "1,000", expected: { ok: false, error: "invalid" } },
    { raw: "1.", expected: { ok: false, error: "invalid" } },
    { raw: "1.5", expected: { ok: false, error: "fractional" } },
    { raw: "0.0001K", expected: { ok: false, error: "fractional" } },
    {
      raw: "1.0000000000000001K",
      expected: { ok: false, error: "fractional" },
    },
    {
      raw: "9007199254740991.1",
      expected: { ok: false, error: "fractional" },
    },
    { raw: "9007199254740992", expected: { ok: false, error: "unsafe" } },
    { raw: "9007199254740.992K", expected: { ok: false, error: "unsafe" } },
    { raw: "999999999999999999M", expected: { ok: false, error: "unsafe" } },
  ]
  for (const { raw, expected } of cases)
    assert.deepEqual(parseNumberUnit(raw), expected, raw)
})

test("中英文使用相同的十进制紧凑单位并可显示完整整数", () => {
  const cases = [
    { value: 0, compact: "0", full: "0" },
    { value: 999, compact: "999", full: "999" },
    { value: 1_000, compact: "1K", full: "1,000" },
    { value: 1_500, compact: "1.5K", full: "1,500" },
    { value: 128_000, compact: "128K", full: "128,000" },
    { value: 204_800, compact: "204.8K", full: "204,800" },
    { value: 1_000_000, compact: "1M", full: "1,000,000" },
    { value: 1_500_000, compact: "1.5M", full: "1,500,000" },
    {
      value: Number.MAX_SAFE_INTEGER,
      compact: "9007199254.7M",
      full: "9,007,199,254,740,991",
    },
  ]
  for (const { value, compact, full } of cases) {
    assert.equal(formatNumberUnit(value), compact)
    for (const language of ["zh-CN", "en"])
      assert.equal(formatFullInteger(value, language), full, language)
  }
})
