import { shouldShowLoadError } from "@/lib/rpc-error"
import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { ListEmpty } from "@/components/base/list-empty"
import { QueryState } from "@/components/base/query-state"
import { RowList, RowListItem } from "@/components/base/row-list"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { useServerConnection } from "@/components/server-connection-provider"
import { executorListOptions } from "@/lib/query/executor"

// Executor 列表（只读）：ID、说明和默认根目录，按配置顺序
export function ExecutorSettingsPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const executors = useQuery(executorListOptions(connection))
  return (
    <SettingsLayout section="executors">
      <QueryState shouldShowError={shouldShowLoadError} query={executors}>
        {({ executors }) =>
          executors.length === 0 ? (
            <ListEmpty title={t("settings.executors.empty")} />
          ) : (
            <RowList>
              {executors.map((executor) => (
                <RowListItem key={executor.id}>
                  <div className="flex min-w-0 items-center gap-2">
                    <OverflowTooltip
                      render={<span className="min-w-0 truncate" />}
                    >
                      {executor.id}
                    </OverflowTooltip>
                    {executor.description && (
                      <OverflowTooltip
                        render={
                          <span className="ml-auto max-w-1/2 min-w-0 truncate text-caption color-muted" />
                        }
                      >
                        {executor.description}
                      </OverflowTooltip>
                    )}
                  </div>
                  <OverflowTooltip
                    render={<p className="truncate text-table color-muted" />}
                  >
                    {executor.workspace_directory
                      ? t("settings.executors.workspaceDirectory", {
                          directory: executor.workspace_directory,
                        })
                      : t("settings.executors.noWorkspaceDirectory")}
                  </OverflowTooltip>
                </RowListItem>
              ))}
            </RowList>
          )
        }
      </QueryState>
    </SettingsLayout>
  )
}
