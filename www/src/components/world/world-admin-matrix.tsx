import { useTranslation } from "react-i18next"
import { ListEmpty } from "@/components/base/list-empty"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import {
  matchesWorldAdminFilters,
  useWorldAdminFilters,
} from "@/hooks/use-world-admin-filters"
import type { ACLMatrix } from "@/lib/api/protocol.gen"
import { cn } from "@/lib/utils"
import { WorldAdminFilters } from "./world-admin-filters"

const allowedFill = "bg-foreground"
const deniedFill = "bg-muted ring-1 ring-inset ring-border"
const changedOutline = "outline-2 -outline-offset-2 outline-warning"

export function WorldMatrix({
  baseline,
  matrix,
  dirty,
  onSelect,
}: {
  baseline: ACLMatrix
  matrix: ACLMatrix
  dirty: boolean
  onSelect(index: number): void
}) {
  const { t } = useTranslation()
  const { search, open } = useWorldAdminFilters()
  const savedIndices = new Map(baseline.nodes.map((node, i) => [node.name, i]))
  const indices = matrix.nodes.map((n) => savedIndices.get(n.name))
  // 对照完整矩阵，再筛选要展示的行列。
  const changes = dirty
    ? matrix.cells.map((row, i) =>
        row.map((cell, j) => {
          const a = indices[i],
            b = indices[j]
          if (i === j || a === undefined || b === undefined) return undefined
          const prev = baseline.cells[a][b]
          return prev.allowed !== cell.allowed ||
            prev.rule_index !== cell.rule_index
            ? prev
            : undefined
        })
      )
    : []
  const visible = matrix.nodes
    .map((node, index) => ({ ...node, index }))
    .filter((n) => matchesWorldAdminFilters(n, search))
    .sort(
      (a, b) => a.owner.localeCompare(b.owner) || a.name.localeCompare(b.name)
    )
  const byOwner = new Map<string, typeof visible>()
  for (const node of visible) {
    const group = byOwner.get(node.owner)
    if (group) group.push(node)
    else byOwner.set(node.owner, [node])
  }
  const groups = Array.from(byOwner)
  const action = (allowed: boolean) =>
    t(allowed ? "worldAdmin.allow" : "worldAdmin.deny")
  const rule = (index: number) =>
    index === -1
      ? t("worldAdmin.defaultName")
      : t("worldAdmin.ruleNumber", { n: index + 1 })
  return (
    <section aria-label={t("worldAdmin.matrix")} className="min-w-0">
      <header className="mb-3 flex flex-wrap items-center gap-3">
        <h3 className="mr-auto text-section">{t("worldAdmin.matrix")}</h3>
        {dirty && (
          <span className="text-caption color-muted">
            {t("worldAdmin.changes", {
              n: changes.flat().filter(Boolean).length,
            })}
          </span>
        )}
        <WorldAdminFilters items={matrix.nodes} />
      </header>
      {visible.length === 0 ? (
        <ListEmpty
          title={t("worldAdmin.emptyNodes")}
          filtered={search.has("owner") || search.has("tag")}
          noMatchTitle={t("worldAdmin.noNodes")}
          onClearFilters={() => open("acl")}
        />
      ) : (
        <>
          <div className="max-w-full overflow-x-auto pb-1">
            <table
              aria-label={t("worldAdmin.matrix")}
              className="border-separate border-spacing-[2px] pr-20 text-caption"
            >
              <thead>
                <tr>
                  <th
                    colSpan={2}
                    rowSpan={2}
                    className="pr-3 text-left align-top font-normal whitespace-nowrap color-muted"
                  >
                    {t("worldAdmin.axes")}
                  </th>
                  {groups.map(([owner, nodes], g) => (
                    <th
                      key={owner}
                      scope="colgroup"
                      colSpan={nodes.length}
                      className={cn(
                        "align-bottom font-normal color-muted",
                        g > 0 && "pl-1.5"
                      )}
                    >
                      <span className="block w-0 min-w-full border-b pb-1 text-left">
                        <span
                          title={owner}
                          className="block w-[calc(100%+6px)] truncate"
                        >
                          {owner}
                        </span>
                      </span>
                    </th>
                  ))}
                </tr>
                <tr>
                  {visible.map((node, j) => (
                    <th
                      scope="col"
                      key={node.name}
                      className={cn(
                        "align-bottom font-normal",
                        j > 0 && node.owner !== visible[j - 1].owner && "pl-1.5"
                      )}
                    >
                      <div className="relative w-5 pt-4">
                        <span
                          aria-hidden
                          className="invisible block max-h-28 truncate text-code [writing-mode:vertical-rl]"
                        >
                          {node.name}
                        </span>
                        <span
                          title={node.name}
                          className="absolute bottom-0.5 left-4 w-max max-w-28 origin-bottom-left -rotate-45 truncate text-code"
                        >
                          {node.name}
                        </span>
                      </div>
                    </th>
                  ))}
                </tr>
              </thead>
              {groups.map(([owner, nodes], g) => (
                <tbody key={owner}>
                  {nodes.map((from, n) => (
                    <tr key={from.name}>
                      {n === 0 && (
                        <th
                          scope="rowgroup"
                          rowSpan={nodes.length}
                          className={cn(
                            "pr-3 text-left align-top font-normal color-muted",
                            g > 0 && "pt-1.5"
                          )}
                        >
                          {owner}
                        </th>
                      )}
                      <th
                        scope="row"
                        className={cn(
                          "pr-2 text-right text-code font-normal",
                          g > 0 && n === 0 && "pt-1.5"
                        )}
                      >
                        {from.name}
                      </th>
                      {visible.map((to, j) => {
                        const cell = matrix.cells[from.index][to.index]
                        const prev = changes[from.index]?.[to.index]
                        const explanation = t("worldAdmin.cell", {
                          from: from.name,
                          to: to.name,
                          action:
                            prev && prev.allowed !== cell.allowed
                              ? action(prev.allowed) +
                                " ⇒ " +
                                action(cell.allowed)
                              : action(cell.allowed),
                          rule:
                            prev && prev.allowed === cell.allowed
                              ? rule(prev.rule_index) +
                                " ⇒ " +
                                rule(cell.rule_index)
                              : rule(cell.rule_index),
                        })
                        return (
                          <td
                            key={to.name}
                            className={cn(
                              g > 0 && n === 0 && "pt-1.5",
                              j > 0 &&
                                to.owner !== visible[j - 1].owner &&
                                "pl-1.5"
                            )}
                          >
                            {from.index === to.index ? (
                              <span className="grid size-5 place-items-center color-muted">
                                ·
                              </span>
                            ) : (
                              <Tooltip>
                                <TooltipTrigger
                                  render={
                                    <button
                                      type="button"
                                      aria-label={explanation}
                                      onClick={() => onSelect(cell.rule_index)}
                                    />
                                  }
                                  className={cn(
                                    "grid size-5 cursor-pointer place-items-center rounded-[2px] hover:brightness-90",
                                    cell.allowed ? allowedFill : deniedFill,
                                    prev && changedOutline
                                  )}
                                >
                                  {prev && (
                                    <span
                                      className={cn(
                                        "size-1 rounded-full",
                                        cell.allowed
                                          ? "bg-background"
                                          : "bg-foreground"
                                      )}
                                    />
                                  )}
                                </TooltipTrigger>
                                <TooltipContent
                                  role="tooltip"
                                  side="right"
                                  align="start"
                                  className="max-w-36 text-caption wrap-anywhere sm:max-w-xs [&>[data-side=left]]:top-2.5! [&>[data-side=right]]:top-2.5!"
                                >
                                  {explanation}
                                </TooltipContent>
                              </Tooltip>
                            )}
                          </td>
                        )
                      })}
                    </tr>
                  ))}
                </tbody>
              ))}
            </table>
          </div>
          <div className="mt-3 flex flex-wrap items-center gap-4 text-caption color-muted">
            {[
              [allowedFill, t("worldAdmin.allow")],
              [deniedFill, t("worldAdmin.deny")],
              [changedOutline, t("worldAdmin.changed")],
            ].map(([style, label]) => (
              <span key={label} className="inline-flex items-center gap-1.5">
                <span
                  aria-hidden
                  className={cn("size-3 rounded-[2px]", style)}
                />
                {label}
              </span>
            ))}
          </div>
        </>
      )}
    </section>
  )
}
