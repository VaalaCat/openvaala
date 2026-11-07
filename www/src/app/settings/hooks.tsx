import { shouldShowLoadError } from "@/lib/rpc-error"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { ListEmpty } from "@/components/base/list-empty"
import { QueryState } from "@/components/base/query-state"
import { RowList, RowListItem } from "@/components/base/row-list"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { useServerConnection } from "@/components/server-connection-provider"
import { Badge } from "@/components/ui/badge"
import type { HookInfo } from "@/lib/api/protocol.gen"
import { hookListOptions } from "@/lib/query/hook"

// hook 列表（只读）：事件、matcher、命令、超时、所属插件和配置文件，按配置顺序
export function HookSettingsPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const hooks = useQuery(hookListOptions(connection))
  return (
    <SettingsLayout section="hooks">
      <QueryState shouldShowError={shouldShowLoadError} query={hooks}>
        {(data) => {
          const hookList = (data.hooks ?? []).filter(
            (each): each is HookInfo => !!each
          )
          if (hookList.length === 0)
            return (
              <ListEmpty
                title={t("settings.hooks.empty")}
                description={t("settings.hooks.emptyDescription")}
              />
            )
          return (
            <RowList>
              {hookList.map((hook) => (
                <RowListItem key={hook.key}>
                  <div className="flex min-w-0 items-center gap-2">
                    <Badge variant="outline" className="text-ui">
                      {hook.event}
                    </Badge>
                    {hook.matcher && (
                      <OverflowTooltip
                        render={<span className="min-w-0 truncate" />}
                      >
                        {hook.matcher}
                      </OverflowTooltip>
                    )}
                    {hook.plugin_name && (
                      <OverflowTooltip
                        render={
                          <span className="ml-auto max-w-1/2 min-w-0 truncate text-table color-muted" />
                        }
                      >
                        {t("settings.hooks.plugin", { name: hook.plugin_name })}
                      </OverflowTooltip>
                    )}
                  </div>
                  <pre className="overflow-x-auto text-code-reading">
                    {hook.handler}
                  </pre>
                  <div className="flex min-w-0 items-center gap-2 color-muted">
                    <OverflowTooltip
                      render={<span className="min-w-0 truncate text-table" />}
                    >
                      {hook.source_path}
                    </OverflowTooltip>
                    <span className="ml-auto shrink-0 text-caption">
                      {t("settings.hooks.timeout", {
                        timeout: hook.timeout_seconds,
                      })}
                    </span>
                  </div>
                </RowListItem>
              ))}
            </RowList>
          )
        }}
      </QueryState>
    </SettingsLayout>
  )
}
