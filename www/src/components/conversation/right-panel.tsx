import { Skeleton } from "@/components/ui/skeleton"
import { motion } from "motion/react"
import { useIsMobile } from "@/hooks/use-mobile"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import type { ParseKeys } from "i18next"
import { XIcon } from "lucide-react"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { TabBar } from "@/components/base/tab-bar"
import { AgentsTree } from "@/components/conversation/agents-tree"
import { ContextTab } from "@/components/conversation/context-tab"
import { ItemDebugTab } from "@/components/conversation/item-debug-tab"
import { TurnsTab } from "@/components/conversation/turns-tab"
import { RpcLogPanel } from "@/components/debug/rpc-log-panel"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Button } from "@/components/ui/button"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { ItemDebugTarget, MessageRow } from "@/lib/item-renderers"
import {
  $rightPanelTab,
  type RightPanelTab,
} from "@/lib/appearance/preferences"

const tabLabelKeys = {
  agents: "conversation.panel.tabs.agents",
  context: "conversation.panel.tabs.context",
  turns: "conversation.panel.tabs.turns",
  rpc: "conversation.panel.tabs.rpc",
  debug: "conversation.panel.tabs.debug",
} as const satisfies Record<RightPanelTab, ParseKeys>

// 对话页右栏：Agent、上下文、Turn、RPC、Item 调试，内容跟随这个页面的 Sync；
// 上次打开的页签记在本机，进子 Session 页、换对话时保留。桌面放在可拖宽的右栏里，窄屏放在抽屉里
export function RightPanel({
  sync,
  isReady,
  onClose,
  debugTarget,
  messageRows,
}: {
  sync: ConversationSync
  isReady: boolean
  onClose(): void
  debugTarget?: ItemDebugTarget
  messageRows: MessageRow[]
}) {
  const { t } = useTranslation()
  const isMobile = useIsMobile()
  const tab = useStore($rightPanelTab)
  const targetKey = isReady
    ? sync.conversationID + ":" + sync.getFollowedSessionID()
    : ""
  return (
    <Tabs
      value={tab}
      onValueChange={(next: RightPanelTab) => $rightPanelTab.set(next)}
      className="h-full min-h-0 min-w-0 gap-0"
    >
      <TabBar
        actions={
          <Tooltip disabled={isMobile}>
            <TooltipTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  className="shrink-0"
                  aria-label={t("conversation.panel.close")}
                  onClick={onClose}
                />
              }
            >
              <XIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("conversation.panel.close")}
            </TooltipContent>
          </Tooltip>
        }
      >
        <TabsList>
          {Object.entries(tabLabelKeys).map(([value, labelKey]) => (
            <TabsTrigger key={value} value={value} className="text-ui">
              {t(labelKey)}
            </TabsTrigger>
          ))}
        </TabsList>
      </TabBar>
      {!isReady && tab !== "rpc" && <Skeleton className="m-3 h-28" />}
      {(isReady || tab === "rpc") && (
        <motion.div
          key={`${tab}:${targetKey}`}
          className="flex min-h-0 min-w-0 flex-1 flex-col"
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          transition={{ duration: 0.12 }}
        >
          <TabsContent
            value="agents"
            className="min-h-0 overflow-y-auto p-3 text-body"
          >
            {isReady && tab === "agents" && (
              <AgentsTree key={targetKey} sync={sync} />
            )}
          </TabsContent>
          <TabsContent
            value="context"
            className="min-h-0 overflow-y-auto p-3 text-body"
          >
            {isReady && tab === "context" && (
              <ContextTab key={targetKey} sync={sync} />
            )}
          </TabsContent>
          <TabsContent
            value="turns"
            className="min-h-0 overflow-y-auto p-3 text-body"
          >
            {isReady && tab === "turns" && (
              <TurnsTab key={targetKey} sync={sync} />
            )}
          </TabsContent>
          <TabsContent value="rpc" className="flex min-h-0 flex-col text-body">
            {tab === "rpc" && (
              <RpcLogPanel conversationID={sync.conversationID} />
            )}
          </TabsContent>
          <TabsContent
            value="debug"
            className="flex min-h-0 flex-col overflow-y-auto p-3 text-body"
          >
            {isReady && tab === "debug" && (
              <ItemDebugTab
                key={targetKey}
                sync={sync}
                target={debugTarget}
                rows={messageRows}
              />
            )}
          </TabsContent>
        </motion.div>
      )}
    </Tabs>
  )
}
