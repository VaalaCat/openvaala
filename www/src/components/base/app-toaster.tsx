import { useStore } from "@nanostores/react"
import {
  CircleCheckIcon,
  InfoIcon,
  OctagonXIcon,
  TriangleAlertIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { Toaster } from "@/components/ui/sonner"
import { $appearance } from "@/lib/appearance/preferences"

export function AppToaster() {
  const { t } = useTranslation()
  const { mode } = useStore($appearance)
  return (
    <Toaster
      theme={mode}
      position="bottom-right"
      visibleToasts={3}
      icons={{
        success: <CircleCheckIcon className="size-4" />,
        info: <InfoIcon className="size-4" />,
        warning: <TriangleAlertIcon className="size-4" />,
        error: <OctagonXIcon className="size-4" />,
        loading: <LatticeLoader aria-label={t("common.loading")} />,
      }}
      containerAriaLabel={t("common.toast.region")}
      toastOptions={{
        closeButtonAriaLabel: t("common.toast.close"),
        classNames: {
          toast: "cn-toast",
          title: "text-body",
          description: "text-caption",
        },
      }}
    />
  )
}
