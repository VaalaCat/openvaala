import {
  useWorldAdminFilters,
  matchesWorldAdminSearch,
} from "@/hooks/use-world-admin-filters"
import { useId, useState, useEffect } from "react"
import { useQuery, type UseMutationResult } from "@tanstack/react-query"
import { ArrowDownIcon, ArrowRightIcon, ArrowUpIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { PageActionsTargetContext } from "@/components/base/page-layout"
import { Badge } from "@/components/ui/badge"
import {
  AdminAddButton,
  AdminRowMenu,
  AdminSection,
  AdminTable,
  AdminSearch,
} from "./world-admin-table"
import { WorldMatrix } from "./world-admin-matrix"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ListEmpty } from "@/components/base/list-empty"
import { RequestError } from "@/components/base/request-error"
import { SettingsDraftActions } from "@/components/settings/settings-layout"
import { Button } from "@/components/ui/button"
import {
  Combobox,
  ComboboxChip,
  ComboboxChips,
  ComboboxChipsInput,
  ComboboxContent,
  ComboboxItem,
  ComboboxList,
  useComboboxAnchor,
} from "@/components/ui/combobox"
import {
  getWorldACL,
  type WorldAdminConnection,
  type WorldACLRule,
} from "@/lib/api/world-admin"
import {
  adminQueryKey,
  worldAdminUsersOptions,
  worldMatrixOptions,
} from "@/lib/query/world-admin"
import { cn } from "@/lib/utils"
import { AdminSelect } from "./world-admin-filters"

function RuleSelectors({
  label,
  value,
  candidates,
  matching,
  onChange,
}: {
  label: string
  value: string[]
  candidates: string[]
  matching: Set<string>
  onChange: (selectors: string[]) => void
}) {
  const { t } = useTranslation()
  const anchor = useComboboxAnchor()
  const [input, setInput] = useState("")
  const items =
    input.trim() && !candidates.includes(input.trim())
      ? [input.trim(), ...candidates]
      : candidates
  return (
    <div className="min-w-0">
      <Combobox
        multiple
        items={items}
        value={value}
        inputValue={input}
        onInputValueChange={setInput}
        onValueChange={(v) => {
          onChange(v)
          setInput("")
        }}
        autoHighlight
      >
        <ComboboxChips ref={anchor} className="min-w-0 text-ui">
          {value.map((v) => (
            <ComboboxChip
              key={v}
              className={cn(
                "max-w-full text-code",
                !matching.has(v) && "color-muted"
              )}
            >
              <span className="truncate">{v}</span>
            </ComboboxChip>
          ))}
          <ComboboxChipsInput
            aria-label={label}
            placeholder={value.length ? undefined : label}
            className="w-12 min-w-8 text-ui"
          />
        </ComboboxChips>
        <ComboboxContent anchor={anchor}>
          <ComboboxList>
            {(v: string) => (
              <ComboboxItem key={v} value={v} className="text-code">
                {v}
              </ComboboxItem>
            )}
          </ComboboxList>
        </ComboboxContent>
      </Combobox>
      {value.some((v) => !matching.has(v)) && (
        <span className="text-caption color-muted">
          {t("worldAdmin.unmatched")}
        </span>
      )}
    </div>
  )
}

