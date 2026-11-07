import { useIsMobile } from "@/hooks/use-mobile"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useMemo, useRef, useState } from "react"
import { motion } from "motion/react"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { CheckIcon, ChevronDownIcon, XIcon } from "lucide-react"
import {
  Drawer,
  DrawerTrigger,
  DrawerContent,
  DrawerTitle,
} from "@/components/ui/drawer"
import {
  Popover,
  PopoverTrigger,
  PopoverContent,
  PopoverTitle,
} from "@/components/ui/popover"
import { Input } from "@/components/ui/input"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { RequestError } from "@/components/base/request-error"
import { Skeleton } from "@/components/ui/skeleton"
import { TokenNumberValue } from "@/components/base/token-number-value"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Combobox,
  ComboboxCollection,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxGroup,
  ComboboxInput,
  ComboboxItem,
  ComboboxLabel,
  ComboboxList,
  ComboboxTrigger,
} from "@/components/ui/combobox"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectGroup,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { InputGroupInput } from "@/components/ui/input-group"
import type {
  ModelDefinition,
  ModelRef,
  ModelSelection,
} from "@/lib/api/protocol.gen"
import { modelListOptions } from "@/lib/query/model"
import { shouldShowLoadError } from "@/lib/rpc-error"
import { cn, formatModelRef } from "@/lib/utils"

// ModelGroup 是一个 Provider 的模型，按 model/list 里首次出现的顺序
interface ModelGroup {
  value: string
  items: ModelDefinition[]
}

