import { keepPreviousData, useInfiniteQuery } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { useSearchParams } from "react-router"
import { InfiniteList } from "@/components/base/infinite-list"
import { ListEmpty } from "@/components/base/list-empty"
import { ListSearch } from "@/components/base/list-search"
import { ConversationListItem } from "@/components/conversation-list/conversation-list-item"
import { useServerConnection } from "@/components/server-connection-provider"
import { SidebarGroup } from "@/components/ui/sidebar"
import type { Order } from "@/lib/api/protocol.gen"
import { conversationListOptions } from "@/lib/query/conversation"
import { shouldShowLoadError } from "@/lib/rpc-error"

// 最近更新的在前，和 TUI 的恢复对话列表一致
const conversationListOrder: Order = { by: "updated_at", direction: "desc" }

// 左栏按搜索词显示未归档对话，滚到底时加载下一页。
export function ConversationList() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const connected = useStore(connection.$state) === "connected"
  const [searchParams, setSearchParams] = useSearchParams()
  const keyword = searchParams.get("q") ?? ""
  const searchTerm = keyword.trim()
  // 换搜索词时保留上一份列表，收到新结果后替换。
  const query = useInfiniteQuery({
    ...conversationListOptions(connection, {
      search_term: searchTerm || undefined,
      archived: false,
      order: conversationListOrder,
    }),
    placeholderData: keepPreviousData,
  })
  function setParam(name: string, value: string | undefined) {
    setSearchParams(
      (previous) => {
        const next = new URLSearchParams(previous)
        if (value) next.set(name, value)
        else next.delete(name)
        return next
      },
      { replace: true }
    )
  }
  return (
    <div className="flex min-h-0 flex-1 flex-col group-data-[collapsible=icon]:hidden">
      <SidebarGroup className="shrink-0 pt-0">
        <form
          role="search"
          onSubmit={(event) => {
            event.preventDefault()
            setParam(
              "q",
              String(new FormData(event.currentTarget).get("keyword"))
            )
          }}
        >
          <ListSearch
            keyword={keyword}
            aria-label={t("conversationList.search")}
            placeholder={t("conversationList.search")}
            autoComplete="off"
            onKeywordChange={(value) => setParam("q", value)}
            onClear={() => setParam("q", undefined)}
          />
        </form>
      </SidebarGroup>
      <SidebarGroup className="min-h-0 flex-1 overflow-y-auto pt-0">
        <InfiniteList
          query={query}
          shouldShowError={shouldShowLoadError}
          paused={!connected}
          direction="down"
          getItems={(page) => page.conversations}
          renderItem={(entry) => (
            <ConversationListItem
              key={entry.conversation.id}
              conversation={entry.conversation}
            />
          )}
          emptyState={
            <ListEmpty
              filtered={searchTerm !== ""}
              title={t("conversationList.empty.title")}
              noMatchTitle={t("conversationList.empty.noMatchTitle")}
              onClearFilters={() => setParam("q", undefined)}
            />
          }
        />
      </SidebarGroup>
    </div>
  )
}
