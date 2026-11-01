export type NumberUnitError = "negative" | "invalid" | "fractional" | "unsafe"

export type NumberUnitResult =
  | { ok: true; value: number | undefined }
  | { ok: false; error: NumberUnitError }

const compactNumberFormat = new Intl.NumberFormat("en", {
  useGrouping: false,
  maximumFractionDigits: 1,
})

// 按十进制 K／M 展示数量，最多保留一位小数。
export function formatNumberUnit(value: number): string {
  // 1. 按数量选择十进制单位。
  if (value >= 1_000_000)
    return compactNumberFormat.format(value / 1_000_000) + "M"
  if (value >= 1_000) return compactNumberFormat.format(value / 1_000) + "K"
  return String(value)
}

// 按界面语言显示带分组的完整整数。
export function formatFullInteger(value: number, language: string): string {
  // 1. 保留整数的全部有效位。
  return new Intl.NumberFormat(language, { maximumFractionDigits: 0 }).format(
    value
  )
}

// 解析十进制整数和 K／M 小数，返回未设置、完整整数或字段错误。
export function parseNumberUnit(raw: string): NumberUnitResult {
  // 1. 识别空白、负数和十进制格式。
  const text = raw.trim()
  if (!text) return { ok: true, value: undefined }
  if (text.startsWith("-")) return { ok: false, error: "negative" }
  const match = /^(\d+(?:\.\d+)?|\.\d+)([km])?$/i.exec(text)
  if (!match) return { ok: false, error: "invalid" }

  // 2. 用整数运算换算单位并检查余数。
  const [integer, fraction = ""] = match[1].split(".")
  const multiplier =
    match[2]?.toUpperCase() === "M"
      ? 1_000_000n
      : match[2]?.toUpperCase() === "K"
        ? 1_000n
        : 1n
  const numerator = BigInt(integer + fraction) * multiplier
  const denominator = 10n ** BigInt(fraction.length)
  if (numerator % denominator !== 0n) return { ok: false, error: "fractional" }

  // 3. 只把安全范围内的整数交给调用方。
  const value = numerator / denominator
  if (value > BigInt(Number.MAX_SAFE_INTEGER))
    return { ok: false, error: "unsafe" }
  return { ok: true, value: Number(value) }
}
