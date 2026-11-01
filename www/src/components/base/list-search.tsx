import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useEffect, useLayoutEffect, useRef, useState } from "react"
import type { ComponentProps } from "react"
import { useTranslation } from "react-i18next"
import { SearchIcon, XIcon } from "lucide-react"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"

// 搜索框：keyword 是已生效的搜索词，框内文字是草稿。
// 提交（回车或点搜索按钮）由外层表单处理，并取消还没到时间的防抖；传 onKeywordChange 时，
// 停止输入 debounceMs 毫秒后也会把草稿交给最新一次渲染传进来的 onKeywordChange。
export function ListSearch({
  keyword,
  searchDisabled,
  onClear,
  onKeywordChange,
  debounceMs = 300,
  ...props
}: Omit<
  ComponentProps<typeof InputGroupInput>,
  "value" | "defaultValue" | "onChange"
> & {
  keyword: string
  searchDisabled?: boolean
  onClear?: () => void
  onKeywordChange?: (keyword: string) => void
  debounceMs?: number
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState({ keyword, value: keyword })
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  // 防抖到时调用最新的回调：调用方的回调可能读渲染时的状态（例如地址参数），旧回调会写回过期的值
  const latestOnKeywordChange = useRef(onKeywordChange)
  useLayoutEffect(() => {
    latestOnKeywordChange.current = onKeywordChange
  })
  useEffect(() => () => clearTimeout(timer.current), [])
  if (draft.keyword !== keyword) setDraft({ keyword, value: keyword })
  return (
    <InputGroup>
      <InputGroupInput
        name="keyword"
        className="px-2 text-ui"
        {...props}
        value={draft.value}
        onChange={(event) => {
          const value = event.target.value
          setDraft({ keyword, value })
          if (!onKeywordChange) return
          clearTimeout(timer.current)
          timer.current = setTimeout(
            () => latestOnKeywordChange.current?.(value),
            debounceMs
          )
        }}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.nativeEvent.isComposing)
            clearTimeout(timer.current)
          props.onKeyDown?.(event)
        }}
      />
      <InputGroupAddon align="inline-end">
        {keyword && onClear && (
          <Tooltip>
            <TooltipTrigger
              render={
                <InputGroupButton
                  type="button"
                  size="icon-sm"
                  aria-label={t("common.actions.clearSearch")}
                  onClick={() => {
                    clearTimeout(timer.current)
                    onClear()
                  }}
                />
              }
            >
              <XIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("common.actions.clearSearch")}
            </TooltipContent>
          </Tooltip>
        )}
        <Tooltip>
          <TooltipTrigger
            render={
              <InputGroupButton
                type="submit"
                size="icon-sm"
                disabled={searchDisabled}
                focusableWhenDisabled
                aria-label={t("common.actions.search")}
                onClick={() => clearTimeout(timer.current)}
              />
            }
          >
            <SearchIcon />
          </TooltipTrigger>
          <TooltipContent className="text-caption">
            {t("common.actions.search")}
          </TooltipContent>
        </Tooltip>
      </InputGroupAddon>
    </InputGroup>
  )
}
