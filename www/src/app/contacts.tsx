import { useLayoutEffect, useMemo, useRef, useState } from "react"
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query"
import { MessagesSquare, MessageSquarePlus } from "lucide"
import {
  ArrowLeftIcon,
  ArrowUpIcon,
  ArrowUpRightIcon,
  BotIcon,
  ServerIcon,
} from "lucide-react"
import { MorphIcon } from "morphicons/react"
import { useTranslation } from "react-i18next"
import { Navigate, useLocation, useSearchParams } from "react-router"
import { NavigationButton } from "@/components/base/navigation-button"
import { InfiniteList } from "@/components/base/infinite-list"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ListEmpty } from "@/components/base/list-empty"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { RequestError } from "@/components/base/request-error"
import { ScrollableTabsList } from "@/components/base/scrollable-tabs-list"
import { TabBar } from "@/components/base/tab-bar"
import { VirtualMessageList } from "@/components/base/virtual-message-list"
import { useServerConnection } from "@/components/server-connection-provider"
import { Input } from "@/components/ui/input"
import { Button, buttonVariants } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { Tabs, TabsContent, TabsTrigger } from "@/components/ui/tabs"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupTextarea,
} from "@/components/ui/input-group"
import {
  WorldContactList,
  WorldIconWithStatus,
  getVisibleTargets,
} from "@/components/world/contact-list"
import {
  WorldSendError,
  WorldMessageBubble,
  WorldMessageTime,
} from "@/components/world/message-bubble"
import {
  mergeSentMessagePage,
  shouldShowTimeDivider,
} from "@/components/world/message-bubble-helpers"
import { usePageNavigation } from "@/hooks/use-page-navigation"
import { useIsMobile } from "@/hooks/use-mobile"
import type { ListPeersResult } from "@/lib/api/protocol.gen"
import type { ConversationID, MethodParams } from "@/lib/api/protocol.gen"
import {
  parseWorldAddress,
  selectFollowUpThread,
  type WorldMessage,
  type WorldMessageDirection,
  type WorldPeer,
} from "@/lib/api/world"
import {
  getConversationListSearch,
  getConversationPath,
  getServerPath,
  type ContactsTab,
} from "@/lib/paths"
import { useNotifyIfUnmounted } from "@/lib/notify"
import { invalidateWorldQueries } from "@/lib/query/invalidation"
import {
  useWorldStatusState,
  worldMessagesInfiniteOptions,
  worldPeersOptions,
  worldThreadsInfiniteOptions,
  worldThreadsOptions,
} from "@/lib/query/world"
import { shouldSubmitOnKeyDown } from "@/lib/composer-keys"
import { cn } from "@/lib/utils"

const contactTabs = [
  ["contacts", "world.contacts"],
  ["messages", "world.messages"],
  ["threads", "world.threads"],
] as const satisfies ReadonlyArray<readonly [ContactsTab, string]>

