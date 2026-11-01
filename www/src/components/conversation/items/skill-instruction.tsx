import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { BookOpen } from "lucide"
import { useTranslation } from "react-i18next"
import {
  DetailSection,
  ItemNotice,
} from "@/components/conversation/item-notice"
import { MarkdownView } from "@/components/conversation/markdown-view"
import type { ItemRendererProps } from "@/lib/item-renderers"

// skill_instruction：用户点名的 skill 读进了上下文；点开看文件位置和全文
export function SkillInstructionItem({ view }: ItemRendererProps) {
  // 1. 标题显示动作和技能名，展开区显示位置与正文。
  const { t } = useTranslation()
  const skill = view.item.skill_instruction
  if (!skill) return null
  return (
    <ItemNotice
      icon={BookOpen}
      details={
        <div className="flex flex-col gap-2">
          <DetailSection label={t("conversation.notice.file")}>
            <OverflowTooltip
              render={<span className="block truncate text-table" />}
            >
              {skill.path}@{skill.executor}
            </OverflowTooltip>
          </DetailSection>
          <MarkdownView text={skill.text} />
        </div>
      }
    >
      <span className="inline-flex max-w-full min-w-0 items-center gap-2">
        <span className="shrink-0">
          {t("conversation.notice.skillInstruction")}
        </span>
        <OverflowTooltip
          render={<span className="min-w-0 truncate color-muted" />}
        >
          {skill.name}
        </OverflowTooltip>
      </span>
    </ItemNotice>
  )
}
