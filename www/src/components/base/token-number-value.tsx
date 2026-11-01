import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import {
  Popover,
  PopoverContent,
  PopoverTitle,
  PopoverTrigger,
} from "@/components/ui/popover"
import { formatFullInteger, formatNumberUnit } from "@/lib/number-unit"
import { cn } from "@/lib/utils"

// 展示紧凑 token 数量，通过键盘或触摸展开完整整数。
export function TokenNumberValue({
  value,
  prefix = "",
  interactive = true,
  className,
}: {
  value: number
  prefix?: string
  interactive?: boolean
  className?: string
}) {
  // 1. 生成紧凑值和带调用方标记的完整整数。
  const { t, i18n } = useTranslation()
  const compact = prefix + formatNumberUnit(value)
  const full = prefix + formatFullInteger(value, i18n.language)
  if (value < 1_000 || !interactive)
    return (
      <Tooltip disabled={value < 1_000}>
        <TooltipTrigger
          render={<span />}
          aria-label={value >= 1_000 ? full : undefined}
          className={cn("inline-flex h-5 items-center text-token", className)}
        >
          {compact}
        </TooltipTrigger>
        <TooltipContent className="text-caption">{full}</TooltipContent>
      </Tooltip>
    )

  // 2. 缩略数量提供可点击、可聚焦的完整值浮层。
  return (
    <Popover>
      <PopoverTrigger
        aria-label={t("tokenNumberValue.fullValue", { value: full })}
        render={
          <Button
            variant="ghost"
            className={cn("h-5 p-0 text-token text-help", className)}
          />
        }
      >
        {compact}
      </PopoverTrigger>
      <PopoverContent className="w-auto max-w-full p-2 text-token">
        <PopoverTitle className="sr-only">
          {t("tokenNumberValue.fullValue", { value: full })}
        </PopoverTitle>
        {full}
      </PopoverContent>
    </Popover>
  )
}
