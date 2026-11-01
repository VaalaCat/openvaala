import { useEffect, useRef, type ReactNode } from "react"
import type {
  InfiniteData,
  UseInfiniteQueryResult,
} from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { RequestError } from "@/components/base/request-error"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { Skeleton } from "@/components/ui/skeleton"

// 无限滚动列表，只管分页状态，不知道列表项是什么。
// direction "down"：哨兵在列表底部，页按服务端返回的顺序排列。
// direction "up"：哨兵在列表顶部，服务端的页是从新到旧，列表倒过来显示，最新的在最下面。
// renderItem 返回的元素要带 key，并且是 <li>。
export function InfiniteList<TPage, TItem>({
  query,
  getItems,
  renderItem,
  direction,
  emptyState,
  shouldShowError,
  paused = false,
}: {
  query: UseInfiniteQueryResult<InfiniteData<TPage>, Error>
  getItems(page: TPage): TItem[]
  renderItem(item: TItem): ReactNode
  direction: "down" | "up"
  emptyState: ReactNode
  shouldShowError?: (error: unknown) => boolean
  paused?: boolean
}) {
  const { t } = useTranslation()
  const sentinel = useRef<HTMLDivElement>(null)
  const { data, hasNextPage, isFetching, isError, isPending, fetchNextPage } =
    query
  // 出错时不自动翻页，免得失败后立刻又失败；重试由用户点
  const canFetchNextPage = hasNextPage && !isFetching && !isError && !paused
  const showError = isError && (shouldShowError?.(query.error) ?? true)
  // 每次拉取结束（isFetching 回到 false）都重新观察，哨兵仍然可见时继续翻页
  useEffect(() => {
    const element = sentinel.current
    if (!element || !canFetchNextPage) return
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) void fetchNextPage()
    })
    observer.observe(element)
    return () => observer.disconnect()
  }, [canFetchNextPage, fetchNextPage])
  if (isPending)
    return (
      <div className="flex items-center gap-2 p-2 text-caption color-muted">
        <LatticeLoader aria-hidden />
        {t("common.loading")}
      </div>
    )
  if (isError && !data && !showError)
    return <Skeleton className="h-28 w-full" />
  if (showError && !data)
    return (
      <RequestError error={query.error} onRetry={() => void query.refetch()} />
    )
  const items = (data?.pages ?? []).flatMap(getItems)
  if (direction === "up") items.reverse()
  const status = (
    <div ref={sentinel} className="flex flex-col gap-2">
      {showError && (
        <RequestError
          error={query.error}
          onRetry={() =>
            void (query.isFetchNextPageError
              ? query.fetchNextPage()
              : query.refetch())
          }
        />
      )}
      {!paused && !isError && hasNextPage && (
        <div className="flex items-center justify-center gap-2 p-2 text-caption color-muted">
          <LatticeLoader aria-hidden />
          {t("common.loading")}
        </div>
      )}
    </div>
  )
  return (
    <div className="flex min-w-0 flex-col">
      {direction === "up" && status}
      {items.length === 0 ? (
        emptyState
      ) : (
        <ul className="flex min-w-0 flex-col">{items.map(renderItem)}</ul>
      )}
      {direction === "down" && status}
    </div>
  )
}
