import { useState } from "react"
import { ExternalLinkIcon, ServerIcon } from "lucide-react"
import { useLocation, useNavigate, useSearchParams } from "react-router"
import { useTranslation } from "react-i18next"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { ScrollableTabsList } from "@/components/base/scrollable-tabs-list"
import { RequestError } from "@/components/base/request-error"
import { useServerConnection } from "@/components/server-connection-provider"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { Button } from "@/components/ui/button"
import { Tabs, TabsTrigger, TabsContent } from "@/components/ui/tabs"
import { ExternalWorldAdmin } from "@/components/world/world-admin-access"
import { WorldAdminConsole } from "@/components/world/world-admin-console"
import {
  cohostedWorldAdmin,
  normalizeWorldControlURL,
} from "@/lib/api/world-admin"
import { useWorldStatusState } from "@/lib/query/world"
import { getConversationListSearch } from "@/lib/paths"
import { WorldLocalSettings } from "./world-local"

export function WorldAdminSettingsPage() {
  const { t } = useTranslation()
  const [search, setSearch] = useSearchParams()
  const [restarting, setRestarting] = useState(false)
  const tab = !restarting && search.get("tab") === "admin" ? "admin" : "local"
  return (
    <SettingsLayout section="world-admin">
      <Tabs
        value={tab}
        onValueChange={(v) =>
          setSearch((previous) => {
            const next = new URLSearchParams(previous)
            if (v === "admin") next.set("tab", "admin")
            else next.delete("tab")
            return next
          })
        }
      >
        {!restarting && (
          <ScrollableTabsList>
            <TabsTrigger className="text-ui" value="local">
              {t("worldSettings.local")}
            </TabsTrigger>
            <TabsTrigger className="text-ui" value="admin">
              {t("worldSettings.admin")}
            </TabsTrigger>
          </ScrollableTabsList>
        )}
        <TabsContent value="local" keepMounted>
          <WorldLocalSettings
            restarting={restarting}
            localTabActive={tab === "local"}
            setRestarting={setRestarting}
          />
        </TabsContent>
        <TabsContent value="admin">
          <WorldAdmin />
        </TabsContent>
      </Tabs>
    </SettingsLayout>
  )
}
function WorldAdmin() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { search } = useLocation()
  const server = useServerConnection()
  const world = useWorldStatusState()
  const [external, setExternal] = useState(false)
  const connection = cohostedWorldAdmin(server)
  const hasLocal = world?.status?.admin_enabled === true
  const isLocal = hasLocal && !external
  const statusURL = world?.status?.enabled
    ? normalizeWorldControlURL(world.status.server_url)
    : undefined
  return (
    <>
      {world?.isPending ? (
        <LatticeLoader aria-label={t("common.loading")} />
      ) : (
        <>
          {world?.isError && !world.status && (
            <RequestError
              error={world.error}
              onRetry={() => void world.refetch()}
            />
          )}
          {hasLocal && (
            <div className="flex flex-wrap items-center gap-2">
              <Button
                variant={isLocal ? "secondary" : "ghost"}
                aria-pressed={isLocal}
                className="text-ui"
                onClick={() => setExternal(false)}
              >
                <ServerIcon data-icon="inline-start" />
                {t("worldAdmin.localServer")}
              </Button>
              <Button
                variant={external ? "secondary" : "ghost"}
                aria-pressed={external}
                className="text-ui"
                onClick={() => setExternal(true)}
              >
                <ExternalLinkIcon data-icon="inline-start" />
                {t("worldAdmin.externalServer")}
              </Button>
            </div>
          )}
          <div className="mt-4 min-w-0">
            {isLocal ? (
              <WorldAdminConsole
                key={connection.scope}
                connection={connection}
                onOpenRelaySettings={() =>
                  void navigate({
                    search: getConversationListSearch(search),
                    hash: "#world-relays",
                  })
                }
              />
            ) : (
              <ExternalWorldAdmin
                defaultURL={statusURL ?? connection.controlURL}
              />
            )}
          </div>
        </>
      )}
    </>
  )
}
