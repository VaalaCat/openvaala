import type { ReactElement } from "react"
import { LanguagesIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { supportedLanguages } from "@/i18n"

// 菜单按钮显示当前语言，读屏读作「语言 + 当前语言」；render 换成调用方的按钮外观，例如左栏菜单按钮
export function LanguageSwitcher({
  render = <Button className="text-ui" variant="ghost" />,
}: {
  render?: ReactElement
}) {
  const { t, i18n } = useTranslation()
  const current = supportedLanguages.find(
    ({ value }) => value === i18n.resolvedLanguage
  )
  return (
    <DropdownMenu>
      <DropdownMenuTrigger render={render}>
        <LanguagesIcon />
        <span className="sr-only">{t("common.language")}</span>
        <span className="truncate">{current?.label}</span>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-32">
        <DropdownMenuRadioGroup
          value={i18n.resolvedLanguage}
          onValueChange={(value) => void i18n.changeLanguage(value)}
        >
          {supportedLanguages.map(({ value, label }) => (
            <DropdownMenuRadioItem
              className="text-ui"
              key={value}
              value={value}
              closeOnClick
            >
              {label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
