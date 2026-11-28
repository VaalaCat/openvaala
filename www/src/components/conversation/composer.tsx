import {
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
  type ClipboardEvent,
  type DragEvent,
  type KeyboardEvent,
} from "react"
import {
  BookOpen,
  SquareTerminal,
  ListEndIcon,
  PlusIcon,
  XIcon,
} from "lucide-react"
import { ArrowUp, Square } from "lucide"
import { MorphIcon } from "morphicons/react"
import { AnimatePresence, motion, useIsPresent } from "motion/react"
import { useStore } from "@nanostores/react"
import { useMutation, useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useServerConnection } from "@/components/server-connection-provider"
import { BrandIcon } from "@/components/base/brand-icon"
import { AttachmentStrip } from "./attachment-strip"
import { Button } from "@/components/ui/button"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupTextarea,
} from "@/components/ui/input-group"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import {
  Collapsible,
  CollapsibleTrigger,
  CollapsibleContent,
} from "@/components/ui/collapsible"
import { PendingInputs } from "@/components/conversation/pending-inputs"
import { RequestError } from "@/components/base/request-error"
import { shouldShowLoadError } from "@/lib/rpc-error"
import { LatticeLoader } from "@/components/base/lattice-loader"
import type { ConversationSync } from "@/lib/api/conversation-sync"
import type {
  AgentInfo,
  ExternalCommand,
  Content,
  ConversationID,
  SessionInputMode,
  SkillInfo,
} from "@/lib/api/protocol.gen"
import {
  attachmentStatus,
  attachmentBudgetBytes,
  checkAttachmentBudget,
  getImageBytes,
  readImageAsDataURL,
  uploadDraftFiles,
} from "@/lib/attachments"
import {
  getSideConversationQuestion,
  getSlashToken,
  shouldSubmitOnKeyDown,
} from "@/lib/composer-keys"
import {
  $drafts,
  getDraft,
  getDraftKey,
  newConversationDraftID,
  removeSentContent,
  removeSentText,
  getDraftText,
  replaceDraftText,
  setDraft,
} from "@/lib/drafts"
import { notifyFailure } from "@/lib/notify"
import { skillListOptions, type SkillTarget } from "@/lib/query/skill"
import { getRpcErrorMessage } from "@/lib/rpc-error"
import { cn } from "@/lib/utils"

export type ComposerProps = {
  externalAgent?: AgentInfo
  commands?: ExternalCommand[]
  supportsSteer?: boolean
} &
  // 对话页：提交到这个对话；isRunning 时可以补充、排队、插队和中断。
  // 给了 onSideConversation 才有内置的 /btw：输入以 /btw 开头时调用它开旁支，成功后清掉输入框里的文字，失败时抛错、文字保留
  (
    | {
        serverID: string
        conversationID: ConversationID
        sync: ConversationSync
        uploadFile(file: File): Promise<Content>
        isReady: boolean
        isRunning: boolean
        sessionControls: ReactNode
        contextUsage: ReactNode
        onSideConversation?(question: string): Promise<void>
        // 输入框挂上时是否直接获得焦点
        autoFocus?: boolean
      }
    // 新对话页：建对话并提交第一条输入；返回 false 时页面已在字段旁写明没提交的原因，其他失败抛错。
    // 斜杠补全按还没建的对话的 Agent 和 Executor 设置查 skill
    | {
        serverID: string
        onCreateAndSubmit(content: Content[]): Promise<boolean>
        sessionControls: ReactNode
        skillTarget: SkillTarget
      }
  )

