import { OverflowTooltip } from "@/components/base/overflow-tooltip"
import { ListEmpty } from "@/components/base/list-empty"
import { RowList, RowListItem } from "@/components/base/row-list"
import type { SkillInfo } from "@/lib/api/protocol.gen"

// Skill 列表：名字、所在 Executor、说明和文件路径，按 serve 返回的顺序；没有 Skill 时显示空状态。
// 对话设置的 Skill 页签和设置页的 Skill 一节共用
export function SkillList({
  skills,
  emptyTitle,
  emptyDescription,
}: {
  skills: SkillInfo[]
  emptyTitle: string
  emptyDescription: string
}) {
  if (skills.length === 0)
    return <ListEmpty title={emptyTitle} description={emptyDescription} />
  return (
    <RowList>
      {skills.map((skill) => (
        <RowListItem key={skill.executor + "/" + skill.path}>
          <div className="flex min-w-0 items-center gap-2">
            <OverflowTooltip render={<span className="min-w-0 truncate" />}>
              {skill.name}
            </OverflowTooltip>
            <OverflowTooltip
              render={
                <span className="ml-auto max-w-1/2 shrink-0 truncate color-muted" />
              }
            >
              @{skill.executor}
            </OverflowTooltip>
          </div>
          {skill.description && (
            <p className="text-caption wrap-anywhere color-muted">
              {skill.description}
            </p>
          )}
          <OverflowTooltip
            render={<p className="truncate text-table color-muted" />}
          >
            {skill.path}
          </OverflowTooltip>
        </RowListItem>
      ))}
    </RowList>
  )
}
