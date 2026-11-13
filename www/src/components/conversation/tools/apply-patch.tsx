import { useTranslation } from "react-i18next"
import { CodeView } from "@/components/base/code-view"
import { ToolCard } from "@/components/conversation/tool-call-view"
import type { ToolRendererProps } from "@/lib/item-renderers"
import {
  getPatchFileChanges,
  getStringArrayField,
  getStringField,
} from "@/lib/tool-arguments"

// apply_patch：详情列出文件、增删行数和着色补丁；只应用了一部分时写出已改的文件和原因。
export function ApplyPatchTool(props: ToolRendererProps) {
  const { t } = useTranslation()
  const { result } = props
  const patch = getStringField(props.arguments.value, "patch")
  const summary = getPatchFileChanges(patch ?? "")
    .map((change) =>
      change.kind === "delete"
        ? t("conversation.tool.patch.deleted", { path: change.path })
        : `${change.path} +${change.addedLines} −${change.deletedLines}`
    )
    .join(", ")
  // 结果正文同 pkg/executor/tools 的 applyPatchResult：outcome 是 applied 或 partially_applied；
  // added、updated、deleted 没有对应文件时是 null
  const content = result?.content
  const changedPaths = ["added", "updated", "deleted"].flatMap((field) =>
    getStringArrayField(content, field)
  )
  const problem = getStringField(
    (content as { problem?: unknown } | null | undefined)?.problem,
    "code"
  )
  return (
    <ToolCard
      {...props}
      argumentSummary={summary && <span className="text-code">{summary}</span>}
      displayedArguments={["patch"]}
    >
      {summary && <pre className="overflow-x-auto text-code">{summary}</pre>}
      {patch !== undefined && (
        <CodeView
          code={patch}
          lang="diff"
          maxHeight="24rem"
          viewStateKey={props.viewKey + ":patch"}
        />
      )}
      {getStringField(content, "outcome") === "partially_applied" && (
        <p className="text-ui wrap-anywhere color-warning">
          {t("conversation.tool.patch.partiallyApplied", {
            paths: changedPaths.join(", ") || "—",
            problem: problem ?? "—",
          })}
        </p>
      )}
    </ToolCard>
  )
}
