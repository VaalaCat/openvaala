import { useRouteError } from "react-router"
import { useTranslation } from "react-i18next"
import { RequestError } from "@/components/base/request-error"
import { StandalonePage } from "@/components/base/standalone-page"
import { Button } from "@/components/ui/button"
import { isModuleLoadError } from "@/lib/route-error"

// 页面加载或渲染失败，由根路由的 errorElement 显示。
// 只有页面模块没加载出来才提示「页面可能刚升级过」，其他错误用中性标题。
export function RouteLoadError() {
  const { t } = useTranslation()
  const error = useRouteError()
  const title = t(
    isModuleLoadError(error)
      ? "common.errors.pageLoadFailed"
      : "common.errors.pageFailed"
  )
  return (
    <StandalonePage title={title}>
      <div className="flex flex-col gap-4">
        <RequestError error={error} />
        <Button
          variant="outline"
          className="self-start text-ui"
          onClick={() => location.reload()}
        >
          {t("common.actions.reload")}
        </Button>
      </div>
    </StandalonePage>
  )
}
