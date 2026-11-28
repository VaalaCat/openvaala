import type { ReactNode } from "react"
import { EllipsisIcon, PlusIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { ListSearch } from "@/components/base/list-search"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { useWorldAdminFilters } from "@/hooks/use-world-admin-filters"
import { cn } from "@/lib/utils"

export function AdminSection({
  title,
  description,
  actions,
  children,
}: {
  title: string
  description: string
  actions?: ReactNode
  children: ReactNode
}) {
  return (
    <section aria-label={title} className="min-w-0">
      <header className="mb-4">
        <div className="flex min-h-8 flex-wrap items-center justify-between gap-2">
          <h2 className="world-admin-title">{title}</h2>
          {actions}
        </div>
        <p className="world-admin-description mt-1 color-muted">
          {description}
        </p>
      </header>
      {children}
    </section>
  )
}

export function AdminSearch({
  placeholder,
  children,
}: {
  placeholder: string
  children?: ReactNode
}) {
  const { search, update } = useWorldAdminFilters()
  return (
    <div className="mb-3 flex min-w-0 flex-wrap items-center justify-between gap-2">
      <form
        className="w-full sm:max-w-72"
        onSubmit={(e) => {
          e.preventDefault()
          update(
            "adminSearch",
            String(new FormData(e.currentTarget).get("keyword") ?? "")
          )
        }}
      >
        <ListSearch
          keyword={search.get("adminSearch") ?? ""}
          placeholder={placeholder}
          aria-label={placeholder}
          onKeywordChange={(q) => update("adminSearch", q)}
          onClear={() => update("adminSearch", "")}
        />
      </form>
      {children}
    </div>
  )
}

export function AdminTable({
  label,
  headings,
  numeric = [],
  children,
}: {
  label: string
  headings: string[]
  numeric?: number[]
  children: ReactNode
}) {
  return (
    <div className="min-w-0 overflow-x-auto">
      <table
        aria-label={label}
        className="world-admin-table w-full border-collapse text-left text-table"
      >
        <thead>
          <tr>
            {headings.map((heading, i) => (
              <th
                key={i}
                scope="col"
                className={cn(
                  "text-caption font-normal color-muted",
                  numeric.includes(i) && "text-right"
                )}
              >
                {heading}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  )
}

export function AdminRowMenu({
  label,
  action,
  destructiveLabel,
  onRemove,
  disabled,
}: {
  label: string
  action?: { label: string; run(): void }
  destructiveLabel: string
  onRemove(): void
  disabled?: boolean
}) {
  const { t } = useTranslation()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        aria-label={t("worldAdmin.rowMenu", { name: label })}
        render={
          <Button size="icon-sm" variant="ghost" className="color-muted" />
        }
      >
        <EllipsisIcon />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-max">
        {action && (
          <>
            <DropdownMenuItem className="text-ui" onClick={action.run}>
              {action.label}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
          </>
        )}
        <DropdownMenuItem
          className="text-ui"
          variant="destructive"
          disabled={disabled}
          onClick={onRemove}
        >
          {destructiveLabel}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

export function AdminAddButton({
  label,
  onClick,
  secondary,
}: {
  label: string
  onClick(): void
  secondary?: boolean
}) {
  return (
    <Button
      variant={secondary ? "outline" : "default"}
      className="text-ui"
      aria-label={label}
      onClick={onClick}
    >
      <PlusIcon data-icon="inline-start" />
      <span className="hidden sm:inline">{label}</span>
    </Button>
  )
}

export function AdminTags({ tags }: { tags: string[] | null }) {
  return (
    <div className="flex flex-wrap gap-1 empty:hidden">
      {tags?.map((tag) => (
        <Badge key={tag} variant="secondary" className="text-caption">
          {tag}
        </Badge>
      ))}
    </div>
  )
}
