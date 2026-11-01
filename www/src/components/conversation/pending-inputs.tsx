import { useState } from "react"
import {
  AnimatePresence,
  motion,
  useIsPresent,
  useReducedMotion,
} from "motion/react"
import {
  ArrowUpIcon,
  ChevronRightIcon,
  EllipsisIcon,
  ImageIcon,
  PencilIcon,
} from "lucide-react"
import { useStore } from "@nanostores/react"
import { useMutation } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { ConfigurationChangeDialog } from "@/components/base/configuration-change-dialog"
import { RequestError } from "@/components/base/request-error"
import { RotatingText } from "@/components/base/rotating-text"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import {
  Collapsible,
  CollapsibleTrigger,
  CollapsibleContent,
} from "@/components/ui/collapsible"
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu"
import { useServerConnection } from "@/components/server-connection-provider"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type { PendingInput } from "@/lib/api/protocol.gen"
import { setDraft } from "@/lib/drafts"
import { notifyFailure } from "@/lib/notify"

// 在输入框上沿展示服务端待处理输入：标题行可折叠，收起时预览下一条；支持移动、取回及确认撤回。
export function PendingInputs({ sync }: { sync: ConversationSync }) {
  const { t } = useTranslation()
  const reducedMotion = useReducedMotion()
  const pending = useStore(sync.$pendingInputs)
  const followedSessionID = sync.getFollowedSessionID()
  const activeTurnID = useStore(sync.$sessions, { keys: [followedSessionID] })[
    followedSessionID
  ]?.active_turn_id
  const connection = useServerConnection()
  const isSynced = useStore(sync.$syncState) === "synced"
  const isConnected = useStore(connection.$state) === "connected"
  const isReady = isSynced && isConnected
  const [open, setOpen] = useState(true)
  const [withdrawTarget, setWithdrawTarget] = useState<PendingInput>()
  const action = useMutation({
    mutationFn: async ({
      input,
      kind,
    }: {
      input: PendingInput
      kind: "retrieve" | "steer" | "interrupt"
    }) => {
      // 1. 直接调用服务端原子操作，取回成功才覆盖草稿。
      if (kind === "retrieve") {
        const content = await sync.withdrawInput(input.item_id, true)
        setDraft(sync.serverID, sync.conversationID, { content: content! })
      } else if (kind === "steer") {
        await sync.steerInput(input.item_id, pending.steer_turn_id!)
      } else {
        await sync.interruptTurnWithInput(activeTurnID!, input.item_id)
      }
    },
    onError: (error) => notifyFailure(error, t),
  })
  const withdraw = useMutation({
    mutationFn: (input: PendingInput) =>
      sync.withdrawInput(input.item_id, false),
    onSuccess: () => {
      setWithdrawTarget(undefined)
    },
  })
  const hasPending = pending.inputs.length > 0
  // 服务端先列补充、再按排队顺序列普通输入，第一条就是下一条要处理的。
  const nextInput = pending.inputs[0]
  const pendingCount = String(pending.inputs.length)
  const transition = {
    duration: reducedMotion ? 0 : 0.2,
    ease: [0.22, 1, 0.36, 1] as const,
  }
  return (
    <>
      <div className="contents" inert={!hasPending} aria-hidden={!hasPending}>
        <AnimatePresence initial={false}>
          {hasPending && (
            <motion.section
              key="pending"
              aria-label={t("conversation.pending.title")}
              initial={{ height: 0, opacity: 0 }}
              animate={{ height: "auto", opacity: 1 }}
              exit={{ height: 0, opacity: 0 }}
              transition={transition}
              className="order-first w-full shrink-0 overflow-hidden"
            >
              <Collapsible
                open={open}
                onOpenChange={setOpen}
                className="border-b"
              >
                <CollapsibleTrigger
                  render={
                    <Button
                      type="button"
                      variant="ghost"
                      className="group/pending-trigger h-8 w-full justify-start gap-2 px-3 text-ui aria-expanded:bg-transparent aria-expanded:hover:bg-muted dark:aria-expanded:hover:bg-muted/50"
                    />
                  }
                >
                  <ChevronRightIcon className="transition-transform duration-200 ease-out group-data-panel-open/pending-trigger:rotate-90 motion-reduce:transition-none" />
                  {t("conversation.pending.title")}{" "}
                  <span className="shrink-0 color-muted tabular-nums">
                    <RotatingText
                      text={pendingCount}
                      transitionKey={pendingCount}
                    />
                  </span>
                  <AnimatePresence initial={false}>
                    {!open && nextInput && (
                      <motion.span
                        key="next-input"
                        initial={{ opacity: 0 }}
                        animate={{ opacity: 1 }}
                        exit={{ opacity: 0 }}
                        transition={{ duration: reducedMotion ? 0 : 0.16 }}
                        className="flex min-w-0 flex-1 items-center gap-2 font-normal color-muted"
                      >
                        {nextInput.is_steer && (
                          <span className="shrink-0 text-caption">
                            {t("conversation.pending.steer")}
                          </span>
                        )}
                        <RotatingText
                          text={
                            nextInput.preview ||
                            (nextInput.image_count > 0
                              ? t("conversation.pending.images", {
                                  count: nextInput.image_count,
                                })
                              : t("conversation.pending.message"))
                          }
                          transitionKey={nextInput.item_id}
                        />
                      </motion.span>
                    )}
                  </AnimatePresence>
                </CollapsibleTrigger>
                <CollapsibleContent className="process-disclosure-panel">
                  <div
                    data-scroll-density="compact"
                    className="process-disclosure-content max-h-24 overflow-y-auto"
                  >
                    <AnimatePresence initial={false} propagate>
                      {pending.inputs.map((input) => (
                        <motion.div
                          key={input.item_id}
                          initial={{ height: 0, opacity: 0 }}
                          animate={{ height: "auto", opacity: 1 }}
                          exit={{ height: 0, opacity: 0 }}
                          transition={transition}
                          className="overflow-hidden"
                        >
                          <PendingInputRow
                            input={input}
                            steerTurnID={pending.steer_turn_id}
                            activeTurnID={activeTurnID}
                            isReady={isReady}
                            actionPending={action.isPending}
                            onAction={(kind) => action.mutate({ input, kind })}
                            onWithdraw={() => {
                              withdraw.reset()
                              setWithdrawTarget(input)
                            }}
                          />
                        </motion.div>
                      ))}
                    </AnimatePresence>
                  </div>
                </CollapsibleContent>
              </Collapsible>
            </motion.section>
          )}
        </AnimatePresence>
      </div>
      <ConfigurationChangeDialog
        open={!!withdrawTarget}
        title={t("conversation.pending.withdrawTitle", {
          name: withdrawTarget?.preview || t("conversation.pending.message"),
        })}
        description={t("conversation.pending.withdrawDescription")}
        confirmLabel={t("conversation.pending.withdraw")}
        pending={withdraw.isPending}
        destructive
        onConfirm={() => withdrawTarget && withdraw.mutate(withdrawTarget)}
        onOpenChange={(next) => {
          if (!next) {
            setWithdrawTarget(undefined)
            withdraw.reset()
          }
        }}
        preview={
          withdraw.isError ? <RequestError error={withdraw.error} /> : undefined
        }
      />
    </>
  )
}