// 输入完整草稿、补全 Skill，并发送或排队；成功后只移除已发送的内容。
export function Composer(props: ComposerProps) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const sync = "sync" in props ? props.sync : undefined
  const onSideConversation =
    "sync" in props ? props.onSideConversation : undefined
  const isRunning = "sync" in props && props.isRunning
  const serverID = props.serverID
  const draftID =
    "conversationID" in props ? props.conversationID : newConversationDraftID
  const draftKey = getDraftKey(serverID, draftID)
  const isReady = !("sync" in props) || props.isReady
  const draft =
    useStore($drafts, { keys: [draftKey] })[draftKey] ??
    getDraft(serverID, draftID)
  const text = getDraftText(draft.content)
  const attachments = draft.content.filter((item) => item.image || item.file)
  const materials = draft.content.flatMap((content, index) =>
    content.kind === "text" && content.text?.is_context
      ? [{ content, index }]
      : []
  )
  const isConnected = useStore(connection.$state) === "connected"
  // 最近一次加图片没加进去的原因，显示在输入框里；再加图片、移除图片或发送成功后清掉

  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  // 补全插入后光标要放的位置，文字更新后的 layout effect 里放好
  const pendingCaretRef = useRef<number | undefined>(undefined)
  useLayoutEffect(() => {
    pendingCaretRef.current = undefined
    if (fileInputRef.current) fileInputRef.current.value = ""
  }, [draftKey])
  // 返回这条输入是否已提交
  const submit = useMutation({
    mutationFn: async (variables: {
      draftKey: string
      target: ComposerProps
      // 输入是 /btw 命令时开旁支，不当作普通输入发送
      openSideConversation?: () => Promise<void>
      content: Content[]
      mode: SessionInputMode
    }): Promise<boolean> => {
      if (variables.openSideConversation) {
        await variables.openSideConversation()
        return true
      }
      const target = variables.target
      if ("onCreateAndSubmit" in target)
        return target.onCreateAndSubmit(variables.content)
      // 新建页发送期间追加的文件会随草稿进入已创建的对话。
      const content = await uploadDraftFiles(
        variables.content,
        target.sync.conversationID,
        target.uploadFile
      )
      await target.sync.submitInput(content, variables.mode)
      return true
    },
  })

  // 1. 斜杠补全：光标所在的词以 / 开头时按前缀匹配 skill；对话页的输入开头还有内置的 /btw，排在 skill 前面；Esc 收起，再改文字时重新出现。
  const listboxID = useId()
  const [cursor, setCursor] = useState(0)
  const [isSlashDismissed, setIsSlashDismissed] = useState(false)
  const [highlightedIndex, setHighlightedIndex] = useState(0)
  const [inputTarget, setInputTarget] = useState(draftKey)
  if (inputTarget !== draftKey) {
    setInputTarget(draftKey)
    setCursor(0)
    setIsSlashDismissed(true)
    setHighlightedIndex(0)
  }
  const slashToken = isSlashDismissed ? undefined : getSlashToken(text, cursor)
  const skills = useQuery({
    ...skillListOptions(
      connection,
      "sync" in props
        ? { conversationID: props.sync.conversationID }
        : props.skillTarget
    ),
    enabled: !props.externalAgent && !!slashToken && isReady && isConnected,
  })
  const builtinCommands: SlashCommand[] =
    onSideConversation && slashToken?.start === 0
      ? [
          {
            name: "btw",
            description: t("composer.btwDescription"),
            type: "builtin",
          },
        ]
      : []
  const matches = slashToken
    ? matchSkills(
        [
          ...builtinCommands,
          ...(props.externalAgent
            ? (props.commands ?? [])
                .filter((c) => !builtinCommands.some((b) => b.name === c.name))
                .map((c) => ({ ...c, type: "external" as const }))
            : (skills.data ?? []).map((c) => ({
                ...c,
                type: "skill" as const,
              }))),
        ],
        slashToken.prefix
      )
    : []
  const isSlashOpen =
    isReady &&
    isConnected &&
    !!slashToken &&
    ((!props.externalAgent && (skills.isPending || skills.isError)) ||
      matches.length > 0)
  const highlighted = Math.min(
    highlightedIndex,
    Math.max(matches.length - 1, 0)
  )
  useEffect(() => {
    if (isSlashOpen)
      document
        .getElementById(listboxID + "-" + highlighted)
        ?.scrollIntoView({ block: "nearest" })
  }, [isSlashOpen, listboxID, highlighted])

  // 2. 高度随内容增高（最高视口 40%，由样式限制，超出后框内滚动）；补全插入后把光标放到插入内容后面。
  useLayoutEffect(() => {
    const textarea = textareaRef.current
    if (!textarea) return
    textarea.style.height = "auto"
    textarea.style.height = textarea.scrollHeight + "px"
    if (pendingCaretRef.current !== undefined) {
      textarea.setSelectionRange(
        pendingCaretRef.current,
        pendingCaretRef.current
      )
      pendingCaretRef.current = undefined
    }
  }, [text])

  const hasContent = draft.content.some(
    (each) => each.kind !== "text" || !!each.text?.text.trim()
  )
  const isSubmitting =
    submit.isPending && submit.variables?.draftKey === draftKey
  const canSubmit =
    isReady &&
    isConnected &&
    !isSubmitting &&
    hasContent &&
    !attachments.some((item) => {
      const status = attachmentStatus.get(item)
      return status?.loading || (status?.error && !status.file)
    })

  // 提交完整内容，成功后移除本次发送的部分。
  async function handleSubmit(mode: SessionInputMode = "steer") {
    if (!canSubmit) return
    // 1. 保存本次发出的内容引用，不改写空白、材料和图片顺序。
    const content = draft.content
    const sideQuestion = onSideConversation
      ? getSideConversationQuestion(text)
      : undefined
    // 2. 提交；新对话页已在字段旁说明没提交的原因时，不提示也不清空。
    let isSubmitted: boolean
    try {
      isSubmitted = await submit.mutateAsync({
        draftKey,
        target: props,
        openSideConversation:
          sideQuestion === undefined
            ? undefined
            : () => onSideConversation!(sideQuestion),
        content,
        mode:
          mode === "steer" && isRunning && props.supportsSteer === false
            ? "interrupt"
            : mode,
      })
    } catch (error) {
      notifyFailure(error, t)
      return
    }
    if (!isSubmitted) return
    // 3. 发送期间又追加的文字和新加的图片留着，只去掉已发出的部分；/btw 只用了文字，图片和材料都留着。
    const latest = getDraft(serverID, draftID)
    setDraft(
      serverID,
      draftID,
      sideQuestion === undefined
        ? removeSentContent(latest, content)
        : {
            content: replaceDraftText(
              latest.content,
              removeSentText(getDraftText(latest.content), text)
            ),
          }
    )
  }

  async function addFiles(files: File[]) {
    const entries = files.map((file): Content => ({
      kind: "file",
      file: { name: file.name, mime: file.type, size: file.size, path: "" },
    }))
    const latest = getDraft(serverID, draftID)
    let inlineBytes = latest.content.reduce(
      (bytes, item) =>
        bytes +
        (item.image
          ? getImageBytes(item.image.data_url)
          : item.file?.mime.startsWith("image/") &&
              attachmentStatus.get(item)?.loading
            ? item.file.size
            : 0),
      0
    )
    for (const [i, item] of entries.entries()) {
      const file = files[i]
      const tooLarge = file.size > attachmentBudgetBytes
      const exceedsBudget =
        !tooLarge &&
        file.type.startsWith("image/") &&
        !checkAttachmentBudget([inlineBytes], [file.size]).ok
      const error = tooLarge
        ? t("externalSession.tooLarge")
        : exceedsBudget
          ? t("composer.attachmentsTooLarge")
          : undefined
      if (!error && file.type.startsWith("image/")) inlineBytes += file.size
      const stageFile =
        !error && !file.type.startsWith("image/") && !("sync" in props)
      attachmentStatus.set(item, {
        id: crypto.randomUUID?.() ?? Math.random().toString(36),
        loading: !error && !stageFile,
        file: stageFile ? file : undefined,
        error,
      })
    }
    setDraft(serverID, draftID, { content: [...latest.content, ...entries] })
    await Promise.all(
      entries.map(async (item, i) => {
        const status = attachmentStatus.get(item)!
        if (!status.loading) return
        const file = files[i]
        let result = item
        try {
          if (file.type.startsWith("image/")) {
            result = {
              kind: "image",
              image: {
                alt: file.name,
                data_url: await readImageAsDataURL(file),
              },
            }
          } else if ("sync" in props) {
            result = await props.uploadFile(file)
          }
          attachmentStatus.set(result, { id: status.id })
        } catch (error) {
          attachmentStatus.set(item, {
            id: status.id,
            error: getRpcErrorMessage(error, t),
          })
        }
        const draft = getDraft(serverID, draftID)
        if (draft.content.includes(item))
          setDraft(serverID, draftID, {
            content: draft.content.map((c) => (c === item ? result : c)),
          })
      })
    )
  }

  // 用选中的 skill 换掉光标处的斜杠词，后面补一个空格
  function insertSkill(skill: SlashCommand) {
    if (!slashToken) return
    const replacement = "/" + skill.name + " "
    pendingCaretRef.current = slashToken.start + replacement.length
    setCursor(pendingCaretRef.current)
    setDraft(serverID, draftID, {
      content: replaceDraftText(
        getDraft(serverID, draftID).content,
        text.slice(0, slashToken.start) +
          replacement +
          text.slice(slashToken.end)
      ),
    })
  }

  // 补全列表打开时的按键：↑ ↓ 选择，Enter 和 Tab 插入，Esc 收起；返回按键是否已处理
  function handleSlashKey(event: KeyboardEvent<HTMLTextAreaElement>): boolean {
    if (event.key === "Escape") {
      setIsSlashDismissed(true)
      return true
    }
    if (matches.length === 0) return false
    if (event.key === "Enter" && matchMedia("(pointer: coarse)").matches)
      return false
    switch (event.key) {
      case "ArrowDown":
        setHighlightedIndex((highlighted + 1) % matches.length)
        return true
      case "ArrowUp":
        setHighlightedIndex((highlighted - 1 + matches.length) % matches.length)
        return true
      case "Enter":
      case "Tab":
        if (event.shiftKey) return false
        insertSkill(matches[highlighted])
        return true
    }
    return false
  }

  function handleKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    const { isComposing, keyCode } = event.nativeEvent
    // 1. 输入法组字中的按键交给输入法；补全列表打开时先由它处理。
    if (
      isSlashOpen &&
      !isComposing &&
      keyCode !== 229 &&
      handleSlashKey(event)
    ) {
      event.preventDefault()
      return
    }
    // 2. Enter 发送，Shift+Enter 换行；触屏键盘没有 Shift，Enter 只换行。
    if (
      shouldSubmitOnKeyDown({
        key: event.key,
        shiftKey: event.shiftKey,
        isComposing,
        keyCode,
        isTouchPrimary: matchMedia("(pointer: coarse)").matches,
      })
    ) {
      event.preventDefault()
      void handleSubmit()
    }
  }

  // 剪贴板含文件时加入附件，纯文字照常粘贴。
  function handlePaste(event: ClipboardEvent<HTMLTextAreaElement>) {
    const files = Array.from(event.clipboardData.files)
    if (files.length === 0) return
    event.preventDefault()
    void addFiles(files)
  }

  // 拖入的文件加入这条输入。
  function handleDrop(event: DragEvent<HTMLDivElement>) {
    if (!event.dataTransfer.types.includes("Files")) return
    event.preventDefault()
    void addFiles(Array.from(event.dataTransfer.files))
  }

  return (
    <form
      className="relative"
      onSubmit={(event) => {
        event.preventDefault()
        void handleSubmit()
      }}
    >
      {sync && sync.hasInitialData() && <ConversationSyncError sync={sync} />}
      {isSlashOpen && (
        <SlashSkillList
          id={listboxID}
          skills={{
            ...skills,
            isPending: !props.externalAgent && skills.isPending,
            isError: !props.externalAgent && skills.isError,
          }}
          icon={props.externalAgent?.icon ?? ""}
          matches={matches}
          highlighted={highlighted}
          onHighlight={setHighlightedIndex}
          onPick={insertSkill}
        />
      )}
      {/* 发送按钮禁用时整组不变灰；输入框自己从不禁用 */}
      <InputGroup
        className="has-disabled:bg-transparent has-disabled:opacity-100 dark:has-disabled:bg-input/30"
        onDragOver={(event) => {
          if (event.dataTransfer.types.includes("Files")) event.preventDefault()
        }}
        onDrop={handleDrop}
      >
        {sync && sync.hasInitialData() && (
          <PendingInputs key={`pending:${draftKey}`} sync={sync} />
        )}
        <div
          className={cn(
            "order-first w-full min-w-0",
            attachments.length > 0 && "pt-2"
          )}
        >
          <AttachmentStrip
            contents={attachments}
            onRemove={(item) => {
              const latest = getDraft(serverID, draftID)
              setDraft(serverID, draftID, {
                content: latest.content.filter((c) => c !== item),
              })
            }}
          />
        </div>
        {materials.length > 0 && (
          <InputGroupAddon
            align="block-start"
            className="flex-col items-stretch gap-2 px-3"
          >
            {materials.map(({ content, index }) => (
              <div key={index} className="flex min-w-0 items-start gap-2">
                <Collapsible className="min-w-0 flex-1">
                  <CollapsibleTrigger className="text-ui text-help">
                    {t("composer.contextMaterial")}
                  </CollapsibleTrigger>
                  <CollapsibleContent className="max-h-40 overflow-auto text-body wrap-anywhere whitespace-pre-wrap">
                    {content.text?.text}
                  </CollapsibleContent>
                </Collapsible>
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon-xs"
                        aria-label={t("composer.removeMaterial")}
                        onClick={() =>
                          setDraft(serverID, draftID, {
                            content: getDraft(serverID, draftID).content.filter(
                              (_, position) => position !== index
                            ),
                          })
                        }
                      />
                    }
                  >
                    <XIcon />
                  </TooltipTrigger>
                  <TooltipContent className="text-caption">
                    {t("composer.removeMaterial")}
                  </TooltipContent>
                </Tooltip>
              </div>
            ))}
          </InputGroupAddon>
        )}
        <InputGroupTextarea
          ref={textareaRef}
          rows={1}
          value={text}
          aria-label={t("composer.label")}
          placeholder={t(
            props.externalAgent
              ? "externalSession.placeholder"
              : "composer.placeholder"
          )}
          autoFocus={"autoFocus" in props && props.autoFocus}
          aria-autocomplete="list"
          aria-controls={isSlashOpen ? listboxID : undefined}
          aria-activedescendant={
            isSlashOpen && matches.length > 0
              ? listboxID + "-" + highlighted
              : undefined
          }
          className="max-h-[40svh] min-h-10 overflow-y-auto px-3 text-body"
          onChange={(event) => {
            setDraft(serverID, draftID, {
              content: replaceDraftText(
                getDraft(serverID, draftID).content,
                event.target.value
              ),
            })
            setCursor(event.target.selectionStart)
            setIsSlashDismissed(false)
            setHighlightedIndex(0)
          }}
          onSelect={(event) => setCursor(event.currentTarget.selectionStart)}
          onKeyDown={handleKeyDown}
          onPaste={handlePaste}
        />
        <InputGroupAddon
          align="block-end"
          className="gap-1 px-2 text-foreground"
        >
          <div className="flex min-w-0 flex-1 items-center gap-1">
            <Tooltip>
              <TooltipTrigger
                render={
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    className="h-8 w-7"
                    aria-label={t("externalSession.add")}
                    onClick={() => fileInputRef.current?.click()}
                  />
                }
              >
                <PlusIcon />
              </TooltipTrigger>
              <TooltipContent className="text-caption">
                {t("externalSession.add")}
              </TooltipContent>
            </Tooltip>
            {props.sessionControls}
          </div>
          <div className="flex shrink-0 items-center gap-1">
            {"contextUsage" in props && props.contextUsage}
            <SendControls
              conversationID={sync?.conversationID}
              isRunning={isRunning}
              isConnected={isConnected && isReady}
              canSubmit={canSubmit}
              isSubmitting={isSubmitting}
              hasContent={hasContent}
              onQueue={() => void handleSubmit("normal")}
            />
          </div>
        </InputGroupAddon>
      </InputGroup>
      <input
        ref={fileInputRef}
        type="file"
        multiple
        hidden
        onChange={(event) => {
          void addFiles(Array.from(event.target.files ?? []))
          event.target.value = ""
        }}
      />
    </form>
  )
}

