import { Children, useState, type ReactNode } from "react"
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

// 设置表单的一节：标题行右边放操作和摘要；有内容时标题可以折叠内容，默认展开
export function ConfigurationLayout({
  title,
  actions,
  summary,
  children,
}: {
  title: ReactNode
  actions?: ReactNode
  summary?: ReactNode
  children?: ReactNode
}) {
  const [expanded, setExpanded] = useState(true)
  const hasBody = Children.toArray(children).length > 0
  return (
    <Collapsible
      open={expanded}
      onOpenChange={setExpanded}
      className="configuration-section min-w-0 border"
    >
      <FieldSet className="min-w-0 gap-0">
        <FieldLegend className="sr-only">{title}</FieldLegend>
        <div className="flex w-full items-center gap-2 p-3">
          {hasBody ? (
            <CollapsibleTrigger
              render={
                <Button
                  type="button"
                  variant="ghost"
                  className="configuration-trigger min-w-0 justify-start text-section"
                />
              }
            >
              <ChevronRightIcon
                className={cn(
                  "shrink-0 transition-transform",
                  expanded && "rotate-90"
                )}
              />
              <span className="truncate">{title}</span>
            </CollapsibleTrigger>
          ) : (
            <span className="min-w-0 truncate text-section">{title}</span>
          )}
          <div className="ml-auto flex min-w-0 items-center gap-2">
            {actions && (
              <div className="flex shrink-0 items-center gap-2">{actions}</div>
            )}
            {summary && (
              <span className="truncate text-caption color-muted">
                {summary}
              </span>
            )}
          </div>
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
