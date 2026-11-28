import { useTranslation } from "react-i18next"
import { getServerDisplay, type ServerEntry } from "@/lib/api/servers"

// 服务器列表首行是名字，次行是可区分这台服务器的地址。
export function ServerListLabel({
  server,
  disconnected = false,
}: {
  disconnected?: boolean
  server: Pick<ServerEntry, "url" | "name" | "nodeAddr">
}) {
  const { t } = useTranslation()
  const { name, address } = getServerDisplay(server)
  return (
    <span className="flex min-w-0 flex-col text-left">
      <span className="truncate text-ui">{name}</span>
      {(address !== name || disconnected) && (
        <span className="flex min-w-0 gap-1 text-caption color-muted">
          <span className="truncate">{address}</span>
          {disconnected && (
            <span className="shrink-0">
              · {t("connection.state.disconnected")}
            </span>
          )}
        </span>
      )}
    </span>
  )
}