// 已有对话内容后同步失败时，在输入框上方显示错误。
function ConversationSyncError({ sync }: { sync: ConversationSync }) {
  const error = useStore(sync.$syncError)
  if (!error || sync.$restoreError.get() || !shouldShowLoadError(error))
    return null
  return <RequestError error={error} />
}

// 斜杠补全列表：先列出匹配的内置命令和 skill，查 skill 时在后面显示加载格点、失败时写原因；点选不让输入框失去焦点
function SlashSkillList({
  id,
  icon,
  skills,
  matches,
  highlighted,
  onHighlight,
  onPick,
}: {
  id: string
  skills: { isPending: boolean; isError: boolean; error: unknown }
  icon: string
  matches: SlashCommand[]
  highlighted: number
  onHighlight(index: number): void
  onPick(skill: SlashCommand): void
}) {
  const { t } = useTranslation()
  return (
    <div
      id={id}
      role="listbox"
      aria-label={t("composer.skills")}
      className="absolute inset-x-0 bottom-full z-20 mb-2 max-h-64 overflow-y-auto border bg-popover p-1 text-popover-foreground shadow-md"
    >
      {matches.map((skill, index) => (
        <div
          key={skill.name}
          id={id + "-" + index}
          role="option"
          aria-selected={index === highlighted}
          className="flex cursor-default items-start gap-2 px-2 py-1 aria-selected:bg-accent aria-selected:text-accent-foreground"
          onMouseDown={(event) => event.preventDefault()}
          onMouseEnter={() => onHighlight(index)}
          onClick={() => onPick(skill)}
        >
          <span className="mt-1 flex shrink-0">
            {skill.type === "external" ? (
              <BrandIcon icon={icon} />
            ) : skill.type === "builtin" ? (
              <SquareTerminal className="size-4" />
            ) : (
              <BookOpen className="size-4" />
            )}
          </span>
          <div className="flex min-w-0 flex-col">
            <span className="text-table wrap-anywhere">/{skill.name}</span>
            {skill.description && (
              <span className="truncate text-caption color-muted">
                {skill.description}
              </span>
            )}
          </div>
        </div>
      ))}
      {skills.isPending ? (
        <div className="flex items-center gap-2 px-2 py-2 text-caption color-muted">
          <LatticeLoader aria-hidden />
          {t("common.loading")}
        </div>
      ) : skills.isError ? (
        <p className="px-2 py-2 text-caption wrap-anywhere color-destructive">
          {getRpcErrorMessage(skills.error, t)}
        </p>
      ) : null}
    </div>
  )
}

