import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useMemo, useState } from "react"
import { useTranslation } from "react-i18next"
import { ListEmpty } from "@/components/base/list-empty"
import { TokenNumberValue } from "@/components/base/token-number-value"
import { ListSearch } from "@/components/base/list-search"
import { RequestError } from "@/components/base/request-error"
import { Skeleton } from "@/components/ui/skeleton"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { LatticeLoader } from "@/components/base/lattice-loader"
import type { ProviderModelConfig } from "@/lib/api/protocol.gen"
import { groupProviderModels } from "@/lib/provider-form"

type ProviderModels = Record<string, ProviderModelConfig | null>

// 拉取模型弹窗：models 是上游或模型目录给的模型（加载中为 undefined），按厂商分组、可搜索、整组勾选，
// 已配置的模型先勾上；确定时交出弹窗里勾选的模型 id，由表单合并进模型列表。拉取失败时弹窗里显示原因和重试
export function FetchModelsDialog({
  open,
  title,
  models,
  error,
  shouldShowError,
  configuredModelIDs,
  onRetry,
  onConfirm,
  onOpenChange,
}: {
  open: boolean
  title: string
  models: ProviderModels | undefined
  error: unknown
  shouldShowError?: (error: unknown) => boolean
  configuredModelIDs: string[]
  onRetry(): void
  onConfirm(selectedModelIDs: string[]): void
  onOpenChange(open: boolean): void
}) {
  const { t } = useTranslation()
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[calc(100svh-2rem)] flex-col text-body sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="text-section">{title}</DialogTitle>
        </DialogHeader>
        {error && (shouldShowError?.(error) ?? true) ? (
          <RequestError error={error} onRetry={onRetry} />
        ) : error && !models ? (
          <Skeleton className="h-28 w-full" />
        ) : !models ? (
          <div className="flex items-center gap-2 py-2 text-caption color-muted">
            <LatticeLoader aria-hidden />
            {t("settings.fetchModels.loading")}
          </div>
        ) : (
          <FetchedModelPicker
            models={models}
            configuredModelIDs={configuredModelIDs}
            onConfirm={onConfirm}
            onCancel={() => onOpenChange(false)}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

// 拿到模型后的勾选列表；每次拿到新结果时重新挂载，勾选从已配置的模型开始
function FetchedModelPicker({
  models,
  configuredModelIDs,
  onConfirm,
  onCancel,
}: {
  models: ProviderModels
  configuredModelIDs: string[]
  onConfirm(selectedModelIDs: string[]): void
  onCancel(): void
}) {
  const { t } = useTranslation()
  const configured = new Set(configuredModelIDs)
  const [selected, setSelected] = useState(
    () => new Set(Object.keys(models).filter((id) => configured.has(id)))
  )
  const [keyword, setKeyword] = useState("")
  const groups = useMemo(() => groupProviderModels(models), [models])
  // 1. 按 id、上游 id 和显示名过滤，滤空的组不显示。
  const query = keyword.trim().toLowerCase()
  const visibleGroups = groups
    .map((group) => ({
      ...group,
      modelIDs: group.modelIDs.filter((id) =>
        [id, models[id]?.upstream_model, models[id]?.display_name]
          .join(" ")
          .toLowerCase()
          .includes(query)
      ),
    }))
    .filter((group) => group.modelIDs.length > 0)
  // 2. 统计确定后会新增和移除的模型数。
  const modelIDs = Object.keys(models)
  const addedCount = modelIDs.filter(
    (id) => selected.has(id) && !configured.has(id)
  ).length
  const removedCount = modelIDs.filter(
    (id) => !selected.has(id) && configured.has(id)
  ).length

  function toggle(ids: string[], checked: boolean) {
    const next = new Set(selected)
    for (const id of ids) {
      if (checked) next.add(id)
      else next.delete(id)
    }
    setSelected(next)
  }

  if (modelIDs.length === 0)
    return (
      <>
        <ListEmpty title={t("settings.fetchModels.empty")} />
        <DialogFooter>
          <Button className="text-ui" variant="outline" onClick={onCancel}>
            {t("common.actions.cancel")}
          </Button>
        </DialogFooter>
      </>
    )
  return (
    <>
      <form
        role="search"
        onSubmit={(event) => {
          // 弹窗挂在页面表单外面，搜索提交不往上冒泡
          event.preventDefault()
          event.stopPropagation()
          setKeyword(String(new FormData(event.currentTarget).get("keyword")))
        }}
      >
        <ListSearch
          keyword={keyword}
          aria-label={t("settings.fetchModels.search")}
          placeholder={t("settings.fetchModels.search")}
          autoComplete="off"
          debounceMs={150}
          onKeywordChange={setKeyword}
          onClear={() => setKeyword("")}
        />
      </form>
      <div className="min-h-0 flex-1 overflow-y-auto border-y">
        {visibleGroups.length === 0 ? (
          <ListEmpty
            filtered
            title={t("settings.fetchModels.noMatch")}
            noMatchTitle={t("settings.fetchModels.noMatch")}
            onClearFilters={() => setKeyword("")}
          />
        ) : (
          visibleGroups.map((group) => {
            const groupName = group.name || t("settings.fetchModels.other")
            const selectedCount = group.modelIDs.filter((id) =>
              selected.has(id)
            ).length
            return (
              <section
                key={group.name}
                aria-label={groupName}
                className="flex flex-col py-1"
              >
                <label className="sticky top-0 z-10 flex min-h-8 items-center gap-2 bg-popover px-2 py-1 text-ui font-medium">
                  <Checkbox
                    checked={selectedCount === group.modelIDs.length}
                    // 只勾了一部分时画一条横线，官方 Checkbox 的指示器只有对勾
                    className="data-indeterminate:border-primary data-indeterminate:bg-primary data-indeterminate:text-primary-foreground data-indeterminate:before:h-0.5 data-indeterminate:before:w-2 data-indeterminate:before:bg-current data-indeterminate:[&_svg]:hidden"
                    indeterminate={
                      selectedCount > 0 && selectedCount < group.modelIDs.length
                    }
                    aria-label={t("settings.fetchModels.selectGroup", {
                      name: groupName,
                    })}
                    onCheckedChange={(checked: boolean) =>
                      toggle(group.modelIDs, checked)
                    }
                  />
                  <span className="min-w-0 truncate">{groupName}</span>
                  <span className="ml-auto shrink-0 text-caption font-normal color-muted">
                    {selectedCount}/{group.modelIDs.length}
                  </span>
                </label>
                <ul className="flex flex-col">
                  {group.modelIDs.map((id) => {
                    const config = models[id]
                    return (
                      <li key={id}>
                        <div className="flex min-h-8 min-w-0 items-center gap-2 py-1 pr-2 pl-6 text-table">
                          <label className="flex min-w-0 flex-1 items-center gap-2">
                            <Checkbox
                              checked={selected.has(id)}
                              onCheckedChange={(checked: boolean) =>
                                toggle([id], checked)
                              }
                            />
                            <OverflowTooltip
                              render={<span className="min-w-0 truncate" />}
                            >
                              {id}
                            </OverflowTooltip>
                            {config?.display_name &&
                              config.display_name !== id && (
                                <OverflowTooltip
                                  render={
                                    <span className="min-w-0 truncate color-muted max-sm:hidden" />
                                  }
                                >
                                  {config.display_name}
                                </OverflowTooltip>
                              )}
                          </label>
                          {!!config?.context_window && (
                            <TokenNumberValue
                              value={config.context_window}
                              className="ml-auto shrink-0 color-muted"
                            />
                          )}
                        </div>
                      </li>
                    )
                  })}
                </ul>
              </section>
            )
          })
        )}
      </div>
      <DialogFooter className="flex-col sm:flex-row sm:items-center sm:justify-between">
        <div className="flex flex-wrap gap-2 text-caption color-muted">
          <span>{t("settings.fetchModels.added", { count: addedCount })}</span>
          <span>
            {t("settings.fetchModels.removed", { count: removedCount })}
          </span>
        </div>
        <div className="flex justify-end gap-2">
          <Button className="text-ui" variant="outline" onClick={onCancel}>
            {t("common.actions.cancel")}
          </Button>
          <Button className="text-ui" onClick={() => onConfirm([...selected])}>
            {t("settings.fetchModels.confirm")}
          </Button>
        </div>
      </DialogFooter>
    </>
  )
}
