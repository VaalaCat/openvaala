import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"
import { ConfigurationLayout } from "./configuration-layout"

// 设置页签的最后一节：只放按钮，后果写在按钮打开的确认弹窗里
export function DangerZone({
  actionLabel,
  disabled = false,
  disabledReason,
  onAction,
}: {
  actionLabel: string
  disabled?: boolean
  disabledReason?: string // 禁用时显示在按钮下方
  onAction(): void
}) {
  const { t } = useTranslation()
  return (
    <div data-danger-zone className="min-w-0">
      <ConfigurationLayout
        title={t("common.dangerZone")}
        actions={
          <Button
            variant="outline"
            className="text-ui color-destructive hover:enabled:color-destructive"
            disabled={disabled}
            onClick={onAction}
          >
            {actionLabel}
          </Button>
        }
      >
        {disabled && disabledReason && (
          <p role="status" className="text-caption color-warning">
            {disabledReason}
          </p>
        )}
      </ConfigurationLayout>
    </div>
  )
}
