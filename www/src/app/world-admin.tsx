import { useLayoutEffect, useState } from "react"
import { ServerIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { ExternalWorldAdmin } from "@/components/world/world-admin-access"

export function WorldAdminPage() {
  const { t } = useTranslation()
  const [initialToken] = useState(() =>
    new URLSearchParams(location.hash.slice(1)).get("token")
  )
  useLayoutEffect(() => {
    if (initialToken !== null) {
      history.replaceState(
        history.state,
        "",
        location.pathname + location.search
      )
    }
  }, [initialToken])
  return (
    <main className="flex min-h-svh min-w-0 flex-col [&>section]:mx-auto [&>section]:w-full [&>section]:max-w-6xl [&>section]:px-3 [&>section]:md:px-6">
      <ExternalWorldAdmin
        defaultURL={location.origin}
        initialToken={initialToken ?? undefined}
        header={(actions, address) => (
          <header className="flex h-10 shrink-0 items-center gap-2 border-b px-3 md:px-6">
            <ServerIcon
              size={16}
              aria-hidden
              className="shrink-0 color-muted"
            />
            <h1 className="shrink-0 text-title">{t("worldAdmin.title")}</h1>
            {address && (
              <span className="min-w-0 truncate text-caption color-muted">
                {address}
              </span>
            )}
            <div className="ml-auto shrink-0">{actions}</div>
          </header>
        )}
      />
    </main>
  )
}
