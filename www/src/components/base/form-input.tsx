import type { ComponentProps, ReactNode } from "react"
import { FieldTooltip } from "./field-tooltip"
import { Input } from "@/components/ui/input"
import { Field, FieldLabel } from "@/components/ui/field"
import { cn } from "@/lib/utils"
export function FormInput({
  label,
  action,
  error,
  notice,
  className,
  ...props
}: ComponentProps<typeof Input> & {
  label: string
  action?: ReactNode
  error?: string
  notice?: string
}) {
  return (
    <Field
      className={cn("min-w-0", className)}
      data-disabled={props.disabled}
      data-invalid={!!error || props["aria-invalid"]}
    >
      <FieldLabel className="text-ui" htmlFor={props.id}>
        {label}
      </FieldLabel>
      <div className="flex min-w-0 items-center gap-2">
        <FieldTooltip
          message={error ?? notice}
          severity={error ? "error" : "notice"}
          focusOnly={!error}
        >
          <Input className="text-ui" {...props} />
        </FieldTooltip>
        {action}
      </div>
    </Field>
  )
}
