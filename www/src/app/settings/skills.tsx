import { shouldShowLoadError } from "@/lib/rpc-error"
import { useQuery } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"
import { QueryState } from "@/components/base/query-state"
import { SettingsLayout } from "@/components/settings/settings-layout"
import { SkillList } from "@/components/settings/skill-list"
import { useServerConnection } from "@/components/server-connection-provider"
import { newConversationAgentName } from "@/lib/query/conversation"
import { skillListOptions, type SkillTarget } from "@/lib/query/skill"

// 新对话固定用的 default Agent，在默认 Executor 设置下查它能用的 Skill；模块级常量，query key 稳定
const defaultAgentSkillTarget: SkillTarget = {
  agentName: newConversationAgentName,
  executorSettings: {},
}

// default Agent 能用的 Skill（只读）
export function SkillSettingsPage() {
  const { t } = useTranslation()
  const connection = useServerConnection()
  const skills = useQuery(skillListOptions(connection, defaultAgentSkillTarget))
  return (
    <SettingsLayout section="skills">
      <QueryState shouldShowError={shouldShowLoadError} query={skills}>
        {(data) => (
          <SkillList
            skills={data}
            emptyTitle={t("settings.skills.empty")}
            emptyDescription={t("settings.skills.emptyDescription")}
          />
        )}
      </QueryState>
    </SettingsLayout>
  )
}