export function ContactsPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const statusState = useWorldStatusState()
  const [searchParams, setSearchParams] = useSearchParams()
  const tab =
    contactTabs.find(([name]) => name === searchParams.get("tab"))?.[0] ??
    "contacts"
  const peerAddress = searchParams.get("peer") ?? ""
  usePageNavigation(t("world.contacts"))

  function selectPeer(address: string) {
    setSearchParams((params) => {
      const next = new URLSearchParams(params)
      if (address) next.set("peer", address)
      else next.delete("peer")
      return next
    })
  }

  if (!statusState || statusState.isPending)
    return <LatticeLoader className="m-3" aria-label={t("common.loading")} />
  if (statusState.isError && !statusState.status)
    return (
      <div className="p-3 md:p-6">
        <RequestError
          error={statusState.error}
          onRetry={() => void statusState.refetch()}
        />
      </div>
    )
  if (statusState.status?.enabled === false)
    return <Navigate to={getServerPath(connection.getServerID())} replace />
  if (!statusState.status) return null

  return (
    <Tabs
      value={tab}
      onValueChange={(value) => {
        setSearchParams((params) => {
          const next = new URLSearchParams(params)
          if (value === "contacts") next.delete("tab")
          else next.set("tab", value)
          return next
        })
      }}
      className="min-h-0 min-w-0 flex-1 gap-0"
    >
      <TabBar>
        <div className="flex items-center">
          {tab === "contacts" && peerAddress && (
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="mx-1 shrink-0 md:hidden"
              aria-label={t("world.backToContacts")}
              onClick={() => selectPeer("")}
            >
              <ArrowLeftIcon className="size-4" />
            </Button>
          )}
          <ScrollableTabsList aria-label={t("world.contacts")}>
            {contactTabs.map(([name, label]) => (
              <TabsTrigger key={name} value={name} className="text-ui">
                {t(label)}
              </TabsTrigger>
            ))}
          </ScrollableTabsList>
        </div>
      </TabBar>
      {statusState.isError && (
        <div className="shrink-0 px-3 pt-3 md:px-6">
          <RequestError
            error={statusState.error}
            onRetry={() => void statusState.refetch()}
          />
        </div>
      )}
      <TabsContent value="contacts" className="min-h-0 min-w-0">
        <ContactsDirectory peerAddress={peerAddress} onSelect={selectPeer} />
      </TabsContent>
      <TabsContent value="messages" className="min-h-0 min-w-0 overflow-y-auto">
        <MessageLogList />
      </TabsContent>
      <TabsContent value="threads" className="min-h-0 min-w-0 overflow-y-auto">
        <ThreadList />
      </TabsContent>
    </Tabs>
  )
}

function ContactsDirectory({
  peerAddress,
  onSelect,
}: {
  peerAddress: string
  onSelect(address: string): void
}) {
  const connection = useServerConnection()
  const { t } = useTranslation()
  const isMobile = useIsMobile()
  const peers = useQuery(worldPeersOptions(connection))
  const selected =
    peerAddress || (isMobile ? "" : (peers.data?.peers?.[0]?.address ?? ""))
  const selectedAddress = parseWorldAddress(selected)
  const selectedPeer = peers.data?.peers?.find(
    (peer) =>
      peer.address === selectedAddress?.nodeAddress &&
      (!selectedAddress.target ||
        peer.targets?.some((target) => target.name === selectedAddress.target))
  )

  return (
    <div className="flex h-full min-h-0 min-w-0">
      <div
        className={cn(
          "min-h-0 min-w-0 flex-1 overflow-y-auto md:block md:w-72 md:flex-none md:shrink-0 md:border-r",
          peerAddress && "hidden"
        )}
      >
        <form
          className="flex min-w-0 items-center gap-2 border-b p-2"
          onSubmit={(event) => {
            event.preventDefault()
            const address = String(
              new FormData(event.currentTarget).get("address") ?? ""
            ).trim()
            if (address) onSelect(address)
          }}
        >
          <Input
            key={selected}
            name="address"
            aria-label={t("world.contactAddress")}
            placeholder={t("world.addressFormat")}
            defaultValue={selectedPeer ? "" : selected}
            className="min-w-0 flex-1 text-ui"
          />
          <Button
            type="submit"
            variant="outline"
            size="icon"
            aria-label={t("world.openContact")}
          >
            <ArrowUpRightIcon />
          </Button>
        </form>
        <WorldContactList
          peers={peers}
          selected={selected}
          onSelect={onSelect}
          bordered={false}
        />
      </div>
      {selected && (
        <div
          className={cn(
            "min-h-0 min-w-0 flex-1 flex-col",
            peerAddress ? "flex" : "hidden md:flex"
          )}
        >
          <ContactHistory
            address={selected}
            peer={selectedPeer}
            peers={peers}
            isMobile={isMobile}
          />
        </div>
      )}
    </div>
  )
}