// 发送区按运行状态和草稿内容展示停止、排队与发送。
function SendControls({
  conversationID,
  isRunning,
  isConnected,
  canSubmit,
  isSubmitting,
  hasContent,
  onQueue,
}: {
  conversationID?: ConversationID
  isRunning: boolean
  isConnected: boolean
  canSubmit: boolean
  isSubmitting: boolean
  hasContent: boolean
  onQueue(): void
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const interrupt = useMutation({
    mutationFn: (id: ConversationID) =>
      connection.call("conversation/interruptTurn", { conversation_id: id }),
    onError: (error) => notifyFailure(error, t),
  })
  // 1. 操作和等待状态立即跟随当前目标，图标在同一按钮中切换。
  const showStop = isRunning && !hasContent
  const isInterrupting =
    interrupt.isPending && interrupt.variables === conversationID
  const isLoading = showStop ? isInterrupting : isSubmitting
  const label = showStop ? t("composer.interrupt") : t("composer.send")
  return (
    <div className="flex shrink-0 items-center">
      <AnimatePresence initial={false}>
        {isRunning && hasContent && (
          <QueueButton key="queue" disabled={!canSubmit} onQueue={onQueue} />
        )}
      </AnimatePresence>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              type={showStop ? "button" : "submit"}
              variant={showStop ? "destructive" : "default"}
              size="icon"
              className={cn(
                "transition-colors",
                showStop && "composer-interrupt"
              )}
              aria-label={label}
              aria-description={
                !isConnected ? t("composer.disconnected") : undefined
              }
              disabled={showStop ? !isConnected || isInterrupting : !canSubmit}
              onClick={
                showStop
                  ? () => conversationID && interrupt.mutate(conversationID)
                  : undefined
              }
              focusableWhenDisabled
            />
          }
        >
          <span className="relative flex size-4 items-center justify-center">
            <MorphIcon
              icon={showStop ? Square : ArrowUp}
              size={16}
              aria-hidden
              className={isLoading ? "invisible" : undefined}
            />
            {isLoading && (
              <LatticeLoader
                className="absolute"
                aria-label={t("common.loading")}
              />
            )}
          </span>
        </TooltipTrigger>
        <TooltipContent className="text-caption">{label}</TooltipContent>
      </Tooltip>
    </div>
  )
}

