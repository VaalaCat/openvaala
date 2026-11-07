import { CheckIcon, RotateCcwIcon, TerminalIcon, XIcon } from "lucide-react"
import { useStore } from "@nanostores/react"
import type { Model } from "flexlayout-react"
import { useTranslation } from "react-i18next"
import {
  WorkspaceTabIcon,
  WorkspaceTabTitle,
} from "@/components/workspace/workspace-tab-label"
import { Button } from "@/components/ui/button"
import {
  Drawer,
  DrawerContent,
  DrawerFooter,
  DrawerTitle,
} from "@/components/ui/drawer"
import { useIsTerminalRunning } from "@/hooks/use-terminal-running"
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"

// 窄屏的标签页列表：从底部拉起，按区域顺序列出全部标签页，当前那一个打勾；
// 点一行切过去并收起，能关的标签页行尾有 ×；底部新开终端，当前标签页是运行中的终端时还能重置它
export function WorkspaceTabDrawer({
  workspace,
  open,
  onOpenChange,
}: {
  workspace: TabWorkspace
  open: boolean
  onOpenChange(open: boolean): void
}) {
  const { t } = useTranslation()
  const tabs = useStore(workspace.$tabs)
  const selectedTabID = useStore(workspace.$selectedTabID)
  const isTerminalRunning = useIsTerminalRunning(workspace, selectedTabID)
  return (
    <Drawer open={open} onOpenChange={onOpenChange}>
      <DrawerContent className="text-body [--drawer-content-max-height:85svh]">
        <DrawerTitle className="sr-only text-section">
          {t("workspace.tabs.title")}
        </DrawerTitle>
        <ul className="flex min-h-0 flex-col overflow-y-auto py-2">
          {getTabIDsInLayoutOrder(workspace.$layout.get()).map((tabID) => {
            const tab = tabs[tabID]
            if (!tab) return null
            const isSelected = tabID === selectedTabID
            return (
              <li key={tabID} className="flex items-center pr-2">
                <Button
                  variant="ghost"
                  className="h-10 min-w-0 flex-1 justify-start gap-2 px-4 text-ui"
                  aria-current={isSelected ? "true" : undefined}
                  onClick={() => {
                    workspace.selectTab(tabID)
                    onOpenChange(false)
                  }}
                >
                  <CheckIcon className={isSelected ? undefined : "invisible"} />
                  <WorkspaceTabIcon workspace={workspace} tabID={tabID} />
                  <span className="truncate">
                    <WorkspaceTabTitle workspace={workspace} tabID={tabID} />
                  </span>
                </Button>
                {workspace.isTabClosable(tabID) && (
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-10 shrink-0"
                    aria-label={t("workspace.tabs.close", {
                      name: workspace.getTabTitle(tabID),
                    })}
                    onClick={() => void workspace.closeTab(tabID)}
                  >
                    <XIcon />
                  </Button>
                )}
              </li>
            )
          })}
        </ul>
        <DrawerFooter className="flex-row border-t pt-4">
          <Button
            variant="outline"
            className="flex-1 text-ui"
            onClick={() => {
              void workspace.openTerminal()
              onOpenChange(false)
            }}
          >
            <TerminalIcon data-icon="inline-start" />
            {t("workspace.newTerminal")}
          </Button>
          {isTerminalRunning && (
            <Button
              variant="outline"
              className="flex-1 text-ui"
              onClick={() => {
                workspace.resetTerminal(selectedTabID)
                onOpenChange(false)
              }}
            >
              <RotateCcwIcon data-icon="inline-start" />
              {t("workspace.terminal.reset")}
            </Button>
          )}
        </DrawerFooter>
      </DrawerContent>
    </Drawer>
  )
}

// 全部标签页的 ID，按区域顺序、区域内的标签页顺序排列
function getTabIDsInLayoutOrder(model: Model): string[] {
  const tabIDs: string[] = []
  model.visitNodes((node) => {
    if (node.getType() === "tab") tabIDs.push(node.getId())
  })
  return tabIDs
}
