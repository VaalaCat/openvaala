import { useLayoutEffect, useRef, useState } from "react"
import { FoldVerticalIcon } from "lucide-react"
import { useMutation } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { DataGlyph } from "@/components/base/data-glyph"
import { TokenNumberValue } from "@/components/base/token-number-value"
import { ModelPicker } from "@/components/conversation/model-picker"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import {
  Popover,
  PopoverTrigger,
  PopoverContent,
} from "@/components/ui/popover"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { Separator } from "@/components/ui/separator"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { ModelSelection, SessionRuntimeInfo } from "@/lib/api/protocol.gen"
import { notifyFailure } from "@/lib/notify"
import { formatNumberUnit } from "@/lib/number-unit"

// Session 的模型和推理强度选择；窄容器合并为一个入口。
export function SessionControls({
  agentName,
  value,
  onChange,
  disabled = false,
}: {
  agentName?: string
  value?: ModelSelection
  onChange(model: ModelSelection): void
  disabled?: boolean
}) {
  const container = useRef<HTMLDivElement>(null)
  const [compact, setCompact] = useState(true)
  useLayoutEffect(() => {
    // 1. 容器宽度只决定模型与强度入口是否合并，弹层形态由 ModelPicker 按视口决定。
    const element = container.current
    if (!element) return
    setCompact(element.clientWidth < 400)
    const observer = new ResizeObserver(([entry]) =>
      setCompact(entry.contentRect.width < 400)
    )
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  return (
    <div ref={container} className="flex min-w-0 flex-1 items-center">
      <ModelPicker
        agentName={agentName}
        value={value}
        onChange={onChange}
        compact={compact}
        disabled={disabled}
      />
    </div>
  )
}

// 上下文占用：环表示下一次请求预计占模型窗口的比例，窗口未知时只显示 token 数。
// 点开看具体数字（来源等细节在右栏的上下文页签）；主 Session 在这里压缩上下文（conversation/compact），
// 一轮还在执行时不能压缩并写明原因
export function ContextUsageButton({
  sync,
  sessionInfo,
  canCompact,
  disabled,
}: {
  sync: ConversationSync
  sessionInfo?: SessionRuntimeInfo
  canCompact: boolean
  disabled: boolean
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const [openConversationID, setOpenConversationID] = useState<string>()
  if (!sessionInfo && openConversationID !== undefined)
    setOpenConversationID(undefined)
  const compact = useMutation({
    mutationFn: (conversationID: string) =>
      connection.call("conversation/compact", {
        conversation_id: conversationID,
      }),
    onSuccess: (_, conversationID) =>
      setOpenConversationID((opened) =>
        opened === conversationID ? undefined : opened
      ),
    onError: (error) => notifyFailure(error, t),
  })
  const isCompacting =
    compact.isPending && compact.variables === sync.conversationID
  const tokens = sessionInfo?.context.tokens ?? 0
  const contextWindow = sessionInfo?.context.window ?? 0
  const isMeasured = !!sessionInfo && sessionInfo.context.source !== "unknown"
  const unknown = t("conversation.context.source.unknown")
  const isRunning = !!sessionInfo?.active_turn_id
  const percent =
    contextWindow > 0 ? Math.round((tokens / contextWindow) * 100) : undefined
  const summary = !isMeasured
    ? unknown
    : percent === undefined
      ? t("conversation.context.tokensOnly", {
          tokens: formatNumberUnit(tokens),
        })
      : t("conversation.context.ratio", {
          tokens: formatNumberUnit(tokens),
          window: formatNumberUnit(contextWindow),
          percent,
        })
  return (
    <Popover
      open={!!sessionInfo && openConversationID === sync.conversationID}
      onOpenChange={(next) =>
        setOpenConversationID(next ? sync.conversationID : undefined)
      }
    >
      <PopoverTrigger
        render={
          <Button
            variant="ghost"
            className="shrink-0 px-1 text-ui transition-colors"
            disabled={!sessionInfo}
          />
        }
        aria-label={t("conversation.context.label", { summary })}
      >
        {!isMeasured ? (
          <DataGlyph
            kind="ring"
            value={0}
            label={unknown}
            className="text-title"
          />
        ) : percent === undefined ? (
          <TokenNumberValue value={tokens} prefix="≈" interactive={false} />
        ) : (
          <>
            <DataGlyph
              kind="ring"
              value={percent}
              label={t("conversation.context.percent", { percent })}
              className="text-title"
            />
            <span className="tabular-nums max-md:hidden">{percent}%</span>
          </>
        )}
      </PopoverTrigger>
      <PopoverContent align="end" className="w-64 gap-2 text-body">
        <div className="flex items-center gap-1 text-token">
          {isMeasured ? (
            <TokenNumberValue value={tokens} prefix="≈" />
          ) : (
            unknown
          )}
          {contextWindow > 0 && (
            <>
              {" "}
              / <TokenNumberValue value={contextWindow} />
            </>
          )}
        </div>
        {canCompact && (
          <div className="flex flex-col gap-2">
            <Separator />
            <Button
              variant="outline"
              className="self-start text-ui"
              disabled={disabled || !sessionInfo || isRunning || isCompacting}
              onClick={() => {
                if (
                  disabled ||
                  !sessionInfo ||
                  connection.$state.get() !== "connected"
                )
                  return
                compact.mutate(sync.conversationID)
              }}
            >
              {isCompacting ? (
                <LatticeLoader
                  data-icon="inline-start"
                  aria-label={t("common.loading")}
                />
              ) : (
                <FoldVerticalIcon data-icon="inline-start" />
              )}
              {t("conversation.context.compact")}
            </Button>
            {isRunning && (
              <p className="text-caption color-muted">
                {t("conversation.context.compactWhileRunning")}
              </p>
            )}
          </div>
        )}
      </PopoverContent>
    </Popover>
  )
}
