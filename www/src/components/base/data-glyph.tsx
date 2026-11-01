import { cn } from "@/lib/utils"

// 连字前缀：环用 {p:42}（外圈加按比例填充的扇形；字体没有 r 前缀），柱用 {b:10,40,80}
const glyphPrefixes = { ring: "p", bar: "b" } as const

export type DataGlyphProps = {
  // 读屏软件念的说明，由调用方按业务写
  label: string
  className?: string
} & ({ kind: "ring"; value: number } | { kind: "bar"; values: number[] })

// 用 Datatype 字体的连字画指标字形：环是一个百分比，柱是一组百分比；值先夹到 0–100 再取整。
// 字号跟随所在的文字角色
export function DataGlyph(props: DataGlyphProps) {
  const body =
    props.kind === "ring"
      ? String(toGlyphPercent(props.value))
      : props.values.map(toGlyphPercent).join(",")
  return (
    <span
      role="img"
      aria-label={props.label}
      className={cn("font-datatype", props.className)}
    >
      {"{" + glyphPrefixes[props.kind] + ":" + body + "}"}
    </span>
  )
}

// 字体只认 0–100 的整数
function toGlyphPercent(value: number): number {
  if (Number.isNaN(value)) return 0
  return Math.round(Math.min(Math.max(value, 0), 100))
}
