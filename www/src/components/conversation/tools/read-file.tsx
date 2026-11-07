import { useTranslation } from "react-i18next"
import { ImageIcon } from "lucide-react"
import { CodeView } from "@/components/base/code-view"
import { ToolCard, ToolMeta } from "@/components/conversation/tool-call-view"
import type { ToolRendererProps } from "@/lib/item-renderers"
import {
  getBooleanField,
  getFileLanguage,
  getNumberField,
  getReadFileText,
  getStringField,
} from "@/lib/tool-arguments"
import { formatByteSize } from "@/lib/utils"

// read_file：详情显示路径、按扩展名着色的文件内容和行号范围，读到图片时显示缩略图。
// 结果正文同 pkg/executor/tools 的 readFileResult（文本，content 每行带「行号: 」）或 readImageResult（图片，宽高、大小是原文件的）
export function ReadFileTool(props: ToolRendererProps) {
  const { t, i18n } = useTranslation()
  const { result } = props
  const path = getStringField(props.arguments.value, "path")
  const content = result?.content
  const size = getNumberField(content, "size")
  const text = getStringField(content, "content")
  const images = (result?.images ?? []).flatMap((each) => (each ? [each] : []))
  return (
    <ToolCard
      {...props}
      argumentSummary={
        path &&
        (images.length > 0 ? (
          <span className="flex max-w-full items-center gap-1">
            <ImageIcon
              data-read-file-image
              aria-hidden
              className="size-4 shrink-0"
            />
            <span className="truncate text-code">{path}</span>
          </span>
        ) : (
          <span className="text-code">{path}</span>
        ))
      }
    >
      {text !== undefined && (
        <>
          {text ? (
            <CodeView
              code={getReadFileText(text)}
              lang={getFileLanguage(
                getStringField(content, "path") ?? path ?? ""
              )}
            />
          ) : (
            <p className="text-ui color-muted">
              {t("conversation.tool.readFile.empty")}
            </p>
          )}
          <ReadFileMeta content={content} language={i18n.language} />
        </>
      )}
      {images.length > 0 && (
        <div className="flex min-w-0 flex-col gap-1">
          {text === undefined && (
            <ToolMeta
              items={[
                getNumberField(content, "width") !== undefined &&
                  `${getNumberField(content, "width")}×${getNumberField(content, "height")}`,
                getStringField(content, "mime_type"),
                size !== undefined && formatByteSize(size, i18n.language),
                getBooleanField(content, "compressed") &&
                  t("conversation.tool.readFile.compressed"),
              ]}
            />
          )}
        </div>
      )}
    </ToolCard>
  )
}

// 文本结果的附加信息：本页行号范围（next_offset 是下一页的起始行）、文件大小、是否读完、是否达到单次上限
function ReadFileMeta({
  content,
  language,
}: {
  content: unknown
  language: string
}) {
  const { t } = useTranslation()
  const first = getNumberField(content, "offset")
  const nextOffset = getNumberField(content, "next_offset")
  const last = nextOffset === undefined ? undefined : nextOffset - 1
  const size = getNumberField(content, "size")
  return (
    <ToolMeta
      items={[
        first !== undefined &&
          last !== undefined &&
          last >= first &&
          t("conversation.tool.readFile.lines", { first, last }),
        size !== undefined && formatByteSize(size, language),
        getBooleanField(content, "eof") === false &&
          t("conversation.tool.readFile.more"),
        getBooleanField(content, "truncated") &&
          t("conversation.tool.readFile.truncated"),
      ]}
    />
  )
}
