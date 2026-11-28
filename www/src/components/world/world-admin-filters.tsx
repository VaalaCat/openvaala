import { useWorldAdminFilters } from "@/hooks/use-world-admin-filters"
import { useTranslation } from "react-i18next"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

export function AdminSelect({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: string
  options: { value: string; label: string }[]
  onChange: (value: string) => void
}) {
  return (
    <Select
      value={value}
      items={options}
      onValueChange={(v) => {
        if (v !== null) onChange(v)
      }}
    >
      <SelectTrigger aria-label={label} className="min-w-0 text-ui">
        <SelectValue className="truncate" />
      </SelectTrigger>
      <SelectContent alignItemWithTrigger={false}>
        {options.map((v) => (
          <SelectItem key={v.value} value={v.value} className="text-table">
            {v.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

export function WorldAdminFilters({
  items,
  statusKey,
  statuses,
}: {
  items: { owner: string; tags: string[] | null }[]
  statusKey?: "online" | "status"
  statuses?: { value: string; label: string }[]
}) {
  const { t } = useTranslation()
  const { search, update } = useWorldAdminFilters()
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-2">
      {(["owner", "tag"] as const).map((key) => {
        const value = search.get(key) ?? ""
        const choices = new Set(
          key === "owner"
            ? items.map((n) => n.owner)
            : items.flatMap((n) => n.tags ?? [])
        )
        if (value) choices.add(value)
        const label =
          key === "owner" ? t("worldAdmin.allOwners") : t("worldAdmin.allTags")
        return (
          <AdminSelect
            key={key}
            label={label}
            value={value}
            options={[
              { value: "", label },
              ...Array.from(choices)
                .sort()
                .map((v) => ({ value: v, label: v })),
            ]}
            onChange={(v) => update(key, v)}
          />
        )
      })}
      {statusKey && statuses && (
        <AdminSelect
          label={t("worldAdmin.status")}
          value={search.get(statusKey) ?? ""}
          options={[
            { value: "", label: t("worldAdmin.allStatuses") },
            ...statuses,
          ]}
          onChange={(v) => update(statusKey, v)}
        />
      )}
    </div>
  )
}
