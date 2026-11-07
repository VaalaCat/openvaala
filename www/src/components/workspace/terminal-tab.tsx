import { useEffect, useLayoutEffect, useRef, useSyncExternalStore } from "react"
import { useStore } from "@nanostores/react"
import type { ParseKeys } from "i18next"
import { RefreshCwIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"
import type { TerminalKey } from "@/lib/workspace/terminal-input"
import type { TerminalSession } from "@/lib/workspace/terminal-session"
import type { TabWorkspace } from "@/lib/workspace/tab-workspace"
import "@xterm/xterm/css/xterm.css"

// 窄屏按键条上的按键：Ctrl 是修饰键，作用于下一次输入，其余直接发出按键序列
const terminalKeys: {
  key: TerminalKey | "ctrl"
  label: string
  labelKey?: ParseKeys
}[] = [
  { key: "esc", label: "Esc" },
  { key: "tab", label: "Tab" },
  { key: "ctrl", label: "Ctrl" },
  { key: "up", label: "↑", labelKey: "workspace.terminal.keyUp" },
  { key: "down", label: "↓", labelKey: "workspace.terminal.keyDown" },
  { key: "left", label: "←", labelKey: "workspace.terminal.keyLeft" },
  { key: "right", label: "→", labelKey: "workspace.terminal.keyRight" },
  { key: "enter", label: "Enter" },
  { key: "ctrl-c", label: "Ctrl+C" },
  { key: "ctrl-d", label: "Ctrl+D" },
  { key: "ctrl-z", label: "Ctrl+Z" },
  { key: "home", label: "Home" },
  { key: "end", label: "End" },
]

// 终端标签页：xterm 铺满标签页；窄屏下面多一排按键条；终端结束后右下角显示结束原因和「重新启动」，
// 连接断开时右下角显示正在重连
export function TerminalTab({
  workspace,
  tabID,
}: {
  workspace: TabWorkspace
  tabID: string
}) {
  const { t } = useTranslation()
  const session = workspace.getTerminalSession(tabID)
  if (!session) throw new Error("terminal tab has no session: " + tabID)
  const snapshot = useSyncExternalStore(session.subscribe, session.getSnapshot)
  const isSelected = useStore(workspace.$selectedTabID) === tabID
  const hostRef = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => session.attach(hostRef.current!), [session])
  // 标签页被选中、打开完成或重新启动时把键盘焦点交给终端，打开后可以直接输入；已结束的终端没有输入。
  // 断线重连时阶段在 running 和 reconnecting 之间切换，不在依赖里，免得抢走用户正在输入的地方
  const isExited = snapshot.phase === "exited"
  const isOpening = snapshot.phase === "opening"
  useEffect(() => {
    if (isSelected && !isExited) session.terminal.focus()
  }, [isSelected, session, isExited, isOpening])
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="relative min-h-0 flex-1">
        <div
          ref={hostRef}
          role="region"
          aria-label={t("workspace.terminal.screen")}
          className="terminal-viewport size-full overflow-hidden bg-background"
        />
        {(snapshot.phase === "exited" || snapshot.isWaitingForConnection) && (
          <div className="pointer-events-none absolute inset-x-3 bottom-3 flex justify-end">
            <div
              role="status"
              className="pointer-events-auto flex max-w-full min-w-0 items-center gap-2 border bg-popover p-2 text-caption shadow-md"
            >
              <span className="min-w-0 wrap-anywhere color-muted">
                {snapshot.isWaitingForConnection
                  ? t("workspace.terminal.reconnecting")
                  : snapshot.exitReason}
              </span>
              {snapshot.phase === "exited" && (
                <Button
                  variant="outline"
                  className="text-ui"
                  onClick={() => workspace.restartTerminal(tabID)}
                >
                  <RefreshCwIcon data-icon="inline-start" />
                  {t("workspace.terminal.restart")}
                </Button>
              )}
            </div>
          </div>
        )}
      </div>
      <TerminalKeys session={session} />
    </div>
  )
}

// 窄屏的按键条：手机键盘没有的 Esc、Tab、Ctrl、方向键等；终端没在运行时不能按
function TerminalKeys({ session }: { session: TerminalSession }) {
  const { t } = useTranslation()
  const snapshot = useSyncExternalStore(session.subscribe, session.getSnapshot)
  return (
    <div
      role="toolbar"
      aria-label={t("workspace.terminal.keys")}
      className="flex shrink-0 gap-2 overflow-x-auto border-t p-2 md:hidden"
    >
      {terminalKeys.map(({ key, label, labelKey }) => (
        <Button
          key={key}
          variant="outline"
          className="min-w-10 shrink-0 text-code aria-pressed:bg-foreground aria-pressed:text-background"
          disabled={snapshot.phase !== "running"}
          aria-label={labelKey ? t(labelKey) : label}
          aria-pressed={key === "ctrl" ? snapshot.isControlPending : undefined}
          // 按下时不让按钮抢走终端的焦点，软键盘才不会收起
          onPointerDown={(event) => event.preventDefault()}
          onClick={() => {
            if (key === "ctrl") {
              session.setControlPending(!snapshot.isControlPending)
              session.terminal.focus()
            } else session.sendKey(key)
          }}
        >
          {label}
        </Button>
      ))}
    </div>
  )
}
