import { useSyncExternalStore } from "react"
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"

const subscribeNothing = () => () => {}

// 这个标签页是运行中的终端吗？终端阶段变化时跟着更新；不是终端标签页或没有标签页时是 false
export function useIsTerminalRunning(
  workspace: TabWorkspace,
  tabID: string | undefined
): boolean {
  const session = tabID ? workspace.getTerminalSession(tabID) : undefined
  return useSyncExternalStore(
    session?.subscribe ?? subscribeNothing,
    () => session?.getSnapshot().phase === "running"
  )
}