function ContactHistory({
  address,
  peer,
  peers,
  isMobile,
}: {
  address: string
  peer?: WorldPeer
  peers: UseQueryResult<ListPeersResult>
  isMobile: boolean
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const queryClient = useQueryClient()
  const parsedAddress = parseWorldAddress(address)
  const name =
    parsedAddress?.target || peer?.node_name || parsedAddress?.node || address
  const recentThread = peers.data?.recent?.find(
    (contact) => contact.address === address
  )?.thread_id
  const recentPeer = recentThread && parsedAddress
  const ContactIcon = parsedAddress?.target ? BotIcon : ServerIcon
  const hideSingleDefault =
    !parsedAddress?.target &&
    peer?.targets?.length === 1 &&
    getVisibleTargets(peer).length === 0
  const messagesOptions = worldMessagesInfiniteOptions(connection, {
    peerAddrs: hideSingleDefault ? [address, `${address}/default`] : [address],
    limit: 30,
  })
  const messagesQuery = useInfiniteQuery({
    ...messagesOptions,
    enabled: !peers.isPending,
  })
  const { messages, followUp } = useMemo(() => {
    const messagesByID = new Map<string, WorldMessage>()
    for (const page of messagesQuery.data?.pages ?? []) {
      for (const message of page.items) {
        if (!messagesByID.has(message.message_id))
          messagesByID.set(message.message_id, message)
      }
    }
    const history = Array.from(messagesByID.values())
    return {
      followUp: selectFollowUpThread(history),
      messages: history
        .filter((message) => message.kind === "message")
        .reverse(),
    }
  }, [messagesQuery.data])
  const statusHint =
    peer && !peer.online ? t("world.offlineDelivery") : undefined

  const ready = messagesQuery.data !== undefined
  const [lastSent, setLastSent] = useState<{ address: string; id: string }>()
  const earlierError = messagesQuery.isFetchNextPageError

  async function onSent(sentMessage: WorldMessage) {
    const { queryKey } = messagesOptions
    await queryClient.cancelQueries({ queryKey, exact: true })
    queryClient.setQueryData(queryKey, (data) =>
      mergeSentMessagePage(data, sentMessage)
    )
    setLastSent({ address, id: sentMessage.message_id })
    invalidateWorldQueries(queryClient, connection, "message_updated")
  }

  return (
    <>
      {(isMobile || recentPeer) && (
        <div
          className={cn(
            "flex min-h-10 min-w-0 shrink-0 items-center gap-2 px-3 text-ui",
            !statusHint && "border-b"
          )}
        >
          {peer ? (
            <WorldIconWithStatus
              icon={<ContactIcon className="size-4" />}
              online={peer.online}
              label={t(peer.online ? "world.online" : "world.offline")}
            />
          ) : (
            <ContactIcon className="size-4 shrink-0" />
          )}
          <OverflowTooltip className="max-w-1/2 min-w-0 truncate text-ui">
            {recentPeer ? recentPeer.node + " · " + recentPeer.domain : name}
          </OverflowTooltip>
          {!recentPeer && name !== address && (
            <OverflowTooltip className="min-w-0 truncate text-ui color-subtle">
              {address}
            </OverflowTooltip>
          )}
        </div>
      )}
      {statusHint && (
        <p className="flex min-h-10 shrink-0 items-center border-b px-3 py-2 text-ui color-subtle md:px-6">
          {statusHint}
        </p>
      )}
      <VirtualMessageList
        alignBottom
        rows={messages}
        getRowKey={getWorldMessageKey}
        renderRow={(message, index) => (
          <div className="flex min-w-0 flex-col gap-2 pb-2">
            {shouldShowTimeDivider(
              messages[index - 1]?.created_at,
              message.created_at
            ) && (
              <div className="mb-4 text-center text-caption color-muted">
                <WorldMessageTime ms={message.created_at} />
              </div>
            )}
            <WorldMessageThreadItem message={message} allowCancel />
          </div>
        )}
        ariaLabel={t("world.messages")}
        header={
          <>
            {(peers.isPending || messagesQuery.isPending) && (
              <LatticeLoader aria-label={t("common.loading")} />
            )}
            {peers.isError && (
              <RequestError
                error={peers.error}
                onRetry={() => void peers.refetch()}
              />
            )}
            {messagesQuery.isError && !earlierError && (
              <RequestError
                error={messagesQuery.error}
                onRetry={() => void messagesQuery.refetch()}
              />
            )}
            {ready && messages.length === 0 && !messagesQuery.hasNextPage && (
              <ListEmpty title={t("world.emptyHistory")} />
            )}
          </>
        }
        earlier={{
          hasMore: messagesQuery.hasNextPage,
          canLoad: !messagesQuery.isFetching && !earlierError,
          load: () =>
            void messagesQuery.fetchNextPage({ cancelRefetch: false }),
          content: (retry) =>
            earlierError ? (
              <RequestError error={messagesQuery.error} onRetry={retry} />
            ) : (
              <LatticeLoader
                aria-label={t("common.loading")}
                className={
                  messagesQuery.isFetchingNextPage ? "color-muted" : "invisible"
                }
              />
            ),
        }}
        resetKey={`${address}:${hideSingleDefault}`}
        scrollToBottomKey={
          lastSent?.address === address ? lastSent.id : undefined
        }
        footer={
          <ContactComposer
            key={address}
            address={address}
            threadID={recentThread ?? followUp?.thread_id}
            disabled={!ready}
            onSent={onSent}
          />
        }
      />
    </>
  )
}

function getWorldMessageKey(message: WorldMessage) {
  return message.message_id
}

function ContactComposer({
  address,
  threadID,
  disabled,
  onSent,
}: {
  address: string
  threadID?: string
  disabled: boolean
  onSent(message: WorldMessage): Promise<void>
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const [message, setMessage] = useState("")
  const [startNewThread, setStartNewThread] = useState(false)
  const notifyIfUnmounted = useNotifyIfUnmounted(t)
  const send = useMutation({
    mutationFn: (input: MethodParams["world/sendMessage"]) =>
      connection.call("world/sendMessage", input),
    onSuccess: onSent,
    onError: notifyIfUnmounted,
  })
  const canSend = !disabled && !send.isPending && !!message.trim()

  useLayoutEffect(() => {
    const textarea = textareaRef.current
    if (!textarea) return
    textarea.style.height = "auto"
    textarea.style.height = textarea.scrollHeight + "px"
  }, [message])

  return (
    <form
      className="flex min-w-0 flex-col gap-2 md:pb-1"
      onSubmit={(event) => {
        event.preventDefault()
        if (!canSend) return
        send.mutate(
          {
            to: address,
            message: message.trim(),
            thread_id: startNewThread ? undefined : threadID,
          },
          {
            onSuccess: () => {
              setMessage("")
              setStartNewThread(false)
            },
          }
        )
      }}
    >
      {send.isError && <WorldSendError error={send.error} />}
      <InputGroup className="has-disabled:bg-transparent has-disabled:opacity-100 dark:has-disabled:bg-input/30">
        <InputGroupTextarea
          ref={textareaRef}
          rows={1}
          aria-label={t("world.message")}
          placeholder={t("world.message")}
          className="max-h-[40svh] min-h-10 overflow-y-auto px-3 text-body"
          value={message}
          onChange={(event) => setMessage(event.target.value)}
          disabled={disabled}
          readOnly={send.isPending}
          onKeyDown={(event) => {
            const { isComposing, keyCode } = event.nativeEvent
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
              event.currentTarget.form?.requestSubmit()
            }
          }}
        />
        <InputGroupAddon
          align="block-end"
          className="gap-1 px-2 text-foreground"
        >
          <div className="flex min-w-0 flex-1 items-center gap-1">
            {threadID && (
              <Tooltip>
                <TooltipTrigger
                  delay={600}
                  closeOnClick
                  render={
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      aria-label={t("world.newTopic")}
                      aria-pressed={startNewThread}
                      className="h-8 w-8 color-muted transition-colors aria-pressed:bg-foreground aria-pressed:text-background aria-pressed:hover:bg-foreground/90 aria-pressed:hover:text-background"
                      disabled={disabled || send.isPending}
                      onClick={() => setStartNewThread((enabled) => !enabled)}
                    />
                  }
                >
                  <MorphIcon
                    icon={startNewThread ? MessageSquarePlus : MessagesSquare}
                    size={16}
                    aria-hidden
                  />
                </TooltipTrigger>
                <TooltipContent className="text-caption">
                  {startNewThread
                    ? t("world.returnToCurrentTopic")
                    : t("world.startNewTopic")}
                </TooltipContent>
              </Tooltip>
            )}
          </div>
          <div className="flex shrink-0 items-center gap-1">
            <Button
              type="submit"
              size="icon"
              className="h-8 w-8"
              disabled={!canSend}
              aria-label={t("composer.send")}
            >
              {send.isPending ? (
                <LatticeLoader aria-label={t("common.loading")} />
              ) : (
                <ArrowUpIcon className="size-4" />
              )}
            </Button>
          </div>
        </InputGroupAddon>
      </InputGroup>
    </form>
  )
}

function WorldConversationLink({
  conversationID,
  label,
}: {
  conversationID: ConversationID
  label: string
}) {
  const connection = useServerConnection()
  const { search } = useLocation()
  return (
    <Tooltip>
      <TooltipTrigger
        delay={600}
        closeOnClick
        render={
          <NavigationButton
            className={cn(
              buttonVariants({ variant: "ghost", size: "icon" }),
              "color-muted"
            )}
            aria-label={label}
            to={{
              pathname: getConversationPath(
                connection.getServerID(),
                conversationID
              ),
              search: getConversationListSearch(search),
            }}
          />
        }
      >
        <ArrowUpRightIcon aria-hidden />
      </TooltipTrigger>
      <TooltipContent className="text-caption">{label}</TooltipContent>
    </Tooltip>
  )
}

function WorldMessageThreadItem({
  message,
  allowCancel = true,
  showConversationLink = true,
}: {
  message: WorldMessage
  allowCancel?: boolean
  showConversationLink?: boolean
}) {
  const { t } = useTranslation()
  const outbound = message.direction === "outbound"
  return (
    <>
      <WorldMessageBubble
        side={outbound ? "self" : "peer"}
        viewStateKey={`${message.message_id}:body`}
        subject={message.subject}
        body={message.body}
        status={message.status}
        error={message.error_text}
        errorCode={message.error_code}
        cancelMessage={outbound && allowCancel ? message : undefined}
        conversationLink={
          showConversationLink && message.conversation_id ? (
            <WorldConversationLink
              conversationID={message.conversation_id}
              label={t(
                outbound ? "world.fromConversation" : "world.openConversation"
              )}
            />
          ) : undefined
        }
      />
      {message.reply_body && (
        <WorldMessageBubble
          side={outbound ? "peer" : "self"}
          body={message.reply_body}
          viewStateKey={`${message.message_id}:reply`}
        />
      )}
    </>
  )
}

function WorldLogHeader({
  directionLabel,
  address,
  target,
  time,
  conversationID,
  conversationLabel,
}: {
  directionLabel?: string
  address: string
  target?: string
  time?: number
  conversationID?: ConversationID
  conversationLabel: string
}) {
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 text-ui">
      {directionLabel && <span className="color-subtle">{directionLabel}</span>}
      <div className="flex min-w-0 items-center gap-2">
        <ServerIcon className="size-4 shrink-0" />
        <OverflowTooltip className="min-w-0 truncate">
          {address}
        </OverflowTooltip>
      </div>
      {target && (
        <span className="inline-flex min-w-0 items-center gap-2 color-subtle">
          {target !== "default" && <BotIcon className="size-4 shrink-0" />}
          <OverflowTooltip className="min-w-0 truncate">
            {target}
          </OverflowTooltip>
        </span>
      )}
      {time !== undefined && <WorldMessageTime ms={time} />}
      {conversationID && (
        <WorldConversationLink
          conversationID={conversationID}
          label={conversationLabel}
        />
      )}
    </div>
  )
}

