import {
  useEffect,
  useId,
  useState,
  type FormEvent,
  type ReactNode,
} from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { KeyRoundIcon, LogOutIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { RequestError } from "@/components/base/request-error"
import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  externalWorldAdmin,
  getWorldAdminNodes,
  normalizeWorldControlURL,
  WorldAdminError,
  type ExternalWorldAdminConnection,
} from "@/lib/api/world-admin"
import { WorldAdminConsole } from "./world-admin-console"

const credentialsKey = (url: string) => "openvaala.worldAdmin.token:" + url

function storedToken(url: string) {
  return localStorage.getItem(credentialsKey(url)) ?? ""
}

export function ExternalWorldAdmin({
  defaultURL,
  initialToken,
  header,
}: {
  defaultURL: string
  initialToken?: string
  header?: (actions?: ReactNode, address?: string) => ReactNode
}) {
  const { t } = useTranslation()
  const id = useId()
  const cache = useQueryClient()
  const [url, setURL] = useState(defaultURL)
  const [token, setToken] = useState("")
  const [invalid, setInvalid] = useState<"url" | "token">()
  const [attempt, setAttempt] = useState<{
    connection?: ExternalWorldAdminConnection
    number: number
  }>(() => {
    const saved = initialToken || storedToken(defaultURL)
    return {
      connection: saved ? externalWorldAdmin(defaultURL, saved) : undefined,
      number: 0,
    }
  })
  const connection = attempt.connection
  const validation = useQuery({
    queryKey: [connection?.scope, "world-admin-login", id, attempt.number],
    enabled: connection !== undefined,
    queryFn: ({ signal }) => getWorldAdminNodes(connection!, signal),
    gcTime: 0,
  })
  const accepted = validation.isSuccess && connection
  useEffect(() => {
    if (accepted)
      localStorage.setItem(credentialsKey(accepted.controlURL), accepted.token)
  }, [accepted])

  function login(event: FormEvent) {
    event.preventDefault()
    const controlURL = normalizeWorldControlURL(url)
    if (!controlURL) {
      setInvalid("url")
      return
    }
    const credential = token.trim() || storedToken(controlURL)
    if (!credential) {
      setInvalid("token")
      return
    }
    setInvalid(undefined)
    setAttempt((previous) => ({
      number: previous.number + 1,
      connection: externalWorldAdmin(controlURL, credential),
    }))
  }

  function changeInput() {
    setInvalid(undefined)
    if (validation.isError) {
      setAttempt((previous) => ({ number: previous.number + 1 }))
    }
  }

  function logout() {
    if (!accepted) return
    localStorage.removeItem(credentialsKey(accepted.controlURL))
    cache.removeQueries({
      queryKey: [accepted.scope, "world-admin"],
    })
    setToken("")
    setAttempt((previous) => ({ number: previous.number + 1 }))
  }

  if (accepted) {
    const actions = (
      <Button variant="ghost" onClick={logout} className="text-ui">
        <LogOutIcon data-icon="inline-start" />
        {t("worldAdmin.logout")}
      </Button>
    )
    return (
      <>
        {header ? (
          header(actions, accepted.controlURL)
        ) : (
          <div className="flex min-w-0 items-center justify-between gap-2 border-b px-3 py-1">
            <span className="truncate text-caption color-muted">
              {accepted.controlURL}
            </span>
            {actions}
          </div>
        )}
        <WorldAdminConsole
          key={accepted.apiURL}
          connection={accepted}
          initialNodes={validation.data}
        />
      </>
    )
  }

  const pending = connection !== undefined && validation.isPending
  const unauthorized =
    validation.error instanceof WorldAdminError &&
    validation.error.status === 401
  return (
    <>
      {header?.()}
      <div className="grid flex-1 place-items-center px-3 py-6">
        <form
          onSubmit={login}
          noValidate
          className="flex w-full max-w-sm flex-col gap-4 rounded-lg border bg-card p-4 md:p-6"
        >
          <div className="flex items-center gap-3">
            <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-muted">
              <KeyRoundIcon size={16} aria-hidden />
            </span>
            <h2 className="text-section">{t("worldAdmin.loginTitle")}</h2>
          </div>
          {unauthorized ? (
            <p role="alert" className="text-caption color-destructive">
              {t("worldAdmin.unauthorized")}
            </p>
          ) : validation.isError ? (
            <RequestError error={validation.error} />
          ) : null}
          <Field>
            <FieldLabel className="text-ui" htmlFor={id + "-url"}>
              {t("worldAdmin.controlURL")}
            </FieldLabel>
            <Input
              id={id + "-url"}
              className="text-ui"
              type="url"
              value={url}
              disabled={pending}
              onChange={(event) => {
                setURL(event.target.value)
                changeInput()
              }}
              aria-invalid={invalid === "url"}
              autoComplete="url"
            />
            {invalid === "url" && (
              <p role="alert" className="text-caption color-destructive">
                {t("worldAdmin.invalidURL")}
              </p>
            )}
          </Field>
          <Field>
            <FieldLabel className="text-ui" htmlFor={id + "-token"}>
              {t("worldAdmin.adminToken")}
            </FieldLabel>
            <Input
              id={id + "-token"}
              className="text-ui"
              type="password"
              value={token}
              disabled={pending}
              onChange={(event) => {
                setToken(event.target.value)
                changeInput()
              }}
              aria-invalid={invalid === "token" || unauthorized}
              autoComplete="current-password"
            />
            {invalid === "token" && (
              <p role="alert" className="text-caption color-destructive">
                {t("worldAdmin.tokenRequired")}
              </p>
            )}
          </Field>
          <Button type="submit" className="text-ui" disabled={pending}>
            {pending && <LatticeLoader data-icon="inline-start" />}
            {t("worldAdmin.login")}
          </Button>
        </form>
      </div>
    </>
  )
}
