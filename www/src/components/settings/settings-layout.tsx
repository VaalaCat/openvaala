import type { ReactNode } from "react"
import { PlusIcon } from "lucide-react"
import { motion } from "motion/react"
import type { ParseKeys } from "i18next"
import { useTranslation } from "react-i18next"
import { useLocation, useNavigate } from "react-router"
import { LatticeLoader } from "@/components/base/lattice-loader"
import { PageActions, PageLayout } from "@/components/base/page-layout"
import { ScrollableTabsList } from "@/components/base/scrollable-tabs-list"
import { ReloadConfigButton } from "@/components/settings/reload-config-button"
import { useServerConnection } from "@/components/server-connection-provider"
import { Button } from "@/components/ui/button"
import { Tabs, TabsContent, TabsTrigger } from "@/components/ui/tabs"
import {
  getConversationListSearch,
  getSettingsPath,
  type SettingsSection,
} from "@/lib/paths"

// 各节的标题，也是页签文字，按页签顺序
const sectionLabelKeys = {
  general: "settings.sections.general",
  appearance: "settings.sections.appearance",
  providers: "settings.sections.providers",
  agents: "settings.sections.agents",
  executors: "settings.sections.executors",
  hooks: "settings.sections.hooks",
  skills: "settings.sections.skills",
  archived: "settings.sections.archived",
  "world-admin": "settings.sections.worldAdmin",
} as const satisfies Record<SettingsSection, ParseKeys>

// 设置页显示分节页签、面包屑和页级操作；配置页提供重新加载配置。
export function SettingsLayout({
  section,
  actions,
  children,
}: {
  section: SettingsSection
  actions?: ReactNode
  children: ReactNode
}) {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const navigate = useNavigate()
  const { search } = useLocation()
  return (
    <Tabs
      value={section}
      onValueChange={(next: SettingsSection) =>
        void navigate({
          pathname: getSettingsPath(connection.getServerID(), next),
          search: getConversationListSearch(search),
        })
      }
      className="min-w-0"
    >
      <PageLayout
        title={t(sectionLabelKeys[section])}
        parent={{ label: t("settings.title") }}
        actions={
          <>
            {section !== "general" &&
              section !== "appearance" &&
              section !== "archived" &&
              section !== "world-admin" && <ReloadConfigButton />}
            {actions}
          </>
        }
        toolbar={
          <ScrollableTabsList aria-label={t("settings.title")}>
            {Object.entries(sectionLabelKeys).map(([value, labelKey]) => (
              <TabsTrigger className="text-ui" key={value} value={value}>
                {t(labelKey)}
              </TabsTrigger>
            ))}
          </ScrollableTabsList>
        }
      >
        <TabsContent
          key={section}
          value={section}
          className="flex min-w-0 flex-col gap-4 text-body"
          render={
            <motion.div
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              transition={{ duration: 0.12 }}
            />
          }
        >
          {children}
        </TabsContent>
      </PageLayout>
    </Tabs>
  )
}

// 设置草稿的页头操作。编辑时先撤销再保存，新增时只显示提交按钮。
export function SettingsDraftActions({
  dirty,
  saving,
  saveDisabled,
  saveLabel,
  form,
  onSave,
  onRevert,
  revertLabel,
  status,
}: {
  dirty: boolean
  saving: boolean
  saveDisabled: boolean
  saveLabel: string
  form?: string
  onSave?: () => void
  onRevert?: () => void
  revertLabel?: string
  status?: string
}) {
  const { t } = useTranslation()
  const disabled = saving || (onRevert !== undefined && !dirty)
  return (
    <PageActions>
      {status && (
        <span role="status" className="text-caption color-muted">
          {status}
        </span>
      )}
      {onRevert && (
        <Button
          className="text-ui"
          type="button"
          variant="outline"
          disabled={disabled}
          onClick={onRevert}
        >
          {revertLabel ?? t("settings.providerEditor.revert")}
        </Button>
      )}
      <Button
        className="text-ui"
        type={form ? "submit" : "button"}
        form={form}
        disabled={disabled || saveDisabled}
        onClick={onSave}
      >
        {saving ? (
          <LatticeLoader
            data-icon="inline-start"
            aria-label={t("common.loading")}
          />
        ) : (
          !onRevert && <PlusIcon data-icon="inline-start" />
        )}
        {saveLabel}
      </Button>
    </PageActions>
  )
}
