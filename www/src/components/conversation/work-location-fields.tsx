import { useIsMobile } from "@/hooks/use-mobile"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useId, useRef, useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { CheckIcon, FolderIcon, ServerIcon, XIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { RequestError } from "@/components/base/request-error"
import { Skeleton } from "@/components/ui/skeleton"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Combobox,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxGroup,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
  ComboboxTrigger,
} from "@/components/ui/combobox"
import {
  Drawer,
  DrawerClose,
  DrawerContent,
  DrawerTitle,
  DrawerTrigger,
} from "@/components/ui/drawer"
import {
  Field,
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { useIsDesktop } from "@/hooks/use-is-desktop"
import type { WorkLocation } from "@/hooks/use-work-location"
import type { ExecutorDetails } from "@/lib/api/protocol.gen"
import {
  getDirectorySearch,
  workspaceDirectoriesOptions,
} from "@/lib/query/executor"
import {
  getRpcErrorDetail,
  getRpcErrorMessage,
  shouldShowLoadError,
} from "@/lib/rpc-error"
import { cn } from "@/lib/utils"

// 新对话和对话设置共用 Executor 选择、可手填的目录和目录候选。
export function WorkLocationFields({
  executors,
  location,
}: {
  executors: ExecutorDetails[]
  location: WorkLocation
}) {
  // 1. Executor 选择和目录字段顶部对齐，窄空间自然换行。
  return (
    <FieldGroup className="flex-row flex-wrap items-start gap-2">
      {executors.length > 0 && (
        <ExecutorPicker executors={executors} location={location} />
      )}
      {location.executor && (
        <DirectoryField key={location.executor.id} location={location} />
      )}
    </FieldGroup>
  )
}

// Executor 选项共用名称、说明和目录展示。
export function ExecutorOptionDetails({
  executor,
  directory = executor.workspace_directory,
}: {
  executor: ExecutorDetails
  directory?: string
}) {
  // 1. 名称和目录单行省略，说明最多显示两行。
  return (
    <span className="flex min-w-0 flex-1 flex-col gap-1 text-left text-table">
      <OverflowTooltip render={<span className="truncate" />}>
        {executor.id}
      </OverflowTooltip>
      {executor.description && (
        <span className="line-clamp-2 text-caption wrap-anywhere whitespace-normal color-muted">
          {executor.description}
        </span>
      )}
      {directory && (
        <OverflowTooltip
          render={<span className="truncate text-table color-muted" />}
        >
          {directory}
        </OverflowTooltip>
      )}
    </span>
  )
}

// 宽布局使用搜索 Combobox，窄布局在 Drawer 中显示同一组选项。
function ExecutorPicker({
  executors,
  location,
}: {
  executors: ExecutorDetails[]
  location: WorkLocation
}) {
  // 1. 搜索和弹层开关留在选择器内，选择结果由工作位置持有。
  const { t } = useTranslation()
  const isMobile = useIsMobile()
  const isDesktop = useIsDesktop()
  const anchor = useRef<HTMLButtonElement>(null)
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState("")
  const filtered = executors.filter((executor) =>
    [executor.id, executor.description, executor.workspace_directory]
      .join(" ")
      .toLowerCase()
      .includes(search.trim().toLowerCase())
  )
  const triggerContent = (
    <>
      <ServerIcon className="color-muted" />
      <OverflowTooltip render={<span className="min-w-0 truncate" />}>
        {location.executorID}
      </OverflowTooltip>
    </>
  )

  // 2. Drawer 直接提供搜索和选项。
  if (!isDesktop)
    return (
      <Drawer
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          if (!next) setSearch("")
        }}
      >
        <DrawerTrigger
          render={
            <Button
              type="button"
              variant="outline"
              className="max-w-48 min-w-0 text-ui"
              aria-label={t("workLocation.executor")}
            />
          }
        >
          {triggerContent}
        </DrawerTrigger>
        <DrawerContent className="text-body">
          <div className="flex shrink-0 items-center justify-between gap-2 p-3">
            <DrawerTitle className="text-section">
              {t("workLocation.executor")}
            </DrawerTitle>
            <Tooltip disabled={isMobile}>
              <TooltipTrigger
                render={
                  <DrawerClose
                    render={
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon"
                        aria-label={t("workLocation.closeExecutors")}
                      />
                    }
                  />
                }
              >
                <XIcon />
              </TooltipTrigger>
              <TooltipContent className="text-caption">
                {t("workLocation.closeExecutors")}
              </TooltipContent>
            </Tooltip>
          </div>
          <div className="flex min-h-0 flex-col gap-2 px-3 pb-3">
            <Input
              type="search"
              className="shrink-0 text-ui"
              aria-label={t("workLocation.searchExecutors")}
              placeholder={t("workLocation.searchExecutors")}
              value={search}
              onChange={(event) => setSearch(event.target.value)}
            />
            <div className="max-h-[50svh] min-h-0 overflow-y-auto p-1">
              {filtered.map((executor) => (
                <Button
                  key={executor.id}
                  type="button"
                  variant="ghost"
                  className="h-auto min-h-8 w-full min-w-0 justify-start px-2 py-2 text-table"
                  aria-pressed={executor.id === location.executorID}
                  onClick={() => {
                    location.chooseExecutor(executor.id)
                    setOpen(false)
                    setSearch("")
                  }}
                >
                  <ExecutorOptionDetails executor={executor} />
                  <CheckIcon
                    aria-hidden
                    className={cn(
                      executor.id !== location.executorID && "invisible"
                    )}
                  />
                </Button>
              ))}
              {filtered.length === 0 && (
                <p className="p-2 text-caption color-muted">
                  {t("common.selection.empty")}
                </p>
              )}
            </div>
          </div>
        </DrawerContent>
      </Drawer>
    )

  // 3. Combobox 弹层独立于短触发器宽度，按名称、说明和目录筛选。
  return (
    <Combobox<ExecutorDetails>
      items={executors}
      value={location.executor ?? null}
      itemToStringLabel={(executor) => executor.id}
      isItemEqualToValue={(executor, value) => executor.id === value.id}
      filter={(executor, query) =>
        [executor.id, executor.description, executor.workspace_directory]
          .join(" ")
          .toLowerCase()
          .includes(query.trim().toLowerCase())
      }
      onValueChange={(executor) => {
        if (executor) location.chooseExecutor(executor.id)
      }}
    >
      <ComboboxTrigger
        ref={anchor}
        aria-label={t("workLocation.executor")}
        render={
          <Button
            type="button"
            variant="outline"
            className="max-w-48 min-w-0 text-ui"
          />
        }
      >
        {triggerContent}
      </ComboboxTrigger>
      <ComboboxContent anchor={anchor} className="w-96 min-w-0 text-body">
        <ComboboxInput
          render={<InputGroupInput className="text-ui" />}
          showTrigger={false}
          aria-label={t("workLocation.searchExecutors")}
          placeholder={t("workLocation.searchExecutors")}
          className="text-ui"
        />
        <ComboboxEmpty className="text-ui">
          {t("common.selection.empty")}
        </ComboboxEmpty>
        <ComboboxList className="p-1">
          {(executor: ExecutorDetails) => (
            <ComboboxItem
              key={executor.id}
              value={executor}
              className="text-table"
            >
              <ExecutorOptionDetails executor={executor} />
            </ComboboxItem>
          )}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  )
}

