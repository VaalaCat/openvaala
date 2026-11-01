const minuteMs = 60_000
const hourMs = 60 * minuteMs
const dayMs = 24 * hourMs

// 把毫秒时间戳显示成「5 分钟前」这样的相对时间；超过一周显示日期，不是今年的带上年份
export function formatRelativeTime(
  timestamp: number,
  language: string,
  now: number = Date.now()
): string {
  const elapsed = Math.max(0, now - timestamp)
  const relative = new Intl.RelativeTimeFormat(language, {
    numeric: "auto",
    style: "short",
  })
  if (elapsed < minuteMs) return relative.format(0, "second")
  if (elapsed < hourMs)
    return relative.format(-Math.floor(elapsed / minuteMs), "minute")
  if (elapsed < dayMs)
    return relative.format(-Math.floor(elapsed / hourMs), "hour")
  if (elapsed < 7 * dayMs)
    return relative.format(-Math.floor(elapsed / dayMs), "day")
  const date = new Date(timestamp)
  return date.toLocaleDateString(language, {
    year:
      date.getFullYear() === new Date(now).getFullYear()
        ? undefined
        : "numeric",
    month: "short",
    day: "numeric",
  })
}

// Intl.DurationFormat 还不在 TypeScript 的 lib 里，只声明这里用到的部分
type DurationFormatConstructor = new (
  language: string,
  options: { style: "narrow"; secondsDisplay?: "always" }
) => { format(duration: Record<string, number>): string }

// 把毫秒时长显示成「3m 4s」「3分钟4秒」：只保留最大的两个单位，不到一分钟只显示秒
export function formatDuration(ms: number, language: string): string {
  const { DurationFormat } = Intl as unknown as {
    DurationFormat: DurationFormatConstructor
  }
  const totalSeconds = Math.floor(ms / 1000)
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60
  if (hours > 0)
    return new DurationFormat(language, { style: "narrow" }).format({
      hours,
      minutes,
    })
  if (minutes > 0)
    return new DurationFormat(language, { style: "narrow" }).format({
      minutes,
      seconds,
    })
  return new DurationFormat(language, {
    style: "narrow",
    secondsDisplay: "always",
  }).format({ seconds })
}
