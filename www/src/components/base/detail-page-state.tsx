import type { ReactNode } from "react"
import { Link } from "react-router"
import { useTranslation } from "react-i18next"
import { isNotFoundError } from "@/lib/rpc-error"
import { PageLayout, type PageNavigation } from "@/components/base/page-layout"
import { RequestError } from "@/components/base/request-error"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

// 详情页在数据到达前的统一状态，按顺序判断：缺 ID、已有数据、资源不存在、其他错误、加载中。
// 已有数据时即使后台刷新失败也继续渲染数据，草稿不丢；刷新失败由页面自己在顶部显示。
// 资源从列表里找、列表到了却没有它时，调用方传 notFound。各状态的面包屑都用 parents。
export function DetailPageState<T>({
  id,
  title,
  entity,
  parents,
  notFoundReturn,
  missingIdText,
  query,
  notFound = false,
  shouldShowError,
  embedded = false,
  children,
}: {
  id: string
  title: string
  entity: string
  parents: PageNavigation["parents"]
  // 资源不存在时按钮回到的页面和按钮文字
  notFoundReturn: { to: string; label: string }
  missingIdText: string
  query: { data?: T; error: unknown; refetch(): void }
  notFound?: boolean
  shouldShowError?: (error: unknown) => boolean
  embedded?: boolean
  children(data: T): ReactNode
}) {
  const { t } = useTranslation()
  if (id && query.data !== undefined) return children(query.data)
  const state = !id
    ? "missing-id"
    : notFound || isNotFoundError(query.error)
      ? "not-found"
      : query.error && (shouldShowError?.(query.error) ?? true)
        ? "error"
        : "loading"
  const content = (
    <div data-detail-state={state}>
      {state === "missing-id" ? (
        <p className="text-body">{missingIdText}</p>
      ) : state === "not-found" ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle className="text-section">
              {t("common.notFound.title", { entity })}
            </EmptyTitle>
            <EmptyDescription className="text-caption">
              {t("common.notFound.description")}
            </EmptyDescription>
          </EmptyHeader>
          <EmptyContent className="text-body">
            <Button
              className="text-ui"
              nativeButton={false}
              variant="outline"
              render={<Link to={notFoundReturn.to} />}
            >
              {notFoundReturn.label}
            </Button>
          </EmptyContent>
        </Empty>
      ) : state === "error" ? (
        <RequestError
          error={query.error}
          onRetry={() => void query.refetch()}
        />
      ) : (
        <Skeleton className="h-28 w-full" />
      )}
    </div>
  )
  if (embedded) return content
  return (
    <PageLayout title={title} parents={parents}>
      {content}
    </PageLayout>
  )
}