// 选模型和推理强度：模型按 Provider 分组、可搜索；选中的模型支持推理强度时旁边多一个强度选择框。
// value 为空或没有 ref 时显示 Agent 默认模型；换模型时新模型支持原来的强度就保留，否则用新模型的默认强度。
// 只选模型、不带强度（设置页的默认模型）时传 showReasoningEffort={false}
export function ModelPicker({
  agentName,
  value,
  onChange,
  variant = "ghost",
  showReasoningEffort = true,
  compact = false,
  disabled = false,
}: {
  compact?: boolean
  disabled?: boolean
  agentName?: string
  value?: ModelSelection
  onChange(selection: ModelSelection): void
  // 顶栏用 ghost，表单里用 outline；浮层跟着分别右对齐、左对齐
  variant?: "ghost" | "outline"
  showReasoningEffort?: boolean
}) {
  const { t } = useTranslation()
  const isMobile = useIsMobile()
  const [compactOpen, setCompactOpen] = useState(false)
  const [modelOpen, setModelOpen] = useState(false)
  const [effortOpen, setEffortOpen] = useState(false)
  const [search, setSearch] = useState("")
  const connection = useServerConnection()
  const triggerRef = useRef<HTMLButtonElement>(null)
  const searchRef = useRef<HTMLInputElement>(null)
  const models = useQuery({
    ...modelListOptions(connection, agentName ?? ""),
    enabled: agentName !== undefined,
  })
  const modelData = agentName === undefined ? undefined : models.data
  const isSelectionPending = agentName === undefined || models.isPending
  const selectionDisabled = disabled || isSelectionPending
  // 1. 目标尚未就绪或控件禁用时，关闭浮层并清空搜索。
  if (selectionDisabled && (compactOpen || modelOpen || effortOpen || search)) {
    setCompactOpen(false)
    setModelOpen(false)
    setEffortOpen(false)
    setSearch("")
  }
  const definitions = useMemo(
    () => (modelData?.models ?? []).flatMap((model) => (model ? [model] : [])),
    [modelData]
  )
  const groups = useMemo(
    () => groupModelsByProvider(definitions),
    [definitions]
  )
  const agentDefaultRef = modelData?.agent_default_model
  const selectedRef =
    agentName === undefined
      ? undefined
      : value?.ref.provider_id
        ? value.ref
        : agentDefaultRef
  const selected = definitions.find((model) =>
    isSameModel(model.ref, selectedRef)
  )
  const effort = selected
    ? getReasoningEffort(selected, value?.reasoning_effort)
    : undefined
  const label = selected
    ? getModelName(selected)
    : selectedRef?.provider_id
      ? formatModelRef(selectedRef)
      : isSelectionPending
        ? t("common.loading")
        : t("modelPicker.placeholder")
  // 2. 容器窄时合并入口；手机用抽屉，桌面用锚定浮层。
  if (compact || isMobile) {
    const filtered = groups
      .map((group) => ({
        ...group,
        items: group.items.filter((model) =>
          (getModelName(model) + " " + formatModelRef(model.ref))
            .toLowerCase()
            .includes(search.trim().toLowerCase())
        ),
      }))
      .filter((group) => group.items.length > 0)
    const trigger = (
      <Button
        type="button"
        variant={variant}
        className={cn(
          "model-selection-value max-w-full min-w-0 shrink gap-2 text-ui transition-colors",
          variant === "ghost" && "px-1"
        )}
        aria-label={t("modelPicker.model")}
        disabled={selectionDisabled}
      >
        <OverflowTooltip render={<span className="min-w-0 truncate" />}>
          {label}
        </OverflowTooltip>
        {showReasoningEffort && effort && (
          <span className="shrink-0 color-muted">{effort}</span>
        )}
        <ChevronDownIcon className="shrink-0" />
      </Button>
    )
    const onCompactOpenChange = (next: boolean) => {
      setCompactOpen(next)
      if (!next) setSearch("")
    }
    const content = (
      <>
        <div className="flex shrink-0 items-center justify-between gap-2 p-3">
          {isMobile ? (
            <DrawerTitle className="text-section">
              {t("modelPicker.model")}
            </DrawerTitle>
          ) : (
            <PopoverTitle className="text-section">
              {t("modelPicker.model")}
            </PopoverTitle>
          )}
          <Tooltip disabled={isMobile}>
            <TooltipTrigger
              render={
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={t("modelPicker.close")}
                  onClick={() => onCompactOpenChange(false)}
                />
              }
            >
              <XIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("modelPicker.close")}
            </TooltipContent>
          </Tooltip>
        </div>
        <div className="flex min-h-0 flex-col gap-2 px-3 pb-3">
          <Input
            ref={searchRef}
            type="search"
            className="shrink-0 text-ui"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            aria-label={t("modelPicker.search")}
            placeholder={t("modelPicker.search")}
          />
          {showReasoningEffort &&
            selected &&
            selected.reasoning_efforts.length > 0 && (
              <ToggleGroup
                disabled={selectionDisabled}
                className="max-w-full shrink-0 overflow-x-auto"
                aria-label={t("modelPicker.reasoningEffort", {
                  effort: effort || t("modelPicker.defaultEffort"),
                })}
                value={effort ? [effort] : []}
                onValueChange={(next) => {
                  if (next[0])
                    onChange({ ref: selected.ref, reasoning_effort: next[0] })
                }}
              >
                {selected.reasoning_efforts.map((each) => (
                  <ToggleGroupItem
                    key={each}
                    value={each}
                    className="model-selection-option text-ui"
                  >
                    {each}
                  </ToggleGroupItem>
                ))}
              </ToggleGroup>
            )}
          {models.isError && shouldShowLoadError(models.error) && (
            <RequestError
              error={models.error}
              onRetry={() => void models.refetch()}
            />
          )}
          {models.isPending && (
            <LatticeLoader aria-label={t("common.loading")} />
          )}
          {!models.data &&
            models.isError &&
            !shouldShowLoadError(models.error) && (
              <Skeleton className="h-28 w-full" />
            )}
          <div className="max-h-[50svh] min-h-0 overflow-y-auto">
            {filtered.map((group) => (
              <div key={group.value} className="flex flex-col gap-1 pb-2">
                <OverflowTooltip
                  render={
                    <p className="truncate px-2 py-1 text-caption color-muted" />
                  }
                >
                  {group.value}
                </OverflowTooltip>
                {group.items.map((model) => (
                  <Button
                    key={model.ref.model_id}
                    type="button"
                    variant="ghost"
                    className="model-selection-option w-full min-w-0 justify-start px-2 text-table"
                    aria-pressed={isSameModel(model.ref, selectedRef)}
                    disabled={selectionDisabled}
                    onClick={() =>
                      onChange({
                        ref: model.ref,
                        reasoning_effort:
                          value?.reasoning_effort &&
                          model.reasoning_efforts.includes(
                            value.reasoning_effort
                          )
                            ? value.reasoning_effort
                            : undefined,
                      })
                    }
                  >
                    <OverflowTooltip
                      render={
                        <span className="min-w-0 flex-1 truncate text-left" />
                      }
                    >
                      {getModelName(model)}
                    </OverflowTooltip>
                    {model.context_window > 0 && (
                      <TokenNumberValue
                        value={model.context_window}
                        interactive={false}
                        className="shrink-0 color-muted"
                      />
                    )}
                    <CheckIcon
                      aria-hidden
                      className={cn(
                        !isSameModel(model.ref, selectedRef) && "invisible"
                      )}
                    />
                  </Button>
                ))}
              </div>
            ))}
            {models.isSuccess && filtered.length === 0 && (
              <p className="py-2 text-caption color-muted">
                {definitions.length === 0
                  ? t("modelPicker.noModels")
                  : t("common.selection.empty")}
              </p>
            )}
          </div>
        </div>
      </>
    )
    return isMobile ? (
      <Drawer
        open={compactOpen && !selectionDisabled}
        onOpenChange={onCompactOpenChange}
      >
        <DrawerTrigger render={trigger} />
        <DrawerContent className="text-body">{content}</DrawerContent>
      </Drawer>
    ) : (
      <Popover
        open={compactOpen && !selectionDisabled}
        onOpenChange={onCompactOpenChange}
      >
        <PopoverTrigger render={trigger} />
        <PopoverContent
          initialFocus={searchRef}
          align={variant === "ghost" ? "end" : "start"}
          side="top"
          className="max-h-(--available-height) w-80 max-w-(--available-width) gap-0 p-0 text-body"
        >
          {content}
        </PopoverContent>
      </Popover>
    )
  }
  return (
    <div className="flex min-w-0 items-center">
      <Combobox<ModelDefinition>
        disabled={selectionDisabled}
        open={modelOpen && !selectionDisabled}
        onOpenChange={setModelOpen}
        items={groups}
        value={selected ?? null}
        onValueChange={(model) => {
          if (!model || selectionDisabled) return
          onChange({
            ref: model.ref,
            reasoning_effort:
              value?.reasoning_effort &&
              model.reasoning_efforts.includes(value.reasoning_effort)
                ? value.reasoning_effort
                : undefined,
          })
        }}
        itemToStringLabel={getModelName}
        isItemEqualToValue={(model, other) => isSameModel(model.ref, other.ref)}
        filter={(model, query) =>
          (getModelName(model) + " " + formatModelRef(model.ref))
            .toLowerCase()
            .includes(query.trim().toLowerCase())
        }
      >
        <ComboboxTrigger
          ref={triggerRef}
          aria-label={t("modelPicker.model")}
          render={
            <Button
              variant={variant}
              type="button"
              className={cn(
                "model-selection-value min-w-0 text-ui transition-colors",
                variant === "ghost" ? "w-auto max-w-48 px-1" : "w-28 md:w-48"
              )}
            />
          }
        >
          <OverflowTooltip
            render={<span className="min-w-0 flex-1 truncate text-left" />}
          >
            {label}
          </OverflowTooltip>
        </ComboboxTrigger>
        <ComboboxContent
          anchor={triggerRef}
          // 顶栏的选择器在右侧，浮层右对齐；表单里的在左侧，左对齐
          align={variant === "ghost" ? "end" : "start"}
          sideOffset={4}
          className="flex w-72 flex-col *:data-[slot=input-group]:shrink-0"
        >
          <ComboboxInput
            render={<InputGroupInput className="text-ui" />}
            showTrigger={false}
            aria-label={t("modelPicker.search")}
            placeholder={t("modelPicker.search")}
            className="[&_input]:pr-8"
          >
            {models.isFetching && (
              <LatticeLoader
                className="absolute right-2"
                aria-label={t("common.loading")}
              />
            )}
          </ComboboxInput>
          {models.isError && shouldShowLoadError(models.error) && (
            <RequestError
              error={models.error}
              onRetry={() => void models.refetch()}
            />
          )}
          {!models.data &&
            models.isError &&
            !shouldShowLoadError(models.error) && (
              <Skeleton className="h-28 w-full" />
            )}
          {models.isSuccess && (
            <ComboboxEmpty className="text-ui">
              {definitions.length === 0
                ? t("modelPicker.noModels")
                : t("common.selection.empty")}
            </ComboboxEmpty>
          )}
          <ComboboxList className="min-h-0 p-1">
            {(group: ModelGroup) => (
              <ComboboxGroup key={group.value} items={group.items}>
                <OverflowTooltip
                  data-slot="combobox-label"
                  render={
                    <ComboboxLabel className="min-w-0 truncate text-caption" />
                  }
                >
                  {group.value}
                </OverflowTooltip>
                <ComboboxCollection>
                  {(model: ModelDefinition) => (
                    <ComboboxItem
                      key={model.ref.model_id}
                      value={model}
                      className="model-selection-option h-8 min-w-0 py-1 text-table"
                    >
                      <OverflowTooltip
                        render={<span className="min-w-0 flex-1 truncate" />}
                      >
                        {getModelName(model)}
                      </OverflowTooltip>
                      {isSameModel(model.ref, agentDefaultRef) && (
                        <span className="shrink-0 text-caption color-muted">
                          {t("modelPicker.agentDefault")}
                        </span>
                      )}
                      {model.context_window > 0 && (
                        <TokenNumberValue
                          value={model.context_window}
                          interactive={false}
                          className="shrink-0 color-muted"
                        />
                      )}
                    </ComboboxItem>
                  )}
                </ComboboxCollection>
              </ComboboxGroup>
            )}
          </ComboboxList>
        </ComboboxContent>
      </Combobox>
      <motion.div
        initial={false}
        animate={{
          width:
            showReasoningEffort &&
            (isSelectionPending || !!selected?.reasoning_efforts.length)
              ? "auto"
              : 0,
          opacity:
            showReasoningEffort &&
            (isSelectionPending || !!selected?.reasoning_efforts.length)
              ? 1
              : 0,
        }}
        transition={{ duration: 0.2 }}
        className="shrink-0"
        inert={
          selectionDisabled ||
          !showReasoningEffort ||
          !selected?.reasoning_efforts.length
        }
        aria-hidden={
          !showReasoningEffort ||
          (!isSelectionPending && !selected?.reasoning_efforts.length)
        }
      >
        <div className={variant === "ghost" ? "pl-1" : "pl-2"}>
          <Select
            disabled={selectionDisabled || !selected?.reasoning_efforts.length}
            open={
              effortOpen &&
              !selectionDisabled &&
              !!selected?.reasoning_efforts.length
            }
            onOpenChange={setEffortOpen}
            value={effort || null}
            onValueChange={(next: string | null) => {
              if (next && selected && !selectionDisabled)
                onChange({ ref: selected.ref, reasoning_effort: next })
            }}
          >
            <SelectTrigger
              aria-label={t("modelPicker.reasoningEffort", {
                effort: effort || t("modelPicker.defaultEffort"),
              })}
              className={cn(
                "model-selection-value shrink-0 text-ui transition-colors",
                variant === "ghost"
                  ? "w-auto border-transparent px-1 hover:bg-muted aria-expanded:bg-muted dark:bg-transparent dark:hover:bg-muted/50"
                  : "w-28"
              )}
            >
              <SelectValue className="text-table">
                {isSelectionPending
                  ? t("common.loading")
                  : effort || t("modelPicker.defaultEffort")}
              </SelectValue>
            </SelectTrigger>
            <SelectContent alignItemWithTrigger={false} align="end">
              <SelectGroup>
                {selected?.reasoning_efforts.map((each) => (
                  <SelectItem
                    key={each}
                    value={each}
                    className="model-selection-option text-table"
                  >
                    {each}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
        </div>
      </motion.div>
    </div>
  )
}

// 模型的显示名，没配显示名时用模型 ID
function getModelName(model: ModelDefinition): string {
  return model.display_name || model.ref.model_id
}

// 两个模型引用指的是同一个模型吗？
function isSameModel(ref: ModelRef, other: ModelRef | undefined): boolean {
  return (
    !!other &&
    ref.provider_id === other.provider_id &&
    ref.model_id === other.model_id
  )
}

// 按 Provider 首次出现的顺序分组
function groupModelsByProvider(models: ModelDefinition[]): ModelGroup[] {
  const groups: ModelGroup[] = []
  for (const model of models) {
    const group = groups.find((each) => each.value === model.ref.provider_id)
    if (group) group.items.push(model)
    else groups.push({ value: model.ref.provider_id, items: [model] })
  }
  return groups
}

// 实际生效的推理强度：请求的强度在模型支持的列表里就用它，否则用模型的默认强度（可能为空，请求不带强度）；
// 同 Go 的 GetReasoningEffort
function getReasoningEffort(
  model: ModelDefinition,
  requested: string | undefined
): string {
  if (requested && model.reasoning_efforts.includes(requested)) return requested
  return model.default_reasoning_effort ?? ""
}
