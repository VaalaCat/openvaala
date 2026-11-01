import { useRef, type ReactNode } from "react"
import { useTranslation } from "react-i18next"
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { LatticeLoader } from "@/components/base/lattice-loader"

// 确认弹窗：标题写操作和资源，description 写后果，preview 放提交失败的原因；提交中不能关闭
export function ConfigurationChangeDialog({
  open,
  title,
  description,
  preview,
  confirmLabel,
  pending,
  confirmDisabled = false,
  destructive = false,
  onConfirm,
  onOpenChange,
}: {
  open: boolean
  title: ReactNode
  description?: ReactNode
  preview?: ReactNode
  confirmLabel: string
  destructive?: boolean
  pending: boolean
  confirmDisabled?: boolean
  onConfirm(): void
  onOpenChange(open: boolean): void
}) {
  const { t } = useTranslation()
  const cancel = useRef<HTMLButtonElement>(null)
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (!pending) onOpenChange(next)
      }}
    >
      <AlertDialogContent
        className="max-h-[calc(100dvh-2rem)] w-[calc(100vw-2rem)] overflow-y-auto text-body data-[size=default]:max-w-2xl data-[size=default]:sm:max-w-2xl"
        initialFocus={cancel}
      >
        <AlertDialogHeader>
          <AlertDialogTitle className="text-section wrap-anywhere">
            {title}
          </AlertDialogTitle>
          {description && (
            <AlertDialogDescription className="text-caption wrap-anywhere">
              {description}
            </AlertDialogDescription>
          )}
        </AlertDialogHeader>
        {preview && (
          <div className="flex max-h-[60vh] min-w-0 flex-col gap-3 overflow-auto">
            {preview}
          </div>
        )}
        <AlertDialogFooter className="flex-row justify-end">
          <AlertDialogCancel
            className="text-ui"
            ref={cancel}
            disabled={pending}
          >
            {t("common.actions.cancel")}
          </AlertDialogCancel>
          <Button
            className="text-ui"
            variant={destructive ? "destructive" : "default"}
            disabled={pending || confirmDisabled}
            onClick={onConfirm}
          >
            {pending && (
              <LatticeLoader
                data-icon="inline-start"
                aria-label={t("common.loading")}
              />
            )}
            {confirmLabel}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