const worldLogClassName =
  "mx-auto w-full max-w-3xl px-3 py-4 md:px-6 [&>div>ul]:gap-4"
const worldLogCardClassName = "flex min-w-0 flex-col gap-2 border p-4"

const messageDirections = [
  [undefined, "world.allMessages"],
  ["inbound", "world.inbound"],
  ["outbound", "world.outbound"],
] as const

function MessageLogList() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const [direction, setDirection] = useState<WorldMessageDirection>()
  const messages = useInfiniteQuery(
    worldMessagesInfiniteOptions(connection, {
      kind: "message",
      direction,
      limit: 30,
    })
  )
  const threads = useQuery({
    ...worldThreadsOptions(connection),
    enabled:
      messages.data?.pages.some((page) =>
        page.items.some(
          (message) => message.direction === "inbound" && !message.target
        )
      ) ?? false,
  })
  const threadTargets = new Map(
    threads.data?.map((binding) => [binding.thread_id, binding.target])
  )

  return (
    <div className={worldLogClassName}>
      <div className="mb-4 flex flex-wrap items-center gap-2">
        {messageDirections.map(([value, label]) => (
          <Button
            key={label}
            type="button"
            variant={direction === value ? "secondary" : "ghost"}
            aria-pressed={direction === value}
            className="text-ui"
            onClick={() => setDirection(value)}
          >
            {t(label)}
          </Button>
        ))}
      </div>
      <InfiniteList
        query={messages}
        getItems={(page) => page.items}
        direction="down"
        emptyState={<ListEmpty title={t("world.emptyMessages")} />}
        renderItem={(message) => {
          const outbound = message.direction === "outbound"
          const target = outbound
            ? parseWorldAddress(message.peer_addr)?.target
            : message.target || threadTargets.get(message.thread_id)
          return (
            <li key={message.message_id} className={worldLogCardClassName}>
              <WorldLogHeader
                directionLabel={t(
                  outbound ? "world.outbound" : "world.inbound"
                )}
                address={message.peer_addr}
                target={target}
                time={message.created_at}
                conversationID={message.conversation_id}
                conversationLabel={t(
                  outbound ? "world.fromConversation" : "world.openConversation"
                )}
              />
              <WorldMessageThreadItem
                message={message}
                showConversationLink={false}
              />
            </li>
          )
        }}
      />
    </div>
  )
}

function ThreadList() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const threads = useInfiniteQuery(
    worldThreadsInfiniteOptions(connection, { limit: 30 })
  )
  return (
    <div className={worldLogClassName}>
      <InfiniteList
        query={threads}
        getItems={(page) => page.items}
        direction="down"
        emptyState={<ListEmpty title={t("world.emptyThreads")} />}
        renderItem={(binding) => (
          <li key={binding.thread_id} className={worldLogCardClassName}>
            <WorldLogHeader
              address={binding.peer_addr}
              target={binding.target}
              conversationID={binding.conversation_id}
              conversationLabel={t("world.openConversation")}
            />
            <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 text-caption color-muted">
              <span>
                {t(
                  binding.direction === "inbound"
                    ? "world.inbound"
                    : "world.outbound"
                )}
              </span>
              <span>
                {t("world.remainingHops", { count: binding.remaining_hops })}
              </span>
              <OverflowTooltip className="min-w-0 truncate">
                {binding.thread_id}
              </OverflowTooltip>
            </div>
          </li>
        )}
      />
    </div>
  )
}
