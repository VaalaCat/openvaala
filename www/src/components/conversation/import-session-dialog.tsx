import { useState } from "react"
import {
  useInfiniteQuery,
  useQuery,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"
import { motion, AnimatePresence } from "motion/react"
import { ImportIcon } from "lucide-react"
import { BrandIcon } from "@/components/base/brand-icon"
import { ListSearch } from "@/components/base/list-search"
import { RequestError } from "@/components/base/request-error"
import { useServerConnection } from "@/components/server-connection-provider"
import {
  Dialog,
  DialogContent,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { getConversationPath } from "@/lib/paths"
import type { AgentInfo, ExternalAgentLocation } from "@/lib/api/protocol.gen"

export function ImportSessionDialog({
  agent,
  executorID,
  resolveDirectory,
}: {
  agent: AgentInfo
  executorID?: string
  resolveDirectory(): Promise<string | undefined>
}) {
  const { t } = useTranslation()
  const [location, setLocation] = useState<ExternalAgentLocation>()
  return (
    <>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              type="button"
              variant="ghost"
              size="icon"
              aria-label={t("externalSession.importExisting")}
              onClick={async () => {
                try {
                  const cwd = await resolveDirectory()
                  setLocation({
                    agent_name: agent.name,
                    executor_id: executorID,
                    cwd,
                  })
                } catch {
                  /* 目录框显示原因。 */
                }
              }}
            />
          }
        >
          <ImportIcon />
        </TooltipTrigger>
        <TooltipContent>{t("externalSession.importExisting")}</TooltipContent>
      </Tooltip>
      <Dialog
        open={!!location}
        onOpenChange={(open) => {
          if (!open) setLocation(undefined)
        }}
      >
        {location && <ImportContent agent={agent} location={location} />}
      </Dialog>
    </>
  )
}

function ImportContent({
  agent,
  location,
}: {
  agent: AgentInfo
  location: ExternalAgentLocation
}) {
  const { t, i18n } = useTranslation()
  const connection = useServerConnection()
  const client = useQueryClient()
  const navigate = useNavigate()
  const [query, setQuery] = useState("")
  const [selected, setSelected] = useState("")
  const [manual, setManual] = useState("")
  const sessions = useInfiniteQuery({
    queryKey: [connection.getServerID(), "importSessions", location, query],
    queryFn: ({ pageParam }) =>
      connection.call("agent/listSessions", {
        ...location,
        query,
        cursor: pageParam,
      }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor || undefined,
    retry: false,
  })
  const rows =
    sessions.data?.pages
      .flatMap((p) => p.sessions ?? [])
      .sort((a, b) => b.updated_at - a.updated_at) ?? []
  const row = rows.find((r) => r.session_id === selected)
  const preview = useQuery({
    queryKey: [connection.getServerID(), "importPreview", location, selected],
    queryFn: () =>
      connection.call("agent/previewSession", {
        ...location,
        external_session_id: selected,
      }),
    enabled: !!row && !sessions.isError,
  })
  const id = sessions.isError ? manual.trim() : row?.session_id
  const submit = useMutation({
    mutationFn: () =>
      connection.call("session/import", {
        ...location,
        external_session_id: id!,
        title: row?.title,
      }),
    onSuccess: ({ conversation }) => {
      void client.invalidateQueries({
        queryKey: [connection.getServerID(), "conversations"],
      })
      void navigate(
        getConversationPath(connection.getServerID(), conversation.id)
      )
    },
  })
  return (
    <DialogContent className="flex max-h-[80svh] flex-col gap-4 text-body">
      <DialogTitle className="flex items-center gap-2 text-section">
        <BrandIcon icon={agent.icon ?? ""} />
        {t("externalSession.importTitle", { name: agent.name })}
      </DialogTitle>
      {sessions.isError ? (
        <>
          <RequestError error={sessions.error} />
          <Input
            aria-label={t("externalSession.sessionID")}
            placeholder={t("externalSession.sessionID")}
            value={manual}
            onChange={(e) => setManual(e.target.value)}
          />
        </>
      ) : (
        <>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              setSelected("")
              setQuery(
                String(new FormData(e.currentTarget).get("keyword") ?? "")
              )
            }}
          >
            <ListSearch
              type="search"
              placeholder={t("externalSession.search")}
              aria-label={t("externalSession.search")}
              keyword={query}
              onKeywordChange={(value) => {
                setSelected("")
                setQuery(value)
              }}
              onClear={() => {
                setSelected("")
                setQuery("")
              }}
            />
          </form>
          <div className="min-h-0 overflow-y-auto">
            {rows.map((r) => (
              <div
                key={r.session_id}
                className={r === row ? "bg-muted" : undefined}
              >
                <button
                  type="button"
                  aria-expanded={r === row}
                  className="flex w-full items-start justify-between gap-4 px-2 py-2 text-left hover:bg-muted"
                  onClick={() => setSelected(r === row ? "" : r.session_id)}
                >
                  <span className="min-w-0">
                    <span className="block truncate text-ui">
                      {r.title ||
                        t("externalSession.untitled") +
                          " " +
                          r.session_id.slice(0, 8)}
                    </span>
                    <span className="block truncate text-caption color-muted">
                      {r.cwd}
                    </span>
                  </span>
                  <time className="shrink-0 text-caption color-muted">
                    {r.updated_at
                      ? new Date(r.updated_at).toLocaleString(i18n.language)
                      : ""}
                  </time>
                </button>
                <AnimatePresence>
                  {r === row && (
                    <motion.div
                      initial={{ height: 0, opacity: 0 }}
                      animate={{ height: "auto", opacity: 1 }}
                      exit={{ height: 0, opacity: 0 }}
                      transition={{ duration: 0.15, ease: "easeOut" }}
                      className="overflow-hidden"
                    >
                      <div className="px-2 pb-2 text-caption color-muted">
                        {preview.isError ? (
                          <RequestError
                            error={preview.error}
                            onRetry={() => void preview.refetch()}
                          />
                        ) : preview.isPending ? (
                          t("common.loading")
                        ) : (
                          <blockquote className="flex flex-col gap-2 border-l pl-3 wrap-anywhere whitespace-pre-wrap">
                            <p>{preview.data.first_user_message}</p>
                            {preview.data.last_user_message !==
                              preview.data.first_user_message && (
                              <p>{preview.data.last_user_message}</p>
                            )}
                          </blockquote>
                        )}
                      </div>
                    </motion.div>
                  )}
                </AnimatePresence>
              </div>
            ))}
            {sessions.isPending ? (
              <p className="p-2 text-caption color-muted">
                {t("common.loading")}
              </p>
            ) : (
              rows.length === 0 && (
                <p className="p-2 text-caption color-muted">
                  {t("common.selection.empty")}
                </p>
              )
            )}
            {sessions.hasNextPage && (
              <Button
                variant="ghost"
                disabled={sessions.isFetchingNextPage}
                onClick={() => void sessions.fetchNextPage()}
              >
                {t("externalSession.loadMore")}
              </Button>
            )}
          </div>
        </>
      )}
      {submit.isError && <RequestError error={submit.error} />}
      <DialogFooter>
        <Button
          disabled={!id || submit.isPending}
          onClick={() => submit.mutate()}
        >
          {t("externalSession.import")}
        </Button>
      </DialogFooter>
    </DialogContent>
  )
}