// 目录可直接输入；候选仅在打开后读取，同层末段名称在前端过滤。
function DirectoryField({ location }: { location: WorkLocation }) {
  // 1. 以已保存目录为起点查询一个层级，不保留第二份目录缓存。
  const { t } = useTranslation()
  const connection = useServerConnection()
  const id = useId()
  const input = useRef<HTMLInputElement>(null)
  const [open, setOpen] = useState(false)
  const { directory, keyword } = getDirectorySearch(location.directory)
  const directories = useQuery({
    ...workspaceDirectoriesOptions(connection, {
      executor_id: location.executorID!,
      directory,
      base_directory: location.baseDirectory,
    }),
    enabled: (query) =>
      open &&
      (query.state.status !== "error" ||
        !shouldShowLoadError(query.state.error)),
  })
  const names = (directories.data?.directories ?? []).filter((name) =>
    name.startsWith(keyword)
  )
  const directoryErrorDetail = getRpcErrorDetail(location.directoryError)
  const directoryErrorText = !location.directoryError
    ? undefined
    : directoryErrorDetail
      ? t("workLocation.directoryInvalid", { detail: directoryErrorDetail })
      : getRpcErrorMessage(location.directoryError, t)

  // 2. 选中目录后进入下一层；候选失败仍保留输入和局部重试。
  return (
    <Field
      className="min-w-48 flex-1 gap-1"
      data-invalid={!!directoryErrorText}
    >
      <FieldLabel htmlFor={id} className="sr-only text-ui">
        {t("workLocation.directory")}
      </FieldLabel>
      <Combobox<string>
        items={names}
        filter={null}
        value={null}
        inputValue={location.directory}
        open={open}
        onOpenChange={(next, details) => {
          if (details.reason === "item-press") {
            details.cancel()
            return
          }
          setOpen(next)
        }}
        onInputValueChange={(value, details) => {
          if (details.reason === "input-change") location.setDirectory(value)
          else details.cancel()
        }}
        onValueChange={(name) => {
          if (name && directories.data)
            location.setDirectory(
              directories.data.directory.replace(/\/$/, "") + "/" + name + "/"
            )
        }}
      >
        <ComboboxInput
          render={<InputGroupInput className="text-table" />}
          id={id}
          ref={input}
          showTrigger={false}
          aria-label={t("workLocation.directory")}
          aria-invalid={!!directoryErrorText}
          placeholder={
            location.executor?.workspace_directory ||
            t("workLocation.directoryPlaceholder")
          }
          className="w-full"
          autoComplete="off"
          spellCheck={false}
        >
          <InputGroupAddon>
            <FolderIcon />
          </InputGroupAddon>
          <InputGroupAddon align="inline-end">
            <Tooltip>
              <TooltipTrigger
                render={
                  <InputGroupButton
                    render={<ComboboxTrigger />}
                    aria-label={t("workLocation.directorySuggestions")}
                    size="icon-xs"
                  />
                }
              />
              <TooltipContent className="text-caption">
                {t("workLocation.directorySuggestions")}
              </TooltipContent>
            </Tooltip>
          </InputGroupAddon>
          {location.isResolvingDirectory && (
            <InputGroupAddon align="inline-end">
              <LatticeLoader aria-label={t("common.loading")} />
            </InputGroupAddon>
          )}
        </ComboboxInput>
        <ComboboxContent
          className="w-80 min-w-0 text-body"
          aria-label={t("workLocation.directorySuggestions")}
        >
          {directories.isFetching && (
            <div className="flex items-center gap-2 p-2 text-caption color-muted">
              <LatticeLoader aria-hidden />
              {t("common.loading")}
            </div>
          )}
          {directories.isError && shouldShowLoadError(directories.error) && (
            <RequestError
              error={directories.error}
              onRetry={() => {
                input.current?.focus()
                void directories.refetch()
              }}
            />
          )}
          {!directories.data &&
            directories.isError &&
            !shouldShowLoadError(directories.error) && (
              <Skeleton className="h-28 w-full" />
            )}
          <ComboboxEmpty
            className={cn(
              "text-ui",
              (!directories.isSuccess || directories.isFetching) &&
                "group-data-empty/combobox-content:hidden"
            )}
          >
            {t("workLocation.noDirectories")}
          </ComboboxEmpty>
          <ComboboxList className="p-1">
            <ComboboxGroup>
              {names.map((name) => (
                <ComboboxItem key={name} value={name} className="text-table">
                  <OverflowTooltip
                    render={<span className="min-w-0 truncate" />}
                  >
                    {name}/
                  </OverflowTooltip>
                </ComboboxItem>
              ))}
            </ComboboxGroup>
          </ComboboxList>
        </ComboboxContent>
      </Combobox>
      {directoryErrorText && (
        <FieldError className="text-caption">{directoryErrorText}</FieldError>
      )}
    </Field>
  )
}
