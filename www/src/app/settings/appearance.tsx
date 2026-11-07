import { useLayoutEffect, useRef } from "react"
import type { ParseKeys } from "i18next"
import { useStore } from "@nanostores/react"
import { useTranslation } from "react-i18next"
import { Terminal } from "@xterm/xterm"
import { FitAddon } from "@xterm/addon-fit"
import { MinusIcon, PlusIcon } from "lucide-react"
import { ThemeSwitcher } from "@/components/base/theme-switcher"
import { MarkdownView } from "@/components/conversation/markdown-view"
import { CodeView } from "@/components/base/code-view"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  $appearance,
  defaultAppearance,
  appearanceNumberRanges,
} from "@/lib/appearance/preferences"
import { getTerminalTheme } from "@/lib/workspace/terminal-session"
import "@xterm/xterm/css/xterm.css"

const appearanceGroups = [
  { name: "body", label: "settings.appearance.body" },
  { name: "code", label: "settings.appearance.code" },
  { name: "terminal", label: "settings.appearance.terminal" },
] as const
const numberLabels = {
  fontSize: "settings.appearance.fontSize",
  lineHeight: "settings.appearance.lineHeight",
  paragraphSpacing: "settings.appearance.paragraphSpacing",
  paddingX: "settings.appearance.paddingX",
  paddingY: "settings.appearance.paddingY",
  letterSpacing: "settings.appearance.letterSpacing",
  padding: "settings.appearance.padding",
} as const satisfies Record<string, ParseKeys>

// 外观按主题与字体、正文、代码、终端分组，即时保存并更新预览。
export function AppearanceSettingsPage() {
  // 1. 主题和字体沿用已有偏好，阅读尺寸与终端独立调整。
  const { t } = useTranslation()
  const appearance = useStore($appearance)
  const fonts = [
    { value: "noto-serif", label: "Noto Serif" },
    { value: "jetbrains-mono", label: "JetBrains Mono" },
    { value: "system", label: t("settings.appearance.systemFont") },
  ] as const
  return (
    <SettingsLayout
      section="appearance"
      actions={
        <Button
          variant="outline"
          className="text-ui"
          onClick={() => $appearance.set({ ...defaultAppearance })}
        >
          {t("settings.appearance.reset")}
        </Button>
      }
    >
      <div className="flex max-w-4xl min-w-0 flex-col gap-6">
        <section
          aria-labelledby="appearance-theme-title"
          className="flex min-w-0 flex-col gap-4"
        >
          <div className="flex items-center justify-between gap-2">
            <h2 id="appearance-theme-title" className="text-section">
              {t("settings.appearance.themeAndFonts")}
            </h2>
            <Button
              variant="ghost"
              className="text-ui"
              onClick={() =>
                $appearance.set({
                  ...$appearance.get(),
                  mode: defaultAppearance.mode,
                  theme: defaultAppearance.theme,
                  bodyFont: defaultAppearance.bodyFont,
                  headingFont: defaultAppearance.headingFont,
                })
              }
            >
              {t("settings.appearance.resetGroup")}
            </Button>
          </div>
          <div className="grid min-w-0 gap-4 lg:grid-cols-2">
            <FieldGroup className="min-w-0 gap-4">
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
                  {
                    name: "bodyFont",
                    label: t("settings.appearance.bodyFont"),
                  },
                  {
                    name: "headingFont",
                    label: t("settings.appearance.headingFont"),
                  },
                ] as const
              ).map(({ name, label }) => (
                <Field key={name}>
                  <FieldLabel
                    id={`appearance-${name}-label`}
                    className="text-ui"
                  >
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
                      <ToggleGroupItem
                        key={value}
                        value={value}
                        className="text-ui"
                      >
                        {label}
                      </ToggleGroupItem>
                    ))}
                  </ToggleGroup>
                </Field>
              ))}
            </FieldGroup>
            <div
              className="flex min-w-0 flex-col gap-3 border bg-background p-3"
              aria-label={t("settings.appearance.preview")}
            >
              <h3 className="text-section">
                {t("settings.appearance.previewTitle")}
              </h3>
              <p className="text-body">
                {t("settings.appearance.previewBody")}
              </p>
              <p className="text-caption color-muted">
                {t("settings.appearance.previewCaption")}
              </p>
            </div>
          </div>
        </section>
        {appearanceGroups.map(({ name, label }) => (
          <section
            key={name}
            aria-labelledby={`appearance-${name}-title`}
            className="flex min-w-0 flex-col gap-4 border-t pt-4"
          >
            <div className="flex items-center justify-between gap-2">
              <h2 id={`appearance-${name}-title`} className="text-section">
                {t(label)}
              </h2>
              <Button
                variant="ghost"
                className="text-ui"
                onClick={() =>
                  $appearance.set({
                    ...$appearance.get(),
                    [name]: { ...defaultAppearance[name] },
                  })
                }
              >
                {t("settings.appearance.resetGroup")}
              </Button>
            </div>
            <div className="grid min-w-0 gap-4 lg:grid-cols-2">
              <FieldGroup className="min-w-0 gap-4">
                {Object.entries(appearanceNumberRanges[name]).map(
                  ([field, range]) => {
                    const fieldLabel = t(
                      numberLabels[field as keyof typeof numberLabels]
                    )
                    const value = (appearance[name] as Record<string, number>)[
                      field
                    ]
                    const setValue = (next: number) =>
                      $appearance.set({
                        ...$appearance.get(),
                        [name]: { ...$appearance.get()[name], [field]: next },
                      })
                    const unit =
                      name === "terminal" && field === "lineHeight" ? "×" : "px"
                    return (
                      <Field
                        key={field}
                        className="min-w-0"
                        orientation="horizontal"
                      >
                        <FieldLabel
                          htmlFor={`appearance-${name}-${field}`}
                          className="flex-1 text-ui"
                        >
                          {fieldLabel}
                        </FieldLabel>
                        <div className="flex shrink-0 items-center gap-2">
                          <Button
                            variant="outline"
                            size="icon"
                            disabled={value <= range.min}
                            aria-label={t("settings.appearance.decrease", {
                              field: fieldLabel,
                            })}
                            onClick={() =>
                              setValue(
                                Math.max(
                                  range.min,
                                  Number((value - range.step).toFixed(2))
                                )
                              )
                            }
                          >
                            <MinusIcon />
                          </Button>
                          <AppearanceNumberInput
                            id={`appearance-${name}-${field}`}
                            value={value}
                            range={range}
                            onChange={setValue}
                          />
                          <span className="w-5 text-caption color-muted">
                            {unit}
                          </span>
                          <Button
                            variant="outline"
                            size="icon"
                            disabled={value >= range.max}
                            aria-label={t("settings.appearance.increase", {
                              field: fieldLabel,
                            })}
                            onClick={() =>
                              setValue(
                                Math.min(
                                  range.max,
                                  Number((value + range.step).toFixed(2))
                                )
                              )
                            }
                          >
                            <PlusIcon />
                          </Button>
                        </div>
                      </Field>
                    )
                  }
                )}
              </FieldGroup>
              <div
                className="min-w-0"
                aria-label={t("settings.appearance.preview")}
              >
                {name === "body" && (
                  <div className="border p-3">
                    <MarkdownView
                      text={
                        t("settings.appearance.previewBody") +
                        "\n\n" +
                        t("settings.appearance.previewSecondParagraph")
                      }
                    />
                  </div>
                )}
                {name === "code" && (
                  <CodeView
                    lang="typescript"
                    code={
                      'const message = "Hello, 世界";\nconsole.log(message);\n// 0123456789 _ → ─│┌┐└┘'
                    }
                  />
                )}
                {name === "terminal" && <TerminalAppearancePreview />}
              </div>
            </div>
          </section>
        ))}
      </div>
    </SettingsLayout>
  )
}

