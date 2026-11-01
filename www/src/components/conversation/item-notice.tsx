import { useId, useState, type ComponentProps, type ReactNode } from "react"
import { ChevronDown, ChevronRight } from "lucide"
import { MorphIcon } from "morphicons/react"
import { LatticeLoader } from "@/components/base/lattice-loader"
import {
  BanIcon,
  BracesIcon,
  ChevronRightIcon,
  CircleAlertIcon,
  CirclePauseIcon,
  CircleStopIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { Button } from "@/components/ui/button"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import { useItemDisclosure } from "@/lib/item-renderers"
import { cn } from "@/lib/utils"

// Item 的运行和异常用图标表示，完整状态供悬停、聚焦和读屏读取。
export function ItemStatus({
  state,
  label,
}: {
  state:
    "generating" | "running" | "failed" | "interrupted" | "blocked" | "stopped"
  label: string
}) {
  // 1. 状态选择图形与语义色，运行图标遵循减少动画设置。
  const Icon =
    state === "failed"
      ? CircleAlertIcon
      : state === "interrupted"
        ? CirclePauseIcon
        : state === "blocked"
          ? BanIcon
          : state === "stopped"
            ? CircleStopIcon
            : LatticeLoader
  const running = state === "running" || state === "generating"
  return (
    <Tooltip>
      <TooltipTrigger
        render={<span />}
        delay={600}
        closeOnClick
        tabIndex={0}
        role="img"
        aria-label={label}
        data-item-status={state}
        className={cn(
          "inline-flex size-4 shrink-0 items-center justify-center rounded-sm outline-none focus-visible:ring-1 focus-visible:ring-ring",
          state === "failed"
            ? "color-destructive"
            : running
              ? "color-muted"
              : "color-warning"
        )}
      >
        <Icon aria-hidden className="size-4" />
      </TooltipTrigger>
      <TooltipContent className="text-caption">{label}</TooltipContent>
    </Tooltip>
  )
}

// Item 标题：类型与展开箭头共位，状态、来源和调试分别交互。
export function ItemHeader({
  icon,
  title,
  titleTooltip = true,
  summary,
  status,
  tone,
  open,
  onOpenChange,
  onInspect,
  actions,
  className,
}: {
  icon: ComponentProps<typeof MorphIcon>["icon"]
  title: ReactNode
  titleTooltip?: boolean
  summary?: ReactNode
  status?: ReactNode
  tone?: "warning" | "destructive"
  open?: boolean
  onOpenChange?: (open: boolean) => void
  onInspect?: () => void
  actions?: ReactNode
  className?: string
}) {
  // 1. 读取真实 Item 的展开与调试入口，仅保存当前指针和键盘交互。
  const { t } = useTranslation()
  const disclosure = useItemDisclosure()
  const inspect = onInspect ?? disclosure?.onInspect
  const titleID = useId()
  const summaryID = useId()
  const actionID = useId()
  const [hovered, setHovered] = useState(false)
  const [focused, setFocused] = useState(false)
  const expanded = !!onOpenChange && !!open
  const interacting = !!onOpenChange && (hovered || focused)
  const triggerProps = {
    "aria-labelledby": `${actionID} ${titleID}`,
    "aria-describedby": summary ? summaryID : undefined,
    "aria-expanded": expanded,
    onClick: (event: React.MouseEvent<HTMLElement>) => {
      // 1. 鼠标拖选标题或参数时保留选区，键盘激活直接展开。
      const selection = window.getSelection()
      if (
        event.detail > 0 &&
        selection &&
        !selection.isCollapsed &&
        selection.containsNode(event.currentTarget, true)
      )
        return
      onOpenChange?.(!open)
    },
    onPointerEnter: () => setHovered(true),
    onPointerLeave: () => setHovered(false),
    onFocus: (event: React.FocusEvent<HTMLElement>) =>
      setFocused(event.currentTarget.matches(":focus-visible")),
    onBlur: () => setFocused(false),
  }
  // 2. 图标实例保持挂载，只更新目标图形；状态不参与形变。
  const heading = (
    <>
      <MorphIcon
        data-item-icon
        data-status-tone={tone}
        icon={
          !onOpenChange
            ? icon
            : expanded
              ? ChevronDown
              : interacting
                ? ChevronRight
                : icon
        }
        reducedMotion="user"
        size={16}
        aria-hidden
        className="item-header-icon size-4 shrink-0"
      />
      {titleTooltip ? (
        <OverflowTooltip
          id={titleID}
          data-item-title
          className="item-header-title min-w-0 truncate select-text"
        >
          {title}
        </OverflowTooltip>
      ) : (
        <span
          id={titleID}
          data-item-title
          className="item-header-title min-w-0 truncate select-text"
        >
          {title}
        </span>
      )}
    </>
  )
  return (
    <div
      data-item-header
      data-open={expanded || undefined}
      data-interacting={interacting || undefined}
      className={cn("flex h-8 min-w-0 items-center gap-2 text-ui", className)}
    >
      {onOpenChange && (
        <span id={actionID} className="sr-only">
          {expanded
            ? t("conversation.item.collapse")
            : t("conversation.item.expand")}
        </span>
      )}
      {onOpenChange ? (
        <Button
          variant="ghost"
          className="item-header-trigger h-8 min-w-0 shrink justify-start gap-2 border-0 px-0 text-ui color-subtle hover:bg-transparent"
          {...triggerProps}
        >
          {heading}
        </Button>
      ) : (
        <span className="item-header-trigger flex min-w-0 items-center gap-2 color-subtle">
          {heading}
        </span>
      )}
      {status}
      {summary &&
        (onOpenChange ? (
          <Button
            variant="ghost"
            tabIndex={-1}
            className="item-header-summary h-8 min-w-0 flex-1 justify-start border-0 px-0 text-ui color-muted hover:bg-transparent"
            {...triggerProps}
          >
            <span
              id={summaryID}
              data-item-summary
              className="min-w-0 truncate select-text"
            >
              {summary}
            </span>
          </Button>
        ) : (
          <span
            id={summaryID}
            data-item-summary
            className="item-header-summary min-w-0 flex-1 truncate color-muted"
          >
            {summary}
          </span>
        ))}
      {actions && (
        <span className="flex min-w-0 items-center gap-2">{actions}</span>
      )}
      {!summary && <span className="flex-1" />}
      {inspect && (
        <Tooltip>
          <TooltipTrigger
            delay={600}
            closeOnClick
            render={
              <Button
                variant="ghost"
                size="icon"
                className="shrink-0 color-muted"
                aria-label={t("conversation.debug.inspect")}
                aria-describedby={titleID}
                onClick={inspect}
              />
            }
          >
            <BracesIcon />
          </TooltipTrigger>
          <TooltipContent className="text-caption">
            {t("conversation.debug.inspect")}
          </TooltipContent>
        </Tooltip>
      )}
    </div>
  )
}

// 普通 Item 显示统一标题，内容首次展开后保持挂载。
export function ItemNotice({
  icon,
  tone,
  children,
  details,
  status,
}: {
  icon: ComponentProps<typeof MorphIcon>["icon"]
  tone?: "warning" | "destructive"
  children: ReactNode
  details?: ReactNode
  status?: ReactNode
}) {
  // 1. 展开选择和首次阅读记录由消息区保存。
  const disclosure = useItemDisclosure()
  return (
    <Collapsible
      open={!!details && (disclosure?.open ?? false)}
      onOpenChange={disclosure?.onOpenChange}
    >
      <ItemHeader
        icon={icon}
        title={children}
        status={status}
        tone={tone}
        open={disclosure?.open}
        onOpenChange={details ? disclosure?.onOpenChange : undefined}
      />
      {details && (
        <CollapsibleContent keepMounted className="process-disclosure-panel">
          <div className="process-disclosure-content ml-2 border-l py-2 pr-2 pl-6">
            {disclosure?.isChosen && details}
          </div>
        </CollapsibleContent>
      )}
    </Collapsible>
  )
}

// 弱化文字样式的展开触发器（说明行、思考过程、附带材料）：悬停和聚焦时文字变成前景色，键盘聚焦另有描边
export function DisclosureTrigger({
  className,
  children,
}: {
  className?: string
  children: ReactNode
}) {
  return (
    <CollapsibleTrigger
      className={cn(
        "group flex items-center gap-2 text-left text-ui color-muted outline-none hover:text-foreground focus-visible:text-foreground focus-visible:ring-1 focus-visible:ring-ring",
        className
      )}
    >
      {children}
    </CollapsibleTrigger>
  )
}

// 展开触发器里的箭头，展开时转向下方；所在的触发器要带 group 类
export function DisclosureChevron({ className }: { className?: string }) {
  return (
    <ChevronRightIcon
      aria-hidden
      className={cn(
        "size-4 shrink-0 transition-transform group-data-[panel-open]:rotate-90",
        className
      )}
    />
  )
}

// 说明或工具卡详情里的一段：小标题加内容
export function DetailSection({
  label,
  children,
  className,
}: {
  label: string
  children: ReactNode
  className?: string
}) {
  return (
    <div className={cn("flex min-w-0 flex-col gap-1", className)}>
      <div className="text-caption color-muted">{label}</div>
      <div className="min-w-0 text-body">{children}</div>
    </div>
  )
}
