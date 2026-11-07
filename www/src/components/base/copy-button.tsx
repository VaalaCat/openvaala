import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useEffect, useState } from "react"
import { CheckIcon, CopyIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { copyText } from "@/lib/clipboard"

export function CopyButton({
  value,
  label,
  className,
  size = "icon-sm",
}: {
  value: string
  label?: string
  className?: string
  size?: "icon" | "icon-sm" | "icon-xs"
}) {
  const { t } = useTranslation()
  const [result, setResult] = useState<
    { state: "copied" } | { state: "failed"; message?: string }
  >()
  const failed = result?.state === "failed"
  const [pending, setPending] = useState(false)
  useEffect(() => {
    if (result?.state !== "copied") return
    const timer = setTimeout(() => setResult(undefined), 1500)
    return () => clearTimeout(timer)
  }, [result])
  async function copy() {
    setPending(true)
    try {
      await copyText(value)
      setResult({ state: "copied" })
    } catch (error) {
      setResult({
        state: "failed",
        message: error instanceof Error ? error.message : undefined,
      })
    } finally {
      setPending(false)
    }
  }
  return (
    <div
      data-copy-result={result?.state}
      className="relative inline-flex shrink-0 flex-col items-end gap-2"
    >
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              variant="ghost"
              size={size}
              className={className}
              aria-label={label ?? t("common.actions.copy")}
              disabled={pending}
              onClick={(event) => {
                event.stopPropagation()
                void copy()
              }}
              focusableWhenDisabled
            />
          }
        >
          {result && !failed ? <CheckIcon /> : <CopyIcon />}
        </TooltipTrigger>
        <TooltipContent className="text-caption">
          {label ?? t("common.actions.copy")}
        </TooltipContent>
      </Tooltip>
      <span
        role="status"
        className={cn(
          failed
            ? "max-w-40 text-caption wrap-anywhere color-destructive"
            : "sr-only"
        )}
      >
        {result?.state === "copied"
          ? t("common.copy.success")
          : result?.state === "failed"
            ? result.message
              ? t("common.copy.failed", { message: result.message })
              : t("common.copy.manual")
            : undefined}
      </span>
    </div>
  )
}
