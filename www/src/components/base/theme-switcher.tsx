import type { ReactElement } from "react"
import { useStore } from "@nanostores/react"
import { MonitorIcon, MoonIcon, SunIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { $appearance, type AppearanceMode } from "@/lib/appearance/preferences"

const themes = {
  light: { labelKey: "common.theme.light", icon: SunIcon },
  dark: { labelKey: "common.theme.dark", icon: MoonIcon },
  system: { labelKey: "common.theme.system", icon: MonitorIcon },
} as const satisfies Record<AppearanceMode, unknown>
const modes = Object.keys(themes) as AppearanceMode[]

// 菜单按钮显示当前主题，读屏读作「主题 + 当前主题」；render 换成调用方的按钮外观，例如左栏菜单按钮
export function ThemeSwitcher({
  render = <Button className="text-ui" variant="ghost" />,
}: {
  render?: ReactElement
}) {
  const { t } = useTranslation()
  const { mode } = useStore($appearance)
  const { labelKey, icon: Icon } = themes[mode]
  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={render}>
        <Icon />
        <span className="sr-only">{t("common.theme.title")}</span>
        <span className="truncate">{t(labelKey)}</span>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-32">
        <DropdownMenuRadioGroup
          value={mode}
          onValueChange={(value: AppearanceMode) =>
            $appearance.set({ ...$appearance.get(), mode: value })
          }
        >
          {modes.map((value) => {
            const { labelKey, icon: Icon } = themes[value]
            return (
              <DropdownMenuRadioItem
                className="text-ui"
                key={value}
                value={value}
                closeOnClick
              >
                <Icon />
                {t(labelKey)}
              </DropdownMenuRadioItem>
            )
          })}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