// 数字输入保留未输完的小数，合法数值立即生效，失焦时恢复最后的合法值。
function AppearanceNumberInput({
  id,
  value,
  range,
  onChange,
}: {
  id: string
  value: number
  range: { min: number; max: number; step: number }
  onChange: (value: number) => void
}) {
  // 1. 加减按钮、重置和其他标签页的修改直接同步到输入，不重建输入框。
  const inputRef = useRef<HTMLInputElement>(null)
  useLayoutEffect(() => {
    inputRef.current!.value = String(value)
  }, [value])
  return (
    <Input
      ref={inputRef}
      id={id}
      type="number"
      inputMode="decimal"
      className="w-20 text-center text-ui"
      min={range.min}
      max={range.max}
      step={range.step}
      defaultValue={value}
      onChange={(event) => {
        if (event.target.value && event.target.validity.valid)
          onChange(event.target.valueAsNumber)
      }}
      onBlur={(event) => {
        event.target.value = String(value)
      }}
    />
  )
}

// 终端预览只写入本地样例，不创建 shell 或发送请求。
function TerminalAppearancePreview() {
  const hostRef = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    // 1. 字体加载后建立独立显示终端，按外观变化重新排版。
    const host = hostRef.current!
    const terminal = new Terminal({
      disableStdin: true,
      cursorBlink: false,
      scrollback: 0,
      fontFamily: getComputedStyle(host).fontFamily,
    })
    const fit = new FitAddon()
    terminal.loadAddon(fit)
    let disposed = false
    let frame = 0
    const update = () => {
      if (disposed) return
      const { fontSize, lineHeight, letterSpacing } = $appearance.get().terminal
      terminal.options = {
        fontSize,
        lineHeight,
        letterSpacing,
        theme: getTerminalTheme(host),
      }
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => {
        if (terminal.element) fit.fit()
      })
    }
    const stopAppearance = $appearance.subscribe(update)
    const observer = new ResizeObserver(update)
    const themeObserver = new MutationObserver(update)
    observer.observe(host)
    themeObserver.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["class", "data-theme"],
    })
    const mount = () => {
      if (disposed) return
      terminal.open(host)
      terminal.write(
        '\x1b[?25l$ printf "Hello, 世界"\r\nHello, 世界\r\n0123456789 _ → ─│┌┐└┘\r\n\x1b[32mready\x1b[0m'
      )
      update()
    }
    void document.fonts
      .load(`${terminal.options.fontSize}px ${terminal.options.fontFamily}`)
      .then(mount, mount)
    // 2. 离开设置页时释放显示终端与订阅。
    return () => {
      disposed = true
      stopAppearance()
      observer.disconnect()
      themeObserver.disconnect()
      cancelAnimationFrame(frame)
      terminal.dispose()
    }
  }, [])
  return (
    <div
      ref={hostRef}
      className="appearance-terminal-preview h-40 min-w-0 overflow-hidden border bg-background text-code"
    />
  )
}
