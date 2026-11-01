import { useState, type FormEvent } from "react"
import { AlertCircleIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { SecretInput } from "@/components/base/secret-input"
import { StandalonePage } from "@/components/base/standalone-page"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"

// token 输入页：地址里没有 token，或 serve 拒绝了已保存的 token（isTokenInvalid）时显示
export function TokenEntryPage({
  isTokenInvalid,
  onSubmit,
}: {
  isTokenInvalid: boolean
  onSubmit: (token: string) => void
}) {
  const { t } = useTranslation()
  const [token, setToken] = useState("")
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    onSubmit(token.trim())
  }
  return (
    <StandalonePage
      title={t("connection.tokenEntry.title")}
      description={t("connection.tokenEntry.description")}
    >
      <form onSubmit={submit} className="flex flex-col gap-4">
        {isTokenInvalid && (
          <Alert variant="destructive">
            <AlertCircleIcon />
            <AlertDescription className="text-caption">
              {t("connection.tokenEntry.invalid")}
            </AlertDescription>
          </Alert>
        )}
        <SecretInput
          id="token"
          label={t("connection.tokenEntry.label")}
          isLabelHidden
          value={token}
          onChange={setToken}
        />
        <Button className="text-ui" type="submit" disabled={!token.trim()}>
          {t("connection.tokenEntry.submit")}
        </Button>
      </form>
    </StandalonePage>
  )
}
