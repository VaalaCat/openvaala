import type { ReactNode } from "react"
import { RequestError } from "@/components/base/request-error"
import { Skeleton } from "@/components/ui/skeleton"

// 展示查询数据和请求错误；刷新失败时保留已显示的内容，并提供重试。
export function QueryState<T>({
  query,
  shouldShowError,
  children,
}: {
  query: {
    data: T | undefined
    isError: boolean
    error: unknown
    refetch(): unknown
  }
  shouldShowError?: (error: unknown) => boolean
  children(data: T): ReactNode
}) {
  // 1. 错误和数据各占固定位置，后台重取不卸载已有内容。
  const showError = query.isError && (shouldShowError?.(query.error) ?? true)
  return (
    <>
      {showError && (
        <RequestError
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      )}
      {query.data !== undefined
        ? children(query.data)
        : !showError && <Skeleton className="h-28 w-full" />}
    </>
  )
}
