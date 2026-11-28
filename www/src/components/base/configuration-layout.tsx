import { Children, useId, useState, type ReactNode } from "react"
import { ChevronRightIcon } from "lucide-react"
import {
  Collapsible,
  CollapsibleTrigger,
  CollapsibleContent,
} from "@/components/ui/collapsible"
import { FieldSet, FieldLegend, FieldGroup } from "@/components/ui/field"
import { Separator } from "@/components/ui/separator"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

// 设置表单的一节：点标题或摘要折叠内容，右侧操作独立。
export function ConfigurationLayout({
  id,
  title,
  actions,
  summary,
  open,
  onOpenChange,
  children,
}: {
  id?: string
  title: ReactNode
  actions?: ReactNode
  summary?: ReactNode
  open?: boolean
  onOpenChange?: (open: boolean) => void
  children?: ReactNode
}) {
  const [localOpen, setLocalOpen] = useState(true)
  const expanded = open ?? localOpen
  const titleID = useId()
  const summaryID = useId()
  const hasBody = Children.toArray(children).length > 0
  const details = summary && (
    <span
      id={summaryID}
      className="ml-auto flex min-w-0 flex-wrap items-center justify-end gap-x-2 gap-y-1 text-right text-caption wrap-anywhere whitespace-normal color-muted"
    >
      {summary}
    </span>
  )
  return (
    <Collapsible
      id={id}
      open={expanded}
      onOpenChange={onOpenChange ?? setLocalOpen}
      className="configuration-section min-w-0 border"
    >
      <FieldSet className="min-w-0 gap-0">
        <FieldLegend id={titleID} className="sr-only">
          {title}
        </FieldLegend>
        <div
          className={cn("flex w-full items-center", !hasBody && "gap-2 p-3")}
        >
          {hasBody ? (
            <CollapsibleTrigger
              aria-labelledby={titleID}
              aria-describedby={summary ? summaryID : undefined}
              render={
                <Button
                  type="button"
                  variant="ghost"
                  className="configuration-trigger h-auto min-h-12 min-w-0 flex-1 justify-start gap-2 px-3 py-2 text-left text-section"
                />
              }
            >
              <ChevronRightIcon
                className={cn(
                  "shrink-0 transition-transform",
                  expanded && "rotate-90"
                )}
              />
              <span className="max-w-1/2 shrink-0 whitespace-normal">
                {title}
              </span>
              {details}
            </CollapsibleTrigger>
          ) : (
            <>
              <span className="min-w-0 flex-1 truncate text-section">{title}</span>
              {details}
            </>
          )}
          {actions && (
            <div
              className={cn(
                "flex shrink-0 items-center gap-2",
                hasBody && "pr-3"
              )}
            >
              {actions}
            </div>
          )}
        </div>
        {hasBody && (
          <CollapsibleContent
            keepMounted
            className="process-disclosure-panel min-w-0"
            inert={!expanded}
            aria-hidden={!expanded || undefined}
          >
            <div className="process-disclosure-content">
              <Separator />
              <FieldGroup className="gap-3 p-3 [&_[data-slot=field-group]]:gap-3 [&_[data-slot=field]]:gap-2">
                {children}
              </FieldGroup>
            </div>
          </CollapsibleContent>
        )}
      </FieldSet>
    </Collapsible>
  )
}