// 待处理输入保持单行，退场时关闭菜单并禁用全部操作。
function PendingInputRow({
  input,
  steerTurnID,
  activeTurnID,
  isReady,
  actionPending,
  onAction,
  onWithdraw,
}: {
  input: PendingInput
  steerTurnID?: string
  activeTurnID?: string
  isReady: boolean
  actionPending: boolean
  onAction(kind: "retrieve" | "steer" | "interrupt"): void
  onWithdraw(): void
}) {
  const { t } = useTranslation()
  const isPresent = useIsPresent()
  const [menuOpen, setMenuOpen] = useState(false)
  // 1. 退出动画开始时关闭挂载在 body 上的菜单。
  if (!isPresent && menuOpen) setMenuOpen(false)
  return (
    <div
      data-pending-input={input.item_id}
      className="flex h-8 min-w-0 items-center gap-2 px-3"
    >
      {input.is_steer && (
        <span className="shrink-0 text-caption color-muted">
          {t("conversation.pending.steer")}
        </span>
      )}
      <div className="flex min-w-0 flex-1 items-center gap-2">
        {input.preview ? (
          <p className="min-w-0 flex-1 truncate text-body">{input.preview}</p>
        ) : (
          input.image_count === 0 && (
            <p className="min-w-0 flex-1 truncate text-body color-muted">
              {t("conversation.pending.message")}
            </p>
          )
        )}
        {input.image_count > 0 && (
          <span className="flex shrink-0 items-center gap-1 text-caption color-muted">
            <ImageIcon className="size-4" />
            {t("conversation.pending.images", {
              count: input.image_count,
            })}
          </span>
        )}
      </div>
      <div className="flex shrink-0 items-center gap-2">
        <Tooltip disabled={!isPresent}>
          <TooltipTrigger
            render={
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label={t("conversation.pending.retrieve")}
                disabled={!isPresent || !isReady || actionPending}
                focusableWhenDisabled={isPresent}
                onClick={() => onAction("retrieve")}
              />
            }
          >
            <PencilIcon />
          </TooltipTrigger>
          <TooltipContent className="text-caption">
            {t("conversation.pending.retrieve")}
          </TooltipContent>
        </Tooltip>
        {!input.is_steer && steerTurnID && (
          <Tooltip disabled={!isPresent}>
            <TooltipTrigger
              render={
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  aria-label={t("conversation.pending.join")}
                  disabled={!isPresent || !isReady || actionPending}
                  focusableWhenDisabled={isPresent}
                  onClick={() => onAction("steer")}
                />
              }
            >
              <ArrowUpIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("conversation.pending.join")}
            </TooltipContent>
          </Tooltip>
        )}
        <DropdownMenu open={isPresent && menuOpen} onOpenChange={setMenuOpen}>
          <Tooltip disabled={!isPresent}>
            <TooltipTrigger
              render={
                <DropdownMenuTrigger
                  render={
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      aria-label={t("conversation.pending.more")}
                      disabled={!isPresent}
                    />
                  }
                />
              }
            >
              <EllipsisIcon />
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t("conversation.pending.more")}
            </TooltipContent>
          </Tooltip>
          <DropdownMenuContent
            align="end"
            inert={!isPresent}
            aria-hidden={!isPresent}
          >
            {!input.is_steer && activeTurnID && (
              <>
                <DropdownMenuGroup>
                  <DropdownMenuItem
                    className="text-ui"
                    disabled={!isPresent || !isReady || actionPending}
                    onClick={() => onAction("interrupt")}
                  >
                    {t("conversation.pending.prioritize")}
                  </DropdownMenuItem>
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
              </>
            )}
            <DropdownMenuGroup>
              <DropdownMenuItem
                className="text-ui"
                variant="destructive"
                disabled={!isPresent || !isReady}
                onClick={onWithdraw}
              >
                {t("conversation.pending.withdraw")}
              </DropdownMenuItem>
            </DropdownMenuGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>
  )
}
