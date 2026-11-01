import { AlertCircleIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { getRpcErrorDetail, getRpcErrorMessage } from "@/lib/rpc-error"

// 请求失败的提示：标题翻译，serve 返回的原文放进可展开的详情；传 onRetry 时带「重试」
export function RequestError({
  error,
  onRetry,
}: {
  error: unknown
  onRetry?: () => void
}) {
  const { t } = useTranslation()
  const message = getRpcErrorMessage(error, t)
  const detail = getRpcErrorDetail(error)
  return (
    <Alert variant="destructive">
      <AlertCircleIcon />
      <AlertTitle className="text-section">
        {t("common.errors.title")}
      </AlertTitle>
      <AlertDescription className="flex flex-col gap-2 text-caption">
        <p className="text-caption wrap-anywhere">{message}</p>
        {detail && (
          <details>
            <summary className="cursor-pointer text-ui">
              {t("common.errors.detail")}
            </summary>
            <p className="text-caption wrap-anywhere color-muted">{detail}</p>
          </details>
        )}
        {onRetry && (
          <Button
            variant="outline"
            className="self-start text-ui"
            onClick={onRetry}
          >
            {t("common.actions.retry")}
          </Button>
        )}
      </AlertDescription>
    </Alert>
  )
}