export function WorldRules({
  connection,
  draft,
  setDraft,
  save,
}: {
  connection: WorldAdminConnection
  save: UseMutationResult<void, Error, WorldACLRule[]>
  draft: WorldACLRule[] | undefined
  setDraft: (rules: WorldACLRule[] | undefined) => void
}) {
  const { t } = useTranslation()
  const id = useId()
  const { search, update } = useWorldAdminFilters()
  const saved = useQuery({
    queryKey: adminQueryKey(connection, "acl"),
    queryFn: ({ signal }) => getWorldACL(connection, signal),
  })
  const users = useQuery(worldAdminUsersOptions(connection))
  const baseline = useQuery(worldMatrixOptions(connection))
  const preview = useQuery({
    ...worldMatrixOptions(connection, draft),
    enabled: draft !== undefined,
    gcTime: 0,
  })
  const [selected, setSelected] = useState<number>()
  const [editing, setEditing] = useState<number>()
  const [deleting, setDeleting] = useState<number>()
  const [actions, setActions] = useState<HTMLDivElement | null>(null)
  useEffect(() => {
    if (editing !== undefined)
      document.getElementById(id + "-" + editing)?.focus()
  }, [id, editing])
  const savedRules = saved.data ?? []
  const rules = draft ?? savedRules
  function edit(rules: WorldACLRule[]) {
    const numbered = rules.map((r, i) => ({ ...r, priority: (i + 1) * 10 }))
    setDraft(
      !save.isPending &&
        rules.length === savedRules.length &&
        rules.every((rule, i) => {
          const base = savedRules[i]
          return (
            rule.action === base.action &&
            JSON.stringify(rule.from) === JSON.stringify(base.from) &&
            JSON.stringify(rule.to) === JSON.stringify(base.to)
          )
        })
        ? undefined
        : numbered
    )
    setSelected(undefined)
    if (save.isError) save.reset()
  }
  function move(from: number, to: number) {
    const next = [...rules]
    next.splice(to, 0, ...next.splice(from, 1))
    setEditing(undefined)
    edit(next)
  }
  function select(index: number) {
    setEditing(undefined)
    if (!document.getElementById(id + "-" + index)) update("adminSearch", "")
    setSelected(index)
  }
  useEffect(() => {
    if (selected !== undefined)
      document
        .getElementById(id + "-" + selected)
        ?.scrollIntoView({ block: "nearest" })
  }, [id, selected])
  const nodes = baseline.data?.nodes ?? []
  const matching = new Set(
    nodes.flatMap((n) => [
      "*",
      "owner:*",
      "node:" + n.name,
      "owner:" + n.owner,
      ...n.tags.map((tag) => "tag:" + tag),
    ])
  )
  const candidates = [
    ...new Set([
      "*",
      "owner:*",
      ...Array.from(matching),
      ...(users.data ?? []).map((u) => "owner:" + u.owner),
    ]),
  ].sort()
  const ruleOwner = search.get("ruleOwner")
  const matrix = draft === undefined ? baseline : preview
  const filtered = rules
    .map((rule, i) => ({ rule, i }))
    .filter(
      ({ rule, i }) =>
        i === editing ||
        matchesWorldAdminSearch(search, [
          ...rule.from,
          ...rule.to,
          t(rule.action === "allow" ? "worldAdmin.allow" : "worldAdmin.deny"),
        ])
    )
  return (
    <div className="flex min-w-0 flex-col gap-6">
      <AdminSection
        title={t("worldAdmin.acl")}
        description={t("worldAdmin.rulesDescription")}
        actions={
          <div className="flex items-center gap-2">
            <div ref={setActions} className="contents" />
            {saved.data && (
              <AdminAddButton
                label={t("worldAdmin.addRule")}
                secondary={!!draft}
                onClick={() => {
                  if (search.has("adminSearch")) update("adminSearch", "")
                  edit([
                    ...rules,
                    { priority: 0, from: [], to: [], action: "allow" },
                  ])
                  setEditing(rules.length)
                }}
              />
            )}
          </div>
        }
      >
        {draft && (
          <PageActionsTargetContext.Provider value={actions}>
            <SettingsDraftActions
              dirty
              saving={save.isPending}
              saveDisabled={!preview.isSuccess}
              saveLabel={t("worldAdmin.saveACL")}
              revertLabel={t("worldSettings.discard")}
              onSave={() => save.mutate(draft)}
              onRevert={() => {
                setDraft(undefined)
                setEditing(undefined)
                setSelected(undefined)
                save.reset()
              }}
            />
          </PageActionsTargetContext.Provider>
        )}
        {save.isError && <RequestError error={save.error} />}
        {saved.isError && (
          <RequestError
            error={saved.error}
            onRetry={() => void saved.refetch()}
          />
        )}
        {users.isError && (
          <RequestError
            error={users.error}
            onRetry={() => void users.refetch()}
          />
        )}
        {saved.isPending && <LatticeLoader />}
        <AdminSearch placeholder={t("worldAdmin.searchRules")} />
        {saved.data && (
          <AdminTable
            label={t("worldAdmin.acl")}
            headings={[
              "#",
              t("worldAdmin.action"),
              t("worldAdmin.source"),
              "",
              t("worldAdmin.destination"),
              t("worldAdmin.order"),
              "",
            ]}
          >
            {filtered.map(({ rule, i }) => {
              const active = editing === i
              const highlight =
                selected === i ||
                (selected === undefined &&
                  !!ruleOwner &&
                  [...rule.from, ...rule.to].includes("owner:" + ruleOwner))
              const change = (patch: Partial<WorldACLRule>) =>
                edit(rules.map((r, j) => (i === j ? { ...r, ...patch } : r)))
              const selectors = (key: "from" | "to") =>
                active ? (
                  <RuleSelectors
                    label={t(
                      key === "from" ? "worldAdmin.from" : "worldAdmin.to"
                    )}
                    candidates={candidates}
                    matching={matching}
                    value={rule[key]}
                    onChange={(v) => change({ [key]: v })}
                  />
                ) : (
                  <div className="flex flex-wrap items-center gap-1">
                    {rule[key].map((v) => (
                      <Badge
                        key={v}
                        variant={matching.has(v) ? "secondary" : "outline"}
                        className={cn(
                          "text-code",
                          !matching.has(v) && "color-muted"
                        )}
                      >
                        {v}
                      </Badge>
                    ))}
                    {rule[key].some((v) => !matching.has(v)) && (
                      <span className="text-caption color-muted">
                        {t("worldAdmin.unmatched")}
                      </span>
                    )}
                  </div>
                )
              return (
                <tr
                  key={i}
                  id={id + "-" + i}
                  tabIndex={-1}
                  data-highlighted={highlight}
                  className={cn(
                    "cursor-pointer focus:outline-none",
                    highlight && "bg-accent"
                  )}
                  onClick={(event) => {
                    if (
                      !(event.target instanceof Element) ||
                      event.target.closest(
                        "button, [role=menuitem], [role=option], [data-slot=dropdown-menu-content]"
                      )
                    )
                      return
                    setEditing(i)
                  }}
                  onBlur={(event) => {
                    const next = event.relatedTarget
                    if (
                      active &&
                      !event.currentTarget.contains(next) &&
                      !(
                        next instanceof Element &&
                        next.closest(
                          "[data-slot=select-content], [data-slot=combobox-content]"
                        )
                      )
                    )
                      setEditing(undefined)
                  }}
                  onKeyDownCapture={(event) => {
                    if (active && event.key === "Escape") {
                      event.preventDefault()
                      event.stopPropagation()
                      setEditing(undefined)
                      event.currentTarget.querySelector("button")?.focus()
                    }
                  }}
                >
                  <td className="w-10 text-code color-muted">
                    <button
                      aria-label={t("worldAdmin.editRule", { n: i + 1 })}
                      onClick={() => setEditing(i)}
                    >
                      #{i + 1}
                    </button>
                  </td>
                  <td>
                    {active ? (
                      <AdminSelect
                        label={t("worldAdmin.action")}
                        value={rule.action}
                        options={[
                          { value: "allow", label: t("worldAdmin.allow") },
                          { value: "deny", label: t("worldAdmin.deny") },
                        ]}
                        onChange={(action) => change({ action })}
                      />
                    ) : (
                      <Badge
                        variant={
                          rule.action === "allow" ? "default" : "outline"
                        }
                        className={cn(
                          "text-caption",
                          rule.action === "deny" &&
                            "border-destructive/40 color-destructive"
                        )}
                      >
                        {t(
                          rule.action === "allow"
                            ? "worldAdmin.allow"
                            : "worldAdmin.deny"
                        )}
                      </Badge>
                    )}
                  </td>
                  <td className="w-1/3 min-w-32">{selectors("from")}</td>
                  <td className="w-6">
                    <ArrowRightIcon
                      aria-hidden
                      className="size-3.5 color-muted"
                    />
                  </td>
                  <td className="w-1/3 min-w-32">{selectors("to")}</td>
                  <td className="w-16">
                    <div className="flex">
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        disabled={!i}
                        aria-label={t("worldAdmin.up")}
                        onClick={() => move(i, i - 1)}
                      >
                        <ArrowUpIcon />
                      </Button>
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        disabled={i === rules.length - 1}
                        aria-label={t("worldAdmin.down")}
                        onClick={() => move(i, i + 1)}
                      >
                        <ArrowDownIcon />
                      </Button>
                    </div>
                  </td>
                  <td className="text-right">
                    <AdminRowMenu
                      label={"#" + (i + 1)}
                      destructiveLabel={t("worldAdmin.deleteRule")}
                      onRemove={() => setDeleting(i)}
                    />
                  </td>
                </tr>
              )
            })}
            {search.has("adminSearch") && !filtered.length && (
              <tr>
                <td colSpan={7}>
                  <ListEmpty
                    filtered
                    title={t("worldAdmin.acl")}
                    noMatchTitle={t("worldAdmin.noRules")}
                    onClearFilters={() => update("adminSearch", "")}
                  />
                </td>
              </tr>
            )}
            <tr
              id={id + "--1"}
              data-highlighted={selected === -1}
              className={cn("color-muted", selected === -1 && "bg-accent")}
            >
              <td>—</td>
              <td colSpan={6} className="text-caption">
                {t("worldAdmin.defaultRule")}
              </td>
            </tr>
          </AdminTable>
        )}
        <ConfigurationChangeDialog
          destructive
          open={deleting !== undefined}
          title={t("worldAdmin.deleteRuleTitle", { n: (deleting ?? 0) + 1 })}
          confirmLabel={t("worldAdmin.deleteRule")}
          pending={false}
          onConfirm={() => {
            edit(rules.filter((_, i) => i !== deleting))
            setDeleting(undefined)
            setEditing(undefined)
          }}
          onOpenChange={(v) => {
            if (!v) setDeleting(undefined)
          }}
        />
      </AdminSection>
      {baseline.isError && (
        <RequestError
          error={baseline.error}
          onRetry={() => void baseline.refetch()}
        />
      )}
      {draft && preview.isError && (
        <RequestError
          error={preview.error}
          onRetry={() => void preview.refetch()}
        />
      )}
      {matrix.isPending && <LatticeLoader aria-label={t("common.loading")} />}
      {baseline.data && matrix.data && (
        <WorldMatrix
          baseline={baseline.data}
          matrix={matrix.data}
          dirty={!!draft}
          onSelect={select}
        />
      )}
    </div>
  )
}
