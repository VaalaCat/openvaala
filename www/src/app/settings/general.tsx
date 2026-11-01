import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { LanguageSwitcher } from "@/components/base/language-switcher"
import { ThemeSwitcher } from "@/components/base/theme-switcher"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { $appearance, defaultAppearance } from "@/lib/appearance/preferences"

// 通用设置即时应用本机外观和语言。
export function GeneralSettingsPage() {
  // 1. 组合即时生效的偏好控件。
  const { t } = useTranslation()
  const appearance = useStore($appearance)
  const fonts = [
    { value: "noto-serif", label: "Noto Serif" },
    { value: "jetbrains-mono", label: "JetBrains Mono" },
    { value: "system", label: t("settings.appearance.systemFont") },
  ] as const
  return (
    <SettingsLayout section="general">
      <FieldGroup className="max-w-lg gap-4">
        <Field>
          <FieldLabel htmlFor="appearance-mode" className="text-ui">
            {t("common.theme.title")}
          </FieldLabel>
          <ThemeSwitcher
            render={
              <Button
                id="appearance-mode"
                variant="outline"
                className="text-ui"
              />
            }
          />
        </Field>
        <Field>
          <FieldLabel id="appearance-theme-label" className="text-ui">
            {t("settings.appearance.color")}
          </FieldLabel>
          <ToggleGroup
            variant="outline"
            aria-labelledby="appearance-theme-label"
            value={[appearance.theme]}
            onValueChange={(values) => {
              const theme = values[0]
              if (theme === "stone" || theme === "blue")
                $appearance.set({ ...$appearance.get(), theme })
            }}
          >
            <ToggleGroupItem value="stone" className="text-ui">
              {t("settings.appearance.stone")}
            </ToggleGroupItem>
            <ToggleGroupItem value="blue" className="text-ui">
              {t("settings.appearance.blue")}
            </ToggleGroupItem>
          </ToggleGroup>
        </Field>
        {(
          [
            { name: "bodyFont", label: t("settings.appearance.bodyFont") },
            {
              name: "headingFont",
              label: t("settings.appearance.headingFont"),
            },
          ] as const
        ).map(({ name, label }) => (
          <Field key={name}>
            <FieldLabel id={`appearance-${name}-label`} className="text-ui">
              {label}
            </FieldLabel>
            <ToggleGroup
              variant="outline"
              aria-labelledby={`appearance-${name}-label`}
              value={[appearance[name]]}
              className="flex-wrap"
              onValueChange={(values) => {
                const font = values[0]
                if (fonts.some(({ value }) => value === font))
                  $appearance.set({ ...$appearance.get(), [name]: font })
              }}
            >
              {fonts.map(({ value, label }) => (
                <ToggleGroupItem key={value} value={value} className="text-ui">
                  {label}
                </ToggleGroupItem>
              ))}
            </ToggleGroup>
          </Field>
        ))}
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
        <Button
          variant="outline"
          className="self-start text-ui"
          onClick={() => $appearance.set({ ...defaultAppearance })}
        >
          {t("settings.appearance.reset")}
        </Button>
      </FieldGroup>
    </SettingsLayout>
  )
}
