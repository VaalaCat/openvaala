import { useTranslation } from "react-i18next"
import { LanguageSwitcher } from "@/components/base/language-switcher"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"

// 通用设置即时应用本机语言。
export function GeneralSettingsPage() {
  // 1. 语言保留在通用页，外观单独设置。
  const { t } = useTranslation()
  return (
    <SettingsLayout section="general">
      <FieldGroup className="max-w-lg gap-4">
        <Field>
          <FieldLabel htmlFor="interface-language" className="text-ui">
            {t("common.language")}
          </FieldLabel>
          <LanguageSwitcher
            render={
              <Button
                id="interface-language"
                variant="outline"
                className="text-ui"
              />
            }
          />
        </Field>
      </FieldGroup>
    </SettingsLayout>
  )
}
