import {
  I18nLabelDefaults,
  Layout,
  type ITabRenderValues,
  type TabNode,
} from "flexlayout-react"
import { useStore } from "@nanostores/react"
import type { ParseKeys } from "i18next"
import {
  ArrowDownIcon,
  ArrowRightIcon,
  ChevronDownIcon,
  MaximizeIcon,
  MinimizeIcon,
  PlusIcon,
  RotateCcwIcon,
  TerminalIcon,
  XIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { ConversationTab } from "@/components/workspace/conversation-tab"
import { TerminalTab } from "@/components/workspace/terminal-tab"
import {
  WorkspaceTabIcon,
  WorkspaceTabTitle,
} from "@/components/workspace/workspace-tab-label"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { useIsTerminalRunning } from "@/hooks/use-terminal-running"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { ItemDebugTarget } from "@/lib/item-renderers"
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"
import type { SplitDirection } from "@/lib/workspace/types"
import "@/components/base/tab-bar.css"
import "flexlayout-react/style/light.css"
import "./workspace.css"

type InspectItem = (
  tabID: string,
  sync: ConversationSync,
  target: ItemDebugTarget
) => void

const splitDirections = [
  {
    direction: "RIGHT",
    Icon: ArrowRightIcon,
    labelKey: "workspace.splitRight",
  },
  { direction: "BOTTOM", Icon: ArrowDownIcon, labelKey: "workspace.splitDown" },
] as const satisfies readonly {
  direction: SplitDirection
  Icon: unknown
  labelKey: ParseKeys
}[]

// 工作区内容区：flexlayout 的分栏和标签栏。窄屏（宽度 < 768）仍然是同一个 Layout，只是用样式隐藏标签栏和分栏、
// 让选中的标签页铺满，这样跨过窄屏分界时标签页的内容不重新挂载，布局本身也不变
export function WorkspaceLayout({
  workspace,
  onInspect,
}: {
  workspace: TabWorkspace
  onInspect: InspectItem
}) {
  const selectedTabID = useStore(workspace.$selectedTabID)
  const { t } = useTranslation()
  const labels: Record<string, string> = {
    "flexlayout.ui.close.tab": t("workspace.closeTab"),
    "flexlayout.ui.overflow.menu.tooltip": t("workspace.hiddenTabs"),
    "flexlayout.ui.splitter": t("workspace.resizeArea"),
  }
  return (
    <div className="workspace-layout relative min-h-0 min-w-0 flex-1 text-ui">
      {/* 窄屏只留选中的标签页，其余区域的内容面板收起 */}
      {selectedTabID && (
        <style>
          {`@media (max-width: 767px) { .workspace-layout .flexlayout__tab:not(#flexlayout-tab-${CSS.escape(selectedTabID)}) { display: none !important; } }`}
        </style>
      )}
      <Layout
        model={workspace.$layout.get()}
        factory={(node: TabNode) => (
          <WorkspaceTabContent
            workspace={workspace}
            tabID={node.getId()}
            onInspect={onInspect}
          />
        )}
        // 拖动分隔条的中间帧不保存，松手后的那一次才保存
        onModelChange={(_, action) => {
          if (!action.isAdjusting()) workspace.handleLayoutChanged()
        }}
        realtimeResize
        i18nTranslator={(key) =>
          labels[key] ??
          (I18nLabelDefaults as Record<string, string>)[key] ??
          key
        }
        onRenderTab={(node, values) => {
          // 图标和标题自己订阅，跟着对话标题变化，不用 flexlayout 记下的名字
          const tabID = node.getId()
          values.leading = (
            <WorkspaceTabIcon
              workspace={workspace}
              tabID={tabID}
              className="size-4"
              data-tab-closable={node.isEnableClose()}
            />
          )
          values.content = (
            <WorkspaceTabTitle workspace={workspace} tabID={tabID} />
          )
          // name 是标签按钮的无障碍名称和溢出菜单里的名字，运行时有、类型里没有
          const named = values as ITabRenderValues & { name: string }
          named.name = workspace.getTabTitle(tabID)
        }}
        onRenderTabSet={(node, values) => {
          // 区域的标签栏右端只有一个「+」按钮，所有区域操作都在它的菜单里；排在溢出按钮的右边
          values.buttons.push(
            <AreaMenu
              key="area"
              workspace={workspace}
              tabSetID={node.getId()}
            />
          )
        }}
        icons={{
          close: <XIcon className="size-4" />,
          more: <ChevronDownIcon className="size-4" />,
        }}
      />
    </div>
  )
}

// 区域标签栏的「+」菜单：在这个区域新开标签页、分栏、最大化或还原；区域里选中的是运行中的终端时还能重置它。
// 和桌面端一样，标签栏上只有它一个操作按钮
function AreaMenu({
  workspace,
  tabSetID,
}: {
  workspace: TabWorkspace
  tabSetID: string
}) {
  const { t } = useTranslation()
  const maximizeAction = workspace.getAreaMaximizeAction(tabSetID)
  const selectedTabID = workspace.getAreaSelectedTabID(tabSetID)
  const isTerminalRunning = useIsTerminalRunning(workspace, selectedTabID)
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            className="flexlayout__tab_toolbar_button text-ui"
            aria-label={t("workspace.areaActions")}
          />
        }
      >
        <PlusIcon />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuGroup>
          <DropdownMenuItem
            className="text-ui"
            onClick={() => void workspace.openTerminal(tabSetID)}
          >
            <TerminalIcon />
            {t("workspace.newTerminal")}
          </DropdownMenuItem>
          {isTerminalRunning && (
            <DropdownMenuItem
              className="text-ui"
              onClick={() => workspace.resetTerminal(selectedTabID!)}
            >
              <RotateCcwIcon />
              {t("workspace.terminal.reset")}
            </DropdownMenuItem>
          )}
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          {splitDirections.map(({ direction, Icon, labelKey }) => (
            <DropdownMenuItem
              key={direction}
              className="text-ui"
              onClick={() => void workspace.split(tabSetID, direction)}
            >
              <Icon />
              {t(labelKey)}
            </DropdownMenuItem>
          ))}
          {maximizeAction && (
            <DropdownMenuItem
              className="text-ui"
              onClick={() => workspace.toggleMaximize(tabSetID)}
            >
              {maximizeAction === "maximize" ? (
                <MaximizeIcon />
              ) : (
                <MinimizeIcon />
              )}
              {t(
                maximizeAction === "maximize"
                  ? "workspace.maximizeArea"
                  : "workspace.restoreArea"
              )}
            </DropdownMenuItem>
          )}
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

// 按标签页的种类显示内容；主对话、子 Agent 和旁支共用一个标签页组件
function WorkspaceTabContent({
  workspace,
  tabID,
  onInspect,
}: {
  workspace: TabWorkspace
  tabID: string
  onInspect: InspectItem
}) {
  const tab = useStore(workspace.$tabs)[tabID]
  switch (tab?.kind) {
    case "main":
    case "sub-agent":
    case "side-conversation":
      return (
        <ConversationTab
          workspace={workspace}
          tabID={tabID}
          tab={tab}
          onInspect={onInspect}
        />
      )
    case "terminal":
      return <TerminalTab workspace={workspace} tabID={tabID} />
    default:
      return null
  }
}
