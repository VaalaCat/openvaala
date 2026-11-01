import { useBlocker, useBeforeUnload } from "react-router"
import { useTranslation } from "react-i18next"
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogFooter,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"

// 有未保存的修改时拦住离开：换页面先确认，刷新或关闭标签页由浏览器提示；只改查询参数（页签、筛选）不算离开
export function UnsavedChangesGuard({ dirty }: { dirty: boolean }) {
  const { t } = useTranslation()
  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) =>
      dirty && currentLocation.pathname !== nextLocation.pathname
  )
  useBeforeUnload((event) => {
    if (dirty) {
      event.preventDefault()
      event.returnValue = ""
    }
  })
  return (
    <AlertDialog
      open={blocker.state === "blocked"}
      onOpenChange={(open) => {
        if (!open && blocker.state === "blocked") blocker.reset()
      }}
    >
      <AlertDialogContent className="text-body">
        <AlertDialogHeader>
          <AlertDialogTitle className="text-section">
            {t("common.unsavedChanges.title")}
          </AlertDialogTitle>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <Button
            className="text-ui"
            variant="outline"
            onClick={() => blocker.state === "blocked" && blocker.reset()}
          >
            {t("common.unsavedChanges.stay")}
          </Button>
          <Button
            className="text-ui"
            onClick={() => blocker.state === "blocked" && blocker.proceed()}
          >
            {t("common.unsavedChanges.leave")}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