// 排队入口连同右侧间距进退，退场期间停止全部交互。
function QueueButton({
  disabled,
  onQueue,
}: {
  disabled: boolean
  onQueue(): void
}) {
  const { t } = useTranslation()
  const isPresent = useIsPresent()
  // 1. 按内容宽度展开或收起按钮及右侧间距，主按钮留在原处。
  return (
    <motion.div
      initial={{ width: 0, opacity: 0 }}
      animate={{ width: "auto", opacity: 1 }}
      exit={{ width: 0, opacity: 0 }}
      transition={{ duration: 0.2 }}
      className="shrink-0"
      inert={!isPresent}
      aria-hidden={!isPresent}
    >
      <div className="pr-1">
        <Tooltip disabled={!isPresent}>
          <TooltipTrigger
            render={
              <Button
                type="button"
                variant="outline"
                size="icon"
                className="transition-colors"
                aria-label={t("composer.queue")}
                disabled={disabled || !isPresent}
                focusableWhenDisabled={isPresent}
                onClick={isPresent ? onQueue : undefined}
              />
            }
          >
            <ListEndIcon />
          </TooltipTrigger>
          <TooltipContent className="text-caption">
            {t("composer.queue")}
          </TooltipContent>
        </Tooltip>
      </div>
    </motion.div>
  )
}

type SlashCommand = Pick<SkillInfo, "name"> & {
  description?: string
  type: "external" | "builtin" | "skill"
}

// 按前缀匹配 skill，不分大小写；插件 skill 名是「插件名:skill 名」，输入 skill 名开头也能匹配。同 TUI 的补全规则
function matchSkills(skills: SlashCommand[], prefix: string): SlashCommand[] {
  const lowered = prefix.toLowerCase()
  return skills.filter((skill) => {
    const name = skill.name.toLowerCase()
    return (
      name.startsWith(lowered) ||
      name.slice(name.indexOf(":") + 1).startsWith(lowered)
    )
  })
}
