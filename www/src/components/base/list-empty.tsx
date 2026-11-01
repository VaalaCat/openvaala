import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty"

// 列表没有数据行时的统一空状态。
// filtered：用户设置了筛选条件（关键字、下拉筛选）；页面固定的过滤不算。可以筛选的列表要传 noMatchTitle 和 onClearFilters。
export function ListEmpty({
  filtered = false,
  title,
  description,
  noMatchTitle,
  onClearFilters,
}: {
  filtered?: boolean
  title: string
  description?: ReactNode
  noMatchTitle?: string
  onClearFilters?(): void
}) {
  const { t } = useTranslation()
  return (
    <Empty data-empty-state={filtered ? "no-match" : "empty"}>
      <EmptyHeader>
        <EmptyTitle className="text-section">
          {filtered ? noMatchTitle : title}
        </EmptyTitle>
        {!filtered && description && (
          <EmptyDescription className="text-caption">
            {description}
          </EmptyDescription>
        )}
      </EmptyHeader>
      {filtered && (
        <EmptyContent className="text-body">
          <Button
            className="text-ui"
            variant="outline"
            onClick={onClearFilters}
          >
            {t("common.actions.clearFilters")}
          </Button>
        </EmptyContent>
      )}
    </Empty>
  )
}
