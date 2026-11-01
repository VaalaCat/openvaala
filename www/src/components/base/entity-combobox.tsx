import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useEffect, useId, useRef, useState } from "react"
import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { ChevronDownIcon, XIcon } from "lucide-react"
import {
  Combobox,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
  ComboboxTrigger,
  ComboboxValue,
  useComboboxAnchor,
} from "@/components/ui/combobox"
import { Button } from "@/components/ui/button"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"
import { Field, FieldLabel } from "@/components/ui/field"
import { Skeleton } from "@/components/ui/skeleton"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { FieldTooltip } from "./field-tooltip"
import { RequestError } from "./request-error"

export type EntityOption = {
  id: string
  label: string
  description?: string
  disabled?: boolean
}
export type EntitySelection =
  | { multiple?: false; value: string; onChange: (value: string) => void }
  | { multiple: true; value: string[]; onChange: (value: string[]) => void }

export function EntityCombobox(
  props: EntitySelection & {
    label: string
    placeholder: string
    options: EntityOption[]
    selectedOptions: EntityOption[]
    emptyText?: string // 没有搜索词且结果为空时显示，没传时显示「无匹配结果」
    emptyAction?: ReactNode // 跟在 emptyText 下面，通常是前置资源的链接
    fieldError?: string
    disabled?: boolean
    loading: boolean
    // 选项分页加载时才传 error、onRetry、hasMore、onMore
    error?: unknown
    shouldShowError?: (error: unknown) => boolean
    paused?: boolean
    onRetry?: () => void
    hasMore?: boolean
    onMore?: () => void
    onSearch: (keyword: string) => void
    onOpenChange?: (open: boolean) => void
  }
) {
  const { t } = useTranslation()
  const id = useId()
  const anchor = useComboboxAnchor()
  const [menuElement, setMenuElement] = useState<HTMLDivElement | null>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const input = useRef<HTMLInputElement>(null)
  const [keyword, setKeyword] = useState("")
  const [open, setOpen] = useState(false)
  const { onSearch } = props
  useEffect(() => {
    const timer = setTimeout(() => onSearch(keyword.trim()), 250)
    return () => clearTimeout(timer)
  }, [keyword, onSearch])
  const selected = props.multiple
    ? props.value
    : props.value
      ? [props.value]
      : []
  const known = new Map(
    [...props.selectedOptions, ...props.options].map((option) => [
      option.id,
      option,
    ])
  )
  const labels = selected.map((value) => known.get(value)?.label ?? value)
  function change(value: string | string[] | null) {
    if (props.multiple) props.onChange(Array.isArray(value) ? value : [])
    else props.onChange(typeof value === "string" ? value : "")
  }
  return (
    <Field
      className="min-w-0"
      data-disabled={props.disabled}
      data-invalid={!!props.fieldError}
    >
      <FieldLabel className="text-ui" htmlFor={id}>
        {props.label}
      </FieldLabel>
      <Combobox<string, boolean>
        multiple={!!props.multiple}
        value={props.multiple ? props.value : props.value || null}
        onValueChange={change}
        items={[...known.keys()]}
        filteredItems={props.options.map((option) => option.id)}
        filter={null}
        itemToStringLabel={(value) => known.get(value)?.label ?? value}
        disabled={props.disabled}
        inputValue={keyword}
        onInputValueChange={(value, details) => {
          if (
            details.reason === "input-change" ||
            details.reason === "input-clear"
          )
            setKeyword(value)
          else if (props.multiple && details.isItemPress) details.cancel()
        }}
        open={open}
        onOpenChange={(next, details) => {
          if (!next && props.multiple && details.reason === "item-press") {
            details.cancel()
            return
          }
          setOpen(next)
          props.onOpenChange?.(next)
          if (!next) setKeyword("")
        }}
      >
        <InputGroup ref={anchor}>
          <FieldTooltip
            message={props.fieldError}
            menuOpen={open}
            menuElement={menuElement}
          >
            <ComboboxTrigger
              id={id}
              ref={trigger}
              className="[&>svg]:hidden"
              render={
                <Button
                  variant="ghost"
                  className="h-full min-w-0 flex-1 justify-between border-0 text-ui font-normal"
                />
              }
            >
              <ComboboxValue>
                <OverflowTooltip
                  disabled={!!props.fieldError}
                  render={
                    <span className="min-w-0 flex-1 truncate text-left" />
                  }
                >
                  {labels[0] ?? props.placeholder}
                </OverflowTooltip>
                {labels.length > 1 && (
                  <span aria-label={labels.slice(1).join(", ")}>
                    … +{labels.length - 1}
                  </span>
                )}
              </ComboboxValue>
            </ComboboxTrigger>
          </FieldTooltip>
          <InputGroupAddon align="inline-end">
            <Tooltip>
              <TooltipTrigger
                render={
                  <InputGroupButton
                    size="icon-xs"
                    disabled={props.disabled}
                    tabIndex={selected.length > 0 ? 0 : -1}
                    aria-label={
                      selected.length > 0
                        ? t("common.selection.clear")
                        : props.label
                    }
                    onMouseDown={(event) => event.preventDefault()}
                    onClick={() => {
                      if (selected.length > 0) {
                        change(null)
                        setKeyword("")
                        setOpen(false)
                        props.onOpenChange?.(false)
                        trigger.current?.focus()
                      } else trigger.current?.click()
                    }}
                    focusableWhenDisabled
                  />
                }
              >
                {selected.length > 0 ? <XIcon /> : <ChevronDownIcon />}
              </TooltipTrigger>
              <TooltipContent className="text-caption">
                {selected.length > 0
                  ? t("common.selection.clear")
                  : props.label}
              </TooltipContent>
            </Tooltip>
          </InputGroupAddon>
        </InputGroup>
        <ComboboxContent
          ref={setMenuElement}
          anchor={anchor}
          sideOffset={4}
          className="flex flex-col *:data-[slot=input-group]:shrink-0"
        >
          <ComboboxInput
            ref={input}
            render={<InputGroupInput className="text-ui" />}
            showTrigger={false}
            aria-label={t("common.actions.search")}
            placeholder={t("common.actions.search")}
            showClear={false}
            className="[&_input]:pr-8"
          >
            {props.loading && (
              <LatticeLoader
                className="absolute right-2"
                aria-label={t("common.loading")}
              />
            )}
          </ComboboxInput>
          {!!props.error && (props.shouldShowError?.(props.error) ?? true) && (
            <RequestError error={props.error} onRetry={props.onRetry} />
          )}
          {!!props.error &&
            props.shouldShowError?.(props.error) === false &&
            props.options.length === 0 && <Skeleton className="h-28 w-full" />}
          {!props.loading && !props.error && (
            <ComboboxEmpty className="text-ui">
              {keyword.trim() || !props.emptyText ? (
                t("common.selection.empty")
              ) : (
                <div className="flex flex-col items-center gap-1 text-ui">
                  <span>{props.emptyText}</span>
                  {props.emptyAction}
                </div>
              )}
            </ComboboxEmpty>
          )}
          <ComboboxList className="min-h-0 p-1">
            {(value: string) => (
              <ComboboxItem
                key={value}
                value={value}
                disabled={known.get(value)?.disabled}
                className="min-h-8 min-w-0 text-table"
              >
                <OverflowTooltip render={<span className="min-w-0 truncate" />}>
                  {known.get(value)?.label ?? value}
                </OverflowTooltip>
                {known.get(value)?.description && (
                  <OverflowTooltip
                    render={
                      <span className="min-w-0 truncate text-caption color-muted" />
                    }
                  >
                    {known.get(value)?.description}
                  </OverflowTooltip>
                )}
              </ComboboxItem>
            )}
          </ComboboxList>
          {props.hasMore && (
            <div className="flex shrink-0 justify-end border-t p-1">
              <Button
                className="text-ui"
                variant="ghost"
                size="sm"
                disabled={props.loading || props.paused}
                focusableWhenDisabled
                onClick={props.onMore}
              >
                {t("common.selection.more")}
              </Button>
            </div>
          )}
        </ComboboxContent>
      </Combobox>
    </Field>
  )
}
