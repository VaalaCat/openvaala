import type { ComponentProps } from "react"
import { useTranslation } from "react-i18next"
import { Input } from "@/components/ui/input"
import type { NumberUnitError } from "@/lib/number-unit"
import { cn } from "@/lib/utils"
import { FieldTooltip } from "./field-tooltip"

const errorKeys = {
  negative: "numberUnitInput.errors.negative",
  invalid: "numberUnitInput.errors.invalid",
  fractional: "numberUnitInput.errors.fractional",
  unsafe: "numberUnitInput.errors.unsafe",
} as const

// 保留调用方的数值输入原文，并展示提交时返回的字段错误。
export function NumberUnitInput({
  value,
  onChange,
  error,
  className,
  ...props
}: Omit<ComponentProps<typeof Input>, "value" | "onChange" | "type"> & {
  value: string
  onChange: (value: string) => void
  error?: NumberUnitError
}) {
  // 1. 将字段错误转成当前语言的提示，输入时只传递原文。
  const { t } = useTranslation()
  return (
    <FieldTooltip message={error ? t(errorKeys[error]) : undefined}>
      <Input
        {...props}
        type="text"
        className={cn("text-ui", className)}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </FieldTooltip>
  )
}
