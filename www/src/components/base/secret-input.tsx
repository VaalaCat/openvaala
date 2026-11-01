import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { EyeIcon, EyeOffIcon } from "lucide-react"
import { Field, FieldLabel } from "@/components/ui/field"
import {
  InputGroup,
  InputGroupInput,
  InputGroupAddon,
  InputGroupButton,
} from "@/components/ui/input-group"
import { cn } from "@/lib/utils"

// 密钥输入框，可以切换明文；不让浏览器和密码管理器当成密码保存或填充
export function SecretInput({
  id,
  label,
  isLabelHidden = false,
  value,
  onChange,
}: {
  id: string
  label: string
  // 标签是否只给读屏？页面上已经写明这是唯一的输入框时用
  isLabelHidden?: boolean
  value: string
  onChange: (value: string) => void
}) {
  const { t } = useTranslation()
  const [revealed, setRevealed] = useState(false)
  return (
    <Field>
      <FieldLabel
        className={cn("text-ui", isLabelHidden && "sr-only")}
        htmlFor={id}
      >
        {label}
      </FieldLabel>
      <InputGroup>
        <InputGroupInput
          className="text-ui"
          id={id}
          type={revealed ? "text" : "password"}
          autoComplete="off"
          data-1p-ignore
          data-lpignore="true"
          data-bwignore
          value={value}
          onChange={(event) => onChange(event.target.value)}
        />
        <InputGroupAddon align="inline-end">
          <Tooltip>
            <TooltipTrigger
              render={
                <InputGroupButton
                  size="icon-xs"
                  aria-label={t(
                    revealed ? "common.actions.hide" : "common.actions.reveal"
                  )}
                  aria-pressed={revealed}
                  onClick={() => setRevealed(!revealed)}
                />
              }
            >
              {revealed ? <EyeOffIcon /> : <EyeIcon />}
            </TooltipTrigger>
            <TooltipContent className="text-caption">
              {t(revealed ? "common.actions.hide" : "common.actions.reveal")}
            </TooltipContent>
          </Tooltip>
        </InputGroupAddon>
      </InputGroup>
    </Field>
  )
}
