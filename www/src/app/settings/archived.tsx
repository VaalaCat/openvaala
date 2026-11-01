import { shouldShowLoadError } from "@/lib/rpc-error"
import { useStore } from "@nanostores/react"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { keepPreviousData, useInfiniteQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { Link, useLocation, useSearchParams } from "react-router"
import { InfiniteList } from "@/components/base/infinite-list"
import { ListEmpty } from "@/components/base/list-empty"
import { ListSearch } from "@/components/base/list-search"
import { ConversationActions } from "@/components/conversation-list/conversation-actions"
import { useServerConnection } from "@/components/server-connection-provider"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { Button } from "@/components/ui/button"
import { getConversationListSearch, getConversationPath } from "@/lib/paths"
import { conversationListOptions } from "@/lib/query/conversation"
import { formatRelativeTime } from "@/lib/relative-time"

// 归档页独立搜索已归档的对话，滚到底时按游标继续加载。
export function ArchivedConversationsPage() {
  // 1. 读取归档搜索词，查询已归档对话。
  const { t, i18n } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const { search } = useLocation()
  const [searchParams, setSearchParams] = useSearchParams()
  const keyword = searchParams.get("archived_q") ?? ""
  const searchTerm = keyword.trim()
  const query = useInfiniteQuery({
    ...conversationListOptions(connection, {
      archived: true,
      search_term: searchTerm || undefined,
      order: { by: "updated_at", direction: "desc" },
    }),
    placeholderData: keepPreviousData,
  })
  function setKeyword(value: string) {
    setSearchParams(
      (previous) => {
        const next = new URLSearchParams(previous)
        if (value) next.set("archived_q", value)
        else next.delete("archived_q")
        return next
      },
      { replace: true }
    )
  }

  // 2. 显示搜索、分页列表和共用的对话操作。
  return (
    <SettingsLayout section="archived">
      <form
        role="search"
        className="max-w-lg"
        onSubmit={(event) => {
          event.preventDefault()
          setKeyword(String(new FormData(event.currentTarget).get("keyword")))
        }}
      >
        <ListSearch
          keyword={keyword}
          aria-label={t("settings.archived.search")}
          placeholder={t("settings.archived.search")}
          autoComplete="off"
          onKeywordChange={setKeyword}
          onClear={() => setKeyword("")}
        />
      </form>
      <div role="region" aria-label={t("settings.sections.archived")}>
        <InfiniteList
          shouldShowError={shouldShowLoadError}
          paused={!isConnected}
          query={query}
          direction="down"
          getItems={(page) => page.conversations}
          renderItem={({ conversation }) => {
            const name = conversation.title || t("conversationList.untitled")
            return (
              <li
                key={conversation.id}
                className="flex min-w-0 items-center gap-2 border-b p-2"
              >
                <div className="flex min-w-0 flex-1 flex-col gap-1">
                  <OverflowTooltip
                    render={
                      <Link
                        to={{
                          pathname: getConversationPath(
                            connection.getServerID(),
                            conversation.id
                          ),
                          search: getConversationListSearch(search),
                        }}
                        className="truncate text-table text-link"
                      />
                    }
                  >
                    {name}
                  </OverflowTooltip>
                  {conversation.updated_at !== undefined && (
                    <Tooltip>
                      <TooltipTrigger
                        render={
                          <time
                            dateTime={new Date(
                              conversation.updated_at
                            ).toISOString()}
                            className="text-caption color-muted"
                          />
                        }
                      >
                        {formatRelativeTime(
                          conversation.updated_at,
                          i18n.language
                        )}
                      </TooltipTrigger>
                      <TooltipContent className="text-caption">
                        {new Date(conversation.updated_at).toLocaleString(
                          i18n.language
                        )}
                      </TooltipContent>
                    </Tooltip>
                  )}
                </div>
                <ConversationActions
                  conversation={conversation}
                  name={name}
                  isOpened={false}
                  trigger={
                    <Button variant="ghost" size="icon" className="text-ui" />
                  }
                />
              </li>
            )
          }}
          emptyState={
            <ListEmpty
              filtered={searchTerm !== ""}
              title={t("conversationList.empty.archivedTitle")}
              noMatchTitle={t("conversationList.empty.noMatchTitle")}
              onClearFilters={() => setKeyword("")}
            />
          }
        />
      </div>
    </SettingsLayout>
  )
}
