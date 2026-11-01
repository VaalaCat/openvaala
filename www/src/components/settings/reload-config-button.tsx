import { useMutation } from "@tanstack/react-query"
import { useStore } from "@nanostores/react"
import { RefreshCwIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { notifyDefinitionsUpdated, notifyFailure } from "@/lib/notify"
import { reloadConfigOptions } from "@/lib/query/provider"

// 设置页页头的「重新加载配置」：serve 重新读整份配置；有对话正在执行、没换上新定义时，toast 里提示它们空闲后再生效
export function ReloadConfigButton() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const isConnected = useStore(connection.$state) === "connected"
  const reload = useMutation(reloadConfigOptions(connection))
  return (
    <Button
      className="text-ui"
      variant="outline"
      disabled={reload.isPending || !isConnected}
      onClick={() => {
        if (connection.$state.get() !== "connected") return
        reload.mutate(undefined, {
          onSuccess: (result) =>
            notifyDefinitionsUpdated(t("settings.reload.done"), result, t),
          onError: (error) => notifyFailure(error, t),
        })
      }}
    >
      {reload.isPending ? (
        <LatticeLoader
          data-icon="inline-start"
          aria-label={t("common.loading")}
        />
      ) : (
        <RefreshCwIcon data-icon="inline-start" />
      )}
      {t("settings.reload.action")}
    </Button>
  )
}
